# backend/internal/upkeep

Pure primitives of the upkeep scan (E79 / #3726): the hidden finding marker and
the open-issue dedupe an `upkeep_report_v1` ingest runs over its proposed
issues (#3921), and the two deterministic evidence detectors the scan prompt is
built from — `DetectPinDrift` and `AggregateFlakes` (#3922). #3924 (apply)
consumes the duplicate marks. #3750 adds the `advisory` source's Dependabot
coverage matcher (`MarkCovered`) and its server-rendered filing helpers
(`RenderAdvisoryTitle`, `RenderAdvisoryFacts`) in `advisory.go`.

The package imports no forge client, no `workmgmt`, no server package, no
`bundle` and not `backend/internal/plan`'s report types. Its inputs are its own
`Proposal`, a path → content map, `FlakeStage`s, and `AdvisoryProposal` /
`PullRequest` / `AdvisoryFacts`; the server adapts a validated report, the files
it read, the gate evidence it read and a forge pull-request listing into them.
Nothing here reads, files, closes, comments on or relabels anything.

## Finding marker

```
<!-- fishhawk-upkeep:v1 finding_id=<id> -->
```

- `FindingMarker(id)` returns exactly these bytes. #3924 embeds the marker in
  the body of every issue it files for a finding; `MarkDuplicates` reads it.
- `<id>` is the report's `<source>:<subject>` finding id. The
  `upkeep_report_v1` id pattern excludes whitespace and `<`/`>`, so matching
  the marker INCLUDING its closing ` -->` is an exact match: the marker of
  `flake:TestAB` never matches `flake:TestA`.
- Changing the format orphans every issue filed under the old one. Pinned by
  `TestFindingMarker_Format`.

## `MarkDuplicates(proposals, candidates) []Duplicate`

Rules, in order:

1. **Closed candidates never mark**, by either basis. They are dropped first. A
   closed issue for a finding the scan still sees means the fix did not hold —
   a reason to file again, not to skip.
2. **Marker** (`basis: marker`): the lowest-numbered open candidate whose body
   contains `FindingMarker(id)`.
3. **Similarity** (`basis: similarity`): otherwise, `intakegroom.Duplicates`
   over the open set, taking the best result at `medium` confidence or higher
   (`intakegroom.ThresholdMedium`, lexical title Jaccard plus the same-type
   bonus). A `low` band never marks.

- At most one `Duplicate` per proposal; output is in proposal order; the result
  is never nil (an empty set is `[]`, never `null`).
- The filing handed to `intakegroom` carries an EMPTY body on purpose.
  `intakegroom` skips a candidate whose title AND body both equal a non-empty
  filing body (its reader-echo guard); an identical open issue is precisely the
  duplicate this must catch. `Proposal` therefore has no `Body` field.
- `Duplicate` JSON: `finding_id`, `issue_number`, `issue_url` (omitted when the
  reader supplied none), `basis`, and — for `similarity` only — `score` and
  `confidence`. A marker match is exact, not scored.

## Server adapter (`backend/internal/server/upkeep_dedupe.go`)

`(*Server).upkeepDuplicates(ctx, runRow, proposals)` reads the candidate window
through the intake hook's existing `intakeCandidates` seam (unchanged) and
runs `MarkDuplicates`. It never returns an error and never panics out.

- **Target**: the run's `owner/name`; `Project`/`Jira`/`GitLab` from the
  repo's conventions; scope from the run's `InstallationID`, else (GitHub
  client wired and provider `github_projects`) the resolved App installation —
  the run-scoped shape `handleFileWorkItem` uses.
- **Bound**: conventions load, installation lookup and the enumeration share
  ONE child context of `intakeGroomDeadline()` (default
  `intakegroom.DefaultDeadline`). As in the intake hook, that bounds a
  cancellation-cooperative reader; the production reader is one.
- **Empty report**: zero proposals skip the read entirely (healthy, `[]`).
- **Degrades** (each: `Degraded=true`, the reason, `Duplicates=[]`):

  | Reason | Cause |
  |---|---|
  | `repo_malformed` | run `Repo` is not `owner/name` (adapter-only) |
  | `conventions_unavailable` | conventions loader errored before the deadline (adapter-only) |
  | `reader_unavailable` | provider has no work-item reader |
  | `reader_error` | reader errored, returned a nil page, or the installation lookup failed before the deadline |
  | `budget_exceeded` | the shared deadline had passed when ANY of the three steps — conventions load, installation lookup, enumeration — failed |
  | `hook_panic` | a panic in the adapter or the reader was recovered |

  A timeout is attributed to the budget, not to the step it interrupted: the
  conventions and installation branches report `budget_exceeded` when the
  child context's deadline has passed, and `intakeCandidates` already does the
  same for the enumeration. Pinned by
  `TestUpkeepDuplicates_ConventionsDeadline_BudgetExceeded`,
  `TestUpkeepDuplicates_InstallationLookupDeadline_BudgetExceeded` and
  `TestUpkeepDuplicates_BudgetExceeded`.
- Adapter-only reasons and the conventions/installation-step degrades
  (including their `budget_exceeded`) WARN-log as
  `upkeep dedupe degraded; …`; reasons raised inside `intakeCandidates` log
  under the intake-groom message (cosmetic misattribution, accepted to leave
  the intake hook unchanged).
- `ScannedItems` and `WindowTruncated` report the window the marks were drawn
  from, so a miss caused by the window is visible, not silent.

## Cited-run refusal at ingest (`server/upkeep_binding.go`, `server/upkeep_report.go`)

Recorded here because the binding approval of #3921 places it in this README.
A report citing a run that is unknown, belongs to another repository, or
belongs to another account is refused `upkeep_report_invalid` with ONE
caller-visible reason, `run_ref_invalid` (plus the refused `run_id`), for all
three — the caller cannot use the refusal to probe whether a run id exists.
Which of the three it was goes to the server WARN log ONLY: never to the
response, the stage failure reason or an audit row, all of which the run's
tenant can read. Ownership compares `Repo` case-insensitively and compares
`AccountID` ONLY when both rows carry one; if either row lacks an `AccountID`
the check is repository-only. The rule is `upkeepRunOwnershipRefusal`
(`server/upkeep_binding.go`), shared with the scan's flake gather.

Only a run ref's `run_id` is checked. Its `stage_id` and `trace_ref` are
AGENT-ASSERTED and UNVERIFIED: the ingest parses `stage_id` as a UUID but never
confirms the stage exists or belongs to the cited run, and never resolves
`trace_ref`. Treat both as the scanning agent's pointer, not as evidence the
server vouched for.

## `DetectPinDrift(files) []PinDrift` (`pindrift.go`)

One `PinDrift` per pin FAMILY whose values disagree. The family is the finding
subject, so the id is `toolchain_drift:<family>`.

| Family | Files read | Pin | Compared on |
|---|---|---|---|
| `go` | `go.work`, root `go.mod`, any `*/go.mod` | column-0 `go X.Y[.Z]` directive | MAJOR.MINOR |
| | root `.golangci.yml` / `.golangci.yaml` | `go:` directly under the top-level `run:` block | MAJOR.MINOR |
| | `.github/workflows/*.yml\|*.yaml` | literal `go-version:` (`go-version-file:` ignored) | MAJOR.MINOR |
| `golangci-lint` | workflows | `golangci-lint/vX.Y.Z/install.sh` tag; the last `vX.Y.Z` after `sh -s --` on a line (or after a line) naming golangci-lint | exact |
| `@redocly/cli` | workflows, root `AGENTS.md`, files directly under `docs/api/` | `@redocly/cli@X.Y.Z` | exact |

- A pin's `Value` is the UNQUOTED scalar: `'1.25'` and `"1.22"` yield `1.25` /
  `1.22`, which pass the prompt's fact charset.
- **Unnormalizable go-version literals are not pins.** A value that is not
  `MAJOR.MINOR[.PATCH]` — `stable`, `1.25.x`, `>=1.25`, a `${{ matrix.go }}`
  expression, a `[1.24, 1.25]` list — contributes NO occurrence, so it can
  neither raise nor hide a drift. Pinned by `TestUpkeepScan_UnnormalizableGoVersionIsNotAPin`
  (server) and the detector's own tests.
- YAML lines whose first non-space rune is `#` are skipped (a commented pin is
  not executed); Markdown lines are all scanned (a command in prose is the
  command people run).
- Occurrences sort by (path, line, value), families by name; the input map is
  iterated in sorted order, so output is deterministic.
- **Occurrence cap** (`capPinHits`, #3924 carried item 7): at most
  `PinMaxOccurrences` (32) per family, the rest counted in
  `OmittedOccurrences`. A finding must show the disagreement AND name at least
  two distinct paths (`upkeep_report_v1` rule (g)), so the slots fill in tiers,
  each in hit (sorted) order:
  1. one occurrence per distinct value;
  2. when the slots kept so far cover fewer than 2 paths while the input spans
     2 or more, one occurrence from an uncovered path — added if a slot is
     free, otherwise REPLACING the latest-added tier-1 slot;
  3. one occurrence per distinct (value, path) pair;
  4. the remaining hits.

  The kept occurrences are returned in the original hit order. **Bound**: a
  distinct value can lose its only occurrence ONLY to keep the two-path
  minimum — when the distinct values alone fill every slot from a single path
  — and then exactly one value (the latest in hit order) yields its slot.
  Otherwise every distinct value keeps at least one occurrence. Pinned by
  `TestCapPinHits_RetainsEveryPathPerValue` (32 alternating `1.24`/`1.25` in
  `a.yml` plus one in `b.yml`: both paths kept),
  `TestCapPinHits_SwapsInSecondPathWhenValuesFillTheCap` (`a.yml` alone
  carries `PinMaxOccurrences` distinct values), `TestCapPinHits_KeepsOnePerValuePath`
  and `TestDetectPinDrift_OccurrenceCapKeepsEveryValue`.

## `AggregateFlakes(stages) []Flake` (`flakes.go`)

- **Rule**: within ONE stage, a FAILED verify attempt counts when a LATER
  attempt PASSED on the identical, non-empty `TreeSHA`. A pass on a different
  tree, an unrecorded tree or a pass-then-fail order counts nothing.
- **Subjects** come from the failed attempt's output tail: each
  `--- FAIL: <name>` contributes its TOP-LEVEL test (`TestA/case` → `TestA`),
  and the whole name must match `[A-Za-z0-9_]+` — `TestBad-Name` becomes
  `verify-gate:unnamed`, never `TestBad`. A tail with no FAIL line is
  `verify-gate:unnamed`. `InfraRetries` (the runner's absorbed
  `verify_infra_flake_retry` count) adds that many occurrences to
  `verify-gate:infra`. The detector returns subjects only, never the tail.
- **Aggregation**: occurrences summed per subject; refs are the distinct
  (run, stage) pairs, sorted, capped at `FlakeMaxRefs` (20) with the rest in
  `OmittedRefs`; output sorted by occurrences desc, then subject.
- **Subject cap**: `CapFlakeSubjects` keeps the first `FlakeMaxSubjects` (32)
  and returns the dropped count, which the gather passes on as
  `OmittedFlakes`.

## Evidence gather (`server/upkeep_evidence.go`)

`(*Server).resolveUpkeepScanContext` runs in BOTH prompt handlers (signed
`/prompt` and the `/prompt-render` preview) right after the grooming
determination, and sets `prompt.Trigger.Upkeep`. A non-plan stage costs
nothing; a plan stage costs the binding read #3921's ingest already pays (one
`GetRun` + spec parse); only a stage whose cached spec declares `produces:
upkeep_report` gathers. That gather is NEW serve-time cost: the sibling
grooming determination's serve-time read is one document fetch, while this one
reads pin files and up to 40 redacted bundles, so it is served through the scan
cache below. A binding transport error is a 500
(`resolve the stage's upkeep_report declaration failed`); the gather itself
never errors — every partial read is a named DEGRADE, WARN-logged with the
run id and rendered in the prompt's facts block, so a partial scan is never
presented as a complete one. No audit row is written.

**Pins (source `toolchain_drift`).** Read through the document-injection
fetcher (`cfg.DocumentResolver.Fetcher`) under `cfg.DocumentScope`, at the
run's RECORDED admission commit `runs.document_base_commit` exactly — never a
mutable ref. The file set: `go.work`, the `go.mod` of each go.work `use` dir
(both forms; `.`, absolute and `..` dirs dropped), the root `go.mod`, both
root golangci configs, the `*.yml`/`*.yaml` entries `githubclient.ListDirectory`
lists under `.github/workflows` at that commit (GitHub runs only), `AGENTS.md`,
`docs/api/README.md` and `docs/api/v0.md`. An absent file or workflow directory
(`forge.ErrNotFound`) is NOT a degrade.

**Flakes (source `flake`).** `ListRuns{Repo, AccountID}` pages newest-first
(the query orders `created_at DESC, id DESC`, 100 runs a page); per run it
skips the scanning run itself and any run an EARLIER page already served —
offset paging over a newest-first list re-serves the boundary run when a run is
created between two pages, and the seen-run set reads it once and counts it
once toward the scan cap (#3924 carried item 8; pinned by
`TestGatherUpkeepFlakes_TwoPages`) — STOPS at the first run older than the 14-day window, and drops any
run `upkeepRunOwnershipRefusal` rejects — the same predicate the ingest's
`checkUpkeepRunRefs` applies — so the gather only ever reads **runs the
upkeep_report ingest ownership predicate accepts** (same repository
case-insensitively; same account only when BOTH rows carry one, so an
account-less same-repo run is read). For each implement stage it reads the
newest REDACTED bundle (`trace_uploaded` → `pickRedactedTraceHash` →
`TraceStore.Get` → `bundle.ExtractGateEvidence`) and maps `VerifyRuns` to
attempts and `FlakeRetries` to `InfraRetries`. A stage with no redacted trace
or no gate evidence contributes nothing and is not a degrade.

**Bounds** (package vars): wall budget 20 s for both gathers; flake window
14 days; run page 100; run-scan cap 300; bundle cap 40; pin file 1 MiB; bundle
64 MiB; 32 go.work dirs; 64 workflow files; scan-cache TTL 60 s and 256
entries. The prompt renderer adds its own caps (per
family occurrences, flake subjects, facts bytes), each disclosed by an omitted
line.

| Source | Reason | Cause |
|---|---|---|
| `toolchain_drift` | `pin_reader_unwired` | no `DocumentResolver` / fetcher wired |
| | `base_commit_unrecorded` | the run has no `document_base_commit` (no fetch is made) |
| | `repo_malformed` | run `Repo` is not `owner/name` |
| | `scope_unavailable` | `DocumentScope` errored |
| | `workflow_listing_unavailable` | non-GitHub run, or no GitHub client |
| | `workflow_listing_failed` | the Contents API listing errored |
| | `workspace_dirs_capped` | go.work `use`s more than 32 dirs (count = dirs cut) |
| | `workflow_files_capped` | more than 64 workflow files (count = files cut) |
| | `pin_fetch_failed` | a file read errored (count = files) |
| | `pin_file_too_large` | a file exceeded 1 MiB (count = files) |
| | `budget_exceeded` | the wall budget ran out (count = reads not made) |
| `flake` | `trace_store_unwired` | no run repo, audit repo or trace store |
| | `run_list_failed` | `ListRuns` errored |
| | `run_scan_capped` | more eligible runs than the 300-run cap |
| | `stage_list_failed` | a run's stage list errored |
| | `trace_list_failed` | a run's `trace_uploaded` read errored |
| | `trace_fetch_failed` | a bundle `Get`/read errored |
| | `bundle_too_large` | a bundle exceeded 64 MiB |
| | `gate_evidence_parse_failed` | a bundle's gate evidence did not parse |
| | `bundle_cap_reached` | the 40-bundle cap stopped the gather |
| | `budget_exceeded` | the wall budget ran out |

**Scan cache** (#3924 carried item 11). The gather is served through a
process-local, per-(server, run, stage) single-flight + short-TTL cache
(`upkeepScanCache`, hand-rolled: one mutex plus an in-flight channel per entry,
no `golang.org/x/sync` dependency). The binding is still read on every call, so
its transport error is never cached.

- **Shared.** `/prompt` and `/prompt-render` for one stage share one gather
  within `upkeepScanCacheTTL` (60 s), and concurrent serves of one stage gather
  once: a caller arriving while a gather is in flight waits for it. Distinct
  (run, stage) pairs never share an entry.
- **Detached.** The leader runs the gather under
  `context.WithoutCancel(request ctx)` bounded by its own 20 s budget, so a
  cancelled leader never hands its waiters a cancelled or truncated result;
  a waiter blocks at most that one budget.
- **Failures are not cached.** A gather that panics leaves no entry; its
  waiters retry (one becomes the new leader). A DEGRADED gather (e.g.
  `budget_exceeded`) completed and IS cached for the TTL — the accepted price
  of bounding repeated 64 MiB bundle reads.
- **Bounded.** Expired entries are swept on every insert, and a hard cap
  (`upkeepScanCacheMaxEntries`, 256) runs a gather UNCACHED when the swept map
  is still full.

Pinned by `TestResolveUpkeepScanContext_CachesPerStage`, `_SingleFlight`,
`_CancelledLeaderDoesNotPoisonWaiters`, `TestUpkeepScanCache_KeyIncludesStage`,
`_AbortedGatherIsNotCached` and `_BoundedMap`.

**Undecidable binding** (#3924 carried item 10). A plan stage in a workflow
declaring `produces: upkeep_report` whose own declaration is undecidable
(`workflow_unresolved`, `stage_unmappable`) gets a nil scan context, i.e. the
ORDINARY plan prompt. This is a documented residual, not a prompt fork: the
plan-path guard refuses that stage's plan, and the ingest refuses its
upkeep_report fail-closed (`upkeepIngestRefusal` →
`stage_binding_undecidable`, category B), so an upkeep prompt served there
could not produce an accepted report either. Pinned by
`TestResolveUpkeepScanContext_UndecidableIsResidual`.

Test seams: `newUpkeepPinSource`, `upkeepListWorkflowDir`, `upkeepScanNow` and
`upkeepScanJoinHook` are package vars (no Server/Config field), swapped by
non-parallel tests, as are the bounds (`upkeepFlakeRunPageSize`,
`upkeepScanCacheTTL`, …).

## Plan-path guard: undetectable bodies (`server/upkeep_report.go`)

A body `plan.DetectArtifactKind` cannot classify is mapped to kind `plan`, so
on a stage declaring `produces: upkeep_report` the guard refuses it. Since
#3922 the guard receives the raw body and the detection error, and the refusal
says WHY via `upkeepGuardBodyDetail`: the JSON parse error verbatim, no top-level
`kind`, a kind `plan.AllArtifactKinds` recognizes but the upkeep allowlist does
not admit (an explicit `"kind":"plan"` reads "a recognized artifact kind but is
not allowed on a stage declaring produces: upkeep_report", #3924 carried item
9), or the unrecognized kind (echo truncated to 64 bytes), and names `upkeep_report_v1` / kind `upkeep_report`. The code stays
`plan_invalid`, so the bounded schema retry still runs; its recorded
`validation_error` reaches the next attempt's prompt, which `buildUpkeepScan`
renders under "Prior upkeep-scan schema validation failure" naming
`upkeep_report_v1`, never the plan schema's heading.

## Unanchored scans: the `ticket_reference` fallback

`upkeep_report_v1` requires a `github_issue` `ticket_reference`, but a
scheduled scan with no `issue` anchor has no triggering issue. The scan prompt
instructs the RATIFIED fallback: the repository's issues index URL
(`https://github.com/<owner>/<repo>/issues`) with id `<owner>/<repo>`, stated in
`summary`. It satisfies the schema (`url` is a URI, `id` is non-empty) but is a
convention, not a ticket. Pinned by `TestUpkeepFacts_RoundTripThroughIngest`,
which ingests a report carrying it.

## Dependabot coverage and advisory filing (#3750)

`advisory.go`. An advisory finding an open Dependabot pull request already
fixes is MARKED covered at ingest and skipped at apply; an approved advisory
finding that is not covered is filed from structured fields only. The server
adapter (`server/upkeep_coverage.go`) feeds it `githubclient.ListOpenPullRequests`
and the manifest directories from `plan.UpkeepAdvisoryManifestDirs`.

### `MarkCovered(advisories, prs) []Covered`

A finding is covered iff ALL hold:

1. it has a fixed version (`FixedVersion != ""`; a "no fix" advisory is never
   covered);
2. it names at least one manifest directory — with none, the every-directory
   rule would be vacuously true;
3. for EVERY directory, some pull request (input order, first wins) has
   `Author == "dependabot[bot]"` (the REST `user.login`), a head ref whose
   ecosystem equals the finding's (`DependabotEcosystem`:
   `dependabot/go_modules/` → `go`, `dependabot/npm_and_yarn/` → `npm`, anything
   else → unknown, never covers), and a bump of the same package in that exact
   directory to a version `VersionAtLeast` reports comparable and at least the
   fixed version.

- Output is in advisory input order and never nil. `Covered` JSON (the
  `upkeep_report_recorded` payload's `covered` entry shape): `finding_id`,
  `package`, `pulls: [{number, url (omitempty), directory, bumps_to}]`, one pull
  per directory in the finding's directory order.
- Directories use the manifest form: `.` for the repository root, else a slash
  path (`backend`, `site/docs`). Dependabot's `/backend` normalizes to `backend`
  and `/` to `.`.

### `DependabotBumps(pr) []Bump`

Grounded in this repository's real pull requests, not a documented contract
(#3823, #3825, #3820). The title is read after any commit-style prefix
(`deps(backend)(deps): `) or `[Security] ` tag, case-insensitively.

| Title shape | Bumps | Directory |
|---|---|---|
| `bump <pkg> from <a> to <b> in /<dir>` | that one | `<dir>` |
| `bump <pkg> from <a> to <b>` (no `in`) | that one | unknown |
| `bump the <g> group in /<dir> with N updates`, `bump <a> and <b> in /<dir>` | one per body line `` Updates `<pkg>` from <a> to <b> `` or `Bumps [<pkg>](<url>) from <a> to <b>.` | `<dir>` |
| `bump the <g> group across N directories with M updates` (even N = 1) | one per body line | unknown |
| anything not starting `bump ` | none | — |

An unknown directory never equals a manifest directory, so it never covers.

### `VersionAtLeast(have, want) (atLeast, comparable bool)`

Semver §11 precedence: one leading `v` trimmed, `MAJOR.MINOR.PATCH` numeric
without leading zeros, prerelease identifiers dot-separated (numeric ones
compared numerically, numeric below alphanumeric, the shorter set lower, a
release above its own prereleases), build metadata ignored. **Go
pseudo-versions are comparable** — all three forms (`vX.0.0-<ts>-<hash>`,
`vX.Y.Z-pre.0.<ts>-<hash>`, `vX.Y.(Z+1)-0.<ts>-<hash>`) are valid semver
prereleases, so `v0.23.1-0.<ts>-<hash>` reaches a `v0.23.0` fix and
`v0.23.0-0.<ts>-<hash>` does not. Anything else — a range (`^1.2.3`,
`>=1.2.3`), a partial version, an empty string, a leading zero, an empty or
non-`[0-9A-Za-z-]` identifier — is `comparable=false`, and `MarkCovered` treats
it as NOT covered.

### Filing helpers

- `RenderAdvisoryTitle(f)` → `<primary id>: <package> <version> (<severity>
  severity)`, cut at GitHub's 256-character title limit.
- `RenderAdvisoryFacts(f)` → a `### Advisory facts (server-rendered)` block with
  one line per field: advisory ids, ecosystem, package, in-use version, fixed
  version (`Fixed version: no fix published` when `""`), reachability, severity
  and the cited manifests. It takes no call path and no prose.
- Both withhold a value that is not a plain token for its field
  (`(withheld: not a plain token)`), and the body renders each value as an
  inline code span. The token charsets exclude whitespace and the backtick, so
  a span cannot be broken out of and markdown, mentions and issue references
  inside one are inert: no free text reaches the tracker through a structured
  field (`version` and `fixed_version` carry no schema charset of their own).
- `CallPathDisclosure(body, frames)` is DEFENSE IN DEPTH only: the apply files
  server-rendered advisory titles and bodies, so agent prose never reaches the
  tracker. It reports the first caller-frame token (frames at index >= 1; index
  0 is the public vulnerable symbol) found in `body`: a non-empty filename,
  `Package.Function`, or `Receiver.Function` with a leading `*` trimmed. A
  case-sensitive substring match, so a paraphrase evades it.

### Conservative residuals (fail toward filing)

Each leaves a real fix uncovered, so the finding is proposed and, if approved,
filed even though a Dependabot pull request would fix it:

- **Multi-directory groups.** An `across N directories` group names no single
  directory, so none of its bumps covers.
- **Root without `in /`.** A single-dependency title naming no directory is
  unknown, not assumed to be the root.
- **Unparseable versions.** Ranges and malformed versions never compare.
- **Unknown ecosystems and shapes.** A head ref outside `go_modules` /
  `npm_and_yarn`, a title or body Dependabot reshapes, or a pull request past
  the listing cap (`truncated`) is not read as covering.

### False-cover risk (the unsafe direction)

A covered finding is NOT filed. These cases suppress a filing for a dependency
that stays vulnerable somewhere:

- **Under-cited manifests.** Covered marks are only as complete as the agent's
  manifest citations. A finding that cites `backend/go.mod` but not
  `runner/go.mod`, where the module also appears, is covered by a `/backend`
  pull request alone, and the `runner` copy is never filed. The prompt's merge
  rule (cite EVERY manifest where the module appears) is the only control; the
  server cannot see a manifest the agent did not cite.
- **Agent-asserted fixed version.** A fixed version stated lower than the real
  fix lets a too-low bump cover.
- **Proposed, not merged.** Covered means an open pull request proposes the
  fix, not that it landed. While it stays open, every scan re-proposes and
  re-skips the finding. Coverage is a snapshot at ingest: a pull request closed
  between ingest and the gate still suppresses that filing (the next scan
  re-proposes it).

## Residuals

- **Marker and title planting.** Anyone who can open or edit an issue in the
  repo can plant a finding's marker, or a near-identical title, on an OPEN
  issue and so suppress that finding's filing. Bounded: it only SKIPS a filing
  (nothing is closed or edited), and the mark — issue number and basis — is
  visible in the `upkeep_report_recorded` payload's `duplicates`. #3924
  decided a marker match suppresses filing WITHOUT a provenance check; the
  decision, its rationale and the apply-side residuals are recorded in
  `backend/internal/server/README.md` § "On-approval upkeep apply".
- **Open-window miss.** `intakeCandidates` enumerates newest-first with closed
  items included, capped at `intakegroom.DefaultMaxScanned`. Closed items
  consume part of the window, so an older OPEN duplicate past the cap is not
  seen. `WindowTruncated` makes this visible.
- **Cross-forge / cross-installation same-name repo.** `checkUpkeepRunRefs`
  (and the flake gather through the same `upkeepRunOwnershipRefusal`) compares
  `Repo` case-insensitively and `AccountID` ONLY when both rows carry one; it
  does not compare the forge or the installation. A run of a same-named
  `owner/name` on another forge (a GitLab project spelled like the GitHub
  repo) or under another installation therefore passes when either row lacks
  an account id: the report may cite it and the gather may read its redacted
  verify history.
- **Unverified evidence pointers.** A flake run ref's `stage_id` and
  `trace_ref` are agent-asserted (see above); only `run_id` existence and
  ownership are checked.
- **Process-local ingest mutex.** The ingest serializes on a mutex local to one
  fishhawkd process, as the grooming ingest does. Two replicas ingesting the
  same report at the same instant can both write; the content-hash idempotency
  bounds what a retry does, not what a concurrent pair does.
- **Lexical similarity** false-positives and false-negatives. A wrong mark costs
  one skipped filing the captain can see; a missed one costs one redundant
  proposal.
- **Single-file drift.** A disagreement inside ONE file (an install.sh tag
  `v2.8.0` and a `sh -s --` argument `v2.9.0` in the same `ci.yml`) is
  detected but cannot satisfy rule (g), which needs at least 2 distinct paths;
  the prompt tells the agent to report it in `summary` only.
- **Cut FAIL lines.** Gate evidence keeps a bounded output tail (30 lines /
  ~4 KB), so a `--- FAIL:` line can be cut and the flake lands as
  `verify-gate:unnamed`.
- **Latest bundle only.** Only the newest redacted bundle per stage is read,
  so verify runs from earlier fix-up passes of that stage are not seen.
- **Preview drift (narrowed).** Within the 60 s scan-cache TTL `/prompt-render`
  and the signed `/prompt` share one gather and agree. Past the TTL the second
  call re-gathers, so a run landing or the budget expiring between them can
  still make the two differ.
- **Stale or degraded scan for one TTL.** A cached gather — including a
  degraded one — is served for up to 60 s, so a run recorded inside that window
  is not seen until the entry expires.
- **Process-local scan cache.** Each fishhawkd replica keeps its own cache, so
  two replicas serving one stage each gather.
- **Undecidable binding gets the plain plan prompt** (see "Evidence gather");
  the stage then fails category B on its first ship.
- **Test-name charset.** Names are restricted to `[A-Za-z0-9_]` at extraction
  and `[A-Za-z0-9._/@:+-]` at render (non-conforming values withheld).
  Identifier-shaped words can still spell text, but only inside the labelled
  FACTS block, never as raw tail.
- **Crew deliveries not rendered.** The prompt handlers record crew-message
  delivery for every plan stage, but `buildUpkeepScan` (like
  `buildGroomingPropose`) does not render `CrewMessages`, so a delivery to an
  upkeep stage is recorded but not shown.
- **`ticket_reference` fallback** for unanchored scans is a convention, not a
  ticket (see above).
- **Live forge read unexercised here.** The gather's tests drive the forge
  fetcher and the Contents API listing through fakes that assert the ref; the
  live API at a commit SHA is exercised only by #3924's end-to-end run.
