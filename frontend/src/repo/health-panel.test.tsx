import { readFileSync } from 'node:fs';
import { resolve } from 'node:path';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { render, screen, within } from '@testing-library/react';
import { api } from '@/api/client';
import type { AsyncState } from '@/api/use-async';
import type { RepoHealth } from '@/api/types';
import { HealthPanel, Rate } from './health-panel';

/*
 * Served from the SHARED wire golden (testdata/wire/repodash_health.json)
 * through the REAL api client (approval condition 3). Variants are derived
 * from the golden, never a second hand-authored payload.
 */
const GOLDEN_BYTES = readFileSync(
  resolve(__dirname, '../../../testdata/wire/repodash_health.json'),
  'utf8',
);

function golden(): RepoHealth {
  return JSON.parse(GOLDEN_BYTES) as RepoHealth;
}

function stubFetch(status: number, body: string) {
  vi.stubGlobal(
    'fetch',
    vi.fn(
      async () => new Response(body, { status, headers: { 'Content-Type': 'application/json' } }),
    ),
  );
}

async function loadHealth(body: string = GOLDEN_BYTES): Promise<AsyncState<RepoHealth>> {
  stubFetch(200, body);
  return { status: 'ok', data: await api.getRepoHealth('acme', 'app', { weeks: 4 }) };
}

function statValue(label: string): string | null {
  return screen.getByText(label).nextElementSibling?.textContent ?? null;
}

function categoryCount(c: string): string | null {
  const table = within(screen.getByRole('table', { name: 'Failure categories' }));
  return table.getByText(c).closest('tr')!.lastElementChild!.textContent;
}

afterEach(() => vi.unstubAllGlobals());

describe('<HealthPanel>', () => {
  it('renders the golden rates with their n-of-m and the failure mix', async () => {
    render(<HealthPanel state={await loadHealth()} />);
    expect(screen.getByRole('heading', { name: 'Health' })).toBeInTheDocument();
    expect(statValue('Plan first-shot approval')).toBe('100%');
    expect(screen.getByText('1 of 1 plans')).toBeInTheDocument();
    expect(statValue('Fixup rate')).toBe('50%');
    expect(screen.getByText('1 of 2 runs')).toBeInTheDocument();
    expect(statValue('Acceptance pass rate')).toBe('100%');
    expect(screen.getByText('1 of 1 verdicts')).toBeInTheDocument();
    expect(
      screen.getByText(/1 passed · 0 not\s+validated · 0 failed · 0 undecidable/),
    ).toBeInTheDocument();
    expect(categoryCount('A')).toBe('1');
    expect(categoryCount('B')).toBe('0');
    expect(categoryCount('C')).toBe('0');
    expect(categoryCount('D')).toBe('0');
    expect(screen.queryByText(/Partial data/)).toBeNull();
  });

  it('renders an all-zero window as 0% of 0, never NaN', async () => {
    const g = golden();
    for (const k of Object.keys(g) as Array<keyof RepoHealth>) {
      if (typeof g[k] === 'number') (g as unknown as Record<string, number>)[k] = 0;
    }
    g.failure_categories = { A: 0, B: 0, C: 0, D: 0 };
    render(<HealthPanel state={await loadHealth(JSON.stringify(g))} />);
    expect(statValue('Plan first-shot approval')).toBe('0%');
    expect(screen.getByText('0 of 0 plans')).toBeInTheDocument();
    expect(statValue('Fixup rate')).toBe('0%');
    expect(statValue('Acceptance pass rate')).toBe('0%');
    expect(categoryCount('A')).toBe('0');
    expect(document.body.textContent).not.toMatch(/NaN/);
  });

  it('shows the partial-data note when the scan was truncated', async () => {
    const g = golden();
    g.truncated = true;
    render(<HealthPanel state={await loadHealth(JSON.stringify(g))} />);
    expect(screen.getByText(/Partial data/)).toBeInTheDocument();
    expect(statValue('Fixup rate')).toBe('50%');
  });

  it('renders a panel-scoped alert when the fetch fails', async () => {
    stubFetch(403, '{"error":"repo_forbidden","message":"repo acme/app is not visible"}');
    const error = await api.getRepoHealth('acme', 'app').catch((e: Error) => e);
    render(<HealthPanel state={{ status: 'error', error: error as Error }} />);
    expect(screen.getByRole('alert')).toHaveTextContent('repo acme/app is not visible');
    expect(screen.queryByText('Fixup rate')).toBeNull();
  });
});

describe('<Rate>', () => {
  it.each([
    [0, '0%'],
    [0.5, '50%'],
    [1 / 3, '33%'],
    [1, '100%'],
  ])('%d → %s', (rate, want) => {
    const { container } = render(<Rate value={rate} />);
    expect(container.textContent).toBe(want);
  });
});
