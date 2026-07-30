package server

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/plan"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
	"github.com/kuhlman-labs/fishhawk/backend/internal/spec"
)

// applies_to PLAN-GATE tests (E53.3 / #2226) — phase two of the fail-closed
// routing control. The admission half is pinned in applies_to_test.go.
//
// One assertion per enumerated branch, per #1199: this is a governance gate,
// so "the happy path plus a subset" is precisely the test shape that ships a
// control which refuses everything (or enforces nothing) and stays green.

// newAppliesToPlanGateServer wires a server whose run row carries the given
// workflow-spec SNAPSHOT and workflow id — the same bytes admission parsed,
// which is what the gate resolves its declaration from. The audit fake is the
// STORING variant so runHasAppliesToOverride actually reads back seeded
// override entries rather than always erroring.
func newAppliesToPlanGateServer(t *testing.T, workflowSpec, workflowID string) (*Server, *storingAuditFake, *run.Run) {
	t.Helper()
	rr := newOrchestratorRepo()
	au := newStoringAuditFake()
	runRow := rr.seedRun()
	runRow.WorkflowID = workflowID
	runRow.WorkflowSpec = []byte(workflowSpec)
	s := New(Config{
		Addr:      "127.0.0.1:0",
		AuditRepo: au,
		RunRepo:   rr,
	})
	return s, au, runRow
}

// pathsAppliesToSpec builds the two-workflow v2 fixture with `guarded`
// declaring a paths-only applies_to over the given globs. `open` declares none
// and is therefore always a satisfying alternative, which is what lets the
// rejection-message assertions check the "where can I take this instead?" half.
func pathsAppliesToSpec(globs ...string) string {
	return appliesToSpec("    applies_to:\n      paths: [" + strings.Join(globs, ", ") + "]\n")
}

// planWithScope builds a plan.Plan carrying the given top-level scope files.
// The gate reads the plan struct directly (handleShipPlan decodes the body
// once and hands it over), so the unit tests construct it directly rather than
// round-tripping JSON; the wired end-to-end path is covered by
// TestShipPlan_AppliesToPathsViolation_FailsB in plan_test.go.
func planWithScope(paths ...string) *plan.Plan {
	p := &plan.Plan{}
	for _, path := range paths {
		p.Scope.Files = append(p.Scope.Files, plan.ScopeFile{Path: path, Operation: plan.FileOpModify})
	}
	return p
}

func scopeOf(paths ...string) *plan.Scope {
	sc := &plan.Scope{}
	for _, path := range paths {
		sc.Files = append(sc.Files, plan.ScopeFile{Path: path, Operation: plan.FileOpModify})
	}
	return sc
}

// seedOverrideEntry appends the run-scoped run_admitted_applies_to_override
// entry checkAppliesTo writes when an operator forces admission — the
// override's carry-forward SOURCE OF TRUTH.
func seedOverrideEntry(t *testing.T, au *storingAuditFake, runID uuid.UUID) {
	t.Helper()
	kind := audit.ActorKind("system")
	if _, err := au.AppendChained(context.Background(), audit.ChainAppendParams{
		RunID:     runID,
		Category:  "run_admitted_applies_to_override",
		ActorKind: &kind,
		Payload:   []byte(`{"reason":"operator forced this run"}`),
	}); err != nil {
		t.Fatalf("seed override entry: %v", err)
	}
}

// --- M8: the accept path -------------------------------------------------

// TestAppliesToPlanGate_SatisfyingScope_Clears is M8: a plan whose scope.files
// all fall inside the declared globs clears the gate. This is the criterion
// every non-exceptional run depends on — a control that rejects everything
// would still satisfy each rejection assertion below, so the admit path needs
// its own pin (the operator's binding condition 2, applied to phase two).
func TestAppliesToPlanGate_SatisfyingScope_Clears(t *testing.T) {
	s, _, runRow := newAppliesToPlanGateServer(t, pathsAppliesToSpec(`"docs/**"`), "guarded")
	p := planWithScope("docs/MVP_SPEC.md", "docs/spec/workflow-v2.md")

	if reason := s.appliesToPlanGateRejection(context.Background(), runRow.ID, p); reason != "" {
		t.Errorf("want a docs-only plan to clear a docs-only declaration; got reject %q", reason)
	}
}

// --- M9: the reject path and its message shape ---------------------------

