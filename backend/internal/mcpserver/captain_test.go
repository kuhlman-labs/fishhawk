package mcpserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
)

const captainTestRepo = "kuhlman-labs/captain-mcp"

// captainStubBackend answers GET /v0/captain with getBody and every POST
// verb with postStatus/postBody, recording each request line.
type captainStubBackend struct {
	requests   []string
	bodies     []string
	getBody    string
	postStatus int
	postBody   string
}

func (b *captainStubBackend) serve(t *testing.T) *httptest.Server {
	t.Helper()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		b.requests = append(b.requests, r.Method+" "+r.URL.RequestURI())
		b.bodies = append(b.bodies, string(raw))
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodGet {
			_, _ = io.WriteString(w, b.getBody)
			return
		}
		if b.postStatus != 0 {
			w.WriteHeader(b.postStatus)
		}
		_, _ = io.WriteString(w, b.postBody)
	}))
	t.Cleanup(ts.Close)
	return ts
}

const captainReadBody = `{"repo":"` + captainTestRepo + `","captain":{"subject":"github:alice","identity_verified":true,"claim_verified":false,"basis":"claimed","assigned_sequence":2,"assigned_entry_hash":"h2","assigned_at":"2026-09-01T00:00:00Z"},"pending_offer":{"successor":"ops@local","identity_verified":false,"offered_by":"github:alice","offer_entry_hash":"h3","offered_sequence":3,"offered_at":"2026-09-01T00:00:00Z"},"last_captain":null,"history":[],"history_total":3,"skipped_entries":0}`

// TestCaptainTool_ReadRendersFlagsSeparately: the default action GETs
// /v0/captain and the output carries claim_verified and identity_verified as
// separate fields — the one state where they disagree.
func TestCaptainTool_ReadRendersFlagsSeparately(t *testing.T) {
	b := &captainStubBackend{getBody: captainReadBody}
	r := digestResolver(b.serve(t).URL, nil)
	_, out, err := r.captain(context.Background(), nil, CaptainInput{Repo: captainTestRepo})
	if err != nil {
		t.Fatalf("captain read: %v", err)
	}
	if len(b.requests) != 1 || b.requests[0] != "GET /v0/captain?repo=kuhlman-labs%2Fcaptain-mcp" {
		t.Errorf("requests = %v, want one GET /v0/captain", b.requests)
	}
	raw, _ := json.Marshal(out)
	var wire struct {
		Record struct {
			Captain      map[string]any `json:"captain"`
			PendingOffer map[string]any `json:"pending_offer"`
		} `json:"record"`
	}
	_ = json.Unmarshal(raw, &wire)
	if wire.Record.Captain["identity_verified"] != true || wire.Record.Captain["claim_verified"] != false {
		t.Errorf("captain = %v, want identity_verified true and claim_verified false", wire.Record.Captain)
	}
	if wire.Record.PendingOffer["identity_verified"] != false {
		t.Errorf("pending_offer = %v, want identity_verified false", wire.Record.PendingOffer)
	}
	if out.Result != nil || out.Elisions != nil {
		t.Errorf("read output = %+v, want record only, unbounded", out)
	}
}

// TestCaptainTool_VerbPostsAndReturnsEvent: each verb POSTs its route with
// the successor only on offer.
func TestCaptainTool_VerbPostsAndReturnsEvent(t *testing.T) {
	b := &captainStubBackend{postBody: `{"repo":"` + captainTestRepo + `","event":{"sequence":4,"entry_hash":"h4","category":"captain_claimed","at":"2026-09-01T00:00:00Z","payload":{"claim_verified":false}},"captain":null,"pending_offer":null}`}
	r := digestResolver(b.serve(t).URL, nil)
	for _, in := range []CaptainInput{
		{Repo: captainTestRepo, Action: "offer", Successor: "github:bob"},
		{Repo: captainTestRepo, Action: "withdraw"},
		{Repo: captainTestRepo, Action: "accept"},
		{Repo: captainTestRepo, Action: "relinquish"},
		{Repo: captainTestRepo, Action: "claim"},
	} {
		_, out, err := r.captain(context.Background(), nil, in)
		if err != nil {
			t.Fatalf("%s: %v", in.Action, err)
		}
		if out.Result == nil || out.Result.Event.Sequence != 4 || out.Record != nil {
			t.Errorf("%s output = %+v, want the verb result", in.Action, out)
		}
	}
	want := []string{"POST /v0/captain/offer", "POST /v0/captain/withdraw", "POST /v0/captain/accept", "POST /v0/captain/relinquish", "POST /v0/captain/claim"}
	for i, w := range want {
		if b.requests[i] != w {
			t.Errorf("request %d = %s, want %s", i, b.requests[i], w)
		}
	}
	if !strings.Contains(b.bodies[0], `"successor":"github:bob"`) || strings.Contains(b.bodies[4], "successor") {
		t.Errorf("bodies = %v, want successor only on offer", b.bodies)
	}
}

