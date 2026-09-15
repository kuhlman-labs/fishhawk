# `gateiso` — gate isolation for untrusted gate commands (ADR-063 / [#2127](https://github.com/kuhlman-labs/fishhawk/issues/2127), [E51.1 / #2134](https://github.com/kuhlman-labs/fishhawk/issues/2134))

The pure pieces of the runner's gate-isolation layer: safe container-runtime
detection over the EFFECTIVE endpoint, the mode × profile × runtime selection
policy, the container argv/env builder with its resolved-path socket-mount
guard, the throwaway-clone materializer, the Linux no-network sandbox, and the
per-exec build-cache posture. Nothing here executes a gate. The runner wires
every piece at its ONE gate-exec seam — `runner/cmd/fishhawk-runner/main.go::runBoundedGateArgv`
(`gateisolation.go` carries the runner-side glue) — so the verify gates,
`diff_coverage` and the auto-format absorb inherit isolation together and
there is still no second exec path.

## Environment variables (read by the runner at startup)

| Variable | Values | Default | Meaning |
|---|---|---|---|
| `FISHHAWK_GATE_ISOLATION` | `auto` \| `container` \| `clone-sandbox` \| `clone` | `auto` | the isolation mode (`ParseMode`; an unknown value is a startup config error naming the valid values) |
| `FISHHAWK_GATE_IMAGE` | an image reference | empty | the image the container path runs the gate in; empty means the container path is UNAVAILABLE, so a default runner never pays the per-exec cache cost below |
| `FISHHAWK_DEPLOYMENT_PROFILE` | `local` \| `self-hosted` \| `hosted` | `local` | the runner-DECLARED deployment profile (`ParseProfile`); `hosted` forbids every non-container path |

A config error (bad mode/profile, or `hosted` with an explicit `clone` /
`clone-sandbox` — `ProfileForbidsFallback`) fails the runner at startup with
`runner_failed reason=config` BEFORE any backend contact. A valid config logs
`gate_isolation_configured`; the first gate exec logs `gate_isolation_selected`
carrying the whole `Selection` JSON (path, mode, profile, image, the classified
runtime including its endpoint, the sandbox probe, and the reason) — the
struct #2135's evidence carries.

