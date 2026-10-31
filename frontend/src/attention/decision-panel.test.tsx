import { readFileSync } from 'node:fs';
import { resolve } from 'node:path';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { fireEvent, render, screen, waitFor } from '@testing-library/react';
import { MemoryRouter } from 'react-router';
import { DecisionPanel } from './decision-panel';
import { DECISION_VERBS, isDrivePlaneVerb } from './decision-verbs';
import type { AttentionItem, AttentionItemKind, AttentionList, Stage } from '@/api/types';

/*
 * Drive the REAL DecisionPanel against a stubbed global fetch (the pattern
 * approval-panel.test.tsx establishes) so each case crosses
 * component → api/client.ts → URL/method/body. Fixtures come from the shared
 * wire golden testdata/wire/attention_list.json — never a hand-authored copy.
 */
const golden = JSON.parse(
  readFileSync(resolve(__dirname, '../../../testdata/wire/attention_list.json'), 'utf8'),
) as AttentionList;

function itemOf(kind: AttentionItemKind): AttentionItem {
  const item = golden.items.find((i) => i.kind === kind);
  if (!item) throw new Error(`golden carries no ${kind} item`);
  return structuredClone(item);
}

const stageAwaiting: Stage = {
  id: itemOf('plan_gate').stage_id!,
  run_id: itemOf('plan_gate').run_id!,
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

interface StubResponse {
  status: number;
  body: unknown;
}
type Router = (url: string, init?: RequestInit) => StubResponse;

function stubFetch(router: Router): ReturnType<typeof vi.fn> {
  const fetchMock = vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
    const url = typeof input === 'string' ? input : input.toString();
    const r = router(url, init);
    return new Response(JSON.stringify(r.body), {
      status: r.status,
      headers: { 'Content-Type': 'application/json' },
    });
  });
  vi.stubGlobal('fetch', fetchMock);
  return fetchMock;
}

function renderPanel(item: AttentionItem) {
  const onResolved = vi.fn();
  const onCancel = vi.fn();
  render(
    <MemoryRouter>
      <DecisionPanel item={item} onResolved={onResolved} onCancel={onCancel} />
    </MemoryRouter>,
  );
  return { onResolved, onCancel };
}

function postCalls(fetchMock: ReturnType<typeof vi.fn>) {
  return fetchMock.mock.calls.filter(([, init]) => (init as RequestInit)?.method === 'POST');
}

function bodyOf(call: unknown[]): Record<string, unknown> {
  return JSON.parse(String((call[1] as RequestInit)?.body));
}

// chooseJudgement picks a judgement in the concern panel's select (E94.7 /
// #4840): Waive and Defer both require one.
function chooseJudgement(value: string) {
  fireEvent.change(screen.getByLabelText('Judgement'), { target: { value } });
}

