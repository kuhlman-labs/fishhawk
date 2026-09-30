import { describe, expect, it } from 'vitest';
import { render, screen } from '@testing-library/react';
import { ApiClientError } from '@/api/client';
import type { CaptainRecord, CaptainResponse } from '@/api/captain';
import type { AsyncState } from '@/api/use-async';
import { CaptainPanel } from './captain-panel';

/*
 * Every fixture is a literal `CaptainResponse` wire body (docs/api/
 * v0.openapi.yaml). The two verifications are asserted as DISTINCT states.
 */

const ASSIGNED: CaptainRecord = {
  subject: 'github:octo',
  identity_verified: true,
  claim_verified: null,
  basis: 'assigned',
  assigned_sequence: 41,
  assigned_entry_hash: 'a'.repeat(64),
  assigned_at: '2026-09-20T12:00:00Z',
};

function body(over: Partial<CaptainResponse> = {}): CaptainResponse {
  return {
    repo: 'acme/app',
    captain: ASSIGNED,
    pending_offer: null,
    last_captain: null,
    history: [],
    history_total: 3,
    skipped_entries: 0,
    ...over,
  };
}

function ok(data: CaptainResponse): AsyncState<CaptainResponse> {
  return { status: 'ok', data };
}

describe('<CaptainPanel>', () => {
  it('renders the seated captain, captain-since with its citation, and the basis', () => {
    render(<CaptainPanel state={ok(body())} />);
    expect(screen.getByText('github:octo')).toBeInTheDocument();
    expect(screen.getByText('2026-09-20T12:00:00Z')).toBeInTheDocument();
    expect(screen.getByText(`#41 · ${'a'.repeat(12)}…`)).toBeInTheDocument();
    expect(screen.getByText('Handed over')).toBeInTheDocument();
    expect(screen.getByText('3 captain entries on the chain for acme/app.')).toBeInTheDocument();
  });

  it('identity_verified true renders the verified line and NO unverified badge', () => {
    render(<CaptainPanel state={ok(body())} />);
    expect(screen.getByTestId('identity-verified')).toBeInTheDocument();
    expect(screen.queryByTestId('identity-unverified')).toBeNull();
  });

  it('identity_verified false renders its own badge naming the static-token case', () => {
    render(
      <CaptainPanel
        state={ok(
          body({ captain: { ...ASSIGNED, subject: 'ops-token', identity_verified: false } }),
        )}
      />,
    );
    expect(screen.getByTestId('identity-unverified')).toHaveTextContent(
      'identity not provider-verified',
    );
    expect(screen.getByText(/a static-token subject carries false/)).toBeInTheDocument();
    expect(screen.queryByTestId('identity-verified')).toBeNull();
  });

  it('claim_verified null, false and true render three different sentences', () => {
    const { unmount } = render(<CaptainPanel state={ok(body())} />);
    expect(screen.getByText('Not applicable — the seat was handed over')).toBeInTheDocument();
    unmount();

    const claimed = { ...ASSIGNED, basis: 'claimed' as const };
    const r2 = render(
      <CaptainPanel state={ok(body({ captain: { ...claimed, claim_verified: false } }))} />,
    );
    expect(
      screen.getByText('Admitted on a positively trivial approval predicate'),
    ).toBeInTheDocument();
    expect(screen.getByText('Claimed while vacant')).toBeInTheDocument();
    expect(screen.queryByText('Not applicable — the seat was handed over')).toBeNull();
    r2.unmount();

    render(<CaptainPanel state={ok(body({ captain: { ...claimed, claim_verified: true } }))} />);
    expect(
      screen.getByText('A non-trivial approval predicate was checked and satisfied'),
    ).toBeInTheDocument();
    expect(screen.queryByText('Admitted on a positively trivial approval predicate')).toBeNull();
  });

  it('a vacant seat renders the vacancy banner naming last_captain', () => {
    render(<CaptainPanel state={ok(body({ captain: null, last_captain: 'github:former' }))} />);
    expect(screen.getByRole('status')).toHaveTextContent('The captain seat is vacant.');
    expect(screen.getByRole('status')).toHaveTextContent('Last captain: github:former');
  });

  it('a never-seated vacancy says no captain has ever sat', () => {
    render(<CaptainPanel state={ok(body({ captain: null, history_total: 0 }))} />);
    expect(screen.getByRole('status')).toHaveTextContent(
      'No captain has ever sat for this repository.',
    );
  });

  it('skipped_entries > 0 renders the named partial-fold note', () => {
    render(<CaptainPanel state={ok(body({ skipped_entries: 2 }))} />);
    expect(
      screen.getByText(/Partial fold: 2 captain entries could not be used/),
    ).toBeInTheDocument();
  });

  it('skipped_entries 0 renders no partial-fold note', () => {
    render(<CaptainPanel state={ok(body())} />);
    expect(screen.queryByText(/Partial fold/)).toBeNull();
  });

  it('a 501 captain_unconfigured renders the named state, not the generic error', () => {
    const err = new ApiClientError(
      501,
      { error: 'captain_unconfigured', message: 'captain store not wired' },
      'captain store not wired',
    );
    render(<CaptainPanel state={{ status: 'error', error: err }} />);
    expect(
      screen.getByText('No captain record on this instance (captain_unconfigured).'),
    ).toBeInTheDocument();
    expect(screen.queryByText(/Couldn.t load captain/)).toBeNull();
  });

  it('any other read error renders the generic panel error', () => {
    const err = new ApiClientError(
      500,
      { error: 'internal_error', message: 'chain read' },
      'chain read',
    );
    render(<CaptainPanel state={{ status: 'error', error: err }} />);
    expect(screen.getByRole('alert')).toHaveTextContent("Couldn't load captain.");
  });
});
