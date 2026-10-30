#!/usr/bin/env bash
# Builds lazy for every platform and packages it into dist/.
set -euo pipefail
cd "$(dirname "$0")/.."

VERSION="${VERSION:-dev}"
TARGETS="darwin/arm64 darwin/amd64 linux/amd64 linux/arm64 windows/amd64 windows/arm64"

command -v zip >/dev/null || { echo "zip is required (Ubuntu: sudo apt install zip)"; exit 1; }

rm -rf dist && mkdir -p dist
go mod tidy

for target in $TARGETS; do
  os="${target%/*}"; arch="${target#*/}"
  name="lazy-${VERSION}-${os}-${arch}"
  ext=""; [ "$os" = "windows" ] && ext=".exe"
  echo "-> building $name"
  mkdir -p "dist/$name"
  CGO_ENABLED=0 GOOS="$os" GOARCH="$arch" \
    go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o "dist/$name/lazy$ext" ./cmd/lazy
  cp README.md LICENSE "dist/$name/"
  (cd dist && zip -qr "$name.zip" "$name" && rm -rf "$name")
done

echo "-> packaging source"
zip -qr "dist/lazy-${VERSION}-source.zip" . -x 'dist/*' '.git/*' '.lazyfinger/*'

echo "Done:"
ls -1 dist
