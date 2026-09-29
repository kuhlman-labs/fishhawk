import type { AcceptanceArtifactBody, AcceptanceCriterionOutcome } from '@/api/acceptance';
import { isStandardV1Plan, type ScopeFile } from '@/api/plan';
import type {
  Artifact,
  AuditEntry,
  GateView,
  GateViewFixup,
  GateViewResolution,
  GateViewStageKind,
  Stage,
  StageState,
} from '@/api/types';

/*
 * Pure derivations behind the run-detail evidence narrative (#1715). Every
 * function here runs over ALREADY-FETCHED data — no fetch, no React — so the
 * section models are unit-testable in isolation. Every decoder takes an
 * `unknown` payload and returns null on a shape it cannot read; none throws.
 * The loader that feeds these lives in use-run-narrative.ts.
 */

// ---------------------------------------------------------------------------
// Evidence references
// ---------------------------------------------------------------------------

/**
 * What a rendered claim links to. An audit-backed claim carries the entry's
 * SEQUENCE so the link resolves to that exact entry (approval condition 1:
 * `/runs/:runId?entry=<sequence>` + an in-page anchor, never a bare #audit);
 * an artifact-backed claim links to the stage page that renders the artifact.
 */
export type EvidenceRef =
  | {
      kind: 'audit';
      runId: string;
      sequence: number;
      /**
       * The entry's hash and category, when the claim was derived from the
       * entry itself. A claim derived from a SECONDARY record that carries
       * only the sequence — a gate-view fix-up or resolution row, which
       * names its audit sequence but neither hash nor category — leaves both
       * undefined; the link still resolves the entry, and the evidence panel
       * shows the hash and category it reads back.
       */
      entryHash?: string;
      category?: string;
    }
  | {
      kind: 'artifact';
      runId: string;
      stageId: string;
      artifactId: string;
      artifactKind: string;
      contentHash: string;
    };

/** The query parameter the run-detail page resolves to one audit entry. */
export const ENTRY_QUERY_PARAM = 'entry';

/** The in-page anchor id of the evidence panel showing entry `sequence`. */
export function evidenceAnchorId(sequence: number): string {
  return `entry-${sequence}`;
}

/** The SPA-relative href for an evidence reference. */
export function evidenceHref(ref: EvidenceRef): string {
  if (ref.kind === 'audit') {
    return `/runs/${encodeURIComponent(ref.runId)}?${ENTRY_QUERY_PARAM}=${ref.sequence}#${evidenceAnchorId(ref.sequence)}`;
  }
  return `/runs/${encodeURIComponent(ref.runId)}/stages/${encodeURIComponent(ref.stageId)}`;
}

/**
 * Parse a `?entry=` value into a positive integer sequence, or null when the
 * value is absent or not one (so a hand-edited URL renders nothing rather
 * than issuing a malformed audit read).
 */
export function parseEntryParam(raw: string | null): number | null {
  if (raw === null || !/^[1-9][0-9]*$/.test(raw)) return null;
  const n = Number(raw);
  return Number.isSafeInteger(n) ? n : null;
}

/**
 * The evidence reference for a claim whose backing record names only an
 * audit SEQUENCE (a gate-view fix-up or resolution row). The link resolves
 * the entry through the same `?entry=<sequence>` path as every other
 * audit-backed claim (approval condition 1).
 */
export function sequenceEvidenceRef(runId: string, sequence: number): EvidenceRef {
  return { kind: 'audit', runId, sequence };
}

/**
 * The href to render for an EXTERNAL url decoded from untrusted agent output
 * (an agent-authored plan artifact's ticket url, a pr url off an audit
 * payload), or null when it is not one. React escapes link TEXT but not a
 * link's scheme, so a `javascript:` value would otherwise become a clickable
 * script link for the operator. Only absolute http(s) urls are rendered as
 * links; anything else — another scheme, a relative or unparseable value —
 * returns null and the caller renders the raw value as plain text.
 */
