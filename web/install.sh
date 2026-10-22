#!/bin/sh
# Install ferry: download the release for this machine, check its SHA-256
# against the published list, and put it in ~/.local/bin (or $FERRY_BIN).
#
#   curl -fsSL https://zahakj.github.io/ferry/install.sh | sh
#
# Read it first if you like; it is short.
set -eu

repo=ZahakJ/ferry
dest=${FERRY_BIN:-$HOME/.local/bin}

os=$(uname -s | tr '[:upper:]' '[:lower:]')
case $os in linux|darwin) ;; *) echo "ferry: no build for $os yet" >&2; exit 1 ;; esac
arch=$(uname -m)
case $arch in
	x86_64|amd64) arch=amd64 ;;
	aarch64|arm64) arch=arm64 ;;
	*) echo "ferry: no build for $arch yet" >&2; exit 1 ;;
esac

fetch() { curl -fsSL "$1" -o "$2" 2>/dev/null || wget -qO "$2" "$1"; }

tag=${FERRY_VERSION:-$(curl -fsSLI -o /dev/null -w '%{url_effective}' "https://github.com/$repo/releases/latest" | sed 's|.*/||')}
[ -n "$tag" ] || { echo "ferry: could not find the latest release" >&2; exit 1; }
name=ferry-$tag-$os-$arch
base=${FERRY_RELEASE_URL:-https://github.com/$repo/releases/download/$tag}

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
echo "Downloading ferry $tag for $os/$arch…"
fetch "$base/$name" "$tmp/$name"
fetch "$base/SHA256SUMS" "$tmp/SHA256SUMS"

want=$(grep " $name\$" "$tmp/SHA256SUMS" | cut -d' ' -f1)
if command -v sha256sum >/dev/null; then have=$(sha256sum "$tmp/$name" | cut -d' ' -f1)
else have=$(shasum -a 256 "$tmp/$name" | cut -d' ' -f1); fi
[ -n "$want" ] && [ "$want" = "$have" ] || { echo "ferry: checksum mismatch; not installing" >&2; exit 1; }

mkdir -p "$dest"
install -m 0755 "$tmp/$name" "$dest/ferry"
echo "Installed $dest/ferry (sha256 $have)."
case :$PATH: in *":$dest:"*) ;; *) echo "Add $dest to your PATH to run it as: ferry" ;; esac
echo "Next: ferry check"
