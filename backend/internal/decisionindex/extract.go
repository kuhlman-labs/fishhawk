package decisionindex

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
)

// Errors Extract returns for an entry it must not index. Callers (the backfill
// and the writer) COUNT and report these rather than swallowing them.
var (
	// ErrNotDecisionBearing: the entry's category is not in the
	// decision-bearing set.
	ErrNotDecisionBearing = errors.New("decisionindex: category is not decision-bearing")
	// ErrNoRun: the entry carries no run id. Every decision-bearing category
	// is run-scoped today, so this names a new global-chain category loudly
	// instead of writing a row the run_id NOT NULL column would refuse.
	ErrNoRun = errors.New("decisionindex: decision-bearing entry has no run id")
	// ErrNilEntry: Extract was handed no entry at all.
	ErrNilEntry = errors.New("decisionindex: nil audit entry")
)

// escalationFiredCategory is the audit category whose fired_keys the
// RowContext resolver reads (E75.1 / #3729). Not decision-bearing itself.
const escalationFiredCategory = "escalation_fired"

// RowContext carries the joined facts the audit entry itself lacks. It is
// resolved ONCE per entry by a shared resolver (the backfill's batched join
// and the live writer's per-append lookup) so both paths hand Extract the same
// input for the same entry.
type RowContext struct {
	Repo            string // runs.repo
	WorkflowID      string // runs.workflow_id
	DoctrineVersion string // runs.workflow_sha (see Row.DoctrineVersion)
	StageKind       string // stages.stage_type of the entry's stage
	// TouchedPaths are the run's plan-artifact scope.files paths
	// (TouchedPathsFromPlan). Extract sorts and de-duplicates them.
	TouchedPaths []string
	// EscalationKeys are the fired_keys of the latest escalation_fired entry
	// on the SAME stage below the decision (LatestEscalationKeys). Extract
	// sorts and de-duplicates them.
	EscalationKeys []string
	// ConcernCategory / ConcernSeverity are the review_concerns row's values,
	// used ONLY for a concern-class entry whose own payload carries no
	// category / severity key (concern_addressed_by_condition entries that
	// predate E75.1). A resolver that does not join review_concerns leaves
	// them empty, and such an entry then indexes the fields as empty.
	ConcernCategory string
	ConcernSeverity string
}

// Extract turns one audit entry plus its joined RowContext into a Row. It is
// PURE — no database handle, no clock — so the backfill and the live writer
// produce byte-identical rows for the same entry, which is what makes a rebuild
// from scratch equal to the index the writer built incrementally.
//
// Payload decoding is tolerant: every payload key is optional, and a key that
// is absent (an entry predating the field) or of an unexpected JSON type
// indexes as empty. Only a payload that is not JSON at all is an error.
func Extract(e *audit.Entry, rc RowContext) (*Row, error) {
	if e == nil {
		return nil, ErrNilEntry
	}
	class, ok := ClassFor(e.Category)
	if !ok {
		return nil, fmt.Errorf("%w: %q (sequence %d)", ErrNotDecisionBearing, e.Category, e.Sequence)
	}
	if e.RunID == nil {
		return nil, fmt.Errorf("%w: %s entry at sequence %d", ErrNoRun, e.Category, e.Sequence)
	}
	p, err := decodePayload(e.Payload)
	if err != nil {
		return nil, fmt.Errorf("decisionindex: decode %s payload at sequence %d: %w", e.Category, e.Sequence, err)
	}

	row := &Row{
		SourceSequence:  e.Sequence,
		SourceEntryHash: e.EntryHash,
		RunID:           *e.RunID,
		StageID:         copyUUID(e.StageID),
		AccountID:       copyUUID(e.AccountID),
		Repo:            rc.Repo,
		WorkflowID:      rc.WorkflowID,
		DoctrineVersion: rc.DoctrineVersion,
		DecisionClass:   class,
		StageKind:       rc.StageKind,
		Outcome:         outcomeFor(class, p),
		TouchedPaths:    sortedUnique(rc.TouchedPaths),
		EscalationKeys:  sortedUnique(rc.EscalationKeys),
		Delegated:       p.delegated(),
		DecidedAt:       e.Timestamp.UTC().Truncate(time.Microsecond),
		ReasonSequence:  e.Sequence,
		ReasonKey:       p.reasonKey(),
	}
	if e.ActorKind != nil {
		row.ActorKind = string(*e.ActorKind)
	}
	if e.ActorSubject != nil {
		row.ActorSubject = *e.ActorSubject
	}
	if class == ClassPlanApproval {
		row.RejectClass, _ = p.str("reject_class")
	}
	if isConcernClass(class) {
		raw, present := p.str("category")
		if !present {
			raw = rc.ConcernCategory
		}
		sev, present := p.str("severity")
		if !present {
			sev = rc.ConcernSeverity
		}
		row.ConcernCategoryRaw = raw
		row.Severity = sev
		canonical, mapped := NormalizeConcernCategory(raw)
		row.ConcernCategory = canonical
		row.ConcernCategoryUnmapped = canonical != "" && !mapped
	}
	return row, nil
}

