import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { render, screen, waitFor } from '@testing-library/react';
import { MemoryRouter, Route, Routes } from 'react-router';
import { StageDetail } from './stage-detail';
import { api } from '@/api/client';
import type { Artifact, Stage } from '@/api/types';

/*
 * Route-level seam test (#1389 / the #618 cross-boundary gap gpt-5.5
 * flagged): drives a deploy stage through routes/stage-detail.tsx end
 * to end — the newest-deployment-artifact selector → api.getArtifact →
 * isDeploymentArtifact guard applied to the UNKNOWN wire content →
 * DeployDocument render, plus the unrecognized-shape warning fallback.
 * The component-level deploy-document.test.tsx + the guard unit test
 * cannot catch a broken route selector/loader or a missing deploy
 * branch here — only this route test does.
 */

const RUN_ID = '00000000-0000-0000-0000-0000000000ab';
const STAGE_ID = '00000000-0000-0000-0000-0000000000aa';

const deployStage: Stage = {
  id: STAGE_ID,
  run_id: RUN_ID,
  sequence: 3,
  type: 'deploy',
  executor: { kind: 'agent', ref: 'delegating-deploy' },
  state: 'succeeded',
  started_at: '2026-05-04T20:00:00Z',
  ended_at: '2026-05-04T20:10:00Z',
  failure_category: null,
  failure_reason: null,
  resolved_model: '',
  created_at: '2026-05-04T20:00:00Z',
  updated_at: '2026-05-04T20:10:00Z',
  gate: { type: 'approval', approvers: { any_of: ['founder'] } },
};

function deploymentArtifact(over: Partial<Artifact> = {}): Artifact {
  return {
    id: 'art-deploy-1',
    stage_id: STAGE_ID,
    kind: 'deployment',
    schema_version: null,
    content_hash: 'h1',
    created_at: '2026-05-04T20:05:00Z',
    ...over,
  };
}

function renderRoute() {
  return render(
    <MemoryRouter initialEntries={[`/runs/${RUN_ID}/stages/${STAGE_ID}`]}>
      <Routes>
        <Route path="runs/:runId/stages/:stageId" element={<StageDetail />} />
      </Routes>
    </MemoryRouter>,
  );
}

