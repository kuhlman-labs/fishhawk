package plan

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// Milestone-scope typed domain and semantic rules (E54.9 / #2309).
//
// A milestone_scope takes a human-supplied release definition and produces a
// scoped, sequenced release plan: the items selected INTO the milestone (each
// with a membership reason and a wave index), the items scoped OUT, the
// ambiguous calls the class DECLINED to make rather than guessing, a critical
// path, and its confidence in the prose-derived dependency edges. It proposes
// no tracker mutation — this file is contract and validation only; the
// derivation is performed by the groom-stage agent (approval condition C1).
//
// The three new entry classes reuse #2235's derived-entry-id contract, so
// run-over-run identity stays mechanical. Their ids live under the same
// report-wide uniqueness / class-prefix / recomposition rules as the six
// existing arrays (collectGroomingEntries appends them); the EIGHT rules the
// schema cannot express are machine-checked here by checkMilestoneScope.

// The three milestone entry classes. Each is the leading segment of a derived
// entry id (see GroomingEntryID).
const (
	GroomingClassMilestoneInclusion = "milestone_inclusion"
	GroomingClassMilestoneExclusion = "milestone_exclusion"
	GroomingClassMilestoneDeclined  = "milestone_declined"
)

// milestoneFingerprintClass is the class tag on MilestoneScopeFingerprint's
// envelope, preventing a cross-class collision on an identical payload —
// mirroring workmgmt's groomingBasisHash envelope shape.
const milestoneFingerprintClass = "milestone_scope"

// MilestoneScope is the parsed milestone-scoping variant of a grooming report.
// JSON tags mirror grooming-report-v1.schema.json's `milestone-scope` $def.
type MilestoneScope struct {
	ReleaseDefinition MilestoneReleaseDefinition `json:"release_definition"`
	Framing           MilestoneFraming           `json:"framing"`
	Included          []MilestoneInclusion       `json:"included"`
	Excluded          []MilestoneExclusion       `json:"excluded"`
	DeclinedCalls     []MilestoneDeclinedCall    `json:"declined_calls"`
	// CriticalPath is an ORDERED sequence of item keys — its order IS its
	// content, so it is the one array canonical ordering does not sort.
	CriticalPath   []string                `json:"critical_path"`
	EdgeConfidence MilestoneEdgeConfidence `json:"edge_confidence"`
}

// MilestoneReleaseDefinition is the human input the class scopes against and
// must never invent — source is a single-value enum naming the operator.
type MilestoneReleaseDefinition struct {
	Label  string `json:"label"`
	Text   string `json:"text"`
	Source string `json:"source"`
}

// MilestoneFraming is the framing the class adopted plus its architectural
// basis — the step most likely to be plausible-and-wrong, stated and grounded.
type MilestoneFraming struct {
	Statement string `json:"statement"`
	Basis     string `json:"basis"`
}

// MilestoneInclusion is one item scoped INTO the milestone.
type MilestoneInclusion struct {
	ID      string  `json:"id"`
	ItemRef ItemRef `json:"item_ref"`
	Reason  string  `json:"reason"`
	Wave    int     `json:"wave"`
	// DependsOn holds item keys (not ids) this inclusion depends on; each must
	// resolve to an included or excluded item (R2).
	DependsOn []string `json:"depends_on,omitempty"`
	// OpenQuestionIDs references declined_calls[].question_id — the ambiguities
	// this inclusion is contingent on (R6).
	OpenQuestionIDs []string         `json:"open_question_ids,omitempty"`
	RubricCitations []RubricCitation `json:"rubric_citations"`
}

// MilestoneExclusion is one item scoped OUT. It carries a rubric citation on
// the same terms as an inclusion (approval condition C2).
type MilestoneExclusion struct {
	ID              string           `json:"id"`
	ItemRef         ItemRef          `json:"item_ref"`
	Reason          string           `json:"reason"`
	RubricCitations []RubricCitation `json:"rubric_citations"`
}

