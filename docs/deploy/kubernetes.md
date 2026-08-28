# Local Kubernetes quickstart

One-command bring-up of fishhawkd on a local Kubernetes cluster, using the Helm
chart under `deploy/helm/fishhawk/`. This is the M1 "works on Docker Desktop"
path (ADR-034); it is an operator smoke test for the chart, not exercised in CI
(no cluster is available there).

## Deployment modes

The chart serves both ADR-057 deployment modes; only the values differ.

- **Mode 1 — self-hosted, single tenant**: one customer, own perimeter, one
  implicit account bootstrapped from `singleTenant.accountKey`. See
  [self-hosted.md](self-hosted.md).
- **Mode 2 — hosted regional**: many accounts across N cells behind a global
  directory. Runbook: [hosted-regional.md](hosted-regional.md); protocol:
  [regional-cells.md](regional-cells.md).

The rest of this page is the local Docker-Desktop bring-up, which is neither —
it renders with both profiles unset.

## Prerequisites

- **Docker Desktop with Kubernetes enabled** (Settings → Kubernetes → Enable
  Kubernetes). Docker Desktop's Kubernetes shares the host Docker daemon's image
  store, so an image built locally with `docker build` is directly resolvable
  in-cluster — no registry push or `kind load` is required.
- **`helm`** (v3) and **`kubectl`** on `PATH`, with the current context pointed
  at the Docker-Desktop cluster (`kubectl config use-context docker-desktop`).

## Bring up

```sh
scripts/dev k8s        # or: make k8s-up
```

This:

1. Builds the fishhawkd image into the host Docker daemon as
   `ghcr.io/kuhlman-labs/fishhawkd:dev-local`, for the host architecture —
   BuildKit's `TARGETARCH` automatic platform ARG defaults to the host
   platform, so on an Apple Silicon host the image is arm64 and runs
   natively on the Docker-Desktop node rather than under emulation.
2. Runs `helm upgrade --install fishhawk deploy/helm/fishhawk -f
   deploy/helm/fishhawk/values-local.yaml --set image.tag=dev-local --set
   image.pullPolicy=IfNotPresent`. The `--set` overrides point the chart at the
   local build instead of the `main` ghcr tag `values-local.yaml` declares.
   `helm upgrade --install` is idempotent, so re-running the command is safe.
3. Waits for the Deployment rollout (`kubectl rollout status`, 120s timeout).
4. Opens `kubectl port-forward svc/fishhawk 8080:8080` in the background and
   polls `http://localhost:8080/healthz` until fishhawkd answers healthy.
