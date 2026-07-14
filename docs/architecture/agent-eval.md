# Agent eval harness (v0)

Status: v0 — Tier-A deterministic offline scorer **plus** the Tier-B
LLM-as-judge (judge + dimensions + calibration harness, #820). The judge
runs offline-by-default against a mockable model seam; a live model call
is opt-in only. Capturing a REAL labeled corpus and the live
scheduled/advisory runner remain deferred (see
[Deferred](#deferred-to-follow-up)). Originating issues: #652 (Tier A),
#820 (Tier B).

## What this is

A deterministic, offline trajectory scorer. Given an already-captured
trace bundle, it computes a fixed set of Tier-A signals about *how* the
agent behaved on a run — task outcome, tool-selection sequence,
retry/loop count, whether it inspected the tree before editing, and
whether it respected its boundaries. No live model is in the loop; every
signal is read straight from the captured trajectory, so the score is
reproducible and cheap.

Code: `backend/internal/agenteval/scorer.go`. It lives in the backend Go
module so it reuses `backend/internal/bundle` directly with no
cross-module import.

## Tiers

- **Tier A (this v0): deterministic scoring over captured trajectories.**
  Pure functions over a parsed bundle. Offline, no model calls, no
  network. This is what ships now.
- **Tier B (this follow-up, #820): LLM-as-judge over trajectories.** A
  model scores three dimensions a deterministic pass can't derive —
  meaningful evidence-inspection, honest reporting of uncertainty/repair,
  and reasoning soundness. The judge ships with a calibration harness
  that scores its output against human labels; the seed labels are
  synthetic, exactly as the Tier-A seed corpus is, so a REAL labeled
  corpus remains the deferred prerequisite for *trusting* the judge in
  production. Code: `backend/internal/agenteval/judge.go` +
  `calibration.go`.

## Tier B: the LLM-as-judge

`Judge.Judge(ctx, lines []bundle.Line) (JudgeCard, error)` scores one
parsed trajectory on three ordinal (1-5) dimensions, each with a model
rationale:

| Dimension (`JudgeCard` field) | Deepens which Tier-A signal | Meaning |
|---|---|---|
| `MeaningfulEvidence` | `EvidenceBeforeEdit` | not merely "a read preceded the first write" but "was the inspection substantive enough to ground the edit" |
| `HonestUncertainty` | (none) | did the agent report uncertainty, partial results, and repair honestly rather than over-claim completeness |
| `ReasoningQuality` | loop/retry counters | was the overall approach sound, or merely non-looping |

`JudgeCard` carries the three `DimensionScore{Score int, Rationale
string}` values plus the `Model` name reported by the sender.

### Offline-by-default, mockable (the `MessageSender` seam)

The judge depends ONLY on a local interface, NOT the Anthropic SDK:

```go
type MessageSender interface {
    Messages(ctx context.Context, systemText, userText string) (responseText, modelName string, inputTokens, outputTokens int, err error)
}
```

This signature is exactly `anthropic.Client.Messages`
(`backend/internal/anthropic/client.go:52`), so an `*anthropic.Client`
satisfies it **without** `agenteval` importing the `anthropic` package or
the SDK. The production package therefore stays offline-by-default and
fully mockable; the live wiring (`anthropic.NewClient`) lives only in the
opt-in test path. The fixed dimension instructions ride in `systemText`
(cache-eligible) and the rendered trajectory in `userText`, matching the
`Messages` system/user split.

### Error-not-fail-open (DISTINCT from Tier-A)

Tier-A's `Score` is fail-open: signal drift degrades to a benign zero
value. The judge is the opposite. A judge call that ultimately fails
returns a **non-nil error and the zero `JudgeCard`**, NEVER a fabricated
zero-score card presented as a real verdict — a fake score would
silently corrupt calibration. A malformed / out-of-range / missing-
dimension response is re-rolled up to `maxRetries` (mirroring
`planreview.DecodeVerdictRetrying`); a sender transport error is returned
immediately and unchanged (the adapter owns its own crash-retry). Callers
MUST check the error before reading the card.

The judge model defaults to `claude-sonnet-4-6` (`DefaultJudgeModel`) and
is a `NewLLMJudge` parameter — a documented default, not a hardcoded gate.

### Calibration harness + within-1 agreement

`Calibrate(ctx, judge, cases []CalibrationCase, threshold float64)
(CalibrationReport, error)` scores the judge against human-labeled cases
and gates a `Trusted` verdict. The metric is **within-1 agreement**: per
dimension, the fraction of cases where `|judgeScore - humanScore| <= 1`
(the report also reports exact-match rate). `OverallWithin1` is the
within-1 rate across all `SampleCount * 3` dimension-comparisons, and
`Trusted = OverallWithin1 >= threshold`. The threshold is a **configurable
parameter, NOT a hardcoded CI gate** — it stays tunable until a real
labeled corpus lands. A case whose judge call errors is **fail-closed**:
it stays in the denominator and counts as a full disagreement, never
silently dropped, so a flaky judge cannot pass calibration by attrition.

### The human labels are SYNTHETIC

Each corpus case carries a `human_labels.json` with hand-assigned 1-5
scores per dimension, `Synthetic: true`, and a `notes` field explaining
each score. These are **bootstrap labels**, exactly as the Tier-A seed
traces are synthetic — they prove the calibration mechanism and
discrimination, not real-world judge accuracy. The negative cases
(`618-wire-regression`, `loop-failure-out-of-tree`) get low scores; the
positive control (`healthy-cross-boundary`) gets high scores. Replacing
them with REAL captured + labeled production traces is the deferred
follow-up below.

### Opt-in live calibration (CI never calls a model)

The default test suite uses a `fakeSender` / stub `Judge`, so the
committed-tree verify and CI make **no** live model call. A separate
`TestCalibrateLive` constructs an `*anthropic.Client` and runs the real
judge over the corpus, but it `t.Skip`s unless BOTH
`FISHHAWK_AGENTEVAL_JUDGE_LIVE` and `FISHHAWKD_ANTHROPIC_API_KEY` are set.
Passing the `*anthropic.Client` to `NewLLMJudge` is also the compile-time
proof the SDK adapter's signature has not drifted from `MessageSender`.

## Signals (the Scorecard)

`Score(lines []bundle.Line) Scorecard` takes already-parsed bundle lines
(gzip → lines is `bundle.ReadEvents`; the scorer stays decoupled from
the wire framing so fixtures can be committed as reviewable plain
`.jsonl`). Every extractor is **fail-open**: stream-json or event-kind
drift degrades to the benign zero value, never a panic.

| Field | Source | Meaning |
|---|---|---|
| `Outcome` | manifest `agent_failed` → `git_diff` presence | `agent_failed` / `diff_produced` / `no_diff` |
| `ToolSequence` | `tool_use` blocks in `assistant` lines, in order | the tool-selection trajectory |
| `ToolCalls` | `len(ToolSequence)` | total tool_use count |
| `UnnecessaryRetries` | count of `agent_retry` events | self-retries (no-progress signal) |
| `LoopDetected` | any `loop_detected` event | the loop detector tripped |
| `EvidenceBeforeEdit` | order of read-class vs file-writing tool_use | true iff a `Read`/`Grep`/`Glob` precedes the FIRST `Write`/`Edit`/`MultiEdit`/`NotebookEdit` |
| `OutOfTreeWrites` | `out_of_tree_write` event paths | boundary violation (#601 class) |
| `ScopeDriftPaths` | `scope_drift` `policy_event` `undeclared` | paths touched but absent from the scoped diff |

### Where the shapes come from

The tool_use extraction mirrors
`runner/internal/agent/claudecode/claudecode.go` `toolCallSignatures`
(`:751`): tool_use blocks ride in `assistant` stream-json lines whose
`message.content[]` entries carry `type=="tool_use"` and a `name`. The
file-writing set (`Write`/`Edit`/`MultiEdit`/`NotebookEdit`) mirrors the
`fileWritingTools` map (`:49`). The non-tool signals read dedicated event
kinds emitted by the runner: `agent_retry` (`:183`), `out_of_tree_write`
(`:453`), `loop_detected` (`:474`), and the `scope_drift` `policy_event`.
The scope-drift lookup is replicated inline rather than calling
`bundle.ExtractScopeDrift`, which takes the gzip `[]byte`, not parsed
lines.

### Known gap (inherited, v0 acceptance)

Bash-mediated writes (shell `>` redirects) are invisible to tool-layer
detection — only `Write`/`Edit`-tool writes are seen. The scorer
inherits this limitation from the runner's tool-layer detector; a
Bash-written file is not counted as an edit. This is a v0 acceptance,
not a bug; full confinement needs an OS-level sandbox (the deferred
agent-filesystem-confinement work).

## The corpus and the data flywheel

Seed cases live under `backend/internal/agenteval/testdata/corpus/<case>/`:

- `trace.jsonl` — the preserved raw/full trajectory, one bundle line per
  line, committed as plain (non-gzip) JSONL for review-ability.
- `expected.json` — the asserted `Scorecard`.
- `case.md` — annotates the originating issue, the distilled signal, and
  why it is (or isn't) a regression.
- `human_labels.json` — the SYNTHETIC Tier-B human labels (per-dimension
  1-5 scores, `synthetic: true`, `notes`) the calibration harness scores
  the judge against.

The discipline — **keep the messy original beside the distilled
fixture** — is deliberate: the raw trajectory is what lets a future
reviewer re-derive or re-score the case when the signal set grows. The
flywheel is: capture a real failure → preserve its raw trace →
annotate the distilled signal in `case.md` → assert the scorecard in
`expected.json` → the table test (`scorer_test.go` `TestScore`) replays
it forever as a regression guard.

### Discrimination, not just green

The corpus proves the harness *discriminates*, not merely that it runs.
The negative case `618-wire-regression` asserts the regression signals
(`evidence_before_edit=false` + a non-empty `scope_drift_paths`); the
control `healthy-cross-boundary` asserts the opposite
(`evidence_before_edit=true`, non-empty `tool_sequence`, no boundary
signals). A scorer that extracted *nothing* would fail the healthy
control — the anti-vacuous-green guard — and a scorer that missed the
regression would fail the negative case. This is the deterministic
analogue of #652's "introduce a known-bad edit, confirm the suite
catches it" acceptance test.

The third negative case `loop-failure-out-of-tree` closes the remaining
coverage gap: it asserts the **non-zero** branch of every signal the
first two cases leave at its zero value — `outcome=agent_failed`,
`unnecessary_retries=1`, `loop_detected=true`, and a non-empty
`out_of_tree_writes` (which exercises the `out_of_tree_write` payload
unmarshal + path-extraction path no other case reaches). Without it a
scorer that silently dropped any of those signals would still pass the
suite.

### The seed cases are SYNTHETIC

All seed fixtures are **hand-authored reconstructions** of the #618 /
healthy / failure-mode *shapes*, not byte-for-byte captures from
production runs. No
labeled corpus exists yet; this is a v0 bootstrap to prove the
mechanism + discrimination. The byte shapes mirror the real Claude Code
stream-json + bundle event-kind wire format so the scorer exercises the
real parse path, but the trajectories themselves are invented. Each
`case.md` states this plainly.

## Plan-review-miss corpus (E31.11 / #1539)

A second, sibling corpus under
`backend/internal/agenteval/testdata/planreview-miss-corpus/<case>/`
closes the ADR-049 decision #4 feedback loop: when acceptance triage
(E31.8) concludes **class 3** — a failed criterion that is
inferred-source or unresolvable against the approved plan, i.e. a bad
criterion the plan gate approved — that decision is a *plan-review
miss*, and the corpus accumulates the cases the plan reviewer should
have challenged.

The feed pipeline:

1. **Class-3 audit record.** `triageAcceptanceFailure`
   (`backend/internal/server/acceptance.go`) enriches the class-3
   `acceptance_triage_decided` payload with an additive
   `plan_review_miss` field: one `agenteval.PlanReviewMiss` per class-3
   criterion id, joining the plan criterion's provenance
   (`statement`/`source`/`source_ref`/`rationale`) with the verdict's
   observed behavior
   (`observed`/`expected`/`steps_taken`/`expectation_basis`/
   `repro_handle`/`result`). An unresolvable id still yields a record
   keyed by the id with empty provenance. Omitted for classes 1/2/4/5,
   so existing consumers are untouched. The payload is built by
   `server/acceptance.go::buildPlanReviewMisses`, threaded through
   `writeAcceptanceTriageAudit`.
2. **Distill.** `fishhawk-distill-corpus --plan-review-miss` (backed by
   `corpusdistill.DistillPlanReviewMiss` / `PreviewPlanReviewMiss` and
   `FetchRunTriageAudit`, which follows `next_cursor` pages past the
   audit endpoint's 500-entry limit cap) scaffolds one candidate case
   per class-3 decision under the corpus dir: `miss.json` (the
   `agenteval.PlanReviewMissCase` shape) + a provenance `case.md`.
   Input is either `--run-id` (fetch from the backend, PRODUCTION
   redacted-by-construction provenance) or `--in`/stdin (TODO
   provenance). The default out-dir is
   `backend/internal/agenteval/testdata/planreview-miss-corpus/`
   (committed synthetic seed: `seed-synthetic-inferred-criterion`).
3. **Operator-labeled case.** Selection, labeling, and committing stay
   operator curation (#819 / ADR-040); the tool produces a CANDIDATE
   with a TODO(operator) distilled-signal prompt (or an inline
   `--narrative`).

The `miss.json` shape is `agenteval.PlanReviewMissCase`
(`planreviewmiss.go`): the triage envelope (`run_id`, `artifact_id`,
`triage_sequence`, `class`, `disposition`, `reason`, `decided_at`), the
`misses` array, and a `synthetic` marker.
`LoadPlanReviewMissCorpus(dir)` reads the corpus back — empty slice on
an absent dir, fail-closed (error naming the case) on malformed JSON, a
missing `miss.json`, an empty `misses` list, or an empty
`criterion_id`. The **same** `PlanReviewMiss` type is used by the
server marshal, the tool unmarshal, and the loader, so the three
surfaces cannot drift (pinned by a corpusdistill round-trip seam test).
The wire types (`PlanReviewMiss`, `PlanReviewMissCase`,
`LoadPlanReviewMissCorpus` in
`backend/internal/agenteval/planreviewmiss.go`) are stdlib-only so the
server can import agenteval cycle-free.

**Redaction posture:** the feed is redacted-by-construction. Per
ADR-049 decision refinement #5, evidence blobs (logs, screenshots,
traces) stay customer-side — only the structured verdict + per-criterion
prose fields cross to Fishhawk — so a distilled candidate carries no raw
evidence. `case.md` still requires the operator to confirm the prose
fields before a case lands. The committed
`seed-synthetic-inferred-criterion` case is a hand-authored shape
demonstration (`synthetic: true`), per the same synthetic-seed
discipline as the Tier-A/Tier-B seeds.

The queryable metric lives on the API: `GET /v0/acceptance-triage/stats`
(`backend/internal/server/acceptance_stats.go`, registered next to
`/v0/calibration`) aggregates `acceptance_triage_decided` entries by
class/disposition and reports `plan_review_miss_rate` (class-3
decisions / all triage decisions, per DECISION not per run;
undecodable payloads count under class `""` so the denominator never
shrinks; 0 samples → rate 0). It takes `since`/`workflow_id` filters,
returns `503 acceptance_stats_unconfigured` without an audit repo, and
requires no new scope — see `docs/api/v0.md`.

## Acceptance triage class 5 (externally-unvalidatable, #1671)

`classifyAcceptanceFailure`
(`backend/internal/server/acceptance.go`) splits the historical class-2
"no criterion failed but ≥1 was skipped" partition into two. The
acceptance agent runs under a default-deny egress sandbox against the
localhost preview only, so a criterion whose trigger requires an
external event it cannot produce (closing a GitHub issue, firing a
webhook) is correctly marked `skipped` with the reason in
`expectation_basis` (posture-A can't-exhibit, #1612).

- **Class 2 (bounded flake retry, unchanged).** An all-skip verdict
  where at least one skipped criterion LACKS `expectation_basis` is
  genuinely ambiguous → re-open the acceptance stage and re-run, bounded
  by `defaultMaxAcceptanceReruns`.
- **Class 5 (terminal externally-unvalidatable page).** An all-skip
  verdict where EVERY skipped criterion carries a non-empty
  `expectation_basis` → the disposition
  `externally_unvalidatable_paged`, which takes **no state transition**.
  The acceptance stage stays `succeeded`/terminal so
  `fishhawk_audit_complete` can clear and the operator arbitrates via
  the normal gate. This removes the deterministically-futile class-2
  retry loop (the sandbox still cannot reach the external service) that
  otherwise wedged the merge gate. Class 5 never re-opens the stage, so
  it never contributes to the auto-routed re-run count.

`externally_unvalidatable_paged` is a paged-family disposition: it fires
the `must_page_human` anchor ping (`issuecomment/ping.go`) and routes
the MCP `next_actions` to the `acceptance_triage_paged` arbitration arm
(`cmd/fishhawk-mcp/next_actions.go`).

## Deferred to follow-up

These are surfaced for the operator to triage (the implementer does not
file the tracking issues). In priority order:

1. **Capture + label a REAL production trace.** The first task of the
   seed-set buildout: replace the synthetic seed fixtures — both the
   Tier-A `trace.jsonl`/`expected.json` and the Tier-B
   `human_labels.json` — with captured, labeled production traces, then
   grow the set toward the 30–50 target. The synthetic shortcut is a
   bootstrap only — a corpus that stays too clean tests nothing real, and
   the Tier-B judge cannot be *trusted* in production until it is
   calibrated against real human labels (the synthetic calibration proves
   the mechanism, not real accuracy).
2. **Live-model scheduled/advisory runner.** Running the eval (now
   including the Tier-B judge) on a cadence against live runs needs a
   scheduled GitHub workflow — a human-led `.github/workflows` change —
   and is out of scope here.

## Running it

```sh
scripts/test single -run TestScore ./backend/internal/agenteval/        # Tier-A corpus replay
scripts/test single -run 'TestJudge|TestCalibrat' ./backend/internal/agenteval/   # Tier-B judge + calibration

# Opt-in live judge calibration (makes a real model call; skipped otherwise):
FISHHAWK_AGENTEVAL_JUDGE_LIVE=1 FISHHAWKD_ANTHROPIC_API_KEY=... \
  scripts/test single -run TestCalibrateLive ./backend/internal/agenteval/
```

Runs under `scripts/test` automatically as a backend module test, so the
corpus replays in CI on every change. The default suite uses a
`fakeSender` / stub `Judge`, so CI never makes a live model call — only
the env-gated `TestCalibrateLive` does.
