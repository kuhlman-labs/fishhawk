---
id: ADR-147
title: "Execution and data boundary: where agents run, what a control plane stores, what reviewers read"
status: accepted
date: 2026-11-14
issue: https://github.com/kuhlman-labs/fishhawk/issues/5273
supersedes: []
superseded_by: []
amends: ["ADR-027", "ADR-057", "ADR-062", "ADR-066", "ADR-072", "ADR-074", "ADR-078", "ADR-084"]
applies_to: []
---

# ADR-147: Execution and data boundary: where agents run, what a control plane stores, what reviewers read

**Status: accepted (ratified 2026-11-14).**

## Context

Prepared at the Captain's request on 2026-11-14, from the ratified-ADR coherence review of that date. Repository citations are to `main` at `4e40a1e96`.

Accepted records disagree about where agents run and what a control plane holds.

**Where reviewers run.**
- ADR-074 decides: "**`fishhawkd` orchestrates and records. It never executes an agent.**" (`docs/adr/074-reviewer-agents-run-on-customer-infrastructure.md:86`) and "**No backend fast path.**" (`:111`).
- ADR-072 decides "Reviews run control-plane-side" (`docs/adr/072-customers-bring-own-model-credentials-byok.md:82`) and "The control plane holds and *uses* customer credentials" (`:84`).
- ADR-084 rule 6: "In-process in fishhawkd (N6). When runner-hosted reviewers land (E63 #2307, ADR-074), personas move with them." (`docs/adr/084-reviewer-personas.md:51`).
- ADR-138 D4 keeps the two subprocess flags off on the hosted node (`docs/adr/138-azure-k3s-hosting-substrate.md:44`). It does not turn off the `anthropic` SDK reviewer, which runs in the fishhawkd process when `FISHHAWKD_ANTHROPIC_API_KEY` is set (`backend/cmd/fishhawkd/serve.go:528-543`) and is the provider the chart documents (`deploy/helm/fishhawk/README.md:300`).

**What a control plane holds.**
- ADR-057 justifies pooled tenancy with "Because source never touches the backend and compute is offloaded, pooled logical isolation is an acceptable posture for the metadata we hold." (`docs/adr/057-multi-tenancy-model-tenant-isolation.md:56`).
- Stored trace bundles carry the diff, which review recovery extracts with `bundle.ExtractDiff` (`backend/internal/server/review_redispatch.go:866-873`).

**Which tree a reviewer reads.** ADR-074 §1: "a **clean checkout at the base ref**" (`docs/adr/074-reviewer-agents-run-on-customer-infrastructure.md:92`). ADR-078 rule 4: "**HEAD tree, not base.**" (`docs/adr/078-ground-review-agents-widen-context-narrow-capability.md:59`), with requirements that "should be carried across" to runner-hosted reviewers (`:73`).

**Grounding and gating.**
- ADR-078 accepts its residual "because reviewer verdicts are ADVISORY", "a standing reason not to make reviewer verdicts binding without revisiting this ADR" (`docs/adr/078-ground-review-agents-widen-context-narrow-capability.md:69`; reinforced at `:97`).
- Grounding is a deployment flag, off by default (`backend/cmd/fishhawkd/serve.go:2235`), "suitable only for a trusted single-tenant host" (`docs/adr/078-ground-review-agents-widen-context-narrow-capability.md:95`).
- Reviewer authority is declared as `reviewers.authority: advisory | gating` (`docs/adr/066-workflow-declared-autonomy-tier.md:36`, `:92`; `docs/spec/workflow-v2.schema.json:1319`). This repository's `routine_change` declares `authority: gating` (`.fishhawk/workflows.yaml:1047`, `:1102`).

**Regional inference.** ADR-062 adopts "per-cell inference configuration" (`docs/adr/062-regional-cells-control-plane.md:42`), a control-plane credential which ADR-074 removes.

**Phase.** ADR-080 moved hosted, multi-tenant and Kubernetes work to beta (`docs/adr/080-solo-captain-alpha.md:82-83`).

## Options

