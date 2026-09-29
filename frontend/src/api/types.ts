/*
 * TypeScript mirrors of the OpenAPI schemas in docs/api/v0.openapi.yaml.
 * No runtime validation: the backend validates on ingest, and the only
 * way bad shapes reach the SPA is through bugs that we'd want to know
 * about loudly anyway. Add or update here whenever the OpenAPI surface
 * changes.
 */

export type RunState = 'pending' | 'running' | 'succeeded' | 'failed' | 'cancelled';
// 'on_demand' is the operator-initiated NON-DIFF trigger form (E54.22 / #2826):
// the producer for a workflow declaring
// `applies_to: {trigger: [scheduled, on_demand]}`. Display sites render
// `run.trigger_source` verbatim, so widening the union is the only change
// needed; a future switch over this type will fail typecheck until it handles
// the new member, which is the desired direction.
export type TriggerSource = 'github_issue' | 'cli' | 'ui' | 'on_demand';

export interface Run {
  id: string;
  repo: string;
  workflow_id: string;
  workflow_sha: string;
  trigger_source: TriggerSource;
  trigger_ref: string | null;
  state: RunState;
  /**
   * Set when the dispatcher saw a non-terminal run on the same
   * (repo, trigger_ref) at create time and threaded this run as
   * its follow-up (#216).
   */
  parent_run_id?: string | null;
  /**
   * Set when the implement stage produced a pull_request artifact
   * (#216). The threaded-runs view groups by this column to render
   * "every run on this PR."
   */
  pull_request_url?: string | null;
  /**
   * Position in the CI-failure auto-retry chain (#279 / E16). 0 for
   * the canonical first attempt; N for the Nth retry. Compared
   * against max_retries_snapshot to decide whether more retries
   * are available.
   */
  retry_attempt: number;
  /**
   * Workflow's on_ci_failure.max_retries cap snapshotted at
   * run-create time (#280 / E16). Defaults to 1 when the spec
   * has no on_ci_failure block. Renders alongside retry_attempt
   * as "Retry N/M" on the run-detail header.
   */
  max_retries_snapshot: number;
  created_at: string;
  updated_at: string;
}

export type StageState =
  | 'pending'
  | 'dispatched'
  | 'running'
  | 'awaiting_approval'
  // Deploy-stage parked states, mirroring the Go constants in
  // backend/internal/run/run.go (StageStateAwaitingDeployApproval /
  // StageStateAwaitingDeployment). awaiting_deploy_approval is the
  // pre-execution gate (operator action pending); awaiting_deployment
  // is the in-flight post-approval state while the delegated external
  // pipeline runs. The OpenAPI Stage.state enum may not yet enumerate
  // these even though the backend emits them — the mirror tracks the
  // wire values the SPA actually renders.
  | 'awaiting_deploy_approval'
  | 'awaiting_deployment'
  | 'succeeded'
  | 'failed'
  // Merge-supersede terminal state, mirroring the Go constant
  // backend/internal/run.StageStateSuperseded (#3083): the merge of the
  // change this stage was gating made it UNREACHABLE. Distinct from
  // 'failed' (attempted, did not pass) and 'cancelled' (an operator halted
  // the run). Terminal, so the SPA renders it as a settled stage.
  | 'cancelled'
  | 'superseded';

export type StageType = 'plan' | 'implement' | 'review' | 'deploy';
export type ExecutorKind = 'agent' | 'human';
export type FailureCategory = 'A' | 'B' | 'C' | 'D';

/**
 * Mirrors backend/internal/run.FailureCategory.Description(). Keep
 * the two in sync — the audit log and the UI must agree on wording.
 * Update both sides together; there is no schema-sync CI for the
 * Go-vs-TS string here, so drift is silent.
 */
export const FAILURE_DESCRIPTIONS: Record<FailureCategory, string> = {
  A: 'agent failure',
  B: 'constraint or policy violation',
  C: 'infrastructure failure',
  D: 'approval timeout or rejection',
};

