# -*- coding: utf-8 -*-
"""
cidkey.py — cid / 番号 归一化的**唯一事实来源**

为什么单独一个文件
------------------
这份规则以前在三处各写一份（`dmm_index.py::_CID_RE`、`export-d1.py::CID_KEY_RE`、
`worker/index.ts::normalizeCode`），已经跑偏，而且漏收是静默的 —— 详见
`审查报告-预览视频抽取完整性.md` 的 P0-3。

旧正则长这样：

    ^(?P<pre>\\d*)(?P<letters>[a-z_]+?)(?P<num>\\d*)(?P<suf>r|re\\d+|c|d)?$

三个缺陷叠加：字母段**非贪婪**（只吃最少字符）、数字段**可空**、尾缀里还有
`re\\d+` 会把数字段整段抢走。结果 `118tre00024` 被解析成 `letters='t', num=''`
→ 直接丢弃。按 2026-09-29 dump 实测，线上查询键因此少了 **79,644 个（17.6%）**，
`export-d1.py` 侧更是静默丢掉 24.6% 的行。

现行规则（放宽版）
------------------
1. 先剥掉厂牌前缀 `h_<digits>` / `<digits>_`（如 `h_491fone00062` → `fone00062`）；
2. 字母段改**贪婪** `[a-z_]+`，数字段改**非空** `\\d+`；
3. 尾缀 `r` / `re\\d+` / `c` / `d` 保持可选。

放宽后的键是旧键的**严格超集**（实测 0 误配：旧规则能解析的，新规则解析结果完全一致），
所以可以直接替换，不存在「多出来的键」。

⚠️ 仍解析不了的是「字母-数字-字母」形态（`venu12dod`、`n_1234tsds567`、`td12sero34`），
   实测 65,248 行（8.3%）。这类应按厂牌建 `alias` 表，**不要**继续往正则里堆规则 ——
   历史上 4k / pv3001 / pv 新体系 / ①B 命名四次漏收都是靠堆正则补的，每次都补不全。
"""

import re

# 厂牌前缀：h_491 / 000_ （DMM 的动画厂牌码写法）
_PREFIX_RE = re.compile(r"^(?:h_\d+|\d+_)")
# cid 形态：[厂牌数字][字母段][数字段][可选尾缀]
_CID_RE = re.compile(
    r"^(?P<pre>\d*)(?P<letters>[a-z_]+)(?P<num>\d+)(?:r|re\d+|c|d)?$"
)
# 番号形态：ABC-123 / ABC123 / 118abc-123（调用方已先剥掉连字符与空格）
_CODE_RE = re.compile(r"^(?P<pre>\d*)(?P<letters>[a-z]+?)(?P<num>\d+)$")


def cid_key_full(cid):
    """
    cid → (字母段, 数字段去前导零, 原始字母段)

    原始字母段保留给「扩展匹配」用：库里 cid 可能是 `1stars00359`（完整段 'stars'），
    而番号写作 `STAR-359`，两者互为前缀时应互通。
    无法归一化返回 None（如 `000_035` —— 剥掉前缀后只剩数字）。
    """
    s = (cid or "").strip().lower()
    if not s:
        return None
    s = _PREFIX_RE.sub("", s)
    m = _CID_RE.match(s)
    if not m:
        return None
    raw = m.group("letters")
    letters = raw.strip("_")
    num = m.group("num").lstrip("0")
    if not letters or not num:
        return None
    return letters, num, raw


def cid_key(cid):
    """cid → (字母段, 数字段去前导零)；无法归一化返回 None。"""
    r = cid_key_full(cid)
    return None if r is None else (r[0], r[1])


def code_key(code):
    """番号 → (字母段, 数字段去前导零)；解析失败返回 None。"""
    s = (code or "").strip().lower()
    if not s:
        return None
    # 连字符 / 空格 / 下划线一律去掉：ABP-888 / abp 888 / ABP888 等价
    s = re.sub(r"[-_\s]+", "", s)
    m = _CODE_RE.match(s)
    if not m:
        return None
    letters, num = m.group("letters"), m.group("num")
    if not letters or not num:
        return None
    return letters, num.lstrip("0") or "0"


if __name__ == "__main__":
    # 这份用例同时是规则的文档：前五个就是旧正则过不了的那批
    cases = {
        "118tre00024": ("tre", "24"),
        "118abp00888": ("abp", "888"),
        "h_491fone00062": ("fone", "62"),
        "h_1034yink00002": ("yink", "2"),
        "1stars00359": ("stars", "359"),
        "ssis00095": ("ssis", "95"),
        "4ssis095r": ("ssis", "95"),
        "000_035": None,
        "venu12dod": None,
    }
    bad = 0
    for cid, want in cases.items():
        got = cid_key(cid)
        ok = got == want
        bad += 0 if ok else 1
        print(f"{'ok ' if ok else 'FAIL'} {cid:<18} -> {got}  (want {want})")
    raise SystemExit(1 if bad else 0)
