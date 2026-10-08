package mcpserver

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/apitoken"
	"github.com/kuhlman-labs/fishhawk/backend/internal/artifact"
	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/pgtest"
	"github.com/kuhlman-labs/fishhawk/backend/internal/plan"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
	"github.com/kuhlman-labs/fishhawk/backend/internal/server"
)

// comms_dispositions_test.go pins fishhawk_record_comms_dispositions (#4016):
// the pre-hop validations the tool owns, the backend refusals it must surface
// VERBATIM rather than mask, the decode of every output field, and the
// CROSS-BOUNDARY full path — draft_id / verdict / parent_epic carried from the
// tool input through the real client, the real server.Handler(), the real
// handler and the real Postgres FamilyWindowAppender, and the review surface
// (recorded previews, unaccounted ids, cluster splits) carried back out of
// both the POST echo and the GET.
//
// The counting fake backend is the upkeep sibling's (newUKDFakeBackend): the
// tool's transport seam is identical, and a second copy would be a drift
// source.

// TestRecordCommsDispositions_PreHopValidation pins the three checks the tool
// makes BEFORE the HTTP hop. Each asserts the backend was dialed ZERO times —
// an error-identity assertion alone would pass if the tool forwarded a bad
// request and the (fake) backend happened to refuse it.
func TestRecordCommsDispositions_PreHopValidation(t *testing.T) {
	cases := []struct {
		name    string
		in      RecordCommsDispositionsInput
		wantErr string
	}{
		{
			name:    "invalid run uuid",
			in:      RecordCommsDispositionsInput{RunID: "not-a-uuid", Dispositions: []CommsDispositionEntry{{DraftID: "draft:UR-issue-12", Verdict: "approved"}}},
			wantErr: "not a valid UUID",
		},
		{
			name:    "empty dispositions",
			in:      RecordCommsDispositionsInput{RunID: uuid.NewString()},
			wantErr: "dispositions is required",
		},
		{
			name: "blank draft_id",
			in: RecordCommsDispositionsInput{RunID: uuid.NewString(), Dispositions: []CommsDispositionEntry{
				{DraftID: "draft:UR-issue-12", Verdict: "approved"}, {DraftID: "   ", Verdict: "approved"},
			}},
			wantErr: "dispositions[1].draft_id is required",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fb := newUKDFakeBackend(t)
			_, _, err := fb.resolver().recordCommsDispositions(context.Background(), nil, tc.in)
			if err == nil {
				t.Fatalf("recordCommsDispositions accepted %+v, want a pre-hop refusal", tc.in)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("error = %q, want it to name %q", err, tc.wantErr)
			}
			if n := fb.calls.Load(); n != 0 {
				t.Errorf("the backend was dialed %d time(s) at %q; the refusal must land BEFORE the HTTP hop", n, fb.lastPath)
			}
		})
	}
}

