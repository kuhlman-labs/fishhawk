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
| `steps_carried_from` | set ONLY by a re-record that kept the richer prior `steps` over genuine new ones (`Amend`); names the origin those steps predate; omitted otherwise (#3412) |
| `assertions.expected`, `assertions.observed_at_record`, `assertions.repro_handle` | the passing row's `expected` / `observed` / `repro_handle` |
| `origin.issue`, `origin.pr`, `origin.run_id`, `origin.head_sha`, `origin.recorded_at` | attribution; `pr: 0` means UNKNOWN and is never substituted with the issue number |

`Load(dir)` is STRICT (`yaml.Decoder.KnownFields(true)`): an unknown field,
an id without the prefix, a missing `origin.run_id` or
`origin.recorded_at`, or undecodable YAML is a named error carrying the
file's corpus-relative path — never a silent skip. A missing directory is an
empty corpus. `Write(dir, s)` writes atomically (temp + rename) at
`PathFor(s.ID)` and refuses an id without the prefix or one carrying `..` /
a leading `/`. It ALSO refuses — by component name, BEFORE `MkdirAll`
creates anything — a symlinked component of that path under `dir` (the
`issue-<N>` directory or the `.yaml` leaf), so a committed symlink cannot
redirect the write outside the tree (#3396; `MkdirAll` would already have
followed the link, which is why the check precedes it).

`RefuseSymlinks(root, rel)` is that check, exported so the persist step can
apply it to the corpus-root components it owns (`acceptance/`,
`acceptance/scenarios/`) which sit ABOVE the `dir` this package receives.
Contract: every existing component of `rel` under `root` is `Lstat`ed; a
symlink → `refusing to write through symlinked path component "<comp>"`;
an existing intermediate that is not a directory → `non-directory path
component "<comp>"`; a `..` component → `refusing parent-directory path
component`, and an absolute `rel` → `refusing absolute path`, BOTH before
any lstat (the walk joins with `filepath.Join`, which would normalize a
`..` up and out of `root` — so the function does not depend on a caller
having pre-validated `rel` the way `PathFor` does); the walk stops at the
first absent component (nothing
that does not exist can redirect `MkdirAll`); the leaf is checked too
(`rename(2)` onto a symlink replaces the link entry, not the target, so this
half is belt-and-braces). Residuals, stated: `root` itself is NOT checked
(a symlinked temp root — macOS `t.TempDir()` under `/var` → `/private/var` —
is legitimate and not attacker-committed), `Load` / `LoadRetired` still READ
through a symlinked component (disclosure into the prompt, not a write
escape), and the lstat→`MkdirAll` window against a concurrent host-side
writer is accepted for the runner's own detached checkout. Pinned by
`TestWrite_RefusesSymlinkedDirComponent`, `TestWrite_RefusesSymlinkedLeaf`,
`TestWriteRetired_RefusesSymlinkedLedger` (real `os.Symlink`, outside dir
asserted empty / target bytes unchanged) and
`TestRefuseSymlinks_NonDirectoryComponentAndMissingTail`; the `..` /
absolute refusals by `TestRefuseSymlinks_RejectsParentAndAbsoluteRel`.

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
reason that survives. `WriteRetired` is atomic and shares `Write`'s
`writeAtomic` helper, so a symlinked `retired.yaml` (or a symlinked
component above it under `dir`) is refused the same way.

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

## Amend on re-record — `Existing(dir, id)` + `Amend(prev, next)`

A fix-up push re-opens the acceptance stage, and the second pass re-records
the same scenario id. Before #3412 `Write` REPLACED the file wholesale, so a
new pass whose `steps_taken` was a back-reference (`"same two-run drive as
the replayed scenario …"`) overwrote the expensive prior recipe with a
pointer to the recording it was deleting. Re-record is now Compose + **Amend**
+ Write.

`Existing(dir, id)` is the re-record read: `PathFor(id)` resolves the path,
`RefuseSymlinks` refuses a symlinked component, an absent file is
`(zero, false, nil)`, a decodable file is `(s, true, nil)` with `Path` set,
a malformed file is `(zero, false, named-error)`. The caller MUST have proven
the tree clean first (persist refuses a dirty tree before this call), so the
file read IS HEAD's and a planted file never reaches it.

`Amend(prev, next)` merges the re-record onto the prior file:

| field | rule |
|---|---|
| `id`, `statement`, `verify_hint`, `seed`, whole `origin` | ALWAYS from `next` (plan-authoritative + the volatile head/run/recorded_at, so the file attributes itself to THIS pass) |
| `steps` | the richer text wins — longer after a whitespace trim, ties to `next`. TWO fallback guards: a PRIOR `StepsNotRecorded` fallback never wins (else it could beat genuine-but-shorter new steps — the bug inverted), and a NEW `StepsNotRecorded` fallback never displaces genuine prior steps |
| `assertions.*` | `next`'s value when non-empty after a trim, else `prev`'s |
| `steps_carried_from` | SET when the richer PRIOR steps are kept AND `next` carried genuine (non-fallback) steps that were displaced — the disclosure (see below); empty otherwise |

`AmendReport.StepsKept` is `prior` or `new` for the caller's log.

**Disclosure (`steps_carried_from`).** When Amend keeps the prior steps over
genuine new ones, the written file could otherwise assert a fresh `origin`
(head, run, recorded_at) beside steps describing a DIFFERENT, older drive —
a record quietly wrong about itself. So `steps_carried_from` names the origin
the kept steps came from (`head <sha> recorded_at <ts>`), and a reader of the
file ALONE can tell the steps predate the origin (#3412). A chain preserves
the DEEPEST source: a re-record over a file that already carries a disclosure
keeps that disclosure rather than re-pointing it at the intermediate origin —
and that hold is UNCONDITIONAL on `next` being the fallback. No NEW disclosure
is synthesized when `next`'s steps were the fallback (nothing was displaced this
pass) or when the new steps won, but a fallback keep still carries prev's
EXISTING disclosure forward: clearing it would strand prev's kept steps beside
`next`'s fresh origin with no disclosure (the chained-fallback gap). Pinned by `TestAmend` and, end to end
through persist → git → bare origin, by
`TestPersist_ReRecordAmendsPriorRecording` /
`TestPersist_ReRecordTakesRicherNewSteps` /
`TestPersist_ReRecordOverUndecodablePriorReplaces` (runner cmd).

## Prompt section — `RenderPromptSection(chosen, timeCap)`

The `### Regression corpus` markdown the runner appends to the acceptance
prompt: replay-first instruction, one block per scenario (`scenario: <id>`,
statement, seed when set, steps, expected, `origin PR #n` or
`origin PR unknown`), the total time cap, and the rule that an unreached
scenario is reported `skipped` with `expectation_basis:
replay_budget_exhausted` (`BudgetExhaustedBasis`). Empty when nothing is
served. It ALSO instructs the agent that every `steps_taken` — for a replayed
scenario and for a criterion — must be a COMPLETE standalone recipe, never a
back-reference to another listed scenario, because a re-record rewrites the
referenced file and the referent disappears (#3412) — the source-side half of
the same fix `Amend` protects the corpus from structurally.

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
