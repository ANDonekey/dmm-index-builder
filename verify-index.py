"""
校验 build-index.py 的产物：分列字段拼出的 URL 与 dump 里的原始 URL 完全一致。

这是比"联网实测"更可靠的验证——它逐条比对了 62 万条记录，只要有一条拼错就会列出来。
联网实测只抽 3 条，且受代理出口影响。

⚠️ 基准表有两张，缺一不可：
   source_dmm_trailer  —— 档位普遍较高（trailer）
   derived_video       —— 档位较低但覆盖不同 cid（sample）
   合并规则是「同一 cid 取更高档」，所以一条记录可能来自任一张表；
   只拿其中一张当基准会误报（早期版本就因此报了 1503 条假不一致）。
"""

import re
import sqlite3
import sys

DUMP = r"F:\codx\DmmRequest\r18dotdev_dump_2026-09-29.sql"
DB = r"F:\codx\DmmRequest\dmm-index.db"
PREFIX_RE = re.compile(
    r"^https://(?P<cdn>" + "|".join(("cc3001", "pv3001", "cc3002", "cc3003"))
    + r")\.dmm\.co\.jp/litevideo/freepv/[^/]+/[^/]+/(?P<stem>[^/]+)/"
      r"(?P=stem)_(?P<q>[a-z0-9]+)_(?P<v>[ws])\.mp4$"
)
QRE = re.compile(r"_([a-z0-9]+)_([ws])\.mp4$")
RANK = {"hhb": 5, "mhb": 4, "dmb": 3, "dm": 2, "sm": 1}


def norm(u):
    return u.replace("http://", "https://").replace("cc3001.dmm.com", "cc3001.dmm.co.jp")


def quality_of(url):
    m = QRE.search(url)
    return m.group(1) if m else None


# 1) 从 dump 重建 cid -> (档位rank, 归一化 URL)
#    trailer 表 3 列、derived_video 20 列，URL 都在第 1 / 第 8 列
original = {"trailer": {}, "sample": {}}
for table, key, url_idx, min_cols in (
    ("source_dmm_trailer ", "trailer", 1, 2),
    ("derived_video (", "sample", 8, 9),
):
    in_block = False
    with open(DUMP, "r", encoding="utf-8") as f:
        for raw in f:
            line = raw.rstrip("\r\n")
            if not in_block:
                if line.startswith(f"COPY public.{table}"):
                    in_block = True
                continue
            if line.startswith("\\."):
                break
            p = line.split("\t")
            if len(p) < min_cols or p[url_idx] == "\\N" or "freepv" not in p[url_idx]:
                continue
            q = quality_of(p[url_idx])
            if q in RANK:
                original[key][p[0]] = (RANK[q], norm(p[url_idx]))

print(f"dump: trailer {len(original['trailer']):,} / sample {len(original['sample']):,}")

# 2) 校验：拼装结果必须等于「两表按档位取高」后的值
c = sqlite3.connect(DB)
rows = c.execute("SELECT cid, cdn, dirpath, stem, quality, variant FROM video").fetchall()
print(f"索引库: {len(rows):,}\n")

exact = mismatch = not_in_dump = 0
bad = []
for cid, cdn, dirpath, stem, quality, variant in rows:
    built = f"https://{cdn}.dmm.co.jp/litevideo/freepv/{dirpath}/{stem}_{quality}_{variant}.mp4"
    m = PREFIX_RE.match(built)
    rank = RANK.get(quality)
    if not m or m.group("stem") != stem or m.group("q") != quality or m.group("v") != variant:
        mismatch += 1
        if len(bad) < 5:
            bad.append((cid, "URL 结构自校验失败", built))
        continue
    cands = [d[cid] for d in original.values() if cid in d]
    if not cands:
        not_in_dump += 1
        continue
    # 合并规则：先比档位（rank 大者胜），档位相同 trailer 优先
    best_rank, best_url = max(
        cands, key=lambda x: (x[0], 1 if x[1].startswith("pv") or True else 0)
    )
    if built == best_url and rank == best_rank:
        exact += 1
    else:
        mismatch += 1
        if len(bad) < 5:
            bad.append((cid, best_url, built))

print(f"✅ 与 dump 一致: {exact:,}")
print(f"❌ 不一致:      {mismatch:,}")
print(f"ℹ️  dump 中无对应: {not_in_dump:,}")
if bad:
    print("\n不一致样例:")
    for cid, src, built in bad:
        print(f"  {cid}\n    期望: {src}\n    实得: {built}")

sys.exit(1 if mismatch else 0)
