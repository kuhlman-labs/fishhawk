import { useState } from 'react';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { fireEvent, render, screen, waitFor, within } from '@testing-library/react';
import { MemoryRouter } from 'react-router';
import { api } from '@/api/client';
import { useAsync, type AsyncState } from '@/api/use-async';
import type { CaptainOffer, CaptainRecord, CaptainResponse, HandoverBrief } from '@/api/captain';
import { AuthContext } from '@/auth/auth-context';
import type { AuthContextValue } from '@/auth/types';
import { HandoverPanel } from './handover-panel';

/*
 * The handover write surface driven through the REAL api client over a
 * stubbed fetch. Every fixture is a literal `CaptainResponse` /
 * `CaptainOffer` wire body. The non-gate (counterfactual 7) is pinned
 * directly: a viewer whose login does not match the gitlab-qualified captain
 * still sees every state-admitted verb ENABLED, and a click still POSTs.
 */

const REPO = 'acme/app';

const CAPTAIN: CaptainRecord = {
  subject: 'gitlab:someone',
  identity_verified: true,
  claim_verified: null,
  basis: 'assigned',
  assigned_sequence: 5,
  assigned_entry_hash: 'a'.repeat(64),
  assigned_at: '2026-09-20T12:00:00Z',
};

const OFFER: CaptainOffer = {
  successor: 'gitlab:other',
  identity_verified: true,
  offered_by: 'gitlab:someone',
  offer_entry_hash: 'f'.repeat(64),
  offered_sequence: 9,
  offered_at: '2026-09-21T12:00:00Z',
  brief_hash: 'b'.repeat(64),
  brief_from_sequence: 6,
  brief_to_sequence: 9,
};

function record(over: Partial<CaptainResponse> = {}): CaptainResponse {
  return {
    repo: REPO,
    captain: CAPTAIN,
    pending_offer: null,
    last_captain: null,
    history: [],
    history_total: 1,
    skipped_entries: 0,
    ...over,
  };
}

const BRIEF: HandoverBrief = {
  repo: REPO,
  window: { from_sequence: 6, to_sequence: 9, chain_head: 9, basis: 'since_last_handover' },
  sections: [],
  absent: [],
  gaps: [],
  degradations: [],
  gaps_truncated: false,
  gaps_omitted_count: 0,
  brief_hash: 'b'.repeat(64),
  truncated: false,
};

interface Stub {
  fetchMock: ReturnType<typeof vi.fn>;
  posts: () => Array<{ path: string; body: unknown }>;
  captainGets: () => number;
}

/** GET /v0/captain serves `rec`; a verb POST answers `verbStatus` / `verbBody`. */
function stub(rec: CaptainResponse, verbStatus = 200, verbBody?: unknown): Stub {
  const fetchMock = vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
    const url = new URL(String(input), 'http://localhost');
    const respond = (b: unknown, status = 200) =>
      new Response(JSON.stringify(b), { status, headers: { 'Content-Type': 'application/json' } });
    if (url.pathname === '/v0/captain' && (init?.method ?? 'GET') === 'GET') {
      return respond(rec);
    }
    if (url.pathname.startsWith('/v0/captain/') && init?.method === 'POST') {
      return respond(
        verbBody ?? { repo: REPO, event: {}, captain: rec.captain, pending_offer: null },
        verbStatus,
      );
    }
    return respond({ error: 'not_found' }, 404);
  });
  vi.stubGlobal('fetch', fetchMock);
  return {
    fetchMock,
    posts: () =>
      fetchMock.mock.calls
        .filter(([, init]) => (init as RequestInit | undefined)?.method === 'POST')
        .map(([input, init]) => ({
          path: new URL(String(input), 'http://localhost').pathname,
          body: JSON.parse(String((init as RequestInit).body)) as unknown,
        })),
    captainGets: () =>
      fetchMock.mock.calls.filter(
        ([input, init]) =>
          new URL(String(input), 'http://localhost').pathname === '/v0/captain' &&
          ((init as RequestInit | undefined)?.method ?? 'GET') === 'GET',
      ).length,
  };
}

function authAs(login: string): AuthContextValue {
  return {
    status: 'authenticated',
    user: { id: 'u1', github_login: login, name: login, email: null, account_id: null },
    reload: async () => {},
    signOut: async () => {},
  };
}

