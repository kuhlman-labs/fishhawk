import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { render, screen, waitFor } from '@testing-library/react';
import { MemoryRouter } from 'react-router';
import { ReviewDocument } from './review-document';
import { api } from '@/api/client';
import type { PullRequestArtifactBody } from '@/api/pull-request';
import type { AuditEntry, Stage } from '@/api/types';

const sampleArtifact: PullRequestArtifactBody = {
  pr_number: 209,
  pr_url: 'https://github.com/kuhlman-labs/fishhawk/pull/209',
  branch: 'fishhawk/run-aaa/stage-bbb',
  head_sha: '1234567890abcdef1234567890abcdef12345678',
  base_sha: 'abcdef1234567890abcdef1234567890abcdef12',
  title: 'Add make minio-init target',
  body: 'Closes #184',
  files_changed_count: 3,
};

const baseStage: Stage = {
  id: '00000000-0000-0000-0000-0000000000aa',
  run_id: '00000000-0000-0000-0000-0000000000ab',
  sequence: 2,
  type: 'review',
  executor: { kind: 'human', ref: 'human' },
  state: 'awaiting_approval',
  started_at: '2026-05-04T20:00:00Z',
  ended_at: null,
  failure_category: null,
  failure_reason: null,
  created_at: '2026-05-04T20:00:00Z',
  updated_at: '2026-05-04T20:05:00Z',
  gate: {
    type: 'approval',
    approvers: { any_of: ['founder'] },
  },
};

function auditEntry(overrides: Partial<AuditEntry>): AuditEntry {
  return {
    id: '00000000-0000-0000-0000-000000000000',
    sequence: 1,
    run_id: baseStage.run_id,
    stage_id: baseStage.id,
    ts: '2026-05-13T12:00:00Z',
    category: 'pr_merged',
    actor_kind: 'user',
    actor_subject: 'alice',
    payload: {},
    prev_hash: null,
    entry_hash: 'h',
    ...overrides,
  };
}

function renderDoc(stageOverride: Partial<Stage> = {}) {
  const stage = { ...baseStage, ...stageOverride };
  return render(
    <MemoryRouter>
      <ReviewDocument artifact={sampleArtifact} stage={stage} runId={stage.run_id} />
    </MemoryRouter>,
  );
}

// Wait for the listStageChecks fetch to settle so the post-promise
// setState fires inside act(). Saves us from "update not wrapped in
// act()" warnings without changing the surface each test asserts.
async function flushAsync() {
  await waitFor(() => {
    // Either the not-tracked placeholder shows or the audit panel
    // settles — either resolves the act() warning. We pick whichever
    // text the page should reach a stable state with.
    const ready =
      screen.queryByText(/not tracked yet/i) ||
      screen.queryByText(/no required checks configured/i) ||
      screen.queryByText(/no github activity yet/i) ||
      screen.queryByTestId('review-activity');
    expect(ready).toBeTruthy();
  });
}

