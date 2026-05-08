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
	"github.com/kuhlman-labs/fishhawk/backend/internal/auditcomplete"
	"github.com/kuhlman-labs/fishhawk/backend/internal/githubclient"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
	"github.com/kuhlman-labs/fishhawk/backend/internal/stagecheck"
)

// CheckName is the reserved GitHub Check Run name. Customers wire
// this exact string into their branch-protection rule.
const CheckName = "fishhawk_audit_complete"

// CheckRunCreator is the slice of githubclient.Client this package
// needs. Defining it as an interface lets tests swap in a fake
// without standing up a fake api.github.com.
type CheckRunCreator interface {
	CreateCheckRun(ctx context.Context, installationID int64, repo githubclient.RepoRef, p githubclient.CreateCheckRunParams) (*githubclient.CreateCheckRunResult, error)
}

// Publisher publishes audit-complete state to GitHub. Construct
// once with New and share — concurrent calls to Publish are safe.
type Publisher struct {
	github      CheckRunCreator
	runs        run.Repository
	artifacts   artifact.Repository
	externalURL string

	mu   sync.Mutex
	last map[string]stagecheck.State // (repo, head_sha) → most-recent published state
}

// Deps groups the dependencies New needs. Production wires the
// real Postgres-backed repos and the typed githubclient.
type Deps struct {
	GitHub      CheckRunCreator
	Runs        run.Repository
	Artifacts   artifact.Repository
	ExternalURL string
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
		externalURL: strings.TrimRight(d.ExternalURL, "/"),
		last:        map[string]stagecheck.State{},
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
//     already matches — don't spam GitHub on every read.
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
		return false, nil
	}

	params := buildParams(state, missing, headSHA, p.detailsURL(runID))
	if _, err := p.github.CreateCheckRun(ctx, *runRow.InstallationID, repo, params); err != nil {
		return false, fmt.Errorf("auditcheckpublisher: create check run: %w", err)
	}
	p.recordPublished(repo, headSHA, state)
	return true, nil
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

func (p *Publisher) recordPublished(repo githubclient.RepoRef, headSHA string, state stagecheck.State) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.last[cacheKey(repo, headSHA)] = state
}

func cacheKey(repo githubclient.RepoRef, headSHA string) string {
	return repo.Owner + "/" + repo.Name + "@" + headSHA
}

func (p *Publisher) detailsURL(runID uuid.UUID) string {
	return p.externalURL + "/runs/" + runID.String()
}

// findHeadSHA walks the run's stages, locates the implement stage,
// and pulls head_sha out of its pull_request artifact body. Returns
// (sha, true, nil) on success, ("", false, nil) when the run has no
// implement stage or no PR artifact (still dispatching, or a
// workflow shape with no implement stage yet).
//
// We re-decode the artifact content rather than reaching into the
// server.pullRequestBody type to avoid an import cycle (the server
// package depends on this one in production).
func (p *Publisher) findHeadSHA(ctx context.Context, runID uuid.UUID) (string, bool, error) {
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
