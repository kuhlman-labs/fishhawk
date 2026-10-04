package mcpserver

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
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

// upkeep_dispositions_test.go pins fishhawk_record_upkeep_dispositions
// (#3923): the pre-hop validations the tool owns, the backend refusals it must
// surface VERBATIM rather than mask, and the CROSS-BOUNDARY full path —
// finding_id / verdict / authorize_delegation_tier / parent_epic carried from
// the tool input through the real client, the real server.Handler(), the real
// handler and the real Postgres UpkeepWindowAppender, and back out of the GET.

// --- a purpose-built counting fake backend for the refusal cases ------------

type ukdFakeBackend struct {
	srv      *httptest.Server
	calls    atomic.Int64
	lastBody []byte
	lastPath string
	status   int
	code     string
	message  string
	respond  func(w http.ResponseWriter, r *http.Request) bool
}

func newUKDFakeBackend(t *testing.T) *ukdFakeBackend {
	t.Helper()
	fb := &ukdFakeBackend{status: http.StatusOK}
	fb.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fb.calls.Add(1)
		fb.lastPath = r.URL.Path
		fb.lastBody, _ = io.ReadAll(r.Body)
		if fb.respond != nil && fb.respond(w, r) {
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if fb.status >= 400 {
			w.WriteHeader(fb.status)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"error": map[string]any{"code": fb.code, "message": fb.message},
			})
			return
		}
		_ = json.NewEncoder(w).Encode(RecordUpkeepDispositionsOutput{
			RunID: uuid.NewString(), ArtifactID: uuid.NewString(),
		})
	}))
	t.Cleanup(fb.srv.Close)
	return fb
}

func (fb *ukdFakeBackend) resolver() *runResolver {
	return &runResolver{api: newAPIClient(config{backendURL: fb.srv.URL, apiToken: "tok-test"})}
}

func ukdStr(s string) *string { return &s }

