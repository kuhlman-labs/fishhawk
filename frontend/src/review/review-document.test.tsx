import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { render, screen, waitFor } from '@testing-library/react';
import { MemoryRouter } from 'react-router';
import { ReviewDocument } from './review-document';
import { api } from '@/api/client';
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

// flushChecksFetch waits for the listStageChecks mock to settle so
// the post-promise setState fires inside act(). Every test in this
// file fetches checks at mount; calling this at the end avoids the
// "update inside test was not wrapped in act" warning without
// changing what each test asserts.
async function flushChecksFetch(spy: ReturnType<typeof vi.spyOn>) {
  await waitFor(() => expect(spy).toHaveBeenCalled());
}

describe('<ReviewDocument>', () => {
  let checksSpy: ReturnType<typeof vi.spyOn>;

  // Default checks-stub: declared list reflects the run's branch-
  // protection snapshot (#251 / #254). Empty observed → the panel
  // renders all entries as `not_tracked`. Prevents the live-state
  // fetch from racing synchronous assertions.
  beforeEach(() => {
    checksSpy = vi.spyOn(api, 'listStageChecks').mockResolvedValue({
      declared: ['ci_pass', 'fishhawk_audit_complete'],
      items: [],
    });
  });
  afterEach(() => {
    vi.restoreAllMocks();
  });

  it('renders the PR title as the page heading and the review eyebrow', async () => {
    renderDoc();
    expect(screen.getByRole('heading', { name: sampleArtifact.title })).toBeInTheDocument();
    expect(screen.getByText(/^Review · pull request$/i)).toBeInTheDocument();
    // Wait for the listStageChecks promise to settle so React's
    // post-render state update doesn't fire outside act().
    await waitFor(() => {
      expect(screen.getAllByText(/not tracked yet/i).length).toBeGreaterThan(0);
    });
  });

  it('renders the stage state badge', async () => {
    renderDoc();
    expect(screen.getByText('awaiting_approval')).toBeInTheDocument();
    await waitFor(() => {
      expect(screen.getAllByText(/not tracked yet/i).length).toBeGreaterThan(0);
    });
  });

  it('renders the shared PullRequestSummary block (branch + GitHub link)', async () => {
    renderDoc();
    expect(screen.getByText(sampleArtifact.branch)).toBeInTheDocument();
    const link = screen.getByRole('link', { name: /view on github/i });
    expect(link).toHaveAttribute('href', sampleArtifact.pr_url);
    await flushChecksFetch(checksSpy);
  });

  it("renders the response's declared check names with the not-tracked-yet placeholder", async () => {
    renderDoc();
    expect(screen.getByRole('heading', { name: /blocking checks/i })).toBeInTheDocument();
    // The declared list now sources from the run's branch-protection
    // snapshot via the response (#251 / #254), not stage.gate.
    await waitFor(() => {
      expect(screen.getByText('ci_pass')).toBeInTheDocument();
    });
    expect(screen.getByText('fishhawk_audit_complete')).toBeInTheDocument();
    // Two declared, none observed → two placeholder pills.
    expect(screen.getAllByText(/not tracked yet/i)).toHaveLength(2);
  });

  it('shows the ApprovalPanel buttons when state is awaiting_approval and gate is approval-typed', async () => {
    renderDoc();
    expect(screen.getByRole('button', { name: /^approve$/i })).toBeEnabled();
    expect(screen.getByRole('button', { name: /^reject$/i })).toBeEnabled();
    await flushChecksFetch(checksSpy);
  });

  it('hides the ApprovalPanel for terminal states and shows the audit-log link instead', async () => {
    renderDoc({ state: 'succeeded' });
    expect(screen.queryByRole('button', { name: /^approve$/i })).not.toBeInTheDocument();
    expect(screen.getByRole('link', { name: /view audit log/i })).toHaveAttribute(
      'href',
      `/runs/${baseStage.run_id}#audit`,
    );
    await flushChecksFetch(checksSpy);
  });

  it('suppresses the ApprovalPanel on check-only review gates (no human action)', async () => {
    renderDoc({
      gate: {
        type: 'check',
      },
    });
    expect(screen.queryByRole('button', { name: /^approve$/i })).not.toBeInTheDocument();
    // Audit-log link still present so the reviewer can inspect chain.
    expect(screen.getByRole('link', { name: /view audit log/i })).toBeInTheDocument();
    await flushChecksFetch(checksSpy);
  });

  it('renders the approvers list with the any_of mode label for approval gates', async () => {
    renderDoc();
    expect(screen.getByRole('heading', { name: /approvers/i })).toBeInTheDocument();
    expect(screen.getByText('any of')).toBeInTheDocument();
    expect(screen.getByText('founder')).toBeInTheDocument();
    await flushChecksFetch(checksSpy);
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
    await flushChecksFetch(checksSpy);
  });

  it('omits the Approvers section for check-only gates', async () => {
    renderDoc({
      gate: {
        type: 'check',
      },
    });
    expect(screen.queryByRole('heading', { name: /approvers/i })).not.toBeInTheDocument();
    await flushChecksFetch(checksSpy);
  });

  it('renders a usable page even when the gate is missing on the wire (legacy / pre-#213 row)', async () => {
    // Empty declared list mimics a run that pre-dates branch
    // protection wiring (#251) or skipped the snapshot.
    checksSpy.mockResolvedValue({ declared: [], items: [] });
    renderDoc({ gate: undefined });
    // PR summary still shows.
    expect(screen.getByText(sampleArtifact.branch)).toBeInTheDocument();
    // Blocking-checks panel renders with empty-gate fallback.
    await waitFor(() => {
      expect(screen.getByText(/no blocking checks declared/i)).toBeInTheDocument();
    });
    // No approvers section (no gate at all).
    expect(screen.queryByRole('heading', { name: /approvers/i })).not.toBeInTheDocument();
  });
});

