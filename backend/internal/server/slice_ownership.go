package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strconv"
	"strings"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/plan"
	"github.com/kuhlman-labs/fishhawk/backend/internal/scopeamendment"
)

// CategoryScopeAmendmentRefused is the audit-log category for the entry the
// slice-ownership guard writes when it refuses a decomposition child's
// mid-stage scope amendment (#4679): at REQUEST time (phase "request", actor
// agent, no amendment row created) or at an operator's APPROVE (phase
// "decide", actor user, the row stays pending). Payload carries {phase, paths,
// slice_ownership} plus reason (request) or amendment_id (decide). Internal
// audit kind — NOT an issue-comment surface — and WITHHELD from run-bound
// audit reads (withholdScopeOverlapFromRunBound), because its slice_ownership
// names sibling slices' titles, paths and child run ids.
const CategoryScopeAmendmentRefused = "scope_amendment_refused"

// errCodeAmendmentPathOwnedBySibling is the 409 code both scope-amendment
// routes answer when a requested path is owned by a non-ancestor sibling
// slice (#4679).
const errCodeAmendmentPathOwnedBySibling = "amendment_path_owned_by_sibling_slice"

// errCodeSliceOwnershipCheckFailed is the decide route's fail-CLOSED 500 when
// the slice-ownership check could not read the run or the parent's plan.
const errCodeSliceOwnershipCheckFailed = "slice_ownership_check_failed"

// sliceOwnershipRefusalReason is the details.reason of the 409.
const sliceOwnershipRefusalReason = "path_owned_by_non_ancestor_slice"

// sliceOwnershipRemedy is the remedy both the 409 body and the refusal audit
// row carry. There is no override: the only ways forward are the owning slice
// or a follow-up.
const sliceOwnershipRemedy = "leave this edit to the owning slice (it is in that slice's scope.files) or defer it to a follow-up issue; " +
	"do NOT edit the path — re-file the amendment without it if other paths are still needed"

// sliceOwnershipOverlap is one requested path overlapping a path another slice
// DECLARES in the parent's approved decomposition.
type sliceOwnershipOverlap struct {
	Path             string `json:"path"`
	OwningSlice      int    `json:"owning_slice"`
	OwningTitle      string `json:"owning_title"`
	OwningPath       string `json:"owning_path"`
	OwningChildRunID string `json:"owning_child_run_id,omitempty"`
	OverlapKind      string `json:"overlap_kind"`
}

// sliceOwnershipCheck is the slice-ownership verdict for one amendment from a
// decomposition child at SliceIndex. Conflicts are overlaps with a slice that
// is NOT a transitive depends_on ancestor (refused); AncestorOwned are overlaps
// with an ancestor slice, whose commits are already in the requester's base
// (allowed, recorded). Checked is false only on a fail-open payload.
type sliceOwnershipCheck struct {
	SliceIndex     int                     `json:"slice_index"`
	AncestorSlices []int                   `json:"ancestor_slices"`
	Conflicts      []sliceOwnershipOverlap `json:"conflicts"`
	AncestorOwned  []sliceOwnershipOverlap `json:"ancestor_owned"`
	Checked        bool                    `json:"checked"`
	Error          string                  `json:"error,omitempty"`
}

// refused reports whether the check found a non-ancestor owner. Nil-safe: a
// nil check (the run is not a decomposition child, or the plan is absent)
// never refuses.
func (c *sliceOwnershipCheck) refused() bool { return c != nil && len(c.Conflicts) > 0 }

// sliceAncestors returns the TRANSITIVE depends_on closure of slice idx: every
// slice idx depends on directly or through another dependency. idx itself is
// excluded, out-of-range edges are ignored, and a cyclic depends_on (refused at
// the plan gate, handled defensively here) terminates through the visited set.
// Pure.
func sliceAncestors(subPlans []plan.SubPlanSummary, idx int) map[int]bool {
	out := make(map[int]bool)
	if idx < 0 || idx >= len(subPlans) {
		return out
	}
	stack := append([]int(nil), subPlans[idx].DependsOn...)
	for len(stack) > 0 {
		n := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		if n < 0 || n >= len(subPlans) || n == idx || out[n] {
			continue
		}
		out[n] = true
		stack = append(stack, subPlans[n].DependsOn...)
	}
	return out
}

