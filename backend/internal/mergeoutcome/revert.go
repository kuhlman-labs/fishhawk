package mergeoutcome

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/forge"
	"github.com/kuhlman-labs/fishhawk/backend/internal/githubclient"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

// Detection kinds: which revert signal pointed at the candidate run.
const (
	DetectionRevertTrailer = "revert_trailer"
	DetectionRevertSubject = "revert_subject"
)

// Attestation classes computed from the forge's file-level diffs (operator
// condition 1). Only AttestationInverseDiff and AttestationPartialInverse may
// count as a revert OUTCOME in the beta projection; AttestationSignalOnly is a
// recorded pointer the forge diffs do not corroborate.
const (
	AttestationInverseDiff    = "inverse_diff"
	AttestationPartialInverse = "partial_inverse"
	AttestationSignalOnly     = "signal_only"
)

// Attestation reasons: the machine-readable why behind an attestation.
const (
	attestReasonAllInverted      = "all_merge_files_inverted"
	attestReasonSubsetInverted   = "subset_of_merge_files_inverted"
	attestReasonRevertEmpty      = "revert_touches_no_files"
	attestReasonFileNotInMerge   = "file_not_in_merge"
	attestReasonNotInverted      = "counts_not_inverted"
	attestReasonFilesTruncated   = "file_list_truncated"
	attestReasonFilesUnavailable = "file_list_unavailable"
)

// DefaultMaxRevertSignals caps the revert signals one push resolves
// (operator condition 2); signals past it are logged as a named truncation.
const DefaultMaxRevertSignals = 50

// runsPerPRLimit bounds the run lookup for one PR URL.
const runsPerPRLimit = 20

// sourcePushWebhook is the payload's source tag for run_merge_reverted.
const sourcePushWebhook = "github_push_webhook"

// PushEvent is the slice of a GitHub `push` delivery the revert observer
// reads. Nothing else in the payload is decoded.
type PushEvent struct {
	Ref            string
	Deleted        bool
	After          string
	FullName       string
	HTMLURL        string
	DefaultBranch  string
	InstallationID int64
	Commits        []PushCommit
}

// PushCommit is one commit of a push delivery. Message is read for revert
// POINTERS only and is never recorded.
type PushCommit struct {
	ID      string
	Message string
}

// ParsePush decodes the fields of a GitHub push payload the observer needs.
func ParsePush(raw []byte) (*PushEvent, error) {
	var body struct {
		Ref        string `json:"ref"`
		Deleted    bool   `json:"deleted"`
		After      string `json:"after"`
		Repository struct {
			FullName      string `json:"full_name"`
			HTMLURL       string `json:"html_url"`
			DefaultBranch string `json:"default_branch"`
		} `json:"repository"`
		Installation struct {
			ID int64 `json:"id"`
		} `json:"installation"`
		Commits []struct {
			ID      string `json:"id"`
			Message string `json:"message"`
		} `json:"commits"`
	}
	if err := json.Unmarshal(raw, &body); err != nil {
		return nil, fmt.Errorf("mergeoutcome: parse push: %w", err)
	}
	p := &PushEvent{
		Ref:            body.Ref,
		Deleted:        body.Deleted,
		After:          body.After,
		FullName:       body.Repository.FullName,
		HTMLURL:        strings.TrimSuffix(body.Repository.HTMLURL, "/"),
		DefaultBranch:  body.Repository.DefaultBranch,
		InstallationID: body.Installation.ID,
	}
	for _, c := range body.Commits {
		p.Commits = append(p.Commits, PushCommit{ID: c.ID, Message: c.Message})
	}
	return p, nil
}

// IsDefaultBranchPush reports whether the push advanced the repository's
// default branch. A missing default_branch or a branch delete is false (fail
// closed).
func (p *PushEvent) IsDefaultBranchPush() bool {
	if p == nil || p.Deleted || p.DefaultBranch == "" {
		return false
	}
	return p.Ref == "refs/heads/"+p.DefaultBranch
}

