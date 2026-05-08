import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { api, CSRF_COOKIE_NAME, CSRF_HEADER_NAME } from './client';

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
