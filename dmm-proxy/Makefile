BINARY  := dmm-proxy
VPS     ?= jump-box
REMOTE  ?= /opt/dmm-proxy
SSH     ?= ssh
SCP     ?= scp

.PHONY: build vet fmt test deploy logs status

build:
	GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o dist/$(BINARY) .

vet:
	go vet ./...
	GOOS=linux GOARCH=amd64 go vet ./...

fmt:
	gofmt -w .

test:
	go test ./...

deploy: build
	$(SCP) dist/$(BINARY) $(VPS):/tmp/$(BINARY)
	$(SSH) $(VPS) 'install -m 0755 /tmp/$(BINARY) $(REMOTE)/$(BINARY) && rm -f /tmp/$(BINARY) && systemctl restart $(BINARY)'

logs:
	$(SSH) $(VPS) 'journalctl -u $(BINARY) -f --no-pager -n 100'

status:
	$(SSH) $(VPS) 'systemctl status $(BINARY) --no-pager; curl -s 127.0.0.1:8080/_stats'
