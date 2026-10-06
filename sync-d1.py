"""
sync-d1.py — 把新构建的索引库「增量」同步到 Cloudflare D1

为什么必须增量：
  D1 免费版 **10 万行写入/天**。video 表归一化后有 292,621 行，
  每次全量重灌会直接撞上限（且 INSERT OR REPLACE 每一行都算一次写入）。
  所以只能算差集：只写「新增 / 变了 / 删了」的那些键。

为什么从 D1 读现状而不是和上一版索引库 diff：
  和上一版 diff 便宜，但一旦上一次同步中途失败，D1 与「上一版索引库」就永久错开，
  之后每次 diff 都会漏掉那部分，且无人察觉。从 D1 读现状是**自愈**的。
  代价是每周多读约 29 万行（免费版 500 万行/天，占比 6%）。

为什么不走 wrangler d1 execute：
  那条路要在 CI 里装 ~50MB 的 wrangler，且本机 Windows 实测会撞
  "@cloudflare/workerd-windows-64 缺失" 装不上。D1 的 REST /query 接口
  直接可用，只受「单条 SQL ≤ 100 KB」限制 —— 把多行打包进一条
  INSERT ... VALUES (..),(..) 即可，几千行的差集只需几次请求。

用法:
  # 只算差集并落 SQL（可人工用 wrangler 重放）
  python sync-d1.py -i dmm-index.db --outdir d1-sync

  # 算差集 + 直接写入 D1
  python sync-d1.py -i dmm-index.db --outdir d1-sync --apply

  # 导入后核对行数
  python sync-d1.py --verify d1-sync/manifest.json

环境变量:
  CLOUDFLARE_API_TOKEN / CLOUDFLARE_ACCOUNT_ID / CLOUDFLARE_DATABASE_ID
"""

import argparse
import importlib.util
import json
import os
import pathlib
import sys
import time
import urllib.error
import urllib.request

API = "https://api.cloudflare.com/client/v4"
PAGE = 20000                 # 读现状时每页行数
BATCH_BYTES = 80_000         # 单条 SQL 的体积上限（硬限制 100 KB，留 20 KB 余量）
UPSERT_COLS = "(k_letters,k_num,cdn,dirpath,stem,quality,variant)"


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


def fetch_current(token, acc, db):
    """游标分页读全表。返回 {(letters,num): (cdn,dirpath,stem,quality,variant)}"""
    cols = "k_letters,k_num,cdn,dirpath,stem,quality,variant"
    out = {}
    rows_read = 0
    last = None
    while True:
        if last is None:
            sql = f"SELECT {cols} FROM video ORDER BY k_letters,k_num LIMIT ?1"
            params = [PAGE]
        else:
            # 经典游标式：等值打头列 + 范围打第二列，主键 (k_letters,k_num) 一定能走区间扫描
            sql = (f"SELECT {cols} FROM video "
                   "WHERE k_letters > ?1 OR (k_letters = ?1 AND k_num > ?2) "
                   "ORDER BY k_letters,k_num LIMIT ?3")
            params = [last[0], last[1], PAGE]
        rows, meta = d1_query(sql, params, token, acc, db)
        rows_read += (meta.get("rows_read") or 0)
        for r in rows:
            out[(r["k_letters"], int(r["k_num"]))] = (
                r["cdn"], r["dirpath"], r["stem"], r["quality"], r["variant"])
        if len(rows) < PAGE:
            break
        last = (rows[-1]["k_letters"], int(rows[-1]["k_num"]))
        print(f"  ...已读 {len(out):,} 行", flush=True)
    return out, rows_read


def sql_str(s):
    return "'" + str(s).replace("'", "''") + "'"


