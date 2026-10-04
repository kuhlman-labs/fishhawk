# syntax=docker/dockerfile:1
#
# fishhawk-gate: the pinned verify toolchain for this repository (E51.17 / #3966).
# It is the image ADR-063's container gate path names via FISHHAWK_GATE_IMAGE,
# defined in-repo and pin-checked, and the image `scripts/test lint|verify
# --in-gate-image` runs for CI parity. Long form: deploy/gate-image/README.md.
#
# BUILD CONTEXT: deploy/gate-image/ alone. The image installs tools and copies
# NO repository files, so a checkout is always bind-mounted at run time:
#   docker buildx build --load -t fishhawk-gate:local deploy/gate-image
#
# PINS. Each ARG default below is a pin site, and scripts/check-gate-image holds
# them to the rest of the repository:
#   GO_VERSION              exact MAJOR.MINOR.PATCH; its MAJOR.MINOR must equal
#                           every go.work / */go.mod `go` directive and every
#                           workflow `go-version:` literal, and no go.work /
#                           go.mod `go` or `toolchain` directive may exceed it
#                           (GOTOOLCHAIN=local below refuses a newer one).
#   GOLANGCI_LINT_VERSION   EXACTLY the golangci-lint install.sh tag in every
#                           .github/workflows file and scripts/test's hint.
#   HELM_VERSION            exact vMAJOR.MINOR.PATCH, MAJOR >= 4: test-helm-render
#                           r9 renders NOTES.txt with `helm install --dry-run`,
#                           which Helm v3 (both --dry-run and --dry-run=client)
#                           refuses without a reachable cluster, and this image
#                           has none; Helm v4 renders it cluster-free.
#   CHECK_JSONSCHEMA_VERSION the `check-jsonschema` docs/spec validator.
# Static mode (a `scripts/test verify` leg) compares this file to the repo;
# `scripts/check-gate-image --image REF` compares a BUILT image to this file.
# Bumping a pin without rebuilding the image therefore fails one of the two.
#
# NO DOCKER. No docker engine, CLI, plugin or socket enters this image. The
# harnesses that drive docker (test-container-lease, test-verify-scope) stub it
# with a fake on PATH, and the daemon-dependent Go tests (testcontainers
# Postgres) are out of reach here until #2137. scripts/check-gate-image fails
# on a docker package in this file and on a docker binary inside a built image.
#
# ARCHITECTURE. Every download is keyed on BuildKit's TARGETARCH, declared
# inside the stage. No architecture literal may appear in a URL (the AGENTS.md
# GOARCH/TARGETARCH trap, #2912): an installer that detects the arch with
# `uname` reads the EMULATED arch under QEMU, so CI's install.sh is not used.
#
# TOOL SET, enumerated from the scripts the non-test verify legs execute
# (cmd_lint, the doc-line budget, scripts/sync-schemas, every harness in
# _verify_gate_harnesses, check-site-voice, check-site-ia, check-adr and
# check-coverage.py), not from memory:
#   from the golang:bookworm base  go, gcc/libc6-dev (cgo for -race), make,
#                                  git, curl, procps, tar/gzip, coreutils
#                                  (sha256sum, timeout, mktemp, stat, realpath)
#   apt, below                     zsh (scripts/dev, test-dev), jq (scripts/test,
#                                  scripts/dev), python3 (check-adr,
#                                  check-coverage.py, harness fixtures), lsof +
#                                  netcat-openbsd + openssl (test-dev; it uses
#                                  the BSD `nc -l HOST PORT` form, which
#                                  netcat-traditional rejects), perl (`shasum`
#                                  in test-dev / test-site-reference), diffutils,
#                                  file, ca-certificates
#   fetched, below                 golangci-lint, helm (test-helm-render,
#                                  test-site-reference), check-jsonschema
# Deliberately absent: docker, kubectl, kind and node. Each harness that names
# them stubs a fake on PATH or skips with a printed reason.
#
# RUNTIME CONTRACT. No ENTRYPOINT (the runner passes --entrypoint '') and no
# USER (callers pass --user uid:gid). Everything works as an arbitrary uid with
# HOME=/tmp, matching runner/internal/gateiso's container env, EXCEPT name
# lookup: an arbitrary uid has no /etc/passwd entry here, so `whoami` fails and
# test-helm-render r9h cannot pass. `scripts/test --in-gate-image` therefore
# mounts a passwd file mapping the caller's uid; the runner's container path
# does not yet (#2137).

