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
