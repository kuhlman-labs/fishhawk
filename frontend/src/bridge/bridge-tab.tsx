import { useState } from 'react';
import { api } from '@/api/client';
import { useAsync, type AsyncState } from '@/api/use-async';
import type { CaptainDelegationUnconfirmed, CaptainResponse } from '@/api/captain';
import { CaptainPanel } from './captain-panel';
import { DelegationPanel } from './delegation-panel';
import { HandoverPanel } from './handover-panel';
import { shortHash } from './viewer';

/*
 * The repo dashboard's Bridge tab (E76.6 / #3769), at
 * /repos/:owner/:name?tab=bridge: change of command in one place.
 *
 * ONE GET /v0/captain read is shared by the captain panel, the handover
 * panel and the unconfirmed-delegation badge; a resolved captain verb
 * re-derives it (and the brief). A SEAT CHANGE (accept, claim, relinquish)
 * additionally bumps the delegation panel's `generation`, because the server
 * voids every earlier delegation confirmation at a seat change and the
 * verdicts must be re-read rather than keep a stale "confirmed" badge.
 *
 * Every panel owns its own loading and error surface, so one failing read
 * never blanks the tab — the Overview tab's independent-fetch posture.
 */
export function BridgeTab({ repo, owner, name }: { repo: string; owner: string; name: string }) {
  const [captainGen, setCaptainGen] = useState(0);
  const [seatGen, setSeatGen] = useState(0);
  const captain = useAsync(() => api.getCaptain(repo), [repo, captainGen]);
  const brief = useAsync(() => api.getHandoverBrief({ repo }), [repo, captainGen]);

  return (
    <div className="space-y-4">
      <UnconfirmedDelegationBadge state={captain} />
      <CaptainPanel state={captain} />
      <HandoverPanel
        repo={repo}
        state={captain}
        brief={brief}
        reload={() => setCaptainGen((n) => n + 1)}
        onSeatChange={() => setSeatGen((n) => n + 1)}
      />
      <DelegationPanel owner={owner} name={name} generation={seatGen} />
    </div>
  );
}

/*
 * The unconfirmed-delegation badge from `CaptainResponse.delegation_unconfirmed`.
 * FAIL CLOSED: `workflows` is EMPTY when `inventory_unavailable` or
 * `unavailable` is set, and the block is absent when the confirmation store is
 * not wired — in every one of those cases the badge names the unavailability
 * and NEVER renders the zero-unconfirmed all-clear. The block is computed over
 * the CACHED spec (`source: run_cache`) and does not report hash staleness
 * (`hash_staleness_reported: false`), so even the all-clear says so and
 * defers to the delegation section's live read.
 */
export function UnconfirmedDelegationBadge({ state }: { state: AsyncState<CaptainResponse> }) {
  // The captain panel already renders the loading and error states of this
  // same read; the badge adds nothing to them.
  if (state.status !== 'ok') return null;
  return <BadgeBody block={state.data.delegation_unconfirmed} />;
}

const AMBER =
  'rounded-md border border-amber-300 bg-amber-50 px-3 py-2 text-sm text-amber-900 dark:border-amber-900/60 dark:bg-amber-950/40 dark:text-amber-200';
const NEUTRAL =
  'rounded-md border border-neutral-200 px-3 py-2 text-sm text-neutral-700 dark:border-neutral-800 dark:text-neutral-300';

function BadgeBody({ block }: { block: CaptainDelegationUnconfirmed | undefined }) {
  if (block === undefined) {
    return (
      <p role="status" data-testid="unconfirmed-delegation" className={NEUTRAL}>
        Unconfirmed delegation is not reported: no delegation-confirmation store on this instance.
      </p>
    );
  }
  if (block.unavailable) {
    return (
      <p role="status" data-testid="unconfirmed-delegation" className={AMBER}>
        Unconfirmed delegation could not be read ({block.unavailable}); this is not an all-clear.
      </p>
    );
  }
  if (block.inventory_unavailable) {
    return (
      <p role="status" data-testid="unconfirmed-delegation" className={AMBER}>
        Unconfirmed delegation could not be counted: the workflow inventory is unavailable (
        {block.inventory_unavailable}); this is not an all-clear.
      </p>
    );
  }
  const spec = block.workflow_sha ? ` at ${shortHash(block.workflow_sha)}` : '';
  if (block.workflows.length === 0) {
    return (
      <p role="status" data-testid="unconfirmed-delegation" className={NEUTRAL}>
        Every workflow in the cached spec{spec} is confirmed for the seat in force (#
        {block.seat_sequence}). Hash staleness is not checked here — the delegation section is the
        live read.
      </p>
    );
  }
  return (
    <p role="status" data-testid="unconfirmed-delegation" className={AMBER}>
      {block.workflows.length} {block.workflows.length === 1 ? 'workflow' : 'workflows'} unconfirmed
      for the seat in force (#{block.seat_sequence}
      {block.unconfirmed_since ? `, since ${block.unconfirmed_since}` : ''}):{' '}
      <span className="font-mono">{block.workflows.join(', ')}</span>.
    </p>
  );
}
