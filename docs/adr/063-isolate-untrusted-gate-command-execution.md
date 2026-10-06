---
id: ADR-063
title: "Isolate untrusted gate-command execution: container sandbox for .git-metadata, egress, and host-filesystem containment"
status: accepted
issue: https://github.com/kuhlman-labs/fishhawk/issues/2127
supersedes: []
superseded_by: []
applies_to: []
---

# ADR-063: Isolate untrusted gate-command execution: container sandbox for .git-metadata, egress, and host-filesystem containment

## Context

Every runner gate that executes untrusted repository-supplied code does so in a `git worktree add --detach` checkout, running the command via `sh -c` with `cmd.Dir = wt`. The command is contained for THREE things — credentials (the default-deny env allow-list, ADR-029 / #650 item 4), wall-clock (bounded timeout), and process tree (process-group kill) — and for NOTHING ELSE. That leaves three untrusted-execution vectors open, all rooted in the same gap: the command runs directly on the host as the same OS user as the runner.

1. **`.git` metadata sharing (the originating finding).** A LINKED worktree shares refs, config, and hooks with the primary repository. An untrusted command can write `.git/hooks/*`, rewrite `config` (`core.hooksPath`, url rewrites, credential helpers), or manipulate refs and locks — and the runner's SUBSEQUENT TRUSTED operations (commit, push) then execute that content while holding an installation token. This is a privilege escalation, not merely a containment inconvenience.

2. **Network egress.** The gate subprocess has unrestricted network access AND read access to the (private) repository contents it is measuring. That completes the untrusted-input + sensitive-data + exfiltration-egress triangle even with runner credentials stripped from the env. (Added as an amendment to the original metadata-only framing — see the issue comment thread.)

3. **Host-filesystem read.** Because the command runs on the host, it can read files OUTSIDE its checkout — `~/.ssh/*`, other repositories, ambient environment — regardless of worktree or clone isolation. Neither a linked worktree nor a throwaway clone closes this; a clone only gives the command its own `.git`.

AFFECTED CALL SITES (all pre-existing, verified in-tree at 7d008c6a):
- `runner/cmd/fishhawk-runner/main.go:4324` — committed-tree verify gate: `git worktree add --detach` then `sh -c verifyCmd`.
- `runner/cmd/fishhawk-runner/main.go:5008` — the second committed-tree gate, same pattern.
- `runner/cmd/fishhawk-runner/acceptancetree.go` — `git worktree add --detach` against the operator's dispatch checkout.
- The `diff_coverage` constraint (E46.3 / #1888), which reuses this posture for parity.

DISCOVERED: raised as a high-severity finding by gpt-5.6-sol in run `4d0bfef2-...` (PR for #1888); a test planted a lock in the PRIMARY repo's `.git/refs/heads` from the throwaway worktree, demonstrating reach into shared metadata. NOT introduced by #1888 — the verify-gate precedent was verified in-tree before merge; #1888 achieves parity with the established posture, and this ADR is about the posture itself.

Filed at `autonomy:low`: a containment/security posture change touching every gate execution path warrants human-led review per METHODOLOGY, and the options differ materially in cost.

## Options

1. **ACCEPT (status quo).** Document the boundary; contain credentials/wall-clock/process-tree only. Cheapest; leaves the hooks-to-trusted-push escalation, egress, and host-fs-read all open.

2. **HARDEN IN PLACE.** Keep linked worktrees; neutralize the metadata vectors (`core.hooksPath` → empty dir, `GIT_CONFIG_GLOBAL`/`GIT_CONFIG_SYSTEM` → /dev/null — the idiom `scripts/test` already uses — plus fail-closed on any post-command ref/config/hook delta). Blocklist-shaped: relies on enumerating every vector. Does not address egress or host-fs-read.

3. **FULL ISOLATION (throwaway clone per gate).** Each gate command gets its own clone with an independent `.git`, so metadata is unshared by construction. Not vector-enumeration for the metadata axis. Does NOT close egress or host-fs-read — the command still runs on the host with host network and host filesystem visibility.

4. **VERIFY-AFTER.** Snapshot primary refs/config/hooks before, compare after, fail on delta. Detection not prevention; racy while the command runs. Complements 2/3.

5. **CONTAINER ISOLATION (chosen).** Run the gate command in a `--network=none` container with ONLY the checkout-to-measure mounted (report dir bind-mounted writable for the result). Closes all three vectors at once: no primary `.git` is visible (metadata), `--network=none` is a uniform cross-platform egress cut, and nothing outside the mount is readable (host-fs-read). Resource limits come for free. Fit is good — the repo already depends on a container runtime (testcontainers Postgres #1174, CI docker-build, `scripts/dev k8s`). Two real caveats: the runner must be able to invoke a container runtime WITHOUT handing the untrusted container the host Docker socket (a container-escape path); and the customer `diff_coverage` case needs the customer's toolchain in the image.

## Recommendation

Adopt Option 5 (container isolation) as the primary mechanism, with Option 3 + a no-network sandbox as the documented fallback for runner topologies that cannot safely nest a container. Apply it at ONE shared helper covering all four call sites — the current per-gate duplication is why the diff-coverage gate inherited the posture silently. The hooks vector deserves closing first regardless, as it is a token-holding privilege escalation.

## Decision

**ACCEPTED — container isolation primary, clone+sandbox fallback.**

**Primary: container isolation.** The shared gate-exec helper (`runBoundedGateCommand`, the seam all four call sites route through) runs the untrusted command in a container:
- `--network=none` — uniform egress cut, identical on Linux and macOS (Docker Desktop). This RESOLVES the per-platform egress problem that the OS-level-sandbox fallback carries (see below): no `unshare -n` vs `sandbox-exec` divergence.
- Only the checkout-to-measure is mounted (read-only where the command permits; the WIP/commit dance that produces the committed tree stays runner-side, outside the container). A writable bind mount carries the coverage/verify report back out. No primary `.git`, no host home, no sibling repos are visible.
- Existing containment (bounded timeout, process-group semantics via the container lifecycle, default-deny env) is preserved; the container is an additional boundary, not a replacement.
- Fishhawk's own gates run in a known build image (extend `backend/Dockerfile`'s toolchain, or a dedicated gate image). The customer `diff_coverage` case requires a new **image** field alongside the already-declared coverage command — until that field ships, containerized `diff_coverage` is Fishhawk-image-only, and a customer command with no image declared uses the fallback.

**Fallback: clone-per-gate + no-network sandbox.** For runner topologies that cannot safely nest a container — the runner is itself containerized with no access to a nesting-safe runtime — fall back to a throwaway clone (metadata isolation by construction) plus an OS-level no-network sandbox. In this path egress control is OS-dependent and NOT uniform: clean on Linux (network namespace / `unshare -n`), a documented gap on the macOS local dogfood runner (no namespace equivalent; `sandbox-exec` is deprecated). The fallback also does not close host-fs-read. It is strictly weaker than the container path and exists only so a gate never silently reverts to the fully-unisolated status quo.

**Hard requirement on the container path — never the host Docker socket.** The gate container must be launched by a runtime the runner can invoke without granting the untrusted container access to the host daemon: a host-direct runtime, rootless Podman, or a nesting-safe runtime (e.g. sysbox). Mounting `/var/run/docker.sock` into (or making it reachable from) an untrusted-command container is a container-escape path and is worse than today's posture — it is explicitly forbidden. If no safe launch path exists on a given runner, that runner takes the fallback.

**Selection is runner-capability-driven, at the shared helper:** container path when a safe runtime is detected; fallback otherwise; and the selection plus which path ran is recorded on the gate evidence so an operator can tell an isolated run from a fallback run.

## Consequences

- A container runtime becomes a runner requirement wherever the container path is used; the fallback keeps gates functional where it is absent, at reduced isolation. This must be reflected in the self-hosted / single-tenant distribution profile (#1833) and the runner setup docs.
- The two coverage gates upgrade from **tamper-evident** to **isolated**: #2124 (patch-coverage, tamper-evident via a parent-memory digest) and #2129 (diff_coverage's residual measured-zero shapes) both cite "full prevention needs the process/filesystem isolation ADR-063 will decide." That prevention is this decision. Their waived residuals (profile forgery, same-user snapshot tamper) close once the gate command can no longer reach those paths.
- A new customer-facing `diff_coverage` schema field (the container image) is required for the customer case; scope it as an additive `workflow-v1.x` change per the schema-change checklist, following the same shape #1888 used.
- Any FUTURE gate that executes repository-supplied commands inherits isolation for free by routing through the shared helper — the copy-an-existing-call-site drift that let diff_coverage inherit the unisolated posture is closed.
- Related: ADR-029 / #650 (env-sanitization half of gate containment); this ADR is the filesystem/metadata/egress half that was never specified. E46 follow-ups #2124, #2125, #2129 depend on this for their full close.

**Implementation** is a follow-up (or set of follow-ups) filed against this ADR: the shared-helper container path + runtime detection + fallback, the gate-evidence path-recording, the `diff_coverage` image field, and the self-hosted-profile doc update. Human-led review per `autonomy:low`.

## Decision addendum (E51.4 / #2137): daemon-dependent gate commands

**Gap.** This repository's `scripts/test verify` runs the pgtest-backed backend suite, which starts a testcontainers Postgres through the host Docker daemon. The `--network=none` gate container has no daemon, no daemon socket (forbidden above) and no network, so the container path could not run this repository's own verify. Three options were weighed: (a) a nesting-safe runtime inside the gate, (b) a runner-provisioned service container whose unix socket alone is shared into the gate, (c) routing daemon-dependent gates to the fallback.

**Decided: option (b), with least privilege** (operator decision on the #2137 plan approval).
- **Rejected (a):** there is no nesting-safe runtime on Docker Desktop or the macOS dogfood runner; sysbox is a Linux-only runtime installed with root.
- **Rejected (c):** it would make the flagship verify gate permanently a fallback gate, which is the silent regression this ADR exists to prevent.

**Mechanism.** The runner switch is `FISHHAWK_GATE_SERVICES=postgres` (a comma list; `postgres` is the only member; an unknown member is a startup config error, `runner_failed reason=config`, before any backend contact). `FISHHAWK_GATE_POSTGRES_IMAGE` overrides the service image (default `postgres:16-alpine`; operators should pin it by digest). The switch only acts on the container path; elsewhere the runner logs `gate_services_ignored` and changes nothing. #2136's workflow-v2 `gate_container` block is the future spec mapping (a `gate_container.services` member); until then the runner variable is the switch. Per container exec:
1. `volume create` makes one fresh named volume, `fishhawk-gate-svc-<12 hex>`, labelled `org.fishhawk.gate-service=postgres`.
2. `run -d` starts the service as `--network=none --cap-drop=ALL --security-opt=no-new-privileges --user postgres`, with the same label and no published port. Its only mounts are that named volume at `/var/run/postgresql` and a `--tmpfs` at the image's declared PGDATA `VOLUME` (`/var/lib/postgresql/data`), so the daemon creates no anonymous data volume. No host path is mounted. initdb runs with `--auth-local=scram-sha-256 --auth-host=scram-sha-256`, so every connection, the socket included, needs a password. The superuser password is random per service.
3. **Readiness.** Each iteration reads the service logs FIRST and runs `pg_isready` only once the image's `PostgreSQL init process complete; ready for start up.` line has been seen, because the temporary init server already answers `pg_isready`. The service is ready only when `pg_isready` succeeds after that line. A parent cancellation ends the poll at once, as a readiness failure; a readiness failure carries the last `pg_isready` output and the tail of the service logs, which the teardown then removes.
4. **Bootstrap.** Inside the service container, as the superuser, the runner creates the gate role `fishhawk` (`LOGIN CREATEDB NOSUPERUSER NOCREATEROLE NOBYPASSRLS NOREPLICATION`) and a database `fishhawk` owned by it.
5. **Gate exec.** The socket volume is mounted READ-ONLY at `/pgsock` in the gate container. `FISHHAWK_TEST_PG_URL=postgres://fishhawk:fishhawk@/fishhawk?host=/pgsock&sslmode=disable` is applied LAST, so neither the sanitized env nor extras can redirect it. The superuser credential never enters the gate container: not in its env, files, mounts or argv.
6. **Teardown.** `rm -f -v <service>` then `volume rm -f <volume>` run on EVERY exit: provisioning failure, gate failure, timeout and a parent cancellation. They run on a detached, bounded context and are registered before `volume create` is attempted. A teardown failure is logged (`gate_service_cleanup_failed`) and never changes the gate's verdict.

Every runtime call (the passwd read, all six service steps, the gate run and its kill) is bound to the endpoint the selection validated.

**Bounds.** Each provisioning step has its own bound, and none of them counts against the gate's own timeout (`executor.verify.timeout`), just like the module-cache seed:
- `volume create`: 1m.
- `run -d`: 5m, which covers a cold pull of the service image.
- Readiness: 90s overall, polled every 250ms, with each `logs` / `pg_isready` probe bounded at 15s.
- Bootstrap: 1m.

ANY provisioning failure is `gateUnavailable`: category C, the gate argv never executes, and the fix agent is never invoked.

**On EVERY container exec, services or not:**
- the in-container env pin `FISHHAWK_GATE_CONTAINER=1`;
- a runner-generated `/etc/passwd`, mounted read-only, that maps the caller uid;
- the passwd-read helper that builds it.

The file is the gate image's `/etc/passwd` plus one caller entry, written to a fresh per-exec file. The helper is `run --rm --network=none --cap-drop=ALL --security-opt=no-new-privileges --user <uid>:<gid> --entrypoint '' <image> cat /etc/passwd`. Its output is the runtime CLI's combined stdout and stderr, so only well-formed passwd entries are kept: a cold pull's progress lines arrive beside the file with exit 0, and an output with no entry at all is a failed read. Only a successful read is cached, per image per process, so a transient failure is retried on a later exec. A read or write failure degrades to no passwd mount, with a `gate_passwd_unavailable` log line; it never refuses the gate.

**Test side.**
- `backend/internal/pgtest` uses `FISHHAWK_TEST_PG_URL` as its shared base without testcontainers. Every failure on that branch is fatal, never a skip.
- With `FISHHAWK_GATE_CONTAINER=1` and no URL, `pgtest` fails, naming `FISHHAWK_GATE_SERVICES`.
- `backend/internal/postgres`'s raw-database helper creates throwaway `fh_raw_<uuid>` databases on that server.
- The RustFS-backed `backend/internal/tracestore` S3 suite skips inside the gate container.

**Scoped exceptions to "only the checkout is mounted".** These are deliberate, and they are the only ones:
- (i) One per-exec NAMED volume holding only the Postgres unix socket, mounted read-only. It is never a host path and never the runtime daemon socket: `BuildArgv` refuses, with no argv, any service mount whose volume is not `^fishhawk-gate-svc-[0-9a-f]{12}$`, whose target is not absolute, clean and non-root, or that is not read-only.
- (ii) The runner-generated read-only `/etc/passwd`. It is a host file under the per-exec cache root and passes through the same resolved-path socket-mount guard as every other bind source.

The service container itself mounts no host path: one named volume plus a tmpfs, nothing else.

**Residuals, stated:**
- **A CREATEDB role on a network-less, per-exec Postgres.** The gate connects as a non-superuser that can create databases and own what it creates, and nothing else. It cannot run superuser-only SQL: `COPY … TO PROGRAM` and `CREATE ROLE` are refused, and the superuser cannot be reached over the socket without its password. The service it talks to has no network, no host mount, no published port and no capabilities, and is destroyed after the exec. The superuser password exists only in the service container's env and the host-side runtime CLI argv. Those are visible to same-user processes on the runner HOST, but not inside the gate container.
- **The containment fixtures skip once verify itself runs on the container path.** The docker-gated fixtures in `runner/cmd/fishhawk-runner/gateisolation_e2e_test.go` (the containment set (a)–(g), (k), (l) and the service fixtures (m), (n)) need a container runtime, and the gate container has none, so inside it they and the `TestMain` sentinel's eligibility skip. They keep running wherever the runner package's `go test` runs on a host with Docker: a host-side `scripts/test verify` / `scripts/test single -run TestGate`, on the operator's docker host and in CI.
- **The RustFS S3 suite is skipped on the container path.** No RustFS service is provisioned. CI and host-side verify still run it. Patch coverage measured INSIDE the gate container therefore counts `backend/internal/tracestore`'s S3 code as uncovered, so a change there can fail the in-loop 85% patch gate on the container path and pass it on the host. The aggregate CI gate is unaffected.
- **Postgres is the only provisioned daemon.** Any other daemon dependency is unsupported on the container path. Under the hosted profile (no fallback) it surfaces as a red tree, never as a silent skip. `directory/internal/store` still starts its own testcontainers Postgres outside `pgtest`, so it is a known in-container failure until it learns the external URL.
- **RAM-backed PGDATA.** The service's PGDATA tmpfs has no size option and the service container has no memory limit, so the databases the gate creates (it holds `CREATEDB`) live in host memory, bounded only by the runtime's tmpfs default. A hostile or simply large suite can pressure the runner host's memory. This is a host-availability risk only, since the service has no egress, and it matches the gate container's own posture: it sets no `--memory` or pids limit either. A size cap waits on the operator walk showing what the full suite needs.
- **Crash-orphaned services.** A runner SIGKILLed mid-exec runs no deferred teardown, and the service container and volume outlive it. Both carry the `org.fishhawk.gate-service` label; the label-filtered manual cleanup is in `runner/README.md`.
- **Cost.** Each container exec gains a Postgres start (seconds) plus one passwd read per image per process.
- **Not yet proven in-loop.** The full in-container `scripts/test verify` is validated by an operator walk, not by the implement gate. The opt-in fixture (n), `TestGateContainer_SelfHostPgtestSuite`, drives pgx and golang-migrate (the real `pgtest` external branch) over the mounted socket inside the built `fishhawk-gate` image. It must pass, with its result recorded, before an operator sets `FISHHAWK_GATE_IMAGE` with `FISHHAWK_GATE_SERVICES=postgres` for this repository.

**Rollback.** The feature is opt-in. With `FISHHAWK_GATE_SERVICES` unset the runner provisions nothing, and `pgtest` / `postgres` tests behave as before outside the gate container. The `FISHHAWK_GATE_CONTAINER=1` pin, the `/etc/passwd` mount and the passwd-read helper apply to EVERY container exec, so reverting them changes every container gate, not only service-bearing ones. Unsetting `FISHHAWK_GATE_IMAGE` returns a runner to the clone fallback with no code change.

## Addendum (E51.3 / #2136): per-project gate container

Kept separate from the "Decision addendum (E51.4 / #2137)" above; it records the shipped behaviour of #2136 and does not amend #2137's.

### Decision

**Replaces** the Decision's sentence "The customer `diff_coverage` case requires a new **image** field alongside the already-declared coverage command …" and Consequences bullet 3 ("A new customer-facing `diff_coverage` schema field …"). The image is declared ONCE for every gate kind, not as a `diff_coverage`-only field: a workflow-v2 `gate_container` block — `{image: <ref>}` OR `{dockerfile: <path>, context: <path>}`, exactly one source — at workflow level with a per-stage override. It is additive-optional in workflow-v2; v0/v1 are frozen and do not carry it. `services` is reserved for #2137 and not implemented.

- **Resolution and precedence.** The backend resolves the effective block by stage IDENTITY and serves it on the stage prompt with its source; the runner declares it before any gate. Precedence: stage > workflow > `FISHHAWK_GATE_IMAGE` > the clone-sandbox / clone fallback. The verify gates, `diff_coverage` and the auto-format absorb share the one exec seam, so all run in the declared image.
- **Image policy per profile.** A tag-only declared ref is allowed with a recorded warning in `local` / `self-hosted` and refused in `hosted`. `FISHHAWK_GATE_IMAGE_ALLOWLIST` (registry host, namespace prefix, repository or exact digest; bare names, tags and wildcards refused at startup) constrains every declared image and every build base; `hosted` refuses every declared image or build while it is empty. `FISHHAWK_GATE_IMAGE` stays operator-chosen and is not checked.
- **Pull.** Declared images are pulled explicitly with the runtime CLI's own credential store (pinned: only when absent; tag-only: every gate) and run by `name@<registry digest>`. A declared `image:` must be pullable from a registry; a local-only image fails category C with a reason naming the `dockerfile`/`context` and `FISHHAWK_GATE_IMAGE` remedies.
- **Builds: allowed but closed down** (operator decision on the #2136 plan approval). Allowed by default in `local` / `self-hosted`, denied in `hosted` (`FISHHAWK_GATE_BUILD` overrides). The build context is the COMMITTED tree at the gate's head SHA, read from git objects — never the working tree. The content digest is git's own (the context tree id plus the Dockerfile's mode and blob id plus its ignore file's blob id), so every gate kind and clone of one commit shares one cached image, and a mode-only change moves it. Before any build call a deny-by-default, case-insensitive static scan refuses every parser directive other than `check` (`# syntax=` selects a frontend that can ignore `--network=none`), a non-comment non-instruction first line (`// syntax=`, a JSON directive), any ADD source that is not a plain relative in-context path (`$`, `://`, `@`, `.git`, `:`, a host-shaped first component), `RUN --network` other than `none`, `RUN --security=insecure`, and — under `hosted` — `RUN --mount=type=cache`. With an allowlist configured every `FROM`, `COPY --from=<image>` and `RUN --mount from=<image>` base must pass it (and be digest-pinned in `hosted`). RUN steps build with `--network=none`.
- **Classification.** A policy, source or Dockerfile refusal is `gateRefused`; a pull, inspect, digest or build failure is `gateUnavailable`. Both are category C, neither reaches the fix agent, and neither falls back to `FISHHAWK_GATE_IMAGE`.
- **Unhonoured declarations.** A declaration the selected path cannot honour (no safe runtime under `auto`, or an explicit clone mode) runs the host fallback in `local` / `self-hosted` with a warning log line and a `declared_unhonored` evidence marker; `hosted` refuses, as it refuses every non-container path.
- **Evidence.** The `gate_isolation` evidence and its `gate_isolation_recorded` audit row gain `image_source`, the FINAL DECIDING verify gate's `image_digest` / `image_id` / build paths / `build_context_digest`, `distinct_images_count` when a stage ran more than one image, `policy_warning` and `declared_unhonored`.

### Consequences

- **Accepted limit — cache mounts outside `hosted`.** `RUN --mount=type=cache` persists on the daemon across builds and projects, so in `local` / `self-hosted` a build can read a cache another build wrote. Accepted for a single-tenant host; `hosted` refuses it.
- **Documented residuals.** RUN steps run as root inside the build sandbox; a base image's `ONBUILD` triggers are invisible to the static scan; the scan is a best-effort hedge that over-refuses on a builder divergence but is not a proof of equivalence with every builder; the agent-writable Dockerfile protects the host, not the verdict.
- **Cost.** A tag-only ref pulls on every gate; a build runs once per distinct content digest (`context: '.'` rebuilds on every commit). Build tags accumulate under label `fishhawk.gate-build=1`; pruning is documented, not automated.
- The `FISHHAWK_GATE_IMAGE` path is byte-identical (implicit pull inside the gate timeout, no digest on the evidence).
- Contract: `runner/internal/gateiso/README.md` § "Declared gate image"; spec: `docs/spec/workflow-v2.md` § "Gate container".
