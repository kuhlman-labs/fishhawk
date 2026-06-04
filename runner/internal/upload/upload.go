// Package upload is the runner's HTTP client for the backend's
// signing-key + trace endpoints. Two calls per run:
//
//  1. IssueKey → POST /v0/runs/{run_id}/signing-key returns the
//     Ed25519 keypair (the private half is delivered exactly once
//     and the runner must hold it in process memory only).
//  2. ShipTrace → POST /v0/runs/{run_id}/trace uploads the gzipped
//     bundle bytes with X-Fishhawk-Signature: hex(Ed25519(sha256
//     (body))).
//
// Auth on /trace IS the signature itself per the OpenAPI spec; auth
// on /signing-key is GitHub OIDC in production, not yet enforced
// (tracked as E3.10 / #112 on the backend side).
//
// Retries: ShipTrace retries on transient failures (5xx + network
// errors) with exponential backoff up to maxRetries. The endpoint
// is idempotent at the storage layer (content-addressed key + same
// bytes → no-op), so retries are safe.
package upload

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Default backoff parameters for ShipTrace. Public so tests can
// reach in and shrink them; production callers should leave the
// defaults alone.
var (
	DefaultMaxRetries = 3
	DefaultBackoff    = 500 * time.Millisecond
)

// Errors callers may want to switch on. ErrSignatureRejected is
// the runner's signal to STOP retrying — the backend rejected the
// signature, retrying with the same bytes won't help.
var (
	ErrSignatureRejected = errors.New("upload: backend rejected signature")
	// ErrPlanInvalid surfaces when the backend's standard_v1 schema
	// validation rejects the plan body. Permanent at the protocol
	// layer — re-shipping the same bytes won't help; the agent's
	// output is bad.
	ErrPlanInvalid = errors.New("upload: plan rejected as schema-invalid")
	// ErrPullRequestInvalid surfaces when the backend rejects the
	// pull-request body for missing required fields or wrong shape.
	// Permanent at the protocol layer — the runner shipped the
	// wrong fields; retrying won't help.
	ErrPullRequestInvalid = errors.New("upload: pull-request rejected as invalid")
	ErrAlreadyIssued      = errors.New("upload: signing key already issued for this run")
	ErrNotFound           = errors.New("upload: run or signing key not found")
	// ErrUnsupportedStage means the backend has no prompt template
	// for this stage type. Non-retryable; the runner should fail
	// the stage rather than guess.
	ErrUnsupportedStage = errors.New("upload: backend does not support this stage type")
	// ErrRetryNotApplicable is returned by RetryStage when the backend
	// responds 422 (retry_not_applicable). The stage's failure category
	// does not permit a retry (e.g., category-B). Non-retryable; the
	// runner should exit with the original failure.
	ErrRetryNotApplicable = errors.New("upload: retry not applicable for this stage")
	// ErrNoInstallation is returned by FetchInstallationToken when the
	// backend responds 400 (no_installation_for_run): the run has no
	// GitHub App installation attributed to it (a local / MCP-created
	// run on a repo with no App). Callers switch on this sentinel to
	// fall back to the operator's `gh` CLI token for push + PR (#713).
	ErrNoInstallation = errors.New("upload: run has no GitHub App installation")
)

// Client wraps a net/http.Client with a base URL. Construct via
// New; tests can override HTTP and Backoff for determinism.
type Client struct {
	BaseURL string
	HTTP    *http.Client

	// MaxRetries caps ShipTrace retry attempts on retryable
	// failures. Zero means DefaultMaxRetries.
	MaxRetries int
	// Backoff is the initial delay before the first retry; each
	// subsequent retry doubles. Zero means DefaultBackoff.
	Backoff time.Duration
}

// New returns a Client pointed at baseURL with sensible defaults.
// baseURL must NOT have a trailing slash; the path-building code
// concatenates `/v0/...` directly.
func New(baseURL string) *Client {
	return &Client{
		BaseURL: baseURL,
		// 120s is defense-in-depth, NOT the real fix for the reviewer-
		// killed-at-60s bug (#584). The actual fix is server-side: the
		// backend now detaches advisory plan/implement review from the
		// upload request context (context.WithoutCancel) and runs it
		// asynchronously, so the review completes to its own
		// FISHHAWKD_PLAN_REVIEW_TIMEOUT budget regardless of when this
		// client disconnects. A gating-mode synchronous review can still
		// exceed any fixed client timeout; if this client gives up first,
		// the detached server-side review completes anyway and a retried
		// upload short-circuits at GetByHash. Raising the ceiling just
		// keeps advisory uploads well under it and reduces spurious
		// gating retries.
		HTTP: &http.Client{Timeout: 120 * time.Second},
	}
}

