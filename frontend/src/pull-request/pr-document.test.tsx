import { describe, expect, it, vi } from 'vitest';
import { render, screen } from '@testing-library/react';
import { MemoryRouter } from 'react-router';
import { PullRequestDocument } from './pr-document';
import type { PullRequestArtifactBody } from '@/api/pull-request';
import type { Stage } from '@/api/types';

const sampleArtifact: PullRequestArtifactBody = {
  pr_number: 209,
  pr_url: 'https://github.com/kuhlman-labs/fishhawk/pull/209',
  branch: 'fishhawk/run-aaa/stage-bbb',
  head_sha: '1234567890abcdef1234567890abcdef12345678',
  base_sha: 'abcdef1234567890abcdef1234567890abcdef12',
  title: 'Add make minio-init target',
  body: 'This adds an idempotent target.\n\nCloses #184',
  files_changed_count: 3,
};

const succeededStage: Stage = {
  id: '00000000-0000-0000-0000-0000000000aa',
  run_id: '00000000-0000-0000-0000-0000000000ab',
  sequence: 1,
  type: 'implement',
  executor: { kind: 'agent', ref: 'claude-code' },
  state: 'succeeded',
  started_at: '2026-05-04T20:00:00Z',
  ended_at: '2026-05-04T20:05:00Z',
  failure_category: null,
  failure_reason: null,
  created_at: '2026-05-04T20:00:00Z',
  updated_at: '2026-05-04T20:05:00Z',
};

function renderDoc(
  artifact: PullRequestArtifactBody = sampleArtifact,
  stage: Stage = succeededStage,
) {
  return render(
    <MemoryRouter>
      <PullRequestDocument
        artifact={artifact}
        stage={stage}
        runId={stage.run_id}
        onStageUpdate={vi.fn()}
        onStageRollback={vi.fn()}
      />
    </MemoryRouter>,
  );
}

describe('PullRequestDocument', () => {
  it('renders the PR title as the page heading', () => {
    renderDoc();
    expect(screen.getByRole('heading', { name: 'Add make minio-init target' })).toBeInTheDocument();
    expect(screen.getByText(/^Implement · pull request$/i)).toBeInTheDocument();
  });

  it('renders the stage state badge', () => {
    renderDoc();
    expect(screen.getByText('succeeded')).toBeInTheDocument();
  });

  it('exposes a primary "View on GitHub" link to pr_url, opening in a new tab', () => {
    renderDoc();
    const link = screen.getByRole('link', { name: /view on github/i });
    expect(link).toHaveAttribute('href', sampleArtifact.pr_url);
    expect(link).toHaveAttribute('target', '_blank');
    expect(link).toHaveAttribute('rel', 'noopener noreferrer');
  });

  it('renders the branch and short head/base shas', () => {
    renderDoc();
    expect(screen.getByText(sampleArtifact.branch)).toBeInTheDocument();
    expect(screen.getByText('1234567')).toBeInTheDocument();
    expect(screen.getByText('abcdef1')).toBeInTheDocument();
  });

  it('shows the full sha as a tooltip on the truncated value', () => {
    renderDoc();
    expect(screen.getByText('1234567')).toHaveAttribute('title', sampleArtifact.head_sha);
    expect(screen.getByText('abcdef1')).toHaveAttribute('title', sampleArtifact.base_sha);
  });

  it('renders the files-changed count', () => {
    renderDoc();
    expect(screen.getByText('Files changed').nextElementSibling).toHaveTextContent('3');
  });

  it('renders the PR body as preformatted text when present', () => {
    renderDoc();
    expect(screen.getByRole('heading', { name: /description/i })).toBeInTheDocument();
    expect(screen.getByText(/Closes #184/)).toBeInTheDocument();
  });

  it('omits the body section when body is empty / missing', () => {
    renderDoc({ ...sampleArtifact, body: '' });
    expect(screen.queryByRole('heading', { name: /description/i })).not.toBeInTheDocument();
  });

  it('omits the body section when body is whitespace only', () => {
    renderDoc({ ...sampleArtifact, body: '   \n  ' });
    expect(screen.queryByRole('heading', { name: /description/i })).not.toBeInTheDocument();
  });

  it('shows the audit-log link when the stage is not awaiting approval', () => {
    renderDoc();
    const link = screen.getByRole('link', { name: /view audit log/i });
    expect(link).toHaveAttribute('href', `/runs/${succeededStage.run_id}#audit`);
  });

  it('shows ApprovalPanel buttons when state is awaiting_approval (forward-looking gate)', () => {
    renderDoc(sampleArtifact, { ...succeededStage, state: 'awaiting_approval' });
    expect(screen.getByRole('button', { name: /^approve$/i })).toBeEnabled();
    expect(screen.getByRole('button', { name: /^reject$/i })).toBeEnabled();
  });

  it('does not show approval buttons for a succeeded gateless implement stage', () => {
    renderDoc();
    expect(screen.queryByRole('button', { name: /^approve$/i })).not.toBeInTheDocument();
    expect(screen.queryByRole('button', { name: /^reject$/i })).not.toBeInTheDocument();
  });

  it('renders the small PR-number link in the header pointing at pr_url', () => {
    renderDoc();
    const link = screen.getByRole('link', { name: /^#209/ });
    expect(link).toHaveAttribute('href', sampleArtifact.pr_url);
    expect(link).toHaveAttribute('target', '_blank');
    expect(link).toHaveAttribute('rel', 'noopener noreferrer');
  });
});
