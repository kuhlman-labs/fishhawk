package server

import (
	"bytes"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
)

// ---------------------------------------------------------------------
// CSV export test helpers.
// ---------------------------------------------------------------------

// csvSeedEntry describes one audit entry to seed. subject == "" seeds an
// entry with nil actor_kind AND nil actor_subject (exercising the empty
// cell rendering); a non-empty subject seeds a human actor.
type csvSeedEntry struct {
	category string
	subject  string
	payload  string
}

// chainCSVEntries builds a genuinely-chained audit entry set (real
// prev_hash / entry_hash) with per-entry control over category,
// actor_subject, and payload — the flexibility the CSV filter and
// quoting tests need beyond chainEntries.
func chainCSVEntries(t *testing.T, runID *uuid.UUID, specs ...csvSeedEntry) []*audit.Entry {
	t.Helper()
	var out []*audit.Entry
	var prev *string
	for i, sp := range specs {
		ts := time.Date(2026, 5, 1, 12, i, 30, 0, time.UTC)
		var ak *audit.ActorKind
		var subj *string
		if sp.subject != "" {
			a := audit.ActorUser
			ak = &a
			s := sp.subject
			subj = &s
		}
		payload := json.RawMessage(sp.payload)
		h, err := audit.ComputeEntryHash(audit.HashInputs{
			RunID:        runID,
			Timestamp:    ts,
			Category:     sp.category,
			ActorKind:    ak,
			ActorSubject: subj,
			Payload:      payload,
			PrevHash:     prev,
		})
		if err != nil {
			t.Fatalf("compute hash: %v", err)
		}
		out = append(out, &audit.Entry{
			ID:           uuid.New(),
			Sequence:     int64(i + 1),
			RunID:        runID,
			Timestamp:    ts,
			Category:     sp.category,
			ActorKind:    ak,
			ActorSubject: subj,
			Payload:      payload,
			PrevHash:     prev,
			EntryHash:    h,
		})
		hh := h
		prev = &hh
	}
	return out
}

func doExportCSV(s *Server, query string) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v0/audit/export.csv"+query, nil)
	s.Handler().ServeHTTP(rec, req)
	return rec
}

// parseCSV parses a CSV response body into header + data rows, failing
// the test on any malformed structure.
func parseCSV(t *testing.T, body []byte) (header []string, rows [][]string) {
	t.Helper()
	rd := csv.NewReader(bytes.NewReader(body))
	all, err := rd.ReadAll()
	if err != nil {
		t.Fatalf("csv parse: %v\nbody: %s", err, body)
	}
	if len(all) == 0 {
		t.Fatalf("csv body has no header row: %s", body)
	}
	return all[0], all[1:]
}

// col returns the index of a named column in the header.
func col(t *testing.T, header []string, name string) int {
	t.Helper()
	for i, h := range header {
		if h == name {
			return i
		}
	}
	t.Fatalf("column %q not in header %v", name, header)
	return -1
}

// ---------------------------------------------------------------------
// (a) RFC 4180 quoting/escaping torture + rune-boundary truncation.
// ---------------------------------------------------------------------

