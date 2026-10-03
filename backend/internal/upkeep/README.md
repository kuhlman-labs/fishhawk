# backend/internal/upkeep

Pure primitives of the upkeep scan (E79 / #3726): the hidden finding marker and
the open-issue dedupe an `upkeep_report_v1` ingest runs over its proposed
issues. Anchor: #3921 (contract + ingest + dedupe). #3922 extends this README
with the scan's detectors; #3924 (apply) consumes the duplicate marks.

The package imports no forge client, no `workmgmt`, no server package and not
`backend/internal/plan`'s report types. Its input is its own `Proposal`; the
server adapts a validated report into it. Nothing here files, closes, comments
on or relabels anything.

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
the check is repository-only.

Only a run ref's `run_id` is checked. Its `stage_id` and `trace_ref` are
AGENT-ASSERTED and UNVERIFIED: the ingest parses `stage_id` as a UUID but never
confirms the stage exists or belongs to the cited run, and never resolves
`trace_ref`. Treat both as the scanning agent's pointer, not as evidence the
server vouched for.

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
