import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { fireEvent, render, screen, waitFor } from '@testing-library/react';
import { FailureBanner } from './failure-banner';
import type { FailureCategory, Stage } from '@/api/types';
import { FAILURE_DESCRIPTIONS } from '@/api/types';

function stage(overrides: Partial<Stage> = {}): Stage {
  return {
    id: '00000000-0000-0000-0000-0000000000aa',
    run_id: '00000000-0000-0000-0000-0000000000ab',
    sequence: 0,
    type: 'plan',
    executor: { kind: 'agent', ref: 'claude-code' },
    state: 'awaiting_approval',
    started_at: null,
    ended_at: null,
    failure_category: null,
    failure_reason: null,
    created_at: '2026-05-04T20:00:00Z',
    updated_at: '2026-05-04T20:00:00Z',
    ...overrides,
  };
}

const dTimeoutStage: Stage = stage({
  state: 'failed',
  failure_category: 'D',
  failure_reason: 'sla_timeout: 5h elapsed (deadline 4h)',
  ended_at: '2026-05-04T22:00:00Z',
});

describe('FailureBanner', () => {
  beforeEach(() => vi.unstubAllGlobals());
  afterEach(() => vi.unstubAllGlobals());

  it('renders nothing when stage is not failed', () => {
    const { container } = render(<FailureBanner stage={stage({ state: 'succeeded' })} />);
    expect(container.firstChild).toBeNull();
  });

  it('renders nothing when failure_category is missing', () => {
    const { container } = render(
      <FailureBanner stage={stage({ state: 'failed', failure_category: null })} />,
    );
    expect(container.firstChild).toBeNull();
  });

  it('renders category, description, and reason when failed', () => {
    render(
      <FailureBanner
        stage={stage({
          state: 'failed',
          failure_category: 'B',
          failure_reason: 'forbidden_paths violated: backend/internal/policy/secret.go',
        })}
      />,
    );
    const banner = screen.getByRole('alert');
    expect(banner).toHaveTextContent('category B');
    expect(banner).toHaveTextContent(FAILURE_DESCRIPTIONS.B);
    expect(banner).toHaveTextContent('forbidden_paths violated');
  });

  it('renders each category with its canonical description', () => {
    for (const cat of ['A', 'B', 'C', 'D'] satisfies FailureCategory[]) {
      const { unmount } = render(
        <FailureBanner stage={stage({ state: 'failed', failure_category: cat })} />,
      );
      expect(screen.getByRole('alert')).toHaveTextContent(FAILURE_DESCRIPTIONS[cat]);
      unmount();
    }
  });

  it('omits the Retry button when failure category is not D-timeout', () => {
    render(
      <FailureBanner
        stage={stage({
          state: 'failed',
          failure_category: 'B',
          failure_reason: 'forbidden_paths',
        })}
        onStageUpdate={vi.fn()}
        onStageRollback={vi.fn()}
      />,
    );
    expect(screen.queryByRole('button', { name: /retry/i })).not.toBeInTheDocument();
  });

  it('omits the Retry button when D failure_reason is rejection (not timeout)', () => {
    render(
      <FailureBanner
        stage={stage({
          state: 'failed',
          failure_category: 'D',
          failure_reason: 'gate rejected by approver',
        })}
        onStageUpdate={vi.fn()}
        onStageRollback={vi.fn()}
      />,
    );
    expect(screen.queryByRole('button', { name: /retry/i })).not.toBeInTheDocument();
  });

  it('omits the Retry button when callbacks are not supplied', () => {
    render(<FailureBanner stage={dTimeoutStage} />);
    expect(screen.queryByRole('button', { name: /retry/i })).not.toBeInTheDocument();
  });

  it('renders the Retry button when D-timeout + callbacks are supplied', () => {
    render(
      <FailureBanner stage={dTimeoutStage} onStageUpdate={vi.fn()} onStageRollback={vi.fn()} />,
    );
    expect(screen.getByRole('button', { name: /retry/i })).toBeEnabled();
  });

  it('retries optimistically and replaces with the server response', async () => {
    const reopened: Stage = {
      ...dTimeoutStage,
      state: 'awaiting_approval',
      failure_category: null,
      failure_reason: null,
      ended_at: null,
    };
    const fetchMock = vi.fn(
      async () =>
        new Response(JSON.stringify(reopened), {
          status: 200,
          headers: { 'Content-Type': 'application/json' },
        }),
    );
    vi.stubGlobal('fetch', fetchMock);

    const onStageUpdate = vi.fn();
    const onStageRollback = vi.fn();
    render(
      <FailureBanner
        stage={dTimeoutStage}
        onStageUpdate={onStageUpdate}
        onStageRollback={onStageRollback}
      />,
    );

    fireEvent.click(screen.getByRole('button', { name: /retry/i }));

    await waitFor(() => {
      expect(onStageUpdate).toHaveBeenCalledTimes(2);
    });
    // Optimistic update fires first with awaiting_approval state…
    expect(onStageUpdate.mock.calls[0][0]).toMatchObject({
      state: 'awaiting_approval',
      failure_category: null,
    });
    // …then the server response replaces it.
    expect(onStageUpdate.mock.calls[1][0]).toEqual(reopened);
    expect(onStageRollback).not.toHaveBeenCalled();

    // The POST hit /retry.
    const call = fetchMock.mock.calls.find(([url]) => String(url).endsWith('/retry'));
    expect(call).toBeDefined();
    expect((call?.[1] as RequestInit | undefined)?.method).toBe('POST');
  });

  it('rolls back the optimistic update and surfaces the error on backend rejection', async () => {
    const fetchMock = vi.fn(
      async () =>
        new Response(JSON.stringify({ error: 'retry_not_applicable' }), {
          status: 422,
          headers: { 'Content-Type': 'application/json' },
        }),
    );
    vi.stubGlobal('fetch', fetchMock);

    const onStageUpdate = vi.fn();
    const onStageRollback = vi.fn();
    render(
      <FailureBanner
        stage={dTimeoutStage}
        onStageUpdate={onStageUpdate}
        onStageRollback={onStageRollback}
      />,
    );

    fireEvent.click(screen.getByRole('button', { name: /retry/i }));

    await waitFor(() => {
      expect(onStageRollback).toHaveBeenCalledWith(dTimeoutStage);
    });
    expect(screen.getByRole('alert')).toHaveTextContent(/422/);
    // Button comes back enabled so the user can retry once the
    // failure mode has changed.
    expect(screen.getByRole('button', { name: /retry/i })).toBeEnabled();
  });
});
