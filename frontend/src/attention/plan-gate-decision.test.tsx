import { readFileSync } from 'node:fs';
import { resolve } from 'node:path';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { fireEvent, render, screen, waitFor } from '@testing-library/react';
import { MemoryRouter } from 'react-router';
import { PlanGateDecision } from './plan-gate-decision';
import type { AttentionItem, AttentionList, Stage } from '@/api/types';

/*
 * PlanGateDecision REUSES the existing ApprovalPanel end to end. These cases
 * assert the reuse by exercising ApprovalPanel's distinctive behaviour — the
 * idle → confirming → submit machine and the optimistic-then-rollback
 * contract — reached through PlanGateDecision, not a second implementation.
 * A stubbed global fetch serves GET /v0/stages/{id} then POST /approvals.
 */
const golden = JSON.parse(
  readFileSync(resolve(__dirname, '../../../testdata/wire/attention_list.json'), 'utf8'),
) as AttentionList;

const planItem: AttentionItem = golden.items.find((i) => i.kind === 'plan_gate')!;

const stageAwaiting: Stage = {
  id: planItem.stage_id!,
  run_id: planItem.run_id!,
  sequence: 0,
  type: 'plan',
  executor: { kind: 'agent', ref: 'claude-code' },
  state: 'awaiting_approval',
  started_at: '2026-09-01T20:00:00Z',
  ended_at: null,
  failure_category: null,
  failure_reason: null,
  created_at: '2026-09-01T20:00:00Z',
  updated_at: '2026-09-01T20:00:00Z',
};

interface Setup {
  stage?: { ok: true; stage: Stage } | { ok: false };
  approval?: { ok: true; stage: Stage } | { ok: false; status: number; error: string };
}

function setupFetch({ stage = { ok: true, stage: stageAwaiting }, approval }: Setup) {
  const fetchMock = vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
    const url = typeof input === 'string' ? input : input.toString();
    if (url.includes('/stages/') && !url.endsWith('/approvals')) {
      if (!stage.ok)
        return new Response(JSON.stringify({ error: 'stage_read_failed' }), { status: 500 });
      return new Response(JSON.stringify(stage.stage), {
        status: 200,
        headers: { 'Content-Type': 'application/json' },
      });
    }
    if (url.endsWith('/approvals') && init?.method === 'POST') {
      if (!approval || !approval.ok) {
        return new Response(
          JSON.stringify({ error: approval?.ok === false ? approval.error : 'x' }),
          {
            status: approval && approval.ok === false ? approval.status : 500,
            headers: { 'Content-Type': 'application/json' },
          },
        );
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

function renderDecision(item: AttentionItem = planItem) {
  const onResolved = vi.fn();
  render(
    <MemoryRouter>
      <PlanGateDecision item={item} onResolved={onResolved} />
    </MemoryRouter>,
  );
  return { onResolved };
}

describe('<PlanGateDecision>', () => {
  beforeEach(() => vi.unstubAllGlobals());
  afterEach(() => vi.unstubAllGlobals());

  it('fetches the stage and drives ApprovalPanel to a successful approve, resolving the item', async () => {
    const succeeded: Stage = { ...stageAwaiting, state: 'succeeded' };
    const fetchMock = setupFetch({ approval: { ok: true, stage: succeeded } });
    const { onResolved } = renderDecision();

    // ApprovalPanel's two-step machine, reached through PlanGateDecision.
    fireEvent.click(await screen.findByRole('button', { name: /^approve$/i }));
    fireEvent.click(screen.getByRole('button', { name: /confirm approve/i }));

    await waitFor(() => expect(onResolved).toHaveBeenCalledTimes(1));
    const post = fetchMock.mock.calls.find(([u]) => String(u).endsWith('/approvals'));
    expect(post).toBeDefined();
    expect(JSON.parse(String((post?.[1] as RequestInit)?.body))).toEqual({ decision: 'approve' });
  });

  it('C1: a 500 on the approval POST rolls back and does NOT resolve the item', async () => {
    const fetchMock = setupFetch({ approval: { ok: false, status: 500, error: 'boom' } });
    const { onResolved } = renderDecision();

    fireEvent.click(await screen.findByRole('button', { name: /^approve$/i }));
    fireEvent.click(screen.getByRole('button', { name: /confirm approve/i }));

    // The optimistic onUpdate fired, but the POST failed → ApprovalPanel rolled
    // back and onSubmitted never fired, so the item is NOT resolved.
    await screen.findByRole('alert');
    expect(screen.getByRole('alert')).toHaveTextContent(/500/);
    expect(onResolved).not.toHaveBeenCalled();
    expect(fetchMock.mock.calls.some(([u]) => String(u).endsWith('/approvals'))).toBe(true);
  });

  it('F5: a failed stage read renders the error envelope and no panel', async () => {
    setupFetch({ stage: { ok: false } });
    const { onResolved } = renderDecision();

    await screen.findByRole('alert');
    expect(screen.queryByRole('button', { name: /^approve$/i })).not.toBeInTheDocument();
    expect(onResolved).not.toHaveBeenCalled();
  });

  it('renders a refusal (no fetch) when the item is missing its stage_id', () => {
    const fetchMock = setupFetch({});
    const noStage: AttentionItem = { ...planItem, stage_id: undefined };
    renderDecision(noStage);
    expect(screen.getByRole('note')).toHaveTextContent(/missing its stage or run id/i);
    expect(fetchMock).not.toHaveBeenCalled();
  });
});
