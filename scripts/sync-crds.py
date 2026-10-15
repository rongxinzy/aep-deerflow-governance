#!/usr/bin/env python3
"""Sync the operator's generated CRDs into every place that ships a copy.

`operator/Makefile` only writes `operator/config/crd/bases/` (controller-gen).
Two other files ship a copy of that output:

  * `charts/zhiyuan-stack/crds/*.yaml` — Helm applies these; a stale copy makes
    the Kubernetes structural schema **silently prune** fields the schema does
    not declare, so the CR the operator reconciles loses them.
  * `operator/deploy/crd.yaml` — the bundle `k3s/cicd` and the local deploy
    apply. Same failure mode.

Both are generated from the same source here so they cannot drift apart.

Usage:
  python3 scripts/sync-crds.py           # rewrite every copy
  python3 scripts/sync-crds.py --check   # exit 1 when a copy drifted (CI)
"""

from __future__ import annotations

import argparse
import sys
from pathlib import Path

REPO = Path(__file__).resolve().parent.parent
BASE_DIR = REPO / "operator" / "config" / "crd" / "bases"
CHART_CRD_DIR = REPO / "charts" / "zhiyuan-stack" / "crds"
DEPLOY_BUNDLE = REPO / "operator" / "deploy" / "crd.yaml"

# operator/config/crd/bases/<key> -> charts/zhiyuan-stack/crds/<value>
# Deliberately explicit: adding a CRD without listing it here is a mistake the
# script should surface rather than guess at.
CRD_TARGETS: dict[str, str] = {
    "digitalemployee.zhiyuan.io_digitalemployees.yaml": "digitalemployee.yaml",
    "digitalemployee.zhiyuan.io_personalworkspaces.yaml": "personalworkspace.yaml",
}


def main(argv: list[str]) -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument(
        "--check",
        action="store_true",
        help="do not write; fail if a copy differs from the operator source",
    )
    args = parser.parse_args(argv)

    sources = sorted(BASE_DIR.glob("*.yaml"))
    if not sources:
        print(f"no CRDs found under {BASE_DIR}", file=sys.stderr)
        return 1

    drift: list[tuple[str, str]] = []  # (path, description)

    def sync(path: Path, wanted: str, description: str) -> None:
        current = path.read_text(encoding="utf-8") if path.exists() else ""
        if wanted == current:
            return
        drift.append((str(path.relative_to(REPO)), description))
        if not args.check:
            path.parent.mkdir(parents=True, exist_ok=True)
            path.write_text(wanted, encoding="utf-8", newline="\n")
            print(f"synced {path.relative_to(REPO)} <- {description}")

    # One file per CRD, alongside the chart.
    for source in sources:
        target_name = CRD_TARGETS.get(source.name)
        if target_name is None:
            print(
                f"error: {source.name} has no chart target; add it to "
                f"CRD_TARGETS in {Path(__file__).name} and ship the CRD "
                "with the chart",
                file=sys.stderr,
            )
            return 1
        sync(
            CHART_CRD_DIR / target_name,
            source.read_text(encoding="utf-8"),
            source.name,
        )

    # Every document in one bundle, in the same order, separated by `---`.
    # The base files already open with `---`, so the separator appears twice
    # between documents — that is the established shape of this file.
    bundle = "".join(
        ("---\n" if index else "") + source.read_text(encoding="utf-8")
        for index, source in enumerate(sources)
    )
    sync(DEPLOY_BUNDLE, bundle, "operator/config/crd/bases/*")

    if not drift:
        print("CRD copies are in sync with the operator sources")
        return 0

    if args.check:
        for path, description in drift:
            print(f"error: {path} is out of date (from {description})", file=sys.stderr)
        print(
            "\nrun `python3 scripts/sync-crds.py` and commit the result",
            file=sys.stderr,
        )
        return 1

    return 0


if __name__ == "__main__":
    raise SystemExit(main(sys.argv[1:]))
