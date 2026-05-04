import { Navigate } from 'react-router';
import type { ReactNode } from 'react';
import { useAuth } from './use-auth';

interface Props {
  children: ReactNode;
}

/*
 * Gate around the app shell. While auth is loading we render a
 * minimal placeholder rather than nothing, so refresh on a deep
 * link doesn't cause the shell to flash empty before the redirect
 * decision is made.
 */
export function RequireAuth({ children }: Props) {
  const { status } = useAuth();

  if (status === 'loading') {
    return (
      <div
        role="status"
        aria-live="polite"
        className="flex min-h-full items-center justify-center text-sm text-neutral-500"
      >
        Checking session…
      </div>
    );
  }

  if (status === 'unauthenticated') {
    return <Navigate to="/login" replace />;
  }

  return <>{children}</>;
}
