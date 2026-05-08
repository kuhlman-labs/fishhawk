import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { fireEvent, render, screen, waitFor } from '@testing-library/react';
import { api } from '@/api/client';
import type { AuditEntry } from '@/api/types';
import { PolicySection } from './policy-section';

const RUN_ID = '11111111-1111-1111-1111-111111111111';
const STAGE_ID = '22222222-2222-2222-2222-222222222222';

function policyEntry(payload: Record<string, unknown>): AuditEntry {
  return {
    id: '33333333-3333-3333-3333-333333333333',
    sequence: 1,
    run_id: RUN_ID,
    stage_id: STAGE_ID,
    ts: '2026-05-07T12:00:00Z',
    category: 'policy_evaluated',
    actor_kind: 'system',
    actor_subject: null,
    payload,
    prev_hash: null,
    entry_hash: 'deadbeef1234567890',
  };
}

describe('<PolicySection>', () => {
  beforeEach(() => vi.restoreAllMocks());
  afterEach(() => vi.restoreAllMocks());

  it('renders the empty state when no policy_evaluated entry exists', async () => {
    vi.spyOn(api, 'listRunAudit').mockResolvedValue({ items: [], next_cursor: null });
    render(<PolicySection runId={RUN_ID} stageId={STAGE_ID} />);
    await waitFor(() => {
      expect(screen.getByText(/policy evaluation pending/i)).toBeInTheDocument();
    });
  });

  it('renders the pass header + applied constraints when passed=true', async () => {
    vi.spyOn(api, 'listRunAudit').mockResolvedValue({
      items: [
        policyEntry({
          stage_type: 'implement',
          passed: true,
          violations: [],
          diff: [
            { path: 'a.go', status: 'A' },
            { path: 'b.go', status: 'M' },
            { path: 'c.go', status: 'D' },
          ],
          applied_constraints: {
            forbidden_paths: ['infra/**'],
            max_files_changed: 10,
            required_outcomes: ['tests_added_or_updated'],
          },
        }),
      ],
      next_cursor: null,
    });
    render(<PolicySection runId={RUN_ID} stageId={STAGE_ID} />);

    await waitFor(() => {
      expect(screen.getByText(/policy passed/i)).toBeInTheDocument();
    });
    expect(screen.getByText(/3 files: 1 added · 1 modified · 1 deleted/i)).toBeInTheDocument();
    // Pass-state defaults the constraints panel open.
    expect(screen.getByText('forbidden_paths:')).toBeInTheDocument();
    expect(screen.getByText('infra/**')).toBeInTheDocument();
    expect(screen.getByText('max_files_changed:')).toBeInTheDocument();
    expect(screen.getByText('10')).toBeInTheDocument();
    expect(screen.getByText('required_outcomes:')).toBeInTheDocument();
  });

  it('renders violations grouped by constraint when passed=false', async () => {
    vi.spyOn(api, 'listRunAudit').mockResolvedValue({
      items: [
        policyEntry({
          stage_type: 'implement',
          passed: false,
          violations: [
            {
              constraint: 'forbidden_paths',
              detail: 'pattern infra/** matched',
              files: ['infra/main.tf'],
            },
            {
              constraint: 'forbidden_paths',
              detail: 'pattern infra/** matched',
              files: ['infra/variables.tf'],
            },
            {
              constraint: 'required_outcomes',
              detail: 'no test files modified',
            },
          ],
          diff: [
            { path: 'infra/main.tf', status: 'M' },
            { path: 'infra/variables.tf', status: 'A' },
          ],
          applied_constraints: {
            forbidden_paths: ['infra/**'],
            required_outcomes: ['tests_added_or_updated'],
          },
        }),
      ],
      next_cursor: null,
    });

    render(<PolicySection runId={RUN_ID} stageId={STAGE_ID} />);
    await waitFor(() => {
      expect(screen.getByText(/policy violations \(3\)/i)).toBeInTheDocument();
    });
    // Both constraint groups appear as headings.
    expect(screen.getByText('forbidden_paths')).toBeInTheDocument();
    expect(screen.getByText('required_outcomes')).toBeInTheDocument();
    // Per-violation files are listed.
    expect(screen.getByText('infra/main.tf')).toBeInTheDocument();
    expect(screen.getByText('infra/variables.tf')).toBeInTheDocument();
    expect(screen.getAllByText('pattern infra/** matched')).toHaveLength(2);
    expect(screen.getByText('no test files modified')).toBeInTheDocument();
    // Fail-state defaults the constraints panel collapsed — the
    // toggle button is visible but the constraints list isn't.
    expect(screen.getByRole('button', { name: /Show applied constraints/i })).toBeInTheDocument();
    expect(screen.queryByText('forbidden_paths:')).not.toBeInTheDocument();
  });

  it('expands the collapsed constraints panel when the toggle is clicked', async () => {
    vi.spyOn(api, 'listRunAudit').mockResolvedValue({
      items: [
        policyEntry({
          passed: false,
          violations: [{ constraint: 'forbidden_paths', detail: 'matched' }],
          diff: [{ path: 'x.go', status: 'M' }],
          applied_constraints: { forbidden_paths: ['infra/**'] },
        }),
      ],
      next_cursor: null,
    });
    render(<PolicySection runId={RUN_ID} stageId={STAGE_ID} />);
    const button = await screen.findByRole('button', { name: /Show applied constraints/i });
    fireEvent.click(button);
    expect(screen.getByText('forbidden_paths:')).toBeInTheDocument();
  });

  it('shows the no-constraints fallback when the entry has none', async () => {
    vi.spyOn(api, 'listRunAudit').mockResolvedValue({
      items: [
        policyEntry({
          passed: true,
          violations: [],
          diff: [{ path: 'x.go', status: 'M' }],
          applied_constraints: {},
        }),
      ],
      next_cursor: null,
    });
    render(<PolicySection runId={RUN_ID} stageId={STAGE_ID} />);
    await waitFor(() => {
      expect(screen.getByText(/policy passed/i)).toBeInTheDocument();
    });
    expect(screen.getByText(/no constraints configured/i)).toBeInTheDocument();
  });

  it('renders the diff-summary fallback when diff is empty', async () => {
    vi.spyOn(api, 'listRunAudit').mockResolvedValue({
      items: [
        policyEntry({
          passed: true,
          violations: [],
          diff: [],
          applied_constraints: { forbidden_paths: ['infra/**'] },
        }),
      ],
      next_cursor: null,
    });
    render(<PolicySection runId={RUN_ID} stageId={STAGE_ID} />);
    await waitFor(() => {
      expect(screen.getByText(/empty diff/i)).toBeInTheDocument();
    });
  });

  it('shows the error block when the audit fetch errors', async () => {
    vi.spyOn(api, 'listRunAudit').mockRejectedValue(new Error('audit log offline'));
    render(<PolicySection runId={RUN_ID} stageId={STAGE_ID} />);
    await waitFor(() => {
      expect(screen.getByRole('alert')).toHaveTextContent(/audit log offline/i);
    });
  });
});
