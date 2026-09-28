import { decodeAcceptanceArtifact, type AcceptanceArtifactBody } from '@/api/acceptance';
import { api } from '@/api/client';
import type { Artifact, AuditEntry, GateView, PaginatedList, Run, Stage } from '@/api/types';
import { useAsync, type AsyncState } from '@/api/use-async';
import {
  MERGE_CATEGORIES,
  REVIEW_CATEGORIES,
  TRANSITION_CATEGORIES,
  decodePolicyDiff,
  decodeReviewVerdicts,
  derivePlan,
  deriveAcceptanceRows,
  deriveApprovals,
  deriveGateTimeline,
  deriveMerge,
  deriveScopeDivergence,
  evidenceRefFor,
  joinConcernLifecycle,
  latestAcceptanceOutcome,
  type AcceptanceOutcome,
  type AcceptanceRow,
  type ApprovalRow,
  type EvidenceRef,
  type GateTimeline,
  type JoinedVerdict,
  type MergeModel,
  type PlanModel,
  type ScopeDivergence,
} from './narrative';

/*
 * The composed loader behind the run-detail evidence narrative (#1715).
 * Every read runs under Promise.allSettled, deliberately: one failed or
 * unconfigured read (a 503 gate view, a 403 on one audit category) degrades
 * the ONE section that depends on it — recorded as a DegradeNote naming the
 * read — and never blanks the page. The only page-level failure is getRun,
 * whose absence leaves nothing to render a header for.
 */

/** The subset of the API client the loader reads. Injectable for tests. */
export type NarrativeClient = Pick<
  typeof api,
  | 'getRun'
  | 'listRunStages'
  | 'getRunGateView'
  | 'listRunAudit'
  | 'listStageArtifacts'
  | 'getArtifact'
>;

export type NarrativeSectionId =
  'plan' | 'verdicts' | 'diff' | 'acceptance' | 'approvals' | 'merge' | 'timeline';

export type DegradeKind =
  /** The read itself rejected (network, 4xx/5xx, unconfigured). */
  | 'read_failed'
  /** The read succeeded but the thing does not exist (yet). */
  | 'absent'
  /** The read succeeded but its content could not be decoded. */
  | 'unreadable'
  /** The read succeeded but is partial (page bound hit, history gaps). */
  | 'incomplete';

export interface DegradeNote {
  section: NarrativeSectionId;
  kind: DegradeKind;
  /** The read (or derivation input) the note is about, e.g. `getRunGateView`, `audit:policy_evaluated`. */
  read: string;
  message: string;
}

/** A section's model plus its degrade notes. model === null means the whole section is unavailable. */
export interface SectionState<T> {
  model: T | null;
  notes: DegradeNote[];
}

export interface VerdictsModel {
  verdicts: JoinedVerdict[];
}

export interface AcceptanceModel {
  /** The newest recorded outcome, or null when acceptance has not recorded one. */
  outcome: AcceptanceOutcome | null;
  /**
   * One row per plan criterion (pending when unrecorded), or null when the
   * per-criterion evidence could not be read — the outcome's verdict and
   * tallies still render from the audit payload in that case.
   */
  rows: AcceptanceRow[] | null;
  body: AcceptanceArtifactBody | null;
  /** The acceptance artifact backing the rows, when it was read. */
  artifact: EvidenceRef | null;
}

export interface MergeSectionModel {
  /** null = no merge-related entry at all: render the explicit not-merged state. */
  merge: MergeModel | null;
}

export interface RunNarrativeSections {
  plan: SectionState<PlanModel>;
  verdicts: SectionState<VerdictsModel>;
  diff: SectionState<ScopeDivergence>;
  acceptance: SectionState<AcceptanceModel>;
  approvals: SectionState<ApprovalRow[]>;
  merge: SectionState<MergeSectionModel>;
}

export interface RunNarrative {
  run: Run;
  /** The run's stages, or [] when the stages read failed (see the timeline section's note). */
  stages: Stage[];
  sections: RunNarrativeSections;
  timeline: SectionState<GateTimeline>;
  /** Every section's notes, flattened, in section order. */
  degraded: DegradeNote[];
}

/** Every audit category the narrative reads, one bounded category-filtered read each. */
export const NARRATIVE_AUDIT_CATEGORIES: readonly string[] = [
  ...new Set<string>([
    ...REVIEW_CATEGORIES,
    'policy_evaluated',
    ...MERGE_CATEGORIES,
    // The full classifyTransition set (approval condition 3): gate AND
    // auto-advance categories, including run_auto_driven and
    // approval_predicate_rejected, so every classified transition is reachable.
    ...TRANSITION_CATEGORIES,
  ]),
];

