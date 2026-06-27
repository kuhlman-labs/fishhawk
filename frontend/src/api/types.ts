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
