import { useState } from 'react';
import { api } from '@/api/client';
import type { AttentionItem } from '@/api/types';
import { DECISION_VERBS } from './decision-verbs';
import {
  AcknowledgeCheckbox,
  DecisionActions,
  DecisionError,
  DecisionRefusal,
  ReasonField,
  TextField,
  useDecisionSubmit,
} from './decision-form';
import { PlanGateDecision } from './plan-gate-decision';

/*
 * The per-kind decision panel wired into each attention-queue card
 * (E40.2 / #1717). `onResolved` removes the item from the live list on a
 * successful write; `onCancel` collapses the disclosure. Every submit button
 * label is read from DECISION_VERBS so the queue's verb-equality control has
 * a single source to hold to account.
 *
 * Exhaustive over AttentionItemKind (the `never` assignment): a seventh kind
 * fails typecheck here rather than rendering a blank panel.
 */
export function DecisionPanel({
  item,
  onResolved,
  onCancel,
}: {
  item: AttentionItem;
  onResolved: () => void;
  onCancel: () => void;
}) {
  const kind = item.kind;
  switch (kind) {
    case 'plan_gate':
      return <PlanGateDecision item={item} onResolved={onResolved} />;
    case 'scope_amendment':
      return <ScopeAmendmentDecision item={item} onResolved={onResolved} onCancel={onCancel} />;
    case 'acceptance_disposition':
      return <AcceptanceDecision item={item} onResolved={onResolved} onCancel={onCancel} />;
    case 'split_verdict':
    case 'paged_concern':
      return <ConcernDecision item={item} onResolved={onResolved} onCancel={onCancel} />;
    case 'attend_human_led_campaign':
      // Work for a human to lead, not a decision to submit — no endpoint, no
      // submit control. (The card also renders no Decide toggle for this kind.)
      return (
        <DecisionRefusal message="This is a human-led campaign item — lead it directly; there is no decision to submit here." />
      );
    default: {
      const unhandled: never = kind;
      return <DecisionRefusal message={`Unknown item kind: ${String(unhandled)}`} />;
    }
  }
}

interface PanelProps {
  item: AttentionItem;
  onResolved: () => void;
  onCancel: () => void;
}

function ScopeAmendmentDecision({ item, onResolved, onCancel }: PanelProps) {
  const [reason, setReason] = useState('');
  const { submitting, error, submit } = useDecisionSubmit(onResolved);

  if (!item.run_id || !item.amendment_id) {
    return (
      <DecisionRefusal message="Can't decide this amendment: it is missing its run or amendment id." />
    );
  }
  const runId = item.run_id;
  const amendmentId = item.amendment_id;
  const verbs = DECISION_VERBS.scope_amendment; // ['Approve', 'Deny']

  return (
    <div className="flex flex-col gap-3 text-sm">
      <ReasonField
        label="Decision reason (optional)"
        value={reason}
        onChange={setReason}
        disabled={submitting}
      />
      <DecisionActions
        submitting={submitting}
        onCancel={onCancel}
        verbs={[
          {
            label: verbs[0],
            onClick: () =>
              submit(() =>
                api.decideScopeAmendment(runId, amendmentId, {
                  decision: 'approve',
                  reason: reason.trim() || undefined,
                }),
              ),
          },
          {
            label: verbs[1],
            onClick: () =>
              submit(() =>
                api.decideScopeAmendment(runId, amendmentId, {
                  decision: 'deny',
                  reason: reason.trim() || undefined,
                }),
              ),
          },
        ]}
      />
      {error && <DecisionError message={error} />}
    </div>
  );
}

function ConcernDecision({ item, onResolved, onCancel }: PanelProps) {
  const [reason, setReason] = useState('');
  const [parentEpic, setParentEpic] = useState('');
  const [note, setNote] = useState('');
  const { submitting, error, submit } = useDecisionSubmit(onResolved);

  if (!item.concern_id) {
    return <DecisionRefusal message="Can't decide this concern: it is missing its concern id." />;
  }
  const concernId = item.concern_id;
  const verbs =
    item.kind === 'split_verdict' ? DECISION_VERBS.split_verdict : DECISION_VERBS.paged_concern; // ['Waive', 'Defer']

  return (
    <div className="flex flex-col gap-3 text-sm">
      <p className="text-xs text-neutral-500 dark:text-neutral-400">
        Waive resolves the concern with your reason. Defer files a follow-up issue under the parent
        epic and consumes no fix-up budget — both are terminal.
      </p>
      <ReasonField label="Waive reason" value={reason} onChange={setReason} disabled={submitting} />
      <TextField
        label="Defer: parent epic (e.g. #1196)"
        value={parentEpic}
        onChange={setParentEpic}
        disabled={submitting}
      />
      <ReasonField
        label="Defer note (optional)"
        value={note}
        onChange={setNote}
        disabled={submitting}
      />
      <DecisionActions
        submitting={submitting}
        onCancel={onCancel}
        verbs={[
          {
            label: verbs[0], // Waive
            requiredBlank: !reason.trim(),
            onClick: () => submit(() => api.waiveConcern(concernId, { reason: reason.trim() })),
          },
          {
            label: verbs[1], // Defer
            requiredBlank: !parentEpic.trim(),
            onClick: () =>
              submit(() =>
                api.deferConcern(concernId, {
                  parent_epic: parentEpic.trim(),
                  note: note.trim() || undefined,
                }),
              ),
          },
        ]}
      />
      {error && <DecisionError message={error} />}
    </div>
  );
}

function AcceptanceDecision({ item, onResolved, onCancel }: PanelProps) {
  const [reason, setReason] = useState('');
  const [acknowledged, setAcknowledged] = useState(false);
  const { submitting, error, submit } = useDecisionSubmit(onResolved);

  if (!item.run_id) {
    return (
      <DecisionRefusal message="Can't arbitrate: this acceptance item is missing its run id." />
    );
  }
  const runId = item.run_id;

  // Acknowledge is required — and its checkbox shown — ONLY when the discharged
  // outcome carries failed criteria (criteria_failed > 0). Deleting the
  // `criteria_failed > 0` guard enables the button with the box unticked; the
  // acceptance golden carries criteria_failed = 2, so that regression reddens.
  const failedCount = item.context.criteria_failed ?? 0;
  const requiresAck = failedCount > 0;
  const requiredBlank = !reason.trim() || (requiresAck && !acknowledged);
  const verb = DECISION_VERBS.acceptance_disposition[0]; // 'Record arbitration'

  return (
    <div className="flex flex-col gap-3 text-sm">
      <ReasonField
        label="Arbitration reason"
        value={reason}
        onChange={setReason}
        disabled={submitting}
      />
      {requiresAck && (
        <AcknowledgeCheckbox
          label={`Acknowledge merging despite ${failedCount} failed acceptance criteri${failedCount === 1 ? 'on' : 'a'}`}
          checked={acknowledged}
          onChange={setAcknowledged}
          disabled={submitting}
        />
      )}
      <DecisionActions
        submitting={submitting}
        onCancel={onCancel}
        verbs={[
          {
            label: verb,
            requiredBlank,
            onClick: () =>
              submit(() =>
                api.arbitrateAcceptance(runId, {
                  reason: reason.trim(),
                  acknowledge_failed_criteria: acknowledged,
                }),
              ),
          },
        ]}
      />
      {error && <DecisionError message={error} />}
    </div>
  );
}