export function describeFailure(cat: FailureCategory | null | undefined): string | null {
  if (!cat) return null;
  return FAILURE_DESCRIPTIONS[cat] ?? cat;
}

/**
 * The persisted shape of a stage's workflow-spec gate (#213). Mirrors
 * the StageGate schema in docs/api/v0.openapi.yaml. Approval gates
 * carry approvers; check gates don't. The pre-#254 `blocking_checks`
 * array was dropped in v0.2 (ADR-017 / #249) — required CI checks
 * now live in branch protection and surface via
 * GET /v0/stages/{id}/checks.
 */
export type StageGateType = 'approval' | 'check';

export interface StageGateApprovers {
  any_of?: string[];
  all_of?: string[];
}

export interface StageGate {
  type: StageGateType;
  approvers?: StageGateApprovers | null;
}

export interface Stage {
  id: string;
  run_id: string;
  sequence: number;
  type: StageType;
  executor: { kind: ExecutorKind; ref: string };
  state: StageState;
  started_at: string | null;
  ended_at: string | null;
  failure_category: FailureCategory | null;
  failure_reason: string | null;
  /**
   * Persisted workflow-spec gate. Omitted when the stage has no gate
   * (e.g. implement, or pre-#213 rows). The review-stage detail page
   * reads this to render the approval panel; live check state comes
   * from GET /v0/stages/{id}/checks.
   */
  gate?: StageGate;
  /**
   * The model the gate resolved for this stage's agent spawn, read
   * from the per-stage `model_resolved` audit (#1416). The wire always
   * carries it; it is empty string when no resolution was recorded for
   * the stage (the adapter-default spawn, and every stage of a run
   * approved before per-stage model selection landed). Optional here
   * only so existing Stage fixtures need not enumerate it — the UI
   * treats absent and empty identically (renders nothing).
   */
  resolved_model?: string;
  created_at: string;
  updated_at: string;
}

export type ArtifactKind = 'plan' | 'pull_request' | 'deployment';

export interface Artifact<C = unknown> {
  id: string;
  stage_id: string;
  kind: ArtifactKind;
  schema_version: string | null;
  content_hash: string;
  content?: C;
  created_at: string;
}

export interface PaginatedList<T> {
  items: T[];
  next_cursor: string | null;
}

export type AuditActorKind = 'agent' | 'user' | 'system';

export interface AuditEntry {
  id: string;
  sequence: number;
  run_id: string;
  stage_id: string | null;
  ts: string;
  category: string;
  actor_kind: AuditActorKind | null;
  actor_subject: string | null;
  payload: unknown;
  prev_hash: string | null;
  entry_hash: string;
}

/*
 * Campaign surface (ADR-047 / #1437). Mirrors the Campaign / CampaignItem /
 * CampaignRollup / CampaignNextAction / CampaignStatus schemas in
 * docs/api/v0.openapi.yaml, which in turn match the JSON tags on
 * campaignResponse / campaignItemResponse / campaignRollupPayload /
 * campaignNextActionPayload in backend/internal/server/campaigns.go and
 * campaign.PauseReason in backend/internal/campaign/campaign.go verbatim.
 */
/**
 * Campaign lifecycle state.
 *
 * `awaiting_human` (E72.33 / #3660) is the DERIVED, NON-terminal state of a
 * campaign whose only remaining open items are human-led (`autonomy:low`): the
 * engine has no dispatchable work, but the campaign is neither finished nor
 * wedged, and it returns to `pending`/`running` once such an issue is relabelled
 * to a driveable tier.
 */
export type CampaignState =
  'pending' | 'running' | 'paused' | 'awaiting_human' | 'succeeded' | 'failed' | 'cancelled';

export type CampaignItemState =
  'pending' | 'blocked' | 'running' | 'paused' | 'succeeded' | 'failed' | 'cancelled';

export type PausePolicy = 'pause_campaign' | 'pause_item';

export type CampaignNextActionType = 'attention' | 'resume' | 'start_run' | 'wait' | 'complete';

