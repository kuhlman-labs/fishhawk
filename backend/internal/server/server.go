// Package server is the HTTP control plane for fishhawkd.
//
// It owns the http.Server lifecycle (Start, Shutdown), the middleware
// stack, and the route registration. Handlers are deliberately thin —
// real workflow logic lives in adjacent packages as it's added under
// epic E3 (#3).
package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/kuhlman-labs/fishhawk/backend/internal/apitoken"
	"github.com/kuhlman-labs/fishhawk/backend/internal/approval"
	"github.com/kuhlman-labs/fishhawk/backend/internal/artifact"
	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/auditcheckpublisher"
	"github.com/kuhlman-labs/fishhawk/backend/internal/auth"
	"github.com/kuhlman-labs/fishhawk/backend/internal/githubapp"
	"github.com/kuhlman-labs/fishhawk/backend/internal/githubclient"
	"github.com/kuhlman-labs/fishhawk/backend/internal/githuboidc"
	"github.com/kuhlman-labs/fishhawk/backend/internal/issuecomment"
	"github.com/kuhlman-labs/fishhawk/backend/internal/mcptoken"
	"github.com/kuhlman-labs/fishhawk/backend/internal/orchestrator"
	"github.com/kuhlman-labs/fishhawk/backend/internal/role"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
	"github.com/kuhlman-labs/fishhawk/backend/internal/signing"
	"github.com/kuhlman-labs/fishhawk/backend/internal/stagecheck"
	"github.com/kuhlman-labs/fishhawk/backend/internal/tracestore"
	"github.com/kuhlman-labs/fishhawk/backend/internal/webhook"
)