- **A. Leave the records as they are and finish E63.** The contradictions end when reviewers move, except the stored diff, the reviewer tree and the gating rule, which E63 does not touch. Until then ADR-074 reads as a fact that the local loop breaks every day.
- **B. Withdraw ADR-074's invariant and treat it as a goal.** This matches today's code. It gives up the hosted trust claim, and it removes the constraint ADR-104 was designed around (`docs/adr/104-host-dispatcher.md:43`).
- **C. Keep the invariant, name one bounded local exception, and state the data boundary truthfully (recommended).** One record says where agents run, what is stored, which tree is read and when a reviewer may gate.

## Recommendation

**Option C.**

**D1. ADR-074 is an invariant, with one exception.**
- For every hosted deployment and every customer-installed deployment, fishhawkd never executes an agent. ADR-074's Decision stands for them, except where D2 and D3 replace it. A customer-installed deployment therefore has no agent review until E63 lands; the in-process `anthropic` reviewer its chart documents today is not an exception.
- The exception is a single-tenant local deployment in which fishhawkd and the runner run on the same machine for one account. There, fishhawkd may run in process the agents it runs today: reviewers, personas (ADR-084 rule 6) and the refinement drafting agent (`backend/cmd/fishhawkd/serve.go:862-884`). The exception is those paths and nothing wider.
- Outside the exception every in-process agent path is out of contract: each reviewer adapter, the SDK adapter included, personas, and the refinement drafting agent. Once Direction (c) is built, fishhawkd refuses each of them; until then D1 is declared, not enforced.
- The exception ends when the E63 #2307 change is merged to main and, in that change or an earlier one, every in-process agent path has been removed, the refinement drafting agent included; the removal covers the local stack. If E63 #2307 does not itself remove the drafter, a separate change removes it no later than E63's merge; that removal is also a beta-entry obligation. E63 is a prerequisite for beta entry and for agent review on any hosted deployment.
- How a deployment declares that it is a single-tenant local deployment is Direction (c). No configuration signal carries it today: an unset `FISHHAWKD_SINGLE_TENANT_ACCOUNT_KEY` means hosted multi-tenant and is what the local stack runs, and a set key is also what a customer-installed deployment runs (`backend/cmd/fishhawkd/serve.go:1928-1932`). Until (c) is decided and built, D1 is declared, not enforced: the deployment's configuration must meet it. Keeping `FISHHAWKD_ENABLE_LOCAL_CLAUDE_REVIEWER` and `FISHHAWKD_ENABLE_CODEX_REVIEWER` off (which ADR-138 D4 already decides for the hosted node) and leaving `FISHHAWKD_ANTHROPIC_API_KEY`, `FISHHAWKD_MODEL_API_KEY` and `FISHHAWKD_OPENAI_API_KEY` unset are the operator's obligation; `FISHHAWKD_OPENAI_API_KEY` is both the Codex reviewer subprocess's credential and the model oracle's key. fishhawkd does not refuse. The D1 refusal tests are owed by that change. If E63 merges first, the D1 refusal tests lapse: no in-process path remains to refuse. Direction (c) does not lapse with them: D2's `working_dir` rule still needs its signal. The signal (c) chooses defaults to not-local, so an undeclared deployment gets no exception.

