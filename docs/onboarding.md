# Onboarding a repo to Fishhawk

This document covers the first-run preflight surfaced by `fishhawk doctor`. It
is the operator-facing companion to the E29.4 readiness endpoint
(`backend/internal/server/onboarding.go`) and the E29.5 doctor extension
(`cli/cmd/fishhawk/doctor_onboarding.go`).

## `fishhawk doctor`

`fishhawk doctor` runs a set of preflight rungs and prints, for each, one of
`ok` / `warn` / `fail` plus a remediation hint when the rung is not `ok`. The
command exits non-zero if any rung **fails**; warnings alone still exit 0.

```
fishhawk doctor [--repo owner/name] [--working-dir D] [--runner-binary P] [--spec-only]
                [--run-verify-command] [--skip-verify-command] [--verify-timeout D]
```

Beyond the local-loop rungs (Docker stack, backend reachability, token
acceptance, spec presence, runner binary, MCP registration, git remote/tree,
`gh` auth, version/schema drift), `doctor` runs the **onboarding preflight**:
the per-repo prerequisites that make a repo *look* onboarded but wedge on the
first run.

### External repo vs. the fishhawk dev loop

`doctor` is run in two very different contexts, and several rungs only make
sense in one of them. In the **fishhawk dev loop** you are inside a checkout of
this repository, driving the local Docker stack; in an **external repo** you are
onboarding some other project to a running backend, typically over HTTP-MCP and
OAuth (`fho_` access tokens), with no local Fishhawk build at all. The rungs
below name which context they apply to:

- **token valid** accepts any credential class the backend accepts (`fhk_`
  operator, `fhm_` machine, `fho_` OAuth) — the backend, not a prefix table,
  decides validity — and *warns* rather than fails when no CLI credential is
  configured, because an MCP/OAuth-driven loop needs none.
- **runner binary found** and **backend SHA drift** are dev-loop rungs: an
  external repo has no runner binary beside it and no commit that fishhawkd was
  built from, so they *warn* / *skip* rather than fail there.
- **MCP registered** matches an HTTP-transport registration (the primary
  external-repo path) as well as the stdio shim.

The overriding invariant: a repo whose **GitHub App is not installed still
FAILS** (non-zero exit). Every relaxation below is scoped by a positive
server-side signal — never a blanket softening.

### Credential ladder and the token rung

The **token valid** rung resolves a bearer credential with the SAME ladder
`fishhawk run start` uses (`newClient`): an explicit `--token` / `$FISHHAWK_TOKEN`
wins; when empty, the stored credential minted by `fishhawk token login` (keyed
by backend URL) is used. Whatever it finds is probed against the backend — the
prefix is used only to LABEL the credential class in the rung detail, never to
admit or reject it. Branches:

- **no credential** in any tier → **warn** (`fishhawk token login`, or
  `--token` / `$FISHHAWK_TOKEN`; an MCP/OAuth loop needs none) — never a fail.
- credential the backend **accepts** (200 on `/v0/runs`) → **ok**, naming the
  class and source.
- credential the backend **rejects** (401/403/other) AND the readiness endpoint
  did NOT answer an authoritative 200 → **fail**.
- credential rejected on `/v0/runs` BUT the readiness endpoint answered an
  authoritative 200 for the same credential → **warn**, never fail: an
  OAuth/cookie identity carries no explicit scope list and can legitimately 403
  on `/v0/runs` while being adequate per readiness. **token scope adequate**
  (server-side, below) remains the authority on scope.

A successful readiness probe therefore **outranks** the local token rung — but
"successful" means HTTP 200 **and** a body that decoded into a readiness
verdict. A 200 whose body does not decode is NOT an answer, so a broken backend
response can never silently downgrade a genuine credential failure to a warning.

### MCP registration rung

**MCP registered** parses `claude mcp list` and recognises a Fishhawk MCP
registration in EITHER transport:

- the **stdio** shim (`fishhawk: /path/to/fishhawk-mcp-shim`), and
- the **HTTP** registration (`fishhawk-http: https://host/mcp (HTTP)`) — the
  transport this rung was extended to recognise.

