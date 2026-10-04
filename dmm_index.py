"""
DMM 索引库查询辅助。

⚠️ 为什么不能直接 `WHERE code = 'ABP-888'`：
   索引库里的 `code` 只有 111,484 / 620,025（18%）非空，因为 DMM 的
   content_id 与「番号」不是一一对应：

     - 厂牌带数字前缀：ABP-888  → cid `118abp00888`（118=动画厂牌码）
     - 纯字母+5位数字： SSIS-095 → cid `ssis00095`
     - 不规则：h_491fone00062、000_035、1STARS00359、4ssis095 …

   所以正确做法是**归一化后比对「字母段 + 数字段」**，而不是字符串相等。

匹配规则（优先级从高到低）：

  1. 数字段去前导零后相等，且字母段匹配（允许厂牌数字前缀、允许尾缀 r/re01）
  2. 同数字但字母段是番号字母的扩展（STARS → STARS、STAR）

性能：单次 lookup < 10 ms（先按候选 cid 精确查，未命中才全表兜底）。
批量查询请用 `lookup_many()`——一次全表扫描服务所有番号。
"""

import re
import sqlite3

# cid 形态： [厂牌数字][字母段][数字段][可选后缀]
# 例：118abp00888 / ssis00095 / 1STARS00359 / 4ssis095r / 000_035 / h_491fone00062
_CID_RE = re.compile(r"^(?P<pre>\d*)(?P<letters>[a-z_]+?)(?P<num>\d*)(?P<suf>r|re\d+|c|d)?$")
# 番号形态： ABC-123 / ABC123 / 118abc-123
# 先剥掉连字符再匹配，避免「字母段非贪婪 + 可选连字符」把连字符吃进字母段
_CODE_RE = re.compile(r"^(?P<pre>\d*)(?P<letters>[a-z]+?)(?P<num>\d+)$")


def normalize_code(code: str):
    """番号 → (字母段小写, 数字段去零)。解析失败返回 None。"""
    s = (code or "").strip().lower()
    if not s:
        return None
    # 去掉所有连字符与空格（ABP-888 / abp 888 / ABP888 都等价）
    s = re.sub(r"[-_\s]+", "", s)
    m = _CODE_RE.match(s)
    if not m:
        return None
    letters, num = m.group("letters"), m.group("num")
    if not letters or not num:
        return None
    return letters, num.lstrip("0") or "0"


def _cid_key(cid: str):
    """
    cid → (字母段, 数字段去零, 完整字母段)

    完整字母段用于「扩展匹配」：如 1STAR / 1STARS 这类，
    库里 cid 是 1STARS00359（完整段 'stars'），而番号写作 STAR-359。
    """
    m = _CID_RE.match((cid or "").lower())
    if not m:
        return None
    letters = m.group("letters").strip("_")
    num = m.group("num")
    if not letters:
        return None
    return letters, num.lstrip("0"), m.group("letters")


