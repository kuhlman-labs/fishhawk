package gitops

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

// stubResp is one canned HTTP response.
type stubResp struct {
	code    int
	body    string
	headers map[string]string
}

// stubAPI is a method-aware fake for /repos/{o}/{r}/pulls. GET (list-by-head)
// and POST (create) responses are served from separate ordered slices; once a
// slice is exhausted the last entry repeats (so a "sustained 500" is one entry).
type stubAPI struct {
	listResponses   []stubResp
	createResponses []stubResp

	listCalls   int
	createCalls int

	lastCreateBody map[string]any
	lastCreatePath string
	gotAuth        string
}

func pick(resps []stubResp, i int) stubResp {
	if len(resps) == 0 {
		return stubResp{code: http.StatusInternalServerError}
	}
	if i >= len(resps) {
		return resps[len(resps)-1]
	}
	return resps[i]
}

func (s *stubAPI) write(w http.ResponseWriter, resp stubResp) {
	for k, v := range resp.headers {
		w.Header().Set(k, v)
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(resp.code)
	_, _ = io.WriteString(w, resp.body)
}

func newStubAPI(t *testing.T) (*stubAPI, *httptest.Server) {
	t.Helper()
	stub := &stubAPI{
		// Defaults: no existing PR, and a successful create.
		listResponses:   []stubResp{{code: http.StatusOK, body: `[]`}},
		createResponses: []stubResp{{code: http.StatusCreated, body: `{"number":42,"html_url":"https://github.com/owner/repo/pull/42"}`}},
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		stub.gotAuth = r.Header.Get("Authorization")
		switch r.Method {
		case http.MethodGet:
			resp := pick(stub.listResponses, stub.listCalls)
			stub.listCalls++
			stub.write(w, resp)
		case http.MethodPost:
			raw, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(raw, &stub.lastCreateBody)
			stub.lastCreatePath = r.URL.Path
			resp := pick(stub.createResponses, stub.createCalls)
			stub.createCalls++
			stub.write(w, resp)
		default:
			w.WriteHeader(http.StatusMethodNotAllowed)
		}
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return stub, srv
}

// noSleep is an injected sleeper that records nothing and never blocks.
func noSleep(context.Context, time.Duration) error { return nil }

func newRetryClient(srv *httptest.Server) *OpenPRClient {
	return &OpenPRClient{
		HTTP:         &http.Client{Timeout: time.Second},
		APIBase:      srv.URL,
		Token:        "ghs_xyz",
		MaxRetries:   3,
		RetryBackoff: time.Microsecond,
		sleeper:      noSleep,
	}
}

func TestOpenPR_HappyPath(t *testing.T) {
	stub, srv := newStubAPI(t)
	c := newRetryClient(srv)

	got, err := c.OpenPR(context.Background(), OpenPRArgs{
		Owner: "owner", Repo: "repo",
		Head: "fishhawk/branch", Base: "main",
		Title: "Add a thing", Body: "context",
	})
	if err != nil {
		t.Fatalf("OpenPR: %v", err)
	}
	if got.PRNumber != 42 || got.PRURL == "" {
		t.Errorf("got = %+v", got)
	}
	if stub.createCalls != 1 {
		t.Errorf("createCalls = %d, want 1", stub.createCalls)
	}
	if stub.lastCreatePath != "/repos/owner/repo/pulls" {
		t.Errorf("create path = %q", stub.lastCreatePath)
	}
	if stub.gotAuth != "Bearer ghs_xyz" {
		t.Errorf("Authorization = %q", stub.gotAuth)
	}
	for _, want := range []string{"title", "head", "base"} {
		if _, ok := stub.lastCreateBody[want]; !ok {
			t.Errorf("body missing %q: %+v", want, stub.lastCreateBody)
		}
	}
}

// (a) An isolated 500 on create is retried and the second attempt succeeds.
func TestOpenPR_RetriesTransient500ThenSucceeds(t *testing.T) {
	stub, srv := newStubAPI(t)
	stub.createResponses = []stubResp{
		{code: http.StatusInternalServerError, body: `{"message":"server error"}`},
		{code: http.StatusCreated, body: `{"number":42,"html_url":"https://x/42"}`},
	}
	c := newRetryClient(srv)

	got, err := c.OpenPR(context.Background(), OpenPRArgs{
		Owner: "o", Repo: "r", Head: "h", Base: "b", Title: "t",
	})
	if err != nil {
		t.Fatalf("OpenPR: %v", err)
	}
	if got.PRNumber != 42 {
		t.Errorf("PRNumber = %d, want 42", got.PRNumber)
	}
	if stub.createCalls != 2 {
		t.Errorf("createCalls = %d, want 2 (one retry)", stub.createCalls)
	}
}

// (b) An existing open PR for head is adopted with ZERO create POSTs.
func TestOpenPR_AdoptsExistingByHead(t *testing.T) {
	stub, srv := newStubAPI(t)
	stub.listResponses = []stubResp{{code: http.StatusOK, body: `[{"number":7,"html_url":"https://x/7"}]`}}
	c := newRetryClient(srv)

	got, err := c.OpenPR(context.Background(), OpenPRArgs{
		Owner: "o", Repo: "r", Head: "h", Base: "b", Title: "t",
	})
	if err != nil {
		t.Fatalf("OpenPR: %v", err)
	}
	if got.PRNumber != 7 {
		t.Errorf("PRNumber = %d, want 7 (adopted)", got.PRNumber)
	}
	if stub.createCalls != 0 {
		t.Errorf("createCalls = %d, want 0 (adopted, no create)", stub.createCalls)
	}
	if stub.listCalls != 1 {
		t.Errorf("listCalls = %d, want 1", stub.listCalls)
	}
}

// (c) A 422-duplicate create race falls back to list-and-adopt.
func TestOpenPR_DuplicateRaceAdopts(t *testing.T) {
	stub, srv := newStubAPI(t)
	stub.listResponses = []stubResp{
		{code: http.StatusOK, body: `[]`},                                      // first list: none yet
		{code: http.StatusOK, body: `[{"number":9,"html_url":"https://x/9"}]`}, // fallback list: winner's PR
	}
	stub.createResponses = []stubResp{
		{code: http.StatusUnprocessableEntity, body: `{"message":"A pull request already exists for h."}`},
	}
	c := newRetryClient(srv)

	got, err := c.OpenPR(context.Background(), OpenPRArgs{
		Owner: "o", Repo: "r", Head: "h", Base: "b", Title: "t",
	})
	if err != nil {
		t.Fatalf("OpenPR: %v", err)
	}
	if got.PRNumber != 9 {
		t.Errorf("PRNumber = %d, want 9 (adopted after race)", got.PRNumber)
	}
	if stub.createCalls != 1 {
		t.Errorf("createCalls = %d, want 1", stub.createCalls)
	}
	if stub.listCalls != 2 {
		t.Errorf("listCalls = %d, want 2 (initial + fallback)", stub.listCalls)
	}
}

// (d) A non-retryable 404 on create returns promptly with no retry.
func TestOpenPR_NonRetryable404ReturnsPromptly(t *testing.T) {
	stub, srv := newStubAPI(t)
	stub.createResponses = []stubResp{{code: http.StatusNotFound, body: `{"message":"not found"}`}}
	c := newRetryClient(srv)

	_, err := c.OpenPR(context.Background(), OpenPRArgs{
		Owner: "o", Repo: "r", Head: "h", Base: "b", Title: "t",
	})
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "404") {
		t.Errorf("err = %v, want 404", err)
	}
	if stub.createCalls != 1 {
		t.Errorf("createCalls = %d, want 1 (no retry)", stub.createCalls)
	}
}

// (e) A sustained 500 exhausts MaxRetries: createCalls == MaxRetries+1.
func TestOpenPR_SustainedTransientExhaustsRetries(t *testing.T) {
	stub, srv := newStubAPI(t)
	stub.createResponses = []stubResp{{code: http.StatusInternalServerError, body: `{"message":"down"}`}}
	c := newRetryClient(srv)
	c.MaxRetries = 2

	_, err := c.OpenPR(context.Background(), OpenPRArgs{
		Owner: "o", Repo: "r", Head: "h", Base: "b", Title: "t",
	})
	if err == nil {
		t.Fatal("expected error after exhausting retries")
	}
	if !strings.Contains(err.Error(), "500") {
		t.Errorf("err = %v, want 500", err)
	}
	if stub.createCalls != 3 {
		t.Errorf("createCalls = %d, want 3 (MaxRetries+1)", stub.createCalls)
	}
}

// (f) An integer Retry-After header is honored as the exact delay VALUE (capped).
func TestOpenPR_HonorsRetryAfterDelayValue(t *testing.T) {
	stub, srv := newStubAPI(t)
	stub.createResponses = []stubResp{
		{code: http.StatusInternalServerError, body: `{}`, headers: map[string]string{"Retry-After": "1"}},
		{code: http.StatusCreated, body: `{"number":42,"html_url":"https://x/42"}`},
	}
	var delays []time.Duration
	c := newRetryClient(srv)
	c.sleeper = func(_ context.Context, d time.Duration) error {
		delays = append(delays, d)
		return nil
	}

	_, err := c.OpenPR(context.Background(), OpenPRArgs{
		Owner: "o", Repo: "r", Head: "h", Base: "b", Title: "t",
	})
	if err != nil {
		t.Fatalf("OpenPR: %v", err)
	}
	if len(delays) != 1 {
		t.Fatalf("delays = %v, want exactly one honored delay", delays)
	}
	if delays[0] != time.Second {
		t.Errorf("delay = %v, want 1s (honored Retry-After)", delays[0])
	}
}

// A header-gated 403 (rate limit) IS retried; a plain permission 403 is NOT.
func TestOpenPR_403RateLimitedRetried_PlainForbiddenNot(t *testing.T) {
	t.Run("rate-limited 403 is retried", func(t *testing.T) {
		stub, srv := newStubAPI(t)
		stub.createResponses = []stubResp{
			{code: http.StatusForbidden, body: `{"message":"rate limited"}`, headers: map[string]string{"Retry-After": "0"}},
			{code: http.StatusCreated, body: `{"number":42,"html_url":"https://x/42"}`},
		}
		c := newRetryClient(srv)
		if _, err := c.OpenPR(context.Background(), OpenPRArgs{Owner: "o", Repo: "r", Head: "h", Base: "b", Title: "t"}); err != nil {
			t.Fatalf("OpenPR: %v", err)
		}
		if stub.createCalls != 2 {
			t.Errorf("createCalls = %d, want 2 (retried)", stub.createCalls)
		}
	})

	t.Run("plain 403 is not retried", func(t *testing.T) {
		stub, srv := newStubAPI(t)
		stub.createResponses = []stubResp{{code: http.StatusForbidden, body: `{"message":"forbidden"}`}}
		c := newRetryClient(srv)
		if _, err := c.OpenPR(context.Background(), OpenPRArgs{Owner: "o", Repo: "r", Head: "h", Base: "b", Title: "t"}); err == nil {
			t.Fatal("expected error")
		}
		if stub.createCalls != 1 {
			t.Errorf("createCalls = %d, want 1 (no retry)", stub.createCalls)
		}
	})
}

// A server delay exceeding the remaining context budget gives up promptly
// rather than sleeping past the deadline.
func TestOpenPR_RetryAfterExceedingBudgetGivesUp(t *testing.T) {
	stub, srv := newStubAPI(t)
	stub.createResponses = []stubResp{{code: http.StatusInternalServerError, body: `{}`, headers: map[string]string{"Retry-After": "10"}}}
	// A real-future deadline (so the request actually fires) with an injected
	// clock parked 9s in, leaving 1s of budget against a 10s server delay.
	realNow := time.Now()
	sleeperCalled := false
	c := newRetryClient(srv)
	c.now = func() time.Time { return realNow.Add(9 * time.Second) }
	c.sleeper = func(context.Context, time.Duration) error { sleeperCalled = true; return nil }

	ctx, cancel := context.WithDeadline(context.Background(), realNow.Add(10*time.Second))
	defer cancel()

	_, err := c.OpenPR(ctx, OpenPRArgs{Owner: "o", Repo: "r", Head: "h", Base: "b", Title: "t"})
	if err == nil {
		t.Fatal("expected error")
	}
	if stub.createCalls != 1 {
		t.Errorf("createCalls = %d, want 1 (no retry: budget exceeded)", stub.createCalls)
	}
	if sleeperCalled {
		t.Error("sleeper was called; expected prompt give-up without sleeping")
	}
}

// A server delay EXACTLY equal to the remaining context budget gives up rather
// than sleeping the whole deadline only to retry with none left (#2167 boundary:
// delay >= remaining, not just delay > remaining).
func TestOpenPR_RetryAfterEqualsBudgetGivesUp(t *testing.T) {
	stub, srv := newStubAPI(t)
	stub.createResponses = []stubResp{{code: http.StatusInternalServerError, body: `{}`, headers: map[string]string{"Retry-After": "1"}}}
	// Injected clock parked 9s in against a 10s deadline: remaining == 1s,
	// exactly the honored 1s Retry-After delay.
	realNow := time.Now()
	sleeperCalled := false
	c := newRetryClient(srv)
	c.now = func() time.Time { return realNow.Add(9 * time.Second) }
	c.sleeper = func(context.Context, time.Duration) error { sleeperCalled = true; return nil }

	ctx, cancel := context.WithDeadline(context.Background(), realNow.Add(10*time.Second))
	defer cancel()

	_, err := c.OpenPR(ctx, OpenPRArgs{Owner: "o", Repo: "r", Head: "h", Base: "b", Title: "t"})
	if err == nil {
		t.Fatal("expected error")
	}
	if stub.createCalls != 1 {
		t.Errorf("createCalls = %d, want 1 (delay == budget, no retry)", stub.createCalls)
	}
	if sleeperCalled {
		t.Error("sleeper was called; expected give-up at the budget boundary")
	}
}

// contextSleep returns promptly with ctx.Err() when the context is already
// cancelled, so a retry loop cannot block past caller cancellation.
func TestContextSleep_CancelledReturnsPromptly(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := contextSleep(ctx, time.Hour); err == nil {
		t.Error("expected ctx.Err() on a cancelled context")
	}
}

// contextSleep returns ctx.Err() when the context is cancelled MID-sleep, taking
// the timer-vs-ctx.Done() select race path (not just the already-cancelled path).
func TestContextSleep_CancelledMidSleep(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(10 * time.Millisecond)
		cancel()
	}()
	start := time.Now()
	if err := contextSleep(ctx, time.Hour); err == nil {
		t.Error("expected ctx.Err() on mid-sleep cancellation")
	}
	if elapsed := time.Since(start); elapsed > 30*time.Second {
		t.Errorf("contextSleep blocked %v; expected a prompt return once cancelled", elapsed)
	}
}

