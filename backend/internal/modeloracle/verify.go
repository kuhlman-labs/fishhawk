package modeloracle

import (
	"context"
	"fmt"
	"sort"
	"strings"
)

// ModelStatus is the verdict Verify returns for one (provider, model) pair. It
// is the single shared vocabulary the run-create validation (spec.ValidateModels),
// the reviewer-set resolution (planReviewerSet.For), and the doctor reviewers
// rung all answer against, so an id is judged the same way wherever it is seen.
type ModelStatus string

const (
	// ModelVerified — the id is present in a FRESH, available snapshot: a real,
	// currently-served model for the provider.
	ModelVerified ModelStatus = "verified"
	// ModelRejected — the id is ABSENT from a fresh, available snapshot:
	// authoritatively not a served model (a typo or a sunset id both land here).
	ModelRejected ModelStatus = "rejected"
	// ModelUnverifiable — no authoritative answer: the oracle is nil, has no
	// snapshot (ok=false), or has a stale one (fresh=false). The caller MUST fail
	// open — accept the id and warn — because absence from a non-authoritative
	// list cannot reject.
	ModelUnverifiable ModelStatus = "unverifiable"
)

// Verdict is the structured result of Verify. Suggestion and Available are
// populated only on ModelRejected: Suggestion is the closest available id by
// edit distance (empty when nothing is a near miss) and Available is the sorted
// served set, so a caller can render a did-you-mean and list the alternatives.
type Verdict struct {
	Status     ModelStatus
	Provider   string
	Model      string
	Suggestion string
	Available  []string
}

// Verify asks the oracle whether model is a currently-served model for provider
// and returns the shared verdict every consumer keys on:
//
//   - oracle == nil, ok == false, or fresh == false → ModelUnverifiable (fail open).
//   - fresh && ok && model present → ModelVerified.
//   - fresh && ok && model ABSENT → ModelRejected, with a did-you-mean Suggestion
//     when a near miss exists and the sorted Available set.
//
// It centralizes the routing spec.ValidateModels performed inline before #3578,
// so the run path, the reviewer-set resolution, and the doctor rung cannot drift
// apart on what "a valid model" means.
func Verify(ctx context.Context, oracle ModelOracle, provider, model string) Verdict {
	v := Verdict{Provider: provider, Model: model}
	if oracle == nil {
		v.Status = ModelUnverifiable
		return v
	}
	models, fresh, ok := oracle.Snapshot(ctx, provider)
	if !ok || !fresh {
		v.Status = ModelUnverifiable
		return v
	}
	if modelInSet(model, models) {
		v.Status = ModelVerified
		return v
	}
	v.Status = ModelRejected
	v.Suggestion = suggest(model, models)
	sorted := append([]string(nil), models...)
	sort.Strings(sorted)
	v.Available = sorted
	return v
}

// RejectMessage renders the hard-error sentence for a ModelRejected verdict:
// the rejected model, a did-you-mean suggestion when a near-miss exists, and the
// available set. It reproduces EXACTLY the string spec.modelRejectMessage
// produced before this logic moved here (`model %q is not a known %q model (did
// you mean %q?); available: a, b`), so spec.ValidateModels's error text — and
// the tests pinning it — stay byte-for-byte unchanged.
func (v Verdict) RejectMessage() string {
	var b strings.Builder
	fmt.Fprintf(&b, "model %q is not a known %q model", v.Model, v.Provider)
	if v.Suggestion != "" {
		fmt.Fprintf(&b, " (did you mean %q?)", v.Suggestion)
	}
	if len(v.Available) > 0 {
		fmt.Fprintf(&b, "; available: %s", strings.Join(v.Available, ", "))
	} else {
		b.WriteString("; available: (none)")
	}
	return b.String()
}

// RejectedError wraps a ModelRejected Verdict so a caller that receives a
// reviewer-resolution error across a package boundary (the reviewer set lives in
// package main, the doctor rung in package server — they cannot share a concrete
// type) can recover the STRUCTURED verdict via errors.As: the RESOLVED model
// (which the caller may not otherwise know — e.g. a deployment default the spec
// omitted), the did-you-mean suggestion, and the available set (#3578).
// planReviewerSet.For wraps it; the doctor reviewers rung unwraps it to render
// model_status/model_hint/priced from the resolved model.
type RejectedError struct {
	Verdict Verdict
}

// Error renders the reject sentence (the same one Verdict.RejectMessage
// produces), so a wrapped RejectedError reads identically to the pre-typed error.
func (e RejectedError) Error() string { return e.Verdict.RejectMessage() }

// modelInSet reports membership of model in available.
func modelInSet(model string, available []string) bool {
	for _, m := range available {
		if m == model {
			return true
		}
	}
	return false
}

// suggest returns the closest available model to model by Levenshtein distance,
// or "" when nothing is within a conservative edit-distance budget (so an
// unrelated typo doesn't get a misleading suggestion). The budget scales with
// the candidate length: up to a third of the longer string's length, capped.
func suggest(model string, available []string) string {
	best := ""
	bestDist := 1 << 30
	for _, cand := range available {
		d := levenshtein(model, cand)
		if d < bestDist {
			bestDist = d
			best = cand
		}
	}
	if best == "" {
		return ""
	}
	budget := len(best) / 3
	if budget < 2 {
		budget = 2
	}
	if budget > 8 {
		budget = 8
	}
	if bestDist > budget {
		return ""
	}
	return best
}

// levenshtein computes the edit distance between a and b (classic two-row
// dynamic program). No such helper exists elsewhere in the repo.
func levenshtein(a, b string) int {
	ra, rb := []rune(a), []rune(b)
	if len(ra) == 0 {
		return len(rb)
	}
	if len(rb) == 0 {
		return len(ra)
	}
	prev := make([]int, len(rb)+1)
	curr := make([]int, len(rb)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(ra); i++ {
		curr[0] = i
		for j := 1; j <= len(rb); j++ {
			cost := 1
			if ra[i-1] == rb[j-1] {
				cost = 0
			}
			curr[j] = min3(prev[j]+1, curr[j-1]+1, prev[j-1]+cost)
		}
		prev, curr = curr, prev
	}
	return prev[len(rb)]
}

func min3(a, b, c int) int {
	m := a
	if b < m {
		m = b
	}
	if c < m {
		m = c
	}
	return m
}
