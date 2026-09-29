// Package digest computes the "since you last looked" digest (E75.6 / #3734,
// ADR-082 #3728 rule 7): a per-captain, per-repository summary of what the
// audit chain recorded since that captain's READ WATERMARK, computed on demand
// from the decision index (backend/internal/decisionindex, E75.2 / #3730) and
// the chain itself.
//
// Load-bearing properties, in the order a reader should check them:
//
//   - Mark-read is never silent. MarkRead appends the digest_marked_read chain
//     entry FIRST and advances the watermark ONLY after that append succeeds
//     (watermark.go). Retrieval (Build) never writes.
//   - Every item cites its chain entry by sequence and entry hash. An index
//     row whose source entry is absent, a decision-bearing entry the
//     best-effort index writer never recorded, and a parked stage with no
//     identifiable parking entry are all reported as GAPS — never dropped and
//     never cited with a made-up sequence.
//   - ONE serialized-size bound (bound.go) applies to every surface; truncation
//     is marked and every elided remainder carries a cursor naming the exact
//     next call.
//
// The four shipped content sections are the closed set {merges,
// waivers_and_deferrals, pages, open_decisions}. Two content kinds are
// deliberately ABSENT until the work they depend on exists: doctrine_changes
// (#3733) and scheduled_runs (#3725).
package digest

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"sort"
	"strconv"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/decisionindex"
)

// SectionKind names a digest section, or the gaps selector.
type SectionKind string

// The four shipped content sections plus the gaps selector.
const (
	SectionMerges              SectionKind = "merges"
	SectionWaiversAndDeferrals SectionKind = "waivers_and_deferrals"
	SectionPages               SectionKind = "pages"
	SectionOpenDecisions       SectionKind = "open_decisions"
	// SectionGaps is a SELECTOR, not a content section: it addresses the
	// Gaps + Degradations collections so their truncated remainder is
	// retrievable with the same cursor mechanism (#3734 approval condition 2).
	SectionGaps SectionKind = "gaps"
)

// ContentSections is the closed, ordered set of content sections a full
// digest carries.
var ContentSections = []SectionKind{SectionMerges, SectionWaiversAndDeferrals, SectionPages, SectionOpenDecisions}

// ParseSection validates a section selector. The empty string selects the
// full digest.
func ParseSection(s string) (SectionKind, error) {
	switch k := SectionKind(s); k {
	case "", SectionMerges, SectionWaiversAndDeferrals, SectionPages, SectionOpenDecisions, SectionGaps:
		return k, nil
	}
	return "", fmt.Errorf("%w: unknown section %q (want one of merges, waivers_and_deferrals, pages, open_decisions, gaps)", ErrInvalidRequest, s)
}

// Item is one digest entry. SourceSequence and SourceEntryHash cite the chain
// entry the item is ABOUT; SourceSequence is also the item's ordering key
// within its section and the value a continuation cursor carries.
type Item struct {
	// Category is the audit category of the cited entry.
	Category        string     `json:"category"`
	SourceSequence  int64      `json:"source_sequence"`
	SourceEntryHash string     `json:"source_entry_hash"`
	RunID           uuid.UUID  `json:"run_id"`
	StageID         *uuid.UUID `json:"stage_id,omitempty"`
	StageKind       string     `json:"stage_kind,omitempty"`
	// StageState is set on open_decisions items: the awaiting state the stage
	// is parked in.
	StageState string    `json:"stage_state,omitempty"`
	At         time.Time `json:"at"`
	Outcome    string    `json:"outcome,omitempty"`
	// Headline is a short, derived, human-readable label. It is
	// variable-length, so Bound caps it (FieldsTruncated marks the cut).
	Headline        string `json:"headline,omitempty"`
	ConcernCategory string `json:"concern_category,omitempty"`
	Severity        string `json:"severity,omitempty"`
	// ReasonSequence/ReasonKey POINT at the chain entry and payload key
	// recording the reason (waivers_and_deferrals); the prose is never copied.
	ReasonSequence int64  `json:"reason_sequence,omitempty"`
	ReasonKey      string `json:"reason_key,omitempty"`
	AmendmentID    string `json:"amendment_id,omitempty"`
	// Answered is set on pages items only (nil elsewhere) — see the paired
	// answered predicate on answeredBy in store.go.
	Answered         *bool `json:"answered,omitempty"`
	AnsweredSequence int64 `json:"answered_sequence,omitempty"`
	// SourceMissing marks an item whose cited source entry has no
	// audit_entries row (a source_entry_missing gap is reported beside it).
	SourceMissing bool `json:"source_missing,omitempty"`
	// FieldsTruncated marks an item one of whose variable-length fields Bound
	// capped.
	FieldsTruncated bool `json:"fields_truncated,omitempty"`
}

