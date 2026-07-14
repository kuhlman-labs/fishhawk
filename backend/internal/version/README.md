# backend/internal/version

Cross-binary version coordination: `git_sha`, `min_runner_version`, and embedded-schema drift detection.

## Exported values and /healthz

`version.go` exports `GitSHA` and `MinRunnerVersion` alongside the existing `Version` — all three stamped at link time via `-X` ldflags.

`/healthz` advertises all three plus a `schemas` map (`plan-standard-v1` → sha256, `workflow-v0` → sha256, `workflow-v1` → sha256); hash is the canonical form: unmarshal JSON → re-marshal (strip whitespace) → sha256 → hex.

Canonical hash functions live in `backend/internal/plan/validate.go::EmbeddedSchemaHash` and `backend/internal/spec/parse.go::EmbeddedSchemaHash` / `EmbeddedSchemaHashV1` (ADR-046, the per-major v0/v1 hashes). The same hash is available on the runner side via `runner/internal/plan/plan.go::EmbeddedSchemaHash`.

`/healthz` additionally echoes the optional `start_nonce` (`server.Config.StartNonce`, set via `--start-nonce` / env `FISHHAWKD_START_NONCE`; omitted when unset) — a per-start opaque identity token `scripts/dev` generates per spawn and round-trips to prove listener identity across OS pid reuse (#1018).

## Runner version-skew enforcement

The prompt-fetch response (`GET /v0/stages/{id}/prompt`) carries `min_runner_version` (omitempty); the runner reads it from `FetchedPrompt.MinRunnerVersion`, compares via `semverLT` (`runner/cmd/fishhawk-runner/main.go`), and exits code 3 (`exitVersionSkew`) when the runner is older.

`semverLT` treats `"dev"` or any unparseable string as non-comparable and always returns false — dev builds never trigger skew.

The runner exposes a `version` subcommand (`fishhawk-runner version`) that prints `{"version":…,"plan_schema_hash":…}` as JSON so `fishhawk doctor` can interrogate it without a full run.

## Doctor checks

Three doctor checks in `cli/cmd/fishhawk/doctor.go`:

- `checkBackendSHADrift` — backend `git_sha` vs local `HEAD`.
- `checkRunnerSchemaDrift` — runner schema hash vs backend's `plan-standard-v1` hash.
- `checkCLIVersion` — CLI version vs backend's `min_runner_version`.
