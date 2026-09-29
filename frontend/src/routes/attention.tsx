import { useCallback, useState } from 'react';
import { api } from '@/api/client';
import type { AttentionItem, AttentionList } from '@/api/types';
import { useAsync } from '@/api/use-async';
import { AttentionItemCard } from '@/attention/attention-item';

/*
 * The "Needs You" attention queue (E40.1 / #1713; write surface E40.2 /
 * #1717) — the SPA index route. One ranked list of every decision parked on a
 * human across runs and campaigns, from GET /v0/attention. Each card links to
 * its detail page AND (for a kind with a decision endpoint) expands an inline
 * decision panel; a successful decision removes the item from the live list
 * here, client-side, with no refetch or reload.
 *
 * Completeness is never silent: `truncated: true` or a non-empty
 * `degraded` renders an "incomplete" banner, INCLUDING when `items` is
 * empty — an incomplete scan must never read as "nothing needs you".
 */

export const ATTENTION_EMPTY_TEXT = 'Nothing needs you right now.';
export const ATTENTION_INCOMPLETE_TEXT = 'This list is incomplete.';

function isAttentionIncomplete(list: AttentionList): boolean {
  return list.truncated || list.degraded.length > 0;
}

function IncompleteBanner({ list }: { list: AttentionList }) {
  return (
    <div
      role="status"
      className="rounded-md border border-amber-300 bg-amber-50 p-4 text-sm text-amber-900 dark:border-amber-900/60 dark:bg-amber-950/40 dark:text-amber-200"
    >
      <div className="font-medium">{ATTENTION_INCOMPLETE_TEXT}</div>
      <p className="mt-1">
        Some parked decisions may be missing — check the runs and campaigns lists before assuming
        the queue is clear.
      </p>
      <ul className="mt-2 space-y-0.5 font-mono text-xs">
        {list.truncated && <li>truncated: the candidate scan or the item limit cut the list</li>}
        {list.degraded.map((d, i) => (
          <li key={i}>
            {d.reason}
            {d.run_id ? ` · run ${d.run_id}` : ''}
            {d.campaign_id ? ` · campaign ${d.campaign_id}` : ''}
            {d.detail ? ` — ${d.detail}` : ''}
          </li>
        ))}
      </ul>
    </div>
  );
}

export function Attention() {
  const state = useAsync(() => api.listAttention(), []);

  /*
   * Items resolved in-session leave the live list without a refetch or reload
   * (E40.2 / #1717). Keyed on item.id ALONE — the stable subject identity, so
   * two same-kind items resolve independently — and applied as a client-side
   * filter over the loaded list; the loaded AttentionList is never mutated.
   */
  const [resolved, setResolved] = useState<Set<string>>(new Set());
  const onResolved = useCallback((item: AttentionItem) => {
    setResolved((prev) => {
      const next = new Set(prev);
      next.add(item.id);
      return next;
    });
  }, []);

  const visibleItems =
    state.status === 'ok' ? state.data.items.filter((i) => !resolved.has(i.id)) : [];

  return (
    <section className="space-y-4">
      <header>
        <h1 className="text-xl font-semibold tracking-tight">Needs You</h1>
        <p className="text-sm text-neutral-600 dark:text-neutral-400">
          Decisions parked on a human across runs and campaigns, most urgent first.
        </p>
      </header>

      {state.status === 'loading' && (
        <div className="rounded-md border border-neutral-200 p-8 text-sm text-neutral-500 dark:border-neutral-800">
          Loading the attention queue…
        </div>
      )}

      {state.status === 'error' && (
        <div
          role="alert"
          className="rounded-md border border-rose-300 bg-rose-50 p-4 text-sm text-rose-900 dark:border-rose-900/60 dark:bg-rose-950/40 dark:text-rose-200"
        >
          <div className="font-medium">Couldn&apos;t load the attention queue.</div>
          <div className="mt-1 font-mono text-xs">{state.error.message}</div>
        </div>
      )}

      {state.status === 'ok' && isAttentionIncomplete(state.data) && (
        <IncompleteBanner list={state.data} />
      )}

      {state.status === 'ok' && visibleItems.length === 0 && (
        <div className="rounded-md border border-dashed border-neutral-300 p-8 text-sm text-neutral-500 dark:border-neutral-700">
          {isAttentionIncomplete(state.data)
            ? 'No parked decisions were found in the part of the queue that could be read.'
            : ATTENTION_EMPTY_TEXT}
        </div>
      )}

      {state.status === 'ok' && visibleItems.length > 0 && (
        <ol className="space-y-3">
          {visibleItems.map((item) => (
            <li key={item.id}>
              <AttentionItemCard item={item} onResolved={onResolved} />
            </li>
          ))}
        </ol>
      )}
    </section>
  );
}
