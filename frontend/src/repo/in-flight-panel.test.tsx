import { afterEach, describe, expect, it, vi } from 'vitest';
import { render, screen, within } from '@testing-library/react';
import { MemoryRouter } from 'react-router';
import type { Campaign, CampaignItem, CampaignStatus, Run, Stage } from '@/api/types';
import { RepoInFlight } from './in-flight-panel';

/*
 * The in-flight panel composes EXISTING surfaces (/v0/runs, /v0/runs/{id}/
 * stages, /v0/campaigns, /v0/campaigns/{id}/status), which carry no shared
 * wire golden, so the payloads below mirror those surfaces' snake_case
 * shapes. They go through the REAL api client via a stubbed fetch, so the
 * query string the panel sends (repo=acme%2Fapp) is asserted too.
 */

const T = '2026-09-28T12:00:00Z';

function run(id: string, state: Run['state'], trigger_ref: string | null = null): Run {
  return {
    id,
    repo: 'acme/app',
    workflow_id: 'feature_change',
    workflow_sha: 'abc123',
    trigger_source: 'github_issue',
    trigger_ref,
    state,
    retry_attempt: 0,
    max_retries_snapshot: 1,
    created_at: T,
    updated_at: T,
  };
}

function stage(
  run_id: string,
  sequence: number,
  type: Stage['type'],
  state: Stage['state'],
): Stage {
  return {
    id: `${run_id}-${type}`,
    run_id,
    sequence,
    type,
    executor: { kind: 'agent', ref: 'claude-code' },
    state,
    started_at: null,
    ended_at: null,
    failure_category: null,
    failure_reason: null,
    created_at: T,
    updated_at: T,
  };
}

function campaign(id: string, state: Campaign['state'], epic_ref: string): Campaign {
  return {
    id,
    repo: 'acme/app',
    epic_ref,
    state,
    pause_policy: 'pause_campaign',
    created_at: T,
    updated_at: T,
  };
}

function item(
  issue_ref: string,
  state: CampaignItem['state'],
  depends_on: string[] = [],
): CampaignItem {
  return { id: `item-${issue_ref}`, issue_ref, depends_on, state, created_at: T, updated_at: T };
}

const ACTIVE_CAMPAIGN = campaign('camp-1', 'running', 'issue:100');
const CAMPAIGN_STATUS: CampaignStatus = {
  campaign: ACTIVE_CAMPAIGN,
  items: [
    item('issue:1', 'succeeded'),
    item('issue:2', 'running', ['issue:1']),
    item('issue:3', 'blocked', ['issue:1', 'issue:2']),
  ],
  rollup: {
    eligible: [],
    blocked: ['issue:3'],
    running: ['issue:2'],
    done: ['issue:1'],
    failed: [],
    cancelled: [],
    paused: [],
  },
  next_action: { action: 'wait' },
};

interface Routes {
  runs?: Run[];
  runsCursor?: string | null;
  campaigns?: Campaign[];
  campaignsCursor?: string | null;
  stages?: Record<string, Stage[] | 'fail'>;
  status?: Record<string, CampaignStatus | 'fail'>;
  listFails?: boolean;
}

function json(body: unknown, status = 200): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: { 'Content-Type': 'application/json' },
  });
}

function stubRoutes(r: Routes) {
  const fetchMock = vi.fn(async (input: RequestInfo | URL) => {
    const url = new URL(String(input), 'http://localhost');
    const p = url.pathname;
    if (p === '/v0/runs') {
      if (r.listFails) return json({ error: 'internal', message: 'run list exploded' }, 500);
      return json({ items: r.runs ?? [], next_cursor: r.runsCursor ?? null });
    }
    if (p === '/v0/campaigns') {
      return json({ items: r.campaigns ?? [], next_cursor: r.campaignsCursor ?? null });
    }
    let m = p.match(/^\/v0\/runs\/([^/]+)\/stages$/);
    if (m) {
      const s = r.stages?.[m[1]] ?? [];
      return s === 'fail'
        ? json({ error: 'internal', message: 'stage read failed' }, 500)
        : json({ items: s });
    }
    m = p.match(/^\/v0\/campaigns\/([^/]+)\/status$/);
    if (m) {
      const s = r.status?.[m[1]];
      return s === undefined || s === 'fail'
        ? json({ error: 'internal', message: 'campaign status failed' }, 500)
        : json(s);
    }
    return json({ error: 'not_found', message: `unrouted ${p}` }, 404);
  });
  vi.stubGlobal('fetch', fetchMock);
  return fetchMock;
}