func TestAuditExportCSV_QuotingAndTruncation(t *testing.T) {
	fr := newFakeRepo()
	id := seedExportRun(fr, "acme/app", time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC))

	// actor_subject carries a real newline, comma, and double quote.
	nastySubject := "quote\"here,comma\nnewline"
	// payload contains commas and escaped quotes so the compacted JSON
	// cell must be RFC 4180 quoted.
	nastyPayload := `{"a":"b,c","d":"\"q\""}`
	// A >256-rune payload whose 256th boundary lands mid multi-byte rune
	// run: 300 'é' (2 bytes each) inside a JSON string.
	bigPayload := `{"note":"` + strings.Repeat("é", 300) + `"}`

	af := &exportAuditFake{perRun: map[uuid.UUID][]*audit.Entry{
		id: chainCSVEntries(t, &id,
			csvSeedEntry{category: "approval_submitted", subject: nastySubject, payload: nastyPayload},
			csvSeedEntry{category: "note", subject: "u@x", payload: bigPayload},
		),
	}}
	s := New(Config{AuditRepo: af, RunRepo: fr, SigningRepo: &exportSigningFake{}})

	rec := doExportCSV(s, "?include_global=false")
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	header, rows := parseCSV(t, rec.Body.Bytes())
	if len(rows) != 2 {
		t.Fatalf("got %d data rows, want 2", len(rows))
	}
	subjCol := col(t, header, "actor_subject")
	payCol := col(t, header, "payload_summary")

	// Round-trip proves RFC 4180 quoting: the parsed cell is byte-exact.
	if rows[0][subjCol] != nastySubject {
		t.Errorf("actor_subject round-trip mismatch:\n got %q\nwant %q", rows[0][subjCol], nastySubject)
	}
	var compact bytes.Buffer
	if err := json.Compact(&compact, json.RawMessage(nastyPayload)); err != nil {
		t.Fatal(err)
	}
	if rows[0][payCol] != compact.String() {
		t.Errorf("payload_summary round-trip mismatch:\n got %q\nwant %q", rows[0][payCol], compact.String())
	}

	// Rune-boundary truncation: the big payload cell ends with the
	// marker, the pre-marker text is exactly 256 valid runes, and the
	// whole cell is valid UTF-8 (never split a multi-byte character).
	big := rows[1][payCol]
	if !strings.HasSuffix(big, "...(truncated)") {
		t.Fatalf("big payload not truncated: %q", big)
	}
	if !utf8.ValidString(big) {
		t.Errorf("truncated payload is not valid UTF-8 (split a rune): %q", big)
	}
	head := strings.TrimSuffix(big, "...(truncated)")
	if n := utf8.RuneCountInString(head); n != 256 {
		t.Errorf("truncated to %d runes, want 256", n)
	}
}

// ---------------------------------------------------------------------
// (b) date-range + approver filter exactness.
// ---------------------------------------------------------------------

func TestAuditExportCSV_DateRangeAndApprover(t *testing.T) {
	fr := newFakeRepo()
	// Two runs inside the date range, one outside.
	inA := seedExportRun(fr, "acme/app", time.Date(2026, 5, 10, 12, 0, 0, 0, time.UTC))
	inB := seedExportRun(fr, "acme/app", time.Date(2026, 5, 12, 12, 0, 0, 0, time.UTC))
	outside := seedExportRun(fr, "acme/app", time.Date(2026, 4, 1, 12, 0, 0, 0, time.UTC))

	af := &exportAuditFake{perRun: map[uuid.UUID][]*audit.Entry{
		inA: chainCSVEntries(t, &inA,
			csvSeedEntry{category: "approval_submitted", subject: "alice@x", payload: `{"i":0}`},
			csvSeedEntry{category: "approval_submitted", subject: "bob@x", payload: `{"i":1}`},
			csvSeedEntry{category: "plan_generated", subject: "alice@x", payload: `{"i":2}`},
		),
		inB: chainCSVEntries(t, &inB,
			csvSeedEntry{category: "approval_submitted", subject: "alice@x", payload: `{"i":0}`},
		),
		// Outside the date range: alice approval that must be excluded.
		outside: chainCSVEntries(t, &outside,
			csvSeedEntry{category: "approval_submitted", subject: "alice@x", payload: `{"i":0}`},
		),
	}}
	s := New(Config{AuditRepo: af, RunRepo: fr, SigningRepo: &exportSigningFake{}})

	rec := doExportCSV(s, "?from=2026-05-01T00:00:00Z&to=2026-05-31T00:00:00Z&approver=alice@x&include_global=false")
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	header, rows := parseCSV(t, rec.Body.Bytes())
	catCol := col(t, header, "category")
	subjCol := col(t, header, "actor_subject")
	// Exactly two rows: alice's approval in inA and inB. Not bob's, not
	// the plan_generated, not the outside-range run.
	if len(rows) != 2 {
		t.Fatalf("got %d rows, want 2 (alice approvals inside range); rows=%v", len(rows), rows)
	}
	for _, row := range rows {
		if row[catCol] != "approval_submitted" || row[subjCol] != "alice@x" {
			t.Errorf("unexpected row %v", row)
		}
	}
}

