#!/usr/bin/env bash
# Cross-compile static release binaries into dist/ and bundle the Linux ones
# into the one-click installer dist/filerelay-install.sh.
set -euo pipefail
cd "$(dirname "$0")"
mkdir -p dist
version=$(sed -n 's/^var version = "\(.*\)"/\1/p' main.go)

build() { # os arch [goarm]
	local out="dist/filerelay-$1-$2"
	[ "$1" = windows ] && out="$out.exe"
	CGO_ENABLED=0 GOOS="$1" GOARCH="$2" GOARM="${3:-}" go build -trimpath -ldflags "-s -w" -o "$out" .
	echo "built $out"
}
build linux amd64
build linux arm64
build linux arm 6 # runs on armv6 and armv7
build darwin arm64
build windows amd64

# Installer = deploy/install.sh + marker line + tar.gz payload. The script
# extracts the payload with `tail -n +<line>`, so record where it starts.
linux=(filerelay-linux-amd64 filerelay-linux-arm64 filerelay-linux-arm)
(cd dist && sha256sum "${linux[@]}" >SHA256SUMS)
tar -C dist -czf dist/payload.tar.gz "${linux[@]}" SHA256SUMS
lines=$(wc -l <deploy/install.sh)
{
	sed -e "s/^PAYLOAD_LINE=__PAYLOAD_LINE__$/PAYLOAD_LINE=$((lines + 2))/" \
		-e "s/^VERSION=__VERSION__$/VERSION=$version/" deploy/install.sh
	echo "__FILERELAY_PAYLOAD__"
	cat dist/payload.tar.gz
} >dist/filerelay-install.sh
chmod +x dist/filerelay-install.sh
rm dist/payload.tar.gz dist/SHA256SUMS
grep -q "^PAYLOAD_LINE=$((lines + 2))$" dist/filerelay-install.sh || { echo "failed to stamp payload offset" >&2; exit 1; }
echo "built dist/filerelay-install.sh ($(du -h dist/filerelay-install.sh | cut -f1))"
