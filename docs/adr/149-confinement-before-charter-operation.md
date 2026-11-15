---
id: ADR-149
title: "Confinement before charter operation: a charter operator cannot reach human credentials"
status: accepted
date: 2026-11-14
issue: https://github.com/kuhlman-labs/fishhawk/issues/5275
supersedes: []
superseded_by: []
amends: ["ADR-028", "ADR-029", "ADR-063", "ADR-101", "ADR-115", "ADR-119", "ADR-122"]
applies_to: []
---

# ADR-149: Confinement before charter operation: a charter operator cannot reach human credentials

**Status: accepted (ratified 2026-11-14).**

## Context

Prepared at the Captain's request from the 2026-11-14 ratified-ADR coherence review; citations are to `main` at `4e40a1e96`.

Two accepted records authorise charter operation on a host where a third decided that agents are not confined.

**Local agents are not confined.**
- ADR-028 decides for the local runner: "option 3 — detection-only ... do NOT add a local OS sandbox", because this is "the operator's own agent on the operator's own machine" (`docs/adr/028-agent-filesystem-confinement-os-level-sandbox.md:42`). It rejected `sandbox-exec` and bubblewrap as "platform-fragile" (`:46`) and made container confinement mandatory only for a hosted runner (`:44`).
- ADR-066 ships `permissions:` "declared and audited, NOT enforced until E51 #2133 (ADR-063 container isolation) lands" (`docs/adr/066-workflow-declared-autonomy-tier.md:101`).
- ADR-063's container is for gate commands: "The shared gate-exec helper ... runs the untrusted command in a container" (`docs/adr/063-isolate-untrusted-gate-command-execution.md:54`).
- ADR-119: the transcript store "under the operator's `$HOME/.claude/projects/…`, since `HOME` is allow-listed" is writable "by the unsandboxed agent and by other agents on the host" (`docs/adr/119-fixup-session-resume-opt-in.md:56`).

**Human credentials are on that host.**
- ADR-143: "A terminal-bound confirmation and an ordinary forge-verified signature are not proofs an agent's shell cannot produce." (`docs/adr/143-human-only-actions-credential-kind.md:63`).
- ADR-145: "A process with a shell on the deployment host, as on the default single-host local stack, holds the deployment" (`docs/adr/145-proof-of-presence-recovery.md:55`); R4 runs "on the server host with the deployment's own host privilege" (`:39`).

**What charter operation assumes.**
- ADR-101 D5: "The charter operator is product-spawned, holds ONLY the lease credential, and has no access to human credentials or to unrestricted forge or host mutation." (`docs/adr/101-charter-authorization-mode.md:110`).
- ADR-101's pilot is "One repository, one host" (`:154`), and its kill switch is "a host-local switch" (`:96`).
- ADR-101 D10 lists eight prerequisites that "block any lease, in shadow or pilot" (`:165-173`). None is agent confinement. Item 4 is "#4363, runner-attested evidence" (`:169`).
- ADR-122 enables self-origination "only through a `self_improvement` section of its ADR-101 charter, under a live lease" (`docs/adr/122-self-improvement-charter.md:49`) and inherits "ADR-101 D10's prerequisites" (`:98`).

No decided mechanism makes ADR-101 D5 true on the host ADR-101 names. A search of `runner/` found an environment allow-list (`runner/internal/agentenv/agentenv.go`), a macOS-only Seatbelt wrapper that denies network and host credential paths to the acceptance agent and signal delivery to implement agents (`runner/internal/netsandbox/`), and no path that establishes D1 for any agent.

**Phase.** ADR-080 made the local machine the alpha product (`docs/adr/080-solo-captain-alpha.md:60`).

## Options

- **A. Leave ADR-101 D5 as a statement.** The property stays asserted: a distinct lease credential does not prevent the operator's process from reading other host credentials.
- **B. Confine every local agent, attended runs included.** This reverses ADR-028 for the whole alpha product, putting a platform dependency ADR-028 judged not worth its cost in front of the attended loop.
- **C. Make confinement a prerequisite for charter operation only, state it as a property the launcher attests, and keep ADR-028 for every other local run as a stated residual (recommended).**

## Recommendation

**Option C.**