// TestAppliesToPlanGate_ViolatingScope_Rejects is M9: a plan reaching outside
// the declaration is rejected with the SAME message shape the admission
// rejection uses (M3) — the workflow, the criterion that failed, the value
// observed, and which workflows WOULD accept the change. An operator refused
// at the plan gate is further into a run than one refused at admission and
// needs more help, not less.
func TestAppliesToPlanGate_ViolatingScope_Rejects(t *testing.T) {
	s, _, runRow := newAppliesToPlanGateServer(t, pathsAppliesToSpec(`"docs/**"`), "guarded")
	p := planWithScope("backend/internal/server/plan.go")

	reason := s.appliesToPlanGateRejection(context.Background(), runRow.ID, p)
	if reason == "" {
		t.Fatal("want a reject for a plan scoping backend/** under a docs-only declaration")
	}
	for _, want := range []string{
		`"guarded"`,                       // the workflow
		"paths",                           // the criterion that failed
		"docs/**",                         // what the declaration requires
		"backend/internal/server/plan.go", // the value observed
		"Workflows that would accept",     // the alternatives
		"open",                            // ... which includes the unconstrained workflow
		"applies_to_override",             // the sanctioned exception
	} {
		if !strings.Contains(reason, want) {
			t.Errorf("reject reason missing %q\ngot: %s", want, reason)
		}
	}
}

// --- The universal-vs-existential keystone -------------------------------

// TestAppliesToPlanGate_PartiallyMatchingScope_Rejects is the load-bearing
// semantic pin. spec.Predicate.Match's `paths` rule is EXISTENTIAL (any change
// path matching any glob satisfies it), which is right for its other consumers
// but wrong for a confinement control: under existential semantics this plan —
// one docs file plus one backend file — would SATISFY `paths: ["docs/**"]`,
// and the contract published in docs/spec/workflow-v2.md ("a run admitted
// under a workflow declaring paths: [docs/**] is therefore *confined* to
// docs/**, not merely claimed to be") would be false.
//
// The gate therefore applies the UNIVERSAL quantifier over the ratified
// matcher used verbatim. If someone "simplifies" planGateUnmatchedPaths into a
// single sub.Match(union) call, this test is the one that fails — and it is
// the only test here that would.
func TestAppliesToPlanGate_PartiallyMatchingScope_Rejects(t *testing.T) {
	s, _, runRow := newAppliesToPlanGateServer(t, pathsAppliesToSpec(`"docs/**"`), "guarded")
	p := planWithScope("docs/ARCHITECTURE.md", "backend/internal/server/plan.go")

	reason := s.appliesToPlanGateRejection(context.Background(), runRow.ID, p)
	if reason == "" {
		t.Fatal("want a reject: one in-declaration file must not launder an out-of-declaration one")
	}
	if !strings.Contains(reason, "backend/internal/server/plan.go") {
		t.Errorf("reject reason must name the OFFENDING path; got: %s", reason)
	}
	if strings.Contains(reason, "docs/ARCHITECTURE.md") {
		t.Errorf("reject reason should name only out-of-declaration files, not the satisfying one; got: %s", reason)
	}
}

// --- M9b: the scope UNION, not the top level -----------------------------

// TestAppliesToPlanGate_SubPlanScopeViolation_Rejects is M9b: a DECOMPOSED
// plan whose top-level scope is clean but whose sub_plan scope reaches outside
// the declaration is rejected. The fan-out child runs bounded to the SLICE
// scope, so checking only the top level would let a decomposed slice escape
// the routing declaration entirely.
func TestAppliesToPlanGate_SubPlanScopeViolation_Rejects(t *testing.T) {
	s, _, runRow := newAppliesToPlanGateServer(t, pathsAppliesToSpec(`"docs/**"`), "guarded")
	p := planWithScope("docs/METHODOLOGY.md")
	p.Decomposition = &plan.Decomposition{
		Rationale: "two slices",
		SubPlans: []plan.SubPlanSummary{
			{Title: "docs slice", Scope: scopeOf("docs/ARCHITECTURE.md")},
			{Title: "escaping slice", Scope: scopeOf("backend/internal/server/runs.go")},
		},
	}

	reason := s.appliesToPlanGateRejection(context.Background(), runRow.ID, p)
	if reason == "" {
		t.Fatal("want a reject: a sub-plan scope outside the declaration must not pass on a clean top-level scope")
	}
	if !strings.Contains(reason, "backend/internal/server/runs.go") {
		t.Errorf("reject reason must name the offending SUB-PLAN path; got: %s", reason)
	}
}

