# -*- coding: utf-8 -*-
"""本地压测 dmm-query：随机番号 + 并发，统计 QPS 与延迟分位"""
import json
import random
import sqlite3
import ssl
import statistics
import sys
import time
import urllib.request
from concurrent.futures import ThreadPoolExecutor

BASE = sys.argv[1] if len(sys.argv) > 1 else "http://127.0.0.1:8099"
CONC = int(sys.argv[2]) if len(sys.argv) > 2 else 20
REQS = int(sys.argv[3]) if len(sys.argv) > 3 else 3000

OPENER = urllib.request.build_opener(urllib.request.ProxyHandler({}))

# 从库里取一批真实可查询的番号，避免全打 ABC-123 这种未命中
db = sqlite3.connect("file:F:/tmp/idx-kkey.db?mode=ro", uri=True)
codes = [r[0] for r in db.execute(
    "SELECT code FROM video WHERE code IS NOT NULL AND code <> '' "
    "ORDER BY random() LIMIT 3000")]
if not codes:
    sys.exit("没有可用番号")


def one(_):
    c = random.choice(codes)
    url = f"{BASE}/?code={c}"
    t0 = time.perf_counter()
    try:
        with OPENER.open(url, timeout=10) as r:
            r.read()
            st = r.status
    except Exception:
        st = 0
    return (time.perf_counter() - t0) * 1000, st


def main():
    # 预热：让 page cache 与连接池就位
    for c in codes[:50]:
        try:
            OPENER.open(f"{BASE}/?code={c}", timeout=10).read()
        except Exception:
            pass

    t0 = time.perf_counter()
    with ThreadPoolExecutor(max_workers=CONC) as ex:
        res = list(ex.map(one, range(REQS)))
    dt = time.perf_counter() - t0

    lat = sorted(r[0] for r in res)
    ok = sum(1 for r in res if r[1] == 200)
    n = len(lat)

    def pct(p):
        return lat[min(n - 1, int(n * p))]

    print(f"并发 {CONC}  请求 {REQS}  用时 {dt:.2f}s")
    print(f"QPS       {REQS/dt:,.0f}")
    print(f"成功率    {100*ok/n:.1f}%")
    print(f"延迟 p50  {pct(.50):.2f} ms")
    print(f"     p90  {pct(.90):.2f} ms")
    print(f"     p99  {pct(.99):.2f} ms")
    print(f"     max  {lat[-1]:.2f} ms")
    print(f"     均值 {statistics.mean(lat):.2f} ms")


if __name__ == "__main__":
    main()