describe('<ReviewDocument>', () => {
  beforeEach(() => {
    vi.spyOn(api, 'listStageChecks').mockResolvedValue({
      declared: ['ci_pass', 'fishhawk_audit_complete'],
      items: [],
    });
    vi.spyOn(api, 'listRunAudit').mockResolvedValue({
      items: [],
      next_cursor: null,
    });
  });
  afterEach(() => {
    vi.restoreAllMocks();
  });

  it('renders the PR title as the page heading and the review eyebrow', async () => {
    renderDoc();
    expect(screen.getByRole('heading', { name: sampleArtifact.title })).toBeInTheDocument();
    expect(screen.getByText(/^Review · pull request$/i)).toBeInTheDocument();
    await flushAsync();
  });

  it('renders the stage state badge', async () => {
    renderDoc();
    expect(screen.getByText('awaiting_approval')).toBeInTheDocument();
    await flushAsync();
  });

  it('renders a "View on GitHub" header affordance that links to the PR (ADR-018 / #314)', async () => {
    // Header replaces the old approve/reject control. Two "View on
    // GitHub" links land on the page — the header CTA and the
    // PullRequestSummary's own link — both pointing at pr_url.
    renderDoc();
    const links = screen.getAllByRole('link', { name: /view on github/i });
    expect(links.length).toBeGreaterThanOrEqual(1);
    for (const l of links) {
      expect(l).toHaveAttribute('href', sampleArtifact.pr_url);
    }
    await flushAsync();
  });

  it('does NOT render the in-Fishhawk Approve/Reject controls (ADR-018 / #313)', async () => {
    // Pre-#314 the review-stage page rendered ApprovalPanel buttons.
    // ADR-018 / #313 moved review-stage approval to GitHub; the
    // surface is gone.
    renderDoc();
    expect(screen.queryByRole('button', { name: /^approve$/i })).not.toBeInTheDocument();
    expect(screen.queryByRole('button', { name: /^reject$/i })).not.toBeInTheDocument();
    await flushAsync();
  });

  it('renders the same shape on terminal states (no more audit-log fallback link)', async () => {
    // Pre-#314 a terminal-state review stage swapped the approval
    // panel for an "audit log" link. Post-#314 the header is the
    // PR link in every state.
    renderDoc({ state: 'succeeded' });
    const links = screen.getAllByRole('link', { name: /view on github/i });
    expect(links.length).toBeGreaterThanOrEqual(1);
    expect(screen.queryByRole('link', { name: /view audit log/i })).not.toBeInTheDocument();
    await flushAsync();
  });

  it("renders the response's declared check names with the not-tracked-yet placeholder", async () => {
    renderDoc();
    expect(screen.getByRole('heading', { name: /required checks/i })).toBeInTheDocument();
    await waitFor(() => {
      expect(screen.getByText('ci_pass')).toBeInTheDocument();
    });
    expect(screen.getByText('fishhawk_audit_complete')).toBeInTheDocument();
    expect(screen.getAllByText(/not tracked yet/i)).toHaveLength(2);
  });

  it('renders the approvers list with the any_of mode label (informational; ADR-018)', async () => {
    renderDoc();
    expect(screen.getByRole('heading', { name: /approvers/i })).toBeInTheDocument();
    expect(screen.getByText('any of')).toBeInTheDocument();
    expect(screen.getByText('founder')).toBeInTheDocument();
    // New copy explains the informational nature (#314).
    expect(screen.getByText(/branch protection.*required-reviewers/i)).toBeInTheDocument();
    await flushAsync();
  });

  it('renders the all_of mode when the gate uses all_of', async () => {
    renderDoc({
      gate: {
        type: 'approval',
        approvers: { all_of: ['founder', 'security-lead'] },
      },
    });
    expect(screen.getByText('all of')).toBeInTheDocument();
    expect(screen.getByText('founder')).toBeInTheDocument();
    expect(screen.getByText('security-lead')).toBeInTheDocument();
    await flushAsync();
  });

  it('omits the Approvers section for check-only gates', async () => {
    renderDoc({
      gate: {
        type: 'check',
      },
    });
    expect(screen.queryByRole('heading', { name: /approvers/i })).not.toBeInTheDocument();
    await flushAsync();
  });

  it('renders a usable page even when the gate is missing on the wire (legacy / pre-#213 row)', async () => {
    (api.listStageChecks as ReturnType<typeof vi.fn>).mockResolvedValue({
      declared: [],
      items: [],
    });
    renderDoc({ gate: undefined });
    expect(screen.getByText(sampleArtifact.branch)).toBeInTheDocument();
    await waitFor(() => {
      expect(screen.getByText(/no required checks configured/i)).toBeInTheDocument();
    });
    expect(screen.queryByRole('heading', { name: /approvers/i })).not.toBeInTheDocument();
  });
});

