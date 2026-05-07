import { useState } from 'react';
import { api } from '@/api/client';
import type { PaginatedHandle } from '@/api/use-paginated';
import { usePaginated } from '@/api/use-paginated';
import type { AuditEntry } from '@/api/types';
import { AuditEntryRow } from '@/components/audit-entry-row';
import { Pagination } from '@/components/pagination';

const AUDIT_PAGE_SIZE = 50;

/*
 * Categories the SPA exposes in the dropdown. Mirrors the audit
 * categories the backend writes today (`audit.AppendChained` /
 * `audit.AppendGlobalChained` callers across `backend/internal/server/`,
 * `backend/internal/sla`, etc.). Keep in sync — the backend doesn't
 * gate on this list (any string the user types in `?category=`
 * works), so a missing category here only means "operators can't
 * pick it from the dropdown" and is recoverable.
 *
 * Brand voice: bare nouns, no marketing adjectives. The labels are
 * the underlying category strings — engineers reading the audit
 * log are the audience and that's what's in the data.
 */
const CATEGORY_OPTIONS: { value: string; label: string }[] = [
  { value: '', label: 'All categories' },
  { value: 'run_created', label: 'run_created' },
  { value: 'stage_dispatched', label: 'stage_dispatched' },
  { value: 'stage_succeeded', label: 'stage_succeeded' },
  { value: 'stage_failed', label: 'stage_failed' },
  { value: 'stage_retried', label: 'stage_retried' },
  { value: 'plan_generated', label: 'plan_generated' },
  { value: 'pull_request_opened', label: 'pull_request_opened' },
  { value: 'trace_uploaded', label: 'trace_uploaded' },
  { value: 'approval_granted', label: 'approval_granted' },
  { value: 'approval_rejected', label: 'approval_rejected' },
  { value: 'installation_token_issued', label: 'installation_token_issued' },
  { value: 'api_token_issued', label: 'api_token_issued' },
  { value: 'api_token_revoked', label: 'api_token_revoked' },
  { value: 'oauth_signin', label: 'oauth_signin' },
  { value: 'webhook_received', label: 'webhook_received' },
];

/*
 * Audit-log search surface (#211). Brand Foundations §6 names this
 * a first-class queryable page; the per-run audit list at
 * /runs/:id#audit gives one slice, this gives the cross-run feed.
 */
export function Audit() {
  const [category, setCategory] = useState<string>('');

  const handle = usePaginated<AuditEntry>(
    (cursor) =>
      api.listGlobalAudit({
        limit: AUDIT_PAGE_SIZE,
        cursor: cursor ?? undefined,
        category: category || undefined,
      }),
    [category],
  );

  return (
    <section className="space-y-4">
      <header>
        <h1 className="text-xl font-semibold tracking-tight">Audit log</h1>
        <p className="text-sm text-neutral-600 dark:text-neutral-400">
          Append-only, signed record of every approval and run transition.
        </p>
      </header>

      <div className="flex items-center gap-3">
        <label className="flex items-center gap-2 text-xs text-neutral-600 dark:text-neutral-400">
          <span className="tracking-wide uppercase">Category</span>
          <select
            value={category}
            onChange={(e) => setCategory(e.target.value)}
            className="rounded-md border border-neutral-300 bg-white px-2 py-1 font-mono text-xs dark:border-neutral-700 dark:bg-neutral-950"
            aria-label="Filter audit entries by category"
          >
            {CATEGORY_OPTIONS.map((opt) => (
              <option key={opt.value} value={opt.value}>
                {opt.label}
              </option>
            ))}
          </select>
        </label>
      </div>

      <AuditFeed {...handle} />
    </section>
  );
}

function AuditFeed({
  state,
  hasNext,
  hasPrev,
  next,
  prev,
  pageIndex,
}: PaginatedHandle<AuditEntry>) {
  if (state.status === 'loading') {
    return <p className="text-sm text-neutral-500">Loading audit log…</p>;
  }
  if (state.status === 'error') {
    return (
      <div
        role="alert"
        className="rounded-md border border-rose-300 bg-rose-50 p-4 text-sm text-rose-900 dark:border-rose-900/60 dark:bg-rose-950/40 dark:text-rose-200"
      >
        <div className="font-medium">Couldn&apos;t load audit log.</div>
        <div className="mt-1 font-mono text-xs">{state.error.message}</div>
      </div>
    );
  }

  const entries = state.data.items;
  if (entries.length === 0) {
    return (
      <p className="rounded-md border border-dashed border-neutral-300 p-8 text-sm text-neutral-500 dark:border-neutral-700">
        No audit entries match these filters.
      </p>
    );
  }

  return (
    <div className="space-y-3">
      <ol className="overflow-hidden rounded-md border border-neutral-200 dark:border-neutral-800">
        {entries.map((entry) => (
          <AuditEntryRow key={entry.id} entry={entry} showRun />
        ))}
      </ol>
      {(hasPrev || hasNext) && (
        <Pagination
          pageIndex={pageIndex}
          hasPrev={hasPrev}
          hasNext={hasNext}
          onPrev={prev}
          onNext={next}
        />
      )}
    </div>
  );
}
