/*
 * TypeScript mirrors of the change-of-command schemas in
 * docs/api/v0.openapi.yaml (E76.6 / #3769): the captain record
 * (E76.2 / #3765), the handover brief (E76.4 / #3767), the per-workflow
 * delegation view (E76.1 / #3747) and delegation confirmation
 * (E76.5 / #3768).
 *
 * HAND-MIRRORED, field-for-field. The schema-sync CI does not cover the
 * frontend types, so drift is SILENT here — change both sides together.
 * Every interface names the OpenAPI schema it mirrors. A field absent on
 * the wire is optional here; a field the schema marks nullable is `| null`.
 */

// ---------------------------------------------------------------------------
// Captain record — GET /v0/captain and the five captain verbs.
// ---------------------------------------------------------------------------

/** Mirrors `CaptainRecord`. `claim_verified` is SEPARATE from `identity_verified`. */
export interface CaptainRecord {
  subject: string;
  /** True only for a provider-qualified subject (`github:` / `gitlab:`). */
  identity_verified: boolean;
  /** Null for an assigned (handed-over) seat; true/false only on a claimed seat. */
  claim_verified: boolean | null;
  basis: 'assigned' | 'claimed';
  assigned_sequence: number;
  assigned_entry_hash: string;
  assigned_at: string;
}

export type CaptainBriefUnavailableReason =
  | 'dependency_unconfigured'
  | 'captain_record_read_failed'
  | 'window_read_failed'
  | 'compose_failed';

/** Mirrors `CaptainOffer`. `identity_verified` is the SUCCESSOR's provider qualification. */
export interface CaptainOffer {
  successor: string;
  identity_verified: boolean;
  offered_by: string;
  offer_entry_hash: string;
  offered_sequence: number;
  offered_at: string;
  /** Omitted when `brief_unavailable` is true, and for a pre-E76.4 offer. */
  brief_hash?: string;
  brief_from_sequence?: number;
  brief_to_sequence?: number;
  brief_unavailable?: boolean;
  brief_unavailable_reason?: CaptainBriefUnavailableReason;
}

export type CaptainHistoryCategory =
  | 'captain_handover_offered'
  | 'captain_handover_withdrawn'
  | 'captain_assigned'
  | 'captain_relinquished'
  | 'captain_claimed';

/** Mirrors `CaptainHistoryItem`. */
export interface CaptainHistoryItem {
  sequence: number;
  entry_hash: string;
  category: CaptainHistoryCategory;
  at: string;
  payload: Record<string, unknown>;
}

/**
 * Mirrors `CaptainDelegationUnconfirmed`. `workflows` is EMPTY when
 * `inventory_unavailable` or `unavailable` is set — then it is NOT
 * "all confirmed".
 */
export interface CaptainDelegationUnconfirmed {
  seat_sequence: number;
  unconfirmed_since?: string;
  source: 'run_cache';
  workflow_sha?: string;
  workflows: string[];
  hash_staleness_reported: false;
  inventory_unavailable?:
    'no_run_repository' | 'list_runs_failed' | 'no_cached_spec' | 'spec_unparseable';
  unavailable?: 'chain_read_failed';
}

/** Mirrors `CaptainResponse` — the GET /v0/captain body. */
export interface CaptainResponse {
  repo: string;
  /** Null when the seat is vacant. */
  captain: CaptainRecord | null;
  pending_offer: CaptainOffer | null;
  last_captain: string | null;
  history: CaptainHistoryItem[];
  history_total: number;
  skipped_entries: number;
  /** Present only when the delegation-confirmation store is wired. */
  delegation_unconfirmed?: CaptainDelegationUnconfirmed;
}

/**
 * Mirrors `CaptainVerbRequest`. `successor` is accepted by `offer` ONLY
 * (400 `validation_failed` on every other verb), and `delegated` is never
 * sent by the SPA — every captain verb refuses the delegated path.
 */
export interface CaptainVerbRequest {
  repo: string;
  successor?: string;
}

/** Mirrors `CaptainVerbResponse` — every captain verb's 200 body. */
export interface CaptainVerbResponse {
  repo: string;
  event: CaptainHistoryItem;
  captain: CaptainRecord | null;
  pending_offer: CaptainOffer | null;
  delegation_unconfirmed?: CaptainDelegationUnconfirmed;
}

// ---------------------------------------------------------------------------
// Handover brief — GET /v0/handover-brief.
// ---------------------------------------------------------------------------

/**
 * Mirrors `DigestItem` — one cited chain entry. types.ts carries no digest
 * mirror yet, so the brief's embedded shape is declared here.
 */
export interface DigestItem {
  category: string;
  source_sequence: number;
  source_entry_hash: string;
  run_id: string;
  stage_id?: string;
  stage_kind?: string;
  stage_state?: string;
  at: string;
  outcome?: string;
  headline?: string;
  concern_category?: string;
  severity?: string;
  reason_sequence?: number;
  reason_key?: string;
  amendment_id?: string;
  answered?: boolean;
  answered_sequence?: number;
  source_missing?: boolean;
  fields_truncated?: boolean;
}

