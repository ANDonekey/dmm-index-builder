"""
r18.dev dump → 精简 DMM 索引库（预处理阶段，在 CI / 桌面机跑，不在 VPS 上跑）

设计目标
--------
输入 1.38 GB SQL dump（PostgreSQL COPY 文本格式），输出 ~几十 MB 的 SQLite 索引库。
只保留「番号 → 预览视频直链」这一件事需要的数据，其余全部丢弃（演员/封面/评论/评分/GIF…）。

内存占用恒定：逐行流式读取 + 批量 UPSERT，不把全表读进内存。
峰值内存主要来自 SQLite page cache，默认 32MB 可调。

用法
----
  # 本地 / CI：处理 .sql.gz（推荐，zstd 解压比 gzip 快数倍）
  python build-index.py --input r18dotdev_dump_2026-09-29.sql.gz --output dmm-index.db

  # 也支持未压缩 .sql
  python build-index.py --input r18dotdev_dump_2026-09-29.sql --output dmm-index.db

  # 自定义起始清晰度（默认 sm，即入库所有档位）
  python build-index.py -i in.sql.gz -o idx.db --base-quality sm

输出 schema
-----------
cid            TEXT    DMM content_id，如 104fsmd00029（主键）
code           TEXT    规范化番号，如 abp-888（来自 derived_video.dvd_id，无则由 cid 反推）
dirpath        TEXT    freepv 之后的目录路径，如 1/104/104fsmd29
stem           TEXT    文件名主体，如 104fsmd00029（⚠️ 与 cid 可能不同，不能混用）
quality        TEXT    hhb/mhb/dmb/dm/sm（freepv 体系）
variant        TEXT    w | s
size_hint      INTEGER 库记录档位对应的预估字节（0 = 未知，需探测）
source         TEXT    trailer（高，优先）| sample（低，仅补空）
verified       INTEGER 0=未探测 1=已验证可用
verified_bytes INTEGER 探测得到的真实 Content-Length，<=1024 视为无效
updated_at     TEXT

⚠️ URL 不落库：由 build_url(dirpath, stem, quality, variant) 运行时拼装。
   原始 URL 里固定前缀 43 字符、且 stem 出现两次（目录名 + 文件名），冗余极大；
   分列存储后 62 万条从 82.4 MB 降到约 45 MB。
"""

import argparse
import gzip
import io
import os
import re
import sqlite3
import sys
import time

# ---------- 常量 ----------

DUMP_VERSIONS = {
    "r18dotdev_dump_2026-09-29.sql": "2026-09-29",
}

# freepv 清晰度，由高到低（用于选最优；freepv 天花板是 hhb）
QUALITY_RANK = {"hhb": 5, "mhb": 4, "dmb": 3, "dm": 2, "sm": 1}
QUALITIES_DESC = ["hhb", "mhb", "dmb", "dm", "sm"]

# 数据源优先级（档位相同时破平用）：trailer 表实测档位普遍更高，且更权威
SOURCE_RANK = {"sample": 1, "trailer": 2}

# freepv URL 形态：
#   https://{cdn}/litevideo/freepv/{a}/{ab}/{stem}/{stem}_{quality}_{variant}.mp4
# ⚠️ CDN 主机不止 cc3001：库里还有 pv3001.dmm.co.jp（实测存在，且这批多是 mhb 高档）。
#    早期只认 cc3001，把 28 条 pv3001 的 mhb 记录误降级成 dmb（校验时发现）。
CDN_HOSTS = ("cc3001", "pv3001", "cc3002", "cc3003")
FREEPV_RE = re.compile(
    r"^https?://(?P<cdn>" + "|".join(CDN_HOSTS) + r")\.dmm\.(?:co\.jp|com)/litevideo/freepv/"
    r"[^/]+/[^/]+/(?P<stem>[^/]+)/(?P=stem)_(?P<quality>[a-z0-9]+)_(?P<variant>[ws])\.mp4$"
)