// sortedSliceSet returns a set's members ascending, never nil.
func sortedSliceSet(set map[int]bool) []int {
	out := make([]int, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	sort.Ints(out)
	return out
}

// classifySliceOwnership checks each requested path against every OTHER
// slice's declared scope.files (scopePathsOverlap: identical or directory
// containment on segment boundaries). An overlap with a transitive depends_on
// ancestor is allowed (AncestorOwned): the ancestor's commits are in the
// requester's base. Any other owner — a same-wave sibling, an earlier-wave
// non-dependency, a descendant — is a Conflict. Same-slice overlaps are never
// recorded. Order: requested-path order, then slice index ascending, then the
// owner's declared order. Pure.
func classifySliceOwnership(subPlans []plan.SubPlanSummary, sliceIdx int, paths []string) *sliceOwnershipCheck {
	ancestors := sliceAncestors(subPlans, sliceIdx)
	c := &sliceOwnershipCheck{
		SliceIndex:     sliceIdx,
		AncestorSlices: sortedSliceSet(ancestors),
		Conflicts:      []sliceOwnershipOverlap{},
		AncestorOwned:  []sliceOwnershipOverlap{},
		Checked:        true,
	}
	for _, p := range paths {
		for j, sp := range subPlans {
			if j == sliceIdx || sp.Scope == nil {
				continue
			}
			for _, f := range sp.Scope.Files {
				if !scopePathsOverlap(p, f.Path) {
					continue
				}
				o := sliceOwnershipOverlap{
					Path:        p,
					OwningSlice: j,
					OwningTitle: sp.Title,
					OwningPath:  f.Path,
					OverlapKind: overlapKind(p, f.Path),
				}
				if ancestors[j] {
					c.AncestorOwned = append(c.AncestorOwned, o)
				} else {
					c.Conflicts = append(c.Conflicts, o)
				}
			}
		}
	}
	return c
}

// amendmentSliceOwnership runs the slice-ownership check for an amendment on
// runID (#4679). It returns (nil, nil) — nothing to check — when the RunRepo is
// unconfigured, when the run is not a decomposition child (DecomposedFrom or
// SliceIndex nil), or when the parent's approved plan is ABSENT (nil, no
// decomposition, or SliceIndex out of range): the same ABSENT partition
// resolveSliceDependencies documents. A run or plan read ERROR returns a
// non-nil error; the request route fails open on it, the decide route fails
// closed. So does a decomposition child with no ArtifactRepo: the parent's
// plan is unreadable, not absent, and loadApprovedPlanForRun's own nil-repo
// (nil, nil) would otherwise read as ABSENT and admit the approve. OwningChildRunID is filled best-effort from the parent's minted
// children: a sibling-list error WARN-logs and leaves the ids empty, because
// the decision depends only on the plan.
func (s *Server) amendmentSliceOwnership(ctx context.Context, runID uuid.UUID, paths []scopeamendment.PathEntry) (*sliceOwnershipCheck, error) {
	if s.cfg.RunRepo == nil {
		return nil, nil
	}
	runRow, err := s.cfg.RunRepo.GetRun(ctx, runID)
	if err != nil {
		return nil, fmt.Errorf("get run: %w", err)
	}
	if runRow.DecomposedFrom == nil || runRow.SliceIndex == nil {
		return nil, nil
	}
	parentID := *runRow.DecomposedFrom
	if s.cfg.ArtifactRepo == nil {
		return nil, errors.New("artifact repository unconfigured: the parent's approved plan cannot be read")
	}
	p, err := s.loadApprovedPlanForRun(ctx, parentID)
	if err != nil {
		return nil, fmt.Errorf("load parent plan: %w", err)
	}
	idx := *runRow.SliceIndex
	if p == nil || p.Decomposition == nil || idx < 0 || idx >= len(p.Decomposition.SubPlans) {
		return nil, nil
	}
	reqPaths := make([]string, 0, len(paths))
	for _, pe := range paths {
		reqPaths = append(reqPaths, pe.Path)
	}
	check := classifySliceOwnership(p.Decomposition.SubPlans, idx, reqPaths)
	if len(check.Conflicts) == 0 && len(check.AncestorOwned) == 0 {
		return check, nil
	}
	siblings, err := s.listDecomposedSiblings(ctx, parentID)
	if err != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn,
			"slice ownership: list decomposed siblings failed; owning child run ids omitted",
			slog.String("run_id", runID.String()),
			slog.String("parent_run_id", parentID.String()),
			slog.String("error", err.Error()))
		return check, nil
	}
	childBySlice := make(map[int]string, len(siblings))
	for _, sib := range siblings {
		if sib == nil || sib.SliceIndex == nil {
			continue
		}
		if _, seen := childBySlice[*sib.SliceIndex]; !seen {
			childBySlice[*sib.SliceIndex] = sib.ID.String()
		}
	}
	for i := range check.Conflicts {
		check.Conflicts[i].OwningChildRunID = childBySlice[check.Conflicts[i].OwningSlice]
	}
	for i := range check.AncestorOwned {
		check.AncestorOwned[i].OwningChildRunID = childBySlice[check.AncestorOwned[i].OwningSlice]
	}
	return check, nil
}