describe('<StageDetail> deploy route', () => {
  beforeEach(() => {
    vi.restoreAllMocks();
    vi.spyOn(api, 'getStage').mockResolvedValue(deployStage);
  });
  afterEach(() => vi.restoreAllMocks());

  it('selects the newest deployment artifact, fetches it, and renders DeployDocument', async () => {
    // Two deployment artifacts on the stage; the selector must pick
    // the most recent by created_at (a rollback record supersedes the
    // original deploy). Only that id should reach api.getArtifact.
    vi.spyOn(api, 'listStageArtifacts').mockResolvedValue({
      items: [
        deploymentArtifact({ id: 'old-deploy', created_at: '2026-05-04T20:01:00Z' }),
        deploymentArtifact({ id: 'new-deploy', created_at: '2026-05-04T20:09:00Z' }),
      ],
    });
    // getArtifact returns content typed `unknown` on the wire — the
    // route applies isDeploymentArtifact to it.
    const getArtifact = vi.spyOn(api, 'getArtifact').mockResolvedValue({
      id: 'new-deploy',
      stage_id: STAGE_ID,
      kind: 'deployment',
      schema_version: null,
      content_hash: 'h2',
      created_at: '2026-05-04T20:09:00Z',
      content: {
        environment: 'production',
        ref: '1234567890abcdef1234567890abcdef12345678',
        external_run_url: 'https://github.com/kuhlman-labs/fishhawk/actions/runs/42',
        outcome: 'succeeded',
      },
    } as Artifact);

    renderRoute();

    await waitFor(() => {
      expect(screen.getByRole('heading', { name: /deploy stage/i })).toBeInTheDocument();
    });
    // Newest-by-created_at selector fed the loader the right id — and
    // ONLY that id. A selector that picked the first row, or failed to
    // sort descending, would fetch 'old-deploy' instead; assert the
    // stale artifact is excluded, not merely that the newest is included.
    expect(getArtifact).toHaveBeenCalledWith('new-deploy');
    expect(getArtifact).not.toHaveBeenCalledWith('old-deploy');
    expect(getArtifact).toHaveBeenCalledTimes(1);
    // Guard passed → DeployDocument rendered the artifact fields.
    expect(screen.getByText('production')).toBeInTheDocument();
    expect(screen.getByText('1234567890abcdef1234567890abcdef12345678')).toBeInTheDocument();
    expect(screen.getByTestId('deploy-outcome')).toHaveTextContent('succeeded');
    expect(screen.getByRole('link', { name: /view external run/i })).toHaveAttribute(
      'href',
      'https://github.com/kuhlman-labs/fishhawk/actions/runs/42',
    );
    // No unrecognized-shape warning on a well-formed body.
    expect(screen.queryByText(/unrecognized deployment artifact shape/i)).not.toBeInTheDocument();
  });

  it('surfaces the unrecognized-shape warning when the artifact fails the guard, still rendering the page', async () => {
    vi.spyOn(api, 'listStageArtifacts').mockResolvedValue({
      items: [deploymentArtifact({ id: 'bad-deploy' })],
    });
    vi.spyOn(api, 'getArtifact').mockResolvedValue({
      id: 'bad-deploy',
      stage_id: STAGE_ID,
      kind: 'deployment',
      schema_version: null,
      content_hash: 'h3',
      created_at: '2026-05-04T20:05:00Z',
      // Missing the required external_run_url + outcome → guard fails.
      content: { environment: 'production', ref: 'abc' },
    } as Artifact);

    renderRoute();

    await waitFor(() => {
      expect(screen.getByText(/unrecognized deployment artifact shape/i)).toBeInTheDocument();
    });
    // The page still renders (degrade-not-block): DeployDocument shows
    // with no artifact, so the "no deployment artifact yet" note shows.
    expect(screen.getByRole('heading', { name: /deploy stage/i })).toBeInTheDocument();
    expect(screen.getByText(/no deployment artifact yet/i)).toBeInTheDocument();
  });

  it('renders the deploy page with the gate when no deployment artifact exists yet', async () => {
    vi.spyOn(api, 'getStage').mockResolvedValue({
      ...deployStage,
      state: 'awaiting_deploy_approval',
      ended_at: null,
    });
    vi.spyOn(api, 'listStageArtifacts').mockResolvedValue({ items: [] });
    const getArtifact = vi.spyOn(api, 'getArtifact');

    renderRoute();

    await waitFor(() => {
      expect(screen.getByRole('heading', { name: /deploy stage/i })).toBeInTheDocument();
    });
    // No deployment artifact → loader short-circuits, getArtifact unused.
    expect(getArtifact).not.toHaveBeenCalled();
    expect(screen.getByText(/no deployment artifact yet/i)).toBeInTheDocument();
    // The pre-execution gate is shown so the operator can act.
    expect(screen.getByText('awaiting_deploy_approval')).toBeInTheDocument();
    expect(screen.getByRole('button', { name: /^approve$/i })).toBeInTheDocument();
  });
});

/*
 * Per-stage resolved_model observability (#1416, slice d). The Stage API
 * surfaces the model the gate resolved for the stage's agent spawn from
 * the per-stage model_resolved audit. The detail page renders it when
 * non-empty and omits the line entirely on the empty (default-spawn /
 * legacy) path — the two observable branches of slice d's frontend.
 */
describe('<StageDetail> resolved model line', () => {
  beforeEach(() => {
    vi.restoreAllMocks();
    vi.spyOn(api, 'listStageArtifacts').mockResolvedValue({ items: [] });
  });
  afterEach(() => vi.restoreAllMocks());

  it('renders the resolved model when the Stage carries a non-empty resolved_model', async () => {
    vi.spyOn(api, 'getStage').mockResolvedValue({
      ...deployStage,
      resolved_model: 'claude-opus-4-8',
    });

    renderRoute();

    await waitFor(() => {
      expect(screen.getByTestId('stage-resolved-model')).toBeInTheDocument();
    });
    expect(screen.getByTestId('stage-resolved-model')).toHaveTextContent('claude-opus-4-8');
  });

  it('omits the resolved-model line when resolved_model is empty (default-spawn / legacy path)', async () => {
    // deployStage carries resolved_model: '' — the no-resolution path.
    vi.spyOn(api, 'getStage').mockResolvedValue(deployStage);

    renderRoute();

    await waitFor(() => {
      expect(screen.getByRole('heading', { name: /deploy stage/i })).toBeInTheDocument();
    });
    expect(screen.queryByTestId('stage-resolved-model')).not.toBeInTheDocument();
  });
});
