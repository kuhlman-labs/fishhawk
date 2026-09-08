# runner/internal/conflictresolve

Pure decision logic for the runner's conflict-resolution confinement gate
(#3202, E64.62).

`fishhawk_rebase_run_branch` fails closed with `422 rebase_conflict` when
advancing a run branch onto its declared base conflicts, handing the operator
back to resolve-push-vouch — a push to a branch ADR-035 declares runner-owned.
#3202 replaces that refusal with a bounded agent pass that resolves the
conflict ON the run branch. This package is the half of that pass which
decides whether the agent's work may be committed. It is the FIRST of the
three slices #3202 is split into, so it ships with no callers: the runner git
wiring and the backend trigger that changes the verb's observable behaviour
land alongside it.

The package runs **no git and performs no I/O**. Every input is a captured
value, so each named refusal has a table test and the runner's git wiring
(`runner/cmd/fishhawk-runner/conflictresolve.go`, slice 2) is left with
capture and execution only.

## Division of labour

| Layer | Owns |
|---|---|
| `backend/internal/server` | The trigger, the ceiling-1 budget, the failure recovery |
| `runner/cmd/fishhawk-runner` | The local merge, baseline capture from git, the agent invocation, the scoped add + single commit, the abort/reset recovery |
| **this package** | Marker recognition, the hunk-level partition check, baseline verification — every decision, none of the execution |

## What the gate proves

The gate is a mechanical check against the **merge state git itself
produced**, never a status scan. The runner captures a `Baseline` in one read
immediately after `git merge --no-commit --no-ff <remote>/<base>` stops on
conflicts, re-reads the same state as an `Observed` after the agent pass, and
calls `Verify`. An empty result authorizes `git add <conflicted paths>` plus
one `git commit --no-edit`; anything else refuses and the runner aborts the
merge.

A passing gate proves:

- **HEAD, MERGE_HEAD and the merge message source are unchanged.** These are
  the inputs that feed the commit the gate authorizes — a merge commit's first
  parent comes from HEAD, its second from `MERGE_HEAD`, and its message from
  the merge message source. All three are on-disk repository metadata an agent
  can write while leaving the index and the working tree byte-identical, so
  each carries its own named reason rather than riding on the tree checks
  (operator approval condition 1, 2026-09-06).
- **The index is exactly what git left.** Every non-conflicted stage-0 entry
  matches the baseline in mode and object id; nothing new is staged; no
  conflicted path has lost its unmerged stages. Clean base changes git already
  auto-staged are AUTHORIZED — they are part of the baseline.
- **The working tree changed only inside the conflicted set.** No unstaged
  change and no untracked path outside it.
- **Each conflicted file changed only inside its conflicted hunks** — the
  partition check below.

## What the gate deliberately does NOT prove

- **It does not prove the resolution is CORRECT.** Any bytes are acceptable
  inside a hunk git marked as conflicted. The gate bounds the blast radius; it
  does not decide which side should have won.
- **It does not make the agent immune to being misled.** A conflict hunk is
  repository content, which is untrusted input, and a hostile hunk can steer
  which side the agent keeps. This is structural containment, not behavioural
  resistance — the same posture `AGENTS.md` states for the #2291 injection
  corpora.
- **It does not inspect commit authorship, hooks or git config.** The runner's
  own invocation environment is the control for those.

## Marker recognition (`markers.go`)

A conflict-marker line is **exactly seven identical marker characters at line
start**, followed by end-of-line or by a space and the label git emits. All
four forms are recognised: `<<<<<<<`, `|||||||`, `=======`, `>>>>>>>`.

- The `|||||||` common-ancestor form appears only under
  `merge.conflictStyle=diff3` or `zdiff3`, so the partition **recognises** it
  without **requiring** it.
- An eight-character run (`<<<<<<<<`), a six-character run, an indented
  marker, or a label with no separating space is ordinary content.
- A trailing CR is tolerated: git writes markers with the file's own line
  endings, so a CRLF file's markers arrive as `<<<<<<< HEAD\r`.

## The partition check (`partition.go`)

`Partition` splits a conflicted file into its ordered **non-conflict
segments** — the text before the first conflict block, between consecutive
blocks, and after the last. N blocks yield N+1 segments, each carrying its own
trailing newline, so the segments plus the blocks reconstruct the input byte
for byte.

Only `<<<<<<<` opens a block. The other three forms are significant only
*inside* one, so a file whose content legitimately begins with a line of seven
equals signs partitions as ordinary content rather than as a malformed file.

`AcceptResolution(markerBytes, resolvedBytes)` accepts iff

```
resolvedBytes == seg0 + X1 + seg1 + ... + XN + segN
```

for some replacement regions `X1..XN`, and **no Xi carries a residual marker
line**. That is hunk-level confinement: every byte the agent wrote lies inside
a region git itself marked as conflicted, and every byte git did not mark is
preserved verbatim and in order.

Two details are load-bearing:

- **The residual-marker check applies to the Xi, not to the whole file.** A
  repository may legitimately contain a line of seven equals signs; it lives
  in a segment and survives into every accepted resolution.
- **The match is a reachable-position sweep, not a greedy scan.** A segment
  may occur again inside a resolution, and the earliest match can be the wrong
  one. The sweep is bounded by a step budget and **fails closed** with
  `conflict_resolution_partition_undecidable` when the budget is exhausted —
  an undecidable partition is refused, never accepted.

**Delete/modify** conflicts carry no markers: git records stages 1+2 or 1+3
and leaves the surviving side on disk. They accept either side's full content,
where keeping the deletion is expressed by the path being absent from the
working tree.

**Binary** conflicts — a binary file, or one carrying `-merge` in
`.gitattributes` — are refused with a named reason. Git leaves OURS on disk
with no markers, so there is no hunk boundary to confine an edit to, and
accepting whatever bytes are on disk would accept an unbounded change.

## Named reasons

Exactly one reason is reported per violation, so the runner can tell the
backend which rule refused rather than reporting a generic failure.

| Reason | Fires when |
|---|---|
| `conflict_resolution_head_moved` | HEAD is not the commit the merge stopped on |
| `conflict_resolution_merge_head_changed` | `MERGE_HEAD` changed — the merge commit's second parent would differ |
| `conflict_resolution_merge_message_changed` | the merge message source changed |
| `conflict_resolution_index_entry_changed` | a non-conflicted index entry's mode or object id differs |
| `conflict_resolution_index_entry_removed` | a non-conflicted baseline index entry is gone |
| `conflict_resolution_newly_staged_path` | a stage-0 entry exists for a path the baseline never carried |
| `conflict_resolution_unmerged_entry_outside_set` | an unmerged entry exists outside the conflicted set |
| `conflict_resolution_conflicted_path_not_unmerged` | a conflicted path lost its unmerged stages (the agent staged or removed it) |
| `conflict_resolution_unstaged_change_outside_set` | a working-tree change outside the conflicted set |
| `conflict_resolution_untracked_path_outside_set` | an untracked path outside the conflicted set |
| `conflict_resolution_conflicted_path_missing` | a textually conflicted path was deleted |
| `conflict_resolution_outside_hunk` | the resolution does not preserve the non-conflict segments in order |
| `conflict_resolution_residual_marker` | a replacement region still carries a marker line |
| `conflict_resolution_malformed_markers` | the captured file is not a well-formed sequence of conflict blocks |
| `conflict_resolution_partition_undecidable` | the segment match exhausted its step budget |
| `conflict_resolution_binary_conflict` | git could not merge the file textually |
| `conflict_resolution_delete_modify_content` | a delete/modify resolution is neither side's full content nor the deletion |

`Verify` is deterministic: per-path violations are reported in sorted path
order, so a refusal message is stable across runs.

## Tests

Table tests only — no git, no I/O, no temporary repositories. Every named
reason above has a case asserting it is reported EXACTLY once and on the right
path, and the accepted cases cover the diff3 form, CRLF, empty segments, a
segment whose text also appears inside the hunk, and the auto-staged clean
base change. The real-repository half — capture, execution and the recovery
postcondition — lives with the git wiring in
`runner/cmd/fishhawk-runner/conflictresolve_test.go`.