describe('DecisionPanel — happy paths (one per kind)', () => {
  beforeEach(() => vi.unstubAllGlobals());
  afterEach(() => vi.unstubAllGlobals());

  it('scope_amendment: Approve POSTs the decision and resolves the item', async () => {
    const item = itemOf('scope_amendment');
    const fetchMock = stubFetch((url) => ({ status: 200, body: { id: url } }));
    const { onResolved } = renderPanel(item);

    fireEvent.click(screen.getByRole('button', { name: 'Approve' }));

    await waitFor(() => expect(onResolved).toHaveBeenCalledTimes(1));
    const calls = postCalls(fetchMock);
    expect(calls).toHaveLength(1);
    expect(String(calls[0][0])).toBe(
      `/v0/runs/${item.run_id}/scope-amendments/${item.amendment_id}/decision`,
    );
    expect(bodyOf(calls[0])).toEqual({ decision: 'approve' });
  });

  it('scope_amendment: Deny POSTs decision:deny with the optional reason', async () => {
    const item = itemOf('scope_amendment');
    const fetchMock = stubFetch(() => ({ status: 200, body: {} }));
    renderPanel(item);

    fireEvent.change(screen.getByLabelText(/decision reason/i), { target: { value: 'no' } });
    fireEvent.click(screen.getByRole('button', { name: 'Deny' }));

    await waitFor(() => expect(postCalls(fetchMock)).toHaveLength(1));
    expect(bodyOf(postCalls(fetchMock)[0])).toEqual({ decision: 'deny', reason: 'no' });
  });

  it('split_verdict: Waive POSTs the reason to the waive URL and resolves', async () => {
    const item = itemOf('split_verdict');
    const fetchMock = stubFetch(() => ({ status: 200, body: {} }));
    const { onResolved } = renderPanel(item);

    chooseJudgement('no_defect');
    fireEvent.change(screen.getByLabelText('Waive reason'), {
      target: { value: 'false positive' },
    });
    fireEvent.click(screen.getByRole('button', { name: 'Waive' }));

    await waitFor(() => expect(onResolved).toHaveBeenCalledTimes(1));
    const calls = postCalls(fetchMock);
    expect(String(calls[0][0])).toBe(`/v0/concerns/${item.concern_id}/waive`);
    expect(bodyOf(calls[0])).toEqual({ reason: 'false positive', judgement: 'no_defect' });
  });

  it('split_verdict: Defer POSTs the parent_epic and judgement to the defer URL', async () => {
    const item = itemOf('split_verdict');
    const fetchMock = stubFetch(() => ({ status: 200, body: { concern: {}, issue: {} } }));
    renderPanel(item);

    chooseJudgement('defect_pre_existing');
    fireEvent.change(screen.getByLabelText(/defer: parent epic/i), { target: { value: '#1196' } });
    fireEvent.click(screen.getByRole('button', { name: 'Defer' }));

    await waitFor(() => expect(postCalls(fetchMock)).toHaveLength(1));
    const call = postCalls(fetchMock)[0];
    expect(String(call[0])).toBe(`/v0/concerns/${item.concern_id}/defer`);
    expect(bodyOf(call)).toEqual({ judgement: 'defect_pre_existing', parent_epic: '#1196' });
  });

  it('paged_concern: Waive POSTs the reason to the waive URL (same panel, other kind)', async () => {
    const item = itemOf('paged_concern');
    const fetchMock = stubFetch(() => ({ status: 200, body: {} }));
    renderPanel(item);

    chooseJudgement('preference');
    fireEvent.change(screen.getByLabelText('Waive reason'), { target: { value: 'accepted' } });
    fireEvent.click(screen.getByRole('button', { name: 'Waive' }));

    await waitFor(() => expect(postCalls(fetchMock)).toHaveLength(1));
    expect(String(postCalls(fetchMock)[0][0])).toBe(`/v0/concerns/${item.concern_id}/waive`);
    expect(bodyOf(postCalls(fetchMock)[0])).toEqual({
      reason: 'accepted',
      judgement: 'preference',
    });
  });

  it('acceptance_disposition: ticking ack POSTs acknowledge_failed_criteria: true', async () => {
    const item = itemOf('acceptance_disposition');
    expect(item.context.criteria_failed).toBe(2); // the golden state condition C4/F4 depend on
    const fetchMock = stubFetch(() => ({ status: 200, body: { already_recorded: false } }));
    const { onResolved } = renderPanel(item);

    fireEvent.change(screen.getByLabelText(/arbitration reason/i), {
      target: { value: 'ship it' },
    });
    fireEvent.click(screen.getByLabelText(/acknowledge merging despite 2 failed/i));
    fireEvent.click(screen.getByRole('button', { name: 'Record arbitration' }));

    await waitFor(() => expect(onResolved).toHaveBeenCalledTimes(1));
    const call = postCalls(fetchMock)[0];
    expect(String(call[0])).toBe(`/v0/runs/${item.run_id}/acceptance-arbitration`);
    expect(bodyOf(call)).toEqual({ reason: 'ship it', acknowledge_failed_criteria: true });
  });
});

