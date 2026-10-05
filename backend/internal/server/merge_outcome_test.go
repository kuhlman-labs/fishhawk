package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/githubclient"
	"github.com/kuhlman-labs/fishhawk/backend/internal/mergeoutcome"
	"github.com/kuhlman-labs/fishhawk/backend/internal/pgtest"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
	"github.com/kuhlman-labs/fishhawk/backend/internal/timescale"
	"github.com/kuhlman-labs/fishhawk/backend/internal/webhook"
)

// merge_outcome_test.go is the CROSS-BOUNDARY test for E82.2 / #3779's push
// route: a signed `push` delivery through s.Handler() → mergeoutcome's
// revert observer → a REAL *githubclient.Client against an httptest GitHub
// mux → pgtest-backed run lookup → the committed run_merge_reverted row.

const (
	pushMergeSHA  = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	pushRevertSHA = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	pushPRURL     = "https://github.com/acme/widgets/pull/42"
)

type pushFixture struct {
	s       *Server
	audit   audit.Repository
	runID   uuid.UUID
	hits    *atomic.Int32
	release chan struct{} // non-nil: the commit→pulls lookup blocks until closed
}

func newPushFixture(t *testing.T, block bool) *pushFixture {
	t.Helper()
	ctx := context.Background()
	pool := pgtest.NewPool(t)
	runRepo := run.NewPostgresRepository(pool)
	auditRepo := audit.NewPostgresRepository(pool)
	r, err := runRepo.CreateRun(ctx, run.CreateRunParams{
		Repo: "acme/widgets", WorkflowID: "feature_change", WorkflowSHA: "abc",
		TriggerSource: run.TriggerCLI, WorkflowSpec: []byte(autoDriveAcceptanceSpecYAML),
	})
	if err != nil {
		t.Fatalf("create run: %v", err)
	}
	if _, err := runRepo.SetRunPullRequestURL(ctx, r.ID, pushPRURL); err != nil {
		t.Fatalf("set pr url: %v", err)
	}

	f := &pushFixture{audit: auditRepo, runID: r.ID, hits: &atomic.Int32{}}
	if block {
		f.release = make(chan struct{})
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/repos/acme/widgets/commits/"+pushMergeSHA+"/pulls", func(w http.ResponseWriter, _ *http.Request) {
		f.hits.Add(1)
		if f.release != nil {
			<-f.release
		}
		_, _ = fmt.Fprintf(w, `[{"number":42,"html_url":%q,"merged_at":"2026-09-01T12:00:00Z"}]`, pushPRURL)
	})
	mux.HandleFunc("/repos/acme/widgets/pulls/42", func(w http.ResponseWriter, _ *http.Request) {
		f.hits.Add(1)
		_, _ = fmt.Fprintf(w, `{"node_id":"PR_42","state":"closed","merged":true,"merge_commit_sha":%q,"merged_at":"2026-09-01T12:00:00Z"}`, pushMergeSHA)
	})
	mux.HandleFunc("/repos/acme/widgets/commits/"+pushMergeSHA, func(w http.ResponseWriter, _ *http.Request) {
		f.hits.Add(1)
		_, _ = w.Write([]byte(`{"sha":"` + pushMergeSHA + `","files":[{"filename":"a.go","status":"modified","additions":10,"deletions":2}]}`))
	})
	mux.HandleFunc("/repos/acme/widgets/commits/"+pushRevertSHA, func(w http.ResponseWriter, _ *http.Request) {
		f.hits.Add(1)
		_, _ = w.Write([]byte(`{"sha":"` + pushRevertSHA + `","files":[{"filename":"a.go","status":"modified","additions":2,"deletions":10}]}`))
	})
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		f.hits.Add(1)
		t.Errorf("unexpected GitHub request %s %s", r.Method, r.URL.Path)
		w.WriteHeader(http.StatusNotFound)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	client := githubclient.New(seamTokenProvider{})
	client.BaseURL = srv.URL
	client.HTTP = &http.Client{Timeout: 10 * time.Second}

	f.s = New(Config{
		Addr:                "127.0.0.1:0",
		GitHubWebhookSecret: []byte(testSecret),
		WebhookDeliveries:   webhook.NewMemoryStore(0),
		RunRepo:             runRepo,
		AuditRepo:           auditRepo,
		GitHub:              client,
	})
	return f
}