// MilestoneDeclinedCall is one ambiguous scope question the class refused to
// resolve. Its id derives from question_id via GroomingEntryID's zero-ref form.
type MilestoneDeclinedCall struct {
	ID                 string           `json:"id"`
	QuestionID         string           `json:"question_id"`
	Question           string           `json:"question"`
	WhyAmbiguous       string           `json:"why_ambiguous"`
	RecommendedDefault string           `json:"recommended_default,omitempty"`
	Options            []string         `json:"options,omitempty"`
	ItemRefs           []ItemRef        `json:"item_refs,omitempty"`
	RubricCitations    []RubricCitation `json:"rubric_citations"`
}

// MilestoneEdgeConfidence is the class's confidence in the dependency edges it
// derived, since a milestone's edges are usually read out of prose.
type MilestoneEdgeConfidence struct {
	Level string `json:"level"`
	Basis string `json:"basis"`
}

// checkMilestoneScope enforces the eight rules the schema cannot express, each
// returning a *SemanticError naming the offending JSON pointer. A no-op when
// the report carries no milestone_scope.
func checkMilestoneScope(gr *GroomingReport) error {
	ms := gr.MilestoneScope
	if ms == nil {
		return nil
	}
	// includedKeys / excludedKeys: the item key of each in-scope / out-of-scope
	// item, and the wave of each included item, so the graph rules below resolve
	// a depends_on key without re-deriving it per check.
	includedWave := make(map[string]int, len(ms.Included))
	for _, inc := range ms.Included {
		includedWave[itemKey(inc.ItemRef)] = inc.Wave
	}
	excludedKeys := make(map[string]bool, len(ms.Excluded))
	for _, exc := range ms.Excluded {
		excludedKeys[itemKey(exc.ItemRef)] = true
	}

	if err := checkMilestoneWaveContiguity(ms); err != nil {
		return err
	}
	// R8 runs BEFORE the dependency rules: a dual-membership item would otherwise
	// be resolved to its included interpretation by checkMilestoneDependencies
	// (which tests includedWave before excludedKeys), silently suppressing R4.
	if err := checkMilestoneMembershipDisjoint(ms, includedWave); err != nil {
		return err
	}
	if err := checkMilestoneDependencies(ms, includedWave, excludedKeys); err != nil {
		return err
	}
	if err := checkMilestoneCriticalPath(ms, includedWave); err != nil {
		return err
	}
	if err := checkMilestoneOpenQuestions(ms); err != nil {
		return err
	}
	return checkMilestoneCanonicalOrder(ms)
}

// checkMilestoneWaveContiguity is R1: the wave indices across `included` are
// exactly the contiguous set 0..K with no gaps. A gapped wave means the DAG
// layering does not describe an executable sequence.
func checkMilestoneWaveContiguity(ms *MilestoneScope) error {
	if len(ms.Included) == 0 {
		return nil
	}
	present := make(map[int]bool, len(ms.Included))
	maxWave := 0
	for _, inc := range ms.Included {
		present[inc.Wave] = true
		if inc.Wave > maxWave {
			maxWave = inc.Wave
		}
	}
	for w := 0; w <= maxWave; w++ {
		if !present[w] {
			return &SemanticError{Message: fmt.Sprintf(
				"/milestone_scope/included: wave %d is missing; waves must be the contiguous set 0..%d with no gaps (a gap means the layering is not an executable sequence)",
				w, maxWave)}
		}
	}
	return nil
}