// Cursor names the exact next call that retrieves an elided remainder.
// FromSequence is the source_sequence of the FIRST OMITTED item (inclusive),
// so a page that retained nothing still carries a cursor and following it
// always starts at an item this response did not return.
type Cursor struct {
	Section      SectionKind `json:"section"`
	FromSequence int64       `json:"from_sequence"`
	ToSequence   int64       `json:"to_sequence"`
	// Call is the next call rendered as its REST request line; the MCP tool
	// passes the same section/from_sequence/to_sequence as arguments.
	Call string `json:"call"`
}

// Section is one content section.
type Section struct {
	Kind  SectionKind `json:"kind"`
	Items []Item      `json:"items"`
	// Complete is true when nothing was elided: following no cursor is
	// needed. It is always !Truncated.
	Complete  bool `json:"complete"`
	Truncated bool `json:"truncated"`
	// OmittedCount counts the items known to be elided from this response.
	// When a scan limit also bit (a scan_limit Degradation names it) it is a
	// LOWER bound: the rows past the limit were never read.
	OmittedCount int     `json:"omitted_count"`
	Next         *Cursor `json:"next,omitempty"`
}

// Gap kinds.
const (
	// GapUnindexed: a decision-bearing entry in the window with no
	// decision_index row (decisionindex.GapsInWindow).
	GapUnindexed = "unindexed_decision"
	// GapSourceEntryMissing: an index row whose cited source entry has no
	// audit_entries row.
	GapSourceEntryMissing = "source_entry_missing"
	// GapSourceHashMismatch: an index row whose cited entry hash differs from
	// the chain entry's.
	GapSourceHashMismatch = "source_hash_mismatch"
	// GapParkedWithoutCitation: a parked stage with no identifiable parking
	// entry. It has no chain position, so Sequence is 0 and it is carried in
	// the constant-size floor (bounded by uncitedLimit), never trimmed.
	GapParkedWithoutCitation = "parked_without_citation"
)

// Gap is a fact the digest could not ground on the chain + index.
type Gap struct {
	Kind     string     `json:"kind"`
	Sequence int64      `json:"sequence"`
	Category string     `json:"category,omitempty"`
	RunID    *uuid.UUID `json:"run_id,omitempty"`
	StageID  *uuid.UUID `json:"stage_id,omitempty"`
	Detail   string     `json:"detail,omitempty"`
}

// Degradation kinds.
const (
	// DegradationScanLimit: a bounded read hit its LIMIT. The section's
	// cursor retrieves the rest; the degradation names the bite so a short
	// list is never silent. Sequence 0: carried in the floor, never trimmed.
	DegradationScanLimit = "scan_limit"
	// DegradationReasonMissing: a waiver/deferral index row with no reason
	// pointer (reason_sequence 0 or reason_key empty).
	DegradationReasonMissing = "reason_missing"
)

// Degradation names a place the digest is less complete than the chain.
type Degradation struct {
	Kind     string      `json:"kind"`
	Section  SectionKind `json:"section,omitempty"`
	Sequence int64       `json:"sequence,omitempty"`
	Detail   string      `json:"detail,omitempty"`
}

