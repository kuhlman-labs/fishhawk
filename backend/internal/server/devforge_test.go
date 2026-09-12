package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/forge"
	forgegitlab "github.com/kuhlman-labs/fishhawk/backend/internal/forge/gitlab"
	"github.com/kuhlman-labs/fishhawk/backend/internal/forge/stub"
	"github.com/kuhlman-labs/fishhawk/backend/internal/githubclient"
	"github.com/kuhlman-labs/fishhawk/backend/internal/pgtest"
	"github.com/kuhlman-labs/fishhawk/backend/internal/splitfiling"
	"github.com/kuhlman-labs/fishhawk/backend/internal/webhook"
	"github.com/kuhlman-labs/fishhawk/backend/internal/workmgmt"
)

// This file pins the dev-only stub-forge control surface (E72.3 / #3327,
// devforge.go) in two layers.
//
// The route layer drives each handler's happy path and every named refusal
// through New(cfg)'s REAL middleware chain from a loopback peer, plus the
// registration-off (404) and non-loopback (403) postures.
//
// The END-TO-END layer is the issue's done-means: a Server whose ONLY forges
// are the in-process stub (cfg.GitHub over stub.Transport with
// stub.StaticTokens; cfg.ForgeResolver -> a real *forgegitlab.Forge over the
// same transport), whose audit repo is pre-seeded with the split_children_filed
// linkage rows the split-parent-linked scenario materializes, driven ONLY via
// the control API: POST /v0/dev/forge/issues seeds the parent, POST
// /v0/dev/forge/deliveries signs and dispatches the contract child's close
// through the genuine receiver, and GET /v0/dev/forge/issues reads the
// COMMITTED parent state back. The split_parent_closed observation is then
// read back through GET /v0/audit?category=split_parent_closed with NO
// Authorization header — the exact observation path the credential-free
// acceptance criteria use (approval condition 1) — not through the store.

// --- audit fake ------------------------------------------------------------

// devForgeAudit is an ordering-faithful in-memory audit repository whose
// AppendGlobalChained rows land in the SAME list ListAll serves — the way
// Postgres behaves — so a watcher-written split_parent_closed row is
// readable through GET /v0/audit exactly as a seeded row is. ListAll honours
// ListAllParams.AccountID with the documented NULL-allow semantics (a set
// AccountID keeps rows whose AccountID matches OR is nil; empty keeps all),
// so the anonymous-read assertion below reflects the production contract
// rather than a fake that ignores scoping.
type devForgeAudit struct {
	audit.BaseFake
	mu      sync.Mutex
	entries []*audit.Entry
	seq     int64
}

// linkagePayloadJSON is the split_children_filed payload the watcher keys
// on, shared by the in-memory fake and the Postgres-backed end-to-end case.
func linkagePayloadJSON(t *testing.T, family, parentRepo string, parentIssue, contractChild int) json.RawMessage {
	t.Helper()
	payload, err := json.Marshal(splitChildrenFiledPayload{
		ContractClassification: "contract",
		Children:               []splitFilingChild{},
		ContractChildNumber:    contractChild,
		ParentRepo:             parentRepo,
		ParentIssue:            parentIssue,
		ParentForge:            family,
	})
	if err != nil {
		t.Fatalf("marshal linkage payload: %v", err)
	}
	return payload
}

func (a *devForgeAudit) seedLinkage(t *testing.T, family, parentRepo string, parentIssue, contractChild int) {
	t.Helper()
	payload := linkagePayloadJSON(t, family, parentRepo, parentIssue, contractChild)
	a.mu.Lock()
	defer a.mu.Unlock()
	a.seq++
	a.entries = append(a.entries, &audit.Entry{
		ID:        uuid.New(),
		Sequence:  a.seq,
		Timestamp: time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC),
		Category:  splitChildrenFiledCategory,
		Payload:   payload,
	})
}

func (a *devForgeAudit) ListAll(_ context.Context, p audit.ListAllParams) ([]*audit.Entry, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	var out []*audit.Entry
	for _, e := range a.entries {
		if p.Category != nil && e.Category != *p.Category {
			continue
		}
		if p.AccountID != "" && e.AccountID != nil && e.AccountID.String() != p.AccountID {
			continue
		}
		out = append(out, e)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if !out[i].Timestamp.Equal(out[j].Timestamp) {
			return out[i].Timestamp.After(out[j].Timestamp)
		}
		return out[i].Sequence > out[j].Sequence
	})
	return out, nil
}

func (a *devForgeAudit) AppendGlobalChained(_ context.Context, p audit.GlobalChainAppendParams) (*audit.Entry, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.seq++
	e := &audit.Entry{
		ID:           uuid.New(),
		Sequence:     a.seq,
		Timestamp:    p.Timestamp,
		Category:     p.Category,
		ActorKind:    p.ActorKind,
		ActorSubject: p.ActorSubject,
		Payload:      p.Payload,
		AccountID:    p.AccountID,
		EntryHash:    fmt.Sprintf("h%d", a.seq),
	}
	a.entries = append(a.entries, e)
	return e, nil
}

// --- harness ---------------------------------------------------------------

const (
	devForgeRepo      = "stub/parent-close"
	devForgeParent    = 100
	devForgeContract  = 103
	devForgeProjectID = 7
	devForgeInstallID = 4242
)

// newDevForgeServer builds a Server through New(cfg) with the stub as the
// ONLY forge on both families, so a delivery has nowhere to go but the
// stub. The sibling board-sync reconciler on the same issues.closed event
// has its conventions loader stubbed to a hard error so it exits at once
// (the split_parent_close_test.go pattern).
func newDevForgeServer(t *testing.T, st *stub.Forge, au audit.Repository, opts ...func(*Config)) *Server {
	t.Helper()
	prev := conventionsLoader
	conventionsLoader = func(context.Context, string) (workmgmt.Conventions, error) {
		return workmgmt.Conventions{}, errors.New("conventions unavailable in this test")
	}
	t.Cleanup(func() { conventionsLoader = prev })

	cfg := Config{
		Addr:                "127.0.0.1:0",
		DevStubForge:        st,
		GitHubWebhookSecret: []byte(stub.GitHubWebhookSecret),
		GitLabWebhookSecret: []byte(stub.GitLabWebhookToken),
		WebhookDeliveries:   webhook.NewMemoryStore(0),
	}
	if au != nil {
		cfg.AuditRepo = au
	}
	if st != nil {
		cfg.GitHub = &githubclient.Client{
			BaseURL: stub.GitHubBaseURL,
			Tokens:  stub.StaticTokens{Value: stub.InstallationToken},
			HTTP:    st.HTTPClient(),
		}
		glForge := forgegitlab.New(stub.GitLabBaseURL,
			forgegitlab.NewStaticCredentialProvider(stub.InstallationToken),
			forgegitlab.WithHTTPClient(st.HTTPClient()))
		cfg.ForgeResolver = func(id string) (forge.Forge, error) {
			if id == webhook.ForgeGitLab {
				return glForge, nil
			}
			return nil, fmt.Errorf("no forge registered for %q in this test", id)
		}
	}
	for _, o := range opts {
		o(&cfg)
	}
	return New(cfg)
}

func devForgeJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return b
}

// seedIssue drives POST /v0/dev/forge/issues and asserts the 201.
func seedIssue(t *testing.T, s *Server, body map[string]any) stub.Issue {
	t.Helper()
	w := devRequest(t, s, http.MethodPost, "/v0/dev/forge/issues", devForgeJSON(t, body), devLoopbackPeer, nil)
	if w.Code != http.StatusCreated {
		t.Fatalf("seed issue: status = %d, want 201\n%s", w.Code, w.Body.String())
	}
	var is stub.Issue
	if err := json.Unmarshal(w.Body.Bytes(), &is); err != nil {
		t.Fatalf("decode seeded issue: %v", err)
	}
	return is
}

// readIssue drives GET /v0/dev/forge/issues and asserts the 200.
func readIssue(t *testing.T, s *Server, query string) stub.Issue {
	t.Helper()
	w := devRequest(t, s, http.MethodGet, "/v0/dev/forge/issues?"+query, nil, devLoopbackPeer, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("read issue %q: status = %d, want 200\n%s", query, w.Code, w.Body.String())
	}
	var is stub.Issue
	if err := json.Unmarshal(w.Body.Bytes(), &is); err != nil {
		t.Fatalf("decode issue: %v", err)
	}
	return is
}

// deliver drives POST /v0/dev/forge/deliveries and asserts the control
// route's own 200, returning the receiver verdict it reports.
func deliver(t *testing.T, s *Server, body map[string]any) devForgeDeliveryResponse {
	t.Helper()
	w := devRequest(t, s, http.MethodPost, "/v0/dev/forge/deliveries", devForgeJSON(t, body), devLoopbackPeer, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("deliver: status = %d, want 200\n%s", w.Code, w.Body.String())
	}
	var out devForgeDeliveryResponse
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode delivery response: %v", err)
	}
	return out
}

func githubIssuesClosedPayload(repo string, number int) map[string]any {
	return map[string]any{
		"action":       "closed",
		"repository":   map[string]any{"full_name": repo},
		"sender":       map[string]any{"login": "someone"},
		"installation": map[string]any{"id": devForgeInstallID},
		"issue":        map[string]any{"number": number, "state_reason": "completed"},
	}
}

func gitlabIssueClosePayload(projectID int, path string, iid int) map[string]any {
	return map[string]any{
		"object_kind":       "issue",
		"event_type":        "issue",
		"user":              map[string]any{"username": "someone"},
		"project":           map[string]any{"id": projectID, "path_with_namespace": path},
		"object_attributes": map[string]any{"iid": iid, "action": "close", "state": "closed"},
	}
}

