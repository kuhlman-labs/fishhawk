import { Link } from 'react-router';
import { ChevronRight } from 'lucide-react';
import { describeFailure } from '@/api/types';
import { StageStateBadge } from '@/components/stage-state-badge';
import { Section } from '@/plan/sections';
import { AcceptanceBlock } from './acceptance-block';
import { ApprovalsBlock, MergeBlock } from './decision-blocks';
import { DiffSummaryBlock } from './diff-summary-block';
import { DegradeNotes, EvidenceLink, TransitionMarker } from './evidence';
import type { EvidenceRef, GateTimeline, TimelineTransition } from './narrative';
import { PlanBlock } from './plan-block';
import type { NarrativeSectionId, RunNarrative, SectionState } from './use-run-narrative';
import { VerdictsBlock } from './verdicts-block';

/*
 * The ordered run-detail narrative (#1715). The section ORDER is data — one
 * array, NARRATIVE_SECTIONS — not a markup accident: plan, advisory
 * verdicts, diff summary, acceptance, approvals, merge. The gate timeline
 * follows, carrying the run's stages (and their deep links) annotated with
 * the gate-vs-auto-advance treatment.
 */

type SixSectionId = Exclude<NarrativeSectionId, 'timeline'>;

/** The mandated section order (issue #1715). Each block owns its own heading. */
const NARRATIVE_SECTIONS: readonly SixSectionId[] = [
  'plan',
  'verdicts',
  'diff',
  'acceptance',
  'approvals',
  'merge',
];

function renderSection(
  id: SixSectionId,
  narrative: RunNarrative,
  stagedEvidence: EvidenceRef | null,
): React.ReactNode {
  const s = narrative.sections;
  switch (id) {
    case 'plan':
      return <PlanBlock section={s.plan} />;
    case 'verdicts':
      return <VerdictsBlock section={s.verdicts} />;
    case 'diff':
      return (
        <DiffSummaryBlock
          section={s.diff}
          declaredEvidence={s.plan.model?.evidence ?? null}
          stagedEvidence={stagedEvidence}
        />
      );
    case 'acceptance':
      return <AcceptanceBlock section={s.acceptance} />;
    case 'approvals':
      return <ApprovalsBlock section={s.approvals} />;
    case 'merge':
      return <MergeBlock section={s.merge} />;
  }
}

function TransitionItem({ t }: { t: TimelineTransition }) {
  return (
    <li
      data-testid="timeline-transition"
      data-category={t.category}
      data-treatment={t.treatment}
      className="flex items-center gap-2 py-0.5"
    >
      <TransitionMarker kind={t.treatment} />
      <span className="font-mono text-xs">{t.category}</span>
      <EvidenceLink refTo={t.evidence} />
    </li>
  );
}

export function GateTimelineBlock({
  runId,
  section,
}: {
  runId: string;
  section: SectionState<GateTimeline>;
}) {
  const timeline = section.model;
  return (
    <Section id="narrative-timeline" title="Gate timeline">
      <div className="space-y-3">
        <DegradeNotes notes={section.notes} />
        {timeline && (
          <ol className="overflow-hidden rounded-md border border-neutral-200 dark:border-neutral-800">
            {timeline.stages.length === 0 && (
              <li className="px-4 py-3 text-sm text-neutral-500">No stages yet.</li>
            )}
            {timeline.stages.map(({ stage, treatment, transitions }) => (
              <li
                key={stage.id}
                data-testid="timeline-stage"
                data-stage-id={stage.id}
                data-treatment={treatment}
                className={`border-b border-neutral-200 last:border-b-0 dark:border-neutral-800 ${
                  treatment === 'gate' ? 'border-l-4 border-l-amber-500' : ''
                }`}
              >
                <Link
                  to={`/runs/${runId}/stages/${stage.id}`}
                  aria-label={`Review ${stage.type} stage`}
                  className="flex items-center gap-4 px-4 py-3 hover:bg-neutral-50 focus-visible:bg-neutral-50 focus-visible:ring-1 focus-visible:ring-neutral-400 focus-visible:outline-none dark:hover:bg-neutral-900/50 dark:focus-visible:bg-neutral-900/50"
                >
                  <span className="font-mono text-xs text-neutral-500">#{stage.sequence}</span>
                  <span className="font-mono text-sm font-medium">{stage.type}</span>
                  <span className="font-mono text-xs text-neutral-500">
                    {stage.executor.kind}:{stage.executor.ref}
                  </span>
                  {stage.resolved_model && (
                    <span
                      className="font-mono text-xs text-neutral-500"
                      title="Resolved model for this stage's agent spawn"
                    >
                      {stage.resolved_model}
                    </span>
                  )}
                  <span className="ml-auto flex items-center gap-2 font-mono text-xs">
                    {treatment === 'gate' && (
                      <TransitionMarker kind="gate" label="Parked at gate" />
                    )}
                    {stage.state === 'failed' && stage.failure_category && (
                      <span
                        className="rounded bg-rose-100 px-1.5 py-0.5 text-rose-800 dark:bg-rose-900/40 dark:text-rose-300"
                        title={describeFailure(stage.failure_category) ?? undefined}
                      >
                        {stage.failure_category}
                      </span>
                    )}
                    <StageStateBadge state={stage.state} />
                  </span>
                  <ChevronRight className="size-4 text-neutral-400" aria-hidden />
                </Link>
                {transitions.length > 0 && (
                  <ul className="px-4 pb-2">
                    {transitions.map((t) => (
                      <TransitionItem key={t.sequence} t={t} />
                    ))}
                  </ul>
                )}
              </li>
            ))}
          </ol>
        )}
        {timeline && timeline.runTransitions.length > 0 && (
          <div className="space-y-1">
            <h3 className="text-xs font-medium text-neutral-600 dark:text-neutral-400">
              Run-level transitions
            </h3>
            <ul>
              {timeline.runTransitions.map((t) => (
                <TransitionItem key={t.sequence} t={t} />
              ))}
            </ul>
          </div>
        )}
      </div>
    </Section>
  );
}

export function RunNarrativeView({
  narrative,
  stagedEvidence,
}: {
  narrative: RunNarrative;
  /** The policy_evaluated entry backing the staged scope column, when known. */
  stagedEvidence: EvidenceRef | null;
}) {
  return (
    <div className="space-y-8">
      {NARRATIVE_SECTIONS.map((id) => (
        <div key={id} data-testid="narrative-section" data-section={id}>
          {renderSection(id, narrative, stagedEvidence)}
        </div>
      ))}
      <GateTimelineBlock runId={narrative.run.id} section={narrative.timeline} />
    </div>
  );
}
