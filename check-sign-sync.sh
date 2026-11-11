#!/bin/bash
# sign.go / sign_test.go 在 dmm-query、dmm-proxy、dmm-mirror 三个目录里各有一份
# （三个独立 go module，共享不了包）。签发方与校验方一旦走样，表现是
# 「所有预览 403」这种最难定位的故障，所以把「三份必须逐字一致」做成可检查的。
#
# 用法：./check-sign-sync.sh   （在仓库根目录跑；不一致时退出码 1）
set -euo pipefail

cd "$(dirname "$0")"

fail=0
for f in sign.go sign_test.go; do
  ref="dmm-query/$f"
  if [ ! -f "$ref" ]; then
    echo "缺少基准文件 $ref"
    exit 1
  fi
  for d in dmm-proxy dmm-mirror; do
    if ! cmp -s "$ref" "$d/$f"; then
      echo "❌ $d/$f 与 dmm-query/$f 不一致"
      diff -u "$ref" "$d/$f" | head -20 || true
      fail=1
    fi
  done
done

if [ "$fail" -ne 0 ]; then
  echo ""
  echo "签名实现已分叉。请以 dmm-query 为准同步另外两份，或反向同步。"
  exit 1
fi

echo "✅ sign.go / sign_test.go 三份一致"
