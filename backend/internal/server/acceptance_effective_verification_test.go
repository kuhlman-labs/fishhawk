package server

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/orchestrator"
	"github.com/kuhlman-labs/fishhawk/backend/internal/plan"
)

// effAllSkipBasisCriteria is the run 0aad7486 contract: every reviewed
// criterion skip_expected with a basis.
func effAllSkipBasisCriteria() []plan.AcceptanceCriterion {
	return []plan.AcceptanceCriterion{
		{ID: "crit-a", Statement: "webhook fires", Source: plan.CriterionSourceExplicit, SkipExpected: true, ExpectationBasis: "covered by webhook_integration_test.go"},
		{ID: "crit-b", Statement: "issue closes", Source: plan.CriterionSourceExplicit, SkipExpected: true, ExpectationBasis: "covered by closer_e2e_test.go"},
	}
}

// TestEffectiveAcceptanceVerification_AddMakesAllSkipPlanDrivable pins the
// adapter's done-means: a recorded add appends a drivable criterion to the
// verification, so the orchestrator's all-skip-with-basis predicate no longer
// fires — while every non-criteria verification field is the plan's own.
func TestEffectiveAcceptanceVerification_AddMakesAllSkipPlanDrivable(t *testing.T) {
	runID, stageID := uuid.New(), uuid.New()
	s, _, ar, au, rr := newAcceptanceServer(t, runID, stageID)
	seedAmendedPlanFixture(t, ar, au, rr, runID, effAllSkipBasisCriteria(), addCritOp())
	p := &plan.Plan{Verification: plan.Verification{AcceptanceCriteria: effAllSkipBasisCriteria(), TestStrategy: "unit"}}
	if !plan.AcceptanceSkippableAllSkipWithBasis(p.Verification) {
		t.Fatal("fixture: the plan verbatim must be all-skip-with-basis")
	}

	v, err := s.EffectiveAcceptanceVerification(context.Background(), runID, p)
	if err != nil {
		t.Fatalf("EffectiveAcceptanceVerification: %v", err)
	}
	var ids []string
	for _, c := range v.AcceptanceCriteria {
		ids = append(ids, c.ID)
	}
	if want := []string{"crit-a", "crit-b", "crit-op"}; !reflect.DeepEqual(ids, want) {
		t.Fatalf("effective criteria ids = %v, want %v", ids, want)
	}
	if plan.AcceptanceSkippableAllSkipWithBasis(v) {
		t.Error("effective verification is still all-skip-with-basis; the added drivable criterion would never be driven")
	}
	if v.TestStrategy != "unit" {
		t.Errorf("TestStrategy = %q, want the plan's own value", v.TestStrategy)
	}
	if len(p.Verification.AcceptanceCriteria) != 2 {
		t.Errorf("the caller's plan was mutated: %d criteria", len(p.Verification.AcceptanceCriteria))
	}
}

// TestEffectiveAcceptanceVerification_Unamended_Verbatim pins the no-behaviour-
// change path: an un-amended run gets the plan's verification verbatim.
func TestEffectiveAcceptanceVerification_Unamended_Verbatim(t *testing.T) {
	runID, stageID := uuid.New(), uuid.New()
	s, _, ar, au, rr := newAcceptanceServer(t, runID, stageID)
	seedAmendedPlanFixture(t, ar, au, rr, runID, effAllSkipBasisCriteria(), nil)
	p := &plan.Plan{Verification: plan.Verification{AcceptanceCriteria: effAllSkipBasisCriteria()}}
	v, err := s.EffectiveAcceptanceVerification(context.Background(), runID, p)
	if err != nil {
		t.Fatalf("EffectiveAcceptanceVerification: %v", err)
	}
	if !reflect.DeepEqual(v, p.Verification) {
		t.Errorf("un-amended verification = %+v, want the plan verbatim", v)
	}
}

// TestEffectiveAcceptanceVerification_NilPlan pins the nil guard.
func TestEffectiveAcceptanceVerification_NilPlan(t *testing.T) {
	s, _, _, _, _ := newAcceptanceServer(t, uuid.New(), uuid.New())
	v, err := s.EffectiveAcceptanceVerification(context.Background(), uuid.New(), nil)
	if err != nil || len(v.AcceptanceCriteria) != 0 {
		t.Errorf("nil plan = (%+v, %v), want (zero, nil)", v, err)
	}
}

// TestEffectiveAcceptanceVerification_ChainReadError pins the error branch: an
// unreadable approval chain is RETURNED (the orchestrator then falls back to
// the plan verbatim), never a partial set.
func TestEffectiveAcceptanceVerification_ChainReadError(t *testing.T) {
	runID, stageID := uuid.New(), uuid.New()
	s, _, ar, au, rr := newAcceptanceServer(t, runID, stageID)
	seedAmendedPlanFixture(t, ar, au, rr, runID, effAllSkipBasisCriteria(), addCritOp())
	au.listByCategoryErr = errors.New("audit store outage")
	p := &plan.Plan{Verification: plan.Verification{AcceptanceCriteria: effAllSkipBasisCriteria()}}
	if _, err := s.EffectiveAcceptanceVerification(context.Background(), runID, p); err == nil {
		t.Error("err = nil, want the chain read error surfaced")
	}
}

// TestServerNew_WiresEffectiveAcceptanceHook pins the server.New back-reference:
// without it the orchestrator evaluates the plan verbatim and an added
// criterion on an all-skip plan is never driven.
func TestServerNew_WiresEffectiveAcceptanceHook(t *testing.T) {
	o := &orchestrator.Orchestrator{}
	s := New(Config{Addr: "127.0.0.1:0", Orchestrator: o})
	if o.EffectiveAcceptance != s {
		t.Errorf("Orchestrator.EffectiveAcceptance = %v, want the *Server", o.EffectiveAcceptance)
	}
}
