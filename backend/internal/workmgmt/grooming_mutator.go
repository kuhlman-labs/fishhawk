package workmgmt

// This file carries the MUTATE half of the work-management provider
// abstraction (E54.5 / #2237): the optional GroomingMutator capability, the
// CLOSED provider-neutral mutation vocabulary it dispatches, and the
// MutatorFor resolution chokepoint. It is the sixth capability interface,
// after Provider's four in provider.go and WorkItemReader in reader.go, and
// lives in its own file for the same reason reader.go does — one file per
// capability keeps each capability's rationale readable.
//
// NOTHING IN PRODUCTION CALLS THIS YET. #2237 reserves the apply seam; no HTTP
// route, MCP tool, CLI verb, env var, config field or migration resolves a
// mutator. The ADR-064 invariant that no board path reaches the MCP agent tool
// surface is extended to these WRITE symbols by
// backend/internal/mcpserver/board_read_guard_test.go.

import (
	"context"
	"sort"
	"strings"
)

// GroomingMutator is the optional grooming-APPLY capability (E54.5 / #2237):
// execute ONE approved grooming mutation against the tracker. It is the write
// counterpart to WorkItemReader — the seam through which an approved
// plan.GroomingReport's proposals reach the forge.
//
// Like Transitioner (#1012), NumberDiscoverer (#1269), EpicChildrenQuerier
// (ADR-047), IssueSetDependencyResolver (#2051) and WorkItemReader (#2230) it
// is a SEPARATE capability interface rather than folded into Provider, for the
// reasons those give: gitlab and jira are File-only in v0, and widening
// Provider would force every registered fake in every sibling package's tests
// to grow the method. Callers resolve it through MutatorFor, which type-asserts
// the capability and returns a typed *UnavailableError when the provider does
// not implement it — never a nil interface a caller could dispatch against.
//
// ApplyGroomingMutation executes EXACTLY ONE mutation and reports what it did.
// It MUST NOT interpret authorization: every containment rule (join, approval,
// mode, destructive authorization, idempotence, manual-placement courtesy)
// is decided by ApplyGrooming BEFORE dispatch, so a provider that is handed a
// request has already been cleared to act. A provider MAY re-check the
// expected-source set as defence in depth (Transition already does), and MUST
// return a typed error rather than a silent no-op for a kind it cannot execute.
type GroomingMutator interface {
	ApplyGroomingMutation(ctx context.Context, req GroomingMutationRequest) (*GroomingMutationResult, error)
}

// GroomingCapability is the capability name UnavailableError carries for the
// grooming-mutation capability, so the error text names what was unavailable.
const GroomingCapability = "grooming mutation"

// MutatorFor resolves the registered provider for id and type-asserts the
// optional GroomingMutator capability. It is the SINGLE chokepoint every
// consumer of the apply capability must resolve through, mirroring ReaderFor:
// a File-only provider yields a typed
// *UnavailableError{Reason: ReasonNotImplemented} here, not a nil interface a
// caller could dispatch against.
//
// An unregistered id returns Get's existing *UnknownProviderError unchanged.
func MutatorFor(id string) (GroomingMutator, error) {
	p, err := Get(id)
	if err != nil {
		return nil, err
	}
	m, ok := p.(GroomingMutator)
	if !ok {
		return nil, &UnavailableError{
			Provider:   p.Name(),
			Capability: GroomingCapability,
			Reason:     ReasonNotImplemented,
			Detail:     "this provider files work items but cannot apply grooming mutations; use a provider that implements the grooming-mutation capability",
		}
	}
	return m, nil
}

// GroomingMutationKind is the CLOSED set of tracker mutations a grooming
// apply may dispatch. Closed is the point: the derivation maps a report entry
// onto a kind from this set or records the entry as unmappable, so no apply
// can ever dispatch an action nobody enumerated. The five families are the
// ones #2237 names — hygiene, ordering, dependency, dedup and scoping.
//
// ORDERING KINDS ARE FIELD WRITES, NOT POSITIONAL REORDERING (#2237 approval
// condition I3). GitHub's ProjectV2 GraphQL surface does expose a positional
// mutation, but this repository's provider implements NO positional primitive
// and #2237 adds none. GroomingKindRankSet and GroomingKindPrioritySet
// therefore write a board FIELD VALUE — the report's rank, and a priority
// field value — and do NOT reorder any queue. A consumer that wants the
// proposed order (E54.6 / #2238's campaign feed) reads the rank FIELD back and
// sorts on it; it must not expect the board's own item order to have moved.
type GroomingMutationKind string

