import { describe, expect, it } from 'vitest';
import { render, screen, within } from '@testing-library/react';
import { MemoryRouter } from 'react-router';
import { ApiClientError } from '@/api/client';
import type { HandoverBrief, HandoverBriefPart } from '@/api/captain';
import { HandoverBriefPanel } from './handover-brief';

/*
 * Literal `HandoverBrief` wire bodies (docs/api/v0.openapi.yaml). The
 * load-bearing case is part UNAVAILABILITY: an unavailable part's `items` is
 * `[]` either way, so only the guard tells "the read failed" from "nothing
 * happened" (counterfactual 6).
 */

const RUN = '11111111-2222-4333-8444-555555555555';
const CAMPAIGN = '99999999-8888-4777-8666-555555555555';
const HASH = 'b'.repeat(64);

function part(
  over: Partial<HandoverBriefPart> & Pick<HandoverBriefPart, 'kind'>,
): HandoverBriefPart {
  return { complete: true, truncated: false, omitted_count: 0, ...over };
}

function brief(over: Partial<HandoverBrief> = {}): HandoverBrief {
  return {
    repo: 'acme/app',
    captain_subject: 'github:octo',
    window: { from_sequence: 10, to_sequence: 40, chain_head: 40, basis: 'since_last_handover' },
    sections: [
      {
        kind: 'standing_orders',
        parts: [],
        standing_orders: {
          source: 'run_cache',
          schema_major: 2,
          spec_version: '2.1',
          delegation_content_hash: 'd'.repeat(64),
        },
      },
      {
        kind: 'what_changed',
        parts: [
          part({
            kind: 'merges',
            items: [
              {
                category: 'pr_merged',
                source_sequence: 17,
                source_entry_hash: 'e'.repeat(64),
                run_id: RUN,
                at: '2026-09-21T00:00:00Z',
                headline: 'feat: add bridge',
              },
            ],
          }),
        ],
      },
      {
        kind: 'in_flight',
        parts: [
          part({
            kind: 'campaigns',
            in_flight: [
              {
                kind: 'campaign',
                id: CAMPAIGN,
                state: 'running',
                created_at: '2026-09-22T00:00:00Z',
              },
            ],
          }),
          part({
            kind: 'runs',
            in_flight: [
              {
                kind: 'run',
                id: RUN,
                state: 'running',
                workflow_id: 'feature_change',
                created_at: '2026-09-22T01:00:00Z',
              },
            ],
          }),
        ],
      },
      {
        kind: 'delegation_in_force',
        parts: [
          part({
            kind: 'workflows',
            workflows: [
              {
                workflow_id: 'feature_change',
                autonomy: 'medium',
                must_page_human: ['budget_exceeded'],
                content_hash: 'c'.repeat(64),
                confirmation: 'unavailable',
              },
            ],
          }),
        ],
      },
    ],
    absent: [{ section: 'charter_revision', reason: 'no source on main', anchor: '#3242' }],
    gaps: [{ kind: 'source_entry_missing', sequence: 22, category: 'waiver_recorded' }],
    degradations: [{ kind: 'campaign_read_failed', section: 'in_flight', part: 'campaigns' }],
    gaps_truncated: false,
    gaps_omitted_count: 0,
    brief_hash: HASH,
    truncated: false,
    ...over,
  };
}

function renderBrief(b: HandoverBrief) {
  return render(
    <MemoryRouter>
      <HandoverBriefPanel state={{ status: 'ok', data: b }} />
    </MemoryRouter>,
  );
}

function partEl(kind: string): HTMLElement {
  return document.querySelector(`[data-part="${kind}"]`) as HTMLElement;
}