/** listRunAudit's server cap. */
export const AUDIT_PAGE_LIMIT = 500;
/** Hard bound on pages per category read; hitting it records an `incomplete` note. */
export const AUDIT_MAX_PAGES = 10;

interface CategoryRead {
  entries: AuditEntry[];
  truncated: boolean;
}

async function readCategory(
  client: NarrativeClient,
  runId: string,
  category: string,
): Promise<CategoryRead> {
  const entries: AuditEntry[] = [];
  let cursor: string | undefined;
  for (let page = 0; page < AUDIT_MAX_PAGES; page++) {
    const res: PaginatedList<AuditEntry> = await client.listRunAudit(runId, {
      category,
      limit: AUDIT_PAGE_LIMIT,
      cursor,
    });
    entries.push(...res.items);
    if (!res.next_cursor) return { entries, truncated: false };
    cursor = res.next_cursor;
  }
  return { entries, truncated: true };
}

function errorText(reason: unknown): string {
  return reason instanceof Error ? reason.message : String(reason);
}

interface PlanRead {
  artifact: Artifact | null;
  absent: string | null;
}

/** Newest plan stage → newest plan artifact → its content. */
async function readPlanArtifact(
  client: NarrativeClient,
  stages: readonly Stage[],
): Promise<PlanRead> {
  const planStage = [...stages]
    .filter((s) => s.type === 'plan')
    .sort((a, b) => b.sequence - a.sequence)[0];
  if (!planStage) return { artifact: null, absent: 'run has no plan stage' };
  const list = await client.listStageArtifacts(planStage.id);
  const planArtifact = list.items
    .filter((a) => a.kind === 'plan')
    .sort((a, b) => (a.created_at < b.created_at ? 1 : a.created_at > b.created_at ? -1 : 0))[0];
  if (!planArtifact) return { artifact: null, absent: 'plan stage has no plan artifact' };
  if (planArtifact.content !== undefined) return { artifact: planArtifact, absent: null };
  return { artifact: await client.getArtifact(planArtifact.id), absent: null };
}

/**
 * Load every read the narrative needs and derive each section. Rejects only
 * when getRun rejects.
 */