describe('DecisionPanel — precise verb equality (both directions)', () => {
  beforeEach(() => vi.unstubAllGlobals());
  afterEach(() => vi.unstubAllGlobals());

  const KINDS: AttentionItemKind[] = [
    'scope_amendment',
    'acceptance_disposition',
    'split_verdict',
    'paged_concern',
  ];

  it.each(KINDS)('%s renders exactly DECISION_VERBS[kind] as its submit verbs', (kind) => {
    stubFetch(() => ({ status: 200, body: {} }));
    const { container } = render(
      <MemoryRouter>
        <DecisionPanel item={itemOf(kind)} onResolved={vi.fn()} onCancel={vi.fn()} />
      </MemoryRouter>,
    );
    const verbLabels = Array.from(container.querySelectorAll('[data-decision-verb]'))
      .map((el) => (el.textContent ?? '').trim())
      .sort();
    expect(verbLabels).toEqual([...DECISION_VERBS[kind]].sort());
  });

  it.each(KINDS)('%s renders NO drive-plane verb on any button or link', (kind) => {
    stubFetch(() => ({ status: 200, body: {} }));
    render(
      <MemoryRouter>
        <DecisionPanel item={itemOf(kind)} onResolved={vi.fn()} onCancel={vi.fn()} />
      </MemoryRouter>,
    );
    for (const el of [...screen.queryAllByRole('button'), ...screen.queryAllByRole('link')]) {
      expect(
        isDrivePlaneVerb(el.textContent ?? ''),
        `"${el.textContent}" is a drive-plane verb`,
      ).toBe(false);
    }
  });
});

describe('DecisionPanel — blank-required-field guard (C2)', () => {
  beforeEach(() => vi.unstubAllGlobals());
  afterEach(() => vi.unstubAllGlobals());

  it('Waive with an empty reason issues ZERO fetches (button disabled)', () => {
    const fetchMock = stubFetch(() => ({ status: 200, body: {} }));
    renderPanel(itemOf('split_verdict'));
    chooseJudgement('no_defect');
    // reason deliberately left empty
    fireEvent.click(screen.getByRole('button', { name: 'Waive' }));
    expect(fetchMock).not.toHaveBeenCalled();
  });

  it('Defer with an empty parent_epic issues ZERO fetches (button disabled)', () => {
    const fetchMock = stubFetch(() => ({ status: 200, body: {} }));
    renderPanel(itemOf('split_verdict'));
    chooseJudgement('defect');
    fireEvent.click(screen.getByRole('button', { name: 'Defer' }));
    expect(fetchMock).not.toHaveBeenCalled();
  });
});

