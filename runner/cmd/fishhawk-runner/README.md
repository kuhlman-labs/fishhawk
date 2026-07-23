# runner/cmd/fishhawk-runner

The runner binary entrypoint: flag parsing in `flags.go`, stage dispatch in `main.go`. Operator-facing inputs and the action contract are in `runner/README.md`; this file covers `main.go`-level mechanics.

## Per-run working-tree isolation (E22.X / #1137)

`worktree.go` provisions a per-lineage git worktree so concurrent runs on one local host never share a working tree:

- `lineageRoot` — the parent id for a decomposed child, else the run's own id.
- `worktreesDir` — resolves `<git-common-dir>/fishhawk-worktrees` via `git rev-parse --git-common-dir`.
- `provisionLineageWorktree` — `git worktree add --detach … HEAD`, reused across siblings of one lineage.
- `acquireLineageLock` — O_EXCL lockfile beside the worktree; a live-pid conflict is a category-A fail-loud, a stale pid is reclaimed.

Wired in `main.go::run` right after the prompt fetch resolves `decomposed_from_run_id`: it relocates `cfg.workingDir` into the worktree so every downstream `repoDir := cfg.workingDir` git op is isolated.

Local loop only — GitHub Actions is per-job-isolated by `actions/checkout`.

The relocation-broken `diff_summary` seam is closed on the `git_diff` wire event (`insertions`/`deletions`, mirrored in `backend/internal/bundle/bundle.go::gitDiffPayload`) and read in `backend/cmd/fishhawk-mcp/run_stage.go` — the MCP server stays worktree-unaware. See the `docs/ARCHITECTURE.md` §4 lifecycle bullet. This extends ADR-035's branch-ownership invariant to tree-ownership.

## Acceptance executor (E31.7 / #1535, ADR-049 #3/#5)

`acceptance.go` — verdict capture/validation/redaction + evidence event for the `stageType=="acceptance"` branch in `main.go::run`.

**Prompt wire:** the acceptance-stage prompt response carries `egress_target_hosts` (the full spec list — proxy allow-list input) + `acceptance_criteria_ids` (the plan-criterion join keys), decoded onto `upload.FetchedPrompt` by tag.

**Pre-agent containment (main.go):**

