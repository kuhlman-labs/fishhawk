package userreport

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/captain"
	"github.com/kuhlman-labs/fishhawk/backend/internal/workmgmt"
)

// DefaultInitialLookback is how far back a repository's FIRST scan reads when
// no cursor row exists: far enough to catch recent activity, short enough that
// the first run on a large repository does not flood the comms stage.
const DefaultInitialLookback = 14 * 24 * time.Hour

// ErrInvalidParams reports a ScanParams Scan refuses before any I/O.
var ErrInvalidParams = errors.New("userreport: invalid scan params")

// ErrCursorNotAdvanced reports a scan whose report WAS recorded but whose
// cursor advance failed afterwards. The next scan re-reads the same window —
// duplicates are the safe direction, skips are not.
var ErrCursorNotAdvanced = errors.New("userreport: report recorded but cursor not advanced")

// CursorStore is the cursor persistence Scan drives. *Store implements it;
// each call is its own short transaction.
type CursorStore interface {
	Get(ctx context.Context, key Key) (time.Time, bool, error)
	Init(ctx context.Context, key Key, initial time.Time) (time.Time, error)
	Advance(ctx context.Context, key Key, to time.Time) (time.Time, bool, error)
}

// CaptainReader reads the repository's captain record (captain.Store.Read's
// signature).
type CaptainReader interface {
	Read(ctx context.Context, accountID *uuid.UUID, repo string) (*captain.Snapshot, error)
}

// Recorder durably records a scan's report. Scan advances the cursor ONLY
// after Record returns nil.
type Recorder interface {
	Record(ctx context.Context, r Report) error
}

// RecorderFunc adapts a function to Recorder.
type RecorderFunc func(ctx context.Context, r Report) error

// Record calls f.
func (f RecorderFunc) Record(ctx context.Context, r Report) error { return f(ctx, r) }

// ReportDegradationCode is the CLOSED set of REPORT-level gap codes Scan sets.
// It is distinct from workmgmt's page-level UserReportDegradationCode set and
// never shares a value with it.
type ReportDegradationCode string

// CaptainUnavailable means the current captain could not be used as an
// internal-author signal: no captain reader was configured, the read failed,
// the seated subject is not identity-verified, or it is qualified for a
// different forge. A VACANT seat is a normal state and names nothing.
const CaptainUnavailable ReportDegradationCode = "captain_unavailable"

// ReportDegradationCodes returns the closed report-level code set.
func ReportDegradationCodes() []ReportDegradationCode {
	return []ReportDegradationCode{CaptainUnavailable}
}

// DegradationSource says which closed set a Degradation's Code belongs to.
type DegradationSource string

const (
	// DegradationSourcePage is a provider's page-level code
	// (workmgmt.UserReportDegradationCode), copied from the page.
	DegradationSourcePage DegradationSource = "page"
	// DegradationSourceReport is a Scan-level code (ReportDegradationCode).
	DegradationSourceReport DegradationSource = "report"
)

// Degradation is one named gap on a Report. Source names which closed set
// Code is drawn from; Count is the affected count (0 for a report-wide gap).
type Degradation struct {
	Source DegradationSource
	Code   string
	Detail string
	Count  int
}

// ReportItem is one page item with its classification.
type ReportItem struct {
	workmgmt.UserReportItem
	Classification     Classification
	Basis              string
	MarkerFromExternal bool
}

// Report is what one scan hands its Recorder. Since is the bound the listing
// read from; NextCursor is the bound the cursor advances to once Record
// succeeds. NoteSince and NextNoteCursor are the same pair for the note floor
// (SourceIssueNotes). Items carry the page's order (deduplicated,
// deterministic).
type Report struct {
	AccountID      *uuid.UUID
	Repo           string
	Source         Source
	Forge          string
	Since          time.Time
	NextCursor     time.Time
	NoteSince      time.Time
	NextNoteCursor time.Time
	Items          []ReportItem
	Degradations   []Degradation
}

