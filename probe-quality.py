"""
DMM freepv 档位探测工具（2026-10-05 实测验证）

结论备忘：
  1. freepv 直链无签名，清晰度只是文件名后缀之差 → 可逐级向上试档
  2. ⚠️ 不存在的档位返回 404 + Content-Length: 150（openresty 错误页 HTML）
     判据要用字节数（bytes > 1024），并注意代理隧道行会污染状态码解析
  3. ⚠️ 走 HTTP 代理时 curl 会先打印 `HTTP/1.1 200 Connection Established` 隧道行，
     真正的上游状态码在最后一个 HTTP/ 行 —— 取第一行会把所有 404 误判成 200
  4. freepv 天花板是 hhb（≈1080p，实测最大 102MB）；4k/4ks 不存在，需走 /pv/ 签名链
  5. _w / _s 是两种不同资源，不能横向替换
  6. 个别片子会单独下线（实测 9 条里 1 条变 404），不能因个别失败否定整套

用法:
  python probe-quality.py <dmm直链> [--max-mb 200]
  python probe-quality.py "https://cc3001.dmm.co.jp/litevideo/freepv/j/jufe00225/jufe00225_dmb_w.mp4"

注意: 必须走日本出口，否则 CloudFront 一律 403 Request blocked。
"""

import argparse
import re
import subprocess
import sys

UA = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/124.0 Safari/537.36"

# 从高到低；freepv 实际只到 hhb
QUALITY_ORDER = ["hhb", "mhb", "dmb", "dm", "sm"]

# 错误页（openresty 404 HTML）固定大小；真实视频远大于此
ERROR_PAGE_BYTES = 1024


def head(url: str, timeout: int = 25):
    """
    返回 (状态码, Content-Length)。

    ⚠️ 走 HTTP 代理时 curl 会先打印隧道行 `HTTP/1.1 200 Connection Established`，
    真正的上游状态码在**最后**一个 `HTTP/` 行——必须取最后一条，不能取第一条。
    """
    try:
        r = subprocess.run(
            ["curl", "-sI", "--max-time", str(timeout), "-H", f"User-Agent: {UA}", url],
            capture_output=True,
            text=True,
            timeout=timeout + 15,
        )
        lines = [x for x in r.stdout.splitlines() if x.strip()]
        # 代理隧道会插入 `HTTP/1.1 200 Connection Established`，取最后一个状态行
        status_line = next(
            (x for x in reversed(lines) if x.upper().startswith("HTTP/")),
            "",
        )
        code = status_line.split()[1] if len(status_line.split()) > 1 else "?"
        length = next(
            (x.split(":", 1)[1].strip() for x in lines if x.lower().startswith("content-length")),
            "?",
        )
        return code, length
    except Exception as exc:  # noqa: BLE001
        return "ERR", str(exc)[:24]


def parse_url(url: str):
    """拆出 (目录, 主体stem, 当前清晰度, 变体)"""
    url = url.replace("http://", "https://")
    m = re.match(r"^(https://[^/]+/.+)/([^/]+)\.mp4$", url)
    if not m:
        sys.exit(f"无法解析 URL: {url}")
    directory, filename = m.group(1), m.group(2)
    parts = filename.split("_")
    if len(parts) < 3:
        sys.exit(f"文件名不符合 <stem>_<quality>_<variant>.mp4 规则: {filename}")
    return directory, "_".join(parts[:-2]), parts[-2], parts[-1]


def normalize_host(url: str) -> str:
    return url.replace("http://", "https://").replace("cc3001.dmm.com", "cc3001.dmm.co.jp")


def probe(url: str, max_mb: int = 200):
    url = normalize_host(url)
    directory, stem, quality, variant = parse_url(url)
    limit = max_mb * 1024 * 1024

    # 库里那档必须先确认有效——它决定探测起点
    print(f"库记录档位: {quality}  变体: {variant}")
    print(f"目录: {directory}")
    print(f"{'档位':<8}{'状态':<7}{'字节':>13}  判定")
    print("-" * 52)

    found = []
    for q in reversed(QUALITY_ORDER):  # 从低到高打印，最终取最高
        candidate = f"{directory}/{stem}_{q}_{variant}.mp4"
        code, length = head(candidate)
        try:
            size = int(length)
        except (TypeError, ValueError):
            size = -1

        if code == "?" or size < 0:
            verdict = "无法探测"
        elif code == "403":
            verdict = "地域封锁（需日本出口）"
        elif code in ("404", "410"):
            verdict = "不存在"
        elif code != "200":
            verdict = f"异常状态 {code}"
        elif size <= ERROR_PAGE_BYTES:
            verdict = "错误页（字节数过小，非视频）"
        elif size > limit:
            verdict = "存在但超 max-mb，已跳过"
        else:
            verdict = "✅ 可用"
            found.append((q, candidate, size))

        shown = f"{size:,}" if size > 0 else str(length)
        print(f"{q:<8}{code:<7}{shown:>13}  {verdict}")

    if not found:
        print("\n没有任何可用档位。检查：① 是否走日本出口 ② URL 是否正确")
        return

    best_q, best_url, best_size = found[-1]
    print(f"\n最高可用档: {best_q}  ({best_size:,} 字节 / {best_size / 1024 / 1024:.1f} MB)")
    print(f"代理链接（把域名换成 <sub>.dmm.example.com 即可）:")
    print(f"  {best_url.replace('cc3001.dmm.co.jp', 'cc3001.dmm.example.com')}")

    if quality != best_q:
        print(f"\n提示: 库里存的是 {quality}，实际可用到 {best_q} —— 值得换档后回写数据库")


def main():
    ap = argparse.ArgumentParser(description="DMM freepv 档位探测")
    ap.add_argument("url", help="库里记录的 freepv 直链（不必是可用的那一档）")
    ap.add_argument("--max-mb", type=int, default=200, help="单档位大小上限，超过则视为存在但不列出")
    args = ap.parse_args()
    probe(args.url, args.max_mb)


if __name__ == "__main__":
    main()
