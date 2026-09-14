package main

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/kuhlman-labs/fishhawk/runner/internal/agent"
)

// TestApprovalConditionsHeadingLiteral pins the shipped heading grammar on the
// RUNNER side of the #3400 contract. The backend's implement prompt instructs
// the agent to write this exact line (TestBuild_Implement_ApprovalConditions_CommitBodyInstruction
// in backend/internal/prompt asserts the same literal), so a one-sided edit
// fails the other module's pin.
func TestApprovalConditionsHeadingLiteral(t *testing.T) {
	if approvalConditionsHeading != "Approval conditions:" {
		t.Fatalf("approvalConditionsHeading = %q, want the literal the implement prompt teaches", approvalConditionsHeading)
	}
}

func TestParseCommitMessageSidecar(t *testing.T) {
	cases := []struct {
		name          string
		raw           string
		subject, body string
		ok            bool
	}{
		{"crlf", "feat: x\r\n\r\nbody line\r\n", "feat: x", "body line", true},
		{"leading blank lines", "\n\n  feat: x\n\nbody", "feat: x", "body", true},
		{"subject only", "feat: x\n", "feat: x", "", true},
		{"empty", "", "", "", false},
		{"whitespace only", " \n\t\n", "", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			subject, body, ok := parseCommitMessageSidecar([]byte(tc.raw))
			if ok != tc.ok || subject != tc.subject || body != tc.body {
				t.Errorf("parseCommitMessageSidecar(%q) = (%q, %q, %v), want (%q, %q, %v)",
					tc.raw, subject, body, ok, tc.subject, tc.body, tc.ok)
			}
		})
	}
}

func TestExtractApprovalConditionResponses(t *testing.T) {
	cases := []struct {
		name  string
		body  string
		text  string
		found bool
	}{
		{"present with entries", "What changed.\n\nApproval conditions:\n- condition 1: added the test\n- condition 2: no action required because the path is a permission\n",
			"- condition 1: added the test\n- condition 2: no action required because the path is a permission", true},
		{"absent", "What changed.\n", "", false},
		{"heading only, whitespace remainder", "What changed.\n\nApproval conditions:\n   \n", "", false},
		{"markdown heading does not match", "## Approval conditions\n- condition 1: x\n", "", false},
		{"lower-case does not match", "approval conditions:\n- condition 1: x\n", "", false},
		{"indented heading matches (TrimSpace)", "  Approval conditions:  \n- condition 1: x", "- condition 1: x", true},
		{"first heading wins", "Approval conditions:\n- condition 1: first\n\nApproval conditions:\n- condition 1: second",
			"- condition 1: first\n\nApproval conditions:\n- condition 1: second", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			text, found := extractApprovalConditionResponses(tc.body)
			if found != tc.found || text != tc.text {
				t.Errorf("extractApprovalConditionResponses(%q) = (%q, %v), want (%q, %v)", tc.body, text, found, tc.text, tc.found)
			}
		})
	}
}

func TestBoundApprovalConditionResponses(t *testing.T) {
	under := strings.Repeat("a", approvalConditionResponsesMaxBytes)
	if got, tr := boundApprovalConditionResponses(under); tr || got != under {
		t.Errorf("at-cap text must pass untruncated; truncated=%v len=%d", tr, len(got))
	}
	// Multi-byte runes straddling the cap: the cut must land on a rune boundary.
	over := strings.Repeat("é", approvalConditionResponsesMaxBytes) // 2 bytes each → 8 KiB
	got, tr := boundApprovalConditionResponses(over)
	if !tr {
		t.Fatal("over-cap text must report truncated=true")
	}
	if len(got) > approvalConditionResponsesMaxBytes {
		t.Errorf("bounded len = %d, want ≤ %d", len(got), approvalConditionResponsesMaxBytes)
	}
	if !utf8.ValidString(got) {
		t.Errorf("bounded text is not valid UTF-8 — cut inside a rune")
	}
}

// redirectCommitMessageDirs points both sidecar dirs at a temp dir for the
// duration of the test.
func redirectCommitMessageDirs(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	origImpl, origFix := implementCommitMessageDir, fixupCommitMessageDir
	implementCommitMessageDir, fixupCommitMessageDir = dir, dir
	t.Cleanup(func() { implementCommitMessageDir, fixupCommitMessageDir = origImpl, origFix })
	return dir
}

type acrPayload struct {
	RunID     string `json:"run_id"`
	StageID   string `json:"stage_id"`
	Source    string `json:"source"`
	Text      string `json:"text"`
	Truncated bool   `json:"truncated"`
}

func decodeACR(t *testing.T, ev *agent.Event) acrPayload {
	t.Helper()
	if ev == nil {
		t.Fatal("expected an approval_condition_responses event, got nil")
	}
	if ev.Kind != "approval_condition_responses" {
		t.Fatalf("event kind = %q", ev.Kind)
	}
	var p acrPayload
	if err := json.Unmarshal(ev.Payload, &p); err != nil {
		t.Fatalf("decode payload: %v", err)
	}
	return p
}

