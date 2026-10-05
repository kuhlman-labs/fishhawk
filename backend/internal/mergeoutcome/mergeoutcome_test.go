package mergeoutcome

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/forge"
	"github.com/kuhlman-labs/fishhawk/backend/internal/githubclient"
)

// memAudit is the fallback-path store: AppendChained + ListForRunByCategory
// over a mutex-guarded slice (no DedupedChainAppender capability).
type memAudit struct {
	mu      sync.Mutex
	entries []*audit.Entry
	listErr error
	appends int
}

func (m *memAudit) AppendChained(_ context.Context, p audit.ChainAppendParams) (*audit.Entry, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.appends++
	r := p.RunID
	e := &audit.Entry{ID: uuid.New(), Sequence: int64(len(m.entries) + 1), RunID: &r, StageID: p.StageID,
		Timestamp: p.Timestamp, Category: p.Category, ActorKind: p.ActorKind, ActorSubject: p.ActorSubject, Payload: p.Payload}
	m.entries = append(m.entries, e)
	return e, nil
}

func (m *memAudit) ListForRunByCategory(_ context.Context, runID uuid.UUID, category string) ([]*audit.Entry, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.listErr != nil {
		return nil, m.listErr
	}
	var out []*audit.Entry
	for _, e := range m.entries {
		if e.RunID != nil && *e.RunID == runID && e.Category == category {
			out = append(out, e)
		}
	}
	return out, nil
}

func (m *memAudit) rows(runID uuid.UUID, category string) []*audit.Entry {
	out, _ := (&memAudit{entries: m.snapshot()}).ListForRunByCategory(context.Background(), runID, category)
	return out
}

func (m *memAudit) snapshot() []*audit.Entry {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]*audit.Entry(nil), m.entries...)
}

// dedupAudit implements the atomic capability with a scripted result.
type dedupAudit struct {
	memAudit
	dedupErr   error
	dedupCalls int
}

func (d *dedupAudit) AppendChainedDeduped(ctx context.Context, p audit.ChainAppendParams, _ audit.DedupeSpec) (*audit.Entry, error) {
	d.dedupCalls++
	if d.dedupErr != nil {
		return nil, d.dedupErr
	}
	return d.AppendChained(ctx, p)
}

func payloadWith(t *testing.T, key, val string) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(map[string]string{key: val})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// TestAppendDeduped_SecondAppendIsNoOp is control (6) on the fallback leg:
// the second append with the same key commits nothing, read from the
// COMMITTED rows. Counterfactual: a plain AppendChained → 2 rows.
func TestAppendDeduped_SecondAppendIsNoOp(t *testing.T) {
	store := &memAudit{}
	runID := uuid.New()
	spec := audit.DedupeSpec{PayloadKey: "reverting_commit_sha", PayloadValue: "abc"}
	p := systemChainParams(runID, CategoryRunMergeReverted, time.Now(), payloadWith(t, "reverting_commit_sha", "abc"))
	for i, want := range []bool{true, false} {
		got, err := appendDeduped(context.Background(), store, p, spec)
		if err != nil || got != want {
			t.Fatalf("append %d = (%v, %v), want (%v, nil)", i+1, got, err, want)
		}
	}
	if n := len(store.rows(runID, CategoryRunMergeReverted)); n != 1 {
		t.Fatalf("committed rows = %d, want 1", n)
	}
	// A different key value is not a duplicate.
	p2 := systemChainParams(runID, CategoryRunMergeReverted, time.Now(), payloadWith(t, "reverting_commit_sha", "def"))
	if got, err := appendDeduped(context.Background(), store, p2,
		audit.DedupeSpec{PayloadKey: "reverting_commit_sha", PayloadValue: "def"}); err != nil || !got {
		t.Fatalf("distinct key append = (%v, %v), want (true, nil)", got, err)
	}
}