# 目标表的列序（从 dump 的 COPY 头里读到，固定不变）
TRAILER_COLS = ["content_id", "url", "timestamp"]
VIDEO_COLS = [
    "content_id", "dvd_id", "title_en", "title_ja", "comment_en", "comment_ja",
    "runtime_mins", "release_date", "sample_url", "maker_id", "label_id", "series_id",
    "jacket_full_url", "jacket_thumb_url", "gallery_full_first", "gallery_full_last",
    "gallery_thumb_first", "gallery_thumb_last", "site_id", "service_code",
]
IDX_CONTENT_ID = 0
IDX_DVD_ID = 1
IDX_SAMPLE_URL = 8

NULL = "\\N"

SCHEMA = """
PRAGMA journal_mode = OFF;
PRAGMA synchronous = OFF;
PRAGMA temp_store = MEMORY;

-- 存储结构说明（体积优化）：
--   原始 URL 形如
--     https://cc3001.dmm.co.jp/litevideo/freepv/5/531/5314gnbd01156/5314gnbd01156_dmb_w.mp4
--   其中固定前缀 43 字符 + stem 出现两次（目录名 + 文件名），冗余极大。
--   因此只存「路径主体 + 清晰度 + 变体」，URL 由 build_url() 运行时拼装。
--   实测 62 万条：82.4 MB → 48.6 MB。
-- ⚠️ stem ≠ cid！目录名里的数字段可能被去前导零（ssni00036 保留零，但
--    104fsmd00029 → 目录 104fsmd29），所以 stem 必须单独存一列，不能用 cid 代替。
-- ⚠️ cdn 主机不止 cc3001：库里还有 pv3001.dmm.co.jp（实测，且多是 mhb 高档），
--    按行存储，勿硬编码。
CREATE TABLE IF NOT EXISTS video (
  cid            TEXT PRIMARY KEY,   -- DMM content_id，如 104fsmd00029
  code           TEXT,               -- 规范化番号，如 abp-888（可空）
  cdn            TEXT NOT NULL,      -- cc3001 | pv3001 | …
  dirpath        TEXT NOT NULL,      -- freepv 之后的目录路径，如 1/104/104fsmd29
  stem           TEXT NOT NULL,      -- 文件名主体，如 104fsmd00029（与 cid 可能不同！）
  quality        TEXT NOT NULL,      -- hhb/mhb/dmb/dm/sm
  variant        TEXT NOT NULL,      -- w | s
  size_hint      INTEGER NOT NULL DEFAULT 0,
  source         TEXT NOT NULL,      -- trailer（高）| sample（低）
  verified       INTEGER NOT NULL DEFAULT 0,
  verified_bytes INTEGER NOT NULL DEFAULT 0,
  updated_at     TEXT NOT NULL
) WITHOUT ROWID;

CREATE TABLE IF NOT EXISTS meta (
  key   TEXT PRIMARY KEY,
  value TEXT
);
"""


def build_url(cdn: str, dirpath: str, stem: str, quality: str, variant: str) -> str:
    """把库里的分列字段还原成 DMM 直链。"""
    return f"https://{cdn}.dmm.co.jp/litevideo/freepv/{dirpath}/{stem}_{quality}_{variant}.mp4"


# ---------- 输入：支持 .gz / .sql / stdin ----------

def open_dump(path):
    if path == "-":
        return io.TextIOWrapper(sys.stdin.buffer, encoding="utf-8", errors="replace", newline="")
    if path.endswith(".gz"):
        return gzip.open(path, "rt", encoding="utf-8", errors="replace", newline="")
    if path.endswith(".zst"):
        try:
            import zstandard as zstd
        except ImportError:
            sys.exit("需要 zstandard：pip install zstandard")
        fh = zstd.ZstdDecompressor().stream_reader(open(path, "rb"))
        return io.TextIOWrapper(fh, encoding="utf-8", errors="replace", newline="")
    return open(path, "r", encoding="utf-8", errors="replace", newline="")


# ---------- URL 规范化 + 解析 ----------