export function safeExternalHref(raw: string | null | undefined): string | null {
  if (!raw) return null;
  let parsed: URL;
  try {
    parsed = new URL(raw);
  } catch {
    return null;
  }
  return parsed.protocol === 'http:' || parsed.protocol === 'https:' ? raw : null;
}

/** Build the evidence reference backing a claim read from `source`. */
export function evidenceRefFor(source: AuditEntry | Artifact, runId: string): EvidenceRef {
  if ('entry_hash' in source) {
    return {
      kind: 'audit',
      runId: source.run_id || runId,
      sequence: source.sequence,
      entryHash: source.entry_hash,
      category: source.category,
    };
  }
  return {
    kind: 'artifact',
    runId,
    stageId: source.stage_id,
    artifactId: source.id,
    artifactKind: source.kind,
    contentHash: source.content_hash,
  };
}

// ---------------------------------------------------------------------------
// Small payload readers
// ---------------------------------------------------------------------------

function isRecord(v: unknown): v is Record<string, unknown> {
  return typeof v === 'object' && v !== null && !Array.isArray(v);
}

function str(v: unknown): string | undefined {
  return typeof v === 'string' ? v : undefined;
}

/** A non-empty string, or undefined — the approvals block's presence test. */
function nonEmpty(v: unknown): string | undefined {
  return typeof v === 'string' && v !== '' ? v : undefined;
}

function num(v: unknown): number | undefined {
  return typeof v === 'number' && Number.isFinite(v) ? v : undefined;
}

/** Newest-first by audit sequence (a total order within one run chain). */
function newestFirst(entries: readonly AuditEntry[]): AuditEntry[] {
  return [...entries].sort((a, b) => b.sequence - a.sequence);
}

// ---------------------------------------------------------------------------
// Plan
// ---------------------------------------------------------------------------

/** One acceptance criterion as the PLAN declares it (verification.acceptance_criteria). */
export interface PlanAcceptanceCriterion {
  id: string;
  statement: string;
  blocking: boolean;
  skipExpected: boolean;
}

export interface PlanModel {
  summary: string;
  ticketUrl: string;
  ticketId: string;
  scopeFiles: ScopeFile[];
  approachStepCount: number;
  acceptanceCriteria: PlanAcceptanceCriterion[];
  evidence: EvidenceRef;
}

/**
 * Read verification.acceptance_criteria from a plan's content. plan.ts's
 * StandardV1Plan type predates the field, so it is read structurally here.
 * Returns [] when the plan declares none; elements without a string id are
 * skipped (the backend's schema validation makes that a drift case).
 */
export function readPlanAcceptanceCriteria(planContent: unknown): PlanAcceptanceCriterion[] {
  if (!isRecord(planContent) || !isRecord(planContent.verification)) return [];
  const raw = planContent.verification.acceptance_criteria;
  if (!Array.isArray(raw)) return [];
  const out: PlanAcceptanceCriterion[] = [];
  for (const el of raw) {
    if (!isRecord(el) || typeof el.id !== 'string' || el.id === '') continue;
    out.push({
      id: el.id,
      statement: str(el.statement) ?? '',
      blocking: el.blocking !== false,
      skipExpected: el.skip_expected === true,
    });
  }
  return out;
}

/** Derive the plan block model, or null when the artifact is not a standard_v1 plan. */
export function derivePlan(artifact: Artifact, runId: string): PlanModel | null {
  const content = artifact.content;
  if (!isStandardV1Plan(content)) return null;
  const files = Array.isArray(content.scope?.files) ? content.scope.files : [];
  return {
    summary: typeof content.summary === 'string' ? content.summary : '',
    ticketUrl: str(content.ticket_reference?.url) ?? '',
    ticketId: str(content.ticket_reference?.id) ?? '',
    scopeFiles: files.filter((f) => isRecord(f) && typeof f.path === 'string'),
    approachStepCount: Array.isArray(content.approach) ? content.approach.length : 0,
    acceptanceCriteria: readPlanAcceptanceCriteria(content),
    evidence: evidenceRefFor(artifact, runId),
  };
}

