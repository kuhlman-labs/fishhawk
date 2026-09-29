package mcpserver

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/kuhlman-labs/fishhawk/backend/internal/apitoken"
	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/campaign"
	"github.com/kuhlman-labs/fishhawk/backend/internal/captain"
	"github.com/kuhlman-labs/fishhawk/backend/internal/decisionindex"
	"github.com/kuhlman-labs/fishhawk/backend/internal/digest"
	"github.com/kuhlman-labs/fishhawk/backend/internal/handoverbrief"
	"github.com/kuhlman-labs/fishhawk/backend/internal/pgtest"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
	"github.com/kuhlman-labs/fishhawk/backend/internal/server"
)

// fishhawk_handover_brief (E76.4 / #3767).

const briefTestRepo = "kuhlman-labs/brief-mcp"

// briefFixture builds a canonical brief carrying n cited merges and one
// uuid-bearing in-flight run, stamped with its canonical hash exactly as
// handoverbrief.Compose stamps it.
func briefFixture(n int) handoverbrief.Brief {
	items := make([]digest.Item, n)
	for i := range items {
		items[i] = digest.Item{
			Category: "merge_verdict_recorded", SourceSequence: int64(i + 2),
			SourceEntryHash: strings.Repeat("a", 64), RunID: uuid.New(),
			At: time.Unix(1_700_000_000, 0).UTC(), Outcome: "merged",
			Headline: strings.Repeat("h", 200),
		}
	}
	head := int64(n + 1)
	b := handoverbrief.Brief{
		Repo: briefTestRepo, CaptainSubject: "github:alice", Successor: "github:carol",
		Window: handoverbrief.Window{FromSequence: 2, ToSequence: head, ChainHead: head, Basis: handoverbrief.BasisSinceLastHandover},
		Sections: []handoverbrief.Section{
			{Kind: handoverbrief.SectionWhatChanged, Parts: []handoverbrief.Part{{Kind: handoverbrief.PartMerges, Items: items, Complete: true}}},
			{Kind: handoverbrief.SectionInFlight, Parts: []handoverbrief.Part{{Kind: handoverbrief.PartRuns, Complete: true,
				InFlight: []handoverbrief.InFlightItem{{Kind: "run", ID: uuid.New(), State: "running", CreatedAt: time.Unix(1_700_000_000, 0).UTC()}}}}},
		},
		Absent: handoverbrief.DeclaredAbsences(), Gaps: []digest.Gap{}, Degradations: []handoverbrief.Degradation{},
	}
	b.BriefHash = handoverbrief.Hash(b)
	return b
}

// fakeBriefBackend serves GET /v0/handover-brief from brief, applying the SAME
// server-side handoverbrief.Bound at DefaultByteBudget the real handler
// applies, and records every request's raw query.
func fakeBriefBackend(t *testing.T, brief handoverbrief.Brief) (*httptest.Server, *[]string) {
	t.Helper()
	var queries []string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/v0/handover-brief" {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
			return
		}
		queries = append(queries, r.URL.RawQuery)
		b, err := handoverbrief.Bound(brief, handoverbrief.DefaultByteBudget)
		if err != nil {
			t.Errorf("server-side bound: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(b)
	}))
	t.Cleanup(ts.Close)
	return ts, &queries
}

// TestHandoverBrief_BoundsAtSessionBudgetKeepingCanonicalHash (approval
// condition 1, MCP half): the REST body is re-bounded by handoverbrief.Bound
// at the session budget; the result fits, is genuinely truncated with a
// cursor, and reports the canonical brief_hash — never a re-hash of its
// bounded body.
func TestHandoverBrief_BoundsAtSessionBudgetKeepingCanonicalHash(t *testing.T) {
	const budget = mcpConvergenceFloorBytes
	canon := briefFixture(60)
	ts, _ := fakeBriefBackend(t, canon)
	r := digestResolver(ts.URL, map[string]string{mcpResponseBudgetEnvVar: strconv.Itoa(budget)})

	_, out, err := r.handoverBrief(context.Background(), nil, HandoverBriefInput{Repo: briefTestRepo})
	if err != nil {
		t.Fatalf("handover brief: %v", err)
	}
	if out.Brief == nil {
		t.Fatal("brief is nil")
	}
	raw, _ := json.Marshal(out.Brief)
	if len(raw) > budget {
		t.Errorf("tool brief is %d bytes, want <= the session budget %d", len(raw), budget)
	}
	if !out.Brief.Truncated {
		t.Fatal("brief not truncated at the floor budget; the hash check would be vacuous")
	}
	p := out.Brief.Sections[0].Parts[0]
	if !p.Truncated || p.Next == nil || !strings.HasPrefix(p.Next.Call, "GET /v0/digest?") {
		t.Errorf("merges part = truncated %v next %+v, want a cursor naming the digest query", p.Truncated, p.Next)
	}
	if out.Brief.BriefHash != canon.BriefHash {
		t.Errorf("bounded brief_hash = %q, want the canonical %q", out.Brief.BriefHash, canon.BriefHash)
	}
	if rehash := handoverbrief.Hash(*out.Brief); rehash == canon.BriefHash {
		t.Errorf("the bounded body hashes to the canonical hash; the fixture does not distinguish a re-hash")
	}
}

