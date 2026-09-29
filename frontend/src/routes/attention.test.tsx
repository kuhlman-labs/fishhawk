import { readFileSync } from 'node:fs';
import { resolve } from 'node:path';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { fireEvent, render, screen, waitFor, within } from '@testing-library/react';
import { MemoryRouter } from 'react-router';
import { Attention, ATTENTION_EMPTY_TEXT, ATTENTION_INCOMPLETE_TEXT } from './attention';
import { api } from '@/api/client';
import { isDrivePlaneVerb } from '@/attention/decision-verbs';
import type { AttentionItem, AttentionList } from '@/api/types';

/*
 * The shared wire golden (testdata/wire/attention_list.json) is the
 * END-TO-END body the backend's TestAttention_EndToEnd_AllSixKinds pins.
 * Read as raw BYTES so the round-trip below goes through the real
 * api.listAttention decode, not a hand-authored copy of the payload.
 */
const GOLDEN_BYTES = readFileSync(
  resolve(__dirname, '../../../testdata/wire/attention_list.json'),
  'utf8',
);
const golden = JSON.parse(GOLDEN_BYTES) as AttentionList;

/*
 * Every field name the SPA mirrors (src/api/types.ts). A key in the golden
 * that is not listed here means the backend renamed or added a field the
 * SPA does not know — fail rather than silently ignore it.
 */
const ITEM_KEYS = new Set([
  'id',
  'kind',
  'priority',
  'run_id',
  'campaign_id',
  'stage_id',
  'concern_id',
  'amendment_id',
  'repo',
  'title',
  'context',
  'detail_path',
  'since',
]);
const CONTEXT_KEYS = new Set([
  'plan_summary',
  'review_verdicts',
  'reason',
  'requested_paths',
  'verdict',
  'criteria_failed',
  'criteria_skipped',
  'failed_criteria',
  'stage_kind',
  'severity',
  'category',
  'reviewer_model',
  'note',
  'new_evidence',
  'dispute_reasons',
  'confirmation_note',
  'epic_ref',
  'human_led_refs',
  'detail',
]);

function renderPage() {
  return render(
    <MemoryRouter>
      <Attention />
    </MemoryRouter>,
  );
}

function list(over: Partial<AttentionList> = {}): AttentionList {
  return { items: [], degraded: [], truncated: false, scanned_runs: 0, ...over };
}

