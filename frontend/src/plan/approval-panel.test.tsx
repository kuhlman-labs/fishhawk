import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { fireEvent, render, screen, waitFor } from '@testing-library/react';
import { MemoryRouter } from 'react-router';
import { ApprovalPanel } from './approval-panel';
import type { Stage } from '@/api/types';

const stageAwaiting: Stage = {
  id: '00000000-0000-0000-0000-0000000000aa',
  run_id: '00000000-0000-0000-0000-0000000000ab',
  sequence: 0,
  type: 'plan',
  executor: { kind: 'agent', ref: 'claude-code' },
  state: 'awaiting_approval',
  started_at: '2026-05-04T20:00:00Z',
  ended_at: null,
  failure_category: null,
  failure_reason: null,
  created_at: '2026-05-04T20:00:00Z',
  updated_at: '2026-05-04T20:00:00Z',
};

interface FetchSetup {
  approval: { ok: true; stage: Stage } | { ok: false; status: number; error: string };
}

function setupFetch({ approval }: FetchSetup) {
  const fetchMock = vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
    const url = typeof input === 'string' ? input : input.toString();
    if (url.endsWith('/approvals') && init?.method === 'POST') {
      if (!approval.ok) {
        return new Response(JSON.stringify({ error: approval.error }), {
          status: approval.status,
          headers: { 'Content-Type': 'application/json' },
        });
      }
      return new Response(JSON.stringify(approval.stage), {
        status: 200,
        headers: { 'Content-Type': 'application/json' },
      });
    }
    return new Response('not stubbed', { status: 404 });
  });
  vi.stubGlobal('fetch', fetchMock);
  return fetchMock;
}

function renderPanel(
  stage: Stage = stageAwaiting,
  extra: Partial<Parameters<typeof ApprovalPanel>[0]> = {},
) {
  const onUpdate = vi.fn();
  const onRollback = vi.fn();
  render(
    <MemoryRouter>
      <ApprovalPanel
        stage={stage}
        runId={stage.run_id}
        onUpdate={onUpdate}
        onRollback={onRollback}
        {...extra}
      />
    </MemoryRouter>,
  );
  return { onUpdate, onRollback };
}

