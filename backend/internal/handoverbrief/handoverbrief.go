// Package handoverbrief composes the handover brief (E76.4 / #3767, under
// ADR-083 #3751 and ADR-082 #3728 rule 7): a bounded, fully-cited summary of
// everything an incoming captain needs since the last handover.
//
// It COMPOSES existing readers and re-derives nothing:
//
//   - what_changed and needs_decision are digest.Build calls (E75.6) over the
//     brief's window, so every item keeps the digest's citation (source
//     sequence + source entry hash) — the brief's citations ARE the digest's.
//   - in_flight is the non-terminal campaigns and runs, read through Store
//     (campaign.Repository.ListCampaigns / run.Repository.ListRuns).
//   - delegation_in_force and standing_orders are projected from a
//     delegationview.View (E76.1) the caller supplies.
//
// The WINDOW starts after the last captain_assigned entry of the E76.2 captain
// record (DeriveWindow), or at chain sequence 1 for a first captain.
//
// Load-bearing properties:
//
//   - Failure vs degradation. Only a failure to ESTABLISH the brief — the
//     captain record read or the window (chain head) read — fails Compose, as
//     an *UnavailableError. Every section read failure is a DEGRADATION: the
//     part is marked unavailable, a named Degradation is appended, and the
//     brief still composes and still carries a BriefHash.
//   - Declared absences. Sections the issue names that have no source on main
//     (charter_revision, adr_index, doctrine_changes) are listed in Absent with
//     a reason and an anchor, never emitted empty or silently omitted.
//   - ONE canonical hash. BriefHash is computed once, by Compose, over the
//     canonical form (canonical): the full, unbounded composition with the
//     hash itself, the section selector and every truncation marker / bounding
//     field zeroed. Select and Bound copy it through and never re-hash.
//
// The brief is a POINT-IN-TIME composition: the hash commits to what was
// composed at that moment; live sections (parked gates, campaigns, runs) may
// differ on a later read.
//
// Long-form contract: backend/internal/handoverbrief/README.md.
package handoverbrief

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/captain"
	"github.com/kuhlman-labs/fishhawk/backend/internal/delegationview"
	"github.com/kuhlman-labs/fishhawk/backend/internal/digest"
)

// SectionKind names one of the five brief sections.
type SectionKind string

// The five sections, in render order.
const (
	SectionWhatChanged       SectionKind = "what_changed"
	SectionNeedsDecision     SectionKind = "needs_decision"
	SectionInFlight          SectionKind = "in_flight"
	SectionDelegationInForce SectionKind = "delegation_in_force"
	SectionStandingOrders    SectionKind = "standing_orders"
)

// Sections is the closed, ordered set of brief sections.
var Sections = []SectionKind{SectionWhatChanged, SectionNeedsDecision, SectionInFlight, SectionDelegationInForce, SectionStandingOrders}

// ParseSection validates a section selector; the empty string selects the
// whole brief.
func ParseSection(s string) (SectionKind, error) {
	if s == "" {
		return "", nil
	}
	for _, k := range Sections {
		if string(k) == s {
			return k, nil
		}
	}
	return "", fmt.Errorf("%w: unknown section %q (want one of what_changed, needs_decision, in_flight, delegation_in_force, standing_orders)", ErrInvalidRequest, s)
}

// PartKind names one underlying read inside a section. Each part maps to ONE
// underlying query, which is what its cursor names.
type PartKind string

// Parts.
const (
	PartMerges              PartKind = "merges"
	PartWaiversAndDeferrals PartKind = "waivers_and_deferrals"
	PartUnansweredPages     PartKind = "unanswered_pages"
	PartOpenDecisions       PartKind = "open_decisions"
	PartCampaigns           PartKind = "campaigns"
	PartRuns                PartKind = "runs"
	PartWorkflows           PartKind = "workflows"
	// PartGaps is the cursor part of the window's gaps stream.
	PartGaps PartKind = "gaps"
)

// ConfirmationUnavailable is the value every delegation entry's Confirmation
// carries until E76.5 (#3768) lands a per-workflow confirmation read.
const ConfirmationUnavailable = "unavailable"

