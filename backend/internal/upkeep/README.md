# backend/internal/upkeep

Pure primitives of the upkeep scan (E79 / #3726): the hidden finding marker and
the open-issue dedupe an `upkeep_report_v1` ingest runs over its proposed
issues (#3921), and the two deterministic evidence detectors the scan prompt is
built from — `DetectPinDrift` and `AggregateFlakes` (#3922). #3924 (apply)
consumes the duplicate marks.

The package imports no forge client, no `workmgmt`, no server package, no
`bundle` and not `backend/internal/plan`'s report types. Its inputs are its own
`Proposal`, a path → content map and `FlakeStage`s; the server adapts a
validated report, the files it read and the gate evidence it read into them.
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
- **Occurrence cap**: at most `PinMaxOccurrences` (32) per family, the rest
  counted in `OmittedOccurrences`; every distinct value keeps at least one
  occurrence, so the cap never hides the disagreement.

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
upkeep_report` gathers. A binding transport error is a 500
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
(the query orders `created_at DESC, id DESC`); per run it skips the scanning
run itself, STOPS at the first run older than the 14-day window, and drops any
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
14 days; run-scan cap 300; bundle cap 40; pin file 1 MiB; bundle 64 MiB; 32
go.work dirs; 64 workflow files. The prompt renderer adds its own caps (per
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

Test seams: `newUpkeepPinSource` and `upkeepListWorkflowDir` are package vars
(no Server/Config field), swapped by non-parallel tests.

## Plan-path guard: undetectable bodies (`server/upkeep_report.go`)

A body `plan.DetectArtifactKind` cannot classify is mapped to kind `plan`, so
on a stage declaring `produces: upkeep_report` the guard refuses it. Since
#3922 the guard receives the raw body and the detection error, and the refusal
says WHY via `upkeepGuardBodyDetail`: the JSON parse error verbatim, no top-level
`kind`, or the unrecognized kind (echo truncated to 64 bytes),
and names `upkeep_report_v1` / kind `upkeep_report`. The code stays
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

## Residuals

- **Marker and title planting.** Anyone who can open or edit an issue in the
  repo can plant a finding's marker, or a near-identical title, on an OPEN
  issue and so suppress that finding's filing. Bounded: it only SKIPS a filing
  (nothing is closed or edited), and the mark — issue number and basis — is
  visible in the `upkeep_report_recorded` payload's `duplicates`. Whether a
  marker match needs a provenance check (e.g. issue author is the App) is
  #3924's decision.
- **Open-window miss.** `intakeCandidates` enumerates newest-first with closed
  items included, capped at `intakegroom.DefaultMaxScanned`. Closed items
  consume part of the window, so an older OPEN duplicate past the cap is not
  seen. `WindowTruncated` makes this visible.
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
- **Preview drift.** `/prompt-render` re-gathers; a run landing or the budget
  expiring between it and the signed serve can make the two differ.
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
