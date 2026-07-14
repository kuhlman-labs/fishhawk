# backend/internal/campaigndriver

Campaign driver: the ticker that mechanically advances running campaigns — settling items whose runs terminate, starting newly-eligible items under a concurrency budget, auto-driving run gates under delegation, and pausing/paging on hand-offs. E25.5 / #1444, ADR-047 Track C.

## Ticker: ADVANCE then START

`Ticker.Run` polls every `--campaign-driver-interval` (default 60s, `campaigndriver.DefaultInterval`) and, for each campaign in state `running` (`CampaignStore.ListCampaigns` filtered to `running`), runs two passes per campaign — **ADVANCE** then **START**, re-reading the items between them so a predecessor that reaches terminal this tick settles AND its now-eligible dependent starts in the SAME tick.

- **ADVANCE**: each running item whose linked run reached a terminal run state (`run.State.IsTerminal()` via the narrow `RunReader.GetRun`) is transitioned to the mapped terminal item-state (`succeeded`/`failed`/`cancelled`, guarded by `campaign.ValidCampaignItemTransition`) and emits `campaign_issue_settled`; the campaign state is then re-derived (`campaign.DeriveState`) and transitioned with a `campaign_advanced` emit when it changes.
- **START**: items partitioned `Eligible` by the E25.3 pure engine (`campaign.NextEligible`) are started up to a per-campaign concurrency budget (`MaxParallel` − currently-running, default `DefaultMaxParallel`=4) through the `RunStarter` seam, linked (`SetCampaignItemRun`), transitioned to `running`, and recorded with `campaign_issue_started`.

The three audit kinds ride the GLOBAL chain (`AppendGlobalChained` — a campaign is not a run; the run linkage is in the payload), best-effort (see `docs/issue-comment-surfaces.md`).

## RunStarter seam

The `RunStarter` is satisfied in `serve.go` by `campaignRunStarter`, a thin adapter over `server.Server.StartRunForCampaignIssue` (`runs.go`) — which resolves the GitHub App installation, fetches+parses the workflow spec at `--campaign-driver-workflow-ref` (empty = the repo's default branch; the fetched blob SHA becomes the run's `workflow_sha`) for `--campaign-driver-workflow-id` (default `feature_change`), then mints the run via the EXTRACTED `server.Server.CreateRunForTrigger` (the single run+stage-creation seam `handleCreateRun` also routes through).

A campaign carries no workflow context (E25.2), which is why the driver resolves it from the repo spec at start time.

## Off by default, fail-closed start

Enable with `--enable-campaign-driver` (`FISHHAWKD_ENABLE_CAMPAIGN_DRIVER=true`). The fail-closed switch (`campaignDriverStartDecision`, unit-tested) refuses to start — WARN-logs and constructs no ticker — when `CampaignRepo`/`RunRepo`/`AuditRepo` is unwired OR the GitHub client is absent (the run-starter needs it to resolve the spec); a flag-off start is a silent no-op.

Per-item/per-campaign errors WARN-log and never abort the tick (mirrors the `deployreconciler`/`childcompletion` posture).

## Auto-drive on each run gate (E25.6 / #1445, ADR-047 Track C)

Layered on the mechanical advancement, during the ADVANCE pass every running item whose linked run is NON-terminal is handed to the optional `GateActor` seam (`Ticker.driveGate`) BEFORE terminal observation.

The seam is satisfied in `serve.go` by `campaignGateActor`, a thin adapter over `server.Server.AutoDriveRunGate` binding the campaign operator identity (`campaignOperatorIdentity` — subject `operatorrole.CampaignActorSubject`, the `operatorrole.CampaignActorScopes` write scopes, a non-empty TokenID so the handler scope check is ENFORCED not bypassed) and a `githubAutoMerger` (`EnableAutoMerge`, squash).

`AutoDriveRunGate` re-evaluates the run's `operator_agent` delegation in-process (read-only) and, for a delegated knob whose condition is met AND whose real gate state independently matches (double-gating, fail-CLOSED to observe-only on any mismatch / evaluation error / unrecognised condition / unconfigured merge seam), takes the gate action — `may_approve`→approve, `may_route_fixup`→fixup, `may_retry`→retry, `may_merge`→merge.
Actions go via the EXTRACTED identity-parameterised service methods (`approveStageAs`/`fixupStageAs`/`retryStageAs`), recording the run-level audit stamped `audit.ActorAgent`. The driver then records a campaign-level `campaign_gate_acted` marker on the global chain.

A `must_page_human` condition (`reviewer_reject`, `requirement_arbitration`) is REFUSED: the actor emits the `campaign_gate_paged` hand-off (run chain) and takes NO action.

**Auto-drive is OPTIONAL and fail-closed**: `newCampaignGateActor` returns nil — the driver then runs OBSERVE-ONLY, leaving every gate parked for the human operator-agent — when the GitHub merge client is unconfigured (`campaignDriverStartDecision` already refuses to start the driver without it, so the nil path is defensive).

## Pause/page on the hand-off (E25.7 / #1446, ADR-047 Track C)

The driver ACTS on the `out.Paged` refusal in `Ticker.pageGate`:

- It pauses the affected item (`CampaignStore.PauseCampaignItem`, recording the `campaign.PauseReason` `{page_event, run_id}` as JSONB on the item) and, unless the campaign's `pause_policy` is `pause_item` (continue-others; the default `pause_campaign` blocks the whole campaign), transitions the campaign `running`→`paused`.
- It records a campaign-level `campaign_paused` marker (payload `{campaign_id, issue_ref, run_id, page_event, policy}`) on the global chain and fires the human page through the optional `Notifier` seam (`NotifyStatusUpdateForRun`, satisfied in `serve.go` by the existing `issuecomment` notifier — nil → observe-only, pause recorded but no page; the typed-nil interface trap is avoided by only assigning a non-nil notifier).
- A campaign already `paused` is sticky — `deriveAndTransition` skips re-derivation so a sibling settling can never auto-unpause it, and the START pass is skipped once a tick leaves the campaign non-running.
- Resuming is an explicit operator action (`POST /v0/campaigns/{id}/resume`, E25.7 Slice 3), never a derivation.
