package server

import (
	"bytes"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/forge/stub"
)

// Dev-only stub-forge control surface (E72.3 / #3327).
//
// Six routes — GET/DELETE /v0/dev/forge, GET/POST /v0/dev/forge/issues,
// POST /v0/dev/forge/pulls and POST /v0/dev/forge/deliveries — exist ONLY
// when Config.DevStubForge is non-nil (fishhawkd --dev-stub-forge /
// FISHHAWKD_DEV_STUB_FORGE=1). A nil stub leaves the surface ABSENT: the
// mux never learns the paths, so every request answers the router's 404
// — the same posture as the sibling seeded-fixture surface
// (devfixtures.go), and for the same reason: a production deployment
// must not advertise that a forge-state-minting route could exist.
//
// Every route is wrapped by devLoopbackOnly (403 dev_surface_loopback_only
// for a non-loopback or unparseable peer) and, like devfixtures.go, needs
// no csrfExemptPath entry: the routes are called credential-less and the
// csrf middleware passes a session-less identity.
//
// POST /v0/dev/forge/deliveries is the load-bearing route: it builds the
// raw webhook body from the request's payload object, signs it the way the
// REAL receiver verifies it (X-Hub-Signature-256 = HMAC-SHA256 under
// Config.GitHubWebhookSecret; X-Gitlab-Token = Config.GitLabWebhookSecret
// verbatim) and dispatches the request IN-PROCESS through s.Handler() —
// the full middleware chain and the genuine /webhooks/{github,gitlab}
// receivers. Both receivers run their consumers (the E50.6 parent-close
// watcher among them) SYNCHRONOUSLY before answering, so when this route's
// 200 returns the forge state it caused is already committed and readable
// via GET /v0/dev/forge/issues.

// devForgeMaxBodyBytes bounds the body every POST /v0/dev/forge* route
// decodes, applied via http.MaxBytesReader BEFORE the decoder reads (the
// devFixturesMaxBodyBytes pattern). A seeded issue carries a handful of
// short strings; a delivery carries one webhook payload object, which for
// the issue events the watcher consumes is a few KiB. 256 KiB is generous
// for either while keeping the dev route from being a memory sink.
const devForgeMaxBodyBytes = 256 << 10

// devForgeIssueRequest is the POST /v0/dev/forge/issues body. Repo is the
// GitHub "owner/name" full name (required for github) or the GitLab
// namespaced path (optional for gitlab, registered for project lookups);
// ProjectID is the GitLab numeric project id (required for gitlab).
type devForgeIssueRequest struct {
	Forge       string   `json:"forge"`
	Repo        string   `json:"repo"`
	ProjectID   int      `json:"project_id"`
	Number      int      `json:"number"`
	State       string   `json:"state"`
	StateReason string   `json:"state_reason"`
	Title       string   `json:"title"`
	Body        string   `json:"body"`
	Comments    []string `json:"comments"`
}

// devForgePullRequest is the POST /v0/dev/forge/pulls body: the issue
// addressing fields plus the merge facts GetPullRequest reports.
type devForgePullRequest struct {
	Forge          string     `json:"forge"`
	Repo           string     `json:"repo"`
	ProjectID      int        `json:"project_id"`
	Number         int        `json:"number"`
	State          string     `json:"state"`
	Title          string     `json:"title"`
	Body           string     `json:"body"`
	Merged         bool       `json:"merged"`
	MergeCommitSHA string     `json:"merge_commit_sha"`
	MergedAt       *time.Time `json:"merged_at"`
	HeadSHA        string     `json:"head_sha"`
	HeadRef        string     `json:"head_ref"`
	BaseRef        string     `json:"base_ref"`
}

// devForgeDeliveryRequest is the POST /v0/dev/forge/deliveries body.
// Payload is the webhook event object exactly as the forge would send it;
// it is re-marshaled before signing, so key ORDER may differ from the
// caller's input — harmless, because the signature is computed over the
// bytes this route sends, which are the bytes the receiver verifies.
type devForgeDeliveryRequest struct {
	Forge      string          `json:"forge"`
	Event      string          `json:"event"`
	DeliveryID string          `json:"delivery_id"`
	Payload    json.RawMessage `json:"payload"`
}

// devForgeDeliveryResponse is the POST /v0/dev/forge/deliveries body: the
// delivery id used (minted when the request carried none), the receiver's
// HTTP status and its response body verbatim.
type devForgeDeliveryResponse struct {
	DeliveryID string          `json:"delivery_id"`
	Status     int             `json:"status"`
	Body       json.RawMessage `json:"body"`
}

