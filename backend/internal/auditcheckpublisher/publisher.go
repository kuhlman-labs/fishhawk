// Package auditcheckpublisher posts the derived
// `fishhawk_audit_complete` state to GitHub as a Check Run on the
// PR's head commit (#231). Once published, repo admins can mark
// the check Required in branch protection and GitHub itself
// refuses the merge until Fishhawk reports `success`.
//
// Why a separate package: the publish path is a side-effect
// counterpart to the pure derivation in `auditcomplete`. Keeping
// them apart lets the derivation stay pure (still callable from
// tests with no GitHub fake), while the publisher carries the
// I/O, the dedup cache, and the env-dependent details_url. Both
// the read endpoint and the approval-handler enforcement call
// Publish best-effort; a failure logs but doesn't unwind the
// in-Fishhawk gate.
//
// What this package does NOT do:
//   - Fan out to multiple PRs per run. The implement stage's
//     pull_request artifact carries a single PR; multi-PR
//     workflows would need a second pass here.
//   - Post on GitHub push events. We publish only when a Fishhawk
//     surface (read endpoint or approval) computes a state. If
//     the customer pushes between reads the check stays at the
//     last published value until the next compute. Acceptable
//     for v0; tighter behaviour belongs in a webhook listener.
//   - Translate `ci_pass` or other externally-fed checks. Those
//     ORIGINATE from GitHub; re-posting them would be circular.
package auditcheckpublisher

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/artifact"
	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/auditcomplete"
	"github.com/kuhlman-labs/fishhawk/backend/internal/githubclient"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
	"github.com/kuhlman-labs/fishhawk/backend/internal/stagecheck"
)

// CheckName is the reserved GitHub Check Run name. Customers wire
// this exact string into their branch-protection rule.
const CheckName = "fishhawk_audit_complete"

// DefaultDegradedThreshold is the number of CONSECUTIVE failed
// CreateCheckRun attempts for one (run, head_sha) episode after which
// the publisher fires OnDegraded (#993). At the reconciler's one
// attempt per tick (mergereconciler.DefaultInterval = 60s) that is
// ~5 minutes of sustained failure; other Publish callers (SPA reads,
// synchronize webhooks) count toward the same episode and can only
// make the signal earlier, never noisier.
const DefaultDegradedThreshold = 5

// CheckRunCreator is the slice of githubclient.Client this package
// needs. Defining it as an interface lets tests swap in a fake
// without standing up a fake api.github.com.
type CheckRunCreator interface {
	CreateCheckRun(ctx context.Context, installationID int64, repo githubclient.RepoRef, p githubclient.CreateCheckRunParams) (*githubclient.CreateCheckRunResult, error)
}

// AuditReader is the slice of audit.Repository the publisher needs to prefer
// the run's newest fixup_pushed head over the stale PR-open artifact head
// (#1682). A narrow interface (not the full audit.Repository) keeps the test
// fake trivial. Optional — a nil AuditReader falls back to the artifact head.
type AuditReader interface {
	ListForRunByCategory(ctx context.Context, runID uuid.UUID, category string) ([]*audit.Entry, error)
}

// Publisher publishes audit-complete state to GitHub. Construct
// once with New and share — concurrent calls to Publish are safe.
type Publisher struct {
	github      CheckRunCreator
	runs        run.Repository
	artifacts   artifact.Repository
	audit       AuditReader
	externalURL string
	onDegraded  func(ctx context.Context, runID uuid.UUID, headSHA string, attempts int, lastErr error)
	onRecovered func(ctx context.Context, runID uuid.UUID, headSHA string, attempts int)

	mu       sync.Mutex
	last     map[string]stagecheck.State // (repo, head_sha) → most-recent published state
	episodes map[string]*episode         // (run_id, head_sha) → consecutive CreateCheckRun failure streak
}

// episode tracks one (run, head_sha) pair's consecutive-failure
// streak. `degraded` latches once OnDegraded has fired so attempts
// past the threshold stay silent; the episode is deleted on the
// first successful publish.
type episode struct {
	count    int
	degraded bool
}

