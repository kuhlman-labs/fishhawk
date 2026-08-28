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

## Charter injection consumer (`charter_injection.go`, E54.2 / #2234)

The SECOND consumer of the repo-document injection mechanism `backend/internal/repodoc`
shipped inert (E55.1 / #2242). It adds **no** resolution, fetch, hashing, capping,
delimiter-neutralization or audit code of its own — all of that is repodoc's, reached
through the untouched `resolveInjectedDocuments`. It supplies exactly three things:
the **declaration site** (`charter.path` in `.fishhawk/work-management.yaml`, read
through the process-wide `conventionsLoader` seam), the **framing** (what a charter is,
plus the instruction to cite rubric lines BY ID — the uppercase `V*/R*/U*/S*` ids the
charter's rubric tables carry, matching the citation contract #2235 validates), and the
**fail-closed policy**.

**Stage discriminator — STRUCTURAL.** `stageRequiresCharter` is true for a `plan`-typed
(PROPOSE, ADR-067 §2) run stage whose resolved workflow satisfies the shipped
`WorkflowRequiresCharter` predicate, i.e. declares the `grooming_report` artifact. Not
the workflow's NAME (renaming would evade it) and not a `kind:` field.

**Two fail-closed layers.**

| Layer | Where | Refuses when |
|---|---|---|
| L1 `charterDeclarations` | `Config.DocumentDeclarations` | conventions unreadable (`conventions_unavailable`), no `charter:` block (`charter_absent`), empty `path:` (`charter_path_empty`), base ref unresolvable (`charter_base_ref_unresolved`) |
| L2 `assertCharterInjected` | `handleGetStagePrompt`, after `resolveInjectedDocuments` | the injected set carries no document resolved from the declared charter path (`charter_not_injected`) |

L2 is what makes the guarantee unbypassable by a deployment that simply leaves the seam
unwired — the one configuration in which L1 cannot run. **L2 verifies charter IDENTITY,
not "some document":** it re-resolves the declared path and requires an injected document
AT THAT PATH, so a grooming prompt carrying an unrelated injected document and no charter
is refused. Both layers surface as `document_injection_failed` (no new error code) with
the machine-readable `reason` above; the actionable message reaches the operator through
the log record, because a 5xx body's details are redacted to the default-deny allow-list
above.

**Base ref — what is actually guaranteed.** `Config.DocumentBaseRef` (wired in
`cmd/fishhawkd` to `forge.Forge.GetRepository`) supplies the repo's **default branch**,
which repodoc pins to a 40-hex commit before any fetch. So: resolution pins to a specific
commit, never a mutable ref; the commit and content hash are recorded so a ranking is
attributable to an exact charter revision; and the commit is the default-branch head AT
SERVE TIME, which for a non-diff workflow is the defined base (a grooming run produces no
diff and owns no branch). Consequence, stated rather than hidden: a charter amendment
landing between run creation and prompt serve changes which revision constrains that run
— acceptable for grooming, decidable after the fact from the audit hash, and **not
reusable** by E55's review-conventions consumer, which still needs the per-run source
`backend/internal/repodoc/README.md` names.

**Non-grooming prompts are byte-identical.** The declarations func returns zero
declarations for every other stage (and never touches the conventions loader), and
`resolveInjectedDocuments` short-circuits on an empty set. The one place that could have
leaked is a cached `WorkflowSpec` that cannot be re-parsed, where whether the stage is a
grooming propose stage is undecidable. `specCouldBeGrooming` narrows the refusal, and the
narrowing is ATTRIBUTED rather than a document-wide byte scan — a raw `bytes.Contains`
refused a corrupt NON-grooming spec whose bytes carried `grooming_report` incidentally,
which is exactly the non-grooming behaviour change H4 forbids:

- **The document decodes as YAML** (the dominant corruption class: well-formed YAML that
  fails schema validation). A decoded document has exactly ONE parse, so the test is
  exact — some scalar or key EQUAL to the artifact kind, searched inside THIS run's
  workflow subtree only. A comment is not a node; an unrelated prose scalar is not equal;
  another workflow's `grooming_report` is outside the subtree. All three fall OPEN,
  byte-identically. The search widens to the whole document when the run's workflow is
  absent (nothing to attribute to) or the document uses workflow-v2 `defaults` / `extends`
  reuse, where an inherited `produces` block can come from outside the subtree.
- **The document does not decode at all** (YAML syntax corruption). No structure to
  attribute against, so the fallback is the byte scan minus FULL-LINE comments.

A spec with no grooming evidence falls OPEN exactly as `resolveImplementConstraints` and
`resolveImplementRequiredOutcomes` do. Residuals, stated: in the non-decoding branch a
token in an INLINE comment or an unrelated scalar still refuses a corrupt non-grooming
spec; and a corruption that also destroys the token falls open on what may have been a
grooming run. Closing the second in general means refusing every plan prompt whose cached
spec is corrupt — the repo-wide flip H4 rules out — and the cached bytes were validated at
run-create, so a parse failure is storage corruption, not a normal or adversarial state.
A third: `yamlUsesSameDocumentReuse` keys on the NAME of a `defaults` / `extends` mapping
key ANYWHERE in the document rather than on a resolvable inheritance edge, so an unrelated
`defaults` map widens the search document-wide and another workflow's `grooming_report` is
then counted. All three refusals fail CLOSED and reach only storage-corrupted specs, and
each is pinned as DELIBERATE by a row in `TestSpecCouldBeGrooming_Attribution` so a later
edit to either branch cannot silently widen or narrow it.

**Preview divergence (#2804).** `handleGetStagePromptRender` injects no documents at all
(a pre-existing #2242 divergence), so L2 is deliberately NOT wired there — asserting on a
handler that never injects would refuse every grooming preview. A preview and a served
prompt for the same stage therefore differ in exactly the security-relevant block.

**Wiring.** `cmd/fishhawkd`'s `wireDocumentInjection` installs the four `Config` members
(resolver, scope, base ref, declarations) TOGETHER or leaves all four nil, because
`resolveInjectedDocuments` treats a configured declaration seam with a nil resolver as a
wiring defect and fails EVERY prompt request. Forge selection is github-then-gitlab, and
selection ALONE does not make a non-selected-forge repo fail: `forge.RepoRef` carries only
owner/name, so a gitlab-hosted `acme/widgets` whose owner/name also names an accessible
github repository would resolve, and the prompt would carry THAT repository's charter — a
cross-repository provenance failure, not the refusal the mixed-forge guarantee claims.
`documentForgeOwnershipGuard` enforces it, wrapping `DocumentScope` (through which the
credential scope, the default-branch lookup and every file fetch all pass, so a refusal
costs ZERO forge calls). It consults the same `accounts.provider` discriminator the
conventions loader and the repo-visibility cross-forge deny use: a resolved provider that
is not the selected forge REFUSES; a resolver error REFUSES; an AMBIGUOUS repo (no
resolver, or `found=false`) refuses only on a MIXED deployment, where a second forge could
own it — a single-forge deployment keeps the pre-guard posture rather than switching
document injection off wherever no accounts table exists.
`Server.CharterDocumentDeclarations` is the exported entry point the wiring needs
because `server.New` copies its `Config` by value, so the seam must be installed before
construction while the implementation needs the constructed Server.

Tests: `charter_injection_test.go` (cross-boundary end-to-end plus one behavioural test
per refusal branch, each asserting the reason IDENTITY so a deleted branch reddens rather
than passing on a neighbouring control; the M8 H4-boundary set — token in a comment, in an
unrelated scalar, in ANOTHER workflow, and the same document read for the workflow that
DOES declare it; `TestCharterFixtures_AreActuallyUnparseable`, which pins that each M8
fixture is genuinely parser-rejected and which branch it drives; and
`TestGroomingPrompt_L2Divergence_FailsClosed`, which drives a conventions loader that
answers differently within one request — L2 re-resolves independently of L1 by design, so
it must fail closed on a divergence), `cmd/fishhawkd/serve_test.go` (forge preference, the
all-four-or-none invariant, the base-ref adapter, and the cross-forge collision: ONE
owner/name registered on both forges, refused through the non-owning one with zero forge
calls). The counterfactual RED observations for each control are recorded at the top of
`charter_injection_test.go`.

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

## Per-wave integration guard + `base_branch` derivation (`decomposition_dispatch_guard.go`, E50.13 / #2363)

The wave-ORDER guard above proves a dependent child's dependency slices have
SUCCEEDED. That is deliberately NOT sufficient to spawn it. Run state flips to
`succeeded` BEFORE the between-wave integration merges the slice branches onto
the parent's consolidated branch, so a child admitted on state alone would build
on a base missing its predecessors' symbols (#1302). `resolveDependentChildBase`
closes that gap and, in the same step, makes the marker the AUTHORITY on the
per-wave re-base: the 200 body gains `base_branch`, and the client derives
nothing.

- **The admission rule**: for a fan-out child with a non-empty resolved
  `depends_on`, read the parent's NEWEST `slices_integrated` audit entry and
  ADMIT with `base_branch = consolidated_branch` only when (a) that branch is
  non-empty AND (b) `wavecoverage.Covered(depends_on, sliceRunID, child_run_ids)`
  holds. **A non-empty consolidated branch is NOT on its own sufficient** — that
  is precisely the STALE case (wave 1 integrated; this wave-2 child's
  newly-succeeded dependency not yet merged), which `consolidatedBranchFromAudit`
  cannot distinguish because it reads only the branch name. Anything else
  refuses `409 wave_not_integrated` with `details` `{slice_index, depends_on,
  missing_dependency_slices, consolidated_branch_present}`.
- **One predicate, three callers**: the coverage question is answered by the
  shared leaf package `backend/internal/wavecoverage`, the SAME function the
  child-completion sweeper's steady-state short-circuit and the MCP await verb's
  `children_dispatchable` release use. A reconstruction here — or a caller keying
  on predecessor run STATE — would announce dispatchability this endpoint then
  refuses; that drift is the class the repo already names load-bearing (the
  `SliceBranch`/`childSliceBranch` "MUST stay byte-identical" note).
- **One entry, both fields**: `latestSlicesIntegrated` decodes
  `consolidated_branch` AND `child_run_ids` from THE SAME newest entry. Reading
  them through separate walks would let a child be admitted onto a branch whose
  coverage was proved by a different integration (pinned by
  `TestResolveDependentChildBase_DoesNotSpliceAcrossEntries`).
- **Ordering relative to the CAS**: derived AFTER `guardDecompositionWaveOrder`
  and BEFORE the state switch/CAS, so a refusal commits NO state and the child
  stays cleanly re-dispatchable once the sweeper integrates — and the wave-ORDER
  refusal still decides first when a dependency has not succeeded (a
  wave-order violation must never be reported as an integration wait, since
  waiting would never clear it). `base_branch` also rides the idempotent
  already-`dispatched` arm, because that caller re-spawns.
- **Absent vs errored**, the same partition the wave-order guard uses: a
  non-fan-out run, an empty `depends_on`, or an unresolvable parent plan are
  ABSENT — admit with no `base_branch`, i.e. byte-identical to pre-#2363
  behaviour. A DEPENDENT child (non-empty `depends_on`) with an UNCONFIGURED
  `AuditRepo` is NOT absent — it REFUSES (fail-closed): with no audit repository
  there is no integration record to prove its predecessors merged, and admitting
  would spawn it onto an unproven base (the #1302 stale-base class the guard
  exists to close). A wave-0 child with no dependencies still admits, so an
  unconfigured deployment's INDEPENDENT dispatches are never wedged — only a
  genuinely dependent child is refused, and in practice `AuditRepo` is always
  configured (`serve.go`), so this is a test-posture fail-closed. An undecodable
  newest payload is ABSENT for the coverage question and therefore REFUSES
  (it must not admit on bytes the server could not read). An ERRORED read
  (plan load, audit list, sibling list) is `500 dependency_check_failed`,
  retryable, never a silent admit.
- **Wire fixture (#2660 lesson)**: `testdata/host_dispatch_dependent_child.json`
  is proven byte-identical to the handler's own 200 body by
  `TestHostDispatch_DependentChild_ResponseMatchesWireFixture`, and the MCP
  client test serves THOSE bytes to a real `apiClient` — so the client cannot
  share a wrong json tag with a fake that re-marshals its own struct. The
  sibling decode (`child_run_ids`) is tied to the emitter the same way by
  `TestLatestSlicesIntegrated_DecodesRealEmitterPayload`, which drives the REAL
  fan-in and decodes its genuine entry: drifting the decoder tag and the
  hand-seeded fixture key TOGETHER leaves every coverage test green and reddens
  only that test.

Tests: `decomposition_dispatch_guard_test.go` (`TestResolveDependentChildBase_*`
— one case per admit/refuse/degrade branch, including the unconfigured-audit
refusal for a dependent child, the undecodable payload, the
covered-but-empty-branch refusal and the cross-entry splice pin),
`host_dispatch_test.go`
(`TestHostDispatch_DependentChild_EndToEnd`, the two counterfactual controls
`TestHostDispatch_RefusesDependentChildWhenWaveNotIntegrated` /
`…WhenIntegrationIsStale` — each asserting error IDENTITY *and* reading the
stage row back, since the control's effect is committed state — the wave-order
precedence pin, the wire-fixture equality and the emitter-fidelity decode), and
`mcpserver/client_test.go` (`TestHostDispatchStage_WireShape`: the fixture-bytes
decode and the `wave_not_integrated` annotation).

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

## Stage progress heartbeat ingest (`stage_progress.go`, E48.96 / #2541)

`POST /v0/runs/{run_id}/stages/{stage_id}/progress` (`handleReportStageProgress`) projects the runner's mid-execution `stage_progress` heartbeat onto the stage row so an operator poll returns `last_event` / `turns_this_attempt` / `tokens_this_attempt` (via the `Stage.progress` field, and the derived `elapsed_seconds` on the MCP stage-wait status) instead of a single `running` bit.

- **Tier + auth impact inventory is empty.** Registered at `memberWrite` on the byte-identical path shape as the sibling host-dispatch route, so the run-bound `fhm_` bearer the runner already holds admits it and **no new token capability is introduced** (`fishhawkd token migrate` reports `scanned=N migrated=0`).
- **Consumer-side narrow capability.** The handler type-asserts `s.cfg.RunRepo` for `stageProgressStore` (the `runCostRecorder` / `runnerKindResolver` precedent); a RunRepo that does not implement it answers `503 progress_unsupported` rather than panicking. `run.StageProgressStore` is the concrete-repo capability it mirrors.
- **Input validation.** `run_id`/`stage_id` are parsed as UUIDs (400 `validation_failed`); the body is decoded with `DisallowUnknownFields` (an unknown field → 400); a negative `turns_this_attempt`/`tokens_this_attempt` → 400; `last_event` is CLAMPED to `progressLastEventMaxLen = 128` runes; `reported_at` is stamped server-side from the request clock — the runner does not get to set it.
- **The 409 `stage_terminal` refusal IS the UPDATE's own `WHERE state NOT IN (...)` predicate** (`RecordStageProgress`, `:execrows`): a heartbeat that arrives after the stage settled matches zero rows (`applied=false`) and is answered 409 with `details {stage_id, state}` — no read-then-write window (#2536). Success is `204 No Content` (the reader is the stage read). The stage-belongs-to-run handle is verified (404 `stage_not_found` on mismatch), mirroring host-dispatch.
- **`updated_at` side effect (condition 4).** The heartbeat UPDATE bumps `stages.updated_at` every ~15s via the `stages_set_updated_at` trigger. The one consumer that observes it is the dispatch watchdog (`state='dispatched'`, which a stage holds during agent execution); the audit + the design-question note live in `run/README.md` and migration 0070.

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
3. The *path-trigger rule table* (`testSweepPathTriggerRules`, #1031) — curated rows of trigger glob → required paths evaluated against the scope set only (no Contents API consultation), currently one row: `migration_walk`, any scoped `backend/internal/postgres/migrations/*.sql` requires `backend/internal/postgres/postgres_test.go` (a new migration needs its own reversal test there; planners missed it on 0029/0030/0031); `RequiredPaths` is a slice so a future row can require multiple paths per trigger.

Overlapping declared+default conventions are deduped (candidate-set per production file + findings by `(rule, trigger_path)`), so an overlap yields exactly one finding.

**NOT call-graph/behavior-coverage analysis** — a plan changing package A whose tests live in package B is out of reach by design (#942 defers that).

Bounds and degradation:

- Bounded at `testSweepMaxDirs` (20) distinct directories per upload, counted AFTER candidate expansion so parallel-tree candidate directories (`tests/`, `spec/`) are included (the rest WARN-skipped).
- Each listing failure fails open per-call, and an all-listings-failed sweep writes NO entry (never-checked, not falsely clean).
- Writes a `plan_test_sweep` audit entry (payload `TestSweepPayload{findings, scanned_files, listed_dirs}`, `findings` an empty array not null) even on a clean sweep, and additionally fails open with no entry when `cfg.GitHub` is nil or the run's `installation_id` is nil (non-GitHub triggers / unwired deployments).

The returned payload threads into the plan-review prompt's gate-evidence section as a reviewer-judged ADVISORY (not an automatic high-severity concern: judge whether the changed behavior's tests or shared harness live in the flagged files — if so the plan must scope them or the runner will scope_drift-exclude the edits).

MCP surface: `fishhawk_get_plan` adds `test_sweep` (`TestSweep{findings[], scanned_files, listed_dirs}`) decoded from the **newest** `plan_test_sweep` entry (`loadTestSweep` in `tools.go`).

## Routed-reporting-obligation undelivered pre-review signal (#2737)

`trace.go::runImplementReviews` — adjacent to the `#1407` block and using the same allocate-if-nil `gateEvidence` pattern. A fix-up pass can be routed an instruction that asks the agent to RECORD something on a report surface ("record the per-deletion counterfactual results in the PR body's `## Notes`"), and the slim fix-up prompt renders NO PR-description block, so before this the instruction could be declined with nothing marking the omission.

- `prompt.go::resolveFixupReportObligations` classifies the routed instructions (`concerns` notes, `operator_concern`, `reason` — all already on the `stage_fixup_triggered` audit payload) via `backend/internal/fixupobligation`, assigning stable `ob-N` ids. The prompt SERVE resolves the newest stage-bound entry and then pins it by audit `Sequence` in a `fixup_report_obligations_declared` anchor (`recordFixupReportObligationsDeclared`); this review-time re-derivation reads that anchor (`resolveFixupReportObligationAnchor`) and resolves that EXACT entry, so a second fix-up triggered between the serve and the review cannot make the review report ids the agent never saw. No anchor → no signal. The SPA `prompt-render` preview renders the block but writes no anchor.
- The agent records each obligation in the existing `#1210` fix-up self-report sidecar's new `obligations` array; the runner validates each entry fail-closed and carries the survivors on `gate_evidence.fixup_reporting_obligations` → `bundle.FixupReportingObligations` → `prompt.GateEvidence.FixupObligationReports` (a CARRIER field, never rendered).
- A non-empty remainder after subtracting the `met` reports renders a distinct high-priority block (`prompt.GateEvidence.FixupReportingObligations`), deliberately worded so it cannot be read as a diff-only-unverifiable finding, AND appends ONE advisory `fixup_reporting_obligation_undelivered` audit entry (payload `{declared_count, undelivered_count, obligations:[{id, source, status, text_excerpt}]}`, status `unreported` | `declined`) BEFORE any reviewer verdict.
- No agent-authored free text crosses the upload boundary. The `record`/`reason` the agent writes is validated on the RUNNER (`met` needs a record, `declined` needs a reason) and discarded there; only `{id, status}` is transmitted, with the id constrained to the `ob-N` shape. Quarantining that text at the render site would have bounded what it could impersonate but not what it could carry — an arbitrary egress channel from a command-running agent to the reviewer, around the committed diff — so the channel is removed rather than hardened. The reviewer prompt and the audit payload carry only the OPERATOR's `text_excerpt`.
- EVIDENCE ONLY: it never fails, re-opens, or re-budgets the pass. Both directions are pinned — no signal when every obligation is met (the anti-noise control), and identical review result / stage status / fix-up budget when the signal DOES fire.
- Best-effort: nil `AuditRepo`, a list error, or a malformed trigger payload contributes nothing; an append failure WARNs and the review proceeds.
- Long-form contract (classifier grammar, id ordering, fail-closed drop table, the fabricated-`met` weakness): `backend/internal/fixupobligation/README.md`. Audit-kind note in `docs/issue-comment-surfaces.md`.

## Operator-scope-undelivered pre-review signal (#1407)

`trace.go::runImplementReviews` — before building the implement-review prompt, unions the run's two operator-add provenance channels (the approval-time `add_scope_files` folds via `amendedScopeFilesForReview`, and approved mid-stage scope amendments via `approvedAmendmentScopePaths` → `ScopeAmendmentRepo.ListByRun`) and computes the subset UNTOUCHED by the committed diff (`operatorScopeUndelivered`, untouched-only: absent from `diff.ChangedFiles`; directory-prefix / non-repo-relative tokens skipped like `MissingScopeFiles`).

- A non-empty set renders a high-priority `operator_scope_path_undelivered` warning in the prompt's gate-evidence section (`prompt.GateEvidence.OperatorScopeUndelivered`, allocate-if-nil) AND appends one deterministic advisory `operator_scope_path_undelivered` audit entry (payload `{undelivered_paths, undelivered_count, operator_added_count, indeterminate}`) BEFORE any reviewer verdict — so a dropped operator-required edit (E23.9/E23.10) is visible pre-review instead of only at the reject→fixup round-trip.
- **Indeterminate carry-through (#2398 fixup).** `operatorScopeUndelivered` returns the `committedPathSet` `indeterminate` flag alongside the untouched set; when the diff carries a rename row with no source path an absent operator-added path cannot be distinguished from a rename source, so the flag is threaded onto `GateEvidence.OperatorScopeUndeliveredIndeterminate` (and `indeterminate:true` on the audit payload) and `writeGateEvidence` HEDGES the warning as NOT DETERMINABLE rather than asserting it as fact. This keeps the second evidence surface from contradicting the `scopeProvenanceForReview` decomposition, which hedges the same path under the same diff mode.
- Advisory + best-effort: a nil `ScopeAmendmentRepo` or `ListByRun` error contributes nothing and never blocks the review; an all-delivered commit keeps the prompt byte-identical and emits no entry.
- The complementary BLOCKING gate for a FULLY-untouched concrete DECLARED scope path is the runner's #1151/#1231/#2501 scope-completeness park (`gitops.MissingScopeFiles`; the scope-completeness invariant in `docs/ARCHITECTURE.md`); this is the advisory pre-review surface for the partial / operator-added case.
- Audit-kind note in `docs/issue-comment-surfaces.md`.

### Decomposed-parent child-amendment rollup (#2820)

A decomposed PARENT's implement review resolves its declared-scope provenance from the PARENT run's own records only, so a fan-out CHILD's APPROVED mid-stage scope amendment — which lives under the CHILD run id — reaches the parent's consolidated diff with no provenance on the parent: it renders as an unexplained residual and the reviewer flags scope drift (#2237, #2239, the second of which produced a durable audit record falsely asserting a skipped process step). `childApprovedAmendmentScopePaths` (`child_scope_amendment.go`) closes that gap. `trace.go::runImplementReviews` calls it once and threads the result onto `prompt.Trigger.ChildAmendedScopeFiles`; it is consumed BOTH by the review prompt's "Scope authorized by child slice amendments" section and by `scopeProvenanceForReview`, which folds it as the new **`child-scope-amendment`** channel (each fold carrying a per-path `GateScopeFold.Detail` naming the authorizing amendment id, slice index, and child run id) so the machine `UnexplainedCount` arithmetic accounts for the path instead of reporting a residual.

- **Children discriminated by `DecomposedFrom`, not `ParentRunID`.** The resolver reuses `listAllDecomposedChildren` (`consolidate.go`, `ListRuns{DecomposedFrom}`) so paging past the 100-row cap is inherited, not reimplemented. `ParentRunID` is set for recovery children too (the E48.102/#2549 recovery-lineage walk), so it would wrongly sweep them in; `DecomposedFrom` names only fan-out slices.
- **Fail-open, per-child.** A nil `RunRepo`/`ScopeAmendmentRepo`, a `listAllDecomposedChildren` error, or a per-child `ListByRun` error all WARN-log and contribute nothing — the last ONLY for the failing child, whose siblings still resolve. A provenance lookup must never block a review; the end-to-end degrade (a failing amendment lookup still yields a DISPATCHED review with the section simply absent) is pinned in `implement_review_test.go`. Children are sorted deterministically (`SliceIndex` nil-last, then `CreatedAt`, then id) and paths deduped first-wins so the rendered prompt is stable.
- **Deliberately OMITTED from the #1407 operator-scope-undelivered union** (comment recorded in `runImplementReviews` at the `operatorAdded` build): the child-amendment set is a PERMISSION granted on a child slice, not a work-order the operator placed on THIS parent. Folding an approved-but-unused child path into the undelivered union would surface it as an operator-scope-undelivered MISS on the parent — trading the #2820 false drift signal for a false miss signal. It is folded only into the review-facing provenance.
- **Runner-side enforcement is unchanged.** The runner's scope gate on a decomposed-parent fix-up pass is out of scope; this change is review-facing provenance only.

### Rename-aware touched set (#2398)

Both this `#1407` signal and the `#1914` `scopeProvenanceForReview` decompose "was this declared path touched?" against a shared `committedPathSet(diff)` helper. It inserts every row's destination `Path` AND, for `Status == StatusRenamed` rows carrying a non-empty `OldPath`, the rename **SOURCE** — because porcelain rename detection collapses a declared delete+create into a single `R` row that records only the destination, so the source path never appears as its own row and would otherwise read as untouched (manufacturing a spurious high-severity `evidence_conflict` on any file move — run 933cd6ee / #2397). A **COPY** (`StatusCopied`) source is deliberately NOT inserted: a copy leaves its source byte-unchanged, so treating it as touched would replace a false negative with a false positive. When the diff carries any RENAME row with an EMPTY `OldPath` (a pre-field bundle, or the forge-compare consolidated-review diff), the helper reports `indeterminate` — an absent declared path cannot be distinguished from a rename source, so the provenance marks itself indeterminate and the prompt hedges the untouched label rather than asserting it. Copy rows with an empty source do NOT set indeterminate (a copy source is never counted as touched, so its absence cannot flip an untouched verdict).

## Re-review convergence: settled ledger + re-litigation guard (#1913)

`trace.go` makes implement re-review rounds converge by threading settled history forward and turning operator arbitrations into a machine-binding suppression guard (issue #1913; measured churn on runs a04d5cbf / 98704b0c).

- **Settled-ledger threading.** `settledConcernsForReview` (sibling of the OPEN-only `priorConcernsForReview`) gathers the stage's `waived`/`deferred` + `addressed`/`superseded` concerns into `prompt.Trigger.SettledConcerns`, threaded into every post-fixup round so a round-N reviewer has the full settled history (deferred arbitrations, invisible before, now reach the reviewer). Waived concerns MOVED out of `priorConcernsForReview` into this set; `hasFixupRoutedConcern` still gates the #1725 delta on `addressed_pending`, unaffected.
- **`concern_relitigation_suppressed` audit-category contract.** An internal, advisory, best-effort audit kind (system actor, payload `{settled_ref, settled_state, severity, category, note, reviewer_model, origin_review_sequence}`) written by `persistReviewConcerns` → `suppressRelitigation`/`appendRelitigationSuppressed` when a verdict concern's `settled_ref` resolves to a **same-run/same-stage/same-stageKind** `waived`/`deferred` concern AND its `new_evidence` is empty — the guard excludes that concern from the durable open-row insert and records this entry instead (so the suppression is visible, never silent). It posts NO issue comment and adds no Notifier method, so it is NOT an issue-comment surface (it is registered in `audit.KnownCategories` for `fishhawk_await_audit`). Fail-open on every other case — unparsable/unknown ref, cross-stage ref, non-waived/deferred state, non-empty `new_evidence`, and any lookup/append error (WARN) all fall through to the normal insert, so a sloppy tag never suppresses a genuine finding and a store outage never wedges the loop. A re-raise against an `addressed`/`superseded` concern is deliberately insertable (a genuine regression must reach the operator).

## Round-level concern-resolution veto (`trace.go`, E48.103 / #2551)

A routed concern used to auto-resolve on whichever reviewer confirmed it. `runImplementReviewInvocations` applied each verdict's `concern_resolutions` INLINE, per reviewer, with no knowledge of what the other reviewers in the SAME round said or of what the fix-up pass actually did — so a diff-only peer's `confirmed` retired a high-severity concern while the reviewer that RAISED it was still returning `reject` (run 9bba554d, concern 3b012c1f).

- **Buffered round application.** The loop now buffers each verdict as a `roundReviewVerdict{model, verdict, resolutions, reviewSequence}` under the UNCHANGED append-gated posture (buffered only when the `implement_reviewed` append returned a sequence), and `applyRoundConcernResolutions` replays them IN LOOP ORDER after the loop and BEFORE `recomputeAndPublishAuditComplete` (so the republished check still reflects post-resolution state). REOPEN-WINS is unaffected — `concern.validTransitions` encodes it order-independently, and both orders are pinned by test. The only mid-loop concern-row reader is `persistReviewConcerns` → `suppressRelitigation`, which keys strictly on `waived`/`deferred` — states no delta-verification resolution can produce — so buffering cannot change what it observes; `resolveConditionClaimedPlanConcerns` reads PLAN-stage rows (this path is implement-only) and the `hasRejection`/`pagedRejectAppended` bookkeeping is in-memory.
- **Four deterministic veto arms, evaluated in fixed order against a `confirmed` resolution ONLY** (`resolutionVetoContext.vetoReason`; `reopened`/`superseded` are never vetoed): (1) `raiser_rejected_same_round` — a DIFFERENT reviewer confirms a concern whose RAISING reviewer (`concern.ReviewerModel`, matched by model string) returned `reject` in this round; a reviewer confirming its OWN concern is never vetoed, since that reviewer IS the authority the veto respects; (2) `operator_evidence_routed` — the concern was routed by a same-stage `stage_fixup_triggered` carrying a non-empty `operator_evidence` (see below); (3) `fixup_pass_no_changes` — the concern's NEWEST same-stage routing trigger is followed by `fixup_no_changes` with no earlier `fixup_pushed`; arm 3 fires on POSITIVE evidence of a no-change pass, so a newest trigger with NO following outcome entry at all is NOT vetoed — a re-review round runs only after the fix-up pass terminalizes, so the only real-round way to observe that is a failed best-effort outcome append (an audit-durability defect), and vetoing there would refuse confirms for a pass that DID push with no later entry to clear the veto; (4) `evidence_lookup_failed` — the audit reads behind arms 2/3 failed.
- **The evidence reads FAIL CLOSED** (a deliberate departure from this loop's prevailing fail-open posture): a nil `AuditRepo`, a `ListForRunByCategory` error, or a malformed trigger payload vetoes every `confirmed` in the round rather than applying it on unknown evidence. Asymmetry of harm — a wrongly-vetoed concern stays open and costs the operator one waive/defer (both remain available, so the gate cannot wedge), while a wrongly-applied confirm is the silent false-GREEN this issue reports.
- **A vetoed resolution mutates NOTHING.** `ApplyResolution` is not called: the concern keeps its current open state AND its `state_reason`. The refusal is WARN-logged and recorded as a `concern_resolution_vetoed` audit entry.
- **`concern_resolution_vetoed` audit-category contract.** Internal, advisory, BEST-EFFORT (system actor, payload `{concern_id, resolution, veto_reason, confirming_reviewer_model, raising_reviewer_model, concern_severity, concern_category, note, review_sequence, origin_review_sequence}`), registered in `audit.KnownCategories` so `fishhawk_await_audit` accepts it. It posts NO issue comment and adds no Notifier method, so it is NOT an issue-comment surface (same posture as `concern_relitigation_suppressed`) — `docs/issue-comment-surfaces.md` is deliberately untouched. An append failure WARN-logs and **the veto still stands**: refusing to veto because bookkeeping failed would retire a disputed concern, which is strictly worse. That is safe only because the gate view derives the dispute from durable evidence rather than from this entry (next section).
- **`operator_evidence` (`fixup.go`).** An optional field on `POST /v0/stages/{stage_id}/fixup` declaring that the OPERATOR executed a reproduction. Its meaning is AUTHORITY, not prose delivery — the reproduction text still reaches the agent via `reason`/`operator_concern`. Validated beside `operator_concern` (whitespace-only → 400 `field: operator_evidence`; > `maxOperatorConcernBytes` → 400 naming the byte count and cap) and NOT a selection input (it alone does not satisfy the at-least-one-of rule). Recorded on the `stage_fixup_triggered` payload under `operator_evidence` ONLY when non-empty, so pre-#2551 payloads stay byte-identical. The exemption it grants is PERMANENT for those concerns: no later confirmation retires them — only a waive, a defer, or a genuine fix.

## Gate decision view (`gateview.go`, E48.13 / #1960)

`GET /v0/runs/{run_id}/gate-view` (`handleGetRunGateView`) answers "what is still open at this gate and why" in ONE read, replacing the `getRun` + `listRunAudit` stitch an operator otherwise runs at a review/fix-up gate. The run-status concerns block (`runs.go::buildRunConcernsPayload`) carries only a BOUNDED note-derived `short_summary` label per concern (at most 100 bytes, one line; #2488) — enough to recognise a defect, not the full prose; this surface returns the untruncated `concern.Concern.Note` intact.

- **Response shape.** Each OPEN concern (`raised`/`addressed_pending`/`reopened`) carries its FULL `note`, `severity`, `category`, `reviewer_model`, `origin_review_sequence`, a derived `round` (implement-only), `state_reason`, `has_suggested_patch`, the reviewer's `new_evidence` and the re-raise lineage tag `settled_ref` (#2353, both `omitempty` — a no-evidence concern leaves the payload byte-identical, and rows minted before migration 0069 carry neither), plus `fixups[]` and `resolutions[]`. The settled ledger (`waived`/`deferred`/`addressed`/`superseded`, each with `state_reason` and the same two evidence fields — the ledger is what an operator re-reads when judging whether a re-raise is legitimate) and the run's `concern_relitigation_suppressed` entries ride along. `suggested_patch` diff text stays elided as `has_suggested_patch` (token-dominant, not decision prose) — the response is sized by SCOPING (the optional `stage_kind=plan|implement` filter), not truncation.
- **History is reconstructed from the immutable audit payloads**, because `concern.StateReason` is OVERWRITTEN on every transition (`MarkAddressedPending` writes the routing reason, then `applyConcernResolutions` overwrites it with the re-review note) — there is no stored per-round history. `fixups[]` join each `stage_fixup_triggered` whose `concern_ids` names the concern (contributing `{sequence, reason}`) to the outcome (`apply_path`/`head_sha`) of the earliest following `fixup_pushed`/`fixup_no_changes` (`pending` when none yet). `resolutions[]` join each `implement_reviewed`/`plan_reviewed` payload's `concern_resolutions` entries keyed by concern ID. `round` = `1 +` the count of same-stage `stage_fixup_triggered` sequences below the review sequence (the `review_action_hint.go::latestRoundConcerns` convention); the handler sorts fetched audit entries by `Sequence` defensively rather than relying on repo order.
- **Disputes (E48.103 / #2551).** Each open concern carries `disputed` plus `disputes[]`. `disputed` is DERIVED FROM DURABLE EVIDENCE — this row's own open state plus a `confirmed` entry in the authoritative `implement_reviewed`/`plan_reviewed` `concern_resolutions` join — so a confirmation that did not settle the concern is always visible, INDEPENDENT of the best-effort `concern_resolution_vetoed` append. `disputes[]` is enrichment decoded from that category (added to `gateViewHistoryCategories`, so an unreadable read degrades via `history_incomplete` + a named gap) carrying `{sequence, round, veto_reason, resolution, confirming_reviewer_model, raising_reviewer_model, note}`; it can be EMPTY on a disputed concern when the veto append failed. A concern confirmed and later reopened also reads as disputed — the same operator-visible fact (a confirmation is on record that did not settle it). The settled ledger is untouched: a vetoed concern is by construction still open.
- **Degradation is visible, never silent.** `AuditRepo` nil, or any per-category `ListForRunByCategory` error, returns 200 with the concerns intact, `history_incomplete=true`, and `history_gaps` naming each failed category; a single malformed payload entry is skipped warn-only while its siblings still join. `ConcernRepo` unconfigured → 503 `gate_view_unconfigured` (mirrors `fixup_unconfigured`); `RunRepo` unconfigured → 503 `run_repo_unconfigured`; unknown run → 404; bad `stage_kind` → 400; a `ConcernRepo.ListByRun` error → 500 `internal_error`. **Auth mirrors `handleListRunAudit`'s read posture** (full reviewer prose must not be anonymously readable, #1960 authz): a run-bound `mcp:run:<uuid>` token is authorized by the cross-run subject guard alone — it may read only its own run (403 `cross_run_gate_view`, mirroring the fix-up handler; a malformed `mcp:run:` subject → 401 `authentication_required`) — while every other caller must clear the `read:audit` scope (anonymous → 401 `authentication_required`, a token missing the scope → 403 `insufficient_scope`, cookie-session operators bypass per `requireWriteScope`).

## Deploy-record environment label (`trace.go`, E23.18 / #2324)

The deploy record's `environment` label — the `environment` field of the KindDeployment artifact body and of the `deployment_outcome_recorded` audit payload, written by `ResolveDeploymentFromPollState` and `ResolveDeploymentRollbackFromPollState` — is the environment the operator ACTUALLY approved, not one re-derived from schema ordering. A multi-environment deploy stage (`allowed_environments: [staging, prod]`) approved with `--environment=prod` previously mislabeled the deploy as `staging` (the first entry) on both surfaces.

- **Resolution order.** `deployEnvironmentForStage(ctx, runID, stageID)` returns the approved environment first (`deployApprovedEnvironment`), and the spec derivation (`deployEnvironmentFromSpecStage`) only as the FALLBACK when no explicit `--environment=` approval is recorded (the genuinely single-environment case). `deployApprovedEnvironment` reads the deploy stage's approvals (`ApprovalRepo.ListForStage`, submitted_at-ascending) and keeps the LAST APPROVE carrying an explicit `--environment=` flag — last-approve-wins mirrors the gate, where the approval that advanced the stage is the one that passed the pre-flight.
- **Structural agreement with the gate, by construction — STAGE-SCOPED (E23.19 / #2642).** The deploy-stage SELECTION (`deployStageForRunStage`, keyed on the stage row's DEPLOY ORDINAL and reached through the one `resolveDeploySpecStage` chokepoint) and the allowed_environments FOLD (`lastWinsAllowedEnvironments`) are each ONE helper called by the pre-execution approval gate (`approvals.go::checkDeployPreflight`), this record-side resolver (`deploySpecStageForStage`), AND the deploy trigger's delegate resolution (`deploy_trigger.go::resolveDeployDelegate`). All three therefore key on the SAME deploy stage — the one matching the stage row being acted on — so on a workflow with more than one deploy stage the gate, the record, and the trigger each resolve the stage actually being deployed rather than the first deploy stage (the #2642 defect). `resolveDeploySpecStage` returns a typed reason so the gate keeps its per-precondition refusal messages while the record and trigger collapse every non-OK reason to a fail-closed outcome. An unresolvable selection fails closed on BOTH sides: the gate refuses `422 deploy_preflight_unevaluable` + a `deploy_preflight_refused` audit, and the record returns `""` rather than labelling from a stage it cannot confirm. `deployApprovedEnvironment` re-checks the approved value against `lastWinsAllowedEnvironments` — the gate's last-wins fold on the SAME resolved stage — and a non-member value returns `""` so the record falls back rather than publishing an environment the gate would have refused.
- **The two run-scoped rollback resolvers stay first-match.** `deploy_rollback.go`'s `deployStageForRun` and `deployDelegateForRun` serve a run-scoped rollback request that carries NO stage identity, so first-match is the only selection available there; on a multi-deploy-stage workflow they resolve the FIRST deploy stage (tracked by #2642), and their doc comments no longer claim a false schema-uniqueness invariant.
- **Why a reject comment and a non-member value are ignored.** Only an APPROVE decision reflects a gate-admitted choice; a `--environment=` flag on a REJECT never passed the gate, and an environment absent from the stage's allow-list is one the gate would refuse — both are dropped.
- **Best-effort, silent fallback.** A nil `ApprovalRepo`, a `ListForStage` error, an absent/unparseable spec, or a workflow/deploy stage the spec does not carry all return `""` and fall back — never an error. The label is a convenience surface; `external_run_url` + `outcome` remain the authoritative outcome fields.
- **First-wins in the fallback is the #2218-characterized behavior**, retained deliberately: on a legacy duplicate-kind `allowed_environments` document the fallback reports the first entry. Selection across MULTIPLE deploy stages is now STAGE-SCOPED (#2642): the gate, the record, and the trigger each key on the deploy stage matching the stage row being acted on, so a second deploy stage gates and labels against its OWN `allowed_environments`, not the first stage's.

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
- **`missing_live_validation_marker` (#2845, E54.31)** — the second advisory rule on the same shared evaluator, and NOT a duplicate of `undecidable_criterion`. It flags a criterion whose statement names a LIVE forge/deploy/external **target** but which is not marked `requires_live_validation`. Two matchers, either sufficient: **M1** reuses the shared `unevaluableCapabilities` corpus, restricted to entries flagged `liveTarget` (the live-forge-round-trip and running-external-instance/deployed-environment entries) — corpus REUSE, so no second phrase list can drift, and no phrase string moved, leaving `undecidable_criterion`'s output byte-identical; **M2** is a three-conjunct proximity matcher for named-system prose no fixed phrase list anticipates — a liveness qualifier within 4 tokens of a live-ACTION noun, AND `against` within 4 tokens of an external-target noun, AND no sandbox marker in that against-phrase window.
  - **Exemption is `requires_live_validation` ALONE**, unlike `undecidable_criterion`'s either-declaration exemption. That difference IS the fix: only the marker auto-files the tracked operator-validation walk on plan approval, so a live-target criterion marked `skip_expected`-with-basis alone was exempt from `undecidable_criterion`, drew no finding at all, and silently lost its walk — observed across four runs in #2845.
  - **Deliberate non-coverage** of the MCP-client / operator-session / webhook-delivery capabilities (`liveTarget: false`). The plan artifact schema scopes `requires_live_validation` to a live forge/deploy/external target, "not merely an external trigger event, which `skip_expected` covers" — for those three, `skip_expected` with a basis is the doctrinally complete marking and no walk is owed, so demanding the marker would fire on correctly-authored criteria. Widening is a one-line `liveTarget` flip; the decision is recorded by a control test.
  - **No cross-rule suppression**: a wholly-unmarked live-target criterion draws exactly one finding from EACH rule (one per criterion per rule). Complementary, not redundant — `undecidable_criterion` says "declare it (either marking)", this rule says "the weaker marking will not suffice here" — and it avoids a two-step in which an author applies the weaker remedy and only then learns it was insufficient.
  - **M1's sandbox-marker negation is scoped to ONE CLAUSE**, not to the whole statement. A whole-statement negation is itself a false-NEGATIVE hole — any stray `fake`/`mock`/`preview` anywhere in a sentence disabled M1 even when the sentence named a genuine live target, recreating the defect the rule closes. So the statement is split at clause punctuation (`,` `;` `:` newline em/en-dash — `.` deliberately excluded, since splitting on it would cut the corpus phrase `against github.com` in half) and a clause carrying a liveTarget phrase and no stand-in **of its own** fires, however many stand-ins its neighbours mention.
  - **Known residual**, stated rather than papered over: the clause-scoped negation rescues prose whose clause names its own stand-in ("the `github api` client retries in the **fake** transport test") but not a liveTarget phrase in sandbox-validatable prose with no marker at all ("the **deployed environment** config template is rendered"). The only narrowing that would suppress it also drops the true positive "the deployed environment serves the new endpoint", so the residual is pinned by a test instead. A corpus phrase straddling a clause boundary would also be missed; no corpus phrase has that shape.
- Writes a `plan_acceptance_precheck` audit entry (payload `AcceptancePrecheckPayload{workflow_id, acceptance_stage_id, findings, criteria_count, blocking_count, out_of_scope_count, undecidable_count, live_validation_marker_count}`, `findings` an empty array not null) **even when clean** so a reader distinguishes "checked and clean" from "never checked". `undecidable_count` and `live_validation_marker_count` are independent per-criterion headline counts read back off the findings the one evaluator already returned — no second evaluation — and an unmarked live-target criterion increments BOTH.
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

### The per-slice move channel (#2596)

`move_scope_files_to_slice` is the slice-boundary MOVE the add channel above deliberately refuses (`path_owned_by_another_slice`): the narrower cut that relocates an ALREADY-declared file from one slice to another. Same wire shape as the add — `{"<destination sub-plan title or 0-based index>": ["path", …]}` on `POST /v0/stages/{id}/approvals`, surfaced on `fishhawk_approve_plan` — but the key names the DESTINATION and the SOURCE is DERIVED from ownership. `approvals.go::checkSliceMoveScopeFiles` runs immediately AFTER `checkSliceAddScopeFiles` (so the add's canonical map is available for the cross-channel disjointness check) and BEFORE `checkPlanScopeCap`.

- **Single-owner-file preserved BY CONSTRUCTION, not by validation.** A file has exactly one owner before the move and exactly one after, so a move can never create the add/add fan-in the add channel's `path_owned_by_another_slice` refusal exists to prevent — which is the whole reason a move is safer than the add it complements. The source slice is never declared; it is located by scanning every sub-plan's `scope.files` for the exact owned path.
- **Shared key resolution + path hygiene.** `resolveSliceKeys` and `canonicalizeSlicePaths` are factored out of the add validator and reused here (parameterised on the field name), so title-first/index-fallback resolution, the ambiguous/unresolvable/duplicate-key refusals, and the trimmed/deduped/**SORTED** canonical form are byte-for-byte identical to the add channel. A title-keyed request and the equivalent index-keyed one record BYTE-IDENTICAL `move_scope_files_to_slice` payloads (index-keyed), the prompt-hash replay-stability property.
- **EXACT ownership identity, not containment.** Unlike the add's `scopePathsOverlap`, a move locates its owner by `normalizeOwnershipPath` EQUALITY. A path that only overlaps a directory-valued declared entry by containment is refused `move_requires_exact_owned_path` — neither side of a move can be expressed by splitting a directory entry, so that case must be re-planned. Ownership is located by NORMALIZED equality but then required to be BYTE-EXACT: a normalized-equal request that differs from the declared spelling only by a trailing slash (a directory alias — `pkg/dir` for a declared `pkg/dir/`, or vice versa) is ALSO refused `move_requires_exact_owned_path` (`details.declared_path` names the exact spelling to retry with). The accepted spelling is folded VERBATIM into the destination slice, so admitting the alias would drop the trailing slash the scope format uses to mark a directory and narrow the destination to a FILE path while the source directory is removed — so a directory move must name the owned path byte-for-byte (#2596 fixup).
- **Enumerated refusals, all pre-Submit (no approval row, no audit entry).** TWO new codes: `422 plan_slice_move_scope_files_requires_decomposed_plan` (a positively FLAT plan → `plan_not_decomposed`; a plan whose decomposition cannot be positively confirmed → `plan_indeterminate`, FAIL-CLOSED with the same universal no-carve-out posture as the add) and `409 plan_slice_move_after_dispatch` (a SOURCE or DESTINATION fan-out child past run state `pending` → `slice_already_started`; a `ListRuns` failure → `dispatch_state_indeterminate`, fail-closed — re-scoping a slice whose work has begun is the harm). Everything else reuses `400 validation_failed` with `details.field = move_scope_files_to_slice`: the shared key/path shape refusals, plus `path_under_two_slices`, `path_in_both_scope_channels` (the field composes with `add_scope_files_to_slice` in ONE approve but over DISJOINT paths), `path_not_in_declared_scope` (nothing to move — the message NAMES `add_scope_files_to_slice` as the channel that adds a net-new path), `move_requires_exact_owned_path`, `path_already_owned_by_destination` (a no-op move), and `move_would_empty_source_slice` (the last declared path of a source slice — an empty per-slice scope 409s at dispatch).
- **No cap headroom.** A move changes neither the effective-scope union nor its count, so it deliberately does NOT ride in `unionScopeAdds` and consumes zero `max_files_changed` headroom — an at-cap plan can still take a move.
- **Two-sided fold.** The DESTINATION side (`gained`) reaches BOTH surfaces the acceptance criterion names. The enforced implement scope, shown set, reviewer drift baseline, and trace provenance inherit it via `resolveApprovalAddScopeFiles` — the documented SOLE fold source — with no new call site. The agent-facing `ScopeConstraint.ScopeFiles`, which `resolveApprovalAddScopeFiles` does NOT reach (it folds only the enforced `scopeFiles`, and `trigger.ScopeConstraint` is set from `requireDecomposedScope` before that fold), gains `gained` directly in `resolveDecomposedScopeConstraint` — so the moved-in file is not enforced-in-scope yet absent from the narrowing the agent reads (#2596 fixup). The SOURCE side (`lost`) is the genuinely new machinery: `requireDecomposedScope` resolves BOTH `gained` and `lost` once and threads them into the two halves it returns — `resolveDecomposedScopeFiles` subtracts `lost` (before the coupled-`_test.go`-sibling fold, so a moved-away `foo.go` cannot drag `foo_test.go` back) and `resolveDecomposedScopeConstraint` subtracts `lost` AND adds `gained`. `resolveApprovalSliceMoves` returns `lost` as every path under every non-own key without re-reading the plan — subtracting a path a slice never owned is a harmless no-op.
- **Provenance conflation (accepted, documented).** Because the destination side routes through `resolveApprovalAddScopeFiles`, `trace.go` and the gate view label a MOVED-IN path IDENTICALLY to a genuinely ADDED one. This is deliberate (one fold source, no site to miss); the audit's `move_scope_files_resolved` `[{path, from_slice, to_slice}]` list is the ONLY place the true move provenance survives. An operator reading trace output alone sees a move as an add — read `move_scope_files_resolved` to disambiguate.
- **Reviewer baseline is whole-plan (no source-side review change needed).** The implement-review drift baseline for a fan-out child is the WHOLE-plan `scope.files` (the union of all slices): `trace.go::amendedScopeFilesForReview` keys on `approvedPlan.Scope.Files` and `writePlanForReview` renders it. A moved path is in that union both before and after the move, so the move is a NO-OP on the review/trace surface — the source-side subtraction lives only in the per-slice enforced scope (`requireDecomposedScope`), which the review side does not consult.

### Plan-stage-only recording, all four channels (#2598)

The "recordable from the PLAN STAGE alone" property above is now held UNIFORMLY by the four approve-time scope channels — `add_scope_files` (#824), `remove_scope_files` (#1726), `add_scope_files_to_slice` (#2515), and `move_scope_files_to_slice` (#2596) — plus a loader-side second wall.

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
  It (1) finds the awaiting-approval stage by listing runs for the repo and matching `trigger_ref = issue:N`, (2) authorizes the sender via the existing `role.Resolver.CanApprove`, (3) submits via `approval.Repo.Submit` with `Surface = SurfaceGitHubComment`, (4) advances the stage (`advanceStage(ctx, stage, …)` — the same compare-and-swap primitive the HTTP path uses, see above), (5) writes the same `approval_submitted` audit row the HTTP path writes, (6) calls `Orchestrator.Advance`, and (7) posts a reply comment via `Notifier.NotifySlashApprovalReply`.
- Replies are NOT deduped — every command attempt produces its own reply.
- **Over-cap approve-comment refusal + its breadcrumb (#2583, made observable by E67.24 / #2621).** An approve comment longer than `prompt.MaxApprovalConditionBytes` is refused BEFORE `Submit` (no approval row) on this channel too, because the comment is injected verbatim as a binding approval condition (#558) and must not be silently truncated. A REJECT is never refused — its comment feeds the advisory `PriorRejectionFeedback` channel, not binding conditions. On the reply-comment (`silent=true`) variant the refusal deliberately posts **NO reply**: a passer-by `+1` that happens to exceed the cap should not draw an unsolicited bot comment. BOTH variants append an `approval_comment_refused` audit entry (`CategoryApprovalCommentRefused`, registered in `audit.KnownCategories`) so the drop is visible in the run record rather than invisible — payload `{stage_id, decision, surface, approver, silent, reason:"over_cap_approve_comment", bytes, max_bytes, overflow_bytes}`, stage-anchored, actor `user`. The entry is a trace, not a response: the silent channel stays silent. Best-effort — an append failure WARN-logs and never unwinds the refusal into an admission.
- Per #253 / ADR-017 the slash command no longer reads `stage_check` state — branch protection is the merge gate.
- The slash-command path falls open when its deps aren't wired (`approvalCommandConfigured()` returns false), and is best-effort throughout: a comment failure logs but never returns 5xx to GitHub.
- PR-triggered approvals, `/fishhawk cancel`, and per-stage targeting (`/fishhawk approve plan`) are out of scope — separate followups when those scenarios surface.

## Live-validation walk auto-filing (`live_validation_filing.go`, E48.35 / #2045)

`fileOrLinkLiveValidationWalk`, a best-effort side effect of `finishApprovalAdvance` (fired on a plan approval, same posture as `fileSplitProposalChildren`): when the approved plan carries any `requires_live_validation` acceptance criterion, it auto-files an operator-validation walk `chore` work item and records a durable audit marker so the pending live check is surfaced on run status / gate view rather than shipped silently unvalidated. Every forge / work-item / audit error logs and returns — it NEVER unwinds the approval. A plan with no marked criterion no-ops with zero side effects.

- **Intent-marker-before-file idempotency + per-run lock.** A durable `live_validation_walk_intent` marker is appended BEFORE the forge call; on entry, if ANY intent marker is already present the hook no-ops (never a second walk). The list-then-append guard is non-atomic, so `lockLiveValWalk(runID)` serializes the intent-check → intent-append → file → linked-append section per run. **That lock is no longer what prevents production double-filing** — since #2656 the approve CAS refuses a raced second approval before either hook is reached, and the lock is retained as a subordinate backstop that still bites only where the CAS is bypassed (a direct caller). See *Approval-hook self-protection: one CAS, one subordinate lock* above for the decision, the measured evidence, and the residual. In-process only (single-daemon v0); a multi-instance deployment needs a Postgres advisory lock, mirroring `childNumberLocks` in `workitems.go`.
- **MANUAL RECOVERY — stranded intent marker (binding condition A(2), #2045).** The idempotency guard leaves one unhandled terminal state: if the process dies (or the forge call is never reached) BETWEEN the intent-marker append and the filing, the newest marker is a bare intent marker with no linked marker following it. The hook deliberately does NOT re-file it — a bare intent marker cannot distinguish crashed-before-file from filed-but-linked-write-failed without a forge query this hook avoids, and re-filing would reopen the double-file window. This rare case degrades to the pre-#2045 status quo: `liveValidationForRun` renders the stranded intent as the file-manually variant (`filing_failed=true`, `filing_incomplete=true` — never a healthy `walk: #X` and never a malformed empty ref), and the **operator files the walk by hand**. A filing-failure linked marker (`filing_failed=true`, empty ref) renders identically.
- **Companion-link, not epic-parent (implement-review fix).** The originating issue asks for the walk filed "under the epic", but the hook only knows the TRIGGERING issue — an E48.35 CHILD, not the E48 epic. Parenting the walk to that child is the wrong hierarchy, and resolving the child's own parent-epic issue number would need a sub-issue-PARENT forge query `githubclient` does not expose (only children-direction `ListSubIssues`). So the walk COMPANION-LINKS to the triggering issue with an explicit `{epic}=<issue>`/`{n}=1` title that always renders (`[E<issue>.1]`, mirroring `split_filing.go`), which neither mis-parents nor collides with the real epic's child numbering. Filing under the true epic is a **follow-up** (it needs that parent-resolution query).
- **Single deterministic filing.** One filing, not an epic-parented-then-companion-fallback pair: the two-attempt design opened a same-approval double-file window (a provider 502 AFTER the first `File` created the issue would still trigger the second attempt; the intent marker only dedupes ACROSS approvals). Any error now routes to the `filing_failed` linked marker, never to a second differently-shaped walk.

## On-approval grooming apply (`grooming_apply.go`, E54.19 / [#2822](https://github.com/kuhlman-labs/fishhawk/issues/2822))

`applyApprovedGrooming` is `workmgmt.ApplyGrooming`'s **production caller**, and the fourth best-effort side effect of `finishApprovalAdvance`'s plan-stage block (beside `fileSplitProposalChildren` / `fileOrLinkLiveValidationWalk` / `recordPlanPredictedRuntime`). When the approved plan stage carries a `grooming_report` artifact, it applies that report's HYGIENE-class mutations through the work-management provider and audits every one. Like its siblings it NEVER unwinds the approval: the gate already passed and its row is in place, so every failure logs and returns.

**Why the gate and not an agent stage.** The apply layer's eight containment rules ARE the authorization. Routing the call through an agent stage would put an agent between the operator's gate decision and the tracker write — the exact inversion the backlog-grooming workflow exists to prevent — and would need an implement-stage runner path that produces no diff. The gate approval was already the ratification signal (`campaign_grooming_source.go` reads the same predicate to build a campaign from a report); this makes it the apply trigger too, so the two surfaces agree on what "ratified" means by construction rather than by convention.

**What it authorizes.** Hygiene defects and dependency edges only — both route to the `hygiene` action class through the exported `workmgmt.GroomingActionClassFor`, the SAME map the derivation keys on, so a remap out of that class fails in `TestGroomingActionClassFor` rather than by silently widening what one approval authorizes. Ordering, duplicate, decomposition and vision-drift entries receive **no decision at all**, so the apply layer records each skipped `no_decision` and dispatches nothing. `GateApproved` is deliberately nil (it is the per-entry override, and this slice grants none) and `IceboxColumn` deliberately empty (icebox is a scoping kind with nothing to route). **Since #2855 that nil `GateApproved` ALSO means a hygiene entry proposing an `autonomy:` delegation-tier label is refused here unconditionally**: the apply layer's rule 8 reads that map, and this hook populates no entry in it — so a whole-report approval cannot set the label that decides whether an agent may drive an item at all. The entry is still fully REPORTED and the refusal is audited as `delegation_tier_not_authorized` carrying every proposed label, so the suggestion stays visible and resurfaces for a human. The per-entry disposition surface (#2843) authorizes such an entry by SUPPLYING `GateApproved` for it, needing no change to the rule. The ingest census (`groomingEntryCounts`, `grooming_report_recorded`) gains a **`delegation_tier_proposals`** key counting the hygiene entries whose `fix.labels` satisfy `workmgmt.LabelsSetDelegationTier` — the same predicate the refusal keys on — emitted **only when non-zero** (the milestone-counts precedent), so an ordinary report's audit payload stays byte-identical and the operator reading the gate sees both that the proposals exist and that none will land. Pinned by `TestApproveGroomStage_DelegationTierLabelNotApplied` (cross-boundary, pgtest) and `TestGroomingEntryCounts_DelegationTierProposals` (value + absent-when-zero, read back off the recorded payload). **Delegation is unchanged, structurally**: no file under `backend/internal/spec/` or `docs/spec/` is touched, so `ordering`/`dedup`/`scoping` remain refused at `mode: auto` by the two existing parse-time controls.

**The ladder is three rungs, each an explicitly deletable control.**

| # | Rung | Rule | Counterfactual test |
|---|---|---|---|
| C1 | **Decision** | A decision other than `approve` applies NOTHING (issue AC4) | `TestApplyApprovedGrooming_RejectDispatchesNothing` |
| C2 | **Report present** | *An early-out, NOT a control.* No `grooming_report` on the stage means an ordinary plan approval: return having written nothing. Behaviour-PRESERVING under deletion, so `TestApplyApprovedGrooming_OrdinaryPlanStageNoOps` is named a regression pin, not a counterfactual vehicle | — |
| C3 | **Re-ratification** | Re-read the stage's approval rows and require ≥ 1 grant and ZERO rejections — the identical predicate `campaign_grooming_source.go::approvedGroomingReport` ships | `TestApplyApprovedGrooming_ContestedGateDispatchesNothing`, `..._UngrantedGateDispatchesNothing` |

C1's placement is itself part of the control: the call sits in `finishApprovalAdvance`'s **type-only** plan block and is **passed** the decision, rather than nested inside the `DecisionApprove` block, precisely so its deletion is observable on the reject path. Nesting it would make "a rejected report applies nothing" structurally untestable, and therefore unproven. C3's assertions read **committed state** — the audit rows after the call returns — because the hook returns nothing and a control whose effect is a write is only observable in what was written.

**The submission is not the gate.** `approval.SubmitResult` carries only `{Approval, Inserted}` and no gate-satisfied flag, so satisfaction is not inferable from one approve — hence the re-read. RESIDUAL, stated: under this repo's `count: 1` gate, grant-and-no-rejection and gate-satisfied coincide; under a `count: 2` gate a single grant would satisfy this predicate before the gate itself is satisfied. That is the SAME bar the campaign source already ships, so the two stay consistent rather than diverging, and tightening both to a real count check is a follow-up that must move them together.

**Five named degrade reasons.** Past every rung, each resolution failure degrades with NO dispatch and writes exactly ONE server-authored `grooming_apply_completed` row carrying `{applied:0, failed:0, skipped:0, degraded:true, degrade_reason:<reason>}`, so an operator reading the trail can tell "the apply did not happen, and why" from "the apply happened and applied nothing". No `grooming_mutation_applied` row is ever written on a degrade path, so the churn guard's disposition baseline is untouched and the entries correctly resurface next run. The reasons: `grooming_apply_report_unparseable`, `grooming_apply_run_unreadable`, `grooming_apply_conventions_unavailable`, `grooming_apply_mutator_unavailable`, `grooming_apply_reader_unavailable`, plus `grooming_apply_not_ratified` (C3) and `grooming_apply_repo_unresolvable`. One case per reason in `TestApplyApprovedGrooming_DegradeModes` and its two siblings. An unreadable ARTIFACT LIST is deliberately silent rather than a degrade — it is indistinguishable from "this stage never had a report", and degrading every ordinary plan approval into a grooming audit row would be worse than the silence.

**The audit payload shape is load-bearing** ([#2813](https://github.com/kuhlman-labs/fishhawk/issues/2813)). `groomingApplyAuditSink` marshals the `workmgmt` record **BARE** — its own json tags, NOT wrapped in a run/stage envelope — because `grooming_report.go`'s `priorGroomingDispositions` decodes `entry_id` / `outcome` / `skip_reason` directly off the payload and the churn guard's whole idempotence baseline reads it. Run and stage identity ride the audit row's own columns. `TestApproveGroomStage_AppliesHygieneAndAuditsEndToEnd` feeds the written rows back through the UNCHANGED `priorGroomingDispositions` on a `pgtest` database — the seam per-layer units cannot cover, and the assertion that the payload the sink writes is the payload the guard reads.

**Detached and bounded.** The hook runs SYNCHRONOUSLY on the operator's approve request, so the apply executes under `context.WithTimeout(context.WithoutCancel(ctx), groomingApplyBudget)` (3m) — the `acceptance_admission.go` precedent. `WithoutCancel` keeps the request's values but drops its cancellation, so a client disconnect cannot strand a half-applied report mid-loop; the timeout bounds a wedged forge so it cannot hold the approve request open. Both halves are pinned: `TestApplyApprovedGrooming_BoundedContext` (timescale-derived, no raw elapsed bound) and `..._DetachedFromRequestCancellation`.

**An unreadable spec authorizes nothing.** `groomingModesForRun` projects the resolved autonomy matrix onto `map[string]workmgmt.GroomingMode`; an unparseable spec or absent autonomy block yields an EMPTY map, `workmgmt.ResolveGroomingMode` normalizes an absent class to `gated`, and the destructive rule then requires `auto` + an approved entry (or an explicit per-entry gate approval), neither of which this slice supplies.

**Known operational residual.** This repository's Project #7 is USER-owned, and a GitHub App installation token cannot reach a user-owned Projects v2 board — board placement routes onto `FISHHAWKD_PROJECTS_TOKEN` instead ([#1114](https://github.com/kuhlman-labs/fishhawk/issues/1114)). With that token unset, the `unboarded` and `missing_estimate` hygiene defects dispatch and are recorded outcome `failed`, not `applied`. That is correct continue-and-report behaviour, not a defect: label-set and epic-link mutations land first. It doubles as a **kill switch without a deploy** — leave the token unset, or reject the groom gate instead of approving it.

## Per-entry grooming disposition capture (`grooming_dispositions.go`, E54.30 / [#2843](https://github.com/kuhlman-labs/fishhawk/issues/2843))

`POST` / `GET /v0/runs/{run_id}/grooming-dispositions` record and read back an operator's verdict on INDIVIDUAL entries of a run's grooming report, keyed by the entry's stable DERIVED id. Each disposition persists as one chained `grooming_disposition_recorded` audit row.

**The category is deliberately distinct from `grooming_mutation_applied`.** This row is what the OPERATOR DECIDED; that one is what was APPLIED, and the second derives from the first. Collapsing them would make "the operator approved this and the apply then failed" indistinguishable from "the operator never decided" — which is exactly the state the churn guard's baseline (`priorGroomingDispositions`) reads. `TestGroomingDispositionAuditCategoryDistinct` pins the literal category string, its registry membership, and that capture writes ZERO apply-family rows.

**Nothing consumes these dispositions.** This slice is CAPTURE ONLY. Recording an approval applies nothing, closes no duplicate and re-ranks no backlog. The consumption half — the apply stage, the watermark/ordering/batch-atomicity concurrency protocol between capture and apply, and unlocking the gated destructive classes — is [#2991](https://github.com/kuhlman-labs/fishhawk/issues/2991), and this file specifies no consumption ordering. A row written here is inert, forward-compatible audit history. That is also WHY the concurrency protocol is absent rather than deferred: with nothing consuming dispositions, capture and apply do not race for the same gate, so the race has no incorrect outcome to produce.

**Which report a capture attaches to.** `newestGroomingReportArtifact` resolves the run's NEWEST `grooming_report` artifact — the maximum by `(CreatedAt, ID)` across the run's plan stages, a TOTAL order so two artifacts written in the same clock tick still order stably regardless of repository return order. BOTH verbs resolve through it, which is what makes `POST` and `GET` agree on WHICH report by construction rather than by convention. The resolved `artifact_id` + `content_hash` come back in the response and ride on every audit payload, so a later consumer can still distinguish captures across artifacts. It keeps three outcomes distinct exactly as `priorGroomingReport` does — found / genuinely absent / read-or-parse failure — because collapsing absent and unreadable is how a capture would silently attach to the wrong report. `TestGroomingDispositionsNewestArtifactWins` seeds two artifacts and fails if either verb attaches to the older one.

**The valid-entry-id set has one owner.** `plan.GroomingEntryIDs` / `plan.GroomingEntryClasses` are thin exports over the EXISTING unexported `collectGroomingEntries` — the same collector `groomingSemanticCheck` walks — so the ids capture accepts cannot drift from the ids `ValidateGroomingReport` already proved the report declares. A class added to the report domain without being routed through the shared derivation fails in `plan`'s own table test, not here.

**The ladder is eight rungs, every one evaluated BEFORE any write.**

| # | Rung | Status / code | Counterfactual test |
|---|---|---|---|
| G0 | Repositories unwired | `503 grooming_dispositions_unconfigured` | `TestGroomingDispositionsUnconfigured` |
| G1 | Anonymous | `401 authentication_required` | `TestGroomingDispositionsAnonymousRejected` |
| G2 | Run-bound agent token (`mcp:run:<uuid>`), even for its OWN run | `403 run_token_forbidden` | `TestGroomingDispositionsRunBoundTokenRejected` |
| G3 | Delegated operator-agent token (`operator-agent/` prefix) | `403 operator_agent_forbidden` | `TestGroomingDispositionsOperatorAgentRejected` |
| G4 | Missing `write:approvals`, enforced unconditionally | `403 insufficient_scope` | `TestGroomingDispositionsMissingScopeRejected` + `..._ScopeRungIsReachableThroughTheMux` |
| G5 | Unparseable body, TRAILING CONTENT after the object, empty batch, empty `entry_id`, intra-batch duplicate | `400 validation_failed` | `TestGroomingDispositionsEmptyBatchRejected`, `..._IntraBatchDuplicateRejected`, `..._EmptyEntryIDRejected`, `..._MalformedBodyRejected`, `..._TrailingContentRejected` |
| G6 | Verdict outside the closed `workmgmt` set | `400 grooming_verdict_invalid` | `TestGroomingDispositionsInvalidVerdictRejected` |
| G7 | No report vs an unreadable report | `409 grooming_report_absent` vs `500 internal_error` | `TestGroomingDispositionsNoReportRejected`, `..._ReportUnparseableIs500`, `..._StageListErrorIs500` |
| G8 | An `entry_id` the newest report does not declare | `422 grooming_entry_unknown` | `TestGroomingDispositionsUnknownEntryRejected` (identity) + `..._UnknownEntryLeavesNoRows` (committed state) |

**Batch-atomic validation.** The WHOLE batch is validated before ANY row is appended, so a request naming one unknown `entry_id` records NOTHING and a partially-recorded capture is unreachable. This control's effect is COMMITTED STATE, not error identity: the 422 bytes are byte-identical whether or not the first disposition leaked into the chain, so the counterfactual vehicle is `TestGroomingDispositionsUnknownEntryLeavesNoRows` (pgtest-backed), which READS the rows after the call returns and asserts zero. Moving the append inside the per-entry loop leaves the identity test GREEN and reddens only that one — verified empirically, not asserted.

**The body must be ONE JSON document.** `json.Decoder` stops after the first value, so a body of two concatenated batches decoded the first, SILENTLY DISCARDED the second, and returned `200` — a success response for a capture that recorded half of what the operator sent, which is the exact failure shape this endpoint exists to prevent. G5 now re-Decodes after the first value and refuses anything but `io.EOF`, ahead of every write. Trailing WHITESPACE is still accepted: a trailing newline is what every curl heredoc and most HTTP clients send, and refusing it would break every real caller to fix a synthetic one. `TestGroomingDispositionsTrailingContentRejected` tables all five shapes (second object, garbage, array, newline, whitespace) and asserts on COMMITTED STATE — zero appended rows on each refusal, exactly one on each acceptance.

**The mid-batch append failure is the one place the batch-atomic guarantee is knowingly weakened**, and it is now pinned. Rows appended before an `AppendChained` failure ARE durable, so the `500 internal_error` reports `{recorded, requested}` — the operator's only evidence of what survived. `TestGroomingDispositionsMidBatchAppendFailure` wraps the REAL Postgres audit repository in a decorator that fails the k-th disposition append and passes every other call through, then asserts the counts OFF THE SHIPPED RESPONSE BYTES, that `recorded` equals the rows that actually persisted, that the failed entry did NOT persist, and that the documented recovery holds (re-POST the batch, last-wins resolves to the retried verdicts, all five rows stay in the chain). Asserting on the details map handed to `writeError` instead would have hidden the defect that test found: `recorded` / `requested` were NOT members of `redactableDetailKeys`, so the 5xx allow-list dropped both and the shipped body carried NO count at all while the handler comment claimed it did. Both keys are now allow-listed (product-owned integers computed from the caller's own batch, the `failed_ordinal` precedent). The read-back's `ListForRunByCategory` failure (`..._ReadBackListErrorIs500`, which also asserts the capture itself LANDED — the failure is on the projection read, and that distinction decides whether an operator should retry) and the GET's `run_id` parse (`..._GetInvalidRunIDRejected`) are pinned alongside.

**Last-wins supersession.** An `entry_id` may not repeat WITHIN one request — one request carrying two verdicts for one entry states no verdict, so it is `400 validation_failed`, not a supersession. A LATER request on the same entry DOES supersede, collapsed last-wins by audit sequence. Both rows stay in the chain, so the correction is itself auditable; refusing the repeat instead would make an operator's corrected verdict unrecordable. `TestGroomingDispositionsReadBackLastWins` fails if the read-back returns the earlier verdict or if either row is missing.

**The projection is tolerant in the fail-safe direction.** `projectGroomingDispositions` skips a row whose payload cannot be decoded — contributing no disposition, mirroring `priorGroomingDispositions` — and keeps only rows whose `artifact_id` matches the resolved artifact, so a capture against an older report never leaks into a newer report's read-back. Output is sorted by `entry_id` for a deterministic body. Both branches are pinned by `TestProjectGroomingDispositions_SkipsUndecodableAndForeignRows`, whose foreign-row fixture carries a DISTINCT `entry_id` on purpose: sharing one would let the last-wins collapse absorb it and the case would stay green with the artifact filter deleted.

**Operator-only means TWO refusals, and the second is the point.** G2 mirrors `merge_run.go` and `vouch.go`. G3 goes beyond what the issue asked: the grooming report is AGENT-AUTHORED, so an agent that could also disposition it would convert an operator gate into a rubber stamp. It is keyed on `operatorrole.IsTokenSubject` — the same predicate `actor.go` already uses to classify a delegated writer as `actor_kind=agent` — so it reuses the existing notion of agent identity rather than inventing one, and if that prefix convention changes, actor classification and this refusal move together. G4 mirrors `merge_run.go`'s posture verbatim: `write:approvals` enforced with NO cookie-session bypass, because a disposition is an approval-class judgment.

**The READ is not operator-only.** The issue states the requirement for CAPTURE, and the underlying `grooming_report` artifact is already agent-readable, so a refusal on the read would add a failure mode with no corresponding hazard. The `GET` is registered at `readAccess`.

**Delegation is unchanged, structurally.** No file under `backend/internal/spec/` or `docs/spec/` is touched, so `ordering` / `dedup` / `scoping` remain refused at `mode: auto` by the two existing parse-time controls — re-asserted by `TestGroomingRegistry_DispositionCaptureDoesNotWidenDelegation`.

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
  `RequiredChecksSnapshot` is captured in `CreateRunForTrigger` via `captureRequiredChecks` (#2506), from the best-effort App installation resolved at run-create (#713) — closing the #2497 gap that left local/MCP/CLI/campaign runs permanently at `checks_unresolved`. It resolves the repo's REAL default branch and threads it into `webhook.ResolveRequiredChecks` so a `~DEFAULT_BRANCH` ruleset on a non-`main`-default repo resolves correctly. Fail-safe: every degrade (no GitHub client, no installation, unsplittable repo, default-branch lookup failure, missing `administration:read`, transport error, or a ruleset condition the matcher cannot authoritatively evaluate) leaves the snapshot NIL — never a present-but-empty one, which is a positive "this repo requires nothing" claim (the `nil` = greenness-unknown vs present-but-empty = nothing-required contract, `aggregateCIGreen` above). Freshness contract: resolved ONCE at run-create, never re-resolved; GitHub's protected-branch enforcement remains the merge-time authority (a stale-green snapshot can't force a merge — the API refuses it, which `merge_run` classifies as a wait per #2722).
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
- **Endpoint-side idempotence** (binding condition, #1954): a repeated POST that finds an existing `merge_verdict_recorded` row appends NO duplicate and responds `already_recorded:true`, but ALWAYS re-dispatches the merge helper — so a `502`-then-reinvoke re-queues the merge without ever duplicating the verdict. On a merge-helper error the handler branches on the cause: a checks-not-all-passed refusal (`forge.ErrPullRequestUnstableStatus` — GitHub reports the PR in UNSTABLE status, E67.56 / #2717) returns `409 merge_checks_pending` (`details` `{verdict_sequence, pr_url, reason:"checks_pending"}`) — an expected precondition, not a fault, whose message says the required checks have NOT all passed, that an immediate retry cannot succeed, and that a check which has already FAILED means inspecting the PR rather than waiting; EVERY other error returns `502 merge_dispatch_failed` stating the verdict row is durable and the queue step is retryable, so a genuine dispatch failure is never masked as "just waiting". The verdict row is durable across a `merge_checks_pending` refusal, so `fishhawk_merge_run` re-POSTs across a bounded wait with no duplicate row. Response `{run_id, merge_queued, verdict_sequence, already_recorded, pr_url}`.
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

#### `mode: report` per-class occurrence key (`autodrive.go`, E52.15 / #2337)

The report arm (`reportMatrixProposals`) emits an `act:"report"` `run_auto_driven` row **at most once per gate occurrence per class**, deduped on an opaque occurrence key. `reportGateOccurrence` returns the BASE key (the anchoring stage + its state, `stageOccurrence`; or `run:<id>:merge_ready` for the run-level merge gate) and its anchor stage; `reportOccurrenceKey` then folds a **per-CLASS re-occurrence discriminator** onto the base, dispatched on the ACTION (not the anchor's type). The base holds duplicate poll cycles to one row (the flooding guarantee); the discriminator makes a genuine re-opening a distinct occurrence so a materially-changed gate re-surfaces its proposal.

| Class | Discriminator | Counter |
|---|---|---|
| `approve` / `route_fixup` / `waive` | review round | `reviewRoundCount` — count of the anchor stage's `*_review_started` entries |
| `retry` | stage retry count | `stageRetryCount` — `stage_retried` + `stage_override_retried` entries **keyed to the anchor stage** |
| `merge` | ready-transition proxy | `mergeReadyRoundCount` — the run's `approval_submitted` count |

- **Why `approval_submitted` is a sound merge ready-transition proxy.** `mergeGateReady` requires that NO stage is parked in `awaiting_approval`, and an `approval_submitted` row can only be appended against a stage that IS parked there (both append sites — `approvals.go` and `issue_approval.go` via `findAwaitingApprovalStage` — resolve a stage at the approval gate first). So the count cannot advance during a continuously-merge-ready window (the flooding guarantee holds) and DOES advance across the un-ready/re-ready cycle a re-opened gate produces.
- **Why `stageRetryCount` filters on stage ID.** `ListForRunByCategory` is RUN-scoped, so an unfiltered count would let a SIBLING stage's retry advance this stage's key and emit a second row inside one occurrence — the flooding direction. The filter mirrors `countFixupPasses` (`fixup.go`). ORDERING (retry.go:492): `retryStageAs` calls `run.RetryStage` (the state flip) BEFORE writing the retry receipt, so a poll that reads count=N also sees a stage that has left `failed` for the Nth time — the count advances across distinct FAILURES, never within one continuously-failed span.
- **Degrade branches — all fall back to the BASE key (under-emission, never a flood).** Each counter returns `ok=false` on a read error or a zero/absent count, and `reportOccurrenceKey` then keeps the base key: `reviewRoundCount` (nil anchor, a stage type with no review surface, zero rounds, an audit read error); `stageRetryCount` (nil anchor, either category read errors, zero total); `mergeReadyRoundCount` (read error, zero approvals). An unreadable discriminator suppresses a re-surface rather than flooding — the safe direction.
- **Known limitation (residual, from the amended #2337 criterion).** A merge gate that re-becomes ready WITHOUT an intervening approval — a gate re-closed by a retry or a fix-up re-dispatch — keeps the same `approval_submitted` count, so its key does not advance and it **under-emits**. Closing that needs a new audit category recording a stage entering/leaving `awaiting_approval`; summing today's available signals would instead break the flooding guarantee (`mergeGateReady` ignores failed stages, so a `stage_retried` row can land while the gate is continuously ready), so it is deliberately deferred. This is a known limitation, not completeness.

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

- The pass pages EVERY NON-TERMINAL run state (`reconcileOrphanedReviewStates` = `pending` + `running`, one `ListRuns` walk per state, healed runs de-duplicated by id) and, per review stage (plan + implement), reads the LATEST `*_review_started` anchor (payload `ReviewStartedPayload{ConfiguredAgents, Authority}`, entry carries `StageID`).
- **Attempt correlation**: a stage accumulates several review rounds (a fix-up re-triggers the review, appending a fresh `*_review_started`); the CURRENT attempt is the latest started entry.
  Landed terminals are counted ONLY with audit sequence strictly greater than that started entry's sequence, and that SAME entry's timestamp drives the boot-marker comparison — never the earliest started nor a run-wide terminal count (which would mix a prior round's landed verdicts into the current tally).
- **Boot marker**: `Server.processStart` (in-memory, stamped once at `New` from `Config.ProcessStart`, default `time.Now()`) is the reference the pass compares the latest started entry's timestamp against — a review whose latest start predates the boot belongs to a dead prior process; one that does NOT is a review THIS process legitimately still has in flight and is spared.
  At startup `processStart == now`, so every un-terminated started entry predates it; the comparison is load-bearing only if the pass is ever invoked mid-process-life (a periodic sweep is a deferred follow-up, not wired here).
- When a review's latest round predates the boot with `landed < ConfiguredAgents`, it emits exactly `ConfiguredAgents - landed` terminal `*_review_failed` entries via the existing `emitReviewFailed` helper (`Timeout:false`, a restart-naming reason), driving `landed == ConfiguredAgents` so `reviewStatusFor` flips from `pending` to a terminal `failed` status `await_review` resolves on.
- Synthesized entries carry a placeholder model (`""`) / the round's authority — a documented fidelity limitation: the dead goroutine's per-reviewer model state is gone, but `reviewStatusFor`/`await_review` treat any `*_review_failed` as terminal regardless of model.
- After healing an implement review it republishes `fishhawk_audit_complete` (`recomputeAndPublishAuditComplete`, mirroring `trace.go`'s implement-review path) so the #947 review-pending presence gate reflects the now-terminal state.
- Best-effort PER RUN (a per-run error is logged and skipped); only a systemic `ListRuns` paging failure aborts. Reuses existing repo methods + terminal writers only (no new query, no schema change).

#### Why `pending` is load-bearing in the swept state set (#2712)

The sweep originally filtered on `State=running` ALONE, which made the **plan-review half of #1781 dead code**. A run is created in `run.StatePending` (`runs.go`); on the GATED plan path `advancePlanStageTerminal` (`plan.go`) parks the plan stage at `awaiting_approval` WITHOUT calling `Orchestrator.Advance`, so the first `pending → running` transition happens at plan APPROVAL (`approvals.go`). Every plan review is therefore dispatched while its run is still `pending`: a restart mid-plan-review left a strand the boot sweep could never see, and the round stayed `pending` forever with no recovery short of hand-writing audit rows. `ListRuns` takes a single equality string, so "not terminal" cannot be expressed as a filter; the set is enumerated and pinned against `run.State.IsTerminal()` by `TestReconcileOrphanedReviewStates_CoversEveryNonTerminalState`, so a future non-terminal state cannot be silently omitted.

#### On-demand recovery: `POST /v0/runs/{run_id}/reviews/reconcile` (`review_reconcile_http.go`, #2712)

The boot sweep only runs AT BOOT, so an operator whose review was ALREADY orphaned had to restart the daemon a second time to clear it. `handleReconcileRunReviews` runs the SAME per-run helper on demand (`adminWrite` via the route wrapper, plus the handler's own authenticated-identity + `write:runs` check, mirroring `reap-failure`).

- **No run-state filter.** That is the whole point relative to the sweep: the caller names the run.
- **The boot-marker gate is KEPT**, and with no state filter it is the control that stops the verb terminating a review THIS process legitimately still has in flight. A round whose latest `*_review_started` entry is not before `s.processStart` reports `skip_reason: review_dispatched_by_this_process` and nothing is written. Pinned by `TestReconcileRunReviews_Endpoint_RefusesLiveRound`, which seeds the post-boot timestamp BY CONSTRUCTION over a real `pgtest` database and asserts ZERO persisted `plan_review_failed` entries — deleting the gate reddens it on committed state, not on fixture setup.
- **Idempotent.** Only the MISSING count is synthesized, so already-landed verdicts are never re-paid for; a second call reports `round_already_settled`.
- **Serialized per run.** That idempotency rests on a read-then-append sequence (count the round's landed terminals, append exactly `ConfiguredAgents - landed`) which is NOT atomic on its own: two concurrent POSTs — or one racing the boot sweep — can both observe the same shortfall and each synthesize it, persisting MORE terminal entries than the round configured. `reconcileRunOrphanedReviewsLocked` holds a per-run stripe lock (`reconcileEmitLockFor`, mirroring `reportEmitLockFor` in `autodrive.go`) across both stages' count-and-append, so a later caller re-counts AFTER the first append and takes the settled skip; the `fishhawk_audit_complete` republish runs OUTSIDE the lock (idempotent on its own, and it must not extend the critical section). `TestReconcileRunReviews_Endpoint_ConcurrentCalls_SynthesizeOnlyMissing` fires four simultaneous POSTs at one orphaned round over a real `pgtest` database and asserts exactly the missing count is persisted — deleting the lock reddens it on committed state (observed 4–6 rows for a 2-reviewer round). Residual, stated honestly: this is an in-PROCESS guarantee. Two fishhawkd processes sharing a database could still double-synthesize; a cross-process guarantee needs a DB-level conditional write (advisory lock or a uniqueness constraint on the synthesized row). The single-daemon race is the one an operator can trigger by double-clicking the verb.
- **Structured counts, not a bare bool.** `reconcileStageOrphanedReviews` returns `reconciledStage{Stage, ConfiguredAgents, LandedBefore, Synthesized, Skipped, SkipReason}` and each gate records its reason (`no_review_started_entry`, `no_configured_agents`, `started_entry_has_no_stage_id`, `review_dispatched_by_this_process`, `round_already_settled`), so an operator sees WHY nothing was healed instead of a silent no-op. Every gate is otherwise byte-for-byte the #1781 sweep's.
- Surfaced as the `fishhawk_reconcile_reviews` MCP verb (`backend/internal/mcpserver/README.md`).

#### `/healthz` `process_start` (`handlers.go`, #2712)

`healthResponse` publishes `process_start` — `s.processStart` as RFC3339Nano UTC, `omitempty` on a zero marker. It is an ADDITIVE SIBLING of `start_nonce` (#1018), not a replacement: the nonce proves listener IDENTITY but is opaque and unorderable, whereas deciding whether a review's dispatching process is still alive requires COMPARING the boundary against an audit timestamp. The omission on a zero marker is load-bearing on the consumer side — a zero time compares as before every audit entry, so a client that could not distinguish "absent" from "zero" would classify every pending review as stranded. The MCP client therefore reports `ProcessStartOK` (present AND parseable) and treats every other outcome as undecidable.

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
- **Idempotent-replay terminal self-heal (#2630).** The dedup short-circuit (a `GetByHash` hit) already reuses `ensureGovernanceAuditEntry` (#1396) to backfill a missing `pull_request_opened` entry; it ALSO now drives `advanceImplementStageAfterPR` under EXACTLY the create path's guard — `stage.Type == implement && stage.State == running` — before writing the `200 idempotent:true`. Without this, a re-ship of an already-persisted artifact healed the audit entry but left a still-`running` implement stage un-terminalised: the terminal transition rode ONLY on the create-path PR-open event, so an idempotent-suppressed upload settled nothing (the #2630 amplifier that turned a sticky re-entry into a permanently stranded run). The guard keeps this safe WITHOUT re-running the create path's gating-reject and lineage checks, because both leave the stage terminally `failed` — never `running` — so a stage that failed either can never reach the advance. A replay against an already-terminal stage fails the running guard and is an unchanged no-op (no double transition; the second `Orchestrator.Advance` the gateless success path fires never runs). The response body is byte-unchanged.
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

## Plan-stage terminal settle is shared across artifact siblings (`grooming_report.go` / `plan.go`, E54.29 / #2837)

`handleGroomingReport` ingested, persisted, audited and churn-checked a `grooming_report` and then returned WITHOUT transitioning the stage, so a groom stage sat in `running` forever and its approval gate never opened (#2837). The fix REUSES the shared terminal settle rather than hand-rolling a per-sibling copy: `handleGroomingReport` calls `advancePlanStageTerminal` (`plan.go`) on BOTH success paths — the fresh-create path and the idempotent-retry path — which already owns the gated (`running → awaiting_approval`) and gateless (`running → succeeded` + orchestrator `Advance`) arms plus the sticky status-comment notify.

- **The settle is SHARED, not per-sibling.** `advancePlanStageTerminal` is THE plan-stage terminal settle for every artifact sibling that settles to the approval gate — today the plan artifact (#603) and the `grooming_report` (#2837). `clarification_request` is the deliberate EXCEPTION: it parks at `awaiting_input`, owned by `handleClarificationRequest`. A new sibling whose ingest settles to the gate MUST reuse `advancePlanStageTerminal`; #2837 (and #2833 before it, where the clarification sibling was special-cased by name) exist because a sibling was NOT wired to the shared settle.
- **Placement is fail-closed.** Both settle calls sit AFTER every validation guard and every storage-500 return, so an invalid or wrong-stage-type report fails category-B and NEVER reaches the gate, and an artifact-create / audit-append / churn-append failure leaves the stage in `running` for the runner's retry to heal — a not-durable report is never advanced to an approvable state. The retry path re-settles (a same-state re-application is a valid no-op via `ValidStageTransition`'s `from == to` short-circuit), so a retry after a partial first write cannot re-strand the stage.
- **No `notifyPlanReadyIfReady` on the grooming path.** `notifyPlanReady` resolves the run's PLAN artifact via `tryLoadPlanForRun`, which filters `a.Kind != artifact.KindPlan` (`prompt.go`), so on a grooming run it finds no plan and no-ops. `advancePlanStageTerminal`'s own `notifyStatusUpdate` is the operator-visible surface; a grooming-specific report-ready comment is a candidate follow-up.
- **A two-layer guard forces a future sibling to declare its settle.** (1) `plan.AllArtifactKinds()` + `TestAllArtifactKinds_EnumeratesEveryDeclaredConst` (parses the plan package's own non-test source across every file, not just `plan.go`) fails when an `ArtifactKind` const is declared without being enumerated. (2) `TestShipPlan_EverySiblingSettlesItsStage` iterates that enumeration — not its own table keys — driving every kind through the real signed router and `t.Fatalf`-ing on a kind with no declared settle row. So a fourth sibling cannot land without declaring the state its ingest settles to.

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

### Mid-stage scope-amendment deadline observability (`scope_amendment.go`, #2540)

A scope amendment filed near the agent wall clock is **undecidable by construction**: the implement agent's ~15-minute wait-poll window can outlive the stage's remaining agent runtime, so the stage is SIGKILLed while the agent is still polling and a correctly-approved amendment is lost with the whole pass. `handleRequestScopeAmendment` derives the executing implement stage's **remaining agent wall clock** — the spec-resolved `agent_timeout_seconds` (`resolveAgentTimeout`, the same budget the runner enforces) minus the stage's elapsed time — via the pure `amendmentDeadlineRemaining`, and when it can, surfaces it OBSERVABILITY-ONLY on the created row's REQUEST response + `scope_amendment_requested` audit as `stage_deadline_seconds_remaining` + `amendment_poll_window_seconds` (`AmendmentPollWindowSeconds`, 900), so an operator can **see** whether a race against the deadline is already lost. The row is always **created** and `pending` — a late decision plus `fishhawk_retry_stage` still folds the paths, and the runner's `detectUndecidedScopeAmendments` still names them on the failure path.

- **There is DELIBERATELY no refusal (#2540 approval condition 1).** An earlier design emitted an `undecidable_before_deadline` flag that told the agent to skip its wait-poll; it was struck because the remainder is uncertain in the **pessimistic** direction (`started_at` precedes the real agent clock, and an operator retry can leave a cumulative `started_at`), so a computed "too short" can be a false positive — and refusing a WINNABLE amendment, killing a stage that would have succeeded, is strictly worse than the bug it guarded against (which merely loses an already-unwinnable amendment). The numbers are **displayed, never acted on**; no wire/audit/prompt signal tells the agent to abandon a wait.
- **`amendmentDeadlineRemaining` still gates whether the numbers are shown, failing open in four named modes** (a misfire now costs at most a hidden number, never a killed amendment): **(a)** `budgetSeconds <= 0` (unresolvable spec budget — `resolveAgentTimeout` is fail-open); **(b)** `startedAt == nil` (never-started stage); **(c)** `remaining <= 0` (elapsed exceeds the whole budget — a live agent posting proves the deadline has not literally passed, so this is a bad derivation, not a passed deadline); **(d)** `selfRetryCount > 0` (`stages.started_at` is `COALESCE`'d and never overwritten, so a re-spawned stage's elapsed is cumulative and its remainder understated; an operator `fishhawk_retry_stage` that does not bump `SelfRetryCount` leaves the same cumulative `started_at`, indistinguishable from a first run — precisely why no refusal is built on the remainder). Each mode is pinned by a row of `TestRequestScopeAmendment_DeadlineFailOpen` asserting both numbers absent (struct AND raw-body key) and the audit key set exactly `{amendment_id, paths, reason, remaining_budget}`; `TestRequestScopeAmendment_TightRemainderNeverRefuses` pins that even a tight remainder surfaces the numbers without any refusal signal.
- **`AmendmentPollWindowSeconds` (900) is one of four copies of the window** (this constant, the prompt's ~15-minute text, `mcpserver`'s `amendmentPollWindowMinutes`, and the `mcpserver`/`scopeamendment` READMEs). It is now an OBSERVABILITY figure on every surface. The server↔mcpserver copies are pinned equal by a cross-package test (`backend/internal/mcpserver/stage_wait_test.go`), so a desync reddens rather than silently showing an operator two different windows.

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

### Forge-state (query-before-file) idempotency for split-child filing (`split_filing.go`, E50.7 / #2064)

`fileSplitProposalChildren` dedups on the **audit log** — a per-ordinal `work_item_filed` marker written immediately AFTER `provider.File`, with a hard abort when that append fails. The residual is the interleaving where **File SUCCEEDS and the marker append then FAILS**: the ordinal has no durable record, so a re-approval re-files a DUPLICATE child. #2064 closes it with FORGE STATE.

- **Stamp, then query before filing.** Every filed child carries a deterministic hidden idempotency marker (`splitfiling.SplitChildKey(runID, phaseIndex)`, routed through `workmgmt.FilingRequest.IdempotencyKey` so `Apply` renders it into the body and the provider carries it onto the create request). Before filing any un-recorded ordinal, `loadAdoptableSplitChildren` queries the parent's children **exactly once** through the existing `workmgmt.EpicChildrenQuerier` seam; a match is **ADOPTED** — its number and the forge's own url fill the in-memory `filed` map and the missing durable marker is written — instead of re-filed.
- **The adopted url comes from the forge (`EpicChild.URL`), never composed.** `githubclient.Client.BaseURL` is configurable, so a literal `https://github.com/{owner}/{repo}/issues/{n}` would be wrong on a GitHub Enterprise Server host. `TestSplitFiling_MarkerAppendFails_ReapprovalAdoptsInsteadOfRefiling` asserts the adoption-pass url is **byte-identical** to the url the creation pass recorded for that same child.
- **The child path fails CLOSED; the comment path fails OPEN. The asymmetry is chosen.** A children-query ERROR aborts the pass before ANY creation (no completion marker, run resumable) — filing blind on an unreadable child set is precisely what creates the duplicate this change removes, so an unknown forge state must never be read as an empty one. A parent-thread LISTING error, by contrast, posts the comment anyway: a MISSING operator-facing acceptance-carrier or refusal comment is worse than a duplicated one, whereas a duplicate child ISSUE is the bug being fixed. Both directions are documented at the call sites and pinned by `TestSplitFiling_QueryErrorAbortsBeforeAnyCreation` and `TestSplitFiling_CommentListErrorPostsAnyway`.
- **Duplicate-key tie-break: LOWEST NUMBER WINS — an intentional choice, not iteration order.** Two children can bear the same key only after a prior CONCURRENT re-approval. The lowest-numbered child is the earliest-created, so every later pass converges on the same one regardless of the order the forge returns them in; a first-match-wins tie-break would let two passes adopt different children and record different numbers for one ordinal. **The second child is NOT reconciled** — nothing closes or links it, and an operator resolves the duplicate. `TestSplitFiling_DuplicateKeyTieBreakIsDeterministic` pins it.
- **Parent comments are STAMPED on the way out, and that has its own counterfactual.** Both `postSplitParentComment` (acceptance carrier) and `postSplitRefusalComment` (#2412) stamp their outbound body and skip the post when the thread already carries that key. The read half can only ever find a marker the write half put there, so an unstamped outbound comment would leave the dedup **silently inert in production** while every reader-side test (which SEEDS a keyed comment) still passed. `TestSplitFiling_ParentCommentCarriesIdempotencyMarker` / `..._RefusalCommentCarriesIdempotencyMarker` assert on the body the fake server RECEIVED; deleting either stamp reddens them.
- **The comment dedup reads the WHOLE thread.** `TestSplitFiling_CommentDedupReadsWholeThread` seeds page 1 with only unrelated comments and puts the keyed comment on **page 2** behind a `rel="next"` Link header, so the zero-new-posts assertion can hold only for a reader that follows the link. Deleting the pagination follow in `githubclient.ListIssueComments` reddens it with a duplicate post. `TestSplitFiling_CommentDedupToleratesCRLF` covers the `\r\n` round-trip (its fixture appends a trailing newline **before** converting line endings — otherwise the trailing marker line carries no `\r` and the case cannot discriminate).

**Three honestly-narrowed claims — read these rather than the headline.**

- **At-most-once is SEQUENTIAL-only.** `EpicChildren → File` and `ListIssueComments → PostComment` are not atomic, so two CONCURRENT re-approvals can both observe nothing and both create: a named residual of **at most one duplicate**, deliberately not serialized (a leaked distributed-lock session wedging the filing path entirely is a worse failure). `TestSplitFiling_SequentialReapprovalFilesAtMostOnce` claims only what the code holds.
- **The degrade guarantees apply to QUERIER-CAPABLE providers only.** `gitlab` and `jira` do not implement `EpicChildrenQuerier`, so for them the window stays open exactly as today — fail-open by design, pinned by `TestSplitFiling_CapabilityLessProviderKeepsTodaysPath`. Note the stamping is NOT gated on the capability: a capability-less provider still files keyed bodies, so a later capability gain finds the markers already in place.
- **A failed sub-issue LINK is an uncovered residual.** A child whose create succeeded but whose link to the parent then failed is not in the parent's sub-issue graph, so the query cannot return it and it is re-filed. Narrower than the pre-change window; closing it needs a repo-scoped search rather than a sub-issue sweep. There is no test — this is stated as a residual, not a claim.

**Failure-mode coverage.** m1 query error → abort before any creation; m2 empty result → normal path; m3 non-matching child → normal path; m4 near-miss key (both directions) → not adopted; m5 CLOSED child bearing the key → adopted; m6 capability-less provider → no query, today's path; m7 comment listing error → posted anyway; m8 marker append failure on the ADOPTION path → abort, no completion marker; m9 duplicate keys → deterministic tie-break.

### Close-parent-when-contract-child-lands watcher (`split_parent_close.go`, E50.6 / #2062)

`fileSplitProposalChildren` files a split proposal's phased children and posts a parent comment asking the operator to close the parent by hand once the contract-phase child lands (#2057). `handleContractChildClosed` retires that manual step: it is a second `issues.closed` consumer, a SIBLING of the #1817 board-sync reconciler on the same event (that one moves a card, this one closes a parent), best-effort and never influencing the 202.

**The forge is the SOLE AUTHORITY for every decision with a side effect.** "Is the parent already closed?" is answered by `GetIssue`. "Has the linking comment already been posted?" is answered by re-reading the parent thread for a `splitfiling.ParentCloseCommentKey` marker. Consequently there is **no lock, no cross-run fold, and no `split_parent_closed` idempotency record** — the audit entry this file writes is a pure OBSERVATION that nothing reads to gate behavior, so an append failure changes nothing (`TestContractChildClosed_ObservationAppendFailure_DoesNotUnwind`).

**Linkage is a PURE PAYLOAD READ, and that is what removes a whole defect class.** #2062 widened `split_children_filed` additively with `parent_repo` + `parent_issue`, so `resolveSplitParentLinkage` answers "which parent does this closed issue belong to?" from `AuditRepo.ListAll` alone — no run resolution, no forge call. The installation identity comes off `webhook.Event.InstallationID`, an `int64` that is ZERO when the event isn't installation-scoped, so the `*run.InstallationID` nil-deref the run-resolving design carried is **structurally absent** here rather than merely guarded.

**Ordering is load-bearing, twice.**

- **Linkage BEFORE every linkage-dependent outcome.** The installation and `state_reason` gates run AFTER linkage, never before. An unrelated issue in the repo closed as `not_planned`, or any close lacking installation data, must write ZERO audit entries — an observation about a split it has nothing to do with would be a false record. Nothing forces the other order: the installation id is needed only for the forge calls, which come last. `TestUnrelatedIssueClosed_NotPlanned_WritesNothing` / `..._ZeroInstallation_WritesNothing` assert zero audit entries and zero forge calls; hoisting either gate ahead of linkage reddens both.
- **COMMENT FIRST, THEN CLOSE.** The close is what stops future deliveries reaching the forge (a closed parent short-circuits at `already_closed`), so closing first would make a transient comment failure PERMANENT. With comment-first, a torn delivery leaves the parent OPEN with the comment present, and the next redelivery finds the marker, skips the post, and closes. `TestContractChildClosed_ClosesParentWithLinkingComment` asserts on the fake's recorded CALL ORDER (counts alone cannot pin an ordering property); `..._CommentPostFails_ParentStaysOpen` asserts ZERO `PATCH` calls after a failed comment.

**Every defined skip.** `AuditRepo` nil; a non-GitHub `ev.Forge` (`s.cfg.GitHub` is a `*githubclient.Client` and cannot serve a GitLab project — GitLab parity deferred to #2900); an EMPTY `ev.Repo` (stated explicitly so the legacy no-op holds as a rule rather than depending on an empty-vs-empty comparison failing — without it, an empty-repo event MATCHES a legacy entry and the `state_reason` gate writes a false observation); an undecodable payload or non-positive issue number; a `ListAll` error (WARN, return, change nothing, **no** observation — an unreadable linkage is not a fact about any split); no linkage match (silent, zero writes — this is also the path a non-contract child close takes); AMBIGUOUS linkage; `InstallationID == 0`; a nil `GitHub` client (a server misconfiguration, not a fact about the split, so no observation); an unsplittable repo full name; and each of the four forge errors.

**AMBIGUOUS LINKAGE is a defined skip, not an arbitrary pick.** Issue-number uniqueness proves the CHILD is unique; it does NOT prove two payloads agree on the PARENT. When two surviving same-repo matches name the same contract child but DIFFERENT `parent_issue` values, taking the newest could close the WRONG parent — an unrecoverable, operator-visible error. The watcher touches no forge and records `outcome: "ambiguous_linkage"` naming both candidates (`TestContractChildClosed_ConflictingLinkage_SkipsAndAudits`).

**The `state_reason` decision, and its justification.** A contract child closed as `not_planned` or `duplicate` did NOT land, so closing the parent would falsely assert completion — the same disposition the sibling #1817 reconciler takes on the same event. The parent is left open, the operator's manual close remains correct and available, and the skip is audited as `child_not_landed`. Every other value — `completed`, and the missing/null form GitHub sends routinely for a plain close — PROCEEDS (`TestContractChildClosed_NullStateReason_ClosesParent` covers the absent-key, explicit-`null` and `"completed"` forms).

**`ListIssueComments` fails CLOSED here — deliberately opposite to `splitParentThreadHasComment` in `split_filing.go`.** There, a missing operator-facing comment was worse than a duplicate. Here that read IS the entire idempotency record, so posting blind would duplicate the comment on every redelivery. The parent stays open and the next delivery retries (`TestContractChildClosed_ListCommentsError_DoesNotPostBlind`). Do not "fix" one to match the other.

**Honestly-narrowed claims.**

- **Exactly-once is SEQUENTIAL-only.** Two GENUINELY CONCURRENT deliveries for the same contract child can interleave between the thread read and the comment write, both see no marker, and both post — a DUPLICATE LINKING COMMENT. The close does not duplicate (the second `UpdateIssue` is a no-op close on an already-closed issue). This window is deliberately left open: a duplicate comment is cosmetic, and a distributed lock's failure mode — a leaked lock wedging every later delivery — is strictly worse. `TestContractChildClosed_DuplicateDeliveryIsIdempotent` claims only the sequential property the code holds.
- **Splits filed before #2062 still need a manual parent close.** A legacy `split_children_filed` entry carries neither `parent_repo` nor `parent_issue`, so it matches nothing. That is the intended fail-quiet direction, asserted by `TestContractChildClosed_LegacyPayloadWithoutParentRepo_NoOp` rather than left to inference.
- **Cross-repo child filing would break the repo comparison.** `fileSplitProposalChildren` builds the children's `workmgmt.Target.Repo` from `runRow.Repo`, the same value stored as `parent_repo`, so the watcher compares the closed issue's repo against `parent_repo` directly. If cross-repo filing is ever added, the payload needs a separate `child_repo` and this comparison must move to it.

**The producer-to-consumer seam has ONE test, and it is not optional.** `TestSplitFiling_ProducerToConsumer_ParentCloseEndToEnd` runs the real `fileSplitProposalChildren`, then delivers a signed `issues.closed` for the contract child the PROVIDER actually filed (the number comes from the provider's recorded creation, never from the audit payload under test) into a second Server sharing the same audit store. A producer assertion plus a hand-seeded consumer fixture cannot cover this: both halves would agree on a wrong value. Changing the producer to store the bare repo NAME instead of `runRow.Repo` reddens it. Note the honest limit — because both halves decode through ONE Go struct, a json-TAG rename is symmetric and this test does not (and cannot) discriminate against it.

### Charter admission gate for backlog-grooming workflows (`charter_gate.go`, ADR-065 / E54.4 / #2236)

A backlog-grooming run ranks the backlog against the repo's **charter** — every ranking it proposes cites a rubric line by id — so a grooming run in a repo with no charter has nothing to anchor on. ADR-065-as-amended says plainly that there is no unanchored-grooming mode, and `checkCharterDeclared` is where that becomes a refusal: `422 charter_required`, no run row.

**WHERE THE RULE IS ENFORCED.** **Run admission is the load-bearing enforcement point** — on every seam that mints a run, which today means `POST /v0/runs` and the campaign item-run start (`StartRunForCampaignIssue`). It is what makes *starting* an unanchored grooming run impossible, it fails closed on a loader error, and it is **unchanged** by the static half (AC5) apart from the message single-ownership extraction below. The rule is **also** checked statically now, by `fishhawk validate` (E54.11 / #2801): the CLI reruns the same `grooming_report` structural discriminator over the spec and reads the repo's **working-tree** `.fishhawk/work-management.yaml` for a `charter:` block, rendering the shared `MsgFmtCharterRequired` template when a grooming workflow has none.

**The two are an earlier warning plus the load-bearing gate, not equivalents.** The CLI decides the charter question from the **local working tree**; run admission decides it from the **forge-fetched** conventions at the run's ref (through the TTL-cached loader that also consults the provider discriminator and the `FISHHAWKD_WORKMGMT_CONVENTIONS` break-glass override). On a dirty or diverged checkout, or under the override, the two can legitimately disagree. **The static check can be LESS strict than admission (a missed early warning), never more** (a false refusal of a spec the product accepts) — the CLI validates the conventions file against the canonical `work-management-v0` schema it now mirrors, but does **not** reproduce the backend's cross-field *semantic* checks (the mandatory-field trio, provider-connection requirements, ADR numbering), which live in Go and not the schema, so a conventions file that violates **only** a semantic rule is admitted by the CLI though run admission refuses it. That residual direction is deliberate; see `cli/README.md`. The message template and the three reason values are held byte-identical across the two modules (which cannot share a package) by `TestCharterMessageParityAcrossModules`. `WorkflowRequiresCharter` stays **pure** (no receiver, no I/O); the CLI reruns an independent structural twin over its raw YAML tree rather than importing it, held to this copy by that parity test.

- **The discriminator is STRUCTURAL: a stage declaring `produces: [{artifact: grooming_report}]`.** Not the workflow's name (`backlog_grooming`), which is trivially evaded by renaming it, and not a `kind:` field, which the epic's AC1 forbids — the grooming workflow is built on the standard plan/implement/review stage types and is recognised by what it PRODUCES. `TestWorkflowRequiresCharter_StructuralDiscriminator` pins all three legs, including a workflow *named* `backlog_grooming` that produces no report and therefore does not trip the gate.
- **The gate is NARROW, and that is a tested property, not an intention.** An ordinary code-change workflow returns at the `WorkflowRequiresCharter` early return and never reaches the conventions loader, so it cannot be refused by a rule that has no business applying to it. `TestCharterGate_NonGroomingWorkflowWithoutCharter_Admits` is the pin: deleting that early return reddens it with a 422.
- **Three branches share the 422, so `details.reason` is what distinguishes them** — `charter_absent` (conventions loaded, no `charter` block), `charter_path_empty` (a block whose `path` is empty or whitespace, which anchors nothing), `conventions_unavailable` (the load itself failed). Every refusal test asserts the reason as well as the status; without that, deleting one branch would leave the other two branches' tests green.
- **A conventions-load ERROR fails CLOSED.** Admitting on a transient forge fault would let a grooming run start unanchored — the one outcome ADR-065 rules out — and it matches `RepoConventionsLoader`'s own documented posture (a fetch/parse failure other than `ErrNotFound` fails closed). The cost is that a forge outage blocks grooming run *creation*, which is the correct direction for a governance gate. Note the interaction with the default: `workmgmt.Default()` DOES declare `charter: {path: .fishhawk/charter.md}`, so the fallback posture admits and only a repo whose own `.fishhawk/work-management.yaml` omits the block is refused.
- **The refusal audit is BEST-EFFORT — the deliberate asymmetry `grantAppliesToOverride` documents (#2361).** A REFUSAL is the safe outcome, so failing to record it must not convert an audit outage into a governance outage; a GRANT records an exception being made, so its append is a precondition. This gate issues no grants, so it has only the best-effort half. `TestCharterGate_AuditAppendFailure_RefusalStands` pins it: the 422 stands when every append fails.
- **Placement: immediately after `checkAppliesTo`, before `CreateRunForTrigger`.** Same seam and the same two properties: **pre-insert** (a refusal leaves no run row — `TestCharterGate_MissingCharterBlock_Refuses` reads the run repo *after* the call, because a control that fired and rolled back would return a byte-identical envelope) and **post-replay** (an `Idempotency-Key` resolving to an existing run short-circuits above, so a replay re-evaluates no configuration decision and appends no second entry — `TestCharterGate_ReplayShortCircuitsBeforeGate` removes the charter *between* the two deliveries of the same request and still expects the 200).
- **`conventionsLoader` is a process-wide package var**, so every test here swaps it with a `t.Cleanup` restore and none calls `t.Parallel` — the established pattern in this package.
- **The `haveStageDefs` guard at the call site is a NARROWING, not the safety property.** The gate sits inside the existing `haveStageDefs` admission region beside `checkAppliesTo`, so it is fair to ask whether a grooming run could reach `CreateRunForTrigger` with that flag false and bypass it. It cannot: **both** spec-resolution branches in `handleCreateRun` (inline `workflow_spec` and the GitHub fetch) set `haveStageDefs = true` in the same statement that assigns `workflowDef`, and each rejects a zero-stage workflow *before* reaching it — so `haveStageDefs == false` means no workflow definition was resolved **at all**, `workflowDef` is the zero `spec.Workflow`, no stage rows are created, and `WorkflowRequiresCharter` (which iterates `wf.Stages`) is false **by construction**. A grooming-capable workflow necessarily declares a `grooming_report`-producing stage, so it can never sit behind that branch. Deleting the guard and re-running the suite leaves it green, which is the empirical form of the same statement: the **structural early return**, not the guard, is the load-bearing control. `TestCharterGate_NoStageDefsPathCarriesNoWorkflow` pins the observable half — the no-spec path creates a run with zero stages, with the loader stubbed to the worst case (no charter at all), so a future path that began carrying a grooming-capable workflow there would turn that 201 into a 422.
- **EVERY `CreateRunForTrigger` caller is gated, and that is a source-level pin rather than a claim.** The mint seam has two callers: `handleCreateRun` and `StartRunForCampaignIssue` (reached by the campaign item-run endpoint and by the campaign driver). The campaign seam is **not** structurally barred from carrying a grooming workflow the way the `haveStageDefs == false` branch is — it fetches the repo's spec from the forge and resolves an **operator-named** `workflow_id` out of it, so a repo whose spec declares a `grooming_report`-producing workflow can name it there. Documenting that seam as unreachable would therefore be false, so it is **gated**: the decision core is factored into `evaluateCharterAdmission` (discriminator → loader → reason → best-effort audit) and consumed by two arms that differ only in how they REPORT the verdict, never in the verdict — `checkCharterDeclared` writes the 422, and `ensureCharterDeclared` returns an `errCharterRequired`-wrapped error carrying the same reason and message for callers holding no `ResponseWriter`. Both fire **pre-mint**, so "refused before any run row exists" holds on both seams; the campaign arm's error surfaces through the item-run endpoint's existing `502 campaign_run_start_failed` mapping and leaves a driver-started item un-started for the next tick. `TestCharterGate_CampaignSeam_*` pins the seam behaviourally (refusal + fail-closed + both non-vacuity controls, reading the run repo and the audit fake after the call), and `TestCharterGate_EveryCreateRunForTriggerCallerIsGated` is the AST source scan — modelled on `run/childparams_gate_test.go` — that a **third** caller cannot be added ungated. Deleting the `ensureCharterDeclared` call reddens all three at once; so does returning `""` from `charterAdmissionReason`'s `charter_absent` arm, which reddens the HTTP and campaign refusal tests together and is the empirical form of "one core, two arms".
- **AC7's report-mode behaviour is pinned at the RUNTIME consumer, in `charter_gate_test.go`, not only in the spec package.** The spec-package `TestShippedGroomingExample_ReportModeDerivesNoAuthorityOrGate` asserts the *static* shape of the shipped declaration (empty `may_*` knobs, the declared gate inventory, an empty page list) — necessary but **not sufficient**, because a runtime consumer could begin treating a `mode: report` class as gated, or park the run on it, without touching either the workflow's `Gates` slices or `ResolveAutonomy`'s `PageHumanOn`. `TestShippedGrooming_ReportModeNeitherGatesNorParksAtTheConsumer` closes that: it drives the shipped declaration (read from disk) through the real `delegation.Evaluate` + `AutoDriveRunGate` report arm at a **live** approval gate and asserts the consumer derives no decision, surfaces no proposal, dispatches nothing, pages nothing, moves no stage, does not change the run state, and appends no `run_auto_driven` row — every assertion read as committed state *after* the call. `TestShippedGrooming_ReportArmIsLiveInThisHarness` is its paired control (a report entry on a backend-known class in the same harness DOES surface a proposal), so those zeros are the consumer answering rather than the consumer never running. Note the helper that creates the run, `startShippedGroomingRun`, no longer carries an `applies_to_override` (E54.22 / #2826): it starts the run with `trigger_source: on_demand`, which `appliesto.TriggerFormForSource` maps to `spec.TriggerOnDemand`, so the shipped `trigger: [scheduled, on_demand]` declaration admits it on its own terms. That converts a standing workaround into a **regression test** — if the mapping breaks, every `TestShippedGrooming*` case fails at admission with a 422 against the declaration read from disk.

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

- `POST /v0/campaigns` is **runless** — it resolves the repo's GitHub App installation directly (the same path `handleCreateRun` uses, no run row), queries the epic's children via the work-management provider, runs `campaign.Assemble`, and persists campaign + items; `pause_policy` (`pause_campaign` default / `pause_item`) is fixed here. `working_dir` (E48.87 / #2527) is the OPTIONAL campaign-level checkout binding every item run inherits: a non-empty value must be ABSOLUTE (400 `validation_failed` `{field: working_dir}`), validated with the other body checks **before** the `Idempotency-Key` lookup and the installation resolution, so a bad value costs no forge round-trip and the refusal is reachable without a live forge. It is threaded onto the assembly (`assembly.WorkingDir`) beside `pause_policy`/`operator_agent` and echoed back on the `Campaign` response (`omitempty` — an unbound campaign carries no key).
- `GET /v0/campaigns` (cursor-paginated, repo + state filters), `GET /v0/campaigns/{id}`, `GET /v0/campaigns/{id}/items`.
- `GET /v0/campaigns/{id}/status`: campaign + items + `campaign.NextEligible` readiness rollup + the server-computed `next_action`, forward-progress-first precedence (#1838): resume > start_run (eligible) > start_run (restartable) > attend_human_led > attention > wait > complete.
  **Terminal-campaign post-filter (E67.43 / #2681):** `computeCampaignNextAction(state, eligibility)` takes the POST-reconcile campaign state and, when `state.IsTerminal()`, rewrites the base action so the surface never advertises a verb its own state gate refuses. It KEEPS `start_run` when the state is `failed` AND the named ref is `Restartable` (the restart verb reopens such a campaign — see below), KEEPS `complete` when every item is done (that arm names no verb), and otherwise reports the `closed` action carrying the stranded `issue_ref` plus a detail naming the terminal state, the succeeded/failed/cancelled counts (Restartable folded into cancelled, matching the wire rollup) and the instruction to drive any remaining issue standalone with `fishhawk_start_run`. A NON-terminal campaign is byte-identical to the pre-#2681 behavior — in particular a PAUSED campaign is not terminal and still advertises `resume`, which is correct rather than an omission: resume IS legal on a paused campaign, so the "never advertise a refused verb" invariant already holds there, and reporting `closed` would tell the operator to abandon a campaign that is one verb away from continuing. `TestCampaignStatus_AdvertisedActionIsLegal` pins the invariant by INVOKING the advertised verb for each fixture (including an explicit paused row that invokes `POST /resume`), with a `default:` arm that fails on any action value it has no arm for.
  **Reconcile-on-read** as of E26.2 — `reconcileCampaignItemsOnRead` settles any running item whose linked run reached terminal and re-derives the campaign, best-effort + idempotent.
  **Recovery-lineage walk discriminator (E48.102 / #2549):** when the linked run is terminal-**failed**, `newestTerminalRecoveryDescendant` walks its `parent_run_id` children and settles off the newest terminal one (the #1751 resume/CI-retry behavior). The `ParentRunID` filter is NOT the discriminator — `run.ChildParamsFrom` sets `parent_run_id` for EVERY child kind, so a decomposition slice carries BOTH `parent_run_id` and `decomposed_from` and the query returns it. What makes the lineage recovery-only is the explicit `DecomposedFrom == nil` skip: a slice is neither a settle candidate nor an in-flight signal, and its own subtree is not enqueued. Two outcomes it buys: a failed decomposition parent settles `failed` off ITSELF (and keeps its `run_id`) instead of settling `succeeded` off whichever slice sorts newest by `CreatedAt` — the live incident, campaign 96997403 / issue:2501 — and a still-running slice never trips the `inFlight` early return and hangs the item `running` after the parent has already failed. Genuine recovery children (`handleRecoverRun`) and CI-retry children (`webhook.dispatcher`) leave `decomposed_from` nil, so the #1751 settle + provenance relink is unchanged for them (`TestReconcileOnRead_FailedRun_RecoveryChildWinsOverNewerSlice` pins both directions in one walk).
  A second `settleIssueClosedItems` arm (#1558, extended #2029) settles a deps-satisfied item whose GitHub issue is closed-as-completed `succeeded` with `settled_via=issue_closed`, unblocking descendants — same fail-closed posture — in two classes: a **run-less** pending/blocked item (no `run_id` in the marker) AND a **run-linked** item whose linked run went terminal-non-succeeded (cancelled/failed) but was delivered out-of-band, settled via the guard-bypassing `SettleCampaignItemOutOfBand` with the `run_id` retained (present in the marker). An open or `not_planned` closure still never settles either class.
  **Autonomy refresh (E25.20 / #2355):** the SAME per-poll `GetIssue` the settle pass issues also re-reads each item's `autonomy:*` label. `settleIssueClosedItems`' Phase 1 widens its fetch set to every **autonomy-relevant** item — `pending`/`blocked`/`failed`/`cancelled`, the states where `NextEligible` consults the tier (`running`/`succeeded`/`paused` route nothing on autonomy: a `paused` item is bucketed BEFORE any autonomy check, so its tier is never read) — a superset of the settle candidate classes, so this ADDS reads on some polls (run-less terminal + run-linked non-running items), honestly NOT parity, and adds NO new call class. `refreshItemAutonomyFromIssues` then folds the fresh tier through `SetCampaignItemAutonomy` ONLY when it differs from the stored tier (the no-op-write guarantee), emitting `campaign_item_autonomy_refreshed` `{campaign_id,issue_ref,from,to}`, so a relabelled child unblocks a campaign parked on `attend_human_led` on the same read. Best-effort / fail-open on every guard (nil GitHub, non-`owner/name` repo, install-resolve error, `GetIssue` error, non-`issue:N` ref, `SetCampaignItemAutonomy` error) — the read never fails and the tier is left stale. Because the repo-visibility check runs BEFORE this reconcile pass, a caller who cannot see the repo cannot drive this durable write (`TestGetCampaignStatus_RefreshDeniedCallerCannotWrite`).
- `POST /v0/campaigns/{id}/runs` (E26.2 / #1481, scope `write:campaigns`; `handleStartCampaignItemRun` — the operator-driven local-drive start): DAG-gates an `issue_ref` via `campaign.NextEligible`, mints the run through `StartRunForCampaignIssue` (a params struct as of E48.69 / #2498) carrying `runner_kind` and `working_dir` — the latter an absolute path bound onto the minted run so every later runner-spawning verb inherits it — links + transitions the item, advances a pending campaign.
  As of E48.87 / #2527 the minted run's checkout resolves through a THREE-RUNG ladder, evaluated BEFORE any mutation (item restart, run mint, link) so a refusal leaves no committed state: (1) an explicit per-item `working_dir` WINS — including when it DIFFERS from the campaign's binding, which is ACCEPTED as a deliberate override (a campaign is a batch; an item legitimately executing in another checkout is conceivable, unlike `resolveWorkingDirForRun`'s single-run conflict, which is incoherent) and logged, with the minted run row recording what actually applied; (2) otherwise the campaign's `working_dir` is INHERITED, **re-validated** through the same absolute-path gate an explicit value passes, so a relative binding written straight to the campaign row cannot bypass validation by arriving through a different door; (3) otherwise a `runner_kind: local` item is refused `working_dir_required` — #2498's "a local item needs a resolvable checkout" refusal RELOCATED here from the MCP tool, because only the backend can see the campaign's binding. It stays transport-independent (it fires for stdio and HTTP MCP clients alike) and now also covers direct REST callers, which the MCP-only guard never did; the MCP tool keeps its purely syntactic non-absolute refusal (no I/O, dials nothing) and renders the returned code as the two-remedy message.
  Gate codes: `item_not_eligible`/`campaign_item_not_found` (409+404), `item_human_led`/409 (a deps-satisfied `autonomy:low` item — a human must lead it, no ref named to start, #1697), `campaign_not_startable`/409 — as of #2681 this means the campaign is **paused** (resume it first) or **cancelled/succeeded** (closed; drive the issue standalone), NOT terminal-failed — `validation_failed`/400 (a non-absolute `working_dir`, or a non-absolute binding INHERITED from the campaign row, mirroring `POST /v0/runs`), `working_dir_required`/400 (a local item whose campaign carries no binding and whose body passed none, #2527), `campaign_run_start_failed`/502; **no `idempotency_key`**.
  **Terminal-failed reopen (E67.43 / #2681):** the campaign-state gate also admits a terminal-**failed** campaign, because a campaign whose LAST unsettled item failed derives terminal (`campaign.DeriveState`: `anyFailed && allTerminal`) while that item is still deps-satisfied and restartable — refusing here made it unrecoverable inside the campaign. The reopen happens strictly AFTER the DAG gate, so a failed campaign whose named item is NOT restartable is refused `item_not_eligible` with the campaign left `failed`, no run minted and no audit emitted (`TestStartCampaignItemRun_TerminalFailedCampaign_NonRestartableItemRefusedWithoutReopen` is the ordering counterfactual). When the item IS restartable the handler calls `campaign.Repository.ReopenCampaignForItemRestart` instead of `RestartCampaignItem`: ONE transaction holding row locks on the campaign and the item, flipping the campaign `failed`→`running` AND resetting the item to `pending` with its run link cleared, so no concurrent `reconcileCampaignItemsOnRead` can observe a running campaign whose every item is still terminal. On success it emits BOTH `campaign_advanced` `{from:"failed",to:"running"}` and the existing `campaign_issue_restarted`, then re-lists and falls through the unchanged mint/link/transition flow. Its error arms map by CAUSE, never folding a campaign-shaped miss into an item-shaped code: `InvalidTransitionError{Kind:"campaign"}` → `campaign_not_startable`/409 (the campaign left `failed` underneath us), `Kind:"campaign_item"` → `item_not_eligible`/409, `campaign.ErrCampaignNotFound` → `campaign_not_found`/404, any other `ErrNotFound` → `campaign_item_not_found`/404, else `internal_error`/500 — and on EVERY one of them no audit is emitted and no run is minted. A defensive guard refuses `campaign_not_startable` if a FAILED campaign ever reached the restart branch with a non-restartable item (unreachable under `DeriveState`, since a failed campaign has no non-terminal item) rather than minting a run inside a still-failed campaign. A running/pending campaign takes the unchanged `RestartCampaignItem` path (#1729/#1838).
  The start verb also runs the SAME autonomy refresh on the ONE named item BEFORE its DAG gate (`refreshOneItemAutonomy`), so a relabel-then-start works without an intervening status poll; best-effort / fail-open, and it runs AFTER the campaign-state gate so it never widens the campaign-state refusal surface. When the refresh DID change the tier but the follow-up relist fails, the refreshed item is patched into the stale slice in place so the `NextEligible` gate still partitions on the FRESH tier — otherwise the one relist-error branch would refuse a just-promoted item `item_human_led` on the pre-refresh value, the sole branch where the best-effort refresh's fail-open worked against the operator (`TestStartCampaignItemRun_RefreshChangedButRelistFails_StillStarts`). `humanLedDetail()` names the relabel-and-re-poll remedy.
- `POST /v0/campaigns/{id}/resume` (E25.7, scope `write:campaigns`): flips campaign+items `paused`→`running`, `campaign_not_paused`/409 when nothing is paused.
- `POST /v0/campaigns/{id}/cancel` (E25.20 / #2355, scope `write:campaigns`; `handleCancelCampaign`): marks every **non-terminal** item `cancelled` then the campaign `cancelled`, so an abandoned/rebuilt campaign stops showing as live `running` work in `GET /v0/campaigns`. **Recovery contract — idempotent + convergent:** the N item transitions run before the campaign transition (which runs LAST), all state-guarded under the repo's `FOR UPDATE`, so a mid-loop failure leaves the campaign non-terminal and a re-invoke re-lists, skips the already-cancelled items, and completes the cancellation — a partial failure never strands a campaign half-cancelled (`TestCancelCampaign_PartialFailureConvergence`). It emits `campaign_cancelled` `{campaign_id,from,items_cancelled}` and **deliberately does NOT cancel the linked RUNS** — a run in flight keeps running; `fishhawk_cancel_run` / `POST /v0/runs/{id}/cancel` owns run cancellation (`TestCancelCampaign_LeavesLinkedRunsUntouched`). `campaign_not_cancellable`/409 for an already-terminal campaign; the nil-repo 503 guard precedes the `write:campaigns` check.
- Gate codes on create: `repo_not_installed`/`campaign_dangling_dependency`/422, `validation_failed`/400 (bad ref or dependency cycle). The `campaign_dangling_dependency` details map carries a per-cause key set the fishhawk-mcp remedy renderer branches on: `dangling_not_child` (open out-of-set target), `dangling_excluded_incomplete` (#2120), and — added #2953 — `dangling_closed_incomplete` (a target closed WITHOUT completing; no widen remedy) and `dangling_state_unreadable` (an unreadable target; retry).
- **Satisfied-dependency elision on create (#2953):** a `depends_on` target OUTSIDE the assembled set that is already **closed-and-completed** is NOT dangling — the provider records it in `EpicChildrenResult.SatisfiedEdges`, `campaign.Assemble` carries it onto `Assembly.SatisfiedDependencies`, and `handleCreateCampaign` renders it into the 201 response's **create-response-only** `satisfied_dependencies` block (`[{from,to,state,state_reason}]`, `omitempty`, NOT persisted — GET/list/status omit it) plus a best-effort `campaign_dependency_elided` audit (`{campaign_id,repo,elided}`), emitted only when at least one edge was elided. So a batch whose prerequisite already landed assembles instead of refusing, and an operator can tell "this dependency is done" from "this dependency was ignored".
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

Read-only; cascades gracefully (not-installed → spec-unavailable → empty reviewers).

**Gate ordering is load-bearing: 401 anonymous → 400 malformed repo → `enforceRepoVisibility` (#1512, ADR-057 Amendment A2 / #2071).** It is NOT a write-scope gate — scope adequacy is a reported field — but it IS a repo read-visibility gate **for a non-admin cookie-session caller**: that identity class is the ONLY one that can receive the 403 — the three unfiltered classes below (bearer/MCP tokens, workspace admins INCLUDING admin cookie sessions, no-mirror deployments) keep the exact pre-change surface and never see it. Anonymous is rejected before any filter resolves (an unauthenticated caller must not learn a repo exists); the `repo` query value is validated to a well-formed `owner/name` before the filter is handed it; only then does the visibility gate run, so a denied non-admin cookie session reaches ZERO forge calls, ZERO spec fetches, and receives no `spec.Error` text. The gate reuses `enforceRepoVisibility` rather than hand-rolling a filter, so the endpoint inherits the whole #2071 point-read contract: 403 `repo_forbidden` on a deny of a non-admin cookie session, 503 `service_unavailable` on a mirror-store / provider-resolution / role-resolution fault (the store-fault class is kept DISTINCT from the permission-denied class), and the cross-forge / ambiguous-row / prefixless-subject fail-closed denies.

Three identity classes are UNFILTERED, each preserving the exact pre-change surface:

- **Bearer/MCP token identities** (`TokenID != ""`) — `repoFilterFor` returns nil; bounded by token ownership and scope, so the mirror (keyed on a human forge subject) has nothing to say about them. This is what keeps `fishhawk doctor` / the `fishhawk_doctor` MCP tool working.
- **Workspace admins** — `RoleAdmin` bypasses the filter, INCLUDING admin cookie sessions. The 403 is a NON-ADMIN qualification: an admin cookie session never sees it.
- **No-mirror deployments** — `Config.RepoVisibility == nil` is `repoFilterFor`'s first early return.

Pinned by, in `onboarding_test.go`: `TestOnboardingReadiness_RepoNotVisible` (the 403 + zero-forge control and counterfactual vehicle), `_RepoVisible` (admission control), `_BearerTokenUnfiltered`, `_AdminCookieBypass`, `_NoMirrorWired`, `_VisibilityStoreFault`, `_RoleResolutionFault`, `_ProviderResolutionFault`, `_CrossForgeDeny`, `_AmbiguousRowForgeDeny`, `_PrefixlessSubjectDenyAll`, `_AnonymousBeforeVisibility` (documents the observed handler-level posture — 401 `authentication_required` + zero mirror calls for anonymous — NOT a strict hoist counterfactual: `repoFilterFor` ALSO short-circuits anonymous callers with a nil filter, so a hoisted guard would still not reach the mirror fault), `_MalformedRepoBeforeVisibility`; and the cross-layer arms in `repovisibility_integration_test.go` (`TestRepoVisibility_Integration_MemberSeesOnlyGrantedRepo` / `_AdminSeesEverything`) that drive the endpoint through the real router, session middleware and Postgres-backed mirror.

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

- **Newest-entry-wins over a DECISION-plus-INVALIDATOR set.** It resolves the NEWEST audit entry for the stage across `scopeCompletenessDecisionInvalidatorCategories`, comparing by `Entry.Sequence` and filtering on `entry.StageID == stage.ID`. That set is the three `scope_completeness_*` decision carriers (`_parked` / `_exempted` / `_failed`) PLUS the three lineage INVALIDATORS `pull_request_opened` / `fixup_pushed` / `child_pushed`, mirroring `pushCheckpointCategories`. Only an `exempted` newest entry emits. A stage that RE-parked after an earlier exempt emits nothing (the later `parked` entry wins), and — because the invalidators are in the walk (#2630) — a stage whose exempt was SUPERSEDED by an opened PR, a landed fix-up push, or a child push also emits nothing, so a later dispatch takes the ordinary agent path instead of re-entering the completed PR-open short-circuit (the #2630 sticky re-entry). The `newestParked` walk stays restricted to `scope_completeness_parked`, so widening the decision set does NOT change which entry supplies the #2563 base-SHA fallback.
- **Fix-up refusal (#2630).** A fix-up dispatch (`fixup == true`, passed from both call sites) returns `ok=false` before the audit walk, mirroring the sibling `resolvePushCheckpointResume`. A fix-up carries a prompt the runner MUST run; emitting the held-commit fields would send it to its pre-agent `openHeldCommitPR` short-circuit and discard the fix — the exact trigger that stranded implement stage 266b8cf8 in `running` with no verb short of `cancel_run`.
- **Base-SHA source order (#2563).** The fourth field is the base commit the held commit was built on — the value the runner ships as the PR artifact's `base_sha`, which the backend's success-arm `validate()` REQUIRES (without it the exempt resume opens the PR and then always fails category-B, orphaning it — the #2562 bug). It resolves from (a) `park.BaseSHA` (persisted on every park since #2563), else (b) the `base_sha` of the NEWEST `scope_completeness_parked` audit payload for the stage — read from the same category walk the gate already performs. The fallback is what makes an ALREADY-parked legacy row (e.g. #2169's, whose park predates the field) resumable instead of re-failing identically after a `retry_stage`.
- **Fail-closed.** This gate is the only thing standing between the widened `awaiting_scope_decision → pending` transition edge (which any caller can drive) and a PR opened from a commit no operator accepted. So every uncertain branch WARN-logs and emits nothing: a fix-up dispatch (above), a nil `AuditRepo`, any `ListForRunByCategory` error, no entry for the stage, a newest entry that is not `exempted` (a re-park OR a superseding invalidator), a non-implement stage, a stage with no `ScopeCompletenessPark`, a park whose `HeldCommitSHA`/`RunBranch` is empty, an undecodable parked-audit payload, AND a park for which no non-empty base SHA resolves from either source. The four exempt fields are emitted together or all withheld — a missing base SHA withholds the held-commit fields too, so the runner never opens a PR it cannot ship.
- **Both handlers.** Wired into `handleGetStagePrompt` (the runner-facing dispatch path) AND `handleGetStagePromptRender` (the SPA preview), per that handler's byte-consistency convention.
- **The decision handler writes the proof before it opens the door.** Because the gate reads the audit chain, `scope_completeness.go::exemptScopeCompleteness` appends the `scope_completeness_exempted` entry BEFORE transitioning the stage out of `awaiting_scope_decision` (and therefore before the `Orchestrator.Advance` that can spawn a runner): a runner whose prompt fetch raced the append would read the older `parked` entry, get no held-commit fields, and re-invoke the agent — the full re-run the park exists to avoid. For the same reason the append is BLOCKING there: a failure returns 500 `exemption_unrecorded` and leaves the stage parked (re-POST the decision) rather than dispatching a stage whose exemption cannot be proven. `failScopeCompleteness` keeps its best-effort append — nothing reads the `failed` entry as authorization and its transition is terminal.
- **An `amend` DEMOTES a stale `exempted` (#2591).** `CategoryScopeCompletenessAmended` is in `scopeCompletenessDecisionInvalidatorCategories` for the same reason the three lineage invalidators are: an amend means "do NOT ship this tree, re-run against a wider scope", so an amend following an earlier exempt on the SAME stage must not leave `exempted` newest. Without it the gate re-emits all four held-commit fields on the amend's re-dispatch, the runner takes its pre-agent `openHeldCommitPR` short-circuit, and the widened re-run is discarded — the #2630 sticky re-entry shape, on a new trigger.
- The four json tags are the byte-identical cross-module wire contract with `runner/internal/upload/upload.go`'s `FetchedPrompt` decoder; a golden fixture duplicated across the two modules' tests (`goldenExemptPromptJSON`) is the prompt-response seam. The PR artifact the runner ships from the held commit has its OWN cross-module seam: `testdata/wire/held_commit_pr_artifact.json` is ONE file the runner test asserts its output equals and the backend test POSTs through the real handler — so a runner field the backend rejects fails at test time (#2558 tracks folding this into a shared wire package). Both seams exist because the runner and the backend are separate Go modules and no single-process end-to-end can span them.

## Build-required scope-drift park — the third shortfall class (E48.101 / #2548)

A scope-completeness park carries exactly ONE of three shortfall classes. The first two — missing declared scope files (#1151) and unsatisfied binding assertions (#1171/#2501) — are standalone-open-PR-push only and EXEMPT-ELIGIBLE. The third is not.

- **What it is.** A decomposition slice whose scope-only committed tree COMPILES but whose tests are red because a build-required file is owned by a SIBLING slice — the boundary cut a coupling (#2548's live case: slice 0 needed `backend/internal/server/prompt.go`, owned by slice 1). Before this, that failed category-B at commit+push, after the agent had exited, and the whole pass was discarded. Now the gate-verified commit is pushed to the child's own sole-writer slice branch, the child implement stage parks in `awaiting_scope_decision`, and the park names the build-required paths AND the owning sibling slice for each (`owning_slices`, resolved from the parent plan's `decomposition.sub_plans[i].scope.files` — the same authority `resolveSliceDependencies` reads).
- **Scoped to fan-out children.** The runner sets `ParkOnBuildRequiredDrift = isDecomposed && !isFixup`. A standalone run's build-required failure keeps today's category-B abort and its `fishhawk_resume_run --add_scope_files` in-band recovery (which a child has no equivalent of — its coupled file belongs to a sibling); a fix-up pass is byte-for-byte unchanged. A COMPILE failure and a test failure with EMPTY drift stay non-parkable on every path.
- **What it delivers: PARK-AND-PRESERVE WITH ATTRIBUTION.** The slice's work survives on `fishhawk/run-<parent>/slice-<n>` and the operator is told which sibling slice owns the file that broke the boundary. **Since #2591 the admissible decisions are `amend` and `fail`** (see the amend section below) — `amend` widens THIS child's stage scope with the coupled paths and resumes it, guarded so the single-owner-file invariant is only relaxed where it can be shown safe; `fail` keeps the original "correct the decomposition and re-run" recovery.
- **`exempt` is REFUSED (409 `exempt_refused_build_required`).** `scope_completeness.go::exemptScopeCompleteness` returns before it does anything — before the load-bearing `scope_completeness_exempted` append, before the stage leaves `awaiting_scope_decision`, before any `Advance`. Exempt resumes the stage and drives a PR-open from the held commit, whose committed tree is RED BY CONSTRUCTION for this class (that redness IS the shortfall), and opening a PR from it is the one outcome this design says must never happen. The refusal leaves the stage parked and appends no exempted entry, so the operator can still resolve it with `fail`.
- **Defence in depth.** `prompt.go::resolveHeldCommitExemption` independently withholds all four held-commit fields for any park carrying non-empty `BuildRequiredPaths`, even when an `exempted` entry is somehow newest for the stage. The endpoint refusal is the primary control; this gate is the last thing between a hand-written, legacy, or otherwise anomalous exempted entry and a PR opened from a red tree.
- **The parent side.** A parked child is settled but NOT terminal, so the decomposition parent stays in `awaiting_children`. That wait is correct but must not be silent: a `parent_awaiting_child_scope_decision` entry is emitted on the PARENT, from BOTH `emitParentAwaitingChildScopeDecision` at park time and `orchestrator.maybeAdvanceDecomposedParent` when a sibling settles. At most one entry exists per `(parent run, child stage)`, enforced at the store layer by migration 0067's partial unique index on `(run_id, payload->>'child_stage_id')` — not by a best-effort read in one emitter; the race-loser's `AppendChained` hits the index and both emitters treat that specific collision as the benign already-recorded outcome (INFO, park untouched) via `audit.IsParentAwaitingChildScopeDecisionDuplicate`. See `backend/internal/orchestrator/README.md` for why the emission has to be dual, and `backend/internal/audit/README.md` (0067 / #2594) for the index and the #2591 no-episode-component caveat.

> **ROLLBACK PRECONDITION — resolve every outstanding build-required park with `fail` BEFORE reverting #2548.** A revert removes the `build_required_paths` field AND both safeguards (the endpoint refusal and the prompt withholding gate). An already-parked build-required row would then decode with the key dropped, look like an ordinary park, and become EXEMPTIBLE — an ordinary operator `exempt` would set the held-commit emission fields and the runner would open a PR from a commit whose committed tree is red, which is precisely the outcome this design forbids. This is a narrow revert-window hazard and the resolution is operational, not structural: drain the parks first. Skipping this step does not corrupt data or break the revert; it leaves a live, reachable path to a red-tree PR for as long as any such park remains parked. To enumerate them, look for implement stages in `awaiting_scope_decision` whose `scope_completeness_park` JSONB carries a non-empty `build_required_paths`.

## Amend-and-resume a scope-completeness park (`scope_completeness_amend.go`, E67.18 / [#2591](https://github.com/kuhlman-labs/fishhawk/issues/2591))

`POST /v0/runs/{run_id}/scope-completeness/decision` takes a THIRD decision, `amend`: the operator names the paths the parked implement stage actually needed, they are folded into that stage's effective scope, and the stage RESUMES so the agent re-runs against the wider scope. It exists because the build-required class (#2548) had only `fail` — its `exempt` is refused since its held tree is red by construction — so a boundary miscut cost a whole pass and a re-run of the entire run.

**The four design questions the issue deferred, and how they are settled here.**

- **Whose scope widens.** The parked CHILD's own implement stage, never the parent's approved decomposition — an approved plan artifact is immutable. The widening is a stage-scoped, pre-approved #961 scope-amendment row, so it lasts for the remainder of this child's implement pass and nothing else.
- **The held commit.** REUSED, never discarded: `amendScopeCompleteness` does not reset, delete or rewrite `stage.ScopeCompletenessPark`, so the slice's work stays on its sole-writer run branch (ADR-035). The park row also remains as the durable record of what was parked; the prompt-gate invalidator above (not deletion of the park) is what stops it being read as a live exemption.
- **Re-run, not re-verify.** `gitops.StageScoped` strips the out-of-scope edits from the commit BEFORE the committed-tree gate runs — that stripping IS what produces the build-required shortfall — so the coupled file's content survives in no durable artifact the backend could re-verify. Making re-verify possible needs the runner to persist the stripped drift as a patch at park time; left to a follow-up.
- **Relationship to #961.** `amend` reuses that channel outright: the same `scopeamendment.ValidatePaths`, the same `{path, operation}` vocabulary, the same create-then-auto-approve row `recover.go::createApprovedScopeAmendment` mints, and therefore the same `prompt.go` fold (`mergeApprovedScopeAmendments` → `resolveApprovedScopeAmendmentEntries` → `foldScopeEntries`), which filters on `Status == approved` with no origin discriminator.

**The single-owner-file invariant is relaxed only where that can be SHOWN safe.** An amend hands a second slice permission to edit a file a sibling owns. The guard resolves each submitted path against the park's persisted `owning_slices` attribution (#2548) and inspects the owning sibling's implement stage:

| Owner state | Outcome |
|---|---|
| Run is not a decomposition child (`DecomposedFrom == nil`) | Resolved BY CONSTRUCTION — no sibling slice exists, so none can hold a divergent edit. No acknowledgement needed. |
| Path owned by THIS run's own slice | Resolved — a slice cannot conflict with itself. |
| Owner resolved, its implement stage NOT started | Permitted; the owning slice is named on the response and the audit entry so the operator knows to drop the file from it or expect a merge. |
| Owner resolved, its implement stage STARTED (`StartedAt != nil`) or succeeded | **409 `amend_refused_owner_slice_active`** — from that moment two branches can carry divergent edits to one file. |
| Ownership UNRESOLVED (no attribution entry, nil slice index, sibling lookup failed, sibling run/implement stage absent, sibling stages unreadable) | **409 `amend_owner_attribution_unresolved` — FAILS CLOSED.** Unknown ownership is exactly the case in which a sibling might already hold divergent edits, so recording the risk is not preventing it. `acknowledge_owner_unresolved: true` re-admits it and the acknowledgement plus the unresolved path list land on BOTH the response and the `scope_completeness_amended` audit entry — an audited operator decision, never a silent default. It does NOT relax the started-owner refusal. The flag is `amend`-ONLY: supplied with `exempt` or `fail` it is refused 400 `validation_failed` at BOTH seams (the handler and the `fishhawk_decide_scope_completeness` tool's pre-HTTP check), because only the amend arm runs the guard — forwarding it elsewhere would leave the operator believing an unrecorded risk decision had been audited. |

`Stage.StartedAt` is the sound proxy because `stages.started_at` is written under `COALESCE(started_at, $3)` and never overwritten (see `scope_amendment.go::amendmentDeadlineRemaining`); its documented cumulative-on-respawn caveat can only make the guard MORE conservative.

**Ordering is load-bearing** — `validate paths → coverage → in-flight row lookup → amend cap → owner guard → row → audit entry → resume → advance`. Everything up to the owner guard is a no-side-effect refusal (409/400/500: stage parked, no row, no audit entry) — the in-flight lookup is a READ. Coverage refuses an amend that omits any `build_required_path` (409 `amend_incomplete_coverage`), because a re-run under a scope that still excludes one re-parks identically and burns a full agent pass. The audit append is BLOCKING for the same reason as the exempt arm: that entry is what demotes a stale `exempted`, so a stage whose widening the chain never recorded must never become dispatchable. The resume is to `pending`, not `running` — `host_dispatch.go`'s admission switch accepts only `{pending, awaiting_host_dispatch}` (#2501).

**RETRY-SAFE, not atomic.** The row, the audit entry and the transition are three writes with no shared transaction, so a failure at either of the last two leaves an orphaned APPROVED row. That orphan is inert for the effective scope (`foldScopeEntries` dedupes by path) but NOT for the budget, which counts ROWS via `ScopeAmendmentRepo.CountByStage`. So the amend is IDEMPOTENT BY REUSE: `findOperatorAmendRow` recognizes its own orphan by `(stage, marker reason, identical path set)` — exactly what a re-POST of the same decision presents — and `resolveOrCreateOperatorAmendRow` reuses it, completing a PENDING one (`Create` landed, the auto-approve `Decide` did not) rather than duplicating. `ListByRun` failing REFUSES the amend (500 `amend_unrecorded`) rather than guessing, because guessing wrong drains the agent's budget silently. Correspondingly the per-stage amend cap counts DISTINCT `amendment_id`s across the stage's `scope_completeness_amended` entries, not raw entries, so a retry does not spend a second slot.

Two boundary conditions make that reuse safe rather than a cap bypass:

- **The reuse key carries a PARK-EPISODE component** (`amendEpisodeKey` — the park's `held_commit_sha`, falling back to `verified_tree_sha`, embedded in the marker reason). A stage that is amended, re-runs and parks AGAIN presents a byte-identical `(stage, reason, path set)` on its second episode; without the episode key it would adopt the first episode's row, keep one `amendment_id`, and amend without limit — the cap would never bite. An unidentifiable episode (empty key) refuses reuse entirely and mints a row per attempt: that over-counts against the cap (fail-CLOSED on the control) and only over-spends the agent's #961 budget, which `agent_amendment_budget_remaining` reports honestly.
- **The cap admits a retry of an amendment it has ALREADY counted.** A resume failure on the LAST permitted amend has already appended that amendment's audit entry, so a plain `used >= max` check would refuse the retry `amend_budget_exhausted` and strand the stage parked with no admissible decision left. `scopeCompletenessAmendUsage` therefore returns the counted `amendment_id`s alongside the count, and the cap refuses only a request that does NOT resolve to one of them — a retry adds no new amendment, so it stays admissible at the boundary while a genuinely new amend is capped exactly as before.

**Retry-safe is NOT concurrency-safe (accepted residual, stated rather than implied).** Retry-safety orders a SEQUENTIAL re-POST against its own orphan; it does not serialize two SIMULTANEOUS ones. Every read the amend decides on is a non-transactional read-then-write, leaving two TOCTOU windows on concurrent POSTs against the same parked stage. (1) **The cap and the reuse key:** `scopeCompletenessAmendUsage` and `findOperatorAmendRow` both read before either writes, so two amends can each pass the cap, and two decisions differing in paths or reason miss each other's reuse key and mint two rows — the same budget drain the retry path closes, arriving by a different route. (2) **The owner guard:** the owning sibling's `StartedAt` is inspected before the resume, so a sibling implement stage that STARTS inside that window admits exactly the divergent-edits state `amend_refused_owner_slice_active` exists to prevent. Neither window is guarded or tested. Closing them needs the three writes plus their preceding reads under one transaction with a per-stage lock, which is a repository-interface change (`ScopeAmendmentRepo` and `RunRepo` are separate seams with no shared `Tx`), not a local fix. The exposure is bounded by who can reach it: the decision is operator-only (a run-bound agent token is refused `run_token_forbidden`), so it takes two operators — or one operator double-submitting — racing on one parked stage.

**The true cost to the re-run agent, stated honestly.** An operator amend consumes ONE of the stage's TWO `maxScopeAmendmentsPerStage` slots, because that cap counts rows and the amend row hangs off the same stage — a re-run agent that would have had two #961 requests has one. Excluding operator-originated rows from that count would need an origin column and a migration. The response's `agent_amendment_budget_remaining` reports what is actually left. The residual: a retry that CHANGES the paths or the reason is a different amendment and correctly mints its own row, so the orphan from the earlier attempt does stay and does consume a slot; that is visible in the same field rather than hidden.

**Declared residual (not fixed here).** After an amend the re-run can park AGAIN, and migration 0067's partial unique index on `parent_awaiting_child_scope_decision` is keyed `(run_id, child_stage_id)` with no episode component (see `build_required_park.go`). The second park's PARENT-side signal is therefore deduped away and the parent chain keeps the FIRST park's `build_required_paths`. The child's own `scope_completeness_parked` entry is fresh, so the shortfall itself is never lost — only the parent-side attribution goes stale.

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


## Held-commit PR text (`heldcommitpr.go`, E67.5 / [#2570](https://github.com/kuhlman-labs/fishhawk/issues/2570))

Both resume gates above serve a runner that takes its PRE-AGENT short-circuit, so no agent runs and no PR-description handoff is written. The resumed PR therefore opened as `chore: fishhawk implement stage <id>` with no summary, no test plan and — the blocking part — no `Closes #N`, so merging never auto-closed the trigger issue. `heldCommitPRTitleBody` resolves the text the resumed runner opens with, served as `held_commit_pr_title` / `held_commit_pr_body` alongside the existing held-commit fields on BOTH `/prompt` and `/prompt-render`.

- **Why the backend owns this.** The runner's `/tmp` PR-description handoff is deleted on read and consumed by the pass that later parks or checkpoints, and on an ephemeral runner `/tmp` is gone before the retry dispatches. The park row (`run.ScopeCompletenessPark.PRTitle`/`PRBody`, additive JSONB — no migration) and the `push_checkpoint` payload are the only storage a same-host AND a fresh-host resume can both reach.
- **Recovery ladder, PER-FIELD.** Each field is resolved independently, so a recovered title is never discarded because the body was empty. Exempt gate: the park row's field, then the newest `scope_completeness_parked` audit payload's `pr_title`/`pr_body` (the legacy-row fallback, same ladder shape #2563 uses for `base_sha`, for a park written before the struct carried them). Checkpoint gate: the `push_checkpoint`'s `pr_title`/`pr_body`. Then, for whichever field is still empty, a documented synthesis from the run's cached `IssueContext` — a Conventional-Commits title (the issue title verbatim when it already matches the header shape, else `chore: `-prefixed, modelled on `orchestrator.consolidatedPRTitleBody`) and a body naming the issue and stating explicitly that the agent did not re-run. With no issue context either, BOTH come back empty and the runner keeps today's placeholder.
- **`Closes #N` is appended unless a REAL closing reference is already present.** Detection is deliberately precise in both directions. A false negative costs a duplicated line; a false POSITIVE silences the only auto-close directive and the issue stays open on merge — precisely what #2570 exists to prevent. So a match requires a GitHub closing keyword (`close`/`closes`/`closed`, `fix`/`fixes`/`fixed`, `resolve`/`resolves`/`resolved`) immediately preceding `#<n>` with the number terminated by a real word boundary, evaluated over text with inline code spans and fenced code blocks ELIDED (`stripCodeContexts`). A bare or non-closing mention (`Related to #N`, `See #N`), a longer number (`#25701`), trailing garbage (`#2570foo`), and closing-looking text inside a code span or fence ALL still get the directive appended. The elision itself must be false-positive-free, which cost two corrections: an elided inline span leaves a NON-SPACE, NON-WORD boundary behind (`codeSpanElision`) rather than joining its neighbours, so `` Closes `note` #N `` cannot collapse into the active directive `Closes  #N` that the source never contained; and only a VALID closing fence ends a block (same character, at least as long as the opener, no info string, per CommonMark §4.5), so an inner `~~~`, a three-backtick run inside a four-backtick block, or an info-string line like ` ```go ` is fence CONTENT and cannot expose the directive still inside it. Fence recognition is asymmetric on purpose: OPENING is liberal (an over-eager open only elides more text, costing a duplicate line) while CLOSING is strict (a wrongly accepted closer is the silently-un-closed-issue direction).
- **NEVER a safety precondition.** Every branch here degrades to empty text; nothing in this resolver can withhold a resume. The fail-closed controls in the two gates above are about opening a PR from an unintended HEAD, which is a different risk class. `TestPromptHeldCommitPRText_ExemptNoContextOmitsKeys` asserts both directions at once: the text keys absent, `open_pr_from_held_commit` still `true`.
- **Recording is byte-identical when absent.** `pr_title`/`pr_body` are written into the `scope_completeness_parked` and `push_checkpoint` payloads only when non-empty — those payloads are raw maps, not `omitempty` structs, so writing them unconditionally would stamp two JSON nulls onto every pre-#2570 entry. Neither is enforced in `validate()`, for the same reason as the checkpoint coordinates: a 400 there would strand the implement stage in `running`.
- **Footer ownership.** The served body EXCLUDES the Fishhawk attribution footer; the runner that opens the pull request appends it exactly once.
- **Pattern mirroring.** `conventionalCommitHeaderRe` here is the THIRD copy of the header pattern. `TestConventionalCommitHeaderRe_MatchesOrchestratorSource` pins it against `orchestrator.go`'s source literal (same module, directly readable), leaving only the runner's cross-module copy comment-guarded.


## Acceptance-criteria amendment at the plan gate (`acceptance_amendments.go`, E67.11 / #2581)

Plan-approval conditions reshape the design but never touch the plan's
acceptance criteria, so the acceptance stage validates the shipped behaviour
against the PRE-approval contract and fails a correct implementation (#2581, the
observed `healthz-reports-server-budget` failure in run 66c938d1). Two halves fix
that, and a deliberately-rejected third is documented below.

**1. The explicit channel.** `POST /v0/stages/{id}/approvals` accepts
`amend_acceptance_criteria`: a list of `{id, action: retire|restate, reason,
statement?}`. `retire` drops a criterion out of the live contract; `restate`
replaces its statement and leaves it LIVE (restatement is NOT a silencing
channel — a restated criterion still fails if it genuinely fails). The reason is
REQUIRED per criterion; the amendment is recorded on the SAME
`approval_submitted` payload as the `comment` / `add_scope_files` /
`remove_scope_files` that motivated it, so each retirement's id, reason and
source are reconstructable from the chain alone.

**2. Contested context in the acceptance prompt.** The acceptance prompt renders
the binding approval conditions AND the paths the operator dropped via
`remove_scope_files` as CONTESTED CONTEXT, with a skip-not-fail instruction:
where an observed behaviour conflicts with a criterion because a condition
changed the design or dropped its surface, the validator reports that criterion
`result`=`skipped` with the conflict in its `expectation_basis`, never
`result`=`failed`. This half is a PROMPT INSTRUCTION to an LLM validator — the
tests prove the instruction RENDERS, not that a validator obeys it. The failure
direction is safe: a non-compliant validator yields at worst today's spurious
failure, never a silent pass.

**Automatic DERIVED retirement is deliberately NOT provided.** Retiring a
criterion because its "subject" is a file dropped by `remove_scope_files` is a
natural-language judgement no token rule decides, and a rule that silently
retires a live criterion converts a loud acceptance failure into silence — the
exact inversion of the defect this fixes. So the ONLY retirement source is the
operator (`acceptanceRetirementSourceOperator`), and the dropped paths reach the
validator as context to judge in situ rather than as an inference.

### The single seam and its call sites

`resolveEffectiveAcceptanceCriteria` is the ONE place the effective set is
computed. No caller may union, filter, or recompute any part of it; a consumer
needing a different projection changes that signature. It returns `Live` (plan
order, restatements applied), `Retired` (plan order, with reason + source +
recording audit sequence), `Restated` (the ids a restatement replaced, plan
order), and `AllIDs` — ALWAYS the full plan id list.

`Restated` exists because `Live` alone cannot signal that an amendment applied:
a restate-only history leaves `Live` the same LENGTH as the plan set with only a
statement differing. `amended()` (`len(Retired) > 0 || len(Restated) > 0`) is the
single predicate a consumer uses to choose the effective set over the plan set —
`resolveAcceptancePromptCriteria` keying that decision off `Retired` alone
silently dropped a restate-only amendment on BOTH prompt paths, so the validator
judged the change against the very statement the operator replaced at the gate.
On a restate-only history the live set renders and the retired block does not
(nothing was retired). Pinned by
`TestGetStagePrompt_Acceptance_RestateOnly_RendersReplacement` and its
`TestRenderStagePrompt_…` twin.

It is consumed at exactly FOUR call sites:

1. `checkAmendAcceptanceCriteria` (the approve gate) — **validation-only**: it
   resolves the PRIOR effective set to evaluate the refusals below and records
   nothing derived from the result.
2. `handleGetPrompt`'s acceptance branch (the signed dispatch prompt).
3. `handleRenderPrompt`'s acceptance branch (the render/preview prompt).
4. `handleShipAcceptance` (verdict ingest), where the recorded retired-id set is
   the strict key for the downgrade.

### Anti-silencing gate — nine named refusals, all PRE-Submit

Every refusal inserts NO approval row, so a corrected retry flows normally
(the ADR-036 placement its sibling gates use). `details.rule` names each one:

| # | Refusal | Status |
|---|---|---|
| R1 | `unknown_criterion_id` — not in the approved plan | 400 `validation_failed` |
| R2 | `reason_required` — blank/whitespace reason | 400 |
| R3 | `statement_required` — `restate` with no statement | 400 |
| R4 | `duplicate_id` — the same id twice in one request | 400 |
| R5 | `amendment_not_approve_plan_stage` — a reject, or a non-plan stage | 400 |
| R6 | every criterion retired in ONE call | 422 `acceptance_criteria_all_retired` |
| R7 | every criterion retired CUMULATIVELY (prior approvals retired the rest) | 422 `acceptance_criteria_all_retired` |
| R8 | `already_retired` — an id a PRIOR approval retired | 400 |
| R9 | plan unloadable / zero criteria / prior amendments unreadable | 422 `acceptance_criteria_unavailable` |

R6 and R7 are ONE control evaluated on the deduplicated union of prior and
in-flight retirements: the channel cannot empty a plan's acceptance contract, in
one call or across many. An action outside `{retire, restate}` is refused under
`unknown_action`; an oversized reason/statement is CAPPED (`prompt.CapText`),
not refused. Unlike the scope channels — which IGNORE a non-plan-stage value
(#2598) — this channel REFUSES it, because a silently dropped amendment diverges
what the operator believes they retired from what the chain records.

### Downgrade at verdict ingest — four CONJUNCTIVE preconditions

`acceptanceDowngrade` neutralizes a FAILED verdict to `passed` only when ALL
hold:

- **D1** `verdict=failed` AND `failure_mode=assertion_fail`. An `error` mode (a
  crash / 500) is never downgraded — a crashing target is not a superseded
  expectation.
- **D2** EVERY failed criterion result's id is in the RECORDED retired-id set.
  Strict keying on the chain, never a prompt-text or heuristic match.
- **D3** the surviving partition is non-empty (at least one reported result whose
  id is NOT retired) — a verdict reporting only retired criteria evidences
  nothing about the live contract.
- **D4** no surviving BLOCKING un-evaluated criterion: no non-retired criterion
  REPORTED `skipped` **or `undecidable`** whose plan `blocking` value (nil → true)
  is true. D4 keys on what the verdict REPORTS, not on the live set: a blocking
  live criterion the verdict OMITS entirely does not block the downgrade (the
  same trust model that already lets a validator omit criteria from a `passed`
  verdict). `undecidable` (#2512) joins the arm with identical semantics — without
  it, a retirement plus a surviving blocking criterion the validator could not
  decide silently produces `passed`, a green light over unevaluated evidence.
  Pinned in both directions by `TestDowngrade_SurvivingBlockingSkip_NotDowngraded`,
  `TestDowngrade_SurvivingBlockingUndecidable_NotDowngraded` and
  `TestDowngrade_OmittedBlockingLiveCriterion_StillDowngrades`.

On a downgrade the `acceptance_outcome_recorded` payload records
`verdict=passed` plus `verdict_reported`, `downgrade_basis`, and
`retired_criterion_ids`; `failure_mode` and the raw criteria tallies stay what
the agent reported (evidence, not verdict), the STORED ARTIFACT BYTES are never
rewritten, and triage is skipped (no failure left to route). The merge gate reads
the audit payload, so `acceptanceGateState` returns the merge-eligible
`acceptance_passed` with no gate change.

### The undecidable acceptance outcome (#2512, E48.78 layer 4)

A validator that attempted a criterion and genuinely could not DECIDE it used to
have only two words for it: `failed` (which flattens honest uncertainty into a
defect signal, lands the run in acceptance triage, and can be discharged only by
the #2474 arbitration verb) or `passed` (a green light over an unevaluated
criterion). `undecidable` is the third: a per-criterion `result` value carrying a
REQUIRED, non-whitespace `undecidable_reason`, from which the server derives a
run-level disposition.

**The partition, settled by construction rather than by convention.** Three names
share one contract — merge-eligible, never a pass, distinct state string,
operator acknowledgement asked for in the merge verdict — and are partitioned by
a single total question: *was there evidence, and what did it say?*

| | decided | evidence | outcome |
|---|---|---|---|
| `not_validated` (#2347) | PRE-SPAWN, from the plan alone | none — no runner, no preview, no observation, and therefore NO criteria rows | merge-eligible |
| `undecidable` (#2512) | POST-RUN, from the agent's own rows | the stage ran and drove the preview; at least one row could not be decided | merge-eligible |
| `failed` → `acceptance_triage` (#2474) | POST-RUN, from the agent's own rows | a criterion genuinely failed | blocked until arbitrated |

They are MUTUALLY EXCLUSIVE by construction: the short-circuit skips dispatch
entirely so no criteria rows can exist for the ladder to read, and the precedence
ladder puts `failed` strictly above `undecidable` so one failed row keeps the run
in triage exactly as today. `undecidable` routes NO `acceptance_triage_decided`
disposition and never enters the class-1..5 triage classifier — an undecidable row
is not a defect, so there is nothing to fix up or retry. That is the #2474
wedge-surface reduction: the run lands merge-eligible with no arbitration.
Disjointness is pinned by
`TestAcceptanceAdmission_ShortCircuitIsDisjointFromUndecidable`.

**The aggregation is a TOTAL, EXPLICIT precedence ladder** over the per-criterion
rows (`aggregateAcceptanceResults`): any `failed` row → `failed`; else any
`undecidable` row → `undecidable`, **including when EVERY row is undecidable and
nothing was verified**; else `passed`. Read that last clause literally — this is
NOT "verified some criteria and could not evaluate others". A run that verified
nothing at all is admitted by the ladder, and it is exactly the case an operator
most needs described correctly. A row set containing an undecidable row can NEVER
be recorded as `passed`: a green light over an unevaluated criterion is the
dangerous direction, because nobody looks behind it.

The shipped-verdict/derived-verdict mismatch resolves SEVERITY-MONOTONE as a
lower bound on the total order `passed < undecidable < failed`:
`recorded = acceptanceVerdictAtLeast(shipped, derived)`. Nothing is ever softened
below what either source claims. **Compatibility note (not purely additive):** an
already-valid body shipping top-level `passed` with a `failed` criterion row
recorded `passed` before #2512 and records `failed` now. That is a behaviour
change on a pre-existing wire shape carrying no undecidable data, and it is
deliberate — recording a pass over a row the agent itself reported failed was the
dishonest direction. Pinned by
`TestAcceptanceSeam_ShippedPassed_FailedRow_NowRecordsFailed`.

`aggregateAcceptanceResults` is TOTAL and answers an EMPTY row set with `passed`.
That is safe ONLY behind the caller-side `len(rows) > 0` guard at its single call
site; the hazard is pinned openly by
`TestAggregateAcceptanceResults/EMPTY-row-set-answers-passed-the-documented-hazard`
and the guard by
`TestAcceptanceSeam_NoCriteriaRows_ShippedVerdictRecordedUnchanged`.

**Ordering at the ingest seam is load-bearing.** The #2581 retired-criterion
downgrade runs FIRST over the retired-id set; the ladder then runs over the
NON-RETIRED rows only. If the ladder ran over all rows, a retired criterion's
undecidable row would pin the run to `undecidable` forever and the operator's
retirement would have no effect
(`TestDowngrade_RetiredCriterionUndecidableRow_DoesNotPinTheRun`, with its paired
positive `TestDowngrade_SurvivingUndecidableOutsideRetirement_RecordsUndecidable`).
`verdict_reported` is the ONE "what the agent said vs what was recorded" field
and is present whenever the two differ, for either reason; `downgrade_basis` and
`retired_criterion_ids` stay gated on an actual retirement.

**`undecidable` is SERVER-DERIVED and UNFORGEABLE.** The ship endpoint's
top-level verdict enum still admits only `passed`/`failed` — that switch is
deliberately INCOMPLETE with respect to the `acceptanceVerdict*` constant set, and
`undecidable` joins `not_validated` in never being admissible on the wire. A
producer expresses undecidability on a criterion ROW, never at the top level. Do
not "complete" the enum: the counterfactual for that control is COMPLETION, not
deletion (`TestAcceptanceBody_TopLevelUndecidableVerdictRejected`).

`acceptanceGateState` resolves a recorded `undecidable` verdict to
`acceptance_undecidable`, handled BESIDE the `not_validated` arm rather than
falling through to the settled-outcome-unknown hole (which would wedge every
undecidable run at a 409), and `acceptanceGateAdmitsMerge` admits it — one
predicate, so all three merge consumers (#2474) admit it at once.

**`undecidable_reason` is decided on field PRESENCE, never on emptiness.** It is
decoded as a `*string` on both validators: Go's `encoding/json` makes an ABSENT
field indistinguishable from a PRESENT empty one on a plain `string`, so
`{"result":"passed","undecidable_reason":""}` would be silently admitted while
violating the rule that the field belongs only on an undecidable row. Absent is
accepted on a non-undecidable row; present — empty or not — is rejected. A literal
JSON `null` decodes to nil and is treated as absent.

**Corpus agreement, not byte-carrying.** The wire shape is validated by two
hand-maintained twins that cannot import each other:
`validateAcceptanceVerdict` in package `main` of `runner/cmd/fishhawk-runner` (the
runner module does not require the backend module) and `acceptanceBody.validate`
here. `docs/spec/acceptance-verdict-fixtures.json`, mirrored into both `testdata/`
dirs by `scripts/sync-schemas`, is the shared proof surface: each side runs the
SAME rows and must produce the SAME admit/reject partition, and CI's schema-sync
gate red-lines a mirror that drifts. The claim established is corpus agreement —
NOT that any test carries bytes returned by one validator into the other, which is
unimplementable across the module boundary. The sharpest row is a WHITESPACE-ONLY
`undecidable_reason`: the shape one side would admit and the other reject if
either compared against `""` instead of trimming, which would strand a completed
acceptance stage while both suites stayed green.

The backend half proves the partition at the WIRE, not only at the decoder:
EVERY row — both halves — is POSTed VERBATIM to the real ship endpoint, an
admitted body asserted `201` and a rejected body asserted `400
acceptance_invalid`. POSTing only the admitted half would leave a handler whose
decode path had become laxer than the test's own strict decoder admitting a
corpus-rejected shape while the test stayed green; the `top-level-unknown-field`
row is the one that detects exactly that drift (deleting the handler's top-level
`DisallowUnknownFields` admits it `201`).

### Invariants

- **`acceptance_criteria_ids` stays a SUPERSET.** The ids served on the
  acceptance prompt response are every PLAN criterion id, retired ones included,
  so a verdict reporting a retired id can never fail the stage closed on the
  runner's join-key validation.
- **Stored artifact bytes are never rewritten.** Only the governance payload
  carries the effective verdict.
- **Byte-identical when unused.** With no amendment recorded, the
  `approval_submitted` payload, the `acceptance_outcome_recorded` payload, the
  acceptance prompt, and the ship response are byte-for-byte what they were
  before this change (every new key is `omitempty`/conditional). Asserted against
  FROZEN PRE-CHANGE goldens, not against heading/key absence: the
  `approval_submitted` payload against `wantPreChangeAmendlessApprovalPayload`
  (`approvals_test.go`) and the acceptance prompt's criteria span against
  `wantPreChangeAcceptanceCriteriaSpan` (`prompt_test.go`, captured by rendering
  the package at the pre-#2581 commit) — so a same-keys-different-bytes
  regression fails.
- **A replay echoes the CHAIN, not a recomputation.** `handleShipAcceptance`
  computes the downgrade from the CURRENT approval chain, so a retirement
  recorded AFTER the original ship would make a replay of the same verdict bytes
  answer differently from the governance entry the merge gate reads. The
  idempotent branch therefore reads the recorded outcome entry
  (`recordedAcceptanceEffectiveVerdict`) and echoes ITS effective verdict, falling
  back to the fresh computation only when no entry is readable (in which case the
  #1396 heal just appended exactly that value). Pinned by
  `TestShipAcceptance_IdempotentReplay_EchoesRecordedEffectiveVerdict`.
- **Every degrade fails toward MORE validation.** An unreadable chain at prompt
  build renders the FULL plan criteria set; an unreadable chain, an unreadable
  plan, or NO approved plan at all at ingest performs NO downgrade; an unreadable
  chain at the approve gate REFUSES the amendment; an absent `AuditRepo` or an
  undecodable `approval_submitted` payload leaves the criterion LIVE. None of them
  can silence a criterion.

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
