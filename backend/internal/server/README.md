# backend/internal/server

fishhawkd's HTTP surface: route handlers for the v0 REST API and the
cross-component seams they anchor.

## 5xx information-disclosure chokepoint (`errors.go`, E67.15 / #2587)

`writeError` is the single runtime control that keeps a 5xx caller from seeing
raw plan/artifact/database/third-party causes while the operator keeps the full
cause. Because it reads the ACTUAL status int at every call site, no
endpoint-local code is needed and the runtime-resolved-status sites
(`derr.status` / `herr.status` / `werr.status` in deploy_rollback.go,
release_notes.go, defer_concern.go, workitems.go) are handled correctly — which
a static per-endpoint sweep could not do.

On `status >= 500` it:

1. **Redacts details against a DEFAULT-DENY allow-list** (`redactableDetailKeys`).
   Every detail key not on the list is dropped from the response. Membership
   rule: a key is admitted only when its value is a product-owned enum /
   identifier / boolean / integer, a caller-echoed request field, or a static
   literal; NEVER a value derived from an error, a subprocess, or a third-party
   API response. The `error` key is deliberately absent.
2. **Sets `error_ref`** on the body from the request id the requestID middleware
   already mints (also echoed as `X-Request-ID`), so the caller gets a
   correlation handle with no new plumbing. `error_ref` is the caller's own
   `X-Request-ID` echoed back, so it is a correlation handle, NOT a
   server-authenticated identifier.
3. **Logs ONE record joining `error_ref` with the FULL pre-redaction cause** —
   the client details before redaction plus a dedicated `cause` attribute — so
   the redaction is lossless for the operator: the caller gets the ref, the log
   line keyed by that ref gets everything.

4xx responses are byte-identical to the pre-#2587 shape: no allow-list, no
`error_ref`, details verbatim.

