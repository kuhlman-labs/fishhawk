import { readFileSync } from 'node:fs';
import { resolve } from 'node:path';
import { afterEach, describe, expect, it, vi } from 'vitest';
import { fireEvent, render, screen, waitFor, within } from '@testing-library/react';
import { MemoryRouter, Route, Routes } from 'react-router';
import type { Run } from '@/api/types';
import { App } from '@/App';
import { RepoDashboard } from './repo-dashboard';
import { Runs } from './runs';

/*
 * Mounts the REAL route with the REAL five panels. The four rollup
 * endpoints serve the SHARED wire goldens the backend seam test pins
 * (testdata/wire/repodash_*.json, approval condition 3) through a stubbed
 * fetch and the real api client; the in-flight surfaces serve a minimal
 * run list.
 */
const WIRE = resolve(__dirname, '../../../testdata/wire');
const GOLDEN: Record<string, string> = Object.fromEntries(
  ['throughput', 'health', 'economics', 'posture'].map((k) => [
    k,
    readFileSync(resolve(WIRE, `repodash_${k}.json`), 'utf8'),
  ]),
);

const RUN: Run = {
  id: 'run-1',
  repo: 'acme/app',
  workflow_id: 'feature_change',
  workflow_sha: 'abc123',
  trigger_source: 'github_issue',
  trigger_ref: 'issue:42',
  state: 'running',
  retry_attempt: 0,
  max_retries_snapshot: 1,
  created_at: '2026-09-28T12:00:00Z',
  updated_at: '2026-09-28T12:00:00Z',
};

function stubBackend({ failing }: { failing?: string } = {}) {
  const fetchMock = vi.fn(async (input: RequestInfo | URL) => {
    const p = new URL(String(input), 'http://localhost').pathname;
    const respond = (body: string, status = 200) =>
      new Response(body, { status, headers: { 'Content-Type': 'application/json' } });
    const m = p.match(/^\/v0\/repos\/acme\/app\/(throughput|health|economics|posture)$/);
    if (m) {
      return m[1] === failing
        ? respond('{"error":"audit_repo_unavailable","message":"audit store down"}', 503)
        : respond(GOLDEN[m[1]]);
    }
    if (p === '/v0/auth/me') {
      return respond(JSON.stringify({ id: 'u1', github_login: 'octo', name: 'Octo', email: null }));
    }
    if (p === '/v0/runs') return respond(JSON.stringify({ items: [RUN], next_cursor: null }));
    if (p === '/v0/runs/run-1/stages') return respond('{"items":[]}');
    if (p === '/v0/campaigns') return respond('{"items":[],"next_cursor":null}');
    return respond('{"error":"not_found","message":"unrouted"}', 404);
  });
  vi.stubGlobal('fetch', fetchMock);
  return fetchMock;
}

function renderAt(path: string) {
  return render(
    <MemoryRouter initialEntries={[path]}>
      <Routes>
        <Route path="runs" element={<Runs />} />
        <Route path="repos/:owner/:name" element={<RepoDashboard />} />
      </Routes>
    </MemoryRouter>,
  );
}

function panel(title: string): HTMLElement {
  return screen.getByRole('heading', { name: title }).closest('section')!;
}

const PANELS = ['In flight', 'Throughput', 'Economics', 'Health', 'Posture'];

afterEach(() => vi.unstubAllGlobals());

describe('<RepoDashboard>', () => {
  it('renders all five panels, in order, from the wire goldens', async () => {
    const fetchMock = stubBackend();
    renderAt('/repos/acme/app');
    expect(screen.getByRole('heading', { level: 1, name: 'acme/app' })).toBeInTheDocument();
    await waitFor(() => expect(screen.getByText('Plan first-shot approval')).toBeInTheDocument());
    await waitFor(() => expect(screen.getByText('Active runs')).toBeInTheDocument());
    await waitFor(() =>
      expect(screen.getByText('feature_change', { selector: 'span' })).toBeInTheDocument(),
    );

    const headings = screen.getAllByRole('heading', { level: 2 }).map((h) => h.textContent);
    expect(headings).toEqual(PANELS);
    expect(
      within(panel('Throughput')).getByText('Merged changes').nextElementSibling,
    ).toHaveTextContent('1');
    expect(within(panel('Health')).getByText('Fixup rate').nextElementSibling).toHaveTextContent(
      '50%',
    );
    expect(within(panel('Economics')).queryByText('No cost recorded in this window.')).toBeNull();
    expect(
      within(panel('In flight')).getByRole('link', { name: 'feature_change' }),
    ).toHaveAttribute('href', '/runs/run-1');
    expect(screen.queryByRole('alert')).toBeNull();

    const urls = fetchMock.mock.calls.map((c) => String(c[0]));
    expect(urls).toContain('/v0/repos/acme/app/throughput');
    expect(urls).toContain('/v0/runs?limit=100&repo=acme%2Fapp');
  });

  it('isolates a failing rollup: one panel-scoped alert, the other four still render', async () => {
    stubBackend({ failing: 'health' });
    renderAt('/repos/acme/app');
    const alert = await screen.findByRole('alert');
    await waitFor(() => expect(screen.getByText('Merged changes')).toBeInTheDocument());
    await waitFor(() => expect(screen.getByText('Active runs')).toBeInTheDocument());
    await waitFor(() => expect(screen.getByText('0.4')).toBeInTheDocument());

    expect(screen.getAllByRole('alert')).toHaveLength(1);
    expect(panel('Health')).toContainElement(alert);
    expect(alert).toHaveTextContent('audit store down');
    for (const title of PANELS) {
      expect(screen.getByRole('heading', { name: title })).toBeInTheDocument();
    }
    expect(within(panel('Throughput')).queryByRole('alert')).toBeNull();
  });
});

describe('App route table', () => {
  it('mounts the dashboard at /repos/:owner/:name inside the authenticated shell', async () => {
    stubBackend();
    render(
      <MemoryRouter initialEntries={['/repos/acme/app']}>
        <App />
      </MemoryRouter>,
    );
    expect(await screen.findByRole('heading', { level: 1, name: 'acme/app' })).toBeInTheDocument();
    expect(await screen.findByText('Plan first-shot approval')).toBeInTheDocument();
  });
});

describe('<Runs> → repo dashboard link', () => {
  it('links the repo cell to /repos/:owner/:name and the workflow cell to the run', async () => {
    stubBackend();
    renderAt('/runs');
    const repoLink = await screen.findByRole('link', { name: 'acme/app' });
    expect(repoLink).toHaveAttribute('href', '/repos/acme/app');
    expect(screen.getByRole('link', { name: 'feature_change' })).toHaveAttribute(
      'href',
      '/runs/run-1',
    );
    fireEvent.click(repoLink);
    expect(await screen.findByRole('heading', { level: 1, name: 'acme/app' })).toBeInTheDocument();
  });

  it('renders a repo that is not owner/name as plain text, not a dead link', async () => {
    vi.stubGlobal(
      'fetch',
      vi.fn(
        async () =>
          new Response(
            JSON.stringify({ items: [{ ...RUN, repo: 'no-slash' }], next_cursor: null }),
            {
              status: 200,
              headers: { 'Content-Type': 'application/json' },
            },
          ),
      ),
    );
    renderAt('/runs');
    expect(await screen.findByText('no-slash')).toBeInTheDocument();
    expect(screen.queryByRole('link', { name: 'no-slash' })).toBeNull();
  });
});