A registration qualifies when its **name** is `fishhawk` or `fishhawk-*`, OR its
**target** is an http(s) URL whose host:port equals the configured
`--backend-url`'s and whose path ends in `/mcp` (a name-agnostic "points at this
backend" test). Both matchers are load-bearing: an HTTP registration on a
different port than `--backend-url` matches only by name; a non-`fishhawk` name
matches only by URL. On no match the rung **fails** with a transport-aware hint
that names the HTTP remedy first
(`claude mcp add --transport http fishhawk-http <backendURL>/mcp`) and the stdio
shim second. When the `claude` CLI is **absent or erroring**, the rung degrades
to **warn** ("cannot determine") — never a fail and never a false ok — because
the operator may drive from another MCP client entirely. A `claude mcp get`
fallback (probing BOTH the stdio and HTTP names) covers the corner where
`list` errors but `get` succeeds.

### Runner binary rung

**runner binary found** mirrors the resolution order the MCP dispatch path uses:
explicit `--runner-binary` > `$FISHHAWK_RUNNER_BIN` > a `fishhawk-runner`
**sibling of this CLI binary** > PATH > `<working-dir>/bin/fishhawk-runner`. The
sibling tier resolves next to *this CLI binary* as a **proxy for the serving
binary's sibling** the dispatch path actually resolves against — the doctor CLI
process is not the serving process, so a green from that tier proves a runner
sits beside the CLI, not beside the serving binary. When every tier misses the
rung **warns** rather than fails (naming the sibling-of-serving-binary case it
cannot observe), because a hard fail would be a false negative for an
MCP-driven dispatch whose runner sits beside the serving binary. The
`$FISHHAWK_RUNNER_BIN` / PATH remedy is still named for the local CLI loop.

### Backend SHA-drift rung

**backend SHA drift** compares fishhawkd's build SHA to the local HEAD. That
comparison is only meaningful inside the backend's own source checkout, so the
rung **skips outright** (ok, no remediation, before any HTTP call) when the
working dir is not one — detected structurally by a `backend/cmd/fishhawkd`
directory plus a root `go.work`. Comparing a backend build SHA to an unrelated
repo's HEAD is a category error that would warn forever and can never be
satisfied. Inside a real backend checkout the rung is unchanged (prefix
compare, `-dirty` strip, unknown-SHA ok, git-failure warn).

### `verify command` (E48.58 / #2485)

**Opt-in.** Under `--run-verify-command` this rung EXECUTES every distinct
`executor.verify.command` the spec configures, in a throwaway detached git
worktree at HEAD — the same shape the runner provisions for its committed-tree
verify gate. A fresh worktree materializes only **tracked** files, so a command
that depends on a gitignored build artifact, a downloaded toolchain, a generated
protobuf, or a `//go:embed`ed binary fails here in seconds rather than after an
implement pass has been paid for.

- **Execution is off by default.** Every other rung reads bytes or queries a
  service; this one runs a command string supplied by the checkout under
  inspection, so `doctor` on a repository you have just cloned and not yet read
  would otherwise be a code-execution primitive for whoever controls that
  repo's `.fishhawk/workflows.yaml`. Without the flag the rung is a **warn**
  that names it, so the gap #2485 is about stays visible on every run.
- A non-zero exit or a timeout is a **fail**, naming which command failed, its
  exit status, the throwaway worktree path, and a bounded tail of the output.
- An absent verify block, an unresolvable spec, an unavailable git worktree, an
  absent `--run-verify-command`, or `--skip-verify-command` is a **warn**, never
  a fail: the preset explicitly permits removing the verify block, and `fishhawk
  validate` / **workflow spec present** remain the authorities on a broken spec.
- `--verify-timeout` (default `5m`) caps how long *each* command may run; the
  spec's own `executor.verify.timeout` wins only when it is shorter, so a
  preset's `15m` gate cannot turn `doctor` into a quarter-hour command.
- `--skip-verify-command` is the opt-out and **wins over**
  `--run-verify-command`, so a shell alias or wrapper script that opts in can
  always be overridden on a single invocation.
