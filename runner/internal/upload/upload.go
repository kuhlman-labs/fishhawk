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
	"unicode/utf8"

	"github.com/kuhlman-labs/fishhawk/runner/internal/reachability"
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
	// Re-shipping the SAME bytes won't help, but the defect is
	// RUNNER-side (a wrong/missing wire field) and the run is
	// recoverable once it is fixed — so a call site must NOT infer a
	// non-retryable failure category from this sentinel alone. The
	// correct category depends on the path: on the held-commit resume
	// path (openHeldCommitPR) the body is runner-assembled and a retry
	// invokes no agent, so it is category C (retryable); on the
	// ordinary push path pushFailureCategory keeps it category B
	// because that body carries agent-authored text and a retry costs
	// a full agent pass (#2566).
	ErrPullRequestInvalid = errors.New("upload: pull-request rejected as invalid")
	// ErrAcceptanceInvalid surfaces when the backend rejects the
	// acceptance verdict body (400 acceptance_invalid) — missing or
	// malformed fields per the ADR-049 evidence shape. Permanent at
	// the protocol layer: the agent's verdict is bad, not the network,
	// so the call site classifies it category-B (E31.7 / #1535).
	ErrAcceptanceInvalid = errors.New("upload: acceptance verdict rejected as invalid")
	ErrAlreadyIssued     = errors.New("upload: signing key already issued for this run")
	ErrNotFound          = errors.New("upload: run or signing key not found")
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
	// ErrAmendmentBudgetExhausted is returned by RequestScopeAmendment when
	// the backend responds 422 (amendment_budget_exhausted): the stage has
	// already spent its per-stage scope-amendment budget (rows are counted,
	// so denied requests consume it too). Non-retryable within the stage;
	// the caller falls open to the un-amended scope (#2748).
	ErrAmendmentBudgetExhausted = errors.New("upload: stage scope-amendment budget exhausted")
	// ErrStageNotImplement is returned by RequestScopeAmendment when the
	// backend responds 409 (stage_not_implement): the run has no executing
	// implement stage to hang the amendment off. Non-retryable; the caller
	// falls open (#2748).
	ErrStageNotImplement = errors.New("upload: run has no executing implement stage")
	// ErrScopeAmendmentForbidden is returned by RequestScopeAmendment on a
	// 403 — the token is not run-bound, is cross-run, or lacks
	// write:scope-amendments. Non-retryable; the caller falls open (#2748).
	ErrScopeAmendmentForbidden = errors.New("upload: token may not request scope amendments for this run")
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
// key, and the backend's Verify accepts a signature from ANY unexpired
// key for the run (#1872), so a sibling stage's rotation does not
// invalidate an in-flight runner's still-open upload. Older
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
	StageID             string `json:"stage_id"`
	StageType           string `json:"stage_type"`
	Prompt              string `json:"prompt"`
	PromptHash          string `json:"prompt_hash"`
	AgentTimeoutSeconds int    `json:"agent_timeout_seconds"`
	DecomposedFromRunID string `json:"decomposed_from_run_id,omitempty"`
	// SliceIndex is the decomposed child's 0-based sub_plan position
	// (E24.1 / #1141 / ADR-041), echoed by the backend only for
	// decomposed children. The runner routes the child onto its own
	// sole-writer slice branch fishhawk/run-<parent>/slice-<n>.
	// omitempty drops a 0 value: the runner reads it only when
	// DecomposedFromRunID is set, defaulting to 0 — the correct value
	// for slice 0.
	SliceIndex           int    `json:"slice_index,omitempty"`
	VerifyCommand        string `json:"verify_command,omitempty"`
	VerifyTimeoutSeconds int    `json:"verify_timeout_seconds,omitempty"`
	// VerifyMaxIterations is the verify-fix loop budget from
	// executor.verify.max_iterations. 0 (or absent) preserves the
	// single-shot demote-on-failure gate; >0 enables the bounded fix
	// loop. Decoded and threaded through but not yet consumed.
	VerifyMaxIterations int `json:"verify_max_iterations,omitempty"`
	// DiffCoverage is the stage's spec-declared `diff_coverage` constraint
	// (workflow-v1.6, ADR-059 / #1888), echoed by the backend so the runner
	// knows what coverage command to run, where it writes its report, and
	// what threshold the backend will hold the measurement to. Nil (the
	// byte-identical default) when the stage does not declare it — the
	// runner then runs no coverage command and emits no evidence, and the
	// backend's gate never fires.
	//
	// CROSS-MODULE WIRE CONTRACT: the json tags MUST stay byte-identical to
	// the backend's promptResponse.DiffCoverage / diffCoverageConfig
	// (backend/internal/server/prompt.go). Same independent-struct-by-tag
	// convention as ScopeExemptions / BindingAssertions / AgentVersionRange.
	// A tag drift silently DISABLES the measurement — the runner decodes
	// nil, runs nothing, and the gate reports nothing wrong because it never
	// ran. That is the silent-no-op failure the constraint exists to
	// eliminate, so the decode side is pinned by a test feeding the
	// BACKEND's literal json field names.
	DiffCoverage *DiffCoverageConfig `json:"diff_coverage,omitempty"`
	// MinRunnerVersion is the minimum runner version the backend requires.
	// Non-empty only when the backend is a release build. The runner compares
	// this against its own version and exits with exitVersionSkew when it is
	// older than required.
	MinRunnerVersion string `json:"min_runner_version,omitempty"`
	// AgentVersionRange is the stage executor's spec-declared agent CLI
	// compatibility range (executor.agent_version, E32.13 / #1743): a semver
	// comparator range (e.g. ">=2.1 <2.2") the workflow was validated
	// against. Non-empty only when the stage's executor declares it. Before
	// spawning the coding agent the runner compares its resolved (#1769-probed)
	// CLI version against this range and fails the stage LOUDLY pre-spawn
	// (category C, agent_version_mismatch) on an out-of-range version, or
	// degrades to a warn and proceeds when the version is unprobeable/unknown.
	// Empty (the byte-identical default) means no constraint — no check runs,
	// mirroring the MinRunnerVersion precedent.
	//
	// CROSS-MODULE WIRE CONTRACT: the json tag (min/agent_version_range) MUST
	// stay byte-identical to the backend's promptResponse.AgentVersionRange
	// (backend/internal/server/prompt.go). Same independent-struct-by-tag
	// convention as MinRunnerVersion / ImplementModel. A tag drift silently
	// disables the pre-spawn compatibility check.
	AgentVersionRange string `json:"agent_version_range,omitempty"`
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
	// BindingAssertions is the operator-declared binding-assertion list
	// (#1171), echoed by the backend on implement stages only when an
	// approved plan declared them. Each entry is a deterministic,
	// post-implement substring check the runner evaluates against the
	// committed scope-only tree before the push; an unsatisfied assertion
	// fails the stage category-B. Empty (the byte-identical default) when
	// no assertions were declared — the gate is a no-op. The json tags
	// (type/path/literal) are byte-identical to the backend's
	// bindingAssertion prompt-response struct so the declaration
	// round-trips approve-request → audit payload → prompt-response →
	// this decoder unchanged.
	BindingAssertions []BindingAssertion `json:"binding_assertions,omitempty"`
	// ScopeExemptions is the operator-declared exempt_scope_files list
	// (#1229), echoed by the backend on implement stages only when the run's
	// recovery (resume_run exempt_scope_files) recorded operator-justified
	// exemptions on its plan_reused_from provenance. Each entry marks a
	// declared scope.files path the operator justified as unchanged, so the
	// runner SUBTRACTS it from the #1151 scope-completeness shortfall exactly
	// like a #1153 agent self-exemption — it does NOT widen scope (no
	// scope-amendment row). Empty (the byte-identical default) on every
	// non-recovery run — the gate is the strict #1151 default. The json tags
	// (scope_exemptions/path/reason) are byte-identical to the backend's
	// promptResponse.ScopeExemptions / scopeExemption prompt-response struct,
	// the runner↔backend wire contract for the exemption round-trip (the same
	// cross-module convention as BindingAssertions/#1171 and add_scope_files/
	// #824). CRITICAL: these arrive in the prompt, not the consumable #1153
	// sidecar, so the runner holds them in a separate var that survives the
	// base-rebase re-invoke reset.
	ScopeExemptions []ScopeExemption `json:"scope_exemptions,omitempty"`
	// CommitAuthorName / CommitAuthorEmail are the GitHub App bot
	// account's git commit identity, resolved backend-side from the App
	// (slug + bot user-id) so App-backed commits attribute to the App's
	// bot account (#722). Empty when the backend couldn't resolve it (no
	// App JWT, dev/CLI) — the runner falls back to
	// gitops.DefaultAuthorName/DefaultAuthorEmail.
	CommitAuthorName  string `json:"commit_author_name,omitempty"`
	CommitAuthorEmail string `json:"commit_author_email,omitempty"`
	// ImplementModel is the backend-resolved implement model id (#1013),
	// echoed by the backend on an implement-stage prompt so the runner pins
	// the agent spawn to it (the claudecode adapter appends `--model
	// <ImplementModel>` when set). The backend resolves it through the
	// implement-model ladder: operator gate decision > plan
	// model_recommendation.implement_model > spec executor.model > deployment
	// default. EMPTY/omitted (the common case on every run today) means no
	// rung supplied a model, and the runner leaves agent.Invocation.Model
	// unset so the adapter spawns on its built-in default — byte-identical to
	// today's spawn.
	//
	// CROSS-MODULE WIRE CONTRACT: the json tag (implement_model) MUST stay
	// byte-identical to the backend's promptResponse.ImplementModel
	// (backend/internal/server/prompt.go) so the wire value round-trips. Same
	// independent-struct-by-tag convention as ScopeExemptions (#1229) and
	// BindingAssertions (#1171). A tag drift silently drops the model and the
	// runner falls back to today's spawn.
	ImplementModel string `json:"implement_model,omitempty"`
	// PlanModel is the backend-resolved plan model id (#1416), echoed by the
	// backend on a plan-stage prompt so the runner pins the plan agent spawn to
	// it (the claudecode adapter appends `--model <PlanModel>` when set), reusing
	// the same FetchedPrompt -> agent.Invocation.Model seam as ImplementModel.
	// The backend resolves it through the plan-model ladder: spec executor.model
	// (plan stage) > deployment default, with the operator gate rung added by a
	// sibling slice. EMPTY/omitted (the common case on every run today) means no
	// rung supplied a model, and the runner leaves agent.Invocation.Model unset so
	// the adapter spawns on its built-in default — byte-identical to today's spawn.
	//
	// CROSS-MODULE WIRE CONTRACT: the json tag (plan_model) MUST stay
	// byte-identical to the backend's promptResponse.PlanModel
	// (backend/internal/server/prompt.go) so the wire value round-trips. Same
	// independent-struct-by-tag convention as ImplementModel. A tag drift silently
	// drops the model and the runner falls back to today's spawn.
	PlanModel string `json:"plan_model,omitempty"`
	// Fixup is true when this implement stage is an operator-triggered
	// implement-review fix-up pass (sub-plan A / #762). A fix-up re-runs
	// the implement agent against the selected concerns and commits the
	// result onto the EXISTING PR branch (FixupBranch) via the runner's
	// RebaseFromRemote path, UPDATING the open PR rather than opening a
	// new one — distinct from a fresh implement stage. Both false/empty
	// on a normal implement stage; the runner's branch routing falls
	// through to the per-stage branch unchanged.
	Fixup bool `json:"fixup,omitempty"`
	// FixupBranch is the existing PR branch a fix-up pass commits onto.
	// Non-empty only when Fixup is true. The runner checks out + rebases
	// this branch (fetch + pull --rebase) and pushes the fix-up commit so
	// the open PR's head advances; it does NOT call OpenPR.
	FixupBranch string `json:"fixup_branch,omitempty"`
	// FixupExpectedHeadSHA is the run's recorded head — the newest
	// reported head_sha in the backend's lineage ledger. Non-empty only
	// when Fixup is true. Before invoking the agent on a fix-up pass the
	// runner fetches + checks out FixupBranch and verifies the fetched tip
	// equals this SHA (ADR-035, #967), failing fast on a mismatch. Empty
	// (older backend, or backend-side resolution failure) means the runner
	// skips the comparison and proceeds with checkout only.
	FixupExpectedHeadSHA string `json:"fixup_expected_head_sha,omitempty"`
	// FixupApplyPatches is the near-deterministic apply-list (#1165): one
	// reviewer-emitted unified diff per routed concern, in routing order.
	// Non-empty ONLY on a fix-up dispatch whose routed concerns EVERY carry a
	// suggested_patch (the backend's all-or-nothing eligibility gate). The
	// runner then attempts `git apply --3way` of each patch against the
	// already-checked-out PR branch and, on a clean apply that passes the
	// committed-tree verify gate, commits/pushes via the existing fixup_pushed
	// path WITHOUT spawning the agent. ANY apply or verify failure, or an empty
	// list (a routed concern lacked a patch), sends the runner down the
	// unchanged agent fix-up path. Empty on a normal implement dispatch and on
	// a non-eligible fix-up — byte-identical to today. The wire tag
	// (fixup_apply_patches / patch) matches the backend's fixupApplyPatch shape.
	FixupApplyPatches []FixupApplyPatch `json:"fixup_apply_patches,omitempty"`
	// OpenPRFromHeldCommit is true on an operator EXEMPT resolution of a
	// scope-completeness park (#1231): the implement stage previously parked
	// because the missing-declared-scope-file gate was its sole failure, and the
	// runner pushed the gate-verified commit to the run branch WITHOUT opening a
	// PR. The operator decided exempt, so the backend dispatches THIS stage to
	// open the PR from that exact held commit with NO agent re-invocation
	// (zero-re-run). The runner skips the agent, the gates, and CommitAndPush
	// entirely (the commit already exists on the branch) and opens the PR from
	// HeldCommitBranch at HeldCommitSHA. Both false/empty on every other
	// dispatch. The wire tags (open_pr_from_held_commit / held_commit_sha /
	// held_commit_branch) are byte-identical to the backend's prompt-response
	// scope-completeness exempt fields, the runner↔backend wire contract for the
	// exempt resolution (the same cross-module convention as ScopeExemption /
	// #1229 and BindingAssertions / #1171).
	OpenPRFromHeldCommit bool `json:"open_pr_from_held_commit,omitempty"`
	// HeldCommitSHA is the head commit the scope-completeness park pushed to the
	// run branch (#1231). Non-empty only when OpenPRFromHeldCommit is true. The
	// runner asserts the branch tip equals this SHA before opening the PR so the
	// opened PR head is byte-identical to the gate-verified held tree (ADR-035).
	HeldCommitSHA string `json:"held_commit_sha,omitempty"`
	// HeldCommitBranch is the run branch the held commit was pushed to (#1231).
	// Non-empty only when OpenPRFromHeldCommit is true. The runner opens the PR
	// with this branch as head.
	HeldCommitBranch string `json:"held_commit_branch,omitempty"`
	// HeldCommitBaseSHA is the base commit the held commit was built on (#2563).
	// Non-empty only when OpenPRFromHeldCommit is true. The runner ships it as
	// the pull-request artifact's base_sha, which the backend's success-arm
	// validate() REQUIRES — without it the exempt resume opens the PR and then
	// always fails category-B, orphaning the PR (#2562). When this is empty on an
	// exempt dispatch the runner refuses the resume BEFORE calling the forge
	// (category-C, no orphaned PR) rather than opening one it cannot ship. The
	// wire tag (held_commit_base_sha) is byte-identical to the backend's
	// prompt-response promptResponse.HeldCommitBaseSHA — the same cross-module
	// convention as the three exempt fields above.
	HeldCommitBaseSHA string `json:"held_commit_base_sha,omitempty"`
	// HeldCommitResumeKind DISCRIMINATES which recovery the held-commit trio
	// above encodes (#2169). Empty is the legacy #1231 scope-completeness EXEMPT
	// resolution, byte-identical to every dispatch before this field existed.
	// "pr_open" is the PR-open CHECKPOINT resume: the implement stage already
	// committed and PUSHED (CommitAndPush succeeded) and then failed opening the
	// PR or shipping the artifact — typically a sustained forge outage — so the
	// backend re-dispatches the stage to re-attempt ONLY the idempotent
	// adopt-then-create OpenPR (#2167) from the pushed head, with no agent
	// re-invocation. The two kinds differ in the runner's behavior: a "pr_open"
	// resume additionally verifies the run branch's REMOTE TIP still equals
	// HeldCommitSHA before touching the forge (the held commit was never
	// operator-reviewed at a park gate, so a branch moved by
	// fishhawk_reset_run_branch must not produce an artifact whose head_sha
	// lies), and re-reports the checkpoint on its own failure so a multi-retry
	// outage never degrades back to a full agent re-run.
	//
	// The wire tag (held_commit_resume_kind) is byte-identical to the backend's
	// promptResponse.HeldCommitResumeKind — the same cross-module
	// independent-struct-by-tag convention as the four fields above. omitempty,
	// so a legacy exempt dispatch and every non-resume dispatch stay
	// byte-identical to today.
	HeldCommitResumeKind string `json:"held_commit_resume_kind,omitempty"`
	// HeldCommitPRTitle and HeldCommitPRBody are the pull-request text the
	// held-commit resume opens its PR with (#2570) — the agent-authored title +
	// body recovered from the park row / checkpoint payload, or the backend's
	// documented issue-context fallback when no agent text survived. Served ONLY
	// alongside the held-commit trio above; empty on every other dispatch.
	//
	// PER-FIELD, NOT ALL-OR-NOTHING (#2570 binding condition 4): each is used
	// independently, so a recovered title is never discarded because the body was
	// empty. The runner falls back to its own placeholder only for whichever
	// field is empty.
	//
	// HeldCommitPRBody EXCLUDES the Fishhawk attribution footer: the opening
	// runner owns that footer and appends it exactly once (see HeldCommitPRText).
	//
	// CROSS-MODULE WIRE CONTRACT: the json tags MUST stay byte-identical to the
	// backend's promptResponse.HeldCommitPRTitle / HeldCommitPRBody
	// (backend/internal/server/prompt.go). Same independent-struct-by-tag
	// convention as the held-commit trio. A tag drift silently degrades the
	// resume back to the placeholder — the exact defect #2570 fixes.
	HeldCommitPRTitle string `json:"held_commit_pr_title,omitempty"`
	HeldCommitPRBody  string `json:"held_commit_pr_body,omitempty"`
	// EgressTargetHosts is the acceptance stage's full spec-declared
	// egress.target_hosts list (E31.4 / #1532 grammar), served ONLY on
	// acceptance-stage prompt responses (E31.7 / #1535). It is the
	// customer-controlled slot of the ADR-050 egress-proxy allow-list:
	// the runner composes egressproxy.BuildAllowlist(EgressTargetHosts,
	// backendURL) before spawning the acceptance agent. Empty on every
	// other stage type and on a spec with no egress block — the proxy
	// then admits only the model + backend hosts (fail-closed posture).
	//
	// CROSS-MODULE WIRE CONTRACT: the json tag (egress_target_hosts) MUST
	// stay byte-identical to the backend's promptResponse.EgressTargetHosts
	// (backend/internal/server/prompt.go). Same independent-struct-by-tag
	// convention as ImplementModel (#1013) and ScopeExemptions (#1229). A
	// tag drift silently drops the hosts and the acceptance agent cannot
	// reach its target — fail-closed, but loud only via the proxy denials.
	EgressTargetHosts []string `json:"egress_target_hosts,omitempty"`
	// AcceptanceCriteriaIDs is the approved plan's
	// verification.acceptance_criteria ids in plan order, served ONLY on
	// acceptance-stage prompt responses (E31.7 / #1535). The runner
	// validates the shipped verdict's criteria[].id join keys against
	// this served set — an unknown id fails closed (category-B) rather
	// than pinning evidence to a criterion the plan never declared.
	// Empty when the run has no approved plan or the plan declares no
	// criteria; the runner then skips the membership check.
	//
	// CROSS-MODULE WIRE CONTRACT: the json tag (acceptance_criteria_ids)
	// MUST stay byte-identical to the backend's
	// promptResponse.AcceptanceCriteriaIDs
	// (backend/internal/server/prompt.go), the same convention as
	// EgressTargetHosts above.
	AcceptanceCriteriaIDs []string `json:"acceptance_criteria_ids,omitempty"`
	// AcceptanceExpectedHeadSHA is the run's merge-candidate identity — the
	// newest reported head_sha in the backend's lineage ledger (the same
	// source as FixupExpectedHeadSHA) — served ONLY on acceptance-stage
	// prompt responses (E31.18 / #1569). The pre-spawn target-identity gate
	// compares the declared target's /healthz git_sha against it before
	// spawning the acceptance agent, so acceptance validates the merge
	// candidate rather than whatever build answers at the declared host.
	// Empty (older backend, or backend-side ledger resolution failure) means
	// the gate treats the target as unverifiable and warns-and-proceeds
	// rather than blocking the stage. Decoded here; the gate consumer in
	// main.go's acceptance pre-spawn block is the E31.18 sibling slice.
	//
	// CROSS-MODULE WIRE CONTRACT: the json tag
	// (acceptance_expected_head_sha) MUST stay byte-identical to the
	// backend's promptResponse.AcceptanceExpectedHeadSHA
	// (backend/internal/server/prompt.go), the same convention as
	// EgressTargetHosts above. A tag drift silently drops the expectation
	// and the identity gate degrades to unverifiable-warn on every dispatch.
	AcceptanceExpectedHeadSHA string `json:"acceptance_expected_head_sha,omitempty"`
}

