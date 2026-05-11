import { describe, expect, it } from 'vitest';
import { render, screen } from '@testing-library/react';
import { RequiredChecksPanel, type RequiredCheck } from './required-checks-panel';

describe('<RequiredChecksPanel>', () => {
  it('renders one row per declared check with the check name', () => {
    const checks: RequiredCheck[] = [
      { name: 'ci_pass', state: 'not_tracked' },
      { name: 'fishhawk_audit_complete', state: 'not_tracked' },
    ];
    render(<RequiredChecksPanel checks={checks} />);
    expect(screen.getByText('ci_pass')).toBeInTheDocument();
    expect(screen.getByText('fishhawk_audit_complete')).toBeInTheDocument();
  });

  it('shows a "not tracked yet" pill with an explanatory tooltip for the placeholder state', () => {
    render(<RequiredChecksPanel checks={[{ name: 'ci_pass', state: 'not_tracked' }]} />);
    const pill = screen.getByText(/not tracked yet/i);
    expect(pill).toHaveAttribute(
      'title',
      'Backend ingestion of check states lands in a follow-up issue.',
    );
  });

  it('renders pass/fail/pending labels without the placeholder tooltip', () => {
    render(
      <RequiredChecksPanel
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
    render(<RequiredChecksPanel checks={[]} />);
    expect(screen.getByText(/no required checks configured/i)).toBeInTheDocument();
  });

  it('renders the branch-protection sub-label when sources include branch_protection (#256)', () => {
    render(
      <RequiredChecksPanel
        checks={[{ name: 'ci_pass', state: 'pass' }]}
        sources={['branch_protection']}
      />,
    );
    expect(screen.getByText(/required by branch protection$/i)).toBeInTheDocument();
  });

  it('credits both surfaces when classic protection + a ruleset contribute', () => {
    render(
      <RequiredChecksPanel
        checks={[{ name: 'ci_pass', state: 'pass' }]}
        sources={['branch_protection', 'ruleset:42']}
      />,
    );
    expect(screen.getByText(/required by branch protection \+ 1 ruleset/i)).toBeInTheDocument();
  });

  it('pluralizes the ruleset count when multiple contribute', () => {
    render(
      <RequiredChecksPanel
        checks={[{ name: 'ci_pass', state: 'pass' }]}
        sources={['ruleset:42', 'ruleset:99']}
      />,
    );
    expect(screen.getByText(/required by 2 rulesets/i)).toBeInTheDocument();
  });

  it('omits the sub-label when no sources are provided', () => {
    render(<RequiredChecksPanel checks={[{ name: 'ci_pass', state: 'pass' }]} />);
    expect(screen.queryByText(/required by/i)).not.toBeInTheDocument();
  });
});