describe('<ReviewDocument> live blocking-check states (#228)', () => {
  let checksSpy: ReturnType<typeof vi.spyOn>;
  beforeEach(() => {
    checksSpy = vi.spyOn(api, 'listStageChecks');
  });
  afterEach(() => vi.restoreAllMocks());

  it('replaces the not_tracked placeholder with live state from the backend', async () => {
    checksSpy.mockResolvedValue({
      declared: ['ci_pass', 'fishhawk_audit_complete'],
      items: [
        { name: 'ci_pass', state: 'pass' },
        { name: 'fishhawk_audit_complete', state: 'pending' },
      ],
    });
    render(
      <MemoryRouter>
        <ReviewDocument
          artifact={sampleArtifact}
          stage={baseStage}
          runId={baseStage.run_id}
          onStageUpdate={vi.fn()}
          onStageRollback={vi.fn()}
        />
      </MemoryRouter>,
    );
    await waitFor(() => {
      expect(screen.getByText('pass')).toBeInTheDocument();
    });
    expect(screen.getByText('pending')).toBeInTheDocument();
    // None of the checks render the not_tracked placeholder when
    // observed states are present.
    expect(screen.queryAllByText(/not tracked yet/i)).toHaveLength(0);
  });

  it('falls back to not_tracked for declared checks the backend has not observed', async () => {
    checksSpy.mockResolvedValue({
      declared: ['ci_pass', 'fishhawk_audit_complete'],
      items: [
        { name: 'ci_pass', state: 'pass' },
        // fishhawk_audit_complete observed-but-not-yet — falls back.
      ],
    });
    render(
      <MemoryRouter>
        <ReviewDocument
          artifact={sampleArtifact}
          stage={baseStage}
          runId={baseStage.run_id}
          onStageUpdate={vi.fn()}
          onStageRollback={vi.fn()}
        />
      </MemoryRouter>,
    );
    await waitFor(() => {
      expect(screen.getByText('pass')).toBeInTheDocument();
    });
    expect(screen.getAllByText(/not tracked yet/i)).toHaveLength(1);
  });

  it('renders the empty-declared fallback when the backend errors (legacy / 503)', async () => {
    // Post-#254 the declared list lives on the response (sourced
    // from the run's branch-protection snapshot), so a 503 from
    // /v0/stages/{id}/checks leaves the panel with nothing to
    // render — the empty-declared "no blocking checks declared"
    // fallback fires instead of the not_tracked placeholders that
    // pre-v0.2 came from stage.gate.blocking_checks.
    checksSpy.mockRejectedValue(new Error('503 stage_checks_unconfigured'));
    render(
      <MemoryRouter>
        <ReviewDocument
          artifact={sampleArtifact}
          stage={baseStage}
          runId={baseStage.run_id}
          onStageUpdate={vi.fn()}
          onStageRollback={vi.fn()}
        />
      </MemoryRouter>,
    );
    await waitFor(() => {
      expect(screen.getByText(/no blocking checks declared/i)).toBeInTheDocument();
    });
  });
});