/**
 * The campaign-level `operator_agent` delegation override (E25.12 / #1451).
 * When present it is the effective delegation contract for EVERY issue-run the
 * campaign drives — it wins WHOLESALE over each run's per-workflow
 * `operator_agent` (campaign > gate > workflow, never merged). Mirrors
 * spec.OperatorAgent / the OpenAPI Campaign.operator_agent object: each `may_*`
 * knob is the single condition under which the action is delegated;
 * `must_page_human` lists the events that always page; `model_policy` is the
 * scenario-A model-selection block, typed loosely to avoid coupling to the full
 * ModelPolicy schema. The OpenAPI shape is additionalProperties:true, so the
 * known knobs need not be exhaustive.
 */
export interface OperatorAgentOverride {
  may_approve?: string;
  may_route_fixup?: string;
  may_waive?: string;
  may_retry?: string;
  may_merge?: string;
  must_page_human?: string[];
  model_policy?: Record<string, unknown>;
}

export interface Campaign {
  id: string;
  repo: string;
  /** The epic the campaign decomposes, in `issue:N` form. */
  epic_ref: string;
  state: CampaignState;
  pause_policy: PausePolicy;
  /**
   * The campaign-level operator_agent delegation override (E25.12 / #1451).
   * Omitted when the campaign carries no override (each issue-run inherits its
   * per-workflow contract).
   */
  operator_agent?: OperatorAgentOverride;
  created_at: string;
  updated_at: string;
}

/**
 * Why a paused item was handed off to a human (E25.7). Present only while
 * the item is — or was — paused; every field is optional (omitempty on the
 * Go side).
 */
export interface PauseReason {
  /** The audit category that triggered the page (e.g. `campaign_gate_paged`). */
  page_event?: string;
  /** The run whose gate was handed off. */
  run_id?: string | null;
  /** The gate's stage, if any. */
  stage_id?: string | null;
  /** The gate/decision the human must own. */
  gate?: string;
}

export interface CampaignItem {
  id: string;
  issue_ref: string;
  /** Sibling issue refs this item depends on — the DAG edges (possibly empty). */
  depends_on: string[];
  /** The run executing this item. Omitted while the item is unlinked. */
  run_id?: string | null;
  state: CampaignItemState;
  /** Present only while the item is — or was — paused. */
  pause_reason?: PauseReason | null;
  created_at: string;
  updated_at: string;
}

/**
 * The engine's readiness partition over a campaign's items. Every slice
 * holds issue refs; an item appears in exactly one slice. Each field is
 * always an array (never null).
 */
export interface CampaignRollup {
  eligible: string[];
  blocked: string[];
  running: string[];
  done: string[];
  failed: string[];
  cancelled: string[];
  paused: string[];
}

export interface CampaignNextAction {
  action: CampaignNextActionType;
  /** The item the action refers to. Omitted for `wait` / `complete`. */
  issue_ref?: string;
  detail?: string;
}

/**
 * GET /v0/campaigns/{id}/status — the campaign + its items + the engine's
 * readiness rollup + the distilled next action, in one payload.
 */
export interface CampaignStatus {
  campaign: Campaign;
  items: CampaignItem[];
  rollup: CampaignRollup;
  next_action: CampaignNextAction;
}

/*
 * Attention queue (E40.1 / #1713). Mirrors the AttentionList /
 * AttentionItem / AttentionContext / AttentionDegraded schemas in
 * docs/api/v0.openapi.yaml (GET /v0/attention) field-for-field.
 */

/** The closed six-member item-kind set, in priority order (1 = most urgent). */
export type AttentionItemKind =
  | 'plan_gate'
  | 'scope_amendment'
  | 'acceptance_disposition'
  | 'split_verdict'
  | 'paged_concern'
  | 'attend_human_led_campaign';

export type AttentionDegradedReason =
  | 'concern_store_unconfigured'
  | 'scope_amendment_store_unconfigured'
  | 'campaign_store_unconfigured'
  | 'stage_read_failed'
  | 'acceptance_state_unreadable'
  | 'scope_amendment_read_failed'
  | 'concern_read_failed'
  | 'gate_view_history_incomplete'
  | 'gate_view_budget_exhausted'
  | 'plan_summary_unavailable'
  | 'plan_reviews_unreadable'
  | 'campaign_read_failed'
  | 'campaign_scan_truncated'
  | 'campaign_items_unreadable';

