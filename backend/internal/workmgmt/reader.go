package workmgmt

// This file carries the READ half of the work-management provider
// abstraction (#2230 / ADR-064): the optional WorkItemReader capability, its
// request/result vocabulary, and the typed UnavailableError every degradation
// surfaces through. It lives beside provider.go rather than inside it because
// provider.go already carries the base Provider plus four capability
// interfaces; the read capability is a fifth, and a separate file keeps each
// capability's rationale readable.
//
// NOTHING IN PRODUCTION CALLS THIS YET. #2230 reserves the board-as-input
// seam; no HTTP route, MCP tool, or CLI verb resolves a reader, and the
// ADR-064 invariant that no board-read path reaches the MCP agent tool
// surface is enforced by backend/internal/mcpserver/board_read_guard_test.go.

import (
	"context"
	"fmt"
	"sort"
	"strings"
)

// WorkItemReader is the optional work-item READ capability (#2230 /
// ADR-064): resolve a single work item by reference, or enumerate a
// query-scoped set of them. It is the counterpart to the create-only
// Provider.File — the seam a future selection source (a campaign that picks
// its items off the board, a backlog-grooming pass) reads through.
//
// Like Transitioner (#1012), NumberDiscoverer (#1269), EpicChildrenQuerier
// (ADR-047) and IssueSetDependencyResolver (#2051) it is a SEPARATE
// capability interface rather than folded into Provider, for the same two
// reasons those give: not every provider reads work items (gitlab and jira
// are File-only in v0), and widening Provider would force every registered
// fake in every sibling package's tests to grow both methods. That is not a
// style preference here — it is the acceptance-criterion-4 requirement that a
// second implementation not be forced into a GitHub-shaped contract. Callers
// resolve the capability through ReaderFor, which type-asserts it and returns
// a typed *UnavailableError when the provider does not implement it.
//
// Both methods FAIL CLOSED on every degradation. A provider that cannot read
// the board — no installation scope, no project configured, a user-owned
// board with no projects token (#1114), a forge permission refusal — returns
// a nil result and an *UnavailableError naming the Reason, NEVER an empty
// page or a zero-valued record a caller would read as "no items" / "an item
// with no board state". That distinction is the whole point of the capability:
// a silent empty answer from a permissions failure is indistinguishable from a
// genuinely empty backlog, and acting on it would drive the wrong decision.
type WorkItemReader interface {
	// ReadWorkItem resolves ONE work item by reference. Ref accepts the repo's
	// existing reference conventions: "#123", "123", or "issue:123".
	ReadWorkItem(ctx context.Context, req ReadWorkItemRequest) (*WorkItemRecord, error)
	// ListWorkItems enumerates the work items matching the request's filters.
	// The provider MUST enumerate ISSUES, never a project board's item list —
	// a board item list is capped and truncates silently (the AGENTS.md
	// Project #7 pagination trap).
	ListWorkItems(ctx context.Context, req ListWorkItemsRequest) (*WorkItemPage, error)
}

// ReadWorkItemRequest is the resolved input to ReadWorkItem: the Target (repo
// + installation scope + optional project connection) and the issue Ref.
//
// ResolveBoardState opts the read into board-state resolution. It is OFF by
// default because reading the board costs extra round trips and — for a
// user-owned board — a credential the installation token cannot supply
// (#1114); a caller that only needs title/labels/state should not be failed
// closed on a board it never asked about. When it is ON, the same
// project/token preconditions ListWorkItems enforces apply, and a failure to
// meet them is an *UnavailableError, not a silently-absent BoardState.
//
// States is the Conventions.States canonical-state -> provider-option map,
// carried on the request exactly as TransitionRequest already carries it, so
// the provider reverse-maps the board option to a canonical state without
// re-reading the conventions.
type ReadWorkItemRequest struct {
	Target            Target
	Ref               string
	ResolveBoardState bool
	States            map[string]string
}

