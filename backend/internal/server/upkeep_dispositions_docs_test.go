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

	"github.com/kuhlman-labs/fishhawk/backend/internal/plan"
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

// upkeepDocSection returns doc from the first occurrence of start up to the
// next occurrence of end after it (or the end of doc), failing when start is
// absent.
func upkeepDocSection(t *testing.T, doc, rel, start, end string) string {
	t.Helper()
	i := strings.Index(doc, start)
	if i < 0 {
		t.Fatalf("%s: cannot locate %q", rel, strings.TrimSpace(start))
	}
	sec := doc[i+len(start):]
	if j := strings.Index(sec, end); j >= 0 {
		sec = sec[:j]
	}
	return sec
}

// TestUpkeepDispositionsDocs_StrictDecodeAndTwo500s pins the two #3924
// carried-item facts the handler now enforces (strict decode, item 14; the
// post-commit read-back 500 distinct from the atomic-failure 500, item 16) in
// BOTH docs. It pins the load-bearing TOKENS, not sentences. The behaviour
// itself is pinned by TestUpkeepDispositions_UnknownFieldRefused,
// _AtomicFailureRecordsNothing and _ReadBackFailureAfterCommit; when either
// fact changes, update the UpkeepDispositionsRequest description and the
// recordUpkeepDispositions 400/500 descriptions in docs/api/v0.openapi.yaml,
// the POST route line and the `internal_error` (upkeep) row in docs/api/v0.md,
// and docs/spec/upkeep-report-v1.md's internal_error row.
func TestUpkeepDispositionsDocs_StrictDecodeAndTwo500s(t *testing.T) {
	oa := readRepoDoc(t, upkeepDocsOpenAPI)
	req := upkeepDocSection(t, oa, upkeepDocsOpenAPI, "\n    UpkeepDispositionsRequest:\n", "\n    UpkeepDispositions:\n")
	if !strings.Contains(req, "STRICT") || !strings.Contains(req, "unknown key") {
		t.Errorf("%s UpkeepDispositionsRequest description does not state the STRICT decode refusing an unknown key", upkeepDocsOpenAPI)
	}
	post := upkeepDocSection(t, oa, upkeepDocsOpenAPI, "operationId: recordUpkeepDispositions", "operationId: listUpkeepDispositions")
	r500 := upkeepDocSection(t, post, upkeepDocsOpenAPI, "\n        '500':\n", "\n        '503':\n")
	for _, want := range []string{"`details.recorded`", "`details.requested`", "ATOMIC BATCH FAILURE", "AFTER A COMMITTED BATCH", "last-wins"} {
		if !strings.Contains(r500, want) {
			t.Errorf("%s recordUpkeepDispositions 500 description is missing %q (the atomic-vs-post-commit distinction)", upkeepDocsOpenAPI, want)
		}
	}
	r400 := upkeepDocSection(t, post, upkeepDocsOpenAPI, "\n        '400':\n", "\n        '401':")
	if !strings.Contains(r400, "unknown key") {
		t.Errorf("%s recordUpkeepDispositions 400 description does not name the unknown-key refusal", upkeepDocsOpenAPI)
	}

	v0 := readRepoDoc(t, upkeepDocsV0)
	route := upkeepDocSection(t, v0, upkeepDocsV0, "\nPOST   /v0/runs/{run_id}/upkeep-dispositions — ", "\n")
	for _, want := range []string{"STRICT", "unknown key", "details.recorded=0", "details.recorded=details.requested"} {
		if !strings.Contains(route, want) {
			t.Errorf("%s POST upkeep-dispositions route line is missing %q", upkeepDocsV0, want)
		}
	}
	row := upkeepDocSection(t, v0, upkeepDocsV0, "\n| `internal_error` (upkeep) | 500 |", "\n")
	for _, want := range []string{"`recorded: 0`", "`recorded` = `requested`", "last-wins"} {
		if !strings.Contains(row, want) {
			t.Errorf("%s `internal_error` (upkeep) error-table row is missing %q", upkeepDocsV0, want)
		}
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

// TestOpenAPI_UpkeepDispositionsSourceEnumMatchesPlan derives the
// upkeep-dispositions `source` enum from plan.UpkeepSources() (the Go-side
// owner of the closed source set, itself pinned to the schema by
// TestUpkeepSources_MatchSchemaEnum), so a source added to the report schema
// (#3750 added `advisory`) cannot leave the API document stale. On failure,
// update the `source` enum under UpkeepDispositions in docs/api/v0.openapi.yaml.
func TestOpenAPI_UpkeepDispositionsSourceEnumMatchesPlan(t *testing.T) {
	doc := readRepoDoc(t, upkeepDocsOpenAPI)
	// No end marker ("\x00" never occurs): the schema is bounded by the line
	// scan below, at the next 4-space-indented components.schemas key.
	sec := upkeepDocSection(t, doc, upkeepDocsOpenAPI, "\n    UpkeepDispositions:\n", "\x00")
	lines := strings.Split(sec, "\n")
	for k, l := range lines {
		if strings.HasPrefix(l, "    ") && len(l) > 4 && l[4] != ' ' {
			lines = lines[:k]
			break
		}
	}
	sec = "\n" + strings.Join(lines, "\n")
	const field = "\n              source:\n                type: string\n                enum: ["
	i := strings.Index(sec, field)
	if i < 0 {
		t.Fatalf("%s UpkeepDispositions: cannot locate the dispositions `source` enum", upkeepDocsOpenAPI)
	}
	line := sec[i+len(field):]
	line = line[:strings.Index(line, "]")]
	var documented []string
	for _, v := range strings.Split(line, ",") {
		documented = append(documented, strings.TrimSpace(v))
	}
	want := plan.UpkeepSources()
	if strings.Join(documented, ",") != strings.Join(want, ",") {
		t.Errorf("%s UpkeepDispositions `source` enum = %v, want plan.UpkeepSources() = %v", upkeepDocsOpenAPI, documented, want)
	}
}
