package campaign

// append.go holds the pure, handler-independent core of campaign APPEND
// (E86.47 / #4358): adding issues to an existing (possibly running) campaign
// and applying add-only depends_on additions to members that have not started.
// Everything here is side-effect-free over []*Item so it is unit-testable
// without a Repository, and the Postgres adapter re-runs the same validation
// under the campaign row lock (ValidateAppend) so a check made before the lock
// cannot be raced.

import (
	"errors"
	"fmt"
	"strings"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/workmgmt"
)

// Sentinel errors returned by the append validators and AppendCampaignItems.
var (
	// ErrDuplicateItem: a ref to append is already a campaign member, or is
	// named twice in one request. Returned wrapped by *DuplicateItemError.
	ErrDuplicateItem = errors.New("campaign: issue is already a campaign member")
	// ErrInvalidDependencyRef: a ref that would be WRITTEN (a new item's ref, a
	// depends_on edge, or an addition) is not the canonical `issue:N` form. An
	// unresolvable tracker token (`unparsable:<digest>:…`), `#N` and `issue:0`
	// all fail, so a malformed edge is refused rather than persisted.
	ErrInvalidDependencyRef = errors.New("campaign: dependency ref is not a canonical issue:N ref")
	// ErrExternalDependency: a NEW item depends on an issue that is not a
	// campaign member and the caller did not admit external blockers. Returned
	// wrapped by *ExternalDependencyError.
	ErrExternalDependency = errors.New("campaign: dependency targets an issue outside the campaign")
	// ErrAppendUnrecorded: the caller's Record callback failed, so the whole
	// append transaction was rolled back and NOTHING was written. Wraps the
	// callback's own error.
	ErrAppendUnrecorded = errors.New("campaign: append could not be recorded")
)

// DuplicateItemError names every duplicate ref. It wraps ErrDuplicateItem.
type DuplicateItemError struct {
	Refs []string
}

func (e *DuplicateItemError) Error() string {
	return fmt.Sprintf("%s: %s", ErrDuplicateItem.Error(), strings.Join(e.Refs, ", "))
}

// Unwrap makes errors.Is(err, ErrDuplicateItem) hold.
func (*DuplicateItemError) Unwrap() error { return ErrDuplicateItem }

// CycleError carries the cycle an append would close, as refs, starting and
// ending at the NEW node the cycle passes through. It wraps ErrCycle.
type CycleError struct {
	Cycle []string
}

func (e *CycleError) Error() string {
	return fmt.Sprintf("%s: %s", ErrCycle.Error(), strings.Join(e.Cycle, " -> "))
}

// Unwrap makes errors.Is(err, ErrCycle) hold.
func (*CycleError) Unwrap() error { return ErrCycle }

// ExternalEdge is one depends_on edge from a NEW item to a non-member issue.
type ExternalEdge struct {
	From string `json:"from"`
	To   string `json:"to"`
}

// ExternalDependencyError lists every external edge on a NEW item. It wraps
// ErrExternalDependency.
type ExternalDependencyError struct {
	Edges []ExternalEdge
}

func (e *ExternalDependencyError) Error() string {
	parts := make([]string, 0, len(e.Edges))
	for _, ed := range e.Edges {
		parts = append(parts, ed.From+"->"+ed.To)
	}
	return fmt.Sprintf("%s: %s", ErrExternalDependency.Error(), strings.Join(parts, ", "))
}

// Unwrap makes errors.Is(err, ErrExternalDependency) hold.
func (*ExternalDependencyError) Unwrap() error { return ErrExternalDependency }

// ValidDependencyRef reports whether ref is the canonical `issue:N` form with
// N > 0: it must carry the `issue:` prefix, parse with workmgmt.ParseIssueRef,
// and re-render byte-equal. Everything else — an `unparsable:<digest>` target,
// `#N`, `issue:0`, `issue:05`, surrounding whitespace — is refused, so only a
// ref the engine can match against a member's IssueRef is ever persisted.
func ValidDependencyRef(ref string) bool {
	if !strings.HasPrefix(ref, "issue:") {
		return false
	}
	n, err := workmgmt.ParseIssueRef(ref)
	if err != nil {
		return false
	}
	return issueRef(n) == ref
}

