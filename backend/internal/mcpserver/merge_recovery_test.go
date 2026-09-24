package mcpserver

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
)

// mergeRecoveryResolver builds a resolver pointed at a bespoke httptest backend
// running h. A bespoke fake — rather than the shared fakeBackend — is what makes
// the NEVER-DIALED seam below possible: the handler can t.Fatal on any request,
// so the pre-hop UUID guard's counterfactual RED lands on a behavioural
// assertion instead of on fixture setup.
func mergeRecoveryResolver(t *testing.T, h http.HandlerFunc) *runResolver {
	t.Helper()
	ts := httptest.NewServer(h)
	t.Cleanup(ts.Close)
	return &runResolver{
		api:    newAPIClient(config{backendURL: ts.URL, apiToken: "tok-test"}),
		getenv: envFuncFromMap(nil),
	}
}

// mergeRecoveryNeverDialed is a backend that FAILS the test if it is reached at
// all. It is the counterfactual vehicle for the pre-hop uuid.Parse guards.
func mergeRecoveryNeverDialed(t *testing.T) http.HandlerFunc {
	t.Helper()
	return func(_ http.ResponseWriter, r *http.Request) {
		t.Errorf("the backend was dialed (%s %s), but the malformed run_id must be refused BEFORE the HTTP hop",
			r.Method, r.URL.Path)
	}
}

// writeJSONErr writes the backend's OpenAPI error envelope.
func writeJSONErr(w http.ResponseWriter, status int, code, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = io.WriteString(w, `{"error":{"code":"`+code+`","message":"`+msg+`"}}`)
}

// ---------------------------------------------------------------------------
// fishhawk_record_merge_observation
// ---------------------------------------------------------------------------

// TestRecordMergeObservation_RecordsAndNamesNextVerb covers the success arm: the
// observation decodes field-for-field and the message names the SETTLE verb,
// because the observe verb settles nothing.
func TestRecordMergeObservation_RecordsAndNamesNextVerb(t *testing.T) {
	runID := uuid.New()
	var gotMethod, gotPath string
	r := mergeRecoveryResolver(t, func(w http.ResponseWriter, req *http.Request) {
		gotMethod, gotPath = req.Method, req.URL.Path
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"run_id":"`+runID.String()+`","already_recorded":false,`+
			`"observation":{"pull_request_url":"https://github.com/kuhlman-labs/fishhawk/pull/4242",`+
			`"pull_request_number":4242,"merge_commit_sha":"deadbeefcafe",`+
			`"merged_at":"2026-09-20T10:00:00Z","observed_at":"2026-09-23T11:30:00Z"}}`)
	})

	_, out, err := r.recordMergeObservation(context.Background(), nil,
		RecordMergeObservationInput{RunID: runID.String()})
	if err != nil {
		t.Fatalf("recordMergeObservation: %v", err)
	}
	if gotMethod != http.MethodPost {
		t.Errorf("method = %s, want POST", gotMethod)
	}
	if want := "/v0/runs/" + runID.String() + "/record-merge-observation"; gotPath != want {
		t.Errorf("path = %q, want %q", gotPath, want)
	}
	if out.AlreadyRecorded {
		t.Error("already_recorded = true, want false on the recording arm")
	}
	if out.Observation.PullRequestNumber != 4242 || out.Observation.MergeCommitSHA != "deadbeefcafe" {
		t.Errorf("observation = %+v, want the PR number and merge commit decoded", out.Observation)
	}
	// merged_at and observed_at are DISTINCT instants on purpose; asserting each
	// by value is what makes a transposed json tag on the client mirror RED.
	if out.Observation.MergedAt != "2026-09-20T10:00:00Z" {
		t.Errorf("merged_at = %q, want the FORGE's merge timestamp", out.Observation.MergedAt)
	}
	if out.Observation.ObservedAt != "2026-09-23T11:30:00Z" {
		t.Errorf("observed_at = %q, want the instant Fishhawk read it", out.Observation.ObservedAt)
	}
	for _, want := range []string{"recorded one merge_observation_recorded row", "SETTLES NOTHING", "fishhawk_reconcile_merge"} {
		if !strings.Contains(out.Message, want) {
			t.Errorf("message missing %q: %q", want, out.Message)
		}
	}
}

// TestRecordMergeObservation_AlreadyRecordedClaimsNoRow covers the idempotent
// no-op arm. The observation block must be EMPTY: the backend deliberately
// zeroes it so the response cannot claim a row it did not write.
func TestRecordMergeObservation_AlreadyRecordedClaimsNoRow(t *testing.T) {
	runID := uuid.New()
	r := mergeRecoveryResolver(t, func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"run_id":"`+runID.String()+`","already_recorded":true,"observation":{}}`)
	})

	_, out, err := r.recordMergeObservation(context.Background(), nil,
		RecordMergeObservationInput{RunID: runID.String()})
	if err != nil {
		t.Fatalf("recordMergeObservation: %v", err)
	}
	if !out.AlreadyRecorded {
		t.Fatal("already_recorded = false, want true")
	}
	if out.Observation != (RecordMergeObservationObservation{}) {
		t.Errorf("observation = %+v, want the ZERO value — the no-op arm appended nothing and must claim nothing", out.Observation)
	}
	for _, want := range []string{"recorded NOTHING", "fishhawk_reconcile_merge"} {
		if !strings.Contains(out.Message, want) {
			t.Errorf("message missing %q: %q", want, out.Message)
		}
	}
}