func TestOpenPR_RejectsBadInputs(t *testing.T) {
	c := &OpenPRClient{Token: "x"}
	cases := map[string]OpenPRArgs{
		"missing owner": {Repo: "r", Head: "h", Base: "b", Title: "t"},
		"missing repo":  {Owner: "o", Head: "h", Base: "b", Title: "t"},
		"missing head":  {Owner: "o", Repo: "r", Base: "b", Title: "t"},
		"missing base":  {Owner: "o", Repo: "r", Head: "h", Title: "t"},
		"missing title": {Owner: "o", Repo: "r", Head: "h", Base: "b"},
	}
	for name, args := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := c.OpenPR(context.Background(), args)
			if err == nil {
				t.Error("expected error")
			}
		})
	}

	c2 := &OpenPRClient{}
	_, err := c2.OpenPR(context.Background(), OpenPRArgs{
		Owner: "o", Repo: "r", Head: "h", Base: "b", Title: "t",
	})
	if err == nil {
		t.Error("expected token-required error")
	}
}

func TestOpenPR_RejectsMalformedSuccess(t *testing.T) {
	stub, srv := newStubAPI(t)
	stub.createResponses = []stubResp{{code: http.StatusCreated, body: `{"number":0,"html_url":""}`}}
	c := newRetryClient(srv)

	_, err := c.OpenPR(context.Background(), OpenPRArgs{
		Owner: "o", Repo: "r", Head: "h", Base: "b", Title: "t",
	})
	if err == nil {
		t.Fatal("expected error on missing fields")
	}
}

