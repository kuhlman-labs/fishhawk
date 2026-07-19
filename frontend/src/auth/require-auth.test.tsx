import { describe, expect, it, vi } from 'vitest';
import { render, screen } from '@testing-library/react';
import { MemoryRouter, Route, Routes } from 'react-router';
import { AuthContext } from './auth-context';
import { RequireAuth } from './require-auth';
import type { AuthContextValue, AuthStatus } from './types';

function contextFor(status: AuthStatus): AuthContextValue {
  return {
    status,
    user: null,
    reload: vi.fn(async () => {}),
    signOut: vi.fn(async () => {}),
  };
}

function renderGate(status: AuthStatus, initialPath = '/runs') {
  return render(
    <AuthContext.Provider value={contextFor(status)}>
      <MemoryRouter initialEntries={[initialPath]}>
        <Routes>
          <Route path="/login" element={<div>login page</div>} />
          <Route path="/access-denied" element={<div>access denied page</div>} />
          <Route
            path="*"
            element={
              <RequireAuth>
                <div>protected content</div>
              </RequireAuth>
            }
          />
        </Routes>
      </MemoryRouter>
    </AuthContext.Provider>,
  );
}

describe('RequireAuth', () => {
  it('renders children when authenticated', () => {
    renderGate('authenticated');
    expect(screen.getByText('protected content')).toBeInTheDocument();
  });

  it('shows the session placeholder while loading', () => {
    renderGate('loading');
    expect(screen.getByRole('status')).toHaveTextContent(/checking session/i);
    expect(screen.queryByText('protected content')).not.toBeInTheDocument();
  });

  // E44.3 #1827: denied is terminal for this account — redirect to the
  // public /access-denied page, not /login (which would loop).
  it('redirects denied sessions to /access-denied', () => {
    renderGate('denied');
    expect(screen.getByText('access denied page')).toBeInTheDocument();
    expect(screen.queryByText('protected content')).not.toBeInTheDocument();
  });

  it('does not attach a ?next= intent on the denied redirect', () => {
    // Unlike the /login redirect there is nowhere to resume — the
    // denial applies to every protected route equally.
    renderGate('denied', '/runs/abc-123?tab=stages');
    expect(screen.getByText('access denied page')).toBeInTheDocument();
  });

  it('redirects unauthenticated visitors to /login', () => {
    renderGate('unauthenticated');
    expect(screen.getByText('login page')).toBeInTheDocument();
    expect(screen.queryByText('protected content')).not.toBeInTheDocument();
  });
});
