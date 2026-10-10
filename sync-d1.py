"""
sync-d1.py — 把新构建的索引库「增量」同步到 Cloudflare D1

═══ 为什么不读 D1 全表 ═══
最初的实现是「从 D1 读全表 → 与新索引求差集」。逻辑上没错，但成本炸了：
D1 免费版 **500 万行读取/天**，而 video 表有 292,621 行 —— 一次全表读就吃掉 6%，
加上用 `SELECT COUNT(*)` 做校验（同样是一次 29 万行的全表扫描），
调试几次就把当天额度烧穿（错误 code 7500）。

现在改成元数据驱动：
  D1 里放一张只有 1 行的 `d1_sync_state`，记录「D1 当前对应哪一版索引」。
  每次运行只读这一行（**1 row read**）：
    - index_id 相同        → 什么都不做，结束
    - index_id 不同        → 从 release 下载那一版旧索引库，本地求差集，只写差集
  这样常规周更的成本是：1 行读 + 差集行数写。

  `--reconcile` 保留全表读作为兜底（明确知道要花 ~29 万行 read 时才用），
  但**默认路径永远不会触发它**。

═══ 另外两个硬约束 ═══
- **写入**：免费版 10 万行/天，全量重灌 292,621 行必然超限，所以只写差集。
- **单条 SQL ≤ 100 KB**：把多行打包进一条 `INSERT ... VALUES (..),(..)`。

用法:
  # 常规（CI 用）：读状态 → 需要时自动从 release 拉旧索引 → 写差集
  python sync-d1.py -i dmm-index.db --index-id 2026-10-06 --apply --prev-from-release

  # 手动指定旧索引库
  python sync-d1.py -i dmm-index.db --index-id 2026-10-06 --apply --prev-db prev.db

  # 兜底：不管状态，全表读一遍重新对齐（贵！约 29 万行 read）
  python sync-d1.py -i dmm-index.db --index-id 2026-10-06 --apply --reconcile

  # 首次接入：D1 里已有正确数据，只补建状态行（1 次写，不校验）
  python sync-d1.py -i dmm-index.db --index-id 2026-10-06 --bootstrap

环境变量:
  CLOUDFLARE_API_TOKEN / CLOUDFLARE_ACCOUNT_ID / CLOUDFLARE_DATABASE_ID
  GH_TOKEN            （--prev-from-release 时用，GitHub Runner 自带）
"""

import argparse
import importlib.util
import json
import os
import pathlib
import subprocess
import sys
import time
import urllib.error
import urllib.request

API = "https://api.cloudflare.com/client/v4"
PAGE = 20000                 # 仅 --reconcile 全表读时用
BATCH_BYTES = 80_000         # 单条 SQL 的体积上限（D1 硬限制 100 KB，留 20 KB 余量）
UPSERT_COLS = "(k_letters,k_num,cdn,dirpath,stem,quality,variant,fmt)"

# 只有 1 行的状态表。读取它就是 1 row read —— 这是正常路径唯一的读开销。
STATE_DDL = """
CREATE TABLE IF NOT EXISTS d1_sync_state (
  id         INTEGER PRIMARY KEY CHECK (id = 1),
  index_id   TEXT NOT NULL,
  rows       INTEGER NOT NULL,
  updated_at TEXT NOT NULL
);
"""


def load_export_d1():
    """import export-d1.py（文件名带连字符，不能直接 import）。"""
    p = pathlib.Path(__file__).with_name("export-d1.py")
    spec = importlib.util.spec_from_file_location("export_d1", p)
    mod = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(mod)
    return mod


def die(msg):
    print(f"::error::{msg}", file=sys.stderr)
    raise SystemExit(1)


