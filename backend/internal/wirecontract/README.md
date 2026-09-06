# wirecontract

A repo-level guard for the cross-module JSON wire contracts #2558 names: structs
that must agree on the wire but live in two Go modules (`backend` and `runner`)
which cannot import one another, so no compiler enforces their agreement. The
guard parses each side's declared struct with `go/ast` and asserts their json
tags agree. It runs as an ordinary Go test (`TestCrossModuleWireParity`,
`TestManifestCompleteness`), so `scripts/test verify`'s per-module
`go test -race ./...` loop already executes it — drift fails IN-LOOP with no
shell wiring, because the package sits inside the already-registered `backend`
`go.work` module.

## What it does NOT do

It proves TAG parity (the wire keys agree), not SEMANTIC agreement (the values
mean the same thing). The stronger fix — a shared wire MODULE both sides import,
making the contract COMPILE-enforced — is deliberately deferred (#2558); its
blast radius (`promptResponse` alone has ~140 references) is far past this
change's scope. The guard protects the interim: once a contract moves into a
shared module its manifest row is deleted, so the manifest shrinks to zero as
the module absorbs the contracts — a legible migration signal.

## Manifest (`manifest.go`)

`SeedManifest()` is the declarative seed. Each `Pair` names an EMITTER endpoint
(marshals the bytes) and a CONSUMER endpoint (decodes them), plus a comparison
`Mode`:

- **`ModeExact`** — the sorted `(json name, options)` tuple lists must be EQUAL.
  Used for the six leaf pairs that genuinely round-trip both ways (`scopeFile`,
  `bindingAssertion`, `scopeExemption`, `diffCoverageConfig`, `fixupApplyPatch`,
  `unsatisfiedAssertion`↔`BindingAssertionReport`), so options — including
  `omitempty` — must match.
- **`ModeSubset`** — every CONSUMER json name must exist on the EMITTER (names).
  Used for `promptResponse`→`FetchedPrompt` and for the backend's
  `pullRequestBody` UNION against each of the three runner ship bodies
  (`pullRequestScopeParkBody`, `pullRequestFailureBody`,
  `pullRequestChildPushBody`).

A consumer name the emitter never emits is ALWAYS a violation in both modes —
that is the #2558 failure shape (a backend rename leaving the runner decoding a
key nobody sends).

### Why subset mode compares NAMES not full tags

`omitempty` is a MARSHAL-side-only directive (encoding/json:
["the field is omitted from the encoding if the field has an empty value"](https://pkg.go.dev/encoding/json#Marshal)),
and in each subset pair only ONE side marshals, so an `omitempty` difference is
invisible on the wire. Requiring it to match would be RED ON ARRIVAL today for a
non-wire reason (`outcome` vs `outcome,omitempty`; `title` vs `title,omitempty`)
and would force cosmetic churn on production structs — and a guard that must be
weakened on arrival teaches people to weaken it later. The deliberate asymmetry
is PAIRED-TESTED: `TestCheck_SubsetModeIgnoresOptionDrift` (tolerated) against
`TestCheck_ExactModeRejectsOptionDrift` (rejected), so a future author who
disagrees changes a test rather than discovering an undocumented hole.

### But subset mode FAILS CLOSED on any OTHER option (condition 3)

Ignoring `omitempty` is safe; silently ignoring an option that changes the
encoded FORM is not. `,string` is the clear example — it quotes the encoded
number, so a subset pair that gained `,string` on one side would DIVERGE on the
wire while a names-only check stayed green. That is the exact shape of #2558
itself, so this guard does not repeat it: in subset mode, on a field that
round-trips (a consumer field or the emitter field it matches), ANY tag option
OTHER than `omitempty` is a FAILURE naming the field and the option. An option
the guard has not analysed is loud, not invisible. Pinned by
`TestCheck_SubsetModeRejectsStringOptionOnEmitter` /
`...OnConsumer`. To handle a new option deliberately, widen `checkSubset` (or
move the pair to `ModeExact`) — do not silence the check.

## Manifest-completeness sweep

`Check` also sweeps the declared `CoveredFiles` for every struct whose doc
comment OR a field comment carries the literal marker `CROSS-MODULE WIRE
CONTRACT`, and requires each to appear as a manifest `Pair` endpoint or in
`UnpairedExemptions`. A NEW duplicated contract that copies the marker comment
but skips the manifest fails verify instead of joining the unguarded population.
`UnpairedExemptions` currently holds `upload.ShipPlanArgs`, whose marker is about
`reachability.Result`'s tags (pinned separately by `upload_test.go`) rather than
the args struct's own wire shape.

## Fail-closed table

Every resolution failure is a returned error, never a silent skip — a guard that
can silently cover nothing is the vacuity class #2558 exists to close.

| Condition | Behavior | Test |
|---|---|---|
| no `go.work` above the start dir | `RepoRoot` returns a named error | `TestRepoRoot_NoGoWorkFailsClosed` |
| endpoint names a nonexistent file | error naming the file | `TestCheck_MissingFileFailsClosed` |
| endpoint names an absent type | error `type "X" not found` | `TestCheck_MissingTypeFailsClosed` |
| type resolves to a non-struct | error `is not a struct` | `TestCheck_NonStructTypeFailsClosed` |
| embedded type unresolvable in the same file | error naming it | `TestExtract_UnresolvableEmbeddedFailsClosed` |
| an endpoint file does not parse | error naming the parse failure | `TestExtract_MalformedSourceFailsClosed` |
| a `CoveredFiles` entry does not parse | error naming the parse failure | `TestCompleteness_MalformedSourceFailsClosed` |

## Whole-repo-tree requirement (condition 1)

This package AND the golden tests it anchors (`prompt_test.go`,
`scope_completeness_test.go`, `main_test.go`) REQUIRE the full repository tree at
test time: `RepoRoot` walks up to the `go.work` marker, `Check` reads a SIBLING
module's (`runner`'s) source by path, and the golden tests read the repo-root
fixture `testdata/wire/exempt_prompt_fields.json`. This is DELIBERATE and
FAIL-CLOSED, not skippable. Every place these tests execute today has the full
tree — CI routes the Go gate through `scripts/test lint`/`coverage` from the
REPO ROOT (the only `working-directory:` in `ci.yml` is `frontend`), and
`backend/Dockerfile` runs no `go test`. A FUTURE module-scoped test job, a
partial checkout, or a vendored build that runs these packages' tests WITHOUT
the whole workspace would go red by design and must account for it (run from the
repo root with the full tree present).

## Adding a contract

1. Give the emitter and consumer structs a doc/field comment carrying the
   `CROSS-MODULE WIRE CONTRACT` marker (the convention already in use).
2. Add a `Pair` row to `SeedManifest()` naming both endpoints and the mode.

The completeness sweep raises the floor — a contract that follows the repo's
documented marker convention cannot skip the manifest — without claiming to
catch a contract that follows NO convention.

## Residuals (stated, not stronger)

- **TAG parity, not compile enforcement.** Only a shared wire module makes a
  contract structurally impossible to drift; see #2558.
- **The sweep keys on the marker string.** An author who adds a duplicated
  struct WITHOUT the `CROSS-MODULE WIRE CONTRACT` comment is not caught. The
  sweep covers only the declared `CoveredFiles`; marker-bearing CONSTANTS and
  non-struct values are out of scope.
- **The repo-root walk is duplicated across the two modules' golden test files.**
  A drift there produces a LOUD file-not-found `t.Fatalf`, never a silent pass,
  so it is not the failure class this guard addresses.
