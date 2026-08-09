# backend/internal/splitfiling

Pure logic for **on-approval child filing of an approved split proposal** (#2057, E50.5). When an operator approves a plan carrying a `split_proposal` (#2055) at the plan gate, the server hook (`backend/internal/server/split_filing.go`, a sibling slice) files N phased child issues, classifies the contract phase, and drafts a governed cap exception when needed. This package holds the dependency-light logic behind that hook so it can be unit-tested without a server or a forge.

It imports only `backend/internal/plan` (for the `SplitProposal` shape) and owns neutral input/output types — **no server import**, so the server package depends on this package and never the reverse.

## What it does

Three pure operations:

- **`BuildChildSpecs(BuildInput) []ChildSpec`** — walks the split proposal's phases in order, one `ChildSpec` per phase. Each spec carries:
  - the phase `Title` (the child issue summary),
  - a **symbol-set scope statement** synthesized from the phase's `scope_hint` (prose like `migrate consumers of Foo to NewFoo …`, or a package-level fallback derived from the phase's declared files — **never a stale file list**, which goes wrong as the migration proceeds),
  - the phase's 0-based `depends_on` edges copied verbatim (the hook resolves them to sibling `#N` at filing time in wave order),
  - the parent (run) issue and design issue (`#2008`) references, rendered into the body,
  - `IsContract` on the **terminal** phase.

  The **contract child** additionally carries the parent's acceptance criteria (it is the acceptance carrier) and, **by construction, no `Closes #<parent>` line**: a `Closes` line in a child *issue* body is functionless — GitHub auto-closes an issue only from a PR/commit and only the enclosing issue. The live parent-close mechanism is deferred to follow-up **#2062** (E50.6); every child body says so and references the parent without closing it.

- **`Classify(SplitProposal, []PhaseEvidence, cap) ContractClassification`** — decides how the contract (terminal) phase is handled:
  - `delete-only` (default) — keep the transitional names permanently, delete only; fits one in-cap PR.
  - `governed-exception` — chosen **only** when the contract phase's reachability `DerivedCount` **strictly exceeds** the resolved implement `cap`, i.e. an atomic rename cannot ship as one in-cap PR.

  It **fails safe to `delete-only`** on every uncertain branch — `cap <= 0` (unresolved), an empty proposal, or no reachability evidence for the contract phase index — honoring the issue's stated default of keeping transitional names permanently. Equal (`derived == cap`) is *not* over-cap.

- **`DraftCapException(SplitProposal, []PhaseEvidence, cap) *CapExceptionDraft`** — for a governed-exception contract phase, renders an **in-memory-only** draft (`nil` for delete-only and every fail-safe branch):
  - a unified-diff-style **spec diff** raising `max_files_changed` from `cap` to the contract phase's `DerivedCount`,
  - a **PR body** derived from the atomicity evidence (derived-vs-declared counts) that states explicitly the change must be **operator-authored and admin-merged** because `.fishhawk/**` is agent-forbidden.

  Both are strings only and are **never written to disk** — they ride the `split_children_filed` audit payload and surface via `fishhawk_get_plan`.

- **`PhaseCapViolations(SplitProposal, cap) []PhaseCapViolation`** (#2412) — the pure phase-cap validator behind the refusal defect #2412 reports. It compares **each phase's own** declared `scope.files` count against the resolved implement `cap` and returns one `PhaseCapViolation{Index, Title, DeclaredCount, Cap}` per phase over the cap, in phase order. It **fails open exactly like `Classify`**: `cap <= 0` (an unresolved cap) returns `nil` so an unresolved cap never manufactures a violation; a phase with a nil/empty scope is skipped defensively (`checkSplitProposal` already requires a non-empty per-phase scope). `PhaseCapViolation.String()` renders the operator-facing one-liner shared by the plan-gate advisory, the refusal audit payload, and the parent refusal comment — one phrasing, no drift. The server hook (`split_filing.go`) calls this immediately after resolving the cap and **files zero children** when any phase is over cap (the #2410 lead-phase-over-cap case), recording a `split_filing_refused` marker instead.

## Landability non-guarantee (#2412)

Split-filing does **not** verify a phase's **independent landability**. It checks each phase's declared file count against the implement cap and, when the runner shipped a reachability sweep, Go symbol reachability across phase boundaries — but it does **not** detect **path-keyed couplings**: literal file-path strings in surface-sweep tables, path-keyed maps in tests, and build/rebuild matrices. A phase that moves or renames files may leave such a coupling dangling and may not be individually green. `BuildChildSpecs` states this plainly on **every** child body via the exported `LandabilityCaveat` (which the server hook mirrors into the parent acceptance-carrier comment), naming the exact coupling class that broke run `1d94db6f`, and the non-contract `buildDoneMeans` no longer asserts the unsatisfiable "the resulting tree compiles as an at-or-under-cap intermediate".

## Classification rule

`governed-exception` operationalizes "naming genuinely matters" as: the contract phase's reachability-derived file count (`PhaseEvidence.DerivedCount`, from the `plan_reachability_sweep` audit entry, #2056) exceeds the resolved implement `max_files_changed` cap. The reachability sweep runs for split-proposal plans, so the evidence is normally present; when it is absent the classifier fails safe to `delete-only` rather than guessing.

`PhaseEvidence` is a neutral mirror of the server's `PlanReachabilityPhase` / the runner's `reachability.PhaseResult` (`Index` / `Title` / `DeclaredCount` / `DerivedCount`), owned here so this package takes no server or runner dependency.