describe('<ReviewDocument> activity panel (#314)', () => {
  beforeEach(() => {
    vi.spyOn(api, 'listStageChecks').mockResolvedValue({
      declared: [],
      items: [],
    });
  });
  afterEach(() => vi.restoreAllMocks());

  it('renders an empty-state when no PR activity yet', async () => {
    vi.spyOn(api, 'listRunAudit').mockResolvedValue({ items: [], next_cursor: null });
    renderDoc();
    await waitFor(() => {
      expect(screen.getByText(/no github activity yet/i)).toBeInTheDocument();
    });
  });

  it('renders pr_merged with the merger name', async () => {
    vi.spyOn(api, 'listRunAudit').mockResolvedValue({
      items: [
        auditEntry({
          id: 'a1',
          sequence: 1,
          category: 'pr_merged',
          actor_subject: 'alice',
          payload: { pr_url: sampleArtifact.pr_url, merger: 'alice' },
        }),
      ],
      next_cursor: null,
    });
    renderDoc();
    await waitFor(() => {
      const panel = screen.getByTestId('review-activity');
      expect(panel).toHaveTextContent('@alice');
      expect(panel).toHaveTextContent(/merged the PR/i);
    });
  });

  it('renders pr_approved_on_github with the reviewer name', async () => {
    vi.spyOn(api, 'listRunAudit').mockResolvedValue({
      items: [
        auditEntry({
          id: 'a2',
          sequence: 2,
          category: 'pr_approved_on_github',
          actor_subject: 'bob',
          payload: { state: 'approved' },
        }),
      ],
      next_cursor: null,
    });
    renderDoc();
    await waitFor(() => {
      const panel = screen.getByTestId('review-activity');
      expect(panel).toHaveTextContent('@bob');
      expect(panel).toHaveTextContent(/approved/i);
    });
  });

  it('renders pr_closed_without_merge with the closer name (#316)', async () => {
    vi.spyOn(api, 'listRunAudit').mockResolvedValue({
      items: [
        auditEntry({
          id: 'cl1',
          sequence: 5,
          category: 'pr_closed_without_merge',
          actor_subject: 'erin',
          payload: { closer: 'erin' },
        }),
      ],
      next_cursor: null,
    });
    renderDoc();
    await waitFor(() => {
      const panel = screen.getByTestId('review-activity');
      expect(panel).toHaveTextContent('@erin');
      expect(panel).toHaveTextContent(/closed without merging/i);
    });
  });

  it('picks the right verb for non-approve review states', async () => {
    vi.spyOn(api, 'listRunAudit').mockResolvedValue({
      items: [
        auditEntry({
          id: 'r1',
          sequence: 3,
          category: 'pr_review_submitted',
          actor_subject: 'carol',
          payload: { state: 'changes_requested' },
        }),
        auditEntry({
          id: 'r2',
          sequence: 4,
          category: 'pr_review_submitted',
          actor_subject: 'dave',
          payload: { state: 'commented' },
        }),
      ],
      next_cursor: null,
    });
    renderDoc();
    await waitFor(() => {
      const panel = screen.getByTestId('review-activity');
      expect(panel).toHaveTextContent(/@carol requested changes/i);
      expect(panel).toHaveTextContent(/@dave commented/i);
    });
  });

  it('orders the timeline oldest-first regardless of audit-log return order', async () => {
    // listRunAudit returns DESC by sequence; the panel reverses so
    // the reader sees a left-to-right chronology.
    vi.spyOn(api, 'listRunAudit').mockResolvedValue({
      items: [
        auditEntry({
          id: 'newest',
          sequence: 3,
          category: 'pr_merged',
          actor_subject: 'alice',
          payload: {},
        }),
        auditEntry({
          id: 'oldest',
          sequence: 1,
          category: 'pr_approved_on_github',
          actor_subject: 'bob',
          payload: {},
        }),
      ],
      next_cursor: null,
    });
    renderDoc();
    await waitFor(() => {
      expect(screen.getByTestId('review-activity')).toBeInTheDocument();
    });
    const items = screen.getByTestId('review-activity').querySelectorAll('li');
    expect(items.length).toBe(2);
    expect(items[0].textContent).toMatch(/@bob/);
    expect(items[1].textContent).toMatch(/@alice/);
  });

  it('filters out non-PR audit categories', async () => {
    vi.spyOn(api, 'listRunAudit').mockResolvedValue({
      items: [
        auditEntry({
          id: 'unrelated',
          category: 'run_dispatched',
          actor_subject: 'system',
          payload: {},
        }),
      ],
      next_cursor: null,
    });
    renderDoc();
    await waitFor(() => {
      expect(screen.getByText(/no github activity yet/i)).toBeInTheDocument();
    });
  });
});

describe('<ReviewDocument> live blocking-check states (#228)', () => {
  beforeEach(() => {
    vi.spyOn(api, 'listRunAudit').mockResolvedValue({ items: [], next_cursor: null });
  });
  afterEach(() => vi.restoreAllMocks());

  it('replaces the not_tracked placeholder with live state from the backend', async () => {
    vi.spyOn(api, 'listStageChecks').mockResolvedValue({
      declared: ['ci_pass', 'fishhawk_audit_complete'],
      items: [
        { name: 'ci_pass', state: 'pass' },
        { name: 'fishhawk_audit_complete', state: 'pending' },
      ],
    });
    render(
      <MemoryRouter>
        <ReviewDocument artifact={sampleArtifact} stage={baseStage} runId={baseStage.run_id} />
      </MemoryRouter>,
    );
    await waitFor(() => {
      expect(screen.getByText('pass')).toBeInTheDocument();
    });
    expect(screen.getByText('pending')).toBeInTheDocument();
    expect(screen.queryAllByText(/not tracked yet/i)).toHaveLength(0);
  });

  it('falls back to not_tracked for declared checks the backend has not observed', async () => {
    vi.spyOn(api, 'listStageChecks').mockResolvedValue({
      declared: ['ci_pass', 'fishhawk_audit_complete'],
      items: [{ name: 'ci_pass', state: 'pass' }],
    });
    render(
      <MemoryRouter>
        <ReviewDocument artifact={sampleArtifact} stage={baseStage} runId={baseStage.run_id} />
      </MemoryRouter>,
    );
    await waitFor(() => {
      expect(screen.getByText('pass')).toBeInTheDocument();
    });
    expect(screen.getAllByText(/not tracked yet/i)).toHaveLength(1);
  });

  it('renders the empty-declared fallback when the backend errors (legacy / 503)', async () => {
    vi.spyOn(api, 'listStageChecks').mockRejectedValue(new Error('503 stage_checks_unconfigured'));
    render(
      <MemoryRouter>
        <ReviewDocument artifact={sampleArtifact} stage={baseStage} runId={baseStage.run_id} />
      </MemoryRouter>,
    );
    await waitFor(() => {
      expect(screen.getByText(/no required checks configured/i)).toBeInTheDocument();
    });
  });
});
