# runner/internal/conflictresolve

The pure decision logic for the runner's bounded conflict-resolution pass (E64.62 / [#3202](https://github.com/kuhlman-labs/fishhawk/issues/3202)): conflict-marker classification, the hunk-level partition and accept check, and the baseline/observed verification that decides whether the agent stayed inside the conflicted hunks git itself produced.

## Invariant: no I/O, no git

Nothing in this package opens a file, runs a command, or reads the environment. Every input is a value the runner captured from the merge state git produced, so every decision is table-testable and the whole gate is exercised without a repository. The runner git wiring (`runner/cmd/fishhawk-runner/conflictresolve.go`, a sibling slice) captures, calls, and acts on the verdict — it never re-derives it.

**This package is the sole owner of the accept/refuse decision.** A gate rule implemented anywhere else is a rule that no table test pins.

## What the pass is confining

`git merge --no-commit --no-ff <base>` stops on conflicts, leaving unmerged index stages, marker-bearing working-tree files, `MERGE_HEAD` and `MERGE_MSG`. The agent is invited to edit working-tree files ONLY — never `git add`, never commit. The gate then answers one question: is every byte that would land in the merge commit either a byte git itself produced, or a byte inside a hunk git marked as conflicted?

A merge commit takes its **first parent from HEAD**, its **second from MERGE_HEAD**, its **tree from the index**, and its **message from the merge message source**. All four are gate inputs, each with its own named refusal reason — checking only HEAD, the index and the working tree leaves an agent able to rewrite the commit's ancestry with every other input unchanged.

## The three files

### `markers.go` — what IS a marker line