export interface AttentionReviewVerdict {
  reviewer_model?: string;
  verdict: string;
  concern_count: number;
}

export interface AttentionRequestedPath {
  path: string;
  operation: 'modify' | 'create';
}

/**
 * One failed acceptance criterion: its id and the request whose response the
 * failing assertion evaluated. The request fields are absent when the failed
 * criterion recorded no requests.
 */
export interface AttentionFailedCriterion {
  id: string;
  method?: string;
  path?: string;
  status?: number;
}

/**
 * One-screen decision context. Which fields are present depends on the
 * item kind — see the AttentionContext schema description.
 */
export interface AttentionContext {
  // plan_gate
  plan_summary?: string;
  review_verdicts?: AttentionReviewVerdict[];
  // scope_amendment
  reason?: string;
  requested_paths?: AttentionRequestedPath[];
  // acceptance_disposition
  verdict?: string;
  criteria_failed?: number;
  criteria_skipped?: number;
  failed_criteria?: AttentionFailedCriterion[];
  // split_verdict / paged_concern
  stage_kind?: string;
  severity?: string;
  category?: string;
  reviewer_model?: string;
  note?: string;
  new_evidence?: string;
  dispute_reasons?: string[];
  confirmation_note?: string;
  // attend_human_led_campaign
  epic_ref?: string;
  human_led_refs?: string[];
  detail?: string;
}

export interface AttentionItem {
  /** Stable identity of the decision's subject (stage, amendment, concern or campaign id). */
  id: string;
  kind: AttentionItemKind;
  priority: number;
  run_id?: string;
  campaign_id?: string;
  stage_id?: string;
  concern_id?: string;
  amendment_id?: string;
  repo: string;
  title: string;
  context: AttentionContext;
  /** SPA-relative link target: /runs/{id}, /runs/{id}/stages/{id} or /campaigns/{id}. */
  detail_path: string;
  since: string;
}

export interface AttentionDegraded {
  reason: AttentionDegradedReason;
  run_id?: string;
  campaign_id?: string;
  detail?: string;
}

/**
 * GET /v0/attention body. `truncated: true` or a non-empty `degraded`
 * means the list is INCOMPLETE — even when `items` is empty.
 */
export interface AttentionList {
  items: AttentionItem[];
  degraded: AttentionDegraded[];
  truncated: boolean;
  scanned_runs: number;
}

/*
 * Repo dashboard rollups (E40.3 / #1714). Mirrors the RepoThroughput /
 * RepoHealth / RepoEconomics / RepoPosture schemas in
 * docs/api/v0.openapi.yaml, served by GET /v0/repos/{owner}/{name}/{...}
 * (backend/internal/server/repodash.go). The shared wire goldens
 * testdata/wire/repodash_*.json pin these shapes on both sides.
 */

/**
 * The window envelope every rollup carries. `truncated: true` means the
 * server's scan ceiling stopped the run scan before the look-back boundary
 * — the rollup is PARTIAL, and each panel says so.
 */
export interface RepoDashWindow {
  repo: string;
  window_start: string;
  window_end: string;
  window_weeks: number;
  runs_scanned: number;
  truncated: boolean;
}

/** One ISO week (UTC, Monday start) of merged changes, bucketed by merge time. */
export interface RepoWeekCount {
  week_start: string;
  merged_changes: number;
}

export interface RepoGateWaitRollup {
  gate: string;
  samples: number;
  median_wait_seconds: number;
}

/** Wait-on-human latency (#1702), folded over the runs that resolved a rollup. */
export interface RepoWaitOnHuman {
  runs: number;
  median_total_wait_seconds: number;
  total_wait_on_human_seconds: number;
  gates: RepoGateWaitRollup[];
}

