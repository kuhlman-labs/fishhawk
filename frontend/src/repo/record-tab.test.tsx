import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { fireEvent, render, screen, waitFor, within } from '@testing-library/react';
import { RecordTab } from './record-tab';

/*
 * Mounts the REAL <RecordTab> through the REAL api client over a stubbed
 * global fetch serving real Export v1 bodies (E40.6 / #1718). This is the
 * cross-boundary test: client → types → verifyExportChain → render is
 * exercised end to end rather than per layer.
 */

const RUN_A = '11111111-1111-4111-8111-111111111111';
const H1 = 'a'.repeat(64);
const H2 = 'b'.repeat(64);
const UNRELATED = 'd'.repeat(64);
const EXPORTED_AT = '2026-09-20T12:00:00Z';

function entry(sequence: number, prevHash: string | null, entryHash: string) {
  return {
    id: `00000000-0000-4000-8000-${String(sequence).padStart(12, '0')}`,
    sequence,
    run_id: RUN_A,
    stage_id: null,
    ts: '2026-09-19T00:00:00Z',
    category: 'run_created',
    actor_kind: 'operator',
    actor_subject: 'octo',
    payload: {},
    prev_hash: prevHash,
    entry_hash: entryHash,
  };
}

const SIGNING_KEY = {
  public_key: 'AAAA',
  issued_at: '2026-09-01T00:00:00Z',
  expires_at: '2026-12-01T00:00:00Z',
};

function exportBody(entries: ReturnType<typeof entry>[]) {
  return JSON.stringify({
    schema: 'v1',
    exported_at: EXPORTED_AT,
    runs: { [RUN_A]: { signing_key: SIGNING_KEY, audit_entries: entries } },
  });
}

const CLEAN = exportBody([entry(1, null, H1), entry(2, H1, H2)]);
const TAMPERED = exportBody([entry(1, null, H1), entry(2, UNRELATED, H2)]);

const SCOPE_403 =
  '{"error":"insufficient_scope","message":"token is missing required scope: read:audit-export",' +
  '"details":{"required_scope":"read:audit-export"}}';

interface StubOptions {
  body?: string;
  /** `null` omits the header entirely, mirroring a response without one. */
  completeHeader?: string | null;
  status?: number;
}

function stubExport({ body = CLEAN, completeHeader = 'true', status = 200 }: StubOptions = {}) {
  const fetchMock = vi.fn(async () => {
    const headers: Record<string, string> = { 'Content-Type': 'application/json' };
    if (status === 200 && completeHeader !== null) {
      headers['X-Fishhawk-Export-Complete'] = completeHeader;
    }
    return new Response(body, { status, headers });
  });
  vi.stubGlobal('fetch', fetchMock);
  return fetchMock;
}

function renderTab() {
  return render(<RecordTab repo="acme/app" owner="acme" name="app" />);
}

function panel(title: string): HTMLElement {
  return screen.getByRole('heading', { name: title }).closest('section')!;
}

beforeEach(() => {
  vi.unstubAllGlobals();
});
afterEach(() => {
  vi.unstubAllGlobals();
  vi.restoreAllMocks();
});

describe('<RecordTab> sections', () => {
  it('renders all three section headings', async () => {
    stubExport();
    renderTab();
    await screen.findByText('Pass');
    expect(screen.getAllByRole('heading', { level: 2 }).map((h) => h.textContent)).toEqual([
      'Chain verification',
      'Export',
      'Release evidence',
    ]);
  });

  it('renders the release-evidence placeholder, visibly disabled', async () => {
    stubExport();
    renderTab();
    const placeholder = panel('Release evidence');
    expect(placeholder).toHaveTextContent('E33 (#1583)');
    expect(
      within(placeholder).getByRole('button', { name: 'View release evidence' }),
    ).toBeDisabled();
    await screen.findByText('Pass');
  });
});

