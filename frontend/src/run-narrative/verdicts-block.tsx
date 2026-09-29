import { Section } from '@/plan/sections';
import { DegradeNotes, EvidenceLink } from './evidence';
import {
  sequenceEvidenceRef,
  type ConcernLifecycle,
  type JoinedConcern,
  type JoinedVerdict,
} from './narrative';
import type { SectionState, VerdictsModel } from './use-run-narrative';

/*
 * Advisory verdicts: one column per reviewer verdict, side by side, each
 * concern listed under the verdict that raised it with its lifecycle state
 * ADJACENT to it (joined from the gate-view ledger), plus its fix-up and
 * resolution history when the join supplied one.
 */

const lifecycleLabel: Record<ConcernLifecycle, string> = {
  raised: 'raised',
  addressed_pending: 'addressed (pending review)',
  addressed: 'addressed',
  reopened: 'reopened',
  waived: 'waived',
  superseded: 'superseded',
  deferred: 'deferred',
  addressed_by_condition: 'addressed by condition',
  unmatched: 'unmatched',
  unavailable: 'lifecycle unavailable',
  unknown_state: 'unknown state',
};

const lifecycleStyle: Partial<Record<ConcernLifecycle, string>> = {
  raised: 'bg-rose-100 text-rose-800 dark:bg-rose-900/40 dark:text-rose-300',
  reopened: 'bg-rose-100 text-rose-800 dark:bg-rose-900/40 dark:text-rose-300',
  addressed: 'bg-emerald-100 text-emerald-800 dark:bg-emerald-900/40 dark:text-emerald-300',
  addressed_by_condition:
    'bg-emerald-100 text-emerald-800 dark:bg-emerald-900/40 dark:text-emerald-300',
  addressed_pending: 'bg-amber-100 text-amber-800 dark:bg-amber-900/40 dark:text-amber-300',
  waived: 'bg-neutral-200 text-neutral-800 dark:bg-neutral-800 dark:text-neutral-200',
  deferred: 'bg-neutral-200 text-neutral-800 dark:bg-neutral-800 dark:text-neutral-200',
};

function LifecycleBadge({ concern }: { concern: JoinedConcern }) {
  const label =
    concern.lifecycle === 'unknown_state' && concern.rawState
      ? concern.rawState
      : lifecycleLabel[concern.lifecycle];
  return (
    <span
      data-testid="concern-lifecycle"
      data-lifecycle={concern.lifecycle}
      title={concern.stateReason}
      className={`shrink-0 rounded px-1.5 py-0.5 font-mono text-xs ${
        lifecycleStyle[concern.lifecycle] ??
        'border border-dashed border-neutral-400 text-neutral-600 dark:text-neutral-400'
      }`}
    >
      {label}
    </span>
  );
}

/*
 * Each history claim resolves ITS OWN backing entry (approval condition 1):
 * a fix-up at sequence 12 and a resolution at sequence 13 are later events
 * than the review the column links, so the review's entry cannot
 * substantiate them. The gate-view rows name only the sequence, so the link
 * is built from that and the evidence panel reads back the hash/category.
 */
function ConcernItem({ concern, runId }: { concern: JoinedConcern; runId: string }) {
  return (
    <li data-testid="verdict-concern" className="space-y-1 py-2">
      <div className="flex items-start gap-2">
        <LifecycleBadge concern={concern} />
        <span className="font-mono text-xs text-neutral-500">
          {concern.severity || '—'} · {concern.category || '—'}
        </span>
        {concern.disputed && (
          <span className="font-mono text-xs text-amber-700 dark:text-amber-300">disputed</span>
        )}
      </div>
      <p className="text-sm whitespace-pre-wrap">{concern.note}</p>
      {(concern.fixups.length > 0 || concern.resolutions.length > 0) && (
        <ul data-testid="concern-history" className="space-y-0.5 pl-2 font-mono text-xs">
          {concern.fixups.map((f) => (
            <li
              key={`f-${f.sequence}`}
              data-testid="concern-fixup"
              className="flex flex-wrap items-baseline gap-1 text-neutral-600 dark:text-neutral-400"
            >
              <span>
                fix-up #{f.sequence}: {f.outcome}
                {f.head_sha ? ` @ ${f.head_sha.slice(0, 12)}` : ''}
              </span>
              <EvidenceLink refTo={sequenceEvidenceRef(runId, f.sequence)} />
            </li>
          ))}
          {concern.resolutions.map((r) => (
            <li
              key={`r-${r.sequence}`}
              data-testid="concern-resolution"
              className="flex flex-wrap items-baseline gap-1 text-neutral-600 dark:text-neutral-400"
            >
              <span>
                resolution #{r.sequence}: {r.resolution}
              </span>
              <EvidenceLink refTo={sequenceEvidenceRef(runId, r.sequence)} />
            </li>
          ))}
        </ul>
      )}
    </li>
  );
}

function VerdictColumn({ verdict }: { verdict: JoinedVerdict }) {
  return (
    <article
      data-testid="review-verdict"
      data-stage-kind={verdict.stageKind}
      className="space-y-2 rounded-md border border-neutral-200 p-3 dark:border-neutral-800"
    >
      <header className="space-y-1">
        <div className="flex flex-wrap items-center gap-2">
          <span className="font-mono text-xs text-neutral-500 uppercase">{verdict.stageKind}</span>
          <span className="font-mono text-sm font-medium">
            {verdict.reviewerModel || verdict.reviewerKind || 'reviewer'}
          </span>
        </div>
        <div className="flex flex-wrap items-center gap-2 text-xs">
          <span data-testid="verdict-value" className="font-mono font-semibold">
            {verdict.verdict}
          </span>
          {verdict.authority && <span className="text-neutral-500">{verdict.authority}</span>}
          <EvidenceLink refTo={verdict.evidence} />
        </div>
      </header>
      {verdict.concerns.length === 0 ? (
        <p className="text-xs text-neutral-500">No concerns raised.</p>
      ) : (
        <ul className="divide-y divide-neutral-200 dark:divide-neutral-800">
          {verdict.concerns.map((c, i) => (
            <ConcernItem key={i} concern={c} runId={verdict.evidence.runId} />
          ))}
        </ul>
      )}
    </article>
  );
}

export function VerdictsBlock({ section }: { section: SectionState<VerdictsModel> }) {
  const model = section.model;
  return (
    <Section id="narrative-verdicts" title="Advisory verdicts">
      <div className="space-y-3">
        <DegradeNotes notes={section.notes} />
        {model &&
          (model.verdicts.length === 0 ? (
            <p className="text-sm text-neutral-500">No review verdicts recorded yet.</p>
          ) : (
            <div className="grid gap-3 md:grid-cols-2">
              {model.verdicts.map((v) => (
                <VerdictColumn key={v.sequence} verdict={v} />
              ))}
            </div>
          ))}
      </div>
    </Section>
  );
}
