# Grooming runbook

Operator handoff for the `backlog_grooming` loop: how to run it, how to read what it
produces, and how to turn its ranking into work. Written for an agent picking this up
with no prior context.

State as of 2026-08-24. Loop verified end to end by walks #2828, #2839, #2848.

## 1. What works today

An on-demand run produces a charter-anchored `grooming_report`, parks at an approval
gate, and **approving it applies the hygiene-class mutations** to the tracker with every
mutation audited.

That path was assembled from five changes, each unblocking the next:

| Change | What it fixed |
|---|---|
| #2826 | `applies_to` routing could not select the workflow — no producer emitted a non-diff trigger form |
| #2833 | the runner destroyed the report before upload (structured-output adoption overwrote it) |
| #2837 | `handleGroomingReport` never advanced the stage, so the gate never opened |
| #2822 | `ApplyGrooming` had no production caller |
| #2847 | the apply wrote `suggested_fix` PROSE as the mutation payload |

Every one was found by RUNNING the loop, not by a test. Each layer was individually
correct and tested; the seams between them were where it broke.

Last verified walk: **18 mutations applied, 0 failed**, confirmed on the forge.

## 2. Invocation

```
fishhawk_start_run(
  repo:           "kuhlman-labs/fishhawk",
  workflow_id:    "backlog_grooming",
  issue:          2832,
  trigger_source: "on_demand",
  runner_kind:    "local",
  working_dir:    "<absolute path to your checkout>"
)

fishhawk_run_stage(
  run_id:      <id>,
  stage:       "plan",              # the stage TYPE, not the id "groom"
  workflow:    "backlog_grooming",
  working_dir: <same>
)
```

Two failure modes that look like product bugs and are not:

- **`trigger_source: "on_demand"` is load-bearing.** Omit it and the run derives a `diff`
  trigger, `applies_to` refuses the workflow, and the run never starts.
- **`stage` takes the TYPE.** Passing `"groom"` fails with
  `available: [plan implement review]`. `groom` is the stage id.

The anchor issue supplies the request — the `groom` stage declares
`inputs: [{source: github_issue, required: true}]`. Reuse #2832 or open a new issue whose
body states what you want groomed; the body IS the request.

