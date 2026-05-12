import { Link } from 'react-router';
import { GitBranch } from 'lucide-react';
import { api } from '@/api/client';
import { useAsync } from '@/api/use-async';
import type { Run } from '@/api/types';

/*
 * Related-runs section (#216). Lists every other run that touched
 * the same PR or fired off the same trigger as the current run.
 *
 * Two grouping keys, in priority order:
 *   - pull_request_url — set after the implement stage produces a
 *     pull_request artifact. Once any run on the trigger has reached
 *     that point, "every run on this PR" is a single equality
 *     query and is the most-useful grouping.
 *   - trigger_ref + repo — used until a PR exists. Threads the
 *     follow-ups on the same issue together (e.g. multiple
 *     /fishhawk run comments).
 *
 * The component picks one — pull_request_url when available, else
 * trigger_ref — to avoid showing two overlapping lists. Both
 * backends are equality predicates with partial indexes so the
 * fetch is cheap.
 */

export function RelatedRunsSection({ run }: { run: Run }) {
  // Decide the filter once. The hook's deps array is keyed off the
  // chosen filter so changing it across renders re-fetches cleanly.
  const filterKey =
    run.pull_request_url != null
      ? `pr:${run.pull_request_url}`
      : run.trigger_ref != null
        ? `trigger:${run.repo}:${run.trigger_ref}`
        : null;

  const result = useAsync(() => {
    if (run.pull_request_url) {
      return api.listRuns({ pullRequestURL: run.pull_request_url, limit: 50 });
    }
    if (run.trigger_ref) {
      return api.listRuns({ repo: run.repo, triggerRef: run.trigger_ref, limit: 50 });
    }
    return Promise.resolve({ items: [], next_cursor: null });
  }, [filterKey]);

  // Nothing to show when the run has no groupable key (rare —
  // CLI ad-hoc runs without a trigger_ref). Hide the section
  // entirely rather than render a confusing empty panel.
  if (!filterKey) return null;

  if (result.status === 'loading') {
    return (
      <RelatedRunsShell groupedBy={groupingLabel(run)}>
        <p className="text-sm text-neutral-500">Loading related runs…</p>
      </RelatedRunsShell>
    );
  }
  if (result.status === 'error') {
    return (
      <RelatedRunsShell groupedBy={groupingLabel(run)}>
        <div
          role="alert"
          className="rounded-md border border-rose-300 bg-rose-50 p-3 font-mono text-xs text-rose-900 dark:border-rose-900/60 dark:bg-rose-950/40 dark:text-rose-200"
        >
          Couldn&apos;t load related runs: {result.error.message}
        </div>
      </RelatedRunsShell>
    );
  }

  // Drop the current run from the list — it's redundant on its own
  // detail page and would otherwise look like a self-loop.
  const others = result.data.items.filter((r) => r.id !== run.id);
  if (others.length === 0) {
    return (
      <RelatedRunsShell groupedBy={groupingLabel(run)}>
        <p className="text-sm text-neutral-500">
          No other runs share this {run.pull_request_url ? 'PR' : 'trigger'}.
        </p>
      </RelatedRunsShell>
    );
  }

  return (
    <RelatedRunsShell groupedBy={groupingLabel(run)}>
      <ul className="overflow-hidden rounded-md border border-neutral-200 dark:border-neutral-800">
        {others.map((r) => (
          <RelatedRunRow key={r.id} run={r} />
        ))}
      </ul>
    </RelatedRunsShell>
  );
}

function RelatedRunsShell({
  groupedBy,
  children,
}: {
  groupedBy: string;
  children: React.ReactNode;
}) {
  return (
    <section id="related-runs" className="scroll-mt-8 space-y-2">
      <div className="flex items-baseline justify-between">
        <h2 className="text-sm font-medium tracking-wide text-neutral-600 uppercase dark:text-neutral-400">
          Related runs
        </h2>
        <span className="font-mono text-xs text-neutral-500">{groupedBy}</span>
      </div>
      {children}
    </section>
  );
}

