import { Circle } from 'lucide-react';

/*
 * Required-checks panel for the review-stage detail page (#213,
 * renamed in #256 / ADR-017).
 *
 * Renders one row per required check with a state pill, plus a
 * sub-label naming where the list comes from. Sources are the
 * `branch_protection` and/or `ruleset:<id>` entries from the run's
 * required-checks snapshot (#251) — pre-v0.2 the list sourced from
 * the spec-level `gate.blocking_checks` field, which was removed in
 * #254. The panel is informational; merge gating happens on GitHub
 * via branch protection (#253 dropped the in-Fishhawk approval gate).
 */

export type RequiredCheckState = 'pass' | 'fail' | 'pending' | 'not_tracked';

export interface RequiredCheck {
  name: string;
  state: RequiredCheckState;
}

const stateLabel: Record<RequiredCheckState, string> = {
  pass: 'pass',
  fail: 'fail',
  pending: 'pending',
  not_tracked: 'not tracked yet',
};

const statePillClass: Record<RequiredCheckState, string> = {
  pass: 'bg-emerald-100 text-emerald-800 dark:bg-emerald-900/40 dark:text-emerald-300',
  fail: 'bg-rose-100 text-rose-800 dark:bg-rose-900/40 dark:text-rose-300',
  pending: 'bg-amber-100 text-amber-800 dark:bg-amber-900/40 dark:text-amber-300',
  not_tracked: 'bg-neutral-100 text-neutral-600 dark:bg-neutral-800 dark:text-neutral-400',
};

export function RequiredChecksPanel({
  checks,
  sources,
}: {
  checks: RequiredCheck[];
  sources?: string[];
}) {
  const sourceLabel = describeSources(sources ?? []);

  if (checks.length === 0) {
    return (
      <p className="rounded-md border border-dashed border-neutral-300 p-4 text-sm text-neutral-500 dark:border-neutral-700">
        No required checks configured.
      </p>
    );
  }

  return (
    <div className="space-y-2">
      {sourceLabel && <p className="text-xs text-neutral-500">{sourceLabel}</p>}
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
    </div>
  );
}

// describeSources renders the snapshot's sources array as a
// human-readable attribution sub-label. v0 ships two recognized
// shapes from the dispatcher: `branch_protection` (classic API)
// and `ruleset:<id>` (one entry per contributing ruleset). The
// branch name isn't in the snapshot today — adding it is a v0.x
// follow-up — so the label stays generic ("branch protection")
// rather than "branch protection on `main`".
function describeSources(sources: string[]): string | null {
  if (sources.length === 0) return null;
  const hasClassic = sources.includes('branch_protection');
  const rulesetCount = sources.filter((s) => s.startsWith('ruleset:')).length;

  if (hasClassic && rulesetCount > 0) {
    const noun = rulesetCount === 1 ? 'ruleset' : 'rulesets';
    return `Required by branch protection + ${rulesetCount} ${noun}`;
  }
  if (hasClassic) {
    return 'Required by branch protection';
  }
  if (rulesetCount > 0) {
    const noun = rulesetCount === 1 ? 'ruleset' : 'rulesets';
    return `Required by ${rulesetCount} ${noun}`;
  }
  return null;
}
