# -*- coding: utf-8 -*-
"""
migrate-kkey.py — 给索引库补上「归一化键」列与索引

为什么
------
原先查询靠 `dmm_index.py` 在进程内建一份「(字母段, 数字段) → [cid…]」的倒排索引，
79 万条要占 150~250 MB 内存。VPS 只有 2 GB 内存、可用不到 1 GB，还要和 docker
里一堆服务共存，这份倒排太贵。

改成**构建期算好、落库建索引**：
    k_letters TEXT    -- cidkey.cid_key(cid)[0]
    k_num     INTEGER -- cidkey.cid_key(cid)[1]（已去前导零）

查询退化成一条 `WHERE k_letters=? AND k_num=?` 的索引查找（<1 ms），
进程内存只剩 SQLite 的 page cache（可控到 16 MB）。

规则仍以 `cidkey.py` 为唯一事实来源 —— 这里只调用，不复制正则。

用法
----
    python migrate-kkey.py dmm-index.db            # 原地升级（幂等）
    python migrate-kkey.py in.db out.db            # 产出新文件，原库不动
    python migrate-kkey.py dmm-index.db --stats    # 只报告现状
"""

import argparse
import os
import sqlite3
import sys
import time

from cidkey import cid_key

# 与 D1 的列名保持一致，方便两边对拍
COLS = ("k_letters", "k_num")
INDEX = "idx_video_kkey"


def has_col(conn, table, col):
    return any(r[1] == col for r in conn.execute(f"PRAGMA table_info({table})"))


def ensure_cols(conn):
    added = []
    for col, typ in (("k_letters", "TEXT"), ("k_num", "INTEGER")):
        if not has_col(conn, "video", col):
            conn.execute(f"ALTER TABLE video ADD COLUMN {col} {typ}")
            added.append(col)
    return added


def backfill(conn, batch=20000):
    """
    回填两列。逐批 SELECT + UPDATE：
    79 万行一次性 fetchall 会吃掉几百 MB，分批把峰值压在 ~20 MB。
    WITHOUT ROWID 表没有 rowid，用 cid（主键）定位。
    """
    todo = conn.execute(
        "SELECT COUNT(*) FROM video WHERE k_letters IS NULL"
    ).fetchone()[0]
    if not todo:
        return 0, 0

    done = 0
    unparsed = 0
    last = ""
    t0 = time.time()
    while True:
        rows = conn.execute(
            "SELECT cid FROM video WHERE k_letters IS NULL AND cid > ? "
            "ORDER BY cid LIMIT ?",
            (last, batch),
        ).fetchall()
        if not rows:
            break
        upd = []
        for (cid,) in rows:
            k = cid_key(cid)
            if k:
                upd.append((k[0], int(k[1]), cid))
            else:
                # 解析不了的（venu12dod 这类字母-数字-字母）标空串而不是 NULL，
                # 否则每轮都会被 WHERE k_letters IS NULL 再捞一次，变成死循环
                upd.append(("", 0, cid))
                unparsed += 1
        conn.executemany(
            "UPDATE video SET k_letters=?, k_num=? WHERE cid=?", upd
        )
        conn.commit()
        done += len(rows)
        last = rows[-1][0]
        print(f"  回填 {done:,} 行  ({time.time()-t0:.1f}s)", end="\r", flush=True)
    print()
    return done, unparsed


def ensure_index(conn):
    conn.execute(
        f"CREATE INDEX IF NOT EXISTS {INDEX} ON video(k_letters, k_num)"
    )
    conn.commit()


def report(conn):
    total = conn.execute("SELECT COUNT(*) FROM video").fetchone()[0]
    keyed = conn.execute(
        "SELECT COUNT(*) FROM video WHERE k_letters IS NOT NULL AND k_letters <> ''"
    ).fetchone()[0]
    unparsed = conn.execute(
        "SELECT COUNT(*) FROM video WHERE k_letters = ''"
    ).fetchone()[0]
    dup = conn.execute(
        "SELECT COUNT(*) FROM (SELECT k_letters, k_num FROM video "
        "WHERE k_letters <> '' GROUP BY 1,2 HAVING COUNT(*) > 1)"
    ).fetchone()[0]
    pages = conn.execute("PRAGMA page_count").fetchone()[0]
    psize = conn.execute("PRAGMA page_size").fetchone()[0]
    print(f"  总行数        {total:,}")
    print(f"  可查询键      {keyed:,}  ({100*keyed/total:.1f}%)")
    print(f"  无法归一化    {unparsed:,}")
    print(f"  一键多 cid    {dup:,} 个键")
    print(f"  库体积        {pages*psize/1048576:.1f} MB")


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("src")
    ap.add_argument("dst", nargs="?")
    ap.add_argument("--stats", action="store_true", help="只报告，不改动")
    args = ap.parse_args()

    if args.stats:
        conn = sqlite3.connect(f"file:{args.src}?mode=ro", uri=True)
        report(conn)
        return

    dst = args.dst or args.src
    if dst != args.src:
        if os.path.exists(dst):
            sys.exit(f"目标已存在，拒绝覆盖：{dst}")
        print(f"复制 {args.src} → {dst}")
        with open(args.src, "rb") as f, open(dst, "wb") as g:
            while True:
                b = f.read(1 << 20)
                if not b:
                    break
                g.write(b)

    conn = sqlite3.connect(dst)
    conn.execute("PRAGMA journal_mode=WAL")
    conn.execute("PRAGMA synchronous=NORMAL")

    print("补列…")
    added = ensure_cols(conn)
    print(f"  新增列：{added or '已存在'}")

    print("回填…")
    t0 = time.time()
    done, unparsed = backfill(conn)
    print(f"  回填 {done:,} 行，{unparsed:,} 行无法归一化，耗时 {time.time()-t0:.1f}s")

    print("建索引…")
    t0 = time.time()
    ensure_index(conn)
    print(f"  耗时 {time.time()-t0:.1f}s")

    print("整理…")
    conn.execute("PRAGMA journal_mode=DELETE")   # 收掉 -wal/-shm，便于单文件分发
    conn.execute("VACUUM")
    conn.close()

    print("结果：")
    conn = sqlite3.connect(f"file:{dst}?mode=ro", uri=True)
    report(conn)
    print(f"  文件体积      {os.path.getsize(dst)/1048576:.1f} MB")


if __name__ == "__main__":
    main()