// Config holds the values needed to construct a Server. Zero-valued
// fields fall back to safe defaults.
type Config struct {
	// Addr is the listen address (e.g. ":8080" or "127.0.0.1:0").
	Addr string

	// Logger is the structured logger used by middleware. Defaults to
	// slog.Default() when nil.
	Logger *slog.Logger

	// ShutdownTimeout caps Shutdown's wait for in-flight requests to
	// drain before forcing closure.
	ShutdownTimeout time.Duration

	// RunRepo persists workflow runs and stages. Wired by the
	// /v0/runs handlers; nil leaves those handlers returning 503.
	// Tests inject in-memory fakes; production wires the Postgres
	// adapter (run.NewPostgresRepository).
	RunRepo run.Repository

	// SigningRepo issues + persists per-run Ed25519 keys for the
	// trace bundle signing flow. Wired by the
	// /v0/runs/{id}/signing-key handler; nil leaves it 503.
	SigningRepo signing.Repository

	// TraceStore persists agent trace bundles to S3 / MinIO. Wired
	// by the /v0/runs/{id}/trace handler; nil leaves it 503.
	TraceStore tracestore.Storage

	// AuditRepo writes the audit log entries that pair with every
	// state change. Wired by the trace-upload handler; nil leaves
	// it 503.
	AuditRepo audit.Repository

	// ApprovalRepo persists gate decisions. Wired by
	// POST /v0/stages/{id}/approvals; nil leaves it 503.
	ApprovalRepo approval.Repository

	// ArtifactRepo persists typed stage outputs (plans, PR refs).
	// Wired by GET /v0/stages/{id}/artifacts and
	// GET /v0/artifacts/{id}; nil leaves both 503.
	ArtifactRepo artifact.Repository

	// StageCheckRepo persists blocking-check states. Wired by GET
	// /v0/stages/{id}/checks, the GitHub check_run webhook
	// ingester, and the approval handler's gate-enforcement read
	// (#228). Nil leaves the checks endpoint at 503 and the
	// approval handler falls open (no enforcement) so v0
	// deployments without check ingestion don't refuse every
	// approve.
	StageCheckRepo stagecheck.Repository

	// Orchestrator advances a run's stages after a gate passes.
	// The approval handler calls Advance(runID) after transitioning
	// a stage to succeeded; without an orchestrator the run stalls
	// at "first stage succeeded, no next stage dispatched."
	Orchestrator *orchestrator.Orchestrator

	// GitHubWebhookSecret is the shared secret GitHub uses to
	// HMAC-sign webhook deliveries. Empty disables the
	// /webhooks/github endpoint (handler returns 503).
	GitHubWebhookSecret []byte

	// WebhookDeliveries dedups GitHub webhook deliveries by their
	// X-GitHub-Delivery UUID across the GitHub retry window. nil
	// disables the /webhooks/github endpoint.
	WebhookDeliveries webhook.DeliveryStore

	// WebhookDispatcher translates accepted webhook deliveries
	// into Run records and workflow_dispatch firings. nil leaves
	// the receiver in "log + 202" mode (handy for early dev
	// against a backend that hasn't been wired to GitHub yet).
	WebhookDispatcher *webhook.Dispatcher

	// GitHubTokens issues per-installation tokens for backend-side
	// GitHub interactions (workflow_dispatch, fetching workflow
	// spec contents, opening PRs). nil disables anything that
	// requires acting on a customer's repo. No handler consumes
	// it directly today; the webhook dispatcher (#109) is the
	// first planned reader.
	GitHubTokens githubapp.TokenProvider

	// GitHub is the typed REST wrapper consumers use for repo
	// operations (fetching the workflow spec, firing
	// workflow_dispatch). Built on top of GitHubTokens. Nil when
	// GitHubTokens is nil.
	GitHub *githubclient.Client

	// OIDCVerifier authenticates GitHub Actions OIDC tokens on
	// the signing-key endpoint per `githubOIDC` in the OpenAPI
	// spec. Nil leaves the endpoint open (the v0 self-execution
	// posture); the operator opts in by wiring githuboidc.New().
	// When set, OIDCAudience MUST also be set.
	OIDCVerifier githuboidc.Verifier

	// OIDCAudience is the `aud` claim Verifier expects on tokens.
	// Customers configure their workflow's
	// `id-token: write` step to mint tokens with this audience
	// (typically the backend's external URL). Empty when
	// OIDCVerifier is nil.
	OIDCAudience string

	// APITokenRepo persists and authenticates scoped bearer
	// tokens for the CLI / UI surfaces. Wired by the
	// /v0/tokens handlers; nil leaves them returning 503 and
	// `Authorization: Bearer <fhk_…>` requests resolving to the
	// anonymous identity.
	APITokenRepo apitoken.Repository

	// MCPTokenRepo persists the short-lived per-run bearer
	// tokens runner-side Claude Code agents use to call the
	// MCP server (E19.8 / #348). Wired by the /v0/runs/{id}/
	// mcp-token handler; nil leaves the endpoint returning 503
	// and `Authorization: Bearer <fhm_…>` requests resolving to
	// the anonymous identity. The bearer-auth middleware routes
	// to apitoken or mcptoken by inspecting the prefix.
	MCPTokenRepo mcptoken.Repository

	// AuthRepo persists users + sessions for the OAuth
	// sign-in flow (E4.2). Wired by the /v0/auth/* handlers; nil
	// leaves them returning 503 and cookie-bearing requests
	// resolving to the anonymous identity.
	AuthRepo auth.Repository

	// GitHubOAuth is the OAuth client wrapping GitHub's
	// authorize / token / user endpoints. Required for the
	// login + callback handlers; nil leaves both at 503.
	GitHubOAuth *auth.GitHubOAuth

	// GitHubManifest converts the one-shot `code` GitHub returns from
	// the manifest-flow redirect into App credentials. Required for
	// the manifest-flow start + callback handlers (E4.7); nil leaves
	// both at 503.
	GitHubManifest *auth.GitHubManifest

	// AuthRedirectAfterLogin is the URL the callback handler
	// redirects to on successful sign-in (typically the SPA's
	// root). Defaults to "/" when empty.
	AuthRedirectAfterLogin string

	// ExternalURL is the operator-facing root URL for the SPA, e.g.
	// `https://app.fishhawk.example.com`. Used to build links in
	// surfaces that escape the backend (today: GitHub Check Runs,
	// E4.x #231 — `details_url` on a check run points back here so a
	// reviewer who clicks the check on github.com lands in
	// Fishhawk). Empty disables those features cleanly; the
	// in-Fishhawk gates still work without it.
	ExternalURL string

	// RoleResolver expands `@org/team` references in the workflow
	// spec to a GitHub-login allowlist and decides whether an
	// approver subject is authorized for a gate. Wired by the
	// approval handler. Nil leaves approval submissions
	// authorization-checked only by Identity (any authenticated
	// caller can approve), which is acceptable for the v0 demo
	// loop but NOT safe for production.
	RoleResolver *role.Resolver
}

