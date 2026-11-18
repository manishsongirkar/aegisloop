#!/bin/sh
# Build AegisLoop from this checkout and install `aegis` and `aegisloop`.
set -eu

here=$(cd "$(dirname "$0")/.." && pwd)
command -v go >/dev/null 2>&1 || { echo "error: Go >= 1.22 is required (https://go.dev/dl/)" >&2; exit 1; }
command -v git >/dev/null 2>&1 || { echo "error: git is required" >&2; exit 1; }

gobin=$(go env GOBIN)
[ -n "$gobin" ] || gobin="$(go env GOPATH)/bin"
mkdir -p "$gobin"

cd "$here"
version=$(git describe --tags --always --dirty 2>/dev/null || echo dev)
go build -trimpath -ldflags "-s -w -X main.version=$version" -o "$gobin/aegis" ./cmd/aegis
cp "$gobin/aegis" "$gobin/aegisloop"
echo "installed: $gobin/aegis (also as aegisloop) — version $("$gobin/aegis" version)"

case ":$PATH:" in
  *":$gobin:"*) ;;
  *) echo "note: add this to your shell profile:  export PATH=\"$gobin:\$PATH\"" ;;
esac