**D2. What a hosted control plane holds.**
- A hosted control plane never holds a repository checkout and never runs inference. Once E63 lands it holds no model key. The model-list check that validates a declared model at run creation loses its credential with it; whether that check moves to the runner, uses an unauthenticated source, or is dropped is Direction (e), decided before the E63 change merges because the model oracle holds the same keys (`backend/cmd/fishhawkd/serve.go:1686-1693`). Until then the model oracle's provider model-list check (#1341), which is not part of ADR-072 §3, fails open for a provider with no key (`backend/cmd/fishhawkd/serve.go:2488-2491`); ADR-072 §3's deployment allow-list (`:2497`) needs no key, and its call-time check of the customer's credential still holds. D1 replaces ADR-072 §2's control-plane execution rule for every deployment, subject only to D1's local exception. ADR-072 §1's control-plane model-key custody ends, for a hosted or customer-installed deployment, when that deployment meets D1: by the operator's obligation until E63 lands, and by construction after it. Its later per-account encrypted storage of customer model keys on the control plane is not built under this record; where a runner obtains a customer's key is outside this record. BYOK itself is unchanged. No hosted path may read a run's `working_dir`. Today the grounding export and the merge-candidate transfer inputs read whatever checkout a run names (`backend/internal/server/runs.go:1493`, `backend/internal/server/merge_candidate_transfer.go:400-415`); they are permitted only on a local deployment. Until Direction (c) this is declared, not enforced.
- A control plane stores signed trace bundles, and those bundles include the change's diff. This is the product's record and it stays. Today it stores a raw and a redacted variant of each bundle under a run-keyed object path (ADR-003), isolates them by the run's account at the API handler, and declares no retention period. From beta entry a hosted control plane must (i) serve only the redacted variant on every read path, until Direction (f) decides who, if anyone, may read raw, (ii) refuse, in the trace store itself, a read or write whose account is not the run's account, on internal paths as well as handlers, and (iii) declare a retention period. These obligations leave ADR-003's key layout unchanged; how (ii) is built, and any layout change, which needs an ADR-003 amendment, are Direction (f). These are obligations, not present facts.
- ADR-057's Invariant #1 (`:30`) and its rationale (`:56`) are restated: the control plane holds no checkout and runs no agent or inference on source. It does receive and store source-derived content: the diff in trace bundles, and code excerpts and file paths in plans and review comments, as ADR-057 Amendment A2 already records. It also reads from the forge, at named commits, the change's diff where a review or check is built from a pushed head rather than a trace bundle, and the repository files D3 has it inject (the workflow spec, convention and remit documents, decision records); it stores the workflow spec with the run and places the others in prompts. It runs deterministic checks over the diff (policy evaluation, redaction, secret detection, review recovery and prompt assembly). A logical-isolation defect therefore exposes source-derived content, not only metadata. Pooled isolation is re-accepted on that basis by the Captain at beta entry, or changed then. Until then ADR-057's decision stands for the code already built.
- ADR-074's sentence "The control plane stops processing customer code entirely" (`docs/adr/074-reviewer-agents-run-on-customer-infrastructure.md:133`) is replaced by the three statements above. Backend prompt assembly (`:73`) stands until Direction (a) decides otherwise.

**D3. The tree a reviewer reads.**
- An implement review reads the candidate HEAD commit the server names in the dispatch, which is a commit the forge holds (the pushed branch head, not a runner-local verify commit), with the trusted base commit, exported into a fresh, isolated invocation that inherits nothing from the implementing agent's working tree or environment. An implement review is not dispatched until the server holds such a commit. The signed verdict carries the commits it read, and the server refuses a verdict whose commits are missing or are not the ones it dispatched. This replaces ADR-074 §1's "clean checkout at the base ref" and keeps its independence requirement.
- The server resolves at the base commit the files a review is judged against (the workflow spec, declared convention and remit documents, and selected decision records) and injects them. The exported HEAD tree still holds the candidate's versions of those documents; the prompt says the injected base text governs and that a differing copy in the tree is part of the change under review. Files a reviewer CLI would load as its own instructions or configuration (the agent-instruction paths the export withholds today, `backend/internal/reviewsandbox/tree.go:88-94`) are never materialised in the export; the reviewer sees a change to them only in the diff.
- A plan review has no candidate and reads the base.
- The prompt states which commit the reviewer is reading (ADR-078 rule 4).
- ADR-078's requirements on the reviewer's environment and capability (its rules 1, 2, 3 and 6: a scrubbed environment, export cleanup, read-and-search-only tools, and the prompt-and-sandbox contract test, with the MCP isolation its Correction records as live) apply to the runner-hosted invocation unchanged, as does its Correction's finding that an export does not bound what the reviewer can read. The Correction's consequent decision, that grounding is off by default and an operator's choice, is replaced for runner-hosted reviews: D3 gives every one a tree.

