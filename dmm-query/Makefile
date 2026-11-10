# dmm-query 部署
#
# ⚠️ Windows 上 Git Bash 自带的 ssh 不认 Windows ssh-agent，必须指定 OpenSSH：
#     make deploy SSH=/c/Windows/System32/OpenSSH/ssh.exe \
#                 SCP=/c/Windows/System32/OpenSSH/scp.exe

BINARY  := dmm-query
VPS     ?= jump-box
REMOTE  ?= /opt/dmm-query
DATADIR ?= /var/lib/dmmpv
DBFILE  ?= dmm-index.db
SSH     ?= ssh
SCP     ?= scp

.PHONY: build vet test bench deploy push-index reload status logs

# 纯 Go（modernc.org/sqlite），CGO_ENABLED=0 才能交叉编译出静态二进制
build:
	GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o dist/$(BINARY) .

vet:
	go vet ./...

test:
	go test ./...

# 本机压测：先 make run-local，再 python bench_keepalive.py
run-local:
	LISTEN=127.0.0.1:8099 DB_PATH=../dmm-index-kkey.db ./$(BINARY).exe

bench:
	python bench_keepalive.py 127.0.0.1 8099 20 250

deploy: build
	$(SSH) $(VPS) 'mkdir -p $(REMOTE) $(DATADIR)'
	$(SCP) dist/$(BINARY) $(VPS):/tmp/$(BINARY)
	$(SSH) $(VPS) 'install -m 0755 /tmp/$(BINARY) $(REMOTE)/$(BINARY) && rm -f /tmp/$(BINARY) && systemctl restart $(BINARY)'
	@echo "已部署。检查：make status"

# 推送新索引：先传成 .new，再原子 mv，最后 SIGHUP 让服务重开库。
# 直接覆盖正在被打开的文件会读到半截内容 —— 必须先落临时名再 rename。
push-index:
	$(SCP) $(DBFILE) $(VPS):$(DATADIR)/$(DBFILE).new
	$(SSH) $(VPS) 'chown dmmpv:dmmpv $(DATADIR)/$(DBFILE).new && mv -f $(DATADIR)/$(DBFILE).new $(DATADIR)/$(DBFILE) && systemctl reload $(BINARY)'
	@echo "索引已替换并 reload"

reload:
	$(SSH) $(VPS) 'systemctl reload $(BINARY)'

status:
	$(SSH) $(VPS) 'systemctl status $(BINARY) --no-pager | head -12; echo; curl -s 127.0.0.1:8090/health; echo; curl -s 127.0.0.1:8090/_stats'

logs:
	$(SSH) $(VPS) 'journalctl -u $(BINARY) -f --no-pager -n 80'
