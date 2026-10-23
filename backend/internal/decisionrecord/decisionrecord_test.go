package decisionrecord

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/kuhlman-labs/fishhawk/backend/internal/timescale"
)

// rec builds one index record as a JSON object. Each parse fixture below
// differs from a valid index in exactly ONE field, so no other rung refuses it
// and a deleted rung shows up as a clean parse.
func rec(id, path, status string, appliesTo ...string) map[string]any {
	if appliesTo == nil {
		appliesTo = []string{}
	}
	return map[string]any{
		"id":            id,
		"title":         "title of " + id,
		"path":          path,
		"status":        status,
		"supersedes":    []string{},
		"superseded_by": []string{},
		"applies_to":    appliesTo,
	}
}

// linked sets a record's supersession links.
func linked(r map[string]any, supersedes, supersededBy []string) map[string]any {
	if supersedes == nil {
		supersedes = []string{}
	}
	if supersededBy == nil {
		supersededBy = []string{}
	}
	r["supersedes"] = supersedes
	r["superseded_by"] = supersededBy
	return r
}

// amending sets a record's OPTIONAL amendment links; a nil list leaves the key
// absent, which is how scripts/check-adr writes an unlinked record.
func amending(r map[string]any, amends, amendedBy []string) map[string]any {
	if amends != nil {
		r["amends"] = amends
	}
	if amendedBy != nil {
		r["amended_by"] = amendedBy
	}
	return r
}

func indexJSON(t *testing.T, recs ...map[string]any) []byte {
	t.Helper()
	if recs == nil {
		recs = []map[string]any{}
	}
	raw, err := json.Marshal(map[string]any{"schema_version": IndexSchemaVersion, "records": recs})
	if err != nil {
		t.Fatalf("marshal index: %v", err)
	}
	return raw
}

func baseRecords() []map[string]any {
	return []map[string]any{
		rec("ADR-001", "docs/adr/001-a.md", "accepted", "backend/internal/audit/**"),
		rec("ADR-002", "docs/adr/002-b.md", "unknown"),
	}
}

func TestParseIndex_AcceptsTheShippedIndex(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "..", "docs", "adr", "index.json"))
	if err != nil {
		t.Fatalf("read shipped index: %v", err)
	}
	idx, err := ParseIndex(raw)
	if err != nil {
		t.Fatalf("ParseIndex(docs/adr/index.json): %v — the strict parser must accept the index scripts/check-adr generates", err)
	}
	if len(idx.Records) == 0 {
		t.Fatal("shipped index parsed to zero records")
	}
}

func TestParseIndex_Valid(t *testing.T) {
	idx, err := ParseIndex(indexJSON(t, baseRecords()...))
	if err != nil {
		t.Fatalf("ParseIndex: %v", err)
	}
	if len(idx.Records) != 2 || idx.Records[0].ID != "ADR-001" || idx.Records[1].Status != StatusUnknown {
		t.Fatalf("parsed = %+v", idx)
	}
	if !reflect.DeepEqual(idx.Records[0].AppliesTo, []string{"backend/internal/audit/**"}) {
		t.Errorf("applies_to = %v", idx.Records[0].AppliesTo)
	}
	if idx.Records[0].Amends != nil || idx.Records[0].AmendedBy != nil {
		t.Errorf("absent amends / amended_by decoded as %v / %v, want empty", idx.Records[0].Amends, idx.Records[0].AmendedBy)
	}
}

// TestParseIndex_Valid_AmendmentKeys: the optional amends / amended_by keys
// are accepted when present (on the two linked records only) and when present
// but empty.
func TestParseIndex_Valid_AmendmentKeys(t *testing.T) {
	idx, err := ParseIndex(indexJSON(t,
		amending(rec("ADR-001", "docs/adr/001-a.md", "accepted", "backend/internal/audit/**"), nil, []string{"ADR-002"}),
		amending(rec("ADR-002", "docs/adr/002-b.md", "accepted"), []string{"ADR-001"}, nil),
		amending(rec("ADR-003", "docs/adr/003-c.md", "accepted"), []string{}, []string{}),
	))
	if err != nil {
		t.Fatalf("ParseIndex: %v", err)
	}
	if !slices.Equal(idx.Records[0].AmendedBy, []string{"ADR-002"}) || len(idx.Records[0].Amends) != 0 {
		t.Errorf("ADR-001 amends %v amended_by %v, want [] / [ADR-002]", idx.Records[0].Amends, idx.Records[0].AmendedBy)
	}
	if !slices.Equal(idx.Records[1].Amends, []string{"ADR-001"}) || len(idx.Records[1].AmendedBy) != 0 {
		t.Errorf("ADR-002 amends %v amended_by %v, want [ADR-001] / []", idx.Records[1].Amends, idx.Records[1].AmendedBy)
	}
}