func pushBody(t *testing.T, ref, senderType string, commits ...map[string]string) []byte {
	t.Helper()
	b, err := json.Marshal(map[string]any{
		"ref": ref, "after": pushRevertSHA, "deleted": false,
		"repository": map[string]any{
			"full_name": "acme/widgets", "html_url": "https://github.com/acme/widgets", "default_branch": "main",
		},
		"installation": map[string]any{"id": 9},
		"sender":       map[string]any{"login": "fishhawk-dev[bot]", "type": senderType},
		"commits":      commits,
	})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func revertCommit() map[string]string {
	return map[string]string{
		"id":      pushRevertSHA,
		"message": "Revert \"feat: add widgets (#42)\"\n\nThis reverts commit " + pushMergeSHA + ".\n",
	}
}

func postPush(t *testing.T, s *Server, ctx context.Context, delivery string, body []byte) int {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/webhooks/github", bytes.NewReader(body)).WithContext(ctx)
	req.Header.Set("X-GitHub-Event", "push")
	req.Header.Set("X-GitHub-Delivery", delivery)
	req.Header.Set("X-Hub-Signature-256", sign(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)
	return w.Code
}

func (f *pushFixture) committedRows(t *testing.T) []*audit.Entry {
	t.Helper()
	rows, err := f.audit.ListForRunByCategory(context.Background(), f.runID, mergeoutcome.CategoryRunMergeReverted)
	if err != nil {
		t.Fatalf("list rows: %v", err)
	}
	return rows
}

// TestWebhookPush_RevertOfRunMerge_RecordsRunMergeReverted is the
// cross-boundary done-means: deleting the `push` route in webhook.go reddens
// it.
func TestWebhookPush_RevertOfRunMerge_RecordsRunMergeReverted(t *testing.T) {
	f := newPushFixture(t, false)
	if code := postPush(t, f.s, context.Background(), "push-1", pushBody(t, "refs/heads/main", "User", revertCommit())); code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", code)
	}
	pushObservers.Wait()
	rows := f.committedRows(t)
	if len(rows) != 1 {
		t.Fatalf("committed run_merge_reverted rows = %d, want 1", len(rows))
	}
	var p map[string]any
	if err := json.Unmarshal(rows[0].Payload, &p); err != nil {
		t.Fatal(err)
	}
	for k, want := range map[string]any{
		"merge_commit_sha": pushMergeSHA, "reverting_commit_sha": pushRevertSHA,
		"detection": "revert_trailer", "attestation": "inverse_diff", "delivery_id": "push-1",
	} {
		if p[k] != want {
			t.Errorf("payload[%s] = %v, want %v", k, p[k], want)
		}
	}
	if rows[0].ActorSubject == nil || *rows[0].ActorSubject != mergeoutcome.ActorSubject {
		t.Errorf("actor subject = %v, want %s", rows[0].ActorSubject, mergeoutcome.ActorSubject)
	}
}

func TestWebhookPush_UnrelatedCommit_RecordsNothing(t *testing.T) {
	f := newPushFixture(t, false)
	body := pushBody(t, "refs/heads/main", "User", map[string]string{"id": pushRevertSHA, "message": "feat: add widgets (#42)"})
	if code := postPush(t, f.s, context.Background(), "push-2", body); code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", code)
	}
	pushObservers.Wait()
	if n := len(f.committedRows(t)); n != 0 || f.hits.Load() != 0 {
		t.Fatalf("rows = %d GitHub hits = %d, want 0/0", n, f.hits.Load())
	}
}

func TestWebhookPush_NonDefaultBranch_RecordsNothing(t *testing.T) {
	f := newPushFixture(t, false)
	if code := postPush(t, f.s, context.Background(), "push-3", pushBody(t, "refs/heads/feature", "User", revertCommit())); code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", code)
	}
	pushObservers.Wait()
	if n := len(f.committedRows(t)); n != 0 || f.hits.Load() != 0 {
		t.Fatalf("rows = %d GitHub hits = %d, want 0/0", n, f.hits.Load())
	}
}

