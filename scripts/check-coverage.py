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

    scripts/check-coverage.py \\
        --diff-base origin/main --diff-threshold 85 \\
        --exclude '/db/' --repo-root . \\
        backend/patchcov.out [more.out ...]

Reads each profile, sums statement counts (excluding any line whose
file path contains an --exclude substring), and computes
covered/total. Prints a per-package breakdown for visibility.
Profiles must be in Go's standard `go test -coverprofile=` format.

With --diff-base the tool ALSO runs a patch-scoped (diff) gate: the
profiles' per-statement line ranges are intersected with the lines this
branch added/changed relative to the merge base of <base> and the WORK
TREE, and the gate fails when new-line coverage is below
--diff-threshold. Both gates are independent; with both requested the
exit code is non-zero if EITHER fails, and a git failure in the diff
path degrades to a printed SKIP so the aggregate gate still decides.
"""

import argparse
import os
import re
import shutil
import subprocess
import sys
from collections import defaultdict

# Default import-path prefix stripped from a coverage profile's file field to
# obtain a repo-relative path (Go writes `<import path>/<file>.go`, not a
# filesystem path).
DEFAULT_MODULE_PREFIX = "github.com/kuhlman-labs/fishhawk/"

# `@@ -a[,b] +c[,d] @@` — with --unified=0 the `+c,d` side is the added range.
HUNK_RE = re.compile(r"^@@ -\S+ \+(\d+)(?:,(\d+))? @@")

MAX_LISTED_UNCOVERED = 25


def skip(reason):
    """Print a one-line skip reason. Every degrade branch goes through here so
    a skip is never silent — a quiet gate is indistinguishable from a passing
    one, which is the exact failure mode this gate exists to remove."""
    print(f"SKIP: {reason} — patch-coverage gate skipped", file=sys.stderr)


def parse_profile(path, excludes):
    """Yield (pkg, file_path, num_stmts, count) for non-excluded lines."""
    for file_path, _, _, num_stmts, run_count in _parse_blocks(path, excludes):
        yield file_path.rsplit("/", 1)[0], file_path, num_stmts, run_count


def parse_blocks(path, excludes):
    """Yield (file_path, start_line, end_line, count) for non-excluded blocks."""
    for file_path, start, end, _, run_count in _parse_blocks(path, excludes):
        yield file_path, start, end, run_count


def _parse_blocks(path, excludes):
    """Yield (file_path, start_line, end_line, num_stmts, count) per profile block."""
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
                file_path, span = loc.split(":", 1)
                start_str, end_str = span.split(",", 1)
                start_line = int(start_str.split(".", 1)[0])
                end_line = int(end_str.split(".", 1)[0])
                num_stmts = int(numstmts)
                run_count = int(count)
            except (ValueError, IndexError) as e:
                raise SystemExit(f"{path}: bad line: {line!r}: {e}")
            if any(ex in file_path for ex in excludes):
                continue
            yield file_path, start_line, end_line, num_stmts, run_count


# --------------------------------------------------------------------------
# Diff (patch-scoped) mode
# --------------------------------------------------------------------------


class GitSkip(Exception):
    """A git-side degrade: the patch gate is skipped, never failed."""


def _git(repo_root, *args):
    """Run a git command, raising GitSkip with a one-line reason on any failure."""
    try:
        proc = subprocess.run(
            ["git", "-C", repo_root, *args],
            capture_output=True,
            text=True,
        )
    except FileNotFoundError:
        raise GitSkip("git not found on PATH")
    except OSError as e:
        raise GitSkip(f"could not run git ({e})")
    if proc.returncode != 0:
        raise GitSkip(f"git {' '.join(args)} failed (rc={proc.returncode})")
    return proc.stdout


def resolve_merge_base(repo_root, base):
    """Resolve <base> and return its merge base with HEAD.

    Raises GitSkip with a distinct reason per degrade branch: git absent,
    non-git/bare root, syntactically invalid base override, unresolvable base
    ref, or a resolved ref with no merge base.
    """
    if shutil.which("git") is None:
        raise GitSkip("git not found on PATH")

    # Non-git or BARE repo: `--is-inside-work-tree` prints `false` yet exits 0
    # inside a bare repo, so guard on the printed VALUE, not the exit status.
    try:
        inside = _git(repo_root, "rev-parse", "--is-inside-work-tree").strip()
    except GitSkip:
        raise GitSkip(f"{repo_root} is not a git work tree")
    if inside != "true":
        raise GitSkip(f"{repo_root} is not a git work tree")

    # Reject a syntactically bogus override before handing it to git, so an
    # operator typo reads as an invalid override rather than an opaque git error.
    if not base or base.startswith("-") or any(c.isspace() for c in base):
        raise GitSkip(f"invalid --diff-base value {base!r}")

    try:
        _git(repo_root, "rev-parse", "--verify", "--quiet", f"{base}^{{commit}}")
    except GitSkip:
        raise GitSkip(f"base ref {base!r} does not resolve")

    try:
        out = _git(repo_root, "merge-base", base, "HEAD").strip()
    except GitSkip:
        raise GitSkip(f"no merge base between {base!r} and HEAD")
    if not out:
        raise GitSkip(f"no merge base between {base!r} and HEAD")
    return out


def changed_lines(repo_root, merge_base):
    """Return {repo_relative_path: set(added_line_numbers)}.

    Diffs the merge base against the WORK TREE (no second revision), NOT
    against HEAD. Coverage profiles reflect the tree `go test` compiled — the
    work tree — so diffing the work tree keeps line attribution and coverage
    on the same snapshot; diffing HEAD would skew on a dirty tree. Using the
    merge base (rather than the base tip) keeps commits that landed on the base
    branch after this branch forked out of the patch denominator, exactly as
    `git diff base...HEAD` would.
    """
    out = _git(
        repo_root,
        "diff",
        "--unified=0",
        "--no-color",
        "--diff-filter=ACMR",
        merge_base,
        "--",
        "*.go",
    )
    changed = defaultdict(set)
    current = None
    for line in out.splitlines():
        if line.startswith("+++ "):
            target = line[4:].strip()
            if target == "/dev/null":
                current = None  # deleted file — nothing added
            elif target.startswith("b/"):
                current = target[2:]
            else:
                current = target
            continue
        if line.startswith("@@"):
            m = HUNK_RE.match(line)
            if not m or current is None:
                continue
            start = int(m.group(1))
            count = 1 if m.group(2) is None else int(m.group(2))
            # count == 0 is a pure-deletion hunk: it adds nothing.
            for i in range(count):
                changed[current].add(start + i)
    return {k: v for k, v in changed.items() if v}


def map_profile_path(file_path, module_prefix, changed):
    """Map a profile's import-path-based file field to a changed repo-relative path."""
    if module_prefix and file_path.startswith(module_prefix):
        rel = file_path[len(module_prefix):]
        if rel in changed:
            return rel
    if file_path in changed:
        return file_path
    for key in changed:
        if file_path.endswith("/" + key):
            return key
    return None


