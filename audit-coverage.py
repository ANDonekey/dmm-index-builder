# -*- coding: utf-8 -*-
"""
audit-coverage.py — 索引库「召回率」独立审计（只读）

为什么需要它：
  verify-index.py 与 build-index.py 共用同一份正则与 build_url，只能验证
  「已入库的字段能不能拼回原 URL」，**无法验证「该入库的是否都入了」**。
  实测：把库砍到只剩 10.6% 的记录，verify-index.py 依然输出 ✅ 并 exit 0。

  本脚本不复用被测代码的任何正则，直接对 dump 做独立统计，回答两个问题：
    1. dump 里出现了预览 URL 的番号键，库里有几个？（召回率）
    2. 库里的 cid 有多少根本没法归一化成查询键？（导出侧静默丢弃）

用法：
  python audit-coverage.py --dump r18dotdev_dump_2026-09-29.sql --db dmm-index.db
  python audit-coverage.py --dump dump.sql --db dmm-index.db --min-coverage 0.98

退出码：0 = 召回率达到阈值；1 = 低于阈值（CI 应据此 fail）
"""

import argparse
import re
import sqlite3
import sys

# dump 里两张目标表 → 预览 URL 所在的列号
TABLES = {"source_dmm_trailer": 1, "derived_video": 8}
PART_URL = "/litevideo/-/part/=/cid="

# 旧规则（P0-3 修复前的实现）。此处刻意**照抄历史版本**留作对照：
# 本脚本的定位是「不依赖被测代码的独立审计」，所以不 import cidkey.py。
# ⚠️ 如果哪天这里的旧规则和新规则又对不上了，那是新规则跑偏的信号 —— 放宽规则
#    必须是旧规则的严格超集（实测过 0 误配），否则就是回归。
_CUR = re.compile(r"^(?P<pre>\d*)(?P<letters>[a-z_]+?)(?P<num>\d*)(?P<suf>r|re\d+|c|d)?$")
# 放宽后的规则：字母段贪婪、数字段非空、先剥厂牌前缀
_PREFIX = re.compile(r"^(?:h_\d+|\d+_)")
_RELAXED = re.compile(r"^(?P<pre>\d*)(?P<letters>[a-z_]+)(?P<num>\d+)(?:r|re\d+|c|d)?$")


def key_current(cid):
    m = _CUR.match((cid or "").lower())
    if not m:
        return None
    letters = m.group("letters").strip("_")
    num = m.group("num")
    if not letters or not num:
        return None
    stripped = num.lstrip("0")
    return (letters, stripped) if stripped else None


def key_relaxed(cid):
    s = _PREFIX.sub("", (cid or "").lower())
    m = _RELAXED.match(s)
    if not m:
        return None
    letters = m.group("letters").strip("_")
    num = m.group("num").lstrip("0")
    return (letters, num) if letters and num else None


def dump_keys(path):
    """独立扫描 dump：返回 (有预览 URL 的查询键集合, URL 总数, part 页数)"""
    keys, part_only, total, n_part = set(), set(), 0, 0
    in_block = None
    with open(path, "r", encoding="utf-8", errors="replace", newline="") as f:
        for line in f:
            line = line.rstrip("\r\n")
            if in_block is None:
                if line.startswith("COPY public."):
                    m = re.match(r"COPY public\.(\w+)", line)
                    name = m.group(1) if m else None
                    in_block = name if name in TABLES else "__skip__"
                continue
            if line.startswith("\\."):
                in_block = None
                continue
            if in_block == "__skip__":
                continue
            p = line.split("\t")
            idx = TABLES[in_block]
            if len(p) <= idx:
                continue
            url = p[idx]
            if url == "\\N" or not url:
                continue
            total += 1
            k = key_relaxed(p[0])
            if not k:
                continue
            keys.add(k)
            if PART_URL in url:
                n_part += 1
                part_only.add(k)
    return keys, part_only, total, n_part


def main():
    ap = argparse.ArgumentParser(description="索引库召回率独立审计")
    ap.add_argument("--dump", required=True)
    ap.add_argument("--db", required=True)
    ap.add_argument("--min-coverage", type=float, default=0.98,
                    help="召回率下限，低于则 exit 1（默认 0.98）")
    args = ap.parse_args()

    dk, part_only, total_url, n_part = dump_keys(args.dump)
    print(f"dump : 预览 URL {total_url:,} 条（其中 part 播放页 {n_part:,}）"
          f" → 查询键 {len(dk):,}")

    conn = sqlite3.connect(f"file:{args.db}?mode=ro", uri=True)
    rows = [r[0] for r in conn.execute("SELECT cid FROM video")]
    cur_keys, rel_keys = set(), set()
    for cid in rows:
        a, b = key_current(cid), key_relaxed(cid)
        if a:
            cur_keys.add(a)
        if b:
            rel_keys.add(b)
    print(f"索引库: {len(rows):,} 行 → 旧规则键 {len(cur_keys):,} / 放宽规则键 {len(rel_keys):,}")
    if cur_keys - rel_keys:
        print(f"  ⚠️ 旧规则能解析、新规则解析不了的键 {len(cur_keys - rel_keys):,} 个 —— "
              f"新规则不是超集，属回归")

    missing = dk - rel_keys
    cov = 1 - len(missing) / max(len(dk), 1)
    print()
    print(f"召回率（dump 有、库中没有）: {cov:.4%}   缺口 {len(missing):,} 个键")
    part_gap = len(part_only - rel_keys)
    print(f"  其中仅出现在 part 播放页里的缺口: {part_gap:,}")

    dropped = len(rows) - sum(1 for cid in rows if key_relaxed(cid))
    print(f"导出侧丢弃（cid 无法归一化的行）: {dropped:,} "
          f"({dropped / max(len(rows), 1):.2%})")
    n_cur_drop = len(rows) - len([1 for cid in rows if key_current(cid)])
    print(f"  若仍用旧规则，丢弃为: {n_cur_drop:,} "
          f"({n_cur_drop / max(len(rows), 1):.2%})")

    if missing:
        print("\n缺口样例:", sorted(missing)[:10])

    if cov < args.min_coverage:
        print(f"\n::error::召回率 {cov:.4%} 低于阈值 {args.min_coverage:.2%}")
        return 1
    print(f"\n✅ 召回率达标（≥ {args.min_coverage:.2%}）")
    return 0


if __name__ == "__main__":
    sys.exit(main())
