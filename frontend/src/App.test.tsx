import { describe, expect, it } from 'vitest';
import { render, screen } from '@testing-library/react';
import { MemoryRouter } from 'react-router';
import { App } from './App';

function renderAt(path: string) {
  return render(
    <MemoryRouter initialEntries={[path]}>
      <App />
    </MemoryRouter>,
  );
}

describe('App routing', () => {
  it('renders the runs route as the index', () => {
    renderAt('/');
    expect(screen.getByRole('heading', { name: 'Runs' })).toBeInTheDocument();
  });

  it('renders the audit route', () => {
    renderAt('/audit');
    expect(screen.getByRole('heading', { name: 'Audit log' })).toBeInTheDocument();
  });

  it('renders the login route outside the app shell', () => {
    renderAt('/login');
    expect(screen.getByRole('button', { name: /continue with github/i })).toBeInTheDocument();
    // The shell nav shouldn't be present on /login.
    expect(screen.queryByRole('link', { name: /runs/i })).not.toBeInTheDocument();
  });

  it('renders a not-found page for unknown routes', () => {
    renderAt('/does-not-exist');
    expect(screen.getByRole('heading', { name: /not found/i })).toBeInTheDocument();
  });
});
