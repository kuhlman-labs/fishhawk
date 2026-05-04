# fishhawkd

The Go service that orchestrates workflow runs in Fishhawk. Owns the
workflow run / stage state machine, the policy evaluator, approval state,
the audit log writer, the GitHub App webhook receiver, and the REST API
consumed by the CLI and the Web UI.

This directory is its own Go module. It is tied into the repo via
`go.work` at the root so it can be tagged and released independently of
the runner action and the CLI. See
[ADR-014](https://github.com/kuhlman-labs/fishhawk/issues/78) for the
multi-module rationale.

## Layout

- `cmd/fishhawkd/` — the binary entrypoint with `serve` and `migrate` subcommands.
- `internal/postgres/` — pgx pool wrapper and embedded `golang-migrate` runner. Migrations live under `internal/postgres/migrations/`.
- `internal/run/` — workflow run / stage state machine. Domain types in `run.go`, transition tables in `transition.go`, `Repository` interface in `repository.go`, Postgres adapter in `postgres.go`. sqlc-generated code under `internal/run/db/`.
- `internal/server/` — HTTP server, middleware, handlers.
- `internal/version/` — build version exposed via `-ldflags`.

## Build and test

From the repo root (workspace-aware):

    go build ./backend/...
    go test ./backend/...
    golangci-lint run ./backend/...

Integration tests under `internal/run/postgres_test.go` require Docker (testcontainers spins up Postgres 16). Devs without Docker get a `t.Skip`.

To regenerate sqlc code after editing `internal/run/queries.sql`:

    cd backend && sqlc generate

## Status

- **E3.1 (#41)** — module scaffold.
- **E3.2 (#42)** — HTTP server, middleware, `/healthz`. Middleware order: `recovery → requestID → logging → authStub → mux`.
- **E3.3 (#43)** — run/stage state machine on Postgres. Transitions are validated against an explicit table; persistence uses `SELECT … FOR UPDATE` inside a transaction so concurrent transitions can't both succeed. `fishhawkd migrate up|down` applies the embedded migrations.

Upcoming under epic E3 (#3):

- E3.4 (#44) — policy evaluator.
- E3.5 (#45) — approval state + SLA tracking.
- E3.6 (#46) — REST API surface for CLI + UI.
- E3.7 (#47) — GitHub App webhook receiver wiring.

## Run

Bring up Postgres locally:

    docker compose up -d postgres

Apply migrations and start the server:

    export FISHHAWKD_DATABASE_URL='postgres://fishhawk:fishhawk@localhost:5432/fishhawk?sslmode=disable'
    go run ./backend/cmd/fishhawkd migrate up
    go run ./backend/cmd/fishhawkd serve

    curl http://localhost:8080/healthz

Override the listen address with `--addr` or `FISHHAWKD_ADDR`.

Optional flags:

- `--enable-sla-timer` (or `FISHHAWKD_ENABLE_SLA_TIMER=true`) — start the background goroutine that times out `awaiting_approval` stages past their gate SLA, transitioning them to failed with category D. Off by default so dev runs aren't racing the timer.
- `--sla-interval` — scan interval; defaults to `60s`. Hour-grained SLAs need no finer cadence.
- `--oidc-audience` (or `FISHHAWKD_OIDC_AUDIENCE`) — turn on GitHub Actions OIDC verification on the signing-key endpoint. Callers must present a `Bearer` token whose `aud` claim matches this value, and whose `repository` + `workflow` claims bind to the path's run. Unset = endpoint accepts any caller (v0 self-execution posture; not safe for production).
- `--oidc-jwks-url` — override the JWKS endpoint. Defaults to GitHub's published URL; useful for testing.
- `--oauth-client-id` / `--oauth-client-secret` / `--oauth-callback-url` (or `FISHHAWKD_OAUTH_CLIENT_ID` / `_CLIENT_SECRET` / `_CALLBACK_URL`) — enable the GitHub OAuth sign-in flow at `/v0/auth/github/*`. All three must be set; mismatched configuration fails fast. The callback URL is the public URL of `/v0/auth/github/callback` (the value the OAuth App is registered with).
- `--oauth-redirect-after-login` (default `/`) — relative path the callback handler redirects to on successful sign-in. Absolute URLs and scheme-relative paths are rejected.

### Bootstrapping API tokens

`/v0/tokens` requires an authenticated identity to mint a new token (a chicken-and-egg). For the first token, use the CLI:

```sh
fishhawkd token issue --subject github:42 --scopes runs:read,runs:write
```

The plaintext is printed to stdout exactly once (suitable for `... | head -n1`); only the sha256 hash is stored. Subsequent tokens can be minted via `POST /v0/tokens` once you have one bearer in hand.

Larger context: `docs/MVP_SPEC.md` §5.1.1 (component) and §5.2 (execution flow); `docs/ARCHITECTURE.md` §4–§6 for the workflow lifecycle, storage model, and invariants.
