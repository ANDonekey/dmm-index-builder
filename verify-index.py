"""
校验 build-index.py 的产物。**两道闸，缺一不可**：

  1. 自洽：库里分列字段拼出的 URL，必须与 dump 里的原始 URL 逐条一致；
  2. 召回：dump 里「预览 URL 列非空」的 content_id，库里必须覆盖到（默认 ≥ 98%）。

为什么必须加第 2 条（审查报告 P0-1）：
  本脚本直接 `import` 了被测代码（normalize_and_parse / build_url 都来自 build-index.py），
  所以它只能回答「已入库的字段能不能拼回原 URL」，**回答不了「该入库的是否都入了」**：
  - 它用同一个正则从 dump 重建基准集，正则不认的 URL 两边都不认 → 差异恒为 0；
  - 库里缺 cid 时它不算 mismatch（没有 expected − actual 这一项）；
  - `not_in_dump` 以前只打印、不参与退出码。

  实测：把库砍到只剩 10.6% 的记录（DELETE 掉 89.4%），本脚本照样输出 ✅ 并 exit 0。
  召回断言不看被测正则 —— 分母是 dump 里 URL 列非空的 content_id 全集。

⚠️ 基准表有两张，缺一不可：
   source_dmm_trailer  —— 档位普遍较高（trailer）
   derived_video       —— 档位较低但覆盖不同 cid（sample）
   合并规则是「同一 cid 取更高档」，所以一条记录可能来自任一张表；
   只拿其中一张当基准会误报（早期版本就因此报了 1503 条假不一致）。

用法：
  python verify-index.py                          # 用默认路径 dump.sql / dmm-index.db
  python verify-index.py dump.sql dmm-index.db    # 显式指定（CI 用这个）
  python verify-index.py dump.sql.gz idx.db       # .gz 也能直接读

退出码：0 = 自洽且召回达标；1 = 任一不达标（CI 应据此 fail）
"""

import argparse
import gzip
import importlib.util
import pathlib
import re
import sqlite3
import sys

# URL 形态（freepv/pv × A/B 命名）只有一处定义，就是 build-index.py。
# 校验器直接复用它，避免两边各写一份正则而悄悄跑偏 —— 以前就是这么漏掉 pv 体系的。
_spec = importlib.util.spec_from_file_location(
    "build_index", pathlib.Path(__file__).with_name("build-index.py"))
_bi = importlib.util.module_from_spec(_spec)
_spec.loader.exec_module(_bi)
normalize_and_parse = _bi.normalize_and_parse
build_url = _bi.build_url
RANK = _bi.QUALITY_RANK

# 两张基准表 → (内部名, URL 所在列号, 至少要有几列)
TABLES = {
    "source_dmm_trailer": ("trailer", 1, 2),
    "derived_video": ("sample", 8, 9),
}
NULL = "\\N"


def open_dump(path):
    if path.endswith(".gz"):
        return gzip.open(path, "rt", encoding="utf-8", errors="replace", newline="")
    return open(path, "r", encoding="utf-8", errors="replace", newline="")


def norm(u, strict=False):
    """
    比对前的 URL 归一化，返回 (归一化后的 URL, 是否改写了 host)。

    ⚠️ `cc3001.dmm.com → cc3001.dmm.co.jp` 这层改写会**抹掉真实差异**（P1-1）：
       build_url() 永远输出 `.co.jp`，而 dump 里有 23,570 条原文是 `.com`。
       以前这层改写无声无息，于是「改了数据」+「看不见」同时成立。

       现在默认仍改写（否则闸门会立刻变红），但改写条数会被统计并打印出来；
       加 `--strict-host` 则**不**改写，用于人工核对这个差异究竟有多大。
    """
    s = u.replace("http://", "https://")
    if strict:
        return s, False
    if "cc3001.dmm.com" in s:
        return s.replace("cc3001.dmm.com", "cc3001.dmm.co.jp"), True
    return s, False