// TestParseIndex_FailsClosed is one case per refusal rung. Each case asserts
// ErrInvalidIndex AND, for a record-level rung, that the offending record is
// named.
func TestParseIndex_FailsClosed(t *testing.T) {
	withRecord := func(field string, value any) func(t *testing.T) []byte {
		return func(t *testing.T) []byte {
			recs := baseRecords()
			recs[1][field] = value
			return indexJSON(t, recs...)
		}
	}
	withRecords := func(mutate func(recs []map[string]any)) func(t *testing.T) []byte {
		return func(t *testing.T) []byte {
			recs := baseRecords()
			mutate(recs)
			return indexJSON(t, recs...)
		}
	}
	cases := []struct {
		name  string
		raw   func(t *testing.T) []byte
		names string // substring the error must carry; "" = no record named
	}{
		{"malformed JSON", func(*testing.T) []byte { return []byte(`{"schema_version": "adr-index-v1", "records": [`) }, ""},
		{"trailing data", func(t *testing.T) []byte { return append(indexJSON(t, baseRecords()...), []byte(`{}`)...) }, "trailing"},
		{"wrong schema_version", func(*testing.T) []byte { return []byte(`{"schema_version": "adr-index-v2", "records": []}`) }, "adr-index-v2"},
		{"unknown top-level field", func(*testing.T) []byte {
			return []byte(`{"schema_version": "adr-index-v1", "records": [], "extra": 1}`)
		}, "extra"},
		{"unknown record field", withRecord("extra", "x"), "extra"},
		{"bad id", withRecord("id", "ADR-02"), "records[1]"},
		{"lowercase id", withRecord("id", "adr-002"), "records[1]"},
		{"absolute path", withRecord("path", "/docs/adr/002-b.md"), "records[1]"},
		{"dot-dot path", withRecord("path", "docs/../adr/002-b.md"), "records[1]"},
		{"control-character path", withRecord("path", "docs/adr/002\n-b.md"), "records[1]"},
		{"unknown status", withRecord("status", "settled"), "records[1]"},
		{"empty status", withRecord("status", ""), "records[1]"},
		{"bad superseded_by id", withRecord("superseded_by", []string{"ADR-x"}), "records[1]"},
		{"bad supersedes id", withRecord("supersedes", []string{"001"}), "records[1]"},
		{"malformed applies_to glob", withRecord("applies_to", []string{"backend/[x"}), "records[1]"},
		{"empty applies_to glob", withRecord("applies_to", []string{""}), "records[1]"},
		{"duplicate id", withRecord("id", "ADR-001"), "duplicate id"},
		{"duplicate path", withRecord("path", "docs/adr/001-a.md"), "duplicate path"},
		// The four link-integrity fixtures carry well-formed ids, so only
		// checkLinks refuses them.
		{"dangling superseded_by", withRecord("superseded_by", []string{"ADR-099"}), `records[1] (id "ADR-002"): superseded_by[0] "ADR-099" names no record in the index`},
		{"dangling supersedes", withRecord("supersedes", []string{"ADR-099"}), `records[1] (id "ADR-002"): supersedes[0] "ADR-099" names no record in the index`},
		{"self superseded_by", withRecord("superseded_by", []string{"ADR-002"}), `records[1] (id "ADR-002"): superseded_by[0] "ADR-002" names the record itself`},
		{"self supersedes", withRecord("supersedes", []string{"ADR-002"}), `records[1] (id "ADR-002"): supersedes[0] "ADR-002" names the record itself`},
		// The optional amendment keys (E78.13 / #4471) take the same rungs.
		{"amends not a list", withRecord("amends", "ADR-001"), "amends"},
		{"bad amends id", withRecord("amends", []string{"ADR-x"}), `records[1] (id "ADR-002"): amends[0] "ADR-x" does not match`},
		{"bad amended_by id", withRecord("amended_by", []string{"001"}), `records[1] (id "ADR-002"): amended_by[0] "001" does not match`},
		{"dangling amends", withRecord("amends", []string{"ADR-099"}), `records[1] (id "ADR-002"): amends[0] "ADR-099" names no record in the index`},
		{"dangling amended_by", withRecord("amended_by", []string{"ADR-099"}), `records[1] (id "ADR-002"): amended_by[0] "ADR-099" names no record in the index`},
		{"self amends", withRecord("amends", []string{"ADR-002"}), `records[1] (id "ADR-002"): amends[0] "ADR-002" names the record itself`},
		{"self amended_by", withRecord("amended_by", []string{"ADR-002"}), `records[1] (id "ADR-002"): amended_by[0] "ADR-002" names the record itself`},
		// Exclusivity: the target exists and is not the record itself, so
		// only the both-relations check refuses these.
		{"same id in supersedes and amends", withRecords(func(recs []map[string]any) {
			recs[1]["supersedes"] = []string{"ADR-001"}
			recs[1]["amends"] = []string{"ADR-001"}
		}), `records[1] (id "ADR-002"): "ADR-001" is listed in both supersedes and amends`},
		{"same id in superseded_by and amended_by", withRecords(func(recs []map[string]any) {
			recs[1]["superseded_by"] = []string{"ADR-001"}
			recs[1]["amended_by"] = []string{"ADR-001"}
		}), `records[1] (id "ADR-002"): "ADR-001" is listed in both superseded_by and amended_by`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			idx, err := ParseIndex(tc.raw(t))
			if err == nil {
				t.Fatalf("ParseIndex accepted the fixture: %+v", idx)
			}
			if !errors.Is(err, ErrInvalidIndex) {
				t.Errorf("err = %v, want ErrInvalidIndex", err)
			}
			if tc.names != "" && !strings.Contains(err.Error(), tc.names) {
				t.Errorf("err = %q, want it to name %q", err, tc.names)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Select.
// ---------------------------------------------------------------------------

func mustIndex(t *testing.T, recs ...map[string]any) *Index {
	t.Helper()
	idx, err := ParseIndex(indexJSON(t, recs...))
	if err != nil {
		t.Fatalf("ParseIndex: %v", err)
	}
	return idx
}

func ids(ms []Match) []string {
	out := make([]string, 0, len(ms))
	for _, m := range ms {
		out = append(out, m.Record.ID)
	}
	return out
}

// TestSelect_MatchesOnlyGovernedRecords: R-front is a well-formed non-empty
// record, so only the predicate excludes it; R-empty declares no path, so only
// the empty-applies_to skip keeps it away from Predicate.Match (which errors on
// an empty predicate).
func TestSelect_MatchesOnlyGovernedRecords(t *testing.T) {
	idx := mustIndex(t,
		rec("ADR-010", "docs/adr/010-audit.md", "accepted", "backend/internal/audit/**"),
		rec("ADR-011", "docs/adr/011-front.md", "accepted", "frontend/**"),
		rec("ADR-012", "docs/adr/012-empty.md", "accepted"),
	)
	got, err := Select(idx, []string{"backend/internal/audit/categories.go"})
	if err != nil {
		t.Fatalf("Select: %v", err)
	}
	if !reflect.DeepEqual(ids(got), []string{"ADR-010"}) {
		t.Fatalf("selected %v, want [ADR-010]", ids(got))
	}
	if got[0].Rank != 1 || got[0].MatchedPaths != 1 {
		t.Errorf("match = %+v, want rank 1, 1 matched path", got[0])
	}
}

func TestSelect_NoChangePaths_SelectsNothing(t *testing.T) {
	idx := mustIndex(t, rec("ADR-010", "docs/adr/010-audit.md", "accepted", "**"))
	got, err := Select(idx, nil)
	if err != nil || len(got) != 0 {
		t.Fatalf("Select(nil paths) = %v, %v; want none", ids(got), err)
	}
}

func TestSelect_CountsDistinctChangePaths(t *testing.T) {
	idx := mustIndex(t, rec("ADR-010", "docs/adr/010-audit.md", "accepted", "backend/**"))
	got, err := Select(idx, []string{"backend/a.go", "backend/a.go", "backend/b.go", "cli/c.go"})
	if err != nil {
		t.Fatalf("Select: %v", err)
	}
	if len(got) != 1 || got[0].MatchedPaths != 2 {
		t.Fatalf("got %+v, want ADR-010 matching 2 distinct paths", got)
	}
}

func TestSelect_Ranking(t *testing.T) {
	cases := []struct {
		name    string
		records []map[string]any
		paths   []string
		want    []string
	}{
		{
			name: "accepted before unknown at equal match count",
			records: []map[string]any{
				rec("ADR-020", "docs/adr/020.md", "unknown", "a/**"),
				rec("ADR-005", "docs/adr/005.md", "accepted", "a/**"),
			},
			paths: []string{"a/x.go"},
			want:  []string{"ADR-005", "ADR-020"},
		},
		{
			name: "accepted before a non-accepted record matching more paths",
			records: []map[string]any{
				rec("ADR-020", "docs/adr/020.md", "proposed", "a/**", "b/**"),
				rec("ADR-005", "docs/adr/005.md", "accepted", "a/**"),
			},
			paths: []string{"a/x.go", "b/y.go"},
			want:  []string{"ADR-005", "ADR-020"},
		},
		{
			name: "more matched paths first",
			records: []map[string]any{
				rec("ADR-009", "docs/adr/009.md", "accepted", "a/**"),
				rec("ADR-001", "docs/adr/001.md", "accepted", "a/**", "b/**"),
			},
			paths: []string{"a/x.go", "b/y.go"},
			want:  []string{"ADR-001", "ADR-009"},
		},
		{
			name: "higher record number first",
			records: []map[string]any{
				rec("ADR-003", "docs/adr/003.md", "accepted", "a/**"),
				rec("ADR-1007", "docs/adr/1007.md", "accepted", "a/**"),
				rec("ADR-007", "docs/adr/007.md", "accepted", "a/**"),
			},
			paths: []string{"a/x.go"},
			want:  []string{"ADR-1007", "ADR-007", "ADR-003"},
		},
		{
			name: "id ascending on an equal number",
			records: []map[string]any{
				rec("ADR-007", "docs/adr/007.md", "accepted", "a/**"),
				rec("ADR-0007", "docs/adr/0007.md", "accepted", "a/**"),
			},
			paths: []string{"a/x.go"},
			want:  []string{"ADR-0007", "ADR-007"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Select(mustIndex(t, tc.records...), tc.paths)
			if err != nil {
				t.Fatalf("Select: %v", err)
			}
			if !reflect.DeepEqual(ids(got), tc.want) {
				t.Fatalf("ranked %v, want %v", ids(got), tc.want)
			}
			for i, m := range got {
				if m.Rank != i+1 {
					t.Errorf("%s rank = %d, want %d", m.Record.ID, m.Rank, i+1)
				}
			}
		})
	}
}

// TestSelect_MatchErrorFailsWholeSelection builds the index by hand (bypassing
// ParseIndex) so a malformed glob reaches Select: it must fail the selection,
// never read as a non-match.
func TestSelect_MatchErrorFailsWholeSelection(t *testing.T) {
	idx := &Index{SchemaVersion: IndexSchemaVersion, Records: []Record{
		{ID: "ADR-001", Path: "docs/adr/001.md", Status: StatusAccepted, AppliesTo: []string{"a/**"}},
		{ID: "ADR-002", Path: "docs/adr/002.md", Status: StatusAccepted, AppliesTo: []string{"a/[x"}},
	}}
	got, err := Select(idx, []string{"a/x.go"})
	if !errors.Is(err, ErrInvalidIndex) || got != nil {
		t.Fatalf("Select = %v, %v; want nil, ErrInvalidIndex", ids(got), err)
	}
	if !strings.Contains(err.Error(), "ADR-002") {
		t.Errorf("err = %q, want it to name ADR-002", err)
	}
	if _, err := Select(nil, []string{"a"}); !errors.Is(err, ErrInvalidIndex) {
		t.Errorf("Select(nil) err = %v, want ErrInvalidIndex", err)
	}
}

// ---------------------------------------------------------------------------
// Supersession following.
// ---------------------------------------------------------------------------

// wantMatch is one expected selected record, in rank order.
type wantMatch struct {
	id        string
	via       []string
	matched   int
	rankPaths int
}

// TestSelect_FollowsSupersessionToSuccessors: every successor fixture has an
// EMPTY applies_to (unless the case says otherwise), so only following can
// select it.
func TestSelect_FollowsSupersessionToSuccessors(t *testing.T) {
	cases := []struct {
		name    string
		records []map[string]any
		paths   []string
		want    []wantMatch
	}{
		{
			name: "one hop via superseded_by",
			records: []map[string]any{
				linked(rec("ADR-001", "docs/adr/001.md", "accepted", "a/**"), nil, []string{"ADR-002"}),
				rec("ADR-002", "docs/adr/002.md", "accepted"),
			},
			paths: []string{"a/x.go"},
			want:  []wantMatch{{"ADR-002", []string{"ADR-001"}, 0, 1}, {"ADR-001", nil, 1, 1}},
		},
		{
			name: "one hop via the successor's supersedes only (non-reciprocal)",
			records: []map[string]any{
				rec("ADR-001", "docs/adr/001.md", "accepted", "a/**"),
				linked(rec("ADR-002", "docs/adr/002.md", "accepted"), []string{"ADR-001"}, nil),
			},
			paths: []string{"a/x.go"},
			want:  []wantMatch{{"ADR-002", []string{"ADR-001"}, 0, 1}, {"ADR-001", nil, 1, 1}},
		},
		{
			name: "transitive two-hop chain",
			records: []map[string]any{
				linked(rec("ADR-001", "docs/adr/001.md", "accepted", "a/**"), nil, []string{"ADR-002"}),
				linked(rec("ADR-002", "docs/adr/002.md", "superseded"), []string{"ADR-001"}, []string{"ADR-003"}),
				linked(rec("ADR-003", "docs/adr/003.md", "accepted"), []string{"ADR-002"}, nil),
			},
			paths: []string{"a/x.go"},
			want: []wantMatch{
				{"ADR-003", []string{"ADR-002"}, 0, 1},
				{"ADR-001", nil, 1, 1},
				{"ADR-002", []string{"ADR-001"}, 0, 1},
			},
		},
		{
			name: "a successor that also matches directly appears once",
			records: []map[string]any{
				linked(rec("ADR-001", "docs/adr/001.md", "accepted", "a/**"), nil, []string{"ADR-002"}),
				linked(rec("ADR-002", "docs/adr/002.md", "accepted", "a/**", "b/**"), []string{"ADR-001"}, nil),
			},
			paths: []string{"a/x.go", "b/y.go"},
			want:  []wantMatch{{"ADR-002", []string{"ADR-001"}, 2, 2}, {"ADR-001", nil, 1, 1}},
		},
		{
			name: "diamond: two selected predecessors share one successor",
			records: []map[string]any{
				linked(rec("ADR-001", "docs/adr/001.md", "accepted", "a/**"), nil, []string{"ADR-003"}),
				linked(rec("ADR-002", "docs/adr/002.md", "accepted", "a/**"), nil, []string{"ADR-003"}),
				linked(rec("ADR-003", "docs/adr/003.md", "accepted"), []string{"ADR-001", "ADR-002"}, nil),
			},
			paths: []string{"a/x.go"},
			want: []wantMatch{
				{"ADR-003", []string{"ADR-001", "ADR-002"}, 0, 1},
				{"ADR-002", nil, 1, 1},
				{"ADR-001", nil, 1, 1},
			},
		},
		{
			// ADR-001 matches directly (so the walk runs); ADR-002 does not,
			// so only seeding the walk from direct matches keeps ADR-003 out.
			name: "a successor of an unselected record is not selected",
			records: []map[string]any{
				rec("ADR-001", "docs/adr/001.md", "accepted", "a/**"),
				linked(rec("ADR-002", "docs/adr/002.md", "accepted", "b/**"), nil, []string{"ADR-003"}),
				linked(rec("ADR-003", "docs/adr/003.md", "accepted"), []string{"ADR-002"}, nil),
			},
			paths: []string{"a/x.go"},
			want:  []wantMatch{{"ADR-001", nil, 1, 1}},
		},
		{
			// Without inheritance S ranks by 0 below P (2) and Q (1): P, Q, S.
			name: "a successor inherits its predecessor's rank",
			records: []map[string]any{
				linked(rec("ADR-001", "docs/adr/001.md", "accepted", "a/**", "b/**"), nil, []string{"ADR-002"}),
				linked(rec("ADR-002", "docs/adr/002.md", "accepted"), []string{"ADR-001"}, nil),
				rec("ADR-003", "docs/adr/003.md", "accepted", "c/**"),
			},
			paths: []string{"a/x.go", "b/y.go", "c/z.go"},
			want: []wantMatch{
				{"ADR-002", []string{"ADR-001"}, 0, 2},
				{"ADR-001", nil, 2, 2},
				{"ADR-003", nil, 1, 1},
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Select(mustIndex(t, tc.records...), tc.paths)
			if err != nil {
				t.Fatalf("Select: %v", err)
			}
			wantIDs := make([]string, 0, len(tc.want))
			for _, w := range tc.want {
				wantIDs = append(wantIDs, w.id)
			}
			if !reflect.DeepEqual(ids(got), wantIDs) {
				t.Fatalf("selected %v, want %v", ids(got), wantIDs)
			}
			for i, w := range tc.want {
				m := got[i]
				if !slices.Equal(m.Via, w.via) || m.MatchedPaths != w.matched || m.RankPaths != w.rankPaths || m.Rank != i+1 {
					t.Errorf("%s = {via %v matched %d rankPaths %d rank %d}, want {via %v matched %d rankPaths %d rank %d}",
						w.id, m.Via, m.MatchedPaths, m.RankPaths, m.Rank, w.via, w.matched, w.rankPaths, i+1)
				}
			}
		})
	}
}

// TestSelect_SupersessionCycleTerminates: a hand-built A<->B cycle (ParseIndex
// accepts it — each link names another existing record) must not loop.
func TestSelect_SupersessionCycleTerminates(t *testing.T) {
	idx := &Index{SchemaVersion: IndexSchemaVersion, Records: []Record{
		{ID: "ADR-001", Path: "docs/adr/001.md", Status: StatusAccepted, AppliesTo: []string{"a/**"}, SupersededBy: []string{"ADR-002"}},
		{ID: "ADR-002", Path: "docs/adr/002.md", Status: StatusAccepted, SupersededBy: []string{"ADR-001"}},
	}}
	type result struct {
		ms  []Match
		err error
	}
	done := make(chan result, 1)
	go func() {
		ms, err := Select(idx, []string{"a/x.go"})
		done <- result{ms, err}
	}()
	select {
	case r := <-done:
		if r.err != nil {
			t.Fatalf("Select: %v", r.err)
		}
		got := ids(r.ms)
		slices.Sort(got)
		if !slices.Equal(got, []string{"ADR-001", "ADR-002"}) {
			t.Fatalf("selected %v, want each of ADR-001, ADR-002 once", ids(r.ms))
		}
	case <-time.After(timescale.D(5 * time.Second)):
		t.Fatal("Select did not return on a supersession cycle")
	}
}

// TestSelect_DanglingLinkFailsWholeSelection builds the index by hand
// (bypassing ParseIndex, which would refuse it) so Select's own guard is the
// only refusal in the path.
func TestSelect_DanglingLinkFailsWholeSelection(t *testing.T) {
	cases := []struct {
		name  string
		first Record
		want  string
	}{
		{
			name:  "superseded_by",
			first: Record{ID: "ADR-001", Path: "docs/adr/001.md", Status: StatusAccepted, AppliesTo: []string{"a/**"}, SupersededBy: []string{"ADR-404"}},
			want:  `(id "ADR-001"): superseded_by "ADR-404" names no record`,
		},
		{
			name:  "amended_by",
			first: Record{ID: "ADR-001", Path: "docs/adr/001.md", Status: StatusAccepted, AppliesTo: []string{"a/**"}, AmendedBy: []string{"ADR-404"}},
			want:  `(id "ADR-001"): amended_by "ADR-404" names no record`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			idx := &Index{SchemaVersion: IndexSchemaVersion, Records: []Record{
				tc.first,
				{ID: "ADR-002", Path: "docs/adr/002.md", Status: StatusAccepted, AppliesTo: []string{"a/**"}},
			}}
			got, err := Select(idx, []string{"a/x.go"})
			if !errors.Is(err, ErrInvalidIndex) || got != nil {
				t.Fatalf("Select = %v, %v; want nil, ErrInvalidIndex", ids(got), err)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("err = %q, want it to carry %q", err, tc.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Amendment following (E78.13 / #4471).
// ---------------------------------------------------------------------------

// wantAmend is one expected selected record, in rank order, with both link
// provenance lists.
type wantAmend struct {
	id        string
	via       []string
	amendsVia []string
	matched   int
	rankPaths int
}

// TestSelect_FollowsAmendments: every amendment fixture has an EMPTY
// applies_to (unless the case says otherwise), so only following can select
// it, and every amendment is NEWER than the record it amends, so the
// comparator alone would rank it first: only placeAmendmentsAfterAmended puts
// the amended record ahead.
func TestSelect_FollowsAmendments(t *testing.T) {
	cases := []struct {
		name    string
		records []map[string]any
		paths   []string
		want    []wantAmend
	}{
		{
			name: "one hop via amended_by and the amendment's amends (reciprocal)",
			records: []map[string]any{
				amending(rec("ADR-001", "docs/adr/001.md", "accepted", "a/**"), nil, []string{"ADR-002"}),
				amending(rec("ADR-002", "docs/adr/002.md", "accepted"), []string{"ADR-001"}, nil),
			},
			paths: []string{"a/x.go"},
			want:  []wantAmend{{"ADR-001", nil, nil, 1, 1}, {"ADR-002", nil, []string{"ADR-001"}, 0, 1}},
		},
		{
			name: "one hop via the amendment's amends only (reverse-only, non-reciprocal)",
			records: []map[string]any{
				rec("ADR-001", "docs/adr/001.md", "accepted", "a/**"),
				amending(rec("ADR-002", "docs/adr/002.md", "accepted"), []string{"ADR-001"}, nil),
			},
			paths: []string{"a/x.go"},
			want:  []wantAmend{{"ADR-001", nil, nil, 1, 1}, {"ADR-002", nil, []string{"ADR-001"}, 0, 1}},
		},
		{
			name: "one hop via amended_by only (forward-only, non-reciprocal)",
			records: []map[string]any{
				amending(rec("ADR-001", "docs/adr/001.md", "accepted", "a/**"), nil, []string{"ADR-002"}),
				rec("ADR-002", "docs/adr/002.md", "accepted"),
			},
			paths: []string{"a/x.go"},
			want:  []wantAmend{{"ADR-001", nil, nil, 1, 1}, {"ADR-002", nil, []string{"ADR-001"}, 0, 1}},
		},
		{
			// Without inheritance ADR-002 ranks by 0 below ADR-003 (1).
			name: "an amendment inherits the amended record's rank",
			records: []map[string]any{
				amending(rec("ADR-001", "docs/adr/001.md", "accepted", "a/**", "b/**"), nil, []string{"ADR-002"}),
				amending(rec("ADR-002", "docs/adr/002.md", "accepted"), []string{"ADR-001"}, nil),
				rec("ADR-003", "docs/adr/003.md", "accepted", "c/**"),
			},
			paths: []string{"a/x.go", "b/y.go", "c/z.go"},
			want: []wantAmend{
				{"ADR-001", nil, nil, 2, 2},
				{"ADR-002", nil, []string{"ADR-001"}, 0, 2},
				{"ADR-003", nil, nil, 1, 1},
			},
		},
		{
			// The amendment matches MORE paths directly, so the comparator
			// ranks it first; the amended record still precedes it.
			name: "an amended record ranks ahead of an amendment matching more paths",
			records: []map[string]any{
				amending(rec("ADR-001", "docs/adr/001.md", "accepted", "a/**"), nil, []string{"ADR-002"}),
				amending(rec("ADR-002", "docs/adr/002.md", "accepted", "a/**", "b/**"), []string{"ADR-001"}, nil),
			},
			paths: []string{"a/x.go", "b/y.go"},
			want:  []wantAmend{{"ADR-001", nil, nil, 1, 1}, {"ADR-002", nil, []string{"ADR-001"}, 2, 2}},
		},
		{
			// The comparator puts the accepted amendment ahead of the unknown
			// record; the placement rule keeps the amended record first.
			name: "an amendment ranks after a non-accepted record it amends",
			records: []map[string]any{
				amending(rec("ADR-001", "docs/adr/001.md", "unknown", "a/**"), nil, []string{"ADR-002"}),
				amending(rec("ADR-002", "docs/adr/002.md", "accepted"), []string{"ADR-001"}, nil),
			},
			paths: []string{"a/x.go"},
			want:  []wantAmend{{"ADR-001", nil, nil, 1, 1}, {"ADR-002", nil, []string{"ADR-001"}, 0, 1}},
		},
		{
			name: "an amendment of a successor is followed (mixed chain)",
			records: []map[string]any{
				linked(rec("ADR-001", "docs/adr/001.md", "accepted", "a/**"), nil, []string{"ADR-002"}),
				amending(linked(rec("ADR-002", "docs/adr/002.md", "accepted"), []string{"ADR-001"}, nil), nil, []string{"ADR-003"}),
				amending(rec("ADR-003", "docs/adr/003.md", "accepted"), []string{"ADR-002"}, nil),
			},
			paths: []string{"a/x.go"},
			want: []wantAmend{
				{"ADR-002", []string{"ADR-001"}, nil, 0, 1},
				{"ADR-003", nil, []string{"ADR-002"}, 0, 1},
				{"ADR-001", nil, nil, 1, 1},
			},
		},
		{
			// ADR-002 matches directly; the record it amends (ADR-001) does
			// not, and amended records are never followed.
			name: "the amended record of a directly matched amendment is not followed",
			records: []map[string]any{
				amending(rec("ADR-001", "docs/adr/001.md", "accepted", "b/**"), nil, []string{"ADR-002"}),
				amending(rec("ADR-002", "docs/adr/002.md", "accepted", "a/**"), []string{"ADR-001"}, nil),
			},
			paths: []string{"a/x.go"},
			want:  []wantAmend{{"ADR-002", nil, nil, 1, 1}},
		},
		{
			// The ADR-040 shape: two amendments of one record, the later one
			// also amending the earlier one and a record that is not selected.
			name: "an amendment of an amendment ranks after both",
			records: []map[string]any{
				amending(rec("ADR-040", "docs/adr/040.md", "accepted", ".fishhawk/operator.yaml"), nil, []string{"ADR-097", "ADR-101"}),
				amending(rec("ADR-089", "docs/adr/089.md", "accepted"), nil, []string{"ADR-097", "ADR-101"}),
				amending(rec("ADR-097", "docs/adr/097.md", "accepted"), []string{"ADR-040", "ADR-089"}, []string{"ADR-101"}),
				amending(rec("ADR-101", "docs/adr/101.md", "accepted"), []string{"ADR-040", "ADR-089", "ADR-097"}, nil),
			},
			paths: []string{".fishhawk/operator.yaml"},
			want: []wantAmend{
				{"ADR-040", nil, nil, 1, 1},
				{"ADR-097", nil, []string{"ADR-040"}, 0, 1},
				{"ADR-101", nil, []string{"ADR-040", "ADR-097"}, 0, 1},
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Select(mustIndex(t, tc.records...), tc.paths)
			if err != nil {
				t.Fatalf("Select: %v", err)
			}
			wantIDs := make([]string, 0, len(tc.want))
			for _, w := range tc.want {
				wantIDs = append(wantIDs, w.id)
			}
			if !reflect.DeepEqual(ids(got), wantIDs) {
				t.Fatalf("selected %v, want %v", ids(got), wantIDs)
			}
			for i, w := range tc.want {
				m := got[i]
				if !slices.Equal(m.Via, w.via) || !slices.Equal(m.AmendsVia, w.amendsVia) || m.MatchedPaths != w.matched || m.RankPaths != w.rankPaths || m.Rank != i+1 {
					t.Errorf("%s = {via %v amendsVia %v matched %d rankPaths %d rank %d}, want {via %v amendsVia %v matched %d rankPaths %d rank %d}",
						w.id, m.Via, m.AmendsVia, m.MatchedPaths, m.RankPaths, m.Rank, w.via, w.amendsVia, w.matched, w.rankPaths, i+1)
				}
			}
		})
	}
}

// TestSelect_AmendmentCycleTerminates: a hand-built A<->B amendment cycle
// must not loop, in the walk or in the placement pass (where neither record
// is ever eligible, so the fallback takes the highest-ranked one).
func TestSelect_AmendmentCycleTerminates(t *testing.T) {
	idx := &Index{SchemaVersion: IndexSchemaVersion, Records: []Record{
		{ID: "ADR-001", Path: "docs/adr/001.md", Status: StatusAccepted, AppliesTo: []string{"a/**"}, AmendedBy: []string{"ADR-002"}},
		{ID: "ADR-002", Path: "docs/adr/002.md", Status: StatusAccepted, AmendedBy: []string{"ADR-001"}},
	}}
	type result struct {
		ms  []Match
		err error
	}
	done := make(chan result, 1)
	go func() {
		ms, err := Select(idx, []string{"a/x.go"})
		done <- result{ms, err}
	}()
	select {
	case r := <-done:
		if r.err != nil {
			t.Fatalf("Select: %v", r.err)
		}
		// Comparator order is ADR-002, ADR-001 (newer first); each amends the
		// other, so the fallback keeps that order.
		if got := ids(r.ms); !slices.Equal(got, []string{"ADR-002", "ADR-001"}) {
			t.Fatalf("selected %v, want [ADR-002 ADR-001]", got)
		}
	case <-time.After(timescale.D(5 * time.Second)):
		t.Fatal("Select did not return on an amendment cycle")
	}
}

// TestSelect_ShippedIndexGovernsSupersedingRecordPaths reads the REAL index:
// ADR-098 declares backend/internal/basedelta/** (E86.58 / #4398), and no
// other record's glob matches basedelta, so a change there selects it
// directly. ADR-096, the record it supersedes, is not followed backwards.
func TestSelect_ShippedIndexGovernsSupersedingRecordPaths(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "..", "docs", "adr", "index.json"))
	if err != nil {
		t.Fatalf("read shipped index: %v", err)
	}
	idx, err := ParseIndex(raw)
	if err != nil {
		t.Fatalf("ParseIndex: %v", err)
	}
	got, err := Select(idx, []string{"backend/internal/basedelta/basedelta.go"})
	if err != nil {
		t.Fatalf("Select: %v", err)
	}
	i := slices.IndexFunc(got, func(m Match) bool { return m.Record.ID == "ADR-098" })
	if i < 0 {
		t.Fatalf("selected %v, want ADR-098 (docs/adr/098-*.md applies_to backend/internal/basedelta/**)", ids(got))
	}
	if got[i].MatchedPaths != 1 {
		t.Errorf("ADR-098 matched %d paths, want 1 (a direct match)", got[i].MatchedPaths)
	}
	if slices.Contains(ids(got), "ADR-096") {
		t.Errorf("selected %v: ADR-096 is a predecessor, never followed", ids(got))
	}
}

// fullRecord populates EVERY Record field, the amendment relations included.
func fullRecord() Record {
	return Record{
		ID:           "ADR-002",
		Title:        "Amended & superseding <record>",
		Path:         "docs/adr/002-b.md",
		Status:       StatusAccepted,
		Supersedes:   []string{"ADR-001"},
		SupersededBy: []string{"ADR-004"},
		Amends:       []string{"ADR-003"},
		AmendedBy:    []string{"ADR-005"},
		AppliesTo:    []string{"backend/internal/audit/**"},
	}
}

// TestRenderIndex_RendersEveryRecordField (#4478): a fully populated record's
// line names every json key Record declares, and every slice field's tag
// carries omitempty — so a field added to Record renders, and an empty list
// renders compactly (left out of its line).
func TestRenderIndex_RendersEveryRecordField(t *testing.T) {
	r := fullRecord()
	out, err := RenderIndex(&Index{SchemaVersion: IndexSchemaVersion, Records: []Record{r}})
	if err != nil {
		t.Fatalf("RenderIndex: %v", err)
	}
	rt := reflect.TypeOf(Record{})
	for i := range rt.NumField() {
		f := rt.Field(i)
		tag := f.Tag.Get("json")
		name, opts, _ := strings.Cut(tag, ",")
		v := reflect.ValueOf(r).Field(i)
		if v.IsZero() {
			t.Fatalf("fixture leaves Record.%s zero; populate every field", f.Name)
		}
		if !strings.Contains(string(out), `"`+name+`":`) {
			t.Errorf("rendering lacks Record.%s (json %q):\n%s", f.Name, name, out)
		}
		if f.Type.Kind() == reflect.Slice && !slices.Contains(strings.Split(opts, ","), "omitempty") {
			t.Errorf("Record.%s json tag %q lacks omitempty: an empty %s list would cost the compact rendering bytes", f.Name, tag, name)
		}
	}
	var back Record
	dec := json.NewDecoder(strings.NewReader(string(out)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&back); err != nil || !reflect.DeepEqual(back, r) {
		t.Errorf("line decodes to %+v (%v), want %+v", back, err, r)
	}
}

// TestRenderIndex_EmptyListsOmitted pins the exact line of a record with no
// links and no globs, of a superseding record, and of an amended record (the
// amends / amended_by relations render alongside supersedes / superseded_by).
func TestRenderIndex_EmptyListsOmitted(t *testing.T) {
	plain := Record{ID: "ADR-001", Title: "Plain", Path: "docs/adr/001-a.md", Status: StatusUnknown, Supersedes: []string{}, SupersededBy: nil, AppliesTo: []string{}}
	superseding := Record{ID: "ADR-004", Title: "Successor", Path: "docs/adr/004-d.md", Status: StatusAccepted, Supersedes: []string{"ADR-002"}}
	amended := Record{ID: "ADR-003", Title: "Amended", Path: "docs/adr/003-c.md", Status: StatusAccepted, Amends: []string{"ADR-001"}, AmendedBy: []string{"ADR-005", "ADR-006"}, AppliesTo: []string{".fishhawk/operator.yaml"}}
	out, err := RenderIndex(&Index{SchemaVersion: IndexSchemaVersion, Records: []Record{plain, superseding, amended}})
	if err != nil {
		t.Fatalf("RenderIndex: %v", err)
	}
	want := `{"id":"ADR-001","title":"Plain","path":"docs/adr/001-a.md","status":"unknown"}` + "\n" +
		`{"id":"ADR-004","title":"Successor","path":"docs/adr/004-d.md","status":"accepted","supersedes":["ADR-002"]}` + "\n" +
		`{"id":"ADR-003","title":"Amended","path":"docs/adr/003-c.md","status":"accepted","amends":["ADR-001"],"amended_by":["ADR-005","ADR-006"],"applies_to":[".fishhawk/operator.yaml"]}` + "\n"
	if string(out) != want {
		t.Errorf("RenderIndex =\n%s\nwant\n%s", out, want)
	}
}

// countRepodocLineBreaks counts every line separator repodoc's neutralizer
// honours (repodoc lineSeparatorWidth): LF, CR, CRLF as one, VT, FF, U+0085,
// U+2028 and U+2029.
func countRepodocLineBreaks(s string) int {
	n := 0
	for i := 0; i < len(s); i++ {
		switch {
		case s[i] == '\r' && i+1 < len(s) && s[i+1] == '\n':
			n++
			i++
		case s[i] == '\n', s[i] == '\r', s[i] == '\v', s[i] == '\f':
			n++
		case strings.HasPrefix(s[i:], "\u0085"), strings.HasPrefix(s[i:], "\u2028"), strings.HasPrefix(s[i:], "\u2029"):
			n++
		}
	}
	return n
}

// TestRenderIndex_OneLinePerRecordRoundTrips: titles carrying every line
// separator repodoc honours, a double quote and '<'. The rendering has exactly
// one repodoc line break per record, no raw NEL, the RAW '<' (SetEscapeHTML
// false) and no < escape, and every line decodes strictly back to its
// record.
func TestRenderIndex_OneLinePerRecordRoundTrips(t *testing.T) {
	titles := []string{"lf\nbreak", "cr\rbreak", "crlf\r\nbreak", "vt\vbreak", "ff\fbreak", "nel\u0085break", "ls\u2028break", "ps\u2029break", `a "quoted" <tag> & more`}
	recs := make([]Record, 0, len(titles))
	for i, title := range titles {
		r := Record{ID: fmt.Sprintf("ADR-%03d", i+1), Title: title, Path: fmt.Sprintf("docs/adr/%03d.md", i+1), Status: StatusAccepted}
		if i%2 == 0 {
			r.Supersedes, r.AppliesTo = []string{}, []string{"docs/**"}
		}
		recs = append(recs, r)
	}
	out, err := RenderIndex(&Index{SchemaVersion: IndexSchemaVersion, Records: recs})
	if err != nil {
		t.Fatalf("RenderIndex: %v", err)
	}
	s := string(out)
	if got := countRepodocLineBreaks(s); got != len(recs) {
		t.Errorf("rendering holds %d repodoc line breaks, want exactly %d (one per record):\n%q", got, len(recs), s)
	}
	if strings.Contains(s, "\u0085") {
		t.Errorf("rendering carries a raw U+0085 NEL, which repodoc reads as a line break:\n%q", s)
	}
	if !strings.Contains(s, "<tag>") || strings.Contains(s, `\u003c`) {
		t.Errorf("rendering must carry the raw '<' and no \\u003c escape (SetEscapeHTML(false)):\n%s", s)
	}
	lines := strings.Split(strings.TrimSuffix(s, "\n"), "\n")
	if len(lines) != len(recs) {
		t.Fatalf("%d lines, want %d", len(lines), len(recs))
	}
	norm := func(l []string) []string {
		if len(l) == 0 {
			return nil
		}
		return l
	}
	for i, line := range lines {
		var got Record
		dec := json.NewDecoder(strings.NewReader(line))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&got); err != nil {
			t.Fatalf("line %d does not decode: %v\n%s", i, err, line)
		}
		want := recs[i]
		for _, p := range []*[]string{&got.Supersedes, &got.SupersededBy, &got.Amends, &got.AmendedBy, &got.AppliesTo, &want.Supersedes, &want.SupersededBy, &want.Amends, &want.AmendedBy, &want.AppliesTo} {
			*p = norm(*p)
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("line %d decodes to %+v, want %+v", i, got, want)
		}
	}
}

// TestRenderedTitle pins the #4699 cut directly: at most MaxRenderedTitleChars
// code points passes unchanged, a longer title keeps its first
// MaxRenderedTitleChars-1 code points plus '…'.
func TestRenderedTitle(t *testing.T) {
	for _, tc := range []struct {
		name, in, want string
	}{
		{"empty", "", ""},
		{"short", "Plain title", "Plain title"},
		{"exactly 99", strings.Repeat("a", 99), strings.Repeat("a", 99)},
		{"exactly 100", strings.Repeat("a", 100), strings.Repeat("a", 100)},
		{"101", strings.Repeat("a", 101), strings.Repeat("a", 99) + "…"},
		{"100 é (200 bytes)", strings.Repeat("é", 100), strings.Repeat("é", 100)},
		{"101 é", strings.Repeat("é", 101), strings.Repeat("é", 99) + "…"},
		{"100 then '…'", strings.Repeat("a", 100) + "…", strings.Repeat("a", 99) + "…"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := RenderedTitle(tc.in); got != tc.want {
				t.Errorf("RenderedTitle(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestRenderIndex_CapsLongTitles (#4699): every title RenderIndex writes is at
// most MaxRenderedTitleChars code points. The exact-100 ASCII and 100-'é'
// (200-byte) titles render unchanged, which pins `<=` over code points rather
// than bytes; 101, 249 and 120-'é' titles render as their first 99 code
// points plus a RAW '…'; a cut through a title carrying '\n' and U+0085 just
// before the cut point still leaves one repodoc line break per record. Every
// line decodes strictly to the record with Title == RenderedTitle(original)
// and every other field unchanged, and the caller's Index is not modified.
func TestRenderIndex_CapsLongTitles(t *testing.T) {
	ascii := func(n int) string { return strings.Repeat("a", n) }
	breaks := ascii(96) + "\n\u0085" + ascii(3) // 101 runes; '\n' and NEL at runes 97 and 98
	for _, tc := range []struct {
		name, title string
		// wantTitle is the title's EXACT bytes inside the rendered line.
		wantTitle string
	}{
		{"exactly 100 ASCII, unchanged", ascii(100), ascii(100)},
		{"101 ASCII, cut", ascii(101), ascii(99) + "…"},
		{"249 ASCII, cut", ascii(249), ascii(99) + "…"},
		{"100 é (200 bytes), unchanged", strings.Repeat("é", 100), strings.Repeat("é", 100)},
		{"120 é, cut on a rune boundary", strings.Repeat("é", 120), strings.Repeat("é", 99) + "…"},
		{"line breaks just before the cut", breaks, ascii(96) + `\n\u0085` + ascii(1) + "…"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := Record{ID: "ADR-007", Title: tc.title, Path: "docs/adr/007-g.md", Status: StatusAccepted, Supersedes: []string{"ADR-002"}, AmendedBy: []string{"ADR-009"}, AppliesTo: []string{"docs/**"}}
			idx := &Index{SchemaVersion: IndexSchemaVersion, Records: []Record{r}}
			out, err := RenderIndex(idx)
			if err != nil {
				t.Fatalf("RenderIndex: %v", err)
			}
			want := `{"id":"ADR-007","title":"` + tc.wantTitle + `","path":"docs/adr/007-g.md","status":"accepted","supersedes":["ADR-002"],"amended_by":["ADR-009"],"applies_to":["docs/**"]}` + "\n"
			if string(out) != want {
				t.Errorf("RenderIndex =\n%q\nwant\n%q", out, want)
			}
			if idx.Records[0].Title != tc.title {
				t.Errorf("RenderIndex modified the caller's index: title now %q", idx.Records[0].Title)
			}
			if !utf8.Valid(out) {
				t.Errorf("rendering is not valid UTF-8: %q", out)
			}
			if got := countRepodocLineBreaks(string(out)); got != 1 {
				t.Errorf("rendering holds %d repodoc line breaks, want 1:\n%q", got, out)
			}
			var back Record
			dec := json.NewDecoder(strings.NewReader(string(out)))
			dec.DisallowUnknownFields()
			if err := dec.Decode(&back); err != nil {
				t.Fatalf("line does not decode strictly: %v\n%s", err, out)
			}
			if n := utf8.RuneCountInString(back.Title); n > MaxRenderedTitleChars || !utf8.ValidString(back.Title) {
				t.Errorf("rendered title is %d code points (valid %v), want <= %d", n, utf8.ValidString(back.Title), MaxRenderedTitleChars)
			}
			wantBack := r
			wantBack.Title = RenderedTitle(tc.title)
			if !reflect.DeepEqual(back, wantBack) {
				t.Errorf("line decodes to %+v, want %+v", back, wantBack)
			}
		})
	}
}