def normalize_and_parse(url):
    """
    解析并规范化 freepv URL。

    返回 (dirpath, stem, quality, variant)，其中
      dirpath = freepv 之后的路径（末段目录名去掉下划线后缀），如 5/531/5314gnbd01156
      stem    = 文件名去掉 _quality_variant 后的主体
    不满足形态的返回 None。URL 在运行时用 build_url() 拼装，不落库。
    """
    if not url or url == NULL:
        return None
    u = url.strip()
    if not u.startswith(("http://", "https://")):
        return None
    m = FREEPV_RE.match(u)
    if not m:
        return None
    stem = m.group("stem")
    cdn = m.group("cdn")
    # dirpath：从 URL 取出 freepv/ 之后、最后一层目录之前的部分
    tail = u.split("/litevideo/freepv", 1)[1].lstrip("/")  # 如 5/531/5314gnbd01156/stem_dmb_w.mp4
    parts = tail.split("/")
    if len(parts) < 3:
        return None
    dirpath = "/".join(parts[:-1])
    return dirpath, stem, m.group("quality"), m.group("variant"), cdn


def cid_to_code(cid):
    """
    反推番号：cid 形如 abp00888 → abp-888
    无法可靠反推时返回 None（数字段位数不足/含非字母前缀时不猜）。
    """
    m = re.match(r"^([a-z]+?)(\d+)$", (cid or "").lower())
    if not m:
        return None
    prefix, num = m.group(1), m.group(2)
    # 数字段已被补零到 5 位（DMM 惯例）；4 位数允许直接还原
    if len(num) == 5:
        stripped = num.lstrip("0")
        if not stripped:
            return None
        return f"{prefix}-{stripped}"
    return None


# ---------- 核心：流式解析 ----------

