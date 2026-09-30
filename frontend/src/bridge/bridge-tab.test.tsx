import { afterEach, describe, expect, it, vi } from 'vitest';
import { fireEvent, render, screen, waitFor, within } from '@testing-library/react';
import { MemoryRouter } from 'react-router';
import type {
  CaptainDelegationUnconfirmed,
  CaptainOffer,
  CaptainRecord,
  CaptainResponse,
  DelegationConfirmationResponse,
  HandoverBrief,
  RepoDelegation,
} from '@/api/captain';
import { AuthContext } from '@/auth/auth-context';
import type { AuthContextValue } from '@/auth/types';
import { BridgeTab } from './bridge-tab';

/*
 * The Bridge tab with its REAL panels over one stubbed fetch and the REAL api
 * client. Fixtures are literal wire bodies.
 *
 * Counterfactual (1): the unconfirmed-delegation unavailability guard. The
 * `workflows` list is EMPTY in both fixtures, exactly as in an all-confirmed
 * record, so only the guard tells them apart.
 * Approval condition 1: a resolved seat change re-reads the delegation
 * verdicts; the mocked confirmation read flips once the seat has changed.
 */

const REPO = 'acme/app';

const CAPTAIN: CaptainRecord = {
  subject: 'github:octo',
  identity_verified: true,
  claim_verified: null,
  basis: 'assigned',
  assigned_sequence: 5,
  assigned_entry_hash: 'a'.repeat(64),
  assigned_at: '2026-09-20T12:00:00Z',
};

const OFFER: CaptainOffer = {
  successor: 'github:hubot',
  identity_verified: true,
  offered_by: 'github:octo',
  offer_entry_hash: 'f'.repeat(64),
  offered_sequence: 9,
  offered_at: '2026-09-21T12:00:00Z',
  brief_hash: 'b'.repeat(64),
  brief_from_sequence: 6,
  brief_to_sequence: 9,
};

function captainBody(over: Partial<CaptainResponse> = {}): CaptainResponse {
  return {
    repo: REPO,
    captain: CAPTAIN,
    pending_offer: null,
    last_captain: null,
    history: [],
    history_total: 1,
    skipped_entries: 0,
    delegation_unconfirmed: {
      seat_sequence: 5,
      source: 'run_cache',
      workflow_sha: 'abcdef0123456789abcd',
      workflows: [],
      hash_staleness_reported: false,
    },
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

const VIEW: RepoDelegation = {
  repo: REPO,
  source: 'ref',
  ref: 'main',
  schema_major: 2,
  content_hash: 'v'.repeat(64),
  workflows: [
    {
      id: 'feature_change',
      autonomy: 'medium',
      matrix: [{ action: 'merge', mode: 'gated', source: 'tier' }],
      content_hash: 'c'.repeat(64),
    },
  ],
  confirmation: { captain: 'github:octo', seat_sequence: 5, unconfirmed_workflows: [] },
};

function verdictBody(seatChanged: boolean): DelegationConfirmationResponse {
  return {
    repo: REPO,
    source: 'ref',
    captain: seatChanged ? 'github:hubot' : 'github:octo',
    seat_sequence: seatChanged ? 10 : 5,
    workflows: [
      seatChanged
        ? { workflow: 'feature_change', status: 'unconfirmed', reason: 'handover' }
        : { workflow: 'feature_change', status: 'confirmed' },
    ],
    unconfirmed_workflows: seatChanged ? ['feature_change'] : [],
    skipped_entries: 0,
    ignored_entries: 0,
  };
}

interface Opts {
  captain?: CaptainResponse;
  /** The captain body served once a seat-changing verb has resolved. */
  captainAfter?: CaptainResponse;
  failing?: 'captain' | 'brief' | 'delegation' | 'confirmation';
}

function stub({ captain = captainBody(), captainAfter, failing }: Opts = {}) {
  let seatChanged = false;
  const fetchMock = vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
    const p = new URL(String(input), 'http://localhost').pathname;
    const respond = (b: unknown, status = 200) =>
      new Response(JSON.stringify(b), { status, headers: { 'Content-Type': 'application/json' } });
    const fail = () => respond({ error: 'internal', message: `${failing} read failed` }, 500);
    if (p.startsWith('/v0/captain/') && init?.method === 'POST') {
      if (/\/(accept|claim|relinquish)$/.test(p)) seatChanged = true;
      return respond({ repo: REPO, event: {}, captain: CAPTAIN, pending_offer: null });
    }
    if (p === '/v0/captain') {
      if (failing === 'captain') return fail();
      return respond(seatChanged && captainAfter ? captainAfter : captain);
    }
    if (p === '/v0/handover-brief') return failing === 'brief' ? fail() : respond(BRIEF);
    if (p === '/v0/repos/acme/app/delegation') {
      return failing === 'delegation' ? fail() : respond(VIEW);
    }
    if (p === '/v0/repos/acme/app/delegation/confirmation') {
      return failing === 'confirmation' ? fail() : respond(verdictBody(seatChanged));
    }
    return respond({ error: 'not_found' }, 404);
  });
  vi.stubGlobal('fetch', fetchMock);
  return {
    gets: (path: string) =>
      fetchMock.mock.calls.filter(
        ([input]) => new URL(String(input), 'http://localhost').pathname === path,
      ).length,
  };
}

