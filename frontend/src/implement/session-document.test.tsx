import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { fireEvent, render, screen, waitFor } from '@testing-library/react';
import { MemoryRouter } from 'react-router';
import { ImplementSessionDocument } from './session-document';
import { ApiClientError, api } from '@/api/client';
import type { PullRequestArtifactBody } from '@/api/pull-request';
import type { AuditEntry, Stage } from '@/api/types';

const samplePR: PullRequestArtifactBody = {
  pr_number: 209,
  pr_url: 'https://github.com/kuhlman-labs/fishhawk/pull/209',
  branch: 'fishhawk/run-x/stage-y',
  head_sha: '1234567890abcdef1234567890abcdef12345678',
  base_sha: 'abcdef1234567890abcdef1234567890abcdef12',
  title: 'Add make minio-init target',
  body: 'Closes #184',
  files_changed_count: 3,
};

const baseStage: Stage = {
  id: 'aaaaaaaa-1111-1111-1111-111111111111',
  run_id: 'bbbbbbbb-2222-2222-2222-222222222222',
  sequence: 1,
  type: 'implement',
  executor: { kind: 'agent', ref: 'claude-code' },
  state: 'succeeded',
  started_at: '2026-05-07T12:00:00Z',
  ended_at: '2026-05-07T12:05:00Z',
  failure_category: null,
  failure_reason: null,
  created_at: '2026-05-07T12:00:00Z',
  updated_at: '2026-05-07T12:05:00Z',
};

function makeEntry(over: Partial<AuditEntry> = {}): AuditEntry {
  return {
    id: 'cccccccc-3333-3333-3333-333333333333',
    sequence: 1,
    run_id: baseStage.run_id,
    stage_id: baseStage.id,
    ts: '2026-05-07T12:01:00Z',
    category: 'stage_dispatched',
    actor_kind: 'system',
    actor_subject: null,
    payload: {},
    prev_hash: null,
    entry_hash: 'deadbeef1234567890',
    ...over,
  };
}

function renderDoc(
  stageOver: Partial<Stage> = {},
  pullRequest: PullRequestArtifactBody | null = samplePR,
) {
  const stage = { ...baseStage, ...stageOver };
  return render(
    <MemoryRouter>
      <ImplementSessionDocument
        stage={stage}
        runId={stage.run_id}
        pullRequest={pullRequest}
        onStageUpdate={vi.fn()}
        onStageRollback={vi.fn()}
      />
    </MemoryRouter>,
  );
}