- No MCP token (`acceptance_no_mcp_token` event — ADR-050 #2).
- `inv.WorkingDir` → a fresh empty `os.MkdirTemp` dir. This is diff-withholding per ADR-049 #4 plus accidental-write hygiene ONLY, not an authority boundary — the boundary is credential-free env + no commit/push/PR path.
- `egressproxy.Start(BuildAllowlist(...))` — a start error fails category-C BEFORE any agent spawn.
- `acceptenv.Env` → `Invocation.BaseEnv` (the `runner/internal/agent` seam): a non-nil BaseEnv REPLACES the `os.Environ()` seed in both the claudecode and codex adapters, with the API-key + `Env` overlays still applied on top; nil preserves the inherit-parent-env behavior byte-for-byte, so every non-acceptance spawn is unchanged. Refused passthroughs → `acceptance_env_refused`.
- `inv.JSONSchema = acceptanceVerdictJSONSchema` (claudecode structured output; other backends use the file fallback).

**Verdict file path (#1780):** the buildAcceptance output contract NAMES the run/stage-keyed `/tmp/fishhawk-acceptance-<run>-<stage>.json` path (`prompt.AcceptanceVerdictPath(runID, stageID)`, threaded via `Trigger.AcceptanceRunID`/`AcceptanceStageID`). The runner's `acceptanceVerdictPath` reads that FIRST, falling back to the legacy fixed `/tmp/fishhawk-acceptance.json` (`legacyAcceptanceVerdictPath` ↔ `prompt.LegacyAcceptanceVerdictPath`) when a trigger threads no ids. The keyed and legacy format strings are byte-identical across the two modules.

**Post-agent:**

- `captureAcceptanceVerdict` — StructuredOutput > file.
- `validateAcceptanceVerdict` — backend-`acceptanceBody`-mirrored rules + served-criteria-id membership, fail-closed on unknown ids. Missing verdict → category-B `acceptance_verdict_missing`; invalid → category-B `acceptance_verdict_invalid`. A VALID `failed` verdict is NOT a runner failure — routing is E31.8.
- `redactAcceptanceVerdict` BEFORE embed/ship.
- `composeAcceptanceEvidence` appends the `acceptance_evidence` event pre-`PackBytes` (both bundle variants).

**Ship:** after the trace upload, `upload.Client.ShipAcceptance` POSTs the redacted verdict to `/v0/runs/{run_id}/acceptance?stage_id=…` signed with the re-issued run key (ShipPlan-modeled retries; `ErrAcceptanceInvalid` → category-B, other failures → category-C).

Shape lockstep (schema ↔ runner validator ↔ backend validator) is guarded by `TestAcceptanceVerdictSchema_LockstepWithValidator`.

## Committed-tree verify-fix loop (#651)

`main.go::runVerifyFixLoop` — the bounded evaluator-optimizer fix loop on the implement push path, enabled by `executor.verify.max_iterations > 0` (default 0 = the single-shot #441 working-tree gate behavior).

Helpers:

- `runVerifyCommittedTree` — isolated `git worktree add --detach` at the throwaway-commit SHA, reusing the #728/#800 pattern + `runVerifyGate`'s process-group SIGKILL.
- `commitVerifyWIP` — throwaway scope-only commit.
- `gitResetSoftHEAD1` — undo, preserving working-tree edits + index.
- `verifyFixPrompt` — fix-iteration prompt embedding the captured output.

Trace events: `verify_run` per attempt (with committed `head_sha`) + one `verify_summary` (`{outcome: passed|failed|skipped, iterations, max_iterations}`, emitted exactly once) + `verify_fix_reinvoke_error` per failed fix-Invoke attempt (#804) + `verify_infra_flake_retry` when a testcontainers start-timeout flake is absorbed (#972, once per stage; the matcher `isTestcontainersStartFlake` requires `context deadline exceeded` + a container-start marker).
The flake absorption covers both gates: the fix loop repeats the iteration in place without invoking the fix agent or advancing `iter`; the single-shot gate re-runs `runVerifyCommittedTree` once against the same throwaway headSHA before the reset.

Log lines: `verify_fix_reinvoke`, `verify_fix_reinvoke_error`, `verify_fix_skipped`, `verify_infra_flake_retry`.

The fix re-invocation is bounded against transient agent-API failures by `maxFixInvokeInfraRetries` (=2) in-place retries per outer iteration that do NOT advance the iteration counter; exhaustion is a non-blocking skip (`outcome=skipped`), never category-A.

**Terminal, non-compounding (DECISION c2):** the loop lives OUTSIDE the ADR-023 self-retry `for{}` loop so exhaustion can never call `RetryStage`; total agent invocations are capped at `max_iterations + 1`; fix-iteration `Result.Events` + tokens fold into `res` and `EmitStage` fires after the loop (honest ADR-030 cost). Wall-clock bound: `(max_iterations+1) × (executor.timeout + verify.timeout)`.

**Single-shot committed-tree gate (#802):** `runVerifyGateCommitted` is the `max_iterations == 0` sibling on the implement push path — it runs the verify command ONCE against the committed scope-only worktree (reusing `StageScoped`/`commitVerifyWIP`/`runVerifyCommittedTree`/`gitResetSoftHEAD1`) and demotes to **category-B** via `gitops.ErrCommittedTestsFailed`, the language-agnostic twin of the #728/#800 Go gate.
Pre-commit infra errors are non-blocking skips, while a post-commit `gitResetSoftHEAD1` failure is fatal. Only `--no-pr` and non-implement stages keep the working-tree #441 `runVerifyGate` (category-A).

**Verified-SHA invariant (#960):** both gates return the verified throwaway commit's tree hash (`gitRevParseTreeOf`, fail-closed when a pass's tree can't be resolved). `runVerifyCommittedTree` returns an explicit `passed|failed|skipped` outcome string (replacing the lossy ok bool) and stamps `verify_run` with `tree_sha`.
The pre-push `VerifyCommit` closure in `openPRAndShipArtifact` enforces tree-hash equivalence — `verified_tree_match` / `verified_tree_mismatch` / `pushed_tree_reverified` log events, with a single strict re-verify on mismatch; `gitops.ErrPushedTreeNotVerified` → category-B before the push.
The mismatch re-verify emits its own `verify_run` log event, pass or fail, and on pass rebinds the stamped `verified_tree_sha` to the re-verified pushed tree so `verified_tree_sha == tree_sha` holds unconditionally while `pushed_tree_reverified` carries both trees (#969).
See `docs/ARCHITECTURE.md` §4's Verified-SHA invariant bullet.

Full behavior in `docs/ARCHITECTURE.md` §4 step 5; the prompt-wire seam (`verify_command` / `verify_timeout_seconds` / `verify_max_iterations`, #504/#651) is in `backend/internal/prompt/README.md` ("Verify wire").

## Policy/review diff base anchoring (#1294 / #1801 / #1975)

The `git_diff` event is the SINGLE source for BOTH the backend policy gate (`policy_evaluated`, e.g. `max_files_changed`) AND the implement-review prompt patch. Both emitters — `computeAndEmitDiff` (the original) and `reemitScopedGitDiff` (the last-write-wins re-emit read by the reviewer, #1801) — resolve the commit-ish the staged index is measured against through the shared `resolveDiffBaseRef`, so the two paths cannot drift apart.

The diff is a 3-dot comparison against the run's fork point (`git diff --cached <merge-base>`), NOT the base-branch tip: a file the base branch added orthogonally after the run branched is absent from the merge-base tree, so it never shows as a phantom deletion inflating the file count (#1294 / ADR-043 rev 2).

**Re-anchoring to the CURRENT base tip (#1975).** When a long-lived run's base branch (`main`) advances remotely and a fix-up folds that advance into the run branch, the LOCAL base ref still points near the original fork point, so `merge-base(<local base>, HEAD)` resolves to that stale fork point and the staged diff folds in the base's unrelated content (the run-98020210 79-vs-45 category-B failure that disarmed re-review, #1932; the run-fc219396 phantom root-README review "scope drift"). To match what GitHub renders on the PR, `resolveDiffBaseRef` — when a remote is configured — first fetches the base branch's CURRENT tip from the remote (`gitops.FetchBaseTip`, the checkout-less sibling of the fix-up/child base fetch, wired through the `fetchDiffBaseTip` test seam) and merge-bases against THAT tip. The base branch name is derived from the base ref via `TrimPrefix(baseRef, "origin/")` (handling both the `main` and decomposition-child `origin/<shared-branch>` shapes).

Fail-open ladder (each rung is byte-identical to the prior behavior it degrades to):

1. **current-tip merge-base** — remote configured, fetch succeeds, and `merge-base(<fetched tip>, HEAD)` resolves → the re-anchored base. Emits `diff_base_reanchored` `{stage_id, base_ref, current_base_tip, merge_base}`.
2. **local-ref merge-base** — reached when the remote is UNCONFIGURED (a bare local repo / offline-by-design host: a configured mode, NOT a degradation — no fetch is attempted and NO `diff_base_refresh_degraded` is emitted), OR when a re-anchor was ATTEMPTED and failed (fetch error, or a fetched tip with no shared local history). Only the attempted-and-failed case emits `diff_base_refresh_degraded` `{stage_id, base_ref, detail}` (a distinct event from `merge_base_unresolved`). Falls through to `merge-base(<local base>, HEAD)`.
3. **tip baseRef** — `merge-base(<local base>, HEAD)` itself unresolvable (unrelated histories, shallow clone, ref not fetched) → logs `merge_base_unresolved` and returns the original tip baseRef (today's 2-dot behavior), never blocking the diff.

The `git_diff` event's `base_ref` label stays the human-meaningful fork-point label (`main`), and the recorded `base_sha` (the lineage/audit fork point — ADR-035) is UNTOUCHED: only the commit-ish the diff is measured against re-anchors. The event payload shape is unchanged, so backend decoders stay untouched.

## Forge selection: GitHub PR vs GitLab MR (ADR-058 / E45.5, #1859)

The implement-stage push + open path targets a **change-request forge** selected by `--forge` (default `github`):

- `--forge github` (default) — opens a GitHub **pull request** via the `prOpener` seam (`gitops.OpenPRClient`, `Authorization: Bearer`). Historical behavior; unchanged when the flag is omitted.
- `--forge gitlab` — opens a GitLab **merge request** via the `mrOpener` seam (`gitops.OpenMRClient`, `PRIVATE-TOKEN`). Requires `--gitlab-base-url` (e.g. `https://gitlab.com` or a self-managed `https://gitlab.example.com`) — there is **no gitlab.com default**, so a self-managed instance is a first-class target and `parseFlags` rejects `--forge=gitlab` without it. An unknown `--forge` value is rejected at parse time.

`openImplementChangeRequest` (main.go) is the single MR-vs-PR dispatch point: it maps the shared `gitops.OpenPRArgs` onto `gitops.OpenMRArgs` for GitLab — the `--github-repo`/`GITHUB_REPOSITORY` `owner/name` slug becomes the namespaced project path (`url.PathEscape`d into one `%2F`-encoded segment by `OpenMR`), `Head`→`source_branch`, `Base` (`resolveImplementBaseRef`)→`target_branch`, `Title`/`Body`→`title`/`description`. Both openers return the unified `gitops.OpenPRResult` (PR number / MR iid + web URL), so every downstream artifact-upload path stays forge-agnostic.

**Credential (`FISHHAWK_GITLAB_TOKEN`).** On the gitlab forge, `mintImplementToken` skips the GitHub App broker entirely and reads `FISHHAWK_GITLAB_TOKEN` — a GitLab access token with `api` scope (a group/project access token in v0). It authenticates both the run-branch push (via `PushToken` → `http.<host>.extraheader`) and the MR-create REST call. When unset, the stage fails with an actionable error naming the env var and scope. The secret is on the gate (`gateenv.go`) and acceptance (`acceptenv` package) **denylists**, so agent-authored gate code and the acceptance agent never see it.

**Runner-kind self-report.** `detectRunnerKind` reports `gitlab_ci` when `GITLAB_CI=true` or `CI_PIPELINE_ID` is non-empty (GitHub signals win when both are present; a bare `CI=true` still resolves `local`). The backend ignores the unrecognized value until #1861 adds the enum member, so shipping the detection first is additive-safe.

ADR-035 lineage/tree-ownership is git-level and remote-shape independent — the push machinery never parses the forge host — pinned by `gitops.TestCommitAndPush_ShapedRemote_LineageIsRemoteShapeIndependent`. The live GitLab walk with real credentials is tracked in #2032 (E45.18).

## Diff-coverage measurement (workflow-v1.6 `diff_coverage`, #1888 / ADR-059)

When the stage's prompt response carries a `diff_coverage` config, `run()`
calls `runDiffCoverageGate` on the implement path, **after** the
committed-tree verify gates (the tree is final and the agent has stopped
writing) and **before** `composeGateEvidence` folds the result into
`gate_evidence`. Implement is the only stage type that measures, which is
why the spec validator rejects `diff_coverage` on every other stage type
(#1888): an absent signal on a declared constraint is a violation, so
allowing the declaration elsewhere would be a guaranteed false RED.

**Measurement only.** It never touches `res.OK` / `res.FailureCategory`. A
coverage shortfall fails the stage through the backend's category-B
re-evaluation of the uploaded bundle, never as an opaque runner abort.

**It always emits evidence when the constraint is configured** — there is
deliberately no "only if there was a diff" guard. A stage that added no
coverable lines emits an explicit measured-with-zero result, because the
backend treats an ABSENT signal as a violation and a legitimately vacuous
stage must not be able to reach that state. (The customer command is not
run in that case: there is nothing for it to measure.)

**Containment (condition 6).** The customer command is untrusted input and
runs through `runBoundedGateCommand` — the SAME bounded-exec path the
committed-tree verify gate uses: a bounded child context, `Setpgid` plus a
`Cancel` that kills the whole **process group** (SIGKILL to the direct child
alone leaves grandchildren holding the inherited stdout pipe open and
`CombinedOutput` never sees EOF), the default-deny gate-env allow-list from
`gateenv.go` (no runner credential is visible to it), and a per-invocation
isolated lint cache. Do NOT add a second exec path for a new spec-supplied
command, and do NOT widen the env allow-list to make a particular coverage
tool work — that is a separate, explicit decision.

**Filesystem isolation: a throwaway checkout.** `runBoundedGateCommand`
contains the process and the environment; only a separate checkout contains
the **filesystem**. So the command runs in a disposable
`git worktree add --detach` checkout of the stage's committed scope-only
tree — the same isolation `runVerifyCommittedTree` gives the verify gate,
and what the schema and API documentation promise. Build artifacts, deleted
or modified sources, and the report itself land in the throwaway checkout
and are swept with it; the operator's real repository is untouched, and the
command cannot mutate the tree AFTER it was verified.

Materializing that tree reuses the #651 scaffolding — `StageScoped` +
`commitVerifyWIP` + `git reset --soft HEAD~1`. As in
`runVerifyGateCommitted`, a failed undo is **fatal**, not a measurement
failure: HEAD left on the throwaway commit would make the real commit stack
on top and push a WIP commit into the PR. `runDiffCoverageGate` returns it
as an error and the call site demotes the stage category-B. Both cleanup
commands (worktree removal, `reset --soft`) run on a **bounded context
detached from `ctx`'s cancellation** — a wedged coverage command is killed
by cancelling `ctx`, and the undo must still happen.

**One snapshot, pinned merge base.** The checkout is clean and detached at
the committed head, so `ChangedLines`' merge-base → work-tree diff taken
INSIDE it is exactly merge-base → committed tree. The merge base is resolved
to a SHA (`diffcov.MergeBase`) **before** the throwaway commit: that commit
advances whatever branch HEAD is on, so a `base_ref` naming that same branch
would otherwise merge-base to the throwaway commit itself and the
measurement would see zero added lines — a silent false vacuous pass. The
diff is computed before the command runs, so the report is not mistaken for
an untracked added file.

**Base ref resolution (condition 5).** `resolveDiffCoverageBaseRef` is the
named resolver: the spec's `base_ref` wins when declared; an OMITTED
`base_ref` falls back to `resolveImplementBaseRef`, the same
`--base-branch` > `GITHUB_REF_NAME` > `main` ladder the implement push
uses, so the measurement's base and the PR's base can never disagree. It
never returns empty, and `diffcov.ChangedLines` still fails closed on an
empty ref rather than trusting that.

**Report hygiene.** The report is written inside the throwaway checkout and
swept with it, so it never lands in the real working tree as untracked
litter or as an out-of-scope creation.

**Evidence.** One `diff_coverage` event carrying either the measurement or
a named failure reason. Every failure mode names what ran, its exit code,
and what was measured. `composeGateEvidence` pre-redacts the reason and
caps `uncovered_files` at `diffCoverageMaxUncovered`, like every sibling
field. The measurement itself lives in
[`runner/internal/diffcov`](../../internal/diffcov/README.md).