// Deps groups the dependencies New needs. Production wires the
// real Postgres-backed repos and the typed githubclient.
type Deps struct {
	GitHub      CheckRunCreator
	Runs        run.Repository
	Artifacts   artifact.Repository
	ExternalURL string

	// Audit, when set, lets findHeadSHA prefer the run's newest fixup_pushed
	// head over the stale PR-open artifact head (#1682) — resolved through the
	// SAME auditcomplete.LatestReportedHeadSHA ordering the server-side
	// resolver uses, so audit_complete publishing and acceptance/retry head
	// binding cannot diverge. Nil (legacy / dev posture) falls back to the
	// artifact head, preserving the pre-#1682 behavior.
	Audit AuditReader

	// OnDegraded, when non-nil, is invoked exactly once per failure
	// episode (#993): the moment a (run, head_sha) pair accumulates
	// DefaultDegradedThreshold CONSECUTIVE CreateCheckRun failures.
	// Only the publish attempt proper counts — GetRun/findHeadSHA
	// read errors and the skip paths never touch the streak. Invoked
	// outside the publisher's mutex; the callback may do DB I/O.
	OnDegraded func(ctx context.Context, runID uuid.UUID, headSHA string, attempts int, lastErr error)

	// OnRecovered, when non-nil, is invoked after EVERY successful
	// CreateCheckRun publish AND after every dedup no-op (a hit means
	// the desired state is already live on GitHub — possibly posted by
	// another run sharing the head commit), carrying the in-process
	// failure streak length the success cleared (0 when there was
	// none). Whether a
	// recovered signal is actually due is the callee's decision from
	// durable state (the run's audit chain), NOT this process's
	// counter — so a daemon restart mid-episode can never orphan a
	// degraded signal. Invoked outside the publisher's mutex.
	OnRecovered func(ctx context.Context, runID uuid.UUID, headSHA string, attempts int)
}

// New returns a Publisher. Returns nil when the deps don't add up
// to a working publisher (no GitHub client, no run repo, no
// artifact repo, or no ExternalURL) — callers must nil-check, and
// Publish on a nil receiver is a safe no-op so callers can write
// `pub.Publish(...)` without branching.
//
// We bail on missing ExternalURL because the GitHub Check Run's
// details_url has to point somewhere; without one the check would
// be a dead-end on github.com.
func New(d Deps) *Publisher {
	if d.GitHub == nil || d.Runs == nil || d.Artifacts == nil || d.ExternalURL == "" {
		return nil
	}
	return &Publisher{
		github:      d.GitHub,
		runs:        d.Runs,
		artifacts:   d.Artifacts,
		audit:       d.Audit,
		externalURL: strings.TrimRight(d.ExternalURL, "/"),
		onDegraded:  d.OnDegraded,
		onRecovered: d.OnRecovered,
		last:        map[string]stagecheck.State{},
		episodes:    map[string]*episode{},
	}
}

// Publish posts the given (state, missing) to GitHub for the run
// if it hasn't been posted with the same state already. Best-
// effort: returns errors so callers can log them, but a publish
// failure should not unwind whatever computed the state.
//
// Skips silently and returns (false, nil) when:
//   - The receiver is nil (Publisher disabled).
//   - The run has no implement-stage pull_request artifact yet
//     (no head_sha to publish against).
//   - The run lacks installation_id or a parseable repo (non-
//     GitHub-triggered runs, e.g. CLI ad-hoc).
//   - The most-recent published state for this (repo, head_sha)
//     already matches — don't spam GitHub on every read. This path
//     still clears the run's failure episode and fires OnRecovered:
//     the state being live on GitHub ends the episode even when
//     another run sharing the head commit published it.
//
// The bool return is "did we actually publish to GitHub on this
// call." Useful for tests; production callers usually ignore it.
func (p *Publisher) Publish(ctx context.Context, runID uuid.UUID, state stagecheck.State, missing []auditcomplete.MissingItem) (bool, error) {
	if p == nil {
		return false, nil
	}

	runRow, err := p.runs.GetRun(ctx, runID)
	if err != nil {
		return false, fmt.Errorf("auditcheckpublisher: get run: %w", err)
	}
	if runRow.InstallationID == nil {
		return false, nil
	}
	repo, err := parseRepo(runRow.Repo)
	if err != nil {
		return false, nil
	}
	headSHA, ok, err := p.findHeadSHA(ctx, runID)
	if err != nil {
		return false, fmt.Errorf("auditcheckpublisher: find head_sha: %w", err)
	}
	if !ok {
		return false, nil
	}

	if !p.shouldPublish(repo, headSHA, state) {
		// The dedup cache records only successful publishes, so a hit
		// means this (repo, head_sha) already carries `state` on GitHub
		// — posted by this run or by another run sharing the head
		// commit. Either way the merge gate at this head is current, so
		// any open failure episode for THIS run is over: clear it and
		// fire OnRecovered. Without this, a run whose twin filled the
		// dedup cache first would never close its episode (#993). The
		// callee derives whether a recovered entry is actually due from
		// the run's audit chain, so firing on every hit is idempotent.
		attempts := p.clearEpisode(runID, headSHA)
		if p.onRecovered != nil {
			p.onRecovered(ctx, runID, headSHA, attempts)
		}
		return false, nil
	}

	params := buildParams(state, missing, headSHA, p.detailsURL(runID))
	if _, err := p.github.CreateCheckRun(ctx, *runRow.InstallationID, repo, params); err != nil {
		err = fmt.Errorf("auditcheckpublisher: create check run: %w", err)
		p.recordFailure(ctx, runID, headSHA, err)
		return false, err
	}
	attempts := p.recordPublished(repo, runID, headSHA, state)
	if p.onRecovered != nil {
		p.onRecovered(ctx, runID, headSHA, attempts)
	}
	return true, nil
}

