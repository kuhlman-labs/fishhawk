# runner/internal/scenario

The replayable acceptance-scenario corpus (E72.4 / #3328): the YAML a
passing, drivable acceptance criterion is persisted as, the retirement
ledger beside it, the deterministic sampler that bounds a replay pass, and
the prompt section that asks the acceptance agent to replay prior scenarios
FIRST. Imports the standard library and `gopkg.in/yaml.v3` only — no
`runner/internal/upload` symbol and no backend symbol, because this package
PRODUCES the replay wire types and must build alone.

## Layout in the repository

```
acceptance/scenarios/                 CorpusDir
  issue-<N>/<criterion-id>.yaml       one Scenario per recorded criterion (PathFor)
  retired.yaml                        the retirement ledger (RetiredFile)
```

A scenario id is `scenario:issue-<N>/<criterion-id>` (`IDPrefix` +
issue + criterion). The prefix is what lets the backend partition verdict
rows into criterion rows and replayed-scenario rows.

## Scenario document

| field | meaning |
|---|---|
| `id` | `scenario:issue-<N>/<criterion-id>`; must carry `IDPrefix` |
| `statement`, `verify_hint` | copied from the approved criterion |
| `seed` | the E72.2 seeded-fixture scenario the criterion needs; omitted when none |
| `steps` | the passing row's `steps_taken`; an empty one records the literal `StepsNotRecorded` (`not recorded by the validator`), never an empty string |
| `assertions.expected`, `assertions.observed_at_record`, `assertions.repro_handle` | the passing row's `expected` / `observed` / `repro_handle` |
| `origin.issue`, `origin.pr`, `origin.run_id`, `origin.head_sha`, `origin.recorded_at` | attribution; `pr: 0` means UNKNOWN and is never substituted with the issue number |

`Load(dir)` is STRICT (`yaml.Decoder.KnownFields(true)`): an unknown field,
an id without the prefix, a missing `origin.run_id` or
`origin.recorded_at`, or undecodable YAML is a named error carrying the
file's corpus-relative path — never a silent skip. A missing directory is an
empty corpus. `Write(dir, s)` writes atomically (temp + rename) at
`PathFor(s.ID)` and refuses an id without the prefix or one carrying `..` /
a leading `/`.

Hand-authored YAML trap: a value carrying ` #` (a reason like
`behaviour replaced by #3327`) MUST be quoted, or YAML reads the tail as a
comment. `Write` / `WriteRetired` quote it themselves; only a hand edit can
lose it (`testdata/retired.yaml` shows the quoted form).

## Retirement ledger

`retired.yaml` is `retired: [ {id, reason, run_id, pr, retired_at} ]`.
`LoadRetired` returns an empty ledger for an absent file and a named error
for a malformed one (undecodable, unknown field, entry without `id`).
`MergeRetired(existing, incoming)` is idempotent on `id` and KEEPS the
existing entry whole on a duplicate id — the reason recorded first is the
reason that survives. `WriteRetired` is atomic.

## Sampling — `Sample(list, cap, seed)`

Keeps the `ceil(cap/2)` NEWEST scenarios by `origin.recorded_at` (ties by
id) and fills the rest from the OLDER pool with a `math/rand/v2` PCG seeded
from the FNV-64 of `seed` (the run id) — the same run always serves the same
set, another run rotates the older half, and input order never leaks in.
`cap <= 0` disables replay (nothing served). The returned `ReplaySet`
header carries `Cap`, `CorpusSize`, `Served`, `SampledOut` and `Seed`
computed ONCE, here; the caller fills `RetiredExcluded` and `Scenarios`
(`Entries(chosen)` builds the latter from the loaded files, the only
attribution source). The chosen list is newest-first.

## Composition — `Compose(criteria, results, origin)`

One `Scenario` per criterion that is `Drivable` (not `skip_expected`) AND
whose verdict row is `passed`. A skipped/failed/undecidable row, a criterion
with no row, and a non-drivable criterion are all excluded.

## Prompt section — `RenderPromptSection(chosen, timeCap)`

The `### Regression corpus` markdown the runner appends to the acceptance
prompt: replay-first instruction, one block per scenario (`scenario: <id>`,
statement, seed when set, steps, expected, `origin PR #n` or
`origin PR unknown`), the total time cap, and the rule that an unreached
scenario is reported `skipped` with `expectation_basis:
replay_budget_exhausted` (`BudgetExhaustedBasis`). Empty when nothing is
served.

## Wire types (CROSS-MODULE WIRE CONTRACT)

Three structs carry the marker; their backend twins and the
`backend/internal/wirecontract` manifest `Pair` rows (plus this file in
`CoveredFiles`) are registered by the slice that introduces the backend
side. Until then the marker-bearing structs are unregistered but outside
`CoveredFiles`, so `TestManifestCompleteness` does not sweep them.

| type | json keys | used as |
|---|---|---|
| `RetiredEntry` | `id, reason, run_id, pr, retired_at` | the `retired.yaml` row (yaml tags), the served `acceptance_retired_scenarios` element, the `acceptance_scenarios_pushed` report element — ONE type so the reason cannot be lost at a conversion boundary |
| `ReplayedScenario` | `scenario_id, origin_pr (omitempty), origin_issue, origin_run_id, path` | one element of `replay.scenarios`; `origin_pr` is ABSENT when unknown |
| `ReplaySet` | `cap, corpus_size, served, sampled_out, retired_excluded, seed, scenarios` | the top-level `replay` object the runner injects into the validated verdict body; no count is `omitempty`, so the backend copies them verbatim even at 0 |

`TestWireTypes_ExactJSONKeys` pins the key sets.

## Runner consumer

`runner/cmd/fishhawk-runner/acceptancecorpus.go` — `loadReplayCorpus`
(corpus + ledger read, file-ledger ∪ served-retirement exclusion, sampling
seeded by the run id, `acceptance_replay_corpus_loaded` /
`acceptance_replay_corpus_unreadable` events) and `appendPromptSection`,
with the `FISHHAWK_ACCEPTANCE_REPLAY_MAX_SCENARIOS` (default 25, `0`
disables) and `FISHHAWK_ACCEPTANCE_REPLAY_TIME_CAP_SECS` (default 600)
knobs. An unreadable corpus returns the named error with a ZERO set and an
empty section — replay is skipped, the stage proceeds; callers never fail
the stage on it. Wiring into the acceptance block of `main.go` is a
separate slice.
