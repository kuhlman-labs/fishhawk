// Plan-review-miss corpus scaffolding (E31.11 / #1539, ADR-049 decision #4).
//
// The class-3 sibling of the trace-bundle Distill: given a run's
// acceptance_triage_decided audit items, it filters to class-3 decisions
// (a failed criterion that was inferred-source or unresolvable — a bad
// criterion the plan gate approved) carrying the plan_review_miss record
// and writes one plan-review-miss corpus case per decision
// (miss.json + case.md) under OutDir. Case SELECTION, labeling, and
// committing stay operator curation (#819 / ADR-040) — the tool scaffolds
// a CANDIDATE. Only structured verdict fields cross: evidence blobs stay
// customer-side per ADR-049 #5, so the feed is redacted-by-construction.

package corpusdistill

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/kuhlman-labs/fishhawk/backend/internal/agenteval"
)

// AuditItem is the minimal audit-endpoint item shape the plan-review-miss
// distiller consumes ({sequence, run_id, ts, payload} from the
// GET /v0/runs/{run_id}/audit items envelope).
type AuditItem struct {
	Sequence  int64           `json:"sequence"`
	RunID     string          `json:"run_id"`
	Timestamp string          `json:"ts"`
	Payload   json.RawMessage `json:"payload"`
}

// MissOptions configures a DistillPlanReviewMiss / PreviewPlanReviewMiss
// call. The fields mirror trace-mode Options where they overlap.
type MissOptions struct {
	// CaseName is the base case slug; when a run carries several class-3
	// decisions the second and later cases get a -2, -3, … suffix. Required.
	CaseName string
	// Issue is the originating issue/run reference recorded in case.md.
	// Required for provenance.
	Issue string
	// OutDir is the corpus parent directory. Required (the command layer
	// resolves a default).
	OutDir string
	// Force permits overwriting an existing case directory.
	Force bool
	// Fetched reports that the items came from FetchRunTriageAudit (the
	// --run-id path), whose payloads carry only the structured verdict
	// fields — evidence blobs stay customer-side per ADR-049 #5 — so
	// case.md can assert the PRODUCTION redacted-by-construction
	// provenance. Operator-supplied items (--in/stdin) get a TODO prompt.
	Fetched bool
	// Narrative is the optional operator-supplied distilled-signal
	// explanation pre-filled into case.md (else a TODO prompt).
	Narrative string
}

// MissCaseResult describes one would-be plan-review-miss corpus case.
type MissCaseResult struct {
	CaseDir  string
	MissJSON []byte
	CaseMD   string
	Case     agenteval.PlanReviewMissCase
}

// missTriagePayload is the acceptance_triage_decided payload subset the
// distiller reads. PlanReviewMiss uses the SAME agenteval type the server
// marshals, so the server → tool seam cannot drift.
type missTriagePayload struct {
	RunID          string                     `json:"run_id"`
	ArtifactID     string                     `json:"artifact_id"`
	Class          string                     `json:"class"`
	Disposition    string                     `json:"disposition"`
	Reason         string                     `json:"reason"`
	PlanReviewMiss []agenteval.PlanReviewMiss `json:"plan_review_miss"`
}

// DistillPlanReviewMiss filters items to class-3 acceptance-triage decisions
// carrying plan_review_miss and writes one corpus case per decision under
// OutDir (<case-name>[-N]/{miss.json, case.md}), returning the written case
// dirs. Fail-loud contract: an undecodable payload is an error naming the
// item's sequence, and zero eligible decisions is an error, never an empty
// success.
func DistillPlanReviewMiss(items []AuditItem, opts MissOptions) ([]string, error) {
	results, err := preparePlanReviewMiss(items, opts)
	if err != nil {
		return nil, err
	}
	dirs := make([]string, 0, len(results))
	for _, res := range results {
		if _, statErr := os.Stat(res.CaseDir); statErr == nil {
			if !opts.Force {
				return nil, fmt.Errorf("corpusdistill: case dir %q already exists; pass --force to overwrite", res.CaseDir)
			}
			if rmErr := os.RemoveAll(res.CaseDir); rmErr != nil {
				return nil, fmt.Errorf("corpusdistill: remove existing case dir %q: %w", res.CaseDir, rmErr)
			}
		} else if !os.IsNotExist(statErr) {
			return nil, fmt.Errorf("corpusdistill: stat case dir %q: %w", res.CaseDir, statErr)
		}
		if err := os.MkdirAll(res.CaseDir, 0o755); err != nil {
			return nil, fmt.Errorf("corpusdistill: create case dir %q: %w", res.CaseDir, err)
		}
		if err := os.WriteFile(filepath.Join(res.CaseDir, "miss.json"), res.MissJSON, 0o644); err != nil {
			return nil, fmt.Errorf("corpusdistill: write miss.json: %w", err)
		}
		if err := os.WriteFile(filepath.Join(res.CaseDir, "case.md"), []byte(res.CaseMD), 0o644); err != nil {
			return nil, fmt.Errorf("corpusdistill: write case.md: %w", err)
		}
		dirs = append(dirs, res.CaseDir)
	}
	return dirs, nil
}

// PreviewPlanReviewMiss computes the would-be cases exactly as
// DistillPlanReviewMiss does but writes NOTHING to the filesystem — the
// --dry-run path. It surfaces the same validation errors (unsafe case name,
// undecodable payload, zero class-3 decisions).
func PreviewPlanReviewMiss(items []AuditItem, opts MissOptions) ([]MissCaseResult, error) {
	return preparePlanReviewMiss(items, opts)
}