Stage budget is 45m (#2838). A real pass over this backlog measures ~24m.

## 3. Reading the report

The artifact lands at `/tmp/fishhawk-plan.json` and is ingested as `grooming_report_v1`.
Typical shape: ~27 ordering entries, ~16 hygiene defects, plus duplicates, dependency
edges, decomposition suggestions, vision drift.

Before approving, check that hygiene entries carry the STRUCTURED member:

```json
"fix": {"labels": ["phase:alpha"]}
```

The value alone. Prose in that field means the #2847 defect has returned.

## 4. Approving is a write

`fishhawk_approve_plan` executes the hygiene mutations server-side. There is no separate
apply step to reconsider at — the gate IS the apply trigger, which is why the `apply`
stage was removed (#2851).

- `hygiene` applies (labels, fields, boarding, epic links — objective and reversible),
  **except a proposed `autonomy:` delegation-tier label**, which is refused with a named
  audited skip and needs your hand or #2843 (see §8).
- `ordering`, `dedup`, `scoping` receive no decision and apply nothing. They are
  non-delegable by construction and refused at `mode: auto` at parse time.

**Verify on the forge, not from the summary.** On walk #2844 the summary truthfully
reported eight applied while every applied VALUE was garbage. An audit row saying
`applied` is not the same claim as the tracker carrying the change.

## 5. Confirming is the second write

The `confirm` stage is a `type: review` gate declaring `executor: human` with
`approvals: {count: 1, not: [agent]}`. It is where you record that you checked the FORGE
rather than the summary (§4). Until #3041 it had no approvable surface anywhere: the
approval endpoint refused EVERY review stage with 409 `review_stage_managed_by_github`
(ADR-018), which is correct for a PR-merge-managed review gate and wrong here — a
grooming run could be started but never finished, parking in `running` indefinitely.

**There is no MCP verb and no CLI verb for this gate. A human runs the curl.** That is
the gate working as declared, not a gap:

```
curl -sS -X POST "$FISHHAWK_BACKEND_URL/v0/stages/<confirm_stage_id>/approvals" \
  -H "Authorization: Bearer $FISHHAWK_OPERATOR_TOKEN" \
  -H "Content-Type: application/json" \
  -d '{"decision":"approve","comment":"checked the forge: #2801 carries area:runner and the parent link; #2799 label landed"}'
```

- The credential must be a **human-held operator credential**. Both a run-bound `fhm_`
  token (403 `self_decision`) and a delegated operator-agent token (403
  `operator_agent_forbidden`) are refused BY DESIGN — an MCP tool is invoked under
  exactly the second identity, which is why no such tool exists. **After #3041 an agent
  still cannot approve this gate. A human must.**
- The `comment` is the **attestation and is REQUIRED**: an empty or whitespace-only
  comment on an approve is refused 400 `attestation_required`. State what you checked. A
  `reject` needs no comment — it fails the stage category D.
- Approving takes the run **terminal** (`succeeded`); the orchestrator finds no remaining
  stage and completes the run.
- Get `<confirm_stage_id>` from `fishhawk_get_run_status` — it is the `review` stage
  sitting at `awaiting_approval`.
- **An approval is bound to the STAGE ROW, not to the run's intent.** Retrying or
  re-running creates new stage rows, so a prior approval cannot be reused; you approve
  the new row.
- A 409 with `details.admission_reason` means the gate was not admitted. `pull_request_managed`
  means you are looking at a PR-merge review stage (approve it by merging the PR);
  anything else (`workflow_spec_unparseable`, `multiple_review_spec_stages`,
  `not_human_executor_row`, …) names a fail-closed resolution failure against the run's
  CACHED workflow spec.

## 6. Turning the ranking into work

An approved order seeds a campaign directly:

```
fishhawk_start_campaign(
  repo:                 "kuhlman-labs/fishhawk",
  grooming_run_id:      "<approved run id>",
  working_dir:          "<absolute path>",
  grooming_order_limit: 5
)
```

Pass `grooming_run_id` INSTEAD of `epic_ref` / `items` — combining them is refused.

**A FULL ratified order now assembles (E54.59 / #3113).** `grooming_order_limit` is a
batch-sizing choice, not a workaround for an assembly failure. It used to be both: the
no-epic source resolves each named issue's `depends_on` through the forge one at a time,
so a sixty-item order cost sixty-plus round-trips — and the MCP client's own 30s
`http.Client` wall aborted at `29999ms` with a bare transport error carrying no counts,
while the server had no deadline on that path at all. (The issue's original diagnosis of
a *server-side* 30s deadline was wrong.) Now the server bounds the resolution itself
(`--issue-set-resolution-budget`, default 120s, permitted maximum 10m) and the client
waits longer than any permitted budget, so the SERVER is what decides.

If the order is genuinely too large for the budget, the refusal is
`issue_set_resolution_timeout` and it NAMES A NUMBER: `resolved N of M`, and — when a
value could be **proven** to fit — a `grooming_order_limit=N` to retry with. Take that
number rather than guessing; it is the longest prefix of the ratified order that fully
resolved. When the refusal says it could not prove any count, bisect (halve the limit and
retry) or raise the deploy's `FISHHAWKD_ISSUE_SET_RESOLUTION_BUDGET`.

Still cap the first batch and watch the first item land before trusting the queue — for
the ordinary reason (a bad first item is cheaper to notice in a batch of five), not
because a full order cannot assemble.

The alternative — walking the ranked list downward with individual `fishhawk_start_run`
calls — respects the ordering and skips nothing. Slower, more reliable.

## 7. Autonomy tiers

**Policy for this repo: only agents author code.** `autonomy:low` therefore means an
agent STRUCTURALLY CANNOT do it, not that the work is sensitive — sensitivity is what
the gate is for.

Structural blockers, and nothing else:

- edits `.github/workflows/**` (agent workflows forbid it)
- edits `.fishhawk/**` (forbidden path)
- requires a real external target the sandbox cannot reach: a second real repo,
  GitLab.com, a real cluster, real secrets, a real OAuth app registration, a real domain
- the human is the experimental subject (operator drills)
- it is not code at all (partner agreements, commercial terms)

A campaign refuses `autonomy:low` items as `item_human_led`.

Current: **34 open `autonomy:low`**, of which 14 are epics (parents, never campaign
items), leaving 20 genuine human-led work items. 36 issues were re-tiered to
`autonomy:medium` on 2026-08-24 under this policy.

#2274 is the durable fix — it introduces an `execution:` namespace so "cannot" and
"should not be delegated" stop sharing one label.

## 8. Hazards

**A groom still PROPOSES a tier; the apply refuses it (#2855, shipped).** The groomer
proposed `autonomy:medium` for #1512 on one run and `autonomy:low` on the next, and
autonomy is the delegation control — so approving a report used to undo a deliberate tier
decision. It no longer can: a hygiene entry whose `fix.labels` carry an `autonomy:` label
is recorded as an audited `delegation_tier_not_authorized` skip and applies **nothing**,
under `mode: auto` and under a whole-report approval alike. What you still have to do:

- The refusal fails the **whole entry**, so a mixed `[area:, autonomy:, phase:]` proposal
  applies none of the three. The clerical halves need you.
- The proposal stays fully visible — the audit row carries every proposed label, and the
  ingest census gains a `delegation_tier_proposals` count so you can see at the gate how
  many entries propose a tier. Apply the ones you agree with **by hand**, or wait for the
  per-entry disposition surface (#2843) to carry the decision.
- The entry is not suppressed: a refused proposal never enters the churn baseline, so it
  resurfaces next run rather than disappearing.

**The acceptance sandbox cannot reach a forge.** A criterion needing one must carry
`requires_live_validation` so acceptance short-circuits it into an operator walk rather
than failing the run. Plans have omitted this four runs running (#2845).

**A fix-up pass cannot rewrite the PR body.** It is composed once at PR-open. Route
attestations to the fix-up self-report sidecar instead — and since E68.20 / #3042 the
sidecar REACHES the reviewers. The committed-tree verify tail is attached automatically
from the fix-up stage's OWN uploaded bundle (or the round names the machine reason it
could not be, so "no evidence attached" is never again an unresolvable agent-shaped
concern), and counterfactual results ride the sidecar's `counterfactuals` array as
`{control_path, observed, restored}` triples. Both the record narrative and the test name
stay on the runner: only the triple crosses the upload boundary, and the reviewer is told
plainly that it is an agent CLAIM, not a runner observation.

## 9. Open follow-ups

| Issue | Subject |
|---|---|
| #2843 | per-entry dispositions; unblocks applying the destructive classes |
| #2834 | prompt builder has no grooming branch (groom stage gets standard_v1 instructions) |
| #2827 | intake scoring is margin-bound at filing time |
| #2850 | churn basis vs apply-path normalization disagree on `parent_epic` |
| #2845 | plans omit `requires_live_validation` |
| #2274 | migrate issues to `execution:agent` |

## 10. Verification discipline

Habits that cost time when skipped:

- Run `scripts/test verify`, never bare `go test ./...`. The latter bypasses the shared
  reused Postgres container and produces a wall of false failures (#1174/#972).
- Read exit codes without a pipe. `| tail` masks the real status; and zsh does NOT
  word-split unquoted parameters the way bash does, so `for n in $LIST` iterates once
  over the whole string.
- Check claims against the forge or the code, not against a summary. Two verify-status
  disputes cost real time in this epic, in opposite directions — one where reviewers
  rejected on a failure that did not reproduce, one where a self-report disagreed with a
  green gate.