/** Mirrors `DigestGap`. */
export interface DigestGap {
  kind:
    | 'unindexed_decision'
    | 'source_entry_missing'
    | 'source_hash_mismatch'
    | 'parked_without_citation';
  sequence: number;
  category?: string;
  run_id?: string;
  stage_id?: string;
  detail?: string;
}

export type HandoverBriefSectionKind =
  'what_changed' | 'needs_decision' | 'in_flight' | 'delegation_in_force' | 'standing_orders';

export type HandoverBriefPartKind =
  | 'merges'
  | 'waivers_and_deferrals'
  | 'unanswered_pages'
  | 'open_decisions'
  | 'campaigns'
  | 'runs'
  | 'workflows';

/** Mirrors `HandoverBriefCursor` — the exact underlying query for an elided remainder. */
export interface HandoverBriefCursor {
  part: string;
  call: string;
  from_sequence?: number;
  to_sequence?: number;
  offset?: number;
}

/** Mirrors `HandoverBriefWindow`. */
export interface HandoverBriefWindow {
  from_sequence: number;
  to_sequence: number;
  chain_head: number;
  basis: 'since_last_handover' | 'first_captain' | 'requested';
  assigned_sequence?: number;
  assigned_entry_hash?: string;
}

/** Mirrors `HandoverBriefPart.in_flight[]`. */
export interface HandoverBriefInFlight {
  kind: 'campaign' | 'run';
  id: string;
  state: string;
  ref?: string;
  workflow_id?: string;
  created_at: string;
  fields_truncated?: boolean;
}

/** Mirrors `HandoverBriefPart.workflows[]`. `confirmation` is the constant `unavailable`. */
export interface HandoverBriefWorkflow {
  workflow_id: string;
  autonomy?: string;
  must_page_human?: string[];
  escalations?: Array<{ match?: Record<string, unknown>; max_autonomy?: string }>;
  content_hash: string;
  confirmation: 'unavailable';
  fields_truncated?: boolean;
}

/** Mirrors `HandoverBriefPart`. */
export interface HandoverBriefPart {
  kind: HandoverBriefPartKind;
  items?: DigestItem[];
  in_flight?: HandoverBriefInFlight[];
  workflows?: HandoverBriefWorkflow[];
  unavailable?: boolean;
  unavailable_reason?: string;
  complete: boolean;
  truncated: boolean;
  omitted_count: number;
  next?: HandoverBriefCursor;
}

/** Mirrors `HandoverBriefSection.standing_orders`. */
export interface HandoverBriefStandingOrders {
  source: string;
  ref?: string;
  workflow_sha?: string;
  spec_version?: string;
  schema_major: number;
  delegation_content_hash: string;
}

/** Mirrors `HandoverBriefSection`. */
export interface HandoverBriefSection {
  kind: HandoverBriefSectionKind;
  parts: HandoverBriefPart[];
  standing_orders?: HandoverBriefStandingOrders;
  /** True when every part is unavailable. */
  unavailable?: boolean;
}

/** Mirrors `HandoverBrief` — the GET /v0/handover-brief body. */
export interface HandoverBrief {
  repo: string;
  captain_subject?: string;
  successor?: string;
  window: HandoverBriefWindow;
  section?: string;
  sections: HandoverBriefSection[];
  absent: Array<{ section: string; reason: string; anchor: string }>;
  gaps: DigestGap[];
  degradations: Array<{
    kind: string;
    section?: string;
    part?: string;
    sequence?: number;
    detail?: string;
  }>;
  gaps_truncated: boolean;
  gaps_omitted_count: number;
  gaps_next?: HandoverBriefCursor;
  uncited_next?: HandoverBriefCursor;
  brief_hash: string;
  truncated: boolean;
  next?: HandoverBriefCursor;
}

// ---------------------------------------------------------------------------
// Delegation view + confirmation — GET/POST /v0/repos/{owner}/{name}/delegation*.
// ---------------------------------------------------------------------------

export type AutonomyTier = 'low' | 'medium' | 'high';
export type DelegationSource = 'ref' | 'run_cache';

/** Mirrors `RepoDelegationAction` — one resolved action class with its provenance. */
export interface RepoDelegationAction {
  action: string;
  mode: 'gated' | 'auto' | 'report';
  condition?: string;
  min_severity?: string;
  source: 'tier' | 'explicit' | 'default' | 'escalation';
}

/** Mirrors `RepoDelegationModelPolicy`. */
export interface RepoDelegationModelPolicy {
  strategy?: 'follow_plan_recommendation' | 'explicit_defaults';
  defaults?: { plan?: string; implement?: string; review?: string };
  allowed?: string[];
}

