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
both (`_verify_gate_harnesses`, ~3s, right after the schema-sync check):
"must be green" is machine-enforced rather than asserted in prose,
because a Python/shell-only diff otherwise takes the
no-changed-Go-packages SKIP path and exercises neither the gate nor its
harnesses. A missing/non-executable harness prints a reason and is
skipped; a failing one fails verify. `test-patch-coverage` must stub
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
patch coverage and `PATCH PASS`. The same fixture run without the
restricted `-coverpkg` reports 0% and fails, so the case discriminates
rather than merely running. It self-skips with a printed reason when no
`go` toolchain is present. CI's
aggregate invocation is unchanged — diff mode is inert without
`--diff-base` — and `.github/workflows/**` is untouched (human-led).

The pre-test snapshot (#2124) is pinned on both sides. `test-check-coverage`
(s1–s11): emit serializes the change set with a non-ASCII path key that
round-trips; consume produces the correct verdict; **consume is invariant
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
