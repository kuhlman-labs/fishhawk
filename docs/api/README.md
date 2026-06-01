# Fishhawk API documentation

Two artifacts live here:

| File | Purpose |
|---|---|
| [`v0.openapi.yaml`](./v0.openapi.yaml) | **Source of truth** — OpenAPI 3.1 spec for the v0 backend HTTP surface |
| [`v0.md`](./v0.md) | Human-readable companion — endpoint map, auth model, caller flows, error codes |
| [`redocly.yaml`](./redocly.yaml) | Lint configuration; disables checks that don't apply to OAuth redirect endpoints |

If `v0.md` and `v0.openapi.yaml` disagree, the OpenAPI document wins. CI lints
the OpenAPI document on changes; new endpoints land in both files in the same
PR.

## Versioning

Per ADR (#46) and `MVP_SPEC.md` §5.1.1:

- `/v0` is **unstable** — breaking changes allowed until v1 cuts at the end
  of phase 4.
- Once `/v1` ships, both prefixes coexist for one minor version cycle, then
  `/v0` is retired.
- Webhook ingestion (`/webhooks/github`) is unversioned because GitHub
  controls the request shape.

## Working on the API

```sh
# Lint
npx -y @redocly/cli@2.31.5 lint --config docs/api/redocly.yaml docs/api/v0.openapi.yaml

# Preview rendered docs
npx -y @redocly/cli@2.31.5 preview-docs docs/api/v0.openapi.yaml
```

When adding an endpoint:

1. Add the path to `v0.openapi.yaml` with all expected response codes.
2. Update the endpoint map and (if non-trivial) caller flows in `v0.md`.
3. Implement the handler in `backend/internal/server/` with a name matching
   the `operationId`.
4. Lint locally before pushing.
