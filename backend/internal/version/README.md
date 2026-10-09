# backend/internal/version

Cross-binary version coordination: `git_sha`, `min_runner_version`, and embedded-schema drift detection.

## Exported values and /healthz

`version.go` exports `GitSHA` and `MinRunnerVersion` alongside the existing `Version` — all three stamped at link time via `-X` ldflags. `Version` and `GitSHA` are stamped with the pairs `scripts/release-ldflags <component> <version> <git-sha>` prints — the ONE stamping source for all five binaries (#4117); `scripts/dev` calls it with `Version=dev`, and `backend/Dockerfile` carries the same stanza as a literal byte-pinned to it by `scripts/test-dev`. Long-form: `scripts/README.md` § "Release stamping".

`String()` renders the build identity as `<Version> (<GitSHA>)` — the shape `fishhawk version` prints — or the bare `Version` when `GitSHA` is `unknown`/empty (an unstamped `go build` / `go install`). Its pure core `format(v, sha)` is what the tests drive, so they never mutate the package vars.

### Version surfaces per binary

| Binary | Surface | Output |
|---|---|---|
| `fishhawk` | `fishhawk version` / `--version` | `<Version> (<GitSHA>)` |
| `fishhawk-runner` | `fishhawk-runner version` (a subcommand; there is no `--version` flag) | JSON `{"git_sha":…,"plan_schema_hash":…,"version":…}` |
| `fishhawkd` | `fishhawkd version` / `--version` | `version.String()` |
| `fishhawk-mcp` | `fishhawk-mcp --version` (needs no backend URL or token) | `version.String()` |
| `fishhawk-mcp-shim` | `fishhawk-mcp-shim --version` (spawns no child) | `version.String()` |
| `fishhawkd` (running) | `GET /healthz` | `version`, `git_sha`, `min_runner_version`, `schemas` |
| `fishhawk-mcp` / fishhawkd `/mcp` | MCP `initialize` `serverInfo.version` | `<Version>+<GitSHA>` (bare `<Version>` when unstamped) |

`scripts/test-release-ldflags` builds every binary with the helper's flags and reads each of these surfaces back (r7/r8).

`/healthz` advertises all three plus a `schemas` map (`plan-standard-v1` → sha256, `workflow-v0` → sha256, `workflow-v1` → sha256, `workflow-v2` → sha256); hash is the canonical form: unmarshal JSON → re-marshal (strip whitespace) → sha256 → hex.

Canonical hash functions live in `backend/internal/plan/validate.go::EmbeddedSchemaHash` and `backend/internal/spec/parse.go::EmbeddedSchemaHash` / `EmbeddedSchemaHashV1` (ADR-046, the per-major v0/v1 hashes). The same hash is available on the runner side via `runner/internal/plan/plan.go::EmbeddedSchemaHash`.

`/healthz` additionally echoes the optional `start_nonce` (`server.Config.StartNonce`, set via `--start-nonce` / env `FISHHAWKD_START_NONCE`; omitted when unset) — a per-start opaque identity token `scripts/dev` generates per spawn and round-trips to prove listener identity across OS pid reuse (#1018).

## Runner version-skew enforcement

The prompt-fetch response (`GET /v0/stages/{id}/prompt`) carries `min_runner_version` (omitempty); the runner reads it from `FetchedPrompt.MinRunnerVersion`, compares via `semverLT` (`runner/cmd/fishhawk-runner/main.go`), and exits code 3 (`exitVersionSkew`) when the runner is older.

`semverLT` treats `"dev"` or any unparseable string as non-comparable and always returns false — dev builds never trigger skew.

The runner exposes a `version` subcommand (`fishhawk-runner version`) that prints `{"version":…,"git_sha":…,"plan_schema_hash":…}` as JSON so `fishhawk doctor` can interrogate it without a full run.

## Doctor checks

Three doctor checks in `cli/cmd/fishhawk/doctor.go`:

- `checkBackendSHADrift` — backend `git_sha` vs local `HEAD`.
- `checkRunnerSchemaDrift` — runner schema hash vs backend's `plan-standard-v1` hash.
- `checkCLIVersion` — CLI version vs backend's `min_runner_version`.
