---
id: ADR-098
title: "Amend ADR-096 D3: split the global surface into verdict-wide and doc-check inputs, add data-read edges, and enable verdict transfer only after a shadow measurement"
status: accepted
date: 2026-10-11
issue: https://github.com/kuhlman-labs/fishhawk/issues/4316
supersedes: ["ADR-096"]
superseded_by: []
applies_to: []
---

# ADR-098: Amend ADR-096 D3: split the global surface into verdict-wide and doc-check inputs, add data-read edges, and enable verdict transfer only after a shadow measurement

## Context

ADR-096 (#4290, accepted 2026-10-11) lets a passed merge-candidate verdict transfer across an independent base delta. Its D1 conditions and its D3 global-surface set decide what counts as "independent".

On 2026-10-11 the overseer cross-checked the slice-1 plan (#4292, run 2d177c50). The check found that ADR-096 as written will rarely transfer anything, for two reasons.

1. **D3 puts the doc-check inputs on the global surface:** `docs/adr/**`, `docs/ARCHITECTURE.md`, `site/**` and `docs/api/**`. Global-surface paths always force a pass, so a merged ADR record, a site page or an ARCHITECTURE pointer row never transfers. That is ADR-096's own motivating case: merging the ADR record #4244 cost #4235 a full pass.
2. **D1(c) intersects reverse-dependency closures.** `backend/cmd/fishhawkd` imports nearly every backend package, so the closures of any two backend changes meet there, and transfer effectively happens only across modules.

The check also showed that the doc tree is NOT inert to Go tests. Go tests read some docs as data:
- `backend/internal/decisionrecord/decisionrecord_test.go:57` parses the real `docs/adr/index.json`;
- `backend/internal/spec/hotfix_change_test.go:372-373` reads `docs/spec/workflow-v2.md` and `docs/METHODOLOGY.md`;
- `backend/internal/server/comms_scan_handler_test.go` and `backend/internal/artifact/kind_openapi_test.go` read `docs/api`.

A rule that makes doc paths inert would therefore transfer over a real interaction.

The size of the effect is unknown. No transfer decision has been computed on a real base advance yet.

## Options

- **A. Keep ADR-096 as is.** Safe, but most of the expected throughput gain never materialises.
- **B. Split D3 and add data-read edges now, and enable transfer immediately.** This relaxes a safety rule before any measurement exists.
- **C. Split D3 into verdict-wide inputs and doc-check inputs, add data-read edges, and gate enabling transfer on a shadow measurement (RECOMMENDED).** Decide the D1(c) question with data later.

## Recommendation

Option C.

**D1 Split the global surface.** ADR-096 D3 becomes two classes. The versioned set file and its harness pin carry both.

- **(G) Verdict-wide inputs.** Any delta path in G forces a full pass, exactly as ADR-096 D3 does today. G contains:
  - `go.work`, every `go.mod`/`go.sum`;
  - `scripts/**`, `.golangci.yml`, `deploy/gate-image/**`, `.fishhawk/**`;
  - `backend/internal/postgres/migrations/**`, `backend/internal/audit/categories.go`;
  - `docs/spec/**` (schemas, mirrors, examples);
  - root `testdata/**`;
  - the slice-1 additions #4292 surveyed: `deploy/helm/**`, `backend/Dockerfile`, `Makefile`, `.github/workflows/**`, the version packages, `docgen`/`cmdinfo`.
- **(D) Doc-check inputs.** Everything else under `docs/**`, and all of `site/**`. A delta path in D forces a pass only when one of the following holds:
  - **(D-i)** the PR's own diff touches an input of the same doc leg. The leg-to-inputs table is pinned by the verify-legs harness. For example, check-adr reads `docs/adr/**`, the doc-line budget reads `docs/ARCHITECTURE.md`, and site voice/IA read `site/**`.
  - **(D-ii)** the path is a data input of a Go package (D2) whose reverse-dependency closure intersects the PR's.

**D2 Data-read edges.** A repo file is an input of package P when either of these holds:
- a Go file in P (tests included) names it, or a directory containing it, as a string literal that resolves relative to P's directory or the repo root;
- P embeds it with `go:embed`.

The classifier extracts these edges by a literal scan at classification time and treats them like the existing "non-Go file under a package directory" rule. This is conservative by construction: a literal naming a DIRECTORY prefix (for example `"docs/adr"`) makes every file under it an input.

Stated residual: a path assembled at run time from fragments that never appear as a literal is invisible. Post-merge CI on main stays the backstop. A red main traced to a transfer is a D2 coverage bug.

**D3 Shadow first. Transfer is enabled only on the captain's word.** The transfer slice (#4293) ships in **shadow mode** by default:
- At every base advance it computes the classification. It appends `merge_candidate_verdict_transfer_shadow` with the would-be decision and reason, registered in `categories.go`.
- It still runs the full pass. When the pass settles, the outcome is recorded against the shadow decision.

A shadow "transfer" whose real pass went RED is a **false transfer**. It blocks enablement until the set or the edge rule is fixed and the case is added as a test.

Enabling transfer is a captain act. It requires a shadow report over at least the next campaign's base advances, giving the transfer rate, every reason for refusal, and zero unexplained false transfers.

**D4 Revisit D1(c) on data, not now.** The shadow report breaks refusals down by reason (`global_surface:*`, `doc_leg:*`, `data_edge:*`, `reach_overlap:<pkg>`). If `reach_overlap:backend/cmd/fishhawkd`, or another wiring package, dominates, a follow-up ADR may consider treating a common dependent as overlap only when that dependent has tests linking both changed packages. This ADR does not decide that.

**D5 Sequencing.**
1. Slice 1 (#4292) ships the classifier with the ADR-096 D3 set unchanged, because a conservative superset is safe.
2. #4293 ships transfer in shadow mode.
3. A new implementation issue implements D1 and D2 (the G/D split and data-read edges) in the classifier.
4. The shadow measurement runs.
5. The captain enables transfer.

**Ratification questions:**
- **Q1:** Should D3 split into verdict-wide (G) and doc-check (D) inputs, with D forcing a pass only on a same-leg PR touch or a data-read edge?
- **Q2:** Should data-read edges come from a literal scan for paths and directory prefixes plus `go:embed`, with the run-time-assembled-path residual stated?
- **Q3:** Should transfer ship shadow-first and be enabled only by the captain on a shadow report with zero unexplained false transfers?
- **Q4:** Should the D1(c) common-dependent question be deferred to a data-driven follow-up?

## Decision

**Accepted (2026-10-11), ratified by the Captain as recommended.** Adopt Option C with D1–D5 as written.

- **Q1: yes.** D3 splits into verdict-wide (G) and doc-check (D) inputs. D forces a pass only on a same-leg PR touch or a data-read edge.
- **Q2: yes.** Data-read edges come from a literal path or directory-prefix scan plus `go:embed`, with the run-time-assembled-path residual stated.
- **Q3: yes.** Transfer ships shadow-first. It is enabled only by the captain, on a shadow report with zero unexplained false transfers.
- **Q4: yes.** The D1(c) common-dependent question is deferred to a data-driven follow-up.

## Consequences

- **Throughput.** Doc-only and ADR-record deltas transfer unless the PR touches the same doc leg or a reading package. This restores ADR-096's motivating case.
- **Safety.** Every relaxation is measured in shadow before it takes effect. False transfers are detected by construction, because the full pass still runs in shadow mode. Data-read edges close the "docs are inert" hole that a naive split would open.
- **Cost.** Shadow mode adds no verify savings until enablement, which delays ADR-096's benefit by one campaign.
- **Supersession.** This amends ADR-096: D3 is replaced by D1–D2 here, and D3 here is added. ADR-096 D1(a)–(d), D2, D4–D6 are unchanged.
- **Implementation:**
  - a #4293 approval condition (shadow mode, the shadow audit category, outcome pairing);
  - a new E87 issue for the G/D split plus data-read edges in `backend/internal/basedelta`;
  - the shadow report as part of the enablement act.

## Relations

Amends ADR-096 (#4290). Related: #4292, #4293, #4294, E87 #4291, ADR-090 (#4170), ADR-092/093.
