#!/usr/bin/env bash
# 在 VPS 上以 root 执行：bash /tmp/dmm-query-install.sh
#
# 前置（本机）：
#   make build
#   scp dist/dmm-query  jump-box:/tmp/dmm-query
#   scp deploy/dmm-query.env deploy/dmm-query.service deploy/nginx-api.conf jump-box:/tmp/
#   scp dmm-index-kkey.db jump-box:/tmp/dmm-index.db
#
# ⚠️ 上传前先 tr -d '\r'，否则 bash 报 $'\r': command not found

set -euo pipefail

BIN_SRC=/tmp/dmm-query
ENV_SRC=/tmp/dmm-query.env
UNIT_SRC=/tmp/dmm-query.service
DB_SRC=/tmp/dmm-index.db
NGX_SRC=/tmp/nginx-api.conf

REMOTE=/opt/dmm-query
DATADIR=/var/lib/dmmpv
LISTEN_PORT=8090

echo "== 1. 检查端口 =="
if ss -lnt | awk '{print $4}' | grep -q ":$LISTEN_PORT$"; then
  echo "✗ 端口 $LISTEN_PORT 已被占用，先处理掉再继续："
  ss -lntp | grep ":$LISTEN_PORT"
  exit 1
fi
echo "  $LISTEN_PORT 空闲"

echo "== 2. 用户与目录 =="
id -u dmmpv >/dev/null 2>&1 || useradd --system --no-create-home --shell /usr/sbin/nologin dmmpv
install -d -m 0755 "$REMOTE"
install -d -m 0755 -o dmmpv -g dmmpv "$DATADIR"
echo "  ok"

echo "== 3. 二进制与配置 =="
install -m 0755 "$BIN_SRC" "$REMOTE/dmm-query"
install -m 0644 "$ENV_SRC" /etc/dmm-query.env
install -m 0644 "$UNIT_SRC" /etc/systemd/system/dmm-query.service
systemctl daemon-reload
systemctl enable dmm-query >/dev/null
echo "  ok"

echo "== 4. 索引库 =="
if [ -f "$DB_SRC" ]; then
  install -m 0644 -o dmmpv -g dmmpv "$DB_SRC" "$DATADIR/dmm-index.db.new"
  mv -f "$DATADIR/dmm-index.db.new" "$DATADIR/dmm-index.db"
  echo "  已就位 $(du -h "$DATADIR/dmm-index.db" | cut -f1)"
else
  echo "  ⚠️ 未找到 $DB_SRC，跳过（服务会因缺库起不来）"
fi

echo "== 5. 启动 =="
systemctl restart dmm-query
sleep 1
systemctl is-active dmm-query
# /health 现在也要过准入（DMM_QUERY_KEY 配了就必须带）；没配就裸调。
# 这里直接读 env 文件，别指望脚本环境里有这个变量。
QUERY_KEY=$(sed -n 's/^DMM_QUERY_KEY=//p' /etc/dmm-query.env 2>/dev/null | head -1 || true)
curl -sf -H "X-Dmm-Key: ${QUERY_KEY}" "http://127.0.0.1:$LISTEN_PORT/health" || { echo "✗ 健康检查失败"; journalctl -u dmm-query -n 20 --no-pager; exit 1; }
echo

echo "== 6. nginx（可选） =="
CONF=/etc/nginx/conf.d/dmmpv.conf
if grep -q "location = /api" "$CONF"; then
  echo "  已存在 /api location，跳过"
else
  cp -a "$CONF" "$CONF.bak-$(date +%Y%m%d%H%M%S)"
  # 插在 "location / {" 之前
  awk -v frag="$NGX_SRC" '
    /location \/ \{/ && !done { while ((getline line < frag) > 0) print line; done=1 }
    { print }
  ' "$CONF" > "$CONF.new"
  mv -f "$CONF.new" "$CONF"
  if nginx -t 2>&1 | tail -3; then
    systemctl reload nginx
    echo "  nginx 已重载"
  else
    echo "✗ nginx 语法检查失败，已回滚"
    mv -f "$(ls -t "$CONF".bak-* | head -1)" "$CONF"
    nginx -t
    exit 1
  fi
fi

echo
echo "完成。验证："
echo "  curl -s 'https://cc3001.fastcdn.dpdns.org/api?code=ABP-888'"
