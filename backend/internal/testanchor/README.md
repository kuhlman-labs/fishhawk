# backend/internal/testanchor

Repo-level guard that keeps Go test fixtures path-independent (E83.72 / #4179).
`TestNoRuntimeCallerFileAnchorsInTests` parses every `_test.go` file in every
`go.work` module with `go/ast` and fails on any `runtime.Caller` call whose FILE
result is used. It runs in-loop via `go test -race ./...` inside
`scripts/test verify`, with no shell wiring (the `wirecontract` precedent:
production code consumed only by its own test).

## The rule

Anchor every source-tree fixture on the package's init-captured source dir,
declared once per test package:

```go
var pkgSrcDir = mustGetwd()

func mustGetwd() string {
	dir, err := os.Getwd()
	if err != nil {
		panic(fmt.Sprintf("pkgSrcDir: os.Getwd: %v", err))
	}
	return dir
}
```

then derive paths with the same parent hops the file anchor used
(`filepath.Join(pkgSrcDir, "..", "..", "..", "testdata", "wire", "x.json")`).

Why this and not `runtime.Caller(0)`:

- **`-trimpath`.** Under `-trimpath` the recorded file name is module-relative
  (`go help build`: "recorded file names will begin either a module
  path@version … or a plain import path"), so a path derived from
  `runtime.Caller`'s file resolves against the package cwd and misses every
  fixture. On `origin/main` before #4179 a `-trimpath` leg failed 86 tests.
- **`go test` runs the binary from the package source dir** (`go help
  testflag`), so `os.Getwd()` at package init IS that dir.
- **Package-level variables initialize before `TestMain`**, so the value is
  captured before any `TestMain` or test changes the process cwd
  (`runner/cmd/fishhawk-runner`'s `TestMain` chdirs into a throwaway repo;
  `t.Chdir` in a test is equally harmless).

Residual: a compiled test binary executed directly from another directory
(`go test -c`, then run elsewhere) captures the wrong dir. `go help testflag`
states the same requirement for any test reading `testdata/`.

## What is allowed

Exactly one form: a `runtime.Caller` call that is the SOLE right-hand side of a
four-value assignment or `var` spec whose SECOND target (the file) is the blank
identifier:

```go
if pc, _, _, ok := runtime.Caller(1); ok { … }   // allowed: reads a function name
```

Every other `runtime.Caller` reference is a finding: a bound file result
(`_, f, _, _ := runtime.Caller(0)`, also at package level), and any context the
guard cannot prove discards the file — `return runtime.Caller(0)`, a call passed
as an argument, a method value (`f := runtime.Caller`). The import is resolved
per file: default `runtime`, an alias (`rt "runtime"` → `rt.Caller`), or a dot
import (bare `Caller(…)`); a blank `_ "runtime"` import binds nothing.

## go.work parsing (approval condition 3)

`Modules` reads `go.work` line by line. Each line has any trailing `//` comment
removed and is trimmed, so comment-only lines (`// use ./disabled`, the prose
header of the repo's own `go.work`) vanish. Both the single-line `use ./x` and
the block `use ( … )` forms are read; the `use (` opener and `)` closer are
delimiters, not entries, and entries of any OTHER block (`replace ( … )`) are
ignored. Each `use` entry is normalized by `normalizeUse`: unquote a quoted
path, strip a leading `./`, `filepath.Clean`, then slash form — so `./backend`,
`backend/` and `"./backend"` all become `backend`. `RequireModules` normalizes
the names it is given the same way and matches on the cleaned path.

## Scan walk

Per module: directories the Go tool ignores (`testdata`, `vendor`,
`node_modules`, names starting with `.` or `_`) and files starting with `.` or
`_` are skipped; a nested directory that is itself a `go.work` module is skipped
and scanned under its own entry, so per-module counts attribute correctly.
Non-test `.go` files are not scanned.

## Fail-closed list

Every resolution failure is an error, never an empty result:

| Failure | Function | Test |
|---|---|---|
| No `go.work` at or above the start | `RepoRoot` | `TestRepoRootFailsClosedWithoutGoWork` |
| `go.work` unreadable, or zero `use` entries | `Modules` | `TestModulesFailsClosedOnEmptyGoWork` |
| A `use` path that is not an existing directory | `Modules` | `TestModulesFailsClosedOnMissingModuleDir` |
| A `_test.go` that does not parse | `Scan` | `TestScanFailsClosedOnUnparseableTestFile` |
| A named module scanned zero `_test.go` files, is not a `go.work` module, or no names given | `Result.RequireModules` | `TestRequireModulesFailsOnUnscannedModule` |

The real-tree test also requires `backend`, `runner`, `cli` and `verifier` to
have scanned test files, and `AllowedCalls >= 1` (today
`backend/internal/server/approvals_test.go`'s `Caller(1)`), so a scan that never
reaches a `runtime.Caller` call cannot pass green. `TestScanFlagsSeededFileAnchors`
pins the detector itself over a seeded tree, because the real tree carries no
positives.

Like `wirecontract`, this package REQUIRES the full repository tree at test
time: a partial checkout or vendored build fails it by design.

## What it does NOT catch

- `runtime.Callers` + `runtime.CallersFrames`, and `runtime.FuncForPC(pc).FileLine`
  — other routes to a source file name.
- A dot-imported `Caller` used as a method value (`f := Caller`); only dot-import
  CALLS are seen.
- A local identifier shadowing the `runtime` import name is treated as the
  import (a false positive, fail-closed).
- Non-test code. The rule is about test fixtures; production code must not
  read source paths at all.
