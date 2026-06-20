// Package server is the HTTP control plane for fishhawkd.
//
// It owns the http.Server lifecycle (Start, Shutdown), the middleware
// stack, and the route registration. Handlers are deliberately thin —
// real workflow logic lives in adjacent packages as it's added under
// epic E3 (#3).
package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/kuhlman-labs/fishhawk/backend/internal/apitoken"
	"github.com/kuhlman-labs/fishhawk/backend/internal/approval"
	"github.com/kuhlman-labs/fishhawk/backend/internal/artifact"
	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/auditcheckpublisher"
	"github.com/kuhlman-labs/fishhawk/backend/internal/auth"
	"github.com/kuhlman-labs/fishhawk/backend/internal/concern"
	"github.com/kuhlman-labs/fishhawk/backend/internal/drive"
	"github.com/kuhlman-labs/fishhawk/backend/internal/githubapp"
	"github.com/kuhlman-labs/fishhawk/backend/internal/githubclient"
	"github.com/kuhlman-labs/fishhawk/backend/internal/githuboidc"
	"github.com/kuhlman-labs/fishhawk/backend/internal/issuecomment"
	"github.com/kuhlman-labs/fishhawk/backend/internal/mcptoken"
	"github.com/kuhlman-labs/fishhawk/backend/internal/orchestrator"
	"github.com/kuhlman-labs/fishhawk/backend/internal/planreview"
	"github.com/kuhlman-labs/fishhawk/backend/internal/role"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
	"github.com/kuhlman-labs/fishhawk/backend/internal/scopeamendment"
	"github.com/kuhlman-labs/fishhawk/backend/internal/signing"
	"github.com/kuhlman-labs/fishhawk/backend/internal/spec"
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

	// StartNonce is a per-start opaque identity token echoed by
	// GET /healthz as start_nonce; empty omits the field. scripts/dev
	// sets one per spawn so its readiness gate and down port-fallback
	// can prove the listener on the port is the daemon it started,
	// surviving OS pid reuse (#1018).
	StartNonce string

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

	// ScopeAmendmentRepo persists the mid-stage operator-gated
	// scope amendment requests an implement agent files while its
	// stage runs (E22.X / #961). Wired by the /v0/runs/{id}/
	// scope-amendments handlers; nil leaves them returning 503.
	ScopeAmendmentRepo scopeamendment.Repository

	// ConcernRepo persists the durable review-concern lifecycle
	// behind stable concern IDs (E22.X / #964): every plan_reviewed /
	// implement_reviewed verdict's concerns land here, fix-up routing
	// resolves concern_ids against it, and GET /v0/runs/{id} surfaces
	// the open set. Best-effort throughout — nil disables persistence
	// (the audit payload remains the authoritative record) and leaves
	// the ID-addressed fix-up path returning 503.
	ConcernRepo concern.Repository

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

	// PlanReviewers resolves the review-agent adapters invoked when a
	// stage's workflow spec declares agent review (ADR-027 / #955).
	// Default() is the precedence-selected adapter the bare `agent: N`
	// count form repeats; For(provider, model) resolves one entry of the
	// heterogeneous `agents` list. A nil set — or a set whose Default()
	// returns nil — means no reviewer backend is configured: agent review
	// degrades to the *_review_skipped path regardless of the spec config.
	// Production wires the serve.go adapter set; tests inject a stub.
	PlanReviewers ReviewerSet

	// PlanReviewer is the single-reviewer convenience form of PlanReviewers,
	// the shape this config had before the #955 ReviewerSet: when
	// PlanReviewers is nil and PlanReviewer is non-nil, New wraps it into a
	// set whose Default() is this reviewer. The wrapped set's For() always
	// errors — a heterogeneous `agents` list needs a real ReviewerSet — so
	// spec-declared providers degrade via the *_review_failed path rather
	// than silently routing every provider to one adapter. Ignored when
	// PlanReviewers is set.
	PlanReviewer PlanReviewer

	// ReviewBudget is the size-aware per-invocation timeout policy for plan-
	// and implement-review agent calls (#747). The server applies
	// ReviewBudget.Budget(len(promptText)) as a context deadline at each
	// review call site, so a large diff gets proportionally more wall-clock
	// while the worst case stays bounded by the Cap. A zero-value budget is
	// defaulted to planreview.DefaultReviewBudget in New. This is the single
	// place the server reads the budget policy from; serve.go wires it from
	// the FISHHAWKD_PLAN_REVIEW_TIMEOUT (Floor), FISHHAWKD_REVIEW_BUDGET_PER_KB
	// (PerKB), and FISHHAWKD_REVIEW_BUDGET_CAP (Cap) inputs.
	ReviewBudget planreview.ReviewBudget

	// SpendAlertMultiple is the trip threshold for the spend-anomaly
	// check (#649): the trace handler warns (spend_alert audit entry)
	// when the current hour's estimated model spend exceeds this
	// multiple of the rolling average of prior hours. Non-positive
	// values fall back to spendalert.DefaultMultiple (3x). Warn-only —
	// it never gates a run.
	SpendAlertMultiple float64

	// BudgetLocation is the IANA timezone the periodic-budget evaluator
	// (ADR-030 / #688) computes calendar period boundaries in: a weekly
	// budget resets Monday 00:00 in this zone, a monthly budget on the
	// 1st 00:00. Nil is treated as time.UTC by the trace handler's
	// checkBudgetAlerts (and by budget.PeriodRange itself). Wired from
	// FISHHAWKD_BUDGET_TIMEZONE in serve.go, which falls back to UTC on
	// an unresolvable zone name so a minimal container image's missing
	// zoneinfo never crashes startup.
	BudgetLocation *time.Location

	// MaxRunUSD is the per-run US-dollar spend ceiling — a global operator
	// safety rail (ADR-030 / #653) that HALTS a single run once its rolled
	// cost_usd_total (#649) reaches this figure, independent of the
	// per-workflow periodic budgets (#688). On breach the trace handler
	// cancels the run (SYSTEM actor, non-retryable) and writes a
	// run_budget_exceeded audit entry. This is a backstop distinct from a
	// per-workflow spec policy: it is operator config, not a workflow-v0
	// schema field. Non-positive (the default 0) disables the US$ tripwire.
	MaxRunUSD float64

	// MaxRunTokens is the per-run token ceiling, the token-dimension twin of
	// MaxRunUSD (ADR-030 / #653). Compared against the run's cumulative
	// input+output tokens summed from its cost_recorded audit ledger (there
	// is no dedicated runs column for it). Non-positive (the default 0)
	// disables the token tripwire. When both ceilings are set and both are
	// breached, US$ is reported as the breached dimension.
	MaxRunTokens int64

	// MaxParallelChildren is the global default cap on how many decomposed
	// child runs may dispatch concurrently for a single run (E24.6 / #1146).
	// It is the fall-through default behind the per-workflow
	// decomposition.max_parallel knob (the knob wins when > 0; see
	// spec.Workflow.EffectiveMaxParallel). 0 (the default) = unlimited.
	// Wired from FISHHAWKD_MAX_PARALLEL_CHILDREN in serve.go onto the
	// orchestrator, which resolves and surfaces the effective cap;
	// concurrency enforcement that consumes it lands in E24.3 (#1143).
	MaxParallelChildren int

	// ImplementModelDefault is the deployment-configured default implement
	// model — the lowest rung of the implement-model resolution ladder
	// (resolveImplementModel). Empty (the default) means "no deployment
	// default": with no spec executor.model, no plan model_recommendation, and
	// no operator override, the resolved model is empty and the runner spawns
	// the implement agent on the adapter's built-in default exactly as today
	// (byte-for-byte). Wired from FISHHAWKD_IMPLEMENT_MODEL_DEFAULT in serve.go.
	ImplementModelDefault string

	// ImplementAllowedModels is the per-adapter allowed-model policy
	// (AllowedModels) the approval gate validates the RESOLVED implement model
	// against (#1013). A nil/empty policy — or an adapter with no configured
	// set — fails OPEN (any model accepted, byte-identical to today). Wired
	// from FISHHAWKD_IMPLEMENT_ALLOWED_MODELS in serve.go via ParseAllowedModels.
	ImplementAllowedModels AllowedModels
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
	// to the triggering GitHub issue (#234). Typed as the
	// issuecomment.Channel seam (ADR-015 #79 option B): server.New
	// wires an issuecomment.Router fanning out to the v0 GitHub-comment
	// channel, so a future Slack adapter drops in with no call-site
	// change. nil when the deps don't add up (no GitHub client, no
	// audit repo, or no ExternalURL). Concurrent NotifyXxx calls are
	// safe.
	issueNotifier issuecomment.Channel

	// bgReviews tracks detached advisory plan/implement review
	// goroutines (#584). Advisory-mode agent review is dispatched off
	// the upload request goroutine so the response returns before the
	// reviewer finishes (decoupling the review from the runner's upload
	// client timeout). Shutdown drains this group, bounded by the
	// shutdown context, so a graceful stop doesn't strand an in-flight
	// review. Gating-mode review stays synchronous and is never tracked
	// here.
	bgReviews sync.WaitGroup

	// p95Cache memoizes implement-stage calibration p95 results keyed
	// by workflow_id so resolveImplementTimeout's per-prompt-fetch call
	// to implementCalibrationP95 doesn't run a full AuditRepo.ListAll
	// scan (plus the per-entry RunRepo.GetRun N+1) on every implement
	// prompt fetch. Best-effort: entries expire after
	// implementP95CacheTTL and a miss simply re-scans the audit log.
	// Guarded by p95CacheMu.
	//
	// p95CacheMu serializes the whole check-compute-store across ALL
	// workflow_ids, not just concurrent fetches for the same workflow —
	// a single mutex protects the one shared map. At v0 implement-fetch
	// volumes this is acceptable: it dedupes the thundering-herd scan
	// rather than harming throughput.
	p95CacheMu sync.Mutex
	p95Cache   map[string]p95CacheEntry

	// nowFunc is the clock the p95 cache uses to age out entries against
	// implementP95CacheTTL. It ALSO drives the spend-alert hour bucketing in
	// the spend path: the spendalert.Evaluate reference time and the
	// cost_recorded timestamp both read it so a test can pin evaluation and
	// seeding to one controlled instant. Defaults to time.Now; tests inject a
	// fake to drive TTL expiry without sleeping or to fix the spend-alert hour.
	nowFunc func() time.Time

	// appBotIdentity{Mu,Resolved,Name,Email} memoize the GitHub App bot
	// account's git commit identity (#722). The App slug and bot user-id are
	// App-global and immutable for the process, so resolveAppBotIdentity runs
	// GetApp + GetUser at most once on SUCCESS and caches the result. A
	// failed/empty resolution is NOT cached — a transient error on the first
	// prompt fetch (e.g. that caller's context cancelled) must not permanently
	// disable dynamic attribution; it is retried on the next fetch, with the
	// runner using its hardcoded fallback meanwhile.
	appBotIdentityMu       sync.Mutex
	appBotIdentityResolved bool
	appBotIdentityName     string
	appBotIdentityEmail    string

	// appIdentityGetterOverride lets tests substitute a stub for the
	// App-identity lookups resolveAppBotIdentity depends on without an
	// httptest fake of api.github.com. nil in production; the resolver
	// then falls through to cfg.GitHub.
	appIdentityGetterOverride appIdentityGetter

	// drive emits the run_auto_advanced audit trail for drive-enabled
	// runs (#1023). Built at New from AuditRepo; nil when no audit
	// repository is wired, in which case every drive hook no-ops. The
	// hooks themselves gate on the run row's Drive flag, so the engine
	// is inert for non-drive runs.
	drive *drive.Engine
}

