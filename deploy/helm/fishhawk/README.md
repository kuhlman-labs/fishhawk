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
- In `split` mode the Service selector carries
  `app.kubernetes.io/component: api`, so HTTP + webhook traffic routes
  only to the api pods — which dedup webhook deliveries via the
  Postgres store whenever `FISHHAWKD_DATABASE_URL` is set (`serve.go`).
  The `-worker` pod (`component: worker`) is excluded from the Service
  endpoints; its probes hit the pod directly.

## Config

A ConfigMap carries the non-secret `FISHHAWKD_*` env: addr, S3
region/endpoint/bucket, external URL, OAuth callback/redirect, OIDC
audience/JWKS, GitHub App id, plan-review model, budget timezone.
Empty keys are omitted to match fishhawkd's ignore-if-unset semantics.

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

## Optional in-cluster Postgres + MinIO

Mirroring `docker-compose.yml`, gated by `postgres.enabled` /
`minio.enabled` (both default false, so the prod baseline points at
external DB/S3). Each is a single-replica Deployment + `ReadWriteOnce`
PVC + ClusterIP Service (`<fullname>-postgres` on 5432,
`<fullname>-minio` on 9000/9001), plus a post-install/upgrade
bucket-bootstrap Job (`minio.createBucket`) that retries
`mc alias set` then `mc mb --ignore-existing` (the mc image pinned to
the same `RELEASE.2025-01-20T14-49-07Z` tag as the server).

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

## Worker toggles

Worker enable toggles in `values.workers.*` render to
`FISHHAWKD_ENABLE_*` env (all default false). In allInOne keep
`replicaCount: 1` while any is true (enforced by
`fishhawk.validateTopology`), or switch to `deployment.mode=split` to
run them on the single `-worker` Deployment while the api tier scales.
Worker-singleton leader-election remains out of scope (#851).

## Verify

`values-local.yaml` is a localhost-flavored override that turns
Postgres + MinIO on for a self-contained Docker-Desktop stack and
renders standalone.

```sh
helm lint deploy/helm/fishhawk
helm template fishhawk deploy/helm/fishhawk -f deploy/helm/fishhawk/values-local.yaml
helm template fishhawk deploy/helm/fishhawk -f deploy/helm/fishhawk/values-prod.yaml   # ingress/TLS posture
# split topology (two Deployments):
helm template fishhawk deploy/helm/fishhawk --set deployment.mode=split --set workers.slaTimer=true
# confirm the allInOne topology guard fails:
helm template fishhawk deploy/helm/fishhawk --set replicaCount=2 --set workers.slaTimer=true
```