`ClassifyMarkerLine` recognises the four forms as **exactly seven identical marker characters at line start, followed by end-of-line or a space** (git's label separator), tolerating one trailing CR. A longer run, an indented run and a mixed run are ordinary content.

The diff3/zdiff3 common-ancestor form (`|||||||`) is **RECOGNISED but never REQUIRED**: git emits it only under `merge.conflictStyle=diff3`/`zdiff3`, so a partition that required it would fail on every default-configured repository.

`markerLineSpans` spans each marker line `[Start, Next)` — the terminating newline is deliberately part of the span, so a replacement region that supplies a marker line's newline still intersects it.

### `partition.go` — the hunk-level accept check

`Partition` splits a conflicted file into its ordered NON-CONFLICT segments; a file with N blocks yields N+1 segments.

**Outside a block only the opening `<<<<<<<` line is significant.** A bare `=======` or `>>>>>>>` line in ordinary content (a reStructuredText underline, a fence in prose) is content, not a malformed sequence — treating it as malformed would refuse legitimate files. Inside a block, every transition git would not have written returns `ErrMalformedMarkers`, as does a block left open at end of file.

`AcceptResolution` accepts iff `resolved == seg0 + X1 + seg1 + ... + XN + segN` for some replacement regions `X1..XN`. It uses a **reachable-position sweep, not a greedy scan**: a segment may occur more than once and an earlier match can be the wrong one, so every occurrence is carried forward and the last segment must land exactly at the end of the file. The sweep is bounded by `partitionStepBudget`, spent across BOTH sweeps of one call so the total work per file is bounded rather than the work per sweep, and it **fails closed** on exhaustion with `ReasonPartitionUndecidable` — never an accept.

**The residual-marker rule is a property of the ASSEMBLED RESULT, not of each replacement in isolation.** The operator reproduced the round-1 defect with these bytes:

```
before
<<<<<<< HEAD
ours
=======
theirs
>>>>>>> base
<<<<<< HEAD          <- SIX '<': ordinary content
```

They partition into `before\n` and `<<<<<< HEAD\n`, so the candidate `before\n<<<<<<< HEAD\n` has a single replacement region holding the one byte `<` — a per-replacement marker scan sees nothing, and a live opening marker lands in the committed file. The marker-line spans of the whole candidate are therefore computed once, and an assembly is refused when a marker line intersects any replacement region. A marker line lying **wholly inside a preserved segment** came from the file's own non-conflict content and stays ACCEPTED; that control is pinned by its own test beside the fixture.

`markerCrosses` uses the plain half-open overlap test, which also gives the degenerate ZERO-LENGTH region (two preserved segments abutting) exactly the semantics it needs: a junction strictly inside a marker line refuses, a junction at that line's first or last byte does not. No separate branch is needed and none is written; all three positions are pinned by `TestMarkerCrosses`.

### `baseline.go` — the captured values and the verdict

`Baseline` is the snapshot captured in ONE read immediately after the merge stopped: `HeadSHA`, `MergeHeadSHA`, `MergeMessage`, the non-conflicted stage-0 `Index` (mode + OID per path), and `Conflicted` (per path: `Kind`, `MarkerBytes`, `Sides`, and `Mode`). **Clean base changes git already auto-staged in other files are part of the baseline and are AUTHORIZED.**

`Observed` is the same shape read back after the agent, plus `Unmerged` / `Unstaged` / `Untracked` and a `Working` `FileState` (present, mode, bytes) per conflicted path.

`Verify` returns one `Violation` per rule broken, each carrying its OWN named reason, in deterministic order: the repository-level rules first (their `Path` is empty, which sorts first), then the path-keyed rules sorted by path and reason. **An empty result is the accept verdict — and only then may the runner perform its scoped `git add` of exactly the conflicted paths and its single `git commit --no-edit`.**

| Reason | Rule |
|---|---|
| `conflict_resolution_head_moved` | HEAD (the merge commit's first parent) moved |
| `conflict_resolution_merge_head_changed` | MERGE_HEAD (its second parent) was rewritten |
| `conflict_resolution_merge_message_changed` | the merge message source was rewritten |
| `conflict_resolution_index_entry_changed` | a non-conflicted index entry's mode or OID moved |
| `conflict_resolution_index_entry_removed` | a non-conflicted index entry disappeared |
| `conflict_resolution_newly_staged_path` | a stage-0 entry absent from the baseline index |
| `conflict_resolution_unstaged_change_outside_set` | a working-tree edit outside the conflicted set |
| `conflict_resolution_untracked_path_outside_set` | a new untracked path |
| `conflict_resolution_unmerged_entry_outside_set` | an unmerged entry that was not conflicted at capture |
| `conflict_resolution_conflicted_path_not_unmerged` | the agent staged or committed a conflicted path |
| `conflict_resolution_conflicted_path_missing` | a content-conflicted path deleted outright |
| `conflict_resolution_conflicted_mode_changed` | a mode flip on a conflicted path |
| `conflict_resolution_binary_conflict` | refused: git leaves OURS on disk with no markers, so there is no hunk boundary |
| `conflict_resolution_delete_modify_content` | a delete/modify resolution that is neither side's full content nor the deletion |
| `conflict_resolution_edit_outside_hunk` | the resolution does not preserve every non-conflict segment |
| `conflict_resolution_residual_marker` | a marker line survives at a position a replacement reaches |
| `conflict_resolution_malformed_markers` | the file cannot be partitioned, so nothing can be authorized |
| `conflict_resolution_partition_undecidable` | the accept sweep exhausted its step budget |

Two rules are worth naming explicitly:

- **Mode, not just bytes.** The resolution contract is BYTES-ONLY, and the runner's scoped `git add` stages mode alongside content, so an agent could resolve a file legitimately AND set its executable bit. Every conflicted path's mode is captured and verified — `100644` vs `100755` vs `120000` each have a test case.
- **A staged conflicted path draws `conflict_resolution_conflicted_path_not_unmerged`, not the generic newly-staged reason.** The two rules deliberately do not double-report the same path; the precise reason is the one the operator reads out of the terminal failure report.

## What this package does NOT decide

It does not know whether a resolution is semantically RIGHT — only that its blast radius is bounded to bytes inside the hunks git itself marked. The resulting merge commit still lands on a PR that goes back through the review gate the pass re-parks. This is a confinement gate, not a substitute for review.
