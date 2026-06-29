import { Link, useParams } from 'react-router';
import { api } from '@/api/client';
import { useAsync } from '@/api/use-async';
import type {
  CampaignItem,
  CampaignItemState,
  CampaignNextAction,
  CampaignRollup,
  CampaignStatus,
} from '@/api/types';
import { cn } from '@/lib/cn';

export function CampaignDetail() {
  const { campaignId } = useParams<{ campaignId: string }>();
  if (!campaignId) {
    return <div role="alert">Missing campaign id.</div>;
  }
  return <CampaignDetailLoaded campaignId={campaignId} />;
}

function CampaignDetailLoaded({ campaignId }: { campaignId: string }) {
  const status = useAsync(() => api.getCampaignStatus(campaignId), [campaignId]);

  if (status.status === 'loading') {
    return <div className="text-sm text-neutral-500">Loading campaign…</div>;
  }
  if (status.status === 'error') {
    return <ErrorBox label="campaign" error={status.error} />;
  }
  return <CampaignDetailView status={status.data} />;
}

const itemStateStyles: Record<CampaignItemState, string> = {
  pending: 'bg-neutral-200 text-neutral-700 dark:bg-neutral-800 dark:text-neutral-300',
  blocked: 'bg-neutral-200 text-neutral-600 dark:bg-neutral-800 dark:text-neutral-400',
  running: 'bg-blue-100 text-blue-800 dark:bg-blue-900/40 dark:text-blue-300',
  paused: 'bg-amber-100 text-amber-800 dark:bg-amber-900/40 dark:text-amber-300',
  succeeded: 'bg-emerald-100 text-emerald-800 dark:bg-emerald-900/40 dark:text-emerald-300',
  failed: 'bg-rose-100 text-rose-800 dark:bg-rose-900/40 dark:text-rose-300',
  cancelled: 'bg-neutral-200 text-neutral-600 dark:bg-neutral-800 dark:text-neutral-400',
};

