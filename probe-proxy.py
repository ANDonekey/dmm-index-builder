"""probe-proxy.py — 抽样探测 VPS 反代在各「CDN × 路径体系」组合下的可用率

背景：同步 D1 后抽查发现 pv3001 + freepv 的链接返回 403
（x-cache: passthrough:upstream-error），需要确认是个例还是某个组合整体挂掉。

用法: python probe-proxy.py [每组样本数]
"""
import concurrent.futures as cf
import sqlite3
import sys
import urllib.request
import collections

PROXY_HOST = "fastcdn.dpdns.org"
N = int(sys.argv[1]) if len(sys.argv) > 1 else 12


def build(cdn, dirpath, stem, quality, variant, fmt):
    """fmt: 0=freepv+A形 1=freepv+B形 2=pv+A形 3=pv+B形"""
    base = "litevideo/freepv" if fmt < 2 else "pv"
    name = f"{stem}_{quality}_{variant}.mp4" if fmt % 2 == 0 else f"{stem}{quality}{variant}.mp4"
    p = f"/{base}/{dirpath}/{name}"
    return f"https://{cdn}.{PROXY_HOST}{p}"


def main():
    db = sqlite3.connect("dmm-index-new.db")
    # 每个 (cdn, fmt) 组合取 N 条
    samples = []
    cdns = [r[0] for r in db.execute(
        "SELECT cdn FROM video GROUP BY cdn ORDER BY COUNT(*) DESC")]
    for cdn in cdns:
        for fmt in (0, 1, 2, 3):
            rows = list(db.execute(
                "SELECT cdn,dirpath,stem,quality,variant,fmt FROM video "
                "WHERE cdn=? AND fmt=? LIMIT ?", (cdn, fmt, N)))
            for r in rows:
                samples.append((cdn, fmt, build(*r)))

    # 本机有本地 HTTP 代理，会让 urllib 直接 URLError —— 必须显式绕过
    # 另：本机 certifi 对该站点报 CERTIFICATE_VERIFY_FAILED（curl 走系统证书库是通的），
    # 探测阶段只关心可用性，故不校验证书。
    import ssl
    ctx = ssl.create_default_context()
    ctx.check_hostname = False
    ctx.verify_mode = ssl.CERT_NONE
    opener = urllib.request.build_opener(
        urllib.request.ProxyHandler({}), urllib.request.HTTPSHandler(context=ctx))
    print(f"共 {len(samples)} 条样本，并发 HEAD（Range 0-1023）\n")

    def probe(item):
        cdn, fmt, url = item
        req = urllib.request.Request(url, method="HEAD",
                                     headers={"Range": "bytes=0-1023",
                                              "User-Agent": "probe/1.0"})
        try:
            with opener.open(req, timeout=30) as r:
                return (cdn, fmt, r.status, r.headers.get("x-cache", "-"))
        except urllib.error.HTTPError as e:
            return (cdn, fmt, e.code, e.headers.get("x-cache", "-"))
        except Exception as e:
            return (cdn, fmt, "ERR", type(e).__name__)

    with cf.ThreadPoolExecutor(max_workers=8) as ex:
        res = list(ex.map(probe, samples))

    agg = collections.defaultdict(collections.Counter)
    for cdn, fmt, st, xc in res:
        agg[(cdn, fmt)][st] += 1

    print(f"{'CDN':<10}{'fmt':<5}{'OK':<6}{'403':<6}{'其他':<20}")
    for (cdn, fmt), c in sorted(agg.items()):
        ok = c.get(200, 0) + c.get(206, 0)
        other = {k: v for k, v in c.items() if k not in (200, 206, 403)}
        print(f"{cdn:<10}{fmt:<5}{ok:<6}{c.get(403,0):<6}{str(other):<20}")

    bad = [r for r in res if r[2] in (403, "ERR") or (isinstance(r[2], int) and r[2] >= 500)]
    if bad:
        print(f"\n失败样本 {len(bad)} 条，前 5 条：")
        for cdn, fmt, st, xc in bad[:5]:
            print(f"  {cdn} fmt={fmt} → {st} (x-cache={xc})")


if __name__ == "__main__":
    main()
