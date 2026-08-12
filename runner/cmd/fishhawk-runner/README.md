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

### Lineage-lock holder contract ([#2545](https://github.com/kuhlman-labs/fishhawk/issues/2545))

`lockholder.go` owns the rule for a CONTENDED lineage lock whose recorded pid is still alive. Before #2545, a live pid was automatically legitimate: cancelling a run left the detached `fishhawk-runner` it spawned alive, still holding the lockfile, so every later same-lineage dispatch failed category-A/C until an operator killed the process by hand.

`evictTerminalLockHolder` will not signal ANYTHING until all five of these are PROVEN, in order. Each refusal is independently observable and separately tested, and every one of them leaves the holder alive and the lockfile intact:

1. a backend client was threaded in (`acquireLineageLock`'s `client` param; nil → refuse);
2. the lockfile records a holder run id on line 2 (`readLockRunID`);
3. the backend returns a state for that run — a read error, a 404 and an empty state all refuse;
4. that state is EXACTLY `cancelled` (`runStateCancelled`). `running`, `failed` AND `succeeded` all refuse: a run flips terminal in the backend BEFORE its runner has necessarily finished post-stage work (PR push, trace upload), so only an explicit operator cancel is evictable;
5. the pid's REAL argv, read via `ps -o args=`, identifies it as a `fishhawk-runner` carrying `--run-id <that run id>` (`runnerIdentityMatches`).

(5) is the AUTHORIZING control — nothing else decides whether a signal may be delivered. It matches on TOKEN BOUNDARIES, never substring containment: argv is split on whitespace, **`argv[0]` alone** must match `fishhawk-runner` on a path/basename boundary, and the `--run-id` token must be exactly equal with the id as the IMMEDIATELY FOLLOWING token. Proving the binary from `argv[0]` — the executable the kernel started, which `ps -o args=` renders first — rather than from any token is load-bearing: an any-token rule would authorize an unrelated process that merely mentions the runner name as an ARGUMENT (`/bin/sleep fishhawk-runner --run-id <holder id>`), inverting the requirement that an unprovable or recycled holder pid stay untouched. The fused `--run-id=<id>` form and any longer token merely embedding the name or the id are refused. `runIDArgFlag` is the single source of truth the matcher derives from; the cross-module pin on the MCP side's `composeRunnerArgv` is `TestComposeRunnerArgv_RunIDFlagAndValueAreAdjacentTokens` (the runner-side test cannot see a reshape there — no dependency edge either way).

The group test inside `signalLockHolder` is only a SIGNAL SELECTOR: a group-leading pid (every runner, spawned `Setpgid`) is signalled as a group so the agent subprocesses go too; a non-leader gets a single-pid signal — never `-pgid`, which on a non-leader is a group the holder merely belongs to, up to and including our own (`TestSignalLockHolder_NonGroupLeaderSignalsOnlyThatPid` arranges a leader + a joined member so that regression kills the leader, not the test runner). A non-positive pid is refused ESRCH before it can reach `kill(2)`, where `0` means this process's whole group and `-1` every process we may signal. On Windows the whole path degrades to refuse.

Once authorized (`terminateLockHolder`): TERM → `lockHolderTermGrace` → KILL → `lockHolderKillGrace`. A holder that survives BOTH leaves the lockfile INTACT and fails loud — never break a lock a live holder still owns. A holder that exits NATURALLY in the window between the identity check and either signal fails the delivery with **ESRCH**; that counts as SUCCESS, not as a failure (`holderAlreadyExited`), because preserving the lockfile over an already-dead pid would re-create the #2545 wedge inside its own fix. Both halves of that test are load-bearing and pinned by `TestAcquireLineageLock_HolderExitsBeforeSignalLands`: the errno must be exactly ESRCH (an EPERM or the Windows `errSignalUnsupported` stub keeps the refusal), and liveness is RE-PROBED so an ESRCH reported for a pid that is still alive still refuses. On confirmed death the runner emits `{"event":"lineage_lock_evicted","root":…,"holder_pid":…,"holder_run_id":…,"holder_state":"cancelled"}`, reclaims the lockfile and retries the acquire.

Every refusal error names the holder pid, its run id and its state, so an operator driving over HTTP MCP can diagnose the wedge without `ps`.

The state vocabulary is read from ONE definition (`runStateCancelled` in `lockholder.go`). The runner and backend are separate Go modules with no dependency edge, so the backend half of that pin is `TestGetRun_CancelledState_WireLiteral` in `backend/internal/server/runs_get_test.go`, which serialises a cancelled run and asserts the wire carries this exact literal. A vocabulary drift turns that test RED instead of silently making this reclaim a permanent no-op.

In practice a TERMed runner exits gracefully and its deferred release removes the lockfile, so this path is normally never reached — it is the net for the SIGKILL / host-crash / cancel-from-a-different-MCP-process cases the MCP-side reap (`backend/internal/mcpserver/detached_registry.go`) cannot cover.

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

Trace events: `verify_run` per attempt (with committed `head_sha`) + one `verify_summary` (`{outcome: passed|failed|skipped, iterations, max_iterations}`, emitted exactly once) + `verify_fix_reinvoke_error` per failed fix-Invoke attempt (#804) + `verify_infra_flake_retry` when an infrastructure failure is absorbed (once per stage). The absorb matcher is `isVerifyInfraFailure` (#972, widened by #2645), which claims two DIFF-INDEPENDENT signature classes: a testcontainers container-start timeout (`isTestcontainersStartFlake` — `context deadline exceeded` + a container-start marker) and golangci-lint global-lock contention (`parallel golangci-lint is running`, the signature two concurrent local implement stages produce). A THIRD class — a `signal: <name>` death (`isReverifyInfraFailure`) — is admitted at the pre-push strict re-verify ONLY, see below.
The flake absorption covers both gates: the fix loop repeats the iteration in place without invoking the fix agent or advancing `iter`; the single-shot gate re-runs `runVerifyCommittedTree` once against the same throwaway headSHA before the reset, and classifies a still-matching second failure as `gitops.ErrVerifyInfraFailure` (**category-C**) rather than `ErrCommittedTestsFailed`.

Log lines: `verify_fix_reinvoke`, `verify_fix_reinvoke_error`, `verify_fix_skipped`, `verify_infra_flake_retry`.

The fix re-invocation is bounded against transient agent-API failures by `maxFixInvokeInfraRetries` (=2) in-place retries per outer iteration that do NOT advance the iteration counter; exhaustion is a non-blocking skip (`outcome=skipped`), never category-A.

**Terminal, non-compounding (DECISION c2):** the loop lives OUTSIDE the ADR-023 self-retry `for{}` loop so exhaustion can never call `RetryStage`; total agent invocations are capped at `max_iterations + 1`; fix-iteration `Result.Events` + tokens fold into `res` and `EmitStage` fires after the loop (honest ADR-030 cost). Wall-clock bound: `(max_iterations+1) × (executor.timeout + verify.timeout)`.

**Single-shot committed-tree gate (#802):** `runVerifyGateCommitted` is the `max_iterations == 0` sibling on the implement push path — it runs the verify command ONCE against the committed scope-only worktree (reusing `StageScoped`/`commitVerifyWIP`/`runVerifyCommittedTree`/`gitResetSoftHEAD1`) and demotes to **category-B** via `gitops.ErrCommittedTestsFailed`, the language-agnostic twin of the #728/#800 Go gate.
Pre-commit infra errors are non-blocking skips, while a post-commit `gitResetSoftHEAD1` failure is fatal. Only `--no-pr` and non-implement stages keep the working-tree #441 `runVerifyGate` (category-A).

**Verified-SHA invariant (#960):** both gates return the verified throwaway commit's tree hash (`gitRevParseTreeOf`, fail-closed when a pass's tree can't be resolved). `runVerifyCommittedTree` returns an explicit `passed|failed|skipped` outcome string (replacing the lossy ok bool) and stamps `verify_run` with `tree_sha`.
The pre-push `VerifyCommit` closure in `openPRAndShipArtifact` enforces tree-hash equivalence — `verified_tree_match` / `verified_tree_mismatch` / `pushed_tree_reverified` log events, with a single strict re-verify on mismatch; `gitops.ErrPushedTreeNotVerified` → category-B before the push.
The mismatch re-verify emits its own `verify_run` log event, pass or fail, and on pass rebinds the stamped `verified_tree_sha` to the re-verified pushed tree so `verified_tree_sha == tree_sha` holds unconditionally while `pushed_tree_reverified` carries both trees (#969).

**Pre-push strict-re-verify infra absorb (#2645).** Both recorded occurrences of a discarded implement pass died here: the committed-tree gate passed, an unrelated merge advanced the base so the real commit's tree differs, and the ONE strict re-verify then failed for an infrastructure reason (a sibling process holding golangci-lint's global lock; a segfaulting `git rev-parse` in an untouched test) — returning `ErrPushedTreeNotVerified`, the one non-retryable category. The re-verify now gets the gates' once-per-stage absorb: a failure matching `isReverifyInfraFailure` is re-run ONCE against the same real headSHA (`verify_infra_flake_retry` log line, each re-verify emitting its own `verify_run`), and only if the retry also fails for an infra reason does the hook return `gitops.ErrVerifyInfraFailure` → **category-C**, retryable in place via `fishhawk_retry_stage`. A failure matching no infra signature still returns `ErrPushedTreeNotVerified` → category-B, and the push decision is unchanged in every case: origin is touched only on an explicit `passed` outcome.

`isReverifyInfraFailure(output, signalRuleAdmissible)` is `isVerifyInfraFailure` PLUS a `signal: <name>` death, and the signal rule is confined to this one site AND gated on `reverifySignalRuleAdmissible`. The "same tree already verified" precondition the #2645 approval condition asked for is UNSATISFIABLE here by construction — the strict re-verify runs only when the gate-verified tree and the committed tree DIFFER — so the admissibility check substitutes the strongest evidence the site does carry: the verified-tree → committed-tree delta must be RESOLVED, non-empty, and touch NO declared scope file (rename records contribute both paths), i.e. every differing path came from the advanced base and the change's own content is byte-identical to what already ran this verify command to completion. When the delta touches a scope file, is unresolved (`tree_delta_unresolved`), or is empty, the signal rule is refused and the failure keeps today's category-B classification. At a committed-tree gate, with no prior pass at all, a signal death never buys the infrastructure claim — a diff can hang until a supervisor kills it, OOM, or crash a subprocess.

Two residuals, both accepted and both bounded to retry churn (never a verified-tree bypass — the push reads the verify OUTCOME, not the classifier, and a retry re-runs the same command): (1) the admissibility check bounds the CHANGE as the cause, not the freshly merged BASE or its interaction with the change, so `signal: killed` / `signal: terminated` can still be rendered when a supervisor kills a verify the base+change combination made genuinely slow or hung; (2) the classifier's input is UNTRUSTED — verify output is influenced by the diff, so a test the agent authored can print the lint-lock line or a `signal: <name>` rendering and steer its own genuine category-B failure to category-C. A stage going category-C REPEATEDLY on either signature warrants inspecting the change and the merged base for a hang, and inspecting the failing output for a self-produced signature, rather than blaming the host.

Both pre-push failure messages LEAD with the failing command and a bounded excerpt of its output (`verifyFailureExcerpt`), ahead of the tree-mismatch and `N file(s) excluded as scope drift` accounting that read as a scope problem they were not; the full output is still appended unabridged. `pushFailureCategory` (the extracted push-failure classifier) checks `ErrVerifyInfraFailure` FIRST so the category-C verdict survives an error that also wraps a category-B sentinel.

**Where the test coverage stops (stated plainly rather than implied).** Every #2645 test ends at `pushFailureCategory(err) == "C"` inside the runner — that a category-C stage is then ADMITTED by `fishhawk_retry_stage` is a property of `backend/internal/server/retry.go` (`C → 200`, re-open on the same A/C path), asserted here from code reading with NO cross-layer test in this change. The runner-side claim is: the failure record this path produces carries category `C`. The recovery half is the backend's existing, separately-tested contract, and a cross-layer pin (runner failure record → retry admission) is a follow-up, not something this change verifies.
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
deliberately no "only if there was a diff" guard. Only a **genuinely-empty
diff** (no lines added against the merge base) emits an explicit
measured-with-zero result, because the backend treats an ABSENT signal as a
violation and a legitimately vacuous stage must not be able to reach that
state. (The customer command is not run in that case: there is nothing for
it to measure.) A stage that DID add lines but whose report measured none of
them **fails closed**, naming the unmeasured changed files (E46.7) — see
"Zero WITH added lines is a fail-closed failure" in
`runner/internal/diffcov/README.md`. A clean work tree is **not** proof of
no added lines: HEAD may already be ahead of the pinned merge base (work
committed on the run branch), so when nothing new is staged the runner
measures the committed diff against the merge base instead of
short-circuiting — only a genuinely-empty change set stays the vacuous pass.

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

### Gate-env allow-list

`gateenv.go::sanitizedGateEnv` builds the child env for every gate exec site
(`runBoundedGateCommand`, the compile/vet/test gates, and the preview probe)
under a **default-deny allow-list**: an entry survives only if it is a system
essential (`gateEnvAllowExact`: PATH/HOME/locale/temp/CC/CXX), a Go
toolchain/runtime name (`gateEnvAllowGo`), or a `CGO_*`/`LC_*` prefix
(`gateEnvAllowPrefix`). A secret added to the runner later is dropped
automatically because it is not on the list.

The Go rung is an explicit NAME set, **not** a bare `GO` prefix. The old prefix
also admitted `GOOGLE_API_KEY` / `GOOGLE_APPLICATION_CREDENTIALS` and every
other `GOOGLE_*` value, and the URL-userinfo redaction (`redactGoEnvUserinfo`)
does not cover a bare API key — it only rewrites URL-shaped values — so those
credentials passed into gate children verbatim (#2504). `gateEnvAllowGo` is the
union of `go env` and the GO-prefixed names in `go help environment`, plus the
runtime knobs (`GOMAXPROCS`/`GOGC`/`GOTRACEBACK`/`GOMEMLIMIT`) the bare prefix
used to admit. `GOLANGCI_LINT_CACHE` is on the list, but `withIsolatedLintCache`
strips any inherited value and appends its own per-invocation cache dir after
sanitization (#1796), so admitting it is behaviour-neutral on the runner.

Layered on top is a known-secret denylist (`gateEnvDeny`) plus a `GOOGLE_` deny
**prefix** (`gateEnvDenyPrefix`) — belt-and-suspenders that keeps the Google
credentials out even if a future allow-rule re-widens. There is no Go toolchain
variable named `GOOGLE_*`, so the prefix is unambiguous.

The CLI's `verifyEnv*` copy in `cli/cmd/fishhawk/doctor_verify.go` guards the
`doctor` verify-command rung on the OPERATOR's own machine and is kept in
lockstep with this file — the same allow-exact, allow-Go, allow-prefix,
deny-exact and deny-prefix sets — by `TestGateEnvListsMatchCLICopy`, which reads
the CLI source through the workspace root and fails on any divergence. **Adding
a Go variable means adding it to BOTH copies**, or that test fails.

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
as an error and the call site demotes the stage category-B
(`TestRunDiffCoverageGate_PostCommitResetFailureFatal` provokes the branch
by having the coverage command plant the branch's ref lock, and asserts HEAD
is left on the throwaway commit — which is precisely why the error must
reach the call site). Both cleanup
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
an untracked added file. The **same** pinned merge base backs the
clean-tree (`!committed`) path: when nothing new is staged the runner runs
`ChangedLines` against it in `repoDir` and, on a non-empty committed diff,
measures it rather than emitting a false measured-zero — no throwaway commit
is made on that path, so there is nothing to `reset --soft`.

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

**`run()`-level wiring is pinned end to end.** A mis-threaded position in
`fetchPromptToFile`'s return tuple or a wrong stage-type string compiles
fine and silently emits NOTHING — the absent-signal false RED the rest of
the design works to avoid. `TestRun_DiffCoverage_EmitsEventFromFetchedPrompt`
drives a fetched prompt carrying the config through `run()` to a
`diff_coverage` event in the uploaded bundle, and
`TestRun_NoDiffCoverage_EmitsNoEvent` pins the other half: a stage with no
declared constraint runs no command and emits no event.

## PR-open checkpoint resume (E48.46 / #2169)

An implement stage that gets all the way through the agent, the committed-tree gates, and `CommitAndPush` — and then fails opening the PR (or shipping the artifact) against a forge that is down — used to cost a full agent re-run on `retry_stage`. The commit was already on the run branch; only the last two API calls were missing. This closes that with a durable, past-agent CHECKPOINT that reuses the #1231 zero-re-run machinery end to end rather than adding a second recovery flow.

**Arm after push.** `openPRAndShipArtifact` writes the pushed coordinates (`branch`, `head_sha`, `base_sha`, `verified_tree_sha`) into a caller-supplied `pushCheckpoint` out-parameter at exactly one point: after `CommitAndPush` succeeds and the standalone open-PR path is selected — past the `--no-pr`, `NoChanges`, fix-up, decomposed-child, and scope-completeness-park early returns, all of which own their own recovery. It emits `pr_open_checkpoint_armed` so a later trace shows why a retry resumed. `run()` then passes the armed checkpoint into `reportPullRequestFailure`, so the `{outcome:"failed"}` report carries it. **A failure BEFORE the push (mint, commit, or the push itself) leaves it unarmed** — that is the control that stops an un-pushed stage claiming a resumable branch, and it keeps every pre-push failure body byte-identical to the pre-#2169 shape (all four wire fields are `omitempty`).

**Resume at the pre-agent short-circuit.** The backend's `resolvePushCheckpointResume` serves the ordinary held-commit fields plus `held_commit_resume_kind:"pr_open"`, and the runner takes the SAME `if exemptOpenPR` branch #1231 uses — before the prompt file is read and the agent invocation is wired, which is what structurally guarantees the agent is never spawned. `openHeldCommitPR` then branches on the kind: an empty kind is the legacy exempt path, byte-identical to before (`scope_completeness_pr_opened`, `scope_exempt_open_pr`, no tip probe, no checkpoint on its failure report); `pr_open` gets the two extra behaviors below. Only the idempotent adopt-then-create `OpenPR` (#2167) is re-attempted, which also recovers the ship-failed case — the adopt-by-head arm returns the PR the previous attempt opened, so no duplicate is created.

**Remote-tip guard (`pr_open` only).** Unlike an exempt park, a checkpointed commit passed no operator gate, so before touching the forge the resume re-proves the run branch still points at the checkpointed head via `gitops.RemoteBranchTip`. A tip that is MOVED (e.g. by `fishhawk_reset_run_branch`), ABSENT, or UNREADABLE fails the stage category C having opened NO PR — including on an `ls-remote` error, because a transient probe fault is not evidence the tip is intact and the retry costs nothing when no agent runs. Without it the stage could ship a `pull_request` artifact whose `head_sha` claims a tree the branch no longer carries.

**Repeatable resume.** A resume that itself fails RE-reports the checkpoint on its failure report. This is what makes a ~30-minute outage spanning several `retry_stage` attempts safe: without it, the first failed resume would write a checkpoint-less `pull_request_failed` that becomes newest, and the next retry would silently degrade back to a full agent re-run.

**Skew is safe in both directions.** Every wire field is optional/`omitempty`. An old runner against a new backend ignores the unknown kind and takes the #1231 exempt path — same branch, same PR, just without the tip guard. A new runner against an old backend never sees a resume kind and behaves exactly as today.
