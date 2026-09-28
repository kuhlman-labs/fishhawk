import type { AsyncState } from '@/api/use-async';
import type { RepoFailureMix, RepoHealth } from '@/api/types';
import { RepoPanelFrame, Stat } from './throughput-panel';

/*
 * Health rollup panel (E40.3 / #1714): plan first-shot approval rate, fixup
 * rate, acceptance pass rate and the failure-category mix. The server
 * returns 0 for every rate on a zero denominator, so each rate is shown
 * with its "n of m" so a 0% on no samples is not read as a 0% on many.
 */

function formatRate(rate: number): string {
  return `${Math.round(rate * 100)}%`;
}

/** A 0..1 ratio as a whole percentage. */
export function Rate({ value }: { value: number }) {
  return <>{formatRate(value)}</>;
}

const FAILURE_CATEGORIES: Array<keyof RepoFailureMix> = ['A', 'B', 'C', 'D'];

export function HealthPanel({ state }: { state: AsyncState<RepoHealth> }) {
  return (
    <RepoPanelFrame title="Health" state={state}>
      {(data) => (
        <div className="space-y-3">
          <dl className="grid grid-cols-2 gap-3 sm:grid-cols-3">
            <Stat
              label="Plan first-shot approval"
              value={formatRate(data.plan_first_shot_approval_rate)}
              hint={`${data.plan_first_shot_approvals} of ${data.plan_approval_samples} plans`}
            />
            <Stat
              label="Fixup rate"
              value={formatRate(data.fixup_rate)}
              hint={`${data.fixup_runs} of ${data.runs_considered} runs`}
            />
            <Stat
              label="Acceptance pass rate"
              value={formatRate(data.acceptance_pass_rate)}
              hint={`${data.acceptance_passed} of ${data.acceptance_samples} verdicts`}
            />
          </dl>
          <p className="text-xs text-neutral-600 dark:text-neutral-400">
            Acceptance: {data.acceptance_passed} passed · {data.acceptance_not_validated} not
            validated · {data.acceptance_failed} failed · {data.acceptance_undecidable} undecidable
          </p>
          <table aria-label="Failure categories" className="w-full text-sm">
            <thead className="text-left text-xs tracking-wide text-neutral-500 uppercase">
              <tr>
                <th className="py-1 font-medium">Failure category</th>
                <th className="py-1 font-medium">Stages</th>
              </tr>
            </thead>
            <tbody>
              {FAILURE_CATEGORIES.map((c) => (
                <tr key={c}>
                  <td className="py-1 font-mono text-xs">{c}</td>
                  <td className="py-1 tabular-nums">{data.failure_categories[c]}</td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}
    </RepoPanelFrame>
  );
}
