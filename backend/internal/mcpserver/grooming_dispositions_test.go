package mcpserver

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
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
	"github.com/kuhlman-labs/fishhawk/backend/internal/workmgmt"
)

// grooming_dispositions_test.go pins fishhawk_record_grooming_dispositions
// (E54.30 / #2843): the pre-hop validations the tool owns, the backend refusals
// it must surface VERBATIM rather than mask, and — the change's actual
// deliverable — ONE test carrying entry_id / verdict / close_target end to end
// through the real client serialization, the real handler, real chained audit
// persistence, and back out of the GET.

// --- a purpose-built fake backend for the refusal cases ---------------------
//
// Deliberately NOT the package's shared fakeBackend: these cases need only a
// canned status + error envelope on one route, and a local server keeps the
// refusal assertion attributable to THIS route rather than to a shared mux.

type gdFakeBackend struct {
	srv *httptest.Server
	// lastBody is the raw request body the tool sent, so the forwarding
	// assertion is made on the WIRE bytes rather than on the tool's inputs.
	lastBody []byte
	lastPath string
	status   int
	code     string
	message  string
	respond  func(w http.ResponseWriter, r *http.Request) bool
}

func newGDFakeBackend(t *testing.T) *gdFakeBackend {
	t.Helper()
	fb := &gdFakeBackend{status: http.StatusOK}
	fb.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fb.lastPath = r.URL.Path
		if r.Body != nil {
			buf := make([]byte, 0, 512)
			tmp := make([]byte, 512)
			for {
				n, err := r.Body.Read(tmp)
				buf = append(buf, tmp[:n]...)
				if err != nil {
					break
				}
			}
			fb.lastBody = buf
		}
		if fb.respond != nil && fb.respond(w, r) {
			return
		}
		if fb.status >= 400 {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(fb.status)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"error": map[string]any{"code": fb.code, "message": fb.message},
			})
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(RecordGroomingDispositionsOutput{
			RunID: uuid.NewString(), ArtifactID: uuid.NewString(),
		})
	}))
	t.Cleanup(fb.srv.Close)
	return fb
}

func (fb *gdFakeBackend) resolver() *runResolver {
	return &runResolver{api: newAPIClient(config{backendURL: fb.srv.URL, apiToken: "tok-test"})}
}

// --- pre-hop validation (the tool's own controls) ---------------------------

// TestRecordGroomingDispositions_PreHopValidation pins the three checks the
// tool makes BEFORE the HTTP hop. Each asserts the backend was NEVER dialed —
// an error-identity assertion alone would pass if the tool forwarded a bad
// request and the (fake) backend happened to refuse it.
func TestRecordGroomingDispositions_PreHopValidation(t *testing.T) {
	cases := []struct {
		name    string
		in      RecordGroomingDispositionsInput
		wantErr string
	}{
		{
			name:    "invalid run uuid",
			in:      RecordGroomingDispositionsInput{RunID: "not-a-uuid", Dispositions: []GroomingDispositionEntry{{EntryID: "ordering:a", Verdict: "approved"}}},
			wantErr: "not a valid UUID",
		},
		{
			name:    "empty dispositions",
			in:      RecordGroomingDispositionsInput{RunID: uuid.NewString()},
			wantErr: "dispositions is required",
		},
		{
			name:    "blank entry_id",
			in:      RecordGroomingDispositionsInput{RunID: uuid.NewString(), Dispositions: []GroomingDispositionEntry{{EntryID: "   ", Verdict: "approved"}}},
			wantErr: "entry_id is required",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fb := newGDFakeBackend(t)
			_, _, err := fb.resolver().recordGroomingDispositions(context.Background(), nil, tc.in)
			if err == nil {
				t.Fatalf("recordGroomingDispositions accepted %+v, want a pre-hop refusal", tc.in)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("error = %q, want it to name %q", err, tc.wantErr)
			}
			if fb.lastPath != "" {
				t.Errorf("the backend was dialed at %q; the refusal must land BEFORE the HTTP hop", fb.lastPath)
			}
		})
	}
}

