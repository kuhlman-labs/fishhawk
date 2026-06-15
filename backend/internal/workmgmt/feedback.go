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
