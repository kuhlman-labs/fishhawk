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

### Acceptance stage

Some workflows declare an **acceptance stage** (E31.9 / ADR-049) after the
implement review: an advisory, runner-hosted validator that drives the change
against a **running instance you provision** and ships a structured verdict.
The default placement is a **pre-merge preview** built from the PR ref;
release-acceptance against a staging or release instance is the documented
variant. The preview/target instance is customer-provisioned — Fishhawk does
not stand it up.

**Dispatch it like implement.** After the implement review settles with no open
concerns, `fishhawk_dispatch_stage` (acceptance) — non-blocking is the default
because acceptance runs long against the live instance, and it keeps the session
free. `fishhawk_run_stage` (acceptance) is the blocking opt-in. The stage takes
no new argv (no `--plan-out`, no `--check-base-ref`); its egress target hosts and
criteria ids arrive via `--fetch-prompt`.

**Await the verdict.** Poll `fishhawk_get_run_status` — `acceptance_stage_wait_status`
tracks the stage's execution — or `fishhawk_await_audit` anchored at the
`acceptance_dispatched` sequence. `fishhawk_await_review` does **not** fit: it is
shaped around configured-reviewer verdicts and the acceptance stage has no
reviewers; its settle signal is the audit trail.

**Verdict vs. stage state (load-bearing).** A **failed** acceptance verdict
leaves the STAGE `succeeded` — the stage settles through the ordinary agent
trace-bundle path regardless of pass/fail. Read the `verdict` from the
`acceptance_outcome_recorded` audit entry, never from the stage state. Merge only
on the `acceptance_passed` next-actions state (ADR-049 decision #6: the merge is
gated on the acceptance_passed evidence condition).

**Deterministic triage of a failure** (ADR-049 decision #2, server-side, bounded
at **2 auto re-runs** per run):

| Class | Failure | Auto disposition |
|---|---|---|
| 1 | the code errors, or every failed criterion is explicit-source | `fixup_dispatched` — re-opens the implement stage as a fix-up pass |
| 2 | no criterion failed but ≥1 skipped (environment/flake) | `retry_dispatched` — re-opens the acceptance stage for a re-run |
| 3 | a failed criterion is inferred-source/unresolvable (bad/ambiguous criterion) | `paged` — no transition, you arbitrate |
| 4 | unitemized / provenance-ungroundable (works-as-planned, disputed) | `paged` — no transition, you arbitrate |

At the re-run budget, or when the fix-up/retry route is unavailable, the
disposition degrades to a paged variant (`rerun_budget_exhausted`,
`fixup_unavailable_paged`, `retry_unavailable_paged`, `unsettled_paged`) so
non-convergence always lands on the human.

**LOCAL-runner re-open rule.** An auto-routed re-open (`fixup_dispatched` or
`retry_dispatched`) re-opens the stage server-side but **never spawns the local
runner** — the same rule as local-drive fixup above, generalized. You MUST
`fishhawk_dispatch_stage` the re-opened stage explicitly: the **implement** stage
after `fixup_dispatched`, the **acceptance** stage after `retry_dispatched`.
`next_actions` surfaces this as the `acceptance_triage_rerouting` state; on the
next snapshot the re-opened stage's own dispatch arm serves the move.

**Paged arbitration.** For a paged-family disposition, `next_actions` gives the
`acceptance_triage_paged` arm: read the evidence first (`fishhawk_list_audit` on
`acceptance_outcome_recorded` for the criteria results and
`acceptance_triage_decided` for the class + reason), then arbitrate —
`fishhawk_fixup_stage` (a manual fix-up pass, consumes the shared fix-up budget),
`merge_and_file_follow_up` (accept-and-ship, e.g. a class-3 bad criterion), or
`fishhawk_cancel_run`.

**Settled-outcome-unknown recovery (E31.16 / #1567).** A different failure from
the paged case: the acceptance stage settled `succeeded` but **no**
`acceptance_outcome_recorded` verdict shipped at all — the agent emitted a
non-schema field and the verdict failed closed before it reached the backend
(the run-f7a4b71b hole). `next_actions` surfaces this as the
`acceptance_settled_outcome_unknown` state — deliberately NEVER the merge ritual
(fail toward read, not toward merge). First `fishhawk_list_audit` on
`acceptance_outcome_recorded` to CONFIRM no verdict exists for the stage (the
default `audit_limit` is 5, so a real verdict can merely have aged out of the
window). Once confirmed, `fishhawk_retry_stage` on the **acceptance stage id**
re-opens it `succeeded → pending` (operator token only; an agent token is
refused 403, and the server 422s `retry_not_applicable` if a verdict IS
recorded). The reopen lands the stage in pending, so on the local runner
`fishhawk_dispatch_stage` (acceptance) — surfaced by the `acceptance_pending`
arm on the next snapshot — spawns the actual re-run.

### Late CI/SAST finding after the fix-up ceiling

The bounded fix-up budget is hard-capped at 3 total passes per implement stage
(normal pass + operator overrides). Once that ceiling is reached,
`fishhawk_fixup_stage` refuses with `422 fixup_ceiling_reached` and the MCP
`review_action_hint` stops offering an override. A required external check
(CodeQL/SAST) can still surface a late finding at that point, and there is no
fix-up pass left to route it through the agent.

The sanctioned in-loop remedy is the operator-vouched patch path (#1068/#1044),
NOT a separate CI/SAST budget: commit the one-line fix on the run branch
yourself, then `fishhawk_vouch_commit` it. The vouch unions your operator
commit into the run's reported-head ledger so it clears the ADR-035
sole-writer lineage gate — the run is not wedged with a `foreign_commit_on_branch`
failure, and the operator commit is attributed in the audit chain.

Use the **operator / operator-agent token** for the vouch. `fishhawk_vouch_commit`
rejects a run-bound `mcp:run:<uuid>` (`fhm_`) token outright
(`run_token_forbidden`) by design — an agent self-declaring lineage for a
commit on its own branch would defeat the cross-write protection the vouch
exists to preserve. So the surfaced remedy is only actionable with the operator
token. If the finding is not worth an in-loop fix, the ceiling-reached hint's
other arms still apply: merge with a follow-up issue, or start a fresh run.

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