// TestCaptainTool_LocalRefusals: an unknown action, a successor on a
// non-offer action, and a missing repo are refused BEFORE any request.
func TestCaptainTool_LocalRefusals(t *testing.T) {
	b := &captainStubBackend{}
	r := digestResolver(b.serve(t).URL, nil)
	for _, in := range []CaptainInput{
		{Repo: captainTestRepo, Action: "seize"},
		{Repo: captainTestRepo, Action: "claim", Successor: "github:bob"},
		{Repo: captainTestRepo, Successor: "github:bob"},
		{},
	} {
		if _, _, err := r.captain(context.Background(), nil, in); err == nil {
			t.Errorf("captain(%+v) = nil error, want a refusal", in)
		}
	}
	if len(b.requests) != 0 {
		t.Errorf("requests = %v, want none (refused locally)", b.requests)
	}
}

// TestCaptainTool_RepoFallsBackToEnv: an omitted repo resolves from
// GITHUB_REPOSITORY.
func TestCaptainTool_RepoFallsBackToEnv(t *testing.T) {
	b := &captainStubBackend{getBody: captainReadBody}
	r := digestResolver(b.serve(t).URL, map[string]string{"GITHUB_REPOSITORY": captainTestRepo})
	if _, _, err := r.captain(context.Background(), nil, CaptainInput{}); err != nil {
		t.Fatalf("captain: %v", err)
	}
	if b.requests[0] != "GET /v0/captain?repo=kuhlman-labs%2Fcaptain-mcp" {
		t.Errorf("request = %s", b.requests[0])
	}
}

// TestCaptainTool_RefusalPassthrough: one backend refusal per status class
// surfaces as the typed *apiError with its code intact.
func TestCaptainTool_RefusalPassthrough(t *testing.T) {
	for _, tc := range []struct {
		status int
		code   string
	}{
		{http.StatusBadRequest, "captain_successor_required"},
		{http.StatusForbidden, "captain_agent_identity_refused"},
		{http.StatusConflict, "captain_no_offer"},
		{http.StatusUnprocessableEntity, "captain_predicate_undeterminable"},
		{http.StatusNotImplemented, "captain_unconfigured"},
	} {
		b := &captainStubBackend{postStatus: tc.status, postBody: `{"error":{"code":"` + tc.code + `","message":"refused"}}`}
		r := digestResolver(b.serve(t).URL, nil)
		_, _, err := r.captain(context.Background(), nil, CaptainInput{Repo: captainTestRepo, Action: "claim"})
		var ae *apiError
		if !errors.As(err, &ae) || ae.StatusCode != tc.status || ae.Code != tc.code {
			t.Errorf("err = %v, want *apiError %d %s", err, tc.status, tc.code)
		}
	}
}

// captainLongHistory renders a GET body with n history entries each carrying
// a long payload.
func captainLongHistory(n int) string {
	var items []string
	for i := 1; i <= n; i++ {
		items = append(items, fmt.Sprintf(`{"sequence":%d,"entry_hash":"%s","category":"captain_handover_offered","at":"2026-09-01T00:00:00Z","payload":{"note":"%s"}}`,
			i, strings.Repeat("a", 64), strings.Repeat("p", 300)))
	}
	return `{"repo":"` + captainTestRepo + `","captain":{"subject":"github:alice","identity_verified":true,"claim_verified":null,"basis":"assigned","assigned_sequence":1,"assigned_entry_hash":"h1","assigned_at":"2026-09-01T00:00:00Z"},"pending_offer":null,"last_captain":null,"history":[` +
		strings.Join(items, ",") + `],"history_total":` + strconv.Itoa(n) + `,"skipped_entries":0}`
}

