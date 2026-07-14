# backend/internal/spec

Workflow-spec parsing, semantic validation, and version-routed embedded JSON Schemas for `.fishhawk/workflows.yaml`.

## Agent-version compatibility ranges (v1.4, E32.13 / #1743)

`executor.agent_version` + `reviewers.agents[i].agent_version` declare the semver comparator RANGE (e.g. `">=2.1 <2.2"`) of agent CLI versions a workflow was validated against, failing dispatch loudly when the resolved CLI version falls outside it (the #1741 opaque-CLI-drift diagnosis).

- Matcher/validator: `spec.ValidAgentVersionRange` / `spec.MatchAgentVersionRange` in `backend/internal/spec/agentversion.go`, with a byte-parity twin in `cli/internal/spec/agentversion.go`. Called by the semantic validator (`validate.go`).
- The executor range is threaded to the runner via `promptResponse.agent_version_range` (`backend/internal/server/prompt.go`).
- The runner's own duplicated `matchAgentVersionRange` (`runner/cmd/fishhawk-runner/main.go`) fails the stage **pre-spawn category-C** (`agent_version_mismatch`) on an out-of-range resolved (#1769-probed) version, or degrades-and-proceeds (`agent_version_uncomparable`) on an unprobeable one.
- Absent range = no constraint.
- The binary pin stays a host concern via `FISHHAWK_AGENT_BIN` / `FISHHAWK_CODEX_BIN` (#1741 / #1769).
- The reviewer-side (codex-only) enforcement is a sibling change.
