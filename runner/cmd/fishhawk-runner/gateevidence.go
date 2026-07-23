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

// diffCoverageMaxUncovered bounds the uncovered-file sample carried in the
// diff-coverage evidence (#1888). The list exists to make the violation
// actionable, not to be exhaustive — a stage that leaves 400 files
// uncovered must not push 400 paths into the review prompt.
const diffCoverageMaxUncovered = 10

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
	// BindingAssertions digests the operator-declared binding-assertion
	// checks evaluated against the committed scope-only tree (#1171),
	// each with its satisfied verdict, so the implement review sees which
	// binding conditions were machine-verified. Absent (the byte-identical
	// default) when no assertions were declared.
	BindingAssertions []bindingAssertionEvidence `json:"binding_assertions,omitempty"`
	// ScopeExemptions digests the agent's validated scope self-exemptions
	// (#1153): declared scope.files paths deliberately left unchanged and
	// justified in-band, folded from the scope_files_exempted event. Absent
	// when none were validated. The json tag MUST stay identical to the
	// backend's bundle.ScopeExemptionEvidence mirror.
	ScopeExemptions []scopeExemptionEvidence `json:"scope_exemptions,omitempty"`
	// FixupSelfReportDivergence digests the ADVISORY fix-up self-report
	// divergence (#1210): on a fix-up pass the agent CLAIMED a verify outcome
	// (via its structured sidecar) that disagreed with the committed-tree verify
	// outcome the runner computed. Folded from the fixup_selfreport_divergence
	// event. Absent (the byte-identical default) when there was no fix-up pass,
	// no claim, or claim and reality agreed. The json tag MUST stay identical to
	// the backend's bundle.FixupSelfReportDivergenceEvidence mirror.
	FixupSelfReportDivergence *fixupSelfReportDivergenceEvidence `json:"fixup_selfreport_divergence,omitempty"`
	// DiffCoverage digests the workflow-v1.6 `diff_coverage` measurement
	// (ADR-059 / #1888): what the customer coverage command was, how it
	// exited, and how much of the stage's added-line set the resulting
	// report showed as executed. Present WHENEVER the stage declared the
	// constraint — including when the diff added no coverable lines, which
	// is reported as an explicit measured-with-zero result. Absent (the
	// byte-identical default) when the stage did not declare it. The json
	// tag MUST stay identical to the backend's bundle.DiffCoverageEvidence
	// mirror; a drift silently DISABLES the gate, because the backend then
	// sees no signal and the constraint reports nothing wrong.
	DiffCoverage *diffCoverageEvidence `json:"diff_coverage,omitempty"`
}

// diffCoverageEvidence digests one diff-coverage measurement (#1888).
// Bounded like every sibling: no report body, no command output beyond the
// existing tail cap, and UncoveredFiles capped at diffCoverageMaxUncovered.
// The json tags MUST stay identical to the backend's
// bundle.DiffCoverageEvidence mirror — the same lockstep runner↔backend
// wire contract as the parent payload.
type diffCoverageEvidence struct {
	// Outcome is "measured" (the command ran, the report parsed, the
	// counts below are meaningful) or "failed" (the measurement could not
	// be completed; Reason names why).
	Outcome         string   `json:"outcome"`
	Command         string   `json:"command,omitempty"`
	ExitCode        int      `json:"exit_code"`
	ReportPath      string   `json:"report_path,omitempty"`
	BaseRef         string   `json:"base_ref,omitempty"`
	NewLines        int      `json:"new_lines"`
	CoveredNewLines int      `json:"covered_new_lines"`
	Percent         float64  `json:"percent"`
	UncoveredFiles  []string `json:"uncovered_files,omitempty"`
	Reason          string   `json:"reason,omitempty"`
}

// fixupSelfReportDivergenceEvidence digests an advisory fix-up self-report
// divergence (#1210): the agent's claimed verify status vs the runner's actual
// committed-tree verify outcome. The json tags MUST stay identical to the
// backend's bundle.FixupSelfReportDivergenceEvidence mirror — the same lockstep
// runner↔backend wire contract as the parent payload.
type fixupSelfReportDivergenceEvidence struct {
	ClaimedVerifyStatus string `json:"claimed_verify_status"`
	ActualVerifyStatus  string `json:"actual_verify_status"`
}