def diff_coverage(profiles, excludes, changed, module_prefix):
    """Intersect profile blocks with added lines.

    Returns (per_file_covered, per_file_total) keyed by repo-relative path.
    A line is counted only when it falls inside at least one profile block, so
    added comments/blanks/imports/bare braces carry no statement and are ignored
    rather than counted uncovered. Blocks overlap at nested-statement
    boundaries, so a covered block wins over an uncovered block spanning the
    same line — the optimistic union can slightly over-report but can never
    manufacture a false failure, the conservative choice for an in-loop gate.
    """
    covered = defaultdict(set)
    total = defaultdict(set)
    for path in profiles:
        for file_path, start, end, count in parse_blocks(path, excludes):
            rel = map_profile_path(file_path, module_prefix, changed)
            if rel is None:
                continue
            hit = {ln for ln in changed[rel] if start <= ln <= end}
            if not hit:
                continue
            total[rel] |= hit
            if count > 0:
                covered[rel] |= hit
    return covered, total


def run_diff_gate(args):
    """Run the patch-scoped gate. Returns 0 (pass or skip) or 1 (fail)."""
    try:
        merge_base = resolve_merge_base(args.repo_root, args.diff_base)
        changed = changed_lines(args.repo_root, merge_base)
    except GitSkip as e:
        skip(str(e))
        return 0

    if not changed:
        skip(f"no changed Go files vs {args.diff_base}")
        return 0

    covered, total = diff_coverage(
        args.profile, args.exclude, changed, args.module_prefix
    )

    total_new = sum(len(v) for v in total.values())
    total_cov = sum(len(v) for v in covered.values())

    if total_new == 0:
        skip("no coverable new Go statements in the diff")
        return 0
    if total_new < args.diff_min_statements:
        skip(
            f"only {total_new} new statements "
            f"(< --diff-min-statements {args.diff_min_statements})"
        )
        return 0

    print()
    print("Patch coverage by file (new/changed lines carrying statements):")
    width = max(len(p) for p in total) + 2
    for rel in sorted(total):
        t = len(total[rel])
        c = len(covered.get(rel, ()))
        pct = 100.0 * c / t if t else 0.0
        print(f"  {rel:<{width}}  {c:>5}/{t:<5}  {pct:5.1f}%")

    pct = 100.0 * total_cov / total_new
    print()
    print(f"Patch coverage: {total_cov}/{total_new} = {pct:.1f}% (base {args.diff_base})")
    print(f"Diff threshold: {args.diff_threshold:.1f}%")

    if pct < args.diff_threshold:
        misses = []
        for rel in sorted(total):
            for ln in sorted(total[rel] - covered.get(rel, set())):
                misses.append(f"{rel}:{ln}")
        print(
            f"FAIL: patch coverage {pct:.1f}% is below diff threshold "
            f"{args.diff_threshold:.1f}%. Uncovered new lines:",
            file=sys.stderr,
        )
        for loc in misses[:MAX_LISTED_UNCOVERED]:
            print(f"  {loc}", file=sys.stderr)
        if len(misses) > MAX_LISTED_UNCOVERED:
            print(f"  ... and {len(misses) - MAX_LISTED_UNCOVERED} more", file=sys.stderr)
        return 1

    print("PATCH PASS")
    return 0


