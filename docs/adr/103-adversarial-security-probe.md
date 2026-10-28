---
id: ADR-103
title: "Adversarial security probe: a scheduled acceptance-stage variant drives a disposable target under ADR-050 containment, emits a runner-replayed security_probe artifact, and files findings through a captain gate on a confidential channel (amends ADR-050)"
status: accepted
date: 2026-10-21
issue: https://github.com/kuhlman-labs/fishhawk/issues/4518
supersedes: []
superseded_by: []
amends: ["ADR-050"]
amended_by: ["ADR-126"]
applies_to: []
---

# ADR-103: Adversarial security probe: a scheduled acceptance-stage variant drives a disposable target under ADR-050 containment, emits a runner-replayed security_probe artifact, and files findings through a captain gate on a confidential channel (amends ADR-050)

## Context

Fishhawk's security controls are review-time and diff-scoped. The security persona (ADR-084 #3752, E80.5 #3762, remit `docs/review/security-remit.md`) reads a plan or a diff. The diff-secrets and permission-drift checks (E80.3 #3760, E80.4 #3761) read a diff. The advisory watch (E80.2 #3750) reads the dependency graph. Nothing exercises a running instance of the code with an adversary's intent.

On 2026-10-16 five authorization defects in fishhawkd were found by plan-stage code sweeps, not by plan review, implement review, or tests:

- #4476 — an unconfined, caller-supplied spawn input reaches an exec path over the HTTP MCP transport (class: untrusted input shapes a process spawn);
- #4481 and #4483 — write routes with no scope check (class: a write reachable by a read-only bearer);
- #4503 — a token-mint route with no scope ceiling (class: self-escalation one hop from every scope check);
- #4506 — a minted token loses its tenant (class: cross-tenant escape through a derived credential).

Each is a reachable request/response fact about a live instance. Four are one-hop allow/deny matrix facts (route × token class → 2xx where policy says refuse) that the route scope sweep, E80.21 #4515 (the sibling of `route_account_sweep_test.go`, #4211), will pin as a Go test with a reviewed `scopeSweepOptOuts` allow-list. The fifth is payload-shaped: a matrix never finds it, a reader of the handler did, and so would an agent that tries inputs. Two of the five (#4503, #4506) were filed as public issues on a public repository before a fix merged; there is no disclosure rule today.

What already exists, verified in the tree at cf0438a6:

