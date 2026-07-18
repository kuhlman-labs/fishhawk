# reachability

Symbol-reachability engine that validates the phase partition of a plan's
`split_proposal` against the compiler's view of the working tree (#2056, E50.4;
parent #2008). Runner-side only — the runner is the sole component with the
checked-out Go source; `fishhawkd` has no on-disk checkout.

## What it answers

For each phase of a `split_proposal`, one question: **do the files the phase
declares match the files reachability says belong together to keep the phase
compile-atomic?** A phase whose symbols leak into a sibling phase would produce
a non-compiling intermediate when the phases ship one at a time.

It is **advisory** and **fail-open**. It never blocks or transitions the plan
stage. On any doubt it returns `Result{Available:false}` and the caller drops
the advisory.

## Not a grep

The engine loads the working tree through `go/packages` + `go/types` (the real
compiler front end) and reasons over the resolved type graph. It detects a
cross-boundary use site even when the source shares **no textual token** with
the defining symbol:

- a **dot-imported** or **aliased** construction (`Widget{…}` with no package
  qualifier),
- an interface implemented across a package boundary by a type that never names
  the interface,
- a test fake reading a struct field.

A name grep keyed on `<pkg>.<Symbol>` misses every one of those; and with two
identically-named types in different packages a grep cannot say which phase a
use belongs to. Type resolution can. `TestAnalyze_NotAGrep` pins this with a
same-named decoy type the engine must **not** attribute the use to.

## The three cross-boundary kinds

Each violation pairs a defining symbol (in one phase) with a use site (in a
different phase):

| Kind | Detected via | Breaks when |
|---|---|---|
| `construction_site` | composite literal `T{…}` whose named struct `T` is defined in another phase (`types.Info.Types`) | `T`'s fields change |
| `interface_implementer` | a concrete named type that structurally implements (`types.Implements`, value or pointer receiver) an interface with ≥1 method defined in another phase | the interface's method set changes |
| `test_fake_field_reader` | a field selection `x.F` inside a `_test.go` file whose field `F`'s struct is defined in another phase (`types.Info.Selections`, `FieldVal`) | `F` is renamed or removed |

Interface matching is **global** across all loaded packages, because the common
case is an interface in one phase's package satisfied by a concrete type in
another phase's package — it cannot be scoped to a single package's scope.

## Fail-open contract

`packages.Load` returns a **nil error even when an individual package fails to
parse or type-check** — the failures land only in each package's `.Errors`
slice. Computing over a broken or incomplete type graph would produce garbage,
so `Analyze`:

1. walks **every** loaded package via `packages.Visit` — including the synthetic
   `*.test` variants that `Tests:true` produces and every dependency — and skips
   the whole advisory if **any** has a non-empty `.Errors`
   (`TestAnalyze_PerPackageError_FailOpen`, the operator's load-bearing case);
2. skips on a **load-level** error or a source root with no loadable packages
   (`TestAnalyze_LoadError_FailOpen`);
3. skips on a **missing or degenerate** split proposal — no phases, or phases
   that declare no files (`TestAnalyze_MissingSplitProposal_Skip`).

`Analyze` never returns an error and never panics on malformed input; a caller
can always publish or drop the `Result` unconditionally.

## Per-phase counts

`PhaseResult` carries `DeclaredCount` (files the phase declares) and
`DerivedCount` (declared files plus any sibling-phase file that references a
symbol this phase defines through one of the three kinds). `DerivedCount >
DeclaredCount` is the discrepancy signal the plan gate surfaces. A clean
partition has them equal (`TestAnalyze_CleanPartition`).

Create-only phase files do not yet exist in the working tree, so they
contribute no definitions and no use sites — they are counted toward the
declared total but are naturally absent from the loaded type graph (the
repartitioning-of-existing-code case, #1855;
`TestAnalyze_CreateOnlyFile_NoDefinitions`).

## `Result` is the wire contract

The runner ships `Result` to the server as JSON on plan upload; the backend
cannot import this package across the module boundary, so it owns a mirroring
decode struct. The JSON tags here are **load-bearing** — a tag drift between the
two structs fails the advisory open silently. `TestResult_JSONWireKeys` locks
the exact wire keys at the source; slice 2 adds the runner→server→`get_plan`
round-trip test.

## Caller responsibilities (slice 2)

This package has no production caller yet. The runner plan stage wires it in:
build `[]Phase` from `plan.SplitProposal.Phases` (repo-relative
`scope.files`), call `Analyze(phases, RepoDir)`, and on `Available:false` **or**
any surprising output log and continue — never fail the stage. For a
multi-module repo with a committed `go.work` at `RepoDir`, `packages.Load` with
`"./..."` loads every workspace module and the repo-relative phase paths map
via `filepath.Rel(RepoDir, …)`.
