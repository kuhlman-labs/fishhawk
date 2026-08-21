package workmgmt

import (
	"context"
	"sort"
	"sync"
)

// FeedbackReport is the resolved upstream product-feedback report to file
// (#1006): the rendered title and body, classification labels, and the
// dedup Fingerprint. The provider embeds the fingerprint as a hidden
// marker in the filed body so a later SearchOpenByFingerprint can find
// this report and append an occurrence instead of duplicating it.
type FeedbackReport struct {
	Title       string
	Body        string
	Labels      []string
	Fingerprint string
	// BoardPlacement is the desired project-board placement for a newly
	// filed report (#1737). It reaches the board only when the Target
	// carries a Project; placement is BEST-EFFORT exactly as it is on the
	// work-item path (#1107) — the filed report is the durable result, so a
	// placement failure records its cause and never fails the filing.
	BoardPlacement BoardPlacement
}

// Boarding-status values reported alongside a filed product report
// (#1737). They exist because `boarded=false` alone is ambiguous, and the
// two false cases call for DIFFERENT operator actions: nothing to do when
// no project is configured, versus investigate a real placement failure.
//
// "no project configured" is a CONFIGURATION STATE, not an error, so it
// carries its own status and leaves BoardingError empty; a genuine
// placement failure is the only case that sets BoardingError.
const (
	// BoardingStatusBoarded — the report was placed on the board.
	BoardingStatusBoarded = "boarded"
	// BoardingStatusNotAttemptedNoProject — the conventions declare no
	// project, so placement was never attempted. Not an error.
	BoardingStatusNotAttemptedNoProject = "not_attempted_no_project"
	// BoardingStatusFailed — placement was attempted and failed; the
	// cause is in BoardingError.
	BoardingStatusFailed = "failed"
	// BoardingStatusNotAttemptedNoReport — no new report was created (a
	// dedup hit appended an occurrence comment to an existing report), so
	// there was nothing to board.
	BoardingStatusNotAttemptedNoReport = "not_attempted_no_report"
	// BoardingStatusNotAttemptedProjectNotAuthorized — a project IS
	// configured, but its coordinates did not come from the destination
	// repo's own conventions, so placement was refused before any
	// privileged call. Also a configuration state, not an error: the
	// caller did nothing wrong and there is no cause to investigate.
	BoardingStatusNotAttemptedProjectNotAuthorized = "not_attempted_project_not_authorized"
)

// BoardingCause is the closed set of CALLER-SAFE placement-failure
// literals. A raw provider error is not safe to echo on a 2xx body: it
// carries third-party API text and, on the unknown-status path, the
// project's own field-option names — private board metadata reachable
// only through the operator's privileged token. So the wire carries a
// literal from this set naming WHICH STEP failed, and the raw cause goes
// to the server-side log the operator already reads. This mirrors the
// error-envelope posture (see writeError's default-deny detail
// allow-list): causes reach the operator, never the caller.
const (
	// BoardingCauseProjectFieldsUnavailable — resolving the project's
	// Status field failed (missing project, missing token, no access).
	BoardingCauseProjectFieldsUnavailable = "project_fields_unavailable"
	// BoardingCauseAddItemFailed — adding the issue to the project failed.
	BoardingCauseAddItemFailed = "add_project_item_failed"
	// BoardingCauseStatusOptionUnknown — the desired Status column is not
	// an option on the project's Status field.
	BoardingCauseStatusOptionUnknown = "status_option_unknown"
	// BoardingCauseSetStatusFailed — setting the Status field failed after
	// the item was added.
	BoardingCauseSetStatusFailed = "set_status_failed"
	// BoardingCauseUnclassified — placement failed in a way this table
	// does not recognize. The FAIL-SAFE default: an unmatched cause is
	// reported as unclassified rather than echoed, so a new or reworded
	// provider error can never become a disclosure by omission.
	BoardingCauseUnclassified = "placement_failed"
	// BoardingCauseUnreported — the provider reported neither placement
	// nor a cause. Synthesized so `failed` ALWAYS carries a cause, which
	// is what the docs and the response schema promise; a `failed` with an
	// empty cause is a state an operator cannot act on.
	BoardingCauseUnreported = "provider_reported_neither_placement_nor_cause"
)

