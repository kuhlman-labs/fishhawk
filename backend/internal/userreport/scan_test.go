package userreport

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/captain"
	"github.com/kuhlman-labs/fishhawk/backend/internal/workmgmt"
)

var (
	scanNow   = time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)
	scanSince = time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	scanNext  = time.Date(2026, 10, 4, 11, 58, 0, 0, time.UTC)
)

// memCursors is an in-memory CursorStore with the Store's semantics
// (insert-if-absent Init returning the stored value, monotonic Advance), plus
// per-method error injection and call counts.
type memCursors struct {
	rows                        map[Key]time.Time
	getErr, initErr, advanceErr error
	gets, inits, advances       int
	initArgs                    []time.Time
	// advanceErrFor fails Advance for one Source only.
	advanceErrFor map[Source]error
}

func newMemCursors() *memCursors { return &memCursors{rows: map[Key]time.Time{}} }

func (m *memCursors) k(key Key) Key { key.AccountID = nil; return key }

func (m *memCursors) Get(_ context.Context, key Key) (time.Time, bool, error) {
	m.gets++
	if m.getErr != nil {
		return time.Time{}, false, m.getErr
	}
	at, ok := m.rows[m.k(key)]
	return at, ok, nil
}

func (m *memCursors) Init(_ context.Context, key Key, initial time.Time) (time.Time, error) {
	m.inits++
	m.initArgs = append(m.initArgs, initial)
	if m.initErr != nil {
		return time.Time{}, m.initErr
	}
	if at, ok := m.rows[m.k(key)]; ok {
		return at, nil
	}
	m.rows[m.k(key)] = initial
	return initial, nil
}

func (m *memCursors) Advance(_ context.Context, key Key, to time.Time) (time.Time, bool, error) {
	m.advances++
	if m.advanceErr != nil {
		return time.Time{}, false, m.advanceErr
	}
	if err := m.advanceErrFor[key.Source]; err != nil {
		return time.Time{}, false, err
	}
	cur, ok := m.rows[m.k(key)]
	if ok && !cur.Before(to) {
		return cur, false, nil
	}
	m.rows[m.k(key)] = to
	return to, true, nil
}

// fakeReader returns a canned page (NextCursor fixed unless pageFn is set)
// and records every since it is asked for.
type fakeReader struct {
	page       *workmgmt.UserReportPage
	pageFn     func(since time.Time) *workmgmt.UserReportPage
	err        error
	sinces     []time.Time
	noteSinces []time.Time
}

func (r *fakeReader) ListUserReports(_ context.Context, req workmgmt.ListUserReportsRequest) (*workmgmt.UserReportPage, error) {
	r.sinces = append(r.sinces, req.Since)
	r.noteSinces = append(r.noteSinces, req.NoteSince)
	if r.err != nil {
		return nil, r.err
	}
	if r.pageFn != nil {
		return r.pageFn(req.Since), nil
	}
	return r.page, nil
}

type recorder struct {
	reports []Report
	err     error
}

func (r *recorder) Record(_ context.Context, rep Report) error {
	r.reports = append(r.reports, rep)
	return r.err
}

type fakeCaptain struct {
	snap *captain.Snapshot
	err  error
}

func (c fakeCaptain) Read(context.Context, *uuid.UUID, string) (*captain.Snapshot, error) {
	return c.snap, c.err
}

func seated(subject string, verified bool) fakeCaptain {
	return fakeCaptain{snap: &captain.Snapshot{State: captain.State{Current: &captain.Record{Subject: subject, IdentityVerified: verified}}}}
}

func githubPage(items ...workmgmt.UserReportItem) *workmgmt.UserReportPage {
	return &workmgmt.UserReportPage{Forge: workmgmt.UserReportForgeGitHub, Items: items, NextCursor: scanNext}
}

func outsiderItem(login string) workmgmt.UserReportItem {
	return workmgmt.UserReportItem{Kind: workmgmt.UserReportKindIssue, IssueNumber: 1, Author: author(login, "NONE", false, false), UpdatedAt: scanSince.Add(time.Hour)}
}

