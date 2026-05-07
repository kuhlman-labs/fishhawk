import { Link } from 'react-router';
import type { AuditEntry } from '@/api/types';

/*
 * Shared audit-entry row, used by both the per-run audit list
 * (RunAuditList) and the global audit-search page (#211). One
 * component so the layout stays in lockstep across surfaces.
 *
 * The `showRun` knob trades the sequence column for a deep-link to
 * the originating run — the global feed needs that lookup affordance
 * (entries from many runs land in the same page); the per-run list
 * already lives under the run URL, so the column would be redundant.
 *
 * Global-chain entries (run_id = null) render "—" in the run column.
 */

interface Props {
  entry: AuditEntry;
  showRun?: boolean;
}

export function AuditEntryRow({ entry, showRun = false }: Props) {
  return (
    <li
      className={
        showRun
          ? 'grid grid-cols-[3rem_8rem_8rem_1fr_8rem_6rem] items-center gap-3 border-b border-neutral-200 px-3 py-2 font-mono text-xs last:border-b-0 dark:border-neutral-800'
          : 'grid grid-cols-[3rem_8rem_8rem_1fr_8rem] items-center gap-3 border-b border-neutral-200 px-3 py-2 font-mono text-xs last:border-b-0 dark:border-neutral-800'
      }
    >
      <span className="text-neutral-500">#{entry.sequence}</span>
      <span className="truncate">{entry.category}</span>
      <span className="truncate text-neutral-500">
        {entry.actor_kind ?? '—'}
        {entry.actor_subject ? ` · ${entry.actor_subject}` : ''}
      </span>
      <span className="truncate text-neutral-600 dark:text-neutral-400">
        {new Date(entry.ts).toLocaleString()}
      </span>
      <span className="truncate text-neutral-400" title={entry.entry_hash}>
        {entry.entry_hash.slice(0, 12)}…
      </span>
      {showRun && (
        <span className="truncate text-neutral-500">
          {entry.run_id ? (
            <Link
              to={`/runs/${entry.run_id}#audit`}
              className="hover:text-neutral-900 hover:underline dark:hover:text-neutral-100"
              title={entry.run_id}
            >
              {entry.run_id.slice(0, 8)}…
            </Link>
          ) : (
            '—'
          )}
        </span>
      )}
    </li>
  );
}
