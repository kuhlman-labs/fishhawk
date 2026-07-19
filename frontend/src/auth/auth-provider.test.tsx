import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import { render, screen } from '@testing-library/react';
import { AuthProvider } from './auth-provider';
import { useAuth } from './use-auth';
import type { User } from './types';

const sampleUser: User = {
  id: '00000000-0000-0000-0000-000000000001',
  github_login: 'kuhlman-labs',
  name: 'Brett Kuhlman',
  email: 'brett@example.com',
  account_id: '00000000-0000-0000-0000-0000000000aa',
};

function Probe() {
  const { status, user } = useAuth();
  return (
    <div>
      <div data-testid="status">{status}</div>
      <div data-testid="account-id">{user?.account_id ?? 'none'}</div>
    </div>
  );
}

function stubMe(response: () => Promise<Response>) {
  vi.stubGlobal(
    'fetch',
    vi.fn(async (input: RequestInfo | URL) => {
      const url = typeof input === 'string' ? input : input.toString();
      if (url.endsWith('/v0/auth/me')) {
        return response();
      }
      return new Response('not stubbed', { status: 404 });
    }),
  );
}

function renderProvider() {
  return render(
    <AuthProvider>
      <Probe />
    </AuthProvider>,
  );
}

describe('AuthProvider /v0/auth/me status mapping', () => {
  beforeEach(() => {
    vi.unstubAllGlobals();
  });
  afterEach(() => {
    vi.unstubAllGlobals();
  });

  it('maps 200 to authenticated with the account_id from the response', async () => {
    stubMe(async () => new Response(JSON.stringify(sampleUser), { status: 200 }));
    renderProvider();
    expect(await screen.findByTestId('status')).toHaveTextContent('authenticated');
    expect(screen.getByTestId('account-id')).toHaveTextContent(
      '00000000-0000-0000-0000-0000000000aa',
    );
  });

  // E44.3 #1827: a valid session with no resolvable workspace account
  // is denied, not unauthenticated — re-running the OAuth flow would
  // just land back on the same 403.
  it('maps 403 to denied', async () => {
    stubMe(async () => new Response('{"error":"account_unresolved"}', { status: 403 }));
    renderProvider();
    expect(await screen.findByTestId('status')).toHaveTextContent('denied');
    expect(screen.getByTestId('account-id')).toHaveTextContent('none');
  });

  it('maps 401 to unauthenticated', async () => {
    stubMe(async () => new Response('{"error":"auth_required"}', { status: 401 }));
    renderProvider();
    expect(await screen.findByTestId('status')).toHaveTextContent('unauthenticated');
  });

  it('maps a network failure to unauthenticated', async () => {
    stubMe(async () => {
      throw new Error('network');
    });
    renderProvider();
    expect(await screen.findByTestId('status')).toHaveTextContent('unauthenticated');
  });
});
