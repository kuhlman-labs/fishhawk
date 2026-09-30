import { useContext, useState, type ReactNode } from 'react';
import { api } from '@/api/client';
import type { AsyncState } from '@/api/use-async';
import type { CaptainOffer, CaptainResponse, HandoverBrief } from '@/api/captain';
import { AuthContext } from '@/auth/auth-context';
import { Button } from '@/components/ui/button';
import { DecisionError, TextField, useDecisionSubmit } from '@/attention/decision-form';
import { RepoPanelFrame } from '@/repo/throughput-panel';
import { IdentityLine } from './captain-panel';
import { HandoverBriefPanel } from './handover-brief';
import { apiErrorCode, captainRefusalMessage, shortHash, viewerMatchesSubject } from './viewer';

/*
 * The handover write surface on the Bridge tab (E76.6 / #3769).
 *
 * Verbs are offered by the RECORD'S STATE, never by a client-side identity
 * check (see viewer.ts for why the SPA cannot know the viewer's subject):
 *
 *   seated          ⇒ Offer the seat (successor required) + Relinquish
 *   pending offer   ⇒ Accept + Withdraw, with the offer and the brief in view
 *   vacant          ⇒ Claim the seat, with the predicate consequence stated
 *
 * `viewerMatchesSubject` only adds a non-authoritative "(you)" marker; it
 * never disables or hides a button. Every refusal renders through
 * `captainRefusalMessage` (the mapped sentence AND the raw server code).
 *
 * On a RESOLVED verb only, `reload` re-derives the captain record. A SEAT
 * CHANGE (accept, claim, relinquish) additionally fires `onSeatChange`: the
 * server voids every earlier delegation confirmation at a seat change, so the
 * delegation panel must re-read its verdicts rather than keep a stale
 * "confirmed" badge. Offer and withdraw move no seat and do not fire it.
 */

export interface HandoverPanelProps {
  /** `owner/name`. */
  repo: string;
  /** The ONE GET /v0/captain read the tab shares with the captain panel. */
  state: AsyncState<CaptainResponse>;
  /** The GET /v0/handover-brief read, rendered in view next to Accept. */
  brief: AsyncState<HandoverBrief>;
  /** Re-fetch the captain record (and whatever else the tab derives from it). */
  reload: () => void;
  /** Fired after a resolved accept / claim / relinquish — never on a refusal. */
  onSeatChange?: () => void;
}

// `truncated?: false` satisfies RepoPanelFrame's weak-type constraint without
// ever triggering its run-scan partial-data note.
type HandoverView = ({ kind: 'record'; record: CaptainResponse } | { kind: 'unconfigured' }) & {
  truncated?: false;
};

function toView(state: AsyncState<CaptainResponse>): AsyncState<HandoverView> {
  if (state.status === 'error' && apiErrorCode(state.error) === 'captain_unconfigured') {
    return { status: 'ok', data: { kind: 'unconfigured' } };
  }
  if (state.status === 'ok') return { status: 'ok', data: { kind: 'record', record: state.data } };
  return state;
}

export function HandoverPanel({ repo, state, brief, reload, onSeatChange }: HandoverPanelProps) {
  const viewerLogin = useContext(AuthContext)?.user?.github_login ?? null;
  return (
    <div className="space-y-4">
      <RepoPanelFrame title="Handover" state={toView(state)}>
        {(view) =>
          view.kind === 'unconfigured' ? (
            <p className="text-sm text-neutral-600 dark:text-neutral-400" role="note">
              Handover is unavailable: no captain record on this instance (captain_unconfigured).
            </p>
          ) : (
            <HandoverBody
              repo={repo}
              record={view.record}
              brief={brief}
              viewerLogin={viewerLogin}
              reload={reload}
              onSeatChange={onSeatChange}
            />
          )
        }
      </RepoPanelFrame>
      <HandoverBriefPanel state={brief} />
    </div>
  );
}

function You({ show }: { show: boolean }) {
  if (!show) return null;
  return (
    <span data-testid="you-marker" className="ml-1 text-xs text-neutral-500">
      (you)
    </span>
  );
}