// TestRecordCommsDispositions_ForwardsBodyVerbatim asserts the tool sends the
// draft_id / verdict / parent_epic it was given, on the WIRE, to the comms
// route. draft_id and verdict are trimmed exactly as the backend trims them;
// parent_epic is NOT trimmed, because the backend refuses a padded value and a
// tool-side trim would turn that refusal into an acceptance. A present-but-
// blank parent_epic must reach the backend (and its refusal) rather than
// collapse to "absent", and the body must carry no key the backend's STRICT
// decode would refuse.
func TestRecordCommsDispositions_ForwardsBodyVerbatim(t *testing.T) {
	fb := newUKDFakeBackend(t)
	runID := uuid.New()
	_, _, err := fb.resolver().recordCommsDispositions(context.Background(), nil,
		RecordCommsDispositionsInput{RunID: runID.String(), Dispositions: []CommsDispositionEntry{
			{DraftID: " draft:UR-issue-12+UR-issue-40 ", Verdict: " approved ", ParentEpic: ukdStr("#3775")},
			{DraftID: "draft:UR-issue-50", Verdict: "rejected"},
			{DraftID: "draft:UR-issue-60", Verdict: "approved", ParentEpic: ukdStr(" 5")},
			{DraftID: "draft:UR-issue-70", Verdict: "approved", ParentEpic: ukdStr("")},
		}})
	if err != nil {
		t.Fatalf("recordCommsDispositions: %v", err)
	}
	if want := "/v0/runs/" + runID.String() + "/comms-dispositions"; fb.lastPath != want {
		t.Errorf("path = %q, want %q", fb.lastPath, want)
	}
	var sent struct {
		Dispositions []map[string]any `json:"dispositions"`
	}
	dec := json.NewDecoder(strings.NewReader(string(fb.lastBody)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&sent); err != nil {
		t.Fatalf("decode sent body %q strictly (the backend's decode is strict): %v", fb.lastBody, err)
	}
	if len(sent.Dispositions) != 4 {
		t.Fatalf("sent %d dispositions, want 4: %s", len(sent.Dispositions), fb.lastBody)
	}
	allowed := map[string]bool{"draft_id": true, "verdict": true, "parent_epic": true}
	for i, d := range sent.Dispositions {
		for k := range d {
			if !allowed[k] {
				t.Errorf("entry %d carries key %q, which the backend's strict decode refuses", i, k)
			}
		}
	}
	first := sent.Dispositions[0]
	if first["draft_id"] != "draft:UR-issue-12+UR-issue-40" || first["verdict"] != "approved" || first["parent_epic"] != "#3775" {
		t.Errorf("first entry on the wire = %v, want trimmed draft_id/verdict and parent_epic #3775", first)
	}
	if _, has := sent.Dispositions[1]["parent_epic"]; has {
		t.Errorf("an absent parent_epic was sent as %v; absent must stay absent", sent.Dispositions[1]["parent_epic"])
	}
	if pe := sent.Dispositions[2]["parent_epic"]; pe != " 5" {
		t.Errorf("a padded parent_epic was sent as %q; it must reach the backend VERBATIM (\" 5\") so the backend refuses it", pe)
	}
	if pe, has := sent.Dispositions[3]["parent_epic"]; !has || pe != "" {
		t.Errorf("a present-but-blank parent_epic was sent as %v (present=%v); it must reach the backend as \"\" so the backend refuses it", pe, has)
	}
}

// TestRecordCommsDispositions_SurfacesBackendRefusals is the MCP-layer half of
// the captain-only guarantee and of every domain refusal: the tool error must
// NAME the backend's code, never flatten it into a transport fault. One case
// per code the backend's commsDispositionsCodes lists.
func TestRecordCommsDispositions_SurfacesBackendRefusals(t *testing.T) {
	cases := []struct {
		name    string
		status  int
		code    string
		message string
	}{
		{"unconfigured", http.StatusServiceUnavailable, "comms_dispositions_unconfigured",
			"comms-dispositions endpoint requires run + artifact + audit repositories"},
		{"anonymous", http.StatusUnauthorized, "authentication_required", "authentication required"},
		{"run-bound agent token", http.StatusForbidden, "run_token_forbidden",
			"a run-bound agent token may not record a comms disposition"},
		{"delegated operator-agent token", http.StatusForbidden, "operator_agent_forbidden",
			"a delegated operator-agent token may not record a comms disposition"},
		{"missing scope", http.StatusForbidden, "insufficient_scope",
			"token is missing required scope: write:approvals"},
		{"malformed body", http.StatusBadRequest, "validation_failed",
			"draft would file under parent_epic 12, the issue its cited report lives on"},
		{"unknown run", http.StatusNotFound, "run_not_found", "run not found"},
		{"invalid verdict", http.StatusBadRequest, "comms_verdict_invalid",
			"verdict must name one of the comms verdicts"},
		{"no report", http.StatusConflict, "comms_report_absent",
			"this run has no recorded comms_report"},
		{"unknown draft", http.StatusUnprocessableEntity, "comms_draft_unknown",
			"one or more draft_id values are not declared"},
		{"window closed", http.StatusConflict, "comms_window_closed",
			"this comms report's disposition-capture window has been settled"},
		{"superseded", http.StatusConflict, "comms_report_superseded",
			"a newer comms_report was recorded on this run"},
		{"internal", http.StatusInternalServerError, "internal_error",
			"the run's recorded comms_report, or the gather it names, could not be read or parsed"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fb := newUKDFakeBackend(t)
			fb.status, fb.code, fb.message = tc.status, tc.code, tc.message
			_, _, err := fb.resolver().recordCommsDispositions(context.Background(), nil,
				RecordCommsDispositionsInput{RunID: uuid.NewString(),
					Dispositions: []CommsDispositionEntry{{DraftID: "draft:UR-issue-12", Verdict: "approved"}}})
			if err == nil {
				t.Fatalf("a %d %s backend response surfaced as success", tc.status, tc.code)
			}
			if !strings.Contains(err.Error(), tc.code) {
				t.Errorf("tool error = %q, want it to name the backend code %q", err, tc.code)
			}
			if n := fb.calls.Load(); n != 1 {
				t.Errorf("backend dialed %d times, want exactly 1 (no retry, no pre-hop refusal)", n)
			}
		})
	}
}

