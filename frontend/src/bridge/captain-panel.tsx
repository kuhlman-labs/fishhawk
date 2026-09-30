import type { AsyncState } from '@/api/use-async';
import type { CaptainRecord, CaptainResponse } from '@/api/captain';
import { RepoPanelFrame } from '@/repo/throughput-panel';
import { apiErrorCode, shortHash } from './viewer';

/*
 * The "current captain" read on the Bridge tab (E76.6 / #3769). Takes the
 * already-fetched GET /v0/captain state — the tab fetches it ONCE and shares
 * it with the handover panel — and renders it inside the shared
 * RepoPanelFrame shell.
 *
 * `identity_verified` and `claim_verified` are TWO SEPARATE verifications and
 * render as two separate lines: identity is whether the subject is
 * provider-qualified; claim is whether a claimed seat was admitted on a
 * non-trivial approval predicate. A 501 `captain_unconfigured` is an expected
 * state on a partially-configured instance and renders as that, not as the
 * generic panel error.
 */

// `truncated?: false` satisfies RepoPanelFrame's weak-type constraint without
// ever triggering its run-scan partial-data note.
type CaptainView = ({ kind: 'record'; record: CaptainResponse } | { kind: 'unconfigured' }) & {
  truncated?: false;
};

function toView(state: AsyncState<CaptainResponse>): AsyncState<CaptainView> {
  if (state.status === 'error' && apiErrorCode(state.error) === 'captain_unconfigured') {
    return { status: 'ok', data: { kind: 'unconfigured' } };
  }
  if (state.status === 'ok') return { status: 'ok', data: { kind: 'record', record: state.data } };
  return state;
}

export function CaptainPanel({ state }: { state: AsyncState<CaptainResponse> }) {
  return (
    <RepoPanelFrame title="Captain" state={toView(state)}>
      {(view) =>
        view.kind === 'unconfigured' ? (
          <p className="text-sm text-neutral-600 dark:text-neutral-400" role="note">
            No captain record on this instance (captain_unconfigured).
          </p>
        ) : (
          <CaptainBody record={view.record} />
        )
      }
    </RepoPanelFrame>
  );
}

function CaptainBody({ record }: { record: CaptainResponse }) {
  return (
    <div className="space-y-3">
      {record.captain ? (
        <Seated captain={record.captain} />
      ) : (
        <Vacancy last={record.last_captain} />
      )}
      {record.skipped_entries > 0 && (
        <p
          role="note"
          className="rounded-md border border-amber-300 bg-amber-50 px-3 py-2 text-xs text-amber-900 dark:border-amber-900/60 dark:bg-amber-950/40 dark:text-amber-200"
        >
          Partial fold: {record.skipped_entries} captain{' '}
          {record.skipped_entries === 1 ? 'entry' : 'entries'} could not be used (undecodable or
          another repository) and {record.skipped_entries === 1 ? 'was' : 'were'} skipped, never
          guessed at.
        </p>
      )}
      <p className="text-xs text-neutral-500 dark:text-neutral-400">
        {record.history_total} captain {record.history_total === 1 ? 'entry' : 'entries'} on the
        chain for {record.repo}.
      </p>
    </div>
  );
}

function Seated({ captain }: { captain: CaptainRecord }) {
  return (
    <dl className="grid gap-2 text-sm sm:grid-cols-[max-content_1fr] sm:gap-x-4">
      <dt className="text-neutral-500">Captain</dt>
      <dd className="font-mono break-all">{captain.subject}</dd>
      <dt className="text-neutral-500">Captain since</dt>
      <dd>
        <time dateTime={captain.assigned_at}>{captain.assigned_at}</time>{' '}
        <span className="font-mono text-xs text-neutral-500">
          #{captain.assigned_sequence} · {shortHash(captain.assigned_entry_hash)}
        </span>
      </dd>
      <dt className="text-neutral-500">Basis</dt>
      <dd>{captain.basis === 'assigned' ? 'Handed over' : 'Claimed while vacant'}</dd>
      <dt className="text-neutral-500">Identity</dt>
      <dd>
        <IdentityLine verified={captain.identity_verified} />
      </dd>
      <dt className="text-neutral-500">Claim</dt>
      <dd>
        <ClaimLine verified={captain.claim_verified} />
      </dd>
    </dl>
  );
}

export function IdentityLine({ verified }: { verified: boolean }) {
  if (verified) {
    return <span data-testid="identity-verified">Provider-verified identity</span>;
  }
  return (
    <span>
      <span
        data-testid="identity-unverified"
        className="mr-2 inline-block rounded bg-amber-100 px-1.5 py-0.5 text-xs font-medium text-amber-900 dark:bg-amber-900/40 dark:text-amber-200"
      >
        identity not provider-verified
      </span>
      <span className="text-xs text-neutral-500">
        a static-token subject carries false: it is not github:/gitlab: qualified
      </span>
    </span>
  );
}

function ClaimLine({ verified }: { verified: boolean | null }) {
  if (verified === null) {
    return <span>Not applicable — the seat was handed over</span>;
  }
  if (verified) {
    return <span>A non-trivial approval predicate was checked and satisfied</span>;
  }
  return (
    <span className="text-amber-900 dark:text-amber-200">
      Admitted on a positively trivial approval predicate
    </span>
  );
}

function Vacancy({ last }: { last: string | null }) {
  return (
    <div
      role="status"
      className="rounded-md border border-amber-300 bg-amber-50 px-3 py-2 text-sm text-amber-900 dark:border-amber-900/60 dark:bg-amber-950/40 dark:text-amber-200"
    >
      <div className="font-medium">The captain seat is vacant.</div>
      {last ? (
        <div className="mt-1">
          Last captain: <span className="font-mono">{last}</span>
        </div>
      ) : (
        <div className="mt-1">No captain has ever sat for this repository.</div>
      )}
    </div>
  );
}
