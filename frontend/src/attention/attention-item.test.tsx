import { readFileSync } from 'node:fs';
import { resolve } from 'node:path';
import { describe, expect, it } from 'vitest';
import { render, screen, within } from '@testing-library/react';
import { MemoryRouter } from 'react-router';
import { AttentionItemCard } from './attention-item';
import type { AttentionItem, AttentionItemKind, AttentionList } from '@/api/types';

/*
 * Per-kind fixtures come from the SHARED wire golden the backend's
 * TestAttention_EndToEnd_AllSixKinds pins (testdata/wire/attention_list.json)
 * — not a hand-authored copy — so a field rename on either side of the wire
 * reddens a test here.
 */
const golden = JSON.parse(
  readFileSync(resolve(__dirname, '../../../testdata/wire/attention_list.json'), 'utf8'),
) as AttentionList;

function itemOf(kind: AttentionItemKind): AttentionItem {
  const item = golden.items.find((i) => i.kind === kind);
  if (!item) throw new Error(`golden carries no ${kind} item`);
  return item;
}

function renderCard(item: AttentionItem) {
  return render(
    <MemoryRouter>
      <AttentionItemCard item={item} />
    </MemoryRouter>,
  );
}

const ALL_KINDS: AttentionItemKind[] = [
  'plan_gate',
  'scope_amendment',
  'acceptance_disposition',
  'split_verdict',
  'paged_concern',
  'attend_human_led_campaign',
];

describe('<AttentionItemCard>', () => {
  it('renders the plan summary and each recorded plan-review verdict', () => {
    renderCard(itemOf('plan_gate'));
    expect(screen.getByText('Plan gate')).toBeInTheDocument();
    expect(
      screen.getByText('Add GET /v0/attention and make it the SPA home page.'),
    ).toBeInTheDocument();
    expect(screen.getByText(/claude-opus: approve · 1 concern$/)).toBeInTheDocument();
  });

  it("renders the agent's amendment reason and every requested path", () => {
    renderCard(itemOf('scope_amendment'));
    expect(screen.getByText('the registry must list the new audit category')).toBeInTheDocument();
    expect(screen.getByText('modify backend/internal/audit/categories.go')).toBeInTheDocument();
  });

  it('renders the acceptance verdict and the failing/skipped criteria counts', () => {
    renderCard(itemOf('acceptance_disposition'));
    expect(screen.getByText('failed')).toBeInTheDocument();
    expect(screen.getByText('2 failed · 1 skipped')).toBeInTheDocument();
  });

  it('names which acceptance criteria failed and each one’s failing request', () => {
    const item = itemOf('acceptance_disposition');
    const failed = item.context.failed_criteria!;
    expect(failed.length).toBeGreaterThan(0);
    const { container } = renderCard(item);
    for (const c of failed) {
      // The card shows the criterion id and the request whose response the
      // failing assertion evaluated — the decision-relevant explanation.
      expect(container).toHaveTextContent(c.id);
      if (c.path) expect(container).toHaveTextContent(c.path);
      if (c.status) expect(container).toHaveTextContent(String(c.status));
    }
  });

  it("renders a split verdict's FULL note and the re-review that recorded it resolved", () => {
    renderCard(itemOf('split_verdict'));
    expect(
      screen.getByText(
        'The retry loop never re-reads the lease, so a stale holder is never displaced.',
      ),
    ).toBeInTheDocument();
    expect(screen.getByText('fixed by the lease re-read')).toBeInTheDocument();
    expect(screen.getByText('high · correctness · implement gate')).toBeInTheDocument();
  });

  it('renders a paged concern without the split-only confirmation block', () => {
    renderCard(itemOf('paged_concern'));
    expect(
      screen.getByText('The degraded branch has no test asserting its reason string.'),
    ).toBeInTheDocument();
    expect(screen.queryByText(/recorded as resolved/i)).not.toBeInTheDocument();
  });

  it("renders a human-led campaign's epic, human-led refs and next-action detail", () => {
    const item = itemOf('attend_human_led_campaign');
    renderCard(item);
    expect(screen.getByText('issue:900')).toBeInTheDocument();
    expect(screen.getByText('issue:901')).toBeInTheDocument();
    expect(screen.getByText(item.context.detail!)).toBeInTheDocument();
  });

  it('renders the split-only dispute reasons and new evidence when present', () => {
    renderCard({
      ...itemOf('split_verdict'),
      context: {
        ...itemOf('split_verdict').context,
        new_evidence: 'the lease file is re-read on every poll',
        dispute_reasons: ['reviewer B still reproduces the stale holder'],
      },
    });
    expect(screen.getByText('the lease file is re-read on every poll')).toBeInTheDocument();
    expect(screen.getByText('reviewer B still reproduces the stale holder')).toBeInTheDocument();
  });

  it.each(ALL_KINDS)('%s links to the server-emitted detail_path and renders no action', (kind) => {
    const item = itemOf(kind);
    const { container } = renderCard(item);
    const card = screen.getByRole('article');
    const links = within(card).getAllByRole('link');
    expect(links).toHaveLength(1);
    expect(links[0]).toHaveAttribute('href', item.detail_path);
    // Read-only control: no button, form or input affordance on any kind.
    expect(screen.queryAllByRole('button')).toHaveLength(0);
    expect(screen.queryAllByRole('form')).toHaveLength(0);
    expect(container.querySelector('button, form, input, select, textarea')).toBeNull();
  });

  it('renders a labelled fallback, not a blank card, for a kind the client does not know', () => {
    renderCard({
      ...itemOf('plan_gate'),
      kind: 'future_kind' as unknown as AttentionItemKind,
    });
    expect(screen.getByText('Unknown item')).toBeInTheDocument();
    expect(screen.getAllByText('future_kind').length).toBeGreaterThan(0);
  });
});
