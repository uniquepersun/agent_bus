#!/usr/bin/env bash
# Builds release archives in dist/: one per platform, each with the five
# binaries, demo.sh, the example configuration, and the docs.
#
#   ./release.sh v0.1.0
set -euo pipefail
version="${1:?usage: ./release.sh <version>}"
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
cd "$ROOT"
rm -rf dist && mkdir dist
for target in darwin/arm64 darwin/amd64 linux/amd64 linux/arm64; do
  os="${target%/*}" arch="${target#*/}"
  name="agent-bus-$version-$os-$arch"
  stage="dist/$name"
  mkdir -p "$stage/bin"
  CGO_ENABLED=0 GOOS="$os" GOARCH="$arch" go build -trimpath -ldflags "-s -w" -o "$stage/bin/" ./cmd/...
  cp -R demo.sh bus.json mandates seed README.md INSTRUCTIONS.md TWO-LAPTOPS.md SPEC.md "$stage/"
  tar -C dist -czf "dist/$name.tar.gz" "$name"
  rm -rf "$stage"
  echo "built dist/$name.tar.gz"
done
(cd dist && shasum -a 256 *.tar.gz > SHA256SUMS)
