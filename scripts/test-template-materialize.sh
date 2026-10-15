#!/bin/sh
set -eu
SCRIPT=$(cd "$(dirname "$0")/../charts/zhiyuan-stack/files" && pwd)/materialize-template.sh
TEST_DIR=$(mktemp -d)
trap 'rm -rf "$TEST_DIR"' EXIT HUP INT TERM
mkdir "$TEST_DIR/payload"
for f in rootfs.ext4 vmlinux firecracker template.json; do printf '%s\n' "$f" > "$TEST_DIR/payload/$f"; done
(cd "$TEST_DIR/payload" && sha256sum rootfs.ext4 vmlinux firecracker template.json > SHA256SUMS)
sh "$SCRIPT" "$TEST_DIR/payload" "$TEST_DIR/templates/v1" >/dev/null
sh "$SCRIPT" "$TEST_DIR/payload" "$TEST_DIR/templates/v1" >/dev/null
test -x "$TEST_DIR/templates/v1/firecracker"
# An interrupted or corrupted destination must not silently count as distributed.
printf corrupt > "$TEST_DIR/templates/v1/rootfs.ext4"
if sh "$SCRIPT" "$TEST_DIR/payload" "$TEST_DIR/templates/v1" >/dev/null 2>&1; then exit 1; fi
# Invalid source must never become a selectable version.
printf corrupt > "$TEST_DIR/payload/rootfs.ext4"
if sh "$SCRIPT" "$TEST_DIR/payload" "$TEST_DIR/templates/v2" >/dev/null 2>&1; then exit 1; fi
test ! -e "$TEST_DIR/templates/v2"
echo 'template materialization: publication, idempotence and corruption rejection passed'
