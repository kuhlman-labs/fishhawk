package main

import (
	"context"
	"errors"
	"flag"
	"io"
	"log/slog"
	"os/signal"
	"syscall"

	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kuhlman-labs/fishhawk/backend/internal/anthropic"
	"github.com/kuhlman-labs/fishhawk/backend/internal/apitoken"
	"github.com/kuhlman-labs/fishhawk/backend/internal/approval"
	"github.com/kuhlman-labs/fishhawk/backend/internal/artifact"
	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	authpkg "github.com/kuhlman-labs/fishhawk/backend/internal/auth"
	"github.com/kuhlman-labs/fishhawk/backend/internal/childcompletion"
	"github.com/kuhlman-labs/fishhawk/backend/internal/claudecode"
	dispatchwatchdog "github.com/kuhlman-labs/fishhawk/backend/internal/dispatchwatchdog"
	"github.com/kuhlman-labs/fishhawk/backend/internal/githubapp"
	"github.com/kuhlman-labs/fishhawk/backend/internal/githubclient"
	"github.com/kuhlman-labs/fishhawk/backend/internal/githuboidc"
	"github.com/kuhlman-labs/fishhawk/backend/internal/issuecomment"
	"github.com/kuhlman-labs/fishhawk/backend/internal/mcptoken"
	"github.com/kuhlman-labs/fishhawk/backend/internal/mergereconciler"
	"github.com/kuhlman-labs/fishhawk/backend/internal/orchestrator"
	"github.com/kuhlman-labs/fishhawk/backend/internal/plan"
	"github.com/kuhlman-labs/fishhawk/backend/internal/planreview"
	"github.com/kuhlman-labs/fishhawk/backend/internal/reactionpoller"
	"github.com/kuhlman-labs/fishhawk/backend/internal/role"
	runpkg "github.com/kuhlman-labs/fishhawk/backend/internal/run"
	"github.com/kuhlman-labs/fishhawk/backend/internal/server"
	"github.com/kuhlman-labs/fishhawk/backend/internal/signing"
	slapkg "github.com/kuhlman-labs/fishhawk/backend/internal/sla"
	"github.com/kuhlman-labs/fishhawk/backend/internal/spendalert"
	"github.com/kuhlman-labs/fishhawk/backend/internal/stagecheck"
	"github.com/kuhlman-labs/fishhawk/backend/internal/tracestore"
	"github.com/kuhlman-labs/fishhawk/backend/internal/version"
	"github.com/kuhlman-labs/fishhawk/backend/internal/webhook"

	"os"
	"strconv"
)

// defaultPlanReviewTimeout is the #606 code default for the per-invocation
// plan-review bound — raised from 60s to 300s to cover review of large
// standard_v1 plans. It is the single source for BOTH the
// FISHHAWKD_PLAN_REVIEW_TIMEOUT flag fallback and the startup warn threshold
// so the two can never drift (#664).
const defaultPlanReviewTimeout = 300 * time.Second

// planReviewTimeoutBelowDefault reports whether the effective plan-review
// timeout is below the #606 floor (defaultPlanReviewTimeout). Extracted as a
// pure predicate so the below/equal/above boundary is unit-testable without
// capturing startup logs (#664).
func planReviewTimeoutBelowDefault(configured time.Duration) bool {
	return configured < defaultPlanReviewTimeout
}

// resolveBudgetLocation resolves an IANA timezone name to a
// *time.Location for the advisory periodic-budget evaluator (#688). A
// missing zoneinfo (minimal container image) or a typo'd name must never
// crash startup, so an unresolvable name falls back to time.UTC with a
// WARN — advisory budgets then evaluate calendar periods in UTC rather
// than the requested zone.
func resolveBudgetLocation(name string, logger *slog.Logger) *time.Location {
	loc, err := time.LoadLocation(name)
	if err != nil {
		logger.Warn("budget timezone unresolved — falling back to UTC",
			slog.String("requested", name),
			slog.String("error", err.Error()))
		return time.UTC
	}
	return loc
}

