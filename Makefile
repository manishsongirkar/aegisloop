.PHONY: build install test vet check

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