// ---------------------------------------------------------------------
// (c) filters compose ANDed.
// ---------------------------------------------------------------------

func TestAuditExportCSV_FiltersComposeANDed(t *testing.T) {
	fr := newFakeRepo()
	id := seedExportRun(fr, "acme/app", time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC))
	af := &exportAuditFake{perRun: map[uuid.UUID][]*audit.Entry{
		id: chainCSVEntries(t, &id,
			csvSeedEntry{category: "approval_submitted", subject: "alice@x", payload: `{"i":0}`},
			csvSeedEntry{category: "plan_generated", subject: "alice@x", payload: `{"i":1}`},
		),
	}}
	s := New(Config{AuditRepo: af, RunRepo: fr, SigningRepo: &exportSigningFake{}})

	t.Run("approver + matching category narrows", func(t *testing.T) {
		rec := doExportCSV(s, "?approver=alice@x&category=approval_submitted&include_global=false")
		if rec.Code != http.StatusOK {
			t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
		}
		_, rows := parseCSV(t, rec.Body.Bytes())
		if len(rows) != 1 {
			t.Fatalf("got %d rows, want 1", len(rows))
		}
	})

	t.Run("approver + conflicting category is header-only", func(t *testing.T) {
		rec := doExportCSV(s, "?approver=alice@x&category=plan_generated&include_global=false")
		if rec.Code != http.StatusOK {
			t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
		}
		header, rows := parseCSV(t, rec.Body.Bytes())
		if len(rows) != 0 {
			t.Fatalf("got %d rows, want 0 (approval_submitted AND plan_generated is empty)", len(rows))
		}
		if len(header) != len(csvExportHeader) {
			t.Errorf("header = %v, want the full column set even when empty", header)
		}
	})
}

// ---------------------------------------------------------------------
// (d) PARITY: CSV rows are a field-for-field projection of the JSON
// export for the identical run-level filter set.
// ---------------------------------------------------------------------

