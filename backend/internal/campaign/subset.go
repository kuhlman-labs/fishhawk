package campaign

import (
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/kuhlman-labs/fishhawk/backend/internal/workmgmt"
)

// ErrItemNotChild is returned (wrapped) by FilterToSubset when a requested
// subset item is not among the epic's children. Callers errors.Is against it
// to map the failure onto a typed 422 (campaign_item_not_child) without
// depending on the underlying message. It fails closed: a subset that names a
// non-child issue is rejected rather than silently assembled over the children
// that DO match, so a typo or a stale ref surfaces loudly.
var ErrItemNotChild = errors.New("campaign: subset item is not a child of the epic")

// FilterToSubset narrows an epic-children result to the named subset of items,
// returning a new result over only those children and the depends_on edges
// among them. It is the pure engine behind the create handler's optional
// `items` subset filter (#2003): an operator can scope a campaign to a triaged
// slice of an epic's children in one call instead of filing a shadow epic and
// re-parenting issues.
//
// items are issue refs in either the bare-number ("101") or issue:N
// ("issue:101") form, mirroring the ref convention Assemble emits. Every
// requested ref MUST resolve to a child in res.Children; the FIRST miss returns
// a wrapped ErrItemNotChild naming the offending ref (fail closed).
//
// The edge set is re-partitioned against the included item set:
//   - an edge with BOTH endpoints included is kept in Edges;
//   - an edge whose From is included but whose To is EXCLUDED is appended to
//     DroppedEdges — an included item depending on an excluded one is a dangling
//     dependency, so Assemble fails it closed as campaign_dangling_dependency,
//     exactly as a cross-epic dangling edge does today;
//   - an edge whose From is excluded is dropped silently (the depending item is
//     not in the campaign, so its dependency is irrelevant).
//
// Any pre-existing DroppedEdges on res are carried through unchanged, so a
// subset filter never hides a dangling edge the provider already surfaced.
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

	// Re-partition the edges against the included set. Carry any pre-existing
	// dropped edges through unchanged.
	var edges []workmgmt.DependsEdge
	dropped := make([]workmgmt.DependsEdge, 0, len(res.DroppedEdges))
	dropped = append(dropped, res.DroppedEdges...)
	for _, e := range res.Edges {
		_, fromIn := included[e.From]
		_, toIn := included[e.To]
		switch {
		case fromIn && toIn:
			edges = append(edges, e)
		case fromIn && !toIn:
			// An included item depends on an excluded one: a dangling
			// dependency, surfaced closed exactly like a cross-epic edge.
			dropped = append(dropped, e)
		default:
			// From is excluded: the depending item is not in the campaign, so
			// drop the edge silently.
		}
	}

	return &workmgmt.EpicChildrenResult{
		Children:     children,
		Edges:        edges,
		DroppedEdges: dropped,
	}, nil
}

// parseItemRef parses a subset item ref into a child issue number. It accepts
// both the bare-number ("101") and issue:N ("issue:101") forms, mirroring the
// issue:N ref convention Assemble emits. A ref in any other shape is a caller
// error (an unresolvable subset item), returned as a wrapped ErrItemNotChild so
// it maps onto the same fail-closed 422 as a non-child number.
func parseItemRef(ref string) (int, error) {
	s := strings.TrimSpace(ref)
	s = strings.TrimPrefix(s, "issue:")
	num, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil {
		return 0, fmt.Errorf("%w: %q is not a valid issue ref (want a number or issue:N)", ErrItemNotChild, ref)
	}
	return num, nil
}
