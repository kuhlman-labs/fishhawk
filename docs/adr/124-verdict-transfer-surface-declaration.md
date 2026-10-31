---
id: ADR-124
title: "Amend ADR-098: the verdict-transfer surface is a repository declaration; absent refuses"
status: accepted
date: 2026-10-27
issue: https://github.com/kuhlman-labs/fishhawk/issues/4754
supersedes: []
superseded_by: []
amends: ["ADR-098"]
applies_to: []
---

# ADR-124: Amend ADR-098: the verdict-transfer surface is a repository declaration; absent refuses

## Context

Amendment to ADR-098 (which amends ADR-096 D3), **v2.1**. Status: accepted (ratified by the Captain, 2026-10-27). v1 was reviewed by claude-fable-5-1 and gpt-6-astra, both of which said revise. v2 folds in both reviews; v2.1 folds the confirmation pass (gpt-6-astra: ratify_with_notes; claude-fable-5-1: one class-precedence blocker, now fixed).

ADR-098 D1 makes the merge-candidate verdict-transfer surface a product constant: "The versioned set file and its harness pin carry both" classes, verdict-wide (G) and doc-check inputs. In code that is `basedelta.GlobalSurface` and `SharedArtifactDirs`, which hard-code THIS repository's layout. The import-graph loader is Go-only and returns an empty graph without `go.mod`. A non-Go path maps to no seeds and is treated as inert.

Two consequences follow:
- A non-Go repository's PR delta plus base delta can classify `independent` and skip re-verify. In enforce mode that is fail-open (#4657).
- ADR-110 (ratified 2026-10-21) forbids this-repository values on product surfaces, so the surface has to be a repository declaration.

## Options

A. **Keep ADR-098's product constant, and add an `unsupported_language` refusal only.** This still carries this-repository values on a product surface (an ADR-110 violation), and other Go repositories inherit a wrong G set.

B. **A repository declaration, where an absent declaration means an EMPTY surface.** This is fail-open: with an empty G, the paths that should force a pass (go.work, the verify scripts) classify as ordinary paths, and two deltas can classify `independent`.

C. **A protected repository declaration, with every undecidable case refusing transfer (recommended).** The declaration is workflow-v2 `verdict_transfer`, read from the run's cached admission spec. Absent, empty or incomplete declarations refuse, and so do unsupported workspaces and unclassifiable paths. Completeness is checked where it can be, and enforce mode waits for the declaration's protection.

## Recommendation

C.

## Decision

**Accepted 2026-10-27:** ratified by the Captain (v2.1).

The decision:

1. **Declaration.**
   - **What it holds.** The verdict-transfer surface is a repository declaration under ADR-110: an additive, TOP-LEVEL workflow-v2 `verdict_transfer` block. There is one per repository, never one per workflow, so no workflow carries a narrower surface. It holds ADR-098 D1's verdict-wide G class, the shared-artifact dirs, the doc-check inputs, and the inert-path classes (D3).
   - **Where it is read from.** It is resolved from the run's cached admission spec, so a PR cannot change its own surface.
   - **What is recorded.** Every transfer and shadow row records the resolved declaration's content digest and its admission provenance, beside the classifier version. That replaces the product `SurfaceVersion` as the surface identity.
2. **Refusals.** Each of these classifies `undecidable:<reason>`, and the merge-candidate pass then runs wherever ADR-090 requires one (a workflow with no verify command stays exempt, as today):
   - an absent, unparseable or nil declaration (`surface_undeclared`; ADR-110 rule 4);
   - an empty or structurally incomplete declaration (`surface_incomplete`);
   - a malformed entry or pattern syntax error (`invalid_surface:<entry>`).

   A valid pattern that matches no current file is NOT an error. It is prospective coverage: it is diagnosed for operator review, but never counts as evidence of completeness.
