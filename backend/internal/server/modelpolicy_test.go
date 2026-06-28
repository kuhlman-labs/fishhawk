package server

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
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

// fixupTriggerAuditFake returns canned stage_fixup_triggered entries (or an
// error) so fixupResolvedModelFromAudit's branches are unit-testable.
type fixupTriggerAuditFake struct {
	audit.BaseFake
	entries []*audit.Entry
	err     error
}

func (f *fixupTriggerAuditFake) ListForRunByCategory(_ context.Context, _ uuid.UUID, category string) ([]*audit.Entry, error) {
	if f.err != nil {
		return nil, f.err
	}
	if category != CategoryStageFixupTriggered {
		return nil, nil
	}
	return f.entries, nil
}

// stageEntry builds a stage_fixup_triggered entry for a given stage with a raw
// JSON payload, so the StageID filter in fixupResolvedModelFromAudit applies.
func stageEntry(stageID uuid.UUID, payload string) *audit.Entry {
	sid := stageID
	return &audit.Entry{StageID: &sid, Payload: []byte(payload)}
}

// TestResolveFixupImplementModel covers the #1164 fix-up model resolution:
// a non-empty operator override wins as the operator rung; an empty override
// inherits the run's resolved implement model (here the deployment default,
// and the empty-ladder none).
func TestResolveFixupImplementModel(t *testing.T) {
	ctx := context.Background()

	t.Run("operator override wins", func(t *testing.T) {
		s := New(Config{})
		rm := s.resolveFixupImplementModel(ctx, &run.Run{}, "  claude-haiku-4-5-20251001 ")
		if rm.Value != "claude-haiku-4-5-20251001" || rm.Source != ModelSourceOperator {
			t.Fatalf("got {%q,%q}, want {claude-haiku-4-5-20251001,operator}", rm.Value, rm.Source)
		}
	})
	t.Run("empty override inherits deployment default", func(t *testing.T) {
		s := New(Config{ImplementModelDefault: "claude-opus-4-8"})
		rm := s.resolveFixupImplementModel(ctx, &run.Run{}, "")
		if rm.Value != "claude-opus-4-8" || rm.Source != ModelSourceDefault {
			t.Fatalf("got {%q,%q}, want {claude-opus-4-8,default}", rm.Value, rm.Source)
		}
	})
	t.Run("empty override and empty ladder yields none", func(t *testing.T) {
		s := New(Config{})
		rm := s.resolveFixupImplementModel(ctx, &run.Run{}, "   ")
		if rm.Value != "" || rm.Source != ModelSourceNone {
			t.Fatalf("got {%q,%q}, want {\"\",none}", rm.Value, rm.Source)
		}
	})
}