def creds():
    tok = os.environ.get("CLOUDFLARE_API_TOKEN")
    acc = os.environ.get("CLOUDFLARE_ACCOUNT_ID")
    db = os.environ.get("CLOUDFLARE_DATABASE_ID")
    miss = [n for n, v in (("CLOUDFLARE_API_TOKEN", tok),
                           ("CLOUDFLARE_ACCOUNT_ID", acc),
                           ("CLOUDFLARE_DATABASE_ID", db)) if not v]
    if miss:
        die("缺少环境变量: " + ", ".join(miss))
    return tok, acc, db


def d1_query(sql, params=None, token=None, acc=None, db=None):
    """调 D1 REST /query。注意：success 在 result[0] 里，不在顶层。"""
    body = {"sql": sql}
    if params:
        body["params"] = params
    req = urllib.request.Request(
        f"{API}/accounts/{acc}/d1/database/{db}/query",
        method="POST",
        data=json.dumps(body).encode("utf-8"),
        headers={"Authorization": "Bearer " + token,
                 "Content-Type": "application/json"},
    )
    try:
        with urllib.request.urlopen(req, timeout=180) as r:
            resp = json.loads(r.read())
    except urllib.error.HTTPError as e:
        die(f"D1 HTTP {e.code}: {e.read().decode('utf-8', 'replace')[:400]}")
    res = (resp.get("result") or [{}])[0]
    if not res.get("success"):
        die("D1 查询失败: " + json.dumps(res.get("errors") or resp.get("errors"),
                                         ensure_ascii=False)[:400])
    return res.get("results") or [], (res.get("meta") or {})


# ---------- 状态行：正常路径唯一的读开销 ----------
def ensure_state(token, acc, db):
    d1_query(STATE_DDL, None, token, acc, db)


def ensure_video_schema(ed1, token, acc, db):
    """
    补齐后加的列（D1 不支持 ADD COLUMN IF NOT EXISTS，得先查 PRAGMA 再决定）。

    背景：fmt 列（URL 形态）是 2026-10-07 才引入的，线上已有的 video 表只有 7 列。
    不加这一步，带 fmt 的 UPSERT 会直接报 "table video has no column named fmt"。
    已有行的 fmt 自然是 0（旧代码只认 freepv + 有下划线），DEFAULT 0 正好对上。
    """
    rows, _ = d1_query("PRAGMA table_info(video)", None, token, acc, db)
    cols = {r["name"] for r in rows}
    if not cols:
        print("D1 里没有 video 表，按 D1_SCHEMA 建表 ...", flush=True)
        d1_query(ed1.D1_SCHEMA, None, token, acc, db)
        return
    if "fmt" not in cols:
        d1_query("ALTER TABLE video ADD COLUMN fmt INTEGER NOT NULL DEFAULT 0",
                 None, token, acc, db)
        print("已为 D1 video 表补 fmt 列（存量行取默认值 0）", flush=True)


def read_state(token, acc, db):
    rows, _ = d1_query(
        "SELECT index_id, rows, updated_at FROM d1_sync_state WHERE id = 1",
        None, token, acc, db)
    return rows[0] if rows else None


def write_state(index_id, nrows, token, acc, db):
    d1_query(
        "INSERT OR REPLACE INTO d1_sync_state (id,index_id,rows,updated_at) "
        f"VALUES(1,{sql_str(index_id)},{int(nrows)},{sql_str(utcnow())})",
        None, token, acc, db)


def utcnow():
    return time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime())


def sql_str(s):
    return "'" + str(s).replace("'", "''") + "'"


# ---------- 目标状态：从索引库按归一化键重建 ----------
def load_target(db_path, ed1):
    best, total, skipped = ed1.collect(db_path)
    # 丢弃比例必须可见：export-d1.py 里那句 `if not k: continue` 曾经静默吃掉
    # 24.6% 的行（审查报告 P0-4），现在 collect() 会把 skipped 报回来。
    if total:
        print(f"  索引库 {total:,} 行 → 归一化键 {len(best):,} "
              f"（丢弃 {skipped:,}，{skipped / total:.2%}）")
    # export-d1.py 的键里 num 是「去前导零后的字符串」，D1 侧是 INTEGER，必须统一
    return {(k[0], int(k[1])): v[1:] for k, v in best.items()}, total


