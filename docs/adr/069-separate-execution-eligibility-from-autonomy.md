---
id: ADR-069
title: "Separate execution eligibility from autonomy: an `execution:` namespace, type-derived workflow eligibility, and the emergency bypass as an audited action"
status: accepted
date: 2026-07-26
issue: https://github.com/kuhlman-labs/fishhawk/issues/2268
supersedes: []
superseded_by: []
applies_to: []
---

# ADR-069: Separate execution eligibility from autonomy: an `execution:` namespace, type-derived workflow eligibility, and the emergency bypass as an audited action

## Context

ADR-066 (#2209) and its implementation slice E53.7 (#2267) settled that the issue `autonomy:*` label and the workflow spec's autonomy tier are **one ordered scale**, composing as `min(workflow tier, issue label, escalation ceiling)`. That resolved the interaction question. It surfaced two problems it did not solve.

**1. `autonomy:low` carries two different facts.** It means *both* "a human authors this change" *and* "nothing is delegated at gates." `docs/METHODOLOGY.md` §"Low autonomy (human-led)" states the first: *"Human writes the code. Agents may assist… but the human is the author and reviewer of record."* That is an **authorship** claim, not a delegation setting. The conflation is visible in the code: `backend/internal/server/campaigns.go` treats `autonomy:low` as a **dispatch gate** — deps-satisfied low items are diverted out of `Eligible` into `HumanLed` and refused with `item_human_led` — while `medium` and `high` are pure delegation settings that never affect dispatch. The special case exists precisely because `low` means something categorically different from the other two values on its own scale.

Note this is not a claim that human-authored work escapes governance: the repo's own `human_led_change` workflow has **no implement stage** but still runs, gates on approval, and produces an audit record.

**2. Some work items cannot go through a workflow at all, and nothing handles it.** Epics, ADRs, and non-code business/ops work (this repo's E11.1 sourcing pipeline, E11.5 partner agreements, domain registration) have no diff and no run. Today:

- **Campaign eligibility has no type awareness.** The only human-led diversion is the `autonomy:*` label check; nothing in `backend/internal/campaign/` filters by item type.
- **The default is fail-open.** `backend/internal/campaign/engine_test.go:190` pins it: *"autonomy unset → eligible (unknown defaults to non-human-led)"*. An item filed outside the `fishhawk_file_issue` path (which defaults `autonomy:medium`) and carrying no autonomy label is dispatchable.
- The conventions already recognise the shape without enforcing it: `epic` and `adr` carry **no** autonomy default because "they have their own conventions" (`work-management-v0.md`).

**3. The obvious fix does not work.** A founder discussion on 2026-07-26 considered adding `autonomy:none`. It fails two structural tests: it is **not a point on the scale** (`min(medium, none)` is not "very constrained" — it is "no workflow exists to constrain"), so it does not compose; and unlike every other input to that resolution it is **clamped by nothing**, making it a governance exit with no counterweight, applicable by anyone with triage rights, in the least conspicuous place available. `docs/METHODOLOGY.md` §"No founder bypass under pressure" requires emergency paths to be *audited and to require post-hoc justification* — the opposite of a quiet fourth enum value.

**Namespace constraint.** `workflow:` should stay unallocated: ADR-066's `applies_to` predicate reads `labels:` (E53.3 #2226), so routing labels of the form `workflow:<id>` are a plausible near-term need. Spending that namespace on exemption forecloses it.

## Options

1. **Status quo.** `autonomy:*` keeps both meanings; eligibility stays unhandled and fail-open. Rejected: non-code items remain dispatchable by default, and the conflation has now resurfaced twice — once as the campaign's binary special case, once as "is `low` even on the same scale as `medium`?"

2. **Add `autonomy:none` as a fourth value.** Rejected for the three structural reasons above: it does not compose under `min()`, it is unclamped, and it dilutes the conspicuousness a bypass requires by living in a namespace present on every issue.

3. **A sparse boolean eligibility label** (e.g. `run:exempt`, absent = eligible), leaving `autonomy:` untouched. Partial: solves eligibility cleanly and cheaply, but leaves the `autonomy:low` conflation in place to resurface again.

4. **An `execution:` namespace separating authorship from delegation**, plus type-derived structural eligibility, plus the emergency bypass as an audited action rather than a label.

   | Label | Who authors | Run happens? |
   |---|---|---|
   | *(absent)* / `execution:agent` | agent | yes |
   | `execution:human` | human | yes — a workflow with no implement stage |
   | `execution:none` | nobody; not workflow work | no |

   `autonomy: low \| medium \| high` then means **only** "how much the operator agent decides at gates," applying wherever a run exists. METHODOLOGY's tiers decompose: low = `execution:human` + `autonomy:low`; medium and high = `execution:agent` + their tier.

5. **Derive everything from type alone**, with no new label. Rejected: type cannot distinguish a `chore` that is a code change from one that is not (E11.1 and E48's chores are both `chore`), so it under-covers.

## Recommendation

**Option 4**, with three bindings that make it safe rather than merely tidy:

**a. `execution:none` is rejected by validation on code types** (`feature`, `bug`). This is what stops the label becoming the bypass — the failure mode that disqualified `autonomy:none`. The label stays rare and therefore conspicuous.

**b. Structural ineligibility derives from type, not the label.** Extend the existing `epic`/`adr` precedent: types declare `workflow_eligible`. Epics and ADRs need no label and cannot be relabelled around, since changing an item's type changes its title format and is conspicuous. `execution:none` covers only the residue type cannot catch — non-code work filed as `chore`.

**c. The emergency bypass is not a label at all.** It is an audited action carrying a recorded reason, in the same family as a waiver or a policy override. **Labels do not carry reasons**, and METHODOLOGY requires post-hoc justification; a tag cannot satisfy that contract. This is the sharpest reason to prefer option 4 over option 3 with a permissive escape hatch.

**Resolution order.** `execution:none` short-circuits *before* autonomy resolution — there is no run whose posture needs computing. `execution:human` selects a workflow with no implement stage; `autonomy:` then resolves normally via E53.7's `min()`, which stays a clean three-value scale with no special case.

**Fix the fail-open while here.** An absent `autonomy:*` label should resolve to the conventions default (`autonomy:medium`) rather than to "unknown → dispatchable" (`engine_test.go:190`). An unlabelled item should not be *more* permissively treated than a labelled one.

Option 3 remains a reasonable smaller step if the appetite for touching `autonomy:` semantics is low; it solves the question asked and defers the conflation.

## Decision

**Accepted (2026-07-26).** Adopt **Option 4** — the `execution:` namespace separating authorship from delegation, with the three bindings in the Recommendation. Three forks were settled at ratification; where they refine the Recommendation, **these govern**.

### 1. Scope: the full split

```
execution:  agent (absent default) | human | none
autonomy:   low | medium | high      — gate delegation ONLY
```

METHODOLOGY's tiers decompose: low = `execution:human` + `autonomy:low`; medium and high = `execution:agent` + their tier. `autonomy:` becomes a clean ordered scale, so E53.7 (#2267)'s `min(workflow tier, issue label, escalation ceiling)` composition holds with no special case, and the campaign's `HumanLed` diversion becomes a consequence of `execution:human` rather than a bespoke branch keyed on a delegation label.

`execution:none` short-circuits **before** autonomy resolution — there is no run whose posture needs computing. It is **rejected by validation on `feature` and `bug`**, which is what stops the label becoming the bypass. Structural ineligibility for epics and ADRs derives from **type**, extending the existing precedent where those types carry no autonomy default.

### 2. Emergency bypass: an audited action, never a label

A code change that must skip the workflow is exempted by a **recorded action carrying a reason**, in the same family as a waiver or a policy override — not by a tag. **Labels cannot carry reasons**, and `docs/METHODOLOGY.md` §"No founder bypass under pressure" requires emergency paths to be audited and to require post-hoc justification; a label structurally cannot satisfy that contract. This is the sharpest reason Option 4 beats Option 3 with a permissive escape hatch.

### 3. Migration: default to workflow-eligible; migrate only what an agent *cannot* do

**This repo is explicitly agent-driven for code changes** (`docs/METHODOLOGY.md`; every substantive change has flowed through a workflow run since Day 22). So the migration default is **`execution:agent`** — existing `autonomy:low` issues stay agent-authorable and keep `autonomy:low` meaning what it should now mean: *every judgment is gated*, not *a human types it*.

**The exception is derived, not judged.** An issue is `execution:human` when its **primary deliverable lands in a path the implement stage forbids** — this repo's `forbidden_paths` are `.github/workflows/**`, `.fishhawk/**`, `LICENSE`, `NOTICE`. If the agent is structurally barred from the files, a human must author it. That is a checkable rule rather than a taste call, and it derives from the workflow spec the repo already enforces.

**Sizing (2026-07-26): 51 open `autonomy:low` issues; roughly 2–3 qualify.** A text scan matched 13, but most are false positives — they *mention* `.fishhawk/` because they discuss the workflow spec while their changes land in `backend/internal/spec/` and `docs/spec/`. Genuine cases look like **#1418** (`deploy.yml`, a `.github/workflows/` file) and **#2253** (LICENSE.md). The rest are `execution:agent` + `autonomy:low`.

**The test is PRIMARY deliverable, not mention.** Several E52/E53 issues do the bulk of their work in agent-allowed paths plus one small `.fishhawk/` dogfood step; those stay `execution:agent`, with the operator committing that step — which is already how this repo dogfoods a workflow-config change. Classifying on "touches" rather than "is primarily" would sweep most of the spec work into human authorship and contradict the methodology.

**Non-code items** (business/ops work filed as `chore` — e.g. #2256 partner sourcing, #2260 partner agreements) migrate to `execution:none`.

### 4. Fix the fail-open while here

An absent `autonomy:*` label must resolve to the conventions default (`autonomy:medium`) rather than to "unknown → dispatchable" (`backend/internal/campaign/engine_test.go:190`). An unlabelled item should never be treated *more* permissively than a labelled one.

Named approver: repository maintainer (human).

## Consequences

An operator reading an issue can answer two separate questions from two separate labels — *who does this work* and *how much does the agent decide* — instead of inferring both from one value whose meaning changes at the bottom of its range. `autonomy:` becomes a clean ordered scale, so E53.7's `min()` composition holds without a special case, and the campaign's `HumanLed` diversion becomes a consequence of `execution:human` rather than a bespoke branch keyed on a delegation label.

Non-code work stops being dispatchable by default, closing a fail-open that currently depends on the filing path having applied a label.

Costs and risks: a second namespace is a second thing to read, mitigated by `execution:agent` being the absent default so normal issues carry nothing. Existing `autonomy:low` issues carry an implied `execution:human` that must be migrated or interpreted — a decision this ADR must settle rather than leave to inference. METHODOLOGY §"Autonomy tiers" and the preset library's tier mapping both need rewriting against the split. And the emergency-bypass action is new surface area that does not exist today: until it ships, a genuine emergency has no sanctioned path, which is arguably already true but becomes explicit.