function Harness({
  onSeatChange,
  brief,
}: {
  onSeatChange?: () => void;
  brief: AsyncState<HandoverBrief>;
}) {
  const [n, setN] = useState(0);
  const state = useAsync(() => api.getCaptain(REPO), [n]);
  return (
    <HandoverPanel
      repo={REPO}
      state={state}
      brief={brief}
      reload={() => setN((x) => x + 1)}
      onSeatChange={onSeatChange}
    />
  );
}

function mount({
  login = 'octo',
  onSeatChange,
  brief = { status: 'ok', data: BRIEF },
}: {
  login?: string;
  onSeatChange?: () => void;
  brief?: AsyncState<HandoverBrief>;
} = {}) {
  return render(
    <MemoryRouter>
      <AuthContext.Provider value={authAs(login)}>
        <Harness onSeatChange={onSeatChange} brief={brief} />
      </AuthContext.Provider>
    </MemoryRouter>,
  );
}

function verb(name: string): HTMLButtonElement {
  return document.querySelector(`[data-captain-verb="${name}"]`) as HTMLButtonElement;
}

async function loaded() {
  await waitFor(() => expect(screen.queryByText('Loading handover…')).toBeNull());
}

afterEach(() => vi.unstubAllGlobals());

describe('<HandoverPanel> verbs by record state', () => {
  it('a seated captain offers Offer + Relinquish; no Accept/Withdraw/Claim', async () => {
    stub(record());
    mount();
    await loaded();
    expect(verb('offer')).toBeInTheDocument();
    expect(verb('relinquish')).toBeInTheDocument();
    expect(verb('accept')).toBeNull();
    expect(verb('withdraw')).toBeNull();
    expect(verb('claim')).toBeNull();
  });

  it('offer is disabled while the successor is blank, then POSTs {repo, successor} and re-fetches', async () => {
    const s = stub(record());
    const onSeatChange = vi.fn();
    mount({ onSeatChange });
    await loaded();
    expect(verb('offer')).toBeDisabled();
    fireEvent.change(screen.getByLabelText('Successor subject'), { target: { value: '   ' } });
    expect(verb('offer')).toBeDisabled();
    fireEvent.change(screen.getByLabelText('Successor subject'), {
      target: { value: ' gitlab:next ' },
    });
    const before = s.captainGets();
    fireEvent.click(verb('offer'));
    await waitFor(() => expect(s.captainGets()).toBe(before + 1));
    expect(s.posts()).toEqual([
      { path: '/v0/captain/offer', body: { repo: REPO, successor: 'gitlab:next' } },
    ]);
    // An offer moves no seat.
    expect(onSeatChange).not.toHaveBeenCalled();
  });

  it('relinquish POSTs {repo}, re-fetches, and fires onSeatChange', async () => {
    const s = stub(record());
    const onSeatChange = vi.fn();
    mount({ onSeatChange });
    await loaded();
    const before = s.captainGets();
    fireEvent.click(verb('relinquish'));
    await waitFor(() => expect(s.captainGets()).toBe(before + 1));
    expect(s.posts()).toEqual([{ path: '/v0/captain/relinquish', body: { repo: REPO } }]);
    expect(onSeatChange).toHaveBeenCalledTimes(1);
  });

  it('a pending offer offers Accept + Withdraw with the offer details in view', async () => {
    stub(record({ pending_offer: OFFER }));
    mount();
    await loaded();
    const offer = within(screen.getByTestId('pending-offer'));
    expect(offer.getByText('gitlab:other')).toBeInTheDocument();
    expect(offer.getByText('2026-09-21T12:00:00Z')).toBeInTheDocument();
    expect(offer.getByText(`#9 · ${'f'.repeat(12)}…`)).toBeInTheDocument();
    expect(verb('accept')).toBeEnabled();
    expect(verb('withdraw')).toBeEnabled();
    // The brief renders IN VIEW next to Accept.
    expect(screen.getByRole('heading', { name: 'Handover brief' })).toBeInTheDocument();
  });

  it('accept POSTs {repo}, re-fetches and fires onSeatChange', async () => {
    const s = stub(record({ pending_offer: OFFER }));
    const onSeatChange = vi.fn();
    mount({ onSeatChange });
    await loaded();
    const before = s.captainGets();
    fireEvent.click(verb('accept'));
    await waitFor(() => expect(s.captainGets()).toBe(before + 1));
    expect(s.posts()).toEqual([{ path: '/v0/captain/accept', body: { repo: REPO } }]);
    expect(onSeatChange).toHaveBeenCalledTimes(1);
  });

  it('withdraw POSTs {repo}, re-fetches and does NOT fire onSeatChange', async () => {
    const s = stub(record({ pending_offer: OFFER }));
    const onSeatChange = vi.fn();
    mount({ onSeatChange });
    await loaded();
    const before = s.captainGets();
    fireEvent.click(verb('withdraw'));
    await waitFor(() => expect(s.captainGets()).toBe(before + 1));
    expect(s.posts()).toEqual([{ path: '/v0/captain/withdraw', body: { repo: REPO } }]);
    expect(onSeatChange).not.toHaveBeenCalled();
  });

  it('a vacant seat offers Claim with the predicate consequence stated', async () => {
    const s = stub(record({ captain: null, last_captain: 'gitlab:former' }));
    const onSeatChange = vi.fn();
    mount({ onSeatChange });
    await loaded();
    expect(screen.getByText(/admits the claim with claim_verified false/)).toBeInTheDocument();
    expect(verb('offer')).toBeNull();
    expect(verb('relinquish')).toBeNull();
    const before = s.captainGets();
    fireEvent.click(verb('claim'));
    await waitFor(() => expect(s.captainGets()).toBe(before + 1));
    expect(s.posts()).toEqual([{ path: '/v0/captain/claim', body: { repo: REPO } }]);
    expect(onSeatChange).toHaveBeenCalledTimes(1);
  });
});

