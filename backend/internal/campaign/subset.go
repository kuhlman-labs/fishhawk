package campaign

import (
	"errors"
	"fmt"

	"github.com/kuhlman-labs/fishhawk/backend/internal/workmgmt"
)

// ErrItemNotChild is returned (wrapped) by FilterToSubset when a requested
// subset item PARSES as an issue ref but is not among the epic's children.
// Callers errors.Is against it to map the failure onto a typed 422
// (campaign_item_not_child) without depending on the underlying message. It
// fails closed: a subset that names a non-child issue is rejected rather than
// silently assembled over the children that DO match, so a typo or a stale ref
// surfaces loudly.
//
// It covers ONLY the not-a-child claim (#2176). A ref that does not PARSE is a
// different claim — it cannot be said to be "not a child of the epic" when it
// names no issue at all — and is returned wrapped around
// workmgmt.ErrInvalidItemRef, which the handler maps onto 422
// campaign_item_ref_invalid.
var ErrItemNotChild = errors.New("campaign: subset item is not a child of the epic")

// FilterToSubset narrows an epic-children result to the named subset of items,
// returning a new result over only those children and the depends_on edges
// among them. It is the pure engine behind the create handler's optional
// `items` subset filter (#2003): an operator can scope a campaign to a triaged
// slice of an epic's children in one call instead of filing a shadow epic and
// re-parenting issues.
//
// items are issue refs in the "101", "#101", or "issue:101" form (#3314),
// each parsed by the single shared workmgmt.ParseIssueRef, which tolerates at
// most one of each prefix — a doubled prefix like "issue:issue:101" fails to
// parse rather than silently resolving. Every requested ref MUST parse AND
// resolve to a child in res.Children; the FIRST miss fails closed naming the
// offending ref — a ref that does not parse returns a wrapped
// workmgmt.ErrInvalidItemRef (this now also covers a non-positive number, a
// tightening from the prior campaign-path behavior), a parseable ref that is
// not a child returns a wrapped ErrItemNotChild (#2176).
//
// The edge set is re-partitioned against the included item set:
//   - an edge with BOTH endpoints included is kept in Edges;
//   - an edge whose From is included but whose To is EXCLUDED is resolved
//     against the excluded target's completion state: if that target is already
//     closed-and-completed (EpicChild.Complete) the dependency is satisfied and
//     the edge is dropped SILENTLY (the same result the full all-children sweep
//     produces via closed-child auto-settle, #2120); otherwise it is appended to
//     DroppedEdges stamped DropExcludedIncomplete — an included item depending
//     on an excluded, not-yet-complete sibling is a dangling dependency, so
//     Assemble fails it closed as campaign_dangling_dependency, exactly as a
//     cross-epic dangling edge does today;
//   - an edge whose From is excluded is dropped silently (the depending item is
//     not in the campaign, so its dependency is irrelevant).
//
// A pre-existing DroppedEdge on res carries through only when its From is IN
// the subset: an INCLUDED item's provider-surfaced dangling/cross-epic edge
// still fails Assemble closed, while an EXCLUDED item's dropped edge is dropped
// silently — mirroring the From-excluded handling of res.Edges above. So the
// dangling-dependency error fires only for edges among or FROM included items,
// and a subset that excludes a child no longer inherits that child's cross-epic
// dependency (#2087).
//
// When items is empty/nil, res is returned unchanged — the backward-compatible
// no-op that sweeps every child, so omitting the field preserves prior
// all-children behavior.
func FilterToSubset(res *workmgmt.EpicChildrenResult, items []string) (*workmgmt.EpicChildrenResult, error) {
	if res == nil {
		return nil, errors.New("campaign: nil epic-children result")
	}
	if len(items) == 0 {
		return res, nil
	}

	// Index the epic's children by number so each requested ref can be
	// validated and the ascending child order preserved.
	childByNumber := make(map[int]workmgmt.EpicChild, len(res.Children))
	for _, c := range res.Children {
		childByNumber[c.Number] = c
	}

	// Resolve every requested ref to a child number, failing closed on the
	// first ref that is not a child of the epic.
	included := make(map[int]struct{}, len(items))
	for _, ref := range items {
		num, err := parseItemRef(ref)
		if err != nil {
			return nil, err
		}
		if _, ok := childByNumber[num]; !ok {
			return nil, fmt.Errorf("%w: %s", ErrItemNotChild, ref)
		}
		included[num] = struct{}{}
	}

	// Build the filtered children preserving res's ascending order.
	children := make([]workmgmt.EpicChild, 0, len(included))
	for _, c := range res.Children {
		if _, ok := included[c.Number]; ok {
			children = append(children, c)
		}
	}

	// Carry the provider-recorded satisfied (elided) edges through, DEDUPING by
	// (From,To) as they are copied so a duplicate already present in the inbound
	// slice collapses to one entry, and seed the same dedup set so an edge already
	// recorded as satisfied is never re-added by the excluded-complete branch
	// below (#2953 condition 3: an edge appears in SatisfiedEdges AT MOST ONCE).
	// The provider now collapses duplicate depends_on tokens at the source, but
	// deduping here too makes the at-most-once invariant hold for ANY inbound
	// producer rather than relying on every producer to dedup first. These inbound
	// entries are for out-of-EPIC targets, disjoint from the excluded-CHILD
	// elisions below.
	satisfied := make([]workmgmt.SatisfiedEdge, 0, len(res.SatisfiedEdges))
	seenSatisfied := make(map[[2]int]struct{}, len(res.SatisfiedEdges))
	for _, s := range res.SatisfiedEdges {
		if _, dup := seenSatisfied[[2]int{s.From, s.To}]; dup {
			continue
		}
		seenSatisfied[[2]int{s.From, s.To}] = struct{}{}
		satisfied = append(satisfied, s)
	}

	// Re-partition the edges against the included set. Carry a pre-existing
	// dropped edge through only when its From is IN the subset; an excluded
	// item's dropped (cross-epic/dangling) edge is dropped silently, mirroring
	// the From-excluded handling of res.Edges below.
	var edges []workmgmt.DependsEdge
	dropped := make([]workmgmt.DependsEdge, 0, len(res.DroppedEdges))
	for _, e := range res.DroppedEdges {
		if _, fromIn := included[e.From]; fromIn {
			dropped = append(dropped, e)
		}
	}
	for _, e := range res.Edges {
		_, fromIn := included[e.From]
		_, toIn := included[e.To]
		switch {
		case fromIn && toIn:
			edges = append(edges, e)
		case fromIn && !toIn:
			// An included item depends on an EXCLUDED fellow child. If that
			// excluded target is already closed-and-completed, its dependency is
			// satisfied — elide the edge, RECORDING it as a SatisfiedEdge (#2953)
			// rather than dropping it silently, so the same elision the full
			// all-children sweep performs (#2120) is now visible/auditable.
			// childByNumber indexes ALL children (included and excluded), so the
			// lookup resolves; an unexpectedly-missing target fails closed
			// (treated as excluded-incomplete) rather than panicking.
			if child, ok := childByNumber[e.To]; ok && child.Complete {
				if _, seen := seenSatisfied[[2]int{e.From, e.To}]; !seen {
					seenSatisfied[[2]int{e.From, e.To}] = struct{}{}
					satisfied = append(satisfied, workmgmt.SatisfiedEdge{
						From: e.From, To: e.To, State: "closed", StateReason: "completed",
					})
				}
				continue
			}
			e.Reason = workmgmt.DropExcludedIncomplete
			dropped = append(dropped, e)
		default:
			// From is excluded: the depending item is not in the campaign, so
			// drop the edge silently.
		}
	}

	return &workmgmt.EpicChildrenResult{
		Children:       children,
		Edges:          edges,
		DroppedEdges:   dropped,
		SatisfiedEdges: satisfied,
	}, nil
}

// parseItemRef parses a subset item ref into a child issue number. It accepts
// the `N`, `#N`, and `issue:N` forms (#3314) by delegating to the single
// shared workmgmt.ParseIssueRef — the SAME parser the no-epic
// IssueSetDependencyResolver path uses — passed the RAW ref: no local trim,
// no local strip, since ParseIssueRef is the ONLY normalization either path
// may apply (a local pre-strip here would double-strip a doubled "issue:"
// prefix on this path while the delegate-once github path rejects it). A ref
// in any other shape, including a non-positive number, is a caller error (an
// unresolvable subset item), returned as a wrapped workmgmt.ErrInvalidItemRef
// — the SHARED sentinel the no-epic resolver also wraps — so the handler
// answers 422 campaign_item_ref_invalid. It is deliberately NOT ErrItemNotChild
// (#2176): a ref that names no issue cannot be claimed to be "not a child of
// the epic".
func parseItemRef(ref string) (int, error) {
	num, err := workmgmt.ParseIssueRef(ref)
	if err != nil {
		return 0, fmt.Errorf("%w: %q is not a valid issue ref (want N, #N or issue:N): %w", workmgmt.ErrInvalidItemRef, ref, err)
	}
	return num, nil
}
