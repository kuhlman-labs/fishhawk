# backend/internal/server: Authorization, identity and sessions

Split out of `backend/internal/server/README.md` (the index) on 2026-10-29 so concurrent runs stop colliding on one file. Section text is moved verbatim.

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
   API response. The `error` key is deliberately absent. As of E54.59 / #3113 the
   list also admits `resolved` / `items_total` /
   `suggested_grooming_order_limit` / `budget_seconds` — the counts the `504
   issue_set_resolution_timeout` refusal is made of, each a product-owned
   integer the handler computes (see the campaign create section below).
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
(d) **`writeJSON` 2xx bodies have NO runtime redaction chokepoint and no AST
tripwire at all** (E45.94 / [#3631](https://github.com/kuhlman-labs/fishhawk/issues/3631)):
`writeError`'s allow-list and the raw-cause guard are both scoped to
status-carrying refusals, so a raw cause interpolated into a `200`/`201`
`message` field (the shape `merge_run.go`'s `writeAlreadyMergedResponse` fixed)
is caught only by human review, never by a test or a static check. Recorded
here rather than fixed structurally — the next such site needs a reviewer, not
a tool.

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
(destructive/admin sub-actions, 13 routes: cancel, recover, redrive, revive,
reset-branch, rebase-branch, reviews/reconcile, reviews/rerun, reap-failure, signing-key,
deployment rollback, installation-token, mcp-token).

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

**Route sweep (#4211).** `route_account_sweep_test.go` pins the WIRING of
every run-scoped route to the wrappers above, present and future, without a
hand-listed route table. `registerRoutes` takes a one-method `routeRegistrar`
(`HandleFunc`), which `*http.ServeMux` satisfies, so the test hands it a
recorder and enumerates every registered pattern carrying a `{run_id}`,
`{stage_id}` or `{concern_id}` segment, together with the exact wrapped handler
the mux receives. Two assertions run per route. (1)
`TestRunScopedRoutes_CrossAccountBearer_Forbidden` drives the full
`s.Handler()` chain with a bearer bound to account B against a run owned by
account A. It requires `403` with exactly one `account_forbidden` envelope, a
run-repository read log equal to the wrapper's own resolution (a further read
means the handler ran), zero audit appends and zero ERROR log records. It also
checks that each concrete request routes to its own pattern on a real
`ServeMux`. (2) `TestRunScopedRoutes_AccessTierPinned` infers each route's tier
by probing its captured handler with cookie identities and compares it with
the pinned rule: `GET` → `readAccess`, any other method → `memberWrite`, and
the `adminWriteRoutes` set → `adminWrite`. Moving a route between tiers turns
it RED until that set is updated. **No `404` is admitted:** `enforceAccount`
answers a resolved foreign run with `403 account_forbidden` on every tier, so
no run-scoped route hides existence at the wrapper; existence-hiding would
need `enforceAccount` and the sweep changed together. **Opt-outs** go in
`accountIsolationOptOuts` as a pattern plus a reviewed, non-empty reason; an
entry naming an unregistered pattern fails, as does a stale `adminWriteRoutes`
entry. **Floors:** each family (`runs`, `stages`, `concerns`) must enumerate at
least its pinned count, so a broken enumeration cannot pass vacuously. Floors
are raised by hand and never lowered silently. A run-scoped pattern outside the
three families, without a method, or carrying an unknown placeholder fails
closed. **Residual:** the recorder sees only routes registered inside
`registerRoutes`; a route mounted on the mux anywhere else escapes the sweep.

**403 codes:** `account_forbidden`, `account_unresolved`, `insufficient_role`.
The cross-boundary integration matrix is `authz_account_test.go` (per-mode
`enforceAccount` logic) plus `route_account_sweep_test.go` (per-route wiring).

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

## `approvals.members` enforcement (#4116)

A non-empty `approvals.members` list is an exact-match approver allow-list (`identity.SubjectListed`; the canonicalisation rule — provider-qualified member verbatim, plain member qualified with the run's forge via `observationForgeID(run.InstallationRef)`, principal NEVER qualified and required to be `github:`/`gitlab:`-shaped, byte-exact — is documented in `backend/internal/identity/README.md`). That rule checks the subject's SHAPE only; the credential leg below checks how the presenting credential was minted. An empty or absent list is byte-identical to before.

**The principal (`approvalPrincipal`, `quorum.go`).** Non-delegated: the authenticated subject (an `operator-agent/...` subject has no forge provider, so a NON-delegated agent-kind submission on a members gate is now refused 403 where it was previously recorded-but-never-counted — deliberate, pinned by `TestSubmitApproval_Members_NonDelegatedAgentKindRefused`). Delegated human-kind: the subject — the `on_behalf_of` `writeApprovalAudit` records after the #2381 remap. Delegated agent-kind: the seated captain via `delegatedCaptainPrincipal(ctx, stage, "").OnBehalfOf` (E76.3 / #3766), `""` on a vacant seat (refused, fail-closed). Never `operatorrole.DelegatedApprovalActorSubject`.

**Two enforcement seams.**

- **Pre-Submit (`checkApprovalPredicates`).** After the escalation resolution and the #2374 `ferr` handling, the members leg runs BEFORE the delegated/agent forge skip and before any forge call. The run row is read ONCE by a direct `s.cfg.RunRepo.GetRun` in the function's own body (shared with the forge leg); a failure is the retryable `503 forge_unavailable` `reason: run_row_unreadable` with `predicate: members` — never a guessed github forge. Unlisted → `403 approver_predicate_unmet` (`details.predicate: members`, `members`, `members_principal`, `subject`, `escalated`, `result`, `next_actions`) plus an `approval_predicate_rejected` audit row whose snapshot carries `members` / `members_principal` / `members_listed: false`; no approval row. Listed and no forge predicate (or a delegated/agent submission) → returns `(&predicateResolution{MembersPrincipal, MembersListed:&true}, true)` BEFORE `resolvePredicates`; otherwise the forge leg runs and its satisfied resolution carries the same two fields. A listed delegated submission is ADMITTED (row recorded) and never COUNTED — the #1709 agent floor in `eligibleApprover` is untouched.
- **Count time (`approveStageAs`).** The distinct eligible subjects pass through `filterListedSubjects` before the escalated forge re-validation, so a row that reached the table without the pre-Submit leg (the in-process campaign auto-driver, or a row recorded before deploy) cannot count. The forge comes from a direct `GetRun` in `approveStageAs`' own body; a failure sets `gateUnreachable` (fail toward NOT advancing). `predicate_snapshot.members` (+ `members_principal` / `members_listed` when the handler resolved them) is additive and `omitempty`.

**The credential leg (#4633; ADR-123, #4750, RATIFIED 2026-10-27).** `identity.SubjectListed` admits any `github:`/`gitlab:` subject, and a static API token (`fishhawkd token issue`, or a child token from `POST /v0/tokens`) carries whatever subject its minter wrote, so the members leg also checks the credential. Inside the members leg, `credentialBound := principal == subject`: the principal IS the presenting credential's own subject for a direct submission and a delegated human-kind one (#2381 remap), and is NOT for a delegated agent-kind one (principal = the seated captain), which is therefore not credential-evaluated. When bound, `approvalCredentialKind(IdentityFrom(ctx))` (`approval_credential.go`, a thin adapter) passes the fields `bearerAuth` and the cookie path set to `identity.CredentialKind`, the ONE classifier (ADR-123 D1) — `SessionID` without `TokenID` → `session`; `OAuthAudience` → `oauth_as`; no `TokenID` → `unknown`; an `mcp:run:` subject → `mcp_run`; else the token's `auth_method` (`oauth` / `static` / empty → `unrecorded` / other → `unknown`) — and `identity.ForgeVerifiedCredential` (the one allow-list: `session`, `oauth`, `oauth_as`) decides. A LISTED principal on a non-verified kind is refused `403 approver_predicate_unmet` with `details.reason: credential_not_forge_verified`, `details.credential_kind`, `details.forge_verified_kinds` and the usual members fields; the `approval_predicate_rejected` snapshot records `members_listed: true` and `members_credential`; no approval row. The kind is threaded on `predicateResolution.MembersCredential` — copied into the forge resolution when a forge predicate also runs — so an admitted approval's `approval_submitted` snapshot and a forge-predicate rejection's snapshot record `members_credential` too (omitempty). **Surfaces covered:** every approval-row gate (plan, deploy, approval-row review, grooming/upkeep/comms) — all reach `checkApprovalPredicates` through `POST /v0/stages/{stage_id}/approvals`, which the MCP approve tools call. **Forge-verified by construction:** the merge-settled review gate (`merge_members.go`) checks forge-read identities, not a credential. **Residual:** the count-time half (`approveStageAs`, `filterListedSubjects`) does not re-check the credential, so an approval that reached the table without the pre-Submit leg (the in-process campaign auto-driver, or a row recorded before deploy) counts on its subject alone. The impact inventory `fishhawkd approver-members` carries the same credential leg (`backend/cmd/fishhawkd/README.md`).

**Escalations cannot widen it.** `escalatedApprovals.members` is a fresh copy of the BASELINE list only; `spec.ComposedRequirements` has no members field and the schema gives an escalation's `require.approvals` no `members` key (`TestParseRefusesEscalationMembers`). Narrowing would need a schema key (out of scope).

**Merge-settled review gates (`merge_members.go`, E39.17 / #4132).** A review gate that settles on PR merge records no approval row; it resolves through `resolveReviewStageOnMerge` (`pullrequest_review_events.go`), whose three callers — the GitHub `pull_request` webhook, the merge-status poll `ResolveReviewFromPollState` (GitHub and, through `forge.Get`, GitLab) and the GitLab merge-request webhook (`webhook_gitlab.go`) — share ONE seam, `mergeSettledMembersAdmit`, called in the merged arm after the ADR-036 `checkImplementReviewSettled` hold and before `writePRMergedAudit`. Rungs: a terminal review stage admits; `approvalsForRun(target, …)` (`quorum.go`, the pure spec walk `fetchApprovalsForStage` now delegates to) — `errNoCachedWorkflowSpec` or a typed `stageTypeNotInWorkflowError` (`stageTypeMissing`) admits with no forge call, any other error holds `members_unevaluable`; nil/empty `members` admits with no forge call; the merger is read from the forge through the merge-observation ladder (`resolveObservationTarget` + `prStateReaderFor` + `mergeObservationScope`, `forge.PullRequest.MergedBy`), falling back to `meta.actorLogin` only when the forge yields none AND `meta.actorKind` is user; on GitHub the approver leg lists reviews (`approvingReviewers`, the latest-per-reviewer fold of `latestReviewVerdict`); each identity is qualified with `identity.CanonicalMember` and matched by `listedIdentity` → `identity.SubjectListed`. A hold returns false and appends `merge_settle_members_refused` (reason `members_unevaluable` \| `identity_unavailable` \| `no_listed_identity`), deduplicated on `(stage_id, fingerprint)`; a list error appends anyway, an append error still holds. INTERNAL: not in `issuecomment` `activityCategories`. Approval condition C1: the merged arm reads the review stage through `findReviewStageStrict`, and a stage-list read ERROR holds the merge (no `pr_merged`, no no-review-stage tail, no supersede sweep) instead of being read as "no review stage". Counterfactual vehicles: the call site forced false → `TestResolveReviewStageOnMerge_MembersWebhookUnlistedHeld` / `TestGitLabMergeRequest_MembersUnlistedMergerHeld` settle; `listedIdentity` admitting any non-empty id → `TestMergeSettledMembers_Verdicts` rows 09/14/25/30; dropping `COMMENTED` from the fold skip → row 11; deleting `|| stageTypeMissing(err)` → row 04 (secondary, pgtest: `TestMergeCandidateQueue_MergeOnSpecWithoutReviewStageAppendsNoMembersRefusal`); `merger = pr.MergedBy` → `""` → rows 08/24 and `TestGitLabMergeRequest_MembersPollResolvesFromForgeMerger`; restoring the nil-on-error stage lookup → `TestResolveReviewStageOnMerge_StageLookupFailureHoldsMerge`; dropping the dedup scan → `TestMergeSettledMembers_AuditDedup`. Also not yet members-aware: the captain seating predicate fold, the delegation preview, and the permission-drift extraction.

**Counterfactual vehicles** (`approval_members_test.go`, `approval_members_pg_test.go`): the members-leg condition forced false → T1/T13 (200 + a row); `filterListedSubjects` returning its input → T11 advances (T1 stays green — the arms are isolated); `approvalPrincipal` returning the synthetic subject → T5 refused; a delegated early return before the leg → T6 records a row; the captain branch returning `""` → T7's seated arm refused (vacant arm green); qualifying the subject → T4 (`TestSubmitApproval_Members_NonForgeSubjectUnlisted`) + `TestSubjectListed` admit; `effectiveApprovals` dropping members → T8/T1 admit; a github fallback on either run-row read failure → T10 200 / T12 advances (both assert the `callerScopedGetRunFailRepo` fault `Fired() >= 1`). Credential leg (#4633, `approval_members_test.go`, `approval_credential_test.go`, `approval_credential_pg_test.go`, `identity/credential_test.go`): the credential guard forced false → `TestSubmitApproval_Members_StaticCredentialRefused` / `…UnrecordedCredentialRefused` / `…UnknownAuthMethodRefused` / `…NoCredentialRefused`, `…DelegatedHumanStaticRefused` and `TestApprovalCredential_PostgresCrossBoundary` admit (200 + a row); `ForgeVerifiedCredential`'s default returning true → `TestForgeVerifiedCredential`; an empty or static `auth_method` mapped to the device flow → `TestApprovalCredentialKind` + the matching members test; `credentialBound` narrowed with `&& !delegated` → `…DelegatedHumanStaticRefused`; dropping the `MembersCredential` copy into the forge resolution → `TestSubmitApproval_Members_WithForgePredicateRecordsCredential`.

## The approve advance is a compare-and-swap (`approvals.go`, E50.15 / #2656)

`advanceStage`'s approve leg applied the transition with the non-CAS `RunRepo.TransitionStage`, and `postgresRepo.transitionStage`'s row-locked `from == to` short-circuit returns a **silent success**. So an approval racing one that had already advanced the stage was told it succeeded and walked straight into `finishApprovalAdvance`'s post-approval tail — `fileSplitProposalChildren` and `fileOrLinkLiveValidationWalk` — filing duplicate work items. `advanceStage` now takes the loaded `*run.Stage` and routes the approve through `casTransitionFromObserved`.

- **Anchored on the OBSERVED state, not on the literal `awaiting_approval`** (operator binding condition, #2656). The CAS `from` is `stage.State` — the state this caller loaded — so it means "nothing changed since I looked". That closes the race identically (a concurrent flip changes the state, the compare fails, the loser is refused) while keeping every transition the endpoint admits today admissible: no currently-succeeding approve of a non-parked stage turns into a 409, and no external consumer sees a 200 become a 409.
- **Capability degradation, not a hard dependency.** `casTransitionFromObserved` runtime-asserts `run.StageCASTransitioner` — the same convention as `markStageRunningOnPromptFetch` (`prompt.go`) and `handleHostDispatch` (`host_dispatch.go`). The production `postgresRepo` implements it; an in-memory `RunRepo` that does not degrades to the plain `TransitionStage`, i.e. today's behavior, with no panic.
- **The reap-failure path is the deliberate EXCEPTION — a hard dependency, refusing rather than degrading (#2672).** `failStageForReap` (`reap_failure.go`) also type-asserts `run.StageCASTransitioner`, but a repo that lacks it makes the reap FAIL LOUDLY (`errReapRepoNotCAS` → `handleReapStageFailure` classifies it BEFORE its post-transition re-load and answers `503 reap_failure_repo_not_cas`, nothing transitioned, no audit entry) rather than fall back to `run.FailStage`. The reason the reap path cannot degrade where the others can: `run.FailStage` re-anchors through whatever live state a concurrent advance produced and takes the legal park → failed edge on the four non-children parks — exactly the live-park destruction GUARD 2 (`reapReanchor`) exists to prevent. `run.FailStage` is now UNREFERENCED from `reap_failure.go`, so that edge no longer exists there rather than being merely unreached. A compile-time assertion in `run/postgres.go` (`var _ StageCASTransitioner = (*postgresRepo)(nil)`) plus a boot-time refusal in `cmd/fishhawkd/serve.go` (`runRepoCASWiringError`, called once before `server.New` — refuses startup for any non-nil `RunRepo` lacking the capability) make this refusal unreachable in a deployed daemon: production `postgresRepo` always has the capability.
- **The reap-failure endpoint carries an OPTIONAL `expected_state` compare-and-set precondition (E67.51 / #2699).** Supplying it makes the reap conditional: the value must name one of the endpoint's three reapable anchors (`reapConditionalAnchors` = {`pending`, `dispatched`, `running`}, the complement of the terminal states and `reapProtectedParkStates`) or the request is `400 validation_failed`, and the reap is refused `409 stage_state_precondition_failed` when the stage is not in it — checked BOTH at the handler's load (ahead of the terminal no-op and the protected-park fast path) and, decisively, at the row-locked CAS. **Conditional mode DISABLES the re-anchor** (`reapFailCAS`'s `conditional` arm returns any `run.StageStateChangedError` unchanged instead of retrying): the #1907 benign `dispatched → running` absorption IS the interleaving #2699 names — a concurrent dispatch spawning a runner while the reap is in flight — so a caller that pinned a state must LOSE it, not absorb it. That is what turns `fishhawk_reap_stage`'s client-side re-probe from a narrowing into a closed race. **Presence, not emptiness and not decodability, is the switch:** the field is a `json.RawMessage`, so an OMITTED field is the unconditional request (today's absorbing, idempotent contract, byte-for-byte, for the detached reaper and `run_children`'s spawn-error compensation — the `dispatch_reaper_failed` audit payload omits the key entirely then, so its key set is unchanged) while ANY present value that is not a string naming an anchor — `""`, an explicit JSON `null`, a number, an object — is a `400`, never a silent unpinned reap. The raw bytes are load-bearing over a `*string` (E67.51 fix-up): `encoding/json` decodes an explicit `null` to a NIL pointer, indistinguishable from omission, so a `*string` presence check downgraded that malformed conditional request to an UNPINNED reap — the untrusted-input path into the very race this closes. `validateReapExpectedState` owns the resolution; `TestReapStageFailure_PresentNonStringExpectedStateIs400` pins it per class. Conditional callers also diverge from the idempotent no-op: an already-terminal or parked stage is a `409`, not `200 {transitioned:false}`. Ordering is load-bearing — `errReapRepoNotCAS` is classified BEFORE the `409` (a wiring fault must never be reported as a lost precondition) and a non-typed repo error still falls through to the documented retryable `500`. **What a 409 guarantees, precisely:** the stage was never driven to `failed`, no audit entry was written, no advance ran. It does NOT mean nothing was committed — a `dispatched`-pinned walk is `dispatched → running → failed`, so a refusal on the SECOND leg leaves that intermediate hop committed (exit from `running` mid-walk is reachable: `run/transition.go`'s `StageStateRunning` row admits five non-terminal successors). Pinned by `reap_failure_test.go`'s `TestReapStageFailure_Conditional*` set — including `ConditionalRefusesMidFlightAdvance` (the done-means committed-state read), `ConditionalSecondLegFlipIs409`, `ConditionalRejectsNonAnchorState` (the explicitly-empty row), `ConditionalHappyPathPending` + `ConditionalPendingMismatchIs409` (the third anchor, whose only other coverage is unconditional), `ConditionalParkIs409` (subtests DERIVED from `reapProtectedParkStates` itself, so a sixth park is covered by adding it to the map), `OmittedExpectedStateIsUnconditional`, `PresentNonStringExpectedStateIs400`, and `UnconditionalAuditPayloadKeySet`.
- **Both hooks are protected by construction.** The loser's `run.StageStateChangedError` is wrapped as `*approveActionError{failedAt: gateActionAdvance}` and returned from `finishApprovalAdvance` BEFORE `recordDrivePlanApproved` / `fileSplitProposalChildren` / `fileOrLinkLiveValidationWalk` / `recordPlanPredictedRuntime` / `notifyPlanReady` / `notifyStatusUpdate`. No new early return was needed.
- **The deploy pre-execution leg gets the same treatment**, anchored the same way (`awaiting_deploy_approval → dispatched` when that is what the caller observed). It is the sharper of the two: the hook it guards is an EXTERNAL delegating-pipeline fire, so a silently-succeeding second advance means a duplicate release trigger.
- **The reject leg is deliberately unchanged.** `run.FailStage` already CASes internally (`run/failure.go`, re-anchoring on the actual state) and refuses a terminal stage via `ValidStageTransition`.
- **The raced loser keeps its approval row and its `approval_submitted` audit entry** (`Submit` and the audit write both precede the advance) and receives `409 invalid_state_transition` with `details.stage_id`, `details.from` (observed) and `details.state` (drifted actual). That is the same shape the endpoint already produced for an `InvalidTransitionError` advance failure, so it introduces no new inconsistency class. The `#986` same-subject duplicate submission is untouched — it returns before the advance, still `200` with `duplicate_submission=true` and no hooks.
- **Second consumer: the slash-command path** (`issue_approval.go`) is on the same primitive, so no silent-success hole is left behind on a live surface. A state-changed advance there logs at WARN (not ERROR) and replies that the gate was **already decided**, naming the state the stage is now in — a superseded approver's action did not fail. Every other advance error keeps today's ERROR log and generic reply text, and the reply-comment (`silent`) channel stays silent on both.
- **What the CAS does NOT close, deliberately.** Because the anchor is the observed state, a strictly SEQUENTIAL re-approval — a second approver whose request loads the stage AFTER the first advance completed — still compares equal (`succeeded → succeeded`), short-circuits, and re-enters the hook tail. Refusing that would be exactly the 200→409 narrowing the observed-state anchor exists to avoid. It is not the duplicate-filing defect in practice: `fileSplitProposalChildren` no-ops on its prior `split_children_filed` completion marker and `fileOrLinkLiveValidationWalk` no-ops on its `live_validation_walk_intent` marker, so the sequential re-entry files nothing — a claim now pinned by `TestApproveAdvanceCAS_SequentialFreshLoadReapprove_MarkerDedupFilesNothing`, which asserts the hook was RE-ENTERED (a second read of the idempotency guard) and that it filed nothing on either path, rather than leaving the marker-dedup guarantee to this paragraph. The CONCURRENT case is the one those durable-marker guards cannot catch (`fileSplitProposalChildren`'s guard is an unlocked list-then-append) and is what the CAS closes.
- **Pinned by** `approve_advance_cas_test.go`: the concurrent two-approver end-to-end (per-filing-path exactly-once — 3 split children, 1 walk — plus the loser's typed error), the sequential fresh-load re-approval's marker dedup, the stale-observation refusal (error identity AND committed state), the HTTP 409 rendering, the non-CAS degradation, the `#986` duplicate submission (asserted on three gate seams: no further transition attempt, no further run-row read, no second `approval_submitted` row), the deploy leg (a first approve that dispatches exactly once, THEN a raced second that is refused without re-firing, plus its 409 rendering), the untouched reject leg, and the three slash-reply branches.
- **Why the concurrent test rendezvouses TWICE.** Synchronizing only the transition entry leaves the deleted-CAS counterfactual interleaving-dependent: both approvals clear the silent-success transition and then race unsynchronized into the hooks, and a scheduling in which the first writes its `split_children_filed` marker before the second reads it makes the second no-op and the deletion come back GREEN (measured: 3 of 10 runs reddened). So the test also rendezvouses INSIDE `fileSplitProposalChildren`'s list-then-append window, via a gated audit repo that performs the marker read and then blocks — that window IS the race the CAS closes. Departures count toward the release condition, so in the CAS-present world (where only one approval reaches the guard) the refused loser's return releases the winner: no timeout, no wall clock. With both rendezvous the counterfactual reddens on every run, with 6 split-child filings against the expected 3.

## Approval-hook self-protection: one CAS, one subordinate lock (`live_validation_filing.go` / `split_filing.go`, E50.16 / #2657)

The two post-approval hooks in `finishApprovalAdvance`'s tail protect themselves differently, and until #2657 the difference was undocumented. It is now a RECORDED OPERATOR DECISION rather than an accident — and deliberately not dressed up as a principled distinction, because at this layer there isn't one.

| Hook | Durable marker vs forge call | In-process lock |
|---|---|---|
| `fileOrLinkLiveValidationWalk` | intent marker **BEFORE** the forge call (one all-or-nothing filing) | `lockLiveValWalk(runID)`, per-run `sync.Mutex` |
| `fileSplitProposalChildren` | per-phase `work_item_filed` marker **AFTER** each child (N-child resume) | none |

- **The decision: keep the walk's mutex, do NOT add one to split.** Since #2656 the approve advance is a compare-and-swap anchored on the observed state, so a raced second approval is refused at `advanceStage` and never reaches either hook. Two facts make that cover everything reachable: (a) the production `RunRepo` is the postgres one, which implements `run.StageCASTransitioner`, so `casTransitionFromObserved`'s non-CAS degradation is reachable only by in-memory test fakes, not by a deployed daemon; (b) each hook has exactly ONE production call site, both in `finishApprovalAdvance` (`approvals.go:1177` split, `approvals.go:1186` walk — verified by grep at the #2657 head; the remaining callers are the two packages' own tests, which call the hooks directly). So NEITHER mutex is load-bearing in production any more. The walk's is retained because removing a working guard buys nothing; a matching one on split is declined because it would defend a path that cannot occur outside tests.
- **The walk's mutex PREDATES the CAS and is now belt-and-braces, not the primary guard.** It is still a live control where the CAS is bypassed — a direct caller — which is exactly what `TestFileOrLinkLiveValidationWalk_ConcurrentApprovals` is.
- **THE RESIDUAL (the tripwire for the next person).** If the CAS is ever removed, or the `run.StageCASTransitioner` assert in `casTransitionFromObserved` is dropped, BOTH hooks lose their protection — and the SPLIT hook loses it FIRST and SILENTLY, because it has nothing underneath. That is the honest cost of the asymmetry, not a reason the asymmetry is safe.
- **Measured, not asserted (#2657).** With the CAS deleted, the concurrent two-approver case files **3 → 6 split children** (and 2 `split_children_filed` markers, 2 approvals reaching the guard) while the **walk count stays at 1** — its intent/linked markers stay at 1 too, held up by the retained mutex alone. With the CAS present and the `lockLiveValWalk` acquisition deleted instead: `TestApproveAdvanceCAS_ConcurrentApprovals_HooksRunExactlyOncePerFilingPath` is GREEN on **20 of 20** `-race` iterations (the mutex is redundant for concurrency behind the CAS), while `TestFileOrLinkLiveValidationWalk_ConcurrentApprovals` is RED on **19 of 20** (`provider File called 2 times ... want 1`) — interleaving-dependent, as an unsynchronized concurrency counterfactual is, and the same class of flakiness the twice-rendezvous note above exists to remove.
- **Marker ordering is about RESUME, not about self-protection.** The walk is one all-or-nothing filing with nothing to resume, so it burns its marker first and accepts a stranded-intent residual (→ operator files by hand) in exchange for never double-filing. Split files N children and must know which ordinals landed, so its marker records a completed fact and it accepts the at-least-once residual of a child filed whose marker never persisted.
- **A multi-instance deployment replaces BOTH considerations** with a durable cross-process lock (a Postgres advisory lock): an in-process mutex is invisible across processes, so neither hook's in-process story survives horizontal scaling. That is E44 / ADR-057 territory and out of scope here.

## Slash-command approval (#238)

`/fishhawk approve` and `/fishhawk reject` in the triggering issue's conversation submit a gate decision against the run's currently-awaiting-approval stage — closing the loop entirely in the issue surface.

- `webhook.matchIssueComment` parses the three commands (run / approve / reject) and tags the `Match.Action` accordingly; the `Match.CommentBody` carries any trailing reviewer rationale. `Dispatcher.Handle` routes Action ∈ {approve, reject} to `Dispatcher.handleApprovalCommand`, which delegates to the `ApprovalCommandHandler` interface.
- **Implementation lives on `Server.HandleApprovalCommand`** (`issue_approval.go`), where the approval, role, and stage-check repos already live.
  It (1) finds the awaiting-approval stage by listing runs for the repo and matching `trigger_ref = issue:N`, (2) authorizes the channel and the sender via `authorizeCommentApproval` (below), (3) submits via `approval.Repo.Submit` with `Surface = SurfaceGitHubComment`, (4) advances the stage (`advanceStage(ctx, stage, …)` — the same compare-and-swap primitive the HTTP path uses, see above), (5) writes the same `approval_submitted` audit row the HTTP path writes, (6) calls `Orchestrator.Advance`, and (7) posts a reply comment via `Notifier.NotifySlashApprovalReply`.
- Replies are NOT deduped — every command attempt produces its own reply.
- **Authorization: the ADR-123 D4 channel refusal, then the legacy check (#4755).** The comment channel — slash command, reply-comment `+1`, and reaction approvals the `reactionpoller` forwards into the same handler — cannot evaluate a workflow-v2 `approvals` predicate, so `authorizeCommentApproval` FAILS CLOSED, replacing the prior best-effort allow. It re-reads the run (`commentGateSpecMajor`: `GetRun` + `spec.ParseBytes`, needing neither an `installation_id` nor the run's workflow id) and REFUSES, in order: cause `spec_unreadable` (run read error, no cached spec, unparseable spec); `workflow_v2_gate` (spec major `>= 2` — approve AND reject, member or not, whether or not the gate declares `approvals`); `role_resolver_unconfigured` (a v0/v1 gate with no `RoleResolver`); `spec_unreadable` again when the v0/v1 gate cannot be resolved (`fetchGateForStage` errors — a workflow id missing from the spec, or a run with no `installation_id`, which makes every v0/v1 **GitLab** comment approval refuse: accepted by design, the old allow was the same bypass class); `role_resolution_failed` (`role.Resolver.CanApprove` errored, e.g. a team-API flap). Otherwise the legacy v0/v1 `approvers` check is unchanged: a stage with no approvers block admits, a listed member admits, and a non-member gets the existing `not in this stage's approvers list` reply (or, on the reply-comment channel, a silent drop) with NO audit row. A channel refusal records nothing, transitions nothing and captures no delegation shadow; it appends an `approval_comment_refused` entry with payload `{stage_id, decision, surface, approver, silent, reason:"comment_channel_refused", cause, spec_major?}` (`spec_major` omitted for `spec_unreadable`) and, unless silent, replies naming the supported channels (MCP `fishhawk_approve_plan` / `fishhawk_reject_plan`, CLI `fishhawk plan approve|reject`, web UI — all `POST /v0/stages/{id}/approvals`). The reply-comment variant posts no reply but is still audited (`silent=true`); each forwarded reaction draws one reply, since only `reply_comment` is silent. Pinned by `issue_approval_channel_test.go`.
- **Over-cap approve-comment refusal + its breadcrumb (#2583, made observable by E67.24 / #2621).** An approve comment longer than `prompt.MaxApprovalConditionBytes` is refused BEFORE `Submit` (no approval row) on this channel too, because the comment is injected verbatim as a binding approval condition (#558) and must not be silently truncated. A REJECT is never refused — its comment feeds the advisory `PriorRejectionFeedback` channel, not binding conditions. On the reply-comment (`silent=true`) variant the refusal deliberately posts **NO reply**: a passer-by `+1` that happens to exceed the cap should not draw an unsolicited bot comment. BOTH variants append an `approval_comment_refused` audit entry (`CategoryApprovalCommentRefused`, registered in `audit.KnownCategories`) so the drop is visible in the run record rather than invisible — payload `{stage_id, decision, surface, approver, silent, reason:"over_cap_approve_comment", bytes, max_bytes, overflow_bytes}` (the same category as the channel refusal above, told apart by `reason`), stage-anchored, actor `user`. The entry is a trace, not a response: the silent channel stays silent. Best-effort — an append failure WARN-logs and never unwinds the refusal into an admission.
- Per #253 / ADR-017 the slash command no longer reads `stage_check` state — branch protection is the merge gate.
- The slash-command path skips (records nothing) when its deps aren't wired (`approvalCommandConfigured()` returns false), and is best-effort throughout: a comment failure logs but never returns 5xx to GitHub.
- PR-triggered approvals, `/fishhawk cancel`, and per-stage targeting (`/fishhawk approve plan`) are out of scope — separate followups when those scenarios surface.

## Webhook delegate secret channel (`deploy_trigger.go` / `deploy_rollback.go`, E45.57 / #3497)

A webhook deploy delegate may declare `secret_env` (+ `secret_header` | `secret_field`) so the trigger POST is authenticated without the credential ever being in `.fishhawk/workflows.yaml`. Both webhook POST sites — the forward trigger `triggerDeployWebhook` and the rollback re-dispatch `dispatchRollbackWebhook` — go through ONE shared builder/dispatcher, `postWebhookDeploy`, so every property below holds for both.

- **Lookup seam.** `Config.DeploySecretLookup func(name) (value, ok)` resolves the variable NAME from the run's committed spec; nil → `os.LookupEnv` over fishhawkd's OWN process environment (never the runner or MCP host). Tests inject a map by mutating `s.cfg` after `newApprovalServer` (`withSecretLookup`), exactly as they mutate `s.cfg.GitHub`.
- **Placement.** `spec.(*DelegateConfig).SecretPlacement()`: header (`req.Header.Set(secret_header, value)`, default `PRIVATE-TOKEN`) or a top-level JSON body key (`secret_field`). Never both (schema `not` + validator).
- **Refusal modes, each named by variable NAME only.** Unset OR empty value → forward: category-C `deployment_dispatch_failed` with reason `webhook secret env var <NAME> is unset|empty in fishhawkd's environment` and details `{secret_env, url}`; rollback: `502 rollback_dispatch_failed`. Fires BEFORE any request is built — the reachable target records zero requests (`TestTriggerDeploy_Webhook_SecretEnvUnsetOrEmpty_FailsStage`, `TestRollbackDeployment_Webhook_SecretEnvUnset_RefusesWithoutPost`).
- **Redirect refusal.** `deployHTTPClient.CheckRedirect` returns `http.ErrUseLastResponse`, so a 3xx is returned as-is and lands in the non-2xx arm carrying ONLY `{status, url}` — never `resp.Header` (Location), never `resp.Body`. A target reflecting the credential into `Location`, or a 307/308 that would re-send the header/body to another host, can never produce a second request (Go's default would forward a custom `PRIVATE-TOKEN` header cross-host; it strips only `Authorization`/`WWW-Authenticate`/`Cookie`). Pinned empirically by the `redirect_not_followed` arms: a second httptest server behind the 302 records zero requests.
- **Error classification (operator condition 1a).** `CheckRedirect` alone is insufficient: net/http parses the Location header BEFORE consulting `CheckRedirect` and returns `failed to parse Location header %q` embedding the RAW header. So a `deployHTTPClient.Do` error is never propagated verbatim — `classifyWebhookDoError` maps it onto the fixed vocabulary `timeout | dns | connection_refused | tls | redirect_parse | other` (via `errors.As` on `*url.Error` / `net.Error` / `*net.DNSError` / `syscall.ECONNREFUSED` / the `crypto/tls` error types, and the `failed to parse Location header` prefix) and ONLY the class plus the committed `delegate.URL` reach the reason (`webhook POST failed: <class>`), the details (`{error_class, url}`) and the log line (`deploy webhook POST failed`). Pinned by the `malformed_location` arms (`Location: http://[<secret>` → class `redirect_parse`, secret absent everywhere).
- **Redaction (operator condition 1b), belt-and-braces.** `redactSecret(s, value)` replaces the resolved value with `[redacted]` in EVERY string that leaves `postWebhookDeploy` for a sink — reason, each string detail value (`redactSecretDetails`), the error returned upward, the log attrs. A no-op on an empty value.
- **Reserved-key runtime assertion.** Before inserting the secret field, `postWebhookDeploy` asserts every key of the body is in `spec.WebhookReservedBodyKeys` (`fishhawk_run_id`, `fishhawk_stage_id`, `fishhawk_rollback`, `repo`, `workflow_id`, `variables`); a foreign key is a programming error refused BEFORE the POST, naming the key. The validator refuses a `secret_field` IN that set at admission; the assertion keeps the writers and the set in lockstep (`TestWebhookReservedBodyKeys_MatchBuilders` drives both builders and the refusal arm).
- **Body shape.** `webhookTriggerBody` writes the flat correlation keys plus an additive `variables` object (`FISHHAWK_RUN_ID`, `FISHHAWK_STAGE_ID`, `FISHHAWK_REPO`, `FISHHAWK_WORKFLOW_ID`; rollback adds `fishhawk_rollback: true` and `variables.FISHHAWK_ROLLBACK: "true"`, plus `variables.FISHHAWK_ROLLBACK_HANDLE` when the deploy stored a `rollback_handle`, E35.3 / #1600) so a GitLab trigger pipeline targeted directly receives them as CI variables. The `variables` key and the redirect refusal are intentional tightenings that apply to every webhook target (accepted in #3497).
- **Audit payloads carry names only.** `deployment_dispatched` and `deployment_rollback_initiated` gain `secret_env` and `secret_placement` (`header` | `body`) when a secret channel is declared; the value appears in no payload, log or error. The redaction invariant is pinned across 2xx / non-2xx / transport failure / env unset / 302-to-second-server / malformed-Location by `TestTriggerDeploy_Webhook_SecretNeverLeaks` and `TestRollbackDeployment_Webhook_SecretNeverLeaks` (buffer-backed `s.cfg.Logger`, every appended audit row of every category, the returned error, the failure reason, the 502 body).

## Onboarding + session security

### First-run readiness introspection (`onboarding.go`, E29.4)

`backend/internal/server/onboarding.go` — `handleGetOnboardingReadiness` serves `GET /v0/onboarding/readiness?repo=<path>[&forge=github|gitlab]`, aggregating the server-side-only checks a repo's first run needs, consumed by `fishhawk doctor` (E29.5) — five on GitHub, six on GitLab (E45.43 / #3348, "Forge family" below; E45.66 / #3580, "Check (5) on GitLab" below; E45.68 / #3582, "GitLab registration rung" below), plus two deployment-scoped rungs and one hybrid-scoped rung on both families — `trace_store` (E45.75 / #3600, "Trace-store rung" below), `review_grounding` (E45.90 / #3625, "Review-grounding rung" below) and `work_item_provider` (E45.94 / #3646, "Work-item provider rung" below):

1. GitHub App installation via `githubclient.GetRepoInstallation`, reusing the run-create `ErrNotInstalled` classification (`runs.go`) — `probeGitHub`; on GitLab, `installed` = a gitlab installation registered for EXACTLY this project path AND the project resolvable with the deployment credential (#3582) — `probeGitLab` sets `resolvable`, the handler conjoins it with check (6);
2. the committed workflow spec's `spec.ParseBytes` + `spec.Validate` state (`classifySpecBytes`, shared by both families; the bytes come from `GetWorkflowSpec` on GitHub and `forge.FileFetcher.FetchFile` on GitLab);
3. per-reviewer availability via the same `ReviewerSet.For(provider, model, reasoningEffort)` probe `unavailableSpecReviewers` performs (`runs.go`), surfacing the adapter's missing-env hint (`probeReviewers`). Since #3578 each rung also carries the model-id honesty fields: `available` means "the provider is wired AND the resolved model was not authoritatively rejected" (it does NOT assert the model is served when `model_status` is unverifiable); `model_status` is the tri-state `verified | rejected | unverifiable` from `modeloracle.Verify`, computed from the RESOLVED model — the explicit spec value, or the deployment default (recovered via the `reviewerModelDefaulter` seam on the wired set) when the spec omits it — so an omitted default is judged exactly as an explicit one (a `For()` rejection wraps a `modeloracle.RejectedError`; the success-path second read forces `available=false` on a snapshot that flipped to authoritative absence between reads, the #3578 TOCTOU); `model_hint` is the did-you-mean / passed-to-vendor-verbatim / unpriced sentence; and `priced` (`*bool`, nil only when the resolved model id is unavailable) flags an id whose family the pricing table does not know;
4. caller-token scope adequacy against `requiredRunScopes` (the run-drive subset of `operatorDefaultScopes`, `backend/cmd/fishhawkd/token.go`) (`probeScopes`);
5. a forge-shaped merge-gate read: `merge_gate` — whether the forge actually requires the `fishhawk_audit_complete` check Fishhawk publishes (E64.44 / #3161), below, **GitHub family only; OMITTED on GitLab** — and its GitLab-shaped sibling `gitlab_merge_gate` — whether the project's real default branch is protected and the project requires a successful pipeline to merge (E45.66 / #3580), below, **GitLab family only; OMITTED on GitHub**;
6. **GitLab family only; OMITTED on GitHub**: `gitlab_registration` — whether an `installations` row is registered for exactly this project path, the check `POST /v0/runs` refuses `422 gitlab_project_not_registered` on (E45.68 / #3582), below — `probeGitLabRegistration`.
7. **Both families, DEPLOYMENT-scoped, never cascading**: `trace_store` — whether this fishhawkd has a trace store wired (`s3` | `memory` | `none` | `other`), i.e. whether a run's trace upload will 503 after the agent is billed (E45.75 / #3600), below — `traceStoreReadinessFor`.
8. **Both families, DEPLOYMENT-scoped, never cascading**: `review_grounding` — whether this fishhawkd grounds its plan-/implement-review agents against an exported read-only tree or leaves them DIFF-ONLY, plus the per-adapter read bound (E45.90 / #3625), below — `reviewGroundingReadinessFor`.
9. **Both families, HYBRID-scoped, never cascading**: `work_item_provider` — whether the work-item provider this REPO's work-management conventions resolve to is actually REGISTERED on this DEPLOYMENT, i.e. whether a campaign, a `POST /v0/work-items` filing or a grooming apply will respond `501 provider_unimplemented` (E45.94 / #3646), below — `workItemProviderReadinessFor`.

Read-only; cascades gracefully (GitHub: not-installed → spec-unavailable → empty reviewers; GitLab: not-RESOLVABLE → spec-unavailable → empty reviewers — an unregistered-but-resolvable project still gets its spec read). The response's `forge` field names the family that answered.

#### Forge family (E45.43 / [#3348](https://github.com/kuhlman-labs/fishhawk/issues/3348))

Before #3348 the endpoint refused any `repo` with more than one `/` at parameter validation and ran GitHub-only probes, so `fishhawk_doctor` could not answer for a nested GitLab project at all. Two couplings were removed.

**Path shape.** `repo` is validated with `account.ProjectPathWellFormed` — the shape half of `ValidateGitLabProjectPath`, exported so the rule is shared rather than restated: a non-empty namespace, then one or more non-empty components (`a/b`, `a/b/c`, five-segment paths all admitted; `a//b`, `a/ /b`, `a/b/`, ` / ` refused under BOTH families). The GitHub family alone keeps the two-segment rule: a nested path on `github` is 400 `validation_failed` (field `repo`) whose message names `forge=gitlab` as the remedy. The repo is split into `forge.RepoRef{Owner: first segment, Name: remainder}` — `RepoRef.String()` re-joins the full path, which is what the GitLab adapter's `ResolveRepoScope` / `FetchFile` address the project by. The handler `strings.TrimSpace`s `repo` exactly ONCE, immediately after reading the query value and before every consumer: `ProjectPathWellFormed` trims INTERNALLY, so the predicate alone cannot be relied on to keep a padded value from reaching the visibility gate, the `RepoRef` split, both probes and the echoed `repo` field verbatim (#3488).

**Family resolution** (`onboardingForgeFamily`), in order: an explicit `forge` query value (`github` | `gitlab`; anything else is 400 field `forge`, BEFORE visibility); else `cfg.RepoProviders.ResolveProvider(repo)` — the accounts-table discriminator the conventions loader and the repo-visibility gate already key on; not-found, an unknown provider, or no resolver wired → `github` (the byte-identical legacy behaviour); a resolver STORE fault → 503 `service_unavailable`. Consequence, stated: a GitLab project whose namespace is not registered as an account, queried without `forge=gitlab`, is answered as a GitHub repo (`installed:false`, "GitHub App is not installed") — the tool description tells the operator to pass `forge=gitlab` in that case.

**The two-segment rule lands in one of two places, by WHEN the family is known** (approval condition on #3348): with `forge=github` EXPLICIT it fires BEFORE `enforceRepoVisibility`, so an authenticated caller gets the promised 400 ahead of any visibility denial with zero visibility and zero forge calls (`TestOnboardingReadiness_GitHub_NestedPathRejected_BeforeVisibility`); with `forge` omitted the family is only known after registry resolution, which necessarily follows visibility (the resolver is a store read a denied caller must not trigger), so a registry-resolved `github` rejects the nested path AFTER the gate — still with zero forge calls, and a denied caller still sees 403 (`_ForgeFromRegistry_GitHub_NestedPathRejected_AfterVisibility`, both sub-cases).

**What each check became on GitLab** (`probeGitLab`, the forge resolved through `onboardingForgeFor` — the `cfg.ForgeResolver` → `forge.Get` → `isNilForge` ladder `issueOpsFor` uses):

| Check | GitHub | GitLab |
|---|---|---|
| `app` | `GetRepoInstallation`; `installation_id` when installed | `Forge.ResolveRepoScope` with the deployment credential sets `resolvable` (`&true` on success; `&false` on `forge.ErrNotInstalled` (404) with the reason naming the credential, or on any other error with the error as reason + WARN; ABSENT when the forge is unconfigured / typed-nil, reason naming `FISHHAWKD_GITLAB_TOKEN` + `FISHHAWKD_GITLAB_BASE_URL`). `installed` = `gitlab_registration.status == registered && resolvable` (#3582, below); `note` states that on every gitlab report. `installation_id` never set. |
| `spec` | `GetWorkflowSpec` on the installation scope | `forge.FileFetcher.FetchFile` on the resolved scope by the FULL nested path; `ErrNotFound` → unavailable + note; an adapter without file reads → unavailable + note; then the same `classifySpecBytes` |
| `reviewers` | follow the parse | identical |
| `scopes` | always run | identical |
| `merge_gate` | `probeMergeGate` | **OMITTED ENTIRELY** — `MergeGate` is `*mergeGateReadiness` with `omitempty`, nil on this family |
| `gitlab_merge_gate` | **OMITTED ENTIRELY** — `GitLabMergeGate` is `*gitLabMergeGateReadiness` with `omitempty`, nil on this family (`_GitHub_MergeGateKeyPresent` asserts the raw key's absence) | `probeGitLabMergeGate` on the forge + scope `probeGitLab` resolved (E45.66 / #3580, below) |
| `gitlab_registration` | **OMITTED ENTIRELY** — `GitLabRegistration` is `*gitLabRegistrationReadiness` with `omitempty`, nil on this family, and the registry is NEVER consulted (`_GitHub_NoGitLabRegistrationKey` asserts zero registry calls) | `probeGitLabRegistration` on the trimmed path + the resolved scope's ref (E45.68 / #3582, below) |

**Why `merge_gate` is omitted rather than `not_required`.** `mergegate.Reconcile` reads classic branch protection AND rulesets through a `*githubclient.Client`; the GitLab adapter stubs those surfaces (`ListRulesetRequiredChecks` returns `nil, nil`), so no authoritative read exists there, and the field's own invariant says a surface that was never read must never render as "requires nothing". Both client mirrors already model absence as a pointer (#3161), so the omitted key is read as "no claim", the same way a pre-#3161 backend's absence is. `TestOnboardingReadiness_GitLab_NestedPath_EndToEnd` asserts the RAW body carries no `merge_gate` key (the counterfactual vehicle: a zero-valued gate assigned on the gitlab arm turns it red) and, since #3580, that `gitlab_merge_gate` IS present with the by-id + protected-branch reads observed on the resolved project; `_GitHub_MergeGateKeyPresent` guards the pointer change from silently dropping the key on GitHub and asserts `gitlab_merge_gate` is ABSENT there.

Pinned by, in `onboarding_test.go`: the cross-boundary `_GitLab_NestedPath_EndToEnd` (real `forgegitlab.Forge` over an httptest v4 stub that switches on the DECODED path, asserting both calls carried the full five-segment path), `_GitLab_ProjectNotVisible` (zero file reads), `_GitLab_SpecNotFound` / `_SpecInvalid` / `_SpecMalformed`, `_GitLab_ForgeUnconfigured`, `_GitLab_ForgeResolverTypedNil` (no panic), `_ForgeFromRegistry_GitLab` / `_NotFound_DefaultsGitHub` / `_Fault` (503, zero forge calls), `_ForgeParam_Invalid` (400 before visibility), `_ExplicitForgeOverridesRegistry`, and `_MalformedRepo` extended with the empty-component cases under both families. The predicate itself: `account.TestProjectPathWellFormed`.

**Degrade arms (#3488).** Three `probeGitLab` arms not covered above each get their own test: `_GitLab_ForgeWithoutFileReads` (the resolved forge does not implement `forge.FileFetcher`), `_GitLab_ResolveFault` (a `ResolveRepoScope` error that does NOT wrap `forge.ErrNotInstalled`), and `_GitLab_FetchFault` (a `FetchFile` error that does NOT wrap `forge.ErrNotFound`) — each asserting the 200 degraded report, the exact reason/note text and, for the two error arms, the WARN log line.

#### Check (5) on GitLab: the `gitlab_merge_gate` rung (E45.66 / [#3580](https://github.com/kuhlman-labs/fishhawk/issues/3580))

`probeGitLabMergeGate` is the GitLab-shaped sibling of `probeMergeGate`, rendered under its OWN key `gitlab_merge_gate` — never by reusing `merge_gate` — because the two rungs answer DIFFERENT questions and must stay distinguishable. GitLab expresses no per-context required status check, so nothing on GitLab can answer "is `fishhawk_audit_complete` individually required by protection". The rung answers what GitLab CAN answer: is the project's REAL default branch protected, by which rules, with what push/merge access levels and force-push posture, and does the project require a successful head pipeline to merge. The read is one `forge.MergeProtectionReader.ReadMergeProtection(ctx, scope, ref, "")` call (a standalone capability only `forge/gitlab` implements — GET `/projects/:id` FIRST, then GET `/projects/:id/protected_branches` paged to exhaustion, bounded by a fail-closed page cap that refuses a partial rule set rather than truncating; contract in `backend/internal/forge/README.md`) under the same `mergeGateProbeTimeout` child context, on the forge + scope `probeGitLab` resolved (its signature widened to return them).

**Shape.** `status` (`pipeline_gated` | `not_pipeline_gated` | `unknown`), `branch` (the resolved default branch), `protected`, `matched_rules[]` (EVERY rule covering the branch — the exact-name rule first, then wildcard rules in API order, because GitLab applies the MOST PERMISSIVE of all matching rules and a single selected rule is not the effective protection), `allow_force_push` (the OR across matched rules), `push_access_levels[]` / `merge_access_levels[]` (`{level, description}`, the UNION across matched rules deduplicated by level and ascending so the most permissive level is first), `pipeline_must_succeed` (`only_allow_merge_if_pipeline_succeeds`), `allow_skipped_pipeline` and `discussions_must_be_resolved` (informational), `authoritative`, `reason` / `detail` / `remediation`, and a constant `note` stating what the rung answers, that the access levels are the effective union per GitLab's most-permissive semantics, and that `pipeline_gated` is NOT a claim that `fishhawk_audit_complete` individually gates the merge. The three rule-derived signals (`allow_force_push`, `push_access_levels`, `merge_access_levels`) are rendered ONLY when a rule matched — on an authoritative UNPROTECTED read (`protected:false`) they are ABSENT, because there is nothing to OR/union and an unprotected GitLab branch permits force pushes to anyone with push access, so a rendered `false` would read as the opposite (#3591).

**Status decision.** `pipeline_gated` iff `protected && pipeline_must_succeed`; otherwise `not_pipeline_gated` with `detail` naming which signal is off (unprotected default branch and/or pipeline not required) and `remediation` naming the GitLab settings to change. Both verdicts are reachable ONLY from an authoritative read in which BOTH the project settings and the protected-branch list answered.

**Pointer-bool absence rule.** `protected`, `allow_force_push`, `pipeline_must_succeed`, `allow_skipped_pipeline` and `discussions_must_be_resolved` are `*bool` with `omitempty`: on every `unknown` verdict they are ABSENT from the wire, never rendered as `false` — an unread signal must not look like a read one. `assertGitLabMergeGateUnknown` in `onboarding_test.go` asserts each key's absence on every degrade test below. The three rule-derived signals (`allow_force_push`, `push_access_levels`, `merge_access_levels`) are ALSO absent on an AUTHORITATIVE unprotected read (`protected:false`, no rule matched), for the same "a signal with nothing to report is absent, never false" reason — pinned by `_NotProtected_NotPipelineGated` asserting the three keys' absence and by the `_ProtectedForcePushAllowed_SignalsPresent` counterfactual asserting they are present-and-valued when a rule matched (#3591).

| Degrade | `reason` | Test |
|---|---|---|
| No gitlab forge on this deployment (resolver error / typed nil) | `gitlab_forge_unconfigured` | `TestOnboardingReadiness_GitLabMergeGate_ForgeUnconfigured_Unknown` |
| The project did not resolve with the deployment credential (rung (1) `resolvable` false — NOT the stricter `installed`, #3582: an unregistered-but-resolvable project still gets its protection read) — zero forge calls | `project_not_visible` | `_ProjectNotVisible_Unknown` |
| The resolved adapter does not implement `forge.MergeProtectionReader` | `merge_protection_unsupported` | `_Unsupported_Unknown` |
| The project carries no default branch (`forge.ErrNotFound`; an empty repository) — the rule list is never read | `default_branch_unresolved` | `_DefaultBranchEmpty_Unknown` |
| 401/403 on the project read or the protected-branch list (`forge.ErrForbidden`; the list needs at least the Maintainer role) — never `not_pipeline_gated` | `forbidden` | `_ProjectByIDForbidden_Unknown` (list never read), `_ProtectedListForbidden_Unknown` (the fail-closed counterfactual vehicle + WARN log) |
| Any other read failure, including the probe exceeding `mergeGateProbeTimeout` | `transport_error` | `_TransportError_Unknown`, `_ProbeTimeout_Unknown` |

Verdict tests: `_EndToEnd` (HTTP → handler → the REAL `forgegitlab.Forge` → the real `gitlabclient` → the v4 stub → the RAW body: `pipeline_gated`, every signal mapped, the note, NO `merge_gate` key), `_OverlappingRules_Union` (exact `main` + wildcard `m*` → `matched_rules [main, m*]`, merge levels ascending `[30, 40]`, `allow_force_push` OR'd true), `_NotProtected_NotPipelineGated` (also the rule-derived-signal absence assertions), `_ProtectedForcePushAllowed_SignalsPresent` (the absence-guard counterfactual — the three signals present-and-valued on a protected read), `_PipelineNotRequired_NotPipelineGated`.

**Residual, stated plainly.** GitLab has no per-context required check, so `pipeline_gated` verifies that the default branch is protected and the head pipeline must succeed — the closest analogue to a required check, on the assumption that the `fishhawk_audit_complete` commit status Fishhawk posts participates in the head pipeline's status as an external job — and NOT that any named status individually gates the merge; approval rules (Premium) are not read. The operator confirms both by hand (`docs/deploy/gitlab.md`). The read is point-in-time and REPORTING only. The `gitlab_merge_gate` key is OPTIONAL on the wire (absent from the OpenAPI `required` list) and the MCP mirror models it as a pointer (`backend/internal/mcpserver/client.go`), so a github-family report and a pre-#3580 fishhawkd both decode to absence, never a zero-valued object.

#### GitLab registration rung (E45.68 / [#3582](https://github.com/kuhlman-labs/fishhawk/issues/3582))

Before #3582 the gitlab family's `app.installed` meant "the project resolves with the deployment credential" — a credential-visibility fact — while the authorization a run actually needs on GitLab is an `installations` row registered for the exact project path (`fishhawkd installation register`), which `POST /v0/runs` refuses `422 gitlab_project_not_registered` without. So `installed:true` over-claimed: a resolvable-but-unregistered project reported ready and then could not start a run. Two changes.

**`app.installed` is redefined on gitlab** to mean "the Fishhawk-specific authorization a run needs exists", the same thing it means on github: `installed = gitlab_registration.status == registered && app.resolvable`, computed in the handler's gitlab arm AFTER both probes. The weaker fact moves to its own explicit `app.resolvable` (`*bool`, gitlab only): `&true` when `ResolveRepoScope` succeeded, `&false` on `forge.ErrNotInstalled` or any resolve fault, nil (ABSENT on the wire) when the gitlab forge is unconfigured or typed-nil — the forge was never read, and absence must never look like `false`. `probeGitLab` NEVER sets `Installed` any more; `app.reason` keeps the forge/resolve reasons (set first, they win) and otherwise gets `onboardingGitLabNotRegisteredReason` (pointing at `gitlab_registration.remediation`) or the registry-unknown reason naming the rung's `reason` code; `app.note` (`onboardingGitLabInstalledNote`) is set on every gitlab report and says what installed means. The not-visible reason (`onboardingGitLabProjectNotVisible`) no longer names the register command — registration is a different precondition from visibility, and the two are now reported separately. **The `spec` and `gitlab_merge_gate` cascades key on `resolvable`, not on `installed`** (`probeGitLabMergeGate`'s parameter is renamed `resolvable`; its `project_not_visible` degrade is unchanged in semantics), so a resolvable-but-unregistered project still gets its spec fetched + parsed and its protection read — pinned by `_GitLab_Registration_NotRegistered` asserting `spec.source == fetched` and `gitlab_merge_gate.status == pipeline_gated`.

**The rung** (`probeGitLabRegistration(ctx, repo, resolvedRef)`, rendered under `gitlab_registration`, a pointer nil on github and set on EVERY gitlab-family report — including forge-unconfigured, where the registry can still answer): `status` (`registered` | `not_registered` | `unknown`), `project_path` + `installation_ref` (the registered row's, absent unless registered), `resolved_ref` (the `gitlab:<project_id>` ref `ResolveRepoScope` produced — `forge/gitlab` builds it from the numeric project id, the same format `--installation-ref` takes; a fact about the PATH, not the row, so it is present on EVERY posture whenever the project resolved — `registered`, `not_registered` and BOTH `unknown` degrades — and absent ONLY when the path did not resolve, E45.72 / [#3595](https://github.com/kuhlman-labs/fishhawk/issues/3595); it is assigned in the initial struct literal for exactly that reason, so a degrade that returns before reading the registry still reports it), `ref_matches` (`*bool`, set ONLY when both refs are known — so a present `resolved_ref` never implies a comparison), `reason` (non-empty whenever `unknown`), `detail`, `remediation`, and the constant `note` (`gitLabRegistrationNote`).

**The invariant, and why it holds by construction.** `registered` is computed through the SAME `cfg.GitLabInstallations.ResolveGitLabProject` seam, with the SAME single-trimmed `repo` and the SAME exact-path / ambiguous-is-not-found posture, as `resolveCreateGitLabInstallation` (`runs.go`) — so `gitlab_registration.status == registered` is exactly "POST /v0/runs will pass its registry check for this path". `TestOnboardingReadiness_GitLab_RegistrationAgreesWithRunCreate` pins it end to end: ONE `Server` with ONE `fakeGitLabInstallations`, GET readiness then `POST /v0/runs` for the same path over {registered, not found, lookup error, unwired}, asserting registered ⇔ 201 and each non-registered posture's create refusal matches the rung's classification (`not_registered` ⇔ 422 `gitlab_project_not_registered`; `registry_unwired` ⇔ 503 `gitlab_unconfigured`; `registry_lookup_failed` ⇔ 500 `internal_error`), and that BOTH handlers handed the seam the byte-identical path (a rung that canonicalised the path before the lookup goes red).

| Posture | `status` / `reason` | `app.installed` | Test |
|---|---|---|---|
| Row registered for the exact path, refs equal | `registered`, `ref_matches: true` | true (when resolvable) | `_GitLab_NestedPath_EndToEnd` |
| Registry answered found=false (no row, or an ambiguous double registration) | `not_registered`; `remediation` = the copy-pasteable `fishhawkd installation register --provider gitlab --account-key <namespace> --installation-ref <ref> --project-path <path>` command with the REAL resolved `gitlab:<id>` when the path resolved, the `gitlab:<project_id>` placeholder otherwise | false, reason → `gitlab_registration.remediation` | `_GitLab_Registration_NotRegistered`, `_NoteAndUnresolvedRefAbsent` |
| `cfg.GitLabInstallations == nil` (no database) | `unknown` / `registry_unwired`; `resolved_ref` CARRIED when the path resolved (`ref_matches` absent — never compared) | false, reason naming the code | `_GitLab_Registration_RegistryUnwired`, `_NoteAndUnresolvedRefAbsent` |
| `ResolveGitLabProject` errored (WARN log) | `unknown` / `registry_lookup_failed`; `resolved_ref` CARRIED when the path resolved (`ref_matches` absent — never compared) | false, reason naming the code | `_GitLab_Registration_LookupFailed`, `_NoteAndUnresolvedRefAbsent` |
| Row registered but `installation_ref` ≠ `resolved_ref` | `registered`, `ref_matches: false`, `detail` naming BOTH refs, re-register `remediation` | **true** — run-create ACCEPTS the row (it never resolves the path); the rung surfaces that a run would act on the registered project | `_GitLab_Registration_RefMismatch` |
| Row registered with an EMPTY `installation_ref`, path resolved | `registered`, `installation_ref` absent (omitempty), `resolved_ref` present, `ref_matches` ABSENT — an empty ref is never a mismatch | true | `_GitLab_Registration_EmptyInstallationRef_RefMatchesAbsent` |
| Project did not resolve (not visible / forge unconfigured) | whatever the registry says; `resolved_ref` + `ref_matches` ABSENT (never resolved, never compared) | false, the visibility/forge reason wins | `_GitLab_ProjectNotVisible`, `_GitLab_ForgeUnconfigured`, `_ForgeResolverTypedNil`, `_NoteAndUnresolvedRefAbsent` |
| github family | key ABSENT, registry never consulted, `app.resolvable` absent | App semantics unchanged | `_GitHub_NoGitLabRegistrationKey` |

**Fail-closed, like the merge-gate rungs.** `unknown` is reachable ONLY when the registry could not answer (unwired / lookup fault), always with a naming `reason`; a registry that answered found=false is the POSITIVE `not_registered`. `ref_matches` is absent whenever either ref is unknown, so an unresolved project never renders a false match and a `registered` row whose `installation_ref` is EMPTY renders `ref_matches` ABSENT, never `false` — an empty ref is unknown territory for the comparison, not a mismatch (`_GitLab_Registration_EmptyInstallationRef_RefMatchesAbsent` pins that second disjunct of the guard, E45.72 / [#3595](https://github.com/kuhlman-labs/fishhawk/issues/3595)). Because `resolved_ref` is now carried on the two `unknown` degrades as well, a present `resolved_ref` is NOT evidence that a comparison happened — read `ref_matches` for that.

**What is NOT checked, stated plainly.** The rung reports the `installations` row for the exact path — the run-create check. It does NOT check the `account_key` binding the webhook receiver additionally enforces (`gitlab_webhook.go`), nor that the credential the row implies can push; the constant `note` says so on every report. The read is point-in-time and REPORTING only: it gates no run. The CLI `fishhawk doctor` is GitHub-only by construction and keeps its byte-compatible `installed` decode; the MCP `fishhawk_doctor` mirror (`backend/internal/mcpserver/client.go`) carries `app.resolvable` and the unexported `onboardingGitLabRegistration`, both pointers so absence stays absence (`TestOnboardingReadinessReport_GitLabRegistrationMirrorsBackendTags` decodes an all-fields-populated literal body so a tag typo on either side cannot pass on zero values; `_ResolvableFalseSurvivesReemit` pins why `resolvable` is a pointer).

#### Check (5): the `merge_gate` rung (E64.44 / [#3161](https://github.com/kuhlman-labs/fishhawk/issues/3161))

`probeMergeGate` reconciles the check Fishhawk **publishes** against the branch protection the forge **enforces**. Fishhawk posts `fishhawk_audit_complete` on every run's PR; nothing in Fishhawk makes it a required status check, and until #3161 nothing read the forge to find out whether anything else had. On this repository the check was unrequired for the whole dogfood period while four operator-facing surfaces asserted it was required.

Why it matters concretely: `backend/internal/server/merge_run.go` has no server-side implement-review gate — `resolveReviewStageOnMerge` settles the review stage ON the merge — and `backend/internal/auditcomplete` rule 6 is explicitly the pre-merge presence gate whose ONLY enforcement is the published Check Run. With the check unrequired, `fishhawk_merge_run` queues a GitHub auto-merge that fires with the review verdict still pending. That is why an absent requirement is reported as a **warning**, not as information.

**Shape.** The rung resolves the repo's REAL default branch via `mergeGateRepository` (never a hardcoded `main`) and hands it to `mergegate.Reconcile` with `auditcheckpublisher.CheckName`, under a `mergeGateProbeTimeout` (10s) child context. The response carries `status`, `check`, `branch`, `required_contexts`, `sources[]` (each with its `identity`, `bypass_entries` and, for classic protection, `classic` + `enforce_admins`), `bypassable`, `reason` and `remediation`.

**Fail-closed to `unknown`, never to `not_required`.** Every degrade named below resolves to `status: "unknown"` with a `reason` naming it. `not_required` is reachable only from a fully authoritative evaluation in which both protection surfaces answered definitively — the vacuous-green discipline `required_checks_capture.go` documents (#2497 / #2506): a surface that was never read must never be reported as a positive "this repo requires nothing".

| Degrade | `reason` | Test |
|---|---|---|
| No GitHub client on this deployment | `github_client_unconfigured` | `TestOnboardingReadiness_MergeGate_NoGitHubClient_Unknown` |
| The App is not installed on the repo (both protection reads need an installation token) | `app_not_installed` | `TestOnboardingReadiness_MergeGate_AppNotInstalled_Unknown` |
| The repo's default branch could not be resolved, or resolved empty / nil metadata | `default_branch_unresolved` | `TestOnboardingReadiness_MergeGate_DefaultBranchLookupFails_Unknown`, `_NilRepositoryMetadata_Unknown` |
| `Reconcile` rejected the call itself (nil api / empty branch / empty check) | `probe_failed` | `TestOnboardingReadiness_MergeGate_ReconcileRejects_Unknown` |
| The probe exceeded `mergeGateProbeTimeout` | inherited from the reconciler | `TestOnboardingReadiness_MergeGate_ProbeTimeout_Unknown` |
| Every forge-side degrade (rulesets 404, un-evaluatable `ref_name` token, 403 for a missing `administration: read`, transport error) | `mergegate`'s own reasons | `_RulesetsNotFound_Unknown`, `_UnevaluatableRefName_Unknown`, `_ForbiddenAdministrationRead_Unknown`, `_TransportError_Unknown`; contract in `backend/internal/mergegate/README.md` |

**Bypass posture rides along, per source, as a conjunction.** A required check anyone can bypass is not the gate it looks like, so `sources[]` carries each requiring source's own bypass condition and `bypassable` is the AND over them — one source with no bypass path still enforces the check regardless of what the others allow. A ruleset `bypass_actors` entry is a role, team, app or integration covering an unknown number of people, so it is reported as a count of *entries*, never as a count of actors; classic `enforce_admins: false` is its own named condition ("repository admins are exempt"), never coerced into a count of 1. Full contract: `backend/internal/mergegate/README.md`.

**Residuals, stated:**

- **The read is point-in-time.** A ruleset edited after the probe is not reflected. The rung is REPORTING only — it gates no run, no merge, and no exit code (`warn` does not move `fishhawk doctor`'s exit status; only `fail` does), so an operator who deliberately opts out is informed rather than blocked. GitHub remains the merge-time authority.
- **`merge_gate` is OPTIONAL on the wire** — deliberately absent from the OpenAPI `required` list, and BOTH client mirrors model that absence as a pointer so a newer client against a pre-#3161 fishhawkd degrades to silence rather than a bogus verdict: the CLI (`cli/cmd/fishhawk/doctor_onboarding.go`) emits no rung at all, and the MCP mirror (`backend/internal/mcpserver/client.go`, `merge_gate,omitempty`) re-emits no key at all. The symmetry is the point — a value field on either side would decode the missing key into a zero-valued object whose `status` is `""`, a verdict outside the `required|not_required|unknown` enum that no forge read established. Absence ("this backend cannot answer") is a distinct claim from `unknown` ("the question was asked and could not be settled"), and neither surface renders it as the other.
- **Org-level rulesets are only partially covered.** The client lists repo rulesets with `includes_parents=true`, which surfaces inherited org rulesets, but an org ruleset scoped by repository-property conditions is not evaluated by the v0 matcher and lands as non-authoritative → `unknown`.

**Gate ordering is load-bearing: 401 anonymous → 400 malformed forge/repo (+ the explicit-`forge=github` nested-path 400) → `enforceRepoVisibility` (#1512, ADR-057 Amendment A2 / #2071) → family resolution (+ the registry-resolved nested-path 400).** It is NOT a write-scope gate — scope adequacy is a reported field — but it IS a repo read-visibility gate **for a non-admin cookie-session caller**: that identity class is the ONLY one that can receive the 403 — the three unfiltered classes below (bearer/MCP tokens, workspace admins INCLUDING admin cookie sessions, no-mirror deployments) keep the exact pre-change surface and never see it. Anonymous is rejected before any filter resolves (an unauthenticated caller must not learn a repo exists); the `repo` query value is validated to a well-formed `owner/name` before the filter is handed it; only then does the visibility gate run, so a denied non-admin cookie session reaches ZERO forge calls, ZERO spec fetches, and receives no `spec.Error` text. The gate reuses `enforceRepoVisibility` rather than hand-rolling a filter, so the endpoint inherits the whole #2071 point-read contract: 403 `repo_forbidden` on a deny of a non-admin cookie session, 503 `service_unavailable` on a mirror-store / provider-resolution / role-resolution fault (the store-fault class is kept DISTINCT from the permission-denied class), and the cross-forge / ambiguous-row / prefixless-subject fail-closed denies.

Three identity classes are UNFILTERED, each preserving the exact pre-change surface:

- **Bearer/MCP token identities** (`TokenID != ""`) — `repoFilterFor` returns nil; bounded by token ownership and scope, so the mirror (keyed on a human forge subject) has nothing to say about them. This is what keeps `fishhawk doctor` / the `fishhawk_doctor` MCP tool working.
- **Workspace admins** — `RoleAdmin` bypasses the filter, INCLUDING admin cookie sessions. The 403 is a NON-ADMIN qualification: an admin cookie session never sees it.
- **No-mirror deployments** — `Config.RepoVisibility == nil` is `repoFilterFor`'s first early return.

Pinned by, in `onboarding_test.go`: `TestOnboardingReadiness_RepoNotVisible` (the 403 + zero-forge control and counterfactual vehicle), `_RepoVisible` (admission control), `_BearerTokenUnfiltered`, `_AdminCookieBypass`, `_NoMirrorWired`, `_VisibilityStoreFault`, `_RoleResolutionFault`, `_ProviderResolutionFault`, `_CrossForgeDeny`, `_AmbiguousRowForgeDeny`, `_PrefixlessSubjectDenyAll`, `_AnonymousBeforeVisibility` (documents the observed handler-level posture — 401 `authentication_required` + zero mirror calls for anonymous — NOT a strict hoist counterfactual: `repoFilterFor` ALSO short-circuits anonymous callers with a nil filter, so a hoisted guard would still not reach the mirror fault), `_MalformedRepoBeforeVisibility`; and the cross-layer arms in `repovisibility_integration_test.go` (`TestRepoVisibility_Integration_MemberSeesOnlyGrantedRepo` / `_AdminSeesEverything`) that drive the endpoint through the real router, session middleware and Postgres-backed mirror.

#### Trace-store rung (E45.75 / [#3600](https://github.com/kuhlman-labs/fishhawk/issues/3600))

`trace_store` answers "will this run's trace upload 503?" BEFORE an agent is dispatched and billed, instead of after. `POST /v0/runs/{id}/trace` responds 503 when `cfg.TraceStore` is nil, and that only surfaces after the agent has run — the documented local dogfood loop hit exactly this.

- **Deployment-scoped, never cascades.** `traceStoreReadinessFor(s.cfg.TraceStore)` is resolved in `handleGetOnboardingReadiness` BEFORE the forge switch, so every report of BOTH families carries it — a not-installed repo, an unresolvable GitLab project and an unavailable spec included. It is a fact about the deployment, not the repo.
- **Four kinds, resolved by concrete type** (no `tracestore.Storage` interface change): nil (or a typed-nil concrete store) → `configured:false`, `kind:"none"`, a `note` naming the 503-after-billing consequence and a `remediation` naming `FISHHAWKD_S3_BUCKET`, the `.env.example` trace-storage block and `make s3-init`; `*tracestore.MemStorage` → `kind:"memory"` with a `note` that the in-memory store (`--dev-fixtures` or `--dev-trace-store`, E45.76 / #3601) is EPHEMERAL (lost on restart — the E45.76 trap); `*tracestore.S3Storage` → `kind:"s3"`; any other implementation → `kind:"other"`, never mislabelled `s3`.
- **Pointer on the wire.** `TraceStore *traceStoreReadiness` with `omitempty`; this handler never leaves it nil, but both client mirrors (`fishhawk_doctor` in `backend/internal/mcpserver`, the CLI `fishhawk doctor`) model it as a pointer so an older fishhawkd's absent key renders NO rung — absence is "cannot answer", not `configured:false`.

Pinned by, in `onboarding_test.go`: `TestTraceStoreReadinessFor` (every branch including the typed-nil and third-implementation cases, asserting kind + configured + branch note/remediation), the no-cascade seams `_TraceStore_NotInstalledStillCarriesRung` / `_TraceStore_SpecUnavailableStillCarriesRung` / `_TraceStore_GitLabForgeUnconfiguredCarriesRung` (raw served JSON), and the cross-boundary `_TraceStore_SurvivesMCPMirrorDecode` (the real handler's served body decoded with `mcpserver.OnboardingReadinessReport`).

#### Review-grounding rung (E45.90 / [#3625](https://github.com/kuhlman-labs/fishhawk/issues/3625))

`review_grounding` makes the dormant grounding posture DISCOVERABLE without changing it. Reviews are DIFF-ONLY by default — `FISHHAWKD_REVIEW_GROUNDING` defaults false (#2522) — so a reviewer that cannot settle a diff-invisible question downgrades it to UNTRACED / UNESTABLISHED and the operator inherits the check. Nothing previously told the operator that a switch existed.

- **Deployment-scoped, never cascades.** `reviewGroundingReadinessFor(s.cfg.ReviewGroundingDisabled)` is resolved in `handleGetOnboardingReadiness` BEFORE the forge switch, alongside `trace_store`, so every report of BOTH families carries it — a not-installed repo, an unresolvable GitLab project and an unavailable spec included. It is a fact about fishhawkd's wiring, not the repo.
- **The default is UNCHANGED.** This rung is a readout, not a policy change: `backend/cmd/fishhawkd/serve.go` is untouched and grounding still ships off. `enabled:false` is the SUPPORTED posture, so the CLI renders it at status `ok` with a hint and the doctor's aggregate outcome and exit code do not move — a `warn` would nag every correctly-configured deployment.
- **The adapter table is STATIC and its two bounds are NOT equivalent.** `adapters[]` renders in BOTH postures, because it is precisely what an operator deciding whether to opt IN needs. `bound` is a closed two-value vocabulary: `confined` (codex — the synthesized `confined` permission profile, an OS-level deny-by-default ALLOWLIST; an out-of-tree read returns EPERM) and `blocklist` (claude — a bounded `--disallowed-tools` deny-rule list over credential roots, enforced at the TOOL layer: defence-in-depth, and NEVER labelled confinement here). It restates the posture `backend/internal/reviewsandbox/confine.go` shipped; the backend does NOT spawn a reviewer to measure confinement at readiness time. The honest-label asymmetry is the one `backend/internal/reviewsandbox/README.md` § "Reviewer read bounds (#2522)" forbids collapsing, and it is asserted rather than left to prose.
- **Pointer on the wire.** `ReviewGrounding *reviewGroundingReadiness` with `omitempty`; this handler never leaves it nil, but both client mirrors model it as a pointer so an older fishhawkd's absent key renders NO rung — absence is "cannot answer", not `enabled:false`.

Pinned by, in `onboarding_test.go`: `TestReviewGroundingReadinessFor` (both postures, asserting `enabled`, the EXACT two-row adapter vocabulary and the branch note/remediation text including the `FISHHAWKD_REVIEW_GROUNDING` literal), `TestReviewGroundingReadiness_HonestLabelInvariant` (the claude row is a `blocklist` and its note makes no confinement claim, in BOTH postures), the no-cascade seams `_ReviewGrounding_NotInstalledStillCarriesRung` / `_ReviewGrounding_GitLabForgeUnconfiguredCarriesRung` (raw served JSON), and the cross-boundary `_ReviewGrounding_SurvivesMCPMirrorDecode`.

#### Work-item provider rung (E45.94 / [#3646](https://github.com/kuhlman-labs/fishhawk/issues/3646))

`work_item_provider` answers, AHEAD of the first call, the two facts a `501 provider_unimplemented` already prints after it: which work-item provider this repo's conventions RESOLVE to, and which providers are REGISTERED on this deployment. Before it, a deployment on which every campaign, every `fishhawk_file_issue` and the whole backlog-grooming loop was impossible reported all-green.

- **HYBRID-scoped — the one rung that is neither purely deployment- nor purely repo-scoped.** `provider` comes from `conventionsLoader(ctx, repo)` (the same seam `POST /v0/campaigns` and `POST /v0/work-items` resolve through, `workitems.go`), a REPO fact; `registered` comes from `workmgmt.Registered()`, populated at startup by `registerWorkmgmtProviders` gated per configured client, a DEPLOYMENT fact. `status` is their INTERSECTION. It is nevertheless resolved BEFORE the forge switch, alongside `trace_store` and `review_grounding`, so no repo-scoped cascade (not-installed app, unresolvable GitLab project, unavailable spec) can suppress it.
- **The conventions read is cheap.** `RepoConventionsLoader` is TTL-cached per `(provider, repo)` (5 minutes) and falls back to `workmgmt.Default()` whenever the repo commits no `.fishhawk/work-management.yaml`; a repo whose App is not installed resolves no credential scope and takes the fallback with NO fetch at all. The handler already performs several forge reads, so this sits inside the endpoint's existing cost envelope.
- **Three statuses, one of them a FAILURE.** `registered` — a filing call will dispatch. `unregistered` — it will not: `note` states the 501 consequence and `missing_hint` names the remedy, and the CLI renders this at status `fail`, so it MOVES the doctor's aggregate outcome and exit code. That is deliberate and is the issue's explicit intent ("should read as a failure, not as data"); it is the behavioural consequence of this change, and an operator on a deployment that never had a work-item provider wired will start seeing a non-zero `fishhawk doctor` exit.
- **`unknown` is FAIL-CLOSED and comes FIRST.** A conventions-resolution error yields `unknown` with the closed-set `reason` `conventions_unresolved` — the error's TEXT is never echoed (a conventions load can carry forge transport detail, matching the `merge_gate` / `gitlab_registration` reason discipline). The branch is evaluated BEFORE the membership comparison precisely because falling through with an empty provider id would render `unregistered`: a positive finding about a repo whose conventions were never read. It is NOT evidence the provider is missing, and the CLI renders it `warn`, never a pass.
- **`registered` is ALWAYS an array**, possibly empty, and is populated even on `unknown`: the deployment registry answers when the repo's conventions do not. An EMPTY registry is a distinguished sub-case of `unregistered` — the hint says no provider is wired at ALL rather than naming one provider's env vars.
- **`campaign_sources` (#3658) is ALWAYS an array too** — the campaign sources the resolved provider serves, in the fixed order `epic_ref`, `items`, computed by `campaignSourcesSupported` (compile-time type assertions, the same list the campaign refusals carry as `campaign_sources_supported`). The HANDLER resolves the provider instance (`workmgmt.Get`) and passes the list into the still-pure `workItemProviderReadinessFor`; the resolver emits it ONLY on `registered` and `[]` on `unregistered` / `unknown`, which make NO capability claim. A `registered` rung with `[]` (a File-only provider such as `jira`) means `POST /v0/campaigns` refuses `501` in either mode; `gitlab` reports both since #3658. Pinned by `TestWorkItemProviderReadinessFor`'s `campaign_sources` arms and the served-body `TestOnboardingReadiness_WorkItemProvider_CampaignSourcesShipped` (real gitlab provider, File-only fake, unregistered, unknown).
- **The hints name REAL startup gates, or none.** `workItemProviderMissingHint` maps `github_projects` → `FISHHAWKD_GITHUB_APP_ID` + `FISHHAWKD_GITHUB_APP_PRIVATE_KEY_FILE`, `gitlab` → `FISHHAWKD_GITLAB_BASE_URL` + `FISHHAWKD_GITLAB_TOKEN`, `jira` → `FISHHAWKD_JIRA_BASE_URL` + `FISHHAWKD_JIRA_EMAIL` + `FISHHAWKD_JIRA_API_TOKEN` (verified against `backend/cmd/fishhawkd/serve.go`'s flag table), each with the startup-registration contract: a credential set after boot changes nothing until fishhawkd RESTARTS. An id outside that set gets a generic hint naming the id, the registered set and the `provider:` conventions key, and FABRICATES no env var.
- **Pointer on the wire.** `WorkItemProvider *workItemProviderReadiness` with `omitempty`; this handler never leaves it nil, but both client mirrors model it as a pointer so an older fishhawkd's absent key renders NO rung. That matters more here than for the other rungs: this is the one that can FAIL the command, so a zero-valued decode would fail `fishhawk doctor` against a backend that made no claim.

Pinned by, in `onboarding_test.go`: `TestWorkItemProviderReadinessFor` (a table over every branch including both empty-registry sub-cases and an unrecognised id), `TestWorkItemProviderReadinessFor_UnknownIsNotUnregistered` (the fail-closed identity control, also asserting the resolution error's text and credential are never echoed), `TestWorkItemProviderMissingHint_NamesRealEnvVars` (the done-means test: each provider's SHIPPED hint names its actual startup env vars, and an unrecognised id fabricates none), the no-cascade seams `_WorkItemProvider_NotInstalledStillCarriesRung` / `_SpecUnavailableStillCarriesRung`, the handler-level `_WorkItemProvider_UnknownOnConventionsError` and `_RegisteredWhenConventionsMatch`, the end-to-end `_WorkItemProvider_UnregisteredEndToEnd` (raw served bytes; also the counterfactual vehicle for the unregistered classification), and the cross-boundary `_WorkItemProvider_SurvivesMCPMirrorDecode`. Tests that mutate the process-global `workmgmt` registry or the `conventionsLoader` seam are non-parallel and restore what they replaced; the registry exposes `Register` but no `Deregister`, so the fake registers under an id no production provider uses and the assertions are on MEMBERSHIP, never on removal.

### CSRF enforcement (`csrf.go`, ADR-005)

`backend/internal/server/csrf.go` ships the double-submit pattern per ADR-005.

- The OAuth callback (`server.handleGitHubCallback`) mints a 32-byte hex token and sets it in the `__Host-csrf` cookie alongside `fishhawk_session`; logout clears both.
- The `csrf` middleware sits after `bearerAuth` in the chain (`recovery → requestID → logging → bearerAuth → csrf → mux`) and enforces `X-CSRF-Token` ≡ `__Host-csrf` on POST/PUT/PATCH/DELETE for session-cookie identities only.
  Bearer-token clients (CLI, server-to-server) and GET-style methods bypass; safe-listed paths (`/v0/auth/github/*`, `/webhooks/github`) bypass too.
- ONE path-scoped exception: on `POST /v0/oauth/authorize` with a form-encoded body and no header, the middleware verifies the consent form's `csrf_token` field as a session-bound signed token (`consentcsrf.go`, #2442) instead of comparing it to the cookie — see "CSRF on `/v0/oauth/authorize`" below. The header path is unchanged on that route too.
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
| Membership resolution error on an escalated group | `predicateUnavailable` → 503 `forge_unavailable` (`retryable: true`) | n/a |
| No configured identity provider for the run's forge (E45.49 / #3466) | `predicateUnconfigured` → 503 `forge_unavailable` (`reason: identity_provider_unconfigured`, `retryable: false`, no `approval_predicate_rejected` audit entry) | escalated count fails closed (ok=false, gate not advanced) |
| Gate `approvals` block unreadable while an escalation IS firing | retryable 503 `escalation_unevaluable` (`reason: baseline_unreadable`) | n/a |
| Run row unreadable at the predicate site (E45.58 / #3502) | retryable 503 `forge_unavailable` (`reason: run_row_unreadable`) | escalated count fails closed (ok=false, gate not advanced) |

The last row is #2374, and its reason is that an escalation raises **relative to** the baseline: composing a firing escalation against a `nil` baseline drops the baseline's own `member_of` / `min_permission`, so a `member_of` baseline under a COUNT-ONLY escalation would compose to "the raised count, no membership at all" and admit an out-of-group approver — a shape the count-time forge re-validation does not cover either, since a count-only escalation carries no escalated forge predicate to re-resolve. The refusal is gated strictly on a firing (or unevaluable) escalation: a fetch error with NOTHING escalated keeps the pre-existing baseline fail-**open** read unchanged. It applies at BOTH approval points — `checkApprovalPredicates` returns the 503 pre-Submit, and `approveStageAs` (reached directly by the campaign auto-driver, which has no pre-Submit gate) RECORDS the approval but does NOT advance, the same posture its post-Submit resolver-error branch takes.

**Enforcement at BOTH approval points.** The raise is applied at the pre-Submit 403 (`checkApprovalPredicates`) *and* at the quorum count (`approveStageAs`), for the #2358 reason `not:` had to be: raising only the 403 would record an approval the gate then declines to count, wedging it one short. Both read the same immutable cached spec bytes on the same run row within one request. `effectiveApprovals` takes max / strictest / union per dimension, so **runtime lowering is structurally impossible** even for a spec that bypassed declaration-time validation.

**Audit posture (operator-ratified, #2361 asymmetry).** The `escalation_fired` append is **best-effort, not a precondition of enforcement**: an escalation firing moves in the SAFE direction (the gate gets stricter), so failing the gate because a log line could not be written would convert an audit-store outage into a total governance outage. The rule: *a REFUSAL or a RAISE is best-effort — the safe outcome already happened; a GRANT (the `applies_to` override) is a precondition — it records an exception being MADE.* De-duplication is read-then-append on `(run, stage, fingerprint)`, which handles the common SEQUENTIAL case; it is **not** atomic, so two concurrent evaluations can both append. A duplicate governance-chain entry is hygiene, not a control gap (the #2366 class) — there is no per-`(run, stage)` uniqueness guarantee. A de-duplication READ failure emits anyway (fail toward visibility).

Surfaced on `GET /v0/runs/{run_id}` as the `escalations` block (single-run read only, omitted when the workflow declares none or nothing fired), rendered through the same `escalation.RenderFired` helper as the audit payload so the two cannot drift.

**`require.reviewers` is NOT a composed requirement (E55.9 / [#3754](https://github.com/kuhlman-labs/fishhawk/issues/3754)).** `spec.ComposedRequirements` deliberately has no reviewers dimension: its `IsZero` drives the approval gate's fetch-error fail-closed branch, the `snapshot.Escalated` stamp, the 503 refusal and the 403 `escalated` flag, so a reviewers-only escalation must stay "nothing raised" on the approval/delegation seams. Its effect is review-time only (see "Reviewer personas" → escalation-attached personas). Emission is keyed on the FIRED set, not on the requirement: `resolveEscalations` calls `writeEscalationFiredAudit` whenever `res.Any()`, so a FIRING reviewers-only escalation still writes ONE `escalation_fired` entry at these seams — its `summary` names the firing with NO `Raised:` clause and the payload carries no `required_*` / `max_autonomy` field — while the composed requirement stays zero (the gate is neither blocked nor marked escalated). Pinned by `TestResolveEscalations_ReviewersOnlyFiringAuditsWithoutRaise`.

**Two coordinates name a firing (E75.1 / [#3729](https://github.com/kuhlman-labs/fishhawk/issues/3729)).** The `escalation_fired` payload carries `fired` — the POSITIONAL declaration index, the coordinate spec *validation* errors report at (`/workflows/<name>/escalations/<i>`), so it is what an operator uses to find the rule in the document in front of them — and, since #3729, a parallel `fired_keys` array of `escalation.RuleKey` values: the STABLE content-derived join key a decision index follows a rule by ACROSS document edits. Reordering unrelated declarations moves the index but not the key; editing THIS rule's own globs, labels, triggers or `require` clamp changes the key. The two arrays are index-aligned by construction (built in one loop over the fired set). `fired_keys` is ADDITIVE — `fired`, `summary` and the result-level `fingerprint` are untouched, so the audit de-duplication behaviour is unchanged, and `RuleKey` is deliberately NOT `Fingerprint` (result-level de-duplication key vs rule-level content key).

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
[mcpserver README](../../mcpserver/README.md).

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
| 8 | lifted off loopback for OAuth, bare `fhk_`/`fhm_` token (no validated audience) | `401 authentication_required` (byte-identical challenge) |
| 9 | **POST only** — JSON-RPC batch body | `403 mcp_batch_not_supported` |
| 10 | **POST only** — body unclassifiable (unreadable, or no non-whitespace byte) | `400 mcp_request_unreadable` (`details.reason` = `read_error` \| `no_content`) |
| 11 | **POST only** — `Mcp-Protocol-Version` absent or `< 2026-07-28` | `403 mcp_standard_headers_required` |
| 12 | **POST only** — `Mcp-Method` absent | `403 mcp_standard_headers_required` |
| 13 | **POST only** — `Mcp-Method` is not `tools/call` | pass through to the streamable handler |
| 14 | **POST only** — `tools/call` with no `Mcp-Name` | `403 mcp_tool_name_required` |
| 15 | **POST only** — the named tool has no `mcpToolScopes` entry | `403 mcp_tool_not_authorized` (`details.tool`) |
| 16 | **POST only** — the identity does not satisfy the tool's rule | `403 insufficient_scope` (`details.required_scope`, `details.tool`) |
| — | otherwise | delegate to the streamable handler |

### Per-tool authorization at the HTTP layer (`mcpscopes.go`, E66.30 / #2459)

Rungs 9–16 are `authorizeMCPToolCall`, which runs on **POST only** (GET and
DELETE carry no JSON-RPC message and must keep reaching the SDK for its
spec-prescribed `405 Allow: POST`) and — the load-bearing placement — runs
BEFORE `mcpHandler.ServeHTTP`, so a refusal precedes the per-request
`MCPServerFactory`: no tool registry is built and no inner REST call is dialled.
`TestHandleMCP_RefusalPrecedesServerFactory` and
`TestMCPRoute_ScopeRefusalIsCrossLayer` assert exactly that (a factory-call
counter of zero, and zero recorded inner `/v0/` requests), because a status code
alone cannot distinguish a gate refusal from one raised inside an
already-constructed registry.

**The 2026-07-28 client floor is a DELIBERATE NARROWING, and it is forced, not
chosen.** go-sdk v1.7.0 `mcp/streamable_headers.go` `validateMcpHeaders` returns
`nil` immediately when `Mcp-Protocol-Version` is empty or lexically below
`minVersionForStandardHeaders` (`"2026-07-28"`), so BELOW that version the SDK
validates NO header against the body and `Mcp-Method` / `Mcp-Name` are
unauthenticated hints. AT or ABOVE it, a missing or disagreeing header is
answered `-32020 CodeHeaderMismatch` + HTTP 400 — which is what makes a
header-derived authorization decision sound, and is what
`TestMCPRoute_ForgedMcpNameRejected` demonstrates end to end. Enforcing only
when the headers HAPPEN to be present would be a complete bypass, since a
header-less `tools/call` is indistinguishable at the header layer from a
header-less `initialize`. **Symptom of the narrowing:** a pre-2026-07-28 MCP
client's first request carries no version header, so it draws `403
mcp_standard_headers_required` and never connects. Any client already pointed at
a deployment's `/mcp` must be confirmed to negotiate 2026-07-28 before this
ships.

**Batching is UNSUPPORTED on this route.** The SDK validates header/body
agreement only for a SINGLE non-batch message — `mcp/streamable.go` guards the
`validateMcpHeaders` call with `if !isBatch && len(incoming) == 1` — so a
batch's `Mcp-Name` is checked against none of its elements and the request
cannot be authorized from its headers at all. The gate detects a batch from the
first non-whitespace byte of a BOUNDED body prefix (`mcpBodyProbeLimit`),
restores everything it read so the SDK still decodes the whole body, and refuses
with its own identity.

**`Stateless: true` is a silent precondition.** The streamable transport accepts
protocol 2026-07-28 only on a stateless server, so flipping this route to
stateful would negotiate down to 2025-11-25, strip the standard headers, and
make this gate refuse EVERY request. Recorded as a comment on both
`newMCPHandler` and the gate.

**The table lives HERE, not beside the registrations.** `mcpToolScopes` maps
every tool `mcpserver.NewServer` registers onto the scope its underlying REST
endpoint enforces. It cannot live in `backend/internal/mcpserver` for the same
reason `MCPServerFactory` is an injected seam: that import edge closes a cycle
in `mcpserver`'s TEST binary. The REVERSE edge is legal and already in use
(`mcproute_test.go` imports `mcpserver` for `TestMCPRoute_ToolRegistryParity`),
which is what makes `TestMCPToolScopeTable_CoversRegistry` possible — it asserts
SET EQUALITY IN BOTH DIRECTIONS against the live registry, so a tool added
without an entry AND a stale entry are each RED.

**Derivation rule, per entry:** find the REST endpoint the tool dials and mirror
its predicate. `requireWriteScope(w, r, X)` → `anyOf{X}`; `retry.go` /
`revive.go`'s `write:stages OR write:retries` and `fixup.go` /
`waive.go` / `defer_concern.go`'s `write:stages OR write:fixups` → both members;
a handler enforcing nothing beyond authentication → the named sentinel
`mcpScopeAuthenticatedOnly`. Where a tool dials several endpoints the entry
mirrors the one performing its PRIMARY action, any-of across them when it
performs several — the gate must never be STRICTER than the endpoints, because
each inner REST call still enforces its own predicate. An unmapped name fails
CLOSED.

**The `fhm_` interaction is the sharpest hazard.** A run-bound MCP token
(subject `mcp:run:<uuid>`, minted by `handleIssueMCPToken`) carries only
`mcp:read`, plus `write:retries` when the stage's spec sets
`executor.agent_self_retry`, plus `write:scope-amendments` on implement stages.
An entry demanding an operator-vocabulary scope for a tool the in-run agent
calls would break the agent loop with no compile error — mapping
`fishhawk_retry_stage` to `write:stages` alone would refuse a self-retrying
implement agent `retry.go` admits today. Two shapes guard it: any-of rules, and
`runBoundSubjectOK` for handlers that authorize a run-bound token by its SUBJECT
rather than by scope (`gateview.go`, `product_report.go`).
`TestMCPToolScopeTable_RunBoundAgentLoopIntact` pins it.

`TestMCPToolScopeTable_VocabularyIsNotParallel` additionally pins every `anyOf`
member to `oauthas.SupportedScopes`, the three run-bound `fhm_` scopes, or an
existing named constant, so no parallel MCP-only scope language can appear.

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
(identical to the AS metadata's — both publish `oauthas.DefaultScopes`, the
advertised posture, NOT the whole mintable vocabulary; #2477),
`bearer_methods_supported: ["header"]`,
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

### CSRF on `/v0/oauth/authorize`: the session-bound form token (#2442)

The `POST` consent decision is CSRF-protected, but NOT by the `__Host-csrf`
cookie double-submit every other route uses: the consent page embeds a
**stateless, session-bound signed `csrf_token`** in its hidden form field, and
the `csrf` middleware's ONE path-scoped form branch verifies that token instead
of comparing it to the cookie. This route neither reads nor writes
`__Host-csrf`.

**Why the shared cookie collided.** The `__Host-csrf` cookie is
`SameSite=Strict`, so it is **NOT sent on the cross-site top-level navigation**
that lands the browser on `GET /v0/oauth/authorize` from the MCP client's site
(the `SameSite=Lax` session cookie IS). The #2436 design therefore minted a
fresh cookie on every consent render — and because the mint branch always
fired, every render OVERWROTE the single host-wide cookie slot. Two concurrent
authorization flows (two MCP clients, or one client retried in a second tab)
rendered two pages but the browser held only the SECOND cookie, so the first
page's form 403'd `csrf_required` on submit. The signed token removes the shared
slot: every rendered page carries its own independently-verifiable value, N
concurrent consent pages all submit, the SPA's cookie is never clobbered by an
authorize navigation, and SameSite is out of the picture.

**Token format** (`consentcsrf.go`):
`base64url( ts[8] || nonce[16] || HMAC-SHA256(key, label || ts || nonce) )`,
`ts` = big-endian unix seconds of the mint, `label` =
`fishhawk-consent-csrf-v1`. This is the OWASP HMAC-based synchronizer-token
pattern, keyed **per session** — `key = SHA-256(label || 0x00 ||
fishhawk_session-cookie-plaintext)` — rather than per server secret, because
`Config` carries no signing secret and a process-random key would break across
the Helm chart's replicas and across a `fishhawkd` restart mid-consent. The key
derivation is DOMAIN-SEPARATED from `auth.HashPlaintext` (the unlabelled
SHA-256 persisted in the sessions table) by the label + `0x00` separator, so a
sessions-table read never yields the CSRF key
(`TestConsentCSRFKey_IsNotThePersistedSessionHash`). A forger needs the
HttpOnly session cookie plaintext to compute a valid token — the same secret the
double-submit ultimately protected — and cannot read the consent HTML
cross-origin; HMAC output does not reveal its key (RFC 2104 §6), so embedding
the MAC in the page exposes nothing. Both `renderConsent` and the middleware
read the raw cookie from the request rather than carrying the plaintext on
`Identity` (context-visible; must not carry a credential); `renderConsent` runs
only under `id.SessionID != ""`, which `bearerAuth` sets exclusively from that
cookie, and an empty plaintext still fails closed `500 internal_error`
(`TestOAuthAuthorize_ConsentRenderWithoutSessionCookieFailsClosed`).

**TTL.** `consentCSRFTokenTTL = 1h`, checked against `s.nowFunc()` at request
time: long enough to read the page, short enough to bound replay of a leaked
one. The boundary is pinned (`ts+TTL` passes, `ts+TTL+1s` refused).

**Three refusal modes**, each `403 csrf_required` at the middleware with a
form-specific message: `ErrConsentCSRFMalformed` (not base64url, or not exactly
56 bytes), `ErrConsentCSRFExpired`, `ErrConsentCSRFInvalid` (MAC does not verify
under THIS session's key — tampered, or minted for another session). The
comparison is constant-time. The OLD protocol — a cookie value in the form field
with the matching cookie attached — is refused too: the form branch is
signature-only and never falls back to the cookie mirror
(`TestCSRF_ConsentCookieValueInFormRefused`).

**The header path is unchanged for every route including the consent POST**:
with an `X-CSRF-Token` header present the middleware never enters the form
branch and compares header ≡ `__Host-csrf` exactly as before
(`TestCSRF_HeaderPathUnchanged`). The anonymous bypass (`id.SessionID == ""` →
next) precedes the form branch, so an anonymous consent POST reaches the handler
and is `401 auth_required`, never `403`
(`TestCSRF_AnonymousConsentPostIs401NotCSRF`).

**Tests** (all through `srv.Handler()`, crossing middleware + handler +
template): `TestCSRF_ConcurrentConsentFlows_FirstFormSucceeds` (the #2442
obligation — two GETs, the FIRST form still submits; RED under the pre-change
code), `TestCSRF_ConsentAcceptsFormFieldFallback` (form token, NO cookie on the
POST → 302), `TestCSRF_ConsentTokenTamperedRefused`,
`TestCSRF_ConsentTokenNotBoundToSessionRefused`,
`TestCSRF_ConsentTokenExpiredRefused`, `TestCSRF_ConsentMissingFormFieldRefused`,
`TestCSRF_FormFallbackIsPathScoped`, and the done-means for the clobber removal
`TestOAuthAuthorize_ConsentGETSetsNoCSRFCookie` (200, non-empty `csrf_token`, NO
`Set-Cookie __Host-csrf`). The codec is unit-pinned with `errors.Is` identity in
`consentcsrf_test.go`.

**General lesson, recorded because it defeated handler-level testing:** a test
suite that constructs cookies programmatically cannot observe `SameSite` at
all — `net/http` does not replay the browser's cross-site send/suppress rule —
so a handler test that hand-attaches the `__Host-csrf` cookie passes while a
real browser sends no such cookie on the cross-site navigation. The stack tests
above model the cross-site arrival by attaching NO `__Host-csrf` cookie; the
browser-level check stays with the operator walk (#2439).

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

### Token lifecycle: 15-minute access tokens, refresh rotation, operator revocation (E66.5 / #2393)

| Credential | Default lifetime | Knob |
|---|---|---|
| authorization code | `60s` | `--oauth-code-ttl` |
| access token | **`15m`** (`defaultOAuthAccessTokenTTL`) | `--oauth-access-token-ttl` |
| refresh token | `336h` (14 days, unchanged) | `--oauth-refresh-token-ttl` |

The access token is deliberately SHORT-LIVED: a session rides the refresh grant,
so a leaked bearer is useful for minutes rather than an hour. `expires_in` is
stamped per token at mint time from the row (`writeTokenResponse`), never read
from config at validation time — so a TTL change affects only tokens minted
after it, and a revert leaves already-minted tokens on their own expiry.
`resolveOAuthASState` applies the default only when the configured TTL is zero
or negative; an operator who pinned `1h` explicitly is unaffected.

**Rotation and reuse detection already live in the store.** `grant_type=
refresh_token` calls `oauthstore.RotateRefreshToken`, which consumes the
presented token, mints a successor pair chained via `replaced_by_id`, and — on a
REPLAYED (already-rotated) token — revokes the WHOLE lineage inside the same
transaction, committed before `ErrRefreshReused` returns. Nothing on that path
moved in #2393. A client that refreshes proactively derives its margin from the
credential's own lifetime (`credstore.RefreshSkew`: 2 minutes at the 15-minute
default), and `oauthttlskew_test.go` pins the server default and that derived
skew TOGETHER, so a future TTL change that would leave the margin thinner than a
refresh round-trip fails in-loop rather than in production.

**Operator revocation is a `fishhawkd` verb, not an endpoint.** `fishhawkd
oauth token revoke --subject <s> [--client-id <c>]` calls
`oauthstore.RevokeGrantsForSubject`: one transaction, refresh tokens first (the
credential that can mint more), every lineage root for the subject locked `FOR
UPDATE` in the order rotation already uses, so a rotation racing the sweep
cannot slip a successor past it. The RFC 7009 `/revoke` endpoint is
**deliberately NOT shipped** in this slice — it would be a new public,
unauthenticated AS route, and the operator refused that scope; it is tracked as
an operator-filed follow-up. Contract and tests: `backend/internal/oauthstore/README.md`.

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
calls `oauthas.ResolveRequestedScope(req.scope, registeredScopeSet(client), registeredScopeAuthority(client))`: a
request that CARRIES NO SCOPE TOKEN (the key absent, or a present-but-empty
`scope=` — `url.Values.Get` cannot tell them apart and they are deliberately
equivalent) is granted the client's registered scope INTERSECTED with the server
vocabulary, or `oauthas.DefaultScopes` when the registration pins nothing; a
registration naming no supported scope fails CLOSED with `invalid_scope`. Every
PRESENT value still validates exactly as before — an unknown scope, a
whitespace-only `scope=%20%20`, and a scope exceeding the registration all still
fail `invalid_scope`. Step 7 is NOT skipped for a defaulted set: it passes by
construction, so one unconditional restriction ladder beats a conditional one a
later edit could get wrong. Contract detail: `backend/internal/oauthas/README.md`.

**The default is `DefaultScopes`, NOT the whole vocabulary (#2477 / #2471).**
This is the ratified scope posture, and the rationale is the whole point: the
least-effort path for a first-time MCP client must not be authority to approve
gates and ship. Before this, the PRM and AS metadata advertised all eight
operator scopes, so a client that echoed `scopes_supported` back verbatim — or
omitted `scope` entirely, taking the #2466 default — was granted `write:deploy`.
`oauthas.DefaultScopes` is `SupportedScopes` minus `write:deploy`, and it is now
what BOTH advertised surfaces publish and what the no-registration default hands
out.

The VOCABULARY is untouched: nothing is deleted or renamed, `ParseScope` still
validates against `SupportedScopes`, and no operator token changes —
`fishhawkd token issue` still mints `write:deploy`.

`write:deploy` is additionally refused on the ordinary authorization-request path
(`oauthas.PreRegistrationOnlyScopes`, enforced at the single
`ResolveRequestedScope` chokepoint so it covers both the explicit and the
defaulted route): a request naming it is `invalid_scope` unless an
OPERATOR-AUTHORED registration PINS it, and a MIXED request naming it alongside
valid scopes is refused WHOLE rather than silently stripped — stripping would
grant less than the consent page displayed. The operator write path for such a
registration has SHIPPED: `fishhawkd oauth client register --scope
"... write:deploy"` (#2438), which `resolveOAuthClient` prefers over a CIMD fetch.

**Only the STORE branch is an operator act, and the resolver says which branch it
was.** `resolveOAuthClient` falls through to the client's own CIMD document on a
store miss, and that document's `scope` member is unvalidated client-authored
input — so honouring it would let a client with no store row declare
`write:deploy` about itself and satisfy the very bound the scope sits behind, on
the supported connection path. It does not: `resolvedOAuthClient.ScopeAuthority`
carries the provenance (`resolvedFromStore` →
`oauthas.OperatorAuthoredRegistration`, `resolvedFromCIMD` →
`oauthas.ClientAuthoredRegistration`, the untrusted ZERO value), the ladder passes
it to the chokepoint, and only the operator value unlocks the scope. A
client-authored pin is refused WHOLE on the explicit path and DROPPED from the
defaulted one (a defaulted request named nothing, so there is no asked-for set to
diverge from; an intersection emptied that way still fails closed). Driven
adversarially end to end by `TestAuthorize_CIMDClientCannotSelfPinWriteDeploy` and
`TestOAuthFlow_CIMDSelfPinnedWriteDeployNeverReachesAToken`, each with a
discrimination arm proving the SAME scope string from a store row still grants.
Residual: anything able to WRITE an `oauth_clients` row counts as an operator, so
a future RFC 7591 dynamic-registration endpoint would have to be excluded
explicitly. Full contract and the forward rule for adding a scope to the bound:
`backend/internal/oauthas/README.md`.

Step 7's registered-scope restriction is **unchanged** by #2477 and still bounds
BOTH the explicit and the defaulted set.

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

Operator write path for pre-registration, now SHIPPED (E66.21 / #2438):
`fishhawkd oauth client register|list|remove` is the operator surface that calls
`oauthstore.UpsertClient` — the documented path for a Codex-style client that
hosts no CIMD document (#2394). Registration-free CIMD clients remain the
default; this verb is for the offline/no-CIMD case, and `resolveOAuthClient`
prefers a pre-registered row over a CIMD fetch. Contract:
`backend/cmd/fishhawkd/README.md`.

Still recorded, not acted on:

- **No negative cache for failed `client_id` validations.** Caching a TRANSIENT
  failure would pin a briefly-unreachable legitimate CIMD host as invalid for the
  whole TTL; the limiter already bounds repeated bad ids. Filed as its own design
  question rather than folded in here.

## Federated identity providers (E66.4 / #2392)

`fishhawkd` stays the **sole** OAuth authorization server and clients keep
seeing one issuer; what becomes forge-agnostic is which federated provider
authenticates the human behind a token.

### The enumeration, and why NoOp is never in it

`Config.IdentityProviders` (forge-keyed) + `Config.IdentityDeviceClientIDs`
(the NON-Confidential DEVICE application's client_id per forge) +
`Config.IdentityBaseURLs` (the instance root the CLI appends
`/oauth/authorize_device` and `/oauth/token` to) carry the multi-provider
config. `New` SEEDS the `github` entry from the legacy single
`IdentityProvider` + `OAuthClientID` fields.

`configuredIdentityProviders()` is the **single** filtered enumeration every
caller goes through — discovery, the mint's provider selection, and anything
added later. **No handler may range over `cfg.IdentityProviders` directly.**
It excludes two shapes:

1. Anything `identity.IsConfigured` reports false for — a nil interface, a
   typed-nil concrete provider, or `*identity.NoOpIdentityProvider`. The NoOp
   is installed *because* no forge is configured, so enumerating it would
   advertise a provider that cannot authenticate anyone.
2. An entry with an empty DEVICE client id. Discovery exists to tell the CLI
   which id to drive the device flow with; a provider it cannot drive is not
   advertised.

**The approvals gate is the one keyed lookup, not a range (E45.49 / #3466).**
`resolvePredicates` (`quorum.go`) resolves `min_permission` / `member_of`
PER RUN FORGE — the run row's `InstallationRef` → `observationForgeID`
(`github` for nil/empty, so every pre-#3466 run and GitHub-seeded fixture is
unchanged) keys `cfg.IdentityProviders` through `predicateIdentityProvider`,
which applies exclusion 1 (`identity.IsConfigured`) but deliberately NOT
exclusion 2: quorum needs only the members/permission reads, so a provider
with no device leg must still resolve predicates. Before #3466 the resolver
read the GitHub-only `cfg.IdentityProvider` singleton, so a GitLab run's
approver was evaluated against the NoOp default's CLEAN deny and refused
with a misleading `403 approver_predicate_unmet`. Now a forge with no
configured provider yields the fourth outcome `predicateUnconfigured` →
`503 forge_unavailable` with `details.reason: identity_provider_unconfigured`,
`details.forge`, `retryable: false` and a forge-specific env-var hint in
`next_actions`; nothing was evaluated, so NO `approval_predicate_rejected`
entry is written. Precedence: the `min_permission` empty-repo and
unparseable-tier `predicateUnavailable` checks run BEFORE the provider lookup
and shadow it. `countEscalatedForgeApprovers` derives the same forge and
treats unconfigured as not-countable. Residual: the repo ACL
(`serve.go::resolveRepoVisibility`) still reads the singular field.

**A failed run-row read is refused, not defaulted to github (E45.58 / #3502).**
Both call sites read the run row to derive `InstallationRef` before deriving
the forge. Before #3502 a FAILED read (as opposed to a successful read of a
nil/empty ref) was silently swallowed the same way as the nil-ref case,
defaulting `forge` to `github` — so a transient store hiccup on a GitLab run
could resolve a `member_of`-only gate against the GitHub provider, the same
misdiagnosis class as the unconfigured-provider defect above. Now a failed
read is refused BEFORE `observationForgeID` is called at all:
`checkApprovalPredicates` returns retryable `503 forge_unavailable` with
`details.reason: run_row_unreadable` and `details.error`, and
`countEscalatedForgeApprovers` returns `(0, false)`. Neither site special-cases
`run.ErrNotFound`: the predicate-site read is the THIRD read of the same run
row within one request (after `fetchApprovalsForStage` and
`resolveStageEscalations`), so any error reaching it is transient by
construction.

Two **deliberately redundant** layers keep a NoOp out of the map, and each is
observed by its own test:

| Layer | What it does | What it is pinned by |
|---|---|---|
| **Ordering** | `New` seeds the maps STRICTLY BEFORE the `IdentityProvider == nil → NewNoOp()` default and only when the field is non-nil, so the NoOp is not visible to the seeder | `TestNew_RawIdentityProvidersMap_NeverContainsNoOp` reads the RAW map |
| **Type exclusion** | `configuredIdentityProviders` drops a `*NoOpIdentityProvider` whatever put it there | `TestNew_EmptyIdentityProvidersMap_ExcludesNoOp`'s explicitly-wired NoOp case |

The two layers are **not** interchangeable, and the filtered enumeration alone
cannot tell them apart: with the type check intact, moving the seeding after
the default keeps `TestNew_NilIdentityProvidersMap_ExcludesNoOp` GREEN (run and
observed — that was the plan's originally-claimed counterfactual, and it is
unattainable). The raw-map test is the observation that isolates ordering.

`configuredIdentityProviderNames()` orders github, then gitlab, then any other
name sorted — a deterministic order the discovery response and its legacy
first-entry mirror depend on.

### `GET /v0/tokens/login` discovery

Returns `{provider, client_id, providers: [{provider, client_id, base_url?}]}`.
Each entry's `client_id` is the **DEVICE** client id, not the Confidential
browser-leg id. `base_url` is omitted for github (the CLI knows github.com's
device endpoints) and is REQUIRED for gitlab, which may be SaaS or any
self-managed host. The legacy top-level `provider`/`client_id` mirror the
FIRST entry, so a CLI predating the array keeps working byte-compatibly.
Zero configured providers → `503 tokens_unconfigured`.

### `POST /v0/tokens/login` mint

The posted `provider` selects which configured provider re-verifies the token,
and the **resolved** provider — not a constant — is what `IssueOAuth` stamps on
the row. An omitted `provider` defaults to the **SOLE** configured entry when
exactly one is configured (so a GitLab-only deployment is drivable by an old
CLI rather than locked out), and to `github` otherwise. An unconfigured
provider is refused `400` naming the configured set. A verified subject whose
`identity.ProviderOf` disagrees with the selected provider is refused `401`
rather than minted — defence in depth against a mis-stamped row.

**Client binding (#4627).** `VerifyAccessToken` binds the token to the
deployment's OWN device OAuth client before resolving the subject (GitHub
check-a-token, GitLab token-info; contract in
`backend/internal/identity/README.md` § "Client binding"), so a token the same
human obtained through any other client cannot mint. Outcomes:

- every refusal (foreign or undeterminable issuer, failed introspection,
  failed subject lookup) is ONE generic `401 token_verification_failed`; the
  response never says which check failed.
- a provider missing its introspection credential
  (`identity.ErrClientCredentialUnconfigured`) is `503 tokens_unconfigured`,
  `details.provider` set, the message naming the env var:
  `FISHHAWKD_OAUTH_CLIENT_SECRET` (with `FISHHAWKD_OAUTH_CLIENT_ID`) for github,
  `FISHHAWKD_GITLAB_DEVICE_CLIENT_ID` for gitlab, a generic wording otherwise.

**Refusal logging is a closed set (#4627 C1).** A refused verification writes
one WARN line, `token-login verification refused`, carrying ONLY `provider` and
`reason`, one of the `tokenLoginReason*` constants in `tokens.go`, classified
by sentinel (`errors.Is`), never by text:

| `reason` | Sentinel |
|---|---|
| `rate_limited` | `identity.ErrRateLimited` (either request) |
| `issuer_mismatch` | `identity.ErrTokenClientMismatch` |
| `introspection_refused` | `identity.ErrIntrospectionRefused` |
| `introspection_failed` | `identity.ErrIntrospectionFailed` |
| `subject_resolution_failed` | anything else (the subject lookup) |

The error text never reaches the log, the response body or an audit row: a
provider error can embed up to 256 bytes of a forge response body, and a forge
body can echo the submitted access token. Pinned by
`TestTokenLoginMint_VerificationErrorNeverLeaksUpstreamBody`
(`tokens_clientbinding_test.go`), which reads all three sinks.

### `POST /v0/tokens` mint ceiling

`handleCreateToken` (#4503) mints for the caller's OWN subject and refuses, in
order and each before anything is issued (no token row, no `api_token_issued`
audit row): `401 auth_required` (anonymous) → `403
first_officer_cannot_mint_tokens` (seat) → `403 run_bound_cannot_mint_tokens`
(any `mcp:run:` subject) → `403 scope_escalation_refused` (a requested scope
outside the ceiling; `details.excess_scopes` names every excess scope,
de-duplicated, request order; `details.ceiling` names the ceiling applied).

- **Ceiling.** A bearer (`Identity.TokenID` set: `fhk_`, `fho_`, a clamped
  seat) is bounded by its own resolved `Identity.Scopes` (`token_scopes`), so
  an `fho_` token cannot widen its AS grant. A cookie session (`TokenID`
  empty) carries no scope list and no role-to-scope mapping exists, so it is
  bounded by `Config.OperatorDefaultScopes` (`operator_default_scopes`), the
  set `POST /v0/tokens/login` mints within for the same humans; an unwired set
  admits no explicit scope. An empty `scopes` list is a subset of any ceiling.
- **Run-bound refusal.** bearerAuth resolves an `fhm_` agent token to
  `mcp:run:<run_id>`, and the route has no wrapper, so before #4503 an in-run
  agent could mint itself an untenanted `fhk_` token with operator scopes.
  `isRunBoundSubject` matches the PREFIX, not a parseable UUID, so a
  malformed `mcp:run:` subject self-minted under the old hole is refused too.
  It precedes the ceiling: the code is `run_bound_cannot_mint_tokens` even for
  a subset of the agent's own scopes.
- **Tenancy (#4506).** The minted token inherits the caller's workspace
  account. A tenanted caller (`Identity.AccountID` set, from a tenanted
  bearer's row or a cookie session's resolved account) mints through
  `apitoken.Repository.IssueForAccount`, so the row's `account_id` is the
  caller's account and the minted bearer resolves to that account's view;
  an untenanted caller (operator token, accountless session) mints through
  `Issue`, an untenanted row. A repository failure on the tenanted path is
  `500 internal_error` with no row. `POST /v0/tokens/login` (`IssueOAuth`)
  and `fishhawkd token issue` stay untenanted by design; the posture and a
  read-only inventory query for pre-existing untenanted rows are in
  [`../apitoken/README.md`](../../apitoken/README.md). Counterfactual vehicles:
  `TestCreateToken_TenantedBearerMintStampsAccount`,
  `TestCreateToken_TenantedCookieMintStampsAccount` and
  `TestCreateToken_UntenantedMintStaysUntenanted` (`tokens_tenancy_test.go`,
  the real token repository on Postgres, committed `account_id` read back by
  SQL), plus `TestCreateToken_TenantedIssueError` (`tokens_test.go`).
- **DELETE stays scope-free.** `DELETE /v0/tokens/{token_id}` requires no
  scope and does not refuse a run-bound subject: revocation only removes
  privilege, and `Repository.Revoke`'s ownership check confines it to the
  caller's own tokens, so a token minted under the old hole can revoke itself.
- **Counterfactual tests** (`tokens_test.go`): `TestCreateToken_ScopeEscalationRefused`
  (ceiling), `TestCreateToken_RunBoundRefused` (run-bound refusal, a real
  `fhm_` token through bearerAuth), `TestCreateToken_CookieSessionCeiling`
  (cookie ceiling). Each asserts committed state (token rows, audit rows)
  after the call; `TestRevokeToken_RunBoundSubject` pins the DELETE decision.

### Forge-aware federated sign-in

`signInForges()` lists the configured BROWSER sign-in legs (`GitHubOAuth` /
`GitLabOAuth` non-nil). It is **independent** of the device-flow enumeration
above: the browser leg is gated on the Confidential application's OAuth trio,
the device leg on a NON-Confidential application's client id alone, and reading
one from the other would render a sign-in link to a 503 route.

- zero → the pre-existing `503 oauth_unconfigured`
- exactly one → a 302 to `/v0/auth/<forge>/login?next=…`, byte-identical to
  the previous unconditional GitHub redirect on a GitHub-only deployment
- two → a minimal `html/template` provider-choice page: one **link** per forge,
  each carrying the same `next=` return target. There is no form and no hidden
  field; the page is a pure navigation choice.

### Session subject carries the user's provider

`sessionSubject` builds the cookie-session subject as `<provider>:<login>`.
Before #2392 it was hardcoded `"github:" + login` regardless of
`user.Provider`, so a GitLab browser sign-in (live since E44.22 / #2109) minted
a `github:`-prefixed subject the GitLab provider could never resolve. An EMPTY
`Provider` falls back to `github`: rows predating the column read as `""`, and
that fallback is what keeps a GitHub-only deployment byte-identical.

Consumers were enumerated rather than assumed, and none hard-fails: the
`repoacl` mirror's cache key takes a MISS that re-resolves live;
`account.Store.MemberRole` is REPAIRED (a GitLab user currently keys their
grant under `provider="github"`); `actorKindForSubject` keys only on the
operator-agent prefix; the approval snapshot identity is likewise repaired; the
`mcp:run:` prefix branches are unaffected; and no migration constrains a
subject column's format. Two consequences are named rather than glossed:
`repoacl.Mirror` holds a SINGLE resolver and uses provider only as a cache key,
so GitLab-owned rows now reach a GitHub resolver, error, and are absorbed into
`(false, nil)` plus a WARN — fail-closed, a visibility gap not a security one,
and provider-ROUTING the mirror is follow-on work; and `ApproverSubject` is a
dedup JOIN key, so on a deployment that already had GitLab sign-in live a
user's pre-change `github:` vote and post-change `gitlab:` vote occupy two
approver slots.

## Membership-gate denial page (`accessdenied.go`, E44.31 / [#2467](https://github.com/kuhlman-labs/fishhawk/issues/2467))

`handleForgeCallback` denies a sign-in on two named branches and 302s the
browser at a **relative** target. That target resolves against the callback's
**own origin** — fishhawkd — so in the standard split-origin layout (Vite on
`:5173` proxying only `/v0` to fishhawkd on `:8080`) it never reaches the SPA
page added by #1827. fishhawkd registered no such route, so the operator landed
on a bare `ServeMux` 404 with no explanation. (The companion `GET /favicon.ico
404` in the issue's log is the tell that the Go mux, not Vite, answered.)

`GET /access-denied` makes that target resolve and explain itself.

### The two reason codes

| `reason` | Branch | What the page says |
|---|---|---|
| `no_membership_resolver` | `Config.AuthMembership == nil` | The login gate has no membership resolver wired, so EVERY sign-in is denied. A **deployment-configuration** fault, not a per-user one; the remedy is wiring the database and the workspace profile. |
| `no_admitting_account` | `len(accountIDs) == 0` | No workspace account admits this login — no invited membership, no auto-join match. Remedies: `FISHHAWKD_SINGLE_TENANT_ACCOUNT_KEY`, or an admin invite. |

On the `no_admitting_account` branch the callback ALSO logs
`oauth sign-in denied: no admitting account` at Info naming, beside the
authenticated `login`, the **configured** single-tenant profile
(`configured_provider` / `configured_account_key` / `configured_granularity`),
read from `Config.SingleTenantProfile` (#2468). That field is threaded from the
`FISHHAWKD_SINGLE_TENANT_*` flags in serve.go and is **diagnostic only**: it is
read on this one denial branch and NOWHERE else — never an admission input,
never consulted by the membership resolver, and never disclosed to the browser
(the redirect, its reason code, and the rendered page are unchanged). It reports
the RESOLVED profile (`SingleTenantConfig.Resolved()`) so an operator who left
granularity empty sees `enterprise`, the value admission actually uses, and the
fields are emitted ONLY when `SingleTenantProfile.Enabled()` — an unconfigured
profile logs a single `single_tenant_profile=unconfigured` marker with no empty
placeholders. This makes the two first-boot foot-guns (a byte-exact account-key
casing mismatch; a personal-namespace install left on the default `enterprise`
granularity) diagnosable from one server-side log line.

Absent or unrecognized → the generic body naming both remedies. `reason` is
matched by `parseAccessDeniedReason`, a **closed allow-list, not a
passthrough**: the value arrives on the query string of a public,
unauthenticated route and reaches the rendered page, so it must never be
attacker-chosen free text.

That allow-list's effect is made **observable**: a non-empty unrecognized value
is logged (`access-denied page: unrecognized reason code`). Without that,
deleting the allow-list would change nothing a test could assert — the template
renders the generic branch for an unrecognized value either way — and the
control would have no attainable counterfactual. The log line also makes the
generic page's "recorded in the fishhawkd log" advice true.

### No identity is claimed — by either page, in any topology

The page names the DEPLOYMENT'S decision, never the user's action. There is no
"You signed in to GitHub as `octocat`" line, no `provider`/`login` on the deny
redirect, and no identity parameter read by either renderer.

That is a correction, not the original design (implement-review high/security
on this PR). The first cut rendered an identity line on the backend page under
an approval condition asserting that the backend "knows the authenticated login
from the callback it just processed". It does not. The callback authenticates
and then issues a **302**; `GET /access-denied` is a SEPARATE, unauthenticated
request with no session and no cookie, whose parameters are whatever the URL
carries. Its authority is exactly the SPA's: none. Anyone could have crafted
`/access-denied?provider=github&login=<valid-name>` and obtained a
Fishhawk-branded page asserting that login had signed in. A charset bound stops
markup but establishes nothing about authenticity, so the CLAIM was removed
rather than the input hardened further — no amount of validation turns an
unsigned query parameter into an authenticated fact.

| Topology | Serves `/access-denied` | Shows the login? |
|---|---|---|
| Split-origin (Vite dev, self-host with the SPA on its own origin) | fishhawkd's `handleAccessDenied` | **No** |
| Same-origin reverse proxy routing non-`/v0` to the SPA | `frontend/src/routes/access-denied.tsx` | **No** |
| Client-side `RequireAuth` navigation (no query at all) | the SPA page | **No** |

Both pages read the same `reason` enum, so either routing renders the same
branch-specific explanation — the two now differ in nothing that matters. The
page closes by disclosing its own limits ("This page names no account: it is
served without a session…") and pointing at the fishhawkd log, which records
the provider and login the attempt was made with. That is where an operator
correlates identity, and the only place that can do it honestly.

Binding an identity properly — a short-lived HttpOnly cookie or one-time token
set by the callback — is a real design with its own surface, and out of
proportion to a page whose job is explaining a denial. It is deliberately NOT
done here.

### 200, not 403

The request to `/access-denied` **succeeds** and returns the explanation. A 403
would claim the operator is forbidden from READING the explanation, and would
invite an intermediary proxy to substitute its own error body — the exact class
of failure this issue fixes. The denial itself is still recorded honestly: the
callback creates no session and no cookie, and logs the reason server-side.

The page sets `Cache-Control: no-store` and `Referrer-Policy: no-referrer` —
ordinary hygiene for a public error page reached mid-OAuth, whose URL would
otherwise ride the Referer to whatever the operator navigates to next.

### Parameter merge

`accessDeniedRedirect` resolves the base target exactly as before —
`Config.AuthAccessDeniedRedirect` when `isSafeRelativeRedirect` accepts it,
`/access-denied` otherwise — then merges `reason`, and nothing else, into the
resolved target's **existing** query via `net/url`, never a naive `"?"` append,
so an operator-configured target keeps its own parameters. `url.Values.Set`
(not `Add`) is load-bearing: `Add` yields duplicate keys and `Query().Get`
returns the FIRST, so an operator-set `reason` would shadow the branch code and
the page would name the wrong denial — precisely the failure this change exists
to fix.