// TestRecordMergeObservation_InvalidUUIDMakesNoRequest is the counterfactual
// vehicle for the pre-hop uuid.Parse guard (plan counterfactual (a)): the
// backend fails the test if it is dialed at all.
func TestRecordMergeObservation_InvalidUUIDMakesNoRequest(t *testing.T) {
	r := mergeRecoveryResolver(t, mergeRecoveryNeverDialed(t))

	_, _, err := r.recordMergeObservation(context.Background(), nil,
		RecordMergeObservationInput{RunID: "not-a-uuid"})
	if err == nil {
		t.Fatal("want an error for a malformed run_id")
	}
	if !strings.Contains(err.Error(), "not a valid UUID") {
		t.Errorf("error = %v, want the UUID refusal", err)
	}
}

// TestRecordMergeObservation_NamedRefusalsSurfaceVerbatim covers one case per
// named refusal of the observe endpoint, asserting the backend's error CODE
// reaches the caller rather than being flattened into a generic failure.
func TestRecordMergeObservation_NamedRefusalsSurfaceVerbatim(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		code   string
	}{
		{"no pull request", http.StatusConflict, "record_merge_observation_no_pull_request"},
		{"malformed pr url", http.StatusConflict, "record_merge_observation_malformed_pr_url"},
		{"pr url repo mismatch", http.StatusConflict, "record_merge_observation_pr_url_repo_mismatch"},
		{"pr not merged", http.StatusConflict, "record_merge_observation_pr_not_merged"},
		{"no merge commit", http.StatusConflict, "record_merge_observation_no_merge_commit"},
		{"no merge timestamp", http.StatusConflict, "record_merge_observation_no_merge_timestamp"},
		{"forge unavailable", http.StatusBadGateway, "record_merge_observation_forge_unavailable"},
		{"unconfigured", http.StatusServiceUnavailable, "record_merge_observation_unconfigured"},
		{"run not found", http.StatusNotFound, "run_not_found"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := mergeRecoveryResolver(t, func(w http.ResponseWriter, _ *http.Request) {
				writeJSONErr(w, tc.status, tc.code, "refused")
			})
			_, _, err := r.recordMergeObservation(context.Background(), nil,
				RecordMergeObservationInput{RunID: uuid.New().String()})
			if err == nil {
				t.Fatalf("want an error on %s", tc.code)
			}
			if !strings.Contains(err.Error(), tc.code) {
				t.Errorf("error = %v, want the backend code %q verbatim", err, tc.code)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// fishhawk_reconcile_merge
// ---------------------------------------------------------------------------

// TestReconcileMerge_SupersedesAndReportsRunState covers the success arm: the
// moved and repaired rows decode and the message names the resulting run state,
// which is how an operator sees whether the reconcile actually settled the run.
func TestReconcileMerge_SupersedesAndReportsRunState(t *testing.T) {
	runID := uuid.New()
	stageID := uuid.New()
	repairedID := uuid.New()
	var gotMethod, gotPath string
	r := mergeRecoveryResolver(t, func(w http.ResponseWriter, req *http.Request) {
		gotMethod, gotPath = req.Method, req.URL.Path
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"run_id":"`+runID.String()+`",`+
			`"superseded":[{"stage_id":"`+stageID.String()+`","stage_type":"acceptance",`+
			`"from_state":"awaiting_input","reason":"merge_superseded"}],`+
			`"repaired":[{"stage_id":"`+repairedID.String()+`","stage_type":"implement",`+
			`"from_state":"superseded","reason":"missing_audit_row"}],`+
			`"run_state":"succeeded"}`)
	})

	_, out, err := r.reconcileMerge(context.Background(), nil, ReconcileMergeInput{RunID: runID.String()})
	if err != nil {
		t.Fatalf("reconcileMerge: %v", err)
	}
	if gotMethod != http.MethodPost {
		t.Errorf("method = %s, want POST", gotMethod)
	}
	if want := "/v0/runs/" + runID.String() + "/reconcile-merge"; gotPath != want {
		t.Errorf("path = %q, want %q", gotPath, want)
	}
	if len(out.Superseded) != 1 || out.Superseded[0].StageID != stageID.String() ||
		out.Superseded[0].FromState != "awaiting_input" {
		t.Fatalf("superseded = %+v, want the moved stage decoded", out.Superseded)
	}
	if len(out.Repaired) != 1 || out.Repaired[0].StageID != repairedID.String() {
		t.Fatalf("repaired = %+v, want the repaired stage decoded", out.Repaired)
	}
	if out.RunState != "succeeded" {
		t.Errorf("run_state = %q, want succeeded", out.RunState)
	}
	for _, want := range []string{"superseded 1 parked stage", "acceptance parked at awaiting_input",
		"re-appended the missing audit row for 1", "state succeeded"} {
		if !strings.Contains(out.Message, want) {
			t.Errorf("message missing %q: %q", want, out.Message)
		}
	}
}

// TestReconcileMerge_IdempotentSecondCall covers the repeat arm: two empty lists
// and the run state passed through, reported as a no-op rather than a false
// success.
func TestReconcileMerge_IdempotentSecondCall(t *testing.T) {
	runID := uuid.New()
	calls := 0
	r := mergeRecoveryResolver(t, func(w http.ResponseWriter, _ *http.Request) {
		calls++
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"run_id":"`+runID.String()+`","superseded":[],"repaired":[],"run_state":"succeeded"}`)
	})

	for i := 0; i < 2; i++ {
		_, out, err := r.reconcileMerge(context.Background(), nil, ReconcileMergeInput{RunID: runID.String()})
		if err != nil {
			t.Fatalf("reconcileMerge call %d: %v", i+1, err)
		}
		if len(out.Superseded) != 0 || len(out.Repaired) != 0 {
			t.Errorf("call %d: superseded=%+v repaired=%+v, want two EMPTY lists", i+1, out.Superseded, out.Repaired)
		}
		if out.RunState != "succeeded" {
			t.Errorf("call %d: run_state = %q, want the state passed through", i+1, out.RunState)
		}
		if !strings.Contains(out.Message, "idempotent no-op") {
			t.Errorf("call %d: message does not report the no-op: %q", i+1, out.Message)
		}
	}
	if calls != 2 {
		t.Errorf("endpoint calls = %d, want 2", calls)
	}
}

// TestReconcileMerge_InvalidUUIDMakesNoRequest is the counterfactual vehicle for
// the settle verb's pre-hop uuid.Parse guard (plan counterfactual (b)).
func TestReconcileMerge_InvalidUUIDMakesNoRequest(t *testing.T) {
	r := mergeRecoveryResolver(t, mergeRecoveryNeverDialed(t))

	_, _, err := r.reconcileMerge(context.Background(), nil, ReconcileMergeInput{RunID: "not-a-uuid"})
	if err == nil {
		t.Fatal("want an error for a malformed run_id")
	}
	if !strings.Contains(err.Error(), "not a valid UUID") {
		t.Errorf("error = %v, want the UUID refusal", err)
	}
}

// TestReconcileMerge_NamedRefusalsSurfaceVerbatim covers one case per named
// refusal of the settle endpoint.
func TestReconcileMerge_NamedRefusalsSurfaceVerbatim(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		code   string
	}{
		{"pr not merged", http.StatusConflict, "reconcile_merge_pr_not_merged"},
		{"not applicable", http.StatusConflict, "reconcile_merge_not_applicable"},
		{"unconfigured", http.StatusServiceUnavailable, "reconcile_merge_unconfigured"},
		{"run not found", http.StatusNotFound, "run_not_found"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := mergeRecoveryResolver(t, func(w http.ResponseWriter, _ *http.Request) {
				writeJSONErr(w, tc.status, tc.code, "refused")
			})
			_, _, err := r.reconcileMerge(context.Background(), nil,
				ReconcileMergeInput{RunID: uuid.New().String()})
			if err == nil {
				t.Fatalf("want an error on %s", tc.code)
			}
			if !strings.Contains(err.Error(), tc.code) {
				t.Errorf("error = %v, want the backend code %q verbatim", err, tc.code)
			}
		})
	}
}