const AUTH: AuthContextValue = {
  status: 'authenticated',
  user: { id: 'u1', github_login: 'hubot', name: 'Hubot', email: null, account_id: null },
  reload: async () => {},
  signOut: async () => {},
};

function mount() {
  return render(
    <MemoryRouter>
      <AuthContext.Provider value={AUTH}>
        <BridgeTab repo={REPO} owner="acme" name="app" />
      </AuthContext.Provider>
    </MemoryRouter>,
  );
}

function panel(title: string): HTMLElement {
  return screen.getByRole('heading', { name: title }).closest('section')!;
}

function unconfirmed(block: CaptainDelegationUnconfirmed | undefined): CaptainResponse {
  const body = captainBody();
  if (block === undefined) delete body.delegation_unconfirmed;
  else body.delegation_unconfirmed = block;
  return body;
}

const ALL_CLEAR = /Every workflow in the cached spec/;

afterEach(() => vi.unstubAllGlobals());

describe('<BridgeTab> unconfirmed-delegation badge (counterfactual 1)', () => {
  it('unavailable: chain_read_failed with an EMPTY list names the failure, never the all-clear', async () => {
    stub({
      captain: unconfirmed({
        seat_sequence: 7,
        source: 'run_cache',
        workflows: [],
        hash_staleness_reported: false,
        unavailable: 'chain_read_failed',
      }),
    });
    mount();
    const badge = await screen.findByTestId('unconfirmed-delegation');
    expect(badge).toHaveTextContent(
      'Unconfirmed delegation could not be read (chain_read_failed); this is not an all-clear.',
    );
    expect(badge).not.toHaveTextContent(ALL_CLEAR);
  });

  it('inventory_unavailable: no_cached_spec with an EMPTY list names it, never the all-clear', async () => {
    stub({
      captain: unconfirmed({
        seat_sequence: 7,
        source: 'run_cache',
        workflows: [],
        hash_staleness_reported: false,
        inventory_unavailable: 'no_cached_spec',
      }),
    });
    mount();
    const badge = await screen.findByTestId('unconfirmed-delegation');
    expect(badge).toHaveTextContent('the workflow inventory is unavailable (no_cached_spec)');
    expect(badge).not.toHaveTextContent(ALL_CLEAR);
  });

  it('an ABSENT block says it is not reported, never the all-clear', async () => {
    stub({ captain: unconfirmed(undefined) });
    mount();
    const badge = await screen.findByTestId('unconfirmed-delegation');
    expect(badge).toHaveTextContent('Unconfirmed delegation is not reported');
    expect(badge).not.toHaveTextContent(ALL_CLEAR);
  });

  it('an empty list with no unavailability is the all-clear, and says hash staleness is not checked', async () => {
    stub();
    mount();
    const badge = await screen.findByTestId('unconfirmed-delegation');
    expect(badge).toHaveTextContent(ALL_CLEAR);
    expect(badge).toHaveTextContent('Hash staleness is not checked here');
  });

  it('counts and names the unconfirmed workflows', async () => {
    stub({
      captain: unconfirmed({
        seat_sequence: 7,
        unconfirmed_since: '2026-09-25T00:00:00Z',
        source: 'run_cache',
        workflows: ['bug_fix', 'feature_change'],
        hash_staleness_reported: false,
      }),
    });
    mount();
    const badge = await screen.findByTestId('unconfirmed-delegation');
    expect(badge).toHaveTextContent(
      '2 workflows unconfirmed for the seat in force (#7, since 2026-09-25T00:00:00Z): bug_fix, feature_change.',
    );
  });
});

