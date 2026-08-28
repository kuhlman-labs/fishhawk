# deploy/helm/fishhawk

Helm v3 chart (`apiVersion: v2`) shipping the fishhawkd workload — the
k8s-deploy keystone (ADR-034 /
[#846](https://github.com/kuhlman-labs/fishhawk/issues/846)).

## Topology (`deployment.mode`, [#851](https://github.com/kuhlman-labs/fishhawk/issues/851))

- **`allInOne`** (default) — a single Deployment of `replicaCount` pods
  with the background workers co-located. Safe out of the box and an
  in-place upgrade for existing installs: the Deployment keeps its
  pre-split name/labels, so the rendered object is byte-identical bar a
  comment.
- **`split`** — a horizontally-scalable `-api` Deployment of
  `deployment.api.replicaCount` pods (every `FISHHAWKD_ENABLE_*` forced
  `"false"` — no background workers) plus a single-replica `-worker`
  Deployment that owns the worker singletons.

All three Deployments render their pod template from ONE named helper,
`fishhawk.fishhawkdPodSpec` (invoked with
`(dict "root" $ "role" all|api|worker)` — single source of truth, no
per-template duplication). Its only role-aware output is the worker
env: role `api` forces every toggle off; roles `all`/`worker` honor
`values.workers.*`.

The `-worker` Deployment's `replicas` is hardcoded to 1 — the worker
singleton invariant. There is no values knob, so it cannot be raised
past 1 and race the timers — until leader election lands (#851, the
future alternative that would let the workers run multi-replica).

### Topology guard

A render-time guard, `fishhawk.validateTopology` (included once from
the allInOne `deployment.yaml`), `fail`s when `allInOne` is combined
with `replicaCount > 1` AND any worker toggle is on — naming the
offending toggle and the two safe ways out (`deployment.mode=split`, or
`replicaCount: 1`). Split-mode templates have NO `validateTopology`
include site (`deployment.yaml` does not render in split mode), so any
future split-mode guard must be wired into
`deployment-worker.yaml` / `deployment-api.yaml`.

## Image, probes, Service

- Consumes the published `ghcr.io/kuhlman-labs/fishhawkd` image;
  `image.tag` falls back to `Chart.AppVersion` (the `main` rolling
  tag).
- Liveness + readiness probes on `GET /healthz` against containerPort
  8080; a ClusterIP Service on 8080.
- The Service selector carries an `app.kubernetes.io/component`
  discriminator so it selects EXACTLY the fishhawkd pod, never the
  in-cluster postgres/minio/jaeger pods or the migrate/minio-bucket
  hook Job pods (all of which carry the same bare `name`+`instance`
  labels). In `allInOne` mode the discriminator is
  `component: server`; in `split` mode it is `component: api`, so HTTP
  + webhook traffic routes only to the api pods — which dedup webhook
  deliveries via the Postgres store whenever `FISHHAWKD_DATABASE_URL`
  is set (`serve.go`). The `-worker` pod (`component: worker`) is
  excluded from the Service endpoints; its probes hit the pod directly.
  Without the discriminator the bare selector matched every workload
  pod, and selector-resolving commands (`kubectl port-forward
  svc/fishhawk`, `kubectl logs deploy/fishhawk`) picked an arbitrary
  match — the jaeger-logs symptom of
  [#2916](https://github.com/kuhlman-labs/fishhawk/issues/2916). The
  same `component` label lands in the Deployment's immutable
  `spec.selector` — see **Upgrading** below.

## Upgrading

**Chart 0.3.0 changes the fishhawkd Deployment's `spec.selector.matchLabels`**
(it adds `app.kubernetes.io/component: server` to the allInOne workload,
[#2916](https://github.com/kuhlman-labs/fishhawk/issues/2916)). A
Deployment's `spec.selector` is **immutable** in the Kubernetes API — the
API server rejects any update that changes it — so a plain `helm upgrade`
from chart 0.2.x **FAILS** with a field-immutable error
(`field is immutable`) rather than silently reconciling. This is expected;
pick ONE of the two remedies, which are NOT interchangeable:

1. **Delete the Deployment, then upgrade onto the still-installed release.**
   Deleting only the Deployment leaves the Helm release intact, so you
   `helm upgrade` (or `helm rollback`) onto it — do NOT `helm install`,
   there is nothing to install onto:

   ```sh
   # default FOREGROUND cascade, so the old ReplicaSet + pods go too
   # (do NOT use --cascade=orphan — that strands the old ReplicaSet, which
   # still carries the old selector and would fight the new one):
   kubectl -n <namespace> delete deploy <release>-fishhawk
   helm upgrade <release> deploy/helm/fishhawk -n <namespace> [ -f <your-values> ]
   ```

2. **Uninstall the release, then install fresh.** No release remains, so a
   fresh `helm install` is correct here (a `helm upgrade` would have nothing
   to upgrade):

   ```sh
   helm uninstall <release> -n <namespace>
   helm install <release> deploy/helm/fishhawk -n <namespace> [ -f <your-values> ]
   ```

Either path causes a brief API downtime while the fishhawkd pod is recreated.
The in-cluster postgres/minio/jaeger workloads are untouched — they already
carried a `component` label — and in **`split` mode the `-api` and `-worker`
Deployments are unaffected**, because they already selected on
`component: api`/`worker` before 0.3.0; only the allInOne Deployment's
selector changed.

> **In-cluster rollback is NOT free either.** Because `spec.selector` is
> immutable, rolling a release that is already on 0.3.0 *back* to 0.2.x hits
> the SAME immutable-field error as rolling forward, and needs the same
> remedy — delete the Deployment (foreground cascade) then `helm rollback`,
> or `helm uninstall` then `helm install` the 0.2.x chart. Reverting the PR
> that shipped 0.3.0 is a code rollback with no data migration; the in-cluster
> rollback of a *running* release is the part that costs the downtime above.

## Config

A ConfigMap carries the non-secret `FISHHAWKD_*` env: addr, S3
region/endpoint/bucket, external URL, OAuth callback/redirect, OIDC
audience/JWKS, GitHub App id, plan-review model, budget timezone, the
GHES/EMU endpoint overrides, and the single-tenant profile.
Empty keys are omitted to match fishhawkd's ignore-if-unset semantics.

| Value | ConfigMap key | Notes |
|---|---|---|
| `config.oauthClientId` | `FISHHAWKD_OAUTH_CLIENT_ID` | the PUBLIC half of the OAuth sign-in trio (not a secret) and its ENABLEMENT SIGNAL: set it and `FISHHAWKD_OAUTH_CLIENT_SECRET` becomes required AND the ingress-derived callback URL renders; empty → OAuth off (see "OAuth trio" below) |
| `config.extraEnv` | *(map, verbatim)* | escape hatch — a map of NON-SECRET `FISHHAWKD_*` name → value merged into the ConfigMap for any key the chart has no dedicated field for; values are coerced to strings. `fishhawk.validateExtraEnv` fails the render on a collision with a chart-managed key, an invalid env identifier, **or a known SECRET-bearing key** (any `fishhawk.secretKeySpec` key such as `FISHHAWKD_OAUTH_CLIENT_SECRET`) — the ConfigMap is readable by anyone with `get configmaps`, so a secret routed here would leak in plaintext; supply secrets through the Secret (see below), never here |
| `config.githubApiUrl` | `FISHHAWKD_GITHUB_API_URL` | GHES REST + App API base (E44.2 / [#1826](https://github.com/kuhlman-labs/fishhawk/issues/1826)); empty → `api.github.com` |
| `config.githubUploadUrl` | `FISHHAWKD_GITHUB_UPLOAD_URL` | release-asset upload host |
| `config.oauthAuthorizeUrl` | `FISHHAWKD_OAUTH_AUTHORIZE_URL` | GHES/EMU OAuth authorize URL |
| `config.oauthTokenUrl` | `FISHHAWKD_OAUTH_TOKEN_URL` | GHES/EMU OAuth token URL |
| `config.oauthUserUrl` | `FISHHAWKD_OAUTH_USER_URL` | GHES/EMU user-profile URL |
| `config.oauthOrgsUrl` | `FISHHAWKD_OAUTH_ORGS_URL` | GHES/EMU user-orgs URL (the login gate's org lister) |
| `singleTenant.accountKey` | `FISHHAWKD_SINGLE_TENANT_ACCOUNT_KEY` | **the enablement signal** — set it and fishhawkd bootstraps ONE implicit account at startup |
| `singleTenant.granularity` | `FISHHAWKD_SINGLE_TENANT_GRANULARITY` | `enterprise` \| `organization` \| `group`; empty → `enterprise` |
| `singleTenant.autoJoinRole` | `FISHHAWKD_SINGLE_TENANT_AUTO_JOIN_ROLE` | empty → `member`; an account with no auto-join role admits nobody |
| `singleTenant.displayName` | `FISHHAWKD_SINGLE_TENANT_DISPLAY_NAME` | cosmetic; empty stores NULL |
| `singleTenant.provider` | `FISHHAWKD_SINGLE_TENANT_PROVIDER` | `github` \| `gitlab`; empty → `github` |

Every `singleTenant.*` value defaults to EMPTY. Setting one of them
while `accountKey` is empty makes fishhawkd REFUSE to start (naming the
missing key) rather than degrade to hosted multi-tenant — a deployment
with no admitting account is one nobody can sign in to. Operator guide:
[docs/deploy/self-hosted.md](../../../docs/deploy/self-hosted.md).

### OAuth trio: all three or none ([#2915](https://github.com/kuhlman-labs/fishhawk/issues/2915))

GitHub OAuth sign-in is a three-part credential — **client id**
(`config.oauthClientId`, public, in the ConfigMap), **client secret**
(`FISHHAWKD_OAUTH_CLIENT_SECRET`, in the Secret), and **callback URL**
(`config.oauthCallbackUrl` or the ingress derivation, in the ConfigMap).
fishhawkd enforces all-three-or-none at startup: if any one is set they must
all be, else it logs `oauth misconfigured` and exits (`serve.go`). The chart
mirrors that whole contract instead of encoding half of it:

- **`config.oauthClientId` is the enablement signal.** Set it and the client
  secret becomes required (see the derived-requiredness note below) and the
  ingress-derived callback URL renders. Leave it empty and OAuth is OFF —
  fishhawkd serves `/v0/auth/github/*` as `503` rather than exiting, and the
  client secret is not required.
- **`fishhawk.validateOAuthTrio`** (included from `service.yaml`) fails the
  render on every OBSERVABLE partial combination with a named message. If you
  set the client id on a **non-ingress** install and the callback is empty, the
  message names BOTH ways out: set `config.oauthCallbackUrl` explicitly, **or**
  enable the ingress (`ingress.enabled` + `ingress.host`) that derives it as
  `<scheme>://<host>/v0/auth/github/callback`. It runs **before**
  `validateSecretContract` on purpose: setting only the client id leaves BOTH
  the callback empty AND (since the id makes the client secret required) the
  secret missing, so running the contract guard first would fail on the missing
  secret and hide the callback remedy. Because the trio's empty-callback branch
  fires only when the callback is empty, the operator sees the callback remedy
  first; once the callback is reachable that branch is silent and the contract
  guard owns the id-set-but-secret-missing message.
- **What the guard can OBSERVE is mode-dependent.** In `chartManaged` it reads
  `secrets.values.oauthClientSecret`; in `externalSecrets` it reads the same
  `secrets.externalSecrets.data[].secretKey` field `validateSecretContract`
  inspects (so the two cannot drift); in `existing` mode the chart can see
  NOTHING about a pre-created Secret at render time (see "Why `existing` mode
  has no render-time key check"), so it checks only the id/callback pair there.
  The id-set-but-secret-missing case is produced by `validateSecretContract`
  via derived requiredness, so there is one message per condition, not two
  guards racing.
- **Chart default is OAuth-off** (no ingress, no client id). `values-local`
  ships a complete dev trio; `values-prod` / `values-cell` /
  `values-single-tenant` ship a substitute-me client id (their enabled ingress
  derives the callback and their `existing` Secret carries the client secret).

### GitLab ([E45.32 / #2922](https://github.com/kuhlman-labs/fishhawk/issues/2922))

The chart wires the whole `FISHHAWKD_GITLAB_*` family so a chart-installed
deployment can turn GitLab on. The **non-secret** half is a top-level `gitlab:`
block (a profile-scoped family, sibling of `singleTenant:` / `cell:`), rendered
into the ConfigMap; the **secret** half is three `fishhawk.secretKeySpec` records
travelling the same secrets machinery in all three modes.

| `gitlab.*` value | ConfigMap env | Notes |
|---|---|---|
| `baseUrl` | `FISHHAWKD_GITLAB_BASE_URL` | GitLab instance host. **Alone** is a supported login-gate posture (group auto-join on the signing-in user's OAuth token); also the host the browser sign-in redirect reaches |
| `oauthClientId` | `FISHHAWKD_GITLAB_OAUTH_CLIENT_ID` | Confidential browser-sign-in application; the **enablement signal** for the trio |
| `oauthCallbackUrl` | `FISHHAWKD_GITLAB_OAUTH_CALLBACK_URL` | public URL of `/v0/auth/gitlab/callback`. **Explicit-only** — deliberately NOT derived from the ingress (an ungated derivation would give every ingress-enabled GitHub-only install a non-empty GitLab callback and fire the id-empty guard spuriously) |
| `deviceClientId` | `FISHHAWKD_GITLAB_DEVICE_CLIENT_ID` | a **SEPARATE, non-Confidential** application for the RFC 8628 device flow (`fishhawk token login --provider gitlab`), with **no fallback** to `oauthClientId` (`serve.go`, E66.4 / #2392) |
| `installationHostAllowlist` | `FISHHAWKD_GITLAB_INSTALLATION_HOST_ALLOWLIST` | Mode-2 per-installation base-URL host allowlist; genuinely non-secret, so the ConfigMap is correct for it |

| `secrets.values` field | Secret key | Required when |
|---|---|---|
| `gitlabToken` | `FISHHAWKD_GITLAB_TOKEN` | **never** — supplied-or-not |
| `gitlabWebhookSecret` | `FISHHAWKD_GITLAB_WEBHOOK_SECRET` | **never** — supplied-or-not |
| `gitlabOauthClientSecret` | `FISHHAWKD_GITLAB_OAUTH_CLIENT_SECRET` | `gitlab.oauthClientId` is set |

The three credentials go through the **Secret** in every mode
(`secrets.values.gitlab*` in `chartManaged`, `secrets.externalSecrets.data[]` in
`externalSecrets`, the pre-created Secret in `existing`) — **never**
`config.extraEnv`, whose suffix guard refuses any key ending in `_SECRET` /
`_TOKEN` by design (#2915).

Why the token and webhook secret are **never** required: `serve.go` WARNS and
leaves the gitlab forge/work-item provider disabled (`501`) on a
base-URL-without-token config rather than refusing, and documents base-URL-alone
as a supported login-gate posture; the webhook receiver is inert without a
secret. Mandating either would refuse every GitHub-only deploy — the #2915 shape
#2922 asks us not to repeat (mandating one half of a trio the chart cannot
complete). Only the OAuth client secret is required, and only when the client id
is set (the same derived requiredness the GitHub client secret uses).

**`fishhawk.validateGitLabOAuthTrio`** (included from `service.yaml`, after the
GitHub trio and before `validateSecretContract`) mirrors fishhawkd's own
`resolveGitLabOAuth` contract — all-three-or-none PLUS the base-URL requirement —
failing the render on every OBSERVABLE partial combination. Branches, in
evaluation order (helm `fail` halts at the first match):

1. `gitlab.oauthClientId` set + `gitlab.oauthCallbackUrl` empty → names the
   explicit-callback route and states the chart does **not** derive it from the
   ingress.
2. `gitlab.oauthCallbackUrl` set + `gitlab.oauthClientId` empty.
3. a GitLab OAuth client secret OBSERVABLY supplied + `gitlab.oauthClientId`
   empty (`chartManaged` reads `secrets.values.gitlabOauthClientSecret`;
   `externalSecrets` reads `data[]`; `existing` leaves the secret half UNKNOWN).
4. `gitlab.oauthClientId` set + `gitlab.baseUrl` empty → the extra refusal
   fishhawkd makes (no host to send the OAuth redirect to).

The id-set-but-secret-missing case is produced by `validateSecretContract` via
the derived requiredness above — one message per condition, not two guards
racing.

**Residual (honest):** the chart makes GitLab **configurable**; a GitLab repo
still cannot produce a run until an `installations` row exists
(`gitLabProjectRegistry`, `serve.go`), which has no CLI or API route today — the
separate onboarding gap #2922 itself flags.

## Secrets ([#849](https://github.com/kuhlman-labs/fishhawk/issues/849))

Sensitive env arrives via `envFrom` `secretRef` whose name is resolved
by the single `fishhawk.secretName` helper — the one source of truth
that the serve Deployment and the migrate Job both reference, so all
three provisioning modes converge on one name with no template
duplication. `secrets.mode` is:

- **`existing`** (default) — references a pre-created Secret via
  `values.existingSecret` (default `fishhawk-secrets`),
  `optional:false` → fail-loud if absent. Back-compat with the
  pre-#849 posture.
- **`chartManaged`** — the chart renders an Opaque Secret from
  `secrets.values`. DEV-ONLY. Named `<fullname>-secrets`.
- **`externalSecrets`** — emits an `ExternalSecret` CR
  (`templates/externalsecret.yaml`; apiVersion overridable, default
  `external-secrets.io/v1beta1`) whose `target.name` equals the
  converged Secret name, so the External Secrets Operator materializes
  the same-named Secret. A prod hook/foundation pairing with
  [#182](https://github.com/kuhlman-labs/fishhawk/issues/182); needs
  ESO + a pre-provisioned SecretStore.

### The credential contract (`fishhawk.secretKeySpec`, E62.2 / [#2301](https://github.com/kuhlman-labs/fishhawk/issues/2301))

Every key the chart expects in the Secret is declared ONCE, in the
`fishhawk.secretKeySpec` helper. It emits a YAML list of records, each
built as a dict and serialized with `toYaml` — never hand-written YAML
text, so a dynamic, dotted or dash-bearing `secretKey` (the PEM's)
round-trips through `fromYamlArray` as a well-formed string instead of
producing an invalid map key downstream.

The spec is keyed by **secret key** (the key in the Secret's `data`),
NOT by rendered env key. That re-keying is load-bearing: the GitHub App
PEM is not an env key at all (`envFrom` skips its dotted name), so an
env-keyed map had no home for it and silently left it unvalidated.

| Secret key | `secrets.values` field | Delivered as | Required when |
|---|---|---|---|
| `FISHHAWKD_DATABASE_URL` | `databaseUrl` | env (`envFrom`) | `postgres.enabled` is **false** |
| `FISHHAWKD_GITHUB_WEBHOOK_SECRET` | `githubWebhookSecret` | env | always |
| `FISHHAWKD_OAUTH_CLIENT_SECRET` | `oauthClientSecret` | env | `config.oauthClientId` is set (OAuth is a feature that can be off — see "OAuth trio") |
| `FISHHAWKD_ANTHROPIC_API_KEY` | `anthropicApiKey` | env | always |
| `AWS_ACCESS_KEY_ID` | `awsAccessKeyId` | env | `config.s3Bucket` is set |
| `AWS_SECRET_ACCESS_KEY` | `awsSecretAccessKey` | env | `config.s3Bucket` is set |
| `secrets.githubApp.privateKeyFile.secretKey` (dotted, default `github-app-private-key.pem`) | `githubAppPrivateKey` | **file** (projected volume) | `secrets.githubApp.privateKeyFile.enabled` |
| `FISHHAWKD_HANDOFF_SECRET` | `handoffSecret` | env | `cell.homeRegion` is set |
| `FISHHAWKD_MODEL_API_KEY` | `modelApiKey` | env | `cell.modelBaseUrl` is set |
| `FISHHAWKD_GITLAB_TOKEN` | `gitlabToken` | env | **never** (see "GitLab") |
| `FISHHAWKD_GITLAB_WEBHOOK_SECRET` | `gitlabWebhookSecret` | env | **never** (see "GitLab") |
| `FISHHAWKD_GITLAB_OAUTH_CLIENT_SECRET` | `gitlabOauthClientSecret` | env | `gitlab.oauthClientId` is set |

Requiredness is **derived**, not hardcoded. A blanket "always required"
would break the local profile: in-cluster Postgres supplies
`FISHHAWKD_DATABASE_URL` through a container-level env entry that
overrides `envFrom`, so the Secret key is genuinely optional there.
`FISHHAWKD_OAUTH_CLIENT_SECRET` derives the same way (#2915): OAuth is a
feature that can be off, so it is required only when `config.oauthClientId`
is set — a blanket "always required" would refuse every OAuth-off deploy.

Four consumers read that one spec and nothing else:
`templates/secret.yaml` ranges it to render `chartManaged` keys,
`fishhawk.requiredSecretKeys` filters it, `fishhawk.validateSecretContract`
validates against it, and `NOTES.txt` prints it. One source, one render
path, one validation path.

### Per-mode validation

`fishhawk.validateSecretContract` is `include`d once from
`templates/service.yaml` — the only template that renders in EVERY
topology mode AND every secrets mode, which is why `validateMode`
already lives there. What it CAN check is mode-dependent, and it says
so rather than quietly doing less:

| Mode | Render-time check | What is NOT checked at render time |
|---|---|---|
| `chartManaged` | each required record's `secrets.values` field is present **and non-empty** (an empty string fails, naming both the secret key and the values field) | nothing — the values are visible |
| `externalSecrets` | each required secret key appears in the declared `secrets.externalSecrets.data[]` mapping | whether the backing store actually holds that remote ref (the chart does not install the ESO CRDs) |
| `existing` | `existingSecret` is non-empty | **the Secret's contents.** Key presence is enforced by Kubernetes at pod start (`envFrom` `secretRef` is non-optional, and the PEM `items:` projection refuses to start the pod on a missing key). Value-level emptiness is a live-drill item |

### Why `existing` mode has no render-time key check

The obvious guard — `lookup "v1" "Secret" .Release.Namespace <name>`
— was built, tested, and **retired**. Two determinations, one
empirical and one from source:

1. **Empirical.** Rendering a probe chart calling
   `lookup "v1" "Secret" .Release.Namespace "definitely-absent-secret"`
   under `helm template` with no cluster connection (helm v4.2.4)
   returned an **empty but non-nil** map: `isNil=false`, `isEmpty=true`,
   `len=0`, exit 0. So a lookup-based presence guard fails on EVERY
   dry-run render — including `scripts/test-helm-render` and every
   operator `helm template` review.
2. **From source.** `helm.sh/helm` `pkg/engine/lookup_func.go`,
   `newLookupFunction`, returns `map[string]any{}, nil` ONLY inside the
   `if apierrors.IsNotFound(err)` branch; **every** other error,
   `Forbidden` included, is returned as `map[string]any{}, err`, and the
   non-nil error propagates out of the template engine as a render
   failure.

Together those refute the premise in both directions: an authorization
failure is NOT indistinguishable from an empty result (it hard-fails
the render), and the empty result such a guard would key on is exactly
what a cluster-less render produces. A guard built on it would be
unusable in dry-run and would make the chart un-installable for an
identity lacking `secrets:get`. So the chart **fails open** here, by
choice, and says so; the live drill below records the real behaviour
under an authorized and an under-privileged identity rather than
predicting it.

### GitHub App private key

Never an env string. It lives in the same Secret under a dotted key
(`secrets.githubApp.privateKeyFile.secretKey`, default
`github-app-private-key.pem`) that `envFrom` skips (a dotted name is
not a valid env identifier), projected read-only as a single file via a
Deployment volume/`subPath` mount at
`secrets.githubApp.privateKeyFile.mountPath`. The non-secret path is
advertised to fishhawkd as `FISHHAWKD_GITHUB_APP_PRIVATE_KEY_FILE` in
the ConfigMap (pre-existing in `serve.go`, only newly surfaced by the
chart).

The GitHub App is **all-or-nothing at the fishhawkd level**: `serve.go`
requires `config.githubAppId` (`FISHHAWKD_GITHUB_APP_ID`) **and** a
mounted, parseable PEM together, or neither. With only one set it exits
at boot (`github app misconfigured: both --github-app-id and
--github-app-private-key-file required`); with neither set it logs a
Warn (`FISHHAWKD_GITHUB_APP_ID not set; webhook dispatch and GitHub-side
actions will be disabled`) and runs with GitHub-side actions off.

`values-local.yaml` ships the App **OFF**
(`config.githubAppId: ""` + `secrets.githubApp.privateKeyFile.enabled:
false`, no `secrets.values.githubAppPrivateKey`) because a placeholder
PEM cannot parse and fishhawkd parses it **eagerly at boot** — so a
committed placeholder is not a render-only concern, it crashloops the
pod ([#2914](https://github.com/kuhlman-labs/fishhawk/issues/2914)). A
real deploy with a typo'd key still refuses to boot rather than silently
degrading; the eager parse is deliberately left fail-closed.

To turn the App on for a local cluster, generate a **throwaway** key and
pass it at install time (never commit a key to the repo):

```sh
openssl genrsa 2048 > /tmp/fh-dev-key.pem   # LibreSSL on macOS emits PKCS#1; do NOT pass OpenSSL 3's -traditional (LibreSSL rejects it)
helm upgrade --install fishhawk deploy/helm/fishhawk -f deploy/helm/fishhawk/values-local.yaml \
  --set config.githubAppId=<your-app-id> \
  --set secrets.githubApp.privateKeyFile.enabled=true \
  --set-file secrets.values.githubAppPrivateKey=/tmp/fh-dev-key.pem
```

A throwaway key authenticates to nothing — it only gets fishhawkd past
the PEM parse, not onto GitHub. `githubapp.NewSignerFromPEM` accepts
both PKCS#1 (`BEGIN RSA PRIVATE KEY`) and PKCS#8, so either form works.

### Deploy-time secrets guard

`fishhawk.validateSecrets` — included once from whichever serve
Deployment renders (`deployment.yaml` in allInOne,
`deployment-api.yaml` in split; the #847 carry-over) — `fail`s the
render outside `profile: local` when a dev-only convenience is active:

- `chartManaged` mode;
- in-cluster Postgres with the default password;
- in-cluster MinIO with the default rootPassword;
- the in-cluster Jaeger trace collector (`jaeger.enabled` —
  ephemeral/unauthenticated, dev/dogfooding only).

It also fails `externalSecrets` mode with an empty
`secretStoreRef.name` in ANY profile. `profile` (default `prod`, set to
`local` by `values-local.yaml`) is the local-vs-prod signal the guard
keys off.

## Ingress + cert-manager TLS ([#850](https://github.com/kuhlman-labs/fishhawk/issues/850))

Gated by `ingress.enabled` (default false → `templates/ingress.yaml` is
inert for existing installs; local uses port-forward/NodePort). When
enabled it renders one `networking.k8s.io/v1` Ingress for the required
`ingress.host` (the render `fail`s loud if unset):

- `ingressClassName` emitted only when `ingress.className` is set.
- A single rule routing `ingress.path`/`ingress.pathType` (pathType
  always emitted, as v1 requires) to the fishhawkd Service's named
  `http` port.
- TLS driven by `ingress.tls.enabled`: adds a `tls` block listing
  `[host]` with `secretName` defaulting to `<fullname>-tls`, and —
  when `ingress.tls.clusterIssuer` is set — merges the
  `cert-manager.io/cluster-issuer` annotation. cert-manager (not the
  chart) provisions the TLS Secret from that ClusterIssuer — an
  operator/cluster prerequisite.

When the ingress is enabled and `config.externalUrl` /
`config.oauthCallbackUrl` are left empty, they DERIVE from the ingress
host via the `fishhawk.externalUrl` / `fishhawk.oauthCallbackUrl`
helpers (consumed by the ConfigMap): `<scheme>://<host>` and
`<scheme>://<host>/v0/auth/github/callback` (the path `serve.go`
registers), scheme `https` when TLS is on else `http`. Explicit
`config.*` values always override.

`values-prod.yaml` is a worked prod example (ingress + TLS on) parallel
to `values-local.yaml` (ingress explicitly off); it also carries a
commented split-mode example.

## DB-migration hook Job ([#848](https://github.com/kuhlman-labs/fishhawk/issues/848))

Gated by `migrate.enabled`; runs `fishhawkd migrate up`, reusing the
same image with `args: [migrate, up]` overriding the `serve` CMD —
`serve` does NOT auto-migrate, so this is the sole migration path —
before serve handles traffic against an unmigrated DB. The k8s analog
of the ECS one-shot migrate Fargate task.

The hook phase is conditional on `postgres.enabled`:

- `pre-install,pre-upgrade` for the external-DB baseline (the prod/ECS
  analog, so serve never starts against an unmigrated DB);
- `post-install,post-upgrade` for the in-cluster local stack — a
  pre-install hook depending on the in-cluster Postgres (a normal
  resource Helm creates only AFTER all pre-install hooks complete)
  would deadlock.

Retry on a not-yet-ready DB is handled by Job `backoffLimit` (a fresh
pod per attempt; the distroless image has no shell for a wait loop).
`hook-delete-policy: before-hook-creation,hook-succeeded` retains a
failed Job for `kubectl logs` while leaving no orphans across upgrades.

### `restartPolicy: Never`, not `OnFailure`

Two reasons, both about the failure path this Job exists to make
diagnosable. Under `OnFailure` the Kubernetes docs note that the pod
running the Job is **terminated** once the backoff limit is reached,
which makes retrieving the logs carrying the SQL error materially
harder — and they recommend `Never` for exactly that reason. Second,
under `OnFailure` retries are governed by kubelet's CrashLoopBackOff
schedule (10s..300s) rather than the Job-controller schedule the timing
derivation below assumes, so that derivation would not hold. With
`Never`, each attempt leaves a distinct `Failed` pod whose logs persist
for `kubectl logs`.

### Failure timing, derived

One counting convention, recorded once (also in `values.yaml`'s
`migrate` block):

- The Job controller marks the Job `Failed` with reason
  `BackoffLimitExceeded` when `status.failed` **exceeds**
  `backoffLimit`, so total attempts = `backoffLimit + 1`.
- Cumulative inter-attempt backoff is the 10s-doubling series
  (10, 20, 40, 80, 160, 320; each term capped at 6 minutes) summed over
  `backoffLimit` terms.
- `timeToFailed = (backoffLimit + 1) * assumedAttemptSeconds + backoff`

At the shipped `backoffLimit: 2` / `assumedAttemptSeconds: 60` that is
`3 * 60 + (10 + 20) = 210s`, inside Helm's **300s default `--timeout`**
with 90s of margin. The rejected alternative is why the number moved:
`backoffLimit: 3` gives `4 * 60 + 70 = 310s`, already past Helm's
default timeout — the release would report a Helm timeout instead of
the migration error, which is the whole point of the failure path.

**210s is a MODEL OUTPUT and a LOWER BOUND, not an observed number.**
The Kubernetes backoff series resets when no new failed pods appear, so
real wall-clock time-to-Failed can exceed it. Nothing in this chart
asserts the figure is correct: `fishhawk.validateMigrateTiming` only
ever compares a **set** `migrate.activeDeadlineSeconds` AGAINST the
derived figure, failing the render when the deadline would win the race
(and naming both numbers). Because the figure is a lower bound, the
comparison is `<=`, not `<`: a deadline set to EXACTLY 210 fires at the
modelled moment the Job is marked Failed and so loses the race whenever
the model is not exact — the boundary is rejected on purpose, and
`scripts/test-helm-render` r4 pins `=210` as a REJECTED input alongside
`60` (far below) and `600` (far above). `null` and `0` both mean unset:
0 is not a meaningful ceiling (the Job controller's validation on the
field is exclusive-minimum 0), so the chart emits no field for it and
the guard has nothing to compare. The live drill below records the
OBSERVED time-to-Failed and Helm's actual error text — that is what
turns this model into evidence.

`migrate.activeDeadlineSeconds` defaults to **unset**. A deadline
generous enough for a slow but legitimate migration cannot also be
tight enough to beat the failure path, and when it fires it reports
`DeadlineExceeded`, hiding the migration error. The trade-off is
explicit: an unset deadline means a migration blocked on a lock runs
until Helm's own `--timeout` rather than being killed at a fixed point.
An operator who LOWERS `--timeout` below the derived time-to-Failed
sees a Helm timeout instead of the migration error; `--timeout` is a
client-side flag the chart cannot observe at render time, so that is
documented rather than guarded.

### No half-migrated schema

The claim "a failed migration leaves no half-migrated schema" is
enforced at the code layer, not only asserted here.
`backend/internal/postgres` runs each migration file through the pgx5
driver's implicit transaction, so a migration whose first statement
succeeds and whose second fails leaves **neither** object behind —
`TestMigrateUp_PartialMigrationLeavesNoSchemaBehind` asserts that
post-failure SCHEMA STATE directly (the object created by statement 1
does not exist), which is a stronger invariant than
`schema_migrations.dirty` plus a refusal. golang-migrate additionally
marks the version dirty, so the NEXT run refuses rather than proceeding;
`MigrateUp` enriches that refusal with the dirty version and the
recovery step instead of golang-migrate's bare text.

## Optional in-cluster Postgres + MinIO

Mirroring `docker-compose.yml`, gated by `postgres.enabled` /
`minio.enabled` (both default false, so the prod baseline points at
external DB/S3). Each is a single-replica Deployment + `ReadWriteOnce`
PVC + ClusterIP Service (`<fullname>-postgres` on 5432,
`<fullname>-minio` on 9000/9001), plus a post-install/upgrade
bucket-bootstrap Job (`minio.createBucket`) that retries
`mc alias set` then `mc mb --ignore-existing`.

`minio.mcImage` is pinned **independently** of `minio.image` — mc and
the MinIO server are **not** co-versioned, so `minio/mc` has no tag for
every `minio/minio` release, and the pin must name a tag that actually
exists in the registry. Because the bucket Job is a Helm **hook**, a
nonexistent tag surfaces only as a `helm upgrade` timeout (never a named
`ImagePullBackOff`) — the class of failure #2913 reports.
`scripts/test-helm-render` **r15** fail-closes the render gate on an
unpublished third-party image tag, and `scripts/dev k8s` dumps hook Job
+ pod state (naming the pull reason) on a helm failure. Verify a new
pin is pullable before bumping.

When `postgres.enabled`, the fishhawkd Deployment gets an explicit
container-level `FISHHAWKD_DATABASE_URL` env pointing at the in-cluster
Service, which overrides the same key from the Secret `envFrom`; the
in-cluster S3 endpoint is set via `config.s3Endpoint` (ConfigMap).

## Optional in-cluster Jaeger

An in-cluster **Jaeger all-in-one** — the local OTLP trace collector
for the runner's #649 GenAI spans (`templates/jaeger.yaml`, gated by
`jaeger.enabled` — default false, on in `values-local.yaml`,
dev/dogfooding only per the `validateSecrets` guard above) — ships as a
single-replica Deployment + ClusterIP Service exposing UI 16686 / OTLP
HTTP 4318 / OTLP gRPC 4317, with in-memory storage (no PVC).

It carries no fishhawkd wiring — fishhawkd does not emit spans; the
runner does, reaching the collector at the host's `localhost:4318` via
the port-forward `scripts/dev k8s` opens (the k8s analog of the `otel`
compose profile; see `scripts/README.md` "Local k8s ergonomics" and the
`docs/ARCHITECTURE.md` §10 "Local OTLP trace collector" entry).

## Resource requests and limits

Both are set on the fishhawkd container (`resources`) and on the
migrate Job (`migrate.resources`), and the reasoning is recorded beside
them in `values.yaml`. fishhawkd is a plain Go HTTP service — since the
2026-07-27 simplification that moved the `claudecode`/`codex` reviewers
to the **runner**, the control-plane image carries no agent CLIs and
spawns no reviewer subprocesses, so the pod is sized with no subprocess
headroom: 100m/128Mi requests (idle-to-light steady state of a Go
service with a pgx pool and a few timers) and 500m/512Mi limits (~5x CPU
burst, a memory ceiling that caps a leak at the pod rather than the
node). Limits are set on purpose: an unset memory limit leaves the pod
without a ceiling.

## Worker toggles

Worker enable toggles in `values.workers.*` render to
`FISHHAWKD_ENABLE_*` env (all default false). In allInOne keep
`replicaCount: 1` while any is true (enforced by
`fishhawk.validateTopology`), or switch to `deployment.mode=split` to
run them on the single `-worker` Deployment while the api tier scales.
Worker-singleton leader-election remains out of scope (#851).

## Values profiles

| File | Posture |
|---|---|
| `values-local.yaml` | localhost dev — in-cluster Postgres/MinIO/Jaeger, `chartManaged` secrets, ingress off |
| `values-prod.yaml` | production baseline — ingress + cert-manager TLS, `existing` secrets, derived URLs |
| `values-single-tenant.yaml` | ADR-057 **Mode 1** — one customer, own perimeter, `singleTenant.*` set |
| `values-cell.yaml` | ADR-057 **Mode 2** / ADR-062 — one regional cell, `cell.homeRegion` + `cell.modelBaseUrl`, workers on |

`values-prod`, `values-single-tenant` and `values-cell` are complete as
shipped: every field an install needs carries a concrete working value
(a real IngressClass, a real hostname, a real ClusterIssuer), so
substituting your own hostname and pre-creating the Secret is the whole
of the work. Their derived URLs are asserted against the chosen
hostname by `scripts/test-helm-render`, not merely rendered.

## Render gate (`scripts/test-helm-render`)

The chart is machine-checked by `scripts/test-helm-render`, run inside
`scripts/test verify` via `_verify_gate_harnesses`. It drives the REAL
chart through `helm template` / `helm lint` and asserts on shipped
rendered output — the credential-contract failure modes (one case per
named mode), the migrate Job's timing and `restartPolicy`, the
`envFrom` wiring across all three workload shapes plus the migrate Job,
the derived ingress URLs, the Mode-1 half-configured fail-closed case,
the OAuth-trio positive + OFF posture (r10) and one case per named
`fishhawk.validateOAuthTrio` failure mode (r11), the `config.extraEnv`
passthrough with its collision / identifier guards and an anti-drift
loop pinning `fishhawk.managedConfigKeys` to the rendered ConfigMap
(r12), a cross-boundary grep pinning the trio claim to `serve.go` (r13),
the GitLab family (r14), a **registry-existence check** over every
third-party image the chart RENDERS (r15 — extracted from
`helm template` output, classified by an anonymous Docker Hub
registry-v2 manifest HEAD into exists/missing/indeterminate, fail-closed
only on a definite 404 and guarded by a known-bad **sentinel** so an
unreachable registry can never green the case), and a
**selector-integrity check** ([r17](https://github.com/kuhlman-labs/fishhawk/issues/2916),
run in BOTH allInOne and split renders): r17a asserts every rendered
Service selects EXACTLY ONE workload pod, over a universe that includes
the migrate and minio-bucket **Job** pod templates (they carry the same
bare labels and are part of the shipped defect); r17b asserts
`svc/fishhawk`'s selector is the FULL `{name,instance,component}` set
(`server` allInOne, `api` split), not just the discriminator; r17c
asserts each Deployment's `spec.selector.matchLabels` identity set
EQUALS its pod-template set and that the primary Deployment carries the
full expected set; r17d asserts no pod set satisfies two DISTINCT
Deployment selectors. The complete mutation-to-verdict counterfactual
matrix (M0–M4) is recorded in the r17 header comment in
`scripts/test-helm-render`. A render + lint of
every profile rounds out the suite. It **skips with a printed reason
and exits 0** when `helm` is absent from PATH, so a helm-less host is
not red-lined; the cost is honest — on such a host the chart is
unguarded, the same residual the zsh guard already accepts for
`scripts/test-dev`.

### CI job — OPERATOR-INSTALLED, copy-paste verbatim

`.github/workflows/**` is in the implement stage's `forbidden_paths`,
so this job ships as a block for a human to install (the same pattern
`site/README.md` uses for the Pages workflow, #2261). Nothing
downstream checks it — an error here surfaces only when the workflow
runs. Save it as `.github/workflows/helm-render.yml` verbatim:

```yaml
name: Helm render

on:
  pull_request:
    branches: [main]
    paths:
      - 'deploy/helm/**'
      - 'scripts/test-helm-render'
      - '.github/workflows/helm-render.yml'
  push:
    branches: [main]
    paths:
      - 'deploy/helm/**'
      - 'scripts/test-helm-render'
      - '.github/workflows/helm-render.yml'

permissions:
  contents: read

concurrency:
  group: ${{ github.workflow }}-${{ github.ref }}
  cancel-in-progress: ${{ github.event_name == 'pull_request' }}

jobs:
  render:
    name: Chart render gate
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v7

      # Pinned to an immutable version per AGENTS.md's run-time tooling
      # pinning rule: a floating tag lets a third-party release red-line
      # main with no change on our side.
      - name: Install Helm
        uses: azure/setup-helm@v4
        with:
          version: v3.16.4

      - name: helm version
        run: helm version

      # The harness SKIPS when helm is absent; failing here if it is not
      # installed keeps a silent skip from passing as a green gate.
      - name: Assert helm is on PATH
        run: command -v helm

      - name: Render gate
        run: scripts/test-helm-render

      - name: Per-profile lint
        run: |
          for p in values-local values-prod values-cell values-single-tenant; do
            echo "== $p"
            helm lint deploy/helm/fishhawk -f "deploy/helm/fishhawk/$p.yaml"
          done
```

## Live operator drill (cluster-only)

Four things no render-time guard can check. This table is deliberately
EMPTY of results: the drill runs on real infrastructure, which the
change that added it could not reach. Record results against
[#2301](https://github.com/kuhlman-labs/fishhawk/issues/2301) (the
operator walk is ticketed separately at merge).

| # | Drill | Observable to record | Result |
|---|---|---|---|
| 1 | Install once with an authorized identity, once with an identity lacking `secrets:get` / `namespaces:get` | Does the render/install succeed, fail, or silently see an empty result? The plan-stage source reading says a `Forbidden` **hard-fails** the render — confirm or refute | |
| 2 | `existing` mode with a required key holding an **empty string**; separately, an `externalSecrets` mapping whose remote property is absent | What the pod does in each case (starts with an empty env value? refuses? CrashLoopBackOff with which message?) | |
| 3 | Deliberately fail a migration (point at a DB where the next migration cannot apply) | **Observed** wall-clock time-to-Failed vs the derived 210s, and Helm's ACTUAL error text | |
| 4 | End to end: confirm the release fails, the schema is not half-migrated, and `kubectl logs job/<fullname>-migrate` reaches the SQL error | Release status; **the same observable the Go test asserts** — the object created by the failed migration's FIRST statement does not exist (`SELECT to_regclass('<table>')` returns NULL); the SQL error text in the Job logs | |

Drill 4's schema observable is deliberately the same one
`TestMigrateUp_PartialMigrationLeavesNoSchemaBehind` asserts, so the
cluster run checks the invariant the docs claim rather than the weaker
`dirty`-plus-refusal.

### chartManaged boot walk — OPERATOR GATE, not a sandbox check ([#2915](https://github.com/kuhlman-labs/fishhawk/issues/2915))

That a `chartManaged` install with a complete OAuth trio actually BOOTS
is not something the render gate or the default-deny acceptance sandbox
can verify — it needs a live cluster. The operator walk is a concrete
command, not "install it somewhere and check health":

```sh
scripts/dev k8s   # builds the image into Docker Desktop's shared store,
                  # helm-installs with values-local.yaml, waits for the
                  # rollout, port-forwards svc/fishhawk 8080:8080, and gates
                  # on /healthz
curl -fsS http://localhost:8080/healthz   # then probe health directly
```

`values-local.yaml` now ships a complete trio (client id +
`chartManaged` client secret + explicit callback), so this produces a
fishhawkd that starts rather than exiting at `oauth misconfigured`. This
is an operator gate walked at merge, not part of `scripts/test verify`.

## Verify

`values-local.yaml` is a localhost-flavored override that turns
Postgres + MinIO on for a self-contained Docker-Desktop stack and
renders standalone.

```sh
scripts/test-helm-render     # the full render gate (also run by `scripts/test verify`)
```

```sh
helm lint deploy/helm/fishhawk
helm template fishhawk deploy/helm/fishhawk -f deploy/helm/fishhawk/values-local.yaml
helm template fishhawk deploy/helm/fishhawk -f deploy/helm/fishhawk/values-prod.yaml   # ingress/TLS posture
helm template fishhawk deploy/helm/fishhawk -f deploy/helm/fishhawk/values-single-tenant.yaml  # ADR-057 Mode 1
helm template fishhawk deploy/helm/fishhawk -f deploy/helm/fishhawk/values-cell.yaml           # ADR-057 Mode 2
# confirm the credential contract fails a missing required key:
helm template fishhawk deploy/helm/fishhawk --set secrets.mode=existing --set existingSecret=
# confirm the OAuth trio guard fails each partial combination (all-three-or-none):
helm template fishhawk deploy/helm/fishhawk --set config.oauthClientId=cid   # id without a callback
helm template fishhawk deploy/helm/fishhawk --set config.oauthCallbackUrl=https://x/cb   # callback without an id
# confirm the GitLab OAuth trio guard mirrors resolveGitLabOAuth (all-three-or-none + base URL):
helm template fishhawk deploy/helm/fishhawk --set gitlab.baseUrl=https://gl.x --set gitlab.oauthClientId=cid   # id without a callback
helm template fishhawk deploy/helm/fishhawk --set gitlab.oauthClientId=cid --set gitlab.oauthCallbackUrl=https://x/cb   # id without a base URL
# GitLab is off by default (GitHub-only emits zero GitLab keys); a complete config renders all of them:
helm template fishhawk deploy/helm/fishhawk | grep -c FISHHAWKD_GITLAB_   # → 0
helm template fishhawk deploy/helm/fishhawk -f deploy/helm/fishhawk/values-local.yaml \
  --set gitlab.baseUrl=https://gl.x --set gitlab.oauthClientId=cid \
  --set gitlab.oauthCallbackUrl=https://gl.x/v0/auth/gitlab/callback \
  --set secrets.values.gitlabOauthClientSecret=s | grep FISHHAWKD_GITLAB_
# confirm the extraEnv guards fire on a collision and an invalid identifier:
helm template fishhawk deploy/helm/fishhawk --set config.extraEnv.FISHHAWKD_ADDR=x   # collides
helm template fishhawk deploy/helm/fishhawk --set config.extraEnv.9bad=x              # invalid identifier
# confirm the migrate timing guard rejects a deadline that would win the race,
# including the boundary case equal to the derived (lower-bound) figure:
helm template fishhawk deploy/helm/fishhawk --set migrate.activeDeadlineSeconds=60
helm template fishhawk deploy/helm/fishhawk --set migrate.activeDeadlineSeconds=210
# split topology (two Deployments):
helm template fishhawk deploy/helm/fishhawk --set deployment.mode=split --set workers.slaTimer=true
# confirm svc/fishhawk carries the component discriminator (server allInOne, api split):
helm template fishhawk deploy/helm/fishhawk -f deploy/helm/fishhawk/values-local.yaml \
  --show-only templates/service.yaml | grep 'app.kubernetes.io/component'   # → server
# confirm the allInOne topology guard fails:
helm template fishhawk deploy/helm/fishhawk --set replicaCount=2 --set workers.slaTimer=true
# Mode-1 profile + GHES/EMU endpoints: unset renders NO such key, set renders each.
helm template fishhawk deploy/helm/fishhawk | grep -c FISHHAWKD_SINGLE_TENANT_   # → 0
helm template fishhawk deploy/helm/fishhawk \
  --set singleTenant.accountKey=acme-corp \
  --set config.githubApiUrl=https://ghes.acme.example/api/v3 | grep -E 'SINGLE_TENANT|GITHUB_API_URL'
```
