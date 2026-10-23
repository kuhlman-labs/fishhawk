---
id: ADR-110
title: "Product features are repository-agnostic: repository specifics are declared configuration, and this repository is a configured instance, not a special case"
status: accepted
date: 2026-10-21
issue: https://github.com/kuhlman-labs/fishhawk/issues/4605
supersedes: []
superseded_by: []
applies_to: []
---

# ADR-110: Product features are repository-agnostic: repository specifics are declared configuration, and this repository is a configured instance, not a special case

**Status: accepted (ratified by the Captain 2026-10-21).** Revised 2026-10-21 after independent fable and Codex reviews; both returned approve_with_concerns, and their findings are folded in below. This record is tracker-only for now: `docs/adr/` is frozen until the 2026-11-01 catch-rate re-pin, and the in-repo record follows the upkeep rule.

## Context

Fishhawk is built by dogfooding on its own repository. That is how most features are found and proven, and it is also how this repository's specifics leak into product designs.
- **ADR-103 (#4518) is the clearest case.** As written, its deterministic pre-pass hard-wires Fishhawk's own token classes, security convention doc and `scopeSweepOptOuts` table, and its eval corpus seeds only Fishhawk's own defects. Slice 4(a) (#4604) had to carry portability conditions to undo it.
- **The reviews found a live instance.** When a repository has no `.fishhawk/work-management.yaml`, `backend/internal/server/conventions_loader.go` falls back to the embedded default `backend/internal/workmgmt/defaults/work-management-default.yaml` (canonical copy `docs/spec/work-management-default.yaml`). That default declares `project: {owner: kuhlman-labs, owner_type: user, number: 7}`. A second repository with no declaration would therefore file work items against this repository's board.

The principle already exists piecemeal:
- ADR-031: platform-neutral review gates, and its "concrete-now / abstract-when-needed" discipline;
- ADR-055: forge-agnostic approval identity, plus portable preset approvals;
- ADR-058: GitLab support, delivered in phases;
- ADR-072: customer-supplied model credentials;
- ADR-074 and ADR-075: agents on customer infrastructure;
- ADR-080 / charter §2 T9: every crew role reaches its done-means "on this repository and on one repository that is not this one".

No record states it as a general rule for values, so each new feature re-decides it, or misses it.

## Options

- **A. Status quo.** Decide portability per feature in review. It keeps failing silently, because the dogfood repository is the only test bed.
- **B. A stated principle with definitions a reviewer can apply, enforced at review (recommended).**
- **C. B plus mechanical guards.**
  - A broad identifier lint (refuse `kuhlman-labs` anywhere in product code) is the wrong tool on both sides of the org migration. Product-home constants legitimately carry the publisher's name before and after E90, and declared configuration legitimately carries the approver identity.
  - Two NARROW guards are cheap and non-noisy now; see rule 6.

## Recommendation

**Option B, with the narrow guards from C.**

### Definitions

- **Governed repository:** the repository a run executes against, interprets or governs.
- **Product surface:** shipped behaviour that executes against, interprets or governs a governed repository, wherever its source lives (`backend/`, `runner/`, `cli/`, the schemas under `docs/spec/`, prompts, artifact shapes).
- **Repository-specific value:** a value that describes the governed repository: its owner/name, board, labels whose meaning exists only in its tracker, title formats, document paths, verify command and its knobs, approvers, and branch names.
- **Product default:** a value correct for any supported repository without reference to this one. It is a tool, language, forge or product-vocabulary convention, documented as a default in `docs/spec/` or the owning package README, and overridable by declaration. Examples: the built-in `agent_instruction_paths` class, the Go/TypeScript test-location rules, the `local-implement:<host>` group, the medium preset, the preset `approvals: {count: 1, not: [author, agent]}`, and deployment-default model selection (ADR-072). A default may be generalised FROM this repository's practice; once documented as a default, it is product.
- **Dogfood fallback:** a value correct only for this repository. Forbidden.
- **Product-home identifier:** where the product itself lives or what it publishes, such as the upstream repository product reports are filed to (`productRepo`), the published runner action, and the product URL in a PR footer. It is a product constant held in one place per binary, and outside this rule. E90 rewrites these as a release concern.
- **Product vocabulary:** values the product itself defines and consumes (autonomy tiers, run states, concern severities). These are product, not repository-specific.
- **Not a value:** an example in a message or document.
- **The reviewer's test for any literal.** Would a second repository with its own `.fishhawk/` get correct behaviour from it unchanged (product)? Wrong behaviour it can replace by declaration (it must be configuration)? Or wrong behaviour it cannot replace (a violation)?

### Rules

1. **Product surfaces carry no repository-specific values.** Each such value is declared on a repository configuration surface:
   - `.fishhawk/workflows.yaml`, for workflow and stage settings;
   - `.fishhawk/work-management.yaml`, for tracker conventions;
   - `.fishhawk/operator.yaml`, for operator identity and pointers;
   - a document one of these references by path.

   Deployment configuration and credentials stay where ADR-072/074/075 put them. These are configuration surfaces, not an exhaustive storage mandate.

   Governance documents used to judge a run resolve at the run's pinned admission commit (`document_base_commit`, E55.7), inherited across children and retries through the existing resolver. A candidate change never governs its own review. Other surfaces keep their documented resolution rules; a probe target's SHA is ADR-103's own case.

   This ADR governs VALUES, not abstractions. ADR-031's concrete-now rule for seams is unchanged: a second fixture configuration is required, a second implementation is not.

2. **This repository is a configured instance.** Its specifics live in its own declarations and documents, exactly as a customer's would. "Fishhawk's own configuration is the first instance" is the standard framing for a new configurable feature.

3. **Portability evidence.**
   - (a) **Fixtures.** A new or changed behaviour that interprets repository-dependent data ships with at least one non-Fishhawk fixture. The fixture is synthetic or real, and is exercised through the PRODUCTION resolver and consumer. It asserts that behaviour changes when the declarations change, and that the documented missing-declaration behaviour holds. Renaming the repository alone is not enough, and shared fixtures are allowed. The rule also applies to a behaviour with a hard-coded repository dependency, so a feature cannot evade it by consuming no configuration at all.
   - (b) **Portability arm.** A feature measured under the catch-rate gate carries a portability arm before it is enabled beyond this repository: representative defective and clean cases targeting a repository other than this one, with that repository's own declarations, judged on catch and false-alarm rates with versioned evidence and stated thresholds. `security_portability` is the ABSENCE half only: a finding must not name a control the reviewer was not shown. It does not substitute for a second target. Rule 3(b) is ADR-080 T9 applied per measured feature, and its second target is the same repository T9 uses. No arm can be recorded before the 2026-11-01 re-pin.
   - (c) **Scope.** Portability applies within each feature's declared supported capabilities and phase: forge coverage stays ADR-058's phased decision, and the runner image stays ADR-075's. It neither requires speculative providers nor expands MVP scope (MVP §2). A GitHub-only feature is not a violation if its GitHub-ness is declared (`provider:`), not assumed. Unsupported capabilities must be explicit. Fixture coverage is a development requirement and does not replace ADR-080's external-repository readiness demonstration.

4. **Missing declarations.**
   - A REQUIRED control that needs a repository declaration and doesn't find one refuses, with a named reason.
   - An advisory or optional feature may degrade instead, only under its documented failure policy, with the reason recorded on the audit chain. Existing vocabulary: `review_convention_missing` refuses; `optional_document_missing` / `document_injection_degraded` degrade.
   - The feature's spec reference states which of the two applies.
   - It never substitutes a dogfood fallback.
   - A product default is not a fallback and needs no declaration, and a missing override is not a missing required declaration.

5. **Repository tooling is out of scope, classified by use, not directory.**
   - This repository's operating kit may be as specific as it likes: `scripts/`, `.agents/skills/`, its CI workflows, `AGENTS.md`, generators of this product's own documentation such as `cli/internal/docgen`, and the evidence harnesses that measure this instance (`backend/internal/agenteval`, `docs/compliance/`).
   - Tooling may be the reference implementation of a product contract only when three conditions hold:
     - the contract is written in `docs/spec/` as product-facing (the verify exit-code contract already is);
     - the product behaves correctly for a repository whose tooling does not implement it (fail-open to today's behaviour, never fail-closed on the contract's absence);
     - every knob the product mirrors is stated in the contract rather than read from the script.

     A test binding product code to the script text is then a conformance test, and is allowed.
   - A product surface that interprets tooling (for example the runner's scoped-verify protocol) is product behaviour under rules 1–4, even though the script is exempt.

6. **Enforcement.**
   - **At review.** Plan and implement reviewers check rules 1–4 against the definitions. Until the architect remit gains a portability item, which is edited with the 2026-11-01 re-pin batch because `docs/review/` is a trigger path, the first-officer seat (the overseer) applies the rule at plan approval. Unclear cases are escalated to the Captain.
   - **Two narrow guards, filed now.**
     - (i) A test that the shipped work-management default declares no `project:` connection, and that filing with no repository declaration refuses with a named reason (e.g. `work_management_unconfigured`).
     - (ii) A `scripts/test verify` leg that fails on a literal path under `docs/review/`, `docs/adr/`, `.fishhawk/` or `scripts/` in non-test Go under `backend/`, `runner/` and `cli/`. It has a reviewed allow-list, each entry carrying a reason and an owner. Adding the leg obliges `backend/internal/basedelta/verifylegs_test.go` (ADR-096 D3).
   - **After E90.** A broader identifier lint is revisited once the org migration lands and the product-home constant set is known. E90 updates publisher coordinates and the guards' allow-lists; it does not satisfy this ADR or postpone detection of new coupling.

## Decision

**Accepted 2026-10-21** by the Captain (repository maintainer): **Option B with the narrow guards from C**, i.e. the Definitions and Rules 1–6 above, as revised after the fable and Codex reviews. The E89 debt inventory and the two guards are filed with this ratification.

## Consequences

- **Cost.** New configurable features cost slightly more up front (a configuration surface plus a non-Fishhawk fixture). The cost is paid once instead of per adopter.
- **Existing debt.** It is inventoried, not fixed, by this ADR, and it does not authorize new coupling. Each item is fixed, or recorded as an exception with its surface, rationale, owner, external-enable restriction and review trigger, in one E89 inventory issue filed with ratification. Seeds:
  - the work-management default's `project:` block and its Project-7-shaped title formats (guard (i));
  - the plan sweep's hard-coded `docs/issue-comment-surfaces.md` rule (`backend/internal/server/surface_sweep.go`) and the test sweep's Fishhawk migration and generated-doc paths (`test_sweep.go`);
  - `verifyLockWaitBudget` (600 s) and the flake-absorption attest format, which move into the verify contract in `docs/spec/` (`runner/cmd/fishhawk-runner/verifylockcontention.go`, `verifyabsorb.go`);
  - the label literals `area:backend` (`live_validation_filing.go`) and the `type:`/`phase:` namespace constants, which should be read from the loaded conventions;
  - `backend/internal/prompt/prompt.go`'s "enforced by `scripts/test verify`" sentence and `backend/internal/failuresig/registry.go`'s playbook text (frozen path until 2026-11-01);
  - **accepted exceptions:** `productRepo`, the runner action ref, the PR footer URL and the manifest URL (product-home, rewritten by E90); and the agenteval persona owner and paths (instance harness, rule 5).
- **Record metadata.** The in-repo record carries `applies_to: []`. Its relation to ADR-103 is related, not amends.
  - Portability is enforced through rule 6 (plan approval now, the architect remit's portability item after the re-pin), NOT through the architect persona's ADR-084 decision-record selection.
  - Why: selection ranks records by matched changed paths, so a blanket-glob record matching nearly every change would rank first in almost every architect review. This record's ~14 KB body would then crowd specific records out of the 32 KB record budget.
  - *Amended 2026-10-21 on the Captain's direction*, after recording this ADR showed exactly that: ADR-046 dropped from a workflow-schema review.
- **Related:** ADR-031, ADR-055, ADR-058, ADR-072, ADR-074, ADR-075, ADR-080, ADR-084, ADR-096, ADR-103 (#4518) and its slice #4604. Epics E89 #4537 and E90 #4540.

## Review history

- **2026-10-21, fable (approve_with_concerns, 10 findings) and Codex (approve_with_concerns, 8 findings).** Changes made from them:
  - added definitions: product surface by use, product default vs dogfood fallback, product-home identifier, product vocabulary;
  - split rule 4 into required (refuse) and optional (degrade with an audited reason) behaviour, and permitted documented product defaults;
  - corrected document resolution to the admission commit;
  - added `.fishhawk/work-management.yaml`, and separated deployment configuration and secrets;
  - reframed `security_portability` as absence-only, and made rule 3 assert behaviour under changed declarations;
  - kept ADR-031/058/075 phasing and the ADR-080 bar;
  - replaced the deferred identifier lint with two narrow guards now;
  - named the live work-management violation and seeded the inventory;
  - named the enforcement roles;
  - stated `applies_to`.