const acrSidecarWithSection = "feat: x\n\nWhat changed.\n\nApproval conditions:\n- condition 1: added the drift test\n- condition 2: no action required because the path is a permission\n"

func TestPeekApprovalConditionResponses(t *testing.T) {
	cfg := config{runID: "run-3400", stageID: "stage-3400"}

	t.Run("implement sidecar with section → event, sidecar NOT deleted", func(t *testing.T) {
		redirectCommitMessageDirs(t)
		path := implementCommitMessagePath(cfg.runID, cfg.stageID)
		if err := os.WriteFile(path, []byte(acrSidecarWithSection), 0o600); err != nil {
			t.Fatal(err)
		}
		var logSink strings.Builder
		p := decodeACR(t, peekApprovalConditionResponses(cfg, &logSink))
		if p.Source != "implement_commitmsg" || p.RunID != cfg.runID || p.StageID != cfg.stageID || p.Truncated {
			t.Errorf("payload = %+v", p)
		}
		if p.Text != "- condition 1: added the drift test\n- condition 2: no action required because the path is a permission" {
			t.Errorf("text = %q", p.Text)
		}
		if _, err := os.Stat(path); err != nil {
			t.Errorf("peek must be read-not-delete; sidecar stat: %v", err)
		}
		if !strings.Contains(logSink.String(), `"event":"approval_condition_responses_captured"`) ||
			!strings.Contains(logSink.String(), `"source":"implement_commitmsg"`) {
			t.Errorf("missing captured log line: %s", logSink.String())
		}
	})

	t.Run("cfg.fixup selects the fixup sidecar", func(t *testing.T) {
		redirectCommitMessageDirs(t)
		fcfg := cfg
		fcfg.fixup = true
		if err := os.WriteFile(fixupCommitMessagePath(cfg.runID, cfg.stageID), []byte(acrSidecarWithSection), 0o600); err != nil {
			t.Fatal(err)
		}
		var logSink strings.Builder
		p := decodeACR(t, peekApprovalConditionResponses(fcfg, &logSink))
		if p.Source != "fixup_commitmsg" {
			t.Errorf("source = %q, want fixup_commitmsg", p.Source)
		}
		// The implement path was never written; a fixup cfg must not read it.
		if ev := peekApprovalConditionResponses(cfg, &logSink); ev != nil {
			t.Errorf("non-fixup cfg read the fixup sidecar: %s", ev.Payload)
		}
	})

	t.Run("not-exist → nil", func(t *testing.T) {
		redirectCommitMessageDirs(t)
		var logSink strings.Builder
		if ev := peekApprovalConditionResponses(cfg, &logSink); ev != nil {
			t.Errorf("got event %s, want nil", ev.Payload)
		}
		if logSink.Len() != 0 {
			t.Errorf("not-exist must log nothing: %s", logSink.String())
		}
	})

	t.Run("oversize → nil", func(t *testing.T) {
		redirectCommitMessageDirs(t)
		path := implementCommitMessagePath(cfg.runID, cfg.stageID)
		// A section-bearing message padded past the sidecar ceiling: the
		// ceiling — not a missing section — is what makes it absent.
		big := acrSidecarWithSection + strings.Repeat("x", int(maxSidecarBytes)+1)
		if err := os.WriteFile(path, []byte(big), 0o600); err != nil {
			t.Fatal(err)
		}
		var logSink strings.Builder
		if ev := peekApprovalConditionResponses(cfg, &logSink); ev != nil {
			t.Errorf("oversize sidecar must be absent to the peek; got %s", ev.Payload)
		}
		if _, err := os.Stat(path); err != nil {
			t.Errorf("peek must not delete even an oversize sidecar (the loader owns that): %v", err)
		}
	})

	t.Run("empty → nil", func(t *testing.T) {
		redirectCommitMessageDirs(t)
		if err := os.WriteFile(implementCommitMessagePath(cfg.runID, cfg.stageID), []byte(" \n\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		var logSink strings.Builder
		if ev := peekApprovalConditionResponses(cfg, &logSink); ev != nil {
			t.Errorf("got %s, want nil", ev.Payload)
		}
	})

	t.Run("no section → nil", func(t *testing.T) {
		redirectCommitMessageDirs(t)
		if err := os.WriteFile(implementCommitMessagePath(cfg.runID, cfg.stageID), []byte("feat: x\n\nbody with no section\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		var logSink strings.Builder
		if ev := peekApprovalConditionResponses(cfg, &logSink); ev != nil {
			t.Errorf("got %s, want nil", ev.Payload)
		}
	})

	t.Run("credential in the section is redacted", func(t *testing.T) {
		redirectCommitMessageDirs(t)
		const secret = "ghp_ABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789ab"
		msg := "feat: x\n\nApproval conditions:\n- condition 1: used token " + secret + " to verify\n"
		if err := os.WriteFile(implementCommitMessagePath(cfg.runID, cfg.stageID), []byte(msg), 0o600); err != nil {
			t.Fatal(err)
		}
		var logSink strings.Builder
		p := decodeACR(t, peekApprovalConditionResponses(cfg, &logSink))
		if strings.Contains(p.Text, secret) {
			t.Fatalf("secret leaked into the event payload: %q", p.Text)
		}
		if !strings.Contains(p.Text, "[REDACTED") {
			t.Errorf("expected a redaction marker in %q", p.Text)
		}
	})

	t.Run("credential straddling the byte bound leaves no fragment", func(t *testing.T) {
		// Redaction must run BEFORE bounding: a ghp_ token needs exactly 36
		// trailing chars to match, so cutting first would leave an
		// unrecognizable prefix (ghp_ + leading bytes) in the carried text.
		// Place the token so the 4096-byte cut lands in its middle.
		redirectCommitMessageDirs(t)
		const secret = "ghp_ABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789ab"
		pad := strings.Repeat("a", approvalConditionResponsesMaxBytes-len("- condition 1: ")-len(secret)/2)
		section := "- condition 1: " + pad + secret + " used to verify\n"
		if got := len(section); got <= approvalConditionResponsesMaxBytes {
			t.Fatalf("fixture must exceed the bound: len=%d", got)
		}
		msg := "feat: x\n\nApproval conditions:\n" + section
		if err := os.WriteFile(implementCommitMessagePath(cfg.runID, cfg.stageID), []byte(msg), 0o600); err != nil {
			t.Fatal(err)
		}
		var logSink strings.Builder
		p := decodeACR(t, peekApprovalConditionResponses(cfg, &logSink))
		if strings.Contains(p.Text, "ghp_") {
			t.Fatalf("credential fragment reached the event payload: %q", p.Text[len(p.Text)-64:])
		}
		if !p.Truncated || len(p.Text) > approvalConditionResponsesMaxBytes {
			t.Errorf("truncated=%v len=%d", p.Truncated, len(p.Text))
		}
	})

	t.Run("over-bound section → truncated=true", func(t *testing.T) {
		redirectCommitMessageDirs(t)
		msg := "feat: x\n\nApproval conditions:\n" + strings.Repeat("- condition 1: long\n", 400)
		if err := os.WriteFile(implementCommitMessagePath(cfg.runID, cfg.stageID), []byte(msg), 0o600); err != nil {
			t.Fatal(err)
		}
		var logSink strings.Builder
		p := decodeACR(t, peekApprovalConditionResponses(cfg, &logSink))
		if !p.Truncated || len(p.Text) > approvalConditionResponsesMaxBytes {
			t.Errorf("truncated=%v len=%d", p.Truncated, len(p.Text))
		}
	})
}

// TestPeekAndConsumeAgree pins that the section the PEEK reports is exactly
// the section the CONSUMING loader carries into the commit message for the
// same sidecar bytes — the two share parseCommitMessageSidecar and
// readSidecarBounded, so what the reviewer sees is what the commit would say.
func TestPeekAndConsumeAgree(t *testing.T) {
	redirectCommitMessageDirs(t)
	cfg := config{runID: "run-agree", stageID: "stage-agree"}
	path := implementCommitMessagePath(cfg.runID, cfg.stageID)
	if err := os.WriteFile(path, []byte(acrSidecarWithSection), 0o600); err != nil {
		t.Fatal(err)
	}
	var logSink strings.Builder
	peeked := decodeACR(t, peekApprovalConditionResponses(cfg, &logSink))
	// Consume through the REAL commit-message resolver (deletes the sidecar).
	msg := implementCommitMessage(cfg, "pr title", "pr body", &logSink)
	_, body, ok := parseCommitMessageSidecar([]byte(msg))
	if !ok {
		t.Fatal("consumed commit message parsed as empty")
	}
	consumed, found := extractApprovalConditionResponses(body)
	if !found || consumed != peeked.Text {
		t.Errorf("peek/consume disagree:\npeeked:   %q\nconsumed: %q (found=%v)", peeked.Text, consumed, found)
	}
	if _, err := os.Stat(path); err == nil {
		t.Error("the consuming loader must still delete the sidecar after the peek")
	}
}

func TestLogApprovalConditionResponsesCommitted(t *testing.T) {
	cfg := config{runID: "run-c", stageID: "stage-c"}
	cases := []struct {
		name    string
		msg     string
		headSHA string
		want    bool
	}{
		{"section + head → logged", acrSidecarWithSection, "abc123", true},
		{"section but no head (NoChanges) → nothing", acrSidecarWithSection, "", false},
		{"no section → nothing", "feat: x\n\nbody\n", "abc123", false},
		{"synthetic fallback message → nothing", "chore: fishhawk fixup stage s (base b)", "abc123", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var logSink strings.Builder
			logApprovalConditionResponsesCommitted(cfg, tc.msg, tc.headSHA, &logSink)
			got := strings.Contains(logSink.String(), `"event":"approval_condition_responses_committed"`)
			if got != tc.want {
				t.Errorf("logged=%v want %v: %s", got, tc.want, logSink.String())
			}
			if tc.want && !strings.Contains(logSink.String(), `"head_sha":"abc123"`) {
				t.Errorf("committed line must name the head: %s", logSink.String())
			}
		})
	}
}
