import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { render, screen, waitFor, within } from '@testing-library/react';
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
    // The campaign-level operator_agent override (E25.12 / #1451) — opaque on
    // the shared Campaign type but present on the wire; the detail view reads it
    // off the payload and renders the Delegation override block.
    operator_agent: {
      may_approve: 'always',
      may_retry: 'infra_flake',
      must_page_human: ['reviewer_reject'],
    },
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
    replanning: [],
  },
  next_action: {
    action: 'resume',
    issue_ref: 'issue:1442',
    detail: 'the auto-driver paged a human at a run gate; handle the gate then POST /resume',
  },
};

/*
 * The per-item awaiting block (E88.4 / #4528) with the heartbeat-classified
 * host_dispatcher observation (#5489). Every host_dispatcher key below is one
 * of the json tags on hostDispatcherObservation in
 * backend/internal/server/campaign_item_host_dispatcher.go — the same list
 * TestAwaitingReasons_DocumentedAndGuided pins against the OpenAPI
 * CampaignItemAwaitingHostDispatcher properties. The next_step strings are
 * the server constants from hostDispatcherNextSteps.
 */
const RUN_P = '33333333-3333-3333-3333-333333333333';
const RUN_S = '44444444-4444-4444-4444-444444444444';
const RUN_C = '55555555-5555-5555-5555-555555555555';
const RUN_BB = '66666666-6666-6666-6666-666666666666';
const RUN_R = '77777777-7777-7777-7777-777777777777';
const RUN_T = '88888888-8888-8888-8888-888888888888';
const RUN_L = '99999999-9999-9999-9999-999999999999';
const RUN_N = 'aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa';
const STAGE = 'bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb';

const PAUSED_NEXT_STEP =
  'The host dispatcher named in host_dispatcher.host is paused and spawns nothing. Resume it on that host with `fishhawk-mcp host-dispatch --resume --state-dir <dir>`, or dispatch the stage by hand with fishhawk_dispatch_stage.';
const STALE_NEXT_STEP =
  "The host dispatcher's last heartbeat (host_dispatcher.last_heartbeat_at from host_dispatcher.host) is stale: it is stopped, hung or cannot reach the server. Check `fishhawk-mcp host-dispatch --status --state-dir <dir>` on that host and restart it, or dispatch the stage by hand with fishhawk_dispatch_stage.";
const CAPPED_NEXT_STEP =
  'The host dispatcher reached its daily spawn cap (host_dispatcher.daily_cap) and resumes at host_dispatcher.cap_resets_at. Wait for the reset, raise the cap on that host, or dispatch the stage by hand with fishhawk_dispatch_stage.';
const BUDGET_NEXT_STEP =
  'The host dispatcher refuses this run on the budget it was admitted under (host_dispatcher.budget_source), which stays fixed for this run. Wait for the budget period to roll over, or start a new run with budget_override. A hand fishhawk_dispatch_stage is not budget-gated.';
const REFUSING_NEXT_STEP =
  'The host dispatcher refuses to spawn this stage (host_dispatcher.refusal_code, or tick_failure_code for a failing tick). Fix the named cause on that host, read `fishhawk-mcp host-dispatch --status --state-dir <dir>` there, or dispatch the stage by hand with fishhawk_dispatch_stage.';

function runningItem(id: string, ref: string, runId: string, awaiting?: Record<string, unknown>) {
  return {
    id,
    issue_ref: ref,
    depends_on: [],
    run_id: runId,
    state: 'running',
    created_at: '2026-11-24T09:00:00Z',
    updated_at: '2026-11-24T09:30:00Z',
    ...(awaiting ? { awaiting } : {}),
  };
}

function hostDispatchAwaiting(runId: string, rest: Record<string, unknown>) {
  return {
    run_id: runId,
    stage_id: STAGE,
    stage_type: 'implement',
    stage_state: 'awaiting_host_dispatch',
    ...rest,
  };
}

