#!/bin/sh
# Build release binaries and their checksums into dist/.
#
#   scripts/release.sh v0.1.0
#
# Binaries are static (CGO_ENABLED=0) and reproducible (-trimpath, no
# build ID). SHA256SUMS lists them; sign it with
#   ssh-keygen -Y sign -f ~/.ssh/id_ed25519 -n file dist/SHA256SUMS
# and verify with ssh-keygen -Y verify (see docs/install.md).
set -eu
version=${1:?usage: scripts/release.sh vX.Y.Z}
cd "$(dirname "$0")/.."
rm -rf dist
mkdir -p dist
for target in linux/amd64 linux/arm64 darwin/amd64 darwin/arm64; do
	os=${target%/*} arch=${target#*/}
	out=dist/ferry-$version-$os-$arch
	GOOS=$os GOARCH=$arch CGO_ENABLED=0 go build -trimpath \
		-ldflags "-s -w -buildid= -X main.version=$version" -o "$out" ./cmd/ferry
	echo "built $out"
done
(cd dist && sha256sum ferry-* > SHA256SUMS)
cat dist/SHA256SUMS
