---
name: frontend-check
description: Run the Fishhawk Web UI (frontend/) checks that CI enforces — prettier format check, eslint, TypeScript typecheck, vitest, and the production build — with the locked toolchain. Use before pushing any change under frontend/, or when asked to lint/test/format the UI.
---

# Check the Web UI before pushing

CI runs `pnpm format:check` as its own gate. `lint` and `typecheck` do **not** catch formatting, so a change that passes those can still fail CI on prettier.

## 1. Use the locked dependencies

```sh
cd frontend && pnpm install --frozen-lockfile
```

A stale local `node_modules` can carry a different prettier/eslint version than the lockfile. For example, local prettier 3.8 vs locked 3.9 produces opposite formatting verdicts. Reinstall before trusting, or overruling, a format result.

## 2. Run the checks (in `frontend/`)

| Check | Command |
|---|---|
| Format (CI gate) | `pnpm format:check`, and fix with `pnpm format` |
| Lint | `pnpm lint` |
| Types | `pnpm typecheck` |
| Tests | `pnpm test` (vitest run, jsdom at `https://localhost/` so `__Host-` cookies behave) |
| Build | `pnpm build` |

Run them in that order; format first is cheapest. After `pnpm format`, re-run `git diff --stat` so the formatting changes are committed together with the real ones.

## 3. Known traps

- **Inside a run worktree** (`.git/fishhawk-worktrees/run-*`), vitest can fail with a misleading `Cannot find module '/src/test-setup.ts'`. That's Vite's `**/.git/**` fs-deny rule, handled by `frontend/vite-fs-deny.ts`. Don't "fix" it by making `setupFiles` absolute.
- **Dev server:** `make dev-frontend` (`:5173`, proxies `/v0` to fishhawkd on `:8080`). It needs the stack up; see the `deploy-local` skill.

## Report

Pass/fail per check, with the first error verbatim, plus any files `pnpm format` rewrote.
