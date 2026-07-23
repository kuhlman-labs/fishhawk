# backend/internal/server

fishhawkd's HTTP surface: route handlers for the v0 REST API and the
cross-component seams they anchor.

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
  `acceptance_outcome_recorded` passed verdict for the other two), and `Advance`
  is re-entered so the run rolls forward.
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
- The complementary BLOCKING gate for a FULLY-untouched concrete DECLARED scope path is the runner's #1151/#1231 scope-completeness park (`gitops.MissingScopeFiles`; the scope-completeness invariant in `docs/ARCHITECTURE.md`); this is the advisory pre-review surface for the partial / operator-added case.
- Audit-kind note in `docs/issue-comment-surfaces.md`.

## Re-review convergence: settled ledger + re-litigation guard (#1913)

`trace.go` makes implement re-review rounds converge by threading settled history forward and turning operator arbitrations into a machine-binding suppression guard (issue #1913; measured churn on runs a04d5cbf / 98704b0c).

- **Settled-ledger threading.** `settledConcernsForReview` (sibling of the OPEN-only `priorConcernsForReview`) gathers the stage's `waived`/`deferred` + `addressed`/`superseded` concerns into `prompt.Trigger.SettledConcerns`, threaded into every post-fixup round so a round-N reviewer has the full settled history (deferred arbitrations, invisible before, now reach the reviewer). Waived concerns MOVED out of `priorConcernsForReview` into this set; `hasFixupRoutedConcern` still gates the #1725 delta on `addressed_pending`, unaffected.
- **`concern_relitigation_suppressed` audit-category contract.** An internal, advisory, best-effort audit kind (system actor, payload `{settled_ref, settled_state, severity, category, note, reviewer_model, origin_review_sequence}`) written by `persistReviewConcerns` → `suppressRelitigation`/`appendRelitigationSuppressed` when a verdict concern's `settled_ref` resolves to a **same-run/same-stage/same-stageKind** `waived`/`deferred` concern AND its `new_evidence` is empty — the guard excludes that concern from the durable open-row insert and records this entry instead (so the suppression is visible, never silent). It posts NO issue comment and adds no Notifier method, so it is NOT an issue-comment surface (it is registered in `audit.KnownCategories` for `fishhawk_await_audit`). Fail-open on every other case — unparsable/unknown ref, cross-stage ref, non-waived/deferred state, non-empty `new_evidence`, and any lookup/append error (WARN) all fall through to the normal insert, so a sloppy tag never suppresses a genuine finding and a store outage never wedges the loop. A re-raise against an `addressed`/`superseded` concern is deliberately insertable (a genuine regression must reach the operator).

## Gate decision view (`gateview.go`, E48.13 / #1960)

`GET /v0/runs/{run_id}/gate-view` (`handleGetRunGateView`) answers "what is still open at this gate and why" in ONE read, replacing the `getRun` + `listRunAudit` stitch an operator otherwise runs at a review/fix-up gate. The full concern prose already exists server-side (`concern.Concern.Note`) but is deliberately elided by the run-status concerns block (`runs.go::buildRunConcernsPayload`) and further stripped by the MCP compaction levers (`compact.go`); this surface returns it intact.

- **Response shape.** Each OPEN concern (`raised`/`addressed_pending`/`reopened`) carries its FULL `note`, `severity`, `category`, `reviewer_model`, `origin_review_sequence`, a derived `round` (implement-only), `state_reason`, `has_suggested_patch`, plus `fixups[]` and `resolutions[]`. The settled ledger (`waived`/`deferred`/`addressed`/`superseded`, each with `state_reason`) and the run's `concern_relitigation_suppressed` entries ride along. `suggested_patch` diff text stays elided as `has_suggested_patch` (token-dominant, not decision prose) — the response is sized by SCOPING (the optional `stage_kind=plan|implement` filter), not truncation.
- **History is reconstructed from the immutable audit payloads**, because `concern.StateReason` is OVERWRITTEN on every transition (`MarkAddressedPending` writes the routing reason, then `applyConcernResolutions` overwrites it with the re-review note) — there is no stored per-round history. `fixups[]` join each `stage_fixup_triggered` whose `concern_ids` names the concern (contributing `{sequence, reason}`) to the outcome (`apply_path`/`head_sha`) of the earliest following `fixup_pushed`/`fixup_no_changes` (`pending` when none yet). `resolutions[]` join each `implement_reviewed`/`plan_reviewed` payload's `concern_resolutions` entries keyed by concern ID. `round` = `1 +` the count of same-stage `stage_fixup_triggered` sequences below the review sequence (the `review_action_hint.go::latestRoundConcerns` convention); the handler sorts fetched audit entries by `Sequence` defensively rather than relying on repo order.
- **Degradation is visible, never silent.** `AuditRepo` nil, or any per-category `ListForRunByCategory` error, returns 200 with the concerns intact, `history_incomplete=true`, and `history_gaps` naming each failed category; a single malformed payload entry is skipped warn-only while its siblings still join. `ConcernRepo` unconfigured → 503 `gate_view_unconfigured` (mirrors `fixup_unconfigured`); `RunRepo` unconfigured → 503 `run_repo_unconfigured`; unknown run → 404; bad `stage_kind` → 400; a `ConcernRepo.ListByRun` error → 500 `internal_error`. **Auth mirrors `handleListRunAudit`'s read posture** (full reviewer prose must not be anonymously readable, #1960 authz): a run-bound `mcp:run:<uuid>` token is authorized by the cross-run subject guard alone — it may read only its own run (403 `cross_run_gate_view`, mirroring the fix-up handler; a malformed `mcp:run:` subject → 401 `authentication_required`) — while every other caller must clear the `read:audit` scope (anonymous → 401 `authentication_required`, a token missing the scope → 403 `insufficient_scope`, cookie-session operators bypass per `requireWriteScope`).

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

## Slash-command approval (#238)

`/fishhawk approve` and `/fishhawk reject` in the triggering issue's conversation submit a gate decision against the run's currently-awaiting-approval stage — closing the loop entirely in the issue surface.

- `webhook.matchIssueComment` parses the three commands (run / approve / reject) and tags the `Match.Action` accordingly; the `Match.CommentBody` carries any trailing reviewer rationale. `Dispatcher.Handle` routes Action ∈ {approve, reject} to `Dispatcher.handleApprovalCommand`, which delegates to the `ApprovalCommandHandler` interface.
- **Implementation lives on `Server.HandleApprovalCommand`** (`issue_approval.go`), where the approval, role, and stage-check repos already live.
  It (1) finds the awaiting-approval stage by listing runs for the repo and matching `trigger_ref = issue:N`, (2) authorizes the sender via the existing `role.Resolver.CanApprove`, (3) submits via `approval.Repo.Submit` with `Surface = SurfaceGitHubComment`, (4) advances the stage (refactored `advanceStage(ctx, …)`), (5) writes the same `approval_submitted` audit row the HTTP path writes, (6) calls `Orchestrator.Advance`, and (7) posts a reply comment via `Notifier.NotifySlashApprovalReply`.
- Replies are NOT deduped — every command attempt produces its own reply.
- Per #253 / ADR-017 the slash command no longer reads `stage_check` state — branch protection is the merge gate.
- The slash-command path falls open when its deps aren't wired (`approvalCommandConfigured()` returns false), and is best-effort throughout: a comment failure logs but never returns 5xx to GitHub.
- PR-triggered approvals, `/fishhawk cancel`, and per-stage targeting (`/fishhawk approve plan`) are out of scope — separate followups when those scenarios surface.

## Run lifecycle endpoints

### Run CRUD handlers (`runs.go`)

`backend/internal/server/runs.go`; wired in `backend/cmd/fishhawkd/serve.go` from `FISHHAWKD_DATABASE_URL`. POST/GET/list/cancel for runs.

- **Idempotency (E8.2)**: POST accepts `Idempotency-Key` — same `(repo, key)` returns the existing run with 200 instead of creating a duplicate. Webhook-driven runs use the dedicated dedup path (E3.9) and don't carry a key.
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
- **Blocking budget admission**: `handleCreateRun` calls `budget_admission.go::checkBlockingBudget` after the plan-reviewer guard and before `CreateRun` — documented with its shared decision core in `backend/internal/webhook/README.md`.

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

### Plan-gate over-cap split reject (`plan_warnings.go` / `plan.go`, #2055, E50.3)

`backend/internal/server/plan_warnings.go::overCapSplitRejection` — called from `handleShipPlan` (`plan.go`) right before `runPlanReviews`. This is the **SERVER-AUTHORITATIVE, count-derived HARD reject** for an over-cap monolith — the E50 keystone, distinct from the advisory above.

- **Count-derived, flag-independent.** `overCapByCount` factors the #2053 count determination (`resolveImplementConstraints` → `len(scope.files)` vs `MaxFilesChanged`) into a shared helper that `overCapWarning` (advisory) and `overCapSplitRejection` (reject) both call; it **never reads `over_cap`**. `overCapSplitRejection` returns a reject reason when the plan is over cap **by count** AND carries no `split_proposal`, and `""` otherwise — so an over-cap-by-count monolith without a split is rejected whether `over_cap` is omitted, `false`, or `true`; an over-cap plan carrying a valid `split_proposal` is accepted; an under-cap plan is unaffected.
- **Decodes with `json.Unmarshal`, not `plan.Parse`** (in `handleShipPlan`): decoding without `semanticCheck` keeps the gate flag- AND parse-independent, so it fires on the server-derived count alone for `over_cap` `{omitted, false, true}` alike and no in-artifact semantic error can preempt the authoritative count reject.
- On a non-empty reason: emit a terminal `plan_review_failed` audit entry (`emitReviewFailed`), fail the plan stage category-B (`run.FailStage` + `advanceAfterFailure`, the same terminal path the plan-invalid/decomposition reject uses), and set `gatingRejected` so `advancePlanStageTerminal`/`notifyPlanReadyIfReady` are suppressed — the rejected plan never advances, and the artifact is still stored so the operator can inspect it via `fishhawk_get_plan`.
- **Fail-open** on every leg exactly like the advisory (nil `RunRepo`, `GetRun` error, no spec/implement stage, unresolved/zero cap → `overCapByCount` returns `ok=false` → `""`), so an unresolved cap can never spuriously block a plan.
- There is deliberately **no** `over_cap ⇒ split_proposal` coupling in `semanticCheck` (`backend/internal/plan/validate.go`). Because that check had no view of the resolved cap it was count-blind, and rejected an **under-cap** plan that merely set the `over_cap` hint — turning the advisory into a server rejection across every `plan.Parse` caller (the plan reviewers plus the fail-open scope/surface/test gates) and breaking the under-cap-unaffected guarantee. It was removed (#2055 fixup); `overCapSplitRejection` is the sole authoritative over-cap reject, and `semanticCheck` only validates the **structure** of a `split_proposal` that is present (`checkSplitProposal`).

### Plan-gate scope-regression sweep (`scope_regression.go`, #1257)

`backend/internal/server/scope_regression.go::runScopeRegression` — called from `handleShipPlan` (`plan.go`) immediately after `runTestSweep` and before `runPlanReviews`, but ONLY on a **revise pass**.
`fishhawk_revise_plan` regenerates the WHOLE plan artifact, so a narrowly-scoped revision constraint can silently DROP files the immediately-prior (revision-base) plan scoped — even one that says "keep everything else" — and the runner then scope_drift-excludes the agent's edits to them (#1257).

- The gate's run-guard is a prior `plan_revised` audit entry (`countRevisePasses > 0`, the same durable revise-pass record the bound is counted against); the revision **base** is captured via `loadApprovedPlanForRun` **BEFORE** `ArtifactRepo.Create` (an after-Create capture would diff the just-shipped plan against itself and report no regression).
- `scopedPaths(*plan.Plan)` is the pure helper: the slash-normalized (`filepath.ToSlash`), sorted, de-duplicated UNION of `plan.Scope.Files[].Path` AND every `plan.Decomposition.SubPlans[].Scope.Files[].Path`.
  The observed regression dropped files living in decomposition sub-plan slice scopes, so the diff MUST cover sub-plan scopes, not only the flat top-level list.
- `runScopeRegression` computes `removed = base-scoped − new-scoped` (the regression) and `added = new − base`, sets `Regressed = len(removed) > 0`, and writes a `plan_scope_regression` audit entry (payload `ScopeRegressionPayload{removed_files, added_files, scanned_files, regressed}`, removed/added empty arrays not null on a clean diff).
- Advisory + fail-open: `base==nil` (non-revise ship), a nil `AuditRepo`, a parse failure, OR an audit-append failure WARN-logs and returns nil — never blocks or unwinds the ship.
- The returned payload threads into the plan-review prompt's gate-evidence section as a HIGH-severity signal (`prompt.ScopeRegressionEvidence`, rendered only when `RemovedFiles` is non-empty) so BOTH reviewers and the operator see the drop before approving.
- **Budget refund seam**: `backend/internal/server/revise.go::countRegressedRevisePasses` counts the stage's `plan_scope_regression` entries with `regressed==true`.
  `handleRevisePlan` sets `run.ReviseOptions.BudgetPassCount = max(0, priorPasses − regressedPasses)` so a regressing revise pass refunds the NORMAL revise budget (the operator gets a free recovery pass), while `PriorPassCount` keeps governing the HARD CEILING (`defaultReviseCeiling` = 3) so the refund cannot create an unbounded revise loop — total work stays bounded.
  `RevisePlanStage` compares `BudgetPassCount` against `MaxPasses` for `ErrReviseBudgetExhausted` and derives `remaining`/`forced` from it, leaving the ceiling check on `PriorPassCount`.

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
- `POST /v0/campaigns/{id}/runs` (E26.2 / #1481, scope `write:campaigns`; `handleStartCampaignItemRun` — the operator-driven local-drive start): DAG-gates an `issue_ref` via `campaign.NextEligible`, mints the run through `StartRunForCampaignIssue` carrying `runner_kind`, links + transitions the item, advances a pending campaign.
  Gate codes: `item_not_eligible`/`campaign_item_not_found` (409+404), `item_human_led`/409 (a deps-satisfied `autonomy:low` item — a human must lead it, no ref named to start, #1697), `campaign_not_startable`/409, `campaign_run_start_failed`/502; **no `idempotency_key`**.
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
