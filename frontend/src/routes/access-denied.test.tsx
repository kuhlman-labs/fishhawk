import { describe, expect, it, vi } from 'vitest';
import { fireEvent, render, screen } from '@testing-library/react';
import { MemoryRouter, Route, Routes } from 'react-router';
import { AccessDenied } from './access-denied';
import { AuthContext } from '@/auth/auth-context';
import type { AuthContextValue } from '@/auth/types';

function renderPage(query = '', signOut = vi.fn(async () => {})) {
  const value: AuthContextValue = {
    status: 'denied',
    user: null,
    reload: vi.fn(async () => {}),
    signOut,
  };
  render(
    <AuthContext.Provider value={value}>
      <MemoryRouter initialEntries={[`/access-denied${query}`]}>
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
  it('explains the denial and names the remedy with no reason parameter', () => {
    renderPage();
    expect(screen.getByRole('heading', { name: /access denied/i })).toBeInTheDocument();
    expect(screen.getByText(/isn't a member of any workspace/i)).toBeInTheDocument();
    expect(screen.getByText(/ask a workspace admin to invite you/i)).toBeInTheDocument();
    expect(screen.queryByText(/no membership resolver wired/i)).not.toBeInTheDocument();
  });

  it('names the no_admitting_account branch and its two remedies', () => {
    renderPage('?reason=no_admitting_account');
    expect(
      screen.getByText(/no workspace account on this deployment admits your login/i),
    ).toBeInTheDocument();
    expect(screen.getByText(/FISHHAWKD_SINGLE_TENANT_ACCOUNT_KEY/)).toBeInTheDocument();
    expect(screen.getByText(/ask an existing workspace admin to invite you/i)).toBeInTheDocument();
    expect(screen.queryByText(/no membership resolver wired/i)).not.toBeInTheDocument();
  });

  it('names the no_membership_resolver branch as a deployment fault', () => {
    renderPage('?reason=no_membership_resolver');
    expect(
      screen.getByText(/no membership resolver wired on this deployment/i),
    ).toBeInTheDocument();
    expect(screen.getByText(/deployment-configuration fault/i)).toBeInTheDocument();
    expect(
      screen.queryByText(/no workspace account on this deployment admits your login/i),
    ).not.toBeInTheDocument();
  });

  // The closed allow-list: an unrecognized code renders the generic body and
  // the raw parameter appears nowhere on the page. Deleting parseReason (so
  // the raw value indexes EXPLANATION) reddens this — the lookup yields
  // undefined and the render throws.
  it('falls back to the generic body for an unrecognized reason and never echoes it', () => {
    renderPage('?reason=totally-not-a-branch-code');
    expect(screen.getByText(/isn't a member of any workspace/i)).toBeInTheDocument();
    expect(screen.queryByText(/totally-not-a-branch-code/)).not.toBeInTheDocument();
    expect(document.body.textContent).not.toContain('totally-not-a-branch-code');
  });

  // The login is deliberately NOT rendered here: this page cannot verify it
  // (see the component comment). A crafted URL must not produce a
  // "you signed in as X" claim on the deployment's own denial page.
  it('never renders a login carried on the query', () => {
    renderPage('?reason=no_admitting_account&login=attacker-chosen&provider=github');
    expect(document.body.textContent).not.toContain('attacker-chosen');
    expect(document.body.textContent).not.toMatch(/you signed in/i);
  });

  it('signs out and routes to /login', async () => {
    const signOut = renderPage();
    fireEvent.click(screen.getByRole('button', { name: /sign out/i }));
    expect(await screen.findByText('login page')).toBeInTheDocument();
    expect(signOut).toHaveBeenCalledTimes(1);
  });
});
