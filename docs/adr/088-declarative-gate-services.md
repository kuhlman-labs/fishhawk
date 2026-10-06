---
id: ADR-088
title: "Declarative gate services: a closed, Fishhawk-owned `services:` schema under workflow-v2 `gate_container`, joined to the gate by a shared --network=none namespace (not raw compose, not a bridge network)"
status: accepted
date: 2026-10-06
issue: https://github.com/kuhlman-labs/fishhawk/issues/4042
supersedes: []
superseded_by: []
applies_to: []
---

# ADR-088: Declarative gate services: a closed, Fishhawk-owned `services:` schema under workflow-v2 `gate_container`, joined to the gate by a shared --network=none namespace (not raw compose, not a bridge network)

Epic: E51 (#2133). Builds on ADR-063, #2137, #2136 and #4040.

## Context

- **Today's isolation.** The container gate path (ADR-063) runs each gate exec in one `--network=none` container (`runner/internal/gateiso/container.go`).
- **Today's service.** Daemon-dependent gates get exactly one runner-provisioned service, Postgres (#2137, `FISHHAWK_GATE_SERVICES=postgres`). It is `--network=none` and shares only its unix socket, read-only, into the gate. The Postgres image is operator env.
- **The goal.** Generalize Fishhawk to other projects and languages, whose tests need Redis, Kafka, MySQL and similar services.

The Captain asked for options. A raw docker-compose file was considered; this ADR settles the shape. Consulted 2026-10-06: an independent Fable review (code-grounded) and the backlog-grooming session. Both agree on rejecting compose and on a closed schema.

## Options

1. **Pass a raw docker-compose file.** REJECTED. It is repo-committed and agent-editable, so the code under test chooses its own isolation: `privileged`, `network_mode: host`, docker.sock mounts, `cap_add`, `pid: host`, `extends`/`include`, env interpolation, published ports. `build:` would also bypass the committed-tree build and the #2136 static Dockerfile guard. It would introduce a second, unpinned executor in place of argv from validated builders, and docker-compose and podman-compose diverge.
2. **A closed `services:` schema plus a per-exec `--internal` bridge network.** REJECTED (Fable review). It reverses ADR-063's `--network=none` invariant. `--internal` is daemon policy, not a kernel no-interface property, and it opens holes `--network=none` never had:
   - the embedded DNS at 127.0.0.11 may forward external lookups (a DNS exfil channel; version-dependent);
   - the host's bridge address in the internal subnet may accept INPUT;
   - Docker Desktop resolves `host.docker.internal` / `host-gateway`;
   - IPv6 depends on daemon config.
3. **A closed `services:` schema plus a SHARED `--network=none` namespace.** RECOMMENDED.
   - **Network.** The first service runs `--network=none`; the other services and the gate run `--network=container:<first>`. The joined namespace IS a none namespace: loopback only, with no bridge, no embedded DNS, no gateway and no IPv6 route beyond `::1`. The gate reaches services on `127.0.0.1:<port>`, and services must use distinct ports.
   - **Cleanup.** No network object is created, so none can leak after a crash. Teardown ordering already exists, since services are removed after the gate.
   - **Code change.** `BuildArgv` gains one validated field (`NetworkContainer`, matched against the service-name pattern) rendering `--network=container:<name>`. Prepare and passwd helpers stay `none`.
   - **Kubernetes.** This maps one-to-one onto a Kubernetes pod (shared netns, localhost), so #2316 can consume the same schema.

## Recommendation (option 3)

**Schema.** Fields are closed and intent-named, with no docker-isms in the names:
```yaml
gate_container:
  dockerfile: deploy/gate-image/Dockerfile
  context: deploy/gate-image
  services:
    - name: db                      # ^[a-z][a-z0-9-]{0,30}$
      image: postgres:16-alpine@sha256:...
      transport: socket             # socket (read-only unix socket, #2137 posture) | loopback (shared netns)
      user: "70:70"                 # REQUIRED unless the image declares a non-root USER; implicit root refused
      command: ["postgres", "-c", "fsync=off"]   # argv only, no shell; first token not flag-shaped
      env: { POSTGRES_DB: test }    # keys ^[A-Za-z_][A-Za-z0-9_]*$; literal values; secrets runner-generated
      tmpfs: ["/var/lib/postgresql/data"]        # size-bounded by the runner
      memory: 512m                  # optional, capped by the runner ceiling
      ready: { log: "ready to accept connections" }   # or { command: [...] }
```

**Invariants.**
1. **The spec is read from the BASE ref, enforced by the product.** The runner reads `gate_container` and `services:` from the run's pinned workflow SHA, never from the working tree, regardless of any repo's forbidden_paths. A run changing `services:` takes effect only after it merges.
2. **What cannot be expressed.** privileged, host network, bind mounts, devices, published ports, `cap_add`, sysctls and ulimits. Every service gets `--cap-drop=ALL`, `no-new-privileges`, and `--memory` / `--cpus` / `--pids-limit` under a runner ceiling (the #3663 starved-host class).
3. **Discovery is runner-composed.** The gate receives `FISHHAWK_SERVICE_<NAME>_URL` / `_HOST` / `_PORT` plus generated credentials, applied LAST (the `WithServiceEnv` precedent). The spec can never set gate env.
4. **Image policy as for the gate image (#2136), non-optional.** Under local, a tag-only reference is allowed with a warning. Under hosted, a reference must be digest-pinned and on the allowlist, or it is refused. Every service digest goes into `gate_isolation` evidence, with the wire golden extended (`backend/internal/wirecontract`).
5. **Lifetime is per gate exec.** The fix loop runs verify repeatedly, and a per-stage service would let iteration N plant state that changes N+1's verdict. An opt-in `lifetime: process` (bound to the owner like #3967, never cross-run) is deferred until Kafka or Elasticsearch startup cost proves it necessary. Service startup counts against the GATE budget, not the agent's (#4023).
6. **Failure is category C, with a named cause.** For example "image entrypoint needs root", "ulimit/sysctl unsupported", or "readiness timed out after Ns", tailing bounded service logs.
7. **Postgres keeps socket transport by default.** That is #2137's reviewed posture; `FISHHAWK_GATE_SERVICES=postgres` becomes its built-in entry. Socket transport also covers MySQL and Redis (`--unixsocket`); loopback covers Kafka and similar.

**Stated non-goals and limits.**
- **Testcontainers.** Many ecosystems (Java, .NET, Node, Go) start services via testcontainers and need a Docker socket. The container path will never provide one. This schema serves tests that accept an external URL, as `pgtest` does with `FISHHAWK_TEST_PG_URL`. The adoption guide must document the adapter pattern: env-URL-first, testcontainers fallback off-gate.
- **Services needing host sysctls or ulimits** (Elasticsearch's `vm.max_map_count` and memlock) are out of reach and must fail as named category C.
- **A compose → schema importer CLI** is out of scope; a possible later convenience.
- **podman.** `--network=container:` works rootless, but the joiner likely must share the target's user namespace (UNVERIFIED), so services need the same `--userns=keep-id`. The podman argv stays golden-pinned, not live-validated, as today.

## Ratification amendments (2026-10-06)

Reviewed before ratification by an independent Fable review (code-grounded) and the backlog-grooming session. Both said RATIFY-WITH-AMENDMENTS and converged on the following, which SUPERSEDE any conflicting text above.

1. **Inert namespace holder.** A holder container owns the shared `--network=none` namespace: the gate image with `--entrypoint '' … sleep <N>`, so there is no new image and no new trust source (the gate image already runs the passwd and cache-prepare helpers). Every loopback service AND the gate join `--network=container:<holder>`. The holder mirrors a k8s pod's pause container. A service crash or OOM never removes the namespace, and services are symmetric. `sleep` joins the gate-image contents check. Ordering: the holder runs, then services start and become ready, then the gate runs. Teardown: the gate, then services, then the holder.
2. **Socket-transport services keep their OWN `--network=none` namespace** (today's `RunArgv`, unchanged) and never join the shared one. Otherwise the Postgres entrypoint's `listen_addresses='*'` would expose `127.0.0.1:5432` and break #2137's socket-only posture.
3. **Schema additions and refusals:**
   - `port` is required for `transport: loopback` and forbidden for `socket`;
   - `socket_path` (absolute, clean) is required for `socket`, except the built-in Postgres entry;
   - its gate-side mount is `/fishhawk/svc/<name>:ro`;
   - duplicate names and duplicate ports are refused;
   - an explicit `user: "0:0"` is refused on every profile, the same as implicit root;
   - at most 8 services;
   - services never build: `dockerfile`/`context` belong to the gate image only;
   - `tmpfs` paths are absolute, clean and non-root, mounted with `uid`/`gid` = `user:` and a runner `size=`.
4. **Discovery and credentials:**
   - `FISHHAWK_SERVICE_<NAME>_HOST` / `_PORT` for every loopback service, always the literal `127.0.0.1`, never `localhost`.
   - `_URL` only for built-in kinds (Postgres), since the runner knows no scheme for an arbitrary image.
   - A typed, interpolation-free `env: { KEY: { generated: password } }`: the runner generates the value, injects it under KEY, and exposes it to the gate as `FISHHAWK_SERVICE_<NAME>_<KEY>`.
   - A generated value is by definition handed to the gate, so it is for single-tier credentials only (e.g. Redis `requirepass`). The Postgres built-in cannot be expressed this way: that would hand the gate the superuser password and undo #2137's CREATEDB-only role.
   - All discovery env is composed by the runner and applied LAST; the spec can never set gate env.
5. **Post-exec service liveness.** After the gate exits, the runner inspects every service: running, OOMKilled, exit code. A service that died during the exec reclassifies the gate result to category C `service_died`, naming the service and the cause, never category A against the agent.
6. **Invariant 1, narrowed.** "A run that changes `services:` cannot execute under it." Every stage reads the run's FROZEN workflow snapshot (`runs.workflow_spec`, via `resolveGateContainerConfig`) and never re-reads a checkout. The snapshot is the forge default branch on the fetch path, which hosted REQUIRES. On the local inline path it is the operator's spec at `start_run`, which is operator-trusted like `FISHHAWK_GATE_IMAGE`. `gate_isolation` evidence records the snapshot source (`forge_fetch` | `operator_inline`) and whether its `gate_container` matches the base ref. Local warns on a mismatch and does not refuse.
7. **Wire parity.** Backend `gateContainerConfig` and runner `upload.GateContainerConfig` gain `services`; extend the #2558 wirecontract pair. The `gate_isolation` evidence golden gains the service digests and the snapshot provenance.

**Delivery.** #4043 is pre-split to stay under the 45-file cap: #4043 is (a) schema, validate, wire pair and spec docs; a sibling is (b) runner holder/argv, discovery env, image policy, evidence, liveness and the live fixture.

## Decision

**ACCEPTED (2026-10-06, Captain): option 3 with the ratification amendments above.** It supersedes the ADR-063 #2137 addendum's "Postgres is the only provisioned daemon" mechanism; ADR-063's containment decision stands as this ADR's basis. Implementation: #4043 and its (b) sibling, after #4040.

## Consequences

- A new ADR, not an ADR-063 addendum. It adds a customer-facing spec surface and a new trust source (repo-declared service images). It SUPERSEDES the #2137 addendum's "Postgres is the only provisioned daemon" mechanism section, and leaves ADR-063's containment decision intact as its basis.
- Workflow-v2 gains an additive, optional field, so the spec-change checklist applies: canonical schema plus mirrors, docs/spec/workflow-v2.md, the generated site reference, and `fishhawk validate`.
- Sequencing: after #4040 (this repo onto the container path with Postgres). #2316 (Kubernetes gates) consumes the same schema as pod sidecars.
- Related generalization work (E83): #4022, #4023, #4025. #4027 (the shared test Postgres) is solved on the container path only.
- Not related to #3717 (compose for the product stack), which is a different trust domain.

### Intake signals (advisory)

Derived automatically when this item was filed. Everything below is a candidate for a human: nothing was closed, relabelled or transitioned.

**Possible duplicates**
- none found

**Parent epic suggestion**
- none

**Provisional score**
- 2.0, citing S4, U4
  - **S4** — Missing the structure the loop needs: absent Done-means, missing label namespace, unlinked parent epic, unrecorded `depends_on` edge, `boarded:false`. Objective and reversible — the `hygiene` action class.
    (no parent epic linked)
  - **U4** — Blocks nothing, and nothing blocks it. Schedule on value alone.
    (no depends_on edge declared)

Scanned 300 existing item(s); the scan window was truncated, so an older duplicate may be missed.

<!-- fishhawk-intake:v1 {"score":{"value":2,"citations":[{"rubric_id":"S4","quote":"Missing the structure the loop needs: absent Done-means, missing label namespace, unlinked parent epic, unrecorded `depends_on` edge, `boarded:false`. Objective and reversible — the `hygiene` action class.","note":"no parent epic linked"},{"rubric_id":"U4","quote":"Blocks nothing, and nothing blocks it. Schedule on value alone.","note":"no depends_on edge declared"}],"unscored":false},"degraded":false,"scanned_items":300,"window_truncated":true,"duration_ms":2065} -->