// ScanParams configures one Scan.
//
// Repo is the repository string the cursor and captain record are keyed by;
// Target is what the Reader lists. Captain is optional: nil names
// captain_unavailable. InitialLookback is a HOST-CLOCK WINDOW bound only (0 =
// DefaultInitialLookback, negative refused); Now defaults to time.Now.
// Logger receives the raw captain read error the report's fixed Detail
// withholds; nil selects slog.Default().
type ScanParams struct {
	AccountID       *uuid.UUID
	Repo            string
	Source          Source
	Target          workmgmt.Target
	Reader          workmgmt.UserReportReader
	Cursors         CursorStore
	Captain         CaptainReader
	Record          Recorder
	InitialLookback time.Duration
	Now             func() time.Time
	Logger          *slog.Logger
}

// ScanResult reports a successful scan: the recorded report and the cursor
// committed after it (Advanced false when it did not move, e.g. a held
// cursor_anchor_unavailable scan).
type ScanResult struct {
	Report   Report
	Cursor   time.Time
	Advanced bool
}

func (p ScanParams) validate() error {
	switch {
	case p.Record == nil:
		return fmt.Errorf("%w: a recorder is required (a scan with nowhere to record must not advance the cursor)", ErrInvalidParams)
	case p.Reader == nil:
		return fmt.Errorf("%w: a reader is required", ErrInvalidParams)
	case p.Cursors == nil:
		return fmt.Errorf("%w: a cursor store is required", ErrInvalidParams)
	case p.InitialLookback < 0:
		return fmt.Errorf("%w: initial lookback %s is negative (the first since must be earlier than now)", ErrInvalidParams, p.InitialLookback)
	case p.Source == SourceIssueNotes:
		return fmt.Errorf("%w: source %q is a note-floor row, not a scannable source", ErrInvalidParams, p.Source)
	}
	return p.key().validate()
}

func (p ScanParams) key() Key {
	return Key{AccountID: p.AccountID, Repo: p.Repo, Source: p.Source}
}

// noteKey selects the scan's note-floor row (SourceIssueNotes).
func (p ScanParams) noteKey() Key {
	return Key{AccountID: p.AccountID, Repo: p.Repo, Source: SourceIssueNotes}
}

