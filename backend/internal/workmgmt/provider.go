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

// Transitioner is the optional board-state-sync capability (#1012): move an
// already-filed work item's board Status along a run-lifecycle edge. It is
// declared as a separate capability interface rather than folded into
// Provider because not every provider boards work (jira is interface-only in
// v0) — and, decisively for the decomposed rollout, widening Provider would
// force every registered fake in a sibling slice's test to grow the method,
// the cross-slice scope-amendment trap the plan's decomposition explicitly
// avoids. The run-lifecycle hook resolves a provider via Get and type-asserts
// this capability before dispatching; a provider that does not implement it
// simply yields no board move.
//
// Transition honors the never-fight-the-human guard: it advances the card
// only when its current status is in the request's expected source set,
// otherwise returning a Skipped result with no mutation. It touches ONLY the
// Status column, never the rich fields File applies — the scope split with
// #1005.
type Transitioner interface {
	Transition(ctx context.Context, req TransitionRequest) (*TransitionResult, error)
}

// NumberDiscoverer is the optional server-side number-discovery capability
// (#1269): enumerate the sequential numbers already in use for a numbered
// type (e.g. ADR) by querying the tracker, so a numbered filing no longer
// requires the caller to pass existing_numbers. Like Transitioner it is a
// SEPARATE capability interface rather than folded into Provider, because not
// every provider discovers numbers (jira is interface-only in v0) and widening
// Provider would force every registered fake to grow the method. The filing
// handler resolves a provider via Get and type-asserts this capability before
// the pure Apply runs; a provider that does not implement it yields no
// discovery, leaving Apply's existing fail-closed allocate (#1265) as the
// last-line guard.
//
// DiscoverNumbers returns the numbers found (possibly empty, no error — an
// empty result means a genuinely-first numbered item) or an error on a genuine
// discovery failure (the handler then fails the filing closed). It must NOT
// invent a number; allocation stays in Apply.
type NumberDiscoverer interface {
	DiscoverNumbers(ctx context.Context, req DiscoverNumbersRequest) ([]int, error)
}

// EpicChildrenQuerier is the optional epic-children query capability
// (ADR-047 / #1437, the campaign DAG source): given an epic reference, list
// the epic's child issues and return the depends_on edges among them. Like
// Transitioner and NumberDiscoverer it is a SEPARATE capability interface
// rather than folded into Provider, because not every provider can resolve
// a sub-issue graph (jira is interface-only in v0) and widening Provider
// would force every registered fake to grow the method. The campaign-
// assembly path (E25.3) resolves a provider via Get and type-asserts this
// capability; a provider that does not implement it yields no children.
//
// EpicChildren returns the children and the depends_on edges restricted to
// the sibling (children) set — a child body's reference to an issue that is
// NOT a child of the queried epic is dropped, because the campaign wave DAG
// (plan.Waves) is over the epic's own children. The result is the input
// E25.3 feeds to plan.Waves.
type EpicChildrenQuerier interface {
	EpicChildren(ctx context.Context, req EpicChildrenRequest) (*EpicChildrenResult, error)
}

// EpicChildrenRequest is the resolved input to EpicChildren: the filing
// Target (repo + installation) and the epic issue reference (`#N` or `N`)
// whose children and depends_on edges are queried.
type EpicChildrenRequest struct {
	Target Target
	Epic   string
}

// EpicChildrenResult is the epic-children query output: the epic's child
// issues and the depends_on edges among them. Children are ordered
// ascending by number; Edges are deterministically sorted. It is the input
// E25.3 assembles into the campaign wave DAG (plan.Waves).
type EpicChildrenResult struct {
	Children []EpicChild
	Edges    []DependsEdge
	// DroppedEdges are the parsed depends_on edges whose target is NOT a
	// fellow child of the queried epic — a dangling/mis-targeted reference
	// (a typo'd number or a real cross-epic dependency). They are kept out of
	// Edges (the wave DAG is over the epic's own children) but surfaced here
	// rather than silently discarded, so campaign assembly (E25.3) can fail
	// closed on a missing dependency instead of dropping it. Like Edges, it is
	// deterministically sorted by (From, To). Empty when every depends_on
	// reference points at a sibling.
	DroppedEdges []DependsEdge
}

// EpicChild is one child issue of an epic: its number, title, and resolved
// autonomy tier. Autonomy is the tier sourced from the child's `autonomy:<tier>`
// label (e.g. "low"/"medium"/"high"), empty when the child carries no autonomy
// label. The campaign engine treats a "low" (human-led) item as never
// autonomously dispatchable (#1551).
type EpicChild struct {
	Number   int
	Title    string
	Autonomy string
}

// DependsEdge is one depends_on edge over the sibling set: From depends on
// To. Both are child issue numbers of the queried epic.
type DependsEdge struct {
	From int
	To   int
}

// DiscoverNumbersRequest is the resolved input to NumberDiscoverer: the
// filing Target (repo + installation), and the numbered type's Prefix (e.g.
// "ADR-") and TitleFormat (e.g. "[ADR-{number}] {summary}") so the provider
// can compose the in:title search term and parse the number back out of each
// matched title.
//
// DefaultLabels is the numbered type's default_labels from the conventions.
// Its FIRST element (when present) is used by the provider as a `label:`
// discovery qualifier so a recency-ordered title search cannot bury the real
// max behind lower-ranked hits (#1522): an `[E{number}]` epic search is
// otherwise buried under its own `[E{number}.x]` children, so the anchored
// re-parse finds no valid epic and the fail-closed allocate mis-picks a
// colliding low number. Narrowing by the type LABEL (which the children do
// NOT carry) returns exactly the numbered items — a small, complete set no
// recency window truncates. Empty for a type without a default label, in
// which case the provider keeps the title-only query.
type DiscoverNumbersRequest struct {
	Target        Target
	Prefix        string
	TitleFormat   string
	DefaultLabels []string
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
	// Jira is the Jira connection from the conventions (nil for providers
	// that don't use it). The instance base URL and credentials are
	// server-side env (FISHHAWKD_JIRA_*), not carried here — this block
	// selects only the target Jira project.
	Jira *JiraConnection
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