// TestFixupResolvedModelFromAudit covers the #1164 read-back, including binding
// condition 1: a PRESENT-but-empty fixup_model returns ok=true (Value=""), an
// ABSENT key (pre-#1164 entry) returns ok=false, and the defensive branches
// (nil repo, list error, no entry, malformed) all return ok=false.
func TestFixupResolvedModelFromAudit(t *testing.T) {
	ctx := context.Background()
	runID := uuid.New()
	stageID := uuid.New()

	t.Run("nil AuditRepo returns not-ok", func(t *testing.T) {
		s := New(Config{})
		if _, ok := s.fixupResolvedModelFromAudit(ctx, runID, stageID); ok {
			t.Fatal("expected not-ok with a nil AuditRepo")
		}
	})
	t.Run("list error returns not-ok", func(t *testing.T) {
		s := New(Config{AuditRepo: &fixupTriggerAuditFake{err: errors.New("boom")}})
		if _, ok := s.fixupResolvedModelFromAudit(ctx, runID, stageID); ok {
			t.Fatal("expected not-ok on a list error")
		}
	})
	t.Run("no entry for stage returns not-ok", func(t *testing.T) {
		s := New(Config{AuditRepo: &fixupTriggerAuditFake{entries: []*audit.Entry{
			stageEntry(uuid.New(), `{"fixup_model":"x","fixup_model_source":"operator"}`),
		}}})
		if _, ok := s.fixupResolvedModelFromAudit(ctx, runID, stageID); ok {
			t.Fatal("expected not-ok when no entry matches the stage")
		}
	})
	t.Run("malformed payload returns not-ok", func(t *testing.T) {
		s := New(Config{AuditRepo: &fixupTriggerAuditFake{entries: []*audit.Entry{
			stageEntry(stageID, "{not json"),
		}}})
		if _, ok := s.fixupResolvedModelFromAudit(ctx, runID, stageID); ok {
			t.Fatal("expected not-ok on a malformed payload")
		}
	})
	t.Run("pre-#1164 entry (no fixup_model key) returns not-ok", func(t *testing.T) {
		s := New(Config{AuditRepo: &fixupTriggerAuditFake{entries: []*audit.Entry{
			stageEntry(stageID, `{"pass_ordinal":1}`),
		}}})
		if _, ok := s.fixupResolvedModelFromAudit(ctx, runID, stageID); ok {
			t.Fatal("expected not-ok (fall through to live resolution) on a pre-#1164 entry")
		}
	})
	t.Run("present non-empty pin returns the source-tagged model", func(t *testing.T) {
		s := New(Config{AuditRepo: &fixupTriggerAuditFake{entries: []*audit.Entry{
			stageEntry(stageID, `{"fixup_model":"claude-haiku-4-5-20251001","fixup_model_source":"operator"}`),
		}}})
		rm, ok := s.fixupResolvedModelFromAudit(ctx, runID, stageID)
		if !ok || rm.Value != "claude-haiku-4-5-20251001" || rm.Source != ModelSourceOperator {
			t.Fatalf("got {%q,%q} ok=%v, want {claude-haiku-4-5-20251001,operator} ok=true", rm.Value, rm.Source, ok)
		}
	})
	t.Run("present-but-empty pin returns ok with empty value", func(t *testing.T) {
		s := New(Config{AuditRepo: &fixupTriggerAuditFake{entries: []*audit.Entry{
			stageEntry(stageID, `{"fixup_model":"","fixup_model_source":""}`),
		}}})
		rm, ok := s.fixupResolvedModelFromAudit(ctx, runID, stageID)
		if !ok || rm.Value != "" || rm.Source != ModelSourceNone {
			t.Fatalf("got {%q,%q} ok=%v, want {\"\",none} ok=true (present-but-empty pin honored)", rm.Value, rm.Source, ok)
		}
	})
	t.Run("newest entry for stage wins", func(t *testing.T) {
		s := New(Config{AuditRepo: &fixupTriggerAuditFake{entries: []*audit.Entry{
			stageEntry(stageID, `{"fixup_model":"old-model","fixup_model_source":"operator"}`),
			stageEntry(uuid.New(), `{"fixup_model":"other-stage","fixup_model_source":"operator"}`),
			stageEntry(stageID, `{"fixup_model":"new-model","fixup_model_source":"operator"}`),
		}}})
		rm, ok := s.fixupResolvedModelFromAudit(ctx, runID, stageID)
		if !ok || rm.Value != "new-model" {
			t.Fatalf("got {%q} ok=%v, want the newest stage entry {new-model}", rm.Value, ok)
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

func TestGateResolveImplementModel_OperatorPrecedence(t *testing.T) {
	specWithModel := []byte(`
workflows:
  feature_change:
    stages:
      - id: implement
        type: implement
        executor:
          agent: claude-code
          model: spec-model
`)
	tests := []struct {
		name       string
		deflt      string
		spec       []byte
		operator   string
		wantValue  string
		wantSource ModelSource
	}{
		{
			name: "operator override wins over spec",
			spec: specWithModel, operator: "operator-model",
			wantValue: "operator-model", wantSource: ModelSourceOperator,
		},
		{
			name: "spec wins when no operator",
			spec: specWithModel, operator: "",
			wantValue: "spec-model", wantSource: ModelSourceSpec,
		},
		{
			name:  "deployment default wins when only rung",
			deflt: "default-model", spec: nil, operator: "",
			wantValue: "default-model", wantSource: ModelSourceDefault,
		},
		{
			name:  "operator wins over default with no spec",
			deflt: "default-model", spec: nil, operator: "  operator-model  ",
			wantValue: "operator-model", wantSource: ModelSourceOperator,
		},
		{
			name:  "all empty yields none (today's spawn)",
			deflt: "", spec: nil, operator: "",
			wantValue: "", wantSource: ModelSourceNone,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := New(Config{ImplementModelDefault: tt.deflt})
			runRow := &run.Run{WorkflowID: "feature_change", WorkflowSpec: tt.spec}
			// ArtifactRepo is nil → planImplementModelRecommendation yields "".
			got := s.gateResolveImplementModel(context.Background(), runRow, tt.operator)
			if got.Value != tt.wantValue || got.Source != tt.wantSource {
				t.Fatalf("gateResolveImplementModel = {%q,%q}, want {%q,%q}",
					got.Value, got.Source, tt.wantValue, tt.wantSource)
			}
		})
	}
}

func TestAdapterForImplementAgent(t *testing.T) {
	tests := []struct {
		agent string
		want  string
	}{
		{"", "claudecode"},
		{"claude-code", "claudecode"},
		{"claudecode", "claudecode"},
		{"  claude-code  ", "claudecode"},
		{"codex", "codex"},
		{"anthropic", "anthropic"},
		{"future-agent", "future-agent"},
	}
	for _, tt := range tests {
		t.Run(tt.agent, func(t *testing.T) {
			if got := adapterForImplementAgent(tt.agent); got != tt.want {
				t.Fatalf("adapterForImplementAgent(%q) = %q, want %q", tt.agent, got, tt.want)
			}
		})
	}
}

func TestSpecImplementExecutorAgent(t *testing.T) {
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
          agent: codex
`)
	specByType := []byte(`
workflows:
  feature_change:
    stages:
      - id: impl-stage
        type: implement
        executor:
          agent: claude-code
`)
	specNoAgent := []byte(`
workflows:
  feature_change:
    stages:
      - id: implement
        type: implement
        executor:
          model: x
`)
	tests := []struct {
		name       string
		spec       []byte
		workflowID string
		want       string
	}{
		{"matched by stage id", specByID, "feature_change", "codex"},
		{"matched by stage type when id differs", specByType, "feature_change", "claude-code"},
		{"implement stage declares no agent", specNoAgent, "feature_change", ""},
		{"unknown workflow id", specByID, "nonexistent", ""},
		{"empty spec bytes", nil, "feature_change", ""},
		{"malformed yaml degrades to empty", []byte("\t\tnot: [valid"), "feature_change", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := specImplementExecutorAgent(tt.spec, tt.workflowID); got != tt.want {
				t.Fatalf("specImplementExecutorAgent() = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestResolvePlanModel_LadderPrecedence covers the plan-model ladder (#1416):
// operator > spec > default, with an all-empty ladder yielding ModelSourceNone
// (today's spawn). Unlike the implement ladder it has no plan-recommendation
// rung — the plan agent is spawned before any plan artifact exists.
func TestResolvePlanModel_LadderPrecedence(t *testing.T) {
	tests := []struct {
		name                  string
		deflt, spec, operator string
		wantValue             string
		wantSource            ModelSource
	}{
		{
			name:  "operator wins over all lower rungs",
			deflt: "d", spec: "s", operator: "o",
			wantValue: "o", wantSource: ModelSourceOperator,
		},
		{
			name:  "spec wins when no operator",
			deflt: "d", spec: "s", operator: "",
			wantValue: "s", wantSource: ModelSourceSpec,
		},
		{
			name:  "default wins when it is the only rung",
			deflt: "d", spec: "", operator: "",
			wantValue: "d", wantSource: ModelSourceDefault,
		},
		{
			name:  "all empty yields none (today's spawn)",
			deflt: "", spec: "", operator: "",
			wantValue: "", wantSource: ModelSourceNone,
		},
		{
			name:  "higher empty rung skipped, lower non-empty wins",
			deflt: "d", spec: "", operator: "",
			wantValue: "d", wantSource: ModelSourceDefault,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := resolvePlanModel(tt.deflt, tt.spec, tt.operator)
			if got.Value != tt.wantValue || got.Source != tt.wantSource {
				t.Fatalf("resolvePlanModel(%q,%q,%q) = {%q,%q}, want {%q,%q}",
					tt.deflt, tt.spec, tt.operator,
					got.Value, got.Source, tt.wantValue, tt.wantSource)
			}
		})
	}
}

// TestResolvePlanModelForRun covers the run-level plan-model resolution (#1416):
// the spec executor.model (plan stage) is honored (Scenario B), and a spec with
// no plan executor.model resolves to ModelSourceNone (byte-identical default
// spawn). The deployment-default and operator rungs are not wired in this slice,
// so resolution is spec-only.
func TestResolvePlanModelForRun(t *testing.T) {
	s := New(Config{})
	ctx := context.Background()

	t.Run("spec pin honored", func(t *testing.T) {
		runRow := &run.Run{
			WorkflowID: "feature_change",
			WorkflowSpec: []byte("workflows:\n" +
				"  feature_change:\n" +
				"    stages:\n" +
				"      - id: plan\n" +
				"        type: plan\n" +
				"        executor:\n" +
				"          agent: claudecode\n" +
				"          model: claude-opus-4-8\n"),
		}
		rm := s.resolvePlanModelForRun(ctx, runRow)
		if rm.Value != "claude-opus-4-8" || rm.Source != ModelSourceSpec {
			t.Fatalf("resolvePlanModelForRun = {%q,%q}, want {claude-opus-4-8, spec}", rm.Value, rm.Source)
		}
	})

	t.Run("no plan executor.model yields none", func(t *testing.T) {
		runRow := &run.Run{
			WorkflowID: "feature_change",
			WorkflowSpec: []byte("workflows:\n" +
				"  feature_change:\n" +
				"    stages:\n" +
				"      - id: plan\n" +
				"        type: plan\n" +
				"        executor:\n" +
				"          agent: claudecode\n"),
		}
		rm := s.resolvePlanModelForRun(ctx, runRow)
		if rm.Value != "" || rm.Source != ModelSourceNone {
			t.Fatalf("resolvePlanModelForRun = {%q,%q}, want {empty, none}", rm.Value, rm.Source)
		}
	})
}

// TestSpecPlanExecutorModel mirrors TestSpecImplementExecutorModel for the PLAN
// stage probe (#1416): prefer a stage whose id == "plan", else the first stage
// whose type == "plan"; every malformed/absent path degrades to "".
func TestSpecPlanExecutorModel(t *testing.T) {
	specByID := []byte(`
workflows:
  feature_change:
    stages:
      - id: plan
        type: plan
        executor:
          agent: claudecode
          model: claude-opus-4-8
      - id: implement
        type: implement
        executor:
          model: gpt-5.5
`)
	specByType := []byte(`
workflows:
  feature_change:
    stages:
      - id: plan-stage
        type: plan
        executor:
          model: claude-sonnet-4-6
`)
	specNoModel := []byte(`
workflows:
  feature_change:
    stages:
      - id: plan
        type: plan
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
		{"matched by stage type when id differs", specByType, "feature_change", "claude-sonnet-4-6"},
		{"plan stage declares no model", specNoModel, "feature_change", ""},
		{"unknown workflow id", specByID, "nonexistent", ""},
		{"empty spec bytes", nil, "feature_change", ""},
		{"malformed yaml degrades to empty", []byte("\t\tnot: [valid"), "feature_change", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := specPlanExecutorModel(tt.spec, tt.workflowID); got != tt.want {
				t.Fatalf("specPlanExecutorModel() = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestResolveReviewModel_LadderPrecedence covers the review-model ladder (#1416),
// which mirrors the plan ladder: operator > spec > default > none.
func TestResolveReviewModel_LadderPrecedence(t *testing.T) {
	tests := []struct {
		name                  string
		deflt, spec, operator string
		wantValue             string
		wantSource            ModelSource
	}{
		{"operator wins over all lower rungs", "d", "s", "o", "o", ModelSourceOperator},
		{"spec wins when no operator", "d", "s", "", "s", ModelSourceSpec},
		{"default wins when it is the only rung", "d", "", "", "d", ModelSourceDefault},
		{"all empty yields none (today's reviewer spawn)", "", "", "", "", ModelSourceNone},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := resolveReviewModel(tt.deflt, tt.spec, tt.operator)
			if got.Value != tt.wantValue || got.Source != tt.wantSource {
				t.Fatalf("resolveReviewModel(%q,%q,%q) = {%q,%q}, want {%q,%q}",
					tt.deflt, tt.spec, tt.operator, got.Value, got.Source, tt.wantValue, tt.wantSource)
			}
		})
	}
}

// TestSpecReviewExecutorModel mirrors TestSpecPlanExecutorModel for the REVIEW
// stage probe (#1416): prefer a stage whose id == "review", else the first stage
// whose type == "review"; every malformed/absent path degrades to "".
func TestSpecReviewExecutorModel(t *testing.T) {
	specByID := []byte(`
workflows:
  feature_change:
    stages:
      - id: review
        type: review
        executor:
          model: gpt-5.5
      - id: implement
        type: implement
        executor:
          model: claude-opus-4-8
`)
	specByType := []byte(`
workflows:
  feature_change:
    stages:
      - id: review-stage
        type: review
        executor:
          model: claude-sonnet-4-6
`)
	specNoModel := []byte(`
workflows:
  feature_change:
    stages:
      - id: review
        type: review
        executor:
          agent: codex
`)
	tests := []struct {
		name       string
		spec       []byte
		workflowID string
		want       string
	}{
		{"matched by stage id", specByID, "feature_change", "gpt-5.5"},
		{"matched by stage type when id differs", specByType, "feature_change", "claude-sonnet-4-6"},
		{"review stage declares no model", specNoModel, "feature_change", ""},
		{"unknown workflow id", specByID, "nonexistent", ""},
		{"empty spec bytes", nil, "feature_change", ""},
		{"malformed yaml degrades to empty", []byte("\t\tnot: [valid"), "feature_change", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := specReviewExecutorModel(tt.spec, tt.workflowID); got != tt.want {
				t.Fatalf("specReviewExecutorModel() = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestModelResolvedStageMatches covers the legacy-compat discriminator (#1416):
// an exact stage_type match always wins; a legacy entry with no stage_type ("")
// is treated as the implement resolution and NOTHING else.
func TestModelResolvedStageMatches(t *testing.T) {
	tests := []struct {
		have, want string
		match      bool
	}{
		{"implement", "implement", true},
		{"plan", "plan", true},
		{"review", "review", true},
		{"", "implement", true}, // legacy entry → implement
		{"", "plan", false},     // legacy entry never matches plan
		{"", "review", false},   // legacy entry never matches review
		{"plan", "implement", false},
		{"review", "implement", false},
		{"implement", "review", false},
	}
	for _, tt := range tests {
		if got := modelResolvedStageMatches(tt.have, tt.want); got != tt.match {
			t.Fatalf("modelResolvedStageMatches(%q,%q) = %v, want %v", tt.have, tt.want, got, tt.match)
		}
	}
}

// TestGateResolvedModelForStage covers the per-stage filtering (#1416): with one
// model_resolved entry per stage stamped at the gate, each reader resolves ONLY
// its own stage's entry regardless of which was written last, and the implement
// reader still matches a legacy untyped entry.
func TestGateResolvedModelForStage(t *testing.T) {
	ctx := context.Background()
	runID := uuid.New()

	t.Run("each stage reads its own entry", func(t *testing.T) {
		s := New(Config{AuditRepo: &gateAuditFake{entries: []*audit.Entry{
			entry(1, `{"model":"impl-m","model_source":"operator","stage_type":"implement"}`),
			entry(2, `{"model":"plan-m","model_source":"operator","stage_type":"plan"}`),
			entry(3, `{"model":"rev-m","model_source":"operator","stage_type":"review"}`),
		}}})
		impl, ok := s.gateResolvedModelForStage(ctx, runID, "implement")
		if !ok || impl.Value != "impl-m" {
			t.Fatalf("implement = {%q} ok=%v, want impl-m", impl.Value, ok)
		}
		// gateResolvedModel must still route the implement entry, NOT the
		// highest-sequence review entry.
		gm, ok := s.gateResolvedModel(ctx, runID)
		if !ok || gm.Value != "impl-m" {
			t.Fatalf("gateResolvedModel = {%q} ok=%v, want impl-m (not the newest review entry)", gm.Value, ok)
		}
		pl, ok := s.gateResolvedModelForStage(ctx, runID, "plan")
		if !ok || pl.Value != "plan-m" {
			t.Fatalf("plan = {%q} ok=%v, want plan-m", pl.Value, ok)
		}
		if rev := s.gateResolvedReviewModel(ctx, runID); rev != "rev-m" {
			t.Fatalf("gateResolvedReviewModel = %q, want rev-m", rev)
		}
	})

	t.Run("legacy untyped entry resolves as implement only", func(t *testing.T) {
		s := New(Config{AuditRepo: &gateAuditFake{entries: []*audit.Entry{
			entry(1, `{"model":"legacy-m","model_source":"plan"}`),
		}}})
		impl, ok := s.gateResolvedModelForStage(ctx, runID, "implement")
		if !ok || impl.Value != "legacy-m" {
			t.Fatalf("implement = {%q} ok=%v, want legacy-m", impl.Value, ok)
		}
		if _, ok := s.gateResolvedModelForStage(ctx, runID, "plan"); ok {
			t.Fatal("legacy untyped entry must NOT match the plan stage")
		}
		if rev := s.gateResolvedReviewModel(ctx, runID); rev != "" {
			t.Fatalf("gateResolvedReviewModel = %q, want \"\" (legacy entry is not a review entry)", rev)
		}
	})

	t.Run("no review entry yields empty override (fail-open)", func(t *testing.T) {
		s := New(Config{AuditRepo: &gateAuditFake{entries: []*audit.Entry{
			entry(1, `{"model":"impl-m","model_source":"operator","stage_type":"implement"}`),
		}}})
		if rev := s.gateResolvedReviewModel(ctx, runID); rev != "" {
			t.Fatalf("gateResolvedReviewModel = %q, want \"\"", rev)
		}
	})

	t.Run("recorded empty plan resolution is honored as none/ok", func(t *testing.T) {
		s := New(Config{AuditRepo: &gateAuditFake{entries: []*audit.Entry{
			entry(1, `{"model":"","model_source":"","stage_type":"plan"}`),
		}}})
		rm, ok := s.gateResolvedModelForStage(ctx, runID, "plan")
		if !ok || rm.Value != "" || rm.Source != ModelSourceNone {
			t.Fatalf("plan = {%q,%q} ok=%v, want {\"\",none} ok=true", rm.Value, rm.Source, ok)
		}
	})
}

// TestGateResolvePlanReviewModel_OperatorPrecedence covers the gate-time plan and
// review resolvers (#1416): the operator override wins over the spec rung, and an
// empty operator falls back to the spec executor.model.
func TestGateResolvePlanReviewModel_OperatorPrecedence(t *testing.T) {
	s := New(Config{})
	runRow := &run.Run{
		WorkflowID: "feature_change",
		WorkflowSpec: []byte("workflows:\n" +
			"  feature_change:\n" +
			"    stages:\n" +
			"      - id: plan\n" +
			"        type: plan\n" +
			"        executor:\n" +
			"          model: spec-plan\n" +
			"      - id: review\n" +
			"        type: review\n" +
			"        executor:\n" +
			"          model: spec-review\n"),
	}

	if got := s.gateResolvePlanModel(runRow, "op-plan"); got.Value != "op-plan" || got.Source != ModelSourceOperator {
		t.Fatalf("gateResolvePlanModel(op) = {%q,%q}, want {op-plan,operator}", got.Value, got.Source)
	}
	if got := s.gateResolvePlanModel(runRow, ""); got.Value != "spec-plan" || got.Source != ModelSourceSpec {
		t.Fatalf("gateResolvePlanModel(empty) = {%q,%q}, want {spec-plan,spec}", got.Value, got.Source)
	}
	if got := s.gateResolveReviewModel(runRow, "op-rev"); got.Value != "op-rev" || got.Source != ModelSourceOperator {
		t.Fatalf("gateResolveReviewModel(op) = {%q,%q}, want {op-rev,operator}", got.Value, got.Source)
	}
	if got := s.gateResolveReviewModel(runRow, ""); got.Value != "spec-review" || got.Source != ModelSourceSpec {
		t.Fatalf("gateResolveReviewModel(empty) = {%q,%q}, want {spec-review,spec}", got.Value, got.Source)
	}
	// Combination case (#1416 verification mode 6): review pinned in spec while
	// the operator overrides only the plan model — each resolves independently.
	if pl := s.gateResolvePlanModel(runRow, "op-plan"); pl.Value != "op-plan" {
		t.Fatalf("combination plan = %q, want op-plan", pl.Value)
	}
	if rev := s.gateResolveReviewModel(runRow, ""); rev.Value != "spec-review" {
		t.Fatalf("combination review = %q, want spec-review (spec pin unaffected by plan override)", rev.Value)
	}
}

// TestResolvePlanModelForRun_ReadsGateEntry covers the operator plan-model
// override routing to a re-dispatched plan spawn (#1416): when the gate recorded
// a plan model_resolved entry, resolvePlanModelForRun returns it (winning over
// the spec rung).
func TestResolvePlanModelForRun_ReadsGateEntry(t *testing.T) {
	ctx := context.Background()
	s := New(Config{AuditRepo: &gateAuditFake{entries: []*audit.Entry{
		entry(1, `{"model":"op-plan","model_source":"operator","stage_type":"plan"}`),
	}}})
	runRow := &run.Run{
		WorkflowID: "feature_change",
		WorkflowSpec: []byte("workflows:\n" +
			"  feature_change:\n" +
			"    stages:\n" +
			"      - id: plan\n" +
			"        type: plan\n" +
			"        executor:\n" +
			"          model: spec-plan\n"),
	}
	rm := s.resolvePlanModelForRun(ctx, runRow)
	if rm.Value != "op-plan" || rm.Source != ModelSourceOperator {
		t.Fatalf("resolvePlanModelForRun = {%q,%q}, want {op-plan,operator} (gate entry wins over spec)", rm.Value, rm.Source)
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
