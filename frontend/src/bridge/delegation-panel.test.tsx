import { afterEach, describe, expect, it, vi } from 'vitest';
import { fireEvent, render, screen, waitFor, within } from '@testing-library/react';
import type {
  DelegationConfirmationResponse,
  DelegationWorkflowStatus,
  RepoDelegation,
} from '@/api/captain';
import { DelegationPanel } from './delegation-panel';

/*
 * The delegation confirmation screen driven through the REAL api client over
 * a stubbed fetch. Every fixture is a literal `RepoDelegation` /
 * `DelegationConfirmationResponse` wire body.
 *
 * Counterfactual (2): the view-level confirmation-block guard. Its fixtures
 * serve a SUCCESSFUL verdict read, so with the guard deleted the badges would
 * render from those verdicts — only the guard keeps them off.
 * Counterfactual (3): the per-workflow hash binding. The two per-workflow
 * hashes differ from each other AND from the view-level hash.
 */

const VIEW_HASH = 'v'.repeat(64);
const FC_HASH = 'a'.repeat(64);
const BF_HASH = 'b'.repeat(64);

function view(over: Partial<RepoDelegation> = {}): RepoDelegation {
  return {
    repo: 'acme/app',
    source: 'ref',
    ref: 'main',
    workflow_sha: '0123456789abcdef0123',
    spec_version: '2',
    schema_major: 2,
    content_hash: VIEW_HASH,
    workflows: [
      {
        id: 'feature_change',
        autonomy: 'medium',
        matrix: [
          { action: 'merge', mode: 'gated', source: 'tier' },
          { action: 'review_resolve', mode: 'auto', min_severity: 'high', source: 'explicit' },
        ],
        must_page_human: ['acceptance_failed'],
        model_policy: { strategy: 'explicit_defaults', defaults: { plan: 'opus' } },
        escalations: [
          {
            match: { paths: ['db/migrations/**'] },
            max_autonomy: 'low',
            approvals: { count: 2, member_of: 'acme/dba', min_permission: 'maintain' },
            ceiling_matrix: [{ action: 'deploy', mode: 'report', source: 'escalation' }],
          },
        ],
        content_hash: FC_HASH,
      },
      {
        id: 'bug_fix',
        autonomy: 'high',
        matrix: [{ action: 'merge', mode: 'auto', source: 'tier' }],
        content_hash: BF_HASH,
      },
    ],
    confirmation: {
      captain: 'github:octo',
      seat_sequence: 7,
      unconfirmed_workflows: ['bug_fix'],
    },
    ...over,
  };
}

function verdicts(
  workflows: DelegationWorkflowStatus[] = [
    {
      workflow: 'feature_change',
      status: 'confirmed',
      current_content_hash: FC_HASH,
      confirmation: {
        subject: 'github:octo',
        content_hash: FC_HASH,
        sequence: 8,
        entry_hash: 'e'.repeat(64),
        at: '2026-09-28T12:00:00Z',
      },
    },
    {
      workflow: 'bug_fix',
      status: 'unconfirmed',
      reason: 'handover',
      current_content_hash: BF_HASH,
    },
  ],
): DelegationConfirmationResponse {
  return {
    repo: 'acme/app',
    source: 'ref',
    ref: 'main',
    captain: 'github:octo',
    seat_sequence: 7,
    workflows,
    unconfirmed_workflows: workflows.filter((w) => w.status !== 'confirmed').map((w) => w.workflow),
    skipped_entries: 0,
    ignored_entries: 0,
  };
}

interface Reply {
  status?: number;
  body: unknown;
}

