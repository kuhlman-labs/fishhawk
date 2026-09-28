import { readFileSync } from 'node:fs';
import { resolve } from 'node:path';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { render, screen, within } from '@testing-library/react';
import { api } from '@/api/client';
import type { AsyncState } from '@/api/use-async';
import type { RepoEconomics, RepoEconomicsResponse } from '@/api/types';
import { EconomicsPanel } from './economics-panel';

/*
 * Served from the SHARED wire golden (testdata/wire/repodash_economics.json)
 * through the REAL api client (approval condition 3). Variants are derived
 * from the golden; the `{}` no-cost body is the documented empty shape, not
 * a copy of the golden.
 */
const GOLDEN_BYTES = readFileSync(
  resolve(__dirname, '../../../testdata/wire/repodash_economics.json'),
  'utf8',
);

function golden(): RepoEconomics {
  return JSON.parse(GOLDEN_BYTES) as RepoEconomics;
}

function stubFetch(status: number, body: string) {
  vi.stubGlobal(
    'fetch',
    vi.fn(
      async () => new Response(body, { status, headers: { 'Content-Type': 'application/json' } }),
    ),
  );
}

async function loadEconomics(
  body: string = GOLDEN_BYTES,
): Promise<AsyncState<RepoEconomicsResponse>> {
  stubFetch(200, body);
  return { status: 'ok', data: await api.getRepoEconomics('acme', 'app', { weeks: 4 }) };
}

function statValue(label: string): string | null {
  return screen.getByText(label).nextElementSibling?.textContent ?? null;
}

afterEach(() => vi.unstubAllGlobals());

describe('<EconomicsPanel>', () => {
  it('renders the golden: cost per merged change, budget burn and the weekly cache trend', async () => {
    render(<EconomicsPanel state={await loadEconomics()} />);
    expect(screen.getByRole('heading', { name: 'Economics' })).toBeInTheDocument();
    expect(statValue('Cost per merged change')).toBe('$4.00');
    expect(screen.getByText('1 merged change')).toBeInTheDocument();
    expect(statValue('Total cost')).toBe('$4.00');
    expect(screen.getByText('2 cost entries')).toBeInTheDocument();

    const budget = within(screen.getByRole('table', { name: 'Budget burn' }));
    const row = budget.getByText('feature_change').closest('tr')!;
    expect(within(row).getByText('weekly')).toBeInTheDocument();
    expect(within(row).getByText('$12.50 / $50.00')).toBeInTheDocument();
    expect(within(row).getByText('25%')).toBeInTheDocument();
    expect(row).toHaveTextContent('ok (advisory)');

    const cache = within(screen.getByRole('table', { name: 'Weekly cache efficiency' }));
    expect(cache.getAllByRole('row')).toHaveLength(1 + 4);
    const last = cache.getByText('Sep 28').closest('tr')!;
    expect(within(last).getByText('75%')).toBeInTheDocument();
    expect(within(last).getByText('3.0×')).toBeInTheDocument();
    expect(screen.queryByText(/Partial data/)).toBeNull();
  });

  it('omits the budget table when no workflow declares a periodic budget', async () => {
    const g = golden();
    delete g.budgets;
    render(<EconomicsPanel state={await loadEconomics(JSON.stringify(g))} />);
    expect(screen.queryByRole('table', { name: 'Budget burn' })).toBeNull();
    expect(screen.getByRole('table', { name: 'Weekly cache efficiency' })).toBeInTheDocument();
  });

  it('omits the budget table on an empty budgets list', async () => {
    const g = golden();
    g.budgets = [];
    render(<EconomicsPanel state={await loadEconomics(JSON.stringify(g))} />);
    expect(screen.queryByRole('table', { name: 'Budget burn' })).toBeNull();
  });

  it('renders an all-zero window without a bogus cost per change', async () => {
    const g = golden();
    delete g.budgets;
    g.merged_changes = 0;
    g.cost_per_merged_change_usd = 0;
    g.total_cost_usd = 0;
    g.weeks = g.weeks.map((w) => ({
      ...w,
      cost_usd: 0,
      cache_read_ratio: 0,
      reuse_factor: 0,
      net_savings_usd: 0,
    }));
    render(<EconomicsPanel state={await loadEconomics(JSON.stringify(g))} />);
    expect(statValue('Cost per merged change')).toBe('—');
    expect(screen.getByText('0 merged changes')).toBeInTheDocument();
    expect(statValue('Total cost')).toBe('$0.00');
    expect(document.body.textContent).not.toMatch(/NaN/);
  });

  it('renders the empty `{}` body as "no cost recorded", not an error', async () => {
    render(<EconomicsPanel state={await loadEconomics('{}')} />);
    expect(screen.getByText('No cost recorded in this window.')).toBeInTheDocument();
    expect(screen.queryByRole('alert')).toBeNull();
    expect(screen.queryByText(/Partial data/)).toBeNull();
  });

  it('shows the partial-data note on a truncated empty body', async () => {
    render(<EconomicsPanel state={await loadEconomics('{"truncated":true}')} />);
    expect(screen.getByText(/Partial data/)).toBeInTheDocument();
    expect(screen.getByText('No cost recorded in this window.')).toBeInTheDocument();
  });

  it('shows the partial-data note on a truncated populated body', async () => {
    const g = golden();
    g.truncated = true;
    render(<EconomicsPanel state={await loadEconomics(JSON.stringify(g))} />);
    expect(screen.getByText(/Partial data/)).toBeInTheDocument();
    expect(statValue('Total cost')).toBe('$4.00');
  });

  it('renders a panel-scoped alert when the fetch fails', async () => {
    stubFetch(500, '{"error":"internal","message":"audit read failed"}');
    const error = await api.getRepoEconomics('acme', 'app').catch((e: Error) => e);
    render(<EconomicsPanel state={{ status: 'error', error: error as Error }} />);
    expect(screen.getByRole('alert')).toHaveTextContent('audit read failed');
  });
});