// Digest is the wire model shared by every surface.
type Digest struct {
	Repo           string `json:"repo"`
	CaptainSubject string `json:"captain_subject"`
	// Watermark is the captain's last-read sequence (0 with HasWatermark
	// false when the captain never marked read).
	Watermark    int64 `json:"watermark"`
	HasWatermark bool  `json:"has_watermark"`
	// FromSequence..ToSequence is the INCLUSIVE window the content sections
	// cover (open_decisions excepted: it lists every currently parked gate
	// cited at or below ToSequence).
	FromSequence int64 `json:"from_sequence"`
	ToSequence   int64 `json:"to_sequence"`
	ChainHead    int64 `json:"chain_head"`
	// Section echoes the selector; empty for a full digest.
	Section          SectionKind   `json:"section,omitempty"`
	Sections         []Section     `json:"sections"`
	Gaps             []Gap         `json:"gaps"`
	Degradations     []Degradation `json:"degradations"`
	GapsTruncated    bool          `json:"gaps_truncated"`
	GapsOmittedCount int           `json:"gaps_omitted_count"`
	GapsNext         *Cursor       `json:"gaps_next,omitempty"`
	// Truncated is true when anything in this response was elided; Next is
	// then the first elided collection's cursor.
	Truncated bool    `json:"truncated"`
	Next      *Cursor `json:"next,omitempty"`
}

// Errors.
var (
	// ErrInvalidRequest marks a malformed request (a surface maps it to 400).
	ErrInvalidRequest = errors.New("digest: invalid request")
)

// BeyondChainHeadError refuses a to_sequence above the repository's chain
// head, naming the head.
type BeyondChainHeadError struct {
	Requested int64
	Head      int64
}

func (e *BeyondChainHeadError) Error() string {
	return fmt.Sprintf("digest: to_sequence %d is beyond the repository's chain head %d", e.Requested, e.Head)
}

// Request selects a digest. FromSequence and ToSequence are INCLUSIVE; 0
// defaults them to the captain's watermark + 1 and the repository's chain head
// respectively. A continuation passes a Cursor's Section and FromSequence
// verbatim.
type Request struct {
	Repo           string
	CaptainSubject string
	AccountID      *uuid.UUID
	Section        SectionKind
	FromSequence   int64
	ToSequence     int64
	// ScanLimit bounds every read (0 = DefaultScanLimit); tests lower it to
	// exercise the bite.
	ScanLimit int
}

// DefaultScanLimit bounds each section's read.
const DefaultScanLimit = 500

// uncitedLimit bounds the parked-without-citation gaps, which have no chain
// position to page by and so ride in Bound's constant-size floor.
const uncitedLimit = 16

// IndexReader is the decisionindex read surface Build needs.
type IndexReader interface {
	List(ctx context.Context, f decisionindex.ListFilter) ([]decisionindex.Row, error)
	GapsInWindow(ctx context.Context, f decisionindex.GapFilter) (*decisionindex.GapReport, error)
}

// Deps are Build's collaborators. Both run over the caller's (tenant-scoped)
// connection.
type Deps struct {
	Store *Store
	Index IndexReader
}