describe('DecisionPanel — judgement gate (E94.7 / #4840)', () => {
  beforeEach(() => vi.unstubAllGlobals());
  afterEach(() => vi.unstubAllGlobals());

  it('offers exactly the seven human judgements behind an empty placeholder, never unlabelled', () => {
    stubFetch(() => ({ status: 200, body: {} }));
    renderPanel(itemOf('split_verdict'));
    const select = screen.getByLabelText('Judgement') as HTMLSelectElement;
    expect(select.value).toBe('');
    expect(Array.from(select.options).map((o) => o.value)).toEqual([
      '',
      'defect',
      'defect_pre_existing',
      'no_defect',
      'tradeoff',
      'preference',
      'stale',
      'insufficient_evidence',
    ]);
  });

  it('Waive with a reason but NO judgement issues ZERO fetches (button disabled)', () => {
    const fetchMock = stubFetch(() => ({ status: 200, body: {} }));
    renderPanel(itemOf('split_verdict'));
    fireEvent.change(screen.getByLabelText('Waive reason'), { target: { value: 'fine' } });
    // judgement deliberately left on the placeholder
    expect(screen.getByRole('button', { name: 'Waive' })).toBeDisabled();
    fireEvent.click(screen.getByRole('button', { name: 'Waive' }));
    expect(fetchMock).not.toHaveBeenCalled();
  });

  it('Defer with a parent_epic but NO judgement issues ZERO fetches (button disabled)', () => {
    const fetchMock = stubFetch(() => ({ status: 200, body: {} }));
    renderPanel(itemOf('split_verdict'));
    fireEvent.change(screen.getByLabelText(/defer: parent epic/i), { target: { value: '#1196' } });
    expect(screen.getByRole('button', { name: 'Defer' })).toBeDisabled();
    fireEvent.click(screen.getByRole('button', { name: 'Defer' }));
    expect(fetchMock).not.toHaveBeenCalled();
  });

  it('the tradeoff cost field appears only for a tradeoff judgement', () => {
    stubFetch(() => ({ status: 200, body: {} }));
    renderPanel(itemOf('split_verdict'));
    expect(screen.queryByLabelText('Tradeoff cost')).not.toBeInTheDocument();
    chooseJudgement('tradeoff');
    expect(screen.getByLabelText('Tradeoff cost')).toBeInTheDocument();
    chooseJudgement('defect');
    expect(screen.queryByLabelText('Tradeoff cost')).not.toBeInTheDocument();
  });

  it('a tradeoff with a blank cost keeps Waive and Defer disabled (ZERO fetches)', () => {
    const fetchMock = stubFetch(() => ({ status: 200, body: {} }));
    renderPanel(itemOf('split_verdict'));
    chooseJudgement('tradeoff');
    fireEvent.change(screen.getByLabelText('Waive reason'), { target: { value: 'accepted' } });
    fireEvent.change(screen.getByLabelText(/defer: parent epic/i), { target: { value: '#1196' } });
    fireEvent.change(screen.getByLabelText('Tradeoff cost'), { target: { value: '   ' } });
    expect(screen.getByRole('button', { name: 'Waive' })).toBeDisabled();
    expect(screen.getByRole('button', { name: 'Defer' })).toBeDisabled();
    fireEvent.click(screen.getByRole('button', { name: 'Waive' }));
    fireEvent.click(screen.getByRole('button', { name: 'Defer' }));
    expect(fetchMock).not.toHaveBeenCalled();
  });

  it('Waive with a tradeoff and its cost POSTs both', async () => {
    const item = itemOf('split_verdict');
    const fetchMock = stubFetch(() => ({ status: 200, body: {} }));
    const { onResolved } = renderPanel(item);
    chooseJudgement('tradeoff');
    fireEvent.change(screen.getByLabelText('Tradeoff cost'), {
      target: { value: ' one extra query per page ' },
    });
    fireEvent.change(screen.getByLabelText('Waive reason'), { target: { value: 'accepted' } });
    fireEvent.click(screen.getByRole('button', { name: 'Waive' }));

    await waitFor(() => expect(onResolved).toHaveBeenCalledTimes(1));
    expect(bodyOf(postCalls(fetchMock)[0])).toEqual({
      reason: 'accepted',
      judgement: 'tradeoff',
      tradeoff_cost: 'one extra query per page',
    });
  });

  it('Defer with a tradeoff and its cost POSTs both', async () => {
    const fetchMock = stubFetch(() => ({ status: 200, body: { concern: {}, issue: {} } }));
    renderPanel(itemOf('paged_concern'));
    chooseJudgement('tradeoff');
    fireEvent.change(screen.getByLabelText('Tradeoff cost'), { target: { value: 'slower CI' } });
    fireEvent.change(screen.getByLabelText(/defer: parent epic/i), { target: { value: '#1196' } });
    fireEvent.click(screen.getByRole('button', { name: 'Defer' }));

    await waitFor(() => expect(postCalls(fetchMock)).toHaveLength(1));
    expect(bodyOf(postCalls(fetchMock)[0])).toEqual({
      judgement: 'tradeoff',
      tradeoff_cost: 'slower CI',
      parent_epic: '#1196',
    });
  });

  it('a cost typed under tradeoff is NOT sent after switching to another judgement', async () => {
    const fetchMock = stubFetch(() => ({ status: 200, body: {} }));
    renderPanel(itemOf('split_verdict'));
    chooseJudgement('tradeoff');
    fireEvent.change(screen.getByLabelText('Tradeoff cost'), { target: { value: 'stray' } });
    chooseJudgement('stale');
    fireEvent.change(screen.getByLabelText('Waive reason'), { target: { value: 'gone' } });
    fireEvent.click(screen.getByRole('button', { name: 'Waive' }));

    await waitFor(() => expect(postCalls(fetchMock)).toHaveLength(1));
    const body = bodyOf(postCalls(fetchMock)[0]);
    expect(body).toEqual({ reason: 'gone', judgement: 'stale' });
    expect('tradeoff_cost' in body).toBe(false);
  });

  it('a 400 judgement refusal (e.g. a contradicting reason prefix) surfaces, no resolve', async () => {
    stubFetch(() => ({
      status: 400,
      body: {
        error: 'validation_failed',
        message: 'judgement "defect" contradicts reason prefix "false_positive:"',
        details: { field: 'judgement', rule: 'judgement_conflicts_reason_prefix' },
      },
    }));
    const { onResolved } = renderPanel(itemOf('split_verdict'));
    chooseJudgement('defect');
    fireEvent.change(screen.getByLabelText('Waive reason'), {
      target: { value: 'false_positive: not reachable' },
    });
    fireEvent.click(screen.getByRole('button', { name: 'Waive' }));
    await screen.findByRole('alert');
    expect(screen.getByRole('alert')).toHaveTextContent(/400 · validation_failed/);
    expect(onResolved).not.toHaveBeenCalled();
  });
});

