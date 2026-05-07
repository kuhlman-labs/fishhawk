import { useEffect, useState } from 'react';
import { Link, useParams } from 'react-router';
import { api } from '@/api/client';
import { useAsync } from '@/api/use-async';
import { isStandardV1Plan, type StandardV1Plan } from '@/api/plan';
import { isPullRequestArtifact, type PullRequestArtifactBody } from '@/api/pull-request';
import type { Artifact, Stage } from '@/api/types';
import { FailureBanner } from '@/components/failure-banner';
import { PlanDocument } from '@/plan/plan-document';
import { PullRequestDocument } from '@/pull-request/pr-document';

/*
 * Stage detail. Dispatches on stage.type:
 *   - plan      → PlanDocument fed the most-recent standard_v1 plan artifact
 *   - implement → PullRequestDocument fed the most-recent pull_request artifact (#205)
 *   - other     → placeholder (review and later stage types land in their own issues)
 *
 * Stage state lives in component state so the approval panel can
 * apply optimistic updates and roll them back on failure (E7.4).
 */
export function StageDetail() {
  const { runId, stageId } = useParams<{ runId: string; stageId: string }>();
  if (!runId || !stageId) {
    return <div role="alert">Missing route params.</div>;
  }

  return <StageDetailLoaded runId={runId} stageId={stageId} />;
}

function StageDetailLoaded({ runId, stageId }: { runId: string; stageId: string }) {
  const stage = useAsync(() => api.getStage(stageId), [stageId]);
  const artifacts = useAsync(() => api.listStageArtifacts(stageId), [stageId]);

  if (stage.status === 'loading' || artifacts.status === 'loading') {
    return <div className="text-sm text-neutral-500">Loading stage…</div>;
  }
  if (stage.status === 'error') {
    return <ErrorBox label="stage" error={stage.error} />;
  }
  if (artifacts.status === 'error') {
    return <ErrorBox label="artifacts" error={artifacts.error} />;
  }

  return (
    <StageDetailView runId={runId} initialStage={stage.data} artifacts={artifacts.data.items} />
  );
}

function StageDetailView({
  runId,
  initialStage,
  artifacts,
}: {
  runId: string;
  initialStage: Stage;
  artifacts: Artifact[];
}) {
  const [stage, setStage] = useState<Stage>(initialStage);

  // Re-sync if the loader returns a different stage row (e.g., the
  // user navigated to a different stage without a full route remount).
  useEffect(() => {
    setStage(initialStage);
  }, [initialStage]);

  const planArtifact = artifacts.find(
    (a) => a.kind === 'plan' && a.schema_version === 'standard_v1',
  );
  // Implement stages produce a single pull_request artifact per
  // stage; the upload handler dedups on (stage_id, content_hash) so
  // a re-uploaded identical body returns the same row. Pick the
  // most recent so a forced re-run with a different head_sha shows
  // the latest PR rather than the original.
  const prArtifact = artifacts
    .filter((a) => a.kind === 'pull_request')
    .sort((a, b) => b.created_at.localeCompare(a.created_at))[0];

  return (
    <section className="space-y-6">
      <div>
        <Link to={`/runs/${runId}`} className="text-xs text-neutral-500 hover:underline">
          ← Run
        </Link>
      </div>

      <FailureBanner stage={stage} onStageUpdate={setStage} onStageRollback={setStage} />

      {stage.type === 'plan' &&
        (planArtifact ? (
          <PlanArtifact
            artifactId={planArtifact.id}
            stage={stage}
            runId={runId}
            onStageUpdate={setStage}
            onStageRollback={setStage}
          />
        ) : (
          <p className="text-sm text-neutral-500">
            No standard_v1 plan artifact attached to this stage yet.
          </p>
        ))}

      {stage.type === 'implement' &&
        (prArtifact ? (
          <PullRequestArtifact
            artifactId={prArtifact.id}
            stage={stage}
            runId={runId}
            onStageUpdate={setStage}
            onStageRollback={setStage}
          />
        ) : (
          <p className="text-sm text-neutral-500">
            No pull-request artifact attached to this stage yet.
          </p>
        ))}

      {stage.type !== 'plan' && stage.type !== 'implement' && (
        <article className="space-y-2">
          <h1 className="font-mono text-lg font-semibold tracking-tight">Stage · {stage.type}</h1>
          <p className="text-sm text-neutral-500">
            Detail view for {stage.type} stages lands later in E7.
          </p>
        </article>
      )}
    </section>
  );
}

interface PlanArtifactProps {
  artifactId: string;
  stage: Stage;
  runId: string;
  onStageUpdate: (next: Stage) => void;
  onStageRollback: (prev: Stage) => void;
}

function PlanArtifact({
  artifactId,
  stage,
  runId,
  onStageUpdate,
  onStageRollback,
}: PlanArtifactProps) {
  const result = useAsync(() => api.getArtifact<unknown>(artifactId), [artifactId]);

  if (result.status === 'loading') {
    return <div className="text-sm text-neutral-500">Loading plan…</div>;
  }
  if (result.status === 'error') {
    return <ErrorBox label="plan artifact" error={result.error} />;
  }

  const content = result.data.content;
  if (!isStandardV1Plan(content)) {
    return (
      <div
        role="alert"
        className="rounded-md border border-amber-300 bg-amber-50 p-4 text-sm text-amber-900 dark:border-amber-900/60 dark:bg-amber-950/40 dark:text-amber-200"
      >
        <div className="font-medium">Unrecognized plan version.</div>
        <div className="mt-1 font-mono text-xs">
          schema_version={result.data.schema_version ?? 'null'} · kind={result.data.kind}
        </div>
      </div>
    );
  }

  return (
    <PlanDocument
      plan={content as StandardV1Plan}
      stage={stage}
      runId={runId}
      onStageUpdate={onStageUpdate}
      onStageRollback={onStageRollback}
    />
  );
}

interface PullRequestArtifactProps {
  artifactId: string;
  stage: Stage;
  runId: string;
  onStageUpdate: (next: Stage) => void;
  onStageRollback: (prev: Stage) => void;
}

function PullRequestArtifact({
  artifactId,
  stage,
  runId,
  onStageUpdate,
  onStageRollback,
}: PullRequestArtifactProps) {
  const result = useAsync(() => api.getArtifact<unknown>(artifactId), [artifactId]);

  if (result.status === 'loading') {
    return <div className="text-sm text-neutral-500">Loading pull request…</div>;
  }
  if (result.status === 'error') {
    return <ErrorBox label="pull-request artifact" error={result.error} />;
  }

  const content = result.data.content;
  if (!isPullRequestArtifact(content)) {
    return (
      <div
        role="alert"
        className="rounded-md border border-amber-300 bg-amber-50 p-4 text-sm text-amber-900 dark:border-amber-900/60 dark:bg-amber-950/40 dark:text-amber-200"
      >
        <div className="font-medium">Unrecognized pull-request artifact shape.</div>
        <div className="mt-1 font-mono text-xs">
          schema_version={result.data.schema_version ?? 'null'} · kind={result.data.kind}
        </div>
      </div>
    );
  }

  return (
    <PullRequestDocument
      artifact={content as PullRequestArtifactBody}
      stage={stage}
      runId={runId}
      onStageUpdate={onStageUpdate}
      onStageRollback={onStageRollback}
    />
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
