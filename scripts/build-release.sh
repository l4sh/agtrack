#!/usr/bin/env bash
# Cross-compile agtrack for every supported platform from one host.
#
#   scripts/build-release.sh [VERSION]     # default: git describe
#
# No cgo anywhere (modernc.org/sqlite is pure Go), so this is a for loop.
# Output: dist/agtrack_<version>_<os>_<arch>.tar.gz plus dist/SHA256SUMS.
set -euo pipefail

cd "$(dirname "$0")/.."
version="${1:-$(git describe --tags --always --dirty 2>/dev/null || echo dev)}"
version="${version#v}"
targets=(linux/amd64 linux/arm64 darwin/amd64 darwin/arm64)

rm -rf dist
mkdir -p dist
for target in "${targets[@]}"; do
  os="${target%/*}"
  arch="${target#*/}"
  name="agtrack_${version}_${os}_${arch}"
  stage="dist/${name}"
  mkdir -p "$stage"
  CGO_ENABLED=0 GOOS="$os" GOARCH="$arch" go build -trimpath \
    -ldflags "-s -w -X github.com/l4sh/agtrack/internal/cli.Version=${version}" \
    -o "$stage/agtrack" ./cmd/agtrack
  cp README.md LICENSE "$stage/" 2>/dev/null || cp README.md "$stage/"
  tar -C dist -czf "dist/${name}.tar.gz" "$name"
  rm -rf "$stage"
  printf 'built %s\n' "dist/${name}.tar.gz"
done
(cd dist && sha256sum ./*.tar.gz | sed 's# \./# #' > SHA256SUMS)
cat dist/SHA256SUMS