function stub({
  viewReply = { body: view() },
  verdictReply = { body: verdicts() },
  confirmReply = { body: { repo: 'acme/app', workflow: {}, event: {} } },
}: { viewReply?: Reply; verdictReply?: Reply; confirmReply?: Reply } = {}) {
  const fetchMock = vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
    const p = new URL(String(input), 'http://localhost').pathname;
    const respond = (r: Reply) =>
      new Response(JSON.stringify(r.body), {
        status: r.status ?? 200,
        headers: { 'Content-Type': 'application/json' },
      });
    if (p === '/v0/repos/acme/app/delegation') return respond(viewReply);
    if (p === '/v0/repos/acme/app/delegation/confirmation') return respond(verdictReply);
    if (p === '/v0/repos/acme/app/delegation/confirm' && init?.method === 'POST') {
      return respond(confirmReply);
    }
    return respond({ status: 404, body: { error: 'not_found' } });
  });
  vi.stubGlobal('fetch', fetchMock);
  const calls = (path: string) =>
    fetchMock.mock.calls.filter(
      ([input]) => new URL(String(input), 'http://localhost').pathname === path,
    );
  return {
    viewGets: () => calls('/v0/repos/acme/app/delegation').length,
    verdictGets: () => calls('/v0/repos/acme/app/delegation/confirmation').length,
    confirmBodies: () =>
      calls('/v0/repos/acme/app/delegation/confirm').map(
        ([, init]) => JSON.parse(String((init as RequestInit).body)) as unknown,
      ),
  };
}

function card(id: string): HTMLElement {
  return screen.getByTestId(`delegation-workflow-${id}`);
}

function confirmButton(id: string): HTMLButtonElement {
  return within(card(id)).getByRole('button', { name: 'Confirm as shown' }) as HTMLButtonElement;
}

async function loaded() {
  await screen.findByTestId('delegation-workflow-feature_change');
}

afterEach(() => vi.unstubAllGlobals());

describe('<DelegationPanel> resolved view', () => {
  it('renders the matrix with provenance, paging, model policy, escalations and both contracts', async () => {
    const s = stub();
    render(<DelegationPanel owner="acme" name="app" />);
    await loaded();
    const fc = card('feature_change');
    expect(within(fc).getByText('review_resolve')).toBeInTheDocument();
    expect(within(fc).getByText('explicit')).toBeInTheDocument();
    expect(within(fc).getByText('≥ high')).toBeInTheDocument();
    expect(fc).toHaveTextContent('tier medium');
    expect(fc).toHaveTextContent('Must page a human: acceptance_failed');
    expect(fc).toHaveTextContent('Model policy: explicit_defaults · defaults plan=opus');
    expect(fc).toHaveTextContent('paths=db/migrations/**');
    expect(fc).toHaveTextContent('max autonomy low');
    expect(fc).toHaveTextContent('Approvals: 2 from acme/dba (≥ maintain)');
    expect(fc).toHaveTextContent('Ceiling matrix (declarative)');
    expect(screen.getByText(/WORKFLOW-level matrix/)).toHaveTextContent(
      "a gate's own autonomy block overrides it",
    );
    expect(screen.getByText(/WORKFLOW-level matrix/)).toHaveTextContent('DECLARATIVE');
    expect(screen.getByTestId('confirmation-state')).toHaveTextContent(
      'Captain in force: github:octo (seat #7) · unconfirmed: bug_fix',
    );
    // Reads source=ref by default: no source/ref query param is sent.
    expect(s.viewGets()).toBe(1);
    expect(s.verdictGets()).toBe(1);
  });

  it('renders a view-read failure as a named panel error with the server message', async () => {
    stub({
      viewReply: {
        status: 503,
        body: {
          error: 'github_unconfigured',
          message: 'no GitHub client; read source=run_cache instead',
        },
      },
    });
    render(<DelegationPanel owner="acme" name="app" />);
    const alert = await screen.findByRole('alert');
    expect(alert).toHaveTextContent('503 · github_unconfigured');
    expect(alert).toHaveTextContent('source=run_cache');
  });
});