- That child gets a **stripped environment**, the same default-deny allow-list
  the runner applies to the identical `sh -c <verifyCmd>` gate child (ADR-029 /
  #650 item 4): it sees `PATH`/`HOME`/locale/temp essentials and the Go
  toolchain vars (an explicit Go-name set plus `CGO_*`/`LC_*` — NOT a bare
  `GO*` prefix, which also admitted `GOOGLE_API_KEY` /
  `GOOGLE_APPLICATION_CREDENTIALS`, now dropped by a `GOOGLE_` deny prefix,
  #2504; URL userinfo is redacted out of `GOPROXY`-style values), and never
  your `FISHHAWK_API_TOKEN`, forge token, or agent API keys. Spec-supplied code
  plus network plus the invoking process's credentials is the shape the
  stripping exists to break.

### `--spec-only`

`--spec-only` restricts `doctor` to the two environment-free rungs (the
`verify command` rung is excluded: it provisions a worktree and spawns a
subprocess, so it is neither environment-free nor byte-only) —
**workflow spec present** (schema validity) and **execution path configured**
(every stage declares an executor) — and skips every docker/backend/token/MCP/
git/gh/onboarding rung. It is the fresh-repo quick-validate path: a repo whose
sole Fishhawk artifact is a freshly-scaffolded `.fishhawk/workflows.yaml` exits
0 with **no** local Fishhawk environment (no Docker, no backend, no token),
while a missing or schema-invalid spec still fails closed (exit non-zero). Run
it right after `fishhawk init` to confirm the scaffolded spec is valid to the
plan gate before wiring up the backend, token, and execution path.

### `--repo`

The onboarding rungs target a specific GitHub repo. Pass it as
`--repo owner/name`. When omitted, `doctor` auto-detects it from the working
dir's git `origin` remote (via the same parser `fishhawk run start` uses). If
the repo cannot be resolved (no `github.com` origin), the onboarding rungs
degrade to a single **warning** prompting for `--repo` — they do not fail the
whole command.

### Onboarding rungs

Four rungs are read from the readiness endpoint
(`GET /v0/onboarding/readiness?repo=owner/name`) — they are server-side-only
checks the CLI cannot perform locally:

| Rung | Fails when | Remediation |
|---|---|---|
| **app installed** | the Fishhawk GitHub App is not installed on the target repo | install the App: `https://github.com/apps/fishhawk/installations/new` |
| **reviewer available: `<provider>`** (one per spec-declared reviewer) | the reviewer's backend is not wired on this deployment | the adapter's missing-env hint, carried verbatim (e.g. set `FISHHAWKD_ANTHROPIC_API_KEY`) |
| **token scope adequate** | the caller token lacks a run-driving scope | reissue with the named missing scope(s) via `fishhawkd token issue --subject <login> --scopes …` |
| **workflow spec (committed) valid** | the spec on the repo's default branch fails to parse/validate (`source==fetched && !valid`); **warns** when the spec is unavailable (App not installed / no spec on the default branch) | run `fishhawk validate` for details |

A fifth rung is checked **client-side** against the discovered
`.fishhawk/workflows.yaml`:

| Rung | Fails when | Remediation |
|---|---|---|
| **execution path configured** | *any* stage in the **resolved** spec declares no executor (a spec that looks onboarded but wedges on the first unconfigured stage) | add an executor to each named stage (see `docs/spec/workflows-v0.md`) |

The execution-path rung reports `ok` **only** when *every* stage declares a
non-empty executor (`agent`, `human`, or a `delegate`). It checks the
**resolved** document (same-document reuse resolved first, #2340), so a
workflow-v2 stage that omits its own executor and inherits one from a
`defaults` block or an `extends` base counts as configured — the rung sees the
same document the validator does. A mixed workflow — some stages configured, at
least one not — **fails**, and the remediation names the unconfigured stage(s).
It warns (rather than fails) when no spec is found or the spec cannot be
resolved; the local-loop **workflow spec present** rung is the authority on a
hard-missing or schema-invalid spec.

### Degradation

The onboarding rungs never crash the doctor. A transport error, a non-200
response (401/403/5xx), or an unparseable body from the readiness endpoint
degrades to a single **warning** naming the failure, so a backend that does not
yet serve `/v0/onboarding/readiness` (or a token that is rejected) leaves the
rest of the preflight intact.

Only an authoritative readiness answer — HTTP 200 **with** a decodable body —
grants the readiness endpoint authority over the local token rung (the cascade
break above). A transport error, a non-200, or a malformed 200 body leaves the
token rung free to fail on a genuine credential rejection: the relaxation is
scoped to a positive server-side signal, never inferred from a status code
alone. A `403 repo_forbidden` (below) is one such non-200: it degrades to the
same single warning rather than crashing the doctor.

### Repo read-visibility gate (#1512)

Since #1512 (ADR-057 Amendment A2 / #2071), the readiness endpoint applies the
repo-scoped read-visibility gate the product already owns: a caller who holds
no forge `read` on the queried repo is denied `403 repo_forbidden` **before**
any installation resolve or spec fetch, so verbatim `spec.Error` text and
installation state never reach a caller with no read on the repo. A `503
service_unavailable` is returned instead when the visibility mirror cannot be
resolved (a store / provider-resolution / role-resolution fault) — deliberately
distinct from the `403` permission-denied class, never a silent allow.

**Three identity classes are UNFILTERED and keep the exact pre-gate surface:**

1. **Bearer / MCP token identities** — `repoFilterFor` returns nil for any
   identity with `TokenID != ""`. *Rationale:* bearer identities are bounded by
   token ownership and scope, and a repo-permission mirror keyed on a human
   forge subject has nothing to say about them. This is what keeps `fishhawk
   doctor` and the `fishhawk_doctor` MCP tool working — they authenticate with a
   bearer token.
2. **Workspace admins** — `RoleAdmin` bypasses the filter, INCLUDING admin
   **cookie** sessions.
3. **Deployments with no repo-ACL mirror wired** — `Config.RepoVisibility ==
   nil` is `repoFilterFor`'s first early return, so the endpoint keeps its exact
   pre-change surface.

**Who CAN see the `403`:** a **non-admin** browser cookie session on a
mirror-wired deployment. (Not "a browser cookie session on a mirror-wired
deployment" — that would be overbroad and wrong, because an admin cookie session
bypasses filtering.)

The concern's **cookie-session half is CLOSED** by this gate. Its
**bearer-token half is deliberately closed** with the recorded #2071 rationale
above (bearer identities are bounded by token ownership and scope), not left as
an unexamined gap.

## `fishhawk init`

`fishhawk init` (E29.3) is the primary onboarding surface. It scaffolds a repo
for Fishhawk in one command: it writes a schema-valid `.fishhawk/workflows.yaml`
from an autonomy preset, ensures the managed agent-docs bridge (AGENTS.md +
CLAUDE.md), prints the out-of-band prerequisites it cannot perform, and runs the
`doctor` preflight above as a closing step.

```
fishhawk init [--preset low|medium|high] [--working-dir D] \
              [--budget-usd N] [--single-reviewer] [--human-gates ids] \
              [--force] [--repo owner/name]
```

### What it does

1. **Resolves the repo root** by walking up from `--working-dir` to the
   directory containing `.git` (falling back to `--working-dir` when no `.git`
   is found), then targets `<root>/.fishhawk/workflows.yaml`.
2. **Writes the workflow spec** from the chosen `--preset` (default `medium`)
   plus optional structured deltas, reusing the E29.1 preset generator — which
   validates its own output and fails closed on a delta that would break schema
   validity:
   - `--budget-usd N` overrides the `feature_change` weekly advisory cost
     ceiling (`budgets[0].limit_usd`).
   - `--single-reviewer` drops the Codex agent reviewer, leaving Claude alone
     on every stage.
   - `--human-gates id,id` keeps the human gate only on the named stages; any
     stage with a gate whose id is not listed has it removed (omit the flag to
     leave every gate as authored).
   The generated spec is a **generic template**: it carries no
   fishhawk-repo-specific defaults. Its approval gates use the
   forge-neutral `approvals` block (`approvals: {count: 1, not: [author,
   agent]}`), so there is **no** `@your-github-handle` handle and **no**
   top-level `roles` map to fill in. One value is a placeholder you must
   replace before your first run:
   - the implement stage's `executor.verify.command: "make test"` → your
     repository's test command (run via `sh -c` after the agent exits), or
     remove the whole `verify` block if your project has no test entrypoint.
     It runs in a **fresh worktree**, so gitignored build artifacts and
     downloaded dependencies will not be present — a command that needs
     `node_modules/`, a downloaded toolchain, or a generated file must fetch or
     build it itself. `fishhawk doctor --run-verify-command` proves this by
     executing the command in a throwaway worktree before any run is started;
     plain `doctor` only warns, because execution is opt-in (see the
     `verify command` rung above).

   The placeholder is schema-valid as shipped, so `fishhawk doctor
   --spec-only` passes on the freshly-scaffolded spec before you customize
   it. See `docs/spec/workflow-preset.md` for the placeholder table and
   `docs/spec/workflow-v1.md` for the `approvals` predicate.
3. **Ensures the agent-docs bridge** via the E29.2 `bridge` package: the
   Fishhawk managed block in AGENTS.md and the `@AGENTS.md` import in CLAUDE.md.
   Both are idempotent and preserve content outside the managed markers, and
   `init` reports each file's per-file status (created / updated / unchanged).
4. **Prints the out-of-band checklist** — the three prerequisites `init`
   deliberately does not perform: installing the GitHub App, issuing an operator
   token, and configuring the execution path (the `.github/workflows/fishhawk.yml`
   workflow, `vars.FISHHAWK_BACKEND_URL`, and the reviewer API-key secrets).
5. **Runs the `doctor` preflight** so `init` finishes by telling the operator
   exactly which first-run rungs still need attention.

### Non-destructive

`init` refuses to clobber an existing `.fishhawk/workflows.yaml`: if the spec is
already present it prints the path and the `--force` escape hatch and exits
non-zero **without touching the file**. Pass `--force` to overwrite it. Offer-to-
merge an existing spec is out of scope for v1 — refuse is the safe default.

The bridge files are always merged idempotently (managed block / import line
only), so re-running `init` on an already-scaffolded tree yields a clean diff
even under `--force`.

### Guides, does not perform

`init` only writes files in the working tree. It does **not** install the App,
mint a token, or push the execution-path workflow — those cross an external
boundary and are the operator's to complete. The closing `doctor` run and the
printed checklist name each one. A `doctor` failure does not fail `init`: the
scaffold succeeded, so `init` reports the doctor issues and still exits 0.

## OAuth-based MCP onboarding (ADR-076 slice 3, #2391)

Connecting a spec-compliant MCP client to fishhawkd's `/mcp` surface is the
PRIMARY onboarding path (#2262): the client discovers where to authenticate
rather than being hand-fed a token. It walks RFC 9728 / RFC 8414 discovery:

1. **Unauthenticated `POST /mcp`** → `401` with a `WWW-Authenticate: Bearer`
   challenge carrying `resource_metadata="<PRM URL>"` (RFC 6750 §3 / RFC 9728
   §5.1).
2. **`GET` the PRM URL** (`/.well-known/oauth-protected-resource/<resource
   path>`) → the protected-resource metadata, whose `authorization_servers[0]`
   names the authorization server.
3. **`GET /.well-known/oauth-authorization-server`** → the RFC 8414 AS metadata
   (`authorization_endpoint`, `token_endpoint`, `code_challenge_methods_
   supported: ["S256"]`, …).
4. **Authorize** (`GET /v0/oauth/authorize`, authorization-code + PKCE) →
   consent → an authorization code.
5. **Token** (`POST /v0/oauth/token`) → an audience-bound `fho_` access token.
6. **Authenticated `POST /mcp`** with `Authorization: Bearer fho_…` → the tool
   surface.

This loop is served only when the OAuth AS is ENABLED (`--oauth-issuer` set,
plus a wired store and CIMD fetcher). A DISABLED or MISCONFIGURED AS answers the
discovery routes `503 oauth_as_unconfigured`, and the `/mcp` `401` degrades to
the realm-only challenge — the client then falls back to an operator-issued
`fhk_` bearer.

### Bind address vs. the loopback lift

`FISHHAWKD_ADDR` MUST be a loopback address (e.g. `127.0.0.1:8080`) while the
lift condition is unmet — i.e. whenever no `--oauth-issuer` is configured. With
no OAuth AS, `/mcp` is bearer-only and loopback-only per ADR-033, so a
non-loopback bind answers `403 mcp_route_loopback_only`.

The bind MAY be non-loopback ONLY when the OAuth AS is enabled: enabling it lifts
the loopback refusal into authenticated-only mode, where `/mcp` accepts ONLY an
audience-validated `fho_` identity and a bare `fhk_`/`fhm_` token is refused
off-host. Network exposure and auth tightening land together — you do not get one
without the other. `--oauth-require-loopback` (default off) forces BOTH the AS and
`/mcp` back to loopback-only even with an issuer configured.