// FixupApplyPatch is one entry in FetchedPrompt.FixupApplyPatches: a single
// routed concern's reviewer-emitted unified diff (#1165). The patch carries its
// own file paths in the diff headers. The json tag (patch) is byte-identical to
// the backend's fixupApplyPatch struct, the runner↔backend wire contract for
// the deterministic apply-list.
type FixupApplyPatch struct {
	Patch string `json:"patch"`
}

// ScopeFile is one entry in FetchedPrompt.ScopeFiles: a declared path
// plus its per-file operation (create/modify/delete). Mirrors the
// backend's scope_files response shape and the standard_v1 plan
// scope.files entries.
type ScopeFile struct {
	Path      string `json:"path"`
	Operation string `json:"operation"`
}

// BindingAssertion is one entry in FetchedPrompt.BindingAssertions: a
// typed, operator-declared deterministic substring check (#1171). v0
// types are file_contains and test_asserts (the type field is a plain
// string — an open enum validated backend-side at declaration time, so
// the runner treats both identically and a future type decodes without a
// wire-shape break). The json tags (type/path/literal) are byte-identical
// to the backend's bindingAssertion struct, the runner↔backend wire
// contract for the declaration round-trip.
type BindingAssertion struct {
	Type    string `json:"type"`
	Path    string `json:"path"`
	Literal string `json:"literal"`
}