// Build computes the digest for req. It NEVER writes: in particular it never
// advances the watermark (only MarkRead does). The result is unbounded in
// serialized size; pass it through Bound before returning it to a caller.
func Build(ctx context.Context, deps Deps, req Request) (Digest, error) {
	if req.Repo == "" {
		return Digest{}, fmt.Errorf("%w: repo is required", ErrInvalidRequest)
	}
	if req.CaptainSubject == "" {
		return Digest{}, fmt.Errorf("%w: captain subject is required", ErrInvalidRequest)
	}
	if _, err := ParseSection(string(req.Section)); err != nil {
		return Digest{}, err
	}
	if req.FromSequence < 0 || req.ToSequence < 0 {
		return Digest{}, fmt.Errorf("%w: from_sequence and to_sequence must be non-negative", ErrInvalidRequest)
	}
	limit := req.ScanLimit
	if limit <= 0 {
		limit = DefaultScanLimit
	}
	head, err := deps.Store.ChainHead(ctx, req.Repo)
	if err != nil {
		return Digest{}, err
	}
	wm, has, err := deps.Store.GetWatermark(ctx, req.AccountID, req.CaptainSubject, req.Repo)
	if err != nil {
		return Digest{}, err
	}
	to := req.ToSequence
	if to == 0 {
		to = head
	}
	if to > head {
		return Digest{}, &BeyondChainHeadError{Requested: to, Head: head}
	}
	from := req.FromSequence
	if from == 0 {
		from = wm + 1
	}
	d := Digest{
		Repo: req.Repo, CaptainSubject: req.CaptainSubject,
		Watermark: wm, HasWatermark: has,
		FromSequence: from, ToSequence: to, ChainHead: head,
		Section:  req.Section,
		Sections: []Section{}, Gaps: []Gap{}, Degradations: []Degradation{},
	}
	b := &builder{deps: deps, req: req, d: &d, limit: limit, to: to, from: from}

	want := func(k SectionKind) bool { return req.Section == "" || req.Section == k }
	emitGaps := req.Section == "" || req.Section == SectionGaps
	// The gaps selector recomputes the item-derived gaps (source_entry_missing,
	// reason_missing) from the same reads, without emitting the items.
	for _, k := range ContentSections {
		if !want(k) && !emitGaps {
			continue
		}
		sec, err := b.section(ctx, k)
		if err != nil {
			return Digest{}, err
		}
		if want(k) && req.Section != SectionGaps {
			d.Sections = append(d.Sections, sec)
		}
	}
	if emitGaps {
		if err := b.unindexedGaps(ctx); err != nil {
			return Digest{}, err
		}
		if req.FromSequence == 0 {
			// Parked-without-citation gaps have no chain position; they ride
			// only the non-continuation call (a cursor-following call carries
			// an explicit FromSequence).
			if err := b.uncitedParked(ctx); err != nil {
				return Digest{}, err
			}
		}
	} else {
		// A single content section carries only its own findings.
		d.Gaps = []Gap{}
		d.Degradations = keepSection(d.Degradations, req.Section)
	}
	sortGaps(d.Gaps)
	sortDegradations(d.Degradations)
	return d, nil
}

type builder struct {
	deps     Deps
	req      Request
	d        *Digest
	limit    int
	from, to int64
}

// sectionFrom is a section's inclusive lower bound. open_decisions lists
// every CURRENTLY parked gate, so absent an explicit cursor it starts at 1
// rather than the watermark: a gate parked before the last mark-read is still
// open.
func (b *builder) sectionFrom(k SectionKind) int64 {
	if k == SectionOpenDecisions && b.req.FromSequence == 0 {
		return 1
	}
	return b.from
}

func (b *builder) section(ctx context.Context, k SectionKind) (Section, error) {
	sec := Section{Kind: k, Items: []Item{}, Complete: true}
	from := b.sectionFrom(k)
	if b.to == 0 || from > b.to {
		return sec, nil // empty window (e.g. a chain with no entries yet)
	}
	var (
		items []Item
		err   error
	)
	switch k {
	case SectionMerges:
		items, err = b.indexItems(ctx, from, decisionindex.ClassMergeVerdict)
	case SectionWaiversAndDeferrals:
		items, err = b.indexItems(ctx, from, decisionindex.ClassConcernWaive, decisionindex.ClassConcernDefer)
	case SectionPages:
		items, err = b.pages(ctx, from)
	case SectionOpenDecisions:
		items, err = b.openDecisions(ctx, from)
	}
	if err != nil {
		return Section{}, err
	}
	if len(items) > b.limit {
		// Read limit+1: the extra row IS the first omitted item.
		sec.Truncated, sec.Complete = true, false
		sec.OmittedCount = 1
		sec.Next = NewCursor(b.d.Repo, k, items[b.limit].SourceSequence, b.to)
		b.d.Degradations = append(b.d.Degradations, Degradation{Kind: DegradationScanLimit, Section: k,
			Detail: fmt.Sprintf("read limit %d reached; follow the section cursor for the rest", b.limit)})
		items = items[:b.limit]
	}
	sec.Items = items
	return sec, nil
}