// TestWebhookPush_ReplayUnderNewDeliveryID_RecordsOnce pins the AUDIT-layer
// dedup: a second delivery with a DIFFERENT X-GitHub-Delivery passes the
// delivery store and reaches the observer, so only the deduped append stops a
// second row. (A same-GUID redelivery never reaches the observer at all —
// TestWebhookPush_SameDeliveryID_NeverReachesObserver.)
func TestWebhookPush_ReplayUnderNewDeliveryID_RecordsOnce(t *testing.T) {
	f := newPushFixture(t, false)
	body := pushBody(t, "refs/heads/main", "User", revertCommit())
	for _, id := range []string{"push-4a", "push-4b"} {
		if code := postPush(t, f.s, context.Background(), id, body); code != http.StatusAccepted {
			t.Fatalf("%s: status = %d, want 202", id, code)
		}
		pushObservers.Wait()
	}
	if n := len(f.committedRows(t)); n != 1 {
		t.Fatalf("committed rows = %d, want 1", n)
	}
}

// TestWebhookPush_SameDeliveryID_NeverReachesObserver pins the fact the
// README's loss residual rests on: handleWebhook's delivery-dedup Mark runs
// BEFORE routing, so a same-GUID redelivery is acknowledged and dropped — the
// observer makes no forge call for it.
func TestWebhookPush_SameDeliveryID_NeverReachesObserver(t *testing.T) {
	f := newPushFixture(t, false)
	body := pushBody(t, "refs/heads/main", "User", revertCommit())
	postPush(t, f.s, context.Background(), "push-5", body)
	pushObservers.Wait()
	first := f.hits.Load()
	if first == 0 {
		t.Fatal("first delivery made no GitHub call; fixture broken")
	}
	if code := postPush(t, f.s, context.Background(), "push-5", body); code != http.StatusAccepted {
		t.Fatalf("redelivery status = %d, want 202", code)
	}
	pushObservers.Wait()
	if got := f.hits.Load(); got != first {
		t.Fatalf("GitHub hits after same-GUID redelivery = %d, want %d (unchanged)", got, first)
	}
}

// TestWebhookPush_BotSender_StillObserved: a revert pushed by a Bot (the
// GitHub App merging a revert PR) is recorded — the push route sits outside
// the dispatcher's bot-sender skip.
func TestWebhookPush_BotSender_StillObserved(t *testing.T) {
	f := newPushFixture(t, false)
	if code := postPush(t, f.s, context.Background(), "push-6", pushBody(t, "refs/heads/main", "Bot", revertCommit())); code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", code)
	}
	pushObservers.Wait()
	if n := len(f.committedRows(t)); n != 1 {
		t.Fatalf("committed rows = %d, want 1", n)
	}
}

// TestWebhookPush_ObserverDetached_DoesNotDelay202 is operator condition 2:
// with the forge lookup BLOCKED, the delivery still returns 202, and after the
// request context is cancelled the released observation still records.
// Counterfactuals: run ObservePush inline → the POST blocks past the bound;
// drop context.WithoutCancel → the cancelled request kills the observation.
func TestWebhookPush_ObserverDetached_DoesNotDelay202(t *testing.T) {
	f := newPushFixture(t, true)
	reqCtx, cancelReq := context.WithCancel(context.Background())
	done := make(chan int, 1)
	go func() {
		done <- postPush(t, f.s, reqCtx, "push-7", pushBody(t, "refs/heads/main", "User", revertCommit()))
	}()
	select {
	case code := <-done:
		if code != http.StatusAccepted {
			t.Fatalf("status = %d, want 202", code)
		}
	case <-time.After(timescale.D(5 * time.Second)):
		close(f.release)
		t.Fatal("POST /webhooks/github blocked on the push observer's forge call")
	}
	cancelReq()
	close(f.release)
	pushObservers.Wait()
	if n := len(f.committedRows(t)); n != 1 {
		t.Fatalf("committed rows = %d, want 1 (the observation must outlive the request)", n)
	}
}

