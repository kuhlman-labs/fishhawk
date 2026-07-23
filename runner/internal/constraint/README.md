# runner/internal/constraint

Workflow-spec constraint evaluator (`forbidden_paths`, `allowed_paths`, `max_files_changed`, `required_outcomes`) — the runner-side copy, giving the agent immediate in-band feedback. `backend/internal/policy/` is the backend-side source of truth, which emits the chained `policy_evaluated` audit entry.

## Trace-ingest wiring

The runner emits a `git_diff` event in the bundle; the backend trace handler calls `bundle.ExtractDiff` + `policy.EmitEvaluation`; violations transition the stage to `failed-B` instead of `awaiting_approval`.

The runner emits the `git_diff` event whenever `--check-base-ref` is set (decoupled from `--constraints-file` per #247), so the bundle has the data the backend needs even when the customer skips in-band enforcement.

## Diff form (#296)

`computeAndEmitDiff` stages everything with `git add -A` then runs `git diff --cached --name-status -z <base>` — the only form that catches both modified files and freshly-created files in one shot, regardless of whether the agent committed its own edits.

Pre-#296 the form was `<base>...HEAD`, which only saw committed state; agents leaving edits unstaged (the common case for Claude Code) shipped empty diff events and every PR silently failed `tests_added_or_updated` / `max_files_changed`.

## Patch payload (#585)

`computeAndEmitDiff` additionally runs `git diff --cached <base>` (no `--name-status`) via `gitdiff.Runner.RunPatch` to capture the full unified-diff hunk text into the `git_diff` event's optional `patch` field (size-capped at 256 KiB with a truncation marker; `patch_truncated` flags the cap).

The patch is **content for the implement-review prompt only** — `policy.Evaluate` never reads it, so constraint evaluation is unaffected. It rides inside the event payload, so the runner's per-event `RedactDefault` pass redacts secrets in the redacted bundle variant automatically.

Patch-compute failure degrades gracefully: the `git_diff` event still ships with the load-bearing name-status list, just without the patch.

## Constraints source (#283)

The backend trace handler reads constraints from `runs.workflow_spec` (cached at run-create by the dispatcher; migration 0019).

Pre-#283 it refetched from GitHub using `runs.workflow_sha` as the contents-API ref — but that's a blob SHA, not a commit ref, so the call 404'd in production and the policy section stayed "pending" forever. The approval handler's `fetchGateForStage` reads from the same cache for the same reason (the role-check was silently bypassed by the broken refetch).

## `policy_evaluated` audit entry — always written

The backend **always** writes a `policy_evaluated` audit entry — including on the empty-diff, no-constraints, and skipped paths.

When evaluation can't run meaningfully, the audit payload's `skip_reason` field (`spec_unavailable` / `spec_unparseable` / `workflow_not_in_spec` / `stage_not_in_spec` / `no_diff_in_bundle`) carries the structured cause; `<PolicySection>` renders a dedicated "Policy evaluation skipped · <reason>" arm instead of a misleading pass state.

## Deferred outcomes (#297)

When a `required_outcomes` entry can't be asserted at trace-upload time because no signal is available yet (today: `ci_green` — CI hasn't run against the just-opened PR), the policy engine skips the violation and lists the outcome in `payload.deferred_outcomes`.

Branch protection (#251 / ADR-017) is the actual gate at merge time, so the policy engine's vote there is duplicative; pre-#297 it produced a false-positive violation on every Fishhawk-managed PR. The SPA renders an inline "Deferred to branch protection: ci_green" note next to the pass state.

## `tests_added_or_updated` heuristic (#610, surfaced by #601)

`isTestPath` recognizes more than Go `*_test.go` — it covers JS/TS `.test`/`.spec`, Python `_test.py`/`test_` prefix, Ruby/Elixir `/spec/`, `test/` & `tests/` dirs, and non-Go script conventions (`scripts/test*`, a base name of `test`/`tests`, or a `test-` prefix — e.g. `scripts/test-dev`, which previously matched no clause).

The outcome is also **scoped**: a non-empty diff that touches no unit-testable source (docs/scripts/config only, per the `diffTouchesTestableCode` source-extension allowlist) is vacuously satisfied rather than failed-B, while an *empty* diff still fails (the `len(ChangedFiles) > 0` guard preserves the "stage produced nothing" signal).

The allowlist fails open: an unrecognized source language reads as no-testable-code and passes, never a new false-fail.

## Generated-path allowlist for `max_files_changed` (#2054)

`max_files_changed` counts only files that are NOT generated or vendored: `IsGeneratedPath` exempts sqlc-generated db packages (a `.go` file under a `db/` directory) and vendored dependencies (anything under `vendor/`). `CountedFileCount` returns the un-exempted count, and `checkMaxFiles` compares that (and reports it in the violation `Detail`) against the cap — so a diff of 5 hand-written files plus a regenerated `db/queries.sql.go` counts as 5, not 6.

The db exemption mirrors CI's coverage exclusion (`scripts/check-coverage.py --exclude '/db/'`), narrowed to `.go` files. Only `max_files_changed` is affected — `forbidden_paths`/`allowed_paths` still match the full file set, so a generated file under a forbidden glob is still a violation. No `vendor/` directory exists in the repo today; that branch is forward-looking and exercised only by unit tests.

## Lockstep with the backend copy

Both copies (runner `constraint.go` + backend `policy.go`) carry identical `isTestPath`/`diffTouchesTestableCode`/`checkRequiredOutcomes`/`IsGeneratedPath`/`CountedFileCount` logic so the runner's in-line verdict and the backend re-eval agree. Change them together.

## `verification_reported` — backend-authoritative (#1886 / ADR-059)

The workflow-v1.5 `verification_reported` required outcome is **skipped** by this runner-side evaluator: it never emits a violation for it, and specifically not the `default:` branch's `unknown outcome` (which would fail every opted-in run as category-B before the agent's work was even verified).

The reason is ordering, not policy. This in-line check runs on the implement push path **before** both committed-tree verify gates (`runner/cmd/fishhawk-runner/main.go`: the verify-fix loop and the single-shot gate both come later), so no verify result exists locally at check time — there is nothing truthful the runner could assert.

The backend owns the outcome instead: `reEvaluatePolicy` derives a `policy.VerificationSignal` from the uploaded bundle's `gate_evidence` event (verify_summary, else the last non-superseded verify_run) and evaluates fail-closed — absent, `failed`, and `skipped` each violate. See `backend/internal/policy/README.md`.

This is the one deliberate asymmetry with the lockstep rule below; the rest of `checkRequiredOutcomes` stays identical between the two copies.

## `diff_coverage` — backend-authoritative (#1888 / ADR-059)

The workflow-v1.6 `diff_coverage` constraint kind is **carried but not evaluated** by this runner-side evaluator: `Constraints.DiffCoverage` exists so the struct stays mirrored with the backend's `policy.Constraints` (and so a caller threading spec constraints through here cannot silently drop it), but `Evaluate` emits no violation for it.

Same rationale as `verification_reported`: this in-line check fires on the implement push path, BEFORE the coverage command has run — the measurement happens after the committed-tree verify gate, in `runner/cmd/fishhawk-runner/main.go`'s `runDiffCoverageGate`. There is nothing truthful the runner could assert locally, and asserting anything would either fail every opted-in run before its work was measured or invent a pass. The backend re-evaluates from the uploaded bundle's `gate_evidence`, where the measurement IS available.

The type is a LOCAL mirror (`constraint.DiffCoverage`), not an import: this package is deliberately dependency-free on the backend's policy package.
