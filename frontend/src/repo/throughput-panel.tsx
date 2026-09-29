import { useId, type ReactNode } from 'react';
import type { AsyncState } from '@/api/use-async';
import type { RepoThroughput, RepoWaitOnHuman } from '@/api/types';

/*
 * Repo dashboard rollup panels (E40.3 / #1714). Each panel takes its OWN
 * already-fetched AsyncState, so one failing rollup renders a panel-scoped
 * alert while its siblings keep rendering. RepoPanelFrame is the shared
 * shell — heading, loading, error and the "partial data" note a truncated
 * scan carries (approval condition 1) — reused by the economics and health
 * panels.
 */

export function RepoPanelFrame<T extends { truncated?: boolean }>({
  title,
  state,
  children,
}: {
  title: string;
  state: AsyncState<T>;
  children: (data: T) => ReactNode;
}) {
  const headingId = useId();
  return (
    <section
      aria-labelledby={headingId}
      className="space-y-3 rounded-md border border-neutral-200 p-4 dark:border-neutral-800"
    >
      <h2 id={headingId} className="text-base font-semibold tracking-tight">
        {title}
      </h2>
      {state.status === 'loading' && (
        <div className="text-sm text-neutral-500">Loading {title.toLowerCase()}…</div>
      )}
      {state.status === 'error' && (
        <div
          role="alert"
          className="rounded-md border border-rose-300 bg-rose-50 p-3 text-sm text-rose-900 dark:border-rose-900/60 dark:bg-rose-950/40 dark:text-rose-200"
        >
          <div className="font-medium">Couldn&apos;t load {title.toLowerCase()}.</div>
          <div className="mt-1 font-mono text-xs">{state.error.message}</div>
        </div>
      )}
      {state.status === 'ok' && (
        <>
          {state.data.truncated === true && <PartialDataNote />}
          {children(state.data)}
        </>
      )}
    </section>
  );
}

/** Rendered when the server's scan ceiling cut the run scan short. */
export function PartialDataNote() {
  return (
    <p className="rounded-md border border-amber-300 bg-amber-50 px-3 py-2 text-xs text-amber-900 dark:border-amber-900/60 dark:bg-amber-950/40 dark:text-amber-200">
      Partial data: the run scan hit its ceiling before covering the whole window, so these figures
      undercount.
    </p>
  );
}

export function Stat({ label, value, hint }: { label: string; value: ReactNode; hint?: string }) {
  return (
    <div>
      <dt className="text-xs font-medium tracking-wide text-neutral-500 uppercase dark:text-neutral-400">
        {label}
      </dt>
      <dd className="mt-0.5 text-lg font-semibold text-neutral-900 tabular-nums dark:text-neutral-100">
        {value}
      </dd>
      {hint && <dd className="text-xs text-neutral-500 dark:text-neutral-400">{hint}</dd>}
    </div>
  );
}

/** Coarse duration: 45s, 30m, 5h, 5h 12m, 2d 3h. */
function formatDuration(seconds: number): string {
  const s = Math.max(0, Math.round(seconds));
  if (s < 60) return `${s}s`;
  const mins = Math.floor(s / 60);
  if (mins < 60) return `${mins}m`;
  const hours = Math.floor(mins / 60);
  if (hours < 24) return mins % 60 === 0 ? `${hours}h` : `${hours}h ${mins % 60}m`;
  const days = Math.floor(hours / 24);
  return hours % 24 === 0 ? `${days}d` : `${days}d ${hours % 24}h`;
}

/** ISO week start → "Sep 28" (UTC, matching the server's bucketing). */
function formatWeek(iso: string): string {
  const d = new Date(iso);
  if (Number.isNaN(d.getTime())) return iso;
  return d.toLocaleDateString('en-US', { month: 'short', day: 'numeric', timeZone: 'UTC' });
}

export function Duration({ seconds }: { seconds: number }) {
  return <>{formatDuration(seconds)}</>;
}

export function WeekLabel({ iso }: { iso: string }) {
  return <>{formatWeek(iso)}</>;
}

function WaitOnHuman({ wait }: { wait: RepoWaitOnHuman }) {
  return (
    <div className="space-y-2 border-t border-neutral-200 pt-3 dark:border-neutral-800">
      <h3 className="text-sm font-semibold">Wait on human</h3>
      <dl className="grid grid-cols-2 gap-3 sm:grid-cols-3">
        <Stat
          label="Median total wait"
          value={formatDuration(wait.median_total_wait_seconds)}
          hint={`over ${wait.runs} run${wait.runs === 1 ? '' : 's'}`}
        />
        <Stat label="Total wait" value={formatDuration(wait.total_wait_on_human_seconds)} />
      </dl>
      {wait.gates.length > 0 && (
        <table className="w-full text-sm">
          <thead className="text-left text-xs tracking-wide text-neutral-500 uppercase">
            <tr>
              <th className="py-1 font-medium">Gate</th>
              <th className="py-1 font-medium">Samples</th>
              <th className="py-1 font-medium">Median wait</th>
            </tr>
          </thead>
          <tbody>
            {wait.gates.map((g) => (
              <tr key={g.gate}>
                <td className="py-1 font-mono text-xs">{g.gate}</td>
                <td className="py-1 tabular-nums">{g.samples}</td>
                <td className="py-1 tabular-nums">{formatDuration(g.median_wait_seconds)}</td>
              </tr>
            ))}
          </tbody>
        </table>
      )}
    </div>
  );
}

function ThroughputBody({ data }: { data: RepoThroughput }) {
  const peak = Math.max(1, ...data.weeks.map((w) => w.merged_changes));
  return (
    <div className="space-y-3">
      <dl className="grid grid-cols-2 gap-3 sm:grid-cols-3">
        <Stat label="Merged changes" value={data.merged_changes} />
        <Stat
          label="Median cycle time"
          value={data.cycle_time_samples > 0 ? formatDuration(data.median_cycle_time_seconds) : '—'}
          hint={
            `${data.cycle_time_samples} sample${data.cycle_time_samples === 1 ? '' : 's'}` +
            (data.cycle_time_excluded > 0
              ? `, ${data.cycle_time_excluded} excluded (no pr_merged row)`
              : '')
          }
        />
      </dl>
      <ol aria-label="Merged changes per week" className="flex items-end gap-1">
        {data.weeks.map((w) => (
          <li
            key={w.week_start}
            aria-label={`Week of ${formatWeek(w.week_start)}: ${w.merged_changes} merged`}
            className="flex flex-1 flex-col items-center gap-1"
          >
            <div className="flex h-20 w-full items-end">
              <div
                className="w-full rounded-sm bg-blue-500 dark:bg-blue-400"
                style={{ height: `${(w.merged_changes / peak) * 100}%` }}
              />
            </div>
            <span className="text-[10px] text-neutral-500 tabular-nums">
              {formatWeek(w.week_start)}
            </span>
          </li>
        ))}
      </ol>
      {data.wait_on_human !== undefined && <WaitOnHuman wait={data.wait_on_human} />}
    </div>
  );
}

export function ThroughputPanel({ state }: { state: AsyncState<RepoThroughput> }) {
  return (
    <RepoPanelFrame title="Throughput" state={state}>
      {(data) => <ThroughputBody data={data} />}
    </RepoPanelFrame>
  );
}
