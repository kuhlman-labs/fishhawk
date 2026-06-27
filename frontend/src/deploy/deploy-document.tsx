import { useState } from 'react';
import { Link } from 'react-router';
import { Check, ExternalLink, FileClock, X } from 'lucide-react';
import { Button } from '@/components/ui/button';
import { api, ApiClientError } from '@/api/client';
import type {
  DeploymentArtifactBody,
  DeploymentOutcome,
  DeploymentRollbackAction,
} from '@/api/deployment';
import type { ApprovalDecision, Stage } from '@/api/types';
import { Section } from '@/plan/sections';
import { StageStateBadge } from '@/components/stage-state-badge';

/*
 * Deploy-stage detail (E23.9 / #1389). Surfaces the delegating-deploy
 * lifecycle: a pre-execution approval gate (awaiting_deploy_approval),
 * the in-flight state while the external pipeline runs
 * (awaiting_deployment), and the `deployment` artifact reporting the
 * terminal outcome (ADR-038 / #1384).
 *
 * Composition:
 *   - Header: stage state badge + "View audit log" link, plus the
 *     pre-execution DeployApprovalPanel when the stage is parked at
 *     the gate (mirrors the plan/implement header).
 *   - Deployment artifact panel: environment, deployed ref, the
 *     external pipeline run link, the outcome badge, and the rollback
 *     handle/action when present. Rendered eagerly with artifact=null
 *     before the deploy executes (an inline note stands in).
 *
 * DeployApprovalPanel is deliberately a separate component from
 * plan/approval-panel.tsx rather than a generalization: the deploy
 * approve transition is awaiting_deploy_approval → awaiting_deployment
 * (NOT directly terminal), so its optimistic target differs. Keeping
 * it separate avoids regressing the well-tested plan/implement path.
 */

interface Props {
  artifact: DeploymentArtifactBody | null;
  stage: Stage;
  runId: string;
  onStageUpdate: (next: Stage) => void;
  onStageRollback: (prev: Stage) => void;
}

export function DeployDocument({ artifact, stage, runId, onStageUpdate, onStageRollback }: Props) {
  return (
    <article className="max-w-3xl space-y-8 pb-20">
      <header className="flex items-start justify-between gap-4">
        <div>
          <p className="text-xs tracking-wide text-neutral-500 uppercase">Deploy · stage</p>
          <h1 className="mt-1 text-2xl font-semibold tracking-tight">Deploy stage</h1>
          <div className="mt-2 flex items-center gap-3">
            <StageStateBadge state={stage.state} />
            <Link to={`/runs/${runId}#audit`} className="text-xs text-neutral-500 hover:underline">
              View audit log
            </Link>
          </div>
        </div>
        <DeployApprovalPanel
          stage={stage}
          runId={runId}
          onUpdate={onStageUpdate}
          onRollback={onStageRollback}
        />
      </header>

      <Section id="deployment" title="Deployment">
        {artifact ? (
          <DeploymentPanel artifact={artifact} />
        ) : (
          <p className="text-sm text-neutral-500">
            No deployment artifact yet — it lands once the delegated pipeline reports an outcome.
          </p>
        )}
      </Section>
    </article>
  );
}

/* -- Deployment artifact panel --------------------------------------- */

function DeploymentPanel({ artifact }: { artifact: DeploymentArtifactBody }) {
  return (
    <dl className="grid grid-cols-[10rem_1fr] gap-x-4 gap-y-3 text-sm">
      <dt className="text-neutral-500">Environment</dt>
      <dd className="font-medium">{artifact.environment}</dd>

      <dt className="text-neutral-500">Deployed ref</dt>
      <dd className="font-mono text-xs break-all">{artifact.ref}</dd>

      <dt className="text-neutral-500">Outcome</dt>
      <dd>
        <OutcomeBadge outcome={artifact.outcome} />
      </dd>

      <dt className="text-neutral-500">Pipeline run</dt>
      <dd>
        <a
          href={artifact.external_run_url}
          target="_blank"
          rel="noopener noreferrer"
          className="inline-flex items-center gap-1 text-blue-700 hover:underline dark:text-blue-300"
        >
          <ExternalLink className="size-3.5" aria-hidden />
          View external run
        </a>
      </dd>

      {artifact.rollback_handle && (
        <>
          <dt className="text-neutral-500">Rollback handle</dt>
          <dd className="font-mono text-xs break-all">{artifact.rollback_handle}</dd>
        </>
      )}

      {artifact.rollback_action && (
        <>
          <dt className="text-neutral-500">Rollback action</dt>
          <dd>
            <RollbackActionBadge action={artifact.rollback_action} />
          </dd>
        </>
      )}
    </dl>
  );
}

