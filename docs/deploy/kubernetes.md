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
  Kubernetes). Whether the cluster can see a locally built image depends on
  WHICH provisioner Docker Desktop installed — see the next section; the
  bring-up detects it and never needs a registry push.
- **`helm`** (v3) and **`kubectl`** (≥ 1.27 — `kubectl debug --profile` is
  required on the kind-based provisioner) on `PATH`, with the current context
  pointed at the Docker-Desktop cluster (`kubectl config use-context
  docker-desktop`).

### Image store: two provisioners

Docker Desktop has shipped two Kubernetes provisioners, and they differ in the
one property this bring-up depends on
([#3344](https://github.com/kuhlman-labs/fishhawk/issues/3344)):

| Provisioner | `kubectl get nodes -o name` | Image store |
|---|---|---|
| classic (cri-dockerd on the host daemon) | `node/docker-desktop` | **shared** with the host daemon — a `docker build` is directly visible to the kubelet |
| kind-based (what current Docker Desktop installs) | `node/desktop-control-plane` | **isolated** — the node runs its own containerd that the host daemon cannot see (`docker ps -a` shows no node container) |

On the kind-based provisioner a `docker build` never reaches the kubelet, and
`pullPolicy: IfNotPresent` turns "the image never arrived" into "reuse whatever
`dev-local` the node already holds": the bring-up used to go green while the
cluster served a STALE fishhawkd. `scripts/dev k8s` now:

- detects the provisioner **before** building (`kubectl get nodes -o name`),
  and **refuses** an unrecognised node list — it cannot tell whether the image
  it is about to build will ever reach the node, and proceeding is exactly the
  silent-stale outcome above;
- on the isolated provisioner, loads the built image into every node with ONE
  `docker save` + a privileged node-debug pod (`kubectl debug node/<n>
  --profile=sysadmin --image=debian:12-slim`, glibc so it can exec the node's
  own `ctr`) + `kubectl cp` + `ctr -n k8s.io images import`. Never a registry
  push (the [#2918](https://github.com/kuhlman-labs/fishhawk/issues/2918)
  egress discipline);
- **verifies image identity** after the readiness gate, fail-closed — see
  [Image identity](#image-identity-what-a-green-bring-up-now-proves).

Manual cross-check of the provisioner: `kubectl get nodes -o
jsonpath='{.items[*].status.nodeInfo.containerRuntimeVersion}'` prints
`docker://…` on the classic provisioner and `containerd://…` on the kind-based
one.

## Bring up

```sh
scripts/dev k8s        # or: make k8s-up
```

This:

1. Detects the image-store provisioner from `kubectl get nodes -o name`
   (`shared` / `isolated` / `unknown`, see above) and **fails closed** on
   `unknown` before building anything. `FISHHAWK_K8S_SKIP_IDENTITY=1` downgrades
   that refusal to a warning (and skips step 8).
2. Builds the fishhawkd image into the host Docker daemon as
   `ghcr.io/kuhlman-labs/fishhawkd:dev-local`, for the host architecture —
   BuildKit's `TARGETARCH` automatic platform ARG defaults to the host
   platform, so on an Apple Silicon host the image is arm64 and runs
   natively on the Docker-Desktop node rather than under emulation. The
   stamped `GIT_SHA` build arg is computed ONCE and reused by step 8.
3. On the isolated provisioner only: `docker save`s the image once and imports
   it into each node through a node-debug pod (kept alive for step 8, deleted
   on every exit path). On the shared provisioner this step prints `no load
   needed`.
4. Runs `helm upgrade --install fishhawk deploy/helm/fishhawk -f
   deploy/helm/fishhawk/values-local.yaml --set image.tag=dev-local --set
   image.pullPolicy=IfNotPresent`. The `--set` overrides point the chart at the
   local build instead of the `main` ghcr tag `values-local.yaml` declares.
   `helm upgrade --install` is idempotent, so re-running the command is safe.
5. Runs `kubectl rollout restart deployment/fishhawk` unconditionally: an
   unchanged rendered manifest (same tag, same values) triggers no rollout, so
   without this a rebuilt image would never be scheduled and the old pod would
   keep serving. On a fresh install it costs one extra rollout.
6. Waits for the Deployment rollout (`kubectl rollout status`, 120s timeout).
7. Opens `kubectl port-forward svc/fishhawk <pf-port>:8080` in the background and
   polls `http://localhost:<pf-port>/healthz` until fishhawkd answers healthy.
   `<pf-port>` defaults to `8080`; see [Overriding the forwarded host
   ports](#overriding-the-forwarded-host-ports).
8. Verifies **image identity** — the pod's running image ID equals the image
   step 2 built AND `/healthz` `git_sha` equals the stamped build SHA — and
   exits non-zero on any mismatch or unresolvable identity. See the next
   section.
9. If the dev-only in-cluster Jaeger is present (`values-local.yaml` enables it),
   opens a second forward for its UI (`16686`) and OTLP HTTP receiver (`4318`).
   See [Tracing (Jaeger)](#tracing-jaeger) below.

Before step 1, the command preflights the fishhawkd forward port and aborts —
naming every squatting pid and its command — if something already holds it. That
check runs *before* the image build so a collision costs seconds rather than a
multi-minute build followed by a misleading 60s `/healthz` timeout
([#2917](https://github.com/kuhlman-labs/fishhawk/issues/2917)). After the
readiness gate passes it also verifies the listener on that port really is the
forward it spawned (the #965 identity check), so a squatter that raced in after
the preflight cannot answer the gate on the forward's behalf — and only THEN
runs the image-identity gate of step 8, so a wrong listener is never mistaken
for a wrong image.

### Changing values only: the pod now restarts (chart 0.5.0, [#3577](https://github.com/kuhlman-labs/fishhawk/issues/3577))

A `helm upgrade` that changes ONLY a `config.*` or (under
`secrets.mode: chartManaged`) a `secrets.values.*` entry used to leave the
Deployment manifest byte-identical: every `FISHHAWKD_*` value arrives via
`envFrom`, read once at container start, so `helm upgrade` exited 0,
`kubectl rollout status` reported success, and the pod kept serving the OLD
value. Chart 0.5.0 hashes the rendered ConfigMap and Secret into
`checksum/config` / `checksum/secret` pod-template annotations, so such a change
moves `spec.template` and the pods actually roll. (`scripts/dev k8s` already
forces a `kubectl rollout restart` in step 5, so a local bring-up was never
exposed to this; a plain `helm upgrade` against any cluster was.)

> **RESIDUAL under `secrets.mode: existing` / `externalSecrets`.** There the
> chart does not own the Secret's contents — it sees only a name, or ESO
> materializes the values out of band — so `checksum/secret` is deliberately NOT
> emitted, and an out-of-band credential rotation still reports success while
> the running pod keeps the old credential. Restart explicitly:
> `kubectl -n <ns> rollout restart deployment/<release>-fishhawk` (split mode:
> the `-api` and `-worker` Deployments). The same applies to the GitHub App PEM,
> which fishhawkd reads at start even though the kubelet refreshes the mounted
> Secret volume in place. Full contract: `deploy/helm/fishhawk/README.md`
> § "Credential rotation and pod restart".

### Image identity: what a green bring-up now proves

A healthy `/healthz` through the right forward proves a fishhawkd is serving; it
does not prove it is the one you just built. `scripts/dev k8s` therefore ends
with `_verify_k8s_image_identity`, which returns 0 ONLY when BOTH checks match
and exits the bring-up non-zero otherwise — there is no warn-and-continue code:

- **Primary — image ID.** `docker image inspect
  ghcr.io/kuhlman-labs/fishhawkd:dev-local --format '{{.Id}}'` (the image
  CONFIG digest, which survives `docker save` → `ctr images import` unchanged)
  must equal the newest fishhawkd pod's `status.containerStatuses[0].imageID`.
  The kubelet reports that value as `docker://sha256:…` /
  `containerd://sha256:…` (compared directly) or as `<repo>@sha256:<manifest
  digest>`, which is resolved to its config digest via `ctr -n k8s.io content
  get` in the retained node-debug pod (isolated provisioner) or `docker image
  inspect <repo>@<digest>` against the shared host daemon. An OCI index costs
  exactly one extra hop.
- **Secondary — `git_sha`.** `/healthz` must report the SHA stamped into the
  build. Two dirty builds at the same commit carry the SAME `-dirty` SHA, so
  this check alone cannot tell a fresh build from the previous one — which is
  exactly why the image ID is the primary check, not a nicety.

Every fail-closed branch names what was missing (`identity: image id
mismatch`, `identity: /healthz carries no git_sha`, `identity: build sha
unknown`, `identity: pod reports no imageID`, …) and the single escape hatch:
`FISHHAWK_K8S_SKIP_IDENTITY=1` skips the gate (and downgrades the
unknown-provisioner refusal) with a loud stderr warning that the deployed image
was NOT verified. Only the exact value `1` disables it; a blank
`FISHHAWK_K8S_SKIP_IDENTITY=` in `.env` does not.

By hand, the same three reads:

```sh
curl -fsS http://localhost:8080/healthz | jq -r .git_sha
docker image inspect ghcr.io/kuhlman-labs/fishhawkd:dev-local --format '{{.Id}}'
kubectl get pod -l app.kubernetes.io/component=server \
  --sort-by=.metadata.creationTimestamp \
  -o jsonpath='{.items[-1:].status.containerStatuses[0].imageID}'
```

The first must equal `git rev-parse --short HEAD` (with `-dirty` on a dirty
tree); the second must equal the third once any `<repo>@sha256:` repo digest is
resolved to its config digest.

### Overriding the forwarded host ports

Three environment variables move the **host** side of each forward; the
in-cluster side is chart-owned and never changes. Each can be exported in the
shell or set in `.env` (the command sources it, like `scripts/dev up` does):

| Variable | Default | Forward |
|---|---|---|
| `FISHHAWK_K8S_PF_PORT` | `8080` | fishhawkd (`svc/fishhawk`) |
| `FISHHAWK_K8S_JAEGER_UI_PORT` | `16686` | Jaeger UI |
| `FISHHAWK_K8S_JAEGER_OTLP_PORT` | `4318` | Jaeger OTLP HTTP |

An **empty** value means "use the default", exactly like every other
`${VAR:-default}` knob in `scripts/dev` — so a commented-out or blank `.env`
entry is inert. A non-numeric, zero, negative or `> 65535` value is rejected
with a diagnostic naming the variable, the value and the accepted range
(`1..65535`) before `kubectl` is ever invoked.

Setting `FISHHAWK_K8S_JAEGER_UI_PORT` and `FISHHAWK_K8S_JAEGER_OTLP_PORT` to the
**same** value is rejected up front, even when that port is free: one host port
cannot carry both mappings, so `kubectl` would bind the UI forward and fail the
OTLP one. The bring-up names both variables and the shared value, skips the
Jaeger forward, and advertises no Jaeger endpoint.

The two forwards have **different collision policies, by design**: a squatted
fishhawkd port is fatal (fishhawkd is unreachable without it), while an
unusable Jaeger port — squatted, or equal to the other Jaeger port — prints a
warning, skips the Jaeger forward, and leaves the command exit code 0. Tracing
is optional and fishhawkd is already healthy by then.

**Caveat — OAuth callback.** `values-local.yaml` pins
`config.oauthCallbackUrl` to `http://localhost:8080/v0/auth/github/callback`. A
non-default `FISHHAWK_K8S_PF_PORT` therefore needs a matching chart override for
GitHub sign-in to complete; the command prints the exact `--set` line to add.

#### Operator walk (executable)

```sh
# 1. Bring up on a non-default host port.
FISHHAWK_K8S_PF_PORT=18080 scripts/dev k8s

# 2. fishhawkd answers on the overridden port.
curl -sS http://localhost:18080/healthz

# 3. Tear the stack down FIRST. Step 1 left a live `kubectl port-forward` bound
#    to 18080; without this, nc below cannot acquire the port and step 5 would
#    identify that forward instead of the intended squatter — a collision the
#    walk did not stage.
scripts/dev k8s-down

# 4. Now hold the port with nc, and PROVE nc owns it before asserting anything.
#    lsof must print exactly one pid, and it must be nc's.
nc -l 127.0.0.1 18080 &
nc_pid=$!
sleep 1
lsof -nP -iTCP:18080 -sTCP:LISTEN
# expected: a single LISTEN row whose PID column equals $nc_pid and whose
# COMMAND column is `nc`. If it is empty, nc failed to bind (something else
# still holds 18080) and the collision assertion below would be meaningless —
# stop and clear the port first.

# 5. Re-run: the preflight must abort BEFORE the image build, naming the nc pid
#    and pointing at the override variable.
FISHHAWK_K8S_PF_PORT=18080 scripts/dev k8s
# expected: error: port 18080 already has a listener: pid <nc_pid> (nc) — run
# 'scripts/dev k8s-down', kill the pid, or set FISHHAWK_K8S_PF_PORT to a free
# port, then retry
# and NO `building image ...` line above it.

# 6. Release the port.
kill "$nc_pid"
```

The equal-port variant of the same walk exercises the Jaeger leg's divergent
policy — no squatter needed, because the collision is between the two overrides
themselves:

```sh
FISHHAWK_K8S_JAEGER_UI_PORT=26686 FISHHAWK_K8S_JAEGER_OTLP_PORT=26686 \
  scripts/dev k8s
# expected, AFTER fishhawkd is healthy (exit code 0, fishhawkd usable):
# warning: FISHHAWK_K8S_JAEGER_UI_PORT and FISHHAWK_K8S_JAEGER_OTLP_PORT both
# resolve to port 26686 — one host port cannot carry both forwards
#   set one of them to a different free port; skipping the jaeger forward
# fishhawkd is up and usable; tracing is unavailable this session
# and NO `jaeger UI at ...` line — a forward that was not opened is never
# advertised.
```

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
`http://localhost:8080` (or at `http://localhost:$FISHHAWK_K8S_PF_PORT` when
that override is set). To re-establish a forward later:

```sh
kubectl port-forward svc/fishhawk 8080:8080
# on an overridden host port:
kubectl port-forward svc/fishhawk "${FISHHAWK_K8S_PF_PORT:-8080}:8080"
```

Local uses port-forward (or a NodePort) rather than an Ingress;
`values-local.yaml` sets `ingress.enabled: false` so `config.externalUrl` /
`config.oauthCallbackUrl` are used verbatim.

`kubectl port-forward svc/fishhawk 8080:8080` and `kubectl logs deploy/fishhawk`
now resolve **deterministically** to the fishhawkd pod. The Service + Deployment
carry an `app.kubernetes.io/component: server` discriminator
([#2916](https://github.com/kuhlman-labs/fishhawk/issues/2916)), so the selector
no longer also matches the in-cluster postgres/rustfs/jaeger pods or the
migrate/rustfs-bucket hook Job pods — before this, a selector-resolving command
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
  is used verbatim. With the bring-up's forward alive on the default port, that
  is `http://localhost:8080`; a non-default `FISHHAWK_K8S_PF_PORT` needs the
  matching `--set` (see [the override
  caveat](#overriding-the-forwarded-host-ports)).

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

This section is the **local dev / dogfooding** tracing path — running Jaeger in
your Docker-Desktop cluster to inspect your own runs. For the vendor-neutral
capability itself — the single `OTEL_EXPORTER_OTLP_ENDPOINT` switch, export to
any OTLP-compatible backend, and the runner-kind reachability matrix — see the
public [Operating >
Tracing](https://kuhlman-labs.github.io/fishhawk/operating/tracing/) page.

`values-local.yaml` enables an in-cluster **Jaeger all-in-one** (`jaeger.enabled`)
— the k8s analog of the opt-in `otel` profile in `docker-compose.yml`, and the
local OTLP collector for the runner's per-run GenAI trace spans (the `stage`/`chat`
span shape is in [`docs/ARCHITECTURE.md`](../ARCHITECTURE.md) §10). It is
**DEV / DOGFOODING ONLY**: an ephemeral, unauthenticated collector with in-memory
span storage (no PVC). `fishhawk.validateSecrets` fails the render outside
`profile: local`, so it can never reach a prod cluster.

While the bring-up's Jaeger forward is alive:

- **Jaeger UI** — `http://localhost:16686` (`FISHHAWK_K8S_JAEGER_UI_PORT`)
- **OTLP HTTP receiver** — `http://localhost:4318` (the runner's `otlptracehttp`
  target; `FISHHAWK_K8S_JAEGER_OTLP_PORT`)

Both host ports are overridable — see [Overriding the forwarded host
ports](#overriding-the-forwarded-host-ports). If either is already held, or if
the two overrides resolve to the same port (which no per-port check can catch,
since one free port passes a free-port test twice), the bring-up warns naming
the offending port and the override variable, skips the Jaeger forward, and
still exits 0 with fishhawkd usable; tracing is simply unavailable that session.
The endpoint lines are printed only on the branch that actually spawned the
forward, so the printed guidance can never disagree with the live forward.

**Execution-locality caveat.** fishhawkd does *not* emit these spans — the
`fishhawk-runner` does, and under the dogfood loop the runner is spawned by
`fishhawk-mcp` **on the operator's host** (inheriting that process's env), not
in-cluster. So the runner reaches the collector at the host's `localhost:4318`
through the forward, *not* via an in-cluster Service DNS name. To capture spans,
set `OTEL_EXPORTER_OTLP_ENDPOINT=http://localhost:4318` in the host environment
that spawns the runner (unset is a clean no-op; use the overridden port when
`FISHHAWK_K8S_JAEGER_OTLP_PORT` is set — the bring-up prints the exact line). The same caveat as the compose
path applies: a runner executing on a GitHub-hosted CI runner sees its *own*
loopback, not yours — end-to-end local viewing requires the runner to run on this
host (the `runner_kind=local` flow).

To re-establish the Jaeger forward later:

```sh
kubectl port-forward svc/fishhawk-jaeger \
  "${FISHHAWK_K8S_JAEGER_UI_PORT:-16686}:16686" \
  "${FISHHAWK_K8S_JAEGER_OTLP_PORT:-4318}:4318"
```

## Tear down

```sh
scripts/dev k8s-down   # or: make k8s-down
```

Kills the tracked port-forwards (fishhawkd pid in `.fishhawk/k8s-pf.pid`, Jaeger
pid in `.fishhawk/k8s-jaeger-pf.pid`) and runs `helm uninstall fishhawk`. All
steps are idempotent, so a double teardown is a no-op.

## Reviewer providers: `anthropic` only in-cluster

The runtime stage of `backend/Dockerfile` is
`gcr.io/distroless/static-debian12:nonroot` (see the TLS section below,
which relies on the same fact). It ships no shell, no package manager,
and no agent CLI — so the two SUBPROCESS reviewer providers a workflow
spec can declare, `claudecode` (spawns `claude`) and `codex` (spawns
`codex`), have nothing to spawn in this pod and nothing can be installed
into it.

Use `provider: anthropic` for every reviewer, backed by
`anthropicApiKey` → `FISHHAWKD_ANTHROPIC_API_KEY`. The shipped workflow
presets already do. Setting `FISHHAWKD_ENABLE_LOCAL_CLAUDE_REVIEWER` or
`FISHHAWKD_ENABLE_CODEX_REVIEWER` in-cluster is not a remedy: those flags
are capability gates, not installers, and since #3583 fishhawkd refuses
such a reviewer at resolution when the binary is absent from PATH.

Full table, the enabling-flag semantics and the host-deployment swap:
`deploy/helm/fishhawk/README.md` § "Reviewer providers".

## When a reviewer reports `not found on PATH`

The spec declares `provider: claudecode` or `provider: codex`, the
matching `FISHHAWKD_ENABLE_*` flag is set, and the CLI is not on the
fishhawkd process's PATH — on a chart install it never is. `fishhawk
doctor` reports the `reviewers` rung `available: false` with that reason,
and a run records a `reviewer_capability_unavailable` audit entry.

Remedy: change the reviewer to `provider: anthropic` (and set
`anthropicApiKey`), per the section above. There is no in-cluster fix:
the distroless image has no CLI to install.

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

## When the build fails with `x509: certificate signed by unknown authority`

This covers a host behind a TLS-inspecting corporate egress proxy, where
`docker build -f backend/Dockerfile` (step 2 of `## Bring up`, above) fails
before the image is even built. The certificate that fixes it is
site-specific and is deliberately NOT vendored into this repo or into
`backend/Dockerfile`.

### Symptom

The builder stage's dependency-download layer fails:

```
 > [builder 5/9] RUN cd backend && go mod download:
------
failed to solve: process "/bin/sh -c cd backend && go mod download" did not complete successfully: exit code: 1
go: github.com/...: reading github.com/...: tls: failed to verify certificate: x509: certificate signed by unknown authority
```

This looks like a network or Go module-proxy outage, but it is not. Registry
pulls are UNAFFECTED — the Docker daemon reads the HOST's trust store to pull
base images, so `FROM golang:${GO_VERSION}-bookworm` and
`FROM gcr.io/distroless/static-debian12:nonroot` succeed normally. Only
in-container egress (the `RUN cd backend && go mod download` layer talking to
the Go module proxy over TLS) breaks, because that TLS handshake is verified
against the trust store baked into the `golang:${GO_VERSION}-bookworm`
builder image, not the host's.

### Diagnostic: compare the issuer inside vs. outside the container

The decisive tell is the certificate issuer observed FROM INSIDE THE
CONTAINER, not a host/container comparison in the abstract — the host-side
check below is corroborating only. Compare:

```sh
# Outside the container (on the host):
echo | openssl s_client -connect storage.googleapis.com:443 2>/dev/null | openssl x509 -noout -issuer
```

```sh
# Inside the container — use the builder image, which already ships openssl
# (verified: `docker run --rm golang:1.25-bookworm sh -c 'command -v openssl'`
# prints `/usr/bin/openssl`). Do NOT reach for alpine + `apk add openssl` here:
# the package download itself goes through the same untrusted egress path, so
# its failure would be mistaken for evidence rather than recognized as the
# same failure mode compounding.
docker run --rm golang:1.25-bookworm sh -c \
  "echo | openssl s_client -connect storage.googleapis.com:443 2>/dev/null | openssl x509 -noout -issuer"
```

Decision rule:

- **Container shows a corporate/non-public CA, host shows the public
  issuer:** interception that only the container's egress path sees — this is
  the failure mode this section documents.
- **Both host and container show the same corporate CA:** still interception.
  The host succeeds anyway because it already trusts that CA through its own
  system trust store — that's why the corroborating host-side check below
  passes even though the host is *also* behind the proxy.
- **Container shows the public issuer:** not this failure mode; look
  elsewhere (a genuine module-proxy outage, DNS, etc.).

What differs between host and container is TRUST, not the certificate
presented — the proxy presents the same interception certificate to both; the
host's OS trust store is configured to accept it (via IT-managed MDM/config)
and the container's is not.

Corroborating check: `cd backend && go mod download` run directly on the host
(outside any container) succeeds, because it uses the host's trust store.

### Build-side fix: inject the corporate CA via a BuildKit named context

Add two lines to `backend/Dockerfile`'s `builder` stage, placed BEFORE the
`RUN cd backend && go mod download` line — a copy placed after it does not
help the layer that fails:

```dockerfile
COPY --from=cacontext corp-root-ca.crt /usr/local/share/ca-certificates/corp-root-ca.crt
RUN update-ca-certificates
```

Feed the context at build time:

```sh
docker build --build-context cacontext=/path/to/ca-dir \
  --build-arg GIT_SHA=<sha> \
  -t ghcr.io/kuhlman-labs/fishhawkd:dev-local \
  -f backend/Dockerfile .
```

Use a named context (`--build-context`), not a path inside the repo, because
it keeps the certificate out of the REPOSITORY and out of the DEFAULT build
context (`.`) — it is never committed and never accidentally picked up by an
unrelated `COPY`. It is NOT invisible to the build, though: the `COPY
--from=cacontext` line places it in a builder-stage image layer, and (per the
runtime half below) the runtime `COPY` places the augmented CA bundle in the
final image. **The resulting image therefore trusts the corporate CA and must
not be pushed to a shared registry.**

`--build-context` requires the BuildKit builder (the default in current
Docker Desktop). Check availability with:

```sh
docker build --help | grep -- --build-context
```

### Runtime half — easy to miss

The build-side fix above only fixes `go mod download` in the `builder` stage.
The runtime stage is `gcr.io/distroless/static-debian12:nonroot`, which ships
no shell and no package manager, so `update-ca-certificates` CANNOT run
there. If fishhawkd itself makes outbound TLS calls (GitHub, Anthropic)
through the same intercepting proxy at runtime, carry the builder's augmented
bundle forward into the runtime stage:

```dockerfile
COPY --from=builder /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
```

This OVERWRITES the bundle distroless already ships at that path — it is a
replacement, not an addition, but the replacement carries the upstream public
roots forward along with the interception CA (`update-ca-certificates` merges
rather than replaces), so the runtime image still trusts everything it did
before, plus the corporate CA. The failure this prevents shows up at a
completely different surface than the build-side one — a runtime outbound-call
TLS failure long after a green build — which is why fixing only the build
half leaves half the problem live.

### Residual: `scripts/dev k8s` cannot carry the certificate

`scripts/dev k8s` builds the image with exactly (`cmd_k8s_up` in `scripts/dev`;
`_k8s_image_ref` resolves to `ghcr.io/kuhlman-labs/fishhawkd:dev-local`, per
`## Bring up` step 2 above):

```sh
docker build --build-arg GIT_SHA="$build_sha" -t "$(_k8s_image_ref)" -f backend/Dockerfile .
```

It passes no `--build-context` and exposes no hook for extra build
arguments, so the one-command `## Bring up` path CANNOT carry the
certificate through on a host behind a TLS-inspecting proxy. On such a host:

1. Run the `docker build --build-context cacontext=...` command above by
   hand (with the local, uncommitted Dockerfile edit from the build-side fix
   section), or make the equivalent local, uncommitted edit to `scripts/dev`.
2. On the kind-based provisioner (`node/desktop-control-plane`), load the
   image into the node by hand — the same sequence step 3 runs:

   ```sh
   docker save ghcr.io/kuhlman-labs/fishhawkd:dev-local -o /tmp/fishhawk-image.tar
   kubectl debug node/desktop-control-plane --profile=sysadmin --image=debian:12-slim -- sleep 3600
   # note the printed node-debugger-<node>-<id> pod name, then:
   kubectl wait --for=condition=Ready pod/<pod> --timeout=120s
   kubectl cp /tmp/fishhawk-image.tar <pod>:/tmp/fishhawk-image.tar
   kubectl exec <pod> -- /host/usr/local/bin/ctr --address /host/run/containerd/containerd.sock -n k8s.io images import /tmp/fishhawk-image.tar
   kubectl delete pod <pod> --wait=false
   ```

3. Then run the `helm upgrade --install` command from `## Bring up` step 4
   directly, followed by `kubectl rollout restart deployment/fishhawk` and
   `kubectl rollout status deployment/fishhawk --timeout=120s`.
4. Run the by-hand identity check from [Image
   identity](#image-identity-what-a-green-bring-up-now-proves) — the script's
   gate did not run, so nothing else has proven the pod runs your build.

Both the Dockerfile CA-injection lines and the certificate file are a LOCAL,
UNCOMMITTED edit — the certificate is site-specific and must not be vendored.
An optional empty-by-default named context wired into the committed
Dockerfile is a possible follow-up if this recurs, not part of this change.

## When the bring-up fails with `identity: image id mismatch` / `stale fishhawkd image`

The pod is running an image whose config digest differs from the one this run
built. On the kind-based provisioner that means the load in step 3 did not
re-point the `dev-local` tag on the node, or the pod sampled was not the
restarted one. Remedy: `kubectl get pods -l app.kubernetes.io/component=server`
— if an old pod is still `Terminating`, wait and re-run `scripts/dev k8s`;
otherwise `kubectl delete deployment fishhawk` and re-run so the kubelet has no
prior `dev-local` to reuse. Do NOT reach for `FISHHAWK_K8S_SKIP_IDENTITY=1` as
a fix — it only silences the gate.

## When the bring-up fails with `identity: /healthz carries no git_sha`

The serving fishhawkd predates GitSHA stamping
([#1007](https://github.com/kuhlman-labs/fishhawk/issues/1007)) or was built
outside `scripts/dev` without `--build-arg GIT_SHA`. Rebuild through
`scripts/dev k8s`; if you built by hand, pass `--build-arg
GIT_SHA="$(git rev-parse --short HEAD)"`.

## When the bring-up fails with `identity: build sha unknown`

`git rev-parse HEAD` failed in the checkout `scripts/dev` runs from (not a git
work tree?), so the build was stamped `unknown` and cannot be corroborated. Run
from a git checkout; `FISHHAWK_K8S_SKIP_IDENTITY=1` brings up the unverified
image deliberately.

## When the bring-up fails with `cannot tell whether this cluster shares the host image store`

`kubectl get nodes -o name` printed neither `node/docker-desktop` nor a
kind-style `*-control-plane` / `*-worker` node (or nothing at all — is
`kubectl` pointed at the Docker Desktop context?). Fix the context (`kubectl
config use-context docker-desktop`), or set `FISHHAWK_K8S_SKIP_IDENTITY=1` to
proceed with no image load and no identity check.

## Leftover `node-debugger-*` pods

The loader keeps its node-debug pod alive until the bring-up exits (the
identity resolver reads the node's content store through it) and deletes it
from an EXIT trap on every path, including failures. A bring-up killed by a
signal (Ctrl-C mid-`kubectl cp`) can leave one behind:

```sh
kubectl get pods | grep node-debugger-
kubectl delete pod node-debugger-<node>-<id> --wait=false
```

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
| Postgres / RustFS | in-cluster (`postgres.enabled`, `rustfs.enabled`) | external DB / S3 |
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
chart-managed Secret, the default in-cluster DB/RustFS credentials, and the
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
