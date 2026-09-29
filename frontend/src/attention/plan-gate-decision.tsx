import { useState } from 'react';
import { api } from '@/api/client';
import { useAsync } from '@/api/use-async';
import type { AttentionItem, Stage } from '@/api/types';
import { ApprovalPanel } from '@/plan/approval-panel';
import { DecisionError, DecisionRefusal } from './decision-form';

/*
 * Plan-gate decision for the attention queue (E40.2 / #1717): REUSE, not
 * reimplementation. It fetches the stage the item points at and renders the
 * existing <ApprovalPanel>, so the two-step optimistic-then-rollback contract
 * lives in exactly one place.
 *
 * The item leaves the queue via `onResolved`, wired to ApprovalPanel's
 * `onSubmitted` — which fires ONLY after a successful approval POST. It is
 * deliberately NOT wired to `onUpdate`: ApprovalPanel calls onUpdate with the
 * optimistic stage BEFORE awaiting the POST, so resolving on onUpdate would
 * remove the item before the write landed and leave it removed on a failure
 * that rolls back. A rolled-back failure therefore keeps the item in the queue.
 */

export function PlanGateDecision({
  item,
  onResolved,
}: {
  item: AttentionItem;
  onResolved: () => void;
}) {
  // Fail closed on the optional wire ids: without both we cannot fetch the
  // stage or address the run, so refuse rather than build an undefined URL.
  if (!item.stage_id || !item.run_id) {
    return (
      <DecisionRefusal message="Can't decide the plan gate: this item is missing its stage or run id." />
    );
  }
  return <PlanGateLoader stageId={item.stage_id} runId={item.run_id} onResolved={onResolved} />;
}

function PlanGateLoader({
  stageId,
  runId,
  onResolved,
}: {
  stageId: string;
  runId: string;
  onResolved: () => void;
}) {
  const state = useAsync(() => api.getStage(stageId), [stageId]);

  if (state.status === 'loading') {
    return (
      <p className="text-xs text-neutral-500" role="status" aria-live="polite">
        Loading the plan stage…
      </p>
    );
  }
  if (state.status === 'error') {
    // A failed stage read must not present a decision surface over unknown
    // state — render the error envelope and no panel (F5).
    return <DecisionError message={state.error.message} />;
  }
  return <PlanGatePanel stage={state.data} runId={runId} onResolved={onResolved} />;
}

function PlanGatePanel({
  stage: fetched,
  runId,
  onResolved,
}: {
  stage: Stage;
  runId: string;
  onResolved: () => void;
}) {
  // Local stage state so ApprovalPanel's optimistic update + rollback have a
  // parent to write to (the queue has no page-level loader for this stage).
  const [stage, setStage] = useState<Stage>(fetched);
  return (
    <ApprovalPanel
      stage={stage}
      runId={runId}
      onUpdate={setStage}
      onRollback={setStage}
      onSubmitted={() => onResolved()}
      showRegenerate={false}
    />
  );
}
