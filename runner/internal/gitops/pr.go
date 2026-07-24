package gitops

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// defaultGitHubAPIBase is the public api.github.com URL. Override
// via OpenPRClient.APIBase in tests pointing at an httptest.Server.
const defaultGitHubAPIBase = "https://api.github.com"

// OpenPRClient creates pull requests via GitHub's REST API. Caller
// constructs once with the auth token; OpenPR is safe to call
// repeatedly across stages (each PR is independent).
type OpenPRClient struct {
	// HTTP defaults to a 30s-timeout client when nil. Tests
	// override.
	HTTP *http.Client

	// APIBase is the GitHub API root. Empty defaults to
	// https://api.github.com. Tests point at httptest.
	APIBase string

	// Token is the GitHub token (workflow GITHUB_TOKEN or App
	// installation token). Required.
	Token string

	// MaxRetries is the number of ADDITIONAL attempts after the first for a
	// transient fault (up to MaxRetries+1 total round trips per HTTP call).
	// Zero → defaultMaxRetries.
	MaxRetries int

	// RetryBackoff seeds the exponential fallback backoff (base * 2^attempt)
	// before full jitter. Zero → defaultRetryBackoff. Tests inject a
	// sub-millisecond base.
	RetryBackoff time.Duration

	// sleeper performs a context-aware sleep between attempts; injected in tests
	// to record the requested delay VALUE without real time. Nil → contextSleep.
	sleeper func(ctx context.Context, d time.Duration) error

	// now supplies the wall clock for HTTP-date / X-RateLimit-Reset arithmetic
	// and the remaining-budget cap; injected in tests. Nil → time.Now.
	now func() time.Time
}

// Retry tuning. maxRetryDelay caps any single honored/backoff wait; it is
// bounded so one honored Retry-After cannot dominate the caller's own timeout.
const (
	defaultMaxRetries   = 4
	defaultRetryBackoff = 200 * time.Millisecond
	maxRetryDelay       = 20 * time.Second
)

// OpenPRArgs collects the inputs for a single PR creation.
type OpenPRArgs struct {
	Owner string // "kuhlman-labs"
	Repo  string // "fishhawk"
	Head  string // "fishhawk/run-aaa/stage-bbb"
	Base  string // "main"
	Title string
	Body  string
}

// OpenPRResult mirrors the shape the runner's pull-request artifact
// upload needs.
type OpenPRResult struct {
	PRNumber int
	PRURL    string
}

// OpenPR opens (or adopts) the pull request for args.Head → args.Base and
// returns its (number, html_url). Authenticates with `Authorization: Bearer
// <token>` per GitHub's REST guidance.
//
// It is IDEMPOTENT by head and RETRIED on transient faults (#2167 / E48.45):
//
//  1. It FIRST lists open PRs by head (a GET) and ADOPTS an existing one with NO
//     create POST, so a re-attempt after a partial failure never opens a
//     duplicate.
//  2. Otherwise it POSTs the create inside a bounded exponential-backoff retry
//     loop (honoring Retry-After / X-RateLimit-Reset, context-aware sleeps), so
//     an isolated 5xx / secondary-rate-limit blip on create is absorbed instead
//     of failing the implement stage.
//  3. If a concurrent attempt won a create race (GitHub returns 422 "already
//     exists"), it falls back to the list-by-head adopt.
//
// The create POST is retry-safe because a re-applied create surfaces the same
// 422-duplicate that step 3 handles. A non-rate-limit 4xx or exhausted retries
// returns promptly. The caller surfaces any error to the audit log.
func (c *OpenPRClient) OpenPR(ctx context.Context, args OpenPRArgs) (*OpenPRResult, error) {
	switch {
	case c.Token == "":
		return nil, errors.New("gitops: token required")
	case args.Owner == "" || args.Repo == "":
		return nil, errors.New("gitops: owner and repo required")
	case args.Head == "" || args.Base == "":
		return nil, errors.New("gitops: head and base required")
	case args.Title == "":
		return nil, errors.New("gitops: title required")
	}

	apiBase := c.APIBase
	if apiBase == "" {
		apiBase = defaultGitHubAPIBase
	}
	apiBase = strings.TrimRight(apiBase, "/")

	httpClient := c.HTTP
	if httpClient == nil {
		httpClient = &http.Client{Timeout: 30 * time.Second}
	}

	// (1) Adopt an existing open PR for head→base without any create POST.
	if existing, err := c.listOpenPRByHead(ctx, httpClient, apiBase, args); err != nil {
		return nil, err
	} else if existing != nil {
		return existing, nil
	}

	// (2) Create inside the retry loop.
	result, dup, err := c.createPR(ctx, httpClient, apiBase, args)
	if err != nil {
		return nil, err
	}
	if !dup {
		return result, nil
	}

	// (3) Lost a create race (422 duplicate): adopt the PR the winner created.
	existing, err := c.listOpenPRByHead(ctx, httpClient, apiBase, args)
	if err != nil {
		return nil, err
	}
	if existing == nil {
		return nil, errors.New("gitops: open PR: duplicate reported but no open PR found for head")
	}
	return existing, nil
}