// scopeExemptionEvidence is one validated scope self-exemption (#1153): a
// declared scope.files path the agent deliberately left unchanged plus the
// reason. The json tags MUST stay identical to the backend's
// bundle.ScopeExemptionEvidence mirror — the same lockstep runner↔backend wire
// contract as the parent payload.
type scopeExemptionEvidence struct {
	Path   string `json:"path"`
	Reason string `json:"reason"`
}

// bindingAssertionEvidence is one digested binding-assertion check (#1171):
// the operator-declared type/path/literal plus whether the committed tree
// satisfied it. The json tags MUST stay identical to the backend's
// bundle.BindingAssertionEvidence mirror — the same lockstep runner↔backend
// wire contract as the parent payload.
type bindingAssertionEvidence struct {
	Type      string `json:"type"`
	Path      string `json:"path"`
	Literal   string `json:"literal"`
	Satisfied bool   `json:"satisfied"`
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
	// Superseded is true when a LATER verify_run iteration superseded this
	// one in the verify-fix loop (#651/#800), so it is NOT the committed-tree
	// result — the loop re-ran the SAME gate on a newer tree and only the
	// LAST verify_run reflects the pushed/committed tree (it agrees with
	// verify_summary.Outcome). The terminal (last) run is NEVER marked, so a
	// genuine terminal failure stays a committed-tree blocker (#1205).
	// Additive/omitempty: older bundles decode to false (not superseded).
	Superseded bool `json:"superseded,omitempty"`
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
		case "verify_run", "verify_summary", "policy_event", "binding_assertion", "scope_files_exempted", "fixup_selfreport_divergence", "diff_coverage":
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
		case "binding_assertion":
			var w struct {
				Assertions []bindingAssertionEvidence `json:"assertions"`
			}
			if json.Unmarshal(e.Payload, &w) != nil {
				continue
			}
			payload.BindingAssertions = append(payload.BindingAssertions, w.Assertions...)
		case "scope_files_exempted":
			var w struct {
				Exemptions []scopeExemptionEvidence `json:"exemptions"`
			}
			if json.Unmarshal(e.Payload, &w) != nil {
				continue
			}
			payload.ScopeExemptions = append(payload.ScopeExemptions, w.Exemptions...)
		case "fixup_selfreport_divergence":
			var w struct {
				ClaimedVerifyStatus string `json:"claimed_verify_status"`
				ActualVerifyStatus  string `json:"actual_verify_status"`
			}
			if json.Unmarshal(e.Payload, &w) != nil {
				continue
			}
			payload.FixupSelfReportDivergence = &fixupSelfReportDivergenceEvidence{
				ClaimedVerifyStatus: w.ClaimedVerifyStatus,
				ActualVerifyStatus:  w.ActualVerifyStatus,
			}
		case "diff_coverage":
			var w diffCoverageEvidence
			if json.Unmarshal(e.Payload, &w) != nil {
				continue
			}
			// Pre-redact + bound the only free-text field, exactly like
			// every other evidence tail: Reason can quote the coverage
			// command's stderr, which is agent/customer-controlled text.
			w.Reason, _ = boundEvidenceTail(redactEvidenceText(w.Reason))
			w.Command = redactEvidenceText(w.Command)
			if len(w.UncoveredFiles) > diffCoverageMaxUncovered {
				w.UncoveredFiles = w.UncoveredFiles[:diffCoverageMaxUncovered]
			}
			dc := w
			payload.DiffCoverage = &dc
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

	// Mark every verify_run EXCEPT the last as superseded (#1205). The
	// verify-fix loop re-runs the SAME committed-tree gate per iteration; an
	// earlier failing iteration that the loop then absorbed and re-ran green
	// operates on a stale tree, so surfacing it as the committed-tree result
	// false-rejects the review (run fa5a6416/#1199). Only the LAST verify_run
	// reflects the pushed/committed tree and agrees with verify_summary.Outcome,
	// so it is NEVER marked — a genuine terminal failure (including a
	// budget-exhausted [fail,fail] loop) still surfaces as a blocker.
	if len(payload.VerifyRuns) > 1 {
		for i := 0; i < len(payload.VerifyRuns)-1; i++ {
			payload.VerifyRuns[i].Superseded = true
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
