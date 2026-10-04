package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

// upkeep_dispositions_docs_test.go pins the API documentation of the #3923
// surface: the upkeep-dispositions route and every code it (and the #3921
// ingest it rides on) can answer, in BOTH the OpenAPI source of truth and its
// human companion v0.md. Two facts are DERIVED rather than transcribed, so the
// test fails when the code moves and the prose does not: the /healthz schemas
// key set (read off the live handler) and the issue-anchored trigger-source set
// (read off run.IsIssueAnchored).

const (
	upkeepDocsOpenAPI = "docs/api/v0.openapi.yaml"
	upkeepDocsV0      = "docs/api/v0.md"
)

func readRepoDoc(t *testing.T, rel string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "..", filepath.FromSlash(rel)))
	if err != nil {
		t.Fatalf("read %s: %v", rel, err)
	}
	return string(raw)
}

// upkeepDocumentedCodes is every error code the upkeep-dispositions route
// answers that is specific to it, the two #3921 ingest codes, and the #3725
// scheduler outcome code — each must be named in BOTH docs.
var upkeepDocumentedCodes = []string{
	"upkeep_dispositions_unconfigured",
	"upkeep_verdict_invalid",
	"upkeep_report_absent",
	"upkeep_finding_unknown",
	"upkeep_report_superseded",
	"upkeep_window_closed",
	"upkeep_report_invalid",
	"upkeep_report_stage_invalid",
	"scheduled_key_occupied",
}

// TestOpenAPI_UpkeepDispositionsRouteDocumented: the route, both operations,
// their schemas and every code are in the OpenAPI source of truth.
func TestOpenAPI_UpkeepDispositionsRouteDocumented(t *testing.T) {
	doc := readRepoDoc(t, upkeepDocsOpenAPI)
	for _, want := range []string{
		"\n  /v0/runs/{run_id}/upkeep-dispositions:\n",
		"operationId: recordUpkeepDispositions",
		"operationId: listUpkeepDispositions",
		"\n    UpkeepDispositionsRequest:\n",
		"\n    UpkeepDispositions:\n",
		"authorize_delegation_tier:",
		"parent_epic:",
		"enum: [approved, rejected]",
	} {
		if !strings.Contains(doc, want) {
			t.Errorf("%s is missing %q", upkeepDocsOpenAPI, strings.TrimSpace(want))
		}
	}
	for _, code := range upkeepDocumentedCodes {
		if !strings.Contains(doc, "`"+code+"`") {
			t.Errorf("%s does not name the code `%s`", upkeepDocsOpenAPI, code)
		}
	}
}

// TestV0md_UpkeepDispositionsRouteDocumented: v0.md lists both route lines and
// carries an error-table ROW for every code (a mention in prose is not a row).
func TestV0md_UpkeepDispositionsRouteDocumented(t *testing.T) {
	doc := readRepoDoc(t, upkeepDocsV0)
	for _, want := range []string{
		"\nPOST   /v0/runs/{run_id}/upkeep-dispositions — ",
		"\nGET    /v0/runs/{run_id}/upkeep-dispositions — ",
	} {
		if !strings.Contains(doc, want) {
			t.Errorf("%s is missing the route line %q", upkeepDocsV0, strings.TrimSpace(want))
		}
	}
	for _, code := range upkeepDocumentedCodes {
		if code == "scheduled_key_occupied" {
			// A scheduler outcome, not an HTTP response: documented in the
			// Schedules last_outcome text, not the error table.
			continue
		}
		if !strings.Contains(doc, "\n| `"+code+"` |") {
			t.Errorf("%s has no error-table row for `%s`", upkeepDocsV0, code)
		}
	}
	i := strings.Index(doc, "## Schedules (`GET /v0/schedules`)")
	if i < 0 {
		t.Fatalf("%s has no Schedules section", upkeepDocsV0)
	}
	sched := doc[i:]
	if j := strings.Index(sched[1:], "\n## "); j >= 0 {
		sched = sched[:j+1]
	}
	if !strings.Contains(sched, "`scheduled_key_occupied`") {
		t.Errorf("%s § Schedules does not name the `scheduled_key_occupied` last_outcome", upkeepDocsV0)
	}
}

