import { describe, expect, it, vi } from 'vitest';
import { render, screen } from '@testing-library/react';
import { MemoryRouter, Route, Routes } from 'react-router';
import { Login } from './login';
import { AuthContext } from '@/auth/auth-context';
import type { AuthContextValue, AuthStatus } from '@/auth/types';

function renderPage(status: AuthStatus = 'unauthenticated', entry = '/login') {
  const value: AuthContextValue = {
    status,
    user: null,
    reload: vi.fn(async () => {}),
    signOut: vi.fn(async () => {}),
  };
  render(
    <AuthContext.Provider value={value}>
      <MemoryRouter initialEntries={[entry]}>
        <Routes>
          <Route path="/login" element={<Login />} />
          <Route path="/" element={<div>home</div>} />
          <Route path="/runs" element={<div>runs page</div>} />
        </Routes>
      </MemoryRouter>
    </AuthContext.Provider>,
  );
}

describe('Login', () => {
  it('points the sign-in link at the backend OAuth start endpoint', () => {
    renderPage();
    expect(screen.getByRole('link', { name: /continue with github/i })).toHaveAttribute(
      'href',
      '/v0/auth/github/login',
    );
  });

  it('forwards ?next= so the backend can route the user back after callback', () => {
    renderPage('unauthenticated', '/login?next=%2Fruns');
    expect(screen.getByRole('link', { name: /continue with github/i })).toHaveAttribute(
      'href',
      '/v0/auth/github/login?next=%2Fruns',
    );
  });

  it('renders the GitHub mark as decoration, leaving the label to carry the name', () => {
    // Regression guard for #2203: lucide-react v1 removed brand icons, so the
    // mark is now a local component. The assertion is on the rendered SVG
    // rather than the import so it keeps holding if the source moves again.
    // aria-hidden matters — without it the icon competes with the link text
    // for the accessible name.
    const { container } = render(
      <AuthContext.Provider
        value={{
          status: 'unauthenticated',
          user: null,
          reload: vi.fn(async () => {}),
          signOut: vi.fn(async () => {}),
        }}
      >
        <MemoryRouter initialEntries={['/login']}>
          <Routes>
            <Route path="/login" element={<Login />} />
          </Routes>
        </MemoryRouter>
      </AuthContext.Provider>,
    );
    const svg = container.querySelector('svg');
    expect(svg).not.toBeNull();
    expect(svg).toHaveAttribute('aria-hidden');
    expect(svg?.querySelector('path')).not.toBeNull();
    expect(screen.getByRole('link', { name: 'Continue with GitHub' })).toBeInTheDocument();
  });

  it('sends an already-authenticated visitor home instead of showing the form', () => {
    renderPage('authenticated');
    expect(screen.getByText('home')).toBeInTheDocument();
    expect(screen.queryByRole('link', { name: /continue with github/i })).not.toBeInTheDocument();
  });
});