// The closed mutation vocabulary. Each constant names one provider-executable
// action; a provider's kind switch must handle every one or return a typed
// error naming the kind it could not execute.
const (
	// GroomingKindLabelSet adds the proposed label to the item's label set.
	// The provider reads the current set and sends the UNION — the forge's
	// label write replaces wholesale.
	GroomingKindLabelSet GroomingMutationKind = "label_set"
	// GroomingKindFieldSet writes a board single-select/number field value
	// (the missing-estimate defect's fix).
	GroomingKindFieldSet GroomingMutationKind = "field_set"
	// GroomingKindBoardPlace puts an off-board item onto the board.
	GroomingKindBoardPlace GroomingMutationKind = "board_place"
	// GroomingKindEpicLink links the item to its parent epic.
	GroomingKindEpicLink GroomingMutationKind = "epic_link"
	// GroomingKindPrioritySet writes a priority FIELD value. See the type
	// doc: a field write, not a queue reorder.
	GroomingKindPrioritySet GroomingMutationKind = "priority_set"
	// GroomingKindRankSet writes the proposed rank as a FIELD value. See the
	// type doc: a field write, not a queue reorder.
	GroomingKindRankSet GroomingMutationKind = "rank_set"
	// GroomingKindDependsOnAdd records a depends_on edge on the item.
	GroomingKindDependsOnAdd GroomingMutationKind = "depends_on_add"
	// GroomingKindCloseDuplicate closes the item as a duplicate. DESTRUCTIVE.
	GroomingKindCloseDuplicate GroomingMutationKind = "close_duplicate"
	// GroomingKindCloseNotPlanned closes the item as not planned. DESTRUCTIVE.
	GroomingKindCloseNotPlanned GroomingMutationKind = "close_not_planned"
	// GroomingKindIcebox moves the item to the board's icebox column.
	// DESTRUCTIVE: it removes the item from the active backlog.
	GroomingKindIcebox GroomingMutationKind = "icebox"
)

// destructiveKinds is the ONE declaration of which kinds are destructive, so
// the authorization gate in grooming_apply.go and the audit payload cannot
// drift apart. Icebox is destructive alongside the two closes: it takes an
// item out of the active backlog, which is a scoping decision a human owns.
var destructiveKinds = map[GroomingMutationKind]bool{
	GroomingKindCloseDuplicate:  true,
	GroomingKindCloseNotPlanned: true,
	GroomingKindIcebox:          true,
}

// Destructive reports whether the kind removes an item from the active
// backlog — the closes and the icebox move. A destructive kind dispatches
// ONLY under the authorization rule in grooming_apply.go.
func (k GroomingMutationKind) Destructive() bool { return destructiveKinds[k] }

// boardPlacementKinds are the kinds that MOVE A CARD on the board, and so are
// routed through the never-fight-the-human expected-source guard AND the
// board-state pre-read. Icebox is in the set (approval condition I2): it is a
// board move like any other, and leaving it out would let an icebox overwrite
// a placement a human chose.
var boardPlacementKinds = map[GroomingMutationKind]bool{
	GroomingKindBoardPlace: true,
	GroomingKindIcebox:     true,
}

// BoardPlacement reports whether the kind moves a card on the board, and so
// must pass the expected-source-set courtesy before it dispatches.
func (k GroomingMutationKind) BoardPlacement() bool { return boardPlacementKinds[k] }