**D1. The property.** A charter-driven run is one that persists a lease id and generation (ADR-101 D1). From lease issue, in shadow or pilot, until the lease is void and its drain is complete, none of these processes can reach any item below: the charter operator and every process it spawns; and every agent, reviewer, gate command and job process executing work requested under the lease, whichever launcher starts it. After the lease is void and its drain is complete, a charter-driven run is not continued without confinement; a person who wants the work starts a new run. A launcher (the seat spawner, the ADR-104 host dispatcher, the runner) is outside the confinement and is not spawned by a confined process; only a launcher that starts a confined process owes an attestation. The lease-scoped first officer's own seat process is outside D1 until Direction (b). A confined process never starts a launcher or a runner.
- a forge token or forge session, whether a human's or the deployment's;
- the Fishhawk CLI credential store;
- a browser profile;
- an SSH key or signing key of a human;
- a shell on the deployment host outside the confinement;
- the deployment's own control surface: its database, its signing and attestation keys, any write to the kill-switch state, and any Fishhawk credential other than the one issued to that process, including over the host loopback.

"Cannot reach" covers reading, using through a helper or socket, and causing a host process to use on its behalf. It does not cover a request to the product's own authenticated interface under the credential issued to that process, or the runner's push of a stage's commit, the one launcher act this record names, provided the launcher runs it so that nothing the confined process wrote (hooks, repository configuration, filter drivers, worktree redirection) executes outside the confinement; any other launcher act needs an amendment. Content a confined process writes through the product's interfaces (issue text, comments, review text) that a process outside the confinement later reads is not a reach under D1; it is the co-resident case in Residuals and ADR-101 D6's quarantine.

D1's items are mandatory exclusions, not the whole of ADR-101 D5's prohibition. A human-bound credential of any kind stays out of reach wherever it is stored or however it is delivered.

**D2. A prerequisite, not a setting.** D1 is added to ADR-101 D10 as a ninth prerequisite that blocks any lease, in shadow or pilot. It is met only by D3 attestations produced at lease issue on the charter's registered host, one from each launcher that will start a confined process under the lease, each bound to the lease id and generation being issued and each showing a probe under the confinement that reached none of D1's items. A declaration, a build flag or an earlier lease's attestation never meets it, and no charter, workflow or deployment declaration waives it. Through ADR-122 D4 item 7 it also blocks every self-improvement class.

**D3. Attested, and failing closed.**
- Each process tree D1 covers is started by a launcher outside the confinement, which attests D1 for its root and every descendant: ADR-101 D4's seat spawner (#4148) for the charter operator; the runner for each separately launched stage agent, gate command and job; and whichever process launches a reviewer. Descendants may start inside that tree only while inheriting its confinement; separately launched work needs its own attestation. Where fishhawkd launches reviewers in process, as it does on a local deployment today (ADR-084 D6 and rule 6), a charter-driven review is refused and the round stays incomplete. The read-confinement in `backend/internal/reviewsandbox/` does not qualify: for claude it is a tool-layer blocklist, and for codex an unattested read allow-list that does not cover D1's other items.
- The attestation is a new evidence type, not ADR-101 D10 item 4's result attestation (#4363). It is signed by the launcher under a key the confined process cannot reach; it is bound to the lease, the launch and any credential delivered into the process; it lists every host path and socket exposed, judged against a product-owned allow-list; and it carries a probe the launcher ran from inside the confinement against each D1 item. It attests the launch and a probe at that moment, not what the process can reach afterwards. Its fields, staleness bound and request path are the implementing change's.
- For a request made by a confined process, admission authenticates that it comes from that process's attested tree; holding the credential is not enough. A trusted component outside the confinement acts under its own authenticated identity and rules, with server-derived lease provenance and ADR-101's action-admission checks; its request cannot stand in for the attestation of the confined process whose work it dispatches or reports. How is the implementing change's.
- A charter action that dispatches, mutates or spends, sent by a confined process, is refused when that process's attestation is missing, unreadable, stale, replayed or fails these checks; an unknown confinement is a refusal, never a warning. A result from any stage, gate command, job or review carrying lease provenance may be recorded, but does not advance charter work or satisfy a charter prerequisite unless that launch's attestation passes, whether or not the work belongs to a run. A refused charter action, or a result whose launch attestation fails, suspends a live lease and pages the captain; resuming the lease needs a fresh D2 attestation. The in-process review refusal, the gate-path refusal, a launcher that does not start a process and work the backend does not release hold the run and do not suspend the lease.
- The refusal never applies to revocation, suspension, the kill switch, cancellation of work that carries the lease's provenance, or the result recording ADR-101 D4 permits during drain.
- D1 also covers code a charter-driven process supplied or modified that a trusted component executes, including gate, build and preview commands, and every specialist job requested under the lease, with or without a run. A path that cannot establish D1, such as a gate with no container, ADR-063's fallback, or a gate image pull that can use a registry credential held on the host, is refused for charter work.
- The backend serves lease provenance with every stage, fix-up, gate, review and job dispatch, as a registered cross-module wire field, and does not release charter work to a launcher that has not declared it can attest. The field is present on every dispatch with an explicit true or false, so a run that is not charter-driven never depends on the default. A launcher treats an absent value as charter-driven.
- A launcher that cannot establish the confinement does not start the process.

