package server

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/githubclient"
	"github.com/kuhlman-labs/fishhawk/backend/internal/prompt"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
	"github.com/kuhlman-labs/fishhawk/backend/internal/userreport"
	"github.com/kuhlman-labs/fishhawk/backend/internal/workmgmt"
	workmgmtgithub "github.com/kuhlman-labs/fishhawk/backend/internal/workmgmt/github"
)

// Tests in this file mutate package vars (conventionsLoader,
// commsUserReportReaderFor, commsScan*) and are therefore NON-parallel.

var csBase = time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)

// csCharterDoc carries two conforming rubric rows and two non-goal bullets.
const csCharterDoc = `# Charter

## 3. Non-goals

- **N1 — Fishhawk is not a hosted service.** It runs on your infrastructure.
- **N2 — Fishhawk is not a CI/CD platform.** It drives changes, not builds.

## 4. Rubric

| id | line |
|---|---|
| **V1** | Directly unblocks a user. |
| **R2** | Reduces operator toil. |
`

// csCharterUnconforming parses rubric ids, but none the prompt renders.
const csCharterUnconforming = `# Charter

| **vv2** | Lowercase id. |
| **N9** | A non-goal id in the rubric table. |
`

// csCharterMixed carries one conforming and two unconforming rubric ids.
const csCharterMixed = `# Charter

- **N1 — Not a hosted service.** Prose.

| **V1** | Directly unblocks a user. |
| **vv2** | Lowercase id. |
| **N9** | A non-goal id in the rubric table. |
`

// csReader is a fake workmgmt.UserReportReader implementing the DOCUMENTED
// inclusive contract: it returns every item whose updated_at is AT OR AFTER
// req.Since, and a NextCursor later than every item.
type csReader struct {
	mu        sync.Mutex
	items     []workmgmt.UserReportItem
	next      time.Time
	degr      []workmgmt.UserReportDegradation
	err       error
	block     bool
	panicking bool
	calls     int
}

func (r *csReader) ListUserReports(ctx context.Context, req workmgmt.ListUserReportsRequest) (*workmgmt.UserReportPage, error) {
	r.mu.Lock()
	r.calls++
	items, next, degr, err, block, panicking := r.items, r.next, r.degr, r.err, r.block, r.panicking
	r.mu.Unlock()
	if panicking {
		panic("comms reader exploded")
	}
	if block {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	if err != nil {
		return nil, err
	}
	var out []workmgmt.UserReportItem
	for _, it := range items {
		if !it.UpdatedAt.Before(req.Since) {
			out = append(out, it)
		}
	}
	if next.Before(req.Since) {
		next = req.Since
	}
	return &workmgmt.UserReportPage{Forge: "github", Items: out, Since: req.Since, NextCursor: next, NextNoteCursor: next, Degradations: degr}, nil
}

func (r *csReader) callCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.calls
}

// csCursors is an in-memory userreport.CursorStore with the real store's
// monotonic Advance and insert-if-absent Init.
type csCursors struct {
	mu       sync.Mutex
	vals     map[string]time.Time
	inits    int
	advances int
}

func csCursorKey(k userreport.Key) string {
	acct := ""
	if k.AccountID != nil {
		acct = k.AccountID.String()
	}
	return k.Repo + "|" + string(k.Source) + "|" + acct
}

func (c *csCursors) Get(_ context.Context, k userreport.Key) (time.Time, bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	v, ok := c.vals[csCursorKey(k)]
	return v, ok, nil
}

func (c *csCursors) Init(_ context.Context, k userreport.Key, initial time.Time) (time.Time, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.inits++
	if v, ok := c.vals[csCursorKey(k)]; ok {
		return v, nil
	}
	c.vals[csCursorKey(k)] = initial
	return initial, nil
}

func (c *csCursors) Advance(_ context.Context, k userreport.Key, to time.Time) (time.Time, bool, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.advances++
	cur, ok := c.vals[csCursorKey(k)]
	if ok && !cur.Before(to) {
		return cur, false, nil
	}
	c.vals[csCursorKey(k)] = to
	return to, true, nil
}

func csKey(src userreport.Source) userreport.Key {
	return userreport.Key{Repo: commsTestRepo, Source: src}
}

type csFixture struct {
	s       *Server
	rr      *upkeepRunRepo
	au      *auditFake
	cursors *csCursors
	reader  *csReader
	fetcher *igFetcher
	runRow  *run.Run
	plan    *run.Stage
	logs    *bytes.Buffer
	now     time.Time
}

