import { Link } from 'react-router';
import { api } from '@/api/client';
import { usePaginated } from '@/api/use-paginated';
import type { RunState } from '@/api/types';
import { Pagination } from '@/components/pagination';
import { cn } from '@/lib/cn';

const RUNS_PAGE_SIZE = 50;

const stateStyles: Record<RunState, string> = {
  pending: 'bg-neutral-200 text-neutral-700 dark:bg-neutral-800 dark:text-neutral-300',
  running: 'bg-blue-100 text-blue-800 dark:bg-blue-900/40 dark:text-blue-300',
  succeeded: 'bg-emerald-100 text-emerald-800 dark:bg-emerald-900/40 dark:text-emerald-300',
  failed: 'bg-rose-100 text-rose-800 dark:bg-rose-900/40 dark:text-rose-300',
  cancelled: 'bg-neutral-200 text-neutral-600 dark:bg-neutral-800 dark:text-neutral-400',
};

function StateBadge({ state }: { state: RunState }) {
  return (
    <span
      className={cn('inline-flex rounded-full px-2 py-0.5 font-mono text-xs', stateStyles[state])}
    >
      {state}
    </span>
  );
}

function formatTimestamp(iso: string): string {
  return new Date(iso).toLocaleString(undefined, {
    year: 'numeric',
    month: 'short',
    day: 'numeric',
    hour: '2-digit',
    minute: '2-digit',
  });
}

export function Runs() {
  const { state, hasNext, hasPrev, next, prev, pageIndex } = usePaginated(
    (cursor) => api.listRuns({ limit: RUNS_PAGE_SIZE, cursor: cursor ?? undefined }),
    [],
  );

  return (
    <section className="space-y-4">
      <header>
        <h1 className="text-xl font-semibold tracking-tight">Runs</h1>
        <p className="text-sm text-neutral-600 dark:text-neutral-400">
          Workflow runs across your repositories.
        </p>
      </header>

      {state.status === 'loading' && (
        <div className="rounded-md border border-neutral-200 p-8 text-sm text-neutral-500 dark:border-neutral-800">
          Loading runs…
        </div>
      )}

      {state.status === 'error' && (
        <div
          role="alert"
          className="rounded-md border border-rose-300 bg-rose-50 p-4 text-sm text-rose-900 dark:border-rose-900/60 dark:bg-rose-950/40 dark:text-rose-200"
        >
          <div className="font-medium">Couldn&apos;t load runs.</div>
          <div className="mt-1 font-mono text-xs">{state.error.message}</div>
        </div>
      )}

      {state.status === 'ok' && state.data.items.length === 0 && (
        <div className="rounded-md border border-dashed border-neutral-300 p-8 text-sm text-neutral-500 dark:border-neutral-700">
          No runs yet. The first one will appear here when a workflow is dispatched.
        </div>
      )}

      {state.status === 'ok' && state.data.items.length > 0 && (
        <>
          <div className="overflow-hidden rounded-md border border-neutral-200 dark:border-neutral-800">
            <table className="w-full text-sm">
              <thead className="bg-neutral-100 text-left text-xs tracking-wide text-neutral-600 uppercase dark:bg-neutral-900 dark:text-neutral-400">
                <tr>
                  <th className="px-4 py-2 font-medium">Repo</th>
                  <th className="px-4 py-2 font-medium">Workflow</th>
                  <th className="px-4 py-2 font-medium">State</th>
                  <th className="px-4 py-2 font-medium">Trigger</th>
                  <th className="px-4 py-2 font-medium">Created</th>
                </tr>
              </thead>
              <tbody className="divide-y divide-neutral-200 dark:divide-neutral-800">
                {state.data.items.map((run) => (
                  <tr key={run.id} className="hover:bg-neutral-50 dark:hover:bg-neutral-900/50">
                    <td className="px-4 py-2">
                      <Link
                        to={`/runs/${run.id}`}
                        className="font-mono text-neutral-900 underline-offset-2 hover:underline dark:text-neutral-100"
                      >
                        {run.repo}
                      </Link>
                    </td>
                    <td className="px-4 py-2 font-mono text-neutral-700 dark:text-neutral-300">
                      {run.workflow_id}
                    </td>
                    <td className="px-4 py-2">
                      <StateBadge state={run.state} />
                    </td>
                    <td className="px-4 py-2 font-mono text-xs text-neutral-600 dark:text-neutral-400">
                      {run.trigger_source}
                      {run.trigger_ref ? ` · ${run.trigger_ref}` : ''}
                    </td>
                    <td className="px-4 py-2 text-neutral-600 dark:text-neutral-400">
                      {formatTimestamp(run.created_at)}
                    </td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
          {(hasPrev || hasNext) && (
            <Pagination
              pageIndex={pageIndex}
              hasPrev={hasPrev}
              hasNext={hasNext}
              onPrev={prev}
              onNext={next}
            />
          )}
        </>
      )}
    </section>
  );
}