// preparePlanReviewMiss performs the filter/convert/render work shared by
// DistillPlanReviewMiss and PreviewPlanReviewMiss, with no filesystem effect.
func preparePlanReviewMiss(items []AuditItem, opts MissOptions) ([]MissCaseResult, error) {
	if opts.CaseName == "" {
		return nil, fmt.Errorf("corpusdistill: CaseName is required")
	}
	if err := validateCaseName(opts.CaseName); err != nil {
		return nil, err
	}
	if opts.Issue == "" {
		return nil, fmt.Errorf("corpusdistill: Issue is required")
	}
	if opts.OutDir == "" {
		return nil, fmt.Errorf("corpusdistill: OutDir is required")
	}

	var results []MissCaseResult
	class3 := 0
	for _, item := range items {
		var p missTriagePayload
		if err := json.Unmarshal(item.Payload, &p); err != nil {
			return nil, fmt.Errorf("corpusdistill: decode acceptance_triage_decided payload at sequence %d: %w", item.Sequence, err)
		}
		if p.Class != "3" {
			continue
		}
		class3++
		if len(p.PlanReviewMiss) == 0 {
			// A class-3 entry written by a pre-#1539 backend carries no
			// plan_review_miss record; there is nothing to scaffold from it.
			continue
		}
		runID := p.RunID
		if runID == "" {
			runID = item.RunID
		}
		c := agenteval.PlanReviewMissCase{
			RunID:          runID,
			ArtifactID:     p.ArtifactID,
			TriageSequence: item.Sequence,
			Class:          p.Class,
			Disposition:    p.Disposition,
			Reason:         p.Reason,
			DecidedAt:      item.Timestamp,
			Misses:         p.PlanReviewMiss,
			Synthetic:      false,
		}
		name := opts.CaseName
		if len(results) > 0 {
			name = fmt.Sprintf("%s-%d", opts.CaseName, len(results)+1)
		}
		missJSON, err := json.MarshalIndent(c, "", "  ")
		if err != nil {
			return nil, fmt.Errorf("corpusdistill: marshal miss.json for sequence %d: %w", item.Sequence, err)
		}
		missJSON = append(missJSON, '\n')
		results = append(results, MissCaseResult{
			CaseDir:  filepath.Join(opts.OutDir, name),
			MissJSON: missJSON,
			CaseMD:   renderMissCaseMD(name, opts, c),
			Case:     c,
		})
	}
	if len(results) == 0 {
		if class3 > 0 {
			return nil, fmt.Errorf("corpusdistill: %d class-3 triage entr%s found but none carry plan_review_miss (written by a pre-#1539 backend?); nothing to scaffold", class3, plural(class3))
		}
		return nil, fmt.Errorf("corpusdistill: no class-3 acceptance_triage_decided entries in the input; nothing to scaffold")
	}
	return results, nil
}

func plural(n int) string {
	if n == 1 {
		return "y"
	}
	return "ies"
}

// renderMissCaseMD produces the plan-review-miss case.md template: the
// provenance block, the triage envelope, and the operator-curation sections.
func renderMissCaseMD(name string, opts MissOptions, c agenteval.PlanReviewMissCase) string {
	narrative := opts.Narrative
	if narrative == "" {
		narrative = `TODO(operator): describe why the plan-review gate should have challenged
this criterion — what grounding, testability, or scoping question would
have caught it before approval — and confirm the prose fields carry no
sensitive target detail before this case lands (#819 / ADR-040).`
	}
	ids := make([]string, 0, len(c.Misses))
	for _, m := range c.Misses {
		ids = append(ids, m.CriterionID)
	}
	idsJSON, _ := json.Marshal(ids)
	return fmt.Sprintf(`# Case: %s

%s

Scaffolded by `+"`fishhawk-distill-corpus --plan-review-miss`"+` (#1539). A
class-3 acceptance-triage decision: a failed criterion was inferred-source
or unresolvable against the approved plan — a bad criterion the plan-review
gate approved (ADR-049 decision #4). Selection, labeling, and committing
stay operator curation (#819 / ADR-040).

## Triage decision

Run %s, triage sequence %d, disposition `+"`%s`"+`.
Criterion ids: %s.
Reason: %s

## Distilled signal

%s
`, name, missProvenanceBlock(opts), c.RunID, c.TriageSequence, c.Disposition,
		string(idsJSON), c.Reason, narrative)
}

// missProvenanceBlock returns the case.md provenance paragraph. The
// --run-id fetch path can assert the PRODUCTION redacted-by-construction
// provenance (only structured verdict fields cross the audit payload —
// evidence blobs remain customer-side per ADR-049 #5); operator-supplied
// items get a TODO the operator must resolve.
func missProvenanceBlock(opts MissOptions) string {
	if opts.Fetched {
		return fmt.Sprintf(`**Provenance: PRODUCTION.** This case was distilled from a real Fishhawk
acceptance-triage audit entry (%s) fetched from the backend. It carries
structured verdict fields only — evidence blobs remain customer-side per
ADR-049 #5 (redacted-by-construction).`, opts.Issue)
	}
	return fmt.Sprintf(`**Provenance: TODO(operator).** This case was scaffolded from
operator-supplied audit items (%s) via `+"`--in`/stdin"+`, so the tool cannot
assert their origin. If these items came from a real run's audit feed,
replace this line with "Provenance: PRODUCTION" and note the
structured-fields-only redaction posture (ADR-049 #5); if they are
hand-authored, state that instead.`, opts.Issue)
}
