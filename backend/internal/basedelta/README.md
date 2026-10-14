# basedelta

ADR-096 slice 1 (E87.1 / #4292): the base-delta independence classifier. A PR head H verified green against base B may merge onto B' (main advanced) WITHOUT a fresh verify pass only when the base delta B..B' cannot interact with the PR. `Classifier.Classify` decides that from path lists plus Go import graphs. Slice 2 (E87.2 / #4293) reads its inputs with `GitDelta` and materializes its workspaces with `MaterializeTree` (see "Git inputs"); the server's merge-candidate verdict transfer (`backend/internal/server/merge_candidate_transfer.go`) is the consumer.

## Contract

`Classify(ctx, Input{DeltaPaths, PRPaths, Workspaces}) Result{Independent, Reason}`.

- Paths are repo-relative, slash-separated and **canonical**: `path.Clean(p) == p`. `./docs/ARCHITECTURE.md`, `a//b`, `x/` are refused (approval condition C8), so a non-canonical spelling cannot slip past the anchored global-surface match.
- Callers pass **both sides of a rename** (`git diff --no-renames`): the classifier sees only path lists.
- Callers pass the B' **and** merge-candidate H' workspaces. The reach rule runs over the UNION of their graphs (see below).
- `Reason` is single-line and at most 256 bytes. Fail closed means verify: only `independent` lets a merge skip the pass.
- `SurfaceVersion` versions `GlobalSurface` + `SharedArtifactDirs`; bump it on any change so a recorded verdict names its set.

## Rule order (cheapest first; first match decides)

| # | Rule | Fires when | Reason |
|---|---|---|---|
| 0 | validation | nil classifier/loader, empty delta/PR, no or blank workspace, invalid path | `undecidable:<why>` |
| b | global surface | ANY delta OR PR path matches `GlobalSurface` (C2: both sides) | `global_surface:<path>` |
| d | shared artifact | the same path on both sides, or a delta and a PR path under one `SharedArtifactDirs` entry | `shared_artifact:<path or longest common dir>` |
| c | reach | the reverse-import closures (test imports included, full transitive fixed point) of the packages each side maps to intersect | `reach_overlap:<pkg dir>` |
| - | none fired | | `independent` |

## Reasons

| Reason | Meaning |
|---|---|
| `independent` | no rule fired |
| `global_surface:<path>` | `<path>` (delta or PR) is in the D3 set |
| `shared_artifact:<p>` | same path, or both sides under shared-artifact dir `<p>` |
| `reach_overlap:<dir>` | the package at `<dir>` is reachable from both sides |
| `undecidable:<why>` | `nil_classifier`, `no_loader`, `empty_delta`, `empty_pr_diff`, `no_workspace`, `invalid_workspace`, `invalid_path:<p>`, `load:<err>`, `load:nil_graph`, `deadline_exceeded`, `panic`, `unmapped_go_path:<p>`, `unmapped_go_mod:<p>` |

## Path mapping (rule c)

- `go.work` (any depth) → every package. A `go.mod`/`go.sum` → every package of the module rooted in its directory (else `unmapped_go_mod`). Both are also global, so rule (b) normally fires first.
- A `.go` file in a package directory → that package.
- Any other path (non-Go, or a `.go` file outside every package, e.g. under `testdata/`) → **every** ancestor directory, its own first, that is a package directory in ANY loaded graph (approval condition C3). Nearest-only would miss a directory that is a package in H but plain data under a package in B'. No ancestor: a `.go` path is `unmapped_go_path` (undecidable); a non-Go path is inert.

## Why the graph is the union of B' and H'

The issue specified "the workspace at B'". B' alone cannot see an import edge the PR itself adds (PR makes q import a; delta changes a): the B' graph has no q → a edge, so the closures look disjoint. The classifier therefore takes a list of workspaces and unions their graphs. Slice 1 named the PR head H as the second workspace; slice 2 passes the merge candidate H' (H merged with B') instead, because H' contains every import edge the PR adds, which is the only reason H was asked for. An edge that exists only in H because the delta removed its target is gone from H' — see the residual on removed packages. A brand-new PR package is not missed even with B' alone, because its `.go` paths are unmapped there (undecidable). Accepted by the operator as a conservative refinement of ADR-096 and recorded here rather than in a new ADR (ADR-098 / #4316 leaves this slice on the current conservative D3 set).