function HandoverBody({
  repo,
  record,
  brief,
  viewerLogin,
  reload,
  onSeatChange,
}: {
  repo: string;
  record: CaptainResponse;
  brief: AsyncState<HandoverBrief>;
  viewerLogin: string | null;
  reload: () => void;
  onSeatChange?: () => void;
}) {
  const { submitting, error, submit } = useDecisionSubmit(reload);
  const [successor, setSuccessor] = useState('');

  // One verb in flight at a time. The thrown error is re-thrown as the
  // refusal line so DecisionError renders the mapped sentence + raw code.
  function run(call: () => Promise<unknown>, seatChange: boolean, after?: () => void) {
    void submit(async () => {
      try {
        await call();
      } catch (err) {
        throw new Error(captainRefusalMessage(err), { cause: err });
      }
      after?.();
      if (seatChange) onSeatChange?.();
    });
  }

  const captain = record.captain;
  const offer = record.pending_offer;

  return (
    <div className="space-y-4 text-sm">
      {captain ? (
        <p>
          Seated captain: <span className="font-mono">{captain.subject}</span>
          <You show={viewerMatchesSubject(viewerLogin, captain.subject)} />
        </p>
      ) : (
        <p>The seat is vacant.</p>
      )}

      {offer && (
        <PendingOffer offer={offer} brief={brief} viewerLogin={viewerLogin}>
          <div className="flex flex-wrap gap-2">
            <Button
              size="sm"
              variant="outline"
              data-captain-verb="accept"
              disabled={submitting}
              onClick={() => run(() => api.captainAccept(repo), true)}
            >
              Accept the seat
            </Button>
            <Button
              size="sm"
              variant="outline"
              data-captain-verb="withdraw"
              disabled={submitting}
              onClick={() => run(() => api.captainWithdraw(repo), false)}
            >
              Withdraw the offer
            </Button>
          </div>
        </PendingOffer>
      )}

      {captain && (
        <div className="space-y-2">
          <TextField
            label="Successor subject"
            value={successor}
            onChange={setSuccessor}
            disabled={submitting}
          />
          <p className="text-xs text-neutral-500">
            A provider-qualified subject such as github:octo.
            {offer ? ' A new offer replaces the pending one.' : ''}
          </p>
          <div className="flex flex-wrap gap-2">
            <Button
              size="sm"
              variant="outline"
              data-captain-verb="offer"
              disabled={submitting || successor.trim() === ''}
              onClick={() =>
                run(
                  () => api.captainOffer({ repo, successor: successor.trim() }),
                  false,
                  () => setSuccessor(''),
                )
              }
            >
              Offer the seat
            </Button>
            <Button
              size="sm"
              variant="outline"
              data-captain-verb="relinquish"
              disabled={submitting}
              onClick={() => run(() => api.captainRelinquish(repo), true)}
            >
              Relinquish the seat
            </Button>
          </div>
        </div>
      )}

      {!captain && (
        <div className="space-y-2">
          <p className="text-xs text-neutral-600 dark:text-neutral-400">
            A claim is checked against this repository&apos;s approval predicate. A non-trivial
            predicate must be satisfied; a positively trivial one admits the claim with
            claim_verified false, and the seat records that.
          </p>
          <Button
            size="sm"
            variant="outline"
            data-captain-verb="claim"
            disabled={submitting}
            onClick={() => run(() => api.captainClaim(repo), true)}
          >
            Claim the seat
          </Button>
        </div>
      )}

      {error && <DecisionError message={error} />}
    </div>
  );
}

function PendingOffer({
  offer,
  brief,
  viewerLogin,
  children,
}: {
  offer: CaptainOffer;
  brief: AsyncState<HandoverBrief>;
  viewerLogin: string | null;
  children: ReactNode;
}) {
  return (
    <div
      data-testid="pending-offer"
      className="space-y-2 rounded-md border border-neutral-200 p-3 dark:border-neutral-800"
    >
      <div className="font-medium">A handover offer is pending.</div>
      <dl className="grid gap-1 sm:grid-cols-[max-content_1fr] sm:gap-x-4">
        <dt className="text-neutral-500">Successor</dt>
        <dd>
          <span className="font-mono">{offer.successor}</span>
          <You show={viewerMatchesSubject(viewerLogin, offer.successor)} />{' '}
          <IdentityLine verified={offer.identity_verified} />
        </dd>
        <dt className="text-neutral-500">Offered by</dt>
        <dd>
          <span className="font-mono">{offer.offered_by}</span>
          <You show={viewerMatchesSubject(viewerLogin, offer.offered_by)} />
        </dd>
        <dt className="text-neutral-500">Offered</dt>
        <dd>
          <time dateTime={offer.offered_at}>{offer.offered_at}</time>{' '}
          <span className="font-mono text-xs text-neutral-500">
            #{offer.offered_sequence} · {shortHash(offer.offer_entry_hash)}
          </span>
        </dd>
      </dl>
      <OfferBrief offer={offer} brief={brief} />
      {children}
    </div>
  );
}

/*
 * The offer's recorded brief versus the live one. `brief_unavailable` is
 * checked FIRST: such an offer committed with NO recorded hash, and must never
 * render as though one were recorded. A recorded hash that differs from the
 * live brief's is reported as MOVEMENT — the brief is a point-in-time
 * composition — never asserted to match.
 */
function OfferBrief({ offer, brief }: { offer: CaptainOffer; brief: AsyncState<HandoverBrief> }) {
  if (offer.brief_unavailable) {
    return (
      <p
        role="note"
        data-testid="offer-brief"
        className="text-xs text-amber-900 dark:text-amber-200"
      >
        The offer committed without a recorded brief hash: the brief could not be established (
        {offer.brief_unavailable_reason ?? 'no reason recorded'}).
      </p>
    );
  }
  if (!offer.brief_hash) {
    return (
      <p data-testid="offer-brief" className="text-xs text-neutral-500">
        This offer predates recorded brief hashes.
      </p>
    );
  }
  const live = brief.status === 'ok' ? brief.data.brief_hash : null;
  return (
    <p data-testid="offer-brief" className="text-xs">
      Offered with brief <span className="font-mono">{shortHash(offer.brief_hash)}</span>
      {offer.brief_from_sequence !== undefined && offer.brief_to_sequence !== undefined && (
        <span className="text-neutral-500">
          {' '}
          (window #{offer.brief_from_sequence} → #{offer.brief_to_sequence})
        </span>
      )}
      .{' '}
      {live === null ? (
        <span className="text-neutral-500">The live brief is not loaded to compare.</span>
      ) : live === offer.brief_hash ? (
        <span>The live brief below carries the same hash.</span>
      ) : (
        <span className="text-amber-900 dark:text-amber-200">
          The brief has moved since the offer (live:{' '}
          <span className="font-mono">{shortHash(live)}</span>).
        </span>
      )}
    </p>
  );
}