**D4. A grounded reviewer does not gate.**
- fishhawkd refuses at run admission, on every entry point that creates a run (operator start, webhook, schedule, campaign item and child run), when grounding is enabled and any stage of the run's resolved workflow gives its agent reviewers effective gating authority. The refusal is an audit row. A run already admitted is checked again before a review is launched after a restart, a change of grounding or the change to runner-hosted reviews. A run that fails the re-check launches no review and its stage does not advance: it parks for a human with the same audit row. The human may cancel the run; no action launches a gating grounded review. Whether a human may instead decide the stage in place of the agent review, and by which verb, is stated by the change that records this ADR. Before the E63 change, turning grounding off and resuming is also a remedy. A deployment diagnostic may list affected workflows it already knows; it is not a boot refusal.
- Effective authority is the stage's resolved authority (`backend/internal/planreview/review.go:1028-1046`): the declared `reviewers.authority`, or ADR-027's count-derived rule where none is declared. A stage with no agent reviewer is gateless and is not refused. Every reviewer invocation on the stage carries it, including a persona attached statically or by an escalation (ADR-084 rule 3), so the check is per stage.
- A review is grounded when its invocation is given a repository tree. While reviewers run in process, that is a subprocess adapter on a deployment with the grounding flag on. From the merge of the E63 change, every review under D3 is given a tree, so D4 refuses a gating agent reviewer on every deployment until Direction (b) is decided. The dispatch records, per review, whether it was given a tree.
- The refusal names the workflow, the stage, whether the authority is declared or count-derived, and the remedy. Before E63, a declared `gating` is remedied by declaring `advisory` or turning grounding off; a count-derived `gating` also by setting `reviewers.human` to 1 or more. For runner-hosted reviews, turning grounding off is not a remedy and the refusal does not name it. Adding an approval gate does not change reviewer authority and is not a remedy. On a stage with no human reviewer and no approval gate, `advisory` leaves no reviewer able to stop the stage; the refusal says so.
- The rule lasts until an ADR ratifies the containment and evaluation conditions under which a grounded reviewer may gate (Direction (b)). This applies ADR-078's standing condition; it does not widen it.
- While reviewers run in process it is a server refusal, not a schema rule: the workflow validator cannot see a deployment flag.
- This repository's `routine_change` is refused on a grounded host until its declaration changes; the change or retirement is this repository's decision, made outside this record.

**D5. Regional inference is deferred to beta.**
- ADR-062's per-cell inference configuration creates no alpha obligation.
- From beta, on a region-scoped cell (ADR-062), dispatch refuses a review unless an approved binding of runner, model endpoint and region exists for the account's home region. A deployment whose accounts have no home region is not checked. What approves a binding and how a runner's region is verified are Direction (d).
- Existing credential-withholding guards (`backend/cmd/fishhawkd/serve.go:645-666`) stay until their execution paths are retired.

**Directions (not decided here; each needs its own ADR or amendment).**
- **(a) Source-bearing prompts.** Whether diff injection moves to the runner, so that a hosted control plane assembles no source-bearing prompt.
- **(b) Gating by a grounded reviewer.** The containment and evaluation conditions that lift D4. It is a beta priority.
- **(c) Deployment mode.** How a deployment declares or proves that it is single-tenant and local.
- **(d) Regional binding.** The approver, the evidence for a runner's region, and the refusal cases; whether the binding also governs stage agents, and how a deployment with no home region is bound.
- **(e) Historical data and cutover.** Trace bundles and review records written before this record; keys held by existing cells; the order in which a cell stops holding a key; the model-list check's credential.
- **(f) Retention, the raw tier and store isolation.** Minimum and maximum retention, deletion evidence, their relation to ADR-134's data disposition authority, and whether a hosted cell may hold ADR-003's raw variant at all; how D2's obligation (ii) is built, and any change to ADR-003's key layout.

## Decision

**Accepted 2026-11-14** by the Captain (1 of 1; manual ratification path, the ADR-113 D2 verb is not yet built; approver, erratum and hashes in the ratification comment). **Option C**, i.e. D1 to D5 as stated in the Recommendation, decided now: fishhawkd never executes an agent on a hosted or customer-installed deployment, with one single-tenant local exception that ends when E63 #2307 lands; a hosted control plane holds no checkout and runs no inference, and its stored trace bundles carry the obligations D2 states; a reviewer reads a server-named candidate commit with policy at base; a grounded reviewer does not gate; regional binding is deferred to beta. It amends ADR-027, ADR-057, ADR-062, ADR-066, ADR-072, ADR-074, ADR-078 and ADR-084 only as stated. The Directions are not decided. This record builds nothing by itself.