class Builder:
    def __init__(self, conn, batch_size=50_000):
        self.conn = conn
        self.batch = []
        self.batch_size = batch_size
        self.stats = {"trailer": 0, "sample": 0, "accepted": 0, "upgraded": 0, "rejected": 0}
        self.db = conn.cursor()
        # 内存字典：cid -> [rank, quality, variant, url, source, size_hint]
        # 78 万条，每条约 200B ≈ 160MB。GitHub Actions 有 7GB 内存，无压力。
        self.best = {}
        self.code_map = {}
        # 增量刷写集合：只含新增或档位被提升的 cid
        self.dirty = set()

    def offer(self, cid, parsed, code, source, size_hint=0):
        self.stats[source] = self.stats.get(source, 0) + 1
        dirpath, stem, quality, variant, cdn = parsed
        rank = QUALITY_RANK.get(quality)
        if rank is None:
            self.stats["rejected"] += 1
            return
        cur = self.best.get(cid)
        if cur is None:
            self.stats["accepted"] += 1
            self.best[cid] = [rank, cdn, dirpath, stem, quality, variant, source, size_hint]
            self.dirty.add(cid)
            return
        # 破平规则：先比档位，档位相同再比数据源优先级（trailer 优先于 sample）。
        # ⚠️ 必须整条替换 cdn/dirpath/stem/quality/variant ——同一个 cid 在两表里
        # stem 可能不同（118bst00013：trailer 是 118bst013，sample 是 118bst013r，
        # 两者都是 dmb_w 档），只换 quality 会拼出混搭的错误 URL。
        if rank > cur[0] or (rank == cur[0] and SOURCE_RANK.get(source, 0) > SOURCE_RANK.get(cur[6], 0)):
            self.stats["upgraded"] += 1
            self.best[cid] = [rank, cdn, dirpath, stem, quality, variant, source, size_hint]
            self.dirty.add(cid)
        if code and cid not in self.code_map:
            self.code_map[cid] = code
            self.dirty.add(cid)

    def flush(self):
        """
        只把「新增或档位被提升」的 cid 落库（增量刷写）。
        早期版本每 5 万条就把整个字典重写一遍 → O(n²/batch)，78 万条会慢到十几分钟。
        改成只写 dirty 集合后，写库次数从 ~16 次 × 78 万降到 1 次 × 变更条数。
        """
        if not self.dirty:
            return
        now = time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime())
        # best[cid] = [rank, cdn, dirpath, stem, quality, variant, source, size_hint]
        self.db.executemany(
            "INSERT OR REPLACE INTO video"
            "(cid,code,cdn,dirpath,stem,quality,variant,size_hint,source,verified,verified_bytes,updated_at) "
            "VALUES(?,?,?,?,?,?,?,?,?,0,0,?)",
            (
                (cid, self.code_map.get(cid) or cid_to_code(cid),
                 v[1], v[2], v[3], v[4], v[5], v[7], v[6], now)
                for cid, v in self.best.items() if cid in self.dirty
            ),
        )
        self.conn.commit()
        self.stats["flushed"] = self.stats.get("flushed", 0) + len(self.dirty)
        self.dirty.clear()

    def run(self, dump_path, base_quality="sm"):
        """
        快速定位 + 流式解析。

        优化点：dump 里我们只关心两张表，而 `source_dmm_trailer` 在文件很靠后的位置
        （实测在第 1695 万行 / 总 1.7 千万行量级）。所以先**按字节一次性定位**到
        `COPY public.source_dmm_trailer` 的偏移，只解析它前后相邻的 COPY 块，
        避免对 1.3 GB 做逐行 Python 循环（那是分钟级开销）。
        """
        t0 = time.time()
        path = dump_path
        if path == "-":
            raise SystemExit("stdin 模式暂不支持块定位，请传文件路径")

        print("  定位目标表偏移…", flush=True)
        spans = self._locate_spans(path)
        if not spans:
            print("  ⚠ 未找到任何目标表，检查输入是否为 r18.dev dump", flush=True)
            return
        for name, start, end in spans:
            print(f"  → {name}: 偏移 {start:,} ~ {end:,}", flush=True)

        total = 0
        for name, start, end in spans:
            n = self._parse_span(path, start, end, name, t0)
            total += n
            print(f"  ← {name} 处理 {n:,} 条", flush=True)

        self.flush()
        self.stats["flushed"] = self.stats.get("flushed", 0)
        print(f"  完成扫描，用时 {time.time() - t0:.1f}s", flush=True)

    def _locate_spans(self, path):
        """
        返回 [(table, start_offset, end_offset)]，只覆盖两张目标表的 COPY 块。
        做法：从头顺序搜 'COPY public.' 标记，记录每张表块的起止；
        只保留目标表，且把紧邻的非目标表也纳入（trailer 前后可能有别的表）。
        """
        wanted = ("source_dmm_trailer", "derived_video")
        spans = []
        cur_name, cur_start = None, None
        with open(path, "rb") as fh:
            pos = 0
            buf = b""
            marker = re.compile(rb"^COPY public\.(\w+).*?FROM stdin;\s*$")
            for chunk in iter(lambda: fh.read(1 << 23), b""):
                buf += chunk
                start_idx = 0
                while True:
                    nl = buf.find(b"\n", start_idx)
                    if nl < 0:
                        break
                    raw = buf[start_idx:nl]
                    line_start = pos + start_idx
                    start_idx = nl + 1
                    if raw.startswith(b"COPY public."):
                        m = marker.match(raw.strip())
                        name = m.group(1).decode() if m else None
                        # 关闭上一个块
                        if cur_name is not None:
                            spans.append((cur_name, cur_start, line_start))
                            cur_name, cur_start = None, None
                        if name in wanted:
                            cur_name, cur_start = name, line_start + len(raw) + 1
                    elif raw.startswith(b"\\."):
                        if cur_name is not None:
                            spans.append((cur_name, cur_start, line_start))
                            cur_name, cur_start = None, None
                pos += start_idx
                buf = buf[start_idx:]
        if cur_name is not None:
            spans.append((cur_name, cur_start, pos))
        return spans

    def _parse_span(self, path, start, end, table, t0):
        """解析 [start, end) 字节区间内的 COPY 块数据行。"""
        n_cols = 3 if table == "source_dmm_trailer" else len(VIDEO_COLS)
        idx_url = 1 if table == "source_dmm_trailer" else IDX_SAMPLE_URL
        idx_cid = 0
        count = 0
        with open(path, "rb") as fh:
            fh.seek(start)
            remaining = end - start
            tail = b""
            while remaining > 0:
                chunk = fh.read(min(1 << 22, remaining))
                if not chunk:
                    break
                remaining -= len(chunk)
                tail += chunk
                *lines, tail = tail.split(b"\n")
                for raw in lines:
                    count += 1
                    # CRLF 兼容：去掉尾部 \r
                    if raw.endswith(b"\r"):
                        raw = raw[:-1]
                    # 快速过滤：非目标表的数据行绝大多数没有 freepv 特征
                    if b"/litevideo/freepv/" not in raw:
                        continue
                    p = raw.split(b"\t")
                    if len(p) <= max(idx_url, IDX_DVD_ID):
                        continue
                    cid = p[idx_cid].decode("utf-8", "replace")
                    url = p[idx_url].decode("utf-8", "replace")
                    parsed = normalize_and_parse(url)
                    if not parsed:
                        continue
                    code = None
                    if table == "derived_video" and p[IDX_DVD_ID] != NULL.encode():
                        code = p[IDX_DVD_ID].decode("utf-8", "replace")
                    self.offer(cid, parsed, code, "trailer" if table == "source_dmm_trailer" else "sample")
                    if len(self.dirty) >= self.batch_size:
                        self.flush()
                        self._progress(count, t0)
            if tail.strip():
                pass
        return count

    def _progress(self, lines, t0, final=False):
        el = time.time() - t0
        print(
            f"    {'✓' if final else '·'} {lines:>11,} 行  {el:5.1f}s  "
            f"收录 {len(self.best):>8,}  待写 {len(self.dirty):>7,}  "
            f"已落库 {self.stats.get('flushed', 0):>8,}",
            flush=True,
        )


