# -*- coding: utf-8 -*-
"""
把 `wrangler d1 export` 导出的 D1 video 表，转成 diff2sql.py 能吃的「索引库」格式。

为什么要转：diff2sql.py 用 export-d1.py 的 collect() 读库，它按
  SELECT cid, cdn, dirpath, stem, quality, variant, fmt, source, code
取列，再用 cidkey.cid_key(cid) 归一化成查询键。而 D1 里存的就是键本身
(k_letters, k_num)，没有 cid。所以合成一个「能还原出同一个键」的 cid：
  cid = k_letters + str(k_num).zfill(5)    例 ('tre', 24) -> 'tre00024'
反解：cidkey 对 'tre00024' 得到 ('tre','24')，与 D1 的键一致。
"""
import sqlite3
import sys

RAW = "d1-current.sql"
OUT = "d1-as-index.db"

# 1) 让 SQLite 自己解析导出的 SQL（比手写正则拆 VALUES 可靠得多）
raw = sqlite3.connect(":memory:")
raw.executescript(open(RAW, encoding="utf-8").read())
rows = list(raw.execute(
    "SELECT k_letters, k_num, cdn, dirpath, stem, quality, variant, fmt FROM video"))
print(f"D1 导出：{len(rows):,} 行")

# 2) 建成索引库形状
out = sqlite3.connect(OUT)
out.executescript("""
DROP TABLE IF EXISTS video;
CREATE TABLE video (
  cid TEXT PRIMARY KEY, code TEXT, cdn TEXT NOT NULL, dirpath TEXT NOT NULL,
  stem TEXT NOT NULL, quality TEXT NOT NULL, variant TEXT NOT NULL,
  fmt INTEGER NOT NULL DEFAULT 0, size_hint INTEGER NOT NULL DEFAULT 0,
  source TEXT NOT NULL, verified INTEGER NOT NULL DEFAULT 0,
  verified_bytes INTEGER NOT NULL DEFAULT 0, updated_at TEXT NOT NULL
) WITHOUT ROWID;
""")
now = "2026-10-08T17:24:50Z"   # D1 状态行的 updated_at，仅占位
data = []
for letters, num, cdn, dirpath, stem, q, v, fmt in rows:
    cid = f"{letters}{str(int(num)).zfill(5)}"
    data.append((cid, None, cdn, dirpath, stem, q, v, int(fmt or 0), 0, "sample", 0, 0, now))
out.executemany("INSERT OR REPLACE INTO video VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?)", data)
out.commit()

# 3) 自检：合成的 cid 必须能还原回原来的键
import cidkey
bad = 0
for letters, num, *_ in rows[:20000]:
    k = cidkey.cid_key(f"{letters}{str(int(num)).zfill(5)}")
    if k != (letters, str(int(num))):
        bad += 1
        if bad <= 3:
            print(f"  ⚠ 键还原失败: {letters}/{num} -> {k}")
print(f"键还原自检（抽样 20,000）：失败 {bad} 条")
print(f"写出 {OUT}: {len(data):,} 行")
