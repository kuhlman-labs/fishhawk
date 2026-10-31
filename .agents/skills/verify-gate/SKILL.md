---
name: verify-gate
description: Run and interpret Fishhawk verification safely. Use before push/PR, or when asked to run verify, tests or the CI gate.
---

# Run the verify gate

`scripts/verify-change` is the repository committed-tree gate: path-aware
frontend checks followed by `scripts/test verify`. Offline dependencies, exit 75,
stage order and coverage contracts are owned by [scripts/README.md](../../../scripts/README.md) and routed
by the [verification guide](../../../docs/agents/verification.md). Lint and static
checks run before the test loop; scoped and no-tests runs do not establish a full
pass. Avoid maintaining a second copy of the gate's leg list here.

## 1. Preflight — do all of these first

```sh
pgrep -fl '[f]ishhawk-runner .*--run-id' || echo no-live-runner
uptime; sysctl -n hw.ncpu
go env GOVERSION
scripts/check-gate-image --host
docker info >/dev/null 2>&1 && echo docker-ok || echo docker-DOWN
git status --short
```

- **Live runner.** It may be holding the per-repository verify lock. A shell `verify` that meets a live holder **refuses immediately**. That refusal is final for that invocation: do NOT retry or poll it. Narrow to `scripts/test single -run TestX ./path/to/pkg/...` instead, or wait for the run to finish.
- **Load average above ~4× the core count.** The host is starved, and unrelated tests will flake. Use the `diagnose-red` skill (orphaned agent processes) before running anything.
- **Toolchain mismatch.** The host checker compares the exact lint version, its build Go and standalone gofmt with the checked-in gate-image pins. Treat `host_toolchain_mismatch` as environmental, never a reason to edit code. Put the matching tools first on PATH or use `--in-gate-image`. Read the current Go version from `deploy/gate-image/Dockerfile`; prefer a per-command toolchain/PATH change. Changing persistent `go env -w` settings needs authorization and does not switch standalone gofmt.
- **Postgres prerequisite.** Host tests use Docker/testcontainers. A runner gate container instead uses its supplied `FISHHAWK_TEST_PG_URL` and has no Docker daemon; a missing or failing service there is a failure, never a skip.
- **Dirty tree.** That's allowed: patch coverage diffs merge-base → work tree, so uncommitted edits are gated too.

## 2. Run

| Situation | Command |
|---|---|
| Before a PR / final check | `scripts/verify-change` |
| Iterating on known packages | `scripts/test verify --packages backend/internal/foo,runner/internal/bar` (repo-relative). It skips patch coverage, so finish with a full run |
| One test | `scripts/test single -run TestName ./backend/internal/foo/` |
| Lint only | `scripts/test lint` |

A full verify takes many minutes. Run it in the background (or with your shell tool's longest timeout) and wait for it to exit. Don't start a second concurrent verify.

## 3. Read the result

| Failure | Meaning / fix |
|---|---|
| golangci-lint / gofmt | Fix the code; the matching pinned `gofmt -w` / `goimports -w` on the named files |
| `Schema sync` drift | A `docs/spec/` canonical file changed without mirroring it: run `scripts/sync-schemas` and commit the mirrors |
| Doc-line budget | A `docs/ARCHITECTURE.md` line is over 1000 chars; move the prose to the package README |
| `check-adr` | Record/index mismatch: `scripts/check-adr --write-index` (see the `record-adr` skill) |
| Site gates (`check-site-voice`, `check-site-ia`, site reference drift) | Banned §5 term on the site, a sidebar/page mismatch, or a stale generated region (`scripts/gen-site-reference`) |
| `TestKnownCategoriesCoversEmittedCategories` | A new audit category: register it in `backend/internal/audit/categories.go` |
| `TestCrossModuleWireParity` | A cross-module wire struct drifted: see `backend/internal/wirecontract/README.md` |
| Patch coverage below 85% | Add tests that exercise the changed lines. Never set `FISHHAWK_SKIP_PATCH_COVERAGE` to get a pass |
| A test unrelated to the diff | Likely environmental (host load, toolchain, Docker): use the `diagnose-red` skill before touching code |
| `host_toolchain_mismatch` | Fix the tool environment or use the gate image; do not change code |
| Refused: verify lock held | See preflight. Don't retry |

A skipped harness (`zsh` or `helm` absent) is a printed skip, not a failure. Report it as an uncovered area.

## 4. Report

State pass/fail and the failed gate leg. If the test loop ran, report every failing module and its failing tests/build errors from the aggregate summary. Include the relevant diagnostics and every skip; distinguish scoped/no-tests evidence from a full pass.