// repoRef splits FullName into an owner/name pair.
func (p *PushEvent) repoRef() (forge.RepoRef, bool) {
	owner, name, ok := strings.Cut(p.FullName, "/")
	if !ok || owner == "" || name == "" || strings.Contains(name, "/") {
		return forge.RepoRef{}, false
	}
	return forge.RepoRef{Owner: owner, Name: name}, true
}

// RevertSignals is what a commit message POINTS at. Neither field is ever
// sufficient on its own: each is confirmed against forge state.
type RevertSignals struct {
	// RevertedSHAs are the `This reverts commit <sha>` trailers, in order,
	// de-duplicated.
	RevertedSHAs []string
	// PRNumber is the last `(#N)` inside the quoted text of a
	// `Revert "..."` subject; 0 when absent.
	PRNumber int
}

// Empty reports whether the message carried no revert signal.
func (s RevertSignals) Empty() bool { return len(s.RevertedSHAs) == 0 && s.PRNumber == 0 }

var (
	revertTrailerRE = regexp.MustCompile(`(?m)This reverts commit ([0-9a-f]{7,40})`)
	prRefRE         = regexp.MustCompile(`\(#([0-9]+)\)`)
)

// ParseRevertSignals extracts the revert pointers from a commit message: the
// `git revert` trailer and the `Revert "... (#N)"` subject. A
// revert-of-revert subject (`Revert "Revert ...`) yields no subject signal —
// it re-lands the original change rather than reverting it — though a
// trailer it carries still names the commit it reverted.
func ParseRevertSignals(message string) RevertSignals {
	var out RevertSignals
	seen := map[string]bool{}
	for _, m := range revertTrailerRE.FindAllStringSubmatch(message, -1) {
		if !seen[m[1]] {
			seen[m[1]] = true
			out.RevertedSHAs = append(out.RevertedSHAs, m[1])
		}
	}
	subject, _, _ := strings.Cut(message, "\n")
	subject = strings.TrimSpace(subject)
	const prefix = `Revert "`
	if !strings.HasPrefix(subject, prefix) {
		return out
	}
	rest := subject[len(prefix):]
	end := strings.LastIndex(rest, `"`)
	if end < 0 {
		return out
	}
	quoted := rest[:end]
	if strings.HasPrefix(quoted, prefix) {
		return out
	}
	refs := prRefRE.FindAllStringSubmatch(quoted, -1)
	if len(refs) == 0 {
		return out
	}
	if n, err := strconv.Atoi(refs[len(refs)-1][1]); err == nil && n > 0 {
		out.PRNumber = n
	}
	return out
}

// RevertForge is the forge surface the observer calls; *githubclient.Client
// satisfies it.
type RevertForge interface {
	ListPullRequestsForCommit(ctx context.Context, scope forge.CredentialScope, repo forge.RepoRef, sha string) ([]forge.PullRequestRef, error)
	GetPullRequest(ctx context.Context, scope forge.CredentialScope, repo forge.RepoRef, number int) (*forge.PullRequest, error)
	GetCommitFiles(ctx context.Context, scope forge.CredentialScope, repo forge.RepoRef, sha string) (*githubclient.CommitFiles, error)
}

// RunFinder looks runs up by repository and pull-request URL.
type RunFinder interface {
	ListRuns(ctx context.Context, f run.ListRunsFilter) ([]*run.Run, error)
}

// RevertObserver resolves a default-branch push's revert signals to the runs
// whose merges they revert and records one run_merge_reverted row per
// (run, reverting commit).
type RevertObserver struct {
	Forge  RevertForge
	Runs   RunFinder
	Audit  AuditStore
	Logger *slog.Logger
	Now    func() time.Time
	// RetryBackoff is the first retry's wait for a transient forge error;
	// zero uses defaultRetryBackoff.
	RetryBackoff time.Duration
	// MaxSignals caps the signals resolved per push; zero uses
	// DefaultMaxRevertSignals.
	MaxSignals int
}