describe('<BridgeTab> composition', () => {
  it('fetches /v0/captain ONCE for the captain panel, the handover panel and the badge', async () => {
    const s = stub();
    mount();
    await screen.findByTestId('unconfirmed-delegation');
    await screen.findByTestId('delegation-workflow-feature_change');
    expect(within(panel('Captain')).getByText('github:octo')).toBeInTheDocument();
    expect(within(panel('Handover')).getByText('github:octo')).toBeInTheDocument();
    expect(s.gets('/v0/captain')).toBe(1);
    expect(s.gets('/v0/handover-brief')).toBe(1);
  });

  it.each([
    ['captain', ['Captain', 'Handover'], ['Handover brief', 'Delegation']],
    ['brief', ['Handover brief'], ['Captain', 'Handover', 'Delegation']],
    ['delegation', ['Delegation'], ['Captain', 'Handover', 'Handover brief']],
  ] as const)('a failing %s read errors only its own panels', async (failing, broken, healthy) => {
    stub({ failing });
    mount();
    await waitFor(() => expect(screen.getAllByRole('alert')).toHaveLength(broken.length));
    for (const title of broken) {
      expect(within(panel(title)).getByRole('alert')).toBeInTheDocument();
    }
    for (const title of healthy) {
      await waitFor(() => expect(within(panel(title)).queryByText(/^Loading /)).toBeNull());
      expect(within(panel(title)).queryByRole('alert')).toBeNull();
    }
  });
});

describe('<BridgeTab> seat change re-reads delegation verdicts (approval condition 1)', () => {
  async function flips(verb: string, opts: Opts) {
    const s = stub(opts);
    mount();
    const badge = await screen.findByTestId('delegation-status-feature_change');
    expect(badge).toHaveAttribute('data-status', 'confirmed');
    const before = s.gets('/v0/repos/acme/app/delegation/confirmation');
    fireEvent.click(document.querySelector(`[data-captain-verb="${verb}"]`) as HTMLElement);
    await waitFor(() =>
      expect(screen.getByTestId('delegation-status-feature_change')).toHaveAttribute(
        'data-status',
        'unconfirmed',
      ),
    );
    expect(screen.getByTestId('delegation-status-feature_change')).toHaveTextContent(
      'unconfirmed — the seat changed hands since the last confirmation',
    );
    expect(s.gets('/v0/repos/acme/app/delegation/confirmation')).toBeGreaterThan(before);
  }

  it('accepting a handover flips a confirmed badge to unconfirmed (handover)', async () => {
    await flips('accept', {
      captain: captainBody({ pending_offer: OFFER }),
      captainAfter: captainBody({ captain: { ...CAPTAIN, subject: 'github:hubot' } }),
    });
  });

  it('claiming a vacant seat flips the badge', async () => {
    await flips('claim', {
      captain: captainBody({ captain: null, last_captain: 'github:octo' }),
      captainAfter: captainBody({
        captain: { ...CAPTAIN, subject: 'github:hubot', basis: 'claimed', claim_verified: true },
      }),
    });
  });

  it('relinquishing the seat flips the badge', async () => {
    await flips('relinquish', {
      captainAfter: captainBody({ captain: null, last_captain: 'github:octo' }),
    });
  });
});
