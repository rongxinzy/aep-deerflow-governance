#!/bin/sh
# Called in each template image. Never mutate an already published version.
set -eu
SOURCE=${1:-/payload}
DEST=$2
cd "$SOURCE"
sha256sum -c SHA256SUMS
if [ -d "$DEST" ]; then
  cmp SHA256SUMS "$DEST/SHA256SUMS"
  cd "$DEST"
  sha256sum -c SHA256SUMS
  exit 0
fi
mkdir -p "$(dirname "$DEST")"
STAGING=$(mktemp -d "${DEST}.staging.XXXXXX")
trap 'rm -rf "$STAGING"' EXIT HUP INT TERM
for f in rootfs.ext4 vmlinux firecracker SHA256SUMS; do cp "$SOURCE/$f" "$STAGING/$f"; done
if [ -f "$SOURCE/template.json" ]; then cp "$SOURCE/template.json" "$STAGING/template.json"; fi
chmod 755 "$STAGING/firecracker"
cd "$STAGING"
sha256sum -c SHA256SUMS
mv "$STAGING" "$DEST"
trap - EXIT HUP INT TERM