// Window basis values.
const (
	// BasisSinceLastHandover: the window starts after the last
	// captain_assigned entry.
	BasisSinceLastHandover = "since_last_handover"
	// BasisFirstCaptain: no captain_assigned entry exists; the window starts
	// at chain sequence 1.
	BasisFirstCaptain = "first_captain"
	// BasisRequested: the caller pinned from_sequence explicitly.
	BasisRequested = "requested"
)

// Degradation kinds. Each names one independently-degraded contribution.
const (
	DegradationDigestSectionFailed       = "digest_section_failed"
	DegradationCampaignStoreUnconfigured = "campaign_store_unconfigured"
	DegradationRunStoreUnconfigured      = "run_store_unconfigured"
	DegradationCampaignReadFailed        = "campaign_read_failed"
	DegradationRunReadFailed             = "run_read_failed"
	DegradationDelegationUnavailable     = "delegation_unavailable"
	// DegradationScanLimit: a bounded in-flight read hit its LIMIT; the part's
	// cursor names the query for the rest. (Digest scan-limit bites are carried
	// through with the digest's own kind, which is the same string.)
	DegradationScanLimit = digest.DegradationScanLimit
)

// Window is the inclusive chain window what_changed and the unanswered pages
// cover. needs_decision's open_decisions part lists every CURRENTLY parked gate
// cited at or below ToSequence (a gate parked before the handover is still
// open).
type Window struct {
	FromSequence int64  `json:"from_sequence"`
	ToSequence   int64  `json:"to_sequence"`
	ChainHead    int64  `json:"chain_head"`
	Basis        string `json:"basis"`
	// AssignedSequence / AssignedEntryHash cite the captain_assigned entry
	// the window starts after (zero for first_captain).
	AssignedSequence  int64  `json:"assigned_sequence,omitempty"`
	AssignedEntryHash string `json:"assigned_entry_hash,omitempty"`
}

// DeriveWindow is the PURE window derivation over the captain record's
// entries: from = (last captain_assigned sequence) + 1, or 1 with basis
// first_captain when there is none; to = head.
func DeriveWindow(entries []captain.ChainEntry, head int64) Window {
	w := Window{FromSequence: 1, ToSequence: head, ChainHead: head, Basis: BasisFirstCaptain}
	for _, e := range entries {
		if e.Category == captain.CategoryAssigned && e.Sequence >= w.AssignedSequence {
			w.AssignedSequence, w.AssignedEntryHash = e.Sequence, e.EntryHash
		}
	}
	if w.AssignedSequence > 0 {
		w.FromSequence = w.AssignedSequence + 1
		w.Basis = BasisSinceLastHandover
	}
	return w
}

// InFlightItem is one non-terminal campaign or run. It cites its row id: these
// are store rows, not chain entries.
type InFlightItem struct {
	Kind       string    `json:"kind"` // "campaign" | "run"
	ID         uuid.UUID `json:"id"`
	State      string    `json:"state"`
	Ref        string    `json:"ref,omitempty"` // campaign epic_ref / run trigger_ref
	WorkflowID string    `json:"workflow_id,omitempty"`
	CreatedAt  time.Time `json:"created_at"`
	// FieldsTruncated marks an item one of whose strings Bound capped.
	FieldsTruncated bool `json:"fields_truncated,omitempty"`
}

// EscalationCeiling is one declared escalation's match and autonomy ceiling.
type EscalationCeiling struct {
	Match       delegationview.EscalationMatch `json:"match"`
	MaxAutonomy string                         `json:"max_autonomy,omitempty"`
}

// DelegationEntry is one workflow's delegation in force.
type DelegationEntry struct {
	WorkflowID    string              `json:"workflow_id"`
	Autonomy      string              `json:"autonomy,omitempty"`
	MustPageHuman []string            `json:"must_page_human,omitempty"`
	Escalations   []EscalationCeiling `json:"escalations,omitempty"`
	// ContentHash is the delegationview per-workflow content hash.
	ContentHash string `json:"content_hash"`
	// Confirmation is ConfirmationUnavailable until E76.5 (#3768).
	Confirmation    string `json:"confirmation"`
	FieldsTruncated bool   `json:"fields_truncated,omitempty"`
}

