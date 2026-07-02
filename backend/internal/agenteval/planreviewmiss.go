// Plan-review-miss corpus types (E31.11 / #1539, ADR-049 decision #4).
//
// A class-3 acceptance-triage decision means a failed criterion was
// inferred-source or unresolvable against the approved plan — a bad
// criterion the plan gate approved. PlanReviewMiss is the shared wire
// type for that per-criterion record: the server marshals it into the
// acceptance_triage_decided audit payload's plan_review_miss field, the
// fishhawk-distill-corpus tool unmarshals it from fetched audit items,
// and LoadPlanReviewMissCorpus reads it back from committed corpus cases
// — one type, so the three surfaces cannot drift.
//
// This file imports ONLY the standard library so backend/internal/server
// can import agenteval without a cycle.

package agenteval

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// PlanReviewMiss is one per-criterion plan-review-miss record: the join of
// an approved-plan acceptance criterion (provenance fields) with the
// acceptance verdict's observed behavior for that criterion id. Only
// structured verdict fields cross — evidence blobs stay customer-side per
// ADR-049 decision refinement #5, so the record is redacted-by-construction.
type PlanReviewMiss struct {
	// CriterionID is the plan-criterion join key (E31.1). Always set, even
	// when the id is unresolvable against the plan (the provenance fields
	// are then empty but the observed behavior still carries).
	CriterionID string `json:"criterion_id"`
	// Statement / Source / SourceRef / Rationale are the approved plan
	// criterion's provenance fields (plan.AcceptanceCriterion). Empty when
	// the failed id did not resolve against the plan.
	Statement string `json:"statement,omitempty"`
	Source    string `json:"source,omitempty"`
	SourceRef string `json:"source_ref,omitempty"`
	Rationale string `json:"rationale,omitempty"`
	// Observed / Expected / StepsTaken / ExpectationBasis / ReproHandle /
	// Result are the verdict's per-criterion evidence fields for this id
	// (the E31.7 acceptance criterion result shape).
	Observed         string `json:"observed,omitempty"`
	Expected         string `json:"expected,omitempty"`
	StepsTaken       string `json:"steps_taken,omitempty"`
	ExpectationBasis string `json:"expectation_basis,omitempty"`
	ReproHandle      string `json:"repro_handle,omitempty"`
	Result           string `json:"result,omitempty"`
}

// PlanReviewMissCase is the miss.json shape of one plan-review-miss corpus
// case: the triage decision's identity + disposition envelope plus its
// per-criterion miss records.
type PlanReviewMissCase struct {
	RunID      string `json:"run_id"`
	ArtifactID string `json:"artifact_id,omitempty"`
	// TriageSequence is the acceptance_triage_decided audit entry's chain
	// sequence, so a committed case stays traceable to its source entry.
	TriageSequence int64  `json:"triage_sequence,omitempty"`
	Class          string `json:"class"`
	Disposition    string `json:"disposition,omitempty"`
	Reason         string `json:"reason,omitempty"`
	// DecidedAt is the triage entry's RFC 3339 timestamp.
	DecidedAt string           `json:"decided_at,omitempty"`
	Misses    []PlanReviewMiss `json:"misses"`
	// Synthetic marks a hand-authored seed case (the Tier-A/Tier-B seed
	// discipline) as opposed to a distilled production triage entry.
	Synthetic bool `json:"synthetic"`
}

// validate is the fail-closed shape gate for a loaded case: a case with no
// miss records, or a miss with an empty criterion id, is malformed.
func (c *PlanReviewMissCase) validate() error {
	if len(c.Misses) == 0 {
		return fmt.Errorf("misses must be non-empty")
	}
	for i, m := range c.Misses {
		if m.CriterionID == "" {
			return fmt.Errorf("misses[%d].criterion_id is required", i)
		}
	}
	return nil
}

// NamedPlanReviewMissCase pairs a loaded case with its corpus directory name.
type NamedPlanReviewMissCase struct {
	Name string
	Case PlanReviewMissCase
}

// LoadPlanReviewMissCorpus walks dir/<case>/miss.json and returns the parsed
// cases in directory order. An absent dir returns an empty slice and nil
// error — the corpus starts empty in most checkouts. Malformed JSON, a
// missing miss.json in a case directory, or a shape-gate failure (no misses,
// empty criterion id) is an error naming the case, fail-closed.
func LoadPlanReviewMissCorpus(dir string) ([]NamedPlanReviewMissCase, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("agenteval: read plan-review-miss corpus dir %q: %w", dir, err)
	}
	var out []NamedPlanReviewMissCase
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(dir, e.Name(), "miss.json"))
		if err != nil {
			return nil, fmt.Errorf("agenteval: plan-review-miss case %q: read miss.json: %w", e.Name(), err)
		}
		var c PlanReviewMissCase
		if err := json.Unmarshal(raw, &c); err != nil {
			return nil, fmt.Errorf("agenteval: plan-review-miss case %q: parse miss.json: %w", e.Name(), err)
		}
		if err := c.validate(); err != nil {
			return nil, fmt.Errorf("agenteval: plan-review-miss case %q: %w", e.Name(), err)
		}
		out = append(out, NamedPlanReviewMissCase{Name: e.Name(), Case: c})
	}
	return out, nil
}
