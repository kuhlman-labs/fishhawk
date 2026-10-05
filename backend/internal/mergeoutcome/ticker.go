package mergeoutcome

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/forge"
	"github.com/kuhlman-labs/fishhawk/backend/internal/githubclient"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

// CI ticker defaults.
const (
	// DefaultCIInterval is the scan cadence.
	DefaultCIInterval = 10 * time.Minute
	// DefaultCIWindow is the maturity window: a merge commit's CI is fixed
	// at forge merged_at + window.
	DefaultCIWindow = 6 * time.Hour
	// DefaultCILookback bounds the scan by MERGE-EVIDENCE time (operator
	// condition 4): a run whose earliest merge-evidence row is older is not
	// evaluated. Never narrower than window + DefaultCITerminalAfter + a day,
	// so every run reaches its terminal row before it leaves the scan.
	DefaultCILookback = 14 * 24 * time.Hour
	// DefaultCIMaxPerTick caps forge evaluations per tick, so a backfill
	// drains over several ticks.
	DefaultCIMaxPerTick = 20
	// DefaultCITerminalAfter is how long after maturity a still-unobservable
	// run keeps being retried before it records a terminal `missing` with
	// missing_reason observation_failed (operator condition 5).
	DefaultCITerminalAfter = 7 * 24 * time.Hour
	// defaultCIMaxBackoff caps the per-run exponential backoff.
	defaultCIMaxBackoff = 12 * time.Hour
)

// lookbackSlack is the margin the lookback keeps past window + terminal.
const lookbackSlack = 24 * time.Hour

// sourceCheckRunsPoll is the payload's source tag for run_merge_ci_observed.
const sourceCheckRunsPoll = "github_check_runs_poll"

// mergeEvidenceCategories are the chain rows that mark a run merged; the
// earliest one's timestamp is the run's merge-evidence time.
var mergeEvidenceCategories = []string{"pr_merged", "post_merge_observed", "merge_observation_recorded"}

// Observation error classes recorded as observation_error_class on a
// terminal row. Forge errors classify by their typed sentinel.
const (
	errClassNotFound     = "not_found"
	errClassForbidden    = "forbidden"
	errClassNotInstalled = "not_installed"
	errClassValidation   = "validation"
	errClassTransient    = "transient"
	errClassForge        = "forge_error"
	errClassNotMerged    = "pr_not_merged"
	errClassMergeFacts   = "merge_facts_missing"
	errClassAuditAppend  = "audit_append"
)

// NewCITicker's named not-started reasons.
const (
	ciDisabledReason = "disabled"
	ciNoRunsReason   = "RunRepo unconfigured"
	ciNoAuditReason  = "AuditRepo unconfigured"
	ciNoGitHubReason = "GitHub client unconfigured (no app id?)"
)

// Why a run has no CI observation target (logged once, then skipped).
const (
	ciSkipNoPRURL        = "no_pull_request_url"
	ciSkipNonGitHub      = "non_github_family"
	ciSkipNoInstallation = "no_installation"
	ciSkipMalformedPRURL = "malformed_pull_request_url"
	ciSkipPRRepoMismatch = "pull_request_url_repo_mismatch"
	ciSkipRunNotFound    = "run_not_found"
)

// CIRunStore loads a candidate run.
type CIRunStore interface {
	GetRun(ctx context.Context, id uuid.UUID) (*run.Run, error)
}

// CIAuditStore is the audit surface the ticker reads and writes.
type CIAuditStore interface {
	AuditStore
	ListAll(ctx context.Context, p audit.ListAllParams) ([]*audit.Entry, error)
}

// CIForge is the forge surface the ticker calls; *githubclient.Client
// satisfies it.
type CIForge interface {
	GetPullRequest(ctx context.Context, scope forge.CredentialScope, repo forge.RepoRef, number int) (*forge.PullRequest, error)
	ListCheckRunsForRef(ctx context.Context, scope forge.CredentialScope, repo forge.RepoRef, ref string) ([]githubclient.CheckRunSummary, bool, error)
}

