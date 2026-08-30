package workmgmt

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"unicode/utf8"

	"github.com/kuhlman-labs/fishhawk/backend/internal/forge"
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
// the sibling (children) set. A child body's reference to an issue that is
// NOT a child of the queried epic is CLASSIFIED by reading its state (#2953):
// a closed-and-completed cross-epic target is a satisfied dependency
// (SatisfiedEdges, elided from the DAG), while an open/incomplete/unreadable
// one is dangling (DroppedEdges) — because the campaign wave DAG (plan.Waves)
// is over the epic's own children. The result is the input E25.3 feeds to
// plan.Waves.
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

// IssueSetDependencyResolver is the optional no-epic campaign source (E48.36 /
// #2051): resolve the depends_on edges over an ARBITRARY, explicitly-named set
// of issues that share no epic parent. It is the items-without-epic_ref
// counterpart to EpicChildrenQuerier — where EpicChildren derives its sibling
// set from an epic's sub-issue graph, ResolveDependencies is HANDED the exact
// set of issues (req.Items) and resolves each one's `Depends on: #X` body marker
// directly, because there is no epic sweep to derive the edge set from. Like
// Transitioner / NumberDiscoverer / EpicChildrenQuerier it is a SEPARATE
// capability interface rather than folded into Provider, because not every
// provider can resolve an issue set (jira is interface-only in v0) and widening
// Provider would force every registered fake to grow the method. The campaign
// create handler resolves a provider via Get and type-asserts this capability;
// a provider that does not implement it yields 501.
//
// ResolveDependencies emits the SAME *EpicChildrenResult shape Assemble already
// consumes: Children are the named issues ascending by number, Edges are the
// in-set depends_on edges, and DroppedEdges/SatisfiedEdges are edges whose target
// is NOT in the named set — CLASSIFIED by reading the target's issue state
// (#2953): a closed-and-completed target is a SATISFIED dependency (recorded in
// SatisfiedEdges and elided from the wave DAG, so a batch whose prerequisite is
// already done assembles instead of failing closed), while an OPEN, closed-but-
// incomplete, or unreadable target keeps the fail-closed refusal in DroppedEdges.
type IssueSetDependencyResolver interface {
	ResolveDependencies(ctx context.Context, req IssueSetRequest) (*EpicChildrenResult, error)
}

// IssueSetRequest is the resolved input to ResolveDependencies: the filing
// Target (repo + installation) and the explicit set of issue references
// (`#N`/`N` or `issue:N`) the campaign scopes to. Unlike EpicChildrenRequest
// there is no epic ref — Items IS the authoritative set, resolved directly.
type IssueSetRequest struct {
	Target Target
	Items  []string
}

// EpicChildrenResult is the epic-children query output: the epic's child
// issues and the depends_on edges among them. Children are ordered
// ascending by number; Edges are deterministically sorted. It is the input
// E25.3 assembles into the campaign wave DAG (plan.Waves).
type EpicChildrenResult struct {
	Children []EpicChild
	Edges    []DependsEdge
	// DroppedEdges are the parsed depends_on edges whose target is NOT a
	// fellow child of the queried epic AND is not an already-satisfied
	// dependency — a genuinely dangling edge: the target is OPEN, closed-but-
	// incomplete (not_planned/duplicate), or its state could not be read
	// (#2953). They are kept out of Edges (the wave DAG is over the epic's own
	// children) but surfaced here rather than silently discarded, so campaign
	// assembly (E25.3) can fail closed on a missing dependency instead of
	// dropping it. Like Edges, it is deterministically sorted by (From, To,
	// ToRefDigest) — the digest tiebreak fully orders a set of unresolvable edges
	// sharing (From, To=0) but naming distinct cross-repo tokens (#2956). Empty
	// when every depends_on reference points at a sibling or an already-completed
	// target.
	DroppedEdges []DependsEdge
	// SatisfiedEdges are depends_on edges deliberately elided from BOTH the
	// DroppedEdges dangling check AND the wave DAG because the target is a
	// closed-AND-completed issue outside the assembled set (#2953): its work
	// already landed, so the dependency is genuinely satisfied and eliding it
	// lets the batch assemble rather than fail closed with an unactionable
	// dangling-dependency error. Each entry carries the state actually observed
	// (State/StateReason) so the elision is auditable rather than silent. Like
	// Edges/DroppedEdges it is deterministically sorted by (From, To). Empty
	// when no out-of-set target was already completed.
	SatisfiedEdges []SatisfiedEdge
}

