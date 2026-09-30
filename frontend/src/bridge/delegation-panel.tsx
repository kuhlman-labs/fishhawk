import { useState } from 'react';
import { api, ApiClientError } from '@/api/client';
import { useAsync, type AsyncState } from '@/api/use-async';
import type {
  DelegationConfirmationResponse,
  DelegationWorkflowStatus,
  RepoDelegation,
  RepoDelegationAction,
  RepoDelegationEscalation,
  RepoDelegationWorkflow,
} from '@/api/captain';
import { Button } from '@/components/ui/button';
import { DecisionError } from '@/attention/decision-form';
import { RepoPanelFrame } from '@/repo/throughput-panel';
import { DelegationLowerForm } from './delegation-lower-form';
import { apiErrorCode, captainRefusalMessage, shortHash } from './viewer';

/*
 * The delegation confirmation screen on the Bridge tab (E76.6 / #3769).
 *
 * Two INDEPENDENT reads, each with its own error surface, joined by workflow
 * id: the resolved view (GET /v0/repos/{owner}/{name}/delegation, E76.1 /
 * #3747) and the confirmation verdicts (GET .../delegation/confirmation,
 * E76.5 / #3768). Both read `source=ref` — the route's documented default —
 * and never fall back to `run_cache` on their own.
 *
 * FAIL CLOSED. A status badge renders only when the view's own view-level
 * `confirmation` block is PRESENT and carries no `unavailable`: an absent
 * block (the store is unwired, or the pre-#3768 shape) or an `unavailable`
 * one means the confirmation state could not be read, and an empty or
 * missing verdict is then NOT "all confirmed" — so no workflow is shown as
 * confirmed OR unconfirmed. A 501 `delegation_confirm_unconfigured` disables
 * both verbs with that reason named.
 *
 * "Confirm as shown" binds THAT workflow's OWN `content_hash`, never the
 * view-level one: per-workflow hashes are stamped before any `workflow`
 * filter while the view-level hash is recomputed over the retained set, so
 * binding the view-level hash 409s every confirm. A 409
 * `delegation_hash_stale` renders `details.current_content_hash` and
 * re-reads the view so the next confirm binds the current hash.
 *
 * `generation` is bumped by the tab after a seat change (accept, claim,
 * relinquish): the server voids every earlier confirmation at a seat change,
 * so both reads re-run rather than keep a stale "confirmed" badge.
 */

export interface DelegationPanelProps {
  owner: string;
  name: string;
  /** Bumped by the tab on a seat change; re-runs both reads. */
  generation?: number;
}

type ConfirmationReading =
  | { kind: 'loading' }
  | { kind: 'unconfigured' }
  | { kind: 'error'; message: string }
  | { kind: 'unreadable'; reason: string }
  | { kind: 'ok'; data: DelegationConfirmationResponse };

const UNCONFIGURED = 'no delegation-confirmation store on this instance';

function readConfirmation(
  view: RepoDelegation,
  verdicts: AsyncState<DelegationConfirmationResponse>,
): ConfirmationReading {
  if (
    verdicts.status === 'error' &&
    apiErrorCode(verdicts.error) === 'delegation_confirm_unconfigured'
  ) {
    return { kind: 'unconfigured' };
  }
  if (view.confirmation === undefined) {
    return { kind: 'unreadable', reason: 'the delegation view carries no confirmation block' };
  }
  if (view.confirmation.unavailable) {
    return { kind: 'unreadable', reason: view.confirmation.unavailable };
  }
  if (verdicts.status === 'loading') return { kind: 'loading' };
  if (verdicts.status === 'error') {
    return { kind: 'error', message: captainRefusalMessage(verdicts.error) };
  }
  return { kind: 'ok', data: verdicts.data };
}

// RepoPanelFrame renders `error.message`; an API refusal is rendered as its
// mapped sentence + raw code followed by the server's own message (which, for
// a 503 github_unconfigured under source=ref, names the run_cache alternative).
function namedError<T>(state: AsyncState<T>): AsyncState<T> {
  if (state.status !== 'error' || !(state.error instanceof ApiClientError)) return state;
  return {
    status: 'error',
    error: new Error(`${captainRefusalMessage(state.error)} — ${state.error.message}`),
  };
}

