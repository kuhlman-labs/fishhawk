# Fishhawk Web UI

The browser surface for plan review, approval, audit log search, and
run visualization. Vite + React 19 + React Router 7 + TypeScript,
styled with Tailwind CSS v4 + shadcn/ui (per
[ADR-004](https://github.com/kuhlman-labs/fishhawk/issues/68)).

This directory is its own pnpm package, decoupled from the Go modules
above it. CI's TS lane is path-filtered to `frontend/**` so backend-only
changes don't pay the install/test cost.

## Layout

- `src/main.tsx` — entry; mounts `<App />` inside `<BrowserRouter>`.
- `src/App.tsx` — route table.
- `src/routes/` — one file per route (`root` is the app shell;
  `login` is rendered outside the shell; `runs` and `audit` are
  child routes of `root`; `not-found` catches the rest).
- `src/components/ui/` — shadcn-copied primitives (currently just
  `Button`; add more on demand, never as a library dep).
- `src/lib/cn.ts` — `clsx + tailwind-merge` class helper.
- `src/index.css` — Tailwind v4 entry + `@theme` token overrides.

## Develop

Requires Node 22+ and pnpm 10+.

```sh
pnpm install
pnpm dev          # http://localhost:5173 → proxies /v0 to localhost:8080
pnpm typecheck
pnpm lint
pnpm test         # vitest, jsdom
pnpm build        # tsc -b + vite build → dist/
```

For backend talkback, run `fishhawkd serve` in another terminal; the
Vite dev server proxies `/v0/*` to `http://localhost:8080` so the
session cookie set by `/v0/auth/github/callback` is same-origin from
the browser's perspective. Override the proxy target by editing
`vite.config.ts` if the backend runs elsewhere.

## What's stubbed

E7.1 (this issue) is scaffolding only. Real surfaces ship in:

- **E7.2 (#38)** — wire the Login button to `/v0/auth/github/login`,
  read the `fishhawk_session` cookie, expose an auth context.
- **E7.3 (#56)** — render `standard_v1` plans as documents.
- **E7.4 (#57)** — approval action against `POST /v0/stages/{id}/approvals`.

## See also

- `docs/MVP_SPEC.md` §5.1.3 (Web UI scope)
- `docs/BRAND_FOUNDATIONS.md` §6 (UI principles: density, restraint,
  audit log as a first-class surface)
- `docs/api/v0.openapi.yaml` (the REST contract this UI consumes)