func isConcernClass(c DecisionClass) bool {
	return c == ClassConcernWaive || c == ClassConcernDefer || c == ClassConcernAddressedByCondition
}

// classOutcome is the outcome of a class whose decision is implied by the
// category itself. The concern classes are listed even though their payloads
// may carry a `verdict` (concern_addressed_by_condition records the CONFIRMING
// REVIEW's verdict, which is not this decision's outcome).
var classOutcome = map[DecisionClass]string{
	ClassConcernWaive:                "waived",
	ClassConcernDefer:                "deferred",
	ClassConcernAddressedByCondition: "addressed",
	ClassClarification:               "answered",
}

// outcomeKeys is the payload ladder for every other class: approval_submitted
// and scope_amendment_decided record `decision`; merge_verdict_recorded,
// acceptance_triage_arbitrated and grooming_disposition_recorded `verdict`.
var outcomeKeys = []string{"decision", "verdict", "outcome"}

func outcomeFor(class DecisionClass, p payload) string {
	if o, ok := classOutcome[class]; ok {
		return o
	}
	for _, k := range outcomeKeys {
		if v, _ := p.str(k); v != "" {
			return v
		}
	}
	return ""
}

// reasonKeys is the ordered set of payload keys that hold a decision's
// recorded reason: `reason` (waive, defer, scope amendment, arbitration),
// `rejection_comment` / `comment` (approval, clarification), `note`.
var reasonKeys = []string{"reason", "rejection_comment", "comment", "note"}

// payload is a decoded audit payload: each top-level key kept raw so a key of
// an unexpected type is simply not read rather than failing the whole entry.
type payload map[string]json.RawMessage

func decodePayload(raw json.RawMessage) (payload, error) {
	if len(raw) == 0 {
		return payload{}, nil
	}
	var p payload
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil, err
	}
	if p == nil { // JSON null
		p = payload{}
	}
	return p, nil
}

// str returns key's value when it is a JSON string. present reports whether the
// key exists as a string at all — distinct from an empty string, which an
// E75.1 emitter records deliberately (absent = predates the field).
func (p payload) str(key string) (value string, present bool) {
	raw, ok := p[key]
	if !ok {
		return "", false
	}
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return "", false
	}
	return s, true
}

// delegated reads the `delegated` key, which emitters record in two shapes:
// the delegation RULE name (a non-empty string, approval/waive) or a bool
// (merge verdict, arbitration). Anything else is not delegated.
func (p payload) delegated() bool {
	raw, ok := p["delegated"]
	if !ok {
		return false
	}
	var b bool
	if err := json.Unmarshal(raw, &b); err == nil {
		return b
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s != ""
	}
	return false
}

func (p payload) reasonKey() string {
	for _, k := range reasonKeys {
		if v, _ := p.str(k); v != "" {
			return k
		}
	}
	return ""
}

