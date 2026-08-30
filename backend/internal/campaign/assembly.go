package campaign

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

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

// DanglingDependencyError is the typed error Assemble returns when a
// campaign's dropped depends_on edges block assembly. It categorizes the
// blocking edges by cause so the create handler can enrich the 422 details map
// (dangling_not_child / dangling_excluded_incomplete) and the MCP tool can name
// the real remedy per cause, without string-parsing the message (#2120). It
// wraps ErrDanglingDependency, so errors.Is(err, ErrDanglingDependency) still
// holds for every existing caller.
type DanglingDependencyError struct {
	// NotChild are edges whose target is not a fellow child of the epic — a
	// typo'd number or a genuine cross-epic dependency (DropNotChild, or an
	// unclassified/zero-reason dropped edge, defensively).
	NotChild []workmgmt.DependsEdge
	// ExcludedIncomplete are edges from an included subset item to a fellow
	// child excluded from the items subset that is not yet complete
	// (DropExcludedIncomplete).
	ExcludedIncomplete []workmgmt.DependsEdge
	// ClosedIncomplete are edges whose out-of-set target is CLOSED but NOT
	// completed — closed as not_planned/duplicate (DropTargetClosedIncomplete,
	// #2953). Its work did not land, so widening the batch cannot satisfy it.
	ClosedIncomplete []workmgmt.DependsEdge
	// StateUnreadable are edges whose out-of-set target's issue state could not
	// be read (DropTargetStateUnreadable, #2953), so satisfaction is UNPROVEN
	// and the refusal stands.
	StateUnreadable []workmgmt.DependsEdge
}

// Error renders one clause per non-empty category. The not_child clause keeps
// the pre-#2120 "not a fellow child of the epic" wording (an out-of-epic cause);
// the excluded_incomplete clause names the real cause and the include/omit
// remedy; the closed_incomplete clause (#2953) says the target closed without
// completing so widening cannot satisfy it; the state_unreadable clause (#2953)
// says the target's state could not be read so the dependency is unproven.
func (e *DanglingDependencyError) Error() string {
	var clauses []string
	if len(e.NotChild) > 0 {
		clauses = append(clauses, fmt.Sprintf("%v target an issue that is not a fellow child of the epic", formatEdges(e.NotChild)))
	}
	if len(e.ExcludedIncomplete) > 0 {
		clauses = append(clauses, fmt.Sprintf("%v depend on a sibling excluded from items that is not yet complete — include it in items, or omit items to sweep every child so a completed dependency auto-settles", formatEdges(e.ExcludedIncomplete)))
	}
	if len(e.ClosedIncomplete) > 0 {
		clauses = append(clauses, fmt.Sprintf("%v target an issue that is closed WITHOUT completing (not_planned/duplicate) — its work did not land, so widening the batch cannot satisfy it; reopen or replace the dependency, or drop the edge", formatEdges(e.ClosedIncomplete)))
	}
	if len(e.StateUnreadable) > 0 {
		clauses = append(clauses, fmt.Sprintf("%v target an issue whose state could not be read, so the dependency is UNPROVEN and assembly refused rather than assume satisfaction — retry", formatEdges(e.StateUnreadable)))
	}
	return fmt.Sprintf("%s: %s", ErrDanglingDependency.Error(), strings.Join(clauses, "; "))
}

// Unwrap makes errors.Is(err, ErrDanglingDependency) hold so the sentinel-based
// status mapping (server + refinement) is unchanged.
func (*DanglingDependencyError) Unwrap() error { return ErrDanglingDependency }