const OUTCOME_STYLES: Record<DeploymentOutcome, string> = {
  succeeded: 'text-emerald-700 dark:text-emerald-300',
  failed: 'text-rose-700 dark:text-rose-300',
  partial: 'text-amber-700 dark:text-amber-300',
  rolled_back: 'text-amber-700 dark:text-amber-300',
};

function OutcomeBadge({ outcome }: { outcome: DeploymentOutcome }) {
  // data-testid disambiguates the outcome label from the stage-state
  // badge, which can carry the same literal string (e.g. "succeeded").
  return (
    <span data-testid="deploy-outcome" className={`font-mono text-xs ${OUTCOME_STYLES[outcome]}`}>
      {outcome}
    </span>
  );
}

function RollbackActionBadge({ action }: { action: DeploymentRollbackAction }) {
  return <span className="font-mono text-xs text-neutral-600 dark:text-neutral-300">{action}</span>;
}

/* -- Pre-execution approval gate ------------------------------------- */

/*
 * Two-step approval surface for the deploy stage's pre-execution gate,
 * mirroring plan/approval-panel.tsx's idle → confirming → submitting
 * flow. The one structural difference: the optimistic approve target
 * is awaiting_deployment (the in-flight state), not succeeded — the
 * external pipeline still has to run after approval. Reject → failed
 * category D, same as plan/implement.
 */

interface PanelProps {
  stage: Stage;
  runId: string;
  onUpdate: (next: Stage) => void;
  onRollback: (prev: Stage) => void;
}

type Phase =
  | { kind: 'idle' }
  | { kind: 'confirming'; decision: ApprovalDecision }
  | { kind: 'submitting' }
  | { kind: 'errored'; message: string };

function optimisticUpdate(stage: Stage, decision: ApprovalDecision): Stage {
  // approve → awaiting_deployment (the post-approval in-flight state,
  // NOT terminal — the delegated pipeline runs next); reject → failed
  // (category D, the "approval timeout / rejection" tag). The
  // server-returned Stage replaces this on success.
  if (decision === 'approve') {
    return {
      ...stage,
      state: 'awaiting_deployment',
      failure_category: null,
      failure_reason: null,
    };
  }
  return {
    ...stage,
    state: 'failed',
    failure_category: 'D',
    failure_reason: 'gate rejected',
  };
}

