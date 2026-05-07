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
  created_at: string;
  updated_at: string;
}

export type StageState =
  | 'pending'
  | 'dispatched'
  | 'running'
  | 'awaiting_approval'
  | 'succeeded'
  | 'failed'
  | 'cancelled';

export type StageType = 'plan' | 'implement' | 'review';
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
 * carry approvers; check gates don't. blocking_checks lists the
 * named checks the gate depends on; v0 doesn't yet ingest their
 * states (a follow-up surface), so the review UI renders the names
 * with a "not tracked yet" placeholder.
 */
export type StageGateType = 'approval' | 'check';

export interface StageGateApprovers {
  any_of?: string[];
  all_of?: string[];
}

export interface StageGate {
  type: StageGateType;
  blocking_checks?: string[];
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
   * reads this to render blocking_checks + the approval panel.
   */
  gate?: StageGate;
  created_at: string;
  updated_at: string;
}

export type ArtifactKind = 'plan' | 'pull_request';

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
