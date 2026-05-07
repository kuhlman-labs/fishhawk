import { describe, expect, it, vi } from 'vitest';
import { render, screen } from '@testing-library/react';
import { MemoryRouter } from 'react-router';
import { ReviewDocument } from './review-document';
import type { PullRequestArtifactBody } from '@/api/pull-request';
import type { Stage } from '@/api/types';

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
    blocking_checks: ['ci_pass', 'fishhawk_audit_complete'],
    approvers: { any_of: ['founder'] },
  },
};

function renderDoc(stageOverride: Partial<Stage> = {}) {
  const stage = { ...baseStage, ...stageOverride };
  return render(
    <MemoryRouter>
      <ReviewDocument
        artifact={sampleArtifact}
        stage={stage}
        runId={stage.run_id}
        onStageUpdate={vi.fn()}
        onStageRollback={vi.fn()}
      />
    </MemoryRouter>,
  );
}

describe('<ReviewDocument>', () => {
  it('renders the PR title as the page heading and the review eyebrow', () => {
    renderDoc();
    expect(screen.getByRole('heading', { name: sampleArtifact.title })).toBeInTheDocument();
    expect(screen.getByText(/^Review · pull request$/i)).toBeInTheDocument();
  });

  it('renders the stage state badge', () => {
    renderDoc();
    expect(screen.getByText('awaiting_approval')).toBeInTheDocument();
  });

  it('renders the shared PullRequestSummary block (branch + GitHub link)', () => {
    renderDoc();
    expect(screen.getByText(sampleArtifact.branch)).toBeInTheDocument();
    const link = screen.getByRole('link', { name: /view on github/i });
    expect(link).toHaveAttribute('href', sampleArtifact.pr_url);
  });

  it("renders the gate's declared blocking_checks with the not-tracked-yet placeholder", () => {
    renderDoc();
    expect(screen.getByRole('heading', { name: /blocking checks/i })).toBeInTheDocument();
    expect(screen.getByText('ci_pass')).toBeInTheDocument();
    expect(screen.getByText('fishhawk_audit_complete')).toBeInTheDocument();
    // Two checks → two placeholder pills.
    expect(screen.getAllByText(/not tracked yet/i)).toHaveLength(2);
  });

  it('shows the ApprovalPanel buttons when state is awaiting_approval and gate is approval-typed', () => {
    renderDoc();
    expect(screen.getByRole('button', { name: /^approve$/i })).toBeEnabled();
    expect(screen.getByRole('button', { name: /^reject$/i })).toBeEnabled();
  });

  it('hides the ApprovalPanel for terminal states and shows the audit-log link instead', () => {
    renderDoc({ state: 'succeeded' });
    expect(screen.queryByRole('button', { name: /^approve$/i })).not.toBeInTheDocument();
    expect(screen.getByRole('link', { name: /view audit log/i })).toHaveAttribute(
      'href',
      `/runs/${baseStage.run_id}#audit`,
    );
  });

  it('suppresses the ApprovalPanel on check-only review gates (no human action)', () => {
    renderDoc({
      gate: {
        type: 'check',
        blocking_checks: ['ci_pass'],
      },
    });
    expect(screen.queryByRole('button', { name: /^approve$/i })).not.toBeInTheDocument();
    // Audit-log link still present so the reviewer can inspect chain.
    expect(screen.getByRole('link', { name: /view audit log/i })).toBeInTheDocument();
  });

  it('renders the approvers list with the any_of mode label for approval gates', () => {
    renderDoc();
    expect(screen.getByRole('heading', { name: /approvers/i })).toBeInTheDocument();
    expect(screen.getByText('any of')).toBeInTheDocument();
    expect(screen.getByText('founder')).toBeInTheDocument();
  });

  it('renders the all_of mode when the gate uses all_of', () => {
    renderDoc({
      gate: {
        type: 'approval',
        blocking_checks: ['ci_pass'],
        approvers: { all_of: ['founder', 'security-lead'] },
      },
    });
    expect(screen.getByText('all of')).toBeInTheDocument();
    expect(screen.getByText('founder')).toBeInTheDocument();
    expect(screen.getByText('security-lead')).toBeInTheDocument();
  });

  it('omits the Approvers section for check-only gates', () => {
    renderDoc({
      gate: {
        type: 'check',
        blocking_checks: ['ci_pass'],
      },
    });
    expect(screen.queryByRole('heading', { name: /approvers/i })).not.toBeInTheDocument();
  });

  it('renders a usable page even when the gate is missing on the wire (legacy / pre-#213 row)', () => {
    renderDoc({ gate: undefined });
    // PR summary still shows.
    expect(screen.getByText(sampleArtifact.branch)).toBeInTheDocument();
    // Blocking-checks panel renders with empty-gate fallback.
    expect(screen.getByText(/no blocking checks declared/i)).toBeInTheDocument();
    // No approvers section (no gate at all).
    expect(screen.queryByRole('heading', { name: /approvers/i })).not.toBeInTheDocument();
  });
});