// errTransport always fails the round trip, exercising the httpClient.Do error
// branch of the retry loop.
type errTransport struct{ err error }

func (t errTransport) RoundTrip(*http.Request) (*http.Response, error) { return nil, t.err }

// A transport-level error propagates out of OpenPR (via the first list-by-head).
func TestOpenPR_TransportErrorPropagates(t *testing.T) {
	c := &OpenPRClient{
		HTTP:    &http.Client{Transport: errTransport{err: io.ErrUnexpectedEOF}},
		APIBase: "https://example.invalid",
		Token:   "ghs_xyz",
		sleeper: noSleep,
	}
	_, err := c.OpenPR(context.Background(), OpenPRArgs{Owner: "o", Repo: "r", Head: "h", Base: "b", Title: "t"})
	if err == nil {
		t.Fatal("expected transport error")
	}
}

// A sleeper error (e.g. ctx cancelled mid-sleep) aborts the retry loop.
func TestOpenPR_SleeperErrorAborts(t *testing.T) {
	stub, srv := newStubAPI(t)
	stub.createResponses = []stubResp{{code: http.StatusInternalServerError, body: `{}`}}
	c := newRetryClient(srv)
	c.sleeper = func(context.Context, time.Duration) error { return context.Canceled }
	_, err := c.OpenPR(context.Background(), OpenPRArgs{Owner: "o", Repo: "r", Head: "h", Base: "b", Title: "t"})
	if err == nil {
		t.Fatal("expected error from aborted retry")
	}
	if stub.createCalls != 1 {
		t.Errorf("createCalls = %d, want 1 (aborted before second attempt)", stub.createCalls)
	}
}