/** Mirrors `RepoDelegationEscalation`. DECLARATIVE — not evaluated by the read. */
export interface RepoDelegationEscalation {
  match: {
    paths?: string[];
    labels?: string[];
    change_kind?: string[];
    trigger?: string[];
  };
  max_autonomy?: AutonomyTier;
  approvals?: {
    count?: number;
    member_of?: string;
    min_permission?: 'read' | 'triage' | 'write' | 'maintain' | 'admin';
  };
  ceiling_matrix?: RepoDelegationAction[];
}

/** Mirrors `DelegationConfirmationRecord` — one COUNTED `delegation_confirmed` entry. */
export interface DelegationConfirmationRecord {
  subject: string;
  content_hash: string;
  workflow_sha?: string;
  sequence: number;
  entry_hash: string;
  at: string;
}

/** Mirrors `DelegationEscalationProposal`. Carries no `approvals`, so nothing in it can raise. */
export interface DelegationEscalationProposal {
  paths: string[];
  max_autonomy: AutonomyTier;
}

/** Mirrors `DelegationLowerProposal` — one COUNTED `delegation_lower_proposed` entry. */
export interface DelegationLowerProposal {
  subject: string;
  proposed_tier?: string;
  proposed_escalation?: DelegationEscalationProposal;
  reason: string;
  filed_ref: string;
  sequence: number;
  entry_hash: string;
  at: string;
}

/** Mirrors `DelegationWorkflowStatus` — one workflow's confirmation verdict. */
export interface DelegationWorkflowStatus {
  workflow: string;
  status: 'confirmed' | 'unconfirmed' | 'no_captain';
  /** `unconfirmed` only. */
  reason?: 'handover' | 'hash_stale';
  /** The value a confirm must bind to. */
  current_content_hash?: string;
  confirmation?: DelegationConfirmationRecord;
  lower_proposal?: DelegationLowerProposal;
}

/** Mirrors `RepoDelegationWorkflow`. */
export interface RepoDelegationWorkflow {
  id: string;
  /** Absent when the workflow declares only `actions`, or no autonomy block. */
  autonomy?: AutonomyTier;
  /** The WORKFLOW-level matrix; a gate's own autonomy block overrides it at that gate. */
  matrix: RepoDelegationAction[];
  must_page_human?: string[];
  model_policy?: RepoDelegationModelPolicy;
  escalations?: RepoDelegationEscalation[];
  /** Bind a single-workflow confirmation to THIS hash, never the view-level one. */
  content_hash: string;
  /** ABSENT — never a false `confirmed` — when the store is unwired or its read failed. */
  confirmation?: DelegationWorkflowStatus;
}

/** Mirrors `DelegationConfirmationSummary` — absent when the store is not wired. */
export interface DelegationConfirmationSummary {
  captain: string | null;
  seat_sequence: number;
  unconfirmed_workflows: string[];
  /** Set when the chain could not be read — an empty list is then NOT "all confirmed". */
  unavailable?: string;
}

/** Mirrors `RepoDelegation` — the GET /v0/repos/{owner}/{name}/delegation body. */
export interface RepoDelegation {
  repo: string;
  source: DelegationSource;
  ref?: string;
  workflow_sha?: string;
  spec_version?: string;
  schema_major: number;
  /** Recomputed over the RETAINED set under a `workflow` filter. */
  content_hash: string;
  workflows: RepoDelegationWorkflow[];
  confirmation?: DelegationConfirmationSummary;
}

/** Mirrors `DelegationConfirmationResponse`. */
export interface DelegationConfirmationResponse {
  repo: string;
  source: DelegationSource;
  ref?: string;
  workflow_sha?: string;
  captain: string | null;
  seat_sequence: number;
  workflows: DelegationWorkflowStatus[];
  unconfirmed_workflows: string[];
  skipped_entries: number;
  ignored_entries: number;
}

/** Mirrors `DelegationConfirmRequest`. `delegated` is deliberately not modelled: the SPA never sends it. */
export interface DelegationConfirmRequest {
  workflow: string;
  /** The workflow's OWN `content_hash` exactly as shown. */
  content_hash: string;
  source?: DelegationSource;
  ref?: string;
}

/** Mirrors `DelegationLowerRequest`. At least one of `proposed_tier` / `proposed_escalation`. */
export interface DelegationLowerRequest {
  workflow: string;
  proposed_tier?: AutonomyTier;
  proposed_escalation?: DelegationEscalationProposal;
  reason: string;
  source?: DelegationSource;
  ref?: string;
  parent_epic?: string;
  title_vars?: Record<string, string>;
  labels?: string[];
}

/** Mirrors `DelegationEventItem`. */
export interface DelegationEventItem {
  sequence: number;
  entry_hash: string;
  category: 'delegation_confirmed' | 'delegation_lower_proposed';
  at: string;
  payload: Record<string, unknown>;
}

/** Mirrors `DelegationVerbResponse` — the 200 body of confirm and lower. */
export interface DelegationVerbResponse {
  repo: string;
  workflow: DelegationWorkflowStatus;
  event: DelegationEventItem;
  /** `lower` only: the ONE filed `autonomy:low` work item. */
  filed?: {
    number: number;
    url: string;
    title: string;
    applied_labels?: string[];
  };
}
