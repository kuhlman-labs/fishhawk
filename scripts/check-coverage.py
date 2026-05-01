#!/usr/bin/env python3
"""
Aggregate Go coverage profiles, filter excluded paths, and exit
non-zero if the result is below the threshold. Used by CI to enforce
the coverage targets documented in docs/ARCHITECTURE.md §9.

Usage:
    scripts/check-coverage.py \\
        --threshold 80 \\
        --exclude 'internal/run/db' \\
        backend/coverage.out [more.out ...]

Reads each profile, sums statement counts (excluding any line whose
file path contains an --exclude substring), and computes
covered/total. Prints a per-package breakdown for visibility.
Profiles must be in Go's standard `go test -coverprofile=` format.
"""

import argparse
import sys
from collections import defaultdict


def parse_profile(path, excludes):
    """Yield (pkg, file_path, num_stmts, count) for non-excluded lines."""
    with open(path) as f:
        first = next(f, None)
        if first is None or not first.startswith("mode:"):
            raise SystemExit(f"{path}: missing mode line; not a coverage profile")
        for line in f:
            line = line.rstrip("\n")
            if not line:
                continue
            # Format: <file>:<startline>.<startcol>,<endline>.<endcol> <numstmts> <count>
            try:
                loc, numstmts, count = line.rsplit(" ", 2)
                file_path = loc.split(":", 1)[0]
                num_stmts = int(numstmts)
                run_count = int(count)
            except (ValueError, IndexError) as e:
                raise SystemExit(f"{path}: bad line: {line!r}: {e}")
            if any(ex in file_path for ex in excludes):
                continue
            pkg = file_path.rsplit("/", 1)[0]
            yield pkg, file_path, num_stmts, run_count


def main():
    ap = argparse.ArgumentParser(description=__doc__.strip().split("\n", 1)[0])
    ap.add_argument(
        "--threshold",
        type=float,
        required=True,
        help="Minimum aggregate coverage percentage (e.g. 80).",
    )
    ap.add_argument(
        "--exclude",
        action="append",
        default=[],
        metavar="SUBSTRING",
        help="Substring of a file path; profiles entries matching it are dropped before counting. May be repeated.",
    )
    ap.add_argument("profile", nargs="+", help="One or more Go coverage profiles.")
    args = ap.parse_args()

    pkg_stmts = defaultdict(int)
    pkg_covered = defaultdict(int)

    for path in args.profile:
        for pkg, _, n, c in parse_profile(path, args.exclude):
            pkg_stmts[pkg] += n
            if c > 0:
                pkg_covered[pkg] += n

    total_stmts = sum(pkg_stmts.values())
    total_covered = sum(pkg_covered.values())

    if total_stmts == 0:
        print(
            "ERROR: zero statements counted; check --exclude patterns and profile paths.",
            file=sys.stderr,
        )
        return 2

    print("Per-package coverage (excluded paths filtered out):")
    pkg_col = max(len(p) for p in pkg_stmts) + 2
    for pkg in sorted(pkg_stmts):
        s = pkg_stmts[pkg]
        c = pkg_covered[pkg]
        pct = 100.0 * c / s if s else 0.0
        # Strip the long module prefix from output for readability.
        short = pkg.replace("github.com/kuhlman-labs/fishhawk/", "")
        print(f"  {short:<{pkg_col}}  {c:>5}/{s:<5}  {pct:5.1f}%")

    pct = 100.0 * total_covered / total_stmts
    print()
    print(f"Aggregate: {total_covered}/{total_stmts} = {pct:.1f}%")
    print(f"Threshold: {args.threshold:.1f}%")

    if pct < args.threshold:
        print(
            f"FAIL: coverage {pct:.1f}% is below threshold {args.threshold:.1f}%.",
            file=sys.stderr,
        )
        return 1

    print("PASS")
    return 0


if __name__ == "__main__":
    sys.exit(main())
