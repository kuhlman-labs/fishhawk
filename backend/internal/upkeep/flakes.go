package upkeep

// Flake aggregation (#3922): the deterministic detector the upkeep scan's flake
// source runs over the verify attempts the server read from recorded runs'
// redacted gate evidence. It is pure — the caller adapts each stage's
// bundle.GateEvidence into a FlakeStage — and it returns SUBJECTS only, never
// the raw output tail.

import (
	"regexp"
	"sort"
	"strings"
)

// Verify attempt outcomes, mirroring bundle.VerifyRunEvidence.Outcome.
const (
	VerifyOutcomePassed  = "passed"
	VerifyOutcomeFailed  = "failed"
	VerifyOutcomeSkipped = "skipped"
)

// Flake subjects and caps. A subject is the finding subject, so a flake
// finding's id is `flake:<subject>`.
const (
	// FlakeSubjectUnnamed is the subject of a flaky failure whose output tail
	// names no test, or names one outside the test-name charset.
	FlakeSubjectUnnamed = "verify-gate:unnamed"
	// FlakeSubjectInfra is the subject of the runner's absorbed verify infra
	// flake retries (bundle.GateEvidence.FlakeRetries).
	FlakeSubjectInfra = "verify-gate:infra"
	// FlakeMaxRefs caps the refs one Flake carries; the rest are counted in
	// OmittedRefs.
	FlakeMaxRefs = 20
	// FlakeMaxSubjects caps how many subjects CapFlakeSubjects keeps.
	FlakeMaxSubjects = 32
)

// VerifyAttempt is one verify run of a stage, in run order.
type VerifyAttempt struct {
	// TreeSHA is the tree the attempt verified; empty when unrecorded.
	TreeSHA string
	// Outcome is one of the VerifyOutcome* constants.
	Outcome string
	// OutputTail is the attempt's bounded, pre-redacted output tail.
	OutputTail string
}

// FlakeStage is one recorded stage's verify history.
type FlakeStage struct {
	RunID   string
	StageID string
	// Attempts are the stage's verify runs, in the order they ran.
	Attempts []VerifyAttempt
	// InfraRetries counts the runner's absorbed verify infra-flake retries.
	InfraRetries int
}

// FlakeRef cites one stage a flake was seen in.
type FlakeRef struct {
	RunID   string
	StageID string
}

// Flake is one subject aggregated across stages.
type Flake struct {
	Subject string
	// Occurrences is the number of flaky failures (or infra retries) seen.
	Occurrences int
	// Refs are the distinct stages cited, sorted by (RunID, StageID), at most
	// FlakeMaxRefs of them.
	Refs []FlakeRef
	// OmittedRefs counts the distinct refs past FlakeMaxRefs.
	OmittedRefs int
}

var (
	// failLineRE captures the whole first token after `--- FAIL: `.
	failLineRE = regexp.MustCompile(`(?m)^[ \t]*--- FAIL: (\S+)`)
	// testNameRE is the charset a top-level test name must match IN FULL.
	testNameRE = regexp.MustCompile(`^[A-Za-z0-9_]+$`)
)

