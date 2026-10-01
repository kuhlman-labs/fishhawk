package precedent

import (
	"errors"
	"testing"
	"time"

	"github.com/kuhlman-labs/fishhawk/backend/internal/decisionindex"
)

// divergence_test.go pins the pure E75.5 / #3733 rule. Each threshold test's
// fixture leaves EXACTLY ONE condition unmet, so deleting that condition's
// check flips the verdict (the masking-guard rule): nothing downstream can
// stand in for it.

var dvNow = time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)

const dvDoctrine = "sha-current"

func dvConfig() DivergenceConfig {
	return DivergenceConfig{Enabled: true, MinDecisions: 5, MinAgreement: 0.8, Window: 90 * 24 * time.Hour}
}

// dvItems builds n human items with outcome, decided one day apart ending a day
// before dvNow, under dvDoctrine.
func dvItems(n int, outcome string) []Item {
	out := make([]Item, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, Item{
			SourceSequence:  int64(100 + i),
			SourceEntryHash: "h",
			DecisionClass:   string(decisionindex.ClassConcernWaive),
			Outcome:         outcome,
			DoctrineVersion: dvDoctrine,
			DecidedAt:       dvNow.Add(-time.Duration(i+1) * 24 * time.Hour),
		})
	}
	return out
}

func dvDecide(cfg DivergenceConfig, items []Item) Verdict {
	return Decide(cfg, decisionindex.ClassConcernDefer, "deferred", dvDoctrine, dvNow, items)
}

func TestDecide_DivergedOnClearContraryPrecedent(t *testing.T) {
	v := dvDecide(dvConfig(), dvItems(6, "waived"))
	if v.Kind != VerdictDiverged {
		t.Fatalf("kind = %q, want diverged", v.Kind)
	}
	if v.ModalOutcome != "waived" || v.AgreementRatio != 1 || v.Human != 6 || len(v.Cited) != 6 {
		t.Fatalf("verdict = %+v, want modal waived, ratio 1, 6 human, 6 cited", v)
	}
}

func TestDecide_AgreedWithPrecedent(t *testing.T) {
	v := Decide(dvConfig(), decisionindex.ClassConcernWaive, "waived", dvDoctrine, dvNow, dvItems(6, "waived"))
	if v.Kind != VerdictAgreed {
		t.Fatalf("kind = %q, want agreed_with_precedent", v.Kind)
	}
}

// (a) The fixture genuinely diverges; Enabled is the ONLY difference from the
// firing configuration.
func TestDecide_DisabledConfigNeverDiverges(t *testing.T) {
	cfg := dvConfig()
	cfg.Enabled = false
	if v := dvDecide(cfg, dvItems(6, "waived")); v.Kind != VerdictDisabled {
		t.Fatalf("kind = %q, want disabled", v.Kind)
	}
	if v := dvDecide(DefaultDivergenceConfig(), dvItems(50, "waived")); v.Kind != VerdictDisabled {
		t.Fatalf("DefaultDivergenceConfig kind = %q, want disabled (the feature ships off)", v.Kind)
	}
}

// (b) MinDecisions-1 human items plus 2 delegated ones: only the delegated
// rows can carry the count over N.
func TestDecide_DelegatedDoNotCountTowardN(t *testing.T) {
	cfg := dvConfig()
	items := dvItems(cfg.MinDecisions+1, "waived")
	items[0].Delegated = true
	items[1].Delegated = true
	v := dvDecide(cfg, items)
	if v.Kind != VerdictNoClearPrecedent || v.Reason != ReasonBelowMinDecisions {
		t.Fatalf("verdict = %q/%q, want no_clear_precedent/below_min_decisions", v.Kind, v.Reason)
	}
}