// TestMergeRecovery_RunIDFallsBackToTheRequestedID covers the defensive fallback
// on BOTH handlers: a backend that omits run_id must not leave the response
// claiming an empty run.
func TestMergeRecovery_RunIDFallsBackToTheRequestedID(t *testing.T) {
	runID := uuid.New()
	t.Run("record", func(t *testing.T) {
		r := mergeRecoveryResolver(t, func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"already_recorded":true}`)
		})
		_, out, err := r.recordMergeObservation(context.Background(), nil,
			RecordMergeObservationInput{RunID: runID.String()})
		if err != nil {
			t.Fatalf("recordMergeObservation: %v", err)
		}
		if out.RunID != runID.String() {
			t.Errorf("run_id = %q, want the requested id %q", out.RunID, runID)
		}
	})
	t.Run("reconcile", func(t *testing.T) {
		r := mergeRecoveryResolver(t, func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"run_state":"running"}`)
		})
		_, out, err := r.reconcileMerge(context.Background(), nil, ReconcileMergeInput{RunID: runID.String()})
		if err != nil {
			t.Fatalf("reconcileMerge: %v", err)
		}
		if out.RunID != runID.String() {
			t.Errorf("run_id = %q, want the requested id %q", out.RunID, runID)
		}
	})
}

// ---------------------------------------------------------------------------
// The pure message renderers
// ---------------------------------------------------------------------------

