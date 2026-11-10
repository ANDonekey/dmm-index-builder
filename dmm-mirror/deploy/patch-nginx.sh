#!/bin/bash
set -euo pipefail

CONF=/etc/nginx/conf.d/dmmpv.conf
FRAG=/tmp/nginx-mirror.conf

if grep -q "location /mirror/" "$CONF"; then
  echo "已存在 /mirror/ location，跳过"
  exit 0
fi

cp -a "$CONF" "$CONF.bak-$(date +%Y%m%d%H%M%S)"

awk -v frag="$FRAG" '
  /^[[:space:]]*location \/ \{/ && !done {
    while ((getline line < frag) > 0) print line
    close(frag)
    done = 1
  }
  { print }
' "$CONF" > "$CONF.new"

mv -f "$CONF.new" "$CONF"

if nginx -t 2>&1 | tail -2; then
  systemctl reload nginx
  echo "nginx 已重载"
else
  echo "语法检查失败，回滚"
  mv -f "$(ls -t "$CONF".bak-* | head -1)" "$CONF"
  nginx -t
  exit 1
fi

grep -n "location /mirror/" "$CONF"