function DeployApprovalPanel({ stage, runId, onUpdate, onRollback }: PanelProps) {
  const [phase, setPhase] = useState<Phase>({ kind: 'idle' });
  const [comment, setComment] = useState('');

  if (stage.state !== 'awaiting_deploy_approval') {
    return <DeployApprovalStatus stage={stage} runId={runId} />;
  }

  function start(decision: ApprovalDecision) {
    setPhase({ kind: 'confirming', decision });
    setComment('');
  }

  function cancel() {
    setPhase({ kind: 'idle' });
    setComment('');
  }

  async function confirm(decision: ApprovalDecision) {
    const previous = stage;
    setPhase({ kind: 'submitting' });
    onUpdate(optimisticUpdate(stage, decision));
    try {
      const updated = await api.submitApproval(stage.id, {
        decision,
        comment: comment.trim() || undefined,
      });
      onUpdate(updated);
      setPhase({ kind: 'idle' });
      setComment('');
    } catch (err) {
      onRollback(previous);
      setPhase({ kind: 'errored', message: formatApprovalError(err) });
    }
  }

  if (phase.kind === 'confirming') {
    const verb = phase.decision === 'approve' ? 'Approve' : 'Reject';
    return (
      <div className="flex flex-col gap-3 rounded-md border border-neutral-200 bg-neutral-50 p-3 text-sm dark:border-neutral-800 dark:bg-neutral-900">
        <label className="block">
          <span className="text-xs tracking-wide text-neutral-500 uppercase">
            Comment (optional)
          </span>
          <textarea
            value={comment}
            onChange={(e) => setComment(e.target.value)}
            rows={2}
            className="mt-1 w-full rounded-md border border-neutral-300 bg-white px-2 py-1 font-mono text-xs dark:border-neutral-700 dark:bg-neutral-950"
            aria-label={`${verb} comment`}
          />
        </label>
        <div className="flex justify-end gap-2">
          <Button variant="ghost" size="sm" onClick={cancel}>
            Cancel
          </Button>
          <Button
            size="sm"
            variant={phase.decision === 'reject' ? 'outline' : 'default'}
            onClick={() => confirm(phase.decision)}
          >
            Confirm {verb.toLowerCase()}
          </Button>
        </div>
      </div>
    );
  }

  if (phase.kind === 'submitting') {
    return (
      <div className="text-xs text-neutral-500" role="status" aria-live="polite">
        Submitting decision…
      </div>
    );
  }

  return (
    <div className="flex flex-col items-end gap-2">
      <div className="flex gap-2">
        <Button variant="outline" size="sm" onClick={() => start('reject')}>
          <X className="size-4" aria-hidden />
          <span>Reject</span>
        </Button>
        <Button size="sm" onClick={() => start('approve')}>
          <Check className="size-4" aria-hidden />
          <span>Approve</span>
        </Button>
      </div>
      <Link
        to={`/runs/${runId}#audit`}
        className="inline-flex items-center gap-1 text-xs text-neutral-500 hover:underline"
      >
        <FileClock className="size-3.5" aria-hidden />
        View audit log
      </Link>
      {phase.kind === 'errored' && (
        <p role="alert" className="font-mono text-xs text-rose-700 dark:text-rose-300">
          Submission failed: {phase.message}
        </p>
      )}
    </div>
  );
}

/*
 * Terminal / non-gate status line shown when the deploy stage is not
 * parked at the pre-execution gate — including the in-flight
 * awaiting_deployment state, a succeeded/failed deploy, or a
 * cancelled stage.
 */
function DeployApprovalStatus({ stage, runId }: { stage: Stage; runId: string }) {
  const verb =
    stage.state === 'succeeded'
      ? 'Deployed'
      : stage.state === 'awaiting_deployment'
        ? 'Deploying'
        : stage.state === 'failed'
          ? stage.failure_category === 'D'
            ? 'Rejected'
            : 'Failed'
          : stage.state;

  const showCategory =
    stage.state === 'failed' && stage.failure_category && stage.failure_category !== 'D';

  return (
    <div className="flex flex-col items-end gap-1 text-right text-sm">
      <span className="font-mono text-xs tracking-wide text-neutral-500 uppercase">
        {verb}
        {showCategory && ` · category ${stage.failure_category}`}
      </span>
      {stage.ended_at && (
        <span className="text-xs text-neutral-500">
          {new Date(stage.ended_at).toLocaleString()}
        </span>
      )}
      <Link
        to={`/runs/${runId}#audit`}
        className="inline-flex items-center gap-1 text-xs text-neutral-500 hover:underline"
      >
        <FileClock className="size-3.5" aria-hidden />
        View audit log
      </Link>
    </div>
  );
}

function formatApprovalError(err: unknown): string {
  if (err instanceof ApiClientError) {
    return `${err.status} · ${err.body?.error ?? err.message}`;
  }
  if (err instanceof Error) {
    return err.message;
  }
  return 'unknown error';
}