function RelatedRunRow({ run }: { run: Run }) {
  return (
    <li className="border-b border-neutral-200 last:border-b-0 dark:border-neutral-800">
      <Link
        to={`/runs/${run.id}`}
        className="flex items-center gap-3 px-3 py-2 hover:bg-neutral-50 focus-visible:bg-neutral-50 focus-visible:ring-1 focus-visible:ring-neutral-400 focus-visible:outline-none dark:hover:bg-neutral-900/50 dark:focus-visible:bg-neutral-900/50"
      >
        <span className="font-mono text-xs text-neutral-500" title={run.id}>
          {run.id.slice(0, 8)}…
        </span>
        <span className="font-mono text-sm">{run.workflow_id}</span>
        <span className="font-mono text-xs text-neutral-500">
          {run.trigger_source}
          {run.trigger_ref ? ` · ${run.trigger_ref}` : ''}
        </span>
        {run.retry_attempt > 0 && (
          <span className="rounded bg-amber-100 px-1.5 py-0.5 font-mono text-xs text-amber-800 dark:bg-amber-900/40 dark:text-amber-300">
            Retry #{run.retry_attempt}
          </span>
        )}
        <span className="ml-auto font-mono text-xs">{run.state}</span>
      </Link>
    </li>
  );
}

function groupingLabel(run: Run): string {
  if (run.pull_request_url) {
    // Render the PR slug rather than the full URL — the side-by-
    // side header already says which repo we're in. Strip the
    // protocol + host so "github.com/x/y/pull/42" → "pull/42".
    try {
      const u = new URL(run.pull_request_url);
      return u.pathname.replace(/^\/[^/]+\/[^/]+\//, '');
    } catch {
      return run.pull_request_url;
    }
  }
  if (run.trigger_ref) {
    return run.trigger_ref;
  }
  return '';
}

// RetryBadge renders "Retry N/M" on the run-detail header when the
// run is part of a CI-failure auto-retry chain (#279 / #280). When
// the attempt has reached the cap, the tone shifts to amber and
// the tooltip names the terminal state so reviewers know no
// further auto-dispatches will fire.
export function RetryBadge({
  attempt,
  max,
  parentRunID,
}: {
  attempt: number;
  max: number;
  parentRunID: string | null;
}) {
  const exhausted = max > 0 && attempt >= max;
  // Two tones: amber when capped (warning — no more auto-retries),
  // neutral otherwise. The badge sits next to the page title so we
  // keep it small and monospace to match the surrounding header.
  const tone = exhausted
    ? 'border-amber-300 bg-amber-100 text-amber-900 dark:border-amber-900/60 dark:bg-amber-900/40 dark:text-amber-200'
    : 'border-neutral-300 bg-neutral-100 text-neutral-700 dark:border-neutral-700 dark:bg-neutral-800 dark:text-neutral-200';
  const tooltip = exhausted
    ? 'Last retry — no further auto-dispatches.'
    : parentRunID
      ? `Re-dispatched after CI failure on ${parentRunID.slice(0, 8)}…`
      : 'Re-dispatched after CI failure.';
  return (
    <span className={`rounded border px-1.5 py-0.5 font-mono text-xs ${tone}`} title={tooltip}>
      Retry {attempt}/{max}
    </span>
  );
}

// FollowUpLink is a small inline affordance for the run-detail
// header: when parent_run_id is set, it points at the predecessor
// so a reviewer can step backwards in the lineage in one click.
export function FollowUpLink({ parentRunID }: { parentRunID: string }) {
  return (
    <span className="inline-flex items-center gap-1 font-mono text-xs text-neutral-500">
      <GitBranch className="size-3.5" aria-hidden />
      Follow-up to{' '}
      <Link
        to={`/runs/${parentRunID}`}
        className="text-blue-700 hover:underline dark:text-blue-300"
        title={parentRunID}
      >
        {parentRunID.slice(0, 8)}…
      </Link>
    </span>
  );
}