// (c) N-1 unanimous, in-window, same-doctrine items: the count is the only
// unmet condition.
func TestDecide_BelowMinDecisions(t *testing.T) {
	cfg := dvConfig()
	v := dvDecide(cfg, dvItems(cfg.MinDecisions-1, "waived"))
	if v.Kind != VerdictNoClearPrecedent || v.Reason != ReasonBelowMinDecisions {
		t.Fatalf("verdict = %q/%q, want no_clear_precedent/below_min_decisions", v.Kind, v.Reason)
	}
}

// (d) 10 in-window same-doctrine items split 6/4: the 0.6 ratio is the only
// unmet condition. This also pins the modal-share definition of
// Summary.AgreementRatio the rule consumes.
func TestDecide_BelowMinAgreement(t *testing.T) {
	items := dvItems(10, "waived")
	for i := 6; i < 10; i++ {
		items[i].Outcome = "deferred"
	}
	v := dvDecide(dvConfig(), items)
	if v.Kind != VerdictNoClearPrecedent || v.Reason != ReasonBelowMinAgreement {
		t.Fatalf("verdict = %q/%q, want no_clear_precedent/below_min_agreement", v.Kind, v.Reason)
	}
	if v.AgreementRatio != 0.6 {
		t.Fatalf("agreement ratio = %v, want 0.6 (modal share)", v.AgreementRatio)
	}
}

// (e) 8 unanimous same-doctrine items, all one day OUTSIDE the window.
func TestDecide_OutsideRecencyWindow(t *testing.T) {
	cfg := dvConfig()
	items := dvItems(8, "waived")
	for i := range items {
		items[i].DecidedAt = dvNow.Add(-cfg.Window - 24*time.Hour)
	}
	v := dvDecide(cfg, items)
	if v.Kind != VerdictNoClearPrecedent || v.Reason != ReasonOutsideWindow {
		t.Fatalf("verdict = %q/%q, want no_clear_precedent/outside_window", v.Kind, v.Reason)
	}
}

// The window's lower edge is inclusive: an item exactly at now-Window counts.
func TestDecide_WindowEdgeIsInclusive(t *testing.T) {
	cfg := dvConfig()
	items := dvItems(6, "waived")
	for i := range items {
		items[i].DecidedAt = dvNow.Add(-cfg.Window)
	}
	if v := dvDecide(cfg, items); v.Kind != VerdictDiverged {
		t.Fatalf("kind = %q, want diverged at the inclusive window edge", v.Kind)
	}
}

// (f) 8 unanimous in-window items under a DIFFERENT doctrine version.
func TestDecide_DoctrineVersionMismatch(t *testing.T) {
	items := dvItems(8, "waived")
	for i := range items {
		items[i].DoctrineVersion = "sha-older"
	}
	v := dvDecide(dvConfig(), items)
	if v.Kind != VerdictNoClearPrecedent || v.Reason != ReasonDoctrineVersionMismatch {
		t.Fatalf("verdict = %q/%q, want no_clear_precedent/doctrine_version_mismatch", v.Kind, v.Reason)
	}
}

// (g) An otherwise-diverging merge_verdict fixture.
func TestDecide_ClassNotAllowed(t *testing.T) {
	items := dvItems(6, "approve")
	v := Decide(dvConfig(), decisionindex.ClassMergeVerdict, "reject", dvDoctrine, dvNow, items)
	if v.Kind != VerdictClassNotAllowed {
		t.Fatalf("kind = %q, want class_not_allowed", v.Kind)
	}
}

// plan_approval is allow-listed ONLY on a reject: an approve against a
// reject-modal set never fires, a reject against an approve-modal set does.
func TestDecide_PlanApprovalOnlyOnReject(t *testing.T) {
	approves := dvItems(6, "approve")
	if v := Decide(dvConfig(), decisionindex.ClassPlanApproval, "reject", dvDoctrine, dvNow, approves); v.Kind != VerdictDiverged {
		t.Fatalf("reject against approve precedent: kind = %q, want diverged", v.Kind)
	}
	rejects := dvItems(6, "reject")
	if v := Decide(dvConfig(), decisionindex.ClassPlanApproval, "approve", dvDoctrine, dvNow, rejects); v.Kind != VerdictClassNotAllowed {
		t.Fatalf("approve against reject precedent: kind = %q, want class_not_allowed", v.Kind)
	}
}

