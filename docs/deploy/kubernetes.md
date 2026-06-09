# Local Kubernetes quickstart

One-command bring-up of fishhawkd on a local Kubernetes cluster, using the Helm
chart under `deploy/helm/fishhawk/`. This is the M1 "works on Docker Desktop"
path (ADR-034); it is an operator smoke test for the chart, not exercised in CI
(no cluster is available there).

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
   `ghcr.io/kuhlman-labs/fishhawkd:dev-local`.
2. Runs `helm upgrade --install fishhawk deploy/helm/fishhawk -f
   deploy/helm/fishhawk/values-local.yaml --set image.tag=dev-local --set
   image.pullPolicy=IfNotPresent`. The `--set` overrides point the chart at the
   local build instead of the `main` ghcr tag `values-local.yaml` declares.
   `helm upgrade --install` is idempotent, so re-running the command is safe.
3. Waits for the Deployment rollout (`kubectl rollout status`, 120s timeout).
4. Opens `kubectl port-forward svc/fishhawk 8080:8080` in the background and
   polls `http://localhost:8080/healthz` until fishhawkd answers healthy.

The `/healthz` poll is the authoritative readiness signal. With the in-cluster
Postgres `values-local.yaml` enables, the migration Job runs as a
`post-install,post-upgrade` hook, so `kubectl rollout status` can report the
Deployment available before migrations finish — fishhawkd only answers `/healthz`
healthy after its own startup completes against the migrated DB.

On a stuck rollout or a `/healthz` timeout the command tails `kubectl get pods` +
`kubectl logs deploy/fishhawk` to stderr, kills the port-forward, and exits
non-zero (the same fail-loud contract as `scripts/dev up`).

## Reaching fishhawkd

While the bring-up's port-forward is alive, fishhawkd is reachable at
`http://localhost:8080`. To re-establish a forward later:

```sh
kubectl port-forward svc/fishhawk 8080:8080
```

Local uses port-forward (or a NodePort) rather than an Ingress;
`values-local.yaml` sets `ingress.enabled: false` so `config.externalUrl` /
`config.oauthCallbackUrl` are used verbatim.

## Tear down

```sh
scripts/dev k8s-down   # or: make k8s-down
```

Kills the tracked port-forward (pid in `.fishhawk/k8s-pf.pid`) and runs
`helm uninstall fishhawk`. Both steps are idempotent, so a double teardown is a
no-op.

## values-local vs values-prod

The chart ships two worked override files (see the chart row in
[`docs/ARCHITECTURE.md`](../ARCHITECTURE.md) §10 for the full template surface):

| | `values-local.yaml` | `values-prod.yaml` |
|---|---|---|
| `profile` | `local` (permits dev-only conveniences) | `prod` |
| Postgres / MinIO | in-cluster (`postgres.enabled`, `minio.enabled`) | external DB / S3 |
| Secrets | `chartManaged` dev Secret with placeholders | `existing` / `externalSecrets` |
| Ingress / TLS | off (port-forward / NodePort) | Ingress + cert-manager TLS on |

The `profile: local` signal is what lets `fishhawk.validateSecrets` permit the
chart-managed Secret and the default in-cluster DB/MinIO credentials; a real
cluster MUST keep `profile: prod`.

## Status

Ingress + cert-manager TLS (#850) and ExternalSecrets (#849) ship as prod
foundations in the chart. Worker-singleton leader election is out of scope
(#851): in `allInOne` mode keep `replicaCount: 1` while any worker toggle is on,
or use `deployment.mode=split` to scale the api tier independently of the single
worker Deployment.
