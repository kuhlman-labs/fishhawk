import { ArrowUpRight } from 'lucide-react';
import { Section, SummarySection } from '@/plan/sections';
import { DegradeNotes, EvidenceLink } from './evidence';
import { safeExternalHref, type PlanModel } from './narrative';
import type { SectionState } from './use-run-narrative';

/*
 * Plan block: what the run committed to. Reuses the plan surface's
 * <Section> / <SummarySection> so it stays in lockstep with the stage page;
 * the full plan document lives one EvidenceLink away on the plan stage page.
 */
export function PlanBlock({ section }: { section: SectionState<PlanModel> }) {
  const plan = section.model;
  // The ticket url comes out of the agent-authored plan artifact, so it is
  // untrusted: link it only when it is an http(s) url, else render the id.
  const ticketHref = safeExternalHref(plan?.ticketUrl);
  return (
    <Section id="narrative-plan" title="Plan">
      <div className="space-y-3">
        <DegradeNotes notes={section.notes} />
        {plan && (
          <>
            <SummarySection summary={plan.summary} />
            <dl className="grid grid-cols-[10rem_1fr] gap-y-1 text-sm">
              {plan.ticketId && (
                <>
                  <dt className="text-neutral-500">Ticket</dt>
                  <dd className="font-mono">
                    {ticketHref ? (
                      <a
                        href={ticketHref}
                        rel="noreferrer"
                        target="_blank"
                        className="inline-flex items-center gap-1 hover:underline"
                      >
                        {plan.ticketId}
                        <ArrowUpRight className="size-3.5" aria-hidden />
                      </a>
                    ) : (
                      plan.ticketId
                    )}
                  </dd>
                </>
              )}
              <dt className="text-neutral-500">Scope</dt>
              <dd data-testid="plan-scope-count" className="font-mono">
                {plan.scopeFiles.length} {plan.scopeFiles.length === 1 ? 'file' : 'files'}
              </dd>
              <dt className="text-neutral-500">Approach</dt>
              <dd data-testid="plan-approach-count" className="font-mono">
                {plan.approachStepCount} {plan.approachStepCount === 1 ? 'step' : 'steps'}
              </dd>
              <dt className="text-neutral-500">Acceptance criteria</dt>
              <dd className="font-mono">{plan.acceptanceCriteria.length}</dd>
              <dt className="text-neutral-500">Evidence</dt>
              <dd>
                <EvidenceLink refTo={plan.evidence} />
              </dd>
            </dl>
          </>
        )}
      </div>
    </Section>
  );
}
