package planreview_test

import (
	"encoding/json"
	"testing"

	"github.com/kuhlman-labs/fishhawk/backend/internal/planreview"
	"github.com/kuhlman-labs/fishhawk/backend/internal/spec"
)

// --- ResolveAuthority ---

func TestResolveAuthority_Gating(t *testing.T) {
	r := spec.ReviewersConfig{Agent: 2, Human: 0}
	if got := planreview.ResolveAuthority(r); got != planreview.AuthorityGating {
		t.Errorf("ResolveAuthority(%+v) = %q, want %q", r, got, planreview.AuthorityGating)
	}
}

func TestResolveAuthority_Advisory(t *testing.T) {
	r := spec.ReviewersConfig{Agent: 1, Human: 1}
	if got := planreview.ResolveAuthority(r); got != planreview.AuthorityAdvisory {
		t.Errorf("ResolveAuthority(%+v) = %q, want %q", r, got, planreview.AuthorityAdvisory)
	}
}

func TestResolveAuthority_Gateless(t *testing.T) {
	r := spec.ReviewersConfig{Agent: 0, Human: 1}
	if got := planreview.ResolveAuthority(r); got != planreview.AuthorityGateless {
		t.Errorf("ResolveAuthority(%+v) = %q, want %q", r, got, planreview.AuthorityGateless)
	}
}

func TestResolveAuthority_GatelessZero(t *testing.T) {
	r := spec.ReviewersConfig{}
	if got := planreview.ResolveAuthority(r); got != planreview.AuthorityGateless {
		t.Errorf("ResolveAuthority(zero) = %q, want %q", got, planreview.AuthorityGateless)
	}
}

// --- Verdict JSON round-trip ---

func TestReviewVerdict_JSONRoundTrip_Approve(t *testing.T) {
	v := planreview.ReviewVerdict{
		Verdict:  planreview.VerdictApprove,
		FreeForm: "looks good",
	}
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var got planreview.ReviewVerdict
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if got.Verdict != planreview.VerdictApprove {
		t.Errorf("Verdict = %q, want %q", got.Verdict, planreview.VerdictApprove)
	}
	if got.FreeForm != v.FreeForm {
		t.Errorf("FreeForm = %q, want %q", got.FreeForm, v.FreeForm)
	}
	if len(got.Concerns) != 0 {
		t.Errorf("Concerns should be empty, got %d", len(got.Concerns))
	}
}

func TestReviewVerdict_JSONRoundTrip_ApproveWithConcerns(t *testing.T) {
	v := planreview.ReviewVerdict{
		Verdict: planreview.VerdictApproveWithConcerns,
		Concerns: []planreview.Concern{
			{Severity: planreview.SeverityMedium, Category: "scope", Note: "touches more files than expected"},
		},
	}
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var got planreview.ReviewVerdict
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if got.Verdict != planreview.VerdictApproveWithConcerns {
		t.Errorf("Verdict = %q, want %q", got.Verdict, planreview.VerdictApproveWithConcerns)
	}
	if len(got.Concerns) != 1 {
		t.Fatalf("Concerns len = %d, want 1", len(got.Concerns))
	}
	if got.Concerns[0].Severity != planreview.SeverityMedium {
		t.Errorf("Concerns[0].Severity = %q, want %q", got.Concerns[0].Severity, planreview.SeverityMedium)
	}
	if got.Concerns[0].Category != "scope" {
		t.Errorf("Concerns[0].Category = %q, want %q", got.Concerns[0].Category, "scope")
	}
}

func TestReviewVerdict_JSONRoundTrip_Reject(t *testing.T) {
	v := planreview.ReviewVerdict{
		Verdict: planreview.VerdictReject,
		Concerns: []planreview.Concern{
			{Severity: planreview.SeverityHigh, Category: "correctness", Note: "approach will break the auth flow"},
			{Severity: planreview.SeverityLow, Category: "style", Note: "minor naming inconsistency"},
		},
		FreeForm: "reject: fundamental approach is wrong",
	}
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var got planreview.ReviewVerdict
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if got.Verdict != planreview.VerdictReject {
		t.Errorf("Verdict = %q, want %q", got.Verdict, planreview.VerdictReject)
	}
	if len(got.Concerns) != 2 {
		t.Fatalf("Concerns len = %d, want 2", len(got.Concerns))
	}
	if got.Concerns[0].Severity != planreview.SeverityHigh {
		t.Errorf("Concerns[0].Severity = %q, want %q", got.Concerns[0].Severity, planreview.SeverityHigh)
	}
	if got.FreeForm != v.FreeForm {
		t.Errorf("FreeForm = %q, want %q", got.FreeForm, v.FreeForm)
	}
}

// --- PlanReviewedPayload JSON round-trip ---

func TestPlanReviewedPayload_JSONRoundTrip(t *testing.T) {
	p := planreview.PlanReviewedPayload{
		ReviewerKind:  "agent",
		ReviewerModel: "claude-opus-4-7",
		Authority:     planreview.AuthorityGating,
		Verdict:       planreview.VerdictApprove,
		Concerns:      nil,
		FreeForm:      "plan is sound",
	}
	b, err := json.Marshal(p)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var got planreview.PlanReviewedPayload
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if got.ReviewerKind != p.ReviewerKind {
		t.Errorf("ReviewerKind = %q, want %q", got.ReviewerKind, p.ReviewerKind)
	}
	if got.ReviewerModel != p.ReviewerModel {
		t.Errorf("ReviewerModel = %q, want %q", got.ReviewerModel, p.ReviewerModel)
	}
	if got.Authority != p.Authority {
		t.Errorf("Authority = %q, want %q", got.Authority, p.Authority)
	}
	if got.Verdict != p.Verdict {
		t.Errorf("Verdict = %q, want %q", got.Verdict, p.Verdict)
	}
}