**Documented residual (test-pinned, approval condition 6 of #2134): the profile
is declared, not detected.** A hosted deployment that forgets
`FISHHAWK_DEPLOYMENT_PROFILE=hosted` gets the `local` default and therefore
fallback, not refusal. The mitigations are the two log lines above, the #2135
evidence and the #2138 deployment docs. Do not widen this into detection
silently.

## Selection (`select.go`)

`Select` is PURE over `Inputs{Mode, Profile, Image, Runtime, Sandbox}` — no
probe, no filesystem, no environment — and returns a `Selection` whose `Path`
is one of `container`, `clone-sandbox`, `clone`, `refused`. The container path
requires `Runtime.Safe && Image != ""`.

| Mode | container available | container unavailable, profile ≠ hosted | container unavailable, profile = hosted |
|---|---|---|---|
| `auto` | container | clone-sandbox when the probe says available, else clone | **refused** (reason names what is missing: the runtime verdict and/or the empty image) |
| `container` | container | **refused** | **refused** |
| `clone-sandbox` | clone-sandbox when available, else refused | same | **refused** (and a startup config error) |
| `clone` | clone | clone | **refused** (and a startup config error) |

`hosted` never executes an untrusted gate outside a container: the fallback
paths share the host filesystem and daemon sockets with the runner.

## Safe-runtime detection over the EFFECTIVE endpoint (`runtime.go`)

`DetectRuntime` classifies the endpoint the docker/podman CLI would actually
talk to — not merely whether a binary is on PATH — through an injectable
`Probes` surface (`DefaultProbes` binds the real host; every CLI probe is
bounded by a 15s timeout and a non-answer is UNSAFE, never awaited). Docker is
evaluated first, then podman; the first SAFE verdict wins, otherwise the first
evaluated verdict is returned so the reason names the primary runtime's
defect. `Runtime.Safe` is the single bit `Select` reads; `Reason`, `Endpoint`,
`Rootless`, `RunnerInContainer` and `SocketPath` are evidence recorded beside
it.

`ClassifyEndpoint` accepts ONLY a `unix://` endpoint (or a bare absolute path)
whose path, after `EvalSymlinks`, `Lstat`s as a socket AND dials. `tcp`, `ssh`,
`npipe`, `fd`, `http(s)`, an unparsable value, a missing or non-socket node, and
a refused connection are all `Local=false` with a named reason.

- **docker.** The context is `DOCKER_CONTEXT` else `docker context show`; its
  host is `docker context inspect --format '{{.Endpoints.docker.Host}}'`. The
  candidate set is `{DOCKER_HOST if set} ∪ {context host}` and EVERY candidate
  must classify Local — the detector deliberately does not encode the CLI's
  precedence between the two, so a remote context with no `DOCKER_HOST`, or a
  remote `DOCKER_HOST` over a local context, is UNSAFE either way. Docker
  Desktop's `desktop-linux` context reports a `unix://` socket under
  `~/.docker/run`, so a macOS dogfood host is SAFE (and still selects the
  fallback under `auto` until an image is configured).
- **podman.** `podman info` must report `Host.RemoteSocket.Exists=true` AND
  `Host.Security.Rootless=true`, and the connection podman would use
  (`CONTAINER_HOST`, else the `podman system connection list` row named by
  `CONTAINER_CONNECTION` or flagged `Default`) must classify Local. A podman
  machine (`ssh://`), a missing connection, or a rootful podman is UNSAFE.
  Consequence: native Linux podman without the socket service is UNSAFE —
  enable it with `systemctl --user enable --now podman.socket`.
- **docker-outside-of-docker is UNSAFE.** When the runner is itself inside a
  container (`/.dockerenv`, `/run/.containerenv`, or a container marker in
  `/proc/1/cgroup`) the daemon socket is the HOST's: the "isolated" gate
  container would be a sibling on the host daemon and the mounted checkout
  path would be interpreted in the host's filesystem namespace. Every remote
  endpoint is unsafe for the same reason — a bind-mount source is resolved on
  the daemon's host, not the runner's.

## The container path (`container.go`)

`ContainerSpec.BuildArgv(MountPolicy)` renders exactly:

```
<docker --host | podman --url> unix://<validated socket>
  run --rm --name fishhawk-gate-<hex> --network=none --cap-drop=ALL
  --security-opt=no-new-privileges --workdir /work (--user <uid>:<gid> | --userns=keep-id)
  -v <checkout>:/work -v <gocache>:/gocache -v <gomodcache>:/gomodcache -v <lintcache>:/lintcache
  -e K=V … --entrypoint '' <image> <argv…>
```

- **The validated endpoint is BOUND to every invocation.** `DetectRuntime`
  validates the endpoint once, at selection, but the CLI re-resolves its
  endpoint on EVERY invocation from mutable state (`docker context use`,
  `~/.docker/config.json`, `DOCKER_HOST`/`DOCKER_CONTEXT`,
  `CONTAINER_HOST`/`CONTAINER_CONNECTION`, `containers.conf`), so a context
  switch between two gates would otherwise send the next bind-mount request to
  a daemon nobody validated. `Runtime.EndpointArgs` renders the global
  endpoint flag (`docker --host unix://<socket>` / `podman --url
  unix://<socket>`, which overrides both the context store and the
  environment) between the binary and the subcommand of BOTH `BuildArgv` and
  `KillArgv`; `Runtime.BindEndpointEnv(base)` drops the four override
  variables from the CLI's env and re-pins `DOCKER_HOST` / `CONTAINER_HOST`
  to the same socket; a `Runtime` with no `SocketPath` renders NOTHING
  (`ErrContainerSpec`). Pinned by `TestBuildArgv_EndpointBindingPrecedesRun`,
  `TestBindEndpointEnv_DropsOverridesAndPins`, the runner-seam
  `TestRunGateInContainer_EndpointBoundAfterSelection` (selection recorded,
  THEN `DOCKER_HOST`/`DOCKER_CONTEXT` redirected, run + rm still bound) and
  the live fixture (l) `TestGateContainer_EndpointBoundAcrossContextSwitch`.

- **Four mounts, nothing else.** The checkout at `/work` (the working
  directory) and the three per-exec throwaway caches. No socket, no `/run`, no
  host cache is ever a mount source.
- **`--entrypoint ''` precedes the image and the full argv follows it**
  (approval condition 1 of #2134): an image whose `ENTRYPOINT` is `git`
  (`docker.io/alpine/git`, the pinned e2e image) cannot swallow or reinterpret
  the gate command. Pinned by the `BuildArgv` golden and by the live
  `TestGateContainer_EntrypointResetSmoke`.
- **Rootless podman gets `--userns=keep-id`; everything else `--user uid:gid`**
  so bind-mount writes are owned by the runner user on Linux
  (`TestGateContainer_LinuxOwnership`).
- **`KillArgv` = `<runtime> (--host|--url) unix://<socket> rm -f <name>`.**
  Killing the runtime CLI does not stop the container, so the runner runs this
  on a detached, bounded context whenever the exec returned -1
  (`TestGateContainer_TimeoutKillsContainer` goes red without it), under the
  same endpoint binding as the run — cleanup reaches the daemon that ran the
  container and no other.

**The resolved-path socket-mount guard.** `ForbidSocketMounts(policy, sources…)`
runs over every source BEFORE any `-v` token is emitted and returns the refusal
with NO argv. Each source must be absolute; it is `EvalSymlinks`-resolved so an
innocuously named symlink cannot dodge the checks; it is refused when the
resolved path lies under `/run` or `/var/run` (both sides resolved, compared by
PATH COMPONENT — macOS `/var/run` → `/private/var/run` is caught, `/runway` is
not), when a symlinked source resolves outside `MountPolicy.Permitted` (the
checkout, the visible-cache root and the lint-cache dir), when it is itself a
socket, when it equals or contains `MountPolicy.DaemonSocket` (the detected
`Runtime.SocketPath`), or when a `WalkDir` bounded to `MaxDepth` finds a socket
(a symlink entry is `Stat`'d for a socket target but never descended into).

**Documented residual (test-pinned, approval condition 6 of #2134): the walk is
bounded to depth 8** (`DefaultMaxMountDepth`). A socket deeper than that is NOT
detected — `TestForbidSocketMounts_DepthBound` pins the limit as a limit, not a
control. The cost is one sub-second walk of the checkout per container exec.
Do not widen or remove the bound silently.

**The container env rung.** `ContainerEnv(sanitized, extras)` projects the
runner's already-sanitized gate env (`sanitizedGateEnv`, ADR-029) into the
container: only `TZ`/`LANG`/`TERM`, `LC_*`, `CGO_*` and `GO*` survive, then
`HOME=/tmp`, `GOPATH=/tmp/gopath`, `GOCACHE=/gocache`, `GOMODCACHE=/gomodcache`,
`GOLANGCI_LINT_CACHE=/lintcache`, `GOPROXY=off`, `GOTOOLCHAIN=local`,
`GIT_CONFIG_GLOBAL/SYSTEM=/dev/null` are appended drop-then-append, then the
runner's `extraEnv` entries drop-then-append so a caller-supplied value wins.
The RUNNER's own inherited environment goes to the runtime CLI (it needs
`PATH` and its config dir) with the endpoint BOUND (`BindEndpointEnv`:
`DOCKER_HOST` / `DOCKER_CONTEXT` / `CONTAINER_HOST` / `CONTAINER_CONNECTION`
dropped, the validated socket re-pinned); the sanitized env crosses into the
container via `-e` only (`TestGateContainer_EnvAllowList`).

## The throwaway clone (`clone.go`)

`MaterializeClone(ctx, git, repoDir, headSHA, dest)` is the ONE materializer
both runner gate sites (the committed-tree verify gate and `diff_coverage`)
call through `materializeGateCheckout`, on EVERY path including the fallbacks:

1. `git clone --quiet --no-hardlinks --no-checkout <repoDir> <dest>` — a copy,
   never hardlinked objects, never a linked worktree of the primary (`repoDir`
   may itself be a linked worktree; git resolves its `.git` file);
2. best-effort `git fetch origin +refs/remotes/origin/*:refs/remotes/origin/*`
   so the clone carries the source's `origin/*` refs (a local clone maps
   `refs/heads/*` to `refs/remotes/origin/*` but does not copy the source's own
   `refs/remotes`, and the verify gate's diff-base ladder resolves
   `origin/main` first) — reported in `CloneReport`, not fatal;
3. `git rev-parse --verify <sha>^{commit}` must succeed (`ErrCommitNotFound`);
4. `git checkout --quiet --detach <sha>`;
5. `git config --unset remote.origin.url` so nothing in the clone can push or
   fetch back to the primary by its default remote.

WHY a clone and not the `git worktree add --detach` it replaced: a linked
worktree shares the primary's refs, objects and hooks, so a gate that ran
`git update-ref` or wrote a hook planted it in the operator's repository.
`TestGateClone_PlantedRefNeverReachesPrimary` proves both halves — the clone
keeps the plant, and a sibling `git worktree add --detach` checkout DOES leak it
to the primary. Because the clone has its OWN git common dir, `scripts/test
verify`'s lock would key there and contend with nothing (#2645 would reopen);
the runner therefore injects `FISHHAWK_VERIFY_LOCK_PATH` naming the PRIMARY's
`fishhawk-verify.lock` on every host path (`verifyLockPathEnv`), and leaves it
out on the container path, where the primary's filesystem is unreachable
(`TestGateContainer_PrimaryGitUnreachable` asserts the variable is absent
inside the container). Shell half: `scripts/README.md`.

## The Linux no-network sandbox (`sandbox.go`)

`ProbeSandbox` reports available only on Linux with `unshare` and `ip` on PATH
and a passing `unshare -rn -- sh -c 'ip link set lo up && true'` (unprivileged
user namespaces may be disabled — Ubuntu 24.04 AppArmor, Docker seccomp — so
availability is PROBED, never assumed). `WrapSandbox(argv)` returns
`unshare -rn -- sh -c 'ip link set lo up && exec "$0" "$@"' argv…` with no argv
interpolation. **macOS gap:** there is no unprivileged no-network sandbox on
macOS, so a macOS host reports unavailable with the ADR-063 gap reason and
`auto` selects `clone`; the container path (configure `FISHHAWK_GATE_IMAGE`)
is the way to get network isolation there. **Linux clone-not-sandbox note:**
the netns hides the HOST loopback too, so a gate that needs a daemon on the
host (this repo's testcontainers Postgres) fails under `clone-sandbox` exactly
as it does under the `--network=none` container; until
[#2137](https://github.com/kuhlman-labs/fishhawk/issues/2137) such hosts set
`FISHHAWK_GATE_ISOLATION=clone`.

## Build-cache posture (`cache.go`) — the INVARIANT

**The host `GOMODCACHE` is the trusted seed and is NEVER bind-mounted into a
container.** Every container exec gets a fresh EMPTY set of visible cache
directories (`NewVisibleCaches`: `fishhawk-gatecache-*` under the OS temp dir,
0700, empty `gocache` + `gomodcache`), populated HOST-side, mounted for that
one exec, and removed afterwards. Host seeding never opens a directory a
container has been given.

- `SeedModCache(ctx, run, checkout, hostModCache, dest, baseEnv, timeout)`
  (1) REFUSES with `ErrDestinationNotEmpty` when `dest.GoModCache` already has
  entries — the invariant's control, so a destination a container has touched
  is never re-seeded; (2) skips when the checkout has no `go.mod`/`go.work` or
  no `go` binary; (3) REFUSES with `ErrSeedCheckout`, BEFORE any go process
  runs, when the checkout's module metadata would let the host-side seed read
  or write outside the checkout — `go.mod` / `go.sum` / `go.work` /
  `go.work.sum` that is a symlink or not a regular file, a `go.work` `use`
  directory or a directory `replace` target (in `go.work` or in any workspace
  module's `go.mod`) resolving outside the `EvalSymlinks`-resolved checkout, or
  a metadata file that does not parse; (4) runs `go mod download all` under
  `baseEnv` — the runner passes its SANITIZED gate env (`sanitizedGateEnv`,
  ADR-029), never `os.Environ()`, so no runner credential reaches the go
  process — with `GOMODCACHE=dest`, a throwaway `GOCACHE`/`GOPATH` under the
  cache root, `GOFLAGS=-mod=mod -modcacherw`, `GOTOOLCHAIN=local` (an
  untrusted `go`/`toolchain` directive fails the seed by name instead of
  downloading and executing a toolchain), `GOWORK` bound to the checkout's own
  `go.work` or `off` (no parent-directory walk),
  `GIT_CONFIG_GLOBAL/SYSTEM=/dev/null`, `GIT_TERMINAL_PROMPT=0` and
  `GOPROXY=file://<hostModCache>/cache/download,<baseEnv GOPROXY | https://proxy.golang.org,direct>`
  — the host cache is a READ-ONLY proxy source, so every module already on the
  host is copied into the destination and only genuinely new modules reach the
  network (during seeding; the container itself runs `GOPROXY=off`). The
  `go env GOMODCACHE` probe (when `hostModCache` is empty) runs in the cache
  root, NOT in the checkout, under `GOTOOLCHAIN=local` + `GOWORK=off`.
  External-canary fixtures: `TestSeedModCache_RefusesMetadataReachingOutsideCheckout`
  (every hostile-metadata row refused with the exec seam never reached and the
  host canary untouched), `TestSeedModCache_LiveGoSumSymlinkNeverWrittenThrough`
  (the REAL go: without the guard it reads THROUGH the planted `go.sum` link),
  `TestSeedModCache_EnvIsBaseEnvPlusPinsNeverProcessEnv`, and the runner-seam
  `TestRunGateInContainer_SeedUnderSanitizedEnvRefusesHostileMetadata`.
  **Residual, stated:** `GOTOOLCHAIN=local` means a checkout whose `go.mod`
  requires a newer Go than the runner host has fails the seed with a named
  error rather than auto-downloading — accepted over a toolchain
  download-and-exec driven by untrusted metadata.
- `GOCACHE` has NO seed: a cold build cache per container exec.
- `VisibleCaches.Remove` is SYMLINK-SAFE (approval condition 2 of #2134): the
  chmod walk examines every entry with Lstat semantics, SKIPS (unlink-only) any
  entry whose mode is `ModeSymlink` — never chmod/chown/open through a link —
  chmods only regular files and directories reached without following a link,
  then `RemoveAll`s. `TestGateContainer_CacheSymlinkNeverReachesHost` plants a
  symlink to a 0444 host file from INSIDE the container and asserts the
  target's mode and contents are untouched, the link is gone with the visible
  dir, and the host module cache carries no planted entry.

**Cost, honestly:** module-cache repopulation plus a cold `GOCACHE` on EVERY
container exec (three per stage: scoped verify, full verify, diff_coverage).
The container path is opt-in via `FISHHAWK_GATE_IMAGE`, so no default runner
pays it. **Residuals:** the fallback paths keep the host caches (they are host
exec); there is no persistent shared cache; the per-exec repopulation and the
cold `GOCACHE` are accepted. The first container exec also pulls the image
through the daemon inside the gate timeout — pre-pull it.

## Refusal → category C

A `refused` selection makes `runBoundedGateArgv` return the text
`gate isolation refused: <reason> (mode=… profile=… image=… runtime=… safe=…)`
and `-1` WITHOUT executing. The verify gates recognise the LEADING signature
(`isGateIsolationRefusal`; a mid-stream literal in untrusted verify output does
not count) and classify it **category C**: `runVerifyCommittedTree` reports
`failed` (never the tolerant `skipped`), `runVerifyGateCommitted` wraps
`gitops.ErrVerifyInfraFailure` + `errGateIsolationRefused` and skips the
infra-flake absorb, and `runVerifyFixLoop` breaks with `verify_gate_refused`
and never re-invokes the fix agent. The signature matches none of
`isVerifyInfraFailure`'s classes, so a refusal is never absorbed as a flake.

## Bind-mount sources on Docker Desktop

Docker Desktop for Mac shares `/Users`, `/Volumes`, `/private`, `/tmp` and
`/var/folders` by default, so `os.MkdirTemp` sources mount; on a host with a
narrower file-sharing list the runtime's own error surfaces in the gate output
— set `TMPDIR` to a shared path.

## End-to-end fixtures (`runner/cmd/fishhawk-runner/gateisolation_e2e_test.go`)

Docker-gated fixtures cross gateiso → the runner wiring → a REAL runtime and
the pinned image `docker.io/alpine/git:v2.47.2` (`FISHHAWK_GATE_TEST_IMAGE`
overrides; manifest verified 2026-09-15). `requireGateImage` runs ONCE per
process: `DetectRuntime` over `DefaultProbes` (skip naming `Runtime.Reason`
when unsafe or absent), a pull (skip naming the runtime error), then an
in-image check that executes `sh -c 'echo ok'` past the git `ENTRYPOINT` and
resolves every binary the fixtures need (skip naming the missing one). Each
docker-gated fixture increments `dockerFixturesRan` at its END; `TestMain`'s
sentinel (main_test.go) fails the binary when an INDEPENDENT runtime probe — a
raw `docker version`/`podman version` plus a local-socket `Lstat`, never
`DetectRuntime`'s own verdict (approval condition 3) — sees a runtime but no
docker-gated fixture ran, so an all-skipped run on a docker host (including
docker-present-but-image-unpullable) is a loud failure, not a green.

| Fixture | Proves |
|---|---|
| (a) `TestGateContainer_PrimaryGitUnreachable` | the verify gate's clone has its `.git` INSIDE `/work`; the primary's absolute path is unreachable; a planted hook + ref never reach the primary; `FISHHAWK_VERIFY_LOCK_PATH` is not injected on the container path |
| (b) `TestGateContainer_NoNetwork` | external `wget` and `wget` to a LIVE host-loopback listener both fail; no `eth*` interface (Docker Desktop's VM kernel lists tunnel pseudo-devices, so the assertion is eth-absence, not "only lo") |
| (c) `TestGateContainer_HostFSUnreadable` | a marker outside the four mounts is unreadable; the host-exec control reads it |
| (d) `TestGateContainer_NoDaemonSocket` | no docker/podman socket node inside; no socket under the mounts; the RUNTIME-side argv carries no socket token; a checkout with a planted unix socket is refused BEFORE the real seam |
| (e) `TestGateContainer_EnvAllowList` | runner credentials set in the runner's env are absent inside; `GOPROXY=off` and the cache pins present; `extraEnv` preserved |
| (f) `TestGateContainer_TimeoutKillsContainer` | `sleep 60` at a scaled 2s timeout → -1 within the bound and `ps -a` no longer lists the container |
| (g) `TestGateContainer_LinuxOwnership` | `id -u` is the runner's uid; on Linux a written file is owned by `os.Getuid()` |
| (k) `TestGateContainer_CacheSymlinkNeverReachesHost` | the cache invariant + symlink-safe cleanup, above |
| (l) `TestGateContainer_EndpointBoundAcrossContextSwitch` | selection recorded against the real socket, THEN `DOCKER_HOST`/`CONTAINER_HOST` redirected to an unreachable tcp endpoint and `DOCKER_CONTEXT`/`CONTAINER_CONNECTION` to a nonexistent context → the gate still runs on the validated daemon; the argv opens with the binding; the CLI env pins the validated socket with the redirecting variables dropped |
| (h) `TestGateClone_PlantedRefNeverReachesPrimary` | clone path: plant never reaches the primary, lock path injected; the `git worktree add` sibling DOES leak the plant |
| (i) `TestGateCloneSandbox_NoNetwork` | Linux-only: a loopback connect fails under `clone-sandbox` through `runBoundedGateCommand` and succeeds under the host-exec control |
| (j) `TestGateHosted_RefusesEndToEnd` | hosted + auto with a REALLY detected runtime whose endpoint is pinned remote (`DOCKER_HOST=tcp://…` via the Getenv probe) → single-shot gate category C, fix loop category C, fix agent never invoked, verify command never ran |

## What the sibling issues own

- [#2135](https://github.com/kuhlman-labs/fishhawk/issues/2135) (E51.2) — recording
  the selection on GATE EVIDENCE: container vs fallback vs refused, the
  detected runtime and the selection inputs (today the `Selection` JSON only
  reaches the `gate_isolation_selected` log line).
- [#2136](https://github.com/kuhlman-labs/fishhawk/issues/2136) (E51.3) — the
  additive workflow-v1.x `diff_coverage` container-image field declaring the
  gate image for customer coverage commands (today the image is the runner-wide
  `FISHHAWK_GATE_IMAGE`).
- [#2137](https://github.com/kuhlman-labs/fishhawk/issues/2137) (E51.4) —
  daemon-dependent gate commands under the container path: this repo's own
  `scripts/test verify` needs the testcontainers Postgres, which a
  `--network=none` container (and the Linux sandbox's netns, above) cannot
  reach.
- [#2138](https://github.com/kuhlman-labs/fishhawk/issues/2138) (E51.5) — the
  operator-facing posture docs: the container-runtime requirement in the
  self-hosted distribution profile, runner setup docs, and the ARCHITECTURE
  containment rows.
