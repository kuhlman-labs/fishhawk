# basedelta

ADR-096 slice 1 (E87.1 / #4292): the base-delta independence classifier. A PR head H verified green against base B may merge onto B' (main advanced) WITHOUT a fresh verify pass only when the base delta B..B' cannot interact with the PR. `Classifier.Classify` decides that from path lists, the repository's DECLARED verdict-transfer surface (ADR-124 / #4657, amending ADR-098) and the workspaces' import graphs. Slice 2 (E87.2 / #4293) reads its inputs with `GitDelta` and materializes its workspaces with `MaterializeTree` (see "Git inputs"); the server's merge-candidate verdict transfer (`backend/internal/server/merge_candidate_transfer.go`) is the consumer.

## Contract

`Classify(ctx, Input{DeltaPaths, PRPaths, Workspaces, Surface}) Result{Independent, Reason}`.

- `Surface` is the repository's declared surface (`surface.go`): the workflow-v2 top-level `verdict_transfer` block, resolved from the run's cached ADMISSION spec by `ResolveSurface`, the ONE production resolver (nil bytes, a spec with no block, or a v0/v1 document → `nil`, undeclared; an unparseable spec → an error, which the server treats as undeclared). A nil `Surface` is refused. Every `Surface` method is nil-safe.
- Paths are repo-relative, slash-separated and **canonical**: `path.Clean(p) == p`. `./docs/ARCHITECTURE.md`, `a//b`, `x/` are refused (approval condition C8), so a non-canonical spelling cannot slip past the anchored G match.
- Callers pass **both sides of a rename** (`git diff --no-renames`): the classifier sees only path lists.
- Callers pass the B' **and** merge-candidate H' workspaces. The reach rule runs over the UNION of their graphs (see below).
- `Reason` is single-line and at most 256 bytes. Fail closed means verify: only `independent` lets a merge skip the pass.
- `ClassifierVersion` versions the rule set and its refusal codes; `Surface.Digest()` (`sha256:<hex>` over every class, each sorted, plus `completeness_check`; `""` for nil, so undeclared and declared-empty differ) identifies the declaration. Together they replace the deleted built-in `SurfaceVersion` as the identity a recorded verdict names (ADR-124 D1). The product carries NO built-in pattern, directory or version of any repository's layout (ADR-110, ADR-124 D6): `GlobalSurface`, `SharedArtifactDirs`, `MatchGlobal` (the package func) and `SurfaceVersion` are gone.

## Rule order (cheapest first; first match decides)

| # | Rule | Fires when | Reason |
|---|---|---|---|
| s | surface guards | `Surface` nil; any entry malformed; no G entry | `undecidable:surface_undeclared` / `invalid_surface:<entry>` / `surface_incomplete` |
| 0 | validation | nil classifier/loader, empty delta/PR, no or blank workspace, invalid path | `undecidable:<why>` |
| w | workspace adapter | `Loader.Supports` refuses ANY workspace (today: no `go.work` and no `go.mod` at its ROOT; a nested module is not enough) — decided before every path rule, so it wins over a G match | `undecidable:unsupported_language` |
| b | G (verdict-wide) | ANY delta OR PR path is `.fishhawk/workflows.yaml` (always, self-covering, ADR-124 D5) or matches a declared `global_surface` pattern | `global_surface:<path>` |
| D | doc-check | ANY delta OR PR path matches `doc_check_inputs` (a conservative superset until the D-i/D-ii relaxation, #4317; ADR-098 D5.1) | `doc_check:<path>` |
| o | opaque | ANY delta OR PR path matches `opaque_paths` | `undecidable:opaque_path:<path>` |
| d | shared artifact | the same path on both sides, or a delta and a PR path under ANY one declared `shared_artifact_dirs` entry (every entry evaluated, so declaration order never matters; inert never exempts) | `shared_artifact:<path or longest common dir>` |
| l | load | `Loader.Load` per workspace under ONE deadline | `undecidable:load:<err>` / `unsupported_language` / `deadline_exceeded` |
| p | per-path classification, both sides | each path: adapter graph coverage (a non-Go path under a package directory included), else declared inert, else refused | `undecidable:unmapped_go_path:<p>` / `unmapped_go_mod:<p>` (an unmapped Go path not declared inert) / `unclassified_path:<p>` |
| c | reach | the reverse-import closures (test imports included, full transitive fixed point) of the packages each side maps to intersect | `reach_overlap:<pkg dir>` |
| - | none fired | | `independent` |

The path precedence is ADR-124 D3's, first match deciding: G → opaque → adapter graph → inert (doc-check sits beside G as a forced pass). An inert declaration never removes a path from graph reach or the shared-artifact rule: it only decides that a path with NO other classification is not unclassified (`TestClassify_InertNeverRemovesGraphReach`, `TestClassify_PathPrecedence`).

## Reasons

| Reason | Meaning |
|---|---|
| `independent` | no rule fired |
| `global_surface:<path>` | `<path>` (delta or PR) is the workflow spec or matches the declared G class |
| `doc_check:<path>` | `<path>` (delta or PR) matches the declared doc-check class |
| `shared_artifact:<p>` | same path, or both sides under shared-artifact dir `<p>` |
| `reach_overlap:<dir>` | the package at `<dir>` is reachable from both sides |
| `undecidable:<why>` | `surface_undeclared`, `invalid_surface:<entry>`, `surface_incomplete`, `unsupported_language`, `opaque_path:<p>`, `unclassified_path:<p>`, `nil_classifier`, `no_loader`, `empty_delta`, `empty_pr_diff`, `no_workspace`, `invalid_workspace`, `invalid_path:<p>`, `load:<err>`, `load:nil_graph`, `deadline_exceeded`, `panic`, `unmapped_go_path:<p>`, `unmapped_go_mod:<p>` |

## Path mapping (rule c)

- `go.work` (any depth) → every package. A `go.mod`/`go.sum` → every package of the module rooted in its directory (else `unmapped_go_mod`). A declaration normally lists both in G, so rule (b) fires first.
- A `.go` file in a package directory → that package.
- Any other path (non-Go, or a `.go` file outside every package, e.g. under `testdata/`) → **every** ancestor directory, its own first, that is a package directory in ANY loaded graph (approval condition C3). Nearest-only would miss a directory that is a package in H but plain data under a package in B'. No ancestor: the graph maps the path to nothing and the CALLER (`classifyPaths`) decides — declared inert → no seed; otherwise a `.go`/`go.mod` path keeps `unmapped_go_path`/`unmapped_go_mod` and any other path is `unclassified_path`. Nothing is inert by default (ADR-124 D3).

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
2. `Supports` (and `Load`, as defence in depth, before any go command) requires a go.work or go.mod AT THE WORKSPACE ROOT, else an error wrapping `ErrUnsupportedLanguage` (a Go module elsewhere in the tree is not enough, ADR-124 D3). go.work present → `go work edit -json <abs go.work>` for the use list; else the root go.mod → `.`. `Load` never returns an empty graph for an unsupported workspace (the pre-ADR-124 empty graph let a non-Go repository classify `independent`).
3. ONE `go list -e -json=<fields> ./<use>/...` in the HOST build context.
4. A go/parser `ImportsOnly` pass over EVERY `.go` file of every use module (walked like `./...`: no `testdata`, `vendor`, `.`/`_` names, nested modules), ignoring build constraints. This puts an OS-tagged file's imports (`x_windows.go` on a linux host) into the graph and discovers a package whose files are all host-excluded, which `./...` skips silently. Parsing needs no module source, so the loader never requires a non-host-platform third-party module in GOMODCACHE: CI's cache is linux-only under `GOPROXY=off` (approval condition C1; the plan's per-platform `go list` loads were dropped for this reason, since a windows load would need go-winio, go-ansiterm, go-ole, wmi and purego via pgtest).
5. Edges are restricted to workspace packages; stdlib and third-party edges change only through go.mod/go.sum, which map to every package of their module (and which a declaration normally lists in G).

Every go command runs with `cmd.Dir` = workspace, `procgroup.Harden` (a timeout kills the process group), and a pinned env appended LAST: `GOWORK=<abs go.work>|off` (both the `go work edit` and the `go list` call; C7), `GOPROXY=off`, `GOTOOLCHAIN=local` (a toolchain mismatch fails with a clear error instead of downloading; C7), `GOFLAGS=` and `CGO_ENABLED=0`. `Load` is bounded by `Timeout` (default 2m); `Classify` is bounded by ONE `Deadline` (default 5m) covering every workspace's Load (C5).

## Fail-closed table

| Mode | Reason / error | Test |
|---|---|---|
| no declaration (nil `Surface`) | `undecidable:surface_undeclared` | `TestClassify_UndeclaredSurfaceFailsClosed`, server `TestMergeCandidateTransfer_E2E_UndeclaredSurfaceRefused` |
| malformed entry (empty, leading `/` or `./`, `..` segment, backslash, control char, doublestar syntax error; a shared dir not ending in `/`, `/` alone, non-canonical) | `undecidable:invalid_surface:<entry>` | `TestClassify_InvalidSurfaceFailsClosed`, `TestSurface_FirstInvalid` |
| no G entry (`{}`, inert-only) | `undecidable:surface_incomplete` | `TestClassify_IncompleteSurfaceFailsClosed` |
| no adapter for a workspace (no root go.work/go.mod; nested module only) | `undecidable:unsupported_language` | `TestClassify_UnsupportedWorkspaceRefusedBeforeEveryPathRule`, `TestClassify_NonGoRepositoryNeverIndependent`, `TestGoListLoader_Supports`, `TestGoListLoader_NonGoRepo`, server `TestMergeCandidateTransfer_E2E_NonGoRepository*`, `…_E2E_NestedGoModuleIsNotAWorkspace` |
| changed path no class covers | `undecidable:unclassified_path:<p>` | `TestClassify_UnclassifiedPathFailsClosed`, server `TestMergeCandidateTransfer_E2E_UnclassifiedPathRefused` |
| opaque path | `undecidable:opaque_path:<p>` | `TestClassify_PathPrecedence` |
| doc-check path | `doc_check:<p>` (dependent) | `TestClassify_PathPrecedence` |
| workflow spec changed, undeclared | `global_surface:.fishhawk/workflows.yaml` | `TestClassify_WorkflowSpecSelfCovered`, server `TestMergeCandidateTransfer_E2E_WorkflowSpecChangeForcesPass` |
| shared-artifact entries declared narrow-first | dependent in either order | `TestClassify_SharedArtifactOrderIndependent` |
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

## Declared surface (ADR-124 / #4657)

The surface is a repository declaration, never a product default (ADR-110, ADR-124 D6): ONE top-level workflow-v2 `verdict_transfer` block per repository (schema and member reference: `docs/spec/workflow-v2.md` § "Verdict transfer"; struct: `spec.VerdictTransfer`). `ResolveSurface` copies its six members into a `Surface`:

| Member | `Surface` field | Role |
|---|---|---|
| `global_surface` | `Global` | ADR-098 D1 verdict-wide (G): root-anchored doublestar patterns (`testdata/**` does not match `backend/internal/x/testdata/y.json`); a match on EITHER side forces a pass. `MatchGlobal(p)` returns the matching pattern (the harness uses it) |
| `shared_artifact_dirs` | `SharedDirs` | ADR-096 D1(d) directory prefixes ending in `/`; a delta and a PR path under ANY one entry are dependent, the reason naming their longest common directory, never shorter than the entry |
| `doc_check_inputs` | `DocCheck` | ADR-098 D1 doc-check (D) inputs; force a pass on either side until #4317 lands the D-i/D-ii relaxation |
| `inert_paths` | `Inert` | consulted only for a path no other class covers |
| `opaque_paths` | `Opaque` | a change refuses (`undecidable:opaque_path:<p>`) |
| `completeness_check` | `CompletenessCheck` | the conformance test that proves the declaration covers every verify input; a declarative reference fishhawkd NEVER executes or interpolates |

- **Source.** Read ONLY from the run's cached ADMISSION spec (`run.WorkflowSpec`), never a checkout, so a PR cannot change the surface its own decision uses; an edit applies to runs admitted after it merges. v0/v1 specs carry no block and never transfer.
- **Refusals** (ADR-124 D2, D3): absent/unparseable → `surface_undeclared`; no G entry → `surface_incomplete`; a malformed entry → `invalid_surface:<entry>`; no adapter for the workspace → `unsupported_language`; a changed path no class covers → `unclassified_path:<p>`. The schema enforces only member types and the closed member set, so a declaration error is a classification refusal, never an admission failure that would block a repository's unrelated runs.
- **Self-coverage** (ADR-124 D5). `WorkflowSpecPath` (`.fishhawk/workflows.yaml`, pinned equal to the product's by `TestWorkflowSpecPathMatchesProduct`) always forces a pass whatever the declaration lists, so a declaration change is always verified.
- **Prospective coverage.** A valid pattern or shared dir matching no current file is NOT an error. `Surface.Unmatched(files)` lists them, and the server records them on the row as a diagnostic (`surface_unmatched`, at most 16, plus `surface_unmatched_count`); they never refuse and never count as completeness evidence.
- **Enforce withheld** (ADR-124 D4). `Surface.EnforceWithheld()` is true for a declaration naming no (or a blank) `completeness_check`: the server keeps enforce mode in shadow for that run. An ABSENT declaration is not withheld, because the classifier refuses it outright.
- **This repository's worked example** is `testdata/fishhawk-verdict-transfer.yaml`: ADR-098's versioned set moved verbatim (`TestThisRepoDeclaration_EqualsADR098Set`), 26 inert classes (the ones ADR-124's Consequences list, plus every other package-less root file), `doc_check_inputs: []` (the doc paths stay in G until #4317), and a per-entry `#` rationale on every line (`TestThisRepoDeclarationEntriesCarryRationale`). It is the block the operator pastes into `.fishhawk/workflows.yaml` AFTER the release carrying the schema key is deployed (the closed v2 root rejects the reverse order). Until then this repository's runs refuse as `surface_undeclared` (shadow only).
- **The harness switch** (`thisRepoSurface`, ADR-124 D4). It resolves the LIVE `.fishhawk/workflows.yaml` through `ResolveSurface`. A live block is returned after its `Digest` is pinned equal to the example's; with no live block, the example is spliced INTO the live spec and resolved, proving the paste validates. The fallback can only be active while the product refuses every transfer, so it can never mask a loosened live declaration. Over it: `TestThisRepoDeclarationValid`, `TestMatchGlobal_RootAnchored`, `TestClassify_Table` (original results preserved except frontend now forces verification), `TestDeclarationKeepsADR096D3Floor` (+ `TestCheckFloor_Synthetic`: the hard-coded `adr096D3Floor` is this repository's ADR-110 rule-5 conformance tooling; a floor pattern must stay in G, or for `docs/api/**`, `site/**`, `docs/adr/**`, `docs/ARCHITECTURE.md` only, in doc-check), the verify-leg tests below, and `TestThisRepository_EveryTrackedPathClassified` (every `git ls-files` path forces a pass, is opaque, or classifies through `classifyPaths` over the real graph; its `knownUnclassified` allow-list is empty, and a stale entry fails).

Throughput cost of the example, accepted as conservative ("fail closed means verify"): a delta touching fishhawkd's main package, docgen/cmdinfo, the version packages or a schema mirror always forces a pass.

## Adding a verify leg

`verifylegs_test.go` parses `scripts/test`. Every non-blank, non-comment line of `cmd_verify` and `_verify_gate_harnesses` must be a call of a leg registered in `realLegTable` or an exact allow-listed plumbing line; leg tokens are word-anchored (`xcmd_lint` never yields `cmd_lint`, `my_verify_lock` never `_verify_lock`; `TestLegTokenWordAnchored`). Each registered `_verify_*` body's `$ROOT/scripts/<x>` / `$SCRIPTS_DIR/<x>` invocations must equal its row's `scripts`. Each row's inputs are matched against the repository: a `global` input must match at least one file and every match must be covered by the declaration's verdict-wide (G, the workflow spec included) or doc-check class — an inert class NEVER covers a verify input; a `goReach` input (compile/lint/test consumption only) may match only `.go` files. `TestSchemaSyncDestinationsCoveredByDeclaration` requires the same of every `scripts/sync-schemas` mirror destination. Floors (>= 8 legs incl. `cmd_lint` + `_verify_gate_harnesses`, >= 12 harnesses, >= 10 sync destinations) turn a parser collapse RED; `TestCheckVerifyLegs_Synthetic` H9 (an input covered only by an inert class) and H10 (a nil surface) pin the coverage predicate.

So: a new leg, harness, inline script call or mirror RED-lines verify (`TestVerifyLegsCoveredByDeclaration`) until it is registered and its inputs are covered by THIS repository's declaration — `testdata/fishhawk-verdict-transfer.yaml` until the operator's live block lands, then `.fishhawk/workflows.yaml` (with the example kept equal). The live block is an operator-only ADR-121 tier-I change (ADR-124 D5): a Fishhawk run cannot write `.fishhawk/**`, so a run that adds a leg flags the declaration change for the operator rather than editing around it.

## Residuals

- **Inert over Go source (ADR-124 D3 precedence; REVIEW BEFORE ANY ENFORCE ENABLEMENT).** The adapter graph is consulted before inert, so a path the graph cannot map — an unmapped `.go` file (e.g. under `testdata/`) or a `go.mod` with no packages — is classified INERT when a declared inert glob covers it, where an undeclared one keeps its `unmapped_go_path` / `unmapped_go_mod` refusal (`TestClassify_PathPrecedence` pins the inert-declared row → `independent`). This is the ratified first-match rule, implemented exactly; no extra refusal rule is added, because that would go beyond the ratified text. Operators should not declare inert globs over Go source, and this residual is a prerequisite to review before enforce is enabled for any repository.
- **A declared-inert class carries no doc-leg or data-read protection** until #4317 implements ADR-098 D1/D2's D-i/D-ii and data edges (ADR-124 Consequences). A verify leg that READS an inert-declared path as data is caught only by the verify-leg coverage tests above (an inert class never covers a verify input), not by the classifier.
- **The declaration's completeness is proven only where a check exists.** In this repository the harness above reads the live block; in any other repository `completeness_check` is a named reference the product never runs, and enforce stays withheld without one. A declaration's presence proves nothing; ADR-098 D3's false-transfer rule remains the shadow-phase detector.
- A harness that starts reading a new path outside the declaration's G/doc-check classes is not caught (only new legs, lines and mirrors are). Partly mitigated in this repository: editing a harness touches `scripts/**`, which its declaration lists in G.
- Parsing is textual (like `scripts/check-site-ia`); an indirect leg call through a variable is caught only as an unclassified line.
- A CUSTOM build tag is visible (the go/parser pass ignores constraints), but a file excluded by name convention (`_x.go`, `.x.go`) or under `testdata`/`vendor` is not a package file, matching `go`'s own rules.
- Go tests reading OTHER packages' source as data (`wirecontract`'s go/ast parity scan, the `agentpaths`/`constraint` twins, `testanchor`'s repo-wide scan, the audit category-emission scan) are invisible to the import graph. Most check a per-file property, which cannot interact across sides; the pairwise ones force each side to edit both members, so reach overlaps. A red main traced to one is a D3/reach coverage bug per ADR-096.
- ADR-096's stated residuals (DB runtime coupling, reflection / `go:linkname`, global state) stand.
- A delta that REMOVES a package the PR imports makes H' fail `go list` (the import no longer resolves), so the verdict is `undecidable:load:…`, not `reach_overlap`. Fail closed and deliberate: either way the pass runs.
- The case-folding guards (`O_EXCL`, symlinks last, the parent `Lstat` walk) are exercised only on a case-insensitive filesystem (macOS dev hosts); on a case-sensitive host the colliding entries are distinct files, the two-symlink test skips with a printed reason, and the planted-symlink test pins the parent walk on any filesystem.
- `MaterializeTree` writes a symlink's target verbatim, so a symlink may point outside the workspace. `go list ./...` and the loader's go/parser walk do not follow directory symlinks, and the loader refuses a package directory outside the workspace. `ExpandSymlinkAliases` makes any such link fail closed (`ErrSymlinkEscape`) before classification.
- Alias resolution is per tree and case-sensitive: on a case-folding filesystem a link target differing from a changed path only in case is not matched.
- `GOPROXY=off` means a host whose module cache lacks a HOST-platform dependency gets `undecidable` (verify), never a fetch. The runner gate's cache holds every host-platform module because the verify test loop builds the same workspace in the same environment; non-host-platform modules are never needed (C1). `TestGoListLoader_RealRepository` fails if the real workspace does not load.

The repository workflow wrapper now verifies frontend inputs too. Its declaration adds `frontend/**` to G (and removes it from inert); `TestThisRepoDeclaration_ADR098PlusFrontend` preserves the frozen ADR-098 snapshot and permits exactly that addition. Shared-directory classifier tests use infra paths so the earlier global match does not mask that branch.