// A reported 422-duplicate with no PR found on the fallback list fails loudly.
func TestOpenPR_DuplicateButNoPRFound(t *testing.T) {
	stub, srv := newStubAPI(t)
	stub.listResponses = []stubResp{{code: http.StatusOK, body: `[]`}} // both lists empty
	stub.createResponses = []stubResp{{code: http.StatusUnprocessableEntity, body: `{"message":"A pull request already exists."}`}}
	c := newRetryClient(srv)
	_, err := c.OpenPR(context.Background(), OpenPRArgs{Owner: "o", Repo: "r", Head: "h", Base: "b", Title: "t"})
	if err == nil || !strings.Contains(err.Error(), "no open PR found") {
		t.Fatalf("err = %v, want 'no open PR found'", err)
	}
}

// A non-200 list-by-head status surfaces an error.
func TestOpenPR_ListByHeadErrorStatus(t *testing.T) {
	stub, srv := newStubAPI(t)
	stub.listResponses = []stubResp{{code: http.StatusUnauthorized, body: `{"message":"bad creds"}`}}
	c := newRetryClient(srv)
	_, err := c.OpenPR(context.Background(), OpenPRArgs{Owner: "o", Repo: "r", Head: "h", Base: "b", Title: "t"})
	if err == nil || !strings.Contains(err.Error(), "list PRs by head") {
		t.Fatalf("err = %v, want a list-PRs error", err)
	}
	if stub.createCalls != 0 {
		t.Errorf("createCalls = %d, want 0", stub.createCalls)
	}
}

