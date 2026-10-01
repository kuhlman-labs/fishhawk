# `backend/internal/precedent`

The ADR-082 (#3728) decision (b) **precedent query**, pure half (E75.3 / #3731):
given a decision context, rank prior decisions of the same class from the SAME
repository, and summarise the agreement of the returned set.

Surfaces: `GET /v0/precedent` (`backend/internal/server/precedent.go`) and the
`fishhawk_precedent` MCP tool (`backend/internal/mcpserver/precedent.go`).
Wire contract: `docs/api/v0.md` § "Precedent query".

## What this package is NOT

- **Not authority.** A precedent item is evidence that a human or a delegation
  rule already weighed something similar. It does not decide, does not lower a
  gate, and confers no permission. Every surface says so, and nothing in the
  tree reads a precedent result as an input to an admission check.
- **Not a model call.** No embeddings, no LLM, no network. Scoring is arithmetic
  over set overlap, so the same inputs give the same answer forever.
- **Not a writer.** `Rank` takes rows and returns items. The query mints no
  audit entry; the chain is untouched.

## Purity, and why it is load-bearing

The package imports `backend/internal/decisionindex` (for the row model and the
canonical concern-category vocabulary) and the standard library. No database
handle, no clock, no HTTP. That is what makes the ranking byte-reproducible and
lets every component be tested in isolation — `precedent_test.go` needs no
fixture beyond a struct literal.

## The score

`score.total` is a weighted sum in `[0, 1]`. Each component is reported as its
WEIGHTED contribution, so a caller reading the response can add them up.

| Signal | Weight | Similarity |
|---|---|---|
| `touched_paths` | `WeightTouchedPaths` = 0.45 | Jaccard over path-PREFIX sets |
| `concern_category` | `WeightConcernCategory` = 0.25 | exact match on the normalized canonical key |
| `escalation_keys` | `WeightEscalationKeys` = 0.20 | Jaccard over fired escalation rule keys |
| `severity` | `WeightSeverity` = 0.10 | exact match |

### Why each weight is where it is

The weights are **chosen, not fitted** — there is no corpus of precedent queries
to tune them against yet. They are exported named constants so a later
calibration is a value change with a test, not a rewrite, and the determinism
guarantee below is independent of their values.

- **Touched paths dominate** because WHERE a decision was made is the strongest
  available proxy for whether it is about the same thing. Prefix expansion is
  what makes it a gradient rather than a coin flip: each path contributes itself
  plus every directory prefix, so a decision on
  `backend/internal/server/foo.go` matches one on
  `backend/internal/server/bar.go` through the three shared prefixes — a real
  score, strictly below an exact hit.
- **Escalation keys come next** because a shared fired key means the two
  decisions were made under the SAME governing rule. That is a stronger claim
  about similarity than any free-text field.
- **Concern category** is lower because the canonical vocabulary is coarse (six
  keys), so a match is weaker evidence than it looks.
- **Severity is weakest**: it co-varies with category, and a bare severity match
  carries little on its own.

### Determinism

`Rank` sorts by `score` DESC, then `decided_at` DESC (recency), then
`source_sequence` DESC. Source sequence is `decision_index`'s primary key, so no
two rows can compare equal: the ordering is TOTAL and the output cannot depend
on the order the store returned candidates in.
`TestRank_DeterministicUnderInputReordering` ranks the same set twice, the second
time reversed, and requires byte-identical marshalled output — with the
tie-breaks removed, `sort.SliceStable` preserves the incoming order and the
reversed arm inverts, so the test is RED.

### `NaN` is refused, not tolerated

An empty Jaccard union returns exactly `0`. A bare `len(inter)/len(union)`
division yields `NaN` on two empty sets, and every comparison against `NaN` is
false — so a `NaN` score slips past every threshold comparison silently. This is
the same rule `backend/internal/intakegroom/duplicate.go` records.

### `hard_filter_only`

A row that matched the repository, class and stage kind and NOTHING else reports
`hard_filter_only: true`, with zeroed components and an empty matched-key set —
ADR-082 rule 3's "reports that" requirement. It is returned rather than hidden,
because "there is prior practice here but none of it resembles your case" is an
answer, and `summary.hard_filter_only` lets a caller weigh it.

### The defensive hard filter

`Rank` RE-ASSERTS the repo / class / stage-kind filter the store is expected to
have applied. It is defence in depth: the scorer structurally cannot emit a
cross-repository or cross-class item even if a caller builds the `ListFilter`
wrongly. The CONTROL for repository isolation is the filter the store receives
(asserted in `backend/internal/server/precedent_test.go` on the recorded
`ListFilter.Repo`); this re-assertion has its own separate test.

### Bounded explanations

Each matched-key list is capped at `MaxMatchedKeys` (20) with its untruncated
total reported. A caller may supply thousands of paths, and the prefix
intersection is `O(paths x depth)`; capping keeps one item's EXPLANATION from
dominating the response. It never affects the score.

## `Summary` — what E75.5 consumes

`Summarize` describes the RETURNED items, not the whole index.

- `agreement_ratio` is the **modal outcome's share** of the returned items: 1.0
  for a unanimous set, 0 for an empty one. `modal_outcome` names that outcome
  alongside it, so the number is never uninterpretable. Any other definition
  (pairwise agreement, entropy) would be equally defensible — this one is
  STATED, here and in the OpenAPI description, so E75.5's divergence threshold
  consumes a documented number rather than inferring one. A count tie breaks to
  the lexicographically smallest outcome, so the field is deterministic.
- `human` / `delegated` split the returned items on `Item.Delegated`.
- `doctrine_versions` is the sorted set spanned; size > 1 means the precedent
  was set under more than one charter revision.
- `count` is the number of items DESCRIBED, which is the number RETURNED — so a
  summary never describes rows the caller cannot see.

## `IndexVersion` and `Fingerprint` (E75.4 / #3732)

`IndexVersion` names the RANKING CONTRACT: the weight set plus `Rank`'s total
ordering (score DESC, decided_at DESC, source_sequence DESC). **Bump it whenever
a weight or the ordering rule changes.** It is recorded on every
`precedent_surfaced` entry and folded into `Fingerprint`, so a gate re-surfaced
under recalibrated weights records as NEW rather than de-duplicating against an
entry whose scores came from the old ones. An explanation-only change (e.g.
`MaxMatchedKeys`) leaves every score and position unchanged and needs no bump.

`Fingerprint(class, stageID, items, summary)` is the de-duplication key of one
surfaced gate block: sha256 over `IndexVersion`, the class, the stage id, the
ORDERED `(source_sequence, source_entry_hash, score total)` triples, the modal
outcome and the agreement ratio, floats rounded to 6 places so formatting cannot
mint a new key. It **excludes `ReasonExcerpt`**: excerpts are read from the chain
at query time and can degrade while the ranked answer is unchanged, and a key
over them would re-record an unchanged precedent on every chain-read hiccup. The
other excluded `Item` fields are pure functions of the cited row, which the entry
hash already pins. Pinned by the four `TestFingerprint_*` tests.

## The one sanctioned agent-facing consumer

ADR-082 ([#3728](https://github.com/kuhlman-labs/fishhawk/issues/3728)) rule 6
makes precedent **captain-facing in alpha**: it does not go to reviewer or
planner prompts. Decision (e) option 3 carves out exactly ONE exception —
agents, later, **only through ADR-081 consults, structured fields only** — and
ADR-081 ([#3727](https://github.com/kuhlman-labs/fishhawk/issues/3727)) rule 8
sequences it.

That exception is `backend/internal/server/crew_historian.go` (E77.8 /
[#3742](https://github.com/kuhlman-labs/fishhawk/issues/3742)), and it is the
only one. It ranks with this package and renders through an explicit allow-list
projection (`projectHistorianItem` → `historianItem`) that carries class,
outcome, reject class, decided date, delegated, actor kind, doctrine version,
concern category, severity, capped matched keys, the score total and the
`(source_sequence, source_entry_hash)` citation — **and no free-text prose from
another run**. `Item.ReasonExcerpt` and `Item.ReasonKey` are deliberately NOT
projected, and the responder holds no audit repository, so it cannot read the
chain the excerpt comes from in the first place.

**What that means when this package changes.** A new `Item` field is NOT
automatically agent-visible: it reaches a planner only by being named in that
projection, and `TestProjectHistorianItem_FieldSetIsClosed` fails when the
projected set drifts. If you add a PROSE field here, leave it out of the
projection — adding it would carry another run's reasoning into a planner's
prompt, which is exactly what rule 6 protects against and what decision (e)
narrowed the exception to avoid.

## Divergence threshold (`divergence.go`, E75.5 / #3733)

ADR-082 decision (d) and rule 5: when a captain's decision at an allow-listed
gate goes AGAINST clear precedent, the server records a `precedent_divergence`
entry (backend/internal/server § "Divergence at the gate"). This file is the
pure rule; it holds no clock (`now` is a parameter), no DB and no HTTP.

**Shipped disabled.** The zero `DivergenceConfig` and `DefaultDivergenceConfig()`
both have `Enabled: false`; the default's numbers (N=5, X=0.8, 180-day window)
are placeholders a tuning report replaces with evidence.

**Closed allow-list.** `concern_waive`, `concern_defer`, and `plan_approval`
ONLY on a `reject` outcome (`ClassAllowed`). The reject restriction keeps the
rule off the approve-dominated plan-gate base rate. Config may NARROW the set
(`AllowedClasses`) but never widen it: `NewDivergenceConfig` refuses an
unrecognised class with `ErrUnknownDivergenceClass` and an out-of-range threshold
with `ErrInvalidDivergenceConfig`. The keys are `decisionindex` constants, so a
renamed class is a compile break, not a silent no-fire.

**Precedent classes.** Each concern class's own outcome is constant
(`waived` / `deferred`), so a waive compared only against prior waives could
never diverge. `ComparisonClasses` therefore compares the two concern classes
against the UNION of both (two answers to one question: what to do with an open
concern); `plan_approval` is compared against itself.

**`Decide` — four conditions, first unmet one is the reason:**

1. at least N **human** items (`!Item.Delegated`; a delegated decision counts
   toward neither N nor the agreement denominator) → else `below_min_decisions`;
2. at least N of those with `DecidedAt >= now - Window` (inclusive edge) → else
   `outside_window`;
3. at least N of those under the deciding run's doctrine version → else
   `doctrine_version_mismatch`;
4. `Summarize` of exactly that set has `agreement_ratio >= X` (the modal-share
   definition above) → else `below_min_agreement`.

With clear precedent the verdict is `diverged` when the captain's outcome
differs from the modal outcome and `agreed_with_precedent` otherwise. The doctrine
version is `runs.workflow_sha` today (`decisionindex.Row.DoctrineVersion`), which
is NARROWER than a charter revision — any workflow-spec edit resets the window.
That makes the rule quieter, never louder.

`ParseWindow` accepts a Go duration or a whole number of days (`180d`) and
refuses zero, negative and malformed values. Pinned by `divergence_test.go`, one
isolating fixture per condition.

## Issue history

- #3742 (E77.8) — the historian consult responder, the one sanctioned agent-facing consumer.
- #3733 (E75.5) — the divergence threshold rule and closed allow-list.
- #3732 (E75.4) — `IndexVersion` + `Fingerprint` for the gate-open precedent block.
- #3731 (E75.3) — this package, the REST route and the MCP tool.
- #3730 (E75.2) — `decision_index`, the rows this ranks.
- #3728 (ADR-082) — decision (b), the precedent query's charter.
