// Package decisionindex owns decision_index (migration 0088, E75.2 / #3730,
// ADR-082 #3728): a DERIVED, rebuildable projection of the decision-bearing
// entries on the audit chain.
//
// The chain is the SOLE authority. The index is never a source of truth: every
// row cites its source entry by sequence and entry_hash, the recorded reason is
// referenced by sequence (ReasonSequence/ReasonKey) rather than copied, and the
// whole table can be truncated and reconstructed from the chain at any time.
// Writes are best-effort — an index failure can never fail the decision whose
// entry it projects (ADR-082 rule 1).
//
// This file holds the row model and the closed decision-class set; normalize.go
// the concern-category normalization table; extract.go the PURE Extract
// function shared byte-for-byte by the backfill and the live writer, which is
// what makes a rebuild identical to what the writer produced.
package decisionindex

import (
	"sort"
	"time"

	"github.com/google/uuid"
)

// DecisionClass is the closed set of decision kinds the index records. Each
// member corresponds to exactly one decision-bearing audit category.
type DecisionClass string

// The nine decision classes.
const (
	ClassPlanApproval                DecisionClass = "plan_approval"
	ClassConcernWaive                DecisionClass = "concern_waive"
	ClassConcernDefer                DecisionClass = "concern_defer"
	ClassConcernAddressedByCondition DecisionClass = "concern_addressed_by_condition"
	ClassScopeAmendment              DecisionClass = "scope_amendment"
	ClassAcceptanceArbitration       DecisionClass = "acceptance_arbitration"
	ClassMergeVerdict                DecisionClass = "merge_verdict"
	ClassClarification               DecisionClass = "clarification"
	ClassGroomingDisposition         DecisionClass = "grooming_disposition"
)

// decisionBearing maps each decision-bearing audit category to its class. It is
// the single source of truth read by the extractor, the backfill and the
// writer; TestDecisionBearingCategories_AllRegistered pins every key against
// audit.KnownCategories so a renamed category is caught, not silently dropped.
var decisionBearing = map[string]DecisionClass{
	"approval_submitted":             ClassPlanApproval,
	"concern_waived":                 ClassConcernWaive,
	"concern_deferred":               ClassConcernDefer,
	"concern_addressed_by_condition": ClassConcernAddressedByCondition,
	"scope_amendment_decided":        ClassScopeAmendment,
	"acceptance_triage_arbitrated":   ClassAcceptanceArbitration,
	"merge_verdict_recorded":         ClassMergeVerdict,
	"clarification_answered":         ClassClarification,
	"grooming_disposition_recorded":  ClassGroomingDisposition,
}

// ClassFor returns the decision class a category maps to, and whether the
// category is decision-bearing at all.
func ClassFor(category string) (DecisionClass, bool) {
	c, ok := decisionBearing[category]
	return c, ok
}

// IsDecisionBearing reports whether entries of category are indexed.
func IsDecisionBearing(category string) bool {
	_, ok := decisionBearing[category]
	return ok
}

// DecisionBearingCategories returns the decision-bearing categories, sorted —
// a fresh slice, deterministic across calls, suitable for a SQL ANY($1).
func DecisionBearingCategories() []string {
	out := make([]string, 0, len(decisionBearing))
	for c := range decisionBearing {
		out = append(out, c)
	}
	sort.Strings(out)
	return out
}

// Row mirrors one decision_index row, column for column. A rebuild is
// byte-identical because every field is a pure function of the source entry
// and its joined RowContext — there is deliberately no operational metadata
// (no indexed_at) the chain does not carry.
type Row struct {
	SourceSequence  int64
	SourceEntryHash string
	RunID           uuid.UUID
	StageID         *uuid.UUID
	AccountID       *uuid.UUID
	Repo            string
	WorkflowID      string
	// DoctrineVersion is runs.workflow_sha today; ADR-082 rule 4's charter
	// revision replaces the value (not the column) once E71.2 #3242 lands.
	DoctrineVersion string
	DecisionClass   DecisionClass
	StageKind       string
	Outcome         string
	// RejectClass is approval_submitted's E75.1 reject_class; empty for an
	// approve, a classless reject, or an entry predating E75.1.
	RejectClass             string
	ConcernCategoryRaw      string
	ConcernCategory         string
	ConcernCategoryUnmapped bool
	Severity                string
	// TouchedPaths and EscalationKeys are always non-nil, sorted and
	// de-duplicated, so the stored arrays never depend on input order.
	TouchedPaths   []string
	EscalationKeys []string
	Delegated      bool
	ActorKind      string
	ActorSubject   string
	DecidedAt      time.Time
	// ReasonSequence points at the chain entry recording the reason and
	// ReasonKey names the payload key holding it (empty when the entry
	// records none). The prose itself is never copied (ADR-082 rule 1).
	ReasonSequence int64
	ReasonKey      string
}
