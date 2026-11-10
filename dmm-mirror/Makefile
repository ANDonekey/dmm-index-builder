BINARY := dmm-mirror
VPS    ?= jump-box
REMOTE ?= /opt/dmm-mirror
SSH    ?= ssh
SCP    ?= scp

.PHONY: build vet test probe deploy status logs

# uTLS 依赖 cgo？不需要：CGO_ENABLED=0 也能交叉编译，utls 是纯 Go
build:
	GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o dist/$(BINARY) .

vet:
	go vet ./...

# 指纹诊断：确认当前网络下哪个 profile 能过 CF
probe:
	go run probe.go

deploy: build
	$(SSH) $(VPS) 'mkdir -p $(REMOTE)'
	$(SCP) dist/$(BINARY) $(VPS):/tmp/$(BINARY)
	$(SSH) $(VPS) 'install -m 0755 /tmp/$(BINARY) $(REMOTE)/$(BINARY) && rm -f /tmp/$(BINARY) && systemctl restart $(BINARY)'
	@echo "已部署。检查：make status"

status:
	$(SSH) $(VPS) 'systemctl status $(BINARY) --no-pager | head -10; echo; curl -s 127.0.0.1:8091/_stats'

logs:
	$(SSH) $(VPS) 'journalctl -u $(BINARY) -f --no-pager -n 80'