// ListWorkItemsRequest is the resolved input to ListWorkItems: the Target and
// the two filters a selection source needs.
//
// Labels filters to items carrying EVERY named label (the forge AND-s them);
// empty means unfiltered. BoardStates filters to items whose board column maps
// to one of the named CANONICAL states (model.go's closed set —
// CanonicalStateBacklog and friends — never a provider option string like
// "In Progress"); a non-empty BoardStates implies ResolveBoardState. States is
// the Conventions.States canonical -> provider-option map. IncludeClosed adds
// closed items to the enumeration (open-only otherwise). Limit caps the
// returned item count AFTER filtering; 0 means no caller cap.
type ListWorkItemsRequest struct {
	Target            Target
	Labels            []string
	BoardStates       []string
	ResolveBoardState bool
	States            map[string]string
	IncludeClosed     bool
	Limit             int
}

// WorkItemRecord is one read work item in provider-neutral vocabulary.
//
// State/StateReason carry the forge's lifecycle vocabulary; Complete is the
// derived closed-AND-completed signal (the same rule EpicChild.Complete uses:
// a not_planned/duplicate close is NOT complete, because its work did not
// land). BoardColumn is the provider's own option name (e.g. "In Progress");
// BoardState is that option reverse-mapped through the request's States to a
// canonical state, and is EMPTY when the item is off-board, when board state
// was not requested, or when the column maps to no canonical state — an
// unmapped column is reported as unmapped, never guessed.
//
// URL is LIST-PATH-ONLY in v0 and is always EMPTY from ReadWorkItem. That
// asymmetry is a property of the payloads the two paths read, not an
// oversight: the GitHub list path decodes the GraphQL issue node, which
// selects `url`, while the read path decodes githubclient.Issue — the REST
// single-issue payload, which carries no URL field at all. Populating it on
// the read path would mean widening that shared REST struct, which several
// unrelated callers consume; #2230 leaves that to the first consumer that
// needs it. Until then a caller MUST NOT treat an empty URL from ReadWorkItem
// as "this item has no URL" — reconstruct it from Number and the target repo,
// or read the item through ListWorkItems.
type WorkItemRecord struct {
	Number      int
	Title       string
	URL         string
	Body        string
	Labels      []string
	State       string
	StateReason string
	Complete    bool
	OnBoard     bool
	BoardColumn string
	BoardState  string
}

// WorkItemPage is the ListWorkItems result. BoardStateResolved reports whether
// board state was actually resolved for the items — false when the caller did
// not ask for it, so an empty BoardState on every record is unambiguously "not
// asked" rather than "asked and nothing found".
type WorkItemPage struct {
	Items              []WorkItemRecord
	BoardStateResolved bool
}

// UnavailableReason is the closed set of reasons a work-management capability
// is unavailable for a request. It is what makes the degradation MACHINE-
// READABLE: a caller switches on the Reason to decide whether to prompt the
// operator for a token, fall back to a different source, or fail the request.
type UnavailableReason string

const (
	// ReasonNotImplemented is set when the resolved provider does not implement the
	// capability at all (a File-only provider — gitlab, jira — in v0).
	ReasonNotImplemented UnavailableReason = "not_implemented"
	// ReasonNoProjectConfigured is set when board state was requested but the target
	// carries no project connection, so there is no board to read.
	ReasonNoProjectConfigured UnavailableReason = "no_project_configured"
	// ReasonNoProjectsToken is set when the board is USER-owned, which a GitHub App
	// installation token cannot reach (#1114), and no projects token is
	// configured.
	ReasonNoProjectsToken UnavailableReason = "no_projects_token"
	// ReasonForbidden is set when the forge refused the read on permissions.
	ReasonForbidden UnavailableReason = "forbidden"
	// ReasonNoInstallation is set when no installation/credential scope is available, so
	// no token can be minted for the read.
	ReasonNoInstallation UnavailableReason = "no_installation"
	// ReasonBoardStateUndecidable is set when the forge CAN be read but its
	// answer is ambiguous, so no board state can be reported honestly — the
	// GitHub case is an issue whose per-issue project-membership page came back
	// FULL without carrying the target project, leaving "is it on this board?"
	// undecidable. It is a distinct reason from ReasonForbidden because the
	// remedy is different: nothing about the credential is wrong, so prompting
	// the operator for a token would be the wrong response. The provider-level
	// cause (e.g. *githubclient.BoardMembershipUndecidableError) is retained in
	// Cause, so a caller that wants the offending item number can errors.As for
	// it through Unwrap.
	ReasonBoardStateUndecidable UnavailableReason = "board_state_undecidable"
)

