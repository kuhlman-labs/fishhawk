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

/*
 * Retriable failure categories per MVP_SPEC §6 + the backend's
 * /retry handler:
 *
 *   - D-timeout (failure_reason starts with "sla_timeout"):
 *     re-opens the gate; updated_at trigger restarts the SLA
 *     clock. No orchestrator handoff.
 *   - A (agent failure) and C (infrastructure failure):
 *     re-dispatch via the orchestrator. The handler fires
 *     workflow_dispatch and the runner produces a fresh trace.
 *
 * D-rejected and B are deliberately not retriable — the approver
 * said no / the spec needs to change first; no Retry button there.
 */
function isRetriable(stage: Stage): boolean {
  if (stage.state !== 'failed') return false;
  switch (stage.failure_category) {
    case 'A':
    case 'C':
      return true;
    case 'D':
      return (
        typeof stage.failure_reason === 'string' && stage.failure_reason.startsWith('sla_timeout')
      );
  }
  return false;
}

// optimisticRetryState returns the state the stage will hold while
// the retry round-trip is in flight. The server returns the
// canonical post-retry stage; the optimistic value is just for the
// few-hundred-ms window where the request is on the wire.
function optimisticRetryState(stage: Stage): Stage['state'] {
  // D-timeout retries re-open the gate (awaiting_approval).
  // A/C retries fall back to pending until the orchestrator's
  // dispatch lands; the response replaces it with dispatched.
  if (stage.failure_category === 'D') return 'awaiting_approval';
  return 'pending';
}

type Phase = { kind: 'idle' } | { kind: 'submitting' } | { kind: 'errored'; message: string };

export function FailureBanner({ stage, onStageUpdate, onStageRollback }: Props) {
  const [phase, setPhase] = useState<Phase>({ kind: 'idle' });

  if (stage.state !== 'failed' || !stage.failure_category) {
    return null;
  }

  const description = describeFailure(stage.failure_category);
  const canRetry = isRetriable(stage) && onStageUpdate != null && onStageRollback != null;

  async function retry() {
    if (!onStageUpdate || !onStageRollback) return;
    const previous = stage;
    setPhase({ kind: 'submitting' });
    // Optimistic update — reflect the post-retry state immediately;
    // the server response replaces it on success. Target depends
    // on category (D-timeout → awaiting_approval; A/C → pending,
    // then dispatched once the orchestrator runs).
    onStageUpdate({
      ...stage,
      state: optimisticRetryState(stage),
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
        {/* Category B is a constraint/policy violation; the
         * implement page's <PolicySection> (#233) renders the
         * structured violations. Anchor link gets the reviewer
         * straight to it instead of leaving them to scroll. */}
        {stage.failure_category === 'B' && (
          <a href="#policy" className="inline-block text-xs underline">
            View violations
          </a>
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
