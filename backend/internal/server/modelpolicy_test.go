package server

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
)

// gateAuditFake returns canned model_resolved entries (or an error) so the
// gateResolvedModel defensive branches are unit-testable.
type gateAuditFake struct {
	audit.BaseFake
	entries []*audit.Entry
	err     error
}

func (f *gateAuditFake) ListForRunByCategory(_ context.Context, _ uuid.UUID, category string) ([]*audit.Entry, error) {
	if f.err != nil {
		return nil, f.err
	}
	if category != "model_resolved" {
		return nil, nil
	}
	return f.entries, nil
}

func entry(seq int64, payload string) *audit.Entry {
	return &audit.Entry{Sequence: seq, Payload: []byte(payload)}
}

func TestGateResolvedModel(t *testing.T) {
	ctx := context.Background()
	runID := uuid.New()

	t.Run("nil AuditRepo returns not-ok", func(t *testing.T) {
		s := New(Config{})
		if _, ok := s.gateResolvedModel(ctx, runID); ok {
			t.Fatal("expected not-ok with a nil AuditRepo")
		}
	})
	t.Run("list error returns not-ok", func(t *testing.T) {
		s := New(Config{AuditRepo: &gateAuditFake{err: errors.New("boom")}})
		if _, ok := s.gateResolvedModel(ctx, runID); ok {
			t.Fatal("expected not-ok on a list error")
		}
	})
	t.Run("no entries returns not-ok", func(t *testing.T) {
		s := New(Config{AuditRepo: &gateAuditFake{}})
		if _, ok := s.gateResolvedModel(ctx, runID); ok {
			t.Fatal("expected not-ok with no model_resolved entries")
		}
	})
	t.Run("malformed payload returns not-ok", func(t *testing.T) {
		s := New(Config{AuditRepo: &gateAuditFake{entries: []*audit.Entry{entry(1, "{not json")}}})
		if _, ok := s.gateResolvedModel(ctx, runID); ok {
			t.Fatal("expected not-ok on a malformed payload")
		}
	})
	t.Run("valid entry returns the source-tagged resolution", func(t *testing.T) {
		s := New(Config{AuditRepo: &gateAuditFake{entries: []*audit.Entry{
			entry(1, `{"model":"claude-opus-4-8","model_source":"operator"}`),
		}}})
		rm, ok := s.gateResolvedModel(ctx, runID)
		if !ok || rm.Value != "claude-opus-4-8" || rm.Source != ModelSourceOperator {
			t.Fatalf("got {%q,%q} ok=%v, want {claude-opus-4-8,operator} ok=true", rm.Value, rm.Source, ok)
		}
	})
	t.Run("newest entry by sequence wins", func(t *testing.T) {
		s := New(Config{AuditRepo: &gateAuditFake{entries: []*audit.Entry{
			entry(1, `{"model":"old-model","model_source":"plan"}`),
			entry(5, `{"model":"new-model","model_source":"operator"}`),
			entry(3, `{"model":"mid-model","model_source":"spec"}`),
		}}})
		rm, ok := s.gateResolvedModel(ctx, runID)
		if !ok || rm.Value != "new-model" || rm.Source != ModelSourceOperator {
			t.Fatalf("got {%q,%q}, want the highest-sequence {new-model,operator}", rm.Value, rm.Source)
		}
	})
	t.Run("recorded empty resolution returns none/ok (deliberate default spawn)", func(t *testing.T) {
		s := New(Config{AuditRepo: &gateAuditFake{entries: []*audit.Entry{
			entry(1, `{"model":"","model_source":""}`),
		}}})
		rm, ok := s.gateResolvedModel(ctx, runID)
		if !ok || rm.Value != "" || rm.Source != ModelSourceNone {
			t.Fatalf("got {%q,%q} ok=%v, want {\"\",none} ok=true", rm.Value, rm.Source, ok)
		}
	})
}

