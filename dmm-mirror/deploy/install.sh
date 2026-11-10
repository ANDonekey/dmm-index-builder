#!/bin/bash
set -euo pipefail

pkill -f "/tmp/dmm-mirror" 2>/dev/null || true
sleep 1

id -u dmmpv >/dev/null 2>&1 || useradd --system --no-create-home --shell /usr/sbin/nologin dmmpv

install -d -m 0755 /opt/dmm-mirror
install -m 0755 /tmp/dmm-mirror /opt/dmm-mirror/dmm-mirror
install -m 0644 /tmp/dmm-mirror.env /etc/dmm-mirror.env
install -m 0644 /tmp/dmm-mirror.service /etc/systemd/system/dmm-mirror.service

systemctl daemon-reload
systemctl enable dmm-mirror >/dev/null 2>&1
systemctl restart dmm-mirror
sleep 2

echo "active=$(systemctl is-active dmm-mirror)"
curl -s 127.0.0.1:8091/_stats
echo
ss -lntp | grep 8091 || true
