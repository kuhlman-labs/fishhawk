package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/forge"
	forgegitlab "github.com/kuhlman-labs/fishhawk/backend/internal/forge/gitlab"
	"github.com/kuhlman-labs/fishhawk/backend/internal/githubclient"
	"github.com/kuhlman-labs/fishhawk/backend/internal/splitfiling"
	"github.com/kuhlman-labs/fishhawk/backend/internal/webhook"
	"github.com/kuhlman-labs/fishhawk/backend/internal/workmgmt"
)

// This file is the END-TO-END seam suite for the E50.6 parent-close watcher
// (#2062), for BOTH forges since E50.17 / #2900. Every behavioral test drives
// the REAL signed-webhook path — postWebhook -> s.Handler() -> handleWebhook
// -> handleContractChildClosed -> forgegithub.Forge over a genuine
// *githubclient.Client -> a STATEFUL httptest GitHub fake; or postGitLab ->
// handleWebhookGitLab -> handleContractChildClosed -> a real *forgegitlab.Forge
// (injected via cfg.ForgeResolver) -> a STATEFUL httptest GitLab fake — and NO
// test calls handleContractChildClosed directly.
//
// That is deliberate. The change spans four layers: the webhook payload decode,
// the audit-payload linkage (whose json tags the producer in split_filing.go
// writes and this consumer reads), the forge client, and the comment render.
// Per-layer unit tests would each pass while the seam between them broke — a
// `parent_repo` tag typo passes a producer assertion AND a hand-seeded consumer
// fixture, and fails only in production (#618).
//
// Assertions read COMMITTED STATE on the fake after the handler returns (was
// the parent patched? how many comments exist? in what ORDER did the calls
// arrive?) rather than an error value, because that is where every control in
// this change has its effect.

// --- stateful GitHub fake -------------------------------------------------

type fakeIssueState struct {
	number      int
	state       string
	stateReason string
}

// splitParentGitHub is an httptest GitHub serving the four endpoints the
// watcher touches, keyed by "owner/name#number" so two repositories can hold
// the SAME issue number simultaneously (the repo-scoping counterfactual needs
// exactly that). It records every request in arrival ORDER — the comment-first
// invariant is an ordering property, so counts alone cannot pin it — and
// injects a per-endpoint failure so each error branch is exercised in isolation
// and then CLEARED to prove the failure was transient, not terminal.
type splitParentGitHub struct {
	mu       sync.Mutex
	issues   map[string]*fakeIssueState
	comments map[string][]string
	calls    []string

	failGetIssue     bool
	failListComments bool
	failPostComment  bool
	failPatchIssue   bool
}

func newSplitParentGitHub() *splitParentGitHub {
	return &splitParentGitHub{
		issues:   map[string]*fakeIssueState{},
		comments: map[string][]string{},
	}
}

func issueKey(repo string, number int) string { return repo + "#" + strconv.Itoa(number) }

func (g *splitParentGitHub) seedIssue(repo string, number int, state string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.issues[issueKey(repo, number)] = &fakeIssueState{number: number, state: state}
}

func (g *splitParentGitHub) seedComment(repo string, number int, body string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	k := issueKey(repo, number)
	g.comments[k] = append(g.comments[k], body)
}

func (g *splitParentGitHub) issue(t *testing.T, repo string, number int) fakeIssueState {
	t.Helper()
	g.mu.Lock()
	defer g.mu.Unlock()
	is, ok := g.issues[issueKey(repo, number)]
	if !ok {
		t.Fatalf("no seeded issue %s", issueKey(repo, number))
	}
	return *is
}

func (g *splitParentGitHub) commentBodies(repo string, number int) []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]string(nil), g.comments[issueKey(repo, number)]...)
}

// callLog returns every request the fake received, in arrival order.
func (g *splitParentGitHub) callLog() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]string(nil), g.calls...)
}

// callsFor returns the requests that touched one issue, in arrival order. The
// zero-forge-call assertions read this: a recorded call fails the assertion
// whether or not the call itself errored.
func (g *splitParentGitHub) callsFor(repo string, number int) []string {
	suffix := " " + issueKey(repo, number)
	var out []string
	for _, c := range g.callLog() {
		if strings.HasSuffix(c, suffix) {
			out = append(out, c)
		}
	}
	return out
}

func (g *splitParentGitHub) countCall(op, repo string, number int) int {
	want := op + " " + issueKey(repo, number)
	n := 0
	for _, c := range g.callLog() {
		if c == want {
			n++
		}
	}
	return n
}

func (g *splitParentGitHub) clearInjections() {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.failGetIssue, g.failListComments, g.failPostComment, g.failPatchIssue = false, false, false, false
}