// ObserveSummary reports what one ObservePush did.
type ObserveSummary struct {
	Signals   int // revert signals found across the push's commits
	Resolved  int // signals actually resolved (≤ the cap)
	Truncated bool
	Recorded  int // run_merge_reverted rows that landed
	Errors    int // per-candidate failures (logged)
}

// revertSignal is one pointer to resolve.
type revertSignal struct {
	commitSHA   string
	detection   string
	revertedSHA string
	prNumber    int
}

// prCandidate is a pull request a signal points at.
type prCandidate struct {
	number int
	url    string
}

// collectSignals turns every commit's message into resolvable signals. A
// commit carrying a trailer resolves by trailer only; the subject is the
// fallback for a commit whose trailer was lost (a squash-merged revert PR).
func collectSignals(commits []PushCommit) []revertSignal {
	var out []revertSignal
	for _, c := range commits {
		if c.ID == "" {
			continue
		}
		sig := ParseRevertSignals(c.Message)
		for _, sha := range sig.RevertedSHAs {
			out = append(out, revertSignal{commitSHA: c.ID, detection: DetectionRevertTrailer, revertedSHA: sha})
		}
		if len(sig.RevertedSHAs) == 0 && sig.PRNumber > 0 {
			out = append(out, revertSignal{commitSHA: c.ID, detection: DetectionRevertSubject, prNumber: sig.PRNumber})
		}
	}
	return out
}

func (o *RevertObserver) logger() *slog.Logger {
	if o.Logger != nil {
		return o.Logger
	}
	return slog.Default()
}

func (o *RevertObserver) now() time.Time {
	if o.Now != nil {
		return o.Now()
	}
	return time.Now()
}

// ObservePush resolves and records the revert signals of one push. Commits
// with no signal cost zero forge calls. A per-candidate failure is logged and
// never aborts the remaining signals.
func (o *RevertObserver) ObservePush(ctx context.Context, push *PushEvent, deliveryID string) ObserveSummary {
	var sum ObserveSummary
	log := o.logger()
	if !push.IsDefaultBranchPush() {
		return sum
	}
	signals := collectSignals(push.Commits)
	sum.Signals = len(signals)
	if len(signals) == 0 {
		return sum
	}
	repo, ok := push.repoRef()
	if !ok || push.InstallationID <= 0 || push.HTMLURL == "" {
		log.LogAttrs(ctx, slog.LevelWarn, "merge outcome: push carries revert signals but no usable repository/installation; skipped",
			slog.String("delivery_id", deliveryID), slog.String("repo", push.FullName),
			slog.Int64("installation_id", push.InstallationID))
		return sum
	}
	limit := o.MaxSignals
	if limit <= 0 {
		limit = DefaultMaxRevertSignals
	}
	if len(signals) > limit {
		log.LogAttrs(ctx, slog.LevelWarn, "merge outcome: revert_signals_truncated",
			slog.String("event", "revert_signals_truncated"),
			slog.String("delivery_id", deliveryID), slog.String("repo", push.FullName),
			slog.Int("signals", len(signals)), slog.Int("resolved", limit),
			slog.Int("dropped", len(signals)-limit))
		signals = signals[:limit]
		sum.Truncated = true
	}
	sum.Resolved = len(signals)
	scope := forge.FromGitHubInstallationID(push.InstallationID)
	files := map[string]*githubclient.CommitFiles{}
	for _, sig := range signals {
		n, errs := o.resolveSignal(ctx, scope, repo, push, sig, deliveryID, files)
		sum.Recorded += n
		for _, err := range errs {
			sum.Errors++
			log.LogAttrs(ctx, slog.LevelWarn, "merge outcome: revert signal resolution failed",
				slog.String("delivery_id", deliveryID), slog.String("repo", push.FullName),
				slog.String("reverting_commit_sha", sig.commitSHA), slog.String("detection", sig.detection),
				slog.String("error", err.Error()))
		}
	}
	return sum
}

