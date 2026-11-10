# -*- coding: utf-8 -*-
"""长连接压测：每线程持有一个 keep-alive 连接，排除 TCP 建连开销"""
import http.client
import random
import sqlite3
import statistics
import sys
import threading
import time
from urllib.parse import quote

HOST = sys.argv[1] if len(sys.argv) > 1 else "127.0.0.1"
PORT = int(sys.argv[2]) if len(sys.argv) > 2 else 8099
CONC = int(sys.argv[3]) if len(sys.argv) > 3 else 20
PER = int(sys.argv[4]) if len(sys.argv) > 4 else 200

db = sqlite3.connect("file:F:/tmp/idx-kkey.db?mode=ro", uri=True)
codes = [r[0] for r in db.execute(
    "SELECT code FROM video WHERE code IS NOT NULL AND code <> '' "
    "ORDER BY random() LIMIT 3000")]

lock = threading.Lock()
lat_all = []
bad = [0]


def worker(k):
    conn = http.client.HTTPConnection(HOST, PORT, timeout=10)
    local = []
    rnd = random.Random(k)
    for _ in range(PER):
        c = rnd.choice(codes)
        t0 = time.perf_counter()
        try:
            conn.request("GET", "/?code=" + quote(c))
            r = conn.getresponse()
            r.read()
            if r.status != 200:
                with lock:
                    bad[0] += 1
        except Exception:
            with lock:
                bad[0] += 1
            conn.close()
            conn = http.client.HTTPConnection(HOST, PORT, timeout=10)
        local.append((time.perf_counter() - t0) * 1000)
    conn.close()
    with lock:
        lat_all.extend(local)


def main():
    warm = http.client.HTTPConnection(HOST, PORT, timeout=10)
    for c in codes[:50]:
        warm.request("GET", "/?code=" + quote(c))
        warm.getresponse().read()
    warm.close()

    ths = [threading.Thread(target=worker, args=(i,)) for i in range(CONC)]
    t0 = time.perf_counter()
    for t in ths:
        t.start()
    for t in ths:
        t.join()
    dt = time.perf_counter() - t0

    lat = sorted(lat_all)
    n = len(lat)
    pct = lambda p: lat[min(n - 1, int(n * p))]
    total = CONC * PER
    print(f"并发 {CONC}  请求 {total}  用时 {dt:.2f}s")
    print(f"QPS       {total/dt:,.0f}")
    print(f"失败      {bad[0]}")
    print(f"p50 {pct(.50):.2f} ms   p90 {pct(.90):.2f} ms   "
          f"p99 {pct(.99):.2f} ms   max {lat[-1]:.2f} ms   均值 {statistics.mean(lat):.2f} ms")


if __name__ == "__main__":
    main()
