import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { fireEvent, render, screen, waitFor } from '@testing-library/react';
import { MemoryRouter } from 'react-router';
import { App } from './App';
import type { User } from './auth/types';

const sampleUser: User = {
  id: '00000000-0000-0000-0000-000000000001',
  github_login: 'kuhlman-labs',
  name: 'Brett Kuhlman',
  email: 'brett@example.com',
};

type MeResponse = { ok: true; user: User } | { ok: false } | { ok: 'throw' };

function mockAuth(me: MeResponse) {
  /*
   * Centralized fetch stub. Anything beyond /v0/auth/* falls through
   * with a 404 so unrelated network calls in routes that haven't
   * been wired to the API yet (runs, audit) don't quietly succeed
   * with stale fixtures.
   */
  const fetchMock = vi.fn(async (input: RequestInfo | URL) => {
    const url = typeof input === 'string' ? input : input.toString();
    if (url.endsWith('/v0/auth/me')) {
      if (me.ok === 'throw') {
        throw new Error('network');
      }
      if (me.ok) {
        return new Response(JSON.stringify(me.user), {
          status: 200,
          headers: { 'Content-Type': 'application/json' },
        });
      }
      return new Response('{"error":"auth_required"}', {
        status: 401,
        headers: { 'Content-Type': 'application/json' },
      });
    }
    if (url.endsWith('/v0/auth/logout')) {
      return new Response(null, { status: 204 });
    }
    return new Response('not stubbed', { status: 404 });
  });
  vi.stubGlobal('fetch', fetchMock);
  return fetchMock;
}

function renderAt(path: string) {
  return render(
    <MemoryRouter initialEntries={[path]}>
      <App />
    </MemoryRouter>,
  );
}

describe('App routing + auth', () => {
  beforeEach(() => {
    vi.unstubAllGlobals();
  });
  afterEach(() => {
    vi.unstubAllGlobals();
  });

  describe('authenticated', () => {
    beforeEach(() => {
      mockAuth({ ok: true, user: sampleUser });
    });

    it('renders the runs route as the index', async () => {
      renderAt('/');
      expect(await screen.findByRole('heading', { name: 'Runs' })).toBeInTheDocument();
    });

    it('renders the audit route', async () => {
      renderAt('/audit');
      expect(await screen.findByRole('heading', { name: 'Audit log' })).toBeInTheDocument();
    });

    it('shows the signed-in user in the shell', async () => {
      renderAt('/');
      expect(await screen.findByText('@kuhlman-labs')).toBeInTheDocument();
      expect(screen.getByRole('button', { name: /sign out/i })).toBeInTheDocument();
    });

    it('redirects /login → / when already signed in', async () => {
      renderAt('/login');
      // Lands on the runs index (rendered inside Root) after the redirect.
      expect(await screen.findByRole('heading', { name: 'Runs' })).toBeInTheDocument();
    });

    it('signs out via POST /v0/auth/logout and routes to /login', async () => {
      const fetchMock = mockAuth({ ok: true, user: sampleUser });
      renderAt('/');
      const signOut = await screen.findByRole('button', { name: /sign out/i });
      fireEvent.click(signOut);

      // Logout request fired with credentials.
      await waitFor(() => {
        const logoutCall = fetchMock.mock.calls.find(([url]) =>
          String(url).endsWith('/v0/auth/logout'),
        );
        expect(logoutCall).toBeDefined();
        expect(logoutCall?.[1]).toMatchObject({
          method: 'POST',
          credentials: 'include',
        });
      });

      // Now on /login, with the OAuth-start link rendered.
      expect(
        await screen.findByRole('link', { name: /continue with github/i }),
      ).toBeInTheDocument();
    });
  });

  describe('unauthenticated', () => {
    beforeEach(() => {
      mockAuth({ ok: false });
    });

    it('renders the login route outside the app shell', async () => {
      renderAt('/login');
      const link = await screen.findByRole('link', {
        name: /continue with github/i,
      });
      expect(link).toHaveAttribute('href', '/v0/auth/github/login');
      expect(screen.queryByRole('link', { name: /^runs$/i })).not.toBeInTheDocument();
    });

    it('redirects protected routes to /login', async () => {
      renderAt('/runs');
      expect(
        await screen.findByRole('link', { name: /continue with github/i }),
      ).toBeInTheDocument();
    });

    // E7.2.1 #153: deep-link routing intent must survive the OAuth
    // round-trip. RequireAuth encodes the original path as ?next=
    // when redirecting to /login; the Login route then forwards it
    // to the backend's OAuth start endpoint.
    it('forwards the original path to the OAuth start as ?next=', async () => {
      renderAt('/runs/abc-123');
      const link = await screen.findByRole('link', { name: /continue with github/i });
      expect(link).toHaveAttribute(
        'href',
        '/v0/auth/github/login?next=' + encodeURIComponent('/runs/abc-123'),
      );
    });

    it('preserves query params in the next intent', async () => {
      renderAt('/audit?category=approval_submitted');
      const link = await screen.findByRole('link', { name: /continue with github/i });
      const expected =
        '/v0/auth/github/login?next=' + encodeURIComponent('/audit?category=approval_submitted');
      expect(link).toHaveAttribute('href', expected);
    });

    it('omits ?next= when redirected from the index path', async () => {
      // Hitting "/" is the default landing target; no point round-tripping it.
      renderAt('/');
      const link = await screen.findByRole('link', { name: /continue with github/i });
      expect(link).toHaveAttribute('href', '/v0/auth/github/login');
    });

    it('renders a not-found page for unknown public routes', async () => {
      renderAt('/does-not-exist');
      expect(await screen.findByRole('heading', { name: /not found/i })).toBeInTheDocument();
    });
  });

  describe('network failure', () => {
    it('falls through to unauthenticated rather than blocking', async () => {
      mockAuth({ ok: 'throw' });
      renderAt('/runs');
      expect(
        await screen.findByRole('link', { name: /continue with github/i }),
      ).toBeInTheDocument();
    });
  });
});