// IssuedKey is what IssueKey returns: the freshly minted keypair
// plus the issuance window. The runner must store PrivateKey in
// process memory only and never persist it.
type IssuedKey struct {
	RunID      string
	PublicKey  ed25519.PublicKey
	PrivateKey ed25519.PrivateKey
	IssuedAt   time.Time
	ExpiresAt  time.Time
}

// signingKeyResponse mirrors the backend's response shape exactly.
// Field names match docs/api/v0.openapi.yaml.
type signingKeyResponse struct {
	RunID      string    `json:"run_id"`
	PublicKey  string    `json:"public_key"`  // base64
	PrivateKey string    `json:"private_key"` // base64
	IssuedAt   time.Time `json:"issued_at"`
	ExpiresAt  time.Time `json:"expires_at"`
}

// IssueKey calls POST /v0/runs/{run_id}/signing-key and returns the
// decoded key material.
//
// Multi-call against backends with migration 0012+: each call inserts
// a new row, every stage's fresh runner process gets its own private
// key, the backend's Verify uses the latest unexpired key. Older
// backends return 409 ErrAlreadyIssued for the second call; we keep
// that mapping as a defensive shim (callers shouldn't see it in
// practice). Single-attempt either way — IssueKey doesn't retry on
// transient errors so the caller can decide whether to abort or
// surface them.
func (c *Client) IssueKey(ctx context.Context, runID string, ttl time.Duration) (*IssuedKey, error) {
	body := struct {
		TTLSeconds int `json:"ttl_seconds,omitempty"`
	}{}
	if ttl > 0 {
		body.TTLSeconds = int(ttl.Seconds())
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("upload: marshal request: %w", err)
	}

	endpoint := fmt.Sprintf("%s/v0/runs/%s/signing-key", c.BaseURL, url.PathEscape(runID))
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(raw))
	if err != nil {
		return nil, fmt.Errorf("upload: build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("upload: issue key: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	switch resp.StatusCode {
	case http.StatusCreated:
		// fall through
	case http.StatusConflict:
		return nil, ErrAlreadyIssued
	case http.StatusNotFound:
		return nil, ErrNotFound
	default:
		return nil, statusError("issue key", resp)
	}

	var out signingKeyResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("upload: decode response: %w", err)
	}

	pub, err := base64.StdEncoding.DecodeString(out.PublicKey)
	if err != nil {
		return nil, fmt.Errorf("upload: decode public key: %w", err)
	}
	priv, err := base64.StdEncoding.DecodeString(out.PrivateKey)
	if err != nil {
		return nil, fmt.Errorf("upload: decode private key: %w", err)
	}
	return &IssuedKey{
		RunID:      out.RunID,
		PublicKey:  ed25519.PublicKey(pub),
		PrivateKey: ed25519.PrivateKey(priv),
		IssuedAt:   out.IssuedAt,
		ExpiresAt:  out.ExpiresAt,
	}, nil
}

// ShipArgs collects everything ShipTrace needs.
type ShipArgs struct {
	RunID      string
	StageID    string
	Variant    string // "raw" or "redacted"
	Bundle     []byte
	PrivateKey ed25519.PrivateKey
}

// ShipResult is the (run, stage, variant, content_hash) tuple the
// backend echoes on 202.
type ShipResult struct {
	RunID       string `json:"run_id"`
	StageID     string `json:"stage_id"`
	Variant     string `json:"variant"`
	ContentHash string `json:"content_hash"`
}

