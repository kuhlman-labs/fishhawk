# backend/internal/modeloracle

Model-id validity layer (#1339 validation + #1341 live snapshot): validates workflow `executor.model` / `reviewers.agents[].model` against a live model snapshot.

## The seam

`ModelOracle.Snapshot(ctx, provider) (models, fresh, ok)` — the provider-agnostic snapshot contract.

**`Cached` (constructor `NewCached(providers, threshold, logger)`) is the wired production impl (#1341):** a per-process, in-memory cache of each provider's `/v1/models`, refreshed by a background goroutine (`Run(ctx, interval)` — initial best-effort fetch + ticker, stops on the server signal context).

- A snapshot is `fresh` iff its last **successful** fetch is younger than the staleness threshold (`FISHHAWKD_MODELS_STALENESS_THRESHOLD`, default 24h); `ok=false` until the first success, and a failed refresh keeps the prior models while decaying freshness (bumps `lastAttempt`, not `lastSuccess`).
- Providers are keyed under the EXISTING `claudecode`/`codex` strings (the same keys the allow-list and `providerForExecutorAgent` use), each fetching its vendor internally: `claudecode` → `AnthropicFetcher` (anthropic-sdk-go `Models.ListAutoPaging`), `codex` → `OpenAIFetcher` (raw `net/http` GET `/v1/models`, `Authorization: Bearer`).
- A provider whose API key is absent is left UNREGISTERED → `Snapshot` `ok=false` → **fail open** (never a boot blocker).
- **Alias (`WithAlias(alias, canonical)`, #3578):** `Snapshot(alias)` resolves to the CANONICAL provider's snapshot before the lookup, adding NO fetch (Refresh still fetches once per registered Fetcher). `serve.go::newModelOracle` aliases `anthropic`→`claudecode` in the single-cell posture (both hit the same Anthropic endpoint), so an `anthropic` reviewer is validated too. A region-scoped cell (`FISHHAWKD_MODEL_BASE_URL` set) registers a SEPARATE `anthropic` fetcher against the reviewer's own endpoint instead of aliasing, so the snapshot represents what that reviewer will call.
- `NoData` (`NewNoData()`, universal `ok=false`) and `Static` (map-backed, `Fresh` flag) remain for tests/fixtures.

With the live oracle wired, **#1341 activates #1339's validation** — a fresh snapshot now rejects a typo'd model in production; #1339 closes once this lands + reloads.

## Validation logic

**`Verify(ctx, oracle, provider, model) Verdict` (verify.go, #3578) is the shared verdict** — `verified` / `rejected` / `unverifiable`, with the `levenshtein`-based did-you-mean and the sorted available set on a `Verdict`. `Verdict.RejectMessage()` renders the hard-error sentence (byte-identical to the pre-#3578 `spec.modelRejectMessage`), and `RejectedError{Verdict}` wraps a rejection so a cross-package caller recovers the RESOLVED model + suggestion via `errors.As`. Three consumers key on it so an id is judged the same everywhere:

- `backend/internal/spec/modelvalidate.go::ValidateModels(s, oracle)` — run-create validation, walking every workflow stage's two model fields (`providerForExecutorAgent` for the executor, the explicit `Provider` for each reviewer).
- `serve.go::planReviewerSet.For` — rejects a reviewer whose RESOLVED model (spec value, or the deployment default when the spec omits one) is authoritatively absent, so run-create degrade, review dispatch, and the doctor rung inherit it.
- `backend/internal/server/onboarding.go::probeReviewers` — the doctor reviewers rung's `model_status` / `model_hint` / `priced` honesty fields.

Severity routing (unchanged):

- nil oracle / `ok==false` / `fresh==false` → `unverifiable` → **fail open** with a `model_unverifiable` `Warning` (the package's one advisory channel, the rest is hard-errors-only).
- `fresh && ok && present` → `verified` → accept.
- `fresh && ok && absent` → `rejected` → hard `*ValidationError` with a did-you-mean + the available set.

The minimal contract carries **no deprecation channel** (neither Anthropic `ModelInfo` nor OpenAI `/v1/models` exposes one), so a deprecated/sunset model and a typo both manifest as absence-from-fresh and BOTH reject.

## Wiring

- Submit-time in `runs.go::handleCreateRun` (after `spec.ParseBytes` on both the inline + GitHub-fetch paths — a hard error is a 422 `model_invalid` inserting no run row; warnings are **logged only** this slice, no HTTP response field / OpenAPI change).
- Gate backstop `server/modelvalidity.go::checkModelValidityGate`, invoked in `approvals.go` (pre-Submit, before `checkPlanModelAllowed`) and `fixup.go` (before `checkFixupModelAllowed`) — the **validity → policy → pricing** layering, with `modelpolicy.go`'s allow-list (`IsAllowed`) untouched.
- Config: `server.Config.ModelOracle` (nil == fail-open everywhere), wired `newModelOracle(buildModelProviders(anthropicKey, openaiKey), modelBaseURL, modelAPIKey, staleness, logger)` in `serve.go` (the helper applies the `anthropic` alias / region fetcher above) with the refresh goroutine started on the signal context. The same oracle is threaded into `planReviewerOptions.modelOracle` for `For()`. Knobs: `FISHHAWKD_MODELS_REFRESH_INTERVAL` (12h) / `FISHHAWKD_MODELS_STALENESS_THRESHOLD` (24h).
- A future Postgres-backed multi-replica impl can replace `Cached` behind the same seam with no validation-code change.
