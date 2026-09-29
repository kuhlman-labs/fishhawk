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
 * Mounts the REAL route with the REAL panels of both tabs. The four rollup
 * endpoints serve the SHARED wire goldens the backend seam test pins
 * (testdata/wire/repodash_*.json, approval condition 3) through a stubbed
 * fetch and the real api client; the in-flight surfaces serve a minimal
 * run list, and /v0/audit/export serves a clean Export v1 body for the
 * Record tab's chain-verification badge (E40.6 / #1718).
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

/* A minimal clean Export v1 body for the Record tab's badge (E40.6 / #1718). */
const EXPORT_RUN_ID = '11111111-1111-4111-8111-111111111111';
const EXPORT_BODY = JSON.stringify({
  schema: 'v1',
  exported_at: '2026-09-20T12:00:00Z',
  runs: {
    [EXPORT_RUN_ID]: {
      signing_key: {
        public_key: 'AAAA',
        issued_at: '2026-09-01T00:00:00Z',
        expires_at: '2026-12-01T00:00:00Z',
      },
      audit_entries: [
        {
          id: '00000000-0000-4000-8000-000000000001',
          sequence: 1,
          run_id: EXPORT_RUN_ID,
          stage_id: null,
          ts: '2026-09-19T00:00:00Z',
          category: 'run_created',
          actor_kind: 'operator',
          actor_subject: 'octo',
          payload: {},
          prev_hash: null,
          entry_hash: 'a'.repeat(64),
        },
      ],
    },
  },
});

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
    if (p === '/v0/audit/export') {
      return new Response(EXPORT_BODY, {
        status: 200,
        headers: { 'Content-Type': 'application/json', 'X-Fishhawk-Export-Complete': 'true' },
      });
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
const RECORD_SECTIONS = ['Chain verification', 'Export', 'Release evidence'];

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

  it('renders the Record tab at ?tab=record: its sections, no rollup panels, no rollup fetches', async () => {
    const fetchMock = stubBackend();
    renderAt('/repos/acme/app?tab=record');
    await screen.findByText('Pass');

    expect(screen.getAllByRole('heading', { level: 2 }).map((h) => h.textContent)).toEqual(
      RECORD_SECTIONS,
    );
    for (const title of PANELS) {
      expect(screen.queryByRole('heading', { name: title })).toBeNull();
    }
    const urls = fetchMock.mock.calls.map((c) => String(c[0]));
    expect(urls.some((u) => /\/v0\/repos\/acme\/app\//.test(u))).toBe(false);
    expect(urls).toContain('/v0/audit/export?repo=acme%2Fapp&limit=25');
  });

  it('switches between the tabs by link, both ways, marking the selected one', async () => {
    stubBackend();
    renderAt('/repos/acme/app');
    const nav = screen.getByRole('navigation', { name: 'Repository views' });
    expect(within(nav).getByRole('link', { name: 'Overview' })).toHaveAttribute(
      'aria-current',
      'page',
    );
    await waitFor(() => expect(screen.getByText('Merged changes')).toBeInTheDocument());

    fireEvent.click(within(nav).getByRole('link', { name: 'Record' }));
    expect(await screen.findByRole('heading', { name: 'Chain verification' })).toBeInTheDocument();
    expect(screen.queryByRole('heading', { name: 'Throughput' })).toBeNull();
    expect(within(nav).getByRole('link', { name: 'Record' })).toHaveAttribute(
      'aria-current',
      'page',
    );

    fireEvent.click(within(nav).getByRole('link', { name: 'Overview' }));
    expect(await screen.findByRole('heading', { name: 'Throughput' })).toBeInTheDocument();
    expect(screen.queryByRole('heading', { name: 'Chain verification' })).toBeNull();
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