// AggregateFlakes returns the flakes in stages, sorted by Occurrences
// descending, then Subject ascending. It never returns nil.
//
// Rule: within ONE stage, a failed attempt counts as one flaky failure when a
// LATER attempt passed on the identical, non-empty TreeSHA. A failure followed
// by a pass on a different tree, an attempt with no recorded tree, or a pass
// followed by a failure yields nothing. Each counted failure adds one
// occurrence to every subject its output tail names (see failedTestSubjects).
// A stage with InfraRetries > 0 adds that many occurrences to
// FlakeSubjectInfra.
//
// Across stages, occurrences are summed per subject and each subject's refs
// are the distinct stages it was seen in, sorted and capped at FlakeMaxRefs.
// The output does not depend on the order of stages.
func AggregateFlakes(stages []FlakeStage) []Flake {
	type agg struct {
		occurrences int
		refs        map[FlakeRef]bool
	}
	bySubject := map[string]*agg{}
	add := func(subject string, n int, ref FlakeRef) {
		a := bySubject[subject]
		if a == nil {
			a = &agg{refs: map[FlakeRef]bool{}}
			bySubject[subject] = a
		}
		a.occurrences += n
		a.refs[ref] = true
	}

	for _, st := range stages {
		ref := FlakeRef{RunID: st.RunID, StageID: st.StageID}
		for i, att := range st.Attempts {
			if !flakyFailure(st.Attempts, i) {
				continue
			}
			for _, subject := range failedTestSubjects(att.OutputTail) {
				add(subject, 1, ref)
			}
		}
		if st.InfraRetries > 0 {
			add(FlakeSubjectInfra, st.InfraRetries, ref)
		}
	}

	out := make([]Flake, 0, len(bySubject))
	for subject, a := range bySubject {
		refs := make([]FlakeRef, 0, len(a.refs))
		for r := range a.refs {
			refs = append(refs, r)
		}
		sort.Slice(refs, func(i, j int) bool {
			if refs[i].RunID != refs[j].RunID {
				return refs[i].RunID < refs[j].RunID
			}
			return refs[i].StageID < refs[j].StageID
		})
		omitted := 0
		if len(refs) > FlakeMaxRefs {
			omitted = len(refs) - FlakeMaxRefs
			refs = refs[:FlakeMaxRefs]
		}
		out = append(out, Flake{Subject: subject, Occurrences: a.occurrences, Refs: refs, OmittedRefs: omitted})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Occurrences != out[j].Occurrences {
			return out[i].Occurrences > out[j].Occurrences
		}
		return out[i].Subject < out[j].Subject
	})
	return out
}

// CapFlakeSubjects keeps the first FlakeMaxSubjects of flakes (AggregateFlakes
// order puts the most frequent first) and returns how many it dropped, so the
// caller can disclose the truncation.
func CapFlakeSubjects(flakes []Flake) ([]Flake, int) {
	if len(flakes) <= FlakeMaxSubjects {
		return flakes, 0
	}
	return flakes[:FlakeMaxSubjects:FlakeMaxSubjects], len(flakes) - FlakeMaxSubjects
}

// flakyFailure reports whether attempts[i] failed on a recorded tree and a
// LATER attempt passed on that same tree.
func flakyFailure(attempts []VerifyAttempt, i int) bool {
	failed := attempts[i]
	if failed.Outcome != VerifyOutcomeFailed || failed.TreeSHA == "" {
		return false
	}
	for j := i + 1; j < len(attempts); j++ {
		if later := attempts[j]; later.Outcome == VerifyOutcomePassed && later.TreeSHA == failed.TreeSHA {
			return true
		}
	}
	return false
}

// failedTestSubjects returns the distinct subjects of one failed attempt's
// output tail, in first-seen order.
//
// Each `--- FAIL: <name>` line contributes the TOP-LEVEL test of <name> (a
// subtest `TestA/case` collapses to TestA). The top-level name must match
// [A-Za-z0-9_] IN FULL: `TestBad-Name` is not truncated to TestBad, it
// contributes FlakeSubjectUnnamed. A tail with no FAIL line yields only
// FlakeSubjectUnnamed.
func failedTestSubjects(tail string) []string {
	matches := failLineRE.FindAllStringSubmatch(tail, -1)
	if len(matches) == 0 {
		return []string{FlakeSubjectUnnamed}
	}
	seen := map[string]bool{}
	var out []string
	for _, m := range matches {
		name, _, _ := strings.Cut(m[1], "/")
		if !testNameRE.MatchString(name) {
			name = FlakeSubjectUnnamed
		}
		if !seen[name] {
			seen[name] = true
			out = append(out, name)
		}
	}
	return out
}