// TouchedPathsFromPlan returns the sorted, de-duplicated scope.files[].path
// values of a standard_v1 plan artifact's content.
//
// Shape tolerance mirrors decodePayload's posture, because the index covers
// HISTORICAL runs whose plan artifacts predate the current scope.files object
// shape: `scope` that is not an object, `files` that is not an array, and any
// element that is not an object carrying a string `path` (a legacy bare-string
// entry such as {"scope":{"files":["a.go"]}}) each contribute NOTHING and yield
// an empty (non-nil) slice rather than an error. Only content that is not JSON
// at all is an error — an intolerant decode here would abort a whole backfill
// page, and fail live indexing for every decision on such a run.
func TouchedPathsFromPlan(content []byte) ([]string, error) {
	if len(content) == 0 {
		return []string{}, nil
	}
	var doc struct {
		Scope json.RawMessage `json:"scope"`
	}
	if err := json.Unmarshal(content, &doc); err != nil {
		return nil, fmt.Errorf("decisionindex: decode plan artifact content: %w", err)
	}
	var scope struct {
		Files []json.RawMessage `json:"files"`
	}
	if len(doc.Scope) == 0 || json.Unmarshal(doc.Scope, &scope) != nil {
		return []string{}, nil
	}
	paths := make([]string, 0, len(scope.Files))
	for _, raw := range scope.Files {
		var f struct {
			Path string `json:"path"`
		}
		if json.Unmarshal(raw, &f) != nil {
			continue
		}
		paths = append(paths, f.Path)
	}
	return sortedUnique(paths), nil
}

// LatestEscalationKeys is the escalation half of the RowContext resolver
// (binding condition 3 of #3730's approval): among entries, it takes the
// escalation_fired entry with the SAME stage as the decision and the HIGHEST
// sequence strictly LOWER than decisionSequence, and returns its fired_keys
// sorted and de-duplicated. A decision with no stage, or no qualifying entry,
// yields an empty slice. Entries of any other category are ignored, so a
// resolver may pass an unfiltered page.
//
// An escalation_fired entry's stage is its StageID, falling back to the
// payload's stage_id when the column is unset.
func LatestEscalationKeys(entries []*audit.Entry, decisionStage *uuid.UUID, decisionSequence int64) []string {
	if decisionStage == nil {
		return []string{}
	}
	var latest *audit.Entry
	for _, e := range entries {
		if e == nil || e.Category != escalationFiredCategory || e.Sequence >= decisionSequence {
			continue
		}
		sid, ok := escalationStage(e)
		if !ok || sid != *decisionStage {
			continue
		}
		if latest == nil || e.Sequence > latest.Sequence {
			latest = e
		}
	}
	if latest == nil {
		return []string{}
	}
	return FiredKeys(latest.Payload)
}

func escalationStage(e *audit.Entry) (uuid.UUID, bool) {
	if e.StageID != nil {
		return *e.StageID, true
	}
	p, err := decodePayload(e.Payload)
	if err != nil {
		return uuid.Nil, false
	}
	s, _ := p.str("stage_id")
	id, err := uuid.Parse(s)
	if err != nil {
		return uuid.Nil, false
	}
	return id, true
}

// FiredKeys returns an escalation_fired payload's fired_keys (E75.1's stable
// escalation.RuleKey values), sorted and de-duplicated. An entry predating
// E75.1, or an undecodable payload, yields an empty slice.
func FiredKeys(raw json.RawMessage) []string {
	var p struct {
		FiredKeys []string `json:"fired_keys"`
	}
	if len(raw) == 0 || json.Unmarshal(raw, &p) != nil {
		return []string{}
	}
	return sortedUnique(p.FiredKeys)
}

// sortedUnique returns a fresh, non-nil, sorted copy of in with duplicates and
// empty strings removed, so a stored array never depends on input order.
func sortedUnique(in []string) []string {
	out := make([]string, 0, len(in))
	seen := make(map[string]struct{}, len(in))
	for _, s := range in {
		if s == "" {
			continue
		}
		if _, dup := seen[s]; dup {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}

func copyUUID(id *uuid.UUID) *uuid.UUID {
	if id == nil {
		return nil
	}
	v := *id
	return &v
}