def scan_dump(path, strict_host=False):
    """
    单遍扫描 dump，返回 (original, url_cids, host_rewritten)。

      original  {trailer|sample: {cid: (rank, 归一化 URL)}} —— 只收能解析的
      url_cids  dump 里「预览 URL 列非空」的 content_id 全集 —— **不看能否解析**，
                这正是召回断言的分母：解析不了不代表不该收录。
    """
    original = {"trailer": {}, "sample": {}}
    url_cids = set()
    host_rewritten = 0
    cur = None            # 当前 COPY 块的表名；"__skip__" 表示不关心的表
    with open_dump(path) as f:
        for raw in f:
            line = raw.rstrip("\r\n")
            if cur is None:
                if line.startswith("COPY public."):
                    m = re.match(r"COPY public\.(\w+)", line)
                    cur = m.group(1) if m and m.group(1) in TABLES else "__skip__"
                continue
            if line.startswith("\\."):
                cur = None
                continue
            if cur == "__skip__":
                continue
            key, url_idx, min_cols = TABLES[cur]
            p = line.split("\t")
            if len(p) < min_cols:
                continue
            url = p[url_idx]
            if not url or url == NULL:
                continue
            url_cids.add(p[0])
            parsed = normalize_and_parse(url)
            if not parsed:
                continue
            q = parsed[2]
            if q not in RANK:
                continue
            nu, rew = norm(url, strict_host)
            if rew:
                host_rewritten += 1
            original[key][p[0]] = (RANK[q], nu)
    return original, url_cids, host_rewritten


def main():
    ap = argparse.ArgumentParser(description="校验索引库与 dump 的一致性（自洽 + 召回）")
    ap.add_argument("dump", nargs="?", default="dump.sql", help="dump 路径（.sql / .sql.gz）")
    ap.add_argument("db", nargs="?", default="dmm-index.db", help="索引库路径")
    ap.add_argument("--min-recall", type=float, default=0.98,
                    help="召回率下限，低于则 exit 1（默认 0.98）")
    ap.add_argument("--strict-host", action="store_true",
                    help="不做 cc3001.dmm.com → .dmm.co.jp 改写（用于核对 P1-1 的真实差异量）")
    args = ap.parse_args()

    original, url_cids, host_rewritten = scan_dump(args.dump, args.strict_host)
    print(f"dump: trailer {len(original['trailer']):,} / sample {len(original['sample']):,}")
    print(f"dump: 预览 URL 列非空的 content_id {len(url_cids):,} 个（召回断言的分母）")
    if host_rewritten:
        print(f"ℹ️  host 改写（.dmm.com → .dmm.co.jp）{host_rewritten:,} 条"
              f"{'（--strict-host 已关闭改写）' if args.strict_host else ''}")

    # 2) 校验：拼装结果必须等于「两表按(档位,来源)取优」后的值
    c = sqlite3.connect(args.db)
    rows = c.execute(
        "SELECT cid, cdn, dirpath, stem, quality, variant, fmt FROM video"
    ).fetchall()
    print(f"索引库: {len(rows):,}\n")

    exact = mismatch = not_in_dump = 0
    bad = []
    db_cids = set()
    for cid, cdn, dirpath, stem, quality, variant, fmt in rows:
        db_cids.add(cid)
        built = build_url(cdn, dirpath, stem, quality, variant, fmt)
        # URL 结构自校验：拼出来的串必须能被原样解析回去（覆盖 4 种形态）
        if normalize_and_parse(built) != (dirpath, stem, quality, variant, cdn, fmt):
            mismatch += 1
            if len(bad) < 5:
                bad.append((cid, "URL 结构自校验失败", built))
            continue
        rank = RANK.get(quality)
        cands = [d[cid] for d in original.values() if cid in d]
        if not cands:
            not_in_dump += 1
            continue
        best_rank, best_url = max(cands, key=lambda x: x[0])
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

    # 3) 召回断言 —— 与被测正则解耦，是「漏收」唯一的防线
    covered = len(db_cids & url_cids)
    missing = len(url_cids - db_cids)
    recall = covered / len(url_cids) if url_cids else 1.0
    print(f"\n召回: 库覆盖 {covered:,} / dump 有 {len(url_cids):,} = {recall:.4%} "
          f"（缺口 {missing:,} 个 content_id）")

    failed = False
    if mismatch:
        print("::error::存在与 dump 不一致的记录")
        failed = True
    if not_in_dump:
        # 以前这一项只打印、不参与退出码 —— 库里凭空多出来的 cid 就这么被放过去了
        print(f"::error::库中有 {not_in_dump:,} 个 cid 在 dump 里找不到对应记录")
        failed = True
    if recall < args.min_recall:
        print(f"::error::召回率 {recall:.4%} 低于阈值 {args.min_recall:.2%}"
              f"（缺口 {missing:,} 个 content_id）")
        failed = True

    return 1 if failed else 0


if __name__ == "__main__":
    sys.exit(main())