// listAuditAnonymously reads GET /v0/audit?category=<cat> with NO
// Authorization header from a loopback peer — byte-for-byte the query the
// acceptance criteria issue — and returns the decoded items.
func listAuditAnonymously(t *testing.T, s *Server, category string) []auditEntryResponse {
	t.Helper()
	w := devRequest(t, s, http.MethodGet, "/v0/audit?category="+category, nil, devLoopbackPeer, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("GET /v0/audit?category=%s (no Authorization): status = %d, want 200\n%s", category, w.Code, w.Body.String())
	}
	var page struct {
		Items []auditEntryResponse `json:"items"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &page); err != nil {
		t.Fatalf("decode audit page: %v", err)
	}
	return page.Items
}

// assertParentClosedObservations reads the split_parent_closed rows back
// through GET /v0/audit anonymously and asserts each expected outcome, in
// write order (the endpoint lists newest first, so the order is reversed).
func assertParentClosedObservations(t *testing.T, s *Server, parentRepo string, wantOutcomes ...string) {
	t.Helper()
	items := listAuditAnonymously(t, s, splitParentClosedCategory)
	if len(items) != len(wantOutcomes) {
		t.Fatalf("GET /v0/audit?category=%s returned %d items, want %d: %s",
			splitParentClosedCategory, len(items), len(wantOutcomes), devForgeJSON(t, items))
	}
	for i, want := range wantOutcomes {
		item := items[len(items)-1-i]
		if item.Category != splitParentClosedCategory {
			t.Errorf("item %d category = %q, want %q", i, item.Category, splitParentClosedCategory)
		}
		if item.RunID != nil {
			t.Errorf("item %d is a global-chain row and must carry a null run_id, got %v", i, item.RunID)
		}
		var payload map[string]any
		if err := json.Unmarshal(item.Payload, &payload); err != nil {
			t.Fatalf("decode observation %d payload: %v", i, err)
		}
		if payload["outcome"] != want {
			t.Errorf("observation %d outcome = %v, want %q (payload %s)", i, payload["outcome"], want, item.Payload)
		}
		if payload["parent_repo"] != parentRepo {
			t.Errorf("observation %d parent_repo = %v, want %q", i, payload["parent_repo"], parentRepo)
		}
		if got, ok := payload["parent_issue"].(float64); !ok || int(got) != devForgeParent {
			t.Errorf("observation %d parent_issue = %v, want %d", i, payload["parent_issue"], devForgeParent)
		}
	}
}

// assertMarkerComment asserts the thread holds EXACTLY ONE comment, whose
// visible text names the contract child and whose last line is the hidden
// parent-close marker.
func assertMarkerComment(t *testing.T, comments []string, parentRepo string) {
	t.Helper()
	if len(comments) != 1 {
		t.Fatalf("parent thread has %d comments, want exactly 1: %v", len(comments), comments)
	}
	if !strings.Contains(comments[0], "#"+fmt.Sprint(devForgeContract)) {
		t.Errorf("linking comment must name the landed contract child #%d: %q", devForgeContract, comments[0])
	}
	key := splitfiling.ParentCloseCommentKey(parentRepo, devForgeParent, devForgeContract)
	if !splitfiling.ThreadHasComment(comments, key) {
		t.Errorf("linking comment must carry the parent-close idempotency marker: %q", comments[0])
	}
	lines := strings.Split(strings.TrimRight(comments[0], "\n"), "\n")
	last := strings.TrimSpace(lines[len(lines)-1])
	if !splitfiling.ThreadHasComment([]string{last}, key) {
		t.Errorf("the marker must be the comment's LAST line; last line = %q", last)
	}
}

// --- registration + loopback posture ---------------------------------------

var devForgeRoutes = []struct{ method, path string }{
	{http.MethodGet, "/v0/dev/forge"},
	{http.MethodDelete, "/v0/dev/forge"},
	{http.MethodGet, "/v0/dev/forge/issues?forge=github&repo=o/r&number=1"},
	{http.MethodPost, "/v0/dev/forge/issues"},
	{http.MethodPost, "/v0/dev/forge/pulls"},
	{http.MethodPost, "/v0/dev/forge/deliveries"},
}

// TestDevForge_RoutesAbsentWhenUnconfigured: a Config without DevStubForge
// leaves every route unregistered — the router's 404, never a 503.
func TestDevForge_RoutesAbsentWhenUnconfigured(t *testing.T) {
	s := newDevForgeServer(t, nil, nil)
	for _, rt := range devForgeRoutes {
		w := devRequest(t, s, rt.method, rt.path, []byte(`{}`), devLoopbackPeer, nil)
		if w.Code != http.StatusNotFound {
			t.Errorf("%s %s with DevStubForge nil: status = %d, want 404 (surface must be ABSENT)\n%s",
				rt.method, rt.path, w.Code, w.Body.String())
		}
	}
}

// TestDevForge_NonLoopbackRefused: every registered route refuses a
// non-loopback peer 403 dev_surface_loopback_only before touching the stub.
func TestDevForge_NonLoopbackRefused(t *testing.T) {
	st := stub.New()
	s := newDevForgeServer(t, st, nil)
	for _, rt := range devForgeRoutes {
		w := devRequest(t, s, rt.method, rt.path, []byte(`{}`), "192.0.2.1:1", nil)
		if w.Code != http.StatusForbidden {
			t.Errorf("%s %s from 192.0.2.1: status = %d, want 403\n%s", rt.method, rt.path, w.Code, w.Body.String())
			continue
		}
		if code := decodeErrorCode(t, w); code != "dev_surface_loopback_only" {
			t.Errorf("%s %s: error code = %q, want dev_surface_loopback_only", rt.method, rt.path, code)
		}
	}
	if reqs := st.Requests(); len(reqs) != 0 {
		t.Errorf("refused requests must not reach the stub, got %v", reqs)
	}
}

// --- per-handler happy paths and refusals ----------------------------------

func TestDevForge_SeedAndReadIssue(t *testing.T) {
	st := stub.New()
	s := newDevForgeServer(t, st, nil)

	got := seedIssue(t, s, map[string]any{
		"forge": "github", "repo": "o/r", "number": 5, "title": "T", "comments": []string{"first"},
	})
	if got.State != "open" || got.Forge != "github" || got.Repo != "o/r" || got.Number != 5 || len(got.Comments) != 1 {
		t.Errorf("seeded github issue = %+v, want open o/r#5 with one comment", got)
	}
	back := readIssue(t, s, "forge=github&repo=o/r&number=5")
	if back.State != "open" || back.Comments[0] != "first" {
		t.Errorf("read back = %+v", back)
	}

	gl := seedIssue(t, s, map[string]any{
		"forge": "gitlab", "repo": "grp/proj", "project_id": 9, "number": 3, "state": "closed",
	})
	if gl.State != "closed" || gl.ProjectID != 9 {
		t.Errorf("seeded gitlab issue = %+v, want closed project 9", gl)
	}
	if back := readIssue(t, s, "forge=gitlab&project_id=9&number=3"); back.State != "closed" {
		t.Errorf("gitlab read back = %+v", back)
	}
}

func TestDevForge_GetIssue_NotFound(t *testing.T) {
	s := newDevForgeServer(t, stub.New(), nil)
	w := devRequest(t, s, http.MethodGet, "/v0/dev/forge/issues?forge=github&repo=o/r&number=42", nil, devLoopbackPeer, nil)
	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404\n%s", w.Code, w.Body.String())
	}
	if code := decodeErrorCode(t, w); code != "stub_issue_not_found" {
		t.Errorf("error code = %q, want stub_issue_not_found", code)
	}
}

// TestDevForge_Validation: one row per named 400 branch across the seed
// and read routes, each asserting the code AND the offending field.
func TestDevForge_Validation(t *testing.T) {
	s := newDevForgeServer(t, stub.New(), nil)
	cases := []struct {
		name, method, path string
		body               []byte
		wantField          string
	}{
		{"seed unknown forge", http.MethodPost, "/v0/dev/forge/issues", []byte(`{"forge":"bitbucket","repo":"o/r","number":1}`), "forge"},
		{"seed non-positive number", http.MethodPost, "/v0/dev/forge/issues", []byte(`{"forge":"github","repo":"o/r","number":0}`), "number"},
		{"seed gitlab without project_id", http.MethodPost, "/v0/dev/forge/issues", []byte(`{"forge":"gitlab","number":1}`), "project_id"},
		{"seed github without repo", http.MethodPost, "/v0/dev/forge/issues", []byte(`{"forge":"github","number":1}`), "repo"},
		{"seed unknown field", http.MethodPost, "/v0/dev/forge/issues", []byte(`{"forge":"github","repo":"o/r","number":1,"bogus":1}`), ""},
		{"seed malformed json", http.MethodPost, "/v0/dev/forge/issues", []byte(`{`), ""},
		{"pull unknown forge", http.MethodPost, "/v0/dev/forge/pulls", []byte(`{"forge":"x","repo":"o/r","number":1}`), "forge"},
		{"pull non-positive number", http.MethodPost, "/v0/dev/forge/pulls", []byte(`{"forge":"github","repo":"o/r","number":-1}`), "number"},
		{"pull malformed json", http.MethodPost, "/v0/dev/forge/pulls", []byte(`nope`), ""},
		{"get unknown forge", http.MethodGet, "/v0/dev/forge/issues?forge=x&number=1", nil, "forge"},
		{"get non-integer number", http.MethodGet, "/v0/dev/forge/issues?forge=github&repo=o/r&number=abc", nil, "number"},
		{"get non-integer project_id", http.MethodGet, "/v0/dev/forge/issues?forge=gitlab&project_id=abc&number=1", nil, "project_id"},
		{"get gitlab without project_id", http.MethodGet, "/v0/dev/forge/issues?forge=gitlab&number=1", nil, "project_id"},
		{"get missing number", http.MethodGet, "/v0/dev/forge/issues?forge=github&repo=o/r", nil, "number"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := devRequest(t, s, tc.method, tc.path, tc.body, devLoopbackPeer, nil)
			if w.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400\n%s", w.Code, w.Body.String())
			}
			if code := decodeErrorCode(t, w); code != "validation_failed" {
				t.Errorf("error code = %q, want validation_failed", code)
			}
			if tc.wantField != "" {
				var env struct {
					Error struct {
						Details map[string]any `json:"details"`
					} `json:"error"`
				}
				if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
					t.Fatalf("decode: %v", err)
				}
				if env.Error.Details["field"] != tc.wantField {
					t.Errorf("details.field = %v, want %q\n%s", env.Error.Details["field"], tc.wantField, w.Body.String())
				}
			}
		})
	}
}

func TestDevForge_SeedPull(t *testing.T) {
	st := stub.New()
	s := newDevForgeServer(t, st, nil)
	mergedAt := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	body := devForgeJSON(t, map[string]any{
		"forge": "github", "repo": "o/r", "number": 8, "state": "closed", "merged": true,
		"merge_commit_sha": "abc123", "merged_at": mergedAt, "head_sha": "def456",
		"head_ref": "feature", "base_ref": "main",
	})
	w := devRequest(t, s, http.MethodPost, "/v0/dev/forge/pulls", body, devLoopbackPeer, nil)
	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201\n%s", w.Code, w.Body.String())
	}
	pr, ok := st.GetPullRequest("github", "o/r", 0, 8)
	if !ok || !pr.Merged || pr.MergeCommitSHA != "abc123" || pr.HeadSHA != "def456" || pr.MergedAt == nil || !pr.MergedAt.Equal(mergedAt) {
		t.Errorf("stored pull = %+v (ok=%v), want merged abc123/def456 at %s", pr, ok, mergedAt)
	}
	// GitLab merge request: Merged folds into the native "merged" state.
	w = devRequest(t, s, http.MethodPost, "/v0/dev/forge/pulls",
		[]byte(`{"forge":"gitlab","project_id":9,"number":2,"merged":true}`), devLoopbackPeer, nil)
	if w.Code != http.StatusCreated {
		t.Fatalf("gitlab status = %d, want 201\n%s", w.Code, w.Body.String())
	}
	if mr, ok := st.GetPullRequest("gitlab", "", 9, 2); !ok || mr.State != "merged" {
		t.Errorf("stored merge request = %+v (ok=%v), want state merged", mr, ok)
	}
}

func TestDevForge_SnapshotAndReset(t *testing.T) {
	st := stub.New()
	s := newDevForgeServer(t, st, nil)
	seedIssue(t, s, map[string]any{"forge": "github", "repo": "o/r", "number": 1})
	seedIssue(t, s, map[string]any{"forge": "gitlab", "project_id": 2, "number": 1})

	w := devRequest(t, s, http.MethodGet, "/v0/dev/forge", nil, devLoopbackPeer, nil)
	if w.Code != http.StatusOK {
		t.Fatalf("snapshot status = %d, want 200\n%s", w.Code, w.Body.String())
	}
	var snap stub.Snapshot
	if err := json.Unmarshal(w.Body.Bytes(), &snap); err != nil {
		t.Fatalf("decode snapshot: %v", err)
	}
	if len(snap.GitHub.Issues) != 1 || len(snap.GitLab.Issues) != 1 {
		t.Errorf("snapshot = %+v, want one issue per family", snap)
	}

	w = devRequest(t, s, http.MethodDelete, "/v0/dev/forge", nil, devLoopbackPeer, nil)
	if w.Code != http.StatusNoContent {
		t.Fatalf("reset status = %d, want 204\n%s", w.Code, w.Body.String())
	}
	w = devRequest(t, s, http.MethodGet, "/v0/dev/forge", nil, devLoopbackPeer, nil)
	if err := json.Unmarshal(w.Body.Bytes(), &snap); err != nil {
		t.Fatalf("decode snapshot after reset: %v", err)
	}
	if len(snap.GitHub.Issues) != 0 || len(snap.GitLab.Issues) != 0 || len(snap.Requests) != 0 {
		t.Errorf("snapshot after reset = %+v, want empty", snap)
	}
	if _, ok := st.GetIssue("github", "o/r", 0, 1); ok {
		t.Error("reset must drop seeded records")
	}
}

// TestDevForge_Deliver_Validation covers the delivery route's own 400s:
// unknown forge, empty event, and a payload that is not a JSON object.
func TestDevForge_Deliver_Validation(t *testing.T) {
	s := newDevForgeServer(t, stub.New(), nil)
	cases := []struct {
		name      string
		body      string
		wantField string
	}{
		{"unknown forge", `{"forge":"x","event":"issues","payload":{}}`, "forge"},
		{"empty event", `{"forge":"github","event":" ","payload":{}}`, "event"},
		{"array payload", `{"forge":"github","event":"issues","payload":[1]}`, "payload"},
		{"string payload", `{"forge":"github","event":"issues","payload":"x"}`, "payload"},
		{"null payload", `{"forge":"github","event":"issues","payload":null}`, "payload"},
		{"absent payload", `{"forge":"github","event":"issues"}`, "payload"},
		{"malformed json", `{"forge":`, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := devRequest(t, s, http.MethodPost, "/v0/dev/forge/deliveries", []byte(tc.body), devLoopbackPeer, nil)
			if w.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400\n%s", w.Code, w.Body.String())
			}
			if code := decodeErrorCode(t, w); code != "validation_failed" {
				t.Errorf("error code = %q, want validation_failed", code)
			}
			if tc.wantField != "" {
				var env struct {
					Error struct {
						Details map[string]any `json:"details"`
					} `json:"error"`
				}
				_ = json.Unmarshal(w.Body.Bytes(), &env)
				if env.Error.Details["field"] != tc.wantField {
					t.Errorf("details.field = %v, want %q", env.Error.Details["field"], tc.wantField)
				}
			}
		})
	}
}

// TestDevForge_Deliver_WebhookUnconfigured: with a family's receiver secret
// empty the route answers 503 stub_forge_webhook_unconfigured for THAT
// family (one subtest each), and the other family is unaffected.
func TestDevForge_Deliver_WebhookUnconfigured(t *testing.T) {
	for _, tc := range []struct {
		name   string
		clear  func(*Config)
		family string
		event  string
		other  string
	}{
		{"github", func(c *Config) { c.GitHubWebhookSecret = nil }, "github", "issues", "gitlab"},
		{"gitlab", func(c *Config) { c.GitLabWebhookSecret = nil }, "gitlab", "Issue Hook", "github"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newDevForgeServer(t, stub.New(), nil, tc.clear)
			w := devRequest(t, s, http.MethodPost, "/v0/dev/forge/deliveries",
				[]byte(`{"forge":"`+tc.family+`","event":"`+tc.event+`","payload":{}}`), devLoopbackPeer, nil)
			if w.Code != http.StatusServiceUnavailable {
				t.Fatalf("status = %d, want 503\n%s", w.Code, w.Body.String())
			}
			if code := decodeErrorCode(t, w); code != "stub_forge_webhook_unconfigured" {
				t.Errorf("error code = %q, want stub_forge_webhook_unconfigured", code)
			}
			// The OTHER family still dispatches (its receiver decides the verdict).
			w = devRequest(t, s, http.MethodPost, "/v0/dev/forge/deliveries",
				[]byte(`{"forge":"`+tc.other+`","event":"ping","payload":{}}`), devLoopbackPeer, nil)
			if w.Code != http.StatusOK {
				t.Errorf("other family %s: status = %d, want 200\n%s", tc.other, w.Code, w.Body.String())
			}
		})
	}
}

// TestDevForge_Deliver_ReportsReceiverVerdict: the receiver's verdict is
// REPORTED, not mapped — a validly-signed event the dispatcher ignores is
// still accepted (202), a redelivery of the SAME id is acknowledged by the
// dedup store (202, consumers not re-run — the receiver's duplicate
// contract), and a caller-supplied delivery id is echoed back.
func TestDevForge_Deliver_ReportsReceiverVerdict(t *testing.T) {
	s := newDevForgeServer(t, stub.New(), nil)
	first := deliver(t, s, map[string]any{
		"forge": "github", "event": "ping", "delivery_id": "d-explicit",
		"payload": map[string]any{"zen": "keep it logically awesome"},
	})
	if first.DeliveryID != "d-explicit" {
		t.Errorf("delivery_id = %q, want the caller's d-explicit", first.DeliveryID)
	}
	if first.Status != http.StatusAccepted {
		t.Errorf("first delivery status = %d, want 202 (body %s)", first.Status, first.Body)
	}
	if !json.Valid(first.Body) {
		t.Errorf("reported body must be valid JSON: %s", first.Body)
	}
	dup := deliver(t, s, map[string]any{
		"forge": "github", "event": "ping", "delivery_id": "d-explicit",
		"payload": map[string]any{"zen": "keep it logically awesome"},
	})
	if dup.Status != http.StatusAccepted {
		t.Errorf("duplicate delivery status = %d, want the receiver's 202 duplicate acknowledgement (body %s)", dup.Status, dup.Body)
	}
	minted := deliver(t, s, map[string]any{
		"forge": "github", "event": "ping", "payload": map[string]any{},
	})
	if _, err := uuid.Parse(minted.DeliveryID); err != nil {
		t.Errorf("minted delivery_id %q is not a UUID: %v", minted.DeliveryID, err)
	}
}

// --- END-TO-END done-means through the real receivers -----------------------

// TestDevForge_GitHubIssueClosed_ClosesParentThroughRealReceiver drives the
// GitHub half of #2945's walk entirely through the control API: seed the
// open parent, deliver the SIGNED issues.closed for the contract child, read
// the parent back closed/completed with exactly one marker-stamped comment,
// and read the split_parent_closed observation back through GET /v0/audit
// with NO Authorization header (approval condition 1).
func TestDevForge_GitHubIssueClosed_ClosesParentThroughRealReceiver(t *testing.T) {
	st := stub.New()
	au := &devForgeAudit{}
	au.seedLinkage(t, "github", devForgeRepo, devForgeParent, devForgeContract)
	s := newDevForgeServer(t, st, au)

	seedIssue(t, s, map[string]any{"forge": "github", "repo": devForgeRepo, "number": devForgeParent, "state": "open"})

	res := deliver(t, s, map[string]any{
		"forge": "github", "event": "issues",
		"payload": githubIssuesClosedPayload(devForgeRepo, devForgeContract),
	})
	if res.Status != http.StatusAccepted {
		t.Fatalf("receiver status = %d, want 202 (body %s)", res.Status, res.Body)
	}

	parent := readIssue(t, s, fmt.Sprintf("forge=github&repo=%s&number=%d", devForgeRepo, devForgeParent))
	if parent.State != "closed" || parent.StateReason != "completed" {
		t.Errorf("parent = %+v, want state=closed state_reason=completed", parent)
	}
	assertMarkerComment(t, parent.Comments, devForgeRepo)

	// Comment-first ordering on the stub's arrival log.
	reqs := st.Requests()
	post := indexOf(reqs, stub.OpGitHubPostComment+" github:"+devForgeRepo+"#100")
	patch := indexOf(reqs, stub.OpGitHubPatchIssue+" github:"+devForgeRepo+"#100")
	if post < 0 || patch < 0 || post > patch {
		t.Errorf("comment must precede close; request log = %v", reqs)
	}

	assertParentClosedObservations(t, s, devForgeRepo, splitParentOutcomeClosed)
}

// TestDevForge_GitLabIssueClosed_ClosesParentThroughRealReceiver is the
// GitLab half (#3303's walk): a tokened Issue Hook close for the contract
// child closes the parent through state_event (native state "closed"), posts
// exactly one note, and the observation is readable anonymously.
func TestDevForge_GitLabIssueClosed_ClosesParentThroughRealReceiver(t *testing.T) {
	st := stub.New()
	au := &devForgeAudit{}
	au.seedLinkage(t, "gitlab", devForgeRepo, devForgeParent, devForgeContract)
	s := newDevForgeServer(t, st, au)

	seedIssue(t, s, map[string]any{
		"forge": "gitlab", "repo": devForgeRepo, "project_id": devForgeProjectID, "number": devForgeParent,
	})

	res := deliver(t, s, map[string]any{
		"forge": "gitlab", "event": "Issue Hook",
		"payload": gitlabIssueClosePayload(devForgeProjectID, devForgeRepo, devForgeContract),
	})
	if res.Status != http.StatusAccepted {
		t.Fatalf("receiver status = %d, want 202 (body %s)", res.Status, res.Body)
	}

	parent := readIssue(t, s, fmt.Sprintf("forge=gitlab&project_id=%d&number=%d", devForgeProjectID, devForgeParent))
	if parent.State != "closed" {
		t.Errorf("parent native state = %q, want closed (%+v)", parent.State, parent)
	}
	assertMarkerComment(t, parent.Comments, devForgeRepo)

	reqs := st.Requests()
	key := fmt.Sprintf(" gitlab:%d#%d", devForgeProjectID, devForgeParent)
	post := indexOf(reqs, stub.OpGitLabPostNote+key)
	put := indexOf(reqs, stub.OpGitLabPutIssue+key)
	if post < 0 || put < 0 || post > put {
		t.Errorf("note must precede close; request log = %v", reqs)
	}
	// Cross-forge binding: a GitLab delivery touches the GitHub stub not at all.
	for _, r := range reqs {
		if strings.HasPrefix(r, "github.") {
			t.Errorf("GitLab delivery made a GitHub call: %v", reqs)
			break
		}
	}

	assertParentClosedObservations(t, s, devForgeRepo, splitParentOutcomeClosed)
}

// TestDevForge_Redelivery_LeavesOneComment pins sequential exactly-once on
// both families: a SECOND delivery with a FRESH delivery id (so the dedup
// store does not short-circuit it) finds the parent already closed, posts
// no second comment, and records an already_closed observation beside the
// first — both readable through GET /v0/audit anonymously.
func TestDevForge_Redelivery_LeavesOneComment(t *testing.T) {
	t.Run("github", func(t *testing.T) {
		st := stub.New()
		au := &devForgeAudit{}
		au.seedLinkage(t, "github", devForgeRepo, devForgeParent, devForgeContract)
		s := newDevForgeServer(t, st, au)
		seedIssue(t, s, map[string]any{"forge": "github", "repo": devForgeRepo, "number": devForgeParent})
		body := map[string]any{"forge": "github", "event": "issues", "payload": githubIssuesClosedPayload(devForgeRepo, devForgeContract)}
		for i := 0; i < 2; i++ {
			if res := deliver(t, s, body); res.Status != http.StatusAccepted {
				t.Fatalf("delivery %d: receiver status = %d, want 202 (body %s)", i, res.Status, res.Body)
			}
		}
		parent := readIssue(t, s, fmt.Sprintf("forge=github&repo=%s&number=%d", devForgeRepo, devForgeParent))
		if parent.State != "closed" {
			t.Errorf("parent state = %q, want closed", parent.State)
		}
		assertMarkerComment(t, parent.Comments, devForgeRepo)
		assertParentClosedObservations(t, s, devForgeRepo, splitParentOutcomeClosed, splitParentOutcomeAlreadyClosed)
	})
	t.Run("gitlab", func(t *testing.T) {
		st := stub.New()
		au := &devForgeAudit{}
		au.seedLinkage(t, "gitlab", devForgeRepo, devForgeParent, devForgeContract)
		s := newDevForgeServer(t, st, au)
		seedIssue(t, s, map[string]any{"forge": "gitlab", "repo": devForgeRepo, "project_id": devForgeProjectID, "number": devForgeParent})
		body := map[string]any{"forge": "gitlab", "event": "Issue Hook", "payload": gitlabIssueClosePayload(devForgeProjectID, devForgeRepo, devForgeContract)}
		for i := 0; i < 2; i++ {
			if res := deliver(t, s, body); res.Status != http.StatusAccepted {
				t.Fatalf("delivery %d: receiver status = %d, want 202 (body %s)", i, res.Status, res.Body)
			}
		}
		parent := readIssue(t, s, fmt.Sprintf("forge=gitlab&project_id=%d&number=%d", devForgeProjectID, devForgeParent))
		if parent.State != "closed" {
			t.Errorf("parent state = %q, want closed", parent.State)
		}
		assertMarkerComment(t, parent.Comments, devForgeRepo)
		assertParentClosedObservations(t, s, devForgeRepo, splitParentOutcomeClosed, splitParentOutcomeAlreadyClosed)
	})
}

// TestDevForge_UnsignedDeliveryIsRefusedByReceiver is the control the
// signing counterfactual anchors on: a delivery posted DIRECTLY to
// /webhooks/github with the same body but no signature is refused 401, so
// the control route's 202 above can only come from a correctly signed body.
func TestDevForge_UnsignedDeliveryIsRefusedByReceiver(t *testing.T) {
	s := newDevForgeServer(t, stub.New(), nil)
	body := devForgeJSON(t, githubIssuesClosedPayload(devForgeRepo, devForgeContract))
	w := postWebhook(t, s, map[string]string{
		"X-GitHub-Event":    "issues",
		"X-GitHub-Delivery": "unsigned-1",
		"Content-Type":      "application/json",
	}, body)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("unsigned delivery status = %d, want 401\n%s", w.Code, w.Body.String())
	}
}

// TestDevForge_Deliver_PreservesLargeIntegers is the boundary-value pin
// for the delivery contract: the payload bytes are signed and dispatched
// VERBATIM. A contract child numbered 2^53+1 (9007199254740993) is exactly
// representable as an int but NOT as a float64, so a route that decoded
// the payload into map[string]any and re-marshaled it would deliver
// 9007199254740992 — an identifier the seeded linkage does not name — and
// the watcher would leave the parent open. Both families are driven
// through the real receivers so the assertion is on the COMMITTED parent
// state, not on the bytes the test believes it sent.
func TestDevForge_Deliver_PreservesLargeIntegers(t *testing.T) {
	const bigChild = 9007199254740993 // 2^53 + 1
	// Sanity: the fixture must straddle float64 precision, or the test
	// cannot discriminate a verbatim body from a re-marshaled one.
	if float64(bigChild) != float64(bigChild-1) {
		t.Fatalf("test fixture %d must not be exactly representable as float64", bigChild)
	}
	t.Run("github", func(t *testing.T) {
		st := stub.New()
		au := &devForgeAudit{}
		au.seedLinkage(t, "github", devForgeRepo, devForgeParent, bigChild)
		s := newDevForgeServer(t, st, au)
		seedIssue(t, s, map[string]any{"forge": "github", "repo": devForgeRepo, "number": devForgeParent})
		res := deliver(t, s, map[string]any{
			"forge": "github", "event": "issues",
			"payload": githubIssuesClosedPayload(devForgeRepo, bigChild),
		})
		if res.Status != http.StatusAccepted {
			t.Fatalf("receiver status = %d, want 202 (body %s)", res.Status, res.Body)
		}
		parent := readIssue(t, s, fmt.Sprintf("forge=github&repo=%s&number=%d", devForgeRepo, devForgeParent))
		if parent.State != "closed" {
			t.Fatalf("parent state = %q, want closed: the delivered issue.number did not match the seeded contract child %d (a float64 round-trip would deliver %d)", parent.State, bigChild, bigChild-1)
		}
		if len(parent.Comments) != 1 || !strings.Contains(parent.Comments[0], "#"+fmt.Sprint(bigChild)) {
			t.Errorf("linking comment must name the contract child #%d verbatim: %v", bigChild, parent.Comments)
		}
	})
	t.Run("gitlab", func(t *testing.T) {
		st := stub.New()
		au := &devForgeAudit{}
		au.seedLinkage(t, "gitlab", devForgeRepo, devForgeParent, bigChild)
		s := newDevForgeServer(t, st, au)
		seedIssue(t, s, map[string]any{"forge": "gitlab", "repo": devForgeRepo, "project_id": devForgeProjectID, "number": devForgeParent})
		res := deliver(t, s, map[string]any{
			"forge": "gitlab", "event": "Issue Hook",
			"payload": gitlabIssueClosePayload(devForgeProjectID, devForgeRepo, bigChild),
		})
		if res.Status != http.StatusAccepted {
			t.Fatalf("receiver status = %d, want 202 (body %s)", res.Status, res.Body)
		}
		parent := readIssue(t, s, fmt.Sprintf("forge=gitlab&project_id=%d&number=%d", devForgeProjectID, devForgeParent))
		if parent.State != "closed" {
			t.Fatalf("parent state = %q, want closed: the delivered object_attributes.iid did not match the seeded contract child %d (a float64 round-trip would deliver %d)", parent.State, bigChild, bigChild-1)
		}
		if len(parent.Comments) != 1 || !strings.Contains(parent.Comments[0], "#"+fmt.Sprint(bigChild)) {
			t.Errorf("linking note must name the contract child #%d verbatim: %v", bigChild, parent.Comments)
		}
	})
}

// devForgeBodyOfLen returns a syntactically valid JSON body for the route
// whose total length is exactly n bytes: the fixed fields plus a "pad"
// string (a seed's issue body / a delivery payload member) sized to fit.
func devForgeBodyOfLen(t *testing.T, prefix, suffix string, n int) []byte {
	t.Helper()
	pad := n - len(prefix) - len(suffix)
	if pad < 0 {
		t.Fatalf("body of %d bytes cannot hold %d bytes of fixed fields", n, len(prefix)+len(suffix))
	}
	b := prefix + strings.Repeat("x", pad) + suffix
	if len(b) != n || !json.Valid([]byte(b)) {
		t.Fatalf("fixture body is %d bytes / valid=%v, want %d bytes of valid JSON", len(b), json.Valid([]byte(b)), n)
	}
	return []byte(b)
}

// TestDevForge_BodyCap pins the devForgeMaxBodyBytes boundary that
// decodeDevForgeBody enforces via http.MaxBytesReader, on BOTH mutating
// routes: a body of exactly the cap is accepted, one byte over is refused
// 400 validation_failed BEFORE any mutation (the oversized seed leaves no
// record — GET answers 404) or dispatch (the oversized delivery's id is
// never marked seen by the receiver's dedup store, so a later Mark of the
// same id is a first write). Removing MaxBytesReader accepts both
// oversized bodies and turns each assertion red.
func TestDevForge_BodyCap(t *testing.T) {
	store := webhook.NewMemoryStore(0)
	st := stub.New()
	s := newDevForgeServer(t, st, nil, func(c *Config) { c.WebhookDeliveries = store })

	t.Run("seed at cap accepted", func(t *testing.T) {
		body := devForgeBodyOfLen(t, `{"forge":"github","repo":"o/r","number":1,"body":"`, `"}`, devForgeMaxBodyBytes)
		w := devRequest(t, s, http.MethodPost, "/v0/dev/forge/issues", body, devLoopbackPeer, nil)
		if w.Code != http.StatusCreated {
			t.Fatalf("seed of exactly %d bytes: status = %d, want 201\n%.200s", devForgeMaxBodyBytes, w.Code, w.Body.String())
		}
	})
	t.Run("seed over cap refused before mutation", func(t *testing.T) {
		body := devForgeBodyOfLen(t, `{"forge":"github","repo":"o/r","number":2,"body":"`, `"}`, devForgeMaxBodyBytes+1)
		w := devRequest(t, s, http.MethodPost, "/v0/dev/forge/issues", body, devLoopbackPeer, nil)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("seed of %d bytes: status = %d, want 400\n%.200s", devForgeMaxBodyBytes+1, w.Code, w.Body.String())
		}
		if code := decodeErrorCode(t, w); code != "validation_failed" {
			t.Errorf("error code = %q, want validation_failed", code)
		}
		if _, ok := st.GetIssue("github", "o/r", 0, 2); ok {
			t.Error("an oversized seed must not reach the stub, but o/r#2 was stored")
		}
		if w := devRequest(t, s, http.MethodGet, "/v0/dev/forge/issues?forge=github&repo=o/r&number=2", nil, devLoopbackPeer, nil); w.Code != http.StatusNotFound {
			t.Errorf("GET after the refused seed: status = %d, want 404", w.Code)
		}
	})
	t.Run("delivery over cap refused before dispatch", func(t *testing.T) {
		body := devForgeBodyOfLen(t, `{"forge":"github","event":"ping","delivery_id":"cap-1","payload":{"pad":"`, `"}}`, devForgeMaxBodyBytes+1)
		w := devRequest(t, s, http.MethodPost, "/v0/dev/forge/deliveries", body, devLoopbackPeer, nil)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("delivery of %d bytes: status = %d, want 400\n%.200s", devForgeMaxBodyBytes+1, w.Code, w.Body.String())
		}
		if code := decodeErrorCode(t, w); code != "validation_failed" {
			t.Errorf("error code = %q, want validation_failed", code)
		}
		if err := store.Mark("cap-1"); err != nil {
			t.Errorf("Mark(cap-1) after the refused delivery = %v, want nil: the receiver must never have seen it", err)
		}
	})
	t.Run("delivery at cap dispatched", func(t *testing.T) {
		body := devForgeBodyOfLen(t, `{"forge":"github","event":"ping","delivery_id":"cap-2","payload":{"pad":"`, `"}}`, devForgeMaxBodyBytes)
		w := devRequest(t, s, http.MethodPost, "/v0/dev/forge/deliveries", body, devLoopbackPeer, nil)
		if w.Code != http.StatusOK {
			t.Fatalf("delivery of exactly %d bytes: status = %d, want 200\n%.200s", devForgeMaxBodyBytes, w.Code, w.Body.String())
		}
		if err := store.Mark("cap-2"); !errors.Is(err, webhook.ErrDeliveryDuplicate) {
			t.Errorf("Mark(cap-2) after the dispatched delivery = %v, want ErrDeliveryDuplicate (the receiver marked it)", err)
		}
	})
}

// TestDevForge_ParentClosedObservation_PostgresAudit_ReadableAnonymously
// closes the observation-path gap in approval condition 1 with REAL
// storage: the audit repository is the Postgres-backed one (pgtest), the
// linkage row is written through the real AppendGlobalChained, the
// watcher's split_parent_closed row lands in the real audit_entries table
// (untenanted, account_id NULL), and it is read back through GET
// /v0/audit?category=split_parent_closed with NO Authorization header —
// the real handler, the real ListAll (ListAuditEntriesAll's
// `$3 IS NULL OR account_id = $3 OR account_id IS NULL` filter, with the
// anonymous caller's empty AccountID mapping to a NULL $3), the real rows.
// Whether a watcher-WRITTEN row is visible to the credential-free
// acceptance agent is therefore established here, not assumed from the
// in-memory fake's documented semantics.
func TestDevForge_ParentClosedObservation_PostgresAudit_ReadableAnonymously(t *testing.T) {
	ctx := context.Background()
	systemKind := audit.ActorSystem
	for _, tc := range []struct {
		family, event string
		seed          map[string]any
		payload       map[string]any
		query         string
	}{
		{
			family: "github", event: "issues",
			seed:    map[string]any{"forge": "github", "repo": devForgeRepo, "number": devForgeParent},
			payload: githubIssuesClosedPayload(devForgeRepo, devForgeContract),
			query:   fmt.Sprintf("forge=github&repo=%s&number=%d", devForgeRepo, devForgeParent),
		},
		{
			family: "gitlab", event: "Issue Hook",
			seed:    map[string]any{"forge": "gitlab", "repo": devForgeRepo, "project_id": devForgeProjectID, "number": devForgeParent},
			payload: gitlabIssueClosePayload(devForgeProjectID, devForgeRepo, devForgeContract),
			query:   fmt.Sprintf("forge=gitlab&project_id=%d&number=%d", devForgeProjectID, devForgeParent),
		},
	} {
		t.Run(tc.family, func(t *testing.T) {
			pool := pgtest.NewPool(t)
			au := audit.NewPostgresRepository(pool)
			if _, err := au.AppendGlobalChained(ctx, audit.GlobalChainAppendParams{
				Timestamp: time.Now().UTC(),
				Category:  splitChildrenFiledCategory,
				ActorKind: &systemKind,
				Payload:   linkagePayloadJSON(t, tc.family, devForgeRepo, devForgeParent, devForgeContract),
			}); err != nil {
				t.Fatalf("seed linkage row: %v", err)
			}
			s := newDevForgeServer(t, stub.New(), au)
			seedIssue(t, s, tc.seed)
			for i := 0; i < 2; i++ {
				if res := deliver(t, s, map[string]any{"forge": tc.family, "event": tc.event, "payload": tc.payload}); res.Status != http.StatusAccepted {
					t.Fatalf("delivery %d: receiver status = %d, want 202 (body %s)", i, res.Status, res.Body)
				}
			}
			if parent := readIssue(t, s, tc.query); parent.State != "closed" {
				t.Fatalf("parent state = %q, want closed", parent.State)
			}
			// The persisted row is untenanted — the precondition the
			// NULL-allow filter keys on — and both observations come back
			// through the anonymous HTTP read, newest first.
			cat := splitParentClosedCategory
			rows, err := au.ListAll(ctx, audit.ListAllParams{Category: &cat})
			if err != nil {
				t.Fatalf("ListAll: %v", err)
			}
			if len(rows) != 2 {
				t.Fatalf("persisted %d split_parent_closed rows, want 2", len(rows))
			}
			for _, e := range rows {
				if e.AccountID != nil || e.RunID != nil {
					t.Errorf("watcher row account_id=%v run_id=%v, want both NULL (untenanted global-chain row)", e.AccountID, e.RunID)
				}
			}
			assertParentClosedObservations(t, s, devForgeRepo, splitParentOutcomeClosed, splitParentOutcomeAlreadyClosed)
		})
	}
}