func TestAuditExportCSV_ParityWithJSON(t *testing.T) {
	fr := newFakeRepo()
	// runA newer than runB → created_at DESC page order is [runA, runB].
	runA := seedExportRun(fr, "acme/app", time.Date(2026, 5, 10, 12, 0, 0, 0, time.UTC))
	runB := seedExportRun(fr, "acme/app", time.Date(2026, 5, 5, 12, 0, 0, 0, time.UTC))
	other := seedExportRun(fr, "other/repo", time.Date(2026, 5, 8, 12, 0, 0, 0, time.UTC))
	af := &exportAuditFake{perRun: map[uuid.UUID][]*audit.Entry{
		runA:  chainEntries(t, &runA, "run_created", "plan_generated", "plan_approved"),
		runB:  chainEntries(t, &runB, "run_created", "implement_reviewed"),
		other: chainEntries(t, &other, "run_created"),
	}}
	s := New(Config{AuditRepo: af, RunRepo: fr, SigningRepo: &exportSigningFake{}})

	filterQ := "?repo=acme/app&from=2026-05-01T00:00:00Z&to=2026-05-31T00:00:00Z&include_global=false"

	// JSON export → expected rows in page order (runA then runB), each
	// run's entries sequence-ascending.
	jsonRec := doExport(s, filterQ)
	if jsonRec.Code != http.StatusOK {
		t.Fatalf("json status %d: %s", jsonRec.Code, jsonRec.Body.String())
	}
	ex := strictDecodeAndVerify(t, jsonRec.Body.Bytes())

	type wantRow struct {
		ts, runID, repo, category, actorKind, actorSubject, sequence, entryHash string
	}
	buildWant := func(runID uuid.UUID, repo string) []wantRow {
		rd, ok := ex.Runs[runID.String()]
		if !ok {
			t.Fatalf("run %s missing from JSON export", runID)
		}
		var w []wantRow
		for _, e := range rd.AuditEntries {
			ak := ""
			if e.ActorKind != nil {
				ak = *e.ActorKind
			}
			as := ""
			if e.ActorSubject != nil {
				as = *e.ActorSubject
			}
			w = append(w, wantRow{
				ts:           e.Timestamp.UTC().Format("2006-01-02T15:04:05Z07:00"),
				runID:        runID.String(),
				repo:         repo,
				category:     e.Category,
				actorKind:    ak,
				actorSubject: as,
				sequence:     fmt.Sprintf("%d", e.Sequence),
				entryHash:    e.EntryHash,
			})
		}
		return w
	}
	want := append(buildWant(runA, "acme/app"), buildWant(runB, "acme/app")...)

	// CSV export → actual rows.
	csvRec := doExportCSV(s, filterQ)
	if csvRec.Code != http.StatusOK {
		t.Fatalf("csv status %d: %s", csvRec.Code, csvRec.Body.String())
	}
	header, rows := parseCSV(t, csvRec.Body.Bytes())
	if len(rows) != len(want) {
		t.Fatalf("csv has %d rows, want %d (parity with JSON)", len(rows), len(want))
	}
	idx := func(name string) int { return col(t, header, name) }
	for i, wr := range want {
		got := rows[i]
		checks := []struct{ field, got, want string }{
			{"ts", got[idx("ts")], wr.ts},
			{"run_id", got[idx("run_id")], wr.runID},
			{"repo", got[idx("repo")], wr.repo},
			{"category", got[idx("category")], wr.category},
			{"actor_kind", got[idx("actor_kind")], wr.actorKind},
			{"actor_subject", got[idx("actor_subject")], wr.actorSubject},
			{"sequence", got[idx("sequence")], wr.sequence},
			{"entry_hash", got[idx("entry_hash")], wr.entryHash},
		}
		for _, c := range checks {
			if c.got != c.want {
				t.Errorf("row %d %s = %q, want %q (CSV must be a field-for-field projection of JSON)", i, c.field, c.got, c.want)
			}
		}
	}
}

// ---------------------------------------------------------------------
// (e) continuation round-trip: limit=1 over 3 runs; union of pages'
// rows equals the unpaginated export; global rows on the first page only.
// ---------------------------------------------------------------------

func TestAuditExportCSV_Continuation(t *testing.T) {
	fr := newFakeRepo()
	ids := []uuid.UUID{
		seedExportRun(fr, "acme/app", time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)),
		seedExportRun(fr, "acme/app", time.Date(2026, 5, 1, 11, 0, 0, 0, time.UTC)),
		seedExportRun(fr, "acme/app", time.Date(2026, 5, 1, 10, 0, 0, 0, time.UTC)),
	}
	perRun := map[uuid.UUID][]*audit.Entry{}
	for _, id := range ids {
		perRun[id] = chainEntries(t, &id, "run_created", "plan_generated")
	}
	af := &exportAuditFake{perRun: perRun, global: chainEntries(t, nil, "token_issued")}
	s := New(Config{AuditRepo: af, RunRepo: fr, SigningRepo: &exportSigningFake{}})

	// Unpaginated reference.
	full := doExportCSV(s, "")
	if full.Code != http.StatusOK {
		t.Fatalf("full status %d: %s", full.Code, full.Body.String())
	}
	_, fullRows := parseCSV(t, full.Body.Bytes())

	// Paginated union.
	var pagedRows [][]string
	cursor := ""
	pages := 0
	globalRows := 0
	header, _ := parseCSV(t, full.Body.Bytes())
	runIDCol := col(t, header, "run_id")
	for {
		pages++
		if pages > 10 {
			t.Fatal("continuation did not terminate")
		}
		q := "?limit=1"
		if cursor != "" {
			q += "&cursor=" + cursor
		}
		rec := doExportCSV(s, q)
		if rec.Code != http.StatusOK {
			t.Fatalf("page %d status %d: %s", pages, rec.Code, rec.Body.String())
		}
		_, rows := parseCSV(t, rec.Body.Bytes())
		for _, row := range rows {
			pagedRows = append(pagedRows, row)
			if row[runIDCol] == "" {
				globalRows++
			}
		}
		complete := rec.Header().Get("X-Fishhawk-Export-Complete")
		next := rec.Header().Get("X-Fishhawk-Export-Next-Cursor")
		if complete == "true" {
			if next != "" {
				t.Errorf("final page carries a next cursor %q", next)
			}
			break
		}
		if complete != "false" || next == "" {
			t.Fatalf("partial page %d: complete=%q next=%q", pages, complete, next)
		}
		cursor = next
	}
	if pages != 3 {
		t.Errorf("got %d pages, want 3 (limit=1 over 3 runs)", pages)
	}
	if globalRows != 1 {
		t.Errorf("global rows across pages = %d, want 1 (first page only)", globalRows)
	}
	if len(pagedRows) != len(fullRows) {
		t.Fatalf("paginated union has %d rows, full export has %d", len(pagedRows), len(fullRows))
	}
	// Set equality of the row multisets.
	key := func(r []string) string { return strings.Join(r, "\x00") }
	seen := map[string]int{}
	for _, r := range fullRows {
		seen[key(r)]++
	}
	for _, r := range pagedRows {
		seen[key(r)]--
	}
	for k, n := range seen {
		if n != 0 {
			t.Errorf("row multiset mismatch (count %d) for %q", n, k)
		}
	}
}

