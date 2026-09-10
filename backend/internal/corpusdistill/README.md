# backend/internal/corpusdistill

Corpus-case distiller: scaffolds an agent-eval corpus case from a captured trace bundle (#1290), with inline labeling + dry-run (#1291). The plan-review-miss sibling corpus feed (`planreviewmiss.go`) is documented in `docs/architecture/agent-eval.md` §Plan-review-miss corpus.

## Distill / Preview

- `Distill(r io.Reader, Options) (caseDir, err)` parses a trace bundle (gzipped `.jsonl.gz` OR plain `.jsonl`, auto-detected by the gzip magic `0x1f 0x8b`) via `bundle.ReadEvents`, scores it with `agenteval.Score`, and writes the three-file corpus case (`trace.jsonl` plain + `expected.json` + a `Provenance: PRODUCTION` `case.md` template) under `OutDir/CaseName`.
- `FetchStageTrace` (in `fetch.go`) GETs the redacted bundle from `GET /v0/stages/{stage_id}/trace` with `Authorization: Bearer`.
- `Preview(r io.Reader, Options) (Result, err)` is the pure no-write entrypoint backing `--dry-run`: it shares all parse/score/render logic with `Distill` (both delegate to `prepare`) but touches no filesystem, returning the would-be artifacts (`CaseDir`/`ExpectedJSON`/`CaseMD`/`Card`).

## The `fishhawk-distill-corpus` command

The standalone command `backend/cmd/fishhawk-distill-corpus` (a dev/operator tool kept out of the `fishhawkd` server binary) drives it.

- Flags: `--in` / `--stage-id` / `--case-name` / `--issue` / `--out-dir` / `--force` / `--backend-url` (env `FISHHAWK_BACKEND_URL`) / `--token` (env `FISHHAWK_TOKEN`) / `--signal` / `--narrative` / `--dry-run`.
- The default `--out-dir` (`backend/internal/agenteval/testdata/corpus`) fails loud unless run from the repo root.
- The inline-labeling flags `--signal`/`--narrative` pre-fill the `case.md` distilled-signal sections (omitted → the #1290 `TODO(operator)` template, byte-for-byte unchanged).
- `--dry-run` scores + prints the would-be case without writing any file (exit 0 on preview, 1 on a genuine error).

Automates the mechanical half of the #819 corpus buildout so the operator can add + label + select in one workflow; case selection stays operator curation.

## `--severity-calibration` mode (E50.22 / #3309)

`DistillSeverityCalibration` / `PreviewSeverityCalibration` scaffold ONE candidate severity-calibration case (`case.json` + `case.md`) for `backend/internal/agenteval`'s pre-/post-#2119 two-arm comparison, by joining a run's `implement_reviewed` concerns to the concern dispositions the operator recorded on them. `FetchRunConcernDispositions` (in `fetch.go`) is the `--run-id` source.

### One paged request PER category, merged ascending by sequence

The audit endpoint filters by exactly ONE category per request (`server/reads.go` `handleListRunAudit` reads a single `category` value and dispatches to `ListForRunByCategory`), so `FetchRunConcernDispositions` issues four paged requests — `implement_reviewed`, `concern_waived`, `concern_deferred`, `concern_addressed_by_condition` — follows `next_cursor` on each, and merges the results **ascending by sequence** before returning. Assuming a comma-separated or repeated `category` parameter works would silently return one category's entries and look like an empty run. A non-200 on ANY category is an error carrying the status and a body snippet, the same shape `FetchStageTrace` and `FetchRunTriageAudit` use.

The ascending merge is load-bearing, not cosmetic: the join consumes the `implement_reviewed` note catalogue in sequence order, so a disposition must never precede the verdict that raised it merely because its category was fetched first.

### THE JOIN, and where the plan's premise was wrong

Verified in the tree rather than taken on report:

- `concern_waived` and `concern_deferred` payloads DO carry `{concern_id, prior_state, reason, stage_kind, severity, category}` (`server/waive.go` `applyConcernWaive`, `server/defer_concern.go`).
- The `implement_reviewed` payload does **NOT** carry a `concern_id`. `planreview.Concern` is `{severity, category, note, suggested_patch, provenance}` — the concern-store UUID is assigned when the verdict is PERSISTED, and no audit category records that assignment (there is no `concern_recorded` category). So "join the review concerns to the dispositions BY `concern_id`" is not implementable as stated: the review side has no such key.
- `concern_addressed_by_condition` carries neither `severity` nor `category` (`server/condition_claims.go`): its payload is `{concern_id, prior_state, approval_sequence, approver_subject, confirming_review_sequence, reviewer_model, verdict, confirming_review_qualified}`.

So the join actually implemented is: **the disposition entries are the spine** — they supply `concern_id`, the disposition and the disposition reason — and **the `implement_reviewed` concerns are a NOTE CATALOGUE matched ONE-TO-ONE by `(severity, category)` AND BY CHRONOLOGY**, consumed in ascending sequence order.

**Chronology is a second key, not a consequence of the ordering.** Sorting ascending guarantees the catalogue is COMPLETE before the first disposition consumes from it — and completeness is exactly what lets a disposition at sequence 20 reach FORWARD and consume the sole `(severity, category)` match at sequence 30. A review recorded AFTER a disposition cannot have originated it, so a catalogue entry whose sequence exceeds the disposition's is not a candidate. On the `--from-run` path a disposition is always preceded by its review, so this refuses nothing that path produces; it bites on `--in` / stdin, where the caller hands over an arbitrary slice of a run's audit chain and a truncated window can leave a disposition with only a later review to match against.

### Fail-loud modes

Mirroring `DistillPlanReviewMiss`, and each asserted by a test that reads the OUTPUT DIRECTORY after the call returns (a control that fires and is then rolled back returns a byte-identical error, so error identity alone would not distinguish a refusal from a write-then-error):

| Mode | Behavior |
|---|---|
| undecodable payload | error naming the item's `sequence`; nothing written |
| disposition with no `concern_id` | error; the disposition cannot be attributed |
| **orphan** — no unconsumed catalogue concern matches `(severity, category)` | error naming the `concern_id`; nothing written |
| **chronologically impossible** — the only unconsumed `(severity, category)` matches are at LATER sequences than the disposition | error naming the disposition's sequence, the `concern_id` and the later sequences, pointing at widening the audit window; nothing written |
| **ambiguous** — MORE THAN ONE unconsumed catalogue concern matches | error naming the `concern_id` and the count; nothing written |
| zero joined concerns | error, never an empty success |
| existing case dir without `--force` | refusal |

Ambiguity fails loud rather than guessing because the audit chain carries no concern id on a review verdict: when two unconsumed review concerns share `(severity, category)`, nothing can say which reviewer note belongs to this `concern_id`, and attributing the wrong prose would silently corrupt a LABELLED corpus. Such a run is curated by hand.

A `concern_addressed_by_condition` disposition is therefore **UNJOINABLE** from the audit chain. It is neither dropped nor guessed: it is listed in `case.md` by `concern_id` for the operator to add by hand, and contributes no labelled concern. A run whose dispositions are ALL unjoinable is the zero-joined-concerns error, naming the ids.

### `operator_severity` is left EMPTY, on purpose

The tool cannot know the severity the operator judged the concern to be worth, so it does not invent one. `agenteval`'s loader mode (g) REFUSES a case with an empty or unrecognised `operator_severity`, so the tool's unedited output cannot silently join the corpus — that is what makes "labelled" mean something, and it is asserted end to end by `TestDistillSeverityCalibration_UnlabelledOutputIsRefusedByLoader`. A distilled case carries `synthetic: false`; the committed hand-authored seed fixtures carry `synthetic: true`.

No audit payload carries the reviewed diff either, so `--diff <path>` supplies it (and `--plan-summary <path>` the approved-plan summary). A named-but-unreadable path is an ERROR, never a silent diff-less case. A candidate written without a diff carries a `TODO(operator): supply the diff` block, because the loader refuses an empty diff.

### FREE TEXT — this surface makes NO redaction claim

Unlike the `--plan-review-miss` mode, whose `--run-id` fetch reads structured verdict fields only and can assert the ADR-049 #5 redacted-by-construction provenance, **this mode's payloads carry free-text prose however they were sourced.** The concern `note` is REVIEWER prose and the `disposition_reason` is OPERATOR prose; either can carry tokens, hostnames, internal paths or customer detail regardless of the structured field it travels in, and no reusable free-text redactor exists in this repository to route them through (`backend/internal/diagnostics` takes a structured-fields-only posture — `ClassifyFailureDetail` maps a failure reason to a CLASS rather than scrubbing prose).

So the generated `case.md` renders, **at the point of use**, a `TODO(operator): REVIEW FREE TEXT BEFORE COMMITTING` block naming both fields, stating that the case is committed to the repository, and requiring the operator to read them before committing. `TestDistillSeverityCalibration_CaseMarkdownWarnsAboutFreeText` asserts that block is present in the WRITTEN `case.md` and that the string `redacted-by-construction` appears nowhere in the written output. Do not reintroduce the phrase on this surface.

### CLI

```sh
# From the repo root; --run-id fetches all four categories, all pages.
fishhawk-distill-corpus --severity-calibration --run-id <uuid> \
  --diff implement.patch --plan-summary plan.md \
  --case-name my-cal --issue '#3309'

# Operator-supplied items (a JSON array, or the {items:[...]} envelope):
fishhawk-distill-corpus --severity-calibration --in items.json \
  --case-name my-cal --issue '#3309' --dry-run
```

`--severity-calibration` and `--plan-review-miss` are mutually exclusive; `--stage-id` is the trace mode's source and is refused in both; `--diff` / `--plan-summary` apply only to `--severity-calibration`; `--run-id` applies to both audit modes. The default `--out-dir` for this mode is `backend/internal/agenteval/testdata/severity-calibration-corpus`, sharing the same fail-loud repo-root parent check.
