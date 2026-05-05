import { Link, useParams } from 'react-router';
import { api } from '@/api/client';
import { useAsync } from '@/api/use-async';
import type { Run, Stage, StageState } from '@/api/types';
import { cn } from '@/lib/cn';
import { RunAuditList } from './audit-list';

const stageStateStyles: Record<StageState, string> = {
  pending: 'text-neutral-500',
  dispatched: 'text-blue-700 dark:text-blue-300',
  running: 'text-blue-700 dark:text-blue-300',
  awaiting_approval: 'text-amber-700 dark:text-amber-300',
  succeeded: 'text-emerald-700 dark:text-emerald-300',
  failed: 'text-rose-700 dark:text-rose-300',
  cancelled: 'text-neutral-500',
};

export function RunDetail() {
  const { runId } = useParams<{ runId: string }>();
  if (!runId) {
    return <div role="alert">Missing run id.</div>;
  }

  return <RunDetailLoaded runId={runId} />;
}

function RunDetailLoaded({ runId }: { runId: string }) {
  const run = useAsync(() => api.getRun(runId), [runId]);
  const stages = useAsync(() => api.listRunStages(runId), [runId]);

  if (run.status === 'loading' || stages.status === 'loading') {
    return <div className="text-sm text-neutral-500">Loading run…</div>;
  }
  if (run.status === 'error') {
    return <ErrorBox label="run" error={run.error} />;
  }
  if (stages.status === 'error') {
    return <ErrorBox label="stages" error={stages.error} />;
  }

  return <RunDetailView run={run.data} stages={stages.data.items} />;
}

function RunDetailView({ run, stages }: { run: Run; stages: Stage[] }) {
  return (
    <section className="space-y-6">
      <div>
        <Link to="/runs" className="text-xs text-neutral-500 hover:underline">
          ← Runs
        </Link>
      </div>

      <header className="space-y-2">
        <h1 className="font-mono text-lg font-semibold tracking-tight">{run.repo}</h1>
        <dl className="grid grid-cols-[10rem_1fr] gap-y-1 text-sm">
          <dt className="text-neutral-500">Workflow</dt>
          <dd className="font-mono">{run.workflow_id}</dd>
          <dt className="text-neutral-500">State</dt>
          <dd className="font-mono">{run.state}</dd>
          <dt className="text-neutral-500">Trigger</dt>
          <dd className="font-mono text-xs">
            {run.trigger_source}
            {run.trigger_ref ? ` · ${run.trigger_ref}` : ''}
          </dd>
          <dt className="text-neutral-500">SHA</dt>
          <dd className="font-mono text-xs">{run.workflow_sha}</dd>
          <dt className="text-neutral-500">Run ID</dt>
          <dd className="font-mono text-xs">{run.id}</dd>
        </dl>
      </header>

      <div className="space-y-2">
        <h2 className="text-sm font-medium tracking-wide text-neutral-600 uppercase dark:text-neutral-400">
          Stages
        </h2>
        <ol className="overflow-hidden rounded-md border border-neutral-200 dark:border-neutral-800">
          {stages.length === 0 && (
            <li className="px-4 py-3 text-sm text-neutral-500">No stages yet.</li>
          )}
          {stages.map((stage) => (
            <li
              key={stage.id}
              className="flex items-center gap-4 border-b border-neutral-200 px-4 py-3 last:border-b-0 hover:bg-neutral-50 dark:border-neutral-800 dark:hover:bg-neutral-900/50"
            >
              <span className="font-mono text-xs text-neutral-500">#{stage.sequence}</span>
              <Link
                to={`/runs/${run.id}/stages/${stage.id}`}
                className="font-mono text-sm font-medium hover:underline"
              >
                {stage.type}
              </Link>
              <span className="font-mono text-xs text-neutral-500">
                {stage.executor.kind}:{stage.executor.ref}
              </span>
              <span className={cn('ml-auto font-mono text-xs', stageStateStyles[stage.state])}>
                {stage.state}
              </span>
            </li>
          ))}
        </ol>
      </div>

      <div id="audit" className="scroll-mt-8 space-y-2">
        <h2 className="text-sm font-medium tracking-wide text-neutral-600 uppercase dark:text-neutral-400">
          Audit log
        </h2>
        <RunAuditList runId={run.id} />
      </div>
    </section>
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