// TestResolvedImplementModelForRunID_NilRunRepo covers the by-id stamp helper's
// fail-soft branch: a nil RunRepo yields the empty (none) ResolvedModel rather
// than panicking the best-effort trace handler.
func TestResolvedImplementModelForRunID_NilRunRepo(t *testing.T) {
	s := New(Config{})
	rm := s.resolvedImplementModelForRunID(context.Background(), uuid.New())
	if rm.Value != "" || rm.Source != ModelSourceNone {
		t.Fatalf("got {%q,%q}, want {\"\",none}", rm.Value, rm.Source)
	}
}

func TestResolveImplementModel_LadderPrecedence(t *testing.T) {
	tests := []struct {
		name                        string
		deflt, spec, plan, operator string
		wantValue                   string
		wantSource                  ModelSource
	}{
		{
			name:  "operator wins over all lower rungs",
			deflt: "d", spec: "s", plan: "p", operator: "o",
			wantValue: "o", wantSource: ModelSourceOperator,
		},
		{
			name:  "plan wins when no operator",
			deflt: "d", spec: "s", plan: "p", operator: "",
			wantValue: "p", wantSource: ModelSourcePlan,
		},
		{
			name:  "spec wins when no operator or plan",
			deflt: "d", spec: "s", plan: "", operator: "",
			wantValue: "s", wantSource: ModelSourceSpec,
		},
		{
			name:  "default wins when it is the only rung",
			deflt: "d", spec: "", plan: "", operator: "",
			wantValue: "d", wantSource: ModelSourceDefault,
		},
		{
			name:  "all empty yields none (today's spawn)",
			deflt: "", spec: "", plan: "", operator: "",
			wantValue: "", wantSource: ModelSourceNone,
		},
		{
			name:  "higher empty rung skipped, lower non-empty wins",
			deflt: "d", spec: "", plan: "p", operator: "",
			wantValue: "p", wantSource: ModelSourcePlan,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := resolveImplementModel(tt.deflt, tt.spec, tt.plan, tt.operator)
			if got.Value != tt.wantValue || got.Source != tt.wantSource {
				t.Fatalf("resolveImplementModel(%q,%q,%q,%q) = {%q,%q}, want {%q,%q}",
					tt.deflt, tt.spec, tt.plan, tt.operator,
					got.Value, got.Source, tt.wantValue, tt.wantSource)
			}
		})
	}
}

func TestAllowedModels_IsAllowed(t *testing.T) {
	policy := AllowedModels{
		"claudecode": {"claude-opus-4-8": true, "claude-sonnet-4-6": true},
		"codex":      {"gpt-5.5": true},
		"empty":      {}, // configured but empty set
	}
	tests := []struct {
		name           string
		adapter, model string
		want           bool
	}{
		{"empty model always allowed (today's default spawn)", "claudecode", "", true},
		{"configured model in set is allowed", "claudecode", "claude-opus-4-8", true},
		{"configured model NOT in set is rejected", "claudecode", "gpt-5.5", false},
		{"unknown adapter fails open", "anthropic", "anything", true},
		{"adapter with empty set fails open", "empty", "anything", true},
		{"codex model in set allowed", "codex", "gpt-5.5", true},
		{"codex model not in set rejected", "codex", "gpt-4", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := policy.IsAllowed(tt.adapter, tt.model); got != tt.want {
				t.Fatalf("IsAllowed(%q,%q) = %v, want %v", tt.adapter, tt.model, got, tt.want)
			}
		})
	}
}

func TestAllowedModels_NilPolicyFailsOpen(t *testing.T) {
	var policy AllowedModels // nil
	if !policy.IsAllowed("claudecode", "any-model") {
		t.Fatal("a nil policy must fail open for any model")
	}
	if !policy.IsAllowed("claudecode", "") {
		t.Fatal("a nil policy must allow the empty model")
	}
}