// TestAppliesToPlanGate_SplitPhaseScopeViolation_Rejects is M9b's split-phase
// twin: a split_proposal phase carries its own scope for the same structural
// reason a sub-plan does, so it is folded into the same union.
func TestAppliesToPlanGate_SplitPhaseScopeViolation_Rejects(t *testing.T) {
	s, _, runRow := newAppliesToPlanGateServer(t, pathsAppliesToSpec(`"docs/**"`), "guarded")
	p := planWithScope("docs/METHODOLOGY.md")
	p.SplitProposal = &plan.SplitProposal{
		Rationale: "expand then contract",
		Phases: []plan.SplitPhase{
			{Title: "Expand", Scope: scopeOf("docs/BRAND_FOUNDATIONS.md")},
			{Title: "Contract", Scope: scopeOf("cli/cmd/fishhawk/run.go")},
		},
	}

	reason := s.appliesToPlanGateRejection(context.Background(), runRow.ID, p)
	if reason == "" {
		t.Fatal("want a reject: a split-phase scope outside the declaration must not pass on a clean top-level scope")
	}
	if !strings.Contains(reason, "cli/cmd/fishhawk/run.go") {
		t.Errorf("reject reason must name the offending SPLIT-PHASE path; got: %s", reason)
	}
}

// TestPlanGateScopePaths_UnionDedupSorted pins the union helper itself:
// top-level + sub-plan + split-phase scopes, de-duplicated, slash-normalized
// and sorted, with a nil sub-plan scope (inherit-parent) tolerated.
func TestPlanGateScopePaths_UnionDedupSorted(t *testing.T) {
	p := planWithScope("docs/b.md", "docs/a.md")
	p.Decomposition = &plan.Decomposition{SubPlans: []plan.SubPlanSummary{
		{Title: "inherits parent scope", Scope: nil},
		{Title: "own scope", Scope: scopeOf("docs/a.md", "backend/x.go")},
	}}
	p.SplitProposal = &plan.SplitProposal{Phases: []plan.SplitPhase{
		{Title: "phase", Scope: scopeOf("cli/y.go")},
	}}

	got := planGateScopePaths(p)
	want := []string{"backend/x.go", "cli/y.go", "docs/a.md", "docs/b.md"}
	if len(got) != len(want) {
		t.Fatalf("planGateScopePaths() = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("planGateScopePaths() = %v, want %v", got, want)
		}
	}
	if planGateScopePaths(nil) != nil {
		t.Error("want nil for a nil plan")
	}
}

// --- Non-constraining declarations ---------------------------------------

// TestAppliesToPlanGate_NoAppliesTo_Clears is the back-compat leg: a workflow
// declaring no applies_to accepts any change, so every plan written before
// this property existed is unaffected.
func TestAppliesToPlanGate_NoAppliesTo_Clears(t *testing.T) {
	s, _, runRow := newAppliesToPlanGateServer(t, appliesToSpec(""), "guarded")

	if reason := s.appliesToPlanGateRejection(context.Background(), runRow.ID,
		planWithScope("anything/at/all.go")); reason != "" {
		t.Errorf("want an undeclared workflow to accept any plan; got reject %q", reason)
	}
}

// TestAppliesToPlanGate_AdmissionOnlyDeclaration_Clears is the mirror of M7
// (a paths-only predicate ADMITS at start_run): a labels-only declaration
// does not constrain the PLAN-GATE phase, so the gate must not invent a
// refusal for a criterion checkAppliesTo already decided. Without the
// `constrains` guard the empty phase sub-predicate would reach Match and fail
// closed, rejecting every plan under a labels-declaring workflow.
func TestAppliesToPlanGate_AdmissionOnlyDeclaration_Clears(t *testing.T) {
	s, _, runRow := newAppliesToPlanGateServer(t,
		appliesToSpec("    applies_to:\n      labels: [dependencies]\n"), "guarded")

	if reason := s.appliesToPlanGateRejection(context.Background(), runRow.ID,
		planWithScope("backend/anything.go")); reason != "" {
		t.Errorf("want a labels-only declaration not to constrain the plan gate; got reject %q", reason)
	}
}

// --- Fail-CLOSED legs ----------------------------------------------------

// TestAppliesToPlanGate_EmptyScope_Rejects pins the zero-path leg: a plan
// committing to no files demonstrates nothing about falling inside the
// declaration, so it is REFUSED rather than admitted on a vacuous "every one
// of zero paths matched". This delegates to the predicate's own ratified
// zero-path answer.
func TestAppliesToPlanGate_EmptyScope_Rejects(t *testing.T) {
	s, _, runRow := newAppliesToPlanGateServer(t, pathsAppliesToSpec(`"docs/**"`), "guarded")

	reason := s.appliesToPlanGateRejection(context.Background(), runRow.ID, &plan.Plan{})
	if reason == "" {
		t.Fatal("want a reject for a plan with an empty scope (fail-closed, not vacuous-true)")
	}
	if !strings.Contains(reason, "scope.files") {
		t.Errorf("reject reason should name scope.files when there is no offending entry to name; got: %s", reason)
	}
}