/** GET /v0/repos/{owner}/{name}/throughput body. */
export interface RepoThroughput extends RepoDashWindow {
  weeks: RepoWeekCount[];
  merged_changes: number;
  /** created_at → earliest pr_merged, median over `cycle_time_samples` runs. */
  median_cycle_time_seconds: number;
  cycle_time_samples: number;
  /** Merged runs with no pr_merged row, left out of the median. */
  cycle_time_excluded: number;
  /** ABSENT (not a zero block) when no run in the window resolved a gate-latency rollup. */
  wait_on_human?: RepoWaitOnHuman;
}

export interface RepoFailureMix {
  A: number;
  B: number;
  C: number;
  D: number;
}

/** GET /v0/repos/{owner}/{name}/health body. Every rate is 0 on a zero denominator. */
export interface RepoHealth extends RepoDashWindow {
  runs_considered: number;
  plan_approval_samples: number;
  plan_first_shot_approvals: number;
  plan_first_shot_approval_rate: number;
  fixup_runs: number;
  fixup_rate: number;
  acceptance_samples: number;
  acceptance_passed: number;
  acceptance_not_validated: number;
  acceptance_failed: number;
  acceptance_undecidable: number;
  acceptance_pass_rate: number;
  failure_categories: RepoFailureMix;
}

export interface RepoWeekEconomics {
  week_start: string;
  cost_usd: number;
  cache_read_ratio: number;
  reuse_factor: number;
  net_savings_usd: number;
}

/** budget.Tier* in backend/internal/budget/budget.go. */
export type BudgetTier = 'ok' | 'warn' | 'over' | 'ack_required' | 'page';

/** One workflow's ADR-030 periodic-budget burn. */
export interface RepoBudgetBurn {
  workflow_id: string;
  period: 'weekly' | 'monthly';
  period_start: string;
  limit_usd: number;
  spent_usd: number;
  fraction: number;
  tier: BudgetTier;
  enforcement: 'advisory' | 'blocking';
}

export interface RepoEconomics extends RepoDashWindow {
  cost_entries: number;
  total_cost_usd: number;
  merged_changes: number;
  cost_per_merged_change_usd: number;
  weeks: RepoWeekEconomics[];
  /** Omitted when no workflow in the window declares a periodic budget. */
  budgets?: RepoBudgetBurn[];
}

/**
 * The no-cost_recorded economics body: `{}`, or `{"truncated":true}` when the
 * scan was cut short (presence-not-status-code, like /cost).
 */
export interface RepoEconomicsEmpty {
  truncated?: boolean;
}

/** GET /v0/repos/{owner}/{name}/economics body. */
export type RepoEconomicsResponse = RepoEconomics | RepoEconomicsEmpty;

export interface PostureGate {
  type: string;
  autonomy?: string;
  approvers_any_of?: string[];
  approvers_all_of?: string[];
}

export interface PostureReviewers {
  agents?: Array<{ provider: string; model?: string }>;
  human?: number;
}

export interface PostureStageBudget {
  max_tokens?: number;
  max_runtime_seconds?: number;
  limit_usd?: number;
}

export interface PostureStage {
  id: string;
  type: string;
  executor: string;
  model?: string;
  gates?: PostureGate[];
  reviewers?: PostureReviewers;
  budget?: PostureStageBudget;
}

export interface PosturePeriodicBudget {
  period: 'weekly' | 'monthly';
  limit_usd: number;
  enforcement?: 'advisory' | 'blocking';
  warn_at?: number;
}

export interface PostureWorkflow {
  id: string;
  autonomy?: string;
  stages: PostureStage[];
  budgets?: PosturePeriodicBudget[];
}

/**
 * Workflow posture projected from the repo's newest run's cached spec.
 * `schema_supported` / `spec_valid` + `spec_error` drive the drift warning.
 */
export interface RepoPosture {
  repo: string;
  run_id: string;
  workflow_id: string;
  workflow_sha: string;
  version: string;
  schema_major: number;
  schema_hash?: string;
  schema_supported: boolean;
  spec_valid: boolean;
  spec_error?: string;
  workflows: PostureWorkflow[];
}