// valueSetKinds are the kinds whose CURRENT value the read capability cannot
// observe: WorkItemRecord exposes labels, body, state, the board Status column
// and the STRUCTURAL parent (ParentRef, #2952), but no arbitrary project FIELD
// value. See IdempotenceObservable for what follows from that.
var valueSetKinds = map[GroomingMutationKind]bool{
	GroomingKindFieldSet:    true,
	GroomingKindRankSet:     true,
	GroomingKindPrioritySet: true,
}

// IdempotenceObservable reports whether the pre-dispatch read can OBSERVE this
// kind's effect, which decides whether the idempotence diff can settle it.
//
// It is true for every kind whose repetition would be observably harmful — a
// second label add, a second epic link, a second depends_on marker, a second
// close, a second board move — and those are diffed against current state and
// never dispatched twice. epic_link is diffed against the STRUCTURAL parent
// (WorkItemRecord.ParentRef, #2952), NOT the `Parent epic:` body marker: the
// marker is a rendering of the link and can disagree with it, so settling on it
// audited a genuine refusal as already_applied. It is FALSE for the three value-set kinds, and that
// residual is stated plainly rather than implied away: a project field value
// is not carried on WorkItemRecord, so the apply layer cannot tell an
// already-written rank from an unwritten one and re-dispatches it. Repeating a
// scalar field write is last-write-wins with no duplicate side effect, which is
// why the residual is acceptable — but it means the zero-dispatch guarantee on
// re-apply covers the seven observable kinds, not all ten. Every record carries
// IdempotenceChecked so which arm settled a candidate is visible in the audit
// row rather than only in this comment.
func (k GroomingMutationKind) IdempotenceObservable() bool { return !valueSetKinds[k] }

// GroomingMode is the provider-neutral auto|gated|report mirror of
// spec.ActionMode. It is deliberately NOT an import of backend/internal/spec:
// the provider layer stays free of the workflow-spec dependency, and the
// future runtime consumer does the one-line mapping. ResolveGroomingMode
// owns the fail-closed default, so an absent or unrecognized mode can never
// widen authority.
type GroomingMode string

// The three grooming modes, mirroring spec.ModeAuto/ModeGated/ModeReport.
const (
	// GroomingModeReport surfaces the proposal and takes NO action. It beats
	// every other authorization input, including an explicit gate approval.
	GroomingModeReport GroomingMode = "report"
	// GroomingModeGated means a human acts. It is the fail-closed default an
	// absent or unrecognized class entry resolves to.
	GroomingModeGated GroomingMode = "gated"
	// GroomingModeAuto means the apply may act on an approved entry without a
	// per-entry gate approval.
	GroomingModeAuto GroomingMode = "auto"
)

// ResolveGroomingMode normalizes a declared mode to the closed set, failing
// CLOSED: an empty string (an absent class in the resolved matrix) and any
// unrecognized value both resolve to GroomingModeGated, matching
// spec.resolveBlock's always-emit-a-gated-default rule. This is the single
// place the default lives, so no call site can accidentally default to auto.
func ResolveGroomingMode(m GroomingMode) GroomingMode {
	switch m {
	case GroomingModeAuto, GroomingModeGated, GroomingModeReport:
		return m
	default:
		return GroomingModeGated
	}
}

// GroomingValue is the before/after shape one mutation carries. Scalar holds a
// single value (a board column, an epic ref, a rank); List holds a set (the
// label set). Both are present on one type so a label mutation and a status
// mutation share one record shape and the audit payload does not fork.
//
// epic_link is the one kind that populates BOTH members of its `before` at once
// (#2952): Scalar is the STRUCTURAL parent (the authority the idempotence diff
// settles on) and List is the `Parent epic:` body-marker refs (a rendering the
// diff ignores). They are reported separately so a divergence — a body marker
// with no structural parent, `{scalar: "", list: ["#389"]}` — is VISIBLE in the
// audit row rather than collapsed into one verdict.
type GroomingValue struct {
	Scalar string   `json:"scalar,omitempty"`
	List   []string `json:"list,omitempty"`
}

