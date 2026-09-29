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
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/kuhlman-labs/fishhawk/backend/internal/apitoken"
	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/decisionindex"
	"github.com/kuhlman-labs/fishhawk/backend/internal/digest"
	"github.com/kuhlman-labs/fishhawk/backend/internal/pgtest"
	"github.com/kuhlman-labs/fishhawk/backend/internal/server"
)

const digestTestRepo = "kuhlman-labs/digest-mcp"

// fakeDigestBackend serves GET /v0/digest from a fixed merges item set,
// honouring section + from_sequence and applying the SAME server-side
// digest.Bound at digest.DefaultByteBudget the real handler applies, so the
// tool's re-bound is exercised on a genuinely REST-bounded value.
func fakeDigestBackend(t *testing.T, items []digest.Item) *httptest.Server {
	t.Helper()
	head := items[len(items)-1].SourceSequence
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/v0/digest" {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
			return
		}
		from, _ := strconv.ParseInt(r.URL.Query().Get("from_sequence"), 10, 64)
		var kept []digest.Item
		for _, it := range items {
			if it.SourceSequence >= from {
				kept = append(kept, it)
			}
		}
		d := digest.Digest{
			Repo: digestTestRepo, CaptainSubject: "captain:a",
			FromSequence: from, ToSequence: head, ChainHead: head,
			Section:      digest.SectionKind(r.URL.Query().Get("section")),
			Sections:     []digest.Section{{Kind: digest.SectionMerges, Items: kept, Complete: true}},
			Gaps:         []digest.Gap{},
			Degradations: []digest.Degradation{},
		}
		b, err := digest.Bound(d, digest.DefaultByteBudget)
		if err != nil {
			t.Errorf("server-side bound: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(b)
	}))
	t.Cleanup(ts.Close)
	return ts
}

// digestItems builds n merges items at sequences 1..n, each with a headline
// long enough that ~8 fit a 4 KiB budget.
func digestItems(n int) []digest.Item {
	items := make([]digest.Item, n)
	for i := range items {
		items[i] = digest.Item{
			Category: "merge_verdict_recorded", SourceSequence: int64(i + 1),
			SourceEntryHash: strings.Repeat("a", 64), RunID: uuid.New(),
			At: time.Unix(1_700_000_000, 0).UTC(), Outcome: "merged",
			Headline: strings.Repeat("h", 200),
		}
	}
	return items
}

func digestResolver(url string, env map[string]string) *runResolver {
	return &runResolver{api: newAPIClient(config{backendURL: url, apiToken: "tok"}), getenv: envFuncFromMap(env)}
}

// TestDigest_BoundsAtSessionBudget: the REST response (bounded at 32 KiB)
// is re-bounded by digest.Bound at the session's resolved MCP budget. With
// the budget lowered to the 4 KiB floor, the tool's digest fits it and marks
// truncation with a cursor at the first omitted item.
func TestDigest_BoundsAtSessionBudget(t *testing.T) {
	const budget = mcpConvergenceFloorBytes
	items := digestItems(60)
	ts := fakeDigestBackend(t, items)
	r := digestResolver(ts.URL, map[string]string{mcpResponseBudgetEnvVar: strconv.Itoa(budget)})

	_, out, err := r.digest(context.Background(), nil, DigestInput{Repo: digestTestRepo})
	if err != nil {
		t.Fatalf("digest: %v", err)
	}
	raw, _ := json.Marshal(out.Digest)
	if len(raw) > budget {
		t.Errorf("tool digest is %d bytes, want <= the session budget %d", len(raw), budget)
	}
	if out.Digest == nil || !out.Digest.Truncated || out.Digest.Next == nil {
		t.Fatalf("digest nil=%v, want truncated with a next cursor", out.Digest == nil)
	}
	s := out.Digest.Sections[0]
	if len(s.Items) == 0 || s.Next == nil || s.Next.FromSequence != s.Items[len(s.Items)-1].SourceSequence+1 {
		t.Errorf("section next = %+v after %d items, want the first OMITTED item's sequence", s.Next, len(s.Items))
	}
	if s.OmittedCount != len(items)-len(s.Items) {
		t.Errorf("omitted_count = %d, want %d", s.OmittedCount, len(items)-len(s.Items))
	}
}