// indexItems reads decision_index rows of the given classes over [from, to],
// merged by source sequence, and checks every citation against the chain.
func (b *builder) indexItems(ctx context.Context, from int64, classes ...decisionindex.DecisionClass) ([]Item, error) {
	var rows []decisionindex.Row
	for _, c := range classes {
		got, err := b.deps.Index.List(ctx, decisionindex.ListFilter{
			Repo: b.req.Repo, DecisionClass: c, FromSequence: from, ToSequence: b.to, Limit: b.limit + 1,
		})
		if err != nil {
			return nil, err
		}
		rows = append(rows, got...)
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].SourceSequence < rows[j].SourceSequence })
	if len(rows) > b.limit+1 {
		rows = rows[:b.limit+1]
	}
	seqs := make([]int64, 0, len(rows))
	for _, r := range rows {
		seqs = append(seqs, r.SourceSequence)
	}
	hashes, err := b.deps.Store.EntryHashes(ctx, seqs)
	if err != nil {
		return nil, err
	}
	items := make([]Item, 0, len(rows))
	for i, r := range rows {
		it := Item{
			Category: categoryFor(r.DecisionClass), SourceSequence: r.SourceSequence, SourceEntryHash: r.SourceEntryHash,
			RunID: r.RunID, StageID: r.StageID, StageKind: r.StageKind, At: r.DecidedAt, Outcome: r.Outcome,
			ConcernCategory: r.ConcernCategoryRaw, Severity: r.Severity,
		}
		switch r.DecisionClass {
		case decisionindex.ClassMergeVerdict:
			it.Headline = "merge verdict: " + r.Outcome
		default:
			it.Headline = string(r.DecisionClass) + ": " + r.ConcernCategoryRaw
			it.ReasonSequence, it.ReasonKey = r.ReasonSequence, r.ReasonKey
		}
		// Findings are recorded only for items this response can carry (the
		// limit+1 look-ahead row is a cursor, not an item).
		if i < b.limit {
			runID := r.RunID
			if h, ok := hashes[r.SourceSequence]; !ok {
				it.SourceMissing = true
				b.d.Gaps = append(b.d.Gaps, Gap{Kind: GapSourceEntryMissing, Sequence: r.SourceSequence,
					Category: it.Category, RunID: &runID, StageID: r.StageID,
					Detail: "index row cites a sequence with no audit_entries row"})
			} else if h != r.SourceEntryHash {
				b.d.Gaps = append(b.d.Gaps, Gap{Kind: GapSourceHashMismatch, Sequence: r.SourceSequence,
					Category: it.Category, RunID: &runID, StageID: r.StageID,
					Detail: "index row's source_entry_hash differs from the chain entry's"})
			}
			if r.DecisionClass != decisionindex.ClassMergeVerdict && (r.ReasonSequence == 0 || r.ReasonKey == "") {
				b.d.Degradations = append(b.d.Degradations, Degradation{Kind: DegradationReasonMissing,
					Section: SectionWaiversAndDeferrals, Sequence: r.SourceSequence,
					Detail: "waiver/deferral index row carries no reason pointer"})
			}
		}
		items = append(items, it)
	}
	return items, nil
}

func (b *builder) pages(ctx context.Context, from int64) ([]Item, error) {
	rows, err := b.deps.Store.Pages(ctx, b.req.Repo, from, b.to, b.limit+1)
	if err != nil {
		return nil, err
	}
	items := make([]Item, 0, len(rows))
	for _, p := range rows {
		answered := p.AnsweredSequence > 0
		it := Item{
			Category: p.Category, SourceSequence: p.Sequence, SourceEntryHash: p.EntryHash,
			RunID: p.RunID, StageID: p.StageID, StageKind: p.StageKind, At: p.At,
			AmendmentID: p.AmendmentID, Answered: &answered, AnsweredSequence: p.AnsweredSequence,
			Headline: p.Category + " (" + map[bool]string{true: "answered", false: "unanswered"}[answered] + ")",
		}
		items = append(items, it)
	}
	return items, nil
}

