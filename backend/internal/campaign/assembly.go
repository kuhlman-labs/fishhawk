package campaign

import (
	"context"
	"errors"
	"fmt"
	"strconv"

	"github.com/kuhlman-labs/fishhawk/backend/internal/plan"
	"github.com/kuhlman-labs/fishhawk/backend/internal/workmgmt"
)

// Sentinel errors returned by Assemble. Callers errors.Is against these
// without depending on the underlying message.
var (
	// ErrDanglingDependency is returned (wrapped) when the epic-children
	// result carries a depends_on edge whose target is not a fellow child —
	// a typo'd number or a real cross-epic dependency. Assembly fails closed
	// on it rather than dropping it, so a missing dependency is surfaced
	// loudly instead of producing a campaign with a silently-broken edge.
	ErrDanglingDependency = errors.New("campaign: dangling depends_on dependency")
	// ErrCycle is returned (wrapped) when the child depends_on edges contain
	// a cycle (or an out-of-range edge), so plan.Waves cannot topologically
	// order the items.
	ErrCycle = errors.New("campaign: dependency cycle among campaign items")
)

// Assembly is the in-memory result of decomposing an epic's children into a
// wave-ordered campaign DAG, before it is persisted. It is produced by
// Assemble and consumed by Persist; the two are split so the pure DAG logic
// is unit-testable without a Repository (Postgres).
type Assembly struct {
	// EpicRef is the epic the campaign decomposes, in the `issue:N` ref
	// convention (matching Campaign.EpicRef).
	EpicRef string
	// Items are the assembled campaign items in ascending issue-number order
	// (the same order EpicChildren returns children), each stamped with its
	// 0-based wave index.
	Items []AssembledItem
	// Waves is the topological dispatch order: Waves[w] holds the issue refs
	// eligible in wave w. Wave 0 holds every item with no dependency.
	Waves [][]string
	// PausePolicy is the operator-chosen pause behavior carried through to the
	// persisted campaign (E25.7). OPTIONAL: a zero value is normalized to
	// PausePolicyPauseCampaign in Persist before the campaign is created, so
	// the existing call site (which does not set it) yields the conservative
	// block-the-campaign default. Slice 3 sets this from the create request.
	PausePolicy PausePolicy
	// OperatorAgent is the OPTIONAL campaign-level delegation override (E25.12),
	// raw JSONB bytes threaded straight through Persist onto the campaign.
	// Nil = no override (each issue-run inherits its workflow's contract). The
	// server sets it from the validated create request; the campaign package
	// never interprets it.
	OperatorAgent []byte
}

// AssembledItem is one campaign item produced by Assemble: its issue ref, the
// sibling issue refs it depends on, and the wave it lands in.
type AssembledItem struct {
	IssueRef  string   // e.g. "issue:1441"
	DependsOn []string // sibling issue refs this item waits on
	Wave      int      // 0-based topological wave index
}

// issueRef formats a child issue number into the campaign `issue:N` ref
// convention used by Campaign.EpicRef and Item.IssueRef.
func issueRef(number int) string {
	return "issue:" + strconv.Itoa(number)
}