// ---------------------------------------------------------------------
// (f) failure modes, one behavioral assertion each.
// ---------------------------------------------------------------------

func TestAuditExportCSV_Unconfigured(t *testing.T) {
	af := &exportAuditFake{}
	fr := newFakeRepo()
	sf := &exportSigningFake{}
	cases := []struct {
		name string
		cfg  Config
	}{
		{"nil audit", Config{RunRepo: fr, SigningRepo: sf}},
		{"nil run", Config{AuditRepo: af, SigningRepo: sf}},
		{"nil signing", Config{AuditRepo: af, RunRepo: fr}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := New(tc.cfg)
			rec := doExportCSV(s, "")
			if rec.Code != http.StatusServiceUnavailable {
				t.Fatalf("status = %d, want 503; body %s", rec.Code, rec.Body.String())
			}
			if !strings.Contains(rec.Body.String(), "audit_export_unconfigured") {
				t.Errorf("body = %s, want audit_export_unconfigured", rec.Body.String())
			}
		})
	}
}

func TestAuditExportCSV_MutualExclusion(t *testing.T) {
	s := configuredExportServer(&exportAuditFake{}, newFakeRepo(), &exportSigningFake{})
	rec := doExportCSV(s, "?run_id="+uuid.New().String()+"&repo=acme/app")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "validation_failed") {
		t.Errorf("body = %s, want validation_failed", rec.Body.String())
	}
}

func TestAuditExportCSV_BadDateAndLimit(t *testing.T) {
	s := configuredExportServer(&exportAuditFake{}, newFakeRepo(), &exportSigningFake{})
	cases := []struct{ name, query string }{
		{"bad from", "?from=not-a-time"},
		{"bad to", "?to=nope"},
		{"from after to", "?from=2026-05-02T00:00:00Z&to=2026-05-01T00:00:00Z"},
		{"limit zero", "?limit=0"},
		{"limit over max", "?limit=9999"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rec := doExportCSV(s, tc.query)
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400; body %s", rec.Code, rec.Body.String())
			}
			if !strings.Contains(rec.Body.String(), "validation_failed") {
				t.Errorf("body = %s, want validation_failed", rec.Body.String())
			}
		})
	}
}

func TestAuditExportCSV_MalformedCursor(t *testing.T) {
	s := configuredExportServer(&exportAuditFake{}, newFakeRepo(), &exportSigningFake{})
	rec := doExportCSV(s, "?cursor=not!base64!")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "cursor_invalid") {
		t.Errorf("body = %s, want cursor_invalid", rec.Body.String())
	}
}

