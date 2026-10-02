package decisionrecord

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
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