// checkMilestoneMembershipDisjoint is R8: the `included` and `excluded` key sets
// must be disjoint — an item cannot be simultaneously scoped in and out of the
// milestone. Report-wide id uniqueness does NOT catch this, because the two
// derived ids differ by class prefix (milestone_inclusion:<key> vs
// milestone_exclusion:<key>); and without this rule checkMilestoneDependencies
// would resolve a dependency on a dual-membership item to its includedWave hit
// before the excludedKeys branch, so R4's out-of-scope-requires-a-declined-call
// rule would silently never fire for the contradictory input a validator exists
// to reject.
func checkMilestoneMembershipDisjoint(ms *MilestoneScope, includedWave map[string]int) error {
	for i, exc := range ms.Excluded {
		key := itemKey(exc.ItemRef)
		if _, both := includedWave[key]; both {
			return &SemanticError{Message: fmt.Sprintf(
				"/milestone_scope/excluded/%d: %q also appears in `included`; the included and excluded key sets must be DISJOINT — an item cannot be simultaneously scoped in and out of the milestone",
				i, key)}
		}
	}
	return nil
}

// checkMilestoneDependencies is R2, R3 and R4: every depends_on key resolves to
// an included OR excluded item (R2); an in-scope dependency's wave is STRICTLY
// LESS than the dependent's, which rejects a cycle by construction (R3); and an
// out-of-scope dependency REQUIRES the dependent to carry a declined call (R4).
func checkMilestoneDependencies(ms *MilestoneScope, includedWave map[string]int, excludedKeys map[string]bool) error {
	for i, inc := range ms.Included {
		dependentKey := itemKey(inc.ItemRef)
		dependentWave := inc.Wave
		for j, dep := range inc.DependsOn {
			depWave, inScope := includedWave[dep]
			switch {
			case inScope:
				// R3: strict monotonicity. A dependency in the same or a later
				// wave (dep >= dependent) admits no strict topological layering,
				// which is exactly the shape a cycle produces.
				if depWave >= dependentWave {
					return &SemanticError{Message: fmt.Sprintf(
						"/milestone_scope/included/%d/depends_on/%d: %q (wave %d) is depended on by %q (wave %d); an in-scope dependency must sit in a STRICTLY EARLIER wave — a same-or-later wave admits no topological layering and is how a dependency cycle presents",
						i, j, dep, depWave, dependentKey, dependentWave)}
				}
			case excludedKeys[dep]:
				// R4: an in-scope item depending on an out-of-scope one is an
				// ambiguous call — it must be declined, not resolved silently.
				if len(inc.OpenQuestionIDs) == 0 {
					return &SemanticError{Message: fmt.Sprintf(
						"/milestone_scope/included/%d: depends_on %q which is EXCLUDED from the milestone, but carries no open_question_ids; an in-scope item depending on an out-of-scope one is an ambiguous call that must be declined, not resolved silently",
						i, dep)}
				}
			default:
				// R2: unresolvable key — the "sequencing a graph with missing
				// edges" failure worse than no plan.
				return &SemanticError{Message: fmt.Sprintf(
					"/milestone_scope/included/%d/depends_on/%d: %q resolves to no item in `included` or `excluded`; a dependency on a missing item cannot be sequenced",
					i, j, dep)}
			}
		}
	}
	return nil
}