func TestAuditExportCSV_RunNotFound(t *testing.T) {
	s := configuredExportServer(&exportAuditFake{}, newFakeRepo(), &exportSigningFake{})
	rec := doExportCSV(s, "?run_id="+uuid.New().String())
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "run_not_found") {
		t.Errorf("body = %s, want run_not_found", rec.Body.String())
	}
}

// Assembly failure → 500 with a clean JSON error and ZERO partial CSV
// bytes in the body (locks the buffer-before-write contract).
func TestAuditExportCSV_AssembleErrorNoPartialCSV(t *testing.T) {
	fr := newFakeRepo()
	id := seedExportRun(fr, "acme/app", time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC))
	af := &exportAuditFake{
		perRun:        map[uuid.UUID][]*audit.Entry{id: chainEntries(t, &id, "run_created")},
		listForRunErr: errors.New("boom"),
	}
	s := New(Config{AuditRepo: af, RunRepo: fr, SigningRepo: &exportSigningFake{}})
	rec := doExportCSV(s, "?include_global=false")
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500; body %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, "internal_error") {
		t.Errorf("body = %s, want internal_error", body)
	}
	// No CSV escaped the server: neither the header nor any row.
	if strings.Contains(body, "ts,run_id,repo") {
		t.Errorf("partial CSV leaked into the error body: %s", body)
	}
	if ct := rec.Header().Get("Content-Type"); strings.HasPrefix(ct, "text/csv") {
		t.Errorf("Content-Type = %q, want a JSON error content-type (no CSV)", ct)
	}
}

// A ListGlobal error also surfaces as 500 with no partial CSV.
func TestAuditExportCSV_GlobalErrorNoPartialCSV(t *testing.T) {
	fr := newFakeRepo()
	id := seedExportRun(fr, "acme/app", time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC))
	af := &exportAuditFake{
		perRun:        map[uuid.UUID][]*audit.Entry{id: chainEntries(t, &id, "run_created")},
		listGlobalErr: errors.New("global boom"),
	}
	s := New(Config{AuditRepo: af, RunRepo: fr, SigningRepo: &exportSigningFake{}})
	rec := doExportCSV(s, "")
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500; body %s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "ts,run_id,repo") {
		t.Errorf("partial CSV leaked into the error body: %s", rec.Body.String())
	}
}

// ---------------------------------------------------------------------
// (g) headers: Content-Type, Content-Disposition filename derived from
// the injected nowFunc, continuation headers.
// ---------------------------------------------------------------------

func TestAuditExportCSV_Headers(t *testing.T) {
	fr := newFakeRepo()
	ids := []uuid.UUID{
		seedExportRun(fr, "acme/app", time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC)),
		seedExportRun(fr, "acme/app", time.Date(2026, 5, 1, 11, 0, 0, 0, time.UTC)),
	}
	perRun := map[uuid.UUID][]*audit.Entry{}
	for _, id := range ids {
		perRun[id] = chainEntries(t, &id, "run_created")
	}
	af := &exportAuditFake{perRun: perRun}
	s := New(Config{AuditRepo: af, RunRepo: fr, SigningRepo: &exportSigningFake{}})
	s.nowFunc = func() time.Time { return time.Date(2026, 7, 2, 14, 45, 0, 0, time.UTC) }

	t.Run("success headers on a complete export", func(t *testing.T) {
		rec := doExportCSV(s, "?include_global=false")
		if rec.Code != http.StatusOK {
			t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
		}
		if ct := rec.Header().Get("Content-Type"); ct != "text/csv; charset=utf-8" {
			t.Errorf("Content-Type = %q", ct)
		}
		wantCD := `attachment; filename="fishhawk-audit-export-20260702T144500Z.csv"`
		if cd := rec.Header().Get("Content-Disposition"); cd != wantCD {
			t.Errorf("Content-Disposition = %q, want %q", cd, wantCD)
		}
		if c := rec.Header().Get("X-Fishhawk-Export-Complete"); c != "true" {
			t.Errorf("Complete = %q, want true", c)
		}
		if n := rec.Header().Get("X-Fishhawk-Export-Next-Cursor"); n != "" {
			t.Errorf("Next-Cursor = %q, want empty on complete", n)
		}
	})

	t.Run("continuation headers on a partial page", func(t *testing.T) {
		rec := doExportCSV(s, "?limit=1&include_global=false")
		if c := rec.Header().Get("X-Fishhawk-Export-Complete"); c != "false" {
			t.Errorf("Complete = %q, want false", c)
		}
		if n := rec.Header().Get("X-Fishhawk-Export-Next-Cursor"); n == "" {
			t.Error("partial page must carry a next cursor")
		}
	})
}

