#!/usr/bin/env bash
# Downloads the VardeFM release that HamBBS archives include, into
# build/vardefm/<os>_<arch>/: bin/ = the program, doc/ = its readme,
# license and udev rules, docs/ = its other documents.
# GoReleaser picks the files up from there (see .goreleaser.yaml).
set -euo pipefail

VERSION=0.2.1 # VardeFM release to bundle
BASE=https://github.com/notepid/vardeFM/releases/download/v$VERSION

cd "$(dirname "$0")/.."
out=build/vardefm
rm -rf "$out"
mkdir -p "$out/download"
curl -fsSL -o "$out/download/checksums.txt" "$BASE/checksums.txt"

# HamBBS platform -> VardeFM archive. Pi 2 and newer run the armv7 build;
# Windows on ARM runs the x86-64 build.
while read -r plat asset; do
	dir=$out/$plat
	mkdir -p "$dir/bin" "$dir/doc"
	if [ "$asset" = - ]; then
		printf 'VardeFM is not available for this system yet.\nSee https://github.com/notepid/vardeFM\n' >"$dir/doc/NOT-INCLUDED.txt"
		continue
	fi
	file=vardefm_${VERSION}_$asset
	[ -f "$out/download/$file" ] || curl -fsSL -o "$out/download/$file" "$BASE/$file"
	(cd "$out/download" && grep " $file\$" checksums.txt | sha256sum -c --quiet -)
	tmp=$(mktemp -d)
	case $file in
	*.zip) unzip -q "$out/download/$file" -d "$tmp" ;;
	*) tar -xzf "$out/download/$file" -C "$tmp" ;;
	esac
	src=$tmp/${file%.zip}
	src=${src%.tar.gz}
	mv "$src"/vardefm* "$dir/bin/"
	mkdir -p "$dir/docs"
	mv "$src"/docs/* "$dir/docs/"
	rmdir "$src/docs"
	mv "$src"/* "$dir/doc/"
	rm -rf "$tmp"
done <<'LIST'
linux_amd64 linux_amd64.tar.gz
linux_arm64 linux_arm64.tar.gz
linux_armv6 linux_armv7.tar.gz
windows_amd64 windows_amd64.zip
windows_arm64 windows_amd64.zip
darwin_amd64 darwin_amd64.tar.gz
darwin_arm64 darwin_arm64.tar.gz
LIST
find "$out" -path "$out/download" -prune -o -type f -print | sort