// ScopeExemption is one entry in FetchedPrompt.ScopeExemptions: an
// operator-justified declared-scope.files path the runner subtracts from the
// #1151 scope-completeness shortfall (#1229). The json tags (path/reason) are
// byte-identical to the backend's scopeExemption struct, the runner↔backend
// wire contract for the exempt_scope_files round-trip (approve-request →
// plan_reused_from audit payload → prompt-response → this decoder). This
// mirrors the BindingAssertion (#1171) and ScopeFile (#824) cross-module
// convention: each module defines its own struct agreeing by json tag, so a
// tag drift surfaces in review against this documented counterpart.
type ScopeExemption struct {
	Path   string `json:"path"`
	Reason string `json:"reason"`
}

// DiffCoverageConfig is the stage's spec-declared `diff_coverage`
// constraint (workflow-v1.6, ADR-059 / #1888) as the backend echoes it.
//
// Format empty means "lcov" — the schema default and the only v1.6 member.
// BaseRef empty means the run's base branch, which the RUNNER resolves at
// measurement time (--base-branch > GITHUB_REF_NAME > "main"); an empty ref
// never reaches git.
//
// The json tags are byte-identical to the backend's diffCoverageConfig, the
// same cross-module convention as ScopeExemption / BindingAssertion /
// ScopeFile: each module defines its own struct agreeing by json tag, so a
// drift surfaces in review against this documented counterpart — and is
// caught by the decode test that feeds the backend's literal field names.
type DiffCoverageConfig struct {
	Command            string `json:"command"`
	ReportPath         string `json:"report_path"`
	Format             string `json:"format,omitempty"`
	MinNewLineCoverage int    `json:"min_new_line_coverage"`
	BaseRef            string `json:"base_ref,omitempty"`
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

// Failure-report body caps (#1791). The backend's POST
// /v0/runs/{id}/pull-request and reap-failure endpoints both cap the request
// body at 32*1024 bytes (backend/internal/server/pullrequest.go
// maxPullRequestBundleBytes and reap_failure.go maxReapFailureBodyBytes) and
// reject an oversized body 413 body_too_large. A category-B implement failure
// whose reason embedded the entire multi-module verify output blew past that
// cap, so the in-band failure report never landed and the stage was stranded
// 'running'. MaxFailureReportReasonBytes bounds a normal 'failed' report's
// reason with headroom under 32*1024 for the JSON envelope;
// AggressiveFailureReportReasonBytes is the far smaller cap the bounded post-4xx
// retry re-marshals with when even the normal cap was still rejected.
const (
	MaxFailureReportReasonBytes        = 30 * 1024
	AggressiveFailureReportReasonBytes = 2 * 1024
)

// Held-commit PR-text caps (#2570). A park / checkpoint report now carries the
// agent-authored PR title + body so a held-commit RESUME opens a real pull
// request instead of the `chore: fishhawk implement stage <id>` placeholder.
// That text is agent-authored and unbounded, so it must be budgeted against the
// SAME 32*1024 request-body cap the reason is: an oversized body would 413
// body_too_large and strand the stage in `running` — strictly worse than the
// placeholder this carries the text to avoid.
//
// MaxPullRequestReportBytes MIRRORS the backend's maxPullRequestBundleBytes
// (backend/internal/server/pullrequest.go). The backend rejects a body STRICTLY
// GREATER than that, so a report marshalling to exactly this many bytes is
// accepted. MinFailureReportReasonBytes is the floor the reason yields down to
// before the PR text starts being dropped — below it the operator's failure
// diagnosis stops being useful.
// MinHeldCommitPRBodyBytes is the floor the recovered PR body TRUNCATES toward
// before it is dropped wholesale. A body that no longer fits is shrunk to the
// largest fitting prefix rather than deleted, so an escape-heavy `## Summary`
// still reaches the resume instead of degrading it to the synthesized fallback;
// below this floor the surviving fragment is not worth the bytes and the whole
// field yields to the reason.
const (
	MaxHeldCommitPRTitleBytes   = 300
	MaxHeldCommitPRBodyBytes    = 8 * 1024
	MinHeldCommitPRBodyBytes    = 512
	MinFailureReportReasonBytes = 4 * 1024
	MaxPullRequestReportBytes   = 32 * 1024
)

// heldCommitPRBodyTruncationMarker is appended to a PR body the size budget
// truncated, so the operator reading the resumed pull request can tell the
// narrative is a fragment rather than everything the agent wrote.
const heldCommitPRBodyTruncationMarker = "\n\n_[truncated by the runner to fit the report size cap]_"

// clampRunes bounds s to at most max BYTES, cutting back to a rune boundary
// (#2570). Rune-safety is load-bearing for the size budget, not just for
// correctness of the text: a cut that split a multi-byte character would leave
// invalid UTF-8, and encoding/json replaces each invalid byte with U+FFFD —
// THREE bytes on the wire for one — so a byte-exact clamp could GROW the
// serialized payload it exists to bound.
func clampRunes(s string, max int) string {
	if max <= 0 {
		return ""
	}
	if len(s) <= max {
		return s
	}
	cut := max
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut]
}

// marshalOverflow reports how many bytes v's JSON encoding exceeds max by
// (negative or zero when it fits), and false when v cannot be marshalled.
//
// MEASURING THE SERIALIZED FORM IS THE WHOLE POINT (#2570). Budgeting raw
// string lengths is wrong: encoding/json HTML-escapes by default, so each `<`,
// `>` and `&` becomes a six-byte `<`-style escape, a backslash doubles, and
// a control character expands to `\uXXXX`. A title/body/reason set that measures
// comfortably under the cap by len() can be several times over it once
// marshalled — which is exactly the 413 this budget exists to prevent.
func marshalOverflow(v any, max int) (int, bool) {
	b, err := json.Marshal(v)
	if err != nil {
		return 0, false
	}
	return len(b) - max, true
}

// shrinkHeldCommitPRBody shrinks the recovered PR body *prBody carries, in
// place, until envelope's MARSHALLED form fits maxBytes — reporting whether it
// now fits.
//
// TRUNCATE BEFORE DROPPING (#2570). The captured `## Summary` is the whole point
// of carrying PR text through a park: dropping it wholesale because its ESCAPED
// form overshot the budget degrades the resume to the synthesized fallback even
// though a shorter prefix would have fit.
//
// The retained length is found by BINARY SEARCH over the original text, each
// candidate MEASURED as marshalled bytes (see marshalOverflow) rather than
// estimated from raw lengths — the escape expansion is per-character and wildly
// non-uniform (six bytes for `<`, one for a letter), so no arithmetic on raw
// lengths can predict where the cut lands. The search keeps the LARGEST fitting
// prefix; a single overflow-sized cut would converge on the floor and throw away
// narrative that fits.
//
// Every candidate is cut on a rune boundary (clampRunes), because invalid UTF-8
// is re-encoded as a THREE-byte U+FFFD per bad byte and could GROW the payload
// this bounds. When not even MinHeldCommitPRBodyBytes of retained text fits, the
// original is restored and false is reported, leaving the field for the caller's
// drop rung.
func shrinkHeldCommitPRBody(envelope any, prBody *string, maxBytes int) bool {
	fits := func() bool {
		over, ok := marshalOverflow(envelope, maxBytes)
		return ok && over <= 0
	}
	if fits() {
		return true
	}

	original := *prBody
	lo, hi := MinHeldCommitPRBodyBytes, len(original)
	best := -1
	for lo <= hi {
		mid := lo + (hi-lo)/2
		*prBody = clampRunes(original, mid) + heldCommitPRBodyTruncationMarker
		if fits() {
			best = mid
			lo = mid + 1
		} else {
			hi = mid - 1
		}
	}
	if best < 0 {
		*prBody = original
		return false
	}
	*prBody = clampRunes(original, best) + heldCommitPRBodyTruncationMarker
	return true
}