func TestAppendDeduped_StageScopedSpecIgnoresOtherStages(t *testing.T) {
	store := &memAudit{}
	runID, stageA, stageB := uuid.New(), uuid.New(), uuid.New()
	p := systemChainParams(runID, CategoryRunMergeReverted, time.Now(), payloadWith(t, "k", "v"))
	p.StageID = &stageA
	if _, err := appendDeduped(context.Background(), store, p, audit.DedupeSpec{StageID: &stageA, PayloadKey: "k", PayloadValue: "v"}); err != nil {
		t.Fatal(err)
	}
	p.StageID = &stageB
	got, err := appendDeduped(context.Background(), store, p, audit.DedupeSpec{StageID: &stageB, PayloadKey: "k", PayloadValue: "v"})
	if err != nil || !got {
		t.Fatalf("other-stage append = (%v, %v), want (true, nil)", got, err)
	}
}

func TestAppendDeduped_FallbackListError_FailsClosed(t *testing.T) {
	store := &memAudit{listErr: errors.New("db down")}
	p := systemChainParams(uuid.New(), CategoryRunMergeReverted, time.Now(), payloadWith(t, "k", "v"))
	got, err := appendDeduped(context.Background(), store, p, audit.DedupeSpec{PayloadKey: "k", PayloadValue: "v"})
	if err == nil || got {
		t.Fatalf("got (%v, %v), want (false, error)", got, err)
	}
	if store.appends != 0 {
		t.Fatalf("appends = %d, want 0 (a list error must not append)", store.appends)
	}
}

func TestAppendDeduped_CapabilityPath(t *testing.T) {
	p := systemChainParams(uuid.New(), CategoryRunMergeReverted, time.Now(), payloadWith(t, "k", "v"))
	spec := audit.DedupeSpec{PayloadKey: "k", PayloadValue: "v"}

	ok := &dedupAudit{}
	if got, err := appendDeduped(context.Background(), ok, p, spec); err != nil || !got || ok.dedupCalls != 1 {
		t.Fatalf("capable append = (%v, %v) calls=%d, want (true, nil) via AppendChainedDeduped", got, err, ok.dedupCalls)
	}

	dup := &dedupAudit{dedupErr: &audit.DedupedDuplicateError{Existing: &audit.Entry{Sequence: 7}}}
	if got, err := appendDeduped(context.Background(), dup, p, spec); err != nil || got {
		t.Fatalf("duplicate = (%v, %v), want (false, nil)", got, err)
	}
	if dup.appends != 0 {
		t.Fatalf("duplicate path appended %d rows, want 0", dup.appends)
	}

	boom := &dedupAudit{dedupErr: errors.New("tx failed")}
	if got, err := appendDeduped(context.Background(), boom, p, spec); err == nil || got {
		t.Fatalf("error = (%v, %v), want (false, error)", got, err)
	}
}

func TestSystemChainParams_SystemActor(t *testing.T) {
	p := systemChainParams(uuid.New(), CategoryRunMergeCIObserved, time.Date(2026, 1, 2, 3, 4, 5, 0, time.FixedZone("x", 3600)), nil)
	if p.ActorKind == nil || *p.ActorKind != audit.ActorSystem || p.ActorSubject == nil || *p.ActorSubject != ActorSubject {
		t.Fatalf("actor = %v/%v, want system/%s", p.ActorKind, p.ActorSubject, ActorSubject)
	}
	if p.Timestamp.Location() != time.UTC {
		t.Errorf("timestamp not UTC: %v", p.Timestamp)
	}
}

type timeoutErr struct{}

func (timeoutErr) Error() string   { return "i/o timeout" }
func (timeoutErr) Timeout() bool   { return true }
func (timeoutErr) Temporary() bool { return true }

var _ net.Error = timeoutErr{}