# ---------- 全表读（仅 --reconcile，贵） ----------
def fetch_current(token, acc, db):
    cols = "k_letters,k_num,cdn,dirpath,stem,quality,variant,fmt"
    out, rows_read, last = {}, 0, None
    while True:
        if last is None:
            sql = f"SELECT {cols} FROM video ORDER BY k_letters,k_num LIMIT ?1"
            params = [PAGE]
        else:
            sql = (f"SELECT {cols} FROM video "
                   "WHERE k_letters > ?1 OR (k_letters = ?1 AND k_num > ?2) "
                   "ORDER BY k_letters,k_num LIMIT ?3")
            params = [last[0], last[1], PAGE]
        rows, meta = d1_query(sql, params, token, acc, db)
        rows_read += (meta.get("rows_read") or 0)
        for r in rows:
            out[(r["k_letters"], int(r["k_num"]))] = (
                r["cdn"], r["dirpath"], r["stem"], r["quality"], r["variant"],
                int(r.get("fmt") or 0))
        if len(rows) < PAGE:
            break
        last = (rows[-1]["k_letters"], int(rows[-1]["k_num"]))
    return out, rows_read


# ---------- 批量打包：多行塞进一条 SQL，受「单条 ≤100KB」限制 ----------
def build_upsert_batches(upsert, limit=BATCH_BYTES):
    batches, cur, size = [], [], 0
    head_len = len("INSERT OR REPLACE INTO video " + UPSERT_COLS + " VALUES ")
    for (letters, num), (cdn, dirpath, stem, q, v, fmt) in upsert:
        tup = (f"({sql_str(letters)},{int(num)},{sql_str(cdn)},{sql_str(dirpath)},"
               f"{sql_str(stem)},{sql_str(q)},{sql_str(v)},{int(fmt)})")
        if cur and head_len + size + len(tup) + 2 > limit:
            batches.append(cur)
            cur, size = [], 0
        cur.append(tup)
        size += len(tup) + 1
    if cur:
        batches.append(cur)
    return ["INSERT OR REPLACE INTO video " + UPSERT_COLS + " VALUES " + ",".join(b) + ";"
            for b in batches]


def build_delete_batches(delete, limit=BATCH_BYTES):
    batches, cur, size = [], [], 0
    head_len = len("DELETE FROM video WHERE (k_letters,k_num) IN (")
    for letters, num in delete:
        tup = f"({sql_str(letters)},{int(num)})"
        if cur and head_len + size + len(tup) + 3 > limit:
            batches.append(cur)
            cur, size = [], 0
        cur.append(tup)
        size += len(tup) + 1
    if cur:
        batches.append(cur)
    return ["DELETE FROM video WHERE (k_letters,k_num) IN (" + ",".join(b) + ");"
            for b in batches]


def apply_batches(batches, token, acc, db, label):
    written = 0
    for i, sql in enumerate(batches, 1):
        last_err = None
        for attempt in range(3):
            try:
                _, meta = d1_query(sql, None, token, acc, db)
                written += meta.get("rows_written") or 0
                break
            except SystemExit as e:
                last_err = str(e)
                time.sleep(2 * (attempt + 1))
            except Exception as e:
                last_err = str(e)
                time.sleep(2 * (attempt + 1))
        else:
            die(f"{label} 第 {i}/{len(batches)} 批失败: {last_err}")
        if i % 10 == 0 or i == len(batches):
            print(f"  {label} {i}/{len(batches)} 批", flush=True)
    return written, len(batches)


