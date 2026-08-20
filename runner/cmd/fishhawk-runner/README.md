# runner/cmd/fishhawk-runner

The runner binary entrypoint: flag parsing in `flags.go`, stage dispatch in `main.go`. Operator-facing inputs and the action contract are in `runner/README.md`; this file covers `main.go`-level mechanics.

## `--no-pr` contract (E22.8 / #406, scoped by [#2691](https://github.com/kuhlman-labs/fishhawk/issues/2691))

`--no-pr` is **per-case**, not global:

- **Standalone implement stage — supported, unchanged.** The runner skips the commit/push/PR-open sequence
  (`implement_pr_skipped`, `reason:"no_pr_flag"`) and **leaves the agent's work in the working tree** for the operator
  to commit. The bundle manifest stamps none of `push_and_open_pr` / `push_to_shared_branch` / `push_fixup`, so the
  trace upload settles the stage. Both the success path (the tree is left dirty) and the agent-failure path (no
  `working_tree_restored`, #953) are pinned by test.
- **Decomposition child or fix-up pass — REFUSED before the agent runs.** `run()` emits
  `{"event":"runner_failed","reason":"no_pr_unsupported_push_path","detail":…}` and returns `exitFailure` from inside
  the `--fetch-prompt` block, immediately after the prompt response resolves `noPR` / `fixup` /
  `decomposedFromRunID` / `stageType` and strictly BEFORE the lineage-worktree block — so no admin lock, no
  sweep/provision, no lineage lock, no base checkout and **no agent invocation** has happened. The `detail` names the
  remedy: `fishhawk_run_children` for a decomposition child (it spawns the identical child without `--no-pr`), or
  re-dispatch without `push_and_open_pr=false` for a fix-up.

**Mechanism.** `willPushChild` ([#771](https://github.com/kuhlman-labs/fishhawk/issues/771)) and `willPushFixup`
([#794](https://github.com/kuhlman-labs/fishhawk/issues/794)) deliberately do NOT test `cfg.noPR` — those forward gates
exist to stop a succeeded-but-unlanded implement stage — so under `--no-pr` a child still stamps
`push_to_shared_branch` and a fix-up still stamps `push_fixup`. The backend then defers the stage's terminal transition
onto a `/pull-request` report `--no-pr` never sends, and the stage sits in `running` until the reaper sweeps it. The
refusal converts that silent strand into a NAMED category-C failure through the existing
[#1747](https://github.com/kuhlman-labs/fishhawk/issues/1747) detached-reaper report path, the same path every other
pre-agent refusal (`fixup_base_mismatch`, `worktree_admin_lock`) uses.

This is also the layer that owns the **fix-up** arm: `cfg.fixup` is served only on the prompt response, so the
prompt fetch is the first point at which it is authoritative — and it is still strictly before the agent invoke. The
MCP-side `guardNoPRImplement` covers the decomposed-child case pre-spawn.

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

Trace events: `verify_run` per attempt (with committed `head_sha`) + one `verify_summary` (`{outcome: passed|failed|skipped, iterations, max_iterations}`, emitted exactly once) + `verify_fix_reinvoke_error` per failed fix-Invoke attempt (#804) + `verify_infra_flake_retry` when an infrastructure failure is absorbed (once per stage). The absorb matcher is `isVerifyInfraFailure` (#972, widened by #2645/#2718), which claims four DIFF-INDEPENDENT signature classes: a testcontainers container-start timeout (`isTestcontainersStartFlake` — `context deadline exceeded` + a container-start marker); golangci-lint global-lock contention (`parallel golangci-lint is running`, the signature two concurrent local implement stages produce); testcontainers-go's port-not-found rendering (`isTestcontainersPortFlake` — `port "9000/tcp" not found`, requiring `port` as a whole WORD (a leading `\b`, so `airport "9000/tcp" not found` does NOT match) plus digits + `/` + a lowercase protocol inside the quotes, the #2718 occurrence on run 2187fa4c where a MinIO container failed in a package the change never touched); and an unreachable Docker daemon (`isDockerDaemonUnavailable` — `cannot connect to the docker daemon` / `is the docker daemon running` / `dial unix /var/run/docker.sock`, the marker set this repo's own `isDockerUnavailable` test helpers already treat as environment). A FIFTH class — a `signal: <name>` death (`isReverifyInfraFailure`) — is admitted at the pre-push strict re-verify ONLY, see below.
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

Three residuals, all accepted and both bounded to retry churn (never a verified-tree bypass — the push reads the verify OUTCOME, not the classifier, and a retry re-runs the same command): (1) the admissibility check bounds the CHANGE as the cause, not the freshly merged BASE or its interaction with the change, so `signal: killed` / `signal: terminated` can still be rendered when a supervisor kills a verify the base+change combination made genuinely slow or hung; (2) the classifier's input is UNTRUSTED — verify output is influenced by the diff, so a test the agent authored can print the lint-lock line or a `signal: <name>` rendering and steer its own genuine category-B failure to category-C; (3) the #2718 daemon-unreachable class has ONE concrete instance of residual (2) named rather than left abstract — `backend/internal/pgtest/pgtest_test.go`'s own table fixtures carry the daemon-unavailable marker strings AS DATA, so a genuine failure in that package prints a marker in its `--- FAIL:` output and self-classifies as infrastructure, a real loss of parking precision for that one package. It is pinned expecting-true in the corpus (`TestIsDockerDaemonUnavailable`'s pgtest-fixture row, the matching `TestIsVerifyInfraFailure` row, and the delegation mirror's `TestInfraFlake` row), so a later narrowing reddens those rows and prompts an update to THIS paragraph in the same change rather than letting doc and behavior drift. A stage going category-C REPEATEDLY on any of these signatures warrants inspecting the change and the merged base for a hang, and inspecting the failing output for a self-produced signature, rather than blaming the host.

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

**Fix-up wins over the held-commit fields (defence in depth, #2630).** The `if exemptOpenPR` short-circuit runs BEFORE the fix-up path, so if a fetched prompt ever carries BOTH `cfg.fixup` and `open_pr_from_held_commit` the runner would open the PR from the stale held commit and DISCARD the fix-up prompt it just fetched — the #2630 defect that stranded an implement stage in `running`. The backend gate (`resolveHeldCommitExemption`'s fix-up refusal) is the control and a current backend never sends both; but a NEW runner against an OLD backend that predates that gate could. So when both are set the runner clears `exemptOpenPR`, emits a `held_commit_fields_ignored_on_fixup` diagnostic (`run_id`/`stage_id`/`held_sha`), and falls through to the ordinary agent fix-up path — the prompt is the authoritative statement of what the dispatch was FOR. This is a backstop, NOT the control; the backend gate is the control.

**Ship-failure classification: every held-commit ship failure is category C (#2566).** `heldCommitShipCategory` classifies EVERY failure on this path — including a backend rejection of the artifact body (`upload.ErrPullRequestInvalid`, the #2563 `base_sha`-omission shape) — as category **C**, i.e. RETRYABLE. The body this path ships is RUNNER-assembled from backend-advertised held-commit coordinates plus the PR title/body: no committed-tree gate verdict, no operator constraint, is involved, so a `400 pull_request_invalid` here is a runner/backend WIRE defect, never an operator constraint violation. A retry is cheap and safe by construction — the short-circuit precedes the prompt read and the agent wiring (the agent is structurally unreachable), the tip guard re-proves the remote head, and `OpenPR` is idempotent adopt-then-create — so leaving it category B (as it was before #2566) made the run permanently unretryable even after the wire defect was fixed, forcing the ~$20 `resume_run` re-run this resume exists to avoid. This DIVERGES from `pushFailureCategory`, which deliberately keeps `ErrPullRequestInvalid` at category **B** on the ordinary push path — that body carries AGENT-authored PR text and its retry costs a full agent pass. `heldCommitShipCategory` and `TestOpenHeldCommitPR_ShipRejectedAsInvalid_ReportsCategoryC` (plus its legacy-exempt sibling) pin the C half; `TestPushFailureCategory_PullRequestInvalidStaysB` pins the B half so a later change cannot widen C onto the ordinary path.

**Cross-layer coverage stops at the module boundary (stated plainly, #2566).** The classification claim above is a RUNNER-side property: the failure record this path produces carries category `C`. That a category-C stage is then ADMITTED by `fishhawk_retry_stage` — 200, category cleared — is a `backend/internal/server/retry.go` property, pinned SEPARATELY by `TestRetryStage_CategoryCPROpenResumeAdmitted` and its category-B counterfactual `TestRetryStage_CategoryBPROpenResumeRefused`. The two layers are covered PER-SIDE, not by a single end-to-end test through both production stacks, because the runner and backend are separate Go modules and the server handlers live under `backend/internal/`, which Go forbids importing across the module boundary — hoisting `server` out of `internal/` for test convenience would trade a real encapsulation boundary for it. The seam between them is exercised over the WIRE by `TestImplementPROpenResume_ShipRejected_RetriesWithoutAgentReinvocation`, whose `checkpointBackend` is a FAKE that MODELS three backend rules — checkpoint recording, resume emission, and retry admissibility (its category-B arm REFUSES the retry dispatch, exactly as production does) — each of which is itself pinned by the `backend/internal/server` tests named above. This is a deliberate consequence of the two-module architecture, not an oversight.


## Held-commit resume opens a REAL pull request (E67.5 / [#2570](https://github.com/kuhlman-labs/fishhawk/issues/2570))

Both zero-re-run resumes above — the #1231 scope-completeness `exempt` resolution and the #2169 PR-open checkpoint — take the pre-agent short-circuit, so no agent runs and no PR-description handoff is written. `prTitleAndBody` therefore fell straight through to its generic fallback and the resumed PR opened as `chore: fishhawk implement stage <id>` with a two-line body: no summary, no test plan, and no `Closes #N` — so merging never auto-closed the trigger issue and `main`'s history recorded a meaningless commit subject.

**The /tmp handoff cannot serve the resume, so the text round-trips through the backend.** The issue's first proposal was to re-read the keyed `/tmp/fishhawk-pr-<run>-<stage>.md` handoff on resume. That cannot work: `openPRAndShipArtifact` resolves the PR text BEFORE `CommitAndPush` runs the gates that park, and `loadAgentAuthoredPR` DELETES the file it read on every read path (delete-after-read, so a stale handoff can never bleed into a later run). By the time the stage parks or checkpoints the file is already consumed — and on an ephemeral GitHub-Actions runner `/tmp` is gone outright before the retry dispatches. The backend is the only storage a same-host AND a fresh-host resume can both reach. `TestOpenHeldCommitPR_FallbackStillOpensPR` pins this: it asserts the PLACEHOLDER, so a surviving-handoff world would fail it.

**Capture at park / checkpoint time.** `prTitleAndBodyParts` splits `prTitleAndBody` into its pre-footer half, returning the agent's title + body and whether the agent actually authored them. `openPRAndShipArtifact` captures those values (only when agent-authored — the placeholder is deliberately never persisted, or the backend's fallback could not tell recovered text from a placeholder) and rides them on both reports: the `scope_park` ship, and the `pushCheckpoint` that `reportPullRequestFailure` copies onto the `{outcome:"failed"}` report. Both wire fields are `omitempty`, so every report shipped without PR text — and every signature and content hash over it — is BYTE-IDENTICAL to the pre-#2570 bytes.

**Footer ownership is single and explicit.** The persisted body EXCLUDES the Fishhawk attribution footer. The runner that OPENS the pull request appends it exactly once — `prTitleAndBody` on the ordinary path, `openHeldCommitPR` on the resume path. Persisting a footered body would double the footer on resume, stamp it with the branch and audit URL of the wrong pass, and expose it to the size clamp below.

**Size budget: the MARSHALLED bytes, not the raw lengths.** The backend caps the request body at 32 KiB and rejects an oversized one 413, which would strand the stage in `running` — strictly worse than the placeholder. Budgeting raw string lengths is not enough: `encoding/json` HTML-escapes by default, so each `<`, `>` and `&` becomes a six-byte escape, a backslash doubles, and a control character expands to `\uXXXX` — a ~180 KiB raw title/body/reason set marshals to ~1.25 MB. So `upload.shrinkFailureBodyFields` / `shrinkParkBodyFields` MEASURE the serialized body and shrink until it fits, yielding in priority order: the nominal clamps (reason 30 KiB, title 300 B, body 8 KiB, all cut on a RUNE boundary so truncation cannot emit invalid UTF-8 — which `json.Marshal` would replace with a three-byte U+FFFD and GROW the payload), then the reason down to a 4 KiB floor, then TRUNCATING the PR body to the largest fitting prefix (`shrinkHeldCommitPRBody` binary-searches the retained length, MEASURING each candidate, and marks the result — an escape-heavy `## Summary` that overshoots the budget is shortened, not deleted, because dropping it degrades the resume to the synthesized fallback; below a 512 B retained-text floor the fragment is not worth its bytes), then dropping the PR body and title, then the reason below its floor, and finally a `false` shrink-exhausted return. The checkpoint COORDINATES are never dropped: losing them costs a full agent re-run, losing the PR text costs a placeholder the backend's fallback covers.

**Consume per-field.** `openHeldCommitPR` takes the served title and body INDEPENDENTLY, so a recovered title is never discarded because the body was empty; whichever field is empty falls back to the placeholder half. A resume that itself fails re-carries the served text on its own checkpoint, so a multi-retry outage does not silently degrade the NEXT retry back to the placeholder — the repeatable-resume property #2169 established for the coordinates.

**Never a new failure mode.** With no served text and nothing to synthesize from, the resume opens with today's placeholder and ships its artifact exactly as before. Unrecoverable PR text is a quality gap, never a reason to withhold a resume.

## PR-open credential re-authentication (E67.62 / [#2730](https://github.com/kuhlman-labs/fishhawk/issues/2730))

An implement stage that runs the full hour could reach PR-open holding a dead GitHub App installation token and fail the WHOLE stage `401 Bad credentials` — after the gate-verified commit was already pushed. The credential is minted at the top of `openPRAndShipArtifact`, but the forge write happens after `CommitAndPush`, whose committed-tree verify (plus the verify-fix loop) can run for minutes; App installation tokens live ~1 hour and the backend's `githubapp.CachedProvider` only guarantees `RefreshLeadTime` (5m) of remaining life at mint. So the push succeeds on that token and the PR-open 401s.

**The trigger is empirical, not a heuristic refresh.** `gitops` classifies a 401 on the PR create POST, the list-by-head GET, and the GitLab MR create by wrapping `gitops.ErrUnauthorized` into each error (the existing status + body text is retained verbatim; the sentinel rides as a trailing wrap). `openChangeRequestWithReauth` wraps BOTH PR-open call sites — the ordinary open-PR push and the held-commit / `pr_open` resume — and keys off `errors.Is(err, gitops.ErrUnauthorized)`: it re-mints the installation token and re-attempts **exactly once**. Three parts of that are load-bearing:

- **401-ONLY.** A 403 (valid credential, insufficient permission), a 422, a 5xx, or a transport error passes through with its error value UNCHANGED — no re-mint, no second attempt, same downstream category. GitHub returns 401 for an expired/invalid credential and 403 for a permitted-but-not-allowed one, so keying on 401 does not swallow permission faults.
- **EXACTLY ONCE.** The re-attempt is straight-line code, not a loop, so a genuinely-invalid credential costs one extra HTTP call and can never become an unbounded retry.
- **NO DUPLICATE PR.** `gitops.OpenPR` is idempotent adopt-then-create (#2167): it lists by head first and adopts, so the re-attempt cannot open a second pull request.

**401 stays OUT of `retryableStatus`.** Replaying a dead credential against the same endpoint in place cannot succeed; re-authentication belongs to the caller, which is the only layer that can mint. `TestOpenPR_UnauthorizedSingleRoundTrip` pins that as one round trip per call.

**Three distinct recorded outcomes, each self-diagnosing.** An expiry a fresh credential FIXES is not a failure at all — the stage proceeds and emits `pr_open_reauth_attempted` then `pr_open_reauth_succeeded`. A re-mint that itself failed reports the 401 AND the mint error. A fresh credential rejected too is reported explicitly as a genuinely-invalid credential (`a freshly minted credential was rejected too … NOT a mid-stage token expiry`) naming the remedy per forge — the GitHub App installation, or `FISHHAWK_GITLAB_TOKEN`'s `api` scope. A second attempt that fails for a DIFFERENT reason is annotated `after credential refresh` so it is not misread as an auth fault.

**`token_held_seconds` is HELD age, a LOWER BOUND on token age.** Both events carry `token_held_seconds`: the wall time between this runner obtaining the credential (`runnerNow()` right after `mintImplementToken`) and the 401. It is deliberately NOT called token age — `CachedProvider` can serve an already-aged cached token, so true age is this number or more. Real issuance metadata would have to come off the wire (`upload.FetchInstallationTokenResult` carries only `token`), and neither the `gh` CLI fallback nor the operator-supplied GitLab token has an issuance time at all, so the field is named for exactly what it measures.

**A revoked-but-unexpired token correctly stays a failure.** The backend cache keys on remaining lifetime, not on validity, so a re-mint inside the refresh-lead window returns the SAME revoked token, the second attempt 401s, and the stage fails as a genuinely-invalid credential — which is the truthful report. The same is true on GitLab, where a re-mint returns the same static `FISHHAWK_GITLAB_TOKEN`: one extra bounded HTTP call, then a bad-credential report. Neither is special-cased.

## Migration-number collision → scope-amendment DECISION (E67.73 / [#2748](https://github.com/kuhlman-labs/fishhawk/issues/2748))

Two concurrent items that both add a database migration are planned against the same next free number. Whichever lands second must renumber its files — the correct engineering move — but `scope.files` pins the filename the stale plan chose, so the renamed pair is a net-new out-of-scope CREATE and the #818/#825 created-out-of-scope gate fails the stage category-B before the push. The operator then hand-drives `fishhawk_resume_run` with `add_scope_files`.

`runner/cmd/fishhawk-runner/migrationrenumber.go` does **not** relax that gate and does **not** auto-substitute anything. It RECOGNIZES the shape and turns it into an operator-gated scope amendment, reusing the mid-stage amendment machinery that already exists: `POST /v0/runs/{id}/scope-amendments`, the amendment park `fishhawk_await_stage` surfaces as `amendment_pending`, and `fishhawk_decide_scope_amendment`.

**Where the park sits, and why not in the gate.** The gate closure itself CANNOT park. `verifyCommit` runs inside `gitops.CommitAndPush`, AFTER `StageScoped` has already built the scope-only commit against the NARROW declared set and BEFORE the push — the created migration files are not in that commit, so honouring an approval there would mean redoing the whole `FreshFetchBase`/stash/re-stage/re-commit/re-verify cycle. The park is therefore placed at the point that DECIDES whether the gate will fire: in `run()`, immediately before the first `openPRAndShipArtifact` call, gated on `willOpenPR` (implement && !noPR && not a decomposed child && not a fix-up) — precisely the applicability set of the gate's open-PR arm. That is the same pre-commit position `refreshScopeAmendments` already folds approved amendment paths at, so the fold mechanism is reused verbatim and recognition is indifferent to whether the agent staged the files.

**The recognition predicate** (`recognizeMigrationRenumbers`) is purely declared-driven and filesystem-only — no git, no index, no base tree. For each DECLARED `scope.files` migration pair it requires ALL of:

- the parent directory's **basename is exactly `migrations`** — a migration-shaped file under `testdata/`, `fixtures/`, or a `migrations-old/` sibling is a fixture, not a migration;
- the basename matches `NNNN_<slug>.{up,down}.sql` with a non-empty slug;
- the declared pair is **complete** ({up,down}) under one (dir, slug, number) key — a declared half-pair is never a candidate;
- **both declared paths are ABSENT** from the work tree (`os.Lstat` → `fs.ErrNotExist`). `os.Lstat`, not `os.Stat`, is deliberate: a dangling symlink at a declared path returns the link's own info rather than `ErrNotExist`, so it counts as PRESENT and disqualifies the candidate — the fail-closed direction;
- the SAME directory holds **exactly one** complete same-slug {up,down} pair of regular files carrying a **different** number — zero or two-or-more disqualifies;
- neither created path is itself already declared (an already-folded path needs no amendment);
- a final **strict global 1:1 sweep**: if any declared or created path appears in more than one substitution the WHOLE result is rejected, not just the offending entry.

`os.Lstat` / `os.ReadDir` errors return `(nil, err)` — fail-closed at the predicate, which the caller turns into fail-open (`migration_renumber_scan_skipped`, proceed unchanged). A ReadDir on a NONEXISTENT directory is not an error: there can be no created pair there, so that candidate is simply disqualified and the common no-collision run stays silent. A fresh result slice is allocated on every invocation with no package state and no cached listing, so the base-rebase re-invoke arm re-derives everything from the work tree as it stands then.

**The one thing recognition cannot re-derive: an already-folded created path's provenance.** Attempt 1's approval folds the created pair (say `0071`) into `cfg.scopeFiles`. If the base-rebase re-invoke renumbers AGAIN (`0071` → `0072`), the declared set holds TWO absent same-slug pairs — the plan's `0070` and the folded `0071` — both resolving to the one on-disk `0072` pair, which the strict 1:1 sweep rejects wholesale: no amendment offered, second ship category-B. That state is structurally IDENTICAL to the genuinely ambiguous shape the sweep exists to reject (a plan declaring two same-slug migrations and delivering one), so no predicate over (declared, on-disk) alone can separate them. `run()` therefore passes attempt 1's APPROVED substitutions back into the re-invoke's `maybeParkForMigrationRenumber`, and `dropAbsentFoldedCreations` removes exactly those created paths from the declared set fed to recognition — leaving the sweep untouched for every other shape. The drop is CONDITIONAL on both created paths being ABSENT: a folded pair the re-invoked agent left in place stays declared, so the "created pair already declared" branch still disqualifies it and no spurious second request is filed; a half-present pair likewise stays declared. Each dropped path gets its own exemption, since it remains in `cfg.scopeFiles` from the first fold but is no longer in the work tree.

**What the recognition does NOT establish.** It carries **NO allocation proof** — a same-slug `9999` candidate, or a number colliding with a third concurrent run, is recognized and offered exactly like a correct `0071`. It carries **NO base-freshness invariant** — it is a pattern match on declared-vs-on-disk paths, never a claim about what the base tree contains. Whether the renumber is *right* is the operator's decision (or a delegated auto-decide policy's). The cost of a wrong recognition is one declined amendment.

**Strict substitution semantics.** On `approved` the driver re-runs `refreshScopeAmendments` (reuse, not a parallel fold) so the created paths enter `cfg.scopeFiles`, and returns one `scopeExemption` per SUBSTITUTED DECLARED path. The exemption is what stops the #1151 scope-completeness gate demanding the stale declared path — the amendment SUBSTITUTES rather than merely ADDS. Those exemptions are merged into the enforcement set AND passed as `supplementalExemptions`, so the backend re-emits them as a supplemental `scope_files_exempted` audit row (#1218) — the visibility surface, since the trace bundle already shipped under #742 forward gating.

**The gate is NOT widened.** The amendment names ONLY the recognized created paths. Any other created file — a plain `created.txt`, or a migration whose slug matches no declared entry — is not in the request, so an approval cannot fold it and the created-out-of-scope gate still fails the stage on it. A MIXED created set therefore costs one approved-but-insufficient amendment before the unchanged category-B; that is an accepted residual, not a bug.

**Trace events**, emitted on the JSONL log sink: `migration_renumber_recognized` (the substitutions) and `migration_renumber_amendment_decided` (`amendment_id` + `decision` ∈ `approved|denied|undecided|unavailable` + `detail`). The decision event fires on EVERY exit path, including every error path, so a recognition is never silent.

**Fail-open ladder — every branch returns `(false, nil)` and leaves the stage on today's category-B path:** a nil client, an empty MCP token, a POST failure (`422 amendment_budget_exhausted`, `409 stage_not_implement`, `403`, or any other status), a POST that returns no amendment, a list-poll fetch failure, context cancellation, an explicit `denied`, and an amendment still `pending` when the decision budget expires. The blocking wait is bounded by `migrationRenumberDecisionBudget` (900s, matching the backend's `AmendmentPollWindowSeconds`) using the existing `?wait=N` long-poll, and honours ctx cancellation. The one branch whose terminal class is NOT category-B is context cancellation — see "Deadline during the park" below for why, and for the classification it does produce.

**The fold is BACKEND-AUTHORITATIVE, which is why a lost denial cannot reach origin from the runner alone.** The driver never widens `cfg.scopeFiles` itself: on `approved` it re-runs `refreshScopeAmendments`, which folds only rows the BACKEND reports as `approved`. So deleting the driver's own `decision != "approved"` early return does NOT put a denied path on the remote — the stage claims approval in its trace event, folds nothing, and still fails category-B. Both layers must lose the denial before anything is pushed. `TestRun_MigrationRenumber_AmendmentDenied_CategoryBUnchanged` is split to match: its `decision` assertions catch the single-point driver regression, its no-`scope_amendments_folded` and untouched-origin assertions catch the full-chain one. That split is recorded in the test's own doc comment with the observed failure text for each mutation, because a single-point driver mutation leaving the pushed state untouched is exactly the shape that makes a pushed-state counterfactual read as satisfied when it never ran.

**The budget bounds the BLOCKED request, not just the gaps between requests.** Checking the deadline only after a long-poll returns lets a request issued just under it hold the stage for another full `migrationRenumberWaitSeconds` — and indefinitely if the server does not honour `?wait` at all. So each iteration bounds its fetch both ways: `boundedRenumberWaitSeconds` floors the per-request `?wait=N` to the whole seconds remaining (a sub-second remainder yields `0`, which omits the parameter and returns immediately), and the request carries a `context.WithDeadline` at the budget so a server ignoring `?wait` cannot outlast it either. A fetch that fails because THAT derived deadline fired classifies as `undecided` — the budget is what ended it, and reporting a healthy backend as `unavailable` would misname the failure. The split is decided by the CAUSE (`errors.Is(err, context.DeadlineExceeded)` with the parent context still healthy), never by reading the clock after the error, so a genuine backend failure landing near the deadline stays `unavailable`; a PARENT cancellation, and a parent DEADLINE (the stage-level timeout), both stay `unavailable` too. `TestAwaitMigrationRenumberDecision_BlockedFetchHonoursBudget` drives a genuinely blocking fetch (`hold` = 50x the budget) and asserts on elapsed wall clock, since a driver that checks only between requests returns the same `undecided` string, just far later.

**Amendment-budget interaction.** The per-stage cap is `maxScopeAmendmentsPerStage = 2` and is counted ON ROWS, so denied requests consume it. A stage whose agent already filed two mid-stage amendments gets `422 amendment_budget_exhausted` for the substitution request and falls open to today's category-B. Real, accepted limitation.

**Scope-cap residual.** Approving GROWS the effective `scope.files` count by the number of created paths — the stale declared entries remain in `scope.files` and are only EXEMPTED from the completeness gate — so a run already near `max_files_changed` could be pushed over by the approval. The backend already surfaces `effective_scope_files_after_approval` and `max_files_changed` on the pending amendment, so the operator sees the number before deciding. Documented, not mitigated in code.

**Deadline during the park.** The blocking wait runs after the agent has finished, so it consumes stage wall clock without producing work. The runner's stage deadline (`cfg.timeout`) bounds the AGENT INVOCATION only; the post-agent path has no deadline of its own. Two distinct expiries can end a park, and they resolve to DIFFERENT terminal classifications — both verified behaviourally, end to end through `run()`, not reasoned about:

- **The DECISION BUDGET expires with the parent context healthy.** The driver returns `(false, nil)` with `decision:"undecided"`, the stage falls through to the unchanged created-out-of-scope gate, and the terminal classification is **category-B** — byte-identical to the same fixture whose park never blocked at all. `TestRun_MigrationRenumber_ParkExpiry_ClassificationMatchesUnparked` asserts that equivalence directly.
- **The PROCESS/STAGE context is cancelled while the long-poll is held.** The driver returns `(false, nil)` with `decision:"unavailable"` and `detail:"context canceled"` — the fail-open branch, not a decision — but that same cancelled context then flows into the whole `openPRAndShipArtifact` chain, which fails at its first git call. The terminal classification is therefore **category-C** (`rev-parse HEAD: context canceled`) with `runner_cancelled` and `exitCancelled`, i.e. the standard #435 cancellation contract. Category-C is the CORRECT class here and category-B would be wrong: the commit chain never ran, so no gate reached a verdict on this tree, and an interrupted stage is retryable infra rather than a park-for-re-scope. `TestRun_MigrationRenumber_ParentCancelledDuringPark_ClassificationUnchanged` pins it against a control that delivers the SAME cancellation at the post-park egress boundary with no park ever blocking: the two classifications match, so the park contributes no failure class of its own. Nothing reaches origin on either.

**No HTTP-surface change.** The `?wait` query parameter already exists and is already documented; `upload.FetchScopeAmendmentsArgs.WaitSeconds` is additive and omits the parameter entirely when zero or negative, so the #961 pre-commit refresh and the #2601 undecided-detection requests stay byte-identical. No new endpoint, no new audit kind, no new Notifier method: the reused `scope_amendment_requested` / `scope_amendment_decided` rows and the existing `must_page_human` `scope_amendment` ping fire unchanged.

## Fix-up reporting-obligation reports (#2737)

On a fix-up pass the self-report sidecar (`#1210`) may carry an `obligations`
array: one entry per routed REPORTING obligation the fix-up prompt named by id.
`loadFixupSelfReport` now returns a validated `fixupSelfReportResult` (claimed
verify status + surviving obligation reports) instead of a bare string.

The whole-sidecar rules are unchanged and dominate: malformed JSON or a
`run_id`/`stage_id` mismatch discards the ENTIRE sidecar including its
obligation reports, so a foreign run's `met` can never satisfy this stage's
obligations. An unrecognized `verify_status` is the one non-fatal case — it
zeroes the claim exactly as before (`fixup_selfreport_status_ignored`) while the
obligation reports are still parsed, because the two signals are independent.

Within a valid sidecar, `validateFixupObligationReports` drops an entry — with a
`fixup_obligation_report_dropped` log naming the id and the reason — when its id
is empty (`empty_id`), its status is not exactly `met` or `declined`
(`unknown_status`), it is `met` with an empty/whitespace `record`
(`met_without_record`), or it is `declined` with an empty/whitespace `reason`
(`declined_without_reason`). Dropping is the SAFE direction: a dropped `met`
leaves the obligation undelivered and the backend's advisory signal fires.

Retained free text is **untrusted at this boundary**. The fix-up agent executes
arbitrary repository commands, so it controls every byte of a `record`/`reason`,
and that text leaves the repository over the trace bundle and lands in the
reviewer's prompt *without ever appearing in the committed diff* — an
instruction-injection channel into the reviewer and a structure-bearing egress
path around the diff. Bounding it would not make it trusted, so
`sanitizeFixupObligationText` **flattens its structure first and bounds second**:
`flattenFixupObligationText` collapses every control rune (newline, CR, tab,
NUL, the ANSI escape introducer), every format rune (bidi overrides,
zero-width joiners), and the Unicode line/paragraph separators to a single
space, collapses whitespace runs and trims — so what crosses the boundary is one
line of words, never a multi-line document with fenced blocks or impersonated
headers. The words survive: this is a structure control, not censorship, so an
honest one-line decline reason is returned byte-identical. Size is then capped
at `maxFixupObligationTextBytes`. Pinned by
`TestLoadFixupSelfReport_ObligationTextFlattenedAtUpload` (adversarial) and
`TestFlattenFixupObligationText` (per-case table).

This is the **upload half** of a two-layer treatment; the reviewer-rendering
half (`backend/internal/prompt`'s `writeFixupObligationDeclineReasons`)
quarantines the surviving text in a BEGIN/END untrusted-DATA envelope. Neither
layer relies on the other — the runner cannot assume a backend version, and the
backend must hold for a bundle a compromised runner composed.

Survivors ride a `fixup_reporting_obligations` event that `composeGateEvidence`
folds into `gate_evidence.fixup_reporting_obligations`. The json tags MUST stay
identical to `backend/internal/bundle`'s `FixupReportingObligationEvidence` — a
one-sided edit silently DISABLES the signal, so both sides are pinned against
one shared literal JSON fixture. EVIDENCE ONLY: this block never touches
`res.OK`, `res.FailureCategory`, or the budget. Long-form contract:
`backend/internal/fixupobligation/README.md`.