// Equal reports value equality, order-insensitively for List (a label set is a
// SET — "type:bug,area:api" and "area:api,type:bug" are the same set, and an
// order-sensitive comparison would re-dispatch an already-applied label
// mutation on every re-apply).
func (v GroomingValue) Equal(other GroomingValue) bool {
	if v.Scalar != other.Scalar {
		return false
	}
	if len(v.List) != len(other.List) {
		return false
	}
	a := append([]string(nil), v.List...)
	b := append([]string(nil), other.List...)
	sort.Strings(a)
	sort.Strings(b)
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// Contains reports whether every element of want is present in v.List. It is
// the idempotence test for an ADDITIVE set mutation (label_set): the proposal
// is "the item should carry this label", which is already satisfied when the
// observed set carries it, whatever else it carries.
func (v GroomingValue) Contains(want []string) bool {
	have := make(map[string]struct{}, len(v.List))
	for _, s := range v.List {
		have[strings.TrimSpace(s)] = struct{}{}
	}
	for _, w := range want {
		if _, ok := have[strings.TrimSpace(w)]; !ok {
			return false
		}
	}
	return true
}

// GroomingMutationRequest is ONE cleared mutation handed to a provider. Every
// containment decision has already been made by ApplyGrooming; the provider's
// job is to execute Kind against ItemRef and report what happened.
//
// EntryID is the grooming-report entry this mutation came from — the join key
// across report, operator decision, applied mutation and audit row. ItemRef is
// the provider-native issue reference ("#2237"), resolved from the report's
// item_ref against Target. Before/After are the observed and proposed values.
// ExpectedFrom is the canonical-state set a board move may proceed from (a
// provider may re-check it as defence in depth); States is the
// Conventions.States canonical -> provider-option map, carried exactly as
// TransitionRequest and ReadWorkItemRequest already carry it.
type GroomingMutationRequest struct {
	Target       Target
	EntryID      string
	Class        string
	Kind         GroomingMutationKind
	ItemRef      string
	Before       GroomingValue
	After        GroomingValue
	ExpectedFrom []string
	States       map[string]string
}

// GroomingMutationResult is what a provider reports for one dispatched
// mutation. ProviderResponse is the short provider-side description the audit
// row carries; Observed is the state the provider read at write time, when it
// read one.
//
// EXACTLY ONE OF THREE — Applied, Skipped, Refused — MUST be true, and a
// provider that returns any other combination is treated as having FAILED
// (#2237 review, widened to three by #2860). The three states are distinct
// AUDIT FACTS and the distinction is the point:
//
//   - Applied: the tracker CHANGED.
//   - Skipped: a deliberate no-op the provider OBSERVED as already-satisfied —
//     the value it was asked to write is the value already there.
//   - Refused: a requested write the provider DECLINED to perform. Nothing
//     changed AND nothing was already correct. This is NOT benign idempotence
//     and must not read as it (#2860): folding a refusal into Skipped is how a
//     0/8 apply rate went unnoticed across three grooming walks, because every
//     refused edge was audited as an ordinary no-op.
//
// The apply layer VALIDATES this rather than assuming it, because
// settleGroomingCandidate turns this struct straight into the load-bearing
// audit outcome. Every other combination — including ALL-FALSE — is FAILED: a
// zero-value result claims no write, no observed no-op and no refusal, and a
// switch whose applied arm is the DEFAULT would fabricate an audit row
// claiming a tracker write that provably did not happen.
type GroomingMutationResult struct {
	Applied          bool          `json:"applied"`
	Skipped          bool          `json:"skipped"`
	Refused          bool          `json:"refused"`
	SkipReason       string        `json:"skip_reason,omitempty"`
	RefuseReason     string        `json:"refuse_reason,omitempty"`
	ProviderResponse string        `json:"provider_response,omitempty"`
	Observed         GroomingValue `json:"observed,omitempty"`
}

// WellFormed reports whether EXACTLY ONE of Applied, Skipped and Refused is
// true — the contract stated on the type. It is a method rather than an inline
// count in settleGroomingCandidate so the validator and the test that pins it
// cannot drift apart.
func (r GroomingMutationResult) WellFormed() bool {
	n := 0
	for _, b := range []bool{r.Applied, r.Skipped, r.Refused} {
		if b {
			n++
		}
	}
	return n == 1
}