// TestHealthzSchemaKeysDocumented derives the /healthz schemas key set from the
// LIVE handler and requires every key in both prose enumerations, so a schema
// advertised later fails here naming the two sites to update.
func TestHealthzSchemaKeysDocumented(t *testing.T) {
	rec := httptest.NewRecorder()
	New(Config{}).handleHealth(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	var body healthResponse
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("decode /healthz: %v", err)
	}
	keys := make([]string, 0, len(body.Schemas))
	for k := range body.Schemas {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	if len(keys) == 0 || body.Schemas["upkeep-report-v1"] == "" {
		t.Fatalf("/healthz schemas = %v; the derivation would be vacuous without upkeep-report-v1", keys)
	}

	openapi := readRepoDoc(t, upkeepDocsOpenAPI)
	const oaStart, oaEnd = "embedded canonical schema bytes. Keys:", "Callers compare"
	i := strings.Index(openapi, oaStart)
	j := strings.Index(openapi[max(i, 0):], oaEnd)
	if i < 0 || j < 0 {
		t.Fatalf("%s: cannot locate the /healthz schemas key enumeration (%q … %q)", upkeepDocsOpenAPI, oaStart, oaEnd)
	}
	oaKeys := openapi[i : i+j]

	v0 := readRepoDoc(t, upkeepDocsV0)
	const v0Row = "\n| `schemas` | object |"
	k := strings.Index(v0, v0Row)
	if k < 0 {
		t.Fatalf("%s: cannot locate the /healthz `schemas` field row", upkeepDocsV0)
	}
	row := v0[k+1:]
	row = row[:strings.Index(row, "\n")]

	for _, key := range keys {
		if !strings.Contains(oaKeys, `"`+key+`"`) {
			t.Errorf("/healthz advertises schemas[%s] but %s's schemas description (\"Keys: …\") does not list it", key, upkeepDocsOpenAPI)
		}
		if !strings.Contains(row, "`"+key+"`") {
			t.Errorf("/healthz advertises schemas[%s] but %s's `schemas` field row does not list it", key, upkeepDocsV0)
		}
	}
}

// TestV0md_IssueContextSentenceNamesIssueAnchoredSources derives the
// issue-anchored set from run.IsIssueAnchored and requires v0.md's issue_context
// validity sentence to name exactly that set — so it now names `scheduled`.
func TestV0md_IssueContextSentenceNamesIssueAnchoredSources(t *testing.T) {
	all := append(run.ValidTriggerSources(), run.TriggerScheduled)
	doc := readRepoDoc(t, upkeepDocsV0)
	const anchor = "`issue_context` is therefore valid with an **issue-anchored**"
	i := strings.Index(doc, anchor)
	if i < 0 {
		t.Fatalf("%s: cannot locate the issue_context validity sentence (%q)", upkeepDocsV0, anchor)
	}
	rest := doc[i+len(anchor):]
	open, closeIdx := strings.Index(rest, "("), strings.Index(rest, ")")
	if open < 0 || closeIdx < open {
		t.Fatalf("%s: the issue_context validity sentence has no parenthesised source list", upkeepDocsV0)
	}
	listed := rest[open:closeIdx]
	var anchored int
	for _, src := range all {
		named := strings.Contains(listed, "`"+string(src)+"`")
		if (&run.Run{TriggerSource: src}).IsIssueAnchored() {
			anchored++
			if !named {
				t.Errorf("%s: the issue_context sentence omits issue-anchored trigger_source `%s` (listed: %s)", upkeepDocsV0, src, listed)
			}
		} else if named {
			t.Errorf("%s: the issue_context sentence lists `%s`, which run.IsIssueAnchored rejects", upkeepDocsV0, src)
		}
	}
	if anchored == 0 {
		t.Fatal("run.IsIssueAnchored accepted no trigger source; the derivation is vacuous")
	}
}