class Index:
    def __init__(self, db_path: str, cache_size: int = 20000):
        self.conn = sqlite3.connect(f"file:{db_path}?mode=ro", uri=True)
        self.conn.row_factory = sqlite3.Row
        # cid → row 的内存缓存（番号重复查询很常见，命中率高）
        self._cache = {}
        self._cache_max = cache_size
        # 归一化键 → [cid…] 的倒排索引（懒建，一次全表扫描建好）
        self._by_key = None
        # 字母段前缀互通的结果缓存
        self._ext_cache = {}

    # ---------- 索引构建 ----------
    def _ensure_index(self):
        """
        建「(字母段, 数字段) → [cid…]」倒排索引。

        同一个番号可能对应多个 cid（厂牌前缀不同 / 同片多条），
        排序规则：code 字段非空者优先 → trailer 源优先 → 数字段规范者优先，
        保证每次查询返回确定的那一条。
        """
        if self._by_key is not None:
            return
        buckets = {}
        rows = self.conn.execute(
            "SELECT cid, code, source, cdn, dirpath FROM video"
        )
        for cid, code, source, cdn, dirpath in rows:
            k = _cid_key(cid)
            if not k:
                continue
            # 排序权重：code 非空(0) > trailer(0) > cc3001(0) > 字母段更短(0)
            weight = (
                0 if code else 1,
                0 if source == "trailer" else 1,
                0 if cdn == "cc3001" else 1,
                len(k[0]),
            )
            buckets.setdefault((k[0], k[1]), []).append((weight, cid))
        for v in buckets.values():
            v.sort()
        self._by_key = {k: [cid for _, cid in v] for k, v in buckets.items()}

    # ---------- 查询 ----------
    def lookup(self, code: str):
        """按番号查预览视频。查不到返回 None。"""
        key = normalize_code(code)
        if not key:
            return None
        self._ensure_index()
        cids = self._by_key.get(key)
        if not cids:
            # 字母段互为前缀时互通（STAR ↔ STARS）
            cids = self._by_key.get(self._extended_key(key))
        if not cids:
            return None
        return self._row_by_cid(cids[0])

    def lookup_many(self, codes):
        """
        批量查询，返回 {原番号: row|None}。
        一次索引扫描 + 内存查找，比逐个 lookup 快几个数量级。
        """
        self._ensure_index()
        out = {}
        for code in codes:
            key = normalize_code(code)
            if not key:
                out[code] = None
                continue
            cids = self._by_key.get(key)
            if not cids:
                ext = self._extended_key(key)
                cids = self._by_key.get(ext) if ext else None
            out[code] = self._row_by_cid(cids[0]) if cids else None
        return out

    def _extended_key(self, key):
        """
        字母段互为前缀时返回对方的键（覆盖 STAR ↔ STARS 这类写法差异）。
        结果缓存，避免每次都全表扫。
        """
        if key in self._ext_cache:
            return self._ext_cache[key]
        letters, num = key
        found = None
        for (l2, n2) in self._by_key:
            if n2 == num and (l2.startswith(letters) or letters.startswith(l2)):
                found = (l2, n2)
                break
        self._ext_cache[key] = found
        return found

    def _row_by_cid(self, cid: str):
        if cid in self._cache:
            return self._cache[cid]
        r = self.conn.execute(
            "SELECT cid, code, cdn, dirpath, stem, quality, variant, source, "
            "       verified, verified_bytes FROM video WHERE cid = ?",
            (cid,),
        ).fetchone()
        out = self._to_dict(r) if r else None
        if len(self._cache) < self._cache_max:
            self._cache[cid] = out
        return out

    @staticmethod
    def _to_dict(r):
        return {
            "cid": r["cid"],
            "code": r["code"],
            "quality": f"{r['quality']}_{r['variant']}",
            "source": r["source"],
            "url": Index.build_url(r["cdn"], r["dirpath"], r["stem"], r["quality"], r["variant"]),
            "verified": bool(r["verified"]),
            "verified_bytes": r["verified_bytes"],
        }

    @staticmethod
    def build_url(cdn, dirpath, stem, quality, variant):
        """由分列字段还原 DMM 直链（URL 不落库，见 build-index.py 的体积优化说明）"""
        return f"https://{cdn}.dmm.co.jp/litevideo/freepv/{dirpath}/{stem}_{quality}_{variant}.mp4"

    def stats(self):
        return {
            "total": self.conn.execute("SELECT COUNT(*) FROM video").fetchone()[0],
            "by_quality": dict(self.conn.execute(
                "SELECT quality, COUNT(*) FROM video GROUP BY quality")),
            "by_cdn": dict(self.conn.execute(
                "SELECT cdn, COUNT(*) FROM video GROUP BY cdn")),
            "meta": dict(self.conn.execute("SELECT key, value FROM meta")),
        }

    def close(self):
        self.conn.close()


if __name__ == "__main__":
    import sys
    import time

    db = sys.argv[1] if len(sys.argv) > 1 else "dmm-index.db"
    idx = Index(db)

    s = idx.stats()
    print(f"库: {db}")
    print(f"总数: {s['total']:,}")
    print(f"档位: {s['by_quality']}")
    print(f"CDN : {s['by_cdn']}\n")

    t0 = time.time()
    idx.lookup("ABP-888")          # 触发倒排索引构建
    print(f"（倒排索引构建耗时 {time.time() - t0:.2f}s）\n")

    tests = sys.argv[2:] or ["ABP-888", "SSIS-095", "MIDE-800", "IPX-777",
                             "ABP-962", "SSIS-001", "1STAR-359", "ABC-123"]
    t0 = time.time()
    for r in idx.lookup_many(tests).items():
        code, v = r
        if v:
            print(f"{code:<12} {v['quality']:<7} {v['source']:<8} cid={v['cid']:<18} {v['url']}")
        else:
            print(f"{code:<12} 未命中")
    print(f"\n查询 {len(tests)} 个番号耗时 {time.time() - t0:.3f}s")