// ---------------------------------------------------------------------------
// Review verdicts + concern lifecycle
// ---------------------------------------------------------------------------

export const REVIEW_CATEGORIES = ['plan_reviewed', 'implement_reviewed'] as const;

export interface ReviewConcern {
  severity: string;
  category: string;
  note: string;
}

export interface ReviewVerdictRow {
  stageKind: GateViewStageKind;
  reviewerKind: string;
  reviewerModel: string;
  authority: string;
  verdict: string;
  concerns: ReviewConcern[];
  freeForm: string;
  sequence: number;
  stageId: string | null;
  ts: string;
  entryHash: string;
  evidence: EvidenceRef;
}

/** Decode one plan_reviewed / implement_reviewed entry, or null. */
export function decodeReviewVerdict(entry: AuditEntry): ReviewVerdictRow | null {
  let stageKind: GateViewStageKind;
  if (entry.category === 'plan_reviewed') stageKind = 'plan';
  else if (entry.category === 'implement_reviewed') stageKind = 'implement';
  else return null;
  const p = entry.payload;
  if (!isRecord(p) || typeof p.verdict !== 'string') return null;
  const concerns: ReviewConcern[] = [];
  if (p.concerns !== undefined && p.concerns !== null) {
    if (!Array.isArray(p.concerns)) return null;
    for (const c of p.concerns) {
      if (!isRecord(c) || typeof c.note !== 'string') return null;
      concerns.push({
        severity: str(c.severity) ?? '',
        category: str(c.category) ?? '',
        note: c.note,
      });
    }
  }
  return {
    stageKind,
    reviewerKind: str(p.reviewer_kind) ?? '',
    reviewerModel: str(p.reviewer_model) ?? '',
    authority: str(p.authority) ?? '',
    verdict: p.verdict,
    concerns,
    freeForm: str(p.free_form) ?? '',
    sequence: entry.sequence,
    stageId: entry.stage_id,
    ts: entry.ts,
    entryHash: entry.entry_hash,
    evidence: evidenceRefFor(entry, entry.run_id),
  };
}

/** Decode every review entry, oldest first; undecodable entries are counted, not dropped silently. */
export function decodeReviewVerdicts(entries: readonly AuditEntry[]): {
  rows: ReviewVerdictRow[];
  undecodable: number;
} {
  const rows: ReviewVerdictRow[] = [];
  let undecodable = 0;
  for (const e of [...entries].sort((a, b) => a.sequence - b.sequence)) {
    const row = decodeReviewVerdict(e);
    if (row) rows.push(row);
    else undecodable++;
  }
  return { rows, undecodable };
}

/** The concern-store lifecycle states (backend/internal/concern State*). */
export type ConcernState =
  | 'raised'
  | 'addressed_pending'
  | 'addressed'
  | 'reopened'
  | 'waived'
  | 'superseded'
  | 'deferred'
  | 'addressed_by_condition';

const CONCERN_STATES: ReadonlySet<string> = new Set([
  'raised',
  'addressed_pending',
  'addressed',
  'reopened',
  'waived',
  'superseded',
  'deferred',
  'addressed_by_condition',
]);

/**
 * A verdict concern's lifecycle label. `unmatched` — the gate view was read
 * but no ledger row joins this concern (visible, never dropped);
 * `unavailable` — the gate view itself could not be read; `unknown_state` —
 * a ledger row joined but carries a state this mirror does not know.
 */
export type ConcernLifecycle = ConcernState | 'unmatched' | 'unavailable' | 'unknown_state';

export interface JoinedConcern extends ReviewConcern {
  lifecycle: ConcernLifecycle;
  /** The ledger row's raw state (set whenever a row joined). */
  rawState?: string;
  concernId?: string;
  stateReason?: string;
  fixups: GateViewFixup[];
  resolutions: GateViewResolution[];
  disputed: boolean;
}

