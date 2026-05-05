import { describe, expect, it } from 'vitest';
import { render, screen } from '@testing-library/react';
import { FailureBanner } from './failure-banner';
import type { FailureCategory, Stage } from '@/api/types';
import { FAILURE_DESCRIPTIONS } from '@/api/types';

function stage(overrides: Partial<Stage> = {}): Stage {
  return {
    id: '00000000-0000-0000-0000-0000000000aa',
    run_id: '00000000-0000-0000-0000-0000000000ab',
    sequence: 0,
    type: 'plan',
    executor: { kind: 'agent', ref: 'claude-code' },
    state: 'awaiting_approval',
    started_at: null,
    ended_at: null,
    failure_category: null,
    failure_reason: null,
    created_at: '2026-05-04T20:00:00Z',
    updated_at: '2026-05-04T20:00:00Z',
    ...overrides,
  };
}

describe('FailureBanner', () => {
  it('renders nothing when stage is not failed', () => {
    const { container } = render(<FailureBanner stage={stage({ state: 'succeeded' })} />);
    expect(container.firstChild).toBeNull();
  });

  it('renders nothing when failure_category is missing', () => {
    const { container } = render(
      <FailureBanner stage={stage({ state: 'failed', failure_category: null })} />,
    );
    expect(container.firstChild).toBeNull();
  });

  it('renders category, description, and reason when failed', () => {
    render(
      <FailureBanner
        stage={stage({
          state: 'failed',
          failure_category: 'B',
          failure_reason: 'forbidden_paths violated: backend/internal/policy/secret.go',
        })}
      />,
    );
    const banner = screen.getByRole('alert');
    expect(banner).toHaveTextContent('category B');
    expect(banner).toHaveTextContent(FAILURE_DESCRIPTIONS.B);
    expect(banner).toHaveTextContent('forbidden_paths violated');
  });

  it('renders each category with its canonical description', () => {
    for (const cat of ['A', 'B', 'C', 'D'] satisfies FailureCategory[]) {
      const { unmount } = render(
        <FailureBanner stage={stage({ state: 'failed', failure_category: cat })} />,
      );
      expect(screen.getByRole('alert')).toHaveTextContent(FAILURE_DESCRIPTIONS[cat]);
      unmount();
    }
  });
});