describe('<RecordTab> chain-verification badge', () => {
  it('reports a pass on a clean export and names the offline verifier', async () => {
    stubExport();
    renderTab();
    const badge = panel('Chain verification');
    await within(badge).findByText('Pass');
    expect(badge).toHaveTextContent('1 run, 2 entries checked');
    expect(badge).toHaveTextContent('Signing-key coverage');
    expect(badge).toHaveTextContent('fishhawk-verify');
    expect(badge).not.toHaveTextContent('signatures verified');
  });

  it('reports a fail naming the broken link on a tampered export', async () => {
    stubExport({ body: TAMPERED });
    renderTab();
    const badge = panel('Chain verification');
    await within(badge).findByText('Fail');
    expect(badge).toHaveTextContent('chain_broken');
    expect(badge).toHaveTextContent(`run ${RUN_A}`);
    expect(badge).toHaveTextContent('seq 2');
  });

  it('reports unverifiable — never a pass — on a body that is not Export v1', async () => {
    stubExport({ body: '{"schema":"v2","exported_at":"2026-09-20T12:00:00Z","runs":{}}' });
    renderTab();
    const badge = panel('Chain verification');
    await within(badge).findByText('Unverifiable');
    expect(within(badge).queryByText('Pass')).toBeNull();
  });

  it('renders no anchored-checkpoint claim (ADR-056 / #1699 has not landed)', async () => {
    stubExport();
    renderTab();
    await screen.findByText('Pass');
    expect(panel('Chain verification').textContent ?? '').not.toMatch(/anchor|checkpoint/i);
  });

  it('renders the partial note when the export page carries no complete header', async () => {
    stubExport({ completeHeader: null });
    renderTab();
    expect(await screen.findByText(/Partial data/)).toBeInTheDocument();
  });

  it("renders the partial note when the complete header is the literal 'false'", async () => {
    stubExport({ completeHeader: 'false' });
    renderTab();
    expect(await screen.findByText(/Partial data/)).toBeInTheDocument();
  });

  it('omits the partial note on a complete page', async () => {
    stubExport({ completeHeader: 'true' });
    renderTab();
    await screen.findByText('Pass');
    expect(screen.queryByText(/Partial data/)).toBeNull();
  });

  it('renders a panel-scoped alert when the export read fails', async () => {
    stubExport({ status: 503, body: '{"error":"audit_unavailable","message":"audit store down"}' });
    renderTab();
    const alert = await within(panel('Chain verification')).findByRole('alert');
    expect(alert).toHaveTextContent('audit store down');
  });
});

describe('<RecordTab> export download', () => {
  it('triggers a download and revokes the object URL', async () => {
    // jsdom implements neither of these (jsdom#1721); an unstubbed call
    // would throw a TypeError rather than pass vacuously.
    const createObjectURL = vi.fn(() => 'blob:stub');
    const revokeObjectURL = vi.fn();
    vi.stubGlobal('URL', Object.assign(URL, { createObjectURL, revokeObjectURL }));
    stubExport();
    const clicks: HTMLAnchorElement[] = [];
    vi.spyOn(HTMLAnchorElement.prototype, 'click').mockImplementation(function (
      this: HTMLAnchorElement,
    ) {
      clicks.push(this);
    });

    renderTab();
    fireEvent.click(await screen.findByRole('button', { name: 'Download export (JSON)' }));

    await waitFor(() => expect(clicks).toHaveLength(1));
    expect(clicks[0].download).toBe('fishhawk-audit-export-acme-app.json');
    expect(clicks[0].href).toBe('blob:stub');
    expect(revokeObjectURL).toHaveBeenCalledWith('blob:stub');
  });

  it('renders the re-auth panel on a 403, naming the scope and the instruction', async () => {
    stubExport({ status: 403, body: SCOPE_403 });
    renderTab();
    fireEvent.click(await screen.findByRole('button', { name: 'Download export (JSON)' }));
    const alert = await within(panel('Export')).findByRole('alert');
    expect(alert).toHaveTextContent('read:audit-export');
    // The server's own 403 message ALSO contains the scope name, so the
    // discriminating assertion is the instruction only this branch emits.
    expect(alert).toHaveTextContent('Re-authenticate with a token that carries it');
    expect(alert).not.toHaveTextContent("Couldn't download the export.");
  });

  it('falls back to the literal scope when the 403 body carries no details', async () => {
    stubExport({ status: 403, body: '{"error":"insufficient_scope"}' });
    renderTab();
    fireEvent.click(await screen.findByRole('button', { name: 'Download export (JSON)' }));
    const alert = await within(panel('Export')).findByRole('alert');
    expect(alert).toHaveTextContent('read:audit-export');
    expect(alert).toHaveTextContent('Re-authenticate with a token that carries it');
  });

  it('renders the generic error on a 503, NOT the re-auth panel', async () => {
    stubExport({ status: 503, body: '{"error":"audit_unavailable","message":"audit store down"}' });
    renderTab();
    fireEvent.click(await screen.findByRole('button', { name: 'Download export (JSON)' }));
    const alert = await within(panel('Export')).findByRole('alert');
    expect(alert).toHaveTextContent("Couldn't download the export.");
    expect(alert).toHaveTextContent('audit store down');
    expect(alert).not.toHaveTextContent('Re-authenticate with a token that carries it');
  });
});
