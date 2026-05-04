# Multi-stage build for fishhawkd (E13.3 / #60).
#
# Stage 1: full Go toolchain compiles a static binary with the
# version stamped via -ldflags (matching runner/release.yml's
# convention).
#
# Stage 2: distroless/static-debian12 runs the binary as a
# non-root user. The image carries no shell, no package manager,
# and no libc beyond what a static Go binary needs — small
# attack surface, ~25 MB.

ARG GO_VERSION=1.25

# ---- builder ----
FROM golang:${GO_VERSION}-bookworm AS builder

WORKDIR /src

# Copy go.work + modules first so dependency-only changes hit
# the docker layer cache instead of re-running every build.
#
# go.work.sum is intentionally gitignored (no external imports
# yet, so it doesn't exist in CI's build context). The glob
# matches go.work always and go.work.sum opportunistically once
# external deps land — no Dockerfile change needed at that point.
COPY go.work* ./
COPY backend/go.mod backend/go.sum ./backend/
COPY runner/go.mod runner/go.sum ./runner/
COPY cli/go.mod cli/go.sum ./cli/
COPY verifier/go.mod verifier/go.sum ./verifier/

# Pre-download deps. With a populated cache, source-only changes
# avoid re-resolving the module graph.
RUN cd backend && go mod download

# Now bring in the actual sources. We only need backend's tree
# plus go.work; the other modules are referenced by go.work but
# go build ./backend/... resolves their packages from the local
# paths above.
COPY backend ./backend
COPY runner ./runner
COPY cli ./cli
COPY verifier ./verifier

ARG VERSION=dev

# CGO_ENABLED=0 produces a fully static binary — required by the
# distroless/static base image (no libc).
RUN cd backend && CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build \
        -trimpath \
        -ldflags="-s -w -X github.com/kuhlman-labs/fishhawk/backend/internal/version.Version=${VERSION}" \
        -o /out/fishhawkd \
        ./cmd/fishhawkd

# ---- runtime ----
# distroless/static-debian12: glibc-free, no shell, runs as
# uid 65532 (`nonroot`). Pin by digest in production deploys.
FROM gcr.io/distroless/static-debian12:nonroot AS runtime

# OCI labels make `docker inspect` + GHCR's UI carry useful
# provenance without us hand-building extra metadata files.
LABEL org.opencontainers.image.source="https://github.com/kuhlman-labs/fishhawk"
LABEL org.opencontainers.image.description="Fishhawk backend control plane (fishhawkd)."
LABEL org.opencontainers.image.licenses="Apache-2.0"
LABEL org.opencontainers.image.title="fishhawkd"

# Default listen port from cmd/fishhawkd/serve.go's --addr
# default; operators override via FISHHAWKD_ADDR.
EXPOSE 8080

# Use the binary directly — no shell to inherit signals or rewrite
# the command line.
ENTRYPOINT ["/fishhawkd"]
CMD ["serve"]

COPY --from=builder /out/fishhawkd /fishhawkd