func baseParams(r workmgmt.UserReportReader, c CursorStore, rec Recorder) ScanParams {
	return ScanParams{
		Repo: "acme/widgets", Source: SourceIssues, Reader: r, Cursors: c, Record: rec,
		Captain: fakeCaptain{}, Now: func() time.Time { return scanNow },
	}
}

func degradationCodes(ds []Degradation) []string {
	out := make([]string, 0, len(ds))
	for _, d := range ds {
		out = append(out, string(d.Source)+":"+d.Code)
	}
	sort.Strings(out)
	return out
}

// TestScan_RecorderFailureDoesNotAdvanceAndNextScanRereads: a scan whose
// recorder fails leaves the cursor where it was, and the next scan passes the
// IDENTICAL since and gets the same items. Mechanism: calling Advance before
// Record would move the stored cursor on the failed scan, so scan 2's since
// would differ.
func TestScan_RecorderFailureDoesNotAdvanceAndNextScanRereads(t *testing.T) {
	cur := newMemCursors()
	cur.rows[Key{Repo: "acme/widgets", Source: SourceIssues}] = scanSince
	rd := &fakeReader{page: githubPage(outsiderItem("rando"))}
	rec := &recorder{err: errors.New("disk full")}
	if res, err := Scan(context.Background(), baseParams(rd, cur, rec)); err == nil || res != nil {
		t.Fatalf("scan 1 = (%+v, %v), want an error and nil result", res, err)
	}
	if got := cur.rows[Key{Repo: "acme/widgets", Source: SourceIssues}]; !got.Equal(scanSince) || cur.advances != 0 {
		t.Fatalf("after a failed record: cursor %v advances %d, want %v and 0", got, cur.advances, scanSince)
	}
	rec.err = nil
	res, err := Scan(context.Background(), baseParams(rd, cur, rec))
	if err != nil {
		t.Fatalf("scan 2: %v", err)
	}
	if len(rd.sinces) != 2 || !rd.sinces[1].Equal(rd.sinces[0]) {
		t.Errorf("sinces = %v, want scan 2 to re-read with the identical since", rd.sinces)
	}
	if len(rec.reports) != 2 || len(rec.reports[1].Items) != 1 {
		t.Errorf("recorded %d reports, want 2 with the same item re-read", len(rec.reports))
	}
	if !res.Advanced || !res.Cursor.Equal(scanNext) {
		t.Errorf("scan 2 result cursor %v advanced %v, want %v true", res.Cursor, res.Advanced, scanNext)
	}
}

// TestScan_FirstScanSincePersistedBeforeRead (approval condition 2): with no
// cursor row, the first scan persists Now − lookback BEFORE it reads; the read
// fails; the retry, with Now advanced by an hour, reads with the IDENTICAL
// since. Mechanism: reading with the computed value instead of Init's stored
// one, or computing it fresh on the retry, makes the second since an hour
// later.
func TestScan_FirstScanSincePersistedBeforeRead(t *testing.T) {
	cur := newMemCursors()
	rd := &fakeReader{err: errors.New("forge 502")}
	p := baseParams(rd, cur, &recorder{})
	p.InitialLookback = 48 * time.Hour
	if _, err := Scan(context.Background(), p); err == nil {
		t.Fatal("scan 1 succeeded, want the read error")
	}
	rd.err, rd.page = nil, githubPage()
	p.Now = func() time.Time { return scanNow.Add(time.Hour) }
	if _, err := Scan(context.Background(), p); err != nil {
		t.Fatalf("scan 2: %v", err)
	}
	want := scanNow.Add(-48 * time.Hour)
	if len(rd.sinces) != 2 || !rd.sinces[0].Equal(want) || !rd.sinces[1].Equal(want) {
		t.Errorf("sinces = %v, want both %v (persisted once, re-used on retry)", rd.sinces, want)
	}
	if cur.inits != 1 {
		t.Errorf("Init called %d times, want once (the retry finds the stored row)", cur.inits)
	}
}

