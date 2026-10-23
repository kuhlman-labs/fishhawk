package issuecomment

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/planreview"
)

// The builders below seed review audit rows by json.Marshal of the REAL
// planreview payload structs, so the role tests cross the payload-struct ↔
// renderer seam on the production wire tags rather than on hand-written maps.

// roleVerdictEntry builds a <stageType>_reviewed row from a real
// PlanReviewedPayload / ImplementReviewedPayload. persona "" is a standard
// reviewer (the field is omitted on the wire).
func roleVerdictEntry(t *testing.T, seq int64, stageType, model, persona string, verdict planreview.Verdict, concerns []planreview.Concern) *audit.Entry {
	t.Helper()
	var payload []byte
	var err error
	switch stageType {
	case "plan":
		payload, err = json.Marshal(planreview.PlanReviewedPayload{
			ReviewerKind: "agent", ReviewerModel: model, Authority: planreview.AuthorityAdvisory,
			Verdict: verdict, Concerns: concerns, Persona: persona,
		})
	case "implement":
		payload, err = json.Marshal(planreview.ImplementReviewedPayload{
			ReviewerKind: "agent", ReviewerModel: model, Authority: planreview.AuthorityAdvisory,
			Verdict: verdict, Concerns: concerns, Persona: persona,
		})
	default:
		t.Fatalf("roleVerdictEntry: unsupported stage type %q", stageType)
	}
	if err != nil {
		t.Fatalf("marshal verdict payload: %v", err)
	}
	return &audit.Entry{Sequence: seq, Category: stageType + "_reviewed", Payload: payload, Timestamp: time.Unix(seq, 0).UTC()}
}

// roleSkippedEntry builds a <stageType>_review_skipped row from a real
// ReviewSkippedPayload.
func roleSkippedEntry(t *testing.T, seq int64, stageType, persona, reason string) *audit.Entry {
	t.Helper()
	payload, err := json.Marshal(planreview.ReviewSkippedPayload{
		Reason: reason, ConfiguredAgents: 2, Authority: planreview.AuthorityAdvisory, Persona: persona,
	})
	if err != nil {
		t.Fatalf("marshal skipped payload: %v", err)
	}
	return &audit.Entry{Sequence: seq, Category: stageType + "_review_skipped", Payload: payload, Timestamp: time.Unix(seq, 0).UTC()}
}

// roleFailedEntry builds a <stageType>_review_failed row from a real
// ReviewFailedPayload.
func roleFailedEntry(t *testing.T, seq int64, stageType, model, persona, reason string, timeout bool) *audit.Entry {
	t.Helper()
	payload, err := json.Marshal(planreview.ReviewFailedPayload{
		Reason: reason, ReviewerModel: model, Authority: planreview.AuthorityAdvisory, Timeout: timeout, Persona: persona,
	})
	if err != nil {
		t.Fatalf("marshal failed payload: %v", err)
	}
	return &audit.Entry{Sequence: seq, Category: stageType + "_review_failed", Payload: payload, Timestamp: time.Unix(seq, 0).UTC()}
}

// TestReviewerRole pins the role-label rule (E89.33 / #4669): an empty persona
// is the stage's standard role; a declared persona is title-cased from its
// name plus " Reviewer" with no built-in persona list (ADR-110 — `compliance`
// is declared by no repository Fishhawk ships and still labels); and a name
// breaking the workflow-v2 reviewer_personas key rule renders the fixed
// fallback and is never echoed.
func TestReviewerRole(t *testing.T) {
	cases := []struct {
		name      string
		stageType string
		persona   string
		want      string
	}{
		{"standard plan", "plan", "", "Plan Reviewer"},
		{"standard implement", "implement", "", "PR Reviewer"},
		{"standard other stage", "acceptance", "", "Reviewer"},
		{"architect", "plan", "architect", "Architect Reviewer"},
		{"security", "implement", "security", "Security Reviewer"},
		{"undeclared-here compliance", "implement", "compliance", "Compliance Reviewer"},
		{"multi-word", "implement", "security_portability", "Security Portability Reviewer"},
		{"trailing reviewer word dropped", "plan", "code_reviewer", "Code Reviewer"},
		{"only word is reviewer", "plan", "reviewer", "Reviewer"},
		{"doubled reviewer", "plan", "reviewer_reviewer", "Reviewer"},
		{"empty words dropped", "plan", "data__privacy_", "Data Privacy Reviewer"},
		{"digits kept", "plan", "a11y", "A11y Reviewer"},
		{"mention and space", "plan", "@evil user", personaFallbackRole},
		{"markdown link", "plan", "[x](https://evil.example)", personaFallbackRole},
		{"upper case", "plan", "Security", personaFallbackRole},
		{"colon", "plan", "x:y", personaFallbackRole},
		{"space", "plan", "has space", personaFallbackRole},
		{"embedded newline", "plan", "sec\nurity", personaFallbackRole},
		{"leading digit", "plan", "1st", personaFallbackRole},
		{"leading underscore", "plan", "_sec", personaFallbackRole},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := reviewerRole(tc.stageType, tc.persona)
			if got != tc.want {
				t.Fatalf("reviewerRole(%q, %q) = %q, want %q", tc.stageType, tc.persona, got, tc.want)
			}
			if tc.want == personaFallbackRole && strings.Contains(got, tc.persona) {
				t.Fatalf("an invalid persona name must never be echoed; got %q", got)
			}
		})
	}
	if personaFallbackRole != "Persona Reviewer" {
		t.Fatalf("personaFallbackRole = %q, want %q", personaFallbackRole, "Persona Reviewer")
	}
}

// TestReviewerPersonaOf pins that the persona decoder yields "" for every
// no-persona input — empty payload, absent key, JSON null, empty string and an
// undecodable payload — and the name when present.
func TestReviewerPersonaOf(t *testing.T) {
	cases := []struct {
		name    string
		payload string
		want    string
	}{
		{"present", `{"persona":"security","verdict":"reject"}`, "security"},
		{"absent", `{"verdict":"approve"}`, ""},
		{"null", `{"persona":null}`, ""},
		{"empty string", `{"persona":""}`, ""},
		{"undecodable", `{"persona":`, ""},
		{"wrong type", `{"persona":7}`, ""},
		{"empty payload", ``, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := reviewerPersonaOf([]byte(tc.payload)); got != tc.want {
				t.Fatalf("reviewerPersonaOf(%q) = %q, want %q", tc.payload, got, tc.want)
			}
		})
	}
}

// TestRoleWithModel pins the "<role> (<model>)" join and the model-less form.
func TestRoleWithModel(t *testing.T) {
	if got := roleWithModel("PR Reviewer", "m1"); got != "PR Reviewer (m1)" {
		t.Errorf("roleWithModel with model = %q", got)
	}
	if got := roleWithModel("PR Reviewer", ""); got != "PR Reviewer" {
		t.Errorf("roleWithModel without model = %q", got)
	}
}