**D4. Charter runs do not resume sessions.** A fix-up in a charter-driven run never resumes a prior agent session, whatever `FISHHAWK_FIXUP_RESUME` is set to. The transcript ADR-119 resumes is writable by every unconfined agent on the host (ADR-119 Context (c)), and no person is present at each step of a charter run, so a confined process never imports it, wherever it is stored. The runner takes "charter-driven" from D3's lease provenance field (ADR-101 D1), not from its environment. The fallback is recorded with its own registered reason, as ADR-119's others are.

**D5. Every other local run keeps ADR-028.** A local run that is not charter-driven keeps ADR-028's detection-only posture. That includes a run a person drives in a session and a run dispatched without one: a campaign under auto-drive, a scheduled run, a stage the ADR-104 host dispatcher starts. Stated residual: in those runs an agent can reach the person's credentials and a host shell, whether or not the person is present, so every control that rests on a credential's subject is agent-drivable there. ADR-143, ADR-144 and ADR-145 already say so for their own actions.

**D6. Mechanism is the implementing change's.** This record names no container runtime, sandbox or operating system. A mechanism qualifies on a host when it establishes D1 there and its launcher can attest it under D3. A host whose platform has no qualifying mechanism cannot be a charter's registered host.

**Directions (not decided here; each needs its own ADR or amendment).**
- **(a) The model credential.** Which model-provider credential a confined agent uses, and how that meets ADR-101 D7's controlled invocation.
- **(b) The lease-scoped first officer.** Whether its seat process is confined under D1. ADR-148 (#5274) places the seat's attach; this direction is only its confinement.
- **(c) Delegated and attended runs.** Whether D1 later applies to D5's unconfined runs, delegated runs no person attends first, and whether ADR-066's `permissions:` is enforced by the same mechanism.
- **(d) Network reach.** Egress limits beyond the host loopback for a confined charter process, and their relation to ADR-029 and ADR-050.
- **(e) The host itself.** What protects the launcher, the attestation key, the lease credential, the kill switch and ADR-145 R4's host command from an escaped process and from D5's unconfined agents.
- **(f) The dispatch path.** How charter work reaches a launcher outside the confinement, including the ADR-024 spawn verbs and ADR-104 Q3's dispatcher eligibility; until it is decided a charter-driven stage that no attesting launcher admits stays parked.

## Decision

**Accepted 2026-11-14** by the Captain (1 of 1; manual ratification path, the ADR-113 D2 verb is not yet built; approver, erratum and hashes in the ratification comment). **Option C**, i.e. D1 to D6 as stated in the Recommendation, decided now: before charter mode or self-improvement is enabled, the charter operator and every process started for charter work cannot reach human credentials, the deployment's control surface or a host shell, attested by the launcher and failing closed. It amends ADR-028, ADR-029, ADR-063, ADR-101, ADR-115, ADR-119 and ADR-122 only as "What this amends" states. The Directions are not decided. This record builds nothing by itself and enables nothing.

## Consequences

- **Scope.** This record builds and enables nothing. It adds one prerequisite, the refusals D1, D3, D4 and D6 state, and D3's suspension trigger, resumption condition and provenance field.
- **What this amends.**
  - ADR-101: D10 gains the confinement prerequisite (D2); D5's "no access to human credentials" gains D1 as a minimum that D3 evidences; D4's lease lifecycle gains a suspension trigger and a resumption condition (D3), and its human disposition of a held charter-driven run does not include continuing it (D1). D5's "unrestricted forge or host mutation" clause is not defined here.
  - ADR-122: D4 item 7 is read as including the confinement prerequisite, and no class definition, registration or widening under ADR-122 D3 may waive it.
  - ADR-028: its local detection-only posture is limited to runs that are not charter-driven (D5). Its hosted decision is unchanged. Its rejection of option 1 is lifted for the D1 mechanism only.
  - ADR-029: decision 3's local detection-only network posture is limited to runs that are not charter-driven, as far as D1 requires (the host loopback, helper and agent sockets). Egress beyond the loopback is Direction (d).
  - ADR-063: its local fallback, and the host fallback for an unhonoured `gate_container` declaration (`docs/spec/workflow-v2.md:210`), are refused for charter work (D3). Gate selection for any other run is unchanged.
  - ADR-115: its degraded clone mode, and a mid-stage fallback to it, are refused for charter work (D3); a charter-driven stage that would fall back stays held.
  - ADR-119: D2 gains a further condition; a charter-driven run never resumes (D4).
- **Costs.**
  - Charter mode and self-improvement wait on a confinement build that does not exist, on Direction (a) and on Direction (f): a confined agent cannot use a person's model-provider login or keychain, charter-driven agents lose any forge access that rests on host credentials, and until (f) is decided a charter-driven stage that no attesting launcher admits stays parked.
  - Charter-driven fix-ups pay for a fresh session.
  - Where reviewers run in process, a charter-driven run gets no agent review, so ADR-101's delegated plan approve on a clean dual review cannot be met there until reviewers have an attesting launcher.
- **Residuals.**
  - A compromised launcher or host defeats the attestation; Direction (e). The attestation store and signing key are in D1's list, so a confined process reaching either is a D1 failure, not a residual.
  - A human credential held somewhere D1's list does not name is still forbidden by ADR-101 D5, but the attestation and its probe will not detect it until the list is amended.
  - Confinement grants no credential or authority: the charter operator holds its lease credential, other confined processes only what their role contracts permit, and ADR-101's authorization and spend limits still apply.
  - While a lease is live, an unconfined agent in a D5 run shares the host and OS user with the launcher. It can reach what D1 keeps from the charter operator, including the launcher's signing key, and can tamper with the launcher, so until Direction (e) the attestation gives no assurance against this co-resident process. That agent, and the unconfined first officer, also read content a confined process wrote through the product's interfaces; D1 does not bound what they then do with the credentials they can reach.
  - The lease-scoped first officer runs unattended for the life of the lease and is not confined; D1 excludes it until Direction (b).
- **Record metadata.** amends: ["ADR-028", "ADR-029", "ADR-063", "ADR-101", "ADR-115", "ADR-119", "ADR-122"]; supersedes: []; applies_to: []

### Validation

- **D1:** on each host platform a charter may register, a confined process cannot read a forge token file, CLI credential store, browser profile or SSH key, reach a credential helper or agent socket, or start a process outside the confinement. The test covers a credential held in the platform keychain or secret service, a grandchild started with an empty environment, the container runtime's socket, and every host service listening on loopback other than the product's authenticated interface. It asserts that interface is reachable under the credential issued to the process and refused under any other Fishhawk credential, and that the runner's push of a stage's commit still succeeds with nothing the confined process wrote executing during it.
- **D2:** issuing a lease, in shadow and in pilot, and a `self_improvement` class are refused with a named reason while the prerequisite is unmet.
- **D1, D2:** a spawn verb called by a confined operator starts no runner. Lease issue is refused on a declaration, a build flag or an earlier lease's attestation, and succeeds on valid attestations from every launcher.
- **D1, D6:** a confined process cannot read its launcher's environment or signal or trace it; lease issue on a host whose platform has no qualifying mechanism is refused with the D2 reason.
- **D3:** a charter action with a missing, failed, probe-less, replayed or misbound attestation, an exposure outside the allow-list, or an unknown confinement is refused as an audit row, and a live lease is suspended with a page. A confined process cannot create or alter an attestation the engine accepts, and the lease credential presented from outside the attested tree is refused.
- **D3:** with a valid operator attestation, stage, gate, job and review results whose launch attestations are missing, failed, stale or bound to another launch are recorded but do not advance work or satisfy a prerequisite, and the lease is suspended with a page; valid ones permit progression; resumption is refused until fresh D2 attestations pass.
- **D3:** a runner that does not declare attestation is given no charter-driven stage; a dispatch with the provenance field missing is launched confined or not at all; a dispatch of a run that is not charter-driven carries the field as false and is launched unconfined with no attestation.
- **D3:** where reviewers run in process, a charter-driven review is refused, the round stays incomplete, nothing is approved on it and the lease stays live. An agent-authored gate command, through the real runner gate path, reaches none of D1's items or is refused before it starts; a gate with no container, a fallback to the clone path and an image pull that can use a registry credential held on the host each hold the run and leave the lease live. A run-less specialist job under the lease is confined and attested through its real request, dispatch and gate paths. A launcher unable to establish the confinement starts no process and reports why. After the lease is void and drained, continuing a charter-driven run is refused; after the drain, a held charter-driven run is not continued unconfined; a person's new run is not charter-driven.
- **D3:** with the attestation store unreadable, revoking and suspending the lease, the kill switch and cancellation succeed, and the lease is suspended with a page.
- **D4:** with resume switched on at the host, a charter-driven fix-up runs fresh and records a refusal, whatever an environment value inside the confinement says, `FISHHAWK_FIXUP_RESUME` included, and when its dispatch lacks the provenance field; a fix-up of a run that is not charter-driven, on the same host, still resumes.
- **D5:** a local run that is not charter-driven, attended or under campaign auto-drive, needs no attestation, and `out_of_tree_write` surfacing is unchanged.