// createPR POSTs the create request inside the retry loop. It returns
// (result, false, nil) on success, (nil, true, nil) on a 422-duplicate (the
// caller adopts), or (nil, false, err) on any other failure.
func (c *OpenPRClient) createPR(ctx context.Context, httpClient *http.Client, apiBase string, args OpenPRArgs) (*OpenPRResult, bool, error) {
	endpoint := fmt.Sprintf("%s/repos/%s/%s/pulls", apiBase, args.Owner, args.Repo)
	payload := map[string]any{
		"title": args.Title,
		"head":  args.Head,
		"base":  args.Base,
		"body":  args.Body,
	}
	raw, _ := json.Marshal(payload)

	resp, err := c.doRetrying(ctx, httpClient, func() (*http.Request, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(raw))
		if err != nil {
			return nil, fmt.Errorf("gitops: build pulls request: %w", err)
		}
		c.setHeaders(req)
		req.Header.Set("Content-Type", "application/json")
		return req, nil
	})
	if err != nil {
		return nil, false, fmt.Errorf("gitops: open PR: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusCreated {
		brief := readBriefBody(resp.Body, 512)
		// A 422 whose body reports an existing PR is a create race, not a
		// failure: signal the caller to adopt via list-by-head.
		if resp.StatusCode == http.StatusUnprocessableEntity && strings.Contains(strings.ToLower(brief), "already exists") {
			return nil, true, nil
		}
		return nil, false, fmt.Errorf("gitops: open PR: %d: %s", resp.StatusCode, brief)
	}

	out, err := decodePR(resp.Body)
	if err != nil {
		return nil, false, err
	}
	return out, false, nil
}

