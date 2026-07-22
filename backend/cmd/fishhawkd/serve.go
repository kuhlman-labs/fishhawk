package main

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os/signal"
	"sort"
	"syscall"

	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kuhlman-labs/fishhawk/backend/internal/account"
	accountdb "github.com/kuhlman-labs/fishhawk/backend/internal/account/db"
	"github.com/kuhlman-labs/fishhawk/backend/internal/anthropic"
	"github.com/kuhlman-labs/fishhawk/backend/internal/apitoken"
	"github.com/kuhlman-labs/fishhawk/backend/internal/approval"
	"github.com/kuhlman-labs/fishhawk/backend/internal/artifact"
	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	authpkg "github.com/kuhlman-labs/fishhawk/backend/internal/auth"
	"github.com/kuhlman-labs/fishhawk/backend/internal/campaign"
	"github.com/kuhlman-labs/fishhawk/backend/internal/campaigndriver"
	"github.com/kuhlman-labs/fishhawk/backend/internal/childcompletion"
	"github.com/kuhlman-labs/fishhawk/backend/internal/claudecode"
	"github.com/kuhlman-labs/fishhawk/backend/internal/codex"
	"github.com/kuhlman-labs/fishhawk/backend/internal/concern"
	"github.com/kuhlman-labs/fishhawk/backend/internal/deployreconciler"
	dispatchwatchdog "github.com/kuhlman-labs/fishhawk/backend/internal/dispatchwatchdog"
	"github.com/kuhlman-labs/fishhawk/backend/internal/drive"
	"github.com/kuhlman-labs/fishhawk/backend/internal/forge"
	forgegithub "github.com/kuhlman-labs/fishhawk/backend/internal/forge/github"
	forgegitlab "github.com/kuhlman-labs/fishhawk/backend/internal/forge/gitlab"
	"github.com/kuhlman-labs/fishhawk/backend/internal/githubapp"
	"github.com/kuhlman-labs/fishhawk/backend/internal/githubclient"
	"github.com/kuhlman-labs/fishhawk/backend/internal/githuboidc"
	"github.com/kuhlman-labs/fishhawk/backend/internal/gitlabclient"
	"github.com/kuhlman-labs/fishhawk/backend/internal/identity"
	"github.com/kuhlman-labs/fishhawk/backend/internal/invariantmonitor"
	"github.com/kuhlman-labs/fishhawk/backend/internal/issuecomment"
	"github.com/kuhlman-labs/fishhawk/backend/internal/jiraclient"
	"github.com/kuhlman-labs/fishhawk/backend/internal/mcptoken"
	"github.com/kuhlman-labs/fishhawk/backend/internal/mergereconciler"
	"github.com/kuhlman-labs/fishhawk/backend/internal/modeloracle"
	"github.com/kuhlman-labs/fishhawk/backend/internal/onboarding"
	"github.com/kuhlman-labs/fishhawk/backend/internal/operatorrole"
	"github.com/kuhlman-labs/fishhawk/backend/internal/orchestrator"
	"github.com/kuhlman-labs/fishhawk/backend/internal/plan"
	"github.com/kuhlman-labs/fishhawk/backend/internal/planreview"
	"github.com/kuhlman-labs/fishhawk/backend/internal/reactionpoller"
	"github.com/kuhlman-labs/fishhawk/backend/internal/refinement"
	"github.com/kuhlman-labs/fishhawk/backend/internal/reviewresolver"
	"github.com/kuhlman-labs/fishhawk/backend/internal/role"
	runpkg "github.com/kuhlman-labs/fishhawk/backend/internal/run"
	"github.com/kuhlman-labs/fishhawk/backend/internal/scopeamendment"
	"github.com/kuhlman-labs/fishhawk/backend/internal/server"
	"github.com/kuhlman-labs/fishhawk/backend/internal/signing"
	slapkg "github.com/kuhlman-labs/fishhawk/backend/internal/sla"
	"github.com/kuhlman-labs/fishhawk/backend/internal/spendalert"
	"github.com/kuhlman-labs/fishhawk/backend/internal/stagecheck"
	"github.com/kuhlman-labs/fishhawk/backend/internal/tracestore"
	"github.com/kuhlman-labs/fishhawk/backend/internal/version"
	"github.com/kuhlman-labs/fishhawk/backend/internal/webhook"
	"github.com/kuhlman-labs/fishhawk/backend/internal/workmgmt"

	"os"
	"strconv"
	"strings"
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

// planReviewerOptions carries the resolved flag/env values that select and
// configure the plan-review adapters. Grouping them lets resolvePlanReviewers
// be a pure function the selection-seam test can drive without booting a
// server.
type planReviewerOptions struct {
	anthropicAPIKey           string
	planReviewModel           string
	enableLocalClaudeReviewer bool
	localClaudeBinary         string
	localClaudeModel          string
	enableCodexReviewer       bool
	codexBinary               string
	codexModel                string
	codexEffort               string
	openAIAPIKey              string
	planReviewMaxTokens       int
	planReviewMaxRetries      int
	planReviewTimeout         time.Duration
	// modelBaseURL / modelAPIKey are the region-scoped inference endpoint and
	// credential (ADR-062, E44.7 / #1831). They govern the Anthropic SDK
	// adapter only — the claudecode and codex adapters are subprocesses whose
	// endpoint is the CLI's own configuration, not ours. Both empty is the
	// single-cell posture: the SDK default endpoint, the anthropic API key.
	modelBaseURL string
	modelAPIKey  string
}

// inferenceAPIKey resolves the credential the Anthropic SDK adapter presents.
// The region-scoped key wins when set; otherwise the deployment's
// FISHHAWKD_ANTHROPIC_API_KEY is used unchanged. It is deliberately NOT part of
// the adapter-selection precedence in Default(): a region key alone does not
// turn the anthropic reviewer on, it only redirects the credential of a
// reviewer that FISHHAWKD_ANTHROPIC_API_KEY already selected.
//
// The fallback is confined to the DEFAULT endpoint. A configured
// FISHHAWKD_MODEL_BASE_URL with no FISHHAWKD_MODEL_API_KEY is half-configured,
// and falling back there would send the deployment's Anthropic credential —
// along with plan and review text — to an operator-supplied host. That is
// secret exfiltration via configurable egress, so it fails closed with an empty
// credential (the endpoint refuses the call) and a startup warning naming the
// missing key.
func (p *planReviewerOptions) inferenceAPIKey() string {
	if p.modelAPIKey != "" {
		return p.modelAPIKey
	}
	if p.modelBaseURL != "" {
		return ""
	}
	return p.anthropicAPIKey
}

// planReviewerSet implements server.ReviewerSet over the deployment's
// resolved reviewer options (#955). Every adapter whose config is present is
// available concurrently: Default() serves the bare `agent: N` count form via
// the historical precedence (anthropic > claudecode > codex), and For()
// resolves a spec-declared {provider, model} reviewer. Adapters are
// constructed per call — NewReviewer for all three backends only populates a
// config struct (no I/O, no shared mutable state), so building a
// model-overridden instance per resolve is cheap and keeps the set stateless.
type planReviewerSet struct {
	opts planReviewerOptions
}

func (p *planReviewerSet) newAnthropic(model string) server.PlanReviewer {
	reviewer := anthropic.NewReviewer(anthropic.Config{
		// Region-scoped inference (ADR-062, E44.7 / #1831): both the endpoint
		// and the credential come from the cell's own config, so plan- and
		// implement-review text for this cell's accounts never leaves the
		// region. Both empty is the single-cell posture (SDK default endpoint,
		// deployment anthropic key) — byte-identical to before.
		APIKey:    p.opts.inferenceAPIKey(),
		BaseURL:   p.opts.modelBaseURL,
		Model:     model,
		MaxTokens: p.opts.planReviewMaxTokens,
		Timeout:   p.opts.planReviewTimeout,
	})
	// Apply the env-resolved decode-retry budget (#901): a 200-response
	// carrying structurally-malformed verdict JSON re-rolls the Messages
	// call, bounded by the same FISHHAWKD_PLAN_REVIEW_MAX_RETRIES value the
	// subprocess adapters use.
	reviewer.SetMaxRetries(p.opts.planReviewMaxRetries)
	return reviewer
}

func (p *planReviewerSet) newClaudeCode(model string) server.PlanReviewer {
	reviewer := claudecode.NewReviewer(claudecode.Config{
		Binary:    p.opts.localClaudeBinary,
		Model:     model,
		MaxTokens: p.opts.planReviewMaxTokens,
		Timeout:   p.opts.planReviewTimeout,
	})
	// Apply the env-resolved retry budget past NewClient's zero->1
	// normalisation: an explicit 0 means retry disabled (single attempt),
	// which the constructor alone cannot express.
	reviewer.SetMaxRetries(p.opts.planReviewMaxRetries)
	return reviewer
}

func (p *planReviewerSet) newCodex(model, reasoningEffort string) server.PlanReviewer {
	reviewer := codex.NewReviewer(codex.Config{
		Binary: p.opts.codexBinary,
		APIKey: p.opts.openAIAPIKey,
		Model:  model,
		// Reasoning effort is now a per-reviewer ladder (#1493): the spec's
		// reviewers.agents[i].reasoning_effort wins over the deployment default
		// (FISHHAWKD_CODEX_REASONING_EFFORT). The caller resolves the rung; this
		// constructor carries the resolved value verbatim.
		ReasoningEffort: reasoningEffort,
		MaxTokens:       p.opts.planReviewMaxTokens,
		Timeout:         p.opts.planReviewTimeout,
	})
	reviewer.SetMaxRetries(p.opts.planReviewMaxRetries)
	return reviewer
}

// Default returns the precedence-selected adapter for the bare `agent: N`
// count form (anthropic > claudecode > codex — unchanged from the
// single-adapter era), or a literal nil interface (never a typed-nil) when
// no backend is configured, so the server's Default()==nil guard stays
// correct.
func (p *planReviewerSet) Default() server.PlanReviewer {
	switch {
	case p.opts.anthropicAPIKey != "":
		return p.newAnthropic(p.opts.planReviewModel)
	case p.opts.enableLocalClaudeReviewer:
		return p.newClaudeCode(p.opts.localClaudeModel)
	case p.opts.enableCodexReviewer:
		// Bare count form carries no spec reasoning_effort — resolve to the
		// deployment default (#1493), byte-identical to today.
		return p.newCodex(p.opts.codexModel, p.resolveCodexEffort(""))
	default:
		return nil
	}
}

// resolveCodexEffort applies the per-reviewer reasoning-effort ladder for the
// codex adapter (#1493): the spec-declared value wins over the deployment
// default (FISHHAWKD_CODEX_REASONING_EFFORT), an empty spec value falls back to
// the env default, and both-empty carries no override (the codex CLI then
// inherits the host ~/.codex config, byte-identical to today). It routes
// through the exported server chokepoint so the env-default rung resolves the
// same way the spec rung does.
func (p *planReviewerSet) resolveCodexEffort(specEffort string) string {
	return server.ResolveReviewerReasoningEffort(p.opts.codexEffort, specEffort).Value
}

// For resolves one spec-declared reviewer (reviewers.agents[i]) to its
// adapter, constructed with the requested model. An empty model falls back
// to that provider's deployment-configured default model. The optional
// reasoningEffort (first variadic value, empty when omitted) is codex-only
// (#1493): the codex branch resolves it through the per-reviewer
// ladder (deployment default FISHHAWKD_CODEX_REASONING_EFFORT < spec value), so
// an empty spec value falls back to the env default exactly as today; the
// anthropic/claudecode branches accept and ignore it (their adapters take no
// reasoning-effort parameter). Errors when the provider is not configured in
// this deployment, naming the env knob that enables it.
func (p *planReviewerSet) For(provider, model string, reasoningEffort ...string) (server.PlanReviewer, error) {
	effort := ""
	if len(reasoningEffort) > 0 {
		effort = reasoningEffort[0]
	}
	switch provider {
	case "anthropic":
		if p.opts.anthropicAPIKey == "" {
			return nil, fmt.Errorf("reviewer provider %q is not configured: set FISHHAWKD_ANTHROPIC_API_KEY", provider)
		}
		if model == "" {
			model = p.opts.planReviewModel
		}
		return p.newAnthropic(model), nil
	case "claudecode":
		if !p.opts.enableLocalClaudeReviewer {
			return nil, fmt.Errorf("reviewer provider %q is not configured: set FISHHAWKD_ENABLE_LOCAL_CLAUDE_REVIEWER", provider)
		}
		if model == "" {
			model = p.opts.localClaudeModel
		}
		return p.newClaudeCode(model), nil
	case "codex":
		if !p.opts.enableCodexReviewer {
			return nil, fmt.Errorf("reviewer provider %q is not configured: set FISHHAWKD_ENABLE_CODEX_REVIEWER", provider)
		}
		if model == "" {
			model = p.opts.codexModel
		}
		return p.newCodex(model, p.resolveCodexEffort(effort)), nil
	default:
		return nil, fmt.Errorf("unknown reviewer provider %q (expected anthropic, claudecode, or codex)", provider)
	}
}