// TestAppliesToPlanGate_NilPlan_Rejects pins the UNDECODABLE-BODY leg. The
// call site in handleShipPlan hands the gate a nil plan when json.Unmarshal of
// the plan body fails, instead of skipping the gate the way the ADVISORY
// over-cap sweep in the same block does — a fail-open leg on a fail-closed
// control would not be among the ones the posture enumerates (nil RunRepo, run
// not found, nil/unparseable spec snapshot, workflow absent), and this is the
// assertion that a "simplification" back inside that decode guard would have to
// delete. A nil plan is a ZERO-PATH plan: refused under a paths declaration for
// the same reason an empty scope.files is.
//
// REACHABILITY, stated rather than implied (as for the malformed-glob leg
// below): the body has already passed plan.Validate against the closed
// standard_v1 schema by the time the decode runs, so schema-valid bytes failing
// json.Unmarshal is an internal inconsistency and no end-to-end test can
// produce one. The assertion is therefore made where the contract lives — the
// gate's own treatment of the input that leg supplies.
func TestAppliesToPlanGate_NilPlan_Rejects(t *testing.T) {
	s, _, runRow := newAppliesToPlanGateServer(t, pathsAppliesToSpec(`"docs/**"`), "guarded")

	reason := s.appliesToPlanGateRejection(context.Background(), runRow.ID, nil)
	if reason == "" {
		t.Fatal("want a nil (undecodable) plan refused, not admitted — nil is a zero-path plan, not a skip")
	}
	if !strings.Contains(reason, "scope.files") {
		t.Errorf("reject reason should name scope.files when there is no offending entry to name; got: %s", reason)
	}

	// The refusal is scoped to a declaration that actually constrains this
	// phase: a workflow declaring no paths criterion still clears, so an
	// undecodable body cannot turn into a blanket plan-ship outage.
	sOpen, _, openRun := newAppliesToPlanGateServer(t, pathsAppliesToSpec(`"docs/**"`), "open")
	if reason := sOpen.appliesToPlanGateRejection(context.Background(), openRun.ID, nil); reason != "" {
		t.Errorf("want a workflow with no paths criterion to clear a nil plan; got %q", reason)
	}
}

// TestPlanGateUnmatchedPaths_MalformedGlob_FailsClosed is M13's plan-gate
// half: a glob that makes spec.Predicate.Match return an error is PROPAGATED,
// never swallowed into "nothing unmatched" (which the caller would read as a
// pass). The four advisory sweeps in the same handler (runScopePrecheck,
// runSurfaceSweep, runTestSweep, overCapSplitRejection) all correctly fail
// OPEN, which is exactly why this needs its own assertion — `if err != nil {
// return nil, nil }` written by analogy with them is the tempting bug.
//
// REACHABILITY, stated rather than implied: this branch is defense in depth
// and is NOT reachable through a stored spec today, because slice 0's
// validateWorkflow calls Predicate.Validate at parse time, so a spec carrying a
// malformed glob fails spec.ParseBytes — at admission, before any run row
// exists, and again (fail-open, warn-logged) at the gate's resolver. The
// assertion is therefore made directly against the pure function that owns the
// posture, which is the honest place to make it: the guard is real code with a
// real contract, and a test that faked an unreachable end-to-end path would be
// asserting the wrong thing.
func TestPlanGateUnmatchedPaths_MalformedGlob_FailsClosed(t *testing.T) {
	unmatched, err := planGateUnmatchedPaths([]string{"docs/[unclosed"}, []string{"docs/a.md"})
	if err == nil {
		t.Fatal("want a malformed glob to return an error, not a silent non-match")
	}
	if unmatched != nil {
		t.Errorf("want no partial result alongside the error; got %v", unmatched)
	}

	// The zero-path leg must surface the same error rather than short-circuit
	// past it into the (false, nil) refusal.
	if _, err := planGateUnmatchedPaths([]string{"docs/[unclosed"}, nil); err == nil {
		t.Error("want the malformed glob surfaced on the zero-path leg too")
	}
}

// TestAppliesToPlanGate_MalformedGlobSpec_FailsOpenAtParse records the
// consequence of that reachability analysis as an executable fact rather than
// a comment: a malformed glob never reaches the gate's Match at all, because
// the spec snapshot does not parse. The resolver's fail-open leg takes it, and
// the run was already unable to be created in the first place.
func TestAppliesToPlanGate_MalformedGlobSpec_FailsOpenAtParse(t *testing.T) {
	s, _, runRow := newAppliesToPlanGateServer(t, pathsAppliesToSpec(`"docs/[unclosed"`), "guarded")

	if reason := s.appliesToPlanGateRejection(context.Background(), runRow.ID,
		planWithScope("backend/anything.go")); reason != "" {
		t.Errorf("want the unparseable-snapshot fail-open leg; got %q", reason)
	}
}

