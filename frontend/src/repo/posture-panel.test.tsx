import { readFileSync } from 'node:fs';
import { resolve } from 'node:path';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { render, screen, within } from '@testing-library/react';
import { MemoryRouter } from 'react-router';
import { api } from '@/api/client';
import type { AsyncState } from '@/api/use-async';
import type { RepoPosture, RepoPostureResponse } from '@/api/types';
import { PosturePanel } from './posture-panel';

/*
 * The payload is the SHARED wire golden the backend's repodash seam test
 * pins (testdata/wire/repodash_posture.json), served from a stubbed fetch
 * through the REAL api client (approval condition 3). Drift variants are
 * DERIVED from the golden, never a hand-authored second copy.
 */
const GOLDEN_BYTES = readFileSync(
  resolve(__dirname, '../../../testdata/wire/repodash_posture.json'),
  'utf8',
);

function golden(): RepoPosture {
  return JSON.parse(GOLDEN_BYTES) as RepoPosture;
}

function stubFetch(status: number, body: string) {
  vi.stubGlobal(
    'fetch',
    vi.fn(
      async () => new Response(body, { status, headers: { 'Content-Type': 'application/json' } }),
    ),
  );
}

async function loadPosture(body: string = GOLDEN_BYTES): Promise<AsyncState<RepoPostureResponse>> {
  stubFetch(200, body);
  return { status: 'ok', data: await api.getRepoPosture('acme', 'app') };
}

function renderPanel(state: AsyncState<RepoPostureResponse>) {
  return render(
    <MemoryRouter>
      <PosturePanel state={state} />
    </MemoryRouter>,
  );
}

afterEach(() => vi.unstubAllGlobals());

describe('<PosturePanel>', () => {
  it('renders the golden (supported + valid) with NO drift warning', async () => {
    renderPanel(await loadPosture());
    expect(screen.getByRole('heading', { name: 'Posture' })).toBeInTheDocument();
    expect(screen.queryByRole('alert')).toBeNull();
    expect(screen.getByText('0.4')).toBeInTheDocument();
    expect(screen.getByTitle(golden().schema_hash!)).toHaveTextContent('e55f960ddc75');
    expect(screen.getByRole('link', { name: '00000000' })).toHaveAttribute(
      'href',
      '/runs/00000000-0000-0000-0000-000000000001',
    );

    const stages = within(screen.getByRole('table', { name: 'Stages of feature_change' }));
    const plan = stages.getByText('plan').closest('tr')!;
    expect(plan).toHaveTextContent('claude-code');
    expect(plan).toHaveTextContent('approval · any of tech_lead');
    expect(plan).toHaveTextContent('anthropic/claude-opus-5-5');
    const implement = stages.getByText('implement').closest('tr')!;
    expect(implement).toHaveTextContent('1000 tokens');
    expect(
      within(screen.getByRole('list', { name: 'Periodic budgets of feature_change' })).getByRole(
        'listitem',
      ),
    ).toHaveTextContent('weekly budget $50');
  });

  it('warns when the declared major is not embedded, naming the version and the error', async () => {
    const g = golden();
    g.version = '9.0';
    g.schema_major = 9;
    delete g.schema_hash;
    g.schema_supported = false;
    g.spec_valid = false;
    g.spec_error = 'unsupported workflow spec version "9.0"';
    g.workflows = [];
    renderPanel(await loadPosture(JSON.stringify(g)));
    const alert = screen.getByRole('alert', { name: 'Workflow spec drift' });
    expect(alert).toHaveTextContent(
      'The spec declares version 9.0 (workflow-v9), which this fishhawkd embeds no schema for.',
    );
    expect(alert).toHaveTextContent('unsupported workflow spec version "9.0"');
  });

  it('warns when the spec is invalid against a supported major, naming the error', async () => {
    const g = golden();
    g.spec_valid = false;
    g.spec_error = 'workflows.feature_change.stages[0].type: must be one of plan, implement';
    g.workflows = [];
    renderPanel(await loadPosture(JSON.stringify(g)));
    const alert = screen.getByRole('alert', { name: 'Workflow spec drift' });
    expect(alert).toHaveTextContent(
      'The spec at version 0.4 does not validate against the embedded workflow-v0 schema: workflows.feature_change.stages[0].type',
    );
    expect(alert).not.toHaveTextContent('embeds no schema');
  });

  it('warns on an unsupported major even when spec_valid is true (isolates the support check)', async () => {
    const g = golden();
    g.schema_supported = false;
    renderPanel(await loadPosture(JSON.stringify(g)));
    const alert = screen.getByRole('alert', { name: 'Workflow spec drift' });
    expect(alert).toHaveTextContent('embeds no schema');
    expect(alert).not.toHaveTextContent('does not validate');
  });

  it('ends the invalid-spec sentence cleanly when no spec_error was reported', async () => {
    const g = golden();
    g.spec_valid = false;
    delete g.spec_error;
    renderPanel(await loadPosture(JSON.stringify(g)));
    expect(screen.getByRole('alert')).toHaveTextContent(
      'does not validate against the embedded workflow-v0 schema.',
    );
  });

  it('renders the no-spec note for an empty {} body, with no warning', async () => {
    renderPanel(await loadPosture('{}'));
    expect(
      screen.getByText('No run for this repo carries a cached workflow spec yet.'),
    ).toBeInTheDocument();
    expect(screen.queryByRole('alert')).toBeNull();
  });

  it('renders a panel-scoped alert when the fetch fails', async () => {
    stubFetch(403, '{"error":"repo_forbidden","message":"repo not visible"}');
    const error = await api.getRepoPosture('acme', 'app').catch((e: Error) => e);
    renderPanel({ status: 'error', error: error as Error });
    expect(screen.getByRole('alert')).toHaveTextContent('repo not visible');
  });
});