// devForgeAddress is the issue/pull address every GET and POST shares,
// validated once: a known family, a positive number, a repo for github and
// a positive project_id for gitlab. It mirrors stub.validateSeed so the
// control API refuses with 400 validation_failed BEFORE the stub sees the
// record, naming the offending field.
type devForgeAddress struct {
	Forge     string
	Repo      string
	ProjectID int
	Number    int
}

// validate returns the offending field and reason, or "" when valid.
func (a devForgeAddress) validate() (field, reason string) {
	switch a.Forge {
	case stub.ForgeGitHub:
		if a.Repo == "" {
			return "repo", "github records require repo (owner/name)"
		}
	case stub.ForgeGitLab:
		if a.ProjectID <= 0 {
			return "project_id", "gitlab records require a positive project_id"
		}
	default:
		return "forge", "forge must be " + strconv.Quote(stub.ForgeGitHub) + " or " + strconv.Quote(stub.ForgeGitLab)
	}
	if a.Number <= 0 {
		return "number", "number must be positive"
	}
	return "", ""
}

// decodeDevForgeBody decodes a capped, unknown-field-refusing JSON body
// into v, answering 400 validation_failed itself and reporting false on
// failure.
func (s *Server) decodeDevForgeBody(w http.ResponseWriter, r *http.Request, v any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, devForgeMaxBodyBytes)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		s.writeError(w, r, http.StatusBadRequest, "validation_failed",
			"request body is not valid JSON or contains unknown fields",
			map[string]any{"error": err.Error()})
		return false
	}
	return true
}

// writeDevForgeAddressError answers the 400 for an invalid address.
func (s *Server) writeDevForgeAddressError(w http.ResponseWriter, r *http.Request, field, reason string) {
	s.writeError(w, r, http.StatusBadRequest, "validation_failed", reason,
		map[string]any{"field": field, "reason": reason})
}

// handleDevForgeSnapshot renders the whole stub: every record per family
// (native state vocabulary) plus the arrival-ordered request log.
func (s *Server) handleDevForgeSnapshot(w http.ResponseWriter, r *http.Request) {
	s.writeJSON(w, r, http.StatusOK, s.cfg.DevStubForge.Snapshot())
}

// handleDevForgeReset drops every record, the request log and every fault,
// so an acceptance criterion starts from an empty forge.
func (s *Server) handleDevForgeReset(w http.ResponseWriter, r *http.Request) {
	s.cfg.DevStubForge.Reset()
	s.cfg.Logger.LogAttrs(r.Context(), slog.LevelInfo, "dev stub forge reset")
	w.WriteHeader(http.StatusNoContent)
}

// handleDevForgeSeedIssue stores one issue (replacing any record at the
// same address) and answers 201 with the stored copy. An empty state
// defaults to the family's native open word.
func (s *Server) handleDevForgeSeedIssue(w http.ResponseWriter, r *http.Request) {
	var req devForgeIssueRequest
	if !s.decodeDevForgeBody(w, r, &req) {
		return
	}
	addr := devForgeAddress{Forge: req.Forge, Repo: req.Repo, ProjectID: req.ProjectID, Number: req.Number}
	if field, reason := addr.validate(); field != "" {
		s.writeDevForgeAddressError(w, r, field, reason)
		return
	}
	stored, err := s.cfg.DevStubForge.SeedIssue(stub.Issue{
		Forge:       req.Forge,
		Repo:        req.Repo,
		ProjectID:   req.ProjectID,
		Number:      req.Number,
		Title:       req.Title,
		Body:        req.Body,
		State:       req.State,
		StateReason: req.StateReason,
		Comments:    req.Comments,
	})
	if err != nil {
		// validate() above mirrors the stub's own checks, so this branch is
		// a defence against the two drifting: report the stub's reason.
		s.writeError(w, r, http.StatusBadRequest, "validation_failed", err.Error(),
			map[string]any{"error": err.Error()})
		return
	}
	s.writeJSON(w, r, http.StatusCreated, stored)
}