// TestScan_FirstScanReadsWithStoredNotComputedValue: Init returning an
// already-stored value (a concurrent first scan won) is what the read uses.
func TestScan_FirstScanReadsWithStoredNotComputedValue(t *testing.T) {
	cur := newMemCursors()
	stored := scanSince
	rd := &fakeReader{page: githubPage()}
	p := baseParams(rd, &initWinsStore{memCursors: cur, stored: stored}, &recorder{})
	if _, err := Scan(context.Background(), p); err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if !rd.sinces[0].Equal(stored) {
		t.Errorf("since = %v, want Init's stored %v", rd.sinces[0], stored)
	}
}

// initWinsStore reports no row on Get but returns a different stored value
// from Init, as a lost insert-if-absent race does.
type initWinsStore struct {
	*memCursors
	stored time.Time
}

func (s *initWinsStore) Init(_ context.Context, _ Key, _ time.Time) (time.Time, error) {
	return s.stored, nil
}

// TestScan_FirstScanUsesInitialLookback: the default and an explicit lookback.
func TestScan_FirstScanUsesInitialLookback(t *testing.T) {
	for name, tc := range map[string]struct {
		lookback time.Duration
		want     time.Time
	}{
		"default":  {0, scanNow.Add(-DefaultInitialLookback)},
		"explicit": {time.Hour, scanNow.Add(-time.Hour)},
	} {
		t.Run(name, func(t *testing.T) {
			cur := newMemCursors()
			rd := &fakeReader{page: githubPage()}
			p := baseParams(rd, cur, &recorder{})
			p.InitialLookback = tc.lookback
			if _, err := Scan(context.Background(), p); err != nil {
				t.Fatalf("Scan: %v", err)
			}
			if !rd.sinces[0].Equal(tc.want) || !cur.initArgs[0].Equal(tc.want) {
				t.Errorf("since %v init %v, want %v", rd.sinces[0], cur.initArgs[0], tc.want)
			}
		})
	}
	// A nil Now falls back to the wall clock: the stored since lies in the
	// past by about the lookback.
	cur := newMemCursors()
	p := baseParams(&fakeReader{page: githubPage()}, cur, &recorder{})
	p.Now, p.InitialLookback = nil, time.Hour
	if _, err := Scan(context.Background(), p); err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if age := time.Since(cur.initArgs[0]); age < time.Hour || age > 2*time.Hour {
		t.Errorf("wall-clock initial since is %v old, want about an hour", age)
	}
}

// TestScan_ReaderFailureDoesNotAdvance covers a transient error and the
// unresumable equal-timestamp-run error: both leave the cursor, record
// nothing, and the unresumable one stays errors.Is-distinguishable.
func TestScan_ReaderFailureDoesNotAdvance(t *testing.T) {
	transient := errors.New("connection reset")
	unresumable := fmt.Errorf("workmgmt/github: list issues: %w", workmgmt.ErrUserReportUnresumable)
	for name, readErr := range map[string]error{"transient": transient, "unresumable": unresumable} {
		t.Run(name, func(t *testing.T) {
			cur := newMemCursors()
			cur.rows[Key{Repo: "acme/widgets", Source: SourceIssues}] = scanSince
			rec := &recorder{}
			_, err := Scan(context.Background(), baseParams(&fakeReader{err: readErr}, cur, rec))
			if !errors.Is(err, readErr) {
				t.Fatalf("err = %v, want it to wrap %v", err, readErr)
			}
			if got := errors.Is(err, workmgmt.ErrUserReportUnresumable); got != (name == "unresumable") {
				t.Errorf("errors.Is(err, ErrUserReportUnresumable) = %v for the %s error", got, name)
			}
			if cur.advances != 0 || len(rec.reports) != 0 || !cur.rows[Key{Repo: "acme/widgets", Source: SourceIssues}].Equal(scanSince) {
				t.Errorf("advances %d reports %d cursor %v, want 0, 0, unchanged", cur.advances, len(rec.reports), cur.rows)
			}
		})
	}
}