// CITicker fixes each merged run's merge-commit CI conclusion ONCE, at
// maturity, as a run_merge_ci_observed row.
type CITicker struct {
	Runs   CIRunStore
	Audit  CIAuditStore
	Forge  CIForge
	Logger *slog.Logger
	// Zero values use the package defaults.
	Interval      time.Duration
	Window        time.Duration
	Lookback      time.Duration
	MaxPerTick    int
	TerminalAfter time.Duration
	MaxBackoff    time.Duration
	RetryBackoff  time.Duration
	Now           func() time.Time

	mu      sync.Mutex
	backoff map[uuid.UUID]*ciBackoff
	skipped map[uuid.UUID]bool
}

// ciBackoff is one run's in-memory retry state.
type ciBackoff struct {
	failures    int
	nextAttempt time.Time
}

// CITickerConfig is the flag-to-ticker input NewCITicker validates.
type CITickerConfig struct {
	Enabled bool
	Runs    CIRunStore
	Audit   CIAuditStore
	// GitHub is concrete so a nil client is caught HERE, before it is
	// wrapped in the CIForge interface (a typed nil would pass != nil).
	GitHub   *githubclient.Client
	Logger   *slog.Logger
	Interval time.Duration
	Window   time.Duration
}

// NewCITicker builds the ticker fishhawkd starts, or returns nil and the
// named reason it is not started (disabled, or a missing dependency).
func NewCITicker(c CITickerConfig) (*CITicker, string) {
	switch {
	case !c.Enabled:
		return nil, ciDisabledReason
	case c.Runs == nil:
		return nil, ciNoRunsReason
	case c.Audit == nil:
		return nil, ciNoAuditReason
	case c.GitHub == nil:
		return nil, ciNoGitHubReason
	}
	return &CITicker{
		Runs:     c.Runs,
		Audit:    c.Audit,
		Forge:    c.GitHub,
		Logger:   c.Logger,
		Interval: c.Interval,
		Window:   c.Window,
	}, ""
}

func (t *CITicker) logger() *slog.Logger {
	if t.Logger != nil {
		return t.Logger
	}
	return slog.Default()
}

func (t *CITicker) now() time.Time {
	if t.Now != nil {
		return t.Now()
	}
	return time.Now()
}

func (t *CITicker) interval() time.Duration {
	if t.Interval > 0 {
		return t.Interval
	}
	return DefaultCIInterval
}

func (t *CITicker) window() time.Duration {
	if t.Window > 0 {
		return t.Window
	}
	return DefaultCIWindow
}

func (t *CITicker) terminalAfter() time.Duration {
	if t.TerminalAfter > 0 {
		return t.TerminalAfter
	}
	return DefaultCITerminalAfter
}

// lookback never drops a run before its terminal deadline has passed.
func (t *CITicker) lookback() time.Duration {
	lb := t.Lookback
	if lb <= 0 {
		lb = DefaultCILookback
	}
	if floor := t.window() + t.terminalAfter() + lookbackSlack; lb < floor {
		lb = floor
	}
	return lb
}

func (t *CITicker) maxPerTick() int {
	if t.MaxPerTick > 0 {
		return t.MaxPerTick
	}
	return DefaultCIMaxPerTick
}

func (t *CITicker) maxBackoff() time.Duration {
	if t.MaxBackoff > 0 {
		return t.MaxBackoff
	}
	return defaultCIMaxBackoff
}

// Run ticks once immediately, then every Interval until ctx ends.
func (t *CITicker) Run(ctx context.Context) error {
	if t.Runs == nil || t.Audit == nil || t.Forge == nil {
		return errors.New("mergeoutcome: CI ticker requires Runs, Audit and Forge")
	}
	t.Tick(ctx)
	tick := time.NewTicker(t.interval())
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-tick.C:
			t.Tick(ctx)
		}
	}
}

// CITickSummary reports what one Tick did.
type CITickSummary struct {
	Candidates int // mature, unobserved runs in the lookback
	Evaluated  int // forge evaluations (≤ MaxPerTick)
	Recorded   int // run_merge_ci_observed rows that landed (terminal included)
	Deferred   int // candidates skipped while in backoff
	Failed     int // evaluations that failed
	Terminal   int // observation_failed rows that landed
}