// checkMilestoneCriticalPath is R5 (with approval condition C3): every element
// resolves to an included item key, elements are pairwise distinct, their waves
// are strictly increasing, AND each consecutive pair is connected by a declared
// depends_on edge (element i+1 depends on element i) — so the sequence is a real
// chain through the dependency graph, not unrelated items in increasing waves.
func checkMilestoneCriticalPath(ms *MilestoneScope, includedWave map[string]int) error {
	// dependsOn[key] = set of keys `key` declares a dependency on, so the
	// connectivity check reads a declared edge rather than assuming one.
	dependsOn := make(map[string]map[string]bool, len(ms.Included))
	for _, inc := range ms.Included {
		set := make(map[string]bool, len(inc.DependsOn))
		for _, dep := range inc.DependsOn {
			set[dep] = true
		}
		dependsOn[itemKey(inc.ItemRef)] = set
	}
	seen := make(map[string]bool, len(ms.CriticalPath))
	for i, key := range ms.CriticalPath {
		if _, ok := includedWave[key]; !ok {
			return &SemanticError{Message: fmt.Sprintf(
				"/milestone_scope/critical_path/%d: %q is not an `included` item key; a critical path may only traverse in-scope items",
				i, key)}
		}
		if seen[key] {
			return &SemanticError{Message: fmt.Sprintf(
				"/milestone_scope/critical_path/%d: %q repeats; a critical path is a chain of DISTINCT items",
				i, key)}
		}
		seen[key] = true
		if i == 0 {
			continue
		}
		prev := ms.CriticalPath[i-1]
		if includedWave[key] <= includedWave[prev] {
			return &SemanticError{Message: fmt.Sprintf(
				"/milestone_scope/critical_path/%d: %q (wave %d) does not sit in a strictly later wave than its predecessor %q (wave %d)",
				i, key, includedWave[key], prev, includedWave[prev])}
		}
		// C3: consecutive elements must be connected by a DECLARED edge —
		// element i depends on element i-1 — so the path is a real chain.
		if !dependsOn[key][prev] {
			return &SemanticError{Message: fmt.Sprintf(
				"/milestone_scope/critical_path/%d: %q is not connected to its predecessor %q by a declared depends_on edge; a critical path must be a PATH through the dependency graph, not unrelated items in increasing waves",
				i, key, prev)}
		}
	}
	return nil
}

// checkMilestoneOpenQuestions is R6: every id in an inclusion's
// open_question_ids matches some declined_calls[].question_id, so a flagged
// ambiguity is always readable.
func checkMilestoneOpenQuestions(ms *MilestoneScope) error {
	declared := make(map[string]bool, len(ms.DeclinedCalls))
	for _, dc := range ms.DeclinedCalls {
		declared[dc.QuestionID] = true
	}
	for i, inc := range ms.Included {
		for j, q := range inc.OpenQuestionIDs {
			if !declared[q] {
				return &SemanticError{Message: fmt.Sprintf(
					"/milestone_scope/included/%d/open_question_ids/%d: %q matches no declined_calls[].question_id; a flagged ambiguity must reference a declared declined call",
					i, j, q)}
			}
		}
	}
	return nil
}

// checkMilestoneCanonicalOrder is R7 (with approval condition C4): every array
// EXCEPT critical_path is in canonical order, so identical logical input yields
// byte-identical output. critical_path is an ordered sequence — its order is its
// content — so it is deliberately excluded.
func checkMilestoneCanonicalOrder(ms *MilestoneScope) error {
	if !sortedBy(ms.Included, milestoneInclusionLess) {
		return &SemanticError{Message: "/milestone_scope/included: entries must be sorted by (wave, item key) — canonical ordering is what makes identical input yield byte-identical output"}
	}
	if !sortedBy(ms.Excluded, milestoneExclusionLess) {
		return &SemanticError{Message: "/milestone_scope/excluded: entries must be sorted by item key (canonical ordering)"}
	}
	if !sortedBy(ms.DeclinedCalls, func(a, b MilestoneDeclinedCall) bool { return a.QuestionID < b.QuestionID }) {
		return &SemanticError{Message: "/milestone_scope/declined_calls: entries must be sorted by question_id (canonical ordering)"}
	}
	for i, inc := range ms.Included {
		if !sortedStrings(inc.DependsOn) {
			return &SemanticError{Message: fmt.Sprintf("/milestone_scope/included/%d/depends_on: item keys must be sorted (canonical ordering)", i)}
		}
		if !sortedStrings(inc.OpenQuestionIDs) {
			return &SemanticError{Message: fmt.Sprintf("/milestone_scope/included/%d/open_question_ids: ids must be sorted (canonical ordering)", i)}
		}
		if !sortedBy(inc.RubricCitations, rubricCitationLess) {
			return &SemanticError{Message: fmt.Sprintf("/milestone_scope/included/%d/rubric_citations: citations must be sorted by rubric_id (canonical ordering)", i)}
		}
	}
	for i, exc := range ms.Excluded {
		if !sortedBy(exc.RubricCitations, rubricCitationLess) {
			return &SemanticError{Message: fmt.Sprintf("/milestone_scope/excluded/%d/rubric_citations: citations must be sorted by rubric_id (canonical ordering)", i)}
		}
	}
	for i, dc := range ms.DeclinedCalls {
		if !sortedStrings(dc.Options) {
			return &SemanticError{Message: fmt.Sprintf("/milestone_scope/declined_calls/%d/options: options must be sorted (canonical ordering)", i)}
		}
		if !sortedBy(dc.ItemRefs, func(a, b ItemRef) bool { return itemKey(a) < itemKey(b) }) {
			return &SemanticError{Message: fmt.Sprintf("/milestone_scope/declined_calls/%d/item_refs: refs must be sorted by item key (canonical ordering)", i)}
		}
		if !sortedBy(dc.RubricCitations, rubricCitationLess) {
			return &SemanticError{Message: fmt.Sprintf("/milestone_scope/declined_calls/%d/rubric_citations: citations must be sorted by rubric_id (canonical ordering)", i)}
		}
	}
	return nil
}