## Git inputs (`GitDelta`, `MaterializeTree`, `ExpandSymlinkAliases`; E87.2 / #4293)

`GitDelta(ctx, repoDir, candidate, priorHead, rootHead) (DeltaInputs{FromBase, ToBase, DeltaPaths, PRPaths}, error)` reads the classifier's path inputs for the merge commit a clean base advance produced, from the run's LOCAL checkout (no fetch). `rootHead` is the head whose RECORDED pass anchors a transfer chain (the prior head itself when its verdict was recorded), so a chain of individually-independent advances is re-judged as one cumulative delta against the base the real pass verified.

1. Every id must be a full lowercase 40- or 64-hex object id; anything else (`HEAD`, an abbreviation, `-x`) is refused before any git child runs.
2. `rev-list --parents -n 1 <candidate>`: exactly two parents, parent 1 == `priorHead`. `ToBase` (B') = parent 2.
3. `merge-base --is-ancestor <rootHead> <priorHead>`.
4. `merge-base --all <rootHead> <ToBase>`: exactly one base; `FromBase` = that base. Criss-cross history (two bases) is refused, never "take the first".
5. `DeltaPaths` = `diff -z --no-renames --no-ext-diff --no-textconv --no-relative --no-color --name-only FromBase ToBase`; `PRPaths` = the de-duplicated union of the same diff `FromBase rootHead` and `ToBase candidate`. The second diff carries an edit made only in the merge commit and any commit between `rootHead` and `priorHead`, which `FromBase..rootHead` alone misses (`TestGitDelta_PRPathsIncludeMergeCommitEdit`, `TestGitDelta_PRPathsIncludeCommitAfterRoot`). `--no-renames` gives both sides of a rename (the classifier contract above). `--no-relative` keeps the paths complete and root-relative when `repoDir` is a subdirectory and the repository sets `diff.relative` (`TestGitInputs_SubdirRepoDirIsRootRelativeAndComplete`). `--no-ext-diff`/`--no-textconv` are defence in depth only: `--name-only` runs no diff driver, so their removal is unobservable (declared, not tested).

`MaterializeTree(ctx, repoDir, commit, dest)` writes a commit's tracked tree into an empty `dest` from `ls-tree -r -z -l --full-tree` plus `cat-file --batch`, writing the bytes itself (`MaterializeTreeLinks` also returns the tree's symlinks, path → target). `--full-tree` keeps the tree complete when `repoDir` is a subdirectory (same test as `--no-relative`). Unlike `git archive` (`reviewsandbox.ExportTree`) it IGNORES `export-ignore` and KEEPS symlinks, runs no filter/textconv driver, and never touches the repository's index or work tree.

That is the #4293 approval condition (incomplete-graph fail-closed): an archive export drops an `export-ignore`d package, and a dropped package that imports both changed packages makes them look disjoint (`TestMaterializeTree_ExportIgnoredDependentNeverIndependent` shows the archive export classifying such a fixture `independent` and the materialized trees classifying it `reach_overlap:c`).

Every write goes through an `os.Root` on `dest`, and before each directory or entry is created every existing parent component is `Lstat`ed: a symlink component is refused (`ErrIncompleteTree`), so nothing is ever created THROUGH a link — not one planted in `dest` (`TestWriteBatch_PlantedSymlinkParentNeverResolved`, any filesystem), and not one a case-folding filesystem aliases, where entries `A` → `/outside` and `a/child` would otherwise resolve `a` through `A` (`TestMaterializeTree_CaseFoldTwoSymlinksNeverEscape`, skipped with a printed reason on a case-sensitive filesystem). Regular files are created `O_EXCL` and symlinks only after the last file, so a case-folding collision fails instead of overwriting a file. The entry count and the total blob bytes, symlink blobs included, are checked against `reviewsandbox.DefaultLimits` (50000 entries, 512 MiB) from the `ls-tree -l` sizes BEFORE anything is written; an oversized tree is `ErrIncompleteTree` with nothing written (`TestMaterializeTree_LimitsRefuseBeforeWriting`). A tree entry that is not a regular file or symlink (a submodule gitlink) or a path with an empty, `.`, `..` or `.git` component makes the workspace incomplete or unsafe: refused, never classified.

