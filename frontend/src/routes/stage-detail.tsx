import { Link, useParams } from 'react-router';
import { api } from '@/api/client';
import { useAsync } from '@/api/use-async';
import { isStandardV1Plan, type StandardV1Plan } from '@/api/plan';
import type { Artifact, Stage } from '@/api/types';
import { PlanDocument } from '@/plan/plan-document';

/*
 * Stage detail. For plan stages, fetches the most-recent plan
 * artifact and renders it via PlanDocument. Other stage types get
 * a minimal placeholder for now — implement and review surfaces
 * are out of scope for E7.3.
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

  return <StageDetailView runId={runId} stage={stage.data} artifacts={artifacts.data.items} />;
}

function StageDetailView({
  runId,
  stage,
  artifacts,
}: {
  runId: string;
  stage: Stage;
  artifacts: Artifact[];
}) {
  const planArtifact = artifacts.find(
    (a) => a.kind === 'plan' && a.schema_version === 'standard_v1',
  );

  return (
    <section className="space-y-6">
      <div>
        <Link to={`/runs/${runId}`} className="text-xs text-neutral-500 hover:underline">
          ← Run
        </Link>
      </div>

      {stage.type === 'plan' && planArtifact ? (
        <PlanArtifact artifactId={planArtifact.id} />
      ) : stage.type === 'plan' ? (
        <p className="text-sm text-neutral-500">
          No standard_v1 plan artifact attached to this stage yet.
        </p>
      ) : (
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

function PlanArtifact({ artifactId }: { artifactId: string }) {
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

  return <PlanDocument plan={content as StandardV1Plan} />;
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