// recordFailure advances the (run, head_sha) consecutive-failure
// streak and fires OnDegraded exactly when the streak reaches
// DefaultDegradedThreshold (== compare; the latched `degraded` flag
// keeps attempts past the threshold silent). The decision is made
// under the mutex but the callback runs after release — it does DB
// I/O and must not serialize concurrent Publish calls.
func (p *Publisher) recordFailure(ctx context.Context, runID uuid.UUID, headSHA string, lastErr error) {
	key := episodeKey(runID, headSHA)
	p.mu.Lock()
	ep := p.episodes[key]
	if ep == nil {
		ep = &episode{}
		p.episodes[key] = ep
	}
	ep.count++
	fire := ep.count == DefaultDegradedThreshold && !ep.degraded
	if fire {
		ep.degraded = true
	}
	attempts := ep.count
	p.mu.Unlock()
	if fire && p.onDegraded != nil {
		p.onDegraded(ctx, runID, headSHA, attempts, lastErr)
	}
}

// shouldPublish returns true when the cached state for this
// (repo, head_sha) differs from `state`. Cache miss → publish
// (the conservative default — operators expect to see the row
// after a backend restart).
func (p *Publisher) shouldPublish(repo githubclient.RepoRef, headSHA string, state stagecheck.State) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	prev, ok := p.last[cacheKey(repo, headSHA)]
	if !ok {
		return true
	}
	return prev != state
}

// recordPublished caches the published state for dedup and clears the
// run's failure episode, returning the streak length the success ended
// (0 when there was none).
func (p *Publisher) recordPublished(repo githubclient.RepoRef, runID uuid.UUID, headSHA string, state stagecheck.State) int {
	p.mu.Lock()
	p.last[cacheKey(repo, headSHA)] = state
	p.mu.Unlock()
	return p.clearEpisode(runID, headSHA)
}

// clearEpisode deletes the (run, head_sha) failure episode, returning
// the streak length it ended (0 when there was none). Called on a real
// publish (recordPublished) AND on a dedup hit — the cache records
// only successes, so either way the state is live on GitHub and the
// episode is over.
func (p *Publisher) clearEpisode(runID uuid.UUID, headSHA string) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	key := episodeKey(runID, headSHA)
	attempts := 0
	if ep := p.episodes[key]; ep != nil {
		attempts = ep.count
		delete(p.episodes, key)
	}
	return attempts
}

func cacheKey(repo githubclient.RepoRef, headSHA string) string {
	return repo.Owner + "/" + repo.Name + "@" + headSHA
}

// episodeKey keys failure episodes by (run_id, head_sha) — NOT the
// dedup cache's (repo, head_sha) — so two runs sharing a head commit
// have independent episodes with entries on their own run chains.
func episodeKey(runID uuid.UUID, headSHA string) string {
	return runID.String() + "@" + headSHA
}

func (p *Publisher) detailsURL(runID uuid.UUID) string {
	return p.externalURL + "/runs/" + runID.String()
}

