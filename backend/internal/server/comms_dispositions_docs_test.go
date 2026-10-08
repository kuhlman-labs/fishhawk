package server

import (
	"sort"
	"strings"
	"testing"
)

// comms_dispositions_docs_test.go pins the documentation of the #4016 surface
// to the handler's canonical code list (commsDispositionsCodes), so a code
// added to or dropped from the handler fails here naming the doc to update:
//
//   - docs/spec/comms-report-v1.md § "Dispositions": its `| Code | HTTP | When |`
//     table must name EXACTLY commsDispositionsCodes (both directions);
//   - docs/api/v0.openapi.yaml: the route, both operationIds, the schemas and
//     every comms-specific code;
//   - docs/api/v0.md: both route lines and an error-table ROW per
//     comms-specific code.
//
// The /healthz comms-report-v1 key is pinned by the existing, live-derived
// TestHealthzSchemaKeysDocumented.

const commsDocsSpec = "docs/spec/comms-report-v1.md"

// commsSpecificCodes is the subset of commsDispositionsCodes the route owns
// (the shared ladder and storage codes are documented generically).
func commsSpecificCodes(t *testing.T) []string {
	t.Helper()
	var out []string
	for _, c := range commsDispositionsCodes {
		if strings.HasPrefix(c, "comms_") {
			out = append(out, c)
		}
	}
	if len(out) < 6 {
		t.Fatalf("commsDispositionsCodes has %d comms_ codes (%v); the doc checks would be vacuous", len(out), out)
	}
	return out
}

// commsSpecDispositionCodes parses the code column of the § "Dispositions"
// `| Code | HTTP | When |` table.
func commsSpecDispositionCodes(t *testing.T) []string {
	t.Helper()
	doc := readRepoDoc(t, commsDocsSpec)
	sec := upkeepDocSection(t, doc, commsDocsSpec, "\n## Dispositions", "\n## ")
	const header = "\n| Code | HTTP | When |\n|---|---|---|\n"
	i := strings.Index(sec, header)
	if i < 0 {
		t.Fatalf("%s § Dispositions has no `| Code | HTTP | When |` table", commsDocsSpec)
	}
	var codes []string
	for _, line := range strings.Split(sec[i+len(header):], "\n") {
		if !strings.HasPrefix(line, "| `") {
			break
		}
		cell := strings.TrimPrefix(line, "| `")
		end := strings.Index(cell, "`")
		if end <= 0 {
			t.Fatalf("%s § Dispositions: malformed code cell in %q", commsDocsSpec, line)
		}
		codes = append(codes, cell[:end])
	}
	return codes
}

// TestCommsDispositionsDocs_SpecTableMatchesCodes: the spec table names exactly
// the handler's codes, once each.
func TestCommsDispositionsDocs_SpecTableMatchesCodes(t *testing.T) {
	documented := commsSpecDispositionCodes(t)
	seen := map[string]bool{}
	for _, c := range documented {
		if seen[c] {
			t.Errorf("%s § Dispositions lists `%s` twice", commsDocsSpec, c)
		}
		seen[c] = true
	}
	want := map[string]bool{}
	for _, c := range commsDispositionsCodes {
		want[c] = true
		if !seen[c] {
			t.Errorf("commsDispositionsCodes answers `%s` but %s § Dispositions has no table row for it", c, commsDocsSpec)
		}
	}
	for _, c := range documented {
		if !want[c] {
			t.Errorf("%s § Dispositions documents `%s`, which commsDispositionsCodes does not list", commsDocsSpec, c)
		}
	}
	got := append([]string(nil), documented...)
	sort.Strings(got)
	if len(got) == 0 {
		t.Fatal("no codes parsed; the check is vacuous")
	}
}

// TestOpenAPI_CommsDispositionsRouteDocumented: the route, both operations,
// their schemas and every comms-specific code are in the OpenAPI source of
// truth.
func TestOpenAPI_CommsDispositionsRouteDocumented(t *testing.T) {
	doc := readRepoDoc(t, upkeepDocsOpenAPI)
	for _, want := range []string{
		"\n  /v0/runs/{run_id}/comms-dispositions:\n",
		"operationId: recordCommsDispositions",
		"operationId: listCommsDispositions",
		"\n    CommsDispositionsRequest:\n",
		"\n    CommsDispositions:\n",
		"\n    CommsDraftPreviewRecord:\n",
		"\n    CommsClusterSplit:\n",
		"parent_epic_is_source",
	} {
		if !strings.Contains(doc, want) {
			t.Errorf("%s is missing %q", upkeepDocsOpenAPI, strings.TrimSpace(want))
		}
	}
	req := upkeepDocSection(t, doc, upkeepDocsOpenAPI, "\n    CommsDispositionsRequest:\n", "\n    CommsDraftPreviewRecord:\n")
	for _, want := range []string{"additionalProperties: false", "maxItems: 25", "STRICT"} {
		if !strings.Contains(req, want) {
			t.Errorf("%s CommsDispositionsRequest is missing %q", upkeepDocsOpenAPI, want)
		}
	}
	for _, code := range commsSpecificCodes(t) {
		if !strings.Contains(doc, "`"+code+"`") {
			t.Errorf("%s does not name the code `%s`", upkeepDocsOpenAPI, code)
		}
	}
}

// TestV0md_CommsDispositionsRouteDocumented: v0.md lists both route lines and
// carries an error-table ROW for every comms-specific code.
func TestV0md_CommsDispositionsRouteDocumented(t *testing.T) {
	doc := readRepoDoc(t, upkeepDocsV0)
	for _, want := range []string{
		"\nPOST   /v0/runs/{run_id}/comms-dispositions — ",
		"\nGET    /v0/runs/{run_id}/comms-dispositions — ",
	} {
		if !strings.Contains(doc, want) {
			t.Errorf("%s is missing the route line %q", upkeepDocsV0, strings.TrimSpace(want))
		}
	}
	for _, code := range commsSpecificCodes(t) {
		if !strings.Contains(doc, "\n| `"+code+"` |") {
			t.Errorf("%s has no error-table row for `%s`", upkeepDocsV0, code)
		}
	}
}