export function DelegationPanel({ owner, name, generation = 0 }: DelegationPanelProps) {
  // Two local counters: a hash-stale refusal re-reads the VIEW (and so the
  // verdicts); a resolved confirm or lower re-reads only the VERDICTS, so the
  // workflow cards — and a lower form's filed result — stay mounted.
  const [viewGen, setViewGen] = useState(0);
  const [verdictGen, setVerdictGen] = useState(0);
  const view = useAsync(
    () => api.getRepoDelegation(owner, name),
    [owner, name, generation, viewGen],
  );
  const verdicts = useAsync(
    () => api.getRepoDelegationConfirmation(owner, name),
    [owner, name, generation, viewGen, verdictGen],
  );
  const [refusals, setRefusals] = useState<Record<string, string>>({});
  const [confirming, setConfirming] = useState<string | null>(null);
  const [verbUnconfigured, setVerbUnconfigured] = useState(false);
  const rereadView = () => setViewGen((n) => n + 1);
  const rereadVerdicts = () => setVerdictGen((n) => n + 1);

  async function confirm(wf: RepoDelegationWorkflow) {
    setConfirming(wf.id);
    setRefusals((r) => ({ ...r, [wf.id]: '' }));
    try {
      await api.confirmRepoDelegation(owner, name, {
        workflow: wf.id,
        content_hash: wf.content_hash,
      });
      rereadVerdicts();
    } catch (err) {
      setRefusals((r) => ({ ...r, [wf.id]: captainRefusalMessage(err) }));
      const code = apiErrorCode(err);
      if (code === 'delegation_hash_stale') rereadView();
      if (code === 'delegation_confirm_unconfigured') setVerbUnconfigured(true);
    } finally {
      setConfirming(null);
    }
  }

  // `truncated?: false` satisfies RepoPanelFrame's weak-type constraint
  // without ever triggering its run-scan partial-data note.
  const frameState: AsyncState<{ view: RepoDelegation; truncated?: false }> =
    view.status === 'ok' ? { status: 'ok', data: { view: view.data } } : namedError(view);

  return (
    <RepoPanelFrame title="Delegation" state={frameState}>
      {({ view: v }) => {
        const reading = readConfirmation(v, verdicts);
        const disabledReason =
          reading.kind === 'unconfigured' || verbUnconfigured ? UNCONFIGURED : undefined;
        return (
          <div className="space-y-4 text-sm">
            <ViewHeader view={v} />
            <ConfirmationLine reading={reading} />
            {disabledReason !== undefined && (
              <p role="note" className="text-xs text-neutral-600 dark:text-neutral-400">
                Confirm and lower are unavailable: {disabledReason}{' '}
                (delegation_confirm_unconfigured).
              </p>
            )}
            {v.workflows.length === 0 && (
              <p className="text-neutral-500">The spec at this source declares no workflows.</p>
            )}
            {v.workflows.map((wf) => (
              <WorkflowCard
                key={wf.id}
                owner={owner}
                name={name}
                wf={wf}
                reading={reading}
                disabledReason={disabledReason}
                busy={confirming !== null}
                refusal={refusals[wf.id] || null}
                onConfirm={() => void confirm(wf)}
                onFiled={rereadVerdicts}
              />
            ))}
          </div>
        );
      }}
    </RepoPanelFrame>
  );
}

function ViewHeader({ view }: { view: RepoDelegation }) {
  return (
    <div className="space-y-1 text-xs text-neutral-600 dark:text-neutral-400">
      <p>
        Source <span className="font-mono">{view.source}</span>
        {view.ref && (
          <>
            {' '}
            at <span className="font-mono">{view.ref}</span>
          </>
        )}
        {view.workflow_sha && (
          <>
            {' '}
            (<span className="font-mono">{shortHash(view.workflow_sha)}</span>)
          </>
        )}{' '}
        · schema major {view.schema_major} · view hash{' '}
        <span className="font-mono" title={view.content_hash}>
          {shortHash(view.content_hash)}
        </span>
      </p>
      <p>
        Each matrix is the WORKFLOW-level matrix: a gate&apos;s own autonomy block overrides it
        wholesale at that gate. An escalation&apos;s ceiling matrix is DECLARATIVE — this view does
        not evaluate it.
      </p>
    </div>
  );
}