`ExpandSymlinkAliases(in, trees...)` resolves every symlink of the given trees (chained, component by component, as the filesystem would) and appends the LINK path to `DeltaPaths` / `PRPaths` when one of their paths equals or lies under its resolved target: the classifier sees only paths, so an edit to `shared/impl.txt` behind `p/impl.go -> ../shared/impl.txt` must also read as a change to `p`. A link resolving outside the repository (an absolute target, a `..` above the root, a loop past 40 hops) is `ErrSymlinkEscape`, so the decision is undecidable.

Every git child is `git -C <repoDir>`, local-only, `procgroup.Harden`ed under `DefaultGitTimeout` (30s) per child, with the env scrubbed to `reviewsandbox.BaseAllow` (no inherited `GIT_DIR`, `GIT_INDEX_FILE` or credential) plus `GIT_TERMINAL_PROMPT=0`, `GIT_CONFIG_NOSYSTEM=1`, `GIT_CONFIG_GLOBAL=/dev/null`, `GIT_NO_REPLACE_OBJECTS=1` (a local `refs/replace` cannot forge parents or the delta; `TestGitDelta_IgnoresReplaceRefs`) and `GIT_OPTIONAL_LOCKS=0`. A failure wraps a typed sentinel; `InputsErrorCode` maps it to a closed code:

| Sentinel | Code | Test |
|---|---|---|
| caller context deadline | `deadline_exceeded` | `TestGitDelta_ContextDeadline` |
| `ErrInvalidObjectID` | `invalid_object_id` | `TestGitDelta_NonSHAInputsRefused` |
| `ErrUnexpectedParents` (single parent, octopus, parent 1 != prior head) | `unexpected_parents` | `TestGitDelta_UnexpectedParents` |
| `ErrNotAncestor` | `not_ancestor` | `TestGitDelta_RootNotAncestor` |
| `ErrAmbiguousMergeBase` | `ambiguous_merge_base` | `TestGitDelta_AmbiguousMergeBase` |
| `ErrNoMergeBase` | `no_merge_base` | `TestGitDelta_NoMergeBase` |
| `ErrIncompleteTree` (gitlink, non-file mode, case-fold collision, symlink parent component, over the entry or byte limit) | `incomplete_tree` | `TestMaterializeTree_SubmoduleIncomplete`, `TestMaterializeTree_CaseFoldFileCollisionFails`, `TestMaterializeTree_CaseFoldSymlinkNeverEscapes`, `TestMaterializeTree_CaseFoldTwoSymlinksNeverEscape`, `TestWriteBatch_PlantedSymlinkParentNeverResolved`, `TestMaterializeTree_LimitsRefuseBeforeWriting` |
| `ErrSymlinkEscape` | `symlink_escape` | `TestExpandSymlinkAliases` |
| `ErrUnsafeTreePath` | `unsafe_tree_path` | `TestCheckTreePath`, `TestParseLsTree_Refusals` |
| `ErrGit` (non-zero exit, timeout, malformed output; single-line stderr tail of at most 256 bytes) | `git` | `TestGitDelta_GitFailureNamesBoundedStderr`, `TestMaterializeTree_GitFailure` |

## Loader (`GoListLoader`)

