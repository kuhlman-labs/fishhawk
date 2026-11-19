import { useState } from 'react';
import { api, ApiClientError } from '@/api/client';
import type { AttentionItem, AttentionWaiveProposal, ConcernJudgement } from '@/api/types';
import { Button } from '@/components/ui/button';
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
 * label for the item's own decision is read from DECISION_VERBS so the
 * queue's verb-equality control has a single source to hold to account. The
 * one exception is the server-check 'Proposed waive' block (ADR-143 A1,
 * #5229): its Confirm/Reject decide an agent's proposal, are not drive-plane
 * verbs, and deliberately sit outside DECISION_VERBS (WaiveProposalBlock).
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

/*
 * The closed set of HUMAN concern judgements (ADR-117 D1 phase 2, E94.7 /
 * #4840), in the server's concern.HumanJudgements order. `unlabelled` is the
 * read of an unjudged concern and is never offered.
 */
const JUDGEMENT_OPTIONS: { value: ConcernJudgement; label: string }[] = [
  { value: 'defect', label: 'defect — a real defect in this change' },
  { value: 'defect_pre_existing', label: 'defect_pre_existing — real, but predates this change' },
  { value: 'no_defect', label: 'no_defect — not a defect (false positive)' },
  { value: 'tradeoff', label: 'tradeoff — a real cost, knowingly accepted' },
  { value: 'preference', label: 'preference — a matter of taste, not correctness' },
  { value: 'stale', label: 'stale — no longer applies to the current code' },
  { value: 'insufficient_evidence', label: 'insufficient_evidence — cannot be judged as raised' },
];

function JudgementSelect({
  value,
  onChange,
  disabled,
}: {
  value: ConcernJudgement | '';
  onChange: (v: ConcernJudgement | '') => void;
  disabled?: boolean;
}) {
  return (
    <label className="block">
      <span className="text-xs tracking-wide text-neutral-500 uppercase">Judgement</span>
      <select
        value={value}
        onChange={(e) =>
          onChange(JUDGEMENT_OPTIONS.find((o) => o.value === e.target.value)?.value ?? '')
        }
        disabled={disabled}
        className="mt-1 w-full rounded-md border border-neutral-300 bg-white px-2 py-1 font-mono text-xs disabled:opacity-50 dark:border-neutral-700 dark:bg-neutral-950"
        aria-label="Judgement"
      >
        <option value="">Choose a judgement…</option>
        {JUDGEMENT_OPTIONS.map((o) => (
          <option key={o.value} value={o.value}>
            {o.label}
          </option>
        ))}
      </select>
    </label>
  );
}

function ConcernDecision({ item, onResolved, onCancel }: PanelProps) {
  const [reason, setReason] = useState('');
  const [judgement, setJudgement] = useState<ConcernJudgement | ''>('');
  const [tradeoffCost, setTradeoffCost] = useState('');
  const [parentEpic, setParentEpic] = useState('');
  const [note, setNote] = useState('');
  const { submitting, error, submit } = useDecisionSubmit(onResolved);

  if (!item.concern_id) {
    return <DecisionRefusal message="Can't decide this concern: it is missing its concern id." />;
  }
  const concernId = item.concern_id;
  const verbs =
    item.kind === 'split_verdict' ? DECISION_VERBS.split_verdict : DECISION_VERBS.paged_concern; // ['Waive', 'Defer']

  // Both verbs REQUIRE a judgement (E94.7 / #4840), and a tradeoff also its
  // stated cost. The cost is sent ONLY with a tradeoff: a cost left over from
  // an earlier tradeoff choice would otherwise be refused server-side
  // (tradeoff_cost_without_tradeoff).
  const isTradeoff = judgement === 'tradeoff';
  const judgementBlank = judgement === '' || (isTradeoff && !tradeoffCost.trim());
  // Fail closed: with no judgement chosen nothing is sent (the buttons are
  // also disabled via requiredBlank).
  const submitJudged = (
    run: (fields: { judgement: ConcernJudgement; tradeoff_cost?: string }) => Promise<unknown>,
  ) => {
    if (judgement === '') return;
    return submit(() =>
      run({ judgement, tradeoff_cost: isTradeoff ? tradeoffCost.trim() : undefined }),
    );
  };

  return (
    <div className="flex flex-col gap-3 text-sm">
      {item.context.provenance === 'server_check' && (
        <p
          data-testid="server-check-notice"
          role="note"
          className="rounded border border-amber-300 bg-amber-50 px-2 py-1.5 text-xs text-amber-900 dark:border-amber-800 dark:bg-amber-950/40 dark:text-amber-200"
        >
          A deterministic server check raised this concern, not a reviewer. Only a human signed in
          to this Web UI can waive or defer it: CLI, MCP and API tokens are refused
          (concern_requires_session, or concern_requires_human for an agent token).
        </p>
      )}
      {item.context.waive_proposal && (
        <WaiveProposalBlock
          concernId={concernId}
          proposal={item.context.waive_proposal}
          onResolved={onResolved}
        />
      )}
      <p className="text-xs text-neutral-500 dark:text-neutral-400">
        Waive resolves the concern with your reason. Defer files a follow-up issue under the parent
        epic and consumes no fix-up budget — both are terminal, and both record your judgement (a
        tradeoff also records its cost).
      </p>
      <JudgementSelect value={judgement} onChange={setJudgement} disabled={submitting} />
      {isTradeoff && (
        <TextField
          label="Tradeoff cost"
          value={tradeoffCost}
          onChange={setTradeoffCost}
          disabled={submitting}
        />
      )}
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
            requiredBlank: !reason.trim() || judgementBlank,
            onClick: () =>
              submitJudged((fields) =>
                api.waiveConcern(concernId, { reason: reason.trim(), ...fields }),
              ),
          },
          {
            label: verbs[1], // Defer
            requiredBlank: !parentEpic.trim() || judgementBlank,
            onClick: () =>
              submitJudged((fields) =>
                api.deferConcern(concernId, {
                  ...fields,
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

/*
 * The 'Proposed waive' block (ADR-143 A1, #5229): an operator agent's PENDING
 * waive proposal on a server-check concern, decided here by the signed-in
 * human in one step. The proposal itself cleared nothing.
 *
 * - Every agent-authored field (reason, evidence, tradeoff cost) is rendered
 *   as React text, never markup, under the 'agent-authored' label. The
 *   check_key (flagged location and pattern class) is server-written.
 * - Confirm sends the proposal_sequence and ONLY a reason the human typed
 *   here. It never forwards the agent's reason: the server records a fixed
 *   template as the concern's state_reason when none is typed (Y1).
 * - Reject requires a human reason and leaves the concern OPEN, so it does
 *   not resolve the queue item: the block collapses to a note and the
 *   Waive/Defer controls below stay available.
 * - Confirm and Reject are NOT DECISION_VERBS entries and carry no
 *   data-decision-verb attribute (Y3). They are session-only decisions on a
 *   proposal, not drive-plane verbs, and their labels are distinct from every
 *   DECISION_VERBS label.
 */
type ProposalPhase =
  | { kind: 'idle' }
  | { kind: 'submitting' }
  | { kind: 'rejected' }
  | { kind: 'errored'; error: unknown };

function shortSha(sha: string | undefined): string {
  return (sha ?? '').slice(0, 12);
}

function WaiveProposalError({ error }: { error: unknown }) {
  if (error instanceof ApiClientError && error.body?.error === 'waive_proposal_stale') {
    const details = error.body.details ?? {};
    const proposed = typeof details.proposal_head_sha === 'string' ? details.proposal_head_sha : '';
    const current = typeof details.current_head_sha === 'string' ? details.current_head_sha : '';
    return (
      <p role="alert" className="text-xs text-rose-700 dark:text-rose-300">
        The PR head moved since this proposal was made
        {proposed && current ? ` (${shortSha(proposed)} → ${shortSha(current)})` : ''}, so the
        proposal is no longer confirmable. Waive or defer the concern directly, or wait for a new
        proposal at the current head.
      </p>
    );
  }
  let message = 'unknown error';
  if (error instanceof ApiClientError) {
    const code = error.body?.error ?? error.message;
    const text = error.body?.message;
    message = `${error.status} · ${code}${text && text !== code ? ` — ${text}` : ''}`;
  } else if (error instanceof Error) {
    message = error.message;
  }
  return (
    <p role="alert" className="font-mono text-xs text-rose-700 dark:text-rose-300">
      {message}
    </p>
  );
}

function WaiveProposalBlock({
  concernId,
  proposal,
  onResolved,
}: {
  concernId: string;
  proposal: AttentionWaiveProposal;
  onResolved: () => void;
}) {
  const [confirmReason, setConfirmReason] = useState('');
  const [rejectReason, setRejectReason] = useState('');
  const [phase, setPhase] = useState<ProposalPhase>({ kind: 'idle' });
  const submitting = phase.kind === 'submitting';

  if (phase.kind === 'rejected') {
    return (
      <p
        data-testid="waive-proposal-rejected"
        role="status"
        className="rounded border border-neutral-300 px-2 py-1.5 text-xs text-neutral-700 dark:border-neutral-700 dark:text-neutral-300"
      >
        Proposal rejected. The concern stays open: waive or defer it below.
      </p>
    );
  }

  async function decide(run: () => Promise<unknown>, after: () => void) {
    setPhase({ kind: 'submitting' });
    try {
      await run();
      after();
    } catch (err) {
      setPhase({ kind: 'errored', error: err });
    }
  }

  const confirm = () =>
    decide(
      () =>
        api.confirmWaiveProposal(concernId, {
          proposal_sequence: proposal.proposal_sequence,
          reason: confirmReason.trim() || undefined,
        }),
      // A confirm waives the concern: the item leaves the queue.
      () => {
        onResolved();
        setPhase({ kind: 'idle' });
      },
    );

  const reject = () => {
    // Fail closed: a blank reason sends nothing (the button is also disabled).
    if (!rejectReason.trim()) return;
    return decide(
      () =>
        api.rejectWaiveProposal(concernId, {
          proposal_sequence: proposal.proposal_sequence,
          reason: rejectReason.trim(),
        }),
      // A reject leaves the concern open: keep the item, drop the block.
      () => setPhase({ kind: 'rejected' }),
    );
  };

  return (
    <section
      data-testid="waive-proposal"
      aria-label="Proposed waive"
      className="flex flex-col gap-2 rounded border border-sky-300 bg-sky-50 px-2 py-2 text-xs text-neutral-800 dark:border-sky-800 dark:bg-sky-950/40 dark:text-neutral-200"
    >
      <h4 className="text-xs font-semibold tracking-wide uppercase">Proposed waive</h4>
      <p
        data-testid="waive-proposal-agent-label"
        className="font-medium text-amber-800 dark:text-amber-300"
      >
        agent-authored: verify before confirming
      </p>
      <dl className="grid grid-cols-[max-content_1fr] gap-x-3 gap-y-1">
        <dt className="text-neutral-500">Proposed by</dt>
        <dd data-testid="waive-proposal-proposer" className="font-mono break-all">
          {proposal.proposed_by} ({proposal.proposed_by_actor_kind})
        </dd>
        <dt className="text-neutral-500">Judgement</dt>
        <dd data-testid="waive-proposal-judgement" className="font-mono">
          {proposal.judgement}
        </dd>
        {proposal.tradeoff_cost && (
          <>
            <dt className="text-neutral-500">Tradeoff cost</dt>
            <dd data-testid="waive-proposal-tradeoff-cost" className="whitespace-pre-wrap">
              {proposal.tradeoff_cost}
            </dd>
          </>
        )}
        <dt className="text-neutral-500">Reason</dt>
        <dd data-testid="waive-proposal-reason" className="whitespace-pre-wrap">
          {proposal.reason}
        </dd>
        <dt className="text-neutral-500">Evidence</dt>
        <dd data-testid="waive-proposal-evidence" className="whitespace-pre-wrap">
          {proposal.evidence}
        </dd>
        <dt className="text-neutral-500">Check</dt>
        <dd data-testid="waive-proposal-check-key" className="font-mono break-all">
          {proposal.check_key}
        </dd>
        <dt className="text-neutral-500">Head</dt>
        <dd data-testid="waive-proposal-head" className="font-mono" title={proposal.head_sha}>
          {shortSha(proposal.head_sha)}
        </dd>
      </dl>
      <p className="text-neutral-500 dark:text-neutral-400">
        Confirm waives the concern with this judgement, recorded as your decision; the agent&apos;s
        text stays on the proposal and is not used as your reason. Reject records your reason and
        leaves the concern open.
      </p>
      <ReasonField
        label="Confirm reason (optional)"
        value={confirmReason}
        onChange={setConfirmReason}
        disabled={submitting}
      />
      <ReasonField
        label="Reject reason"
        value={rejectReason}
        onChange={setRejectReason}
        disabled={submitting}
      />
      <div className="flex flex-wrap justify-end gap-2">
        <Button
          size="sm"
          variant="outline"
          onClick={() => void reject()}
          disabled={submitting || !rejectReason.trim()}
        >
          Reject proposal
        </Button>
        <Button size="sm" variant="outline" onClick={() => void confirm()} disabled={submitting}>
          Confirm waive
        </Button>
      </div>
      {phase.kind === 'errored' && <WaiveProposalError error={phase.error} />}
    </section>
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
