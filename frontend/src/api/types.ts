/*
 * TypeScript mirrors of the OpenAPI schemas in docs/api/v0.openapi.yaml.
 * No runtime validation: the backend validates on ingest, and the only
 * way bad shapes reach the SPA is through bugs that we'd want to know
 * about loudly anyway. Add or update here whenever the OpenAPI surface
 * changes.
 */

export type RunState = 'pending' | 'running' | 'succeeded' | 'failed' | 'cancelled';
export type TriggerSource = 'github_issue' | 'cli' | 'ui';

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
  | 'cancelled';

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
export type CampaignState = 'pending' | 'running' | 'paused' | 'succeeded' | 'failed' | 'cancelled';

export type CampaignItemState =
  | 'pending'
  | 'blocked'
  | 'running'
  | 'paused'
  | 'succeeded'
  | 'failed'
  | 'cancelled';

export type PausePolicy = 'pause_campaign' | 'pause_item';

export type CampaignNextActionType = 'attention' | 'resume' | 'start_run' | 'wait' | 'complete';

export interface Campaign {
  id: string;
  repo: string;
  /** The epic the campaign decomposes, in `issue:N` form. */
  epic_ref: string;
  state: CampaignState;
  pause_policy: PausePolicy;
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

export type ApprovalDecision = 'approve' | 'reject';

export interface ApprovalRequest {
  decision: ApprovalDecision;
  comment?: string;
}

export interface ApiError {
  error: string;
  message?: string;
  details?: Record<string, unknown>;
}
