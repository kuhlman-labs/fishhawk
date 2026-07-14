# backend/internal/auth

Authentication helpers for fishhawkd, including the GitHub App manifest conversion flow.

## GitHub App manifest flow (E4.7)

`github_manifest.go` implements `GitHubManifest.Convert(ctx, code)`, which POSTs to `https://api.github.com/app-manifests/{code}/conversions` and returns App ID + slug + OAuth client ID/secret + webhook secret + PEM.

Two handlers in `backend/internal/server/manifest.go`:

- `GET /v0/auth/github/manifest-flow-start` mints state, sets a short-lived `fishhawk_manifest_state` cookie (separate from the OAuth state cookie), and renders an auto-submitting form to GitHub's manifest endpoint.
- `GET /v0/auth/github/manifest-callback` validates state (single-use; cookie cleared on entry), exchanges the one-shot `code`, and renders an HTML success page with the secrets and a copy-paste `.env` block. `Cache-Control: no-store` keeps the page out of browser history.

Operator-facing flow in `docs/github-app/README.md`. The hosted-deploy "persist secrets to the configured backend" path is deferred.