func TestParseAllowedModels(t *testing.T) {
	t.Run("single adapter", func(t *testing.T) {
		got := ParseAllowedModels("claudecode=claude-opus-4-8,claude-sonnet-4-6")
		if !got.IsAllowed("claudecode", "claude-opus-4-8") || !got.IsAllowed("claudecode", "claude-sonnet-4-6") {
			t.Fatalf("expected both models allowed, got %#v", got)
		}
		if got.IsAllowed("claudecode", "gpt-5.5") {
			t.Fatal("a model outside the configured set must be rejected")
		}
	})
	t.Run("multi adapter with whitespace", func(t *testing.T) {
		got := ParseAllowedModels(" claudecode = claude-opus-4-8 ; codex = gpt-5.5 ")
		if !got.IsAllowed("claudecode", "claude-opus-4-8") {
			t.Fatal("trimmed claudecode model should be allowed")
		}
		if !got.IsAllowed("codex", "gpt-5.5") {
			t.Fatal("trimmed codex model should be allowed")
		}
	})
	t.Run("malformed groups skipped, degrade to fail-open", func(t *testing.T) {
		got := ParseAllowedModels(";=novalueadapter;noequalssign;=;claudecode=claude-opus-4-8")
		if len(got) != 1 {
			t.Fatalf("expected only the one well-formed group, got %#v", got)
		}
		if !got.IsAllowed("claudecode", "claude-opus-4-8") {
			t.Fatal("the well-formed group should still parse")
		}
		// A skipped/garbage adapter fails open.
		if !got.IsAllowed("noequalssign", "x") {
			t.Fatal("an unparsed adapter must fail open")
		}
	})
	t.Run("empty input yields empty fail-open policy", func(t *testing.T) {
		got := ParseAllowedModels("")
		if len(got) != 0 {
			t.Fatalf("empty input must yield an empty policy, got %#v", got)
		}
		if !got.IsAllowed("claudecode", "anything") {
			t.Fatal("an empty policy must fail open")
		}
	})
	t.Run("adapter with no models is dropped", func(t *testing.T) {
		got := ParseAllowedModels("claudecode=")
		if len(got) != 0 {
			t.Fatalf("an adapter with no models must be dropped, got %#v", got)
		}
	})
}

func TestPlanModelRecommendationFromBytes(t *testing.T) {
	tests := []struct {
		name    string
		content string
		want    string
	}{
		{
			name:    "present",
			content: `{"model_recommendation":{"implement_model":"claude-opus-4-8","rationale":"hard","complexity_assessed":"high"}}`,
			want:    "claude-opus-4-8",
		},
		{
			name:    "trimmed",
			content: `{"model_recommendation":{"implement_model":"  claude-opus-4-8  "}}`,
			want:    "claude-opus-4-8",
		},
		{
			name:    "object present but field absent",
			content: `{"model_recommendation":{"rationale":"x"}}`,
			want:    "",
		},
		{
			name:    "no model_recommendation",
			content: `{"summary":"x"}`,
			want:    "",
		},
		{
			name:    "malformed json degrades to empty",
			content: `{not json`,
			want:    "",
		},
		{
			name:    "empty bytes",
			content: ``,
			want:    "",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := planModelRecommendationFromBytes([]byte(tt.content)); got != tt.want {
				t.Fatalf("planModelRecommendationFromBytes() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestSpecImplementExecutorModel(t *testing.T) {
	specByID := []byte(`
workflows:
  feature_change:
    stages:
      - id: plan
        type: plan
        executor:
          agent: planner
      - id: implement
        type: implement
        executor:
          agent: claudecode
          model: claude-opus-4-8
`)
	specByType := []byte(`
workflows:
  feature_change:
    stages:
      - id: impl-stage
        type: implement
        executor:
          model: gpt-5.5
`)
	specNoModel := []byte(`
workflows:
  feature_change:
    stages:
      - id: implement
        type: implement
        executor:
          agent: claudecode
`)
	tests := []struct {
		name       string
		spec       []byte
		workflowID string
		want       string
	}{
		{"matched by stage id", specByID, "feature_change", "claude-opus-4-8"},
		{"matched by stage type when id differs", specByType, "feature_change", "gpt-5.5"},
		{"implement stage declares no model", specNoModel, "feature_change", ""},
		{"unknown workflow id", specByID, "nonexistent", ""},
		{"empty spec bytes", nil, "feature_change", ""},
		{"malformed yaml degrades to empty", []byte("\t\tnot: [valid"), "feature_change", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := specImplementExecutorModel(tt.spec, tt.workflowID); got != tt.want {
				t.Fatalf("specImplementExecutorModel() = %q, want %q", got, tt.want)
			}
		})
	}
}