// ShipTrace signs the bundle's content hash and POSTs it to
// /v0/runs/{run_id}/trace. Retries transient failures (5xx, network
// errors); a 401 signature_invalid is permanent and bubbles up as
// ErrSignatureRejected.
func (c *Client) ShipTrace(ctx context.Context, args ShipArgs) (*ShipResult, error) {
	if len(args.Bundle) == 0 {
		return nil, errors.New("upload: empty bundle")
	}
	if len(args.PrivateKey) != ed25519.PrivateKeySize {
		return nil, errors.New("upload: invalid private key length")
	}

	digest := sha256.Sum256(args.Bundle)
	signature := ed25519.Sign(args.PrivateKey, digest[:])
	sigHex := hex.EncodeToString(signature)

	endpoint := fmt.Sprintf("%s/v0/runs/%s/trace?stage_id=%s&variant=%s",
		c.BaseURL,
		url.PathEscape(args.RunID),
		url.PathEscape(args.StageID),
		url.QueryEscape(args.Variant),
	)

	maxRetries := c.MaxRetries
	if maxRetries == 0 {
		maxRetries = DefaultMaxRetries
	}
	backoff := c.Backoff
	if backoff == 0 {
		backoff = DefaultBackoff
	}

	var lastErr error
	for attempt := 0; attempt <= maxRetries; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(backoff):
			}
			backoff *= 2
		}

		req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(args.Bundle))
		if err != nil {
			return nil, fmt.Errorf("upload: build request: %w", err)
		}
		req.Header.Set("Content-Type", "application/gzip")
		req.Header.Set("X-Fishhawk-Signature", sigHex)
		req.Header.Set("Accept", "application/json")
		// Set Content-Length explicitly so server-side limits engage
		// before we stream a giant body. The server caps at 64 MiB.
		req.ContentLength = int64(len(args.Bundle))

		resp, err := c.HTTP.Do(req)
		if err != nil {
			lastErr = fmt.Errorf("upload: ship trace: %w", err)
			continue
		}

		switch {
		case resp.StatusCode == http.StatusAccepted:
			var out ShipResult
			err := json.NewDecoder(resp.Body).Decode(&out)
			_ = resp.Body.Close()
			if err != nil {
				return nil, fmt.Errorf("upload: decode response: %w", err)
			}
			return &out, nil
		case resp.StatusCode == http.StatusUnauthorized:
			// Signature problems don't get better with retries.
			detail := readBriefBody(resp)
			_ = resp.Body.Close()
			return nil, fmt.Errorf("%w: %s", ErrSignatureRejected, detail)
		case resp.StatusCode == http.StatusNotFound:
			_ = resp.Body.Close()
			return nil, ErrNotFound
		case resp.StatusCode >= 500:
			lastErr = statusError("ship trace", resp)
			_ = resp.Body.Close()
			continue
		default:
			lastErr = statusError("ship trace", resp)
			_ = resp.Body.Close()
			return nil, lastErr
		}
	}
	return nil, fmt.Errorf("upload: ship trace exhausted retries: %w", lastErr)
}

// FetchPromptArgs collects everything FetchPrompt needs.
type FetchPromptArgs struct {
	StageID    string
	PrivateKey ed25519.PrivateKey
}

// FetchedPrompt is the (stage_id, stage_type, prompt, prompt_hash,
// agent_timeout_seconds) tuple the backend returns. Mirrors docs/api/v0.openapi.yaml.
// AgentTimeoutSeconds is the spec-resolved wall-clock cap for the agent
// invocation; 0 means the backend could not resolve a spec-governed timeout
// and the runner should fall back to its own constant.
type FetchedPrompt struct {
	StageID              string `json:"stage_id"`
	StageType            string `json:"stage_type"`
	Prompt               string `json:"prompt"`
	PromptHash           string `json:"prompt_hash"`
	AgentTimeoutSeconds  int    `json:"agent_timeout_seconds"`
	DecomposedFromRunID  string `json:"decomposed_from_run_id,omitempty"`
	VerifyCommand        string `json:"verify_command,omitempty"`
	VerifyTimeoutSeconds int    `json:"verify_timeout_seconds,omitempty"`
	// MinRunnerVersion is the minimum runner version the backend requires.
	// Non-empty only when the backend is a release build. The runner compares
	// this against its own version and exits with exitVersionSkew when it is
	// older than required.
	MinRunnerVersion string `json:"min_runner_version,omitempty"`
	// AgentSelfRetry is true when the workflow spec opts the stage into
	// ADR-023 runner-side self-retry on category-A/C failures.
	AgentSelfRetry bool `json:"agent_self_retry,omitempty"`
	// MaxRetriesSnapshot and RetryAttempt let the runner compute the
	// remaining self-retry budget without an additional API call.
	MaxRetriesSnapshot int `json:"max_retries_snapshot,omitempty"`
	RetryAttempt       int `json:"retry_attempt,omitempty"`
	// ScopeFiles is the approved plan's scope.files, echoed by the
	// backend on implement stages so the runner can bound the commit
	// to exactly those declared paths instead of `git add -A` (#581).
	// Empty when no approved plan was available — the runner falls
	// back to staging every change.
	ScopeFiles []ScopeFile `json:"scope_files,omitempty"`
	// CommitAuthorName / CommitAuthorEmail are the GitHub App bot
	// account's git commit identity, resolved backend-side from the App
	// (slug + bot user-id) so App-backed commits attribute to the App's
	// bot account (#722). Empty when the backend couldn't resolve it (no
	// App JWT, dev/CLI) — the runner falls back to
	// gitops.DefaultAuthorName/DefaultAuthorEmail.
	CommitAuthorName  string `json:"commit_author_name,omitempty"`
	CommitAuthorEmail string `json:"commit_author_email,omitempty"`
}

