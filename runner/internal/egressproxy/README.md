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

## Invocation env (`runner/internal/acceptenv`)

`acceptenv.Env(base, proxyURL)` builds a default-deny allow-list of:

- system essentials;
- the model API keys (the one surviving secret class);
- operator-declared target creds via the
  `FISHHAWK_ACCEPTANCE_ENV_<NAME>` passthrough (prefix stripped; a
  passthrough colliding with a denied key or a proxy var is REFUSED and
  reported, never honored);
- `HTTP(S)_PROXY` / `ALL_PROXY` (both cases) pointed at the proxy, with
  `NO_PROXY` cleared.

`FISHHAWK_API_TOKEN` is never present — the acceptance agent holds NO
MCP token (ADR-050 decision 2); evidence ships signature-authed.

## Consumer and residual risk

The E31.7 runner acceptance executor (#1535) calls `BuildAllowlist` →
`Start` → `acceptenv.Env` around the acceptance invocation.

Residual: proxy env binds cooperating clients only — a raw-socket
bypass needs the OS sandbox (#611-class). Documented as a security
invariant in `docs/ARCHITECTURE.md` §6.

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