// Scan reads every issue and comment updated since the stored cursor,
// classifies each, hands the report to Record, and advances the cursor ONLY
// after Record succeeds.
//
//  1. Validate (a nil Record is refused before any read).
//  2. Read the cursor. With no row, compute Now − InitialLookback ONCE and
//     persist it via Init, which returns the STORED value; read with that.
//     Read the note floor (SourceIssueNotes); an absent row, or one later
//     than the cursor, means the cursor.
//  3. List through Reader with Since and NoteSince. Any error returns with
//     both rows untouched.
//  4. Resolve the captain (degrades, never fails).
//  5. Classify, build the Report, call Record. An error returns with both
//     rows untouched, so the next scan passes the identical bounds.
//  6. Advance the NOTE FLOOR to the page's NextNoteCursor FIRST, then the
//     cursor to NextCursor (both monotonic). The order matters: a truncated
//     scan holds the floor below the cursor, and an absent floor row reads as
//     the cursor, so the floor must be durable before the cursor moves past
//     it. A failure in either returns ErrCursorNotAdvanced: the report exists
//     and the next scan re-reads.
//
// Each cursor call is its own short transaction; none is held across the
// listing.
func Scan(ctx context.Context, p ScanParams) (*ScanResult, error) {
	if err := p.validate(); err != nil {
		return nil, err
	}
	key := p.key()
	since, found, err := p.Cursors.Get(ctx, key)
	if err != nil {
		return nil, fmt.Errorf("userreport: scan: read cursor: %w", err)
	}
	if !found {
		lookback := p.InitialLookback
		if lookback == 0 {
			lookback = DefaultInitialLookback
		}
		now := time.Now
		if p.Now != nil {
			now = p.Now
		}
		if since, err = p.Cursors.Init(ctx, key, now().Add(-lookback)); err != nil {
			return nil, fmt.Errorf("userreport: scan: initialise cursor: %w", err)
		}
	}
	noteSince, noteFound, err := p.Cursors.Get(ctx, p.noteKey())
	if err != nil {
		return nil, fmt.Errorf("userreport: scan: read note floor: %w", err)
	}
	if !noteFound || noteSince.After(since) {
		noteSince = since
	}
	page, err := p.Reader.ListUserReports(ctx, workmgmt.ListUserReportsRequest{Target: p.Target, Since: since, NoteSince: noteSince})
	if err != nil {
		return nil, fmt.Errorf("userreport: scan: list since %s: %w", since.UTC().Format(time.RFC3339Nano), err)
	}
	if page == nil {
		return nil, errors.New("userreport: scan: reader returned a nil page with no error")
	}

	// A provider that sets no NextNoteCursor HOLDS the floor (re-reads, never
	// skips); one beyond NextCursor is capped there.
	nextNote := page.NextNoteCursor
	if nextNote.IsZero() {
		nextNote = noteSince
	}
	if nextNote.After(page.NextCursor) {
		nextNote = page.NextCursor
	}
	report := Report{
		AccountID: p.AccountID, Repo: p.Repo, Source: p.Source, Forge: page.Forge,
		Since: since, NextCursor: page.NextCursor,
		NoteSince: noteSince, NextNoteCursor: nextNote,
	}
	for _, d := range page.Degradations {
		report.Degradations = append(report.Degradations, Degradation{
			Source: DegradationSourcePage, Code: string(d.Code), Detail: d.Detail, Count: d.Count,
		})
	}
	login, capDeg := resolveCaptain(ctx, p, page.Forge)
	if capDeg != nil {
		report.Degradations = append(report.Degradations, *capDeg)
	}
	cc := ClassifyContext{CaptainLogin: login}
	report.Items = make([]ReportItem, 0, len(page.Items))
	for _, it := range page.Items {
		r := Classify(it, cc)
		report.Items = append(report.Items, ReportItem{
			UserReportItem: it, Classification: r.Class, Basis: r.Basis, MarkerFromExternal: r.MarkerFromExternal,
		})
	}

	if err := p.Record.Record(ctx, report); err != nil {
		return nil, fmt.Errorf("userreport: scan: record report (cursor unmoved, the next scan re-reads): %w", err)
	}
	if _, _, err := p.Cursors.Advance(ctx, p.noteKey(), report.NextNoteCursor); err != nil {
		return nil, fmt.Errorf("%w: note floor: %w", ErrCursorNotAdvanced, err)
	}
	committed, advanced, err := p.Cursors.Advance(ctx, key, report.NextCursor)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrCursorNotAdvanced, err)
	}
	return &ScanResult{Report: report, Cursor: committed, Advanced: advanced}, nil
}

// resolveCaptain returns the captain's bare login on forge, or a
// captain_unavailable degradation. A vacant seat returns neither.
func resolveCaptain(ctx context.Context, p ScanParams, forge string) (string, *Degradation) {
	unavailable := func(detail string) (string, *Degradation) {
		return "", &Degradation{Source: DegradationSourceReport, Code: string(CaptainUnavailable), Detail: detail}
	}
	if p.Captain == nil {
		return unavailable("no captain reader was configured for this scan; the captain arm of the internal rule is off")
	}
	snap, err := p.Captain.Read(ctx, p.AccountID, p.Repo)
	if err != nil {
		// The raw error goes to the log, never into the report: a comms
		// renderer must not surface database error text.
		logger := p.Logger
		if logger == nil {
			logger = slog.Default()
		}
		logger.WarnContext(ctx, "userreport: captain read failed; captain arm off", "repo", p.Repo, "error", err)
		return unavailable("the captain record could not be read; the captain arm of the internal rule is off")
	}
	if snap == nil || snap.State.Current == nil {
		return "", nil // vacant: a normal state
	}
	cur := snap.State.Current
	if !cur.IdentityVerified {
		return unavailable("the captain subject is not identity-verified (not provider-qualified); it cannot be matched to a forge login")
	}
	// CaptainLoginFor re-checks the subject's provider qualification itself,
	// so a record flagged verified over a static subject also lands here.
	login := CaptainLoginFor(cur.Subject, forge)
	if login == "" {
		return unavailable(fmt.Sprintf("the captain subject is not qualified for this page's forge %q; it cannot be matched to a login here", forge))
	}
	return login, nil
}