// TestDigest_FollowingCursorThroughToolTerminates: following the tool's own
// cursors (section + from_sequence passed back as arguments) retrieves every
// item exactly once, strictly advancing, in a bounded number of calls — the
// composition of the REST bound and the MCP re-bound never stalls. 150 items
// exceed the 32 KiB REST budget too, so both bounds bite.
func TestDigest_FollowingCursorThroughToolTerminates(t *testing.T) {
	items := digestItems(150)
	ts := fakeDigestBackend(t, items)
	r := digestResolver(ts.URL, map[string]string{mcpResponseBudgetEnvVar: strconv.Itoa(mcpConvergenceFloorBytes)})

	seen := map[int64]int{}
	in := DigestInput{Repo: digestTestRepo, Section: string(digest.SectionMerges)}
	prevFrom := int64(-1)
	for call := 0; ; call++ {
		if call > len(items) {
			t.Fatalf("cursor chain did not terminate within %d calls", len(items))
		}
		_, out, err := r.digest(context.Background(), nil, in)
		if err != nil {
			t.Fatalf("call %d: %v", call, err)
		}
		if raw, _ := json.Marshal(out.Digest); len(raw) > mcpConvergenceFloorBytes {
			t.Fatalf("call %d: digest is %d bytes, want <= the session budget %d", call, len(raw), mcpConvergenceFloorBytes)
		}
		s := out.Digest.Sections[0]
		if len(s.Items) == 0 && !s.Complete {
			t.Fatalf("call %d returned no item and did not report the section complete", call)
		}
		for _, it := range s.Items {
			seen[it.SourceSequence]++
		}
		if s.Next == nil {
			if !s.Complete {
				t.Fatalf("call %d: section has no cursor but is not complete", call)
			}
			break
		}
		if s.Next.FromSequence <= prevFrom || s.Next.FromSequence <= in.FromSequence {
			t.Fatalf("call %d: cursor from_sequence %d did not strictly advance (was %d)", call, s.Next.FromSequence, in.FromSequence)
		}
		prevFrom = in.FromSequence
		in = DigestInput{Repo: digestTestRepo, Section: string(s.Next.Section), FromSequence: s.Next.FromSequence, ToSequence: s.Next.ToSequence}
	}
	if len(seen) != len(items) {
		t.Errorf("retrieved %d distinct items, want %d", len(seen), len(items))
	}
	for seq, n := range seen {
		if n != 1 {
			t.Errorf("item %d retrieved %d times, want once", seq, n)
		}
	}
}

// TestDigest_ReadForwardsSelector: section/from_sequence/to_sequence reach
// GET /v0/digest verbatim, and zero values are omitted so the backend applies
// its watermark / chain-head defaults.
func TestDigest_ReadForwardsSelector(t *testing.T) {
	var queries []string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		queries = append(queries, r.URL.RawQuery)
		_, _ = io.WriteString(w, `{"repo":"`+digestTestRepo+`","sections":[],"gaps":[],"degradations":[]}`)
	}))
	t.Cleanup(ts.Close)
	r := digestResolver(ts.URL, nil)
	for _, in := range []DigestInput{
		{Repo: digestTestRepo},
		{Repo: digestTestRepo, Section: "gaps", FromSequence: 7, ToSequence: 9},
	} {
		if _, _, err := r.digest(context.Background(), nil, in); err != nil {
			t.Fatalf("digest(%+v): %v", in, err)
		}
	}
	if queries[0] != "repo=kuhlman-labs%2Fdigest-mcp" {
		t.Errorf("default query = %q, want repo only", queries[0])
	}
	if queries[1] != "from_sequence=7&repo=kuhlman-labs%2Fdigest-mcp&section=gaps&to_sequence=9" {
		t.Errorf("selector query = %q", queries[1])
	}
}

