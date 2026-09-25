package main

import (
	"context"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/kuhlman-labs/fishhawk/runner/internal/upload"
)

// mcpTokenRefreshSkew is how close to its expiry a stored credential (the
// fhm_ bearer OR the per-run signing key that mints it) may get before
// bearer() re-mints it. Five minutes comfortably exceeds any single
// post-agent call, so a token handed out by bearer() is still valid when the
// request it authorizes lands.
const mcpTokenRefreshSkew = 5 * time.Minute

// mcpTokenRefreshRetryInterval is the minimum gap between refresh attempts
// after a GENUINE mint failure. Without it every consumer poll during a
// backend outage would issue its own IssueKey + FetchMCPToken pair.
const mcpTokenRefreshRetryInterval = 30 * time.Second

// mcpTokenRefreshTimeout bounds one refresh (IssueKey + FetchMCPToken). The
// refresh runs under context.WithTimeout(context.WithoutCancel(ctx), …) — a
// context DETACHED from the caller's cancellation — so the mutex held across
// it is held for at most this long, and a caller cancelling its own context
// can never fail a refresh (nor stamp the retry backoff) on its behalf.
const mcpTokenRefreshTimeout = 15 * time.Second

// mcpTokenClient is the narrow capability mcpTokenSource needs from the
// upload client. upload.Client and the uploadClient test seam satisfy it.
type mcpTokenClient interface {
	IssueKey(ctx context.Context, runID string, ttl time.Duration) (*upload.IssuedKey, error)
	FetchMCPToken(ctx context.Context, args upload.FetchMCPTokenArgs) (*upload.FetchMCPTokenResult, error)
}

// mcpTokenSource owns the run-bound fhm_ bearer and lazily re-mints it when it
// nears expiry (#3255). The token is minted with a fixed 60-minute TTL but a
// stage's wall clock is unbounded by any fixed margin (each verify-fix
// re-invocation gets the full agent timeout again), so every post-agent
// consumer reads the bearer at its point of use through bearer().
//
// The source keeps its OWN copy of the per-run Ed25519 signing key and never
// writes back to run()'s issuedKey local: bearer() is called concurrently from
// the progress tee's report goroutine and the amendment watch goroutine, so a
// write-back would race the main goroutine's reads. The backend's signing-key
// Issue is multi-call-safe on migration 0012+ (see
// reissueSigningKeyForTerminalUpload), so an extra issuance is correct.
//
// A nil *mcpTokenSource is valid: bearer() returns "" with no network call.
type mcpTokenSource struct {
	client  mcpTokenClient
	runID   string
	logSink io.Writer
	now     func() time.Time

	mu          sync.Mutex
	token       string
	tokenExpiry time.Time
	key         *upload.IssuedKey
	nextAttempt time.Time
}

// newMCPTokenSource seeds a source with the stage-start token, its expiry and
// the signing key that minted it.
func newMCPTokenSource(client mcpTokenClient, runID, token string, expiry time.Time, key *upload.IssuedKey, logSink io.Writer) *mcpTokenSource {
	if logSink == nil {
		logSink = io.Discard
	}
	return &mcpTokenSource{
		client:      client,
		runID:       runID,
		logSink:     logSink,
		now:         time.Now,
		token:       token,
		tokenExpiry: expiry,
		key:         key,
	}
}

// bearer returns a bearer valid for at least mcpTokenRefreshSkew, re-minting
// the signing key and then the token when needed. It is BEST-EFFORT and never
// errors: every failure path returns the stored (possibly stale) token, so the
// worst case is byte-identical to the pre-#3255 single-mint behavior.
//
// ctx is used ONLY as the parent of a detached, mcpTokenRefreshTimeout-bounded
// mint context — its cancellation is deliberately not inherited.
func (s *mcpTokenSource) bearer(ctx context.Context) string {
	// (a) No source or no token: the stage is deliberately credential-free
	// (acceptance, ADR-050 decision #2) or the initial fetch failed. Never mint.
	if s == nil {
		return ""
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.token == "" {
		return ""
	}
	now := s.now()
	// (b) Still fresh: no network call.
	if now.Before(s.tokenExpiry.Add(-mcpTokenRefreshSkew)) {
		return s.token
	}
	// (c) A refresh failed recently: back off, return the stale token.
	if now.Before(s.nextAttempt) {
		return s.token
	}

	mintCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), mcpTokenRefreshTimeout)
	defer cancel()

	// (d) The signing key's 30-minute TTL is SHORTER than the token's 60, so a
	// mint signed with the start-of-stage key would itself be rejected —
	// refresh the key FIRST when it is missing or near its own expiry.
	if s.key == nil || !now.Before(s.key.ExpiresAt.Add(-mcpTokenRefreshSkew)) {
		key, err := s.client.IssueKey(mintCtx, s.runID, 0)
		if err != nil {
			return s.degrade(now, "issue_key", err)
		}
		s.key = key
	}

	// (e) Mint the token with the (possibly fresh) key.
	tok, err := s.client.FetchMCPToken(mintCtx, upload.FetchMCPTokenArgs{
		RunID:      s.runID,
		PrivateKey: s.key.PrivateKey,
	})
	if err != nil {
		return s.degrade(now, "fetch_mcp_token", err)
	}
	s.token = tok.Token
	s.tokenExpiry = tok.ExpiresAt
	s.nextAttempt = time.Time{}
	_, _ = fmt.Fprintf(s.logSink,
		`{"event":"mcp_token_refreshed","run_id":%q,"token_id":%q,"expires_at":%q}`+"\n",
		s.runID, tok.TokenID, tok.ExpiresAt.Format(time.RFC3339))
	return s.token
}

// degrade logs a failed refresh, stamps the retry backoff and returns the
// stale token. Caller holds s.mu.
func (s *mcpTokenSource) degrade(now time.Time, step string, err error) string {
	s.nextAttempt = now.Add(mcpTokenRefreshRetryInterval)
	_, _ = fmt.Fprintf(s.logSink,
		`{"event":"mcp_token_refresh_degraded","run_id":%q,"step":%q,"detail":%q}`+"\n",
		s.runID, step, err.Error())
	return s.token
}

// freshMCPToken returns a refreshed run-bound bearer when a token source is
// wired, else fallback unchanged. Callers keep their own `mcpToken == ""`
// guard FIRST, so a stage that never got a token never reaches a refresh.
func (c config) freshMCPToken(ctx context.Context, fallback string) string {
	if c.mcpTokens == nil {
		return fallback
	}
	return c.mcpTokens.bearer(ctx)
}
