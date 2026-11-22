#!/usr/bin/env bash
set -euo pipefail

ROOT=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
cd "$ROOT"

VERSION=${VERSION:-$(git describe --tags --always --dirty 2>/dev/null || printf 'dev')}
COMMIT=${COMMIT:-$(git rev-parse --short HEAD 2>/dev/null || printf 'unknown')}
GOARCH=${GOARCH:-amd64}
BUILD_TELEGRAM=${BUILD_TELEGRAM:-0}
DIST=${DIST:-$ROOT/dist}
SOURCE_DATE_EPOCH=${SOURCE_DATE_EPOCH:-$(git log -1 --format=%ct 2>/dev/null || date +%s)}
BUILD_DATE=${BUILD_DATE:-$(date -u -d "@$SOURCE_DATE_EPOCH" +%Y-%m-%dT%H:%M:%SZ)}

[[ "$VERSION" =~ ^[A-Za-z0-9._-]+$ ]] || { echo "VERSION contains unsafe archive characters" >&2; exit 2; }

case "$GOARCH" in
  amd64|arm64) ;;
  *) echo "unsupported GOARCH: $GOARCH (use amd64 or arm64)" >&2; exit 2 ;;
esac

if ! command -v go >/dev/null 2>&1; then
  echo "go is required" >&2
  exit 2
fi

# Required verification gates for a release candidate.
"$ROOT/scripts/check-shell.sh"
mapfile -t GO_FILES < <(find . -type f -name '*.go' -not -path './generated/*' -print)
if test "${#GO_FILES[@]}" -gt 0 && test -n "$(gofmt -l "${GO_FILES[@]}")"; then
  echo "gofmt check failed; run gofmt -w on Go sources" >&2
  gofmt -l "${GO_FILES[@]}"
  exit 1
fi
go test -count=1 ./...
go test -race -count=1 ./...
go vet ./...

rm -rf "$DIST"
mkdir -p "$DIST/stage/bee-$VERSION-linux-$GOARCH"
LDFLAGS="-X github.com/seth-el/bee.Version=$VERSION -X github.com/seth-el/bee.Commit=$COMMIT -X github.com/seth-el/bee.BuildDate=$BUILD_DATE"
export CGO_ENABLED=0 GOOS=linux GOARCH

go build -trimpath -ldflags "$LDFLAGS" -o "$DIST/stage/bee-$VERSION-linux-$GOARCH/bee" ./cmd/bee
if [[ "$BUILD_TELEGRAM" == "1" ]]; then
  go build -trimpath -ldflags "$LDFLAGS" -o "$DIST/stage/bee-$VERSION-linux-$GOARCH/bee-telegram" ./cmd/bee-telegram
fi
chmod 0755 "$DIST/stage/bee-$VERSION-linux-$GOARCH"/*
cp README.md "$DIST/stage/bee-$VERSION-linux-$GOARCH/"
cp -R docs "$DIST/stage/bee-$VERSION-linux-$GOARCH/"

ARCHIVE="$DIST/bee-$VERSION-linux-$GOARCH.tar.gz"
# GNU tar/gzip options make archive bytes stable for the same tree and epoch.
tar --sort=name --mtime="@$SOURCE_DATE_EPOCH" --owner=0 --group=0 --numeric-owner -C "$DIST/stage" -cf - "bee-$VERSION-linux-$GOARCH" | gzip -n > "$ARCHIVE"
sha256sum "$ARCHIVE" > "$ARCHIVE.sha256"
rm -rf "$DIST/stage"
printf 'release: %s\narchive: %s\nchecksum: %s\n' "$VERSION" "$ARCHIVE" "$ARCHIVE.sha256"