func (b *builder) openDecisions(ctx context.Context, from int64) ([]Item, error) {
	rows, err := b.deps.Store.ParkedStages(ctx, b.req.Repo, from, b.to, b.limit+1)
	if err != nil {
		return nil, err
	}
	items := make([]Item, 0, len(rows))
	for _, p := range rows {
		stageID := p.StageID
		items = append(items, Item{
			Category: p.Category, SourceSequence: p.Sequence, SourceEntryHash: p.EntryHash,
			RunID: p.RunID, StageID: &stageID, StageKind: p.StageKind, StageState: p.State, At: p.At,
			Headline: p.StageKind + " stage " + p.State,
		})
	}
	return items, nil
}

func (b *builder) unindexedGaps(ctx context.Context) error {
	if b.to == 0 || b.from > b.to {
		return nil
	}
	rep, err := b.deps.Index.GapsInWindow(ctx, decisionindex.GapFilter{
		Repo: b.req.Repo, FromSequence: b.from, ToSequence: b.to, Limit: b.limit + 1,
	})
	if err != nil {
		return err
	}
	listed := rep.Gaps
	if len(listed) > b.limit {
		// Read limit+1: the extra gap IS the first omitted one.
		b.d.GapsTruncated = true
		b.d.GapsOmittedCount += rep.GapCount - b.limit
		b.d.GapsNext = NewCursor(b.req.Repo, SectionGaps, listed[b.limit].SourceSequence, b.to)
		b.d.Degradations = append(b.d.Degradations, Degradation{Kind: DegradationScanLimit, Section: SectionGaps,
			Detail: fmt.Sprintf("%d unindexed entries past read limit %d; follow the gaps cursor for the rest", rep.GapCount-b.limit, b.limit)})
		listed = listed[:b.limit]
	}
	for _, g := range listed {
		runID := g.RunID
		b.d.Gaps = append(b.d.Gaps, Gap{Kind: GapUnindexed, Sequence: g.SourceSequence, Category: g.Category,
			RunID: &runID, Detail: "decision-bearing entry has no decision_index row"})
	}
	return nil
}

func (b *builder) uncitedParked(ctx context.Context) error {
	rows, err := b.deps.Store.UncitedParkedStages(ctx, b.req.Repo, uncitedLimit+1)
	if err != nil {
		return err
	}
	if len(rows) > uncitedLimit {
		rows = rows[:uncitedLimit]
		b.d.Degradations = append(b.d.Degradations, Degradation{Kind: DegradationScanLimit, Section: SectionOpenDecisions,
			Detail: fmt.Sprintf("more than %d parked stages have no identifiable parking entry; only the first %d are listed", uncitedLimit, uncitedLimit)})
	}
	for _, p := range rows {
		runID, stageID := p.RunID, p.StageID
		b.d.Gaps = append(b.d.Gaps, Gap{Kind: GapParkedWithoutCitation, RunID: &runID, StageID: &stageID,
			Detail: fmt.Sprintf("%s stage parked in %s with no identifiable parking entry", p.StageKind, p.State)})
	}
	return nil
}

// categoryFor maps a decision class back to its audit category.
func categoryFor(c decisionindex.DecisionClass) string {
	for _, cat := range decisionindex.DecisionBearingCategories() {
		if cl, _ := decisionindex.ClassFor(cat); cl == c {
			return cat
		}
	}
	return string(c)
}

func keepSection(ds []Degradation, k SectionKind) []Degradation {
	out := []Degradation{}
	for _, d := range ds {
		if d.Section == k {
			out = append(out, d)
		}
	}
	return out
}

func sortGaps(gs []Gap) {
	sort.SliceStable(gs, func(i, j int) bool { return gs[i].Sequence < gs[j].Sequence })
}

func sortDegradations(ds []Degradation) {
	sort.SliceStable(ds, func(i, j int) bool { return ds[i].Sequence < ds[j].Sequence })
}

// NewCursor builds a continuation cursor whose Call names the exact next
// REST request.
func NewCursor(repo string, section SectionKind, from, to int64) *Cursor {
	q := url.Values{}
	q.Set("repo", repo)
	q.Set("section", string(section))
	q.Set("from_sequence", strconv.FormatInt(from, 10))
	q.Set("to_sequence", strconv.FormatInt(to, 10))
	return &Cursor{Section: section, FromSequence: from, ToSequence: to, Call: "GET /v0/digest?" + q.Encode()}
}
