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
}

export type AuthStatus = 'loading' | 'authenticated' | 'unauthenticated';

export interface AuthContextValue {
  status: AuthStatus;
  user: User | null;
  reload: () => Promise<void>;
  signOut: () => Promise<void>;
}