func TestDetachedPushContext_BoundedAndUncancelled(t *testing.T) {
	parent, cancel := context.WithCancel(context.Background())
	ctx, stop := detachedPushContext(parent)
	defer stop()
	cancel()
	if ctx.Err() != nil {
		t.Fatalf("detached ctx ended with its parent: %v", ctx.Err())
	}
	dl, ok := ctx.Deadline()
	if !ok || time.Until(dl) > pushObserveTimeout || time.Until(dl) <= 0 {
		t.Fatalf("deadline = %v (ok=%v), want within %v", dl, ok, pushObserveTimeout)
	}
	if pushObserveTimeout != 60*time.Second {
		t.Fatalf("pushObserveTimeout = %v, want 60s (operator condition 2)", pushObserveTimeout)
	}
}

// TestRunDetachedPushObservation_PanicRecovered: a panicking observer is
// recovered, logged and dropped, and its slot is released — the test process
// survives. Counterfactuals: delete the recover → the detached goroutine's
// panic crashes the test binary; delete the slot release → the slot stays held.
func TestRunDetachedPushObservation_PanicRecovered(t *testing.T) {
	buf := &syncBuffer{}
	slots := make(chan struct{}, 1)
	started := runDetachedPushObservation(context.Background(), slog.New(slog.NewTextHandler(buf, nil)), slots, "push-panic",
		func(context.Context) { panic("observer boom") })
	if !started {
		t.Fatal("observation skipped with a free slot")
	}
	pushObservers.Wait()
	if out := buf.String(); !strings.Contains(out, "push observation panicked; dropped") || !strings.Contains(out, "observer boom") {
		t.Fatalf("log = %q, want the recovered panic logged", out)
	}
	if len(slots) != 0 {
		t.Fatalf("slots held = %d after the panic, want 0", len(slots))
	}
}

// TestRunDetachedPushObservation_SaturatedSkipsWithWarn: with every slot held a
// delivery is skipped with a WARN, never queued or run. Counterfactual: drop
// the slot acquisition → the observation runs past the bound.
func TestRunDetachedPushObservation_SaturatedSkipsWithWarn(t *testing.T) {
	buf := &syncBuffer{}
	slots := make(chan struct{}, 1)
	slots <- struct{}{}
	var ran atomic.Bool
	started := runDetachedPushObservation(context.Background(), slog.New(slog.NewTextHandler(buf, nil)), slots, "push-full",
		func(context.Context) { ran.Store(true) })
	pushObservers.Wait()
	if started || ran.Load() {
		t.Fatalf("started = %v ran = %v with every slot held, want a skip", started, ran.Load())
	}
	if out := buf.String(); !strings.Contains(out, "level=WARN") || !strings.Contains(out, "concurrent observations saturated") {
		t.Fatalf("log = %q, want the saturation WARN", out)
	}
	if cap(pushObserveSlots) != maxConcurrentPushObservations {
		t.Fatalf("process-wide slots = %d, want %d", cap(pushObserveSlots), maxConcurrentPushObservations)
	}
}

// TestObserveDefaultBranchPush_UnwiredForge_NoObservation: with no GitHub
// client the route returns before building the observer. Counterfactual:
// delete the nil check → the nil *githubclient.Client is wrapped in the
// interface and the observer panics on first use.
func TestObserveDefaultBranchPush_UnwiredForge_NoObservation(t *testing.T) {
	s := New(Config{
		Addr:                "127.0.0.1:0",
		GitHubWebhookSecret: []byte(testSecret),
		WebhookDeliveries:   webhook.NewMemoryStore(0),
		RunRepo:             run.BaseFake{},
		AuditRepo:           audit.BaseFake{},
	})
	if code := postPush(t, s, context.Background(), "push-8", pushBody(t, "refs/heads/main", "User", revertCommit())); code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", code)
	}
	pushObservers.Wait()
}

func TestWebhookPush_UndecodablePayload_Skipped(t *testing.T) {
	f := newPushFixture(t, false)
	if code := postPush(t, f.s, context.Background(), "push-9", []byte(`{"ref":5}`)); code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", code)
	}
	pushObservers.Wait()
	if f.hits.Load() != 0 || len(f.committedRows(t)) != 0 {
		t.Fatalf("hits = %d rows = %d, want 0/0", f.hits.Load(), len(f.committedRows(t)))
	}
}