export interface JoinedVerdict extends Omit<ReviewVerdictRow, 'concerns'> {
  concerns: JoinedConcern[];
}

/**
 * Join each verdict's concerns to the gate-view lifecycle ledger. The verdict
 * payload carries no concern id (it is minted server-side), so the join key is
 * (stage_kind, reviewer_model, origin_review_sequence, category, note) against
 * OPEN rows and — because settled rows carry no origin sequence —
 * (stage_kind, reviewer_model, category, note) against SETTLED rows. Each
 * ledger row joins at most once, so a near-duplicate pair cannot both claim
 * one row; an unjoined concern gets the explicit `unmatched` lifecycle.
 * A null gateView (the read failed) yields `unavailable` on every concern.
 */
export function joinConcernLifecycle(
  verdicts: readonly ReviewVerdictRow[],
  gateView: GateView | null,
): JoinedVerdict[] {
  const openUsed = new Set<number>();
  const settledUsed = new Set<number>();
  const open = gateView?.open ?? [];
  const settled = gateView?.settled ?? [];
  return verdicts.map((v) => ({
    ...v,
    concerns: v.concerns.map((c): JoinedConcern => {
      const base: JoinedConcern = {
        ...c,
        lifecycle: 'unmatched',
        fixups: [],
        resolutions: [],
        disputed: false,
      };
      if (gateView === null) return { ...base, lifecycle: 'unavailable' };
      const oi = open.findIndex(
        (row, i) =>
          !openUsed.has(i) &&
          row.stage_kind === v.stageKind &&
          (row.reviewer_model ?? '') === v.reviewerModel &&
          row.origin_review_sequence === v.sequence &&
          row.category === c.category &&
          row.note === c.note,
      );
      if (oi >= 0) {
        openUsed.add(oi);
        const row = open[oi];
        return {
          ...base,
          lifecycle: CONCERN_STATES.has(row.state) ? (row.state as ConcernState) : 'unknown_state',
          rawState: row.state,
          concernId: row.id,
          stateReason: row.state_reason,
          fixups: row.fixups ?? [],
          resolutions: row.resolutions ?? [],
          disputed: row.disputed === true,
        };
      }
      const si = settled.findIndex(
        (row, i) =>
          !settledUsed.has(i) &&
          row.stage_kind === v.stageKind &&
          (row.reviewer_model ?? '') === v.reviewerModel &&
          row.category === c.category &&
          row.note === c.note,
      );
      if (si >= 0) {
        settledUsed.add(si);
        const row = settled[si];
        return {
          ...base,
          lifecycle: CONCERN_STATES.has(row.state) ? (row.state as ConcernState) : 'unknown_state',
          rawState: row.state,
          concernId: row.id,
          stateReason: row.state_reason,
        };
      }
      return base;
    }),
  }));
}

// ---------------------------------------------------------------------------
// Declared-vs-staged scope divergence
// ---------------------------------------------------------------------------

export interface PolicyDiffEntry {
  path: string;
  status: string;
  oldPath?: string;
}

/**
 * Decode a policy_evaluated payload's `diff` (backend/internal/policy
 * EvaluationPayload.Diff). null when the payload has no readable diff array.
 */
export function decodePolicyDiff(payload: unknown): PolicyDiffEntry[] | null {
  if (!isRecord(payload) || !Array.isArray(payload.diff)) return null;
  const out: PolicyDiffEntry[] = [];
  for (const d of payload.diff) {
    if (!isRecord(d) || typeof d.path !== 'string') return null;
    const entry: PolicyDiffEntry = { path: d.path, status: str(d.status) ?? '' };
    const oldPath = nonEmpty(d.old_path);
    if (oldPath !== undefined) entry.oldPath = oldPath;
    out.push(entry);
  }
  return out;
}

export interface ScopeDivergence {
  /** Declared scope paths, or null when no plan scope is available. */
  declared: string[] | null;
  /** Staged (actually-changed) paths, or null when no policy_evaluated diff is available. */
  staged: string[] | null;
  declaredOnly: string[];
  stagedOnly: string[];
  both: string[];
}