describe('<HandoverPanel> refusals', () => {
  it.each([
    ['relinquish', 403, 'captain_not_captain', 'you are not it', {}],
    ['withdraw', 403, 'captain_not_offerer', 'Only the captain who made the pending offer', OFFER],
    ['accept', 403, 'captain_offer_successor_mismatch', 'only its successor can accept it', OFFER],
    ['accept', 409, 'captain_no_offer', 'No handover offer is pending', OFFER],
    ['relinquish', 409, 'captain_no_captain', 'The seat is vacant', {}],
    ['accept', 501, 'captain_unconfigured', 'No captain record on this instance.', OFFER],
    ['accept', 403, 'captain_agent_identity_refused', 'only a human can act here', OFFER],
  ] as const)(
    '%s refused with %i %s renders the named message AND the raw code, with no re-fetch',
    async (name, status, code, sentence, offer) => {
      const s = stub(record({ pending_offer: 'successor' in offer ? offer : null }), status, {
        error: code,
        message: 'server says no',
      });
      const onSeatChange = vi.fn();
      mount({ onSeatChange });
      await loaded();
      const before = s.captainGets();
      fireEvent.click(verb(name));
      const alert = await screen.findByRole('alert');
      expect(alert).toHaveTextContent(sentence);
      expect(alert).toHaveTextContent(`${status} · ${code}`);
      expect(s.captainGets()).toBe(before);
      expect(onSeatChange).not.toHaveBeenCalled();
      // The verb stays available for another attempt.
      expect(verb(name)).toBeEnabled();
    },
  );

  it('offer refused with 409 captain_self_handover renders its message', async () => {
    stub(record(), 409, { error: 'captain_self_handover', message: 'x' });
    mount();
    await loaded();
    fireEvent.change(screen.getByLabelText('Successor subject'), {
      target: { value: 'gitlab:someone' },
    });
    fireEvent.click(verb('offer'));
    const alert = await screen.findByRole('alert');
    expect(alert).toHaveTextContent('cannot be offered to the captain already holding it');
    expect(alert).toHaveTextContent('409 · captain_self_handover');
  });

  it.each([
    [409, 'captain_exists', 'The seat is held'],
    [403, 'captain_predicate_rejected', 'do not satisfy'],
    [422, 'captain_predicate_undeterminable', 'could not be determined'],
  ] as const)('claim refused with %i %s renders its message', async (status, code, sentence) => {
    const onSeatChange = vi.fn();
    stub(record({ captain: null }), status, { error: code, message: 'x' });
    mount({ onSeatChange });
    await loaded();
    fireEvent.click(verb('claim'));
    const alert = await screen.findByRole('alert');
    expect(alert).toHaveTextContent(sentence);
    expect(alert).toHaveTextContent(`${status} · ${code}`);
    expect(onSeatChange).not.toHaveBeenCalled();
  });

  it('an UNMAPPED refusal renders the `{status} · {error}` fallback', async () => {
    stub(record({ captain: null }), 418, { error: 'captain_brand_new_refusal', message: 'x' });
    mount();
    await loaded();
    fireEvent.click(verb('claim'));
    expect(await screen.findByRole('alert')).toHaveTextContent('418 · captain_brand_new_refusal');
  });

  it('a 501 captain_unconfigured READ renders the named unavailability, no verbs', async () => {
    const fetchMock = vi.fn(
      async () =>
        new Response(JSON.stringify({ error: 'captain_unconfigured', message: 'x' }), {
          status: 501,
          headers: { 'Content-Type': 'application/json' },
        }),
    );
    vi.stubGlobal('fetch', fetchMock);
    mount();
    expect(
      await screen.findByText(/Handover is unavailable: no captain record on this instance/),
    ).toBeInTheDocument();
    expect(verb('claim')).toBeNull();
  });
});

