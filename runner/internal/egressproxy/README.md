# runner/internal/egressproxy

Acceptance-stage egress containment (E31.4 / #1532, ADR-050): a
default-deny filtering proxy plus the allow-listed invocation
environment that together bound what an acceptance agent can reach.

## Spec grammar (workflow-v1.3)

`egress.target_hosts` on an acceptance stage
(`docs/spec/workflow-v1.schema.json` `$defs/stage_egress`; Go type
`spec.StageEgress`). `backend/internal/spec/validate.go` rejects
`egress` on any non-acceptance stage — the ADR-050 binding. Entries are
host or host:port, schema-pattern-enforced (no scheme, path, or
wildcard); they are the ONLY customer-controlled slot of the
allow-list.

**Enforcement boundary (E53.5 / #2228).** This proxy enforces the
ACCEPTANCE stage's allow-list ONLY. workflow-v2 generalized the `egress`
grammar (and its `permissions.network` spelling) off the acceptance-only
binding, so a NON-acceptance stage may now DECLARE `egress` /
`permissions.network` — but that declaration is declaration-only,
surfaced and audited, and is **not routed to this proxy** (not enforced
until E51 #2133). `permissions.network` is normalized into `Stage.Egress`
at parse time, so an acceptance stage declaring either spelling reaches
`BuildAllowlist` through the same `resolveAcceptanceEgressTargetHosts`
seam and is enforced identically; a non-acceptance stage's declaration
never reaches this package.

## Egress proxy

- `Start(Config)` binds a default-deny filtering proxy on
  `127.0.0.1:0`.
- `BuildAllowlist(targetHosts, backendURL)` composes the three ADR-050
  destination classes: spec targets + `DefaultModelHosts` + the backend
  host.
- CONNECT tunnels admit/deny by host:port (the TLS payload stays
  opaque); absolute-form plain HTTP forwards likewise.
- Host-only entries admit default ports (80/443) only.
- **Anti-rebinding**: hostname resolutions are PINNED at first use for
  the proxy lifetime, and a public hostname resolving to
  loopback/private/link-local space is refused. IP-literal and
  localhost entries dial as declared (the dev-loop target).
- Denials are `403` naming the destination and the allow-list contract.
- **Verb-level deny on an admitted host (E72.13 / #3500).** Plain-HTTP
  forwards that pass the host match are additionally screened by
  `DeniedVerb` against the fixed `deniedVerbs` table BEFORE `RoundTrip`,
  so the upstream never sees a denied verb (CONNECT tunnels are opaque and
  unchanged). Paths are `path.Clean`'d and matched segment-wise, with
  `{id}` admitting any single non-empty segment (the ROUTE is denied, not
  an id format — a trailing slash or a non-UUID id cannot dodge a row).
  The table:

  | Method | Path | Condition |
  |---|---|---|
  | `POST` | `/v0/runs` | — |
  | `POST` | `/v0/runs/{id}/stages/{id}/host-dispatch` | — |
  | `POST` | `/v0/runs/{id}/auto-drive` | — |
  | `POST` | `/v0/campaigns` | — |
  | `POST` | `/v0/campaigns/{id}/runs` | — |
  | `POST` | `/v0/campaigns/{id}/resume` | — |
  | `POST` | `/v0/tokens` | — |
  | `POST` | `/v0/tokens/login` | — |
  | `POST` | `/mcp` | `Mcp-Name` header is one of `fishhawk_start_run`, `fishhawk_run_stage`, `fishhawk_dispatch_stage`, `fishhawk_run_children`, `fishhawk_drive_run`, `fishhawk_start_campaign`, `fishhawk_start_campaign_item_run`, `fishhawk_resume_campaign` |

  Denials reuse `deny()` → `403` naming the verb and the ADR-050
  contract. `TestForward_DeniesRunMintingVerbs` drives a real proxy in
  front of a hit-counting upstream: one case per row with zero upstream
  hits, plus forwarded positive controls (`GET /v0/runs`,
  `POST /v0/runs/{id}/trace`, `POST /mcp` with `Mcp-Name: fishhawk_get_plan`).

  **This layer is defence in depth, NOT the control.** Two bypasses are
  structural and stated here so nobody leans on it:

  1. **Loopback is never proxied by a Go client.** `net/http`'s
     `ProxyFromEnvironment` delegates to `golang.org/x/net/http/httpproxy`,
     whose `Config.ProxyFunc` documents that a `localhost` or loopback
     `req.URL.Host` returns a nil proxy URL regardless of `HTTP_PROXY` /
     `NO_PROXY`. So `bin/fishhawk`, `bin/fishhawk-mcp` or any Go binary
     dialing `localhost:8090` from the sandbox reaches the preview
     DIRECTLY and this table never sees the request. The table binds
     curl-shaped clients (curl honours `http_proxy` for localhost when
     `no_proxy` is empty, which `acceptenv` sets) and nothing more.
  2. **The `Mcp-Name` header is client-supplied.** The `/mcp` row keys on
     it because the JSON-RPC body is opaque to the proxy; a client that
     OMITS the header, or sends a read-only tool's name while the body
     calls `fishhawk_run_stage`, is forwarded. `TestDeniedVerb_Table`'s
     "mcp no header passes" / "mcp read tool passes" cases pin that
     behaviour as the documented residual, not as a guarantee.

  The AUTHORITATIVE control is server-side: a dev-mode `fishhawkd`
  (`FISHHAWKD_DEV_FIXTURES` / `FISHHAWKD_DEV_STUB_FORGE` — what
  `scripts/dev preview` runs) refuses its host-dispatch spawn marker for
  EVERY caller, identity-independent, with `403
  host_dispatch_refused_dev_mode` and a `host_dispatch_refused` audit row
  (`backend/internal/server/devmode.go`), and stamps `forge_writes:
  "deny"` onto every prompt response so a runner that is somehow spawned
  refuses pre-spawn anyway (`runner/README.md` § `FISHHAWK_FORGE_WRITES`).

## Invocation env (`runner/internal/acceptenv`)

`acceptenv.Env(base, proxyURL)` builds a default-deny allow-list of:

- system essentials;
- the model API keys (the one surviving secret class);
- operator-declared target creds via the
  `FISHHAWK_ACCEPTANCE_ENV_<NAME>` passthrough (prefix stripped; a
  passthrough colliding with a denied key or a proxy var is REFUSED and
  reported, never honored);
- `HTTP(S)_PROXY` / `ALL_PROXY` (both cases) pointed at the proxy, with
  `NO_PROXY` cleared;
- `FISHHAWK_FORGE_WRITES=deny` (E72.13 / #3500), a FIXED injection never
  copied from the base env: every descendant `fishhawk-runner` the
  acceptance agent could spawn inherits it and refuses pre-spawn
  (`runner_failed forge_writes_denied`, category C). A passthrough named
  `FISHHAWK_FORGE_WRITES` (any case) is REFUSED and reported exactly like
  a proxy-var passthrough; a base-env value is dropped by the allow-list,
  so the injected deny is the only entry under that name
  (`TestEnv_InjectsForgeWritesDeny`, `TestEnv_RefusesForgeWritesPassthrough`,
  `TestEnv_DropsBaseForgeWritesValue`).

`FISHHAWK_API_TOKEN` is never present — the acceptance agent holds NO
MCP token (ADR-050 decision 2); evidence ships signature-authed.

## Consumer and residual risk

The E31.7 runner acceptance executor (#1535) calls `BuildAllowlist` →
`Start` → `acceptenv.Env` around the acceptance invocation.

Residual: proxy env binds cooperating clients only — a raw-socket
bypass needs the OS sandbox (#611-class). Documented as a security
invariant in `docs/ARCHITECTURE.md` §6. The verb-level deny table above
adds two more stated residuals (Go loopback bypass, `Mcp-Name` omission /
spoof), which is why the server-side dev-mode refusal, not this proxy,
is the control that closed #3500.

## Acceptance containment posture (Rule-of-Two, ADR-050 / #1532)

The acceptance agent is the one agent that deliberately assembles all
three lethal-trifecta legs — code execution + network + credentials
against a running instance rendering untrusted data — so it is treated
as prompt-injected and contained on every leg:

- **Egress** is default-deny through the proxy above; the allow-list is
  exactly the spec-declared `egress.target_hosts` + model API endpoint
  + Fishhawk backend (CONNECT-tunneled, DNS-pinned, rebinding-shaped
  resolutions refused).
- **Credentials** are minimized via `acceptenv` (model key +
  operator-declared `FISHHAWK_ACCEPTANCE_ENV_*` target creds only; no
  MCP/Fishhawk token — evidence ships signature-authed; repo/deploy/
  broad-API tokens denied, and the deny set outranks the passthrough).
- **Authority** is advisory zero-write, so a fully-compromised agent
  can at worst emit a wrong verdict.

The model endpoint is a necessarily-open channel bounded by the egress
lock + zero-write authority.

## Downstream free-text containment (E31.8 / #1613)

The acceptance verdict's free-text evidence fields (`observed` /
`expected` / `steps_taken` / `expectation_basis` / `repro_handle`) are
attacker-influenceable. When class-1 triage
(`synthesizeAcceptanceConcerns`) routes a failed criterion into an
implement fix-up concern, that concern is provenance-marked
(`planreview.Concern.Provenance = ConcernProvenanceAcceptance`, a
server-internal marker never exposed in `VerdictSchema()`) and the
fix-up prompt renderer (`prompt.writeFixupConcerns`) routes its text
through the same `sanitizeUntrustedComment` quarantine envelope
(structure-neutralized, `| `-quoted, DATA-not-instructions framing)
instead of the trusted MANDATORY / win-on-conflict fix-up framing —
closing the injected-acceptance-agent → binding-implement-instruction
chain. Operator/reviewer-authored fix-up concerns (empty provenance)
render byte-identically on the trusted path.