// ScopeFile is one entry in FetchedPrompt.ScopeFiles: a declared path
// plus its per-file operation (create/modify/delete). Mirrors the
// backend's scope_files response shape and the standard_v1 plan
// scope.files entries.
type ScopeFile struct {
	Path      string `json:"path"`
	Operation string `json:"operation"`
}

// FetchPrompt calls GET /v0/stages/{stage_id}/prompt with an
// X-Fishhawk-Signature signed with the per-run signing key. The
// canonical message is sha256("prompt:" + stage_id) — same shape
// the backend re-derives in promptCanonicalMessage. Bound-to-stage
// scope means a leaked signature can't be replayed against another
// stage within the same run's TTL.
//
// 501 (the backend hasn't implemented a prompt template for this
// stage type yet) and 401 (signature rejected) are non-retryable
// and bubble up directly. 5xx are retryable up to MaxRetries with
// the same backoff policy as ShipTrace.
func (c *Client) FetchPrompt(ctx context.Context, args FetchPromptArgs) (*FetchedPrompt, error) {
	if args.StageID == "" {
		return nil, errors.New("upload: empty stage id")
	}
	if len(args.PrivateKey) != ed25519.PrivateKeySize {
		return nil, errors.New("upload: invalid private key length")
	}

	digest := sha256.Sum256([]byte("prompt:" + args.StageID))
	signature := ed25519.Sign(args.PrivateKey, digest[:])
	sigHex := hex.EncodeToString(signature)

	endpoint := fmt.Sprintf("%s/v0/stages/%s/prompt",
		c.BaseURL, url.PathEscape(args.StageID))

	maxRetries := c.MaxRetries
	if maxRetries == 0 {
		maxRetries = DefaultMaxRetries
	}
	backoff := c.Backoff
	if backoff == 0 {
		backoff = DefaultBackoff
	}

	var lastErr error
	for attempt := 0; attempt <= maxRetries; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(backoff):
			}
			backoff *= 2
		}

		req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
		if err != nil {
			return nil, fmt.Errorf("upload: build request: %w", err)
		}
		req.Header.Set("X-Fishhawk-Signature", sigHex)
		req.Header.Set("Accept", "application/json")

		resp, err := c.HTTP.Do(req)
		if err != nil {
			lastErr = fmt.Errorf("upload: fetch prompt: %w", err)
			continue
		}

		switch {
		case resp.StatusCode == http.StatusOK:
			var out FetchedPrompt
			err := json.NewDecoder(resp.Body).Decode(&out)
			_ = resp.Body.Close()
			if err != nil {
				return nil, fmt.Errorf("upload: decode response: %w", err)
			}
			return &out, nil
		case resp.StatusCode == http.StatusUnauthorized:
			detail := readBriefBody(resp)
			_ = resp.Body.Close()
			return nil, fmt.Errorf("%w: %s", ErrSignatureRejected, detail)
		case resp.StatusCode == http.StatusNotFound:
			_ = resp.Body.Close()
			return nil, ErrNotFound
		case resp.StatusCode == http.StatusNotImplemented:
			detail := readBriefBody(resp)
			_ = resp.Body.Close()
			return nil, fmt.Errorf("%w: %s", ErrUnsupportedStage, detail)
		case resp.StatusCode >= 500:
			lastErr = statusError("fetch prompt", resp)
			_ = resp.Body.Close()
			continue
		default:
			lastErr = statusError("fetch prompt", resp)
			_ = resp.Body.Close()
			return nil, lastErr
		}
	}
	return nil, fmt.Errorf("upload: fetch prompt exhausted retries: %w", lastErr)
}

// readBriefBody returns the first 256 bytes of resp.Body as a
// string, useful for surfacing backend error envelopes in client
// errors without unbounded log lines. Caller is responsible for
// closing resp.Body.
func readBriefBody(resp *http.Response) string {
	r := io.LimitReader(resp.Body, 256)
	b, _ := io.ReadAll(r)
	return string(bytes.TrimSpace(b))
}

// statusError renders a non-2xx response into an error including
// the status text and a short body excerpt. Centralized so every
// non-success path produces a uniform error message.
func statusError(op string, resp *http.Response) error {
	body := readBriefBody(resp)
	if body == "" {
		body = "(no body)"
	}
	return fmt.Errorf("upload: %s: %s: %s",
		op, strconv.Itoa(resp.StatusCode), body)
}

