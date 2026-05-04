<!--
  Header for fishhawkd backend releases. The backend-release
  workflow appends GitHub's auto-generated changelog (commits
  since the previous backend/v* tag) underneath this body via
  `generate_release_notes: true`.
-->

## Fishhawk Backend (`fishhawkd`)

The Go control-plane service that orchestrates workflow runs, persists state, evaluates policy, and exposes the REST API consumed by the CLI and Web UI.

## What's in this release

- **Container image**: `ghcr.io/kuhlman-labs/fishhawkd:<version>` and `:latest`. Multi-stage build → distroless static binary, ~25 MB. Runs as `nonroot` (uid 65532), no shell, no package manager.
- **`fishhawkd-<version>.sbom.spdx.json`** — SPDX-JSON SBOM of the image, listing every Go module the binary links against.
- Image is signed with [cosign](https://docs.sigstore.dev/cosign/overview/) keyless via GitHub Actions OIDC. No managed PGP key in the loop.

## Pulling and verifying

```sh
cosign verify ghcr.io/kuhlman-labs/fishhawkd:<version> \
  --certificate-identity-regexp '\.github/workflows/backend-(build|release)\.yml@.*' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com

docker pull ghcr.io/kuhlman-labs/fishhawkd:<version>
```

The verify-identity covers both the per-commit build workflow (`backend-build.yml`) and the tagged-release workflow (`backend-release.yml`). Either is acceptable provenance.

## Running

The image's entrypoint is `/fishhawkd serve`. Configure via env vars / flags:

```sh
docker run --rm \
  -p 8080:8080 \
  -e FISHHAWKD_DATABASE_URL=postgres://… \
  -e FISHHAWKD_S3_BUCKET=… \
  -e FISHHAWKD_GITHUB_APP_ID=… \
  -e FISHHAWKD_GITHUB_APP_PRIVATE_KEY_FILE=/secrets/app-key.pem \
  -v /local/path/app-key.pem:/secrets/app-key.pem:ro \
  ghcr.io/kuhlman-labs/fishhawkd:<version>
```

Full env-var list in `backend/README.md`. Production deploys to AWS ECS Fargate per [ADR-009](https://github.com/kuhlman-labs/fishhawk/issues/73); the task-definition + IAM scaffolding is tracked in a follow-up.

## Compatibility

`fishhawkd` speaks:

- The v0 REST API (`docs/api/v0.openapi.yaml`)
- The v0 trace bundle wire format (ADR-007 / [#71](https://github.com/kuhlman-labs/fishhawk/issues/71))
- The v0 workflow spec (`docs/spec/workflow-v0.schema.json`)

Migrations are applied by the `migrate up` subcommand: `docker run … migrate up`. They're embedded in the binary; no external SQL files needed.