// --- The audited override: carry-forward and its source of truth ---------

// TestAppliesToPlanGate_OverrideEntry_SuppressesRejection is M11: the
// admission-time override carries forward end to end. The run-scoped audit
// entry recorded at run-create suppresses the plan-gate refusal, so an
// operator who forced a run past admission is not re-refused at the next gate.
func TestAppliesToPlanGate_OverrideEntry_SuppressesRejection(t *testing.T) {
	s, au, runRow := newAppliesToPlanGateServer(t, pathsAppliesToSpec(`"docs/**"`), "guarded")
	p := planWithScope("backend/internal/server/plan.go")

	if reason := s.appliesToPlanGateRejection(context.Background(), runRow.ID, p); reason == "" {
		t.Fatal("precondition: this plan must be rejected without an override")
	}

	seedOverrideEntry(t, au, runRow.ID)

	if reason := s.appliesToPlanGateRejection(context.Background(), runRow.ID, p); reason != "" {
		t.Errorf("want the audited override to suppress the plan-gate rejection; got %q", reason)
	}
}

// TestAppliesToPlanGate_OverrideAbsent_Rejects is M11b: the AUDIT ENTRY is the
// source of truth, not the create request. A run whose creation carried an
// override but whose entry is absent — the stated residual failure mode, an
// audit append that failed at run-create — has no override here and is
// refused. This is fail-closed by construction and recoverable (re-start with
// the override, or widen the declaration).
func TestAppliesToPlanGate_OverrideAbsent_Rejects(t *testing.T) {
	s, au, runRow := newAppliesToPlanGateServer(t, pathsAppliesToSpec(`"docs/**"`), "guarded")

	// A DIFFERENT run's override must not leak across runs, and an unrelated
	// category on this run must not be mistaken for one.
	seedOverrideEntry(t, au, uuid.New())
	kind := audit.ActorKind("system")
	if _, err := au.AppendChained(context.Background(), audit.ChainAppendParams{
		RunID: runRow.ID, Category: "run_created", ActorKind: &kind, Payload: []byte(`{}`),
	}); err != nil {
		t.Fatalf("seed unrelated entry: %v", err)
	}

	if reason := s.appliesToPlanGateRejection(context.Background(), runRow.ID,
		planWithScope("backend/internal/server/plan.go")); reason == "" {
		t.Error("want a reject when the run carries no run_admitted_applies_to_override entry")
	}
}

// TestAppliesToPlanGate_OverrideLookupError_Rejects is the third override
// branch: an override we cannot CONFIRM is not an override. A failing audit
// read must refuse rather than degrade into permission — the inverse mistake
// to M13's, and just as reachable.
func TestAppliesToPlanGate_OverrideLookupError_Rejects(t *testing.T) {
	rr := newOrchestratorRepo()
	au := newAuditFake()
	au.listByCategoryErr = errors.New("audit read down")
	runRow := rr.seedRun()
	runRow.WorkflowID = "guarded"
	runRow.WorkflowSpec = []byte(pathsAppliesToSpec(`"docs/**"`))
	s := New(Config{Addr: "127.0.0.1:0", AuditRepo: au, RunRepo: rr})

	if reason := s.appliesToPlanGateRejection(context.Background(), runRow.ID,
		planWithScope("backend/internal/server/plan.go")); reason == "" {
		t.Error("want a reject when the override lookup fails (fail-closed, not fail-open)")
	}
}

// --- Fail-OPEN legs: no declaration can be resolved ----------------------

