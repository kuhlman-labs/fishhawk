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
  `login` is rendered outside the shell; `runs` lists workflow runs,
  `run-detail` drills into one, `stage-detail` renders the plan;
  `audit` is still a stub; `not-found` catches the rest).
- `src/auth/` — auth context, provider, `RequireAuth` gate, hook.
  The provider fetches `/v0/auth/me`; routes inside `<Root />` are
  gated behind it.
- `src/api/` — typed wrappers around the v0 REST surface
  (`client.ts`), TS mirrors of the OpenAPI schemas (`types.ts`,
  `plan.ts`), and a small `useAsync` hook for component-level
  data loading.
- `src/plan/` — the plan-document renderer (`plan-document.tsx`)
  and its section primitives (`sections.tsx`). Each `standard_v1`
  field is its own section so the side nav anchors line up
  one-to-one.
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

The plan-review vertical slice (E7.1 → E7.2 → E7.3) is in. Still
to come:

- **Audit search** under `/audit` is still a placeholder. The
  per-run audit list at `/runs/:id#audit` is wired (E7.4) but the
  global search across runs is later in E7.
- **Pagination on `/runs`** and per-run audit lists
  ([#155](https://github.com/kuhlman-labs/fishhawk/issues/155)) — both call with `limit=50`; cursor controls land
  separately.
- **Regenerate** in the plan-review header
  ([#146](https://github.com/kuhlman-labs/fishhawk/issues/146)) — renders disabled until E8.3 wires re-execution.

## See also

- `docs/MVP_SPEC.md` §5.1.3 (Web UI scope)
- `docs/BRAND_FOUNDATIONS.md` §6 (UI principles: density, restraint,
  audit log as a first-class surface)
- `docs/api/v0.openapi.yaml` (the REST contract this UI consumes)
