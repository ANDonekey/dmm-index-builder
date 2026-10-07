#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""
给 fastcdn.dpdns.org 上「只监听 80」的站点补 443 + 通配 Origin CA 证书。

设计要点：
  - 幂等：已含 wildcard-origin.crt 的文件直接跳过，重复跑不会叠加。
  - 原地改，先写 .bak（仅第一次）。
  - 只动 80 的 server block；dmmpv.conf（已有 443 + AOP）不在列表里，不碰。
  - 每个 server block 只插一次 listen 443 ssl（resolver.conf 有 IPv6 两行 listen 80）。

用法：python3 /root/apply-origin-tls.py [--dry]
"""
import re
import shutil
import sys

CERT = "/etc/nginx/certs/wildcard-origin.crt"
KEY = "/etc/nginx/certs/wildcard-origin.key"

TARGETS = [
    "/etc/nginx/conf.d/fastcdn.conf",
    "/etc/nginx/conf.d/vless.conf",
    "/etc/nginx/conf.d/resolver.conf",
    "/etc/nginx/conf.d/tgstate-proxy.conf",
    "/etc/nginx/conf.d/ehentai-proxy-njs.conf",
    "/etc/nginx/sites-enabled/emby.conf",
    "/etc/nginx/sites-enabled/komari.conf",
]

DRY = "--dry" in sys.argv

LISTEN80 = re.compile(r"^(\s*)listen\s+(?:\[::\]:)?80(?:\s+default_server)?;")


def patch(path):
    src = open(path, encoding="utf-8").read()
    if CERT in src:
        print("skip (already patched): %s" % path)
        return False

    out = []
    pending = False   # 当前 server block 还没补过 443
    changed = False
    has_http2 = "http2 on" in src

    for line in src.splitlines():
        out.append(line)
        stripped = line.strip()

        if stripped.startswith("server") and stripped.endswith("{"):
            pending = True
            continue

        if pending and LISTEN80.match(line):
            indent = LISTEN80.match(line).group(1)
            out.append("%slisten 443 ssl;" % indent)
            if not has_http2:
                out.append("%shttp2 on;" % indent)
            out.append("%sssl_certificate     %s;" % (indent, CERT))
            out.append("%sssl_certificate_key %s;" % (indent, KEY))
            pending = False
            changed = True

    if not changed:
        print("WARN no listen 80 found: %s" % path)
        return False

    new = "\n".join(out) + "\n"
    if not DRY:
        shutil.copy2(path, path + ".bak")
        open(path, "w", encoding="utf-8").write(new)
    print("patched: %s" % path)
    return True


def main():
    n = 0
    for p in TARGETS:
        try:
            if patch(p):
                n += 1
        except FileNotFoundError:
            print("WARN missing: %s" % p)
    print("\npatched %d file(s)%s" % (n, " (dry-run)" if DRY else ""))
    print("next: nginx -t && systemctl reload nginx")


if __name__ == "__main__":
    main()