// TestScan_CursorFailuresStopBeforeRead: a Get or Init failure returns before
// any listing.
func TestScan_CursorFailuresStopBeforeRead(t *testing.T) {
	for name, set := range map[string]func(*memCursors){
		"get":  func(m *memCursors) { m.getErr = errors.New("db down") },
		"init": func(m *memCursors) { m.initErr = errors.New("db down") },
	} {
		t.Run(name, func(t *testing.T) {
			cur := newMemCursors()
			set(cur)
			rd := &fakeReader{page: githubPage()}
			if _, err := Scan(context.Background(), baseParams(rd, cur, &recorder{})); err == nil || !strings.Contains(err.Error(), "db down") {
				t.Fatalf("err = %v, want the cursor error", err)
			}
			if len(rd.sinces) != 0 {
				t.Errorf("reader called %d times, want 0", len(rd.sinces))
			}
		})
	}
}

// TestScan_AdvanceFailureAfterRecordIsReturned: the report was recorded, the
// advance failed, and Scan says so with ErrCursorNotAdvanced.
func TestScan_AdvanceFailureAfterRecordIsReturned(t *testing.T) {
	cur := newMemCursors()
	cur.advanceErr = errors.New("serialization failure")
	rec := &recorder{}
	res, err := Scan(context.Background(), baseParams(&fakeReader{page: githubPage()}, cur, rec))
	if !errors.Is(err, ErrCursorNotAdvanced) || !errors.Is(err, cur.advanceErr) || res != nil {
		t.Fatalf("= (%+v, %v), want ErrCursorNotAdvanced wrapping the advance error", res, err)
	}
	if len(rec.reports) != 1 {
		t.Errorf("recorded %d reports, want 1 (Record precedes Advance)", len(rec.reports))
	}
}

// TestScan_InvalidParamsRefusedBeforeAnyIO: every refused shape touches
// neither the store nor the reader. A nil recorder is the load-bearing one: a
// scan with nowhere to record must never advance.
func TestScan_InvalidParamsRefusedBeforeAnyIO(t *testing.T) {
	cases := map[string]func(*ScanParams){
		"nil recorder":      func(p *ScanParams) { p.Record = nil },
		"nil reader":        func(p *ScanParams) { p.Reader = nil },
		"nil cursors":       func(p *ScanParams) { p.Cursors = nil },
		"negative lookback": func(p *ScanParams) { p.InitialLookback = -time.Hour },
		"empty repo":        func(p *ScanParams) { p.Repo = "" },
		"unknown source":    func(p *ScanParams) { p.Source = "discussions" },
		"note-floor source": func(p *ScanParams) { p.Source = SourceIssueNotes },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			cur := newMemCursors()
			rd := &fakeReader{page: githubPage()}
			p := baseParams(rd, cur, &recorder{})
			mutate(&p)
			_, err := Scan(context.Background(), p)
			if !errors.Is(err, ErrInvalidParams) && !errors.Is(err, ErrInvalidKey) {
				t.Fatalf("err = %v, want ErrInvalidParams or ErrInvalidKey", err)
			}
			if cur.gets+cur.inits+cur.advances != 0 || len(rd.sinces) != 0 {
				t.Errorf("store calls %d reader calls %d, want none", cur.gets+cur.inits+cur.advances, len(rd.sinces))
			}
		})
	}
}

// TestScan_NilPageWithoutErrorIsRefused: a reader breaking its contract does
// not advance the cursor.
func TestScan_NilPageWithoutErrorIsRefused(t *testing.T) {
	cur := newMemCursors()
	if _, err := Scan(context.Background(), baseParams(&fakeReader{}, cur, &recorder{})); err == nil {
		t.Fatal("Scan succeeded on a nil page")
	}
	if cur.advances != 0 {
		t.Errorf("advances = %d, want 0", cur.advances)
	}
}

// TestScan_AnchorUnavailableLeavesCursorUnchanged: a page whose NextCursor
// holds at Since (cursor_anchor_unavailable) records, carries the page code
// with Source page, and leaves the cursor where it was.
func TestScan_AnchorUnavailableLeavesCursorUnchanged(t *testing.T) {
	cur := newMemCursors()
	cur.rows[Key{Repo: "acme/widgets", Source: SourceIssues}] = scanSince
	rd := &fakeReader{pageFn: func(since time.Time) *workmgmt.UserReportPage {
		return &workmgmt.UserReportPage{Forge: workmgmt.UserReportForgeGitHub, Since: since, NextCursor: since,
			Degradations: []workmgmt.UserReportDegradation{{Code: workmgmt.UserReportCursorAnchorUnavailable, Detail: "no Date"}}}
	}}
	rec := &recorder{}
	res, err := Scan(context.Background(), baseParams(rd, cur, rec))
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if res.Advanced || !res.Cursor.Equal(scanSince) {
		t.Errorf("cursor %v advanced %v, want held at %v", res.Cursor, res.Advanced, scanSince)
	}
	if got := degradationCodes(rec.reports[0].Degradations); fmt.Sprint(got) != "[page:cursor_anchor_unavailable]" {
		t.Errorf("degradations = %v, want the page code with Source page", got)
	}
}