// MergeDependsOn is the MONOTONE union of a persisted depends_on and an
// addition: persisted order is kept, new refs are appended in order and
// de-duplicated, and a persisted ref is NEVER removed. Removing an edge is an
// explicit operator act, never a side effect of an append or a refresh.
func MergeDependsOn(persisted, add []string) []string {
	out := make([]string, 0, len(persisted)+len(add))
	seen := make(map[string]bool, len(persisted)+len(add))
	// Persisted refs are kept verbatim — even a duplicate — so the union can
	// only ever grow what is stored.
	for _, r := range persisted {
		seen[r] = true
		out = append(out, r)
	}
	for _, r := range add {
		if seen[r] {
			continue
		}
		seen[r] = true
		out = append(out, r)
	}
	return out
}

// CheckDuplicates refuses a ref that is already a member of existing, or is
// named more than once in newRefs. It is cheap and local, so callers run it
// BEFORE any forge read; the adapter re-runs it under the campaign row lock.
func CheckDuplicates(existing []*Item, newRefs []string) error {
	members := make(map[string]bool, len(existing))
	for _, it := range existing {
		members[it.IssueRef] = true
	}
	seen := make(map[string]bool, len(newRefs))
	var dups []string
	for _, r := range newRefs {
		if members[r] || seen[r] {
			dups = append(dups, r)
		}
		seen[r] = true
	}
	if len(dups) > 0 {
		return &DuplicateItemError{Refs: dups}
	}
	return nil
}

// MemberRefs is the set of refs that are members once newItems are appended
// to existing.
func MemberRefs(existing []*Item, newItems []CreateCampaignItemParams) map[string]bool {
	m := make(map[string]bool, len(existing)+len(newItems))
	for _, it := range existing {
		m[it.IssueRef] = true
	}
	for _, ni := range newItems {
		m[ni.IssueRef] = true
	}
	return m
}

// CheckExternal refuses any NEW item depends_on ref that is not in memberRefs
// (existing ∪ new). It is applied to NEW nodes only: an existing member's
// tracker drift onto a non-member is HELD by the dependency refresh, never a
// reason to refuse an unrelated append.
func CheckExternal(newItems []CreateCampaignItemParams, memberRefs map[string]bool) error {
	var edges []ExternalEdge
	for _, ni := range newItems {
		for _, d := range ni.DependsOn {
			if !memberRefs[d] {
				edges = append(edges, ExternalEdge{From: ni.IssueRef, To: d})
			}
		}
	}
	if len(edges) > 0 {
		return &ExternalDependencyError{Edges: edges}
	}
	return nil
}

// ValidateAppendClosure builds the merged member graph — existing depends_on
// ∪ additions, plus the new items — and returns a *CycleError ONLY when a
// cycle passes through a NEW node. Edges to non-members are ignored for cycle
// purposes. A cycle among existing members alone (one a tracker refresh holds)
// is tolerated, so an unrelated append never fails on it.
func ValidateAppendClosure(existing []*Item, newItems []CreateCampaignItemParams, additions map[uuid.UUID][]string) error {
	graph := make(map[string][]string, len(existing)+len(newItems))
	for _, it := range existing {
		graph[it.IssueRef] = MergeDependsOn(it.DependsOn, additions[it.ID])
	}
	for _, ni := range newItems {
		graph[ni.IssueRef] = ni.DependsOn
	}
	for _, ni := range newItems {
		start := ni.IssueRef
		visited := map[string]bool{}
		var path []string
		var dfs func(node string) []string
		dfs = func(node string) []string {
			path = append(path, node)
			defer func() { path = path[:len(path)-1] }()
			for _, next := range graph[node] {
				if _, member := graph[next]; !member {
					continue
				}
				if next == start {
					return append(append([]string{}, path...), start)
				}
				if visited[next] {
					continue
				}
				visited[next] = true
				if c := dfs(next); c != nil {
					return c
				}
			}
			return nil
		}
		if cycle := dfs(start); cycle != nil {
			return &CycleError{Cycle: cycle}
		}
	}
	return nil
}

