<!--
  Header for CLI releases. The cli-release workflow appends
  GitHub's auto-generated changelog (commits since the previous
  cli/v* tag) underneath this body via
  `generate_release_notes: true`.

  To customize per-release: edit this template before tagging, or
  open the GitHub Release after publication and revise the body.
-->

## Fishhawk CLI (`fishhawk`)

The command-line client: validate a workflow spec, start and drive runs, approve gates, and inspect run state against a `fishhawkd` backend.

## What's in this release

- `fishhawk-<version>-darwin-arm64` — Apple Silicon Mac.
- `fishhawk-<version>-darwin-amd64` — Intel Mac.
- `fishhawk-<version>-linux-amd64` — Linux x86_64.
- `fishhawk-<version>-linux-arm64` — Linux ARM64.
- `cli-<version>.sbom.spdx.json` — SPDX-JSON SBOM produced by [`anchore/sbom-action`](https://github.com/anchore/sbom-action). Lists every Go module the CLI links against.
- `SHA256SUMS` — sha256 of every artifact above.
- `SHA256SUMS.sig` + `SHA256SUMS.pem` — keyless [cosign](https://docs.sigstore.dev/cosign/overview/) signature + Fulcio certificate chain. Issued by the GitHub Actions OIDC identity for this repo + workflow.

Each binary is stamped with this tag's version and commit: `fishhawk version` prints `<version> (<commit>)`.

## Verifying the release

```sh
# Download SHA256SUMS, SHA256SUMS.sig, SHA256SUMS.pem from this release.
cosign verify-blob \
  --certificate-identity-regexp 'https://github.com/kuhlman-labs/fishhawk/\.github/workflows/cli-release\.yml@.*' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com \
  --signature SHA256SUMS.sig \
  --certificate SHA256SUMS.pem \
  SHA256SUMS
sha256sum -c --ignore-missing SHA256SUMS
```

A passing verify means the file came from this repo's cli-release workflow on this tag — no managed PGP key in the loop.

## Installing

```sh
chmod +x fishhawk-<version>-<os>-<arch>
mv fishhawk-<version>-<os>-<arch> /usr/local/bin/fishhawk
fishhawk version
```