// TestScan_CaptainResolution covers every captain state (approval condition
// 5): a vacant seat is normal (no code, no arm); a read error, an unverified
// subject, a forge mismatch and a missing reader each name
// captain_unavailable with Source report and turn the arm off; a verified
// same-forge captain makes their NONE-association item internal.
func TestScan_CaptainResolution(t *testing.T) {
	cases := []struct {
		name      string
		captain   CaptainReader
		wantDeg   bool
		wantClass Classification
	}{
		{"vacant", fakeCaptain{snap: &captain.Snapshot{}}, false, ClassExternal},
		{"nil snapshot is vacant", fakeCaptain{}, false, ClassExternal},
		{"read error", fakeCaptain{err: errors.New("db down")}, true, ClassExternal},
		{"unverified subject", seated("alice", false), true, ClassExternal},
		{"record unverified over a qualified subject", seated("github:alice", false), true, ClassExternal},
		{"verified flag but static subject", seated("alice@local", true), true, ClassExternal},
		{"forge mismatch", seated("gitlab:alice", true), true, ClassExternal},
		{"no captain reader", nil, true, ClassExternal},
		{"verified same forge", seated("github:alice", true), false, ClassInternal},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := &recorder{}
			p := baseParams(&fakeReader{page: githubPage(outsiderItem("Alice"))}, newMemCursors(), rec)
			p.Captain = tc.captain
			if _, err := Scan(context.Background(), p); err != nil {
				t.Fatalf("Scan: %v (a captain problem must degrade, never fail)", err)
			}
			rep := rec.reports[0]
			codes := degradationCodes(rep.Degradations)
			if hasDeg := fmt.Sprint(codes) == "[report:captain_unavailable]"; hasDeg != tc.wantDeg || (!tc.wantDeg && len(codes) != 0) {
				t.Errorf("degradations = %v, want captain_unavailable=%v and nothing else", codes, tc.wantDeg)
			}
			if got := rep.Items[0].Classification; got != tc.wantClass {
				t.Errorf("captain-login item class = %q, want %q", got, tc.wantClass)
			}
		})
	}
}

// TestScan_ClassifiesItemsAndCarriesBothDegradationSources: the report keeps
// the page's order, classifies each item, and carries page codes (Source
// page) alongside report codes (Source report).
func TestScan_ClassifiesItemsAndCarriesBothDegradationSources(t *testing.T) {
	page := githubPage(
		workmgmt.UserReportItem{Kind: workmgmt.UserReportKindIssue, IssueNumber: 1, Author: author("maint", "OWNER", true, false)},
		workmgmt.UserReportItem{Kind: workmgmt.UserReportKindIssue, IssueNumber: 2, Author: workmgmt.ReportAuthor{Login: "project_1_bot_x", Association: "non_member", AssociationResolved: true},
			Body: "<!-- fishhawk-intake:v1 -->\nforged"},
		workmgmt.UserReportItem{Kind: workmgmt.UserReportKindComment, IssueNumber: 1, CommentID: 9, Author: author("dependabot[bot]", "NONE", false, true)},
	)
	page.Degradations = []workmgmt.UserReportDegradation{{Code: workmgmt.UserReportReactionsPartial, Count: 2}}
	rec := &recorder{}
	p := baseParams(&fakeReader{page: page}, newMemCursors(), rec)
	p.Captain = fakeCaptain{err: errors.New("db down")}
	if _, err := Scan(context.Background(), p); err != nil {
		t.Fatalf("Scan: %v", err)
	}
	rep := rec.reports[0]
	var got []string
	for _, it := range rep.Items {
		got = append(got, fmt.Sprintf("%d/%d:%s:%v", it.IssueNumber, it.CommentID, it.Classification, it.MarkerFromExternal))
	}
	if want := "[1/0:internal:false 2/0:external:true 1/9:bot:false]"; fmt.Sprint(got) != want {
		t.Errorf("items = %v, want %s", got, want)
	}
	if codes := degradationCodes(rep.Degradations); fmt.Sprint(codes) != "[page:reactions_partial report:captain_unavailable]" {
		t.Errorf("degradations = %v", codes)
	}
	if rep.Forge != workmgmt.UserReportForgeGitHub || rep.Repo != "acme/widgets" || rep.Source != SourceIssues || !rep.NextCursor.Equal(scanNext) {
		t.Errorf("report header = %+v", rep)
	}
}