// StandingOrders is the spec the delegation was read from.
type StandingOrders struct {
	Source                string `json:"source"`
	Ref                   string `json:"ref,omitempty"`
	WorkflowSHA           string `json:"workflow_sha,omitempty"`
	SpecVersion           string `json:"spec_version,omitempty"`
	SchemaMajor           int    `json:"schema_major"`
	DelegationContentHash string `json:"delegation_content_hash"`
}

// Cursor names the exact underlying query that retrieves an elided remainder.
type Cursor struct {
	Part PartKind `json:"part"`
	// Call is the underlying query rendered as its REST request line.
	Call         string `json:"call"`
	FromSequence int64  `json:"from_sequence,omitempty"`
	ToSequence   int64  `json:"to_sequence,omitempty"`
	// Offset is the first omitted row's offset in a list query.
	Offset int `json:"offset,omitempty"`
}

// Part is one underlying read's contribution to a section. Exactly one of
// Items / InFlight / Workflows is the part's collection (by Kind).
type Part struct {
	Kind              PartKind          `json:"kind"`
	Items             []digest.Item     `json:"items,omitempty"`
	InFlight          []InFlightItem    `json:"in_flight,omitempty"`
	Workflows         []DelegationEntry `json:"workflows,omitempty"`
	Unavailable       bool              `json:"unavailable,omitempty"`
	UnavailableReason string            `json:"unavailable_reason,omitempty"`
	Complete          bool              `json:"complete"`
	Truncated         bool              `json:"truncated"`
	OmittedCount      int               `json:"omitted_count"`
	Next              *Cursor           `json:"next,omitempty"`
}

// Section is one of the five brief sections.
type Section struct {
	Kind           SectionKind     `json:"kind"`
	Parts          []Part          `json:"parts"`
	StandingOrders *StandingOrders `json:"standing_orders,omitempty"`
	// Unavailable is set when the section has no contribution at all (every
	// part unavailable, or standing_orders with no delegation view).
	Unavailable bool `json:"unavailable,omitempty"`
}

// AbsentSection declares a section the issue names that has no source on main.
type AbsentSection struct {
	Section string `json:"section"`
	Reason  string `json:"reason"`
	Anchor  string `json:"anchor"`
}

// DeclaredAbsences is the fixed list of declared-unavailable sections.
func DeclaredAbsences() []AbsentSection {
	return []AbsentSection{
		{Section: "charter_revision", Reason: "no charter revision is recorded anywhere on main; the charter gate and prompt injection carry no revision identifier", Anchor: "#3242"},
		{Section: "adr_index", Reason: "no ADR index exists on main", Anchor: "E78"},
		{Section: "doctrine_changes", Reason: "the digest declares doctrine_changes absent and ships no doctrine-change query", Anchor: "#3733"},
	}
}

// Degradation names a contribution the brief could not include.
type Degradation struct {
	Kind     string      `json:"kind"`
	Section  SectionKind `json:"section,omitempty"`
	Part     PartKind    `json:"part,omitempty"`
	Sequence int64       `json:"sequence,omitempty"`
	Detail   string      `json:"detail,omitempty"`
}

// Brief is the wire model every surface shares.
type Brief struct {
	Repo           string `json:"repo"`
	CaptainSubject string `json:"captain_subject,omitempty"`
	Successor      string `json:"successor,omitempty"`
	Window         Window `json:"window"`
	// Section echoes a selector; empty for the whole brief. A render field,
	// excluded from the hash.
	Section      SectionKind     `json:"section,omitempty"`
	Sections     []Section       `json:"sections"`
	Absent       []AbsentSection `json:"absent"`
	Gaps         []digest.Gap    `json:"gaps"`
	Degradations []Degradation   `json:"degradations"`
	// Gaps-stream markers. UncitedNext continues the parked_without_citation
	// gaps (digest section open_decisions_uncited).
	GapsTruncated    bool    `json:"gaps_truncated"`
	GapsOmittedCount int     `json:"gaps_omitted_count"`
	GapsNext         *Cursor `json:"gaps_next,omitempty"`
	UncitedNext      *Cursor `json:"uncited_next,omitempty"`
	// BriefHash is hex(sha256) of the canonical form, computed ONCE by
	// Compose; Select and Bound carry it through unchanged.
	BriefHash string  `json:"brief_hash"`
	Truncated bool    `json:"truncated"`
	Next      *Cursor `json:"next,omitempty"`
}