// handleDevForgeGetIssue reads one issue by query address and answers
// 200 with the record (comments in arrival order) or 404
// stub_issue_not_found.
func (s *Server) handleDevForgeGetIssue(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	addr := devForgeAddress{Forge: q.Get("forge"), Repo: q.Get("repo")}
	if raw := q.Get("project_id"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil {
			s.writeDevForgeAddressError(w, r, "project_id", "project_id must be an integer")
			return
		}
		addr.ProjectID = n
	}
	if raw := q.Get("number"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil {
			s.writeDevForgeAddressError(w, r, "number", "number must be an integer")
			return
		}
		addr.Number = n
	}
	if field, reason := addr.validate(); field != "" {
		s.writeDevForgeAddressError(w, r, field, reason)
		return
	}
	is, ok := s.cfg.DevStubForge.GetIssue(addr.Forge, addr.Repo, addr.ProjectID, addr.Number)
	if !ok {
		s.writeError(w, r, http.StatusNotFound, "stub_issue_not_found",
			"no issue seeded at that address",
			map[string]any{"forge": addr.Forge, "repo": addr.Repo,
				"project_id": addr.ProjectID, "number": addr.Number})
		return
	}
	s.writeJSON(w, r, http.StatusOK, is)
}

// handleDevForgeSeedPull stores one pull/merge request and answers 201
// with the stored copy.
func (s *Server) handleDevForgeSeedPull(w http.ResponseWriter, r *http.Request) {
	var req devForgePullRequest
	if !s.decodeDevForgeBody(w, r, &req) {
		return
	}
	addr := devForgeAddress{Forge: req.Forge, Repo: req.Repo, ProjectID: req.ProjectID, Number: req.Number}
	if field, reason := addr.validate(); field != "" {
		s.writeDevForgeAddressError(w, r, field, reason)
		return
	}
	stored, err := s.cfg.DevStubForge.SeedPullRequest(stub.PullRequest{
		Forge:          req.Forge,
		Repo:           req.Repo,
		ProjectID:      req.ProjectID,
		Number:         req.Number,
		Title:          req.Title,
		Body:           req.Body,
		State:          req.State,
		Merged:         req.Merged,
		MergeCommitSHA: req.MergeCommitSHA,
		MergedAt:       req.MergedAt,
		HeadSHA:        req.HeadSHA,
		HeadRef:        req.HeadRef,
		BaseRef:        req.BaseRef,
	})
	if err != nil {
		s.writeError(w, r, http.StatusBadRequest, "validation_failed", err.Error(),
			map[string]any{"error": err.Error()})
		return
	}
	s.writeJSON(w, r, http.StatusCreated, stored)
}

// errDevForgeWebhookUnconfigured marks the family whose receiver secret is
// empty; the handler maps it to 503 stub_forge_webhook_unconfigured.
var errDevForgeWebhookUnconfigured = errors.New("webhook secret unconfigured")

// devForgeDeliveryHeaders returns the receiver path and the headers that
// make body a VALID delivery for the family: the event and delivery-id
// headers plus the family's authentication (GitHub: an HMAC signature
// under the configured secret; GitLab: the configured token verbatim).
// An empty configured secret returns errDevForgeWebhookUnconfigured —
// the receiver would answer 503 anyway, but naming the cause here keeps
// the acceptance agent from reading a receiver 503 as a product defect.
func (s *Server) devForgeDeliveryHeaders(family, event, deliveryID string, body []byte) (path string, headers map[string]string, err error) {
	switch family {
	case stub.ForgeGitHub:
		if len(s.cfg.GitHubWebhookSecret) == 0 {
			return "", nil, errDevForgeWebhookUnconfigured
		}
		return "/webhooks/github", map[string]string{
			"X-GitHub-Event":      event,
			"X-GitHub-Delivery":   deliveryID,
			"X-Hub-Signature-256": stub.SignGitHubDelivery(s.cfg.GitHubWebhookSecret, body),
			"Content-Type":        "application/json",
		}, nil
	case stub.ForgeGitLab:
		if len(s.cfg.GitLabWebhookSecret) == 0 {
			return "", nil, errDevForgeWebhookUnconfigured
		}
		return "/webhooks/gitlab", map[string]string{
			"X-Gitlab-Event":      event,
			"X-Gitlab-Event-UUID": deliveryID,
			"X-Gitlab-Token":      string(s.cfg.GitLabWebhookSecret),
			"Content-Type":        "application/json",
		}, nil
	default:
		return "", nil, errors.New("unknown forge")
	}
}