// TestCaptainTool_BoundsHistoryAtSessionBudget: a history exceeding the
// session budget is trimmed from the OLDEST end, the result fits, the
// elisions block names the dropped count, and the captain survives.
func TestCaptainTool_BoundsHistoryAtSessionBudget(t *testing.T) {
	const budget = mcpConvergenceFloorBytes
	b := &captainStubBackend{getBody: captainLongHistory(50)}
	r := digestResolver(b.serve(t).URL, map[string]string{mcpResponseBudgetEnvVar: strconv.Itoa(budget)})
	_, out, err := r.captain(context.Background(), nil, CaptainInput{Repo: captainTestRepo})
	if err != nil {
		t.Fatalf("captain: %v", err)
	}
	raw, _ := json.Marshal(out)
	if len(raw) > budget {
		t.Errorf("output is %d bytes, want <= %d", len(raw), budget)
	}
	if out.Elisions == nil || len(out.Elisions.Fields) != 1 || out.Elisions.Fields[0].Field != "record.history" {
		t.Fatalf("elisions = %+v, want one record.history entry", out.Elisions)
	}
	h := out.Record.History
	if len(h) == 0 || h[len(h)-1].Sequence != 50 {
		t.Errorf("history kept %d entries ending at %v, want the NEWEST kept", len(h), h)
	}
	if out.Elisions.Fields[0].OmittedCount != 50-len(h) {
		t.Errorf("omitted_count = %d, want %d", out.Elisions.Fields[0].OmittedCount, 50-len(h))
	}
	if out.Record.Captain == nil || out.Record.Captain.Subject != "github:alice" {
		t.Errorf("captain = %+v, want kept", out.Record.Captain)
	}
}

// TestCaptainTool_FloorWhenOneEntryDoesNotFit: a single oversized history
// entry forces the floor — history dropped under an aggregate elision, the
// captain kept with capped strings, and the result within the floor.
func TestCaptainTool_FloorWhenOneEntryDoesNotFit(t *testing.T) {
	huge := `{"repo":"` + captainTestRepo + `","captain":{"subject":"github:` + strings.Repeat("x", 9000) + `","identity_verified":true,"claim_verified":null,"basis":"assigned","assigned_sequence":1,"assigned_entry_hash":"h1","assigned_at":"2026-09-01T00:00:00Z"},"pending_offer":null,"last_captain":null,"history":[{"sequence":1,"entry_hash":"h1","category":"captain_assigned","at":"2026-09-01T00:00:00Z","payload":{"note":"` + strings.Repeat("p", 9000) + `"}}],"history_total":1,"skipped_entries":0}`
	b := &captainStubBackend{getBody: huge}
	r := digestResolver(b.serve(t).URL, map[string]string{mcpResponseBudgetEnvVar: strconv.Itoa(mcpConvergenceFloorBytes)})
	_, out, err := r.captain(context.Background(), nil, CaptainInput{Repo: captainTestRepo})
	if err != nil {
		t.Fatalf("captain: %v", err)
	}
	raw, _ := json.Marshal(out)
	if len(raw) > mcpConvergenceFloorBytes {
		t.Errorf("floor output is %d bytes, want <= %d", len(raw), mcpConvergenceFloorBytes)
	}
	if out.Elisions == nil || out.Elisions.Tier != floorTierName || len(out.Record.History) != 0 {
		t.Errorf("elisions = %+v history = %d, want the floor tier with history dropped", out.Elisions, len(out.Record.History))
	}
	if out.Record.Captain == nil || !strings.HasPrefix(out.Record.Captain.Subject, "github:") {
		t.Errorf("captain = %+v, want kept (capped)", out.Record.Captain)
	}
}
