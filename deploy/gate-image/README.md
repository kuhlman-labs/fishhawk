# deploy/gate-image — the pinned verify toolchain image (E51.17 / [#3966](https://github.com/kuhlman-labs/fishhawk/issues/3966))

`deploy/gate-image/Dockerfile` defines `fishhawk-gate`, a multi-arch
(`linux/amd64`, `linux/arm64`) image carrying this repository's exact verify
toolchain. It has two consumers:

1. **CI parity today.** `scripts/test lint --in-gate-image` and `scripts/test
   verify --in-gate-image` run the gate's Docker-free legs inside the image, so
   a local run uses CI's tool versions rather than whatever the host has
   (the local-golangci-newer-than-CI and Go-toolchain-drift reds).
2. **The runner's container gate path.** ADR-063's Decision already
   provides a runner gate image via `FISHHAWK_GATE_IMAGE`; this is that image,
   defined in-repo and pin-checked. **For this repository set it only together
   with `FISHHAWK_GATE_SERVICES=postgres`
   ([#2137](https://github.com/kuhlman-labs/fishhawk/issues/2137)), and only
   after the operator walk is green** — see "Runner gate image" below.

## Image contract

| Property | Value |
|---|---|
| Base | `golang:${GO_VERSION}-bookworm` (official multi-arch image) |
| Build context | `deploy/gate-image/` alone; the image copies NO repository files |
| Entrypoint / user | none / none — callers pass `--entrypoint ''` and `--user uid:gid` |
| Runtime env | works as an arbitrary uid with `HOME=/tmp`, except that such a uid has no `/etc/passwd` entry in the image (`whoami` fails, so test-helm-render r9h fails); both callers SUPPLY one: `--in-gate-image` mounts one (step 6), and the runner's container path mounts a runner-generated read-only `/etc/passwd` on every exec (#2137; when that read or write degrades, `gate_passwd_unavailable`, the gap returns for that exec); `GOTOOLCHAIN=local` is baked in |
| Docker | none: no engine, CLI, plugin or socket, enforced in both check modes |
| Architecture | every download keyed on BuildKit `TARGETARCH`, declared in the stage |

**Tool inventory**, enumerated from the scripts the non-test verify legs run
(the Dockerfile header carries the per-tool reason): from the base `go`,
`gcc`/`libc6-dev` (cgo for `-race`), `make`, `git`, `curl`, `procps`, coreutils;
from apt `zsh`, `jq`, `python3` (+ `python3-venv`), `lsof`, `netcat-openbsd`
(test-dev's BSD `nc -l HOST PORT` form), `openssl`, `perl` (`shasum`),
`diffutils`, `file`, `ca-certificates`; fetched and checksum-verified
`golangci-lint`, `helm`, `check-jsonschema` (in a venv at
`/opt/check-jsonschema`, because bookworm's python is PEP 668
externally-managed). Deliberately absent: `docker`, `kubectl`, `kind`, `node`.

## Pins and the drift chain

| Dockerfile ARG | Held to (by `scripts/check-gate-image`) |
|---|---|
| `GO_VERSION` (exact `MAJOR.MINOR.PATCH`) | MAJOR.MINOR of every `go.work` / `go.mod` `go` directive and every workflow `go-version:` literal; no `go` / `toolchain` directive may EXCEED it in full (`GOTOOLCHAIN=local` refuses a newer one) |
| `GOLANGCI_LINT_VERSION` | EXACTLY the golangci-lint `install.sh` tag in every `.github/workflows/*` file and in `scripts/test`'s install hint |
| `HELM_VERSION` (exact `vMAJOR.MINOR.PATCH`, currently `v4.2.4`) | MAJOR ≥ 4, because `scripts/test-helm-render` r9 renders `NOTES.txt` with `helm install --dry-run`, which Helm v3 (`--dry-run=client` too) refuses without a reachable cluster and the image has none. The helm render gate's documented CI half (`deploy/helm/fishhawk/README.md`, `setup-helm` `version:`) carries the same `v4.2.4`; nothing checks that pair, so bump both together |
| `CHECK_JSONSCHEMA_VERSION` | the `check-jsonschema` validator `AGENTS.md` names for `docs/spec/` |

The chain closes in two halves:

- **repo pins ⇔ Dockerfile** — `scripts/check-gate-image` STATIC mode, a
  `scripts/test verify` leg (`_verify_gate_image_pins`). Bumping the CI
  golangci-lint pin without the Dockerfile ARG (or the reverse) fails verify
  in-loop. It reads files only, `.github/workflows/*` included (reading a path
  forbidden to implement stages is not an edit).
- **Dockerfile ⇔ built image** — `scripts/check-gate-image --image REF
  [--platform P]` runs one probe inside the image and compares `go`,
  `golangci-lint`, `helm` and `check-jsonschema` exactly, and fails when a
  docker binary is inside. Every `--in-gate-image` run performs it first, and
  the workflow below runs it on every build and weekly against the published
  `:main`.

Go parity with CI is MAJOR.MINOR only, because CI itself floats the patch
(`go-version: '1.25'`); the Dockerfile's `GO_VERSION` is the repo's one exact
patch pin.

## Local build

```sh
docker buildx build --load -t fishhawk-gate:local deploy/gate-image
scripts/check-gate-image --image fishhawk-gate:local
```

## `--in-gate-image`

```sh
scripts/test lint --in-gate-image                   # scripts/test lint in the image
scripts/test verify --in-gate-image                 # scripts/test verify --no-tests in the image
FISHHAWK_TEST_GATE_IMAGE=fishhawk-gate:local scripts/test lint --in-gate-image
```

What it does, in order, each step failing closed (long form: the block comment
above `cmd_in_gate_image` in `scripts/test`):

1. Arguments are validated first: `verify --in-gate-image --packages …` is
   rejected (the scoped test loop needs the Docker daemon the image lacks), as
   is an unknown `lint` option.
2. `docker` absent from PATH → exit 1 naming docker. Never a skip.
3. `verify` only: the HOST takes the real per-repository verify lock and
   refuses on a live holder exactly as `scripts/test verify` does. The
   container gets a PRIVATE `FISHHAWK_VERIFY_LOCK_PATH=/tmp/fishhawk-verify.lock`:
   lock liveness is a `kill -0` PID check, which cannot cross a PID namespace,
   so a container sharing the host lock would judge a live host holder dead and
   displace it.
4. The image ref (`$FISHHAWK_TEST_GATE_IMAGE`, default
   `ghcr.io/kuhlman-labs/fishhawk-gate:main`) is resolved to an image ID,
   pulling it once when it is not present locally.
5. `scripts/check-gate-image --image <ID>` runs against that ID. Any tool
   version that disagrees with this checkout's Dockerfile pins refuses the run,
   so a stale locally cached `:main` can never produce results presented as
   CI-identical. The resolved ID and its repo digests are printed.
6. The image's `/etc/passwd` is read (`cat` in a throwaway container on the
   same ID) and, when no entry maps your uid, one is appended (your login name,
   or `fishhawk-gate` when it is not a plain `[A-Za-z0-9._-]` word; home
   `/tmp`). The file lives in a scratch dir under the cache dir, is mounted
   READ-ONLY over `/etc/passwd`, and is removed after the run. Without it the
   uid is nameless: `whoami` fails and test-helm-render r9h's `$(whoami)`
   probe becomes an empty needle that matches every argv. An unreadable
   `/etc/passwd` refuses the run.
7. `docker run --rm --entrypoint '' --user $(id -u):$(id -g)` on that same ID,
   with the checkout bind-mounted at its own absolute path (plus the git common
   dir at its own path when it lies outside the checkout, for a linked
   worktree), three persistent per-user caches (`gocache`, `gomodcache`,
   `lintcache` under `${FISHHAWK_GATE_CACHE_DIR:-${XDG_CACHE_HOME:-$HOME/.cache}/fishhawk-gate}`),
   `HOME=/tmp`, `GOTOOLCHAIN=local`, and `safe.directory=*` as COMMAND-scope git
   config (`GIT_CONFIG_COUNT/KEY/VALUE`, which survives the inner scripts/test's
   `GIT_CONFIG_GLOBAL/SYSTEM=/dev/null` pins). No docker socket is ever mounted.
   The container's exit status is the command's.

**Posture: CI parity, NOT isolation.** The container has the NETWORK (cold
module caches need downloads), the checkout is mounted READ-WRITE, and it runs
as your uid. It answers "would CI's toolchain pass this tree", nothing about
containing what the gate executes; that is the runner's container path
(ADR-063), not this helper.

**Docker Desktop: a locked screen hangs this helper's pull.** This helper
pulls under YOUR docker config. On Docker Desktop for macOS that config's
`"credsStore": "desktop"` is consulted even for an anonymous public pull, and
`docker-credential-desktop get` blocks for as long as the macOS screen is
LOCKED (#4046), so step 4's pull makes no progress and prints no error; the
tell is a `docker-credential-desktop get` child under the `docker` process.
This is the environment, not the image or the script. Unlock the screen, or
use the anonymous-pull remedy (`DOCKER_CONFIG` at a fresh config holding only
`{"auths":{"fishhawk.invalid":{}}}` — no `credsStore`, no `credHelpers`, no
credential; a bare `{"auths":{}}` still lets the CLI auto-detect and call
`docker-credential-osxkeychain` — plus an explicit `DOCKER_HOST`): `docs/deploy/self-hosted.md` § "Docker Desktop: a locked screen
and the credential helper". It works for the default
`ghcr.io/kuhlman-labs/fishhawk-gate:main` only after the one-time
public-visibility step below, and never for a private image. The RUNNER needs
no such remedy: since #4046 it pins its own credential-free docker config.

**Limits.** The `go test -race` loop and the patch-coverage gate do not run
in-image here: the backend tests need a Postgres, and this helper provisions
none (no daemon in the image). The runner's container path does — #2137's
runner-provisioned Postgres service, reached over a read-only unix-socket
mount — but `--in-gate-image` is a shell helper with no such service, so
`verify --in-gate-image` still runs `verify --no-tests` (every other leg)
inside. Host-side, `verify --no-tests` runs the same legs without the image.

**Variable name.** The helper reads `FISHHAWK_TEST_GATE_IMAGE`, deliberately not
`FISHHAWK_GATE_IMAGE`: exporting the latter in a shell that spawns the runner
switches the runner's `auto` gate isolation onto the container path.

**Docker-free PATH for a hint or a test.** Build it from a temp dir of
symlinks to only the binaries you need, never from `/usr/bin`, which holds
`docker` on a typical Linux host:

```sh
d="$(mktemp -d)"; for b in bash git sed awk grep cat; do ln -s "$(command -v "$b")" "$d/$b"; done
PATH="$d" scripts/test lint --in-gate-image   # exits 1 naming docker
```

## Runner gate image

This is the recommended `FISHHAWK_GATE_IMAGE` for this repository, pinned by
DIGEST (`ghcr.io/kuhlman-labs/fishhawk-gate@sha256:…`). It can also be
declared in the spec through the workflow-v2 `gate_container` block
([E51.3 / #2136](https://github.com/kuhlman-labs/fishhawk/issues/2136):
`gate_container: {image: ghcr.io/kuhlman-labs/fishhawk-gate@sha256:…}`, which
beats `FISHHAWK_GATE_IMAGE`); this repository does not declare it yet, and the
service it needs still comes from the runner-side `FISHHAWK_GATE_SERVICES`
(`gate_container.services` is reserved, not implemented), which applies to the
container path whichever source named the image. A declared `image:` must be
pullable from a registry — a locally built, never-pushed tag fails category C
naming the `dockerfile`/`context` or `FISHHAWK_GATE_IMAGE` remedy. The image carries no daemon; the
daemon-dependent backend tests reach Postgres through the runner's gate
service ([#2137](https://github.com/kuhlman-labs/fishhawk/issues/2137)), so
**set it only TOGETHER with `FISHHAWK_GATE_SERVICES=postgres`** (and a
digest-pinned `FISHHAWK_GATE_POSTGRES_IMAGE`): without the service, `pgtest`
fails closed inside the container and every verify is red. **Enable it only
after the operator walk** — fixture (n), `TestGateContainer_SelfHostPgtestSuite`,
run against this image with its PASS line recorded, then one full
container-path verify with pgtest-backed PASS lines and no `--- SKIP` — in
`runner/internal/gateiso/README.md` § "Gate services (#2137)", which is also
the contract.

The runner pulls this image under a runner-owned, credential-free docker
config (E51.26 / #4046), so a locked macOS screen no longer hangs that pull —
but the pull then succeeds only once the GHCR package is public (§ "One-time
operator steps"). A private copy needs `FISHHAWK_GATE_DOCKER_CONFIG` naming a
config that holds GHCR credentials. Contract: `runner/README.md` § "Gate
isolation" (Runtime CLI credentials).

## One-time operator steps

1. Install the workflow below as `.github/workflows/gate-image.yml`
   (`.github/workflows/**` is forbidden to Fishhawk implement stages, so a run
   cannot install it).
2. After its first successful publish on `main`, make the GHCR package public:
   GitHub → kuhlman-labs → Packages → `fishhawk-gate` → Package settings →
   Change visibility → Public. GHCR creates a new container package PRIVATE by
   default, and anonymous `docker pull` of the default image fails until then.

## CI workflow (operator-installed)

Jobs: `pins` (static check), `build` (per-platform buildx `--load` under QEMU,
the dynamic check for both arches, and `scripts/test lint --in-gate-image`
against the amd64 build — lint parity with `ci.yml`'s lint job on the same
commit), `publish` (main only: multi-arch push of `:<sha>` + `:main`, then the
dynamic check against the pushed `:<sha>` for both arches), and
`published-drift` (weekly: the dynamic check against the published `:main`,
which goes red when the pins moved without a republish). `permissions: {}` at
the top; `packages: write` only on `publish`. Action refs follow the AGENTS.md
rule (floating majors, Dependabot-bumped; `docker/login-action` matches the
exact pin the other workflows use); every ref was verified to exist on
2026-10-04. Building arm64 under QEMU is slow (apt and pip run emulated); a
native arm64 runner (`ubuntu-24.04-arm`) is the faster alternative for that
matrix leg.

```yaml
name: gate-image

on:
  push:
    branches: [main]
    paths:
      - 'deploy/gate-image/**'
      - 'scripts/check-gate-image'
      - 'scripts/test'
      - 'go.work'
      - '*/go.mod'
      - '.github/workflows/**'
  pull_request:
    paths:
      - 'deploy/gate-image/**'
      - 'scripts/check-gate-image'
      - 'scripts/test'
      - 'go.work'
      - '*/go.mod'
      - '.github/workflows/**'
  schedule:
    - cron: '17 6 * * 1'
  workflow_dispatch:

permissions: {}

concurrency:
  group: gate-image-${{ github.ref }}
  cancel-in-progress: ${{ github.event_name == 'pull_request' }}

env:
  IMAGE: ghcr.io/kuhlman-labs/fishhawk-gate

jobs:
  pins:
    name: Gate image pins (static)
    if: github.event_name != 'schedule'
    runs-on: ubuntu-latest
    permissions:
      contents: read
    steps:
      - uses: actions/checkout@v7
      - run: scripts/check-gate-image

  build:
    name: Gate image build (${{ matrix.platform }})
    if: github.event_name != 'schedule'
    needs: pins
    runs-on: ubuntu-latest
    permissions:
      contents: read
    strategy:
      fail-fast: false
      matrix:
        platform: [linux/amd64, linux/arm64]
    steps:
      - uses: actions/checkout@v7
      - uses: docker/setup-qemu-action@v4
      - uses: docker/setup-buildx-action@v4
      - name: Build and load
        uses: docker/build-push-action@v7
        with:
          context: deploy/gate-image
          platforms: ${{ matrix.platform }}
          load: true
          provenance: false
          tags: fishhawk-gate:ci
      - name: Dynamic pin check
        run: scripts/check-gate-image --image fishhawk-gate:ci --platform ${{ matrix.platform }}
      - name: Lint parity (scripts/test lint inside the image)
        if: matrix.platform == 'linux/amd64'
        env:
          FISHHAWK_TEST_GATE_IMAGE: fishhawk-gate:ci
        run: scripts/test lint --in-gate-image

  publish:
    name: Gate image publish
    if: (github.event_name == 'push' || github.event_name == 'workflow_dispatch') && github.ref == 'refs/heads/main'
    needs: build
    runs-on: ubuntu-latest
    permissions:
      contents: read
      packages: write
    steps:
      - uses: actions/checkout@v7
      - uses: docker/setup-qemu-action@v4
      - uses: docker/setup-buildx-action@v4
      - uses: docker/login-action@v4.6.0
        with:
          registry: ghcr.io
          username: ${{ github.actor }}
          password: ${{ secrets.GITHUB_TOKEN }}
      - name: Build and push (amd64 + arm64)
        uses: docker/build-push-action@v7
        with:
          context: deploy/gate-image
          platforms: linux/amd64,linux/arm64
          push: true
          tags: |
            ${{ env.IMAGE }}:${{ github.sha }}
            ${{ env.IMAGE }}:main
      - name: Dynamic pin check of the pushed image
        run: |
          for p in linux/amd64 linux/arm64; do
            docker pull --platform "$p" "$IMAGE:$GITHUB_SHA"
            scripts/check-gate-image --image "$IMAGE:$GITHUB_SHA" --platform "$p"
          done

  published-drift:
    name: Published :main drift (${{ matrix.platform }})
    if: github.event_name == 'schedule'
    runs-on: ubuntu-latest
    permissions:
      contents: read
    strategy:
      fail-fast: false
      matrix:
        platform: [linux/amd64, linux/arm64]
    steps:
      - uses: actions/checkout@v7
      - uses: docker/setup-qemu-action@v4
      - run: docker pull --platform ${{ matrix.platform }} "$IMAGE:main"
      - run: scripts/check-gate-image --image "$IMAGE:main" --platform ${{ matrix.platform }}
```

## Testing

- `scripts/test-check-gate-image` — hermetic harness for both check modes
  (temp fixture roots, fake docker).
- `scripts/test-in-gate-image` — hermetic harness for `verify --no-tests` and
  `lint|verify --in-gate-image`: argument order (including a docker-absent
  arm), the docker-absent refusal, the host lock (held during the run, refused
  on a live holder, released on every exit), the pre-run pin check (a
  mismatching image never runs the inner command), image resolution and pull,
  the full docker argv, the linked-worktree common-dir mount, and exit-status
  propagation.

Both run inside `scripts/test verify` via `_verify_gate_harnesses`, with no
network and no real Docker. The live image build and in-image smoke are
operator-validated (the workflow above).