func (g *splitParentGitHub) handler(t *testing.T) http.Handler {
	t.Helper()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// /repos/{owner}/{name}/issues/{n}[/comments]
		parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
		if len(parts) < 5 || parts[0] != "repos" || parts[3] != "issues" {
			t.Errorf("unexpected fake GitHub path %q", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		repo := parts[1] + "/" + parts[2]
		num, err := strconv.Atoi(parts[4])
		if err != nil {
			t.Errorf("unparseable issue number in %q", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		isComments := len(parts) == 6 && parts[5] == "comments"

		g.mu.Lock()
		defer g.mu.Unlock()
		key := issueKey(repo, num)
		record := func(op string) { g.calls = append(g.calls, op+" "+key) }

		switch {
		case isComments && r.Method == http.MethodGet:
			record("GET comments")
			if g.failListComments {
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			out := []map[string]any{}
			for i, b := range g.comments[key] {
				out = append(out, map[string]any{"id": i + 1, "body": b, "user": map[string]any{"login": "fishhawk"}})
			}
			writeFakeJSON(t, w, http.StatusOK, out)
		case isComments && r.Method == http.MethodPost:
			record("POST comment")
			if g.failPostComment {
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			var body struct {
				Body string `json:"body"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Errorf("decode comment body: %v", err)
			}
			g.comments[key] = append(g.comments[key], body.Body)
			writeFakeJSON(t, w, http.StatusCreated, map[string]any{"id": len(g.comments[key]), "body": body.Body})
		case r.Method == http.MethodGet:
			record("GET issue")
			if g.failGetIssue {
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			is, ok := g.issues[key]
			if !ok {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			writeFakeJSON(t, w, http.StatusOK, map[string]any{
				"number": is.number, "state": is.state, "state_reason": is.stateReason,
			})
		case r.Method == http.MethodPatch:
			record("PATCH issue")
			if g.failPatchIssue {
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			var body struct {
				State       string `json:"state"`
				StateReason string `json:"state_reason"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Errorf("decode patch body: %v", err)
			}
			is, ok := g.issues[key]
			if !ok {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			is.state, is.stateReason = body.State, body.StateReason
			writeFakeJSON(t, w, http.StatusOK, map[string]any{
				"number": is.number, "state": is.state, "state_reason": is.stateReason,
			})
		default:
			t.Errorf("unexpected fake GitHub %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	})
}

func writeFakeJSON(t *testing.T, w http.ResponseWriter, code int, v any) {
	t.Helper()
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		t.Errorf("encode fake response: %v", err)
	}
}

// --- stateful GitLab fake -------------------------------------------------

// splitParentGitLab is an httptest GitLab serving the four endpoints the
// watcher touches through the real *forgegitlab.Forge —
// GET/PUT /api/v4/projects/:id/issues/:iid and GET/POST .../notes — keyed by
// "<project-id>#<iid>". Issue state is held in GitLab's NATIVE vocabulary
// ("opened" | "closed") so the adapter's normalization is exercised, and a PUT
// changes state ONLY through `state_event` ("close" -> closed, "reopen" ->
// opened), as the real API does. Like the GitHub fake it records every request
// in arrival ORDER and injects a per-endpoint failure that is then CLEARED.
// pageSize > 0 makes the notes list PAGINATE with an RFC 8288 rel="next" Link
// header, so the adapter's page-to-exhaustion walk is exercised end to end.
type splitParentGitLab struct {
	mu      sync.Mutex
	baseURL string
	issues  map[string]*fakeIssueState
	notes   map[string][]string
	calls   []string

	pageSize int

	failGetIssue  bool
	failListNotes bool
	failPostNote  bool
	failPutIssue  bool
}

func newSplitParentGitLab() *splitParentGitLab {
	return &splitParentGitLab{
		issues: map[string]*fakeIssueState{},
		notes:  map[string][]string{},
	}
}

func gitlabIssueKey(projectID, iid int) string {
	return strconv.Itoa(projectID) + "#" + strconv.Itoa(iid)
}

// seedIssue seeds an issue in GitLab's NATIVE state vocabulary ("opened" or
// "closed").
func (g *splitParentGitLab) seedIssue(projectID, iid int, nativeState string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.issues[gitlabIssueKey(projectID, iid)] = &fakeIssueState{number: iid, state: nativeState}
}

func (g *splitParentGitLab) seedNote(projectID, iid int, body string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	k := gitlabIssueKey(projectID, iid)
	g.notes[k] = append(g.notes[k], body)
}

func (g *splitParentGitLab) issue(t *testing.T, projectID, iid int) fakeIssueState {
	t.Helper()
	g.mu.Lock()
	defer g.mu.Unlock()
	is, ok := g.issues[gitlabIssueKey(projectID, iid)]
	if !ok {
		t.Fatalf("no seeded gitlab issue %s", gitlabIssueKey(projectID, iid))
	}
	return *is
}

func (g *splitParentGitLab) noteBodies(projectID, iid int) []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]string(nil), g.notes[gitlabIssueKey(projectID, iid)]...)
}

func (g *splitParentGitLab) callLog() []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return append([]string(nil), g.calls...)
}

func (g *splitParentGitLab) callsFor(projectID, iid int) []string {
	suffix := " " + gitlabIssueKey(projectID, iid)
	var out []string
	for _, c := range g.callLog() {
		if strings.HasSuffix(c, suffix) {
			out = append(out, c)
		}
	}
	return out
}

func (g *splitParentGitLab) countCall(op string, projectID, iid int) int {
	want := op + " " + gitlabIssueKey(projectID, iid)
	n := 0
	for _, c := range g.callLog() {
		if c == want {
			n++
		}
	}
	return n
}

func (g *splitParentGitLab) clearInjections() {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.failGetIssue, g.failListNotes, g.failPostNote, g.failPutIssue = false, false, false, false
}

func (g *splitParentGitLab) handler(t *testing.T) http.Handler {
	t.Helper()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// /api/v4/projects/{pid}/issues/{iid}[/notes]
		parts := strings.Split(strings.Trim(r.URL.Path, "/"), "/")
		if len(parts) < 6 || parts[0] != "api" || parts[1] != "v4" || parts[2] != "projects" || parts[4] != "issues" {
			t.Errorf("unexpected fake GitLab path %q", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		pid, err1 := strconv.Atoi(parts[3])
		iid, err2 := strconv.Atoi(parts[5])
		if err1 != nil || err2 != nil {
			t.Errorf("unparseable project/issue id in %q", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		isNotes := len(parts) == 7 && parts[6] == "notes"

		g.mu.Lock()
		defer g.mu.Unlock()
		key := gitlabIssueKey(pid, iid)
		record := func(op string) { g.calls = append(g.calls, op+" "+key) }
		issueJSON := func(is *fakeIssueState) map[string]any {
			return map[string]any{
				"iid": is.number, "title": "parent", "description": "", "state": is.state, "labels": []string{},
			}
		}

		switch {
		case isNotes && r.Method == http.MethodGet:
			record("GET notes")
			if g.failListNotes {
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			all := g.notes[key]
			from, to := 0, len(all)
			if g.pageSize > 0 {
				page := 1
				if p := r.URL.Query().Get("page"); p != "" {
					page, _ = strconv.Atoi(p)
				}
				from = (page - 1) * g.pageSize
				if from > len(all) {
					from = len(all)
				}
				to = from + g.pageSize
				if to > len(all) {
					to = len(all)
				}
				if to < len(all) {
					w.Header().Set("Link", fmt.Sprintf(`<%s/api/v4/projects/%d/issues/%d/notes?per_page=%d&page=%d>; rel="next"`,
						g.baseURL, pid, iid, g.pageSize, page+1))
				}
			}
			out := []map[string]any{}
			for i := from; i < to; i++ {
				out = append(out, map[string]any{
					"id": i + 1, "body": all[i], "system": false,
					"created_at": "2026-08-01T12:00:00Z", "author": map[string]any{"username": "fishhawk"},
				})
			}
			writeFakeJSON(t, w, http.StatusOK, out)
		case isNotes && r.Method == http.MethodPost:
			record("POST note")
			if g.failPostNote {
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			var body struct {
				Body string `json:"body"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Errorf("decode note body: %v", err)
			}
			g.notes[key] = append(g.notes[key], body.Body)
			writeFakeJSON(t, w, http.StatusCreated, map[string]any{
				"id": len(g.notes[key]), "body": body.Body, "system": false,
				"created_at": "2026-08-01T12:00:00Z", "author": map[string]any{"username": "fishhawk"},
			})
		case r.Method == http.MethodGet:
			record("GET issue")
			if g.failGetIssue {
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			is, ok := g.issues[key]
			if !ok {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			writeFakeJSON(t, w, http.StatusOK, issueJSON(is))
		case r.Method == http.MethodPut:
			record("PUT issue")
			if g.failPutIssue {
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			var body struct {
				StateEvent string `json:"state_event"`
			}
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Errorf("decode put body: %v", err)
			}
			is, ok := g.issues[key]
			if !ok {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			// The real API moves state ONLY through state_event.
			switch body.StateEvent {
			case "close":
				is.state = "closed"
			case "reopen":
				is.state = "opened"
			default:
				t.Errorf("PUT carried state_event %q, want close|reopen", body.StateEvent)
			}
			writeFakeJSON(t, w, http.StatusOK, issueJSON(is))
		default:
			t.Errorf("unexpected fake GitLab %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	})
}

// --- audit store ----------------------------------------------------------

// splitParentCloseAudit reproduces the production ListAll contract the watcher
// reads against: entries filtered by category and returned ORDER BY ts DESC,
// id DESC (backend/internal/audit/queries.sql), unbounded. Faithful ordering
// matters — the repo-scoping counterfactual seeds the WRONG entry with a
// strictly NEWER timestamp so a linkage read missing the repo term genuinely
// reaches for it first.
type splitParentCloseAudit struct {
	audit.BaseFake
	mu         sync.Mutex
	seeded     []*audit.Entry
	global     []audit.GlobalChainAppendParams
	seq        int64
	listAllErr error
	appendErr  error
}

func newSplitParentCloseAudit() *splitParentCloseAudit { return &splitParentCloseAudit{} }

// seedLinkage appends one split_children_filed entry in the shape a PRE-PARITY
// (#2900) writeSplitChildrenFiledAudit produced: NO parent_forge field. The
// watcher reads an absent value as the github family, so every GitHub case
// relies on this default and seedLinkage keeps producing the legacy shape. A
// GitLab case opts into an explicit family via seedLinkageForge.
func (a *splitParentCloseAudit) seedLinkage(t *testing.T, parentRepo string, parentIssue, contractChild int, ts time.Time) {
	t.Helper()
	a.seedLinkageForge(t, "", parentRepo, parentIssue, contractChild, ts)
}

// seedLinkageForge appends one split_children_filed entry attributed to the
// given forge FAMILY ("github" | "gitlab"; "" reproduces a pre-parity marker
// with the field omitted), in the shape writeSplitChildrenFiledAudit produces.
func (a *splitParentCloseAudit) seedLinkageForge(t *testing.T, forge, parentRepo string, parentIssue, contractChild int, ts time.Time) {
	t.Helper()
	payload, err := json.Marshal(splitChildrenFiledPayload{
		ContractClassification: "delete-only",
		ContractChildNumber:    contractChild,
		DeferralIssue:          splitfiling.DeferralIssue,
		ParentRepo:             parentRepo,
		ParentIssue:            parentIssue,
		ParentForge:            forge,
	})
	if err != nil {
		t.Fatalf("marshal linkage payload: %v", err)
	}
	a.seedRaw(splitChildrenFiledCategory, payload, ts)
}

func (a *splitParentCloseAudit) seedRaw(category string, payload []byte, ts time.Time) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.seq++
	a.seeded = append(a.seeded, &audit.Entry{
		ID:        uuid.New(),
		Sequence:  a.seq,
		Timestamp: ts,
		Category:  category,
		Payload:   payload,
	})
}

func (a *splitParentCloseAudit) ListAll(_ context.Context, p audit.ListAllParams) ([]*audit.Entry, error) {
	if a.listAllErr != nil {
		return nil, a.listAllErr
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	var out []*audit.Entry
	for _, e := range a.seeded {
		if p.Category == nil || e.Category == *p.Category {
			out = append(out, e)
		}
	}
	// ORDER BY ts DESC, id DESC — the production ordering, reproduced.
	sort.SliceStable(out, func(i, j int) bool {
		if !out[i].Timestamp.Equal(out[j].Timestamp) {
			return out[i].Timestamp.After(out[j].Timestamp)
		}
		return out[i].Sequence > out[j].Sequence
	})
	return out, nil
}

func (a *splitParentCloseAudit) AppendGlobalChained(_ context.Context, p audit.GlobalChainAppendParams) (*audit.Entry, error) {
	if a.appendErr != nil {
		return nil, a.appendErr
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.global = append(a.global, p)
	return &audit.Entry{ID: uuid.New(), Category: p.Category, Payload: p.Payload}, nil
}

// observations returns the decoded split_parent_closed entries, in write order.
func (a *splitParentCloseAudit) observations(t *testing.T) []map[string]any {
	t.Helper()
	a.mu.Lock()
	defer a.mu.Unlock()
	var out []map[string]any
	for _, e := range a.global {
		if e.Category != splitParentClosedCategory {
			continue
		}
		var m map[string]any
		if err := json.Unmarshal(e.Payload, &m); err != nil {
			t.Fatalf("decode split_parent_closed payload: %v", err)
		}
		out = append(out, m)
	}
	return out
}

// globalCount reports how many global-chain entries of ANY category were
// written. The "an unrelated close writes ZERO audit entries" assertions read
// this, not a per-category count.
func (a *splitParentCloseAudit) globalCount() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.global)
}

// --- harness --------------------------------------------------------------

type splitParentTokens struct{}

func (splitParentTokens) Token(_ context.Context, _ int64) (string, error) {
	return "test-installation-token", nil
}

type splitParentCloseHarness struct {
	s  *Server
	gh *splitParentGitHub
	gl *splitParentGitLab
	au *splitParentCloseAudit
}

// newSplitParentCloseHarness wires a Server whose ONLY GitHub is the stateful
// GitHub fake, whose ONLY GitLab is a real *forgegitlab.Forge over the stateful
// GitLab fake (injected through cfg.ForgeResolver for the "gitlab" family, every
// other id an error), and whose ONLY audit store is the ordering-faithful one —
// all reachable through the genuine signed/tokened webhook endpoints. BOTH
// forges are always wired so the cross-forge binding test can assert that a
// delivery for one forge makes ZERO calls against the other.
func newSplitParentCloseHarness(t *testing.T, opts ...func(*Config)) *splitParentCloseHarness {
	t.Helper()
	// The sibling #1817 board-sync reconciler runs on this same event. Stub its
	// conventions loader to a hard error so it exits immediately and never
	// reaches a provider — this suite is about the parent-close consumer.
	prev := conventionsLoader
	conventionsLoader = func(context.Context, string) (workmgmt.Conventions, error) {
		return workmgmt.Conventions{}, errors.New("conventions unavailable in this test")
	}
	t.Cleanup(func() { conventionsLoader = prev })

	gh := newSplitParentGitHub()
	srv := httptest.NewServer(gh.handler(t))
	t.Cleanup(srv.Close)

	gl := newSplitParentGitLab()
	glSrv := httptest.NewServer(gl.handler(t))
	gl.baseURL = glSrv.URL
	t.Cleanup(glSrv.Close)
	glForge := forgegitlab.New(glSrv.URL, forgegitlab.NewStaticCredentialProvider("test-gitlab-token"),
		forgegitlab.WithHTTPClient(glSrv.Client()))

	au := newSplitParentCloseAudit()
	cfg := Config{
		Addr:                "127.0.0.1:0",
		GitHubWebhookSecret: []byte(testSecret),
		GitLabWebhookSecret: []byte(gitlabTestToken),
		WebhookDeliveries:   webhook.NewMemoryStore(0),
		AuditRepo:           au,
		GitHub: &githubclient.Client{
			BaseURL: srv.URL,
			Tokens:  splitParentTokens{},
			HTTP:    &http.Client{Timeout: 5 * time.Second},
		},
		ForgeResolver: func(id string) (forge.Forge, error) {
			if id == webhook.ForgeGitLab {
				return glForge, nil
			}
			return nil, fmt.Errorf("no forge registered for %q in this test", id)
		},
	}
	for _, o := range opts {
		o(&cfg)
	}
	return &splitParentCloseHarness{s: New(cfg), gh: gh, gl: gl, au: au}
}

// deliverGitLabIssue posts a tokened GitLab Issue Hook delivery through the
// real /webhooks/gitlab endpoint and asserts the 202 the webhook contract
// promises (this consumer never influences the response).
func (h *splitParentCloseHarness) deliverGitLabIssue(t *testing.T, eventUUID string, body []byte) {
	t.Helper()
	w := postGitLab(t, h.s, gitlabHeaders("Issue Hook", eventUUID), body)
	if w.Code != http.StatusAccepted {
		t.Fatalf("gitlab status = %d, want 202:\n%s", w.Code, w.Body.String())
	}
}

// gitlabIssueBody renders a GitLab Issue Hook payload
// (https://docs.gitlab.com/user/project/integrations/webhook_events/#issue-events):
// object_kind "issue", the project's numeric id + path_with_namespace, and
// object_attributes.{iid,action}. projectID 0 OMITS project.id so
// webhook.ParseGitLabEvent stamps NO CredentialRef — the zero-scope shape. There
// is deliberately no state_reason anywhere: GitLab's issue object has none.
func gitlabIssueBody(projectID int, path string, iid int, action string) []byte {
	pid := ""
	if projectID != 0 {
		pid = fmt.Sprintf(`"id": %d, `, projectID)
	}
	return []byte(fmt.Sprintf(`{
		"object_kind": "issue",
		"event_type": "issue",
		"user": {"username": "someone"},
		"project": {%s"path_with_namespace": %q},
		"object_attributes": {"iid": %d, "action": %q, "state": "closed"}
	}`, pid, path, iid, action))
}

// deliverIssueClosed posts a SIGNED issues.closed delivery through the real
// endpoint and asserts the 202 the webhook contract promises (this consumer
// never influences the response).
func (h *splitParentCloseHarness) deliverIssueClosed(t *testing.T, deliveryID string, body []byte) {
	t.Helper()
	w := postWebhook(t, h.s, map[string]string{
		"X-GitHub-Event":      "issues",
		"X-GitHub-Delivery":   deliveryID,
		"X-Hub-Signature-256": sign(body),
		"Content-Type":        "application/json",
	}, body)
	if w.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202:\n%s", w.Code, w.Body.String())
	}
}

// closedBody renders an issues.closed payload. stateReasonJSON is spliced in
// VERBATIM so the absent-key, explicit-null and string forms are all reachable;
// pass "" to OMIT the key entirely.
func closedBody(repo string, issueNum int, stateReasonJSON string, installationID int64) []byte {
	reason := ""
	if stateReasonJSON != "" {
		reason = fmt.Sprintf(`, "state_reason": %s`, stateReasonJSON)
	}
	inst := ""
	if installationID != 0 {
		inst = fmt.Sprintf(`"installation": {"id": %d},`, installationID)
	}
	return []byte(fmt.Sprintf(`{
		"action": "closed",
		"repository": {"full_name": %q},
		"sender": {"login": "someone"},
		%s
		"issue": {"number": %d%s}
	}`, repo, inst, issueNum, reason))
}

const (
	splitCloseRepo      = "o/r"
	splitCloseParent    = 100
	splitCloseContract  = 103
	splitCloseExpand    = 101
	splitCloseInstallID = int64(4242)
)

func splitCloseTime() time.Time { return time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC) }

// --- T1: happy path -------------------------------------------------------

// TestContractChildClosed_ClosesParentWithLinkingComment is the #2062 done-means
// end to end: the contract child closes, the parent is linked and closed, and
// the COMMENT lands BEFORE the CLOSE (asserted on recorded call order, because
// the close is what stops future deliveries — see split_parent_close.go).
func TestContractChildClosed_ClosesParentWithLinkingComment(t *testing.T) {
	h := newSplitParentCloseHarness(t)
	h.au.seedLinkage(t, splitCloseRepo, splitCloseParent, splitCloseContract, splitCloseTime())
	h.gh.seedIssue(splitCloseRepo, splitCloseParent, "open")

	h.deliverIssueClosed(t, "d-t1", closedBody(splitCloseRepo, splitCloseContract, `"completed"`, splitCloseInstallID))

	got := h.gh.issue(t, splitCloseRepo, splitCloseParent)
	if got.state != "closed" || got.stateReason != "completed" {
		t.Errorf("parent = %+v, want state=closed state_reason=completed", got)
	}
	bodies := h.gh.commentBodies(splitCloseRepo, splitCloseParent)
	if len(bodies) != 1 {
		t.Fatalf("parent thread has %d comments, want exactly 1: %v", len(bodies), bodies)
	}
	if !strings.Contains(bodies[0], "#103") {
		t.Errorf("linking comment must name the landed contract child #103: %q", bodies[0])
	}
	key := splitfiling.ParentCloseCommentKey(splitCloseRepo, splitCloseParent, splitCloseContract)
	if !splitfiling.ThreadHasComment(bodies, key) {
		t.Errorf("linking comment must carry the parent-close idempotency marker: %q", bodies[0])
	}

	// COMMENT-FIRST ORDERING, pinned on arrival order, not counts.
	calls := h.gh.callsFor(splitCloseRepo, splitCloseParent)
	postIdx, patchIdx := indexOf(calls, "POST comment "+issueKey(splitCloseRepo, splitCloseParent)),
		indexOf(calls, "PATCH issue "+issueKey(splitCloseRepo, splitCloseParent))
	if postIdx < 0 || patchIdx < 0 {
		t.Fatalf("expected both a POST comment and a PATCH issue, got %v", calls)
	}
	if postIdx > patchIdx {
		t.Errorf("comment must be posted BEFORE the close; call order was %v", calls)
	}

	obs := h.au.observations(t)
	if len(obs) != 1 {
		t.Fatalf("wrote %d split_parent_closed observations, want 1: %v", len(obs), obs)
	}
	if obs[0]["outcome"] != splitParentOutcomeClosed {
		t.Errorf("outcome = %v, want %q", obs[0]["outcome"], splitParentOutcomeClosed)
	}
	if obs[0]["parent_repo"] != splitCloseRepo {
		t.Errorf("observation parent_repo = %v, want %q", obs[0]["parent_repo"], splitCloseRepo)
	}
}

func indexOf(ss []string, want string) int {
	for i, s := range ss {
		if s == want {
			return i
		}
	}
	return -1
}

// --- T2: done-means negative ---------------------------------------------

// TestNonContractChildClosed_LeavesParentOpen is the #2062 negative done-means:
// a NON-contract child (the expand phase) closing must leave the parent open —
// and must not even reach the forge, because the linkage filter runs first.
func TestNonContractChildClosed_LeavesParentOpen(t *testing.T) {
	h := newSplitParentCloseHarness(t)
	h.au.seedLinkage(t, splitCloseRepo, splitCloseParent, splitCloseContract, splitCloseTime())
	h.gh.seedIssue(splitCloseRepo, splitCloseParent, "open")

	h.deliverIssueClosed(t, "d-t2", closedBody(splitCloseRepo, splitCloseExpand, `"completed"`, splitCloseInstallID))

	if got := h.gh.issue(t, splitCloseRepo, splitCloseParent); got.state != "open" {
		t.Errorf("parent state = %q, want open", got.state)
	}
	if n := len(h.gh.commentBodies(splitCloseRepo, splitCloseParent)); n != 0 {
		t.Errorf("parent thread has %d comments, want 0", n)
	}
	if calls := h.gh.callLog(); len(calls) != 0 {
		t.Errorf("expected ZERO forge calls of any kind, got %v", calls)
	}
	if n := h.au.globalCount(); n != 0 {
		t.Errorf("wrote %d audit entries, want 0", n)
	}
}

// --- C1: an UNRELATED close writes nothing --------------------------------

// TestUnrelatedIssueClosed_NotPlanned_WritesNothing and its zero-installation
// sibling pin the LINKAGE-FIRST ordering (C1). Both gates that could otherwise
// emit an observation — state_reason and installation — run AFTER linkage, so
// an issue with NO linkage at all must produce zero audit entries and zero
// forge calls. Reversing the order writes a false split_parent_closed row about
// a split the issue has nothing to do with.
func TestUnrelatedIssueClosed_NotPlanned_WritesNothing(t *testing.T) {
	h := newSplitParentCloseHarness(t)
	h.au.seedLinkage(t, splitCloseRepo, splitCloseParent, splitCloseContract, splitCloseTime())
	h.gh.seedIssue(splitCloseRepo, splitCloseParent, "open")

	// #777 is in the same repo, closed as not_planned, and is nobody's contract
	// child.
	h.deliverIssueClosed(t, "d-c1a", closedBody(splitCloseRepo, 777, `"not_planned"`, splitCloseInstallID))

	if n := h.au.globalCount(); n != 0 {
		t.Errorf("an unrelated not_planned close wrote %d audit entries, want 0: %v", n, h.au.observations(t))
	}
	if calls := h.gh.callLog(); len(calls) != 0 {
		t.Errorf("expected ZERO forge calls, got %v", calls)
	}
}

func TestUnrelatedIssueClosed_ZeroInstallation_WritesNothing(t *testing.T) {
	h := newSplitParentCloseHarness(t)
	h.au.seedLinkage(t, splitCloseRepo, splitCloseParent, splitCloseContract, splitCloseTime())
	h.gh.seedIssue(splitCloseRepo, splitCloseParent, "open")

	h.deliverIssueClosed(t, "d-c1b", closedBody(splitCloseRepo, 778, `"completed"`, 0))

	if n := h.au.globalCount(); n != 0 {
		t.Errorf("an unrelated zero-installation close wrote %d audit entries, want 0", n)
	}
	if calls := h.gh.callLog(); len(calls) != 0 {
		t.Errorf("expected ZERO forge calls, got %v", calls)
	}
}

// --- T3: already closed ---------------------------------------------------

func TestContractChildClosed_ParentAlreadyClosed_NoCommentNoPatch(t *testing.T) {
	h := newSplitParentCloseHarness(t)
	h.au.seedLinkage(t, splitCloseRepo, splitCloseParent, splitCloseContract, splitCloseTime())
	h.gh.seedIssue(splitCloseRepo, splitCloseParent, "closed")

	h.deliverIssueClosed(t, "d-t3", closedBody(splitCloseRepo, splitCloseContract, `"completed"`, splitCloseInstallID))

	if n := h.gh.countCall("POST comment", splitCloseRepo, splitCloseParent); n != 0 {
		t.Errorf("posted %d comments on an already-closed parent, want 0", n)
	}
	if n := h.gh.countCall("PATCH issue", splitCloseRepo, splitCloseParent); n != 0 {
		t.Errorf("patched an already-closed parent %d times, want 0", n)
	}
	obs := h.au.observations(t)
	if len(obs) != 1 || obs[0]["outcome"] != splitParentOutcomeAlreadyClosed {
		t.Fatalf("observations = %v, want exactly one %q", obs, splitParentOutcomeAlreadyClosed)
	}
}

// --- T4: comment already present -----------------------------------------

func TestContractChildClosed_CommentAlreadyPresent_ClosesWithoutSecondComment(t *testing.T) {
	h := newSplitParentCloseHarness(t)
	h.au.seedLinkage(t, splitCloseRepo, splitCloseParent, splitCloseContract, splitCloseTime())
	h.gh.seedIssue(splitCloseRepo, splitCloseParent, "open")
	key := splitfiling.ParentCloseCommentKey(splitCloseRepo, splitCloseParent, splitCloseContract)
	h.gh.seedComment(splitCloseRepo, splitCloseParent, splitfiling.StampComment("a prior delivery's linking comment", key))

	h.deliverIssueClosed(t, "d-t4", closedBody(splitCloseRepo, splitCloseContract, `"completed"`, splitCloseInstallID))

	if bodies := h.gh.commentBodies(splitCloseRepo, splitCloseParent); len(bodies) != 1 {
		t.Errorf("parent thread has %d comments, want 1 (the marker must suppress a second post): %v", len(bodies), bodies)
	}
	if got := h.gh.issue(t, splitCloseRepo, splitCloseParent); got.state != "closed" {
		t.Errorf("parent state = %q, want closed", got.state)
	}
	obs := h.au.observations(t)
	if len(obs) != 1 || obs[0]["outcome"] != splitParentOutcomeClosed {
		t.Fatalf("observations = %v, want exactly one %q", obs, splitParentOutcomeClosed)
	}
	if obs[0]["commented"] != false {
		t.Errorf("commented = %v, want false (no new comment was posted)", obs[0]["commented"])
	}
}

// --- T5: state_reason exclusion ------------------------------------------

// TestContractChildClosed_NotLandedStateReasons_LeaveParentOpen: a contract
// child closed as not_planned/duplicate did NOT land, so closing the parent
// would falsely assert completion. Defined skip, audited, forge untouched.
func TestContractChildClosed_NotLandedStateReasons_LeaveParentOpen(t *testing.T) {
	for _, reason := range []string{"not_planned", "duplicate"} {
		t.Run(reason, func(t *testing.T) {
			h := newSplitParentCloseHarness(t)
			h.au.seedLinkage(t, splitCloseRepo, splitCloseParent, splitCloseContract, splitCloseTime())
			h.gh.seedIssue(splitCloseRepo, splitCloseParent, "open")

			h.deliverIssueClosed(t, "d-t5-"+reason,
				closedBody(splitCloseRepo, splitCloseContract, `"`+reason+`"`, splitCloseInstallID))

			if calls := h.gh.callLog(); len(calls) != 0 {
				t.Errorf("expected ZERO forge calls for a %s close, got %v", reason, calls)
			}
			if got := h.gh.issue(t, splitCloseRepo, splitCloseParent); got.state != "open" {
				t.Errorf("parent state = %q, want open", got.state)
			}
			obs := h.au.observations(t)
			if len(obs) != 1 || obs[0]["outcome"] != splitParentOutcomeChildNotLanded {
				t.Fatalf("observations = %v, want exactly one %q", obs, splitParentOutcomeChildNotLanded)
			}
			if obs[0]["state_reason"] != reason {
				t.Errorf("observation state_reason = %v, want %q", obs[0]["state_reason"], reason)
			}
		})
	}
}

// --- T6: missing / null / completed state_reason all proceed --------------

// TestContractChildClosed_NullStateReason_ClosesParent covers the three forms
// GitHub actually sends for a plain close — the key ABSENT, an explicit JSON
// null, and "completed" — each of which must close the parent. Defined behavior
// either way, not an accident of decoding.
func TestContractChildClosed_NullStateReason_ClosesParent(t *testing.T) {
	cases := map[string]string{
		"absent_key":    "",
		"explicit_null": "null",
		"completed":     `"completed"`,
	}
	for name, reasonJSON := range cases {
		t.Run(name, func(t *testing.T) {
			h := newSplitParentCloseHarness(t)
			h.au.seedLinkage(t, splitCloseRepo, splitCloseParent, splitCloseContract, splitCloseTime())
			h.gh.seedIssue(splitCloseRepo, splitCloseParent, "open")

			h.deliverIssueClosed(t, "d-t6-"+name, closedBody(splitCloseRepo, splitCloseContract, reasonJSON, splitCloseInstallID))

			if got := h.gh.issue(t, splitCloseRepo, splitCloseParent); got.state != "closed" {
				t.Errorf("parent state = %q, want closed for state_reason form %q", got.state, name)
			}
		})
	}
}

// --- T7: zero installation ------------------------------------------------

// TestContractChildClosed_ZeroInstallation_DefinedSkip is the never-wedges /
// never-nil-deref criterion: an event with no installation object is a DEFINED,
// audited skip — 202, no panic, no forge call. The old run-resolving design
// dereferenced a nil *run.InstallationID here.
func TestContractChildClosed_ZeroInstallation_DefinedSkip(t *testing.T) {
	h := newSplitParentCloseHarness(t)
	h.au.seedLinkage(t, splitCloseRepo, splitCloseParent, splitCloseContract, splitCloseTime())
	h.gh.seedIssue(splitCloseRepo, splitCloseParent, "open")

	h.deliverIssueClosed(t, "d-t7", closedBody(splitCloseRepo, splitCloseContract, `"completed"`, 0))

	if calls := h.gh.callLog(); len(calls) != 0 {
		t.Errorf("expected ZERO forge calls without an installation, got %v", calls)
	}
	obs := h.au.observations(t)
	if len(obs) != 1 || obs[0]["outcome"] != splitParentOutcomeNoInstallation {
		t.Fatalf("observations = %v, want exactly one %q", obs, splitParentOutcomeNoInstallation)
	}
}

// --- T8/T9/T10/T11: forge error branches ---------------------------------

// TestContractChildClosed_GetIssueError_NothingWedged: a failed parent read is
// TRANSIENT. Nothing is posted, nothing is patched, and clearing the injection
// and redelivering closes the parent — the failure never becomes terminal.
func TestContractChildClosed_GetIssueError_NothingWedged(t *testing.T) {
	h := newSplitParentCloseHarness(t)
	h.au.seedLinkage(t, splitCloseRepo, splitCloseParent, splitCloseContract, splitCloseTime())
	h.gh.seedIssue(splitCloseRepo, splitCloseParent, "open")
	h.gh.failGetIssue = true

	h.deliverIssueClosed(t, "d-t8-a", closedBody(splitCloseRepo, splitCloseContract, `"completed"`, splitCloseInstallID))

	if n := h.gh.countCall("POST comment", splitCloseRepo, splitCloseParent); n != 0 {
		t.Errorf("posted %d comments after a failed parent read, want 0", n)
	}
	if n := h.gh.countCall("PATCH issue", splitCloseRepo, splitCloseParent); n != 0 {
		t.Errorf("patched %d times after a failed parent read, want 0", n)
	}
	if got := h.gh.issue(t, splitCloseRepo, splitCloseParent); got.state != "open" {
		t.Errorf("parent state = %q, want open", got.state)
	}

	h.gh.clearInjections()
	h.deliverIssueClosed(t, "d-t8-b", closedBody(splitCloseRepo, splitCloseContract, `"completed"`, splitCloseInstallID))
	if got := h.gh.issue(t, splitCloseRepo, splitCloseParent); got.state != "closed" {
		t.Errorf("after the injection cleared, parent state = %q, want closed", got.state)
	}
}

// TestContractChildClosed_ListCommentsError_DoesNotPostBlind pins the
// deliberate fail-CLOSED posture on the read that IS the idempotency record:
// posting blind would duplicate the comment on every redelivery.
func TestContractChildClosed_ListCommentsError_DoesNotPostBlind(t *testing.T) {
	h := newSplitParentCloseHarness(t)
	h.au.seedLinkage(t, splitCloseRepo, splitCloseParent, splitCloseContract, splitCloseTime())
	h.gh.seedIssue(splitCloseRepo, splitCloseParent, "open")
	h.gh.failListComments = true

	h.deliverIssueClosed(t, "d-t9-a", closedBody(splitCloseRepo, splitCloseContract, `"completed"`, splitCloseInstallID))

	if n := h.gh.countCall("POST comment", splitCloseRepo, splitCloseParent); n != 0 {
		t.Errorf("posted %d comments blind after a failed thread read, want 0", n)
	}
	if got := h.gh.issue(t, splitCloseRepo, splitCloseParent); got.state != "open" {
		t.Errorf("parent state = %q, want open", got.state)
	}

	h.gh.clearInjections()
	h.deliverIssueClosed(t, "d-t9-b", closedBody(splitCloseRepo, splitCloseContract, `"completed"`, splitCloseInstallID))
	if bodies := h.gh.commentBodies(splitCloseRepo, splitCloseParent); len(bodies) != 1 {
		t.Errorf("thread has %d comments after the retry, want exactly 1 ever: %v", len(bodies), bodies)
	}
	if got := h.gh.issue(t, splitCloseRepo, splitCloseParent); got.state != "closed" {
		t.Errorf("after the injection cleared, parent state = %q, want closed", got.state)
	}
}

// TestContractChildClosed_CommentPostFails_ParentStaysOpen asserts the
// comment-first invariant directly: a FAILED comment must never be followed by
// a close, because the close is what stops future deliveries and would make the
// transient comment failure permanent.
func TestContractChildClosed_CommentPostFails_ParentStaysOpen(t *testing.T) {
	h := newSplitParentCloseHarness(t)
	h.au.seedLinkage(t, splitCloseRepo, splitCloseParent, splitCloseContract, splitCloseTime())
	h.gh.seedIssue(splitCloseRepo, splitCloseParent, "open")
	h.gh.failPostComment = true

	h.deliverIssueClosed(t, "d-t10-a", closedBody(splitCloseRepo, splitCloseContract, `"completed"`, splitCloseInstallID))

	if got := h.gh.issue(t, splitCloseRepo, splitCloseParent); got.state != "open" {
		t.Errorf("parent state = %q, want open after a failed comment", got.state)
	}
	if n := h.gh.countCall("PATCH issue", splitCloseRepo, splitCloseParent); n != 0 {
		t.Errorf("issued %d PATCH calls after a FAILED comment, want 0", n)
	}

	h.gh.clearInjections()
	h.deliverIssueClosed(t, "d-t10-b", closedBody(splitCloseRepo, splitCloseContract, `"completed"`, splitCloseInstallID))
	if bodies := h.gh.commentBodies(splitCloseRepo, splitCloseParent); len(bodies) != 1 {
		t.Errorf("thread has %d comments after the retry, want exactly 1: %v", len(bodies), bodies)
	}
	if got := h.gh.issue(t, splitCloseRepo, splitCloseParent); got.state != "closed" {
		t.Errorf("after the injection cleared, parent state = %q, want closed", got.state)
	}
}

// TestContractChildClosed_CloseFails_ConvergesOnRedelivery: the comment landed
// but the close failed. The next delivery finds the marker, skips the post, and
// closes — the comment count stays 1.
func TestContractChildClosed_CloseFails_ConvergesOnRedelivery(t *testing.T) {
	h := newSplitParentCloseHarness(t)
	h.au.seedLinkage(t, splitCloseRepo, splitCloseParent, splitCloseContract, splitCloseTime())
	h.gh.seedIssue(splitCloseRepo, splitCloseParent, "open")
	h.gh.failPatchIssue = true

	h.deliverIssueClosed(t, "d-t11-a", closedBody(splitCloseRepo, splitCloseContract, `"completed"`, splitCloseInstallID))

	if bodies := h.gh.commentBodies(splitCloseRepo, splitCloseParent); len(bodies) != 1 {
		t.Fatalf("thread has %d comments, want 1", len(bodies))
	}
	if got := h.gh.issue(t, splitCloseRepo, splitCloseParent); got.state != "open" {
		t.Errorf("parent state = %q, want open after a failed close", got.state)
	}
	obs := h.au.observations(t)
	if len(obs) != 1 || obs[0]["outcome"] != splitParentOutcomeCloseFailed {
		t.Fatalf("observations = %v, want exactly one %q", obs, splitParentOutcomeCloseFailed)
	}

	h.gh.clearInjections()
	h.deliverIssueClosed(t, "d-t11-b", closedBody(splitCloseRepo, splitCloseContract, `"completed"`, splitCloseInstallID))
	if got := h.gh.issue(t, splitCloseRepo, splitCloseParent); got.state != "closed" {
		t.Errorf("parent state = %q, want closed on redelivery", got.state)
	}
	if bodies := h.gh.commentBodies(splitCloseRepo, splitCloseParent); len(bodies) != 1 {
		t.Errorf("thread has %d comments after redelivery, want 1 (the marker suppressed the second post): %v", len(bodies), bodies)
	}
}

// --- T12: no linkage / legacy payload ------------------------------------

func TestContractChildClosed_NoLinkage_NoOp(t *testing.T) {
	h := newSplitParentCloseHarness(t)
	h.gh.seedIssue(splitCloseRepo, splitCloseParent, "open")

	h.deliverIssueClosed(t, "d-t12a", closedBody(splitCloseRepo, splitCloseContract, `"completed"`, splitCloseInstallID))

	if calls := h.gh.callLog(); len(calls) != 0 {
		t.Errorf("expected ZERO forge calls with no linkage, got %v", calls)
	}
	if n := h.au.globalCount(); n != 0 {
		t.Errorf("wrote %d audit entries with no linkage, want 0", n)
	}
	if got := h.gh.issue(t, splitCloseRepo, splitCloseParent); got.state != "open" {
		t.Errorf("parent state = %q, want open", got.state)
	}
}

// TestContractChildClosed_LegacyPayloadWithoutParentRepo_NoOp: a
// split_children_filed entry written BEFORE #2062 carries neither parent_repo
// nor parent_issue, so it matches nothing. Splits filed before this ships still
// need a manual parent close — the intended fail-quiet direction, asserted
// rather than left to inference.
func TestContractChildClosed_LegacyPayloadWithoutParentRepo_NoOp(t *testing.T) {
	h := newSplitParentCloseHarness(t)
	legacy, err := json.Marshal(map[string]any{
		"contract_classification": "delete-only",
		"contract_child_number":   splitCloseContract,
		"deferral_issue":          splitfiling.DeferralIssue,
	})
	if err != nil {
		t.Fatalf("marshal legacy payload: %v", err)
	}
	h.au.seedRaw(splitChildrenFiledCategory, legacy, splitCloseTime())
	h.gh.seedIssue(splitCloseRepo, splitCloseParent, "open")

	h.deliverIssueClosed(t, "d-t12b", closedBody(splitCloseRepo, splitCloseContract, `"completed"`, splitCloseInstallID))

	if calls := h.gh.callLog(); len(calls) != 0 {
		t.Errorf("expected ZERO forge calls for a legacy entry, got %v", calls)
	}
	if got := h.gh.issue(t, splitCloseRepo, splitCloseParent); got.state != "open" {
		t.Errorf("parent state = %q, want open", got.state)
	}
}

// --- T13: duplicate sequential delivery ----------------------------------

// TestContractChildClosed_DuplicateDeliveryIsIdempotent claims exactly the
// property the design delivers: SEQUENTIAL redelivery is exactly-once. It is
// the FORGE's own state and comments — not any marker of ours — that make the
// second delivery a no-op. The concurrent-interleaving duplicate-comment window
// is a stated, accepted residual and is deliberately NOT claimed here.
func TestContractChildClosed_DuplicateDeliveryIsIdempotent(t *testing.T) {
	h := newSplitParentCloseHarness(t)
	h.au.seedLinkage(t, splitCloseRepo, splitCloseParent, splitCloseContract, splitCloseTime())
	h.gh.seedIssue(splitCloseRepo, splitCloseParent, "open")

	body := closedBody(splitCloseRepo, splitCloseContract, `"completed"`, splitCloseInstallID)
	h.deliverIssueClosed(t, "d-t13-a", body)
	h.deliverIssueClosed(t, "d-t13-b", body) // distinct delivery id: the store dedup must not mask this

	if bodies := h.gh.commentBodies(splitCloseRepo, splitCloseParent); len(bodies) != 1 {
		t.Errorf("thread has %d comments after two deliveries, want exactly 1: %v", len(bodies), bodies)
	}
	if got := h.gh.issue(t, splitCloseRepo, splitCloseParent); got.state != "closed" {
		t.Errorf("parent state = %q, want closed", got.state)
	}
	if n := h.gh.countCall("PATCH issue", splitCloseRepo, splitCloseParent); n != 1 {
		t.Errorf("issued %d PATCH calls across two deliveries, want 1 (the second short-circuits at already_closed)", n)
	}
}

// --- T14: repo scoping ----------------------------------------------------

// TestContractChildClosed_SameNumberDifferentRepo_DoesNotCloseOtherReposParent
// is the repo-scoping counterfactual. Issue numbers are PER-REPO, so two
// repositories can each have a contract child #103.
//
// The WRONG entry (o/r, parent 100) is seeded with a STRICTLY NEWER timestamp
// than the correct one, and ListAll here reproduces the production ORDER BY ts
// DESC. So an implementation missing the `parent_repo == ev.Repo` term does not
// merely "happen to still work": it reaches the o/r entry first and would close
// the WRONG parent — or, given the ambiguity rule, sees two disagreeing
// candidates and closes NEITHER. Both leave this test RED.
func TestContractChildClosed_SameNumberDifferentRepo_DoesNotCloseOtherReposParent(t *testing.T) {
	const otherRepo = "other/repo"
	h := newSplitParentCloseHarness(t)
	// Correct entry for other/repo, OLDER.
	h.au.seedLinkage(t, otherRepo, 900, splitCloseContract, splitCloseTime())
	// Wrong-repo entry, STRICTLY NEWER — it sorts FIRST under ts DESC.
	h.au.seedLinkage(t, splitCloseRepo, splitCloseParent, splitCloseContract, splitCloseTime().Add(time.Hour))

	h.gh.seedIssue(otherRepo, 900, "open")
	h.gh.seedIssue(splitCloseRepo, splitCloseParent, "open")

	h.deliverIssueClosed(t, "d-t14", closedBody(otherRepo, splitCloseContract, `"completed"`, splitCloseInstallID))

	if got := h.gh.issue(t, otherRepo, 900); got.state != "closed" {
		t.Errorf("other/repo parent 900 state = %q, want closed", got.state)
	}
	// The o/r parent must receive NO forge request at all and stay OPEN.
	if calls := h.gh.callsFor(splitCloseRepo, splitCloseParent); len(calls) != 0 {
		t.Errorf("o/r parent 100 received forge requests %v, want none", calls)
	}
	if got := h.gh.issue(t, splitCloseRepo, splitCloseParent); got.state != "open" {
		t.Errorf("o/r parent 100 state = %q, want open", got.state)
	}
}

// --- T15: self-referential linkage ---------------------------------------

// TestContractChildClosed_SelfReferentialLinkage_NoOp: a corrupt entry whose
// parent_issue EQUALS its contract_child_number would make the watcher close
// the very issue that just closed. Defined skip, zero forge calls.
func TestContractChildClosed_SelfReferentialLinkage_NoOp(t *testing.T) {
	h := newSplitParentCloseHarness(t)
	h.au.seedLinkage(t, splitCloseRepo, splitCloseContract, splitCloseContract, splitCloseTime())
	h.gh.seedIssue(splitCloseRepo, splitCloseContract, "open")

	h.deliverIssueClosed(t, "d-t15", closedBody(splitCloseRepo, splitCloseContract, `"completed"`, splitCloseInstallID))

	if calls := h.gh.callLog(); len(calls) != 0 {
		t.Errorf("expected ZERO forge calls for a self-referential entry, got %v", calls)
	}
	if n := h.au.globalCount(); n != 0 {
		t.Errorf("wrote %d audit entries, want 0", n)
	}
}

// --- C4: conflicting linkage ---------------------------------------------

// TestContractChildClosed_ConflictingLinkage_SkipsAndAudits is the C4 case.
// Issue-number uniqueness proves the CHILD is unique; it does NOT prove two
// payloads agree on the PARENT. Two same-repo entries naming the same contract
// child but DIFFERENT parents cannot both be right, and closing the wrong
// parent is unrecoverable and operator-visible — so the watcher SKIPS, touches
// no forge, and audits ambiguous_linkage naming both candidates.
func TestContractChildClosed_ConflictingLinkage_SkipsAndAudits(t *testing.T) {
	h := newSplitParentCloseHarness(t)
	h.au.seedLinkage(t, splitCloseRepo, splitCloseParent, splitCloseContract, splitCloseTime())
	h.au.seedLinkage(t, splitCloseRepo, 200, splitCloseContract, splitCloseTime().Add(time.Hour))
	h.gh.seedIssue(splitCloseRepo, splitCloseParent, "open")
	h.gh.seedIssue(splitCloseRepo, 200, "open")

	h.deliverIssueClosed(t, "d-c4", closedBody(splitCloseRepo, splitCloseContract, `"completed"`, splitCloseInstallID))

	if calls := h.gh.callLog(); len(calls) != 0 {
		t.Errorf("expected ZERO forge calls under conflicting linkage, got %v", calls)
	}
	if got := h.gh.issue(t, splitCloseRepo, splitCloseParent); got.state != "open" {
		t.Errorf("parent 100 state = %q, want open", got.state)
	}
	if got := h.gh.issue(t, splitCloseRepo, 200); got.state != "open" {
		t.Errorf("parent 200 state = %q, want open", got.state)
	}
	obs := h.au.observations(t)
	if len(obs) != 1 || obs[0]["outcome"] != splitParentOutcomeAmbiguousLinkage {
		t.Fatalf("observations = %v, want exactly one %q", obs, splitParentOutcomeAmbiguousLinkage)
	}
	cands, _ := obs[0]["parent_candidates"].([]any)
	if len(cands) != 2 {
		t.Errorf("parent_candidates = %v, want both candidates named", obs[0]["parent_candidates"])
	}
}

// --- C5: malformed payload --------------------------------------------------

// TestContractChildClosed_MalformedIssuePayload_NoOp: the `issue` object is
// present but its `number` is a string, so the consumer's decode fails. 202, no
// panic, no forge call. The webhook event layer decodes only
// action/repository/sender/installation, so this body reaches the consumer.
//
// The seeded linkage carries contract_child_number 0 ON PURPOSE. That entry is
// REAL — writeSplitChildrenFiledAudit records 0 when a proposal has no contract
// phase — and it is what makes the decode guard load-bearing rather than
// cosmetic: without the guard the failed decode leaves closedNumber at 0, which
// MATCHES this entry, and the watcher would close parent #100 off a payload it
// could not read. Pairing the malformed input with a linkage it can actually
// hit is what keeps this a real counterfactual vehicle.
func TestContractChildClosed_MalformedIssuePayload_NoOp(t *testing.T) {
	h := newSplitParentCloseHarness(t)
	h.au.seedLinkage(t, splitCloseRepo, splitCloseParent, 0, splitCloseTime())
	h.gh.seedIssue(splitCloseRepo, splitCloseParent, "open")

	body := []byte(`{
		"action": "closed",
		"repository": {"full_name": "o/r"},
		"sender": {"login": "someone"},
		"installation": {"id": 4242},
		"issue": {"number": "not-a-number"}
	}`)
	h.deliverIssueClosed(t, "d-c5a", body)

	if calls := h.gh.callLog(); len(calls) != 0 {
		t.Errorf("expected ZERO forge calls for a malformed payload, got %v", calls)
	}
	if n := h.au.globalCount(); n != 0 {
		t.Errorf("wrote %d audit entries for a malformed payload, want 0", n)
	}
	if got := h.gh.issue(t, splitCloseRepo, splitCloseParent); got.state != "open" {
		t.Errorf("parent state = %q, want open", got.state)
	}
}

// --- C6: empty repo, and the ListAll error posture -----------------------

// TestContractChildClosed_EmptyRepo_NoOp pins the explicit ev.Repo == "" skip:
// the legacy no-op must hold as a STATED RULE, not as a consequence of an
// empty-vs-empty comparison failing. A legacy entry carries an empty
// parent_repo, so without this guard an empty-repo event MATCHES it — and the
// state_reason gate, which runs BEFORE the repo full-name split, would then
// write a FALSE child_not_landed observation about that legacy split. The
// delivery therefore carries not_planned: that is the state where the missing
// guard has an observable effect, rather than being masked downstream by
// splitRepoFullName refusing an empty full name.
func TestContractChildClosed_EmptyRepo_NoOp(t *testing.T) {
	h := newSplitParentCloseHarness(t)
	// Seed a legacy-shaped entry whose parent_repo is empty but which otherwise
	// names a real parent — exactly what the guard must not match.
	payload, err := json.Marshal(splitChildrenFiledPayload{
		ContractChildNumber: splitCloseContract,
		ParentRepo:          "",
		ParentIssue:         splitCloseParent,
	})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	h.au.seedRaw(splitChildrenFiledCategory, payload, splitCloseTime())
	h.gh.seedIssue(splitCloseRepo, splitCloseParent, "open")

	h.s.handleContractChildClosed(context.Background(), webhook.Event{
		Type:           "issues",
		Action:         "closed",
		Repo:           "",
		InstallationID: splitCloseInstallID,
		RawBody:        closedBody("", splitCloseContract, `"not_planned"`, splitCloseInstallID),
	})

	if calls := h.gh.callLog(); len(calls) != 0 {
		t.Errorf("expected ZERO forge calls for an empty repo, got %v", calls)
	}
	if n := h.au.globalCount(); n != 0 {
		t.Errorf("an empty-repo close wrote %d audit entries, want 0: %v", n, h.au.observations(t))
	}
}

// TestContractChildClosed_ListAllError_ChangesNothing: the linkage read failing
// logs WARN and returns — no forge call, and NO observation (an unreadable
// linkage is not a fact about any split).
func TestContractChildClosed_ListAllError_ChangesNothing(t *testing.T) {
	h := newSplitParentCloseHarness(t)
	h.au.listAllErr = errors.New("injected ListAll failure")
	h.gh.seedIssue(splitCloseRepo, splitCloseParent, "open")

	h.deliverIssueClosed(t, "d-c6b", closedBody(splitCloseRepo, splitCloseContract, `"completed"`, splitCloseInstallID))

	if calls := h.gh.callLog(); len(calls) != 0 {
		t.Errorf("expected ZERO forge calls after a linkage read failure, got %v", calls)
	}
	if n := h.au.globalCount(); n != 0 {
		t.Errorf("wrote %d audit entries after a linkage read failure, want 0", n)
	}
	if got := h.gh.issue(t, splitCloseRepo, splitCloseParent); got.state != "open" {
		t.Errorf("parent state = %q, want open", got.state)
	}
}

// TestContractChildClosed_NilGitHubClient_NoPanic: a server with no GitHub
// client configured is a defined skip after linkage resolves — it is a server
// misconfiguration, not a fact about the split, so no observation is written.
func TestContractChildClosed_NilGitHubClient_NoPanic(t *testing.T) {
	h := newSplitParentCloseHarness(t, func(c *Config) { c.GitHub = nil })
	h.au.seedLinkage(t, splitCloseRepo, splitCloseParent, splitCloseContract, splitCloseTime())
	h.gh.seedIssue(splitCloseRepo, splitCloseParent, "open")

	h.deliverIssueClosed(t, "d-c6c", closedBody(splitCloseRepo, splitCloseContract, `"completed"`, splitCloseInstallID))

	if calls := h.gh.callLog(); len(calls) != 0 {
		t.Errorf("expected ZERO forge calls with no client, got %v", calls)
	}
	if n := h.au.globalCount(); n != 0 {
		t.Errorf("wrote %d audit entries with no client, want 0", n)
	}
}

// TestContractChildClosed_ObservationAppendFailure_DoesNotUnwind: the audit
// entry is a pure OBSERVATION, never a gate. An append failure logs and changes
// nothing — the parent is still linked and closed.
func TestContractChildClosed_ObservationAppendFailure_DoesNotUnwind(t *testing.T) {
	h := newSplitParentCloseHarness(t)
	h.au.seedLinkage(t, splitCloseRepo, splitCloseParent, splitCloseContract, splitCloseTime())
	h.gh.seedIssue(splitCloseRepo, splitCloseParent, "open")
	h.au.appendErr = errors.New("injected append failure")

	h.deliverIssueClosed(t, "d-c6d", closedBody(splitCloseRepo, splitCloseContract, `"completed"`, splitCloseInstallID))

	if got := h.gh.issue(t, splitCloseRepo, splitCloseParent); got.state != "closed" {
		t.Errorf("parent state = %q, want closed despite the audit append failure", got.state)
	}
	if bodies := h.gh.commentBodies(splitCloseRepo, splitCloseParent); len(bodies) != 1 {
		t.Errorf("thread has %d comments, want 1", len(bodies))
	}
}

// --- C3: producer -> consumer, one path ----------------------------------

// TestSplitFiling_ProducerToConsumer_ParentCloseEndToEnd drives the REAL
// producer-to-consumer path in ONE test: fileSplitProposalChildren WRITES the
// split_children_filed linkage marker into a shared audit store, and a signed
// issues.closed delivery for the contract child it actually filed CONSUMES that
// same entry and closes the parent.
//
// This is the one seam per-layer tests cannot cover. A producer assertion plus
// a hand-seeded consumer fixture leaves the field-name/json-tag contract
// untested in BOTH directions: a `parent_repo` tag typo would satisfy the
// producer test (which reads back through the same struct) AND the consumer
// tests (which seed through the same struct), and fail only in production.
// Here the contract child's number comes from what the PROVIDER filed, not from
// the audit payload, so the payload is never the source of its own truth.
func TestSplitFiling_ProducerToConsumer_ParentCloseEndToEnd(t *testing.T) {
	gh := newSplitParentGitHub()
	srv := httptest.NewServer(gh.handler(t))
	t.Cleanup(srv.Close)
	client := &githubclient.Client{
		BaseURL: srv.URL,
		Tokens:  splitParentTokens{},
		HTTP:    &http.Client{Timeout: 5 * time.Second},
	}

	// PRODUCER: file a split proposal's children for real. The harness's run is
	// repo "o/r", parent issue splitParentIssue.
	inst := int64(77)
	h := newSplitFilingHarness(t, splitFilingConfig{
		withSplitProposal: true, withSpec: true,
		reachabilityDerived: 2, // <= cap -> delete-only
		installID:           &inst, github: client,
	})
	gh.seedIssue("o/r", splitParentIssue, "open")
	h.s.fileSplitProposalChildren(context.Background(), h.planStage)

	// The contract child number comes from what the PROVIDER actually filed —
	// deliberately NOT from the audit payload under test.
	contractChild := h.provider.createdFor(t, "contract: delete").Number
	if contractChild == 0 {
		t.Fatal("producer filed no contract child")
	}
	commentsAfterFiling := len(gh.commentBodies("o/r", splitParentIssue))

	// CONSUMER: a second Server sharing the SAME audit store, reached through the
	// genuine signed webhook endpoint.
	consumer := New(Config{
		Addr:                "127.0.0.1:0",
		GitHubWebhookSecret: []byte(testSecret),
		WebhookDeliveries:   webhook.NewMemoryStore(0),
		AuditRepo:           h.au,
		GitHub:              client,
	})
	body := closedBody("o/r", contractChild, `"completed"`, inst)
	w := postWebhook(t, consumer, map[string]string{
		"X-GitHub-Event":      "issues",
		"X-GitHub-Delivery":   "d-c3",
		"X-Hub-Signature-256": sign(body),
		"Content-Type":        "application/json",
	}, body)
	if w.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202:\n%s", w.Code, w.Body.String())
	}

	got := gh.issue(t, "o/r", splitParentIssue)
	if got.state != "closed" || got.stateReason != "completed" {
		t.Errorf("parent #%d = %+v, want state=closed state_reason=completed — the marker the producer wrote was not consumed",
			splitParentIssue, got)
	}
	bodies := gh.commentBodies("o/r", splitParentIssue)
	if len(bodies) != commentsAfterFiling+1 {
		t.Fatalf("parent thread has %d comments, want %d (the filing comments plus ONE linking comment): %v",
			len(bodies), commentsAfterFiling+1, bodies)
	}
	linking := bodies[len(bodies)-1]
	if !strings.Contains(linking, "#"+strconv.Itoa(contractChild)) {
		t.Errorf("linking comment must name the filed contract child #%d: %q", contractChild, linking)
	}
	key := splitfiling.ParentCloseCommentKey("o/r", splitParentIssue, contractChild)
	if !splitfiling.ThreadHasComment([]string{linking}, key) {
		t.Errorf("linking comment must carry the parent-close marker: %q", linking)
	}
}

// =============================================================================
// GitLab parity suite (E50.17 / #2900)
// =============================================================================
//
// Every test below drives the REAL tokened GitLab receiver — postGitLab ->
// s.Handler() -> handleWebhookGitLab -> isIssueClosedDelivery ->
// handleContractChildClosed -> splitParentIssueOpsFor -> a real
// *forgegitlab.Forge -> the stateful GitLab fake — and asserts COMMITTED STATE
// on the fake (native issue state, note count and BODIES, call ORDER), never an
// error value. The GitLab issue object has no state_reason, so every payload
// here is the plain-close shape.

const (
	splitCloseGitLabRepo    = "grp/proj"
	splitCloseGitLabProject = 77
	// splitCloseGitLabNestedRepo is a nested-group path (group/subgroup/project),
	// legal on GitLab and rejected by the shared splitRepoFullName.
	splitCloseGitLabNestedRepo    = "grp/sub/proj"
	splitCloseGitLabNestedProject = 78
)

// gitlabParentCloseBody is the plain contract-child close for the default
// GitLab project.
func gitlabParentCloseBody() []byte {
	return gitlabIssueBody(splitCloseGitLabProject, splitCloseGitLabRepo, splitCloseContract, "close")
}

// assertGitLabCommentThenClose pins the COMMENT-FIRST invariant on the GitLab
// fake's arrival order: the POST note strictly precedes the PUT issue.
func assertGitLabCommentThenClose(t *testing.T, gl *splitParentGitLab, projectID, iid int) {
	t.Helper()
	calls := gl.callsFor(projectID, iid)
	postIdx := indexOf(calls, "POST note "+gitlabIssueKey(projectID, iid))
	putIdx := indexOf(calls, "PUT issue "+gitlabIssueKey(projectID, iid))
	if postIdx < 0 || putIdx < 0 {
		t.Fatalf("expected both a POST note and a PUT issue, got %v", calls)
	}
	if postIdx > putIdx {
		t.Errorf("note must be posted BEFORE the close; gitlab call order was %v", calls)
	}
}

// TestSplitParentClose_GitLab_ClosesParent is the GitLab done-means end to end:
// the contract child closes (object_kind issue / action close), the parent is
// linked and closed through state_event, exactly one marker-stamped note is
// posted, and the note lands BEFORE the PUT (asserted on call ORDER — inverting
// comment-then-close in the watcher reddens the order assertion).
func TestSplitParentClose_GitLab_ClosesParent(t *testing.T) {
	h := newSplitParentCloseHarness(t)
	h.au.seedLinkageForge(t, webhook.ForgeGitLab, splitCloseGitLabRepo, splitCloseParent, splitCloseContract, splitCloseTime())
	h.gl.seedIssue(splitCloseGitLabProject, splitCloseParent, "opened")

	h.deliverGitLabIssue(t, "gl-t1", gitlabParentCloseBody())

	if got := h.gl.issue(t, splitCloseGitLabProject, splitCloseParent); got.state != "closed" {
		t.Errorf("parent native state = %q, want closed", got.state)
	}
	bodies := h.gl.noteBodies(splitCloseGitLabProject, splitCloseParent)
	if len(bodies) != 1 {
		t.Fatalf("parent thread has %d notes, want exactly 1: %v", len(bodies), bodies)
	}
	if !strings.Contains(bodies[0], "#103") {
		t.Errorf("linking note must name the landed contract child #103: %q", bodies[0])
	}
	key := splitfiling.ParentCloseCommentKey(splitCloseGitLabRepo, splitCloseParent, splitCloseContract)
	if !splitfiling.ThreadHasComment(bodies, key) {
		t.Errorf("linking note must carry the parent-close idempotency marker: %q", bodies[0])
	}
	assertGitLabCommentThenClose(t, h.gl, splitCloseGitLabProject, splitCloseParent)

	obs := h.au.observations(t)
	if len(obs) != 1 {
		t.Fatalf("wrote %d split_parent_closed observations, want 1: %v", len(obs), obs)
	}
	if obs[0]["outcome"] != splitParentOutcomeClosed || obs[0]["commented"] != true {
		t.Errorf("observation = %v, want outcome=closed commented=true", obs[0])
	}
	if obs[0]["parent_repo"] != splitCloseGitLabRepo {
		t.Errorf("observation parent_repo = %v, want %q", obs[0]["parent_repo"], splitCloseGitLabRepo)
	}
	// Cross-forge binding, half one: a GitLab delivery touches the GitHub fake
	// NOT AT ALL (the fuller two-way case is TestSplitParentClose_ForgeFamilyBinding).
	if calls := h.gh.callLog(); len(calls) != 0 {
		t.Errorf("GitLab delivery made GitHub calls: %v", calls)
	}
}

// TestSplitParentClose_GitLab_ClosedChildWithNoStateReason_ClosesParent names
// the GitLab state_reason residual as a fact: the payload carries no
// state_reason (GitLab has none), the watcher records "" and PROCEEDS — the same
// rule the GitHub path applies to the missing/null form
// (TestContractChildClosed_NullStateReason_ClosesParent). The GitHub gate itself
// is still live: TestContractChildClosed_NotLandedStateReasons_LeaveParentOpen.
func TestSplitParentClose_GitLab_ClosedChildWithNoStateReason_ClosesParent(t *testing.T) {
	h := newSplitParentCloseHarness(t)
	h.au.seedLinkageForge(t, webhook.ForgeGitLab, splitCloseGitLabRepo, splitCloseParent, splitCloseContract, splitCloseTime())
	h.gl.seedIssue(splitCloseGitLabProject, splitCloseParent, "opened")

	body := gitlabParentCloseBody()
	if strings.Contains(string(body), "state_reason") {
		t.Fatalf("fixture must carry NO state_reason: %s", body)
	}
	h.deliverGitLabIssue(t, "gl-t1b", body)

	if got := h.gl.issue(t, splitCloseGitLabProject, splitCloseParent); got.state != "closed" {
		t.Errorf("parent native state = %q, want closed", got.state)
	}
	obs := h.au.observations(t)
	if len(obs) != 1 || obs[0]["outcome"] != splitParentOutcomeClosed || obs[0]["state_reason"] != "" {
		t.Errorf("observations = %v, want one closed with state_reason \"\"", obs)
	}
}

// TestSplitParentClose_GitLab_RedeliverySkipsComment is THE MARKER COUNTERFACTUAL
// VEHICLE (approval condition 1). It seeds the parent OPEN with the
// marker-stamped note ALREADY PRESENT — the exact state a torn delivery (note
// posted, PUT failed) leaves behind — then delivers. That is the only shape
// that reaches splitfiling.ThreadHasComment PAST the already-closed
// short-circuit: the watcher must find the marker, post NOTHING, and close.
// Deleting the ThreadHasComment check makes the note count 2 (RED). It also
// proves the marker round-tripped through GET .../notes byte-intact.
func TestSplitParentClose_GitLab_RedeliverySkipsComment(t *testing.T) {
	h := newSplitParentCloseHarness(t)
	h.au.seedLinkageForge(t, webhook.ForgeGitLab, splitCloseGitLabRepo, splitCloseParent, splitCloseContract, splitCloseTime())
	h.gl.seedIssue(splitCloseGitLabProject, splitCloseParent, "opened")
	key := splitfiling.ParentCloseCommentKey(splitCloseGitLabRepo, splitCloseParent, splitCloseContract)
	h.gl.seedNote(splitCloseGitLabProject, splitCloseParent,
		splitfiling.StampComment(splitParentCloseCommentBody(splitCloseParent, splitCloseContract), key))

	h.deliverIssueClosedGitLabRedelivery(t, "gl-t2")

	if bodies := h.gl.noteBodies(splitCloseGitLabProject, splitCloseParent); len(bodies) != 1 {
		t.Errorf("thread has %d notes, want exactly 1 (marker must suppress the second post): %v", len(bodies), bodies)
	}
	if n := h.gl.countCall("POST note", splitCloseGitLabProject, splitCloseParent); n != 0 {
		t.Errorf("POST note called %d times, want 0", n)
	}
	if got := h.gl.issue(t, splitCloseGitLabProject, splitCloseParent); got.state != "closed" {
		t.Errorf("parent native state = %q, want closed", got.state)
	}
	obs := h.au.observations(t)
	if len(obs) != 1 || obs[0]["outcome"] != splitParentOutcomeClosed || obs[0]["commented"] != false {
		t.Errorf("observations = %v, want one closed with commented=false", obs)
	}
}

// deliverIssueClosedGitLabRedelivery delivers the default GitLab contract-child
// close under a fresh event UUID (so the delivery store does not dedup it).
func (h *splitParentCloseHarness) deliverIssueClosedGitLabRedelivery(t *testing.T, eventUUID string) {
	t.Helper()
	h.deliverGitLabIssue(t, eventUUID, gitlabParentCloseBody())
}

// TestSplitParentClose_GitLab_MarkerOnSecondPage_SkipsComment puts the marker
// note on PAGE TWO of a paginated notes list (pageSize 1, Link rel="next"). The
// adapter's page-to-exhaustion walk must still find it, so no second note is
// posted. This is the end-to-end consequence of gitlabclient.ListIssueNotes's
// paging loop (owned by slice 1); a single-page read would post a duplicate.
func TestSplitParentClose_GitLab_MarkerOnSecondPage_SkipsComment(t *testing.T) {
	h := newSplitParentCloseHarness(t)
	h.gl.pageSize = 1
	h.au.seedLinkageForge(t, webhook.ForgeGitLab, splitCloseGitLabRepo, splitCloseParent, splitCloseContract, splitCloseTime())
	h.gl.seedIssue(splitCloseGitLabProject, splitCloseParent, "opened")
	key := splitfiling.ParentCloseCommentKey(splitCloseGitLabRepo, splitCloseParent, splitCloseContract)
	h.gl.seedNote(splitCloseGitLabProject, splitCloseParent, "an unrelated earlier note")
	h.gl.seedNote(splitCloseGitLabProject, splitCloseParent,
		splitfiling.StampComment(splitParentCloseCommentBody(splitCloseParent, splitCloseContract), key))

	h.deliverIssueClosedGitLabRedelivery(t, "gl-t2b")

	if n := h.gl.countCall("GET notes", splitCloseGitLabProject, splitCloseParent); n != 2 {
		t.Errorf("GET notes called %d times, want 2 (two pages walked)", n)
	}
	if bodies := h.gl.noteBodies(splitCloseGitLabProject, splitCloseParent); len(bodies) != 2 {
		t.Errorf("thread has %d notes, want the seeded 2 (marker on page 2 must suppress the post): %v", len(bodies), bodies)
	}
	if got := h.gl.issue(t, splitCloseGitLabProject, splitCloseParent); got.state != "closed" {
		t.Errorf("parent native state = %q, want closed", got.state)
	}
}

// TestSplitParentClose_GitLab_RedeliveryAfterClose_NoSecondNote: a second
// delivery AFTER a successful close short-circuits at already_closed — one note,
// still closed, no further writes. This is the sequential exactly-once claim; it
// is NOT the marker counterfactual (the short-circuit fires first), which is why
// TestSplitParentClose_GitLab_RedeliverySkipsComment exists separately.
func TestSplitParentClose_GitLab_RedeliveryAfterClose_NoSecondNote(t *testing.T) {
	h := newSplitParentCloseHarness(t)
	h.au.seedLinkageForge(t, webhook.ForgeGitLab, splitCloseGitLabRepo, splitCloseParent, splitCloseContract, splitCloseTime())
	h.gl.seedIssue(splitCloseGitLabProject, splitCloseParent, "opened")

	h.deliverIssueClosedGitLabRedelivery(t, "gl-t3a")
	h.deliverIssueClosedGitLabRedelivery(t, "gl-t3b")

	if bodies := h.gl.noteBodies(splitCloseGitLabProject, splitCloseParent); len(bodies) != 1 {
		t.Errorf("thread has %d notes after two deliveries, want 1", len(bodies))
	}
	if n := h.gl.countCall("PUT issue", splitCloseGitLabProject, splitCloseParent); n != 1 {
		t.Errorf("PUT issue called %d times, want 1", n)
	}
	obs := h.au.observations(t)
	if len(obs) != 2 || obs[0]["outcome"] != splitParentOutcomeClosed || obs[1]["outcome"] != splitParentOutcomeAlreadyClosed {
		t.Errorf("observations = %v, want [closed, already_closed]", obs)
	}
}

// TestSplitParentClose_GitLab_AlreadyClosedParent: the fake reports the parent
// closed (native "closed", normalized by the adapter), so zero POST, zero PUT,
// and an already_closed observation. Deleting the short-circuit posts a note on
// a closed parent (RED on the note count).
func TestSplitParentClose_GitLab_AlreadyClosedParent(t *testing.T) {
	h := newSplitParentCloseHarness(t)
	h.au.seedLinkageForge(t, webhook.ForgeGitLab, splitCloseGitLabRepo, splitCloseParent, splitCloseContract, splitCloseTime())
	h.gl.seedIssue(splitCloseGitLabProject, splitCloseParent, "closed")

	h.deliverIssueClosedGitLabRedelivery(t, "gl-t4")

	if bodies := h.gl.noteBodies(splitCloseGitLabProject, splitCloseParent); len(bodies) != 0 {
		t.Errorf("posted %d notes on an already-closed parent, want 0: %v", len(bodies), bodies)
	}
	if n := h.gl.countCall("POST note", splitCloseGitLabProject, splitCloseParent); n != 0 {
		t.Errorf("POST note called %d times, want 0", n)
	}
	if n := h.gl.countCall("PUT issue", splitCloseGitLabProject, splitCloseParent); n != 0 {
		t.Errorf("PUT issue called %d times, want 0", n)
	}
	obs := h.au.observations(t)
	if len(obs) != 1 || obs[0]["outcome"] != splitParentOutcomeAlreadyClosed {
		t.Errorf("observations = %v, want one already_closed", obs)
	}
}

// TestSplitParentClose_GitLab_ListNotesFailure_PostsNothing: the notes GET
// fails, so the fail-CLOSED posture holds — nothing posted, parent still opened,
// no observation. Deleting the return after a failed FetchIssueComments posts
// blind (RED on the note count).
func TestSplitParentClose_GitLab_ListNotesFailure_PostsNothing(t *testing.T) {
	h := newSplitParentCloseHarness(t)
	h.au.seedLinkageForge(t, webhook.ForgeGitLab, splitCloseGitLabRepo, splitCloseParent, splitCloseContract, splitCloseTime())
	h.gl.seedIssue(splitCloseGitLabProject, splitCloseParent, "opened")
	h.gl.failListNotes = true

	h.deliverIssueClosedGitLabRedelivery(t, "gl-t5")

	if bodies := h.gl.noteBodies(splitCloseGitLabProject, splitCloseParent); len(bodies) != 0 {
		t.Errorf("posted %d notes after a failed notes list, want 0 (fail-closed): %v", len(bodies), bodies)
	}
	if got := h.gl.issue(t, splitCloseGitLabProject, splitCloseParent); got.state != "opened" {
		t.Errorf("parent native state = %q, want opened", got.state)
	}
	if n := h.gl.countCall("PUT issue", splitCloseGitLabProject, splitCloseParent); n != 0 {
		t.Errorf("PUT issue called %d times, want 0", n)
	}
	if n := h.au.globalCount(); n != 0 {
		t.Errorf("wrote %d audit entries after a failed notes list, want 0", n)
	}

	// Transient, not terminal: clear and redeliver -> converges.
	h.gl.clearInjections()
	h.deliverIssueClosedGitLabRedelivery(t, "gl-t5b")
	if got := h.gl.issue(t, splitCloseGitLabProject, splitCloseParent); got.state != "closed" {
		t.Errorf("after the transient cleared, parent native state = %q, want closed", got.state)
	}
	if bodies := h.gl.noteBodies(splitCloseGitLabProject, splitCloseParent); len(bodies) != 1 {
		t.Errorf("thread has %d notes after convergence, want 1", len(bodies))
	}
}

// TestSplitParentClose_GitLab_PostNoteFailure_DoesNotClose: the note POST fails,
// so the watcher NEVER closes — parent still opened, zero PUT. Deleting the
// return after a failed PostIssueComment commits the parent to closed (RED).
func TestSplitParentClose_GitLab_PostNoteFailure_DoesNotClose(t *testing.T) {
	h := newSplitParentCloseHarness(t)
	h.au.seedLinkageForge(t, webhook.ForgeGitLab, splitCloseGitLabRepo, splitCloseParent, splitCloseContract, splitCloseTime())
	h.gl.seedIssue(splitCloseGitLabProject, splitCloseParent, "opened")
	h.gl.failPostNote = true

	h.deliverIssueClosedGitLabRedelivery(t, "gl-t6")

	if got := h.gl.issue(t, splitCloseGitLabProject, splitCloseParent); got.state != "opened" {
		t.Errorf("parent native state = %q, want opened (never close after a failed comment)", got.state)
	}
	if n := h.gl.countCall("PUT issue", splitCloseGitLabProject, splitCloseParent); n != 0 {
		t.Errorf("PUT issue called %d times after a failed note, want 0", n)
	}
	if n := h.au.globalCount(); n != 0 {
		t.Errorf("wrote %d audit entries after a failed note, want 0", n)
	}
}

// TestSplitParentClose_GitLab_CloseFailure_RecordsCloseFailed: the note lands,
// the PUT fails -> close_failed with commented=true and the parent still opened
// WITH the note. A later redelivery (failure cleared) finds the marker, posts
// nothing, and closes — exactly one note at the end.
func TestSplitParentClose_GitLab_CloseFailure_RecordsCloseFailed(t *testing.T) {
	h := newSplitParentCloseHarness(t)
	h.au.seedLinkageForge(t, webhook.ForgeGitLab, splitCloseGitLabRepo, splitCloseParent, splitCloseContract, splitCloseTime())
	h.gl.seedIssue(splitCloseGitLabProject, splitCloseParent, "opened")
	h.gl.failPutIssue = true

	h.deliverIssueClosedGitLabRedelivery(t, "gl-t7a")

	if got := h.gl.issue(t, splitCloseGitLabProject, splitCloseParent); got.state != "opened" {
		t.Errorf("parent native state = %q, want opened after a failed PUT", got.state)
	}
	if bodies := h.gl.noteBodies(splitCloseGitLabProject, splitCloseParent); len(bodies) != 1 {
		t.Fatalf("thread has %d notes, want 1 (the note landed before the failed close)", len(bodies))
	}
	obs := h.au.observations(t)
	if len(obs) != 1 || obs[0]["outcome"] != splitParentOutcomeCloseFailed || obs[0]["commented"] != true {
		t.Fatalf("observations = %v, want one close_failed with commented=true", obs)
	}

	h.gl.clearInjections()
	h.deliverIssueClosedGitLabRedelivery(t, "gl-t7b")

	if got := h.gl.issue(t, splitCloseGitLabProject, splitCloseParent); got.state != "closed" {
		t.Errorf("parent native state = %q, want closed after redelivery", got.state)
	}
	if bodies := h.gl.noteBodies(splitCloseGitLabProject, splitCloseParent); len(bodies) != 1 {
		t.Errorf("thread has %d notes after convergence, want STILL exactly 1", len(bodies))
	}
	obs = h.au.observations(t)
	if len(obs) != 2 || obs[1]["outcome"] != splitParentOutcomeClosed || obs[1]["commented"] != false {
		t.Errorf("observations = %v, want [close_failed, closed(commented=false)]", obs)
	}
}

// TestSplitParentClose_GitLab_GetIssueFailure_NothingWedged: the issue GET
// fails -> WARN, return, nothing written, nothing wedged; the cleared redelivery
// converges.
func TestSplitParentClose_GitLab_GetIssueFailure_NothingWedged(t *testing.T) {
	h := newSplitParentCloseHarness(t)
	h.au.seedLinkageForge(t, webhook.ForgeGitLab, splitCloseGitLabRepo, splitCloseParent, splitCloseContract, splitCloseTime())
	h.gl.seedIssue(splitCloseGitLabProject, splitCloseParent, "opened")
	h.gl.failGetIssue = true

	h.deliverIssueClosedGitLabRedelivery(t, "gl-t8a")
	if n := h.gl.countCall("POST note", splitCloseGitLabProject, splitCloseParent); n != 0 {
		t.Errorf("POST note called %d times after a failed GET issue, want 0", n)
	}
	if n := h.au.globalCount(); n != 0 {
		t.Errorf("wrote %d audit entries after a failed GET issue, want 0", n)
	}

	h.gl.clearInjections()
	h.deliverIssueClosedGitLabRedelivery(t, "gl-t8b")
	if got := h.gl.issue(t, splitCloseGitLabProject, splitCloseParent); got.state != "closed" {
		t.Errorf("parent native state = %q, want closed after redelivery", got.state)
	}
}

// TestSplitParentClose_GitLab_NoCredentialRef_RecordsNoInstallation is the
// ZERO-SCOPE COUNTERFACTUAL (approval condition 2). The payload omits
// project.id, so webhook.ParseGitLabEvent stamps NO CredentialRef and the
// derived forge.CredentialScope is ZERO. Linkage resolves (path_with_namespace
// is present), so the watcher records no_installation and makes ZERO HTTP calls.
//
// Which assertion the deletion reddens: DELETING THE ZERO-SCOPE GATE reddens
// the OBSERVATION assertion — the no_installation entry DISAPPEARS (the watcher
// falls through to the adapter, whose scope parse refuses the empty ref BEFORE
// any HTTP call, so the delivery ends in a WARN log with no observation at all).
// The zero-HTTP-calls assertion stays GREEN under the deletion for that same
// reason, so it is asserted here as the invariant it is, NOT as the
// counterfactual observable.
func TestSplitParentClose_GitLab_NoCredentialRef_RecordsNoInstallation(t *testing.T) {
	h := newSplitParentCloseHarness(t)
	h.au.seedLinkageForge(t, webhook.ForgeGitLab, splitCloseGitLabRepo, splitCloseParent, splitCloseContract, splitCloseTime())
	h.gl.seedIssue(splitCloseGitLabProject, splitCloseParent, "opened")

	h.deliverGitLabIssue(t, "gl-t9", gitlabIssueBody(0, splitCloseGitLabRepo, splitCloseContract, "close"))

	obs := h.au.observations(t)
	if len(obs) != 1 || obs[0]["outcome"] != splitParentOutcomeNoInstallation {
		t.Errorf("observations = %v, want exactly one no_installation (this is the assertion the gate's deletion reddens)", obs)
	}
	if len(obs) == 1 && obs[0]["parent_issue"] != float64(splitCloseParent) {
		t.Errorf("no_installation observation must name the resolved parent: %v", obs[0])
	}
	if calls := h.gl.callLog(); len(calls) != 0 {
		t.Errorf("expected ZERO GitLab calls with no credential ref, got %v", calls)
	}
	if got := h.gl.issue(t, splitCloseGitLabProject, splitCloseParent); got.state != "opened" {
		t.Errorf("parent native state = %q, want opened", got.state)
	}
}

// TestSplitParentClose_GitLab_UnrelatedIssue_NoCredentialRef_WritesNothing is
// the GitLab re-run of the linkage-before-scope ordering: a zero-scope delivery
// for an issue that is NOT a contract child writes ZERO audit entries. Hoisting
// the scope gate ahead of linkage reddens it with a false no_installation.
func TestSplitParentClose_GitLab_UnrelatedIssue_NoCredentialRef_WritesNothing(t *testing.T) {
	h := newSplitParentCloseHarness(t)
	h.au.seedLinkageForge(t, webhook.ForgeGitLab, splitCloseGitLabRepo, splitCloseParent, splitCloseContract, splitCloseTime())

	h.deliverGitLabIssue(t, "gl-t9b", gitlabIssueBody(0, splitCloseGitLabRepo, 999, "close"))

	if n := h.au.globalCount(); n != 0 {
		t.Errorf("wrote %d audit entries for an unrelated zero-scope close, want 0", n)
	}
	if calls := h.gl.callLog(); len(calls) != 0 {
		t.Errorf("expected ZERO GitLab calls, got %v", calls)
	}
}

// TestSplitParentClose_GitLab_NestedGroupPath: a nested-group project path
// (group/subgroup/project) — which the shared splitRepoFullName REJECTS — must
// still close the parent, and the calls must reach the project addressed by the
// scope's numeric id. Deleting splitParentRepoRef in favour of the shared helper
// reddens it (silent unsplittable skip, parent stays opened).
func TestSplitParentClose_GitLab_NestedGroupPath(t *testing.T) {
	h := newSplitParentCloseHarness(t)
	h.au.seedLinkageForge(t, webhook.ForgeGitLab, splitCloseGitLabNestedRepo, splitCloseParent, splitCloseContract, splitCloseTime())
	h.gl.seedIssue(splitCloseGitLabNestedProject, splitCloseParent, "opened")

	h.deliverGitLabIssue(t, "gl-t10",
		gitlabIssueBody(splitCloseGitLabNestedProject, splitCloseGitLabNestedRepo, splitCloseContract, "close"))

	if got := h.gl.issue(t, splitCloseGitLabNestedProject, splitCloseParent); got.state != "closed" {
		t.Errorf("nested-group parent native state = %q, want closed", got.state)
	}
	if bodies := h.gl.noteBodies(splitCloseGitLabNestedProject, splitCloseParent); len(bodies) != 1 {
		t.Errorf("nested-group parent has %d notes, want 1", len(bodies))
	}
	for _, c := range h.gl.callLog() {
		if !strings.HasSuffix(c, " "+gitlabIssueKey(splitCloseGitLabNestedProject, splitCloseParent)) {
			t.Errorf("call addressed the wrong project: %q (want project %d)", c, splitCloseGitLabNestedProject)
		}
	}
	// The shared helper's refusal is what makes the local split load-bearing.
	if _, _, ok := splitRepoFullName(splitCloseGitLabNestedRepo); ok {
		t.Errorf("splitRepoFullName unexpectedly accepts %q; the local split would be redundant", splitCloseGitLabNestedRepo)
	}
}

// TestSplitParentRepoRef pins the last-slash split: two-segment GitHub names
// are unchanged from splitRepoFullName, nested GitLab paths split on the LAST
// slash, and either half empty is unsplittable.
func TestSplitParentRepoRef(t *testing.T) {
	cases := []struct {
		in          string
		wantOwner   string
		wantName    string
		wantOK      bool
		description string
	}{
		{"o/r", "o", "r", true, "two segments"},
		{"grp/sub/proj", "grp/sub", "proj", true, "nested group splits on the LAST slash"},
		{" o/r ", "o", "r", true, "surrounding whitespace trimmed"},
		{"noslash", "", "", false, "no slash"},
		{"/r", "", "", false, "empty owner"},
		{"o/", "", "", false, "empty name"},
		{"", "", "", false, "empty"},
	}
	for _, tc := range cases {
		got, ok := splitParentRepoRef(tc.in)
		if ok != tc.wantOK || got.Owner != tc.wantOwner || got.Name != tc.wantName {
			t.Errorf("%s: splitParentRepoRef(%q) = (%+v, %v), want (%q/%q, %v)",
				tc.description, tc.in, got, ok, tc.wantOwner, tc.wantName, tc.wantOK)
		}
	}
}

// TestSplitParentForgeFamily pins the two forge-family mappers the #2900 fix-up
// added: splitParentForgeFamilyFromRef derives the marker's family from a run's
// installation_ref, and normalizeSplitParentForge reads a recorded (or absent)
// family back. The pair MUST land a GitLab ref and a GitLab-recorded marker on
// the SAME "gitlab" token and everything else on "github", or the read-side
// filter would mis-bind a linkage.
func TestSplitParentForgeFamily(t *testing.T) {
	refCases := []struct {
		ref  string
		want string
	}{
		{"gitlab:77", webhook.ForgeGitLab},
		{"gitlab:0", webhook.ForgeGitLab},
		{"4242", forgeNameGitHub},      // bare GitHub App installation id
		{"", forgeNameGitHub},          // legacy GitHub run with no ref
		{"gitlab", forgeNameGitHub},    // no ":" — not the gitlab scheme
		{"github:1", forgeNameGitHub},  // unknown scheme falls to github
		{"GITLAB:77", forgeNameGitHub}, // case-sensitive: not the gitlab prefix
	}
	for _, tc := range refCases {
		if got := splitParentForgeFamilyFromRef(tc.ref); got != tc.want {
			t.Errorf("splitParentForgeFamilyFromRef(%q) = %q, want %q", tc.ref, got, tc.want)
		}
	}

	recordedCases := []struct {
		recorded string
		want     string
	}{
		{"", forgeNameGitHub}, // absent field on a pre-parity marker reads as github
		{forgeNameGitHub, forgeNameGitHub},
		{webhook.ForgeGitLab, webhook.ForgeGitLab},
	}
	for _, tc := range recordedCases {
		if got := normalizeSplitParentForge(tc.recorded); got != tc.want {
			t.Errorf("normalizeSplitParentForge(%q) = %q, want %q", tc.recorded, got, tc.want)
		}
	}

	// The round trip the marker relies on: a run's ref stamps a family that reads
	// back as the SAME family a delivery on that forge computes.
	if a, b := splitParentForgeFamilyFromRef("gitlab:77"), normalizeSplitParentForge(webhook.ForgeGitLab); a != b {
		t.Errorf("gitlab round trip: ref->%q vs recorded->%q, want equal", a, b)
	}
}

// TestSplitParentClose_GitLab_AmbiguousLinkage_SkipsAndAudits re-runs the
// conflicting-linkage skip on the GitLab path: two same-project entries naming
// different parents for the same contract child -> ambiguous_linkage, zero
// forge calls.
func TestSplitParentClose_GitLab_AmbiguousLinkage_SkipsAndAudits(t *testing.T) {
	h := newSplitParentCloseHarness(t)
	h.au.seedLinkageForge(t, webhook.ForgeGitLab, splitCloseGitLabRepo, splitCloseParent, splitCloseContract, splitCloseTime())
	h.au.seedLinkageForge(t, webhook.ForgeGitLab, splitCloseGitLabRepo, 200, splitCloseContract, splitCloseTime().Add(time.Minute))
	h.gl.seedIssue(splitCloseGitLabProject, splitCloseParent, "opened")
	h.gl.seedIssue(splitCloseGitLabProject, 200, "opened")

	h.deliverIssueClosedGitLabRedelivery(t, "gl-t11")

	if calls := h.gl.callLog(); len(calls) != 0 {
		t.Errorf("expected ZERO GitLab calls on ambiguous linkage, got %v", calls)
	}
	obs := h.au.observations(t)
	if len(obs) != 1 || obs[0]["outcome"] != splitParentOutcomeAmbiguousLinkage {
		t.Errorf("observations = %v, want one ambiguous_linkage", obs)
	}
}

// TestSplitParentClose_GitLab_NoLinkage_NoOp: a GitLab close with no linkage is
// silent — zero calls, zero audit entries.
func TestSplitParentClose_GitLab_NoLinkage_NoOp(t *testing.T) {
	h := newSplitParentCloseHarness(t)
	h.gl.seedIssue(splitCloseGitLabProject, splitCloseParent, "opened")

	h.deliverIssueClosedGitLabRedelivery(t, "gl-t12")

	if calls := h.gl.callLog(); len(calls) != 0 {
		t.Errorf("expected ZERO GitLab calls with no linkage, got %v", calls)
	}
	if n := h.au.globalCount(); n != 0 {
		t.Errorf("wrote %d audit entries with no linkage, want 0", n)
	}
}

// TestSplitParentClose_GitLab_NonCloseActions_NotRouted drives the REAL GitLab
// receiver with the non-close issue actions (open / update / reopen) and a
// merge_request close, each with linkage seeded and the parent open, and
// asserts the watcher never fires — the end-to-end form of the predicate
// negatives in TestIsIssueClosedDelivery_ForgeVocabulary.
func TestSplitParentClose_GitLab_NonCloseActions_NotRouted(t *testing.T) {
	for _, action := range []string{"open", "update", "reopen"} {
		t.Run("issue/"+action, func(t *testing.T) {
			h := newSplitParentCloseHarness(t)
			h.au.seedLinkageForge(t, webhook.ForgeGitLab, splitCloseGitLabRepo, splitCloseParent, splitCloseContract, splitCloseTime())
			h.gl.seedIssue(splitCloseGitLabProject, splitCloseParent, "opened")

			h.deliverGitLabIssue(t, "gl-t13-"+action,
				gitlabIssueBody(splitCloseGitLabProject, splitCloseGitLabRepo, splitCloseContract, action))

			if calls := h.gl.callLog(); len(calls) != 0 {
				t.Errorf("issue/%s must not reach the watcher; got GitLab calls %v", action, calls)
			}
			if n := h.au.globalCount(); n != 0 {
				t.Errorf("issue/%s wrote %d audit entries, want 0", action, n)
			}
			if got := h.gl.issue(t, splitCloseGitLabProject, splitCloseParent); got.state != "opened" {
				t.Errorf("parent native state = %q, want opened", got.state)
			}
		})
	}
}

// TestSplitParentClose_GitHub_ReopenedNotRouted is the GitHub-side routing
// negative through the real signed receiver: issues.reopened with linkage
// seeded must make ZERO forge calls and write nothing.
func TestSplitParentClose_GitHub_ReopenedNotRouted(t *testing.T) {
	h := newSplitParentCloseHarness(t)
	h.au.seedLinkage(t, splitCloseRepo, splitCloseParent, splitCloseContract, splitCloseTime())
	h.gh.seedIssue(splitCloseRepo, splitCloseParent, "open")

	body := []byte(fmt.Sprintf(`{
		"action": "reopened",
		"repository": {"full_name": %q},
		"sender": {"login": "someone"},
		"installation": {"id": %d},
		"issue": {"number": %d}
	}`, splitCloseRepo, splitCloseInstallID, splitCloseContract))
	h.deliverIssueClosed(t, "gh-reopened", body)

	if calls := h.gh.callLog(); len(calls) != 0 {
		t.Errorf("issues.reopened must not reach the watcher; got GitHub calls %v", calls)
	}
	if n := h.au.globalCount(); n != 0 {
		t.Errorf("issues.reopened wrote %d audit entries, want 0", n)
	}
}

// TestSplitParentClose_ForgeFamilyBinding pins that a split-parent linkage is
// bound to the forge FAMILY it was FILED under (#2900 fix-up), NOT merely to a
// repo path + issue number. Colliding identities are wired simultaneously:
// GitHub repo "o/r" and GitLab project 77 whose path_with_namespace is ALSO
// "o/r", each with an OPEN parent #100 and contract child #103, both fakes
// reachable in-test. A single linkage is filed under ONE family; the delivery on
// that SAME family closes its own parent, and the COLLIDING delivery on the
// OTHER family must find no linkage and leave its parent untouched. Deleting the
// read-side forge filter (normalizeSplitParentForge in resolveSplitParentLinkage)
// lets the wrong-forge delivery match and close the colliding parent — reddening
// the "stays open" assertion on a real committed-state change, not a connection
// error. This subsumes the earlier adapter-isolation shape, which a single
// forge-less linkage "serving both" could not distinguish from linkage
// ownership.
func TestSplitParentClose_ForgeFamilyBinding(t *testing.T) {
	t.Run("github-filed linkage closes only the github parent, never the colliding gitlab one", func(t *testing.T) {
		h := newSplitParentCloseHarness(t)
		h.au.seedLinkageForge(t, forgeNameGitHub, splitCloseRepo, splitCloseParent, splitCloseContract, splitCloseTime())
		h.gh.seedIssue(splitCloseRepo, splitCloseParent, "open")
		h.gl.seedIssue(splitCloseGitLabProject, splitCloseParent, "opened")

		// GitHub delivery: the linkage is github-owned, so the GitHub parent closes.
		h.deliverIssueClosed(t, "bind-gh", closedBody(splitCloseRepo, splitCloseContract, `"completed"`, splitCloseInstallID))
		if got := h.gh.issue(t, splitCloseRepo, splitCloseParent); got.state != "closed" {
			t.Errorf("GitHub parent state = %q, want closed", got.state)
		}

		// Colliding GitLab delivery for path "o/r" #103: a github-owned linkage is
		// NOT this delivery's, so the GitLab parent stays open and no GitLab write
		// happens. This is the assertion the read-side forge filter guards.
		h.deliverGitLabIssue(t, "bind-gl", gitlabIssueBody(splitCloseGitLabProject, splitCloseRepo, splitCloseContract, "close"))
		if got := h.gl.issue(t, splitCloseGitLabProject, splitCloseParent); got.state != "opened" {
			t.Errorf("gitlab parent native state = %q, want opened (a github-filed linkage must not close a gitlab parent)", got.state)
		}
		if calls := h.gl.callLog(); len(calls) != 0 {
			t.Errorf("github-filed linkage drove GitLab calls: %v", calls)
		}
	})

	t.Run("gitlab-filed linkage closes only the gitlab parent, never the colliding github one", func(t *testing.T) {
		h := newSplitParentCloseHarness(t)
		h.au.seedLinkageForge(t, webhook.ForgeGitLab, splitCloseRepo, splitCloseParent, splitCloseContract, splitCloseTime())
		h.gh.seedIssue(splitCloseRepo, splitCloseParent, "open")
		h.gl.seedIssue(splitCloseGitLabProject, splitCloseParent, "opened")

		// GitLab delivery: the linkage is gitlab-owned, so the GitLab parent closes.
		h.deliverGitLabIssue(t, "bind-gl2", gitlabIssueBody(splitCloseGitLabProject, splitCloseRepo, splitCloseContract, "close"))
		if got := h.gl.issue(t, splitCloseGitLabProject, splitCloseParent); got.state != "closed" {
			t.Errorf("gitlab parent native state = %q, want closed", got.state)
		}

		// Colliding GitHub delivery: a gitlab-owned linkage is not this delivery's,
		// so the GitHub parent stays open and no GitHub write happens.
		h.deliverIssueClosed(t, "bind-gh2", closedBody(splitCloseRepo, splitCloseContract, `"completed"`, splitCloseInstallID))
		if got := h.gh.issue(t, splitCloseRepo, splitCloseParent); got.state != "open" {
			t.Errorf("GitHub parent state = %q, want open (a gitlab-filed linkage must not close a github parent)", got.state)
		}
		if calls := h.gh.callLog(); len(calls) != 0 {
			t.Errorf("gitlab-filed linkage drove GitHub calls: %v", calls)
		}
		if bodies := h.gh.commentBodies(splitCloseRepo, splitCloseParent); len(bodies) != 0 {
			t.Errorf("gitlab-filed linkage posted %d GitHub comments, want 0", len(bodies))
		}
	})

	t.Run("github delivery never consults the resolver", func(t *testing.T) {
		// Registry availability must not change a GitHub outcome: a resolver
		// that FAILS for every id leaves the GitHub path untouched.
		h := newSplitParentCloseHarness(t, func(c *Config) {
			c.ForgeResolver = func(id string) (forge.Forge, error) {
				t.Errorf("GitHub delivery consulted ForgeResolver(%q)", id)
				return nil, errors.New("resolver must not be reached")
			}
		})
		h.au.seedLinkage(t, splitCloseRepo, splitCloseParent, splitCloseContract, splitCloseTime())
		h.gh.seedIssue(splitCloseRepo, splitCloseParent, "open")

		h.deliverIssueClosed(t, "bind-gh2", closedBody(splitCloseRepo, splitCloseContract, `"completed"`, splitCloseInstallID))

		if got := h.gh.issue(t, splitCloseRepo, splitCloseParent); got.state != "closed" {
			t.Errorf("GitHub parent state = %q, want closed", got.state)
		}
	})
}

// splitParentNoIssueOpsForge is a forge.Forge that does NOT implement
// forge.IssueOperations (the embedded nil interface satisfies Forge for the
// type assertion; no method is ever called).
type splitParentNoIssueOpsForge struct{ forge.Forge }

// TestSplitParentClose_GitLab_ResolverNilRungs pins each fail-closed nil rung
// of splitParentIssueOpsFor for a non-github family: a resolver error, a nil
// forge, a TYPED-nil forge wrapped in a non-nil interface, and a forge lacking
// the capability. Each is the pre-#2900 nil-GitHub posture — INFO log, ZERO
// forge calls, NO observation (a server misconfiguration is not a fact about the
// split) — and never a panic. Deleting the isNilForge guard panics on the
// typed-nil case; deleting the type assertion's ok check panics on the
// no-capability case.
func TestSplitParentClose_GitLab_ResolverNilRungs(t *testing.T) {
	rungs := map[string]func(string) (forge.Forge, error){
		"resolver error": func(string) (forge.Forge, error) { return nil, errors.New("registry unavailable") },
		"nil forge":      func(string) (forge.Forge, error) { return nil, nil },
		"typed-nil forge": func(string) (forge.Forge, error) {
			var f *forgegitlab.Forge
			return f, nil
		},
		"forge without IssueOperations": func(string) (forge.Forge, error) { return splitParentNoIssueOpsForge{}, nil },
	}
	for name, resolver := range rungs {
		t.Run(name, func(t *testing.T) {
			h := newSplitParentCloseHarness(t, func(c *Config) { c.ForgeResolver = resolver })
			h.au.seedLinkageForge(t, webhook.ForgeGitLab, splitCloseGitLabRepo, splitCloseParent, splitCloseContract, splitCloseTime())
			h.gl.seedIssue(splitCloseGitLabProject, splitCloseParent, "opened")

			h.deliverIssueClosedGitLabRedelivery(t, "gl-rung-"+strings.ReplaceAll(name, " ", "-"))

			if calls := h.gl.callLog(); len(calls) != 0 {
				t.Errorf("expected ZERO GitLab calls, got %v", calls)
			}
			if n := h.au.globalCount(); n != 0 {
				t.Errorf("wrote %d audit entries, want 0 (misconfiguration is not a fact about the split)", n)
			}
			if got := h.gl.issue(t, splitCloseGitLabProject, splitCloseParent); got.state != "opened" {
				t.Errorf("parent native state = %q, want opened", got.state)
			}
		})
	}
}

// TestSplitParentClose_GitLab_DefaultResolverIsRegistry: with cfg.ForgeResolver
// nil the ladder falls back to forge.Get. The process registry has no "gitlab"
// forge registered in this test binary, so the rung resolves to nil and the
// delivery is the same defined skip — pinning that the default is the registry
// and not, say, a fall-through to cfg.GitHub (which would make GitHub calls).
func TestSplitParentClose_GitLab_DefaultResolverIsRegistry(t *testing.T) {
	h := newSplitParentCloseHarness(t, func(c *Config) { c.ForgeResolver = nil })
	if _, err := forge.Get(webhook.ForgeGitLab); err == nil {
		t.Skip("a gitlab forge is registered in this process; the registry fallback is not observable here")
	}
	h.au.seedLinkageForge(t, webhook.ForgeGitLab, splitCloseGitLabRepo, splitCloseParent, splitCloseContract, splitCloseTime())
	h.gl.seedIssue(splitCloseGitLabProject, splitCloseParent, "opened")

	h.deliverIssueClosedGitLabRedelivery(t, "gl-default-resolver")

	if calls := h.gh.callLog(); len(calls) != 0 {
		t.Errorf("a gitlab delivery with the default resolver fell through to GitHub: %v", calls)
	}
	if calls := h.gl.callLog(); len(calls) != 0 {
		t.Errorf("expected ZERO GitLab calls (nothing registered), got %v", calls)
	}
	if n := h.au.globalCount(); n != 0 {
		t.Errorf("wrote %d audit entries, want 0", n)
	}
}
