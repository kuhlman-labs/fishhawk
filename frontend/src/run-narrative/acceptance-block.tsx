import { Section } from '@/plan/sections';
import { DegradeNotes, EvidenceLink } from './evidence';
import type { AcceptanceRowStatus, EvidenceRef } from './narrative';
import type { AcceptanceModel, SectionState } from './use-run-narrative';

/*
 * Per-criterion acceptance verdict table. The row inventory is the PLAN's
 * verification.acceptance_criteria (a plan criterion with no recorded
 * result is `pending`); results come from the acceptance ARTIFACT, since
 * acceptance_outcome_recorded carries only the verdict and tallies. When
 * the artifact cannot be read the recorded verdict and tallies still render
 * from the audit payload.
 */

const statusLabel: Record<AcceptanceRowStatus, string> = {
  passed: 'pass',
  failed: 'fail',
  skipped: 'skipped',
  undecidable: 'undecidable',
  pending: 'pending',
};

const statusStyle: Record<AcceptanceRowStatus, string> = {
  passed: 'text-emerald-700 dark:text-emerald-300',
  failed: 'text-rose-700 dark:text-rose-300',
  skipped: 'text-neutral-500',
  undecidable: 'text-amber-700 dark:text-amber-300',
  pending: 'text-neutral-500 italic',
};

export function AcceptanceBlock({ section }: { section: SectionState<AcceptanceModel> }) {
  const model = section.model;
  const outcome = model?.outcome ?? null;
  // Per-row evidence: the artifact when it was read, else the outcome entry.
  const rowEvidence: EvidenceRef | null = model?.artifact ?? outcome?.evidence ?? null;
  return (
    <Section id="narrative-acceptance" title="Acceptance">
      <div className="space-y-3">
        <DegradeNotes notes={section.notes} />
        {model && (
          <>
            {outcome ? (
              <dl
                data-testid="acceptance-outcome"
                className="grid grid-cols-[10rem_1fr] gap-y-1 text-sm"
              >
                <dt className="text-neutral-500">Verdict</dt>
                <dd data-testid="acceptance-verdict" className="font-mono font-semibold">
                  {outcome.verdict}
                </dd>
                {outcome.failureMode && (
                  <>
                    <dt className="text-neutral-500">Failure mode</dt>
                    <dd className="font-mono">{outcome.failureMode}</dd>
                  </>
                )}
                <dt className="text-neutral-500">Criteria</dt>
                <dd data-testid="acceptance-tallies" className="font-mono text-xs">
                  {outcome.tallies.passed} passed · {outcome.tallies.failed} failed ·{' '}
                  {outcome.tallies.skipped} skipped · {outcome.tallies.undecidable} undecidable ·{' '}
                  {outcome.tallies.total} total
                </dd>
                <dt className="text-neutral-500">Evidence</dt>
                <dd className="flex flex-wrap gap-2">
                  <EvidenceLink refTo={outcome.evidence} />
                  {model.artifact && <EvidenceLink refTo={model.artifact} />}
                </dd>
              </dl>
            ) : (
              <p className="text-sm text-neutral-500">No acceptance outcome recorded yet.</p>
            )}
            {model.rows &&
              (model.rows.length === 0 ? (
                <p className="text-xs text-neutral-500">
                  The plan declares no acceptance criteria.
                </p>
              ) : (
                <table className="w-full text-left text-sm">
                  <thead className="text-xs text-neutral-500">
                    <tr>
                      <th className="py-1 pr-3 font-medium">Criterion</th>
                      <th className="py-1 pr-3 font-medium">Statement</th>
                      <th className="py-1 pr-3 font-medium">Verdict</th>
                      <th className="py-1 font-medium">Evidence</th>
                    </tr>
                  </thead>
                  <tbody>
                    {model.rows.map((r) => (
                      <tr
                        key={r.id}
                        data-testid="acceptance-row"
                        data-criterion={r.id}
                        data-status={r.status}
                        className="border-t border-neutral-200 align-top dark:border-neutral-800"
                      >
                        <td className="py-1 pr-3 font-mono text-xs">
                          {r.id}
                          {!r.inPlan && (
                            <span className="ml-1 text-amber-700 dark:text-amber-300">
                              (not in plan)
                            </span>
                          )}
                        </td>
                        <td className="py-1 pr-3">
                          {r.statement}
                          {r.observed && (
                            <div className="text-xs text-neutral-500">observed: {r.observed}</div>
                          )}
                          {r.undecidableReason && (
                            <div className="text-xs text-neutral-500">
                              undecidable: {r.undecidableReason}
                            </div>
                          )}
                        </td>
                        <td
                          data-testid="acceptance-row-verdict"
                          className={`py-1 pr-3 font-mono text-xs ${statusStyle[r.status]}`}
                        >
                          {statusLabel[r.status]}
                        </td>
                        <td className="py-1">
                          {r.status !== 'pending' && rowEvidence ? (
                            <EvidenceLink refTo={rowEvidence} />
                          ) : (
                            <span className="text-xs text-neutral-400">—</span>
                          )}
                        </td>
                      </tr>
                    ))}
                  </tbody>
                </table>
              ))}
          </>
        )}
      </div>
    </Section>
  );
}