// shrinkFailureBodyFields shrinks a failure report in place until its MARSHALLED
// form fits maxBytes, returning false only when even the emptied-field envelope
// does not fit (#2570). The ladder yields the least valuable field first:
//
//  0. Apply the nominal clamps unconditionally — reason to
//     MaxFailureReportReasonBytes (today's behavior, byte-for-byte), PR title and
//     body to their own caps. A report that fits after this rung is byte-identical
//     to what the pre-#2570 runner shipped whenever the PR text is absent.
//  1. Yield the REASON down toward MinFailureReportReasonBytes, shrinking by the
//     MEASURED marshalled overflow each pass (never by a raw-length delta — see
//     marshalOverflow) and re-measuring, so the loop converges on the real
//     serialized size however the text escapes.
//  2. TRUNCATE the PR body to the largest prefix that fits, measured the same
//     way (shrinkHeldCommitPRBody). A fragment of the agent's `## Summary` is
//     worth far more to the resumed pull request than the synthesized fallback
//     dropping it entirely would force.
//  3. Drop the PR body, then the PR title. Recovered narrative is the expendable
//     field: losing it costs a placeholder PR, losing the reason costs the
//     operator the diagnosis of why the stage failed at all.
//  4. Shrink the reason BELOW its floor, halving until it fits or empties. Only
//     reachable when the envelope alone (outcome/category/checkpoint SHAs) plus a
//     4 KiB reason still exceeds the cap.
//  5. Shrink exhausted: even the empty-field envelope is over. Report false so
//     the caller ships the un-shrinkable body and lets the backend's 413 (and the
//     bounded aggressive retry) decide, rather than silently claiming a fit.
func shrinkFailureBodyFields(body *pullRequestFailureBody, maxBytes int) bool {
	body.Reason = TruncateReason(body.Reason, MaxFailureReportReasonBytes)
	body.Title = clampRunes(body.Title, MaxHeldCommitPRTitleBytes)
	body.PRBody = clampRunes(body.PRBody, MaxHeldCommitPRBodyBytes)

	fits := func() bool {
		over, ok := marshalOverflow(body, maxBytes)
		return ok && over <= 0
	}
	if fits() {
		return true
	}

	// Rung 1: the reason yields to its floor.
	for {
		over, ok := marshalOverflow(body, maxBytes)
		if !ok {
			return false
		}
		if over <= 0 {
			return true
		}
		next := len(body.Reason) - over
		if next < MinFailureReportReasonBytes {
			next = MinFailureReportReasonBytes
		}
		if next >= len(body.Reason) {
			break
		}
		body.Reason = TruncateReason(body.Reason, next)
	}

	// Rung 2: the recovered PR body truncates to what fits.
	if shrinkHeldCommitPRBody(body, &body.PRBody, maxBytes) {
		return true
	}

	// Rung 3: drop the recovered PR text.
	body.PRBody = ""
	if fits() {
		return true
	}
	body.Title = ""
	if fits() {
		return true
	}

	// Rung 4: the reason yields below its floor.
	for n := MinFailureReportReasonBytes / 2; n >= 1; n /= 2 {
		body.Reason = TruncateReason(body.Reason, n)
		if fits() {
			return true
		}
	}
	body.Reason = ""

	// Rung 5: shrink exhausted.
	return fits()
}

// shrinkParkBodyFields is the park-report sibling of shrinkFailureBodyFields
// (#2570). A park body carries no reason, so the ladder is the two PR-text
// clamps, then TRUNCATING the body to what fits, then dropping the body and the
// title when the shortfall lists alone push the marshalled form past the cap.
// Returns false when even the PR-text-free body is over — the shortfall lists
// are the park's whole payload and are never dropped, so that case is the
// caller's to surface.
func shrinkParkBodyFields(body *pullRequestScopeParkBody, maxBytes int) bool {
	body.Title = clampRunes(body.Title, MaxHeldCommitPRTitleBytes)
	body.PRBody = clampRunes(body.PRBody, MaxHeldCommitPRBodyBytes)

	fits := func() bool {
		over, ok := marshalOverflow(body, maxBytes)
		return ok && over <= 0
	}
	if fits() {
		return true
	}
	if shrinkHeldCommitPRBody(body, &body.PRBody, maxBytes) {
		return true
	}
	body.PRBody = ""
	if fits() {
		return true
	}
	body.Title = ""
	return fits()
}

// TruncateReason bounds s to at most max bytes for a failure-report field
// (#1791). When s already fits it is returned byte-identical. Otherwise the
// middle is elided — a head + a "\n… [truncated N bytes] …\n" marker + a tail —
// so BOTH the leading failure classification and the trailing verify summary
// survive, and the returned length never exceeds max. The full untruncated text
// still reaches the trace bundle and the local log; only the wire report shrinks.
func TruncateReason(s string, max int) string {
	if max <= 0 {
		return ""
	}
	if len(s) <= max {
		return s
	}
	const markerFmt = "\n… [truncated %d bytes] …\n"
	// Size the head/tail budget against the marker rendered with the LARGEST
	// possible elided count (len(s)); the real marker cannot have more digits,
	// so the final head+marker+tail can only be <= max.
	upper := fmt.Sprintf(markerFmt, len(s))
	keep := max - len(upper)
	if keep <= 0 {
		// max is too small to fit even the marker — hard byte-truncate so the
		// contract (len(result) <= max) still holds.
		return s[:max]
	}
	head := keep / 2
	tail := keep - head
	elided := len(s) - head - tail
	marker := fmt.Sprintf(markerFmt, elided)
	return s[:head] + marker + s[len(s)-tail:]
}

// agentOutputInvalidCodes is the set of backend 400 error codes that mean
// "the AGENT's output is bad", as opposed to "the request was bad". Every one
// of them is already a category-B stage failure on the backend side — the
// handler transitions the stage to failed-B BEFORE writing the 400 — so the
// runner must agree, or the two halves of one event disagree on the category.
//
// Sources (backend): plan_invalid and clarification_request_invalid in
// backend/internal/server/plan.go; grooming_report_invalid and
// grooming_report_stage_invalid in backend/internal/server/grooming_report.go.
// All four arrive on the SAME endpoint — POST /v0/runs/{run_id}/plan routes by
// the artifact's top-level "kind" discriminator (#2833).
//
// This is a NAMED list, not a blanket: an unlisted 400 (validation_failed on a
// malformed path or query) stays a generic error and maps to category-C.
var agentOutputInvalidCodes = map[string]struct{}{
	"plan_invalid":                  {},
	"grooming_report_invalid":       {},
	"grooming_report_stage_invalid": {},
	"clarification_request_invalid": {},
}

// isAgentOutputInvalid reports whether a 400 response body names one of the
// agent-output-invalid codes.
//
// It EXACT-MATCHES the code out of the backend's error envelope
// ({"error":{"code":…,"message":…}}, backend/internal/server/errors.go) rather
// than substring-scanning the whole body, because `message` is FREE TEXT: a
// validation_failed response whose human-readable message merely mentions
// plan_invalid would otherwise be misclassified permanent-B and never retried.
//
// FALLBACK: when the body does not decode into that envelope with a non-empty
// code — a truncated body, a proxy-injected error page, or an older/flat
// {"code":…} shape — it degrades to the historical substring check. The
// fallback exists so a classification never gets WEAKER than it was before
// exact-match landed: losing category-B on a genuinely-invalid artifact would
// turn a permanent failure into a retry loop against a backend that already
// failed the stage. It is the strictly-less-precise path and only runs when
// the precise one has nothing to read.
func isAgentOutputInvalid(body string) bool {
	var env struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(body), &env); err == nil && env.Error.Code != "" {
		_, ok := agentOutputInvalidCodes[env.Error.Code]
		return ok
	}
	for code := range agentOutputInvalidCodes {
		if strings.Contains(body, code) {
			return true
		}
	}
	return false
}

// classifyBodyLimit bounds the body read used for error-code classification.
// It is larger than briefBodyLimit because the envelope's `message` and
// `details` sit between the code and the closing brace: a 256-byte read
// truncates a verbose rejection mid-JSON, which would fail the decode and push
// every such response onto the substring fallback.
const classifyBodyLimit = 8 << 10

// briefBodyLimit is the excerpt length surfaced in client error messages.
const briefBodyLimit = 256

// readClassifiableBody reads up to classifyBodyLimit bytes of resp.Body and
// returns (brief, full). brief is the trimmed first briefBodyLimit bytes —
// byte-identical to readBriefBody's output, so error-message shape is
// unchanged. full is the larger read, used ONLY for decoding the error
// envelope. Caller is responsible for closing resp.Body.
func readClassifiableBody(resp *http.Response) (brief, full string) {
	b, _ := io.ReadAll(io.LimitReader(resp.Body, classifyBodyLimit))
	head := b
	if len(head) > briefBodyLimit {
		head = head[:briefBodyLimit]
	}
	return string(bytes.TrimSpace(head)), string(bytes.TrimSpace(b))
}

