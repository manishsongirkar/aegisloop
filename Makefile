.PHONY: build install test vet check dist

build:
	go build -trimpath -o bin/aegis ./cmd/aegis
	cp bin/aegis bin/aegisloop

install:
	./scripts/install.sh

test:
	go test -race ./...

vet:
	go vet ./...

check:
	@test -z "$$(gofmt -l .)" || { gofmt -l .; exit 1; }
	go vet ./...
	go test -race ./...

# Cross-compiled release archives and checksums in dist/ (git-ignored).
VERSION ?= $(shell git describe --tags --always --dirty)
dist:
	@for t in darwin/amd64 darwin/arm64 linux/amd64 linux/arm64 windows/amd64; do \
		os=$${t%/*}; arch=$${t#*/}; ext=; [ $$os = windows ] && ext=.exe; \
		name=aegis_$(VERSION)_$${os}_$${arch}; stage=dist/stage/$${os}_$${arch}; \
		mkdir -p $$stage && \
		CGO_ENABLED=0 GOOS=$$os GOARCH=$$arch go build -trimpath -ldflags "-s -w -X main.version=$(VERSION)" -o $$stage/aegis$$ext ./cmd/aegis && \
		cp LICENSE README.md $$stage/ && \
		if [ $$os = windows ]; then (cd $$stage && zip -q ../../$$name.zip aegis$$ext LICENSE README.md); \
		else tar -C $$stage -czf dist/$$name.tar.gz aegis LICENSE README.md; fi || exit 1; \
	done
	cd dist && shasum -a 256 aegis_*.tar.gz aegis_*.zip > checksums.txt
