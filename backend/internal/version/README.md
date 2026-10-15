# backend/internal/version

Cross-binary version coordination: `git_sha`, `min_runner_version`, and embedded-schema drift detection.

## Exported values and /healthz

`version.go` exports `GitSHA` and `MinRunnerVersion` alongside the existing `Version`. `MinRunnerVersion` is resolved from the committed floor (§ "Runner floor"); `Version` and `GitSHA` are stamped at link time via `-X` ldflags, with the pairs `scripts/release-ldflags <component> <version> <git-sha>` prints — the ONE stamping source for all five binaries (#4117); `scripts/dev` calls it with `Version=dev`, and `backend/Dockerfile` carries the same stanza as a literal byte-pinned to it by `scripts/test-dev`. Long-form: `scripts/README.md` § "Release stamping".

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

## Runner floor (#4135)

`backend/internal/version/min-runner-version` is the ONE committed source of truth for the runner floor: a single `vMAJOR.MINOR.PATCH` line (exactly three numeric parts, the shape the runner's `parseSemver` requires; `TestCommittedMinRunnerVersionWellFormed` pins it). It is `go:embed`ded and `init()` resolves `MinRunnerVersion` from it:

- a **release-shaped `Version`** (`vMAJOR.MINOR.PATCH`, optional `-prerelease` suffix — what a `backend/v*` tag stamps) gets the committed floor;
- an **explicit `-X ...MinRunnerVersion=<v>` stamp** wins (the var keeps its constant `"dev"` initializer so `-X` stays effective);
- everything else (`dev`, `ci-<sha>`, `sha-<sha>`, an unstamped build) stays `"dev"`, meaning no enforcement.

Every surface that reads `version.MinRunnerVersion` then advertises the floor on a release: `/healthz` `min_runner_version`, the prompt-fetch `min_runner_version`, and the diagnostics bundle.

**Why not an `-X` pair from `scripts/release-ldflags`.** The released image is built by `backend/Dockerfile`, which cannot exec the helper (`.dockerignore` excludes `scripts/`), and the release workflow's image job passes only `VERSION` and `GIT_SHA` build-args. A helper-only stamp would leave the released image advertising `dev`; keying on the already-stamped `Version` covers the binary and image paths with no helper, Dockerfile or workflow change. `scripts/test-release-ldflags` r9 pins the resolution end to end.

**Bump procedure.** Change `min-runner-version` in the same PR as the backend change that needs newer runner behaviour, set to that runner release's version.

**Refusal reason.** A runner older than the floor exits 3 and emits `{"event":"runner_failed","reason":"version_skew","detail":…}` naming both versions and the upgrade remedy, so the local detached reaper records `version_skew` as the stage failure reason instead of a generic `runner exited 3`.

**`fishhawk doctor` reads this floor too.** `checkCLIVersion` (`cli/cmd/fishhawk/doctor.go`) compares the CLI's own version against `min_runner_version` — this RUNNER floor — so a floor bump can make `doctor` warn on an older CLI even though the CLI is not a runner.

## Runner version-skew enforcement

The prompt-fetch response (`GET /v0/stages/{id}/prompt`) carries `min_runner_version` (omitempty); the runner reads it from `FetchedPrompt.MinRunnerVersion`, compares via `semverLT` (`runner/cmd/fishhawk-runner/main.go`), and exits code 3 (`exitVersionSkew`) when the runner is older.

`semverLT` treats `"dev"` or any unparseable string as non-comparable and always returns false — dev builds never trigger skew.

The runner exposes a `version` subcommand (`fishhawk-runner version`) that prints `{"version":…,"git_sha":…,"plan_schema_hash":…}` as JSON so `fishhawk doctor` can interrogate it without a full run.

## Doctor checks

Three doctor checks in `cli/cmd/fishhawk/doctor.go`:

- `checkBackendSHADrift` — backend `git_sha` vs local `HEAD`.
- `checkRunnerSchemaDrift` — runner schema hash vs backend's `plan-standard-v1` hash.
- `checkCLIVersion` — CLI version vs backend's `min_runner_version`.