// TestRecordCommsDispositions_DecodesEveryOutputField pins that the tool
// decodes EVERY field of the shape the backend emits — the window, the
// disposition rows, all three preview arms (rendered with intake, error,
// skipped), the run-wide preview facts, the unaccounted ids and a cluster
// split with all four placement kinds. It is the mirror-drift guard for the
// passthrough struct: a json-tag typo on any field decodes to the zero value
// and fails the DeepEqual.
func TestRecordCommsDispositions_DecodesEveryOutputField(t *testing.T) {
	fb := newUKDFakeBackend(t)
	fb.respond = func(w http.ResponseWriter, _ *http.Request) bool {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"run_id":"r","artifact_id":"a","stage_id":"s","content_hash":"h","gather_digest":"g",
			"window_closed":true,
			"settlement":{"settlement":"approved","closed_at":"2026-10-08T00:00:00Z","audit_sequence":42},
			"dispositions":[{"draft_id":"draft:UR-issue-12","verdict":"approved","parent_epic":"#9",
				"recorded_at":"t","recorded_by":"github:ops","audit_sequence":7}],
			"undecided_draft_ids":["draft:UR-issue-50"],
			"previews":[
				{"draft_id":"draft:UR-issue-12","filing_body_digest":"d1","title":"T","body":"B",
				 "labels":["type:bug"],"defaulted_labels":["autonomy:medium"],"missing_label_namespaces":["phase"],
				 "number":4,"parent_epic":"#9","source_refs":["#12"],
				 "intake":{"duplicates":[{"number":1240,"title":"Dup","score":0.5,"confidence":"medium","basis":"b","closed":false}],
				           "score":{"value":0.25,"unscored":false},"degraded":true,"degrade_reason":"reader_unavailable",
				           "scanned_items":3,"window_truncated":true,"duration_ms":11}},
				{"draft_id":"draft:UR-issue-50","filing_body_digest":"d2","error":{"code":"work_item_invalid","message":"m"}},
				{"draft_id":"draft:UR-issue-60","filing_body_digest":"d3","skipped":"budget_exhausted"}],
			"preview_degraded":true,"preview_degrade_reason":"conventions_unavailable","charter_text":"rendered",
			"unaccounted_report_ids":["UR-issue-99"],
			"clusters_recorded":true,
			"cluster_splits":[{"report_ids":["UR-issue-12","UR-issue-40","UR-issue-41","UR-issue-99"],"score":0.7,
				"placements":[{"report_id":"UR-issue-12","kind":"draft","entry_id":"draft:UR-issue-12"},
				              {"report_id":"UR-issue-40","kind":"not_drafted","entry_id":"question"},
				              {"report_id":"UR-issue-41","kind":"n_drift","entry_id":"ndrift:N2:UR-issue-41"},
				              {"report_id":"UR-issue-99","kind":"unaccounted"}]}]}`)
		return true
	}
	_, out, err := fb.resolver().recordCommsDispositions(context.Background(), nil,
		RecordCommsDispositionsInput{RunID: uuid.NewString(),
			Dispositions: []CommsDispositionEntry{{DraftID: "draft:UR-issue-12", Verdict: "approved"}}})
	if err != nil {
		t.Fatalf("recordCommsDispositions: %v", err)
	}
	want := RecordCommsDispositionsOutput{
		RunID: "r", ArtifactID: "a", StageID: "s", ContentHash: "h", GatherDigest: "g",
		WindowClosed: true,
		Settlement:   &groomingWindowSettlement{Settlement: "approved", ClosedAt: "2026-10-08T00:00:00Z", AuditSequence: 42},
		Dispositions: []RecordedCommsDisposition{{DraftID: "draft:UR-issue-12", Verdict: "approved", ParentEpic: "#9",
			RecordedAt: "t", RecordedBy: "github:ops", AuditSequence: 7}},
		UndecidedDraftIDs: []string{"draft:UR-issue-50"},
		Previews: []CommsDraftPreviewRecord{
			{DraftID: "draft:UR-issue-12", FilingBodyDigest: "d1", Title: "T", Body: "B",
				Labels: []string{"type:bug"}, DefaultedLabels: []string{"autonomy:medium"}, MissingLabelNamespaces: []string{"phase"},
				Number: 4, ParentEpic: "#9", SourceRefs: []string{"#12"},
				Intake: &IntakeSignals{
					Duplicates: []IntakeDuplicate{{Number: 1240, Title: "Dup", Score: 0.5, Confidence: "medium", Basis: "b"}},
					Score:      IntakeScore{Value: 0.25}, Degraded: true, DegradeReason: "reader_unavailable",
					ScannedItems: 3, WindowTruncated: true, DurationMS: 11,
				}},
			{DraftID: "draft:UR-issue-50", FilingBodyDigest: "d2", Error: &CommsDraftPreviewError{Code: "work_item_invalid", Message: "m"}},
			{DraftID: "draft:UR-issue-60", FilingBodyDigest: "d3", Skipped: "budget_exhausted"},
		},
		PreviewDegraded: true, PreviewDegradeReason: "conventions_unavailable", CharterText: "rendered",
		UnaccountedReportIDs: []string{"UR-issue-99"},
		ClustersRecorded:     true,
		ClusterSplits: []CommsClusterSplit{{
			ReportIDs: []string{"UR-issue-12", "UR-issue-40", "UR-issue-41", "UR-issue-99"}, Score: 0.7,
			Placements: []CommsClusterPlacement{
				{ReportID: "UR-issue-12", Kind: "draft", EntryID: "draft:UR-issue-12"},
				{ReportID: "UR-issue-40", Kind: "not_drafted", EntryID: "question"},
				{ReportID: "UR-issue-41", Kind: "n_drift", EntryID: "ndrift:N2:UR-issue-41"},
				{ReportID: "UR-issue-99", Kind: "unaccounted"},
			},
		}},
	}
	if !reflect.DeepEqual(out, want) {
		gotJSON, _ := json.MarshalIndent(out, "", " ")
		wantJSON, _ := json.MarshalIndent(want, "", " ")
		t.Errorf("decoded output differs from the backend body:\ngot  %s\nwant %s", gotJSON, wantJSON)
	}
}

// TestListCommsDispositions_ClientReadsGET pins the read-back client method:
// a GET (never a POST) to the comms route, decoded into the shared shape.
func TestListCommsDispositions_ClientReadsGET(t *testing.T) {
	fb := newUKDFakeBackend(t)
	var method string
	fb.respond = func(w http.ResponseWriter, r *http.Request) bool {
		method = r.Method
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"run_id":"r","artifact_id":"a","undecided_draft_ids":["draft:UR-issue-12"],"clusters_recorded":false,"cluster_splits":[]}`)
		return true
	}
	runID := uuid.New()
	out, err := fb.resolver().api.ListCommsDispositions(context.Background(), runID)
	if err != nil {
		t.Fatalf("ListCommsDispositions: %v", err)
	}
	if method != http.MethodGet || fb.lastPath != "/v0/runs/"+runID.String()+"/comms-dispositions" {
		t.Errorf("request = %s %s, want GET /v0/runs/%s/comms-dispositions", method, fb.lastPath, runID)
	}
	if out.ArtifactID != "a" || len(out.UndecidedDraftIDs) != 1 || out.ClustersRecorded {
		t.Errorf("decoded = %+v", out)
	}

	fb.respond = nil
	fb.status, fb.code, fb.message = http.StatusConflict, "comms_report_absent", "no report"
	if _, err := fb.resolver().api.ListCommsDispositions(context.Background(), runID); err == nil ||
		!strings.Contains(err.Error(), "comms_report_absent") {
		t.Errorf("a 409 read surfaced as %v, want an error naming comms_report_absent", err)
	}
}