// TestHandoverBrief_ForwardsSelectorAndRepoFallback: section and the two
// sequences reach the backend verbatim (trimmed), and an omitted repo falls
// back to GITHUB_REPOSITORY.
func TestHandoverBrief_ForwardsSelectorAndRepoFallback(t *testing.T) {
	ts, queries := fakeBriefBackend(t, briefFixture(2))
	r := digestResolver(ts.URL, map[string]string{"GITHUB_REPOSITORY": briefTestRepo})
	if _, _, err := r.handoverBrief(context.Background(), nil, HandoverBriefInput{Section: " in_flight ", FromSequence: 4, ToSequence: 9}); err != nil {
		t.Fatalf("handover brief: %v", err)
	}
	want := "from_sequence=4&repo=" + strings.ReplaceAll(briefTestRepo, "/", "%2F") + "&section=in_flight&to_sequence=9"
	if len(*queries) != 1 || (*queries)[0] != want {
		t.Errorf("queries = %v, want [%s]", *queries, want)
	}
}

// TestHandoverBrief_RefusesBeforeHTTP: a missing repo and a negative sequence
// are refused locally, with ZERO backend requests.
func TestHandoverBrief_RefusesBeforeHTTP(t *testing.T) {
	var hits atomic.Int32
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	t.Cleanup(ts.Close)
	r := digestResolver(ts.URL, nil)
	cases := []struct {
		name string
		in   HandoverBriefInput
		want string
	}{
		{"repo missing", HandoverBriefInput{}, "repo is required"},
		{"negative from_sequence", HandoverBriefInput{Repo: briefTestRepo, FromSequence: -1}, "non-negative"},
		{"negative to_sequence", HandoverBriefInput{Repo: briefTestRepo, ToSequence: -1}, "non-negative"},
	}
	for _, c := range cases {
		_, _, err := r.handoverBrief(context.Background(), nil, c.in)
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: err = %v, want it to contain %q", c.name, err, c.want)
		}
	}
	if n := hits.Load(); n != 0 {
		t.Errorf("backend hit %d times, want 0 — every refusal must fire before HTTP", n)
	}
}

// TestHandoverBrief_SurfacesBackendRefusal: a backend 503 passes through as the
// typed *apiError with its code.
func TestHandoverBrief_SurfacesBackendRefusal(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = io.WriteString(w, `{"error":{"code":"handover_brief_unavailable","message":"the handover brief could not be established (captain_record_read_failed)"}}`)
	}))
	t.Cleanup(ts.Close)
	_, _, err := digestResolver(ts.URL, nil).handoverBrief(context.Background(), nil, HandoverBriefInput{Repo: briefTestRepo})
	var ae *apiError
	if !errors.As(err, &ae) || ae.Code != "handover_brief_unavailable" {
		t.Errorf("err = %v, want *apiError handover_brief_unavailable", err)
	}
}

// TestHandoverBrief_BudgetBelowFloorIsAnError: a brief whose constant-size
// floor (here, many sequence-less degradations) cannot fit the session budget
// is a tool error wrapping handoverbrief.ErrBudgetTooSmall, never an
// over-budget result.
func TestHandoverBrief_BudgetBelowFloorIsAnError(t *testing.T) {
	b := briefFixture(1)
	for i := 0; i < 40; i++ {
		b.Degradations = append(b.Degradations, handoverbrief.Degradation{
			Kind: handoverbrief.DegradationRunReadFailed, Section: handoverbrief.SectionInFlight, Detail: strings.Repeat("d", 200),
		})
	}
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(b)
	}))
	t.Cleanup(ts.Close)
	r := digestResolver(ts.URL, map[string]string{mcpResponseBudgetEnvVar: strconv.Itoa(mcpConvergenceFloorBytes)})
	_, out, err := r.handoverBrief(context.Background(), nil, HandoverBriefInput{Repo: briefTestRepo})
	if !errors.Is(err, handoverbrief.ErrBudgetTooSmall) {
		t.Errorf("err = %v (out %+v), want handoverbrief.ErrBudgetTooSmall", err, out.Brief)
	}
}

