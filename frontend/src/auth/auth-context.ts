import { createContext } from 'react';
import type { AuthContextValue } from './types';

/*
 * Lives in its own module so React Fast Refresh (eslint-plugin-
 * react-refresh) doesn't trip over a file mixing component and
 * non-component exports.
 */
export const AuthContext = createContext<AuthContextValue | null>(null);
