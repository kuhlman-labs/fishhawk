package decisionrecord

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/kuhlman-labs/fishhawk/backend/internal/repodoc"
)

// The shipped-index tests cross the boundary from the Python writer
// (scripts/check-adr --write-index) through the COMMITTED docs/adr/index.json
// and record bodies into the Go strict parser, selection, assembly and
// framing (E78.13 / #4471). They read the real files, so a record re-encode, a
// writer change or an index whose compact rendering (RenderIndex, #4478) is
// past its injection cap fails verify here.

// shippedRepoRoot is the repository root relative to this package directory,
// where `go test` runs.
var shippedRepoRoot = filepath.Join("..", "..", "..")

// operatorScope is ADR-040's REAL applies_to path (approval condition 1).
const operatorScope = ".fishhawk/operator.yaml"

func readShipped(t *testing.T, rel string) []byte {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(shippedRepoRoot, filepath.FromSlash(rel)))
	if err != nil {
		t.Fatalf("read shipped %s: %v", rel, err)
	}
	return raw
}

func shippedIndex(t *testing.T) (*Index, []byte) {
	t.Helper()
	raw := readShipped(t, indexPath)
	idx, err := ParseIndex(raw)
	if err != nil {
		t.Fatalf("ParseIndex(shipped %s): %v", indexPath, err)
	}
	return idx, raw
}

// shippedLinks is the E78.13 / #4471 encoding docs/adr/README.md § "Amendment
// relation" records. A record not listed here is not pinned.
var shippedLinks = map[string]struct{ supersedes, supersededBy, amends, amendedBy []string }{
	"ADR-040": {nil, nil, nil, []string{"ADR-097", "ADR-101"}},
	"ADR-087": {nil, []string{"ADR-094"}, nil, []string{"ADR-100"}},
	"ADR-089": {nil, nil, nil, []string{"ADR-097", "ADR-101"}},
	"ADR-091": {nil, nil, nil, []string{"ADR-095"}},
	"ADR-094": {[]string{"ADR-087"}, nil, nil, []string{"ADR-100"}},
	"ADR-095": {nil, nil, []string{"ADR-091"}, nil},
	"ADR-097": {nil, nil, []string{"ADR-040", "ADR-089"}, []string{"ADR-101"}},
	"ADR-100": {nil, nil, []string{"ADR-087", "ADR-094"}, nil},
	"ADR-101": {nil, nil, []string{"ADR-040", "ADR-089", "ADR-097"}, nil},
}

// TestShippedIndex_EncodesAmendments pins the exact four link lists of every
// re-encoded record, as the strict parser reads them from the committed index.
func TestShippedIndex_EncodesAmendments(t *testing.T) {
	idx, _ := shippedIndex(t)
	byID := map[string]Record{}
	for _, r := range idx.Records {
		byID[r.ID] = r
	}
	norm := func(s []string) []string {
		if len(s) == 0 {
			return nil
		}
		return s
	}
	for id, want := range shippedLinks {
		r, ok := byID[id]
		if !ok {
			t.Errorf("%s is absent from the shipped index", id)
			continue
		}
		for _, f := range []struct {
			field     string
			got, want []string
		}{
			{"supersedes", r.Supersedes, want.supersedes},
			{"superseded_by", r.SupersededBy, want.supersededBy},
			{"amends", r.Amends, want.amends},
			{"amended_by", r.AmendedBy, want.amendedBy},
		} {
			if !slices.Equal(norm(f.got), norm(f.want)) {
				t.Errorf("%s %s = %v, want %v (docs/adr/README.md § \"Amendment relation\")", id, f.field, f.got, f.want)
			}
		}
	}
}

// injectedIndexHeadroom returns how many bytes raw is SHOWN as in a prompt —
// its RenderIndex rendering, which is what Assemble derives the index document
// from (#4478) — and the default injection cap minus that.
func injectedIndexHeadroom(t *testing.T, raw []byte) (shown, headroom int) {
	t.Helper()
	idx, err := ParseIndex(raw)
	if err != nil {
		t.Fatalf("ParseIndex: %v", err)
	}
	rendered, err := RenderIndex(idx)
	if err != nil {
		t.Fatalf("RenderIndex: %v", err)
	}
	return len(rendered), repodoc.DefaultMaxBytes - len(rendered)
}