// resolvePlanReviewers builds the server.ReviewerSet from opts and logs every
// configured adapter at startup (#955). Unlike the pre-#955 single-adapter
// resolver, ALL configured backends are concurrently available to the
// heterogeneous reviewers.agents spec form; the bare count form keeps the
// historical precedence via Default().
func resolvePlanReviewers(opts planReviewerOptions, logger *slog.Logger) server.ReviewerSet {
	set := &planReviewerSet{opts: opts}
	configured := 0
	if opts.anthropicAPIKey != "" {
		configured++
		logger.Info("plan review adapter configured",
			slog.String("adapter", "anthropic"),
			slog.String("model", opts.planReviewModel),
			slog.Int("max_tokens", opts.planReviewMaxTokens),
			slog.Int("max_retries", opts.planReviewMaxRetries),
			slog.Duration("timeout", opts.planReviewTimeout))
	}
	if opts.enableLocalClaudeReviewer {
		configured++
		logger.Info("plan review adapter configured",
			slog.String("adapter", "claudecode"),
			slog.String("binary", opts.localClaudeBinary),
			slog.String("model", opts.localClaudeModel),
			slog.Int("max_tokens", opts.planReviewMaxTokens),
			slog.Int("max_retries", opts.planReviewMaxRetries),
			slog.Duration("timeout", opts.planReviewTimeout))
	}
	if opts.enableCodexReviewer {
		configured++
		logger.Info("plan review adapter configured",
			slog.String("adapter", "codex"),
			slog.String("binary", opts.codexBinary),
			slog.String("model", opts.codexModel),
			slog.String("reasoning_effort", opts.codexEffort),
			slog.Int("max_tokens", opts.planReviewMaxTokens),
			slog.Int("max_retries", opts.planReviewMaxRetries),
			slog.Duration("timeout", opts.planReviewTimeout))
	}
	if configured == 0 {
		// #574 / ADR-027: tightened from the plain "gateless" warning so the
		// operator can predict what a workflow declaring reviewers.agent > 0
		// will do with no reviewer wired — fail dispatch up front in gating
		// mode, skip with an audit trail in advisory mode.
		logger.Warn("plan-review agent not configured (set FISHHAWKD_ANTHROPIC_API_KEY, or FISHHAWKD_ENABLE_LOCAL_CLAUDE_REVIEWER, or FISHHAWKD_ENABLE_CODEX_REVIEWER for local mode, to enable); any workflow declaring reviewers.agent > 0 will fail dispatch in gating mode and skip with a plan_review_skipped audit entry in advisory mode")
	}
	if opts.modelBaseURL != "" {
		// Log the endpoint, never the key. Both review paths (plan and
		// implement) share this adapter, so one line covers both.
		logger.Info("region-scoped inference endpoint configured",
			slog.String("base_url", opts.modelBaseURL),
			slog.Bool("region_scoped_key", opts.modelAPIKey != ""),
			slog.String("ref", "#1831"))
		if opts.modelAPIKey == "" {
			logger.Warn("FISHHAWKD_MODEL_BASE_URL is set without FISHHAWKD_MODEL_API_KEY; the anthropic reviewer will present NO credential to that endpoint and its calls will fail. FISHHAWKD_ANTHROPIC_API_KEY is deliberately not sent to a non-default endpoint — set FISHHAWKD_MODEL_API_KEY to the region-scoped key",
				slog.String("base_url", opts.modelBaseURL),
				slog.String("ref", "#1831"))
		}
	}
	return set
}

// regionPinOptions carries the two env values that decide whether this cell
// participates in regional handoffs (ADR-062, E44.7 / #1831).
type regionPinOptions struct {
	homeRegion    string
	handoffSecret string
}

// resolveRegionPin returns the (server.Config.HandoffSecret,
// server.Config.RegionPinner) pair for this deployment.
//
// The surface is constructed ONLY when all three inputs are present: the cell's
// own region, the shared handoff secret, and an account query surface (i.e. a
// configured database). Any one missing returns ("", nil) — the DISABLED
// posture, in which server.withRegionPin refuses a routed request carrying a
// handoff with 503 rather than serving it as though the handoff were absent.
// Disabling is logged once at startup naming what is missing, because a cell
// that silently declines to pin is indistinguishable from a healthy one until
// an account lands in the wrong region.
//
// Returning "" for the secret even when a secret WAS supplied is deliberate:
// half-configured is not a distinct posture, and carrying the secret into a
// Config with no pinner would make the fail-closed guard depend on two
// conditions instead of one.
func resolveRegionPin(opts regionPinOptions, q account.RegionPinnerQueries, logger *slog.Logger) (string, *account.RegionPinner) {
	var missing []string
	if opts.homeRegion == "" {
		missing = append(missing, "FISHHAWKD_HOME_REGION")
	}
	if opts.handoffSecret == "" {
		missing = append(missing, "FISHHAWKD_HANDOFF_SECRET")
	}
	if q == nil {
		missing = append(missing, "FISHHAWKD_DATABASE_URL")
	}
	if len(missing) > 0 {
		logger.Info("region-pin surface disabled; a routed request carrying a directory handoff will be refused with 503 (requests without fh_* parameters are unaffected)",
			slog.String("missing", strings.Join(missing, ", ")),
			slog.String("ref", "#1831"))
		return "", nil
	}
	logger.Info("region-pin surface enabled",
		slog.String("home_region", opts.homeRegion),
		slog.String("routed_path", server.RoutedOnboardingPath),
		slog.String("ref", "#1831"))
	return opts.handoffSecret, account.NewRegionPinner(q, opts.homeRegion)
}

// resolveRefinementDrafter builds the E34.2 refinement drafting agent
// (server.RefinementDrafter) from the local-claude reviewer options. It returns
// a *refinement.Drafter over a claudecode client + the default work-management
// conventions when the local claude adapter is configured, and a literal nil
// interface (never a typed-nil) otherwise — so the server's nil-Drafter guard
// degrades ONLY the agent-backed gate arms (create-session, brief-amendment) to
// 503 while GET / direct edit / decision keep working. It reuses the local
// claude binary/model already resolved for the plan reviewer.
func resolveRefinementDrafter(opts planReviewerOptions, logger *slog.Logger) server.RefinementDrafter {
	if !opts.enableLocalClaudeReviewer {
		return nil
	}
	client := claudecode.NewClient(claudecode.Config{
		Binary: opts.localClaudeBinary,
		Model:  opts.localClaudeModel,
	})
	logger.Info("refinement drafting agent configured",
		slog.String("adapter", "claudecode"),
		slog.String("binary", opts.localClaudeBinary),
		slog.String("model", opts.localClaudeModel))
	return refinement.NewDrafter(client, workmgmt.Default())
}

// resolveRefinementRepo builds the always-on Postgres refinement repository
// (E34.2 / #1593) the serve DB block wires into Config.RefinementRepo. Extracted
// as the single construction seam so the serve-wiring test drives the SAME
// production call rather than duplicating refinement.NewPostgresRepository — a
// hand-rolled duplicate would pass even if the DB block stopped populating the
// field.
func resolveRefinementRepo(pool *pgxpool.Pool) refinement.Repository {
	return refinement.NewPostgresRepository(pool)
}

// resolveGitLabClient constructs the gitlab work-item client when BOTH the
// instance base URL and access token are set (ADR-058 Phase 2, #1856),
// mirroring the jira all-or-warn gate. A partial config — exactly one of the
// two set — returns a nil client and partial=true so the caller warns and
// leaves the provider disabled (the endpoint stays 501 for provider: gitlab).
// Both empty returns nil, false (the provider is simply not configured).
// Extracted so the gating is unit-testable without booting the server.
func resolveGitLabClient(baseURL, token string) (client *gitlabclient.Client, partial bool) {
	switch {
	case baseURL != "" && token != "":
		return gitlabclient.New(baseURL, token), false
	case baseURL != "" || token != "":
		return nil, true
	default:
		return nil, false
	}
}

// resolveGitLabForge constructs the gitlab forge.Forge adapter (ADR-058 /
// E45.5, #1859) when BOTH the instance base URL and access token are set,
// mirroring resolveGitLabClient's both-or-neither gate. A partial or empty
// config returns nil so the caller leaves the forge registry without a
// gitlab entry (forge.Get("gitlab") then fails closed with an
// *UnknownForgeError). The adapter is wired with a static-token credential
// provider carrying token — the v0 group/project access-token path; the
// group-scoped OAuth broker is deferred. Extracted so the gating is
// unit-testable without booting the server.
func resolveGitLabForge(baseURL, token string) *forgegitlab.Forge {
	if baseURL == "" || token == "" {
		return nil
	}
	return forgegitlab.New(baseURL, forgegitlab.NewStaticCredentialProvider(token))
}

// webhookStoreNeeded reports whether the shared webhook delivery store
// must be created. The store is shared by the GitHub and GitLab
// receivers (delivery ids are namespaced, E45.6 / #1860), so it is
// needed when EITHER forge's webhook secret is set — a GitLab-only
// deployment needs the store just as a GitHub-only one does. Previously
// the store was gated on the GitHub secret alone.
func webhookStoreNeeded(githubSecret, gitlabSecret string) bool {
	return githubSecret != "" || gitlabSecret != ""
}

// newWebhookDeliveryStore builds the shared webhook delivery store,
// consulting webhookStoreNeeded so the gating lives in exactly one place:
// the store is created when EITHER forge's secret is set (a GitLab-only
// deployment needs it just as a GitHub-only one does), and nil is returned
// when neither is — so runServe's call site cannot re-inline a GitHub-only
// gate while leaving webhookStoreNeeded intact (E45.6 binding condition 2 /
// fix-up). Prefers the Postgres-backed store when a pool is available
// (dedup survives restarts, shared across instances) and falls back to an
// in-memory store otherwise. The second return is the concrete
// *webhook.PostgresStore for the evictor wiring — non-nil only on the
// Postgres path, nil for the memory store and the no-store case.
func newWebhookDeliveryStore(pool *pgxpool.Pool, githubSecret, gitlabSecret string, retention time.Duration) (webhook.DeliveryStore, *webhook.PostgresStore) {
	if !webhookStoreNeeded(githubSecret, gitlabSecret) {
		return nil, nil
	}
	if pool != nil {
		pg := webhook.NewPostgresStore(pool)
		return pg, pg
	}
	return webhook.NewMemoryStore(retention), nil
}

// loadConventionsOverride reads and parses the deployment-level
// work-management conventions file at path (FISHHAWKD_WORKMGMT_CONVENTIONS),
// returning the BREAK-GLASS fallback the per-repo conventions loader (#2022)
// serves when its discriminator-driven resolution falls through (ADR-058
// Phase 2, #1856). An empty path returns a nil func and nil error (no
// override — the loader falls to workmgmt.Default()). A read or parse
// failure returns a precise error naming the path + cause so runServe fails
// startup fast rather than serving a half-configured provider. Extracted so
// the read/parse/fail-fast branches are unit-testable without booting the
// server.
func loadConventionsOverride(path string) (func() (workmgmt.Conventions, bool), error) {
	if path == "" {
		return nil, nil
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read work-management conventions %q: %w", path, err)
	}
	conv, err := workmgmt.Parse(bytes.NewReader(raw))
	if err != nil {
		return nil, fmt.Errorf("parse work-management conventions %q: %w", path, err)
	}
	return func() (workmgmt.Conventions, bool) { return conv, true }, nil
}

// gitlabDeploymentScopeRef is the deployment-level credential-scope ref the
// conventions loader fetches gitlab files under. The E45.5 gitlab forge is
// wired with a static-token credential provider that ignores the ref, so the
// value only needs to be non-zero — a zero scope is the loader's
// "gitlab unconfigured" sentinel.
const gitlabDeploymentScopeRef = "gitlab:deployment"