describe('<ImplementSessionDocument>', () => {
  beforeEach(() => {
    vi.restoreAllMocks();
    // Default trace-stream mock so the embedded <TranscriptSection>
    // (#218) doesn't fire a real network request during the
    // session-view tests that don't care about the transcript.
    // Returning 404 puts the section in its silent empty state.
    vi.spyOn(api, 'getStageTraceStream').mockRejectedValue(
      new ApiClientError(404, { error: 'trace_not_found' }, 'trace_not_found'),
    );
  });
  afterEach(() => vi.restoreAllMocks());

  it('renders the session header and stage badge — no PR title', async () => {
    vi.spyOn(api, 'getStagePromptRender').mockResolvedValue({
      stage_id: baseStage.id,
      stage_type: 'implement',
      prompt: 'You are implementing...',
      prompt_hash: 'a'.repeat(64),
    });
    vi.spyOn(api, 'listRunAudit').mockResolvedValue({ items: [], next_cursor: null });

    renderDoc();
    expect(screen.getByRole('heading', { name: 'Implement stage' })).toBeInTheDocument();
    expect(screen.getByText(/^Implement · session$/i)).toBeInTheDocument();
    expect(screen.getByText('succeeded')).toBeInTheDocument();
    // The PR title is no longer the page heading (was the case in the old PR-card view).
    expect(screen.queryByRole('heading', { name: samplePR.title })).not.toBeInTheDocument();
    // Wait for the async fetches inside Prompt / Activity / Transcript
    // sections to settle so the test doesn't race teardown (avoids
    // React's "update not wrapped in act" warnings).
    await waitFor(() => {
      expect(screen.getByText(/You are implementing/)).toBeInTheDocument();
    });
  });

  it('fetches and renders the constructed prompt with a hash badge', async () => {
    vi.spyOn(api, 'getStagePromptRender').mockResolvedValue({
      stage_id: baseStage.id,
      stage_type: 'implement',
      prompt: 'You are implementing a change in the repository ...',
      prompt_hash: 'abcdef0123456789' + 'f'.repeat(48),
    });
    vi.spyOn(api, 'listRunAudit').mockResolvedValue({ items: [], next_cursor: null });

    renderDoc();
    await waitFor(() => {
      expect(screen.getByText(/You are implementing a change/)).toBeInTheDocument();
    });
    // Hash is shown truncated.
    expect(screen.getByText(/sha256:abcdef012345/)).toBeInTheDocument();
  });

  it('hides the prompt when the user collapses the section', async () => {
    vi.spyOn(api, 'getStagePromptRender').mockResolvedValue({
      stage_id: baseStage.id,
      stage_type: 'implement',
      prompt: 'visible prompt body',
      prompt_hash: 'x'.repeat(64),
    });
    vi.spyOn(api, 'listRunAudit').mockResolvedValue({ items: [], next_cursor: null });

    renderDoc();
    await waitFor(() => expect(screen.getByText('visible prompt body')).toBeInTheDocument());

    const toggle = screen.getByRole('button', { name: /hide prompt/i });
    fireEvent.click(toggle);
    expect(screen.queryByText('visible prompt body')).not.toBeInTheDocument();
    expect(screen.getByRole('button', { name: /show prompt/i })).toBeInTheDocument();
  });

  it('does not fetch the prompt for a stage that has not dispatched yet', async () => {
    const promptSpy = vi.spyOn(api, 'getStagePromptRender').mockResolvedValue({
      stage_id: baseStage.id,
      stage_type: 'implement',
      prompt: '',
      prompt_hash: '',
    });
    vi.spyOn(api, 'listRunAudit').mockResolvedValue({ items: [], next_cursor: null });

    renderDoc({ state: 'pending' });
    await waitFor(() => {
      expect(
        screen.getByText(/prompt is constructed when the runner fetches it/i),
      ).toBeInTheDocument();
    });
    expect(promptSpy).not.toHaveBeenCalled();
  });

  it('renders stage-scoped audit entries with domain-aware labels', async () => {
    vi.spyOn(api, 'getStagePromptRender').mockResolvedValue({
      stage_id: baseStage.id,
      stage_type: 'implement',
      prompt: '',
      prompt_hash: 'x'.repeat(64),
    });
    vi.spyOn(api, 'listRunAudit').mockResolvedValue({
      items: [
        makeEntry({ id: 'e1', sequence: 1, category: 'stage_dispatched' }),
        makeEntry({
          id: 'e2',
          sequence: 2,
          category: 'policy_evaluated',
          payload: { passed: true, diff: [{ path: 'a.go' }, { path: 'b.go' }] },
        }),
        makeEntry({
          id: 'e3',
          sequence: 3,
          category: 'pull_request_opened',
          payload: { pr_number: 209, files_changed_count: 3 },
        }),
      ],
      next_cursor: null,
    });

    renderDoc();
    await waitFor(() => {
      expect(screen.getByText('Stage dispatched')).toBeInTheDocument();
      expect(screen.getByText('Policy evaluated')).toBeInTheDocument();
      expect(screen.getByText('2 files · pass')).toBeInTheDocument();
      expect(screen.getByText('Pull request opened · #209 · 3 files')).toBeInTheDocument();
    });
  });

  it('passes stage_id when fetching audit entries so sibling stages are excluded', async () => {
    const auditSpy = vi.spyOn(api, 'listRunAudit').mockResolvedValue({
      items: [],
      next_cursor: null,
    });
    vi.spyOn(api, 'getStagePromptRender').mockResolvedValue({
      stage_id: baseStage.id,
      stage_type: 'implement',
      prompt: '',
      prompt_hash: '',
    });
    renderDoc();
    await waitFor(() => expect(auditSpy).toHaveBeenCalled());
    expect(auditSpy.mock.calls[0]?.[1]?.stageId).toBe(baseStage.id);
  });

  it('shows an empty-state for the activity feed when no entries are recorded', async () => {
    vi.spyOn(api, 'getStagePromptRender').mockResolvedValue({
      stage_id: baseStage.id,
      stage_type: 'implement',
      prompt: '',
      prompt_hash: '',
    });
    vi.spyOn(api, 'listRunAudit').mockResolvedValue({ items: [], next_cursor: null });
    renderDoc();
    await waitFor(() => {
      expect(screen.getByText(/no events recorded for this stage yet/i)).toBeInTheDocument();
    });
  });

  it('surfaces a small "View PR on GitHub" affordance — not the full PR card', async () => {
    vi.spyOn(api, 'getStagePromptRender').mockResolvedValue({
      stage_id: baseStage.id,
      stage_type: 'implement',
      prompt: '',
      prompt_hash: '',
    });
    vi.spyOn(api, 'listRunAudit').mockResolvedValue({ items: [], next_cursor: null });
    renderDoc();
    await waitFor(() => {
      const link = screen.getByRole('link', { name: /view pr #209 on github/i });
      expect(link).toHaveAttribute('href', samplePR.pr_url);
      expect(link).toHaveAttribute('target', '_blank');
      expect(link).toHaveAttribute('rel', 'noopener noreferrer');
    });
    // Old PR card had a "View on GitHub" link with the dl block; we
    // suppressed that. Branch + sha shouldn't appear here anymore —
    // they live on the review page.
    expect(screen.queryByText(samplePR.branch)).not.toBeInTheDocument();
  });

  it('omits the PR row entirely when no PR has been opened yet', async () => {
    vi.spyOn(api, 'getStagePromptRender').mockResolvedValue({
      stage_id: baseStage.id,
      stage_type: 'implement',
      prompt: '',
      prompt_hash: '',
    });
    vi.spyOn(api, 'listRunAudit').mockResolvedValue({ items: [], next_cursor: null });
    renderDoc({ state: 'running' }, null);
    await waitFor(() => {
      expect(screen.getByText(/no events recorded for this stage yet/i)).toBeInTheDocument();
    });
    expect(screen.queryByRole('link', { name: /view pr/i })).not.toBeInTheDocument();
  });

  it('mounts the policy section with violations when a policy_evaluated entry exists', async () => {
    // The activity feed and the policy section both call
    // listRunAudit; branch the mock by category so the policy
    // section gets a real entry while the activity feed stays
    // empty.
    vi.spyOn(api, 'getStagePromptRender').mockResolvedValue({
      stage_id: baseStage.id,
      stage_type: 'implement',
      prompt: '',
      prompt_hash: '',
    });
    vi.spyOn(api, 'listRunAudit').mockImplementation(async (_runId, params) => {
      if (params?.category === 'policy_evaluated') {
        return {
          items: [
            makeEntry({
              category: 'policy_evaluated',
              payload: {
                passed: false,
                violations: [
                  {
                    constraint: 'forbidden_paths',
                    detail: 'pattern infra/** matched',
                    files: ['infra/main.tf'],
                  },
                ],
                diff: [{ path: 'infra/main.tf', status: 'M' }],
                applied_constraints: { forbidden_paths: ['infra/**'] },
              },
            }),
          ],
          next_cursor: null,
        };
      }
      return { items: [], next_cursor: null };
    });
    renderDoc();
    await waitFor(() => {
      expect(screen.getByText(/policy violations \(1\)/i)).toBeInTheDocument();
      expect(screen.getByText('infra/main.tf')).toBeInTheDocument();
    });
  });

  it('shows the policy-pending empty state when no policy_evaluated entry exists', async () => {
    vi.spyOn(api, 'getStagePromptRender').mockResolvedValue({
      stage_id: baseStage.id,
      stage_type: 'implement',
      prompt: '',
      prompt_hash: '',
    });
    vi.spyOn(api, 'listRunAudit').mockResolvedValue({ items: [], next_cursor: null });
    renderDoc();
    await waitFor(() => {
      expect(screen.getByText(/policy evaluation pending/i)).toBeInTheDocument();
    });
  });

  it('shows the audit-log link for terminal states; ApprovalPanel for awaiting_approval', async () => {
    vi.spyOn(api, 'getStagePromptRender').mockResolvedValue({
      stage_id: baseStage.id,
      stage_type: 'implement',
      prompt: '',
      prompt_hash: '',
    });
    vi.spyOn(api, 'listRunAudit').mockResolvedValue({ items: [], next_cursor: null });

    const { rerender } = renderDoc({ state: 'succeeded' });
    await waitFor(() => {
      expect(screen.getByRole('link', { name: /view audit log/i })).toBeInTheDocument();
    });

    rerender(
      <MemoryRouter>
        <ImplementSessionDocument
          stage={{ ...baseStage, state: 'awaiting_approval' }}
          runId={baseStage.run_id}
          pullRequest={samplePR}
          onStageUpdate={vi.fn()}
          onStageRollback={vi.fn()}
        />
      </MemoryRouter>,
    );
    await waitFor(() => {
      expect(screen.getByRole('button', { name: /^approve$/i })).toBeEnabled();
    });
  });
});
