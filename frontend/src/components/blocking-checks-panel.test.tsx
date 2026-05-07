import { describe, expect, it } from 'vitest';
import { render, screen } from '@testing-library/react';
import { BlockingChecksPanel, type BlockingCheck } from './blocking-checks-panel';

describe('<BlockingChecksPanel>', () => {
  it('renders one row per declared check with the check name', () => {
    const checks: BlockingCheck[] = [
      { name: 'ci_pass', state: 'not_tracked' },
      { name: 'fishhawk_audit_complete', state: 'not_tracked' },
    ];
    render(<BlockingChecksPanel checks={checks} />);
    expect(screen.getByText('ci_pass')).toBeInTheDocument();
    expect(screen.getByText('fishhawk_audit_complete')).toBeInTheDocument();
  });

  it('shows a "not tracked yet" pill with an explanatory tooltip for the placeholder state', () => {
    render(<BlockingChecksPanel checks={[{ name: 'ci_pass', state: 'not_tracked' }]} />);
    const pill = screen.getByText(/not tracked yet/i);
    expect(pill).toHaveAttribute(
      'title',
      'Backend ingestion of check states lands in a follow-up issue.',
    );
  });

  it('renders pass/fail/pending labels without the placeholder tooltip', () => {
    render(
      <BlockingChecksPanel
        checks={[
          { name: 'ci_pass', state: 'pass' },
          { name: 'fishhawk_audit_complete', state: 'fail' },
          { name: 'sbom_published', state: 'pending' },
        ]}
      />,
    );
    expect(screen.getByText('pass')).not.toHaveAttribute('title');
    expect(screen.getByText('fail')).not.toHaveAttribute('title');
    expect(screen.getByText('pending')).not.toHaveAttribute('title');
  });

  it('renders an empty-gate message instead of an empty list', () => {
    render(<BlockingChecksPanel checks={[]} />);
    expect(screen.getByText(/no blocking checks declared/i)).toBeInTheDocument();
  });
});