describe('DecisionPanel — acknowledge gate (C4)', () => {
  beforeEach(() => vi.unstubAllGlobals());
  afterEach(() => vi.unstubAllGlobals());

  it('Record arbitration is inert with the ack box unticked when criteria_failed > 0', () => {
    const fetchMock = stubFetch(() => ({ status: 200, body: {} }));
    renderPanel(itemOf('acceptance_disposition'));
    fireEvent.change(screen.getByLabelText(/arbitration reason/i), { target: { value: 'ship' } });
    // ack box deliberately NOT ticked
    fireEvent.click(screen.getByRole('button', { name: 'Record arbitration' }));
    expect(fetchMock).not.toHaveBeenCalled();
  });
});

describe('DecisionPanel — failure modes leave the item (onResolved not called)', () => {
  beforeEach(() => vi.unstubAllGlobals());
  afterEach(() => vi.unstubAllGlobals());

  it('F1: scope_amendment 409 amendment_already_decided surfaces the error, no resolve', async () => {
    stubFetch(() => ({ status: 409, body: { error: 'amendment_already_decided' } }));
    const { onResolved } = renderPanel(itemOf('scope_amendment'));
    fireEvent.click(screen.getByRole('button', { name: 'Approve' }));
    await screen.findByRole('alert');
    expect(screen.getByRole('alert')).toHaveTextContent(/409 · amendment_already_decided/);
    expect(onResolved).not.toHaveBeenCalled();
  });

  it('F2: concern waive 422 concern_waive_conflict surfaces the error, no resolve', async () => {
    stubFetch(() => ({ status: 422, body: { error: 'concern_waive_conflict' } }));
    const { onResolved } = renderPanel(itemOf('split_verdict'));
    chooseJudgement('no_defect');
    fireEvent.change(screen.getByLabelText('Waive reason'), { target: { value: 'x' } });
    fireEvent.click(screen.getByRole('button', { name: 'Waive' }));
    await screen.findByRole('alert');
    expect(screen.getByRole('alert')).toHaveTextContent(/422 · concern_waive_conflict/);
    expect(onResolved).not.toHaveBeenCalled();
  });

  it('F3: concern defer 502 work_item_filing_failed surfaces the error, no resolve', async () => {
    stubFetch(() => ({ status: 502, body: { error: 'work_item_filing_failed' } }));
    const { onResolved } = renderPanel(itemOf('split_verdict'));
    chooseJudgement('defect');
    fireEvent.change(screen.getByLabelText(/defer: parent epic/i), { target: { value: '#1196' } });
    fireEvent.click(screen.getByRole('button', { name: 'Defer' }));
    await screen.findByRole('alert');
    expect(screen.getByRole('alert')).toHaveTextContent(/502 · work_item_filing_failed/);
    expect(onResolved).not.toHaveBeenCalled();
  });

  it('F4: acceptance 409 requires_acknowledgement surfaces the error, no resolve', async () => {
    stubFetch(() => ({
      status: 409,
      body: { error: 'acceptance_arbitration_requires_acknowledgement' },
    }));
    const { onResolved } = renderPanel(itemOf('acceptance_disposition'));
    fireEvent.change(screen.getByLabelText(/arbitration reason/i), { target: { value: 'ship' } });
    fireEvent.click(screen.getByLabelText(/acknowledge merging despite 2 failed/i));
    fireEvent.click(screen.getByRole('button', { name: 'Record arbitration' }));
    await screen.findByRole('alert');
    expect(screen.getByRole('alert')).toHaveTextContent(
      /acceptance_arbitration_requires_acknowledgement/,
    );
    expect(onResolved).not.toHaveBeenCalled();
  });
});

