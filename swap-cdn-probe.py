"""swap-cdn-probe.py — 同一条路径分别走 pv3001 / cc3001 反代，判断是「主机不可用」还是「链接失效」

探测发现 pv3001 的链接全部 403，但对照组选错了（样本在 cc3001 上也 403）。
这里做严格对照：路径不变，只换 Host 的 cdn 段。
  - cc3001 也 403  → 该链接本身已失效（内容下架）
  - cc3001 200     → pv3001 主机整体不可用（白名单/源站层面）
"""
import concurrent.futures as cf
import sqlite3
import ssl
import urllib.request

PROXY_HOST = "fastcdn.dpdns.org"


def path_of(dirpath, stem, quality, variant, fmt):
    base = "litevideo/freepv" if fmt < 2 else "pv"
    name = f"{stem}_{quality}_{variant}.mp4" if fmt % 2 == 0 else f"{stem}{quality}{variant}.mp4"
    return f"/{base}/{dirpath}/{name}"


def build_opener():
    ctx = ssl.create_default_context()
    ctx.check_hostname = False
    ctx.verify_mode = ssl.CERT_NONE
    return urllib.request.build_opener(
        urllib.request.ProxyHandler({}), urllib.request.HTTPSHandler(context=ctx))


def main():
    db = sqlite3.connect("dmm-index-new.db")
    rows = list(db.execute(
        "SELECT dirpath,stem,quality,variant,fmt FROM video WHERE cdn='pv3001' LIMIT 20"))
    print(f"pv3001 样本 {len(rows)} 条，逐个测两个主机\n")

    op = build_opener()

    def probe(url):
        req = urllib.request.Request(url, method="HEAD",
                                     headers={"Range": "bytes=0-1023", "User-Agent": "probe/1.0"})
        try:
            with op.open(req, timeout=30) as r:
                return r.status
        except urllib.error.HTTPError as e:
            return e.code
        except Exception as e:
            return type(e).__name__

    jobs = []
    for dirpath, stem, q, v, fmt in rows:
        p = path_of(dirpath, stem, q, v, fmt)
        jobs.append((p, "pv3001", f"https://pv3001.{PROXY_HOST}{p}"))
        jobs.append((p, "cc3001", f"https://cc3001.{PROXY_HOST}{p}"))

    with cf.ThreadPoolExecutor(max_workers=8) as ex:
        res = list(ex.map(lambda j: (j[0], j[1], probe(j[2])), jobs))

    by_path = {}
    for p, cdn, st in res:
        by_path.setdefault(p, {})[cdn] = st

    npv = ncc = both = 0
    for p, d in by_path.items():
        pv_ok = d.get("pv3001") in (200, 206)
        cc_ok = d.get("cc3001") in (200, 206)
        npv += pv_ok
        ncc += cc_ok
        both += (not pv_ok and not cc_ok)
        flag = ""
        if cc_ok and not pv_ok:
            flag = "  ← pv3001 独有失败"
        elif not cc_ok and not pv_ok:
            flag = "  ← 链接本身失效"
        print(f"pv={d.get('pv3001'):<5} cc={d.get('cc3001'):<5} {p[-52:]}{flag}")

    n = len(by_path)
    print(f"\n合计 {n} 条路径：")
    print(f"  pv3001 可用 {npv}/{n}")
    print(f"  cc3001 可用 {ncc}/{n}")
    print(f"  两边都失败 {both}/{n}")


if __name__ == "__main__":
    main()