// ShipPlanArgs collects everything ShipPlan needs.
type ShipPlanArgs struct {
	RunID   string
	StageID string
	// Plan is the JSON bytes of the standard_v1 plan artifact.
	// Caller is responsible for reading the file from --plan-out
	// and validating it locally before shipping.
	Plan       []byte
	PrivateKey ed25519.PrivateKey
}

// ShipPlanResult is the (run, stage, content_hash, idempotent) tuple
// the backend echoes on 201 / 200. Idempotent=true means a plan
// with this content_hash already existed for this stage; the backend
// returned it unchanged and didn't insert a duplicate row.
type ShipPlanResult struct {
	ID            string `json:"id"`
	StageID       string `json:"stage_id"`
	ContentHash   string `json:"content_hash"`
	SchemaVersion string `json:"schema_version"`
	Idempotent    bool   `json:"idempotent"`
}

// ShipPlan signs the plan bytes and POSTs them to
// /v0/runs/{run_id}/plan?stage_id=…. Retries transient failures
// (5xx, network errors). Permanent failures bubble up:
//
//   - 400 plan_invalid → ErrPlanInvalid (agent produced a non-schema plan)
//   - 401 signature_*  → ErrSignatureRejected
//   - 404 stage/key    → ErrNotFound
//
// On 201 the backend created a fresh artifact; on 200 the upload
// matched an existing one (Result.Idempotent==true).
func (c *Client) ShipPlan(ctx context.Context, args ShipPlanArgs) (*ShipPlanResult, error) {
	if len(args.Plan) == 0 {
		return nil, errors.New("upload: empty plan")
	}
	if len(args.PrivateKey) != ed25519.PrivateKeySize {
		return nil, errors.New("upload: invalid private key length")
	}

	digest := sha256.Sum256(args.Plan)
	signature := ed25519.Sign(args.PrivateKey, digest[:])
	sigHex := hex.EncodeToString(signature)

	endpoint := fmt.Sprintf("%s/v0/runs/%s/plan?stage_id=%s",
		c.BaseURL,
		url.PathEscape(args.RunID),
		url.PathEscape(args.StageID),
	)

	maxRetries := c.MaxRetries
	if maxRetries == 0 {
		maxRetries = DefaultMaxRetries
	}
	backoff := c.Backoff
	if backoff == 0 {
		backoff = DefaultBackoff
	}

	var lastErr error
	for attempt := 0; attempt <= maxRetries; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(backoff):
			}
			backoff *= 2
		}

		req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(args.Plan))
		if err != nil {
			return nil, fmt.Errorf("upload: build plan request: %w", err)
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Fishhawk-Signature", sigHex)
		req.Header.Set("Accept", "application/json")
		req.ContentLength = int64(len(args.Plan))

		resp, err := c.HTTP.Do(req)
		if err != nil {
			lastErr = fmt.Errorf("upload: ship plan: %w", err)
			continue
		}

		switch {
		case resp.StatusCode == http.StatusCreated || resp.StatusCode == http.StatusOK:
			var out ShipPlanResult
			err := json.NewDecoder(resp.Body).Decode(&out)
			_ = resp.Body.Close()
			if err != nil {
				return nil, fmt.Errorf("upload: decode plan response: %w", err)
			}
			return &out, nil
		case resp.StatusCode == http.StatusBadRequest:
			detail := readBriefBody(resp)
			_ = resp.Body.Close()
			// 400s are permanent; the body distinguishes plan_invalid
			// (schema fail) from validation_failed (path/query issues).
			if strings.Contains(detail, "plan_invalid") {
				return nil, fmt.Errorf("%w: %s", ErrPlanInvalid, detail)
			}
			return nil, fmt.Errorf("upload: ship plan: 400: %s", detail)
		case resp.StatusCode == http.StatusUnauthorized:
			detail := readBriefBody(resp)
			_ = resp.Body.Close()
			return nil, fmt.Errorf("%w: %s", ErrSignatureRejected, detail)
		case resp.StatusCode == http.StatusNotFound:
			_ = resp.Body.Close()
			return nil, ErrNotFound
		case resp.StatusCode >= 500:
			lastErr = statusError("ship plan", resp)
			_ = resp.Body.Close()
			continue
		default:
			lastErr = statusError("ship plan", resp)
			_ = resp.Body.Close()
			return nil, lastErr
		}
	}
	return nil, fmt.Errorf("upload: ship plan exhausted retries: %w", lastErr)
}