// --- the full path (the cross-boundary test) --------------------------------

// The full-path fixture report's drafts.
const (
	cdFullD1 = "draft:UR-issue-12+UR-issue-40"
	cdFullD2 = "draft:UR-issue-50"
	// cdFullD1Body is D1's recorded preview body: the final rendered filing
	// body (agent prose + server provenance + marker + intake section). It
	// carries `<`, `>` and `&` so a re-encoding anywhere on the path shows.
	cdFullD1Body = "Two users report the CSV export writes no rows.\n\n---\n### Comms provenance (server-rendered)\n- UR-issue-12 (#12) & UR-issue-40 (#40)\n<!-- fishhawk-comms:v1 UR-issue-12 UR-issue-40 -->"
)

type cdFullPath struct {
	runID     uuid.UUID
	stageID   uuid.UUID
	art       *artifact.Artifact
	auditRepo audit.Repository
	r         *runResolver
}

// newCDFullPath stands up a REAL run, plan stage, comms_scan_gathered row,
// kind comms_report artifact and its comms_report_recorded row on real
// Postgres, and an MCP resolver dialing a REAL server.Handler() as a token
// carrying subject.
//
// The report is the shipped example plus a second draft (D2 over
// UR-issue-50): D1 cites UR-issue-12 + UR-issue-40, n_drift cites UR-issue-41,
// UR-comment-12-7 is not_drafted (question), and the shown UR-issue-99 is
// cited nowhere. The gather is raw JSON in the STRICT comms_scan_gathered
// shape (the server's writer is unexported to this package), with three
// recorded clusters: {12, 40} kept whole in D1, {40, comment-12-7} split
// draft/not_drafted, and {41, 99} split n_drift/unaccounted.
//
// The comms_report_recorded row carries the production writer's FULL key set
// (commsRecordedPayload: run_id … charter_text), approval condition 1 — so a
// strict decode of that row on the read path goes RED here.
func newCDFullPath(t *testing.T, subjectFor func(runID uuid.UUID) string) *cdFullPath {
	t.Helper()
	ctx := context.Background()
	pool := pgtest.NewPool(t)
	runRepo := run.NewPostgresRepository(pool)
	artRepo := artifact.NewPostgresRepository(pool)
	auditRepo := audit.NewPostgresRepository(pool)

	rn, err := runRepo.CreateRun(ctx, run.CreateRunParams{
		Repo: "kuhlman-labs/fishhawk", WorkflowID: "user_report_scan",
		WorkflowSHA: "abc", TriggerSource: run.TriggerCLI,
	})
	if err != nil {
		t.Fatalf("create run: %v", err)
	}
	stage, err := runRepo.CreateStage(ctx, run.CreateStageParams{
		RunID: rn.ID, Sequence: 0, Type: run.StageTypePlan,
		ExecutorKind: run.ExecutorAgent, ExecutorRef: "claude-code", RequiresApproval: true,
	})
	if err != nil {
		t.Fatalf("create scan stage: %v", err)
	}
	sid := stage.ID
	systemKind := audit.ActorKind("system")

	// The gather, in the strict shape decodeCommsScanGathered accepts.
	const digest = "4016000000000000000000000000000000000000000000000000000000000001"
	at := "2026-10-06T09:00:00Z"
	shown := func(id string, issue int, comment int64) map[string]any {
		kind := "issue"
		if comment != 0 {
			kind = "comment"
		}
		return map[string]any{"id": id, "kind": kind, "issue_number": issue, "comment_id": comment,
			"content_hash": strings.Repeat("c", 64), "updated_at": at}
	}
	gather, _ := json.Marshal(map[string]any{
		"stage_id": stage.ID.String(), "stage_attempt": "1", "repo": "kuhlman-labs/fishhawk",
		"gather_digest": digest,
		"shown": []any{
			shown("UR-issue-12", 12, 0), shown("UR-issue-40", 40, 0), shown("UR-issue-41", 41, 0),
			shown("UR-comment-12-7", 12, 7), shown("UR-issue-50", 50, 0), shown("UR-issue-99", 99, 0),
		},
		"omitted": []any{}, "omitted_count": 0, "suppressed": []any{}, "suppressed_count": 0,
		"class_excluded": map[string]any{"fishhawk_filed": 0, "bot": 0, "internal": 0},
		"degradations":   []any{}, "malformed_markers": 0,
		"charter": map[string]any{"path": "docs/CHARTER.md", "content_hash": strings.Repeat("d", 64),
			"rubric_ids": []any{"U1", "U2"}, "non_goal_ids": []any{"N1", "N2"}},
		"suggested_clusters": []any{
			map[string]any{"report_ids": []any{"UR-issue-12", "UR-issue-40"}, "score": 0.91},
			map[string]any{"report_ids": []any{"UR-issue-40", "UR-comment-12-7"}, "score": 0.74},
			map[string]any{"report_ids": []any{"UR-issue-41", "UR-issue-99"}, "score": 0.66},
		},
	})
	gEntry, err := auditRepo.AppendChained(ctx, audit.ChainAppendParams{
		RunID: rn.ID, StageID: &sid, Timestamp: time.Now().UTC(),
		Category: server.CategoryCommsScanGathered, ActorKind: &systemKind, Payload: gather,
	})
	if err != nil {
		t.Fatalf("append comms_scan_gathered: %v", err)
	}

	// The report: the shipped example plus D2.
	raw, err := os.ReadFile("../../../docs/spec/examples/comms-report-v1-example.json")
	if err != nil {
		t.Fatalf("read comms example: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	m["drafts"] = append(m["drafts"].([]any), map[string]any{
		"id": cdFullD2, "source_report_ids": []any{"UR-issue-50"},
		"rubric_citations": []any{map[string]any{"rubric_id": "U1"}},
		"proposed_issue": map[string]any{"type": "bug", "title": "Login page rejects valid tokens",
			"body": "Users report the login page rejects valid tokens.", "labels": []any{"area:backend", "type:bug"}},
	})
	body, _ := json.Marshal(m)
	// Parse through the production parser so a fixture the schema or the
	// semantic rules would reject fails HERE rather than as a 500 under test.
	report, perr := plan.ParseCommsReport(body)
	if perr != nil {
		t.Fatalf("fixture report is not valid: %v", perr)
	}
	sv := plan.CommsReportVersion
	art, err := artRepo.Create(ctx, artifact.CreateParams{
		StageID: stage.ID, Kind: artifact.KindCommsReport,
		SchemaVersion: &sv, Content: body, ContentHash: "sha-comms-full-path",
	})
	if err != nil {
		t.Fatalf("create comms_report artifact: %v", err)
	}

	// The recorded row: the production writer's full key set.
	recorded, _ := json.Marshal(map[string]any{
		"run_id": rn.ID.String(), "stage_id": stage.ID.String(), "artifact_id": art.ID.String(),
		"content_hash": art.ContentHash, "schema_version": plan.CommsReportVersion, "size_bytes": len(body),
		"entry_counts": map[string]int{
			"drafts": len(report.Drafts), "n_drift": len(report.NDrift), "not_drafted": len(report.NotDrafted),
		},
		"gather_digest": digest, "gather_sequence": gEntry.Sequence,
		"charter_content_hash":   strings.Repeat("d", 64),
		"unaccounted_report_ids": []any{"UR-issue-99"},
		"previews": []any{
			map[string]any{"draft_id": cdFullD1, "filing_body_digest": strings.Repeat("1", 64),
				"title": "CSV export completes without writing any rows", "body": cdFullD1Body,
				"labels": []any{"area:backend", "type:bug"}, "defaulted_labels": []any{"autonomy:medium"},
				"missing_label_namespaces": []any{}, "source_refs": []any{"#12", "#40"},
				"intake": map[string]any{
					"duplicates": []any{map[string]any{"number": 1240, "title": "Export silently empty",
						"score": 0.5, "confidence": "medium", "basis": "csv export", "closed": false}},
					"score":    map[string]any{"value": 0.25, "unscored": false},
					"degraded": false, "scanned_items": 3, "window_truncated": false, "duration_ms": 4,
				}},
			map[string]any{"draft_id": cdFullD2, "filing_body_digest": strings.Repeat("2", 64),
				"error": map[string]any{"code": "work_item_invalid", "message": "labels: unknown area"}},
		},
		"preview_degraded": false, "charter_text": "rendered",
	})
	if _, err := auditRepo.AppendChained(ctx, audit.ChainAppendParams{
		RunID: rn.ID, StageID: &sid, Timestamp: time.Now().UTC(),
		Category: audit.CommsReportRecordedCategory, ActorKind: &systemKind, Payload: recorded,
	}); err != nil {
		t.Fatalf("append comms_report_recorded: %v", err)
	}

	const bearer = "fhk_cd_full_path"
	tokRepo := &stubMCPAPITokens{tok: &apitoken.Token{
		ID: uuid.New(), Subject: subjectFor(rn.ID),
		Scopes: []string{"write:approvals", "read:runs"}, PlainText: bearer,
	}}
	s := server.New(server.Config{
		RunRepo: runRepo, ArtifactRepo: artRepo, AuditRepo: auditRepo, APITokenRepo: tokRepo,
	})
	httpSrv := httptest.NewServer(s.Handler())
	t.Cleanup(httpSrv.Close)
	return &cdFullPath{
		runID: rn.ID, stageID: stage.ID, art: art, auditRepo: auditRepo,
		r: &runResolver{api: newAPIClient(config{backendURL: httpSrv.URL, apiToken: bearer})},
	}
}

