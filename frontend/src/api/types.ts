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

export interface ApiError {
  error: string;
  message?: string;
  details?: Record<string, unknown>;
}