// ciCandidate is one mature run awaiting its observation.
type ciCandidate struct {
	runID      uuid.UUID
	evidenceAt time.Time
	maturesAt  time.Time
}

// Tick runs one scan: mature, unobserved runs oldest-maturity-first, at most
// MaxPerTick forge evaluations, runs in backoff skipped without spending one.
func (t *CITicker) Tick(ctx context.Context) CITickSummary {
	var sum CITickSummary
	log := t.logger()
	now := t.now()
	observed, err := t.runsWithCategory(ctx, CategoryRunMergeCIObserved)
	if err != nil {
		log.LogAttrs(ctx, slog.LevelWarn, "merge outcome: CI tick could not list observed runs; tick skipped", slog.String("error", err.Error()))
		return sum
	}
	evidence := map[uuid.UUID]time.Time{}
	for _, cat := range mergeEvidenceCategories {
		entries, err := t.Audit.ListAll(ctx, audit.ListAllParams{Category: &cat})
		if err != nil {
			log.LogAttrs(ctx, slog.LevelWarn, "merge outcome: CI tick could not list merge evidence; tick skipped",
				slog.String("category", cat), slog.String("error", err.Error()))
			return sum
		}
		for _, e := range entries {
			if e.RunID == nil {
				continue
			}
			if at, ok := evidence[*e.RunID]; !ok || e.Timestamp.Before(at) {
				evidence[*e.RunID] = e.Timestamp
			}
		}
	}

	floor := now.Add(-t.lookback())
	var cands []ciCandidate
	for id, at := range evidence {
		if observed[id] || at.Before(floor) || t.isSkipped(id) {
			continue
		}
		matures := at.Add(t.window())
		if matures.After(now) {
			continue
		}
		cands = append(cands, ciCandidate{runID: id, evidenceAt: at, maturesAt: matures})
	}
	sort.Slice(cands, func(i, j int) bool {
		if !cands[i].maturesAt.Equal(cands[j].maturesAt) {
			return cands[i].maturesAt.Before(cands[j].maturesAt)
		}
		return cands[i].runID.String() < cands[j].runID.String()
	})
	sum.Candidates = len(cands)

	for _, c := range cands {
		if ctx.Err() != nil {
			return sum
		}
		if t.inBackoff(c.runID, now) {
			sum.Deferred++
			continue
		}
		if sum.Evaluated >= t.maxPerTick() {
			break
		}
		r, err := t.Runs.GetRun(ctx, c.runID)
		if errors.Is(err, run.ErrNotFound) {
			t.skip(ctx, c.runID, ciSkipRunNotFound)
			continue
		}
		if err != nil {
			log.LogAttrs(ctx, slog.LevelWarn, "merge outcome: CI tick could not load run",
				slog.String("run_id", c.runID.String()), slog.String("error", err.Error()))
			continue
		}
		tgt, reason := ciTargetFor(r)
		if reason != "" {
			t.skip(ctx, c.runID, reason)
			continue
		}
		sum.Evaluated++
		t.evaluate(ctx, r, tgt, c, now, &sum)
	}
	return sum
}

// runsWithCategory returns the runs carrying at least one row of category.
func (t *CITicker) runsWithCategory(ctx context.Context, category string) (map[uuid.UUID]bool, error) {
	entries, err := t.Audit.ListAll(ctx, audit.ListAllParams{Category: &category})
	if err != nil {
		return nil, err
	}
	out := map[uuid.UUID]bool{}
	for _, e := range entries {
		if e.RunID != nil {
			out[*e.RunID] = true
		}
	}
	return out, nil
}

// ciTarget is what the forge calls address for one run.
type ciTarget struct {
	scope  forge.CredentialScope
	repo   forge.RepoRef
	number int
	prURL  string
}