// sliceOwnershipFailOpen is the slice_ownership payload the request route
// records when the check could not run: checked=false plus the read error.
func sliceOwnershipFailOpen(err error) map[string]any {
	return map[string]any{"checked": false, "error": err.Error()}
}

// sliceOwnershipRefusalMessage renders the 409 message: each conflicting path,
// its owning slice (index, title, child run id), the requesting slice and its
// ancestor set, and the remedy. phase "decide" addresses the operator: there is
// no override, so the message tells them to deny.
func sliceOwnershipRefusalMessage(c *sliceOwnershipCheck, phase string) string {
	parts := make([]string, 0, len(c.Conflicts))
	for _, o := range c.Conflicts {
		owner := fmt.Sprintf("slice %d (%s", o.OwningSlice, strconv.Quote(o.OwningTitle))
		if o.OwningChildRunID != "" {
			owner += ", child run " + o.OwningChildRunID
		}
		owner += ")"
		as := ""
		if o.OwningPath != o.Path {
			as = fmt.Sprintf(" as %s (%s)", o.OwningPath, o.OverlapKind)
		}
		parts = append(parts, fmt.Sprintf("%s is owned by %s%s", o.Path, owner, as))
	}
	ancestors := "none"
	if len(c.AncestorSlices) > 0 {
		ss := make([]string, 0, len(c.AncestorSlices))
		for _, a := range c.AncestorSlices {
			ss = append(ss, strconv.Itoa(a))
		}
		ancestors = strings.Join(ss, ", ")
	}
	msg := fmt.Sprintf("scope amendment refused (#4679): %s; the owning slice is not a depends_on ancestor of this slice %d "+
		"(ancestors: %s), so its commits are not in this slice's base and editing the path here would conflict at fan-in. ",
		strings.Join(parts, "; "), c.SliceIndex, ancestors)
	if phase == "decide" {
		return msg + "There is no override: DENY this amendment with the remedy as the reason — " + sliceOwnershipRemedy
	}
	return msg + "Remedy: " + sliceOwnershipRemedy
}

// sliceOwnershipRefusalDetails is the 409 details object.
func sliceOwnershipRefusalDetails(c *sliceOwnershipCheck) map[string]any {
	return map[string]any{
		"reason":          sliceOwnershipRefusalReason,
		"requested_slice": c.SliceIndex,
		"ancestor_slices": c.AncestorSlices,
		"conflicts":       c.Conflicts,
		"remedy":          sliceOwnershipRemedy,
	}
}

// sliceSharedPathOverlap is one pair of DISTINCT decomposition slices whose
// declared scope.files overlap by directory containment or trailing-slash
// spelling — the overlaps plan.checkCrossSliceSharedFiles does not refuse
// (it refuses raw-equal paths in every wave). Related is true when either
// slice is a transitive depends_on ancestor of the other.
type sliceSharedPathOverlap struct {
	SliceA, SliceB int
	TitleA, TitleB string
	PathA, PathB   string
	WaveA, WaveB   int
	Related        bool
}

