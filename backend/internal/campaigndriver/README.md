# backend/internal/campaigndriver

Campaign driver: the ticker that mechanically advances running campaigns — settling items whose runs terminate, starting newly-eligible items under a concurrency budget, auto-driving run gates under delegation, and pausing/paging on hand-offs. E25.5 / #1444, ADR-047 Track C.

## Ticker: ADVANCE then START

`Ticker.Run` polls every `--campaign-driver-interval` (default 60s, `campaigndriver.DefaultInterval`) and, for each campaign in state `running` (`CampaignStore.ListCampaigns` filtered to `running`), runs two passes per campaign — **ADVANCE** then **START**, re-reading the items between them so a predecessor that reaches terminal this tick settles AND its now-eligible dependent starts in the SAME tick.

- **ADVANCE**: each running item whose linked run reached a terminal run state (`run.State.IsTerminal()` via the narrow `RunReader.GetRun`) is transitioned to the mapped terminal item-state (`succeeded`/`failed`/`cancelled`, guarded by `campaign.ValidCampaignItemTransition`) and emits `campaign_issue_settled`; the campaign state is then re-derived (`campaign.DeriveState`) and transitioned with a `campaign_advanced` emit when it changes.
- **START**: items partitioned `Eligible` by the E25.3 pure engine (`campaign.NextEligible`) are started up to a per-campaign concurrency budget (`MaxParallel` − currently-running, default `DefaultMaxParallel`=4) through the `RunStarter` seam, linked (`SetCampaignItemRun`), transitioned to `running`, and recorded with `campaign_issue_started`. A `RunStarter` error that is a `*StartRefusal` ends the campaign's START pass (§ "Campaign-level start refusal").

These audit kinds, and `campaign_auto_start_refused` below, ride the GLOBAL chain (`AppendGlobalChained` — a campaign is not a run; the run linkage is in the payload), best-effort (see `docs/issue-comment-surfaces.md`).

## Campaign-level start refusal (E88.1 / #4525)

A `RunStarter` returns `*StartRefusal{Reason, RunnerKind, RunnerKindSource, Detail}` when the CAMPAIGN cannot be auto-started, as opposed to a transient per-item failure. The serve.go adapter fills it from `server.ResolveCampaignAutoStart` (recorded campaign `runner_kind` > `--auto-start-runner-kind`; a `local` resolution inherits the campaign `working_dir`, re-validated absolute), with reasons `working_dir_unbound`, `working_dir_not_absolute` and `campaign_runner_kind_invalid`. `start()` matches it with `errors.As` and RETURNS, so no item of that campaign is started, linked or transitioned that tick; any other starter error keeps the warn-and-continue posture.

The driver records `campaign_auto_start_refused` (payload `{campaign_id, issue_ref, reason, runner_kind, runner_kind_source, detail}`) once per (campaign, reason), deduplicated across ticks by a mutex-guarded in-memory map on the `Ticker`. The key is marked only after the append succeeds, so a failed append retries on the next tick; it is cleared when a later start for the campaign succeeds, and pruned for every campaign no longer listed `running`, so a resumed campaign records a standing refusal again. A restart records each standing refusal once more. The WARN log fires only when recording. `detail` is server text that never echoes the `working_dir` or any issue or campaign text. The row is run-bound-readable under ADR-106: it is campaign-scoped and names no run, so it exposes no other run's data. `GET /v0/campaigns/{id}/status` derives the same refusal into its `auto_start_refusal` block (`backend/internal/server/docs/endpoints.md`).

## RunStarter seam

The `RunStarter` is satisfied in `serve.go` by `campaignRunStarter`, a thin adapter over `server.Server.StartRunForCampaignIssue` (`runs.go`) — which resolves the GitHub App installation, fetches+parses the workflow spec at `--campaign-driver-workflow-ref` (empty = the repo's default branch; the fetched blob SHA becomes the run's `workflow_sha`) for `--campaign-driver-workflow-id` (default `feature_change`), then mints the run via the EXTRACTED `server.Server.CreateRunForTrigger` (the single run+stage-creation seam `handleCreateRun` also routes through).

A campaign carries no workflow context (E25.2), which is why the driver resolves it from the repo spec at start time.

## Off by default, fail-closed start

Enable with `--enable-campaign-driver` (`FISHHAWKD_ENABLE_CAMPAIGN_DRIVER=true`). The fail-closed switch (`campaignDriverStartDecision`, unit-tested) refuses to start — WARN-logs and constructs no ticker — when `CampaignRepo`/`RunRepo`/`AuditRepo` is unwired OR the GitHub client is absent (the run-starter needs it to resolve the spec); a flag-off start is a silent no-op.

Per-item/per-campaign errors WARN-log and never abort the tick (mirrors the `deployreconciler`/`childcompletion` posture).

## Auto-drive on each run gate (E25.6 / #1445, ADR-047 Track C)

Layered on the mechanical advancement, during the ADVANCE pass every running item whose linked run is NON-terminal is handed to the optional `GateActor` seam (`Ticker.driveGate`) BEFORE terminal observation.

