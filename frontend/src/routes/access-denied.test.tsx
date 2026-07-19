import { describe, expect, it, vi } from 'vitest';
import { fireEvent, render, screen } from '@testing-library/react';
import { MemoryRouter, Route, Routes } from 'react-router';
import { AccessDenied } from './access-denied';
import { AuthContext } from '@/auth/auth-context';
import type { AuthContextValue } from '@/auth/types';

function renderPage(signOut = vi.fn(async () => {})) {
  const value: AuthContextValue = {
    status: 'denied',
    user: null,
    reload: vi.fn(async () => {}),
    signOut,
  };
  render(
    <AuthContext.Provider value={value}>
      <MemoryRouter initialEntries={['/access-denied']}>
        <Routes>
          <Route path="/access-denied" element={<AccessDenied />} />
          <Route path="/login" element={<div>login page</div>} />
        </Routes>
      </MemoryRouter>
    </AuthContext.Provider>,
  );
  return signOut;
}

describe('AccessDenied', () => {
  it('explains the denial and names the remedy', () => {
    renderPage();
    expect(screen.getByRole('heading', { name: /access denied/i })).toBeInTheDocument();
    expect(screen.getByText(/isn't a member of any workspace/i)).toBeInTheDocument();
    expect(screen.getByText(/ask a workspace admin to invite you/i)).toBeInTheDocument();
  });

  it('signs out and routes to /login', async () => {
    const signOut = renderPage();
    fireEvent.click(screen.getByRole('button', { name: /sign out/i }));
    expect(await screen.findByText('login page')).toBeInTheDocument();
    expect(signOut).toHaveBeenCalledTimes(1);
  });
});