describe('<DelegationPanel> distinct status badges', () => {
  it('confirmed / handover / hash_stale / no_captain each render their own wording', async () => {
    const v = view();
    v.workflows.push(
      { id: 'hotfix', autonomy: 'low', matrix: [], content_hash: 'h'.repeat(64) },
      { id: 'docs', autonomy: 'low', matrix: [], content_hash: 'd'.repeat(64) },
    );
    stub({
      viewReply: { body: v },
      verdictReply: {
        body: verdicts([
          {
            workflow: 'feature_change',
            status: 'confirmed',
            confirmation: {
              subject: 'github:octo',
              content_hash: FC_HASH,
              sequence: 8,
              entry_hash: 'e'.repeat(64),
              at: '2026-09-28T12:00:00Z',
            },
          },
          { workflow: 'bug_fix', status: 'unconfirmed', reason: 'handover' },
          { workflow: 'hotfix', status: 'unconfirmed', reason: 'hash_stale' },
          { workflow: 'docs', status: 'no_captain' },
        ]),
      },
    });
    render(<DelegationPanel owner="acme" name="app" />);
    await loaded();
    const badge = (id: string) => screen.getByTestId(`delegation-status-${id}`);
    expect(badge('feature_change')).toHaveTextContent(/^confirmed by github:octo \(#8\)$/);
    expect(badge('bug_fix')).toHaveTextContent(
      'unconfirmed — the seat changed hands since the last confirmation',
    );
    expect(badge('hotfix')).toHaveTextContent(
      'unconfirmed — the delegation changed since it was confirmed',
    );
    expect(badge('docs')).toHaveTextContent('no captain — nobody holds the seat to confirm');
    expect(badge('docs')).not.toHaveTextContent(/confirmed/);
    const texts = ['feature_change', 'bug_fix', 'hotfix', 'docs'].map(
      (id) => badge(id).textContent,
    );
    expect(new Set(texts).size).toBe(4);
  });

  it('a workflow the verdict read omits renders "no verdict", not a badge', async () => {
    stub({
      verdictReply: { body: verdicts([{ workflow: 'feature_change', status: 'confirmed' }]) },
    });
    render(<DelegationPanel owner="acme" name="app" />);
    await loaded();
    await screen.findByTestId('delegation-status-feature_change');
    expect(screen.queryByTestId('delegation-status-bug_fix')).toBeNull();
    expect(card('bug_fix')).toHaveTextContent('no verdict returned for this workflow');
  });
});

describe('<DelegationPanel> fail-closed confirmation reading (counterfactual 2)', () => {
  it('an ABSENT view-level confirmation block renders "could not be read" and NO badge', async () => {
    const v = view();
    delete v.confirmation;
    stub({ viewReply: { body: v } });
    render(<DelegationPanel owner="acme" name="app" />);
    await loaded();
    // The verdict read SUCCEEDS; let it settle so a missing guard would render.
    await new Promise((r) => setTimeout(r, 20));
    expect(screen.getByTestId('confirmation-state')).toHaveTextContent(
      /Confirmation state could not be read/,
    );
    expect(screen.queryAllByTestId(/^delegation-status-/)).toHaveLength(0);
    expect(screen.queryByText(/unconfirmed —/)).toBeNull();
  });

  it('a view-level confirmation.unavailable renders the reason and NO badge', async () => {
    stub({
      viewReply: {
        body: view({
          confirmation: {
            captain: null,
            seat_sequence: 0,
            unconfirmed_workflows: [],
            unavailable: 'chain_read_failed',
          },
        }),
      },
    });
    render(<DelegationPanel owner="acme" name="app" />);
    await loaded();
    await new Promise((r) => setTimeout(r, 20));
    expect(screen.getByTestId('confirmation-state')).toHaveTextContent(
      'Confirmation state could not be read (chain_read_failed)',
    );
    expect(screen.queryAllByTestId(/^delegation-status-/)).toHaveLength(0);
  });

  it('a failed verdict read renders a named alert and NO badge, the view still renders', async () => {
    stub({ verdictReply: { status: 500, body: { error: 'internal', message: 'boom' } } });
    render(<DelegationPanel owner="acme" name="app" />);
    await loaded();
    expect(await screen.findByRole('alert')).toHaveTextContent(
      'Confirmation verdicts could not be read: 500 · internal',
    );
    expect(screen.queryAllByTestId(/^delegation-status-/)).toHaveLength(0);
    expect(within(card('bug_fix')).getByText('merge')).toBeInTheDocument();
  });

  it('a 501 delegation_confirm_unconfigured names the reason and disables both verbs', async () => {
    const v = view();
    delete v.confirmation;
    stub({
      viewReply: { body: v },
      verdictReply: {
        status: 501,
        body: { error: 'delegation_confirm_unconfigured', message: 'no store' },
      },
    });
    render(<DelegationPanel owner="acme" name="app" />);
    await loaded();
    await screen.findByText(/Confirm and lower are unavailable/);
    expect(screen.getByTestId('confirmation-state')).toHaveTextContent(
      'No delegation-confirmation store on this instance',
    );
    expect(confirmButton('feature_change')).toBeDisabled();
    expect(confirmButton('bug_fix')).toBeDisabled();
    const lower = within(card('feature_change')).getByRole('button', {
      name: 'File the lower proposal',
      hidden: true,
    });
    expect(lower).toBeDisabled();
    expect(screen.queryAllByTestId(/^delegation-status-/)).toHaveLength(0);
  });
});

describe('<DelegationPanel> confirm', () => {
  it('binds the SECOND workflow its OWN content_hash, never the view-level one (counterfactual 3)', async () => {
    const s = stub();
    render(<DelegationPanel owner="acme" name="app" />);
    await loaded();
    fireEvent.click(confirmButton('bug_fix'));
    await waitFor(() => expect(s.confirmBodies()).toHaveLength(1));
    expect(s.confirmBodies()[0]).toEqual({ workflow: 'bug_fix', content_hash: BF_HASH });
    expect(BF_HASH).not.toBe(VIEW_HASH);
    expect(BF_HASH).not.toBe(FC_HASH);
    // A resolved confirm re-reads the verdicts (not the view).
    await waitFor(() => expect(s.verdictGets()).toBe(2));
    expect(s.viewGets()).toBe(1);
  });

  it('a 409 delegation_hash_stale renders current_content_hash and re-reads the view', async () => {
    const s = stub({
      confirmReply: {
        status: 409,
        body: {
          error: 'delegation_hash_stale',
          message: 'stale',
          details: { current_content_hash: 'f'.repeat(64) },
        },
      },
    });
    render(<DelegationPanel owner="acme" name="app" />);
    await loaded();
    fireEvent.click(confirmButton('feature_change'));
    await waitFor(() => expect(s.viewGets()).toBe(2));
    await loaded();
    const alert = await within(card('feature_change')).findByRole('alert');
    expect(alert).toHaveTextContent('(409 · delegation_hash_stale)');
    expect(alert).toHaveTextContent(`Current content hash: ${'f'.repeat(64)}.`);
  });

  it('a 501 from confirm disables both verbs with the reason named', async () => {
    stub({
      confirmReply: {
        status: 501,
        body: { error: 'delegation_confirm_unconfigured', message: 'no store' },
      },
    });
    render(<DelegationPanel owner="acme" name="app" />);
    await loaded();
    fireEvent.click(confirmButton('feature_change'));
    await screen.findByText(/Confirm and lower are unavailable/);
    expect(confirmButton('bug_fix')).toBeDisabled();
    expect(within(card('feature_change')).getByRole('alert')).toHaveTextContent(
      '(501 · delegation_confirm_unconfigured)',
    );
  });

  const refusals: Array<[number, string, RegExp]> = [
    [403, 'delegation_not_captain', /Only the captain in force.*\(403 · delegation_not_captain\)/],
    [409, 'delegation_no_captain', /The seat is vacant.*\(409 · delegation_no_captain\)/],
    [422, 'workflow_spec_invalid', /does not validate.*\(422 · workflow_spec_invalid\)/],
    [403, 'delegation_agent_identity_refused', /only a human can act here/],
    [418, 'brand_new_refusal', /^418 · brand_new_refusal$/],
  ];
  for (const [status, code, want] of refusals) {
    it(`renders ${status} ${code} with the raw server code`, async () => {
      stub({ confirmReply: { status, body: { error: code, message: 'refused' } } });
      render(<DelegationPanel owner="acme" name="app" />);
      await loaded();
      fireEvent.click(confirmButton('feature_change'));
      expect(await within(card('feature_change')).findByRole('alert')).toHaveTextContent(want);
    });
  }
});

describe('<DelegationPanel> generation', () => {
  it('a bumped generation re-runs both reads', async () => {
    const s = stub();
    const { rerender } = render(<DelegationPanel owner="acme" name="app" generation={0} />);
    await loaded();
    await waitFor(() => expect(s.verdictGets()).toBe(1));
    rerender(<DelegationPanel owner="acme" name="app" generation={1} />);
    await waitFor(() => expect(s.verdictGets()).toBe(2));
    expect(s.viewGets()).toBe(2);
  });
});