// runServe boots the HTTP server with graceful SIGINT/SIGTERM
// handling. Returns the intended process exit code.
func runServe(args []string, logSink io.Writer) int {
	fs := flag.NewFlagSet("fishhawkd serve", flag.ContinueOnError)
	fs.SetOutput(logSink)
	addr := fs.String("addr", envOr("FISHHAWKD_ADDR", ":8080"), "listen address")
	dbURL := fs.String("db", envOr("FISHHAWKD_DATABASE_URL", ""),
		"postgres URL; when empty, /v0/runs endpoints respond 503")
	webhookSecret := fs.String("github-webhook-secret",
		envOr("FISHHAWKD_GITHUB_WEBHOOK_SECRET", ""),
		"shared secret GitHub uses to HMAC-sign webhook deliveries; when empty, /webhooks/github responds 503")
	s3Bucket := fs.String("s3-bucket", envOr("FISHHAWKD_S3_BUCKET", ""),
		"S3 bucket for trace bundle storage; when empty, /v0/runs/{id}/trace responds 503")
	s3Region := fs.String("s3-region", envOr("FISHHAWKD_S3_REGION", "us-east-1"),
		"AWS region for the trace bundle bucket")
	s3Endpoint := fs.String("s3-endpoint", envOr("FISHHAWKD_S3_ENDPOINT", ""),
		"override the S3 endpoint (e.g. http://minio:9000 in dev); empty uses the AWS default")
	githubAppIDStr := fs.String("github-app-id", envOr("FISHHAWKD_GITHUB_APP_ID", ""),
		"GitHub App numeric ID; required to issue installation tokens")
	githubAppKeyFile := fs.String("github-app-private-key-file",
		envOr("FISHHAWKD_GITHUB_APP_PRIVATE_KEY_FILE", ""),
		"path to the GitHub App's PEM-encoded RSA private key")
	enableSLATimer := fs.Bool("enable-sla-timer",
		envOr("FISHHAWKD_ENABLE_SLA_TIMER", "false") == "true",
		"start the approval SLA timeout ticker; off by default to keep dev runs from racing with the timer")
	slaInterval := fs.Duration("sla-interval",
		60*time.Second,
		"SLA ticker scan interval; 60s default fits hour-grained SLAs comfortably")
	enableDispatchWatchdog := fs.Bool("enable-dispatch-watchdog",
		envOr("FISHHAWKD_ENABLE_DISPATCH_WATCHDOG", "false") == "true",
		"start the dispatch watchdog ticker (E8.4); fails category-C any stage stuck in 'dispatched' past --dispatch-watchdog-timeout. Off by default for the same dev-loop reason as --enable-sla-timer")
	dispatchWatchdogTimeout := fs.Duration("dispatch-watchdog-timeout",
		1*time.Hour,
		"how long a stage may stay in 'dispatched' before the watchdog fails it as infrastructure failure; 1h default covers GitHub Actions dispatch + queue + first checkin")
	dispatchWatchdogInterval := fs.Duration("dispatch-watchdog-interval",
		60*time.Second,
		"dispatch watchdog scan interval")
	enableReactionPoller := fs.Bool("enable-reaction-poller",
		envOr("FISHHAWKD_ENABLE_REACTION_POLLER", "false") == "true",
		"start the reaction-polling worker (#360); polls Fishhawk plan comments for approval-shaped reactions GitHub doesn't deliver via webhooks. Off by default — only useful when there's a GitHub App + audit repo wired")
	reactionPollerFastInterval := fs.Duration("reaction-poller-fast-interval",
		reactionpoller.DefaultFastInterval,
		"fast-tier cadence for the reaction poller — applies to plan comments younger than --reaction-poller-age-threshold")
	reactionPollerSlowInterval := fs.Duration("reaction-poller-slow-interval",
		reactionpoller.DefaultSlowInterval,
		"slow-tier cadence for the reaction poller — applies to plan comments older than --reaction-poller-age-threshold")
	reactionPollerAgeThreshold := fs.Duration("reaction-poller-age-threshold",
		reactionpoller.DefaultAgeThreshold,
		"plan-comment age at which the reaction poller switches from fast to slow cadence")
	enableMergeReconciler := fs.Bool("enable-merge-reconciler",
		envOr("FISHHAWKD_ENABLE_MERGE_RECONCILER", "false") == "true",
		"start the merge-status reconciler (ADR-031 Phase 1); resolves a run's review gate on a verified PR merge state when the pull_request.closed webhook was missed. Off by default — only useful with a GitHub App wired. See --merge-reconciler-interval for the rate-limit caveat at scale.")
	mergeReconcilerInterval := fs.Duration("merge-reconciler-interval",
		mergereconciler.DefaultInterval,
		"merge-status reconciler scan interval. Each tick makes one GitHub GetPullRequest call per parked review stage with no per-stage cooldown; tune this upward at scale to stay within GitHub REST rate limits (5,000/hour per installation).")
	enableChildCompletionSweeper := fs.Bool("enable-child-completion-sweeper",
		envOr("FISHHAWKD_ENABLE_CHILD_COMPLETION_SWEEPER", "false") == "true",
		"start the child-completion sweeper (#455 / ADR-025 D4); transitions parent stages parked in awaiting_children once their decomposed children all reach terminal states. Off by default to match the other tickers' dev-loop posture.")
	childCompletionInterval := fs.Duration("child-completion-interval",
		60*time.Second,
		"child-completion sweeper scan interval; 60s is the upper bound on parent latency after the last child terminates")
	oidcAudience := fs.String("oidc-audience",
		envOr("FISHHAWKD_OIDC_AUDIENCE", ""),
		"GitHub Actions OIDC audience the signing-key endpoint requires; when set, callers must present a valid id_token whose aud matches this value")
	oidcJWKSURL := fs.String("oidc-jwks-url",
		envOr("FISHHAWKD_OIDC_JWKS_URL", ""),
		"override the JWKS URL (defaults to GitHub's published endpoint); useful for testing")
	oauthClientID := fs.String("oauth-client-id",
		envOr("FISHHAWKD_OAUTH_CLIENT_ID", ""),
		"GitHub OAuth App client_id for the /v0/auth/* sign-in flow; empty disables the endpoints")
	oauthClientSecret := fs.String("oauth-client-secret",
		envOr("FISHHAWKD_OAUTH_CLIENT_SECRET", ""),
		"GitHub OAuth App client_secret; required when --oauth-client-id is set")
	oauthCallbackURL := fs.String("oauth-callback-url",
		envOr("FISHHAWKD_OAUTH_CALLBACK_URL", ""),
		"public URL of /v0/auth/github/callback; required when --oauth-client-id is set")
	oauthRedirectAfterLogin := fs.String("oauth-redirect-after-login",
		envOr("FISHHAWKD_OAUTH_REDIRECT_AFTER_LOGIN", "/"),
		"URL the callback handler redirects to on successful sign-in (must be a relative path)")
	externalURL := fs.String("external-url",
		envOr("FISHHAWKD_EXTERNAL_URL", ""),
		"operator-facing root URL for the SPA, e.g. https://app.fishhawk.example.com; used to build links in surfaces that escape the backend (today: GitHub Check Runs). Empty disables the publish-to-GitHub paths cleanly.")
	anthropicAPIKey := fs.String("anthropic-api-key",
		envOr("FISHHAWKD_ANTHROPIC_API_KEY", ""),
		"Anthropic API key for plan-review agent invocations; when empty, plan review is gateless regardless of spec config")
	planReviewModel := fs.String("plan-review-model",
		envOr("FISHHAWKD_PLAN_REVIEW_MODEL", "claude-sonnet-4-6"),
		"Anthropic model to use for plan-review agent invocations")
	enableLocalClaudeReviewer := fs.Bool("enable-local-claude-reviewer",
		envOr("FISHHAWKD_ENABLE_LOCAL_CLAUDE_REVIEWER", "false") == "true",
		"opt-in local-mode plan review: spawn the `claude` CLI as a subprocess instead of calling the Anthropic API. Ignored when --anthropic-api-key is set. Off by default")
	localClaudeBinary := fs.String("local-claude-binary",
		envOr("FISHHAWKD_LOCAL_CLAUDE_BINARY", "claude"),
		"executable name or path for the local-mode `claude` CLI; used only when --enable-local-claude-reviewer is set")
	localClaudeModel := fs.String("local-claude-model",
		envOr("FISHHAWKD_LOCAL_CLAUDE_MODEL", "claude-sonnet-4-6"),
		"model the local-mode `claude` CLI uses for plan review; used only when --enable-local-claude-reviewer is set")
	planReviewMaxTokens := fs.Int("plan-review-max-tokens",
		envOrInt("FISHHAWKD_PLAN_REVIEW_MAX_TOKENS", 4096),
		"maximum tokens for plan-review agent responses")
	planReviewMaxRetries := fs.Int("plan-review-max-retries",
		envOrInt("FISHHAWKD_PLAN_REVIEW_MAX_RETRIES", 1),
		"in-adapter retry budget for the local-mode `claude` reviewer's transient-crash class (#620); "+
			"counts retries not attempts (N => N+1 attempts), 0 disables retry (single attempt), unset defaults to 1. "+
			"Used only by the claudecode adapter — the anthropic SDK adapter has no retry field")
	planReviewTimeout := fs.Duration("plan-review-timeout",
		envOrDuration("FISHHAWKD_PLAN_REVIEW_TIMEOUT", defaultPlanReviewTimeout),
		"FLOOR of the size-aware review budget (#747): the minimum per-invocation bound for "+
			"plan-/implement-review agent calls. Preserves the #606 300s floor for small plans; "+
			"larger diffs scale up via --review-budget-per-kb, capped by --review-budget-cap")
	reviewBudgetPerKB := fs.Duration("review-budget-per-kb",
		envOrDuration("FISHHAWKD_REVIEW_BUDGET_PER_KB", planreview.DefaultReviewBudget.PerKB),
		"per-KB allowance added to the review-budget floor per kilobyte of prompt (#747); "+
			"the budget is floor + per_kb*ceil(promptBytes/1024), clamped to [floor, cap]. "+
			"Set to 0 to collapse the budget to a flat floor (today's fixed-timeout behaviour) without a redeploy")
	reviewBudgetCap := fs.Duration("review-budget-cap",
		envOrDuration("FISHHAWKD_REVIEW_BUDGET_CAP", planreview.DefaultReviewBudget.Cap),
		"hard ceiling on the size-aware review budget (#747), bounding the worst-case "+
			"synchronous gating wait for a very large diff. A non-positive value disables the ceiling")
	spendAlertMultiple := fs.Float64("spend-alert-multiple",
		envOrFloat("FISHHAWKD_SPEND_ALERT_MULTIPLE", spendalert.DefaultMultiple),
		"warn-only spend-anomaly threshold (#649): the trace handler emits a spend_alert audit "+
			"entry when the current hour's estimated model spend exceeds this multiple of the "+
			"rolling average of prior hours. Never gates a run")
	budgetTimezone := fs.String("budget-timezone",
		envOr("FISHHAWKD_BUDGET_TIMEZONE", "UTC"),
		"IANA timezone (e.g. America/New_York) the advisory periodic-budget evaluator (#688) "+
			"computes calendar period boundaries in — a weekly budget resets Monday 00:00 in this "+
			"zone, a monthly budget on the 1st. An unresolvable zone name falls back to UTC with a "+
			"WARN at startup rather than failing the boot")
	if err := fs.Parse(args); err != nil {
		return exitFailure
	}

	logger := newLogger(logSink)

	// Warn when an operator .env / flag override drops the plan-review
	// timeout below the #606 code default (300s) — a value that risks
	// timing out review of large standard_v1 plans, silently defeating the
	// raise. Surfaced at startup so the drift is no longer invisible (#664).
	if planReviewTimeoutBelowDefault(*planReviewTimeout) {
		logger.Warn("FISHHAWKD_PLAN_REVIEW_TIMEOUT is below the recommended floor; large standard_v1 plans may time out",
			slog.Duration("configured", *planReviewTimeout),
			slog.Duration("recommended_floor", defaultPlanReviewTimeout),
			slog.String("ref", "#606"))
	}
	logger.Info("plan coercion registry", slog.String("summary", plan.CoercionRegistrySummary()))

	budgetLocation := resolveBudgetLocation(*budgetTimezone, logger)

	// Size-aware review budget (#747): the plan-review timeout is the FLOOR,
	// per-KB scales it up with prompt size, and the cap bounds the worst case.
	// The per-adapter Config.Timeout below stays as the no-deadline fallback
	// for callers that set no context deadline; the server's call sites apply
	// this budget as the effective deadline.
	reviewBudget := planreview.ReviewBudget{
		Floor: *planReviewTimeout,
		PerKB: *reviewBudgetPerKB,
		Cap:   *reviewBudgetCap,
	}
	logger.Info("review budget resolved",
		slog.Duration("floor", reviewBudget.Floor),
		slog.Duration("per_kb", reviewBudget.PerKB),
		slog.Duration("cap", reviewBudget.Cap),
		slog.String("ref", "#747"))

	cfg := server.Config{Addr: *addr, Logger: logger, ExternalURL: *externalURL, SpendAlertMultiple: *spendAlertMultiple, BudgetLocation: budgetLocation, ReviewBudget: reviewBudget}

	// Plan-review agent wiring. Selection precedence (each branch logs which
	// adapter is active at startup):
	//   1. --anthropic-api-key set → anthropic.Reviewer (production SDK adapter, #572).
	//   2. else --enable-local-claude-reviewer → claudecode.Reviewer (local-mode
	//      subprocess adapter, #575): reuses the operator's existing `claude`
	//      CLI auth, no API key needed.
	//   3. else PlanReviewer stays nil and review invocations are skipped
	//      regardless of the spec's reviewers.agent value.
	switch {
	case *anthropicAPIKey != "":
		cfg.PlanReviewer = anthropic.NewReviewer(anthropic.Config{
			APIKey:    *anthropicAPIKey,
			Model:     *planReviewModel,
			MaxTokens: *planReviewMaxTokens,
			Timeout:   *planReviewTimeout,
		})
		logger.Info("plan review agent configured",
			slog.String("adapter", "anthropic"),
			slog.String("model", *planReviewModel),
			slog.Int("max_tokens", *planReviewMaxTokens),
			slog.Duration("timeout", *planReviewTimeout))
	case *enableLocalClaudeReviewer:
		reviewer := claudecode.NewReviewer(claudecode.Config{
			Binary:    *localClaudeBinary,
			Model:     *localClaudeModel,
			MaxTokens: *planReviewMaxTokens,
			Timeout:   *planReviewTimeout,
		})
		// Apply the env-resolved retry budget past NewClient's zero->1
		// normalisation: an explicit 0 means retry disabled (single
		// attempt), which the constructor alone cannot express.
		reviewer.SetMaxRetries(*planReviewMaxRetries)
		cfg.PlanReviewer = reviewer
		logger.Info("plan review agent configured",
			slog.String("adapter", "claudecode"),
			slog.String("binary", *localClaudeBinary),
			slog.String("model", *localClaudeModel),
			slog.Int("max_tokens", *planReviewMaxTokens),
			slog.Int("max_retries", *planReviewMaxRetries),
			slog.Duration("timeout", *planReviewTimeout))
	default:
		// #574 / ADR-027: tightened from the plain "gateless" warning so
		// the operator can predict what a workflow declaring
		// reviewers.agent > 0 will do with no reviewer wired — fail
		// dispatch up front in gating mode, skip with an audit trail in
		// advisory mode.
		logger.Warn("plan-review agent not configured (set FISHHAWKD_ANTHROPIC_API_KEY, or FISHHAWKD_ENABLE_LOCAL_CLAUDE_REVIEWER for local mode, to enable); any workflow declaring reviewers.agent > 0 will fail dispatch in gating mode and skip with a plan_review_skipped audit entry in advisory mode")
	}

	// Wire the run repository when a DB URL is supplied. Without
	// one the server still boots — /healthz works and any
	// repository-dependent handler returns 503 — so operators can
	// smoke-test a deploy before pointing it at production data.
	var pool *pgxpool.Pool
	if *dbURL != "" {
		var err error
		pool, err = pgxpool.New(context.Background(), *dbURL)
		if err != nil {
			logger.Error("db pool create failed", slog.String("error", err.Error()))
			return exitFailure
		}
		defer pool.Close()
		cfg.RunRepo = runpkg.NewPostgresRepository(pool)
		cfg.SigningRepo = signing.NewPostgresRepository(pool)
		cfg.AuditRepo = audit.NewPostgresRepository(pool)
		cfg.ApprovalRepo = approval.NewPostgresRepository(pool)
		cfg.ArtifactRepo = artifact.NewPostgresRepository(pool)
		cfg.StageCheckRepo = stagecheck.NewPostgresRepository(pool)
		cfg.APITokenRepo = apitoken.NewPostgresRepository(pool)
		cfg.MCPTokenRepo = mcptoken.NewPostgresRepository(pool)
		cfg.AuthRepo = authpkg.NewPostgresRepository(pool)
		logger.Info("repositories configured (run + signing + audit + approval + artifact + stagecheck + apitoken + auth)", slog.String("driver", "postgres"))
	} else {
		logger.Warn("FISHHAWKD_DATABASE_URL not set; /v0/runs and /v0/runs/{id}/signing-key endpoints will respond 503")
	}

	// Trace storage wiring. The S3 client uses path-style requests
	// so the same code works against AWS S3 and MinIO. An empty
	// bucket leaves /v0/runs/{id}/trace at 503.
	if *s3Bucket != "" {
		awsCfg, err := awsconfig.LoadDefaultConfig(context.Background(),
			awsconfig.WithRegion(*s3Region))
		if err != nil {
			logger.Error("aws config failed", slog.String("error", err.Error()))
			return exitFailure
		}
		client := s3.NewFromConfig(awsCfg, func(o *s3.Options) {
			if *s3Endpoint != "" {
				o.BaseEndpoint = aws.String(*s3Endpoint)
			}
			o.UsePathStyle = true
		})
		cfg.TraceStore = tracestore.NewS3Storage(client, *s3Bucket)
		logger.Info("trace store configured",
			slog.String("bucket", *s3Bucket),
			slog.String("region", *s3Region),
			slog.String("endpoint", *s3Endpoint))
	} else {
		logger.Warn("FISHHAWKD_S3_BUCKET not set; /v0/runs/{id}/trace will respond 503")
	}

	// Webhook receiver wiring. Secret + delivery store both need
	// to be configured for /webhooks/github to accept deliveries.
	// 24h retention covers GitHub's ~3h retry window with
	// comfortable margin without growing unboundedly.
	//
	// Prefer the Postgres-backed store when a DB pool is available:
	// dedup state survives restarts and is shared across instances
	// (a hard requirement for any horizontally-scaled deploy). Fall
	// back to MemoryStore only when no DB is configured, with a
	// noisy warning so an operator running multi-instance with
	// memory dedup can spot the hazard.
	const webhookRetention = 24 * time.Hour
	var webhookEvictor *webhook.PostgresStore
	if *webhookSecret != "" {
		cfg.GitHubWebhookSecret = []byte(*webhookSecret)
		if pool != nil {
			pgStore := webhook.NewPostgresStore(pool)
			cfg.WebhookDeliveries = pgStore
			webhookEvictor = pgStore
			logger.Info("github webhook receiver configured (postgres dedup)")
		} else {
			cfg.WebhookDeliveries = webhook.NewMemoryStore(webhookRetention)
			logger.Warn("github webhook receiver using memory dedup — NOT safe for multi-instance deploys; set FISHHAWKD_DATABASE_URL")
		}
	} else {
		logger.Warn("FISHHAWKD_GITHUB_WEBHOOK_SECRET not set; /webhooks/github will respond 503")
	}

	// GitHub App installation-token provider. Both ID and key file
	// must be set; either alone is a misconfiguration. Wired before
	// the webhook dispatcher / orchestrator below because both
	// capture cfg.GitHub at construction time — initializing them
	// before the App is set produces a silently-degraded backend
	// that accepts webhooks but never creates Run records.
	if *githubAppIDStr != "" || *githubAppKeyFile != "" {
		if *githubAppIDStr == "" || *githubAppKeyFile == "" {
			logger.Error("github app misconfigured: both --github-app-id and --github-app-private-key-file required")
			return exitFailure
		}
		appID, err := strconv.ParseInt(*githubAppIDStr, 10, 64)
		if err != nil || appID <= 0 {
			logger.Error("github app id invalid", slog.String("got", *githubAppIDStr))
			return exitFailure
		}
		keyBytes, err := os.ReadFile(*githubAppKeyFile)
		if err != nil {
			logger.Error("github app key read failed", slog.String("error", err.Error()))
			return exitFailure
		}
		signer, err := githubapp.NewSignerFromPEM(appID, keyBytes)
		if err != nil {
			logger.Error("github app key parse failed", slog.String("error", err.Error()))
			return exitFailure
		}
		cfg.GitHubTokens = githubapp.NewCachedProvider(githubapp.NewClient(signer))
		cfg.GitHub = githubclient.NewWithSigner(cfg.GitHubTokens, signer)
		logger.Info("github app + REST client configured",
			slog.Int64("app_id", appID))
	} else {
		logger.Warn("FISHHAWKD_GITHUB_APP_ID not set; webhook dispatch and GitHub-side actions will be disabled")
	}

	// Webhook dispatcher requires both the GitHub REST client (for
	// fetching the workflow spec + firing workflow_dispatch) and a
	// run repository (for creating Run records). Without either,
	// the webhook receiver still accepts deliveries but they
	// don't produce runs — useful for early dev against a backend
	// that hasn't been GitHub-wired yet.
	if cfg.GitHub != nil && cfg.RunRepo != nil && cfg.AuditRepo != nil {
		// Issue-comment notifier (#234). nil when ExternalURL is
		// empty; the dispatcher then skips the pickup-ack step
		// silently. Built once + shared between the dispatcher
		// (pickup ack) and the trace handler's plan-ready hook
		// (which goes through Server.issueNotifier separately).
		notifier := issuecomment.New(issuecomment.Deps{
			GitHub:      cfg.GitHub,
			Runs:        cfg.RunRepo,
			Audit:       cfg.AuditRepo,
			ExternalURL: cfg.ExternalURL,
		})
		cfg.WebhookDispatcher = &webhook.Dispatcher{
			GitHub:        cfg.GitHub,
			Runs:          cfg.RunRepo,
			Audit:         cfg.AuditRepo,
			Artifacts:     cfg.ArtifactRepo,
			Logger:        logger,
			IssueNotifier: notifier,
			// PlanReviewerConfigured mirrors the run-create guard's
			// `cfg.PlanReviewer == nil` check (#574) so the webhook-
			// dispatcher path refuses an agent-gated plan stage with
			// no reviewer wired (#577 / ADR-027). cfg.PlanReviewer is
			// resolved earlier (~L167) from the anthropic/claudecode
			// reviewer selection.
			PlanReviewerConfigured: cfg.PlanReviewer != nil,
			// BudgetLocation feeds the blocking periodic-budget
			// admission gate (#688 / ADR-030), shared with the
			// server's cfg.BudgetLocation so both admission seams
			// bucket spend into the same calendar window.
			BudgetLocation: budgetLocation,
			// ApprovalHandler is wired below after the Server
			// is constructed — the Server implements the
			// interface and holds all the deps the handler
			// needs (approval repo, role resolver, stage-check
			// repo, etc.).
		}
		logger.Info("webhook dispatcher configured")
	}

	// Orchestrator wires the run repository to the GitHub client
	// to dispatch subsequent stages after a gate passes. Same
	// dependencies as the dispatcher; without them the approval
	// handler succeeds but the next stage stays in pending.
	//
	// Artifacts + Audit enable the ADR-025 D4 decomposition fanout:
	// when the approved plan declares sub_plans, the orchestrator
	// mints child runs and parks the parent's implement stage in
	// awaiting_children. Either being nil disables the fanout
	// silently — the parent's implement stage dispatches as today.
	if cfg.RunRepo != nil {
		cfg.Orchestrator = &orchestrator.Orchestrator{
			Runs:      cfg.RunRepo,
			GitHub:    cfg.GitHub, // nil-safe; orchestrator skips dispatch when GitHub is nil
			Logger:    logger,
			Artifacts: cfg.ArtifactRepo,
			Audit:     cfg.AuditRepo,
		}
		logger.Info("stage orchestrator configured")
	}

	// OIDC verification on the signing-key endpoint. Off when no
	// audience is configured — that's the v0 self-execution
	// posture. With an audience, every signing-key request must
	// carry a GitHub-signed JWT whose claims bind to the run's
	// repo + workflow_id.
	if *oidcAudience != "" {
		if *oidcJWKSURL != "" {
			cfg.OIDCVerifier = githuboidc.NewWithJWKSURL(*oidcJWKSURL)
			logger.Info("OIDC verifier configured (custom JWKS URL)",
				slog.String("audience", *oidcAudience),
				slog.String("jwks_url", *oidcJWKSURL))
		} else {
			cfg.OIDCVerifier = githuboidc.New()
			logger.Info("OIDC verifier configured",
				slog.String("audience", *oidcAudience))
		}
		cfg.OIDCAudience = *oidcAudience
	} else {
		logger.Warn("FISHHAWKD_OIDC_AUDIENCE not set; signing-key endpoint accepts unauthenticated requests")
	}

	// Role resolver for the approval handler. Wired only when the
	// GitHub client is configured — without it, ListTeamMembers
	// can't run, and the approval handler falls back to "any
	// authenticated subject can approve" (the v0 demo posture).
	if cfg.GitHub != nil {
		cfg.RoleResolver = role.NewResolver(githubTeamListerAdapter{cfg.GitHub})
		logger.Info("role resolver configured")
	} else {
		logger.Warn("role resolver not configured: approval handler will accept any authenticated subject")
	}

	// GitHub OAuth sign-in (E4.2). All three of client_id +
	// client_secret + callback_url must be set; mismatched
	// configuration logs an error and exits rather than running
	// half-configured.
	if *oauthClientID != "" || *oauthClientSecret != "" || *oauthCallbackURL != "" {
		if *oauthClientID == "" || *oauthClientSecret == "" || *oauthCallbackURL == "" {
			logger.Error("oauth misconfigured: --oauth-client-id, --oauth-client-secret, --oauth-callback-url must all be set")
			return exitFailure
		}
		cfg.GitHubOAuth = authpkg.NewGitHubOAuth(
			*oauthClientID, *oauthClientSecret, *oauthCallbackURL, authpkg.OAuthURLs{})
		cfg.AuthRedirectAfterLogin = *oauthRedirectAfterLogin
		logger.Info("github oauth sign-in configured",
			slog.String("callback_url", *oauthCallbackURL),
			slog.String("redirect_after_login", *oauthRedirectAfterLogin))
	} else {
		logger.Warn("FISHHAWKD_OAUTH_CLIENT_ID not set; /v0/auth/github/login + /callback respond 503")
	}

	// GitHub App manifest-flow client (E4.7). No credentials needed —
	// the conversions endpoint accepts the one-shot `code` and
	// returns App credentials in one shot. Always wired so operators
	// can self-register an App from a fresh install.
	cfg.GitHubManifest = authpkg.NewGitHubManifest(authpkg.ManifestURLs{})

	srv := server.New(cfg)

	// Wire the slash-command approval handler now that the Server
	// exists (#238). The dispatcher was constructed earlier without
	// this field; we plug it in here so the dispatcher's nil-check
	// stays honest when slash-command-approval deps aren't ready.
	if cfg.WebhookDispatcher != nil {
		cfg.WebhookDispatcher.ApprovalHandler = srv
		logger.Info("slash-command approval handler wired")
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// Start the webhook dedup evictor when the Postgres store is
	// in use. 1h tick is fine for 24h retention — eviction lag of
	// up to an hour past TTL is harmless (rows just sit a bit
	// longer; dedup behavior is unchanged).
	if webhookEvictor != nil {
		go runWebhookEvictor(ctx, logger, webhookEvictor, webhookRetention)
		logger.Info("webhook dedup evictor started",
			slog.Duration("retention", webhookRetention))
	}

	// Start the approval SLA timeout ticker if requested. Requires
	// run + audit repos; we skip with a warn if either is missing
	// rather than failing the boot, so a partial deploy still
	// serves /healthz and read-only endpoints.
	if *enableSLATimer {
		if cfg.RunRepo == nil || cfg.AuditRepo == nil {
			logger.Warn("--enable-sla-timer set but RunRepo or AuditRepo unconfigured; ticker not started")
		} else {
			ticker := &slaTickerConfig{
				Repo:     cfg.RunRepo,
				Audit:    cfg.AuditRepo,
				Advance:  advanceFuncFor(cfg.Orchestrator),
				Logger:   logger,
				Interval: *slaInterval,
			}
			go ticker.Start(ctx)
			logger.Info("approval SLA timeout ticker started",
				slog.Duration("interval", *slaInterval))
		}
	}

	// Same off-by-default story for the dispatch watchdog (E8.4).
	// Stages stuck in 'dispatched' past --dispatch-watchdog-timeout
	// are transitioned to failed-C and an audit entry is appended.
	if *enableDispatchWatchdog {
		if cfg.RunRepo == nil || cfg.AuditRepo == nil {
			logger.Warn("--enable-dispatch-watchdog set but RunRepo or AuditRepo unconfigured; ticker not started")
		} else {
			ticker := &dispatchwatchdog.Ticker{
				Repo:     cfg.RunRepo,
				Audit:    cfg.AuditRepo,
				Advance:  advanceFuncFor(cfg.Orchestrator),
				Logger:   logger,
				Interval: *dispatchWatchdogInterval,
				Timeout:  *dispatchWatchdogTimeout,
			}
			go func() {
				if err := ticker.Run(ctx); err != nil {
					logger.Error("dispatch watchdog exited with error", slog.String("error", err.Error()))
				}
			}()
			logger.Info("dispatch watchdog started",
				slog.Duration("interval", *dispatchWatchdogInterval),
				slog.Duration("timeout", *dispatchWatchdogTimeout))
		}
	}

	// Reaction-polling worker (#360). Catches the 👍-as-approval
	// path GitHub doesn't deliver via webhooks. Off by default; on
	// requires RunRepo + AuditRepo + a GitHub client + a server
	// implementing the approval handler. Same fall-through posture
	// as the SLA / dispatch watchdog tickers.
	if *enableReactionPoller {
		switch {
		case cfg.RunRepo == nil || cfg.AuditRepo == nil:
			logger.Warn("--enable-reaction-poller set but RunRepo or AuditRepo unconfigured; ticker not started")
		case cfg.GitHub == nil:
			logger.Warn("--enable-reaction-poller set but GitHub client unconfigured (no app id?); ticker not started")
		default:
			ticker := &reactionpoller.Ticker{
				Runs:         cfg.RunRepo,
				Audit:        cfg.AuditRepo,
				Reactions:    cfg.GitHub,
				Approvals:    srv,
				Logger:       logger,
				FastInterval: *reactionPollerFastInterval,
				SlowInterval: *reactionPollerSlowInterval,
				AgeThreshold: *reactionPollerAgeThreshold,
			}
			go func() {
				if err := ticker.Run(ctx); err != nil {
					logger.Error("reaction poller exited with error", slog.String("error", err.Error()))
				}
			}()
			logger.Info("reaction poller started",
				slog.Duration("fast_interval", *reactionPollerFastInterval),
				slog.Duration("slow_interval", *reactionPollerSlowInterval),
				slog.Duration("age_threshold", *reactionPollerAgeThreshold))
		}
	}

	// Merge-status reconciler (ADR-031 Phase 1). Catch-net for a
	// missed pull_request.closed webhook: resolves a review gate on a
	// verified PR merge state through the SAME path the webhook uses.
	// Off by default; on requires RunRepo + AuditRepo + a GitHub client
	// + the server (Resolver). Same fall-through posture as the other
	// tickers.
	if *enableMergeReconciler {
		switch {
		case cfg.RunRepo == nil || cfg.AuditRepo == nil:
			logger.Warn("--enable-merge-reconciler set but RunRepo or AuditRepo unconfigured; ticker not started")
		case cfg.GitHub == nil:
			logger.Warn("--enable-merge-reconciler set but GitHub client unconfigured (no app id?); ticker not started")
		default:
			ticker := &mergereconciler.Ticker{
				Runs:     cfg.RunRepo,
				PRGetter: cfg.GitHub,
				Resolver: srv,
				Logger:   logger,
				Interval: *mergeReconcilerInterval,
			}
			go func() {
				if err := ticker.Run(ctx); err != nil {
					logger.Error("merge reconciler exited with error", slog.String("error", err.Error()))
				}
			}()
			logger.Info("merge-status reconciler started",
				slog.Duration("interval", *mergeReconcilerInterval))
		}
	}

	// One-shot startup run-completion recovery (ADR-031 chain, #727).
	// The merge-resolution path used to transition the review stage
	// without completing the run, leaving runs stuck {all stages
	// terminal, run non-terminal} forever. ReconcileStuckRuns advances
	// only runs whose stages are already all-terminal, so it is a cheap
	// idempotent self-heal on every boot. Run unconditionally (gated only
	// on the wiring); best-effort — a recovery failure logs at warn and
	// never blocks server start.
	if cfg.Orchestrator != nil && cfg.RunRepo != nil {
		if n, err := cfg.Orchestrator.ReconcileStuckRuns(ctx); err != nil {
			logger.Warn("startup stuck-run reconciliation failed", slog.String("error", err.Error()))
		} else if n > 0 {
			logger.Info("startup stuck-run reconciliation completed", slog.Int("rescued", n))
		}
	}

	// Child-completion sweeper (#455 / ADR-025 D4). Resolves parent
	// stages parked in awaiting_children when every decomposed
	// child run reaches a terminal state. Off by default for the
	// same dev-loop reason as the SLA / dispatch watchdog tickers.
	if *enableChildCompletionSweeper {
		switch {
		case cfg.RunRepo == nil || cfg.AuditRepo == nil:
			logger.Warn("--enable-child-completion-sweeper set but RunRepo or AuditRepo unconfigured; sweeper not started")
		case cfg.Orchestrator == nil:
			logger.Warn("--enable-child-completion-sweeper set but Orchestrator unconfigured; sweeper not started")
		default:
			sweeper := &childcompletion.Sweeper{
				Runs:     cfg.RunRepo,
				Audit:    cfg.AuditRepo,
				Advance:  childCompletionAdvancer{cfg.Orchestrator},
				Logger:   logger,
				Interval: *childCompletionInterval,
			}
			go func() {
				if err := sweeper.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
					logger.Error("child-completion sweeper exited with error", slog.String("error", err.Error()))
				}
			}()
			logger.Info("child-completion sweeper started",
				slog.Duration("interval", *childCompletionInterval))
		}
	}

	errCh := make(chan error, 1)
	go func() { errCh <- srv.Start() }()

	select {
	case <-ctx.Done():
		logger.Info("shutdown signal received")
	case err := <-errCh:
		if err != nil && !errors.Is(err, context.Canceled) {
			logger.Error("server start failed", slog.String("error", err.Error()))
			return exitFailure
		}
	}

	if err := srv.Shutdown(context.Background()); err != nil {
		logger.Error("shutdown failed", slog.String("error", err.Error()))
		return exitFailure
	}
	logger.Info("shutdown complete")
	return exitOK
}

