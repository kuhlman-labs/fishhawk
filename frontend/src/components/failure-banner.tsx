import { useState } from 'react';
import { AlertTriangle, RefreshCw } from 'lucide-react';
import { api, ApiClientError } from '@/api/client';
import type { Stage } from '@/api/types';
import { describeFailure } from '@/api/types';
import { Button } from '@/components/ui/button';

/*
 * Inline banner shown above a stage when its state is failed.
 * Surfaces the category letter (A/B/C/D), the canonical description
 * (mirrors run.FailureCategory.Description() in Go), and the
 * stage's failure_reason verbatim. MVP_SPEC §6 requires that
 * failed runs are equally visible to successful ones — this banner
 * is part of how the UI honours that.
 *
 * For D-timeout failures (the only retriable category at v0), a
 * Retry button POSTs to /v0/stages/{id}/retry; success transitions
 * the stage back to awaiting_approval. Optimistic update lives at
 * the parent (StageDetail); we forward via onUpdate/onRollback,
 * matching the ApprovalPanel pattern.
 */
interface Props {
  stage: Stage;
  onStageUpdate?: (next: Stage) => void;
  onStageRollback?: (prev: Stage) => void;
}

function isDTimeout(stage: Stage): boolean {
  return (
    stage.state === 'failed' &&
    stage.failure_category === 'D' &&
    typeof stage.failure_reason === 'string' &&
    stage.failure_reason.startsWith('sla_timeout')
  );
}

type Phase = { kind: 'idle' } | { kind: 'submitting' } | { kind: 'errored'; message: string };

export function FailureBanner({ stage, onStageUpdate, onStageRollback }: Props) {
  const [phase, setPhase] = useState<Phase>({ kind: 'idle' });

  if (stage.state !== 'failed' || !stage.failure_category) {
    return null;
  }

  const description = describeFailure(stage.failure_category);
  const canRetry = isDTimeout(stage) && onStageUpdate != null && onStageRollback != null;

  async function retry() {
    if (!onStageUpdate || !onStageRollback) return;
    const previous = stage;
    setPhase({ kind: 'submitting' });
    // Optimistic — reflect the awaiting_approval state immediately;
    // the server response replaces it on success.
    onStageUpdate({
      ...stage,
      state: 'awaiting_approval',
      failure_category: null,
      failure_reason: null,
      ended_at: null,
    });
    try {
      const updated = await api.retryStage(stage.id);
      onStageUpdate(updated);
      setPhase({ kind: 'idle' });
    } catch (err) {
      onStageRollback(previous);
      const msg =
        err instanceof ApiClientError
          ? `${err.status} · ${err.body?.error ?? err.message}`
          : err instanceof Error
            ? err.message
            : 'unknown error';
      setPhase({ kind: 'errored', message: msg });
    }
  }

  return (
    <div
      role="alert"
      className="flex gap-3 rounded-md border border-rose-300 bg-rose-50 p-4 text-sm text-rose-900 dark:border-rose-900/60 dark:bg-rose-950/40 dark:text-rose-200"
    >
      <AlertTriangle className="mt-0.5 size-4 shrink-0" aria-hidden />
      <div className="flex-1 space-y-1">
        <div className="font-medium">
          Failed · category {stage.failure_category}
          {description && <span className="font-normal"> — {description}</span>}
        </div>
        {stage.failure_reason && (
          <div className="font-mono text-xs leading-relaxed">{stage.failure_reason}</div>
        )}
        {phase.kind === 'errored' && (
          <p className="font-mono text-xs">Retry failed: {phase.message}</p>
        )}
      </div>
      {canRetry && (
        <div className="shrink-0 self-start">
          <Button
            size="sm"
            variant="outline"
            onClick={retry}
            disabled={phase.kind === 'submitting'}
          >
            <RefreshCw
              className={phase.kind === 'submitting' ? 'size-4 animate-spin' : 'size-4'}
              aria-hidden
            />
            <span>{phase.kind === 'submitting' ? 'Retrying…' : 'Retry'}</span>
          </Button>
        </div>
      )}
    </div>
  );
}
