/*
 * Mirrors the OpenAPI `User` schema (docs/api/v0.openapi.yaml).
 * Update both sides together — the schema-sync CI doesn't cover the
 * frontend type, so drift is silent here.
 */
export interface User {
  id: string;
  github_login: string;
  name: string;
  email: string | null;
  account_id: string | null;
}

/*
 * 'denied' is distinct from 'unauthenticated': the session cookie is
 * valid but the backend can't resolve a workspace account for it
 * (/v0/auth/me 403 account_unresolved, E44.3 #1827). Re-running the
 * login flow won't help, so RequireAuth routes it to /access-denied
 * instead of /login.
 */
export type AuthStatus = 'loading' | 'authenticated' | 'unauthenticated' | 'denied';

export interface AuthContextValue {
  status: AuthStatus;
  user: User | null;
  reload: () => Promise<void>;
  signOut: () => Promise<void>;
}