// ShipPullRequestArgs collects everything ShipPullRequest needs.
// Body is the JSON-encoded artifact (the runner serializes the
// PullRequestPayload before signing so the bytes the backend
// verifies against are the same bytes it stores).
type ShipPullRequestArgs struct {
	RunID      string
	StageID    string
	Body       []byte
	PrivateKey ed25519.PrivateKey

	// Outcome, when set to "failed", turns this into a failure-report POST
	// (#742): instead of the success PR artifact in Body, ShipPullRequest
	// signs and ships {"outcome":"failed","category":...,"reason":...} so
	// the backend fails the implement stage its trace gate left in
	// `running`. Category is "B" (invalid PR shape / compile-gate) or "C"
	// (network/git/GitHub). When Outcome is empty the success Body path is
	// used unchanged.
	Outcome  string
	Category string
	Reason   string
}

// pullRequestFailureBody is the failure-report wire shape ShipPullRequest
// marshals when Args.Outcome == "failed". Mirrors the optional fields the
// backend's pullRequestBody decodes (#742); kept here so the runner owns
// the bytes it signs.
type pullRequestFailureBody struct {
	Outcome  string `json:"outcome"`
	Category string `json:"category"`
	Reason   string `json:"reason"`
}

// ShipPullRequestResult is what the backend echoes back. Idempotent
// is true when the upload matched an existing artifact (same
// stage + content_hash) and no new row was inserted.
type ShipPullRequestResult struct {
	ID          string `json:"id"`
	StageID     string `json:"stage_id"`
	ContentHash string `json:"content_hash"`
	PRNumber    int    `json:"pr_number"`
	PRURL       string `json:"pr_url"`
	HeadSHA     string `json:"head_sha"`
	Idempotent  bool   `json:"idempotent"`
}

// ShipPullRequest signs the body and POSTs it to
// /v0/runs/{run_id}/pull-request?stage_id=…. Retries 5xx with
// exponential backoff. Permanent failures bubble up:
//
//   - 400 pull_request_invalid → ErrPullRequestInvalid (runner shipped wrong shape)
//   - 401 signature_*          → ErrSignatureRejected
//   - 404 stage/key            → ErrNotFound
//
// On 201 the backend created a fresh artifact; on 200 the upload
// matched an existing one (Result.Idempotent==true).
func (c *Client) ShipPullRequest(ctx context.Context, args ShipPullRequestArgs) (*ShipPullRequestResult, error) {
	body := args.Body
	if args.Outcome == "failed" {
		// Failure-report POST (#742): build the failure body from the
		// outcome fields rather than the (absent) success artifact.
		marshalled, err := json.Marshal(pullRequestFailureBody{
			Outcome:  args.Outcome,
			Category: args.Category,
			Reason:   args.Reason,
		})
		if err != nil {
			return nil, fmt.Errorf("upload: marshal pull-request failure body: %w", err)
		}
		body = marshalled
	}
	if len(body) == 0 {
		return nil, errors.New("upload: empty pull-request body")
	}
	if len(args.PrivateKey) != ed25519.PrivateKeySize {
		return nil, errors.New("upload: invalid private key length")
	}

	digest := sha256.Sum256(body)
	signature := ed25519.Sign(args.PrivateKey, digest[:])
	sigHex := hex.EncodeToString(signature)

	endpoint := fmt.Sprintf("%s/v0/runs/%s/pull-request?stage_id=%s",
		c.BaseURL,
		url.PathEscape(args.RunID),
		url.PathEscape(args.StageID),
	)

	maxRetries := c.MaxRetries
	if maxRetries == 0 {
		maxRetries = DefaultMaxRetries
	}
	backoff := c.Backoff
	if backoff == 0 {
		backoff = DefaultBackoff
	}

	var lastErr error
	for attempt := 0; attempt <= maxRetries; attempt++ {
		if attempt > 0 {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(backoff):
			}
			backoff *= 2
		}

		req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
		if err != nil {
			return nil, fmt.Errorf("upload: build pull-request request: %w", err)
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Fishhawk-Signature", sigHex)
		req.Header.Set("Accept", "application/json")
		req.ContentLength = int64(len(body))

		resp, err := c.HTTP.Do(req)
		if err != nil {
			lastErr = fmt.Errorf("upload: ship pull-request: %w", err)
			continue
		}

		switch {
		case resp.StatusCode == http.StatusCreated || resp.StatusCode == http.StatusOK:
			var out ShipPullRequestResult
			err := json.NewDecoder(resp.Body).Decode(&out)
			_ = resp.Body.Close()
			if err != nil {
				return nil, fmt.Errorf("upload: decode pull-request response: %w", err)
			}
			return &out, nil
		case resp.StatusCode == http.StatusBadRequest:
			detail := readBriefBody(resp)
			_ = resp.Body.Close()
			if strings.Contains(detail, "pull_request_invalid") {
				return nil, fmt.Errorf("%w: %s", ErrPullRequestInvalid, detail)
			}
			return nil, fmt.Errorf("upload: ship pull-request: 400: %s", detail)
		case resp.StatusCode == http.StatusUnauthorized:
			detail := readBriefBody(resp)
			_ = resp.Body.Close()
			return nil, fmt.Errorf("%w: %s", ErrSignatureRejected, detail)
		case resp.StatusCode == http.StatusNotFound:
			_ = resp.Body.Close()
			return nil, ErrNotFound
		case resp.StatusCode >= 500:
			lastErr = statusError("ship pull-request", resp)
			_ = resp.Body.Close()
			continue
		default:
			lastErr = statusError("ship pull-request", resp)
			_ = resp.Body.Close()
			return nil, lastErr
		}
	}
	return nil, fmt.Errorf("upload: ship pull-request exhausted retries: %w", lastErr)
}