// SatisfiedEdge is one depends_on edge whose out-of-set target is already
// closed-and-completed, so the dependency is satisfied and the edge is elided
// from the wave DAG rather than treated as dangling (#2953). From/To are issue
// numbers; State/StateReason carry the target's OBSERVED issue state
// ("closed"/"completed") so the elision is auditable — an operator can tell
// "this dependency is done" from "this dependency was ignored".
type SatisfiedEdge struct {
	From        int
	To          int
	State       string
	StateReason string
}

// EpicChild is one child issue of an epic: its number, title, autonomy tier,
// and completion state. Autonomy is the tier parsed from the child's
// `autonomy:<tier>` label (low|medium|high), empty when the child carries no
// autonomy label (unknown/default). It is the producer end of the campaign
// autonomy-aware eligibility path (#1551): the campaign engine diverts a
// deps-satisfied autonomy:low child out of the auto-dispatch Eligible slice.
//
// Complete is true when the child issue is closed-and-completed (GitHub
// IssueState CLOSED with IssueStateReason COMPLETED) — the "already
// merged/done" signal the campaign subset filter reads to treat an included
// item's depends_on on an EXCLUDED sibling as satisfied rather than dangling
// (#2120). A closed-as-not_planned or closed-as-duplicate child is NOT complete
// (its work did not land), so it does not satisfy a dependency. False for an
// open child and, fail-safe, whenever the completion signal is absent.
type EpicChild struct {
	Number   int
	Title    string
	Autonomy string
	Complete bool
	// Body is the child issue's raw body. It is the surface the forge-state
	// idempotency lookup reads (#2064, E50.7): a filed child carries a hidden
	// idempotency marker in its body, so query-before-file adoption asks
	// BodyHasIdempotencyKey of this field. Additive and empty for a provider
	// that does not return bodies.
	Body string
	// URL is the forge's OWN canonical absolute URL for the child, carried
	// through verbatim — NEVER composed from owner/repo/number. The filed path
	// records the URL the forge returned (github/provider.go uses
	// issue.HTMLURL) and githubclient.Client.BaseURL is configurable, so
	// composing a literal https://github.com/{owner}/{repo}/issues/{n} would
	// produce a WRONG url on a GitHub Enterprise Server host. Additive and
	// empty for a provider that does not return one.
	URL string
}

// DropReason categorizes why a DependsEdge was dropped from the wave DAG, so
// the campaign-assembly failure can name the real cause and remedy per edge
// (#2120). It is meaningful only on a DroppedEdges entry; a satisfied edge in
// Edges leaves it "" (the zero value), keeping existing equality assertions on
// Edges unchanged.
type DropReason string

const (
	// DropNotChild marks an edge whose target is not a fellow child of the
	// epic — a typo'd number or a genuine cross-epic dependency. It keeps the
	// pre-#2120 "not a fellow child of the epic" wording.
	DropNotChild DropReason = "not_child"
	// DropExcludedIncomplete marks an edge from an INCLUDED subset item to a
	// fellow child that was EXCLUDED from the items subset and is not yet
	// complete, so its dependency cannot be honored within the campaign. The
	// remedy is to include it in items, or omit items to sweep every child so a
	// completed dependency auto-settles (#2120).
	DropExcludedIncomplete DropReason = "excluded_incomplete"
	// DropTargetClosedIncomplete marks an edge whose out-of-set target is CLOSED
	// but NOT completed — closed as not_planned or duplicate (#2953). Its work
	// did not land, so the dependency is genuinely unsatisfied; no batch-widening
	// knob can make a not_planned close satisfy the edge, so the remedy is to
	// reopen/replace the dependency or drop the edge (NOT to widen the batch).
	DropTargetClosedIncomplete DropReason = "target_closed_incomplete"
	// DropTargetStateUnreadable marks an edge whose out-of-set target's issue
	// state could not be read — a GetIssue error, a nil issue, or a ref that is
	// not a resolvable same-repo numeric target (#2953). Satisfaction is
	// UNPROVEN, so the edge fails closed and the refusal stands: an unreadable
	// target is NEVER evidence of satisfaction. The remedy is to retry.
	DropTargetStateUnreadable DropReason = "target_state_unreadable"
)