// Server wraps an http.Server with the routes and middleware stack
// that make up fishhawkd's API surface.
type Server struct {
	cfg  Config
	http *http.Server

	// promptIssueGetterOverride lets tests substitute a stub for
	// the GitHub issue lookup the prompt handler depends on without
	// standing up a fake api.github.com. nil in production; the
	// handler then resolves through cfg.GitHub.
	promptIssueGetterOverride issueGetter

	// auditCheckPublisher posts the derived fishhawk_audit_complete
	// state to GitHub as a Check Run on every compute (#231). nil
	// when ExternalURL or GitHub aren't wired — the in-Fishhawk
	// gate enforcement still runs in that case. Built once at New;
	// concurrent Publish calls are safe.
	auditCheckPublisher *auditcheckpublisher.Publisher

	// issueNotifier posts pickup-ack and plan-ready comments back
	// to the triggering GitHub issue (#234). nil when the deps
	// don't add up (no GitHub client, no audit repo, or no
	// ExternalURL). Concurrent NotifyXxx calls are safe.
	issueNotifier *issuecomment.Notifier
}

// New builds a Server. It does not start listening; call Start.
func New(cfg Config) *Server {
	if cfg.Addr == "" {
		cfg.Addr = ":8080"
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.ShutdownTimeout == 0 {
		cfg.ShutdownTimeout = 15 * time.Second
	}

	s := &Server{cfg: cfg}
	if cfg.GitHub != nil {
		s.auditCheckPublisher = auditcheckpublisher.New(auditcheckpublisher.Deps{
			GitHub:      cfg.GitHub,
			Runs:        cfg.RunRepo,
			Artifacts:   cfg.ArtifactRepo,
			ExternalURL: cfg.ExternalURL,
		})
		s.issueNotifier = issuecomment.New(issuecomment.Deps{
			GitHub:      cfg.GitHub,
			Runs:        cfg.RunRepo,
			Audit:       cfg.AuditRepo,
			ExternalURL: cfg.ExternalURL,
		})
	}
	s.http = &http.Server{
		Addr:              cfg.Addr,
		Handler:           s.buildHandler(),
		ReadHeaderTimeout: 5 * time.Second,
	}
	return s
}

// Handler returns the wrapped http.Handler. Exposed so tests can drive
// the full middleware stack without binding a port.
func (s *Server) Handler() http.Handler {
	return s.http.Handler
}

// Start begins serving on the configured address. It blocks until
// Shutdown is called or the listener errors. http.ErrServerClosed
// is reported as a clean shutdown, not an error.
func (s *Server) Start() error {
	s.cfg.Logger.Info("fishhawkd listening", slog.String("addr", s.cfg.Addr))
	if err := s.http.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("listen: %w", err)
	}
	return nil
}

// Shutdown gracefully drains in-flight requests, capped by
// ShutdownTimeout from the parent context.
func (s *Server) Shutdown(ctx context.Context) error {
	shutdownCtx, cancel := context.WithTimeout(ctx, s.cfg.ShutdownTimeout)
	defer cancel()
	return s.http.Shutdown(shutdownCtx)
}

// buildHandler wires the route mux and the middleware chain.
//
// Middleware order, outermost first:
//
//	recovery → requestID → logging → bearerAuth → csrf → mux
//
// Recovery is outermost so panics in any later layer become 500s.
// Request ID is set before logging so log lines can carry it. Auth
// runs after logging so the request is logged even if auth rejects.
//
// bearerAuth resolves Authorization: Bearer fhk_… tokens via the
// configured APITokenRepo; absent / invalid bearer headers fall
// through to the anonymous Identity and individual handlers
// decide whether anonymous is acceptable.
//
// csrf enforces the double-submit token pattern (E4.6 #152) for
// state-changing methods on session-cookie-authed requests. It
// runs after bearerAuth so it can branch on the resolved Identity:
// bearer tokens and anonymous requests bypass the check.
func (s *Server) buildHandler() http.Handler {
	mux := http.NewServeMux()
	s.registerRoutes(mux)

	var h http.Handler = mux
	h = s.csrf(h)
	h = bearerAuth(s.cfg.APITokenRepo, s.cfg.MCPTokenRepo, s.cfg.AuthRepo)(h)
	h = logging(s.cfg.Logger)(h)
	h = requestID(h)
	h = recovery(s.cfg.Logger)(h)
	return h
}