// UnavailableError is the typed capability-unavailable result (#2230
// acceptance criterion 5). Every read degradation returns it with a nil
// result, so a caller can never mistake a permissions failure for an empty
// backlog. Provider is the provider id, Capability the capability name,
// Reason the machine-readable cause, and Detail the operator-facing remedy.
//
// Cause retains the underlying error where one exists (a forge sentinel such
// as githubclient.ErrForbidden) and is exposed via Unwrap, so
// errors.Is(err, githubclient.ErrForbidden) still holds on the SAME value
// errors.As resolves to an *UnavailableError. Both must work: the Reason is
// what a caller switches on, the sentinel is what existing forge-level
// error handling already matches.
type UnavailableError struct {
	Provider   string
	Capability string
	Reason     UnavailableReason
	Detail     string
	Cause      error
}

func (e *UnavailableError) Error() string {
	msg := fmt.Sprintf("workmgmt: provider %q cannot serve %s: %s",
		e.Provider, e.Capability, e.Reason)
	if e.Detail != "" {
		msg += ": " + e.Detail
	}
	if e.Cause != nil {
		msg += ": " + e.Cause.Error()
	}
	return msg
}

// Unwrap returns the retained cause so errors.Is against a forge sentinel
// still matches through the typed wrapper. Nil when the degradation is a
// precondition this package decided locally (no project, no token, no
// installation, not implemented) rather than an error the forge returned.
func (e *UnavailableError) Unwrap() error { return e.Cause }

// ReaderCapability is the capability name UnavailableError carries for the
// read capability, so the error text names what was unavailable.
const ReaderCapability = "work-item read"

// ReaderFor resolves the registered provider for id and type-asserts the
// optional WorkItemReader capability. It is the SINGLE chokepoint every
// consumer of the read capability must resolve through — that is what makes
// "never an empty list on a permissions failure" enforceable rather than
// conventional: a File-only provider yields a typed
// *UnavailableError{Reason: ReasonNotImplemented} here, not a nil interface a
// caller could dispatch against and not an empty page it could misread.
//
// An unregistered id returns Get's existing *UnknownProviderError unchanged.
func ReaderFor(id string) (WorkItemReader, error) {
	p, err := Get(id)
	if err != nil {
		return nil, err
	}
	r, ok := p.(WorkItemReader)
	if !ok {
		return nil, &UnavailableError{
			Provider:   p.Name(),
			Capability: ReaderCapability,
			Reason:     ReasonNotImplemented,
			Detail:     "this provider files work items but does not read them; use a provider that implements the read capability",
		}
	}
	return r, nil
}

// CanonicalStateForOption reverse-maps a provider board-option name (e.g.
// "In Progress") to its canonical state through a Conventions.States
// canonical -> option map. It returns "" when option is empty or maps to no
// canonical state — an unmapped column is reported as unmapped, never guessed.
// Ties (two canonical states bound to the same option, a config the schema
// permits) resolve to the alphabetically-first canonical state so the answer
// is deterministic rather than map-iteration-order dependent.
func CanonicalStateForOption(states map[string]string, option string) string {
	if option == "" {
		return ""
	}
	canonicals := make([]string, 0, len(states))
	for c := range states {
		canonicals = append(canonicals, c)
	}
	sort.Strings(canonicals)
	for _, c := range canonicals {
		if strings.TrimSpace(states[c]) == option {
			return c
		}
	}
	return ""
}
