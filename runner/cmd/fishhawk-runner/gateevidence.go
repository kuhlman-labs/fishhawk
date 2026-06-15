package main

import (
	"encoding/json"
	"strings"
	"unicode/utf8"

	"github.com/kuhlman-labs/fishhawk/redaction"
	"github.com/kuhlman-labs/fishhawk/runner/internal/agent"
)

/*
 * Gate evidence (#963). The runner already holds every machine-verified
 * gate result for the stage — verify_run / verify_summary /
 * verify_infra_flake_retry events from the verify gates, the scope_drift
 * and constraint-violation policy_events, and the git_diff staged-file
 * count. composeGateEvidence folds them into ONE bounded, pre-redacted
 * `gate_evidence` event appended before bundle.PackBytes, so the backend
 * can render the facts into the review-agent prompts without re-deriving
 * them from the raw events.
 *
 * Pre-redaction is load-bearing, not belt-and-braces: the implement
 * review dispatches on the RAW bundle variant (#793), before the
 * redacted variant exists, so any free text destined for a reviewer
 * prompt must already be redacted inside the raw bundle. The raw
 * verify_run events keep their full unredacted output for compliance —
 * only this derived summary is pre-redacted. The redacted variant's
 * redactEvents pass re-redacts the payload; that second pass is a no-op
 * because the replacement markers don't match the credential patterns
 * (asserted in tests).
 */

// gateEvidenceTailLines / gateEvidenceTailBytes bound each diagnostic
// tail: last 30 lines, hard-capped at ~4KB, so a megabyte of `go test`
// output can't blow up the review prompt.
const (
	gateEvidenceTailLines = 30
	gateEvidenceTailBytes = 4096
)

// gateEvidencePayload is the body of a gate_evidence event in the
// bundle. The json tags MUST stay identical to the backend's
// bundle.ExtractGateEvidence mirror structs — this is the
// runner↔backend wire contract, agreed field-by-field in lockstep,
// same as gitDiffPayload.
type gateEvidencePayload struct {
	VerifyRuns       []verifyRunEvidence       `json:"verify_runs,omitempty"`
	VerifySummary    *verifySummaryEvidence    `json:"verify_summary,omitempty"`
	FlakeRetries     int                       `json:"flake_retries,omitempty"`
	ScopeFacts       *scopeFactsEvidence       `json:"scope_facts,omitempty"`
	PolicyViolations []policyViolationEvidence `json:"policy_violations,omitempty"`
}

// verifyRunEvidence summarizes one verify_run event. OutputTail is the
// bounded, redacted tail of the command's combined output; on the
// gates' skip paths it carries the skip reason (e.g. "worktree_tmp:
// ..."), so a skipped outcome is never reason-less.
type verifyRunEvidence struct {
	Command       string `json:"command"`
	ExitCode      int    `json:"exit_code"`
	Outcome       string `json:"outcome"`
	HeadSHA       string `json:"head_sha,omitempty"`
	TreeSHA       string `json:"tree_sha,omitempty"`
	OutputTail    string `json:"output_tail,omitempty"`
	TailTruncated bool   `json:"tail_truncated,omitempty"`
}

// verifySummaryEvidence mirrors the verify-fix loop's verify_summary
// outcome. Detail is redacted and tail-bounded like the run tails.
type verifySummaryEvidence struct {
	Outcome       string `json:"outcome"`
	Iterations    int    `json:"iterations"`
	MaxIterations int    `json:"max_iterations"`
	Detail        string `json:"detail,omitempty"`
}

// scopeFactsEvidence carries the declared-vs-staged scope facts.
// StagedFiles is a pointer so "no git_diff event ran" (nil) stays
// distinguishable from a real zero-file diff. UndeclaredPaths is the
// scope_drift list of agent-touched paths excluded from the commit.
// UndeclaredCategorized is the per-path A/B categorization of the same
// list (#991); it is additive and may be absent (categorization is
// best-effort at the emit site), so UndeclaredPaths stays the
// authoritative drift list.
type scopeFactsEvidence struct {
	DeclaredFiles         int                 `json:"declared_files"`
	StagedFiles           *int                `json:"staged_files,omitempty"`
	UndeclaredPaths       []string            `json:"undeclared_paths,omitempty"`
	UndeclaredCategorized []driftPathEvidence `json:"undeclared_categorized,omitempty"`
}

// driftPathEvidence is one categorized scope-drift path (#991).
// Category "A" is an agent edit to a tracked file that was EXCLUDED
// from the commit (the pushed head may be missing a required change);
// category "B" is a file created out of scope (where the #818/#825
// created-out-of-scope gate applies). Disposition records what the
// enforcement did with the path: "excluded_from_commit" or
// "would_fail_loud". The json tags MUST stay identical to the backend's
// bundle.DriftPathEvidence mirror — same lockstep wire contract as the
// parent payload.
type driftPathEvidence struct {
	Path        string `json:"path"`
	Category    string `json:"category"`
	Disposition string `json:"disposition,omitempty"`
}

// policyViolationEvidence is one policy_event with outcome
// "violation" (today: the constraint enforcer's per-violation
// events). Detail is redacted.
type policyViolationEvidence struct {
	Check      string   `json:"check"`
	Constraint string   `json:"constraint,omitempty"`
	Detail     string   `json:"detail,omitempty"`
	Files      []string `json:"files,omitempty"`
}