**The `__cause` (`internalCauseKey`) channel.** A handful of 5xx sites used to
pass the raw cause as the MESSAGE argument (recover.go's three scope-amendment
500s, and campaigns.go's `provider_unimplemented` 501), which the details
redactor cannot see. Those now pass a static literal message and hand the cause
through `internalCauseKey`, a details key `writeError`
strips UNCONDITIONALLY — at any status, ignoring the allow-list — and folds into
the same joined log record. Because the strip is unconditional it is not a new
disclosure surface if someone forgets the allow-list
(`TestWriteError_AlwaysStripsInternalCauseKey` pins the strip on a 4xx).

**Both halves of that channel are unconditional (E67.31 / #2637).** The strip
always was; the FOLD was not. Until #2637 the `cause` log attribute was appended
only inside the `status >= 500` branch, so a call site handing a cause through
the channel at a 4xx lost it entirely — stripped from the body (correctly) and
never logged, reaching neither the caller nor the operator. The append now runs
at any status. The `cause != ""` guard is retained, so a call with no cause (or
an empty one) still emits NO `cause` attribute and the common-path record is
byte-identical. `details` deliberately stays 5xx-gated: at 4xx those details
already ship verbatim in the body, so re-logging them is noise.

The residual asymmetry, stated rather than implied away: at 4xx the LOG RECORD
carries `error_ref` (it is in `writeError`'s unconditional attr slice, alongside
status/code/message/path/method) but the 4xx BODY does not — 4xx bytes stay
byte-identical to pre-#2587. So the operator can correlate a 4xx cause by
request id while the caller is handed no correlation handle; the channel is
operator-only at 4xx by design. Giving the 4xx body an `error_ref` would be a
client-visible contract change and is out of scope. No production call site
passes `internalCauseKey` at a 4xx today (the four producers are recover.go's
three 500s and campaigns.go's 501), so this closed a latent branch rather than
changing a shipped response. `TestWriteError_4xxLogsInternalCauseToOperator` is
the pin — one case per branch (4xx with a cause, 4xx with no cause key, 4xx with
an EMPTY cause value, and a 5xx control asserting the pre-existing fold of both
`cause` and the pre-redaction `details` is unregressed).

**Per-exemption security finding (why the three prior raw-cause exemptions were
removed).** The prior plan kept `slice_integration_error`,
`work_item_filing_failed`, and `product_report_failed` disclosing
`details.error` on COMPATIBILITY grounds (a consumer read the field). Reading
the producing paths shows each cause demonstrably can carry storage or
third-party-endpoint internals: `slice_integration_error` wraps run-storage
(pgx/Postgres) errors and a go-github base-ref resolution error carrying the
method, the GHES URL, the status and the API message;
`work_item_filing_failed` and `product_report_failed` wrap
`*github.ErrorResponse.Error()`, which embeds the request method, the (GHES)
URL, and the API message. Compatibility is a requirement, not a safety property;
it is paid by the `error_ref` migration (consumers read the ref and point at the
log) rather than by keeping the disclosure surface open.

**The raw-cause AST guard is a TRIPWIRE, not the control** (`raw_cause_guard_test.go`).
The runtime chokepoint is the control. The guard statically flags known
raw-cause SYNTAXES at 5xx call sites whose status it can resolve — a `.Error()`
message, an `fmt.Sprint*/Errorf` embedding an error, a `+` concatenation, a
single-hop alias, and a cause assigned to an allow-listed details key (the
smuggle surface the key-based allow-list cannot see on its own). It makes NO
completeness claim: it cannot see helper-returned strings, cross-function data
flow, struct-field reads, or multi-hop aliasing, and it REPORTS (never silently
passes) the runtime-status sites it cannot classify.

**Residual gaps (stated, not implied away).** (a) The allow-list is KEY-based,
so a future 5xx site could smuggle a cause under an admitted key such as
`reason`. The guard's details-half check catches a `.Error()` call, an `fmt`
embed, a `+` concat, a bare err-identifier, AND — since E67.29 / #2631 — a
single-hop alias whose name does not itself read as an error (`cause :=
err.Error()` then `{"reason": cause}`), which gives the details half the same
reach the message half already had. Still OUTSIDE the tripwire on this half:
multi-hop aliases, cross-function flow, helper-returned strings, and computed
(non-literal) detail keys. A NON-admitted key is deliberately not flagged — the
runtime allow-list drops it anyway, so a finding there would prove nothing.
(b) The MESSAGE half of a 5xx
response has no runtime control — the chokepoint cannot tell a static literal
from a raw cause at runtime — so the six existing message-half sites are fixed by
hand and the guard catches regressions in known forms; a cause introduced
through a helper-returned string or a multi-hop alias would reach the client
undetected. (c) `workitems.go`'s `provider_unimplemented` 501 message carries
product-owned `*workmgmt.UnknownProviderError` text (safe) and flows through a
runtime-status `writeError`, so the guard reports it UNCHECKED and it is left
unchanged; its message is safe and the file is out of this change's scope.

**Cross-boundary proof and the CLI golden.** `error_redaction_integration_test.go`
stands the REAL server up on `httptest` with a provider failing on a cause
carrying planted internals (a GHES hostname, a pgx SQLSTATE fragment, a secret
payload token), then proves the contract at both consumers: the raw 502 body
carries `error_ref` equal to the response `X-Request-ID` and none of the planted
internals, and the SHIPPED `fishhawk_file_issue` MCP tool's operator-visible
string carries the code, the where-to-look pointer, no planted internal, and the
`error_ref` of THAT consumer request — captured by a per-request capture
middleware and asserted distinct from the raw call's fixed id, so a placeholder
or any-non-empty ref no longer satisfies it (E67.29 / #2631).

Go's internal-package rule bars `cli/` from importing `backend/internal/server`,
so the CLI half consumes a SERVER-PRODUCED artifact,
`cli/internal/httpclient/testdata/5xx_envelope.golden.json`. Since E67.29 that
golden is BYTE-COMPARED on a normal run, not merely rewritten under `-update`:
an artifact that is only ever written is a hand-mirror both halves can agree on
while the server silently diverges. The envelope is deterministic (fixed
`X-Request-ID`, no details on that 502, `json.Encoder` emits struct fields in
declaration order with a trailing newline), so a mismatch is real drift, not a
flake — it fails with the exact refresh command.

## Account-ownership authorization (ADR-057 / E44.5, #1829)

Handler authorization is tenant-scoped through ONE centralized middleware layer
(`middleware.go`), applied at route registration (`handlers.go`) — never
scattered per-handler. `Identity.AccountID` is the caller's tenant workspace
account, populated on every auth path in `bearerAuth`: the cookie path from the
session row's `account_id` (#1827); the api_token bearer path from the resolved
token's `AccountID` (selected by `apitoken` `GetTokenByHash`); the `mcp:run`
path from the token's OWN run via `run.AccountGetter` (`GetRunAccountID`), so an
mcp token is bounded to its run's account exactly like a bearer token bound to
it. That lookup is **unconditional and fail-closed** (E44.11 / #2074):
`AccountGetter` is a REQUIRED method on `run.Repository` (and on
`campaign.Repository` for `enforceCampaignAccount`), not an optional
type-asserted capability, so no wiring can skip account resolution and produce
an accountless — and therefore globally-visible — `mcp:run` identity. `("", nil)`
is the untenanted happy path (empty `AccountID`, allowed); ANY lookup error is a
`503`.

**The tiered wrappers.** `require{Run,Stage,Concern}Account(tier, next)` resolve
the route's run WITH its `account_id` (`run_id` directly; `stage_id →
stage.RunID → run`; `concern_id → concern.RunID → run`) and enforce before
calling the handler. When the run can't be resolved (no repo, bad UUID, not
found, load error) the wrapper **falls through** to the handler unchanged, so
`503`/`400`/`404` surfaces are never altered by the authz layer. Three tiers,
each visible in `handlers.go`'s route table (the tier of every write route
encodes the operator's admin-vs-member founder decision, reviewable there):
`readAccess` (GET run/stage/gate views — ownership only), `memberWrite`
(operator-decision writes + the runner `ship-*` uploads), `adminWrite`
(destructive/admin sub-actions: cancel, recover, revive, reset-branch, redrive,
reap-failure, deployment rollback, signing-key, installation-token, mcp-token).

**`enforceAccount` — two checks.** (1) OWNERSHIP (all tiers): a tenanted run
(`AccountID != ""`) whose account disagrees with the caller's → `403
account_forbidden`; an untenanted run (`AccountID == ""`, every row today) is
allowed — the NULL-allow window #1830 closes once every row is populated. (2)
COOKIE ROLE-BOUNDING (write tiers only, resolved OAuth cookie only:
`SessionID != "" && TokenID == ""`): an empty `AccountID` on a write → `403
account_unresolved`; with a role provider wired (`Config.AccountRoles`,
`account.Store.MemberRole`), an `adminWrite` tier requires the `admin` role
(else `403 insufficient_role`) while `memberWrite` admits `member`/`admin`/
NULL-role (least privilege). Bearer and mcp identities carry a `TokenID`, so
role-bounding never fires for them — ownership alone bounds them. A nil
`AccountRoles` (no database wired) is the untenanted-allow posture:
role-bounding is skipped, ownership still applies. The role lookup is
**forge-agnostic** — it strips the `<provider>:` prefix from the identity
subject generically (`github:`, `gitlab:`, any future forge), never a hard-coded
literal.

**List / export account scoping (filter, not 403).** `GET /v0/runs` bounds its
page to the caller's account via `ListRunsFilter.AccountID` (SQL `account_id = $
OR account_id IS NULL`). The bulk-export surfaces (`GET /v0/audit/export`,
`.../export.csv`, `GET /v0/reports/agent-changes(.md)`) route their resolved
page through `accountVisiblePage` (`audit_export.go`). Both keep untenanted
(NULL-account) rows visible, and an empty caller account (operator/bearer token
with no account) sees everything (the pre-tenancy view). The run-less global
audit-chain partition is never account-scoped (it has no owning run).

**403 codes:** `account_forbidden`, `account_unresolved`, `insufficient_role`.
The cross-boundary integration matrix is `authz_account_test.go`.

## Repo-scoped in-workspace visibility (`repovisibility.go`, ADR-057 Amendment A2 / E44.10, #2071)

A second, narrower boundary layered STRICTLY ON TOP of the account-ownership
checks above: within their own tenant account, a workspace member sees only the
repos they hold at least `read` on at the forge. It loosens nothing — every
ownership check and RLS predicate still applies first, and this only removes
rows. Migration 0057's RLS policies are inert in production today (the runtime
role is a superuser, which bypasses RLS even under FORCE), so this handler
filter is the effective in-workspace boundary until that rollout completes; the
tests assert filtering under the ordinary test role for exactly that reason.

**Where the permission comes from.** `Config.RepoVisibility` (the
`RepoVisibility` interface — `*repoacl.Mirror` in production) mirrors, per
`(provider, subject, repo)`, the tier `identity.IdentityProvider.PermissionLevel`
resolved, TTL-gated. The server package does NOT import `repoacl`, the same
convention `AccountRoles` follows. See `backend/internal/repoacl/README.md` for
the mirror contract.

**Resolution, once per request** (`repoFilterFor`). Filtering resolves to a
`*repoFilter`, or to nil meaning "not applicable" — and nil is nil-safe, so a
handler holds one value and never branches on whether filtering is on. It is
NOT applicable when: `Config.RepoVisibility` is nil (no mirror wired — the
untenanted-allow posture, byte-identical to pre-#2071, and the documented
no-code-change disable switch); the caller is anonymous; the caller is a
bearer / MCP token (`TokenID != ""` — deliberately unfiltered, bounded by
ownership alone, so the CLI and the runner's own MCP token are unaffected); or
the caller resolves to the workspace `admin` role through the SAME
`Config.AccountRoles` seam #1829 uses (the admin bypass). A role-resolution
error is neither a bypass nor a deny — it 503s.

**Cross-forge is default-deny, with ZERO forge calls.** `repoFilter.allows`
resolves the row's forge through `Config.RepoProviders` (`ProviderResolver`,
`accounts.provider` keyed by the repo owner) and denies immediately when it
differs from the provider prefix of the caller's subject — a GitHub-only login
sees no GitLab-installation data, and GitHub is never asked about a GitLab
repo. A not-found / ambiguous answer from a WIRED resolver (the repo owner is
unregistered, or — per `account.Resolver`'s contract — registered under BOTH
forges) also DENIES, with zero forge calls: falling through to the mirror there
would ask the caller's forge about the row's repo, so a GitLab-installation row
`acme/app` could be shown to a GitHub-only login holding read on a same-named
GitHub repo — both a leak and the forge lookup `[cross-forge-default-deny]`
forbids. It is logged at WARN naming the repo, because an ambiguous owner is an
operator-fixable account-registration state, not a permission answer. Only a
NIL resolver (the cross-forge check not configured at all; in production the
resolver and the mirror are wired together, both gated on `pool != nil` in
`serve.go`) leaves the decision to the mirror. A resolver ERROR is a store fault
and 503s. Per-repo answers are memoized for the life of one request, so a list
page asks about each repo once.

**An identity that cannot be keyed into the mirror is denied, not exempted.** A
cookie subject with no `<provider>:` prefix yields a deny-all filter and a WARN,
not an unfiltered request. No such subject is minted today (`bearerAuth` mints
`github:<login>`), and that is precisely why the branch must not be the one path
that silently bypasses filtering if a future auth path ever mints one.

**The two failure classes are never collapsed** (the binding rule, stated once
in `repoacl/README.md` and honored identically here). A FORGE error — including
`identity.ErrRateLimited` — means the permission is UNKNOWN: the mirror returns
`(false, nil)` and logs at WARN naming the repo and the reason, so that repo is
not visible for this request, nothing is memoized in the mirror, and the request
otherwise proceeds. A STORE error means the filter itself cannot function: the
request fails `503 service_unavailable` via `writeRepoFilterUnavailable`. The
classification rides in the return shape, so no handler can turn a forge fault
into a 503 or a DB outage into a silent short page.

**Lists FILTER, point reads 403** — the same convention #1829 uses. `GET
/v0/runs` and `GET /v0/campaigns` drop non-visible page rows. Point reads answer
`403 repo_forbidden`: for runs/stages/concerns centrally, inside
`enforceAccount` (so `require{Run,Stage,Concern}Account` all inherit it — the
run, stage, artifact, per-run-audit and concern point reads are covered without
touching each handler); for campaigns, in `handleGetCampaign`,
`handleListCampaignItems` and `handleGetCampaignStatus`, which the run-scoped
wrappers do not cover.

**READ paths only.** The `enforceAccount` check runs inside the `readAccess`
branch, and the shared refinement loader applies it only to `GET`/`HEAD`
(`isReadRequest`). The mirror is a **non-authoritative, TTL'd cache of a forge
read permission**, and #2071 scopes it to read *visibility*. Gating
`memberWrite`/`adminWrite` on it would let a cached deny — including one a
forge fault produced — block a caller whose *current live* forge permission
authorizes the action, and would reject a refinement decision or an approval
before E39's live decision-point `PermissionLevel` check runs. Write and
approval eligibility are unchanged by #2071: ownership, cookie role-bounding,
and the live checks the write paths already make.

**Pagination artifact (accepted).** The offset cursor counts PRE-filter rows, so
a filtered page can come back shorter than `limit` with `next_cursor` still
non-empty. Following the cursor to exhaustion still returns every visible row
exactly once (`runs_list_test.go` pins it). The alternative — pushing an
allowed-repo array into the SQL filter — needs an enumerable repo set the mirror
cannot supply without a forge repo-list call the `IdentityProvider` seam does
not expose.

**403 code:** `repo_forbidden`. Branch matrix: `repovisibility_test.go`;
per-surface assertions in `runs_list_test.go`, `runs_get_test.go`,
`campaigns_test.go` and `middleware_test.go`.

## Per-repo work-management conventions loader (`conventions_loader.go`, E45.16 / #2022)

`RepoConventionsLoader.Load` is what serve.go installs as the process-wide `conventionsLoader`
seam (`workitems.go`, signature `func(ctx, repo)`): it fetches `.fishhawk/work-management.yaml`
from the filing repo's **own** forge, breaking the chicken-and-egg the deployment override
sidestepped — the fetch-forge is resolved from **outside** the conventions file.

**Resolution chain per filing** (each fall-through is deliberate; each error is fail-closed):

1. **Provider discriminator** (the SOLE out-of-file hint in this pass; run-bound forge-context
   corroboration is explicitly out of scope): `account.Resolver.ResolveProvider` looks up
   `accounts.provider` by the repo owner as `account_key`. Exactly one row selects the forge;
   zero rows **or an ambiguous key** (legal under `accounts.UNIQUE(provider, account_key)`, the
   same key under both providers) fall through cleanly — never an arbitrary first row. A query
   error **fails closed** (propagated) rather than silently selecting a different provider on a
   transient DB fault.
2. **Self-resolved CredentialScope**: github routes through the server's existing
   `resolveRepoScope` (exposed as `Server.GitHubRepoScopeResolver`; zero scope = App not
   installed → fall through; a transient resolution *error* fails closed); gitlab uses the
   deployment-level scope serve.go wires when the gitlab forge is registered. No resolvable
   scope, a nil fetcher (forge absent from the registry), or an unknown provider are all treated
   exactly like an unregistered forge → fall through.
3. **Fetch + parse**: the provider's `forge.FileFetcher` reads the file (gitlab with the explicit
   `ref=HEAD` the Repository Files API requires). `forge.ErrNotFound` (no committed file) falls
   through; **any other fetch error and any parse error fail closed** — an auth/transport/server
   fault must not silently switch providers.
4. **Break-glass override** (`FISHHAWKD_WORKMGMT_CONVENTIONS`, retained from ADR-058 Phase 2
   #1856): served whenever the chain falls through, else `workmgmt.Default()`.

**Cache**: parsed conventions are cached per **`(provider, repo)`** key, TTL-gated (5 min default;
clock/TTL/parse injectable so `conventions_loader_test.go` asserts the counters): within TTL the
cached parse is served with **no fetch**; after TTL a refetch **reuses the cached parse when the
blob SHA is unchanged**. The key is **forge-qualified** so a repo reassigned to a different
provider never serves the prior forge's cached parse. A **per-key mutex** is held across the fetch
so concurrent same-repo filings do one fetch, not a thundering herd — but it is **per repo**, not
process-global, so a slow or hung forge round-trip for one repo does **not** stall filings for any
other repo (the short map-guarding `mu` is never held across the fetch). The per-key lock map, like
the cache, never evicts — bounded in practice by distinct authenticated filing targets.

### Destination authorization (`conventions_destination.go`, E44.14 / #2090)

Resolving where the file is READ FROM is not the same as authorizing where it FILES TO. A
repo-committed conventions file is **untrusted input**: without a binding, a file committed to
any repo the deployment can read could name any provider and any project reachable by deployment
credentials, redirecting filed work items and product reports out of the repo's tenancy boundary.
So a **repo-fetched** parse is destination-authorized immediately after `workmgmt.Parse` and
**before** it is cached or returned:

| Conventions provider | Destination key | Bound to |
|---|---|---|
| `github_projects` | `project.owner` | the repo's resolved account key (github family) |
| `gitlab` | the namespace root of `gitlab.project`, or the filing repo's own owner when the block omits `project` | the repo's resolved account key (gitlab family) |
| `jira` | `jira.project_key` | nothing — a jira destination has **no forge account to bind to**, so it is refused unless allow-listed |

The account key is the filing repo's owner segment — by construction exactly the `account_key`
the discriminator lookup used (`account.Resolver.ResolveProvider` cuts `repo` at the first `/`).
Comparison is case-insensitive (forge logins and namespace paths are case-preserving but
case-insensitive for identity). Both the forge **family** and the **key** must match; a
cross-forge destination (a gitlab file under a github account, or the reverse) is refused. A
provider outside the closed set, an empty provider, or a declared provider with a nil connection
block all fail closed.

**Not cached, no fall-through, on refusal.** A refused destination returns an error wrapping
`errConventionsDestinationUnauthorized` and is neither written to the cache nor allowed to fall
through to the break-glass override / `Default()` — caching would hide the redirect attempt
behind the TTL, and falling through would let repo-committed content *select* the deployment
default. That ordering is also why the cached-serve and unchanged-SHA branches need no re-check:
nothing enters the cache without having passed authorization. An edit that caches earlier
silently reopens the hole (`TestConventionsLoader_DestinationRedirect_Refused` asserts the second
`Load` refetches and re-parses).

**Escape hatch is administrator-controlled, never repo-controlled**:
`FISHHAWKD_WORKMGMT_ALLOWED_DESTINATIONS` carries comma-separated
`<account-key>:<provider>:<destination-key>` entries; a **malformed value fails boot** rather
than degrading to an empty (strict) allow-list. Every refusal names the exact entry to add. A
`gitlab` destination key must be the namespace **root** (`group`), not a project path
(`group/team`): the derived key is the namespace root, so a full-path entry could never match —
it is rejected at parse time, naming the root entry to use, rather than sitting silently inert.

**The administrator-controlled fallbacks are deliberately NOT validated.** The
`FISHHAWKD_WORKMGMT_CONVENTIONS` override and `workmgmt.Default()` are the trusted deployment
inputs whose displacement by untrusted repo input is the entire concern; validating `Default()`
would also break every deployment whose shipped default names a project outside the filing repo's
owner.

**Residual limitation**: the binding is enforced only when the discriminator resolves an account.
Until E44 populates repo→account rows, a repo with no account row still falls through to the
trusted override / `Default()`, unvalidated. This change therefore only ever tightens the current
posture — it never weakens it.

**Operator-accepted E44 posture**: the E44 `accounts` tables are not yet populated in
production, so the discriminator path resolves `found=false` and live filings degrade to the
break-glass override / `Default()` — production effect begins when E44 wires repo→account rows.
Until then the discriminator-driven end-to-end selection is exercised by
`conventions_loader_test.go` (per-failure-mode + the mixed-forge test driving one loader across
a github repo and a gitlab repo).

## Acceptance stage seam (E31, ADR-049 / ADR-050)

The acceptance surface spans spec → plan pre-check → dispatch →
runner-shipped signed verdict → outcome ingest → living-anchor render →
deterministic triage.

- **Spec**: the `acceptance` stage type / artifact / `egress` allowance
  (`docs/spec/workflow-v1.schema.json` v1.1–v1.3, semantic bindings in
  `backend/internal/spec/validate.go`). Full runnable example at
  `docs/spec/examples/workflow-v1-acceptance.yaml` — also the
  operator's `.fishhawk/workflows.yaml` companion-commit stanza, since
  the implement agent cannot touch `.fishhawk/**`.
- **Plan gate**: `acceptance_precheck.go::runAcceptancePrecheck`
  (stage-conditional; writes `plan_acceptance_precheck` with a
  `no_blocking_criterion` finding for a criteria-less behavioral plan).
- **Ingest + triage**: `acceptance.go::handleShipAcceptance` verifies
  the Ed25519 signature (or an operator bearer), persists an
  `artifact.KindAcceptance` row + an `acceptance_outcome_recorded`
  audit entry, and on a failed verdict runs `triageAcceptanceFailure`
  inline (`classifyAcceptanceFailure` → class 1 fix-up / class 2
  acceptance re-open / class 3-4 paged; bounded; exactly one
  `acceptance_triage_decided` entry).
- **Render**: `issuecomment.RenderStatusBody`
  (`status_template.go::renderAcceptanceOutcomeLine` /
  `renderAcceptanceTriageLine`).
- **Triage-miss corpus + stats**: `acceptance_stats.go`,
  `GET /v0/acceptance-triage/stats`.

**Cross-boundary seam test** (#618 pattern, the E31.10 capstone):
`acceptance_integration_test.go` runs `spec.Parse()` on the committed
example and drives the whole seam over one shared audit store — welding
the example's schema-validity to the suite. Per-slice unit coverage:
`acceptance_test.go`, `acceptance_precheck_test.go`,
`acceptance_stats_test.go`. Runner-side schema↔validator lockstep:
`TestAcceptanceVerdictSchema_LockstepWithValidator`.

## Pre-spawn acceptance-dispatch admission (E31.23 / #1928)

`acceptance_admission.go::handleAcceptanceAdmission` —
`POST /v0/stages/{stage_id}/acceptance-admission`, the pre-spawn admission step
a local host dispatch (`fishhawk_dispatch_stage` / `fishhawk_run_stage` /
`fishhawk_drive_run`) calls for an acceptance stage BEFORE it spawns a runner. It
closes the parity gap where the acceptance all-skip / empty-criteria /
out-of-scope short-circuit fired only on the `orchestrator.Advance` retry path,
not at initial host dispatch — so a run whose every acceptance criterion is
`skip_expected`-with-basis spawned a runner that needed a preview and failed
category-C `acceptance_target_unreachable`, a failure the server already knew was
unnecessary.

- **Orchestrator delegate:** the handler calls
  `orchestrator.TryShortCircuitAcceptance(runID, stageID)` — the exported entry
  point that shares the exact predicate/walk/emit core the inline `Advance` arm
  delegates to (so retry-path behavior stays byte-identical). The target must be
  an acceptance stage in a dispatch-admissible state — post-#1936 that set is
  exactly `{pending, awaiting_host_dispatch}`. `dispatched` is deliberately NOT
  admissible: post-#1912 a `dispatched` acceptance stage means the host-dispatch
  marker already stamped a spawn attempt, so short-circuiting it under a live
  runner is the double-drive #1936 closes; a migration-missed legacy `dispatched`
  park degrades to the normal operator-dispatched spawn path (pre-#1928 behavior,
  safe) instead. On a hit the stage is walked straight to `succeeded`, the
  matching audit lands (skip marker for out-of-scope, an
  `acceptance_outcome_recorded` **not-validated** verdict for the other two —
  see below), and `Advance` is re-entered so the run rolls forward.
- **The short-circuit records `not_validated`, never `passed` (#2347).** Both
  verdict-recording predicates settle a stage that verified exactly ZERO criteria
  — no runner, no preview, no observation. Emitting the same `passed`/`accepted`
  words a validator-shipped pass emits made an ABSENCE of verification render as
  certification at every consumer: the merge gate (ADR-049 decision #6), the
  operator's status comment, release evidence. The short-circuit now emits
  `plan.AcceptanceVerdictNotValidated` / `plan.AcceptanceOutcomeNotValidated`
  plus a `criteria_live_validation` count (how many criteria are marked
  `requires_live_validation`, so a skip carrying a tracked operator-validation
  walk — #2338 / #2345 — is distinguishable from one skipped on any other basis).
  `acceptanceGateState` resolves that verdict to the merge-ELIGIBLE
  `acceptance_not_validated` state: eligibility is deliberate, because a change
  with no live target must not be stranded, and a text-matching merge block would
  trade a dishonest pass for a wedge. The distinction is carried by the state
  string, the MCP `next_actions` reason (which asks the operator to acknowledge
  the non-validation in their merge verdict), and a distinct status-comment row —
  a PROMPT, not an enforcement. The verdict is **server-internal only**:
  `acceptanceBody.validate` still rejects any wire verdict other than
  `passed`/`failed`, so a validator cannot forge it, and an already-recorded
  `passed` keeps its exact prior meaning (no migration, no in-flight breakage).
- **Admission ↔ host-dispatch fence (#1936):** the whole read → admissibility-check
  → walk in `TryShortCircuitAcceptance` runs under a per-stage in-process mutex
  (`orchestrator.LockStageAdmission`) that `host_dispatch.go::handleHostDispatchStage`
  also takes across its stage-load → eligibility → CAS. This closes the mid-walk
  race where a client that timed out on the admission POST re-read `pending`/
  `dispatched`, then the host-dispatch marker observed the walk-intermediate
  `dispatched` and returned the idempotent `{transitioned:false}` proceed while
  the walk continued to `succeeded` — a double-drive. Serialized, the marker
  either waits for the walk (then 409s on the settled stage — the MCP verb's
  fail-closed marker handling spawns nothing) or wins the CAS first (then the late
  admission re-reads `dispatched` under the lock and no-ops per the narrowed
  admissible set). The lock map **never evicts** (one mutex per admission-touched
  stage per process lifetime, negligible at v0 volume; eviction under a concurrent
  `LoadOrStore` is a correctness hazard, not a bug). The fence is **single-process
  only** — the multi-replica upgrade is a DB-transactional walk (out of scope; v0
  deploys a single replica).
- **Bounded detached walk (binding condition 1, #1936):** the handler invokes
  `TryShortCircuitAcceptance` under `context.WithTimeout(context.WithoutCancel(r.Context()), acceptanceAdmissionWalkTimeout)`.
  The timeout bounds ONLY the context-cancellable part of the pre-mutation phase
  (the `GetRun`/`ListStagesForRun` admissibility reads). It does **not** bound the
  per-stage `LockStageAdmission` acquisition that precedes those reads — that blocks
  on a plain, non-context-aware `sync.Mutex.Lock()`, so a goroutine parked behind a
  long-held lock waits past the deadline. This degrades safely: once the lock is
  acquired the first admissibility read fails fast on the by-then-expired context,
  so nothing mutates, and the lock hold is itself bounded by the holder's own
  DB/statement timeouts. `TryShortCircuitAcceptance` re-detaches onto
  `context.WithoutCancel` with NO deadline at its **point of no return** (the first
  state transition), so a client disconnect or the handler timeout can no longer
  abort the walk mid-flight. An admission that begins its state walk therefore
  always runs to completion (settle + audit + `Advance`) — nothing changed, or fully
  settled. Individual repo calls stay bounded by their own DB/statement timeouts, the
  honest liveness backstop.
- **Auth mirrors `handleRetryStage`:** authenticated identity required (401
  anonymous), `write:stages` scope gates a token identity (403
  `insufficient_scope`), and an `mcp:run:<uuid>` subject may only admit stages
  within its own run (403 `cross_run_admission`). The endpoint reuses
  `write:stages` and adds NO new scope or audit kind, so the Auth-change impact
  inventory is empty.
- **Fail-open by design (the reconciliation binding condition):** a
  non-admissible stage state (already settled, mixed criteria, an unconfigured
  orchestrator) returns `200 {short_circuited:false}` with NO warning — the
  normal no-op path; the caller records spawn evidence and spawns a runner
  exactly as today. A hit returns `200 {short_circuited:true, kind, basis,
  criteria_total, stage}`. A non-acceptance stage is `422 validation_failed`; an
  unknown stage is `404`.
- **MCP callers fail OPEN only on a TRANSPORT error** (network/5xx → warning +
  spawn as today); `short_circuited:false` never adds a warning. A **4xx
  admission REJECTION** (401 / 403 `cross_run_admission` / 404 / 422) is NOT
  fail-open — the verb HALTS with a tool error and spawns nothing, so a runner
  never executes after the run-subject authorization boundary rejected the
  request. On the 5xx fail-open path the verb ALSO re-checks the target stage
  before spawning: a mid-walk 500 can leave the acceptance stage `running`, and
  an observed non-dispatchable state halts rather than double-driving it. Tests:
  `acceptance_admission_test.go` (endpoint) + the orchestrator's
  `TestTryShortCircuitAcceptance` + the MCP `*_Acceptance*FailsClosed` /
  `*_PostFetchFailure` cases.

## Decomposition wave-order host-dispatch guard (`decomposition_dispatch_guard.go`, E48.99 / #2546)

The host-dispatch marker is the fail-closed chokepoint every host-spawn verb
(`fishhawk_dispatch_stage`, `fishhawk_run_stage`, `fishhawk_drive_run`,
`fishhawk_run_children`) calls immediately before spawning a runner. For a run
carrying `decomposed_from` + `slice_index`, `guardDecompositionWaveOrder`
resolves the child's declared dependencies from the parent's approved plan
(`decomposition.sub_plans[slice_index].depends_on` — the same authority
`plan.Waves` topologically sorts into the `plan_decomposed` `waves` payload),
maps each dependency slice onto its minted sibling child run, and refuses `409
dependency_not_satisfied` naming the LOWEST-indexed blocking sibling when any
dependency has not reached run state `succeeded`. Mirrors
`start_campaign_item_run`'s `item_not_eligible` "blocked on dependency issue:N"
precedent on a different surface.

- **Authority**: the parent's approved plan, loaded via `loadApprovedPlanForRun(*decomposed_from)` — the parent OWNS the plan stage, so the walk resolves on its first hop. Siblings are enumerated with the paged `listDecomposedSiblings` (mandatory explicit page size: `postgresRepo.ListRuns` rejects `Limit<=0`), mirroring `listAllDecomposedChildren`.
- **The absent / errored / unmet partition** (`resolveSliceDependencies`): an ABSENT input is NOT a violation — admit and log (not a fan-out child, plan resolves nil / no decomposition, or `slice_index` out of range, the same defensive degrade `matchDecomposedSubPlan` takes; the prompt-fetch path stays the fail-closed authority for a slice/plan mismatch via `409 decomposed_scope_unresolved`). An ERRORED read is retryable — `500 dependency_check_failed`, never a silent admit. Only a positively-UNMET dependency refuses. Refusing on absence instead would wedge every legitimate dispatch behind an unresolvable plan — a strictly worse failure than the operator error being guarded.
- **Ordering relative to the CAS**: wired into `handleHostDispatchStage` AFTER the runner_kind/executor eligibility checks and BEFORE the state switch/CAS, inside the already-held stage-admission lock. A refusal commits NO state and the stage stays parked at `awaiting_host_dispatch` (pinned by the committed-state handler assertion — error identity alone is insufficient, since a guard placed after the CAS would 409 with the stage already flipped). Because it precedes the `switch stage.State`, it also runs AHEAD of the idempotent already-`dispatched` no-op branch: a fan-out child already `dispatched` whose dependency has since regressed out of `succeeded` (a revived/re-opened sibling) is refused `409 dependency_not_satisfied` on re-dispatch rather than returning the `200 {transitioned:false}` dead-runner no-op — wave order is re-validated on the re-dispatch path, and the `dispatched` state is left unchanged (pinned by `TestHostDispatch_DecompositionAlreadyDispatched_RegressedDependency_Refuses`).
- **The deliberate no-N+1 split**: `slice_index` is a pure row projection surfaced by `toRunResponse` (so it rides both `GET /v0/runs/{id}` and the list endpoint), while `slice_depends_on` is resolved by `handleGetRun` ONLY (the single-run read, best-effort like Concerns/Delegation) — the list endpoint never pays the per-row plan load.
- **Atomicity of the predicate vs the CAS (#2586)**: the guard's `ListRuns` sibling snapshot is NOT atomic with the caller's stage CAS, and NO lock spans the two rows — the guard reads sibling RUN rows while the caller writes this child's STAGE row, and the held stage-admission lock is keyed to the stage and serializes no sibling-run write. The window is real. What makes it safe is run-state MONOTONICITY, not serialization: run state `succeeded` is **absorbing** — `runs.state` is written by exactly one query (`UpdateRunState`) reached by exactly two repository methods, each inside a `SELECT … FOR UPDATE` transaction (`TransitionRun`, gated by `ValidRunTransition`, which refuses any terminal `from`; `RetryRun`, gated by `ValidRunRetryTransition`, which admits only `failed → running`), `ReviveRun` refuses a non-failed run outright, and there is no run-deletion path. Enumerated window directions: a dependency **reaching** `succeeded`, or a dependency slice gaining a freshly-minted (`pending`) sibling, leaves the guard deciding on a stale snapshot and **refusing** — fail closed, a spurious 409 the operator clears by re-dispatching; a dependency **leaving** `succeeded` would admit out of wave order and is the dangerous direction, unreachable precisely because `succeeded` is absorbing. Because enforcement is repository-layer under a row lock rather than an in-process mutex, this argument survives multiple `fishhawkd` replicas — unlike the #1936 stage-admission fence, whose residual is single-process by construction. **Scope**: this covers SIBLING-RUN-STATE staleness only. NOT covered — the dependency SET is read from the parent's approved plan, so a parent plan revised between that read and the CAS can change `depends_on` and leave the guard deciding against a stale dependency set; the absorbing-succeeded argument constrains run STATE, not plan CONTENT. That plan-revision window is an operator-level anomaly rather than a caller-drivable race, and it is untested and unfixed here.
- **What the concurrency test does and does not pin (#2586)**: `TestHostDispatch_ConcurrentDecomposedDispatch_AdmitsExactlyOne` fires 8 concurrent host-dispatch POSTs at one fan-out child stage and asserts exactly one `transitioned:true`, with the rest 200-idempotent or 409 and the stage settled at `dispatched`. That pins **the stage CAS admits one dispatcher** — and nothing more. Deleting the `LockStageAdmission` acquisition was run as a counterfactual and the test still PASSES without `-race` (under `-race` the deletion fails, but on an unsynchronized counter in the `guardCountingRepo` test fake, not on a second transition). So no admission-lock serialization claim is made from it; the #1936 fence's own contract is pinned separately by `TestHostDispatch_AdmissionWalkInFlight_MarkerBlocksThen409`.

Tests: `decomposition_dispatch_guard_test.go` (one case per branch, plus the three window directions — `TestGuard_Window_DependencyReachesSucceeded_StillRefuses`, `TestGuard_Window_DependencySiblingMintedLate_StillRefuses`, `TestGuard_Window_DependencyLeavesSucceeded_AdmitsOnSnapshot` — driven through a deterministic `onListRuns` hook rather than sleep-based racing), `host_dispatch_test.go` (the handler refusal/admit + committed-state, `TestHostDispatch_ConcurrentDecomposedDispatch_AdmitsExactlyOne`, and the pgtest-backed cross-layer round trip whose step (c) asserts a satisfied dependency cannot be falsified after the fact), `runs_get_test.go` / `runs_list_test.go` (the run-row surface + the no-N+1 pin). The absorbing-succeeded premise itself is pinned in the run package by `TestRunSucceededIsAbsorbing` and `TestPostgres_SucceededRunNeverLeavesSucceeded`.

## Run-branch operator-vouch remediation (ADR-035 / #1044)

`vouch.go::handleVouchCommit` — route
`POST /v0/runs/{run_id}/vouch-commit`, MCP verb
`fishhawk_vouch_commit`. The operator-gated, audited provenance path
for a foreign commit on a run branch that no loop-native remediation
can route — e.g. an operator's mechanical remediation commit (a
`scripts/sync-schemas` output pushed onto a fan-out branch whose
children are all terminal with zero open concerns).

Distinct from reset (#867, which DROPS an on-top commit): vouch
**KEEPS** the operator commit and attributes it.

Mechanics:

- The handler appends an `operator_commit_vouched` audit entry
  (operator/`ActorUser` actor; payload
  `{run_id, vouched_sha, reason}`, with `vouched_sha` keyed on the
  shared `lineageVouchedSHAField` constant).
- `lineage.go::buildReportedHeadLedger` unions vouched SHAs
  (`lineageVouchLedgerCategory`, read via `addVouchedSHAs` from the
  `vouched_sha` field — parallel to `addReportedHeads`/`head_sha`) into
  the reported-head ledger on the run's OWN chain AND inside the
  per-child decomposition loop. The union therefore flows automatically
  to BOTH the #858 report-boundary check (`verifyBranchLineage`) and
  the merge-resolution re-check (`ReverifyBranchLineage`) with no
  caller edits — un-wedging the run an operator commit had parked.

Invariants:

- **Fail-closed preserved**: the handler records the declaration
  verbatim without verifying the SHA exists on the branch, so an
  UN-vouched foreign commit still fails category-B and still blocks
  resolution. A vouch read error sets `complete=false` (the ledger
  fails open at detection, matching the head-category contract).
- **Operator-token-only by design** (the ADR-035 sole-writer
  invariant): requires `write:stages`, and a run-bound
  `mcp:run:<uuid>` token is REJECTED OUTRIGHT (`run_token_forbidden`)
  — even for its own run — mirroring the #961
  `decide_scope_amendment` guard, because an agent self-declaring
  lineage for a commit on its own branch would defeat the #797/#856
  cross-write protection the vouch must preserve.
- `operator_commit_vouched` is an internal audit kind, NOT an
  issue-comment surface (the #1067 living anchor comment projects it
  via the audit chain).

## Stage terminal-wait long-poll (#1252, E24.X)

The SDK-independent REST analogue of the scope-amendment `?wait` long-poll, applied to stage settledness so a DETACHED operator-side watcher (a backgrounded shell poll) has ONE authoritative completion signal.

`GET /v0/runs/{run_id}/stages/{stage_id}` (`run_stage_wait.go::handleGetRunStage`) resolves a stage by the durable ADR-037 `(run_id, stage_id)` handle and returns the canonical `Stage` shape plus a wait envelope (`state`, `terminal`, optional `next_action`).

- `terminal` is keyed off the `run.StageState.IsSettled()` classifier (`backend/internal/run/run.go`) — true for the three terminal states (`succeeded`/`failed`/`cancelled`) AND the parked states (`awaiting_approval`/`awaiting_children`/`awaiting_input`/`awaiting_scope_decision`/`awaiting_deploy_approval`/`awaiting_host_dispatch`), false for the in-flight states (`pending`/`dispatched`/`running`/`awaiting_deployment`). `awaiting_host_dispatch` (#1912) is settled — a runner_kind-locked-`local` agent stage parked for a host/operator spawn, released by the host-dispatch marker. `IsTerminal()` is left UNTOUCHED for its narrower transition-table callers.
- Optional `?wait=<0..30>` (`parseRunStageWaitSeconds`, clamped) holds the connection via `awaitStageSettled` — a `time.After(deadline)` / `time.NewTicker(runStageWaitPollInterval)` / `r.Context().Done()` select modeled byte-for-byte on `awaitScopeAmendmentDecision` — returning the moment the stage settles, at the cap (last-read still-unsettled stage), or on client disconnect. A transient re-read error returns the last-good stage at 200 (never a 500).
- Auth mirrors `handleListScopeAmendments`: anonymous → 401; a run-bound `fhm_` token needs `mcp:read` (else 403 `insufficient_scope`) AND must match the path run (else 403 `cross_run_stage`); operator bearers pass. A stage whose `RunID` != the path run is 404 `stage_not_found` (handle consistency).
- `next_action` reuses `applyDriveSurfaces` (`runs.go`) — best-effort, omitted for non-drive/terminal runs, never fabricated.
- Composes existing repo reads only (`RunRepo.GetStage`/`GetRun`, `AuditRepo.ListForRunByCategory`); no orchestration/runner/MCP-tool contract change.
- Companion to the `dispatch_stage` durable non-blocking dispatch work (#1232) that will make the single-session in-band decision native.

## Plan-gate scope/constraint pre-check (#658)

`scope_precheck.go::runScopePrecheck` — called from `handleShipPlan` (`plan.go`) right after the `plan_generated` audit append and before `runPlanReviews`.

Evaluates the uploaded plan's `scope.files` against the run's implement-stage path constraints using the **same `backend/internal/policy` matcher as the post-implement gate**, so the plan-time verdict equals the verdict the implement stage would produce.

- `resolveImplementConstraints` mirrors `resolveStageReviewers`' spec read (parses `runs.workflow_spec`, finds the first implement stage) and flattens its `[]spec.Constraint` into a single `policy.Constraints` — keeping ONLY the scope-knowable constraints `forbidden_paths` / `allowed_paths` / `max_files_changed`.
- `required_outcomes` is deliberately dropped (`tests_added_or_updated` would false-flag any plan not enumerating a `_test.go`, and `ci_green` has no pre-implement signal).
- Writes a `plan_scope_precheck` audit entry (payload `ScopePrecheckPayload{workflow_id, implement_stage_id, violations, scanned_files}`) **even on a clean scope** (empty `violations`) so a reader distinguishes "checked and clean" from "never checked".
- Advisory + fail-open throughout: a missing/unparseable spec or a workflow with no implement stage writes no entry and never blocks/unwinds the upload (matching `runPlanReviews`' degradation contract).
- The `plan FileOperation`→`policy.Status` mapping (create→A / modify→M / delete→D) is fidelity-only — policy path checks match on `Path` only, ignoring `Status`.
- MCP surface: `fishhawk_get_plan` adds `scope_precheck` (`ScopePrecheck{violations[], scanned_files}`) decoded from the **newest** `plan_scope_precheck` entry (`loadScopePrecheck` in `tools.go`; a schema-retry re-upload writes a second entry and the latest is authoritative), so the operator sees "scope hits forbidden_paths — wrong workflow?" before approving.
- Audit-kind note in `docs/issue-comment-surfaces.md`.
- The optional hard category-D plan-stage fail on an unambiguous forbidden match is deferred — this slice delivers the advisory surface only.

## Plan-gate test sweep (#942)

`test_sweep.go::runTestSweep` — called from `handleShipPlan` (`plan.go`) immediately after `runSurfaceSweep` and before `runPlanReviews`.

Generalizes the surface sweep's static registry to the class #942 names: a plan changing behavior whose tests live in an EXISTING `*_test.go` not listed in `scope.files`, which the runner then scope_drift-excludes (silently dropping the test edit, #885) or reconciles late (#862/#876).

fishhawkd has no local checkout, so it consults the repository tree at plan time via the Contents API — `githubclient.Client.ListDirectory` (`GET /repos/{owner}/{repo}/contents/{path}`, directory-listing array shape, default-branch HEAD via empty ref; `run.Run` carries no base tree ref, so a just-advanced main yields at worst one stale advisory).

Candidate generation is **data-driven per-repo** (#1004): `evaluateTestSweep(scopeFiles, dirListings, conventions)` takes effective `[]testConvention` = built-in `defaultTestConventions` (the Go `**/*.go` → `{dir}/{name}_test.go` rule plus colocated TS) **++ the run's declared `test_conventions`** (a top-level workflow-spec array of `{match: <doublestar glob>, candidates: [<path templates>]}` with vars `{dir}`/`{name}`/`{ext}`/`{relpath}`, parsed from `runRow.WorkflowSpec` via `spec.ParseBytes`; empty/unparseable spec fails open to the defaults only).
Declared entries APPEND to the defaults (never replace), so a no-`test_conventions` spec stays byte-identical to #1003's Go+TS behavior while a repo declaring only Python/Ruby keeps Go covered.

Three deterministic rules in the pure matcher:

1. *Stem-sibling* — a scoped production file matching a convention's `match` (and not itself a recognized test file) whose expanded candidate test exists on the base ref and is absent from scope (rule id stays `stem_sibling`).
2. *New-test-in-tested-package* — a scoped CREATE whose basename is a recognized test file, in a directory that already has other recognized test files, reporting them sorted and capped at 10 names with `omitted_count` carrying the remainder.
3. The *path-trigger rule table* (`testSweepPathTriggerRules`, #1031) — curated rows of trigger glob → required paths evaluated against the scope set only (no Contents API consultation), currently one row: `migration_walk`, any scoped `backend/internal/postgres/migrations/*.sql` requires `backend/internal/postgres/postgres_test.go` (it pins the LATEST migration; planners missed it on 0029/0030/0031); `RequiredPaths` is a slice so a future row can require multiple paths per trigger.

Overlapping declared+default conventions are deduped (candidate-set per production file + findings by `(rule, trigger_path)`), so an overlap yields exactly one finding.

**NOT call-graph/behavior-coverage analysis** — a plan changing package A whose tests live in package B is out of reach by design (#942 defers that).

Bounds and degradation:

- Bounded at `testSweepMaxDirs` (20) distinct directories per upload, counted AFTER candidate expansion so parallel-tree candidate directories (`tests/`, `spec/`) are included (the rest WARN-skipped).
- Each listing failure fails open per-call, and an all-listings-failed sweep writes NO entry (never-checked, not falsely clean).
- Writes a `plan_test_sweep` audit entry (payload `TestSweepPayload{findings, scanned_files, listed_dirs}`, `findings` an empty array not null) even on a clean sweep, and additionally fails open with no entry when `cfg.GitHub` is nil or the run's `installation_id` is nil (non-GitHub triggers / unwired deployments).

The returned payload threads into the plan-review prompt's gate-evidence section as a reviewer-judged ADVISORY (not an automatic high-severity concern: judge whether the changed behavior's tests or shared harness live in the flagged files — if so the plan must scope them or the runner will scope_drift-exclude the edits).

MCP surface: `fishhawk_get_plan` adds `test_sweep` (`TestSweep{findings[], scanned_files, listed_dirs}`) decoded from the **newest** `plan_test_sweep` entry (`loadTestSweep` in `tools.go`).

## Operator-scope-undelivered pre-review signal (#1407)

`trace.go::runImplementReviews` — before building the implement-review prompt, unions the run's two operator-add provenance channels (the approval-time `add_scope_files` folds via `amendedScopeFilesForReview`, and approved mid-stage scope amendments via `approvedAmendmentScopePaths` → `ScopeAmendmentRepo.ListByRun`) and computes the subset UNTOUCHED by the committed diff (`operatorScopeUndelivered`, untouched-only: absent from `diff.ChangedFiles`; directory-prefix / non-repo-relative tokens skipped like `MissingScopeFiles`).

- A non-empty set renders a high-priority `operator_scope_path_undelivered` warning in the prompt's gate-evidence section (`prompt.GateEvidence.OperatorScopeUndelivered`, allocate-if-nil) AND appends one deterministic advisory `operator_scope_path_undelivered` audit entry (payload `{undelivered_paths, undelivered_count, operator_added_count}`) BEFORE any reviewer verdict — so a dropped operator-required edit (E23.9/E23.10) is visible pre-review instead of only at the reject→fixup round-trip.
- Advisory + best-effort: a nil `ScopeAmendmentRepo` or `ListByRun` error contributes nothing and never blocks the review; an all-delivered commit keeps the prompt byte-identical and emits no entry.
- The complementary BLOCKING gate for a FULLY-untouched concrete DECLARED scope path is the runner's #1151/#1231/#2501 scope-completeness park (`gitops.MissingScopeFiles`; the scope-completeness invariant in `docs/ARCHITECTURE.md`); this is the advisory pre-review surface for the partial / operator-added case.
- Audit-kind note in `docs/issue-comment-surfaces.md`.

## Re-review convergence: settled ledger + re-litigation guard (#1913)

`trace.go` makes implement re-review rounds converge by threading settled history forward and turning operator arbitrations into a machine-binding suppression guard (issue #1913; measured churn on runs a04d5cbf / 98704b0c).

- **Settled-ledger threading.** `settledConcernsForReview` (sibling of the OPEN-only `priorConcernsForReview`) gathers the stage's `waived`/`deferred` + `addressed`/`superseded` concerns into `prompt.Trigger.SettledConcerns`, threaded into every post-fixup round so a round-N reviewer has the full settled history (deferred arbitrations, invisible before, now reach the reviewer). Waived concerns MOVED out of `priorConcernsForReview` into this set; `hasFixupRoutedConcern` still gates the #1725 delta on `addressed_pending`, unaffected.
- **`concern_relitigation_suppressed` audit-category contract.** An internal, advisory, best-effort audit kind (system actor, payload `{settled_ref, settled_state, severity, category, note, reviewer_model, origin_review_sequence}`) written by `persistReviewConcerns` → `suppressRelitigation`/`appendRelitigationSuppressed` when a verdict concern's `settled_ref` resolves to a **same-run/same-stage/same-stageKind** `waived`/`deferred` concern AND its `new_evidence` is empty — the guard excludes that concern from the durable open-row insert and records this entry instead (so the suppression is visible, never silent). It posts NO issue comment and adds no Notifier method, so it is NOT an issue-comment surface (it is registered in `audit.KnownCategories` for `fishhawk_await_audit`). Fail-open on every other case — unparsable/unknown ref, cross-stage ref, non-waived/deferred state, non-empty `new_evidence`, and any lookup/append error (WARN) all fall through to the normal insert, so a sloppy tag never suppresses a genuine finding and a store outage never wedges the loop. A re-raise against an `addressed`/`superseded` concern is deliberately insertable (a genuine regression must reach the operator).

## Gate decision view (`gateview.go`, E48.13 / #1960)

`GET /v0/runs/{run_id}/gate-view` (`handleGetRunGateView`) answers "what is still open at this gate and why" in ONE read, replacing the `getRun` + `listRunAudit` stitch an operator otherwise runs at a review/fix-up gate. The run-status concerns block (`runs.go::buildRunConcernsPayload`) carries only a BOUNDED note-derived `short_summary` label per concern (at most 100 bytes, one line; #2488) — enough to recognise a defect, not the full prose; this surface returns the untruncated `concern.Concern.Note` intact.

- **Response shape.** Each OPEN concern (`raised`/`addressed_pending`/`reopened`) carries its FULL `note`, `severity`, `category`, `reviewer_model`, `origin_review_sequence`, a derived `round` (implement-only), `state_reason`, `has_suggested_patch`, plus `fixups[]` and `resolutions[]`. The settled ledger (`waived`/`deferred`/`addressed`/`superseded`, each with `state_reason`) and the run's `concern_relitigation_suppressed` entries ride along. `suggested_patch` diff text stays elided as `has_suggested_patch` (token-dominant, not decision prose) — the response is sized by SCOPING (the optional `stage_kind=plan|implement` filter), not truncation.
- **History is reconstructed from the immutable audit payloads**, because `concern.StateReason` is OVERWRITTEN on every transition (`MarkAddressedPending` writes the routing reason, then `applyConcernResolutions` overwrites it with the re-review note) — there is no stored per-round history. `fixups[]` join each `stage_fixup_triggered` whose `concern_ids` names the concern (contributing `{sequence, reason}`) to the outcome (`apply_path`/`head_sha`) of the earliest following `fixup_pushed`/`fixup_no_changes` (`pending` when none yet). `resolutions[]` join each `implement_reviewed`/`plan_reviewed` payload's `concern_resolutions` entries keyed by concern ID. `round` = `1 +` the count of same-stage `stage_fixup_triggered` sequences below the review sequence (the `review_action_hint.go::latestRoundConcerns` convention); the handler sorts fetched audit entries by `Sequence` defensively rather than relying on repo order.
- **Degradation is visible, never silent.** `AuditRepo` nil, or any per-category `ListForRunByCategory` error, returns 200 with the concerns intact, `history_incomplete=true`, and `history_gaps` naming each failed category; a single malformed payload entry is skipped warn-only while its siblings still join. `ConcernRepo` unconfigured → 503 `gate_view_unconfigured` (mirrors `fixup_unconfigured`); `RunRepo` unconfigured → 503 `run_repo_unconfigured`; unknown run → 404; bad `stage_kind` → 400; a `ConcernRepo.ListByRun` error → 500 `internal_error`. **Auth mirrors `handleListRunAudit`'s read posture** (full reviewer prose must not be anonymously readable, #1960 authz): a run-bound `mcp:run:<uuid>` token is authorized by the cross-run subject guard alone — it may read only its own run (403 `cross_run_gate_view`, mirroring the fix-up handler; a malformed `mcp:run:` subject → 401 `authentication_required`) — while every other caller must clear the `read:audit` scope (anonymous → 401 `authentication_required`, a token missing the scope → 403 `insufficient_scope`, cookie-session operators bypass per `requireWriteScope`).

## Deploy-record environment label (`trace.go`, E23.18 / #2324)

The deploy record's `environment` label — the `environment` field of the KindDeployment artifact body and of the `deployment_outcome_recorded` audit payload, written by `ResolveDeploymentFromPollState` and `ResolveDeploymentRollbackFromPollState` — is the environment the operator ACTUALLY approved, not one re-derived from schema ordering. A multi-environment deploy stage (`allowed_environments: [staging, prod]`) approved with `--environment=prod` previously mislabeled the deploy as `staging` (the first entry) on both surfaces.

- **Resolution order.** `deployEnvironmentForStage(ctx, runID, stageID)` returns the approved environment first (`deployApprovedEnvironment`), and the spec derivation (`deployEnvironmentForRun`) only as the FALLBACK when no explicit `--environment=` approval is recorded (the genuinely single-environment case). `deployApprovedEnvironment` reads the deploy stage's approvals (`ApprovalRepo.ListForStage`, submitted_at-ascending) and keeps the LAST APPROVE carrying an explicit `--environment=` flag — last-approve-wins mirrors the gate, where the approval that advanced the stage is the one that passed the pre-flight.
- **Structural agreement with the gate, by construction.** The deploy-stage SELECTION (`firstDeployStage`) and the allowed_environments FOLD (`lastWinsAllowedEnvironments`) are each ONE helper called by BOTH the pre-execution approval gate (`approvals.go::checkDeployPreflight`) and this record-side resolver. The record therefore cannot key on a different stage, or admit against a different allow-list, than the gate that admitted the approval. `deployApprovedEnvironment` re-checks the approved value against `lastWinsAllowedEnvironments` — the gate's last-wins fold — and a non-member value returns `""` so the record falls back rather than publishing an environment the gate would have refused.
- **Why a reject comment and a non-member value are ignored.** Only an APPROVE decision reflects a gate-admitted choice; a `--environment=` flag on a REJECT never passed the gate, and an environment absent from the stage's allow-list is one the gate would refuse — both are dropped.
- **Best-effort, silent fallback.** A nil `ApprovalRepo`, a `ListForStage` error, an absent/unparseable spec, or a workflow/deploy stage the spec does not carry all return `""` and fall back — never an error. The label is a convenience surface; `external_run_url` + `outcome` remain the authoritative outcome fields.
- **First-wins in the fallback is the #2218-characterized behavior**, retained deliberately: on a legacy duplicate-kind `allowed_environments` document the fallback reports the first entry. First-stage keying across MULTIPLE deploy stages is a shared, pre-existing behavior of the gate and the record alike (a latent defect tracked separately, out of #2324 scope); centralizing the selection here does not change or endorse it.

## Fix-up re-review backstop (#1932)

Post-fix-up implement re-review has TWO dispatch paths. The FIRST is the trace-time hook in `trace.go::advanceStageAfterTrace` (the `#793` raw-variant gate): the runner's raw trace of the fix-up carries the new diff/head, and the hook dispatches `runImplementReviews` for it. The SECOND is `succeedFixupPushStage`'s backstop (`trace.go::maybeBackstopFixupReReview`), which re-arms the re-review when the trace-time hook never fired for the pushed head.

- **Why a second path (the run-1-vs-run-3 distinction).** The trace-time hook only runs when control reaches the review block. On the observed wedge (run 98020210, audit seq 34408–34418) the retried fix-up's raw trace failed backend **policy re-evaluation as category-B** — the bundle diff spanned 79 files against `max_files_changed: 45`, a **stale-base diff** — so the handler routed to `failStageCategoryB` and never reached the hook. `#788` fix-up recovery then restored the implement stage to `succeeded` (`stage_fixup_recovered`), and the later `fixup_pushed` report (new head `5d33d25f`) recorded the head with **nothing re-arming the review**, so `implement_review_status` stayed `pending` forever and the `fishhawk_audit_complete` merge gate wedged. A DIFFERENT run whose fix-up raw trace passed policy re-eval fired the trace-time hook normally and never wedged — the category-B-on-stale-base-diff is the whole difference. The separate runner-side stale-base-diff defect (the spurious policy violation) is out of scope here; the backstop makes the re-review contract robust to ANY trace-time miss, whatever its cause.
- **Trigger.** `fixup_pushed` report with no `implement_review_started` entry for the new head (`implementReviewAlreadyStarted(started, stage.ID, headSHA)` false).
- **Four skip modes**, each fail-closed to no-second-review (a double dispatch is the worse failure — 2× cost, divergent verdicts, `#777` hint over-fire): (a) nil `AuditRepo` (started ledger unreadable — a list error skips too); (b) an `implement_review_started` entry already exists for `(stage, new head)` — the normal path where the trace-time hook already dispatched, so the backstop is a no-op and review cost is unchanged; (c) the NEWEST `implement_review_started` for the stage carries an empty `head_sha` (`newestImplementReviewStartedHead`) — an unkeyed prior round is indistinguishable from a missed one, WARN-logged; (d) GitHub client / run installation not wired (CLI/dev posture). When NO started entry exists for the stage the trace-time hook never fired at all, so the backstop proceeds (a genuine miss).
- **Delta diff source.** `githubclient.ComparePatch(base_sha, head_sha)` where `base_sha` is the `fixup_pushed` report's base (the branch head the fix-up committed onto), so the compare result IS the fix-up delta — coherent with the `#1725` delta re-review framing — mapped through `consolidatedReviewDiff`. It reuses the existing `implement_review_started` audit kind via `emitReviewStarted` inside `runImplementReviews` (NO new audit or comment surface). The backstop review carries `gateEvidence=nil` — the PR-report path has no bundle in hand, and the failed attempt's trace-time evidence would be misleading (it described a bundle that failed policy re-evaluation); nil is the documented byte-identical omit case in `prompt.Build`.
- **Dispatch shape.** Detached, shutdown-tracked goroutine (`context.WithoutCancel` + `s.bgReviews`), mirroring `DispatchConsolidatedReview`; the `runImplementReviews` `(stage_id, head_sha)` guard (`#797`) is the second line against a double dispatch, and the gating-reject return is intentionally ignored (the stage is already terminal/restored at push-report time). Called AFTER the `fixup_pushed` audit entry so a `fishhawk_await_audit` anchored on `fixup_pushed` observes the backstop's `implement_review_started` strictly after its anchor; the handler's `(stage_id, head_sha)` dedup early-return structurally prevents the backstop from running twice on a redelivered report.
- **Check-and-start atomicity across the two dispatchers.** The backstop's pre-goroutine absence check (skip mode (b)) is only a fast no-op path — it does NOT by itself make the combined check-and-start atomic against the trace-time hook. A `fixup_pushed` report arriving while the trace-time hook is between its own `#797` absence check and its `emitReviewStarted` could otherwise slip past both and double-dispatch. The load-bearing guarantee is `trace.go::reviewDispatchMu`, a process-global mutex held across the `#797` read-then-append **inside `runImplementReviews`** — where both dispatchers converge. The loser of the race observes the winner's `implement_review_started` under the lock and returns without a second review. Process-global because one backend serves both the trace upload and the PR report for a run; the critical section is a single list + append, so the coarse scope is throughput-neutral at v0 review volumes (mirrors `p95CacheMu`). A multi-replica deployment splitting the two reports across replicas would need a DB-level uniqueness guard for the durable dedup; the in-process lock is the proportionate v0 fix. `TestRunImplementReviews_ConcurrentDispatch_SingleStarted` pins it (N concurrent same-head dispatchers → exactly one started + one reviewer invocation).

## Plan-gate acceptance pre-check (#1533, ADR-049 decision #4)

`acceptance_precheck.go::runAcceptancePrecheck` — called from `handleShipPlan` (`plan.go`) alongside the sibling gates (after `runScopeRegression`, before `runPlanReviews`). The acceptance-criteria sibling of `runScopePrecheck`, shifting an acceptance-quality gap left to the plan gate.

- **Stage-conditional:** `resolveAcceptanceStage` mirrors `resolveImplementConstraints`' spec read (parses `runs.workflow_spec`, finds the first stage with `type: acceptance`) and returns `ok=false` when the spec is absent/unparseable, the workflow is missing, or it has **no acceptance stage** — so a run whose workflow does not configure acceptance produces NO entry and NO block, ever (the issue's off-switch).
- Decodes `verification.acceptance_criteria` from the **RAW plan body** with `json.Unmarshal` — deliberately NOT `plan.Parse`, whose `semanticCheck` rejects duplicate ids (`plan/validate.go`), which would fail-open a duplicate-id plan out of the pre-check before the `duplicate_id` rule could flag it.
- Deterministic rules → `AcceptanceFinding{rule, criterion_id, detail}`: `no_blocking_criterion` (no effectively-blocking criterion — `Blocking == nil || *Blocking` applying the schema default — AND empty `verification.out_of_scope`, the justified-absence escape hatch), `missing_source_ref` (explicit criterion, empty `source_ref`), `missing_rationale` (inferred criterion, empty `rationale` — defense-in-depth; the schema conditional normally rejects this upstream), `empty_id`, `duplicate_id`.
  The rule set is the ONE exported `plan.EvaluateAcceptanceCriteria` (`backend/internal/plan/acceptance_check.go`; `AcceptanceFinding` is a type alias) shared with the intake criteria pre-check — see `backend/internal/refinement/README.md`.
- Writes a `plan_acceptance_precheck` audit entry (payload `AcceptancePrecheckPayload{workflow_id, acceptance_stage_id, findings, criteria_count, blocking_count, out_of_scope_count}`, `findings` an empty array not null) **even when clean** so a reader distinguishes "checked and clean" from "never checked".
- Advisory + fail-open throughout: nil repos, a `GetRun` error, no acceptance stage, or an unmarshal error each returns without blocking/unwinding the upload; an audit-append failure still returns the computed payload.
- The returned payload threads into the plan-review prompt's `### Gate evidence` block (`planGateEvidence` → `prompt.AcceptancePrecheckEvidence`), where a finding inherits the machine-verified "recorded as a high-severity concern, named FIRST" contract. The plan artifact's criteria themselves also render in `writePlanForReview`, and five semantic checklist items (coverage, warrant-of-inferred, testability, independence, falsifiability) are appended to the `### Review criteria` block.
- Audit-kind note in `docs/issue-comment-surfaces.md`.

## Acceptance failure triage (E31.8 / #1536, ADR-049 decision #2)

`acceptance.go::triageAcceptanceFailure` — called from `handleShipAcceptance` ONLY on the fresh-create path (never the idempotent replay, so a re-delivered verdict cannot double-route) and only when `verdict==failed`. **Best-effort relative to the ship:** every internal error WARN-logs and never unwinds the `201`/artifact/`acceptance_outcome_recorded` audit.

**Pure classifier** `classifyAcceptanceFailure(acc, criteria)` → `(class, criterion_ids, reason)`:

- `failure_mode==error` → class 1.
- `assertion_fail` with a non-empty failed set where every failed id resolves to an `explicit`-source plan criterion → class 1.
- Any failed id `inferred`-source or unresolvable against the plan → class 3 (criterion_ids = those ids, the E31.11 per-criterion key).
- No failed but ≥1 skipped where at least one skip LACKS `expectation_basis` → class 2 (ambiguous env/flake).
- No failed but ≥1 skipped where EVERY skip carries a non-empty `expectation_basis` → class 5 (posture-A externally-unvalidatable can't-exhibit, #1671).
- No failed + no skips, or the plan carries no `acceptance_criteria` → class 4.
- Provenance is grounded against `loadApprovedPlanForRun` (nil-tolerant → class 4).

**Routing:**

- Class 1 synthesizes one `[high/acceptance]` `planreview.Concern` per failed criterion from the behavioral evidence (`observed`/`expected`/`steps_taken`/`expectation_basis`/`repro_handle` + the plan statement), or a single envelope concern when the verdict itemized nothing, and routes via the existing `fixupStageAs` under a token-less `Identity{Subject:"system:acceptance-triage"}` (passes `identityHasGateScope`) with `run.FixupOptions.AcceptanceStageID` set.
  The acceptance-driven mode on `run.FixupStage` re-parks the review stage tolerantly and re-opens the settled acceptance stage so the re-dispatched implement → review → acceptance chain re-runs against a fresh preview; disposition `fixup_dispatched`.
- Class 2 calls `run.ReopenAcceptanceStage` (succeeded → pending, the class-2 verb in `run/acceptance.go` — deliberately NOT `RetryStage`, which operates on FAILED stages, but a valid failed VERDICT leaves the STAGE `succeeded`) then orchestrator `Advance` + `notifyStatusUpdate`; disposition `retry_dispatched`.
- Class 3 / class 4 take NO transition; disposition `paged`.
- Class 5 (#1671) ALSO takes NO transition — disposition `externally_unvalidatable_paged`, a terminal page that keeps the acceptance stage `succeeded` so `fishhawk_audit_complete` clears rather than looping the deterministically-futile class-2 re-run; because it never re-opens the stage it never contributes to the auto-routed count.

**Bounds:** `countAcceptanceTriageRoutes` counts prior `acceptance_triage_decided` entries whose disposition auto-routed (`fixup_dispatched`/`retry_dispatched`) — the durable mirror of `countFixupPasses` — with `defaultMaxAcceptanceReruns` = 2. At the cap, or on ANY routing refusal (fixup budget/ceiling exhausted → `fixup_unavailable_paged`, reopen refusal → `retry_unavailable_paged`) or a defensive settle miss (acceptance stage not yet `succeeded` → `unsettled_paged`), the disposition degrades to a paged variant rather than acting, so non-convergence always lands on the human.

**Audit:** ONE `acceptance_triage_decided` chained entry per triage, written AFTER acting (payload `{run_id, stage_id, artifact_id, class, disposition, criterion_ids, failure_mode, prior_routed_passes, reason}`, the class/disposition/criterion_ids matching the E31.3 `renderAcceptanceTriageLine` render contract — no `status_template.go` change).

**Paging:** `issuecomment/ping.go::acceptanceTriageNeedsHuman` fires a page-class ping ONLY for the human-needed dispositions (the paged variants); the auto-routed ones stay edit-only (the fixup/retry surfaces already render). Category `CategoryAcceptanceTriageDecided`; disposition vocabulary + ping in `docs/issue-comment-surfaces.md`.

## Plan-gate surface sweep (#763)

`surface_sweep.go::runSurfaceSweep` — called from `handleShipPlan` (`plan.go`) immediately after `runScopePrecheck` and before `runPlanReviews`. Flags sibling surfaces a plan must move in lockstep with: when `scope.files` touches one path of a known multi-surface pattern but omits a required sibling, the missing sibling is recorded.

- Uses a **static pattern registry** (`var surfacePatterns`), NOT call-graph analysis — broadening to call-graph is explicitly deferred. Registry entries, keyed to cited production misses:
  - *Actor @-mention render surfaces* — `status_template.go` ⇄ `notifier.go` (#751/#755, the wrong-user-ping class), each a trigger AND sibling of the other so touching one flags the missing peer.
  - *Audit kind requires surfaces doc* — triggers `notifier.go` + `server/pullrequest.go` (audit-kind emitters), sibling `docs/issue-comment-surfaces.md` (#742/#748, per the CLAUDE.md mandate).
  - *Work-management schema requires every mirror* — `docs/spec/work-management-v0.schema.json` ⇄ `backend/internal/workmgmt/schemas/work-management-v0.schema.json` (the only two mirror copies per `scripts/sync-schemas`), added to catch the #1101/#1006-case-2 kill-switch field-add.
- `evaluateSurfaceSweep(scopeFiles, patterns)` is the pure matcher: exact slash-normalized (`filepath.ToSlash`) path equality routed through a glob-ready `pathMatches` helper, reporting only siblings ABSENT from scope (so a self-referential pattern never flags a present sibling) with `MissingSiblings` sorted deterministic; `notifier.go` alone fires BOTH the mention and doc patterns (two findings).
- Writes a `plan_surface_sweep` audit entry (payload `SurfaceSweepPayload{findings, scanned_files}`, `findings` an empty array not null) **even on a clean sweep** so a reader distinguishes "checked and clean" from "never checked".
- **Guards only `AuditRepo`** (it uses `plan.Parse` + `AppendChained`, never `RunRepo`), advisory + fail-open: an unparseable plan body or audit-append failure WARN-logs and returns without unwinding the upload.
- A `surface_sweep_test.go` test `os.Stat`s every registry trigger/sibling path so a future rename breaks loudly rather than silently disabling the sweep.

### Cross-slice coupling pass (#1102)

`evaluateCrossSliceCoupling(parsedPlan, patterns)` (pure, no `Server` receiver / I/O) runs after the per-sub-plan sweep when `parsedPlan.Decomposition != nil`.

- It is the **INVERSE** of the #1062 same-file ownership gate (`plan/validate.go::checkCrossSliceSharedFiles`, which FORBIDS two slices declaring the same path): here a registered lockstep pattern's member files (`Triggers ∪ Siblings`, slash-normalized, deduped) are partitioned across **2+ DISTINCT decomposition slices**, so completing the seam would force a later slice to modify an earlier slice's file via a runtime scope amendment that can time out (#1035) and ship the seam broken.
- It partitions over ONLY sub-plans that DECLARE a scope (an undeclared scope inherits the parent's full `scope.files` — same invariant as #1062/#1077); a single slice listing the same member twice collapses to one claimant.
- Each split pattern emits one `CrossSliceCouplingFinding{pattern, slices[]}` naming each involved slice and the member files it owns (slices sorted by title, files sorted). The fix is **consolidation** (one slice owns the whole seam, or the shared file goes to the integrating slice), not dual declaration.
- Carried in the same `plan_surface_sweep` payload field `cross_slice_findings` (empty array not null on a clean sweep).
- The un-registerable request-type/client coupling class (#1006 case 1, `product_report.go`) — which a static file-pair registry cannot express — is addressed instead by a decomposer-prompt cross-slice-seam rule (keep an end-to-end contract's files in one slice or assign the shared file to the integrating slice). Advisory + fail-open like the rest of the sweep.
- MCP surface: `fishhawk_get_plan` adds `surface_sweep` (`SurfaceSweep{findings[], scanned_files, cross_slice_findings[]}`) decoded from the **newest** `plan_surface_sweep` entry (`loadSurfaceSweep` in `tools.go`; a schema-retry re-upload writes a second entry and the latest is authoritative).

## Plan-approval pre-Submit gate: add_scope_files fan-in (#2103)

`approvals.go::checkDecomposedAddScopeFiles`, wired into `handleSubmitApproval`'s `decision==approve && stage.Type==plan` block AFTER the `remove_scope_files` normalization and BEFORE `checkPlanScopeCap`. It refuses a plan-stage approve that supplies `add_scope_files` on a **decomposed** plan (one whose loaded artifact carries `Decomposition != nil`), because `add_scope_files` is persisted as a flat `[]string` and folded into the effective scope by `resolveApprovalAddScopeFiles`, which returns the SAME parent-approval paths to EVERY decomposition child with no per-slice filtering — so an added path lands in every slice's scope, violating the single-owner-file rule `plan/validate.go::checkCrossSliceSharedFiles` already enforces for PLANNED files and producing a guaranteed add/add fan-in conflict (run bc47d2c4).

- **No override, categorical.** Unlike the override-able `checkPlanScopeCap` (`--override-scope-cap`) / `checkPlanBudget` (`--override-budget`) upper-bound heuristics, fanning an added path into every slice is never correct, so there is NO override flag and NO per-slice add channel. The 422 (`plan_add_scope_files_fans_into_slices`) `details` name every inheriting slice — `stage_id`, `add_scope_files`, `slice_count`, and `slices` (an ordered `{index, title}` list of every sub-plan) — and the refusal appends a `plan_add_scope_files_fans_into_slices` audit entry (system actor). Remediation: re-plan the decomposition so each added file is declared in exactly one slice's `scope.files`.
- **Fails CLOSED on an indeterminate plan (binding, diverges from the sibling gates).** When `add_scope_files` is non-empty the gate PASSES only when the plan positively loads AND is confirmed non-decomposed (`Decomposition == nil`). A load error or a nil/indeterminate plan is REJECTED with the same 422 (`details.reason = plan_indeterminate`), not let through — an `add_scope_files` approve must never be recorded without positive confirmation the plan is flat. This deliberately diverges from `checkPlanBudget`'s fail-OPEN-on-load-failure posture: that gate is an override-able heuristic; this one is categorical, so a transient blip must not admit the offending approval. `loadApprovedPlanForRun` returns a non-nil `*plan.Plan` for every real flat plan (a nil/error return means no plan found / a read failed, never a valid flat state), so failing closed on nil does not over-block a legitimate flat-plan approve.
- **No config-absence carve-out — the fail-closed guarantee is universal (#2103 fixup).** There is NO `ArtifactRepo`/`RunRepo`-nil fail-OPEN exception. When the plan-artifact subsystem is unconfigured `loadApprovedPlanForRun` returns `(nil, nil)`, which the gate treats as indeterminate and REJECTS (`plan_indeterminate`), since an unconfigured subsystem does not POSITIVELY confirm the plan is flat. This diverges from the sibling gates' `ArtifactRepo==nil` fail-open on purpose: they are override-able heuristics, this one is categorical. Production always wires both repos, so the config-absent branch is unreachable there; the tightening only affects a test/misconfiguration path and closes the fail-open bypass (binding condition 2 / the authz-bypass review).
- **Pre-Submit (ADR-036).** Like its siblings the gate runs BEFORE `Submit`, so a refused approve records no approval row and a corrected retry after re-planning flows normally. Only `add_scope_files` is gated; a subtractive `remove_scope_files` fan-out is harmless (removing a path absent from a slice's scope no-ops).

### The per-slice add channel (#2515)

`add_scope_files_to_slice` is the decomposed-plan counterpart of the flat add the gate above refuses: `{"<sub-plan title or 0-based index>": ["path", …]}` on `POST /v0/stages/{id}/approvals`, surfaced on `fishhawk_approve_plan`. It restores ONE dropped file into ONE slice without a full revise pass. `approvals.go::checkSliceAddScopeFiles` runs immediately AFTER `checkDecomposedAddScopeFiles` and BEFORE `checkPlanScopeCap` (categorical refusals precede the override-able cap error), and the canonical map it returns is threaded back onto the request so every downstream consumer sees one form.

- **Key resolution — title-first, then index.** An exact (trimmed) `decomposition.sub_plans[].title` match wins; otherwise the key must parse as a 0-based decimal index in range. Explicit intent beats positional coincidence, so a plan whose sub-plan title is literally `"0"` resolves a `"0"` key by TITLE. A title shared by two sub-plans is AMBIGUOUS and refused (the schema declares titles unique — this is the defensive backstop).
- **ONE canonical form.** Keys become `strconv.Itoa(index)`; each path list is trimmed, deduped, then **SORTED**. Sorting (not input order) is what makes a title-keyed request and the equivalent index-keyed request record BYTE-IDENTICAL `add_scope_files_to_slice` payloads, which is what prompt-hash replay stability rests on. The index — not the title — is the durable join key: it matches the `runs.slice_index` column the fan-out children carry.
- **Ownership is CONTAINMENT, not string equality (`scopePathsOverlap`).** A path on this channel may be a DIRECTORY (trailing slash), so slice A owning `pkg/foo/` while slice B is handed `pkg/foo/inner.go` passes every equality check yet stages one file in two slices. Overlap therefore means identical OR ancestor/descendant, compared on path SEGMENT boundaries after trailing-slash normalisation — so `pkg/foo` does not spuriously conflict with the sibling `pkg/foobar`. The same comparison is applied in BOTH directions: across keys within one request, and against the plan's own per-slice `scope.files`.
- **Enumerated refusals, all pre-Submit (no approval row, no audit entry).** `422 plan_slice_add_scope_files_requires_decomposed_plan` — the ONE new error code — for a positively FLAT plan (`details.reason` `plan_not_decomposed`, pointing at plain `add_scope_files`) and, FAIL-CLOSED, for a plan whose decomposition status cannot be positively confirmed (`plan_indeterminate`: a load error, a nil plan, or an unwired artifact subsystem — the same universal no-carve-out posture as `checkDecomposedAddScopeFiles`, and the same deliberate divergence from the fail-open sibling gates). Everything else reuses `400 validation_failed` with `details.field = add_scope_files_to_slice`: an unresolvable key, an ambiguous key, two keys resolving to one slice, a path under two keys, a path overlapping another slice's declared scope, a non-repo-relative path, and an empty path list. Error details list slices in DECLARED index order (never Go map order), so a message is byte-stable across runs.
- **It ADDS; it does NOT MOVE — and says so.** The motivating incident (#2515) was a file already in the plan's total scope that needed to sit in a DIFFERENT slice (`README.md` owned by slice 1, wanted alongside `tools.go` in slice 2). That case is still REFUSED, deliberately: a move has different semantics (the owning slice's plan would reference a file it no longer owns) and is tracked separately. The refusal is made actionable rather than left to be discovered at a gate — the message and `details` name the owning slice (`owning_slice`, `owning_title`, `owning_path`), the semantic (`channel_semantic: add_not_move`), the overlap kind, and the remedy (re-plan the decomposition so the path is declared in the slice that needs it).
- **Recordable from the PLAN STAGE alone.** `checkSliceAddScopeFiles` runs only inside the `approve && stage.Type == plan` branch, so the canonical map it returns is carried in a LOCAL declared outside that branch and assigned only inside it — never written back onto the decoded request (which is what `remove_scope_files` does). The request outlives the branch and feeds `approveActionParams` unconditionally, so a write-back would let a direct HTTP approve of a NON-plan stage (implement, review, deploy) on the same run record a RAW, un-canonicalised, un-validated map — including the non-repo-relative paths `isRepoRelativePath` refuses at the gate — on an `approval_submitted` row. `loadApprovalSliceAddScopeFiles` scans by run + category with NO stage-type filter, so numeric-keyed entries from such a row would fold into a decomposed child's implement scope, bypassing every refusal above. The field is IGNORED (not refused) off the plan stage: that approve still returns 200, it just records nothing on this channel.
- **Cap arithmetic.** The flattened union of the canonical per-slice paths rides into `checkPlanScopeCap` alongside `add_scope_files`, and `effectiveScopePathSet` folds a PRIOR approval's recorded per-slice paths, so the number the gate reports stays equal to the scope the prompt builder assembles. A run with no per-slice map produces a byte-identical set.
- **Fold — one selection, one slice.** `prompt.go::resolveApprovalSliceAddScopeFiles` returns paths only when `DecomposedFrom != nil && SliceIndex != nil`, reading the run's own rows then the parent's, and selects solely `m[strconv.Itoa(*SliceIndex)]`. That single selection IS the single-owner-file guarantee: folding the whole map would reproduce the exact add/add fan-in the #2103 refusal names. It is unioned into `resolveApprovalAddScopeFiles` — the documented SOLE fold source — rather than at a new call site, so the enforced implement scope, the agent-facing shown set, the reviewer drift baseline, and trace.go's provenance sets all inherit the channel with no site to miss. A nil `SliceIndex` (a plan-stage-less recovery child of a decomposed parent, #2027 case 1) folds NOTHING: folding the union would widen its scope beyond what any single slice was granted. An out-of-range persisted index likewise folds nothing rather than falling back to another slice.

### Plan-stage-only recording, all three channels (#2598)

The "recordable from the PLAN STAGE alone" property above is now held UNIFORMLY by the three approve-time scope channels — `add_scope_files` (#824), `remove_scope_files` (#1726), and `add_scope_files_to_slice` (#2515) — plus a loader-side second wall.

- **Recording side (the control), absolute by construction.** All three values are carried in LOCALS (`addScopeFiles`, `removeScopeFiles`, `sliceAddScopeFiles`) declared outside the `decision == approve && stage.Type == plan` block and assigned ONLY inside it. Nothing is written back onto the decoded request: `req` outlives the block and feeds `approveActionParams` UNCONDITIONALLY, so the previous threading (`req.AddScopeFiles` verbatim; `req.RemoveScopeFiles = trimmedRemove` written back) let a direct HTTP approve of a NON-plan stage on the same run record raw, un-trimmed, un-validated lists on its `approval_submitted` row — no gate validates either flat channel outside this block, and `isRepoRelativePath` is never applied to the flat add at any stage. The removal channel is the sharper exposure: `validateRemoveScopeFiles`' would-empty-a-non-empty-scope refusal is plan-block-only, so a non-plan approve naming every scope path emptied the effective scope and re-enabled the runner's `git add -A` fallback, disabling scope enforcement. `req.AddScopeFiles` / `req.RemoveScopeFiles` survive only as the INPUT to the in-block gates. All three fields stay IGNORED (not refused) off the plan stage — that approve still returns 200, it just records nothing on these channels.
- **Loader side (defence in depth), best-effort but decisive for every server-written row.** `prompt.go::approvalEntryStageIsPlan` decodes the payload's `stage_id`, resolves it via `RunRepo.GetStage`, and is called from `loadApprovalAddScopeFiles` / `loadApprovalRemoveScopeFiles` / `loadApprovalSliceAddScopeFiles` AFTER each loop's `decision == approve && len(field) > 0` candidate test — so a lookup is paid only for a candidate row, and a dropped row lets the scan continue to an older legitimate plan-stage row. It exists to neutralise rows a PRE-fix non-plan approve may already have written. Filtering inside the LOADERS (not at the `resolve*` layer) is load-bearing: `scope_headroom.go::effectiveScopePathSet` calls two of them directly, so the number the cap gate reports stays equal to the scope the prompt builder assembles.
- **The filter FAILS OPEN, deliberately.** It drops an entry only on POSITIVE confirmation of a non-plan recording stage (GetStage succeeded, the resolved type is non-empty and != `plan`). Five enumerated branches fold as before with a WARN: nil `RunRepo`, no `stage_id` key, an unparseable `stage_id`, a `GetStage` error, and a resolved stage with an EMPTY type. Fail-closed would drop legitimate folds for every hand-built or legacy audit row that omits `stage_id`. In practice those branches are reachable only by synthetic rows: `writeApprovalAudit` (and its slash-command sibling `writeSlashApprovalAudit`) stamp `stage_id` unconditionally as the first key of every `approval_submitted` payload, so for a real approve the filter is decisive.

## Change-author attribution and `not:` enforcement (#2358)

Two independent defects combined to lock the only eligible approver out of their own plan gate. Both are fixed in `quorum.go` + `approvals.go`. This corrects an **attribution error**, not a security hole: the old behavior refused approvals it should have permitted; it never permitted one it should have refused.

**Defect 1 — attribution.** `resolveChangeAuthor` returned the `ActorSubject` of the run's earliest user-kind audit entry of ANY category. But nearly every user-kind row Fishhawk writes on a run is an operator GATE VERB, not authorship. Live evidence: run `e288e92b`, audit sequence **48558**, category `run_auto_driven` — `fishhawk_drive_run`'s record-before-dispatch attribution row (`autodrive_http.go`, #1961), a machine dispatching a stage on the operator's behalf before the agent has written a line. That row named the operator the change author and 403'd their own approve with `approver_is_change_author`. The issue-body case is the same shape with `clarification_answered`. Since `fishhawk_drive_run` is the documented post-E48 operator playbook, the DEFAULT path wedged at its plan gate. The operating rule was: whichever verb writes the first user-kind entry poisons every approval after it.

The fix inverts the rule into a closed ALLOW-LIST, `authorshipCategories`, seeded with exactly one member: `CategoryOperatorCommitVouched` (`vouch.go`) — the operator's audited declaration that a foreign commit belongs to this run's lineage, i.e. the issue's own definition of authorship ("the identity that pushed the commits"). Every unlisted user-kind category is skipped.

- **The bar a future candidate must clear:** the category must record a HUMAN putting change CONTENT into the run — not participating at a gate. Governance verbs (`run_auto_driven`, `clarification_answered`, `approval_submitted`, scope-amendment decisions, concern waivers/deferrals, merge verdicts, retries, branch resets) are gate participation and never qualify. A new gate verb needs NO change here; a new content-authoring category MUST be registered.
- **Allow-list, not a governance deny-list:** the audit category set grows with every verb added, so a deny-list re-opens this wedge on the next one. The allow-list's failure mode is "no author resolved" — the already-supported fail-open branch. The residual risk (a future content category silently not counting as authorship) is over-permissive relative to intent, never a false refusal.
- **Keyed to the exported constant, never a string literal**, so a rename that misses the map is a compile error rather than a silent behavior change.
- **Fail-open ladder** — every rung skips only the author leg; the agent floor and the quorum count are unaffected: nil `AuditRepo` → unresolved; `ListForRun` error → logged + unresolved; no authorship-category entry → unresolved; an authorship-category entry that is agent-kind or carries an empty `ActorSubject` → skipped (a later valid vouch still wins).
- **Honest consequence:** at a PLAN gate no commit has been vouched, so author separation-of-duties does not bite there at all. The plan-gate controls are the unconditional agent floor and the distinct-human quorum count. The classic "you cannot approve code you pushed" control survives via `operator_commit_vouched`.

**Defect 2 — the inert `not:` field.** `spec.Approvals.Not` was parsed, schema-closed to the `[author, agent]` enum, and documented in the v1/v2 references and the preset doc, but read by NO backend code — the only non-test readers were the migration codemod (`cli/internal/spec/migrate.go`, `migrate_report.go`). The 403 fired purely on "the gate has an approvals block", so `not: [agent]` and `not: [author, agent]` behaved identically: a declared `not:` was grammar, not enforcement. `approvalsExcludeAuthor` now reads it, threaded through BOTH enforcement points:

- **The pre-Submit 403** (`approvals.go`): a gate whose `not` omits `author` skips author resolution entirely (one fewer `ListForRun`).
- **The quorum count** (`countDistinctEligibleApprovers`, via `approveStageAs`). Threading only the 403 is the easy omission and is load-bearing to avoid: a permitted author's approval would be RECORDED and then not COUNTED, leaving quorum permanently one short — the same wedge presenting as a stuck gate rather than a clean 403, and strictly harder to diagnose than the bug being fixed. Both reads parse the same immutable cached spec bytes off the same run row within one request, so the two can never disagree on `not`.
- **`changeAuthor` is resolved UNCONDITIONALLY on the `approveStageAs` path** and only the EXCLUSION is gated, so `predicate_snapshot.submitter_class` keeps its provenance meaning. A permitted author is still labeled `author` — a combination with `quorum_reached: true` that was impossible before. Do not infer eligibility from that label.
- **The AGENT leg stays an unconditional floor** per #1709's binding acceptance criterion: an automated identity never satisfies a human quorum whether or not `not` names it. There is deliberately no `approvalsExcludeAgent` that could turn it off. The asymmetry is documented at the definition rather than left to be inferred.

**Behavior change, intended.** A repo whose gate carries an approvals block but omits `author` from `not` was silently getting author separation-of-duties anyway; it now loses an exclusion it was never declared. All three shipped presets declare `not: [author, agent]`, so preset-derived repos are unaffected.

**Pinning tests** (`quorum_test.go`, `approvals_test.go`): `TestResolveChangeAuthor` (per-category subtests — `run_auto_driven` / `clarification_answered` / `approval_submitted` / `scope_amendment_decided` → UNRESOLVED, a vouch alone → RESOLVED as the positive control, governance-then-vouch → the VOUCHED subject proving earliest-match runs over the FILTERED set, agent-kind vouch → UNRESOLVED, empty subject skipped, nil-repo and list-error fail-opens); `TestApprovalsExcludeAuthor`; `TestEligibleApprover_AuthorLegConditional` (the agent floor asserted in BOTH `excludeAuthor` permutations); `TestCountDistinctEligibleApprovers` (author counted when `excludeAuthor=false`, dropped when true, delegated exclusion holding in both); and five end-to-end cases driving the real `POST /v0/stages/{id}/approvals` — `TestSubmitApproval_Quorum_AutoDrivenActorIsNotAuthor` (the live regression), `_ClarificationAnswererIsNotAuthor`, `_NotOmitsAuthor_AuthorAdvancesStage` (asserts the ADVANCE, so it fails if the count half is dropped), `_AgentFloorIsUnconditional` (on a gate whose `not` is `[author]` only — it OMITS `agent`, so the test can fail for the reason it exists), and `TestSubmitApproval_LegacyGate_VouchedAuthorAdvancesFirstVote` (named for what it asserts — that the author leg does not ENGAGE on a legacy gate; it deliberately does not claim to pin the absence of author *resolution*, which the body does not observe). The three previously-vacuous controls (`TestSubmitApproval_Quorum_AuthorRejected`, `TestV2ApprovalsPredicateDecidesEligibility`, and the `seedQuorumStage`/`seedPredicateStage` fixtures) are re-seeded with `operator_commit_vouched` entries — a control that no longer bites must not keep a green test claiming it does.

## The approve advance is a compare-and-swap (`approvals.go`, E50.15 / #2656)

`advanceStage`'s approve leg applied the transition with the non-CAS `RunRepo.TransitionStage`, and `postgresRepo.transitionStage`'s row-locked `from == to` short-circuit returns a **silent success**. So an approval racing one that had already advanced the stage was told it succeeded and walked straight into `finishApprovalAdvance`'s post-approval tail — `fileSplitProposalChildren` and `fileOrLinkLiveValidationWalk` — filing duplicate work items. `advanceStage` now takes the loaded `*run.Stage` and routes the approve through `casTransitionFromObserved`.

- **Anchored on the OBSERVED state, not on the literal `awaiting_approval`** (operator binding condition, #2656). The CAS `from` is `stage.State` — the state this caller loaded — so it means "nothing changed since I looked". That closes the race identically (a concurrent flip changes the state, the compare fails, the loser is refused) while keeping every transition the endpoint admits today admissible: no currently-succeeding approve of a non-parked stage turns into a 409, and no external consumer sees a 200 become a 409.
- **Capability degradation, not a hard dependency.** `casTransitionFromObserved` runtime-asserts `run.StageCASTransitioner` — the same convention as `markStageRunningOnPromptFetch` (`prompt.go`) and `handleHostDispatch` (`host_dispatch.go`). The production `postgresRepo` implements it; an in-memory `RunRepo` that does not degrades to the plain `TransitionStage`, i.e. today's behavior, with no panic.
- **Both hooks are protected by construction.** The loser's `run.StageStateChangedError` is wrapped as `*approveActionError{failedAt: gateActionAdvance}` and returned from `finishApprovalAdvance` BEFORE `recordDrivePlanApproved` / `fileSplitProposalChildren` / `fileOrLinkLiveValidationWalk` / `recordPlanPredictedRuntime` / `notifyPlanReady` / `notifyStatusUpdate`. No new early return was needed.
- **The deploy pre-execution leg gets the same treatment**, anchored the same way (`awaiting_deploy_approval → dispatched` when that is what the caller observed). It is the sharper of the two: the hook it guards is an EXTERNAL delegating-pipeline fire, so a silently-succeeding second advance means a duplicate release trigger.
- **The reject leg is deliberately unchanged.** `run.FailStage` already CASes internally (`run/failure.go`, re-anchoring on the actual state) and refuses a terminal stage via `ValidStageTransition`.
- **The raced loser keeps its approval row and its `approval_submitted` audit entry** (`Submit` and the audit write both precede the advance) and receives `409 invalid_state_transition` with `details.stage_id`, `details.from` (observed) and `details.state` (drifted actual). That is the same shape the endpoint already produced for an `InvalidTransitionError` advance failure, so it introduces no new inconsistency class. The `#986` same-subject duplicate submission is untouched — it returns before the advance, still `200` with `duplicate_submission=true` and no hooks.
- **Second consumer: the slash-command path** (`issue_approval.go`) is on the same primitive, so no silent-success hole is left behind on a live surface. A state-changed advance there logs at WARN (not ERROR) and replies that the gate was **already decided**, naming the state the stage is now in — a superseded approver's action did not fail. Every other advance error keeps today's ERROR log and generic reply text, and the reply-comment (`silent`) channel stays silent on both.
- **What the CAS does NOT close, deliberately.** Because the anchor is the observed state, a strictly SEQUENTIAL re-approval — a second approver whose request loads the stage AFTER the first advance completed — still compares equal (`succeeded → succeeded`), short-circuits, and re-enters the hook tail. Refusing that would be exactly the 200→409 narrowing the observed-state anchor exists to avoid. It is not the duplicate-filing defect in practice: `fileSplitProposalChildren` no-ops on its prior `split_children_filed` completion marker and `fileOrLinkLiveValidationWalk` no-ops on its `live_validation_walk_intent` marker, so the sequential re-entry files nothing — a claim now pinned by `TestApproveAdvanceCAS_SequentialFreshLoadReapprove_MarkerDedupFilesNothing`, which asserts the hook was RE-ENTERED (a second read of the idempotency guard) and that it filed nothing on either path, rather than leaving the marker-dedup guarantee to this paragraph. The CONCURRENT case is the one those durable-marker guards cannot catch (`fileSplitProposalChildren`'s guard is an unlocked list-then-append) and is what the CAS closes.
- **Pinned by** `approve_advance_cas_test.go`: the concurrent two-approver end-to-end (per-filing-path exactly-once — 3 split children, 1 walk — plus the loser's typed error), the sequential fresh-load re-approval's marker dedup, the stale-observation refusal (error identity AND committed state), the HTTP 409 rendering, the non-CAS degradation, the `#986` duplicate submission (asserted on three gate seams: no further transition attempt, no further run-row read, no second `approval_submitted` row), the deploy leg (a first approve that dispatches exactly once, THEN a raced second that is refused without re-firing, plus its 409 rendering), the untouched reject leg, and the three slash-reply branches.
- **Why the concurrent test rendezvouses TWICE.** Synchronizing only the transition entry leaves the deleted-CAS counterfactual interleaving-dependent: both approvals clear the silent-success transition and then race unsynchronized into the hooks, and a scheduling in which the first writes its `split_children_filed` marker before the second reads it makes the second no-op and the deletion come back GREEN (measured: 3 of 10 runs reddened). So the test also rendezvouses INSIDE `fileSplitProposalChildren`'s list-then-append window, via a gated audit repo that performs the marker read and then blocks — that window IS the race the CAS closes. Departures count toward the release condition, so in the CAS-present world (where only one approval reaches the guard) the refused loser's return releases the winner: no timeout, no wall clock. With both rendezvous the counterfactual reddens on every run, with 6 split-child filings against the expected 3.

## Slash-command approval (#238)

`/fishhawk approve` and `/fishhawk reject` in the triggering issue's conversation submit a gate decision against the run's currently-awaiting-approval stage — closing the loop entirely in the issue surface.

- `webhook.matchIssueComment` parses the three commands (run / approve / reject) and tags the `Match.Action` accordingly; the `Match.CommentBody` carries any trailing reviewer rationale. `Dispatcher.Handle` routes Action ∈ {approve, reject} to `Dispatcher.handleApprovalCommand`, which delegates to the `ApprovalCommandHandler` interface.
- **Implementation lives on `Server.HandleApprovalCommand`** (`issue_approval.go`), where the approval, role, and stage-check repos already live.
  It (1) finds the awaiting-approval stage by listing runs for the repo and matching `trigger_ref = issue:N`, (2) authorizes the sender via the existing `role.Resolver.CanApprove`, (3) submits via `approval.Repo.Submit` with `Surface = SurfaceGitHubComment`, (4) advances the stage (`advanceStage(ctx, stage, …)` — the same compare-and-swap primitive the HTTP path uses, see above), (5) writes the same `approval_submitted` audit row the HTTP path writes, (6) calls `Orchestrator.Advance`, and (7) posts a reply comment via `Notifier.NotifySlashApprovalReply`.
- Replies are NOT deduped — every command attempt produces its own reply.
- **Over-cap approve-comment refusal + its breadcrumb (#2583, made observable by E67.24 / #2621).** An approve comment longer than `prompt.MaxApprovalConditionBytes` is refused BEFORE `Submit` (no approval row) on this channel too, because the comment is injected verbatim as a binding approval condition (#558) and must not be silently truncated. A REJECT is never refused — its comment feeds the advisory `PriorRejectionFeedback` channel, not binding conditions. On the reply-comment (`silent=true`) variant the refusal deliberately posts **NO reply**: a passer-by `+1` that happens to exceed the cap should not draw an unsolicited bot comment. BOTH variants append an `approval_comment_refused` audit entry (`CategoryApprovalCommentRefused`, registered in `audit.KnownCategories`) so the drop is visible in the run record rather than invisible — payload `{stage_id, decision, surface, approver, silent, reason:"over_cap_approve_comment", bytes, max_bytes, overflow_bytes}`, stage-anchored, actor `user`. The entry is a trace, not a response: the silent channel stays silent. Best-effort — an append failure WARN-logs and never unwinds the refusal into an admission.
- Per #253 / ADR-017 the slash command no longer reads `stage_check` state — branch protection is the merge gate.
- The slash-command path falls open when its deps aren't wired (`approvalCommandConfigured()` returns false), and is best-effort throughout: a comment failure logs but never returns 5xx to GitHub.
- PR-triggered approvals, `/fishhawk cancel`, and per-stage targeting (`/fishhawk approve plan`) are out of scope — separate followups when those scenarios surface.

## Live-validation walk auto-filing (`live_validation_filing.go`, E48.35 / #2045)

`fileOrLinkLiveValidationWalk`, a best-effort side effect of `finishApprovalAdvance` (fired on a plan approval, same posture as `fileSplitProposalChildren`): when the approved plan carries any `requires_live_validation` acceptance criterion, it auto-files an operator-validation walk `chore` work item and records a durable audit marker so the pending live check is surfaced on run status / gate view rather than shipped silently unvalidated. Every forge / work-item / audit error logs and returns — it NEVER unwinds the approval. A plan with no marked criterion no-ops with zero side effects.

- **Intent-marker-before-file idempotency + per-run lock.** A durable `live_validation_walk_intent` marker is appended BEFORE the forge call; on entry, if ANY intent marker is already present the hook no-ops (never a second walk). The list-then-append guard is non-atomic, so `lockLiveValWalk(runID)` serializes the intent-check → intent-append → file → linked-append section per run — two concurrent approvals of the same run can't both pass the guard and double-file. In-process only (single-daemon v0); a multi-instance deployment needs a Postgres advisory lock, mirroring `childNumberLocks` in `workitems.go`.
- **MANUAL RECOVERY — stranded intent marker (binding condition A(2), #2045).** The idempotency guard leaves one unhandled terminal state: if the process dies (or the forge call is never reached) BETWEEN the intent-marker append and the filing, the newest marker is a bare intent marker with no linked marker following it. The hook deliberately does NOT re-file it — a bare intent marker cannot distinguish crashed-before-file from filed-but-linked-write-failed without a forge query this hook avoids, and re-filing would reopen the double-file window. This rare case degrades to the pre-#2045 status quo: `liveValidationForRun` renders the stranded intent as the file-manually variant (`filing_failed=true`, `filing_incomplete=true` — never a healthy `walk: #X` and never a malformed empty ref), and the **operator files the walk by hand**. A filing-failure linked marker (`filing_failed=true`, empty ref) renders identically.
- **Companion-link, not epic-parent (implement-review fix).** The originating issue asks for the walk filed "under the epic", but the hook only knows the TRIGGERING issue — an E48.35 CHILD, not the E48 epic. Parenting the walk to that child is the wrong hierarchy, and resolving the child's own parent-epic issue number would need a sub-issue-PARENT forge query `githubclient` does not expose (only children-direction `ListSubIssues`). So the walk COMPANION-LINKS to the triggering issue with an explicit `{epic}=<issue>`/`{n}=1` title that always renders (`[E<issue>.1]`, mirroring `split_filing.go`), which neither mis-parents nor collides with the real epic's child numbering. Filing under the true epic is a **follow-up** (it needs that parent-resolution query).
- **Single deterministic filing.** One filing, not an epic-parented-then-companion-fallback pair: the two-attempt design opened a same-approval double-file window (a provider 502 AFTER the first `File` created the issue would still trigger the second attempt; the intent marker only dedupes ACROSS approvals). Any error now routes to the `filing_failed` linked marker, never to a second differently-shaped walk.

## Run lifecycle endpoints

### Run CRUD handlers (`runs.go`)

`backend/internal/server/runs.go`; wired in `backend/cmd/fishhawkd/serve.go` from `FISHHAWKD_DATABASE_URL`. POST/GET/list/cancel for runs.

- **Idempotency (E8.2)**: POST accepts `Idempotency-Key` — same `(repo, key)` returns the existing run with 200 instead of creating a duplicate. Webhook-driven runs use the dedicated dedup path (E3.9) and don't carry a key.
  The lookup sits between validation and admission (#2366): request/spec validation runs AHEAD of it (a replayed malformed body keeps its 400/422 — and a replayed create whose GitHub spec fetch now fails still gets that failure), while the three audit-emitting admission gates — plan-reviewer capability, blocking budget, `applies_to` — run BEHIND it, so a replay re-evaluates no governance or spend decision and appends no audit entry. Same seam as `handleRecoverRun` ("replay is not new spend") and `handleCreateCampaign`. This is governance-chain hygiene, not a control hole: the pre-#2366 ordering duplicated `run_admitted_applies_to_override` / `run_admitted_budget_override` grants on a retry rather than admitting anything it should have refused.
- **Runner provenance (ADR-022 / #388 + E22.7 / #404)**: every run row carries `runner_kind` (`github_actions` | `local`), assigned by the backend at run-create time — the dispatcher stamps `github_actions`, the local-runner CLI (Phase C of E22 / #389) stamps `local`. The runner never self-declares (its claim would be unverifiable).
  The `trace_uploaded` audit payload echoes the field from the run row so compliance consumers can filter audit history by backend; `GET /v0/runs?runner_kind=` filters the list endpoint the same way.
  Migration 0024 added the column with `DEFAULT 'github_actions'` so legacy rows tag correctly.
- **API-side stage creation (#411)**: `POST /v0/runs` accepts an optional `workflow_spec` (YAML body) so API-minted runs get one Stage row per stage definition — matching the dispatcher's behavior.
  Spec → stage row mapping is shared with the dispatcher via `webhook.CreateStagesFromSpec` (extracted from `dispatcher.createStages` in the same PR).
  Bytes are cached on `runs.workflow_spec` so the trace handler's policy re-evaluation reads constraints from storage; `runs.max_retries_snapshot` is populated from the parsed spec.
  `RequiredChecksSnapshot` is deliberately skipped for API-created runs (no installation token to query GitHub branch protection).
- **CLI side**: `fishhawk run start` discovers the local spec by walking up from `--working-dir` to the `.git` boundary (or honors `--spec-file`), pre-parses via `cli/internal/spec`, and computes the git blob SHA in-process; `--workflow-sha` overrides the computed SHA for historic runs.
- **GitHub-fetch fallback (#413)**: when `workflow_spec` is omitted AND the backend has a GitHub client configured, the handler calls `githubclient.GetRepoInstallation` to resolve the App's installation for the repo, then `GetWorkflowSpec` at `workflow_sha` — covers MCP-driven runs and cross-repo CLI flows that can't easily ship the spec inline.
  `ErrNotInstalled` surfaces as 422 `repo_not_installed`; `ErrNotFound` surfaces as 422 `spec_not_found`; the rest of the path is byte-identical to the inline-spec branch.
  When `workflow_spec` is omitted AND no GitHub client is configured, the run row is created with no stages (legacy shape, kept for integration-test seeding).
- **Local-runner issue context (#415)**: for runs minted outside the webhook flow with `trigger_source=github_issue`, the CLI's `--issue <number-or-URL>` flag shells to `gh issue view --json title,body,url,number` and ships the payload inline as `issue_context`; the backend persists it to migration 0025's new `runs.issue_context` JSONB column.
  `prompt.fillIssueContext` reads the cached payload first (no GitHub call needed), falling back to the existing installation-token fetch only when the row carries an `installation_id` and no cache.
  Missing or unauthed `gh` warns to stderr and proceeds with the URL-only prompt — the pre-#415 degraded shape — rather than failing the verb.
- **Local-runner issue comments (#416)**: write-side counterpart to #415. The backend's `IssueNotifier` is a no-op for local runs (the existing `contextForStatus` nil-installation_id branch covers it), and the CLI posts comments via the operator's authed `gh` after each state-changing verb — `run start` (kickoff), `plan approve` / `plan reject` (decision), `run cancel`, `runner start` (stage complete).
  Renderers live in `cli/internal/ghcomment`; the post step shells to `gh issue comment <N> --repo <owner/name> --body …`.
  v0 scope is append-only — each transition gets a new comment rather than editing a sticky one (edit-in-place deferred).
  Comments are authored by the operator's GitHub identity, not the Fishhawk App — a deliberate split that mirrors who actually triggered the run.
  Failure handling is best-effort: missing `gh` or a failed post warns to stderr and the run continues normally. Full inventory: `docs/issue-comment-surfaces.md`.
- **Blocking budget admission**: `handleCreateRun` calls `budget_admission.go::checkBlockingBudget` after the `Idempotency-Key` replay lookup and before `CreateRun` (#2366) — documented with its shared decision core in `backend/internal/webhook/README.md`.

### Re-drive — run-level reopen (`redrive.go`, #698)

The operator recovery action for a decomposition parent parked in `awaiting_children` (see the "Park-on-retryable" entry in `docs/ARCHITECTURE.md` §10).

- `POST /v0/runs/{run_id}/redrive` (`backend/internal/server/redrive.go`) calls `run.RedriveChild` (`backend/internal/run/redrive.go`), which validates the run is a failed decomposition child (`DecomposedFrom != nil && state == failed`), resets its failed implement stage `failed → pending` via `RetryStage`, and reopens the run `failed → running` via the `RetryRun` primitive, then hands off to `Orchestrator.Advance`.
- Un-terminal-ing the run is mandatory — `Advance` no-ops on a terminal run.
- `RetryRun` mirrors `RetryStage`: `transition.go` keeps a separate `runRetryTransitions` table (`failed → running`) consulted only by `ValidRunRetryTransition`/`RetryRun`, so the normal `ValidRunTransition` invariant ("terminal runs are terminal") stays true; the postgres impl reuses the plain `UpdateRunState` query (runs carry no failure metadata to clear, so no sqlc regen).
- **Auth**: re-drive requires the operator retry scope (`write:stages`/`write:retries`) AND rejects any MCP/agent subject-bound token outright (`403 agent_token_forbidden`) — an agent may not re-drive any run.
- Writes a `child_redriven` audit (user actor + prior implement-stage failure category/reason); the parked parent reconciles on the re-driven child's next terminal transition through the unchanged `maybeAdvanceDecomposedParent` path.

### Revive — one-verb failed-run re-park (`revive.go`, #1915)

The operator recovery action that re-admits ANY terminal-`failed` run for another turn — the single verb that replaces the retry-without-dispatch dance (retry each failed stage, then remember NOT to dispatch).

- `POST /v0/runs/{run_id}/revive` (`backend/internal/server/revive.go`) calls `run.ReviveRun` (`backend/internal/run/revive.go`), which **pre-validates** that the run is `failed` AND that EVERY failed stage is retryable (`run.RetryableFailure`) BEFORE any mutation, then re-parks each failed stage via the existing `run.RetryStage` per-category targets (A/C → `pending`, D-`sla_timeout` → `awaiting_approval`, decomposed-parent implement → `awaiting_children` per #1891) and reopens the run `failed → running` via the same `RetryRun` primitive re-drive uses.
- **No-partial-mutation *pre-validation***: a single non-retryable failed stage (category-B, D-rejected, or a stage with no recorded category) refuses the WHOLE revive with `422 revive_not_applicable` naming the blocking stage — nothing is re-parked, the run stays `failed`. A run in any non-`failed` state (`runRetryTransitions` admits only `failed → running`) refuses the same way. This guard runs BEFORE any mutation.
- **Post-validation partial-failure window + resumable partial state (#1942).** The re-park batch plus run reopen are NOT one transaction: each `RetryStage` and the closing `RetryRun` open their own row-locked transaction, so a mid-batch failure — an infra error or a concurrent guarded transition — can leave the run `failed` with SOME stages re-parked. This is deliberate (a cross-method atomic revive would need a tx-scoped `Repository` refactor out of proportion to the window); every intermediate state is an individually valid state-machine state, and a **second revive is the idempotent compensation**:
  - *Mid-batch `RetryStage` failure* — earlier stages re-parked, later ones still `failed`, run still `failed`. A second revive collects the REMAINING failed stages and re-parks them (already-re-parked stages are no longer `failed`, so no budget is double-consumed).
  - *Tail `RetryRun` failure* — every failed stage re-parked (zero failed stages) but run still `failed`. A second revive takes the **interrupted-revive resume branch**: it finds zero failed stages plus at least one stage in a pre-dispatch park state (`pending`/`awaiting_approval`/`awaiting_children`) and completes the reopen via `RetryRun` alone, returning `resumed:true` with an empty `restored_stages` and NO budget bumped again. Any OTHER zero-failed-stage shape (all `succeeded`, a `running` stage) keeps the `422 revive_not_applicable` refusal, so the resume branch cannot reopen an arbitrary inconsistent run. Both post-validation failure sites wrap their error with a "run left partially re-parked; a second revive resumes from here" hint (a `500 internal_error`), so the endpoint's error self-documents the recovery.
- **No dispatch — the semantic difference from `/retry` and `/redrive`.** Revive performs NO `Orchestrator.Advance` and writes NO drive `retry_reopen` stamp: it re-parks only. Each re-parked stage sits in its pre-dispatch state until the operator dispatches it at its proper gate turn via the existing verbs. Because no `Advance` fires mid-revive, the #1700 wrong-order re-dispatch corruption is structurally impossible. A handler test asserts a re-parked `pending` stage stays `pending` (never `dispatched`) with a real orchestrator wired — proof of zero `Advance` calls.
- Each `RetryStage` bumps the stage's `SelfRetryCount`, so revive consumes per-stage retry budget exactly like `fishhawk_retry_stage` — a batch retry-shaped re-open, not a budget bypass. A resumed revive bumps NO stage's budget (the re-parks already happened).
- **Auth**: revive requires the operator retry scope (`write:stages`/`write:retries`) AND rejects any MCP/agent subject-bound token outright (`403 agent_token_forbidden`) — an agent may not revive any run. Mirrors `/redrive`.
- Writes ONE chained `run_revived` audit (user actor; payload lists each restored stage's `stage_id`/`type`/`prior_category`/`prior_reason`/`restored_state` plus `stage_count` and `resumed` — so a `stage_count:0` resumed revive is self-explaining) and refreshes the sticky status comment. The response body is `{run, restored_stages[], resumed}` (`resumed` additive; `restored_stages` empty on a resumed revive).

### Operator merge verb (`merge_run.go`, E48.7 / #1954)

`backend/internal/server/merge_run.go::handleMergeRun` (route `POST /v0/runs/{run_id}/merge`, MCP verb `fishhawk_merge_run`). The one-verb operator merge path: it records the operator's merge verdict as a chained `merge_verdict_recorded` audit entry (modeled on `vouch.go`) and queues the squash merge through the SAME `s.cfg.GateMerger` seam the delegated `may_merge` arm of `AutoDriveRunGate` dispatches through — extracted into the shared `dispatchAcceptanceGatedMerge` helper (`autodrive.go`), so the human merge and the delegated merge converge on one path by construction. The PR-approval review itself stays a `gh pr review --approve` step under the operator's own GitHub identity (the 2026-07-15 option-a decision; App-identity approval deferred to E39).

- **Auth ladder** (operator-only, mirrors `vouch.go`): anonymous → `401`; a run-bound `mcp:run:<uuid>` token → `403 run_token_forbidden` (even for its own run — an agent self-merging its PR would bypass the operator gate); any identity missing `write:approvals` → `403 insufficient_scope`, enforced UNCONDITIONALLY (no cookie-session bypass, since the verb queues a real squash merge).
- **Fail-closed guards, all BEFORE any write**: `404 run_not_found`; `409 run_not_mergeable` when the run has no PR url OR is `failed`/`cancelled`; `409 acceptance_gate_not_passed` when the acceptance gate is pending/failed/outcome-unknown or unreadable (ADR-049 decision #6 — passed / not-declared / skipped-out-of-scope proceed); `503 merge_seam_unconfigured` when `GateMerger` is nil. It deliberately does NOT block on a review stage parked at `awaiting_approval` — in `feature_change` that stage settles ON merge via `resolveReviewStageOnMerge`, so blocking would deadlock the human merge.
- **Endpoint-side idempotence** (binding condition, #1954): a repeated POST that finds an existing `merge_verdict_recorded` row appends NO duplicate and responds `already_recorded:true`, but ALWAYS re-dispatches the merge helper — so a `502`-then-reinvoke re-queues the merge without ever duplicating the verdict. A merge-helper error returns `502 merge_dispatch_failed` stating the verdict row is durable and the queue step is retryable. Response `{run_id, merge_queued, verdict_sequence, already_recorded, pr_url}`.
- The endpoint does NOT wait for the merge to land: the merge only ENABLES/queues GitHub's merge, and the `pr_merged` / run-completion settle is left to the `pull_request`-closed webhook — the MCP `fishhawk_merge_run` tool awaits the terminal state client-side.
- `merge_verdict_recorded` is registered in `audit.KnownCategories` and is an internal, non-comment audit kind (see `docs/issue-comment-surfaces.md`).
- The admitted acceptance-gate set is the SHARED `acceptanceGateAdmitsMerge(state)` predicate (`acceptance.go`), which this endpoint, `dispatchAcceptanceGatedMerge` and the drive observer's fall-through arm all defer to — so a merge-eligible state can never be admitted by two of them and refused by the third. Callers still gate on `gerr == nil` FIRST: on a read error `acceptanceGateState` returns `("", err)`, and `""` is `acceptanceGateNotDeclared`.

### Paged-acceptance-triage arbitration (`acceptance_arbitration.go`, E66.37 / #2474)

`backend/internal/server/acceptance_arbitration.go::handleAcceptanceArbitration` (route `POST /v0/runs/{run_id}/acceptance-arbitration`, MCP verb `fishhawk_arbitrate_acceptance`). The operator-only **discharge** for a PAGED acceptance triage. Before it, a paged run had no blessed path: `acceptanceGateState` mapped ANY recorded `failed` verdict to `acceptance_triage`, `POST /merge` refused it, and the retry handler's acceptance-reopen arm requires NO recorded verdict — so the operator the product told to "arbitrate before merging" could only leave the loop and hand-merge, losing the `merge_verdict_recorded` entry.

- **What it records.** ONE chained `acceptance_triage_arbitrated` entry (`ActorKind=user`, `StageID` = the acceptance stage the outcome was scoped to) with payload `{run_id, stage_id, reason, outcome_sequence, verdict, criteria_failed, criteria_skipped, acknowledged_failed_criteria, triage_class, triage_disposition, delegated:false}`. Registered in `audit.KnownCategories`; internal, non-comment audit kind.
- **The sequence binding is the whole mechanism.** `outcome_sequence` names the `acceptance_outcome_recorded` entry being discharged. `acceptanceGateState` honours an arbitration only when that value EQUALS the newest recorded outcome's sequence, so a later acceptance re-run (a higher-sequence outcome no arbitration names) re-wedges the gate at `acceptance_triage` **by construction** — there is no separate expiry rule to forget.
- **Auth ladder** (operator-only, mirrors `merge_run.go` because it admits the same merge): anonymous → `401`; a run-bound `mcp:run:<uuid>` token → `403 run_token_forbidden` even for its own run; any identity missing `write:approvals` → `403 insufficient_scope`, enforced UNCONDITIONALLY.
- **Guard order, all fail-closed and BEFORE any write** (so a refused arbitration leaves ZERO rows): `400 validation_failed` (non-UUID `run_id`, malformed body, empty/whitespace `reason`) → `503 acceptance_arbitration_unconfigured` → `404 run_not_found` → read stages + the latest outcome → **idempotence short-circuit** → `409 acceptance_arbitration_not_applicable` unless the gate reads `acceptance_triage` (a gate read error is `500`, never a write) → `409 acceptance_arbitration_not_applicable` unless the CORRELATED triage disposition pages → `409 acceptance_arbitration_requires_acknowledgement` when `criteria_failed > 0` without `acknowledge_failed_criteria:true` → **write-side revalidation** → append.
- **Admission is keyed on whether the disposition PAGED, not on the class number** (the operator's binding condition on #2474): a class-1/2 verdict that auto-routed to `fixup_dispatched`/`retry_dispatched` keeps its automatic route and is refused, while a class-1 `fixup_unavailable_paged` — the fix-up ceiling spent, the human paged — IS arbitrable with the explicit acknowledgement. An ABSENT or undecodable disposition is refused too. Correlation is by audit sequence: the newest `acceptance_triage_decided` entry STRICTLY ABOVE the outcome, matching the ordering `handleShipAcceptance` writes them in (outcome append precedes `triageAcceptanceFailure`).
- **Write-side revalidation (`409 acceptance_outcome_superseded`).** Every guard reads the chain at some earlier instant, and `dispatchAcceptanceGatedMerge` runs on the delegated auto-driver's background path, so a second actor really can land a newer verdict mid-request. The handler therefore RE-READS the latest outcome immediately before appending and refuses, naming `evaluated_outcome_sequence` and `current_outcome_sequence`, if it moved — otherwise it would persist an operator discharge of a verdict nobody evaluated.
- **Read-side revalidation is a CONSISTENT SNAPSHOT, not optimistic re-checking.** `acceptanceArbitrationDischarges` (`acceptance.go`) reads `AuditRepo.ListForRun` — a SINGLE query returning every entry for the run — and evaluates both halves inside it: the newest `acceptance_outcome_recorded` entry in the snapshot must still be the one the caller classified, AND an `acceptance_triage_arbitrated` entry in that same snapshot must name it. There is no instant between the two observations because there is only one observation, so the interleaving window is closed by construction rather than narrowed. Any read error is PROPAGATED so every consumer fails closed.
- **Idempotence** runs BETWEEN the run lookup and the gate guard, deliberately: after the first POST the gate reads `acceptance_arbitrated`, so a check placed after that guard could never match and a timed-out-then-retried call would get a confusing `409`. A repeated POST that finds an arbitration bound to the newest outcome returns `200 already_recorded:true` with the existing row's sequence and appends nothing. It is skipped entirely when no outcome is recorded, so a zero-valued sequence cannot alias a real binding.
- **Best-effort tail**, never unwinding the recorded arbitration: `notifyStatusUpdate` refreshes the living-anchor comment and `refreshDriveAfterArbitration` re-runs `ObserveParkedReviewForDrive` over the run's review stage so `derived_status` flips from `acceptance_triage` to `awaiting_merge` immediately rather than on the next reconciler tick. Both only WARN-log; `merge_run` is unaffected because it reads `acceptanceGateState` directly, not `derived_status`.
- Response `{run_id, acceptance_gate_state, outcome_sequence, arbitration_sequence, already_recorded}`.

### Run-branch reset remediation (`reset_branch.go`, ADR-035 third line / #867)

`backend/internal/server/reset_branch.go::handleResetRunBranch` (route `POST /v0/runs/{run_id}/reset-branch`, MCP verb `fishhawk_reset_run_branch`).
The operator-gated, audited remediation that completes ADR-035: **detect** at the report boundary (#858 `verifyBranchLineage`), **prevent** the base-laundering vector (#861/#865 fresh-fetch base), **remediate** a foreign commit pushed ON TOP of the run's commits (this endpoint).
It rewinds the open run/PR branch back to its **last run-authored HEAD** — the newest commit attributable to the run's reported-head ledger — dropping the on-top foreign commit.
(Contrast the vouch remediation above, which KEEPS and attributes the operator commit.)

- **Classification** reuses the #858 machinery: `resolveLastRunAuthoredHead` (`lineage.go`) resolves the PR base ref via `resolveLineageBaseRef`, builds the reported-head ledger with `ledgerSeedSHA=""` (the foreign tip is NOT self-whitelisted), runs `CompareCommits(baseRef, headSHA)`, and walks the ordered `(merge-base, head]` list.
  The ledger is decomposition-aware per #1038 — a decomposition parent's ledger includes its children's `child_pushed`/`fixup_pushed` heads via the `decomposed_from` linkage, and a child-enumeration or per-child chain-read error fails CLOSED here.
  The newest ledger member is the reset target, the first foreign commit is the offender, and `isOnTop` is true only when every foreign commit sits strictly above the newest ledger member.
- The force-update goes through `githubclient.ForceUpdateRef` (`PATCH .../git/refs/heads/{branch}` with `force:true` — the rewind is non-fast-forward; the REST refs API has no compare-and-swap, so the lease analog is the handler's re-read of the live head immediately before the patch).
- **Inverts detection's fail-open posture for the destructive action**: any uncertainty (unresolvable anchor, incomplete ledger, compare error, no identifiable run-authored HEAD, or a lease change) returns `reset_not_determinable` and never force-updates.
  An ancestor/interleaved foreign commit (which a reset cannot drop — prevention owns it) returns `reset_out_of_scope`; a clean tip returns `reset_not_applicable`.
- **Operator-gated**: requires `confirm:true` else 400, plus `write:runs`; a run-bound `mcp:run:<uuid>` token may reset only its own branch (`cross_run_reset`, mirroring the fixup handler).
- On success it re-parks the run's review stage (`awaiting_approval → pending → awaiting_approval` via `reparkReviewGateForReset`, best-effort/tolerant of the commit-yourself no-review-stage shape) so the merge reconciler + `ReverifyBranchLineage` re-evaluate the rewound clean tip.
  It also writes a `branch_reset` audit entry (operator actor; payload `{run_id, pr_number, branch, dropped_offending_sha, reset_to_sha, prior_head_sha, reason, recovery_note}`) and refreshes the sticky status comment.
- The dropped commit stays recoverable from the remote reflog / the foreign pusher's own branch.

### Local auto-driver endpoints (`autodrive_http.go`, E22.X / ADR-040 / #1700)

Two endpoints exposing the in-process `AutoDriveRunGate` (E25.6 / ADR-047, the campaign driver's delegated approve/route_fixup/retry/merge contract) to the local `fishhawk_drive_run` MCP verb (`backend/cmd/fishhawk-mcp/drive_run.go`).

- `POST /v0/runs/{run_id}/auto-drive` (`handleAutoDrive`) drives the run's ONE parked gate under the caller's operator-agent identity (`write:approvals`), passing `s.cfg.GateMerger` (the SAME `githubAutoMerger` seam `serve.go` builds for the campaign `GateActor`; nil keeps `may_merge` fail-closed to observe-only).
  The delegated action's OWN audit row (`approval_submitted`/`stage_fixup_triggered`/`stage_retried`, written transactionally) is the AUTHORITATIVE delegation record; on an ACTED outcome the handler ALSO appends a SUPPLEMENTARY `run_auto_driven` `act:"gate"` attribution row.
- `POST /v0/runs/{run_id}/auto-drive/acts` (`handleAutoDriveRecordAct`) is the record-before-dispatch sibling: the drive verb records a `run_auto_driven` `act:"dispatch"` row (fail-closed field validation) BEFORE it host-spawns a stage, so no mechanical act is ever unaudited.
- **Fail-loud**: a supplementary-append failure after a gate act returns `500 auto_drive_record_failed` (never a silent `acted:true`), and a record-append failure returns `500 auto_drive_record_failed` so the driver does not dispatch.
- `fishhawk_drive_run` is the bounded, resumable loop that walks a `runner_kind:local` run start→merged with no operator calls when every knob is delegated, stopping at the first genuine decision (`decision_required:<state>`, `paged:<event>`, a pending scope amendment).
- `run_auto_driven` is registered in `audit.KnownCategories` and is an internal, non-comment audit kind (see `docs/issue-comment-surfaces.md`).

#### `may_approve` decline on a human-quorum gate (#2381)

Before submitting a delegated approve, the `may_approve` arm calls `delegatedApproveWouldAdvance` (`quorum.go`) — which MIRRORS `approveStageAs`'s own advance rule (`reached := !delegated && eligibleCount >= required`) so the pre-check and the post-Submit path can never disagree. It DECLINES with `decision_required` / `decision_state: human_quorum_required` (no approval submitted) when a delegated approve could never advance the gate: (a) the gate declares an `approvals` block — a delegated submission is unconditionally uncounted (`eligibleApprover`'s agent floor), so `reached` is always false; (b) the approvals block is unreadable — fail closed; (c) a block-less gate has a firing escalation — the same not-advancing rule the post-Submit nil-block branch applies (#2374); (d) the escalation resolver errors — fail closed. Only a genuine legacy no-approvals, no-escalation gate still auto-approves, byte-for-byte as today. A delegated approve that comes back a DUPLICATE reports `decision_state: delegated_approval_no_progress` rather than a no-op `acted:true`; `handleAutoDrive` appends no `run_auto_driven` row for either declined pass. The two decision-state strings are exported constants in `operatorrole` (`DecisionStateHumanQuorumRequired` / `DecisionStateDelegatedApprovalNoProgress`), referenced by BOTH the server emitter (`autodrive.go`) and the driver's decode/compare (`cmd/fishhawk-mcp/drive_run.go`), so a spelling divergence across the HTTP boundary is a compile error rather than a silent livelock.

#### Delegated-approval distinct identity (#2381)

A DELEGATED approval is recorded under the distinct `operatorrole.DelegatedApprovalActorSubject` (`operator-agent/delegated`) via `effectiveApprovalSubject` (`quorum.go`) — the single source of truth for the mapping, used by BOTH the handler duplicate pre-check and `approveStageAs`'s Submit. An already-agent subject (the campaign `CampaignActorSubject`) and every non-delegated approval are returned unchanged. Because #1709 records-but-never-counts a delegated vote, recording it under the operator's own subject would make their later real approve a #986 duplicate no-op against a gate with no un-approve verb; the distinct identity keeps the operator's slot free and adds an `on_behalf_of` audit-payload key naming the real operator. Consumers of the one-decision-per-subject invariant, walked so the remap is provably safe at each: the duplicate pre-check (now keyed on the effective subject — the fix), `ApprovalRepo.Submit`'s `(stage_id, approver_subject)` uniqueness (unchanged, no migration), `delegatedApproverSubjects` (records the synthetic subject, so the human never lands in the delegated set), `distinctEligibleApproverSubjects`/`countDistinctEligibleApprovers` (synthetic excluded twice over — agent floor AND delegated set — human deduped once), `predicate_snapshot` (labels the synthetic act `agent`, honestly), `resolveChangeAuthor` (its allow-list holds only `operator_commit_vouched`, so an approval row never resolves an author), and `issuecomment` `renderApproverIdentity` (renders the operator-agent form with no `@`-mention).

### Host-dispatch spawn marker (`host_dispatch.go`, #1912)

`POST /v0/runs/{run_id}/stages/{stage_id}/host-dispatch` (`handleHostDispatchStage`) is the spawn marker that splits the conflated local `dispatched` state into two explicit signals (#1912). The backend cannot spawn the host-local runner (ADR-024), so `orchestrator.dispatchStage` parks a runner_kind-locked-`local` agent stage at `awaiting_host_dispatch` rather than `dispatched`; this endpoint stamps the spawn.

- CAS-transitions `{pending, awaiting_host_dispatch} → dispatched` (via the `run.StageCASTransitioner` capability, mirroring `run.failStageCAS`; in-memory fakes fall back to the plain table-validated `TransitionStage`). The MCP host-spawn verbs (`fishhawk_run_stage`, `fishhawk_dispatch_stage`, `fishhawk_drive_run`) call it fail-closed IMMEDIATELY BEFORE spawning, so post-#1912 `dispatched` unambiguously means "a spawn attempt exists".
- **Idempotent** on an already-`dispatched` stage: `200 {transitioned:false}` — the legal manual re-dispatch of a stage whose spawned runner died. A concurrent CAS-loss whose winner already marked `dispatched` is re-classified as the same benign no-op.
- **`409 dispatch_not_admissible`** on a `running`/terminal/`awaiting_*` gate state (and on a CAS-loss whose winner moved the stage to any such state) — a live or settled stage can never be re-marked as a fresh spawn.
- **Auth** mirrors the reap-failure endpoint: an authenticated identity carrying `write:runs` (anonymous → 401; a token without the scope → 403), with the auth ladder running BEFORE the nil-`RunRepo` guard (the #1915 revive convention) so config state never leaks pre-auth. A `(run_id, stage_id)` handle mismatch is `404 stage_not_found`.
- The prompt-fetch liveness flip (`prompt.go::markStageRunningOnPromptFetch`) defensively walks a still-parked `awaiting_host_dispatch → dispatched → running` on the authenticated prompt fetch, so a version-skewed spawn whose marker call was skipped/lost still converges.
- **Admission fence (#1936):** when an `Orchestrator` is wired, the handler acquires the SAME per-stage `orchestrator.LockStageAdmission` mutex the acceptance-admission short-circuit walk holds, across its stage-load → eligibility → CAS, so a marker call landing mid-walk cannot observe the walk-intermediate `dispatched` and return `{transitioned:false}` while the walk settles the stage — it serializes behind the walk and then 409s on the settled stage. With no orchestrator wired no lock is taken (behavior unchanged). See the admission section above for the full fence contract.

### Startup orphaned-review reconcile (`review_reconcile.go`, #1781)

`Server.ReconcileOrphanedReviews(ctx)` is a one-shot self-heal called from `serve.go` at boot immediately after `ReconcileStuckRuns` (gated on `srv != nil && RunRepo != nil && AuditRepo != nil`, best-effort/non-fatal).
It is the review twin of the #727 stuck-run recovery: when `fishhawkd` restarts while an in-process plan/implement review is in flight, the detached reviewing goroutine dies with the process, so no terminal `*_reviewed`/`*_review_skipped`/`*_review_failed` entry ever lands.
`review_status` (derived on demand from the audit trail by the MCP `reviewStatusFor` — `pending` while `landed_terminal < ConfiguredAgents`) then stays `pending` forever, wedging the gate.

- The pass pages `ListRuns(State=running)` and, per review stage (plan + implement), reads the LATEST `*_review_started` anchor (payload `ReviewStartedPayload{ConfiguredAgents, Authority}`, entry carries `StageID`).
- **Attempt correlation**: a stage accumulates several review rounds (a fix-up re-triggers the review, appending a fresh `*_review_started`); the CURRENT attempt is the latest started entry.
  Landed terminals are counted ONLY with audit sequence strictly greater than that started entry's sequence, and that SAME entry's timestamp drives the boot-marker comparison — never the earliest started nor a run-wide terminal count (which would mix a prior round's landed verdicts into the current tally).
- **Boot marker**: `Server.processStart` (in-memory, stamped once at `New` from `Config.ProcessStart`, default `time.Now()`) is the reference the pass compares the latest started entry's timestamp against — a review whose latest start predates the boot belongs to a dead prior process; one that does NOT is a review THIS process legitimately still has in flight and is spared.
  At startup `processStart == now`, so every un-terminated started entry predates it; the comparison is load-bearing only if the pass is ever invoked mid-process-life (a periodic sweep is a deferred follow-up, not wired here).
- When a review's latest round predates the boot with `landed < ConfiguredAgents`, it emits exactly `ConfiguredAgents - landed` terminal `*_review_failed` entries via the existing `emitReviewFailed` helper (`Timeout:false`, a restart-naming reason), driving `landed == ConfiguredAgents` so `reviewStatusFor` flips from `pending` to a terminal `failed` status `await_review` resolves on.
- Synthesized entries carry a placeholder model (`""`) / the round's authority — a documented fidelity limitation: the dead goroutine's per-reviewer model state is gone, but `reviewStatusFor`/`await_review` treat any `*_review_failed` as terminal regardless of model.
- After healing an implement review it republishes `fishhawk_audit_complete` (`recomputeAndPublishAuditComplete`, mirroring `trace.go`'s implement-review path) so the #947 review-pending presence gate reflects the now-terminal state.
- Best-effort PER RUN (a per-run error is logged and skipped); only a systemic `ListRuns` paging failure aborts. Reuses existing repo methods + terminal writers only (no new query, no schema change).

### Required-checks resolution — nil vs empty snapshot (`server.go`, #2497)

The drive observer (`ObserveParkedReviewForDrive`) and the CI-aggregate helper (`aggregateCIGreen`, `policy_reeval.go`) both distinguish "we never looked up branch protection" from "this repo declares zero required checks" — a distinction the pre-#2497 code collapsed, so an ABSENT snapshot folded to the same vacuous green as an empty one. During a total Actions outage a local run with no snapshot then announced "required checks are green" though the ruleset-required check never ran.

- **`reviewChecksResolution(ctx, runRow, stage) (green, resolved bool)`** is the tri-state that gates the observer's `checks_green_awaiting_merge` stamp (it replaced the old bool `reviewChecksGreen`). `resolved=false` ONLY when `RequiredChecksSnapshot == nil` (protection was never looked up — greenness unknown). A present-but-EMPTY snapshot is `(true, true)` — a repo that genuinely declares zero required checks is legitimately green. A present snapshot with contexts walks `LatestForStageAndName`: all `StatePass` → `(true, true)`, any gap (missing row, non-pass, unwired `StageCheckRepo`) → `(false, true)`. An UNREPORTED required check is unknown, not failed, so an unprotected/outage repo is never wedged.
- **`aggregateCIGreen(snap *run.RequiredChecksSnapshot, checks)`** takes the snapshot pointer (not a bare `[]string`) and returns `nil` (unknown) for a nil snapshot; a present-but-empty `Contexts` still folds to `true`. This chokepoint is shared by `deployCIGreen` (`approvals.go`) and the policy re-eval path, so the nil arm is reachable and counterfactually testable from a caller.
- **Unresolved runs still advance, deliberately.** The observer keeps stamping `RuleChecksGreenAwaitingMerge` (and, on an acceptance-declaring run, the acceptance parks) on a nil-snapshot run rather than parking — the local MCP loop never captures a snapshot, so parking would suppress the rule on EVERY local run and wedge `merge_run` (`delegation.evalGatesResolvedCIGreen` keys `may_merge` on that exact rule). What changes is the PROSE, not the rule/action/state: the audit `Event` and `next_action.Detail` stop asserting greenness (`checksEvidencePhrase` / `checksMergeDetail` / `checksAcceptanceLead`) and instead say the checks were not resolved and must be verified on the PR before merging. All three acceptance parks — `acceptance_pending`, `acceptance_settled_outcome_unknown`, `acceptance_triage` — carry the honest wording, since every local run hits the nil-snapshot path (#2497 condition 1).
- **Not fixed here:** capturing the snapshot on the local/MCP create path (#2497 proposal 3, deferred to #2506). With this change the absence is safe and honestly reported rather than silently green. The `GET …/stage-checks` `declared: []` response still cannot distinguish nil from empty (would need a new API field) — consumers must not read an empty declared list as evidence of greenness.

### Advisory-qualified `awaiting_merge` detail (`server.go`, #2487)

The `checks_green_awaiting_merge` `next_action.Detail` was a fixed string — `all gates resolved and required checks are green; review and merge the PR` — regardless of outstanding advisory reviewer rejects or unresolved concerns, so the last sentence an operator read before merging could read as an all-clear while a reviewer had rejected with high-severity defects. `checksMergeDetail(resolved bool, adv advisoryOutstanding)` now composes a checks lead with an advisory qualifier derived at stamp time in `ObserveParkedReviewForDrive`. Three branches:

- **Clean (`Known`, zero rejects, zero concerns):** today's two strings byte-for-byte (a clean run gets no noisier).
- **`Known` with a non-zero count:** the lead becomes `all blocking gates resolved` — the phrase `all gates resolved` never appears — followed by the non-zero counts and the dispositions (`fishhawk_get_gate_view`, then merge / `fishhawk_fixup_stage` / `fishhawk_waive_concern`).
- **`Known == false`:** the advisory state could not be read (a nil/erroring `ConcernRepo` OR an unreadable verdict payload) → an explicit read-first hedge naming `fishhawk_get_gate_view`, matching the run's "fail toward read, not toward merge" posture.

Load-bearing details:

- **The two counts have DIFFERENT windows.** The reject count is **round-scoped**: it is derived inside the existing `implementReviewRound` audit walk, which only counts `implement_reviewed` entries above the latest `implement_review_started` sequence, so a reject a fix-up round already superseded does not count (no extra audit read). The open-concern count is **run-wide** across both stage kinds (`ConcernRepo.ListOpenByRun`, the same set the gate view and the delegated merge gate read). The detail wording names each scope (`(latest review round)` / `(run-wide)`) so the side-by-side counts never imply a shared window (#2487 condition 4).
- **An UNDECODABLE `implement_reviewed` payload fails toward read, not toward the clean all-clear** (#2487 condition 1). `implementReviewRound` flags `verdictsUnreadable` when a payload fails to decode (the truncated-review-returned-success failure mode); `advisoryOutstandingFor` then returns `Known=false` and takes the same hedge as a broken concern read. `Known` cannot stay true on the strength of the concern read alone while the verdict read is broken.
- **Prose-only, by construction.** The drive rule (`checks_green_awaiting_merge`), the `next_action.Action` (`merge_pr`), the derived status (`awaiting_merge`), and the audit `Event` (`checksEvidencePhrase`) are all unchanged, so `delegation.evalGatesResolvedCIGreen`'s `may_merge` input (which keys on the rule) and every other consumer are untouched — only the operator-facing `Detail` differs.
- **Fixed at first-fire.** The observer is poll-driven and idempotent on `Recorded(RuleChecksGreenAwaitingMerge)`, so the detail is set when the rule FIRST fires (reviews are terminal before it can fire, so the window is narrow). A reject that lands after the stamp does not re-qualify an already-stamped entry — the same bound every other field on that stamp carries; re-stamping on later evidence would be a behavioural change beyond this issue.

## Artifact upload endpoints

### Plan artifact upload chain (`plan.go`, E5.X / #191)

- **Runner side**: `runner/internal/upload/upload.go::ShipPlan` POSTs the validated plan JSON with `X-Fishhawk-Signature` reusing the per-run signing key. `runner/cmd/fishhawk-runner/main.go::uploadPlan` runs after trace upload (so they share the key).
- **Backend**: `POST /v0/runs/{run_id}/plan?stage_id=…` (handler at `backend/internal/server/plan.go`) verifies signature, validates against `standard_v1` via `plan.Validate`, dedups via `artifact.GetByHash` (idempotent re-upload returns 200 vs 201), inserts an `artifacts` row, appends a `plan_generated` audit entry.
- **Prompt side**: `backend/internal/prompt/prompt.go` exports `PlanArtifactPath = /tmp/fishhawk-plan.json` — embedded in the plan-stage prompt and matched by the workflow file's `plan-out` input.
- **Bounded in-run schema-retry (#646)**: when a plan fails `standard_v1` validation after coercion (the category-B fail path), `handleShipPlan` first calls `trySchemaRetry` (`plan.go`).
  With the orchestrator + audit wired and a budget remaining (`maxPlanSchemaRetries = 1`, counted via `plan_schema_retry` audit entries for the run), it records the validation error to a chained `plan_schema_retry` entry (payload key `validation_error` — that entry is both the budget counter and the feedback source).
  It then re-opens the plan stage (`FailStage(FailureA)` → `RetryStage(pending)`, which clears the transient FailureA so it never leaks into the run/response), and fires `Orchestrator.Advance` to re-dispatch.
  The response is `400 plan_invalid` with `details.retry_scheduled=true` so the now-finished runner exits cleanly and the local operator/driver knows a re-attempt was set up.
  On `github_actions` Advance fires workflow_dispatch; on `runner_kind=local` it walks pending → dispatched and the operator re-drives via a fresh `fishhawk_run_stage --stage plan`.
  A second identical failure exhausts the budget and falls through to the unchanged `FailStage(FailureB)` + `advanceAfterFailure` path.
  The next plan-stage prompt injects the recorded error via `Trigger.PriorSchemaValidationError` (see the "Per-stage prompt construction" entry in `docs/ARCHITECTURE.md` §10).
  **The budget check-and-consume is atomic (#2518)**: `trySchemaRetry`'s count-then-append is now the shared `consumeRetryBudget` helper, which under the production repo does count-under-row-lock-then-append in ONE transaction (`audit.AppendChainedUnderBudget`) so two concurrent ships cannot both consume the one-shot. **This changed the schema path's read-error behaviour** and is intentional: the old `countSchemaRetries` swallowed an audit-list read error and reported `0`, so a *persistent* audit failure granted a retry on every ship and the one-shot never exhausted; `consumeRetryBudget` now DECLINES on any non-exhaustion error (unreadable/unwritable budget) and the upload takes the pre-existing `FailStage(FailureB)` path (operator-recoverable via `retry_stage`) — the same fail-closed bound the scope path already held (#2517). No test pinned the old behaviour; `TestShipPlan_SchemaRetry_BudgetError_FailsB` pins the new one.

### Pull-request artifact upload chain (`pullrequest.go`, E5.X / #195, #206)

Implement-stage post-processing lives in `runner/cmd/fishhawk-runner/main.go::openPRAndShipArtifact`. Sequence: (1) PR title + body selection, (2) commit + push, (3) PR open, (4) artifact ship; the backend endpoint is `backend/internal/server/pullrequest.go`.
Stage-type gating in main.go: this whole chain only fires when the prompt response says `stage_type == implement`; plan validation/upload is correspondingly skipped in that branch.

#### PR title + body (`prTitleAndBody`)

- (1) `prTitleAndBody` (same file) picks the title + body for the PR — either from the agent-authored file at `prompt.PullRequestDescriptionPath` (`/tmp/fishhawk-pr.md`, format = first line title / blank line / markdown body) with a Fishhawk attribution footer appended, or from a generic Fishhawk fallback template if the file is missing or malformed (`pr_template_invalid` / `pr_template_warning` policy log entries flag the latter).
- The implement-stage prompt instructs the agent on the format and conditionally requests `Closes #<n>` for issue-triggered runs (see `backend/internal/prompt/prompt.go::buildImplement`).
- **Conventional Commits (E32.9 / #1572)**: the prompt requires the first line to be a Conventional Commits v1.0.0 header (`type(scope): description`, allowed types `feat|fix|docs|refactor|test|chore|perf|build`) that doubles as **both** the PR title and the commit subject.
  `loadAgentAuthoredPR` runs a **warn-only** check (`conventionalCommitHeaderRe`) that emits `pr_template_warning` when the agent-authored title is not a conventional header but **uses the title verbatim** — never a hard failure, never a rewrite.
- Both fallbacks are chore-typed: the standalone/implement fallback is `chore: fishhawk implement stage <shortStageID>`; the **fix-up** fallback is `chore: fishhawk fixup stage <shortStageID> (base <shortBaseSHA>)`, made unique per pass by embedding the pass's base tip SHA (each fix-up pass starts from a different tip).
- The CLI auto-PR path (`cli/cmd/fishhawk/autopr.go::parsePRDescriptionFile` / `prFallbackTitle`) mirrors the chore-typed implement fallback and the warn-only conventional-header check.

#### Commit + push (`gitops.Pusher.CommitAndPush`)

- (2) `runner/internal/gitops/commit.go::Pusher.CommitAndPush` configures a bot identity, creates a branch (see branch routing below), stages all changes, commits with `--signoff`, and pushes via HTTPS as `x-access-token:<token>` — the token comes from the installation-token endpoint (#197), not `GITHUB_TOKEN`.
- **Commit message**: the **initial** (non-fix-up) implement commit prefers a run/stage-keyed Conventional-Commits sidecar `prompt.ImplementCommitMessagePath` (`/tmp/fishhawk-implement-commitmsg-<runID>-<stageID>.txt`).
  The full implement prompt instructs the agent to write a clean commit message there (a conventional subject + concise plain-text body) kept SEPARATE from the rich PR review body, so the initial commit no longer stuffs the whole `/tmp/fishhawk-pr.md` artifact (summary/test-plan/notes/checklists/footer) into its message.
  The sidecar is pre-invoke-swept + delete-after-read for freshness, **falling back to exactly `title + "\n\n" + body` when absent/empty** (E32.19 / #1686, no behavior change for older agents that write no sidecar).
  Decomposed children each write their own concise message via the same sidecar (keyed by the child's stage id).
- On a **fix-up** pass the commit instead uses its OWN per-pass Conventional-Commits message, which the fix-up agent writes to the run/stage-keyed `prompt.FixupCommitMessagePath` sidecar — pre-invoke-deleted and delete-after-read for freshness, deliberately NOT `/tmp/fishhawk-pr.md` so a fix-up never clobbers the existing PR title/body — falling back to the chore-typed per-pass message above when absent/empty.
- Both the initial sidecar (runner `implementCommitMessagePath` + CLI auto-PR `implementCommitMessagePath`) and the fix-up sidecar coordinate their path across the three independent modules via a hardcoded format string, locked by the prompt-render test asserting the literal path plus the runner/CLI load tests.

#### No-changes handling

- A clean working tree → `NoChanges=true` short-circuits with an `implement_no_changes` log line; on the standalone path the stage still succeeds.
- A **decomposed child** additionally emits `implement_child_no_changes` (carrying an additive `slice_index` field) and reports `{outcome:"failed", category:"C"}` via `reportPullRequestFailure` (#1036) — the child's terminal transition is owned by the `/pull-request` report (#771).
  With the pre-invoke shared-branch checkout in place a genuine no-changes child is overwhelmingly a planning/decomposition error, so it terminalizes **failed category-C retryable** (mirroring the standalone `no_diff_captured` semantics, #691/#692, with no new backend surface) rather than silently succeeding or hanging.
- **Position-aware no-changes diagnostic (#1258 slice C / #1279)**: the failure reason is built by the pure helper `childNoChangesReason(runSliceIndex)` and branches on the child's 0-based slice position.
  Slice 0 keeps the genuine-no-op / planning-decomposition-error framing (no predecessor to blame), while slice N>0 emits an actionable dependent-slice diagnostic stating that predecessor slices 0..N-1's merged changes are absent from this slice's isolated base (so code referencing them could not compile and was correctly not written) and naming the `fishhawk_consolidate_slices` recovery.
  BOTH branches retain the literal `child_no_changes` token and category C (load-bearing for audit/await keying + #1036 mirroring).
- A **fix-up** reports `{outcome:"fixup_no_changes"}` (#856).

#### PR open + artifact ship

- (3) `gitops.OpenPRClient.OpenPR` creates the PR via `POST /repos/{owner}/{repo}/pulls` with the same App token.
- (4) `upload.ShipPullRequest` POSTs the artifact body (pr_number, pr_url, branch, head_sha, base_sha, title, body, files_changed_count) signed with the same per-run Ed25519 key.

#### Backend endpoint (`backend/internal/server/pullrequest.go`)

- `POST /v0/runs/{run_id}/pull-request?stage_id=…` verifies the signature, validates required fields structurally, dedups on (stage_id, content_hash), inserts `artifacts` (kind=pull_request, no schema_version yet), and appends a `pull_request_opened` audit entry.
  When the trace handler's push-and-open-pr gate left the implement stage in `running`, it also drives the stage's terminal transition (#742; implement-review invocation is `docs/ARCHITECTURE.md` §4.2.1).
  The body also accepts a `{outcome:"failed", category, reason}` failure-report variant that fails the stage instead.
- On a base-rebase re-invoke ship the body additionally carries a `supplemental_scope_exemptions` delta (lowercase `{path,reason}` wire keys, matching `scopeExemptionEvidence`); `handleShipPullRequest` re-emits it as a supplemental `scope_files_exempted` audit row (#1218).
  In the terminal-drive branch, before `advanceImplementStageAfterPR`, it also dispatches the ADR-042 / #1250 supplemental implement-review (`runSupplementalReinvokeReview`) against the pushed re-landed tree — a gating reject fails the stage category-B + closes the PR (#877 helper).

#### Local-runner mode (E22.8 / #406) + CLI auto-PR (#422)

- Three flags on the runner substitute for the GHA-specific env reads — `--github-repo owner/name` (fallback for `GITHUB_REPOSITORY`), `--base-branch <ref>` (fallback for `GITHUB_REF_NAME`), and `--no-pr` (skips the entire push + PR-open + ship chain; the working tree stays dirty for the operator to commit themselves). Flag-precedence on read: explicit flag > env var > default.
- The `--no-pr` short-circuit emits an `implement_pr_skipped` log line with `reason: no_pr_flag` so the audit story is "we deliberately skipped" rather than "we lost the PR step."
- **CLI auto-PR (#422)**: when `--no-pr` is absent and stage=implement, the CLI wrapper (`autoOpenPR` in `cli/cmd/fishhawk/autopr.go` + `ShipLocalPullRequest` in `cli/internal/httpclient`) runs the git+gh flow and calls `POST /v0/runs/{id}/pull-request` with a bearer token carrying `write:runs` scope.
  The per-run Ed25519 signing key is consumed by the trace and plan uploads inside the runner subprocess and is not accessible to the outer CLI wrapper process.
  The endpoint accepts either auth path; the audit payload's `auth_method` field records `ed25519` for the runner path and `bearer` for the CLI operator path.
  The `--no-pr` default flipped from `true` to `false` in that PR so the auto-PR flow is opt-out rather than opt-in.

#### Decomposed-children branch sharing (#473, #714 / ADR-032, #1036)

- Standalone runs use branch `fishhawk/run-<shortRunID>/stage-<shortStageID>` (one branch per stage). Decomposed child runs (non-nil `decomposed_from`) instead use the shared branch `fishhawk/run-<shortParentID>` — all children commit onto a single branch, producing one epic PR.
- The backend's prompt response includes `decomposed_from_run_id` (omitted for standalone runs); the runner reads it from `FetchedPrompt.DecomposedFromRunID` and the CLI reads it via `client.GetRun` after the subprocess exits.
- First vs. subsequent child detection: `git show-ref --verify refs/remotes/origin/<shared-branch>` — non-zero exit = first child (branch not yet on remote); zero exit = subsequent.
- **Subsequent-child base establishment (#1036)**: a subsequent child's declared policy base is the shared branch (#765), so BEFORE the agent is invoked the runner fetches + force-checkouts `origin/<shared-branch>` (`checkoutChildBase` seam → `gitops.CheckoutRemoteBranch`, the decomposition analogue of the fix-up flow's #967 `checkoutFixupBase`).
  It emits `child_base_established` (branch, head_sha, original_ref) and restores the operator's original ref via a defer mirroring the fix-up block's double-fire-safe construction — declared base == working tree (ADR-035 spirit), so a slice depending on a prior sibling's code compiles instead of failing against a main checkout missing its dependency (run d816e58a; this supersedes the operator checkout-the-shared-branch workaround).
  A checkout failure fails fast (`runner_failed` reason `child_base_checkout`) before any agent turns are spent; the first child skips the block (no shared branch yet).
  A sibling push landing between the pre-invoke read and push-time routing is exactly the pre-fix shape, handled by the unchanged stash-transplant + #989 conflict machinery.
- At push time subsequent children still stash uncommitted agent edits, fetch+rebase, restore edits, then commit — now a content no-op in the common case (the stash is cut from and reapplied onto the same tip). All decomposed-child pushes use `--force-with-lease`.
- **One PR per decomposition (#714 / ADR-032)**: EVERY decomposed child skips `OpenPR` and `ShipPullRequest` — the first child no longer opens a child-owned PR either.
  (Pre-#714 the skip was gated on `isSubsequent`, so the first child opened a PR the parent never tracked; the parent run carried no `pull_request_url` and the merge reconciler never resolved its review, parking it at `awaiting_approval` forever.)
  Each child only pushes its commit onto the shared branch and emits an `implement_child_pushed` log line (`shared_branch`, `head_sha`, `is_subsequent`); the **parent** run opens the single consolidated PR once all children settle — see the Stage orchestrator entry in `docs/ARCHITECTURE.md` §10.
- `CommitAndPushArgs` carries `ForceWithLease bool` and `RebaseFromRemote bool` to thread these behaviors through `gitops.Pusher.CommitAndPush`.

#### Standalone run-branch base isolation (#861 / ADR-035 prevention) + base-rebase conflict (#866, #989)

- A standalone run branch is cut from a **freshly-fetched authoritative base** (`origin/<base-branch>`) rather than the ambient local HEAD, so a foreign commit another writer made in the same shared local checkout (the #797 contamination shape) cannot ride in as the branch base.
- `CommitAndPushArgs.FreshFetchBase` (set to `baseRef` only in the standalone `default:` routing case — not fix-up, not decomposed children) drives the same stash → `fetch <url> <base>` → `checkout -B <branch> FETCH_HEAD` → `stash pop` machinery the `RebaseFromRemote` path uses, preserving the agent's uncommitted edits while replacing only the base.
  The freshly-fetched tip is then re-captured as `CommitAndPushResult.BaseSHA`, so the recorded fork point (artifact `base_sha`) is the trustworthy authoritative ref. This gives the local runner the same base isolation GitHub Actions' `actions/checkout` already provides (a clean fetched ref).
- This slice makes the **recorded** `base_sha` trustworthy; it deliberately does **not** change the #858 lineage-detection guard's COMPARE anchor (which still resolves the *live* PR base ref to resist laundering — sourcing the compare from the runner-reported value would reintroduce the exact laundering vector #858 was built to avoid).
- A stash-pop conflict (the agent edited lines the base advanced past — e.g. an earlier decomposition sibling's shared-branch commit) is detected specifically (via `git ls-files --unmerged`) and surfaced as a typed `*gitops.BaseRebaseConflictError` unwrapping to `gitops.ErrBaseRebaseConflict` (#866, #989).
  The conflicted pop is aborted with `git reset --hard` so the working tree returns to the clean fetched base and the stashed edits are preserved/recoverable (`git stash list`, which a pop conflict does not drop); still no push, never a silent bad tree.
  The typed error carries best-effort-captured conflict context (conflicted paths, the conflict-marker hunks read via `git diff` in the only window they exist — between the unmerged probe and the abort — and the stashed patch via `git stash show -p` after it, each capped at 64KB; a failed capture degrades to empty fields without touching the abort sequence).
- **The conflict is no longer an immediate category-B (#989, proposal 1)**: on the open-PR and decomposed-child push paths, `run()` re-invokes the agent ONCE on the fresh base (re-checkout of the run-branch ref, which already points at the fetched tip) with a prompt embedding that context (`base_rebase_conflict_reinvoke` log+trace event; transient invoke failures retry via the #804 `maxFixInvokeInfraRetries` pattern, emitting `base_rebase_reinvoke_error`), then retries the commit+push chain once.
  Gate re-coverage is the existing #960 path: the re-landed tree differs from the gate-verified tree, so the `verified_tree_mismatch` single strict re-verify runs and only an explicit pass reaches origin (#969 stamps the re-verified tree).
  A second conflict, a non-conflict error, re-invoke exhaustion, or a re-invocation that completes with a non-OK agent result (the agent declined or failed semantically — its tree is never pushed) falls through to the unchanged category-B clean-abort failure (stash preserved); the **fix-up path is excluded** and keeps its immediate category-B → #788 recovery semantics.
- Recovery beyond the bounded re-invoke is still a clean abort, not auto-resolution — auto-merging divergent edits in git plumbing would risk shipping an unreviewed tree; the re-invoke routes resolution through the agent + the full gate chain instead. The same `popStash` conflict-detection covers the shared `RebaseFromRemote` decomposed-child path.
- Deferred follow-ups: the decomposed-first-child still cuts from ambient HEAD (same vector, separate flow).
  Per-run git **worktree** isolation (the stronger hardening that defeats mid-run concurrent commits on a shared local checkout) is shipped for the local loop (E22.X / #1137 — see the "Per-run working-tree isolation" lifecycle bullet in `docs/ARCHITECTURE.md` §4 and the worktree entry in §10).

#### Working-tree restoration after every pass (#911, #941, #953)

- `CommitAndPush` switches HEAD onto the run branch (`checkout -b`/`-B`), and on a CommitAndPush-side failure (e.g. the #800 committed-test verify gate flaking) the tree is also left dirty.
  Either way the operator's checkout would be stranded on the run branch, silently breaking the next `scripts/dev post-merge` (a dirty tree refuses `git checkout main`; the run-branch HEAD is not an ancestor of the squash-merge commit so `git merge --ff-only` fails).
- The operator's original ref is captured in `run()` BEFORE the agent is invoked (#941; `gitops.CaptureHead` — `git symbolic-ref --short HEAD`, falling back to the commit SHA on a detached HEAD, the hosted `actions/checkout` shape), so an agent that runs `git checkout -b` mid-stage can never make its own branch the restore target.
- Restoration is then **guaranteed at `run()` exit for implement stages on every path — success, failure, or panic** (#953): a `defer` installed right after the pre-agent capture re-reads current HEAD and force-restores the captured ref (`gitops.RestoreHead` — `git checkout --force <ref>`), emitting a `working_tree_restored` (or `working_tree_restore_failed` / `working_tree_capture_failed`) trace event.
- The defer is **moved-HEAD-guarded**: it skips silently when HEAD still sits on the captured ref — the common failure case where the agent only edited files must keep its dirty tree for operator inspection (a force-checkout would discard the staged+unstaged tracked edits).
  It likewise skips (with `working_tree_restore_failed`) rather than checkout blind when the in-defer HEAD re-read fails.
- It runs under a `context.WithoutCancel` + fresh ~30s timeout so a stage that failed by deadline/cancellation still restores. `--no-pr` skips the defer entirely (its leave-the-tree-as-is semantics are deliberate — the dirty tree is the deliverable).
- The pre-existing `openPRAndShipArtifact` defer and fixup-block defer remain as the early restorers on their paths; LIFO ordering means they fire first, after which the `run()`-level net's guard sees HEAD already restored and no-ops — never a double checkout.
- The restore is **best-effort and log-only**: a restore failure never overrides the stage's primary success/failure outcome or exit code.
  `--force` discards the staged+unstaged tracked modifications a failed pass leaves so the switch is not refused; committed work is preserved (the run branch ref still points at the commit, already pushed on the success path — HEAD just moves off it).
  Untracked files are intentionally left in place by the checkout itself (a `git clean` would risk deleting operator files) — agent-introduced untracked *drift* is instead removed by the discriminating cleanup below.
- The `defer` fires at function return, AFTER the inline `gitdiff` files-changed reads and `ShipPullRequest` reports that need the run-branch tip, so those reads are unaffected. Mirrors the clean-abort posture #866 established.

#### Discriminating drift cleanup after a successful stage (#943)

- The restore's `checkout --force` used to discard tracked drift modifications indiscriminately — including the operator's own pre-existing local edits the #866 stash/pop carried onto the run branch — while leaving untracked (net-new) drift behind to accumulate across loop runs.
- `run()` now snapshots the **pre-agent dirty paths** alongside the #941 HEAD capture (`gitops.DirtyPaths` — `git status --porcelain -uall`; a snapshot failure emits `working_tree_dirty_capture_failed` and disables cleanup for the stage — never revert blind).
  After a successful `CommitAndPush` (including the NoChanges-with-drift return) `openPRAndShipArtifact` partitions the reported `ScopeDrift` against it.
- Paths **not** dirty pre-agent are agent-introduced and reverted via `gitops.CleanDriftPaths` (pathspec-limited `git stash push --include-untracked` + `git stash drop`, covering tracked-modified, tracked-deleted, and untracked drift in one mechanism; an entry-created probe on `refs/stash` guards the drop so a clean-paths no-op never destroys a pre-existing operator stash entry; emits `drift_cleaned` / `drift_clean_failed`).
- Paths dirty pre-agent are **operator-owned and preserved** (`drift_preserved`): the restore defer calls `gitops.RestoreHeadPreserving`, which stashes them across the forced checkout and reapplies via the #989 `popStash` machinery (a pop conflict aborts cleanly and leaves the entry recoverable in `git stash list` — operator content is never silently destroyed; an empty preserve set delegates to plain `RestoreHead`).
- All of it is best-effort and log-only, never overriding the push's primary outcome. Failure paths and `--no-pr` are untouched: the dirty tree on failure remains the operator's inspection deliverable.

### Scope-bounded implement commit + scope-drift signal (#581)

The implement-stage commit is bounded to the approved plan's `scope.files` instead of `git add -A`, so stray dirty files (dev `.pid` artifacts, editor scratch, unrelated local edits) can't leak into a Fishhawk-attributed commit.

- **Backend**: `backend/internal/server/prompt.go` echoes the approved plan's `scope.files` into the prompt response's `scope_files` field (array of `{path, operation}`) on implement stages via `scopeFilesFromPlan`; omitted when no approved plan is available (`plan_missing_for_implement`).
- **Runner**: `FetchedPrompt.ScopeFiles` (`runner/internal/upload/upload.go`) carries it; `runner/cmd/fishhawk-runner/main.go` threads it onto `cfg.scopeFiles`, writes the resolved list to `/tmp/fishhawk-scope.json` (handoff format `{files:[{path,operation}]}`) for the out-of-process CLI auto-PR path, and passes the paths into both `computeAndEmitDiff` (so the policy diff sees the identical scoped index the commit will) and `gitops.CommitAndPushArgs.ScopeFiles`.
- `gitops.Pusher.StageScoped` (`runner/internal/gitops/commit.go`) reads `git status --porcelain`, stages exactly the declared dirty paths via `git add -A -- <paths>` (per-path `-A` covers create/modify/delete), and returns dirty-but-undeclared paths as `CommitAndPushResult.ScopeDrift` — excluded (never staged) and surfaced as a `scope_drift` log line + `policy_event` rather than blocking the commit (flag-only treatment per ADR-027).
- Empty scope → fallback to `git add -A`; all-out-of-scope dirt → `NoChanges` short-circuit.
- **Directory-prefix matching (#824)**: a scope entry ending in `/` is a folded directory — `StageScoped` splits declared entries into an exact-match set plus a slice of dir prefixes (`hasDirPrefix`), and a `git status --porcelain -uall` path stages when it exactly matches OR lies under any declared dir prefix; everything else is drift as before.
  This makes the structured `add_scope_files` directory case (e.g. `pkg/testdata/corpus/newcase/`) actually stage the created files underneath it.
  Exact-path (non-slash) entries keep their precise behavior — a regular file entry never prefix-matches a sibling (`foo/bar.go` does not stage `foo/bar.go.bak`).
- **CLI sibling**: `cli/cmd/fishhawk/autopr.go` reads `/tmp/fishhawk-scope.json` and applies the same per-path staging + drift check, falling back to `git add -A` when the file is missing/empty.

### Acceptance-stage lifecycle (`acceptance.go`, E31.6 / #1534, ADR-049)

The E31.6 ship-handler detail; the cross-component seam overview is the "Acceptance stage seam" section above.

- **Ship handler** `backend/internal/server/acceptance.go::handleShipAcceptance` (route `POST /v0/runs/{run_id}/acceptance?stage_id=…`, registered in `handlers.go`) — models `handleShipDeployment`.
  Guards: repos-unconfigured `503`, `run_id`/`stage_id` UUID `400`, stage-belongs-to-run `400`, non-`acceptance`-stage-type `400`, `32 KB` body cap `413`.
- **Dual-auth** via `authorizeAcceptance`: the Ed25519 `X-Fishhawk-Signature` runner path (ADR-050 #2 — the acceptance agent ships via signature with NO MCP token) OR a bearer with `write:runs`.
  Deliberately NO new scope, unlike deploy's `write:deploy`, since acceptance evidence is advisory.
- **Body validation**: `DisallowUnknownFields` decode + `acceptanceBody.validate()` — `verdict` ∈ passed/failed required; `failure_mode` ∈ error/assertion_fail required-iff-failed and rejected-on-pass; per-criterion `id` non-empty + `result` ∈ passed/failed/skipped.
  Two lossless #1574-class coercions run BEFORE the fail-closed reject — a string-valued object-map `evidence_hashes` collapses to its sorted values, and a schemeless host[:port] `target_url` gains an `http://` prefix.
  Any lossy shape (a non-string/nested map value, a scalar `evidence_hashes`, or a `target_url` whose scheme is not exactly `http://`/`https://`) still → `400 acceptance_invalid`.
- **Persistence**: an `artifact.KindAcceptance` row (SchemaVersion nil for v0) idempotent on `(stage_id, sha256(body))` — a hit reuses `ensureGovernanceAuditEntry` (#1396 self-heal) to backfill a missing outcome entry then returns `200 idempotent:true`.
  It appends an `acceptance_outcome_recorded` chained audit entry whose payload carries `verdict` + `failure_mode` (the E31.8 error-vs-assertion_fail carry-through) alongside the issue-comment render tags `outcome` (accepted/rejected) / `criteria_passed` / `criteria_total` (consumed by `issuecomment/status_template.go::renderAcceptanceOutcomeLine`, E31.3).
  Finishes with `notifyStatusUpdate` so the living anchor re-renders; a `201 acceptanceResponse{id, stage_id, content_hash, verdict, failure_mode, idempotent}`.
- **NO stage-state transition** — the stage settles via the ordinary agent trace-bundle path (E31.2 landed acceptance with no new states); failure routing/triage is E31.8. Audit categories (`acceptance_dispatched`, `acceptance_outcome_recorded`) live in this file.
- **Dispatch emit** `orchestrator.go::emitAcceptanceDispatched` — fired from `Advance` after `dispatchStage` successfully advances an `acceptance`-typed stage (both the agent fireDispatch path and the human awaiting-approval walk): a best-effort `acceptance_dispatched` entry (system actor, `{stage_id, sequence, executor}` payload; nil-Audit guard, WARN-on-error, never unwinds the dispatch).
- **Deliberately NO deploy-style pre-execution park and NO `advanceForDecision` special-case**: acceptance rides the ordinary `pending → dispatched` agent path and the generic `awaiting_approval` approve→succeeded / reject→failed-D gate semantics (contrast the deploy `awaiting_deploy_approval` park + `triggerDeploy` dispatch).
  Regression-pinned in `orchestrator_test.go` (dispatch, not park) and `approvals_test.go` (`TestAdvanceForDecision_AcceptanceStage_GenericGate`).
- **Prompt seam** `prompt.go::buildAcceptance` renders an independent-validator preamble (validate the RUNNING instance; the diff is withheld for independence, ADR-049 #4), the issue context, the approved plan's `verification.acceptance_criteria` + `out_of_scope`, a target-instance section, and the structured-verdict output contract.
  Both `handleGetStagePrompt` and `handleGetStagePromptRender` populate `Trigger.ApprovedPlan` + `Trigger.TargetInstanceURL` for an acceptance stage (NOT scope/diff fields).
- **E31.4 target-URL seam** `server/acceptance.go::resolveAcceptanceTargetURL` — the single named wiring point for the acceptance target-instance URL, ACTIVATED by E31.4/#1532.
  It returns the acceptance stage's first spec-declared `egress.target_hosts` entry in full **http(s) URL form** — a schemeless host or host:port gains an `http://` prefix (e.g. `localhost:8080` → `http://localhost:8080`) so the prompt hands the validator a URL, nudging its verdict `target_url` toward a URL rather than a bare host:port (#1574); an entry already carrying a scheme passes through.
  This SUPERSEDES ADR-050 decision #1's verbatim-host posture **for the prompt seam only** — the sibling `resolveAcceptanceEgressTargetHosts` (the egress-proxy allow-list input) KEEPS the verbatim host:port grammar, since the allow-list matches authorities, not URLs.
  A spec with no egress block yields "" and `buildAcceptance` renders an explicit not-declared line, keeping that state self-diagnosing.

### App installation-token endpoint (`installationtoken.go`, E5.X / #197, #201)

`POST /v0/runs/{run_id}/installation-token?stage_id=…` (`backend/internal/server/installationtoken.go`) mints a fresh installation token for the run's repo.

- **Dual auth** as of #201: the runner's runtime fallback signs with the per-run Ed25519 key (`X-Fishhawk-Signature`); the canonical pre-checkout flow presents a GitHub Actions OIDC token via `Authorization: Bearer <jwt>` (verified through the same `githuboidc` machinery the signing-key endpoint uses, with audience + repository + workflow claims bound to the run row).
  OIDC wins when both are presented; the audit payload's `auth_method` field records which path was taken.
- Implementation reads the run row's `installation_id` and calls `cfg.GitHubTokens.Token(ctx, installationID)`; production wiring is the cached `githubapp.NewCachedProvider` in `serve.go`.
- Audit category `installation_token_issued` records sha256 of the token, never the raw token.
- **Installation attribution for local/MCP runs (#713)**: webhook-dispatched runs get their `installation_id` from the delivery; MCP/local runs (which `POST /v0/runs` with the workflow spec inline) had it left nil and so hit `400 no_installation_for_run` here.
  `handleCreateRun` now resolves the repo's App installation best-effort on **both** the inline-spec and GitHub-fetch paths (`GetRepoInstallation`, hoisted above both branches) and stamps it onto the run row; `ErrNotInstalled` is lenient on the inline path (the run is still created with a nil id).
  When no installation is attributable, the runner's `openPRAndShipArtifact` maps this endpoint's `no_installation_for_run` to `upload.ErrNoInstallation` and falls back to the operator's local `gh auth token` for push + PR (logged `installation_token_received` with `source:gh_cli`); if `gh` is absent/not logged in it fails with an actionable error (install the App, or `gh auth login`) rather than the opaque token-fetch wrap.
  The merge reconciler can only poll the PR when the stamped App id is present — the `gh`-fallback path has no backend installation token, so its review gate resolves via the `pull_request.closed` webhook instead.

## Trace-time policy re-evaluation (`trace.go::reEvaluatePolicy`)

The backend's source-of-truth constraint evaluation on trace upload (E3.13): it loads constraints from the run row's cached workflow spec (#283), extracts the bundle's diff, and calls `policy.EmitEvaluation`, which writes the chained `policy_evaluated` audit entry the SPA renders.

- **Verification-signal derivation (#1886 / ADR-059).** Before evaluating, `reEvaluatePolicy` sets `constraints.Verification` from `verificationSignalFromBundle`, which reads the SAME bundle's single pre-redacted `gate_evidence` event (#963) — the `verify_summary` when present, otherwise the last **non-superseded** `verify_run` (#1205, the only one reflecting the pushed tree). `Commands` carries `{command, exit_code, outcome}` per non-superseded run, no output tails. No new runner emission is involved; this only threads existing evidence into policy evaluation.
- **nil is a violation, not a pass.** The helper returns nil on `bundle.ErrNoGateEvidence`, on any extract error, and when the evidence carries neither a summary nor a verify run — and the `verification_reported` required outcome treats nil as a violation (fail-closed). Contrast `ci_green`, whose nil signal *defers* to branch protection.
- **Diff-coverage signal derivation (#1888 / ADR-059).** `reEvaluatePolicy` also sets `constraints.DiffCoverageSignal` from `diffCoverageSignalFromBundle`, which reads the `diff_coverage` record out of that SAME `gate_evidence` event, while `mergeConstraints` supplies `constraints.DiffCoverage` (the DECLARATION) from the cached spec. Both are nil-safe: a stage that did not declare the constraint never enters the evaluation branch. nil is likewise a violation, not a pass — the runner emits a measured-with-zero signal rather than nothing when there is nothing to measure, so absence unambiguously means the runner never ran.
- **`resolveDiffCoverageConfig` (prompt.go)** serves the declaration to the runner as `promptResponse.diff_coverage`, at BOTH construction sites. It mirrors `resolveVerifyConfig`'s parse + stage-lookup shape and fails open to nil on every degradation (nil spec, parse failure, missing workflow, missing stage). That is safe because it is SYMMETRIC: the same spec lookup drives the backend's own constraint load, so a spec the backend cannot read yields neither a measurement request nor a gate. When a stage declares the constraint twice the most RESTRICTIVE threshold wins, matching `mergeConstraints`, so the runner measures against the same threshold the backend enforces. The stage-type surfaces cannot diverge either: only the implement runner measures, so the spec validator rejects `diff_coverage` on every other stage type (#1888) — otherwise a declaration on, say, an acceptance stage would reach evaluation with no signal, and a nil signal is a violation, i.e. a guaranteed false RED.
- The `bundle` import stays on this side of the seam: `backend/internal/policy` never imports it. Outcome semantics and the audit round-trip invariant live in `backend/internal/policy/README.md`.

## Ship-time plan gates

### Plan-gate warnings advisory (`plan_warnings.go`, #1684)

`backend/internal/server/plan_warnings.go::runPlanWarnings` — called from `handleShipPlan` (`plan.go`) immediately after `runTestSweep` and before `runScopeRegression`/`runPlanReviews`.
Gives `backend/internal/plan/validate.go::Warnings(p *Plan) []string` (unit-tested but previously uncalled in production) its first production caller, so the decomposition safety net it computes actually reaches the operator.

- `plan.Warnings()` returns soft advisory strings: notably a multi-slice (`len(sub_plans) >= 2`) decomposition where EVERY sub-plan omits `depends_on` (the shape that wedged #1551's first attempt — with no declared edges every slice runs in wave 0 and a producer->consumer chain can fail typecheck against a not-yet-integrated symbol, #1679/#1680).
  Plus two pre-existing advisories: a sub-plan `predicted_runtime_minutes` sum less than the parent's (possible scope compression), and an expensive `test_strategy` gate (`-count>=50` or full-repo `-race`) paired with an under-budgeted `predicted_runtime_minutes`.
- **Guards only `AuditRepo`** — unlike the sibling gates it needs no `RunRepo`/workflow spec/GitHub client, since `Warnings()` depends only on the parsed plan itself.
- **Write-only-when-non-empty** (the one divergence from the sibling gates' always-write "checked and clean" convention): a `plan_warnings` audit entry (payload `PlanWarningsPayload{warnings}`) is appended ONLY when `Warnings()` returns at least one string.
  A warning-free plan gets NO entry, which is what keeps `TestShipPlan_HappyPath`'s `len(au.appended)==1` assertion green (the sibling gates are no-ops in that test only because their external guards — `RunRepo`/spec/GitHub — are unmet, whereas this gate needs only `AuditRepo`, which IS wired there).
- Advisory + fail-open: a `plan.Parse` failure or an audit-append failure WARN-logs and returns nil/continues; it never transitions or fails the plan stage.
- The returned payload is NOT YET threaded into the plan-review prompt's gate-evidence section (deferred; the operator-facing surface for this slice is `fishhawk_get_plan`).
- MCP surface: `fishhawk_get_plan` adds `plan_warnings` (`[]string`) decoded from the **newest** `plan_warnings` entry (`loadPlanWarnings` in `tools.go`); absent/omitted when no entry exists (warning-free plan or an older run predating this pass).
- **Over-cap advisory (#2053)**: `runPlanWarnings` also appends a deterministic, count-derived over-cap advisory when the resolved implement-stage `max_files_changed` cap is `> 0` and `len(scope.files) > cap`, via `overCapWarning`. The decode is `json.Unmarshal` (NOT `plan.Parse`) so the advisory stays independent of `semanticCheck` — a `plan.Parse` failure (a decomposition/split structural error) would otherwise suppress the count-derived advisory for a plan the operator still needs to see flagged.
- **Near-cap advisory (#2492, E50.14)**: `nearCapWarning` (appended immediately after `overCapWarning`, before `phaseCapWarnings`/`irreducibleWarning`) makes the SHARED whole-plan budget visible at the plan gate. It fires when the plan is NOT over cap but leaves `capLimit - count <= nearCapMargin` files of headroom (`nearCapMargin = 3`, a package const pinned by `TestRunPlanWarnings_NearCap_ThresholdBoundary` — fires at headroom 0..3, silent at 4+; the headroom-0 row is seeded `count == capLimit`, an at-cap plan that is admissible but has zero headroom, so it discriminates near-cap from over-cap). The advisory names the count, cap and remaining headroom and states that once the headroom is spent the plan-approval scope-cap gate (`checkPlanScopeCap`) and the mid-stage scope-amendment headroom check (`amendmentHeadroom`/`effectiveScopeHeadroom`) refuse any further file — so a correct mid-stage fix needing an un-scoped file has no path in without a re-plan or a governed cap raise.
  - **Decomposition-aware**: when the plan carries a decomposition of `>= 2` sub-plans a second sentence names the slice count and states plainly that all N slices draw against this ONE whole-plan budget (the 'more prominently for a decomposed plan' half of the done-means). A single-slice (schema-forbidden: `sub_plans` `minItems=2`) or absent decomposition renders only the base sentence.
  - **Mutual exclusion is structural.** `nearCapWarning` and `overCapWarning` route through the SAME shared `overCapByCount` cap resolution, and `nearCapWarning` returns `""` the moment `over` is true (the over-cap advisory owns that case), so the near-cap and over-cap advisories can never both fire for one plan and the append is a no-op for every over-cap plan (the existing over-cap/irreducible/phase-cap warnings are byte-identical).
  - **Fail-open on four legs** (all via the shared `overCapByCount` `!ok`): nil `RunRepo`, `GetRun` error, no implement stage, and an implement stage declaring no `max_files_changed` — each skips the advisory and never blocks the settle. Pinned one-per-leg by `TestRunPlanWarnings_NearCap_FailOpenLegs`; the write-only-when-non-empty contract is pinned by `TestRunPlanWarnings_NearCap_AbsentWhenAmpleHeadroom`.
- **Unlandable advisory (#2415)**: `capUnlandableWarning` (appended immediately after `nearCapWarning`, before `phaseCapWarnings`, so the #2053 over-cap-first ordering is untouched) fires when the cap resolves AND the plan's minimum PHYSICAL changed-file count (`planMinChangedFiles`) exceeds it — the state in which `--override-scope-cap` is refused at approval. It states plainly that the plan CANNOT land in this run even with the override (the implement stage re-checks the real diff against a cap fixed for the run) and names the two levers: `remove_scope_files`, or a governed `max_files_changed` raise plus a fresh run. It reads the PHYSICAL count, so a rename-shaped plan whose declared count is over cap but whose physical count fits stays silent. Fail-open on the same shared `overCapByCount` `!ok` legs.

### Scope-cap override is conditional within a run (#2415)

- **The refusal.** `checkPlanScopeCap` (`approvals.go`) still refuses an over-**declared**-cap approve `422 plan_violates_scope_cap`, but `--override-scope-cap` is no longer an unconditional escape. When the override is present AND the effective scope's minimum physical changed-file count (`minPhysicalFileCount`, computed over `effectiveScopePathSetWithOps`'s ops map) exceeds the cap, the approve is REFUSED `422 plan_scope_cap_override_unavailable` (pre-insert — no approval row, stage untouched) with a `plan_scope_cap_override_refused` audit entry (system actor). The message names `constraints.max_files_changed`, both counts (`scoped_files` and `min_changed_files`) against the cap, and the two levers (`remove_scope_files`; or a governed cap raise plus a fresh run). When `min_changed_files` fits the cap the override still succeeds, and the `plan_scope_cap_override_acknowledged` payload now carries `min_changed_files` plus a `note` that it covers the declared-scope pre-check ONLY. The no-override `plan_violates_scope_cap` details also carry `min_changed_files`, and its message appends the "override will not help" sentence when `min_changed_files` already exceeds the cap.
- **Why categorical is safe within a run.** The cap is IMMUTABLE for an in-flight run: `checkPlanScopeCap` resolves it via `resolveImplementConstraints` (`scope_precheck.go`) and the post-implement gate via `loadStageConstraintsFromCache` (`trace.go`), and BOTH parse the same `runs.workflow_spec` snapshot (cached at run creation) with the same min-wins merge. Raising `.fishhawk/workflows.yaml` cannot change this run's cap, so an override that cannot fit today can never fit later in this run — refusing at the plan gate costs nothing, while granting it costs a full run before the implement stage re-check rejects the real diff category-B (the #2415 defect).
- **The estimate.** `minPhysicalFileCount` is deliberately GENEROUS (a lower bound on the physical count): it exempts generated/vendored paths exactly as `policy.CountedFileCount` does, then returns `others + max(creates, deletes)` because git rename detection collapses at most `min(creates, deletes)` declared delete+create pairs into single `R` rows. It is minimal only over DECLARED operations — an undeclared create git would pair with a declared delete as a rename makes the true physical count lower, so the refusal can only over-estimate the count (never admit an over-cap landing) but MAY refuse a rename-heavy plan whose real diff would fit; `remove_scope_files` at the gate is the in-loop escape. The schema has no `rename` operation, so a rename-shaped plan (declared over cap, physical at/under cap) keeps the override — it has no other way to declare itself.

### max_files_changed stays a whole-plan cap (#2492 decision)

**Decision.** `max_files_changed` remains a WHOLE-PLAN cap in v0, unchanged. The near-cap advisory above is the whole of #2492's implementation; the cap itself is NOT re-shaped into a per-slice sub-budget, and the issue's item 3 (a split-time reserved-headroom mechanism) is deliberately not implemented here.

**Rationale.** Cap ENFORCEMENT is already per-slice: the runner (`runner/internal/constraint`) counts each stage's OWN diff, so a decomposition child is evaluated against its own file diff and a slice cannot fail on a sibling's files. What is genuinely SHARED is (a) the plan-time budget and (b) the amendment headroom that `effectiveScopePathSet` computes over the PARENT plan's `scope.files` — which is exactly what #2492 reported blocking three correct mid-stage fixes. A derived per-slice sub-budget would need a plan-schema field, a per-slice headroom computation, and a cap-resolution path that distinguishes parent from child — a schema + spec change, not a local fix. This change instead makes the shared budget VISIBLE at the one gate (plan approval) where the operator can act on it, which sharpens the issue's own framing rather than expanding scope.

**Reopen conditions.** Revisit a per-slice sub-budget (or the reserved-headroom proposal) when EITHER (a) repeated near-cap DECOMPOSED plans show the shared whole-plan budget is the recurring blocker in practice (the advisory's decomposition clause is the signal to watch), OR (b) the #2052 split-proposal work reaches a point where reserving per-phase headroom at split time is the natural home for the mechanism. Cross-references: #2052 (split reachability/headroom), #2363, #2415 (governed cap raise is the current escape hatch), #2412 (irreducible declaration + per-phase cap refusal). This subsection is written to lift into an ADR body verbatim should the operator choose to promote it to a numbered ADR.

### Plan-gate over-cap split reject (`plan_warnings.go` / `plan.go`, #2055, E50.3)

`backend/internal/server/plan_warnings.go::overCapSplitRejection` — called from `handleShipPlan` (`plan.go`) right before `runPlanReviews`. This is the **SERVER-AUTHORITATIVE, count-derived HARD reject** for an over-cap monolith — the E50 keystone, distinct from the advisory above.

- **Count-derived, flag-independent.** `overCapByCount` factors the #2053 count determination (`resolveImplementConstraints` → `len(scope.files)` vs `MaxFilesChanged`) into a shared helper that `overCapWarning` (advisory) and `overCapSplitRejection` (reject) both call; it **never reads `over_cap`**. `overCapSplitRejection` returns a reject reason when the plan is over cap **by count** AND carries no `split_proposal`, and `""` otherwise — so an over-cap-by-count monolith without a split is rejected whether `over_cap` is omitted, `false`, or `true`; an over-cap plan carrying a valid `split_proposal` is accepted; an under-cap plan is unaffected.
- **Decodes with `json.Unmarshal`, not `plan.Parse`** (in `handleShipPlan`): decoding without `semanticCheck` keeps the gate flag- AND parse-independent, so it fires on the server-derived count alone for `over_cap` `{omitted, false, true}` alike and no in-artifact semantic error can preempt the authoritative count reject.
- On a non-empty reason: emit a terminal `plan_review_failed` audit entry (`emitReviewFailed`), fail the plan stage category-B (`run.FailStage` + `advanceAfterFailure`, the same terminal path the plan-invalid/decomposition reject uses), and set `gatingRejected` so `advancePlanStageTerminal`/`notifyPlanReadyIfReady` are suppressed — the rejected plan never advances, and the artifact is still stored so the operator can inspect it via `fishhawk_get_plan`.
- **Fail-open** on every leg exactly like the advisory (nil `RunRepo`, `GetRun` error, no spec/implement stage, unresolved/zero cap → `overCapByCount` returns `ok=false` → `""`), so an unresolved cap can never spuriously block a plan.
- There is deliberately **no** `over_cap ⇒ split_proposal` coupling in `semanticCheck` (`backend/internal/plan/validate.go`). Because that check had no view of the resolved cap it was count-blind, and rejected an **under-cap** plan that merely set the `over_cap` hint — turning the advisory into a server rejection across every `plan.Parse` caller (the plan reviewers plus the fail-open scope/surface/test gates) and breaking the under-cap-unaffected guarantee. It was removed (#2055 fixup); `overCapSplitRejection` is the sole authoritative over-cap reject, and `semanticCheck` only validates the **structure** of a `split_proposal` that is present (`checkSplitProposal`).

### Irreducible declaration + per-phase cap refusal (`plan_warnings.go` / `split_filing.go`, #2412, E32.44)

Two additions close the three defects #2412 reports: a planner facing a compile-atomic over-cap change can honestly DECLINE a split, and split-filing refuses to emit a lead phase that is itself over cap.

- **Irreducible is the ONE plan-artifact field an enforcement path reads — but only to WIDEN.** `overCapSplitRejection` gains a `if parsedPlan.Irreducible.Declared() { return "" }` **after** the count decision and the `split_proposal` short-circuit, so a well-formed `irreducible` (a non-blank `rationale`, checked by the shared `plan.Irreducible.Declared()`) with no `split_proposal` converts the count-derived HARD reject into an operator-decidable advisory. The guarantee is **positional, not merely intended**: the `!ok || !over` return precedes everything, so an under-cap plan returns before the field is read — no under-cap plan can change behaviour, reconciling this with the #2055 rule that a read field must never make the gate *reject* a plan the count says is fine. `plan.semanticCheck::checkIrreducible` rejects `irreducible`+`split_proposal` (contradictory) and a whitespace-only rationale (a bare unjustified flag).
- **Irreducible never suppresses the count advisory.** `runPlanWarnings` appends the count-derived `overCapWarning` FIRST, then `phaseCapWarnings` (one advisory per phase whose own `scope.files` count exceeds the cap) and `irreducibleWarning` (surfaces the rationale as a *challengeable* claim and states plainly the declaration does NOT make the change landable — implement re-checks `max_files_changed` against the real diff, so it still needs a governed cap raise). Ordering is the observable proof of non-suppression; both #2412 legs fail open on an unresolved cap.
- **Split-filing refusal (`fileSplitProposalChildren`).** Immediately after resolving the cap and BEFORE building/classifying/filing any child, the hook calls `splitfiling.PhaseCapViolations`; when any phase is over cap it files **zero** children, writes ONE `split_filing_refused` marker (registered in `audit.KnownCategories`, surfaced via `fishhawk_get_plan` `split_filing.refused`) naming every offending phase, and posts a parent refusal comment — so #2410 (a 160-file lead phase against a cap of 45) is never emitted. The marker append is a **hard prerequisite** (on failure it logs and returns WITHOUT the comment, mirroring `writeSplitChildFiledMarker`), and it **dedups on a prior `split_filing_refused`** so a re-approval of a still-over-cap proposal no-ops. `capFiles == 0` (unresolved cap) yields no violations, so filing proceeds unchanged. Split-filing still does **not** verify independent landability (path-keyed couplings): every child body carries `splitfiling.LandabilityCaveat` and the non-contract done-means drops the unsatisfiable "compiles as an at-or-under-cap intermediate" claim.

### `applies_to` workflow routing, fail-closed in two phases (`applies_to.go` / `applies_to_plan_gate.go`, E53.3 / #2226, ADR-066 fork 4)

A workflow's optional `applies_to` is a `$defs/predicate` (E53.1 / #2224) declaring **which changes may be routed through that workflow**. Enforcement is **fail-closed** and split across two points, because the predicate's criteria do not all have a producer at the same moment (ADR-066 fork 4 ratified enforcement "at `start_run`"; the operator's 2026-07-30 ruling §1 refined that to "at the earliest point each criterion has a producer").

The **pure evaluation core** — the two-phase split, the satisfying-workflow enumeration, the ONE message renderer, and the trigger/label `Change` builders — lives in **`backend/internal/appliesto`** (see its `README.md`), extracted verbatim (E53.10 / #2361) so it can be shared with the webhook dispatcher without an import cycle (`server` already imports `webhook`). This file is the HTTP/audit-shaped ADAPTER for `POST /v0/runs`; `applies_to_plan_gate.go` is the plan-gate adapter; the webhook admission gate (`webhook/applies_to_admission.go`) is the third seam. All three delegate to `appliesto.*` and pass a `Seam` discriminator so the trailing ways-forward sentence differs while the binding message shape does not.

| Criterion | Enforced at | Against | Code |
|---|---|---|---|
| `labels` | run admission, `POST /v0/runs` | the run's `issue_context.labels` snapshot | `applies_to.go::checkAppliesTo` |
| `labels` | webhook admission (`issues.labeled`, `/fishhawk run`) | the forge-authoritative `issue.labels[]` on the event | `webhook/applies_to_admission.go::refusedByAppliesTo` |
| `trigger` | run admission (both admission seams) | the run's `trigger_source` → `spec.TriggerForm` | `checkAppliesTo` / `refusedByAppliesTo` |
| `paths` | the **plan gate** (`handleShipPlan`) | the approved plan's `scope.files` union | `applies_to_plan_gate.go::appliesToPlanGateRejection` |

- **The `paths` deferral is what makes the design sound, not what weakens it.** At admission a code change has proposed no diff, so evaluating `paths` there could only match against zero paths — which the AND-across-types rule turns into a blanket refusal of every run. The first authoritative path set is `scope.files`, which is **binding rather than descriptive**: the scope pre-check plus the runner's post-hoc constraint check confine the implement stage to it. A run admitted under `paths: ["docs/**"]` and cleared at the plan gate is therefore *confined* to docs-only, not merely claimed to be. **Both** rejection points fire before any implement work, so a refusal costs a re-run and never half-applied work.
- **The `paths` quantifier is UNIVERSAL, and that is a deliberate divergence from `Predicate.Match`'s default.** `spec.Predicate.Match`'s `paths` rule is *existential* — any change path matching any glob satisfies it — which is right for its other consumers (`escalations`, ADR-068 conventions) because they ask "does this change *touch* such a file?". It is wrong for a confinement control: under existential semantics a plan scoping `[docs/x.md, backend/everything.go]` would satisfy `paths: ["docs/**"]` and the confinement guarantee above would be false. `planGateUnmatchedPaths` therefore applies the ∀ quantifier **over the ratified matcher used verbatim** — each path is handed to `Predicate.Match` one at a time and every one must be accepted. No second matcher is written and `predicate.go` is untouched (operator ruling §3). `TestAppliesToPlanGate_PartiallyMatchingScope_Rejects` is the pin: it is the only test that fails if this is "simplified" back to one `Match(union)` call.
- **The evaluated path set is the UNION, not the top level**: `scope.files` ∪ every `decomposition.sub_plans[].scope.files` ∪ every `split_proposal.phases[].scope.files` (`planGateScopePaths`, slash-normalized / de-duplicated / sorted). A decomposition fan-out child runs bounded to its **slice** scope, so checking only the top level would let a slice escape the declaration entirely — the same structural reason `scopedPaths` in `scope_regression.go` had to cover sub-plan scopes (#1257).
- **One renderer, three seams.** Every rejection point builds `appliesto.Rejection` and calls `appliesto.RenderRejection`, so all carry the same shape: the workflow, the criterion that failed, the value observed, and **which workflows would accept this change**. An operator refused at the plan gate is further into a run than one refused at admission and needs more help, not less. Only the trailing ways-forward sentence keys off the `Seam` field: `SeamStartRun` and `SeamPlanGate` advertise `applies_to_override`; `SeamWebhook` does NOT (a webhook trigger carries no operator request, so there is nowhere to pass one) and names re-starting through `POST /v0/runs` / the CLI instead. `TestRenderRejection_AllThreeSeamsCarryTheSameShape` (in `appliesto`) pins the shape parity and the tail divergence.
- **The alternatives list is filtered at BOTH phases, and at the plan gate that means the run's labels and trigger too.** The operator's next move after a plan-gate refusal is to re-start under a named alternative, and that re-start hits **admission** first — so a candidate filtered on `paths` alone lists every labels-only or trigger-only workflow unconditionally (they do not constrain the plan gate at all) and walks the operator from one fail-closed refusal into another. The run's labels (`issue_context.labels`) and trigger are immutable for its lifetime, so `resolveRunWorkflowDef` rebuilds the admission-phase `spec.Change` from the same run row (`runAdmissionChange`) and `planGateSatisfyingWorkflows` runs each candidate through `evaluateAppliesTo` at admission **before** the ∀ paths check. Pinned by `TestPlanGateSatisfyingWorkflows_AlsoFiltersOnAdmissionCriteria` (the combined label-incompatible-but-path-compatible edge) and `TestAppliesToPlanGate_RejectionNamesOnlyUsableAlternatives` (the same invariant through the real gate and the real run row).
- **The phase split is exhaustive over the predicate grammar, and a TRIPWIRE — not prose — is what keeps it that way.** `appliesto.PhasePredicate` is a hand-maintained field-by-field copy of `spec.Predicate` whose `constrains` check enumerates today's four criteria by name, so a *fifth* criterion added by a sibling slice (#2227 `escalations`, #2211 review conventions) would be picked up by the `applies_to` `$ref` automatically and copied into **neither** half — declared and silently never enforced, the failure mode the function's own comment names for `change_kind`. `TestAppliesTo_PhaseSplit_IsExhaustive` (now in the `appliesto` package) therefore asserts against the struct via `reflect`: a field-count check (with the actionable message: route it into a phase half **and** `constrains`, extend `appliesto.FirstFailingCriterion` — naming all THREE consumers) plus a per-field check that every criterion survives into at least one half. A new criterion fails the test rather than shipping unenforced.
- **Terminal path adds no audit category.** On a non-empty reason the gate emits `plan_review_failed` (`emitReviewFailed`), fails the stage category-B (`run.FailStage` + `advanceAfterFailure`) and sets `gatingRejected` — byte-for-byte the path `overCapSplitRejection` uses. It runs **after** the artifact is stored and audited, so the operator can read the refused plan via `fishhawk_get_plan`, and it is skipped when the over-cap reject already failed the stage (a stage fails once).

**The override's source of truth is the audit entry, which means the entry is a PRECONDITION of the bypass and not a record written after it.** The audited exception is one concept with one name: `applies_to_override` + a **required** reason on `POST /v0/runs`. It is granted in two writes:

- **The grant** (`grantAppliesToOverride`) appends `run_admitted_applies_to_override` to the **global** chain *before* the run is admitted — the run-scoped entry cannot be written here because there is no run id until the insert. An append failure, or an absent audit repository, **refuses the run** with `503 audit_unavailable` and creates no run row. Warn-logging and admitting anyway would be an unaudited governance bypass, and for an **admission-only** violation nothing downstream would ever catch it: `paths` is re-evaluated at the plan gate, so a lost override there costs a re-run, but a `labels` or `trigger` bypass has no second evaluation point. This is the deliberate inverse of the **refusal** audit, whose append failure is correctly best-effort (`TestAppliesTo_RefusalAuditFailure_StillRefuses`) — there the decision already went the safe way, so an audit problem must not soften it; here it goes the unsafe way, so the audit gates it. Pinned by `TestAppliesTo_M11c_OverrideGrantAuditFailure_RefusesTheRun` and `TestAppliesTo_OverrideWithNoAuditRepo_Refuses`.
- **The carry-forward** (`recordAppliesToOverride`) appends the **run-scoped** entry post-insert. The plan gate suppresses its own refusal by **looking that entry up** (`runHasAppliesToOverride`), never by re-reading request state — audit-entry lookup, no run column, no migration. **This append is a precondition too**: on failure `abandonUnauditedOverrideRun` CANCELS the just-created run and returns `503 audit_unavailable` (`details.run_id` names the cancelled run; the cancel itself is best-effort, the 503 is not conditional on it). Warn-logging and proceeding was the earlier posture, justified by "the plan gate re-rejects a deferred `paths` violation anyway" — true for `paths` and false for the violation the override most often forces past. An **admission-only** (`labels`/`trigger`) violation has no second evaluation point, so that run would complete on a bypass its **own** audit chain does not carry: a global-chain entry no run-scoped query returns is not the run's source of truth. No entry, no run — the same answer both writes give. `TestAppliesToPlanGate_OverrideAbsent_Rejects` and `TestAppliesTo_M11b_OverrideLookup_IsAuditEntryNotRequest` pin it (the latter asserts the 503, the cancelled row, and that the lookup still reports no override).

**The override is recorded independently of whether ADMISSION refuses, which is what makes it reachable for the deferred `paths` criterion.** Deriving the record from the refusal — the obvious reading, since admission is where the refusal happens — yields an override that exists only when admission itself refuses, and so is unavailable in the **common** case: a workflow whose `labels`/`trigger` the run satisfies (or which declares neither, the paths-only declaration) and whose `paths` the plan will not. That operator's only recourse would be widening the declaration permanently, which is the behaviour the override exists to avoid. `TestAppliesTo_M11d_Override_CarriesForwardWhenAdmissionPasses` covers both shapes; `TestAppliesTo_AdmittedRun_RecordsNoOverrideEntry` is the control that an ordinary run — one that asked for no override — still acquires none.

**The empty-label-set rejection has two sentences because it has two causes.** An issue-**less** run (`trigger_source` `cli`/`ui`, no issue context) and an **unlabelled** issue (issue context present, zero labels) share the branch and have different fixes, so telling the operator of an issue-triggered run that it "carries NO issue context" sends them hunting a trigger problem they do not have (`appliesToRejection.IssueContextPresent`). Both sentences still report the **observed value** (`[]`): the message shape is binding on every rejection path, and the empty-set branch is exactly where dropping it as uninteresting is tempting (`TestAppliesTo_M4b_UnlabelledIssue_ReportsTheObservedValue`).

**Degrade posture is deliberately ASYMMETRIC to this handler's advisory neighbours, and the line is drawn at a specific place rather than applied as a mood:**

- **Once a declaration is in hand, an evaluation failure REJECTS.** A `Predicate.Match` error (empty predicate, malformed glob) and an override-lookup error are both refusals — an override that cannot be *confirmed* is not an override. `runScopePrecheck`, `runSurfaceSweep`, `runTestSweep` and `overCapSplitRejection` all correctly fail **open** on their error legs because they are advisory sweeps whose worst case is a missing hint; this is a governance control whose worst case is an unenforced routing declaration. Writing `if err != nil { return "" }` here by analogy with the four neighbours sitting in the same handler is the specific bug this posture exists to prevent (`TestPlanGateUnmatchedPaths_MalformedGlob_FailsClosed`, `TestAppliesToPlanGate_OverrideLookupError_Rejects`).
- **Where NO declaration EXISTS to resolve, the gate fails open** (nil `RunRepo`, run not found, nil workflow spec, unparseable spec, workflow absent from the spec — each warn-logged). There is nothing to enforce, and refusing every plan in a deployment that cannot resolve a declaration would be a denial of service rather than a control. The leg is narrow by construction: the bytes parsed here are the **snapshot admission already parsed successfully**, so a parse failure at plan time is an internal inconsistency, not a reachable bypass.
- **Where the declaration could not be READ, the gate fails closed.** A `GetRun` error that is *not* `run.ErrNotFound` — a transient database failure, a timeout, a reset connection — is an **absence of knowledge**, not the fact "there is no declaration", and only the second is a safe reason to admit. `resolveRunWorkflowDef` returns the two separately (`ok bool, err error`) precisely so a caller cannot collapse them by reading one flag. Collapsing them would leave a governance control that any repository hiccup silently switches off, and switches off exactly when it matters: a plan that *satisfies* the declaration is admitted either way, so the only plan a blind gate changes the verdict on is one that would have been refused. The refusal is retry-shaped — the failure is transient, so re-shipping the plan recovers, whereas a silent admission never does (`TestAppliesToPlanGate_TransientReadFailure_FailsClosed`, `TestResolveRunWorkflowDef_SeparatesAbsenceFromUnreadability`).
- **A plan with an EMPTY scope is refused, not admitted on a vacuous ∀.** The zero-path case delegates to the predicate's own ratified answer (`(false, nil)` against zero paths) rather than to "every one of zero paths matched", which would be the one hole in the posture.
- **The gate's call site sits OUTSIDE `handleShipPlan`'s `json.Unmarshal(body, &parsedForCap)` guard, and an undecodable body is a zero-path plan rather than a skip.** That decode failure is a documented fail-**open** for the *advisory* over-cap gate it shares the block with; nesting this call inside it would hand a fail-closed control a fail-open leg that is **not** among the enumerated ones above — which is precisely the "narrow by construction and enumerated" claim this posture rests on. So the gate runs either way and is passed a `nil` plan when the decode failed (the partially decoded struct is deliberately not reused: a half-populated scope would let a truncated body attest a confinement it never demonstrated). A `nil` plan refuses under a `paths` declaration and still clears a workflow that declares none, so the leg cannot become a blanket plan-ship outage. Like the malformed-glob branch it is **not reachable through a stored body today** — `body` has already passed `plan.Validate` against the closed `standard_v1` schema — so it is pinned where the contract lives (`TestAppliesToPlanGate_NilPlan_Rejects`) rather than through a faked end-to-end path.
- The `Match`-error branch is **defense in depth and not reachable through a stored spec today**: slice 0's `validateWorkflow` calls `Predicate.Validate` at parse time, so a malformed glob fails `spec.ParseBytes` at admission before a run row exists. It is asserted directly against the pure function that owns the posture rather than through a faked end-to-end path.

**Trust boundary (documented, not implied).** On `POST /v0/runs` labels are fetched by the *caller's* `gh` and shipped inline on the create request, so a caller determined to route a change through the wrong workflow can attest whatever labels it likes. `applies_to` prevents **misrouting**, not a determined authorized caller; that is an accepted boundary for a routing control whose sanctioned exception is an audited override, and server-side label fetching is the named hardening follow-up. The **webhook** seam (E53.10 / #2361) is stronger on both counts: it evaluates against the forge's own `issue.labels[]` rather than a caller attestation, and it carries NO override (the event carries no operator request) — so its refusal is unconditional and the refusal comment names amending the declaration or re-starting through `POST /v0/runs` / the CLI. Overlap is benign by construction: `applies_to` *filters* an operator-named `workflow_id`, it never *selects* one, so two matching workflows is not a coin flip.

### Plan-gate scope-regression sweep (`scope_regression.go`, #1257)

`backend/internal/server/scope_regression.go::runScopeRegression` — called from `handleShipPlan` (`plan.go`) immediately after `runTestSweep` and before `runPlanReviews`, but ONLY on a **revise pass**.
`fishhawk_revise_plan` regenerates the WHOLE plan artifact, so a narrowly-scoped revision constraint can silently DROP files the immediately-prior (revision-base) plan scoped — even one that says "keep everything else" — and the runner then scope_drift-excludes the agent's edits to them (#1257).

- The gate's run-guard is a prior `plan_revised` audit entry (`countRevisePasses > 0`, the same durable revise-pass record the bound is counted against); the revision **base** is captured via `loadApprovedPlanForRun` **BEFORE** `ArtifactRepo.Create` (an after-Create capture would diff the just-shipped plan against itself and report no regression).
- `scopedPaths(*plan.Plan)` is the pure helper: the slash-normalized (`filepath.ToSlash`), sorted, de-duplicated UNION of `plan.Scope.Files[].Path`, every `plan.Decomposition.SubPlans[].Scope.Files[].Path`, AND every `plan.SplitProposal.Phases[].Scope.Files[].Path`.
  The observed regression dropped files living in decomposition sub-plan slice scopes, so the diff MUST cover sub-plan scopes, not only the flat top-level list. **Split-phase scopes joined the union with the refusal (#2516)**: under an advisory, reporting a file *moved into* a split phase as dropped was a tolerable false positive; under a refusal it would refuse a revise that merely rearranged the same file set. This now matches the sibling `planGateScopePaths`, which already unioned all three.
- `runScopeRegression` computes `removed = base-scoped − new-scoped` (every dropped path) and `added = new − base`, then splits `removed` by the new plan's top-level `scope_removals` declarations into `declared_removals` and `undeclared_removals` (paths slash-normalized on both sides; an entry naming a path that was NOT dropped is a harmless no-op, exactly as an unmatched `surface_sweep_exemption` is). It writes a `plan_scope_regression` audit entry (payload `ScopeRegressionPayload{removed_files, declared_removals, undeclared_removals, added_files, scanned_files, regressed}`, all four lists empty arrays not null on a clean diff).
- **`Regressed` is `len(undeclared_removals) > 0`, NOT `len(removed_files) > 0` (#2516).** `removed_files` keeps its prior meaning, so no existing reader breaks, but the bit the refusal AND the budget refund key on now excludes a declared drop: a declared narrowing neither refuses nor refunds, because it is not a mistake. (This is a behaviour change to the existing refund mechanism, not purely additive.)
- Advisory + fail-open: `base==nil` (non-revise ship), a nil `AuditRepo`, a parse failure, OR an audit-append failure WARN-logs and returns nil — never blocks or unwinds the ship. **The REFUSAL built on top of it lives in the caller** (`plan.go::tryScopeRetry`), documented below.
- The returned payload threads into the plan-review prompt's gate-evidence section as a HIGH-severity signal (`prompt.ScopeRegressionEvidence`, rendered only when `RemovedFiles` is non-empty) so BOTH reviewers and the operator see the drop before approving.
- **Budget refund seam**: `backend/internal/server/revise.go::countRegressedRevisePasses` counts the stage's `plan_scope_regression` entries with `regressed==true`.
  `handleRevisePlan` sets `run.ReviseOptions.BudgetPassCount = max(0, priorPasses − regressedPasses)` so a regressing revise pass refunds the NORMAL revise budget (the operator gets a free recovery pass), while `PriorPassCount` keeps governing the HARD CEILING (`defaultReviseCeiling` = 3) so the refund cannot create an unbounded revise loop — total work stays bounded.
  `RevisePlanStage` compares `BudgetPassCount` against `MaxPasses` for `ErrReviseBudgetExhausted` and derives `remaining`/`forced` from it, leaving the ceiling check on `PriorPassCount`.

#### Undeclared-narrowing REFUSAL (`plan.go::tryScopeRetry`, #2516)

The sweep above is advisory: it records the drop, but the narrowed plan still walks to the gate. That spends a **reviewer** pass and an **operator revise** pass on a plan whose only remedy is another revise that can drop again — the pain #2516 describes. `handleShipPlan` therefore calls `tryScopeRetry` immediately after `runScopeRegression` and **before** the over-cap gate, when `regression != nil && regression.Regressed`, and on `true` sets the existing `gatingRejected` flag (reusing the advancement suppression, so the refused stage never walks to `awaiting_approval` and no plan-ready ping fires). The artifact is stored and audited before every gate, so the operator can still inspect the refused plan via `fishhawk_get_plan` — matching the over-cap reject's documented behaviour.

- **Budget**: `maxPlanScopeRetries = 1`, enforced by the shared `consumeRetryBudget` helper against the run's `plan_scope_retry` audit entries. It **fails closed**: on budget exhaustion OR any check-and-consume error it declines the refusal (an unreadable/unwritable budget is an *unbounded* one — a persistent audit failure would otherwise grant a refusal on every corrective ship and the one-shot would never exhaust), and the plan falls through to park-with-evidence with the refunded revise pass. The check-and-consume is now **atomic under concurrency (#2518)**: under the production repo it counts under a `SELECT … FOR UPDATE` row lock and appends in ONE transaction (`audit.AppendChainedUnderBudget`; lock-then-count-then-append, READ COMMITTED), so two racing ships cannot both read `0` and both refuse — the one-shot is strictly one-shot. This supersedes the earlier over-consume-but-`>=`-bounds argument and the "would need a unique constraint on `(run_id, category)`" note: no index over the append-only, hash-chained table is used, because its race-produced duplicates cannot be de-duplicated without breaking the chain the verifier depends on. The reachability invariant justifying this — that concurrent same-stage ships are genuinely reachable — is the "Plan-ship concurrency" note below.
- **The `FailStage` reason is capped** at `maxSchemaValidationErrorBytes` (4000), like the sibling schema-retry path's `validation_error`: nothing bounds how many paths a revise can drop, and a stage failure reason is surfaced back to agents and operators.
- **Payload keys** on the `plan_scope_retry` entry: `{run_id, stage_id, attempt, undeclared_removals, required_scope_files}`. `required_scope_files` is the base-scoped union **MINUS** the declared removals — derived SERVER-SIDE from the revision base, never asserted by the planner — i.e. the exact enumerated set the corrected plan must cover. The entry is BOTH the budget counter and the prompt's feedback source.
- **Ordering is consume-first**, mirroring `trySchemaRetry` and `handleRetryStage` so the retry intent is durable even if a later step fails: `consumeRetryBudget` (which commits the `plan_scope_retry` entry atomically) → `FailStage(FailureA)` → `RetryStage(pending)` → `Orchestrator.Advance`. The transient category A never leaks into the run's terminal state. A racer that finds the stage already re-opened to `pending` is absorbed by `failStageCAS`'s bounded RE-ANCHOR loop (`run/failure.go`), which re-fails from any live, legally-failable state — so the budget primitive, NOT the stage CAS, is what bounds the refusal to one (an earlier comment claiming "at most one racer's re-open wins" overstated the CAS's role and was corrected).
- **Return contract, per leg** (each leaves a *different* observable state, and each has its own behavioural test):

  | Leg | Returns | Committed state after |
  |---|---|---|
  | nil `Orchestrator` / nil `AuditRepo` / budget exhausted / atomic consume error / fallback read error | `false` | Nothing NEW mutated; caller's fall-through parks the stage at `awaiting_approval` with the evidence |
  | `FailStage` error | `false` | The entry IS committed and the budget IS consumed, but the stage never left its state → fall-through parks it at `awaiting_approval` |
  | `RetryStage` error | `false` | The entry is committed AND the stage is left at `failed` (transient category A, operator-recoverable via `retry_stage`). It does NOT reach `awaiting_approval` |
  | `Advance` error | **`true`** (log-and-continue) | The stage is already re-opened to `pending`; returning `false` would fall through to normal advancement holding a pending stage — the stranded state. The refusal stands; only the auto-dispatch is missing, which on the local runner is the normal case anyway (ADR-024) |

- **Exhaustion degrades, it never fails terminally.** With the budget spent the gate falls through UNCHANGED to today's behaviour: the plan proceeds to review carrying the regression evidence, and `revise.go`'s refund gives the operator their pass back. Contrast the **over-cap reject**, which IS terminal (`plan_review_failed` + category-B). A refusal must never become a new way to lose a plan.
- The over-cap gate is skipped when the refusal already fired: a stage is failed once, and layering a terminal category-B reject onto a stage already re-opened to `pending` would destroy the recoverable refusal.
- **Carry-forward prompt channel** (`prompt.go::loadScopeCarryForward`, populated at BOTH plan-prompt build sites): resolution order is load-bearing — the newest `plan_scope_retry` entry **for the stage** wins (its `required_scope_files` is the authoritative carry-forward set, its `undeclared_removals` the refusal notice); only when none exists does it fall back to deriving the set from the newest plan artifact through `scopedPaths`. On a corrective re-dispatch the newest plan artifact is the REFUSED narrowed plan, so deriving carry-forward from it would cement the very drop the retry exists to undo. Best-effort/warn-only: an audit read error, an undecodable payload, or a missing artifact yields `(nil, nil)` and the prompt renders as it does today. The first two legs return `(nil, nil)` *without* trying the artifact fallback — both mean "a refusal may be recorded and I cannot read it", and falling back would render the REFUSED narrowed plan's scope under the authoritative heading, cementing the drop; rendering nothing is strictly better. The artifact fallback itself does not test for a revise, so the "first-pass plan prompts stay byte-unchanged" guarantee lives at the RENDERER (`buildPlan` writes the list only inside its `RevisionConstraint != nil` block, and a first-pass dispatch records no `plan_revised`) — pinned by `TestGetStagePrompt_Plan_NonRevise_NoCarryForwardSection`.
- **Path rendering is sanitized** (`prompt.go::sanitizeScopePath`): the paths are server-derived from the machine diff, but their CONTENT is planner-authored and lands inside Fishhawk's own binding prompt sections. Every line terminator and control character is escaped to a visible two-character form and fences are broken, so a crafted path cannot end its `- ` list item and land attacker-chosen text at column 0 where it could impersonate a trusted banner. An ordinary path passes through byte-unchanged.
- The renderer (`prompt.Trigger.RevisionBaseScopeFiles` / `ScopeRestoration`) enumerates the carry-forward set inside the **Revision constraint** section, right after the base-plan blob, stating plainly that the blob is TRUNCATED at 4000 bytes and the list — not the blob — is authoritative. On a 40+ file plan the raw-JSON base greatly exceeds that cap, so a planner told to "not discard the parts the constraint does not touch" could not see the scope it was asked to preserve; the enumeration makes it visible regardless of whether truncation was the cause.

#### Plan-ship concurrency (why the budget window needs its own atomicity, #2518)

`handleShipPlan` is a plain authenticated HTTP handler with NO per-stage serialization ahead of the budget window: the only per-run lock in the path is `AppendChainedTx`'s `SELECT … FOR UPDATE`, taken INSIDE the append, i.e. AFTER the count. So two concurrent executions of the same `(run, stage)` can both enter the count→append window. This is reachable, narrowly: the runner's `upload.Client` is built with `http.Client{Timeout: 120s}` (`runner/internal/upload/upload.go`), and `ShipPlan` treats any HTTP `Do` error — including that client-side timeout — as transient and RE-POSTs the identical signed body, so a server-side stall past 120s puts two in-flight executions of the same invalid body into the window.

What DOES serialize same-stage ships is the stage-state CAS (`advancePlanStageTerminal` / the re-open transitions), but it sits AFTER the budget window, so it cannot bound a count taken before it. What does NOT serialize them is the runner-side lineage lock (`runner/cmd/fishhawk-runner/lockholder.go`, `worktree.go`; surfaced as `runner_failed/lineage_lock`): it is a FILE lock over the local checkout's lineage root that serializes runner PROCESSES on one local runner only — it is absent on the `github_actions` path and blind to a single runner's retried in-flight HTTP request, and nothing in `plan.go` consults it. Therefore the budget window needs its own atomicity, which is `audit.AppendChainedUnderBudget`'s lock-then-count-then-append (above). The cross-layer proof is `TestE2E_Revise_ConcurrentScopeRetryShips_GrantsExactlyOneRefusal` (two concurrent signed ships → exactly one `plan_scope_retry` entry, at most one re-dispatch).

## Release endpoints (ADR-051)

### Release publish (`release_publish.go`, E33.3 / #1588, ADR-051 option B publish half)

`handleReleasePublish` (`POST /v0/releases/publish`) takes `{repo, tag, run_id, artifact_id, stage_id?}`, loads the persisted `release_notes` artifact via `ArtifactRepo.Get`, resolves the App installation, and fetches the published Release by tag (`githubclient.GetReleaseByTag`).
It sets the Release body to the notes markdown (`UpdateReleaseBody`) and attaches the notes as the fixed-name `release-notes.md` asset (`UploadReleaseAsset` → the separate `uploads.github.com` host), then records a `release_published` audit entry (`{tag, release_url, artifact_id, content_hash}`, system actor) on the run's chain.

- **Idempotency keys on CONTENT HASH for BOTH surfaces** (binding condition): a full no-op only when the last recorded `release_published` hash AND the live Release body both equal the desired notes hash; otherwise it PATCHes the body AND replaces the asset (`DeleteReleaseAsset`-by-name then upload) so body and asset never diverge.
- Auth mirrors `release_notes.go` (anonymous → 401, bearer needs `write:runs` → 403); nil artifact/audit/GitHub dependency → 503.
- The `releasePublisher` interface (production `*githubclient.Client`; test override `releasePublisherOverride`) is the offline seam mirroring `releaseNotesResolverOverride`.
- No App permission change — the Releases endpoints ride the existing `contents:write` grant (`docs/ARCHITECTURE.md` §8, auth model).

### Release cut (`release_cut.go`, E33.5 / #1590, ADR-051)

`handleReleaseCut` (`POST /v0/releases/cut`) takes `{repo, run_id, artifact_id, version, stage_id?, bump_level?}`, loads the persisted `release_notes` artifact via `ArtifactRepo.Get` + kind-checks it, then records a `release_cut` audit entry (`{repo, version, artifact_id, bump_level, content_hash}`, system actor) on the run's chain — `content_hash` is the artifact's own stored hash (which notes were cut).

- It records the DECISION only: **no git tag push and no GitHub write** — tagging the release stays a human git action per the delegating posture, so cut needs no GitHub client (only the artifact + audit repos; nil either → 503).
- `bump_level` is the optional advisory semver level, recorded verbatim and never validated (mirrors the classifier hint).
- Auth mirrors `release_publish.go` (anonymous → 401, bearer needs `write:runs` → 403).
- The operator drives prepare → preview → cut → (human-led tag push) → publish through the CLI (`fishhawk release …`) and the `fishhawk_release_notes` MCP verb; the release-loop walk is documented in `docs/deploy/release-loop.md`.

### Release-arc seam integration test (`release_seam_test.go`, E33.6 / #1591, ADR-051)

`backend/internal/server/release_seam_test.go` (`package server`, pgtest-backed): the deterministic in-tree proof of the whole release arc in ONE flow.
It covers evidence assembly (`releaseevidence`) → notes render (`releasenotes`) → prepare persist (`handleReleaseNotesPersist`) → cut decision (`handleReleaseCut`) → publish body/asset via a `fakeReleasePublisher` (`handleReleasePublish`) → the run's audit hash-chain.

- It seeds a loop-merged evidence run (approved `standard_v1` plan + both `implement_reviewed` verdicts + an `acceptance_outcome_recorded` entry on the run's chain) and a separate release run, then asserts the seam the per-slice unit tests cannot:
  1. the persisted/published notes body's evidence lines resolve to the SEEDED plan/reviews/acceptance rows (never fabricated — the ADR-051 honesty constraint, with the unmapped PR marked reduced-evidence);
  2. both `release_cut` and `release_published` audit entries land on the release run's chain with the expected payloads (`version`/`artifact_id`/`bump_level`/`content_hash`, and `tag`/`release_url`/`artifact_id`/`content_hash`);
  3. the run's audit hash-chain is verifiable end to end — `prev_hash`→`hash` continuity across the whole chain including the `release_cut`→`release_published` link (the deterministic analogue of the operator's live "release_published verifiable on the chain" Done-means).
- Reuses the in-package `newReleaseNotesHarness`/`seedLoopRun`/`fakeReleaseResolver` and the `fakeReleasePublisher` so the flow is offline.
- The one real published GitHub Release named in #1591's Done-means is an OPERATOR-EXECUTED live walk (real tag push + real Release), unreachable by the sandboxed implement/acceptance agents; the release-loop walk itself is documented in `docs/deploy/release-loop.md`.

## Campaign REST API + rollup status (`campaigns.go`, ADR-047 / #1437, E25.4 / #1456, resume #1460)

`backend/internal/server/campaigns.go` (+ `campaigns_test.go`): the HTTP surface over the E25.2 store and E25.3 assembly.

- `POST /v0/campaigns` is **runless** — it resolves the repo's GitHub App installation directly (the same path `handleCreateRun` uses, no run row), queries the epic's children via the work-management provider, runs `campaign.Assemble`, and persists campaign + items; `pause_policy` (`pause_campaign` default / `pause_item`) is fixed here.
- `GET /v0/campaigns` (cursor-paginated, repo + state filters), `GET /v0/campaigns/{id}`, `GET /v0/campaigns/{id}/items`.
- `GET /v0/campaigns/{id}/status`: campaign + items + `campaign.NextEligible` readiness rollup + the server-computed `next_action`, FAILED-wins precedence: attention > resume > start_run > wait > complete.
  **Reconcile-on-read** as of E26.2 — `reconcileCampaignItemsOnRead` settles any running item whose linked run reached terminal and re-derives the campaign, best-effort + idempotent.
  A second `settleIssueClosedItems` arm (#1558, extended #2029) settles a deps-satisfied item whose GitHub issue is closed-as-completed `succeeded` with `settled_via=issue_closed`, unblocking descendants — same fail-closed posture — in two classes: a **run-less** pending/blocked item (no `run_id` in the marker) AND a **run-linked** item whose linked run went terminal-non-succeeded (cancelled/failed) but was delivered out-of-band, settled via the guard-bypassing `SettleCampaignItemOutOfBand` with the `run_id` retained (present in the marker). An open or `not_planned` closure still never settles either class.
- `POST /v0/campaigns/{id}/runs` (E26.2 / #1481, scope `write:campaigns`; `handleStartCampaignItemRun` — the operator-driven local-drive start): DAG-gates an `issue_ref` via `campaign.NextEligible`, mints the run through `StartRunForCampaignIssue` (a params struct as of E48.69 / #2498) carrying `runner_kind` and `working_dir` — the latter an absolute path bound onto the minted run so every later runner-spawning verb inherits it — links + transitions the item, advances a pending campaign.
  Gate codes: `item_not_eligible`/`campaign_item_not_found` (409+404), `item_human_led`/409 (a deps-satisfied `autonomy:low` item — a human must lead it, no ref named to start, #1697), `campaign_not_startable`/409, `validation_failed`/400 (a non-absolute `working_dir`, mirroring `POST /v0/runs`), `campaign_run_start_failed`/502; **no `idempotency_key`**.
- `POST /v0/campaigns/{id}/resume` (E25.7, scope `write:campaigns`): flips campaign+items `paused`→`running`, `campaign_not_paused`/409 when nothing is paused.
- Gate codes on create: `repo_not_installed`/`campaign_dangling_dependency`/422, `validation_failed`/400 (bad ref or dependency cycle).
- NO request idempotency on create (no `idempotency_key` column; an `Idempotency-Key` header is accepted but not enforced — deferred).
- Source of truth `docs/api/v0.openapi.yaml`; companion `docs/api/v0.md`.

## Read + export surfaces

### Stage + audit read handlers (`reads.go`)

`backend/internal/server/reads.go`; cursor pagination via `pageOffset`/`encodeOffsetCursor`. Serves `/runs/{id}/stages`, `/runs/{id}/audit`, and `/v0/audit`.

- The per-run audit handler is sequence-ascending and serves the run-detail UI.
- The global handler `handleListGlobalAudit` (#211) is time-descending and mixes both chains for the audit-search surface, with optional `category` and `run_id` filters via `audit.ListAllParams`.
  Distinct from the repository's `ListGlobal`, which is the verifier's view of the global-chain partition only (per-row `run_id IS NULL`).
- `GET /v0/runs/{id}/audit` accepts `?chain=true` to call `audit.Repository.ChainsByParent(runID, false)`, returning entries for the parent run and all CI-retry descendants (excludes decomposed children where `decomposed_from IS NOT NULL`).

### Audit compliance export (`audit_export.go`, E9.1 / #1604)

`backend/internal/server/audit_export.go::handleAuditExport` (`GET /v0/audit/export`) — the producer half of the verifier's `Export v1` wire contract (ADR-008 / ADR-054).

- Assembles `{schema:"v1", exported_at, runs}` from `audit.Repository.ListForRun`/`ListGlobal` + `run.Repository.ListRuns`/`GetRun` + `signing.Repository.Get`, with wire structs mirroring `verifier/internal/audit/export.go` tag-for-tag (the BINDING contract; the verifier's `ParseExport` uses `DisallowUnknownFields`).
- Whole-run page bounding (never splits a chain) with partiality + an opaque keyset cursor carried in the `X-Fishhawk-Export-Complete` / `X-Fishhawk-Export-Next-Cursor` response HEADERS (the body stays the pure three-field shape).
- Run-less entries export under the reserved nil-UUID key `exportGlobalChainKey` with `run_id:null`, first page only, never silently dropped.
- Filter modes (explicit `run_id` set XOR `repo`/`from`/`to`) are mutually exclusive; a missing explicit run is a fail-closed 404; all three repos required (503 `audit_export_unconfigured`).
- Byte-compat pinned by the strict-decode mirror + `audit.ComputeEntryHash` recompute in `audit_export_test.go` (the verifier package is `internal`, unimportable); the cross-module round-trip through `fishhawk-verify` is sibling #1607.
- ALL four export surfaces (this, the CSV, the report + `.md`) require the `read:audit-export` scope (E9.5 / #1608, `scopeAuditExport` enforced via `requireWriteScope` AHEAD of the config probe: anonymous 401, missing scope 403 `required_scope`-named, cookie-session bypass; the scope is in `operatorDefaultScopes`).
  None of them reads the trace store — exports carry content-hash POINTERS only, pinned by `TestExportSurfaces_NeverInlineRawBundle` in `audit_export_auth_test.go`.
- The run-selection code path (query parse, `run_id` XOR `repo`/`from`/`to` mutual exclusion, limit/cursor validation, created_at DESC keyset paging) is extracted into the shared `resolveExportPage` helper both this and the CSV handler call.

### Audit compliance export CSV (`audit_export_csv.go`, E9.2 / #1605)

`backend/internal/server/audit_export_csv.go::handleAuditExportCSV` (`GET /v0/audit/export.csv`) — a flat CSV PROJECTION over the same `resolveExportPage` run-selection + `assembleRunData` assembly the JSON export uses (never a parallel query path).

- One audit entry per row (`ts,run_id,repo,category,actor_kind,actor_subject,sequence,entry_hash,payload_summary`), `payload_summary` compacted and bounded at 256 runes (rune-boundary safe) with a `...(truncated)` marker.
- Two CSV-only in-memory entry filters — `approver` (approval_submitted `actor_subject`) and `category` — ANDed with each other and the run-level filters; CSV-only because dropping entries would break the JSON body's verifier chain walk.
- Whole page buffered before any write, so a per-run assembly error is a clean JSON 500 with no partial CSV; success sets `text/csv` + `Content-Disposition` attachment and the same `X-Fishhawk-Export-Complete` / `X-Fishhawk-Export-Next-Cursor` continuation headers as E9.1.
- The `TestAuditExportCSV_ParityWithJSON` parity test in `audit_export_csv_test.go` locks the CSV rows as a field-for-field projection of the JSON `Export v1` body for the same filter set.

### Agent-changes compliance report (`report_agent_changes.go`, E9.3 / #1606)

`backend/internal/server/report_agent_changes.go::handleAgentChangesReport` / `handleAgentChangesReportMarkdown` (`GET /v0/reports/agent-changes` + `.md`) — a PROJECTION over the same `resolveExportPage` run-selection + `assembleRunData` assembly the JSON/CSV exports use (never a parallel query path).

- Per selected run that produced a change (a `pull_request_opened` entry), `foldRunIntoItem` walks the run's audit chain ONCE keyed on category (`pull_request_opened`, `CategoryPRMerged`, `approval_submitted`, `CategoryPRApprovedOnGitHub`, `plan_reviewed`, `implement_reviewed`, `CategoryAcceptanceOutcomeRecorded`), decoding reviews via the exported `planreview.PlanReviewedPayload`/`ImplementReviewedPayload`; a malformed payload is skipped with a slog warn, never a request failure.
- `human_led_change` runs render in a separate reduced-evidence section (reviews/acceptance dropped); no-PR runs are counted in `totals.runs_without_change` and omitted from both lists.
- ONE `agentChangesReport` model feeds both the JSON handler and the pure `renderAgentChangesMarkdown` (golden-pinned in `testdata/agent_changes_report.golden.md`), guaranteeing one-model-two-renders parity (`TestAgentChangesReport_JSONMarkdownParity`).
- UNLIKE the verifier-strict `Export v1` body, continuation (`complete`/`next_cursor`) rides BOTH the `X-Fishhawk-Export-Complete`/`X-Fishhawk-Export-Next-Cursor` headers AND the body (no verifier strict-decodes this endpoint).
- Evidence links are redacted-tier run/audit/export/artifact API pointers (ADR-054), `cfg.ExternalURL`-prefixed when set else relative.
- Same fail-closed 503 (`audit_export_unconfigured`, all three repos required) AND the same `read:audit-export` scope gate (E9.5/#1608, both renders) as the exports.

### Runtime calibration (`calibration.go`, `GET /v0/calibration`, #470)

`backend/internal/server/calibration.go`.

- The trace upload handler (`trace.go::emitRuntimeObserved`) appends a `runtime_observed` audit entry for every implement-stage terminal upload (success **and** failure). `emitRuntimeObserved` is best-effort — errors log at WARN and do not unwind the upload.
- The calibration handler reads those entries via `audit.Repository.ListAll(category="runtime_observed")`, filters in Go by optional `workflow_id` and `since` params, and computes p50/p95 (nearest-rank), `calibration_ratio = actual_p50 / predicted_p50`, and per-confidence-level within-1.5x accuracy.
- Returns 503 when `AuditRepo` is nil (unconfigured), 400 on a bad `since` timestamp, 200 with `samples=0` when no data exists yet.
- MCP surface: `fishhawk_runtime_calibration` tool in `backend/cmd/fishhawk-mcp/tools.go` — agents call this before writing a plan to self-correct `predicted_runtime_minutes`.

## Onboarding + session security

### First-run readiness introspection (`onboarding.go`, E29.4)

`backend/internal/server/onboarding.go` — `handleGetOnboardingReadiness` serves `GET /v0/onboarding/readiness?repo=owner/name`, aggregating the four server-side-only checks a repo's first run needs, consumed by `fishhawk doctor` (E29.5):

1. GitHub App installation via `githubclient.GetRepoInstallation`, reusing the run-create `ErrNotInstalled` classification (`runs.go`);
2. the committed workflow spec's `spec.ParseBytes` + `spec.Validate` state;
3. per-reviewer availability via the same `ReviewerSet.For(provider, model, reasoningEffort)` probe `unavailableSpecReviewers` performs (`runs.go`), surfacing the adapter's missing-env hint;
4. caller-token scope adequacy against `requiredRunScopes` (the run-drive subset of `operatorDefaultScopes`, `backend/cmd/fishhawkd/token.go`).

Read-only; cascades gracefully (not-installed → spec-unavailable → empty reviewers). Auth-only gate (401 anonymous, NOT a write scope — scope adequacy is a reported field), mirroring `/v0/auth/me`.

### CSRF enforcement (`csrf.go`, ADR-005)

`backend/internal/server/csrf.go` ships the double-submit pattern per ADR-005.

- The OAuth callback (`server.handleGitHubCallback`) mints a 32-byte hex token and sets it in the `__Host-csrf` cookie alongside `fishhawk_session`; logout clears both.
- The `csrf` middleware sits after `bearerAuth` in the chain (`recovery → requestID → logging → bearerAuth → csrf → mux`) and enforces `X-CSRF-Token` ≡ `__Host-csrf` on POST/PUT/PATCH/DELETE for session-cookie identities only.
  Bearer-token clients (CLI, server-to-server) and GET-style methods bypass; safe-listed paths (`/v0/auth/github/*`, `/webhooks/github`) bypass too.
- Mismatch returns `403 csrf_required`.
- Frontend's `frontend/src/api/client.ts` reads the cookie via `getCookie()` (`frontend/src/lib/cookie.ts`) and auto-attaches the header on every state-changing call. Vitest runs jsdom under `https://localhost/` so `__Host-` cookies are accepted (jsdom rejects them under HTTP).

### Per-path escalation enforcement (`escalation_gate.go`, E53.4 / #2227)

A workflow-v2 `escalations:` block declares `{match: <predicate>, require: {...}}` rules that RAISE requirements for a change the predicate matches. The declaration side (grammar, strictest composition, only-ever-raise validation, the pure clamp) lives in `backend/internal/spec`; the pure firing walk in `backend/internal/escalation`. This file is where the run's `spec.Change` is BUILT and the firing decision becomes enforcement.

**One resolver, two seams, one audit emit point.** `resolveEscalations` is reached by both consumers — `approveStageAs` / `checkApprovalPredicates` on the approval gate, and `delegation.Evaluate` through the `escalationResolver()` adapter — and the `escalation_fired` audit entry is written inside it. That is what makes "audited at every firing seam" structural: a `max_autonomy`-only escalation on a workflow with NO approval gate, which changes behaviour purely through the delegation clamp, is audited exactly as an approvals escalation is, and a future third consumer inherits the audit rather than having to remember it.

**The change.** Paths are the UNION of the approved plan's top-level `scope.files`, every decomposition `sub_plan` scope and every `split_proposal` phase scope — the same union `planGateScopePaths` computes for `applies_to` (#2226). A decomposed run's fan-out child runs bounded to its SLICE scope, so checking only the top level would let a slice touch an escalated path without escalating. Labels come from the run row's IMMUTABLE `IssueContext.Labels` snapshot; trigger from `trigger_source` through the shared `appliesto` mapping.

Unlike `applies_to` there is **no two-phase split**: escalations are evaluated only at gate time, when all three producers exist, so the full predicate is evaluated in one pass. The plan is loaded ONLY when some declaration carries a `paths` criterion, so a label-only declaration is not refused because the run has no plan yet.

**Fail-closed modes**, each returning a refusal rather than proceeding unescalated:

| Mode | Approval seam | Delegation seam |
|---|---|---|
| `Match` error (a malformed glob that bypassed validation) | retryable 503 `escalation_unevaluable` | `Evaluate` errors → the caller's existing degradation |
| Plan unreadable/absent while a `paths`-bearing escalation is declared | same 503 | same |
| Membership resolution error on an escalated group | `predicateUnavailable` → 503 `forge_unavailable` | n/a |
| Gate `approvals` block unreadable while an escalation IS firing | retryable 503 `escalation_unevaluable` (`reason: baseline_unreadable`) | n/a |

The last row is #2374, and its reason is that an escalation raises **relative to** the baseline: composing a firing escalation against a `nil` baseline drops the baseline's own `member_of` / `min_permission`, so a `member_of` baseline under a COUNT-ONLY escalation would compose to "the raised count, no membership at all" and admit an out-of-group approver — a shape the count-time forge re-validation does not cover either, since a count-only escalation carries no escalated forge predicate to re-resolve. The refusal is gated strictly on a firing (or unevaluable) escalation: a fetch error with NOTHING escalated keeps the pre-existing baseline fail-**open** read unchanged. It applies at BOTH approval points — `checkApprovalPredicates` returns the 503 pre-Submit, and `approveStageAs` (reached directly by the campaign auto-driver, which has no pre-Submit gate) RECORDS the approval but does NOT advance, the same posture its post-Submit resolver-error branch takes.

**Enforcement at BOTH approval points.** The raise is applied at the pre-Submit 403 (`checkApprovalPredicates`) *and* at the quorum count (`approveStageAs`), for the #2358 reason `not:` had to be: raising only the 403 would record an approval the gate then declines to count, wedging it one short. Both read the same immutable cached spec bytes on the same run row within one request. `effectiveApprovals` takes max / strictest / union per dimension, so **runtime lowering is structurally impossible** even for a spec that bypassed declaration-time validation.

**Audit posture (operator-ratified, #2361 asymmetry).** The `escalation_fired` append is **best-effort, not a precondition of enforcement**: an escalation firing moves in the SAFE direction (the gate gets stricter), so failing the gate because a log line could not be written would convert an audit-store outage into a total governance outage. The rule: *a REFUSAL or a RAISE is best-effort — the safe outcome already happened; a GRANT (the `applies_to` override) is a precondition — it records an exception being MADE.* De-duplication is read-then-append on `(run, stage, fingerprint)`, which handles the common SEQUENTIAL case; it is **not** atomic, so two concurrent evaluations can both append. A duplicate governance-chain entry is hygiene, not a control gap (the #2366 class) — there is no per-`(run, stage)` uniqueness guarantee. A de-duplication READ failure emits anyway (fail toward visibility).

Surfaced on `GET /v0/runs/{run_id}` as the `escalations` block (single-run read only, omitted when the workflow declares none or nothing fired), rendered through the same `escalation.RenderFired` helper as the audit payload so the two cannot drift.

### Declared per-stage permissions surface (`runs.go`, E53.5 / #2228)

A workflow-v2 stage may declare a `permissions` block (`network` / `write` / `shell`) or an `egress` allowance on any agent stage. The block is **DECLARATION-ONLY** — validated, audited and surfaced but NOT enforced until E51 (#2133); no surface calls it containment, sandboxing, isolation, or a security control. The grammar and normalization (`permissions.network` folded into `Stage.Egress`) live in `backend/internal/spec`.

- **Run-status read.** `GET /v0/runs/{run_id}` carries a `permissions` array (`runStagePermissionsPayload`), one entry per stage declaring a `permissions`/`egress` block, populated by `handleGetRun` ONLY via `buildStagePermissionsSurface` (same single-read spec-projection posture as `review_authority`; NOT suppressed on a terminal run). Omitted (nil, `omitempty`) when the run has no cached spec, the spec fails to parse, or no stage declares either block — so a run declaring none stays byte-identical. Each entry's `enforced` flag is the HONEST per-entry qualifier: **true only for an acceptance stage's network declaration** (whose allow-list the runner's default-deny egress proxy enforces today), false everywhere else and for write/shell always — a blanket `enforced:false` would lie for the one place the control is real.
- **Once-per-run audit.** `CreateRunForTrigger` emits ONE `stage_permissions_declared` entry at creation (best-effort, warn-logged — a legibility surface never fails run creation) when any stage declares a block; the payload carries a feature-level `enforced:false` + `enforcement_tracked_by:"#2133"` plus the same per-stage entries. `buildStagePermissionsPayloads` is shared by both surfaces so the read and the audit cannot describe one declaration differently.

## MCP surface on fishhawkd (`mcproute.go`, ADR-076 slice 2 / E66.2 #2390)

`POST/GET/DELETE /mcp` serves the Fishhawk MCP tool registry over the
streamable-HTTP transport, on fishhawkd's own listener, inside the ordinary
middleware chain. The registry comes from `mcpserver.NewServer` — the same call
`backend/cmd/fishhawk-mcp` makes — so there is ONE tool-registration path and no
second registry to drift. The stdio binary is untouched by this route and
remains the headless/CI path.

### Why the registry is INJECTED, not imported

This package does NOT import `backend/internal/mcpserver`. It declares a
`MCPServerFactory func(backendURL, apiToken string) *mcp.Server` seam on
`Config`, and `backend/cmd/fishhawkd` — which already imports both — wires
`mcpserver.NewServer` into it.

The reason is a hard constraint, not a style preference: `mcpserver`'s
IN-PACKAGE tests drive a real `server.New` (see
`backend/internal/mcpserver/campaign_test.go`), so an import edge from here to
`mcpserver` closes an import cycle in `mcpserver`'s TEST binary. `go build`
stays green and `go test ./internal/mcpserver/...` fails with `import cycle not
allowed in test` — a failure mode a plain build check does not surface. Only the
direction of the reference changed; there is still exactly one registration path.

A nil factory is a named failure, not a nil dereference: the route answers `503
mcp_route_misconfigured` naming the unwired seam, diagnosed BEFORE any address
work because an unwired deployment cannot serve the route at any address.

### Clients must pass an absolute `working_dir` ([#2479](https://github.com/kuhlman-labs/fishhawk/issues/2479))

fishhawkd wires the factory with `mcpserver.Config{HTTPTransport: true}` (see
`mcpRouteServerConfig` in `backend/cmd/fishhawkd/serve.go`), because `/mcp`
serves the registry over an HTTP transport on a long-lived daemon: the serving
process's cwd is fishhawkd's OWN checkout, never a caller's. So the four
runner-spawning verbs (`fishhawk_dispatch_stage`, `fishhawk_run_stage`,
`fishhawk_run_children`, `fishhawk_drive_run`) **require an absolute
`working_dir`** over this surface. An omitted or relative `working_dir` is
refused at the tool layer with an actionable error naming the field — the tool
call returns an error result and the run is NOT dispatched against fishhawkd's
tree. Clients pass `working_dir` as an absolute path to the checkout the run
should execute in; the resolved directory is echoed back as
`resolved_working_dir`. The long-form contract lives in the
[mcpserver README](../mcpserver/README.md).

As of [E66.42 / #2482](https://github.com/kuhlman-labs/fishhawk/issues/2482)
the path is **bound once at `fishhawk_start_run`** (persisted as
`runs.working_dir`, migration 0065, echoed as `working_dir` on the Run) and the
four verbs **inherit** it, so a driving loop passes it once. Over HTTP a
`runner_kind: local` `start_run` requires an absolute `working_dir`; the
inheriting verbs still refuse an omitted-**and-unbound** or relative value, and
refuse an explicit value that conflicts with the run's binding. `POST /v0/runs`
itself validates only that a supplied `working_dir` is absolute (a 400 naming
the field) — it is transport-agnostic and cannot know a plain REST caller's
transport, so the required/absolute admission stays in the MCP layer.

### Refusal ladder (`resolveMCPRouteState` → `handleMCP`)

Evaluated in this order. The ordering is load-bearing, not cosmetic: the
malformed-address diagnosis PRECEDES the loopback predicate, because a loopback
check that ran first would swallow every unparseable address into a 403 and
leave the 503 branch unreachable.

| # | Condition | Response |
|---|---|---|
| 1 | `MCPRoute` normalizes to `off` | `404 route_not_found` |
| 2 | no `MCPServerFactory` wired | `503 mcp_route_misconfigured` (names the unwired seam) |
| 3 | `net.SplitHostPort(Addr)` fails | `503 mcp_route_misconfigured` (names the unparseable address) |
| 4 | listener host cannot be classified (DNS failure / no addresses) | `503 mcp_route_misconfigured` |
| 5 | listener host is not loopback | `403 mcp_route_loopback_only` (ADR-033) |
| 6 | `MCPSelfURL` is set but is not a loopback base URL | `503 mcp_route_misconfigured` (names the bearer-forwarding risk) |
| 7 | no bearer identity on the request | `401 authentication_required` |
| — | otherwise | delegate to the streamable handler |

The verdict is computed ONCE at `New()` from the immutable `Config` and stored
on the `Server`. It is a pure function of that config, and resolving it per
request would put a blocking DNS lookup on every `/mcp` call with a resolver
outage intermittently flipping a serving route to 403.

### Protected-resource half: PRM, audience validation, discovery challenge, conditional lift (`oauthprm.go`, ADR-076 slice 3 / E66.3 #2391)

This half makes the MCP resource discoverable and OAuth-authenticatable, and it
depends on the OAuth AS verdict (`oauthas.go`), so `server.New` resolves
`s.oauthAS` BEFORE `s.mcpRoute`.

**PRM document + two URLs (RFC 9728).** `GET /.well-known/oauth-protected-resource`
(bare) and `GET /.well-known/oauth-protected-resource/{resource_path...}`
(RFC 9728 §3.1 path-suffixed) both serve an `oauthPRMetadata` document —
`resource`, `authorization_servers` (this AS's own issuer), `scopes_supported`
(identical to the AS metadata's), `bearer_methods_supported: ["header"]`,
`resource_name` — but ONLY when the AS is ENABLED (else `503
oauth_as_unconfigured`, via the same `oauthASEnabled` gate the AS metadata route
uses; never a partial document). `protectedResourceMetadataURL` derives the URL
by INSERTING the well-known path between the resource's origin and its own path.
The two derived values are encoded DIFFERENTLY on purpose: `prmURL` (advertised)
uses `EscapedPath` so a segment needing encoding stays valid, while `prmSuffix`
(compared against `r.PathValue("resource_path")`) uses the DECODED `url.URL.Path`
because Go's `ServeMux` returns `PathValue` percent-decoded — comparing a decoded
value against an escaped suffix could never match for a path needing encoding, so
the handler would 404 the exact URL its own document advertises. The suffixed
handler answers `404 route_not_found` on a mismatch, so the document is never
served under a URL claiming a different resource. A resource whose scheme cannot
host an http(s) PRM document makes derivation fail, which `resolveOAuthASState`
carries as MISCONFIGURED — an AS that cannot name its own PRM document does not
advertise itself.

**Audience validation lives in `bearerAuth`, not `handleMCP`.** A third bearer
prefix branch resolves `fho_` AS-issued access tokens (`oauthstore.
AccessTokenPrefix`), entered ONLY when the AS is enabled (with no AS there is no
resource to validate against, so `fho_` is never accepted and the store is never
even dialed — fail closed). On success it validates the token's audience against
this deployment's resource with `oauthas.ResourceMatches`; a foreign audience
leaves the ANONYMOUS identity. This is deliberately in the middleware and not in
`handleMCP`: the `/mcp` tool client dials fishhawkd's OWN REST API back with the
caller's raw token (`newMCPHandler`'s factory), so validating only in the handler
would leave every REST route accepting a token minted for a foreign resource. A
matching audience stamps `Identity.OAuthAudience` — the marker the lifted-mode
gate reads. A DB-unavailable classified error is a `503`, matching the #764
posture of the sibling branches; `AuthMethod` is deliberately NOT set (its
OpenAPI enum is `[static, oauth]` and widening it is out of scope).

**Byte-identical discovery challenge.** `writeMCPAuthChallenge` is the ONE
builder for the `/mcp` `401`: it sets `WWW-Authenticate` from
`oauthChallengeHeader(s.oauthAS.prmURL)` — the RFC 9728 §5.1 `resource_metadata`
form when the AS is enabled, the realm-only form otherwise — then writes ONE
fixed envelope. All three refusal paths (no Authorization header, unknown bearer,
off-loopback bare token) produce a BYTE-IDENTICAL status, header and body; giving
the bare-token case a distinct message would make a valid bare token
distinguishable from an invalid one, the token-validity leak `handleMCP`'s
indistinguishability property forbids. `oauthChallengeHeader` FAILS CLOSED on an
unescapable `prmURL` (a `"`, `\`, or control byte) by omitting the parameter and
returning the realm-only form, so an unsafe value is never spliced into the
quoted-string header.

**Conditional loopback lift (exact condition, both directions).** In
`resolveMCPRouteState` the non-loopback arm refuses (`403
mcp_route_loopback_only`) UNLESS the AS verdict is `oauthASEnabled` — then the
refusal LIFTS: the route serves with `authenticatedOnly = true` and
`challengeResource = <resource>`, and `handleMCP` accepts ONLY an
audience-validated OAuth identity (`Identity.OAuthAudience != ""`). A bare
`fhk_`/`fhm_` token carries a `TokenID` but no `OAuthAudience` and is refused
off-host with the same `401`. So network exposure and auth tightening land
together: ADR-033's no-bare-bearer-off-host invariant is kept, not traded away.
`--oauth-require-loopback` drives the AS verdict to `oauthASNotLoopback` (not
enabled) on a public bind, so setting it keeps the `/mcp` refusal too. In the
lifted posture a bind naming a SPECIFIC host pins `listenAddr` to the SAME
resolved literal the selfURL is built from (see the selfURL-asymmetry paragraph),
so the address the tool client dials the caller's bearer to is provably the
address `net.Listen` bound; an empty/unspecified bind leaves `listenAddr` empty so
`Server.listenAddr()` binds `cfg.Addr` verbatim (every interface, which includes
the loopback the selfURL dials). `verifyMCPListenerLoopback` skips the loopback
assertion off-host but STILL fails closed if the lift carries an empty
`challengeResource` — a lift with no audience to enforce would serve the
bare-bearer surface off-host, so `Start` refuses the whole listener.

**selfURL asymmetry in the lifted posture (CONDITION 5).** Unlike the unlifted
route — whose self URL is ALWAYS a resolved loopback IP literal — a bind-derived
lifted selfURL may resolve to a SPECIFIC non-loopback LOCAL address. For an empty
or unspecified bind (`:8080`, `0.0.0.0`, `[::]`) the dial-back stays on
`127.0.0.1`, because `net.Listen` on such a host binds every local unicast
address including loopback, so ADR-033's forwarding invariant is fully preserved.
For a SPECIFIC non-loopback local bind, the tool client's dial-back addresses
that same locally-assigned address; packets to a locally-assigned unicast address
are delivered by the host's own stack and never traverse a physical link, so the
bearer is not put on the wire — but this is a documented NARROWING of ADR-033's
stricter always-`127.0.0.1` posture, not an equivalence. **The selfURL and the
bind derive from ONE resolution.** `liftedSelfURL` returns both the dial-back URL
AND the `listenAddr` the specific-host bind is pinned to, built from the same
`net.ParseIP`/`lookupIP` result, so `net.Listen` cannot re-resolve a hostname at
`Start` and land the listener on a different host than the selfURL dials — the
divergence that would carry the caller's raw bearer toward uncontrolled egress
(#2391). An embedder-supplied `MCPSelfURL` keeps the existing loopback-pinning
treatment unchanged and leaves the bind at `cfg.Addr` verbatim (the self URL is
an independent loopback dial-back target); a hostname bind is resolved and its
first address pinned into BOTH selfURL and `listenAddr`, a resolution failure
being `503 mcp_route_misconfigured` (never a silent accept).

### Why stateless (and what it costs)

`newMCPHandler` passes `StreamableHTTPOptions{Stateless: true}`. As of go-sdk
v1.7.0 a stateless server neither reads nor sets `Mcp-Session-Id` and serves
every request from a temporary session (`ServerOptions.GetSessionID` is not
consulted), so there is no `sessions` map to look up and the request factory
runs on EVERY request. Per-request bearer binding is therefore literally true:
the token authorizing a tool call is provably the token on the request that
triggered it, and there is no session registry to leak, GC, or hijack.

Through v1.6.1 the same guarantee held for a weaker reason — the `sessions` map
was simply never WRITTEN on the stateless branch (the `if stateless { defer
session.Close() } else { …save the transport… }` split after `server.Connect`),
so the lookup was always nil. v1.7.0 removed session handling from stateless
mode outright, per the sessionless direction of SEP-2567. The old behavior is
restorable via the `MCPGODEBUG` parameter `allowsessionsinstateless=1` — **do
not set it**: it would reintroduce the very session lookup this route's security
argument depends on being absent.

Stateless is also what makes the route eligible for protocol `2026-07-28`: the
streamable transport accepts that version only when `Stateless` is true, so this
route negotiates up while a stateful one would negotiate down to `2025-11-25`.

The alternative — a stateful handler plus a hand-rolled session registry — was
rejected on evidence rather than taste. The SDK's own session-hijacking guard
keys on `auth.TokenInfoFromContext`, and `tokenInfoKey` is UNEXPORTED with no
exported setter (`auth/auth.go`), so only the SDK's own
`auth.RequireBearerToken` middleware can populate it — and that middleware emits
its own plain-text 401, conflicting with this route's standard-envelope,
no-new-auth-code requirement. Doing it ourselves would mean response-header
capture plus expiry GC: strictly more code and more failure modes.

The honest cost, since #2390 originally required all three transport legs and
was AMENDED to withdraw that requirement: a stateless server holds no session to
stream from or tear down, so POST is the only method served — `GET` and `DELETE`
both answer the spec-prescribed `405` with `Allow: POST`. (`DELETE` was a `204`
no-op through v1.6.1; with session handling gone there is no session id left to
no-op ON. `TestMCPRoute_DELETE_405WithAllow` presents a fabricated
`Mcp-Session-Id` precisely so the `405` proves the header bought the caller
nothing — a regression to `204` there would mean session handling had returned.)
All three method patterns are still registered so no leg falls through to a 404
that would misreport a disabled route. In-request notifications — the progress heartbeats the long-running tools
emit — are NOT lost: they ride the POST response's own stream, which stateless
preserves, and `TestMCPRoute_InRequestProgressNotification` drives a real tool
call with a `progressToken` over `POST /mcp` and asserts one arrives. Slice 3
(#2391) revisits the mode if a tool ever needs sampling, elicitation, or
subscriptions.

### Derived, IP-pinned self URL

The tools reach data by dialling fishhawkd's own REST API, so the route needs a
base URL to hand them. There is deliberately NO `--mcp-self-url` flag: the value
is DERIVED from the already-loopback-validated listener, so no operator-settable
knob can aim the round-trip — and every caller's raw bearer — off-host.
`Config.MCPSelfURL` survives as an embedder/test seam only (an httptest server
picks its own port) and goes through the same loopback predicate.

The derived URL is always built from a RESOLVED loopback IP literal, never a
hostname. Validating a hostname at construction while STORING the hostname would
leave the HTTP client to re-resolve it at call time, so a DNS record changed
after startup would send every caller's bearer off-host — defeating the refusal
this design claims. `net.JoinHostPort` does the assembly so an IPv6 literal
comes out bracketed (`[::1]:8080` → `http://[::1]:8080`, not the unparseable
`http://::1:8080`).

An `MCPSelfURL` whose scheme is `https` and whose host is a HOSTNAME is REFUSED
(`503`), because pinning and TLS cannot both hold: the rewrite hands the client
an IP literal, so the handshake verifies against a certificate issued for a name
the dialled host no longer carries. That shape would validate at construction
and then fail EVERY tool call in the handshake, so it is diagnosed once at
`New()` instead of continuously at call time. `https` with an IP LITERAL is
still accepted — nothing is rewritten, so a certificate carrying that IP SAN
verifies normally.

### The listener binds the pinned IP too (`listenAddr`, `verifyMCPListenerLoopback`)

Pinning only the self URL leaves the same resolution-to-use gap on the side that
protects the ENTIRE surface. `resolveMCPRouteState` classifies `Config.Addr` at
`New()`, but the bind happens later in `Start`; handing `net.Listen` the
original hostname lets it re-resolve, so a DNS record changed in between binds
the bare-bearer tool surface to an off-host interface while the route still
reports itself loopback-only.

So an enabled route also pins the BIND address: `mcpRouteState.listenAddr` is
`Config.Addr` with the host replaced by the resolved loopback literal, and
`Start` binds that. The classified address and the bound address are then the
same address by construction. Every other verdict leaves `listenAddr` empty and
`Start` binds `Config.Addr` verbatim, so a deployment that does not serve the
route binds exactly what it always did.

`Start` creates the listener explicitly (rather than via `ListenAndServe`) so
`verifyMCPListenerLoopback` can compare the address actually bound against the
verdict before a single request is served. Pinning should make that check
unreachable; it is enforced anyway because the failure it guards is serving the
whole bare-bearer tool surface off-host, so a contradiction fails closed on the
ENTIRE listener — `Start` returns an error naming
`FISHHAWKD_ADDR=127.0.0.1:8080` / `--mcp-route=off` rather than serving REST
with an exposed `/mcp`. `net.Listener.Addr` is always a resolved literal, so no
DNS runs on that path either.

An EMPTY listener host is reported NON-loopback and is deliberately not clamped
to `127.0.0.1`. `net.Listen` documents that an empty or unspecified host listens
on every available unicast address, so by the time the route resolves, fishhawkd
has already bound all interfaces — the opposite of
`backend/cmd/fishhawk-mcp/http_transport.go`'s `validateLoopbackAddr`, which may
clamp because it controls its own bind. The operator-visible consequence is that
the shipped `FISHHAWKD_ADDR` default `:8080` draws the 403 until it is set to
`127.0.0.1:8080`; that is named in the flag help, the 403 message, and the
`fishhawkd` README.

### Auth, CSRF, and privilege parity

The route requires a BEARER identity — non-anonymous AND carrying a `TokenID`.
Both `fhk_` and `fhm_` are accepted with no special-casing. A cookie session is
refused `401`, and that refusal is what makes the `csrfExemptPath` entry for
`/mcp` safe: a browser cookie session can never reach the tool surface, so
exempting the path cannot open a browser-driven CSRF path onto it.

`bearerAuth` resolves an absent OR invalid bearer to the anonymous Identity and
falls through rather than rejecting, so the 401 is emitted by `handleMCP` itself
through the shared `writeError` envelope — no new auth code. It also means a
MISSING header and an UNKNOWN token are indistinguishable to the handler and
deliberately share one message; discriminating them would leak token validity.

Because each tool authenticates by calling this same REST API with the caller's
token, a tool call is exactly as privileged as the equivalent REST call,
including its denials.

### Streaming seam

`statusRecorder` (in `middleware.go`) declares `Unwrap() http.ResponseWriter`.
`http.ResponseController` reaches a wrapped writer only through that convention,
and the go-sdk flushes SSE through it; without `Unwrap`, every `/mcp` response
would buffer until the handler returned. The SDK ignores the `Flush` error, so
this is a streaming-latency fix rather than a correctness one — the response is
identical either way, it just arrives all at once.

### Known cost, accepted

Constructing a full `mcpserver.NewServer` per request pays every tool's
registration (including jsonschema reflection) on each call, and each tool call
additionally costs one loopback HTTP round-trip. Both are accepted for a
loopback single-operator endpoint. A token-keyed server cache is deliberately
NOT added: it would reintroduce exactly the shared-server confusion the
per-request binding exists to prevent. If it proves material under slice 3's
remote posture, a cache with an explicit eviction contract is the follow-up, not
a silent one.

Host-side tools reached through the route (`fishhawk_run_stage`,
`fishhawk_dispatch_stage`, `fishhawk_run_children`, `fishhawk_drive_run`) spawn
the runner on the fishhawkd host and forward the CALLER's token in the child
environment. Under the loopback-only default that host is the operator's own
machine, so dogfood behaviour is unchanged — but both facts become live
questions the moment slice 3 admits a remote client.

## OAuth 2.1 authorization server (`oauthas.go` / `oauthauthorize.go` / `oauthtoken.go`, ADR-076 slice 3 / E66.19 #2436)

The four endpoints — `GET /.well-known/oauth-authorization-server`,
`GET`/`POST /v0/oauth/authorize`, `POST /v0/oauth/token` — mint a Fishhawk
access token to a spec-compliant MCP client via authorization-code + PKCE. The
handlers are the HTTP shell over the pure `backend/internal/oauthas` domain
(PKCE, redirect matching, CIMD fetch, typed RFC 6749 errors) and the
`backend/internal/oauthstore` persistence; this package adds no crypto or
storage logic of its own.

### Three-verdict enablement, resolved ONCE at construction

`New()` resolves the AS to exactly one of three verdicts and stores it; every
route reads that one value:

- **DISABLED** — no issuer configured. All four routes answer
  `503 oauth_as_unconfigured`.
- **ENABLED** — a valid https issuer AND a valid RFC 8707 resource AND a
  non-nil store AND a non-nil CIMD fetcher. Routes served.
- **MISCONFIGURED** — an issuer was supplied but a required input is defective:
  a bad issuer, a bad resource, a nil store, or a nil CIMD fetcher. All four
  routes answer `503 oauth_as_unconfigured`, and the metadata route emits NO
  partial document.

**A nil CIMD Fetcher is MISCONFIGURED, not a conditional advertisement.** The
tempting alternative — serve metadata with `client_id_metadata_document_supported:
false` when no fetcher is wired — was rejected: it would let a deployment that
cannot fetch a Client ID Metadata Document still stand up an AS, advertising a
narrowed capability that silently diverges from what the four
otherwise-unconditional-literal metadata fields promise. Folding the nil fetcher
into MISCONFIGURED keeps the enabled metadata document a set of unconditional
literals — `client_id_metadata_document_supported` is `true` whenever the
document is served at all — so a client never has to branch on a
half-configured server.

### Authorize error ordering: two errors are UNREDIRECTABLE (RFC 6749 §4.1.2.1)

The authorization endpoint must decide WHERE an invalid-request error goes
before it can decide the error itself. Two failures are answered IN-PLACE (an
RFC 6749 §5.2 JSON error via `writeOAuthError`, not an HTML page), never
redirected:

1. an **unresolvable `client_id`** (no CIMD, no registered client), and
2. a **`redirect_uri` that does not match** the resolved client's registered set.

RFC 6749 §4.1.2.1 is explicit: redirecting an error to an unverified or
unregistered `redirect_uri` would make the AS an open redirector and could hand
an attacker-chosen destination the `state`/`iss`. So these two are resolved
FIRST, and only once the client and redirect URI are both validated does any
OTHER invalid-request error (bad `response_type`, missing/short PKCE challenge,
unregistered scope, resource mismatch) redirect back to the now-trusted
`redirect_uri`. **Every redirected error carries `state` and `iss`** (RFC 9207
§2) — the same authorization-response `iss` the metadata advertises via
`authorization_response_iss_parameter_supported: true`, so a client can bind the
error to the issuer it dialed.

### One `responseRedirect` value: the delivery URI, not the registration (#2470)

`runAuthorizeLadder` resolves ONE value — `authorizeResolved.responseRedirect`,
from `oauthas.ResolveRedirectURI` — and every outbound use of a redirect URI in
the authorize flow reads it: the success `302`, all eight in-ladder
`redirectOAuthError` calls, the consent deny branch, AND
`CreateAuthorizationCode(RedirectURI:)`.

It is the **delivery** URI, not the matched registration. RFC 8252 §7.3 has a
loopback native client register PORTLESS because it listens on an ephemeral port,
so `ResolveRedirectURI` substitutes the requested port onto the registered URI
(see `backend/internal/oauthas/README.md` for the construction and its
registered-side-authoritative guarantees). Delivering to the registration instead
meant a loopback client never received its code, and the code row was bound to a
URI the client could not present — the #2470 defect. The field was called
`matchedRedirect`, and that name is what made the wrong value look right at every
call site; the rename is part of the fix.

**`oauthtoken.go`'s `c.RedirectURI != redirectURI` comparison is deliberately
UNCHANGED and stays byte-exact.** RFC 6749 §4.1.3 requires the presented
`redirect_uri` to be identical to the authorization request's, which is now what
the row holds, so the client presenting the URI it actually used compares equal.
Relaxing the port there would add a SECOND port-relaxation surface on the
endpoint that mints tokens, for no client benefit. Consequence, asserted in
`oauthtoken_test.go` and end to end in `oauth_flow_integration_test.go`: a client
that authorized on a port-bearing URI and exchanges with the portless one is
correctly refused `invalid_grant`.

### CSRF on `/v0/oauth/authorize`: the SameSite trap (CONDITION-1)

The `POST` consent decision is CSRF-protected by the standard `__Host-csrf`
cookie, but with a NARROW, path-scoped `csrf_token` form-field fallback that
exists nowhere else in the surface. The reason is a cookie-attribute
interaction, not laxness: the `__Host-csrf` cookie is `SameSite=Strict`, so it
is **NOT sent on the cross-site top-level navigation** that lands the browser on
`GET /v0/oauth/authorize` from the MCP client's site — while the
`SameSite=Lax` session cookie IS. A double-submit that read only the Strict
cookie would therefore see no token on the very first consent render and wedge
the flow.

So the consent **GET mints a `__Host-csrf` cookie when the request carries
none**, and the POST accepts the token from the `csrf_token` form field matched
against that cookie. The fallback is scoped to this one path.

**General lesson, recorded because it defeated handler-level testing:** a test
suite that constructs cookies programmatically cannot observe `SameSite` at
all — `net/http` does not replay the browser's cross-site send/suppress rule —
so a handler test that hand-attaches the `__Host-csrf` cookie will pass while a
real browser sends no such cookie on the cross-site navigation. Handler-level
testing cannot catch this class; the mint-on-GET behavior is the structural fix,
not a test assertion.

**Bounded form read (CONDITION-3).** The `csrf_token` fallback parses the POST
body under a 1 MiB cap — implemented with `io.LimitReader` (reading one byte past
the cap) plus a manual length check that returns `413` on overflow, not
`http.MaxBytesReader` — so an unbounded form body cannot be read into memory on
this unauthenticated, pre-consent path.

### Token endpoint: public-client only

`POST /v0/oauth/token` serves `grant_type=authorization_code` and
`grant_type=refresh_token` for a PUBLIC client
(`token_endpoint_auth_method=none`) exclusively:

- **`client_id` is read from `r.PostForm` ONLY.** There is no client
  authentication to parse.
- **ANY `Authorization` header is refused `401 invalid_client`** with a
  `WWW-Authenticate` response header. A public client presents no client
  credential, so a request that carries one is malformed by definition — failing
  it closed (rather than ignoring the header) keeps a confused-deputy credential
  from being silently accepted as if the endpoint were confidential.

Success bodies and every error carry `Cache-Control: no-store`; errors are the
RFC 6749 §5.2 flat JSON `{error, error_description}` — deliberately NOT the
standard Fishhawk error envelope — while the whole-AS `503
oauth_as_unconfigured` refusal DOES use the standard envelope.

### Verify-before-consume, and the derived-authority invariant

Code redemption and refresh rotation go through `oauthstore`'s transactional
**verify-before-consume** seam: the handler hands the store the presented code
(or refresh token) and the PKCE verifier, and the store verifies the hashed
credential and marks it consumed inside ONE transaction, so a replayed code
cannot race a second redemption.

The handler passes the store only what it must: a `RedemptionRequest` /
`RotationRequest` carries **only the expiries** (`--oauth-access-token-ttl`,
`--oauth-refresh-token-ttl`). The token's SUBJECT, CLIENT, SCOPE, and RESOURCE
are **derived by the store from the persisted code/refresh row**, never taken
from the request — the derived-authority invariant. The handler cannot widen a
token's authority by what it passes; the authority is whatever the original
authorization persisted.

### Client resolution: store-first, no `UpsertClient` on the hot path

`client_id` resolves store-first, in **one** store read: **a `client_id`
resolves to at most one registration.** `oauth_clients` is keyed `UNIQUE
(client_id)` (migration 0064 / #2437) because a client registration names which
*software* is asking — `client_id` **is** a CIMD document URL — not who
authenticated, so it carries no forge discriminator to be scoped by. A
pre-registered row is used if present, else the id is fetched and validated as a
CIMD (RFC 8414 registration-free).

That single read replaces the #2436 interim workaround, which looped the fixed
provider set `{github, gitlab}` and failed closed on divergent duplicates. The
fail-closed branch is not relaxed, it is **unreachable**: the database no longer
permits two rows to share a `client_id`. What holds that claim up is a schema
assertion, not resolver code — `oauthstore`'s
`TestSchema_OAuthClientsKeyedOnClientIDAlone` reads the live catalog for exactly
one unique-or-PK constraint over `{client_id}` and no constraint whose column set
includes `provider`. **A resolved CIMD is deliberately NOT persisted.** Writing an
`oauth_clients` row for a CIMD-resolved client would SHADOW later CIMD
refreshes — the store lookup would win on every subsequent request and pin the
first-seen document, converting the CIMD fetcher's bounded TTL into a permanent
pin. So there is no `UpsertClient` call on the authorize/token path; the fetcher
stays authoritative for CIMD clients.

Two CIMD gate fields (advertised in metadata,
`client_id_metadata_document_supported: true`) are what let a modern client
register-free. **If either the metadata advertisement or the CIMD fetch path is
missed, a client silently DOWNGRADES** — Claude Code, for one, falls back to
deprecated Dynamic Client Registration (DCR) rather than erroring, so a broken
CIMD path is invisible unless you check which registration route the client
actually took.

### Registered-metadata enforcement, RFC 7591 defaults (CONDITION-2)

A client's registered `response_types`, `grant_types` and `scope` are ENFORCED,
not advisory:

- authorize rejects a `response_type` the client did not register with
  `unauthorized_client`;
- token rejects a `grant_type` the client did not register with
  `unauthorized_client`;
- authorize rejects a requested `scope` outside the client's registered `scope`
  set with `invalid_scope` (`runAuthorizeLadder` step 7, via
  `registeredScopeSet`). The token endpoint needs no scope check — it derives
  scope from the persisted code row (the derived-authority invariant), so a
  narrow registration bounds every code and every token descended from it.

Absent registration fields take the RFC 7591 defaults: `response_types` →
`["code"]`, `grant_types` → `["authorization_code"]`. An **absent `scope`** on
the registration means NO scope restriction (the client may request any operator
scope), distinct from an empty list — `registeredScopeSet` returns nil for an
absent scope and the ladder enforces only when the set is non-empty, so a
scope-omitting CIMD client (e.g. Claude Code) stays unrestricted while a client
that DID pin a narrow scope is bounded to it.

An **absent REQUEST `scope`** now defaults too (#2466), which makes the two sides
consistent — an absent registration already meant "no restriction", so failing
closed on an absent request parameter was the odd one out. The ladder's step 6
calls `oauthas.ResolveRequestedScope(req.scope, registeredScopeSet(client))`: a
request that CARRIES NO SCOPE TOKEN (the key absent, or a present-but-empty
`scope=` — `url.Values.Get` cannot tell them apart and they are deliberately
equivalent) is granted the client's registered scope INTERSECTED with the server
vocabulary, or the whole vocabulary when the registration pins nothing; a
registration naming no supported scope fails CLOSED with `invalid_scope`. Every
PRESENT value still validates exactly as before — an unknown scope, a
whitespace-only `scope=%20%20`, and a scope exceeding the registration all still
fail `invalid_scope`. Step 7 is NOT skipped for a defaulted set: it passes by
construction, so one unconditional restriction ladder beats a conditional one a
later edit could get wrong. Contract detail: `backend/internal/oauthas/README.md`.

**The consent form's hidden `scope` field carries the RESOLVED set, not the raw
request value.** The page DISPLAYS the resolved scopes, so submitting the raw
value would make a scope-less GET produce a scope-less POST that re-derives
against whatever the registration says at POST time — a consent/grant divergence
in which the user approves one set and another is granted. Carrying the resolved
value makes the POST validate through the ordinary PRESENT-scope path: if the
registration NARROWED in between, the POST carries scopes it no longer permits
and is refused `invalid_scope` at step 7 (a refusal, not a silent grant); if it
BROADENED, the narrower displayed set is granted. Both outcomes match what the
user saw. This adds no tampering surface — a user editing their own hidden field
could equally have crafted that authorize request directly, and step 7 still
bounds it whenever a registration pins a scope. Pinned by
`TestAuthorizeConsent_GrantMatchesDisplayedScopeWhenRegistrationChanges`, which
MUTATES the registration between the consent GET and POST in both directions.

### CIMD amplification limiter + loopback gate (#2441)

The outbound CIMD amplification primitive on the two unauthenticated OAuth AS
routes is now bounded. A `GET /v0/oauth/authorize` (before its identity check) or
a `POST /v0/oauth/token` (unauthenticated by nature — public client) with an
unseen `client_id` URL still induces one outbound CIMD fetch, but the RATE and
VOLUME of those fetches are now capped, and a code-enforced (default-off) loopback
gate is available. Shipped contract:

- **Two-bucket token limiter (`oauthcimdlimit.go`).** `oauthCIMDLimiter` holds a
  PER-SOURCE bucket keyed by the canonicalised source address AND one GLOBAL
  bucket; a request passes only when BOTH admit. The global bucket is not
  redundant: a per-source limiter alone is defeated by an attacker rotating across
  an IPv6 /64, whose fresh keys each get a full-burst bucket and simply evict the
  old ones out of the LRU — the global bucket is what bounds total outbound rate
  under that rotation. Admission is ATOMIC (both consulted under one mutex, a token
  consumed from NEITHER unless BOTH admit — a refusal never drains the admitting
  bucket, which would silently self-tighten the effective rate under load), and the
  returned wait is the MAXIMUM of the two buckets' waits. The limiter's OWN source
  map is a capped LRU (the key is attacker-influenced, so an unbounded map is the
  same memory-exhaustion bug #2434 closed for the CIMD document cache). The source
  key uses `netip.Addr.Unmap()` so `1.2.3.4` and `::ffff:1.2.3.4` share ONE bucket;
  any unparseable `RemoteAddr` shares a single `unknown` bucket (degrading toward
  MORE limiting). There is DELIBERATELY no `X-Forwarded-For` read (no trusted-proxy
  config exists) and no off switch (an operator-disable knob would reintroduce the
  unbounded case a config typo could reach); raise `--oauth-cimd-rate-burst` /
  `--oauth-cimd-global-rate-burst` to loosen it. Defaults: 5 burst + 1/10s per
  source; 30 burst + 1/1s global.
- **CIMD-branch-only placement.** The limiter is consulted INSIDE
  `resolveOAuthClient`, only on the store-miss branch — a `client_id` that
  resolves from the store dials nothing outbound and is never throttled. Honest
  narrowing: the guard sits on the CIMD RESOLUTION branch, a superset of real
  outbound fetches, so a `client_id` already warm in the fetcher's 15-minute LRU
  costs a token though it dials nothing; erring toward throttling is the safe
  direction for a volume-bounding control.
- **In-place 429.** The refusal renders as HTTP 429 with a `Retry-After`
  (RFC 9110 §10.2.3 delta-seconds, ceil, floor 1) and the flat RFC 6749 §5.2 body
  with code `temporarily_unavailable`. On authorize it fires before
  `ResolveRedirectURI` validates any `redirect_uri`, so it is rendered IN PLACE and
  NEVER redirected (RFC 6749 §4.1.2.1). The wait is carried as a typed
  `cimdRateLimitedError` in the `*oauthas.Error` Cause, so the renderer reads it
  structurally rather than re-consulting the limiter (which would burn a second
  token). NOTE: `temporarily_unavailable` is not in the RFC 6749 §5.2
  token-endpoint code set — see "Deliberate §5.2 departure" below.
- **Four-verdict loopback gate.** `resolveOAuthASState` gained an
  `oauthASNotLoopback` verdict: when `--oauth-require-loopback` is set and the
  listener is not loopback, all four OAuth routes answer `403 oauth_as_loopback_only`
  (reusing the `mcpLoopbackHost` predicate and DNS seam). The ladder order is
  disabled → misconfigured → not-loopback → enabled, so a deployment with both a
  defective issuer and a public bind reports the CONFIG defect (the actionable
  diagnosis). The gate DEFAULTS OFF: an on-by-default gate would silently 403 the
  #2439 / #1642 / #2032 live walks that need a public bind. The honest cost is that
  the gate protects nobody who has not opted in — the LIMITER is what protects an
  unaware operator, and the interim obligation below remains until the operator
  throws the switch.

Interim operational control: with the gate off (the default), an operator binding
this AS to a public address relies on the limiter to bound abuse rate — it does not
make the routes unreachable. Keep the daemon bound to loopback
(`FISHHAWKD_ADDR=127.0.0.1`) unless you have deliberately enabled a public bind.

**Deliberate §5.2 departure.** RFC 6749 §4.1.2.1's authorization-endpoint error
list includes `temporarily_unavailable`, so the authorize side is conformant.
§5.2's token-endpoint list is CLOSED and does NOT include it. We KEEP
`temporarily_unavailable` at the token endpoint anyway: HTTP 429 is the normative
signal and the body is advisory. Substituting a §5.2-conformant code would be worse
— `invalid_client` would tell the client its credentials are wrong (they are not),
inviting it to discard a valid registration rather than retry after the interval.

Still recorded, not acted on:

- **No operator write path for pre-registration.** `oauth_clients` rows are
  hand-written SQL today — `oauthstore.UpsertClient` exists but has NO caller.
  Registration-free CIMD clients are the supported path; a pre-registration
  admin surface is future work.
- **No negative cache for failed `client_id` validations.** Caching a TRANSIENT
  failure would pin a briefly-unreachable legitimate CIMD host as invalid for the
  whole TTL; the limiter already bounds repeated bad ids. Filed as its own design
  question rather than folded in here.

## Scope-completeness exempt emission gate (#2501)

`prompt.go::resolveHeldCommitExemption` decides whether an implement-stage prompt response carries the four zero-re-run fields — `open_pr_from_held_commit`, `held_commit_sha`, `held_commit_branch`, `held_commit_base_sha` — that make the runner skip the agent, the committed-tree gates, and the commit/push entirely and open the PR from the commit a scope-completeness park already held on the run branch.

- **Newest-entry-wins.** It resolves the NEWEST audit entry for the stage across all three `scope_completeness_*` categories (`_parked` / `_exempted` / `_failed`), comparing by `Entry.Sequence` and filtering on `entry.StageID == stage.ID`. Only an `exempted` newest entry emits. A stage that RE-parked after an earlier exempt therefore emits nothing — the later `parked` entry wins and the runner takes its ordinary agent path.
- **Base-SHA source order (#2563).** The fourth field is the base commit the held commit was built on — the value the runner ships as the PR artifact's `base_sha`, which the backend's success-arm `validate()` REQUIRES (without it the exempt resume opens the PR and then always fails category-B, orphaning it — the #2562 bug). It resolves from (a) `park.BaseSHA` (persisted on every park since #2563), else (b) the `base_sha` of the NEWEST `scope_completeness_parked` audit payload for the stage — read from the same category walk the gate already performs. The fallback is what makes an ALREADY-parked legacy row (e.g. #2169's, whose park predates the field) resumable instead of re-failing identically after a `retry_stage`.
- **Fail-closed.** This gate is the only thing standing between the widened `awaiting_scope_decision → pending` transition edge (which any caller can drive) and a PR opened from a commit no operator accepted. So every uncertain branch WARN-logs and emits nothing: a nil `AuditRepo`, any `ListForRunByCategory` error, no entry for the stage, a non-implement stage, a stage with no `ScopeCompletenessPark`, a park whose `HeldCommitSHA`/`RunBranch` is empty, an undecodable parked-audit payload, AND a park for which no non-empty base SHA resolves from either source. The four exempt fields are emitted together or all withheld — a missing base SHA withholds the held-commit fields too, so the runner never opens a PR it cannot ship.
- **Both handlers.** Wired into `handleGetStagePrompt` (the runner-facing dispatch path) AND `handleGetStagePromptRender` (the SPA preview), per that handler's byte-consistency convention.
- **The decision handler writes the proof before it opens the door.** Because the gate reads the audit chain, `scope_completeness.go::exemptScopeCompleteness` appends the `scope_completeness_exempted` entry BEFORE transitioning the stage out of `awaiting_scope_decision` (and therefore before the `Orchestrator.Advance` that can spawn a runner): a runner whose prompt fetch raced the append would read the older `parked` entry, get no held-commit fields, and re-invoke the agent — the full re-run the park exists to avoid. For the same reason the append is BLOCKING there: a failure returns 500 `exemption_unrecorded` and leaves the stage parked (re-POST the decision) rather than dispatching a stage whose exemption cannot be proven. `failScopeCompleteness` keeps its best-effort append — nothing reads the `failed` entry as authorization and its transition is terminal.
- The four json tags are the byte-identical cross-module wire contract with `runner/internal/upload/upload.go`'s `FetchedPrompt` decoder; a golden fixture duplicated across the two modules' tests (`goldenExemptPromptJSON`) is the prompt-response seam. The PR artifact the runner ships from the held commit has its OWN cross-module seam: `testdata/wire/held_commit_pr_artifact.json` is ONE file the runner test asserts its output equals and the backend test POSTs through the real handler — so a runner field the backend rejects fails at test time (#2558 tracks folding this into a shared wire package). Both seams exist because the runner and the backend are separate Go modules and no single-process end-to-end can span them.

## Build-required scope-drift park — the third shortfall class (E48.101 / #2548)

A scope-completeness park carries exactly ONE of three shortfall classes. The first two — missing declared scope files (#1151) and unsatisfied binding assertions (#1171/#2501) — are standalone-open-PR-push only and EXEMPT-ELIGIBLE. The third is not.

- **What it is.** A decomposition slice whose scope-only committed tree COMPILES but whose tests are red because a build-required file is owned by a SIBLING slice — the boundary cut a coupling (#2548's live case: slice 0 needed `backend/internal/server/prompt.go`, owned by slice 1). Before this, that failed category-B at commit+push, after the agent had exited, and the whole pass was discarded. Now the gate-verified commit is pushed to the child's own sole-writer slice branch, the child implement stage parks in `awaiting_scope_decision`, and the park names the build-required paths AND the owning sibling slice for each (`owning_slices`, resolved from the parent plan's `decomposition.sub_plans[i].scope.files` — the same authority `resolveSliceDependencies` reads).
- **Scoped to fan-out children.** The runner sets `ParkOnBuildRequiredDrift = isDecomposed && !isFixup`. A standalone run's build-required failure keeps today's category-B abort and its `fishhawk_resume_run --add_scope_files` in-band recovery (which a child has no equivalent of — its coupled file belongs to a sibling); a fix-up pass is byte-for-byte unchanged. A COMPILE failure and a test failure with EMPTY drift stay non-parkable on every path.
- **What it delivers: PARK-AND-PRESERVE WITH ATTRIBUTION, not amend-and-resume.** There is no verb to amend a slice boundary and resume a parked child; the amend verb is tracked separately as **#2591** (it turns on a real design question — for a decomposition child the coupled file is owned by a SIBLING slice, so widening the child's scope breaks the single-owner-file invariant). The only admissible decision here is `fail`; recovery remains "correct the decomposition and re-run", with two differences that are the point: the slice's work survives on `fishhawk/run-<parent>/slice-<n>`, and the operator is told which sibling slice owns the file that broke the boundary.
- **`exempt` is REFUSED (409 `exempt_refused_build_required`).** `scope_completeness.go::exemptScopeCompleteness` returns before it does anything — before the load-bearing `scope_completeness_exempted` append, before the stage leaves `awaiting_scope_decision`, before any `Advance`. Exempt resumes the stage and drives a PR-open from the held commit, whose committed tree is RED BY CONSTRUCTION for this class (that redness IS the shortfall), and opening a PR from it is the one outcome this design says must never happen. The refusal leaves the stage parked and appends no exempted entry, so the operator can still resolve it with `fail`.
- **Defence in depth.** `prompt.go::resolveHeldCommitExemption` independently withholds all four held-commit fields for any park carrying non-empty `BuildRequiredPaths`, even when an `exempted` entry is somehow newest for the stage. The endpoint refusal is the primary control; this gate is the last thing between a hand-written, legacy, or otherwise anomalous exempted entry and a PR opened from a red tree.
- **The parent side.** A parked child is settled but NOT terminal, so the decomposition parent stays in `awaiting_children`. That wait is correct but must not be silent: a `parent_awaiting_child_scope_decision` entry is emitted on the PARENT, from BOTH `emitParentAwaitingChildScopeDecision` at park time and `orchestrator.maybeAdvanceDecomposedParent` when a sibling settles. At most one entry exists per `(parent run, child stage)`, enforced at the store layer by migration 0067's partial unique index on `(run_id, payload->>'child_stage_id')` — not by a best-effort read in one emitter; the race-loser's `AppendChained` hits the index and both emitters treat that specific collision as the benign already-recorded outcome (INFO, park untouched) via `audit.IsParentAwaitingChildScopeDecisionDuplicate`. See `backend/internal/orchestrator/README.md` for why the emission has to be dual, and `backend/internal/audit/README.md` (0067 / #2594) for the index and the #2591 no-episode-component caveat.

> **ROLLBACK PRECONDITION — resolve every outstanding build-required park with `fail` BEFORE reverting #2548.** A revert removes the `build_required_paths` field AND both safeguards (the endpoint refusal and the prompt withholding gate). An already-parked build-required row would then decode with the key dropped, look like an ordinary park, and become EXEMPTIBLE — an ordinary operator `exempt` would set the held-commit emission fields and the runner would open a PR from a commit whose committed tree is red, which is precisely the outcome this design forbids. This is a narrow revert-window hazard and the resolution is operational, not structural: drain the parks first. Skipping this step does not corrupt data or break the revert; it leaves a live, reachable path to a red-tree PR for as long as any such park remains parked. To enumerate them, look for implement stages in `awaiting_scope_decision` whose `scope_completeness_park` JSONB carries a non-empty `build_required_paths`.

## PR-open checkpoint resume gate (E48.46 / #2169)

`prompt.go::resolvePushCheckpointResume` is the sibling of the exempt gate above, modelled directly on it, and emits the SAME four held-commit fields plus a fifth discriminator — `held_commit_resume_kind: "pr_open"`.

- **What it recovers.** The implement agent ran, the committed-tree gates passed, and `CommitAndPush` landed the gate-verified commit on the run branch — and only THEN did the PR open (or the artifact ship) fail, typically a sustained forge outage. Before this, `retry_stage` re-ran the whole agent: a ~$4-6, ~50-minute redo of work already sitting on the branch. Emitting the held-commit fields sends the runner to the same pre-agent short-circuit `#1231` uses, where it re-attempts only the idempotent adopt-then-create `OpenPR` (#2167) — which also covers the case where the PR actually opened and only the ship failed, since the adopt-by-head arm returns the existing PR.
- **Where the checkpoint comes from.** `pullrequest.go::failPullRequestStage` records `push_checkpoint` (`{branch, head_sha, base_sha, verified_tree_sha}`) onto the `pull_request_failed` audit payload when a `{outcome:"failed"}` report carries BOTH a non-empty `branch` and `head_sha`. A partial checkpoint records NOTHING, and `validate()`'s `failed` arm is deliberately NOT tightened to reject one: a 400 there would strand the implement stage in `running` until the SLA watchdog, converting a runner bug into a hung run. No new decode field and no migration — the runner reuses `pullRequestBody`'s existing coordinates and the checkpoint rides the audit chain.
- **Category set.** The walk covers `pull_request_failed` (the carrier) plus every category that can SUPERSEDE it: `pull_request_opened`, the three `scope_completeness_*`, `fixup_pushed`, and `child_pushed`. A walk over the carrier alone would happily resume a stage that has since opened its PR, parked, or pushed a fix-up.
- **Newest-wins, and it is load-bearing.** The gate tracks the newest entry across ALL categories and the newest `pull_request_failed` separately, then requires them to be the SAME entry. That identity IS the rule (keeping the two walks separate is what stops it being incidentally satisfied), and it makes the gate SELF-INVALIDATING: a successful resume writes `pull_request_opened`, which becomes newest, so a later retry takes the ordinary agent path instead of re-opening a PR that already exists.
- **Base SHA is REQUIRED, not optional.** The success-arm `validate()` requires `base_sha`, so emitting on a base-less checkpoint would open the PR and then always fail category-B, orphaning it (the #2562/#2563 defect the runner now also refuses before the forge). Failing closed here keeps the two layers agreeing rather than leaning on the runner's last line.
- **Exempt WINS.** Wired into both handlers as the `else` of the exempt call, so an exempt-resolved park always takes precedence and the two can never both set the fields — an exempt emission therefore always carries an EMPTY resume kind. The operator's exempt decision is a judgment about the held tree; a checkpoint is a mechanical retry hint.
- **Fail-closed** on: a nil stage, a non-implement stage, a FIXUP dispatch (a fix-up exists to re-invoke the agent with the reviewer's concerns — resuming would open a PR from the UNFIXED commit and report the fix-up as done), a nil `AuditRepo`, any `ListForRunByCategory` error, no entry for the stage, a newest entry that is not a checkpoint-bearing `pull_request_failed`, an undecodable payload, and an incomplete checkpoint. The cost of a wrong emission is a PR opened from an unintended head; the cost of a wrong omission is today's agent re-run. Those are not symmetric.
- **Runner-side guards** (`runner/cmd/fishhawk-runner/README.md`): the `pr_open` kind additionally makes the runner re-verify the run branch's REMOTE TIP still equals `held_commit_sha` before touching the forge (a checkpointed commit passed no operator gate the way a park did), and re-report the checkpoint on its own failure so a multi-retry outage stays resumable.

