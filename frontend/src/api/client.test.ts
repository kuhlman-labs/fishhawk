import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { api, ApiClientError, CSRF_COOKIE_NAME, CSRF_HEADER_NAME } from './client';

/*
 * The api.* wrappers are thin, but the CSRF auto-attach is the kind
 * of behaviour you only notice when it stops working. Lock it in.
 */

function setCookie(name: string, value: string) {
  // jsdom enforces the __Host- prefix's "must have Secure" rule, so
  // include Secure here even though tests run over http://localhost.
  // Path=/ + no Domain are also __Host- requirements.
  document.cookie = `${name}=${value}; path=/; Secure`;
}

function clearCookie(name: string) {
  document.cookie = `${name}=; path=/; Secure; expires=Thu, 01 Jan 1970 00:00:00 GMT`;
}

function mockFetch(): ReturnType<typeof vi.fn> {
  const fetchMock = vi.fn(
    async () =>
      new Response('{"items":[],"next_cursor":null}', {
        status: 200,
        headers: { 'Content-Type': 'application/json' },
      }),
  );
  vi.stubGlobal('fetch', fetchMock);
  return fetchMock;
}

function lastInit(fetchMock: ReturnType<typeof vi.fn>): RequestInit {
  const last = fetchMock.mock.calls[fetchMock.mock.calls.length - 1];
  return (last[1] as RequestInit) ?? {};
}

function headerOf(init: RequestInit, name: string): string | undefined {
  const headers = init.headers as Record<string, string> | undefined;
  if (!headers) return undefined;
  // Case-insensitive lookup — the wrapper always emits canonical
  // casing, so a direct lookup is enough.
  return headers[name];
}

describe('api request CSRF auto-attach', () => {
  beforeEach(() => {
    clearCookie(CSRF_COOKIE_NAME);
    vi.unstubAllGlobals();
  });
  afterEach(() => {
    clearCookie(CSRF_COOKIE_NAME);
    vi.unstubAllGlobals();
  });

  it('omits the CSRF header from a GET even when the cookie is present', async () => {
    setCookie(CSRF_COOKIE_NAME, 'tok123');
    const fetchMock = mockFetch();

    await api.listRuns();

    const init = lastInit(fetchMock);
    expect(init.method ?? 'GET').toBe('GET');
    expect(headerOf(init, CSRF_HEADER_NAME)).toBeUndefined();
  });

  it('attaches the CSRF header from the cookie on POST', async () => {
    setCookie(CSRF_COOKIE_NAME, 'tok-post');
    const fetchMock = mockFetch();

    await api.submitApproval('stage-id', { decision: 'approve' });

    const init = lastInit(fetchMock);
    expect(init.method).toBe('POST');
    expect(headerOf(init, CSRF_HEADER_NAME)).toBe('tok-post');
  });

  it('omits the CSRF header on POST when no cookie is set (bearer-token caller)', async () => {
    const fetchMock = mockFetch();

    await api.submitApproval('stage-id', { decision: 'approve' });

    const init = lastInit(fetchMock);
    expect(init.method).toBe('POST');
    expect(headerOf(init, CSRF_HEADER_NAME)).toBeUndefined();
  });

  it('forwards credentials: include so the session + CSRF cookies ride along', async () => {
    setCookie(CSRF_COOKIE_NAME, 'tok-creds');
    const fetchMock = mockFetch();

    await api.submitApproval('stage-id', { decision: 'approve' });

    const init = lastInit(fetchMock);
    expect(init.credentials).toBe('include');
  });
});

describe('api.listGlobalAudit', () => {
  beforeEach(() => vi.unstubAllGlobals());
  afterEach(() => vi.unstubAllGlobals());

  it('hits /v0/audit with no query string when no params are passed', async () => {
    const fetchMock = mockFetch();
    await api.listGlobalAudit();
    const url = fetchMock.mock.calls.at(-1)?.[0] as string;
    expect(url).toMatch(/\/v0\/audit$/);
  });

  it('serialises every filter into the query string (snake_case for run_id)', async () => {
    const fetchMock = mockFetch();
    await api.listGlobalAudit({
      limit: 20,
      cursor: 'abc',
      category: 'plan_generated',
      runId: '11111111-2222-3333-4444-555555555555',
    });
    const url = fetchMock.mock.calls.at(-1)?.[0] as string;
    expect(url).toContain('limit=20');
    expect(url).toContain('cursor=abc');
    expect(url).toContain('category=plan_generated');
    expect(url).toContain('run_id=11111111-2222-3333-4444-555555555555');
  });

  it('omits empty / undefined params from the query string', async () => {
    const fetchMock = mockFetch();
    await api.listGlobalAudit({ limit: 100 });
    const url = fetchMock.mock.calls.at(-1)?.[0] as string;
    expect(url).toContain('limit=100');
    expect(url).not.toContain('cursor=');
    expect(url).not.toContain('category=');
    expect(url).not.toContain('run_id=');
  });
});

