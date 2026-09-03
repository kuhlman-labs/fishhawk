import { describe, expect, it } from 'vitest';
import { render, screen } from '@testing-library/react';
import { StageStateBadge } from './stage-state-badge';

/*
 * The badge map is config, not compile-enforced beyond Record-key
 * presence: a key with a blank/wrong class would still type-check.
 * These render assertions are the behavioral done-means for the two
 * deploy parked states (#1389) — each must render its literal wire
 * label so the run-detail list and stage-detail header show the real
 * deploy lifecycle state.
 */
describe('StageStateBadge deploy parked states', () => {
  it('renders the awaiting_deploy_approval label (pre-execution gate)', () => {
    render(<StageStateBadge state="awaiting_deploy_approval" />);
    expect(screen.getByText('awaiting_deploy_approval')).toBeInTheDocument();
  });

  it('renders the awaiting_deployment label (in-flight)', () => {
    render(<StageStateBadge state="awaiting_deployment" />);
    expect(screen.getByText('awaiting_deployment')).toBeInTheDocument();
  });
});

/*
 * The merge-supersede terminal state (#3083). `Record<StageState, string>`
 * compile-enforces that the key EXISTS, but not that it renders — a blank or
 * wrong class still type-checks. This asserts the literal wire label reaches
 * the DOM, which is what the run-detail list and stage-detail header show.
 */
describe('StageStateBadge merge-supersede state', () => {
  it('renders the superseded label', () => {
    render(<StageStateBadge state="superseded" />);
    expect(screen.getByText('superseded')).toBeInTheDocument();
  });
});