// formatEdges renders a slice of edges as `[issue:From->issue:To ...]` in the
// campaign ref convention, matching the pre-#2120 message shape. The target side
// routes through the single renderer DependsEdge.TargetRef (#2956): a resolvable
// edge still renders `issue:To`, while an unresolvable edge renders
// `unparsable:<digest>:"token"` instead of the identity-less `issue:0`.
func formatEdges(edges []workmgmt.DependsEdge) []string {
	parts := make([]string, 0, len(edges))
	for _, e := range edges {
		parts = append(parts, fmt.Sprintf("%s->%s", issueRef(e.From), e.TargetRef()))
	}
	return parts
}

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
	// IdempotencyKey is the OPTIONAL create idempotency key (E25.13), threaded
	// straight through Persist onto the campaign. Nil = no key (the unchanged
	// default). The server sets it from a non-empty Idempotency-Key header.
	IdempotencyKey *string
	// WorkingDir is the OPTIONAL campaign-level checkout binding (E48.87 /
	// #2527), threaded straight through Persist onto the campaign. Empty = no
	// binding (the unchanged default: each item run falls back to #2498's
	// explicit per-item working_dir). The server sets it from the validated
	// create request.
	WorkingDir string
	// GroomingSource is the OPTIONAL durable provenance block for a campaign
	// created from an approved grooming order (E54.6 / #2238), threaded straight
	// through Persist onto the campaign row's own INSERT. Nil = not
	// grooming-sourced (every epic_ref / explicit-items campaign). The server
	// marshals it; the campaign package never interprets these bytes.
	GroomingSource []byte
	// SatisfiedDependencies are the depends_on edges elided during assembly
	// because their out-of-set target was already closed-and-completed (#2953),
	// carried verbatim from the resolved EpicChildrenResult.SatisfiedEdges. It is
	// a PURE passthrough: Assemble never builds a DAG edge or wave dependency
	// from it — that is exactly what "excluded from the wave DAG" means. It is
	// NOT persisted; the server renders it into the create-response-only
	// satisfied_dependencies block and a best-effort audit so an operator can
	// tell "this dependency is done" from "this dependency was ignored".
	SatisfiedDependencies []workmgmt.SatisfiedEdge
}

// AssembledItem is one campaign item produced by Assemble: its issue ref, the
// sibling issue refs it depends on, and the wave it lands in.
type AssembledItem struct {
	IssueRef  string   // e.g. "issue:1441"
	DependsOn []string // sibling issue refs this item waits on
	Wave      int      // 0-based topological wave index
	// Autonomy is the item's autonomy tier (low|medium|high), copied from the
	// source EpicChild.Autonomy. Empty when the child carried no autonomy label.
	// Persist threads it onto CreateCampaignItemParams so it reaches the
	// campaign_items.autonomy column (#1551 / E32.4).
	Autonomy string
	// Position is the item's 0-based place in the campaign queue, stamped by
	// Assemble from the item's index in Items and threaded by Persist onto
	// campaign_items.queue_position (migration 0074). Because Assemble preserves
	// the ORDER of res.Children, a caller that permutes Children before
	// assembling (campaign.ReorderByPriority, for the ratified grooming rank
	// order) gets that permutation written down DURABLY here rather than left to
	// insertion sequence — which is not an ordering, since every item of one
	// campaign shares one transaction timestamp.
	Position int
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
//   - any DroppedEdges (a depends_on target that is not a fellow child, an
//     excluded-but-incomplete subset sibling, a closed-but-not-completed target,
//     or an unreadable target) yields a *DanglingDependencyError (which wraps
//     ErrDanglingDependency) categorizing the mis-targeted edges by cause, so a
//     missing dependency blocks assembly rather than being silently dropped;
//   - a cycle or out-of-range edge surfaced by plan.Waves yields a wrapped
//     ErrCycle.
//
// A closed-AND-completed out-of-set target is NOT dangling (#2953): the provider
// records it in res.SatisfiedEdges (never DroppedEdges), and Assemble carries it
// through to Assembly.SatisfiedDependencies without building any DAG edge from
// it — so a batch whose prerequisite is already done assembles cleanly.
func Assemble(epicRef string, res *workmgmt.EpicChildrenResult) (*Assembly, error) {
	if res == nil {
		return nil, errors.New("campaign: nil epic-children result")
	}

	// Fail closed on any mis-targeted depends_on edge surfaced by the
	// provider (or the subset filter). A dangling edge means a declared
	// dependency cannot be honored within the campaign, so it would be built on
	// a broken graph. Group the edges by drop reason so the failure names the
	// real cause per edge: an out-of-epic target keeps the "not a fellow child"
	// wording, while an excluded-but-incomplete sibling names the include/omit
	// remedy (#2120). An empty/unknown reason defaults to not_child, so a
	// pre-existing dropped edge with no reason keeps the current wording.
	if len(res.DroppedEdges) > 0 {
		derr := &DanglingDependencyError{}
		for _, e := range res.DroppedEdges {
			switch e.Reason {
			case workmgmt.DropExcludedIncomplete:
				derr.ExcludedIncomplete = append(derr.ExcludedIncomplete, e)
			case workmgmt.DropTargetClosedIncomplete:
				derr.ClosedIncomplete = append(derr.ClosedIncomplete, e)
			case workmgmt.DropTargetStateUnreadable:
				derr.StateUnreadable = append(derr.StateUnreadable, e)
			default:
				// DropNotChild, or an empty/unknown reason defensively, keeps the
				// pre-existing not_child wording so nothing pre-#2120 changes.
				derr.NotChild = append(derr.NotChild, e)
			}
		}
		return nil, derr
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
			// Carry the source child's autonomy tier through so Persist can
			// stamp campaign_items.autonomy (#1551 / E32.4). Normalize as a
			// campaign-layer backstop to the fail-closed 0049 CHECK: the parse
			// boundary already maps an unrecognized `autonomy:<tier>` label to ""
			// (non-human-led default), but a raw out-of-set tier reaching Assemble
			// from any other source degrades to "" here rather than aborting the
			// entire epic campaign with a CHECK violation at Persist.
			Autonomy: normalizeAutonomy(c.Autonomy),
			// Dense 0-based queue position from the assembled order (migration
			// 0074). i is the index into res.Children, so this records exactly the
			// order the caller handed in — including a ReorderByPriority
			// permutation into ratified rank order.
			Position: i,
		}
	}

	return &Assembly{
		EpicRef: epicRef,
		Items:   items,
		Waves:   waves,
		// Pure passthrough of the elided-because-already-satisfied edges (#2953):
		// no DAG edge was built from any of them above, so surfacing them here is
		// visibility only.
		SatisfiedDependencies: res.SatisfiedEdges,
	}, nil
}