- **A contained agent that drives a running instance.** The acceptance stage (ADR-049, ADR-050 #1540) assembles the lethal trifecta on purpose and contains it: the runner-embedded default-deny egress proxy with DNS pinning and verb-level deny (`runner/internal/egressproxy`), the single customer-controlled allow-list slot `egress.target_hosts` (`docs/spec/workflow-v2.md` § `egress.target_hosts`), the macOS Seatbelt net sandbox over the whole process tree (`runner/internal/netsandbox`, #3393; Linux is env-only, reported loudly), the gate env default-deny allow-list (`runner/cmd/fishhawk-runner/gateenv.go`), no MCP or Fishhawk token (the verdict ships signature-authed), and advisory zero-write authority (the `docs/ARCHITECTURE.md` §6 Rule-of-Two "acceptance" row). `docs/METHODOLOGY.md` § "The acceptance agent": a fully prompt-injected acceptance agent can at worst emit a wrong verdict.
- **The general reading of `acceptance`.** ADR-067 (`docs/spec/workflow-v2.md` § "`acceptance` — verdict"): an acceptance stage emits a verdict with evidence against declared criteria; it is not bound to a code change. Non-diff workflows (`backlog_grooming`, upkeep scan E79.2 #3726, comms scan E81.5 #3775) already run a propose stage that emits a typed report instead of a plan (`grooming_report`, `upkeep_report`, `comms_report`), park at a captain gate, and file only approved items through `applyAndFileWorkItem` with `intakegroom` dedup.
- **A scheduler.** `schedule` + `trigger: [scheduled, on_demand]` (E79.1 #3725): fishhawkd mints exactly one run per due window.
- **Per-stage budget.** `budget.limit_usd` is enforced on spec major 2 (`budget.EvaluateStage`, E48.55 #2328).
- **Admission.** Stage concurrency groups (ADR-087) and resource-keyed admission (ADR-094).
- **An auth policy in prose.** `docs/review/security-conventions.md` § "Tokens, scopes and authorization". #4515 turns it into a reviewed, machine-checked allow-list.
- **Constraints on where an agent may run.** ADR-074: fishhawkd orchestrates and records; it never executes an agent. ADR-097 D2: specialist jobs are a closed kind enum of ADR-089 D3 verification jobs (scratch clone, no push, no state change, no tracker write, result-only); none may egress to a target. ADR-084 rule 1: a reviewer persona is a reviewer, not a stage, and runs in-process in fishhawkd today (D6a).

What does not exist:

- A disposable target. Local acceptance validates against ONE shared preview (`localhost:8090`, database `fishhawk_preview`, serialized by the `local-acceptance` concurrency group; `.fishhawk/workflows.yaml` acceptance stage). E83.34 #4019 proposes an ephemeral per-stage preview when the target is declared dynamic. A probe target must additionally hold no real credential and make no outbound call.
- A structured finding artifact for a live probe, a replay step that makes an agent-claimed finding runner-observed evidence, and a disclosure channel. The work-management layer has no `confidential` write capability (GitLab's confidential issues appear only as an EXCLUSION in the user-report scan, `backend/internal/workmgmt/gitlab/README.md`).

## Options

### Framing: a new ADR amending ADR-050, or an ADR-050 amendment?

- **A pure amendment to ADR-050.** ADR-050 decides an egress and credential posture for one agent. The probe reuses that posture verbatim, so an amendment that adds a second Rule-of-Two row is the minimal record. Rejected as the sole vehicle: the probe also needs decisions ADR-050 never contemplated (a disposable-target requirement, a new artifact with runner replay, a filing gate, a disclosure policy, a budget and timeout rule, an enabling eval). Folding those into an egress ADR makes the egress record unreadable for its own purpose, and the `amends` relation (E78.13 / #4471) encodes "refines this record's rules", not "adds an unrelated capability".
- **A new ADR with `amends: ["ADR-050"]` (recommended).** The new record owns the capability. The amendment is narrow and explicit: ADR-050 D4's Rule-of-Two table gains an `adversarial probe` row, and ADR-050 D1's allow-list is tightened for this variant (below). Nothing in ADR-050 is superseded; the acceptance row is unchanged. ADR-049, ADR-067, ADR-084 and ADR-097 are related, not amended: no stage type, reviewer persona or specialist kind is added.

### Shape

- **A. Do not build it in-product; repository CI only.** Keep #4515 (route × token matrix as a Go test), E80.2 (govulncheck, OSV, pnpm audit), CodeQL via E80.1 #3749, and an operator-run DAST tool on demand. Pros: zero new attack surface; no disclosure machinery; the matrix classes (4 of the 5 misses) are covered once #4515 lands. Cons: nothing exploratory, so the payload-shaped class (#4476) stays a reader's find; the opt-out list is asserted, never challenged; nothing exists for a second repository, so the security officer (E80) never becomes a product capability; the result lives in CI logs, not on the audit chain.
- **B. A specialist job kind `security_probe` (ADR-097 D2 enum addition).** Pros: the first officer could trigger it; result-typed, stateless, audited. Cons: ADR-097 D2 jobs are ADR-089 D3 verification jobs by definition: scratch clone, no egress to a target, `local-verify` admission group. Giving a job a network target and a provisioned instance makes it a stage in all but name while escaping the stage machinery (budget, `egress`, the acceptance identity gate, triage). Rejected.
- **C. Extend the security reviewer persona (ADR-084) with a live-probe tool.** Pros: one security remit, one eval gate. Cons: ADR-084 rule 1 (a persona is a reviewer, not a stage); personas run in-process in fishhawkd (D6a) and ADR-074 forbids fishhawkd executing an agent at all, let alone one with network reach; reviews are diff-scoped and a probe is instance-scoped; a reviewer's write authority is a concern row, a probe's output must be replayed before it is believed. Rejected.
- **D. An acceptance-stage variant in a scheduled non-diff `security_sweep` workflow (recommended).** A stage `type: acceptance` that `produces: [{artifact: security_probe, schema: security_probe_v1}]` instead of `acceptance`, runner-hosted, under ADR-050 containment tightened for an adversarial agent, against a provably disposable target, with a deterministic pre-pass, runner replay of every claimed finding, a captain gate, and confidential filing. Pros: every containment control is already built and audited; the general reading of `acceptance` already covers it; the non-diff propose → gate → file seam already exists; it runs anywhere the runner runs. Cons: a new artifact schema and ingest handler; the target-provisioning work (#4019 plus a probe mode); the disclosure capability; an eval before enabling; a weekly cost.
- **E. Deterministic DAST tooling only (nuclei/ZAP-style templates or the #4515 matrix replayed over HTTP), no agent.** Folded into D as its mandatory pre-pass rather than a separate option: it is the cheap half, it owes the agent its results so the agent does not re-find them, and an external scanner's output is consumed where present (E80's out-of-scope rule: the officer consumes SAST/DAST products, it does not replace them).

## Recommendation

**Option D**, as a new ADR amending ADR-050. Decision points:

**D1 Shape: an acceptance-stage variant, not a new kind.**
- A workflow `security_sweep` (non-diff; `applies_to.trigger: [scheduled, on_demand]`; `schedule` declared) with one agent stage `type: acceptance` producing `security_probe` with `schema: security_probe_v1`. The binding mirrors `upkeep_report` on `plan`: valid on `acceptance` only; a stage may not produce both `acceptance` and `security_probe`; the schema is required alongside it. Backend and CLI validators enforce it; mirrored per the schema checklist.
- The stage's criteria are not plan criteria. Its declared policy is a document pair: the repository's security review convention (`docs/review/security-conventions.md` here) and the #4515 opt-out table, both resolved at the target's SHA and rendered as DATA with their digests recorded on the artifact.
- The agent gets a clean read-only checkout at the target SHA (ADR-074 D1 shape), the OpenAPI document and the policy pair. It does not get the preview's database, the host's `.env`, or any Fishhawk token.
- It is not a crew role and not a reviewer persona. Name: `security_probe` (the artifact and the stage variant). Role naming per ADR-099 is unaffected.

**D2 Deterministic first, inside the same stage.**
- Before the agent spawns, the runner executes a zero-agent pre-pass: the route × token-class matrix (every route in the OpenAPI document, and every MCP tool when `/mcp` is enabled on the target, against anonymous, read-only bearer, foreign run-bound, first-officer seat, cookie session and other-tenant identities), and any configured external scanner. Each 2xx where the policy says refuse becomes a `security_probe` row with `provenance: server_check`, replayed and attested by the runner with no model call.
- The pre-pass result is rendered to the agent as data so the agent does not re-report it. The agent's remit is the space a matrix cannot cover: input-shaped defects and multi-step consequences, judged against the same policy pair.

**D3 Containment: ADR-050's posture, tightened (the amendment).**
- Reuse unchanged: the runner-embedded default-deny egress proxy with DNS pinning and verb-level deny; the gate env default-deny allow-list; the macOS Seatbelt net sandbox over the whole process tree (Linux env-only, reported loudly); no MCP or Fishhawk token (the artifact ships signature-authed, the ADR-050 acceptance pattern); advisory zero-write authority.
- Tighten for an adversarial agent: the single customer-controlled `egress.target_hosts` slot must resolve to the disposable target only (D4). The model API endpoint stays on the allow-list, as ADR-050 D1 requires. This is the honestly-stated residual (Consequences). ADR-050 D1 also admits the Fishhawk backend for the signature-authed trace ship. Whether the probe variant can drop it, with the RUNNER rather than the agent process shipping the artifact, is an open question for ratification.
- Per-stage `limit_usd` (`budget.EvaluateStage`) and a hard `max_runtime`, which the runner enforces by terminating the process tree, are REQUIRED on a `security_probe` stage; the validator refuses the stage without both (see D7).
- Record a new Rule-of-Two row `adversarial probe` in `docs/ARCHITECTURE.md` §6 (this is the amendment to ADR-050 D4): three legs assembled, each bounded exactly as acceptance, with the extra constraint that the credentials leg holds ONLY target credentials minted inside the disposable target (D4) and never leaves the runner.

**D4 A provably disposable target.**
- The target is provisioned per sweep: a fresh database, a fresh instance, never the shared acceptance preview (`localhost:8090` / `fishhawk_preview`) and never production. It builds on E83.34 #4019's dynamic per-stage preview; the probe mode adds that the instance holds no real forge token, no real model key reachable by the probed surface, and no outbound network from the target itself.
- Test credentials (the token classes the matrix and the agent exercise) are minted INSIDE the target and are never shipped to fishhawkd. The sweep tears the target down afterward.
- The stage refuses to spawn if the target cannot be shown disposable: the identity gate (the ADR-050 pre-spawn `/healthz` SHA probe) is extended with a disposability assertion, fail-closed, naming the reason.

**D5 Findings are a structured, schema-validated artifact, replayed before trusted.**
- `security_probe_v1` (new canonical schema under `docs/spec/`, mirrored per the checklist). Each finding row: `class`, `provenance` (`server_check` | `agent`), the request (method, route, token class — never secret bytes), the observed response status and a redacted excerpt, the expected disposition from the policy pair, and the `document_ref` + digest of the policy sentence it rests on.
- The agent's output is treated as injected, exactly as the acceptance verdict is. An `agent`-provenance finding is a CLAIM until the runner REPLAYS its request against the (still-live) disposable target and observes the same result; a finding whose replay does not reproduce is recorded `unconfirmed` and never filed. This is the ADR-089 D4 rule (a finding blocks only on product-produced evidence) applied to a probe: the runner's replay is the machine evidence.
- The artifact is runner-observed and recorded on the audit chain with the requesting actor, like every other stage output.

**D6 Filing only through a captain gate, on a confidential channel.**
- The sweep is a non-diff propose → gate → file workflow. It files NOTHING automatically. The report parks at a captain gate (the `grooming_report` / `upkeep_report` seam, `applyAndFileWorkItem` with `intakegroom` dedup against open issues and advisories).
- Disclosure is the two-part rule. The PUBLIC issue carries the class and the fix contract only. The reproduction and any exploit chain go into a DRAFT GitHub security advisory (GitLab: a confidential issue; Jira: a security level) until the fix merges, then a sanitized public issue is filed.
- This is a new `confidential` capability on the work-management provider. The apply layer REFUSES to file a HIGH security finding publicly on a provider without that capability — fail-closed, named, never a silent public filing. Documented in `docs/board-capability-matrix.md` alongside the existing board-capability degradations.

**D7 Budget and timeout.**
- The stage carries an enforced per-sweep `limit_usd` (spec major 2, `budget.EvaluateStage`) and a hard `max_runtime`. A sweep that exhausts either records a named degradation and files whatever confirmed findings it has; it never blocks and never fails the run.
- Admission: the sweep's target-provisioning competes for a host the same way acceptance does. A `security-sweep` concurrency group (ADR-087) and resource-keyed admission (ADR-094) serialize it against the shared host; it never takes an implement slot.

**D8 Enabled only after an eval gate.**
- Before the probe is enabled on this repository, its agent prompt passes an eval arm the way the security persona did (E80.17 #4314, E80.19 #4402): a corpus of seeded live-instance defects (one per class, including the payload-shaped class #4476 motivates) on which the probe must produce a confirmed finding, plus clean targets on which it must stay quiet, measured for catch rate and false-alarm rate. A prompt change re-runs the arm (the E55.4 #2245 discipline).

## Decision

**Accepted 2026-10-21** by the Captain (repository maintainer). Adopt **Option D**, as a new ADR that amends ADR-050, with decision points D1–D8 as stated in the Recommendation:
- an acceptance-stage variant in a scheduled non-diff `security_sweep` workflow producing `security_probe_v1`;
- a deterministic route × token-class pre-pass inside the same stage;
- ADR-050 containment, tightened to a single disposable-target slot;
- a provably disposable per-sweep target;
- runner replay of every agent-provenance finding before it is trusted;
- filing only through a captain gate on a `confidential` channel, refusing a public HIGH filing fail-closed;
- enforced `limit_usd` and `max_runtime`;
- enablement only after an eval gate.

The Preconditions and sequencing above are binding. Delivery starts with step 4(a), the deterministic auth-matrix pre-pass plus the govulncheck fold-in. The agent persona (4e) is not enabled before preconditions 1–3 and its eval gate are met. Autonomy: `autonomy:low`, with the repository maintainer as the named approver.

The in-repo `docs/adr/103-*.md` record (with `amends: ["ADR-050"]` and ADR-050's `amended_by`) lands with the held ADR-records batch at the 2026-11-01 catch-rate re-pin.

## Consequences

**Threat model.** The probe agent is treated as fully prompt-injected, because the instance it drives renders attacker-controlled data (the acceptance threat model, ADR-050). It assembles all three lethal-trifecta legs: code execution (its own tool use), network (to the target and the model endpoint), and credentials (target-only test tokens minted inside the target). Each is bounded.

**Blast radius.** A fully-compromised probe agent can: (a) emit a wrong or fabricated finding — caught by the runner replay (D5), which refuses to file an unreproducible claim, and by the captain gate (D6); (b) attempt to reach a non-allow-listed host — refused in the kernel by the Seatbelt sandbox and at the proxy (D3); (c) attempt to write — it holds no MCP, Fishhawk or forge token, and the target is disposable and credential-free (D4). It cannot reach production, the shared preview, or the real tracker, and it files nothing itself.

**Cost.** A scheduled sweep spends tokens on every window, including quiet ones, bounded by `limit_usd` and `max_runtime` (D7). The deterministic pre-pass (D2) carries the matrix classes at near-zero model cost; the agent cost buys only the exploratory remainder. Target provisioning is host time, serialized (D7).

**Residual risks, stated honestly.**
- The model API endpoint stays allow-listed (ADR-050's unavoidable residual). Exfiltration of target data through a crafted model prompt is bounded by the target holding no real secret and by zero write authority — bounded, not prevented.
- The matrix (D2) is only as good as the policy pair it reads; a wrong opt-out in the #4515 allow-list is a wrong oracle. The sweep makes the opt-out list adversarially challenged rather than merely asserted, which is a gain over Option A, but a mis-reviewed opt-out still hides a finding.
- Linux has no net sandbox (env-only, reported loudly, #3393). On Linux the proxy and the credential-free target are the containment; a nested `env -i` spawn is a known gap there.
- The disposability assertion (D4) is a check, not a proof of the absence of every credential; it asserts the known credential and network surfaces are absent and fails closed when it cannot.

**What it obsoletes, and does not.**
- It does NOT replace SAST/SCA/DAST products or CodeQL; it consumes their output where present (E80's standing out-of-scope rule).
- It does NOT replace #4515 (the Go route × token sweep) or E80.2 (the advisory watch); it reuses the first as its pre-pass oracle and runs beside the second.
- It does NOT add a stage type, a reviewer persona, or a specialist-job kind; it is an acceptance variant (D1).
- It supersedes nothing. It amends ADR-050 D4 (a new Rule-of-Two row) and tightens D1's allow-list for this variant; ADR-050's acceptance posture is unchanged.

## Preconditions and sequencing

1. **#4515 (E80.21) has run long enough that its reviewed opt-out list is stable.** That `scopeSweepOptOuts` table IS the auth policy the probe's deterministic pre-pass checks against. An unstable oracle makes the probe's matrix findings noise. This is a hard precondition on D2.
2. **A disposable per-sweep target exists.** E83.34 #4019 (ephemeral per-stage preview) is the base; the probe mode adds the credential-free, no-outbound, disposability-asserted target (D4). Until it exists the sweep cannot be enabled on any host.
3. **The `confidential` work-management capability (D6) is decided and built** before any HIGH finding is filed. The disclosure gap that filed #4503 and #4506 publicly is the motivation; shipping the probe without the confidential channel would industrialize that gap.
4. **Sequencing after ratification:** (a) the deterministic auth-matrix pre-pass + govulncheck fold-in (high value, low risk, reuses #4515 and E80.2); (b) the `security_probe_v1` artifact, ingest handler and captain-gate filing; (c) the `confidential` capability and the fail-closed public-filing refusal; (d) the disposable probe target (depends on #4019); (e) the agent persona and its eval gate (D8); (f) a capstone that runs a full sweep on this repository and one other and records the officer's done-means (E80.7 #3764 T9).
5. **Autonomy:** `autonomy:low` (human-led), as ADR-050 was, given the security criticality and the new disclosure surface. Named approver: the repository maintainer.

## Related

- Amends: ADR-050 (#1540).
- Related ADRs: ADR-049 (#1519, acceptance stage), ADR-067 (#2210, the general `acceptance` reading), ADR-074 (#2306, fishhawkd never executes an agent), ADR-084 (#3752, reviewer personas — explicitly distinct), ADR-089 (#4149, finding-blocks-on-evidence), ADR-097 (#4299, specialist jobs — explicitly distinct), ADR-087 (#3968, concurrency groups), ADR-094 (#4239, admission).
- Epics/issues: E80 #3701 (parent); E80.21 #4515 and #4211 (the matrix oracle); E80.2 #3750 (advisory watch); E80.5 #3762 and E80.17/#4314, E80.19/#4402 (security persona + eval pattern); E83.34 #4019 (disposable target); E79.1 #3725 (scheduler); E48.55 #2328 (`limit_usd`).
- The five 2026-10-16 misses as motivation (class only): #4476, #4481, #4483, #4503, #4506.

Parent epic: #3701
