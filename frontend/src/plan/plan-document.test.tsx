import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { render, screen, waitFor, within } from '@testing-library/react';
import { MemoryRouter } from 'react-router';
import { PlanDocument } from './plan-document';
import type { StandardV1Plan } from '@/api/plan';
import type { AuditEntry, PaginatedList, Stage } from '@/api/types';
import { api } from '@/api/client';

const sampleStage: Stage = {
  id: '00000000-0000-0000-0000-0000000000aa',
  run_id: '00000000-0000-0000-0000-0000000000ab',
  sequence: 0,
  type: 'plan',
  executor: { kind: 'agent', ref: 'claude-code' },
  state: 'awaiting_approval',
  started_at: '2026-05-04T20:00:00Z',
  ended_at: null,
  failure_category: null,
  failure_reason: null,
  created_at: '2026-05-04T20:00:00Z',
  updated_at: '2026-05-04T20:00:00Z',
};

const samplePlan: StandardV1Plan = {
  plan_version: 'standard_v1',
  ticket_reference: {
    type: 'github_issue',
    url: 'https://github.com/kuhlman-labs/fishhawk/issues/56',
    id: 'kuhlman-labs/fishhawk#56',
  },
  generated_by: {
    agent: 'claude-code',
    model: 'claude-opus-4-7',
    timestamp: '2026-05-04T20:00:00Z',
  },
  summary: 'Render standard_v1 plans as documents.',
  scope: {
    files: [
      { path: 'frontend/src/plan/plan-document.tsx', operation: 'create' },
      { path: 'frontend/src/routes/runs.tsx', operation: 'modify' },
      { path: 'frontend/src/routes/__obsolete.tsx', operation: 'delete' },
    ],
    estimated_lines_changed: 240,
  },
  approach: [
    { step: 1, description: 'Define the API client and types.' },
    { step: 2, description: 'Build the section components.' },
  ],
  verification: {
    test_strategy: 'Vitest covers the renderer with a fixture plan.',
    rollback_plan: 'Revert PR; no data migrations.',
  },
  risks_and_assumptions: ['Assumes /v0/artifacts/{id} returns the plan as inline JSON.'],
};

let listRunAuditSpy: ReturnType<typeof vi.spyOn>;

function mockAudit(items: AuditEntry[]) {
  const page: PaginatedList<AuditEntry> = { items, next_cursor: null };
  listRunAuditSpy.mockResolvedValue(page);
}

function auditEntry(overrides: Partial<AuditEntry> = {}): AuditEntry {
  return {
    id: `audit-${Math.random()}`,
    sequence: 1,
    run_id: sampleStage.run_id,
    stage_id: sampleStage.id,
    ts: '2026-05-04T20:01:00Z',
    category: 'issue_commented',
    actor_kind: 'system',
    actor_subject: null,
    payload: {},
    prev_hash: null,
    entry_hash: 'h',
    ...overrides,
  };
}

function renderPlan(
  plan: StandardV1Plan = samplePlan,
  stage: Stage = sampleStage,
  audit: AuditEntry[] = [],
) {
  mockAudit(audit);
  return render(
    <MemoryRouter>
      <PlanDocument plan={plan} stage={stage} runId={stage.run_id} />
    </MemoryRouter>,
  );
}

beforeEach(() => {
  listRunAuditSpy = vi.spyOn(api, 'listRunAudit');
});

afterEach(() => {
  vi.restoreAllMocks();
});