5. If the dev-only in-cluster Jaeger is present (`values-local.yaml` enables it),
   opens a second forward for its UI (`16686`) and OTLP HTTP receiver (`4318`).
   See [Tracing (Jaeger)](#tracing-jaeger) below.

The `/healthz` poll is the authoritative readiness signal. With the in-cluster
Postgres `values-local.yaml` enables, the migration Job runs as a
`post-install,post-upgrade` hook, so `kubectl rollout status` can report the
Deployment available before migrations finish — fishhawkd only answers `/healthz`
healthy after its own startup completes against the migrated DB.

On a stuck rollout or a `/healthz` timeout the command tails `kubectl get pods` +
`kubectl logs deploy/fishhawk` to stderr, kills the port-forward, and exits
non-zero (the same fail-loud contract as `scripts/dev up`).

**Upgrading an existing dev release across chart 0.3.0.** Chart 0.3.0 adds
`app.kubernetes.io/component: server` to the allInOne fishhawkd Deployment's
`spec.selector.matchLabels`, and a Deployment's `spec.selector` is **immutable**
in the Kubernetes API. So `scripts/dev k8s` against a release first installed on
chart 0.2.x fails with a `field is immutable` error rather than reconciling — the
`helm upgrade --install` is idempotent for VALUE changes, not for this selector
change. The clean path is `scripts/dev k8s-down` (`helm uninstall`) then
`scripts/dev k8s` (fresh install). The full remedy set (including the
delete-Deployment-then-upgrade branch and the symmetric in-cluster rollback
caveat) is in
[the chart README's Upgrading section](../../deploy/helm/fishhawk/README.md).

## Reaching fishhawkd

While the bring-up's port-forward is alive, fishhawkd is reachable at
`http://localhost:8080`. To re-establish a forward later:

```sh
kubectl port-forward svc/fishhawk 8080:8080
```

Local uses port-forward (or a NodePort) rather than an Ingress;
`values-local.yaml` sets `ingress.enabled: false` so `config.externalUrl` /
`config.oauthCallbackUrl` are used verbatim.

`kubectl port-forward svc/fishhawk 8080:8080` and `kubectl logs deploy/fishhawk`
now resolve **deterministically** to the fishhawkd pod. The Service + Deployment
carry an `app.kubernetes.io/component: server` discriminator
([#2916](https://github.com/kuhlman-labs/fishhawk/issues/2916)), so the selector
no longer also matches the in-cluster postgres/minio/jaeger pods or the
migrate/minio-bucket hook Job pods — before this, a selector-resolving command
picked an arbitrary matching pod and could return jaeger's logs. Confirm exactly
one pod backs the Service:

```sh
kubectl -n fishhawk get pods \
  -l app.kubernetes.io/name=fishhawk,app.kubernetes.io/instance=fishhawk,app.kubernetes.io/component=server
```

## Frontend (SPA)

The SPA frontend is hosted statically out-of-cluster (GitHub Pages, a CDN, or
object storage); the Helm chart serves the fishhawkd API only. There is no
in-cluster nginx Deployment/Service and no second built image — the chart stays
image-build-free, depending solely on the published `fishhawkd` image (#846).

Point the static SPA's API base URL at the chart's `config.externalUrl`:

- **Ingress enabled** — `config.externalUrl` is the ingress host
  (`<scheme>://<ingress.host>`, https when `ingress.tls.enabled`, else http; the
  #850 derivation). Set the SPA's API base to that value.
- **Local / port-forward** — `ingress.enabled: false`, so `config.externalUrl`
  is used verbatim. With the bring-up's forward alive, that is
  `http://localhost:8080`.

The OAuth callback host (`config.oauthCallbackUrl`) must match the SPA host so
the sign-in redirect returns to the served origin.

A working GitHub sign-in needs all THREE OAuth parts, or none (fishhawkd enforces
all-three-or-none and exits at `oauth misconfigured` on a partial set,
`serve.go`; the chart mirrors that via `fishhawk.validateOAuthTrio`):

- **client id** — `config.oauthClientId` (public; rendered into the ConfigMap as
  `FISHHAWKD_OAUTH_CLIENT_ID`). It is the enablement signal: setting it makes the
  client secret required and makes the ingress-derived callback URL render.
- **client secret** — `FISHHAWKD_OAUTH_CLIENT_SECRET`, in the Secret.
- **callback URL** — `config.oauthCallbackUrl`, or the ingress derivation when
  `config.oauthClientId` is set.

`values-local.yaml` now ships a dev client id so the local stack renders a
complete trio and boots. Leave `config.oauthClientId` empty for an OAuth-OFF
install (fishhawkd then serves `/v0/auth/github/*` as `503`).

### GitHub App (local)

The GitHub App is all-three-or-none in the same shape: fishhawkd requires
`config.githubAppId` (`FISHHAWKD_GITHUB_APP_ID`) **and** a mounted, parseable
PEM together, or neither, and it parses the PEM **eagerly at boot** — a
placeholder key crashloops the pod
([#2914](https://github.com/kuhlman-labs/fishhawk/issues/2914)). So
`values-local.yaml` now ships the App **off** (`config.githubAppId: ""` +
`secrets.githubApp.privateKeyFile.enabled: false`, no committed PEM), and the
shipped local values boot unmodified. To enable it for a local cluster, generate
a throwaway key and pass it at install time (never commit a key):

```sh
openssl genrsa 2048 > /tmp/fh-dev-key.pem
helm upgrade --install fishhawk deploy/helm/fishhawk -f deploy/helm/fishhawk/values-local.yaml \
  --set config.githubAppId=<your-app-id> \
  --set secrets.githubApp.privateKeyFile.enabled=true \
  --set-file secrets.values.githubAppPrivateKey=/tmp/fh-dev-key.pem
```

See the chart README's "GitHub App private key" section for the full recipe. A
throwaway key gets fishhawkd past the parse, not onto GitHub.

For any `FISHHAWKD_*` env var the chart has no dedicated field for, use
`config.extraEnv` — a map of NON-SECRET name → value merged verbatim into the
ConfigMap (a collision with a chart-managed key or an invalid env identifier
fails the render; secrets belong in the Secret, not here).

Serving the SPA from an in-cluster nginx Deployment is intentionally out of
scope (decided against on #853), keeping the chart image-build-free per #846.

## Tracing (Jaeger)

`values-local.yaml` enables an in-cluster **Jaeger all-in-one** (`jaeger.enabled`)
— the k8s analog of the opt-in `otel` profile in `docker-compose.yml`, and the
local OTLP collector for the runner's per-run GenAI trace spans (the `stage`/`chat`
span shape is in [`docs/ARCHITECTURE.md`](../ARCHITECTURE.md) §10). It is
**DEV / DOGFOODING ONLY**: an ephemeral, unauthenticated collector with in-memory
span storage (no PVC). `fishhawk.validateSecrets` fails the render outside
`profile: local`, so it can never reach a prod cluster.

While the bring-up's Jaeger forward is alive:

- **Jaeger UI** — `http://localhost:16686`
- **OTLP HTTP receiver** — `http://localhost:4318` (the runner's `otlptracehttp`
  target)

**Execution-locality caveat.** fishhawkd does *not* emit these spans — the
`fishhawk-runner` does, and under the dogfood loop the runner is spawned by
`fishhawk-mcp` **on the operator's host** (inheriting that process's env), not
in-cluster. So the runner reaches the collector at the host's `localhost:4318`
through the forward, *not* via an in-cluster Service DNS name. To capture spans,
set `OTEL_EXPORTER_OTLP_ENDPOINT=http://localhost:4318` in the host environment
that spawns the runner (unset is a clean no-op). The same caveat as the compose
path applies: a runner executing on a GitHub-hosted CI runner sees its *own*
loopback, not yours — end-to-end local viewing requires the runner to run on this
host (the `runner_kind=local` flow).

To re-establish the Jaeger forward later:

```sh
kubectl port-forward svc/fishhawk-jaeger 16686:16686 4318:4318
```

## Tear down

```sh
scripts/dev k8s-down   # or: make k8s-down
```

Kills the tracked port-forwards (fishhawkd pid in `.fishhawk/k8s-pf.pid`, Jaeger
pid in `.fishhawk/k8s-jaeger-pf.pid`) and runs `helm uninstall fishhawk`. All
steps are idempotent, so a double teardown is a no-op.

## When the migration hook fails

The chart's `pre-install`/`pre-upgrade` migrate Job is what stops serve starting
against an unmigrated database, so its FAILURE path is the one worth knowing.

- **`restartPolicy: Never`** (not `OnFailure`). Each attempt leaves a distinct
  `Failed` pod whose logs persist, and the `hook-delete-policy`
  (`before-hook-creation,hook-succeeded` — deliberately no `hook-failed`)
  retains the Job to reach them:

  ```sh
  kubectl logs job/<release>-migrate            # the SQL error, verbatim
  kubectl get pods -l app.kubernetes.io/component=migrate
  ```

- **The release does not go green.** Helm reports the hook failure and the
  fishhawkd Deployment is not created (external-DB baseline) or does not begin
  serving a migrated schema.
- **No half-migrated schema.** A migration whose first statement succeeds and
  whose second fails leaves NEITHER object behind — the pgx5 driver runs the
  file in an implicit transaction. Confirm with
  `SELECT to_regclass('<the table the failed migration would have created>')`,
  which returns NULL. golang-migrate additionally marks the version dirty, so a
  re-run REFUSES rather than proceeding; the refusal names the dirty version and
  the `force` recovery step.
- **Timing.** The Job gives up after `migrate.backoffLimit + 1` attempts. The
  derived time-to-Failed (210s at the shipped defaults) is a MODEL OUTPUT and a
  lower bound, sized to land inside Helm's 300s default `--timeout` so you see
  the migration error rather than a Helm timeout. `migrate.activeDeadlineSeconds`
  is unset by default on purpose — a fired deadline reports `DeadlineExceeded`
  and hides the migration error. Full derivation:
  [the chart README](../../deploy/helm/fishhawk/README.md).

## Chart render gate

`scripts/test-helm-render` drives the chart through `helm template` / `helm lint`
and asserts on rendered output — the credential-contract failure modes, the
migrate Job's timing and `restartPolicy`, the `envFrom` wiring, the derived
ingress URLs, the **selector-integrity** check (r17: every Service selects
exactly one workload pod, and `svc/fishhawk` carries the full
`{name,instance,component}` set — [#2916](https://github.com/kuhlman-labs/fishhawk/issues/2916)),
and a render + lint of every profile. It runs inside
`scripts/test verify` and skips (exit 0, printed reason) when `helm` is absent.

## values-local vs values-prod

The chart ships four worked override files (see the chart row in
[`docs/ARCHITECTURE.md`](../ARCHITECTURE.md) §10 for the full template surface):

| | `values-local.yaml` | `values-prod.yaml` |
|---|---|---|
| `profile` | `local` (permits dev-only conveniences) | `prod` |
| Postgres / MinIO | in-cluster (`postgres.enabled`, `minio.enabled`) | external DB / S3 |
| Jaeger (tracing) | in-cluster (`jaeger.enabled`) | off (dev-only) |
| Secrets | `chartManaged` dev Secret with dev values | `existing` / `externalSecrets` |
| GitHub App | off (generate a throwaway key to enable — see above) | App id + PEM in the Secret |
| Ingress / TLS | off (port-forward / NodePort) | Ingress + cert-manager TLS on |

Two more ship alongside them, both complete as written (real IngressClass, real
hostname, real ClusterIssuer — substitute your own and pre-create the Secret):
`values-single-tenant.yaml` (ADR-057 Mode 1 — see
[self-hosted.md](self-hosted.md)) and `values-cell.yaml` (ADR-057 Mode 2 /
ADR-062 — see [hosted-regional.md](hosted-regional.md)).

The `profile: local` signal is what lets `fishhawk.validateSecrets` permit the
chart-managed Secret, the default in-cluster DB/MinIO credentials, and the
dev-only Jaeger collector; a real cluster MUST keep `profile: prod` (which fails
the render if any of those is left on).

## Status

Ingress + cert-manager TLS (#850) and ExternalSecrets (#849) ship as prod
foundations in the chart. SPA serving (#853) resolved as static-out-of-cluster:
the chart serves the API only and the SPA is hosted separately (see the
"Frontend (SPA)" section above). Worker-singleton leader election is out of scope
(#851): in `allInOne` mode keep `replicaCount: 1` while any worker toggle is on,
or use `deployment.mode=split` to scale the api tier independently of the single
worker Deployment.
