"""
export-d1.py — 从 dmm-index.db 导出 Cloudflare D1 导入 SQL

为什么需要单独脚本：
  build-index.py 已过 CI 验证（620,025 条逐条比对 0 不一致），不宜改动。
  D1 导出是独立的输出格式，放这里。

与 SQLite 版的关键差异：
  SQLite 存 cid 原文，D1 存「归一化键 (k_letters, k_num)」。
  原因：实测 620,025 行里 code 字段只有 18% 非空（cid 与番号非一一对应，
  厂牌带数字前缀如 ABP-888 → 118abp00888），番号查询实际靠归一化。
  归一化后 620,025 → 292,621 唯一键，D1 体积约 19 MB。

分片：按 k_letters 首字母的 ord % shard_count 切分，保证每片行数可控，
      避免触及免费版 10 万行/天写入上限。

用法:
  # 单片（先看行数）
  python export-d1.py -i dmm-index.db -o d1.sql

  # 切成 4 片（推荐，每片应 <10 万行）
  python export-d1.py -i dmm-index.db --shard-count 4 --shard 0 -o d1-0.sql
  python export-d1.py -i dmm-index.db --shard-count 4 --shard 1 -o d1-1.sql
  ...

  # 只统计不导出
  python export-d1.py -i dmm-index.db --stats
"""

import argparse
import os
import re
import sqlite3

D1_SCHEMA = """
PRAGMA foreign_keys = OFF;

CREATE TABLE IF NOT EXISTS video (
  k_letters TEXT NOT NULL,
  k_num     INTEGER NOT NULL,
  cdn       TEXT NOT NULL,
  dirpath   TEXT NOT NULL,
  stem      TEXT NOT NULL,
  quality   TEXT NOT NULL,
  variant   TEXT NOT NULL,
  PRIMARY KEY (k_letters, k_num)
) WITHOUT ROWID;
"""

# cid 形态：[厂牌数字][字母段][数字段][可选尾缀]
# 例 118abp00888 / ssis00095 / 1STARS00359 / 4ssis095r / h_491fone00062
CID_KEY_RE = re.compile(r"^(?P<pre>\d*)(?P<letters>[a-z_]+?)(?P<num>\d*)(?P<suf>r|re\d+|c|d)?$")


def cid_to_key(cid):
    """cid → (letters, num)；num 已去前导零。无法归一化返回 None（如 000_035）。"""
    m = CID_KEY_RE.match((cid or "").lower())
    if not m:
        return None
    letters = m.group("letters").strip("_")
    num = m.group("num")
    if not letters or not num:
        return None
    stripped = num.lstrip("0")
    if not stripped:
        return None
    return letters, stripped


def collect(db_path):
    """读索引库，按归一化键去重，返回 {key: (cdn, dirpath, stem, quality, variant, cdn统计)}"""
    conn = sqlite3.connect(db_path)
    db = conn.cursor()
    best = {}
    total = 0
    for cid, cdn, dirpath, stem, q, v, source, code in db.execute(
        "SELECT cid, cdn, dirpath, stem, quality, variant, source, code FROM video"
    ):
        total += 1
        k = cid_to_key(cid)
        if not k:
            continue
        # 破平优先级，与 build-index.py 的 Builder.offer 一致
        prio = (
            0 if code else 1,
            0 if source == "trailer" else 1,
            0 if cdn == "cc3001" else 1,
            len(k[0]),
        )
        cur = best.get(k)
        if cur is None or prio < cur[0]:
            best[k] = (prio, cdn, dirpath, stem, q, v)
    conn.close()
    return best, total


def shard_of(letters, shard_count):
    """
    按 k_letters 的首字母分片。

    不用 ord % n——那样分布很不均（实测 6 分片时最大片 95,383、最小 25,786）。
    改为按首字母顺序切等宽区间，负载更接近。
    """
    if shard_count <= 1:
        return 0
    # a-z 映射到 0..25；非字母开头（数字/下划线）归入第 0 片
    c = letters[0]
    idx = ord(c) - ord("a") if "a" <= c <= "z" else 0
    return min(idx * shard_count // 26, shard_count - 1)


def sql_str(s):
    return "'" + str(s).replace("'", "''") + "'"


def main():
    ap = argparse.ArgumentParser(description="导出 D1 导入 SQL（归一化键版）")
    ap.add_argument("-i", "--input", default="dmm-index.db", help="索引库路径")
    ap.add_argument("-o", "--output", help="输出 SQL 路径（与 --stats 互斥）")
    ap.add_argument("--shard", type=int, default=0, help="分片序号（从 0 起）")
    ap.add_argument("--shard-count", type=int, default=1, help="分片总数")
    ap.add_argument("--stats", action="store_true", help="只统计各分片行数，不导出")
    args = ap.parse_args()

    best, total = collect(args.input)
    print(f"源库 {total:,} 行 → 归一化唯一键 {len(best):,}")

    # 分桶
    shards = {}
    for key, val in best.items():
        b = shard_of(key[0], args.shard_count)
        shards.setdefault(b, []).append((key, val))

    if args.stats:
        print(f"\n分片分布（shard-count={args.shard_count}）:")
        warn = False
        for b in sorted(shards):
            n = len(shards[b])
            flag = " ⚠️ 超 10 万" if n > 100_000 else ""
            if n > 100_000:
                warn = True
            print(f"  shard {b}: {n:>8,}{flag}")
        # 首字母分布，便于手动调 shard_count
        heads = {}
        for letters, _ in best:
            heads[letters[0]] = heads.get(letters[0], 0) + 1
        top = sorted(heads.items(), key=lambda x: -x[1])[:6]
        print(f"\n首字母 Top6: " + ", ".join(f"{c}={n:,}" for c, n in top))
        if warn:
            print("\n建议增大 --shard-count（如 6 或 8）")
        return

    if not args.output:
        ap.error("需要 -o/--output 或 --stats")

    items = sorted(shards.get(args.shard, []))
    lines = [D1_SCHEMA] if args.shard == 0 else []
    for (letters, num), (_prio, cdn, dirpath, stem, q, v) in items:
        lines.append(
            f"INSERT OR REPLACE INTO video (k_letters,k_num,cdn,dirpath,stem,quality,variant) "
            f"VALUES({sql_str(letters)},{int(num)},{sql_str(cdn)},{sql_str(dirpath)},"
            f"{sql_str(stem)},{sql_str(q)},{sql_str(v)});"
        )
    with open(args.output, "w", encoding="utf-8") as f:
        f.write("\n".join(lines) + "\n")

    mb = os.path.getsize(args.output) / 1048576
    print(f"\n分片 {args.shard + 1}/{args.shard_count}: {len(items):,} 行 → {args.output}  ({mb:.1f} MB)")
    if len(items) > 100_000:
        print(f"  ⚠️ 超过免费版 10 万行/天上限，请增大 --shard-count")


if __name__ == "__main__":
    main()