// readBriefBody returns the first 256 bytes of resp.Body as a
// string, useful for surfacing backend error envelopes in client
// errors without unbounded log lines. Caller is responsible for
// closing resp.Body.
func readBriefBody(resp *http.Response) string {
	r := io.LimitReader(resp.Body, briefBodyLimit)
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

// ReachabilityHeader is the request header ShipPlan carries the advisory
// symbol-reachability sweep result in (#2056, E50.4). The result CANNOT ride
// inside the POST body: the body is the standard_v1 plan artifact, verified
// against the Ed25519 signature and stored verbatim, so any extra bytes would
// corrupt the plan. It rides in this header instead — an advisory side channel
// the server reads fail-open. The header name MUST stay byte-identical to the
// backend's reachabilityHeader constant (backend/internal/server/
// plan_reachability.go); a drift silently disables the advisory.
const ReachabilityHeader = "X-Fishhawk-Plan-Reachability"

// maxReachabilityHeaderBytes caps the serialized reachability header ShipPlan
// will attach (#2056). The advisory MUST NEVER break the plan upload: a
// pathologically large violation list that would overflow the server's header
// limit and fail the whole request is dropped here instead, so the plan still
// ships without its advisory. 16 KiB is ample for a real decomposition's
// per-phase counts + violations while staying well under the server's header
// budget.
const maxReachabilityHeaderBytes = 16 * 1024

// ShipPlanArgs collects everything ShipPlan needs.
type ShipPlanArgs struct {
	RunID   string
	StageID string
	// Plan is the JSON bytes of the standard_v1 plan artifact.
	// Caller is responsible for reading the file from --plan-out
	// and validating it locally before shipping.
	Plan       []byte
	PrivateKey ed25519.PrivateKey

	// Reachability is the optional symbol-reachability sweep result (#2056,
	// E50.4) the runner computed against the plan's split_proposal. When
	// non-nil ShipPlan serializes it to compact JSON and sends it in the
	// ReachabilityHeader header — an ADVISORY side channel that never touches
	// the signed plan body. nil (the common case: a plan with no
	// split_proposal, or a fail-open Analyze skip) sends no header.
	//
	// CROSS-MODULE WIRE CONTRACT: reachability.Result's json tags are the
	// runner→server wire contract; the backend owns a mirroring decode struct
	// (backend/internal/server.PlanReachabilityPayload) it cannot import across
	// the module boundary, so a tag drift silently fails the advisory open.
	// Locked by the exact-wire-key serialization test in upload_test.go.
	Reachability *reachability.Result
}

// reachabilityHeaderValue serializes args.Reachability to the compact JSON the
// ReachabilityHeader carries, or "" when there is nothing to send (nil result
// or a marshal failure). It is FAIL-OPEN by construction: it never lets the
// advisory break the plan upload.
//
// When the full result exceeds maxReachabilityHeaderBytes it is NOT discarded
// wholesale (#2056 fixup). Dropping a VALID sweep whose violation list is large
// enough to overflow the header budget would deny the operator the per-phase
// counts AND every named cross-boundary violation for exactly the leaky
// partitions the advisory exists to surface. Instead a REDUCED payload ships:
// the per-phase counts (bounded by phase count) plus the largest prefix of
// violations that still fits under the cap. The per-phase DerivedCount already
// reflects the FULL leak, so the counts stay accurate even when the
// named-violation list is trimmed. Only a pathological payload that overflows
// even with zero violations falls back to "" (header-less, upload proceeds).
func reachabilityHeaderValue(res *reachability.Result) string {
	if res == nil {
		return ""
	}
	b, err := json.Marshal(res)
	if err != nil {
		return ""
	}
	if len(b) <= maxReachabilityHeaderBytes {
		return string(b)
	}

	// Oversized: keep the phases and binary-search the largest violation prefix
	// that still fits. Re-slicing res.Violations never mutates the caller's
	// slice contents, and `reduced` is a shallow struct copy so the search
	// leaves the original Result untouched.
	reduced := *res
	lo, hi := 0, len(res.Violations)
	for lo < hi {
		mid := (lo + hi + 1) / 2
		reduced.Violations = res.Violations[:mid]
		rb, rerr := json.Marshal(&reduced)
		if rerr == nil && len(rb) <= maxReachabilityHeaderBytes {
			lo = mid
		} else {
			hi = mid - 1
		}
	}
	reduced.Violations = res.Violations[:lo]
	rb, rerr := json.Marshal(&reduced)
	if rerr != nil || len(rb) > maxReachabilityHeaderBytes {
		// Even the phases-only payload overflows — fail open, header-less.
		return ""
	}
	return string(rb)
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

	// Serialize the advisory reachability sweep once (fail-open: "" when there
	// is nothing to send or the payload is oversized). It rides in a header, not
	// the signed body, so it is not covered by sigHex — the server treats it as
	// advisory and never authenticates it.
	reachHeader := reachabilityHeaderValue(args.Reachability)

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
		if reachHeader != "" {
			req.Header.Set(ReachabilityHeader, reachHeader)
		}
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
			brief, full := readClassifiableBody(resp)
			_ = resp.Body.Close()
			// 400s are permanent; the body's error CODE distinguishes an
			// agent-output-invalid rejection (ErrPlanInvalid → runner
			// category-B) from everything else (validation_failed on a
			// path/query issue and friends → category-C).
			if isAgentOutputInvalid(full) {
				return nil, fmt.Errorf("%w: %s", ErrPlanInvalid, brief)
			}
			return nil, fmt.Errorf("upload: ship plan: 400: %s", brief)
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

// ShipAcceptanceArgs collects everything ShipAcceptance needs.
type ShipAcceptanceArgs struct {
	RunID   string
	StageID string
	// Body is the JSON bytes of the acceptance verdict (the ADR-049
	// evidence shape). The runner validates + redacts it locally
	// before shipping; the bytes shipped here are the bytes the
	// backend verifies the signature against and stores verbatim.
	Body       []byte
	PrivateKey ed25519.PrivateKey
}

// ShipAcceptanceResult is the (id, stage, content_hash, verdict,
// idempotent) tuple the backend echoes on 201 / 200. Idempotent=true
// means an acceptance record with this content_hash already existed
// for this stage; the backend returned it unchanged.
type ShipAcceptanceResult struct {
	ID          string `json:"id"`
	StageID     string `json:"stage_id"`
	ContentHash string `json:"content_hash"`
	Verdict     string `json:"verdict"`
	FailureMode string `json:"failure_mode,omitempty"`
	Idempotent  bool   `json:"idempotent"`
}

// ShipAcceptance signs the verdict bytes and POSTs them to
// /v0/runs/{run_id}/acceptance?stage_id=… (E31.7 / #1535). Modeled on
// ShipPlan: retries transient failures (5xx, network errors) with the
// ShipTrace backoff policy. Permanent failures bubble up:
//
//   - 400 acceptance_invalid → ErrAcceptanceInvalid (bad verdict shape;
//     category-B at the call site)
//   - 401 signature_*        → ErrSignatureRejected
//   - 404 stage/key          → ErrNotFound
//
// On 201 the backend created a fresh artifact; on 200 the upload
// matched an existing one (Result.Idempotent==true).
func (c *Client) ShipAcceptance(ctx context.Context, args ShipAcceptanceArgs) (*ShipAcceptanceResult, error) {
	if len(args.Body) == 0 {
		return nil, errors.New("upload: empty acceptance body")
	}
	if len(args.PrivateKey) != ed25519.PrivateKeySize {
		return nil, errors.New("upload: invalid private key length")
	}

	digest := sha256.Sum256(args.Body)
	signature := ed25519.Sign(args.PrivateKey, digest[:])
	sigHex := hex.EncodeToString(signature)

	endpoint := fmt.Sprintf("%s/v0/runs/%s/acceptance?stage_id=%s",
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

		req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(args.Body))
		if err != nil {
			return nil, fmt.Errorf("upload: build acceptance request: %w", err)
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Fishhawk-Signature", sigHex)
		req.Header.Set("Accept", "application/json")
		req.ContentLength = int64(len(args.Body))

		resp, err := c.HTTP.Do(req)
		if err != nil {
			lastErr = fmt.Errorf("upload: ship acceptance: %w", err)
			continue
		}

		switch {
		case resp.StatusCode == http.StatusCreated || resp.StatusCode == http.StatusOK:
			var out ShipAcceptanceResult
			err := json.NewDecoder(resp.Body).Decode(&out)
			_ = resp.Body.Close()
			if err != nil {
				return nil, fmt.Errorf("upload: decode acceptance response: %w", err)
			}
			return &out, nil
		case resp.StatusCode == http.StatusBadRequest:
			detail := readBriefBody(resp)
			_ = resp.Body.Close()
			// 400s are permanent; the body distinguishes acceptance_invalid
			// (bad verdict shape) from validation_failed (path/query issues).
			if strings.Contains(detail, "acceptance_invalid") {
				return nil, fmt.Errorf("%w: %s", ErrAcceptanceInvalid, detail)
			}
			return nil, fmt.Errorf("upload: ship acceptance: 400: %s", detail)
		case resp.StatusCode == http.StatusUnauthorized:
			detail := readBriefBody(resp)
			_ = resp.Body.Close()
			return nil, fmt.Errorf("%w: %s", ErrSignatureRejected, detail)
		case resp.StatusCode == http.StatusNotFound:
			_ = resp.Body.Close()
			return nil, ErrNotFound
		case resp.StatusCode >= 500:
			lastErr = statusError("ship acceptance", resp)
			_ = resp.Body.Close()
			continue
		default:
			lastErr = statusError("ship acceptance", resp)
			_ = resp.Body.Close()
			return nil, lastErr
		}
	}
	return nil, fmt.Errorf("upload: ship acceptance exhausted retries: %w", lastErr)
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
	// (network/git/GitHub).
	//
	// When Outcome is "pushed", this is a decomposed-child push-success
	// report (#771): no PR was opened, so instead of the success PR artifact
	// in Body, ShipPullRequest signs and ships
	// {"outcome":"pushed","branch":...,"head_sha":...,"base_sha":...,
	// "files_changed_count":...} so the backend drives the child stage's
	// terminal transition its push_to_shared_branch trace gate left in
	// `running`. Branch/HeadSHA/BaseSHA carry the pushed shared-branch commit.
	//
	// When Outcome is "fixup_pushed", this is a fix-up re-dispatch
	// push-success report (#794): no PR was opened (the fix-up commit landed on
	// the EXISTING PR branch), so instead of the success PR artifact in Body,
	// ShipPullRequest signs and ships
	// {"outcome":"fixup_pushed","branch":...,"head_sha":...,"base_sha":...,
	// "files_changed_count":...} so the backend drives the fix-up stage's
	// terminal transition its push_fixup trace gate left in `running`.
	//
	// When Outcome is "fixup_no_changes", this is a fix-up re-dispatch that
	// produced NO changes (#856): the fix-up pass committed nothing, so no new
	// commit landed on the PR branch, but the push_fixup trace gate still left
	// the fix-up stage in `running`. ShipPullRequest signs and ships
	// {"outcome":"fixup_no_changes","branch":...,"base_sha":...,
	// "files_changed_count":0} (no head_sha — the branch tip is unchanged) so
	// the backend drives the fix-up stage's terminal transition and re-parks the
	// review gate, instead of hanging until the SLA watchdog reaps it.
	//
	// When Outcome is "scope_park", this is a missing-declared-scope-file-ONLY
	// park report (#1231): the implement stage's sole committed-tree gate failure
	// was the #1151 scope-completeness shortfall, so the runner pushed the
	// gate-verified commit to the run branch WITHOUT opening a PR. ShipPullRequest
	// signs and ships
	// {"outcome":"scope_park","branch":...,"head_sha":...,"base_sha":...,
	// "verified_tree_sha":...,"missing_paths":[...]} so the backend records the
	// park payload, transitions the implement stage to awaiting_scope_decision (a
	// parked judgment, NOT a category-B failure), and surfaces an in-band operator
	// exempt/fail decision — instead of the trace gate leaving the stage hung in
	// `running` until the SLA watchdog reaps it. TreeSHA pins the gate-verified
	// tree (wire tag verified_tree_sha); MissingPaths names the declared paths the
	// commit did not touch. The outcome string + tags are byte-identical to the
	// backend's scope_park pullRequestBody decode (slice 1).
	//
	// When Outcome is empty the success Body path (a real PR artifact) is
	// used unchanged.
	Outcome  string
	Category string
	Reason   string

	// Branch, HeadSHA, BaseSHA, and FilesChangedCount carry the pushed
	// commit details for the Outcome=="pushed" child-push (#771) and
	// Outcome=="fixup_pushed" fix-up (#794) reports. Unused for the empty
	// (success Body) path.
	//
	// On the Outcome=="failed" path, Branch/HeadSHA/BaseSHA (plus TreeSHA below,
	// shipped as verified_tree_sha) are the OPTIONAL PR-open checkpoint (#2169):
	// they are populated only when the failure happened AFTER CommitAndPush
	// pushed the gate-verified commit, and are all empty on a pre-push failure —
	// which keeps that body byte-identical to the pre-#2169 failure shape.
	Branch            string
	HeadSHA           string
	BaseSHA           string
	FilesChangedCount int

	// TreeSHA and MissingPaths carry the scope-completeness park payload for the
	// Outcome=="scope_completeness_parked" report (#1231). TreeSHA is the
	// gate-verified tree object hash of the held commit; MissingPaths is the
	// order-preserving list of concrete declared scope.files the commit did not
	// touch. Unused (and dropped via omitempty below) for every other outcome.
	TreeSHA      string
	MissingPaths []string

	// UnsatisfiedAssertions carries the SECOND scope_park shortfall class
	// (#2501): the operator-declared binding assertions the held commit did not
	// satisfy. Exactly one of MissingPaths / UnsatisfiedAssertions is populated
	// on a park report (a compound failure never parks — it keeps the category-B
	// abort). Dropped via omitempty below, so a pure missing-scope park's signed
	// bytes stay byte-identical to the pre-#2501 body.
	UnsatisfiedAssertions []BindingAssertionReport

	// BuildRequiredPaths carries the THIRD scope_park shortfall class (#2548):
	// the scope-EXCLUDED paths a DECOMPOSED CHILD's committed tree needed to
	// build, i.e. the files a sibling slice owns that the decomposition boundary
	// cut away. Populated only on a decomposed child's build-required park;
	// omitted (omitempty on the wire) everywhere else, so every existing SIGNED
	// body stays byte-identical.
	BuildRequiredPaths []string

	// ApplyPath carries the near-deterministic fix-up apply provenance (#1165)
	// for the Outcome=="fixup_pushed" report: "applied" (a clean git-apply of
	// every routed concern's suggested_patch, no agent), "agent" (no apply-list
	// served / fell back to the agent), "apply_failed_fellback" (an apply-list
	// was served, the apply or its verify gate failed, the worktree reset
	// cleanly, and the agent re-derived), or "apply_failed_reset_failed". Only
	// the fixup_pushed report populates it; with the json omitempty tag below the
	// "pushed" child-push and "fixup_no_changes" bodies stay byte-identical.
	ApplyPath string

	// PRTitle and PRBody carry the agent-authored pull-request text onto the
	// Outcome=="scope_park" park report and the checkpoint-bearing
	// Outcome=="failed" report (#2570), so a held-commit RESUME opens a real pull
	// request instead of the `chore: fishhawk implement stage <id>` placeholder.
	// PRBody excludes the Fishhawk attribution footer — the OPENING runner owns
	// it (see HeldCommitPRText). Both are clamped and budgeted against the
	// backend's 32 KiB request cap before shipping; empty on every other outcome
	// and on every pre-push failure, where the body stays byte-identical to today.
	PRTitle string
	PRBody  string
}

// pullRequestFailureBody is the failure-report wire shape ShipPullRequest
// marshals when Args.Outcome == "failed". Mirrors the optional fields the
// backend's pullRequestBody decodes (#742); kept here so the runner owns
// the bytes it signs.
type pullRequestFailureBody struct {
	Outcome  string `json:"outcome"`
	Category string `json:"category"`
	Reason   string `json:"reason"`

	// Branch, HeadSHA, BaseSHA, and VerifiedTreeSHA are the PR-OPEN CHECKPOINT
	// (#2169): the pushed coordinates of a failure that happened AFTER
	// CommitAndPush already landed the gate-verified commit on the run branch (a
	// PR-open failure, or a ship failure once the PR was open). The backend
	// records them into the pull_request_failed audit payload as push_checkpoint,
	// and the prompt-time resume gate then re-dispatches the stage to re-attempt
	// only the idempotent OpenPR from this exact head — no agent re-invocation.
	//
	// ALL omitempty, and the runner populates them ONLY once the push succeeded:
	// a PRE-push failure (mint, commit, or the push itself) leaves every one
	// empty, so its body marshals BYTE-IDENTICALLY to the legacy
	// {outcome,category,reason} shape and can never claim a resumable branch.
	// The backend decodes them off the same pullRequestBody it already uses for
	// the pushed / scope_park variants, so no new decode field is added.
	Branch          string `json:"branch,omitempty"`
	HeadSHA         string `json:"head_sha,omitempty"`
	BaseSHA         string `json:"base_sha,omitempty"`
	VerifiedTreeSHA string `json:"verified_tree_sha,omitempty"`

	// HeldCommitPRText is the agent-authored PR title + body the checkpoint
	// carries (#2570), appended LAST and fully omitempty so every pre-#2570
	// failure body — and therefore every signature and content hash over it — is
	// BYTE-IDENTICAL. Populated only alongside a real checkpoint.
	HeldCommitPRText
}

// pullRequestChildPushBody is the success-report wire shape ShipPullRequest
// marshals when Args.Outcome == "pushed" (#771). A decomposed child pushes
// onto the shared parent branch without opening a PR, so the body carries the
// pushed commit's branch + SHAs + diff size rather than a PR number/URL. The
// backend decodes these optional fields from the same pullRequestBody as the
// success and failure variants; kept here so the runner owns the bytes it
// signs.
type pullRequestChildPushBody struct {
	Outcome           string `json:"outcome"`
	Branch            string `json:"branch"`
	HeadSHA           string `json:"head_sha"`
	BaseSHA           string `json:"base_sha"`
	FilesChangedCount int    `json:"files_changed_count"`
	// ApplyPath is the fix-up apply provenance (#1165), populated only on the
	// "fixup_pushed" report. omitempty keeps the "pushed" child-push and
	// "fixup_no_changes" bodies byte-identical when it is unset.
	ApplyPath string `json:"apply_path,omitempty"`
}

// HeldCommitPRText is the agent-authored pull-request title + body a park or
// checkpoint report carries so a held-commit RESUME opens a real pull request
// (#2570). Both fields are omitempty on every body that embeds it, so a report
// shipped WITHOUT recovered text — every pre-#2570 shape — marshals
// BYTE-IDENTICALLY to today, and every signature and content hash over those
// bytes is unchanged.
//
// FOOTER OWNERSHIP (#2570 binding condition 3). PRBody is the agent's body
// WITHOUT the Fishhawk attribution footer. The footer is owned by the OPENING
// runner and appended once at open time (openHeldCommitPR), never persisted
// here — so a resumed PR carries exactly one footer, stamped with the branch and
// audit URL of the run that actually opened it, and truncation can never clip it.
//
// The json tags (title/body) are the runner↔backend WIRE CONTRACT: they are
// byte-identical to the backend's pullRequestBody Title/Body fields
// (backend/internal/server/pullrequest.go), which the success artifact already
// declares — so the backend's DisallowUnknownFields decoder accepts both
// variants with no new decode field.
type HeldCommitPRText struct {
	Title  string `json:"title,omitempty"`
	PRBody string `json:"body,omitempty"`
}

// pullRequestScopeParkBody is the scope-completeness park-report wire shape
// ShipPullRequest marshals when Args.Outcome == "scope_park" (#1231). The held
// commit is already pushed to the run branch, so the body carries the held
// commit's branch + SHAs + the gate-verified tree + the missing declared paths
// rather than a PR number/URL. The json tags
// (branch/head_sha/base_sha/verified_tree_sha/missing_paths) are byte-identical
// to the backend's scope_park pullRequestBody decode struct (which maps 1:1 into
// run.ScopeCompletenessPark, slice 1), the runner↔backend wire contract for the
// park round-trip (the same cross-module convention as pullRequestChildPushBody
// / #771 and ScopeExemption / #1229). Kept here so the runner owns the bytes it
// signs.
type pullRequestScopeParkBody struct {
	Outcome string `json:"outcome"`
	Branch  string `json:"branch"`
	HeadSHA string `json:"head_sha"`
	BaseSHA string `json:"base_sha"`
	TreeSHA string `json:"verified_tree_sha"`
	// MissingPaths carries omitempty (#2501) so an ASSERTION-only park omits the
	// key entirely rather than shipping `"missing_paths":null`. A missing-scope
	// park always populates it, so those bodies stay byte-identical.
	MissingPaths []string `json:"missing_paths,omitempty"`
	// UnsatisfiedAssertions is the second shortfall class (#2501), appended LAST
	// and omitempty so a pure missing-scope park's bytes are unchanged.
	UnsatisfiedAssertions []BindingAssertionReport `json:"unsatisfied_assertions,omitempty"`
	// BuildRequiredPaths is the third shortfall class (#2548), appended LAST and
	// omitempty so every pre-#2548 park body — and therefore every signature and
	// content hash computed over it — is BYTE-IDENTICAL.
	BuildRequiredPaths []string `json:"build_required_paths,omitempty"`

	// HeldCommitPRText is the agent-authored PR title + body the park carries so
	// the exempt resume opens a real pull request (#2570). Appended LAST and
	// fully omitempty, so every pre-#2570 park body is BYTE-IDENTICAL.
	HeldCommitPRText
}

// BindingAssertionReport is one unsatisfied operator-declared binding assertion
// carried on a scope_park report (#2501). The json tags (type/path/literal) are
// byte-identical to the backend's run.UnsatisfiedAssertion / the server's
// scope_park decode struct, the runner↔backend wire contract for the
// assertion-class park — the same independent-struct-by-tag convention as
// BindingAssertion (#1171) and ScopeExemption (#1229). A tag drift silently
// empties the park payload the operator reads before deciding.
type BindingAssertionReport struct {
	Type    string `json:"type"`
	Path    string `json:"path"`
	Literal string `json:"literal"`
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
// /v0/runs/{run_id}/pull-request?stage_id=…. The body is the success PR
// artifact (Args.Body) when Args.Outcome is empty, a failure report when
// Outcome=="failed" (#742), or a decomposed-child push-success report when
// Outcome=="pushed" (#771). Retries 5xx with exponential backoff. Permanent
// failures bubble up:
//
//   - 400 pull_request_invalid → ErrPullRequestInvalid (runner shipped wrong shape)
//   - 401 signature_*          → ErrSignatureRejected
//   - 404 stage/key            → ErrNotFound
//
// On 201 the backend created a fresh artifact; on 200 the upload
// matched an existing one (Result.Idempotent==true).
func (c *Client) ShipPullRequest(ctx context.Context, args ShipPullRequestArgs) (*ShipPullRequestResult, error) {
	body := args.Body
	switch args.Outcome {
	case "failed":
		// Failure-report POST (#742): build the failure body from the
		// outcome fields rather than the (absent) success artifact. Truncate the
		// reason to fit the backend's 32*1024 body cap (#1791) — a category-B
		// implement failure whose reason embeds the whole multi-module verify
		// output would otherwise 413 body_too_large and strand the stage
		// 'running'. The full text stays in the trace bundle + local log.
		failure := pullRequestFailureBody{
			Outcome:  args.Outcome,
			Category: args.Category,
			Reason:   args.Reason,
			// PR-open checkpoint (#2169): empty on every pre-push failure, so the
			// body stays byte-identical to the legacy shape there.
			Branch:          args.Branch,
			HeadSHA:         args.HeadSHA,
			BaseSHA:         args.BaseSHA,
			VerifiedTreeSHA: args.TreeSHA,
			// Held-commit PR text (#2570): empty everywhere the checkpoint is,
			// so the legacy body stays byte-identical there too.
			HeldCommitPRText: HeldCommitPRText{Title: args.PRTitle, PRBody: args.PRBody},
		}
		// Budget the MARSHALLED bytes, not the raw field lengths (#2570): the
		// reason's nominal 30 KiB clamp is applied inside, then the ladder yields
		// fields until the SERIALIZED body fits the backend's cap. A false return
		// means even the emptied envelope is over — ship it anyway and let the
		// backend's 413 plus the bounded aggressive retry below decide, rather
		// than dropping the report the stage's recovery depends on.
		_ = shrinkFailureBodyFields(&failure, MaxPullRequestReportBytes)
		marshalled, err := json.Marshal(failure)
		if err != nil {
			return nil, fmt.Errorf("upload: marshal pull-request failure body: %w", err)
		}
		body = marshalled
	case "pushed", "fixup_pushed", "fixup_no_changes":
		// Child-push (#771) / fix-up-push (#794) / fix-up no-changes (#856)
		// success report: build the push body from the pushed commit details
		// rather than the (absent) PR artifact. All three outcomes share the
		// same wire shape (branch + SHAs + diff size); only the outcome
		// discriminator differs. For "fixup_no_changes" no new commit landed, so
		// HeadSHA is empty and FilesChangedCount is 0 — branch + base_sha pin the
		// unchanged tip for the audit entry.
		marshalled, err := json.Marshal(pullRequestChildPushBody{
			Outcome:           args.Outcome,
			Branch:            args.Branch,
			HeadSHA:           args.HeadSHA,
			BaseSHA:           args.BaseSHA,
			FilesChangedCount: args.FilesChangedCount,
			ApplyPath:         args.ApplyPath,
		})
		if err != nil {
			return nil, fmt.Errorf("upload: marshal pull-request push body: %w", err)
		}
		body = marshalled
	case "scope_park":
		// Scope-completeness park report (#1231): no PR was opened (the
		// gate-verified commit landed on the run branch only), so build the park
		// body from the held commit details + the missing declared paths rather
		// than the (absent) PR artifact. The backend records the park payload and
		// transitions the implement stage to awaiting_scope_decision.
		park := pullRequestScopeParkBody{
			Outcome:               args.Outcome,
			Branch:                args.Branch,
			HeadSHA:               args.HeadSHA,
			BaseSHA:               args.BaseSHA,
			TreeSHA:               args.TreeSHA,
			MissingPaths:          args.MissingPaths,
			UnsatisfiedAssertions: args.UnsatisfiedAssertions,
			BuildRequiredPaths:    args.BuildRequiredPaths,
			// Held-commit PR text (#2570) so the exempt resume opens a real PR.
			// Empty on a pre-#2570 runner, where the body is byte-identical.
			HeldCommitPRText: HeldCommitPRText{Title: args.PRTitle, PRBody: args.PRBody},
		}
		// Same marshalled-bytes budget as the failure body (#2570).
		_ = shrinkParkBodyFields(&park, MaxPullRequestReportBytes)
		marshalled, err := json.Marshal(park)
		if err != nil {
			return nil, fmt.Errorf("upload: marshal pull-request scope-park body: %w", err)
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

	// aggressiveRetried guards the single bounded post-4xx retry below (#1791):
	// a 413 body_too_large on a failure report means even the normal-cap body
	// exceeded 32*1024, so re-marshal the reason with the aggressive cap and
	// re-POST exactly once. Only the "failed" outcome carries an operator-facing
	// reason large enough to overflow, so the retry is scoped to it.
	aggressiveRetried := false

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
			// Bounded single retry for a 4xx (esp. 413 body_too_large) on a
			// failure report (#1791): the normal-cap body was still rejected, so
			// re-marshal the reason with the aggressive cap, re-sign the smaller
			// bytes, and re-POST exactly once. aggressiveRetried prevents a loop —
			// a persistent 4xx after the retry surfaces lastErr. A non-failed
			// outcome (a real PR artifact) is never rescued: its body is not a
			// truncatable reason, so it fails fast as before.
			if args.Outcome == "failed" && resp.StatusCode >= 400 && resp.StatusCode < 500 && !aggressiveRetried {
				aggressiveRetried = true
				marshalled, mErr := json.Marshal(pullRequestFailureBody{
					Outcome:  args.Outcome,
					Category: args.Category,
					Reason:   TruncateReason(args.Reason, AggressiveFailureReportReasonBytes),
					// The re-marshal MUST carry the PR-open checkpoint (#2169) too:
					// dropping it here would make a 413-retried failure report
					// silently un-resumable, falling the retry back to a full agent
					// re-run — exactly the ~$4-6 loss the checkpoint exists to avoid.
					Branch:          args.Branch,
					HeadSHA:         args.HeadSHA,
					BaseSHA:         args.BaseSHA,
					VerifiedTreeSHA: args.TreeSHA,
					// The held-commit PR text (#2570) is DELIBERATELY dropped here.
					// This rung exists because even the budgeted body was rejected,
					// so it ships the minimum that still makes the stage recoverable:
					// the checkpoint coordinates (without which the retry re-runs the
					// whole agent) and a 2 KiB reason. The PR text costs only the
					// placeholder title the backend's issue-context fallback then
					// covers — the cheapest field to lose.
				})
				if mErr != nil {
					return nil, fmt.Errorf("upload: marshal aggressive pull-request failure body: %w", mErr)
				}
				body = marshalled
				d := sha256.Sum256(body)
				sigHex = hex.EncodeToString(ed25519.Sign(args.PrivateKey, d[:]))
				continue
			}
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

// FetchScopeAmendmentsArgs collects the inputs for FetchScopeAmendments.
// MCPToken is the run-bound fhm_ bearer FetchMCPToken returned — the
// SAME token the agent's poll loop uses, so the backend has exactly one
// agent-side auth path for the amendment surface (#961). No signing
// key: the GET has no body to sign under the Ed25519 scheme.
type FetchScopeAmendmentsArgs struct {
	RunID    string
	MCPToken string
	// WaitSeconds, when POSITIVE, adds the backend's `?wait=N` bounded
	// long-poll: the server holds the request up to N seconds and returns
	// as soon as a pending amendment is decided (#2748's decision park
	// reuses it). Zero or negative omits the query parameter ENTIRELY, so
	// the request is byte-identical to the pre-#2748 shape — that absence
	// is what keeps the #961 pre-commit refresh and the #2601
	// undecided-detection call sites unchanged.
	WaitSeconds int
}

// ScopeAmendmentPath is one requested path + operation inside a
// ScopeAmendment. Operation is "modify" or "create".
type ScopeAmendmentPath struct {
	Path      string `json:"path"`
	Operation string `json:"operation"`
}

// ScopeAmendment is one amendment row as the backend's GET
// /v0/runs/{run_id}/scope-amendments returns it. Status is
// pending|approved|denied; only approved rows fold into the runner's
// pre-commit scope refresh.
type ScopeAmendment struct {
	ID             string               `json:"id"`
	RunID          string               `json:"run_id"`
	StageID        string               `json:"stage_id"`
	Paths          []ScopeAmendmentPath `json:"paths"`
	Reason         string               `json:"reason"`
	Status         string               `json:"status"`
	DecisionReason string               `json:"decision_reason,omitempty"`
	DecidedBy      string               `json:"decided_by,omitempty"`
}

// fetchScopeAmendmentsResponse is the GET list envelope.
type fetchScopeAmendmentsResponse struct {
	Items []ScopeAmendment `json:"items"`
}

// FetchScopeAmendments calls GET /v0/runs/{run_id}/scope-amendments
// (E22.X / #961) bearing the run-bound MCP token and returns every
// amendment for the run. The caller filters to approved rows and folds
// their paths into the scope set BEFORE StageScoped runs, so the #960
// verified-tree gates verify the same folded tree that is pushed.
//
// Single-attempt: the refresh is best-effort (ADR-021 degradation) —
// on failure the caller proceeds with the unamended scope and the
// #818/#825 gates still fail loud on any undeclared created file.
func (c *Client) FetchScopeAmendments(ctx context.Context, args FetchScopeAmendmentsArgs) ([]ScopeAmendment, error) {
	if args.RunID == "" {
		return nil, errors.New("upload: run_id required")
	}
	if args.MCPToken == "" {
		return nil, errors.New("upload: mcp token required")
	}

	endpoint := fmt.Sprintf("%s/v0/runs/%s/scope-amendments",
		c.BaseURL, url.PathEscape(args.RunID))
	// Purely additive: a non-positive WaitSeconds appends NOTHING, keeping
	// the existing call sites' request byte-identical (#2748).
	if args.WaitSeconds > 0 {
		endpoint += "?wait=" + strconv.Itoa(args.WaitSeconds)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("upload: build scope-amendments request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+args.MCPToken)
	req.Header.Set("Accept", "application/json")

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("upload: fetch scope amendments: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	switch resp.StatusCode {
	case http.StatusOK:
		var out fetchScopeAmendmentsResponse
		if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
			return nil, fmt.Errorf("upload: decode scope-amendments response: %w", err)
		}
		return out.Items, nil
	case http.StatusNotFound:
		return nil, ErrNotFound
	default:
		return nil, statusError("fetch scope amendments", resp)
	}
}

// RequestScopeAmendmentArgs collects the inputs for RequestScopeAmendment.
// MCPToken is the run-bound fhm_ bearer FetchMCPToken returned — the SAME
// token FetchScopeAmendments and the agent's poll loop use, so the backend
// keeps exactly one agent-side auth path for the amendment surface. No
// signing key: the endpoint authenticates on the bearer alone.
type RequestScopeAmendmentArgs struct {
	RunID    string
	MCPToken string
	Paths    []ScopeAmendmentPath
	Reason   string
}

// requestScopeAmendmentBody is the POST wire shape the backend's
// scopeAmendmentRequest decodes.
type requestScopeAmendmentBody struct {
	Paths  []ScopeAmendmentPath `json:"paths"`
	Reason string               `json:"reason"`
}

// RequestScopeAmendment POSTs a scope-amendment request to
// /v0/runs/{run_id}/scope-amendments bearing the run-bound MCP token, and
// returns the created (pending) amendment row (#2748). The runner uses it to
// turn a recognized migration renumber into an operator DECISION rather than
// a silent substitution.
//
// SINGLE ATTEMPT, no retry: every non-201 outcome is a decision the caller
// falls open on (proceeding with the un-amended scope), so a retry would only
// burn stage wall clock. The three distinguishable refusals get their own
// sentinels — 422 amendment_budget_exhausted, 409 stage_not_implement, 403 —
// so the caller can name the branch it took in its trace event.
func (c *Client) RequestScopeAmendment(ctx context.Context, args RequestScopeAmendmentArgs) (*ScopeAmendment, error) {
	if args.RunID == "" {
		return nil, errors.New("upload: run_id required")
	}
	if args.MCPToken == "" {
		return nil, errors.New("upload: mcp token required")
	}
	if len(args.Paths) == 0 {
		return nil, errors.New("upload: at least one path required")
	}
	if strings.TrimSpace(args.Reason) == "" {
		return nil, errors.New("upload: reason required")
	}

	body, err := json.Marshal(requestScopeAmendmentBody{Paths: args.Paths, Reason: args.Reason})
	if err != nil {
		return nil, fmt.Errorf("upload: marshal scope amendment request: %w", err)
	}

	endpoint := fmt.Sprintf("%s/v0/runs/%s/scope-amendments",
		c.BaseURL, url.PathEscape(args.RunID))
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("upload: build scope-amendment request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+args.MCPToken)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("upload: request scope amendment: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	switch resp.StatusCode {
	case http.StatusCreated, http.StatusOK:
		var out ScopeAmendment
		if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
			return nil, fmt.Errorf("upload: decode scope-amendment response: %w", err)
		}
		return &out, nil
	case http.StatusUnprocessableEntity:
		return nil, fmt.Errorf("%w: %s", ErrAmendmentBudgetExhausted, readBriefBody(resp))
	case http.StatusConflict:
		return nil, fmt.Errorf("%w: %s", ErrStageNotImplement, readBriefBody(resp))
	case http.StatusForbidden:
		return nil, fmt.Errorf("%w: %s", ErrScopeAmendmentForbidden, readBriefBody(resp))
	case http.StatusNotFound:
		return nil, ErrNotFound
	default:
		return nil, statusError("request scope amendment", resp)
	}
}

// ReportStageProgressArgs collects the inputs for ReportStageProgress. MCPToken
// is the run-bound fhm_ bearer FetchMCPToken returned — the SAME token the
// agent's poll loop and FetchScopeAmendments use, so the backend has one
// agent-side auth path for the progress surface (#2541). No signing key: the
// endpoint authenticates on the bearer.
type ReportStageProgressArgs struct {
	RunID             string
	StageID           string
	MCPToken          string
	LastEvent         string
	TurnsThisAttempt  int
	TokensThisAttempt int
}

// reportStageProgressBody is the POST wire shape. reported_at is server-stamped,
// so it is not sent.
type reportStageProgressBody struct {
	LastEvent         string `json:"last_event"`
	TurnsThisAttempt  int    `json:"turns_this_attempt"`
	TokensThisAttempt int    `json:"tokens_this_attempt"`
}

// ReportStageProgress POSTs the runner's stage_progress heartbeat to
// /v0/runs/{run_id}/stages/{stage_id}/progress bearing the run-bound MCP token
// (#2541), projecting last_event / turns / tokens onto the stage row so an
// operator poll returns real activity.
//
// SINGLE ATTEMPT, no retry and no backoff — deliberately unlike ShipTrace /
// ShipPlan: the next ~15s heartbeat supersedes a lost report, so a retry would
// only pile work behind a slow backend. 204/200 → nil; every other status and
// every transport error → a wrapped error for the caller to log and drop.
func (c *Client) ReportStageProgress(ctx context.Context, args ReportStageProgressArgs) error {
	if args.RunID == "" || args.StageID == "" {
		return errors.New("upload: run_id and stage_id required")
	}
	if args.MCPToken == "" {
		return errors.New("upload: mcp token required")
	}

	body, err := json.Marshal(reportStageProgressBody{
		LastEvent:         args.LastEvent,
		TurnsThisAttempt:  args.TurnsThisAttempt,
		TokensThisAttempt: args.TokensThisAttempt,
	})
	if err != nil {
		return fmt.Errorf("upload: marshal stage progress: %w", err)
	}

	endpoint := fmt.Sprintf("%s/v0/runs/%s/stages/%s/progress",
		c.BaseURL, url.PathEscape(args.RunID), url.PathEscape(args.StageID))

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("upload: build stage progress request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+args.MCPToken)
	req.Header.Set("Accept", "application/json")

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return fmt.Errorf("upload: report stage progress: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	switch resp.StatusCode {
	case http.StatusNoContent, http.StatusOK:
		return nil
	default:
		return statusError("report stage progress", resp)
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

// RunLineageComplete reports whether the run's lineage is fully terminal —
// the lineage-root run terminal AND every decomposed child terminal — by
// reading the `lineage_complete` field off GET /v0/runs/{run_id} (E22.X /
// #1137). The local-loop runner's worktree sweep calls it with a lineage
// root's run id to decide whether to reclaim that lineage's shared
// worktree.
//
// An absent `lineage_complete` field (an older backend that predates the
// signal) decodes to false, so the sweep degrades to "not reclaimable"
// and leaves the worktree in place rather than removing a possibly-live
// checkout. Single-attempt and unsigned: the run read is an anonymous GET
// on the local loop, and the sweep treats any error as best-effort (it
// logs and skips), so retrying here would only delay provisioning.
func (c *Client) RunLineageComplete(ctx context.Context, runID string) (bool, error) {
	if runID == "" {
		return false, errors.New("upload: run_id required")
	}

	endpoint := fmt.Sprintf("%s/v0/runs/%s", c.BaseURL, url.PathEscape(runID))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return false, fmt.Errorf("upload: build run request: %w", err)
	}
	req.Header.Set("Accept", "application/json")

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return false, fmt.Errorf("upload: get run: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	switch resp.StatusCode {
	case http.StatusOK:
		var out struct {
			LineageComplete *bool `json:"lineage_complete"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
			return false, fmt.Errorf("upload: decode run response: %w", err)
		}
		return out.LineageComplete != nil && *out.LineageComplete, nil
	case http.StatusNotFound:
		return false, ErrNotFound
	default:
		return false, statusError("get run", resp)
	}
}

// RunState reads a run's live state off GET /v0/runs/{run_id} (#2545). The
// runner's lineage-lock reclaim calls it with the run id recorded in a
// contended lockfile: only a holder whose run the backend reports CANCELLED is
// evictable, so this read is the authorizing state half of that rule (the argv
// identity check is the authorizing process half).
//
// An absent or empty `state` field returns "" with NO error, which the caller
// treats as UNKNOWN and refuses on — the same fail-closed posture as a
// transport error or a 404 (ErrNotFound). Single-attempt and unsigned, exactly
// like the adjacent RunLineageComplete: the run read is an anonymous GET on the
// local loop, and every failure mode is already a refusal, so retrying here
// would only delay the stage.
func (c *Client) RunState(ctx context.Context, runID string) (string, error) {
	if runID == "" {
		return "", errors.New("upload: run_id required")
	}

	endpoint := fmt.Sprintf("%s/v0/runs/%s", c.BaseURL, url.PathEscape(runID))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return "", fmt.Errorf("upload: build run request: %w", err)
	}
	req.Header.Set("Accept", "application/json")

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return "", fmt.Errorf("upload: get run: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	switch resp.StatusCode {
	case http.StatusOK:
		var out struct {
			State string `json:"state"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
			return "", fmt.Errorf("upload: decode run response: %w", err)
		}
		return out.State, nil
	case http.StatusNotFound:
		return "", ErrNotFound
	default:
		return "", statusError("get run", resp)
	}
}