export async function loadRunNarrative(
  runId: string,
  client: NarrativeClient = api,
): Promise<RunNarrative> {
  const [runR, stagesR, gateViewR, ...categoryRs] = await Promise.allSettled([
    client.getRun(runId),
    client.listRunStages(runId),
    client.getRunGateView(runId),
    ...NARRATIVE_AUDIT_CATEGORIES.map((c) => readCategory(client, runId, c)),
  ] as const);
  if (runR.status === 'rejected') throw runR.reason;
  const run = runR.value as Run;

  const byCategory = new Map<string, PromiseSettledResult<CategoryRead>>();
  NARRATIVE_AUDIT_CATEGORIES.forEach((c, i) => {
    byCategory.set(c, categoryRs[i] as PromiseSettledResult<CategoryRead>);
  });

  const notes: Record<NarrativeSectionId, DegradeNote[]> = {
    plan: [],
    verdicts: [],
    diff: [],
    acceptance: [],
    approvals: [],
    merge: [],
    timeline: [],
  };
  const note = (section: NarrativeSectionId, kind: DegradeKind, read: string, message: string) => {
    notes[section].push({ section, kind, read, message });
  };

  /**
   * Entries for `category`, or null when its read rejected. A read that hit
   * the page bound returns its entries and records an `incomplete` note on
   * each section named.
   */
  const entriesFor = (category: string, sections: NarrativeSectionId[]): AuditEntry[] | null => {
    const r = byCategory.get(category);
    if (!r || r.status === 'rejected') return null;
    if (r.value.truncated) {
      for (const s of sections) {
        note(
          s,
          'incomplete',
          `audit:${category}`,
          `${category} history truncated after ${AUDIT_MAX_PAGES} pages`,
        );
      }
    }
    return r.value.entries;
  };
  const failedRead = (category: string): string => {
    const r = byCategory.get(category);
    return r && r.status === 'rejected' ? errorText(r.reason) : 'read failed';
  };

  const stages: Stage[] | null =
    stagesR.status === 'fulfilled' ? (stagesR.value as { items: Stage[] }).items : null;

  // Phase 2: the two artifact reads that depend on phase-1 results.
  const acceptanceEntries = entriesFor('acceptance_outcome_recorded', ['acceptance']);
  const outcome = acceptanceEntries ? latestAcceptanceOutcome(acceptanceEntries) : null;
  const [planR, acceptanceArtifactR] = await Promise.allSettled([
    stages ? readPlanArtifact(client, stages) : Promise.reject(new Error('run stages unavailable')),
    outcome ? client.getArtifact(outcome.artifactId) : Promise.resolve(null),
  ]);

  // --- plan -------------------------------------------------------------
  let plan: PlanModel | null = null;
  if (planR.status === 'rejected') {
    note('plan', 'read_failed', 'plan artifact', `no plan artifact: ${errorText(planR.reason)}`);
  } else if (planR.value.artifact === null) {
    note('plan', 'absent', 'plan artifact', `no plan artifact (${planR.value.absent})`);
  } else {
    plan = derivePlan(planR.value.artifact, runId);
    if (plan === null) {
      note(
        'plan',
        'unreadable',
        'plan artifact',
        'no plan artifact: content is not a standard_v1 plan',
      );
    }
  }

  // --- verdicts ---------------------------------------------------------
  let verdicts: VerdictsModel | null = null;
  {
    const reviewEntries: AuditEntry[] = [];
    let readable = 0;
    for (const c of REVIEW_CATEGORIES) {
      const es = entriesFor(c, ['verdicts']);
      if (es === null) {
        note(
          'verdicts',
          'read_failed',
          `audit:${c}`,
          `${c} verdicts unavailable: ${failedRead(c)}`,
        );
      } else {
        readable++;
        reviewEntries.push(...es);
      }
    }
    if (readable > 0) {
      const { rows, undecodable } = decodeReviewVerdicts(reviewEntries);
      if (undecodable > 0) {
        note(
          'verdicts',
          'unreadable',
          'audit:review',
          `${undecodable} review verdict(s) could not be decoded`,
        );
      }
      let gateView: GateView | null = null;
      if (gateViewR.status === 'rejected') {
        note(
          'verdicts',
          'read_failed',
          'getRunGateView',
          `concern lifecycle unavailable: ${errorText(gateViewR.reason)}`,
        );
      } else {
        gateView = gateViewR.value as GateView;
        if (gateView.history_incomplete) {
          const gaps = gateView.history_gaps?.length ? `: ${gateView.history_gaps.join(', ')}` : '';
          note('verdicts', 'incomplete', 'getRunGateView', `concern history incomplete${gaps}`);
        }
      }
      verdicts = { verdicts: joinConcernLifecycle(rows, gateView) };
    }
  }

  // --- diff summary -----------------------------------------------------
  let policyPayload: unknown = undefined;
  {
    if (plan === null) note('diff', 'absent', 'plan artifact', 'declared scope unavailable');
    const policyEntries = entriesFor('policy_evaluated', ['diff']);
    if (policyEntries === null) {
      note(
        'diff',
        'read_failed',
        'audit:policy_evaluated',
        `staged scope unavailable: ${failedRead('policy_evaluated')}`,
      );
    } else {
      const newest = [...policyEntries]
        .sort((a, b) => b.sequence - a.sequence)
        .find((e) => decodePolicyDiff(e.payload) !== null);
      if (newest) {
        policyPayload = newest.payload;
      } else if (policyEntries.length > 0) {
        note(
          'diff',
          'unreadable',
          'audit:policy_evaluated',
          'staged scope unreadable: no policy_evaluated diff could be decoded',
        );
      } else {
        note('diff', 'absent', 'audit:policy_evaluated', 'staged scope not yet evaluated');
      }
    }
  }
  const diff = deriveScopeDivergence(plan ? plan.scopeFiles : null, policyPayload);

  // --- acceptance -------------------------------------------------------
  let acceptance: AcceptanceModel | null = null;
  if (acceptanceEntries === null) {
    note(
      'acceptance',
      'read_failed',
      'audit:acceptance_outcome_recorded',
      `acceptance outcome unavailable: ${failedRead('acceptance_outcome_recorded')}`,
    );
  } else {
    const planCriteria = plan ? plan.acceptanceCriteria : [];
    if (plan === null) {
      note(
        'acceptance',
        'absent',
        'plan artifact',
        'acceptance criteria inventory unavailable (no plan artifact)',
      );
    }
    if (outcome === null) {
      acceptance = {
        outcome: null,
        rows: deriveAcceptanceRows(planCriteria, null),
        body: null,
        artifact: null,
      };
    } else if (acceptanceArtifactR.status === 'rejected') {
      note(
        'acceptance',
        'read_failed',
        'acceptance artifact',
        `per-criterion evidence unavailable: ${errorText(acceptanceArtifactR.reason)}`,
      );
      acceptance = { outcome, rows: null, body: null, artifact: null };
    } else {
      const artifact = acceptanceArtifactR.value as Artifact | null;
      const body = artifact ? decodeAcceptanceArtifact(artifact.content) : null;
      if (artifact === null || body === null) {
        note(
          'acceptance',
          'unreadable',
          'acceptance artifact',
          'unreadable acceptance artifact: per-criterion evidence unavailable',
        );
        acceptance = {
          outcome,
          rows: null,
          body: null,
          artifact: artifact ? evidenceRefFor(artifact, runId) : null,
        };
      } else {
        acceptance = {
          outcome,
          rows: deriveAcceptanceRows(planCriteria, body),
          body,
          artifact: evidenceRefFor(artifact, runId),
        };
      }
    }
  }

  // --- approvals --------------------------------------------------------
  let approvals: ApprovalRow[] | null = null;
  {
    const es = entriesFor('approval_submitted', ['approvals']);
    if (es === null) {
      note(
        'approvals',
        'read_failed',
        'audit:approval_submitted',
        `approvals unavailable: ${failedRead('approval_submitted')}`,
      );
    } else {
      approvals = deriveApprovals(es);
    }
  }

  // --- merge ------------------------------------------------------------
  let merge: MergeSectionModel | null = null;
  {
    const mergeEntries: AuditEntry[] = [];
    let readable = 0;
    for (const c of MERGE_CATEGORIES) {
      const es = entriesFor(c, ['merge']);
      if (es === null) {
        note('merge', 'read_failed', `audit:${c}`, `${c} unavailable: ${failedRead(c)}`);
      } else {
        readable++;
        mergeEntries.push(...es);
      }
    }
    if (readable > 0) merge = { merge: deriveMerge(mergeEntries) };
  }

  // --- gate timeline ----------------------------------------------------
  let timeline: GateTimeline | null = null;
  if (stages === null) {
    note(
      'timeline',
      'read_failed',
      'listRunStages',
      `stages unavailable: ${stagesR.status === 'rejected' ? errorText(stagesR.reason) : 'read failed'}`,
    );
  } else {
    const transitionEntries: AuditEntry[] = [];
    for (const c of TRANSITION_CATEGORIES) {
      const es = entriesFor(c, ['timeline']);
      if (es === null) {
        note(
          'timeline',
          'read_failed',
          `audit:${c}`,
          `${c} transitions unavailable: ${failedRead(c)}`,
        );
      } else {
        transitionEntries.push(...es);
      }
    }
    timeline = deriveGateTimeline(stages, transitionEntries);
  }

  const sections: RunNarrativeSections = {
    plan: { model: plan, notes: notes.plan },
    verdicts: { model: verdicts, notes: notes.verdicts },
    diff: { model: diff, notes: notes.diff },
    acceptance: { model: acceptance, notes: notes.acceptance },
    approvals: { model: approvals, notes: notes.approvals },
    merge: { model: merge, notes: notes.merge },
  };
  return {
    run,
    stages: stages ?? [],
    sections,
    timeline: { model: timeline, notes: notes.timeline },
    degraded: (Object.keys(notes) as NarrativeSectionId[]).flatMap((s) => notes[s]),
  };
}