// ciTargetFor resolves a run's GitHub poll target, or names why it has none.
func ciTargetFor(r *run.Run) (ciTarget, string) {
	if r.PullRequestURL == nil || *r.PullRequestURL == "" {
		return ciTarget{}, ciSkipNoPRURL
	}
	if r.InstallationRef != nil && *r.InstallationRef != "" {
		if fam, _, ok := strings.Cut(*r.InstallationRef, ":"); ok && fam != "github" {
			return ciTarget{}, ciSkipNonGitHub
		}
	}
	if r.InstallationID == nil || *r.InstallationID <= 0 {
		return ciTarget{}, ciSkipNoInstallation
	}
	repo, number, ok := parseGitHubPRURL(*r.PullRequestURL)
	if !ok {
		return ciTarget{}, ciSkipMalformedPRURL
	}
	if !strings.EqualFold(repo.Owner+"/"+repo.Name, strings.Trim(r.Repo, "/")) {
		return ciTarget{}, ciSkipPRRepoMismatch
	}
	return ciTarget{
		scope:  forge.FromGitHubInstallationID(*r.InstallationID),
		repo:   repo,
		number: number,
		prURL:  *r.PullRequestURL,
	}, ""
}

// parseGitHubPRURL extracts owner/name/number from .../<owner>/<name>/pull/<n>.
func parseGitHubPRURL(raw string) (forge.RepoRef, int, bool) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Host == "" {
		return forge.RepoRef{}, 0, false
	}
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	if len(parts) != 4 || parts[2] != "pull" || parts[0] == "" || parts[1] == "" {
		return forge.RepoRef{}, 0, false
	}
	n, err := strconv.Atoi(parts[3])
	if err != nil || n <= 0 {
		return forge.RepoRef{}, 0, false
	}
	return forge.RepoRef{Owner: parts[0], Name: parts[1]}, n, true
}

// evaluate observes one candidate and records its row, or enters backoff.
func (t *CITicker) evaluate(ctx context.Context, r *run.Run, tgt ciTarget, c ciCandidate, now time.Time, sum *CITickSummary) {
	pull, err := withRetry(ctx, t.RetryBackoff, func(ctx context.Context) (*forge.PullRequest, error) {
		return t.Forge.GetPullRequest(ctx, tgt.scope, tgt.repo, tgt.number)
	})
	if err != nil {
		t.fail(ctx, r, tgt, c, nil, now, classifyCIError(ctx, err), err, sum)
		return
	}
	switch {
	case pull == nil || !pull.Merged:
		t.fail(ctx, r, tgt, c, nil, now, errClassNotMerged, errors.New("forge reports the pull request unmerged"), sum)
		return
	case pull.MergeCommitSHA == "" || pull.MergedAt == nil:
		t.fail(ctx, r, tgt, c, pull, now, errClassMergeFacts, errors.New("forge omits merge_commit_sha or merged_at"), sum)
		return
	}
	matures := pull.MergedAt.Add(t.window())
	if matures.After(now) {
		// The chain's evidence predates the forge's merged_at; wait for the
		// forge maturity without counting a failure.
		t.holdUntil(c.runID, matures)
		return
	}
	checks, truncated, err := withRetry2(ctx, t.RetryBackoff, func(ctx context.Context) ([]githubclient.CheckRunSummary, bool, error) {
		return t.Forge.ListCheckRunsForRef(ctx, tgt.scope, tgt.repo, pull.MergeCommitSHA)
	})
	if err != nil {
		t.fail(ctx, r, tgt, c, pull, now, classifyCIError(ctx, err), err, sum)
		return
	}
	v := ClassifyMergeCI(r.RequiredChecksSnapshot, checks, truncated, matures)
	landed, err := t.record(ctx, r, tgt, c, pull, matures, now, v, "")
	if err != nil {
		t.fail(ctx, r, tgt, c, pull, now, errClassAuditAppend, err, sum)
		return
	}
	t.clear(c.runID)
	if landed {
		sum.Recorded++
	}
}

// withRetry2 is withRetry for a two-result call.
func withRetry2[A, B any](ctx context.Context, base time.Duration, fn func(context.Context) (A, B, error)) (A, B, error) {
	type pair struct {
		a A
		b B
	}
	p, err := withRetry(ctx, base, func(ctx context.Context) (pair, error) {
		a, b, err := fn(ctx)
		return pair{a, b}, err
	})
	return p.a, p.b, err
}