// soleReviewerSet adapts the Config.PlanReviewer single-reviewer convenience
// form into a ReviewerSet. For() deliberately errors: the single-reviewer
// form predates per-provider resolution, and mapping every declared provider
// to one adapter would silently misroute a heterogeneous `agents` list.
type soleReviewerSet struct{ reviewer PlanReviewer }

func (s soleReviewerSet) Default() PlanReviewer { return s.reviewer }

func (soleReviewerSet) For(provider, _ string) (PlanReviewer, error) {
	return nil, fmt.Errorf("reviewer provider %q is not resolvable from the single-reviewer configuration: wire Config.PlanReviewers", provider)
}

// New builds a Server. It does not start listening; call Start.
func New(cfg Config) *Server {
	if cfg.Addr == "" {
		cfg.Addr = ":8080"
	}
	if cfg.PlanReviewers == nil && cfg.PlanReviewer != nil {
		cfg.PlanReviewers = soleReviewerSet{reviewer: cfg.PlanReviewer}
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.ShutdownTimeout == 0 {
		cfg.ShutdownTimeout = 15 * time.Second
	}
	// Default a zero-value review budget to the documented policy (#747) so a
	// server constructed without explicit budget config still bounds reviewer
	// invocations size-awarely rather than with a never-firing zero deadline.
	if cfg.ReviewBudget == (planreview.ReviewBudget{}) {
		cfg.ReviewBudget = planreview.DefaultReviewBudget
	}

	s := &Server{cfg: cfg, p95Cache: map[string]p95CacheEntry{}}
	if s.nowFunc == nil {
		s.nowFunc = time.Now
	}
	if cfg.AuditRepo != nil {
		s.drive = &drive.Engine{Audit: cfg.AuditRepo, Logger: cfg.Logger}
	}
	// Wire the gating consolidated-review dispatcher (#1060). The
	// orchestrator dispatches the parent's consolidated implement
	// review through the Server, which owns the review machinery.
	// serve.go constructs the orchestrator before server.New and
	// passes it in cfg; this back-reference activates the dispatch
	// (the shared pointer means serve.go's instance sees it). Without
	// it the field stays nil, the consolidated review never dispatches,
	// and slice-1's drive gate parks every fan-out parent forever.
	if cfg.Orchestrator != nil {
		cfg.Orchestrator.ConsolidatedReview = s
	}
	if cfg.GitHub != nil {
		s.auditCheckPublisher = auditcheckpublisher.New(auditcheckpublisher.Deps{
			GitHub:      cfg.GitHub,
			Runs:        cfg.RunRepo,
			Artifacts:   cfg.ArtifactRepo,
			ExternalURL: cfg.ExternalURL,
			// Persistent-failure surfacing (#993): a sustained
			// CreateCheckRun failure streak / its eventual recovery
			// land as paired run-chain audit entries.
			OnDegraded:  s.auditCheckPublishDegraded,
			OnRecovered: s.auditCheckPublishRecovered,
		})
		// Wire the GitHub-comment channel behind the Router seam (ADR-015
		// #79). Guard the wrap on a non-nil channel so a nil notifier
		// (e.g. empty ExternalURL) leaves s.issueNotifier as a nil
		// interface, NOT a non-nil Router over a typed-nil channel —
		// preserving the exact nil semantics approvalCommandConfigured and
		// the trace.go guards depend on (no behavior change).
		if ghChannel := issuecomment.New(issuecomment.Deps{
			GitHub:      cfg.GitHub,
			Runs:        cfg.RunRepo,
			Audit:       cfg.AuditRepo,
			ExternalURL: cfg.ExternalURL,
			// Artifacts feeds the living anchor's plan section (#1054).
			// Without it loadAnchorPlans short-circuits and the anchor
			// renders no plan in production (#1069 regression) despite a
			// green e2e — the sibling auditcheckpublisher.New above already
			// passes the same cfg.ArtifactRepo.
			Artifacts: cfg.ArtifactRepo,
		}); ghChannel != nil {
			s.issueNotifier = issuecomment.NewRouter(ghChannel)
		}
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
// ShutdownTimeout from the parent context. After the HTTP server
// drains, it also waits for any detached advisory review goroutines
// (#584) to finish, bounded by the same shutdown context so a hung
// reviewer can't block shutdown past the deadline.
func (s *Server) Shutdown(ctx context.Context) error {
	shutdownCtx, cancel := context.WithTimeout(ctx, s.cfg.ShutdownTimeout)
	defer cancel()
	err := s.http.Shutdown(shutdownCtx)

	// Drain in-flight detached reviews, bounded by the shutdown
	// context. A WaitGroup can't be select-ed on directly, so signal
	// completion through a channel.
	done := make(chan struct{})
	go func() {
		s.bgReviews.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-shutdownCtx.Done():
	}
	return err
}

// waitBackgroundReviews blocks until every detached advisory review
// goroutine has finished. It is the deterministic sync point tests use
// to assert on audit entries an async review writes — release the
// blocking reviewer, then call this, then assert. Production code never
// calls it (Shutdown drains the same group, bounded by its context).
func (s *Server) waitBackgroundReviews() { s.bgReviews.Wait() }

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
	h = s.bearerAuth(s.cfg.APITokenRepo, s.cfg.MCPTokenRepo, s.cfg.AuthRepo)(h)
	h = logging(s.cfg.Logger)(h)
	h = requestID(h)
	h = recovery(s.cfg.Logger)(h)
	return h
}

// ObserveParkedReviewForDrive evaluates one parked review stage of a
// drive-enabled run against the poll-driven mechanical rules (#1023):
// reviews_settled_gate when every configured implement reviewer
// reached a terminal verdict, and checks_green_awaiting_merge when the
// review evidence is complete AND every required PR check is green —
// the derived awaiting_merge presentation status with a distilled
// merge next action. It satisfies mergereconciler.DriveObserver, which
// invokes it on the open-PR branch of each tick.
//
// awaiting_merge is presentation-only: this method emits audit entries
// and never transitions the stage — the merge itself stays a judgment
// point (drive.RuleMerge), resolved by the webhook/poll once GitHub
// performs it. Best-effort and idempotent: every read failure skips
// quietly (the next tick retries) and each rule is recorded at most
// once per stage via Engine.Recorded.
func (s *Server) ObserveParkedReviewForDrive(ctx context.Context, stage *run.Stage, prURL string) {
	if s.drive == nil || s.cfg.RunRepo == nil || s.cfg.AuditRepo == nil || stage == nil {
		return
	}
	runRow, err := s.cfg.RunRepo.GetRun(ctx, stage.RunID)
	if err != nil || !runRow.Drive {
		return
	}

	configured, terminal, started, ok := s.implementReviewRound(ctx, runRow)
	if !ok {
		return
	}

	settled := started && planreview.Settled(configured, terminal)
	if settled && !s.drive.Recorded(ctx, stage.RunID, &stage.ID, drive.RuleReviewsSettledGate) {
		s.drive.Record(ctx, stage.RunID, &stage.ID, drive.Advance{
			Rule:  drive.RuleReviewsSettledGate,
			From:  "implement_reviews:in_flight",
			To:    "review_gate:decision_ready",
			Event: fmt.Sprintf("%d of %d implement reviews terminal", terminal, configured),
			NextAction: &drive.NextAction{
				Action: "review_pr",
				Detail: "all configured implement reviews are terminal; read the verdicts and review the PR",
				PRURL:  prURL,
			},
		})
	}

	// A round configured but never dispatched is non-terminal evidence
	// (#1060): reviewers exist on the spec yet no implement_review_started
	// landed — the decomposed-parent consolidated-review case, where the
	// gating review runs against the parent's consolidated diff. Park
	// rather than advance to awaiting_merge; only a genuinely
	// reviewer-less run (configured==0) is vacuously terminal and may
	// advance on a never-dispatched round.
	if !started && configured > 0 {
		return
	}

	// Review evidence is complete when nothing was configured to wait
	// for (configured==0 and no round dispatched — vacuously terminal,
	// mirrors checkPlanReviewSettled's configured-but-not-dispatched
	// posture) or the dispatched round settled.
	if started && !settled {
		return
	}
	if !s.reviewChecksGreen(ctx, runRow, stage) {
		// Negative mirror (#1045): review evidence is complete but a
		// required check concluded red → park in the derived ci_failed
		// state with a classify next action naming the failed check(s). A
		// merely-pending check (none failed) returns silently — only a
		// terminal red trips ci_failed, so a still-running check can never
		// over-claim a failure.
		if failed := s.reviewChecksFailed(ctx, runRow, stage); len(failed) > 0 {
			if s.drive.Recorded(ctx, stage.RunID, &stage.ID, drive.RuleCIFailed) {
				return
			}
			names := strings.Join(failed, ", ")
			s.drive.Record(ctx, stage.RunID, &stage.ID, drive.Advance{
				Rule:  drive.RuleCIFailed,
				From:  "review:awaiting_approval",
				To:    "ci_failed",
				Event: "required PR checks red: " + names,
				NextAction: &drive.NextAction{
					Action: "classify_ci_failure",
					Detail: "required PR checks concluded red (" + names + "); classify the failure and route per the next_actions arms",
					PRURL:  prURL,
				},
			})
		}
		return
	}
	if s.drive.Recorded(ctx, stage.RunID, &stage.ID, drive.RuleChecksGreenAwaitingMerge) {
		return
	}
	s.drive.Record(ctx, stage.RunID, &stage.ID, drive.Advance{
		Rule:  drive.RuleChecksGreenAwaitingMerge,
		From:  "review:awaiting_approval",
		To:    "awaiting_merge",
		Event: "review evidence terminal and required PR checks green",
		NextAction: &drive.NextAction{
			Action: "merge_pr",
			Detail: "all gates resolved and required checks are green; review and merge the PR",
			PRURL:  prURL,
		},
	})
}

// implementReviewRound reports the run's LATEST implement-review
// round: whether one was dispatched (an implement_review_started entry
// exists), how many agents it was configured with, and how many
// terminal entries (implement_reviewed / _failed / _skipped) landed
// after it. Rounds are delimited by started entries — a fix-up re-park
// dispatches a fresh round whose started entry supersedes the prior
// one, so a settled FIRST round can never satisfy the gate while the
// re-review is still in flight. ok=false on any audit read failure
// (the poll-driven caller skips and retries next tick).
func (s *Server) implementReviewRound(ctx context.Context, runRow *run.Run) (configured, terminal int, started, ok bool) {
	startedEntries, err := s.cfg.AuditRepo.ListForRunByCategory(ctx, runRow.ID, "implement_review_started")
	if err != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn, "drive: list implement_review_started failed",
			slog.String("run_id", runRow.ID.String()),
			slog.String("error", err.Error()))
		return 0, 0, false, false
	}
	if len(startedEntries) == 0 {
		// No round dispatched yet. Resolve the configured agent count
		// from the spec so the caller can distinguish a genuinely
		// reviewer-less run (configured==0, vacuously terminal) from a
		// run with reviewers configured but no round dispatched
		// (configured>0, non-terminal — #1060's decomposed-parent
		// consolidated-review case).
		var configuredFromSpec int
		if cfg := s.resolveStageReviewers(ctx, runRow, spec.StageTypeImplement); cfg != nil {
			configuredFromSpec = cfg.AgentCount()
		}
		return configuredFromSpec, 0, false, true
	}
	latest := startedEntries[0]
	for _, e := range startedEntries {
		if e.Sequence > latest.Sequence {
			latest = e
		}
	}

	var startedPayload planreview.ReviewStartedPayload
	if uerr := json.Unmarshal(latest.Payload, &startedPayload); uerr == nil {
		configured = startedPayload.ConfiguredAgents
	}
	if configured == 0 {
		// Pre-#600-payload or malformed entry: fall back to the spec.
		if cfg := s.resolveStageReviewers(ctx, runRow, spec.StageTypeImplement); cfg != nil {
			configured = cfg.AgentCount()
		}
	}

	for _, cat := range []string{"implement_reviewed", "implement_review_failed", "implement_review_skipped"} {
		entries, lerr := s.cfg.AuditRepo.ListForRunByCategory(ctx, runRow.ID, cat)
		if lerr != nil {
			s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn, "drive: list terminal implement-review entries failed",
				slog.String("run_id", runRow.ID.String()),
				slog.String("category", cat),
				slog.String("error", lerr.Error()))
			return 0, 0, false, false
		}
		for _, e := range entries {
			if e.Sequence > latest.Sequence {
				terminal++
			}
		}
	}
	return configured, terminal, true, true
}