func (f *cdFullPath) persisted(t *testing.T) int {
	t.Helper()
	rows, err := f.auditRepo.ListForRunByCategory(context.Background(), f.runID, audit.CommsDispositionRecordedCategory)
	if err != nil {
		t.Fatalf("list persisted rows: %v", err)
	}
	return len(rows)
}

// assertCDReviewSurface checks the review surface every 200 carries: the
// recorded previews (body byte-equal to the seeded value, intake and the error
// arm intact), the unaccounted ids, and exactly the two split clusters.
func assertCDReviewSurface(t *testing.T, label string, out *RecordCommsDispositionsOutput, f *cdFullPath) {
	t.Helper()
	if out.RunID != f.runID.String() || out.ArtifactID != f.art.ID.String() || out.StageID != f.stageID.String() {
		t.Errorf("%s = run %s / artifact %s / stage %s, want %s / %s / %s",
			label, out.RunID, out.ArtifactID, out.StageID, f.runID, f.art.ID, f.stageID)
	}
	if out.ContentHash != "sha-comms-full-path" || out.WindowClosed || out.Settlement != nil {
		t.Errorf("%s content_hash/window = %q/%v/%+v, want sha-comms-full-path / open / no settlement",
			label, out.ContentHash, out.WindowClosed, out.Settlement)
	}
	if len(out.Previews) != 2 {
		t.Fatalf("%s carried %d previews, want 2 (one per draft, in report order): %+v", label, len(out.Previews), out.Previews)
	}
	p1, p2 := out.Previews[0], out.Previews[1]
	if p1.DraftID != cdFullD1 || p1.Body != cdFullD1Body || p1.Title != "CSV export completes without writing any rows" {
		t.Errorf("%s D1 preview = %q / %q / %q — the recorded body did not survive the seam byte-for-byte",
			label, p1.DraftID, p1.Title, p1.Body)
	}
	if p1.Intake == nil || len(p1.Intake.Duplicates) != 1 || p1.Intake.Duplicates[0].Number != 1240 || p1.Intake.ScannedItems != 3 {
		t.Errorf("%s D1 intake = %+v, want the recorded duplicate #1240 and scanned_items 3", label, p1.Intake)
	}
	if !reflect.DeepEqual(p1.SourceRefs, []string{"#12", "#40"}) || p1.Error != nil || p1.Skipped != "" {
		t.Errorf("%s D1 preview arms = refs %v error %+v skipped %q", label, p1.SourceRefs, p1.Error, p1.Skipped)
	}
	if p2.DraftID != cdFullD2 || p2.Error == nil || p2.Error.Code != "work_item_invalid" || p2.Body != "" {
		t.Errorf("%s D2 preview = %+v, want the recorded work_item_invalid error arm", label, p2)
	}
	if !reflect.DeepEqual(out.UnaccountedReportIDs, []string{"UR-issue-99"}) || out.CharterText != "rendered" || out.PreviewDegraded {
		t.Errorf("%s unaccounted/charter_text/degraded = %v/%q/%v", label, out.UnaccountedReportIDs, out.CharterText, out.PreviewDegraded)
	}
	want := []CommsClusterSplit{
		{ReportIDs: []string{"UR-issue-40", "UR-comment-12-7"}, Score: 0.74, Placements: []CommsClusterPlacement{
			{ReportID: "UR-issue-40", Kind: "draft", EntryID: cdFullD1},
			{ReportID: "UR-comment-12-7", Kind: "not_drafted", EntryID: "question"},
		}},
		{ReportIDs: []string{"UR-issue-41", "UR-issue-99"}, Score: 0.66, Placements: []CommsClusterPlacement{
			{ReportID: "UR-issue-41", Kind: "n_drift", EntryID: "ndrift:N2:UR-issue-41"},
			{ReportID: "UR-issue-99", Kind: "unaccounted"},
		}},
	}
	if !out.ClustersRecorded || !reflect.DeepEqual(out.ClusterSplits, want) {
		t.Errorf("%s clusters_recorded/cluster_splits = %v / %+v, want true / %+v", label, out.ClustersRecorded, out.ClusterSplits, want)
	}
}