ARG GO_VERSION=1.25.6

FROM golang:${GO_VERSION}-bookworm

ARG TARGETARCH
ARG GOLANGCI_LINT_VERSION=v2.8.0
ARG HELM_VERSION=v4.2.4
ARG CHECK_JSONSCHEMA_VERSION=0.38.0

LABEL org.opencontainers.image.source="https://github.com/kuhlman-labs/fishhawk" \
      org.opencontainers.image.description="Fishhawk verify gate toolchain (deploy/gate-image)"

SHELL ["/bin/bash", "-o", "pipefail", "-c"]

RUN apt-get update \
 && apt-get install -y --no-install-recommends \
      ca-certificates \
      curl \
      diffutils \
      file \
      git \
      jq \
      lsof \
      make \
      netcat-openbsd \
      openssl \
      perl \
      procps \
      python3 \
      python3-venv \
      zsh \
 && rm -rf /var/lib/apt/lists/*

# Fail closed on an architecture this image does not publish, before any fetch.
RUN case "${TARGETARCH}" in \
      amd64|arm64) ;; \
      *) echo "fishhawk-gate: unsupported TARGETARCH '${TARGETARCH}' (want amd64 or arm64)" >&2; exit 1 ;; \
    esac

# golangci-lint: the same release tarball CI's pinned install.sh downloads,
# checksum-verified against the release's checksums file (exactly one line).
RUN set -eux; \
    ver="${GOLANGCI_LINT_VERSION#v}"; \
    name="golangci-lint-${ver}-linux-${TARGETARCH}"; \
    base="https://github.com/golangci/golangci-lint/releases/download/${GOLANGCI_LINT_VERSION}"; \
    tmp="$(mktemp -d)"; cd "$tmp"; \
    curl -fsSLo "${name}.tar.gz" "${base}/${name}.tar.gz"; \
    curl -fsSLo checksums.txt "${base}/golangci-lint-${ver}-checksums.txt"; \
    awk -v f="${name}.tar.gz" '$2 == f' checksums.txt > sum.txt; \
    test "$(wc -l < sum.txt)" -eq 1; \
    sha256sum -c sum.txt; \
    tar -xzf "${name}.tar.gz"; \
    install -m 0755 "${name}/golangci-lint" /usr/local/bin/golangci-lint; \
    cd /; rm -rf "$tmp"; \
    golangci-lint version

# helm: get.helm.sh tarball with its published .sha256sum.
RUN set -eux; \
    name="helm-${HELM_VERSION}-linux-${TARGETARCH}.tar.gz"; \
    tmp="$(mktemp -d)"; cd "$tmp"; \
    curl -fsSLo "${name}" "https://get.helm.sh/${name}"; \
    curl -fsSLo "${name}.sha256sum" "https://get.helm.sh/${name}.sha256sum"; \
    sha256sum -c "${name}.sha256sum"; \
    tar -xzf "${name}"; \
    install -m 0755 "linux-${TARGETARCH}/helm" /usr/local/bin/helm; \
    cd /; rm -rf "$tmp"; \
    helm version --short

# check-jsonschema: bookworm's system python is externally managed (PEP 668),
# so it goes into its own venv and is linked onto PATH.
RUN set -eux; \
    python3 -m venv /opt/check-jsonschema; \
    /opt/check-jsonschema/bin/pip install --no-cache-dir "check-jsonschema==${CHECK_JSONSCHEMA_VERSION}"; \
    ln -s /opt/check-jsonschema/bin/check-jsonschema /usr/local/bin/check-jsonschema; \
    check-jsonschema --version

# Never download a different Go than the one pinned above: a go.mod asking for a
# newer toolchain fails loudly instead of reaching the network.
ENV GOTOOLCHAIN=local
