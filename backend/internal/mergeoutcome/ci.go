package mergeoutcome

import (
	"sort"
	"time"

	"github.com/kuhlman-labs/fishhawk/backend/internal/auditcheckpublisher"
	"github.com/kuhlman-labs/fishhawk/backend/internal/githubclient"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

// Merge-commit CI conclusions recorded on run_merge_ci_observed.
const (
	CIConclusionGreen   = "green"
	CIConclusionRed     = "red"
	CIConclusionMissing = "missing"
)

// Verdict-level missing reasons: why a conclusion is `missing`.
const (
	MissingRequiredChecksUnknown    = "required_checks_unknown"    // the run has no required-checks snapshot
	MissingNoChecksReported         = "no_checks_reported"         // nothing required and nothing ran
	MissingRequiredCheckAbsent      = "required_check_absent"      // a required check never ran on the merge commit
	MissingNotConcludedByMaturity   = "not_concluded_by_maturity"  // a check had no attempt completed by matures_at
	MissingRequiredCheckUnconcluded = "required_check_unconcluded" // cancelled, stale, skipped-by-forge or an unknown conclusion
	MissingRequiredCheckPending     = "required_check_pending"     // ClassifyMergeGateCI only: a suite's latest attempt has not completed
	MissingChecksTruncated          = "checks_truncated"           // the listing was incomplete
	MissingObservationFailed        = "observation_failed"         // the forge stayed unobservable past the terminal deadline
)

// Per-check missing reasons (missing_check_reasons values).
const (
	checkReasonAbsent       = "absent"
	checkReasonNotConcluded = "not_concluded_by_maturity"
	checkReasonUnconcluded  = "unconcluded"
	checkReasonPending      = "pending" // ClassifyMergeGateCI only
	checkOutcomePassed      = "passed"
	checkOutcomeFailed      = "failed"
	checkOutcomeMissing     = "missing"
	checkStatusCompleted    = "completed"
	requiredSourceSnapshot  = "run_snapshot"
	requiredSourceEmpty     = "run_snapshot_empty"
	requiredSourceAbsent    = "run_snapshot_absent"
)

// passConclusions / failConclusions are the completed-check conclusions that
// count. Anything else — cancelled, stale, an empty or unknown conclusion — is
// missing, never green.
var (
	passConclusions = map[string]bool{"success": true, "neutral": true, "skipped": true}
	failConclusions = map[string]bool{"failure": true, "timed_out": true, "action_required": true, "startup_failure": true}
	// gateFailConclusions is ClassifyMergeGateCI's fail set: failConclusions
	// plus cancelled, which is terminal at the PR-head merge gate (docs/adr/112
	// A3.1(a)). Superseded- or cancelled-run tolerance is deliberately NOT
	// adopted there.
	gateFailConclusions = map[string]bool{"failure": true, "timed_out": true, "action_required": true, "startup_failure": true,
		"cancelled": true}
)

// ciRules selects the attempt rules the shared classifier body applies.
// The zero value is ClassifyMergeCI's (E82.2's post-merge observation); gate
// is ClassifyMergeGateCI's (the PR-head merge gate, docs/adr/112 A3.1).
type ciRules struct {
	// gate: no maturity cut-off; within a suite the attempt with the highest
	// check-run id decides whether or not it completed (a not-completed one
	// is missing/pending); cancelled fails.
	gate bool
}

// excludedChecks never run on a merge commit, so requiring them there would
// make every merge `missing`: Fishhawk's own PR-head merge gate.
var excludedChecks = []string{auditcheckpublisher.CheckName}

// CIVerdict is the merge commit's CI conclusion fixed at maturity.
type CIVerdict struct {
	Conclusion           string
	MissingReason        string // empty unless Conclusion is missing
	RequiredChecks       []string
	RequiredChecksSource string
	ExcludedChecks       []string
	PassedChecks         []string
	FailedChecks         []string
	MissingChecks        []string
	// MissingCheckReasons maps each missing check name to absent,
	// not_concluded_by_maturity or unconcluded (ClassifyMergeCI), or absent,
	// pending or unconcluded (ClassifyMergeGateCI).
	MissingCheckReasons map[string]string
	ChecksObserved      int
	ChecksTruncated     bool
}

// ClassifyMergeCI fixes the merge commit's CI conclusion as of maturesAt
// (operator condition 3) against the run's server-captured required-checks
// snapshot.
//
// Per check name, attempts are grouped by check suite: a re-run lands in the
// same suite, so within a suite the MOST RECENT attempt (highest check-run
// id) whose completed_at is at or before maturesAt decides; attempts started
// after maturesAt are ignored, and a suite with attempts but none completed
// by maturesAt is missing (not_concluded_by_maturity) even if one later
// succeeded. The same name in several suites (another workflow, another app)
// is a duplicate: any failed suite fails the name, else any missing suite
// makes it missing.
//
// The required set is snapshot.Contexts minus the Fishhawk merge gate. The
// verdict is, in order: missing (required_checks_unknown) on a nil snapshot;
// missing (checks_truncated) on an incomplete listing; red if any required
// check failed; missing if any is missing; green otherwise. An empty required
// set (authoritatively empty, or emptied by the exclusion) evaluates the
// observed set instead, and nothing observed is missing (no_checks_reported).
func ClassifyMergeCI(snapshot *run.RequiredChecksSnapshot, checks []githubclient.CheckRunSummary, truncated bool, maturesAt time.Time) CIVerdict {
	return classifyCI(snapshot, checks, truncated, maturesAt, ciRules{})
}

// ClassifyMergeGateCI is the PR-head MERGE-GATE variant of ClassifyMergeCI
// (docs/adr/112 A3.1, #4780): it judges each required name per check suite on
// that suite's LATEST attempt (the highest check-run id), completed or not.
// A latest attempt that has not completed is missing with per-check reason
// pending (verdict reason required_check_pending), even beside an older
// completed success in the same suite. A completed latest attempt passes in
// success|neutral|skipped, FAILS in failure|timed_out|action_required|
// startup_failure|cancelled (cancelled is terminal: A3.1(a)), and is missing
// (unconcluded) on any other conclusion (stale, empty, unknown). Cross-suite
// aggregation, the required set, the exclusion of Fishhawk's own merge gate,
// the observed-set fallback and the verdict order are ClassifyMergeCI's: any
// failed suite fails the name (no superseded-run tolerance), and across names
// red beats missing.
//
// No time cut-off applies: the forge's attempt timestamps are never compared
// against now, so a clock skew between fishhawkd and the forge cannot hide a
// newer attempt. now is the observation instant the caller read the listing
// at; the gate rules make no decision on it.
//
// ClassifyMergeCI (and with it the CITicker's post-merge observation, E82.2)
// is unchanged by these rules.
func ClassifyMergeGateCI(snapshot *run.RequiredChecksSnapshot, checks []githubclient.CheckRunSummary, truncated bool, now time.Time) CIVerdict {
	return classifyCI(snapshot, checks, truncated, now, ciRules{gate: true})
}

// classifyCI is the body ClassifyMergeCI and ClassifyMergeGateCI share; rules
// selects the attempt rules (ciRules).
func classifyCI(snapshot *run.RequiredChecksSnapshot, checks []githubclient.CheckRunSummary, truncated bool, maturesAt time.Time, rules ciRules) CIVerdict {
	v := CIVerdict{
		ChecksObserved:      len(checks),
		ChecksTruncated:     truncated,
		RequiredChecks:      []string{},
		ExcludedChecks:      []string{},
		PassedChecks:        []string{},
		FailedChecks:        []string{},
		MissingChecks:       []string{},
		MissingCheckReasons: map[string]string{},
	}
	excluded := map[string]bool{}
	for _, n := range excludedChecks {
		excluded[n] = true
	}
	byName := map[string][]githubclient.CheckRunSummary{}
	for _, c := range checks {
		if excluded[c.Name] {
			continue
		}
		byName[c.Name] = append(byName[c.Name], c)
	}

	var names []string
	switch {
	case snapshot == nil:
		v.RequiredChecksSource = requiredSourceAbsent
	case len(snapshot.Contexts) == 0:
		v.RequiredChecksSource = requiredSourceEmpty
	default:
		v.RequiredChecksSource = requiredSourceSnapshot
		seen := map[string]bool{}
		for _, ctx := range snapshot.Contexts {
			if seen[ctx] {
				continue
			}
			seen[ctx] = true
			if excluded[ctx] {
				v.ExcludedChecks = append(v.ExcludedChecks, ctx)
				continue
			}
			v.RequiredChecks = append(v.RequiredChecks, ctx)
		}
		names = v.RequiredChecks
	}
	if len(names) == 0 {
		// Observed-set fallback (also the informational lists on a nil
		// snapshot): every name with an attempt started by maturity.
		for name, attempts := range byName {
			if len(rules.relevantAttempts(attempts, maturesAt)) > 0 {
				names = append(names, name)
			}
		}
		sort.Strings(names)
	}

	firstMissing := ""
	for _, name := range names {
		outcome, reason := evaluateCheck(byName[name], maturesAt, rules)
		switch outcome {
		case checkOutcomePassed:
			v.PassedChecks = append(v.PassedChecks, name)
		case checkOutcomeFailed:
			v.FailedChecks = append(v.FailedChecks, name)
		default:
			v.MissingChecks = append(v.MissingChecks, name)
			v.MissingCheckReasons[name] = reason
			if firstMissing == "" {
				firstMissing = reason
			}
		}
	}

	switch {
	case snapshot == nil:
		v.Conclusion, v.MissingReason = CIConclusionMissing, MissingRequiredChecksUnknown
	case truncated:
		v.Conclusion, v.MissingReason = CIConclusionMissing, MissingChecksTruncated
	case len(v.FailedChecks) > 0:
		v.Conclusion = CIConclusionRed
	case len(v.MissingChecks) > 0:
		v.Conclusion, v.MissingReason = CIConclusionMissing, verdictReason(firstMissing)
	case len(names) == 0:
		v.Conclusion, v.MissingReason = CIConclusionMissing, MissingNoChecksReported
	default:
		v.Conclusion = CIConclusionGreen
	}
	return v
}

// verdictReason maps a per-check missing reason to the verdict's.
func verdictReason(checkReason string) string {
	switch checkReason {
	case checkReasonAbsent:
		return MissingRequiredCheckAbsent
	case checkReasonNotConcluded:
		return MissingNotConcludedByMaturity
	case checkReasonPending:
		return MissingRequiredCheckPending
	default:
		return MissingRequiredCheckUnconcluded
	}
}

// relevantAttempts drops the attempts started after maturesAt. An attempt
// with no started_at (queued) is kept: it did not start late. The gate rules
// apply no cut-off and keep every attempt.
func (r ciRules) relevantAttempts(attempts []githubclient.CheckRunSummary, maturesAt time.Time) []githubclient.CheckRunSummary {
	if r.gate {
		return attempts
	}
	var out []githubclient.CheckRunSummary
	for _, a := range attempts {
		if a.StartedAt != nil && a.StartedAt.After(maturesAt) {
			continue
		}
		out = append(out, a)
	}
	return out
}

// evaluateCheck decides one check name across its suites: any failed suite
// fails it, else any missing suite makes it missing, else passed.
func evaluateCheck(attempts []githubclient.CheckRunSummary, maturesAt time.Time, rules ciRules) (string, string) {
	if len(attempts) == 0 {
		return checkOutcomeMissing, checkReasonAbsent
	}
	suites := map[int64][]githubclient.CheckRunSummary{}
	var suiteIDs []int64
	for _, a := range rules.relevantAttempts(attempts, maturesAt) {
		if _, ok := suites[a.CheckSuiteID]; !ok {
			suiteIDs = append(suiteIDs, a.CheckSuiteID)
		}
		suites[a.CheckSuiteID] = append(suites[a.CheckSuiteID], a)
	}
	if len(suiteIDs) == 0 {
		// Every attempt started after maturity.
		return checkOutcomeMissing, checkReasonNotConcluded
	}
	sort.Slice(suiteIDs, func(i, j int) bool { return suiteIDs[i] < suiteIDs[j] })
	anyFailed, missingReason := false, ""
	for _, id := range suiteIDs {
		outcome, reason := rules.evaluateSuite(suites[id], maturesAt)
		switch outcome {
		case checkOutcomeFailed:
			anyFailed = true
		case checkOutcomeMissing:
			if missingReason == "" {
				missingReason = reason
			}
		}
	}
	switch {
	case anyFailed:
		return checkOutcomeFailed, ""
	case missingReason != "":
		return checkOutcomeMissing, missingReason
	default:
		return checkOutcomePassed, ""
	}
}

// evaluateSuite decides one suite's attempts of a check: the most recent
// attempt completed by maturesAt (check-run ids increase with creation), or,
// under the gate rules, gateSuite's latest attempt.
func (r ciRules) evaluateSuite(attempts []githubclient.CheckRunSummary, maturesAt time.Time) (string, string) {
	if r.gate {
		return gateSuite(attempts)
	}
	var latest *githubclient.CheckRunSummary
	for i := range attempts {
		a := &attempts[i]
		if a.Status != checkStatusCompleted || a.CompletedAt == nil || a.CompletedAt.After(maturesAt) {
			continue
		}
		if latest == nil || a.ID > latest.ID {
			latest = a
		}
	}
	switch {
	case latest == nil:
		return checkOutcomeMissing, checkReasonNotConcluded
	case passConclusions[latest.Conclusion]:
		return checkOutcomePassed, ""
	case failConclusions[latest.Conclusion]:
		return checkOutcomeFailed, ""
	default:
		return checkOutcomeMissing, checkReasonUnconcluded
	}
}

// gateSuite decides one suite's attempts under the gate rules (docs/adr/112
// A3.1): the attempt with the highest check-run id decides, whether or not it
// completed. Not completed is missing (pending); completed passes, fails
// (gateFailConclusions, cancelled included) or is missing (unconcluded).
func gateSuite(attempts []githubclient.CheckRunSummary) (string, string) {
	var latest *githubclient.CheckRunSummary
	for i := range attempts {
		if latest == nil || attempts[i].ID > latest.ID {
			latest = &attempts[i]
		}
	}
	switch {
	case latest == nil || latest.Status != checkStatusCompleted:
		return checkOutcomeMissing, checkReasonPending
	case passConclusions[latest.Conclusion]:
		return checkOutcomePassed, ""
	case gateFailConclusions[latest.Conclusion]:
		return checkOutcomeFailed, ""
	default:
		return checkOutcomeMissing, checkReasonUnconcluded
	}
}