// milestoneInclusionLess is the canonical (wave, item key) order for included.
func milestoneInclusionLess(a, b MilestoneInclusion) bool {
	if a.Wave != b.Wave {
		return a.Wave < b.Wave
	}
	return itemKey(a.ItemRef) < itemKey(b.ItemRef)
}

// milestoneExclusionLess is the canonical item-key order for excluded.
func milestoneExclusionLess(a, b MilestoneExclusion) bool {
	return itemKey(a.ItemRef) < itemKey(b.ItemRef)
}

// rubricCitationLess is a total order over rubric citations for canonical
// ordering: rubric_id, then quote, then note, so two citations differing only in
// an optional field still order deterministically.
func rubricCitationLess(a, b RubricCitation) bool {
	if a.RubricID != b.RubricID {
		return a.RubricID < b.RubricID
	}
	if a.Quote != b.Quote {
		return a.Quote < b.Quote
	}
	return a.Note < b.Note
}

// sortedBy reports whether s is already in the order less defines.
func sortedBy[T any](s []T, less func(a, b T) bool) bool {
	for i := 1; i < len(s); i++ {
		if less(s[i], s[i-1]) {
			return false
		}
	}
	return true
}

// sortedStrings reports whether s is in non-decreasing order.
func sortedStrings(s []string) bool {
	for i := 1; i < len(s); i++ {
		if s[i] < s[i-1] {
			return false
		}
	}
	return true
}

// CanonicalizeMilestoneScope sorts every array EXCEPT critical_path into
// canonical order in place, so a scope built from arrays in any input order
// serializes to identical bytes (approval condition C4). critical_path is an
// ordered sequence and is left untouched.
func CanonicalizeMilestoneScope(ms *MilestoneScope) {
	if ms == nil {
		return
	}
	sort.SliceStable(ms.Included, func(i, j int) bool { return milestoneInclusionLess(ms.Included[i], ms.Included[j]) })
	sort.SliceStable(ms.Excluded, func(i, j int) bool { return milestoneExclusionLess(ms.Excluded[i], ms.Excluded[j]) })
	sort.SliceStable(ms.DeclinedCalls, func(i, j int) bool { return ms.DeclinedCalls[i].QuestionID < ms.DeclinedCalls[j].QuestionID })
	for i := range ms.Included {
		sort.Strings(ms.Included[i].DependsOn)
		sort.Strings(ms.Included[i].OpenQuestionIDs)
		sortRubricCitations(ms.Included[i].RubricCitations)
	}
	for i := range ms.Excluded {
		sortRubricCitations(ms.Excluded[i].RubricCitations)
	}
	for i := range ms.DeclinedCalls {
		sort.Strings(ms.DeclinedCalls[i].Options)
		sort.SliceStable(ms.DeclinedCalls[i].ItemRefs, func(a, b int) bool {
			return itemKey(ms.DeclinedCalls[i].ItemRefs[a]) < itemKey(ms.DeclinedCalls[i].ItemRefs[b])
		})
		sortRubricCitations(ms.DeclinedCalls[i].RubricCitations)
	}
}

