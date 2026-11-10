# -*- coding: utf-8 -*-
"""对拍：dmm-query（SQL 索引） vs dmm_index.py（内存倒排）

同一个番号两边必须给出同一个 cid，否则说明 SQL 侧的择优顺序或归一化规则跑偏了。
"""
import json
import random
import sqlite3
import sys
import urllib.request
from urllib.parse import quote

sys.path.insert(0, "F:/codx/DmmRequest")
from dmm_index import Index

BASE = "http://127.0.0.1:8099"
OPENER = urllib.request.build_opener(urllib.request.ProxyHandler({}))

N = int(sys.argv[1]) if len(sys.argv) > 1 else 300

db = sqlite3.connect("file:F:/codx/DmmRequest/dmm-index-new.db?mode=ro", uri=True)
codes = [r[0] for r in db.execute(
    "SELECT code FROM video WHERE code IS NOT NULL AND code <> '' "
    "ORDER BY random() LIMIT ?", (N,))]

idx = Index("F:/codx/DmmRequest/dmm-index-new.db")
ref = idx.lookup_many(codes)

same = diff = 0
miss_ref = miss_new = 0
examples = []
for c in codes:
    try:
        with OPENER.open(f"{BASE}/?code={quote(c)}", timeout=10) as r:
            got = json.loads(r.read())
    except urllib.error.HTTPError as e:
        got = json.loads(e.read())
    a = ref.get(c)
    b = None if "error" in got else got

    if a is None and b is None:
        same += 1
        continue
    if a is None:
        miss_ref += 1
        examples.append((c, "ref=未命中", f"new={b.get('cid')}"))
        continue
    if b is None:
        miss_new += 1
        examples.append((c, f"ref={a['cid']}", "new=未命中"))
        continue
    if a["cid"] == b["cid"] and a["url"] == b["dmmUrl"]:
        same += 1
    else:
        diff += 1
        examples.append((c, f"ref={a['cid']} {a['url']}", f"new={b['cid']} {b['dmmUrl']}"))

print(f"样本 {len(codes)}")
print(f"一致     {same}  ({100*same/len(codes):.1f}%)")
print(f"cid 不同 {diff}")
print(f"仅内存侧命中 {miss_ref}")
print(f"仅 SQL 侧命中 {miss_new}")
for e in examples[:10]:
    print("  ", e)
