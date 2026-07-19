import { Navigate, useLocation } from 'react-router';
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
 *
 * On unauthenticated, the current path+search is forwarded to
 * /login as ?next= so the post-sign-in handler can route the user
 * back to where they were trying to go (E7.2.1 #153). The hash is
 * deliberately dropped — the OAuth callback's 302 response strips
 * fragments anyway.
 */
export function RequireAuth({ children }: Props) {
  const { status } = useAuth();
  const location = useLocation();

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

  if (status === 'denied') {
    // Signed in but no workspace account resolves for the session
    // (E44.3 #1827). /login would loop; /access-denied explains the
    // situation and offers sign-out. No ?next= — there is nowhere to
    // resume until an admin grants membership.
    return <Navigate to="/access-denied" replace />;
  }

  if (status === 'unauthenticated') {
    const intent = location.pathname + location.search;
    const target =
      intent && intent !== '/' ? `/login?next=${encodeURIComponent(intent)}` : '/login';
    return <Navigate to={target} replace />;
  }

  return <>{children}</>;
}
