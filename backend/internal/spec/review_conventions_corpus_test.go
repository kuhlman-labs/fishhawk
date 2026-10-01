package spec_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/kuhlman-labs/fishhawk/backend/internal/spec"
)

// The shared review_conventions parity corpus (ADR-068 / E55.2 / #2243):
// docs/spec/review-conventions-fixtures.json, mirrored into this module's
// testdata/ by scripts/sync-schemas. cli/internal/spec runs the SAME rows
// through ValidateBytes (TestReviewConventionsCorpus_CLIMatches); this side
// runs them through ParseBytes — YAML -> version-routed schema -> v2 reuse
// resolution -> typed decode under DisallowUnknownFields -> Validate — so the
// schema, the struct decode and the semantic layer are all crossed by every
// row. A divergence in either module fails whichever side drifted.

const reviewConventionsCorpusPath = "testdata/review-conventions-fixtures.json"

// Corpus layers: which validator tier a rejection row is expected to fail in.
const (
	rcLayerSchema   = "schema"
	rcLayerSemantic = "semantic"
)

// rcCorpusRow is one corpus row. Message is a pointer so an ABSENT message on
// a semantic row is distinguishable from an empty one and fails the loader.
type rcCorpusRow struct {
	Name    string  `json:"name"`
	Note    string  `json:"note"`
	Doc     string  `json:"doc"`
	Valid   *bool   `json:"valid"`
	Layer   string  `json:"layer"`
	Path    string  `json:"path"`
	Message *string `json:"message"`
}

// loadReviewConventionsCorpus reads and SHAPE-CHECKS the corpus, failing the
// test on an empty or malformed corpus so the runner can never pass vacuously:
// every row needs a unique name, a document and an explicit verdict; a
// rejection row needs a known layer and a path; a semantic row needs the exact
// message; and the corpus must carry at least one row of each kind.
func loadReviewConventionsCorpus(t *testing.T) []rcCorpusRow {
	t.Helper()
	raw, err := os.ReadFile(reviewConventionsCorpusPath)
	if err != nil {
		t.Fatalf("read corpus: %v", err)
	}
	var corpus struct {
		Rows []rcCorpusRow `json:"rows"`
	}
	if err := json.Unmarshal(raw, &corpus); err != nil {
		t.Fatalf("decode corpus: %v", err)
	}
	if len(corpus.Rows) == 0 {
		t.Fatal("review-conventions corpus has no rows — the parity proof would pass vacuously")
	}
	seen := map[string]bool{}
	kinds := map[string]int{}
	for i, r := range corpus.Rows {
		if r.Name == "" || seen[r.Name] {
			t.Fatalf("row %d: name %q is empty or duplicated", i, r.Name)
		}
		seen[r.Name] = true
		if strings.TrimSpace(r.Doc) == "" {
			t.Fatalf("row %q: empty doc", r.Name)
		}
		if r.Valid == nil {
			t.Fatalf("row %q: missing `valid`", r.Name)
		}
		if *r.Valid {
			if r.Layer != "" || r.Path != "" || r.Message != nil {
				t.Fatalf("row %q: a valid row carries layer/path/message", r.Name)
			}
			kinds["valid"]++
			continue
		}
		if r.Path == "" || !strings.HasPrefix(r.Path, "/") {
			t.Fatalf("row %q: rejection path %q is not a JSON pointer", r.Name, r.Path)
		}
		switch r.Layer {
		case rcLayerSemantic:
			if r.Message == nil || *r.Message == "" {
				t.Fatalf("row %q: a semantic row needs its exact message", r.Name)
			}
		case rcLayerSchema:
			if r.Message != nil {
				t.Fatalf("row %q: a schema row asserts a path only, not a message", r.Name)
			}
		default:
			t.Fatalf("row %q: unknown layer %q", r.Name, r.Layer)
		}
		kinds[r.Layer]++
	}
	for _, k := range []string{"valid", rcLayerSemantic, rcLayerSchema} {
		if kinds[k] == 0 {
			t.Fatalf("review-conventions corpus has no %s row", k)
		}
	}
	return corpus.Rows
}

// rcPathAtOrUnder reports whether got is want or a pointer beneath it.
func rcPathAtOrUnder(got, want string) bool {
	return got == want || strings.HasPrefix(got, want+"/")
}

// TestReviewConventionsCorpus_BackendMatches runs every corpus row through the
// REAL backend parse path: a valid row parses; a semantic row returns a
// *ValidationError whose path AND message equal the row's exactly; a schema
// row returns a *SchemaError at or under the row's path.
func TestReviewConventionsCorpus_BackendMatches(t *testing.T) {
	for _, row := range loadReviewConventionsCorpus(t) {
		t.Run(row.Name, func(t *testing.T) {
			_, err := spec.ParseBytes([]byte(row.Doc))
			if *row.Valid {
				if err != nil {
					t.Fatalf("valid row rejected: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("%s row parsed VALID, want a rejection at %s", row.Layer, row.Path)
			}
			switch row.Layer {
			case rcLayerSemantic:
				var ve *spec.ValidationError
				if !errors.As(err, &ve) {
					t.Fatalf("err = %T %v, want *spec.ValidationError", err, err)
				}
				if ve.Path != row.Path {
					t.Errorf("path = %q, want %q", ve.Path, row.Path)
				}
				if ve.Message != *row.Message {
					t.Errorf("message mismatch\n got: %s\nwant: %s", ve.Message, *row.Message)
				}
			case rcLayerSchema:
				var se *spec.SchemaError
				if !errors.As(err, &se) {
					t.Fatalf("err = %T %v, want *spec.SchemaError", err, err)
				}
				if !rcPathAtOrUnder(se.Path, row.Path) {
					t.Errorf("schema error at %q (%s), want at or under %q", se.Path, se.Message, row.Path)
				}
			}
		})
	}
}

// TestReviewConventionsCorpus_MirrorMatchesCanonical holds this module's
// testdata/ copy byte-identical to the canonical docs/spec/ corpus.
// scripts/sync-schemas writes the mirror, but a DROPPED cp line would leave a
// stale mirror that the schema-sync drift check cannot see (re-running the
// sync changes nothing), so the parity proof would silently run old rows.
func TestReviewConventionsCorpus_MirrorMatchesCanonical(t *testing.T) {
	canonical, err := os.ReadFile("../../../docs/spec/review-conventions-fixtures.json")
	if err != nil {
		t.Fatalf("read canonical corpus: %v", err)
	}
	mirror, err := os.ReadFile(reviewConventionsCorpusPath)
	if err != nil {
		t.Fatalf("read mirror: %v", err)
	}
	if !bytes.Equal(canonical, mirror) {
		t.Fatalf("%s differs from docs/spec/review-conventions-fixtures.json — run scripts/sync-schemas (and check it still carries the review-conventions cp lines)", reviewConventionsCorpusPath)
	}
}
