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
	ErrAlreadyIssued     = errors.New("upload: signing key already issued for this run")
	ErrNotFound          = errors.New("upload: run or signing key not found")
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
		HTTP:    &http.Client{Timeout: 60 * time.Second},
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
// Single-attempt: signing-key issuance is not idempotent (a second
// call returns 409 ErrAlreadyIssued), so a transient failure here
// must be surfaced rather than silently retried.
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
