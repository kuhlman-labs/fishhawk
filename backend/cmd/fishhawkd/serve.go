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
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kuhlman-labs/fishhawk/backend/internal/apitoken"
	"github.com/kuhlman-labs/fishhawk/backend/internal/approval"
	"github.com/kuhlman-labs/fishhawk/backend/internal/artifact"
	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	authpkg "github.com/kuhlman-labs/fishhawk/backend/internal/auth"
	"github.com/kuhlman-labs/fishhawk/backend/internal/githubapp"
	"github.com/kuhlman-labs/fishhawk/backend/internal/githubclient"
	"github.com/kuhlman-labs/fishhawk/backend/internal/githuboidc"
	"github.com/kuhlman-labs/fishhawk/backend/internal/orchestrator"
	"github.com/kuhlman-labs/fishhawk/backend/internal/role"
	runpkg "github.com/kuhlman-labs/fishhawk/backend/internal/run"
	"github.com/kuhlman-labs/fishhawk/backend/internal/server"
	"github.com/kuhlman-labs/fishhawk/backend/internal/signing"
	slapkg "github.com/kuhlman-labs/fishhawk/backend/internal/sla"
	"github.com/kuhlman-labs/fishhawk/backend/internal/tracestore"
	"github.com/kuhlman-labs/fishhawk/backend/internal/version"
	"github.com/kuhlman-labs/fishhawk/backend/internal/webhook"

	"os"
	"strconv"
)

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
	if err := fs.Parse(args); err != nil {
		return exitFailure
	}

	logger := newLogger(logSink)

	cfg := server.Config{Addr: *addr, Logger: logger}

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
		cfg.APITokenRepo = apitoken.NewPostgresRepository(pool)
		cfg.AuthRepo = authpkg.NewPostgresRepository(pool)
		logger.Info("repositories configured (run + signing + audit + approval + artifact + apitoken + auth)", slog.String("driver", "postgres"))
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

	// Webhook dispatcher requires both the GitHub REST client (for
	// fetching the workflow spec + firing workflow_dispatch) and a
	// run repository (for creating Run records). Without either,
	// the webhook receiver still accepts deliveries but they
	// don't produce runs — useful for early dev against a backend
	// that hasn't been GitHub-wired yet.
	if cfg.GitHub != nil && cfg.RunRepo != nil && cfg.AuditRepo != nil {
		cfg.WebhookDispatcher = &webhook.Dispatcher{
			GitHub: cfg.GitHub,
			Runs:   cfg.RunRepo,
			Audit:  cfg.AuditRepo,
			Logger: logger,
		}
		logger.Info("webhook dispatcher configured")
	}

	// Orchestrator wires the run repository to the GitHub client
	// to dispatch subsequent stages after a gate passes. Same
	// dependencies as the dispatcher; without them the approval
	// handler succeeds but the next stage stays in pending.
	if cfg.RunRepo != nil {
		cfg.Orchestrator = &orchestrator.Orchestrator{
			Runs:   cfg.RunRepo,
			GitHub: cfg.GitHub, // nil-safe; orchestrator skips dispatch when GitHub is nil
			Logger: logger,
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

	// GitHub App installation-token provider. Both ID and key file
	// must be set; either alone is a misconfiguration. Currently
	// no handlers consume cfg.GitHubTokens — the dispatcher (#109)
	// will pick it up — but wire it now so future handlers find it
	// already constructed.
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
		cfg.GitHub = githubclient.New(cfg.GitHubTokens)
		logger.Info("github app + REST client configured",
			slog.Int64("app_id", appID))
	} else {
		logger.Warn("FISHHAWKD_GITHUB_APP_ID not set; webhook dispatch and GitHub-side actions will be disabled")
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
		logger.Warn("FISHHAWKD_OAUTH_CLIENT_ID not set; /v0/auth/github/* endpoints respond 503")
	}

	srv := server.New(cfg)

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
				Logger:   logger,
				Interval: *slaInterval,
			}
			go ticker.Start(ctx)
			logger.Info("approval SLA timeout ticker started",
				slog.Duration("interval", *slaInterval))
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
	Logger   *slog.Logger
	Interval time.Duration
}

func (c *slaTickerConfig) Start(ctx context.Context) {
	t := &slapkg.Ticker{
		Repo:     c.Repo,
		Audit:    c.Audit,
		Logger:   c.Logger,
		Interval: c.Interval,
	}
	if err := t.Run(ctx); err != nil {
		c.Logger.Error("sla ticker exited with error", slog.String("error", err.Error()))
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