// TestRecordGroomingDispositions_ForwardsBodyVerbatim asserts the tool sends
// the entry_id / verdict / close_target it was given, on the WIRE.
func TestRecordGroomingDispositions_ForwardsBodyVerbatim(t *testing.T) {
	fb := newGDFakeBackend(t)
	runID := uuid.New()
	_, _, err := fb.resolver().recordGroomingDispositions(context.Background(), nil,
		RecordGroomingDispositionsInput{RunID: runID.String(), Dispositions: []GroomingDispositionEntry{
			{EntryID: "duplicate:github/acme/app#2+github/acme/app#3", Verdict: " approved ", CloseTarget: " acme/app#2 "},
		}})
	if err != nil {
		t.Fatalf("recordGroomingDispositions: %v", err)
	}
	if want := "/v0/runs/" + runID.String() + "/grooming-dispositions"; fb.lastPath != want {
		t.Errorf("path = %q, want %q", fb.lastPath, want)
	}
	var sent struct {
		Dispositions []GroomingDispositionEntry `json:"dispositions"`
	}
	if err := json.Unmarshal(fb.lastBody, &sent); err != nil {
		t.Fatalf("decode sent body %q: %v", fb.lastBody, err)
	}
	if len(sent.Dispositions) != 1 {
		t.Fatalf("sent %d dispositions, want 1: %s", len(sent.Dispositions), fb.lastBody)
	}
	got := sent.Dispositions[0]
	if got.EntryID != "duplicate:github/acme/app#2+github/acme/app#3" {
		t.Errorf("sent entry_id = %q", got.EntryID)
	}
	// Surrounding whitespace is trimmed; the VALUES are otherwise verbatim.
	if got.Verdict != "approved" || got.CloseTarget != "acme/app#2" {
		t.Errorf("sent verdict/close_target = %q/%q, want approved/acme/app#2", got.Verdict, got.CloseTarget)
	}
}

// TestRecordGroomingDispositions_SurfacesBackendRefusals is the MCP-layer half
// of the operator-only guarantee: the MCP path must NOT mask the HTTP refusal.
// Each case asserts the tool error NAMES the backend's code — a generic
// "request failed" would let an operator (or an agent) misread a refusal as a
// transport fault.
func TestRecordGroomingDispositions_SurfacesBackendRefusals(t *testing.T) {
	cases := []struct {
		name    string
		status  int
		code    string
		message string
	}{
		{"run-bound agent token", http.StatusForbidden, "run_token_forbidden",
			"a run-bound agent token may not record a grooming disposition"},
		{"delegated operator-agent token", http.StatusForbidden, "operator_agent_forbidden",
			"a delegated operator-agent token may not record a grooming disposition"},
		{"missing scope", http.StatusForbidden, "insufficient_scope",
			"token is missing required scope: write:approvals"},
		{"unknown entry", http.StatusUnprocessableEntity, "grooming_entry_unknown",
			"one or more entry_id values are not declared"},
		{"invalid verdict", http.StatusBadRequest, "grooming_verdict_invalid",
			"verdict must name one of the three grooming verdicts"},
		{"no report", http.StatusConflict, "grooming_report_absent",
			"this run carries no grooming_report artifact"},
		{"window closed", http.StatusConflict, "grooming_window_closed",
			"this grooming report's disposition-capture window has been settled"},
		{"unconfigured", http.StatusServiceUnavailable, "grooming_dispositions_unconfigured",
			"grooming-dispositions endpoint requires run + artifact + audit repositories"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fb := newGDFakeBackend(t)
			fb.status, fb.code, fb.message = tc.status, tc.code, tc.message
			_, _, err := fb.resolver().recordGroomingDispositions(context.Background(), nil,
				RecordGroomingDispositionsInput{RunID: uuid.NewString(),
					Dispositions: []GroomingDispositionEntry{{EntryID: "ordering:a", Verdict: "approved"}}})
			if err == nil {
				t.Fatalf("a %d %s backend response surfaced as success", tc.status, tc.code)
			}
			if !strings.Contains(err.Error(), tc.code) {
				t.Errorf("tool error = %q, want it to name the backend code %q", err, tc.code)
			}
		})
	}
}

// TestRecordGroomingDispositions_DecodesWindowFields pins that the tool decodes
// the #2991 window_closed / settlement fields off the same response shape the
// backend emits — the mirror-drift guard for the passthrough struct.
func TestRecordGroomingDispositions_DecodesWindowFields(t *testing.T) {
	fb := newGDFakeBackend(t)
	fb.respond = func(w http.ResponseWriter, _ *http.Request) bool {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(RecordGroomingDispositionsOutput{
			RunID: uuid.NewString(), ArtifactID: uuid.NewString(),
			WindowClosed: true,
			Settlement:   &groomingWindowSettlement{Settlement: "approved", ClosedAt: "2026-09-05T00:00:00Z", AuditSequence: 42},
		})
		return true
	}
	_, out, err := fb.resolver().recordGroomingDispositions(context.Background(), nil,
		RecordGroomingDispositionsInput{RunID: uuid.NewString(),
			Dispositions: []GroomingDispositionEntry{{EntryID: "ordering:a", Verdict: "approved"}}})
	if err != nil {
		t.Fatalf("recordGroomingDispositions: %v", err)
	}
	if !out.WindowClosed {
		t.Error("window_closed decoded false, want true")
	}
	if out.Settlement == nil || out.Settlement.Settlement != "approved" || out.Settlement.AuditSequence != 42 {
		t.Errorf("settlement = %+v, want approved/seq 42", out.Settlement)
	}
}

