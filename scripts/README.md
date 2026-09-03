# scripts

Operator/dev tooling. `scripts/dev` and `scripts/test` carry their core
contracts in `AGENTS.md`; this file holds the relocated detail entries.

## Patch-scoped coverage gate (ADR-059 / [#1887](https://github.com/kuhlman-labs/fishhawk/issues/1887))

`scripts/check-coverage.py --diff-base <ref>` plus the `cmd_verify` wiring
in `scripts/test`. The aggregate ≥ 80% gate runs only in CI, only after
the implement agent is terminal, and a new 0%-covered function barely
moves it — so `scripts/test verify` gates the DIFF instead, in-loop
(#1064).

### What the Python side does

- `resolve_merge_base(root, base)` → `git merge-base <base> HEAD`.
- `changed_lines(root, merge_base)` → `git diff --unified=0 --no-color
  --diff-filter=ACMR <merge_base> -- '*.go'`, parsed from the `+++ b/…`
  file headers and `@@ -a,b +c,d @@` hunk headers into
  `{repo_relative_path: set(added_lines)}`. An omitted `d` means one
  line; `d == 0` is a pure-deletion hunk and contributes nothing;
  `+++ /dev/null` (deleted file) is skipped.
- `diff_coverage(...)` intersects those lines with each profile block
  `<file>:<start>.<c>,<end>.<c> <n> <count>`. A line counts only when it
  falls inside at least one block, so added comments, blanks, imports and
  bare braces carry no statement and are IGNORED rather than counted
  uncovered; `_test.go` files are never instrumented and drop out. Blocks
  overlap at nested-statement boundaries, so a covered block wins over an
  uncovered block spanning the same line — the optimistic union can
  slightly over-report but can never manufacture a false failure, the
  conservative choice for an in-loop gate.
- A profile's file field is an import path, not a filesystem path, so it
  is mapped to a repo-relative path by stripping `--module-prefix`
  (default `github.com/kuhlman-labs/fishhawk/`), falling back to a
  suffix match against the changed-file keys.
- On failure it prints `path:line` for the first 25 uncovered new lines
  (then `… and K more`) and exits 1.

### Committed-tree assumption — resolved by diffing the WORK TREE

Coverage profiles reflect the tree `go test` compiled. Diffing HEAD
would therefore skew line attribution on a dirty tree. Both layers
instead diff the merge base against the **work tree** (`git diff
<merge_base>`, no second revision), so attribution and coverage are
always taken from the same snapshot: an uncommitted edit to a tracked
file is gated rather than mis-attributed. Using the merge base (not the
base tip) keeps commits that landed on the base branch after this branch
forked out of the patch denominator, exactly as `base...HEAD` would.
The real guarantee is therefore: *no skew between the diff and the
profile*, not *no failure is ever spurious* — a genuinely uncovered new
line fails, which is the point.

`git diff` sees only TRACKED files, so the work-tree diff alone would
miss a brand-new `.go` file that was never `git add`ed — the compiler
sees it, the diff does not, and if it were the only Go change the gate
would be bypassed entirely. Both layers therefore ALSO enumerate
untracked Go files (`git ls-files --others --exclude-standard`): the
shell buckets their packages, and `untracked_lines()` folds every line
of each such file into the denominator (all of it is new). Ignored files
are excluded, and `_test.go` files are skipped as everywhere else. The
untracked enumeration is additive — if it fails, the shell degrades to
the tracked-only list rather than dropping the gate. This is moot on the
runner's committed tree, where every file is committed; it closes the
local dirty-tree hole.

### Pre-test change-set snapshot — TOCTOU ([#2124](https://github.com/kuhlman-labs/fishhawk/issues/2124))

The work-tree diff above is a read of MUTABLE state, and the test loop
it gates runs repository-controlled code. If changed-path discovery is
recomputed AFTER the tests execute, a test can erase its own lines from
the denominator: reverting a changed tracked `.go` file to merge-base
contents (or deleting an untracked `.go` file) makes the recompute see
no diff and the gate resolves to a passing SKIP — the exact bypass this
epic exists to close.

So the change set is captured ONCE, BEFORE any test runs, and consumed
at gate time instead of being recomputed:

- `check-coverage.py --emit-changed-snapshot PATH --diff-base <ref>` runs
  only `resolve_merge_base` + `changed_lines` and serializes
  `{"schema":1,"base","merge_base","changed":{path:[lines]}}` to PATH,
  binary-safe (ASCII-escaped so a surrogateescape-decoded non-UTF-8 path
  key round-trips). A git degrade writes a **skip-snapshot**
  (`{"schema":1,"skip":<reason>}`) and exits 0; an undecodable changed
  path FAILS CLOSED (exit 1) here — the `PathDecodeError` guard is thus
  moved AHEAD of the tests, not left after them.
- `check-coverage.py --changed-snapshot PATH --expected-snapshot-digest
  HEX` loads the snapshot instead of re-running git. A skip-snapshot
  skips (exit 0); a change map is used verbatim.

`cmd_test_with_patch_coverage` emits the snapshot into the PID-keyed
`PATCHCOV_DIR` and `_verify_patch_coverage` consumes it. The recompute
path survives only as backward-compatible behavior when no
`--changed-snapshot` is supplied.

#### Tamper-evidence, not prevention — the digest anchor

A snapshot written to a filesystem path the test loop can reach does not
by itself close the TOCTOU: the tests run as the SAME OS user and can
enumerate and overwrite the `0700` `PATCHCOV_DIR` (they even receive its
path via `-coverprofile=<PATCHCOV_DIR>/…`), so a test could replace the
snapshot with a skip-snapshot or a changed-line map that drops its file.
Filesystem permissions do not protect the snapshot from same-user
tampering.

The integrity anchor is therefore held OUT of any test-reachable place —
in the parent `scripts/test` shell's own memory. The ordering is
load-bearing and is exactly: **emit → hash-into-parent-memory → run
tests → verify-digest → consume**. After emitting, the parent shell
computes `sha256` of the pristine snapshot into `PATCHCOV_SNAPSHOT_DIGEST`
(`_sha256_file`, via `sha256sum`/`shasum`), a variable NEVER written to a
test-reachable path and NEVER exported into a test subprocess. At consume
time that digest is passed to `check-coverage.py`, which re-hashes the
on-disk snapshot and consumes it only on an exact match. Any mismatch —
i.e. any rewrite of the file after capture — **fails closed** with a
named reason, never a skip. A test can still WRITE the file; what it
cannot do is forge the digest in the parent's memory or make the mismatch
resolve to a skip.

This makes tampering tamper-EVIDENT (detected, fail-closed), **not
impossible**. FULL prevention requires process/filesystem isolation of
the untrusted test loop — the untrusted-command containment decision
tracked in **[ADR-063](https://github.com/kuhlman-labs/fishhawk/issues/2127)**.
Until that lands, the patch-coverage gate is a quality aid, not an
adversary-proof control.

Fail-closed discipline is uniform (binding condition 4): a missing,
unreadable, malformed, or digest-mismatched snapshot, an absent expected
digest, and an undecodable path at emit all exit 1 — never a skip. A gate
that silently skips is indistinguishable from a passing gate, the exact
failure this epic removes.

### Binary-safe path handling

Repository contents are untrusted input, so a filename must never be
able to hide executable Go code from the gate. Unless NUL-delimited
output is requested, git presents a path containing a double quote, a
backslash, a control character (including a NEWLINE) or a non-ASCII byte
in C-style quoted form — and a newline inside a name splits one path
into two under newline-delimited parsing. Both are silent-omission
bypasses. So:

- **Every git path enumeration uses `-z` and is split on NUL**, in both
  layers: the changed-file list, the untracked-file list, and (Python)
  `tracked_changed_paths()`.
- **The unified diff is not the authority for WHICH files changed.**
  `tracked_changed_paths()` (NUL-delimited) is; the `--unified=0` diff
  only supplies line numbers. Each `+++` header is decoded out of git's
  C-quoted form by `decode_git_path()` (leading/trailing quote,
  single-character escapes, octal byte escapes, `surrogateescape` for
  non-UTF-8 bytes) and must resolve back into that authoritative set.
  Header recognition is state-machine bounded to the span between a
  `diff --git` line and that file's first hunk, so an added source line
  beginning with `++ ` — which renders as `+++ …` — cannot be mistaken
  for a header.
- **A path that still cannot be identified FAILS CLOSED.** Python raises
  `PathDecodeError` (deliberately not a `GitSkip`), and the gate exits 1
  with a printed reason naming the path. It is never dropped from the
  denominator, because a path the gate cannot identify must never read
  as "nothing to cover".
- **The shell layer cannot carry NUL through command substitution**, so
  it remaps the `-z` stream injectively with `tr '\n\000' '\001\n'`:
  record-separator NUL becomes newline, and a newline (which at that
  point can only be *inside* a filename) becomes `\001`. A record still
  containing `\001` is a path this layer cannot name, so it prints a
  one-line reason and falls back to `_patch_cov_all_modules` —
  instrumenting EVERY module rather than dropping the file. The same
  fail-closed widen covers a path containing a TAB or a COMMA, the two
  delimiters this layer's OWN output encoding uses (`<module>\t<pkg>`
  pairs, parsed by `awk -F'\t'`; comma-joined `-coverpkg` patterns,
  which `go test` splits on). Either character would emit a truncated or
  split pattern, leaving that package tested but UN-instrumented — its
  lines then fall inside no profile block and the Python denominator
  rule drops the file behind a misleading "no coverable new Go
  statements" skip, which is the same silent de-instrumentation the
  newline case is. That costs
  more, but keeps the file inside the reach of the Python layer's
  binary-safe denominator. This is the one shell-side branch that is
  fail-CLOSED in effect while still fail-open in form: it never aborts
  verify, it only widens instrumentation.

### Base-ref resolution ladder (`_patch_cov_base`)

`FISHHAWK_DIFF_BASE` if set (and it must resolve — an unresolvable
override is NOT silently replaced by a fallback), else the first of
`origin/main`, `main` that `git rev-parse --verify --quiet` resolves.
Non-git root, bare repo, absent git binary, or nothing resolving all
return non-zero.

### One test loop, not two (`cmd_test_with_patch_coverage`)

Verify runs the same per-module `go test -race -p "$TEST_P" ./...` loop
it always did. A module owning changed packages additionally gets
`-covermode=atomic -coverprofile=… -coverpkg=<its changed packages>`.
Restricting `-coverpkg` (rather than accepting Go's default per-package
attribution) is load-bearing: the module's FULL test set then credits
the changed packages, so a function exercised only by a SIBLING
package's test is not reported as uncovered — while instrumentation
cost is paid only for changed code and no second test run happens.

Profiles are written to a scratch dir created by `mktemp -d
"${TMPDIR:-/tmp}/fishhawk-patchcov-$$.XXXXXX"` — PID-keyed for
provenance exactly as the container-lease files are, and OUTSIDE the
repo. `mktemp -d` creates ATOMICALLY at mode 0700 and fails rather than
reusing an existing path; it is deliberately NOT `rm -rf` then `mkdir`,
which would both destroy a pre-created path and open a remove→create
window in which the scratch dir (or a symlink standing in for it) could
be substituted locally to steer where profiles are written. Two
concurrent `scripts/test verify` invocations therefore never share,
corrupt, or delete each other's profile, no existing path is ever
clobbered, and no artifact is left in the working tree.

Within ONE invocation, each profile filename is keyed by a per-loop
ORDINAL (`<n>-<slug>.out`), not by the module path alone. A slug built
by `tr '/' '_'` is not injective — the distinct valid module paths `a/b`
and `a_b` both map to `a_b` — so a path-only name lets the second
module's `go test` overwrite the first's profile, after which every
changed line in the overwritten module falls inside no remaining profile
block and its uncovered new code passes the gate unseen. The slug is
kept only as a human-readable suffix, where a collision is harmless. The dir is swept
by the single EXIT handler (`EXIT_TRAP`), which also reaps the shared
Postgres container only when this invocation actually recorded a lease.

The module list is enumerated ONCE per loop via `_module_list` and fed
to the loop as a here-string. Piping `modules` straight into `while
read` meant a failing `modules` yielded zero iterations and still exited
0 — a verify that reported success having run no tests at all. The
coverage loop degrades to the plain loop on that condition, and the
plain loop (`cmd_test`) FAILS CLOSED: an unavailable or empty module
list exits 1 with a printed reason. That is not the patch gate
red-lining verify (it never does); it is the test loop being unable to
run at all, which must never read as green.

### Fail-open contract

Every shell-side git/go/jq call runs in a TESTED context (`if !`,
`if [ … ]`), so none of them can abort verify under `set -e`. Each
degrade prints ONE line naming the reason and falls through to the plain
`cmd_test` loop:

| Branch | Layer |
|---|---|
| `FISHHAWK_SKIP_PATCH_COVERAGE=1` | shell |
| git absent / non-git or bare root / unresolvable base ref | shell (`_patch_cov_base`) |
| no merge base, or `go`/`jq` unavailable | shell (`_patch_cov_changed_modules`) |
| no changed Go packages | shell |
| module list unavailable inside the coverage loop | shell (then the plain loop's own fail-closed check applies) |
| profile scratch dir uncreatable / no profiles emitted | shell |
| git absent, non-git or bare root, invalid `--diff-base` override, unresolvable base ref, no merge base | Python (`GitSkip`) |
| no changed Go files, no coverable new statements, sub-floor diff | Python |
| undecodable changed path (newline `\001` record, or a tab/comma in the path) — widens to every module, does NOT skip | shell (`_patch_cov_all_modules`) |

Only the Python gate's below-threshold verdict and its `PathDecodeError`
fail-closed verdict are allowed to fail verify. In COMBINED mode (`--threshold` AND `--diff-base`) a git failure
skips only the patch gate — the aggregate gate still runs and decides
the exit code.

The pre-test snapshot (#2124) adds FAIL-CLOSED branches — a non-skip exit
1 (or a `return 1` from the shell that `set -e` turns into an aborted
verify), because these represent an untrustworthy change set, not a
missing one:

| Branch | Layer |
|---|---|
| undecodable changed path AT EMIT (`PathDecodeError`, moved pre-test) | Python (`run_emit_snapshot`) |
| snapshot emit failed / no sha256 tool to anchor the digest | shell (`cmd_test_with_patch_coverage` → `return 1`) |
| snapshot missing / unreadable, or digest mismatch, BEFORE the no-profiles skip | shell (`_verify_patch_coverage` → `return 1`) |
| snapshot missing / unreadable / malformed at consume | Python (`SnapshotError`) |
| malformed-but-parseable structure: non-string/ambiguous `skip`, or a line that is not a positive integer | Python (`SnapshotError`) |
| expected digest absent, or on-disk snapshot digest mismatch | Python (`SnapshotError`) |
| digest-verified snapshot with a NON-EMPTY changed map but ZERO coverage profiles | Python (`run_diff_gate`) |

The **no-profiles case is not a hole**, on either the snapshot or the
profiles. `_verify_patch_coverage` re-checks the on-disk snapshot against
the parent-memory anchor BEFORE it can skip on "no coverage profiles", so a
test that deletes/tampers the snapshot AND removes the profiles still fails
closed in the shell (Python is never reached on that path). And when the
snapshot digest DOES match but every `*.out` profile is gone, the shell
hands the zero-profile verdict to `check-coverage.py` (with no profiles)
rather than skipping: a snapshot carrying a non-empty changed map means the
loop instrumented the changed modules, so vanished profiles are an integrity
anomaly that FAILS CLOSED — the profile-deletion path a pristine snapshot
would otherwise let masquerade as "no coverable new statements". A git
degrade at emit still writes a trusted **skip-snapshot**, and a skip-snapshot
or empty change set with zero profiles still skips (exit 0) — a degrade, not
a tamper. This closes profile deletion without protecting the profile files
themselves (which the untrusted loop can still reach at
`-coverprofile=<dir>/…`); full containment of the test loop is
[ADR-063](https://github.com/kuhlman-labs/fishhawk/issues/2127).

### Env overrides

`FISHHAWK_DIFF_BASE`, `FISHHAWK_PATCH_COVERAGE_THRESHOLD` (default 85),
`FISHHAWK_SKIP_PATCH_COVERAGE`. All three are DEV-ONLY: the runner's
gate subprocess env is a default-deny allow-list
(`runner/cmd/fishhawk-runner/gateenv.go` admits only PATH/HOME/locale
essentials plus the `GO*`/`CGO_*`/`LC_*` prefixes), so no `FISHHAWK_*`
var reaches `scripts/test verify` in-loop and an agent cannot switch the
gate off.

### Testing

`scripts/test-check-coverage` (Python CLI, against throwaway git repos +
hand-written profiles) and `scripts/test-patch-coverage` (shell wiring,
sourcing `scripts/test` lib-only with an overridden `ROOT`). They are
standalone in the `scripts/test-*` style AND `scripts/test verify` runs
them via `_verify_gate_harnesses` (right after the schema-sync check),
alongside `scripts/test-dev` since #2455 — the three together take the
loop to ~26s, from ~11s for the two coverage harnesses.
`scripts/test-container-lease` (the #1792 lease + #3122 generation-keyed
orphan sweep contract, no Docker — it uses a fake `docker` on PATH) is
also wired into `_verify_gate_harnesses` and adds well under a second.
"Must be green"
is machine-enforced rather than asserted in prose, because a
Python/shell-only diff otherwise takes the no-changed-Go-packages SKIP
path and exercises neither the gate nor its harnesses. A
missing/non-executable harness prints a reason and is skipped;
`test-dev` is ALSO skipped when zsh is absent from PATH (it is
`#!/usr/bin/env zsh`, so the exec would otherwise fail 127); a failing
harness fails verify. `test-patch-coverage` must stub
`_verify_gate_harnesses` wherever it calls the real `cmd_verify`, or it
re-executes itself without bound.

Binary-safe path handling is pinned on both sides with REAL files whose
names carry a double quote, a backslash, a space and a non-ASCII
character (`test-check-coverage` (p), `test-patch-coverage` (c7)) —
each must be discovered and gated/bucketed. A literal-newline filename
is created for real where the platform allows it ((p2), (c8)), and
(c10) covers the same class one level up — a changed file under a
DIRECTORY whose name carries a tab or a comma must take the same
fail-closed widen, never a corrupt pattern. (g2) pins profile-name
injectivity: modules `a/b` and `a_b` must get distinct profile paths.
The
parsing layer is covered directly regardless by (p3)'s C-quoted decode
and synthetic NUL-delimited fixtures, so the case is never simply
skipped. (p4) asserts the fail-closed end state: an unidentifiable path
exits 1 naming the path rather than passing as "nothing to cover".

`test-patch-coverage` case (j) is the real-toolchain end-to-end for the
load-bearing `-coverpkg` claim: a function with no test in its own
package, exercised only by a SIBLING package's test, must report 100%
patch coverage and `PATCH PASS`. Without the restricted `-coverpkg` the
sibling test never instruments `foo`, so `foo` (having no test of its own)
contributes no coverage block and the patch's changed lines fall inside no
block: `total_new` is 0 and the gate prints a `SKIP` at exit 0 — not the
asserted `PATCH PASS` + 100% — so the case discriminates rather than merely
running. It self-skips with a printed reason when no
`go` toolchain is present. CI's
aggregate invocation is unchanged — diff mode is inert without
`--diff-base` — and `.github/workflows/**` is untouched (human-led).

The pre-test snapshot (#2124) is pinned on both sides. `test-check-coverage`
(s1–s11): emit serializes the change set with a non-ASCII path key that
round-trips — and (s1b) with a truly-undecodable non-UTF-8 (0xFF) byte
that surrogateescape-decodes and round-trips byte-identically (a real
non-UTF-8 file where the platform allows one, e.g. Linux; where it does not,
e.g. macOS APFS, `emit_changed_snapshot` is driven directly in-process with
the same surrogateescape-decoded key — the (p2)→(p3) synthetic-fixture
fallback — so the binary-byte edge is machine-enforced on every platform,
not self-skipped on darwin); consume produces the correct
verdict; **consume is invariant
to a post-emit work-tree mutation while the recompute path SKIPs, shown
side by side** (s3, the TOCTOU proof); one case each for skip-snapshot
→ skip, missing → exit 1, corrupt → exit 1, digest mismatch → exit 1,
absent digest → exit 1, undecodable-path-at-emit → exit 1, and the
zero-profile guard surviving the `nargs +→*` change (s9, WITHOUT a snapshot);
(s10) a table of
malformed-but-parseable snapshots — a non-string `skip`, a payload carrying
BOTH `skip` and `changed`, and line values `int()` would silently coerce
(bool, numeric string, float, zero, negative, nested list) — each asserted
to exit 1 and never a SKIP; and (s11) zero profiles WHILE consuming a
snapshot — a non-empty changed map fails closed (exit 1), a skip-snapshot
still skips (exit 0). `test-patch-coverage`
(t1–t12) drives the ADVERSARIAL cases end to end through the real
`cmd_test_with_patch_coverage` + `_verify_patch_coverage` with a `go` stub
that mutates state mid-loop exactly as an untrusted test could (it receives
`-coverprofile=<dir>/…` and so can reach the snapshot beside it): a
tracked-file revert (t1) and an untracked-file delete (t2) — the issue's
done-means — plus **snapshot tampering** to a skip-snapshot (t3) and to a
changed-map that drops the file (t4), each asserted to FAIL CLOSED and
never a passing SKIP; a no-mutation control that PASSES (t5); the
emit-before-loop ordering + consume-flag wiring (t6); that the digest
anchor is never exported into the test environment (t7); the **combined
no-profiles path** (t8 deletes both snapshot and profile, t9 tampers the
snapshot and removes the profile) proving the no-profiles skip cannot bypass
snapshot integrity; the **snapshot-pristine profile-deletion path** (t12
deletes only the profile, leaving the snapshot's non-empty changed map
intact) proving zero profiles fails closed as an integrity anomaly rather
than skipping; and the two shell fail-closed branches themselves — a
failed emit (t10) and a failed digest anchor (t11) — each aborting the
coverage loop non-zero. The (e2)/(e3) unit cases pin the same no-profiles
integrity re-check directly on `_verify_patch_coverage`, and (e) that a
skip-snapshot with zero profiles still skips.

## Container lease + generation-keyed orphan sweep ([#1792](https://github.com/kuhlman-labs/fishhawk/issues/1792) / [#3122](https://github.com/kuhlman-labs/fishhawk/issues/3122))

`scripts/test` disables the testcontainers ryuk reaper
(`TESTCONTAINERS_RYUK_DISABLED=true`) so the shared `fishhawk-test-postgres`
persists across the package processes that reuse it, and reaps it via an EXIT
trap that is lease-refcounted across concurrent invocations (#1792). But
`backend/internal/postgres/postgres_test.go` is the documented pgtest exemption —
it needs raw, un-migrated throwaway databases, so it starts its OWN anonymous
testcontainers Postgres containers rather than reusing the shared one. Nothing
reaped those: ryuk is off and the lease trap only knew the named container, so
they accumulated across invocations until a loaded daemon failed a verify gate
category-A against packages the change never touched (the #3122 signature).

The root repair is Go-side (`postgres_test.go`'s `terminateStartFailure` on the
start-error path). Layered on top, `scripts/test` sweeps orphans keyed to the
LEASE GENERATION:

- **The generation boundary is the lease's own.** A generation OPENS when
  `$LEASE_DIR` first comes into existence (`_register_lease` sees it absent) and
  CLOSES on the successful `rmdir "$LEASE_DIR"` the last-holder reap already uses.
- **First holder writes the snapshot, later holders reuse it.** The first holder
  to arm records the testcontainers-labelled container set that PREDATES the
  generation into `$LEASE_DIR.snapshot`; a holder joining an already-open
  generation reuses it UNCHANGED. Per-INVOCATION keying was the bug this fixes: A
  arms and creates cA, B arms later and would snapshot cA as pre-existing, A exits
  ref-held sweeping nothing, B exits last-holder and spares cA — so cA leaks.
  Generation keying makes cA absent from the (empty) snapshot B reuses, so B
  sweeps it.
- **Decline to publish on a FAILED query, not just an absent daemon.**
  `_labelled_container_ids` distinguishes docker ABSENT (clean no-op, exit 0)
  from a docker PRESENT-but-failing query (daemon down / transient error), which
  PROPAGATES the non-zero exit rather than `|| true`-swallowing it. On that
  failure `_write_generation_snapshot` writes NO snapshot — it leaves any prior
  snapshot untouched and, when none exists, leaves the sweep to degrade to a
  no-op. Publishing an EMPTY snapshot on a transient failure would, after the
  daemon recovers, make the last holder treat every labelled container as
  generation-created and remove unrelated testcontainers workloads — the
  destructive expansion this guard exists to prevent.
- **Sibling path, NOT inside `$LEASE_DIR`.** The snapshot is `$LEASE_DIR.snapshot`
  (a sibling, exactly as `$LEASE_DIR.lock` is), because the last-holder proof is
  `rmdir` succeeding, and `rmdir` requires an EMPTY directory — a file inside
  `$LEASE_DIR` would make it fail and silently disable BOTH the shared-container
  reap and the sweep. It is written atomically (temp sibling then `mv`).
- **Last holder sweeps, INSIDE the lock.** The last-holder branch removes every
  labelled container ABSENT from the snapshot (created DURING the generation,
  including by an overlapping invocation that exited ref-held) and then deletes
  the snapshot — both held under the lease lock across `rmdir` → sweep → removal,
  so a concurrent invocation cannot arm between the `rmdir` and the removal, open
  a new generation, write a fresh snapshot, and have the outgoing holder delete it
  (the sweep would then be quietly dead for a whole generation).
- **Preflight warning.** When a holder OPENS a generation on a daemon already
  carrying more than `FISHHAWK_TEST_ORPHAN_WARN_AT` (default 4) labelled
  containers, `scripts/test` prints a one-line stderr WARNING naming the count and
  the removal command. Stderr is captured — the runner runs these commands with
  `CombinedOutput`, so the warning reaches the category-A `FailureReason` (#3122
  proposals 3/4) with no runner change.

**What this guarantees, precisely.** A container created at any point DURING the
lease generation is reaped by the last holder — including one created by an
overlapping invocation that exited ref-held and swept nothing, and one whose test
binary was SIGKILLed before its `t.Cleanup` ran. It does NOT reap a container that
predates the generation (spared by design, reported by the preflight instead), one
created after the last holder's comparison, or anything when the sweep degrades.

**Residuals, stated not papered over.** (1) The preflight fires ONLY for the
invocation that OPENS a generation; one joining an already-open generation on a
littered daemon warns nothing. (2) Attribution is by the `org.testcontainers=true`
label — the only signal available with ryuk disabled — so a container created by
an UNRELATED testcontainers workload on the same daemon during the generation
would be swept; `FISHHAWK_TEST_NO_ORPHAN_SWEEP` is the documented opt-out for an
operator running such workloads concurrently. (3) The sweep degrades to a NO-OP
(never remove-everything) when the snapshot is missing/unreadable, docker is
absent, or the opt-out is set. `FISHHAWK_TEST_NO_ORPHAN_SWEEP` /
`FISHHAWK_TEST_ORPHAN_WARN_AT` are DEV-ONLY: the runner's gate env is a
default-deny allow-list, so neither reaches the in-loop gate.

Pinned by `scripts/test-container-lease` (sourcing `scripts/test` lib-only with a
fake `docker` on PATH serving an injectable labelled-container set, an injectable
`ps`-fails sentinel, and a per-`rm` lock-state probe): the interleaving case (the
per-invocation-snapshot counterfactual), pre-generation sparing + preflight
report, sibling-path placement + successful rmdir, the no-snapshot degrade, the
opt-out (with the candidate container added AFTER registration so it is absent
from the snapshot — the case would be vacuous otherwise), the decline-to-publish
transition (a failing `ps` at generation-open writes no snapshot and a recovered
daemon is not swept), the sweep-runs-INSIDE-the-lock ordering (the `docker rm`
fires with the lease lock still held — reddens if the sweep moves after
`_lease_unlock`), snapshot lifecycle (retained after ref-held, deleted after
last-holder), and a load-bearing branch-invariance count assertion over the
unchanged #1792 refcount branches. That harness is now run in-loop by
`scripts/test verify` via `_verify_gate_harnesses`, adding well under a second (it
needs no Docker).

## Local k8s ergonomics (ADR-034 / [#852](https://github.com/kuhlman-labs/fishhawk/issues/852))

`scripts/dev k8s` / `scripts/dev k8s-down` (thin Makefile aliases
`make k8s-up` / `make k8s-down`) — one-command bring-up/teardown of the
Helm chart on Docker Desktop's Kubernetes.

`cmd_k8s_up`:

- Builds the fishhawkd image into the host Docker daemon as
  `ghcr.io/kuhlman-labs/fishhawkd:dev-local` (Docker-Desktop k8s shares
  that image store — no registry push / kind load).
- `helm upgrade --install`s the chart with `values-local.yaml` plus
  `--set image.tag=dev-local --set image.pullPolicy=IfNotPresent`
  (overriding values-local's `main`/`Always` so the local build is
  used).
- Waits for the rollout, then opens a
  `kubectl port-forward svc/fishhawk 8080:8080` and gates on `/healthz`
  via the same `_await_healthz` poll `cmd_up` uses — the authoritative
  readiness signal, since the in-cluster migrate Job runs as a
  `post-install` hook and rollout-status can go green before it
  finishes.
- Fails loud on a stuck rollout or `/healthz` timeout: kubectl
  pods + logs tail to stderr, non-zero exit.

### Jaeger port-forward

When the dev-only in-cluster Jaeger is present (`values-local.yaml`
enables `jaeger.enabled`), `cmd_k8s_up` opens a second
`kubectl port-forward svc/fishhawk-jaeger 16686:16686 4318:4318` AFTER
the `/healthz` gate — Service-guarded, so a jaeger-disabled override is
a clean skip; pid tracked in `.fishhawk/k8s-jaeger-pf.pid` — so the
host-spawned runner can emit spans to `localhost:4318` and the operator
can view the Jaeger UI at `localhost:16686`.

### Teardown

`cmd_k8s_down` kills both tracked port-forwards (fishhawkd pid in
`.fishhawk/k8s-pf.pid`, jaeger pid in `.fishhawk/k8s-jaeger-pf.pid`,
mirroring `PID_FILE`) and `helm uninstall`s (idempotent).

### Testing and docs

The pure helpers `_k8s_image_ref` / `_k8s_healthz_url` are unit-tested
by `scripts/test-dev`. Operator quickstart + the values-local-vs-prod
split: `docs/deploy/kubernetes.md`. The true end-to-end path (image
build → chart install → `/healthz` green) is an operator smoke test
against a Docker-Desktop cluster, not run in CI.

`scripts/test-dev` also asserts that the SHIPPED `backend/Dockerfile`
derives its `go build` `GOARCH` from BuildKit's `TARGETARCH` automatic
platform ARG rather than a hardcoded literal (#2912 — a hardcoded
`GOARCH=amd64` builds an amd64 image that only runs under emulation on
an arm64 Docker-Desktop node): (A) `ARG TARGETARCH` is declared inside
the builder stage — after `FROM ... AS builder`, before the fishhawkd
`go build` RUN line — since an automatic platform ARG declared only in
the global scope does not propagate into a stage; (B) that RUN line
sets `GOARCH=${TARGETARCH}`; (C) no hardcoded architecture literal
(`amd64`, `arm64`, etc., in any quoting/spacing, including inside a
`${VAR:-amd64}`-style default) remains anywhere in the file, with
comment lines stripped first so a comment documenting the prior
behavior doesn't trip it. This pins the shipped build directive so a
no-op or comment-only touch fails the gate — it does NOT prove the
built image's `.Architecture` actually matches the host/node; that
remains the operator smoke test above, not run in CI.

## Opt-in local TLS front end (E66.28 / [#2453](https://github.com/kuhlman-labs/fishhawk/issues/2453))

An **opt-in, default-off** loopback-only reverse proxy (caddy) that
terminates TLS on `:8443` and forwards to the **unmodified** plain-http
`fishhawkd` on `127.0.0.1:8080`, so the OAuth 2.1 AS can be exercised
over `https` on a workstation with no Go code changes. Enabled by
`FISHHAWK_DEV_TLS=1` in `.env`. Operator quickstart, the client-trust
(`NODE_EXTRA_CA_CERTS`) step, the AS config recipe and the
audience-port foot-gun: `docs/local-tls.md`.

### Helper inventory

All under `scripts/dev`, pure or single-purpose so `scripts/test-dev`
drives them with stubs (no real caddy, no real handshake):

- `_tls_enabled` — 0 only when `FISHHAWK_DEV_TLS` is exactly `1`.
- `_tls_port` / `_tls_healthz_url` — TLS port (default 8443,
  `FISHHAWK_DEV_TLS_PORT`) and the through-the-proxy `https` health URL.
- `_tls_ca_file` / `_tls_cert_file` / `_tls_key_file` / `_tls_caddyfile`
  — artifact paths under `.fishhawk/cache/tls/`.
- `_tls_render_caddyfile <port> <cert> <key> <upstream>` — the config
  text: a global block (`auto_https off`, `admin off`) and a bare
  `:<port>` site carrying `bind 127.0.0.1 ::1` (loopback-only listener),
  `tls <cert> <key>` and `reverse_proxy <upstream>`.
- `_tls_resolve_proxy_bin` — fail-loud detection
  (`FISHHAWK_DEV_TLS_PROXY_BIN` > `command -v caddy`); NEVER returns 0
  having found nothing.
- `_tls_require_openssl` — the cert-generation prerequisite.
- `_tls_ensure_certs <dir>` — idempotent openssl generation of a
  self-signed CA + a dual-SAN (`DNS:localhost` + `IP:127.0.0.1`) leaf.
  Regenerates only when missing, near expiry, or when the chain fails
  `openssl verify -CAfile`; generates into a temp dir moved into place
  only after the chain verifies; never rotates a still-valid CA on a
  leaf-only renewal (so an exported `NODE_EXTRA_CA_CERTS` stays valid).
- `_await_tls_healthz <pid> <url> <cafile> <deadline>` — a **sibling**
  of `_await_healthz` (so the `#628`/`#965` gate contract is untouched):
  polls `curl --cacert` through the proxy, fails fast on spawn-then-die.
- `_tls_up` / `_tls_down` — orchestration and teardown.

### Fail-loud detection and the through-the-proxy gate

`_tls_up` checks the proxy binary and openssl **first** (before any
artifact is created), preflights the TLS port (`#965` posture, naming a
squatter), generates certs, renders the Caddyfile, spawns the proxy
(pid → `.fishhawk/tls-proxy.pid`, log → `logs/tls-proxy.log`), then
gates readiness **through the proxy** — `curl --cacert` against
`https://localhost:8443/healthz`. A proxy that never came up cannot read
as green; on gate failure it tails the log, kills the proxy, removes the
pid file and returns non-zero. It runs in `cmd_up` **after** the `#1018`
nonce round-trip (which stays on plain-http `fishhawkd`) and **before**
the MCP banner.

### Teardown

`cmd_down` calls `_tls_down` **unconditionally** (guarded on the pid
file's existence, never on `FISHHAWK_DEV_TLS`), so disabling the flag
between `up` and `down`/`reload` never orphans the proxy. Certs are
deliberately not removed (CA stability).

### Testing

`scripts/test-dev` pins the helpers against stubs: the rendered-config
done-means assertion (bare `:8443` site, `bind 127.0.0.1 ::1`,
`tls`/`reverse_proxy`, `auto_https off`, `admin off`), the non-vacuity
through-the-proxy gate, one case per `_tls_up` failure branch
(proxy-binary-absent, openssl-absent, port-occupied, spawn-then-die,
readiness-timeout), `_tls_down` idempotency + live-kill, the call-site
wiring, and — gated on real `openssl`/`s_server` — the cert path end to
end (idempotency, chain-mismatch self-repair, leaf-only renewal, and a
dual-SAN handshake served by `openssl s_server` verified with
`curl --cacert` on both URL shapes). The Caddyfile itself is unproven
until caddy is installed; the live operator walk covers it.

Since #2455 `scripts/test verify` runs `scripts/test-dev` in its
`_verify_gate_harnesses` loop, so a `scripts/dev` regression fails
in-loop rather than only when a human remembers to run the harness (it
is still runnable standalone; skipped in-loop when zsh is absent).

## Docs-site voice gate (E12.1 / [#2261](https://github.com/kuhlman-labs/fishhawk/issues/2261))

`docs/BRAND_FOUNDATIONS.md` §5 ("Things we never say") bans a specific
vocabulary. On the public site — `site/src/content/docs/`, the one
surface a stranger reads — that ban is machine-enforced rather than
asserted in prose, so a later docs child that writes "seamless" into a
page fails `scripts/test verify` in-loop instead of landing.

Two scripts:

- **`scripts/check-site-voice [CONTENT_ROOT]`** — the gate. Walks every
  `*.md` / `*.mdx` under the content root (default
  `site/src/content/docs`) and exits 1 on any banned term.
- **`scripts/test-site-voice`** — the harness that pins the gate,
  driving the real script against temp-dir fixtures.

### Matching

Case-insensitive and **prefix**-based, anchored on a **leading word
boundary**. Prefix-based so `seamlessly` and `empowering` hit without
enumerating every inflection; leading-boundary-anchored so a banned term
appearing only *inside* a longer word (`unseamless`, `disempower`) does
not. Both halves are load-bearing and both are pinned — the anchor by
harness case c4, whose fixture carries exactly those embedded tokens.

Every violation is reported — file, line number, matched term — before
the non-zero exit. A first-hit-and-stop implementation would hide a
page's second offence until the first was fixed; case c5 pins that with
a two-file fixture and asserts the SECOND file is named. It asserts the
name and not a line count deliberately: the summary line the script
prints means a count is satisfiable by a first-hit-and-stop version.

### What is deliberately NOT mechanized

`trust`. §5 bans trust as a *marketing claim*, not the word, and this
repository legitimately writes "trust boundary" and "trusted tree". A
substring ban would false-positive on those and train readers to ignore
the checker, so `trust` stays a human-review item. Case c4 pins that it
does not fire.

### Fail-open contract — and its one exception

A missing content root prints a one-line reason and exits **0**, as does
a content root holding no pages. This is a voice linter that verify runs
unconditionally, not a security control: it must never red-line a
checkout that has no `site/` yet. Case c6 pins the absent-root branch,
asserting both the exit code and the printed reason.

A FAILED enumeration is the exception and exits **2** (distinct from
1 = violations found). Those two fail-open branches are decisions about a
tree with nothing to lint; a failed walk means the checker does not KNOW
what is there, and reporting it clean would silently bypass the gate. So
the `find … | sort` runs into a file whose exit status is checked, not
into a process substitution whose status the surrounding `while` cannot
see. Case c9 pins it, injecting the failure with a PATH-stubbed `find`
rather than an unreadable directory — a harness running as root would
read that directory fine and turn the case into a silent false pass.

Page paths are handed to `awk` prefixed with `./` when relative. awk
reads an operand of the form `name=value` as a variable assignment rather
than a file, and `CONTENT_ROOT` is caller-supplied, so a root like `a=b`
would leave every page under it silently unlinted. `--` does not help —
this is operand parsing, not option parsing. Case c10 pins it, with the
content root named `a=b` and invoked from its parent: a deeper
`c10/a=b/page.md` is already safe (`c10/a` is not an identifier) and
would make the case a false pass.

### Testing

`scripts/test-site-voice` runs ten cases, one per named behavior:

| Case | Pins |
|---|---|
| c1 | clean content → exit 0 |
| c2 | a banned term → exit 1, report names file + line number + term |
| c3 | mixed-case `Frictionless` → exit 1 (case-insensitivity) |
| c4 | `unseamless` / `disempower` / `seam` / `trusted` → exit 0 (leading-boundary anchor + the `trust` carve-out) |
| c5 | two distinctly-named offending pages → exit 1, BOTH named |
| c6 | absent content root → exit 0 with a printed reason |
| c7 | content root with no `.md`/`.mdx` pages → exit 0 with a printed reason |
| c8 | `scripts/test`'s `_verify_site_voice` skips with a reason (return 0) when the gate is absent |
| c9 | a failed page enumeration (PATH-stubbed `find`) → exit 2 with a reason, never a clean report |
| c10 | a page under a relative content root named `a=b` → exit 1, the page named (awk operand parsing) |

c3 asserts the exit code plus exactly ONE named term, never both. That
is deliberate: it keeps c3 green under a first-hit-and-stop weakening,
so that counterfactual isolates c5.

Eight counterfactuals were run against the controls and OBSERVED, not
reasoned about — each deletion restored byte-identically afterwards:

| Deletion | Observed RED |
|---|---|
| the banned-term match loop | c2, c3, c5 |
| weaken to first-hit-and-stop (exit code + summary line intact) | c5 only — the surviving summary still said "2 occurrence(s)", so a line COUNT would have passed |
| the leading word-boundary anchor | c4 only, on `unseamless` → `seamless` and `disempower` → `empower` |
| `_verify_site_voice`'s not-executable guard | c8 only (exit 127, the red-line it prevents) |
| the empty-file-list fail-open | c7 only (`files[@]: unbound variable` under bash 3.2) |
| the absent-content-root fail-open | c6 only (bare `find` error, no reason printed) |
| the enumeration-status check (back to `< <(find … \| sort)`) | c9 only (exit 0, "no .md/.mdx pages … nothing to lint" — the fail-open itself) |
| the `./` operand prefixing | c10 only (exit 0, "1 page(s) clean under a=b" — the offending page never linted) |

`scripts/test verify` runs BOTH: `test-site-voice` in the
`_verify_gate_harnesses` loop (proving the control still works), and
`check-site-voice` itself via `_verify_site_voice` against the committed
pages (proving they still pass it). Both are bash, so neither needs the
zsh guard `test-dev` carries. Long-form site contract: `site/README.md`.

## Helm chart render gate (`scripts/test-helm-render`, E62.2 / #2301)

The fourth entry in `_verify_gate_harnesses`. `deploy/helm/fishhawk` is
almost entirely YAML defaults and rendered identifiers whose correctness
no compiler enforces, so this is the only machine check that the chart
still renders what the docs claim. It drives the REAL chart through
`helm template` / `helm lint` and asserts on SHIPPED RENDERED OUTPUT:

| Case | What it pins |
|---|---|
| r1 | `values-prod` renders, the copy-paste caveat is gone, and the DERIVED `FISHHAWKD_EXTERNAL_URL` / `FISHHAWKD_OAUTH_CALLBACK_URL`, ingress class, TLS host and cert-manager issuer match the chosen hostname (plus a cross-check that the derived callback PATH is the one `backend/cmd/fishhawkd/` registers) |
| r2a–r2f | one case per named failure mode of `fishhawk.validateSecretContract`: chartManaged field ABSENT, the same field an EMPTY STRING (deliberately distinct — present-but-empty must not pass), externalSecrets `data[]` not covering a required key, empty `existingSecret`, the dotted PEM key participating in the contract at all, and (r2f) the WHOLE `secrets.values` map overridden to null — which `index` would answer with a raw `index of untyped nil` trace, so the guard substitutes an empty dict and r2f asserts the NAMED message and the ABSENCE of that trace |
| r3 | requests AND limits on the fishhawkd container and the migrate Job |
| r4 | migrate Job `restartPolicy: Never`, `backoffLimit: 2`, `activeDeadlineSeconds` omitted by default, and `fishhawk.validateMigrateTiming` rejecting a too-tight deadline naming BOTH numbers — including THE BOUNDARY: the derived time-to-Failed is a LOWER bound, so a deadline EQUAL to it (`=210`) is rejected (`le`, not `lt`) while `211` renders, making the boundary a decided behaviour rather than an artefact of the operator chosen. `activeDeadlineSeconds=0` is pinned as UNSET (no field emitted), not a zero-second ceiling |
| r5 | the `envFrom` wiring across allInOne / split-api / split-worker / migrate Job, the cell keys landing in the ConfigMap vs the credential contract, `FISHHAWKD_DATABASE_URL` per profile against the path that profile uses, and the PEM's dotted key being a Secret key + projected FILE but never an env key. The ref assertions are STRUCTURAL, not substring: `yaml_block` extracts the container's `envFrom:` block and `envfrom_ref` reads the `name:` under each `configMapRef:`/`secretRef:` entry, so a Secret still referenced only by the PEM volume — or a move to explicit `env` + `secretKeyRef` — reddens the case instead of satisfying it. The `FISHHAWKD_DATABASE_URL` per-profile assertions are scoped to the container's own `env:` block for the same reason. The three PEM assertions render a SYNTHETIC App-enabled `chartManaged` fixture, NOT `values-local`, because the local profile deliberately ships the GitHub App OFF since #2914 (see r16); reading `values-local` here would go green-for-the-wrong-reason today and red once the placeholder is gone |
| r6 | every profile renders (local, prod, single-tenant, cell), cell additionally under all three secret modes |
| r7 | `helm lint` per profile |
| r8 | the Mode-1 fail-closed case: a HALF-CONFIGURED `singleTenant` profile (fields set, `accountKey` empty) is emitted UNCHANGED and the account key is neither dropped-and-defaulted nor silently benign, so fishhawkd's documented startup refusal stays reachable |
| r9 | `NOTES.txt` CONTENT via `helm install --dry-run` (`helm template` executes but does not emit NOTES, and `--show-only` cannot select it): the operator-facing env-key list is DERIVED from `fishhawk.secretKeySpec` rather than hand-authored, and the key-confirmation command inspects key NAMES only — the assertion is scoped to the runnable `kubectl` lines and rejects `jsonpath` / `-o yaml` / `-o json`, any of which would print every base64-encoded credential |
| r10 | the OAuth trio POSITIVE + OFF posture (E69.4 / #2915): `FISHHAWKD_OAUTH_CLIENT_ID` + callback + `chartManaged` secret land TOGETHER for `values-local` and `values-prod` (id present, callback DERIVED from the host), and the OFF posture — no client id + an ENABLED ingress — renders NO client id, NO callback, and succeeds with `secrets.values.oauthClientSecret` ABSENT (the derived-requiredness discriminator; the client-id gate on the callback derivation stops a lone callback key that would make fishhawkd exit) |
| r11a–r11e | one case per named failure mode: `fishhawk.validateOAuthTrio` (i) id set + callback empty (message names BOTH ways out — explicit `config.oauthCallbackUrl` or the ingress derivation), (ii) callback set + id empty, (iii) `chartManaged` secret supplied + id empty and (iii) externalSecrets `data[]` maps the secret + id empty, plus (r11e) id set + secret ABSENT which surfaces via `validateSecretContract`'s DERIVED requiredness — one message per condition, not two guards racing |
| r12 | `config.extraEnv` passthrough renders verbatim (string/int/bool coerced to a quoted string, `helm lint`-clean), the collision guard fails naming a key that shadows a chart-managed key, the identifier guard fails naming a non-`^[A-Za-z_][A-Za-z0-9_]*$` key, and the ANTI-DRIFT loop renders an all-config-keys ConfigMap, extracts every emitted `FISHHAWKD_*` key at KEY position, and asserts `--set config.extraEnv.<KEY>=x` collides for EACH — so a key added to `configmap.yaml` without a `fishhawk.managedConfigKeys` entry reddens the gate. The fixture enables an ingress + client id so the DERIVED keys are in scope |
| r13 | CROSS-BOUNDARY: greps `backend/cmd/fishhawkd/` for `FISHHAWKD_OAUTH_CLIENT_ID` and the all-three-or-none refusal text, pinning the chart's joint-validation claim to the `serve.go` contract it mirrors (the technique r1 uses for the callback path) |
| r14 | the GitLab family (E45.32 / #2922): GitHub-only ConfigMap/Secret carry no `FISHHAWKD_GITLAB_*` key, a complete GitLab config renders all five ConfigMap keys + three Secret keys, and one case per named `fishhawk.validateGitLabOAuthTrio` branch + derived-requiredness + the explicit-null edge |
| r15 | REGISTRY EXISTENCE of every third-party image the chart RENDERS (#2913). A rendered-output gate structurally CANNOT catch a nonexistent tag — the manifest is valid YAML and the minio bucket Job is a Helm HOOK, so a bad pin surfaces only as a `helm upgrade` timeout. r15a is the PURE classifier table (`_registry_verdict`: 200→exists, 404→missing, else→indeterminate) plus the probe degrades (non-Docker-Hub / digest / tagless →unsupported, unreachable token endpoint →indeterminate, AND — the manifest-HEAD twin — a reachable-auth-but-unreachable-registry `FISHHAWK_REGISTRY_URL=http://127.0.0.1:1` →indeterminate, exercising the `curl -I` 000 branch live rather than only via the pure classifier — the `FISHHAWK_REGISTRY_AUTH_URL` / `FISHHAWK_REGISTRY_URL` overrides make both skip branches testable). BOTH curls in `_registry_probe` carry `--connect-timeout` + `--max-time` (overridable via `FISHHAWK_REGISTRY_CONNECT_TIMEOUT` / `FISHHAWK_REGISTRY_MAX_TIME`) so an endpoint that ACCEPTS a connection but never responds cannot block verify indefinitely (high untested-path, #2913): a python3-guarded localhost mock (`_r15_mock_server`, TEST-ONLY, skipped-with-reason when python3 is absent) drives the accept-but-never-respond STALL on both the token curl (blackhole endpoint) and the manifest HEAD (token-ok endpoint that stalls non-token requests), and each assertion checks the probe RETURNS within a bound (a MISSING `--max-time` shows as a ~20s stall, the deletion-observed control); the same token-ok mock, pointed at a REFUSING registry, deterministically exercises the manifest-HEAD 000 branch (token fetched OK first, unlike the offline-fragile twin above). These stall tests are gated out of the r15e self-exec child so it stays cheap. r15b is the anti-vacuity SENTINEL: it probes the known-bad `minio/mc:RELEASE.2025-01-20T14-49-07Z` and requires `missing`, else it SKIPS the live probes so an unreachable/rate-limited registry can never green r15c. r15c renders `values-local` and requires every third-party image to probe `exists` (`missing` FAILS naming the ref; `indeterminate`/`unsupported` skips) AND requires at least one ref to have positively probed `exists` — `_r15_probe_refs` emits a trailing `__r15_exists__ <n>` count so r15c's return-0 pass branch cannot collapse "every ref verified exists" into "every ref was SKIPPED indeterminate" (a per-ref 429 storm landing after the sentinel passed); zero-exists here FAILS rather than vacuously passing (low untested-path, #2913). r15d is the COUNTERFACTUAL: render with the exact nonexistent tag and require the same detector to report it missing. r15e is the END-TO-END degraded-network run: it re-execs the whole harness with `FISHHAWK_HELM_RENDER_SELFTEST=1 FISHHAWK_REGISTRY_AUTH_URL=http://127.0.0.1:1` (the self-test flag makes the child skip r1–r14 and re-enter only r15, so it is cheap and does not recurse), driving the sentinel to `indeterminate` so the r15b skip branch fires, and asserts the complete harness still exits 0 while printing the skip reason — proving the degraded-network path never red-lines verify. **SECURITY (ADR-029 / #650): the `FISHHAWK_REGISTRY_AUTH_URL` / `FISHHAWK_REGISTRY_URL` overrides exist for local skip-path testing only; the probe carries NO credentials beyond the anonymous, short-lived, pull-scoped Docker Hub token it fetches per-ref, so an override that redirects that token to another host discloses nothing sensitive.** **Two residuals, both deliberate.** (1) The `helm`/`curl`-absent AND registry-unreachable/rate-limited cases SKIP with a printed reason and exit 0 — the chart's image pins are UNGUARDED there, the same honest residual shape as the helm-absent skip. (2) SHARED DEPENDENCE (binding condition 4, #2913): r15b's sentinel and r15d's counterfactual both hinge on a live 404 for that one tag, so if Docker Hub ever PUBLISHES it, r15b skips and r15d skips WITH it — the counterfactual guard quietly disables itself. Named here rather than mitigated; a second never-published sentinel ref would be the fix if it ever fires |
| r16 | the GitHub App OFF posture (E69.3 / #2914): `values-local` ships the App off because a placeholder PEM cannot parse and fishhawkd parses it EAGERLY at boot, so a committed placeholder crashloops the pod. r16a asserts the rendered ConfigMap carries NEITHER `FISHHAWKD_GITHUB_APP_ID` NOR `FISHHAWKD_GITHUB_APP_PRIVATE_KEY_FILE` (`assert_field_absent`, KEY position); r16b asserts the rendered Secret carries no dotted `github-app-private-key.pem` key AND no `BEGIN RSA PRIVATE KEY` / `DEV-PLACEHOLDER` body appears anywhere in the release (the placeholder is GONE, not relocated); r16c asserts the Deployment has no `github-app-private-key` volume and no mount at the PEM path; r16d is the POSITIVE anti-vacuity case — `values-local` PLUS the README's documented enable overrides renders BOTH env keys and the volume + mount, so r16a–c cannot be greened by a chart that dropped the App path entirely; r16e is CROSS-BOUNDARY — greps `backend/cmd/fishhawkd/` for the joint-requirement text (`both --github-app-id and --github-app-private-key-file required`) and the App-absent warn text (`webhook dispatch and GitHub-side actions will be disabled`), both copied VERBATIM from `serve.go`, so a `serve.go` change that makes the App mandatory reddens this gate instead of silently reinstating the crashloop |
| r17 | SELECTOR INTEGRITY (E69.5 / #2916): the allInOne fishhawkd Service + Deployment carry an `app.kubernetes.io/component: server` discriminator so they select ONLY the fishhawkd pod, not the postgres/minio/jaeger pods or the migrate/minio-bucket hook Job pods (all sharing the same bare `name`+`instance` labels). Four named assertions, ALL run in BOTH allInOne and split renders over a pod universe that INCLUDES the two Job pod templates (they satisfy today's buggy bare `svc/fishhawk` selector, so they are part of the shipped defect, not a hypothetical): r17a — every rendered Service selects EXACTLY ONE workload pod set (a pod satisfies a selector when every selector `key:value` is in the pod labels); r17b — `svc/fishhawk`'s selector is the FULL `{name,instance,component}` set (`server` allInOne, `api` split), a discriminator-only check would prove the discriminator not the selector; r17c — every Deployment's `spec.selector.matchLabels` identity set EQUALS its pod-template identity set (equality, not subset, so a dropped component reddens it), and the primary Deployment carries the full expected set; r17d — no pod set satisfies two DISTINCT Deployment selectors. A Job's OWN selector is controller-generated from a controller-uid and is never rendered by the chart, so Jobs enter r17a/r17d as candidate PODS only, never as an overlapping selector; the harness asserts the Job pods' component values (`migrate`, `minio-bucket`) by rendered output. Parsing is `awk`-only (no yq/PyYAML): `docs_of_kind` splits the render on `---` into per-kind documents, `identity_labels`/`identity_lines` extract the `app.kubernetes.io/{name,instance,component}` set at KEY position (dropping `helm.sh/chart`/`version`/`managed-by`/`pod-template-hash`, so a sibling label cannot perturb the set) for r17b/r17c/r17d, `all_labels` extracts the COMPLETE label set that r17a matches over (so a Service selector carrying an extra key absent from the pod — which would select zero pods — reddens r17a rather than being silently ignored, high/test_vacuity #2916), and `selector_matches` tests the subset relation. The COUNTERFACTUAL RECORD is the M0–M4 mutation-to-verdict matrix in the r17 header comment of `scripts/test-helm-render`: M0 control a/b/c/d GREEN; M1 (drop component from the Deployment selector) c+d RED; M2 (drop from the pod labels) a+c RED; M3 (drop from the Service selector arm) a+b RED — a reports the observed count `!= 1`, not a hardcoded six; M4 (role `server`→`api` in both selector+pod, Service still `server`) a RED plus the r17c full-expected-set pin RED (a condition-2 strengthening beyond the plan's pre-condition matrix). Every assertion is reddened by at least one mutation |

Every failure case asserts BOTH a non-zero exit AND the expected
key/number in stderr, so a render failing for an unrelated reason cannot
green it; `assert_field_absent` matches at YAML KEY position, because the
rendered manifest carries comments that name the very fields being
asserted absent. For the same reason the wiring assertions parse
structurally (`yaml_block` / `envfrom_ref`) rather than grepping the whole
document: a rendered Deployment names the Secret in its PEM volume and in
comments, so a document-wide substring match can stay GREEN with the
container's `envFrom.secretRef` gone — the vacuity the structural form
removes.

Bash (no zsh guard) and awk/grep only (no yq, no PyYAML). It **SKIPS with
a printed reason and exits 0 when `helm` is absent from PATH** — the skip
lives inside the harness, not in `_verify_gate_harnesses`, so the loop
needs no helm-specific branch. The cost is honest: on a helm-less host the
chart is unguarded, the same residual the zsh guard already accepts for
`test-dev`, and preferable to red-lining verify with exit 127.

The CI half of this gate is OPERATOR-INSTALLED: `.github/workflows/**` is
in the implement stage's `forbidden_paths`, so the workflow ships as a
copy-pasteable, install-verbatim block in
`deploy/helm/fishhawk/README.md`, following `site/README.md`'s Pages
workflow pattern. Long-form chart contract: `deploy/helm/fishhawk/README.md`.

## Site Reference generation and drift gate (E12.4 / [#2264](https://github.com/kuhlman-labs/fishhawk/issues/2264))

`gen-site-reference` regenerates the four generated **Reference** pages of the
documentation site (`site/src/content/docs/reference/{workflow-spec,plan-schema,cli,api}.md`)
from the canonical sources — the workflow-spec and plan JSON Schemas under
`docs/spec/`, the OpenAPI document `docs/api/v0.openapi.yaml`, and the
`cli/internal/cmdinfo` command inventory. It is a thin wrapper over the
`fishhawk-docgen` binary in the `cli` module, which splices each rendering
between the page's `<!-- BEGIN/END GENERATED <id> -->` markers and leaves the
orientation prose outside the markers untouched. `gen-site-reference --check`
reports drift and writes nothing.

`test-site-reference` is the drift gate, wired into `scripts/test verify`'s
`_verify_gate_harnesses` loop. It hashes each committed page, regenerates the
pages IN PLACE, hashes them again, then RESTORES the committed bytes
unconditionally — so a generated region that has fallen out of date fails the
gate WITHOUT leaving the working tree mutated. It compares CONTENT HASHES, not
git status classes, so it behaves the same in a dirty tree, a fresh checkout, or
CI. It SKIPS with a printed reason and exits 0 when `go` (or a sha256 tool) is
absent from PATH — the harness owns that skip, the same residual the zsh and
helm guards accept, so a go-less host is not red-lined with exit 127. The
independent second path is the Go drift test `cli/internal/docgen/drift_test.go`,
which gates CI through the module test loop and fails LOUDLY (never skips) when
it cannot resolve the repo root or `site/` is absent.
