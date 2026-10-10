# backend/internal/anthropic

Anthropic SDK adapter. `Client.Messages` is the one Messages API call; `Reviewer`
wraps it as `server.PlanReviewer`. Production constructs it in
`backend/cmd/fishhawkd/serve.go`; the operator-run `backend/internal/agenteval`
live arms construct it too, and are the only consumer of `Config.AuthToken`.

## Credential contract (E83.96 / #4223)

`NewClient` takes exactly one credential, or none. Its signature is unchanged, so
no caller changed.

| `APIKey` | `AuthToken` | On the wire | SDK environment autoloader |
|---|---|---|---|
| set | empty | `X-Api-Key: <APIKey>` | **on** (unchanged) |
| empty | set | `Authorization: Bearer <AuthToken>`, no `X-Api-Key` header at all | **off** |
| empty | empty | no credential (an empty `X-Api-Key:` header) — the #2108 withhold posture | **off** |
| set | set | nothing: `NewClient` returns a `Client` carrying `ErrConflictingCredentials`; `Messages` returns it before any network I/O | n/a (no SDK client is built) |

- The `AuthToken` row never applies `option.WithAPIKey`. Even `WithAPIKey("")`
  puts a present-but-empty `X-Api-Key` header on the wire beside the bearer.
- The autoloader (`ANTHROPIC_API_KEY` → `ANTHROPIC_AUTH_TOKEN` → `ANTHROPIC_PROFILE`
  → env federation → fallback profile) is switched off with
  `option.WithoutEnvironmentDefaults()` whenever `APIKey` is empty, so a token is
  the only credential presented — an ambient `ANTHROPIC_API_KEY` cannot ride beside it.
- `NewClient` keeps its non-error signature: changing it would ripple into
  `serve.go` and every caller. The conflict error therefore rides on the `Client`,
  and `Reviewer.Review` wraps it with `%w`, so `errors.Is(err, ErrConflictingCredentials)`
  holds through `planreview.DecodeVerdictRetrying` (which returns an infer error verbatim).
- No production caller sets `AuthToken`. Which bearer tokens Anthropic's terms
  permit for direct API use is the operator's responsibility.

## The #2108 empty-key invariant

An empty `APIKey` means "present no credential from ANY source". The withhold
posture (`FISHHAWKD_MODEL_BASE_URL` set with no `FISHHAWKD_MODEL_API_KEY`) relies
on `WithoutEnvironmentDefaults()` to keep an operator shell's ambient Anthropic
credential from reaching the configured regional endpoint.

`TestNewClient_EmptyKeyNeutralizesAmbientCredentials` pins it, and asserts that
every observed request carries EMPTY `X-Api-Key` / `Authorization` values (values,
not presence: the explicit `WithAPIKey("")` leaves an empty `X-Api-Key:` header,
which is unchanged).

**Counterfactual caveat.** The "ANTHROPIC_AUTH_TOKEN only" subcase is the vehicle
that goes RED when the `WithoutEnvironmentDefaults()` append is deleted. The
"both ambient sources" subcase is MASKED and stays green: the autoloader takes
`ANTHROPIC_API_KEY` first and returns, and the explicit `WithAPIKey("")` then
overwrites it. Do not read that subcase as a control.

## Known residual (out of scope here)

On the **API-key path** the autoloader stays on, so an ambient
`ANTHROPIC_AUTH_TOKEN` in the process environment adds a second `Authorization:
Bearer` header beside `X-Api-Key`. That is pre-existing production behaviour and
is left unchanged; it is tracked separately.

## BaseURL precedence

`Config.BaseURL` is applied as a DEFAULT, before caller-supplied `opts`, so a
region-scoped endpoint governs production while a test's explicit
`option.WithBaseURL` still wins (ADR-062). It is independent of the credential
rows above.

## Tests

`client_test.go` isolates the ambient environment with `isolateAmbientAnthropicEnv`
(unsets the autoloader's sources and points `ANTHROPIC_CONFIG_DIR` at an empty
directory so the host's real profile cannot load). Tests that use it call
`t.Setenv`, so none may call `t.Parallel`.