# ---------- 批量打包：把多行塞进一条 SQL，受「单条 ≤100KB」限制 ----------
def build_upsert_batches(upsert, limit=BATCH_BYTES):
    batches, cur, size = [], [], 0
    head_len = len("INSERT OR REPLACE INTO video " + UPSERT_COLS + " VALUES ")
    for (letters, num), (cdn, dirpath, stem, q, v) in upsert:
        tup = (f"({sql_str(letters)},{int(num)},{sql_str(cdn)},{sql_str(dirpath)},"
               f"{sql_str(stem)},{sql_str(q)},{sql_str(v)})")
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
    """逐条执行；单条失败重试 3 次。返回 (rows_written, 请求数)。"""
    written = 0
    for i, sql in enumerate(batches, 1):
        last_err = None
        for attempt in range(3):
            try:
                _, meta = d1_query(sql, None, token, acc, db)
                written += meta.get("rows_written") or 0
                break
            except SystemExit as e:      # die() 抛的
                last_err = str(e)
                time.sleep(2 * (attempt + 1))
            except Exception as e:       # 网络抖动
                last_err = str(e)
                time.sleep(2 * (attempt + 1))
        else:
            die(f"{label} 第 {i}/{len(batches)} 批失败: {last_err}")
        if i % 10 == 0 or i == len(batches):
            print(f"  {label} {i}/{len(batches)} 批", flush=True)
    return written, len(batches)