// Errors.
var (
	// ErrInvalidRequest marks a malformed request (a surface maps it to 400).
	ErrInvalidRequest = errors.New("handoverbrief: invalid request")
	// ErrUnavailable marks a brief that could not be established at all.
	ErrUnavailable = errors.New("handoverbrief: brief unavailable")
)

// Unavailable reasons.
const (
	ReasonDependencyUnconfigured = "dependency_unconfigured"
	ReasonCaptainRecordReadFail  = "captain_record_read_failed"
	ReasonWindowReadFailed       = "window_read_failed"
)

// UnavailableError is the ONLY way Compose fails on I/O: the captain record or
// the window could not be read. It wraps ErrUnavailable; Reason is the
// machine-readable marker a caller records.
type UnavailableError struct {
	Reason string
	Err    error
}

func (e *UnavailableError) Error() string {
	return fmt.Sprintf("handoverbrief: brief unavailable (%s): %v", e.Reason, e.Err)
}

// Unwrap returns both ErrUnavailable and the cause.
func (e *UnavailableError) Unwrap() []error { return []error{ErrUnavailable, e.Err} }

// UnavailableReason returns err's machine-readable reason, or "" when err is
// not an *UnavailableError.
func UnavailableReason(err error) string {
	var ue *UnavailableError
	if errors.As(err, &ue) {
		return ue.Reason
	}
	return ""
}

// SnapshotReader is the captain record read (*captain.Store satisfies it).
type SnapshotReader interface {
	Read(ctx context.Context, accountID *uuid.UUID, repo string) (*captain.Snapshot, error)
}

// Deps are Compose's collaborators. Digest and Captain are REQUIRED; InFlight
// and Delegation are optional and degrade independently.
type Deps struct {
	Digest  digest.Deps
	Captain SnapshotReader
	// InFlight reads campaigns and runs; nil degrades both.
	InFlight *Store
	// Delegation is the caller-resolved view; nil degrades delegation_in_force
	// and standing_orders. DelegationUnavailableReason, when set, is carried
	// into the degradation detail.
	Delegation                  *delegationview.View
	DelegationUnavailableReason string
}

// Request selects a brief.
type Request struct {
	Repo      string
	AccountID *uuid.UUID
	// Subject is the reader; digest.Build keys its watermark lookup on it
	// (the value is unused because the brief always passes an explicit
	// window). Empty falls back to the captain, then to "handover-brief".
	Subject string
	// Successor names the incoming captain; empty falls back to the pending
	// offer's successor.
	Successor string
	// FromSequence / ToSequence (inclusive) override the derived window; 0
	// keeps the derivation (to = chain head).
	FromSequence int64
	ToSequence   int64
	// ScanLimit bounds every read (0 = digest.DefaultScanLimit).
	ScanLimit int
}