// TestHandoverBriefTool_WireRoundTrip drives the REGISTERED tool over a real
// client session (in-memory transport) against an httptest backend. The brief
// carries uuid.UUID fields; without the explicit output schema the SDK would
// reflect them as a 16-integer array and reject the string the value
// marshals to.
func TestHandoverBriefTool_WireRoundTrip(t *testing.T) {
	ctx := context.Background()
	canon := briefFixture(2)
	ts, _ := fakeBriefBackend(t, canon)
	srv := mcp.NewServer(&mcp.Implementation{Name: "fishhawk", Version: "test"}, nil)
	registerHandoverBrief(srv, digestResolver(ts.URL, nil))
	client := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "0"}, nil)
	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	serverSession, err := srv.Connect(ctx, serverTransport, nil)
	if err != nil {
		t.Fatalf("server connect: %v", err)
	}
	defer func() { _ = serverSession.Close() }()
	clientSession, err := client.Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatalf("client connect: %v", err)
	}
	defer func() { _ = clientSession.Close() }()

	res, err := clientSession.CallTool(ctx, &mcp.CallToolParams{Name: "fishhawk_handover_brief", Arguments: map[string]any{"repo": briefTestRepo}})
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if res.IsError {
		t.Fatalf("tool returned an error result: %+v", res.Content)
	}
	raw, _ := json.Marshal(res.StructuredContent)
	var out HandoverBriefOutput
	if err := json.Unmarshal(raw, &out); err != nil || out.Brief == nil {
		t.Fatalf("structured content = %s (%v)", raw, err)
	}
	if out.Brief.BriefHash != canon.BriefHash || len(out.Brief.Sections) != 2 || len(out.Brief.Sections[0].Parts[0].Items) != 2 ||
		out.Brief.Sections[1].Parts[0].InFlight[0].ID != canon.Sections[1].Parts[0].InFlight[0].ID {
		t.Errorf("round-tripped brief = %s, want the two-merge, one-run brief with hash %s", raw, canon.BriefHash)
	}
	var sawAbsentDoctrine bool
	for _, a := range out.Brief.Absent {
		if a.Section == "doctrine_changes" {
			sawAbsentDoctrine = true
		}
	}
	if !sawAbsentDoctrine {
		t.Errorf("absent = %+v, want doctrine_changes declared unavailable over the wire", out.Brief.Absent)
	}
}

