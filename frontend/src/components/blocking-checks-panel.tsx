import { Circle } from 'lucide-react';

/*
 * Blocking-checks panel for the review-stage detail page (#213).
 *
 * Renders one row per required check with a state pill. The
 * declared list now comes from the run's branch-protection
 * snapshot (#251 / #254 / ADR-017) — pre-v0.2 it sourced from the
 * spec-level `gate.blocking_checks` field, which has been removed.
 * The panel is informational; merge gating happens on GitHub via
 * branch protection (#253 dropped the in-Fishhawk approval gate).
 */

export type BlockingCheckState = 'pass' | 'fail' | 'pending' | 'not_tracked';

export interface BlockingCheck {
  name: string;
  state: BlockingCheckState;
}

const stateLabel: Record<BlockingCheckState, string> = {
  pass: 'pass',
  fail: 'fail',
  pending: 'pending',
  not_tracked: 'not tracked yet',
};

const statePillClass: Record<BlockingCheckState, string> = {
  pass: 'bg-emerald-100 text-emerald-800 dark:bg-emerald-900/40 dark:text-emerald-300',
  fail: 'bg-rose-100 text-rose-800 dark:bg-rose-900/40 dark:text-rose-300',
  pending: 'bg-amber-100 text-amber-800 dark:bg-amber-900/40 dark:text-amber-300',
  not_tracked: 'bg-neutral-100 text-neutral-600 dark:bg-neutral-800 dark:text-neutral-400',
};

export function BlockingChecksPanel({ checks }: { checks: BlockingCheck[] }) {
  if (checks.length === 0) {
    return (
      <p className="rounded-md border border-dashed border-neutral-300 p-4 text-sm text-neutral-500 dark:border-neutral-700">
        This gate has no blocking checks declared.
      </p>
    );
  }

  return (
    <ul className="overflow-hidden rounded-md border border-neutral-200 dark:border-neutral-800">
      {checks.map((c) => (
        <li
          key={c.name}
          className="flex items-center gap-3 border-b border-neutral-200 px-3 py-2 last:border-b-0 dark:border-neutral-800"
        >
          <Circle className="size-3.5 text-neutral-400" aria-hidden />
          <span className="font-mono text-sm">{c.name}</span>
          <span
            className={`ml-auto rounded-full px-2 py-0.5 font-mono text-xs ${statePillClass[c.state]}`}
            title={
              c.state === 'not_tracked'
                ? 'Backend ingestion of check states lands in a follow-up issue.'
                : undefined
            }
          >
            {stateLabel[c.state]}
          </span>
        </li>
      ))}
    </ul>
  );
}
