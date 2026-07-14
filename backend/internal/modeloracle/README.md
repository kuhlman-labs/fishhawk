# backend/internal/modeloracle

Model-id validity layer (#1339 validation + #1341 live snapshot): validates workflow `executor.model` / `reviewers.agents[].model` against a live model snapshot.

## The seam

`ModelOracle.Snapshot(ctx, provider) (models, fresh, ok)` — the provider-agnostic snapshot contract.

**`Cached` (constructor `NewCached(providers, threshold, logger)`) is the wired production impl (#1341):** a per-process, in-memory cache of each provider's `/v1/models`, refreshed by a background goroutine (`Run(ctx, interval)` — initial best-effort fetch + ticker, stops on the server signal context).

- A snapshot is `fresh` iff its last **successful** fetch is younger than the staleness threshold (`FISHHAWKD_MODELS_STALENESS_THRESHOLD`, default 24h); `ok=false` until the first success, and a failed refresh keeps the prior models while decaying freshness (bumps `lastAttempt`, not `lastSuccess`).
- Providers are keyed under the EXISTING `claudecode`/`codex` strings (the same keys the allow-list and `providerForExecutorAgent` use), each fetching its vendor internally: `claudecode` → `AnthropicFetcher` (anthropic-sdk-go `Models.ListAutoPaging`), `codex` → `OpenAIFetcher` (raw `net/http` GET `/v1/models`, `Authorization: Bearer`).
- A provider whose API key is absent is left UNREGISTERED → `Snapshot` `ok=false` → **fail open** (never a boot blocker).
- `NoData` (`NewNoData()`, universal `ok=false`) and `Static` (map-backed, `Fresh` flag) remain for tests/fixtures.

With the live oracle wired, **#1341 activates #1339's validation** — a fresh snapshot now rejects a typo'd model in production; #1339 closes once this lands + reloads.

## Validation logic

`backend/internal/spec/modelvalidate.go::ValidateModels(s, oracle) (warnings, err)` walks every workflow stage's two model fields, derives the provider (`providerForExecutorAgent` for the executor, the explicit `Provider` for each reviewer), and routes severity:

- nil oracle / `ok==false` / `fresh==false` → **fail open** with a `model_unverifiable` `Warning` (the package's one advisory channel, the rest is hard-errors-only).
- `fresh && ok && present` → accept.
- `fresh && ok && absent` → hard `*ValidationError` with a `levenshtein`-based did-you-mean + the available set.

The minimal contract carries **no deprecation channel** (neither Anthropic `ModelInfo` nor OpenAI `/v1/models` exposes one), so a deprecated/sunset model and a typo both manifest as absence-from-fresh and BOTH reject.

## Wiring

- Submit-time in `runs.go::handleCreateRun` (after `spec.ParseBytes` on both the inline + GitHub-fetch paths — a hard error is a 422 `model_invalid` inserting no run row; warnings are **logged only** this slice, no HTTP response field / OpenAPI change).
- Gate backstop `server/modelvalidity.go::checkModelValidityGate`, invoked in `approvals.go` (pre-Submit, before `checkPlanModelAllowed`) and `fixup.go` (before `checkFixupModelAllowed`) — the **validity → policy → pricing** layering, with `modelpolicy.go`'s allow-list (`IsAllowed`) untouched.
- Config: `server.Config.ModelOracle` (nil == fail-open everywhere), wired `modeloracle.NewCached(buildModelProviders(anthropicKey, openaiKey), staleness, logger)` in `serve.go` with the refresh goroutine started on the signal context. Knobs: `FISHHAWKD_MODELS_REFRESH_INTERVAL` (12h) / `FISHHAWKD_MODELS_STALENESS_THRESHOLD` (24h).
- A future Postgres-backed multi-replica impl can replace `Cached` behind the same seam with no validation-code change.