// Compose composes the whole, unbounded brief and stamps its BriefHash. Pass
// the result through Select (optional) and Bound before returning it.
func Compose(ctx context.Context, deps Deps, req Request) (Brief, error) {
	if req.Repo == "" {
		return Brief{}, fmt.Errorf("%w: repo is required", ErrInvalidRequest)
	}
	if req.FromSequence < 0 || req.ToSequence < 0 {
		return Brief{}, fmt.Errorf("%w: from_sequence and to_sequence must be non-negative", ErrInvalidRequest)
	}
	if deps.Digest.Store == nil || deps.Digest.Index == nil || deps.Captain == nil {
		return Brief{}, &UnavailableError{Reason: ReasonDependencyUnconfigured, Err: errors.New("digest store, decision index and captain store are required")}
	}
	snap, err := deps.Captain.Read(ctx, req.AccountID, req.Repo)
	if err != nil {
		return Brief{}, &UnavailableError{Reason: ReasonCaptainRecordReadFail, Err: err}
	}
	if snap == nil {
		snap = &captain.Snapshot{}
	}
	head, err := deps.Digest.Store.ChainHead(ctx, req.Repo)
	if err != nil {
		return Brief{}, &UnavailableError{Reason: ReasonWindowReadFailed, Err: err}
	}
	w := DeriveWindow(snap.Entries, head)
	if req.ToSequence > 0 {
		if req.ToSequence > head {
			return Brief{}, fmt.Errorf("%w: to_sequence %d is beyond the repository's chain head %d", ErrInvalidRequest, req.ToSequence, head)
		}
		w.ToSequence = req.ToSequence
	}
	if req.FromSequence > 0 {
		w.FromSequence, w.Basis = req.FromSequence, BasisRequested
	}

	b := Brief{
		Repo: req.Repo, Window: w, Successor: req.Successor,
		Sections: []Section{}, Absent: DeclaredAbsences(), Gaps: []digest.Gap{}, Degradations: []Degradation{},
	}
	if snap.State.Current != nil {
		b.CaptainSubject = snap.State.Current.Subject
	}
	if b.Successor == "" && snap.State.PendingOffer != nil {
		b.Successor = snap.State.PendingOffer.Successor
	}
	subject := req.Subject
	if subject == "" {
		subject = b.CaptainSubject
	}
	if subject == "" {
		subject = "handover-brief"
	}
	c := &composer{deps: deps, req: req, b: &b, subject: subject, limit: req.ScanLimit}
	if c.limit <= 0 {
		c.limit = digest.DefaultScanLimit
	}

	b.Sections = append(b.Sections,
		Section{Kind: SectionWhatChanged, Parts: []Part{
			c.digestPart(ctx, SectionWhatChanged, PartMerges, digest.SectionMerges, w.FromSequence, nil),
			c.digestPart(ctx, SectionWhatChanged, PartWaiversAndDeferrals, digest.SectionWaiversAndDeferrals, w.FromSequence, nil),
		}},
		Section{Kind: SectionNeedsDecision, Parts: []Part{
			c.digestPart(ctx, SectionNeedsDecision, PartUnansweredPages, digest.SectionPages, w.FromSequence, unanswered),
			// FromSequence 0: the digest lists every currently parked gate
			// (it starts open_decisions at 1 absent a cursor).
			c.digestPart(ctx, SectionNeedsDecision, PartOpenDecisions, digest.SectionOpenDecisions, 0, nil),
		}},
		Section{Kind: SectionInFlight, Parts: []Part{c.campaigns(ctx), c.runs(ctx)}},
	)
	b.Sections = append(b.Sections, c.delegation()...)
	c.windowGaps(ctx)
	for i := range b.Sections {
		markUnavailable(&b.Sections[i])
	}
	finalize(&b)
	b.BriefHash = Hash(b)
	return b, nil
}

type composer struct {
	deps    Deps
	req     Request
	b       *Brief
	subject string
	limit   int
}

func (c *composer) digestReq(sec digest.SectionKind, from int64) digest.Request {
	return digest.Request{
		Repo: c.req.Repo, CaptainSubject: c.subject, AccountID: c.req.AccountID,
		Section: sec, FromSequence: from, ToSequence: c.b.Window.ToSequence, ScanLimit: c.limit,
	}
}

func (c *composer) degrade(kind string, sec SectionKind, part PartKind, detail string) {
	c.b.Degradations = append(c.b.Degradations, Degradation{Kind: kind, Section: sec, Part: part, Detail: detail})
}

func unavailablePart(kind PartKind, reason string) Part {
	return Part{Kind: kind, Unavailable: true, UnavailableReason: reason}
}

// unanswered keeps only pages whose Answered is false or unset.
func unanswered(it digest.Item) bool { return it.Answered == nil || !*it.Answered }