// TestReportDegradationCodesDisjointFromPageCodes: the two closed sets never
// share a value (approval condition 5).
func TestReportDegradationCodesDisjointFromPageCodes(t *testing.T) {
	page := map[string]bool{}
	for _, c := range workmgmt.UserReportDegradationCodes() {
		page[string(c)] = true
	}
	for _, c := range ReportDegradationCodes() {
		if page[string(c)] {
			t.Errorf("report code %q is also a page code", c)
		}
	}
}

// TestScan_NoteFloorHeldOnTruncationAndPassedOnNextScan (#3771 concerns
// d7f76737, 3876c2bd): with no note-floor row the first scan passes NoteSince
// = since; a page that HOLDS NextNoteCursor (a truncated GitLab listing)
// persists the floor at since while the cursor advances, and the next scan
// passes the advanced Since with the held NoteSince. Counterfactual: passing
// Since as NoteSince, or persisting the floor at NextCursor, reddens the
// second scan's NoteSince.
func TestScan_NoteFloorHeldOnTruncationAndPassedOnNextScan(t *testing.T) {
	issues := Key{Repo: "acme/widgets", Source: SourceIssues}
	floor := Key{Repo: "acme/widgets", Source: SourceIssueNotes}
	cur := newMemCursors()
	cur.rows[issues] = scanSince
	rd := &fakeReader{pageFn: func(since time.Time) *workmgmt.UserReportPage {
		return &workmgmt.UserReportPage{Forge: workmgmt.UserReportForgeGitLab, Since: since, NextCursor: since.Add(time.Hour), NextNoteCursor: scanSince}
	}}
	rec := &recorder{}
	for scan := 1; scan <= 2; scan++ {
		if _, err := Scan(context.Background(), baseParams(rd, cur, rec)); err != nil {
			t.Fatalf("scan %d: %v", scan, err)
		}
	}
	if !rd.noteSinces[0].Equal(scanSince) || !rd.sinces[1].Equal(scanSince.Add(time.Hour)) || !rd.noteSinces[1].Equal(scanSince) {
		t.Errorf("sinces %v noteSinces %v, want scan 1 (%v, %v) and scan 2 (%v, held %v)", rd.sinces, rd.noteSinces, scanSince, scanSince, scanSince.Add(time.Hour), scanSince)
	}
	if got := cur.rows[floor]; !got.Equal(scanSince) {
		t.Errorf("persisted note floor = %v, want held at %v", got, scanSince)
	}
	if r := rec.reports[1]; !r.NoteSince.Equal(scanSince) || !r.NextNoteCursor.Equal(scanSince) {
		t.Errorf("report 2 note pair = (%v, %v), want both %v", r.NoteSince, r.NextNoteCursor, scanSince)
	}
}

