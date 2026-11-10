#!/bin/bash
set -euo pipefail

TS=$(date +%Y%m%d%H%M%S)

# ── 1. dmm-proxy 换端口 + 关掉落盘 ──
cp -a /etc/dmm-proxy.env "/etc/dmm-proxy.env.bak-$TS"
sed -i 's|^LISTEN=.*|LISTEN=127.0.0.1:8092|' /etc/dmm-proxy.env

# CACHE_QUALITY_ALLOW 空 = 全缓存；设成不存在的档位 → allowCache 恒 false
# → 走 streamOnly 纯透传，不写磁盘（VPS 不给缓存空间）
if grep -q '^CACHE_QUALITY_ALLOW=' /etc/dmm-proxy.env; then
  sed -i 's|^CACHE_QUALITY_ALLOW=.*|CACHE_QUALITY_ALLOW=none|' /etc/dmm-proxy.env
else
  printf 'CACHE_QUALITY_ALLOW=none\n' >> /etc/dmm-proxy.env
fi

echo "== env 生效值 =="
grep -E '^(LISTEN|CACHE_QUALITY_ALLOW|CACHE_DIR)=' /etc/dmm-proxy.env

# ── 2. nginx upstream 8080 → 8092 ──
CONF=/etc/nginx/conf.d/dmmpv.conf
cp -a "$CONF" "$CONF.bak-$TS"
sed -i 's|proxy_pass http://127.0.0.1:8080;|proxy_pass http://127.0.0.1:8092;|g' "$CONF"
echo "== nginx upstream =="
grep -n "proxy_pass http://127.0.0.1:" "$CONF"

nginx -t 2>&1 | tail -2

# ── 3. 启动 ──
systemctl restart dmm-proxy
sleep 2
echo "active=$(systemctl is-active dmm-proxy)"
ss -lnt | grep 8092 || echo "8092 未监听"
curl -s --max-time 10 127.0.0.1:8092/_stats || true
echo
systemctl reload nginx
echo "nginx reloaded"
