import { readFileSync } from 'node:fs';
import { resolve } from 'node:path';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { render, screen, within } from '@testing-library/react';
import { api } from '@/api/client';
import type { AsyncState } from '@/api/use-async';
import type { RepoThroughput } from '@/api/types';
import { Duration, ThroughputPanel } from './throughput-panel';

/*
 * The payload is the SHARED wire golden the backend's repodash seam test
 * pins (testdata/wire/repodash_throughput.json), served from a stubbed
 * fetch through the REAL api client — so a json-tag rename on either side
 * of the wire reddens a test here (approval condition 3). Variants (zeroed,
 * truncated, wait_on_human removed) are DERIVED from the golden, never a
 * hand-authored second copy.
 */
const GOLDEN_BYTES = readFileSync(
  resolve(__dirname, '../../../testdata/wire/repodash_throughput.json'),
  'utf8',
);

function golden(): RepoThroughput {
  return JSON.parse(GOLDEN_BYTES) as RepoThroughput;
}

function stubFetch(status: number, body: string) {
  vi.stubGlobal(
    'fetch',
    vi.fn(
      async () => new Response(body, { status, headers: { 'Content-Type': 'application/json' } }),
    ),
  );
}

async function loadThroughput(body: string = GOLDEN_BYTES): Promise<AsyncState<RepoThroughput>> {
  stubFetch(200, body);
  return { status: 'ok', data: await api.getRepoThroughput('acme', 'app', { weeks: 4 }) };
}

function statValue(label: string): string | null {
  return screen.getByText(label).nextElementSibling?.textContent ?? null;
}

afterEach(() => vi.unstubAllGlobals());

describe('<ThroughputPanel>', () => {
  it('renders the golden: merged changes, median cycle time and one bar per week', async () => {
    render(<ThroughputPanel state={await loadThroughput()} />);
    expect(screen.getByRole('heading', { name: 'Throughput' })).toBeInTheDocument();
    expect(statValue('Merged changes')).toBe('1');
    expect(statValue('Median cycle time')).toBe('5h');
    expect(screen.getByText('1 sample')).toBeInTheDocument();
    const weeks = within(screen.getByRole('list', { name: 'Merged changes per week' }));
    expect(weeks.getAllByRole('listitem')).toHaveLength(4);
    expect(weeks.getByLabelText('Week of Sep 28: 1 merged')).toBeInTheDocument();
    expect(weeks.getByLabelText('Week of Sep 7: 0 merged')).toBeInTheDocument();
    expect(screen.queryByText(/Partial data/)).toBeNull();
  });

  it('renders the wait-on-human sub-panel when the key is present', async () => {
    render(<ThroughputPanel state={await loadThroughput()} />);
    expect(screen.getByRole('heading', { name: 'Wait on human' })).toBeInTheDocument();
    expect(statValue('Median total wait')).toBe('30m');
    expect(screen.getByText('over 1 run')).toBeInTheDocument();
    const row = screen.getByText('plan_approval').closest('tr')!;
    expect(within(row).getByText('30m')).toBeInTheDocument();
  });

  it('renders NOTHING for wait-on-human when the key is absent — no heading, no error', async () => {
    const g = golden();
    delete g.wait_on_human;
    const state = await loadThroughput(JSON.stringify(g));
    expect(state.status === 'ok' && 'wait_on_human' in state.data).toBe(false);
    render(<ThroughputPanel state={state} />);
    expect(screen.getByRole('heading', { name: 'Throughput' })).toBeInTheDocument();
    expect(screen.queryByRole('heading', { name: 'Wait on human' })).toBeNull();
    expect(screen.queryByText('Median total wait')).toBeNull();
    expect(screen.queryByRole('alert')).toBeNull();
  });

  it('renders an all-zero window without a bogus cycle time', async () => {
    const g = golden();
    delete g.wait_on_human;
    g.merged_changes = 0;
    g.median_cycle_time_seconds = 0;
    g.cycle_time_samples = 0;
    g.weeks = g.weeks.map((w) => ({ ...w, merged_changes: 0 }));
    render(<ThroughputPanel state={await loadThroughput(JSON.stringify(g))} />);
    expect(statValue('Merged changes')).toBe('0');
    expect(statValue('Median cycle time')).toBe('—');
    expect(screen.getByText('0 samples')).toBeInTheDocument();
    expect(screen.getByLabelText('Week of Sep 28: 0 merged')).toBeInTheDocument();
  });

  it('names the runs excluded from the median for lacking a pr_merged row', async () => {
    const g = golden();
    g.cycle_time_excluded = 2;
    render(<ThroughputPanel state={await loadThroughput(JSON.stringify(g))} />);
    expect(screen.getByText('1 sample, 2 excluded (no pr_merged row)')).toBeInTheDocument();
  });

  it('shows the partial-data note when the scan was truncated', async () => {
    const g = golden();
    g.truncated = true;
    render(<ThroughputPanel state={await loadThroughput(JSON.stringify(g))} />);
    expect(screen.getByText(/Partial data/)).toBeInTheDocument();
    expect(statValue('Merged changes')).toBe('1');
  });

  it('renders a panel-scoped alert when the fetch fails', async () => {
    stubFetch(503, '{"error":"run_repo_unavailable","message":"run repository not configured"}');
    const error = await api.getRepoThroughput('acme', 'app').catch((e: Error) => e);
    render(<ThroughputPanel state={{ status: 'error', error: error as Error }} />);
    expect(screen.getByRole('alert')).toHaveTextContent('run repository not configured');
    expect(screen.queryByText('Merged changes')).toBeNull();
  });

  it('renders a loading line while pending', () => {
    render(<ThroughputPanel state={{ status: 'loading' }} />);
    expect(screen.getByText('Loading throughput…')).toBeInTheDocument();
    expect(screen.queryByRole('alert')).toBeNull();
  });
});

describe('<Duration>', () => {
  it.each([
    [45, '45s'],
    [1800, '30m'],
    [18000, '5h'],
    [18720, '5h 12m'],
    [86400 * 2, '2d'],
    [86400 * 2 + 3 * 3600, '2d 3h'],
    [-5, '0s'],
  ])('%d → %s', (secs, want) => {
    const { container } = render(<Duration seconds={secs} />);
    expect(container.textContent).toBe(want);
  });
});