## Consequences

- **Scope.** This record builds nothing by itself. D1 and D3 describe E63's target. D4 is one new refusal. D2 corrects statements and sets three storage obligations for hosted deployments from beta entry; it builds none of them.
- **What this amends.**
  - ADR-074: the Decision's principle gains D1's exception; its "No backend fast path" (`:111`) is qualified only by D1's exception; §1's base-ref checkout is replaced by D3; the Consequences sentence at `:133` is replaced by D2.
  - ADR-072: §2's control-plane review is replaced by D1 for every deployment; §1's control-plane key custody ends as D2 states; §1's per-account control-plane key storage is not built (D2).
  - ADR-084: rule 6 is confined to D1's exception and ends with it.
  - ADR-078: rule 4 is restated for runner-hosted reviews (D3), and its standing condition becomes an enforced refusal (D4); the Correction's default-off rule does not apply to runner-hosted reviews (D3).
  - ADR-027: the `human == 0` and `agent >= 1` row of its authority table (agent reviewers are the gate) is refused at run admission under D4's condition; the table is otherwise unchanged.
  - ADR-066: a declared `gating` authority is refused under the same condition. The grammar is unchanged.
  - ADR-057: Invariant #1 at `:30` and the rationale at `:56` are restated by D2; the tenancy decision is unchanged. The region-scoped reviewer key at `:60` and `:78` follows ADR-062. `docs/ARCHITECTURE.md` §6 invariant 1 and `docs/MVP_SPEC.md:274` are revised to match when this record lands.
  - ADR-062: the per-cell reviewer key of its inference configuration ends for a hosted cell as D2 states, and from beta the region rule is D5's binding. Its directory, home-region pinning and other residency decisions are unchanged.
- **Costs.**
  - A hosted or customer-installed deployment has no agent review until E63 lands, so a workflow whose agent reviewers gate cannot be admitted there; the run-creation message at `backend/internal/server/runs.go:4021` and the chart text at `deploy/helm/fishhawk/README.md:300` tell the operator the opposite and are corrected when this record lands.
  - Outside the exception, the agent-backed refinement arms (session creation and brief amendment) are unavailable until the drafter runs outside fishhawkd.
  - A grounded host cannot run a workflow whose agent reviewers gate.
  - Once reviews are runner-hosted, no agent reviewer gates on any deployment until Direction (b).
- **Residuals.**
  - Under D1's exception the local fishhawkd holds model credentials and launches reviewers as its own subprocesses. A reviewer reads untrusted diff and issue text, and with grounding on it can read any file the fishhawkd user can read (ADR-078, Correction). On the single-host local stack "A process with a shell on the deployment host ... holds the deployment" (`docs/adr/145-proof-of-presence-recovery.md:55`); host ownership is not containment. D1 grants no exemption from ADR-148 (#5274) or ADR-149 (#5275), which state the human-credential and confinement residuals for the same host.
  - A runner-hosted reviewer can read any file the runner user can read, and its verdict text reaches the control plane (ADR-078, Correction). E63 preserves ADR-078's export cleanup, capability restrictions, MCP isolation and prompt and sandbox contract tests, together with the existing per-adapter read protections: Codex's filesystem allow-list and Claude's credential-root blocklist (`backend/internal/reviewsandbox/confine.go:10-20`). These controls do not by themselves satisfy Direction (b), and Claude's blocklist leaves other runner-readable paths exposed.
  - A hosted control plane still assembles prompts that contain a diff, until Direction (a).
  - D5's binding does not exist yet; before beta nothing checks a runner's region.
  - D4 covers authority to block. A grounded advisory reviewer's approve verdict still satisfies `clean_dual_approval`, the condition under which a delegated approve acts without a human; whether grounding must also withhold that condition is Direction (b).
  - D4's `advisory` remedy, applied to a stage with no human reviewer and no approval gate, leaves a reviewer reject unable to stop the change. D4 does not refuse that declaration; whether an unattended stage must keep a blocking control is Direction (b).