// registeredFileFetcher resolves id from the forge registry as a
// forge.FileFetcher, or nil when the forge is absent (unconfigured) or does
// not implement the file-read capability — the per-repo conventions loader
// then treats that provider like an unregistered forge and falls through to
// its override/Default chain.
func registeredFileFetcher(id string) forge.FileFetcher {
	f, err := forge.Get(id)
	if err != nil {
		return nil
	}
	ff, ok := f.(forge.FileFetcher)
	if !ok {
		return nil
	}
	return ff
}

// buildRepoConventionsLoader assembles the per-repo work-management
// conventions loader (E45.16 / #2022) from the registered forges (each
// guarded — an absent forge yields a nil fetcher and that provider falls
// through to override/Default), the server's GitHub repo-scope resolution,
// the deployment gitlab scope, the accounts provider discriminator, and the
// break-glass override. Extracted so serve_test.go can assert the wiring
// (loader installed, override as fallback) without booting the server.
func buildRepoConventionsLoader(srv *server.Server, resolver server.ProviderResolver, override func() (workmgmt.Conventions, bool)) *server.RepoConventionsLoader {
	gitlabFetcher := registeredFileFetcher("gitlab")
	var gitlabScope forge.CredentialScope
	if gitlabFetcher != nil {
		gitlabScope = forge.FromRef(gitlabDeploymentScopeRef)
	}
	return server.NewRepoConventionsLoader(server.RepoConventionsLoaderConfig{
		Resolver:      resolver,
		GitHubFetcher: registeredFileFetcher("github"),
		GitLabFetcher: gitlabFetcher,
		GitHubScope:   srv.GitHubRepoScopeResolver(),
		GitLabScope:   gitlabScope,
		Override:      override,
	})
}

// resolveIdentityProvider is the OAuth-config gate for the forge-neutral
// identity provider (E39.1 / #1706): with an OAuth client_id present it
// constructs the GitHub device-flow + REST implementation; absent it
// returns nil so server.New falls back to the deny-by-default NoOp. token
// is the optional REST-read accessor (nil → anonymous reads). Extracted so
// the config-gated wiring seam is unit-testable without booting the server.
func resolveIdentityProvider(oauthClientID string, token func(context.Context) (string, error), apiBaseURL, oauthBaseURL string) identity.IdentityProvider {
	if oauthClientID == "" {
		return nil
	}
	var opts []identity.Option
	// Only override when the deployment configured a non-default endpoint
	// (E44.2 / #1826). Empty values leave the GitHub hosts untouched, so an
	// unconfigured deployment constructs exactly as before.
	if apiBaseURL != "" || oauthBaseURL != "" {
		opts = append(opts, identity.WithBaseURLs(apiBaseURL, oauthBaseURL))
	}
	return identity.NewGitHubIdentityProvider(oauthClientID, token, opts...)
}

// githubEndpoints holds the per-client base-URL overrides derived from the
// FISHHAWKD_GITHUB_* / FISHHAWKD_OAUTH_* endpoint config (E44.2 / #1826). An
// empty field means "unset" — each GitHub client keeps its github.com /
// api.github.com default at use time. This is the Mode 1 (per-deployment)
// realization; Mode 2 (per-installation) rides githubapp.Client.ResolveBaseURL.
type githubEndpoints struct {
	APIBaseURL       string            // githubapp.Client.BaseURL + githubclient.Client.BaseURL
	UploadBaseURL    string            // githubclient.Client.UploadBaseURL
	OAuth            authpkg.OAuthURLs // authpkg.NewGitHubOAuth urls
	IdentityAPIURL   string            // identity REST base (WithBaseURLs apiBase)
	IdentityOAuthURL string            // identity device-flow / OAuth host (WithBaseURLs oauthBase)
}

// resolveGitHubEndpoints maps the raw FISHHAWKD_* endpoint env strings onto the
// per-client override surfaces. It is pure (no I/O, no globals) so serve_test.go
// can pin both the unset-defaults and configured-hosts paths without booting the
// server. The identity provider's device-flow / OAuth host is derived from the
// scheme+host of the OAuth authorize URL (…/login/oauth/authorize lives on the
// same host as the device-code + token endpoints); an unset or unparseable
// authorize URL leaves it empty so the identity provider keeps github.com.
func resolveGitHubEndpoints(apiURL, uploadURL, authorizeURL, tokenURL, userURL, orgsURL string) githubEndpoints {
	ep := githubEndpoints{
		APIBaseURL:    apiURL,
		UploadBaseURL: uploadURL,
		OAuth: authpkg.OAuthURLs{
			AuthorizeURL: authorizeURL,
			TokenURL:     tokenURL,
			UserURL:      userURL,
			OrgsURL:      orgsURL,
		},
		IdentityAPIURL: apiURL,
	}
	if authorizeURL != "" {
		if u, err := url.Parse(authorizeURL); err == nil && u.Scheme != "" && u.Host != "" {
			ep.IdentityOAuthURL = u.Scheme + "://" + u.Host
		}
	}
	return ep
}

// resolveOperatorRepoToken builds the REST-read token accessor the forge-neutral
// identity provider uses to authenticate its permission/membership reads
// (E39.10 / #1753). The token-mint authz gate (#1708/#1709) reads
// GET /repos/{owner}/{repo}/collaborators/{login}/permission, which GitHub only
// answers for an authenticated caller with push access — so an anonymous read
// 401s and the mint handler 500s. This accessor mints/reuses the operator-repo
// installation token so those reads carry a credential.
//
// It fail-closes to nil (→ anonymous reads, preserving the pre-#1753 behavior)
// when App auth is absent (gh or tokens nil) or operatorRepo is empty / not
// "owner/name". Otherwise it returns a closure that resolves the App
// installation id for the operator repo (an App-JWT endpoint the NewWithSigner
// client already backs) then mints/reuses the per-installation token via the
// CachedProvider.
//
// Like resolveRefinementRepo / resolveIdentityProvider, this is the single
// construction seam the serve-wiring test (TestResolveOperatorRepoToken) drives,
// so a nil hand-off at the OAuth block is caught by a unit test rather than only
// surfacing as a live 500.
func resolveOperatorRepoToken(gh *githubclient.Client, tokens githubapp.TokenProvider, operatorRepo string) func(context.Context) (string, error) {
	if gh == nil || tokens == nil {
		return nil
	}
	owner, name, ok := strings.Cut(operatorRepo, "/")
	if !ok || owner == "" || name == "" || strings.Contains(name, "/") {
		return nil
	}
	repo := forge.RepoRef{Owner: owner, Name: name}
	return func(ctx context.Context) (string, error) {
		installationID, err := gh.GetRepoInstallation(ctx, repo)
		if err != nil {
			return "", fmt.Errorf("resolve operator repo installation: %w", err)
		}
		return tokens.Token(ctx, installationID)
	}
}

// buildModelProviders builds the provider→Fetcher map the live ModelOracle
// (#1341) refreshes. A provider is registered ONLY when its API key is present:
// an absent key leaves the provider UNREGISTERED, so its Snapshot reports
// ok=false and the #1339 validation fails open for it — never a boot blocker.
// Keyed under the EXISTING provider strings "claudecode"/"codex" (the same keys
// the allow-list and #1339's providerForExecutorAgent use), each fetching its
// vendor internally (claudecode→Anthropic, codex→OpenAI). The optional httpClient
// is injected by tests; production passes none so each Fetcher builds its own
// bounded-timeout client.
func buildModelProviders(anthropicKey, openaiKey string, httpClient ...*http.Client) map[string]modeloracle.Fetcher {
	var client *http.Client
	if len(httpClient) > 0 {
		client = httpClient[0]
	}
	providers := make(map[string]modeloracle.Fetcher)
	if anthropicKey != "" {
		providers["claudecode"] = modeloracle.NewAnthropicFetcher(anthropicKey, "", client)
	}
	if openaiKey != "" {
		providers["codex"] = modeloracle.NewOpenAIFetcher(openaiKey, "", client)
	}
	return providers
}