// ValidateAppend runs every append guard over a repository call's params, in
// the order the adapter applies them under the campaign row lock: every ref to
// be written must be canonical (ErrInvalidDependencyRef), no new ref may
// duplicate a member (ErrDuplicateItem), every addition must target a member
// (ErrCampaignItemNotFound), a NEW item may not depend on a non-member unless
// p.AllowExternal (ErrExternalDependency), and no cycle may pass through a new
// node (ErrCycle). An expedited append (p.Expedite non-nil) is first checked by
// ValidateExpedite (ErrExpediteInvalid). It is exported so a test fake enforces
// exactly the production rules.
func ValidateAppend(existing []*Item, p AppendCampaignItemsParams) error {
	if err := ValidateExpedite(p); err != nil {
		return err
	}
	var bad []string
	newRefs := make([]string, 0, len(p.NewItems))
	for _, ni := range p.NewItems {
		newRefs = append(newRefs, ni.IssueRef)
		if !ValidDependencyRef(ni.IssueRef) {
			bad = append(bad, ni.IssueRef)
		}
		for _, d := range ni.DependsOn {
			if !ValidDependencyRef(d) {
				bad = append(bad, d)
			}
		}
	}
	for _, refs := range p.AddDependencies {
		for _, d := range refs {
			if !ValidDependencyRef(d) {
				bad = append(bad, d)
			}
		}
	}
	if len(bad) > 0 {
		return fmt.Errorf("%w: %s", ErrInvalidDependencyRef, strings.Join(bad, ", "))
	}
	if err := CheckDuplicates(existing, newRefs); err != nil {
		return err
	}
	byID := make(map[uuid.UUID]bool, len(existing))
	for _, it := range existing {
		byID[it.ID] = true
	}
	for id := range p.AddDependencies {
		if !byID[id] {
			return fmt.Errorf("%w: addition targets %s", ErrCampaignItemNotFound, id)
		}
	}
	if !p.AllowExternal {
		if err := CheckExternal(p.NewItems, MemberRefs(existing, p.NewItems)); err != nil {
			return err
		}
	}
	return ValidateAppendClosure(existing, p.NewItems, p.AddDependencies)
}

// AppendPlan is PlanAppend's output: the new-item params (CampaignID and
// Position are assigned by the repository call) plus what the caller reports.
type AppendPlan struct {
	// Items are the new items in resolution order, each with its depends_on
	// (member and non-member edges alike) and normalized autonomy tier.
	Items []CreateCampaignItemParams
	// External maps a new item's ref to its depends_on refs that are NOT
	// members. They are persisted only when the caller admits external
	// blockers; otherwise AppendCampaignItems refuses them.
	External map[string][]string
	// Satisfied are the out-of-set, non-member edges elided because their
	// target is already closed-and-completed.
	Satisfied []workmgmt.SatisfiedEdge
}

// PlanAppend maps a ResolveDependencies result computed over the NEW refs only
// into new-item params. It is handler-independent so the expedite verb
// (#4234) can reuse it.
//
// In-set Edges become depends_on. A DroppedEdge or SatisfiedEdge whose target
// is an existing MEMBER becomes depends_on too, because the engine judges
// satisfaction by member state. A DroppedEdge to a non-member becomes an
// external edge (in depends_on and in External), persisted only when the
// caller admits it. A SatisfiedEdge to a non-member is elided and reported.
// Each child's autonomy is normalized, so an autonomy:low child routes
// human-led. A duplicate (against existing members or within newRefs) returns
// ErrDuplicateItem; callers run CheckDuplicates before the forge read too.
func PlanAppend(existing []*Item, newRefs []string, res *workmgmt.EpicChildrenResult) (*AppendPlan, error) {
	if err := CheckDuplicates(existing, newRefs); err != nil {
		return nil, err
	}
	if res == nil {
		return nil, errors.New("campaign: nil dependency resolution")
	}
	members := make(map[string]bool, len(existing))
	for _, it := range existing {
		members[it.IssueRef] = true
	}
	deps := map[int][]string{}
	external := map[string][]string{}
	for _, e := range res.Edges {
		deps[e.From] = append(deps[e.From], issueRef(e.To))
	}
	for _, e := range res.DroppedEdges {
		to := e.TargetRef()
		deps[e.From] = append(deps[e.From], to)
		if !members[to] {
			external[issueRef(e.From)] = append(external[issueRef(e.From)], to)
		}
	}
	var satisfied []workmgmt.SatisfiedEdge
	for _, e := range res.SatisfiedEdges {
		if to := issueRef(e.To); members[to] {
			deps[e.From] = append(deps[e.From], to)
			continue
		}
		satisfied = append(satisfied, e)
	}
	plan := &AppendPlan{Satisfied: satisfied}
	for _, c := range res.Children {
		ref := issueRef(c.Number)
		plan.Items = append(plan.Items, CreateCampaignItemParams{
			IssueRef:  ref,
			DependsOn: MergeDependsOn(nil, deps[c.Number]),
			Autonomy:  normalizeAutonomy(c.Autonomy),
		})
		if ext := external[ref]; len(ext) > 0 {
			if plan.External == nil {
				plan.External = map[string][]string{}
			}
			plan.External[ref] = MergeDependsOn(nil, ext)
		}
	}
	return plan, nil
}