// ---------------------------------------------------------------------
// (h) route registered.
// ---------------------------------------------------------------------

func TestAuditExportCSVRouteRegistered(t *testing.T) {
	s := New(Config{})
	rec := doExportCSV(s, "")
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 (route reaches handler)", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "audit_export_unconfigured") {
		t.Errorf("body = %s, want audit_export_unconfigured", rec.Body.String())
	}
}

// ---------------------------------------------------------------------
// (i) include_global=false omits global rows; nil actor fields render as
// empty cells.
// ---------------------------------------------------------------------

func TestAuditExportCSV_GlobalOmittedAndNilActors(t *testing.T) {
	fr := newFakeRepo()
	id := seedExportRun(fr, "acme/app", time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC))
	af := &exportAuditFake{
		perRun: map[uuid.UUID][]*audit.Entry{
			// Second entry has no actor (subject == "") → nil actor cells.
			id: chainCSVEntries(t, &id,
				csvSeedEntry{category: "run_created", subject: "u@x", payload: `{"i":0}`},
				csvSeedEntry{category: "note", subject: "", payload: `{"i":1}`},
			),
		},
		global: chainCSVEntries(t, nil,
			csvSeedEntry{category: "token_issued", subject: "sys@x", payload: `{"g":0}`},
		),
	}
	s := New(Config{AuditRepo: af, RunRepo: fr, SigningRepo: &exportSigningFake{}})

	t.Run("include_global=false omits global rows", func(t *testing.T) {
		rec := doExportCSV(s, "?include_global=false")
		header, rows := parseCSV(t, rec.Body.Bytes())
		runIDCol := col(t, header, "run_id")
		for _, row := range rows {
			if row[runIDCol] == "" {
				t.Errorf("global row present despite include_global=false: %v", row)
			}
		}
		if len(rows) != 2 {
			t.Errorf("got %d rows, want 2 run rows", len(rows))
		}
	})

	t.Run("nil actor fields render as empty cells", func(t *testing.T) {
		rec := doExportCSV(s, "?include_global=false")
		header, rows := parseCSV(t, rec.Body.Bytes())
		akCol := col(t, header, "actor_kind")
		asCol := col(t, header, "actor_subject")
		// The second (note) entry has nil actor.
		var noteRow []string
		catCol := col(t, header, "category")
		for _, row := range rows {
			if row[catCol] == "note" {
				noteRow = row
			}
		}
		if noteRow == nil {
			t.Fatal("note row missing")
		}
		if noteRow[akCol] != "" || noteRow[asCol] != "" {
			t.Errorf("nil actor row = kind %q subject %q, want empty cells", noteRow[akCol], noteRow[asCol])
		}
	})

	t.Run("global rows carry empty run_id and repo cells", func(t *testing.T) {
		rec := doExportCSV(s, "")
		header, rows := parseCSV(t, rec.Body.Bytes())
		runIDCol := col(t, header, "run_id")
		repoCol := col(t, header, "repo")
		catCol := col(t, header, "category")
		var found bool
		for _, row := range rows {
			if row[catCol] == "token_issued" {
				found = true
				if row[runIDCol] != "" || row[repoCol] != "" {
					t.Errorf("global row has run_id %q repo %q, want empty", row[runIDCol], row[repoCol])
				}
			}
		}
		if !found {
			t.Error("global token_issued row missing")
		}
	})
}