describe('<HandoverPanel> adds no authority (counterfactual 7)', () => {
  it('a non-matching viewer sees every seated verb ENABLED and a click POSTs', async () => {
    const s = stub(record({ pending_offer: OFFER }));
    // Session login `octo` matches neither gitlab:someone nor gitlab:other.
    mount({ login: 'octo' });
    await loaded();
    expect(screen.queryByTestId('you-marker')).toBeNull();
    for (const name of ['accept', 'withdraw', 'relinquish']) {
      expect(verb(name)).toBeEnabled();
    }
    fireEvent.change(screen.getByLabelText('Successor subject'), { target: { value: 'gitlab:x' } });
    expect(verb('offer')).toBeEnabled();
    fireEvent.click(verb('accept'));
    await waitFor(() =>
      expect(s.posts()).toEqual([{ path: '/v0/captain/accept', body: { repo: REPO } }]),
    );
  });

  it('a non-matching viewer on a vacant seat can still click Claim', async () => {
    const s = stub(record({ captain: null }));
    mount({ login: 'nobody' });
    await loaded();
    expect(verb('claim')).toBeEnabled();
    fireEvent.click(verb('claim'));
    await waitFor(() => expect(s.posts()).toHaveLength(1));
  });

  it('a matching viewer only GAINS the (you) marker — the verb set is identical', async () => {
    stub(record({ pending_offer: OFFER }));
    mount({ login: 'other' });
    await loaded();
    const offer = within(screen.getByTestId('pending-offer'));
    expect(offer.getByTestId('you-marker')).toBeInTheDocument();
    for (const name of ['accept', 'withdraw', 'relinquish']) {
      expect(verb(name)).toBeEnabled();
    }
  });
});

describe('<HandoverPanel> the offer brief', () => {
  it('brief_unavailable renders the recorded reason and the no-recorded-hash line (counterfactual 5)', async () => {
    stub(
      record({
        pending_offer: {
          ...OFFER,
          brief_hash: undefined,
          brief_from_sequence: undefined,
          brief_to_sequence: undefined,
          brief_unavailable: true,
          brief_unavailable_reason: 'window_read_failed',
        },
      }),
    );
    mount();
    await loaded();
    const line = screen.getByTestId('offer-brief');
    expect(line).toHaveTextContent('The offer committed without a recorded brief hash');
    expect(line).toHaveTextContent('window_read_failed');
    expect(line).not.toHaveTextContent('predates');
  });

  it('a recorded hash equal to the live brief says so', async () => {
    stub(record({ pending_offer: OFFER }));
    mount();
    await loaded();
    const line = screen.getByTestId('offer-brief');
    expect(line).toHaveTextContent(`Offered with brief ${'b'.repeat(12)}…`);
    expect(line).toHaveTextContent('(window #6 → #9)');
    expect(line).toHaveTextContent('The live brief below carries the same hash.');
  });

  it('a recorded hash differing from the live brief is flagged as movement', async () => {
    stub(record({ pending_offer: OFFER }));
    mount({ brief: { status: 'ok', data: { ...BRIEF, brief_hash: 'c'.repeat(64) } } });
    await loaded();
    const line = screen.getByTestId('offer-brief');
    expect(line).toHaveTextContent('The brief has moved since the offer');
    expect(line).toHaveTextContent(`${'c'.repeat(12)}…`);
    expect(line).not.toHaveTextContent('same hash');
  });

  it('a pre-E76.4 offer with no hash and no unavailability says it predates hashes', async () => {
    stub(
      record({
        pending_offer: { ...OFFER, brief_hash: undefined },
      }),
    );
    mount();
    await loaded();
    expect(screen.getByTestId('offer-brief')).toHaveTextContent(
      'This offer predates recorded brief hashes.',
    );
  });
});