// TestShippedIndex_WithinInjectionCap is #4471's approval condition 9,
// retargeted by #4478: the committed index's COMPACT RENDERING fits repodoc's
// default injection cap, so it is never shown truncated. Every path that
// injects docs/adr/index.json goes through decisionrecord.Assemble, which shows
// RenderIndex's rendering and never the raw file: the architect persona's
// decision_record (backend/internal/server/reviewer_persona.go) and the
// catch-rate harness's persona arm (backend/internal/agenteval/planreviewpersona.go).
// So the RENDERING, not the raw bytes, is what must fit. The synthetic arm is
// the shipped index re-encoded PRETTY: its raw bytes exceed the cap while its
// rendering does not, so a check that measured the raw file again would fail
// it. The next record that crosses the cap fails here and is an operator
// decision, never a silent truncation.
func TestShippedIndex_WithinInjectionCap(t *testing.T) {
	_, raw := shippedIndex(t)
	var generic map[string]any
	if err := json.Unmarshal(raw, &generic); err != nil {
		t.Fatalf("decode shipped index: %v", err)
	}
	pretty, err := json.MarshalIndent(generic, "", "    ")
	if err != nil {
		t.Fatalf("re-encode shipped index: %v", err)
	}
	if len(pretty) <= repodoc.DefaultMaxBytes {
		t.Fatalf("premise: the pretty re-encoding is %d bytes, want > the %d-byte cap", len(pretty), repodoc.DefaultMaxBytes)
	}
	for _, tc := range []struct {
		name string
		raw  []byte
	}{{"shipped", raw}, {"shipped re-encoded pretty (raw over the cap)", pretty}} {
		shown, headroom := injectedIndexHeadroom(t, tc.raw)
		t.Logf("%s %s: %d raw bytes, rendering %d bytes, cap %d, headroom %d bytes", tc.name, indexPath, len(tc.raw), shown, repodoc.DefaultMaxBytes, headroom)
		if headroom < 0 {
			t.Fatalf("%s %s renders to %d bytes, %d over repodoc.DefaultMaxBytes (%d): the index would be injected truncated; a more compact index format (or a larger cap) is an operator decision",
				tc.name, indexPath, shown, -headroom, repodoc.DefaultMaxBytes)
		}
	}
}

// TestShippedIndex_SelectFollowsAmendedBy: ADR-040's REAL applies_to selects
// it directly, and its amendments ADR-097 and ADR-101 are selected through
// amended_by — AmendsVia set, Via empty, MatchedPaths 0 — never as
// successors. ADR-091 has no applies_to, so an in-memory copy gives it one:
// ADR-095 is then selected as its amendment.
func TestShippedIndex_SelectFollowsAmendedBy(t *testing.T) {
	idx, _ := shippedIndex(t)
	for _, r := range idx.Records {
		if r.ID == "ADR-040" && !slices.Contains(r.AppliesTo, operatorScope) {
			t.Fatalf("ADR-040's shipped applies_to %v no longer names %s: re-anchor this test on ADR-040's real scope", r.AppliesTo, operatorScope)
		}
	}
	check := func(t *testing.T, idx *Index, scope, root string, want map[string][]string) {
		t.Helper()
		matches, err := Select(idx, []string{scope})
		if err != nil {
			t.Fatalf("Select(%s): %v", scope, err)
		}
		got := map[string]Match{}
		for _, m := range matches {
			got[m.Record.ID] = m
		}
		r, ok := got[root]
		if !ok || r.MatchedPaths == 0 {
			t.Fatalf("%s not selected directly by %s (selected %d records)", root, scope, len(matches))
		}
		for id, amendsVia := range want {
			m, ok := got[id]
			if !ok {
				t.Errorf("%s not selected by %s: %s's amended_by was not followed", id, scope, root)
				continue
			}
			if !slices.Equal(m.AmendsVia, amendsVia) || len(m.Via) != 0 || m.MatchedPaths != 0 {
				t.Errorf("%s = {AmendsVia %v Via %v MatchedPaths %d}, want {AmendsVia %v Via [] MatchedPaths 0}: an amendment, never a successor",
					id, m.AmendsVia, m.Via, m.MatchedPaths, amendsVia)
			}
			if m.Rank <= r.Rank {
				t.Errorf("%s ranks %d, ahead of %s (rank %d), the record it amends", id, m.Rank, root, r.Rank)
			}
		}
	}
	t.Run("ADR-040 real scope", func(t *testing.T) {
		check(t, idx, operatorScope, "ADR-040", map[string][]string{
			"ADR-097": {"ADR-040"},
			"ADR-101": {"ADR-040", "ADR-097"},
		})
	})
	t.Run("ADR-091 synthetic scope", func(t *testing.T) {
		const synthetic = "synthetic/adr091/**"
		cp := *idx
		cp.Records = slices.Clone(idx.Records)
		for i := range cp.Records {
			if cp.Records[i].ID == "ADR-091" {
				cp.Records[i].AppliesTo = []string{synthetic}
			}
		}
		check(t, &cp, "synthetic/adr091/x.go", "ADR-091", map[string][]string{"ADR-095": {"ADR-091"}})
	})
}