The seam is satisfied in `serve.go` by `campaignGateActor`, a one-line delegation to `server.Server.DriveCampaignRunGate` (`campaign_gate_outcome.go`), which calls `server.Server.AutoDriveRunGate` unchanged and translates its outcome. The adapter binds the campaign operator identity (`campaignOperatorIdentity` — subject `operatorrole.CampaignActorSubject`, the `operatorrole.CampaignActorScopes` write scopes, a non-empty TokenID so the handler scope check is ENFORCED not bypassed) and a `githubAutoMerger` (`EnableAutoMerge`, squash).

`AutoDriveRunGate` re-evaluates the run's `operator_agent` delegation in-process (read-only) and, for a delegated knob whose condition is met AND whose real gate state independently matches (double-gating, fail-CLOSED to observe-only on any mismatch / evaluation error / unrecognised condition / unconfigured merge seam), takes the gate action — `may_approve`→approve, `may_route_fixup`→fixup, `may_retry`→retry, `may_merge`→merge.
Actions go via the EXTRACTED identity-parameterised service methods (`approveStageAs`/`fixupStageAs`/`retryStageAs`), recording the run-level audit stamped `audit.ActorAgent`. The driver then records a campaign-level `campaign_gate_acted` marker on the global chain.

A `must_page_human` condition (`reviewer_reject`, `requirement_arbitration`) is REFUSED: the actor emits the `campaign_gate_paged` hand-off (run chain) and takes NO action.

**Auto-drive is OPTIONAL and fail-closed**: `newCampaignGateActor` returns nil — the driver then runs OBSERVE-ONLY, leaving every gate parked for the human operator-agent — when the GitHub merge client is unconfigured (`campaignDriverStartDecision` already refuses to start the driver without it, so the nil path is defensive).

## Decision-required marker (E88.4 / #4528)

When `AutoDriveRunGate` cannot auto-act on an expected gate state it returns `DecisionRequired` with a `DecisionState` (`human_quorum_required`, `delegated_approval_no_progress`, `fixup_budget_exhausted`, `fixup_ceiling_reached`) and the `Action` whose arm surfaced it. `DriveCampaignRunGate` carries both into `GateActionOutcome` and then reads the run's stages once: `GateKnown=true`, `GateStageID` = the earliest parked plan/implement `awaiting_approval` stage (`gatedReviewStage`, nil when none), and `ParkedGateStageIDs` = every parked plan, implement or review stage. It skips the read, leaving `GateKnown=false`, when the evaluation errored, when `RunRepo` is unwired, or when the outcome is not a decision and the driver's `WithDecisionKeyHeld` context hint says it holds no key for the run (nothing could be cleared). A failed read also leaves `GateKnown=false`.

`Ticker.driveGate` calls `trackDecision` once per driven run, after the actor-error return (an actor error never appends and never clears):