func TestRetryDelay(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	mk := func(headers map[string]string) *http.Response {
		h := http.Header{}
		for k, v := range headers {
			h.Set(k, v)
		}
		return &http.Response{Header: h}
	}
	limit := 30 * time.Second

	if got := retryDelay(mk(map[string]string{"Retry-After": "5"}), 0, time.Millisecond, limit, now); got != 5*time.Second {
		t.Errorf("integer Retry-After: got %v, want 5s", got)
	}
	// Retry-After huge → clamped to limit.
	if got := retryDelay(mk(map[string]string{"Retry-After": "999999"}), 0, time.Millisecond, limit, now); got != limit {
		t.Errorf("huge Retry-After: got %v, want %v (capped)", got, limit)
	}
	// HTTP-date Retry-After.
	httpDate := now.Add(3 * time.Second).UTC().Format(http.TimeFormat)
	if got := retryDelay(mk(map[string]string{"Retry-After": httpDate}), 0, time.Millisecond, limit, now); got != 3*time.Second {
		t.Errorf("HTTP-date Retry-After: got %v, want 3s", got)
	}
	// X-RateLimit-Reset honored when remaining==0.
	reset := strconv.FormatInt(now.Add(4*time.Second).Unix(), 10)
	if got := retryDelay(mk(map[string]string{"X-RateLimit-Remaining": "0", "X-RateLimit-Reset": reset}), 0, time.Millisecond, limit, now); got != 4*time.Second {
		t.Errorf("X-RateLimit-Reset: got %v, want 4s", got)
	}
	// Malformed Retry-After falls through to bounded backoff (in [0, base]).
	got := retryDelay(mk(map[string]string{"Retry-After": "not-a-number"}), 0, time.Millisecond, limit, now)
	if got < 0 || got > time.Millisecond {
		t.Errorf("malformed Retry-After: got %v, want backoff in [0, 1ms]", got)
	}
	// Exponential backoff without headers is bounded by base*2^attempt (capped).
	got = retryDelay(mk(nil), 2, time.Millisecond, limit, now)
	if got < 0 || got > 4*time.Millisecond {
		t.Errorf("backoff attempt=2: got %v, want in [0, 4ms]", got)
	}
}

