#!/usr/bin/env python3
"""Sync the operator's generated DigitalEmployee CRDs into the Helm chart.

`operator/Makefile` only writes `operator/config/crd/bases/` (controller-gen).
The chart ships its own copies under `charts/zhiyuan-stack/crds/` and Helm
applies those, so any regenerated CRD must be copied over — otherwise Helm
applies a stale schema and the Kubernetes structural schema **silently prunes**
fields the schema does not declare.

`operator/deploy/crd.yaml` is regenerated separately by the release target, so
it is intentionally not covered here.

Usage:
  python3 scripts/sync-crds.py           # rewrite the chart copies
  python3 scripts/sync-crds.py --check   # exit 1 when a copy drifted (CI)
"""

from __future__ import annotations

import argparse
import sys
from pathlib import Path

REPO = Path(__file__).resolve().parent.parent
BASE_DIR = REPO / "operator" / "config" / "crd" / "bases"
CHART_CRD_DIR = REPO / "charts" / "zhiyuan-stack" / "crds"

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
        help="do not write; fail if a chart copy differs from the operator source",
    )
    args = parser.parse_args(argv)

    sources = sorted(BASE_DIR.glob("*.yaml"))
    if not sources:
        print(f"no CRDs found under {BASE_DIR}", file=sys.stderr)
        return 1

    drifted: list[str] = []
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

        target = CHART_CRD_DIR / target_name
        wanted = source.read_text(encoding="utf-8")
        current = target.read_text(encoding="utf-8") if target.exists() else ""

        if wanted == current:
            continue

        drifted.append(f"{target_name} (from {source.name})")
        if not args.check:
            target.write_text(wanted, encoding="utf-8", newline="\n")
            print(f"synced {target_name} <- {source.name}")

    if not drifted:
        print("chart CRDs are in sync with the operator sources")
        return 0

    if args.check:
        for name in drifted:
            print(f"error: {name} is out of date", file=sys.stderr)
        print(
            "\nrun `python3 scripts/sync-crds.py` and commit the result",
            file=sys.stderr,
        )
        return 1

    return 0


if __name__ == "__main__":
    raise SystemExit(main(sys.argv[1:]))
