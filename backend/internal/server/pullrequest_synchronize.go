package server

import (
	"context"
	"encoding/json"
	"log/slog"
	"strings"

	"github.com/kuhlman-labs/fishhawk/backend/internal/auditcomplete"
	"github.com/kuhlman-labs/fishhawk/backend/internal/forge"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
	"github.com/kuhlman-labs/fishhawk/backend/internal/webhook"
)

// synchronizePayload is the slice of GitHub's pull_request payload
// Fishhawk reads. Mirrors the wire shape; we need the PR URL to look up
// a Fishhawk run, the head SHA to publish against, and — since E64.43
// (#3160) — the PR AUTHOR, which is half of the App-identity
// discriminator that keeps a Fishhawk-managed PR from ever receiving the
// not-applicable conclusion.
type synchronizePayload struct {
	PullRequest struct {
		HTMLURL string `json:"html_url"`
		Number  int    `json:"number"`
		Head    struct {
			SHA string `json:"sha"`
		} `json:"head"`
		User struct {
			Login string `json:"login"`
			Type  string `json:"type"`
		} `json:"user"`
	} `json:"pull_request"`
}

// republishOnPullRequestEvent handles a GitHub `pull_request` event
// (`opened`, `reopened` or `synchronize`) by finding the matching
// Fishhawk run on the PR URL (#216 denormalizes pull_request_url onto
// every run) and re-running the audit-complete Compute + publish flow.
// This is what makes the foreign-commit rule (#282) surface drift to
// branch protection without waiting for a SPA visitor to trigger a fresh
// Compute via GET /v0/stages/{id}/checks.
//
// E64.43 (#3160): when the PR has NO Fishhawk run, the handler publishes
// a terminal `neutral` not-applicable Check Run instead of no-oping — but
// only behind the App-identity discriminator below, which must positively
// establish that the PR was neither opened nor pushed by Fishhawk's own
// GitHub App. That guard is FAIL-CLOSED in both directions: an
// unresolvable identity publishes nothing.
//
// Best-effort throughout. A skip emits a structured log line; an
// unrecoverable I/O failure does too. We never 5xx — the canonical
// signal is already on the audit chain via existing flows, and a
// missed delivery only delays the recompute until the next SPA
// visit or webhook on the same PR.
func (s *Server) republishOnPullRequestEvent(ctx context.Context, ev webhook.Event) {
	if s.cfg.RunRepo == nil || s.cfg.ArtifactRepo == nil || s.cfg.AuditRepo == nil {
		return
	}
	var p synchronizePayload
	if err := json.Unmarshal(ev.RawBody, &p); err != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn, "pull_request: parse failed",
			slog.String("action", ev.Action),
			slog.String("error", err.Error()))
		return
	}
	prURL := p.PullRequest.HTMLURL
	if prURL == "" {
		return
	}

	runs, err := s.cfg.RunRepo.ListRuns(ctx, run.ListRunsFilter{
		PullRequestURL: &prURL,
		Limit:          5,
	})
	if err != nil {
		// A lookup ERROR is not evidence of zero runs, so it must not
		// reach the not-applicable publish below. Log and return.
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn,
			"pull_request: run lookup failed",
			slog.String("action", ev.Action),
			slog.String("pr_url", prURL),
			slog.String("error", err.Error()))
		return
	}
	if len(runs) == 0 {
		s.publishNotApplicableForRunlessPR(ctx, ev, p, prURL)
		return
	}
	// The most-recent run on the PR is what branch protection cares
	// about. Recompute against that run's chain — the chain walk
	// inside `auditcomplete.gatherForeignCommitInputs` brings in
	// the parent runs' head_shas, so retry chains (post-#276) work
	// without special-casing here.
	target := runs[0]

	res, err := auditcomplete.ComputeResult(ctx, target.ID, s.auditCompleteDeps())
	if err != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn,
			"pull_request: compute failed",
			slog.String("action", ev.Action),
			slog.String("run_id", target.ID.String()),
			slog.String("error", err.Error()))
		return
	}

	// Publish the fresh state to GitHub so branch protection
	// re-evaluates. Nil-safe — `publishAuditCheck` short-circuits
	// when the publisher isn't wired (dev posture). The decomposition
	// RESOLUTIONS (#3092) ride along so a synchronize-triggered republish
	// carries the same pass summary the read endpoint would.
	s.publishAuditCheck(ctx, target.ID, res.State, res.Missing, res.Resolved)

	s.cfg.Logger.LogAttrs(ctx, slog.LevelInfo, "pull_request: republished audit-complete",
		slog.String("action", ev.Action),
		slog.String("pr_url", prURL),
		slog.String("head_sha", p.PullRequest.Head.SHA),
		slog.String("run_id", target.ID.String()),
		slog.String("state", string(res.State)),
		slog.Int("missing_count", len(res.Missing)),
		slog.Int("resolved_count", len(res.Resolved)),
	)
}