// BoardingStatusOf classifies the board-placement outcome of a filed
// report into one of the closed statuses above. It is the single place
// the "boarded=false, why?" question is answered, so the response
// surface, the docs, and the tests cannot drift apart.
//
// A nil created item means nothing was filed (the dedup-hit path).
func BoardingStatusOf(target Target, created *CreatedItem) string {
	switch {
	case created == nil:
		return BoardingStatusNotAttemptedNoReport
	case target.Project == nil:
		return BoardingStatusNotAttemptedNoProject
	case created.BoardingError != "":
		return BoardingStatusFailed
	case created.Boarded:
		return BoardingStatusBoarded
	default:
		// A provider that neither boarded nor recorded a cause while a
		// project IS configured: report the failure status rather than
		// claiming success. The caller synthesizes
		// BoardingCauseUnreported for this arm, so `failed` never
		// reaches the wire cause-free — the docs and the response schema
		// both state that `failed` is the status that CARRIES a cause.
		return BoardingStatusFailed
	}
}

// ExistingReport is a previously-filed open upstream report that a dedup
// search matched on its fingerprint marker.
type ExistingReport struct {
	Number int
	URL    string
}

// FeedbackProvider files product-feedback reports to the FIXED upstream
// Fishhawk product repo, deduping by fingerprint. It is the egress
// counterpart to Provider (which files ordinary work items into a
// caller-chosen repo): a product report always lands in the product repo
// the Target names, and identical failures collapse onto one report.
//
//   - SearchOpenByFingerprint looks for an open report already carrying
//     the fingerprint marker; it returns nil (not an error) on a miss.
//   - File creates a new fingerprint-marked report.
//   - AppendOccurrence records another occurrence on an existing report.
//
// Like Provider, a FeedbackProvider is selected by id from a registry and
// implemented in a sibling package (workmgmt/github). An unregistered id
// fails closed via GetFeedback rather than dispatching against nil.
type FeedbackProvider interface {
	Name() string
	SearchOpenByFingerprint(ctx context.Context, target Target, fingerprint string) (*ExistingReport, error)
	File(ctx context.Context, target Target, report FeedbackReport) (*CreatedItem, error)
	AppendOccurrence(ctx context.Context, target Target, number int, note string) error
}

var (
	feedbackRegistryMu sync.RWMutex
	feedbackRegistry   = map[string]FeedbackProvider{}
)

// RegisterFeedback adds p to the global feedback-provider registry under
// p.Name(), replacing any prior registration for that id. The server
// wires the concrete provider (the GitHub product-feedback provider) at
// startup; tests register fakes. The registry is independent of the
// work-item Provider registry, so the same id (e.g. "github_projects")
// can name both a work-item and a feedback provider.
func RegisterFeedback(p FeedbackProvider) {
	feedbackRegistryMu.Lock()
	defer feedbackRegistryMu.Unlock()
	feedbackRegistry[p.Name()] = p
}

// GetFeedback returns the registered feedback provider for id, or an
// *UnknownProviderError naming id and the registered set. Callers MUST
// surface this error rather than dispatching against a nil provider.
func GetFeedback(id string) (FeedbackProvider, error) {
	feedbackRegistryMu.RLock()
	defer feedbackRegistryMu.RUnlock()
	p, ok := feedbackRegistry[id]
	if !ok {
		return nil, &UnknownProviderError{ID: id, Known: knownFeedbackIDsLocked()}
	}
	return p, nil
}

// RegisteredFeedback returns the sorted set of registered feedback
// provider ids — used by startup logging and the unknown-provider error.
func RegisteredFeedback() []string {
	feedbackRegistryMu.RLock()
	defer feedbackRegistryMu.RUnlock()
	return knownFeedbackIDsLocked()
}

// knownFeedbackIDsLocked returns the sorted registry keys. Callers hold
// feedbackRegistryMu (read or write).
func knownFeedbackIDsLocked() []string {
	ids := make([]string, 0, len(feedbackRegistry))
	for id := range feedbackRegistry {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}