// TestDigest_RepoFallsBackToEnv: an omitted repo resolves from
// GITHUB_REPOSITORY.
func TestDigest_RepoFallsBackToEnv(t *testing.T) {
	var got string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.URL.Query().Get("repo")
		_, _ = io.WriteString(w, `{"sections":[],"gaps":[],"degradations":[]}`)
	}))
	t.Cleanup(ts.Close)
	r := digestResolver(ts.URL, map[string]string{"GITHUB_REPOSITORY": "o/from-env"})
	if _, _, err := r.digest(context.Background(), nil, DigestInput{}); err != nil {
		t.Fatalf("digest: %v", err)
	}
	if got != "o/from-env" {
		t.Errorf("repo sent = %q, want the GITHUB_REPOSITORY fallback", got)
	}
}

// TestDigest_MarkReadPostsToSequence: mark_read=true POSTs mark-read with
// repo + to_sequence and returns the watermark result; it does NOT read a
// digest.
func TestDigest_MarkReadPostsToSequence(t *testing.T) {
	var method, path string
	var body digestMarkReadRequest
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		method, path = r.Method, r.URL.Path
		_ = json.NewDecoder(r.Body).Decode(&body)
		_, _ = io.WriteString(w, `{"repo":"`+digestTestRepo+`","captain_subject":"captain:a","previous_sequence":3,"had_previous":true,"sequence":12,"advanced":true}`)
	}))
	t.Cleanup(ts.Close)
	r := digestResolver(ts.URL, nil)
	_, out, err := r.digest(context.Background(), nil, DigestInput{Repo: digestTestRepo, MarkRead: true, ToSequence: 12})
	if err != nil {
		t.Fatalf("mark read: %v", err)
	}
	if method != http.MethodPost || path != "/v0/digest/mark-read" || body.Repo != digestTestRepo || body.ToSequence != 12 {
		t.Errorf("request = %s %s %+v, want POST /v0/digest/mark-read {repo, 12}", method, path, body)
	}
	if out.Digest != nil || out.MarkRead == nil || !out.MarkRead.Advanced || out.MarkRead.Sequence != 12 || out.MarkRead.PreviousSequence != 3 {
		t.Errorf("output = %+v / %+v, want only the mark-read result advanced 3 -> 12", out.Digest, out.MarkRead)
	}
}

// TestDigest_RefusesBeforeHTTP: each input refusal fires BEFORE any request
// reaches the backend, and names what to fix.
func TestDigest_RefusesBeforeHTTP(t *testing.T) {
	hits := 0
	ts := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { hits++ }))
	t.Cleanup(ts.Close)
	r := digestResolver(ts.URL, nil)
	for _, tc := range []struct {
		name string
		in   DigestInput
		want string
	}{
		{"repo missing", DigestInput{}, "repo is required"},
		{"negative from", DigestInput{Repo: digestTestRepo, FromSequence: -1}, "non-negative"},
		{"negative to", DigestInput{Repo: digestTestRepo, ToSequence: -1}, "non-negative"},
		{"mark_read with section", DigestInput{Repo: digestTestRepo, MarkRead: true, ToSequence: 5, Section: "merges"}, "do not apply with mark_read"},
		{"mark_read with from_sequence", DigestInput{Repo: digestTestRepo, MarkRead: true, ToSequence: 5, FromSequence: 2}, "do not apply with mark_read"},
		{"mark_read without to_sequence", DigestInput{Repo: digestTestRepo, MarkRead: true}, "to_sequence is required"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, _, err := r.digest(context.Background(), nil, tc.in)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("err = %v, want one containing %q", err, tc.want)
			}
		})
	}
	if hits != 0 {
		t.Errorf("backend received %d requests, want 0 (every refusal is pre-HTTP)", hits)
	}
}