// sortRubricCitations sorts one citation slice into canonical order in place.
func sortRubricCitations(cs []RubricCitation) {
	sort.SliceStable(cs, func(i, j int) bool { return rubricCitationLess(cs[i], cs[j]) })
}

// cloneMilestoneScope returns a DEEP copy of ms: every nested slice
// (depends_on, open_question_ids, rubric_citations, declined-call options and
// item_refs, and critical_path) is freshly allocated. A shallow top-level copy
// is not enough — the copied entry structs still share their nested slice
// backing arrays with the original, so an in-place CanonicalizeMilestoneScope of
// the copy would sort the CALLER's slices, changing its subsequent serialization
// and racing any goroutine that reads or validates the same scope concurrently.
func cloneMilestoneScope(ms *MilestoneScope) MilestoneScope {
	c := *ms
	c.Included = make([]MilestoneInclusion, len(ms.Included))
	for i, inc := range ms.Included {
		inc.DependsOn = append([]string(nil), inc.DependsOn...)
		inc.OpenQuestionIDs = append([]string(nil), inc.OpenQuestionIDs...)
		inc.RubricCitations = append([]RubricCitation(nil), inc.RubricCitations...)
		c.Included[i] = inc
	}
	c.Excluded = make([]MilestoneExclusion, len(ms.Excluded))
	for i, exc := range ms.Excluded {
		exc.RubricCitations = append([]RubricCitation(nil), exc.RubricCitations...)
		c.Excluded[i] = exc
	}
	c.DeclinedCalls = make([]MilestoneDeclinedCall, len(ms.DeclinedCalls))
	for i, dc := range ms.DeclinedCalls {
		dc.Options = append([]string(nil), dc.Options...)
		dc.ItemRefs = append([]ItemRef(nil), dc.ItemRefs...)
		dc.RubricCitations = append([]RubricCitation(nil), dc.RubricCitations...)
		c.DeclinedCalls[i] = dc
	}
	c.CriticalPath = append([]string(nil), ms.CriticalPath...)
	return c
}

// MilestoneScopeFingerprint is a class-tagged sha256 over the canonical
// structural fields of a milestone scope: the definition text, the framing
// statement, each inclusion's key+wave+reason, each exclusion's key+reason, each
// declined question_id, the critical path, and the confidence level. It reuses
// workmgmt's basis-hash envelope shape, so a caller can compare two runs without
// diffing the whole document. It operates on a DEEP-COPIED, canonicalized scope
// (cloneMilestoneScope), so array order does not move the hash AND the supplied
// scope is never mutated — safe to call concurrently with a reader of the same
// scope.
func MilestoneScopeFingerprint(ms *MilestoneScope) string {
	if ms == nil {
		return ""
	}
	c := cloneMilestoneScope(ms)
	CanonicalizeMilestoneScope(&c)

	fields := []string{
		"definition:" + c.ReleaseDefinition.Text,
		"framing:" + c.Framing.Statement,
		"confidence:" + c.EdgeConfidence.Level,
	}
	for _, inc := range c.Included {
		fields = append(fields, "inc:"+itemKey(inc.ItemRef)+":"+strconv.Itoa(inc.Wave)+":"+inc.Reason)
	}
	for _, exc := range c.Excluded {
		fields = append(fields, "exc:"+itemKey(exc.ItemRef)+":"+exc.Reason)
	}
	for _, dc := range c.DeclinedCalls {
		fields = append(fields, "declined:"+dc.QuestionID)
	}
	fields = append(fields, "path:"+strings.Join(c.CriticalPath, ">"))

	sum := sha256.Sum256([]byte(milestoneFingerprintClass + "\x00" + strings.Join(fields, "\x00")))
	return hex.EncodeToString(sum[:])
}