// TestHandoverBriefTool_EndToEndThroughRealServer (approval condition 1, end
// to end across the MCP seam): against an httptest server running the REAL
// fishhawkd handler over a migrated pgtest schema, an offer is POSTed; the
// COMMITTED captain_handover_offered payload is read straight from
// audit_entries; and the tool, re-reading that offer's window at the 4 KiB
// floor budget, returns a genuinely TRUNCATED render whose brief_hash equals
// the committed one. A seeded pending campaign must reach the in_flight
// section through the server's production campaign.Repository adapter
// (server.campaignInFlightLister), which no handoverbrief test exercises.
func TestHandoverBriefTool_EndToEndThroughRealServer(t *testing.T) {
	ctx := context.Background()
	pool := pgtest.NewPool(t)
	auditRepo, err := decisionindex.NewIndexingRepository(audit.NewPostgresRepository(pool), decisionindex.NewStore(pool),
		decisionindex.NewPoolResolver(pool), slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("indexing repository: %v", err)
	}
	const bearer = "fhk_brief_e2e"
	tokRepo := &stubMCPAPITokens{tok: &apitoken.Token{
		ID: uuid.New(), Subject: "github:alice", Scopes: []string{"read:audit", "write:approvals"}, PlainText: bearer,
	}}
	campaigns := campaign.NewPostgresRepository(pool)
	camp, err := campaigns.CreateCampaign(ctx, campaign.CreateCampaignParams{Repo: briefTestRepo, EpicRef: "issue:3767"})
	if err != nil {
		t.Fatalf("create campaign: %v", err)
	}
	s := server.New(server.Config{
		AuditRepo: auditRepo, APITokenRepo: tokRepo,
		DigestStore: digest.NewStore(pool), DigestIndex: decisionindex.NewStore(pool),
		CaptainStore: captain.NewStore(pool),
		RunRepo:      run.NewPostgresRepository(pool),
		CampaignRepo: campaigns,
	})
	httpSrv := httptest.NewServer(s.Handler())
	t.Cleanup(httpSrv.Close)

	// alice is captain BY CONSTRUCTION, then enough cited merges land that
	// the brief exceeds the 4 KiB floor budget.
	kind, alice := audit.ActorUser, "github:alice"
	assigned, _ := json.Marshal(map[string]any{"repo": briefTestRepo, "subject": alice, "identity_verified": true})
	if _, err := auditRepo.AppendGlobalChained(ctx, audit.GlobalChainAppendParams{
		Timestamp: time.Now().UTC(), Category: captain.CategoryAssigned, ActorKind: &kind, ActorSubject: &alice, Payload: assigned,
	}); err != nil {
		t.Fatalf("seed captain: %v", err)
	}
	operator := "operator"
	for i := 0; i < 30; i++ {
		runID := uuid.New()
		if _, err := pool.Exec(ctx, `INSERT INTO runs (id, repo, workflow_id, workflow_sha, trigger_source, state, runner_kind)
			VALUES ($1, $2, 'feature_change', 'sha', 'cli', 'succeeded', 'local')`, runID, briefTestRepo); err != nil {
			t.Fatalf("seed run: %v", err)
		}
		if _, err := auditRepo.AppendChained(ctx, audit.ChainAppendParams{
			RunID: runID, Timestamp: time.Now().UTC(), Category: "merge_verdict_recorded",
			ActorKind: &kind, ActorSubject: &operator, Payload: json.RawMessage(`{"verdict":"merged"}`),
		}); err != nil {
			t.Fatalf("append merge: %v", err)
		}
	}

	r := &runResolver{
		api:    newAPIClient(config{backendURL: httpSrv.URL, apiToken: bearer}),
		getenv: envFuncFromMap(map[string]string{mcpResponseBudgetEnvVar: strconv.Itoa(mcpConvergenceFloorBytes)}),
	}
	if _, err := r.api.PostCaptainVerb(ctx, "offer", briefTestRepo, "github:carol"); err != nil {
		t.Fatalf("offer: %v", err)
	}

	var rawPayload []byte
	if err := pool.QueryRow(ctx, `SELECT payload FROM audit_entries
		WHERE run_id IS NULL AND category = 'captain_handover_offered' AND payload->>'repo' = $1
		ORDER BY sequence DESC LIMIT 1`, briefTestRepo).Scan(&rawPayload); err != nil {
		t.Fatalf("read committed offer: %v", err)
	}
	var p struct {
		BriefHash         string `json:"brief_hash"`
		BriefFromSequence int64  `json:"brief_from_sequence"`
		BriefToSequence   int64  `json:"brief_to_sequence"`
	}
	if err := json.Unmarshal(rawPayload, &p); err != nil || p.BriefHash == "" || p.BriefToSequence == 0 {
		t.Fatalf("committed offer payload = %s (%v), want a brief_hash and window", rawPayload, err)
	}

	_, out, err := r.handoverBrief(ctx, nil, HandoverBriefInput{Repo: briefTestRepo, ToSequence: p.BriefToSequence})
	if err != nil {
		t.Fatalf("handover brief: %v", err)
	}
	if out.Brief == nil || !out.Brief.Truncated {
		t.Fatalf("brief = %+v, want a truncated render at the floor budget (else the hash check is vacuous)", out.Brief)
	}
	if out.Brief.Window.FromSequence != p.BriefFromSequence || out.Brief.Window.ToSequence != p.BriefToSequence {
		t.Errorf("tool window = %d..%d, want the committed %d..%d", out.Brief.Window.FromSequence, out.Brief.Window.ToSequence,
			p.BriefFromSequence, p.BriefToSequence)
	}
	if out.Brief.BriefHash != p.BriefHash {
		t.Errorf("bounded MCP brief_hash = %q, want the committed %q", out.Brief.BriefHash, p.BriefHash)
	}
	raw, _ := json.Marshal(out.Brief)
	if len(raw) > mcpConvergenceFloorBytes {
		t.Errorf("tool brief is %d bytes, want <= %d", len(raw), mcpConvergenceFloorBytes)
	}

	_, inFlight, err := r.handoverBrief(ctx, nil, HandoverBriefInput{Repo: briefTestRepo, Section: string(handoverbrief.SectionInFlight)})
	if err != nil {
		t.Fatalf("handover brief (in_flight): %v", err)
	}
	var found bool
	for _, sec := range inFlight.Brief.Sections {
		for _, part := range sec.Parts {
			for _, it := range part.InFlight {
				if part.Kind == handoverbrief.PartCampaigns && it.Kind == "campaign" && it.ID == camp.ID && it.Ref == "issue:3767" {
					found = true
				}
			}
		}
	}
	if !found {
		t.Errorf("in_flight = %+v, want campaign %s (issue:3767) projected by the server's campaign adapter", inFlight.Brief.Sections, camp.ID)
	}
}
