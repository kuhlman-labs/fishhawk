# Security review conventions (this repository)

This file lists the repository-specific security facts that the generic security remit (`docs/review/security-remit.md`) defers to. When the persona is enabled (#3762 / #3764), it is declared under `review_conventions`, with `applies_to` set to the sensitive paths, and the `feature_change` plan and implement stages select it. Reviewers may quote it verbatim.

## Trust boundaries

- Untrusted text must reach an agent prompt only inside the server's quarantine envelope (`backend/internal/prompt`), never as instructions. Untrusted text means issue bodies, comments, PR text, crew messages, user reports, verify output and repository documents.
- The verify-output, crew-message, review-convention and user-report envelopes each have a byte-exact drift copy in `backend/internal/agenteval/injection_test.go`. A change to one framing must update its copy.

## Tokens, scopes and authorization

- Run-bound tokens (`mcp:run:*`) must be refused on operator verbs. The refusal stands even when such a token also holds `write:runs`.
- Every operator write route checks its scope (`write:runs`, `write:approvals`, and so on) and its account (`requireStageAccount` / `memberWrite` tiers). Anonymous callers are refused on every non-public route.
- Approver gates use the forge identity `github:<login>`, not an MCP token subject.
- **Auth change obligations (the `AGENTS.md` auth checklist).** A change that adds or tightens handler-side authorization states three things in its PR:
  - the impact inventory: `fishhawkd token migrate` dry-run output;
  - the migration path;
  - whether it is safe to ship.

## Load-bearing credential controls

- The runner's default-deny gate env allow-list (`runner/cmd/fishhawk-runner/gateenv.go`). A new allow-listed variable is a widening.
- The credential-stripped `env -i` exec of the untrusted preview binary (`scripts/dev`, #1569).
- Acceptance credential isolation (`runner/README.md` § `FISHHAWK_ACCEPTANCE_CREDENTIAL_ISOLATION`): git config pins, refused credential-locator passthroughs, Seatbelt denies, and a synthetic `HOME`.
- `GIT_CONFIG_GLOBAL`/`GIT_CONFIG_SYSTEM` are pinned to `/dev/null` in `scripts/test` and in the runner's gate subprocesses.

## Path restrictions and built-in constraints

- The built-in `agent_instruction_paths` constraint (#3942) refuses these paths on every implement stage: `.agents/`, `.claude/` and `.codex/`, plus `AGENTS.md`, `AGENTS.override.md`, `CLAUDE.md` and `CLAUDE.local.md`, at any depth and in any letter case.
  - `feature_change` exempts exactly the root `AGENTS.md`.
  - An added exemption or an `enforce: false` is a widening.
- `.fishhawk/workflows.yaml` `forbidden_paths`, escalations (`max_autonomy`) and `permissions` are permission surfaces. A removal or narrowing is a widening.

## Audit and fail-closed

- Every audit category an emitter writes must be registered in `backend/internal/audit/categories.go`.
- An action whose `AppendChained` fails must not proceed when the entry is its authorization record (for example a trigger entry that is also a budget counter).

## Pin policy (the `AGENTS.md` pinning rule)

- Tooling fetched and executed in CI or release `run:` steps (install scripts, `npx` packages) is pinned to an immutable tag or version. GitHub Action major tags are the exception; Dependabot tracks them.
- The gate image's tool ARGs (`deploy/gate-image/Dockerfile`) must match the repository pins. `scripts/check-gate-image` enforces this.
- Every image the Helm chart renders must exist. `scripts/test-helm-render` r15 checks this.