3. **Language support and unclassifiable paths.**
   - **No adapter.** A WORKSPACE with no supported import-graph adapter (today: no `go.work` and no root `go.mod`) classifies `undecidable:unsupported_language`. A Go module's presence somewhere in the tree is not sufficient.
   - **Inside a supported workspace,** every changed path, on both the PR delta and the base delta, must be either:
     - covered by the adapter's graph;
     - in the declared G class;
     - in a declared inert-path class (for example documentation globs; ADR-098 D1/D2 doc-leg and data-read treatment applies);
     - or in a declared opaque subtree, which refuses.

     **Precedence.** Classes are evaluated in this order, and the first match decides:
     1. G;
     2. opaque;
     3. adapter graph coverage (including today's rule that a non-Go path under a package directory is covered by that package);
     4. inert.

     An inert declaration never removes a path from graph reach, data-read edges or the shared-artifact rule. It only decides that a path with NO other classification is not unclassified.

     A path that is none of these classifies `undecidable:unclassified_path`. Inertness is declared, never defaulted, so an unknown extension, or a change that is partly in another language, refuses rather than reaching `independent`.
4. **Completeness checking.** ADR-096 D3's completeness obligation survives the move.
   - **In this repository,** the verify-leg harness (`backend/internal/basedelta/verifylegs_test.go` and the ADR-096 floor test) reads the LIVE declaration through the production resolver, at the test's commit. It asserts that every verify-leg input is covered and that the declaration is a superset of the floor. The floor preserves ADR-098's amended G/D coverage; D inputs need not remain in G. It keeps its negative tests for removed coverage and unregistered legs, as an ADR-110 rule 5 conformance test. The hard-coded ADR-096 floor (`adr096D3Floor`) is this repository's rule-5 conformance tooling, not a product surface value under rule 4.
   - **In any other repository,** enforce mode requires a declared completeness check (a conformance test or harness, named in the declaration). Without one, that repository stays in shadow. A declaration's presence alone proves nothing, and ADR-098 D3's false-transfer rule remains the shadow-phase detector.
5. **Protection: the declaration is ADR-121 tier I.** It decides whether a judgement (the verify pass) runs. So the `verdict_transfer` block, its completeness tables and its floors are tier-I judge inputs, and any change, shrinking especially, requires Captain adoption and the protected merge verdict. A standing-order approval is insufficient, and an agent proposes a change only as a finding.
   - **Until ADR-121 is implemented,** the controls are the implement-stage `.fishhawk/**` forbidden path and the operator-authored rule. They are enough for shadow mode.
   - **Enforce mode** additionally requires ADR-121's bindings and merge condition to be operational for the declaration.
   - **A declaration change** forces verification regardless of its own declared patterns, because the workflow spec is self-covering.
6. **Example content.** ADR-098's versioned set is retained as this repository's documented EXAMPLE declaration. It is not a universal default. Operators write their own, and the product never applies one implicitly.
7. **Unchanged:**
   - ADR-098's G/D semantics, data edges and doc-leg obligations;
   - ADR-098 D3: shadow-first, enabled only on the Captain's word;
   - ADR-090 (a refusal means a pass), ADR-092 (a refused transfer takes a verify slot as today) and ADR-096 D1 (fail closed means verify).

## Consequences

- **This repository** declares its surface in `.fishhawk/workflows.yaml`, an operator-authored change. Until then its shadow rows record `undecidable:surface_undeclared` instead of real shadow data.
- **Enablement clock.** ADR-098 D3's enablement clock ("at least the next campaign's base advances") restarts when the declaration lands.
- **ADR-098 D5's order still applies.** D1/D2 come before the shadow measurement, so enablement stays gated on #4317. Between #4657 and #4317, a declared-inert class carries no doc-leg or data-read protection.
- **What this repository's declaration must list.** Every path that is inert today (a non-Go path with no package ancestor) needs an inert-class entry, plus the current `SharedArtifactDirs`. Examples: `README.md`, `docs/**` outside G, `frontend/**`, `.agents/**`, `.claude/**`, `.githooks/**`, `.github/**` outside `workflows/`, `infra/**`, `acceptance/**`, `auth/**`, `LICENSE`, and `deploy/**` outside helm and gate-image. An omitted path refuses as `unclassified_path`: safe, but it yields no shadow data. Decision 4's coverage check catches an inert glob that overlaps a verify-leg input.
- **Deployment order:**
  1. #4657's schema, parser and consumer merge and are DEPLOYED (fishhawkd reloaded; the MCP shim reconnected, because a stale stdio `fishhawk-mcp` rejects the new key at `start_run`).
  2. The declaration lands.

  The top-level workflow-v2 schema is closed, so the reverse order fails validation. Runs admitted before the declaration lands keep refusing transfer.
- **Older specs.** v0/v1 specs never transfer, because there is no place to declare the surface.
- **The D-class leg table** (ADR-098 D1 D-i/D-ii, #4317) is THIS repository's leg-to-inputs map. It belongs in the declaration, as part of the completeness tables under Decision 5, and #4317 implements it there.
- **What the declaration does not do.** It makes the doc-transfer case POSSIBLE, but does not deliver it. Today the documentation paths (`docs/api/**`, `site/**`, `docs/adr/**`, `docs/ARCHITECTURE.md`) are in G, and a G match refuses transfer at once. Doc-only transfers need #4317's G/D split and data-edge implementation. Even then, "doc-only" is not blanket permission: a declared-inert path still passes the G, doc-leg, data-read and shared-artifact checks, and an overlapping documentation glob never overrides G.
- **Ordering.** Implementation is #4657. Its PR merges once its implementation matches this v2.1 text.
- **Record metadata.** amends: ["ADR-098"]; supersedes: []; applies_to: []. ADR-098 gains `amended_by: ["ADR-124"]`.