async function renderLoaded(r: Routes) {
  const fetchMock = stubRoutes(r);
  render(
    <MemoryRouter>
      <RepoInFlight repo="acme/app" />
    </MemoryRouter>,
  );
  expect(await screen.findByText(/Active runs|Couldn't load/)).toBeInTheDocument();
  return fetchMock;
}

function runRows(): HTMLElement[] {
  return within(screen.getByRole('list', { name: 'Active runs' })).getAllByRole('listitem');
}

afterEach(() => vi.unstubAllGlobals());

describe('<RepoInFlight>', () => {
  it('lists active runs with their current stage and active campaigns with wave progress', async () => {
    const fetchMock = await renderLoaded({
      runs: [run('r1', 'running', 'issue:42'), run('r2', 'pending'), run('r3', 'succeeded')],
      campaigns: [ACTIVE_CAMPAIGN, campaign('camp-done', 'succeeded', 'issue:200')],
      stages: {
        r1: [stage('r1', 2, 'implement', 'running'), stage('r1', 1, 'plan', 'succeeded')],
        r2: [],
      },
      status: { 'camp-1': CAMPAIGN_STATUS },
    });

    const urls = fetchMock.mock.calls.map((c) => String(c[0]));
    expect(urls).toContain('/v0/runs?limit=100&repo=acme%2Fapp');
    expect(urls).toContain('/v0/campaigns?limit=100&repo=acme%2Fapp');
    // The succeeded run and the succeeded campaign are never drilled into.
    expect(urls.some((u) => u.includes('/v0/runs/r3/'))).toBe(false);
    expect(urls.some((u) => u.includes('camp-done'))).toBe(false);

    expect(screen.getByRole('heading', { name: 'In flight' })).toBeInTheDocument();
    const runs = runRows();
    expect(runs).toHaveLength(2);
    expect(runs[0]).toHaveTextContent('issue:42');
    expect(runs[0]).toHaveTextContent('implement · running');
    expect(within(runs[0]).getByRole('link', { name: 'feature_change' })).toHaveAttribute(
      'href',
      '/runs/r1',
    );
    expect(runs[1]).toHaveTextContent('no stages yet');

    const camps = within(screen.getByRole('list', { name: 'Active campaigns' }));
    expect(camps.getByRole('link', { name: 'issue:100' })).toHaveAttribute(
      'href',
      '/campaigns/camp-1',
    );
    expect(camps.getByLabelText('wave progress')).toHaveTextContent('Wave 2 of 3');
    expect(camps.getByText(/1 of 3 items done/)).toBeInTheDocument();
    // The blocker list names only the UNRESOLVED dependency, not the done one.
    const blocked = within(screen.getByRole('list', { name: 'Blocked items' }));
    expect(blocked.getByRole('listitem')).toHaveTextContent('issue:3 blocked by issue:2');
    expect(screen.queryByText(/Partial data/)).toBeNull();
    expect(screen.queryByRole('alert')).toBeNull();
  });

  it('degrades a failed stage read to that row only — no panel alert', async () => {
    await renderLoaded({
      runs: [run('r1', 'running'), run('r2', 'running')],
      stages: { r1: [stage('r1', 1, 'plan', 'awaiting_approval')], r2: 'fail' },
    });
    const rows = runRows();
    expect(rows[0]).toHaveTextContent('plan · awaiting_approval');
    expect(rows[1]).toHaveTextContent('stage unavailable: stage read failed');
    expect(screen.queryByRole('alert')).toBeNull();
  });

  it('degrades a failed campaign status read to that campaign only', async () => {
    await renderLoaded({ campaigns: [ACTIVE_CAMPAIGN], status: { 'camp-1': 'fail' } });
    expect(screen.getByText('status unavailable: campaign status failed')).toBeInTheDocument();
    expect(screen.getByRole('link', { name: 'issue:100' })).toBeInTheDocument();
    expect(screen.queryByRole('alert')).toBeNull();
  });

  it('fails the panel with a scoped alert when a list read fails', async () => {
    await renderLoaded({ listFails: true });
    expect(screen.getByRole('alert')).toHaveTextContent('run list exploded');
    expect(screen.queryByText('Active runs')).toBeNull();
  });

  it('shows the partial-data note when the run list had a further page', async () => {
    await renderLoaded({ runs: [run('r1', 'running')], runsCursor: 'next' });
    expect(
      screen.getByText(/Partial data: only the newest 100 runs were scanned/),
    ).toBeInTheDocument();
  });

  it('names campaigns in the partial-data note when only the campaign list was cut', async () => {
    await renderLoaded({ campaignsCursor: 'next' });
    expect(
      screen.getByText(/Partial data: only the newest 100 campaigns were scanned/),
    ).toBeInTheDocument();
  });

  it('names both lists when both were cut', async () => {
    await renderLoaded({ runsCursor: 'next', campaignsCursor: 'next' });
    expect(
      screen.getByText(/Partial data: only the newest 100 runs and campaigns were scanned/),
    ).toBeInTheDocument();
  });

  it('renders explicit empty states when nothing is in flight', async () => {
    await renderLoaded({ runs: [run('r9', 'failed')], campaigns: [] });
    expect(screen.getByText('No active runs.')).toBeInTheDocument();
    expect(screen.getByText('No active campaigns.')).toBeInTheDocument();
  });

  it('says so when a blocked item records no dependency', async () => {
    const status: CampaignStatus = {
      ...CAMPAIGN_STATUS,
      items: [item('issue:9', 'blocked')],
      rollup: { ...CAMPAIGN_STATUS.rollup, blocked: ['issue:9'], running: [], done: [] },
    };
    await renderLoaded({ campaigns: [ACTIVE_CAMPAIGN], status: { 'camp-1': status } });
    expect(screen.getByText(/blocked by/)).toHaveTextContent(
      'issue:9 blocked by no recorded dependency',
    );
  });

  it('shows the last stage when every stage is terminal', async () => {
    await renderLoaded({
      runs: [run('r1', 'running')],
      stages: { r1: [stage('r1', 2, 'implement', 'failed'), stage('r1', 1, 'plan', 'succeeded')] },
    });
    expect(runRows()[0]).toHaveTextContent('implement · failed');
  });

  it('ignores a dependency naming no sibling item when computing waves', async () => {
    const status: CampaignStatus = {
      ...CAMPAIGN_STATUS,
      items: [item('issue:1', 'pending', ['issue:999'])],
    };
    await renderLoaded({ campaigns: [ACTIVE_CAMPAIGN], status: { 'camp-1': status } });
    expect(screen.getByLabelText('wave progress')).toHaveTextContent('Wave 1 of 1');
  });

  it('cuts a dependency cycle instead of recursing forever', async () => {
    const status: CampaignStatus = {
      ...CAMPAIGN_STATUS,
      items: [item('issue:1', 'pending', ['issue:2']), item('issue:2', 'pending', ['issue:1'])],
    };
    await renderLoaded({ campaigns: [ACTIVE_CAMPAIGN], status: { 'camp-1': status } });
    expect(screen.getByLabelText('wave progress')).toHaveTextContent('Wave 2 of 3');
  });
});