// TestRecordCommsDispositionsFullPath is the composition proof. The server
// tests start at HTTP; the refusal tests above stop at a fake backend. A
// serialization defect in the seam between them — a json-tag rename on either
// side, a field dropped from the client's request struct, a response field the
// tool's output type cannot decode, a strict decode of the recorded row —
// passes both and fails HERE.
func TestRecordCommsDispositionsFullPath(t *testing.T) {
	ctx := context.Background()
	f := newCDFullPath(t, func(uuid.UUID) string { return "github:captain" })

	// The captain reads the review surface BEFORE deciding.
	before, err := f.r.api.ListCommsDispositions(ctx, f.runID)
	if err != nil {
		t.Fatalf("ListCommsDispositions before capture: %v", err)
	}
	assertCDReviewSurface(t, "the pre-capture GET", before, f)
	if len(before.Dispositions) != 0 || !reflect.DeepEqual(before.UndecidedDraftIDs, []string{cdFullD1, cdFullD2}) {
		t.Errorf("pre-capture dispositions/undecided = %+v / %v, want none / both drafts in report order",
			before.Dispositions, before.UndecidedDraftIDs)
	}

	_, out, err := f.r.recordCommsDispositions(ctx, nil, RecordCommsDispositionsInput{
		RunID: f.runID.String(),
		Dispositions: []CommsDispositionEntry{
			{DraftID: cdFullD1, Verdict: "approved", ParentEpic: ukdStr("#3775")},
			{DraftID: cdFullD2, Verdict: "rejected"},
		},
	})
	if err != nil {
		t.Fatalf("recordCommsDispositions over the real server: %v", err)
	}
	assertCDReviewSurface(t, "the capture echo", &out, f)

	assertSet := func(t *testing.T, label string, got *RecordCommsDispositionsOutput) {
		t.Helper()
		if len(got.Dispositions) != 2 {
			t.Fatalf("%s returned %d dispositions, want 2: %+v", label, len(got.Dispositions), got.Dispositions)
		}
		// Sorted by draft_id server-side: draft:UR-issue-12+… < draft:UR-issue-50.
		d1, d2 := got.Dispositions[0], got.Dispositions[1]
		if d1.DraftID != cdFullD1 || d1.Verdict != "approved" || d1.ParentEpic != "#3775" {
			t.Errorf("%s D1 row = %+v, want approved / parent_epic #3775 — a field did not survive the seam", label, d1)
		}
		if d2.DraftID != cdFullD2 || d2.Verdict != "rejected" || d2.ParentEpic != "" {
			t.Errorf("%s D2 row = %+v, want rejected / no parent_epic", label, d2)
		}
		for _, d := range got.Dispositions {
			if d.RecordedBy != "github:captain" || d.AuditSequence == 0 || d.RecordedAt == "" {
				t.Errorf("%s row %s recorded_by/seq/at = %q/%d/%q, want github:captain and the persisted chain facts",
					label, d.DraftID, d.RecordedBy, d.AuditSequence, d.RecordedAt)
			}
		}
		if len(got.UndecidedDraftIDs) != 0 {
			t.Errorf("%s undecided_draft_ids = %v after both drafts were decided, want []", label, got.UndecidedDraftIDs)
		}
	}
	assertSet(t, "the capture echo", &out)

	readBack, err := f.r.api.ListCommsDispositions(ctx, f.runID)
	if err != nil {
		t.Fatalf("ListCommsDispositions: %v", err)
	}
	assertCDReviewSurface(t, "the GET read-back", readBack, f)
	assertSet(t, "the GET read-back", readBack)

	if n := f.persisted(t); n != 2 {
		t.Fatalf("persisted %d comms_disposition_recorded rows, want exactly 2", n)
	}

	// Domain refusals cross the seam by NAME and commit nothing: an unknown
	// draft (whole-batch, so the valid D1 entry beside it is NOT recorded) and
	// a parent_epic naming the issue D1's cited report lives on.
	_, _, err = f.r.recordCommsDispositions(ctx, nil, RecordCommsDispositionsInput{
		RunID: f.runID.String(),
		Dispositions: []CommsDispositionEntry{
			{DraftID: cdFullD1, Verdict: "rejected"}, {DraftID: "draft:UR-issue-77", Verdict: "approved"},
		},
	})
	if err == nil || !strings.Contains(err.Error(), "comms_draft_unknown") {
		t.Errorf("unknown draft over the real server: err = %v, want comms_draft_unknown", err)
	}
	_, _, err = f.r.recordCommsDispositions(ctx, nil, RecordCommsDispositionsInput{
		RunID:        f.runID.String(),
		Dispositions: []CommsDispositionEntry{{DraftID: cdFullD1, Verdict: "approved", ParentEpic: ukdStr("12")}},
	})
	if err == nil || !strings.Contains(err.Error(), "validation_failed") || !strings.Contains(err.Error(), "parent_epic") {
		t.Errorf("parent_epic naming a cited report's issue over the real server: err = %v, want validation_failed naming parent_epic", err)
	}
	if n := f.persisted(t); n != 2 {
		t.Errorf("a refused capture changed the persisted row count to %d, want 2", n)
	}
}

// TestRecordCommsDispositionsFullPath_RunBoundTokenRefusedAtBackend proves the
// captain-only guarantee through the REAL stack: a run-bound "mcp:run:<uuid>"
// token — for its OWN run, with write:approvals — is refused by the real
// handler, the refusal reaches the tool caller naming run_token_forbidden, and
// zero rows land.
func TestRecordCommsDispositionsFullPath_RunBoundTokenRefusedAtBackend(t *testing.T) {
	f := newCDFullPath(t, func(runID uuid.UUID) string { return "mcp:run:" + runID.String() })
	_, _, err := f.r.recordCommsDispositions(context.Background(), nil, RecordCommsDispositionsInput{
		RunID:        f.runID.String(),
		Dispositions: []CommsDispositionEntry{{DraftID: cdFullD1, Verdict: "approved"}},
	})
	if err == nil {
		t.Fatal("a run-bound agent token recorded a comms disposition for its OWN run; the refusal must be unconditional")
	}
	if !strings.Contains(err.Error(), "run_token_forbidden") {
		t.Errorf("tool error = %q, want it to name run_token_forbidden", err)
	}
	if n := f.persisted(t); n != 0 {
		t.Errorf("a refused run-bound capture persisted %d rows, want 0", n)
	}
}