// TestRecordUpkeepDispositions_PreHopValidation pins the three checks the tool
// makes BEFORE the HTTP hop. Each asserts the backend was dialed ZERO times — an
// error-identity assertion alone would pass if the tool forwarded a bad request
// and the (fake) backend happened to refuse it.
func TestRecordUpkeepDispositions_PreHopValidation(t *testing.T) {
	cases := []struct {
		name    string
		in      RecordUpkeepDispositionsInput
		wantErr string
	}{
		{
			name:    "invalid run uuid",
			in:      RecordUpkeepDispositionsInput{RunID: "not-a-uuid", Dispositions: []UpkeepDispositionEntry{{FindingID: "flake:TestX", Verdict: "approved"}}},
			wantErr: "not a valid UUID",
		},
		{
			name:    "empty dispositions",
			in:      RecordUpkeepDispositionsInput{RunID: uuid.NewString()},
			wantErr: "dispositions is required",
		},
		{
			name:    "blank finding_id",
			in:      RecordUpkeepDispositionsInput{RunID: uuid.NewString(), Dispositions: []UpkeepDispositionEntry{{FindingID: "   ", Verdict: "approved"}}},
			wantErr: "finding_id is required",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fb := newUKDFakeBackend(t)
			_, _, err := fb.resolver().recordUpkeepDispositions(context.Background(), nil, tc.in)
			if err == nil {
				t.Fatalf("recordUpkeepDispositions accepted %+v, want a pre-hop refusal", tc.in)
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

// TestRecordUpkeepDispositions_ForwardsBodyVerbatim asserts the tool sends the
// finding_id / verdict / authorize_delegation_tier / parent_epic it was given,
// on the WIRE — including a present-but-blank parent_epic, which must reach the
// backend (and its refusal) rather than collapse to "absent".
func TestRecordUpkeepDispositions_ForwardsBodyVerbatim(t *testing.T) {
	fb := newUKDFakeBackend(t)
	runID := uuid.New()
	_, _, err := fb.resolver().recordUpkeepDispositions(context.Background(), nil,
		RecordUpkeepDispositionsInput{RunID: runID.String(), Dispositions: []UpkeepDispositionEntry{
			{FindingID: " flake:TestWidgetSync ", Verdict: " approved ", AuthorizeDelegationTier: true, ParentEpic: ukdStr(" #3726 ")},
			{FindingID: "toolchain_drift:go", Verdict: "rejected"},
			{FindingID: "deprecation:io/ioutil", Verdict: "approved", ParentEpic: ukdStr("  ")},
		}})
	if err != nil {
		t.Fatalf("recordUpkeepDispositions: %v", err)
	}
	if want := "/v0/runs/" + runID.String() + "/upkeep-dispositions"; fb.lastPath != want {
		t.Errorf("path = %q, want %q", fb.lastPath, want)
	}
	var sent struct {
		Dispositions []map[string]any `json:"dispositions"`
	}
	if err := json.Unmarshal(fb.lastBody, &sent); err != nil {
		t.Fatalf("decode sent body %q: %v", fb.lastBody, err)
	}
	if len(sent.Dispositions) != 3 {
		t.Fatalf("sent %d dispositions, want 3: %s", len(sent.Dispositions), fb.lastBody)
	}
	first := sent.Dispositions[0]
	if first["finding_id"] != "flake:TestWidgetSync" || first["verdict"] != "approved" ||
		first["authorize_delegation_tier"] != true || first["parent_epic"] != "#3726" {
		t.Errorf("first entry on the wire = %v, want trimmed finding_id/verdict, authorize_delegation_tier true, parent_epic #3726", first)
	}
	second := sent.Dispositions[1]
	if _, has := second["parent_epic"]; has {
		t.Errorf("an absent parent_epic was sent as %v; absent must stay absent", second["parent_epic"])
	}
	if _, has := second["authorize_delegation_tier"]; has {
		t.Errorf("an unset authorize_delegation_tier was sent as %v", second["authorize_delegation_tier"])
	}
	third := sent.Dispositions[2]
	if pe, has := third["parent_epic"]; !has || pe != "" {
		t.Errorf("a present-but-blank parent_epic was sent as %v (present=%v); it must reach the backend as \"\" so the backend refuses it", pe, has)
	}
}

// TestRecordUpkeepDispositions_SurfacesBackendRefusals is the MCP-layer half of
// the captain-only guarantee and of every domain refusal: the tool error must
// NAME the backend's code, never flatten it into a transport fault.
func TestRecordUpkeepDispositions_SurfacesBackendRefusals(t *testing.T) {
	cases := []struct {
		name    string
		status  int
		code    string
		message string
	}{
		{"run-bound agent token", http.StatusForbidden, "run_token_forbidden",
			"a run-bound agent token may not record an upkeep disposition"},
		{"delegated operator-agent token", http.StatusForbidden, "operator_agent_forbidden",
			"a delegated operator-agent token may not record an upkeep disposition"},
		{"missing scope", http.StatusForbidden, "insufficient_scope",
			"token is missing required scope: write:approvals"},
		{"unknown run", http.StatusNotFound, "run_not_found", "run not found"},
		{"unknown finding", http.StatusUnprocessableEntity, "upkeep_finding_unknown",
			"one or more finding_id values are not declared"},
		{"invalid verdict", http.StatusBadRequest, "upkeep_verdict_invalid",
			"verdict must name one of the upkeep verdicts"},
		{"no report", http.StatusConflict, "upkeep_report_absent",
			"this run has no recorded upkeep_report"},
		{"window closed", http.StatusConflict, "upkeep_window_closed",
			"this upkeep report's disposition-capture window has been settled"},
		{"superseded", http.StatusConflict, "upkeep_report_superseded",
			"a newer upkeep_report was recorded on this run"},
		{"unconfigured", http.StatusServiceUnavailable, "upkeep_dispositions_unconfigured",
			"upkeep-dispositions endpoint requires run + artifact + audit repositories"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fb := newUKDFakeBackend(t)
			fb.status, fb.code, fb.message = tc.status, tc.code, tc.message
			_, _, err := fb.resolver().recordUpkeepDispositions(context.Background(), nil,
				RecordUpkeepDispositionsInput{RunID: uuid.NewString(),
					Dispositions: []UpkeepDispositionEntry{{FindingID: "flake:TestX", Verdict: "approved"}}})
			if err == nil {
				t.Fatalf("a %d %s backend response surfaced as success", tc.status, tc.code)
			}
			if !strings.Contains(err.Error(), tc.code) {
				t.Errorf("tool error = %q, want it to name the backend code %q", err, tc.code)
			}
		})
	}
}

// TestRecordUpkeepDispositions_DecodesWindowFields pins that the tool decodes
// window_closed / settlement and the per-finding fields off the shape the
// backend emits — the mirror-drift guard for the passthrough struct.
func TestRecordUpkeepDispositions_DecodesWindowFields(t *testing.T) {
	fb := newUKDFakeBackend(t)
	fb.respond = func(w http.ResponseWriter, _ *http.Request) bool {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"run_id":"r","artifact_id":"a","stage_id":"s","content_hash":"h",
			"window_closed":true,
			"settlement":{"settlement":"rejected","closed_at":"2026-10-03T00:00:00Z","audit_sequence":42},
			"dispositions":[{"finding_id":"flake:TestX","source":"flake","verdict":"approved",
				"authorize_delegation_tier":true,"parent_epic":"#9","recorded_at":"t","recorded_by":"github:ops","audit_sequence":7}]}`)
		return true
	}
	_, out, err := fb.resolver().recordUpkeepDispositions(context.Background(), nil,
		RecordUpkeepDispositionsInput{RunID: uuid.NewString(),
			Dispositions: []UpkeepDispositionEntry{{FindingID: "flake:TestX", Verdict: "approved"}}})
	if err != nil {
		t.Fatalf("recordUpkeepDispositions: %v", err)
	}
	if !out.WindowClosed {
		t.Error("window_closed decoded false, want true")
	}
	if out.Settlement == nil || out.Settlement.Settlement != "rejected" || out.Settlement.AuditSequence != 42 {
		t.Errorf("settlement = %+v, want rejected/seq 42", out.Settlement)
	}
	if len(out.Dispositions) != 1 {
		t.Fatalf("decoded %d dispositions, want 1", len(out.Dispositions))
	}
	d := out.Dispositions[0]
	if d.Source != "flake" || !d.AuthorizeDelegationTier || d.ParentEpic != "#9" || d.AuditSequence != 7 || d.RecordedBy != "github:ops" {
		t.Errorf("disposition = %+v, want source flake / tier true / parent_epic #9 / seq 7 / github:ops", d)
	}
}

// --- the full path (the cross-boundary test) --------------------------------

type ukdFullPath struct {
	runID     uuid.UUID
	stageID   uuid.UUID
	art       *artifact.Artifact
	auditRepo audit.Repository
	r         *runResolver
}

// newUKDFullPath stands up a REAL run, plan stage, kind upkeep_report artifact
// (the shipped example, three findings) and its upkeep_report_recorded row on
// real Postgres, and an MCP resolver dialing a REAL server.Handler() as a token
// carrying subject.
func newUKDFullPath(t *testing.T, subjectFor func(runID uuid.UUID) string) *ukdFullPath {
	t.Helper()
	ctx := context.Background()
	pool := pgtest.NewPool(t)
	runRepo := run.NewPostgresRepository(pool)
	artRepo := artifact.NewPostgresRepository(pool)
	auditRepo := audit.NewPostgresRepository(pool)

	rn, err := runRepo.CreateRun(ctx, run.CreateRunParams{
		Repo: "kuhlman-labs/fishhawk", WorkflowID: "upkeep_scan",
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

	body, err := os.ReadFile("../../../docs/spec/examples/upkeep-report-v1-example.json")
	if err != nil {
		t.Fatalf("read upkeep example: %v", err)
	}
	// Parse through the production parser so a fixture the schema would reject
	// fails HERE rather than as a mysterious 500 in the path under test.
	if _, perr := plan.ParseUpkeepReport(body); perr != nil {
		t.Fatalf("fixture report is not schema-valid: %v", perr)
	}
	sv := plan.UpkeepReportVersion
	art, err := artRepo.Create(ctx, artifact.CreateParams{
		StageID: stage.ID, Kind: artifact.KindUpkeepReport,
		SchemaVersion: &sv, Content: body, ContentHash: "sha-upkeep-full-path",
	})
	if err != nil {
		t.Fatalf("create upkeep_report artifact: %v", err)
	}
	payload, _ := json.Marshal(map[string]any{
		"run_id": rn.ID.String(), "stage_id": stage.ID.String(), "artifact_id": art.ID.String(),
	})
	sid := stage.ID
	if _, err := auditRepo.AppendChained(ctx, audit.ChainAppendParams{
		RunID: rn.ID, StageID: &sid, Timestamp: time.Now().UTC(),
		Category: audit.UpkeepReportRecordedCategory, Payload: payload,
	}); err != nil {
		t.Fatalf("append upkeep_report_recorded: %v", err)
	}

	const bearer = "fhk_ukd_full_path"
	tokRepo := &stubMCPAPITokens{tok: &apitoken.Token{
		ID: uuid.New(), Subject: subjectFor(rn.ID),
		Scopes: []string{"write:approvals", "read:runs"}, PlainText: bearer,
	}}
	s := server.New(server.Config{
		RunRepo: runRepo, ArtifactRepo: artRepo, AuditRepo: auditRepo, APITokenRepo: tokRepo,
	})
	httpSrv := httptest.NewServer(s.Handler())
	t.Cleanup(httpSrv.Close)
	return &ukdFullPath{
		runID: rn.ID, stageID: stage.ID, art: art, auditRepo: auditRepo,
		r: &runResolver{api: newAPIClient(config{backendURL: httpSrv.URL, apiToken: bearer})},
	}
}

func (f *ukdFullPath) persisted(t *testing.T) int {
	t.Helper()
	rows, err := f.auditRepo.ListForRunByCategory(context.Background(), f.runID, audit.UpkeepDispositionRecordedCategory)
	if err != nil {
		t.Fatalf("list persisted rows: %v", err)
	}
	return len(rows)
}

// TestRecordUpkeepDispositionsFullPath is the composition proof. The server
// tests start at HTTP; the refusal tests above stop at a fake backend. A
// serialization defect in the seam between them — a json-tag rename on either
// side, a field dropped from the client's request struct, a response field the
// tool's output type cannot decode — passes both and fails HERE.
func TestRecordUpkeepDispositionsFullPath(t *testing.T) {
	ctx := context.Background()
	f := newUKDFullPath(t, func(uuid.UUID) string { return "github:captain" })

	_, out, err := f.r.recordUpkeepDispositions(ctx, nil, RecordUpkeepDispositionsInput{
		RunID: f.runID.String(),
		Dispositions: []UpkeepDispositionEntry{
			{FindingID: "flake:TestWidgetSync", Verdict: "approved", AuthorizeDelegationTier: true, ParentEpic: ukdStr("#3726")},
			{FindingID: "toolchain_drift:go", Verdict: "rejected"},
		},
	})
	if err != nil {
		t.Fatalf("recordUpkeepDispositions over the real server: %v", err)
	}
	if out.RunID != f.runID.String() || out.ArtifactID != f.art.ID.String() || out.StageID != f.stageID.String() {
		t.Errorf("capture response = run %s / artifact %s / stage %s, want %s / %s / %s",
			out.RunID, out.ArtifactID, out.StageID, f.runID, f.art.ID, f.stageID)
	}
	if out.ContentHash != "sha-upkeep-full-path" || out.WindowClosed || out.Settlement != nil {
		t.Errorf("capture content_hash/window = %q/%v/%+v, want sha-upkeep-full-path / open / no settlement",
			out.ContentHash, out.WindowClosed, out.Settlement)
	}

	assertSet := func(t *testing.T, label string, got []RecordedUpkeepDisposition) {
		t.Helper()
		if len(got) != 2 {
			t.Fatalf("%s returned %d dispositions, want 2: %+v", label, len(got), got)
		}
		// Sorted by finding_id server-side: flake:… < toolchain_drift:….
		fl, td := got[0], got[1]
		if fl.FindingID != "flake:TestWidgetSync" || fl.Source != "flake" || fl.Verdict != "approved" {
			t.Errorf("%s flake row = %+v, want flake:TestWidgetSync / flake / approved", label, fl)
		}
		if !fl.AuthorizeDelegationTier || fl.ParentEpic != "#3726" {
			t.Errorf("%s flake authorize_delegation_tier/parent_epic = %v/%q, want true/#3726 — the field did not survive the seam",
				label, fl.AuthorizeDelegationTier, fl.ParentEpic)
		}
		if td.FindingID != "toolchain_drift:go" || td.Source != "toolchain_drift" || td.Verdict != "rejected" ||
			td.AuthorizeDelegationTier || td.ParentEpic != "" {
			t.Errorf("%s toolchain row = %+v, want toolchain_drift:go / rejected / no overrides", label, td)
		}
		for _, d := range got {
			if d.RecordedBy != "github:captain" || d.AuditSequence == 0 || d.RecordedAt == "" {
				t.Errorf("%s row %s recorded_by/seq/at = %q/%d/%q, want github:captain and the persisted chain facts",
					label, d.FindingID, d.RecordedBy, d.AuditSequence, d.RecordedAt)
			}
		}
	}
	assertSet(t, "the capture echo", out.Dispositions)

	readBack, err := f.r.api.ListUpkeepDispositions(ctx, f.runID)
	if err != nil {
		t.Fatalf("ListUpkeepDispositions: %v", err)
	}
	if readBack.ArtifactID != f.art.ID.String() {
		t.Errorf("GET artifact_id = %s, want %s", readBack.ArtifactID, f.art.ID)
	}
	assertSet(t, "the GET read-back", readBack.Dispositions)

	if n := f.persisted(t); n != 2 {
		t.Fatalf("persisted %d upkeep_disposition_recorded rows, want exactly 2", n)
	}

	// A domain refusal crosses the seam by NAME and commits nothing.
	_, _, err = f.r.recordUpkeepDispositions(ctx, nil, RecordUpkeepDispositionsInput{
		RunID:        f.runID.String(),
		Dispositions: []UpkeepDispositionEntry{{FindingID: "flake:NoSuchTest", Verdict: "approved"}},
	})
	if err == nil || !strings.Contains(err.Error(), "upkeep_finding_unknown") {
		t.Errorf("unknown finding over the real server: err = %v, want upkeep_finding_unknown", err)
	}
	if n := f.persisted(t); n != 2 {
		t.Errorf("a refused capture changed the persisted row count to %d, want 2", n)
	}
}

// TestRecordUpkeepDispositionsFullPath_RunBoundTokenRefusedAtBackend proves the
// captain-only guarantee through the REAL stack: a run-bound "mcp:run:<uuid>"
// token — for its OWN run, with write:approvals — is refused by the real
// handler, the refusal reaches the tool caller naming run_token_forbidden, and
// zero rows land.
func TestRecordUpkeepDispositionsFullPath_RunBoundTokenRefusedAtBackend(t *testing.T) {
	f := newUKDFullPath(t, func(runID uuid.UUID) string { return "mcp:run:" + runID.String() })
	_, _, err := f.r.recordUpkeepDispositions(context.Background(), nil, RecordUpkeepDispositionsInput{
		RunID:        f.runID.String(),
		Dispositions: []UpkeepDispositionEntry{{FindingID: "flake:TestWidgetSync", Verdict: "approved"}},
	})
	if err == nil {
		t.Fatal("a run-bound agent token recorded an upkeep disposition for its OWN run; the refusal must be unconditional")
	}
	if !strings.Contains(err.Error(), "run_token_forbidden") {
		t.Errorf("tool error = %q, want it to name run_token_forbidden", err)
	}
	if n := f.persisted(t); n != 0 {
		t.Errorf("a refused run-bound capture persisted %d rows, want 0", n)
	}
}
