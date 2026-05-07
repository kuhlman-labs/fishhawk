import { api } from '@/api/client';
import { usePaginated } from '@/api/use-paginated';
import { AuditEntryRow } from '@/components/audit-entry-row';
import { Pagination } from '@/components/pagination';

const AUDIT_PAGE_SIZE = 50;

/*
 * The per-run audit list. Renders entries chained by the per-run
 * sequence; the chain integrity guarantees from E2 are not surfaced
 * here visually beyond exposing entry_hash (truncated) — verifying
 * integrity is the verifier CLI's job.
 *
 * Cursor pagination via usePaginated (E7.3.1 #155); the backend's
 * /v0/runs/{id}/audit endpoint already speaks limit/cursor.
 */
export function RunAuditList({ runId }: { runId: string }) {
  const { state, hasNext, hasPrev, next, prev, pageIndex } = usePaginated(
    (cursor) => api.listRunAudit(runId, { limit: AUDIT_PAGE_SIZE, cursor: cursor ?? undefined }),
    [runId],
  );

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
  if (entries.length === 0 && pageIndex === 0) {
    return <p className="text-sm text-neutral-500">No audit entries for this run yet.</p>;
  }

  return (
    <div className="space-y-3">
      <ol className="overflow-hidden rounded-md border border-neutral-200 dark:border-neutral-800">
        {entries.map((entry) => (
          <AuditEntryRow key={entry.id} entry={entry} />
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
