# Self-hosted (Mode 1): the single-tenant profile

ADR-057 defines two deployment modes. This is **Mode 1** — one customer runs
`fishhawkd` inside their own perimeter against their own forge (GitHub
Enterprise Server, an EMU / data-resident `<slug>.ghe.com` tenant, or a
self-managed GitLab). For **Mode 2**, the multi-region hosted service, see
[hosted-regional.md](hosted-regional.md).

Mode 1 is not a separate build or a separate code path. It is the same
multi-tenant core (E44.1–E44.8) with tenancy **short-circuited to one implicit
tenant**: a single `accounts` row, created at startup from deployment config,
carrying an `auto_join_role`. Every member of the customer's enterprise / org /
group auto-joins that one account through the existing login gate. There is no
Mode-1 admission logic — the same E44.3 / E44.8 walk runs, with exactly one
account to match.

## Why the profile exists

`auth.MembershipResolver` admits a sign-in only against an EXISTING `accounts`
row and denies when none matches. Nothing else in the product creates that first
row, so without the profile a fresh install has **no admitting account**: every
sign-in is denied. The single-tenant profile is the supported way to create it,
and the `fishhawkd account` / `fishhawkd member` verbs (see
[Bootstrapping without the auto-join profile](#bootstrapping-without-the-auto-join-profile))
are the supported way to admit a specific human — neither requires hand-written
SQL, which used to be the only way out.

## Enablement: the account key, and only the account key

Every `FISHHAWKD_SINGLE_TENANT_*` variable defaults to empty.
`FISHHAWKD_SINGLE_TENANT_ACCOUNT_KEY` alone decides the mode:

| Configuration | Startup behavior |
|---|---|
| Nothing set | Bootstrap skipped. Hosted multi-tenant behavior, unchanged. |
| Account key set | Bootstrap runs. Any omitted field is filled from the internal defaults (`github` / `enterprise` / `member`). |
| Another `SINGLE_TENANT_*` field set, key EMPTY | **Startup ERROR** naming the missing `--single-tenant-account-key`. |

The third row is the load-bearing one. Silently reading a half-configured
profile as "hosted" boots a deployment with no admitting account, in which
nobody can sign in and nothing says why — the exact failure this profile
exists to prevent.

The bootstrap is idempotent (`ON CONFLICT (provider, account_key) DO UPDATE`),
so every restart converges the row on the configured profile without minting a
second account, and it never writes `home_region` — the regional pin
(`PinAccountHomeRegion`) owns that column.

### Fail-closed postures

| Configuration | Behavior |
|---|---|
| Any `SINGLE_TENANT_*` field set with an empty account key | startup error naming `--single-tenant-account-key` |
| Granularity outside `enterprise` / `organization` / `group` / `user` | startup error naming `--single-tenant-granularity` and the accepted set (rather than a raw SQLSTATE 23514 from `accounts_granularity_check`) |
| `user`-granularity account key whose casing differs from the authenticated login | **sign-in denied** at the login gate — the `login == account_key` comparison is byte-exact, with NO case normalization. Set the account key to the login EXACTLY, including case (see the personal-namespace section below) |
| Provider outside `github` / `gitlab` | startup error naming `--single-tenant-provider` |
| Empty auto-join role (direct construction only; the flag path defaults to `member`) | startup error — `ListAutoJoinAccountsByKeys` selects only accounts whose `auto_join_role IS NOT NULL`, so a NULL role is invisible to the login gate and the account would admit nobody |
| Account key set, `FISHHAWKD_DATABASE_URL` unset | startup error — a configured profile with no database is never a silent skip |
| Bootstrap write fails | startup error carrying the DB error |

## Profile env matrix

| Env var | Flag | Empty means |
|---|---|---|
| `FISHHAWKD_SINGLE_TENANT_ACCOUNT_KEY` | `--single-tenant-account-key` | hosted multi-tenant (no bootstrap) |
| `FISHHAWKD_SINGLE_TENANT_GRANULARITY` | `--single-tenant-granularity` | `enterprise`, once the key is set. Accepted: `enterprise` / `organization` / `group` / `user` |
| `FISHHAWKD_SINGLE_TENANT_AUTO_JOIN_ROLE` | `--single-tenant-auto-join-role` | `member`, once the key is set |
| `FISHHAWKD_SINGLE_TENANT_DISPLAY_NAME` | `--single-tenant-display-name` | NULL |
| `FISHHAWKD_SINGLE_TENANT_PROVIDER` | `--single-tenant-provider` | `github`, once the key is set |

The account key is the forge-neutral natural key, matching the granularity: a
GitHub enterprise slug (`enterprise`), a GitHub org login (`organization`), a
GitLab group full path (`group`), or — for the `user` tier — the **owner's
forge login** itself. For `user` granularity the key must equal the
authenticated login EXACTLY, including case: the login-gate comparison is
byte-exact and performs no normalization, so a casing mismatch denies the very
first sign-in of a fresh install (see [Personal-namespace install](#personal-namespace-install)).

## GHES / EMU endpoints

A self-hosted install almost always overrides the GitHub endpoints too (E44.2 /
#1826). All empty is the `github.com` / `api.github.com` posture; set them
together.

| Env var | Flag | Empty means |
|---|---|---|
| `FISHHAWKD_GITHUB_API_URL` | `--github-api-url` | `https://api.github.com` |
| `FISHHAWKD_GITHUB_UPLOAD_URL` | `--github-upload-url` | `https://uploads.github.com` |
| `FISHHAWKD_OAUTH_AUTHORIZE_URL` | `--oauth-authorize-url` | `https://github.com/login/oauth/authorize` |
| `FISHHAWKD_OAUTH_TOKEN_URL` | `--oauth-token-url` | `https://github.com/login/oauth/access_token` |
| `FISHHAWKD_OAUTH_USER_URL` | `--oauth-user-url` | `https://api.github.com/user` |
| `FISHHAWKD_OAUTH_ORGS_URL` | `--oauth-orgs-url` | `https://api.github.com/user/orgs` |

A GitLab-backed install sets `FISHHAWKD_GITLAB_BASE_URL` instead; note the two
GitLab surfaces are configured asymmetrically — the login-gate group lister
needs only the base URL (it authenticates as the signing-in user), while the
forge / work-item provider additionally needs `FISHHAWKD_GITLAB_TOKEN`. See
[gitlab.md](gitlab.md).

Also required, as in any deployment: `FISHHAWKD_DATABASE_URL`, the GitHub App
credentials (`FISHHAWKD_GITHUB_APP_ID` +
`FISHHAWKD_GITHUB_APP_PRIVATE_KEY_FILE`), and the OAuth trio
(`FISHHAWKD_OAUTH_CLIENT_ID` / `_SECRET` / `_CALLBACK_URL`) — all three of the
last must be set together or startup fails.

## Install

The image and chart are the ones the hosted service runs; only the values
differ.

```sh
helm upgrade --install fishhawk deploy/helm/fishhawk \
  -f deploy/helm/fishhawk/values-prod.yaml \
  --set singleTenant.accountKey=acme-corp \
  --set singleTenant.granularity=enterprise \
  --set config.githubApiUrl=https://ghes.acme.example/api/v3 \
  --set config.oauthAuthorizeUrl=https://ghes.acme.example/login/oauth/authorize \
  --set config.oauthTokenUrl=https://ghes.acme.example/login/oauth/access_token \
  --set config.oauthUserUrl=https://ghes.acme.example/api/v3/user \
  --set config.oauthOrgsUrl=https://ghes.acme.example/api/v3/user/orgs
```

`values-single-tenant.yaml` ships the same posture as a COMPLETE profile rather
than a commented example, so the install reduces to substituting the hostname and
account key:

```sh
helm upgrade --install fishhawk deploy/helm/fishhawk \
  -f deploy/helm/fishhawk/values-single-tenant.yaml \
  --set ingress.host=fishhawk.acme.example \
  --set singleTenant.accountKey=acme-corp
```

### The fail-closed posture survives templating

fishhawkd REFUSES to start when any `FISHHAWKD_SINGLE_TENANT_*` key is set while
the ACCOUNT KEY is empty — a deployment with no admitting account is one nobody
can sign in to. That refusal is only reachable if the chart hands the
half-configured profile to the pod UNCHANGED, so the chart does exactly that: it
renders each key it is given verbatim, and it does NOT default an empty account
key to anything. A chart-side default would boot fishhawkd into an account the
operator never chose and make the documented startup error unreachable.
`scripts/test-helm-render` case r8 renders a half-configured profile
(`granularity`/`autoJoinRole`/`displayName`/`provider` set, `accountKey` empty)
and asserts the four keys are emitted unchanged while the account key is neither
emitted nor defaulted.

`values-prod.yaml` carries the same block commented out as a worked example.
The chart's ConfigMap omits every empty key, so an unset profile renders no
`FISHHAWKD_SINGLE_TENANT_*` entry at all. Chart reference:
[deploy/helm/fishhawk/README.md](../../deploy/helm/fishhawk/README.md);
cluster walkthrough: [kubernetes.md](kubernetes.md).

Verify after the rollout: the startup log line
`single-tenant profile bootstrapped` names the resolved account id, key,
granularity, and auto-join role. A sign-in by a member of that
enterprise/org/group then mints an `origin='auto_join'` `account_members` row.

## How admission ends up scoped to the customer

1. The user signs in through the deployment's OAuth app (the GHES/EMU host, if
   overridden).
2. The login gate reads the user's grants. An `origin='invited'` row admits
   **DB-only** — no forge call at all. That is the forge-independent fallback:
   an operator can invite a specific member (an outside collaborator, a
   contractor) whose enterprise/org membership the forge would not report.
3. With no invited grant, the auto-join path runs one live forge read and
   intersects it with accounts whose `auto_join_role` is set — under Mode 1,
   exactly the bootstrapped account. A match mints an audited
   `origin='auto_join'` grant and admits.
4. Every derived membership key stays BOUND to the granularity it was derived
   from, so an org key never admits an enterprise-granularity account of the
   same name. A Mode-1 profile configured at `enterprise` granularity therefore
   requires EMU posture for the enterprise short code to be derivable; on a
   github.com-style posture use `organization` granularity.

Auto-join grants are re-verified against their predicate at every subsequent
login: a user who leaves the org stops being admitted, and the row is kept for
audit rather than deleted.

## Personal-namespace install

A self-host whose owner has **no** enterprise, organization, or group to
auto-join through — a single-developer or personal-account deployment — sets the
`user` granularity instead (E44.35 / #2925):

```sh
helm upgrade --install fishhawk deploy/helm/fishhawk \
  -f deploy/helm/fishhawk/values-single-tenant.yaml \
  --set ingress.host=fishhawk.example \
  --set singleTenant.granularity=user \
  --set singleTenant.accountKey=octocat   # your forge login, EXACT case
```

The `user` tier's membership predicate is `authenticated login == account_key`.
Unlike the enterprise/EMU caveat in step 4 of the admission walk above — where
the enterprise short code must be *derivable* from an EMU login and a
github.com-style posture cannot supply one — the `user` tier needs **no live
forge membership read at all**: the login is already authenticated by the OAuth
handshake, so the owner is admitted even when the forge's org/group listing API
is unreachable, rate-limited, or (for a bare personal account) simply empty.

Two consequences to plan for:

- **The account key must match the login EXACTLY, including case.** The
  comparison is byte-exact with no normalization. GitHub returns the canonically
  cased login from its profile API, so set `singleTenant.accountKey` to that
  exact string. A casing mismatch denies the owner on the very first sign-in of
  a fresh install — check it first by comparing the `configured_account_key` and
  `login` fields the denial log now prints side by side (see [If sign-in shows
  "Access denied"](#if-sign-in-shows-access-denied)). Set the profile from the
  `FISHHAWKD_SINGLE_TENANT_*` variables in [`.env.example`](../../.env.example).
- **Admission does not depend on the forge, but a healthy forge is still read**
  for a login that *also* belongs to auto-join orgs/groups: the user-tier
  admission and the forge-derived admissions are unioned, so configuring `user`
  granularity never *narrows* what the owner can reach.

## Bootstrapping without the auto-join profile

The auto-join profile covers the common case: every member of the customer's
enterprise / org / group signs in and auto-joins. It does **not** cover an
outside collaborator or contractor whose membership the forge would not report,
nor a posture where the enterprise short code is not derivable (a github.com-style
deployment configured at `enterprise` granularity). For those, admit the human
directly with the supported CLI verbs instead of hand-written SQL — they write
the same `origin='invited'` grant step 2 of the admission walk above admits
**DB-only**:

```sh
# Create the admitting account (skip if the single-tenant profile already did).
fishhawkd account create --db "$FISHHAWKD_DATABASE_URL" \
  --provider github --account-key acme-corp

# Invite the member — --member-ref is their forge LOGIN, not a numeric id/email.
fishhawkd member invite --db "$FISHHAWKD_DATABASE_URL" \
  --provider github --account-key acme-corp --member-ref outside-collaborator --role member

# Confirm the roster (origin distinguishes invited from auto_join).
fishhawkd member list --db "$FISHHAWKD_DATABASE_URL"
```

`member invite` **fails closed** naming the `account create` remedy when the
account does not exist — it never materializes it. `--role` defaults to `member`
(least privilege) and must be `admin` or `member`. Re-inviting an existing
`auto_join` member upgrades that grant to `invited`, making the member's
admission forge-independent. Full flag / exit-code contract:
[`backend/cmd/fishhawkd/README.md`](../../backend/cmd/fishhawkd/README.md).

## If sign-in shows "Access denied"

The membership gate refuses a sign-in on two named branches. Since E44.31 /
[#2467](https://github.com/kuhlman-labs/fishhawk/issues/2467) the deny redirect
carries a `reason` code and the landing page names which one fired, so you no
longer have to correlate against the log to learn WHY. The redirect carries
nothing else.

| `reason` | What happened | Remedy |
|---|---|---|
| `no_membership_resolver` | The login gate has no membership resolver wired at all — most often no database configured — so **every** sign-in is denied. A deployment-configuration fault, not a per-user one. | Configure `FISHHAWKD_DATABASE_URL`, then the single-tenant profile (`FISHHAWKD_SINGLE_TENANT_ACCOUNT_KEY`, above). |
| `no_admitting_account` | The resolver ran and no workspace account admits this login: no `invited` `account_members` row, and no auto-join policy matched. | Set `FISHHAWKD_SINGLE_TENANT_ACCOUNT_KEY` to this login or its org (watch the byte-exact casing rule above), **or** have an existing workspace admin run `fishhawkd member invite`. |

**Neither page names the login, in any topology.** In the split-origin layout
(the SPA served on its own origin — Vite dev, or a self-host that does not
proxy the SPA), the deny redirect is relative and resolves against
**fishhawkd**, so fishhawkd's own `GET /access-denied` renders it. Behind a
same-origin reverse proxy that routes non-`/v0` paths to the SPA, the React
page renders instead. Both show the same branch-specific explanation, and
neither states who signed in: `/access-denied` is a separate, unauthenticated
request in both topologies, so a login on its URL is an unverifiable parameter
— claiming an identity from one would let a crafted URL show a misleading login
on your own denial page.

To learn WHO was denied, read the fishhawkd log: on the `no_admitting_account`
branch the callback logs `oauth sign-in denied: no admitting account` with the
authenticated `login` **and the configured single-tenant profile beside it** —
`configured_provider`, `configured_account_key`, and `configured_granularity`
(the RESOLVED values, so an unset granularity shows `enterprise`, the value
admission actually uses), or a single `single_tenant_profile=unconfigured`
marker when no profile is set (#2468). That one line makes both first-boot
foot-guns readable without correlating anything: compare `configured_account_key`
against `login` byte-for-byte to catch a **casing mismatch**, and check
`configured_granularity` to catch a personal-namespace install left on
`enterprise` (it should be `user` — see [Personal-namespace
install](#personal-namespace-install)). The profile fields come from the
`FISHHAWKD_SINGLE_TENANT_*` variables documented in
[`.env.example`](../../.env.example); the log is server-side only, so the
browser-facing denial page is unchanged and still names no login.

If the page shows the generic body instead of a branch, the deployment recorded
the reason but did not carry it — check the same log line.

## Runner gate isolation (ADR-063)

The runner executes spec-supplied gate commands (the committed-tree verify gates, the `diff_coverage` measurement, the auto-format absorb) on its own host, and the checkout they run over is agent-authored. ADR-063 ([#2127](https://github.com/kuhlman-labs/fishhawk/issues/2127)) contains that execution; this section is the operator posture for a Mode 1 runner host. It states what to install, what never to do, and what each fallback leaves open. The contract lives in [`runner/internal/gateiso/README.md`](../../runner/internal/gateiso/README.md); the runner-side summary and cleanup commands in [`runner/README.md`](../../runner/README.md) § "Gate isolation".

### Runtime requirement

- The `container` path needs BOTH a SAFE local container runtime AND a gate image. Image sources: `FISHHAWK_GATE_IMAGE`, or a workflow-v2 `gate_container` in the spec (a registry `image`, or an in-repo `dockerfile` + `context` build).
- SAFE means: docker whose `DOCKER_HOST` (when set) AND active-context host are both local, dialable unix sockets; or rootless podman with its socket service running (`systemctl --user enable --now podman.socket`). Docker Desktop on macOS classifies SAFE. Every other runtime is UNSAFE with a named reason on the recorded selection: tcp/ssh/npipe endpoints, a podman machine, rootful podman, a missing socket.
- Without both, the runner does not fail: under `local` / `self-hosted` it takes the fallback at reduced isolation (table below), and the gate evidence names what the container path lacked (`container_unavailable`).

Owner: `runner/internal/gateiso/README.md` § "Safe-runtime detection over the EFFECTIVE endpoint".

### Safe launch: never the host Docker socket

- Never mount `/var/run/docker.sock` (or any path that reaches the daemon) into a runner container or a gate container to make the container path work. ADR-063's hard requirement: a gate container that can reach the host daemon is a container-escape path and is worse than the unisolated status quo.
- Docker-outside-of-docker, a runner running inside a container against the host's daemon, classifies UNSAFE by design: the gate container would be a sibling on the host daemon, not isolated from it. So do tcp, ssh and npipe endpoints. A runner that is itself inside a container is classified UNSAFE whatever runtime it reaches, and takes the fallback: run the runner on the host to get the container path.
- The mount guard is the backstop, not the plan. It refuses any bind-mount source that resolves under `/run` or `/var/run`, is a unix socket, contains the detected daemon socket, or has a socket within eight directory levels. The gate then does not run: it is category C, never handed to the fix agent. The depth bound is a stated, test-pinned limit.

Owner: `runner/internal/gateiso/README.md` § "The container path" (the resolved-path socket-mount guard).

### What each path closes

| Path | Hosts | `.git` metadata | Egress | Host filesystem read |
|---|---|---|---|---|
| `container` | any host with a SAFE runtime and an image: Linux docker or rootless podman, macOS Docker Desktop | closed: independent clone mounted at `/work`, primary `.git` unreachable | closed: `--network=none` | closed: only the checkout, caches, a read-only `/etc/passwd` and (with a service) a read-only socket volume are mounted |
| `clone-sandbox` | Linux only, when the `unshare -rn` probe passes | closed: independent clone | closed at the IP layer only: network namespace (it hides host loopback too), but unix sockets in the filesystem the runner's user can read, the runtime's daemon socket included, stay reachable | OPEN: the gate reads whatever the runner's OS user reads |
| `clone` | every host; the only host path on macOS | closed: independent clone | OPEN: full host network | OPEN: same OS user as the runner |
| `refused` | any | the gate never runs: category C, not a fallback run, not the fix agent | | |

- `.git` metadata is closed on every path that executes: the gate's checkout is an independent `--no-hardlinks` clone with `remote.origin.url` unset, never a linked worktree of the primary.
- **macOS egress gap.** macOS has no unprivileged no-network sandbox, so `clone-sandbox` is unavailable there and `auto` without an image selects `clone`: full host network and host-filesystem read. Network isolation on macOS comes from the container path (Docker Desktop plus an image) or not at all.
- The coverage gates' waived residuals (profile forgery, same-user snapshot tamper) close once the gate command can no longer reach those paths (ADR-063 § Consequences). Only `container` withholds the host paths.
- The selection is visible: the first gate exec logs `gate_isolation_selected`, and the gate evidence carries `gate_isolation` with the precise `path` and its class (`container` | `fallback` | `refused`). An isolated run and a fallback run are distinguishable after the fact.

Owner: `runner/internal/gateiso/README.md` § "Selection (`select.go`)", § "The throwaway clone (`clone.go`)", § "The Linux no-network sandbox (`sandbox.go`)", § "Evidence (#2135)".

### The profile is declared, not detected

- Set `FISHHAWK_DEPLOYMENT_PROFILE=self-hosted` in the environment of every `fishhawk-runner` (`local` | `self-hosted` | `hosted`). Unset means `local` (`ParseProfile`); an unknown value is a startup config error.
- `self-hosted` behaves exactly like `local`: the image policy treats them identically and only `hosted` forbids the fallback paths. Setting it changes no behaviour; it records which posture the deployment claims on the selection and the gate evidence.
- `FISHHAWKD_SINGLE_TENANT_*` does NOT set it. Those are `fishhawkd` flags; the runner is a separate process that reads its own environment.

Owner: `runner/internal/gateiso/README.md` § "Environment variables (read by the runner at startup)".

### Hosted refuses, never falls back

Under profile `hosted` every non-container path is refused, so a runner without a safe runtime and an image cannot serve a hosted cell at all. A Mode 1 deployment does not run `hosted`; the policy and what it enforces are in [hosted-regional.md](hosted-regional.md#runner-gate-isolation-under-hosted).

### Per-project gate image (#2136)

- A workflow-v2 `gate_container` (`{image}` or `{dockerfile, context}`, workflow level with a per-stage override) names the image every gate kind runs in. Precedence: stage > workflow > `FISHHAWK_GATE_IMAGE` > the fallback. Spec: [`docs/spec/workflow-v2.md`](../spec/workflow-v2.md) § "Gate container".
- Declared images are pulled by the runner under the docker config it pins (credential-free by default; `FISHHAWK_GATE_DOCKER_CONFIG` for a private image, see "Docker Desktop: a locked screen and the credential helper" below; Fishhawk passes no credential and the spec cannot carry one) and run by `name@<registry digest>`. Under `self-hosted` a tag-only reference is allowed with a recorded warning and re-pulled on every gate; pin `name@sha256:<digest>`.
- A declared image the selected path cannot honour runs the host fallback under `self-hosted` (log `gate_container_unhonored`, evidence `declared_unhonored`). `hosted` refuses.
- Builds are allowed by default under `self-hosted`: the context is the COMMITTED tree at the gate's head SHA, behind a deny-by-default static Dockerfile guard, built with `--network=none`. `FISHHAWK_GATE_BUILD=deny` turns them off. Build tags accumulate under label `fishhawk.gate-build=1`; pruning is documented, not automated.
- **Limit: build bases are pulled with whatever credentials the pinned docker config holds.** Without `FISHHAWK_GATE_IMAGE_ALLOWLIST`, a declared build's `FROM`, `COPY --from=` and `RUN --mount from=` references are unconstrained and the daemon pulls them through the runtime CLI's bound environment. The agent-writable Dockerfile gains a daemon-mediated egress channel to any registry host under every posture (an anonymous pull still reaches the registry), and, under `FISHHAWK_GATE_DOCKER_CONFIG` only, read access to any private image that config holds credentials for. Since #4046 the default config carries no credential, so the host's interactive `docker login` store no longer grants that read access. Set `FISHHAWK_GATE_IMAGE_ALLOWLIST` or `FISHHAWK_GATE_BUILD=deny` on any host, and always alongside a `FISHHAWK_GATE_DOCKER_CONFIG` that holds registry credentials.
- **Limit: `RUN --mount=type=cache` persists on the daemon** across builds and projects under `self-hosted`, so one build can read a cache another wrote. Accepted for a single-tenant host.
- **Limit: guard residual [#4034](https://github.com/kuhlman-labs/fishhawk/issues/4034), open.** The guard classes any digits-only `COPY --from=` value as a stage index, but BuildKit treats a value too long for `strconv.Atoi` (20 digits, operator-verified on the issue) as an image, so it escapes the allowlist. Exploitation needs a single-component image under `docker.io/library` plus podman short-name search registries or a registry mirror.
- Other stated residuals (RUN steps as root in the build sandbox, `ONBUILD` triggers invisible to the scan, the scan being best-effort): ADR-063's #2136 addendum.

Owner: `runner/internal/gateiso/README.md` § "Declared gate image (`gate_container`, …)"; `deploy/gate-image/README.md` for this repository's own image.

### Daemon-dependent gates (#2137)

- The `--network=none` gate container has no daemon, no daemon socket and no network, so a gate that needs Docker (testcontainers) cannot run in it.
- `FISHHAWK_GATE_SERVICES=postgres` is the only provisioned daemon: the runner starts one network-less Postgres per container exec and shares only its unix socket, read-only, into the gate as `FISHHAWK_TEST_PG_URL`. Container path only; elsewhere the runner logs `gate_services_ignored`. An unknown member is a startup config error.
- Pin `FISHHAWK_GATE_POSTGRES_IMAGE` by digest (default `postgres:16-alpine`); it runs beside every container gate.
- Any other daemon dependency is unsupported on the container path. A repository whose gates need Postgres must not get a gate image without `FISHHAWK_GATE_SERVICES=postgres`: the pgtest-backed suite then fails closed with no database.
- On `clone`, the host's daemon is reachable as before. On `clone-sandbox` only its loopback TCP is hidden (the network namespace hides host loopback); a pathname unix socket such as `docker.sock` or a rootless `podman.socket` is a filesystem object, and the sandbox adds no mount namespace (`sandbox.go` wraps `unshare -rn` only), so a socket the runner's user can read stays reachable and a gate command could use it to start a networked container. Egress there is closed at the IP layer only; where that matters, use the container path.
- **Residual: RAM-backed PGDATA.** The service's data directory is a `tmpfs` with no size option and the service has no memory limit, so the databases a gate creates live in host memory. A large or hostile suite can pressure the runner host's memory (host availability only; the service has no egress).
- **Residual: crash-orphaned services.** A runner killed with SIGKILL mid-exec runs no teardown. The service container and volume stay behind, both labelled `org.fishhawk.gate-service`; the label-filtered cleanup is in `runner/README.md`.

Owner: `runner/internal/gateiso/README.md` § "Gate services (`service.go`, `passwd.go`; ADR-063 #2137 addendum, …)".

### Cache volume (#3967)

- `FISHHAWK_GATE_CACHE=process` (default) | `off`. It acts only on the container path: `process` keeps `GOCACHE` and the golangci-lint cache in ONE named volume per runner process, so later gate execs of a stage start warm. `off` restores a cold per-exec cache. The module cache stays per-exec either way.
- **Poisoning boundary: one runner process serves one stage of one run.** A fresh runner is spawned per stage dispatch, so no gate reads a cache another run's gate wrote. Within one stage the execs share it: an intermediate fix iteration's test can plant a cached result that a later verify of the same stage reads.
- A failed cache step (volume create, prepare, write check) degrades that exec to the per-exec caches and logs `gate_cache_volume_unavailable`. It never refuses the gate and never changes a verdict.
- The volume is removed when the runner process exits. A SIGKILLed runner leaves it behind, labelled `org.fishhawk.gate-cache`; remove it by label with no runner live (command in `runner/README.md`).
- The container path itself stays opt-in: nothing here changes what a runner without an image does.

Owner: `runner/internal/gateiso/README.md` § "Persistent cache volume (`cachevolume.go`, …)".

### Docker Desktop: a locked screen and the credential helper

Cause and fix: E51.26 / [#4046](https://github.com/kuhlman-labs/fishhawk/issues/4046).

- **Cause.** Docker Desktop's default `~/.docker/config.json` carries `"credsStore": "desktop"`, and the docker CLI consults it for EVERY registry it talks to, an anonymous pull of a public image included. `docker-credential-desktop get` → `docker-credential-osxkeychain` blocks in `SecItemCopyMatching` for as long as the macOS SCREEN is LOCKED; it is not waiting on a prompt you could answer. Operator spike 2026-10-06 (unified log): the helper blocked minutes after the lock and answered in 0.1s after the unlock, and a `credsStore`-free pull succeeded while the screen was still locked. Symptom: a `docker pull` makes no progress and prints no error, with a `docker-credential-desktop get` child under the pulling `docker`.
- **The runner is credential-free by default.** Every runtime CLI call it makes after selection (pull, inspect, build, passwd read, cache volume steps, service lifecycle, gate run, kill, cleanup) runs with `DOCKER_CONFIG` pinned to a runner-owned temp dir holding only `{"auths":{}}` plus your `cliPluginsExtraDirs` (so `docker buildx` still resolves). No `credsStore`, no `credHelpers`: no helper and no keychain is touched, so an unattended laptop with a locked screen still pulls public images. The dir is removed at runner exit. Your interactive config is never used for credentials; the runner reads it only for its `cliPluginsExtraDirs` key. The gate container never sees either config. Recorded as `credentials: anonymous` on `gate_isolation_configured`, `gate_isolation_selected` and the `gate_isolation` evidence.
- **Private images: `FISHHAWK_GATE_DOCKER_CONFIG=<dir>`.** An absolute path to a docker config dir you prepare for the runner, e.g. `credHelpers` scoped to one registry, or a token that needs no keychain. It is validated at startup (a defect is `runner_failed reason=config` naming the variable) and never removed. Recorded as `credentials: operator_config`. Before pulling, the runner probes each credential helper that config would invoke for an image the exec pulls (the gate image, the Postgres service image under `FISHHAWK_GATE_SERVICES=postgres`, and a declared build's bases before the build), with a 20s bound. A helper that does not answer fails the exec as category C with `container_credentials_blocked: credential helper docker-credential-<name> did not answer within <bound> for <server> (a locked keychain/screen, or a slow network-backed helper)`, instead of a silent 10-minute pull timeout. Do not point it at a config whose `credsStore` is `desktop` or `osxkeychain`: that reintroduces the lock dependency, now as a fast category-C failure.
- **A declared-image pull that fails under the default** appends the `FISHHAWK_GATE_DOCKER_CONFIG` remedy to its reason, since a private registry is a likely cause.
- **Residuals.** Runtime detection (`docker context show` / `inspect`, `docker version`) runs before selection with your inherited environment; it invokes no credential helper. On podman the pin is `REGISTRY_AUTH_FILE`, and podman can still call a helper configured in `registries.conf` `credential-helpers`, a system setting outside it; the default is credential-free for docker and only `REGISTRY_AUTH_FILE`-scoped for podman. A screen lock that begins mid-pull under an opt-in helper still hangs until the runner's per-step bound (`runner/README.md`).
- **Pulls outside the runner keep the hazard.** A hand-run pull, `scripts/test lint|verify --in-gate-image` and the docker-gated runner fixtures run under YOUR docker config. For anonymous public pulls there, unlock the screen, or use a fresh empty config instead of copying your own:

  ```sh
  mkdir -p "$HOME/.docker-anon" && printf '{"auths":{}}\n' > "$HOME/.docker-anon/config.json"
  export DOCKER_CONFIG="$HOME/.docker-anon"
  export DOCKER_HOST="unix://$HOME/.docker/run/docker.sock"
  ```

  Set `DOCKER_HOST` explicitly. A fresh config carries no `currentContext` or `contexts/`, so the CLI falls back to the `default` context, whose endpoint is `DOCKER_HOST` or else `/var/run/docker.sock`; pinning the socket keeps the effective endpoint from changing silently. Operator-verified (2026-10-06, Docker Desktop on macOS): with exactly this pair a pull of `docker.io/alpine/git:v2.47.2` completed immediately while the default config hung. A config without credentials cannot pull a private image.

Owner: `runner/internal/gateiso/README.md` § "Runtime CLI credentials"; `runner/README.md` § "Gate isolation" (Runtime CLI credentials); `deploy/gate-image/README.md` for the default gate image.

## What stays untenanted

CLI / bearer-token runs are not bound to an account: the account-scoped authz
check allows an untenanted run (the #1830 NULL-allow window). Under Mode 1 that
is not a cross-tenant exposure — there is exactly one tenant, so there is no
other tenant's data to reach.

## Residency posture

One cell, one database, no directory plane. Residency is a property of where the
operator runs the deployment, so none of the regional handoff configuration
(`FISHHAWKD_HOME_REGION` / `FISHHAWKD_HANDOFF_SECRET`) applies — leave both
unset and the region-pin surface stays disabled, which a single-cell deployment
never reaches. Region-scoped inference is still available per-process if the
install wants an in-region model endpoint: set `FISHHAWKD_MODEL_BASE_URL` and
`FISHHAWKD_MODEL_API_KEY` **together** (see
[regional-cells.md](regional-cells.md#region-scoped-inference) for the
fail-closed rules, which are identical here).

Per-account audit chaining is on regardless of mode — see
[ARCHITECTURE.md](../ARCHITECTURE.md) §5.1.1.