// fail enters (or extends) the run's exponential backoff and, once the run is
// TerminalAfter past maturity, records the terminal observation_failed row.
func (t *CITicker) fail(ctx context.Context, r *run.Run, tgt ciTarget, c ciCandidate, pull *forge.PullRequest,
	now time.Time, class string, cause error, sum *CITickSummary) {
	if ctx.Err() != nil {
		return
	}
	sum.Failed++
	st := t.noteFailure(c.runID, now)
	log := t.logger()
	log.LogAttrs(ctx, slog.LevelWarn, "merge outcome: CI observation failed; backing off",
		slog.String("run_id", r.ID.String()), slog.String("pull_request_url", tgt.prURL),
		slog.String("error_class", class), slog.String("error", cause.Error()),
		slog.Int("failures", st.failures), slog.Time("next_attempt", st.nextAttempt))
	if now.Before(c.maturesAt.Add(t.terminalAfter())) {
		return
	}
	v := CIVerdict{
		Conclusion:           CIConclusionMissing,
		MissingReason:        MissingObservationFailed,
		RequiredChecks:       []string{},
		RequiredChecksSource: requiredSourceForSnapshot(r.RequiredChecksSnapshot),
		ExcludedChecks:       []string{},
		PassedChecks:         []string{},
		FailedChecks:         []string{},
		MissingChecks:        []string{},
		MissingCheckReasons:  map[string]string{},
	}
	matures := c.maturesAt
	if pull != nil && pull.MergedAt != nil {
		matures = pull.MergedAt.Add(t.window())
	}
	landed, err := t.record(ctx, r, tgt, c, pull, matures, now, v, class)
	if err != nil {
		log.LogAttrs(ctx, slog.LevelWarn, "merge outcome: terminal CI observation could not be recorded",
			slog.String("run_id", r.ID.String()), slog.String("error", err.Error()))
		return
	}
	t.clear(c.runID)
	if landed {
		sum.Recorded++
		sum.Terminal++
	}
}

func requiredSourceForSnapshot(s *run.RequiredChecksSnapshot) string {
	switch {
	case s == nil:
		return requiredSourceAbsent
	case len(s.Contexts) == 0:
		return requiredSourceEmpty
	default:
		return requiredSourceSnapshot
	}
}

// classifyCIError names a forge failure's class.
func classifyCIError(ctx context.Context, err error) string {
	switch {
	case errors.Is(err, forge.ErrNotFound):
		return errClassNotFound
	case errors.Is(err, forge.ErrForbidden):
		return errClassForbidden
	case errors.Is(err, forge.ErrNotInstalled):
		return errClassNotInstalled
	case errors.Is(err, forge.ErrValidation):
		return errClassValidation
	case isTransient(ctx, err):
		return errClassTransient
	default:
		return errClassForge
	}
}

// ciPayload is the run_merge_ci_observed allow-list: forge-attested facts and
// Fishhawk-derived enums only — no check output text, commit message or PR
// prose.
type ciPayload struct {
	Repo                  string            `json:"repo"`
	PullRequestURL        string            `json:"pull_request_url"`
	PullRequestNumber     int               `json:"pull_request_number"`
	MergeCommitSHA        string            `json:"merge_commit_sha"`
	MergedAt              *time.Time        `json:"merged_at"`
	MergeEvidenceAt       time.Time         `json:"merge_evidence_at"`
	MaturesAt             time.Time         `json:"matures_at"`
	ObservedAt            time.Time         `json:"observed_at"`
	WindowSeconds         int64             `json:"window_seconds"`
	Conclusion            string            `json:"conclusion"`
	MissingReason         string            `json:"missing_reason"`
	MissingCheckReasons   map[string]string `json:"missing_check_reasons"`
	ObservationErrorClass string            `json:"observation_error_class"`
	RequiredChecks        []string          `json:"required_checks"`
	RequiredChecksSource  string            `json:"required_checks_source"`
	ExcludedChecks        []string          `json:"excluded_checks"`
	PassedChecks          []string          `json:"passed_checks"`
	FailedChecks          []string          `json:"failed_checks"`
	MissingChecks         []string          `json:"missing_checks"`
	ChecksObserved        int               `json:"checks_observed"`
	ChecksTruncated       bool              `json:"checks_truncated"`
	Source                string            `json:"source"`
}

