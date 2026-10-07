"""
diff2sql.py — 本地算「旧索引库 → 新索引库」的差集，打包成可分片执行的 SQL

为什么要单独跑（而不是让 CI 的 sync-d1.py 干）：
  sync-d1.py 默认用 --prev-from-release，会从 tag `index-<date>` 拉旧库。
  但本次改的是构建代码，dump 日期没变（还是 2026-10-06），
  CI 一旦发布新版就会把同一个 tag 的文件覆盖掉 —— 再拉就拉到新库，差集算成 0。
  所以这里显式指定旧库路径，差集才是确定的，分片续传也才有意义。

  D1 REST API 需要 API Token（本地没有，wrangler 的 OAuth 令牌不被接受），
  所以落盘成 SQL 文件，改用 `wrangler d1 execute --file` 执行。

用法:
  python diff2sql.py --new dmm-index.db --old prev-1006/dmm-index.db --rows-per-file 19000
"""
import argparse
import importlib.util
import json
import os
import pathlib

PROJ = pathlib.Path(__file__).resolve().parent  # 与 sync-d1.py / export-d1.py 同级
UPSERT_COLS = "(k_letters,k_num,cdn,dirpath,stem,quality,variant,fmt)"
BATCH_BYTES = 80_000


def load(name, path):
    spec = importlib.util.spec_from_file_location(name, path)
    m = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(m)
    return m


sd = load("sync_d1", PROJ / "sync-d1.py")
ed1 = sd.load_export_d1()


def sql_str(s):
    return "'" + str(s).replace("'", "''") + "'"


def pack(upsert, limit=BATCH_BYTES):
    """把多行塞进一条 INSERT（D1 单条 SQL ≤100KB），返回 [(sql, nrows)]。"""
    out, cur, size, n = [], [], 0, 0
    head = "INSERT OR REPLACE INTO video " + UPSERT_COLS + " VALUES "
    for (letters, num), (cdn, dp, stem, q, v, fmt) in upsert:
        tup = (f"({sql_str(letters)},{int(num)},{sql_str(cdn)},{sql_str(dp)},"
               f"{sql_str(stem)},{sql_str(q)},{sql_str(v)},{int(fmt)})")
        if cur and len(head) + size + len(tup) + 2 > limit:
            out.append((head + ",".join(cur) + ";", n))
            cur, size, n = [], 0, 0
        cur.append(tup)
        size += len(tup) + 1
        n += 1
    if cur:
        out.append((head + ",".join(cur) + ";", n))
    return out


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("--new", required=True)
    ap.add_argument("--old", required=True)
    ap.add_argument("--outdir", default="d1-parts")
    ap.add_argument("--rows-per-file", type=int, default=19000)
    ap.add_argument("--dry-run", action="store_true")
    args = ap.parse_args()

    target, trows = sd.load_target(args.new, ed1)
    old, orows = sd.load_target(args.old, ed1)
    print(f"新库: {trows:,} 行 → {len(target):,} 键")
    print(f"旧库: {orows:,} 行 → {len(old):,} 键")

    upsert = [(k, v) for k, v in target.items() if old.get(k) != v]
    delete = [k for k in old if k not in target]
    upsert.sort()
    delete.sort()
    print(f"\n差集: 新增/变更 {len(upsert):,} / 删除 {len(delete):,}")

    # 抽样看看 fmt 分布，确认新体系确实进来了
    from collections import Counter
    c = Counter(v[5] for _k, v in upsert)
    print("差集 fmt 分布:", dict(sorted(c.items())))
    c2 = Counter(v[3] for _k, v in upsert)
    print("差集 quality 分布:", dict(sorted(c2.items())))

    if args.dry_run:
        return

    stmts = pack(upsert)
    print(f"打包成 {len(stmts)} 条 INSERT 语句")

    outdir = pathlib.Path(args.outdir)
    outdir.mkdir(parents=True, exist_ok=True)
    files, cur, n = [], [], 0
    for sql, nrows in stmts:
        cur.append(sql)
        n += nrows
        if n >= args.rows_per_file:
            idx = len(files) + 1
            p = outdir / f"part{idx:02d}.sql"
            p.write_text("\n".join(cur) + "\n", encoding="utf-8")
            files.append({"file": p.name, "rows": n,
                          "mb": round(os.path.getsize(p) / 1048576, 2)})
            print(f"  {p.name}: {n:,} 行, {files[-1]['mb']} MB")
            cur, n = [], 0
    if cur:
        idx = len(files) + 1
        p = outdir / f"part{idx:02d}.sql"
        p.write_text("\n".join(cur) + "\n", encoding="utf-8")
        files.append({"file": p.name, "rows": n,
                      "mb": round(os.path.getsize(p) / 1048576, 2)})
        print(f"  {p.name}: {n:,} 行, {files[-1]['mb']} MB")

    total = sum(f["rows"] for f in files)
    manifest = {"new": args.new, "old": args.old,
                "upsert": len(upsert), "delete": len(delete),
                "files": files, "total_rows": total}
    (outdir / "manifest.json").write_text(
        json.dumps(manifest, ensure_ascii=False, indent=2), encoding="utf-8")
    print(f"\n合计 {total:,} 行 / {len(files)} 个文件")


if __name__ == "__main__":
    main()