// FetchMCPTokenArgs collects the inputs for FetchMCPToken.
type FetchMCPTokenArgs struct {
	RunID      string
	PrivateKey ed25519.PrivateKey
}

// FetchMCPTokenResult is the wire shape the backend echoes when
// the runner POSTs the empty body. Token is the plaintext bearer
// the runner stamps into the agent's environment as
// FISHHAWK_API_TOKEN; the agent passes it to the MCP server which
// authenticates against the backend on its behalf.
type FetchMCPTokenResult struct {
	Token     string    `json:"token"`
	TokenID   string    `json:"token_id"`
	RunID     string    `json:"run_id"`
	ExpiresAt time.Time `json:"expires_at"`
}

// FetchMCPToken calls POST /v0/runs/{run_id}/mcp-token (E19.8 / #348)
// and returns the short-lived bearer token scoped to the run. Uses
// the same Ed25519 signing-key auth as FetchInstallationToken — the
// runner already has the per-run private key from its IssueKey call
// at stage start; signing over the empty body proves possession.
//
// Single-attempt: token issuance is cheap on the backend (a single
// row insert + audit append); a transient failure here should
// surface so the caller can degrade gracefully (the agent loses its
// FISHHAWK_API_TOKEN but the run continues — MCP awareness is best-
// effort per ADR-021).
func (c *Client) FetchMCPToken(ctx context.Context, args FetchMCPTokenArgs) (*FetchMCPTokenResult, error) {
	if args.RunID == "" {
		return nil, errors.New("upload: run_id required")
	}
	if len(args.PrivateKey) != ed25519.PrivateKeySize {
		return nil, errors.New("upload: invalid private key length")
	}

	body := []byte{}
	digest := sha256.Sum256(body)
	signature := ed25519.Sign(args.PrivateKey, digest[:])
	sigHex := hex.EncodeToString(signature)

	endpoint := fmt.Sprintf("%s/v0/runs/%s/mcp-token",
		c.BaseURL, url.PathEscape(args.RunID))

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("upload: build mcp-token request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Fishhawk-Signature", sigHex)
	req.Header.Set("Accept", "application/json")
	req.ContentLength = 0

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("upload: fetch mcp token: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	switch resp.StatusCode {
	case http.StatusCreated:
		var out FetchMCPTokenResult
		if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
			return nil, fmt.Errorf("upload: decode mcp-token response: %w", err)
		}
		if out.Token == "" {
			return nil, errors.New("upload: mcp-token response missing token")
		}
		return &out, nil
	case http.StatusUnauthorized:
		detail := readBriefBody(resp)
		return nil, fmt.Errorf("%w: %s", ErrSignatureRejected, detail)
	case http.StatusNotFound:
		return nil, ErrNotFound
	default:
		return nil, statusError("fetch mcp token", resp)
	}
}

// FetchInstallationTokenArgs collects the inputs for FetchInstallationToken.
type FetchInstallationTokenArgs struct {
	RunID      string
	StageID    string
	PrivateKey ed25519.PrivateKey
}

// FetchInstallationTokenResult is the (token) tuple the backend
// echoes. v0 is just the string; v1+ may add expires_at if the
// runner ever needs to plan around it.
type FetchInstallationTokenResult struct {
	Token string `json:"token"`
}