/**
 * Intersect the plan's declared scope.files with the REAL changed-file list
 * from the newest policy_evaluated payload. A rename (git status `R`) stages
 * BOTH its new path and its old_path: the standard_v1 scope schema has no
 * rename operation, so a planned rename is declared as a delete of the old
 * path plus a create of the new one, and without folding old_path in the
 * deleted half would read as a false "declared, not staged" divergence.
 * Divergence lists are only computed when BOTH sides are known.
 */
export function deriveScopeDivergence(
  planScopeFiles: readonly ScopeFile[] | null,
  policyEvaluatedPayload: unknown,
): ScopeDivergence {
  const declared = planScopeFiles ? [...new Set(planScopeFiles.map((f) => f.path))].sort() : null;
  const diff =
    policyEvaluatedPayload === undefined ? null : decodePolicyDiff(policyEvaluatedPayload);
  let staged: string[] | null = null;
  if (diff) {
    const set = new Set<string>();
    for (const d of diff) {
      set.add(d.path);
      if (d.status === 'R' && d.oldPath !== undefined) set.add(d.oldPath);
    }
    staged = [...set].sort();
  }
  if (declared === null || staged === null) {
    return { declared, staged, declaredOnly: [], stagedOnly: [], both: [] };
  }
  const stagedSet = new Set(staged);
  const declaredSet = new Set(declared);
  return {
    declared,
    staged,
    declaredOnly: declared.filter((p) => !stagedSet.has(p)),
    stagedOnly: staged.filter((p) => !declaredSet.has(p)),
    both: declared.filter((p) => stagedSet.has(p)),
  };
}

// ---------------------------------------------------------------------------
// Acceptance
// ---------------------------------------------------------------------------

export interface AcceptanceTallies {
  passed: number;
  failed: number;
  skipped: number;
  undecidable: number;
  total: number;
}

export interface AcceptanceOutcome {
  verdict: string;
  failureMode?: string;
  artifactId: string;
  contentHash: string;
  stageId: string | null;
  tallies: AcceptanceTallies;
  evidence: EvidenceRef;
}

/** Decode an acceptance_outcome_recorded entry, or null. */
export function decodeAcceptanceOutcome(entry: AuditEntry): AcceptanceOutcome | null {
  if (entry.category !== 'acceptance_outcome_recorded') return null;
  const p = entry.payload;
  if (!isRecord(p) || typeof p.verdict !== 'string' || typeof p.artifact_id !== 'string') {
    return null;
  }
  const out: AcceptanceOutcome = {
    verdict: p.verdict,
    artifactId: p.artifact_id,
    contentHash: str(p.content_hash) ?? '',
    stageId: entry.stage_id ?? str(p.stage_id) ?? null,
    tallies: {
      passed: num(p.criteria_passed) ?? 0,
      failed: num(p.criteria_failed) ?? 0,
      skipped: num(p.criteria_skipped) ?? 0,
      undecidable: num(p.criteria_undecidable) ?? 0,
      total: num(p.criteria_total) ?? 0,
    },
    evidence: evidenceRefFor(entry, entry.run_id),
  };
  const failureMode = nonEmpty(p.failure_mode);
  if (failureMode !== undefined) out.failureMode = failureMode;
  return out;
}

/** The newest decodable acceptance outcome, or null. */
export function latestAcceptanceOutcome(entries: readonly AuditEntry[]): AcceptanceOutcome | null {
  for (const e of newestFirst(entries)) {
    const o = decodeAcceptanceOutcome(e);
    if (o) return o;
  }
  return null;
}

export type AcceptanceRowStatus = AcceptanceCriterionOutcome | 'pending';

export interface AcceptanceRow {
  id: string;
  statement: string;
  status: AcceptanceRowStatus;
  /** False for a criterion the artifact reports but the plan does not declare. */
  inPlan: boolean;
  observed?: string;
  expected?: string;
  undecidableReason?: string;
}

