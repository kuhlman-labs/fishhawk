# scripts

Operator/dev tooling. `scripts/dev` and `scripts/test` carry their core
contracts in `AGENTS.md`; this file holds the relocated detail entries.

## Local k8s ergonomics (ADR-034 / [#852](https://github.com/kuhlman-labs/fishhawk/issues/852))

`scripts/dev k8s` / `scripts/dev k8s-down` (thin Makefile aliases
`make k8s-up` / `make k8s-down`) — one-command bring-up/teardown of the
Helm chart on Docker Desktop's Kubernetes.

`cmd_k8s_up`:

- Builds the fishhawkd image into the host Docker daemon as
  `ghcr.io/kuhlman-labs/fishhawkd:dev-local` (Docker-Desktop k8s shares
  that image store — no registry push / kind load).
- `helm upgrade --install`s the chart with `values-local.yaml` plus
  `--set image.tag=dev-local --set image.pullPolicy=IfNotPresent`
  (overriding values-local's `main`/`Always` so the local build is
  used).
- Waits for the rollout, then opens a
  `kubectl port-forward svc/fishhawk 8080:8080` and gates on `/healthz`
  via the same `_await_healthz` poll `cmd_up` uses — the authoritative
  readiness signal, since the in-cluster migrate Job runs as a
  `post-install` hook and rollout-status can go green before it
  finishes.
- Fails loud on a stuck rollout or `/healthz` timeout: kubectl
  pods + logs tail to stderr, non-zero exit.

### Jaeger port-forward

When the dev-only in-cluster Jaeger is present (`values-local.yaml`
enables `jaeger.enabled`), `cmd_k8s_up` opens a second
`kubectl port-forward svc/fishhawk-jaeger 16686:16686 4318:4318` AFTER
the `/healthz` gate — Service-guarded, so a jaeger-disabled override is
a clean skip; pid tracked in `.fishhawk/k8s-jaeger-pf.pid` — so the
host-spawned runner can emit spans to `localhost:4318` and the operator
can view the Jaeger UI at `localhost:16686`.

### Teardown

`cmd_k8s_down` kills both tracked port-forwards (fishhawkd pid in
`.fishhawk/k8s-pf.pid`, jaeger pid in `.fishhawk/k8s-jaeger-pf.pid`,
mirroring `PID_FILE`) and `helm uninstall`s (idempotent).

### Testing and docs

The pure helpers `_k8s_image_ref` / `_k8s_healthz_url` are unit-tested
by `scripts/test-dev`. Operator quickstart + the values-local-vs-prod
split: `docs/deploy/kubernetes.md`. The true end-to-end path (image
build → chart install → `/healthz` green) is an operator smoke test
against a Docker-Desktop cluster, not run in CI.