# ---------- 从 release 拉旧索引库 ----------
def download_prev_index(index_id, dest_dir):
    tag = f"index-{index_id}"
    d = pathlib.Path(dest_dir)
    d.mkdir(parents=True, exist_ok=True)
    print(f"下载旧索引库 release {tag} ...", flush=True)
    p = subprocess.run(
        ["gh", "release", "download", tag, "-p", "dmm-index.db", "-O", str(d)],
        capture_output=True, text=True, encoding="utf-8", errors="replace")
    if p.returncode != 0:
        die(f"下载 {tag} 失败: {(p.stderr or p.stdout)[:300]}")
    return str(d / "dmm-index.db")


def main():
    ap = argparse.ArgumentParser(description="增量同步索引库到 D1（元数据驱动）")
    ap.add_argument("-i", "--input", default="dmm-index.db", help="新索引库")
    ap.add_argument("--index-id", required=True,
                    help="本版索引的标识（用 dump 日期，如 2026-10-06）")
    ap.add_argument("-o", "--outdir", default="d1-sync", help="输出目录")
    ap.add_argument("--prev-db", help="旧索引库路径（与 --prev-from-release 二选一）")
    ap.add_argument("--prev-from-release", action="store_true",
                    help="按状态行里的 index_id 从 GitHub release 下载旧索引库")
    ap.add_argument("--reconcile", action="store_true",
                    help="兜底：全表读 D1 重新对齐（约 29 万行 read，别随便用）")
    ap.add_argument("--bootstrap", action="store_true",
                    help="首次接入：D1 数据已正确，只补建状态行（不校验）")
    ap.add_argument("--apply", action="store_true", help="算出差集后写入 D1")
    ap.add_argument("--force", action="store_true",
                    help="即使 index_id 相同也重跑；同时忽略变化比例保护")
    ap.add_argument("--chunk", type=int, default=25000, help="SQL 文件每块语句数")
    ap.add_argument("--batch-bytes", type=int, default=BATCH_BYTES)
    ap.add_argument("--max-write", type=int, default=0,
                    help="本次最多写入多少条（0=不限）。用来突破免费版 10 万行/天上限，"
                         "分多天灌库。没写完时不更新状态行。")
    ap.add_argument("--skip", type=int, default=0,
                    help="跳过前 N 条（与 --max-write 配合续传）。"
                         "差集已按 (k_letters,k_num) 排序，顺序是确定的，所以可安全续传。")
    args = ap.parse_args()

    token, acc, db = creds()
    ed1 = load_export_d1()

    gh_out = os.environ.get("GITHUB_OUTPUT")

    def emit(kv):
        if gh_out:
            with open(gh_out, "a", encoding="utf-8") as f:
                for k, v in kv.items():
                    f.write(f"{k}={v}\n")

    ensure_state(token, acc, db)
    ensure_video_schema(ed1, token, acc, db)
    st = read_state(token, acc, db)
    print(f"D1 状态: {st}" if st else "D1 状态: 无（首次运行）")

    if st and st["index_id"] == args.index_id and not args.force:
        print(f"D1 已是 {args.index_id} 这一版，无需同步。（本次读取：1 行）")
        emit({"changed": "false", "skipped": "true", "upsert": 0, "delete": 0})
        return

    target, total = load_target(args.input, ed1)
    print(f"新索引库: {total:,} 原始行 → {len(target):,} 归一化键")

    # ---------- 取旧状态 ----------
    old = None
    rows_read = 0
    if args.reconcile:
        print("::warning::--reconcile 会全表读 D1（约 29 万行 read），"
              "正常周更不要用", flush=True)
        old, rows_read = fetch_current(token, acc, db)
        print(f"D1 现状: {len(old):,} 行 (rows_read={rows_read:,})")
    elif args.prev_db:
        old, _ = load_target(args.prev_db, ed1)
        print(f"旧索引库 {args.prev_db}: {len(old):,} 键")
    elif args.prev_from_release:
        if not st:
            # 不 die：让定时运行保持绿色，只是明确提示还没 bootstrap。
            # 否则每周二都会因为「还没初始化」红一次，而索引库本身是好的。
            print("::warning::D1 状态行为空，不知道当前对应哪一版索引。"
                  "首次接入请先手动跑一次 d1_sync=bootstrap；"
                  "本次跳过同步（不影响索引库构建与发布）。")
            emit({"changed": "false", "skipped": "true", "need_bootstrap": "true"})
            return
        old_path = download_prev_index(st["index_id"], "prev-index-dl")
        old, _ = load_target(old_path, ed1)
        print(f"旧索引库 {st['index_id']}: {len(old):,} 键")
    elif args.bootstrap:
        print("--bootstrap：假定 D1 现有数据已与目标一致，只补建状态行。")
    else:
        die("需要指定旧状态来源：--prev-db / --prev-from-release / "
            "--reconcile / --bootstrap")

    # ---------- 求差集 ----------
    upsert, delete = [], []
    if old is not None:
        for k, row in target.items():
            if old.get(k) != row:
                upsert.append((k, row))
        for k in old:
            if k not in target:
                delete.append(k)
        upsert.sort()
        delete.sort()
        changed = len(upsert) + len(delete)
        base = max(len(old), 1)
        ratio = changed / base
        print(f"\n差集: 新增/变更 {len(upsert):,} / 删除 {len(delete):,} "
              f"(占旧版 {ratio:.1%})")
        if len(old) and not args.force and ratio > 0.5:
            die(f"变化比例 {ratio:.1%} 超过 50%，疑似索引口径变了。"
                f"确认要全量重灌请加 --force。")
    else:
        changed = 0

    if changed > 100_000:
        print(f"::warning::本次写入 {changed:,} 行，超过免费版 10 万行/天上限")

    # ---------- 落 SQL（便于人工用 wrangler 重放） ----------
    outdir = pathlib.Path(args.outdir)
    outdir.mkdir(parents=True, exist_ok=True)
    files = []
    if changed:
        stmts = []
        for (letters, num), (cdn, dirpath, stem, q, v, fmt) in upsert:
            stmts.append(
                "INSERT OR REPLACE INTO video " + UPSERT_COLS + " VALUES("
                f"{sql_str(letters)},{int(num)},{sql_str(cdn)},{sql_str(dirpath)},"
                f"{sql_str(stem)},{sql_str(q)},{sql_str(v)},{int(fmt)});")
        for letters, num in delete:
            stmts.append(
                f"DELETE FROM video WHERE k_letters={sql_str(letters)} "
                f"AND k_num={int(num)};")
        for i in range(0, len(stmts), args.chunk):
            part = stmts[i:i + args.chunk]
            p = outdir / f"chunk_{i // args.chunk:04d}.sql"
            head = ed1.D1_SCHEMA if i == 0 else ""
            with open(p, "w", encoding="utf-8") as f:
                f.write(head + "\n".join(part) + "\n")
            files.append({"file": p.name, "statements": len(part)})
            print(f"  {p.name}: {len(part):,} 条")

    # ---------- 切片：突破「10 万行/天」写入上限 ----------
    # ⚠️ 必须先算切片、再构造 manifest：以前这段在 manifest 之后，
    #    于是 start/end/remaining/u_sel/d_sel 全是未赋值的名字，
    #    任何走到这里的运行都会 NameError —— D1 增量同步链路曾经整体不可用
    #    （审查报告 P0-6）。顺序本身即是 bug，别再挪回去。
    # 差集顺序是确定的（upsert 先排、delete 后排，各自按 (k_letters,k_num) 排序），
    # 所以 --skip/--max-write 按一维下标切片可以安全续传：不漏、不重复。
    U, D = len(upsert), len(delete)
    total_work = U + D
    start = args.skip
    end = total_work if not args.max_write else min(total_work, start + args.max_write)
    u_sel = upsert[start:min(end, U)]
    d_from = max(0, start - U)
    d_sel = delete[d_from:d_from + max(0, end - max(U, start))]
    remaining = total_work - end
    if args.skip or args.max_write:
        print(f"\n切片: 总 {total_work:,} 条，本次写第 {start:,}~{end:,} 条 "
              f"(upsert {len(u_sel):,} / delete {len(d_sel):,})，剩 {remaining:,}")

    manifest = {
        "index_id": args.index_id,
        "source_rows": total,
        "target_keys": len(target),
        "old_keys": len(old) if old is not None else None,
        "upsert": len(upsert),
        "delete": len(delete),
        "changed": bool(changed),
        "d1_rows_read": rows_read,
        "slice": {"skip": start, "end": end, "remaining": remaining,
                  "this_run": len(u_sel) + len(d_sel)},
        "chunks": files,
    }

    # ---------- 写入 ----------
    if args.apply:
        if not (u_sel or d_sel):
            print("\n本次切片为空，不写 video 表。")
        else:
            up = build_upsert_batches(u_sel, args.batch_bytes)
            de = build_delete_batches(d_sel, args.batch_bytes)
            print(f"写入 D1: upsert {len(up)} 批 / delete {len(de)} 批 ...", flush=True)
            w1, n1 = apply_batches(up, token, acc, db, "upsert")
            w2, n2 = apply_batches(de, token, acc, db, "delete")
            print(f"\nrows_written: upsert={w1:,} delete={w2:,} 合计={w1 + w2:,}")
            manifest["applied"] = {"rows_written": w1 + w2, "requests": n1 + n2}
            emit({"rows_written": w1 + w2})

        if remaining > 0:
            # ⚠️ 没写完就绝不能更新状态行。否则下次运行读到「已是新版」会直接早退，
            #    剩下的行永远不会补上，而且不会有任何报错。
            print(f"\n::warning::还剩 {remaining:,} 条未写（免费版 10 万行/天上限）。"
                  f"状态行保持旧值不动。等 UTC 00:00 额度重置后续传："
                  f"\n  --skip {end} --max-write {args.max_write or 95000}",
                  flush=True)
            emit({"partial": "true", "remaining": remaining, "next_skip": end})
        else:
            # 写状态行：不做 COUNT(*) —— 那是一次 29 万行的全表扫描
            write_state(args.index_id, len(target), token, acc, db)
            manifest["state_written"] = args.index_id
            print(f"状态行已更新为 {args.index_id}（{len(target):,} 行）")
    else:
        print("\n（未加 --apply，只算差集不写入）")
        if remaining > 0:
            print(f"需分 {-(total_work // -args.max_write) if args.max_write else 1} 次写入")

    with open(outdir / "manifest.json", "w", encoding="utf-8") as f:
        json.dump(manifest, f, ensure_ascii=False, indent=2)

    emit({"changed": "true" if changed else "false",
          "upsert": len(upsert), "delete": len(delete),
          "expected": len(target), "chunks": len(files),
          "remaining": remaining, "next_skip": end if remaining else 0})

    gh_sum = os.environ.get("GITHUB_STEP_SUMMARY")
    if gh_sum:
        with open(gh_sum, "a", encoding="utf-8") as f:
            f.write("\n### D1 增量同步（元数据驱动）\n")
            f.write(f"- 目标版本: **{args.index_id}** → {len(target):,} 键\n")
            f.write(f"- 旧版本: {st['index_id'] if st else '无'} "
                    f"({len(old):,} 键)" if old is not None else
                    f"- 旧版本: {st['index_id'] if st else '无'}（--bootstrap 不比对）\n")
            f.write(f"- 新增/变更: **{len(upsert):,}** / 删除: **{len(delete):,}**\n")
            f.write(f"- D1 rows_read: **{rows_read:,}**（正常路径只有状态行那 1 行）\n")


if __name__ == "__main__":
    main()