describe('api.listRunAudit stage_id filter (#215)', () => {
  beforeEach(() => vi.unstubAllGlobals());
  afterEach(() => vi.unstubAllGlobals());

  it('serialises stageId as the snake_case `stage_id` query param', async () => {
    const fetchMock = mockFetch();
    await api.listRunAudit('11111111-2222-3333-4444-555555555555', {
      stageId: '99999999-9999-9999-9999-999999999999',
    });
    const url = fetchMock.mock.calls.at(-1)?.[0] as string;
    expect(url).toContain('stage_id=99999999-9999-9999-9999-999999999999');
  });

  it('omits stage_id from the query string when not provided', async () => {
    const fetchMock = mockFetch();
    await api.listRunAudit('11111111-2222-3333-4444-555555555555');
    const url = fetchMock.mock.calls.at(-1)?.[0] as string;
    expect(url).not.toContain('stage_id=');
  });
});

describe('api.getStagePromptRender (#215)', () => {
  beforeEach(() => vi.unstubAllGlobals());
  afterEach(() => vi.unstubAllGlobals());

  it('hits /v0/stages/{id}/prompt-render with no body or signature header', async () => {
    const fetchMock = mockFetch();
    await api.getStagePromptRender('11111111-2222-3333-4444-555555555555');
    const url = fetchMock.mock.calls.at(-1)?.[0] as string;
    expect(url).toMatch(/\/v0\/stages\/[^/]+\/prompt-render$/);

    const init = lastInit(fetchMock);
    expect((init.method ?? 'GET').toUpperCase()).toBe('GET');
    expect(headerOf(init, 'X-Fishhawk-Signature')).toBeUndefined();
  });
});

describe('api.listCampaigns (ADR-047 / #1437)', () => {
  beforeEach(() => vi.unstubAllGlobals());
  afterEach(() => vi.unstubAllGlobals());

  it('hits /v0/campaigns with a GET and no query string when no params are passed', async () => {
    const fetchMock = mockFetch();
    await api.listCampaigns();
    const url = fetchMock.mock.calls.at(-1)?.[0] as string;
    expect(url).toMatch(/\/v0\/campaigns$/);
    const init = lastInit(fetchMock);
    expect((init.method ?? 'GET').toUpperCase()).toBe('GET');
  });

  it('serialises every filter into the query string', async () => {
    const fetchMock = mockFetch();
    await api.listCampaigns({
      limit: 25,
      cursor: 'abc',
      repo: 'kuhlman-labs/fishhawk',
      state: 'running',
    });
    const url = fetchMock.mock.calls.at(-1)?.[0] as string;
    expect(url).toContain('limit=25');
    expect(url).toContain('cursor=abc');
    expect(url).toContain('repo=kuhlman-labs%2Ffishhawk');
    expect(url).toContain('state=running');
  });

  it('omits empty / undefined params from the query string', async () => {
    const fetchMock = mockFetch();
    await api.listCampaigns({ limit: 50 });
    const url = fetchMock.mock.calls.at(-1)?.[0] as string;
    expect(url).toContain('limit=50');
    expect(url).not.toContain('cursor=');
    expect(url).not.toContain('repo=');
    expect(url).not.toContain('state=');
  });
});

describe('api.getCampaignStatus (ADR-047 / #1437)', () => {
  beforeEach(() => vi.unstubAllGlobals());
  afterEach(() => vi.unstubAllGlobals());

  it('hits /v0/campaigns/{id}/status with a GET and url-encodes the id', async () => {
    const fetchMock = mockFetch();
    await api.getCampaignStatus('cccccccc-1111-1111-1111-111111111111');
    const url = fetchMock.mock.calls.at(-1)?.[0] as string;
    expect(url).toBe('/v0/campaigns/cccccccc-1111-1111-1111-111111111111/status');
    const init = lastInit(fetchMock);
    expect((init.method ?? 'GET').toUpperCase()).toBe('GET');
  });
});