// modelProviderNames returns the registered provider keys in sorted order for a
// deterministic startup log line.
func modelProviderNames(providers map[string]modeloracle.Fetcher) []string {
	names := make([]string, 0, len(providers))
	for name := range providers {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
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
	startNonce := fs.String("start-nonce", envOr("FISHHAWKD_START_NONCE", ""),
		"per-start opaque identity token echoed by GET /healthz as start_nonce; empty omits the field. "+
			"scripts/dev sets one per spawn to prove listener identity across pid reuse (#1018)")
	dbURL := fs.String("db", envOr("FISHHAWKD_DATABASE_URL", ""),
		"postgres URL; when empty, /v0/runs endpoints respond 503")
	webhookSecret := fs.String("github-webhook-secret",
		envOr("FISHHAWKD_GITHUB_WEBHOOK_SECRET", ""),
		"shared secret GitHub uses to HMAC-sign webhook deliveries; when empty, /webhooks/github responds 503")
	gitlabWebhookSecret := fs.String("gitlab-webhook-secret",
		envOr("FISHHAWKD_GITLAB_WEBHOOK_SECRET", ""),
		"secret token GitLab sends VERBATIM in X-Gitlab-Token (no HMAC); when empty, /webhooks/gitlab responds 503")
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
	projectsToken := fs.String("projects-token",
		envOr("FISHHAWKD_PROJECTS_TOKEN", ""),
		"optional user PAT/UAT with the `project` scope; lets fishhawk_file_issue board items on a USER-owned Projects v2 board, which App installation tokens cannot reach")
	// Configurable GitHub / OAuth endpoints (E44.2 / #1826) for GitHub
	// Enterprise Server (Mode 1, self-hosted) and data-resident GitHub
	// Enterprise Cloud <slug>.ghe.com (Mode 2). Empty = unset → keep the
	// github.com / api.github.com defaults, so existing deployments are
	// unchanged. resolveGitHubEndpoints threads these onto the four
	// already-override-capable GitHub clients.
	githubAPIURL := fs.String("github-api-url",
		envOr("FISHHAWKD_GITHUB_API_URL", ""),
		"GitHub REST + App API base URL (e.g. https://ghes.example.com/api/v3); empty → https://api.github.com")
	githubUploadURL := fs.String("github-upload-url",
		envOr("FISHHAWKD_GITHUB_UPLOAD_URL", ""),
		"GitHub release-asset upload host; empty → https://uploads.github.com")
	oauthAuthorizeURL := fs.String("oauth-authorize-url",
		envOr("FISHHAWKD_OAUTH_AUTHORIZE_URL", ""),
		"GitHub OAuth authorize URL; empty → https://github.com/login/oauth/authorize")
	oauthTokenURL := fs.String("oauth-token-url",
		envOr("FISHHAWKD_OAUTH_TOKEN_URL", ""),
		"GitHub OAuth token URL; empty → https://github.com/login/oauth/access_token")
	oauthUserURL := fs.String("oauth-user-url",
		envOr("FISHHAWKD_OAUTH_USER_URL", ""),
		"GitHub OAuth user-profile URL; empty → https://api.github.com/user")
	oauthOrgsURL := fs.String("oauth-orgs-url",
		envOr("FISHHAWKD_OAUTH_ORGS_URL", ""),
		"GitHub OAuth user-orgs URL; empty → https://api.github.com/user/orgs")
	jiraBaseURL := fs.String("jira-base-url",
		envOr("FISHHAWKD_JIRA_BASE_URL", ""),
		"Jira Cloud instance base URL (e.g. https://acme.atlassian.net); with --jira-email + --jira-api-token, enables the jira work-item provider")
	jiraEmail := fs.String("jira-email",
		envOr("FISHHAWKD_JIRA_EMAIL", ""),
		"Jira account email for HTTP Basic auth (secret: never logged); required with --jira-base-url + --jira-api-token to enable the jira provider")
	jiraAPIToken := fs.String("jira-api-token",
		envOr("FISHHAWKD_JIRA_API_TOKEN", ""),
		"Jira API token for HTTP Basic auth (secret: never logged); required with --jira-base-url + --jira-email to enable the jira provider")
	gitlabBaseURL := fs.String("gitlab-base-url",
		envOr("FISHHAWKD_GITLAB_BASE_URL", ""),
		"GitLab instance base URL (e.g. https://gitlab.com or a self-managed host); with --gitlab-token, enables the gitlab work-item provider")
	gitlabToken := fs.String("gitlab-token",
		envOr("FISHHAWKD_GITLAB_TOKEN", ""),
		"GitLab access token for PRIVATE-TOKEN auth (secret: never logged); required with --gitlab-base-url to enable the gitlab provider")
	workmgmtConventions := fs.String("workmgmt-conventions",
		envOr("FISHHAWKD_WORKMGMT_CONVENTIONS", ""),
		"path to a YAML work-management conventions file served for every repo; parsed fail-fast at startup. Empty uses the shipped default. A per-repo in-repo loader is a follow-up (#2022)")
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
	enableDeployReconciler := fs.Bool("enable-deploy-reconciler",
		envOr("FISHHAWKD_ENABLE_DEPLOY_RECONCILER", "false") == "true",
		"start the deploy reconciler (#1386 / E23.6); polls a delegating deploy stage's external GitHub Actions run to a terminal outcome and resolves the stage. Off by default — only useful with a GitHub App wired. See --deploy-reconciler-interval for the rate-limit caveat at scale.")
	deployReconcilerInterval := fs.Duration("deploy-reconciler-interval",
		deployreconciler.DefaultInterval,
		"deploy reconciler scan interval. Each tick makes up to one GitHub GetWorkflowRun call per parked deploy stage with no per-stage cooldown; tune this upward at scale to stay within GitHub REST rate limits (5,000/hour per installation).")
	reviewResolution := fs.String("review-resolution",
		envOr("FISHHAWKD_REVIEW_RESOLUTION", reviewresolver.DefaultResolution),
		"deployment-level review-gate resolution provider (ADR-031 Phase 2; default github_merge). Selects which reviewresolver.Resolver the merge-status reconciler routes through. An unknown value fails startup (fail closed) rather than silently defaulting — succeeded must always mean a verified GitHub merge.")
	enableChildCompletionSweeper := fs.Bool("enable-child-completion-sweeper",
		envOr("FISHHAWKD_ENABLE_CHILD_COMPLETION_SWEEPER", "false") == "true",
		"start the child-completion sweeper (#455 / ADR-025 D4); transitions parent stages parked in awaiting_children once their decomposed children all reach terminal states. Off by default to match the other tickers' dev-loop posture.")
	childCompletionInterval := fs.Duration("child-completion-interval",
		60*time.Second,
		"child-completion sweeper scan interval; 60s is the upper bound on parent latency after the last child terminates")
	enableCampaignDriver := fs.Bool("enable-campaign-driver",
		envOr("FISHHAWKD_ENABLE_CAMPAIGN_DRIVER", "false") == "true",
		"start the campaign-driver ticker (E25.5 / #1444 / ADR-047 Track C); mechanically advances each running campaign — starts runs for dependency-eligible issues and settles items when their runs reach terminal. Off by default to match the other tickers' dev-loop posture.")
	campaignDriverInterval := fs.Duration("campaign-driver-interval",
		campaigndriver.DefaultInterval,
		"campaign-driver scan interval; 60s is the upper bound on campaign-advancement latency after a run terminates")
	campaignDriverWorkflowID := fs.String("campaign-driver-workflow-id",
		envOr("FISHHAWKD_CAMPAIGN_DRIVER_WORKFLOW_ID", "feature_change"),
		"workflow id the campaign driver starts for each eligible campaign issue. A campaign carries no workflow context (E25.2), so the driver fetches this workflow from the repo's spec at --campaign-driver-workflow-ref.")
	campaignDriverWorkflowRef := fs.String("campaign-driver-workflow-ref",
		envOr("FISHHAWKD_CAMPAIGN_DRIVER_WORKFLOW_REF", ""),
		"git ref the campaign driver fetches the workflow spec at (empty = the repo's default branch). The fetched blob SHA becomes each started run's workflow_sha.")
	enableInvariantMonitor := fs.Bool("enable-invariant-monitor",
		envOr("FISHHAWKD_ENABLE_INVARIANT_MONITOR", "false") == "true",
		"start the self-consistency invariant monitor (#764); periodically auto-reconciles the safe {all stages terminal, run non-terminal} class and surfaces (audit + WARN log) the unrecoverable {review awaiting_approval, null pull_request_url on a push-and-open-pr run} class. Off by default to match the other tickers' dev-loop posture.")
	invariantMonitorInterval := fs.Duration("invariant-monitor-interval",
		60*time.Second,
		"invariant monitor scan interval")
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
	operatorRepo := fs.String("oauth-operator-repo",
		envOr("FISHHAWKD_OPERATOR_REPO", ""),
		"owner/name repository the OAuth token-mint gate (POST /v0/tokens/login, E39.3 / #1708) reads a "+
			"verified subject's permission tier on; a subject must hold at least --operator-min-permission "+
			"to mint a token. Empty leaves the mint endpoint at 503 tokens_unconfigured (fail closed)")
	operatorMinPermission := fs.String("operator-min-permission",
		envOr("FISHHAWKD_OPERATOR_MIN_PERMISSION", "write"),
		"minimum repository permission (none|read|triage|write|maintain|admin) a verified subject must hold "+
			"on --oauth-operator-repo to mint an OAuth token (E39.3 / #1708); default write. An unrecognized "+
			"value fails startup (fail closed) rather than silently under-gating")
	homeRegion := fs.String("home-region",
		envOr("FISHHAWKD_HOME_REGION", ""),
		"the region THIS cell serves (ADR-062, E44.7 / #1831), e.g. us or eu. Empty disables the "+
			"region-pin surface entirely: a routed request carrying a directory handoff is then REFUSED "+
			"(503), never served as though the handoff were absent. Requests carrying no fh_* parameters "+
			"are unaffected, so a single-cell deployment behaves identically with or without this flag")
	handoffSecret := fs.String("handoff-secret",
		envOr("FISHHAWKD_HANDOFF_SECRET", ""),
		"HMAC-SHA256 key this cell shares with the directory plane to verify signed handoffs "+
			"(ADR-062, E44.7 / #1831). Empty disables the region-pin surface on the same fail-closed "+
			"terms as an empty --home-region; BOTH must be set for the surface to be constructed")
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
	enableCodexReviewer := fs.Bool("enable-codex-reviewer",
		envOr("FISHHAWKD_ENABLE_CODEX_REVIEWER", "false") == "true",
		"opt-in Codex plan review: spawn the `codex` CLI as a subprocess for advisory review. Lower precedence than --anthropic-api-key and --enable-local-claude-reviewer. Off by default")
	codexBinary := fs.String("codex-reviewer-binary",
		envOr("FISHHAWKD_CODEX_BINARY", "codex"),
		"executable name or path for the Codex reviewer CLI; used only when --enable-codex-reviewer is set")
	codexModel := fs.String("codex-reviewer-model",
		envOr("FISHHAWKD_CODEX_MODEL", ""),
		"model the Codex reviewer runs, passed to `codex exec --model`, and recorded for its invocations (the self-review guard compares it to the plan's GeneratedBy.Model); empty inherits the host ~/.codex config; used only when --enable-codex-reviewer is set")
	codexEffort := fs.String("codex-reviewer-effort",
		envOr("FISHHAWKD_CODEX_REASONING_EFFORT", ""),
		"reasoning effort passed to the Codex reviewer as a model_reasoning_effort config override, e.g. low/medium/high; empty inherits the host ~/.codex config; used only when --enable-codex-reviewer is set")
	openAIAPIKey := fs.String("openai-api-key",
		envOr("FISHHAWKD_OPENAI_API_KEY", ""),
		"OpenAI API key forwarded as OPENAI_API_KEY to the Codex reviewer subprocess; empty is fine when Codex is authenticated via a ChatGPT login on the host")
	modelBaseURL := fs.String("model-base-url",
		envOr("FISHHAWKD_MODEL_BASE_URL", ""),
		"region-scoped inference endpoint for the Anthropic SDK reviewer (ADR-062, E44.7 / #1831). "+
			"Empty uses the SDK default (api.anthropic.com), which is the single-cell posture. "+
			"Selection is process-level: every plan- and implement-review call this cell makes targets this endpoint")
	modelAPIKey := fs.String("model-api-key",
		envOr("FISHHAWKD_MODEL_API_KEY", ""),
		"API key presented to --model-base-url. Empty falls back to --anthropic-api-key ONLY when "+
			"--model-base-url is also empty (the single-cell posture, which needs neither flag); with a "+
			"custom endpoint configured and this key unset the adapter presents no credential rather than "+
			"sending the deployment key to that endpoint. It does NOT enable the anthropic reviewer on its own — "+
			"--anthropic-api-key still selects the adapter; this key only overrides the credential sent")
	planReviewMaxTokens := fs.Int("plan-review-max-tokens",
		envOrInt("FISHHAWKD_PLAN_REVIEW_MAX_TOKENS", 4096),
		"maximum tokens for plan-review agent responses")
	planReviewMaxRetries := fs.Int("plan-review-max-retries",
		envOrInt("FISHHAWKD_PLAN_REVIEW_MAX_RETRIES", 1),
		"retry budget for the reviewers' transient-crash (#620) and structurally-malformed-verdict decode (#901) classes; "+
			"counts retries not attempts (N => N+1 attempts), 0 disables retry (single attempt), unset defaults to 1. "+
			"Honoured by all three reviewer adapters (claudecode, codex, anthropic): the subprocess adapters apply it to "+
			"both the crash-retry and the decode re-roll; the anthropic SDK adapter applies it to the decode re-roll via SetMaxRetries")
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
	maxParallelChildren := fs.Int("max-parallel-children",
		envOrInt("FISHHAWKD_MAX_PARALLEL_CHILDREN", 0),
		"global default cap on how many decomposed child runs may dispatch concurrently for a "+
			"single run (E24.6 / #1146). The per-workflow decomposition.max_parallel knob overrides "+
			"it when set. 0 (the default) = unlimited. This resolves and surfaces the cap; concurrency "+
			"enforcement that consumes it lands in E24.3 (#1143)")
	implementModelDefault := fs.String("implement-model-default",
		envOr("FISHHAWKD_IMPLEMENT_MODEL_DEFAULT", ""),
		"deployment default implement model — the lowest rung of the implement-model resolution "+
			"ladder (#1013). Empty (the default) means no deployment default: with no spec executor.model, "+
			"no plan model_recommendation, and no operator override, the resolved model is empty and the "+
			"runner spawns the implement agent on the adapter's built-in default exactly as today")
	implementAllowedModels := fs.String("implement-allowed-models",
		envOr("FISHHAWKD_IMPLEMENT_ALLOWED_MODELS", ""),
		"per-adapter allowed-model policy the approval gate validates the RESOLVED implement model "+
			"against (#1013). Format: `adapter=model1,model2;adapter2=model3`, e.g. "+
			"`claudecode=claude-opus-4-8,claude-sonnet-4-6;codex=gpt-5.5`. Empty (the default) or an "+
			"adapter with no configured set fails OPEN — any model accepted, byte-identical to today")
	planAllowedModels := fs.String("plan-allowed-models",
		envOr("FISHHAWKD_PLAN_ALLOWED_MODELS", ""),
		"per-adapter allowed-model policy the approval gate validates the RESOLVED plan model "+
			"against (#1416), keyed by the plan stage's executor.agent adapter. Same format as "+
			"--implement-allowed-models. Empty (the default) or an adapter with no configured set "+
			"fails OPEN — any model accepted, byte-identical to today")
	reviewAllowedModels := fs.String("review-allowed-models",
		envOr("FISHHAWKD_REVIEW_ALLOWED_MODELS", ""),
		"per-adapter allowed-model policy the approval gate validates the RESOLVED review model "+
			"against (#1416), keyed by each implement-review reviewer provider. Same format as "+
			"--implement-allowed-models. Empty (the default) or a provider with no configured set "+
			"fails OPEN — any model accepted, byte-identical to today")
	budgetTimezone := fs.String("budget-timezone",
		envOr("FISHHAWKD_BUDGET_TIMEZONE", "UTC"),
		"IANA timezone (e.g. America/New_York) the advisory periodic-budget evaluator (#688) "+
			"computes calendar period boundaries in — a weekly budget resets Monday 00:00 in this "+
			"zone, a monthly budget on the 1st. An unresolvable zone name falls back to UTC with a "+
			"WARN at startup rather than failing the boot")
	budgetLimitOverrideUSD := fs.Float64("budget-limit-override-usd",
		envOrFloat("FISHHAWKD_BUDGET_LIMIT_OVERRIDE_USD", 0),
		"calibrate the advisory periodic-budget limit (#1371) WITHOUT editing .fishhawk/workflows.yaml "+
			"(a forbidden commit path for the implement stage). When > 0 it replaces the spec budget's "+
			"limit_usd in both the GET /v0/runs/{id}/budget display and the budget_alert path, so a normal "+
			"calibrated week reads a believable fraction. The default 0 means use the spec limit. GLOBAL "+
			"across the run's advisory budgets (the dogfood spec declares exactly one)")
	budgetAckMultiple := fs.Float64("budget-ack-multiple",
		envOrFloat("FISHHAWKD_BUDGET_ACK_MULTIPLE", 2.0),
		"multiple of the (possibly overridden) periodic-budget limit at which the escalating tier ladder "+
			"(#1371) trips ack_required — the rung where a plan approval needs an explicit --ack-budget "+
			"acknowledgment. Default 2.0. A non-positive value falls back to the 2x built-in default")
	budgetPageMultiple := fs.Float64("budget-page-multiple",
		envOrFloat("FISHHAWKD_BUDGET_PAGE_MULTIPLE", 3.0),
		"multiple of the (possibly overridden) periodic-budget limit at which the escalating tier ladder "+
			"(#1371) trips the highest 'page' rung. Default 3.0. A non-positive value, or a value <= the ack "+
			"multiple, falls back to the 3x built-in default so a misconfigured pair never collapses every "+
			"fraction into 'page'")
	modelsRefreshInterval := fs.Duration("models-refresh-interval",
		envOrDuration("FISHHAWKD_MODELS_REFRESH_INTERVAL", 12*time.Hour),
		"how often the live ModelOracle (#1341) re-fetches each provider's /v1/models snapshot "+
			"that backs #1339's model-id validation. The background refresh is best-effort: a failed "+
			"fetch keeps the prior snapshot and decays its freshness, never blocking submits")
	modelsStalenessThreshold := fs.Duration("models-staleness-threshold",
		envOrDuration("FISHHAWKD_MODELS_STALENESS_THRESHOLD", 24*time.Hour),
		"how old a provider's last SUCCESSFUL /v1/models fetch may be before the ModelOracle "+
			"snapshot is treated as stale (#1341). A stale snapshot fails open (model_unverifiable "+
			"warning, accept) rather than rejecting — absence from a stale list cannot authoritatively reject")
	if err := fs.Parse(args); err != nil {
		return exitFailure
	}

	logger := newLogger(logSink)

	// OAuth token-mint authz minimum (E39.3 / #1708). Fail closed on an
	// unrecognized tier rather than silently defaulting — a typo must not
	// under-gate the mint endpoint.
	operatorMinPerm, permOK := identity.ParsePermission(*operatorMinPermission)
	if !permOK {
		logger.Error("invalid --operator-min-permission",
			slog.String("got", *operatorMinPermission),
			slog.String("expected", "none|read|triage|write|maintain|admin"))
		return exitFailure
	}

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

	// Live ModelOracle (#1341): the per-provider /v1/models cache that activates
	// #1339's model-id validation. Providers are registered only for present API
	// keys (absent key => unregistered => Snapshot ok=false => fail-open), keyed
	// under the existing "claudecode"/"codex" strings. The background refresh
	// goroutine is started after the signal context is created, below.
	modelProviders := buildModelProviders(*anthropicAPIKey, *openAIAPIKey)
	modelOracle := modeloracle.NewCached(modelProviders, *modelsStalenessThreshold, logger)

	cfg := server.Config{Addr: *addr, StartNonce: *startNonce, Logger: logger, ExternalURL: *externalURL, SpendAlertMultiple: *spendAlertMultiple, BudgetLocation: budgetLocation, BudgetLimitOverrideUSD: *budgetLimitOverrideUSD, BudgetAckMultiple: *budgetAckMultiple, BudgetPageMultiple: *budgetPageMultiple, ReviewBudget: reviewBudget, MaxParallelChildren: *maxParallelChildren, ImplementModelDefault: *implementModelDefault, ImplementAllowedModels: server.ParseAllowedModels(*implementAllowedModels), PlanAllowedModels: server.ParseAllowedModels(*planAllowedModels), ReviewAllowedModels: server.ParseAllowedModels(*reviewAllowedModels), ReviewResolution: *reviewResolution, ModelOracle: modelOracle}

	// OAuth token-login wiring (E39.3 / #1708). The client_id the discovery
	// endpoint advertises is the SAME --oauth-client-id the browser sign-in
	// flow uses; OperatorRepo + OperatorMinPermission gate the mint; the
	// OperatorDefaultScopes ceiling reuses the exact set `fishhawkd token
	// issue` applies so both mint paths cap scopes identically. The
	// IdentityProvider itself is config-gated below (resolveIdentityProvider).
	cfg.OAuthClientID = *oauthClientID
	cfg.OperatorRepo = *operatorRepo
	cfg.OperatorMinPermission = operatorMinPerm
	cfg.OperatorDefaultScopes = operatorDefaultScopes

	// Plan-review agent wiring. Resolved by a pure helper so the selection seam
	// (which adapters the flags configure) is unit-testable without booting a
	// server.
	cfg.PlanReviewers = resolvePlanReviewers(planReviewerOptions{
		anthropicAPIKey:           *anthropicAPIKey,
		planReviewModel:           *planReviewModel,
		enableLocalClaudeReviewer: *enableLocalClaudeReviewer,
		localClaudeBinary:         *localClaudeBinary,
		localClaudeModel:          *localClaudeModel,
		enableCodexReviewer:       *enableCodexReviewer,
		codexBinary:               *codexBinary,
		codexModel:                *codexModel,
		codexEffort:               *codexEffort,
		openAIAPIKey:              *openAIAPIKey,
		planReviewMaxTokens:       *planReviewMaxTokens,
		planReviewMaxRetries:      *planReviewMaxRetries,
		planReviewTimeout:         *planReviewTimeout,
		modelBaseURL:              *modelBaseURL,
		modelAPIKey:               *modelAPIKey,
	}, logger)

	// Refinement drafting agent (E34.2 / #1593). Reuses the local-claude
	// reviewer options: when the local claude adapter is configured, the E34.1
	// Drafter runs over the same claudecode client + the default work-management
	// conventions. Nil when unconfigured — the agent-backed gate arms
	// (create-session, brief-amendment) then degrade to 503 while GET / direct
	// edit / decision keep working. The Postgres RefinementRepo is wired in the
	// DB block below (always-on, mirroring CampaignRepo).
	cfg.RefinementDrafter = resolveRefinementDrafter(planReviewerOptions{
		enableLocalClaudeReviewer: *enableLocalClaudeReviewer,
		localClaudeBinary:         *localClaudeBinary,
		localClaudeModel:          *localClaudeModel,
	}, logger)

	// Wire the run repository when a DB URL is supplied. Without
	// one the server still boots — /healthz works and any
	// repository-dependent handler returns 503 — so operators can
	// smoke-test a deploy before pointing it at production data.
	var pool *pgxpool.Pool
	// accountQueries stays a literal nil interface without a pool (never a
	// typed-nil *accountdb.Queries, which would read as "configured" to
	// resolveRegionPin's q == nil check).
	var accountQueries account.RegionPinnerQueries
	if *dbURL != "" {
		var err error
		pool, err = pgxpool.New(context.Background(), *dbURL)
		if err != nil {
			logger.Error("db pool create failed", slog.String("error", err.Error()))
			return exitFailure
		}
		defer pool.Close()
		cfg.RunRepo = runpkg.NewPostgresRepository(pool)
		cfg.CampaignRepo = campaign.NewPostgresRepository(pool)
		cfg.SigningRepo = signing.NewPostgresRepository(pool)
		cfg.AuditRepo = audit.NewPostgresRepository(pool)
		cfg.ApprovalRepo = approval.NewPostgresRepository(pool)
		cfg.ArtifactRepo = artifact.NewPostgresRepository(pool)
		cfg.StageCheckRepo = stagecheck.NewPostgresRepository(pool)
		cfg.APITokenRepo = apitoken.NewPostgresRepository(pool)
		cfg.MCPTokenRepo = mcptoken.NewPostgresRepository(pool)
		cfg.ScopeAmendmentRepo = scopeamendment.NewPostgresRepository(pool)
		cfg.ConcernRepo = concern.NewPostgresRepository(pool)
		cfg.RefinementRepo = resolveRefinementRepo(pool)
		cfg.AuthRepo = authpkg.NewPostgresRepository(pool)
		// Account-role reader for the handler-authz write tiers (ADR-057 /
		// E44.5, #1829). Nil pool leaves cfg.AccountRoles nil (untenanted-allow
		// — role-bounding skipped), mirroring AuthMembership's nil-pool posture.
		cfg.AccountRoles = account.NewStore(accountdb.New(pool))
		accountQueries = accountdb.New(pool)
		logger.Info("repositories configured (run + signing + audit + approval + artifact + stagecheck + apitoken + auth + account-roles)", slog.String("driver", "postgres"))
	} else {
		logger.Warn("FISHHAWKD_DATABASE_URL not set; /v0/runs and /v0/runs/{id}/signing-key endpoints will respond 503")
	}

	// Regional handoff surface (ADR-062, E44.7 / #1831). Constructed only when
	// the cell's own region, the shared secret, AND a database are all present;
	// anything less leaves the pair zero, which is the fail-closed posture.
	cfg.HandoffSecret, cfg.RegionPinner = resolveRegionPin(regionPinOptions{
		homeRegion:    *homeRegion,
		handoffSecret: *handoffSecret,
	}, accountQueries, logger)

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
	} else {
		logger.Warn("FISHHAWKD_GITHUB_WEBHOOK_SECRET not set; /webhooks/github will respond 503")
	}
	// GitLab is optional and deliberately asymmetric with GitHub: log
	// when configured, but do NOT warn when absent — an absent-warn
	// would nag every GitHub-only deployment.
	if *gitlabWebhookSecret != "" {
		cfg.GitLabWebhookSecret = []byte(*gitlabWebhookSecret)
		logger.Info("gitlab webhook receiver configured")
	}
	// The delivery store is shared by both receivers, so create it when
	// EITHER secret is set (previously gated on the GitHub secret alone).
	// newWebhookDeliveryStore owns the gating + store selection so this
	// call site can't drift back to a GitHub-only gate.
	if store, evictor := newWebhookDeliveryStore(pool, *webhookSecret, *gitlabWebhookSecret, webhookRetention); store != nil {
		cfg.WebhookDeliveries = store
		webhookEvictor = evictor
		if evictor != nil {
			logger.Info("webhook receiver configured (postgres dedup)")
		} else {
			logger.Warn("webhook receiver using memory dedup — NOT safe for multi-instance deploys; set FISHHAWKD_DATABASE_URL")
		}
	}

	// Configurable GitHub / OAuth endpoints (E44.2 / #1826). Resolved once
	// here and threaded onto the four already-override-capable GitHub clients
	// below (App, REST, OAuth web flow, identity device flow). Empty fields
	// leave each client on its github.com / api.github.com default.
	githubEndpoints := resolveGitHubEndpoints(
		*githubAPIURL, *githubUploadURL,
		*oauthAuthorizeURL, *oauthTokenURL, *oauthUserURL, *oauthOrgsURL)

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
		appClient := githubapp.NewClient(signer)
		// Mode 1 (per-deployment, E44.2 / #1826): a configured API base URL
		// (GHES) overrides api.github.com for installation-token minting; empty
		// keeps the default.
		appClient.BaseURL = githubEndpoints.APIBaseURL
		// Mode 2 (per-installation, E44.2 / #1826): resolve the data-resident
		// API host from installations.forge_base_url. Late-bound AFTER the DB
		// pool exists — a nil pool leaves ResolveBaseURL nil (every install
		// keeps the deployment default). A real resolution fault FAILS the mint
		// (fail-closed, see githubapp.Client.ResolveBaseURL); a NULL column /
		// unknown installation falls back to the deployment default.
		if pool != nil {
			endpointResolver := account.NewEndpointResolver(accountdb.New(pool))
			// githubapp hands us the stringified installation id (its
			// installation_ref); we pass it straight to the forge-neutral
			// resolver. Only forge_base_url feeds the App API host; oauth_base_url
			// is a per-installation OAuth follow-up (no consumer here yet).
			appClient.ResolveBaseURL = func(ctx context.Context, installationRef string) (string, error) {
				forgeBase, _, err := endpointResolver.ResolveInstallationEndpoints(
					ctx, "github", installationRef)
				return forgeBase, err
			}
		}
		cfg.GitHubTokens = githubapp.NewCachedProvider(appClient)
		// This REST client backs every backend → GitHub read/write, including
		// the code_scanning_alert webhook ingest (#1096), which reuses it via
		// ListCodeScanningAlerts to surface CodeQL/SAST findings to the
		// implement-review gate. Wired here once; the server consumes it as
		// cfg.GitHub — no separate code-scanning client is constructed.
		cfg.GitHub = githubclient.NewWithSigner(cfg.GitHubTokens, signer)
		// Mode 1 REST + upload host overrides (E44.2 / #1826); empty → defaults.
		cfg.GitHub.BaseURL = githubEndpoints.APIBaseURL
		cfg.GitHub.UploadBaseURL = githubEndpoints.UploadBaseURL
		// Optional user-scoped projects token. Presence-only log: the token
		// is a secret and must never be logged or traced (#1114). Absent, a
		// user-owned project board stays best-effort boarded:false (#1107).
		cfg.GitHub.ProjectsToken = *projectsToken
		// Bind the SAME GitHub auto-merge seam the campaign GateActor uses into
		// the server so the local auto-driver endpoint (POST
		// /v0/runs/{run_id}/auto-drive, #1700) can dispatch a delegated
		// may_merge. Guarded by this cfg.GitHub != nil block exactly like the
		// campaign wiring — a nil GateMerger keeps may_merge fail-CLOSED to
		// observe-only, byte-identical to today.
		cfg.GateMerger = githubAutoMerger{gh: cfg.GitHub}
		logger.Info("github app + REST client configured",
			slog.Int64("app_id", appID),
			slog.Bool("projects_token_configured", *projectsToken != ""))
		if *projectsToken == "" {
			logger.Info("FISHHAWKD_PROJECTS_TOKEN not set; user-owned Projects v2 board placement stays best-effort boarded:false (#1107)")
		}
	} else {
		logger.Warn("FISHHAWKD_GITHUB_APP_ID not set; webhook dispatch and GitHub-side actions will be disabled")
	}

	// campaignNotifier is the issue-comment notifier the campaign driver fires
	// the human page through (E25.7). Hoisted to runServe scope so the
	// campaign-driver wiring below reuses the SAME notifier the webhook
	// dispatcher builds; nil when the GitHub/Run/Audit deps are unconfigured,
	// which makes the driver's Paged branch observe-only (pause recorded, no
	// page).
	var campaignNotifier *issuecomment.Notifier

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
			GitHub:    cfg.GitHub,
			Runs:      cfg.RunRepo,
			Audit:     cfg.AuditRepo,
			Artifacts: cfg.ArtifactRepo,
			Logger:    logger,
			// Route the webhook surfaces through the Channel abstraction
			// too (ADR-015 #79): the Router satisfies the dispatcher's
			// narrower webhook.IssueNotifier subset and fans out to the v0
			// GitHub-comment channel. NewRouter over a nil *Notifier still
			// degrades to a no-op via the Router's nil-channel skipping, so
			// the empty-ExternalURL posture is unchanged.
			IssueNotifier: issuecomment.NewRouter(notifier),
			// Scaffolder opens the App-PR onboarding scaffold when the App
			// is installed on a repo or repos are added (ADR-048 / E29.7).
			// Drives the Git Data API commit + PR through cfg.GitHub.
			Scaffolder: onboarding.NewScaffolder(cfg.GitHub),
			// (campaignNotifier reuses this same notifier for the campaign
			// driver's page seam — assigned just below.)
			// PlanReviewerConfigured mirrors the run-create guard's
			// default-reviewer check (#574) so the webhook-dispatcher
			// path refuses an agent-gated plan stage with no reviewer
			// wired (#577 / ADR-027). cfg.PlanReviewers is resolved
			// earlier from the anthropic/claudecode/codex adapter
			// options; Default()==nil means no backend is configured.
			PlanReviewerConfigured: cfg.PlanReviewers != nil && cfg.PlanReviewers.Default() != nil,
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
		// Reuse this notifier as the campaign driver's page seam (E25.7). It is
		// the *Notifier directly (not the Channel router) so it satisfies the
		// driver's NotifyStatusUpdateForRun seam.
		campaignNotifier = notifier
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
		cfg.Orchestrator = newStageOrchestrator(cfg, logger)
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

	// Jira work-item provider (#1094). The instance URL + credentials are
	// server-side env (secrets cannot live in a checked-in repo config); the
	// per-repo conventions block selects only the project. All three must be
	// set to enable the provider — a partial config is a misconfiguration,
	// warned and left disabled (the endpoint stays 501 for provider: jira).
	// The email + token are secrets: presence-only logging, never the values
	// (the base URL is an instance address, not a secret).
	var jiraClient *jiraclient.Client
	switch {
	case *jiraBaseURL != "" && *jiraEmail != "" && *jiraAPIToken != "":
		jiraClient = jiraclient.New(*jiraBaseURL, *jiraEmail, *jiraAPIToken)
		logger.Info("jira client configured",
			slog.String("base_url", *jiraBaseURL),
			slog.Bool("credentials_configured", true))
	case *jiraBaseURL != "" || *jiraEmail != "" || *jiraAPIToken != "":
		logger.Warn("jira partially configured; all of FISHHAWKD_JIRA_BASE_URL, FISHHAWKD_JIRA_EMAIL, FISHHAWKD_JIRA_API_TOKEN are required to enable the jira provider — leaving it disabled (provider: jira responds 501)")
	}

	// GitLab work-item provider (ADR-058 Phase 2, #1856). The instance base
	// URL + token are server-side env (secrets cannot live in a checked-in
	// repo config); the per-repo conventions block selects only an optional
	// project override. Both must be set to enable the provider — a partial
	// config is a misconfiguration, warned and left disabled (the endpoint
	// stays 501 for provider: gitlab). The token is a secret: presence-only
	// logging, never the value (the base URL is an instance address, not a
	// secret). A configurable base URL covers GitLab.com SaaS and
	// self-managed instances alike.
	gitlabClient, gitlabPartial := resolveGitLabClient(*gitlabBaseURL, *gitlabToken)
	switch {
	case gitlabClient != nil:
		logger.Info("gitlab client configured",
			slog.String("base_url", *gitlabBaseURL),
			slog.Bool("token_configured", true))
	case gitlabPartial:
		logger.Warn("gitlab partially configured; both of FISHHAWKD_GITLAB_BASE_URL and FISHHAWKD_GITLAB_TOKEN are required to enable the gitlab provider — leaving it disabled (provider: gitlab responds 501)")
	}

	// Deployment-level conventions override (ADR-058 Phase 2, #1856),
	// retained as the BREAK-GLASS fallback inside the per-repo conventions
	// loader (#2022): still parsed fail-fast at startup, but its result now
	// feeds the loader's fallback chain (installed after server.New below)
	// instead of being installed as THE loader.
	conventionsOverride, cerr := loadConventionsOverride(*workmgmtConventions)
	if cerr != nil {
		logger.Error("invalid work-management conventions file",
			slog.String("path", *workmgmtConventions), slog.String("error", cerr.Error()))
		return exitFailure
	}
	if conventionsOverride != nil {
		logger.Info("work-management conventions break-glass override loaded",
			slog.String("path", *workmgmtConventions))
	}

	// Work-management providers (#1104, #1094, #1856). Register the
	// github_projects work-item + product-feedback providers, the jira
	// work-item provider, and the gitlab work-item provider so
	// fishhawk_file_issue and fishhawk_report_product_issue resolve a provider
	// instead of 501. Each provider is gated on its own client being
	// configured — an unconfigured client leaves that provider unregistered
	// and the endpoint continues to 501 (the v0 not-yet-wired posture).
	registerWorkmgmtProviders(cfg.GitHub, jiraClient, gitlabClient)
	if len(workmgmt.Registered()) > 0 || len(workmgmt.RegisteredFeedback()) > 0 {
		logger.Info("work-management providers registered",
			slog.Any("work_item", workmgmt.Registered()),
			slog.Any("feedback", workmgmt.RegisteredFeedback()))
	} else {
		logger.Warn("work-management providers not registered: fishhawk_file_issue / fishhawk_report_product_issue respond 501 until GitHub, Jira, or GitLab is configured")
	}

	// Forge providers (ADR-058 / E45.4, E45.5). Register the github adapter
	// over the same concrete cfg.GitHub client so a forge-neutral consumer
	// can resolve "github" via forge.Get instead of holding a
	// *githubclient.Client. Gated on cfg.GitHub like the github work-item
	// provider above: an unconfigured client leaves the forge registry empty
	// rather than registering a nil-backed adapter. The gitlab adapter (#1859)
	// registers alongside it when BOTH --gitlab-base-url and --gitlab-token
	// are set (the same both-or-neither guard the work-item provider uses),
	// wired with a static-token credential provider carrying the configured
	// group/project access token. A single startup log names every registered
	// forge.
	if cfg.GitHub != nil {
		forge.Register(forgegithub.New(cfg.GitHub))
	}
	if glForge := resolveGitLabForge(*gitlabBaseURL, *gitlabToken); glForge != nil {
		forge.Register(glForge)
	}
	if len(forge.Registered()) > 0 {
		logger.Info("forge providers registered", slog.Any("forges", forge.Registered()))
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
		// Mode 1 OAuth endpoint overrides (E44.2 / #1826): a GHES/EMU
		// deployment threads its authorize/token/user/orgs URLs; empty fields
		// keep the github.com OAuth endpoints (NewGitHubOAuth fills defaults).
		cfg.GitHubOAuth = authpkg.NewGitHubOAuth(
			*oauthClientID, *oauthClientSecret, *oauthCallbackURL, githubEndpoints.OAuth)
		cfg.AuthRedirectAfterLogin = *oauthRedirectAfterLogin
		// Forge-neutral identity provider (E39.1 / #1706). Gated on the
		// same OAuth client config: with a client_id present we construct
		// the GitHub device-flow + REST implementation; leaving the field
		// nil on the else branch lets server.New fall back to the
		// deny-by-default NoOp. The identity provider's permission/membership
		// reads authenticate with the operator-repo installation token
		// (E39.10 / #1753): the mint-authz collaborator-permission GET only
		// answers an authenticated caller with push access, so an anonymous
		// read 401s and `fishhawk token login` 500s. resolveOperatorRepoToken
		// degrades to nil (anonymous reads) when App auth is absent.
		operatorRepoToken := resolveOperatorRepoToken(cfg.GitHub, cfg.GitHubTokens, cfg.OperatorRepo)
		if operatorRepoToken == nil {
			logger.Warn("identity-provider REST reads stay anonymous: token-mint authz gate (fishhawk token login) will fail HTTP 500 until FISHHAWKD_GITHUB_APP_ID + key are configured")
		}
		cfg.IdentityProvider = resolveIdentityProvider(*oauthClientID, operatorRepoToken,
			githubEndpoints.IdentityAPIURL, githubEndpoints.IdentityOAuthURL)
		// Workspace-membership login gate (E44.3 / ADR-057 Amendment
		// A2): invited account_members rows admit DB-only; the
		// GitHubOAuth client doubles as the ForgeMembershipLister for
		// the auto-join bootstrap. Without a database the resolver
		// stays nil and the callback denies every sign-in (fail
		// closed).
		if pool != nil {
			cfg.AuthMembership = authpkg.NewMembershipResolver(
				authpkg.NewAccountMembershipStore(accountdb.New(pool)), cfg.GitHubOAuth)
		} else {
			logger.Warn("membership resolver unconfigured (no database); every OAuth sign-in will be denied")
		}
		logger.Info("github oauth sign-in configured",
			slog.String("callback_url", *oauthCallbackURL),
			slog.String("redirect_after_login", *oauthRedirectAfterLogin),
			slog.Bool("github_identity_provider", cfg.IdentityProvider != nil),
			slog.Bool("rest_read_token", operatorRepoToken != nil))
	} else {
		logger.Warn("FISHHAWKD_OAUTH_CLIENT_ID not set; /v0/auth/github/login + /callback respond 503; identity provider defaults to NoOp (deny-by-default)")
	}

	// GitHub App manifest-flow client (E4.7). No credentials needed —
	// the conversions endpoint accepts the one-shot `code` and
	// returns App credentials in one shot. Always wired so operators
	// can self-register an App from a fresh install.
	cfg.GitHubManifest = authpkg.NewGitHubManifest(authpkg.ManifestURLs{})

	srv := server.New(cfg)

	// Per-repo work-management conventions loader (E45.16 / #2022): fetch
	// .fishhawk/work-management.yaml from the filing repo's OWN forge,
	// resolved via the ADR-057/ADR-058 accounts.provider discriminator, with
	// the break-glass override parsed above as the fallback. Wired after
	// server.New (the GitHub scope resolution lives on the Server) and after
	// forge.Register above (the fetchers come from the registry). Without a
	// database the discriminator resolver stays nil and every filing falls
	// through to override/Default — the pre-#2022 posture.
	var accountResolver server.ProviderResolver
	if pool != nil {
		accountResolver = account.NewResolver(accountdb.New(pool))
	}
	server.SetConventionsLoader(buildRepoConventionsLoader(srv, accountResolver, conventionsOverride).Load)
	logger.Info("per-repo work-management conventions loader installed",
		slog.Bool("discriminator_resolver", accountResolver != nil),
		slog.Bool("break_glass_override", conventionsOverride != nil))

	// Wire the slash-command approval handler now that the Server
	// exists (#238). The dispatcher was constructed earlier without
	// this field; we plug it in here so the dispatcher's nil-check
	// stays honest when slash-command-approval deps aren't ready.
	if cfg.WebhookDispatcher != nil {
		cfg.WebhookDispatcher.ApprovalHandler = srv
		// Board-state sync (#1012): the Server implements BoardSyncer and
		// holds the conventions loader, provider registry, run + audit repos
		// the run_started board move needs. Plugged in here for the same
		// reason as ApprovalHandler — the dispatcher was built before the
		// Server existed.
		cfg.WebhookDispatcher.BoardSyncer = srv
		logger.Info("slash-command approval handler wired")
	}

	// Review-gate resolution provider seam (ADR-031 Phase 2). Register the
	// github_merge logic as the first provider — a thin Func adapter over
	// srv.ResolveReviewFromPollState, so the reviewresolver package needs no
	// import of server (no cycle). Select the configured provider by id; an
	// unknown review.resolution value fails startup (fail closed) rather than
	// silently defaulting to github_merge and masking a deployment error —
	// succeeded must always mean a verified GitHub merge. The default
	// (github_merge) wraps srv.ResolveReviewFromPollState, so the merge-status
	// reconciler's resolution path is byte-for-byte unchanged.
	reviewresolver.Register(reviewresolver.Func(reviewresolver.DefaultResolution, srv.ResolveReviewFromPollState))
	reviewResolver, err := reviewresolver.Select(cfg.ReviewResolution)
	if err != nil {
		logger.Error("review-resolution provider invalid",
			slog.String("requested", cfg.ReviewResolution),
			slog.Any("registered", reviewresolver.Registered()),
			slog.String("error", err.Error()))
		return exitFailure
	}
	logger.Info("review-gate resolution provider selected",
		slog.String("provider", reviewResolver.Name()))

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// Start the live ModelOracle refresh goroutine (#1341). Fires an initial
	// best-effort fetch then refreshes on the ticker; stops on ctx cancellation
	// like the other background workers. A failed fetch keeps the prior snapshot
	// and fails open, so this never blocks boot. With no providers registered
	// (no API keys) the loop still runs but refreshes nothing — every Snapshot
	// stays ok=false (inert, exactly as the prior NoData default).
	go modelOracle.Run(ctx, *modelsRefreshInterval)
	logger.Info("model oracle refresh started",
		slog.Any("providers", modelProviderNames(modelProviders)),
		slog.Duration("refresh_interval", *modelsRefreshInterval),
		slog.Duration("staleness_threshold", *modelsStalenessThreshold))

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
				// Resolver is the config-selected review-resolution provider
				// (ADR-031 Phase 2). For the github_merge default it wraps
				// srv.ResolveReviewFromPollState, so resolution is byte-for-byte
				// unchanged. NOTE: the Func adapter is not *server.Server, so
				// Tick's Resolver-upgrade type assertions for the optional
				// DriveObserver / BoardTransitionHealer no longer fire off
				// Resolver — wire those (and LineageReverifier, as before)
				// explicitly so the merge-reconciler behavior is preserved.
				Resolver:              reviewResolver,
				LineageReverifier:     srv,
				AuditCheckRepublisher: srv,
				DriveObserver:         srv,
				BoardTransitionHealer: srv,
				Logger:                logger,
				Interval:              *mergeReconcilerInterval,
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

	// Deploy reconciler (#1386 / E23.6). Polls a delegating deploy stage's
	// external GitHub Actions run to a terminal outcome and resolves the
	// stage through srv.ResolveDeploymentFromPollState — the deploy-side
	// analogue of the merge-status reconciler. Off by default; on requires
	// RunRepo + AuditRepo + a GitHub client + the server (Resolver). Same
	// fall-through posture as the other tickers.
	if *enableDeployReconciler {
		// The deploy reconciler needs the narrow DeployStageSource capability
		// (awaiting-deployment listing), which the concrete repo carries but
		// the broad run.Repository interface does not — assert for it directly.
		deployStageSource, hasDeployListing := cfg.RunRepo.(deployreconciler.DeployStageSource)
		switch {
		case cfg.RunRepo == nil || cfg.AuditRepo == nil:
			logger.Warn("--enable-deploy-reconciler set but RunRepo or AuditRepo unconfigured; ticker not started")
		case cfg.GitHub == nil:
			logger.Warn("--enable-deploy-reconciler set but GitHub client unconfigured (no app id?); ticker not started")
		case !hasDeployListing:
			logger.Warn("--enable-deploy-reconciler set but RunRepo does not support deploy-stage listing; ticker not started")
		default:
			ticker := &deployreconciler.Ticker{
				Runs:     deployStageSource,
				GH:       cfg.GitHub,
				Audit:    cfg.AuditRepo,
				Resolver: srv,
				Logger:   logger,
				Interval: *deployReconcilerInterval,
			}
			go func() {
				if err := ticker.Run(ctx); err != nil {
					logger.Error("deploy reconciler exited with error", slog.String("error", err.Error()))
				}
			}()
			logger.Info("deploy reconciler started",
				slog.Duration("interval", *deployReconcilerInterval))
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

	// One-shot startup orphaned-review recovery (#1781). A fishhawkd restart
	// mid-review kills the detached reviewing goroutine, so no terminal
	// *_reviewed/*_review_failed entry lands and review_status stays 'pending'
	// forever, wedging the plan/implement gate. ReconcileOrphanedReviews
	// synthesizes the missing terminal *_review_failed entries for any review
	// dispatched by a prior process, flipping review_status to a terminal
	// 'failed' the operator can re-trigger. Best-effort — a failure logs at
	// warn and never blocks server start. Gated on the audit + run wiring the
	// pass reads/writes.
	if srv != nil && cfg.RunRepo != nil && cfg.AuditRepo != nil {
		if n, err := srv.ReconcileOrphanedReviews(ctx); err != nil {
			logger.Warn("startup orphaned-review reconciliation failed", slog.String("error", err.Error()))
		} else if n > 0 {
			logger.Info("startup orphaned-review reconciliation completed", slog.Int("terminated", n))
		}
	}

	// Self-consistency invariant monitor (#764). Generalizes the
	// one-shot startup ReconcileStuckRuns above into a periodic sweep:
	// invariant 1 (all-stages-terminal + run non-terminal) auto-
	// reconciles via the same Orchestrator method; invariant 2 (review
	// awaiting_approval + null PR on a push-and-open-pr run) is surface-
	// only (audit + WARN). Off by default to match the other tickers'
	// dev-loop posture. Requires RunRepo + AuditRepo; Reconcile is wired
	// only when the Orchestrator is configured.
	if *enableInvariantMonitor {
		if cfg.RunRepo == nil || cfg.AuditRepo == nil {
			logger.Warn("--enable-invariant-monitor set but RunRepo or AuditRepo unconfigured; monitor not started")
		} else {
			var reconcile func(context.Context) (int, error)
			if cfg.Orchestrator != nil {
				reconcile = cfg.Orchestrator.ReconcileStuckRuns
			}
			ticker := &invariantmonitor.Ticker{
				Runs:      cfg.RunRepo,
				Audit:     cfg.AuditRepo,
				Reconcile: reconcile,
				Logger:    logger,
				Interval:  *invariantMonitorInterval,
			}
			// Lineage sweep (ADR-035, #868): re-verify open-PR running
			// runs for a foreign commit pushed between report boundaries.
			// Wired only when a GitHub client is present — the sweep is a
			// no-op without one (ReverifyBranchLineage fail-opens on a nil
			// GitHub client), so leaving Lineage nil keeps the intent
			// explicit. Mirrors the merge reconciler's GitHub-nil guard.
			if cfg.GitHub != nil {
				ticker.Lineage = srv
			}
			go func() {
				if err := ticker.Run(ctx); err != nil {
					logger.Error("invariant monitor exited with error", slog.String("error", err.Error()))
				}
			}()
			logger.Info("invariant monitor started",
				slog.Duration("interval", *invariantMonitorInterval),
				slog.Bool("lineage_sweep", cfg.GitHub != nil))
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
			sweeper := newChildCompletionSweeper(cfg, logger, *childCompletionInterval)
			go func() {
				if err := sweeper.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
					logger.Error("child-completion sweeper exited with error", slog.String("error", err.Error()))
				}
			}()
			logger.Info("child-completion sweeper started",
				slog.Duration("interval", *childCompletionInterval))
		}
	}

	// Campaign-driver ticker (E25.5 / #1444 / ADR-047 Track C). Mechanically
	// advances each running campaign: starts runs for dependency-eligible
	// issues (bounded by a concurrency cap) and settles items when their runs
	// reach terminal, re-deriving the campaign state. Off by default to match
	// the other tickers' dev-loop posture. The fail-closed switch refuses to
	// start when a required dependency is unwired (the started runs are minted
	// through srv.StartRunForCampaignIssue, which needs the GitHub client to
	// resolve the workflow spec). MECHANICAL advancement only — started runs
	// park at their gates for the operator-agent until E25.6/E25.7 land.
	if start, skipReason := campaignDriverStartDecision(*enableCampaignDriver, cfg); start {
		ticker := newCampaignDriver(cfg, srv, logger, campaignNotifier, *campaignDriverInterval, *campaignDriverWorkflowID, *campaignDriverWorkflowRef)
		go func() {
			if err := ticker.Run(ctx); err != nil {
				logger.Error("campaign driver exited with error", slog.String("error", err.Error()))
			}
		}()
		logger.Info("campaign driver started",
			slog.Duration("interval", *campaignDriverInterval),
			slog.String("workflow_id", *campaignDriverWorkflowID))
	} else if *enableCampaignDriver {
		logger.Warn(skipReason)
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

func (a githubTeamListerAdapter) ListTeamMembers(ctx context.Context, scope forge.CredentialScope, org, slug string) ([]role.TeamMember, error) {
	got, err := a.c.ListTeamMembers(ctx, scope, org, slug)
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

// IntegrateSlices satisfies childcompletion.Integrator by delegating to
// the orchestrator's fan-in step (ADR-041 / #1142) and converting the
// orchestrator's *SliceConflict to childcompletion's identical type — the
// bridge that keeps childcompletion's import graph free of orchestrator.
func (a childCompletionAdvancer) IntegrateSlices(ctx context.Context, parentRunID uuid.UUID) (*childcompletion.SliceConflict, error) {
	if a.o == nil {
		return nil, nil
	}
	conflict, err := a.o.IntegrateSlices(ctx, parentRunID)
	if err != nil || conflict == nil {
		return nil, err
	}
	return &childcompletion.SliceConflict{
		SliceIndex: conflict.SliceIndex,
		ChildRunID: conflict.ChildRunID,
		Detail:     conflict.Detail,
	}, nil
}

// DispatchChildren satisfies childcompletion.ChildDispatcher by
// delegating to the orchestrator's concurrent decomposed-child dispatch
// (E24.3 / #1143) — the sweeper's fail-closed backstop. Returns (0, nil)
// when the orchestrator is unconfigured, matching the other adapter
// methods' nil-safe posture.
func (a childCompletionAdvancer) DispatchChildren(ctx context.Context, parentRunID uuid.UUID) (int, error) {
	if a.o == nil {
		return 0, nil
	}
	return a.o.DispatchDecomposedChildren(ctx, parentRunID)
}

// newStageOrchestrator builds the stage orchestrator from cfg. Extracted
// from runServe so the wiring is unit-testable: in particular the Drive
// engine that emits the RuleChildrenDispatch run_auto_advanced trail for
// concurrent decomposed-child dispatch (E24.3 / #1143) must be set, so a
// miswiring fails a serve-level test rather than passing behind the
// orchestrator-fake behavioral tests. Drive is nil-safe (Engine.Record/
// Recorded guard a nil receiver); it mirrors server.go's drive wiring.
func newStageOrchestrator(cfg server.Config, logger *slog.Logger) *orchestrator.Orchestrator {
	return &orchestrator.Orchestrator{
		Runs:                cfg.RunRepo,
		GitHub:              cfg.GitHub, // nil-safe; orchestrator skips dispatch when GitHub is nil
		Logger:              logger,
		Artifacts:           cfg.ArtifactRepo,
		Audit:               cfg.AuditRepo,
		MaxParallelChildren: cfg.MaxParallelChildren,
		Drive:               &drive.Engine{Audit: cfg.AuditRepo, Logger: logger},
		// ExternalURL threads the operator-facing base URL into the
		// consolidated PR body's audit-log footer (#1774). Empty degrades the
		// footer URL to a relative /v0/runs/<id>/audit.
		ExternalURL: cfg.ExternalURL,
	}
}

// newChildCompletionSweeper builds the child-completion sweeper from cfg.
// Extracted from runServe so the wiring is unit-testable: the Dispatch
// backstop (childCompletionAdvancer, E24.3 / #1143) must be wired non-nil
// so the fail-closed concurrent-dispatch top-up can't be silently omitted.
func newChildCompletionSweeper(cfg server.Config, logger *slog.Logger, interval time.Duration) *childcompletion.Sweeper {
	adapter := childCompletionAdvancer{cfg.Orchestrator}
	return &childcompletion.Sweeper{
		Runs:      cfg.RunRepo,
		Audit:     cfg.AuditRepo,
		Advance:   adapter,
		Integrate: adapter,
		Dispatch:  adapter,
		Logger:    logger,
		Interval:  interval,
	}
}

// campaignRunStarter adapts *server.Server to campaigndriver.RunStarter: it
// starts a run for an eligible campaign issue through
// Server.StartRunForCampaignIssue (which resolves the workflow spec and routes
// through CreateRunForTrigger). Tiny by design — the driver defines the
// interface so it never imports server, and the run-creation work lives in the
// server package next to handleCreateRun.
type campaignRunStarter struct {
	srv         *server.Server
	workflowID  string
	workflowRef string
}

func (a campaignRunStarter) StartCampaignRun(ctx context.Context, item *campaign.Item, c *campaign.Campaign) (*runpkg.Run, error) {
	// Empty runnerKind → repo-layer github_actions default, the GHA auto-driver
	// behavior. The operator-driven campaign start (E26.2 / #1481) passes
	// "local" through its own call site (handleStartCampaignItemRun).
	return a.srv.StartRunForCampaignIssue(ctx, c.Repo, item.IssueRef, a.workflowID, a.workflowRef, "")
}

// campaignDriverStartDecision reports whether the campaign-driver ticker
// should start, and a human reason when it should not. Extracted from
// runServe so the fail-closed gating is unit-testable (serve_test.go): the
// flag-off case must NOT construct/start the ticker, and a missing required
// dependency must be a logged skip rather than a nil-deref at tick time. The
// driver needs the campaign/run/audit repos (it advances campaigns) plus the
// GitHub client (the run-starter resolves the workflow spec the campaign does
// not carry).
func campaignDriverStartDecision(enabled bool, cfg server.Config) (start bool, skipReason string) {
	if !enabled {
		return false, ""
	}
	switch {
	case cfg.CampaignRepo == nil || cfg.RunRepo == nil || cfg.AuditRepo == nil:
		return false, "--enable-campaign-driver set but CampaignRepo, RunRepo, or AuditRepo unconfigured; ticker not started"
	case cfg.GitHub == nil:
		return false, "--enable-campaign-driver set but GitHub client unconfigured (campaigns carry no inline spec to start runs from); ticker not started"
	default:
		return true, ""
	}
}

// newCampaignDriver builds the campaign-driver ticker from cfg + the server
// (the run-starter adapter binds to it). Extracted from runServe so the wiring
// is unit-testable. Callers must gate on campaignDriverStartDecision first.
func newCampaignDriver(cfg server.Config, srv *server.Server, logger *slog.Logger, notifier *issuecomment.Notifier, interval time.Duration, workflowID, workflowRef string) *campaigndriver.Ticker {
	t := &campaigndriver.Ticker{
		Campaigns: cfg.CampaignRepo,
		Runs:      cfg.RunRepo,
		Starter:   campaignRunStarter{srv: srv, workflowID: workflowID, workflowRef: workflowRef},
		Audit:     cfg.AuditRepo,
		GateActor: newCampaignGateActor(cfg, srv, logger),
		Logger:    logger,
		Interval:  interval,
	}
	// Only set the Notifier seam when a concrete notifier was built. Assigning a
	// typed nil *issuecomment.Notifier would make the interface field non-nil
	// (the typed-nil trap), so the Paged branch would call a nil pointer instead
	// of taking the observe-only path. A genuine nil notifier → nil seam.
	if notifier != nil {
		t.Notifier = notifier
	}
	return t
}

// campaignOperatorIdentity builds the in-process Identity the campaign
// auto-driver (E25.6 / ADR-047) acts under when it takes a delegated gate
// action via Server.AutoDriveRunGate. The subject is the stable operator-agent
// attribution (operatorrole.CampaignActorSubject, stamped audit.ActorAgent) and
// the scope set is the gate-action write scopes the handlers enforce. TokenID
// is set NON-empty so the handler scope check applies the same gate it applies
// to an HTTP bearer token (scope-acceptance parity) rather than the
// cookie-session bypass — the in-process actor must HOLD the scopes.
func campaignOperatorIdentity() server.Identity {
	return server.Identity{
		Subject: operatorrole.CampaignActorSubject,
		TokenID: "operator-agent-campaign",
		Scopes:  operatorrole.CampaignActorScopes(),
	}
}

// githubAutoMerger satisfies server.GitHubMerger over the GitHub App client:
// may_merge dispatches a run's pull request through GitHub's auto-merge
// (EnableAutoMerge, squash), and the existing webhook / resolveReviewStageOnMerge
// path settles the review stage. Fails before any HTTP call when the run lacks
// the installation id or PR url the merge needs.
type githubAutoMerger struct {
	gh *githubclient.Client
}

func (m githubAutoMerger) MergePullRequest(ctx context.Context, runRow *runpkg.Run) error {
	if runRow.InstallationID == nil || *runRow.InstallationID == 0 {
		return fmt.Errorf("campaign auto-merge: run %s has no installation id", runRow.ID)
	}
	if runRow.PullRequestURL == nil || *runRow.PullRequestURL == "" {
		return fmt.Errorf("campaign auto-merge: run %s has no pull request url", runRow.ID)
	}
	repo, number, err := parseCampaignPRURL(*runRow.PullRequestURL)
	if err != nil {
		return fmt.Errorf("campaign auto-merge: %w", err)
	}
	scope := forge.FromGitHubInstallationID(*runRow.InstallationID)
	// Primary: queue GitHub auto-merge so the PR lands once branch protection
	// (required review + the fishhawk_audit_complete check) clears. The webhook
	// / resolveReviewStageOnMerge path settles the review stage on the merge.
	err = m.gh.EnableAutoMerge(ctx, scope, repo, number, forge.MergeMethodSquash)
	if err == nil {
		return nil
	}
	// Fallback (E48.7 / #1954): enablePullRequestAutoMerge errors on a PR that
	// is ALREADY merge-ready ("clean status") — the common operator flow where
	// `gh pr review --approve` plus green required checks settle the PR clean
	// before the merge verb runs. GitHub refuses to queue auto-merge on a
	// synchronously-mergeable PR, so merge it directly via REST. Any OTHER
	// enable error surfaces unchanged (no fallback).
	if errors.Is(err, forge.ErrPullRequestCleanStatus) {
		return m.gh.MergePullRequest(ctx, scope, repo, number, forge.MergeMethodSquash)
	}
	return err
}

// parseCampaignPRURL splits a GitHub PR html_url into its repo ref and number.
// Mirrors mergereconciler.parsePRURL (kept local to avoid importing that
// package for one helper).
func parseCampaignPRURL(prURL string) (forge.RepoRef, int, error) {
	s := strings.TrimSpace(prURL)
	for _, prefix := range []string{"https://github.com/", "http://github.com/"} {
		s = strings.TrimPrefix(s, prefix)
	}
	parts := strings.Split(strings.Trim(s, "/"), "/")
	if len(parts) != 4 || parts[2] != "pull" {
		return forge.RepoRef{}, 0, fmt.Errorf("not a github PR html_url: %q", prURL)
	}
	owner, name, num := parts[0], parts[1], parts[3]
	if owner == "" || name == "" {
		return forge.RepoRef{}, 0, fmt.Errorf("PR url missing owner/name: %q", prURL)
	}
	n, err := strconv.Atoi(num)
	if err != nil || n <= 0 {
		return forge.RepoRef{}, 0, fmt.Errorf("PR url has non-numeric number %q: %q", num, prURL)
	}
	return forge.RepoRef{Owner: owner, Name: name}, n, nil
}

// campaignGateActor adapts *server.Server to campaigndriver.GateActor: it drives
// a run gate via Server.AutoDriveRunGate under the campaign operator identity,
// dispatching may_merge through the GitHub auto-merge client. Translates the
// server outcome to the driver's GateActionOutcome shape.
type campaignGateActor struct {
	srv    *server.Server
	id     server.Identity
	merger server.GitHubMerger
}

// DriveRunGate satisfies the base campaigndriver.GateActor seam (no campaign
// override — the run resolves on its own workflow contract).
func (a campaignGateActor) DriveRunGate(ctx context.Context, runRow *runpkg.Run) (campaigndriver.GateActionOutcome, error) {
	return a.DriveRunGateWithCampaign(ctx, runRow, nil)
}

// DriveRunGateWithCampaign satisfies the campaign-aware extension
// (campaigndriver.CampaignGateActor): it threads the owning campaign's
// operator_agent override bytes (E25.12 / #1451) into AutoDriveRunGate.
func (a campaignGateActor) DriveRunGateWithCampaign(ctx context.Context, runRow *runpkg.Run, campaignOverride []byte) (campaigndriver.GateActionOutcome, error) {
	out, err := a.srv.AutoDriveRunGate(ctx, runRow, a.id, a.merger, campaignOverride)
	return campaigndriver.GateActionOutcome{
		Acted:     out.Acted,
		Action:    out.Action,
		Paged:     out.Paged,
		PageEvent: out.PageEvent,
		Note:      out.Note,
	}, err
}

// newCampaignGateActor builds the campaigndriver.GateActor that auto-acts on
// each run gate under the campaign operator identity (E25.6 / ADR-047), or
// returns nil — the driver then runs OBSERVE-ONLY — when the GitHub client is
// unconfigured. The merge knob needs a GitHub client to enable auto-merge; with
// no client the actor could not honour a delegated may_merge, so rather than
// auto-act on the other knobs while silently dropping merges we fail the whole
// actor closed and leave every gate parked for the human operator-agent.
// campaignDriverStartDecision already refuses to start the driver without a
// GitHub client, so in practice this returns a live actor whenever the driver
// runs; the nil path is the defensive fail-closed contract.
func newCampaignGateActor(cfg server.Config, srv *server.Server, logger *slog.Logger) campaigndriver.GateActor {
	if cfg.GitHub == nil {
		logger.Warn("campaign auto-drive disabled: GitHub merge client unconfigured; driver runs observe-only")
		return nil
	}
	return campaignGateActor{
		srv:    srv,
		id:     campaignOperatorIdentity(),
		merger: githubAutoMerger{gh: cfg.GitHub},
	}
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