// resolveSignal maps one signal to candidate PRs, then runs, confirms each
// against the forge, attests it from the file diffs, and records.
func (o *RevertObserver) resolveSignal(ctx context.Context, scope forge.CredentialScope, repo forge.RepoRef,
	push *PushEvent, sig revertSignal, deliveryID string, files map[string]*githubclient.CommitFiles) (int, []error) {
	var prs []prCandidate
	switch sig.detection {
	case DetectionRevertTrailer:
		refs, err := withRetry(ctx, o.RetryBackoff, func(ctx context.Context) ([]forge.PullRequestRef, error) {
			return o.Forge.ListPullRequestsForCommit(ctx, scope, repo, sig.revertedSHA)
		})
		if err != nil {
			return 0, []error{fmt.Errorf("list pulls for %s: %w", sig.revertedSHA, err)}
		}
		for _, r := range refs {
			if r.Number > 0 && r.URL != "" {
				prs = append(prs, prCandidate{number: r.Number, url: r.URL})
			}
		}
	case DetectionRevertSubject:
		prs = append(prs, prCandidate{number: sig.prNumber, url: push.HTMLURL + "/pull/" + strconv.Itoa(sig.prNumber)})
	}

	recorded := 0
	var errs []error
	for _, pr := range prs {
		prURL := pr.url
		runs, err := o.Runs.ListRuns(ctx, run.ListRunsFilter{Repo: push.FullName, PullRequestURL: &prURL, Limit: runsPerPRLimit})
		if err != nil {
			errs = append(errs, fmt.Errorf("list runs for %s: %w", pr.url, err))
			continue
		}
		if len(runs) == 0 {
			continue
		}
		pull, err := withRetry(ctx, o.RetryBackoff, func(ctx context.Context) (*forge.PullRequest, error) {
			return o.Forge.GetPullRequest(ctx, scope, repo, pr.number)
		})
		if err != nil {
			errs = append(errs, fmt.Errorf("get pr %d: %w", pr.number, err))
			continue
		}
		if !confirmMerge(sig, pull) {
			o.logger().LogAttrs(ctx, slog.LevelDebug, "merge outcome: revert signal not confirmed by the forge",
				slog.String("delivery_id", deliveryID), slog.String("pull_request_url", pr.url),
				slog.String("detection", sig.detection))
			continue
		}
		attestation, reason := o.attest(ctx, scope, repo, sig.commitSHA, pull.MergeCommitSHA, files)
		for _, r := range runs {
			if r == nil {
				continue
			}
			payload, err := json.Marshal(revertPayload{
				Repo:              push.FullName,
				PullRequestURL:    pr.url,
				PullRequestNumber: pr.number,
				MergeCommitSHA:    pull.MergeCommitSHA,
				MergedAt:          pull.MergedAt,
				RevertingSHA:      sig.commitSHA,
				PushedRef:         push.Ref,
				PushAfterSHA:      push.After,
				DeliveryID:        deliveryID,
				Detection:         sig.detection,
				Attestation:       attestation,
				AttestationReason: reason,
				ObservedAt:        o.now().UTC(),
				Source:            sourcePushWebhook,
			})
			if err != nil {
				errs = append(errs, err)
				continue
			}
			landed, err := appendDeduped(ctx, o.Audit,
				systemChainParams(r.ID, CategoryRunMergeReverted, o.now(), payload),
				audit.DedupeSpec{PayloadKey: "reverting_commit_sha", PayloadValue: sig.commitSHA})
			if err != nil {
				errs = append(errs, fmt.Errorf("record run %s: %w", r.ID, err))
				continue
			}
			if landed {
				recorded++
			}
		}
	}
	return recorded, errs
}