func TestClampDelay(t *testing.T) {
	limit := 10 * time.Second
	if got := clampDelay(-time.Second, limit); got != 0 {
		t.Errorf("negative: got %v, want 0", got)
	}
	if got := clampDelay(limit+time.Second, limit); got != limit {
		t.Errorf("over cap: got %v, want %v", got, limit)
	}
	if got := clampDelay(3*time.Second, limit); got != 3*time.Second {
		t.Errorf("in range: got %v, want 3s", got)
	}
}

func TestContextSleep_CompletesAndNoop(t *testing.T) {
	if err := contextSleep(context.Background(), time.Millisecond); err != nil {
		t.Errorf("positive sleep: %v", err)
	}
	if err := contextSleep(context.Background(), 0); err != nil {
		t.Errorf("zero sleep: %v", err)
	}
}

func TestRetryableStatus(t *testing.T) {
	rl := http.Header{"Retry-After": []string{"1"}}
	primary := http.Header{"X-Ratelimit-Remaining": []string{"0"}}
	cases := []struct {
		code int
		h    http.Header
		want bool
	}{
		{500, nil, true}, {503, nil, true}, {599, nil, true}, {429, nil, true},
		{403, rl, true}, {403, primary, true}, {403, nil, false},
		{404, nil, false}, {422, nil, false}, {201, nil, false},
	}
	for _, tc := range cases {
		if got := retryableStatus(tc.code, tc.h); got != tc.want {
			t.Errorf("retryableStatus(%d,%v)=%v, want %v", tc.code, tc.h, got, tc.want)
		}
	}
}
