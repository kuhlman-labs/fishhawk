# Fishhawk operator runbook

You are the operator of a Fishhawk run. The agent proposes work and writes
the code; you decide at every gate and own all version-control actions
(approve PR, merge, post-merge). This runbook is the in-band counterpart to
the operator-role contract (ADR-040): read it when you are driving a run
without prior operator memory.

The MCP server's `instructions` field carries the short happy-path map. This
resource is the long form: the full procedure plus the edge cases that strand
a run when handled wrong.

## The operator role

The agent's verdicts and PR bodies are proposals, not facts. Verify the
committed code at the PR head — not the agent's prose — before you approve or
merge. The agent must not file follow-up issues, take git actions, or merge;
those are yours. When a run-status response carries a delegation block marking
an action `delegated:true`, you may take it under the operator-agent token;
otherwise loop a human.

Prefer the server's `next_actions` block (on `fishhawk_get_run_status`) over
procedure recall: it is the authoritative, run-state-aware "what to do next"
and supersedes any step sequence written here when the two disagree.

## Happy-path loop

One issue → one run → one PR.

1. **`fishhawk_start_run`** — open a run for the issue. For the local dogfood
   loop pass `runner_kind:local` (see below — this is the single most common
   mistake). The call returns the run id every later verb takes.
2. **`fishhawk_run_stage` (plan)** — the agent writes a `standard_v1` plan.
   The call blocks until the plan stage settles.
3. **Read the plan and its reviews** with `fishhawk_get_plan`, then
   **`fishhawk_approve_plan`** (approval notes are delivered to the implement
   agent as binding conditions) or **`fishhawk_reject_plan`** with a reason —
   rejection feedback propagates to a fresh planning run.
4. **`fishhawk_dispatch_stage` (implement)** — execute the approved plan.
5. **`fishhawk_await_review`** — block until the implement review reaches a
   terminal verdict. Re-poll `fishhawk_get_run_status` if it times out; that
   poll is the authoritative path to a terminal status.
6. **Approve the PR, merge, post-merge.** Approve the PR with an operator
   verdict before every merge — no exceptions. Then run your post-merge step
   to pull main and reload the stack.

## Edge-case playbook

### runner_kind:local for the local dogfood loop

`fishhawk_start_run` defaults `runner_kind` to `github_actions`. The local
dogfood loop dispatches the runner on this host, so a run started with the
default tag has an execution/tag mismatch: the auto-advance from exempt to
PR-open never fires, fixup does not auto-spawn the runner, and the next-action
hints read "nothing to run" even though work is pending. **Always pass
`runner_kind:local`** when driving the local loop. A run already started with
the wrong kind cannot be retagged — cancel it and start fresh.

### Local-drive fixup needs an explicit dispatch_stage

On a local-drive run, `fishhawk_fixup_stage` re-opens the implement stage to
state `dispatched` but does **not** spawn the runner. It returns no
`log_path`, and the "github_actions auto-dispatches / nothing to run"
next-action hint is **false** for local drive. After a fixup you MUST call
`fishhawk_dispatch_stage` (implement) to actually execute the re-implement.
Skipping this strands the run with a re-opened stage and no runner.

Note also that a fixup re-drives the **entire** implement agent (tens of
thousands of tokens), not a patch. A no-op fixup (zero diff) still burns the
budget and wedges the run. Before approving a fixup, cross-check the
files-changed against the plan scope; if a pass would be a no-op, abandon and
start a fresh run instead.

### Scope-amendment decide / naming flow

When the implement agent discovers a file it must change that is outside the
approved `scope.files` (a coupled test, a registration table, a doc
companion), it does not edit it silently — it files an operator-gated
scope amendment and waits. You decide with `fishhawk_decide_scope_amendment` (or
`fishhawk_list_scope_amendments` to enumerate pending requests). Watch the
runner log for `scope_amendment_pending` (the runner-log event), not
`scope_amendment_requested` (the audit category); missing it lets the agent
loop on its wait-poll until the stage is killed.

To **add** files at the plan-approval gate, name them explicitly as
`dir/file.ext` in the approval reason (this folds them into scope) or use the
add-scope-files path. Do **not** write a repo-relative path into approval
*rationale* prose to merely explain it — that folds the path into required
scope, and an untouched required file fails the stage. Pending amendments
survive a stage failure: approve post-failure, then `fishhawk_retry_stage`
folds them at restart.

### Heterogeneous-review two-verdict waits

A `feature_change` run is reviewed by two agents concurrently (e.g. an Opus
reviewer and a GPT reviewer) on both the plan and implement stages. Wait for
**both** verdicts before acting — the review status stays `pending` until every
configured reviewer has landed a terminal verdict. Advisory disagreement is
normal and expected: one reviewer rejecting while the other approves does
**not** block the run. You arbitrate — read both verbatim verdicts and decide.
A reviewer marked external/OOM-failed is usually a misclassified adapter error
and is non-blocking; do not treat it as a real rejection without checking the
underlying error.

### Post-failure clean-tree discipline

A failed run can leave the working tree dirty: `working_tree_restored` leaves
untracked new files behind, and a failed `fishhawk_fixup_stage` can leave you
on the run branch with staged changes. Run `git status` before every
`fishhawk_run_stage` / `fishhawk_dispatch_stage`; an unclean tree lets the next
runner sweep leftovers into the commit and fail the stage on scope drift
(category-B). Recover with `git checkout -f main` (or remove the untracked
files) so each run starts from a clean base. The runner owns all commit /
branch / push operations — never commit or switch branches yourself.
