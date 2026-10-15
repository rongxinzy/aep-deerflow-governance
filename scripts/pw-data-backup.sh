#!/usr/bin/env bash
# Manual backup/restore of a PersonalWorkspace microVM data disk (P1).
#
# The data disk is a sparse ext4 image at
# /var/lib/zhiyuan/workspace-agent/disks/<ws>/data.ext4 on the node named in
# status.nodeName. There is no automatic backup; CR deletion removes the disk
# (workspace-agent manager teardownWorkspace). Runbook: zhiyuan-docs
# guides/microvm-runtime.md.
#
# Usage:
#   scripts/pw-data-backup.sh backup  <workspace> <node> <dest-dir> [--yes] [--dry-run]
#   scripts/pw-data-backup.sh restore <workspace> <target-node> <backup-file> [--yes] [--dry-run]
#
# backup:  pulls data.ext4 from <node> into <dest-dir> (sparse preserved).
# restore: pushes a backup file onto <target-node> as the workspace data disk;
#          clearing status.nodeName afterwards is a manual runbook step.
#
# Neither subcommand stops a VM, patches the CR, or deletes anything. Confirm
# the VM is stopped before backup; restore refuses a node whose VM looks
# running (fc.sock present) and refuses to overwrite an existing disk unless
# --force is given.
set -euo pipefail

WORKDIR=/var/lib/zhiyuan/workspace-agent

usage() {
  sed -n '2,22p' "$0"
  exit "${1:-1}"
}

log() { printf '%s\n' "$*" >&2; }
die() { log "error: $*"; exit 1; }

confirm() {
  [ "$ASSUME_YES" = 1 ] && return 0
  printf 'proceed? [y/N] ' >&2
  read -r reply
  case "$reply" in y | Y | yes) ;; *) die "aborted" ;; esac
}

run() {
  if [ "$DRY_RUN" = 1 ]; then
    log "dry-run: $*"
  else
    "$@"
  fi
}

# find_pw <workspace> -> prints "<namespace> <phase> <nodeName>" (advisory).
find_pw() {
  local ws="$1" ns
  ns=$(kubectl get pw -A --no-headers 2>/dev/null | awk -v w="$ws" '$2 == w {print $1; exit}') \
    || die "kubectl get pw failed (check kubeconfig)"
  [ -n "$ns" ] || die "PersonalWorkspace '$ws' not found in any namespace"
  kubectl -n "$ns" get pw "$ws" \
    -o jsonpath="$ns {.status.phase} {.status.nodeName}{'\n'}" 2>/dev/null \
    || die "cannot read status of pw $ns/$ws"
}

cmd_backup() {
  local ws="$1" node="$2" dest="$3"
  local src="$WORKDIR/disks/$ws/data.ext4"

  log "== advisory checks (no state is changed) =="
  local info phase node_name
  if ! info=$(find_pw "$ws"); then
    [ "$DRY_RUN" = 1 ] || die "cannot read PersonalWorkspace '$ws' via kubectl"
    log "WARNING: cannot read CR via kubectl (dry-run continues)"
    info="? ? ?"
  fi
  phase=$(echo "$info" | awk '{print $2}')
  node_name=$(echo "$info" | awk '{print $3}')
  log "CR: namespace=$(echo "$info" | awk '{print $1}') phase=${phase:-<empty>} nodeName=${node_name:-<empty>}"
  if [ -n "$node_name" ] && [ "$node_name" != "?" ] && [ "$node_name" != "$node" ]; then
    log "WARNING: status.nodeName is '$node_name', not '$node'; the disk on '$node' may be stale"
  fi
  if [ "$DRY_RUN" != 1 ]; then
    ssh "$node" "test -f '$src'" || die "$src not found on $node"
    if ssh "$node" "test -S '$WORKDIR/run/$ws/fc.sock'"; then
      log "WARNING: $WORKDIR/run/$ws/fc.sock exists on $node — the VM looks RUNNING."
      log "Copying a live disk is inconsistent. Stop it first (e.g. touch any spec"
      log "field to force a config-roll stop/start, or use a maintenance window)."
      confirm
    fi
  fi

  local out="$dest/${ws}-data-$(date -u +%Y%m%dT%H%M%SZ).ext4"
  log "== backup: $node:$src -> $out (sparse) =="
  confirm
  [ "$DRY_RUN" = 1 ] || mkdir -p "$dest"
  # cp --sparse=always reading a stream re-sparsifies zero ranges on write;
  # only coreutils is needed on both ends (rsync -S is an alternative).
  if [ "$DRY_RUN" = 1 ]; then
    log "dry-run: ssh $node cat $src | cp --sparse=always /dev/stdin $out.tmp && mv $out.tmp $out"
  else
    ssh "$node" "cat '$src'" | cp --sparse=always /dev/stdin "$out.tmp"
    mv "$out.tmp" "$out"
    local remote_size local_size
    remote_size=$(ssh "$node" "stat -c %s '$src'")
    local_size=$(stat -c %s "$out")
    [ "$remote_size" = "$local_size" ] \
      || die "size mismatch: remote $remote_size vs local $local_size (keeping $out for inspection)"
    log "ok: $out (apparent ${local_size} bytes, actual $(du -h "$out" | cut -f1))"
  fi

  cat <<EOF

Restore pointer (manual steps, runbook: guides/microvm-runtime.md):
  scripts/pw-data-backup.sh restore $ws <target-node> $out
Optional integrity record: sha256sum "$out"  (reads the full apparent size)
EOF
}