// normalizeAutonomy maps an autonomy tier to the set the campaign_items.autonomy
// CHECK (migration 0049) permits: "", "low", "medium", "high". Any other value
// (an out-of-set tier that slipped past the label parse boundary) normalizes to
// "" — the unknown/default tier the engine treats as non-human-led — so a single
// mislabeled child can never abort persistence of the whole epic campaign.
func normalizeAutonomy(tier string) string {
	switch tier {
	case "low", "medium", "high":
		return tier
	default:
		return ""
	}
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
		// Thread the optional create idempotency key straight through (E25.13):
		// nil = no key.
		IdempotencyKey: a.IdempotencyKey,
		// Thread the optional campaign-level checkout binding straight through
		// (E48.87 / #2527): empty = no binding.
		WorkingDir: a.WorkingDir,
		// Thread the optional durable grooming provenance straight through
		// (E54.6 / #2238). It rides the campaign's OWN single-row INSERT, so the
		// campaign and its provenance are created atomically: this function is
		// NOT transactional (one CreateCampaign then N CreateCampaignItem), so a
		// provenance record written after the fact could be lost while the
		// campaign survived. Nil = not grooming-sourced.
		GroomingSource: a.GroomingSource,
	})
	if err != nil {
		return nil, fmt.Errorf("campaign: create campaign for %s: %w", a.EpicRef, err)
	}
	for _, it := range a.Items {
		if _, err := repo.CreateCampaignItem(ctx, CreateCampaignItemParams{
			CampaignID: c.ID,
			IssueRef:   it.IssueRef,
			DependsOn:  it.DependsOn,
			// Thread the autonomy tier onto the persisted item so the engine can
			// read it back for the HumanLed partition (#1551 / E32.4).
			Autonomy: it.Autonomy,
			// Thread the queue position onto the persisted item so the assembled
			// order is DURABLE (migration 0074) rather than implied by insertion
			// sequence — every insert below shares one transaction timestamp, so
			// (created_at, id) ordering would collapse to the random-UUID tiebreak.
			Position: it.Position,
		}); err != nil {
			return nil, fmt.Errorf("campaign: create item %s: %w", it.IssueRef, err)
		}
	}
	return c, nil
}
