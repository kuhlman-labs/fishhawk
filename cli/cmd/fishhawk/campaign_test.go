package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/cli/internal/httpclient"
)

// campaignFake is a configurable backend for the campaign end-to-end
// tests. It serves the four campaign endpoints the CLI drives and
// captures the last create request body + path so a test can assert the
// wiring (method / path / body) the CLI issued.
type campaignFake struct {
	mu sync.Mutex

	createStatus  int
	createErrCode string
	createResp    httpclient.Campaign
	createBody    map[string]any
	createHit     bool

	statusStatus  int
	statusErrCode string
	statusResp    httpclient.CampaignStatus
	statusHit     bool

	listStatus int
	listResp   httpclient.ListCampaignsResult
	listQuery  string

	resumeStatus  int
	resumeErrCode string
	resumeResp    httpclient.Campaign
	resumeID      string
}

func newCampaignFake(t *testing.T) (*campaignFake, *httptest.Server) {
	t.Helper()
	fb := &campaignFake{
		createStatus: http.StatusCreated,
		statusStatus: http.StatusOK,
		listStatus:   http.StatusOK,
		resumeStatus: http.StatusOK,
	}
	writeErr := func(w http.ResponseWriter, status int, code string) {
		details := map[string]any{}
		if code == "insufficient_scope" {
			details["required_scope"] = "write:campaigns"
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"error": map[string]any{"code": code, "message": "rejected", "details": details},
		})
	}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v0/campaigns", func(w http.ResponseWriter, r *http.Request) {
		var in map[string]any
		_ = json.NewDecoder(r.Body).Decode(&in)
		fb.mu.Lock()
		fb.createHit = true
		fb.createBody = in
		fb.mu.Unlock()
		if fb.createStatus >= 400 {
			writeErr(w, fb.createStatus, orDefault(fb.createErrCode, "validation_failed"))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(fb.createStatus)
		_ = json.NewEncoder(w).Encode(fb.createResp)
	})
	mux.HandleFunc("GET /v0/campaigns", func(w http.ResponseWriter, r *http.Request) {
		fb.mu.Lock()
		fb.listQuery = r.URL.RawQuery
		fb.mu.Unlock()
		if fb.listStatus >= 400 {
			writeErr(w, fb.listStatus, "internal_error")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(fb.listStatus)
		_ = json.NewEncoder(w).Encode(fb.listResp)
	})
	mux.HandleFunc("GET /v0/campaigns/{campaign_id}/status", func(w http.ResponseWriter, r *http.Request) {
		fb.mu.Lock()
		fb.statusHit = true
		fb.mu.Unlock()
		if fb.statusStatus >= 400 {
			writeErr(w, fb.statusStatus, orDefault(fb.statusErrCode, "campaign_not_found"))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(fb.statusStatus)
		_ = json.NewEncoder(w).Encode(fb.statusResp)
	})
	mux.HandleFunc("POST /v0/campaigns/{campaign_id}/resume", func(w http.ResponseWriter, r *http.Request) {
		fb.mu.Lock()
		fb.resumeID = r.PathValue("campaign_id")
		fb.mu.Unlock()
		if fb.resumeStatus >= 400 {
			writeErr(w, fb.resumeStatus, orDefault(fb.resumeErrCode, "campaign_not_paused"))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(fb.resumeStatus)
		_ = json.NewEncoder(w).Encode(fb.resumeResp)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return fb, srv
}

func orDefault(v, def string) string {
	if v == "" {
		return def
	}
	return v
}

// --- campaign start ---

func TestCampaignStart_HappyPath(t *testing.T) {
	fb, srv := newCampaignFake(t)
	withBackend(t, srv)
	id := uuid.New()
	fb.createResp = httpclient.Campaign{
		ID: id, Repo: "kuhlman-labs/fishhawk", EpicRef: "issue:1439",
		State: "pending", PausePolicy: "pause_item",
	}

	var stdout strings.Builder
	got := run([]string{
		"campaign", "start",
		"--repo", "kuhlman-labs/fishhawk", "--epic", "issue:1439", "--pause-policy", "pause_item",
	}, &stdout, io.Discard)
	if got != exitOK {
		t.Fatalf("status = %d, want exitOK", got)
	}
	if fb.createBody["repo"] != "kuhlman-labs/fishhawk" {
		t.Errorf("repo = %v", fb.createBody["repo"])
	}
	if fb.createBody["epic_ref"] != "issue:1439" {
		t.Errorf("epic_ref = %v, want issue:1439", fb.createBody["epic_ref"])
	}
	if fb.createBody["pause_policy"] != "pause_item" {
		t.Errorf("pause_policy = %v, want pause_item", fb.createBody["pause_policy"])
	}
	out := stdout.String()
	for _, want := range []string{id.String(), "kuhlman-labs/fishhawk", "issue:1439", "pause_item"} {
		if !strings.Contains(out, want) {
			t.Errorf("stdout missing %q:\n%s", want, out)
		}
	}
}

// TestCampaignStart_EpicNormalization asserts that both the #N and
// issue:N input forms normalize to the wire 'issue:N' epic_ref.
func TestCampaignStart_EpicNormalization(t *testing.T) {
	for _, in := range []string{"#1439", "issue:1439", "1439",
		"https://github.com/kuhlman-labs/fishhawk/issues/1439"} {
		t.Run(in, func(t *testing.T) {
			fb, srv := newCampaignFake(t)
			withBackend(t, srv)
			fb.createResp = httpclient.Campaign{ID: uuid.New(), State: "pending"}

			got := run([]string{
				"campaign", "start", "--repo", "x/y", "--epic", in,
			}, io.Discard, io.Discard)
			if got != exitOK {
				t.Fatalf("status = %d, want exitOK", got)
			}
			if fb.createBody["epic_ref"] != "issue:1439" {
				t.Errorf("epic_ref = %v, want issue:1439 (input %q)", fb.createBody["epic_ref"], in)
			}
		})
	}
}

// TestCampaignStart_DefaultPausePolicyOmitted asserts that with no
// --pause-policy the field is omitted from the body (omitempty), so the
// backend applies its default.
func TestCampaignStart_DefaultPausePolicyOmitted(t *testing.T) {
	fb, srv := newCampaignFake(t)
	withBackend(t, srv)
	fb.createResp = httpclient.Campaign{ID: uuid.New(), State: "pending"}

	got := run([]string{
		"campaign", "start", "--repo", "x/y", "--epic", "1439",
	}, io.Discard, io.Discard)
	if got != exitOK {
		t.Fatalf("status = %d, want exitOK", got)
	}
	if _, present := fb.createBody["pause_policy"]; present {
		t.Errorf("pause_policy present when not supplied: %v", fb.createBody)
	}
}

func TestCampaignStart_JSONOutput(t *testing.T) {
	fb, srv := newCampaignFake(t)
	withBackend(t, srv)
	id := uuid.New()
	fb.createResp = httpclient.Campaign{ID: id, Repo: "x/y", EpicRef: "issue:1439", State: "pending", PausePolicy: "pause_campaign"}

	var stdout strings.Builder
	got := run([]string{"campaign", "start", "--output", "json", "--repo", "x/y", "--epic", "1439"}, &stdout, io.Discard)
	if got != exitOK {
		t.Fatalf("status = %d", got)
	}
	var decoded httpclient.Campaign
	if err := json.NewDecoder(strings.NewReader(stdout.String())).Decode(&decoded); err != nil {
		t.Fatalf("decode json: %v\nstdout: %s", err, stdout.String())
	}
	if decoded.ID != id || decoded.EpicRef != "issue:1439" {
		t.Errorf("decoded mismatch: %+v", decoded)
	}
}

// TestCampaignStart_InvalidPausePolicy asserts the local validation
// rejects an unrecognized --pause-policy WITHOUT reaching the backend.
func TestCampaignStart_InvalidPausePolicy(t *testing.T) {
	fb, srv := newCampaignFake(t)
	withBackend(t, srv)

	var stderr strings.Builder
	got := run([]string{
		"campaign", "start", "--repo", "x/y", "--epic", "1439", "--pause-policy", "pause_world",
	}, io.Discard, &stderr)
	if got != exitUsage {
		t.Errorf("status = %d, want exitUsage", got)
	}
	if !strings.Contains(stderr.String(), "invalid --pause-policy") {
		t.Errorf("stderr missing diagnostic: %s", stderr.String())
	}
	if fb.createHit {
		t.Errorf("backend POST reached despite local pause-policy validation failure")
	}
}

func TestCampaignStart_MissingRepo(t *testing.T) {
	fb, srv := newCampaignFake(t)
	withBackend(t, srv)
	var stderr strings.Builder
	got := run([]string{"campaign", "start", "--epic", "1439"}, io.Discard, &stderr)
	if got != exitUsage {
		t.Errorf("status = %d, want exitUsage", got)
	}
	if !strings.Contains(stderr.String(), "--repo and --epic are required") {
		t.Errorf("stderr missing diagnostic: %s", stderr.String())
	}
	if fb.createHit {
		t.Errorf("backend reached despite missing --repo")
	}
}

func TestCampaignStart_MissingEpic(t *testing.T) {
	fb, srv := newCampaignFake(t)
	withBackend(t, srv)
	var stderr strings.Builder
	got := run([]string{"campaign", "start", "--repo", "x/y"}, io.Discard, &stderr)
	if got != exitUsage {
		t.Errorf("status = %d, want exitUsage", got)
	}
	if !strings.Contains(stderr.String(), "--repo and --epic are required") {
		t.Errorf("stderr missing diagnostic: %s", stderr.String())
	}
	if fb.createHit {
		t.Errorf("backend reached despite missing --epic")
	}
}

// TestCampaignStart_BadEpic asserts an unparseable --epic fails
// exitUsage without a backend hit.
func TestCampaignStart_BadEpic(t *testing.T) {
	fb, srv := newCampaignFake(t)
	withBackend(t, srv)
	var stderr strings.Builder
	got := run([]string{"campaign", "start", "--repo", "x/y", "--epic", "not-a-ref"}, io.Discard, &stderr)
	if got != exitUsage {
		t.Errorf("status = %d, want exitUsage", got)
	}
	if !strings.Contains(stderr.String(), "--epic") {
		t.Errorf("stderr missing diagnostic: %s", stderr.String())
	}
	if fb.createHit {
		t.Errorf("backend reached despite unparseable --epic")
	}
}

func TestCampaignStart_InsufficientScope403(t *testing.T) {
	fb, srv := newCampaignFake(t)
	withBackend(t, srv)
	fb.createStatus = http.StatusForbidden
	fb.createErrCode = "insufficient_scope"

	var stderr strings.Builder
	got := run([]string{"campaign", "start", "--repo", "x/y", "--epic", "1439"}, io.Discard, &stderr)
	if got != exitFailure {
		t.Errorf("status = %d, want exitFailure", got)
	}
	if !strings.Contains(stderr.String(), "insufficient_scope") {
		t.Errorf("stderr missing api code: %s", stderr.String())
	}
}

// --- campaign status ---

func TestCampaignStatus_HappyPath(t *testing.T) {
	fb, srv := newCampaignFake(t)
	withBackend(t, srv)
	id := uuid.New()
	runID := uuid.New()
	fb.statusResp = httpclient.CampaignStatus{
		Campaign: httpclient.Campaign{ID: id, Repo: "x/y", EpicRef: "issue:1439", State: "running", PausePolicy: "pause_campaign"},
		Items: []httpclient.CampaignItem{
			{ID: uuid.New(), IssueRef: "issue:1441", State: "running", RunID: &runID},
			{ID: uuid.New(), IssueRef: "issue:1442", State: "blocked"},
		},
		Rollup: httpclient.CampaignRollup{Running: []string{"issue:1441"}, Blocked: []string{"issue:1442"}},
		NextAction: httpclient.CampaignNextAction{
			Action: "wait", Detail: "items are running or blocked",
		},
	}

	var stdout strings.Builder
	got := run([]string{"campaign", "status", id.String()}, &stdout, io.Discard)
	if got != exitOK {
		t.Fatalf("status = %d, want exitOK", got)
	}
	out := stdout.String()
	for _, want := range []string{
		id.String(), "issue:1439", "running",
		"next_action:", "wait", "items are running or blocked",
		"issue:1441", runID.String(),
		"issue:1442", "blocked",
		"-", // unlinked item's run_id placeholder
	} {
		if !strings.Contains(out, want) {
			t.Errorf("stdout missing %q:\n%s", want, out)
		}
	}
}

// TestCampaignStatus_NextActionIssueRef asserts the next_action line
// includes the action AND its issue_ref when present (the start_run /
// attention / resume shapes).
func TestCampaignStatus_NextActionIssueRef(t *testing.T) {
	fb, srv := newCampaignFake(t)
	withBackend(t, srv)
	id := uuid.New()
	fb.statusResp = httpclient.CampaignStatus{
		Campaign:   httpclient.Campaign{ID: id, State: "running"},
		Items:      []httpclient.CampaignItem{{ID: uuid.New(), IssueRef: "issue:1441", State: "pending"}},
		Rollup:     httpclient.CampaignRollup{Eligible: []string{"issue:1441"}},
		NextAction: httpclient.CampaignNextAction{Action: "start_run", IssueRef: "issue:1441", Detail: "ready to dispatch"},
	}

	var stdout strings.Builder
	got := run([]string{"campaign", "status", id.String()}, &stdout, io.Discard)
	if got != exitOK {
		t.Fatalf("status = %d", got)
	}
	if !strings.Contains(stdout.String(), "start_run issue:1441") {
		t.Errorf("next_action line missing 'start_run issue:1441':\n%s", stdout.String())
	}
}

func TestCampaignStatus_JSONOutput(t *testing.T) {
	fb, srv := newCampaignFake(t)
	withBackend(t, srv)
	id := uuid.New()
	fb.statusResp = httpclient.CampaignStatus{
		Campaign:   httpclient.Campaign{ID: id, State: "running"},
		Items:      []httpclient.CampaignItem{{ID: uuid.New(), IssueRef: "issue:1441", State: "pending", DependsOn: []string{}}},
		Rollup:     httpclient.CampaignRollup{Eligible: []string{"issue:1441"}},
		NextAction: httpclient.CampaignNextAction{Action: "start_run", IssueRef: "issue:1441"},
	}

	var stdout strings.Builder
	got := run([]string{"campaign", "status", "--output", "json", id.String()}, &stdout, io.Discard)
	if got != exitOK {
		t.Fatalf("status = %d", got)
	}
	var decoded httpclient.CampaignStatus
	if err := json.NewDecoder(strings.NewReader(stdout.String())).Decode(&decoded); err != nil {
		t.Fatalf("decode json: %v\nstdout: %s", err, stdout.String())
	}
	if decoded.Campaign.ID != id || decoded.NextAction.Action != "start_run" || len(decoded.Items) != 1 {
		t.Errorf("decoded mismatch: %+v", decoded)
	}
}

func TestCampaignStatus_BadUUID(t *testing.T) {
	fb, srv := newCampaignFake(t)
	withBackend(t, srv)
	var stderr strings.Builder
	got := run([]string{"campaign", "status", "not-a-uuid"}, io.Discard, &stderr)
	if got != exitUsage {
		t.Errorf("status = %d, want exitUsage", got)
	}
	if !strings.Contains(stderr.String(), "not a UUID") {
		t.Errorf("stderr missing 'not a UUID': %s", stderr.String())
	}
	if fb.statusHit {
		t.Errorf("status backend reached despite local UUID validation failure")
	}
}

func TestCampaignStatus_MissingArg(t *testing.T) {
	_, srv := newCampaignFake(t)
	withBackend(t, srv)
	var stderr strings.Builder
	got := run([]string{"campaign", "status"}, io.Discard, &stderr)
	if got != exitUsage {
		t.Errorf("status = %d, want exitUsage", got)
	}
	if !strings.Contains(stderr.String(), "<campaign-id> required") {
		t.Errorf("stderr missing diagnostic: %s", stderr.String())
	}
}

// --- campaign list ---

func TestCampaignList_HappyPath(t *testing.T) {
	fb, srv := newCampaignFake(t)
	withBackend(t, srv)
	id := uuid.New()
	fb.listResp = httpclient.ListCampaignsResult{
		Items:      []httpclient.Campaign{{ID: id, Repo: "x/y", EpicRef: "issue:1439", State: "running"}},
		NextCursor: "next-page-token",
	}

	var stdout strings.Builder
	got := run([]string{"campaign", "list"}, &stdout, io.Discard)
	if got != exitOK {
		t.Fatalf("status = %d, want exitOK", got)
	}
	out := stdout.String()
	for _, want := range []string{id.String(), "x/y", "issue:1439", "running", "More: --cursor next-page-token"} {
		if !strings.Contains(out, want) {
			t.Errorf("stdout missing %q:\n%s", want, out)
		}
	}
}

// TestCampaignList_Empty asserts the '(no campaigns)' line and no
// cursor trailer when the page is empty.
func TestCampaignList_Empty(t *testing.T) {
	fb, srv := newCampaignFake(t)
	withBackend(t, srv)
	fb.listResp = httpclient.ListCampaignsResult{}

	var stdout strings.Builder
	got := run([]string{"campaign", "list"}, &stdout, io.Discard)
	if got != exitOK {
		t.Fatalf("status = %d", got)
	}
	out := stdout.String()
	if !strings.Contains(out, "(no campaigns)") {
		t.Errorf("stdout missing '(no campaigns)':\n%s", out)
	}
	if strings.Contains(out, "More:") {
		t.Errorf("cursor trailer present on empty list:\n%s", out)
	}
}

// TestCampaignList_NoCursorTrailer asserts the 'More:' trailer is
// absent when NextCursor is empty even with items present.
func TestCampaignList_NoCursorTrailer(t *testing.T) {
	fb, srv := newCampaignFake(t)
	withBackend(t, srv)
	fb.listResp = httpclient.ListCampaignsResult{
		Items: []httpclient.Campaign{{ID: uuid.New(), Repo: "x/y", EpicRef: "issue:1439", State: "running"}},
	}

	var stdout strings.Builder
	got := run([]string{"campaign", "list"}, &stdout, io.Discard)
	if got != exitOK {
		t.Fatalf("status = %d", got)
	}
	if strings.Contains(stdout.String(), "More:") {
		t.Errorf("cursor trailer present when NextCursor empty:\n%s", stdout.String())
	}
}

// TestCampaignList_StateFilterPassthrough asserts the --state and --repo
// filters reach the query string.
func TestCampaignList_StateFilterPassthrough(t *testing.T) {
	fb, srv := newCampaignFake(t)
	withBackend(t, srv)
	fb.listResp = httpclient.ListCampaignsResult{}

	got := run([]string{"campaign", "list", "--repo", "x/y", "--state", "running", "--limit", "10"}, io.Discard, io.Discard)
	if got != exitOK {
		t.Fatalf("status = %d", got)
	}
	for _, want := range []string{"repo=x%2Fy", "state=running", "limit=10"} {
		if !strings.Contains(fb.listQuery, want) {
			t.Errorf("query %q missing %q", fb.listQuery, want)
		}
	}
}

// --- campaign resume ---

func TestCampaignResume_HappyPath(t *testing.T) {
	fb, srv := newCampaignFake(t)
	withBackend(t, srv)
	id := uuid.New()
	fb.resumeResp = httpclient.Campaign{ID: id, Repo: "x/y", EpicRef: "issue:1439", State: "running", PausePolicy: "pause_campaign"}

	var stdout strings.Builder
	got := run([]string{"campaign", "resume", id.String()}, &stdout, io.Discard)
	if got != exitOK {
		t.Fatalf("status = %d, want exitOK", got)
	}
	if fb.resumeID != id.String() {
		t.Errorf("resume hit %s, want %s", fb.resumeID, id)
	}
	if !strings.Contains(stdout.String(), "running") {
		t.Errorf("stdout missing resumed state:\n%s", stdout.String())
	}
}

func TestCampaignResume_NotPaused409(t *testing.T) {
	fb, srv := newCampaignFake(t)
	withBackend(t, srv)
	fb.resumeStatus = http.StatusConflict
	fb.resumeErrCode = "campaign_not_paused"

	var stderr strings.Builder
	got := run([]string{"campaign", "resume", uuid.New().String()}, io.Discard, &stderr)
	if got != exitFailure {
		t.Errorf("status = %d, want exitFailure", got)
	}
	if !strings.Contains(stderr.String(), "campaign_not_paused") {
		t.Errorf("stderr missing api code: %s", stderr.String())
	}
}

func TestCampaignResume_InsufficientScope403(t *testing.T) {
	fb, srv := newCampaignFake(t)
	withBackend(t, srv)
	fb.resumeStatus = http.StatusForbidden
	fb.resumeErrCode = "insufficient_scope"

	var stderr strings.Builder
	got := run([]string{"campaign", "resume", uuid.New().String()}, io.Discard, &stderr)
	if got != exitFailure {
		t.Errorf("status = %d, want exitFailure", got)
	}
	if !strings.Contains(stderr.String(), "insufficient_scope") {
		t.Errorf("stderr missing api code: %s", stderr.String())
	}
}

func TestCampaignResume_BadUUID(t *testing.T) {
	fb, srv := newCampaignFake(t)
	withBackend(t, srv)
	var stderr strings.Builder
	got := run([]string{"campaign", "resume", "not-a-uuid"}, io.Discard, &stderr)
	if got != exitUsage {
		t.Errorf("status = %d, want exitUsage", got)
	}
	if !strings.Contains(stderr.String(), "not a UUID") {
		t.Errorf("stderr missing 'not a UUID': %s", stderr.String())
	}
	if fb.resumeID != "" {
		t.Errorf("resume endpoint reached despite local UUID validation failure")
	}
}

func TestCampaignResume_MissingArg(t *testing.T) {
	_, srv := newCampaignFake(t)
	withBackend(t, srv)
	var stderr strings.Builder
	got := run([]string{"campaign", "resume"}, io.Discard, &stderr)
	if got != exitUsage {
		t.Errorf("status = %d, want exitUsage", got)
	}
	if !strings.Contains(stderr.String(), "<campaign-id> required") {
		t.Errorf("stderr missing diagnostic: %s", stderr.String())
	}
}

// --- dispatcher ---

func TestCampaign_NoSubcommand(t *testing.T) {
	var stderr strings.Builder
	got := run([]string{"campaign"}, io.Discard, &stderr)
	if got != exitUsage {
		t.Errorf("status = %d, want exitUsage", got)
	}
	if !strings.Contains(stderr.String(), "subcommand required") {
		t.Errorf("stderr missing diagnostic: %s", stderr.String())
	}
}

func TestCampaign_UnknownSubcommand(t *testing.T) {
	var stderr strings.Builder
	got := run([]string{"campaign", "frobnicate"}, io.Discard, &stderr)
	if got != exitUsage {
		t.Errorf("status = %d, want exitUsage", got)
	}
	if !strings.Contains(stderr.String(), "unknown subcommand") {
		t.Errorf("stderr missing diagnostic: %s", stderr.String())
	}
}
