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
	MissingChecksTruncated          = "checks_truncated"           // the listing was incomplete
	MissingObservationFailed        = "observation_failed"         // the forge stayed unobservable past the terminal deadline
)

// Per-check missing reasons (missing_check_reasons values).
const (
	checkReasonAbsent       = "absent"
	checkReasonNotConcluded = "not_concluded_by_maturity"
	checkReasonUnconcluded  = "unconcluded"
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
)

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
	// not_concluded_by_maturity or unconcluded.
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
			if len(relevantAttempts(attempts, maturesAt)) > 0 {
				names = append(names, name)
			}
		}
		sort.Strings(names)
	}

	firstMissing := ""
	for _, name := range names {
		outcome, reason := evaluateCheck(byName[name], maturesAt)
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
	default:
		return MissingRequiredCheckUnconcluded
	}
}

// relevantAttempts drops the attempts started after maturesAt. An attempt
// with no started_at (queued) is kept: it did not start late.
func relevantAttempts(attempts []githubclient.CheckRunSummary, maturesAt time.Time) []githubclient.CheckRunSummary {
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
func evaluateCheck(attempts []githubclient.CheckRunSummary, maturesAt time.Time) (string, string) {
	if len(attempts) == 0 {
		return checkOutcomeMissing, checkReasonAbsent
	}
	suites := map[int64][]githubclient.CheckRunSummary{}
	var suiteIDs []int64
	for _, a := range relevantAttempts(attempts, maturesAt) {
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
		outcome, reason := evaluateSuite(suites[id], maturesAt)
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
// attempt completed by maturesAt (check-run ids increase with creation).
func evaluateSuite(attempts []githubclient.CheckRunSummary, maturesAt time.Time) (string, string) {
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