// shippedFetcher seeds a fake fetcher with the committed index and every
// record body it lists, at admissionSHA.
func shippedFetcher(t *testing.T, idx *Index, raw []byte) *fakeFetcher {
	t.Helper()
	files := map[string]string{indexPath: string(raw)}
	for _, r := range idx.Records {
		files[r.Path] = string(readShipped(t, r.Path))
	}
	ff := newFetcher()
	ff.seed(admissionSHA, files)
	return ff
}

// TestShippedIndex_AssembleKeepsAmendedBodyAndFramesAmendments is approval
// conditions 2 and 3 on the SHIPPED records: through Assemble with ADR-040's
// operator.yaml scope, ADR-040's body ranks ahead of its amendments and is
// included whenever any amendment is (at EVERY record budget swept, the
// included set is a prefix of [ADR-040, ADR-097, ADR-101]); and at the
// production cap the RENDERED prompt documents carry the amendment count, the
// order clause and the per-record amendment sentences. The rendered persona
// prompt itself (personaprompt.Build) is driven by agenteval's
// TestPersonaPrompt_AmendmentFramingCrossesTheBoundary on the same files.
func TestShippedIndex_AssembleKeepsAmendedBodyAndFramesAmendments(t *testing.T) {
	idx, raw := shippedIndex(t)
	ff := shippedFetcher(t, idx, raw)
	req := request(admissionSHA)
	req.ChangePaths = []string{operatorScope}

	order := []string{"ADR-040", "ADR-097", "ADR-101"}
	for _, budget := range []int{4096, 8192, 16384, 20000, repodoc.DefaultMaxBytes, 48000, 64000} {
		sel, err := Assemble(context.Background(), resolver(ff, budget), req)
		if err != nil {
			t.Fatalf("budget %d: Assemble: %v", budget, err)
		}
		var got []string
		for _, id := range includedIDs(sel) {
			if slices.Contains(order, id) {
				got = append(got, id)
			}
		}
		if !slices.Equal(got, order[:len(got)]) {
			t.Errorf("budget %d: included %v (of %v), want a prefix of %v: an amendment's body shown without the body of the record it amends", budget, got, includedIDs(sel), order)
		}
		t.Logf("budget %d: included %v, dropped %d", budget, includedIDs(sel), len(sel.Dropped))
	}

	sel, err := Assemble(context.Background(), resolver(ff, 0), req)
	if err != nil {
		t.Fatalf("production cap: Assemble: %v", err)
	}
	if sel.Index.Truncated {
		t.Errorf("the shipped index was injected truncated at the production cap (%d bytes cut)", sel.Index.DroppedBytes)
	}
	if sel.Amendments != 2 {
		t.Fatalf("Amendments = %d, want 2 (ADR-097, ADR-101)", sel.Amendments)
	}
	inc := includedIDs(sel)
	if !slices.Equal(inc[:2], []string{"ADR-040", "ADR-097"}) {
		t.Fatalf("production cap: included %v, want ADR-040 then ADR-097 first", inc)
	}
	var rendered strings.Builder
	for _, d := range sel.PromptDocuments(false) {
		rendered.WriteString(d.Body)
		rendered.WriteString("\n")
	}
	wants := []string{
		fmt.Sprintf("%d of the selected records amend a selected record, and an amended record still governs, except where an amendment decides otherwise; ", sel.Amendments),
		"an amendment ranks with the records it amends, after them",
		"Amended by ADR-097, ADR-101: it still governs, except where an amendment decides otherwise.",
		"This record was selected because it amends ADR-040; the amended record still governs except where this record decides otherwise.",
	}
	if slices.Contains(inc, "ADR-101") {
		wants = append(wants, "This record was selected because it amends ADR-040, ADR-097; the amended record still governs except where this record decides otherwise.")
	} else {
		wants = append(wants, "- ADR-101 (docs/adr/101-charter-authorization-mode.md, status: accepted, amendment of ADR-040, ADR-097)")
	}
	for _, w := range wants {
		if !strings.Contains(rendered.String(), w) {
			t.Errorf("the rendered decision-record documents lack %q", w)
		}
	}
}