/** GET /v0/repos/{owner}/{name}/posture body: `{}` when no run carries a cached spec. */
export type RepoPostureResponse = RepoPosture | Record<string, never>;

/** GET /healthz body (backend/internal/server/handlers.go healthResponse). */
export interface HealthStatus {
  status: string;
  version: string;
  git_sha: string;
  min_runner_version: string;
  /** Embedded schema hashes keyed by id, e.g. `workflow-v2`. */
  schemas: Record<string, string>;
  start_nonce?: string;
  process_start?: string;
  dev_mode?: boolean;
  push_sinks: string[];
}

export type ApprovalDecision = 'approve' | 'reject';

export interface ApprovalRequest {
  decision: ApprovalDecision;
  comment?: string;
}

/*
 * Human-decision write surfaces wired into the attention queue (E40.2 /
 * #1717). Each mirrors an existing OpenAPI request/response shape in
 * docs/api/v0.openapi.yaml — no new endpoint, no schema change. Field
 * names are copied field-for-field from the OpenAPI document.
 */

export type ScopeAmendmentDecision = 'approve' | 'deny';
export type ScopeAmendmentStatus = 'pending' | 'approved' | 'denied';

/** One requested path in a mid-stage scope amendment (#961). */
export interface ScopeAmendmentPath {
  path: string;
  operation: 'modify' | 'create';
}

/** Mirrors the ScopeAmendment schema — the decided-amendment response body. */
export interface ScopeAmendment {
  id: string;
  run_id: string;
  stage_id: string;
  paths: ScopeAmendmentPath[];
  reason: string;
  status: ScopeAmendmentStatus;
  decision_reason?: string;
  decided_by?: string;
  requested_at: string;
  decided_at?: string;
}

/** Body of POST /v0/runs/{run}/scope-amendments/{id}/decision. */
export interface ScopeAmendmentDecisionRequest {
  decision: ScopeAmendmentDecision;
  reason?: string;
}

/**
 * The updated concern row returned by the waive endpoint (#984). Deferred
 * concerns carry the identical row shape nested under
 * DeferredConcernResult.concern, so the alias below names the shared shape.
 */
export interface ConcernRow {
  id: string;
  run_id: string;
  stage_id: string;
  stage_kind: 'plan' | 'implement';
  severity: string;
  category: string;
  note: string;
  state: string;
  state_reason: string;
}
export type WaivedConcern = ConcernRow;

/** Body of POST /v0/concerns/{id}/waive — the REQUIRED audited reason. */
export interface ConcernWaiveRequest {
  reason: string;
}

/**
 * Body of POST /v0/concerns/{id}/defer. `parent_epic` is the only
 * operator-supplied field the server cannot derive; the rest override
 * server defaults.
 */
export interface ConcernDeferRequest {
  parent_epic?: string;
  n?: string;
  type?: string;
  labels?: string[];
  note?: string;
}

/** Mirrors DeferredConcernResult — the filed follow-up issue + deferred concern. */
export interface DeferredConcernResult {
  concern: ConcernRow;
  issue: {
    type: string;
    title: string;
    number: number;
    url: string;
    provider: string;
    applied_labels?: string[];
    defaulted_labels?: string[];
    missing_label_namespaces?: string[];
  };
}

/**
 * Body of POST /v0/runs/{run}/acceptance-arbitration. `reason` is required;
 * `acknowledge_failed_criteria` is required by the server (409
 * `acceptance_arbitration_requires_acknowledgement`) whenever the discharged
 * outcome carries `criteria_failed > 0`.
 */
export interface AcceptanceArbitrationRequest {
  reason: string;
  acknowledge_failed_criteria?: boolean;
}

/** The 200 body of the acceptance-arbitration endpoint. */
export interface AcceptanceArbitrationResult {
  run_id: string;
  acceptance_gate_state: string;
  outcome_sequence: number;
  arbitration_sequence: number;
  already_recorded: boolean;
}

export interface ApiError {
  error: string;
  message?: string;
  details?: Record<string, unknown>;
}