// Assemble decomposes an epic's children (the workmgmt.EpicChildrenResult the
// provider emits) into a wave-ordered campaign DAG. It reuses plan.Waves for
// the topological sort by mapping each child issue number to an ascending
// 0-based index and back.
//
// It fails closed in two ways:
//   - any DroppedEdges (a depends_on target that is not a fellow child) yields
//     a wrapped ErrDanglingDependency naming the mis-targeted edges, so a
//     missing dependency blocks assembly rather than being silently dropped;
//   - a cycle or out-of-range edge surfaced by plan.Waves yields a wrapped
//     ErrCycle.
func Assemble(epicRef string, res *workmgmt.EpicChildrenResult) (*Assembly, error) {
	if res == nil {
		return nil, errors.New("campaign: nil epic-children result")
	}

	// Fail closed on any mis-targeted depends_on edge surfaced by the
	// provider. A dangling edge means a declared dependency cannot be honored
	// within the epic, so the campaign would be built on a broken graph.
	if len(res.DroppedEdges) > 0 {
		parts := make([]string, 0, len(res.DroppedEdges))
		for _, e := range res.DroppedEdges {
			parts = append(parts, fmt.Sprintf("%s->%s", issueRef(e.From), issueRef(e.To)))
		}
		return nil, fmt.Errorf("%w: %v", ErrDanglingDependency, parts)
	}

	// Ascending index map: child issue number -> 0-based index. Children are
	// already returned ascending by number, so index i is children[i].
	indexOf := make(map[int]int, len(res.Children))
	for i, c := range res.Children {
		indexOf[c.Number] = i
	}

	// Build a plan.Decomposition whose sub-plan i carries the indices of the
	// children that child i depends on. Edge {From,To} => item From depends on
	// To. Both endpoints are guaranteed to be children here (dangling edges
	// were rejected above), so every lookup resolves.
	decomp := plan.Decomposition{SubPlans: make([]plan.SubPlanSummary, len(res.Children))}
	for _, e := range res.Edges {
		from, okFrom := indexOf[e.From]
		to, okTo := indexOf[e.To]
		if !okFrom || !okTo {
			// Defensive: an edge endpoint that is not a child should have been
			// surfaced as a dropped edge by the provider. Fail closed rather
			// than panic on the map miss.
			return nil, fmt.Errorf("%w: edge %s->%s references a non-child", ErrDanglingDependency, issueRef(e.From), issueRef(e.To))
		}
		decomp.SubPlans[from].DependsOn = append(decomp.SubPlans[from].DependsOn, to)
	}

	waveIdx, err := plan.Waves(&decomp)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrCycle, err)
	}

	// Translate the wave indices back to issue refs and stamp each item's
	// wave. waveOf indexes child index -> wave number.
	waveOf := make([]int, len(res.Children))
	waves := make([][]string, len(waveIdx))
	for w, wave := range waveIdx {
		refs := make([]string, 0, len(wave))
		for _, idx := range wave {
			waveOf[idx] = w
			refs = append(refs, issueRef(res.Children[idx].Number))
		}
		waves[w] = refs
	}

	items := make([]AssembledItem, len(res.Children))
	for i, c := range res.Children {
		var deps []string
		for _, depIdx := range decomp.SubPlans[i].DependsOn {
			deps = append(deps, issueRef(res.Children[depIdx].Number))
		}
		items[i] = AssembledItem{
			IssueRef:  issueRef(c.Number),
			DependsOn: deps,
			Wave:      waveOf[i],
		}
	}

	return &Assembly{EpicRef: epicRef, Items: items, Waves: waves}, nil
}

// Persist materializes an Assembly into durable rows via the Repository: it
// creates the campaign (repo + epic ref), then one item per assembled item
// (issue ref + depends_on). It is a thin sequencing helper — the DAG logic
// lives in Assemble — so Track C / E25.4 can assemble-and-store in one call.
// Returns the created Campaign.
func Persist(ctx context.Context, repo Repository, repoName string, a *Assembly) (*Campaign, error) {
	if a == nil {
		return nil, errors.New("campaign: nil assembly")
	}
	c, err := repo.CreateCampaign(ctx, CreateCampaignParams{
		Repo:    repoName,
		EpicRef: a.EpicRef,
		// Normalize the optional policy to the block-the-campaign default
		// BEFORE building the params, so the existing call site (which leaves
		// PausePolicy zero) persists as pause_campaign and compiles unchanged.
		PausePolicy: normalizePausePolicy(a.PausePolicy),
		// Thread the optional campaign-level operator_agent override straight
		// through (E25.12): nil = no override.
		OperatorAgent: a.OperatorAgent,
	})
	if err != nil {
		return nil, fmt.Errorf("campaign: create campaign for %s: %w", a.EpicRef, err)
	}
	for _, it := range a.Items {
		if _, err := repo.CreateCampaignItem(ctx, CreateCampaignItemParams{
			CampaignID: c.ID,
			IssueRef:   it.IssueRef,
			DependsOn:  it.DependsOn,
		}); err != nil {
			return nil, fmt.Errorf("campaign: create item %s: %w", it.IssueRef, err)
		}
	}
	return c, nil
}