// TestRecordMergeObservationMessage tables the observe renderer's two arms,
// including the (unknown) placeholder for a field the backend omitted.
func TestRecordMergeObservationMessage(t *testing.T) {
	recorded := recordMergeObservationMessage(RecordMergeObservationOutput{
		Observation: RecordMergeObservationObservation{
			PullRequestURL: "https://example.test/pull/1",
			MergeCommitSHA: "abc123",
			MergedAt:       "2026-09-20T10:00:00Z",
			ObservedAt:     "2026-09-23T11:30:00Z",
		},
	})
	for _, want := range []string{"https://example.test/pull/1", "abc123",
		"2026-09-20T10:00:00Z", "2026-09-23T11:30:00Z", "SETTLES NOTHING", "fishhawk_reconcile_merge"} {
		if !strings.Contains(recorded, want) {
			t.Errorf("recorded arm missing %q: %q", want, recorded)
		}
	}

	noop := recordMergeObservationMessage(RecordMergeObservationOutput{AlreadyRecorded: true})
	for _, want := range []string{"recorded NOTHING", "observation block is empty", "fishhawk_reconcile_merge"} {
		if !strings.Contains(noop, want) {
			t.Errorf("no-op arm missing %q: %q", want, noop)
		}
	}
	if strings.Contains(noop, "recorded one merge_observation_recorded row") {
		t.Errorf("the no-op arm claims a recorded row: %q", noop)
	}

	sparse := recordMergeObservationMessage(RecordMergeObservationOutput{})
	if !strings.Contains(sparse, "(unknown)") {
		t.Errorf("an omitted field should render the (unknown) placeholder, not an empty gap: %q", sparse)
	}
}

