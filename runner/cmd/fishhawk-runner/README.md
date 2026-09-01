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
- `acceptenv.Env` → `Invocation.BaseEnv` (the `runner/internal/agent` seam): a non-nil BaseEnv REPLACES the `os.Environ()` seed in both the claudecode and codex adapters, with the API-key + `Env` overlays still applied on top; nil preserves the inherit-parent-env behavior byte-for-byte. Since #2894 the non-acceptance spawn is ALSO given a non-nil BaseEnv (`agentenv.Env`, below), so the nil case is no longer a path the runner takes; this acceptance branch's assignment runs later and overwrites it, leaving the acceptance posture byte-for-byte unchanged. Refused passthroughs → `acceptance_env_refused`.
- `inv.JSONSchema = acceptanceVerdictJSONSchema` (claudecode structured output; other backends use the file fallback).

**Verdict file path (#1780):** the buildAcceptance output contract NAMES the run/stage-keyed `/tmp/fishhawk-acceptance-<run>-<stage>.json` path (`prompt.AcceptanceVerdictPath(runID, stageID)`, threaded via `Trigger.AcceptanceRunID`/`AcceptanceStageID`). The runner's `acceptanceVerdictPath` reads that FIRST, falling back to the legacy fixed `/tmp/fishhawk-acceptance.json` (`legacyAcceptanceVerdictPath` ↔ `prompt.LegacyAcceptanceVerdictPath`) when a trigger threads no ids. The keyed and legacy format strings are byte-identical across the two modules.

**Post-agent:**

- `captureAcceptanceVerdict` — StructuredOutput > file.
- `validateAcceptanceVerdict` — backend-`acceptanceBody`-mirrored rules + served-criteria-id membership, fail-closed on unknown ids. Missing verdict → category-B `acceptance_verdict_missing`; invalid → category-B `acceptance_verdict_invalid`. A VALID `failed` verdict is NOT a runner failure — routing is E31.8.
- `redactAcceptanceVerdict` BEFORE embed/ship.
- `composeAcceptanceEvidence` appends the `acceptance_evidence` event pre-`PackBytes` (both bundle variants).

**Ship:** after the trace upload, `upload.Client.ShipAcceptance` POSTs the redacted verdict to `/v0/runs/{run_id}/acceptance?stage_id=…` signed with the re-issued run key (ShipPlan-modeled retries; `ErrAcceptanceInvalid` → category-B, other failures → category-C).

**The `undecidable` per-criterion result (#2512, E48.78 layer 4).** A criterion the agent attempted and genuinely could not DECIDE is reported `result="undecidable"` with a REQUIRED, non-whitespace `undecidable_reason` naming what could not be determined and why — never as `failed` (which flattens honest uncertainty into a defect signal and sends a correct change into acceptance triage) and never as `passed` (a green light over an unevaluated criterion). It is distinct from `skipped`, which means the criterion was never in play for this run.

- The **TOP-LEVEL** `verdict` enum stays `["passed", "failed"]` in BOTH `acceptanceVerdictJSONSchema` and `validateAcceptanceVerdict`. `undecidable` is SERVER-DERIVED: the backend reads the rows and records the run's disposition itself, so a producer cannot ship it and the runner rejects it if one tries. The acceptance prompt states the mapping the agent must follow — ship `passed` when NO row failed, even when every row is `undecidable`.
- `undecidable_reason` is decided on field **PRESENCE**, never on emptiness: it is decoded as a `*string` because Go's `encoding/json` makes an ABSENT field indistinguishable from a PRESENT empty one on a plain `string`, so `{"result":"passed","undecidable_reason":""}` would otherwise be silently admitted. Absent is accepted on a non-undecidable row; present — empty or not — is rejected. A literal JSON `null` decodes to nil and is treated as absent.
- **Corpus agreement with the backend twin.** `validateAcceptanceVerdict` is unexported in package `main` and this module does not require the backend module, so no test can carry bytes between the two validators. Instead `docs/spec/acceptance-verdict-fixtures.json` is mirrored into `testdata/` here and into `backend/internal/server/testdata/` by `scripts/sync-schemas`, and `TestValidateAcceptanceVerdict_Corpus` (here) and `TestAcceptanceVerdictCorpus_BackendPartitionMatchesRunner` (there) assert the SAME admit/reject partition over the SAME rows. The claim is corpus agreement, NOT byte-carrying. The sharpest row is a WHITESPACE-ONLY `undecidable_reason` — the divergence that would strand a completed acceptance stage while both suites stayed green.
- The schema stays dialect-free (`TestAcceptanceVerdictSchema_NoDialectOrVendorKeys`); `TestAcceptanceVerdictSchema_LockstepWithValidator` additionally asserts the criteria `result` enum carries `undecidable` and that a schema-admitted undecidable row passes the validator, since the enum is a config-shaped string constant no compiler checks.

Shape lockstep (schema ↔ runner validator ↔ backend validator) is guarded by `TestAcceptanceVerdictSchema_LockstepWithValidator`.

## Plan-stage sibling artifacts ([#2833](https://github.com/kuhlman-labs/fishhawk/issues/2833))

A `plan`-typed stage may emit an additive **standard_v1 sibling** at the
`--plan-out` path instead of a plan. The runner recognizes two kinds, held in
`planSiblingKinds` (`main.go`):

| `kind` | Emitted by | Backend outcome |
|---|---|---|
| `clarification_request` | the planner's step-zero plannability check ([#1057](https://github.com/kuhlman-labs/fishhawk/issues/1057)) | stage parks at `awaiting_input` |
| `grooming_report` | a backlog-grooming **propose** stage ([#2235](https://github.com/kuhlman-labs/fishhawk/issues/2235)) | artifact row persisted, decided at the plan gate |

That set is a deliberate duplicate of the backend's
`plan.ArtifactKindClarificationRequest` / `plan.ArtifactKindGroomingReport`: the
runner module declares **no** dependency on the backend module, so the backend's
discriminator is unreachable here. A third sibling is one entry in this set plus
the backend's own routing — a documented residual, not a solved problem.
`TestDetectPlanSibling` asserts the shipped per-kind behavior so the list cannot
silently drift into a no-op.

**Adoption precedence** is `recognized-sibling(file) > structured_output >
agent-written file`. `detectPlanSibling` peeks only the top-level `kind`
discriminator; every not-a-sibling path (a plan, which carries `plan_version`
and no `kind`; an UNRECOGNIZED kind; malformed JSON; an empty or missing file)
falls through to the normal coerce+validate gate, where a genuinely-bad plan is
demoted to category-B as before.

On a hit the runner does three things and no more:

1. **The sibling wins over structured-output adoption.** This is the control
   that closes the destroy-before-upload window: adopting the invocation's
   `structured_output` plan would overwrite the sibling on disk *before*
   `uploadPlan` re-reads the file, so the artifact would be lost rather than
   merely mis-reported.
2. **`plan.TryCoerce` and `plan.Validate` are skipped**, in the precedence block
   and again inside `validatePlan` as defense in depth for any other caller (a
   local replay driven only by `--plan-out`). Both are standard_v1-shaped:
   `TryCoerce` REWRITES the file in place when it fires, so an ungated run can
   corrupt an otherwise-valid report, and `Validate` would demote it to
   category-B for failing a schema it was never meant to satisfy.
3. **Clarification-only post-processing stays kind-gated.**
   `StripUnknownClarificationProps` carries hand-derived, clarification-shaped
   allowlists (`questions[]` / `ticket_reference` / `generated_by`) with no
   grooming equivalent; running it on a `grooming_report` would strip every
   legitimate grooming property.

**Sibling validation is the BACKEND's job.** Neither sibling schema is embedded
in the runner — `scripts/sync-schemas` routes both to
`backend/internal/plan/schemas/` only, by an explicit comment — so the runner
writes the artifact and the backend validates it on ingest. See
[`docs/spec/grooming-report-v1.md`](../../../docs/spec/grooming-report-v1.md)
and
[`docs/spec/clarification-request-v1.md`](../../../docs/spec/clarification-request-v1.md).

### 400 error code → failure category

All four artifact kinds ship to the same endpoint (`POST
/v0/runs/{run_id}/plan`, routed by `kind`), so `upload.ShipPlan` classifies the
400 by the backend's error **code**, held in `agentOutputInvalidCodes`
(`runner/internal/upload/upload.go`):

| Code | Category | Backend source |
|---|---|---|
| `plan_invalid` | **B** — `upload.ErrPlanInvalid` | `backend/internal/server/plan.go` |
| `clarification_request_invalid` | **B** | `backend/internal/server/plan.go` |
| `grooming_report_invalid` | **B** | `backend/internal/server/grooming_report.go` |
| `grooming_report_stage_invalid` | **B** | `backend/internal/server/grooming_report.go` |
| anything else (e.g. `validation_failed`) | **C** — generic error | — |

Each B code is one the backend handler has ALREADY transitioned the stage to
`failed`-B on before writing the 400, so the runner must agree or the two halves
of one event disagree on the category. The list is NAMED, not a blanket: a
request-shape 400 stays category-C and is not the agent's fault.

The code is **exact-matched** out of the backend's error envelope
(`{"error":{"code":…,"message":…}}`) rather than substring-scanned, because
`message` is free text — a `validation_failed` response whose human-readable
message merely mentions `plan_invalid` would otherwise be classified
permanent-B and never retried.

**The match is made by a STREAMING token walk (`upload.errorEnvelopeCode`), not
by `json.Unmarshal`, and that distinction is the control.** `Unmarshal` needs a
complete, well-formed document, so it fails on any body the classification read
truncated — and `readClassifiableBody` caps that read at `classifyBodyLimit`
(8 KiB). A *valid* envelope whose `message` or `details` runs past the cap would
therefore decode to nothing and drop onto the substring fallback, re-opening the
exact collision the exact match exists to close: a `validation_failed` envelope
mentioning `plan_invalid` in its first 8 KiB classified permanent-B and never
retried. The token walk instead stops the instant it has read `error.code`, so a
valid envelope is classified from its exact code **regardless of the total
response length**. Only the code member has to land inside the window, and it
always does — the backend declares `Code` as `errorBody`'s first field
(`backend/internal/server/errors.go`) and `encoding/json` emits struct fields in
declaration order. `TestShipPlan_AgentOutputInvalid_EnvelopeExceedingClassifyLimit`
drives a 32 KiB envelope through both directions (colliding message → C, listed
code → B) and reddens if the walk is replaced by `Unmarshal`; `TestErrorEnvelopeCode`
pins the walk's own truncation contract.

When the body yields no envelope code at all (a proxy error page, an older flat
`{"code":…}` shape, a body truncated *before* `code` arrived), classification
**falls back** to the historical substring check. That fallback exists so
classification never gets *weaker* than it was before exact-match landed: losing
category-B on a genuinely-invalid artifact would turn a permanent failure into a
retry loop against a backend that has already failed the stage. It is the
strictly-less-precise path and runs only when the precise one has nothing to
read.

**Known residual (accepted, not a defect).** On that fallback path any
undecodable 400 whose free text merely *contains* a listed code is classified
permanent-B — `TestShipPlan_AgentOutputInvalid_UndecodableBodyFallsBackToSubstring`
pins `502 Bad Gateway: plan_invalid` as `wantB=true`. A proxy-injected 400 whose
prose coincidentally names a code is therefore never retried. This is the
deliberate trade above, chosen because the alternative failure (retrying forever
against a stage the backend already failed) is worse. Retiring it is an operator
decision, available once no flat-`{"code":…}` producers remain; it is not a
change to make from the runner side alone.

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

**Third occurrence — run 87962b5b, 2026-08-30 (#3048), and why the absorb correctly did NOT fire.** The pass died at the same site with a `verified_tree_mismatch` line, and both halves of that are worth naming precisely because they read as two defects and are one, plus a design limit.

The `verified_tree_mismatch` was the #960 guard operating NORMALLY, not a second failure. The committed-tree gate verifies a THROWAWAY scope-only commit that is then reset away, so the real staged commit legitimately carries a different tree; the guard NOTICES that and demands the strict re-verify — exactly its purpose. The mismatch was a CONSEQUENCE of the ordinary flow, not a cause. The failure was the re-verify's own non-zero exit.

The #2645 absorb did not fire because `scripts/test verify` ran to COMPLETION and exited non-zero on an ordinary test assertion — a flaky clock-domain comparison in `backend/internal/run/dispatchliveness_test.go` (a Go-side `time.Now()` heartbeat seed compared against a Postgres-container-stamped `dispatched_at`; see the AGENTS.md cross-clock trap bullet). `isReverifyInfraFailure` matches a signature ALLOW-LIST of diff-INDEPENDENT infra signatures, and a completed run with a failed assertion matches none of them. The design's dividing line is **"did the command complete"**, not "is the failure attributable to the diff" — a completed non-zero verify is category-B by construction, so an unrelated flaky test is structurally non-retryable and destroys the implement pass.

That is a design limitation the operator has ruled INTENTIONAL (#3048), not an implementation defect: widening the allow-list to cover assertion failures would let a genuinely-broken change retry itself. The remedy taken in #3048 is therefore to remove the flake AT ITS SOURCE — DB-anchor the comparative seeds so the test carries no cross-clock dependency — rather than to reclassify the gate.

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

### Agent-env allow-list

`runner/internal/agentenv::Env` builds the child env for the NON-acceptance
agent spawn (plan / implement / review), assigned to `Invocation.BaseEnv` at
main.go's single `agent.Invocation` construction site. It is the third and last
member of the family: `gateenv.go` covers gate subprocesses, `acceptenv` covers
the acceptance agent, `agentenv` covers everything else. Before #2894 the
implement agent was the ONE subprocess class with no allow-list — a nil
`BaseEnv` is inherit-parent-env in both adapters, so the agent saw the runner's
whole `os.Environ()`, including the ambient operator bearer
`FISHHAWK_API_TOKEN`.

The posture is the same **default-deny allow-list**, with the rungs keyed to
what the implement agent actually drives (the repo's Go toolchain, git,
Docker-backed testcontainers, a Node-based agent CLI):

- `allowExact` — PATH/HOME/locale/temp/CC/CXX, `CI` (scripts/test's timescale
  auto-5x keys off it), the proxy vars in both cases, and the Docker **client**
  vars by exact name (`DOCKER_HOST`, `DOCKER_CONTEXT`, `DOCKER_CONFIG`,
  `DOCKER_CERT_PATH`, `DOCKER_TLS_VERIFY`, `DOCKER_API_VERSION` — exact rather
  than a `DOCKER_` prefix, which would also admit `DOCKER_AUTH_CONFIG` and
  `DOCKER_PASSWORD`).
- `allowGo` — the explicit Go toolchain/runtime NAME set, not a bare `GO`
  prefix (#2504). It is DUPLICATED from `gateEnvAllowGo` because this package
  cannot import package main and hoisting the set would break the
  source-reading `TestGateEnvListsMatchCLICopy` lockstep with the CLI copy;
  `TestAgentEnvNotNarrowerThanGateEnv` pins the duplication in Go, so drift
  fails a test rather than silently narrowing the agent's toolchain env.
- `allowPrefix` — `LC_`, `CGO_`, `XDG_`, `NODE_`, `NPM_CONFIG_`/`npm_config_`,
  `TESTCONTAINERS_`, `SSL_CERT_`, and the model/agent-CLI configuration family
  `ANTHROPIC_`/`OPENAI_`/`CLAUDE_` (gateway base URLs, alternate auth tokens,
  CLI knobs) — the one class deliberately re-admitted, exactly as `acceptenv`
  re-admits the model keys.

Layered on top, applied BEFORE the allow-list: `denyExact`
(`FISHHAWK_API_TOKEN`, `FISHHAWK_GITHUB_TOKEN`, `FISHHAWK_GITLAB_TOKEN`,
`GITHUB_TOKEN`, `GH_TOKEN`, `ANTHROPIC_API_KEY`, `OPENAI_API_KEY`, `NPM_TOKEN`,
`DOCKER_AUTH_CONFIG`, `SSH_AUTH_SOCK`) and `denyPrefix` (`FISHHAWK_`, `GOOGLE_`,
`AWS_`, `AZURE_`).

**The two credentials that still reach the child are OVERLAYS, not
inheritance.** The run-bound MCP token arrives via `Invocation.Env`
(`FISHHAWK_API_TOKEN` = the freshly minted `fhm_` bearer, plus
`FISHHAWK_BACKEND_URL`) — the SAME variable name the denylist strips from the
base, so the name is present in the child carrying the run-bound value, never
the ambient operator bearer. The model API key arrives via the adapter's
API-key append: `apiKeyForAgent` reads the runner's OWN ambient process env
(`os.Getenv`), which this filter never mutates — it filters a snapshot — so
denying `ANTHROPIC_API_KEY`/`OPENAI_API_KEY` from the base makes the adapter
the SINGLE injection point for the model credential rather than withholding it.
Both routes go through `agent.AppendEnvOverride`, which strips any same-named
entry before appending, so each appears exactly once.

**Operator passthrough.** `FISHHAWK_AGENT_ENV_<NAME>=<value>` on the runner env
becomes `<NAME>=<value>` on the agent env — a per-variable escape hatch needing
no code change. It is checked BEFORE the `FISHHAWK_` deny prefix, so the
channel works while every other `FISHHAWK_*` var (including the
`FISHHAWK_TEST_*` / `FISHHAWK_SKIP_PATCH_COVERAGE` gate knobs) stays out. A
stripped name that collides with the denylist is **refused, never honored**,
and reported on a single-line `agent_env_refused` log event — the passthrough
branch is the one place the denylist is load-bearing rather than
belt-and-suspenders, since default-deny already drops those names from the base.

**Honest residuals.** The passthrough is NOT a universal escape hatch, and the
two residuals below are exactly where it stops. `Denied` consults BOTH
`denyExact` and `denyPrefix`, and the passthrough branch applies it to the
STRIPPED name, so `FISHHAWK_AGENT_ENV_SSH_AUTH_SOCK` and
`FISHHAWK_AGENT_ENV_AWS_BEARER_TOKEN_BEDROCK` are refused and logged, not
honored (`TestEnv_PassthroughDeniedCollisionRefused` names both). Re-admitting
a denied variable takes a code change to the deny rules — deliberately, since
the refusal is the one place the denylist is load-bearing.

- `SSH_AUTH_SOCK` is dropped (and denied): it is an authority handle to the
  operator's SSH agent, and the runner — not the agent — performs the
  run-branch push. An agent-driven `go mod download` of a private module over
  an ssh-rewritten URL would fail; this repo has no private module
  dependencies, and the passthrough will NOT restore it.
- `AWS_*` / `GOOGLE_*` / `AZURE_*` are denied by prefix, so a deployment
  routing the agent through Bedrock or Vertex loses its cloud model credential
  and cannot re-admit it through the passthrough either. Bedrock/Vertex
  routing is therefore unsupported under this policy until those deny rules
  are narrowed in code. The first-party route is unaffected: `apiKeyForAgent`
  reads the runner's ambient `ANTHROPIC_API_KEY`/`OPENAI_API_KEY` and the
  adapter appends it as an overlay, which is why denying the raw keys from the
  base withholds nothing.

**A second model-vendor credential DOES reach the agent.** The
`ANTHROPIC_`/`OPENAI_`/`CLAUDE_` allow rungs re-admit those whole
configuration families, while `denyExact` names only the two raw keys
`ANTHROPIC_API_KEY` and `OPENAI_API_KEY` — so any OTHER credential-bearing
name in those families exported ambiently on the runner (`ANTHROPIC_AUTH_TOKEN`,
`OPENAI_ADMIN_KEY`, `CLAUDE_CODE_OAUTH_TOKEN`) WILL be inherited by the
implement agent. That is a deliberate, documented residual, not an oversight:
the families carry the gateway base URLs and alternate auth tokens the agent
needs, exactly as `acceptenv` re-admits the model keys. An operator who does
not want a particular one reaching the agent must not export it into the
runner's environment.

Pinned by `runner/internal/agentenv/agentenv_test.go` (one behavioral case per
named branch), `TestAgentEnvNotNarrowerThanGateEnv` (the gate-env lockstep),
and `TestRun_ImplementStage_ChildEnvExcludesAmbientCredentials` (the
cross-boundary end-to-end: main.go wiring → `BaseEnv` → the claudecode adapter's
seed → a REAL child process that echoes its own environment).

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

## A non-composed PR body is loud (E52.30 / [#3012](https://github.com/kuhlman-labs/fishhawk/issues/3012))

An implement pass shipped the generic placeholder PR body on its FIRST attempt — no Summary, no Test plan, no `Closes #N`, so merging never auto-closed the trigger issue — and nothing in the trace, the log or the audit chain said the agent's text had been skipped. `loadAgentAuthoredPR`'s commonest fallback branch (neither the run/stage-keyed `/tmp/fishhawk-pr-<run>-<stage>.md` handoff nor the legacy fixed path present) returned `prSourceFallback` with an explicit "don't log" and no reason, so a SKIPPED composition was byte-indistinguishable from a deliberately terse body and from the four other fallback branches.

This is the ORDINARY path, not the [#2570](https://github.com/kuhlman-labs/fishhawk/issues/2570) held-commit resume. `prTitleAndBodyParts` is the ONE composition site, shared by `openPRAndShipArtifact` and `openHeldCommitPR`; #2570 changed only how the RESUME recovers text, so this is neither a #2570 regression nor a second composition path.

**Six classifications, one bounded enum.** `prBodyReason` names why the shipped body is not the agent's:

| Reason | Branch |
|---|---|
| `""` (`prBodyReasonNone`) | composition SUCCEEDED — a non-empty agent title AND body |
| `handoff_absent` | neither the keyed nor the legacy path exists — BOTH reads failed `IsNotExist` (the silent branch that shipped #3011) |
| `handoff_unreadable` | a handoff path exists but `os.ReadFile` failed for a non-`IsNotExist` reason — the keyed path, or the legacy path when the keyed one is absent |
| `empty_file` | the handoff is empty after trimming |
| `empty_title` | the handoff's first line is blank |
| `body_absent` | a TITLE-ONLY handoff — the agent's title is honoured, the PR opens footer-only |

`body_absent` is classified on the body being EMPTY, not on which parse branch produced it, so `title\n\n` (a separator with nothing after it) is caught alongside a bare title line.

**A legacy read FAILURE is not a legacy absence.** The legacy fallback is consulted only when the keyed path is absent, and until the #3012 re-review it classified every `os.ReadFile` error there as `handoff_absent` — so an unreadable legacy handoff (a directory yielding `EISDIR`, a file the runner cannot read) shipped a marker and an audit record falsely claiming neither path existed: this issue's own defect, one branch over. Only `IsNotExist` is now an absence; anything else is `handoff_unreadable`, the same reason the keyed path carries for the identical failure. It gets no sixth wire value, but because the marker names the canonical run/stage-keyed path, the branch emits `pr_description_legacy_unreadable` (path + `detail`) so the path that actually failed appears in the trace — a path diagnostic alongside the pre-existing `pr_description_legacy_path`, not a third member of the composition-signal pair below.

**The composition function is PURE; the CALL SITES emit.** `prTitleAndBodyParts` classifies and RETURNS the reason, logging nothing. The emit lives at the two PR-opening call sites because only they know whether a fallback was subsequently RECOVERED — the held-commit resume replaces the placeholder from backend-served text AFTER composition returns, so emitting inside the composition function would log a composition failure on a resume that recovered successfully. That is the record-asserts-what-did-not-happen defect this issue is about, so reintroducing it here would be self-defeating.

**Two events, mutually exclusive.**

- `pr_body_not_composed` — `{run_id, stage_id, reason, handoff_path}`. Emitted when the reason is non-none AND nothing recovered the body.
- `pr_body_recovered` — `{run_id, stage_id, reason, title_recovered, body_recovered}`. Emitted ONLY when BOTH hold: composition fell back, AND the served body replaced the placeholder. A resume whose composition SUCCEEDED and which also carries served text emits NEITHER — there was no recovery to announce. The pre-existing `held_commit_pr_text_recovered` is unchanged; `pr_body_recovered` is its greppable counterpart, so `grep pr_body_not_composed` never matches a run that recovered.

**The marker is on the BODY, keyed to the REASON.** Whenever the reason is non-none — including `body_absent`, which is agent-authored — the call site prepends a block naming the run, the stage, the reason value and the handoff path checked, and stating outright that the body carries no `Closes #N` so the merge will not auto-close the trigger issue. The rule is *reason != none implies the body names the failure*, NOT *the handoff was absent implies it*: a title-only handoff shipping an unmarked footer-only body would be the original bug reproduced by its own fix.

**The runner cannot repair the auto-close, only report it.** No `issueNumber` field exists anywhere in this package, so the runner structurally cannot synthesize the missing `Closes #N`, and there is no retro-edit path to add one after the PR opens ([#2782](https://github.com/kuhlman-labs/fishhawk/issues/2782)). Saying what it costs is the honest substitute for a repair that is not available.

**ORDERING: the marker is applied AFTER `implementCommitMessage`.** On the ordinary path `body` feeds BOTH the PR/artifact and the no-sidecar commit-message fallback. Marking first would stamp the marker into `main`'s squash commit message, where it cannot be un-shipped. `TestOpenPRAndShipArtifact_FallbackCommitMessageUnchanged` pins the ordering.

**The reason rides the artifact.** `pr_body_fallback_reason` is added to the `pull_request` artifact map ONLY when non-none, so every composed ship's bytes, content hash and signature are byte-identical (the #1218/#2570 omitempty discipline). The backend declares it on `pullRequestBody` (it must, or `DisallowUnknownFields` would 400 the ship AFTER the PR exists — the #2562/#2563 stranding shape), normalizes an unrecognized value to `unknown` rather than rejecting, and folds it into the `pull_request_opened` audit payload. Two shared wire goldens bind the modules: `testdata/wire/ordinary_pr_artifact.json` (produced by the real `openPRAndShipArtifact` with no handoff, POSTed by the backend suite through the real handler) and `testdata/wire/held_commit_pr_artifact.json`. Both golden tests run against an ISOLATED temp handoff dir and delete nothing under `/tmp`: pinning the production dir and `os.Remove`-ing the keyed plus the SHARED legacy handoff (as they did until the #3012 re-review) could destroy an active run's PR description mid-flight, costing that run exactly the Summary/Test plan/`Closes #N` this issue exists to protect. The fixtures still carry the production path literal — `substituteWireHandoffPath` rewrites the temp path back before the byte comparison, and FAILS if the marker does not name it, so the substitution can never become a silent no-op that greens a vacuous comparison.

**Obligation for a future call site.** The emit is at the call sites BY DESIGN, which means a third production caller of `prTitleAndBodyParts` added without its own emit would regain today's silence. Stated here so it is discoverable rather than folklore.

## A base-rebase re-invoke replays the agent handoffs (E67.93 / [#2840](https://github.com/kuhlman-labs/fishhawk/issues/2840))

Four PRs ([#2741](https://github.com/kuhlman-labs/fishhawk/pull/2741), [#2755](https://github.com/kuhlman-labs/fishhawk/pull/2755), [#2776](https://github.com/kuhlman-labs/fishhawk/pull/2776), [#2779](https://github.com/kuhlman-labs/fishhawk/pull/2779)) opened with the `chore: fishhawk implement stage <id>` placeholder title, a placeholder body and a placeholder commit message even though their agents had written a perfectly good PR description — so the trigger issue never auto-closed. The agent did nothing wrong; the stage hit a base-rebase conflict.

**The mechanism is delete-after-read meeting a second pass.** Both agent handoffs are one-shot files under `/tmp`:

| Handoff | Path | Loader |
|---|---|---|
| PR description | `/tmp/fishhawk-pr-<run>-<stage>.md` (plus the legacy fixed path) | `loadAgentAuthoredPR` |
| Initial commit message | `/tmp/fishhawk-implement-commitmsg-<run>-<stage>.txt` | `loadImplementCommitMessage` |

Each loader `os.Remove`s whichever path it read, on every return path, so a stale handoff can never bleed into a later run/stage. That is right ACROSS stages and wrong WITHIN one, because `openPRAndShipArtifact` resolves both handoffs BEFORE `CommitAndPush` can fail with `gitops.ErrBaseRebaseConflict` — and on that error `run()` re-invokes the agent (`reinvokeOnBaseRebaseConflict`, [#989](https://github.com/kuhlman-labs/fishhawk/issues/989)) and calls `openPRAndShipArtifact` a SECOND time. By then both files are gone, so the second pass falls through to the placeholder for the title, the body AND the commit message. The runner log was the only place this was observable at all, and it said nothing about it.

**`agentHandoff` is a per-stage memo, armed lazily.** `cfg.handoff` is a runtime-set POINTER field on `config` (the `agentBinary`/`agentVersion` idiom), armed in `run()` immediately before the FIRST `openPRAndShipArtifact` call of an implement stage and nil everywhere else. `config` is passed by value, but copying a struct copies the pointer, not the pointee — so both calls share ONE memo without adding a 16th parameter to a function with 38 test call sites and two shared wire goldens that construct its args directly.

Arming LAZILY rather than hoisting the capture above the retry is deliberate. A hoist would move the loaders' emits (`pr_template_warning`, `pr_description_legacy_path`, `implement_commitmsg_empty`) earlier in the runner log and break first-pass byte-identity. With a memo the FIRST read still happens inside `openPRAndShipArtifact`, at the same point, with the same emits in the same order; only a SECOND pass consults the memo. `TestAgentHandoff_ArmedNoConflictUnchanged` asserts an armed memo is indistinguishable from a nil one across every handoff shape, and the unedited `testdata/wire/ordinary_pr_artifact.json` golden is the independent byte-level detector.

**Three rules, each its own control.**

| Rule | Behaviour | Why | Pinned by |
|---|---|---|---|
| (a) fresh-first | ALWAYS attempt the fresh read; never short-circuit on a stored value | `baseRebaseConflictPrompt` re-requests both handoffs by absolute keyed path, so a compliant re-invoked agent re-writes them — and its text describes the RE-LANDED slice, so it must win | `TestRun_ImplementStage_BaseRebaseConflict_ReinvokeFreshHandoffWins` |
| (b) store-on-success | Store only `prSourceAgent` / `ok` captures | a first-pass MISS must not poison a later pass that DOES find text, and a cached fallback would announce a replay that never happened | `TestAgentHandoff_LoadOrReplay`, `TestRun_ImplementStage_BaseRebaseConflict_ReinvokeNoHandoffStillFallsBack` |
| (c) replay-on-miss | Replay the stored capture only when the fresh read yielded no agent text, and emit an event | the runner log is the only surface where this failure is visible | `TestRun_ImplementStage_BaseRebaseConflict_ReinvokeKeepsAgentPRText`, `TestAgentHandoff_ReplayEmitsEvent` |

Prefer-fresh, not replay-first, is the correct precedence: replay-first would silently discard the option-3 re-request this change also lands, and would ship a description of the slice the agent could NOT land. The cost is accepted and stated — a re-invoked agent that writes a WORSE description overwrites a good first-pass one.

**Fresh-but-MALFORMED loses to stored.** "Fresh wins" under (a) is scoped to fresh AGENT TEXT — a `prSourceAgent` read — so a re-invoked agent that DID re-write the handoff but botched it (an empty title, an unreadable file: `empty_title`, `empty_file`, `handoff_unreadable`) is a miss under (c), and the stored first-pass capture replays, discarding the fresh read's classification. This is deliberate, not an oversight: the memo exists precisely because the second pass cannot re-derive the agent's text, and a `prBodyReasonEmptyTitle` fallback is strictly worse for the PR than the first pass's real description. The malformed write does not vanish from the trace — the loader's own `pr_template_invalid` / `pr_description_legacy_unreadable` warning is emitted by the fresh read that precedes the replay, so the log carries the warning AND `pr_description_replayed` in that order. Pinned by `TestAgentHandoff_LoadOrReplay`'s `fresh malformed loses to stored` case.

A NIL receiver is a pure load that stores nothing, which is what keeps every un-armed path — the plan and acceptance stages, the held-commit resume, every fix-up pass — byte-identical.

**Two events, and how they relate to [#3012](https://github.com/kuhlman-labs/fishhawk/issues/3012)'s vocabulary.**

- `pr_description_replayed` — `{run_id, stage_id}`.
- `implement_commitmsg_replayed` — `{run_id, stage_id}`.

`pr_description_replayed` is the memo's sibling of `pr_body_recovered`: the same RESOLVED-STATE shape (composition fell back, then text was recovered, so record the recovery rather than a composition failure), reached by a different mechanism — an in-process memo across two `openPRAndShipArtifact` calls, versus backend-served text across a park/resume. They are distinct events because the mechanisms differ, but they must not disagree about what the audit records, so the memo REUSES #3012's vocabulary rather than growing a parallel one: a replay returns the stored `(title, body, kind, prBodyReason)` tuple VERBATIM, so the artifact's `pr_body_fallback_reason` resolves exactly as it did on the capturing pass. A full agent handoff replays reason `none`, which — because `emitPRBodyNotComposed` is keyed off the RETURNED reason — suppresses a spurious `pr_body_not_composed` by construction rather than by suppression machinery; `TestAgentHandoff_ReplayEmitsEvent` asserts that property rather than assuming it. A TITLE-ONLY handoff replays `body_absent` and is marked exactly as the capturing pass marked it — which means the call site emits `pr_body_not_composed reason=body_absent` a second time for the same stage, alongside `pr_description_replayed`. That double-emit is the accepted consequence of marking a replay identically to its capture rather than inventing a replay-only classification; it is pinned by `TestAgentHandoff_LoadOrReplay`'s `pr title-only replays body_absent` case so it stays a decision rather than an accident. When neither fresh nor stored text exists the FRESH fallback is returned verbatim, reason included, so a genuine absence still ships the marked placeholder and still emits `pr_body_not_composed reason=handoff_absent`.

A replayed pass emits FEWER file-level events than a real read would — no `pr_template_warning`, no `pr_description_legacy_path` on the second pass. That is deliberate single-emission (the discipline #3012 applied to `pr_body_not_composed`), and the two replay events are what make it greppable.

**`baseRebaseConflictPrompt` also re-requests both handoffs** (the issue's option 3), naming the exact absolute keyed paths via the runner's own `pullRequestDescriptionPath` / `implementCommitMessagePath` helpers so the prompt can never name a path the runner does not read, and stating that the previous attempt's handoffs were consumed. It takes `cfg` for that; the single call site in `reinvokeOnBaseRebaseConflict` is updated for the new signature. This is belt-and-braces only — the memo already closes the hole when the agent does not comply, which is the [#2658](https://github.com/kuhlman-labs/fishhawk/issues/2658) shape.

**`runVerifyFixLoop` consumes NEITHER handoff** (the `verify_fix_reinvoke` question the issue asks to check rather than assume). Verified by a caller sweep: the only production consumers of `prTitleAndBody` / `prTitleAndBodyParts` / `implementCommitMessage` are `openPRAndShipArtifact` and `openHeldCommitPR`, and the verify-fix loop calls none of them. The memo makes the question moot for any future re-invoke path added on this stage.

**Residual, deliberate.** A stage that opens a PR through any future THIRD pass inherits the memo automatically, because the memo lives on `config` rather than inside the retry block. A stage whose agent never writes a handoff at all still ships the #3012-marked placeholder, by design — the memo recovers text that existed, it does not invent text that did not.

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
is not exactly the `ob-N` shape `validFixupObligationID` accepts
(`invalid_id`), its status is not exactly `met` or `declined`
(`unknown_status`), it is `met` with an empty/whitespace `record`
(`met_without_record`), or it is `declined` with an empty/whitespace `reason`
(`declined_without_reason`). Dropping is the SAFE direction: a dropped `met`
leaves the obligation undelivered and the backend's advisory signal fires.

**The agent's free text never leaves the runner.** The fix-up agent executes
arbitrary repository commands, so it controls every byte of a `record`/`reason`.
An earlier shape of this change flattened that text's structure and shipped it
over the trace bundle into the reviewer's prompt, quarantined. That was the
wrong control: flattening and quarantining bound what the text can *impersonate*,
not what it can *carry*, so the field remained an arbitrary channel for
repository content — a secret, a private file — from the agent to the reviewer
*without ever appearing in the committed diff*. So the text is required (a `met`
must carry a record, a `declined` must carry a reason — that is what makes the
self-report an honest one) and then **discarded**: what a surviving entry carries
is `{id, status}` and nothing else.

The id is the one agent-authored value that must still cross, because the
backend's join has nothing else to key on. It is therefore constrained to a
closed, non-content-bearing shape — `validFixupObligationID`: `ob-` followed by a
positive decimal integer with no leading zero and at most four digits — rather
than merely checked non-empty, since an unconstrained id string would reinstate
exactly the channel the text fields were removed to close. An out-of-shape id can
match no declared obligation anyway, so dropping it costs nothing.

The drop log does not echo agent-authored strings either: an out-of-shape id or
status logs `<invalid>`. Pinned by
`TestLoadFixupSelfReport_ObligationTextNeverLeavesTheRunner` (adversarial, driven
end to end through `composeGateEvidence`),
`TestLoadFixupSelfReport_InvalidObligationIDDropped`, and
`TestValidFixupObligationID` (per-case table).

Survivors ride a `fixup_reporting_obligations` event that `composeGateEvidence`
folds into `gate_evidence.fixup_reporting_obligations`. The json tags MUST stay
identical to `backend/internal/bundle`'s `FixupReportingObligationEvidence` — a
one-sided edit silently DISABLES the signal, so both sides are pinned against
one shared literal JSON fixture. EVIDENCE ONLY: this block never touches
`res.OK`, `res.FailureCategory`, or the budget. Long-form contract:
`backend/internal/fixupobligation/README.md`.