1. `filepath.EvalSymlinks` on the workspace (macOS `/var` → `/private/var`).
2. go.work present → `go work edit -json <abs go.work>` for the use list; else a root go.mod → `.`; else an EMPTY graph (a non-Go repository).
3. ONE `go list -e -json=<fields> ./<use>/...` in the HOST build context.
4. A go/parser `ImportsOnly` pass over EVERY `.go` file of every use module (walked like `./...`: no `testdata`, `vendor`, `.`/`_` names, nested modules), ignoring build constraints. This puts an OS-tagged file's imports (`x_windows.go` on a linux host) into the graph and discovers a package whose files are all host-excluded, which `./...` skips silently. Parsing needs no module source, so the loader never requires a non-host-platform third-party module in GOMODCACHE: CI's cache is linux-only under `GOPROXY=off` (approval condition C1; the plan's per-platform `go list` loads were dropped for this reason, since a windows load would need go-winio, go-ansiterm, go-ole, wmi and purego via pgtest).
5. Edges are restricted to workspace packages; stdlib and third-party edges change only through go.mod/go.sum, which are global.

Every go command runs with `cmd.Dir` = workspace, `procgroup.Harden` (a timeout kills the process group), and a pinned env appended LAST: `GOWORK=<abs go.work>|off` (both the `go work edit` and the `go list` call; C7), `GOPROXY=off`, `GOTOOLCHAIN=local` (a toolchain mismatch fails with a clear error instead of downloading; C7), `GOFLAGS=` and `CGO_ENABLED=0`. `Load` is bounded by `Timeout` (default 2m); `Classify` is bounded by ONE `Deadline` (default 5m) covering every workspace's Load (C5).

## Fail-closed table

| Mode | Reason / error | Test |
|---|---|---|
| empty delta / PR diff | `undecidable:empty_delta` / `empty_pr_diff` | `TestClassify_Table` |
| absolute, escaping, empty, backslash, non-canonical, control-char path | `undecidable:invalid_path:<p>` | `TestClassify_Table`, `TestClassify_NonCanonicalGlobalPathNeverIndependent`, `TestClassify_GuardInputs` |
| nil classifier / loader, no or blank workspace | `undecidable:nil_classifier` / `no_loader` / `no_workspace` / `invalid_workspace` | `TestClassify_GuardInputs` |
| loader error, nil graph | `undecidable:load:<err>` / `load:nil_graph` | `TestClassify_LoaderError`, `TestClassify_NilGraph`, `TestGoListLoader_MissingWorkspaceImport` |
| overall deadline exceeded | `undecidable:deadline_exceeded` | `TestClassify_OneDeadlineCoversAllLoads` |
| panic below Classify | `undecidable:panic` | `TestClassify_Panic` |
| unmapped `.go` path / go.mod | `undecidable:unmapped_go_path` / `unmapped_go_mod` | `TestClassify_Table`, `TestPackagesFor` |
| `go` exits non-zero | error with stderr tail | `TestGoListLoader_NonZeroExit` |
| malformed `go list` / `go work edit` output; bad use entry | error | `TestGoListLoader_MalformedOutput`, `TestGoListLoader_MalformedWorkEdit` |
| package `Error`, `DepsErrors`, `Incomplete`, no module, dir outside workspace, zero packages | error | `TestGoListLoader_PackageFailureFields`, `TestGoListLoader_ZeroPackages` |
| a go.work module with no host-listed package (all files build-excluded, or none) beside a listing one | `module <m>: go list reported no packages for it` | `TestGoListLoader_ModuleWithNoListedPackages` |
| context done between `go list` and a module's go/parser pass | error wrapping the ctx error | `TestBuildGraph_ContextDoneBeforeParse` |
| host-excluded file importing a non-package workspace path; unparsable file | error | `TestGoListLoader_UnresolvedImportInExcludedFile`, `TestGoListLoader_UnparsableFile` |
| wedged `go` | error at `Timeout` | `TestGoListLoader_Timeout` |
| inherited `GOWORK` / `GOTOOLCHAIN` | ignored / refused locally | `TestGoListLoader_GOWORKPinned`, `TestGoListLoader_ToolchainMismatchFailsLocal` |

The `go work edit -json <abs go.work>` call is pinned twice: the explicit file argument and the env `GOWORK`. Either alone keeps it on the checkout's go.work, so the env pin there is deliberately redundant and `TestGoListLoader_GOWORKPinned` goes RED only when BOTH are removed (or when the `go list` env pin is).

## Global surface set

`GlobalSurface` (`surface.go`) is the ADR-096 D3 set. Patterns are doublestar, matched against the whole path (root-anchored: `testdata/**` does not match `backend/internal/x/testdata/y.json`). `TestGlobalSurfaceMatchesREADME` keeps this table and the var in sync both ways; `TestGlobalSurfaceKeepsADR096D3Floor` hard-codes the ADR-096 D3 enumeration (the first fifteen rows), so a floor pattern cannot be dropped from the var and this table together.

| Pattern | Leg / rationale |
|---|---|
| `go.work` | every Go leg (lint, test loops, release-ldflags builds), `_verify_scope_buckets`, check-gate-image |
| `**/go.mod` | every Go leg; check-gate-image reads `*/go.mod` go directives |
| `**/go.sum` | every Go leg (module checksums) |
| `scripts/**` | every leg and harness is a script here; check-pipefail-grep scans `scripts/*` |
| `.golangci.yml` | `cmd_lint` |
| `deploy/gate-image/**` | check-gate-image, test-in-gate-image read the Dockerfile ARG pins |
| `.fishhawk/**` | the workflow spec read by the product and its spec tests (#4292) |
| `docs/spec/**` | sync-schemas sources; test-site-reference (docgen) reads the schemas and examples |
| `docs/api/**` | test-site-reference (docgen) reads `v0.openapi.yaml` |
| `site/**` | check-site-voice, check-site-ia, test-site-reference rewrites the reference pages |
| `docs/adr/**` | check-adr |
| `docs/ARCHITECTURE.md` | `_verify_doc_line_budget` |
| `backend/internal/postgres/migrations/**` | migration numbering is a whole-tree invariant (#4292) |
| `backend/internal/audit/categories.go` | the audit-category registry checked against every emitter (#4292) |
| `testdata/**` | root cross-module parity fixtures read by tests outside the dir (widened from `testdata/wire/**`); test-dev reads `testdata/wire/restart_blockers.json` |
| `deploy/helm/**` | test-helm-render renders `deploy/helm/fishhawk` |
| `backend/cmd/fishhawkd/**` | test-helm-render greps fishhawkd's source as data |
| `backend/Dockerfile` | test-dev checks TARGETARCH and the release-ldflags stanza |
| `Makefile` | test-dev greps the s3-init target |
| `.github/workflows/**` | check-gate-image reads go-version and golangci-lint pins |
| `cli/internal/docgen/**` | test-site-reference regenerates the site reference with it |
| `cli/internal/cmdinfo/**` | test-site-reference (the CLI page is rendered from cmdinfo) |
| `cli/cmd/fishhawk-docgen/**` | test-site-reference runs `go run ./cmd/fishhawk-docgen` (C6) |
| `backend/internal/version/**` | test-release-ldflags and test-dev read `version.go` as data |
| `cli/internal/version/**` | test-release-ldflags and test-dev read `version.go` as data |
| `runner/internal/version/**` | test-release-ldflags and test-dev read `version.go` as data |
| `backend/internal/plan/schemas/**` | sync-schemas mirror |
| `backend/internal/spec/schemas/**` | sync-schemas mirror |
| `backend/internal/crewmessage/schemas/**` | sync-schemas mirror |
| `backend/internal/specialistjob/schemas/**` | sync-schemas mirror |
| `backend/internal/operatorrole/schemas/**` | sync-schemas mirror |
| `backend/internal/workmgmt/schemas/**` | sync-schemas mirror |
| `backend/internal/operatorrole/defaults/**` | sync-schemas mirror |
| `backend/internal/workmgmt/defaults/**` | sync-schemas mirror |
| `backend/internal/spec/presets/**` | sync-schemas mirror |
| `cli/internal/spec/schemas/**` | sync-schemas mirror |
| `cli/internal/spec/presets/**` | sync-schemas mirror |
| `runner/internal/plan/schemas/**` | sync-schemas mirror |
| `backend/internal/spec/testdata/predicate-fixtures.json` | sync-schemas mirror |
| `backend/internal/spec/testdata/review-conventions-fixtures.json` | sync-schemas mirror |
| `backend/internal/spec/testdata/tier-expansion-fixtures.json` | sync-schemas mirror |
| `cli/internal/spec/testdata/predicate-fixtures.json` | sync-schemas mirror |
| `cli/internal/spec/testdata/review-conventions-fixtures.json` | sync-schemas mirror |
| `cli/internal/spec/testdata/tier-expansion-fixtures.json` | sync-schemas mirror |
| `backend/internal/server/testdata/acceptance-verdict-fixtures.json` | sync-schemas mirror |
| `runner/cmd/fishhawk-runner/testdata/acceptance-verdict-fixtures.json` | sync-schemas mirror |

Throughput cost, accepted as conservative ("fail closed means verify"): a delta touching fishhawkd's main package, docgen/cmdinfo, the version packages or a schema mirror always forces a pass.

## Shared-artifact dirs

`docs/`, `frontend/`, `infra/`, `acceptance/`, `auth/`, `.github/`, `.agents/`, `.claude/`, `.githooks/`, `deploy/`: non-Go trees whose consumers the import graph cannot see. A delta path and a PR path under one entry are dependent; the reason names their longest common directory, never shorter than the entry.

## Adding a verify leg

`verifylegs_test.go` parses `scripts/test`. Every non-blank, non-comment line of `cmd_verify` and `_verify_gate_harnesses` must be a call of a leg registered in `realLegTable` or an exact allow-listed plumbing line; leg tokens are word-anchored (`xcmd_lint` never yields `cmd_lint`, `my_verify_lock` never `_verify_lock`; `TestLegTokenWordAnchored`). Each registered `_verify_*` body's `$ROOT/scripts/<x>` / `$SCRIPTS_DIR/<x>` invocations must equal its row's `scripts`. Each row's inputs are matched against the repository: a `global` input must match at least one file and every match must satisfy `MatchGlobal`; a `goReach` input (compile/lint/test consumption only) may match only `.go` files. `TestSchemaSyncDestinationsAreGlobal` requires every `scripts/sync-schemas` mirror destination to be global. Floors (>= 8 legs incl. `cmd_lint` + `_verify_gate_harnesses`, >= 12 harnesses, >= 10 sync destinations) turn a parser collapse RED.

So: a new leg, harness, inline script call or mirror RED-lines verify until it is registered and its inputs are covered by `GlobalSurface` (bump `SurfaceVersion` and add the README row).

## Residuals

- A harness that starts reading a new non-global path is not caught (only new legs, lines and mirrors are). Partly mitigated: editing a harness touches `scripts/**`, which is global.
- Parsing is textual (like `scripts/check-site-ia`); an indirect leg call through a variable is caught only as an unclassified line.
- A CUSTOM build tag is visible (the go/parser pass ignores constraints), but a file excluded by name convention (`_x.go`, `.x.go`) or under `testdata`/`vendor` is not a package file, matching `go`'s own rules.
- Go tests reading OTHER packages' source as data (`wirecontract`'s go/ast parity scan, the `agentpaths`/`constraint` twins, `testanchor`'s repo-wide scan, the audit category-emission scan) are invisible to the import graph. Most check a per-file property, which cannot interact across sides; the pairwise ones force each side to edit both members, so reach overlaps. A red main traced to one is a D3/reach coverage bug per ADR-096.
- ADR-096's stated residuals (DB runtime coupling, reflection / `go:linkname`, global state) stand.
- A delta that REMOVES a package the PR imports makes H' fail `go list` (the import no longer resolves), so the verdict is `undecidable:load:…`, not `reach_overlap`. Fail closed and deliberate: either way the pass runs.
- The case-folding guards (`O_EXCL`, symlinks last, the parent `Lstat` walk) are exercised only on a case-insensitive filesystem (macOS dev hosts); on a case-sensitive host the colliding entries are distinct files, the two-symlink test skips with a printed reason, and the planted-symlink test pins the parent walk on any filesystem.
- `MaterializeTree` writes a symlink's target verbatim, so a symlink may point outside the workspace. `go list ./...` and the loader's go/parser walk do not follow directory symlinks, and the loader refuses a package directory outside the workspace. `ExpandSymlinkAliases` makes any such link fail closed (`ErrSymlinkEscape`) before classification.
- Alias resolution is per tree and case-sensitive: on a case-folding filesystem a link target differing from a changed path only in case is not matched.
- `GOPROXY=off` means a host whose module cache lacks a HOST-platform dependency gets `undecidable` (verify), never a fetch. The runner gate's cache holds every host-platform module because the verify test loop builds the same workspace in the same environment; non-host-platform modules are never needed (C1). `TestGoListLoader_RealRepository` fails if the real workspace does not load.