// listOpenPRByHead GETs the open PRs for head→base and returns the first one
// (adopt), or nil when none exists. The GET flows through the same retry loop.
func (c *OpenPRClient) listOpenPRByHead(ctx context.Context, httpClient *http.Client, apiBase string, args OpenPRArgs) (*OpenPRResult, error) {
	q := url.Values{}
	q.Set("head", args.Owner+":"+args.Head)
	q.Set("base", args.Base)
	q.Set("state", "open")
	endpoint := fmt.Sprintf("%s/repos/%s/%s/pulls?%s", apiBase, args.Owner, args.Repo, q.Encode())

	resp, err := c.doRetrying(ctx, httpClient, func() (*http.Request, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
		if err != nil {
			return nil, fmt.Errorf("gitops: build list-pulls request: %w", err)
		}
		c.setHeaders(req)
		return req, nil
	})
	if err != nil {
		return nil, fmt.Errorf("gitops: list PRs by head: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		brief := readBriefBody(resp.Body, 512)
		return nil, fmt.Errorf("gitops: list PRs by head: %d: %s", resp.StatusCode, brief)
	}

	var list []struct {
		Number  int    `json:"number"`
		HTMLURL string `json:"html_url"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&list); err != nil {
		return nil, fmt.Errorf("gitops: decode list PRs response: %w", err)
	}
	for _, pr := range list {
		if pr.Number != 0 && pr.HTMLURL != "" {
			return &OpenPRResult{PRNumber: pr.Number, PRURL: pr.HTMLURL}, nil
		}
	}
	return nil, nil
}

// setHeaders applies the shared GitHub REST headers + auth to req.
func (c *OpenPRClient) setHeaders(req *http.Request) {
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("Authorization", "Bearer "+c.Token)
}

// decodePR decodes a single PR response body and validates the required fields.
func decodePR(r io.Reader) (*OpenPRResult, error) {
	var out struct {
		Number  int    `json:"number"`
		HTMLURL string `json:"html_url"`
	}
	if err := json.NewDecoder(r).Decode(&out); err != nil {
		return nil, fmt.Errorf("gitops: decode PR response: %w", err)
	}
	if out.Number == 0 || out.HTMLURL == "" {
		return nil, errors.New("gitops: PR response missing number or html_url")
	}
	return &OpenPRResult{PRNumber: out.Number, PRURL: out.HTMLURL}, nil
}

// doRetrying issues build()'s request through httpClient, retrying transient
// faults (retryableStatus) up to MaxRetries additional times with bounded,
// context-aware backoff. build is re-invoked each attempt so the request body is
// freshly rewound. It returns the final (or first non-retryable) response; the
// caller closes its body.
func (c *OpenPRClient) doRetrying(ctx context.Context, httpClient *http.Client, build func() (*http.Request, error)) (*http.Response, error) {
	maxRetries := c.MaxRetries
	if maxRetries <= 0 {
		maxRetries = defaultMaxRetries
	}
	base := c.RetryBackoff
	if base <= 0 {
		base = defaultRetryBackoff
	}
	now := c.now
	if now == nil {
		now = time.Now
	}
	sleeper := c.sleeper
	if sleeper == nil {
		sleeper = contextSleep
	}

	for attempt := 0; ; attempt++ {
		req, err := build()
		if err != nil {
			return nil, err
		}
		resp, err := httpClient.Do(req)
		if err != nil {
			return nil, err
		}
		if attempt >= maxRetries || !retryableStatus(resp.StatusCode, resp.Header) {
			return resp, nil
		}

		delay := retryDelay(resp, attempt, base, maxRetryDelay, now())
		// Cap to the remaining context budget: if the honored delay would sleep
		// past the deadline, give up promptly and return the final response.
		if deadline, ok := ctx.Deadline(); ok {
			if remaining := deadline.Sub(now()); remaining <= 0 || delay > remaining {
				return resp, nil
			}
		}

		drainAndClose(resp.Body)
		if serr := sleeper(ctx, delay); serr != nil {
			return nil, serr
		}
	}
}

// retryableStatus reports whether an HTTP status (with its headers) is a
// transient fault worth retrying: ALL 5xx (>=500), 429, and a 403 carrying a
// rate-limit signal (a Retry-After header, or x-ratelimit-remaining:0). A plain
// permission 403 is NOT retryable and returns promptly.
func retryableStatus(code int, h http.Header) bool {
	switch {
	case code >= 500:
		return true
	case code == http.StatusTooManyRequests:
		return true
	case code == http.StatusForbidden:
		return h.Get("Retry-After") != "" || h.Get("X-RateLimit-Remaining") == "0"
	default:
		return false
	}
}

// retryDelay computes the wait before the next attempt: Retry-After (integer
// seconds OR HTTP-date per RFC 7231 §7.1.3), else X-RateLimit-Reset (unix epoch
// seconds) when x-ratelimit-remaining:0, else exponential backoff
// (base * 2^attempt) with full jitter. Every result is clamped to [0, cap]. A
// malformed header falls through to backoff (fail-safe, never a hang). now is
// injected so tests assert the honored delay VALUE.
func retryDelay(resp *http.Response, attempt int, base, limit time.Duration, now time.Time) time.Duration {
	if v := strings.TrimSpace(resp.Header.Get("Retry-After")); v != "" {
		if secs, err := strconv.Atoi(v); err == nil && secs >= 0 {
			return clampDelay(time.Duration(secs)*time.Second, limit)
		}
		if t, err := http.ParseTime(v); err == nil {
			return clampDelay(t.Sub(now), limit)
		}
	}
	if resp.Header.Get("X-RateLimit-Remaining") == "0" {
		if v := strings.TrimSpace(resp.Header.Get("X-RateLimit-Reset")); v != "" {
			if epoch, err := strconv.ParseInt(v, 10, 64); err == nil {
				return clampDelay(time.Unix(epoch, 0).Sub(now), limit)
			}
		}
	}
	backoff := base << attempt
	if backoff <= 0 || backoff > limit {
		backoff = limit
	}
	return time.Duration(rand.Int63n(int64(backoff) + 1))
}

// clampDelay bounds d to [0, limit].
func clampDelay(d, limit time.Duration) time.Duration {
	if d < 0 {
		return 0
	}
	if d > limit {
		return limit
	}
	return d
}

// contextSleep sleeps for d unless ctx is cancelled first, returning ctx.Err()
// promptly on cancellation. A non-positive d is a no-op.
func contextSleep(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// drainAndClose drains a bounded amount of a to-be-discarded body then closes
// it, so the underlying connection can be reused for the retry.
func drainAndClose(body io.ReadCloser) {
	if body == nil {
		return
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(body, 1<<16))
	_ = body.Close()
}

// readBriefBody reads up to limit bytes for inclusion in error
// messages. Larger bodies get truncated; tests pass small limits to
// pin assertions.
func readBriefBody(r io.Reader, limit int64) string {
	limited := io.LimitReader(r, limit)
	b, _ := io.ReadAll(limited)
	return strings.TrimSpace(string(b))
}