/** Load the narrative for `runId`; refetches when the id changes. */
export function useRunNarrative(runId: string): AsyncState<RunNarrative> {
  return useAsync(() => loadRunNarrative(runId), [runId]);
}

/**
 * Resolve the single audit entry at `sequence` INDEPENDENTLY of list
 * pagination (approval condition 1): `since_sequence = sequence - 1` with
 * `limit = 1` returns the first entry strictly after N-1. The returned entry
 * is accepted only when its sequence IS N — the next entry after a gap is
 * not the one the link named — so a missing sequence resolves to null (the
 * render's "entry not found" state).
 */
export async function loadAuditEntry(
  runId: string,
  sequence: number,
  client: Pick<NarrativeClient, 'listRunAudit'> = api,
): Promise<AuditEntry | null> {
  const res = await client.listRunAudit(runId, { sinceSequence: sequence - 1, limit: 1 });
  const entry = res.items[0];
  return entry && entry.sequence === sequence ? entry : null;
}

/** Hook form of loadAuditEntry; `sequence === null` resolves to null without a read. */
export function useAuditEntry(
  runId: string,
  sequence: number | null,
): AsyncState<AuditEntry | null> {
  return useAsync(
    () => (sequence === null ? Promise.resolve(null) : loadAuditEntry(runId, sequence)),
    [runId, sequence],
  );
}
