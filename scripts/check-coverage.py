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
TREE (plus every line of each UNTRACKED .go file, which `git diff` cannot
see but `go test` compiles), and the gate fails when new-line coverage is below
--diff-threshold. Both gates are independent; with both requested the
exit code is non-zero if EITHER fails, and a git failure in the diff
path degrades to a printed SKIP so the aggregate gate still decides.

Path handling in the diff path is binary-safe: every git path
enumeration is NUL-delimited (-z), and the unified diff's `+++` headers
are decoded out of git's C-quoted form and cross-checked against that
NUL-enumerated set. A path that still cannot be identified FAILS the
patch gate with a printed reason naming it — never a silent omission,
which would let Go code hide from the gate behind its filename.

TOCTOU-safe changed-path discovery (#2124). The diff above reads the WORK
TREE, which the repository-controlled test loop mutates while it runs. If
changed-path discovery is recomputed AFTER the tests execute, a test that
reverts a changed tracked .go file to merge-base contents (or deletes an
untracked .go file) erases its own lines from the denominator and turns the
gate into a passing SKIP. So `--emit-changed-snapshot PATH` captures the
merge-base-resolved change set to JSON BEFORE any test runs, and
`--changed-snapshot PATH` consumes that snapshot instead of re-running git.
`--expected-snapshot-digest HEX` is verified against the on-disk snapshot
before it is consumed: the digest is computed by scripts/test into the parent
shell's memory pre-test and never written to a test-reachable path, so a test
that rewrites the snapshot during the loop produces a digest mismatch that
FAILS CLOSED. Missing, unreadable, malformed, and digest-mismatched snapshots
all exit 1 — never a skip. The recompute path survives only as
backward-compatible behavior when no snapshot is supplied.
"""

import argparse
import hashlib
import json
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
    # surrogateescape: a profile's file field is a Go import path plus the real
    # file name, which need not be valid UTF-8. Decoding strictly would raise
    # rather than gate the file.
    with open(path, errors="surrogateescape") as f:
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


class PathDecodeError(Exception):
    """A path the gate cannot confidently identify. FAIL CLOSED, never skip.

    Silently dropping such a path would let executable Go code hide from the
    denominator behind its filename — the blind-gate bypass this gate exists to
    close. So this is deliberately NOT a GitSkip: it fails the patch gate loudly
    and names the path.
    """


class SnapshotError(Exception):
    """A pre-test changed-line snapshot that cannot be trusted. FAIL CLOSED.

    Raised when a consumed snapshot is missing, unreadable, malformed, or its
    on-disk bytes do not match the digest captured before the test loop ran.
    Deliberately NOT a GitSkip: a tampered or corrupt snapshot must fail the
    gate, never resolve to a passing skip (#2124).
    """


# git's C-style quoting (`quote_c_style`) for a path that cannot be printed
# raw. Only these single-character escapes are ever emitted; anything else is
# an octal byte escape.
_C_ESCAPES = {
    "a": 0x07,
    "b": 0x08,
    "f": 0x0C,
    "n": 0x0A,
    "r": 0x0D,
    "t": 0x09,
    "v": 0x0B,
    "\\": 0x5C,
    '"': 0x22,
}

_OCTAL_DIGITS = "01234567"


def decode_git_path(raw):
    """Decode git's C-style quoted path form back to the real path.

    git quotes a path containing a double quote, a backslash, a control
    character (including a NEWLINE), or — unless core.quotePath=false — a
    non-ASCII byte, wrapping it in double quotes and escaping the offending
    bytes. An unquoted value is returned verbatim. Anything that does not decode
    cleanly raises PathDecodeError rather than being pattern-matched and hoped
    at.
    """
    if not (len(raw) >= 2 and raw.startswith('"') and raw.endswith('"')):
        return raw
    body = raw[1:-1]
    out = bytearray()
    i = 0
    n = len(body)
    while i < n:
        ch = body[i]
        if ch != "\\":
            out.extend(ch.encode("utf-8", "surrogateescape"))
            i += 1
            continue
        i += 1
        if i >= n:
            raise PathDecodeError(f"trailing backslash in quoted git path {raw!r}")
        esc = body[i]
        if esc in _OCTAL_DIGITS:
            digits = body[i : i + 3]
            if len(digits) != 3 or any(d not in _OCTAL_DIGITS for d in digits):
                raise PathDecodeError(f"bad octal escape in quoted git path {raw!r}")
            out.append(int(digits, 8))
            i += 3
            continue
        if esc not in _C_ESCAPES:
            raise PathDecodeError(
                f"unknown escape \\{esc} in quoted git path {raw!r}"
            )
        out.append(_C_ESCAPES[esc])
        i += 1
    return out.decode("utf-8", "surrogateescape")


def _git(repo_root, *args):
    """Run a git command, raising GitSkip with a one-line reason on any failure."""
    try:
        proc = subprocess.run(
            ["git", "-C", repo_root, *args],
            capture_output=True,
            # surrogateescape, not strict UTF-8: a repository may legally carry
            # a path whose bytes are not valid UTF-8, and a decode error here
            # would crash the gate instead of gating the file.
            encoding="utf-8",
            errors="surrogateescape",
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


def untracked_lines(repo_root):
    """Return {repo_relative_path: set(all_line_numbers)} for untracked .go files.

    `git diff <merge_base>` sees only TRACKED files, so a brand-new .go file
    that was never `git add`ed is invisible to it — yet `go test` compiles it,
    so its statements land in the profile. Every line of such a file is new, so
    the whole file is folded into the patch denominator. This is what makes the
    work-tree contract hold for an uncommitted NEW file, not just for edits to
    tracked ones. Ignored files are excluded (`--exclude-standard`).
    """
    out = _git(
        repo_root,
        "ls-files",
        "--others",
        "--exclude-standard",
        "-z",
        "--",
        "*.go",
    )
    new = {}
    for rel in out.split("\0"):
        if not rel or rel.endswith("_test.go"):
            continue
        try:
            with open(os.path.join(repo_root, rel), "rb") as f:
                n = sum(1 for _ in f)
        except OSError as e:
            # Raced away or unreadable — nothing to attribute. Say so on one
            # line: a path dropped from the denominator is never silent.
            print(
                f"WARN: untracked {rel!r} is unreadable ({e}); "
                "not folded into the patch denominator",
                file=sys.stderr,
            )
            continue
        if n:
            new[rel] = set(range(1, n + 1))
    return new


def tracked_changed_paths(repo_root, merge_base):
    """Return the AUTHORITATIVE set of changed tracked .go paths.

    NUL-delimited (`-z`), so a path is never split by a newline inside a
    filename and never arrives in git's C-quoted form. This set — not the
    unified diff's `+++` headers — decides WHICH files changed; the headers only
    supply line numbers, and every one of them must resolve back into this set
    (see parse_unified_diff).
    """
    out = _git(
        repo_root,
        "diff",
        "--name-only",
        "-z",
        "--diff-filter=ACMR",
        merge_base,
        "--",
        "*.go",
    )
    return {p for p in out.split("\0") if p}


def parse_unified_diff(text, authority):
    """Return {path: set(added_line_numbers)} from a --unified=0 diff.

    `authority` is the NUL-delimited changed-path set (tracked_changed_paths);
    it decides which files count. A `+++` header is decoded out of git's
    C-quoted form (decode_git_path) and must resolve back into `authority` — a
    header that does not raises PathDecodeError, so a path the parser cannot
    confidently identify FAILS the gate loudly instead of being dropped from the
    denominator.

    Header lines are recognised only between a `diff --git` line and that file's
    first hunk. Inside hunk bodies an ADDED source line that itself begins with
    `++ ` renders as `+++ …`, which a naive prefix match would mistake for a
    file header; the state machine makes that structurally impossible.
    """
    changed = defaultdict(set)
    current = None
    in_header = False
    for line in text.splitlines():
        if line.startswith("diff --git "):
            in_header = True
            current = None
            continue
        if in_header and line.startswith("+++ "):
            raw = line[4:]
            if raw == "/dev/null":
                current = None  # deleted file — nothing added
                continue
            # git appends a TAB after a header path containing a space so
            # patch(1) can find the name boundary; a path that really ends in a
            # tab arrives quoted instead. Both readings are offered to the
            # authority cross-check below, which decides — no guessing.
            forms = [raw]
            if raw.endswith("\t"):
                forms.append(raw[:-1])
            candidates = []
            for form in forms:
                decoded = decode_git_path(form)
                if decoded.startswith("b/"):
                    candidates.append(decoded[2:])
                candidates.append(decoded)
            current = next((c for c in candidates if c in authority), None)
            if current is None:
                raise PathDecodeError(
                    f"diff header path {raw!r} does not match any "
                    "NUL-enumerated changed path"
                )
            continue
        if line.startswith("@@"):
            in_header = False
            m = HUNK_RE.match(line)
            if not m or current is None:
                continue
            start = int(m.group(1))
            count = 1 if m.group(2) is None else int(m.group(2))
            # count == 0 is a pure-deletion hunk: it adds nothing.
            for i in range(count):
                changed[current].add(start + i)
    return {k: v for k, v in changed.items() if v}


def changed_lines(repo_root, merge_base):
    """Return {repo_relative_path: set(added_line_numbers)}.

    Diffs the merge base against the WORK TREE (no second revision), NOT
    against HEAD. Coverage profiles reflect the tree `go test` compiled — the
    work tree — so diffing the work tree keeps line attribution and coverage
    on the same snapshot; diffing HEAD would skew on a dirty tree. Using the
    merge base (rather than the base tip) keeps commits that landed on the base
    branch after this branch forked out of the patch denominator, exactly as
    `git diff base...HEAD` would. UNTRACKED .go files are added on top (see
    untracked_lines) — `git diff` cannot see them, but the compiler can.

    Path handling is binary-safe end to end: both enumerations are NUL-delimited
    and the unified diff's headers are decoded and cross-checked against the
    NUL-enumerated set, so no .go file can hide from the gate behind its name.
    """
    authority = tracked_changed_paths(repo_root, merge_base)
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
    result = parse_unified_diff(out, authority)
    for rel, lines in untracked_lines(repo_root).items():
        result.setdefault(rel, set()).update(lines)
    return result


# --------------------------------------------------------------------------
# Pre-test change-set snapshot (#2124)
#
# The change set is a mutable read of the WORK TREE. Captured here BEFORE any
# repository-controlled test runs, serialized to JSON, and consumed at gate time
# instead of being recomputed after the tests could have mutated the tree.
# --------------------------------------------------------------------------

SNAPSHOT_SCHEMA = 1


def emit_changed_snapshot(path, base, merge_base, changed):
    """Serialize the change set to JSON at `path`.

    Binary-safe: a path key may be a surrogateescape-decoded (non-UTF-8) string,
    so we dump with ensure_ascii=True — every non-ASCII byte, including a lone
    surrogate, is \\uXXXX-escaped and round-trips through json.loads back to the
    identical key. The output is therefore pure ASCII.
    """
    payload = {
        "schema": SNAPSHOT_SCHEMA,
        "base": base,
        "merge_base": merge_base,
        "changed": {p: sorted(lines) for p, lines in changed.items()},
    }
    data = json.dumps(payload, ensure_ascii=True)
    with open(path, "w", encoding="ascii") as f:
        f.write(data)


def emit_skip_snapshot(path, reason):
    """Write a snapshot that instructs the consumer to SKIP.

    A degrade at emit time (git absent, no merge base, …) writes this rather
    than nothing, so the consume side skips cleanly on a trusted marker instead
    of falling back to the very recompute this snapshot exists to avoid.
    """
    payload = {"schema": SNAPSHOT_SCHEMA, "skip": str(reason)}
    with open(path, "w", encoding="ascii") as f:
        f.write(json.dumps(payload, ensure_ascii=True))


def load_verified_snapshot(path, expected_digest):
    """Load a snapshot after verifying its integrity. FAIL CLOSED on any doubt.

    Returns {"skip": reason} for a skip-snapshot, or {"changed": map, "base":
    label}. Raises SnapshotError when the digest is absent/mismatched, the
    content is unreadable/unparseable, or the structure is malformed (a
    non-string/ambiguous skip, or a line value that is not a positive integer) —
    every one of which exits 1, never a skip.

    The digest is the anchor: scripts/test computes it from the pristine
    pre-test snapshot into the parent shell's memory and passes it here, so a
    test that rewrites the file mid-loop changes its bytes and is DETECTED. The
    bytes are read ONCE and both hashed and parsed, so no read-vs-hash window
    exists inside this function.
    """
    if not expected_digest:
        # The shell always passes the digest; its absence means the anchor was
        # lost, so the snapshot cannot be trusted — refuse rather than consume.
        raise SnapshotError(
            "no expected snapshot digest supplied; refusing to consume snapshot"
        )
    try:
        with open(path, "rb") as f:
            raw = f.read()
    except OSError as e:
        raise SnapshotError(f"cannot read changed snapshot {path!r}: {e}")
    actual = hashlib.sha256(raw).hexdigest()
    if actual != expected_digest.strip():
        raise SnapshotError(
            f"changed-snapshot digest mismatch for {path!r}: "
            f"expected {expected_digest.strip()}, got {actual} — the snapshot was "
            "modified after it was captured; failing closed"
        )
    try:
        payload = json.loads(raw.decode("ascii"))
    except (ValueError, UnicodeDecodeError) as e:
        raise SnapshotError(f"malformed changed snapshot {path!r}: {e}")
    if not isinstance(payload, dict) or payload.get("schema") != SNAPSHOT_SCHEMA:
        raise SnapshotError(f"unrecognized changed-snapshot shape in {path!r}")
    if "skip" in payload:
        # A skip-snapshot carries a STRING reason and nothing that would make
        # the verdict ambiguous. A non-string skip (`{"skip": null}`) or a
        # payload that ALSO carries a `changed` map is malformed: fail closed
        # rather than stringify-and-skip, or a tamperer could smuggle a skip in
        # alongside real changed lines (#2124).
        if "changed" in payload:
            raise SnapshotError(
                f"changed-snapshot {path!r} has both 'skip' and 'changed'"
            )
        reason = payload["skip"]
        if not isinstance(reason, str):
            raise SnapshotError(
                f"changed-snapshot {path!r} 'skip' is not a string"
            )
        return {"skip": reason}
    changed_raw = payload.get("changed")
    if not isinstance(changed_raw, dict):
        raise SnapshotError(f"changed-snapshot {path!r} has no 'changed' map")
    changed = {}
    for p, lines in changed_raw.items():
        if not isinstance(lines, list):
            raise SnapshotError(
                f"changed-snapshot {path!r} entry {p!r} is not a line list"
            )
        nums = set()
        for ln in lines:
            # A line number is a POSITIVE integer. Do NOT int()-coerce: that
            # silently accepts a bool (True→1 — and bool IS an int subclass, so
            # reject it explicitly), a numeric string ("5"), a float (5.0, 5.7
            # truncating), zero, and negatives — every one of which is a
            # malformed snapshot that must fail closed, not be normalized.
            if isinstance(ln, bool) or not isinstance(ln, int) or ln < 1:
                raise SnapshotError(
                    f"changed-snapshot {path!r} entry {p!r} has a "
                    f"non-positive-integer line {ln!r}"
                )
            nums.add(ln)
        changed[p] = nums
    return {"changed": changed, "base": payload.get("base")}


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


def run_emit_snapshot(args):
    """Capture the pre-test change set to --emit-changed-snapshot. Returns 0/1.

    Runs ONLY resolve_merge_base + changed_lines then serializes, short-circuiting
    before any coverage gate — it is invoked before the test loop starts. A git
    degrade writes a skip-snapshot and returns 0 so the consumer skips on a
    trusted marker; an undecodable changed path FAILS CLOSED (exit 1) here, moved
    ahead of the tests rather than after them.
    """
    if args.diff_base is None:
        print(
            "FAIL: --emit-changed-snapshot requires --diff-base", file=sys.stderr
        )
        return 1
    try:
        merge_base = resolve_merge_base(args.repo_root, args.diff_base)
        changed = changed_lines(args.repo_root, merge_base)
    except GitSkip as e:
        # Degrade to a trusted skip marker rather than to a post-test recompute.
        emit_skip_snapshot(args.emit_changed_snapshot, str(e))
        skip(str(e))
        return 0
    except PathDecodeError as e:
        print(f"FAIL: undecodable changed path — {e}", file=sys.stderr)
        return 1
    emit_changed_snapshot(
        args.emit_changed_snapshot, args.diff_base, merge_base, changed
    )
    return 0


def run_diff_gate(args):
    """Run the patch-scoped gate. Returns 0 (pass or skip) or 1 (fail)."""
    if args.changed_snapshot is not None:
        # Consume the pre-test snapshot instead of re-reading the (now
        # test-mutated) work tree. Integrity is anchored by the digest; any
        # tampering, corruption, or loss FAILS CLOSED here — never a skip.
        try:
            loaded = load_verified_snapshot(
                args.changed_snapshot, args.expected_snapshot_digest
            )
        except SnapshotError as e:
            print(f"FAIL: {e}", file=sys.stderr)
            return 1
        if "skip" in loaded:
            skip(loaded["skip"])
            return 0
        changed = loaded["changed"]
        base_label = loaded["base"] or args.diff_base
    else:
        # Backward-compatible recompute path (no snapshot supplied).
        try:
            merge_base = resolve_merge_base(args.repo_root, args.diff_base)
            changed = changed_lines(args.repo_root, merge_base)
        except GitSkip as e:
            skip(str(e))
            return 0
        except PathDecodeError as e:
            # FAIL CLOSED, not skip: a path the gate cannot identify must never
            # be treated as "nothing to cover".
            print(f"FAIL: undecodable changed path — {e}", file=sys.stderr)
            return 1
        base_label = args.diff_base

    if not changed:
        skip(f"no changed Go files vs {base_label}")
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
    print(f"Patch coverage: {total_cov}/{total_new} = {pct:.1f}% (base {base_label})")
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
    ap.add_argument(
        "--emit-changed-snapshot",
        default=None,
        metavar="PATH",
        help="Capture the pre-test change set (resolved against --diff-base) to PATH "
        "as JSON and exit, before any test runs. No profiles are needed.",
    )
    ap.add_argument(
        "--changed-snapshot",
        default=None,
        metavar="PATH",
        help="Consume the change set from a snapshot written by --emit-changed-snapshot "
        "instead of recomputing it from the (test-mutated) work tree.",
    )
    ap.add_argument(
        "--expected-snapshot-digest",
        default=None,
        metavar="HEX",
        help="sha256 of the pristine snapshot, captured pre-test. Verified against the "
        "on-disk --changed-snapshot before consuming; a mismatch fails closed.",
    )
    # nargs="*" (was "+"): emit mode needs no profiles. A zero-profile call in
    # any gate mode is still rejected explicitly below, so the guard is not lost.
    ap.add_argument("profile", nargs="*", help="Zero or more Go coverage profiles.")
    args = ap.parse_args()

    # Emit mode short-circuits: capture the snapshot and return before any gate.
    if args.emit_changed_snapshot is not None:
        return run_emit_snapshot(args)

    if not args.profile:
        ap.error("at least one coverage profile is required")

    if (
        args.threshold is None
        and args.diff_base is None
        and args.changed_snapshot is None
    ):
        ap.error("one of --threshold or --diff-base is required")

    rc = 0
    if args.threshold is not None:
        rc = run_aggregate_gate(args)
    if args.diff_base is not None or args.changed_snapshot is not None:
        # Combined mode: the diff gate never masks the aggregate verdict. A git
        # failure here degrades to a printed SKIP, so the aggregate result above
        # still decides the exit code.
        diff_rc = run_diff_gate(args)
        if diff_rc != 0 and rc == 0:
            rc = diff_rc
    return rc


if __name__ == "__main__":
    sys.exit(main())