describe('PlanDocument', () => {
  it('renders the page header and the plan_version badge', () => {
    renderPlan();
    expect(screen.getByRole('heading', { name: /^plan$/i })).toBeInTheDocument();
    expect(screen.getByText(/^Plan · standard_v1$/i)).toBeInTheDocument();
  });

  it('renders all required sections with anchor ids', () => {
    const { container } = renderPlan();
    for (const id of ['ticket', 'generated-by', 'summary', 'scope', 'approach', 'verification']) {
      expect(container.querySelector(`#${id}`)).not.toBeNull();
    }
  });

  it('renders the risks section when risks_and_assumptions is present', () => {
    const { container } = renderPlan();
    expect(container.querySelector('#risks')).not.toBeNull();
    expect(
      screen.getByText('Assumes /v0/artifacts/{id} returns the plan as inline JSON.'),
    ).toBeInTheDocument();
  });

  it('omits the risks section when risks_and_assumptions is empty / missing', () => {
    const { container } = renderPlan({ ...samplePlan, risks_and_assumptions: undefined });
    expect(container.querySelector('#risks')).toBeNull();
    expect(screen.queryByRole('heading', { name: /risks/i })).not.toBeInTheDocument();
  });

  it('renders the side-nav with anchors that match section ids (no audit → no activity entry)', async () => {
    renderPlan();
    // Wait for the audit fetch to settle so the activity-nav guard
    // (entries.length > 0 || loading) resolves to "no activity".
    await waitFor(() => expect(listRunAuditSpy).toHaveBeenCalled());
    await waitFor(() => {
      const hrefs = within(screen.getByRole('navigation', { name: /plan sections/i }))
        .getAllByRole('link')
        .map((a) => a.getAttribute('href'));
      expect(hrefs).toEqual([
        '#ticket',
        '#generated-by',
        '#summary',
        '#scope',
        '#approach',
        '#verification',
        '#risks',
      ]);
    });
  });

  it('renders scope files with their operation labels', () => {
    renderPlan();
    expect(screen.getByText('frontend/src/plan/plan-document.tsx')).toBeInTheDocument();
    expect(screen.getAllByText(/create/i).length).toBeGreaterThan(0);
    expect(screen.getAllByText(/modify/i).length).toBeGreaterThan(0);
    expect(screen.getAllByText(/delete/i).length).toBeGreaterThan(0);
  });

  it('renders approach steps in order with their numbers', () => {
    renderPlan();
    expect(screen.getByText('Define the API client and types.')).toBeInTheDocument();
    expect(screen.getByText('Build the section components.')).toBeInTheDocument();
  });

  it('exposes the ticket as a link to the ticket URL', () => {
    renderPlan();
    const link = screen.getByRole('link', { name: /kuhlman-labs\/fishhawk#56/ });
    expect(link).toHaveAttribute('href', 'https://github.com/kuhlman-labs/fishhawk/issues/56');
  });

  it('omits Approve / Reject buttons — the approval surface is GitHub now (ADR-020)', async () => {
    renderPlan();
    await waitFor(() => expect(listRunAuditSpy).toHaveBeenCalled());
    expect(screen.queryByRole('button', { name: /^approve$/i })).not.toBeInTheDocument();
    expect(screen.queryByRole('button', { name: /^reject$/i })).not.toBeInTheDocument();
    expect(screen.queryByRole('button', { name: /^regenerate$/i })).not.toBeInTheDocument();
  });

  it('renders the stage-state badge in the header', () => {
    renderPlan();
    // StageStateBadge renders the literal state token.
    expect(screen.getByText('awaiting_approval')).toBeInTheDocument();
  });

  it('exposes a "View audit log" link to the run-detail audit anchor', () => {
    renderPlan();
    const link = screen.getByRole('link', { name: /view audit log/i });
    expect(link).toHaveAttribute('href', `/runs/${sampleStage.run_id}#audit`);
  });

  it('does NOT render the "View on GitHub" link when no plan comment audit row exists', async () => {
    renderPlan(samplePlan, sampleStage, []);
    await waitFor(() => expect(listRunAuditSpy).toHaveBeenCalled());
    expect(screen.queryByRole('link', { name: /view on github/i })).not.toBeInTheDocument();
  });

  it('renders the "View on GitHub" link from the most-recent plan-comment audit row', async () => {
    const audit: AuditEntry[] = [
      auditEntry({
        id: 'a1',
        sequence: 1,
        category: 'issue_commented',
        payload: {
          kind: 'plan_full',
          repo: 'kuhlman-labs/fishhawk',
          issue_number: 56,
          github_comment_id: 4242,
        },
      }),
      auditEntry({
        id: 'a2',
        sequence: 2,
        category: 'issue_commented',
        payload: {
          kind: 'plan_updated',
          repo: 'kuhlman-labs/fishhawk',
          issue_number: 56,
          github_comment_id: 4242,
        },
      }),
    ];
    renderPlan(samplePlan, sampleStage, audit);
    const link = await screen.findByRole('link', { name: /view on github/i });
    expect(link).toHaveAttribute(
      'href',
      'https://github.com/kuhlman-labs/fishhawk/issues/56#issuecomment-4242',
    );
  });

  it('skips the View-on-GitHub link when the audit payload is malformed (defensive)', async () => {
    const audit: AuditEntry[] = [
      auditEntry({
        category: 'issue_commented',
        // Missing github_comment_id — defensive guard should treat
        // this as "no plan comment available".
        payload: { kind: 'plan_full', repo: 'x/y', issue_number: 1 },
      }),
    ];
    renderPlan(samplePlan, sampleStage, audit);
    await waitFor(() => expect(listRunAuditSpy).toHaveBeenCalled());
    expect(screen.queryByRole('link', { name: /view on github/i })).not.toBeInTheDocument();
  });

  it('renders a reply-comment approval row in the activity panel', async () => {
    const audit: AuditEntry[] = [
      auditEntry({
        id: 'a1',
        sequence: 1,
        category: 'issue_commented',
        payload: {
          kind: 'plan_full',
          repo: 'kuhlman-labs/fishhawk',
          issue_number: 56,
          github_comment_id: 4242,
        },
      }),
      auditEntry({
        id: 'a2',
        sequence: 2,
        category: 'approval_submitted',
        actor_subject: 'alice',
        ts: '2026-05-04T20:05:00Z',
        payload: {
          decision: 'approve',
          surface: 'github_reply_comment',
          approver: 'alice',
        },
      }),
    ];
    renderPlan(samplePlan, sampleStage, audit);
    const panel = await screen.findByTestId('plan-activity');
    expect(within(panel).getByText(/@alice/)).toBeInTheDocument();
    expect(within(panel).getByText(/approved/)).toBeInTheDocument();
    expect(within(panel).getByText(/reply comment/i)).toBeInTheDocument();
  });

  it('labels the approval source for slash, ui, cli surfaces', async () => {
    const audit: AuditEntry[] = [
      auditEntry({
        id: 'a-slash',
        sequence: 1,
        category: 'approval_submitted',
        actor_subject: 'alice',
        payload: { decision: 'approve', surface: 'github_comment', approver: 'alice' },
      }),
      auditEntry({
        id: 'a-ui',
        sequence: 2,
        category: 'approval_submitted',
        actor_subject: 'bob',
        payload: { decision: 'approve', surface: 'ui', approver: 'bob' },
      }),
      auditEntry({
        id: 'a-cli',
        sequence: 3,
        category: 'approval_submitted',
        actor_subject: 'carol',
        payload: { decision: 'approve', surface: 'cli', approver: 'carol' },
      }),
    ];
    renderPlan(samplePlan, sampleStage, audit);
    const panel = await screen.findByTestId('plan-activity');
    // Slash command surface renders the literal command name.
    expect(within(panel).getByText(/\/fishhawk approve/)).toBeInTheDocument();
    expect(within(panel).getByText(/dashboard/i)).toBeInTheDocument();
    expect(within(panel).getByText(/CLI/)).toBeInTheDocument();
  });

  it('renders a rejected approval with the rejected verb', async () => {
    const audit: AuditEntry[] = [
      auditEntry({
        category: 'approval_submitted',
        actor_subject: 'alice',
        payload: { decision: 'reject', surface: 'github_reply_comment', approver: 'alice' },
      }),
    ];
    renderPlan(samplePlan, sampleStage, audit);
    const panel = await screen.findByTestId('plan-activity');
    expect(within(panel).getByText(/rejected/)).toBeInTheDocument();
  });

  it('shows the activity panel only when there are entries (empty audit → no panel)', async () => {
    renderPlan(samplePlan, sampleStage, []);
    await waitFor(() => expect(listRunAuditSpy).toHaveBeenCalled());
    // No activity rows → no panel shell at all (avoids a "no
    // activity yet" placeholder cluttering the runtime when most
    // viewers will already see the GitHub side).
    expect(screen.queryByTestId('plan-activity')).not.toBeInTheDocument();
  });
});