describe('api.getCampaign (ADR-047 / #1437)', () => {
  beforeEach(() => vi.unstubAllGlobals());
  afterEach(() => vi.unstubAllGlobals());

  it('hits /v0/campaigns/{id} with a GET', async () => {
    const fetchMock = mockFetch();
    await api.getCampaign('cccccccc-1111-1111-1111-111111111111');
    const url = fetchMock.mock.calls.at(-1)?.[0] as string;
    expect(url).toBe('/v0/campaigns/cccccccc-1111-1111-1111-111111111111');
  });
});

describe('api.listAttention (E40.1 / #1713)', () => {
  beforeEach(() => vi.unstubAllGlobals());
  afterEach(() => vi.unstubAllGlobals());

  it('hits /v0/attention with a GET and no query string when no params are passed', async () => {
    const fetchMock = mockFetch();
    await api.listAttention();
    expect(fetchMock.mock.calls[0][0]).toBe('/v0/attention');
    const init = lastInit(fetchMock);
    expect(init.method ?? 'GET').toBe('GET');
    expect(headerOf(init, CSRF_HEADER_NAME)).toBeUndefined();
  });

  it('serialises limit into the query string', async () => {
    const fetchMock = mockFetch();
    await api.listAttention({ limit: 25 });
    expect(fetchMock.mock.calls[0][0]).toBe('/v0/attention?limit=25');
  });

  it('omits an unset limit from the query string', async () => {
    const fetchMock = mockFetch();
    await api.listAttention({});
    expect(fetchMock.mock.calls[0][0]).toBe('/v0/attention');
  });
});

describe('api decision write surfaces (E40.2 / #1717)', () => {
  beforeEach(() => {
    clearCookie(CSRF_COOKIE_NAME);
    vi.unstubAllGlobals();
  });
  afterEach(() => {
    clearCookie(CSRF_COOKIE_NAME);
    vi.unstubAllGlobals();
  });

  function okFetch(body: unknown = {}): ReturnType<typeof vi.fn> {
    const fetchMock = vi.fn(
      async () =>
        new Response(JSON.stringify(body), {
          status: 200,
          headers: { 'Content-Type': 'application/json' },
        }),
    );
    vi.stubGlobal('fetch', fetchMock);
    return fetchMock;
  }

  it('decideScopeAmendment POSTs the decision to the run+amendment URL with CSRF', async () => {
    setCookie(CSRF_COOKIE_NAME, 'tok-amend');
    const fetchMock = okFetch();
    await api.decideScopeAmendment('run-1', 'amend-9', { decision: 'approve', reason: 'ok' });
    expect(fetchMock.mock.calls[0][0]).toBe('/v0/runs/run-1/scope-amendments/amend-9/decision');
    const init = lastInit(fetchMock);
    expect(init.method).toBe('POST');
    expect(headerOf(init, CSRF_HEADER_NAME)).toBe('tok-amend');
    expect(JSON.parse(String(init.body))).toEqual({ decision: 'approve', reason: 'ok' });
  });

  it('decideScopeAmendment url-encodes both path segments', async () => {
    const fetchMock = okFetch();
    await api.decideScopeAmendment('run/1', 'amend 9', { decision: 'deny' });
    expect(fetchMock.mock.calls[0][0]).toBe('/v0/runs/run%2F1/scope-amendments/amend%209/decision');
  });

  it('waiveConcern POSTs the reason to the concern waive URL', async () => {
    const fetchMock = okFetch();
    await api.waiveConcern('concern-3', { reason: 'accepted trade-off' });
    expect(fetchMock.mock.calls[0][0]).toBe('/v0/concerns/concern-3/waive');
    const init = lastInit(fetchMock);
    expect(init.method).toBe('POST');
    expect(JSON.parse(String(init.body))).toEqual({ reason: 'accepted trade-off' });
  });

  it('deferConcern POSTs parent_epic to the concern defer URL', async () => {
    const fetchMock = okFetch();
    await api.deferConcern('concern-4', { parent_epic: '#1196', note: 'later' });
    expect(fetchMock.mock.calls[0][0]).toBe('/v0/concerns/concern-4/defer');
    const init = lastInit(fetchMock);
    expect(init.method).toBe('POST');
    expect(JSON.parse(String(init.body))).toEqual({ parent_epic: '#1196', note: 'later' });
  });

  it('arbitrateAcceptance POSTs reason + acknowledge to the run URL', async () => {
    const fetchMock = okFetch();
    await api.arbitrateAcceptance('run-2', {
      reason: 'shipping anyway',
      acknowledge_failed_criteria: true,
    });
    expect(fetchMock.mock.calls[0][0]).toBe('/v0/runs/run-2/acceptance-arbitration');
    const init = lastInit(fetchMock);
    expect(init.method).toBe('POST');
    expect(JSON.parse(String(init.body))).toEqual({
      reason: 'shipping anyway',
      acknowledge_failed_criteria: true,
    });
  });

  it('throws ApiClientError carrying the parsed error code on a non-2xx', async () => {
    const fetchMock = vi.fn(
      async () =>
        new Response(JSON.stringify({ error: 'amendment_already_decided' }), {
          status: 409,
          headers: { 'Content-Type': 'application/json' },
        }),
    );
    vi.stubGlobal('fetch', fetchMock);
    const err = await api
      .decideScopeAmendment('run-1', 'amend-9', { decision: 'approve' })
      .then(() => null)
      .catch((e: unknown) => e);
    expect(err).toBeInstanceOf(ApiClientError);
    expect((err as ApiClientError).status).toBe(409);
    expect((err as ApiClientError).body?.error).toBe('amendment_already_decided');
  });
});

