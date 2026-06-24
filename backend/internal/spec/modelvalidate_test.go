package spec_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/kuhlman-labs/fishhawk/backend/internal/modeloracle"
	"github.com/kuhlman-labs/fishhawk/backend/internal/spec"
)

// specWith builds a one-workflow spec whose single implement stage carries the
// given executor agent+model and (optionally) one reviewer.
func specWith(agent, execModel string, reviewers *spec.ReviewersConfig) *spec.Spec {
	return &spec.Spec{
		Version: "0.3",
		Workflows: map[string]spec.Workflow{
			"wf": {
				Stages: []spec.Stage{
					{
						ID:        "implement",
						Type:      spec.StageTypeImplement,
						Executor:  spec.Executor{Agent: agent, Model: execModel},
						Reviewers: reviewers,
					},
				},
			},
		},
	}
}

// freshAnthropic is a fresh+ok fixture serving two Anthropic models. The
// executor agent "claude-code" maps to provider "claudecode"; reviewers name
// providers explicitly.
func freshOracle() modeloracle.Static {
	return modeloracle.Static{
		Models: map[string][]string{
			"claudecode": {"claude-opus-4-8", "claude-sonnet-4-6"},
			"anthropic":  {"claude-opus-4-8", "claude-sonnet-4-6"},
			"codex":      {"gpt-5.5"},
		},
		Fresh: true,
	}
}

// (a) reject: a model ABSENT from a fresh+ok snapshot is a hard *ValidationError
// naming the model and the available set.
func TestValidateModels_RejectOnFreshAbsence(t *testing.T) {
	s := specWith("claude-code", "claude-typo-9", nil)
	warnings, err := spec.ValidateModels(s, freshOracle())
	if err == nil {
		t.Fatal("err = nil, want a *ValidationError")
	}
	var ve *spec.ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("err type = %T, want *spec.ValidationError", err)
	}
	if !strings.Contains(ve.Message, "claude-typo-9") {
		t.Errorf("message %q does not name the rejected model", ve.Message)
	}
	if !strings.Contains(ve.Message, "claude-opus-4-8") {
		t.Errorf("message %q does not name the available set", ve.Message)
	}
	if len(warnings) != 0 {
		t.Errorf("warnings = %v, want none on a fresh-absence reject", warnings)
	}
}

// (b) accept: a model present in a fresh+ok snapshot yields no error and no
// warning.
func TestValidateModels_AcceptOnFreshPresent(t *testing.T) {
	s := specWith("claude-code", "claude-opus-4-8", nil)
	warnings, err := spec.ValidateModels(s, freshOracle())
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if len(warnings) != 0 {
		t.Errorf("warnings = %v, want none", warnings)
	}
}

// (c) warn/fail-open-stale: a Static fixture with Fresh=false accepts and emits
// a model_unverifiable warning (absence from a stale list cannot reject).
func TestValidateModels_FailOpenStale(t *testing.T) {
	o := freshOracle()
	o.Fresh = false
	s := specWith("claude-code", "claude-typo-9", nil)
	warnings, err := spec.ValidateModels(s, o)
	if err != nil {
		t.Fatalf("err = %v, want nil (stale → fail open)", err)
	}
	if len(warnings) != 1 || warnings[0].Code != spec.WarningCodeModelUnverifiable {
		t.Fatalf("warnings = %#v, want one model_unverifiable", warnings)
	}
}

// (d) fail-open-no-snapshot: a nil oracle AND the wired NoData oracle both
// accept with a warning.
func TestValidateModels_FailOpenNoSnapshot(t *testing.T) {
	s := specWith("claude-code", "claude-anything", nil)

	for _, tc := range []struct {
		name   string
		oracle modeloracle.ModelOracle
	}{
		{"nil", nil},
		{"nodata", modeloracle.NewNoData()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			warnings, err := spec.ValidateModels(s, tc.oracle)
			if err != nil {
				t.Fatalf("err = %v, want nil (fail open)", err)
			}
			if len(warnings) != 1 || warnings[0].Code != spec.WarningCodeModelUnverifiable {
				t.Fatalf("warnings = %#v, want one model_unverifiable", warnings)
			}
		})
	}
}

// (e) both-fields: the reviewer model field is reached and rejected even when
// the executor model is valid (and vice-versa) — proving both fields are
// covered.
func TestValidateModels_BothFieldsCovered(t *testing.T) {
	// executor valid, reviewer bad → reviewer field reached + rejected.
	s := specWith("claude-code", "claude-opus-4-8", &spec.ReviewersConfig{
		Agents: []spec.AgentReviewer{{Provider: "codex", Model: "gpt-nope"}},
	})
	_, err := spec.ValidateModels(s, freshOracle())
	if err == nil {
		t.Fatal("err = nil, want a reviewer-model rejection")
	}
	var ve *spec.ValidationError
	if !errors.As(err, &ve) {
		t.Fatalf("err type = %T, want *spec.ValidationError", err)
	}
	if !strings.Contains(ve.Path, "reviewers/agents/0/model") {
		t.Errorf("path %q is not the reviewer model field", ve.Path)
	}

	// reviewer valid, executor bad → executor field reached + rejected.
	s2 := specWith("claude-code", "claude-bad", &spec.ReviewersConfig{
		Agents: []spec.AgentReviewer{{Provider: "codex", Model: "gpt-5.5"}},
	})
	_, err2 := spec.ValidateModels(s2, freshOracle())
	if err2 == nil {
		t.Fatal("err = nil, want an executor-model rejection")
	}
	var ve2 *spec.ValidationError
	if !errors.As(err2, &ve2) {
		t.Fatalf("err type = %T, want *spec.ValidationError", err2)
	}
	if !strings.Contains(ve2.Path, "executor/model") {
		t.Errorf("path %q is not the executor model field", ve2.Path)
	}
}

// (f) did-you-mean: a near-miss typo yields the expected suggestion string.
func TestValidateModels_DidYouMean(t *testing.T) {
	s := specWith("claude-code", "claude-opus-4-7", nil) // near-miss of claude-opus-4-8
	_, err := spec.ValidateModels(s, freshOracle())
	if err == nil {
		t.Fatal("err = nil, want a rejection with a suggestion")
	}
	if !strings.Contains(err.Error(), `did you mean "claude-opus-4-8"`) {
		t.Errorf("error %q does not carry the expected did-you-mean suggestion", err.Error())
	}
}
