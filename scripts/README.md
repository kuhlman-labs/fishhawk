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
always taken from the same snapshot: an uncommitted new function is
gated rather than mis-attributed. Using the merge base (not the base
tip) keeps commits that landed on the base branch after this branch
forked out of the patch denominator, exactly as `base...HEAD` would.
The real guarantee is therefore: *no skew between the diff and the
profile*, not *no failure is ever spurious* — a genuinely uncovered new
line fails, which is the point.

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

Profiles are written to `${TMPDIR:-/tmp}/fishhawk-patchcov-$$` — keyed by
PID exactly as the container-lease files are, and OUTSIDE the repo. Two
concurrent `scripts/test verify` invocations can therefore never share,
corrupt, or delete each other's profile, no fixed in-repo filename is
ever clobbered, and no artifact is left in the working tree. The dir is
swept by the single EXIT handler (`EXIT_TRAP`), which also reaps the
shared Postgres container only when this invocation actually recorded a
lease.

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
| profile scratch dir uncreatable / no profiles emitted | shell |
| git absent, non-git or bare root, invalid `--diff-base` override, unresolvable base ref, no merge base | Python (`GitSkip`) |
| no changed Go files, no coverable new statements, sub-floor diff | Python |

Only the Python gate's below-threshold verdict is allowed to fail
verify. In COMBINED mode (`--threshold` AND `--diff-base`) a git failure
skips only the patch gate — the aggregate gate still runs and decides
the exit code.

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
sourcing `scripts/test` lib-only with an overridden `ROOT`). Both are
standalone in the `scripts/test-*` style and must be green; run them
when touching `scripts/check-coverage.py` or `scripts/test`. CI's
aggregate invocation is unchanged — diff mode is inert without
`--diff-base` — and `.github/workflows/**` is untouched (human-led).

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