describe('ApprovalPanel', () => {
  beforeEach(() => vi.unstubAllGlobals());
  afterEach(() => vi.unstubAllGlobals());

  it('approves with an optimistic update and replaces it with the server response', async () => {
    const succeeded: Stage = { ...stageAwaiting, state: 'succeeded' };
    const fetchMock = setupFetch({ approval: { ok: true, stage: succeeded } });
    const { onUpdate, onRollback } = renderPanel();

    fireEvent.click(screen.getByRole('button', { name: /^approve$/i }));
    fireEvent.change(screen.getByLabelText(/approve comment/i), {
      target: { value: 'lgtm' },
    });
    fireEvent.click(screen.getByRole('button', { name: /confirm approve/i }));

    // Optimistic update fires immediately (state → succeeded), then the
    // server response replaces it with the canonical Stage row.
    await waitFor(() => {
      expect(onUpdate).toHaveBeenCalledTimes(2);
    });
    expect(onUpdate.mock.calls[0][0]).toMatchObject({ state: 'succeeded' });
    expect(onUpdate.mock.calls[1][0]).toEqual(succeeded);
    expect(onRollback).not.toHaveBeenCalled();

    // The POST hit /approvals with the right body.
    const call = fetchMock.mock.calls.find(([url]) => String(url).endsWith('/approvals'));
    expect(call).toBeDefined();
    expect(JSON.parse(String(call?.[1]?.body))).toEqual({
      decision: 'approve',
      comment: 'lgtm',
    });
  });

  it('omits an empty comment from the request body', async () => {
    const fetchMock = setupFetch({
      approval: { ok: true, stage: { ...stageAwaiting, state: 'succeeded' } },
    });
    renderPanel();

    fireEvent.click(screen.getByRole('button', { name: /^approve$/i }));
    fireEvent.click(screen.getByRole('button', { name: /confirm approve/i }));

    await waitFor(() => {
      const call = fetchMock.mock.calls.find(([u]) => String(u).endsWith('/approvals'));
      expect(call).toBeDefined();
      expect(JSON.parse(String(call?.[1]?.body))).toEqual({ decision: 'approve' });
    });
  });

  it('rejects → marks stage failed-D optimistically, replaces with server stage on success', async () => {
    const rejected: Stage = {
      ...stageAwaiting,
      state: 'failed',
      failure_category: 'D',
      failure_reason: 'gate rejected',
    };
    setupFetch({ approval: { ok: true, stage: rejected } });
    const { onUpdate, onRollback } = renderPanel();

    fireEvent.click(screen.getByRole('button', { name: /^reject$/i }));
    fireEvent.click(screen.getByRole('button', { name: /confirm reject/i }));

    await waitFor(() => {
      expect(onUpdate).toHaveBeenCalledTimes(2);
    });
    expect(onUpdate.mock.calls[0][0]).toMatchObject({
      state: 'failed',
      failure_category: 'D',
    });
    expect(onUpdate.mock.calls[1][0]).toEqual(rejected);
    expect(onRollback).not.toHaveBeenCalled();
  });

  it('rolls back the optimistic update and surfaces the error when the backend rejects the call', async () => {
    setupFetch({ approval: { ok: false, status: 403, error: 'forbidden' } });
    const { onRollback } = renderPanel();

    fireEvent.click(screen.getByRole('button', { name: /^approve$/i }));
    fireEvent.click(screen.getByRole('button', { name: /confirm approve/i }));

    await waitFor(() => {
      expect(onRollback).toHaveBeenCalledWith(stageAwaiting);
    });
    expect(screen.getByRole('alert')).toHaveTextContent(/403/);
    // Approve button is back so the user can retry once they have a different identity.
    expect(screen.getByRole('button', { name: /^approve$/i })).toBeEnabled();
  });

  it('renders a terminal status (no buttons) once the stage is no longer awaiting approval', () => {
    renderPanel({ ...stageAwaiting, state: 'succeeded' });
    expect(screen.queryByRole('button', { name: /^approve$/i })).not.toBeInTheDocument();
    expect(screen.getByText(/approved/i)).toBeInTheDocument();
  });

  it('calls onSubmitted ONLY after the successful POST, never on the optimistic onUpdate (E40.2 / #1717)', async () => {
    const succeeded: Stage = { ...stageAwaiting, state: 'succeeded' };
    setupFetch({ approval: { ok: true, stage: succeeded } });
    const onSubmitted = vi.fn();
    renderPanel(stageAwaiting, { onSubmitted });

    fireEvent.click(screen.getByRole('button', { name: /^approve$/i }));
    fireEvent.click(screen.getByRole('button', { name: /confirm approve/i }));

    await waitFor(() => expect(onSubmitted).toHaveBeenCalledTimes(1));
    // Called with the SERVER stage, not the optimistic one.
    expect(onSubmitted).toHaveBeenCalledWith(succeeded);
  });

  it('does NOT call onSubmitted when the approval POST fails (rolled back)', async () => {
    setupFetch({ approval: { ok: false, status: 500, error: 'boom' } });
    const onSubmitted = vi.fn();
    const { onRollback } = renderPanel(stageAwaiting, { onSubmitted });

    fireEvent.click(screen.getByRole('button', { name: /^approve$/i }));
    fireEvent.click(screen.getByRole('button', { name: /confirm approve/i }));

    await waitFor(() => expect(onRollback).toHaveBeenCalled());
    expect(onSubmitted).not.toHaveBeenCalled();
  });

  it('renders Regenerate by default (existing call sites unchanged)', () => {
    renderPanel();
    expect(screen.getByRole('button', { name: /regenerate/i })).toBeInTheDocument();
  });

  it('suppresses Regenerate when showRegenerate={false} (the queue call site)', () => {
    renderPanel(stageAwaiting, { showRegenerate: false });
    expect(screen.queryByRole('button', { name: /regenerate/i })).not.toBeInTheDocument();
    // The verbs remain.
    expect(screen.getByRole('button', { name: /^approve$/i })).toBeInTheDocument();
    expect(screen.getByRole('button', { name: /^reject$/i })).toBeInTheDocument();
  });

  it('cancels the confirmation panel without firing a request', async () => {
    const fetchMock = setupFetch({
      approval: { ok: true, stage: { ...stageAwaiting, state: 'succeeded' } },
    });
    renderPanel();

    fireEvent.click(screen.getByRole('button', { name: /^approve$/i }));
    fireEvent.click(screen.getByRole('button', { name: /^cancel$/i }));

    // Back to the idle state with the action buttons.
    expect(screen.getByRole('button', { name: /^approve$/i })).toBeEnabled();
    expect(fetchMock).not.toHaveBeenCalled();
  });
});