// handleDevForgeDeliver signs one webhook delivery for the named family
// and dispatches it in-process through the real receiver, answering 200
// with the delivery id, the receiver's status and its body. The receiver's
// own verdict (202 accepted, 200 duplicate, 4xx malformed) is REPORTED,
// never mapped: the route succeeded in delivering; what the receiver made
// of it is the acceptance agent's evidence.
func (s *Server) handleDevForgeDeliver(w http.ResponseWriter, r *http.Request) {
	var req devForgeDeliveryRequest
	if !s.decodeDevForgeBody(w, r, &req) {
		return
	}
	if req.Forge != stub.ForgeGitHub && req.Forge != stub.ForgeGitLab {
		s.writeDevForgeAddressError(w, r, "forge",
			"forge must be "+strconv.Quote(stub.ForgeGitHub)+" or "+strconv.Quote(stub.ForgeGitLab))
		return
	}
	if strings.TrimSpace(req.Event) == "" {
		s.writeDevForgeAddressError(w, r, "event", "event is required (e.g. \"issues\" or \"Issue Hook\")")
		return
	}
	// The payload must be a JSON object: a webhook body is always an
	// object, and re-marshaling through map[string]any also proves it
	// decodes so a truncated literal never reaches the signer.
	var payload map[string]any
	if len(req.Payload) == 0 || json.Unmarshal(req.Payload, &payload) != nil || payload == nil {
		s.writeDevForgeAddressError(w, r, "payload", "payload must be a JSON object")
		return
	}
	body, err := json.Marshal(payload)
	if err != nil {
		s.writeDevForgeAddressError(w, r, "payload", "payload could not be re-encoded: "+err.Error())
		return
	}
	deliveryID := req.DeliveryID
	if deliveryID == "" {
		deliveryID = uuid.NewString()
	}
	path, headers, err := s.devForgeDeliveryHeaders(req.Forge, req.Event, deliveryID, body)
	if err != nil {
		s.writeError(w, r, http.StatusServiceUnavailable, "stub_forge_webhook_unconfigured",
			"the "+req.Forge+" webhook receiver has no secret configured, so a delivery cannot be signed",
			map[string]any{"forge": req.Forge})
		return
	}

	// Dispatch IN-PROCESS through the full handler chain. The synthetic
	// request carries a loopback RemoteAddr (the receivers do not gate on
	// it, but the logging middleware records it) and the caller's context
	// so a cancelled control call cancels the delivery.
	inner, err := http.NewRequestWithContext(r.Context(), http.MethodPost, path, bytes.NewReader(body))
	if err != nil {
		s.writeError(w, r, http.StatusInternalServerError, "internal_error",
			"build in-process delivery request failed", map[string]any{internalCauseKey: err.Error()})
		return
	}
	inner.RemoteAddr = "127.0.0.1:0"
	inner.RequestURI = path
	inner.ContentLength = int64(len(body))
	for k, v := range headers {
		inner.Header.Set(k, v)
	}
	rec := &devForgeRecorder{header: http.Header{}}
	s.Handler().ServeHTTP(rec, inner)

	respBody := rec.body.Bytes()
	if !json.Valid(respBody) {
		// The receivers always answer JSON; a non-JSON body (an empty 202
		// would still be valid JSON only if it carried one) is reported as
		// a JSON string so the response stays decodable.
		respBody, _ = json.Marshal(string(respBody))
	}
	s.cfg.Logger.LogAttrs(r.Context(), slog.LevelInfo, "dev stub forge delivery dispatched",
		slog.String("forge", req.Forge),
		slog.String("event", req.Event),
		slog.String("delivery_id", deliveryID),
		slog.Int("receiver_status", rec.status()))
	s.writeJSON(w, r, http.StatusOK, devForgeDeliveryResponse{
		DeliveryID: deliveryID,
		Status:     rec.status(),
		Body:       respBody,
	})
}

// devForgeRecorder is the minimal http.ResponseWriter the in-process
// delivery is captured into. Deliberately not httptest.ResponseRecorder,
// so production code carries no httptest import (the same rule
// forge/stub's transport follows).
type devForgeRecorder struct {
	header http.Header
	code   int
	body   bytes.Buffer
}

func (d *devForgeRecorder) Header() http.Header { return d.header }

func (d *devForgeRecorder) WriteHeader(code int) {
	if d.code == 0 {
		d.code = code
	}
}

func (d *devForgeRecorder) Write(p []byte) (int, error) {
	if d.code == 0 {
		d.code = http.StatusOK
	}
	return d.body.Write(p)
}

// status is the code the receiver wrote, or 200 when it wrote a body
// without an explicit header (net/http's implicit default).
func (d *devForgeRecorder) status() int {
	if d.code == 0 {
		return http.StatusOK
	}
	return d.code
}