function ItemStateBadge({ state }: { state: CampaignItemState }) {
  return (
    <span
      className={cn(
        'inline-flex rounded-full px-2 py-0.5 font-mono text-xs',
        itemStateStyles[state],
      )}
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

function CampaignDetailView({ status }: { status: CampaignStatus }) {
  const { campaign, items, rollup, next_action } = status;
  const pausedItems = items.filter((it) => it.state === 'paused');

  return (
    <section className="space-y-6">
      <div>
        <Link to="/campaigns" className="text-xs text-neutral-500 hover:underline">
          ← Campaigns
        </Link>
      </div>

      <header className="space-y-2">
        <h1 className="font-mono text-lg font-semibold tracking-tight">{campaign.repo}</h1>
        <dl className="grid grid-cols-[10rem_1fr] gap-y-1 text-sm">
          <dt className="text-neutral-500">Epic</dt>
          <dd className="font-mono">{campaign.epic_ref}</dd>
          <dt className="text-neutral-500">State</dt>
          <dd className="font-mono">{campaign.state}</dd>
          <dt className="text-neutral-500">Pause policy</dt>
          <dd className="font-mono text-xs">{campaign.pause_policy}</dd>
          <dt className="text-neutral-500">Created</dt>
          <dd className="font-mono text-xs">{formatTimestamp(campaign.created_at)}</dd>
          <dt className="text-neutral-500">Updated</dt>
          <dd className="font-mono text-xs">{formatTimestamp(campaign.updated_at)}</dd>
          <dt className="text-neutral-500">Campaign ID</dt>
          <dd className="font-mono text-xs">{campaign.id}</dd>
        </dl>
      </header>

      <PendingDecision nextAction={next_action} pausedItems={pausedItems} />

      <RollupSection rollup={rollup} />

      <DependencyGraph items={items} />

      <RunGrid items={items} />
    </section>
  );
}

/**
 * The paged hand-off (E25.7) surfaced prominently: the distilled
 * next_action plus, for every paused item, the pause_reason the human must
 * act on (gate / page_event / run link). Rendered even when nothing is
 * paused so the operator always sees what to do next.
 */
function PendingDecision({
  nextAction,
  pausedItems,
}: {
  nextAction: CampaignNextAction;
  pausedItems: CampaignItem[];
}) {
  const urgent = nextAction.action === 'attention' || nextAction.action === 'resume';
  return (
    <div
      className={cn(
        'space-y-3 rounded-md border p-4',
        urgent
          ? 'border-amber-300 bg-amber-50 dark:border-amber-900/60 dark:bg-amber-950/30'
          : 'border-neutral-200 dark:border-neutral-800',
      )}
    >
      <h2 className="text-sm font-medium tracking-wide text-neutral-600 uppercase dark:text-neutral-400">
        Pending decision
      </h2>
      <div className="space-y-1 text-sm">
        <div>
          <span className="font-mono font-medium">{nextAction.action}</span>
          {nextAction.issue_ref && (
            <span className="font-mono text-neutral-600 dark:text-neutral-400">
              {' · '}
              {nextAction.issue_ref}
            </span>
          )}
        </div>
        {nextAction.detail && (
          <p className="text-neutral-600 dark:text-neutral-400">{nextAction.detail}</p>
        )}
      </div>

      {pausedItems.length > 0 && (
        <div className="space-y-2">
          <h3 className="text-xs font-medium tracking-wide text-neutral-500 uppercase">
            Paged issues
          </h3>
          <ul className="space-y-2">
            {pausedItems.map((it) => (
              <li
                key={it.id}
                className="rounded border border-amber-200 bg-white/60 p-2 text-sm dark:border-amber-900/40 dark:bg-neutral-900/40"
              >
                <div className="flex items-center gap-2">
                  <span className="font-mono">{it.issue_ref}</span>
                  <ItemStateBadge state={it.state} />
                </div>
                {it.pause_reason && (
                  <dl className="mt-1 grid grid-cols-[6rem_1fr] gap-y-0.5 font-mono text-xs text-neutral-600 dark:text-neutral-400">
                    {it.pause_reason.gate && (
                      <>
                        <dt className="text-neutral-500">gate</dt>
                        <dd>{it.pause_reason.gate}</dd>
                      </>
                    )}
                    {it.pause_reason.page_event && (
                      <>
                        <dt className="text-neutral-500">event</dt>
                        <dd>{it.pause_reason.page_event}</dd>
                      </>
                    )}
                    {it.pause_reason.run_id && (
                      <>
                        <dt className="text-neutral-500">run</dt>
                        <dd>
                          <Link
                            to={`/runs/${it.pause_reason.run_id}`}
                            className="text-blue-700 hover:underline dark:text-blue-300"
                          >
                            {it.pause_reason.run_id}
                          </Link>
                        </dd>
                      </>
                    )}
                  </dl>
                )}
              </li>
            ))}
          </ul>
        </div>
      )}
    </div>
  );
}

const ROLLUP_BUCKETS: Array<{ key: keyof CampaignRollup; label: string }> = [
  { key: 'eligible', label: 'eligible' },
  { key: 'blocked', label: 'blocked' },
  { key: 'running', label: 'running' },
  { key: 'done', label: 'done' },
  { key: 'failed', label: 'failed' },
  { key: 'cancelled', label: 'cancelled' },
  { key: 'paused', label: 'paused' },
];

function RollupSection({ rollup }: { rollup: CampaignRollup }) {
  return (
    <div className="space-y-2">
      <h2 className="text-sm font-medium tracking-wide text-neutral-600 uppercase dark:text-neutral-400">
        Rollup
      </h2>
      <dl className="flex flex-wrap gap-2">
        {ROLLUP_BUCKETS.map(({ key, label }) => (
          <div
            key={key}
            className="flex items-center gap-2 rounded-md border border-neutral-200 px-3 py-1.5 text-sm dark:border-neutral-800"
          >
            <dt className="text-neutral-500">{label}</dt>
            <dd className="font-mono font-medium" aria-label={`${label} count`}>
              {rollup[key].length}
            </dd>
          </div>
        ))}
      </dl>
    </div>
  );
}

/**
 * The dependency DAG, rendered as an insertion-ordered node list: each item
 * with the prerequisite refs it waits on (its depends_on edges). No graph
 * library — the wave-ordered item list already encodes topological order.
 */
function DependencyGraph({ items }: { items: CampaignItem[] }) {
  return (
    <div className="space-y-2">
      <h2 className="text-sm font-medium tracking-wide text-neutral-600 uppercase dark:text-neutral-400">
        Dependency graph
      </h2>
      <ul className="overflow-hidden rounded-md border border-neutral-200 dark:border-neutral-800">
        {items.length === 0 && <li className="px-4 py-3 text-sm text-neutral-500">No items.</li>}
        {items.map((it) => (
          <li
            key={it.id}
            className="flex flex-wrap items-center gap-2 border-b border-neutral-200 px-4 py-2 text-sm last:border-b-0 dark:border-neutral-800"
          >
            <span className="font-mono font-medium">{it.issue_ref}</span>
            {it.depends_on.length > 0 ? (
              <span className="font-mono text-xs text-neutral-500">
                depends on {it.depends_on.join(', ')}
              </span>
            ) : (
              <span className="text-xs text-neutral-400">no dependencies</span>
            )}
          </li>
        ))}
      </ul>
    </div>
  );
}

/**
 * Per-issue run grid: one row per item showing its state and a link through
 * to the run executing it. The campaign status payload carries run_id, not a
 * PR URL — the pull request lives on the run detail, so the run link is the
 * path to it (made explicit in the column caption). An unlinked item has no
 * run yet, stated rather than rendered as a bare dash.
 */
function RunGrid({ items }: { items: CampaignItem[] }) {
  return (
    <div className="space-y-2">
      <h2 className="text-sm font-medium tracking-wide text-neutral-600 uppercase dark:text-neutral-400">
        Per-issue runs
      </h2>
      <p className="text-xs text-neutral-500">
        Each item links through to its run, where the pull request lives.
      </p>
      <div className="overflow-hidden rounded-md border border-neutral-200 dark:border-neutral-800">
        <table className="w-full text-sm">
          <thead className="bg-neutral-100 text-left text-xs tracking-wide text-neutral-600 uppercase dark:bg-neutral-900 dark:text-neutral-400">
            <tr>
              <th className="px-4 py-2 font-medium">Issue</th>
              <th className="px-4 py-2 font-medium">State</th>
              <th className="px-4 py-2 font-medium">Run / PR</th>
            </tr>
          </thead>
          <tbody className="divide-y divide-neutral-200 dark:divide-neutral-800">
            {items.length === 0 && (
              <tr>
                <td colSpan={3} className="px-4 py-3 text-sm text-neutral-500">
                  No items.
                </td>
              </tr>
            )}
            {items.map((it) => (
              <tr key={it.id} className="hover:bg-neutral-50 dark:hover:bg-neutral-900/50">
                <td className="px-4 py-2 font-mono text-neutral-700 dark:text-neutral-300">
                  {it.issue_ref}
                </td>
                <td className="px-4 py-2">
                  <ItemStateBadge state={it.state} />
                </td>
                <td className="px-4 py-2 font-mono text-xs">
                  {it.run_id ? (
                    <Link
                      to={`/runs/${it.run_id}`}
                      className="text-blue-700 hover:underline dark:text-blue-300"
                    >
                      {it.run_id}
                    </Link>
                  ) : (
                    <span className="text-neutral-400">no run yet</span>
                  )}
                </td>
              </tr>
            ))}
          </tbody>
        </table>
      </div>
    </div>
  );
}

function ErrorBox({ label, error }: { label: string; error: Error }) {
  return (
    <div
      role="alert"
      className="rounded-md border border-rose-300 bg-rose-50 p-4 text-sm text-rose-900 dark:border-rose-900/60 dark:bg-rose-950/40 dark:text-rose-200"
    >
      <div className="font-medium">Couldn&apos;t load {label}.</div>
      <div className="mt-1 font-mono text-xs">{error.message}</div>
    </div>
  );
}