// composeGateEvidence scans the stage's collected events and
// synthesizes the single gate_evidence event described above.
// declaredScopeCount is the number of declared scope.files paths
// (post-amendment-fold, the same count every gate enforces).
//
// Returns nil when no gate ran — no verify_run, verify_summary, or
// policy_event in the slice — so stages without gates add no event
// and the backend's no-evidence prompt path stays byte-identical.
// Individual payloads that fail to decode are skipped best-effort:
// the evidence is advisory prompt context, never a gate itself.
func composeGateEvidence(events []agent.Event, declaredScopeCount int) *agent.Event {
	gateRan := false
	for _, e := range events {
		switch e.Kind {
		case "verify_run", "verify_summary", "policy_event":
			gateRan = true
		}
	}
	if !gateRan {
		return nil
	}

	payload := gateEvidencePayload{
		ScopeFacts: &scopeFactsEvidence{DeclaredFiles: declaredScopeCount},
	}

	for _, e := range events {
		switch e.Kind {
		case "verify_run":
			var w struct {
				Command  string `json:"command"`
				ExitCode int    `json:"exit_code"`
				Output   string `json:"output"`
				Outcome  string `json:"outcome"`
				HeadSHA  string `json:"head_sha"`
				TreeSHA  string `json:"tree_sha"`
			}
			if json.Unmarshal(e.Payload, &w) != nil {
				continue
			}
			tail, truncated := boundEvidenceTail(redactEvidenceText(w.Output))
			payload.VerifyRuns = append(payload.VerifyRuns, verifyRunEvidence{
				Command:       w.Command,
				ExitCode:      w.ExitCode,
				Outcome:       w.Outcome,
				HeadSHA:       w.HeadSHA,
				TreeSHA:       w.TreeSHA,
				OutputTail:    tail,
				TailTruncated: truncated,
			})
		case "verify_summary":
			var w struct {
				Outcome       string `json:"outcome"`
				Iterations    int    `json:"iterations"`
				MaxIterations int    `json:"max_iterations"`
				Detail        string `json:"detail"`
			}
			if json.Unmarshal(e.Payload, &w) != nil {
				continue
			}
			detail, _ := boundEvidenceTail(redactEvidenceText(w.Detail))
			payload.VerifySummary = &verifySummaryEvidence{
				Outcome:       w.Outcome,
				Iterations:    w.Iterations,
				MaxIterations: w.MaxIterations,
				Detail:        detail,
			}
		case "verify_infra_flake_retry":
			payload.FlakeRetries++
		case "policy_event":
			var w struct {
				Check                 string              `json:"check"`
				Outcome               string              `json:"outcome"`
				Undeclared            []string            `json:"undeclared"`
				UndeclaredCategorized []driftPathEvidence `json:"undeclared_categorized"`
				Constraint            string              `json:"constraint"`
				Detail                string              `json:"detail"`
				Files                 []string            `json:"files"`
			}
			if json.Unmarshal(e.Payload, &w) != nil {
				continue
			}
			switch {
			case w.Check == "scope_drift" && w.Outcome == "excluded":
				payload.ScopeFacts.UndeclaredPaths =
					append(payload.ScopeFacts.UndeclaredPaths, w.Undeclared...)
				payload.ScopeFacts.UndeclaredCategorized =
					append(payload.ScopeFacts.UndeclaredCategorized, w.UndeclaredCategorized...)
			case w.Outcome == "violation":
				payload.PolicyViolations = append(payload.PolicyViolations, policyViolationEvidence{
					Check:      w.Check,
					Constraint: w.Constraint,
					Detail:     redactEvidenceText(w.Detail),
					Files:      w.Files,
				})
			}
		case "git_diff":
			var w struct {
				NumFiles int `json:"num_files"`
			}
			if json.Unmarshal(e.Payload, &w) != nil {
				continue
			}
			// Last-write-wins, matching the backend's ExtractDiff: the
			// #870 post-fix-loop re-emit supersedes the pre-reconcile diff.
			n := w.NumFiles
			payload.ScopeFacts.StagedFiles = &n
		}
	}

	return &agent.Event{
		Kind:    "gate_evidence",
		Payload: agent.MakePayload(payload),
	}
}

// redactEvidenceText runs RedactDefault over one free-text field.
// Redaction happens BEFORE bounding: if the tail cut ran first, it
// could slice a credential in half and the remaining fragment would no
// longer match any pattern — leaking half a secret into the evidence.
func redactEvidenceText(s string) string {
	if s == "" {
		return s
	}
	red, _ := redaction.RedactDefault([]byte(s))
	return string(red)
}

// boundEvidenceTail bounds a diagnostic text to its last
// gateEvidenceTailLines lines and gateEvidenceTailBytes bytes,
// reporting whether anything was cut. The byte cap keeps the suffix
// (failures conventionally end with the summary line) and trims any
// leading partial UTF-8 rune the cut exposed.
func boundEvidenceTail(s string) (string, bool) {
	s = strings.TrimRight(s, "\n")
	if s == "" {
		return "", false
	}
	truncated := false
	lines := strings.Split(s, "\n")
	if len(lines) > gateEvidenceTailLines {
		lines = lines[len(lines)-gateEvidenceTailLines:]
		truncated = true
	}
	out := strings.Join(lines, "\n")
	if len(out) > gateEvidenceTailBytes {
		out = out[len(out)-gateEvidenceTailBytes:]
		for len(out) > 0 && !utf8.RuneStart(out[0]) {
			out = out[1:]
		}
		truncated = true
	}
	return out, truncated
}
