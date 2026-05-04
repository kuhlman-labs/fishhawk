<!--
  Header for runner releases. The runner-release workflow appends
  GitHub's auto-generated changelog (commits since the previous
  runner/v* tag) underneath this body via
  `generate_release_notes: true`.

  To customize per-release: edit this template before tagging, or
  open the GitHub Release after publication and revise the body.
-->

## Fishhawk Runner

GitHub Action that runs an agent under a Fishhawk workflow stage and ships the signed trace bundle to the backend. Pin via:

```yaml
- uses: kuhlman-labs/fishhawk/runner@runner/vX.Y.Z
```

## What's in this release

- `fishhawk-runner-<version>-linux-amd64` — standalone binary (`-ldflags`-stamped with this tag's version). Useful for ad-hoc invocation, replay, and supply-chain inspection.
- `runner-<version>.sbom.spdx.json` — SPDX-JSON Software Bill of Materials produced by [`anchore/sbom-action`](https://github.com/anchore/sbom-action). Lists every Go module the runner links against.
- `SHA256SUMS` — sha256 of every artifact above.
- `SHA256SUMS.sig` + `SHA256SUMS.pem` — keyless [cosign](https://docs.sigstore.dev/cosign/overview/) signature + Fulcio certificate chain. Issued by the GitHub Actions OIDC identity for this repo + workflow.

## Verifying the release

```sh
# Download SHA256SUMS, SHA256SUMS.sig, SHA256SUMS.pem from this release.
cosign verify-blob \
  --certificate-identity-regexp 'https://github.com/kuhlman-labs/fishhawk/\.github/workflows/runner-release\.yml@.*' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com \
  --signature SHA256SUMS.sig \
  --certificate SHA256SUMS.pem \
  SHA256SUMS
sha256sum -c SHA256SUMS
```

A passing verify means the file came from this repo's runner-release workflow on this tag — no managed PGP key in the loop.

## Compatibility

Pinned tags are the supported customer surface. `@main` exists for self-execution in this repo only and may break across commits.

The runner speaks the v0 trace bundle wire format (ADR-007 / [#71](https://github.com/kuhlman-labs/fishhawk/issues/71)) and the v0 backend API (`docs/api/v0.openapi.yaml`).