// findHeadSHA resolves the head the fishhawk_audit_complete Check Run targets.
//
// #1682: it FIRST prefers the run's newest recorded head from the audit chain
// (fixup_pushed > child_pushed > pull_request_opened, via the shared
// auditcomplete.LatestReportedHeadSHA), so a fix-up push retargets the check
// onto the new commit instead of leaving it pinned to the stale PR-open head.
// This is the SAME ordering server.latestRunHeadSHA uses, so audit_complete
// publishing and the acceptance/retry head binding cannot resolve divergent
// heads for the same audit history. When the audit reader is not wired (nil
// Audit dep) or records no head, it falls back to the pull_request artifact
// head — the pre-#1682 behavior.
//
// Returns (sha, true, nil) on success, ("", false, nil) when the run has no
// recorded head and no implement-stage PR artifact (still dispatching, or a
// workflow shape with no implement stage yet).
//
// We re-decode the artifact content rather than reaching into the
// server.pullRequestBody type to avoid an import cycle (the server
// package depends on this one in production).
func (p *Publisher) findHeadSHA(ctx context.Context, runID uuid.UUID) (string, bool, error) {
	// Prefer the newest audit-recorded head (#1682). A read error is returned
	// so the caller surfaces it rather than silently publishing to a stale
	// head; a clean miss falls through to the artifact head below.
	if p.audit != nil {
		var entries []*audit.Entry
		for _, cat := range auditcomplete.HeadReportCategoriesByPrecedence {
			es, err := p.audit.ListForRunByCategory(ctx, runID, cat)
			if err != nil {
				return "", false, fmt.Errorf("auditcheckpublisher: list %s heads: %w", cat, err)
			}
			entries = append(entries, es...)
		}
		if sha, ok := auditcomplete.LatestReportedHeadSHA(entries); ok {
			return sha, true, nil
		}
	}

	stages, err := p.runs.ListStagesForRun(ctx, runID)
	if err != nil {
		return "", false, err
	}
	var implementStage *run.Stage
	for _, s := range stages {
		if s.Type == run.StageTypeImplement {
			implementStage = s
			break
		}
	}
	if implementStage == nil {
		return "", false, nil
	}
	arts, err := p.artifacts.ListForStage(ctx, implementStage.ID)
	if err != nil {
		return "", false, err
	}
	for _, a := range arts {
		if a.Kind != artifact.KindPullRequest {
			continue
		}
		sha := decodeHeadSHA(a.Content)
		if sha != "" {
			return sha, true, nil
		}
	}
	return "", false, nil
}

// decodeHeadSHA extracts head_sha from a pull_request artifact's
// content. Returns "" on any decode failure or absent field; the
// caller treats that as "not yet ready to publish."
func decodeHeadSHA(content []byte) string {
	if len(content) == 0 {
		return ""
	}
	var body struct {
		HeadSHA string `json:"head_sha"`
	}
	if err := json.Unmarshal(content, &body); err != nil {
		return ""
	}
	return body.HeadSHA
}

// parseRepo splits "owner/name" into a RepoRef. Mirrors the
// server-package helper of the same name; duplicated here so
// this package doesn't have to import the server.
func parseRepo(s string) (githubclient.RepoRef, error) {
	parts := strings.SplitN(s, "/", 3)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return githubclient.RepoRef{}, errors.New("repo must be owner/name")
	}
	return githubclient.RepoRef{Owner: parts[0], Name: parts[1]}, nil
}

// buildParams maps the (state, missing) tuple to GitHub's check-
// run wire shape. Pending → in_progress; pass → success; fail →
// failure with the missing list rendered as a markdown summary.
func buildParams(state stagecheck.State, missing []auditcomplete.MissingItem, headSHA, detailsURL string) githubclient.CreateCheckRunParams {
	params := githubclient.CreateCheckRunParams{
		Name:       CheckName,
		HeadSHA:    headSHA,
		DetailsURL: detailsURL,
	}
	switch state {
	case stagecheck.StatePass:
		params.Status = githubclient.CheckRunStatusCompleted
		params.Conclusion = githubclient.CheckRunConclusionSuccess
		params.OutputSummary = "Audit chain is intact: plan, traces (raw + redacted), and pull request all present, audit chain verifies."
	case stagecheck.StateFail:
		params.Status = githubclient.CheckRunStatusCompleted
		params.Conclusion = githubclient.CheckRunConclusionFailure
		params.OutputSummary = renderFailureSummary(missing)
	default:
		// Anything else (pending, not_tracked, empty) is
		// in_progress with no conclusion. The "" -> in_progress
		// fallback is defensive against future enum additions —
		// publishing nothing would let a stale prior state ride.
		params.Status = githubclient.CheckRunStatusInProgress
		params.OutputSummary = "Audit chain is still being assembled. Fishhawk will update this check when the run terminates."
	}
	return params
}

func renderFailureSummary(missing []auditcomplete.MissingItem) string {
	if len(missing) == 0 {
		return "Audit chain is incomplete. Open the run in Fishhawk to see what's missing."
	}
	var b strings.Builder
	b.WriteString("Audit chain is incomplete:\n\n")
	for _, m := range missing {
		fmt.Fprintf(&b, "- **%s** — %s\n", m.Kind, m.Detail)
	}
	return b.String()
}
