# Operating model runbook

Operator handoff for the E85 bridge-crew operating model (E85.7 / #4151): who does what, how to attach a first-officer seat, how to read the ops-monitor, how to interpret runner-observed counterfactual findings, and how to handle fan-in verdicts on a decomposed parent. Written for an agent or human picking this up with no prior context. This file paraphrases and links; it does not restate the contracts.

**Authority.** The settled decisions are ADR-089 (first-officer seat; amended by ADR-097 and ADR-101), ADR-090 (verify the merge candidate; D5 is the fan-in hold), ADR-097 (operator as orchestrator) and ADR-099 (role naming), all under `docs/adr/`. ADR-081 (crew messages) has no in-repo record yet; it is tracker issue #3727. Where this runbook and a record disagree, the record wins; where either disagrees with the code, the package README next to the code wins and this file needs a fix.

**Built vs planned.** This runbook documents only behavior on the current base. #4148 (the seat) and #4330 (host health) are PARTIAL deliveries: the identity, routes, gate hold, probes, alert row and interim dispatch hold exist; the standing-watch trigger engine, the on-request runtime, the seat spawner, the spend cap and eager retirement do not. Each section says which half is which.

## 1. Bridge-crew pattern

The model productizes what two improvised sessions did by convention: one drove, one watched read-only. Naming follows ADR-099 (nautical titles only for command and oversight; working roles take functional names; identifiers in code and API are the lowercase functional name, e.g. `first_officer`).

| Role | Who | May | May not |
|---|---|---|---|
| captain | the human approver of record for a repository | approve plans and merges; attach and retire a first-officer seat; waive, defer or route a fix-up for any concern, including the human-only ones | n/a (final authority) |
| operator | the agent that drives runs and campaigns (ADR-040, ADR-097 D1) | read gates, approve and dispatch within its delegated tier, merge, request specialist jobs, disposition concerns its tier allows | do evidence work inline (a remit, not a permission fence); attach a seat (agent callers are refused on this base; the delegated `operator_agent` grant is not built); clear a human-only concern |
| first officer | a structurally read-only seat, one per campaign or run (ADR-089) | read runs and audit, send `finding`/`notice` crew messages to the captain, request specialist jobs over REST | approve, dispatch, merge, reap, mint tokens, write tracker state, disposition its own findings (the scope clamp and the agent classification enforce this; see § 2) |
| away team | run agents (planner, implementer, reviewers, acceptance) and stateless specialist jobs | the one stage or one typed job they are dispatched for | anything outside the dispatched stage's scope; a specialist job never pushes, commits to a run branch, or files issues |

Authority sits with the captain; the operator's autonomy tier (docs/METHODOLOGY.md) decides which gate actions it may take without asking. A first-officer finding never decides anything: it is evidence, and only a finding resting on product-produced evidence blocks (§ 2).

**What replaced each improvised duty.**

| Improvised duty | Product mechanism | Runbook section |
|---|---|---|
| watching host disk, memory, load, Docker, Postgres, stranded runners | ops-monitor (E85.1, E85.2, E85.11) | § 3 |
| running counterfactual batteries against a plan's controls | plan-declared counterfactuals executed by the runner (E85.3) | § 4 |
| building the consolidated fan-in head and running its tests | ADR-090 D5 fan-in verify hold; `fanin_build` specialist job as extra evidence | § 5 |
| noticing a consolidated PR fell behind its base | `merge_candidate_staleness` block (E85.4) | § 5 |
| cross-session messages with no record | ADR-081 crew messages on the audit chain (tracker #3727) | § 2 |
| a boundary the overseer chose to keep | token scope clamp plus a permission error and an audit row | § 2 |

## 2. Attaching a first-officer seat

**What exists.** A seat is an API token (no new table) whose subject is `first_officer/campaign/<uuid>` or `first_officer/run/<uuid>`. The routes, the scope clamp, the lazy retirement and the gate hold are built. Nothing in the product starts an agent under that token: running a read-only agent session with it is the operator's act today. Not built (remaining scope on #4148): the standing-watch trigger engine and on-request runtime (ADR-097 D3/D4), the seat spawner, the ADR-089 D6 spend cap, eager retirement, per-seat rate bounds, per-account token binding, and the approver-members wrapper. The lease-scoped seat of ADR-101 is #4421.

**Who may attach.** Attaching is a captain act, and REST only (no MCP verb or CLI command wraps it on this base).

- The caller needs `write:approvals` and a human identity. An agent caller of any kind is refused before the anchor is even read.
- While a captain is sitting for the anchor's repository, only that subject may attach; with the captain seat vacant or unreadable, any human holding `write:approvals` may, and the basis is recorded on the attach row.
- One seat per anchor. Concurrent attaches converge to the oldest token; a loser is refused.

**Routes** (contract: `docs/api/v0.md` § "First-officer seats"; server behavior: `backend/internal/server/README.md` § "First-officer seat"):

| Route | Scope | Notes |
|---|---|---|
| `POST /v0/first-officer-seats` `{campaign_id \| run_id}` | `write:approvals` | answers 201 with the token ONCE; the plaintext is never stored or logged |
| `GET /v0/first-officer-seats?campaign_id=\|run_id=` | `read:runs` | attach state, token ids, anchor state |
| `POST /v0/first-officer-seats/retire` `{campaign_id \| run_id, reason}` | `write:approvals` | `reason` required (1-1000 chars); allowed on a terminal anchor |

**Clamped scope profile.** The bearer middleware intersects every seat-subject token with `read:runs`, `read:audit`, `write:messages` and `request:specialist_jobs`, even a hand-minted token that claims more. A seat's crew sends are limited to `finding` and `notice` addressed to the captain, on a run inside its seat. Specialist-job requests go over REST only; the MCP verb that spawns a host program stays out of reach. Each 403 served to a seat appends a `first_officer_action_refused` row.

**Refusals and what to do.**

| Code | HTTP | Meaning | Operator action |
|---|---|---|---|
| `first_officer_attach_requires_human` | 403 | caller is an agent identity | re-issue the call under the captain's human token |
| `first_officer_attach_not_captain` | 403 | a captain is sitting and the caller is someone else | have the sitting captain attach |
| `first_officer_anchor_terminal` | 409 | the campaign or run already ended | attach to a live anchor instead |
| `first_officer_seat_exists` | 409 | an active seat token exists, or a concurrent attach lost | read the existing seat with GET; retire it first to replace it |
| `first_officer_seat_not_attached` | 404 | retire found no active token for the anchor | nothing to retire; confirm the anchor id with GET |
| `first_officer_seats_unconfigured` | 503 | the token, audit, run or campaign store is not wired in this deployment | fix the deployment; no seat route works until it is |
| `first_officer_seat_invalid` | 401 | seat token with a malformed subject | revoke the token; re-attach |
| `first_officer_seat_unverifiable` | 503 | the anchor could not be read (or a store is unwired) | retry; nothing is revoked, so the seat is intact |
| `first_officer_seat_retired` | 401 | the anchor is terminal or gone; the seat's tokens are now revoked | expected end of life; attach a new seat to a new anchor |
| `first_officer_finding_open` | 409 | approve or merge held by an open `first_officer` concern | disposition the concern (below) |

This table covers the attach, retire and seat-use paths. Other seat refusals (`first_officer_cannot_mint_tokens`, `first_officer_tracker_write_not_permitted`, `first_officer_subject_out_of_seat`, `first_officer_evidence_unreadable`, and 500s from a failed attach convergence) are listed in `backend/internal/server/README.md` § "First-officer seat" and `backend/internal/firstofficer/README.md`. A 500 from attach states whether the issued token was withdrawn; if it says the token may still be active, retire the seat.

**From finding to hold.** A seat `finding` that cites, by audit sequence, a runner-attested FAILING specialist-job completion on its anchor run is blocking: the server inserts an open concern with provenance `first_officer`, then marks the finding accepted. While that concern is open, plan approval, review approval, merge, the delegated auto-drive arms and the gate view's `merge_readiness` all hold. Every other seat finding is advisory and holds nothing. Judgement alone never blocks (ADR-089 D4).

**Disposing a blocking finding.** Use the existing concern surface; there is no separate mechanism: `fishhawk_waive_concern` (reason required), `fishhawk_defer_concern` (to an issue), or route a fix-up with `fishhawk_fixup_stage`. The seat cannot disposition its own finding: it is classified as an agent, and the clearing routes that need a human refuse it. Concern semantics: `backend/internal/concern/README.md`.

**Residuals to know.**

- Retirement is lazy: it happens on the seat's next request after the anchor ends, so a quiet seat keeps a live token until then. Retire by hand when a campaign is abandoned.
- No per-seat bound: a looping seat agent can amplify refusals into audit rows and requests into queued specialist jobs. Revoke or retire the seat if a session misbehaves.
- A refusal that skips the normal error path (the `/mcp` tool-scope precheck) is a permission error with no `first_officer_action_refused` row.
- Rollback of a deployment: retire live seats first, because an older server no longer clamps them.

**Where `fanin_build` fits.** ADR-097 defines `fanin_build` as a specialist-job kind (request body and result: `docs/spec/specialist-job-v1.md`). It builds the consolidated head in a scratch clone and returns a runner-attested result. It is evidence a seat can cite; it is NOT what releases the fan-in hold (that is the product's own merge-candidate verify pass, § 5). A seat finding that cites a failing `fanin_build` completion becomes a blocking `first_officer` concern as above.

## 3. Ops-monitor notifications

**Enable.** Off by default. `fishhawkd serve --enable-ops-monitor` starts the ticker; `fishhawkd ops-probe` runs one dry sweep with the same probes and flags and writes nothing (exit 0 nothing failing, 1 any failing result). Flag and env names, defaults and validation: `backend/cmd/fishhawkd/README.md` § "Ops monitor". The flags operators reach for first are `--ops-monitor-notify-issue`, `--ops-monitor-interval`, `--ops-monitor-quiet-window`, `--ops-monitor-deployment`, the `--ops-monitor-amendment-*` trio and the host-health threshold flags. Probe semantics and the fail-safe sweep: `backend/internal/opsmonitor/README.md`.

**Deployment gate.** The host-local probes (disk, gocache, load, docker, runners, memory, workload_containers) run only when the resolved deployment is `local`; under `kubernetes` or `remote` they read `not_applicable`. Pass `remote` on a non-Kubernetes host that does not share the runners' host. Postgres and the two amendment probes run everywhere.

**Where a notification lands.**

| Surface | What | Notes |
|---|---|---|
| global audit chain | one `ops_probe_failed` row per failing condition and subject per quiet window | readable by every tenant member on `GET /v0/audit` (rows carry `category: ops_probe_failed`); carries observed value, threshold and remedy, never host paths or argv |
| issue comment | one comment on the `--ops-monitor-notify-issue` issue, with a hidden `fishhawk-ops-monitor:v1` marker | optional; unset means audit-only. The issue must be access-controlled because comments name pids and run ids |
| global audit chain | one `ops_monitor_alert` row per tick that has a host-health crossing | the trigger record the future seat standing watch will read; not a comment surface; written only on a clear-to-failing edge, so a steady pressured host writes none |

**Status meanings.**

| Status | Meaning | Operator reading |
|---|---|---|
| `failing` | observed and out of threshold | act on the remedy; one row and comment per quiet window while it persists |
| `unknown` | the probe could not observe (error, timeout, panic, missing input, or a tracker it cannot fully see) | NOT healthy and NOT alerting: it only WARN-logs once per probe per window and shows in `ops-probe` output. A probe stuck on `unknown` is a blind spot to fix |
| `ok` | within threshold | none |
| `not_applicable` | host-local probe under a non-local deployment | none |

**Dedup and quiet window.** Each (condition, subject) is notified once per quiet window (default 1h), including across a fishhawkd restart (the audit lookup restores it). An orphaned runner needs two consecutive sweeps to confirm, so worst-case detection latency is two intervals.

**Condition to first action.** The remedy text lives in code: `opsmonitor.RemedyFor` and the `remedies` map in `backend/internal/opsmonitor/opsmonitor.go` are the source of truth, and a memory or container failure composes its own remedy naming the largest holders. This table is only the first move.

| Condition | First operator action |
|---|---|
| `disk_free_low` | free the data volume: `scripts/dev sweep`, `scripts/dev gocache --trim`, prune Docker, remove stale worktrees |
| `gocache_over_threshold` | `scripts/dev gocache --trim` (it refuses while a run is live); never `go clean -cache` under a live run |
| `host_load_high` | look for orphaned `sh`/`zsh` children of pid 1 that no run owns and kill them; a verify started now may fail on load, not on the change |
| `host_load_io_wait` | load is waiting on I/O or swap-in, not CPU: free memory (the memory probe names the holders) and run fewer I/O-heavy workloads |
| `docker_unreachable` | restart Docker, confirm `docker info` answers, then `fishhawk_reap_stage` any stage the outage stranded |
| `storage_eio` | free host disk, restart Docker, confirm Postgres is healthy, then reap stranded stages |
| `postgres_unreachable` | check the Postgres container and its logs, restart it, then reap stages stranded by the outage |
| `orphaned_runner` | confirm the pid is a `fishhawk-runner` whose stage is terminal, then kill it; it can block `reload` and `post-merge` |
| `amendment_window_closing` | decide now with `fishhawk_decide_scope_amendment`; after the window the agent stops waiting and a later approval lands only through `fishhawk_retry_stage` |
| `amendment_pending_on_terminal_stage` | deny the amendment to close it, or approve it and run `fishhawk_retry_stage` |
| `host_memory_pressure` | free memory from the contributors the row names; if the dispatch hold is on, new host dispatches wait (below) |
| `workload_containers_high` | let in-flight stages finish, stop idle gate containers, run fewer stages concurrently |

**Interim dispatch hold.** With `--ops-monitor-dispatch-hold` on a `local` deployment, CRITICAL memory pressure makes the host-dispatch marker refuse NEW admissions with `409 host_memory_pressure` (details carry a reason and a retry note). Nothing in flight is touched, and the stage stays where it was: re-dispatch once the condition clears. The hold reads as released when the ticker has not updated it for three intervals, so a dead monitor cannot pin dispatch shut. It is interim; ADR-100's adaptive concurrency absorbs it. Contract: `backend/internal/server/README.md` § "Interim host memory hold".

**Residuals.** Destructive host remediation stays a human act (ADR-089 D9): the monitor recommends, it never cleans. The seat that turns an `ops_monitor_alert` row into an advisory finding is not built (#4330, #4148); today the row is an audit record you read. Slack parity for ops conditions has no surface yet. Host-health probes cover darwin and linux; a podman-only host reads `unknown` for `workload_containers`.

## 4. Counterfactual gate findings

**Declaring.** A plan may carry an optional `counterfactuals` array, at most 8 entries. Each names a control the change adds or tightens (`control_path`), a mutation that disables it (either find/replace, or a patch limited to that one file), the Go tests that should fail without it (`tests.run`, `tests.package`), and `expect: red`. Schema and validation rules: `docs/spec/plan-standard-v1.md` § `counterfactuals`. A plan that also splits into sub-plans draws a `counterfactuals_not_executed_for_decomposition` warning and its entries never run; declare them in the sub-plan that owns the control.

**What the runner does.** After an implement or fix-up pass's committed-tree verify passes, the runner executes every delivered declaration against a fresh scratch clone of the verified head, never the lineage worktree: an unmutated baseline run that must pass, then the mutated run, then a restore. It classifies from the test event stream, not free text, so an environment failure cannot read as RED. The runner signs the observations into the trace bundle; the backend records each admitted one as a `counterfactual_observed` audit row. Contract: `runner/README.md` § "Plan-declared counterfactuals"; backend half: `backend/internal/server/README.md` § "Runner-observed counterfactuals".

**Per-observed-value action.**

| `observed` | Recorded | Concern | Operator action |
|---|---|---|---|
| `red` | row | none | pinned; no action |
| `green` | row | one open `server_check` concern, category `control_unpinned`, medium severity | the mutated control left the named tests passing, so the control is unpinned. Before concluding the test is weak, check the declaration against the head: does the mutation change behavior, and does the test selector reach the control, or does a second guard on that path make the deletion unobservable? Then strengthen the test with a fix-up (`fishhawk_fixup_stage`), or waive with a reason (`fishhawk_waive_concern`), or defer to an issue (`fishhawk_defer_concern`) |
| `timeout` | row | none | recorded only; the 5-minute run or 20-minute phase budget ran out; re-examine the declaration (a narrower selector, a smaller package) |
| `error` | row | none | recorded only; the `reason` field says why (for example `baseline_not_green`, `build_failed`, `no_tests_matched`, `mutation_not_applied`, `mutation_ambiguous`, `control_not_built`); fix the declaration, because an `error` is not evidence the control works |

**Who may clear a `control_unpinned` concern.** Only a human: an agent token or a delegated request is refused with 403 `concern_requires_human`. While the concern is open, delegated settlement cannot fire (the `may_approve` and `may_merge` conditions require zero open concerns), and the gate view lists it under `open[]`. The human `fishhawk_merge_run` verb does not consult open concerns, so only delegated settlement is held; read `open[]` before merging by hand.

**A missing `counterfactual_observed` row has two different causes.**

1. **Non-execution** (the runner never produced an observation). The runner executes counterfactuals only on a successful implement pass (or fix-up) that was delivered declarations. It does not execute on: a plan with no declarations; a decomposed plan's parent; a pass whose committed-tree verify failed; a merge-candidate verify-only pass (ADR-090 D3), which never reaches the counterfactual phase; and a redacted upload, which records nothing. Check the plan's `counterfactuals` array and the stage's gate evidence first.
2. **Admission rejection** (the runner executed it, the backend refused the observation). Only an executed observation can be rejected at admission, and only for: an `index` that is not a position in the approved plan's `counterfactuals` (undeclared), an `observed`, `baseline` or `reason` value outside the closed vocabulary (`testdata/wire/counterfactual_outcomes.json`), or more than 8 in one bundle. Each rejection is a named WARN in the fishhawkd log and gets no row. A re-POST of the same bundle appends nothing and raises nothing (dedup on stage, content hash and index).

## 5. Fan-in verdicts

A decomposed parent integrates its children's slice branches into one consolidated branch and opens one consolidated PR. Three mechanisms judge that head; this section is the reading guide. Fan-in mechanics: `backend/internal/orchestrator/README.md` § "Fan-in integration"; the verify machinery: `backend/internal/server/merge_candidate_verify.go` and `backend/internal/server/fanin_verify.go`.

**The ADR-090 D5 hold.** The consolidated implement review is not dispatched until the consolidated head's merge-candidate verify state allows it. `DispatchConsolidatedReview` asks `holdConsolidatedReviewForFanInVerify` once the head is resolved. The state is derived from the head's origin, the workflow's declared verify command and the audit chain:

| Verify state of the head | Review | Why / operator action |
|---|---|---|
| `passed` for exactly this head | released | the recorded result is rendered to the review as parent-level verify evidence |
| `not_required` | released | no verify command is declared (ADR-090 D4), or the head is one the runner already gated, or no Fishhawk write that needs a verify produced it |
| `unverified` | held; the server starts the verify-only pass | the pass runs as a re-opened parent implement stage: dispatch it (the hold's log line says so). If the start itself finds the state already `passed` or `not_required`, the review releases at once |
| `in_flight` | held | a pass is live for this head; its report releases the hold |
| `failed` | held; nothing new started | route a fix-up (below); a fix-up's new head is gated by the runner, so the review can proceed after it |
| state unreadable, pass could not be started, or unknown state | held (fail-closed, WARN in the log) | read the log line, fix the cause (audit store, runner capability), re-read the gate view |

The hold releases through the verify pass's report: the report arm settles the re-opened implement stage and advances the run, which re-enters the consolidated review. The hold is not tied to one cause value; `fan_in` is only the cause a decomposed parent's head classifies as when no earlier rule matched. A head produced by a base merge or a conflict-resolution push classifies as `base_advance` or `conflict_resolution` and is judged the same way.

**Reading the gate view.** `fishhawk_get_gate_view` (and the REST gate view) carries these blocks for a decomposed parent:

| Block | Meaning |
|---|---|
| `merge_candidate_verdict` | the verify state (`state`, `cause`, `source`) for the run's CURRENT head, plus the verdict-transfer mode and shadow evidence |
| `fan_in_conflict` | an UNRESOLVED conflict: `kind` is `slice_conflict` or `base_fold_conflict`; carries `conflicting_slice_index`, `conflicting_child_run_id`, `base_ref`, `base_sha`, `conflicting_base_paths`, and a `remedy` naming the verb |
| `merge_candidate_staleness` | `state` is `fresh`, `stale` or `undetermined`, with `behind_by`, `reason`, `next_step`, `checked_at` (E85.4, below) |
| `history_gaps` | an entry named `fan_in` or `merge_candidate_staleness` means that block's read failed and was omitted, so absence of the block is NOT proof there is no conflict or staleness |

**Staleness (E85.4).** `merge_candidate_staleness` (also on `GET /v0/runs/{id}`) tells you BEFORE you try to merge whether the consolidated PR head contains the base branch's current tip. It is a read-time, read-only observation: three forge reads, no write, no audit row, no background sweep. Present only on an open decomposed parent with a PR; omitted once the PR is merged or closed. It is cached per run and recorded head for 60 seconds, so `checked_at` says how old it is. It is advisory; the merge gate re-reads the forge and is the authority.

| `reason` (on `undetermined`) | Meaning |
|---|---|
| `forge_not_probeable` | no GitHub client or installation (every GitLab-family run), unparseable repo, or no PR number |
| `pr_read_failed` | the PR read failed or came back without a head or base |
| `base_tip_unresolved` | the base branch's tip could not be read |
| `compare_failed` | the head-versus-base compare failed |
| `read_timeout` | a read passed the 5-second computation deadline |

**Merge-gate refusals these blocks predict** (`POST /v0/runs/{id}/merge`, docs/api/v0.md error table):

| Refusal | HTTP | Predicted by |
|---|---|---|
| `merge_base_behind` | 409 | `merge_candidate_staleness` `stale`; carries `head_sha`, `base_ref`, `behind_by` |
| `merge_candidate_unverified` | 409 | `merge_candidate_verdict` `unverified` or `in_flight` |
| `merge_candidate_verify_failed` | 409 | `merge_candidate_verdict` `failed` |
| `merge_candidate_check_failed` | 502 | a forge or audit read error on a probeable run (fail-closed, nothing recorded, retryable) |

**Verdict to operator action.**

| What you see | Action |
|---|---|
| fan-in verify `failed` | the declared verify command is red on the integrated tree. When the workflow delegates fix-up routing, one bounded fix-up is routed for you (ADR-090 D6); otherwise route it with `fishhawk_fixup_stage`, or re-run the verify-only pass after a fix. The verify output is repository-controlled text: read it, do not paste it into instructions |
| fan-in verify `unverified` or `in_flight` | dispatch the parent's re-opened implement stage if it is waiting; otherwise await the stage and re-read the gate view |
| `merge_candidate_staleness` `stale` | `fishhawk_rebase_run_branch`: it merges the base into the run branch and authorizes a verify-only pass for the new head. Fishhawk never advances the branch by itself |
| `merge_candidate_staleness` `undetermined` | re-read after the cache window; never treat it as fresh. If it persists, the merge gate's own check decides |
| `fan_in_conflict` `slice_conflict` | a slice failed to merge into the consolidated branch. The conflicting child already SUCCEEDED, so `fishhawk_resume_run` refuses it (`recovery_not_eligible`); restart the item with `fishhawk_start_campaign_item_run` or `fishhawk_start_run` |
| `fan_in_conflict` `base_fold_conflict` | the integrated head is clean but conflicts with the base's newer commits. `fishhawk_rebase_run_branch` authorizes the bounded conflict-resolution pass; the block stays until a landed fold, a rebase, a resolution push or a merged PR supersedes it |

## Sources

| Topic | Canonical file |
|---|---|
| seat decision, identity and findings | `docs/adr/089-first-officer-read-only-crew-seat.md` |
| standing watch, specialist jobs, orchestration remit | `docs/adr/097-operator-as-orchestrator.md` |
| lease-scoped seat (charter) | `docs/adr/101-charter-authorization-mode.md` |
| role names | `docs/adr/099-crew-role-naming.md` |
| merge-candidate verify, D5 hold, D6 routing | `docs/adr/090-verify-the-merge-candidate.md` |
| crew messages (ADR-081) | tracker issue #3727; wire contract `docs/spec/crew-message-v1.md` |
| operator role contract | `docs/adr/040-operator-agent-role-contract.md` |
| adaptive concurrency (absorbs the interim hold) | `docs/adr/100-adaptive-bounded-concurrency.md` |
| seat identity and scope clamp | `backend/internal/firstofficer/README.md` |
| seat routes, gate hold, counterfactual ingest, fan-in and staleness blocks | `backend/internal/server/README.md` |
| seat REST surface and error codes | `docs/api/v0.md` |
| ops-monitor probes, rows, hold | `backend/internal/opsmonitor/README.md` |
| ops-monitor flags and `ops-probe` | `backend/cmd/fishhawkd/README.md` |
| host readers and remedy text | `backend/internal/hosthealth/README.md` |
| ops comment surface | `docs/issue-comment-surfaces.md` |
| counterfactual declaration schema | `docs/spec/plan-standard-v1.md` |
| counterfactual execution | `runner/README.md` |
| concern provenance and human-only clearing | `backend/internal/concern/README.md` |
| specialist-job contract | `docs/spec/specialist-job-v1.md` |
| fan-in integration | `backend/internal/orchestrator/README.md` |
| autonomy tiers | `docs/METHODOLOGY.md` |