// digestPart is one digest.Build section call. A read error is a degradation,
// never a failure of the brief.
func (c *composer) digestPart(ctx context.Context, sec SectionKind, part PartKind, dsec digest.SectionKind, from int64, keep func(digest.Item) bool) Part {
	d, err := digest.Build(ctx, c.deps.Digest, c.digestReq(dsec, from))
	if err != nil {
		c.degrade(DegradationDigestSectionFailed, sec, part, fmt.Sprintf("digest section %s: %v", dsec, err))
		return unavailablePart(part, DegradationDigestSectionFailed)
	}
	p := Part{Kind: part, Items: []digest.Item{}, Complete: true}
	for _, s := range d.Sections {
		if s.Kind != dsec {
			continue
		}
		for _, it := range s.Items {
			if keep == nil || keep(it) {
				p.Items = append(p.Items, it)
			}
		}
		p.Complete, p.Truncated, p.OmittedCount = s.Complete, s.Truncated, s.OmittedCount
		p.Next = fromDigestCursor(part, s.Next)
	}
	c.b.Gaps = append(c.b.Gaps, d.Gaps...)
	for _, dg := range d.Degradations {
		c.b.Degradations = append(c.b.Degradations, Degradation{Kind: dg.Kind, Section: sec, Part: part, Sequence: dg.Sequence, Detail: dg.Detail})
	}
	return p
}

// windowGaps adds the window's unindexed-decision gaps (digest section gaps)
// and the parked-without-citation gaps (digest section open_decisions_uncited).
// Item-derived gaps already arrived with their section calls, so only the
// kinds those calls cannot produce are taken here.
func (c *composer) windowGaps(ctx context.Context) {
	w := c.b.Window
	if d, err := digest.Build(ctx, c.deps.Digest, c.digestReq(digest.SectionGaps, w.FromSequence)); err != nil {
		c.degrade(DegradationDigestSectionFailed, SectionWhatChanged, PartGaps, fmt.Sprintf("digest section gaps: %v", err))
	} else {
		for _, g := range d.Gaps {
			if g.Kind == digest.GapUnindexed {
				c.b.Gaps = append(c.b.Gaps, g)
			}
		}
		for _, dg := range d.Degradations {
			if dg.Section == digest.SectionGaps {
				c.b.Degradations = append(c.b.Degradations, Degradation{Kind: dg.Kind, Section: SectionWhatChanged, Part: PartGaps, Sequence: dg.Sequence, Detail: dg.Detail})
			}
		}
		if d.GapsNext != nil {
			c.b.GapsTruncated, c.b.GapsOmittedCount = true, d.GapsOmittedCount
			c.b.GapsNext = fromDigestCursor(PartGaps, d.GapsNext)
		}
	}
	if d, err := digest.Build(ctx, c.deps.Digest, c.digestReq(digest.SectionOpenDecisionsUncited, 0)); err != nil {
		c.degrade(DegradationDigestSectionFailed, SectionNeedsDecision, PartOpenDecisions, fmt.Sprintf("digest section open_decisions_uncited: %v", err))
	} else {
		c.b.Gaps = append(c.b.Gaps, d.Gaps...)
		for _, dg := range d.Degradations {
			c.b.Degradations = append(c.b.Degradations, Degradation{Kind: dg.Kind, Section: SectionNeedsDecision, Part: PartOpenDecisions, Detail: dg.Detail})
		}
		c.b.UncitedNext = fromDigestCursor(PartOpenDecisions, d.UncitedNext)
	}
}

func fromDigestCursor(part PartKind, dc *digest.Cursor) *Cursor {
	if dc == nil {
		return nil
	}
	return &Cursor{Part: part, Call: dc.Call, FromSequence: dc.FromSequence, ToSequence: dc.ToSequence}
}

// campaignStates / runStates are the non-terminal states in_flight reads.
var (
	campaignStates = []string{"pending", "running", "paused", "awaiting_human"}
	runStates      = []string{"pending", "running"}
)

func (c *composer) campaigns(ctx context.Context) Part {
	if c.deps.InFlight == nil || c.deps.InFlight.campaigns == nil {
		c.degrade(DegradationCampaignStoreUnconfigured, SectionInFlight, PartCampaigns, "no campaign repository is configured")
		return unavailablePart(PartCampaigns, DegradationCampaignStoreUnconfigured)
	}
	return c.inFlight(ctx, PartCampaigns, campaignStates, DegradationCampaignReadFailed, c.deps.InFlight.Campaigns)
}

