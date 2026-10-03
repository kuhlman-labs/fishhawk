---
name: verify-gate
description: Run Fishhawk's `scripts/test verify` gate (lint, schema-sync, gate harnesses, tests, patch coverage) safely and read its result. Use before pushing or opening a PR, when asked to "run verify", "run the gate", "run the tests", or to check a change passes CI locally. Covers the per-repo verify lock, host-load and toolchain preflights, scoped runs, and what each failure means.
---

# Run the verify gate

`scripts/test verify` is the same gate the runner applies to a committed tree. In order, it runs:

1. `golangci-lint` (including gofmt/goimports)
2. schema-sync drift
3. doc-line budget, site gates, ADR gate and the gate harnesses
4. `go test -race` in every module, plus the patch-scoped coverage gate (≥85% of changed lines)

The order matters, because a lint failure aborts before the slow test loop.

## 1. Preflight — do all of these first

```sh
pgrep -fl 'fishhawk-runner .*--run-id' || echo no-live-runner
uptime; sysctl -n hw.ncpu
go env GOVERSION
golangci-lint version
docker info >/dev/null 2>&1 && echo docker-ok || echo docker-DOWN
git status --short
```

- **Live runner.** It may be holding the per-repository verify lock. A shell `verify` that meets a live holder **refuses immediately**. That refusal is final for that invocation: do NOT retry or poll it. Narrow to `scripts/test single -run TestX ./path/to/pkg/...` instead, or wait for the run to finish.
- **Load average above ~4× the core count.** The host is starved, and unrelated tests will flake. Use the `diagnose-red` skill (orphaned agent processes) before running anything.
- **Go newer than the 1.25 pin.** This can turn clean main red (e.g. jsonv2 under Go 1.27). Tell the user and propose `go env -w GOTOOLCHAIN=go1.25.6`. It writes their global Go env, so ask first.
- **golangci-lint.** It must be v2.x, and CI pins **v2.8.0**. A newer local binary can pass while CI's gofmt fails. Pre-check formatting with the pinned version:
  `go run github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.8.0 fmt --diff`
- **Docker down.** Backend tests need it for the shared testcontainers Postgres. Start Docker first.
- **Dirty tree.** That's allowed: patch coverage diffs merge-base → work tree, so uncommitted edits are gated too.

## 2. Run

| Situation | Command |
|---|---|
| Before a PR / final check | `scripts/test verify` |
| Iterating on known packages | `scripts/test verify --packages backend/internal/foo,runner/internal/bar` (repo-relative). It skips patch coverage, so finish with a full run |
| One test | `scripts/test single -run TestName ./backend/internal/foo/` |
| Lint only | `scripts/test lint` |

A full verify takes many minutes. Run it in the background (or with your shell tool's longest timeout) and wait for it to exit. Don't start a second concurrent verify.

## 3. Read the result

| Failure | Meaning / fix |
|---|---|
| golangci-lint / gofmt | Fix the code; `gofmt -w` / `goimports -w` on the named files |
| `Schema sync` drift | A `docs/spec/` canonical file changed without mirroring it: run `scripts/sync-schemas` and commit the mirrors |
| Doc-line budget | A `docs/ARCHITECTURE.md` line is over 1000 chars; move the prose to the package README |
| `check-adr` | Record/index mismatch: `scripts/check-adr --write-index` (see the `record-adr` skill) |
| Site gates (`check-site-voice`, `check-site-ia`, site reference drift) | Banned §5 term on the site, a sidebar/page mismatch, or a stale generated region (`scripts/gen-site-reference`) |
| `TestKnownCategoriesCoversEmittedCategories` | A new audit category: register it in `backend/internal/audit/categories.go` |
| `TestCrossModuleWireParity` | A cross-module wire struct drifted: see `backend/internal/wirecontract/README.md` |
| Patch coverage below 85% | Add tests that exercise the changed lines. Never set `FISHHAWK_SKIP_PATCH_COVERAGE` to get a pass |
| A test unrelated to the diff | Likely environmental (host load, toolchain, Docker): use the `diagnose-red` skill before touching code |
| Refused: verify lock held | See preflight. Don't retry |

A skipped harness (`zsh` or `helm` absent) is a printed skip, not a failure. Report it as an uncovered area.

## 4. Report

State pass/fail, the first failing stage, and the exact failing test or file. Paste the actual error lines; don't paraphrase them. Also mention any skips.