// TestReconcileMergeMessage tables the settle renderer: superseded-only,
// repaired-only, both, the idempotent no-op, and the singular/plural wording.
func TestReconcileMergeMessage(t *testing.T) {
	moved := ReconcileMergeStage{StageType: "acceptance", FromState: "awaiting_input"}
	repaired := ReconcileMergeStage{StageType: "implement", FromState: "superseded"}

	one := reconcileMergeMessage(ReconcileMergeOutput{
		Superseded: []ReconcileMergeStage{moved}, RunState: "succeeded",
	})
	for _, want := range []string{"superseded 1 parked stage (", "acceptance parked at awaiting_input", "state succeeded"} {
		if !strings.Contains(one, want) {
			t.Errorf("superseded-only arm missing %q: %q", want, one)
		}
	}
	if strings.Contains(one, "re-appended") {
		t.Errorf("superseded-only arm should not mention repairs: %q", one)
	}

	many := reconcileMergeMessage(ReconcileMergeOutput{
		Superseded: []ReconcileMergeStage{moved, moved}, RunState: "succeeded",
	})
	if !strings.Contains(many, "superseded 2 parked stages") {
		t.Errorf("plural wording missing: %q", many)
	}

	repairOnly := reconcileMergeMessage(ReconcileMergeOutput{
		Repaired: []ReconcileMergeStage{repaired}, RunState: "succeeded",
	})
	if !strings.Contains(repairOnly, "re-appended the missing audit row for 1 already-superseded stage") {
		t.Errorf("repaired-only arm missing its clause: %q", repairOnly)
	}
	if strings.Contains(repairOnly, "superseded 1 parked") {
		t.Errorf("repaired-only arm should not claim a move: %q", repairOnly)
	}

	both := reconcileMergeMessage(ReconcileMergeOutput{
		Superseded: []ReconcileMergeStage{moved}, Repaired: []ReconcileMergeStage{repaired}, RunState: "succeeded",
	})
	if !strings.Contains(both, "; ") || !strings.Contains(both, "re-appended") {
		t.Errorf("both-arms message should join the two clauses: %q", both)
	}

	noop := reconcileMergeMessage(ReconcileMergeOutput{RunState: "running"})
	for _, want := range []string{"idempotent no-op", "state running", "completion_blocked"} {
		if !strings.Contains(noop, want) {
			t.Errorf("no-op arm missing %q: %q", want, noop)
		}
	}

	unknownState := reconcileMergeMessage(ReconcileMergeOutput{})
	if !strings.Contains(unknownState, "(unknown)") {
		t.Errorf("an omitted run_state should render the (unknown) placeholder: %q", unknownState)
	}

	// A row with no stage_type still renders a label rather than an empty gap.
	unlabelled := reconcileMergeMessage(ReconcileMergeOutput{
		Superseded: []ReconcileMergeStage{{}}, RunState: "running",
	})
	if !strings.Contains(unlabelled, "unknown") {
		t.Errorf("a type-less row should render the unknown label: %q", unlabelled)
	}
}

// TestMergeRecoveryToolOutputsMarshal proves both tool outputs marshal to the
// documented json shape — the wire surface an agent actually reads.
func TestMergeRecoveryToolOutputsMarshal(t *testing.T) {
	raw, err := json.Marshal(RecordMergeObservationOutput{
		RunID: "r", AlreadyRecorded: true, Message: "m",
	})
	if err != nil {
		t.Fatalf("marshal observe output: %v", err)
	}
	for _, want := range []string{`"run_id"`, `"already_recorded"`, `"observation"`, `"message"`} {
		if !strings.Contains(string(raw), want) {
			t.Errorf("observe output missing %s: %s", want, raw)
		}
	}
	raw, err = json.Marshal(ReconcileMergeOutput{RunID: "r", RunState: "running", Message: "m"})
	if err != nil {
		t.Fatalf("marshal reconcile output: %v", err)
	}
	for _, want := range []string{`"run_id"`, `"superseded"`, `"repaired"`, `"run_state"`, `"message"`} {
		if !strings.Contains(string(raw), want) {
			t.Errorf("reconcile output missing %s: %s", want, raw)
		}
	}
}