/**
 * One row per PLAN criterion (the inventory is the plan artifact's
 * verification.acceptance_criteria — approval condition 2), carrying the
 * artifact's recorded result or `pending` when none was recorded. A criterion
 * the artifact reports but the plan does not declare is appended with
 * inPlan=false rather than dropped. `body` is the DECODED artifact (criteria
 * already normalized), or null when there is none yet.
 */
export function deriveAcceptanceRows(
  planCriteria: readonly PlanAcceptanceCriterion[],
  body: AcceptanceArtifactBody | null,
): AcceptanceRow[] {
  const results = new Map((body?.criteria ?? []).map((c) => [c.id, c]));
  const rows: AcceptanceRow[] = planCriteria.map((pc) => {
    const r = results.get(pc.id);
    const row: AcceptanceRow = {
      id: pc.id,
      statement: pc.statement,
      status: r ? r.result : 'pending',
      inPlan: true,
    };
    if (r?.observed !== undefined) row.observed = r.observed;
    if (r?.expected !== undefined) row.expected = r.expected;
    if (r?.undecidable_reason !== undefined) row.undecidableReason = r.undecidable_reason;
    return row;
  });
  const planned = new Set(planCriteria.map((pc) => pc.id));
  for (const r of body?.criteria ?? []) {
    if (planned.has(r.id)) continue;
    const row: AcceptanceRow = { id: r.id, statement: '', status: r.result, inPlan: false };
    if (r.observed !== undefined) row.observed = r.observed;
    if (r.expected !== undefined) row.expected = r.expected;
    if (r.undecidable_reason !== undefined) row.undecidableReason = r.undecidable_reason;
    rows.push(row);
  }
  return rows;
}

// ---------------------------------------------------------------------------
// Approvals
// ---------------------------------------------------------------------------

export interface ApprovalRow {
  decision: string;
  approver?: string;
  identity?: { provider?: string; subject?: string };
  authMethod?: string;
  channel?: string;
  delegated?: string;
  onBehalfOf?: string;
  comment?: string;
  stageId: string | null;
  ts: string;
  sequence: number;
  evidence: EvidenceRef;
}

/**
 * Decode an approval_submitted entry, or null. Every enrichment field
 * (identity, auth_method, channel — ADR-055 / #1709; delegated, on_behalf_of)
 * is OPTIONAL and set on the row only when present and non-empty, so a legacy
 * pre-#1709 row decodes to identity-less and the render can omit the row
 * rather than print an empty value.
 */
export function decodeApproval(entry: AuditEntry): ApprovalRow | null {
  if (entry.category !== 'approval_submitted') return null;
  const p = entry.payload;
  if (!isRecord(p) || typeof p.decision !== 'string') return null;
  const row: ApprovalRow = {
    decision: p.decision,
    stageId: entry.stage_id,
    ts: entry.ts,
    sequence: entry.sequence,
    evidence: evidenceRefFor(entry, entry.run_id),
  };
  const approver = nonEmpty(p.approver);
  if (approver !== undefined) row.approver = approver;
  if (isRecord(p.identity)) {
    const provider = nonEmpty(p.identity.provider);
    const subject = nonEmpty(p.identity.subject);
    if (provider !== undefined || subject !== undefined) {
      row.identity = {};
      if (provider !== undefined) row.identity.provider = provider;
      if (subject !== undefined) row.identity.subject = subject;
    }
  }
  const authMethod = nonEmpty(p.auth_method);
  if (authMethod !== undefined) row.authMethod = authMethod;
  const channel = nonEmpty(p.channel);
  if (channel !== undefined) row.channel = channel;
  const delegated = nonEmpty(p.delegated);
  if (delegated !== undefined) row.delegated = delegated;
  const onBehalfOf = nonEmpty(p.on_behalf_of);
  if (onBehalfOf !== undefined) row.onBehalfOf = onBehalfOf;
  const comment = nonEmpty(p.comment);
  if (comment !== undefined) row.comment = comment;
  return row;
}

