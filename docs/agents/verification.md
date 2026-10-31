# Verification contracts

Use `verify-gate` for execution and `diagnose-red` for an unexplained failure.
[scripts/README.md](../../scripts/README.md) owns gate ordering, flags,
preconditions and failure semantics; [testing rules](testing.md) cover fixtures.

## Gates and scope

- `scripts/test` runs `go test -race` in every registered module; `coverage`
  also checks aggregate coverage. `verify` adds lint, document/schema gates and
  gate harnesses before tests, folding patch coverage into that same test loop.
  `verify --no-tests` omits tests and patch coverage. Do not report it as full verify.
- Full verify is required before push/open PR. Scoped verify restricts only the
  test loop, skips patch coverage with a reason, and widens to full on unknown
  packages. The runner performs full verification of the passing SHA; its
  outside-only failure absorption is narrowly defined in
  [runner documentation](../../runner/README.md), not a general skip license.
- The per-repository lock is shared across worktrees and isolated runner clones.
  A shell refusal (exit 73) is final for that invocation. Do not poll or retry;
  use `scripts/test single` for focused work. Runner wait/displacement rules are
  implemented by the wrapper; do not imitate them in a shell.
- Tests aggregate every failing module rather than stopping at the first one.
  Report all failed modules/tests, distinguish build failures and skips, and
  preserve diagnostics. A failing full loop does not establish patch coverage.
- Aggregate coverage is at least 80% across modules, excluding generated `/db/`.
  Patch coverage is at least 85% of changed statements with a five-statement
  floor; untracked Go files count. Its pre-test snapshot prevents tree rewrites
  from converting a real failure into a skip. Never use dev bypass knobs to
  claim a passing gate. See scripts/README for precondition versus integrity failures.

## Toolchain and execution environment

Run `scripts/check-gate-image --host` for the host lint-toolchain precondition;
read the current pins from `deploy/gate-image/Dockerfile`. The exact lint version,
its build Go major/minor and standalone gofmt must match the checker. A mismatch
is environmental: prefer the pinned toolchain on PATH or `--in-gate-image`.
`go env -w GOTOOLCHAIN=...` changes global configuration and switches `go`, not
standalone gofmt. Do not make that persistent change without authorization.

`--in-gate-image` gives CI parity to Docker-free legs, not full runner isolation.
Its override is `FISHHAWK_TEST_GATE_IMAGE`; `FISHHAWK_GATE_IMAGE` configures
spawned runners instead. Rebuild the operator's local gate image after Dockerfile
or pin changes. A runner build with `--network=none` cannot build this networked
Dockerfile; see [gate-image documentation](../../deploy/gate-image/README.md).

On unexplained red, check host load first, then tools, Docker and reproducibility
on the base. Red on clean main alone does not prove an environmental cause.
Do not clear shared caches or remove live testcontainers as speculative fixes.

## Changing the verification machinery

Register every new verify leg/harness and its inputs in
`backend/internal/basedelta/verifylegs_test.go`; ensure the live repository
`verdict_transfer` declaration covers its inputs. The declaration is
operator-authored. See scripts/README § Leg registration and
[basedelta](../../backend/internal/basedelta/README.md).

Pin tools downloaded and executed by CI/release run steps to immutable versions
or tags, including shell installers and npx packages. GitHub Actions major refs
are the exception, managed by Dependabot. Keep CI/tool documentation/gate-image
pins consistent; search skills for repeated facts and update them together.
The separate repeated-fact checker proposal remains #3943.

Helm render tests skip when helm is absent; report that gap. Registry probes
have their own reachability/sentinel skip conditions. Review-prompt evidence is
an additional path-triggered check outside verify; see
[change checklists](change-checklists.md).
