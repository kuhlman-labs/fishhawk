# Agent eval harness (v0)

Status: v0 — Tier-A deterministic offline scorer only. Tier-B online
LLM-as-judge and the live scheduled/advisory runner are deferred (see
[Deferred](#deferred-to-follow-up)). Originating issue: #652.

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
- **Tier B (deferred): online LLM-as-judge over trajectories.** A model
  scores trajectories on axes a deterministic pass can't (was the plan
  reasonable? was the fix on-target?). Deferred because the judge needs
  a calibration corpus that does not exist yet — building the Tier-A
  corpus is the prerequisite.

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

## Deferred to follow-up

These are surfaced for the operator to triage (the implementer does not
file the tracking issues). In priority order:

1. **Capture + label a REAL production trace.** The first task of the
   seed-set buildout: replace the synthetic seed fixtures with captured,
   labeled production traces, then grow the set toward the 30–50 target.
   The synthetic shortcut is a bootstrap only — a corpus that stays too
   clean tests nothing real.
2. **Tier-B online LLM-as-judge over trajectories.** Depends on (1): the
   judge needs a calibration corpus before it can be trusted.
3. **Live-model scheduled/advisory runner.** Running the eval on a
   cadence against live runs needs a scheduled GitHub workflow — a
   human-led `.github/workflows` change — and is out of scope here.

## Running it

```sh
scripts/test single -run TestScore ./backend/internal/agenteval/
```

Runs under `scripts/test` automatically as a backend module test, so the
corpus replays in CI on every change.