// reviewChecksGreen reports whether every required check from the
// run's create-time snapshot (#251) has a green latest state recorded
// against the review stage — the stage the check_run ingest path
// writes rows for. An empty snapshot is vacuously green: no required
// checks were declared, and branch protection (when any) still
// enforces at merge time. Conservative on any gap: an unwired
// StageCheckRepo, a missing row, or a non-pass state all report not
// green, so the derived awaiting_merge can never overstate readiness.
func (s *Server) reviewChecksGreen(ctx context.Context, runRow *run.Run, stage *run.Stage) bool {
	if runRow.RequiredChecksSnapshot == nil || len(runRow.RequiredChecksSnapshot.Contexts) == 0 {
		return true
	}
	if s.cfg.StageCheckRepo == nil {
		return false
	}
	for _, name := range runRow.RequiredChecksSnapshot.Contexts {
		check, err := s.cfg.StageCheckRepo.LatestForStageAndName(ctx, stage.ID, name)
		if err != nil || check.State != stagecheck.StatePass {
			return false
		}
	}
	return true
}

// reviewChecksFailed returns the required-check contexts whose latest
// state recorded against the review stage is stagecheck.StateFail — the
// red mirror of reviewChecksGreen (#1045). Only StateFail counts as
// red: a StatePending (in-flight) or StateNotTracked (no row) check is
// not failed, so a still-running check can never trip ci_failed.
// Conservative on any gap: an empty snapshot or an unwired
// StageCheckRepo returns nil, so ci_failed can never be over-claimed.
func (s *Server) reviewChecksFailed(ctx context.Context, runRow *run.Run, stage *run.Stage) []string {
	if runRow.RequiredChecksSnapshot == nil || len(runRow.RequiredChecksSnapshot.Contexts) == 0 {
		return nil
	}
	if s.cfg.StageCheckRepo == nil {
		return nil
	}
	var failed []string
	for _, name := range runRow.RequiredChecksSnapshot.Contexts {
		check, err := s.cfg.StageCheckRepo.LatestForStageAndName(ctx, stage.ID, name)
		if err != nil {
			continue
		}
		if check.State == stagecheck.StateFail {
			failed = append(failed, name)
		}
	}
	return failed
}