describe('<Attention>', () => {
  beforeEach(() => {
    vi.restoreAllMocks();
    vi.unstubAllGlobals();
  });
  afterEach(() => {
    vi.restoreAllMocks();
    vi.unstubAllGlobals();
  });

  it('renders the Needs You heading and a loading state', () => {
    vi.spyOn(api, 'listAttention').mockReturnValue(new Promise(() => {}));
    renderPage();
    expect(screen.getByRole('heading', { name: 'Needs You' })).toBeInTheDocument();
    expect(screen.getByText(/loading the attention queue/i)).toBeInTheDocument();
  });

  it('renders the error envelope when the fetch fails', async () => {
    vi.spyOn(api, 'listAttention').mockRejectedValue(new Error('run_repo_unconfigured'));
    renderPage();
    const alert = await screen.findByRole('alert');
    expect(alert).toHaveTextContent(/couldn.t load the attention queue/i);
    expect(alert).toHaveTextContent('run_repo_unconfigured');
  });

  it('renders the all-clear empty state only for a COMPLETE empty list', async () => {
    vi.spyOn(api, 'listAttention').mockResolvedValue(list());
    renderPage();
    expect(await screen.findByText(ATTENTION_EMPTY_TEXT)).toBeInTheDocument();
    expect(screen.queryByText(ATTENTION_INCOMPLETE_TEXT)).not.toBeInTheDocument();
  });

  it('shows the incomplete banner, NOT the all-clear, when truncated with zero items', async () => {
    vi.spyOn(api, 'listAttention').mockResolvedValue(list({ truncated: true }));
    renderPage();
    expect(await screen.findByText(ATTENTION_INCOMPLETE_TEXT)).toBeInTheDocument();
    expect(screen.queryByText(ATTENTION_EMPTY_TEXT)).not.toBeInTheDocument();
  });

  it('shows the incomplete banner naming each degraded reason when degraded with zero items', async () => {
    vi.spyOn(api, 'listAttention').mockResolvedValue(
      list({
        degraded: [
          { reason: 'campaign_scan_truncated', detail: 'scanned 50 campaigns' },
          { reason: 'stage_read_failed', run_id: 'run-1' },
        ],
      }),
    );
    renderPage();
    const banner = await screen.findByRole('status');
    expect(banner).toHaveTextContent(ATTENTION_INCOMPLETE_TEXT);
    expect(banner).toHaveTextContent('campaign_scan_truncated — scanned 50 campaigns');
    expect(banner).toHaveTextContent('stage_read_failed · run run-1');
    expect(screen.queryByText(ATTENTION_EMPTY_TEXT)).not.toBeInTheDocument();
  });

  it('keeps the incomplete banner alongside a populated list', async () => {
    vi.spyOn(api, 'listAttention').mockResolvedValue({
      ...golden,
      degraded: [{ reason: 'concern_store_unconfigured' }],
    });
    renderPage();
    expect(await screen.findByText(ATTENTION_INCOMPLETE_TEXT)).toBeInTheDocument();
    expect(screen.getAllByRole('article')).toHaveLength(golden.items.length);
  });

  it('renders the shared wire golden end to end through api.listAttention', async () => {
    const fetchMock = vi.fn(
      async () =>
        new Response(GOLDEN_BYTES, {
          status: 200,
          headers: { 'Content-Type': 'application/json' },
        }),
    );
    vi.stubGlobal('fetch', fetchMock);
    renderPage();

    const cards = await screen.findAllByRole('article');
    expect(String((fetchMock.mock.calls[0] as unknown[])[0])).toBe('/v0/attention');

    // Six kinds, one card each, in the server's rank order.
    expect(golden.items.map((i) => i.kind)).toEqual([
      'plan_gate',
      'scope_amendment',
      'acceptance_disposition',
      'split_verdict',
      'paged_concern',
      'attend_human_led_campaign',
    ]);
    expect(cards).toHaveLength(6);

    // Every card links to exactly its item's server-emitted detail_path.
    golden.items.forEach((item, i) => {
      const links = within(cards[i]).getAllByRole('link');
      expect(links).toHaveLength(1);
      expect(links[0]).toHaveAttribute('href', item.detail_path);
    });

    // Each kind's one-screen context prose reaches the page.
    const [plan, amend, accept, split, paged, campaign] = cards;
    expect(plan).toHaveTextContent(golden.items[0].context.plan_summary!);
    expect(plan).toHaveTextContent('claude-opus: approve · 1 concern');
    expect(amend).toHaveTextContent(golden.items[1].context.reason!);
    expect(amend).toHaveTextContent(
      `${golden.items[1].context.requested_paths![0].operation} ${golden.items[1].context.requested_paths![0].path}`,
    );
    expect(accept).toHaveTextContent(golden.items[2].context.verdict!);
    expect(accept).toHaveTextContent('2 failed · 1 skipped');
    // The card names WHICH criteria failed and each one's failing request
    // (the decision-relevant explanation) from the shared golden, so an
    // operator sees the disposition without a detail-page fetch.
    const failed = golden.items[2].context.failed_criteria!;
    expect(failed.length).toBeGreaterThan(0);
    failed.forEach((c) => {
      expect(accept).toHaveTextContent(c.id);
      if (c.path) expect(accept).toHaveTextContent(c.path);
      if (c.status) expect(accept).toHaveTextContent(String(c.status));
    });
    expect(split).toHaveTextContent(golden.items[3].context.note!);
    expect(split).toHaveTextContent(golden.items[3].context.confirmation_note!);
    expect(paged).toHaveTextContent(golden.items[4].context.note!);
    expect(campaign).toHaveTextContent(golden.items[5].context.epic_ref!);
    expect(campaign).toHaveTextContent(golden.items[5].context.human_led_refs![0]);
    expect(campaign).toHaveTextContent(golden.items[5].context.detail!);

    // A complete golden renders no incomplete banner. The queue is now
    // actionable (E40.2), but NO rendered button or link is a drive-plane verb.
    expect(screen.queryByText(ATTENTION_INCOMPLETE_TEXT)).not.toBeInTheDocument();
    for (const el of [...screen.queryAllByRole('button'), ...screen.queryAllByRole('link')]) {
      expect(isDrivePlaneVerb(el.textContent ?? ''), `"${el.textContent}"`).toBe(false);
    }
  });

  it('the wire golden carries only field names the SPA mirrors', () => {
    expect(Object.keys(golden).sort()).toEqual(
      ['degraded', 'items', 'scanned_runs', 'truncated'].sort(),
    );
    for (const item of golden.items) {
      for (const key of Object.keys(item)) {
        expect(ITEM_KEYS.has(key), `unknown AttentionItem field "${key}"`).toBe(true);
      }
      for (const key of Object.keys(item.context)) {
        expect(CONTEXT_KEYS.has(key), `unknown AttentionContext field "${key}"`).toBe(true);
      }
    }
  });

  describe('decision write surface (E40.2 / #1717)', () => {
    function scopeItem(over: Partial<AttentionItem>): AttentionItem {
      const base = golden.items.find((i) => i.kind === 'scope_amendment')!;
      return structuredClone({ ...base, ...over });
    }

    function stubDecision(handler: (url: string) => { status: number; body: unknown }) {
      const fetchMock = vi.fn(async (input: RequestInfo | URL) => {
        const r = handler(String(input));
        return new Response(JSON.stringify(r.body), {
          status: r.status,
          headers: { 'Content-Type': 'application/json' },
        });
      });
      vi.stubGlobal('fetch', fetchMock);
      return fetchMock;
    }

    function cardOf(title: string): HTMLElement {
      const el = screen.getByText(title).closest('article');
      if (!el) throw new Error(`no card for "${title}"`);
      return el as HTMLElement;
    }

    it('submitting one of two same-kind items removes ONLY it, with no refetch', async () => {
      const a = scopeItem({ id: 'id-a', amendment_id: 'am-a', title: 'Amendment A' });
      const b = scopeItem({ id: 'id-b', amendment_id: 'am-b', title: 'Amendment B' });
      const listSpy = vi.spyOn(api, 'listAttention').mockResolvedValue(list({ items: [a, b] }));
      const fetchMock = stubDecision(() => ({ status: 200, body: {} }));
      renderPage();

      await screen.findByText('Amendment A');
      const cardA = cardOf('Amendment A');
      fireEvent.click(within(cardA).getByRole('button', { name: /^decide$/i }));
      fireEvent.click(within(cardA).getByRole('button', { name: 'Approve' }));

      await waitFor(() => expect(screen.queryByText('Amendment A')).not.toBeInTheDocument());
      // The sibling with the same kind but a different id is untouched.
      expect(screen.getByText('Amendment B')).toBeInTheDocument();
      // Exactly one POST landed, and the queue was NOT refetched or reloaded.
      const posts = fetchMock.mock.calls.filter(([, i]) => (i as RequestInit)?.method === 'POST');
      expect(posts).toHaveLength(1);
      expect(String(posts[0][0])).toBe('/v0/runs/' + a.run_id + '/scope-amendments/am-a/decision');
      expect(listSpy).toHaveBeenCalledTimes(1);
    });

    it('a failed concern waive (422) leaves the item in the list (C3)', async () => {
      const split = golden.items.find((i) => i.kind === 'split_verdict')!;
      vi.spyOn(api, 'listAttention').mockResolvedValue(list({ items: [structuredClone(split)] }));
      stubDecision(() => ({ status: 422, body: { error: 'concern_waive_conflict' } }));
      renderPage();

      await screen.findByText(split.title);
      const card = cardOf(split.title);
      fireEvent.click(within(card).getByRole('button', { name: /^decide$/i }));
      fireEvent.change(within(card).getByLabelText('Waive reason'), {
        target: { value: 'still open' },
      });
      fireEvent.click(within(card).getByRole('button', { name: 'Waive' }));

      await within(card).findByRole('alert');
      // The waive was rejected, so the item MUST remain in the queue.
      expect(screen.getByText(split.title)).toBeInTheDocument();
    });
  });

  it('does not render the empty state while still loading', async () => {
    let resolve!: (v: AttentionList) => void;
    vi.spyOn(api, 'listAttention').mockReturnValue(new Promise((r) => (resolve = r)));
    renderPage();
    expect(screen.queryByText(ATTENTION_EMPTY_TEXT)).not.toBeInTheDocument();
    resolve(list());
    await waitFor(() => expect(screen.getByText(ATTENTION_EMPTY_TEXT)).toBeInTheDocument());
  });
});