/** Every decodable approval, oldest first. */
export function deriveApprovals(entries: readonly AuditEntry[]): ApprovalRow[] {
  return [...entries]
    .sort((a, b) => a.sequence - b.sequence)
    .map(decodeApproval)
    .filter((r): r is ApprovalRow => r !== null);
}

// ---------------------------------------------------------------------------
// Merge
// ---------------------------------------------------------------------------

export const MERGE_CATEGORIES = [
  'pr_merged',
  'merge_verdict_recorded',
  'merge_observation_recorded',
  'pr_closed_without_merge',
] as const;

export interface MergeOutcome {
  kind: 'merged' | 'closed_without_merge';
  /** pr_merged (webhook/poll) or merge_observation_recorded (reconciled after the fact). */
  source: 'pr_merged' | 'merge_observation_recorded' | 'pr_closed_without_merge';
  prUrl?: string;
  /** The merge commit (observation) or the merged head (pr_merged). */
  commitSha?: string;
  actor?: string;
  ts: string;
  evidence: EvidenceRef;
}

export interface MergeVerdict {
  verdict: string;
  prUrl?: string;
  actor?: string;
  ts: string;
  evidence: EvidenceRef;
}

export interface MergeModel {
  /** The newest terminal merge outcome, or null when the PR is not (yet) merged or closed. */
  outcome: MergeOutcome | null;
  /** The operator's recorded merge verdict, if any — recorded BEFORE the merge dispatches. */
  verdict: MergeVerdict | null;
}

function decodeMergeOutcome(entry: AuditEntry): MergeOutcome | null {
  const p = entry.payload;
  if (!isRecord(p)) return null;
  const evidence = evidenceRefFor(entry, entry.run_id);
  const actor = entry.actor_subject ?? undefined;
  switch (entry.category) {
    case 'pr_merged':
      return {
        kind: 'merged',
        source: 'pr_merged',
        prUrl: nonEmpty(p.pr_url),
        commitSha: nonEmpty(p.head_sha),
        actor: nonEmpty(p.merger) ?? actor,
        ts: entry.ts,
        evidence,
      };
    case 'merge_observation_recorded':
      return {
        kind: 'merged',
        source: 'merge_observation_recorded',
        prUrl: nonEmpty(p.pull_request_url),
        commitSha: nonEmpty(p.merge_commit_sha),
        actor,
        ts: nonEmpty(p.merged_at) ?? entry.ts,
        evidence,
      };
    case 'pr_closed_without_merge':
      return {
        kind: 'closed_without_merge',
        source: 'pr_closed_without_merge',
        prUrl: nonEmpty(p.pr_url),
        actor: nonEmpty(p.closer) ?? actor,
        ts: entry.ts,
        evidence,
      };
    default:
      return null;
  }
}

/**
 * Derive the merge block. The newest terminal outcome (pr_merged /
 * merge_observation_recorded / pr_closed_without_merge) wins; a
 * merge_verdict_recorded entry is reported separately because the backend
 * records it BEFORE dispatching the merge, so on its own it is not proof the
 * PR merged. Returns null only when there is nothing merge-related at all.
 */
export function deriveMerge(entries: readonly AuditEntry[]): MergeModel | null {
  let outcome: MergeOutcome | null = null;
  let verdict: MergeVerdict | null = null;
  for (const e of newestFirst(entries)) {
    if (outcome === null) outcome = decodeMergeOutcome(e);
    if (verdict === null && e.category === 'merge_verdict_recorded' && isRecord(e.payload)) {
      const v = str(e.payload.verdict);
      if (v !== undefined) {
        verdict = {
          verdict: v,
          prUrl: nonEmpty(e.payload.pr_url),
          actor: e.actor_subject ?? undefined,
          ts: e.ts,
          evidence: evidenceRefFor(e, e.run_id),
        };
      }
    }
  }
  if (outcome === null && verdict === null) return null;
  return { outcome, verdict };
}