// publishNotApplicableForRunlessPR is the zero-run branch of
// republishOnPullRequestEvent (E64.43 / #3160). It publishes the terminal
// `neutral` not-applicable Check Run ONLY when it can positively
// establish the PR is not ours; every other outcome returns without
// publishing.
func (s *Server) publishNotApplicableForRunlessPR(ctx context.Context, ev webhook.Event, p synchronizePayload, prURL string) {
	isOurs, resolved := s.authoredByFishhawkApp(ctx, ev, p)
	if !resolved {
		// FAIL CLOSED. We cannot tell whether this PR is Fishhawk's own,
		// so we publish nothing: a missing publish leaves the PR blocked
		// (today's behaviour, recoverable with `gh pr merge --admin`),
		// while a wrong publish greens a real audit gate silently and is
		// not recoverable. This WARN is what makes the residual visible
		// to an operator.
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn,
			"pull_request: zero runs but App identity unresolvable — not publishing not-applicable",
			slog.String("action", ev.Action),
			slog.String("pr_url", prURL))
		return
	}
	if isOurs {
		// A Fishhawk-authored (or Fishhawk-pushed) PR with zero runs is a
		// denormalization lag, not a foreign PR. Publishing neutral here
		// would green an audit gate that was never verified.
		s.cfg.Logger.LogAttrs(ctx, slog.LevelInfo,
			"pull_request: zero runs on a PR our own App authored — run row not yet denormalized; not publishing not-applicable",
			slog.String("action", ev.Action),
			slog.String("pr_url", prURL))
		return
	}

	repo, err := parseRepoRef(ev.Repo)
	if err != nil {
		return
	}
	scope := forge.FromGitHubInstallationID(ev.InstallationID)
	headSHA := strings.TrimSpace(p.PullRequest.Head.SHA)
	if headSHA == "" {
		return
	}
	// Nil-receiver-safe, so the dev posture where the publisher is
	// unwired needs no branch here.
	published, err := s.auditCheckPublisher.PublishNotApplicable(ctx, repo, scope, headSHA)
	if err != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn,
			"pull_request: not-applicable check-run publish failed",
			slog.String("action", ev.Action),
			slog.String("pr_url", prURL),
			slog.String("head_sha", headSHA),
			slog.String("error", err.Error()))
		return
	}
	if published {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelInfo,
			"pull_request: published not-applicable audit-complete",
			slog.String("action", ev.Action),
			slog.String("pr_url", prURL),
			slog.String("head_sha", headSHA))
	}
}

// fishhawkAppBotLogin returns Fishhawk's own GitHub App bot login
// (`<app-slug>[bot]`), or "" when it cannot be resolved.
//
// It reuses the EXISTING memoized resolveAppBotIdentity (prompt.go),
// whose `name` return IS that login, derived from the App's own
// `GET /app` slug — so no login literal is hardcoded and a self-hosted
// install running under a different App resolves ITS OWN slug.
//
// Returns "" when GitHub is unconfigured, no App JWT is wired (the
// dev / CLI posture), or either underlying call (GetApp, GetUser) fails.
// resolveAppBotIdentity memoizes only SUCCESSFUL resolutions, so the
// steady-state cost is one round-trip per daemon start and a failure is
// retried on the next event rather than cached.
func (s *Server) fishhawkAppBotLogin(ctx context.Context) string {
	name, _ := s.resolveAppBotIdentity(ctx)
	return name
}

// authoredByFishhawkApp reports whether this pull_request event belongs
// to a PR Fishhawk's own GitHub App opened or pushed (E64.43 / #3160).
//
// It returns (isOurs, resolved). `resolved` is false when the App
// identity could not be determined at all — the caller MUST then publish
// nothing (fail closed); a false `isOurs` under a false `resolved` is
// "unknown", never "not ours".
//
// The match is case-insensitive against BOTH the PR author and the event
// sender, deliberately: the AUTHOR covers a Fishhawk-opened PR later
// reopened or pushed by someone else, and the SENDER covers a fix-up push
// our App makes to a PR whose run row is not yet denormalized.
func (s *Server) authoredByFishhawkApp(ctx context.Context, ev webhook.Event, p synchronizePayload) (isOurs bool, resolved bool) {
	login := strings.TrimSpace(s.fishhawkAppBotLogin(ctx))
	if login == "" {
		return false, false
	}
	if strings.EqualFold(login, strings.TrimSpace(p.PullRequest.User.Login)) {
		return true, true
	}
	if strings.EqualFold(login, strings.TrimSpace(ev.Sender)) {
		return true, true
	}
	return false, true
}