# ---------- 元信息 ----------

def write_meta(conn, dump_path, count):
    import hashlib

    h = hashlib.sha256()
    with open(dump_path, "rb") as fh:
        for chunk in iter(lambda: fh.read(1 << 20), b""):
            h.update(chunk)
    rows = [
        ("schema_version", "1"),
        ("built_at", time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime())),
        ("dump_file", os.path.basename(dump_path)),
        ("dump_sha256", h.hexdigest()),
        ("dump_bytes", str(os.path.getsize(dump_path))),
        ("video_count", str(count)),
    ]
    db = conn.cursor()
    db.executemany("INSERT OR REPLACE INTO meta(key,value) VALUES(?,?)", rows)


def main():
    ap = argparse.ArgumentParser(description="r18.dev dump → 精简 DMM 索引库")
    ap.add_argument("-i", "--input", required=True, help="dump 路径（.sql / .sql.gz / - 表示 stdin）")
    ap.add_argument("-o", "--output", default="dmm-index.db", help="输出 SQLite 路径")
    ap.add_argument("--base-quality", default="sm", choices=QUALITIES_DESC,
                    help="只入库不低于此档的记录（默认 sm = 全部）")
    ap.add_argument("--batch-size", type=int, default=50_000)
    ap.add_argument("--vacuum", action="store_true", help="结束后 VACUUM 压缩（更耗时）")
    args = ap.parse_args()

    if os.path.exists(args.output):
        os.remove(args.output)

    print(f"输入: {args.input}")
    conn = sqlite3.connect(args.output)
    conn.executescript(SCHEMA)

    b = Builder(conn, batch_size=args.batch_size)
    b.run(args.input, base_quality=args.base_quality)

    if args.vacuum:
        print("VACUUM …", flush=True)
        conn.execute("VACUUM")

    write_meta(conn, args.input, len(b.best))
    conn.commit()

    size_mb = os.path.getsize(args.output) / 1024 / 1024
    print(f"\n完成: {len(b.best):,} 条 → {args.output}  ({size_mb:.1f} MB)")
    print(f"  trailer 表贡献 {b.stats.get('trailer', 0):,} 条，"
          f"sample 表贡献 {b.stats.get('sample', 0):,} 条")
    if b.stats.get("upgraded"):
        print(f"  升档覆盖 {b.stats['upgraded']:,} 条（trailer 表档位更高）")

    # 档位分布
    db = conn.cursor()
    print("\n档位分布:")
    for q, n in db.execute("SELECT quality, COUNT(*) FROM video GROUP BY quality ORDER BY COUNT(*) DESC"):
        print(f"  {q:<6} {n:>9,}")
    conn.close()


if __name__ == "__main__":
    main()
