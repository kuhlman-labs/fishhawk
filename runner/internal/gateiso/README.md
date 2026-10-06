# `gateiso` — gate isolation for untrusted gate commands (ADR-063 / [#2127](https://github.com/kuhlman-labs/fishhawk/issues/2127), [E51.1 / #2134](https://github.com/kuhlman-labs/fishhawk/issues/2134))

The pure pieces of the runner's gate-isolation layer: safe container-runtime
detection over the EFFECTIVE endpoint, the mode × profile × runtime selection
policy, the container argv/env builder with its resolved-path socket-mount
guard, the throwaway-clone materializer, the Linux no-network sandbox, the
per-exec build-cache posture, the per-exec gate services and caller
passwd entry (#2137), and the declared per-project gate image — its policy,
the static Dockerfile guard and the pull/build resolution argv (E51.3 / #2136,
§ "Declared gate image"). Nothing here executes a gate. The runner wires
every piece at its ONE gate-exec seam — `runner/cmd/fishhawk-runner/main.go::runBoundedGateArgv`
(`gateisolation.go` carries the runner-side glue) — so the verify gates,
`diff_coverage` and the auto-format absorb inherit isolation together and
there is still no second exec path.

## Environment variables (read by the runner at startup)

| Variable | Values | Default | Meaning |
|---|---|---|---|
| `FISHHAWK_GATE_ISOLATION` | `auto` \| `container` \| `clone-sandbox` \| `clone` | `auto` | the isolation mode (`ParseMode`; an unknown value is a startup config error naming the valid values) |
| `FISHHAWK_GATE_IMAGE` | an image reference | empty | the image the container path runs the gate in when the stage declares no `gate_container` (a declared one beats it, § "Declared gate image"); empty means the container path is UNAVAILABLE, so a default runner never pays the per-exec cache cost below. For THIS repository the recommended image is the in-repo, pin-checked `fishhawk-gate` (`deploy/gate-image/README.md`, E51.17 / #3966), digest-pinned. **For this repository, set it ONLY together with `FISHHAWK_GATE_SERVICES=postgres`, and only after the § "Gate services (#2137)" operator walk is green:** without the service, `auto` prefers the container path, where `pgtest` fails closed (`FISHHAWK_GATE_CONTAINER=1`, no database) and every verify is red |
| `FISHHAWK_GATE_SERVICES` | comma list; only member `postgres` | empty (none) | services the CONTAINER path provisions beside each gate exec (`ParseServices`; an unknown member is a startup config error naming the variable and the valid value; duplicates collapse). Ignored, with one `gate_services_ignored` log line, on every other path. Future spec mapping: #2136's workflow-v2 `gate_container.services` |
| `FISHHAWK_GATE_POSTGRES_IMAGE` | an image reference | `postgres:16-alpine` (`DefaultPostgresImage`) | the Postgres service image; pin it by DIGEST (`postgres@sha256:…`) — the image runs as a service beside every container gate. **Floor: PostgreSQL 16 or newer.** The gate role holds `CREATEROLE`, which only 16 bounds, so the bootstrap's version guard fails an older image (category C, the gate never runs; § "Gate services (#2137)", Least privilege) |
| `FISHHAWK_DEPLOYMENT_PROFILE` | `local` \| `self-hosted` \| `hosted` | `local` | the runner-DECLARED deployment profile (`ParseProfile`); `hosted` forbids every non-container path |
| `FISHHAWK_GATE_IMAGE_ALLOWLIST` | comma/whitespace list of registry hosts, namespace prefixes, repositories, exact digests | empty (no allowlist) | the operator image allowlist a DECLARED `gate_container` image, and every base of a declared build, must pass (`ParseAllowlist`; grammar in § "Declared gate image"). An invalid entry is a startup config error naming it. Never applied to `FISHHAWK_GATE_IMAGE` (operator-chosen) |
| `FISHHAWK_GATE_BUILD` | `allow` \| `deny` | empty = the profile default (`hosted` deny, `local`/`self-hosted` allow) | whether a declared `gate_container` `dockerfile`/`context` build may run (`ParseBuildPolicy`; any other value is a startup config error) |
| `FISHHAWK_GATE_CACHE` | `process` \| `off` | `process` | the container path's `GOCACHE` / golangci-lint cache posture (`ParseCacheMode`; any other value is a startup config error naming it and both valid values): `process` keeps them in ONE named volume per runner process, `off` keeps the pre-#3967 per-exec cold caches (also the measurement's cold arm). § "Persistent cache volume" |

A config error (bad mode/profile, an unknown `FISHHAWK_GATE_SERVICES` member,
an invalid `FISHHAWK_GATE_IMAGE_ALLOWLIST` entry, `FISHHAWK_GATE_BUILD` or
`FISHHAWK_GATE_CACHE` value, or `hosted` with an explicit `clone` / `clone-sandbox` —
`ProfileForbidsFallback`) fails the runner at startup with
`runner_failed reason=config` BEFORE any backend contact. A valid config logs
`gate_isolation_configured` (mode, profile, image, services,
postgres_image, allowlist_entries, build, cache — `cache` is the LAST field); the first gate exec logs `gate_isolation_selected`
carrying the whole `Selection` JSON (path, mode, profile, image, the classified
runtime including its endpoint, the sandbox probe, and the reason). The same
selection is recorded on the gate evidence once a gate reaches the exec seam
(#2135, see § "Evidence" below).

**Documented residual (test-pinned, approval condition 6 of #2134): the profile
is declared, not detected.** A hosted deployment that forgets
`FISHHAWK_DEPLOYMENT_PROFILE=hosted` gets the `local` default and therefore
fallback, not refusal. The mitigations are the two log lines above, the #2135
evidence and the deployment docs
([`docs/deploy/self-hosted.md`](../../../docs/deploy/self-hosted.md) §
"Runner gate isolation (ADR-063)" and
[`docs/deploy/hosted-regional.md`](../../../docs/deploy/hosted-regional.md) §
"Runner gate isolation under hosted", which tell every hosted runner host to
set it). Do not widen this into detection silently.

## Selection (`select.go`)

`Select` is PURE over `Inputs{Mode, Profile, Image, Runtime, Sandbox, Build,
ImageSource, PolicyRefusal, PolicyWarning}` — no probe, no filesystem, no
environment — and returns a `Selection` whose `Path` is one of `container`,
`clone-sandbox`, `clone`, `refused`. The container path requires
`Runtime.Safe && (Image != "" || Build)`. A non-empty `PolicyRefusal` (a
declared `gate_container` the image policy refused, § "Declared gate image")
refuses under EVERY mode and profile with `container_unavailable: not
attempted: gate_container policy refused`, and never substitutes the
`FISHHAWK_GATE_IMAGE` image.

| Mode | container available | container unavailable, profile ≠ hosted | container unavailable, profile = hosted |
|---|---|---|---|
| `auto` | container | clone-sandbox when the probe says available, else clone | **refused** (reason names what is missing: the runtime verdict and/or the empty image) |
| `container` | container | **refused** | **refused** |
| `clone-sandbox` | clone-sandbox when available, else refused | same | **refused** (and a startup config error) |
| `clone` | clone | clone | **refused** (and a startup config error) |

`hosted` never executes an untrusted gate outside a container: the fallback
paths share the host filesystem and daemon sockets with the runner.

**`ContainerUnavailable` and `Class` (#2135).** Every non-container outcome
sets `Selection.ContainerUnavailable` (`container_unavailable`, omitempty):
what the container path lacked (`containerMissing` — the runtime verdict
and/or the empty image) for `auto` and `container`, or `not attempted:
mode=<m> selects a fallback path` for an explicit `clone` / `clone-sandbox`
(and `not attempted: unknown isolation mode "<m>"`). It is empty on the
container path. `Path.Class()` maps a path to the coarse
`container | fallback | refused` vocabulary: `container` → container,
`clone-sandbox` / `clone` → fallback, `refused` → refused, anything else → "".
Pinned by `TestSelect_ContainerUnavailable` and `TestPathClass`.

**`DeclaredUnhonored` (E51.3 / #2136).** A DECLARED source (`stage` or
`workflow`) landing on `clone-sandbox` or `clone` sets
`declared_unhonored: gate_container declared (<source>) but not honoured:
<container_unavailable>; the gate ran on the host toolchain`, and the runner
logs `gate_container_unhonored`. An env-sourced fallback carries no marker.
Under `hosted` a declared image is never run on the host — every non-container
path is already refused. Pinned by `TestSelect_DeclaredUnhonoredMarker`.

## Evidence (#2135)

The runner (`runner/cmd/fishhawk-runner`) records the selection only when a
gate REACHES the exec seam (`runBoundedGateArgvDisposed`, refusal included) —
not inside `selection()`, which also has a non-exec caller (the committed-tree
gate's lock-path decision). At pack time it folds the recorded selection into
the `gate_evidence` trace event as a flat, pre-redacted `gate_isolation`
member (path, class, mode, profile, image, runtime kind/safe/reason/version/
rootless, runner-in-container, sandbox availability/reason, reason,
container_unavailable; the endpoint's raw value and socket path are NOT
carried). The member comes only from that record — a `gate_isolation`-kind
event in the stream is never folded. Absent when no gate reached the seam
(plan stages, the working-tree verify gate, a nil state), so those payloads
stay byte-identical. The wire shape is pinned cross-module by
`testdata/wire/gate_isolation_evidence.json` (one member per class, plus the
three `gate_container` members below) and the `gate_isolation_evidence`
`ModeExact` pair in `backend/internal/wirecontract`.

**Declared-image fields (E51.3 / #2136), all omitempty** so a stage with
neither a declared block nor `FISHHAWK_GATE_IMAGE` has a member byte-identical
to its #2135 form (an env-image stage's member gains `image_source: "env"`):
`image_source` (`stage` |
`workflow` | `env`), `image_digest` (the registry digest a pulled image ran
by), `image_id` (the runtime's local image id — opaque, recorded, never
compared: on a containerd image store it is an index digest, AGENTS.md trap
#3530), `build_dockerfile` / `build_context` / `build_context_digest` (an
in-repo build), `distinct_images_count`, `policy_warning` and
`declared_unhonored`. The image fields describe the **final deciding gate**:
the LAST verify gate (`runVerifyCommittedTree`, the gate whose outcome decides
the push or the failure) to reach the container path; a non-deciding gate
(`diff_coverage`, the auto-format absorb) is used only when no verify gate ran
there, and a deciding gate whose resolution FAILED leaves the image fields
empty rather than inheriting an earlier gate's image. `distinct_images_count`
is set only when the stage's gates ran in more than one distinct image
(`ResolvedImage.Identity`: registry digest, else build content digest, else
local id). Free text is redacted then bounded like every sibling field. The
shared golden's `container_declared`, `container_build` and
`fallback_declared_unhonored` members are produced by the real `Select` and
`EvaluateImagePolicy`; the backend embeds the struct in the
`gate_isolation_recorded` audit payload, so the fields reach
`GET /v0/runs/{id}/audit` unchanged (`TestTraceUpload_GateIsolationDeclaredPayload`).
The gate view's `gate_isolation` block does not carry them yet.

## Declared gate image (`gate_container`, E51.3 / [#2136](https://github.com/kuhlman-labs/fishhawk/issues/2136))

A workflow-v2 spec may declare the image every gate of a stage runs in —
`gate_container: {image: <ref>}` or `{dockerfile: <path>, context: <path>}`,
at workflow level with a per-stage override (shape:
`docs/spec/workflow-v2.md` § "Gate container"). The backend resolves the
effective block by stage IDENTITY and serves it on the stage prompt
(`gate_container`, with its `source`); `run()` declares it right after the
prompt fetch, before any gate. A declaration arriving after the selection was
decided is ignored with a `gate_container_declaration_ignored` line.

**Precedence:** stage `gate_container` > workflow `gate_container` >
`FISHHAWK_GATE_IMAGE` > the clone-sandbox / clone fallback. A declared
source never falls back to the env image — not on a policy refusal, a pull
failure or a build failure.

**Image policy (`imagepolicy.go`, `EvaluateImagePolicy`)** — every refusal
holds under every mode, is category C, and never reaches the fix agent:

| Request | `local` | `self-hosted` | `hosted` |
|---|---|---|---|
| `FISHHAWK_GATE_IMAGE` (env) | allowed, unchecked | allowed, unchecked | allowed, unchecked |
| declared image, digest-pinned, empty allowlist | allowed | allowed | **refused** |
| declared image, allowlist permits it | allowed | allowed | allowed (must be pinned) |
| declared image, allowlist configured and does not permit it | **refused** | **refused** | **refused** |
| declared image, tag-only | allowed + `policy_warning` | allowed + `policy_warning` | **refused** |
| declared build, builds disabled (`FISHHAWK_GATE_BUILD=deny`; the `hosted` default) | **refused** | **refused** | **refused** |
| declared build, empty allowlist | allowed, bases unconstrained | allowed, bases unconstrained | **refused** |
| declared build, allowlist configured | every base must pass it | every base must pass it | every base must pass it AND be digest-pinned |
| both sources, half a dockerfile/context pair, a `..` or absolute path | **refused** | **refused** | **refused** |

**Allowlist grammar (`ParseAllowlist`).** Entries separated by commas and/or
whitespace, each exactly one of: a registry host (`ghcr.io`, `registry:5000`,
`localhost`, `localhost:5000` — host and port compared exactly); a namespace
prefix with a trailing slash (`ghcr.io/org/`, matched on whole path components,
so it never permits `ghcr.io/organization/x`); a repository
(`ghcr.io/org/gate`, `docker.io/library/alpine` — any tag or digest); an exact
digest (`ghcr.io/org/gate@sha256:<64 hex>` — that digest only, never a
tag-only ref). Refused at startup, naming the entry: a single-component bare
name (`alpine` is ambiguous — write `docker.io/library/alpine`), a tag-bearing
entry (mutable), any wildcard, anything unparsable.

**Pulling a declared `image:`** (`resolvePulledImage`). Every runtime call goes
through the endpoint-bound runtime CLI with the runner host's inherited
environment, so it authenticates with that CLI's OWN credential store
(`docker login` / `containers-auth.json`); Fishhawk passes no credential and
the spec cannot carry one. A digest-pinned ref is inspected and pulled only
when absent; a tag-only ref is pulled on EVERY gate. Either way the gate runs
by `name@<registry digest>` read back from the inspected `RepoDigests`, so a
concurrent retag cannot change what runs, and a pinned ref must find its own
digest there. Pull is bounded at 10 minutes, inspect at 30 seconds, both
outside the gate timeout. Any pull/inspect failure or timeout, a missing
registry digest, or a pinned digest absent from `RepoDigests` is
`gateUnavailable` (category C). **A declared `image:` must be pullable from a
registry:** a local-only image (built on the host, never pushed) fails the
pull or has no registry digest, and the reason names the remedy — declare
`dockerfile` + `context` to build it in-repo, or have the operator set
`FISHHAWK_GATE_IMAGE` (`PullFailedReason`).

**Building a declared `dockerfile`/`context`** (`resolveBuiltImage`,
`imageresolve.go`, `dockerfile.go`), in order, under TWO separate bounds:
reading the committed source from git, the static guard and materializing the
context share one 20-minute bound (`gateImageBuildTimeout`, armed when
resolution starts), and the build call then gets its OWN
`gateImageBuildTimeout` (20 minutes) — so the worst case is about 40 minutes.
The cache-hit and post-build inspects keep the 30-second inspect bound. All of
it is outside the gate timeout.

1. **Source = the COMMITTED tree at the gate checkout's HEAD**, read from git
   objects (`ResolveBuildSource`), never the working tree — an uncommitted or
   untracked file cannot enter a gate image, and every gate kind on one
   commit sees the same bytes. The context must be a tree, the Dockerfile a
   regular-file blob ≤ 1 MiB; a symlink, a submodule, the wrong type or an
   absent path is `gateRefused`.
2. **Content digest** = a domain-separated sha256 over the context path's git
   TREE id, the Dockerfile's mode + blob id and the optional
   `<dockerfile>.dockerignore` blob id. A tree id covers every byte, path and
   file mode beneath it (a mode-only change moves the digest) and the
   context's own `.dockerignore`, so the digest moves whenever what the
   builder could see moves, and is identical across gate kinds and clones of
   one commit.
3. **Static Dockerfile guard** (`ScreenDockerfile`) on the committed bytes —
   every refusal `gateRefused` BEFORE any build call. It is DENY BY DEFAULT
   and case-insensitive (`add`, `Run`, `from`, `copy --from=` are the same
   instructions), and scans the union of three views (BuildKit-faithful
   logical instructions, heredoc-unaware logical instructions, every physical
   line) so a builder that groups lines differently over-refuses. Refused:
   - a control character other than tab and newline, a CR not followed by
     LF, or any non-ASCII whitespace — BuildKit splits an instruction keyword
     from its arguments on `[\t\v\f\r ]+` and trims Unicode whitespace, so
     `ADD<VT>https://…` or `FROM<FF>image` is an instruction to it; refusing
     these leaves space and tab as the only separators either side sees;
   - a logical line (outside a consumed heredoc body) whose first token is
     not a Dockerfile instruction — refused, never skipped (the builder
     rejects it too, so nothing it would build is refused);
   - a flag token carrying a quote or backslash: the builder's flag lexer
     strips them and keeps a quoted space inside one flag
     (`--mount="type=bind, from=evil/x"`), so the value cannot be classified
     statically;
   - any byte >= 0x80 on an instruction line (comment lines and consumed
     heredoc bodies keep non-ASCII text), and in a flag token in every view:
     BuildKit's flag lexer walks the line BYTE by byte and ends a word on
     0x85 and 0xA0, the UTF-8 continuation bytes of `ą` and `à`, so
     `--mount=type=tmpfs,target=/tà--mount=from=<image>,…` is two mounts to
     it and one flag here (operator-verified against the builder);
   - any parser directive other than `check` — `# syntax=` (selects a
     BuildKit frontend image that can ignore `--network=none`) and `# escape=`
     (changes how the file is tokenized) included;
   - a first non-blank, non-shebang line that is neither a `#` comment nor a
     Dockerfile instruction (a `// syntax=` or JSON `{"syntax": …}` first
     line);
   - an ADD source that is not a plain relative in-context path: one carrying
     `$` (expanded before the builder classifies it), `://`, `@`, `.git`, `:`,
     a host-shaped first component (contains a dot), an absolute path, a
     backslash, a `..` segment, or shell-significant characters — mirroring
     BuildKit's remote-source rules (http(s) URLs and scheme-less / scp-like
     git remotes), and refusing when in doubt; COPY sources get the same
     clean-relative-path rule;
   - `RUN --network=` anything but `none`, `RUN --security=insecure`, a
     malformed `RUN --mount`, a URL-bearing `FROM` / `COPY --from=` /
     `RUN --mount from=`, a non-standalone heredoc token;
   - under `hosted`, `RUN --mount=type=cache` (a cache mount persists on the
     shared daemon across builds and projects);
   - with an allowlist configured, every base — `FROM`, `COPY --from=<image>`,
     `RUN --mount=…,from=<image>`; stage names, `COPY --from` stage indexes
     and `scratch` are not bases, while a digits-only `RUN --mount from=` IS
     an image (BuildKit resolves a mount source by stage name only, so
     `from=0` pulls `docker.io/library/0`) — must pass it, a variable-bearing base (`$`) is refused
     (not statically checkable), and under `hosted` every base must be
     digest-pinned.
4. **Cache:** the image is tagged `fishhawk-gate-build:<digest hex>` with
   labels `fishhawk.gate-build=1` and
   `fishhawk.gate-build.context-digest=sha256:<hex>`; an inspect hit on that
   tag skips the build (so two gate kinds on one commit build once).
5. **Build:** the committed context is materialized into a throwaway dir
   (bounded at 50,000 entries / 1 GiB, checked before anything is written;
   over it is `gateUnavailable`, narrow the context) and built with
   `build --network=none`, the builder applying the context's `.dockerignore`
   (or `<dockerfile>.dockerignore`) to what it sends. A symlinked root ignore
   file is refused. Any build failure or timeout — including a RUN step the
   tree broke — is `gateUnavailable` (category C): a red build costs an
   operator retry, never a verified-tree bypass.

Build tags accumulate one per distinct content digest; prune them with
`docker image prune -a --filter label=fishhawk.gate-build=1` (pruning is
documented, not automated). `context: '.'` puts the whole repository in the
digest, so every commit is a new build — declare the narrowest context that
holds the Dockerfile's inputs.

**Seam.** `diff_coverage` and the auto-format absorb reach the same exec seam
as the verify gates, so they run in the declared image too; there is no
`diff_coverage`-specific image rule. The `FISHHAWK_GATE_IMAGE` EXEC path is
unchanged from its pre-#2136 form (the same container argv, implicit pull
inside the gate timeout, no digest on the evidence). Its RECORDS are not
byte-identical: the `gate_isolation_selected` log line and the
`gate_isolation` evidence now carry `image_source: "env"`. The shared
golden's `container` / `refused` members keep the pre-#2136 shape (an
`image` with no `image_source`), which the runner no longer emits for an
env image; they pin the wire's field set, not the env-image record.

**Honest limits (documented, not closed):**
- **Cache mounts outside `hosted`.** `RUN --mount=type=cache` is accepted
  under `local` / `self-hosted`: the cache persists on the daemon across
  builds and projects, so a build can read a cache another build wrote. That
  is accepted for a single-tenant host; `hosted` refuses it.
- **Build bases are pulled with the runner host's own registry
  credentials.** Without `FISHHAWK_GATE_IMAGE_ALLOWLIST` (the default in
  `local` / `self-hosted`), a declared build's `FROM` / `COPY --from=` /
  `RUN --mount from=` references are unconstrained, and the daemon pulls
  them through the runtime CLI's inherited environment, i.e. the host's
  `docker login` / `containers-auth.json` store. The agent-writable
  Dockerfile therefore gets (1) a daemon-mediated egress channel to any
  registry host, with data encodable in the host or repository name, and
  (2) read access to any private image the host is logged in to, whose
  contents become the gate image's filesystem and can reach gate output.
  The mitigation is the allowlist: configure `FISHHAWK_GATE_IMAGE_ALLOWLIST`
  (or `FISHHAWK_GATE_BUILD=deny`) on any host whose runtime CLI holds
  registry credentials; `hosted` requires an allowlist for builds.
- RUN steps run as root inside the build sandbox (the GATE runs non-root via
  `--user`; the build does not).
- A base image's `ONBUILD` triggers are not visible to the static scan.
- The scan is a best-effort hedge against builder divergence, not a proof of
  equivalence with every builder (BuildKit, buildah, a future frontend): it is
  designed so a divergence over-refuses, but it can under-refuse a shape no
  view anticipates.
- The Dockerfile is agent-writable: the guard protects the HOST, not the
  verdict — an agent that edits the gate image can change what the gate
  measures, exactly as it can edit the gate command's inputs today.
- Daemon-dependent gates and `services` stay with #2137 / `FISHHAWK_GATE_SERVICES`.

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

**A SAFE verdict does not mean pulls work.** Detection probes (`docker context
show` / `inspect`, `docker version`, `podman info` / `version`) never pull, so
a runtime classified SAFE can still hang every image pull when its credential
helper blocks (observed on Docker Desktop for macOS: a
`docker-credential-desktop get` child). The classification is unchanged; the
symptom, the per-step bounds and the remedy are in `runner/README.md` §
"Gate isolation" (Docker Desktop: hung credential helper).

## The container path (`container.go`)

`ContainerSpec.BuildArgv(MountPolicy)` renders exactly:

```
<docker --host | podman --url> unix://<validated socket>
  run --rm --name fishhawk-gate-<hex> --network=none --cap-drop=ALL
  --security-opt=no-new-privileges --workdir /work (--user <uid>:<gid> | --userns=keep-id)
  -v <checkout>:/work
  ( -v <gocache>:/gocache -v <gomodcache>:/gomodcache -v <lintcache>:/lintcache
  | -v <gomodcache>:/gomodcache -v fishhawk-gate-cache-<run>-<stage>-<hex>:/gatecache )
  [-v <passwdfile>:/etc/passwd:ro] [-v fishhawk-gate-svc-<hex>:/pgsock:ro]
  -e K=V … --entrypoint '' <image> <argv…>
```

The first cache alternative is the BIND form (`ContainerSpec.CacheVolume`
empty — byte-identical to the pre-#3967 argv, so every existing golden passes
unedited); the second is the VOLUME form (§ "Persistent cache volume"), whose
`/gatecache` mount is read-write and replaces the `gocache` / `lintcache`
binds.

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

- **The mounts, and nothing else.** The checkout at `/work` (the working
  directory), the three per-exec throwaway caches — or, in the volume form,
  the per-exec module cache plus the runner process's cache volume at
  `/gatecache` — the runner-generated
  `/etc/passwd` (read-only; `PasswdFile`, omitted when the read degraded) and,
  only when a gate service is provisioned, its socket volume at `/pgsock`
  (read-only; `ServiceMounts`). The passwd file is a host bind source and goes
  through `ForbidSocketMounts` with the other four; a service mount is a NAMED
  volume, and `BuildArgv` refuses with NO argv any volume not matching
  `^fishhawk-gate-svc-[0-9a-f]{12}$`, a target that is not absolute, clean and
  non-root, or a mount that is not read-only — so a host path or the daemon
  socket can never ride in as a "volume"
  (`TestBuildArgv_ServiceMountRefusesNonServiceVolume`). The cache volume is
  likewise a NAMED volume: `BuildArgv` refuses with NO argv any
  `CacheVolume` not matching
  `^fishhawk-gate-cache-<uuid>-<uuid>-[0-9a-f]{12}$` (`cacheVolumePattern`;
  no `/`, `.`, `:` or upper case, so it can never name a host path, the
  daemon socket or a second mount field), BEFORE any `-v` token, and the
  checkout, module cache and passwd file still go through
  `ForbidSocketMounts` in volume mode
  (`TestBuildArgv_CacheVolumeRefusesNonCacheVolume`,
  `TestBuildArgv_CacheVolumeStillGuardsCheckout`). No socket, no `/run`,
  no host cache is ever a bind source. These additions are the scoped
  exceptions ADR-063's #2137 and #3967 addenda record.
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
`GIT_CONFIG_GLOBAL/SYSTEM=/dev/null` and `FISHHAWK_GATE_CONTAINER=1`
(`GateContainerMarker`, on EVERY container exec) are appended drop-then-append,
then the runner's `extraEnv` entries drop-then-append so a caller-supplied value
wins. In the volume form `WithCacheVolumeEnv` then re-pins
`GOCACHE=/gatecache/gocache` and `GOLANGCI_LINT_CACHE=/gatecache/lintcache`
drop-then-append (the bind-form `/gocache` / `/lintcache` are not mounted
there). A provisioned service's env (`PostgresService.GateEnv`, the
`FISHHAWK_TEST_PG_URL` pin) is applied after ALL of that by `WithServiceEnv`,
so neither the sanitized env nor extras can redirect the DSN
(`TestContainerEnv_ServiceEnvWinsOverExtras`).
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
host (this repo's testcontainers Postgres) fails under `clone-sandbox`. The
#2137 gate service provisions Postgres on the CONTAINER path only — the
sandbox path has no service — so such a host either runs the container path
(`FISHHAWK_GATE_IMAGE` + `FISHHAWK_GATE_SERVICES=postgres`) or sets
`FISHHAWK_GATE_ISOLATION=clone`. **Residual: IP-layer isolation only.**
`unshare -rn` creates user and network namespaces and no mount namespace, so a
pathname unix socket the runner's user can read (the runtime's own
`docker.sock` / `podman.socket`, an ssh-agent socket) stays reachable from the
sandbox; a gate command that reaches a daemon could start a networked
container. The container path is the isolation that withholds those paths.

## Build-cache posture (`cache.go`) — the INVARIANT

**The host `GOMODCACHE` is the trusted seed and is NEVER bind-mounted into a
container.** The invariant is scoped to the MODULE cache; `GOCACHE` and the
lint cache are never host-seeded or mounted from a host cache either, and
take one of the two postures below. Every container exec gets a fresh EMPTY set of visible cache
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
  `go.work.sum` that is a symlink or not a regular file (at the root, in every
  `go.work` `use` directory and in every directory `replace` target, whose
  own `go.mod` go reads), a `go.work` `use` directory or a directory `replace`
  target (in `go.work` or in any workspace module's `go.mod`) resolving
  outside the `EvalSymlinks`-resolved checkout, or a metadata file that does
  not parse; (4) runs `go mod download all` under
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
  In `runGateInContainer` the seed runs only AFTER the mount guard has
  accepted every bind source, so a checkout the guard refuses is never handed
  to a host-side go process (`TestRunGateInContainer_MountGuardPrecedesSeed`).
  External-canary fixtures: `TestSeedModCache_RefusesMetadataReachingOutsideCheckout`
  (every hostile-metadata row — thirteen, including a directory `replace`
  target whose `go.mod` links to the canary — refused with the exec seam never
  reached and the host canary untouched), `TestSeedModCache_LiveGoSumSymlinkNeverWrittenThrough`
  (the REAL go: without the guard it reads THROUGH the planted `go.sum` link),
  `TestSeedModCache_EnvIsBaseEnvPlusPinsNeverProcessEnv`, and the runner-seam
  `TestRunGateInContainer_SeedUnderSanitizedEnvRefusesHostileMetadata`.
  **Residual, stated:** `GOTOOLCHAIN=local` means a checkout whose `go.mod`
  requires a newer Go than the runner host has fails the seed with a named
  error rather than auto-downloading — accepted over a toolchain
  download-and-exec driven by untrusted metadata.
- `GOCACHE` has NO seed. Under `FISHHAWK_GATE_CACHE=off` (or a degraded
  cache step) it is the per-exec throwaway `gocache` dir: a cold build cache
  per container exec. Otherwise it lives in the runner process's cache volume
  (§ "Persistent cache volume"): cold on the first container exec of the
  process, warm on every later one.
- `VisibleCaches.Remove` is SYMLINK-SAFE (approval condition 2 of #2134): the
  chmod walk examines every entry with Lstat semantics, SKIPS (unlink-only) any
  entry whose mode is `ModeSymlink` — never chmod/chown/open through a link —
  chmods only regular files and directories reached without following a link,
  then `RemoveAll`s. `TestGateContainer_CacheSymlinkNeverReachesHost` plants a
  symlink to a 0444 host file from INSIDE the container and asserts the
  target's mode and contents are untouched, the link is gone with the visible
  dir, and the host module cache carries no planted entry.

**Cost, honestly:** module-cache repopulation on EVERY container exec (three
or more per stage: scoped verify, full verify, diff_coverage, plus fix
iterations and absorbs). `GOCACHE` and the lint cache are cold on the FIRST
container exec of each runner process and warm afterwards under
`FISHHAWK_GATE_CACHE=process`; under `off` (or a degraded step) they are cold
on every exec, the pre-#3967 cost. Process mode adds a `volume create`, a
prepare helper and a write check before every exec (about two extra
container starts, ~1–2s on Docker Desktop, outside the gate timeout; logged
as `cache_ms`). The container path is opt-in via `FISHHAWK_GATE_IMAGE` (or a
spec-declared `gate_container`), so no default runner pays any of it.
**Residuals:** the fallback paths keep the host caches (they are host exec);
there is no cache shared ACROSS runs or stages (by design, § "Persistent
cache volume"), so the first container exec of every stage is cold; the
per-exec module-cache repopulation is accepted. The first container exec also pulls the image
through the daemon inside the gate timeout — pre-pull it. With
`FISHHAWK_GATE_SERVICES=postgres` every container exec also pays a Postgres
start (seconds; OUTSIDE the gate timeout, § "Gate services (#2137)").

## Persistent cache volume (`cachevolume.go`, E51.18 / [#3967](https://github.com/kuhlman-labs/fishhawk/issues/3967))

Under `FISHHAWK_GATE_CACHE=process` (the default) the container path keeps
`GOCACHE` and the golangci-lint cache in ONE runner-owned NAMED volume per
runner PROCESS instead of per-exec throwaway host dirs. The module cache keeps
its per-exec host-seeded posture unchanged (§ "Build-cache posture").

- **Name, label, mount, env.** `fishhawk-gate-cache-<run uuid>-<stage
  uuid>-<12 hex>` (`NewCacheVolume`: the bound owner ids lower-cased plus 6
  crypto-random bytes; non-UUID ids are an error, so the exec degrades),
  labelled `org.fishhawk.gate-cache=process` (`CacheVolumeLabel`), mounted
  read-write at `/gatecache` (`MountGateCache`), with
  `GOCACHE=/gatecache/gocache` and `GOLANGCI_LINT_CACHE=/gatecache/lintcache`
  (`CacheVolumeEnv` / `WithCacheVolumeEnv`, applied before the service env,
  which stays last). Every builder (`CreateArgv`, `PrepareArgv`, `CheckArgv`,
  `RemoveArgv`) renders NOTHING (`ErrContainerSpec`) for a runtime with no
  validated socket, an unsupported kind, a name off `cacheVolumePattern`, an
  empty or flag-shaped image, or a negative uid/gid
  (`TestCacheVolumeArgv_Refusals`).
- **Lifecycle, per container exec** (`gateCacheVolume` in
  `cmd/fishhawk-runner/gateisolation.go`): mint ONCE per (state, owner) —
  recorded for cleanup BEFORE the create is attempted, so teardown covers a
  partial create — then on EVERY exec, each step through `execGateAuxArgvFn`
  under the bound CLI env and its own bound OUTSIDE the gate timeout:
  `volume create` (1m; idempotent — docker re-uses an existing name, podman
  gets `--ignore`; a volume an operator removed mid-stage is re-created and
  re-owned, never auto-created root-owned by `run -v`), the prepare helper
  (5m, covers a cold image pull) and the write check (1m). Success logs
  `gate_cache_volume_ready {volume, reused}`. `gateIsolationState.cleanup()`
  (deferred by `run()`) removes every minted volume on a fresh bounded
  context: `gate_cache_volume_removed`, or `gate_cache_volume_cleanup_failed`
  naming the label (never a panic; the global is still cleared).
- **Why a prepare helper.** A fresh named volume's root is root-owned, so a
  gate running `--user uid:gid` cannot create its cache dirs in it.
  `PrepareArgv` runs a `--rm --network=none --cap-drop=ALL
  --security-opt=no-new-privileges --entrypoint ''` helper with ONLY the
  volume mounted. **Docker** (and any non-rootless runtime): `--user 0:0`
  with exactly `--cap-add=CHOWN` and `sh -c 'o="$1"; shift; mkdir -p -m 0700
  "$@" && chown "$o" "$@"'`, owner and dirs passed as positional arguments,
  never interpolated. Not `install -d -o -g -m`: busybox `install` chowns
  BEFORE it chmods, and root without CAP_FOWNER cannot chmod a directory it no
  longer owns (EPERM, caught live by fixture (o)). **Rootless podman**
  (#3967 approval condition 1): `--userns=keep-id --user uid:gid`, no
  capability, `install -d -m 0700` — the helper runs AS the caller, so the
  directories are the caller's by construction. **uid-mapping assumption,
  stated:** under rootless `--userns=keep-id` the caller's host uid maps to
  the SAME uid in the container and container root maps to a SUBORDINATE uid
  (never the caller), which is why the podman helper must not run as root;
  the podman argv is golden-pinned but NOT live-validated (no podman host). A
  wrong mapping cannot pass silently: `CheckArgv` then runs, under the SAME
  user pin as the gate exec (`userArgs`), a create-and-remove of a probe file
  in both subdirs, and a failure DEGRADES the exec with
  `gate_cache_volume_unavailable` step `check`, reason `ownership: the gate
  uid U:G cannot write …` — never a misattributed red verify later. Whatever
  a prior gate left in the volume (a symlink included) can only redirect the
  CHOWN-only, network-less helper into the volume or its own throwaway rootfs.
  The prepare and check helpers need `sh`, `mkdir`, `chown` (docker) and
  `install` (podman) IN the gate image: a missing one degrades every exec, and
  the e2e image-contents check (`gateImageBinaries`) names it loudly
  (approval condition 3).
- **DEGRADE, never refuse.** A mint error (non-UUID owner), a render error, a
  non-zero exit or a timeout at `create` / `prepare` / `check` logs
  `gate_cache_volume_unavailable {volume, step, reason}` and that exec runs on
  the per-exec `/gocache` + `/lintcache` binds exactly as under `off`
  (`TestRunGateInContainer_CacheVolumeDegrades`, one row per step and
  failure mode). The gate's disposition is unchanged — a cache step is never
  `gateUnavailable` and never a verdict.
- **Timing.** Every container exec logs one `gate_container_timing` line:
  `cache` (`process` | `degraded` | `off`), `cache_volume`, `cache_ms`,
  `seed_ms`, `service_ms`, `exec_ms`, `exit_code`
  (`TestRunGateInContainer_TimingLogLine`). Provisioning sits outside the gate
  timeout, so `exec_ms` alone is what that timeout bounds.

**The boundary decision: the RUN, via the runner process.** No gate can read a
build cache another run's gate wrote; execs inside one run share it. The name
is crypto-random per process, never derived from shared state, never reused,
and it is removed at process exit. Rejected:

1. *Per-run copy-on-write over a read-mostly shared base.* An overlay mount
   needs CAP_SYS_ADMIN, which `--cap-drop=ALL` removes, and there is no
   trusted writer for the base: every gate is untrusted.
2. *One shared cache relying on content addressing.* Go's build cache maps an
   action id (a hash of the INPUTS) to an output the reader cannot re-verify
   without rebuilding (`cmd/go/internal/cache`: `GetFile` checks only the
   recorded size), and `go test` caches PASS results in `GOCACHE` (`go help
   test`, "Test caching"; `scripts/test` runs `go test -race ./...` without
   `-count=1`). A shared writable cache would let one run plant another run's
   object files AND its test verdict.

**Process-lifetime invariant — an ASSUMPTION the boundary rests on (#3967
approval condition 2).** "One runner process serves exactly one stage of one
run." The spawn model guarantees it: `fishhawk-runner` takes ONE `--run-id`
and ONE `--stage-id` per invocation (`cmd/fishhawk-runner/flags.go`), `run()`
serves that one stage and returns, the host dispatcher spawns a fresh runner
from `bin/` per stage dispatch (a retry or fix-up pass is a new process), and
`run()` installs the gate state, binds its owner once
(`gateState.bindOwner(cfg.runID, cfg.stageID)`, `cmd/fishhawk-runner/main.go`)
and defers `cleanup()`. Because the invariant lives outside this package, a
cheap RUNTIME guard backs it: the name embeds the owning run and stage ids,
and before reusing a minted volume `gateCacheVolume` checks
`CacheVolume.BelongsTo(boundRun, boundStage)`; an exec for a different pair
mints its own (`gate_cache_volume_not_reused`) and never mounts the first's
(`TestCacheVolume_BelongsTo`, `TestGateCacheVolume_DifferentOwnerNeverReuses`,
`TestGateCacheVolume_UnboundStateNeverMountsAnotherOwnersVolume`,
`TestGateCacheVolume_DistinctPerProcessNeverShared`). An unbound or non-UUID
owner (a runner started without `--stage-id`) degrades every exec to the
per-exec caches rather than minting an unscoped volume. A future runner that
serves several stages in one process MUST re-bind the owner per stage.

**Residuals, stated not stronger:**

- **Within-run sharing.** An intermediate fix iteration's test can plant a
  cached PASS result or a crafted compiled object that the full re-verify of
  a LATER SHA in the same stage reads. Backstops: CI's required checks run on
  clean caches before merge, the intermediate commits stay in the PR history,
  and the clone path today exposes the operator's host-wide `GOCACHE` to every
  run — a strictly larger channel. Rollback: `FISHHAWK_GATE_CACHE=off`.
- **The first container exec of every stage is still cold** (there is no
  cross-run base, by the decision above).
- **The module cache is still repopulated per exec** (host-seeded, unchanged).
- **golangci-lint's cache key** hashes package file contents with their paths;
  if a stale lint result could survive a fix iteration inside one stage, `off`
  is the mitigation (the measurement compares exec 2's output to exec 1's).
- **Podman** prepare argv is golden-pinned, not live-validated; a failure
  degrades with `gate_cache_volume_unavailable`.
- **Crash-orphaned volumes.** A runner SIGKILLed mid-stage runs no cleanup.
  With NO runner live: `docker volume rm $(docker volume ls -q --filter
  label=org.fishhawk.gate-cache)` (`podman` takes the same arguments).

**Measurement procedure** (`cmd/fishhawk-runner/gatemeasure_test.go`,
OPT-IN, never counted toward the e2e sentinel). On an idle docker host with
NO live runner (verify lock and the shared test Postgres):

```sh
(cd runner && FISHHAWK_GATE_MEASURE_IMAGE=<digest-pinned fishhawk-gate> \
  go test -run TestGateMeasure_ThreeWayFullVerify -timeout 4h -v ./cmd/fishhawk-runner/)
```

Optional: `FISHHAWK_GATE_MEASURE_COMMAND` (default `scripts/test verify`),
`FISHHAWK_GATE_MEASURE_SHA` (default HEAD), `FISHHAWK_GATE_MEASURE_OUT` (a
markdown file the table is also written to). With the image set and no safe
runtime it FAILS rather than skips. It pre-pulls the gate and Postgres images,
then runs the same SHA's full verify on a FRESH clone per exec through
`runBoundedGateCommandDisposed` on three arms: `clone`
(`FISHHAWK_GATE_ISOLATION=clone`) x2; `container-cold` (container + image +
`FISHHAWK_GATE_SERVICES=postgres` + `FISHHAWK_GATE_CACHE=off`) x1;
`container-volume` (the same with `process`) x2 on ONE state (exec 1 = cold
volume, exec 2 = warm), then cleanup. Wall time per exec; CPU from
`getrusage(RUSAGE_CHILDREN)` on the host arm and the gate container's own
cgroup v2 `cpu.stat` `usage_usec` on the container arms (`n/a` when absent;
neither counts the Postgres container); the cache/seed/service/exec split from
the `gate_container_timing` lines. Every exec must exit 0 with disposition
executed, and every container exec must log exactly one timing line with the
arm's cache posture (a degraded volume arm, or a warm exec on a different
volume, fails the walk) — a red or mis-postured exec invalidates the
comparison. The pure helpers are pinned in-loop by `TestGateMeasureHelpers`.
The walk is ~1.5–2h (five full verifies), beyond an implement budget, so it is
an operator walk. **Recorded: pending operator walk.**

**Decision rule.** Make the container path with `FISHHAWK_GATE_CACHE=process`
this repository's local default when the stage-shaped sequence —
container-volume exec 1 + exec 2 — is within **1.25x** of clone exec 1 +
exec 2 in wall time; otherwise stay clone and pursue the cross-run
trusted-base follow-up. The harness prints the ratio and the verdict.
Interpretation to confirm, not asserted: without `-trimpath` the compile
action id includes the package directory, so the clone arm's per-exec temp
checkout path gets no build-cache hits for this repository's own packages
across execs, while the container always mounts the checkout at `/work`.

**Current decision: the local default is UNCHANGED.** The container path stays
opt-in via `FISHHAWK_GATE_IMAGE` (or a declared `gate_container`) until the
walk records numbers against the rule. `FISHHAWK_GATE_CACHE=process` is the
default only for a runner that has already opted into the container path.

## Gate services (`service.go`, `passwd.go`; ADR-063 #2137 addendum, [E51.4 / #2137](https://github.com/kuhlman-labs/fishhawk/issues/2137))

A daemon-dependent gate command — this repository's pgtest-backed backend
suite — cannot start its own database inside the `--network=none` gate
container (no daemon, no daemon socket, no network). With
`FISHHAWK_GATE_SERVICES=postgres` and the container path selected, the runner
provisions a per-exec Postgres SERVICE container and shares ONLY its unix
socket with the gate. Everything in `service.go` is pure argv/env rendering;
`runner/cmd/fishhawk-runner/gateisolation.go` (`provisionGateService`,
`teardownGateService`) executes it.

**Lifecycle, per container exec** (every argv opens with the endpoint binding
and is rendered — and validated — before any of them runs; a runtime with no
`SocketPath`, a malformed name or a flag-shaped image renders none,
`ErrContainerSpec`):

| Step | Argv (after `<bin> --host/--url unix://<socket>`) | Bound |
|---|---|---|
| volume create | `volume create --label org.fishhawk.gate-service=postgres fishhawk-gate-svc-<12 hex>` | 1m |
| run | `run -d --name <svc> --network=none --cap-drop=ALL --security-opt=no-new-privileges --user postgres --label … --tmpfs /var/lib/postgresql/data -e PGDATA=…/pgdata -e POSTGRES_USER=postgres -e POSTGRES_PASSWORD=<random> -e POSTGRES_INITDB_ARGS=--auth-local=scram-sha-256 --auth-host=scram-sha-256 -v <svc>:/var/run/postgresql <image>` — no `-p`, no host path | 5m (covers a cold pull of the service image) |
| readiness | each iteration: `logs <svc>` FIRST; only once it carries `PostgreSQL init process complete; ready for start up.`, `exec <svc> pg_isready -h /var/run/postgresql …`; ready = pg_isready exit 0 AFTER the line | 15s per probe, 90s overall, 250ms interval |
| bootstrap | `exec -e PGPASSWORD=<random> <svc> psql … -c "DO $$BEGIN IF current_setting('server_version_num')::int < 160000 THEN RAISE EXCEPTION 'gate service Postgres must be 16 or newer …'; END IF; END$$" -c "CREATE ROLE fishhawk LOGIN CREATEDB CREATEROLE BYPASSRLS NOSUPERUSER NOREPLICATION PASSWORD 'fishhawk'" -c "CREATE DATABASE fishhawk OWNER fishhawk"` (the version guard runs first, so a pre-16 server creates no role) | 1m |
| gate exec | `BuildArgv` with `-v <svc>:/pgsock:ro` and `FISHHAWK_TEST_PG_URL=postgres://fishhawk:fishhawk@/fishhawk?host=/pgsock&sslmode=disable` applied LAST | the gate's own timeout |
| teardown | `rm -f -v <svc>`, then `volume rm -f <svc>` (one name, two runtime namespaces) | `diffCoverageCleanupTimeout` each, detached context |

- **Least privilege (approval condition 1, widened by #4050).** The gate's
  DSN connects as the role `fishhawk` (`LOGIN CREATEDB CREATEROLE BYPASSRLS
  NOSUPERUSER NOREPLICATION`), never the superuser. initdb's `scram-sha-256`
  local auth means the superuser cannot be reached over the socket without
  its per-service random password, which exists only in the service
  container's env and the host-side runtime argv — never in the gate
  container's env, files, mounts or argv. Why `CREATEROLE` and `BYPASSRLS`
  (#4050): the backend suite assumes the host path's superuser — it seeds
  `FORCE ROW LEVEL SECURITY` tables across accounts (a `NOBYPASSRLS` role,
  owner included, gets `42501 new row violates row-level security policy`)
  and its RLS tests create `NOBYPASSRLS` probe roles. Why that is still
  contained:
  - **`NOSUPERUSER` closes the escape route.** `COPY … TO PROGRAM` and
    server-file access need superuser or membership in
    `pg_execute_server_program` / `pg_read_server_files` /
    `pg_write_server_files`, and the role holds neither.
  - **PostgreSQL 16 bounds `CREATEROLE`, enforced by the version guard.**
    From 16, `CREATEROLE` cannot create or alter a superuser and cannot grant
    a role it holds no `ADMIN` option on, so it cannot reach those
    server-program roles. Before 16 it could, so the bootstrap's first
    statement refuses a server below `server_version_num` 160000 before the
    role exists (`ON_ERROR_STOP` makes that a bootstrap failure: category C,
    the gate never runs).
  - **`BYPASSRLS` affects row visibility only**, inside a throwaway
    `--network=none` database destroyed after the exec. It grants no
    statement the role could not otherwise run.

  Pinned live by `TestPostgresService_LiveLeastPrivilegeOverReadOnlySocket`
  (this package) and fixture (m): from the gate DSN `CREATE ROLE` succeeds
  and an insert into a `FORCE ROW LEVEL SECURITY` table with no policy
  succeeds (the live test), while granting `pg_execute_server_program`
  (`ADMIN option`), `CREATE ROLE … SUPERUSER`, `ALTER ROLE postgres` and
  `COPY … TO PROGRAM` fail with `permission denied`.
  `TestPostgresService_LiveBootstrapRefusesPrePG16` runs the real bootstrap
  against `postgres:15-alpine` (skipping when that image is unavailable) and
  asserts a non-zero exit naming `16 or newer` with no `fishhawk` role
  created. Residual: probe roles the RLS tests create leak on this path,
  since PostgreSQL 16 refuses their creator `DROP OWNED BY`; the cluster is
  per-exec and destroyed after it.
- **No anonymous data volume (approval condition 2).** The image declares a
  `VOLUME` for PGDATA; a `--tmpfs` there means the daemon creates none. The
  service's only mount is the named socket volume, plus the tmpfs; `rm -f -v`
  stays as belt and braces should an overriding image declare another
  `VOLUME`. Fixture (m) inspects exactly one mount (type volume, the
  `fishhawk-gate-svc-` name) and a tmpfs at the PGDATA path.
- **RAM-backed PGDATA (residual).** That tmpfs has no size option and the
  service container no memory limit, so the databases the gate creates (it
  holds `CREATEDB`) live in host memory, bounded only by the runtime's tmpfs
  default. A hostile or simply large suite can pressure the runner host's
  memory. Host availability only — the service has no egress — and the same
  posture as the gate container, which sets no `--memory` or pids limit
  either. A size cap waits on the operator walk showing what the full suite
  needs.
- **Readiness ordering (approval condition 3).** The image's temporary init
  server answers `pg_isready` before init completes, so the logs are read
  FIRST in every iteration (`PostgresInitComplete`). Pinned by
  `TestRunGateInContainer_ReadinessReadsLogsFirst` and the `ready but
  init-complete line missing` row of `TestRunGateInContainer_ServiceProvisionFailures`.
- **Provisioning never counts against the gate timeout, and never reaches the
  fix agent.** Each step carries its own bound above; ANY failure — a step's
  non-zero exit, the readiness deadline, a parent cancellation (which ends
  the readiness poll at once, also mid-interval) — returns
  `gate container: provision postgres service: <step>: <output>`, `-1`,
  `gateUnavailable` (category C), and the gate argv never executes
  (`TestRunGateInContainer_ServiceProvisionFailures`, one row per mode). A
  readiness failure carries the last `pg_isready` output AND the tail of the
  last service-log read (or that read's own failure), because the teardown
  removes the container and its logs with it.
- **Teardown on every exit.** Registered before `volume create` is attempted,
  so a partial provision, a failed gate and a timed-out gate all tear down
  (`TestRunGateInContainer_ServiceTornDownOnGateFailureAndTimeout`); so does
  a parent cancellation mid-gate or mid-readiness, on a context the
  cancellation does not reach
  (`TestRunGateInContainer_CancelledParentContextStillTearsDown`). A
  teardown failure logs `gate_service_cleanup_failed` (with the label) and
  never changes the verdict
  (`TestRunGateInContainer_TeardownFailureDoesNotChangeVerdict`). Runner log
  lines: `gate_service_provisioned` / `gate_service_removed` (service, volume).
- **Crash-orphaned services.** A runner SIGKILLed mid-exec runs no deferred
  teardown. Both objects carry `org.fishhawk.gate-service`; the label-filtered
  manual cleanup is in `runner/README.md` § "Gate isolation".
- **Image provenance.** `FISHHAWK_GATE_POSTGRES_IMAGE` defaults to the TAG
  `postgres:16-alpine`; pin it by digest in any operator environment. An
  override must be PostgreSQL 16 or newer (the bootstrap's version guard).

**Caller passwd entry (`passwd.go`, every container exec, services or not).**
The gate runs as the caller's numeric `uid:gid`, which the gate image's
`/etc/passwd` does not name, so `id -un`, git and libpq's default user fail.
The runner reads the IMAGE's `/etc/passwd` through `PasswdReadArgv` — `run --rm
--network=none --cap-drop=ALL --security-opt=no-new-privileges (--user
uid:gid | --userns=keep-id) --entrypoint '' <image> cat /etc/passwd`,
endpoint-bound, bounded at 5m (a cold gate-image pull) — keeps only its
well-formed entries (`WellFormedPasswd`: seven fields, decimal uid and gid),
because the read's output is the runtime CLI's COMBINED stdout and stderr and
a cold pull's progress lines arrive beside the file with exit 0 (an output
with no entry at all is a failed read;
`TestRunGateInContainer_PasswdReadDropsPullNoise`) — and `BuildPasswd`
appends `<name>:x:<uid>:<gid>:fishhawk gate caller:/tmp:/bin/sh` only when no
entry maps the uid (the name falls back to `fishhawk-gate` unless it matches
`^[A-Za-z0-9._][A-Za-z0-9._-]*$`, so no `:`/newline can forge an entry).
`WritePasswdFile` writes a FRESH `0444` file per exec under the visible-cache
root (approval condition 7); only a SUCCESSFUL read is cached (per image, per
process), so a transient failure is retried on a later exec. A read OR write
failure DEGRADES to no passwd mount with a `gate_passwd_unavailable` log line —
never a refusal (`TestRunGateInContainer_PasswdReadFailureDegrades`,
`TestRunGateInContainer_PasswdWriteFailureDegrades`).

**In-gate test contract (`backend/`).** `pgtest` uses `FISHHAWK_TEST_PG_URL`
as its shared base without testcontainers and Fatalf's — never Skips — on any
failure there; with `FISHHAWK_GATE_CONTAINER=1` and no URL it Fatalf's naming
`FISHHAWK_GATE_SERVICES`. `backend/internal/postgres`'s raw-database helper
creates `fh_raw_<uuid>` databases on that server; the RustFS `tracestore` S3
suite skips inside the gate container. `pgtest`'s own tests take their server
from the same routing (`sharedBaseURL`), so the package is clean in there: `go
test ./internal/pgtest/ ./internal/postgres/` was recorded green inside
`fishhawk-gate:smoke-92445ead` against a provided server on the #2137 fix-up
pass (no daemon socket; NOT the runner-provisioned service, so not the walk).
`directory/internal/store` (a separate module that cannot import `pgtest`)
honours the same routing (#4047): `FISHHAWK_TEST_PG_URL` is its base with
throwaway `fh_dir_<hex>` databases and no testcontainers call, and with
`FISHHAWK_GATE_CONTAINER=1` and no URL it Fatalf's naming
`FISHHAWK_GATE_SERVICES` instead of skipping. Its provided-server path was run
against a hand-started server as the runner's least-privilege role, not
against the runner-provisioned service, so the operator walk below remains
the first run on the real socket.

**Operator walk — required before setting `FISHHAWK_GATE_IMAGE` for this
repository (approval conditions 5 and 6).** In-loop the implement gate proves
the pieces, not the full in-container verify. Before enabling:

1. Build the gate image (`docker buildx build --load -t fishhawk-gate:local
   deploy/gate-image`) and run fixture (n):
   `FISHHAWK_GATE_SELFHOST_IMAGE=fishhawk-gate:local scripts/test single -run
   TestGateContainer_SelfHostPgtestSuite ./runner/cmd/fishhawk-runner/`. It
   drives pgx + golang-migrate (the real `pgtest` external branch) over the
   mounted socket; record its `--- PASS: TestSharedContainer_SharesAndIsolates`
   line. (Recorded once on the #2137 slice-3 run against
   `fishhawk-gate:smoke-92445ead`: PASS.)
2. Run a full container-path verify through the runner and collect POSITIVE
   evidence, not just exit 0: the runner log's `gate_service_provisioned` /
   `gate_service_removed` pair, and pgtest-backed `--- PASS` lines (or a
   non-zero test count) for `backend/internal/pgtest` and a pgtest consumer
   package in the in-container `go test` output, with no
   `FISHHAWK_SKIP_INTEGRATION` skip line. What `scripts/test verify` does in
   there, examined for this walk: every docker helper (lease, sweep, reap)
   opens with `command -v docker || return`, so with no docker CLI they no-op
   rather than fail or skip the run; `scripts/test` never sets
   `FISHHAWK_SKIP_INTEGRATION`; and the container env allow-list (`TZ`/`LANG`/
   `TERM`, `LC_*`, `CGO_*`, `GO*`) cannot carry a host
   `FISHHAWK_SKIP_INTEGRATION` in. A suite that skipped anyway shows as
   `--- SKIP`, which the walk must treat as a failure.

## Never-executed gates → category C

**Classification is OUT OF BAND (#3448).** The runner's exec seam carries a
`gateDisposition` beside the output and exit code
(`runBoundedGateArgvDisposed` / `runBoundedGateCommandDisposed` in
`runner/cmd/fishhawk-runner/main.go`; the un-suffixed names are one-line
delegates that drop it, so every pre-existing caller is unchanged and there
is still one containment implementation), `runGateInContainer` returns one
per pre-exec branch, and `runVerifyCommittedTree` returns it as a FOURTH
value that the two classifying gate sites (`runVerifyFixLoop`,
`runVerifyGateCommitted`) read. Nothing matches a literal in the verify
output any more — that output is untrusted, and a test that printed the
refusal literal as its first line must not be able to steer its own red tree
to category C (`isGateIsolationRefusal` is deleted;
`TestRunVerifyFixLoop_PrintedRefusalLiteralIsNotRefused` pins the
counterfactual). The `gate isolation refused:` lead survives as operator-facing
TEXT in the refusal message and the FailureReason only. Of
`runVerifyCommittedTree`'s SIX production call sites (the fix loop's scoped
pass + full re-verify, the single-shot gate's run + absorb re-run, and the
#960 strict re-verify's run + absorb re-run), the first four read the
disposition in full and the #960 pair reads it for the `timed_out` case ONLY
(#3383) — that site otherwise keeps its `isReverifyInfraFailure`
classification, out of scope for #3448.

The five dispositions, by classification:

- **`executed`** — the argv ran (or the tolerant tmp-dir / clone `skipped`
  branch fired); output + exit code are the verdict, classified exactly as
  before. The zero value, so a `_`-receiving call site and the `skipped`
  branches are never category C by construction
  (`TestRunVerifyCommittedTree_SkipIsExecutedDisposition`).
- **`refused`** — a `refused` selection makes the seam return
  `gate isolation refused: <reason> (mode=… profile=… image=… runtime=… safe=…)`
  and `-1` WITHOUT executing. **Category C**: `runVerifyCommittedTree` reports
  `failed` (never the tolerant `skipped`), `runVerifyGateCommitted` wraps
  `gitops.ErrVerifyInfraFailure` + `errGateIsolationRefused` and skips the
  infra-flake absorb, and `runVerifyFixLoop` breaks with `verify_gate_refused`
  and never re-invokes the fix agent.
- **`unavailable`** — the container path failed BEFORE exec for a reason on
  the HOST, not in the tree: visible-cache root creation, the lint-cache dir,
  the resolved-path mount-guard refusal, the host `GOMODCACHE` probe or
  `go mod download` (an offline host), endpoint binding, the gate-service
  argv, or ANY gate-service provisioning step (#2137: volume create, run,
  readiness, bootstrap). The gate never
  executed, so its verdict says nothing about the tree — the same argument
  the refusal rests on — so it is **category C** exactly like a refusal:
  `runVerifyGateCommitted` wraps `gitops.ErrVerifyInfraFailure` +
  `errGateContainerUnavailable` (distinguishable from a refusal and from a
  persistent infra signature with `errors.Is`), `runVerifyFixLoop` breaks
  with `verify_gate_unavailable`, neither absorbs and neither invokes the fix
  agent. One row per branch, including a stubbed endpoint binder, in
  `TestRunGateInContainer_PreExecFailures`; the classification end to end in
  `TestRunVerifyFixLoop_ContainerUnavailableIsCategoryC` and
  `TestRunVerifyGateCommitted_ContainerUnavailableIsCategoryC`. Trade-off,
  stated: a `go mod download` failure can in principle be tree-caused (a
  bogus `require`) and now parks category C on the container path where the
  host path fails it category B at build; bounded by the failure-safety
  argument `isVerifyInfraFailure` documents — the push decision reads the
  verify OUTCOME, never the classification, so a red seed costs an operator
  `retry_stage`, never a verified-tree bypass.
- **`checkout_refused`** — the host-side seed refused the checkout's OWN
  module metadata (`errors.Is(err, ErrSeedCheckout)`: a symlinked `go.sum`, a
  `replace` outside the checkout). That is TREE-attributable, so it keeps the
  executed-failure classification (category A/B): the fix agent sees the
  message naming the refused file
  (`TestRunVerifyFixLoop_SeedCheckoutRefusedReachesFixAgent`). **Deliberate
  residual:** a legitimate tree whose `replace` target sits outside the
  checkout (`replace => ../sibling`) draws the same `ErrSeedCheckout` and
  reaches the fix agent with the refusing message rather than parking C.

- **`timed_out`** — the gate RAN but the runner's OWN per-exec deadline
  (`executor.verify.timeout`) expired before it returned, so
  `execBoundedHostArgv` SIGKILLed the process group and NO verdict was
  reached (#3383). The host-exec seam reports it as a third return value
  (`timedOut`: child-context deadline exceeded while the parent context is
  live and the command did not return success; a parent cancellation — a
  runner shutdown — stays `-1/false`), and every path maps it to
  `gateTimedOut` — the container path after its `rm -f` KillArgv cleanup
  (`TestRunGateInContainer_TimedOutSeamIsTimedOutDisposition`).
  `neverExecutedInfra()` stays FALSE: the gate executed, it simply did not
  finish, so every site tests the value explicitly. **Category C at all
  four gates, no absorb (a re-run would cost another full timeout), no fix
  agent (a no-verdict fragment is nothing to fix)**: `runVerifyCommittedTree`
  appends the `--- fishhawk-runner: verify TERMINATED, no verdict ---`
  trailer and stamps `timed_out:true` on `verify_run`; `runVerifyFixLoop`
  breaks with `verify_gate_timed_out`; `runVerifyGateCommitted` wraps
  `gitops.ErrVerifyInfraFailure` + `errVerifyGateTimedOut`; the #960
  re-verify wraps `gitops.ErrVerifyInfraFailure`; the working-tree
  `runVerifyGate` wraps `errVerifyGateTimedOut` and `run()` maps it through
  `workingTreeGateFailureCategory`. The LAST execution's disposition
  governs, so an absorb re-run that itself times out is a timed-out gate.
  Long-form: `runner/cmd/fishhawk-runner/README.md` § "Timed-out gate".

Every pre-exec output (refusal text included) matches none of
`isVerifyInfraFailure`'s classes, so a never-executed gate is never absorbed
as a flake.

**Both `run()`-level committed-gate call sites now agree on category C
(#3449).** The fix-loop path (`executor.verify.max_iterations > 0`) always
routed `verify_gate_refused` to category C. The DEFAULT single-shot path
(`max_iterations == 0`, `runner/cmd/fishhawk-runner/main.go`'s `res.OK &&
!appliedFixup && stageType == "implement"` block) used to hardcode category B
for EVERY `runVerifyGateCommitted` error, contradicting this section. It now
maps the returned error through `committedGateFailureCategory`:
`errors.Is(err, gitops.ErrVerifyInfraFailure)` → category C — covering BOTH
the gate-isolation refusal above (`errGateIsolationRefused`) and the
post-absorb persistent infra signature (#2645) below — and everything else
(a red committed tree, `gitops.ErrCommittedTestsFailed`) stays category B.

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
docker-present-but-image-unpullable) is a loud failure, not a green. That
eligibility routes through `gateE2EEligible(probe, detect)` (#3448 note 4),
which returns `probe()` and is handed `DetectRuntime` precisely so
`TestGateE2ESentinel_FiresOnIndependentProbeNotDetectRuntime` can inject a
counting UNSAFE detect stub and assert eligible with ZERO detect calls on
every host — no runtime skip, no refused-state theater.

| Fixture | Proves |
|---|---|
| (a) `TestGateContainer_PrimaryGitUnreachable` | the verify gate's clone has its `.git` INSIDE `/work`; the primary's absolute path is unreachable; a planted hook + ref never reach the primary; `FISHHAWK_VERIFY_LOCK_PATH` is not injected on the container path |
| (b) `TestGateContainer_NoNetwork` | external `wget` and `wget` to a LIVE host-loopback listener both fail; no `eth*` interface (Docker Desktop's VM kernel lists tunnel pseudo-devices, so the assertion is eth-absence, not "only lo"). The listener SERVES HTTP (200 `ok`) and a host-side `http.Get` positive control must succeed BEFORE the container exec (#3448 note 3), so `loopback=failed` discriminates on `--network=none`, not on a dead listener |
| (c) `TestGateContainer_HostFSUnreadable` | a marker outside the four mounts is unreadable; the host-exec control reads it |
| (d) `TestGateContainer_NoDaemonSocket` | no docker/podman socket node inside; no socket under the mounts; the RUNTIME-side argv carries no socket token; a checkout with a planted unix socket is refused BEFORE the real seam |
| (e) `TestGateContainer_EnvAllowList` | runner credentials set in the runner's env are absent inside; `GOPROXY=off` and the cache pins present; `extraEnv` preserved |
| (f) `TestGateContainer_TimeoutKillsContainer` | `sleep 60` at a scaled 2s timeout → -1 within the bound and `ps -a` no longer lists the container |
| (g) `TestGateContainer_LinuxOwnership` | `id -u` is the runner's uid; on Linux a written file is owned by `os.Getuid()` |
| (k) `TestGateContainer_CacheSymlinkNeverReachesHost` | the cache invariant + symlink-safe cleanup, above |
| (l) `TestGateContainer_EndpointBoundAcrossContextSwitch` | selection recorded against the real socket, THEN `DOCKER_HOST`/`CONTAINER_HOST` redirected to an unreachable tcp endpoint and `DOCKER_CONTEXT`/`CONTAINER_CONNECTION` to a nonexistent context → the gate still runs on the validated daemon; the argv opens with the binding; the CLI env pins the validated socket with the redirecting variables dropped |
| (m) `TestGateContainer_PostgresServiceReachableAndContained` | with `FISHHAWK_GATE_SERVICES=postgres` and the Postgres image as the gate image (it carries `psql`): `select 1` over the injected `FISHHAWK_TEST_PG_URL` prints 1; the role is `rolsuper=false, rolcreatedb=true, rolcreaterole=true, rolbypassrls=true` (#4050), `CREATE DATABASE` works, `CREATE ROLE` prints its `CREATE ROLE` command tag, `COPY … TO PROGRAM`, `GRANT pg_execute_server_program TO CURRENT_USER`, `CREATE ROLE … SUPERUSER` and `ALTER ROLE postgres` are `permission denied`, the superuser cannot connect without its password, and its password appears nowhere in the gate output, env or argv; the containment set (no external or host-loopback egress after a positive control, no `eth*`, no daemon socket, only the Postgres socket under `/pgsock`, `/pgsock` and `/etc/passwd` mounted `ro` per `/proc/mounts`, `id -un` resolves, `FISHHAWK_GATE_CONTAINER=1`); host-side inspect via `gateServiceObserver` (NetworkMode none, one named-volume mount and a PGDATA tmpfs, no bind, no port, CapDrop ALL); `rm -f -v` + `volume rm` recorded and nothing left on the daemon; counterfactual: the same DSN with services UNSET fails to connect |
| (o) `TestGateContainer_CacheVolumeWarmWithinProcessFreshAcrossProcesses` | `FISHHAWK_GATE_CACHE=process` (#3967): state A's exec 1 prints the runner uid, `GOCACHE=/gatecache/gocache` and WRITES a marker into it (the prepare helper's ownership, live); A's exec 2 finds the marker (warm within a process); state B — a second runner process, a different run/stage — mounts a different volume and finds nothing (fresh across processes); the positive control C (cache off) does NOT see its own exec-1 marker, so warm discriminates on the volume; A's gate argv mounts `/gatecache` BY NAME (a volume that `BelongsTo` A's run/stage, never an absolute path) beside `--network=none`; a host-side `volume inspect` shows the `org.fishhawk.gate-cache` label before `A.cleanup()` and fails after it |
| (n) `TestGateContainer_SelfHostPgtestSuite` | OPT-IN (`FISHHAWK_GATE_SELFHOST_IMAGE`, the built `fishhawk-gate` image; counts toward the sentinel only when opted in and run): the REAL `pgtest` suite of the committed HEAD runs in the gate image over the socket DSN — pgx + golang-migrate, template bootstrap, per-test databases — exit 0, the PASS line, no `--- SKIP` |
| (measure) `TestGateMeasure_ThreeWayFullVerify` (`gatemeasure_test.go`) | OPT-IN (`FISHHAWK_GATE_MEASURE_IMAGE`), NEVER counted toward the sentinel: the three-way full-verify walk of § "Persistent cache volume"; its pure helpers run in-loop via `TestGateMeasureHelpers` |
| (h) `TestGateClone_PlantedRefNeverReachesPrimary` | clone path: plant never reaches the primary, lock path injected; the `git worktree add` sibling DOES leak the plant |
| (i) `TestGateCloneSandbox_NoNetwork` | Linux-only: a loopback connect fails under `clone-sandbox` through `runBoundedGateCommand` and succeeds under the host-exec control |
| (j) `TestGateHosted_RefusesEndToEnd` | hosted + auto with a REALLY detected runtime whose endpoint is pinned remote (`DOCKER_HOST=tcp://…` via the Getenv probe) → single-shot gate category C, fix loop category C, fix agent never invoked, verify command never ran |

`runner/cmd/fishhawk-runner/main_test.go` pins the #3449 fix at the `run()`
level, on the DEFAULT `max_iterations == 0` call site, driving the refusal
through the REAL `configureGateIsolation(os.Getenv, gateiso.DefaultProbes())`
(the same `DOCKER_HOST`/`CONTAINER_HOST` remote-endpoint recipe as (j), since
`run()` re-derives gate isolation from `os.Getenv` on every invocation and
overwrites the process-wide state — `installGateState` cannot inject the
refusal here): `TestRun_VerifyGateCommitted_HostedRefusal_CategoryC` (hosted
profile refusal → category C, verify command never executed) and
`TestRun_VerifyGateCommitted_PersistentInfraFailure_CategoryC` (a lint-lock
signature persisting past the one-shot absorb → category C, two `verify_run`
events, one `verify_infra_flake_retry`). `TestRun_VerifyGateCommitted_DriftExcludedFailureBlocks`
is the over-broadening guard: a genuine red committed tree still classifies
category B.

## What the sibling issues own

- [#2135](https://github.com/kuhlman-labs/fishhawk/issues/2135) (E51.2) — recording
  the selection on GATE EVIDENCE: landed — see § "Evidence (#2135)". The backend
  records it as a `gate_isolation_recorded` audit row and surfaces it on the
  gate view.
- [#2136](https://github.com/kuhlman-labs/fishhawk/issues/2136) (E51.3) — the
  per-project gate image: landed as the workflow-v2 `gate_container` block,
  § "Declared gate image". It replaced the originally proposed
  `diff_coverage`-only image field: every gate kind, `diff_coverage`
  included, runs in the declared image. This repository does not declare one
  yet (`.fishhawk/workflows.yaml` is unchanged); `gate_container` is also the
  reserved future home of the gate services (`gate_container.services`),
  which today are the operator-side `FISHHAWK_GATE_SERVICES`.
- [#2137](https://github.com/kuhlman-labs/fishhawk/issues/2137) (E51.4) —
  daemon-dependent gate commands under the container path: landed as the
  runner-provisioned Postgres service, § "Gate services (#2137)". This
  repository sets `FISHHAWK_GATE_IMAGE` (the digest-pinned `fishhawk-gate`)
  TOGETHER with `FISHHAWK_GATE_SERVICES=postgres`, and only after that
  section's operator walk is green. `scripts/test lint|verify
  --in-gate-image` (E51.17 / #3966) is unchanged: it provisions no service,
  so it still runs only the Docker-free legs (`verify --no-tests`) and is CI
  parity, not isolation.
- [#2138](https://github.com/kuhlman-labs/fishhawk/issues/2138) (E51.5) — the
  operator-facing posture docs: landed in
  [`docs/deploy/self-hosted.md`](../../../docs/deploy/self-hosted.md) §
  "Runner gate isolation (ADR-063)" (runtime requirement, never the host
  Docker socket, the per-path table including the macOS egress gap, the
  declared profile, the #2136 / #2137 / #3967 limits, the Docker Desktop
  credential-helper hang) and
  [`docs/deploy/hosted-regional.md`](../../../docs/deploy/hosted-regional.md) §
  "Runner gate isolation under hosted", with a runner-host checklist in
  `runner/README.md` § "Gate isolation" and pointer rows in
  `docs/ARCHITECTURE.md` §10. The image those docs point a self-hosted runner
  of this repository at is `fishhawk-gate` (`deploy/gate-image/`).