// Config may narrow the allow-list.
func TestDecide_ConfigNarrowsAllowList(t *testing.T) {
	cfg := dvConfig()
	cfg.AllowedClasses = []string{string(decisionindex.ClassConcernWaive)}
	if v := dvDecide(cfg, dvItems(6, "waived")); v.Kind != VerdictClassNotAllowed {
		t.Fatalf("defer under a waive-only narrowing: kind = %q, want class_not_allowed", v.Kind)
	}
}

// (h) Error IDENTITY, not any error.
func TestNewDivergenceConfig_RefusesUnknownClass(t *testing.T) {
	_, err := NewDivergenceConfig(true, 5, 0.8, time.Hour, []string{"concern_waive", "merge_verdict"})
	if !errors.Is(err, ErrUnknownDivergenceClass) {
		t.Fatalf("err = %v, want ErrUnknownDivergenceClass", err)
	}
}

func TestNewDivergenceConfig_RefusesOutOfRangeThresholds(t *testing.T) {
	for name, tc := range map[string]struct {
		n int
		x float64
		w time.Duration
	}{
		"zero min decisions": {0, 0.8, time.Hour},
		"zero agreement":     {5, 0, time.Hour},
		"agreement above 1":  {5, 1.01, time.Hour},
		"zero window":        {5, 0.8, 0},
	} {
		if _, err := NewDivergenceConfig(true, tc.n, tc.x, tc.w, nil); !errors.Is(err, ErrInvalidDivergenceConfig) {
			t.Errorf("%s: err = %v, want ErrInvalidDivergenceConfig", name, err)
		}
	}
}

func TestNewDivergenceConfig_NormalizesClasses(t *testing.T) {
	cfg, err := NewDivergenceConfig(false, 5, 0.8, time.Hour, []string{" plan_approval", "concern_waive", "concern_waive", ""})
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	if len(cfg.AllowedClasses) != 2 || cfg.AllowedClasses[0] != "concern_waive" || cfg.AllowedClasses[1] != "plan_approval" {
		t.Fatalf("classes = %v, want [concern_waive plan_approval]", cfg.AllowedClasses)
	}
	if cfg.Enabled {
		t.Fatal("enabled = true, want false")
	}
}

func TestParseWindow(t *testing.T) {
	for in, want := range map[string]time.Duration{"180d": 180 * 24 * time.Hour, "36h": 36 * time.Hour} {
		got, err := ParseWindow(in)
		if err != nil || got != want {
			t.Errorf("ParseWindow(%q) = %v, %v; want %v", in, got, err, want)
		}
	}
	for _, in := range []string{"", "0d", "-3d", "xd", "soon", "-1h", "0s"} {
		if _, err := ParseWindow(in); !errors.Is(err, ErrInvalidDivergenceConfig) {
			t.Errorf("ParseWindow(%q) err = %v, want ErrInvalidDivergenceConfig", in, err)
		}
	}
}

func TestComparisonClasses(t *testing.T) {
	if got := ComparisonClasses(decisionindex.ClassConcernWaive); len(got) != 2 {
		t.Fatalf("concern_waive family = %v, want waive+defer", got)
	}
	if got := ComparisonClasses(decisionindex.ClassPlanApproval); len(got) != 1 || got[0] != decisionindex.ClassPlanApproval {
		t.Fatalf("plan_approval family = %v", got)
	}
	if got := ComparisonClasses(decisionindex.ClassMergeVerdict); got != nil {
		t.Fatalf("merge_verdict family = %v, want nil", got)
	}
	if got := DivergenceClasses(); len(got) != 3 {
		t.Fatalf("closed allow-list = %v, want 3 classes", got)
	}
}