const awaitingWire = {
  ...statusWire,
  items: [
    runningItem(
      'item-p',
      'issue:2001',
      RUN_P,
      hostDispatchAwaiting(RUN_P, {
        kind: 'human',
        reason: 'host_dispatch_paused',
        next_step: PAUSED_NEXT_STEP,
        host_dispatcher: {
          state: 'paused',
          host: 'dev-mac',
          // Fresh, so the line does not show it: only a stale heartbeat's time
          // is the operator's evidence.
          last_heartbeat_at: '2026-11-24T09:59:30Z',
        },
      }),
    ),
    runningItem(
      'item-s',
      'issue:2002',
      RUN_S,
      hostDispatchAwaiting(RUN_S, {
        kind: 'human',
        reason: 'host_dispatcher_absent',
        next_step: STALE_NEXT_STEP,
        host_dispatcher: {
          state: 'stale',
          host: 'old-host',
          last_heartbeat_at: '2026-11-24T09:50:00Z',
        },
      }),
    ),
    runningItem(
      'item-c',
      'issue:2003',
      RUN_C,
      hostDispatchAwaiting(RUN_C, {
        kind: 'human',
        reason: 'host_dispatch_capped',
        next_step: CAPPED_NEXT_STEP,
        host_dispatcher: {
          state: 'capped',
          host: 'dev-mac',
          last_heartbeat_at: '2026-11-24T09:59:30Z',
          daily_cap: 1,
          spawns_today: 1,
          cap_resets_at: '2026-11-25T00:00:00Z',
        },
      }),
    ),
    runningItem(
      'item-bb',
      'issue:2004',
      RUN_BB,
      hostDispatchAwaiting(RUN_BB, {
        kind: 'human',
        reason: 'host_dispatch_budget_blocked',
        next_step: BUDGET_NEXT_STEP,
        host_dispatcher: {
          state: 'held',
          host: 'dev-mac',
          last_heartbeat_at: '2026-11-24T09:59:30Z',
          refusal_code: 'budget_blocked',
          refusal_first_at: '2026-11-24T09:10:00Z',
          budget_source: 'admission_spec',
        },
      }),
    ),
    runningItem(
      'item-r',
      'issue:2005',
      RUN_R,
      hostDispatchAwaiting(RUN_R, {
        kind: 'human',
        reason: 'host_dispatch_refused',
        next_step: REFUSING_NEXT_STEP,
        host_dispatcher: {
          state: 'refusing',
          host: 'dev-mac',
          last_heartbeat_at: '2026-11-24T09:59:30Z',
          refusal_code: 'path_outside_allowed_roots',
          refusal_detail: 'working_dir /srv/x is outside the allowed roots',
          refusal_first_at: '2026-11-24T09:20:00Z',
        },
      }),
    ),
    runningItem(
      'item-t',
      'issue:2006',
      RUN_T,
      hostDispatchAwaiting(RUN_T, {
        kind: 'human',
        reason: 'host_dispatch_refused',
        // Server text is rendered as text: markup in it must stay literal.
        next_step: '<b>not bold</b>',
        host_dispatcher: {
          state: 'refusing',
          host: 'dev-mac',
          tick_failure_code: 'ledger_read_failed',
        },
      }),
    ),
    runningItem(
      'item-l',
      'issue:2007',
      RUN_L,
      hostDispatchAwaiting(RUN_L, {
        kind: 'host_dispatcher',
        reason: 'host_dispatch',
        next_step: 'The host dispatcher spawns this stage on its next tick.',
        host_dispatcher: {
          state: 'live',
          host: 'dev-mac',
          last_heartbeat_at: '2026-11-24T09:59:30Z',
        },
      }),
    ),
    runningItem('item-n', 'issue:2008', RUN_N),
  ],
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

  it('renders the campaign-level operator_agent override (governs every issue-run wholesale)', async () => {
    stubFetch(statusWire);
    renderDetail();
    await waitFor(() => {
      expect(screen.getByRole('heading', { name: /delegation override/i })).toBeInTheDocument();
    });
    // The set may_* knobs and their conditions render.
    expect(screen.getByText('may_approve')).toBeInTheDocument();
    expect(screen.getByText('always')).toBeInTheDocument();
    expect(screen.getByText('may_retry')).toBeInTheDocument();
    expect(screen.getByText('infra_flake')).toBeInTheDocument();
    // must_page_human is surfaced.
    expect(screen.getByText('must_page_human')).toBeInTheDocument();
    expect(screen.getByText('reviewer_reject')).toBeInTheDocument();
    // The wholesale-override semantics are stated for the operator.
    expect(screen.getByText(/replaces, never merges with/i)).toBeInTheDocument();
  });

  it('renders the override block with a "no knobs set" note when operator_agent is present but empty (every action pages)', async () => {
    const emptyOverride = {
      ...statusWire,
      campaign: { ...statusWire.campaign, operator_agent: {} },
    };
    stubFetch(emptyOverride);
    renderDetail();
    await waitFor(() => {
      expect(screen.getByRole('heading', { name: /delegation override/i })).toBeInTheDocument();
    });
    expect(screen.getByText(/no knobs set/i)).toBeInTheDocument();
  });

  it('omits the delegation override block when the campaign has no operator_agent (the inherit-the-workflow default)', async () => {
    const noOverride = {
      ...statusWire,
      campaign: { ...statusWire.campaign, operator_agent: undefined },
    };
    stubFetch(noOverride);
    renderDetail();
    // Wait for the page to render past loading (the rollup heading always shows).
    await waitFor(() => {
      expect(screen.getByRole('heading', { name: /rollup/i })).toBeInTheDocument();
    });
    expect(screen.queryByRole('heading', { name: /delegation override/i })).not.toBeInTheDocument();
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

  describe('awaiting column (E88.4 / #4528, host dispatcher #5489)', () => {
    // The awaiting cell of the run-grid row for one issue.
    async function awaitingCell(issueRef: string) {
      await waitFor(() => {
        expect(screen.getByRole('columnheader', { name: 'Awaiting' })).toBeInTheDocument();
      });
      const row = screen
        .getAllByRole('row')
        .find((r) => within(r).queryByText(issueRef, { selector: 'td' }));
      if (!row) throw new Error(`no run-grid row for ${issueRef}`);
      const cells = within(row).getAllByRole('cell');
      return cells[3];
    }

    it('renders a paused dispatcher: kind, reason, next_step and the state with its host', async () => {
      stubFetch(awaitingWire);
      renderDetail();
      const cell = await awaitingCell('issue:2001');
      expect(within(cell).getByText('human')).toBeInTheDocument();
      expect(within(cell).getByText('host_dispatch_paused')).toBeInTheDocument();
      expect(within(cell).getByText(PAUSED_NEXT_STEP)).toBeInTheDocument();
      const line = within(cell).getByTestId('host-dispatcher-state');
      expect(within(line).getByText('paused')).toBeInTheDocument();
      expect(within(line).getByText('dev-mac')).toBeInTheDocument();
      // A fresh heartbeat's time is not alarm evidence: only stale shows it.
      expect(within(line).queryByText(/last heartbeat/)).not.toBeInTheDocument();
      expect(within(line).queryByText('2026-11-24T09:59:30Z')).not.toBeInTheDocument();
    });

    it('renders a stale dispatcher with its last_heartbeat_at', async () => {
      stubFetch(awaitingWire);
      renderDetail();
      const cell = await awaitingCell('issue:2002');
      expect(within(cell).getByText('host_dispatcher_absent')).toBeInTheDocument();
      const line = within(cell).getByTestId('host-dispatcher-state');
      expect(within(line).getByText('stale')).toBeInTheDocument();
      expect(within(line).getByText('old-host')).toBeInTheDocument();
      expect(within(line).getByText('last heartbeat')).toBeInTheDocument();
      const at = within(line).getByText('2026-11-24T09:50:00Z');
      expect(at.tagName).toBe('TIME');
      expect(at).toHaveAttribute('datetime', '2026-11-24T09:50:00Z');
    });

    it('renders a capped dispatcher with cap_resets_at', async () => {
      stubFetch(awaitingWire);
      renderDetail();
      const cell = await awaitingCell('issue:2003');
      expect(within(cell).getByText('host_dispatch_capped')).toBeInTheDocument();
      const line = within(cell).getByTestId('host-dispatcher-state');
      expect(within(line).getByText('capped')).toBeInTheDocument();
      expect(within(line).getByText('resets')).toBeInTheDocument();
      expect(within(line).getByText('2026-11-25T00:00:00Z')).toBeInTheDocument();
    });

    it('renders a budget-blocked item with its budget_source and refusal code', async () => {
      stubFetch(awaitingWire);
      renderDetail();
      const cell = await awaitingCell('issue:2004');
      expect(within(cell).getByText('host_dispatch_budget_blocked')).toBeInTheDocument();
      expect(within(cell).getByText(BUDGET_NEXT_STEP)).toBeInTheDocument();
      const line = within(cell).getByTestId('host-dispatcher-state');
      expect(within(line).getByText('held')).toBeInTheDocument();
      expect(within(line).getByText('budget source')).toBeInTheDocument();
      expect(within(line).getByText('admission_spec')).toBeInTheDocument();
      expect(within(line).getByText('budget_blocked')).toBeInTheDocument();
    });

    it('renders a refusing dispatcher with its refusal_code', async () => {
      stubFetch(awaitingWire);
      renderDetail();
      const cell = await awaitingCell('issue:2005');
      const line = within(cell).getByTestId('host-dispatcher-state');
      expect(within(line).getByText('refusing')).toBeInTheDocument();
      expect(within(line).getByText('refusal')).toBeInTheDocument();
      expect(within(line).getByText('path_outside_allowed_roots')).toBeInTheDocument();
    });

    it('renders a failing tick with its tick_failure_code, and server text as text, never HTML', async () => {
      stubFetch(awaitingWire);
      renderDetail();
      const cell = await awaitingCell('issue:2006');
      const line = within(cell).getByTestId('host-dispatcher-state');
      expect(within(line).getByText('tick failure')).toBeInTheDocument();
      expect(within(line).getByText('ledger_read_failed')).toBeInTheDocument();
      expect(within(cell).getByText('<b>not bold</b>')).toBeInTheDocument();
      expect(cell.querySelector('b')).toBeNull();
    });

    it('renders a live host_dispatcher item with its kind and reason and no alarm line', async () => {
      stubFetch(awaitingWire);
      renderDetail();
      const cell = await awaitingCell('issue:2007');
      expect(within(cell).getByText('host_dispatcher')).toBeInTheDocument();
      expect(within(cell).getByText('host_dispatch')).toBeInTheDocument();
      expect(
        within(cell).getByText('The host dispatcher spawns this stage on its next tick.'),
      ).toBeInTheDocument();
      expect(within(cell).queryByTestId('host-dispatcher-state')).not.toBeInTheDocument();
      expect(within(cell).queryByText('dev-mac')).not.toBeInTheDocument();
    });

    it('renders an empty awaiting cell for an item with no awaiting block', async () => {
      stubFetch(awaitingWire);
      renderDetail();
      const cell = await awaitingCell('issue:2008');
      expect(cell).toBeEmptyDOMElement();
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
