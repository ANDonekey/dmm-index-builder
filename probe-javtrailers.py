# -*- coding: utf-8 -*-
"""临时探测：javtrailers 镜像对本地索引的覆盖率（mp4 + HLS 兜底）"""
import ssl, sys, urllib.request, urllib.error
from concurrent.futures import ThreadPoolExecutor

UA = ("Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 "
      "(KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36")
CTX = ssl.create_default_context()
CTX.check_hostname = False
CTX.verify_mode = ssl.CERT_NONE
OPENER = urllib.request.build_opener(
    urllib.request.ProxyHandler({}), urllib.request.HTTPSHandler(context=CTX))


def head(url):
    req = urllib.request.Request(url, headers={
        "User-Agent": UA,
        "Range": "bytes=0-0",
        "Referer": "https://javtrailers.com/",
    })
    try:
        with OPENER.open(req, timeout=25) as r:
            return r.status, r.headers.get("Content-Range", ""), r.headers.get("Content-Length", "")
    except urllib.error.HTTPError as e:
        return e.code, "", ""
    except Exception as e:
        return 0, type(e).__name__, ""


def check(row):
    cid, dp, stem, q, v = row
    mp4 = f"https://media.javtrailers.com/litevideo/freepv/{dp}/{stem}_{q}_{v}.mp4"
    st, cr, cl = head(mp4)
    if st in (200, 206):
        return ("MP4", cid, q, st, cr or cl)
    for cand in (dp, dp.rsplit("/", 1)[0] + "/" + cid, dp.rsplit("/", 1)[0] + "/" + stem):
        hls = f"https://media.javtrailers.com/hlsvideo/freepv/{cand}/playlist.m3u8"
        st2, cr2, cl2 = head(hls)
        if st2 in (200, 206):
            return ("HLS", cid, q, st2, cand)
    return ("NONE", cid, q, st, cr)


def main(path):
    rows = [l.rstrip("\n").split("\t") for l in open(path, encoding="utf-8") if l.strip()]
    with ThreadPoolExecutor(max_workers=6) as ex:
        res = list(ex.map(check, rows))
    stat = {}
    for kind, cid, q, st, extra in res:
        stat[kind] = stat.get(kind, 0) + 1
    for kind, cid, q, st, extra in res:
        print(f"{kind:<5}{st:<5}{q:<5}{cid:<20}{extra}")
    n = len(res)
    print("\n---- 汇总 ----")
    for k in ("MP4", "HLS", "NONE"):
        if k in stat:
            print(f"{k:<5}{stat[k]:>4}  {100*stat[k]/n:5.1f}%")
    print(f"合计 {n} 条，可播放 {stat.get('MP4',0)+stat.get('HLS',0)} 条 "
          f"({100*(stat.get('MP4',0)+stat.get('HLS',0))/n:.1f}%)")


if __name__ == "__main__":
    main(sys.argv[1])