// FetchInstallationToken POSTs an empty body to /v0/runs/{run_id}/installation-token
// and returns the App's installation token for the run's repo. Used
// by the runner's implement-stage flow so push and PR creation
// happen under the App's identity rather than the workflow's
// GITHUB_TOKEN — drops the "Allow Actions to create PRs" repo-level
// dependency from customer onboarding (E5.X / #197).
//
// Single-attempt: token issuance is cheap on the backend (cached),
// and a transient failure here should surface so the caller can
// abort the implement stage cleanly rather than racing a partial
// state. Permanent failures bubble up via ErrSignatureRejected /
// ErrNotFound.
func (c *Client) FetchInstallationToken(ctx context.Context, args FetchInstallationTokenArgs) (*FetchInstallationTokenResult, error) {
	if args.RunID == "" || args.StageID == "" {
		return nil, errors.New("upload: run_id and stage_id required")
	}
	if len(args.PrivateKey) != ed25519.PrivateKeySize {
		return nil, errors.New("upload: invalid private key length")
	}

	// Empty body — the URL path's run_id is the scoping mechanism;
	// signing over the empty bytes still produces a valid signature
	// the backend can verify against the run's stored public key.
	body := []byte{}
	digest := sha256.Sum256(body)
	signature := ed25519.Sign(args.PrivateKey, digest[:])
	sigHex := hex.EncodeToString(signature)

	endpoint := fmt.Sprintf("%s/v0/runs/%s/installation-token?stage_id=%s",
		c.BaseURL,
		url.PathEscape(args.RunID),
		url.PathEscape(args.StageID),
	)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("upload: build installation-token request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Fishhawk-Signature", sigHex)
	req.Header.Set("Accept", "application/json")
	req.ContentLength = 0

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("upload: fetch installation token: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	switch resp.StatusCode {
	case http.StatusCreated:
		var out FetchInstallationTokenResult
		if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
			return nil, fmt.Errorf("upload: decode installation-token response: %w", err)
		}
		if out.Token == "" {
			return nil, errors.New("upload: installation-token response missing token")
		}
		return &out, nil
	case http.StatusBadRequest:
		// The backend returns 400 no_installation_for_run when the run
		// row has no attributed App installation (a local / MCP run on a
		// repo with no App). Map it to ErrNoInstallation so the caller
		// can fall back to the operator's `gh` CLI token (#713). Any
		// other 400 stays opaque.
		detail := readBriefBody(resp)
		if strings.Contains(detail, "no_installation_for_run") {
			return nil, fmt.Errorf("%w: %s", ErrNoInstallation, detail)
		}
		return nil, fmt.Errorf("upload: fetch installation token: 400: %s", detail)
	case http.StatusUnauthorized:
		detail := readBriefBody(resp)
		return nil, fmt.Errorf("%w: %s", ErrSignatureRejected, detail)
	case http.StatusNotFound:
		return nil, ErrNotFound
	case http.StatusBadGateway:
		return nil, fmt.Errorf("upload: installation-token: GitHub rejected App JWT or installation: %s", readBriefBody(resp))
	default:
		return nil, statusError("fetch installation token", resp)
	}
}

// RetryStageArgs collects the inputs for RetryStage.
type RetryStageArgs struct {
	StageID    string
	PrivateKey ed25519.PrivateKey
}

// RetryStage calls POST /v0/stages/{stage_id}/retry using the same
// Ed25519 signing-key auth as FetchInstallationToken — signs over the
// empty body to prove key possession. Single-attempt; the caller
// decides whether to surface the error as a hard failure or log and
// break the retry loop.
//
//   - 200 → nil (stage transitioned; orchestrator will dispatch)
//   - 403 → descriptive non-nil error
//   - 422 (retry_not_applicable) → ErrRetryNotApplicable sentinel
//   - 404 / 5xx → generic error via statusError
func (c *Client) RetryStage(ctx context.Context, args RetryStageArgs) error {
	if args.StageID == "" {
		return errors.New("upload: stage_id required")
	}
	if len(args.PrivateKey) != ed25519.PrivateKeySize {
		return errors.New("upload: invalid private key length")
	}

	body := []byte{}
	digest := sha256.Sum256(body)
	signature := ed25519.Sign(args.PrivateKey, digest[:])
	sigHex := hex.EncodeToString(signature)

	endpoint := fmt.Sprintf("%s/v0/stages/%s/retry",
		c.BaseURL, url.PathEscape(args.StageID))

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("upload: build retry request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Fishhawk-Signature", sigHex)
	req.Header.Set("Accept", "application/json")
	req.ContentLength = 0

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return fmt.Errorf("upload: retry stage: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	switch resp.StatusCode {
	case http.StatusOK:
		return nil
	case http.StatusForbidden:
		detail := readBriefBody(resp)
		return fmt.Errorf("upload: retry stage: forbidden: %s", detail)
	case http.StatusUnprocessableEntity:
		return ErrRetryNotApplicable
	default:
		return statusError("retry stage", resp)
	}
}