function ConfirmationLine({ reading }: { reading: ConfirmationReading }) {
  switch (reading.kind) {
    case 'loading':
      return <p className="text-xs text-neutral-500">Reading confirmation verdicts…</p>;
    case 'unconfigured':
      return (
        <p role="note" data-testid="confirmation-state" className="text-xs">
          No delegation-confirmation store on this instance; no workflow is shown as confirmed or
          unconfirmed.
        </p>
      );
    case 'unreadable':
      return (
        <p
          role="note"
          data-testid="confirmation-state"
          className="rounded-md border border-amber-300 bg-amber-50 px-3 py-2 text-xs text-amber-900 dark:border-amber-900/60 dark:bg-amber-950/40 dark:text-amber-200"
        >
          Confirmation state could not be read ({reading.reason}). No workflow is shown as confirmed
          or unconfirmed.
        </p>
      );
    case 'error':
      return (
        <p role="alert" data-testid="confirmation-state" className="text-xs text-rose-700">
          Confirmation verdicts could not be read: {reading.message}
        </p>
      );
    case 'ok':
      return (
        <p data-testid="confirmation-state" className="text-xs">
          Captain in force:{' '}
          <span className="font-mono">{reading.data.captain ?? 'none (the seat is vacant)'}</span>{' '}
          (seat #{reading.data.seat_sequence}) · unconfirmed:{' '}
          {reading.data.unconfirmed_workflows.length === 0
            ? 'none'
            : reading.data.unconfirmed_workflows.join(', ')}
        </p>
      );
  }
}

function WorkflowCard({
  owner,
  name,
  wf,
  reading,
  disabledReason,
  busy,
  refusal,
  onConfirm,
  onFiled,
}: {
  owner: string;
  name: string;
  wf: RepoDelegationWorkflow;
  reading: ConfirmationReading;
  disabledReason?: string;
  busy: boolean;
  refusal: string | null;
  onConfirm: () => void;
  onFiled: () => void;
}) {
  const verdict =
    reading.kind === 'ok'
      ? (reading.data.workflows.find((w) => w.workflow === wf.id) ?? wf.confirmation)
      : undefined;
  return (
    <article
      data-testid={`delegation-workflow-${wf.id}`}
      className="space-y-3 rounded-md border border-neutral-200 p-3 dark:border-neutral-800"
    >
      <header className="flex flex-wrap items-center gap-2">
        <h3 className="font-mono font-semibold">{wf.id}</h3>
        <span className="text-xs text-neutral-500">
          tier {wf.autonomy ?? 'undeclared'} · hash{' '}
          <span className="font-mono" title={wf.content_hash}>
            {shortHash(wf.content_hash)}
          </span>
        </span>
        {reading.kind === 'ok' &&
          (verdict ? (
            <StatusBadge workflow={wf.id} status={verdict} />
          ) : (
            <span className="text-xs text-neutral-500">no verdict returned for this workflow</span>
          ))}
      </header>
      {verdict?.lower_proposal && (
        <p className="text-xs text-neutral-600 dark:text-neutral-400">
          Lower proposed by <span className="font-mono">{verdict.lower_proposal.subject}</span> (
          {verdict.lower_proposal.filed_ref}), #{verdict.lower_proposal.sequence}.
        </p>
      )}
      <Matrix actions={wf.matrix} />
      {wf.must_page_human && wf.must_page_human.length > 0 && (
        <p className="text-xs">
          Must page a human: <span className="font-mono">{wf.must_page_human.join(', ')}</span>
        </p>
      )}
      {wf.model_policy && (
        <p className="text-xs">
          Model policy: {wf.model_policy.strategy ?? 'unspecified'}
          {wf.model_policy.defaults &&
            ` · defaults ${Object.entries(wf.model_policy.defaults)
              .map(([k, v]) => `${k}=${v}`)
              .join(', ')}`}
          {wf.model_policy.allowed && ` · allowed ${wf.model_policy.allowed.join(', ')}`}
        </p>
      )}
      {wf.escalations && wf.escalations.length > 0 && <Escalations list={wf.escalations} />}
      <div className="flex flex-wrap items-center gap-2">
        <Button
          size="sm"
          variant="outline"
          data-delegation-verb="confirm"
          data-workflow={wf.id}
          disabled={busy || disabledReason !== undefined}
          onClick={onConfirm}
        >
          Confirm as shown
        </Button>
      </div>
      {refusal && <DecisionError message={refusal} />}
      <details>
        <summary className="cursor-pointer text-xs text-neutral-600 dark:text-neutral-400">
          Propose lower
        </summary>
        <div className="mt-2">
          <DelegationLowerForm
            owner={owner}
            name={name}
            workflow={wf}
            disabledReason={disabledReason}
            onFiled={onFiled}
          />
        </div>
      </details>
    </article>
  );
}

function StatusBadge({ workflow, status }: { workflow: string; status: DelegationWorkflowStatus }) {
  const base = 'inline-block rounded px-1.5 py-0.5 text-xs font-medium';
  const testid = `delegation-status-${workflow}`;
  if (status.status === 'confirmed') {
    return (
      <span
        data-testid={testid}
        data-status="confirmed"
        className={`${base} bg-emerald-100 text-emerald-900 dark:bg-emerald-900/40 dark:text-emerald-200`}
      >
        confirmed
        {status.confirmation &&
          ` by ${status.confirmation.subject} (#${status.confirmation.sequence})`}
      </span>
    );
  }
  if (status.status === 'no_captain') {
    return (
      <span
        data-testid={testid}
        data-status="no_captain"
        className={`${base} bg-neutral-100 text-neutral-800 dark:bg-neutral-800 dark:text-neutral-200`}
      >
        no captain — nobody holds the seat to confirm
      </span>
    );
  }
  const why =
    status.reason === 'handover'
      ? 'the seat changed hands since the last confirmation'
      : status.reason === 'hash_stale'
        ? 'the delegation changed since it was confirmed'
        : 'never confirmed for this seat';
  return (
    <span
      data-testid={testid}
      data-status="unconfirmed"
      className={`${base} bg-amber-100 text-amber-900 dark:bg-amber-900/40 dark:text-amber-200`}
    >
      unconfirmed — {why}
    </span>
  );
}

function Matrix({ actions }: { actions: RepoDelegationAction[] }) {
  if (actions.length === 0) {
    return <p className="text-xs text-neutral-500">No action classes resolved.</p>;
  }
  return (
    <table className="w-full text-left text-xs">
      <thead className="text-neutral-500">
        <tr>
          <th className="py-1 pr-3 font-medium">Action</th>
          <th className="py-1 pr-3 font-medium">Mode</th>
          <th className="py-1 pr-3 font-medium">Condition</th>
          <th className="py-1 font-medium">Provenance</th>
        </tr>
      </thead>
      <tbody>
        {actions.map((a) => (
          <tr key={a.action} className="border-t border-neutral-100 dark:border-neutral-900">
            <td className="py-1 pr-3 font-mono">{a.action}</td>
            <td className="py-1 pr-3">{a.mode}</td>
            <td className="py-1 pr-3">
              {[a.condition, a.min_severity && `≥ ${a.min_severity}`].filter(Boolean).join(' ') ||
                '—'}
            </td>
            <td className="py-1">{a.source}</td>
          </tr>
        ))}
      </tbody>
    </table>
  );
}

function Escalations({ list }: { list: RepoDelegationEscalation[] }) {
  return (
    <div className="space-y-2">
      <div className="text-xs font-medium">Escalations</div>
      {list.map((e, i) => (
        <div
          key={i}
          className="space-y-1 border-l border-neutral-200 pl-3 text-xs dark:border-neutral-800"
        >
          <p>
            Match:{' '}
            <span className="font-mono">
              {Object.entries(e.match)
                .map(([k, v]) => `${k}=${(v ?? []).join('|')}`)
                .join(' · ') || 'any'}
            </span>
            {e.max_autonomy && ` · max autonomy ${e.max_autonomy}`}
          </p>
          {e.approvals && (
            <p>
              Approvals: {e.approvals.count ?? 1}
              {e.approvals.member_of && ` from ${e.approvals.member_of}`}
              {e.approvals.min_permission && ` (≥ ${e.approvals.min_permission})`}
            </p>
          )}
          {e.ceiling_matrix && e.ceiling_matrix.length > 0 && (
            <>
              <p className="text-neutral-500">Ceiling matrix (declarative):</p>
              <Matrix actions={e.ceiling_matrix} />
            </>
          )}
        </div>
      ))}
    </div>
  );
}