- **Beta-entry obligations.** E63 #2307 merged (D1), the refinement drafting agent removed from fishhawkd no later than E63's merge, if E63 does not remove it (D1), the Captain's recorded re-acceptance or change of ADR-057's pooled isolation (D2), D2's three storage obligations, Direction (c) ratified and built, so that D2's no-checkout rule is enforced and its `working_dir` test passes, and Direction (d) ratified and D5's binding check built. The change that records this ADR files each as an issue blocking beta entry.
- **Record metadata.** amends: ["ADR-027", "ADR-057", "ADR-062", "ADR-066", "ADR-072", "ADR-074", "ADR-078", "ADR-084"]; supersedes: []; applies_to: []

### Validation

- **D1:** under Direction (c)'s signal, a test configures each in-process agent path (`FISHHAWKD_ANTHROPIC_API_KEY`, `FISHHAWKD_MODEL_API_KEY`, `FISHHAWKD_ENABLE_LOCAL_CLAUDE_REVIEWER`, `FISHHAWKD_ENABLE_CODEX_REVIEWER`) and the refinement drafter in turn on a deployment that is not the local exception and asserts a refusal naming it, and a second test asserts that the local exception still runs a review. The model oracle reads the same keys, so a refusal of `FISHHAWKD_ANTHROPIC_API_KEY` or `FISHHAWKD_MODEL_API_KEY` also ends their use by the oracle, whose provider check then fails open; replacing it is Direction (e).
- **D1:** the E63 change, with the drafter's removal if that is a separate change merged no later, removes every in-process agent path, including on the local stack; its tests show no configuration preserves one, and its plan cites this record.
- **D2:** after E63, a test over the hosted configuration asserts that fishhawkd reads no model-key variable, constructs no model client, and that the model oracle holds no key; the replacement or removal of authenticated model-list validation that Direction (e) decides is exercised.
- **D2:** before beta entry, hosted conformance tests exercise bundle upload, retrieval and review recovery through their production authorization paths, proving cross-account denial, ADR-057 A2's repository visibility, and defined handling of runs with no account; a store-level test reads a bundle with another account's identity through each internal reader (review recovery, upkeep evidence, and the prompt-assembly readers for the fix-up prior diff and stage gate evidence, `backend/internal/server/prompt.go:4893`, `:5041`) and is refused; a store-level write under another account's identity is refused without creating or altering an object; removing the store's write check fails this test even while handler authorization stays on; a request for the raw variant is refused on every read path; removing the store's read check, the store's write check or the raw-variant refusal fails the test; and a deployment with no declared retention period reports it.
- **D2:** under Direction (c)'s signal, on a deployment that is not single-tenant and local, a run created with `working_dir` set causes fishhawkd to run no git command against it and to materialise no tree, on the grounding export and on merge-candidate transfer; this test does not lapse when E63 merges.
- **D3:** a runner-hosted implement review test asserts that the exported and reviewed commit equals the candidate commit named in the server's dispatch, that the signed verdict binds that candidate and the dispatched trusted base, that the invocation's working directory is a fresh export removed when the review ends, including on timeout and kill (ADR-078 rule 2), and that, with a convention file changed by the candidate, the injected text equals the dispatched base blob and the prompt carries the notice.
- **D3:** with an agent-instruction file changed by the candidate, the export does not contain it and the review still receives the diff.
- **D3:** a verdict returned for a different or missing commit is refused, including on review recovery.
- **D4:** Before E63, on D1's local exception with a subprocess reviewer, a run whose workflow declares `authority: gating` is refused at admission with the named remedy when grounding is on; the same run with `advisory` proceeds; with grounding off both proceed. The refusal is asserted on each entry point and on an implement-only gating workflow.
- **D4:** cases cover absent authority with agent and human counts, a declared `gating` with a human reviewer, and static and escalation personas inheriting the stage's authority.
- **D4:** a run admitted with grounding off, whose review is launched after grounding is turned on, launches no review, does not advance and records the refusal; after the E63 change merges, a gating agent reviewer is refused with the grounding flag off, and each dispatched review records whether it was given a tree.
- **D4:** a test resolves this repository's `routine_change` and asserts the refusal on a grounded configuration.
- **D5:** no runner-region test in alpha. Before beta entry, an end-to-end test shows that the binding checked at dispatch governs the runner and model endpoint actually used, and that a missing, invalid or mismatched binding prevents dispatch.