cmd_restore() {
  local ws="$1" node="$2" file="$3"
  local dst_dir="$WORKDIR/disks/$ws"
  local dst="$dst_dir/data.ext4"

  [ -f "$file" ] || die "backup file not found: $file"

  log "== advisory checks (no state is changed) =="
  if info=$(find_pw "$ws" 2>/dev/null); then
    log "CR: namespace=$(echo "$info" | awk '{print $1}') phase=$(echo "$info" | awk '{print $2}') nodeName=$(echo "$info" | awk '{print $3}')"
  else
    log "CR '$ws' not found — restoring the disk file only; create/reassign the CR afterwards"
  fi
  log "NOTE: after restore, clearing status.nodeName lets the operator reassign"
  log "to the FIRST ready node in workspaceAgent.microvmNodes pool order. Make"
  log "sure '$node' is that node (or restore onto every candidate)."
  if [ "$DRY_RUN" != 1 ]; then
    # A live VM keeps the old disk inode open after mv; every guest write
    # after the swap lands in the unlinked inode and is lost. Unlike the
    # backup advisory check there is no acceptable "proceed anyway" here.
    if ssh "$node" "test -S '$WORKDIR/run/$ws/fc.sock'"; then
      die "$WORKDIR/run/$ws/fc.sock exists on $node — the VM looks RUNNING. Stop it before restoring over its disk."
    fi
    if ssh "$node" "test -e '$dst'"; then
      [ "$FORCE" = 1 ] || die "$dst already exists on $node (agent-created empty disk?); use --force to overwrite"
      log "WARNING: overwriting existing $dst on $node"
    fi
  fi

  log "== restore: $file -> $node:$dst (sparse) =="
  confirm
  if [ "$DRY_RUN" = 1 ]; then
    log "dry-run: ssh $node mkdir -p $dst_dir; cat $file | ssh $node 'cp --sparse=always /dev/stdin $dst.tmp'; verify size; ssh $node 'mv $dst.tmp $dst'"
  else
    ssh "$node" "mkdir -p '$dst_dir'"
    # Re-sparsify on the RECEIVING side (remote cp reading /dev/stdin): the
    # local --sparse=always only affects a regular-file destination, so piping
    # through cat would materialize the full apparent size on the node.
    # Verify-then-mv: a dropped ssh connection makes the remote cp exit cleanly
    # on EOF, so a truncated .tmp must never be promoted over a good disk.
    cat "$file" | ssh "$node" "cp --sparse=always /dev/stdin '$dst.tmp'"
    local local_size remote_size
    local_size=$(stat -c %s "$file")
    remote_size=$(ssh "$node" "stat -c %s '$dst.tmp'")
    [ "$local_size" = "$remote_size" ] \
      || die "size mismatch: local $local_size vs remote $remote_size (.tmp left on $node for inspection; $dst untouched)"
    ssh "$node" "mv '$dst.tmp' '$dst'"
    log "ok: $node:$dst (apparent ${remote_size} bytes, actual $(ssh "$node" "du -h '$dst'" | cut -f1))"
  fi

  cat <<EOF

Next manual steps (runbook: guides/microvm-runtime.md):
  1. Clear the sticky assignment so the operator reassigns:
       kubectl patch pw $ws -n <namespace> --subresource=status --type=merge \\
         -p '{"status":{"nodeName":"","vmSubnet":""}}'
  2. The agent on the target node keeps an existing data.ext4
     (EnsureDataDisk no-ops when the file exists) and boots with your data.
  3. Verify: kubectl get pw $ws -n <namespace>  -> phase Running, then
       curl http://<gatewayEndpoint>/healthz
EOF
}

[ $# -ge 1 ] || usage
cmd="$1"; shift
ASSUME_YES=0 DRY_RUN=0 FORCE=0
args=()
for a in "$@"; do
  case "$a" in
    --yes | -y) ASSUME_YES=1 ;;
    --dry-run) DRY_RUN=1 ;;
    --force) FORCE=1 ;;
    -h | --help) usage 0 ;;
    *) args+=("$a") ;;
  esac
done

case "$cmd" in
  backup)
    [ ${#args[@]} -eq 3 ] || usage
    cmd_backup "${args[@]}"
    ;;
  restore)
    [ ${#args[@]} -eq 3 ] || usage
    cmd_restore "${args[@]}"
    ;;
  *) usage ;;
esac