// --- the full path (binding condition 1) ------------------------------------

// TestRecordGroomingDispositionsFullPath is the composition proof binding
// condition 1 requires. The persistence test in backend/internal/server starts
// at HTTP; the refusal tests above stop at a fake backend. Two partial paths
// that compose IN PRINCIPLE do not prove the composition — a serialization
// defect in the seam between them (a json-tag rename on either side, a field
// dropped from the client's request struct, a response field the tool's output
// type cannot decode) passes both and fails here.
//
// One test, one set of values: entry_id / verdict / close_target travel from
// the MCP tool input, through the REAL apiClient marshal, over HTTP into the
// REAL server.Handler(), through the REAL handler and the REAL chained audit
// repository backed by a REAL Postgres, and back out through the REAL GET
// read-back into the tool's typed output.
func TestRecordGroomingDispositionsFullPath(t *testing.T) {
	ctx := context.Background()
	pool := pgtest.NewPool(t)
	runRepo := run.NewPostgresRepository(pool)
	artRepo := artifact.NewPostgresRepository(pool)
	auditRepo := audit.NewPostgresRepository(pool)

	rn, err := runRepo.CreateRun(ctx, run.CreateRunParams{
		Repo: "kuhlman-labs/fishhawk", WorkflowID: "backlog_grooming",
		WorkflowSHA: "abc", TriggerSource: run.TriggerCLI,
	})
	if err != nil {
		t.Fatalf("create run: %v", err)
	}
	stage, err := runRepo.CreateStage(ctx, run.CreateStageParams{
		RunID: rn.ID, Sequence: 0, Type: run.StageTypePlan,
		ExecutorKind: run.ExecutorAgent, ExecutorRef: "claude-code",
		RequiresApproval: true,
	})
	if err != nil {
		t.Fatalf("create groom stage: %v", err)
	}

	body, hygieneID, duplicateID := gdFullPathReport(t)
	sv := plan.GroomingReportVersion
	art, err := artRepo.Create(ctx, artifact.CreateParams{
		StageID: stage.ID, Kind: artifact.KindGroomingReport,
		SchemaVersion: &sv, Content: body, ContentHash: "sha-full-path",
	})
	if err != nil {
		t.Fatalf("create grooming_report artifact: %v", err)
	}

	const bearer = "fhk_gd_full_path"
	tokRepo := &stubMCPAPITokens{tok: &apitoken.Token{
		ID: uuid.New(), Subject: "github:operator",
		Scopes: []string{"write:approvals", "read:runs"}, PlainText: bearer,
	}}
	s := server.New(server.Config{
		RunRepo: runRepo, ArtifactRepo: artRepo, AuditRepo: auditRepo, APITokenRepo: tokRepo,
	})
	httpSrv := httptest.NewServer(s.Handler())
	t.Cleanup(httpSrv.Close)
	r := &runResolver{api: newAPIClient(config{backendURL: httpSrv.URL, apiToken: bearer})}

	// --- capture, through the tool ---
	_, out, err := r.recordGroomingDispositions(ctx, nil, RecordGroomingDispositionsInput{
		RunID: rn.ID.String(),
		Dispositions: []GroomingDispositionEntry{
			{EntryID: hygieneID, Verdict: string(workmgmt.GroomingApproved)},
			{EntryID: duplicateID, Verdict: string(workmgmt.GroomingRejected), CloseTarget: "kuhlman-labs/fishhawk#16"},
		},
	})
	if err != nil {
		t.Fatalf("recordGroomingDispositions over the real server: %v", err)
	}
	if out.RunID != rn.ID.String() || out.ArtifactID != art.ID.String() || out.StageID != stage.ID.String() {
		t.Errorf("capture response = run %s / artifact %s / stage %s, want %s / %s / %s",
			out.RunID, out.ArtifactID, out.StageID, rn.ID, art.ID, stage.ID)
	}
	if out.ContentHash != "sha-full-path" {
		t.Errorf("capture content_hash = %q, want sha-full-path", out.ContentHash)
	}

	// --- the values survived the whole seam ---
	assertGDSet := func(t *testing.T, label string, got []RecordedGroomingDisposition) {
		t.Helper()
		if len(got) != 2 {
			t.Fatalf("%s returned %d dispositions, want 2: %+v", label, len(got), got)
		}
		byEntry := map[string]RecordedGroomingDisposition{}
		for _, d := range got {
			byEntry[d.EntryID] = d
		}
		h, ok := byEntry[hygieneID]
		if !ok {
			t.Fatalf("%s is missing the hygiene entry %q", label, hygieneID)
		}
		if h.Verdict != "approved" || h.EntryClass != plan.GroomingClassHygiene || h.CloseTarget != "" {
			t.Errorf("%s hygiene row = %+v, want verdict approved / class hygiene / no close_target", label, h)
		}
		d, ok := byEntry[duplicateID]
		if !ok {
			t.Fatalf("%s is missing the duplicate entry %q", label, duplicateID)
		}
		if d.Verdict != "rejected" || d.EntryClass != plan.GroomingClassDuplicate {
			t.Errorf("%s duplicate row = %+v, want verdict rejected / class duplicate", label, d)
		}
		if d.CloseTarget != "kuhlman-labs/fishhawk#16" {
			t.Errorf("%s close_target = %q, want kuhlman-labs/fishhawk#16 — the field did not survive the seam", label, d.CloseTarget)
		}
		if h.RecordedBy != "github:operator" || d.RecordedBy != "github:operator" {
			t.Errorf("%s recorded_by = %q/%q, want github:operator", label, h.RecordedBy, d.RecordedBy)
		}
		if h.AuditSequence == 0 || d.AuditSequence == 0 {
			t.Errorf("%s audit_sequence = %d/%d, want the persisted chain sequences", label, h.AuditSequence, d.AuditSequence)
		}
		if h.RecordedAt == "" || d.RecordedAt == "" {
			t.Errorf("%s recorded_at is empty on one row", label)
		}
	}
	assertGDSet(t, "the capture echo", out.Dispositions)

	// --- and back out of the REAL GET, through the REAL client decode ---
	readBack, err := r.api.ListGroomingDispositions(ctx, rn.ID)
	if err != nil {
		t.Fatalf("ListGroomingDispositions: %v", err)
	}
	assertGDSet(t, "the GET read-back", readBack.Dispositions)

	// --- and the rows are really on the chain, under the distinct category ---
	rows, err := auditRepo.ListForRunByCategory(ctx, rn.ID, "grooming_disposition_recorded")
	if err != nil {
		t.Fatalf("list persisted rows: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("persisted %d grooming_disposition_recorded rows, want 2", len(rows))
	}
	applied, err := auditRepo.ListForRunByCategory(ctx, rn.ID, workmgmt.GroomingMutationAppliedCategory)
	if err != nil {
		t.Fatalf("list apply rows: %v", err)
	}
	if len(applied) != 0 {
		t.Errorf("capture wrote %d grooming_mutation_applied rows, want 0 — capture applies NOTHING", len(applied))
	}
}

// TestRecordGroomingDispositionsFullPath_RunBoundTokenRefusedAtBothLayers is
// the operator-only guarantee proved through the REAL stack rather than a fake:
// a run-bound "mcp:run:<uuid>" token is refused by the real handler and the
// refusal reaches the tool caller naming run_token_forbidden. The subject is
// spelled as a literal, seeded by construction.
func TestRecordGroomingDispositionsFullPath_RunBoundTokenRefusedAtBothLayers(t *testing.T) {
	ctx := context.Background()
	pool := pgtest.NewPool(t)
	runRepo := run.NewPostgresRepository(pool)
	artRepo := artifact.NewPostgresRepository(pool)
	auditRepo := audit.NewPostgresRepository(pool)

	rn, err := runRepo.CreateRun(ctx, run.CreateRunParams{
		Repo: "kuhlman-labs/fishhawk", WorkflowID: "backlog_grooming",
		WorkflowSHA: "abc", TriggerSource: run.TriggerCLI,
	})
	if err != nil {
		t.Fatalf("create run: %v", err)
	}
	stage, err := runRepo.CreateStage(ctx, run.CreateStageParams{
		RunID: rn.ID, Sequence: 0, Type: run.StageTypePlan,
		ExecutorKind: run.ExecutorAgent, ExecutorRef: "claude-code",
	})
	if err != nil {
		t.Fatalf("create groom stage: %v", err)
	}
	body, hygieneID, _ := gdFullPathReport(t)
	sv := plan.GroomingReportVersion
	if _, err := artRepo.Create(ctx, artifact.CreateParams{
		StageID: stage.ID, Kind: artifact.KindGroomingReport,
		SchemaVersion: &sv, Content: body, ContentHash: "sha-run-bound",
	}); err != nil {
		t.Fatalf("create grooming_report artifact: %v", err)
	}

	const bearer = "fhm_gd_run_bound"
	tokRepo := &stubMCPAPITokens{tok: &apitoken.Token{
		ID: uuid.New(), Subject: "mcp:run:" + rn.ID.String(),
		Scopes: []string{"write:approvals"}, PlainText: bearer,
	}}
	s := server.New(server.Config{
		RunRepo: runRepo, ArtifactRepo: artRepo, AuditRepo: auditRepo, APITokenRepo: tokRepo,
	})
	httpSrv := httptest.NewServer(s.Handler())
	t.Cleanup(httpSrv.Close)
	r := &runResolver{api: newAPIClient(config{backendURL: httpSrv.URL, apiToken: bearer})}

	_, _, err = r.recordGroomingDispositions(ctx, nil, RecordGroomingDispositionsInput{
		RunID:        rn.ID.String(),
		Dispositions: []GroomingDispositionEntry{{EntryID: hygieneID, Verdict: "approved"}},
	})
	if err == nil {
		t.Fatal("a run-bound agent token recorded a disposition for its OWN run; the refusal must be unconditional")
	}
	if !strings.Contains(err.Error(), "run_token_forbidden") {
		t.Errorf("tool error = %q, want it to name run_token_forbidden", err)
	}
	rows, lerr := auditRepo.ListForRunByCategory(ctx, rn.ID, "grooming_disposition_recorded")
	if lerr != nil {
		t.Fatalf("list persisted rows: %v", lerr)
	}
	if len(rows) != 0 {
		t.Errorf("a refused run-bound capture persisted %d rows, want 0", len(rows))
	}
}

// gdFullPathReport builds a schema-valid grooming_report carrying one hygiene
// defect and one duplicate pair, and returns the body plus those two DECLARED
// entry ids.
func gdFullPathReport(t *testing.T) (body []byte, hygieneID, duplicateID string) {
	t.Helper()
	ref := func(n int) plan.ItemRef {
		id := fmt.Sprintf("kuhlman-labs/fishhawk#%d", n)
		return plan.ItemRef{Type: "github_issue", ID: id,
			URL: fmt.Sprintf("https://github.com/kuhlman-labs/fishhawk/issues/%d", n)}
	}
	h, pa, pb, o := ref(11), ref(15), ref(16), ref(14)
	hygieneID = plan.GroomingEntryID(plan.GroomingClassHygiene, "missing_label_namespace", h)
	duplicateID = plan.GroomingEntryID(plan.GroomingClassDuplicate, "", pa, pb)
	report := &plan.GroomingReport{
		Kind:          plan.KindGroomingReport,
		ReportVersion: plan.GroomingReportVersion,
		TicketReference: plan.TicketReference{
			Type: plan.TicketType("github_issue"), ID: "kuhlman-labs/fishhawk#2843",
			URL: "https://github.com/kuhlman-labs/fishhawk/issues/2843",
		},
		GeneratedBy: plan.GeneratedBy{Agent: "test", Model: "test-model",
			Timestamp: time.Date(2026, 8, 24, 0, 0, 0, 0, time.UTC)},
		Summary: "full-path fixture",
		Ordering: []plan.OrderingEntry{{
			ID: plan.GroomingEntryID(plan.GroomingClassOrdering, "", o), ItemRef: o,
			Rank: 1, Score: 90, RubricCitations: []plan.RubricCitation{{RubricID: "V1"}},
		}},
		Duplicates: []plan.DuplicateCandidate{{
			ID: duplicateID, Pair: []plan.ItemRef{pa, pb},
			Basis: "same defect", Confidence: "high",
		}},
		HygieneDefects: []plan.HygieneDefect{{
			ID: hygieneID, ItemRef: h, Defect: "missing_label_namespace",
			Detail: "no area: label", Fix: &plan.HygieneFix{Labels: []string{"area:api"}},
		}},
		DependencyEdges:          []plan.DependencyEdge{},
		VisionDrift:              []plan.VisionDriftFlag{},
		DecompositionSuggestions: []plan.DecompositionSuggestion{},
	}
	b, err := json.Marshal(report)
	if err != nil {
		t.Fatalf("marshal full-path report: %v", err)
	}
	// Parse it back through the production parser so a fixture the schema would
	// reject fails HERE rather than as a mysterious 500 in the test under test.
	if _, perr := plan.ParseGroomingReport(b); perr != nil {
		t.Fatalf("fixture report is not schema-valid: %v", perr)
	}
	return b, hygieneID, duplicateID
}
