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
	"github.com/kuhlman-labs/fishhawk/backend/internal/githubclient"
	"github.com/kuhlman-labs/fishhawk/backend/internal/splitfiling"
	"github.com/kuhlman-labs/fishhawk/backend/internal/webhook"
	"github.com/kuhlman-labs/fishhawk/backend/internal/workmgmt"
)

// This file is the END-TO-END seam suite for the E50.6 parent-close watcher
// (#2062). Every behavioral test drives the REAL signed-webhook path —
// postWebhook -> s.Handler() -> handleWebhook -> handleContractChildClosed ->
// a genuine *githubclient.Client -> a STATEFUL httptest GitHub fake — and NO
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

// seedLinkage appends one split_children_filed entry in the shape
// writeSplitChildrenFiledAudit produces, at the given timestamp.
func (a *splitParentCloseAudit) seedLinkage(t *testing.T, parentRepo string, parentIssue, contractChild int, ts time.Time) {
	t.Helper()
	payload, err := json.Marshal(splitChildrenFiledPayload{
		ContractClassification: "delete-only",
		ContractChildNumber:    contractChild,
		DeferralIssue:          splitfiling.DeferralIssue,
		ParentRepo:             parentRepo,
		ParentIssue:            parentIssue,
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
	au *splitParentCloseAudit
}

// newSplitParentCloseHarness wires a Server whose ONLY GitHub is the stateful
// fake and whose ONLY audit store is the ordering-faithful one, reachable
// through the genuine signed webhook endpoint.
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

	au := newSplitParentCloseAudit()
	cfg := Config{
		Addr:                "127.0.0.1:0",
		GitHubWebhookSecret: []byte(testSecret),
		WebhookDeliveries:   webhook.NewMemoryStore(0),
		AuditRepo:           au,
		GitHub: &githubclient.Client{
			BaseURL: srv.URL,
			Tokens:  splitParentTokens{},
			HTTP:    &http.Client{Timeout: 5 * time.Second},
		},
	}
	for _, o := range opts {
		o(&cfg)
	}
	return &splitParentCloseHarness{s: New(cfg), gh: gh, au: au}
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

// --- C5: malformed payload and non-GitHub forge --------------------------

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

// TestContractChildClosed_NonGitHubForge_NoOp: s.cfg.GitHub is a
// *githubclient.Client and cannot serve a GitLab project, so a GitLab-sourced
// event is a defined skip. GitLab parity is deferred to #2900. This drives the
// handler directly because the signed GitHub webhook endpoint cannot produce a
// GitLab-forged Event — the forge gate is what is under test, not the routing.
func TestContractChildClosed_NonGitHubForge_NoOp(t *testing.T) {
	h := newSplitParentCloseHarness(t)
	h.au.seedLinkage(t, splitCloseRepo, splitCloseParent, splitCloseContract, splitCloseTime())
	h.gh.seedIssue(splitCloseRepo, splitCloseParent, "open")

	h.s.handleContractChildClosed(context.Background(), webhook.Event{
		Type:           "issues",
		Action:         "closed",
		Repo:           splitCloseRepo,
		Forge:          webhook.ForgeGitLab,
		InstallationID: splitCloseInstallID,
		RawBody:        closedBody(splitCloseRepo, splitCloseContract, `"completed"`, splitCloseInstallID),
	})

	if calls := h.gh.callLog(); len(calls) != 0 {
		t.Errorf("expected ZERO forge calls for a GitLab event, got %v", calls)
	}
	if n := h.au.globalCount(); n != 0 {
		t.Errorf("wrote %d audit entries for a GitLab event, want 0", n)
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