// githubTeamListerAdapter bridges *githubclient.Client (whose
// ListTeamMembers returns []githubclient.TeamMember) and
// role.TeamLister (whose method returns []role.TeamMember). Pure
// type-conversion glue; the two struct shapes are byte-identical.
type githubTeamListerAdapter struct {
	c *githubclient.Client
}

func (a githubTeamListerAdapter) ListTeamMembers(ctx context.Context, installationID int64, org, slug string) ([]role.TeamMember, error) {
	got, err := a.c.ListTeamMembers(ctx, installationID, org, slug)
	if err != nil {
		return nil, err
	}
	out := make([]role.TeamMember, 0, len(got))
	for _, m := range got {
		out = append(out, role.TeamMember{Login: m.Login, ID: m.ID})
	}
	return out, nil
}

// runWebhookEvictor periodically deletes webhook_deliveries rows
// older than retention. 1h tick is fine for 24h retention — a row
// sitting up to an hour past TTL is harmless because dedup
// behavior is unchanged (the row was already evictable; we just
// haven't reclaimed space yet). Exits when ctx is cancelled.
func runWebhookEvictor(ctx context.Context, logger *slog.Logger, store *webhook.PostgresStore, retention time.Duration) {
	const interval = time.Hour
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	evict := func() {
		evictCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		n, err := store.Evict(evictCtx, time.Now().UTC().Add(-retention))
		if err != nil {
			logger.LogAttrs(ctx, slog.LevelWarn, "webhook evict failed",
				slog.String("error", err.Error()))
			return
		}
		if n > 0 {
			logger.LogAttrs(ctx, slog.LevelInfo, "webhook evict",
				slog.Int64("rows", n),
				slog.Duration("retention", retention))
		}
	}

	// Fire once at startup so a long-lived deployment that just
	// restarted catches up on accumulated rows without waiting the
	// full interval.
	evict()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			evict()
		}
	}
}

