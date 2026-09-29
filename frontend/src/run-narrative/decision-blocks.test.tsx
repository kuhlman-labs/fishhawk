import { describe, expect, it } from 'vitest';
import { render, screen, within } from '@testing-library/react';
import { MemoryRouter } from 'react-router';
import type { AuditEntry } from '@/api/types';
import { ApprovalsBlock, MergeBlock } from './decision-blocks';
import { deriveApprovals, deriveMerge } from './narrative';

let seq = 0;
function entry(category: string, payload: unknown): AuditEntry {
  seq++;
  return {
    id: `e${seq}`,
    sequence: seq,
    run_id: 'run-1',
    stage_id: 'st-plan',
    ts: '2026-09-28T00:00:00Z',
    category,
    actor_kind: 'user',
    actor_subject: 'github:octo',
    payload,
    prev_hash: null,
    entry_hash: `hash-${seq}`,
  };
}

function renderApprovals(payload: unknown) {
  return render(
    <MemoryRouter>
      <ApprovalsBlock
        section={{ model: deriveApprovals([entry('approval_submitted', payload)]), notes: [] }}
      />
    </MemoryRouter>,
  );
}

describe('<ApprovalsBlock>', () => {
  it('omits the Auth method and Channel rows entirely when those fields are absent', () => {
    renderApprovals({
      decision: 'approve',
      identity: { provider: 'github', subject: 'github:octo' },
    });
    const approval = screen.getByTestId('approval');
    expect(within(approval).getByText('github · github:octo')).toBeInTheDocument();
    expect(within(approval).queryByText('Auth method')).not.toBeInTheDocument();
    expect(within(approval).queryByText('Channel')).not.toBeInTheDocument();
  });

  it('renders the Auth method and Channel rows with their values when supplied', () => {
    renderApprovals({
      decision: 'approve',
      identity: { provider: 'github', subject: 'github:octo' },
      auth_method: 'oauth',
      channel: 'mcp',
      on_behalf_of: 'github:boss',
      comment: 'lgtm',
    });
    const approval = screen.getByTestId('approval');
    expect(within(approval).getByText('Auth method').nextElementSibling).toHaveTextContent('oauth');
    expect(within(approval).getByText('Channel').nextElementSibling).toHaveTextContent('mcp');
    expect(within(approval).getByText('github:boss')).toBeInTheDocument();
    expect(within(approval).getByText('lgtm')).toBeInTheDocument();
    expect(within(approval).getByTestId('evidence-link').getAttribute('href')).toMatch(
      /\?entry=\d+#entry-\d+$/,
    );
  });

  it('a legacy row with no identity says so rather than printing an empty value', () => {
    renderApprovals({ decision: 'reject' });
    expect(screen.getByText('Identity').nextElementSibling).toHaveTextContent('not recorded');
  });

  it('empty and degraded states', () => {
    const { unmount } = render(
      <MemoryRouter>
        <ApprovalsBlock section={{ model: [], notes: [] }} />
      </MemoryRouter>,
    );
    expect(screen.getByText('No approvals recorded yet.')).toBeInTheDocument();
    unmount();
    render(
      <MemoryRouter>
        <ApprovalsBlock
          section={{
            model: null,
            notes: [
              {
                section: 'approvals',
                kind: 'read_failed',
                read: 'a',
                message: 'approvals unavailable',
              },
            ],
          }}
        />
      </MemoryRouter>,
    );
    expect(screen.getByRole('note')).toHaveTextContent('approvals unavailable');
  });
});

function renderMerge(entries: AuditEntry[] | null) {
  return render(
    <MemoryRouter>
      <MergeBlock
        section={
          entries === null
            ? {
                model: null,
                notes: [
                  {
                    section: 'merge',
                    kind: 'read_failed',
                    read: 'm',
                    message: 'merge unavailable',
                  },
                ],
              }
            : { model: { merge: deriveMerge(entries) }, notes: [] }
        }
      />
    </MemoryRouter>,
  );
}

describe('<MergeBlock>', () => {
  it('no merge entry → explicit not-merged state', () => {
    renderMerge([]);
    expect(screen.getByTestId('merge-state')).toHaveAttribute('data-state', 'not_merged');
    expect(screen.getByTestId('merge-state')).toHaveTextContent('Not merged');
  });

  it('merged → commit SHA, PR link and evidence', () => {
    renderMerge([
      entry('merge_verdict_recorded', { verdict: 'merge' }),
      entry('pr_merged', { pr_url: 'https://github.com/o/r/pull/9', head_sha: 'cafef00d' }),
    ]);
    expect(screen.getByTestId('merge-state')).toHaveAttribute('data-state', 'merged');
    expect(screen.getByText('cafef00d')).toBeInTheDocument();
    expect(screen.getByRole('link', { name: 'https://github.com/o/r/pull/9' })).toBeInTheDocument();
    expect(screen.getByText('Merge verdict').nextElementSibling).toHaveTextContent('merge');
    expect(screen.getAllByTestId('evidence-link')).toHaveLength(2);
  });

  it('closed without merge, and a verdict with no observed merge', () => {
    const { unmount } = renderMerge([entry('pr_closed_without_merge', { pr_url: 'u' })]);
    expect(screen.getByTestId('merge-state')).toHaveAttribute('data-state', 'closed_without_merge');
    unmount();
    renderMerge([entry('merge_verdict_recorded', { verdict: 'merge' })]);
    expect(screen.getByTestId('merge-state')).toHaveAttribute('data-state', 'verdict_only');
  });

  it('read failure → the note, no state claim', () => {
    renderMerge(null);
    expect(screen.getByRole('note')).toHaveTextContent('merge unavailable');
    expect(screen.queryByTestId('merge-state')).not.toBeInTheDocument();
  });
});