// record appends the run_merge_ci_observed row, deduped on merge_commit_sha.
func (t *CITicker) record(ctx context.Context, r *run.Run, tgt ciTarget, c ciCandidate, pull *forge.PullRequest,
	matures, now time.Time, v CIVerdict, errClass string) (bool, error) {
	p := ciPayload{
		Repo:                  r.Repo,
		PullRequestURL:        tgt.prURL,
		PullRequestNumber:     tgt.number,
		MergeEvidenceAt:       c.evidenceAt.UTC(),
		MaturesAt:             matures.UTC(),
		ObservedAt:            now.UTC(),
		WindowSeconds:         int64(t.window() / time.Second),
		Conclusion:            v.Conclusion,
		MissingReason:         v.MissingReason,
		MissingCheckReasons:   v.MissingCheckReasons,
		ObservationErrorClass: errClass,
		RequiredChecks:        v.RequiredChecks,
		RequiredChecksSource:  v.RequiredChecksSource,
		ExcludedChecks:        v.ExcludedChecks,
		PassedChecks:          v.PassedChecks,
		FailedChecks:          v.FailedChecks,
		MissingChecks:         v.MissingChecks,
		ChecksObserved:        v.ChecksObserved,
		ChecksTruncated:       v.ChecksTruncated,
		Source:                sourceCheckRunsPoll,
	}
	if pull != nil {
		p.MergeCommitSHA = pull.MergeCommitSHA
		if pull.MergedAt != nil {
			m := pull.MergedAt.UTC()
			p.MergedAt = &m
		}
	}
	payload, err := json.Marshal(p)
	if err != nil {
		return false, fmt.Errorf("mergeoutcome: marshal ci payload: %w", err)
	}
	return appendDeduped(ctx, t.Audit,
		systemChainParams(r.ID, CategoryRunMergeCIObserved, now, payload),
		audit.DedupeSpec{PayloadKey: "merge_commit_sha", PayloadValue: p.MergeCommitSHA})
}

// --- in-memory per-run state ------------------------------------------------

func (t *CITicker) inBackoff(id uuid.UUID, now time.Time) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	st := t.backoff[id]
	return st != nil && now.Before(st.nextAttempt)
}

func (t *CITicker) noteFailure(id uuid.UUID, now time.Time) ciBackoff {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.backoff == nil {
		t.backoff = map[uuid.UUID]*ciBackoff{}
	}
	st := t.backoff[id]
	if st == nil {
		st = &ciBackoff{}
		t.backoff[id] = st
	}
	st.failures++
	delay := t.interval()
	for i := 1; i < st.failures && delay < t.maxBackoff(); i++ {
		delay *= 2
	}
	if delay > t.maxBackoff() {
		delay = t.maxBackoff()
	}
	st.nextAttempt = now.Add(delay)
	return *st
}

// holdUntil holds a run until notBefore without counting a failure.
func (t *CITicker) holdUntil(id uuid.UUID, notBefore time.Time) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.backoff == nil {
		t.backoff = map[uuid.UUID]*ciBackoff{}
	}
	st := t.backoff[id]
	if st == nil {
		st = &ciBackoff{}
		t.backoff[id] = st
	}
	st.nextAttempt = notBefore
}

func (t *CITicker) clear(id uuid.UUID) {
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.backoff, id)
}

func (t *CITicker) isSkipped(id uuid.UUID) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.skipped[id]
}

// skip drops a run whose static attributes give it no GitHub poll target.
func (t *CITicker) skip(ctx context.Context, id uuid.UUID, reason string) {
	t.mu.Lock()
	if t.skipped == nil {
		t.skipped = map[uuid.UUID]bool{}
	}
	t.skipped[id] = true
	t.mu.Unlock()
	t.logger().LogAttrs(ctx, slog.LevelDebug, "merge outcome: run has no CI observation target; skipped",
		slog.String("run_id", id.String()), slog.String("reason", reason))
}