// slaTickerConfig wraps the inputs sla.Ticker needs so serve.go
// doesn't import internal/sla directly until ticker startup time.
// Keeps the import surface narrow and avoids a serve-startup cost
// when the feature flag is off.
type slaTickerConfig struct {
	Repo     runpkg.Repository
	Audit    audit.Repository
	Advance  func(ctx context.Context, runID uuid.UUID) error
	Logger   *slog.Logger
	Interval time.Duration
}

func (c *slaTickerConfig) Start(ctx context.Context) {
	t := &slapkg.Ticker{
		Repo:     c.Repo,
		Audit:    c.Audit,
		Advance:  c.Advance,
		Logger:   c.Logger,
		Interval: c.Interval,
	}
	if err := t.Run(ctx); err != nil {
		c.Logger.Error("sla ticker exited with error", slog.String("error", err.Error()))
	}
}

// childCompletionAdvancer adapts *orchestrator.Orchestrator to the
// childcompletion.Advancer interface (Advance returning just an
// error). Keeps childcompletion's import graph clean of orchestrator
// internals like Outcome.
type childCompletionAdvancer struct {
	o *orchestrator.Orchestrator
}

func (a childCompletionAdvancer) Advance(ctx context.Context, runID uuid.UUID) error {
	if a.o == nil {
		return nil
	}
	_, err := a.o.Advance(ctx, runID)
	return err
}

// advanceFuncFor wraps the orchestrator's Advance method as a plain
// `func(ctx, runID) error` so the SLA + dispatch-watchdog tickers
// can depend on the behaviour without forcing their packages to
// import orchestrator.Outcome. Returns nil when the orchestrator
// is unconfigured — the tickers tolerate a nil Advance and fall
// back to "fail the stage and log the run-state gap."
func advanceFuncFor(o *orchestrator.Orchestrator) func(ctx context.Context, runID uuid.UUID) error {
	if o == nil {
		return nil
	}
	return func(ctx context.Context, runID uuid.UUID) error {
		_, err := o.Advance(ctx, runID)
		return err
	}
}

// newLogger returns a slog logger writing JSON to logSink with the
// service / version pair pre-attached.
func newLogger(logSink io.Writer) *slog.Logger {
	logger := slog.New(slog.NewJSONHandler(logSink, &slog.HandlerOptions{Level: slog.LevelInfo}))
	logger = logger.With(
		slog.String("service", "fishhawkd"),
		slog.String("version", version.Version),
	)
	slog.SetDefault(logger)
	return logger
}
