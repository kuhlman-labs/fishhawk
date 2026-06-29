import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { render, screen, waitFor } from '@testing-library/react';
import { MemoryRouter, Route, Routes } from 'react-router';
import { CampaignDetail } from './campaign-detail';

/*
 * Cross-boundary test (the type → client → render seam). Per the operator's
 * binding condition 2, this does NOT mock api.getCampaignStatus — it stubs
 * global fetch with the REAL wire JSON the backend emits and lets the typed
 * client (client.ts) deserialize it into the rendered detail view, so a
 * field-name drift between the TS types and the backend JSON tags fails the
 * render assertions rather than passing silently.
 *
 * The wire payload below is built VERBATIM from the GET
 * /v0/campaigns/{id}/status serialization in
 * backend/internal/server/campaigns.go (campaignResponse / campaignItemResponse
 * / campaignRollupPayload / campaignNextActionPayload) and campaign.PauseReason
 * — snake_case keys, run_id as a UUID string, depends_on as an array.
 */
const RUN_A = '11111111-1111-1111-1111-111111111111';
const RUN_B = '22222222-2222-2222-2222-222222222222';

const statusWire = {
  campaign: {
    id: 'cccccccc-1111-1111-1111-111111111111',
    repo: 'kuhlman-labs/fishhawk',
    epic_ref: 'issue:1439',
    state: 'running',
    pause_policy: 'pause_campaign',
    created_at: '2026-06-29T20:00:00Z',
    updated_at: '2026-06-29T20:05:00Z',
  },
  items: [
    {
      id: 'item-a',
      issue_ref: 'issue:1441',
      depends_on: [],
      run_id: RUN_A,
      state: 'succeeded',
      created_at: '2026-06-29T20:00:00Z',
      updated_at: '2026-06-29T20:04:00Z',
    },
    {
      id: 'item-b',
      issue_ref: 'issue:1442',
      depends_on: ['issue:1441'],
      run_id: RUN_B,
      state: 'paused',
      pause_reason: {
        page_event: 'campaign_gate_paged',
        run_id: RUN_B,
        gate: 'deploy_approval',
      },
      created_at: '2026-06-29T20:01:00Z',
      updated_at: '2026-06-29T20:05:00Z',
    },
    {
      id: 'item-c',
      issue_ref: 'issue:1443',
      depends_on: ['issue:1442'],
      state: 'blocked',
      created_at: '2026-06-29T20:01:00Z',
      updated_at: '2026-06-29T20:01:00Z',
    },
  ],
  rollup: {
    eligible: [],
    blocked: ['issue:1443'],
    running: [],
    done: ['issue:1441'],
    failed: [],
    cancelled: [],
    paused: ['issue:1442'],
  },
  next_action: {
    action: 'resume',
    issue_ref: 'issue:1442',
    detail: 'the auto-driver paged a human at a run gate; handle the gate then POST /resume',
  },
};

function stubFetch(body: unknown, status = 200) {
  const fetchMock = vi.fn(
    async () =>
      new Response(JSON.stringify(body), {
        status,
        headers: { 'Content-Type': 'application/json' },
      }),
  );
  vi.stubGlobal('fetch', fetchMock);
  return fetchMock;
}

function renderDetail(id = 'cccccccc-1111-1111-1111-111111111111') {
  return render(
    <MemoryRouter initialEntries={[`/campaigns/${id}`]}>
      <Routes>
        <Route path="campaigns/:campaignId" element={<CampaignDetail />} />
      </Routes>
    </MemoryRouter>,
  );
}

describe('<CampaignDetail> (cross-boundary: fetch → client → render)', () => {
  beforeEach(() => vi.unstubAllGlobals());
  afterEach(() => vi.unstubAllGlobals());

  it('hits GET /v0/campaigns/{id}/status through the real client', async () => {
    const fetchMock = stubFetch(statusWire);
    renderDetail();
    await waitFor(() => {
      const url = fetchMock.mock.calls.at(-1)?.[0] as string;
      expect(url).toMatch(/\/v0\/campaigns\/[^/]+\/status$/);
    });
  });

  it('renders the dependency DAG edges (depends_on) from the wire payload', async () => {
    stubFetch(statusWire);
    renderDetail();
    await waitFor(() => {
      expect(screen.getByText(/depends on issue:1441/i)).toBeInTheDocument();
      expect(screen.getByText(/depends on issue:1442/i)).toBeInTheDocument();
    });
  });

  it('renders the per-issue run grid: state badge + run link when run_id is set', async () => {
    stubFetch(statusWire);
    renderDetail();
    await waitFor(() => {
      expect(screen.getByText('succeeded')).toBeInTheDocument();
    });
    // run_id present → links through to the run detail (where the PR lives).
    const runLink = screen.getByRole('link', { name: RUN_A });
    expect(runLink).toHaveAttribute('href', `/runs/${RUN_A}`);
  });

  it('renders an explicit "no run yet" for an item with no run_id (not a bare dash)', async () => {
    stubFetch(statusWire);
    renderDetail();
    await waitFor(() => {
      expect(screen.getByText(/no run yet/i)).toBeInTheDocument();
    });
  });

  it('renders the rollup status partition counts', async () => {
    stubFetch(statusWire);
    renderDetail();
    await waitFor(() => {
      expect(screen.getByRole('heading', { name: /rollup/i })).toBeInTheDocument();
    });
    expect(screen.getByLabelText('done count')).toHaveTextContent('1');
    expect(screen.getByLabelText('paused count')).toHaveTextContent('1');
    expect(screen.getByLabelText('blocked count')).toHaveTextContent('1');
  });

  it('surfaces the paged item with its pending decision (next_action + pause_reason)', async () => {
    stubFetch(statusWire);
    renderDetail();
    await waitFor(() => {
      expect(screen.getByRole('heading', { name: /pending decision/i })).toBeInTheDocument();
    });
    // next_action action + issue_ref + detail.
    expect(screen.getByText('resume')).toBeInTheDocument();
    expect(screen.getByText(/handle the gate then POST \/resume/i)).toBeInTheDocument();
    // The paged issue's pause_reason gate + event are surfaced.
    expect(screen.getByText('deploy_approval')).toBeInTheDocument();
    expect(screen.getByText('campaign_gate_paged')).toBeInTheDocument();
    // issue_ref of the paged item appears (in the paged-issues list).
    expect(screen.getAllByText('issue:1442').length).toBeGreaterThan(0);
  });

  it('shows the loading state before the fetch resolves', () => {
    stubFetch(statusWire);
    renderDetail();
    expect(screen.getByText(/loading campaign…/i)).toBeInTheDocument();
  });

  it('renders the error box when the status fetch fails', async () => {
    stubFetch({ error: 'campaign_not_found', message: 'no campaign with that id' }, 404);
    renderDetail();
    await waitFor(() => {
      expect(screen.getByRole('alert')).toHaveTextContent(/no campaign with that id/i);
    });
  });

  it('renders the missing-id guard when no campaignId param is present', () => {
    render(
      <MemoryRouter initialEntries={['/campaigns']}>
        <Routes>
          <Route path="campaigns" element={<CampaignDetail />} />
        </Routes>
      </MemoryRouter>,
    );
    expect(screen.getByRole('alert')).toHaveTextContent(/missing campaign id/i);
  });
});
