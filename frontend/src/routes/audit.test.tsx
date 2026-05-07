import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { fireEvent, render, screen, waitFor } from '@testing-library/react';
import { MemoryRouter } from 'react-router';
import { Audit } from './audit';
import { api } from '@/api/client';
import type { AuditEntry, PaginatedList } from '@/api/types';

function makeEntry(over: Partial<AuditEntry> = {}): AuditEntry {
  return {
    id: 'aaaaaaaa-1111-1111-1111-111111111111',
    sequence: 1,
    run_id: '11111111-2222-3333-4444-555555555555',
    stage_id: null,
    ts: '2026-05-04T20:00:00Z',
    category: 'trace_uploaded',
    actor_kind: 'agent',
    actor_subject: 'claude-code',
    payload: {},
    prev_hash: null,
    entry_hash: 'abcdef1234567890abcdef1234567890',
    ...over,
  };
}

function renderPage() {
  return render(
    <MemoryRouter>
      <Audit />
    </MemoryRouter>,
  );
}

describe('<Audit>', () => {
  beforeEach(() => vi.restoreAllMocks());
  afterEach(() => vi.restoreAllMocks());

  it('renders the heading + brand-voice eyebrow', async () => {
    vi.spyOn(api, 'listGlobalAudit').mockResolvedValue({
      items: [],
      next_cursor: null,
    } satisfies PaginatedList<AuditEntry>);
    renderPage();
    expect(screen.getByRole('heading', { name: /audit log/i })).toBeInTheDocument();
    expect(
      screen.getByText(/append-only, signed record of every approval and run transition/i),
    ).toBeInTheDocument();
    // Wait for the async-fetched empty state so the test doesn't
    // race the resolved promise against teardown (avoids React's
    // "update not wrapped in act" warning).
    await waitFor(() => {
      expect(screen.getByText(/no audit entries match these filters/i)).toBeInTheDocument();
    });
  });

  it('shows the empty-state message when no entries match', async () => {
    vi.spyOn(api, 'listGlobalAudit').mockResolvedValue({
      items: [],
      next_cursor: null,
    });
    renderPage();
    await waitFor(() => {
      expect(screen.getByText(/no audit entries match these filters/i)).toBeInTheDocument();
    });
  });

  it('renders the entries returned by listGlobalAudit, including the run column', async () => {
    vi.spyOn(api, 'listGlobalAudit').mockResolvedValue({
      items: [
        makeEntry({ id: 'r1', sequence: 1, category: 'trace_uploaded' }),
        // Global-chain entry: run_id null, run column shows "—".
        makeEntry({ id: 'g1', sequence: 2, run_id: null, category: 'api_token_issued' }),
      ],
      next_cursor: null,
    });
    renderPage();
    await waitFor(() => {
      expect(screen.getByText('trace_uploaded')).toBeInTheDocument();
      expect(screen.getByText('api_token_issued')).toBeInTheDocument();
    });
    // Per-run entry deep-links back to /runs/<id>#audit.
    const runLink = screen.getByRole('link', { name: /11111111/i });
    expect(runLink).toHaveAttribute('href', '/runs/11111111-2222-3333-4444-555555555555#audit');
  });

  it('passes the selected category through to listGlobalAudit when changed', async () => {
    const spy = vi.spyOn(api, 'listGlobalAudit').mockResolvedValue({
      items: [],
      next_cursor: null,
    });
    renderPage();
    await waitFor(() => expect(spy).toHaveBeenCalled());

    fireEvent.change(screen.getByLabelText(/filter audit entries by category/i), {
      target: { value: 'plan_generated' },
    });

    await waitFor(() => {
      const lastCall = spy.mock.calls.at(-1)?.[0];
      expect(lastCall?.category).toBe('plan_generated');
    });
  });

  it('does not pass `category` when "All categories" is the selection', async () => {
    const spy = vi.spyOn(api, 'listGlobalAudit').mockResolvedValue({
      items: [],
      next_cursor: null,
    });
    renderPage();
    await waitFor(() => expect(spy).toHaveBeenCalled());
    const initial = spy.mock.calls[0]?.[0];
    expect(initial?.category).toBeUndefined();
  });

  it('surfaces the API error message in the error box', async () => {
    vi.spyOn(api, 'listGlobalAudit').mockRejectedValue(new Error('boom'));
    renderPage();
    await waitFor(() => {
      expect(screen.getByRole('alert')).toHaveTextContent(/boom/i);
    });
  });
});