// DependsEdge is one depends_on edge over the sibling set: From depends on
// To. Both are child issue numbers of the queried epic. Reason categorizes a
// DROPPED edge (why it could not be honored) and is "" on a satisfied edge in
// Edges (#2120).
//
// ToRef/ToRefDigest give an UNRESOLVABLE dropped edge — one whose target is a
// cross-repo / owner-qualified / unparseable depends_on token, not a same-repo
// number — an IDENTITY (#2956), so two DISTINCT such tokens no longer both
// render `issue:From->issue:0` and no longer collapse to one edge under the
// per-item dedup key (From, To, ToRefDigest). ToRefDigest is the 16-hex-char
// digest of the token's canonical form; ToRef is its sanitized, rune-bounded
// display. BOTH are empty on a resolvable/numeric edge (its downstream shape is
// byte-identical to before, so every existing equality assertion is unchanged).
// The one rendering authority for a depends_on target is TargetRef — an
// unresolvable edge must never render `issue:0`.
//
// This type is an in-memory query result only: it carries NO json struct tags
// and is marshaled into no REST or MCP payload (an edge always reaches the wire
// as a string rendered through TargetRef). Adding these two fields therefore
// changes no public payload shape (#2956, operator condition 2, verified at
// implement time).
type DependsEdge struct {
	From        int
	To          int
	Reason      DropReason
	ToRef       string
	ToRefDigest string
}

// targetRefDisplayMaxRunes bounds the rendered display of an unresolvable
// depends_on target so a maliciously long or control-rune-laden token cannot
// blow up an operator message — enforced HERE in the renderer, not only in the
// upstream GitHub-parser sanitizer, so the bound holds for a DependsEdge built
// by ANY constructor (#2956, operator condition 1).
const targetRefDisplayMaxRunes = 64

// TargetRef is the SINGLE rendering authority for a depends_on edge's target
// (#2956). A resolvable/numeric edge (empty ToRefDigest) renders `issue:<To>`,
// byte-identical to the pre-#2956 hand-rolled `"issue:"+To` formatters it
// replaces. An UNRESOLVABLE edge renders `unparsable:<digest>:"<token>"` — with
// the token bounded to targetRefDisplayMaxRunes runes and strconv.Quote'd so an
// embedded control rune, newline, ANSI escape or quote is escaped rather than
// emitted raw — or the bounded greppable sentinel `unparsable:<digest>:<unprintable>`
// when the display sanitized to empty. It can never render `issue:0` for an
// unresolvable target, nor be confused with a genuine numeric unreadable edge.
func (e DependsEdge) TargetRef() string {
	if e.ToRefDigest == "" {
		return "issue:" + strconv.Itoa(e.To)
	}
	display := boundTargetRefDisplay(e.ToRef)
	if display == "" {
		return "unparsable:" + e.ToRefDigest + ":<unprintable>"
	}
	return "unparsable:" + e.ToRefDigest + ":" + strconv.Quote(display)
}

// boundTargetRefDisplay caps a display string at targetRefDisplayMaxRunes runes,
// appending a horizontal-ellipsis rune on truncation. A parser-produced ToRef is
// already within the bound so this is a no-op there; it bites only on a
// directly-constructed edge whose ToRef bypassed the parser sanitizer (#2956,
// operator condition 1).
func boundTargetRefDisplay(s string) string {
	if utf8.RuneCountInString(s) <= targetRefDisplayMaxRunes {
		return s
	}
	rs := []rune(s)
	return string(rs[:targetRefDisplayMaxRunes-1]) + "…"
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
	// Scope is the opaque forge credential-scope key naming which
	// installation to authenticate as when the filing lands (ADR-058 /
	// #1855). It carries the same installation identity the pre-scope
	// int64 InstallationID did — its zero value (IsZero) is the
	// unresolved-installation sentinel the old InstallationID==0 was —
	// but as a forge-neutral key rather than a bare GitHub id. Target is
	// in-process only, so this field carries no serialization tags.
	Scope forge.CredentialScope
	Repo  Repo
	// Project is the GitHub Projects connection from the conventions
	// (nil for providers that don't use it).
	Project *Project
	// Jira is the Jira connection from the conventions (nil for providers
	// that don't use it). The instance base URL and credentials are
	// server-side env (FISHHAWKD_JIRA_*), not carried here — this block
	// selects only the target Jira project.
	Jira *JiraConnection
	// GitLab is the GitLab connection from the conventions (nil for
	// providers that don't use it). The instance base URL and token are
	// server-side env (FISHHAWKD_GITLAB_*), not carried here — this block
	// selects only an optional target-project override.
	GitLab *GitLabConnection
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