// newCSFixture seeds an untenanted run of the SHIPPED user-report-scan spec,
// a charter, a reader and a cursor store whose issues cursor sits one hour
// before csBase. cfg, when non-nil, edits the Config before New.
func newCSFixture(t *testing.T, cfg func(*Config)) *csFixture {
	t.Helper()
	wf, err := os.ReadFile("../../../docs/spec/examples/workflow-v2-user-report-scan.yaml")
	if err != nil {
		t.Fatalf("read user-report-scan example: %v", err)
	}
	conv := workmgmt.Default()
	conv.Provider = workmgmtgithub.ProviderName
	conv.Charter = &workmgmt.Charter{Path: igCharterPath}
	installConventions(t, conv, nil)

	f := &csFixture{
		rr: newUpkeepRunRepo(), au: newAuditFake(),
		cursors: &csCursors{vals: map[string]time.Time{csCursorKey(csKey(userreport.SourceIssues)): csBase.Add(-time.Hour)}},
		reader:  &csReader{next: csBase.Add(5 * time.Hour)},
		fetcher: &igFetcher{content: csCharterDoc},
		logs:    &bytes.Buffer{},
		now:     csBase,
	}
	inst := int64(42)
	f.runRow = &run.Run{ID: uuid.New(), Repo: commsTestRepo, WorkflowID: "user_report_scan", WorkflowSpec: wf,
		State: run.StateRunning, InstallationID: &inst}
	f.rr.seedRun(f.runRow)
	f.plan = &run.Stage{ID: uuid.New(), RunID: f.runRow.ID, Sequence: 0, Type: run.StageTypePlan, State: run.StageStateRunning}
	f.rr.getStages[f.plan.ID] = f.plan
	f.rr.stagesByRunID = map[uuid.UUID][]*run.Stage{f.runRow.ID: {f.plan}}

	prevReader, prevNow := commsUserReportReaderFor, commsScanNow
	commsUserReportReaderFor = func(string) (workmgmt.UserReportReader, error) { return f.reader, nil }
	commsScanNow = func() time.Time { return f.now }
	t.Cleanup(func() { commsUserReportReaderFor, commsScanNow = prevReader, prevNow })

	c := igCharterConfigWith(f.fetcher)
	c.Addr = "127.0.0.1:0"
	c.RunRepo = f.rr
	c.AuditRepo = f.au
	c.UserReportCursors = f.cursors
	c.Logger = slog.New(slog.NewJSONHandler(f.logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	if cfg != nil {
		cfg(&c)
	}
	f.s = New(c)
	return f
}

func (f *csFixture) gather(t *testing.T) (*prompt.CommsScanContext, *commsGather) {
	t.Helper()
	cc, g, err := f.s.resolveCommsScanContext(context.Background(), f.runRow, f.plan)
	if err != nil {
		t.Fatalf("resolveCommsScanContext: %v", err)
	}
	if cc == nil || g == nil {
		t.Fatal("resolveCommsScanContext returned no context for a comms_report stage")
	}
	return cc, g
}

// expire moves the cache clock past the TTL so the next gather is fresh.
func (f *csFixture) expire() { f.now = f.now.Add(2 * commsScanCacheTTL) }

// applyPhase7 simulates the comms apply advancing the REAL store.
func (f *csFixture) applyPhase7(t *testing.T, cursor, note time.Time) {
	t.Helper()
	ctx := context.Background()
	if _, _, err := f.cursors.Advance(ctx, csKey(userreport.SourceIssueNotes), note); err != nil {
		t.Fatal(err)
	}
	if _, _, err := f.cursors.Advance(ctx, csKey(userreport.SourceIssues), cursor); err != nil {
		t.Fatal(err)
	}
}

func (f *csFixture) seedAudit(e *audit.Entry) {
	f.au.mu.Lock()
	f.au.seeded = append(f.au.seeded, e)
	f.au.mu.Unlock()
}

func csIssue(n int, title, body string, at time.Time, author workmgmt.ReportAuthor) workmgmt.UserReportItem {
	return workmgmt.UserReportItem{Kind: workmgmt.UserReportKindIssue, IssueNumber: n, Title: title, Body: body,
		Author: author, CreatedAt: at, UpdatedAt: at}
}

func csComment(n int, id int64, body string, at time.Time, author workmgmt.ReportAuthor) workmgmt.UserReportItem {
	return workmgmt.UserReportItem{Kind: workmgmt.UserReportKindComment, IssueNumber: n, CommentID: id, Body: body,
		Author: author, CreatedAt: at, UpdatedAt: at}
}

func csShownIDs(g *commsGather) []string {
	var ids []string
	for _, r := range g.Payload.Shown {
		ids = append(ids, r.ID)
	}
	return ids
}

func csHas(ids []string, id string) bool {
	for _, v := range ids {
		if v == id {
			return true
		}
	}
	return false
}

func csBuild(t *testing.T, cc *prompt.CommsScanContext) string {
	t.Helper()
	out, err := prompt.Build("plan", prompt.Trigger{Repo: commsTestRepo, PlanStageTimeout: 20 * time.Minute, Comms: cc})
	if err != nil {
		t.Fatalf("prompt.Build: %v", err)
	}
	return out
}

// csSection returns the lines from heading to the next column-0 "### ".
func csSection(t *testing.T, text, heading string) []string {
	t.Helper()
	i := strings.Index(text, "\n"+heading+"\n")
	if i < 0 {
		t.Fatalf("prompt has no %q section", heading)
	}
	rest := text[i+len(heading)+2:]
	if j := strings.Index(rest, "\n### "); j >= 0 {
		rest = rest[:j]
	}
	return strings.Split(rest, "\n")
}

// csListedIDs returns the "- <id>" / "- <id>: text" list items of a section
// whose id matches accept.
func csListedIDs(lines []string, accept func(string) bool) []string {
	var ids []string
	for _, l := range lines {
		rest, ok := strings.CutPrefix(l, "- ")
		if !ok {
			continue
		}
		id, _, _ := strings.Cut(rest, ":")
		if accept(id) {
			ids = append(ids, id)
		}
	}
	return ids
}

func csIsReportID(id string) bool { return strings.HasPrefix(id, "UR-") && !strings.Contains(id, " ") }

func csEqual(a, b []string) bool {
	return fmt.Sprint(a) == fmt.Sprint(b)
}

// --- binding -----------------------------------------------------------------

func TestResolveCommsScanContext_NonDeclaringStages(t *testing.T) {
	f := newCSFixture(t, nil)
	ctx := context.Background()

	impl := &run.Stage{ID: uuid.New(), RunID: f.runRow.ID, Sequence: 1, Type: run.StageTypeImplement}
	if cc, g, err := f.s.resolveCommsScanContext(ctx, f.runRow, impl); cc != nil || g != nil || err != nil {
		t.Fatalf("implement stage = (%v, %v, %v), want all nil", cc, g, err)
	}

	f.runRow.WorkflowSpec = upkeepScanSpec(t)
	f.runRow.WorkflowID = "upkeep_scan"
	if cc, g, err := f.s.resolveCommsScanContext(ctx, f.runRow, f.plan); cc != nil || g != nil || err != nil {
		t.Fatalf("plan stage of a non-comms workflow = (%v, %v, %v), want all nil", cc, g, err)
	}
	if f.reader.callCount() != 0 {
		t.Fatalf("reader called %d times for a non-declaring stage", f.reader.callCount())
	}

	f.rr.getRunErrs[f.runRow.ID] = errors.New("db down")
	if _, _, err := f.s.resolveCommsScanContext(ctx, f.runRow, f.plan); err == nil || !strings.Contains(err.Error(), "db down") {
		t.Fatalf("binding transport error = %v, want the GetRun error verbatim", err)
	}
}

// --- happy path, class partition (C6), deferred cursor (C7) -------------------

func TestCommsScan_HappyPathPartitionAndFacts(t *testing.T) {
	f := newCSFixture(t, nil)
	f.reader.items = []workmgmt.UserReportItem{
		csIssue(10, "Export to CSV fails silently", "Nothing happens.", csBase.Add(1*time.Minute), commsExternalAuthor),
		csIssue(11, "Maintainer note", "Internal.", csBase.Add(2*time.Minute), commsInternalAuthor),
		csIssue(12, "Bump deps", "bot", csBase.Add(3*time.Minute), workmgmt.ReportAuthor{Login: "dependabot[bot]", Bot: true}),
		csIssue(13, "Filed draft", userreport.DraftMarker(nil)+"<!-- fishhawk-comms:v1 {\"reports\":[]} -->", csBase.Add(4*time.Minute), commsInternalAuthor),
		csComment(10, 7, "\n\nSame on Firefox.\nmore", csBase.Add(5*time.Minute), commsExternalAuthor),
	}
	f.reader.degr = []workmgmt.UserReportDegradation{{Code: workmgmt.UserReportReactionsPartial, Count: 2}}
	cc, g := f.gather(t)

	if got, want := csShownIDs(g), []string{"UR-comment-10-7", "UR-issue-10"}; !csEqual(got, want) {
		t.Fatalf("shown = %v, want newest-first %v", got, want)
	}
	if len(cc.UserReports) != 2 || cc.UserReports[0].CommentID != 7 || cc.UserReports[0].Classification != "external" {
		t.Fatalf("context reports = %+v", cc.UserReports)
	}
	// C6: internal, bot and fishhawk_filed items are absent and counted.
	want := commsClassExcluded{FishhawkFiled: 1, Bot: 1, Internal: 1}
	if g.Payload.ClassExcluded != want {
		t.Fatalf("class_excluded = %+v, want %+v", g.Payload.ClassExcluded, want)
	}
	pc := g.Payload.PendingCursor
	if pc == nil || !pc.Since.Equal(csBase.Add(-time.Hour)) || !pc.Cursor.Equal(csBase.Add(5*time.Hour)) || !pc.NoteCursor.Equal(pc.Cursor) {
		t.Fatalf("pending cursor = %+v, want since %v and cursor = NextCursor", pc, csBase.Add(-time.Hour))
	}
	if c := g.Payload.Charter; c.Path != igCharterPath || !csEqual(c.RubricIDs, []string{"V1", "R2"}) || !csEqual(c.NonGoalIDs, []string{"N1", "N2"}) || c.ContentHash == "" {
		t.Fatalf("charter record = %+v", c)
	}
	if len(cc.Rubric) != 2 || cc.Rubric[0].Text != "Directly unblocks a user." || len(cc.NonGoals) != 2 || cc.NonGoals[0].Text == "" {
		t.Fatalf("context charter = %+v / %+v", cc.Rubric, cc.NonGoals)
	}
	wantDeg := []commsGatherDegradation{
		{Source: commsDegradeSourcePage, Reason: "reactions_partial", Count: 2},
		{Source: commsDegradeSourceClassify, Reason: "captain_unavailable", Count: 1},
	}
	if fmt.Sprint(g.Payload.Degradations) != fmt.Sprint(wantDeg) || len(cc.Degradations) != 2 {
		t.Fatalf("degradations = %+v / %+v, want %+v", g.Payload.Degradations, cc.Degradations, wantDeg)
	}
	if g.Payload.Shown[1].ContentHash != userreport.ContentHash(workmgmt.UserReportKindIssue, "Export to CSV fails silently", "Nothing happens.") {
		t.Fatal("shown content hash is not userreport.ContentHash of the gathered report")
	}
	out := csBuild(t, cc)
	if got := csListedIDs(csSection(t, out, "### Shown report ids (account for every one)"), csIsReportID); !csEqual(got, csShownIDs(g)) {
		t.Fatalf("prompt shown ids %v != payload shown %v", got, csShownIDs(g))
	}
}

// TestCommsScan_ClassFilterExcludesInternal (C6) isolates the filter: the
// only item besides the external one is an INTERNAL report.
func TestCommsScan_ClassFilterExcludesInternal(t *testing.T) {
	f := newCSFixture(t, nil)
	f.reader.items = []workmgmt.UserReportItem{
		csIssue(1, "Crash on save", "x", csBase.Add(time.Minute), commsExternalAuthor),
		csIssue(2, "Crash when saving", "y", csBase.Add(2*time.Minute), commsInternalAuthor),
	}
	_, g := f.gather(t)
	if csHas(csShownIDs(g), "UR-issue-2") {
		t.Fatalf("an internal report was shown: %v", csShownIDs(g))
	}
	if g.Payload.ClassExcluded.Internal != 1 {
		t.Fatalf("class_excluded.internal = %d, want 1", g.Payload.ClassExcluded.Internal)
	}
}

// TestCommsScan_ServeNeverAdvancesCursor (C7): the reader's NextCursor is
// hours after the stored cursor, yet after a real gather the store still
// returns the PRE-gather value and was never advanced.
func TestCommsScan_ServeNeverAdvancesCursor(t *testing.T) {
	f := newCSFixture(t, nil)
	f.reader.items = []workmgmt.UserReportItem{csIssue(1, "Crash on save", "x", csBase.Add(time.Minute), commsExternalAuthor)}
	before, _, _ := f.cursors.Get(context.Background(), csKey(userreport.SourceIssues))
	_, g := f.gather(t)
	after, _, _ := f.cursors.Get(context.Background(), csKey(userreport.SourceIssues))
	if !after.Equal(before) {
		t.Fatalf("cursor moved on serve: %v -> %v", before, after)
	}
	if _, found, _ := f.cursors.Get(context.Background(), csKey(userreport.SourceIssueNotes)); found {
		t.Fatal("the note floor was written on serve")
	}
	if f.cursors.advances != 0 {
		t.Fatalf("store Advance called %d times on serve", f.cursors.advances)
	}
	if g.Payload.PendingCursor == nil || !g.Payload.PendingCursor.Cursor.Equal(csBase.Add(5*time.Hour)) {
		t.Fatalf("pending cursor = %+v, want the deferred target", g.Payload.PendingCursor)
	}
}

// TestCommsScan_FirstGatherInitialisesCursor: with no cursor row the store's
// Init passes through (insert-if-absent), which is not an advance.
func TestCommsScan_FirstGatherInitialisesCursor(t *testing.T) {
	f := newCSFixture(t, nil)
	f.cursors.vals = map[string]time.Time{}
	_, g := f.gather(t)
	if f.cursors.inits != 1 || f.cursors.advances != 0 {
		t.Fatalf("inits=%d advances=%d, want 1 and 0", f.cursors.inits, f.cursors.advances)
	}
	if _, found, _ := f.cursors.Get(context.Background(), csKey(userreport.SourceIssues)); !found {
		t.Fatal("Init did not reach the store")
	}
	if g.Payload.PendingCursor == nil {
		t.Fatal("a completed empty scan must carry a pending cursor")
	}
}

// TestCommsScan_EmptyScan: zero reports still serve a prompt and record the
// pending cursor.
func TestCommsScan_EmptyScan(t *testing.T) {
	f := newCSFixture(t, nil)
	cc, g := f.gather(t)
	if len(g.Payload.Shown) != 0 || g.Payload.PendingCursor == nil {
		t.Fatalf("empty scan payload = %+v", g.Payload)
	}
	if out := csBuild(t, cc); !strings.Contains(out, "No report was shown") {
		t.Fatal("empty scan prompt lacks the no-report instruction")
	}
}

// TestCommsScan_DuplicateIDKeepsLaterUpdate: a reader repeating an id is
// rendered once, at its later update.
func TestCommsScan_DuplicateIDKeepsLaterUpdate(t *testing.T) {
	f := newCSFixture(t, nil)
	f.reader.items = []workmgmt.UserReportItem{
		csIssue(1, "Crash on save", "old", csBase.Add(time.Minute), commsExternalAuthor),
		csIssue(1, "Crash on save", "new", csBase.Add(3*time.Minute), commsExternalAuthor),
		csIssue(1, "Crash on save", "older", csBase.Add(2*time.Minute), commsExternalAuthor),
	}
	cc, g := f.gather(t)
	if len(g.Payload.Shown) != 1 || len(cc.UserReports) != 1 || cc.UserReports[0].ReportBody != "new" {
		t.Fatalf("shown = %+v, want one report at its later update", g.Payload.Shown)
	}
}

// --- hold-back and progress (C8, C9, C10, C11, boundary) ---------------------

// TestCommsScan_OmittedReportReappears (C8 + the inclusive boundary): the
// gather cap keeps the two OLDEST of three reports; the pending cursor is held
// to the omitted report's updated_at, so after phase 7 advances to it the
// next REAL Scan lists — and shows — the omitted report. Its updated_at
// EQUALS the held cursor, which pins the inclusive bound.
func TestCommsScan_OmittedReportReappears(t *testing.T) {
	f := newCSFixture(t, nil)
	prev, prevIDs := commsScanMaxReports, commsScanMaxRecordedIDs
	commsScanMaxReports, commsScanMaxRecordedIDs = 2, 1
	t.Cleanup(func() { commsScanMaxReports, commsScanMaxRecordedIDs = prev, prevIDs })
	f.reader.items = []workmgmt.UserReportItem{
		csIssue(1, "Crash on save", "a", csBase.Add(1*time.Minute), commsExternalAuthor),
		csIssue(2, "Sync is slow", "b", csBase.Add(2*time.Minute), commsExternalAuthor),
		csIssue(3, "Login loops forever", "c", csBase.Add(3*time.Minute), commsExternalAuthor),
		csIssue(4, "Dark mode please", "d", csBase.Add(4*time.Minute), commsExternalAuthor),
	}
	cc, g := f.gather(t)
	if got := csShownIDs(g); !csEqual(got, []string{"UR-issue-2", "UR-issue-1"}) {
		t.Fatalf("first gather shown = %v, want the two oldest newest-first", got)
	}
	if cc.OmittedCount != 2 || g.Payload.OmittedCount != 2 || len(g.Payload.Omitted) != 1 {
		t.Fatalf("omitted = ctx %d payload %d %v, want 2 counted and 1 recorded (cap)", cc.OmittedCount, g.Payload.OmittedCount, g.Payload.Omitted)
	}
	pc := g.Payload.PendingCursor
	if pc == nil || !pc.Cursor.Equal(csBase.Add(3*time.Minute)) || !pc.NoteCursor.Equal(pc.Cursor) {
		t.Fatalf("pending cursor = %+v, want held to the earliest omitted report", pc)
	}
	f.applyPhase7(t, pc.Cursor, pc.NoteCursor)
	f.expire()
	_, g2 := f.gather(t)
	shown := csShownIDs(g2)
	if !csHas(shown, "UR-issue-3") {
		t.Fatalf("second gather shown = %v: the omitted report did not reappear", shown)
	}
	if csHas(shown, "UR-issue-1") || csHas(shown, "UR-issue-2") {
		t.Fatalf("second gather re-showed an accounted report: %v", shown)
	}
}

// csBulkItems returns n external reports with ~4KB bodies, oldest first, so
// prompt.RenderUserReports omits some.
func csBulkItems(n int) []workmgmt.UserReportItem {
	words := []string{"alpha", "bravo", "charlie", "delta", "echo", "foxtrot", "golf", "hotel", "india", "juliet",
		"kilo", "lima", "mike", "november", "oscar", "papa", "quebec", "romeo", "sierra", "tango"}
	var out []workmgmt.UserReportItem
	for i := 1; i <= n; i++ {
		body := strings.Repeat(words[i%len(words)]+" ", 3900/(len(words[i%len(words)])+1))
		out = append(out, csIssue(i, fmt.Sprintf("Problem %s %s", words[i%len(words)], words[(i*7)%len(words)]), body,
			csBase.Add(time.Duration(i)*time.Minute), commsExternalAuthor))
	}
	return out
}

// TestCommsScan_RenderOmittedReportReappears (C9 + C10): the render cap
// omits some of 15 large reports. Newest-first input means the NEWEST are
// omitted and the OLDEST renders (C10); the pending cursor is held to the
// earliest render-omitted report, so every omitted report is listed again on
// the next real Scan (C9).
func TestCommsScan_RenderOmittedReportReappears(t *testing.T) {
	f := newCSFixture(t, nil)
	f.reader.items = csBulkItems(15)
	cc, g := f.gather(t)
	if g.Payload.OmittedCount == 0 || cc.OmittedCount != 0 {
		t.Fatalf("render-omitted reports not recorded as omitted: payload omitted %d (ctx gather-omitted %d), want render omission only", g.Payload.OmittedCount, cc.OmittedCount)
	}
	if !csHas(csShownIDs(g), "UR-issue-1") {
		t.Fatalf("the OLDEST report was not shown under render omission: %v", csShownIDs(g))
	}
	out := csBuild(t, cc)
	if got := csListedIDs(csSection(t, out, "### Shown report ids (account for every one)"), csIsReportID); !csEqual(got, csShownIDs(g)) {
		t.Fatalf("prompt shown %v != payload shown %v", got, csShownIDs(g))
	}
	earliest := csBase.Add(time.Hour * 24)
	for _, id := range g.Payload.Omitted {
		var n int
		if _, err := fmt.Sscanf(id, "UR-issue-%d", &n); err != nil {
			t.Fatal(err)
		}
		if at := csBase.Add(time.Duration(n) * time.Minute); at.Before(earliest) {
			earliest = at
		}
	}
	pc := g.Payload.PendingCursor
	if pc == nil || !pc.Cursor.Equal(earliest) {
		t.Fatalf("pending cursor = %+v, want %v (earliest render-omitted)", pc, earliest)
	}
	f.applyPhase7(t, pc.Cursor, pc.NoteCursor)
	f.expire()
	_, g2 := f.gather(t)
	listed := append(csShownIDs(g2), g2.Payload.Omitted...)
	for _, id := range g.Payload.Omitted {
		if !csHas(listed, id) {
			t.Fatalf("render-omitted %s was not listed again (second gather listed %v)", id, listed)
		}
	}
	if len(csShownIDs(g2)) == 0 {
		t.Fatal("second gather showed nothing")
	}
}

// csThreeShown gathers three small reports, all shown.
func csThreeShown(t *testing.T) (*csFixture, *commsGather) {
	t.Helper()
	f := newCSFixture(t, nil)
	f.reader.items = []workmgmt.UserReportItem{
		csIssue(1, "Crash on save", "a", csBase.Add(1*time.Minute), commsExternalAuthor),
		csIssue(2, "Sync is slow", "b", csBase.Add(2*time.Minute), commsExternalAuthor),
		csIssue(3, "Login loops forever", "c", csBase.Add(3*time.Minute), commsExternalAuthor),
	}
	_, g := f.gather(t)
	if len(g.Payload.Shown) != 3 {
		t.Fatalf("fixture: shown = %v", csShownIDs(g))
	}
	return f, g
}

// csRegatherAfterHoldBack applies commsCursorHoldBack(ids) as phase 7 would
// and returns the next gather's shown ids.
func csRegatherAfterHoldBack(t *testing.T, f *csFixture, g *commsGather, ids ...string) []string {
	t.Helper()
	cursor, note, ok := commsCursorHoldBack(g.Payload, ids)
	if !ok {
		t.Fatal("commsCursorHoldBack refused a completed scan")
	}
	f.applyPhase7(t, cursor, note)
	f.expire()
	_, g2 := f.gather(t)
	return csShownIDs(g2)
}

// TestCommsScan_UnaccountedReportReappears (C11 through the real Scan): an
// unaccounted shown report holds the cursor back to it, so it is shown again.
func TestCommsScan_UnaccountedReportReappears(t *testing.T) {
	f, g := csThreeShown(t)
	shown := csRegatherAfterHoldBack(t, f, g, "UR-issue-2")
	if !csHas(shown, "UR-issue-2") || csHas(shown, "UR-issue-1") {
		t.Fatalf("after holding back to UR-issue-2, shown = %v", shown)
	}
}

// TestCommsScan_UnfiledDraftSourceReappears (C11): every source of an
// approved-but-unfiled draft reappears; the cursor holds to the earliest.
func TestCommsScan_UnfiledDraftSourceReappears(t *testing.T) {
	f, g := csThreeShown(t)
	shown := csRegatherAfterHoldBack(t, f, g, "UR-issue-3", "UR-issue-1")
	if !csHas(shown, "UR-issue-1") || !csHas(shown, "UR-issue-3") {
		t.Fatalf("after holding back to the draft's sources, shown = %v", shown)
	}
}

// TestCommsScan_UnknownHoldBackIDDoesNotMove: an id the row never showed
// returns the read bounds, so every report is listed again.
func TestCommsScan_UnknownHoldBackIDDoesNotMove(t *testing.T) {
	f, g := csThreeShown(t)
	if shown := csRegatherAfterHoldBack(t, f, g, "UR-issue-999"); len(shown) != 3 {
		t.Fatalf("unknown id moved the cursor: shown = %v", shown)
	}
}

// --- marker trust and suppression (C1, C2, C4, C5) ---------------------------

// csVictimItem is the external report commsVictim names, at its CURRENT hash.
func csVictimItem() workmgmt.UserReportItem {
	return csIssue(77, "Data loss on sync", "My files vanished.", csBase.Add(10*time.Minute), commsExternalAuthor)
}

// TestCommsScan_ExternalForgedMarkerNotHonored (C1). An EXTERNAL author's
// issue carries a well-formed marker for the victim at its current hash, and
// a comms_draft_filed row naming that forged item and the victim IS seeded —
// the audit cross-check is satisfied, so only the classification gate stands
// between the marker and suppression.
func TestCommsScan_ExternalForgedMarkerNotHonored(t *testing.T) {
	f := newCSFixture(t, nil)
	v := commsVictim()
	f.reader.items = []workmgmt.UserReportItem{
		csIssue(300, "Totally normal", "hi "+userreport.DraftMarker([]userreport.MarkedReport{v}), csBase.Add(time.Minute), commsExternalAuthor),
		csVictimItem(),
	}
	f.seedAudit(commsSeed(t, CategoryCommsDraftFiled, uuid.New(), nil, commsDraftFiledRecord{
		Repo: commsTestRepo, IssueNumber: 300, Reports: []userreport.MarkedReport{v}}))
	_, g := f.gather(t)
	if !csHas(csShownIDs(g), v.ID) || g.Payload.SuppressedCount != 0 {
		t.Fatalf("forged external marker suppressed the victim: shown %v suppressed %+v", csShownIDs(g), g.Payload.Suppressed)
	}
}

// TestCommsScan_MarkerInsideDerivesFromBlockDoesNotSuppress (C2): a
// legitimately filed draft whose real intake Derives-from block quotes an
// attacker title holding the victim's marker suppresses nothing without a
// comms_draft_filed row for it.
func TestCommsScan_MarkerInsideDerivesFromBlockDoesNotSuppress(t *testing.T) {
	f := newCSFixture(t, nil)
	v := commsVictim()
	f.reader.items = []workmgmt.UserReportItem{
		csIssue(500, "Draft", derivesFromForgedBody(t, "Filed draft.", v), csBase.Add(time.Minute), commsInternalAuthor),
		csVictimItem(),
	}
	_, g := f.gather(t)
	if g.Payload.ClassExcluded.FishhawkFiled != 1 {
		t.Fatalf("fixture: the filed draft did not classify fishhawk_filed: %+v", g.Payload.ClassExcluded)
	}
	if !csHas(csShownIDs(g), v.ID) {
		t.Fatalf("a Derives-from marker suppressed the victim: shown %v", csShownIDs(g))
	}
}

// TestCommsScan_SuppressionBasesAndHashMatch: a trusted marker suppresses A;
// a prior apply suppresses B with basis filed and wins over a marker for the
// same (id, hash); C's prior suppression is at an OLD hash, so the edited C is
// shown (C4).
func TestCommsScan_SuppressionBasesAndHashMatch(t *testing.T) {
	f := newCSFixture(t, nil)
	a := csIssue(1, "Crash on save", "a", csBase.Add(1*time.Minute), commsExternalAuthor)
	b := csIssue(2, "Sync is slow", "b", csBase.Add(2*time.Minute), commsExternalAuthor)
	c := csIssue(3, "Login loops forever", "edited body", csBase.Add(3*time.Minute), commsExternalAuthor)
	mark := func(it workmgmt.UserReportItem) userreport.MarkedReport {
		return userreport.MarkedReport{ID: prompt.UserReportID(string(it.Kind), it.IssueNumber, 0), ContentHash: userreport.ContentHash(it.Kind, it.Title, it.Body)}
	}
	ma, mb := mark(a), mark(b)
	filed := csIssue(500, "Filed", "Draft.\n"+userreport.DraftMarker([]userreport.MarkedReport{ma, mb}), csBase, commsInternalAuthor)
	f.reader.items = []workmgmt.UserReportItem{filed, a, b, c}
	f.seedAudit(commsSeed(t, CategoryCommsDraftFiled, uuid.New(), nil, commsDraftFiledRecord{
		Repo: commsTestRepo, IssueNumber: 500, Reports: []userreport.MarkedReport{ma, mb}}))
	f.seedAudit(commsSeed(t, CategoryCommsApplyCompleted, uuid.New(), nil, applyRow(commsTestRepo,
		commsSuppression{ID: mb.ID, Hash: mb.ContentHash, Basis: commsSuppressionBasisFiled},
		commsSuppression{ID: "UR-issue-3", Hash: userreport.ContentHash(c.Kind, c.Title, "original body"), Basis: commsSuppressionBasisRejected})))
	cc, g := f.gather(t)
	if got := csShownIDs(g); !csEqual(got, []string{"UR-issue-3"}) {
		t.Fatalf("shown = %v, want only the edited UR-issue-3", got)
	}
	want := fmt.Sprint([]commsSuppression{
		{ID: ma.ID, Hash: ma.ContentHash, Basis: commsSuppressionBasisMarker},
		{ID: mb.ID, Hash: mb.ContentHash, Basis: commsSuppressionBasisFiled},
	})
	if fmt.Sprint(g.Payload.Suppressed) != want || g.Payload.SuppressedCount != 2 || cc.SuppressedCount != 2 {
		t.Fatalf("suppressed = %+v (count %d/%d), want %s", g.Payload.Suppressed, g.Payload.SuppressedCount, cc.SuppressedCount, want)
	}
}

// TestCommsScan_SuppressionMemoryFilters (C5 at the gather): a prior
// suppression for ANOTHER repo, or under ANOTHER account, suppresses nothing.
//
// FIXTURE REQUIREMENT (approval condition 1): the shared auditFake.ListAll
// ignores AccountID and an APPENDED row carries a nil account, so the
// other-account row is SEEDED via auditFake.seeded with an explicit NON-NIL
// AccountID while the gather runs under the run's NIL account. Only the
// account-equality filter then stands between that row and suppression.
func TestCommsScan_SuppressionMemoryFilters(t *testing.T) {
	victim := csVictimItem()
	v := commsVictim()
	sup := commsSuppression{ID: v.ID, Hash: v.ContentHash, Basis: commsSuppressionBasisFiled}
	other := uuid.New()
	cases := []struct {
		name    string
		account *uuid.UUID
		repo    string
		shown   bool
	}{
		{"same repo and account suppresses", nil, commsTestRepo, false},
		{"other repo", nil, "acme/other", true},
		{"other account", &other, commsTestRepo, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newCSFixture(t, nil)
			f.reader.items = []workmgmt.UserReportItem{victim}
			f.seedAudit(commsSeed(t, CategoryCommsApplyCompleted, uuid.New(), tc.account, applyRow(tc.repo, sup)))
			_, g := f.gather(t)
			if got := csHas(csShownIDs(g), v.ID); got != tc.shown {
				t.Fatalf("victim shown = %v, want %v (suppressed %+v)", got, tc.shown, g.Payload.Suppressed)
			}
		})
	}
}

// --- charter refusal (C12) ----------------------------------------------------

func TestCommsScan_CharterRefusalReasons(t *testing.T) {
	slowResolver := func(c *Config) {
		c.DocumentResolver = igCharterConfigWith(&igFetcher{content: csCharterDoc, delay: time.Minute}).DocumentResolver
	}
	cases := []struct {
		reason   string
		setup    func(t *testing.T, f *csFixture)
		cfg      func(*Config)
		wantPath bool
	}{
		{reason: commsRefusalConventionsUnavailable, setup: func(t *testing.T, _ *csFixture) {
			installConventions(t, workmgmt.Default(), errors.New("conventions fetch failed"))
		}},
		{reason: commsRefusalRepoMalformed, setup: func(_ *testing.T, f *csFixture) { f.runRow.Repo = "not-a-repo" }},
		{reason: commsRefusalCharterUndeclared, setup: func(t *testing.T, _ *csFixture) {
			conv := workmgmt.Default()
			conv.Charter = nil
			installConventions(t, conv, nil)
		}},
		{reason: commsRefusalSeamUnwired, cfg: func(c *Config) { c.DocumentResolver = nil }, wantPath: true},
		{reason: commsRefusalCharterUnresolved, setup: func(_ *testing.T, f *csFixture) { f.fetcher.missing = true }, wantPath: true},
		{reason: commsRefusalBudgetExceeded, cfg: slowResolver, setup: func(t *testing.T, _ *csFixture) {
			prev := commsScanBudget
			commsScanBudget = 50 * time.Millisecond
			t.Cleanup(func() { commsScanBudget = prev })
		}, wantPath: true},
		{reason: commsRefusalCharterRubricUnparsed, setup: func(_ *testing.T, f *csFixture) { f.fetcher.content = igCharterNoRubric }, wantPath: true},
		{reason: commsRefusalCharterRubricUnconforms, setup: func(_ *testing.T, f *csFixture) { f.fetcher.content = csCharterUnconforming }, wantPath: true},
	}
	seen := map[string]bool{}
	for _, tc := range cases {
		t.Run(tc.reason, func(t *testing.T) {
			f := newCSFixture(t, tc.cfg)
			if tc.setup != nil {
				tc.setup(t, f)
			}
			cc, g, err := f.s.resolveCommsScanContext(context.Background(), f.runRow, f.plan)
			var ref *commsCharterRefusal
			if !errors.As(err, &ref) || cc != nil || g != nil {
				t.Fatalf("got (%v, %v, %v), want a *commsCharterRefusal", cc, g, err)
			}
			if ref.Reason != tc.reason {
				t.Fatalf("reason = %q, want %q", ref.Reason, tc.reason)
			}
			if tc.wantPath && ref.CharterPath != igCharterPath {
				t.Fatalf("charter_path = %q, want %q", ref.CharterPath, igCharterPath)
			}
			if f.reader.callCount() != 0 {
				t.Fatal("a refused gather read user reports")
			}
			if commsScans.size() != 0 && commsScansHas(f.s) {
				t.Fatal("a refusal was cached")
			}
			if !strings.Contains(f.logs.String(), "comms scan gather: charter refused") || !strings.Contains(err.Error(), tc.reason) {
				t.Fatalf("refusal not logged / not named: %s", f.logs.String())
			}
			seen[tc.reason] = true
		})
	}
	for _, r := range commsCharterRefusalReasons() {
		if !seen[r] {
			t.Errorf("closed reason %q has no refusal case", r)
		}
	}
	if len(commsCharterRefusalReasons()) != len(cases) {
		t.Errorf("closed set has %d reasons, table %d", len(commsCharterRefusalReasons()), len(cases))
	}
}

// commsScansHas reports whether the process cache holds an entry for srv.
func commsScansHas(srv *Server) bool {
	commsScans.mu.Lock()
	defer commsScans.mu.Unlock()
	for k := range commsScans.entries {
		if k.srv == srv {
			return true
		}
	}
	return false
}

func TestCommsCharterReason_UnexpectedIsUnresolved(t *testing.T) {
	if got := commsCharterReason("hook_panic"); got != commsRefusalCharterUnresolved {
		t.Fatalf("unexpected reason mapped to %q", got)
	}
}

// TestCommsScan_RecordedCharterIDsMatchRender (C19): a rubric table carrying
// V1, vv2 and N9 records exactly the ids the served prompt renders.
func TestCommsScan_RecordedCharterIDsMatchRender(t *testing.T) {
	f := newCSFixture(t, nil)
	f.fetcher.content = csCharterMixed
	cc, g := f.gather(t)
	out := csBuild(t, cc)
	rubric := csListedIDs(csSection(t, out, "### Charter rubric"), commsRubricIDConforms.MatchString)
	nonGoals := csListedIDs(csSection(t, out, "### Charter non-goals"), commsNonGoalIDConforms.MatchString)
	if !csEqual(rubric, g.Payload.Charter.RubricIDs) || !csEqual(rubric, []string{"V1"}) {
		t.Fatalf("rendered rubric %v, recorded %v, want [V1]", rubric, g.Payload.Charter.RubricIDs)
	}
	if !csEqual(nonGoals, g.Payload.Charter.NonGoalIDs) || !csEqual(nonGoals, []string{"N1"}) {
		t.Fatalf("rendered non-goals %v, recorded %v", nonGoals, g.Payload.Charter.NonGoalIDs)
	}
	if !strings.Contains(out, "2 rubric line(s) withheld") {
		t.Fatal("the unconforming rubric lines were not disclosed as withheld")
	}
}

// --- degrades -----------------------------------------------------------------

func csFailingGitHub(t *testing.T) *githubclient.Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"message":"boom"}`))
	}))
	t.Cleanup(srv.Close)
	return &githubclient.Client{BaseURL: srv.URL, Tokens: &fakeTokenProvider{tok: "ghs_t"},
		HTTP: &http.Client{Timeout: 5 * time.Second}, AppJWT: func() (string, error) { return "jwt", nil }}
}

func TestCommsScan_Degrades(t *testing.T) {
	cases := []struct {
		reason    string
		source    string
		scanLevel bool
		cfg       func(*Config)
		setup     func(t *testing.T, f *csFixture)
	}{
		{reason: commsDegradeCursorStoreUnwired, source: commsDegradeSourceUserReports, scanLevel: true,
			cfg: func(c *Config) { c.UserReportCursors = nil }},
		{reason: commsDegradeReaderUnavailable, source: commsDegradeSourceUserReports, scanLevel: true,
			setup: func(_ *testing.T, _ *csFixture) {
				commsUserReportReaderFor = func(string) (workmgmt.UserReportReader, error) {
					return nil, errors.New("provider lacks the user-report capability")
				}
			}},
		{reason: commsDegradeScopeUnavailable, source: commsDegradeSourceUserReports, scanLevel: true,
			setup: func(t *testing.T, f *csFixture) {
				f.runRow.InstallationID = nil
				f.s.cfg.GitHub = csFailingGitHub(t)
			}},
		{reason: commsDegradeAccountUnparseable, source: commsDegradeSourceUserReports, scanLevel: true,
			setup: func(_ *testing.T, f *csFixture) { f.runRow.AccountID = "not-a-uuid" }},
		{reason: commsDegradeScanFailed, source: commsDegradeSourceUserReports, scanLevel: true,
			setup: func(_ *testing.T, f *csFixture) { f.reader.err = errors.New("forge 502") }},
		{reason: commsDegradeBudgetExceeded, source: commsDegradeSourceUserReports, scanLevel: true,
			setup: func(t *testing.T, f *csFixture) {
				f.reader.block = true
				prev := commsScanBudget
				commsScanBudget = 50 * time.Millisecond
				t.Cleanup(func() { commsScanBudget = prev })
			}},
		{reason: commsDegradeSuppressionMemoryUnavailable, source: commsDegradeSourceAudit,
			setup: func(_ *testing.T, f *csFixture) { f.au.listAllErrCategory = CategoryCommsApplyCompleted }},
		{reason: commsDegradeDraftFiledUnavailable, source: commsDegradeSourceAudit,
			setup: func(_ *testing.T, f *csFixture) { f.au.listAllErrCategory = CategoryCommsDraftFiled }},
	}
	for _, tc := range cases {
		t.Run(tc.reason, func(t *testing.T) {
			f := newCSFixture(t, tc.cfg)
			f.reader.items = []workmgmt.UserReportItem{csIssue(1, "Crash on save", "a", csBase.Add(time.Minute), commsExternalAuthor)}
			if tc.setup != nil {
				tc.setup(t, f)
			}
			cc, g := f.gather(t)
			found := false
			for _, d := range g.Payload.Degradations {
				if d.Source == tc.source && d.Reason == tc.reason {
					found = true
				}
			}
			if !found {
				t.Fatalf("payload degradations %+v lack %s/%s", g.Payload.Degradations, tc.source, tc.reason)
			}
			out := csBuild(t, cc)
			if line := fmt.Sprintf("- degraded source %s: reason %s, count 1", tc.source, tc.reason); !strings.Contains(out, line) {
				t.Fatalf("prompt coverage block lacks %q", line)
			}
			if tc.scanLevel {
				if len(g.Payload.Shown) != 0 || g.Payload.PendingCursor != nil {
					t.Fatalf("scan-level degrade showed %v / pending %+v, want none", csShownIDs(g), g.Payload.PendingCursor)
				}
			} else if len(g.Payload.Shown) != 1 || g.Payload.PendingCursor == nil {
				t.Fatalf("a memory degrade must still show the report: %+v", g.Payload)
			}
			if !strings.Contains(f.logs.String(), "comms scan gather: partial scan") {
				t.Fatal("degrade not WARN-logged")
			}
		})
	}
}

func TestCommsGatherState_DegradeMemoryFallback(t *testing.T) {
	st := &commsGatherState{s: New(Config{Addr: "127.0.0.1:0"})}
	st.degradeMemory(context.Background(), errors.New("opaque"), commsDegradeDraftFiledUnavailable)
	if len(st.deg) != 1 || st.deg[0].Reason != commsDegradeDraftFiledUnavailable || st.deg[0].Source != commsDegradeSourceAudit {
		t.Fatalf("fallback degrade = %+v", st.deg)
	}
}

// --- cache ----------------------------------------------------------------------

func TestCommsScanCache_TTLAndRefusalNotCached(t *testing.T) {
	f := newCSFixture(t, nil)
	f.gather(t)
	f.gather(t)
	if f.reader.callCount() != 1 {
		t.Fatalf("reader calls = %d within the TTL, want 1", f.reader.callCount())
	}
	f.expire()
	f.gather(t)
	if f.reader.callCount() != 2 {
		t.Fatalf("reader calls = %d after expiry, want 2", f.reader.callCount())
	}

	f2 := newCSFixture(t, nil)
	f2.fetcher.missing = true
	for i := 0; i < 2; i++ {
		if _, _, err := f2.s.resolveCommsScanContext(context.Background(), f2.runRow, f2.plan); err == nil {
			t.Fatal("want a refusal")
		}
	}
	if f2.fetcher.fetchCalls() != 2 {
		t.Fatalf("charter fetches = %d, want 2 (a refusal is never cached)", f2.fetcher.fetchCalls())
	}
}

func TestCommsScanCache_MaxEntriesRunsUncached(t *testing.T) {
	prev := commsScanCacheMaxEntries
	commsScanCacheMaxEntries = 0
	t.Cleanup(func() { commsScanCacheMaxEntries = prev })
	f := newCSFixture(t, nil)
	f.gather(t)
	f.gather(t)
	if f.reader.callCount() != 2 {
		t.Fatalf("reader calls = %d with a full cache, want 2 (uncached)", f.reader.callCount())
	}
}

func TestCommsScanCache_PanicIsNotCached(t *testing.T) {
	f := newCSFixture(t, nil)
	f.reader.panicking = true
	func() {
		defer func() {
			if recover() == nil {
				t.Fatal("the gather's panic did not propagate to the leader")
			}
		}()
		_, _, _ = f.s.resolveCommsScanContext(context.Background(), f.runRow, f.plan)
	}()
	if commsScansHas(f.s) {
		t.Fatal("an aborted gather stayed cached")
	}
}

// TestCommsScanCache_ConcurrentServesGatherOnce: a caller arriving while a
// gather is in flight joins it — including its refusal — instead of
// gathering again.
func TestCommsScanCache_ConcurrentServesGatherOnce(t *testing.T) {
	for _, refuse := range []bool{false, true} {
		t.Run(fmt.Sprintf("refuse=%v", refuse), func(t *testing.T) {
			f := newCSFixture(t, nil)
			f.fetcher.delay = 200 * time.Millisecond
			if refuse {
				f.fetcher.content = igCharterNoRubric
			}
			joined := make(chan struct{}, 1)
			prev := commsScanJoinHook
			commsScanJoinHook = func() { joined <- struct{}{} }
			t.Cleanup(func() { commsScanJoinHook = prev })

			type res struct {
				g   *commsGather
				err error
			}
			out := make(chan res, 2)
			serve := func() {
				_, g, err := f.s.resolveCommsScanContext(context.Background(), f.runRow, f.plan)
				out <- res{g, err}
			}
			go serve()
			for f.fetcher.fetchCalls() == 0 {
				time.Sleep(time.Millisecond)
			}
			go serve()
			select {
			case <-joined:
			case <-time.After(5 * time.Second):
				t.Fatal("the second serve never joined the in-flight gather")
			}
			a, b := <-out, <-out
			if f.fetcher.fetchCalls() != 1 {
				t.Fatalf("charter fetches = %d, want 1 (single flight)", f.fetcher.fetchCalls())
			}
			if refuse {
				if a.err == nil || b.err == nil {
					t.Fatalf("the joined caller did not receive the refusal: %v / %v", a.err, b.err)
				}
			} else if a.g != b.g || a.g == nil {
				t.Fatal("the joined caller did not share the leader's gather")
			}
		})
	}
}

func TestCommsScanCache_SweepDropsExpired(t *testing.T) {
	c := &commsScanCache{}
	done := make(chan struct{})
	close(done)
	k1, k2 := commsScanKey{runID: uuid.New()}, commsScanKey{runID: uuid.New()}
	c.entries = map[commsScanKey]*commsScanEntry{
		k1: {done: done, ok: true, expires: csBase.Add(-time.Second)},
		k2: {done: make(chan struct{})},
	}
	c.sweepLocked(csBase)
	if _, ok := c.entries[k1]; ok || c.size() != 1 {
		t.Fatalf("sweep kept the expired entry (size %d)", c.size())
	}
}

// --- clusters, payload stamping and record --------------------------------------

func TestCommsSuggestClusters(t *testing.T) {
	kept := func(it workmgmt.UserReportItem) commsKept {
		ri := commsClassified(it)
		return commsKept{item: ri, id: prompt.UserReportID(string(it.Kind), it.IssueNumber, it.CommentID)}
	}
	reports := []commsKept{
		kept(csIssue(1, "CSV export fails silently", "", csBase, commsExternalAuthor)),
		kept(csComment(1, 9, "\n  \nCSV export fails silently again\nmore", csBase, commsExternalAuthor)),
		kept(csIssue(2, "Dark mode please", "", csBase, commsExternalAuthor)),
		kept(csIssue(3, "CSV export fails silently today", "", csBase, commsExternalAuthor)),
		kept(csComment(4, 1, "   ", csBase, commsExternalAuthor)),
		kept(csIssue(5, "Login loops forever", "", csBase, commsExternalAuthor)),
		kept(csIssue(6, "Login loops forever", "", csBase, commsExternalAuthor)),
	}
	got := commsSuggestClusters(reports)
	if len(got) != 2 {
		t.Fatalf("clusters = %+v, want 2", got)
	}
	if !csEqual(got[0].ReportIDs, []string{"UR-issue-5", "UR-issue-6"}) || got[0].Score != 1 {
		t.Fatalf("first cluster = %+v, want the identical-title pair at score 1", got[0])
	}
	if !csEqual(got[1].ReportIDs, []string{"UR-comment-1-9", "UR-issue-1", "UR-issue-3"}) || got[1].Score <= 0 || got[1].Score >= 1 {
		t.Fatalf("second cluster = %+v", got[1])
	}
}

func TestCommsGather_PayloadForStampsStageAndRecords(t *testing.T) {
	f := newCSFixture(t, nil)
	f.reader.items = []workmgmt.UserReportItem{csIssue(1, "Crash on save", "a", csBase.Add(time.Minute), commsExternalAuthor)}
	at := csBase.Add(30 * time.Second)
	f.plan.DispatchedAt = &at
	_, g := f.gather(t)
	p := g.payloadFor(f.plan)
	if p.StageID != f.plan.ID || p.StageAttempt != run.StageAttemptToken(&at) || p.StageAttempt == "" {
		t.Fatalf("payloadFor = stage %s attempt %q", p.StageID, p.StageAttempt)
	}
	if g.Payload.StageID != uuid.Nil || g.Payload.StageAttempt != "" {
		t.Fatal("payloadFor mutated the cached gather")
	}
	if _, appended, err := f.s.recordCommsScanGathered(context.Background(), f.runRow.ID, f.plan.ID, p); err != nil || !appended {
		t.Fatalf("record = %v, %v", appended, err)
	}
	f.au.mu.Lock()
	n := len(f.au.appended)
	body := f.au.appended[0].Payload
	f.au.mu.Unlock()
	if n != 1 {
		t.Fatalf("appended %d rows", n)
	}
	rec, err := decodeCommsScanGathered(body)
	if err != nil {
		t.Fatal(err)
	}
	if !csEqual(csShownIDs(&commsGather{Payload: *rec}), csShownIDs(g)) || rec.GatherDigest == "" {
		t.Fatalf("recorded payload = %+v", rec)
	}
}

func TestCommsDeferringCursors_AdvanceIsInert(t *testing.T) {
	inner := &csCursors{vals: map[string]time.Time{}}
	d := commsDeferringCursors{inner: inner}
	to, moved, err := d.Advance(context.Background(), csKey(userreport.SourceIssues), csBase)
	if err != nil || moved || !to.Equal(csBase) || inner.advances != 0 || len(inner.vals) != 0 {
		t.Fatalf("Advance = (%v, %v, %v); inner advances %d", to, moved, err, inner.advances)
	}
}

func TestCommsPendingCursorFor_Clamps(t *testing.T) {
	at := func(m int) time.Time { return csBase.Add(time.Duration(m) * time.Minute) }
	omit := func(m int) commsKept {
		return commsKept{item: userreport.ReportItem{UserReportItem: workmgmt.UserReportItem{UpdatedAt: at(m)}}}
	}
	cases := []struct {
		name                       string
		since, noteSince, next, nn int
		omitted                    []commsKept
		cursor, note               int
	}{
		{"no omission keeps the next cursors", 0, 0, 60, 30, nil, 60, 30},
		{"held to the earliest omitted, note follows", 0, 0, 60, 60, []commsKept{omit(40), omit(20)}, 20, 20},
		{"never behind the read bound", 10, 10, 60, 60, []commsKept{omit(5)}, 10, 10},
		{"note never above the cursor", 10, 30, 60, 60, []commsKept{omit(5)}, 10, 10},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pc := commsPendingCursorFor(&userreport.Report{Since: at(tc.since), NoteSince: at(tc.noteSince),
				NextCursor: at(tc.next), NextNoteCursor: at(tc.nn)}, tc.omitted)
			if !pc.Cursor.Equal(at(tc.cursor)) || !pc.NoteCursor.Equal(at(tc.note)) {
				t.Fatalf("pending = %v / %v, want %v / %v", pc.Cursor, pc.NoteCursor, at(tc.cursor), at(tc.note))
			}
		})
	}
}

func TestCommsCapSuppressions(t *testing.T) {
	prev := commsScanMaxRecordedIDs
	commsScanMaxRecordedIDs = 1
	t.Cleanup(func() { commsScanMaxRecordedIDs = prev })
	got := commsCapSuppressions([]commsSuppression{{ID: "a"}, {ID: "b"}})
	if len(got) != 1 || got[0].ID != "a" {
		t.Fatalf("capped = %+v", got)
	}
}