// ---------------------------------------------------------------------------
// Gate vs auto-advance classification + the gate timeline
// ---------------------------------------------------------------------------

export type TransitionTreatment = 'gate' | 'auto_advance' | 'other';

export const GATE_CATEGORIES = [
  'approval_submitted',
  'acceptance_outcome_recorded',
  'approval_predicate_rejected',
] as const;

export const AUTO_ADVANCE_CATEGORIES = ['run_auto_advanced', 'run_auto_driven'] as const;

/** Every audit category classifyTransition treats as non-`other`. The loader fetches all of them. */
export const TRANSITION_CATEGORIES = [...GATE_CATEGORIES, ...AUTO_ADVANCE_CATEGORIES] as const;

const GATE_SET: ReadonlySet<string> = new Set(GATE_CATEGORIES);
const AUTO_ADVANCE_SET: ReadonlySet<string> = new Set(AUTO_ADVANCE_CATEGORIES);

/**
 * Classify an audit entry: a human/verdict decision point is a `gate`; a
 * mechanical drive-engine step is an `auto_advance`; everything else `other`.
 * This drives the distinct visual treatment in the narrative timeline.
 */
export function classifyTransition(entry: Pick<AuditEntry, 'category'>): TransitionTreatment {
  if (GATE_SET.has(entry.category)) return 'gate';
  if (AUTO_ADVANCE_SET.has(entry.category)) return 'auto_advance';
  return 'other';
}

const PARKED_GATE_STATES: ReadonlySet<StageState> = new Set<StageState>([
  'awaiting_approval',
  'awaiting_deploy_approval',
]);

/**
 * Classify a stage from its STATE (approval condition 3): a stage parked at a
 * human gate is a `gate` even before any audit entry records a decision.
 */
export function classifyStage(stage: Pick<Stage, 'state'>): TransitionTreatment {
  return PARKED_GATE_STATES.has(stage.state) ? 'gate' : 'other';
}

export interface TimelineTransition {
  category: string;
  treatment: TransitionTreatment;
  sequence: number;
  ts: string;
  evidence: EvidenceRef;
}

export interface TimelineStage {
  stage: Stage;
  /** From the stage's own state: `gate` while parked at an approval gate. */
  treatment: TransitionTreatment;
  transitions: TimelineTransition[];
}

export interface GateTimeline {
  stages: TimelineStage[];
  /** Transitions not tagged with any listed stage (run-level auto-advances, etc.). */
  runTransitions: TimelineTransition[];
}

/**
 * Annotate the run's stages (by sequence) with their state-derived treatment
 * and attach each classified transition entry to its stage, oldest first.
 * Entries classified `other` are omitted — the raw #audit list keeps them.
 */
export function deriveGateTimeline(
  stages: readonly Stage[],
  entries: readonly AuditEntry[],
): GateTimeline {
  const byStage = new Map<string, TimelineTransition[]>();
  const runTransitions: TimelineTransition[] = [];
  const stageIds = new Set(stages.map((s) => s.id));
  for (const e of [...entries].sort((a, b) => a.sequence - b.sequence)) {
    const treatment = classifyTransition(e);
    if (treatment === 'other') continue;
    const t: TimelineTransition = {
      category: e.category,
      treatment,
      sequence: e.sequence,
      ts: e.ts,
      evidence: evidenceRefFor(e, e.run_id),
    };
    if (e.stage_id && stageIds.has(e.stage_id)) {
      const list = byStage.get(e.stage_id) ?? [];
      list.push(t);
      byStage.set(e.stage_id, list);
    } else {
      runTransitions.push(t);
    }
  }
  return {
    stages: [...stages]
      .sort((a, b) => a.sequence - b.sequence)
      .map((stage) => ({
        stage,
        treatment: classifyStage(stage),
        transitions: byStage.get(stage.id) ?? [],
      })),
    runTransitions,
  };
}