func (c *composer) runs(ctx context.Context) Part {
	if c.deps.InFlight == nil || c.deps.InFlight.runs == nil {
		c.degrade(DegradationRunStoreUnconfigured, SectionInFlight, PartRuns, "no run repository is configured")
		return unavailablePart(PartRuns, DegradationRunStoreUnconfigured)
	}
	return c.inFlight(ctx, PartRuns, runStates, DegradationRunReadFailed, c.deps.InFlight.Runs)
}

type listFn func(ctx context.Context, repo string, accountID *uuid.UUID, state string, limit int) ([]InFlightItem, error)

// inFlight reads each state LIMIT-bounded with the limit+1 probe: the extra
// row IS the first omitted item, and its offset is the cursor's.
func (c *composer) inFlight(ctx context.Context, part PartKind, states []string, failKind string, list listFn) Part {
	p := Part{Kind: part, InFlight: []InFlightItem{}, Complete: true}
	for _, st := range states {
		items, err := list(ctx, c.req.Repo, c.req.AccountID, st, c.limit+1)
		if err != nil {
			c.degrade(failKind, SectionInFlight, part, fmt.Sprintf("%s state %s: %v", part, st, err))
			return unavailablePart(part, failKind)
		}
		if len(items) > c.limit {
			items = items[:c.limit]
			p.Complete, p.Truncated = false, true
			p.OmittedCount++
			if p.Next == nil {
				p.Next = listCursor(c.req.Repo, part, st, c.limit)
			}
			c.degrade(DegradationScanLimit, SectionInFlight, part,
				fmt.Sprintf("%s in state %s: read limit %d reached; follow the part cursor for the rest", part, st, c.limit))
		}
		p.InFlight = append(p.InFlight, items...)
	}
	return p
}

// listCursor names the underlying list query for part in state at offset.
// The cursor parameter is the /v0/campaigns and /v0/runs offset cursor
// (base64url of "offset:N", server.decodeOffsetCursor).
func listCursor(repo string, part PartKind, state string, offset int) *Cursor {
	q := url.Values{}
	q.Set("repo", repo)
	q.Set("state", state)
	if offset > 0 {
		q.Set("cursor", encodeOffset(offset))
	}
	path := "/v0/runs"
	if part == PartCampaigns {
		path = "/v0/campaigns"
	}
	return &Cursor{Part: part, Call: "GET " + path + "?" + q.Encode(), Offset: offset}
}

func encodeOffset(n int) string {
	return base64URL("offset:" + strconv.Itoa(n))
}

// delegation projects delegation_in_force and standing_orders.
func (c *composer) delegation() []Section {
	v := c.deps.Delegation
	if v == nil {
		detail := "no delegation view was supplied"
		if c.deps.DelegationUnavailableReason != "" {
			detail = c.deps.DelegationUnavailableReason
		}
		c.degrade(DegradationDelegationUnavailable, SectionDelegationInForce, PartWorkflows, detail)
		return []Section{
			{Kind: SectionDelegationInForce, Parts: []Part{unavailablePart(PartWorkflows, DegradationDelegationUnavailable)}},
			{Kind: SectionStandingOrders, Parts: []Part{}, Unavailable: true},
		}
	}
	p := Part{Kind: PartWorkflows, Workflows: []DelegationEntry{}, Complete: true}
	for _, wf := range v.Workflows {
		e := DelegationEntry{
			WorkflowID: wf.ID, Autonomy: wf.Autonomy, MustPageHuman: wf.MustPageHuman,
			ContentHash: wf.ContentHash, Confirmation: ConfirmationUnavailable,
		}
		for _, esc := range wf.Escalations {
			e.Escalations = append(e.Escalations, EscalationCeiling{Match: esc.Match, MaxAutonomy: esc.MaxAutonomy})
		}
		p.Workflows = append(p.Workflows, e)
	}
	return []Section{
		{Kind: SectionDelegationInForce, Parts: []Part{p}},
		{Kind: SectionStandingOrders, Parts: []Part{}, StandingOrders: &StandingOrders{
			Source: v.Source, Ref: v.Ref, WorkflowSHA: v.WorkflowSHA, SpecVersion: v.SpecVersion,
			SchemaMajor: v.SchemaMajor, DelegationContentHash: v.ContentHash,
		}},
	}
}