def main():
    ap = argparse.ArgumentParser(description="增量同步索引库到 D1")
    ap.add_argument("-i", "--input", default="dmm-index.db", help="新索引库")
    ap.add_argument("-o", "--outdir", default="d1-sync", help="输出目录")
    ap.add_argument("--chunk", type=int, default=25000, help="每个 SQL 文件的语句数上限")
    ap.add_argument("--max-change-ratio", type=float, default=0.5,
                    help="变化比例超过这个值就中止（防误判全量重灌）")
    ap.add_argument("--force", action="store_true", help="忽略比例保护")
    ap.add_argument("--apply", action="store_true",
                    help="算完差集后直接写入 D1（默认只生成 SQL）")
    ap.add_argument("--batch-bytes", type=int, default=BATCH_BYTES,
                    help="单条 SQL 的体积上限（D1 硬限制 100 KB）")
    ap.add_argument("--verify", metavar="MANIFEST", help="只做导入后核对")
    args = ap.parse_args()

    token, acc, db = creds()

    # ---------- 核对模式 ----------
    if args.verify:
        m = json.load(open(args.verify, encoding="utf-8"))
        rows, meta = d1_query("SELECT COUNT(*) AS n FROM video", None, token, acc, db)
        n = rows[0]["n"]
        want = m["target_keys"]
        print(f"D1 行数 = {n:,} / 目标 = {want:,} (rows_read={meta.get('rows_read')})")
        if n != want:
            die(f"核对失败：D1 {n:,} != 目标 {want:,}")
        print("::notice::D1 同步核对通过")
        return

    ed1 = load_export_d1()

    print("读取 D1 现状 ...")
    current, rows_read = fetch_current(token, acc, db)
    print(f"D1 现状: {len(current):,} 行 (rows_read={rows_read:,})")

    print("读取新索引库 ...")
    best, total = ed1.collect(args.input)
    # export-d1.py 的键里 num 是「去前导零后的字符串」，D1 侧是 INTEGER，
    # 必须统一成 int，否则 292,621 行会全部判成「变了」。
    target = {(k[0], int(k[1])): v[1:] for k, v in best.items()}
    print(f"新索引库: {total:,} 原始行 → {len(target):,} 归一化键")

    upsert, delete = [], []
    for k, row in target.items():
        if current.get(k) != row:
            upsert.append((k, row))
    for k in current:
        if k not in target:
            delete.append(k)
    upsert.sort()
    delete.sort()

    changed = len(upsert) + len(delete)
    base = max(len(current), 1)
    ratio = changed / base
    print(f"\n差集: 新增/变更 {len(upsert):,} / 删除 {len(delete):,} "
          f"(占现有 {ratio:.1%})")

    if len(current) and not args.force and ratio > args.max_change_ratio:
        die(f"变化比例 {ratio:.1%} 超过阈值 {args.max_change_ratio:.0%}。"
            f"正常每周增量应远小于此；确认要全量重灌请加 --force。")
    if changed > 100_000:
        print(f"::warning::本次写入 {changed:,} 行，超过免费版 10 万行/天上限，"
              f"导入很可能失败（付费版无视此限制）")

    outdir = pathlib.Path(args.outdir)
    outdir.mkdir(parents=True, exist_ok=True)
    for f in outdir.glob("chunk_*.sql"):
        f.unlink()

    stmts = []
    files = []

    def flush():
        if not stmts:
            return
        idx = len(files)
        p = outdir / f"chunk_{idx:04d}.sql"
        head = ed1.D1_SCHEMA if idx == 0 else ""
        with open(p, "w", encoding="utf-8") as f:
            f.write(head + "\n".join(stmts) + "\n")
        files.append({"file": p.name, "statements": len(stmts)})
        print(f"  {p.name}: {len(stmts):,} 条")
        stmts.clear()

    for (letters, num), (cdn, dirpath, stem, q, v) in upsert:
        stmts.append(
            "INSERT OR REPLACE INTO video "
            "(k_letters,k_num,cdn,dirpath,stem,quality,variant) VALUES("
            f"{sql_str(letters)},{int(num)},{sql_str(cdn)},{sql_str(dirpath)},"
            f"{sql_str(stem)},{sql_str(q)},{sql_str(v)});"
        )
        if len(stmts) >= args.chunk:
            flush()
    for letters, num in delete:
        stmts.append(
            f"DELETE FROM video WHERE k_letters={sql_str(letters)} AND k_num={int(num)};"
        )
        if len(stmts) >= args.chunk:
            flush()
    flush()

    manifest = {
        "source_rows": total,
        "target_keys": len(target),
        "current_keys": len(current),
        "upsert": len(upsert),
        "delete": len(delete),
        "changed": bool(files),
        "d1_rows_read": rows_read,
        "chunks": files,
    }
    mpath = outdir / "manifest.json"
    with open(mpath, "w", encoding="utf-8") as f:
        json.dump(manifest, f, ensure_ascii=False, indent=2)
    print(f"\nmanifest → {mpath}")

    # 给 workflow / step summary 用
    gh_out = os.environ.get("GITHUB_OUTPUT")

    if args.apply:
        if not files:
            print("\n无变化，无需写入 D1。")
        else:
            up = build_upsert_batches(upsert, args.batch_bytes)
            de = build_delete_batches(delete, args.batch_bytes)
            print(f"\n写入 D1: upsert {len(up)} 批 / delete {len(de)} 批 ...", flush=True)
            w1, n1 = apply_batches(up, token, acc, db, "upsert")
            w2, n2 = apply_batches(de, token, acc, db, "delete")
            rows, meta = d1_query("SELECT COUNT(*) AS n FROM video", None, token, acc, db)
            n = rows[0]["n"]
            print(f"\nrows_written: upsert={w1:,} delete={w2:,} 合计={w1 + w2:,}")
            print(f"D1 行数 = {n:,} / 目标 = {len(target):,}")
            if n != len(target):
                die(f"写入后核对失败：D1 {n:,} != 目标 {len(target):,}")
            print("::notice::D1 增量同步完成并核对通过")
            manifest["applied"] = {"rows_written": w1 + w2,
                                   "requests": n1 + n2, "count_after": n}
            with open(mpath, "w", encoding="utf-8") as f:
                json.dump(manifest, f, ensure_ascii=False, indent=2)
            if gh_out:
                with open(gh_out, "a", encoding="utf-8") as f:
                    f.write(f"rows_written={w1 + w2}\n")

    if gh_out:
        with open(gh_out, "a", encoding="utf-8") as f:
            f.write(f"changed={'true' if files else 'false'}\n")
            f.write(f"upsert={len(upsert)}\n")
            f.write(f"delete={len(delete)}\n")
            f.write(f"expected={len(target)}\n")
            f.write(f"chunks={len(files)}\n")

    gh_sum = os.environ.get("GITHUB_STEP_SUMMARY")
    if gh_sum:
        with open(gh_sum, "a", encoding="utf-8") as f:
            f.write("\n### D1 增量同步\n")
            f.write(f"- D1 现状: **{len(current):,}** 行（本次读取 rows_read={rows_read:,}）\n")
            f.write(f"- 索引库目标: **{len(target):,}** 键（源库 {total:,} 行归一化后）\n")
            f.write(f"- 新增/变更: **{len(upsert):,}**\n")
            f.write(f"- 删除: **{len(delete):,}**\n")
            f.write(f"- SQL 分块: **{len(files)}** 个文件\n")
            if len(upsert) and len(upsert) <= 20:
                f.write(f"- 变更样例: {', '.join(f'{a}{b}' for (a, b), _ in upsert[:20])}\n")


if __name__ == "__main__":
    main()
