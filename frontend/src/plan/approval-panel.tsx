import { useState } from 'react';
import { Link } from 'react-router';
import { Check, FileClock, RefreshCw, X } from 'lucide-react';
import { Button } from '@/components/ui/button';
import { api } from '@/api/client';
import { ApiClientError } from '@/api/client';
import type { ApprovalDecision, Stage } from '@/api/types';

/*
 * Two-step approval surface for a plan stage:
 *
 *   idle   → click Approve/Reject     → confirming
 *   confirming → click Confirm        → submitting (optimistic stage applied)
 *                          (success)  → idle (panel hides; stage is now terminal)
 *                          (error)    → idle + error message; optimistic state rolled back
 *
 * The "optimistic with rollback" requirement (#57) means we move the
 * stage state immediately on submit and only revert if the network
 * call fails. The parent owns the stage prop; we reach in via
 * onUpdate/onRollback callbacks so the page-level loader stays the
 * source of truth.
 */

interface Props {
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
  /*
   * approve → succeeded; reject → failed (category D, "approval timeout"
   * is what the backend tags both rejection and SLA elapse as). The
   * server-returned Stage replaces this on success; we only render
   * this for the few hundred ms the round-trip takes.
   */
  if (decision === 'approve') {
    return { ...stage, state: 'succeeded', failure_category: null, failure_reason: null };
  }
  return {
    ...stage,
    state: 'failed',
    failure_category: 'D',
    failure_reason: 'gate rejected',
  };
}

export function ApprovalPanel({ stage, runId, onUpdate, onRollback }: Props) {
  const [phase, setPhase] = useState<Phase>({ kind: 'idle' });
  const [comment, setComment] = useState('');

  if (stage.state !== 'awaiting_approval') {
    return <ApprovalStatus stage={stage} runId={runId} />;
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
      const msg =
        err instanceof ApiClientError
          ? `${err.status} · ${err.body?.error ?? err.message}`
          : err instanceof Error
            ? err.message
            : 'unknown error';
      setPhase({ kind: 'errored', message: msg });
    }
  }

  if (phase.kind === 'confirming') {
    const verb = phase.decision === 'approve' ? 'Approve' : 'Reject';
    const submittingThis = phase.kind === 'confirming';
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
          <Button variant="ghost" size="sm" onClick={cancel} disabled={!submittingThis}>
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
        <Button variant="outline" size="sm" disabled title="Wires up in E8.3 (#146)">
          <RefreshCw className="size-4" aria-hidden />
          <span>Regenerate</span>
        </Button>
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

function ApprovalStatus({ stage, runId }: { stage: Stage; runId: string }) {
  const verb =
    stage.state === 'succeeded'
      ? 'Approved'
      : stage.state === 'failed'
        ? stage.failure_category === 'D'
          ? 'Rejected'
          : 'Failed'
        : stage.state;

  return (
    <div className="flex flex-col items-end gap-1 text-right text-sm">
      <span className="font-mono text-xs tracking-wide text-neutral-500 uppercase">{verb}</span>
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
