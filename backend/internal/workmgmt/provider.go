package workmgmt

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
)

// Provider files a resolved work item against a concrete backend (GitHub
// Projects, and — once implemented — Jira). A provider is selected by id
// (the conventions' `provider` value) from the registry; the concrete
// implementation lives in a sibling package (e.g. workmgmt/github) and is
// registered by the server at startup.
//
// Name returns the provider id used as the registry key and echoed into
// CreatedItem.Provider. File materializes req — creating the item and
// applying the provider-side placement (board column / status field) and
// relations (epic link) the conventions layer resolved.
type Provider interface {
	Name() string
	File(ctx context.Context, req ProviderRequest) (*CreatedItem, error)
}

// Repo is a provider-neutral repository coordinate. The GitHub provider
// maps it onto its own owner/name ref; a future Jira provider maps it
// onto a project key.
type Repo struct {
	Owner string
	Name  string
}

// Target identifies where a filing lands at request time — the bits the
// conventions config can't carry because they're per-call (installation,
// repo) or come from the conventions but are provider-specific (the
// project connection). Apply leaves Target zero; the filing endpoint
// populates it from the request + conventions before dispatch.
type Target struct {
	InstallationID int64
	Repo           Repo
	// Project is the GitHub Projects connection from the conventions
	// (nil for providers that don't use it).
	Project *Project
}

// ProviderRequest is the fully-resolved filing handed to a Provider: the
// canonical item Apply produced, the allocated sequential number (0 when
// the type isn't numbered), and the Target.
type ProviderRequest struct {
	Item   WorkItem
	Number int
	Target Target
}

// CreatedItem is what a Provider returns on a successful filing: the
// created item's number + URL and the placement/labels actually applied,
// so the caller can audit and echo exactly what landed.
//
// Boarded and EpicLinked report whether the post-create best-effort
// enrichment landed. Creating the issue is the fatal step (no CreatedItem
// is returned when it fails); board placement and epic linking are
// best-effort — a failure leaves Boarded/EpicLinked false and records the
// cause in BoardingError/EpicLinkError, but the issue is still the durable
// result so File returns it with a nil error (#1107). Boarded false with
// an empty BoardingError means there was nothing to board (no project
// configured); likewise EpicLinked false with an empty EpicLinkError means
// no parent epic was requested.
type CreatedItem struct {
	Provider      string   `json:"provider"`
	Number        int      `json:"number"`
	URL           string   `json:"url"`
	AppliedLabels []string `json:"applied_labels,omitempty"`
	Status        string   `json:"status,omitempty"`
	BoardColumn   string   `json:"board_column,omitempty"`
	Boarded       bool     `json:"boarded"`
	EpicLinked    bool     `json:"epic_linked"`
	BoardingError string   `json:"boarding_error,omitempty"`
	EpicLinkError string   `json:"epic_link_error,omitempty"`
}

// UnknownProviderError is returned by Get when no provider is registered
// for the requested id. It is the fail-closed path for an unimplemented
// provider (e.g. jira, which is interface-only in v0) or a config typo:
// the error names the missing id and the registered set rather than
// panicking on a nil dispatch.
type UnknownProviderError struct {
	ID    string
	Known []string
}

func (e *UnknownProviderError) Error() string {
	if len(e.Known) == 0 {
		return fmt.Sprintf("workmgmt: no work-item provider registered for %q (no providers registered)", e.ID)
	}
	return fmt.Sprintf("workmgmt: no work-item provider registered for %q; registered providers: %s",
		e.ID, strings.Join(e.Known, ", "))
}

var (
	registryMu sync.RWMutex
	registry   = map[string]Provider{}
)

// Register adds p to the global provider registry under p.Name(),
// replacing any prior registration for that id. The server wires the
// concrete providers (e.g. the GitHub Projects provider) at startup;
// tests register fakes.
func Register(p Provider) {
	registryMu.Lock()
	defer registryMu.Unlock()
	registry[p.Name()] = p
}

// Get returns the registered provider for id, or an *UnknownProviderError
// naming id and the registered set. Callers MUST surface this error
// rather than dispatching against a nil provider.
func Get(id string) (Provider, error) {
	registryMu.RLock()
	defer registryMu.RUnlock()
	p, ok := registry[id]
	if !ok {
		return nil, &UnknownProviderError{ID: id, Known: knownIDsLocked()}
	}
	return p, nil
}

// Registered returns the sorted set of registered provider ids — used by
// startup logging and the unknown-provider error.
func Registered() []string {
	registryMu.RLock()
	defer registryMu.RUnlock()
	return knownIDsLocked()
}

// knownIDsLocked returns the sorted registry keys. Callers hold
// registryMu (read or write).
func knownIDsLocked() []string {
	ids := make([]string, 0, len(registry))
	for id := range registry {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}