describe('<HandoverBriefPanel>', () => {
  it('renders the window and the truncated brief hash', () => {
    renderBrief(brief());
    expect(
      screen.getByText('#10 → #40 (chain head #40) · since_last_handover'),
    ).toBeInTheDocument();
    expect(screen.getByTestId('brief-hash')).toHaveTextContent(`${'b'.repeat(12)}…`);
  });

  it('renders sections in the fixed order regardless of wire order', () => {
    renderBrief(brief());
    const headings = screen.getAllByRole('heading', { level: 3 }).map((h) => h.textContent);
    expect(headings.slice(0, 4)).toEqual([
      'What changed',
      'In flight',
      'Delegation in force',
      'Standing orders',
    ]);
  });

  it('renders a digest item as a cited row with a run link', () => {
    renderBrief(brief());
    const row = within(partEl('merges'));
    expect(row.getByText('pr_merged')).toBeInTheDocument();
    expect(row.getByText(`#17 · ${'e'.repeat(12)}…`)).toBeInTheDocument();
    expect(row.getByRole('link', { name: 'run 11111111' })).toHaveAttribute('href', `/runs/${RUN}`);
    expect(row.getByText('feat: add bridge')).toBeInTheDocument();
  });

  it('links in-flight campaigns and runs to their detail routes', () => {
    renderBrief(brief());
    expect(within(partEl('campaigns')).getByRole('link')).toHaveAttribute(
      'href',
      `/campaigns/${CAMPAIGN}`,
    );
    expect(within(partEl('runs')).getByRole('link')).toHaveAttribute('href', `/runs/${RUN}`);
  });

  it('renders workflows with the constant confirmation as "not read by the brief"', () => {
    renderBrief(brief());
    const wf = within(partEl('workflows'));
    expect(wf.getByText('feature_change')).toBeInTheDocument();
    expect(wf.getByText('autonomy medium')).toBeInTheDocument();
    expect(wf.getByText('pages on budget_exceeded')).toBeInTheDocument();
    expect(wf.getByText(/confirmation not read by the brief/)).toBeInTheDocument();
  });

  it('an unavailable part renders its reason, never an empty "nothing happened" part', () => {
    renderBrief(
      brief({
        sections: [
          {
            kind: 'what_changed',
            parts: [
              part({
                kind: 'merges',
                items: [],
                complete: true,
                unavailable: true,
                unavailable_reason: 'digest_read_failed',
              }),
            ],
          },
        ],
      }),
    );
    const p = within(partEl('merges'));
    expect(p.getByText(/Unavailable: digest_read_failed/)).toBeInTheDocument();
    expect(p.queryByText('Nothing in this window.')).toBeNull();
  });

  it('an available empty part reads as a quiet window', () => {
    renderBrief(
      brief({ sections: [{ kind: 'what_changed', parts: [part({ kind: 'merges', items: [] })] }] }),
    );
    expect(within(partEl('merges')).getByText('Nothing in this window.')).toBeInTheDocument();
  });

  it('a section whose every part is unavailable says so', () => {
    renderBrief(
      brief({
        sections: [
          {
            kind: 'needs_decision',
            unavailable: true,
            parts: [part({ kind: 'open_decisions', unavailable: true, unavailable_reason: 'x' })],
          },
        ],
      }),
    );
    expect(screen.getByText('Every part of this section is unavailable.')).toBeInTheDocument();
  });

  it('a truncated part renders the omitted count and the next call verbatim', () => {
    const call = 'GET /v0/digest?from_sequence=10&repo=acme%2Fapp&section=merges&to_sequence=40';
    renderBrief(
      brief({
        sections: [
          {
            kind: 'what_changed',
            parts: [
              part({
                kind: 'merges',
                items: [],
                complete: false,
                truncated: true,
                omitted_count: 7,
                next: { part: 'merges', call },
              }),
            ],
          },
        ],
      }),
    );
    const p = within(partEl('merges'));
    expect(p.getByText(/7 more omitted\./)).toBeInTheDocument();
    expect(p.getByText(call)).toBeInTheDocument();
    expect(p.queryByText('Nothing in this window.')).toBeNull();
  });

  it('a truncated brief renders its own next cursor', () => {
    renderBrief(
      brief({ truncated: true, next: { part: 'brief', call: 'GET /v0/handover-brief?x' } }),
    );
    expect(screen.getByText(/The brief was bounded/)).toBeInTheDocument();
    expect(screen.getByText('GET /v0/handover-brief?x')).toBeInTheDocument();
    // The run-scan partial-data note is the wrong claim for a brief.
    expect(screen.queryByText(/run scan hit its ceiling/)).toBeNull();
  });

  it('renders gaps, degradations and absent sections as named lists', () => {
    renderBrief(
      brief({
        gaps_truncated: true,
        gaps_omitted_count: 3,
        gaps_next: { part: 'gaps', call: 'GET g' },
      }),
    );
    expect(screen.getByRole('heading', { name: 'Gaps' })).toBeInTheDocument();
    expect(screen.getByText('source_entry_missing')).toBeInTheDocument();
    expect(screen.getByRole('heading', { name: 'Degradations' })).toBeInTheDocument();
    expect(screen.getByText('campaign_read_failed')).toBeInTheDocument();
    expect(screen.getByText('charter_revision')).toBeInTheDocument();
    expect(screen.getByText(/3 more gaps omitted\./)).toBeInTheDocument();
    expect(screen.getByText('GET g')).toBeInTheDocument();
  });

  it.each([
    [503, 'handover_brief_unavailable', 'window_read_failed', 'could not be established'],
    [501, 'handover_brief_unconfigured', 'digest_store missing', 'not configured on this instance'],
  ])('a %i %s renders the named reason', (status, code, message, sentence) => {
    const err = new ApiClientError(status, { error: code, message }, message);
    render(
      <MemoryRouter>
        <HandoverBriefPanel state={{ status: 'error', error: err }} />
      </MemoryRouter>,
    );
    const note = screen.getByRole('note');
    expect(note).toHaveTextContent(sentence);
    expect(note).toHaveTextContent(`${status} · ${code}`);
    expect(note).toHaveTextContent(message);
    expect(screen.queryByRole('alert')).toBeNull();
  });
});