// revertPayload is the run_merge_reverted allow-list. Every value is a
// forge-attested fact or a Fishhawk-derived enum; no commit message, PR body
// or title is carried.
type revertPayload struct {
	Repo              string     `json:"repo"`
	PullRequestURL    string     `json:"pull_request_url"`
	PullRequestNumber int        `json:"pull_request_number"`
	MergeCommitSHA    string     `json:"merge_commit_sha"`
	MergedAt          *time.Time `json:"merged_at"`
	RevertingSHA      string     `json:"reverting_commit_sha"`
	PushedRef         string     `json:"pushed_ref"`
	PushAfterSHA      string     `json:"push_after_sha"`
	DeliveryID        string     `json:"delivery_id"`
	Detection         string     `json:"detection"`
	Attestation       string     `json:"attestation"`
	AttestationReason string     `json:"attestation_reason"`
	ObservedAt        time.Time  `json:"observed_at"`
	Source            string     `json:"source"`
}

// confirmMerge is the forge confirmation every candidate must pass: the PR
// merged and produced a merge commit, and on the trailer path that merge
// commit IS the reverted commit (the trailer may abbreviate it).
func confirmMerge(sig revertSignal, pull *forge.PullRequest) bool {
	if pull == nil || !pull.Merged || pull.MergeCommitSHA == "" {
		return false
	}
	if sig.detection == DetectionRevertTrailer {
		return strings.HasPrefix(strings.ToLower(pull.MergeCommitSHA), strings.ToLower(sig.revertedSHA))
	}
	return true
}

// attest fetches the reverting commit's and the merge commit's file lists
// (cached per push) and classifies them. Any fetch failure is signal_only.
func (o *RevertObserver) attest(ctx context.Context, scope forge.CredentialScope, repo forge.RepoRef,
	revertSHA, mergeSHA string, cache map[string]*githubclient.CommitFiles) (string, string) {
	get := func(sha string) *githubclient.CommitFiles {
		if cf, ok := cache[sha]; ok {
			return cf
		}
		cf, err := withRetry(ctx, o.RetryBackoff, func(ctx context.Context) (*githubclient.CommitFiles, error) {
			return o.Forge.GetCommitFiles(ctx, scope, repo, sha)
		})
		if err != nil {
			o.logger().LogAttrs(ctx, slog.LevelWarn, "merge outcome: commit file list unavailable; attestation is signal_only",
				slog.String("sha", sha), slog.String("error", err.Error()))
			cf = nil
		}
		cache[sha] = cf
		return cf
	}
	return ClassifyAttestation(get(revertSHA), get(mergeSHA))
}

// ClassifyAttestation compares a reverting commit's file-level diff with the
// merge commit's (operator condition 1):
//
//   - inverse_diff: every file the revert touches is in the merge commit, each
//     with its additions/deletions swapped, and every merge file is covered;
//   - partial_inverse: the same, over a strict subset of the merge's files;
//   - signal_only: anything else, including an unavailable or truncated file
//     list on either side and a revert touching no files.
//
// A rename is matched in either direction (the revert renames back).
func ClassifyAttestation(revert, merge *githubclient.CommitFiles) (string, string) {
	if revert == nil || merge == nil {
		return AttestationSignalOnly, attestReasonFilesUnavailable
	}
	if revert.Truncated || merge.Truncated {
		return AttestationSignalOnly, attestReasonFilesTruncated
	}
	if len(revert.Files) == 0 {
		return AttestationSignalOnly, attestReasonRevertEmpty
	}
	covered := map[int]bool{}
	for _, rf := range revert.Files {
		idx := -1
		for i, mf := range merge.Files {
			if covered[i] {
				continue
			}
			if rf.Path == mf.Path || (rf.PreviousPath != "" && rf.PreviousPath == mf.Path && rf.Path == mf.PreviousPath) {
				idx = i
				break
			}
		}
		if idx < 0 {
			return AttestationSignalOnly, attestReasonFileNotInMerge
		}
		mf := merge.Files[idx]
		if rf.Additions != mf.Deletions || rf.Deletions != mf.Additions {
			return AttestationSignalOnly, attestReasonNotInverted
		}
		covered[idx] = true
	}
	if len(covered) == len(merge.Files) {
		return AttestationInverseDiff, attestReasonAllInverted
	}
	return AttestationPartialInverse, attestReasonSubsetInverted
}
