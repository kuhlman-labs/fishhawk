import { defineConfig } from 'vitest/config';
import react from '@vitejs/plugin-react';
import tailwindcss from '@tailwindcss/vite';
import path from 'node:path';
import { resolveFsDeny } from './vite-fs-deny.ts';

// Backend talks on :8080 by default (see backend/cmd/fishhawkd serve).
// Proxying /v0 here means the dev server can carry the session cookie
// without CORS gymnastics — same-origin from the browser's perspective.
export default defineConfig(async () => ({
  plugins: [react(), tailwindcss()],
  resolve: {
    alias: {
      '@': path.resolve(import.meta.dirname, './src'),
    },
  },
  server: {
    // Drop Vite's `**/.git/**` deny default ONLY when this checkout lives
    // under a `.git/` path segment (a Fishhawk run worktree), so the frontend
    // toolchain runs from there. `undefined` in a normal checkout keeps Vite's
    // stock posture. See ./vite-fs-deny.ts and #3030.
    fs: { deny: await resolveFsDeny(import.meta.dirname) },
    port: 5173,
    proxy: {
      '/v0': {
        target: 'http://localhost:8080',
        changeOrigin: false,
      },
    },
  },
  test: {
    environment: 'jsdom',
    // jsdom's default URL is http://localhost/ — tough-cookie rejects
    // __Host-prefixed cookies there because Secure isn't really
    // enforceable. Browsers treat localhost as a secure context (per
    // Secure Contexts spec) so __Host- works in dev; jsdom doesn't.
    // Use https://localhost/ for tests so cookie semantics match prod.
    environmentOptions: { jsdom: { url: 'https://localhost/' } },
    globals: true,
    setupFiles: ['./src/test-setup.ts'],
    css: true,
  },
}));