// TestAppliesToPlanGate_UnresolvableDeclaration_FailsOpen pins the narrow
// fail-open half of the deliberately asymmetric posture. Where NO declaration
// can be resolved there is nothing to enforce, and refusing every plan would
// be a denial of service rather than a control. Each leg is warn-logged so it
// is visible if it ever occurs; none is reachable in practice, because these
// same bytes were parsed successfully at admission.
func TestAppliesToPlanGate_UnresolvableDeclaration_FailsOpen(t *testing.T) {
	violating := planWithScope("backend/internal/server/plan.go")

	t.Run("nil RunRepo", func(t *testing.T) {
		s := New(Config{Addr: "127.0.0.1:0"}) // RunRepo intentionally nil
		if reason := s.appliesToPlanGateRejection(context.Background(), uuid.New(), violating); reason != "" {
			t.Errorf("want fail-open with a nil RunRepo; got %q", reason)
		}
	})

	t.Run("run not found", func(t *testing.T) {
		s, _, _ := newAppliesToPlanGateServer(t, pathsAppliesToSpec(`"docs/**"`), "guarded")
		if reason := s.appliesToPlanGateRejection(context.Background(), uuid.New(), violating); reason != "" {
			t.Errorf("want fail-open when the run genuinely does not exist; got %q", reason)
		}
	})

	t.Run("nil workflow spec", func(t *testing.T) {
		s, _, runRow := newAppliesToPlanGateServer(t, pathsAppliesToSpec(`"docs/**"`), "guarded")
		runRow.WorkflowSpec = nil
		if reason := s.appliesToPlanGateRejection(context.Background(), runRow.ID, violating); reason != "" {
			t.Errorf("want fail-open with no workflow-spec snapshot; got %q", reason)
		}
	})

	t.Run("unparseable workflow spec", func(t *testing.T) {
		s, _, runRow := newAppliesToPlanGateServer(t, pathsAppliesToSpec(`"docs/**"`), "guarded")
		runRow.WorkflowSpec = []byte("this: is: not: a: spec\n")
		if reason := s.appliesToPlanGateRejection(context.Background(), runRow.ID, violating); reason != "" {
			t.Errorf("want fail-open on an unparseable spec snapshot; got %q", reason)
		}
	})

	t.Run("workflow absent from spec", func(t *testing.T) {
		s, _, runRow := newAppliesToPlanGateServer(t, pathsAppliesToSpec(`"docs/**"`), "guarded")
		runRow.WorkflowID = "no_such_workflow"
		if reason := s.appliesToPlanGateRejection(context.Background(), runRow.ID, violating); reason != "" {
			t.Errorf("want fail-open when the workflow is absent from the spec; got %q", reason)
		}
	})
}

// transientGetRunRepo is a run.Repository whose GetRun fails with a NON-
// not-found error — a database blip, a timeout, a reset connection. Only GetRun
// is reached on the gate's resolution path, so the embedded interface stays nil.
type transientGetRunRepo struct {
	run.Repository
	err error
}

func (f transientGetRunRepo) GetRun(context.Context, uuid.UUID) (*run.Run, error) {
	return nil, f.err
}

// TestAppliesToPlanGate_TransientReadFailure_FailsClosed draws the line the
// fail-open legs above must not cross. "There is no declaration" (run not
// found, no snapshot, workflow absent) is a FACT about the workflow and admits.
// "The store did not answer" is an ABSENCE OF KNOWLEDGE, and admitting on it
// would leave a governance control that any repository hiccup silently switches
// off — at exactly the moment the plan being gated is the one that would have
// been refused, since a plan that satisfies the declaration is admitted either
// way. The refusal is retry-shaped: the failure is transient, so re-shipping
// the plan recovers, whereas a silent admission never does.
func TestAppliesToPlanGate_TransientReadFailure_FailsClosed(t *testing.T) {
	s := New(Config{
		Addr:      "127.0.0.1:0",
		RunRepo:   transientGetRunRepo{err: errors.New("dial tcp: connection reset by peer")},
		AuditRepo: newStoringAuditFake(),
	})
	reason := s.appliesToPlanGateRejection(context.Background(), uuid.New(),
		planWithScope("backend/internal/server/plan.go"))
	if reason == "" {
		t.Fatal("a transient run-store read failure was treated as 'no declaration to enforce' and admitted the plan; a repository blip must not switch a governance gate off")
	}
	for _, want := range []string{"could not be read", "connection reset", "transient"} {
		if !strings.Contains(reason, want) {
			t.Errorf("reason %q is missing %q — the message must say what failed and that a retry recovers", reason, want)
		}
	}
}

// TestResolveRunWorkflowDef_SeparatesAbsenceFromUnreadability pins the
// distinction at its source, so a future caller cannot collapse the two by
// reading only the ok flag.
func TestResolveRunWorkflowDef_SeparatesAbsenceFromUnreadability(t *testing.T) {
	t.Run("transient error", func(t *testing.T) {
		s := New(Config{Addr: "127.0.0.1:0", RunRepo: transientGetRunRepo{err: errors.New("db down")}})
		_, _, _, _, ok, err := s.resolveRunWorkflowDef(context.Background(), uuid.New())
		if err == nil {
			t.Fatal("a repository read failure was reported as a clean 'no declaration' answer")
		}
		if ok {
			t.Error("ok = true on a read failure")
		}
	})

	t.Run("run not found", func(t *testing.T) {
		s, _, _ := newAppliesToPlanGateServer(t, pathsAppliesToSpec(`"docs/**"`), "guarded")
		_, _, _, _, ok, err := s.resolveRunWorkflowDef(context.Background(), uuid.New())
		if err != nil {
			t.Fatalf("run-not-found must be an ABSENCE, not an outage; got err = %v", err)
		}
		if ok {
			t.Error("ok = true for a run that does not exist")
		}
	})
}

