import { useCallback, useEffect, useMemo, useState, type ReactNode } from 'react';
import { AuthContext } from './auth-context';
import type { AuthStatus, User } from './types';

interface State {
  status: AuthStatus;
  user: User | null;
}

const initial: State = { status: 'loading', user: null };

/*
 * Owns the conversation with /v0/auth/me. Same-origin fetch (Vite
 * proxies /v0 → fishhawkd in dev; in prod the SPA is served by the
 * same backend) means the fishhawk_session cookie rides along
 * automatically with credentials: 'include'. ADR-005.
 *
 * No retry / backoff: if /v0/auth/me errors out (offline, backend
 * down) we fall through to unauthenticated rather than spinning
 * forever. The user is then funnelled to /login, which on a clicked
 * sign-in restarts the flow against a presumably-recovered backend.
 */
export function AuthProvider({ children }: { children: ReactNode }) {
  const [state, setState] = useState<State>(initial);

  const reload = useCallback(async () => {
    setState((s) => ({ ...s, status: 'loading' }));
    try {
      const res = await fetch('/v0/auth/me', { credentials: 'include' });
      if (res.ok) {
        const user = (await res.json()) as User;
        setState({ status: 'authenticated', user });
        return;
      }
      setState({ status: 'unauthenticated', user: null });
    } catch {
      setState({ status: 'unauthenticated', user: null });
    }
  }, []);

  const signOut = useCallback(async () => {
    try {
      await fetch('/v0/auth/logout', { method: 'POST', credentials: 'include' });
    } catch {
      // The server may already consider us signed out (network
      // partition, expired session). Either way, drop local state
      // so the UI matches the user's intent.
    }
    setState({ status: 'unauthenticated', user: null });
  }, []);

  useEffect(() => {
    void reload();
  }, [reload]);

  const value = useMemo(() => ({ ...state, reload, signOut }), [state, reload, signOut]);

  return <AuthContext.Provider value={value}>{children}</AuthContext.Provider>;
}