func TestIsTransient(t *testing.T) {
	live := context.Background()
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	cases := []struct {
		name string
		ctx  context.Context
		err  error
		want bool
	}{
		{"nil", live, nil, false},
		{"5xx status", live, fmt.Errorf("githubclient: get pr: 503: unavailable"), true},
		{"wrapped 5xx", live, fmt.Errorf("get pr 42: %w", fmt.Errorf("githubclient: get pr: 500: oops")), true},
		{"429 status", live, fmt.Errorf("githubclient: get pr: 429: slow down"), false},
		{"409 status", live, fmt.Errorf("githubclient: get pr: 409: conflict"), false},
		{"not found", live, fmt.Errorf("%w: get pr", forge.ErrNotFound), false},
		{"forbidden", live, fmt.Errorf("%w: get pr", forge.ErrForbidden), false},
		{"validation", live, fmt.Errorf("%w: get pr", forge.ErrValidation), false},
		{"not installed", live, forge.ErrNotInstalled, false},
		{"url error", live, &url.Error{Op: "Get", URL: "http://x", Err: errors.New("connection refused")}, true},
		{"net error", live, fmt.Errorf("wrap: %w", timeoutErr{}), true},
		{"caller ctx done", cancelled, fmt.Errorf("githubclient: get pr: 503: x"), false},
		{"plain error", live, errors.New("decode failed"), false},
	}
	for _, tc := range cases {
		if got := isTransient(tc.ctx, tc.err); got != tc.want {
			t.Errorf("%s: isTransient = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestWithRetry_RetriesTransientThenSucceeds(t *testing.T) {
	calls := 0
	got, err := withRetry(context.Background(), time.Millisecond, func(context.Context) (int, error) {
		calls++
		if calls < 3 {
			return 0, errors.New("githubclient: get pr: 502: bad gateway")
		}
		return 7, nil
	})
	if err != nil || got != 7 || calls != 3 {
		t.Fatalf("got (%d, %v) after %d calls, want (7, nil) after 3", got, err, calls)
	}
}

func TestWithRetry_BoundedAtThreeAttempts(t *testing.T) {
	calls := 0
	_, err := withRetry(context.Background(), time.Millisecond, func(context.Context) (int, error) {
		calls++
		return 0, errors.New("githubclient: get pr: 503: down")
	})
	if err == nil || calls != forgeAttempts {
		t.Fatalf("calls = %d err = %v, want %d attempts and an error", calls, err, forgeAttempts)
	}
}

func TestWithRetry_NonTransientNotRetried(t *testing.T) {
	calls := 0
	_, err := withRetry(context.Background(), time.Millisecond, func(context.Context) (int, error) {
		calls++
		return 0, fmt.Errorf("%w: get pr", forge.ErrNotFound)
	})
	if err == nil || calls != 1 {
		t.Fatalf("calls = %d, want 1 (404 is terminal)", calls)
	}
}

func TestWithRetry_ContextEndsDuringBackoff(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	calls := 0
	_, err := withRetry(ctx, time.Hour, func(context.Context) (int, error) {
		calls++
		cancel()
		return 0, &url.Error{Op: "Get", URL: "http://x", Err: errors.New("reset")}
	})
	if err == nil || calls != 1 {
		t.Fatalf("calls = %d err = %v, want 1 call and the last error", calls, err)
	}
}

// TestWithRetry_RealClientStatusFormat pins the coupling isTransient relies
// on: a REAL githubclient against a server answering 503, 503, 200 succeeds on
// the third attempt, and a 404 is never retried.
func TestWithRetry_RealClientStatusFormat(t *testing.T) {
	var hits atomic.Int32
	var notFound atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := hits.Add(1)
		if notFound.Load() {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if n < 3 {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte("unavailable"))
			return
		}
		_, _ = w.Write([]byte(`{"node_id":"PR_1","merged":true,"merge_commit_sha":"m1"}`))
	}))
	defer srv.Close()
	c := githubclient.New(staticTokens{})
	c.BaseURL = srv.URL
	c.HTTP = &http.Client{Timeout: 5 * time.Second}
	scope := forge.FromGitHubInstallationID(9)
	repo := forge.RepoRef{Owner: "acme", Name: "widgets"}
	pr, err := withRetry(context.Background(), time.Millisecond, func(ctx context.Context) (*forge.PullRequest, error) {
		return c.GetPullRequest(ctx, scope, repo, 42)
	})
	if err != nil || pr == nil || pr.MergeCommitSHA != "m1" || hits.Load() != 3 {
		t.Fatalf("got (%+v, %v) after %d hits, want success on hit 3", pr, err, hits.Load())
	}
	hits.Store(0)
	notFound.Store(true)
	if _, err := withRetry(context.Background(), time.Millisecond, func(ctx context.Context) (*forge.PullRequest, error) {
		return c.GetPullRequest(ctx, scope, repo, 42)
	}); err == nil || hits.Load() != 1 {
		t.Fatalf("404: hits = %d err = %v, want 1 hit and an error", hits.Load(), err)
	}
}

type staticTokens struct{}

func (staticTokens) Token(context.Context, int64) (string, error) { return "ghs_test", nil }