// TestScan_NoteFloorAdvancesBeforeCursor: the floor is durable before the
// cursor moves past it — an absent floor row reads as the cursor, so a failed
// floor write must leave the cursor unmoved. Counterfactual: advancing the
// cursor first moves it on this failure.
func TestScan_NoteFloorAdvancesBeforeCursor(t *testing.T) {
	issues := Key{Repo: "acme/widgets", Source: SourceIssues}
	cur := newMemCursors()
	cur.rows[issues] = scanSince
	floorErr := errors.New("floor write failed")
	cur.advanceErrFor = map[Source]error{SourceIssueNotes: floorErr}
	res, err := Scan(context.Background(), baseParams(&fakeReader{page: githubPage()}, cur, &recorder{}))
	if !errors.Is(err, ErrCursorNotAdvanced) || !errors.Is(err, floorErr) || res != nil {
		t.Fatalf("= (%+v, %v), want ErrCursorNotAdvanced wrapping the floor error", res, err)
	}
	if got := cur.rows[issues]; !got.Equal(scanSince) {
		t.Errorf("cursor = %v after a failed floor write, want unmoved %v", got, scanSince)
	}
}

// TestScan_NoteFloorClampedAndDefaulted: a stored floor LATER than the cursor
// (a floor written before a failed cursor advance) is clamped to the cursor;
// a page with no NextNoteCursor holds the floor; one beyond NextCursor is
// capped at NextCursor.
func TestScan_NoteFloorClampedAndDefaulted(t *testing.T) {
	issues := Key{Repo: "acme/widgets", Source: SourceIssues}
	floor := Key{Repo: "acme/widgets", Source: SourceIssueNotes}
	t.Run("stored floor later than the cursor reads as the cursor", func(t *testing.T) {
		cur := newMemCursors()
		cur.rows[issues], cur.rows[floor] = scanSince, scanNext
		rd := &fakeReader{page: githubPage()}
		if _, err := Scan(context.Background(), baseParams(rd, cur, &recorder{})); err != nil {
			t.Fatal(err)
		}
		if !rd.noteSinces[0].Equal(scanSince) {
			t.Errorf("NoteSince = %v, want clamped to the cursor %v", rd.noteSinces[0], scanSince)
		}
	})
	t.Run("zero NextNoteCursor holds", func(t *testing.T) {
		cur := newMemCursors()
		cur.rows[issues] = scanSince
		rec := &recorder{}
		if _, err := Scan(context.Background(), baseParams(&fakeReader{page: githubPage()}, cur, rec)); err != nil {
			t.Fatal(err)
		}
		if got := cur.rows[floor]; !got.Equal(scanSince) || !rec.reports[0].NextNoteCursor.Equal(scanSince) {
			t.Errorf("floor %v report %v, want held at %v", got, rec.reports[0].NextNoteCursor, scanSince)
		}
	})
	t.Run("NextNoteCursor beyond NextCursor is capped", func(t *testing.T) {
		cur := newMemCursors()
		cur.rows[issues] = scanSince
		page := githubPage()
		page.NextNoteCursor = scanNow
		if _, err := Scan(context.Background(), baseParams(&fakeReader{page: page}, cur, &recorder{})); err != nil {
			t.Fatal(err)
		}
		if got := cur.rows[floor]; !got.Equal(scanNext) {
			t.Errorf("floor = %v, want capped at NextCursor %v", got, scanNext)
		}
	})
}

// TestScan_CaptainReadErrorDetailIsFixed (#3771 concern 40f9240d): the raw
// captain read error goes to the logger, never into the report's Detail a
// comms renderer may surface. Counterfactual: embedding err.Error() in the
// Detail reddens the first assertion.
func TestScan_CaptainReadErrorDetailIsFixed(t *testing.T) {
	var logs strings.Builder
	rec := &recorder{}
	p := baseParams(&fakeReader{page: githubPage()}, newMemCursors(), rec)
	p.Captain = fakeCaptain{err: errors.New("pq: password authentication failed for user secret_db_user")}
	p.Logger = slog.New(slog.NewTextHandler(&logs, nil))
	if _, err := Scan(context.Background(), p); err != nil {
		t.Fatalf("Scan: %v", err)
	}
	d := rec.reports[0].Degradations
	if len(d) != 1 || d[0].Code != string(CaptainUnavailable) || strings.Contains(d[0].Detail, "secret_db_user") {
		t.Errorf("degradations = %+v, want one captain_unavailable whose Detail omits the raw error", d)
	}
	if !strings.Contains(logs.String(), "secret_db_user") {
		t.Errorf("log = %q, want the raw captain error logged", logs.String())
	}
}