// TestDigest_SurfacesBackendRefusal: a backend 400 reaches the caller as the
// typed *apiError with its code, so to_sequence_beyond_chain_head's head is
// not lost.
func TestDigest_SurfacesBackendRefusal(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"error":{"code":"to_sequence_beyond_chain_head","message":"digest: to_sequence 999 is beyond the repository's chain head 10"}}`)
	}))
	t.Cleanup(ts.Close)
	r := digestResolver(ts.URL, nil)
	for _, in := range []DigestInput{
		{Repo: digestTestRepo, ToSequence: 999},
		{Repo: digestTestRepo, MarkRead: true, ToSequence: 999},
	} {
		_, _, err := r.digest(context.Background(), nil, in)
		var ae *apiError
		if !errors.As(err, &ae) || ae.Code != "to_sequence_beyond_chain_head" || !strings.Contains(err.Error(), "chain head 10") {
			t.Errorf("mark_read=%v err = %v, want *apiError to_sequence_beyond_chain_head naming head 10", in.MarkRead, err)
		}
	}
}

// TestDigest_BudgetBelowFloorIsAnError: a digest whose constant-size floor
// cannot fit the session budget is a tool error, never an over-budget result.
func TestDigest_BudgetBelowFloorIsAnError(t *testing.T) {
	items := digestItems(3)
	for i := range items {
		// A single-section call keeps its first item in the floor; make the
		// sequence-less degradations push that floor over 4 KiB.
		items[i].Headline = "x"
	}
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		degs := make([]digest.Degradation, 40)
		for i := range degs {
			degs[i] = digest.Degradation{Kind: digest.DegradationScanLimit, Section: digest.SectionMerges, Detail: strings.Repeat("d", 200)}
		}
		_ = json.NewEncoder(w).Encode(digest.Digest{Repo: digestTestRepo, Sections: []digest.Section{{Kind: digest.SectionMerges, Items: items}},
			Gaps: []digest.Gap{}, Degradations: degs})
	}))
	t.Cleanup(ts.Close)
	r := digestResolver(ts.URL, map[string]string{mcpResponseBudgetEnvVar: strconv.Itoa(mcpConvergenceFloorBytes)})
	_, out, err := r.digest(context.Background(), nil, DigestInput{Repo: digestTestRepo})
	if !errors.Is(err, digest.ErrBudgetTooSmall) {
		t.Errorf("err = %v (out %+v), want digest.ErrBudgetTooSmall", err, out.Digest)
	}
}

// TestDigest_OutputPassesSDKSchemaValidation drives a real tools/call over
// the in-memory transport. digest.Item carries uuid.UUID fields; without the
// explicit output schema the SDK would reflect them as a 16-integer array and
// reject the string the value marshals to.
func TestDigest_OutputPassesSDKSchemaValidation(t *testing.T) {
	ctx := context.Background()
	ts := fakeDigestBackend(t, digestItems(2))
	srv := mcp.NewServer(&mcp.Implementation{Name: "fishhawk", Version: "test"}, nil)
	registerDigest(srv, digestResolver(ts.URL, nil))
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

	res, err := clientSession.CallTool(ctx, &mcp.CallToolParams{Name: "fishhawk_digest", Arguments: map[string]any{"repo": digestTestRepo}})
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if res.IsError {
		t.Fatalf("tool returned an error result: %+v", res.Content)
	}
	raw, _ := json.Marshal(res.StructuredContent)
	var out DigestOutput
	if err := json.Unmarshal(raw, &out); err != nil || out.Digest == nil || len(out.Digest.Sections[0].Items) != 2 {
		t.Fatalf("structured content = %s (%v), want the two-item digest", raw, err)
	}
}

// TestDigestTool_EndToEndThroughRealServer drives the MCP tool against an
// httptest server running the REAL fishhawkd handler (bearer auth, scope
// checks, digest.Build over a migrated pgtest schema, the production indexing
// audit wiring): read the digest, mark it read, and re-read — the seam
// between the MCP layer and the REST layer, end to end.
func TestDigestTool_EndToEndThroughRealServer(t *testing.T) {
	ctx := context.Background()
	pool := pgtest.NewPool(t)
	auditRepo, err := decisionindex.NewIndexingRepository(audit.NewPostgresRepository(pool), decisionindex.NewStore(pool),
		decisionindex.NewPoolResolver(pool), slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("indexing repository: %v", err)
	}
	const bearer = "fhk_digest_e2e"
	tokRepo := &stubMCPAPITokens{tok: &apitoken.Token{
		ID: uuid.New(), Subject: "captain:mcp", Scopes: []string{"read:audit", "write:approvals"}, PlainText: bearer,
	}}
	s := server.New(server.Config{
		AuditRepo: auditRepo, APITokenRepo: tokRepo,
		DigestStore: digest.NewStore(pool), DigestIndex: decisionindex.NewStore(pool),
	})
	httpSrv := httptest.NewServer(s.Handler())
	t.Cleanup(httpSrv.Close)

	runID := uuid.New()
	if _, err := pool.Exec(ctx, `INSERT INTO runs (id, repo, workflow_id, workflow_sha, trigger_source, state, runner_kind)
		VALUES ($1, $2, 'feature_change', 'sha', 'cli', 'succeeded', 'local')`, runID, digestTestRepo); err != nil {
		t.Fatalf("seed run: %v", err)
	}
	kind, subject := audit.ActorUser, "operator"
	merge, err := auditRepo.AppendChained(ctx, audit.ChainAppendParams{
		RunID: runID, Timestamp: time.Now().UTC(), Category: "merge_verdict_recorded",
		ActorKind: &kind, ActorSubject: &subject, Payload: json.RawMessage(`{"verdict":"merged"}`),
	})
	if err != nil {
		t.Fatalf("append merge: %v", err)
	}

	r := &runResolver{api: newAPIClient(config{backendURL: httpSrv.URL, apiToken: bearer}), getenv: envFuncFromMap(nil)}
	_, out, err := r.digest(ctx, nil, DigestInput{Repo: digestTestRepo})
	if err != nil {
		t.Fatalf("read digest: %v", err)
	}
	d := out.Digest
	if d == nil || d.CaptainSubject != "captain:mcp" || d.ToSequence != merge.Sequence {
		t.Fatalf("digest = %+v, want captain:mcp's window ending at %d", d, merge.Sequence)
	}
	var merges digest.Section
	for _, sec := range d.Sections {
		if sec.Kind == digest.SectionMerges {
			merges = sec
		}
	}
	if len(merges.Items) != 1 || merges.Items[0].SourceSequence != merge.Sequence || merges.Items[0].SourceEntryHash != merge.EntryHash ||
		merges.Items[0].RunID != runID {
		t.Fatalf("merges = %+v, want one item citing %d/%s on run %s", merges.Items, merge.Sequence, merge.EntryHash, runID)
	}

	_, mr, err := r.digest(ctx, nil, DigestInput{Repo: digestTestRepo, MarkRead: true, ToSequence: d.ToSequence})
	if err != nil {
		t.Fatalf("mark read: %v", err)
	}
	if mr.MarkRead == nil || !mr.MarkRead.Advanced || mr.MarkRead.Sequence != d.ToSequence || mr.MarkRead.CaptainSubject != "captain:mcp" {
		t.Fatalf("mark-read = %+v, want captain:mcp advanced to %d", mr.MarkRead, d.ToSequence)
	}
	var stored int64
	if err := pool.QueryRow(ctx, `SELECT sequence FROM captain_read_watermarks WHERE captain_subject = 'captain:mcp' AND repo = $1`,
		digestTestRepo).Scan(&stored); err != nil || stored != d.ToSequence {
		t.Errorf("stored watermark = %d (%v), want %d", stored, err, d.ToSequence)
	}

	_, again, err := r.digest(ctx, nil, DigestInput{Repo: digestTestRepo})
	if err != nil {
		t.Fatalf("re-read digest: %v", err)
	}
	if !again.Digest.HasWatermark || again.Digest.Watermark != d.ToSequence {
		t.Errorf("re-read watermark = %d (has %v), want %d", again.Digest.Watermark, again.Digest.HasWatermark, d.ToSequence)
	}
	for _, sec := range again.Digest.Sections {
		if sec.Kind == digest.SectionMerges && len(sec.Items) != 0 {
			t.Errorf("after mark-read merges = %+v, want the already-read item gone", sec.Items)
		}
	}
}