// delegationCursor names the underlying delegation read. source is the
// standing orders' source (the delegation route's ?source): the route DEFAULTS
// to source=ref, so a brief composed from the run cache whose cursor named the
// bare path would point at a DIFFERENT query than the one the brief read — and
// following it can 502 forge_unavailable where the composed read succeeded.
// An empty source names the bare path (the route's own default).
func delegationCursor(repo, source string, offset int) *Cursor {
	call := "GET /v0/repos/" + repo + "/delegation"
	if source != "" {
		call += "?source=" + url.QueryEscape(source)
	}
	return &Cursor{Part: PartWorkflows, Call: call, Offset: offset}
}

// delegationSource reports the source the brief's standing orders recorded,
// or "" when the brief carries no standing orders (the delegation view was
// unavailable, so no source is known).
func delegationSource(b Brief) string {
	for _, s := range b.Sections {
		if s.Kind == SectionStandingOrders && s.StandingOrders != nil {
			return s.StandingOrders.Source
		}
	}
	return ""
}

func markUnavailable(s *Section) {
	if len(s.Parts) == 0 {
		return
	}
	for _, p := range s.Parts {
		if !p.Unavailable {
			return
		}
	}
	s.Unavailable = true
}

// finalize sets the top-level truncation markers from the collections.
func finalize(b *Brief) {
	b.Truncated, b.Next = false, nil
	for _, s := range b.Sections {
		for _, p := range s.Parts {
			if p.Truncated {
				b.Truncated = true
				if b.Next == nil {
					b.Next = p.Next
				}
			}
		}
	}
	for _, cur := range []*Cursor{b.GapsNext, b.UncitedNext} {
		if cur != nil {
			b.Truncated = true
			if b.Next == nil {
				b.Next = cur
			}
		}
	}
	if b.GapsTruncated {
		b.Truncated = true
	}
}

// Select narrows b to one section, keeping BriefHash (the hash of the WHOLE
// canonical brief) unchanged. The empty kind returns b.
func Select(b Brief, kind SectionKind) (Brief, error) {
	k, err := ParseSection(string(kind))
	if err != nil || k == "" {
		return b, err
	}
	out := b
	out.Section = k
	out.Sections = []Section{}
	for _, s := range b.Sections {
		if s.Kind == k {
			out.Sections = append(out.Sections, s)
		}
	}
	finalize(&out)
	return out, nil
}

// canonical is the ONE form BriefHash is computed over: the full composition
// with the hash itself, the section selector and every truncation marker and
// bounding field zeroed. Degradations (including scan_limit bites) stay in:
// they are composition facts, not bounding metadata.
func canonical(b Brief) Brief {
	b.BriefHash, b.Section = "", ""
	b.Truncated, b.Next = false, nil
	b.GapsTruncated, b.GapsOmittedCount, b.GapsNext, b.UncitedNext = false, 0, nil, nil
	secs := make([]Section, len(b.Sections))
	for i, s := range b.Sections {
		parts := make([]Part, len(s.Parts))
		for j, p := range s.Parts {
			p.Complete, p.Truncated, p.OmittedCount, p.Next = false, false, 0, nil
			if p.Items != nil {
				items := make([]digest.Item, len(p.Items))
				for k, it := range p.Items {
					it.FieldsTruncated = false
					items[k] = it
				}
				p.Items = items
			}
			if p.InFlight != nil {
				fl := make([]InFlightItem, len(p.InFlight))
				for k, it := range p.InFlight {
					it.FieldsTruncated = false
					fl[k] = it
				}
				p.InFlight = fl
			}
			if p.Workflows != nil {
				ws := make([]DelegationEntry, len(p.Workflows))
				for k, e := range p.Workflows {
					e.FieldsTruncated = false
					ws[k] = e
				}
				p.Workflows = ws
			}
			parts[j] = p
		}
		s.Parts = parts
		secs[i] = s
	}
	b.Sections = secs
	return b
}

// Hash returns hex(sha256) over json.Marshal of the canonical form. Every
// brief type is a plain struct with ordered slices and no map, so the bytes
// are a function of the content alone (the delegationview precedent). A
// marshalling failure is unreachable for these types and degrades to "".
func Hash(b Brief) string {
	raw, err := json.Marshal(canonical(b))
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}
