# fishhawkd

Fishhawk control-plane daemon binary: the backend HTTP API server plus its operational subcommands
(`serve.go`, `migrate.go`, `token.go`, `audit_rehash.go`).

## Webhook receiver secrets

`FISHHAWKD_GITHUB_WEBHOOK_SECRET` (`--github-webhook-secret`) enables `POST /webhooks/github` (HMAC-verified);
when unset the endpoint responds 503 and `serve.go` warns.

`FISHHAWKD_GITLAB_WEBHOOK_SECRET` (`--gitlab-webhook-secret`) enables `POST /webhooks/gitlab` (E45.6 / #1860).
GitLab sends this secret VERBATIM in `X-Gitlab-Token` (no HMAC); when unset the endpoint responds 503.
Deliberately asymmetric with GitHub: an absent GitLab secret logs nothing (GitLab is optional — an absent-warn
would nag every GitHub-only deployment). The shared webhook delivery store (`webhook_deliveries` on Postgres,
else in-memory) is created when EITHER secret is set, so a GitLab-only deployment gets the store too.

## Configurable GitHub / OAuth endpoints (E44.2 / #1826)

For GitHub Enterprise Server (Mode 1, self-hosted) and data-resident GitHub
Enterprise Cloud `<slug>.ghe.com` (Mode 2, EMU), the GitHub REST / App API and
OAuth hosts are configurable. All are **optional**: an empty value keeps the
`github.com` / `api.github.com` default, so an existing github.com deployment is
unchanged. `resolveGitHubEndpoints` (in `serve.go`) maps each env var onto the
matching per-client override; the four GitHub clients already accept a base-URL
override, so this is wiring only.

| Env var | Flag | Overrides | Default |
|---|---|---|---|
| `FISHHAWKD_GITHUB_API_URL` | `--github-api-url` | App installation-token mint + REST client base | `https://api.github.com` |
| `FISHHAWKD_GITHUB_UPLOAD_URL` | `--github-upload-url` | REST client release-asset upload host | `https://uploads.github.com` |
| `FISHHAWKD_OAUTH_AUTHORIZE_URL` | `--oauth-authorize-url` | OAuth web-flow authorize URL | `https://github.com/login/oauth/authorize` |
| `FISHHAWKD_OAUTH_TOKEN_URL` | `--oauth-token-url` | OAuth web-flow token URL | `https://github.com/login/oauth/access_token` |
| `FISHHAWKD_OAUTH_USER_URL` | `--oauth-user-url` | OAuth web-flow user-profile URL | `https://api.github.com/user` |
| `FISHHAWKD_OAUTH_ORGS_URL` | `--oauth-orgs-url` | OAuth web-flow user-orgs URL | `https://api.github.com/user/orgs` |
| `FISHHAWKD_GITHUB_INSTALLATION_HOST_ALLOWLIST` | `--github-installation-host-allowlist` | fail-closed allowlist of hosts the Mode-2 per-installation GitHub base URL (App mint + REST) may resolve to (comma-separated; exact host or `.ghe.com` leading-dot suffix) | empty → scheme/parse validation only |
| `FISHHAWKD_GITLAB_INSTALLATION_HOST_ALLOWLIST` | `--gitlab-installation-host-allowlist` | fail-closed allowlist of hosts the Mode-2 per-installation **GitLab** base URL may resolve to (separate from GitHub's — a workspace's github.com and gitlab.com hosts differ) | empty → scheme/parse validation only |

The forge-neutral **identity provider** (device flow + REST reads) is threaded
too, at Mode 1 only: its REST base comes from `FISHHAWKD_GITHUB_API_URL`, and its
device-flow / OAuth host is derived from the scheme+host of
`FISHHAWKD_OAUTH_AUTHORIZE_URL` (an unset or unparseable value keeps
`github.com`).

## GitLab browser sign-in (E44.22 / #2109)

Per-deployment OAuth credentials enable the GitLab browser sign-in pair
`GET /v0/auth/gitlab/{login,callback}`, mirroring the GitHub OAuth leg. The
endpoint host is `FISHHAWKD_GITLAB_BASE_URL` — the **same** base URL the
login-gate group lister uses — so a configured base URL both registers the
gitlab group lister and hosts this sign-in flow, making the seam-first group
auto-join (#1832) reachable. Deployment-default only: the whole flow runs at or
before user identification, so no installation (hence no per-installation
`oauth_base_url`) is knowable, mirroring the deferred GitHub web-OAuth leg. The
credential trio is **all-three-or-error** — a partial config exits at startup —
and all three are empty by default (feature off, both endpoints respond `503`).

| Env var | Flag | Purpose |
|---|---|---|
| `FISHHAWKD_GITLAB_OAUTH_CLIENT_ID` | `--gitlab-oauth-client-id` | GitLab (group-scoped) OAuth application client_id; empty disables `/v0/auth/gitlab/*` (503) |
| `FISHHAWKD_GITLAB_OAUTH_CLIENT_SECRET` | `--gitlab-oauth-client-secret` | GitLab OAuth application client_secret (secret: never logged); required with the client_id |
| `FISHHAWKD_GITLAB_OAUTH_CALLBACK_URL` | `--gitlab-oauth-callback-url` | public URL of `/v0/auth/gitlab/callback`; required with the client_id |

The requested OAuth **scope is `read_api`**, which authorizes BOTH
`GET /api/v4/user` (the profile) and `GET /api/v4/groups` (the group-membership
auto-join list). `read_user` grants only the former and would deny every group
auto-join, so `read_api` is the single scope the whole flow needs. User identity
is forge-scoped (`users.provider`, migration 0061, `UNIQUE (provider,
github_user_id)`) so a GitLab numeric id never overwrites a GitHub user of the
same id.

**Mode 2 (per-installation)** rides on top of Mode 1: when a DB pool is present,
a single shared `account.EndpointResolver` (reading `installations.forge_base_url`)
is late-bound via `installationBaseURLResolver` into every per-installation forge
consumer:

- **GitHub App mint** — `githubapp.Client.ResolveBaseURL` (provider `github`).
- **GitHub REST client** — `githubclient.Client.ResolveBaseURL` (provider
  `github`), applied at the `buildRequest` choke point so every REST method
  routes without per-method edits; it reuses the SAME
  `FISHHAWKD_GITHUB_INSTALLATION_HOST_ALLOWLIST` as the mint (an install's host is
  identical whoever reads it).
- **GitLab forge** — the `gitlabclient.Factory` behind `forge/gitlab` (provider
  `gitlab`), gated by the separate `FISHHAWKD_GITLAB_INSTALLATION_HOST_ALLOWLIST`.

A SET column overrides the deployment default for that install; a NULL column or
unknown installation falls back to the deployment default; a **real DB error
FAILS CLOSED** (no credential shipped) rather than silently targeting the default
host. A nil DB pool leaves every hook nil → deployment default everywhere. Only
`forge_base_url` is consumed; `oauth_base_url` is **not** — its would-be consumer
(the OAuth / device-flow login host) is pre-identification, so the
per-installation OAuth + identity leg is **deferred** (see
`backend/internal/auth` and `backend/internal/identity`). See
`backend/internal/account/README.md`.

The resolved override is always validated for scheme/parse/host before the App
JWT ships (an `http://`, hostless, or malformed value fails the mint). On top of
that, `FISHHAWKD_GITHUB_INSTALLATION_HOST_ALLOWLIST` is an **optional,
default-off, fail-closed** host allowlist (E44.15 / #2093): when configured, the
resolved per-installation host must be an allowlisted entry — an exact host
(`acme.ghe.com`) or a leading-dot suffix (`.ghe.com`, matching any subdomain at
a true label boundary, so `notghe.com` is rejected) — or the mint fails before
the credential is transmitted. **Empty (the default) preserves today's posture
exactly** (scheme/parse validation only), which is safe because the sole writer
of `installations.forge_base_url` is the trusted operator-side
`UpsertInstallation` path (the same trust boundary as any config column). A
future production / tenant-facing writer of `forge_base_url` **must** configure
this allowlist — that deferral trigger is recorded in
`githubapp.Client.AllowedInstallationHosts`.

## Single-tenant deployment profile (ADR-057 Mode 1, E44.9 / #1833)

Five optional env vars bootstrap the ONE implicit account a self-hosted install admits through.
All five default **empty**, and an all-empty deployment is byte-identical to the hosted
multi-tenant posture (no bootstrap, no write).

| Env var | Flag | Effect | Empty means |
|---|---|---|---|
| `FISHHAWKD_SINGLE_TENANT_ACCOUNT_KEY` | `--single-tenant-account-key` | enterprise slug / org login / GitLab group path — **and the sole enablement signal** | hosted multi-tenant; bootstrap skipped |
| `FISHHAWKD_SINGLE_TENANT_GRANULARITY` | `--single-tenant-granularity` | `enterprise` \| `organization` \| `group` | `enterprise`, once the key is set |
| `FISHHAWKD_SINGLE_TENANT_AUTO_JOIN_ROLE` | `--single-tenant-auto-join-role` | role minted on auto-joining members | `member`, once the key is set |
| `FISHHAWKD_SINGLE_TENANT_DISPLAY_NAME` | `--single-tenant-display-name` | cosmetic | NULL |
| `FISHHAWKD_SINGLE_TENANT_PROVIDER` | `--single-tenant-provider` | `github` \| `gitlab` | `github`, once the key is set |

**The account key alone enables the profile.** The defaults above are applied INTERNALLY after
enablement, never as flag defaults — otherwise every hosted boot would look configured. So the
three states are unambiguous: nothing set → skip; key set → bootstrap with defaults filled; any
other field set with the key EMPTY → **startup error** naming `--single-tenant-account-key`. That
last one is deliberate: degrading a half-configured profile to hosted mode boots a deployment
with no admitting account, where every sign-in is denied and nothing says why.

Startup also fails on an out-of-set granularity or provider (naming the flag and the accepted
values, instead of a raw SQLSTATE 23514 from the `accounts` CHECK constraints), on a configured
profile with no `FISHHAWKD_DATABASE_URL`, and on a bootstrap write error. The upsert is idempotent
and never touches `home_region`. Contract: `backend/internal/account/README.md`; operator guide:
`docs/deploy/self-hosted.md`.

## Repo-scoped in-workspace visibility (ADR-057 Amendment A2, E44.10 / #2071)

Membership in a workspace account is not membership in every repository inside it. On top of the
existing account scoping, read paths are narrowed for a **non-admin cookie-session** caller to the
repos that caller holds at least `read` on at the forge, mirrored per identity by
`backend/internal/repoacl` with a freshness TTL.

| Env var | Flag | Effect | Unset means |
|---|---|---|---|
| `FISHHAWKD_REPO_ACL_TTL` | `--repo-acl-ttl` | how long a mirrored forge permission is served before it is re-resolved | `repoacl.DefaultTTL` (15m) |

**The filter is wired only when a database AND a configured identity provider are both present**
(`resolveRepoVisibility`). Either one missing leaves `server.Config.RepoVisibility` nil, which is
the untenanted-allow posture — exactly the pre-#2071 read surface, not a deny-all — and startup
logs a WARN naming which input was missing. That is also the no-code-revert kill switch: unwire the
identity provider and filtering stops. Startup logs an INFO with the effective TTL when it IS on.

The TTL is the whole staleness bound, in both directions: a permission GRANTED on the forge becomes
visible within it, and one REVOKED stays visible until the entry expires. Sign-in purges the
signing-in subject's mirrored entries so a fresh session re-resolves immediately; that purge is
deliberately **non-fatal** — if it fails, the surviving entries (grants included) simply revert to
the same TTL-bounded exposure the design already accepts everywhere else, which is a better trade
than failing sign-in closed on a transient DB blip. Shorter TTLs are safer but spend more forge
rate limit on cold pages.

Two failure classes are kept apart and must stay that way. A **forge** fault (outage or rate limit)
means the permission is unknown: that repo is not visible for the request, nothing is memoized, the
request otherwise proceeds, and it is logged at WARN naming the repo and reason — so a short page is
never silent. A **mirror-store** fault means the filter cannot function and the request answers
`503`. Long-form contract: `backend/internal/repoacl/README.md`; HTTP behavior and the
`repo_forbidden` code: `docs/api/v0.md`.

## Regional cells: handoff surface + region-scoped inference (ADR-062, E44.7 / #1831)

Four optional env vars turn this process into a *regional cell*. All four default empty, and an
all-empty deployment is byte-identical to a single-cell one.

| Env var | Flag | Effect | Empty means |
|---|---|---|---|
| `FISHHAWKD_HOME_REGION` | `--home-region` | the region THIS cell serves (`us`, `eu`, …) | region-pin surface **disabled** |
| `FISHHAWKD_HANDOFF_SECRET` | `--handoff-secret` | HMAC-SHA256 key shared with the directory plane | region-pin surface **disabled** |
| `FISHHAWKD_MODEL_BASE_URL` | `--model-base-url` | region-scoped inference endpoint for the Anthropic SDK reviewer | SDK default (`api.anthropic.com`) |
| `FISHHAWKD_MODEL_API_KEY` | `--model-api-key` | credential presented to that endpoint | falls back to `FISHHAWKD_ANTHROPIC_API_KEY` **only when `FISHHAWKD_MODEL_BASE_URL` is also empty** |

The two region-inference knobs are set **together or not at all**: either half alone is refused,
in the direction each half fails (see below).

**Pin surface construction is all-or-nothing.** `resolveRegionPin` (in `serve.go`) returns the
`(server.Config.HandoffSecret, server.Config.RegionPinner)` pair only when the region, the secret
**and** an account query surface (i.e. `FISHHAWKD_DATABASE_URL`) are all present; any one missing
returns `("", nil)` and logs once naming what is missing. Returning an empty secret even when a
secret *was* supplied is deliberate — half-configured is not a distinct posture, and it keeps the
downstream fail-closed guard depending on one condition instead of two.

Disabled is **fail closed, not permissive**: a request to the routed surface
(`server.RoutedOnboardingPath` = `GET /v0/onboarding/start`) that carries directory handoff
parameters is refused with 503 `region_pin_disabled`. Requests carrying no `fh_*` parameters pass
through untouched, which is what makes the flags a no-op for a single-cell deployment. Only that one
path is routed; the OAuth login/callback pair is deliberately not (see
`docs/deploy/regional-cells.md`).

**Region-scoped inference is process-level** (ADR-062 Q3(a)) — there is no per-account endpoint
registry. `FISHHAWKD_MODEL_BASE_URL` + `FISHHAWKD_MODEL_API_KEY` are threaded into
`anthropic.Config{BaseURL, APIKey}` by `planReviewerSet.newAnthropic`, so **both** the plan-review
and implement-review calls (one adapter, two prompt shapes) target the cell's in-region endpoint and
the review text never leaves the region. It governs the Anthropic **SDK** adapter only — the
`claudecode` and `codex` adapters are subprocesses whose endpoint is the CLI's own configuration.
`FISHHAWKD_MODEL_API_KEY` redirects a credential; it does **not** select the anthropic adapter,
which `FISHHAWKD_ANTHROPIC_API_KEY` still does.

The credential fallback is confined to the **default** endpoint. With `FISHHAWKD_MODEL_BASE_URL`
set and `FISHHAWKD_MODEL_API_KEY` unset, `inferenceAPIKey` resolves to the empty string rather than
the deployment's `FISHHAWKD_ANTHROPIC_API_KEY`: falling back there would send a production
credential — and the plan/review text it authenticates — to an operator-supplied host, which is
secret exfiltration via configurable egress. That half-configured posture fails closed and logs a
startup warning naming the missing key. Crucially, the protection does **not** rely on an empty
explicit key alone: because an empty `X-Api-Key` would not clear an ambient `Authorization: Bearer`
header, `anthropic.NewClient` appends `option.WithoutEnvironmentDefaults()` whenever the resolved key
is empty, so the Anthropic SDK adapter **neutralizes its ambient credential sources**
(`ANTHROPIC_API_KEY` / `ANTHROPIC_AUTH_TOKEN` / profiles) too. A deployment shell's ambient Anthropic
credential therefore cannot reach the operator-configured region endpoint (#2108).

The **mirror** half-configuration fails closed harder. With `FISHHAWKD_MODEL_API_KEY` set and
`FISHHAWKD_MODEL_BASE_URL` unset, the SDK would fall back to its **global default** endpoint and
send both the region-scoped credential and the plan/implement-review text out of the region.
Withholding the credential is not sufficient there — the request body still travels — so the
Anthropic SDK adapter is withheld **entirely**: `planReviewerSet.Default()` skips it (falling
through to `claudecode`/`codex` if either is configured, on a **non**-region-scoped cell — see the
refusal below) and `For("anthropic")` refuses by name, with a startup warning naming
`FISHHAWKD_MODEL_BASE_URL`. Set the endpoint, or unset the region key to run on the default endpoint
with `FISHHAWKD_ANTHROPIC_API_KEY`.

### A region-scoped cell refuses to boot without fully-configured in-region inference (#2107)

The subprocess fall-through above closes the SDK egress hole but not the subprocess one: `claudecode`
and `codex` carry their **own** (unverified, potentially global) endpoints, so on a region-scoped
cell a fall-through to them would still egress residency-sensitive review text outside the region.
`resolvePlanReviewers` therefore keys a hard **startup refusal** on the cell being region-scoped, not
on the inference config alone: when `FISHHAWKD_HOME_REGION` is set (`regionScoped()`) **and** in-region
inference is not fully configured (`FISHHAWKD_MODEL_BASE_URL` and `FISHHAWKD_MODEL_API_KEY` not both
set, `regionInferenceFullyConfigured()`) **and** any reviewer adapter is configured
(`anyReviewerConfigured()`), `resolvePlanReviewers` returns an error naming the missing variable(s)
and `serve()` logs it and returns `exitFailure` — the process refuses to start, so **no** reviewer
adapter can run. The refusal gates on `anyReviewerConfigured()`, so a region-scoped cell brought up
with **no** reviewer (e.g. DB/pin-surface first, reviewers added later) still boots and keeps the
existing "plan-review agent not configured" warning — the refusal fires only when a reviewer would
actually run and therefore egress.

When `FISHHAWKD_HOME_REGION` is **unset** (every deployment today), `resolvePlanReviewers` is
byte-for-byte unchanged: the mirror withhold-and-warn and the `claudecode`/`codex` fall-through above
are preserved. Residual (tracked as an operator follow-up): a region-scoped cell that IS fully
inference-configured but ALSO enables a `claudecode`/`codex` subprocess adapter could still egress via
a spec-declared `reviewers.agents[i].provider = claudecode/codex`; fully locking a regional cell to
region-pinned Anthropic only is a broader change than #2107.

## Work-management provider registration at startup (#1104)

`workmgmt_wiring.go` — `registerWorkmgmtProviders(cfg.GitHub, jiraClient, gitlabClient)`, called from
`serve.go`, registers each work-management provider gated on its OWN client:

- A configured **GitHub** client registers the `github_projects` work-item provider
  (`*githubclient.Client` satisfies the work-item `API` interface directly) **and** the
  product-feedback provider — the latter via `feedbackAPIAdapter`, since
  `FeedbackAPI.SearchOpenIssues` returns the workmgmt/github `MatchedIssue` type.
- A configured **Jira** client registers the `jira` work-item provider.
- A configured **GitLab** client registers the `gitlab` work-item provider
  (`*gitlabclient.Client` satisfies the gitlab `API` interface directly). It is gated on
  `FISHHAWKD_GITLAB_BASE_URL` + `FISHHAWKD_GITLAB_TOKEN` (all-or-warn, the jira precedent), built by
  `resolveGitLabClient` in `serve.go` (ADR-058 Phase 2, #1856).

An unconfigured client leaves that provider unregistered, and the affected endpoint keeps returning
**501** — the v0 not-yet-wired posture. This is the wiring behind #1104: `fishhawk_file_issue` /
`fishhawk_report_product_issue` answer 501 unless the providers are registered.

### The two GitLab surfaces are configured ASYMMETRICALLY (E44.8 / #1832)

`FISHHAWKD_GITLAB_BASE_URL` alone now enables a GitLab surface, so the
"gitlab partially configured … leaving it disabled" warning above does **not**
describe every GitLab path. Both facts, explicitly:

| Surface | Requires | Without the token |
|---|---|---|
| **Login-gate group auto-join** (`GitLabMembershipLister`, `backend/internal/auth/`) | `FISHHAWKD_GITLAB_BASE_URL` **only** — it reads `GET /api/v4/groups` with the **signing-in user's** OAuth access token | **enabled** |
| **Forge + work-item provider** (`gitlab` adapter, `fishhawk_file_issue`, …) | `FISHHAWKD_GITLAB_BASE_URL` **and** `FISHHAWKD_GITLAB_TOKEN` (PRIVATE-TOKEN) | disabled, endpoint 501 |

Startup logs both: the partial-config warning covers only the token-gated
provider, and a separate `gitlab login-gate group auto-join enabled …` line
names the lister. The lister becomes **reachable in production** once the GitLab
browser sign-in flow is configured (`FISHHAWKD_GITLAB_OAUTH_*`, E44.22 / #2109 —
see "GitLab browser sign-in" above), which drives `/v0/auth/gitlab/callback`
through the resolver with `provider=gitlab`. The two GitLab OAuth surfaces are a
THIRD asymmetry axis: the login-gate lister needs only the base URL (it reads as
the signing-in user), the forge/work-item provider additionally needs
`FISHHAWKD_GITLAB_TOKEN`, and the browser sign-in flow additionally needs the
`FISHHAWKD_GITLAB_OAUTH_*` credential trio (which shares the base URL as its
endpoint host).

### EMU enterprise auto-join (E44.8 / #1832)

Pointing `FISHHAWKD_OAUTH_AUTHORIZE_URL` at a data-resident GitHub Enterprise
Cloud host (`https://<slug>.ghe.com/login/oauth/authorize`) additionally enables
**enterprise-granularity** login-gate auto-join: the enterprise short code is
split off the EMU login (`<username>_<shortcode>`) and matched against
`enterprise`-granularity accounts carrying an `auto_join_role`. No new flag and
no extra forge call. On github.com / GHES posture no enterprise key is derived
at all — a public login cannot contain an underscore, so an ungated derivation
would be a spoofing surface. Seed such an account keyed by the enterprise SHORT
CODE. Startup logs `emu_enterprise_auto_join` and `membership_providers`.

## Per-repo work-management conventions loader + break-glass override (E45.16 / #2022)

`serve.go` installs the per-repo conventions loader after `server.New`:
`buildRepoConventionsLoader` assembles `server.RepoConventionsLoader` from the forge registry
(`registeredFileFetcher("github")` / `("gitlab")` — an absent forge yields a nil fetcher and that
provider falls through), the server's GitHub repo-scope resolution
(`srv.GitHubRepoScopeResolver()`), the deployment gitlab credential scope (non-zero exactly when
the gitlab forge is registered; the E45.5 static-token provider ignores the ref), and the
accounts provider discriminator (`account.NewResolver` over the pool — nil without a database, so
every filing then falls through to override/Default, the pre-#2022 posture). The loader fetches
`.fishhawk/work-management.yaml` from the filing repo's **own** forge, resolved via
`accounts.provider`; full contract in `backend/internal/server/README.md`.

`FISHHAWKD_WORKMGMT_CONVENTIONS` (ADR-058 Phase 2, #1856) is retained as the loader's
**break-glass fallback**, no longer THE loader: `loadConventionsOverride` still reads and parses
it fail-fast at startup — an unreadable or invalid file aborts serve with a precise error naming
the path + cause — but the parsed document is now served only when the per-repo resolution falls
through (provider not found/ambiguous, unregistered forge, no credential scope, or no committed
file). The run-absent GitHub installation-resolution branch in `workitems.go` remains gated on
`provider == github_projects`, so a gitlab filing never attempts GitHub egress.

`FISHHAWKD_WORKMGMT_ALLOWED_DESTINATIONS` (flag `--workmgmt-allowed-destinations`, E44.14 /
#2090) is the administrator-controlled escape hatch for the loader's destination binding: a
repo-fetched conventions file may only name a filing destination owned by the filing repo's own
tenancy account (contract in `backend/internal/server/README.md`). The value is comma-separated
`<account-key>:<provider>:<destination-key>` entries — e.g.
`acme:github_projects:enterprise,acme:jira:FISH` — with `provider` one of `github_projects`,
`gitlab`, `jira`; empty means strict binding with no exceptions. A `gitlab` destination key is the
namespace **root** (`group`), never a project path (`group/team`) — a full-path entry is rejected
at startup naming the root entry to use, because it could never match. A **malformed value fails
startup** with an error naming the variable and the offending entry: it must never degrade to an
empty (strict) allow-list, because a typo silently reverting to strict would masquerade as the
security posture working while breaking a legitimate cross-namespace deployment. Every refusal
names the exact entry to add here.
