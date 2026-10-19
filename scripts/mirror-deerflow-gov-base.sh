#!/usr/bin/env bash
# Mirror one locally published deerflow-gov tag from Harbor to ghcr.
#
# build-deerflow-gov pulls its BASE from ghcr (`ghcr.io/<owner>/deerflow-gov`),
# while locally built runtime images are pushed to Harbor only. Without this
# mirror step a governance-variant dispatch fails with
#   failed to resolve source metadata ... :<base>: not found
# Run after every local push that will become the base of the next image.
#
# Usage (needs `crane`; GH_TOKEN or an authenticated `gh` CLI on the same host):
#   scripts/mirror-deerflow-gov-base.sh v29          # copy Harbor -> ghcr
#   scripts/mirror-deerflow-gov-base.sh v29 --check  # compare digests only
set -euo pipefail

tag="${1:?usage: mirror-deerflow-gov-base.sh <tag> [--check]}"
mode="${2:-copy}"
harbor="harbor.zhiyuan.local:30443/zhiyuan/deerflow-gov:${tag}"
ghcr="ghcr.io/rongxinzy/deerflow-gov:${tag}"

if ! command -v crane >/dev/null 2>&1; then
  echo "crane is required (on cicd it lives at /usr/local/bin/crane)" >&2
  exit 1
fi

harbor_digest="$(crane digest --insecure "$harbor")"
if [ "$mode" = "--check" ]; then
  ghcr_digest="$(crane digest "$ghcr" 2>/dev/null || true)"
  if [ "$ghcr_digest" != "$harbor_digest" ]; then
    echo "MISMATCH ${tag}: harbor=${harbor_digest:-<missing>} ghcr=${ghcr_digest:-<missing>}" >&2
    exit 1
  fi
  echo "in sync ${tag}: ${harbor_digest}"
  exit 0
fi

token="${GH_TOKEN:-$(gh auth token 2>/dev/null || true)}"
if [ -z "$token" ]; then
  echo "GH_TOKEN or an authenticated gh CLI is required for ghcr.io" >&2
  exit 1
fi
crane auth login ghcr.io -u "${GHCR_USER:-hawkli-1994}" -p "$token" >/dev/null
crane copy --insecure "$harbor" "$ghcr" >/dev/null
ghcr_digest="$(crane digest "$ghcr")"
if [ "$ghcr_digest" != "$harbor_digest" ]; then
  echo "copy produced a different digest: harbor=$harbor_digest ghcr=$ghcr_digest" >&2
  exit 1
fi
echo "mirrored ${tag}: ${harbor_digest}"