// TestRenderUnmatchedPaths_CapsAndReportsRemainder pins the message cap: a
// large violation stays actionable, and the truncation reports its own size
// rather than silently understating the violation.
func TestRenderUnmatchedPaths_CapsAndReportsRemainder(t *testing.T) {
	var many []string
	for i := 0; i < maxRenderedUnmatchedPaths+5; i++ {
		many = append(many, "backend/f.go")
	}
	got := renderUnmatchedPaths(many)
	if len(got) != maxRenderedUnmatchedPaths+1 {
		t.Fatalf("len(renderUnmatchedPaths()) = %d, want %d", len(got), maxRenderedUnmatchedPaths+1)
	}
	if !strings.Contains(got[len(got)-1], "and 5 more") {
		t.Errorf("want the remainder count reported; got %q", got[len(got)-1])
	}
	short := []string{"a", "b"}
	if len(renderUnmatchedPaths(short)) != 2 {
		t.Error("want an under-cap list returned unchanged")
	}
}

// TestPlanGateSatisfyingWorkflows_QuantifiedLikeTheGate pins that the
// "workflows that would accept this change" list is computed with the SAME
// universal quantifier as the decision. satisfyingWorkflows (applies_to.go) is
// existential on paths, so reusing it here would let the message recommend a
// workflow that then refuses the very same plan at the very same gate —
// advice that costs the operator a second rejection.
//
// `narrow` accepts only docs/**; the plan mixes a docs file with a backend
// file. Under existential semantics `narrow` would be listed as an
// alternative; under the gate's own semantics it is not.
func TestPlanGateSatisfyingWorkflows_QuantifiedLikeTheGate(t *testing.T) {
	parsed, err := spec.ParseBytes([]byte(pathsAppliesToSpec(`"docs/**"`)))
	if err != nil {
		t.Fatalf("parse fixture spec: %v", err)
	}

	// The run's admission-phase change: a diff-form run carrying the labels
	// the recommended workflows must also accept.
	adm := spec.Change{Trigger: spec.TriggerDiff, Labels: []string{"bug"}}

	got := planGateSatisfyingWorkflows(parsed, []string{"docs/a.md", "backend/b.go"}, adm)
	for _, name := range got {
		if name == "guarded" {
			t.Errorf("a workflow that would REJECT this plan must not be recommended; got %v", got)
		}
	}
	if len(got) != 1 || got[0] != "open" {
		t.Errorf("want only the unconstrained workflow; got %v", got)
	}

	// A wholly-satisfying path set lists both.
	if both := planGateSatisfyingWorkflows(parsed, []string{"docs/a.md"}, adm); len(both) != 2 {
		t.Errorf("want both workflows for a docs-only plan; got %v", both)
	}

	// A zero-path plan cannot be accepted by a paths-declaring workflow.
	if zero := planGateSatisfyingWorkflows(parsed, nil, adm); len(zero) != 1 || zero[0] != "open" {
		t.Errorf("want only the unconstrained workflow for a zero-path plan; got %v", zero)
	}
}