- **Hydrate.** Once per run per process, the driver reads the run's `campaign_gate_decision_required` and `campaign_gate_decision_cleared` rows. When the latest marker has a higher `Sequence` than the latest clear, its payload seeds the stored key, so a fishhawkd restart neither duplicates a marker nor loses a clear. A read error WARN-logs and defers: nothing is appended or cleared this tick, and the next tick retries. A malformed marker payload WARN-logs and seeds no key.
- **Record.** A `DecisionRequired` outcome with `GateKnown` is keyed `(run_id, decision_state, action, gate)`, where `gate` is `GateStageID` or the literal `run` (`GateRunLevel`). The key excludes the stage's `updated_at`. When the key differs from the stored one, the driver appends ONE `campaign_gate_decision_required` row on the run chain (`AppendChained`, `StageID` = the gate stage or nil, actor `system`) and stores the key only after the append succeeds. A changed decision appends a new marker that supersedes the old one, with no clear row. A decision with `GateKnown=false` is deferred and never re-keyed to `run`.
- **Clear.** A non-decision outcome clears a stored key only when its own stage read positively shows the keyed gate no longer parked (approval condition C1): a stage key when its stage is absent from `ParkedGateStageIDs` (the keyed stage's own state is the evidence), a run-level key only on an `Acted` or `Paged` outcome whose read shows no plan, implement or review stage parked. An indeterminate outcome (`GateKnown=false`) never clears. Nor does an observe-only outcome clear a run-level key: `AutoDriveRunGate` answers observe-only with a nil error on its own fail-closed reads, and that is indistinguishable from a positive observe-only, so the key is held until the driver acts or pages, or the run goes terminal (the key is then dropped with no clear row). The clear appends ONE `campaign_gate_decision_cleared` row, with `cleared_by` `acted`, `paged` or `no_decision`, and deletes the key only after the append succeeds.
- **Prune.** At the end of each tick the driver drops the stored key and hydration flag of every run it did not drive (settled terminal, unlinked, or its campaign left `running`). No clear row is written for a terminal run, because the status read ignores terminal runs; a run driven again re-hydrates from its chain.

A failed append WARN-logs (`campaigndriver: decision marker append failed` / `campaigndriver: decision clear append failed`, with `campaign_id`, `run_id` and the error) and leaves the stored key unchanged, so the next tick retries. All maps are mutex-guarded.

The payload is exactly `{campaign_id, issue_ref, run_id, gate, decision_state, action}` for the marker, plus `cleared_by` for the clear. It carries no cross-run identifiers: `out.Note` is never recorded, because it can name other runs. Both categories are registered in `audit.KnownCategories` as an interim registration under ADR-106's existing audit-category mechanism; no new ADR is needed, and they take the mandatory run-bound flag via #4599 / E89 #4537. Both are run-bound readable, INTERNAL, and not issue-comment surfaces. The dedupe assumes a single campaign driver per deployment (the E88.3 `scripts/dev` stack runs one); two concurrent drivers could double-append.

A review-type gate never yields `DecisionRequired` and never gets a marker; `GET /v0/campaigns/{id}/status` shows a parked review stage from stage state in its per-item `awaiting` block. That block's stage-state fields are intentionally ungated, because the item's `run_id` is already in the item payload; only its decision sources (these markers and pending amendments) are gated.

Open questions from #4528, answered from code (no behaviour change here):

1. **Delegated merge arm vs `fishhawk_merge_run`.** The delegated `may_merge` arm shares `merge_run`'s per-run merge-intent lock, its ADR-093 queue-ownership deferral, the ADR-090 merge-candidate gate, the ADR-112 forge-approval fold and the merge-dispatch ledger claim, but it never ENQUEUES: where `merge_run` answers `202 queued`, a behind or unverified candidate is observe-only for the delegated arm.
2. **Dispatcher token profile.** `hostdispatcher.Scopes()` is `read:runs`, `read:audit`, `write:runs`, `write:stages`, clamped in bearer auth and confined to the `hostdispatcher.Routes()` allowlist (`backend/internal/hostdispatcher/README.md`).
3. **Behind-base hold while the review gate is parked (corrected).** The delegated merge arm reaches the merge-candidate gate only when NO stage is `awaiting_approval` (`mergeGateReady`, `backend/internal/server/autodrive.go`). In `feature_change` the review stage stays `awaiting_approval` until merge (ADR-018), so the behind-base hold is not reached while the review gate is parked. Where it is reached, it is observe-only and records no marker; it clears a recorded stage key on that tick only when the stage read shows that stage no longer parked, and never clears a run-level key. The status `awaiting` block then shows the item from stage state (`system`/`no_active_stage` when every stage is terminal); a still-current run-level marker shows as `human`/`run_decision`.

## Pause/page on the hand-off (E25.7 / #1446, ADR-047 Track C)

The driver ACTS on the `out.Paged` refusal in `Ticker.pageGate`:

- It pauses the affected item (`CampaignStore.PauseCampaignItem`, recording the `campaign.PauseReason` `{page_event, run_id}` as JSONB on the item) and, unless the campaign's `pause_policy` is `pause_item` (continue-others; the default `pause_campaign` blocks the whole campaign), transitions the campaign `running`→`paused`.
- It records a campaign-level `campaign_paused` marker (payload `{campaign_id, issue_ref, run_id, page_event, policy}`) on the global chain and fires the human page through the optional `Notifier` seam (`NotifyStatusUpdateForRun`, satisfied in `serve.go` by the existing `issuecomment` notifier — nil → observe-only, pause recorded but no page; the typed-nil interface trap is avoided by only assigning a non-nil notifier).
- A campaign already `paused` is sticky — `deriveAndTransition` skips re-derivation so a sibling settling can never auto-unpause it, and the START pass is skipped once a tick leaves the campaign non-running.
- Resuming is an explicit operator action (`POST /v0/campaigns/{id}/resume`, E25.7 Slice 3), never a derivation.

## The ratified grooming order is read exactly once, and never here (E54.6 / #2238)

A campaign can be created from an approved grooming run's ratified priority order
(`POST /v0/campaigns` with `grooming_source`). That order is resolved ONCE, at
campaign assembly, and written down durably — `campaign_items.queue_position` for
the order itself and `campaigns.grooming_source` for its provenance (migration
`0074`). **This package reads those rows and nothing else.**

The invariant is not a preference. The order is a RATIFIED artifact: an operator
approved that specific report at that specific content hash. A mid-campaign
re-read would re-derive the queue from whatever the backlog looks like *now* — a
second, unapproved source of truth for a decision the operator already made, and
one that would change a running campaign's behaviour with no gate and no audit
row.

`no_grooming_read_test.go` enforces it technically: no non-test file in this
package may name a grooming/board read symbol (`ParseGroomingReport`,
`KindGroomingReport`, `OrderFromReport`, `ReorderByPriority`, `GroomingOrder`, …)
or import `backend/internal/workmgmt`. It is a SOURCE-level check over this
package only — tamper-evident within the repo, not a runtime capability boundary,
and it bans the reading vocabulary rather than every conceivable spelling. The
complementary behavioural control is the counting-fake test in
`backend/internal/server`, which proves exactly one grooming-report artifact read
per create and zero afterwards.