describe('DecisionPanel — fail-closed missing id (F6 / C5)', () => {
  beforeEach(() => vi.unstubAllGlobals());
  afterEach(() => vi.unstubAllGlobals());

  it('scope_amendment with amendment_id absent renders a refusal, no button, zero fetches', () => {
    const item = itemOf('scope_amendment');
    delete item.amendment_id;
    const fetchMock = stubFetch(() => ({ status: 200, body: {} }));
    renderPanel(item);
    expect(screen.getByRole('note')).toHaveTextContent(/missing its run or amendment id/i);
    expect(screen.queryByRole('button')).not.toBeInTheDocument();
    expect(fetchMock).not.toHaveBeenCalled();
  });

  it('concern with concern_id absent renders a refusal, no button, zero fetches', () => {
    const item = itemOf('split_verdict');
    delete item.concern_id;
    const fetchMock = stubFetch(() => ({ status: 200, body: {} }));
    renderPanel(item);
    expect(screen.getByRole('note')).toHaveTextContent(/missing its concern id/i);
    expect(screen.queryByRole('button')).not.toBeInTheDocument();
    expect(fetchMock).not.toHaveBeenCalled();
  });
});

describe('DecisionPanel — plan_gate reuses ApprovalPanel', () => {
  beforeEach(() => vi.unstubAllGlobals());
  afterEach(() => vi.unstubAllGlobals());

  it('renders exactly the plan_gate verbs (Approve / Reject) via ApprovalPanel, no drive-plane verb', async () => {
    stubFetch((url) => {
      if (url.includes('/stages/')) return { status: 200, body: stageAwaiting };
      return { status: 404, body: {} };
    });
    const { container } = render(
      <MemoryRouter>
        <DecisionPanel item={itemOf('plan_gate')} onResolved={vi.fn()} onCancel={vi.fn()} />
      </MemoryRouter>,
    );
    await screen.findByRole('button', { name: /^approve$/i });
    const verbLabels = Array.from(container.querySelectorAll('[data-decision-verb]'))
      .map((el) => (el.textContent ?? '').trim())
      .sort();
    expect(verbLabels).toEqual([...DECISION_VERBS.plan_gate].sort());
    // Regenerate (a drive-plane verb) is suppressed; no rendered label matches.
    for (const el of [...screen.queryAllByRole('button'), ...screen.queryAllByRole('link')]) {
      expect(isDrivePlaneVerb(el.textContent ?? '')).toBe(false);
    }
  });
});
