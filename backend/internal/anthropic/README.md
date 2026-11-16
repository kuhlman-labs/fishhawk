# backend/internal/anthropic

Anthropic SDK adapter. `Client.Messages` is the one Messages API call; `Reviewer`
wraps it as `server.PlanReviewer`. Production constructs it in
`backend/cmd/fishhawkd/serve.go`; the operator-run `backend/internal/agenteval`
live arms construct it too, and are the only consumer of `Config.AuthToken`.

## SDK and wire compatibility

The client uses Anthropic Go SDK v2.9.0. Both Messages and the model-listing
fetcher explicitly send `anthropic-version: 2023-06-01`, preserving the
established API contract for regional Anthropic-compatible endpoints. This
pin is independent of the SDK’s default API version; request-capture tests
check it while the existing credential, output-schema and token-accounting
fixtures exercise the SDK adapter.

## Credential contract (E83.96 / #4223)

`NewClient` takes exactly one credential, or none. Its signature is unchanged, so
no caller changed.

| `APIKey` | `AuthToken` | On the wire | SDK environment autoloader |
|---|---|---|---|
| set | empty | `X-Api-Key: <APIKey>`, no `Authorization` header | **off** (SDK v2 explicit-credential selection) |
| empty | set | `Authorization: Bearer <AuthToken>`, no `X-Api-Key` header at all | **off** |
| empty | empty | no credential (an empty `X-Api-Key:` header) — the #2108 withhold posture | **off** |
| set | set | nothing: `NewClient` returns a `Client` carrying `ErrConflictingCredentials`; `Messages` returns it before any network I/O | n/a (no SDK client is built) |

- The `AuthToken` row never applies the API-key defaults. On the API-key path,
  the adapter sets `X-Api-Key` explicitly and removes `Authorization`: SDK v2
  would otherwise serialize even `WithAPIKey("")` as a bare bearer header.
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
not presence: the adapter leaves an empty `X-Api-Key:` header, as before).

**Historical counterfactual.** Under SDK v1, the `ANTHROPIC_AUTH_TOKEN only`
subcase went red when `WithoutEnvironmentDefaults()` was removed, while the
both-sources case was masked by API-key precedence. That measurement does not
transfer to SDK v2: explicit credential selection and header cleanup now add
independent protection. The tests continue to assert the observable wire
contract for every credential combination.

The former SDK-v1 API-key-path residual (an ambient bearer riding beside an
explicit API key) is removed by SDK v2's explicit credential selection and the
adapter's authorization-header cleanup.

## BaseURL precedence

`Config.BaseURL` is applied as a DEFAULT, before caller-supplied `opts`, so a
region-scoped endpoint governs production while a test's explicit
`option.WithBaseURL` still wins (ADR-062). It is independent of the credential
rows above.

## User-turn prompt caching (Config.CacheUserTurn, #4563)

`Messages` always puts an ephemeral `cache_control` breakpoint on the system
block (when `systemText` is non-empty). `Config.CacheUserTurn` additionally puts
one on the single user text block: at most 2 of the API's 4 breakpoints (1 when
`systemText` is empty), the default 5-minute TTL, no `ttl: 1h`.

- **Default off.** Production reviews are rarely repeated within the TTL, so a
  user-turn write would mostly be a 25% cache-write surcharge. The off path's
  request bytes are unchanged, pinned by
  `TestClient_Messages_CacheUserTurnOffLeavesUserUnmarked` (the `messages` array
  is JSON-equal to the pre-change shape) and `TestReviewer_LeavesUserTurnUncached`
  (the `NewReviewer` path `serve.go` uses).
- **When to enable it.** A caller that resends a byte-identical user prompt
  within the TTL. Today that is only agenteval's live arms, through
  `liveCredential.config` in `backend/internal/agenteval/livecredential_test.go`,
  which forces `CacheUserTurn` on whatever the caller's `Config` literal says.
  That seam serves every live eval client, the rubric judges included, so a
  per-judge opt-out would need a new seam parameter, not a caller-literal
  `false`.
- **Outputs.** Caching never changes model outputs, so a catch-rate measurement
  is unaffected.
- **Minimum length.** A prompt below the model's minimum cacheable length is
  simply not cached; the API returns no error, so the saving shrinks and nothing
  else changes.
- **Checking it worked.** The `cacheReadTokens` `Messages` returns should be
  non-zero on samples 2..N of a repeated prompt.

## Tests

`client_test.go` isolates the ambient environment with `isolateAmbientAnthropicEnv`
(unsets the autoloader's sources and points `ANTHROPIC_CONFIG_DIR` at an empty
directory so the host's real profile cannot load). Tests that use it call
`t.Setenv`, so none may call `t.Parallel`.

The `CacheUserTurn` tests (`TestClient_Messages_CacheUserTurn*`,
`TestReviewer_LeavesUserTurnUncached`) assert on the RAW request JSON captured
by an `httptest` server, never on SDK param structs, and are parallel-safe (no
`t.Setenv`).