# --------------------------------------------------------------------------
# Aggregate mode (unchanged behavior)
# --------------------------------------------------------------------------


def run_aggregate_gate(args):
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


def main():
    ap = argparse.ArgumentParser(description=__doc__.strip().split("\n", 1)[0])
    ap.add_argument(
        "--threshold",
        type=float,
        default=None,
        help="Minimum aggregate coverage percentage (e.g. 80). Required unless --diff-base is given.",
    )
    ap.add_argument(
        "--exclude",
        action="append",
        default=[],
        metavar="SUBSTRING",
        help="Substring of a file path; profiles entries matching it are dropped before counting. May be repeated.",
    )
    ap.add_argument(
        "--diff-base",
        default=None,
        metavar="REF",
        help="Enable the patch-scoped gate against this base ref (e.g. origin/main).",
    )
    ap.add_argument(
        "--diff-threshold",
        type=float,
        default=85.0,
        help="Minimum coverage percentage of new/changed lines (default 85).",
    )
    ap.add_argument(
        "--diff-min-statements",
        type=int,
        default=5,
        help="Below this many coverable new statements the diff gate skips (default 5).",
    )
    ap.add_argument(
        "--repo-root",
        default=os.path.dirname(os.path.dirname(os.path.abspath(__file__))),
        metavar="DIR",
        help="Repository root used as `git -C` for the diff gate.",
    )
    ap.add_argument(
        "--module-prefix",
        default=DEFAULT_MODULE_PREFIX,
        help="Import-path prefix stripped to map a profile path to a repo-relative path.",
    )
    ap.add_argument("profile", nargs="+", help="One or more Go coverage profiles.")
    args = ap.parse_args()

    if args.threshold is None and args.diff_base is None:
        ap.error("one of --threshold or --diff-base is required")

    rc = 0
    if args.threshold is not None:
        rc = run_aggregate_gate(args)
    if args.diff_base is not None:
        # Combined mode: the diff gate never masks the aggregate verdict. A git
        # failure here degrades to a printed SKIP, so the aggregate result above
        # still decides the exit code.
        diff_rc = run_diff_gate(args)
        if diff_rc != 0 and rc == 0:
            rc = diff_rc
    return rc


if __name__ == "__main__":
    sys.exit(main())