describe('api.listStageChecks (#228)', () => {
  beforeEach(() => vi.unstubAllGlobals());
  afterEach(() => vi.unstubAllGlobals());

  it('hits /v0/stages/{id}/checks with no body or body content', async () => {
    const fetchMock = mockFetch();
    await api.listStageChecks('11111111-2222-3333-4444-555555555555');
    const url = fetchMock.mock.calls.at(-1)?.[0] as string;
    expect(url).toMatch(/\/v0\/stages\/[^/]+\/checks$/);

    const init = lastInit(fetchMock);
    expect((init.method ?? 'GET').toUpperCase()).toBe('GET');
  });
});

describe('repo dashboard rollups (E40.3 / #1714)', () => {
  beforeEach(() => vi.unstubAllGlobals());
  afterEach(() => vi.unstubAllGlobals());

  it('builds each rollup path with owner and name encoded separately and no default weeks', async () => {
    const fetchMock = mockFetch();
    await api.getRepoThroughput('acme', 'app');
    await api.getRepoHealth('acme', 'app');
    await api.getRepoEconomics('acme', 'app');
    await api.getRepoPosture('acme', 'app');
    expect(fetchMock.mock.calls.map((c) => c[0])).toEqual([
      '/v0/repos/acme/app/throughput',
      '/v0/repos/acme/app/health',
      '/v0/repos/acme/app/economics',
      '/v0/repos/acme/app/posture',
    ]);
    for (const call of fetchMock.mock.calls) {
      const init = (call[1] as RequestInit) ?? {};
      expect((init.method ?? 'GET').toUpperCase()).toBe('GET');
    }
  });

  it('passes weeks only when the caller sets it', async () => {
    const fetchMock = mockFetch();
    await api.getRepoThroughput('acme', 'app', { weeks: 4 });
    await api.getRepoHealth('acme', 'app', { weeks: 52 });
    await api.getRepoEconomics('acme', 'app', { weeks: 1 });
    await api.getRepoThroughput('acme', 'app', {});
    expect(fetchMock.mock.calls.map((c) => c[0])).toEqual([
      '/v0/repos/acme/app/throughput?weeks=4',
      '/v0/repos/acme/app/health?weeks=52',
      '/v0/repos/acme/app/economics?weeks=1',
      '/v0/repos/acme/app/throughput',
    ]);
  });

  it('encodes a separator inside owner or name rather than splitting the path', async () => {
    const fetchMock = mockFetch();
    await api.getRepoHealth('a/b', 'c d?');
    expect(fetchMock.mock.calls[0][0]).toBe('/v0/repos/a%2Fb/c%20d%3F/health');
  });

  it('getHealth hits /healthz', async () => {
    const fetchMock = mockFetch();
    await api.getHealth();
    expect(fetchMock.mock.calls[0][0]).toBe('/healthz');
  });
});