/*
 * Approval condition 1: a seat change voids every earlier delegation
 * confirmation server-side, so whatever renders the verdicts must re-read
 * them after a resolved accept. The delegation panel itself is a sibling
 * slice; this pins the HandoverPanel half of the contract — `onSeatChange`
 * is what drives the re-read — through a verdict reader wired exactly as the
 * tab wires it (a counter bumped by onSeatChange feeding the confirmation
 * GET). The mocked confirmation read flips confirmed → unconfirmed once the
 * accept lands; dropping the onSeatChange call leaves the stale badge.
 */
describe('<HandoverPanel> seat change refreshes delegation verdicts (condition 1)', () => {
  function Verdicts({ generation }: { generation: number }) {
    const state = useAsync(() => api.getRepoDelegationConfirmation('acme', 'app'), [generation]);
    if (state.status !== 'ok') return null;
    return (
      <ul>
        {state.data.workflows.map((w) => (
          <li key={w.workflow} data-testid={`verdict-${w.workflow}`}>
            {w.status}
            {w.reason ? ` (${w.reason})` : ''}
          </li>
        ))}
      </ul>
    );
  }

  function Tab() {
    const [n, setN] = useState(0);
    const [gen, setGen] = useState(0);
    const state = useAsync(() => api.getCaptain(REPO), [n]);
    return (
      <>
        <HandoverPanel
          repo={REPO}
          state={state}
          brief={{ status: 'ok', data: BRIEF }}
          reload={() => setN((x) => x + 1)}
          onSeatChange={() => setGen((x) => x + 1)}
        />
        <Verdicts generation={gen} />
      </>
    );
  }

  it('accept flips a confirmed badge to unconfirmed (handover)', async () => {
    let accepted = false;
    const fetchMock = vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      const p = new URL(String(input), 'http://localhost').pathname;
      const respond = (b: unknown) =>
        new Response(JSON.stringify(b), { headers: { 'Content-Type': 'application/json' } });
      if (p === '/v0/captain/accept' && init?.method === 'POST') {
        accepted = true;
        return respond({ repo: REPO, event: {}, captain: CAPTAIN, pending_offer: null });
      }
      if (p === '/v0/captain') {
        return respond(record({ pending_offer: accepted ? null : OFFER }));
      }
      if (p === '/v0/repos/acme/app/delegation/confirmation') {
        return respond({
          repo: REPO,
          source: 'ref',
          captain: accepted ? 'gitlab:other' : 'gitlab:someone',
          seat_sequence: accepted ? 10 : 5,
          workflows: [
            accepted
              ? { workflow: 'feature_change', status: 'unconfirmed', reason: 'handover' }
              : { workflow: 'feature_change', status: 'confirmed' },
          ],
          unconfirmed_workflows: accepted ? ['feature_change'] : [],
          skipped_entries: 0,
          ignored_entries: 0,
        });
      }
      return respond({});
    });
    vi.stubGlobal('fetch', fetchMock);
    render(
      <MemoryRouter>
        <AuthContext.Provider value={authAs('other')}>
          <Tab />
        </AuthContext.Provider>
      </MemoryRouter>,
    );
    expect(await screen.findByTestId('verdict-feature_change')).toHaveTextContent('confirmed');
    await loaded();
    fireEvent.click(verb('accept'));
    await waitFor(() =>
      expect(screen.getByTestId('verdict-feature_change')).toHaveTextContent(
        'unconfirmed (handover)',
      ),
    );
  });
});