// TestPlanGateSatisfyingWorkflows_AlsoFiltersOnAdmissionCriteria is the other
// half of "the message must name workflows this change could actually use", and
// the half a paths-only enumeration silently gets wrong.
//
// The operator's next move after a plan-gate refusal is to re-start the run
// under one of the named alternatives — and that re-start goes through
// ADMISSION first. A candidate filtered on `paths` alone therefore includes
// every labels-only or trigger-only workflow unconditionally (they do not
// constrain the plan gate at all), sending the operator from this refusal
// straight into a second, fail-closed admission rejection. The run's labels and
// trigger are immutable for its lifetime, so both are knowable here.
//
// The fixture covers the combined edge the concern named: `labelled` is
// label-incompatible AND path-compatible (so only the admission check can
// exclude it), `both` requires a label the run HAS and paths the plan
// satisfies, and `paths_only` is the pure plan-gate case.
func TestPlanGateSatisfyingWorkflows_AlsoFiltersOnAdmissionCriteria(t *testing.T) {
	stages := `    stages:
      - id: plan
        type: plan
        executor:
          agent: claude-code
        produces:
          - artifact: plan
            schema: standard_v1
      - id: implement
        type: implement
        executor:
          agent: claude-code
        produces:
          - artifact: pull_request
`
	doc := "version: \"2\"\nworkflows:\n" +
		"  labelled:\n    applies_to:\n      labels: [dependencies]\n" + stages +
		"  both:\n    applies_to:\n      labels: [documentation]\n      paths: [\"docs/**\"]\n" + stages +
		"  paths_only:\n    applies_to:\n      paths: [\"docs/**\"]\n" + stages +
		"  scheduled_only:\n    applies_to:\n      trigger: [scheduled]\n" + stages +
		"  open:\n" + stages
	parsed, err := spec.ParseBytes([]byte(doc))
	if err != nil {
		t.Fatalf("parse fixture spec: %v", err)
	}

	// A diff run carrying `documentation`, planning a docs-only change.
	adm := spec.Change{Trigger: spec.TriggerDiff, Labels: []string{"documentation"}}
	got := planGateSatisfyingWorkflows(parsed, []string{"docs/a.md"}, adm)

	for _, unusable := range []string{"labelled", "scheduled_only"} {
		for _, name := range got {
			if name == unusable {
				t.Errorf("workflow %q cannot ADMIT this run (its %s criterion is unsatisfiable by the run's immutable labels/trigger) yet was recommended as an alternative; got %v",
					unusable, map[string]string{"labelled": "labels", "scheduled_only": "trigger"}[unusable], got)
			}
		}
	}
	want := []string{"both", "open", "paths_only"}
	if len(got) != len(want) {
		t.Fatalf("planGateSatisfyingWorkflows = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("planGateSatisfyingWorkflows = %v, want %v (name order)", got, want)
		}
	}

	// The same plan under a run whose labels satisfy `labelled` instead:
	// `labelled` becomes a usable alternative and `both` stops being one, so
	// the filter is demonstrably reading the run's labels rather than a
	// constant.
	deps := spec.Change{Trigger: spec.TriggerDiff, Labels: []string{"dependencies"}}
	got = planGateSatisfyingWorkflows(parsed, []string{"docs/a.md"}, deps)
	wantDeps := []string{"labelled", "open", "paths_only"}
	if len(got) != len(wantDeps) {
		t.Fatalf("planGateSatisfyingWorkflows(deps) = %v, want %v", got, wantDeps)
	}
	for i := range wantDeps {
		if got[i] != wantDeps[i] {
			t.Fatalf("planGateSatisfyingWorkflows(deps) = %v, want %v", got, wantDeps)
		}
	}
}

// TestAppliesToPlanGate_RejectionNamesOnlyUsableAlternatives drives the same
// invariant through the REAL gate — the run row's own labels and trigger, not a
// hand-built spec.Change — because that wiring is where the observed values
// would go missing. The blocking requirement is that the refusal names
// workflows the change would satisfy; a message that names a workflow whose
// admission this run cannot pass fails it.
func TestAppliesToPlanGate_RejectionNamesOnlyUsableAlternatives(t *testing.T) {
	stages := `    stages:
      - id: plan
        type: plan
        executor:
          agent: claude-code
        produces:
          - artifact: plan
            schema: standard_v1
      - id: implement
        type: implement
        executor:
          agent: claude-code
        produces:
          - artifact: pull_request
`
	doc := "version: \"2\"\nworkflows:\n" +
		"  guarded:\n    applies_to:\n      paths: [\"docs/**\"]\n" + stages +
		"  needs_label:\n    applies_to:\n      labels: [dependencies]\n" + stages +
		"  open:\n" + stages
	s, _, runRow := newAppliesToPlanGateServer(t, doc, "guarded")
	// The run is issue-triggered and carries a label `needs_label` does not
	// accept — immutable for the life of the run.
	runRow.TriggerSource = run.TriggerGitHubIssue
	runRow.IssueContext = &run.IssueContext{Number: 1, Labels: []string{"bug"}}

	reason := s.appliesToPlanGateRejection(context.Background(), runRow.ID,
		planWithScope("backend/internal/server/plan.go"))
	if reason == "" {
		t.Fatal("an out-of-declaration scope cleared the gate")
	}
	if strings.Contains(reason, "needs_label") {
		t.Errorf("the refusal recommends %q, which would reject this same run at ADMISSION on its labels criterion; the operator is sent from one fail-closed refusal into another:\n%s", "needs_label", reason)
	}
	if !strings.Contains(reason, "open") {
		t.Errorf("the refusal must still name the alternative that WOULD accept this run:\n%s", reason)
	}
}
