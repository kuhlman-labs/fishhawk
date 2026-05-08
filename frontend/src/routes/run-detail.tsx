import { Link, useParams } from 'react-router';
import { ChevronRight } from 'lucide-react';
import { api } from '@/api/client';
import { useAsync } from '@/api/use-async';
import { describeFailure } from '@/api/types';
import type { Run, Stage } from '@/api/types';
import { StageStateBadge } from '@/components/stage-state-badge';
import { FollowUpLink, RelatedRunsSection } from '@/runs/related-runs';
import { RunAuditList } from './audit-list';

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
        {run.parent_run_id && <FollowUpLink parentRunID={run.parent_run_id} />}
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
          {run.pull_request_url && (
            <>
              <dt className="text-neutral-500">Pull request</dt>
              <dd className="font-mono text-xs">
                <a
                  href={run.pull_request_url}
                  rel="noopener noreferrer"
                  target="_blank"
                  className="text-blue-700 hover:underline dark:text-blue-300"
                >
                  {run.pull_request_url}
                </a>
              </dd>
            </>
          )}
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
              className="border-b border-neutral-200 last:border-b-0 dark:border-neutral-800"
            >
              <Link
                to={`/runs/${run.id}/stages/${stage.id}`}
                aria-label={`Review ${stage.type} stage`}
                className="flex items-center gap-4 px-4 py-3 hover:bg-neutral-50 focus-visible:bg-neutral-50 focus-visible:ring-1 focus-visible:ring-neutral-400 focus-visible:outline-none dark:hover:bg-neutral-900/50 dark:focus-visible:bg-neutral-900/50"
              >
                <span className="font-mono text-xs text-neutral-500">#{stage.sequence}</span>
                <span className="font-mono text-sm font-medium">{stage.type}</span>
                <span className="font-mono text-xs text-neutral-500">
                  {stage.executor.kind}:{stage.executor.ref}
                </span>
                <span className="ml-auto flex items-center gap-2 font-mono text-xs">
                  {stage.state === 'failed' && stage.failure_category && (
                    <span
                      className="rounded bg-rose-100 px-1.5 py-0.5 text-rose-800 dark:bg-rose-900/40 dark:text-rose-300"
                      title={describeFailure(stage.failure_category) ?? undefined}
                    >
                      {stage.failure_category}
                    </span>
                  )}
                  {stage.state === 'awaiting_approval' && (
                    <span className="text-amber-700 dark:text-amber-300">Review →</span>
                  )}
                  <StageStateBadge state={stage.state} />
                </span>
                <ChevronRight className="size-4 text-neutral-400" aria-hidden />
              </Link>
            </li>
          ))}
        </ol>
      </div>

      <RelatedRunsSection run={run} />

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
