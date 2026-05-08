import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { render, screen, waitFor } from '@testing-library/react';
import { MemoryRouter } from 'react-router';
import { api } from '@/api/client';
import type { Run } from '@/api/types';
import { RelatedRunsSection, FollowUpLink } from './related-runs';

const RUN_A: Run = {
  id: 'aaaaaaaa-1111-1111-1111-111111111111',
  repo: 'kuhlman-labs/fishhawk',
  workflow_id: 'feature_change',
  workflow_sha: 'sha-a',
  trigger_source: 'github_issue',
  trigger_ref: 'issue:42',
  state: 'running',
  pull_request_url: 'https://github.com/kuhlman-labs/fishhawk/pull/123',
  created_at: '2026-05-08T12:00:00Z',
  updated_at: '2026-05-08T12:00:00Z',
};

const SIBLING_RUN: Run = {
  ...RUN_A,
  id: 'bbbbbbbb-2222-2222-2222-222222222222',
  state: 'succeeded',
  workflow_sha: 'sha-b',
};

function renderInRouter(node: React.ReactElement) {
  return render(<MemoryRouter>{node}</MemoryRouter>);
}

describe('<RelatedRunsSection>', () => {
  beforeEach(() => vi.restoreAllMocks());
  afterEach(() => vi.restoreAllMocks());

  it('groups by pull_request_url when set and lists sibling runs', async () => {
    const listSpy = vi.spyOn(api, 'listRuns').mockResolvedValue({
      items: [RUN_A, SIBLING_RUN],
      next_cursor: null,
    });

    renderInRouter(<RelatedRunsSection run={RUN_A} />);

    await waitFor(() => {
      expect(screen.getByText(/^Related runs$/)).toBeInTheDocument();
    });
    // Header label renders the PR slug, not the full URL.
    expect(screen.getByText('pull/123')).toBeInTheDocument();
    // Sibling row renders; current run is filtered out.
    expect(screen.getByText(SIBLING_RUN.id.slice(0, 8) + '…')).toBeInTheDocument();
    expect(screen.queryByText(RUN_A.id.slice(0, 8) + '…')).not.toBeInTheDocument();

    expect(listSpy).toHaveBeenCalledWith({
      pullRequestURL: RUN_A.pull_request_url,
      limit: 50,
    });
  });

  it('falls back to trigger_ref grouping when no PR URL is set', async () => {
    const triggerOnly: Run = { ...RUN_A, pull_request_url: null };
    const listSpy = vi.spyOn(api, 'listRuns').mockResolvedValue({
      items: [triggerOnly],
      next_cursor: null,
    });

    renderInRouter(<RelatedRunsSection run={triggerOnly} />);

    await waitFor(() => {
      expect(screen.getByText('issue:42')).toBeInTheDocument();
    });
    expect(listSpy).toHaveBeenCalledWith({
      repo: triggerOnly.repo,
      triggerRef: 'issue:42',
      limit: 50,
    });
  });

  it('shows empty-state when no other runs exist on the same key', async () => {
    vi.spyOn(api, 'listRuns').mockResolvedValue({
      items: [RUN_A], // only the current run
      next_cursor: null,
    });
    renderInRouter(<RelatedRunsSection run={RUN_A} />);
    await waitFor(() => {
      expect(screen.getByText(/no other runs share this PR/i)).toBeInTheDocument();
    });
  });

  it('renders nothing when the run has no PR and no trigger_ref', () => {
    const orphan: Run = { ...RUN_A, pull_request_url: null, trigger_ref: null };
    const listSpy = vi.spyOn(api, 'listRuns');
    const { container } = renderInRouter(<RelatedRunsSection run={orphan} />);
    expect(container.firstChild).toBeNull();
    expect(listSpy).not.toHaveBeenCalled();
  });

  it('shows the error block when the fetch fails', async () => {
    vi.spyOn(api, 'listRuns').mockRejectedValue(new Error('runs offline'));
    renderInRouter(<RelatedRunsSection run={RUN_A} />);
    await waitFor(() => {
      expect(screen.getByRole('alert')).toHaveTextContent(/runs offline/i);
    });
  });
});

describe('<FollowUpLink>', () => {
  it('renders a link to the parent run', () => {
    renderInRouter(<FollowUpLink parentRunID="cccccccc-3333-3333-3333-333333333333" />);
    const link = screen.getByRole('link', { name: /cccccccc/i });
    expect(link).toHaveAttribute('href', '/runs/cccccccc-3333-3333-3333-333333333333');
  });
});