// sliceSharedPathOverlaps lists every cross-slice ownership overlap the
// decomposition validator does not already refuse (#4679), ordered by slice
// pair then declared order. A raw-equal pair is skipped (the validator refuses
// it). Returns nil for a nil or empty decomposition and when plan.Waves errors
// (a cyclic or out-of-range depends_on, which the validator refuses). Pure.
func sliceSharedPathOverlaps(d *plan.Decomposition) []sliceSharedPathOverlap {
	if d == nil || len(d.SubPlans) == 0 {
		return nil
	}
	waves, err := plan.Waves(d)
	if err != nil {
		return nil
	}
	waveOf := make(map[int]int, len(d.SubPlans))
	for w, members := range waves {
		for _, m := range members {
			waveOf[m] = w
		}
	}
	var out []sliceSharedPathOverlap
	for a := 0; a < len(d.SubPlans); a++ {
		sa := d.SubPlans[a]
		if sa.Scope == nil {
			continue
		}
		ancA := sliceAncestors(d.SubPlans, a)
		for b := a + 1; b < len(d.SubPlans); b++ {
			sb := d.SubPlans[b]
			if sb.Scope == nil {
				continue
			}
			related := ancA[b] || sliceAncestors(d.SubPlans, b)[a]
			for _, fa := range sa.Scope.Files {
				for _, fb := range sb.Scope.Files {
					if fa.Path == fb.Path || !scopePathsOverlap(fa.Path, fb.Path) {
						continue
					}
					out = append(out, sliceSharedPathOverlap{
						SliceA: a, SliceB: b,
						TitleA: sa.Title, TitleB: sb.Title,
						PathA: fa.Path, PathB: fb.Path,
						WaveA: waveOf[a], WaveB: waveOf[b],
						Related: related,
					})
				}
			}
		}
	}
	return out
}

// sliceSharedPathWarnings renders one plan_warnings advisory per
// sliceSharedPathOverlaps entry (#4679): the file, both slices and their waves,
// the dependency relation, and the remedy. Nil when nothing overlaps.
func sliceSharedPathWarnings(p *plan.Plan) []string {
	if p == nil {
		return nil
	}
	overlaps := sliceSharedPathOverlaps(p.Decomposition)
	if len(overlaps) == 0 {
		return nil
	}
	out := make([]string, 0, len(overlaps))
	for _, o := range overlaps {
		waves := "in the same wave"
		if o.WaveA != o.WaveB {
			waves = "in different waves"
		}
		relation := "neither slice depends on the other, so both edit it on independent branches and the fan-in can conflict"
		if o.Related {
			relation = "one slice depends on the other, so their edits land in sequence, but a mid-stage scope amendment from any other slice naming it is refused"
		}
		out = append(out, fmt.Sprintf(
			"decomposition shared path (#4679): slice %d (%s, wave %d) declares %s and slice %d (%s, wave %d) declares %s — "+
				"the two overlap (%s) %s; %s. Give the file a single owner, or add a final integration slice that depends on both and owns it.",
			o.SliceA, strconv.Quote(o.TitleA), o.WaveA, o.PathA,
			o.SliceB, strconv.Quote(o.TitleB), o.WaveB, o.PathB,
			overlapKind(o.PathA, o.PathB), waves, relation))
	}
	return out
}

// surfacePatternSliceSharedPath is the CrossSliceCouplingFinding pattern name
// for a sliceSharedPathOverlaps entry (#4679).
const surfacePatternSliceSharedPath = "shared path across decomposition slices"

// sliceSharedPathFindings maps each sliceSharedPathOverlaps entry to a
// CrossSliceCouplingFinding, so the plan reviewers see it on the existing
// CROSS-SLICE COUPLING line. Slices are the two claims sorted by title. Nil
// when nothing overlaps. Pure.
func sliceSharedPathFindings(p *plan.Plan) []CrossSliceCouplingFinding {
	if p == nil {
		return nil
	}
	overlaps := sliceSharedPathOverlaps(p.Decomposition)
	if len(overlaps) == 0 {
		return nil
	}
	out := make([]CrossSliceCouplingFinding, 0, len(overlaps))
	for _, o := range overlaps {
		claims := []CrossSliceClaim{
			{SliceTitle: o.TitleA, Files: []string{o.PathA}},
			{SliceTitle: o.TitleB, Files: []string{o.PathB}},
		}
		sort.SliceStable(claims, func(i, j int) bool { return claims[i].SliceTitle < claims[j].SliceTitle })
		out = append(out, CrossSliceCouplingFinding{Pattern: surfacePatternSliceSharedPath, Slices: claims})
	}
	return out
}
