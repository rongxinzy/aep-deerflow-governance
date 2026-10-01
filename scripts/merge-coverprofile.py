#!/usr/bin/env python3
"""Merge a multi-binary Go coverprofile.

With -coverpkg across packages, `go test ./...` appends one block line per
test binary, and `go tool cover -func` only honours the last occurrence —
coverage from earlier binaries is silently lost. This keeps the maximum hit
per block so any test exercising a block counts.

Usage: merge-coverprofile.py <in.profile> <out.profile>
"""

from __future__ import annotations

import sys


def main() -> None:
    if len(sys.argv) != 3:
        raise SystemExit(__doc__)
    source, target = sys.argv[1], sys.argv[2]
    best: dict[str, tuple[int, str]] = {}
    order: list[str] = []
    with open(source) as fh:
        mode = fh.readline()
        for line in fh:
            key = line.split(" ")[0]
            hit = int(line.rsplit(" ", 1)[1])
            if key not in best:
                order.append(key)
                best[key] = (hit, line)
            elif hit > 0 and best[key][0] == 0:
                head = line.rstrip("\n").rsplit(" ", 1)[0]
                best[key] = (1, f"{head} 1\n")
    with open(target, "w") as out:
        out.write(mode)
        for key in order:
            out.write(best[key][1])


if __name__ == "__main__":
    main()
