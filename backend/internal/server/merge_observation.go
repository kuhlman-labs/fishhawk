package server

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"reflect"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/forge"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

// CategoryMergeObservationRecorded is the audit-log category for the chained
// entry the merge-observation recovery verb writes (E64.32 / #3136). It is the
// OBSERVE half of the #3083 recovery pair: one verb observes the forge, a
// DIFFERENT verb (reconcile-merge) settles the run by reading only the chain.
//
// It is DELIBERATELY DISTINCT from CategoryPRMerged rather than a synthetic
// pr_merged row. pr_merged carries a LIVE-observation timestamp that the
// latency (latency.go) and cost (cost.go) surfaces already read as "when
// Fishhawk knew the PR merged"; back-dating one to the forge's merge time
// would corrupt those series and lie about HOW the merge was learned. This
// category records both timestamps — the forge's merged_at and this
// observation's observed_at — plus reconciled_after_the_fact:true, so a reader
// sees the gap without anything being back-dated.
//
// Open-set string — audit_entries.category has no CHECK, so it needs no
// migration; it IS registered in audit.KnownCategories so an operator can arm
// fishhawk_await_audit on it. Internal fact-record kind projected through the
// audit chain — NOT a new issue-comment surface
// (docs/issue-comment-surfaces.md).
const CategoryMergeObservationRecorded = "merge_observation_recorded"

// mergeObservation is the fact this verb records: the forge's own answer, plus
// when Fishhawk learned it. Empty (all zero) on an already_recorded no-op,
// because that call appended nothing and must not claim it did.
type mergeObservation struct {
	PullRequestURL    string `json:"pull_request_url"`
	PullRequestNumber int    `json:"pull_request_number"`
	MergeCommitSHA    string `json:"merge_commit_sha"`
	// MergedAt is the FORGE's merge timestamp — when the merge happened.
	MergedAt string `json:"merged_at"`
	// ObservedAt is when Fishhawk read it — when Fishhawk learned it.
	ObservedAt string `json:"observed_at"`
}

// recordMergeObservationResponse reports what the observe verb did. Recorded is
// the entry this call appended; AlreadyRecorded:true means the chain already
// carried qualifying merge evidence and this call appended NOTHING.
type recordMergeObservationResponse struct {
	RunID           string           `json:"run_id"`
	AlreadyRecorded bool             `json:"already_recorded"`
	Observation     mergeObservation `json:"observation"`
}

// handleRecordMergeObservation implements
// POST /v0/runs/{run_id}/record-merge-observation (E64.32 / #3136) — the
// missing OBSERVE half of the #3083 recovery pair.
//
// The problem it closes: reconcile-merge's evidence gate reads the run's audit
// CHAIN and never the forge, so a run whose PR genuinely merged but whose merge
// was never recorded (a webhook that never arrived, an observation lost when
// fishhawkd restarted mid-write) is unreconcilable — the evidence it needs can
// never appear. This verb is the only new way onto that chain, and it requires
// a LIVE merged=true answer from the forge to use it.
//
// The split is load-bearing. This verb OBSERVES and records a fact; it settles
// nothing, transitions no stage and completes no run. reconcile-merge SETTLES
// and still reads only the chain. So the fail-closed posture #3083 established
// is preserved exactly: evidence is still REQUIRED, and the settling verb still
// never re-observes.
//
// Refusals, ALL evaluated BEFORE any write, so a refused call leaves ZERO rows:
//
//  1. 400 validation_failed — a non-UUID run_id;
//  2. 503 record_merge_observation_unconfigured — the run/audit repositories are
//     unwired (the forge reader is resolved later, at rung 7);
//  3. 404 run_not_found;
//  4. 409 record_merge_observation_no_pull_request — the run carries no
//     PullRequestURL, so it never reached a PR and there is nothing to observe;
//  5. 400 record_merge_observation_malformed_pr_url — the recorded URL is
//     structurally unresolvable: it names no (repo, number) pair under ANY
//     forge shape, or the run's own repo field is not owner/name;
//     Rung 5b, 409 record_merge_observation_pr_url_repo_mismatch: the URL
//     resolves, but it does not name THIS run's repository ON THIS RUN'S FORGE
//     FAMILY — it names another repository, or a DIFFERENT forge family than
//     the run authenticates against (a github-ref run presenting a GitLab
//     merge-request URL, or vice versa, or an unimplemented forge). Without it
//     the verb can confirm a different pull request and then record a row
//     asserting the run's own URL was observed merged. See
//     resolveObservationTarget below for the family-aware resolution;
//  6. IDEMPOTENT 200 with already_recorded:true, appending NOTHING, when the
//     chain ALREADY carries pr_merged / post_merge_observed /
//     merge_observation_recorded. A chain-read failure here is a 500 and never a
//     write — fail closed on unknown evidence;
//  7. 503 record_merge_observation_unconfigured — the per-forge reader could not
//     be resolved (a github-family run with no cfg.GitHub, or a non-github run
//     whose ForgeResolver errored or returned nil). A verb that records forge
//     evidence must never record evidence it could not read;
//     502 record_merge_observation_forge_unavailable — GetPullRequest errored;
//  8. 409 record_merge_observation_pr_not_merged — the forge answers NOT merged.
//     This is the guard that stops the verb manufacturing evidence for a change
//     that never shipped: without it an operator could mint the row that lets
//     reconcile-merge stamp `succeeded` on an unmerged run;
//  9. 409 record_merge_observation_no_merge_commit — the forge answers merged but
//     reports an EMPTY merge_commit_sha. Refuse rather than record an
//     observation carrying no commit — a generic partial-evidence guard on
//     BOTH forge families;
//  10. 409 record_merge_observation_no_merge_timestamp — the forge answers merged
//     with a SHA but a nil merged_at (binding approval condition 2). The summary,
//     the payload and the acceptance criterion all promise the forge's REAL merge
//     time; a row with a zero timestamp would claim evidence it does not carry,
//     which defeats the entire purpose of this verb. Refuse the partial fact.
//
// On success it appends ONE chained merge_observation_recorded entry and
// returns 200.
//
// IDEMPOTENCE — the honest claim (binding approval condition 3). The guard at
// rung 6 is a READ-THEN-APPEND and is therefore NOT atomic: two CONCURRENT
// posts can both read an empty chain and both append. This code does NOT
// serialize them, and the claim is correspondingly weak: a SEQUENTIAL repeat
// POST appends nothing (the test pins that), while a concurrent duplicate is
// POSSIBLE and HARMLESS. Harmless because the evidence gate
// (runPRObservablyMerged) asks only whether AT LEAST ONE qualifying row exists —
// a second row is inert, changes no decision, and the chain is append-only
// history where two observations of the same true fact is redundancy, not
// corruption. The stronger mechanism (the supersedeRepairMu shape) was
// deliberately NOT adopted: it would buy nothing a caller can observe, and the
// weak-mechanism/strong-claim mismatch is the shape being removed, not the
// duplicate row.
func (s *Server) handleRecordMergeObservation(w http.ResponseWriter, r *http.Request) {
	// Rung 1.
	runID, err := uuid.Parse(r.PathValue("run_id"))
	if err != nil {
		s.writeError(w, r, http.StatusBadRequest, "validation_failed",
			"run_id must be a valid UUID",
			map[string]any{"field": "run_id", "got": r.PathValue("run_id")})
		return
	}
	// Rung 2. The per-forge reader is resolved at rung 7 (it depends on the run's
	// forge family, which needs the run row first); here we only require the
	// repositories.
	if s.cfg.RunRepo == nil || s.cfg.AuditRepo == nil {
		s.writeError(w, r, http.StatusServiceUnavailable, "record_merge_observation_unconfigured",
			"recording a merge observation requires run + audit repositories and a forge pull-request reader", nil)
		return
	}
	// Rung 3.
	runRow, gerr := s.cfg.RunRepo.GetRun(r.Context(), runID)
	if gerr != nil {
		if errors.Is(gerr, run.ErrNotFound) {
			s.writeError(w, r, http.StatusNotFound, "run_not_found",
				"no run with that id", map[string]any{"run_id": runID.String()})
			return
		}
		s.writeError(w, r, http.StatusInternalServerError, "internal_error",
			"get run failed", map[string]any{"error": gerr.Error()})
		return
	}
	// Rung 4.
	if runRow.PullRequestURL == nil || *runRow.PullRequestURL == "" {
		s.writeError(w, r, http.StatusConflict, "record_merge_observation_no_pull_request",
			"this run carries no pull request URL, so there is no merge to observe",
			map[string]any{"run_id": runID.String()})
		return
	}
	prURL := *runRow.PullRequestURL
	// Rungs 5 / 5b. Resolve the observation target with the forge-FAMILY-aware
	// resolver: it derives the run's forge id from its InstallationRef, requires
	// the recorded URL's shape to match that family, and requires the URL's
	// project path to equal the run's repo case-insensitively. A malformed value
	// is rung 5's 400; a well-formed value naming another repository or another
	// forge family is rung 5b's 409. Both refuse BEFORE the forge read, so a
	// refusal costs no forge request and — like every other rung — leaves ZERO
	// rows.
	forgeID, repo, prNumber, reason := resolveObservationTarget(runRow)
	switch reason {
	case obsTargetMalformed:
		s.writeError(w, r, http.StatusBadRequest, "record_merge_observation_malformed_pr_url",
			"could not resolve the run's repository and pull request number from its recorded pull request URL",
			map[string]any{
				"run_id":           runID.String(),
				"repo":             runRow.Repo,
				"pull_request_url": prURL,
			})
		return
	case obsTargetMismatch:
		// Rung 5b — the routed security guard. The forge read is scoped by the
		// run's repo/forge, but the pull request NUMBER is scraped from the
		// recorded URL, and that URL is what the appended row records as observed
		// merged. If the URL names a DIFFERENT repository, or a DIFFERENT forge
		// family than the run authenticates against, the handler would confirm
		// somebody else's pull request and then write a trusted row asserting
		// THIS url was observed merged — the evidence-that-proves-something-else
		// failure this verb exists to prevent. Status is 409 (two well-formed
		// recorded facts that CONFLICT), not rung 5's 400 (a malformed value).
		s.writeError(w, r, http.StatusConflict, "record_merge_observation_pr_url_repo_mismatch",
			"the run's recorded pull request URL does not name this run's repository on this run's forge family; refusing to confirm a pull request that is not this run's",
			map[string]any{
				"run_id":           runID.String(),
				"repo":             runRow.Repo,
				"forge":            forgeID,
				"pull_request_url": prURL,
			})
		return
	}
	// Rung 6. Fail CLOSED on an unreadable chain: a verb whose whole job is to
	// decide whether evidence is MISSING must never treat "unknown" as "absent"
	// and write on top of it.
	already, merr := s.runPRObservablyMerged(r.Context(), runID)
	if merr != nil {
		s.writeError(w, r, http.StatusInternalServerError, "internal_error",
			"read merge observation failed", map[string]any{"error": merr.Error()})
		return
	}
	if already {
		s.writeJSON(w, r, http.StatusOK, recordMergeObservationResponse{
			RunID:           runID.String(),
			AlreadyRecorded: true,
		})
		return
	}
	// Rung 7. Resolve the per-forge reader now that the forge family is known.
	// A github-family run uses cfg.GitHub (unless the test seam overrides it);
	// any other forge is resolved through ForgeResolver. A nil reader is a 503,
	// never a nil dispatch.
	reader, readerErr := s.prStateReaderFor(forgeID)
	if readerErr != nil || reader == nil {
		s.writeError(w, r, http.StatusServiceUnavailable, "record_merge_observation_unconfigured",
			"recording a merge observation requires run + audit repositories and a forge pull-request reader", nil)
		return
	}
	pr, perr := reader.GetPullRequest(r.Context(), mergeObservationScope(runRow), repo, prNumber)
	if perr != nil {
		s.writeError(w, r, http.StatusBadGateway, "record_merge_observation_forge_unavailable",
			"could not read the pull request from the forge; the merge state is unknown and nothing was recorded",
			map[string]any{"run_id": runID.String(), "pull_request_number": prNumber, "error": perr.Error()})
		return
	}
	// Rung 8. A nil PR is the same unknown as an error — never a merge.
	if pr == nil || !pr.Merged {
		s.writeError(w, r, http.StatusConflict, "record_merge_observation_pr_not_merged",
			"the forge reports this pull request is not merged; recording an observation would manufacture evidence for a change that never shipped",
			map[string]any{"run_id": runID.String(), "pull_request_number": prNumber})
		return
	}
	// Rung 9.
	if pr.MergeCommitSHA == "" {
		s.writeError(w, r, http.StatusConflict, "record_merge_observation_no_merge_commit",
			"the forge reports this pull request merged but carries no merge commit SHA; refusing to record an observation with no commit",
			map[string]any{"run_id": runID.String(), "pull_request_number": prNumber})
		return
	}
	// Rung 10 (binding approval condition 2).
	if pr.MergedAt == nil {
		s.writeError(w, r, http.StatusConflict, "record_merge_observation_no_merge_timestamp",
			"the forge reports this pull request merged but carries no merge timestamp; refusing to record a partial observation that would claim a merge time it does not have",
			map[string]any{"run_id": runID.String(), "pull_request_number": prNumber})
		return
	}

	observedAt := time.Now().UTC()
	mergedAt := pr.MergedAt.UTC()
	// An operator-invoked observation, so actor_kind is user (or agent for an
	// operator-role token) with the authenticated subject — a second signal
	// alongside the category that this fact was learned by hand, not seen live
	// by the webhook path.
	subject := IdentityFrom(r.Context()).Subject
	if subject == "" {
		subject = "anonymous"
	}
	actorKind := actorKindForSubject(subject)
	payload, _ := json.Marshal(map[string]any{
		"run_id":              runID.String(),
		"pull_request_url":    prURL,
		"pull_request_number": prNumber,
		"merge_commit_sha":    pr.MergeCommitSHA,
		// Both timestamps, deliberately. merged_at is WHEN THE MERGE HAPPENED
		// (the forge's own value); observed_at is WHEN FISHHAWK LEARNED IT. The
		// gap between them is the fact this category exists to make readable.
		"merged_at":                 mergedAt.Format(time.RFC3339Nano),
		"observed_at":               observedAt.Format(time.RFC3339Nano),
		"reconciled_after_the_fact": true,
	})
	if _, aerr := s.cfg.AuditRepo.AppendChained(r.Context(), audit.ChainAppendParams{
		RunID:        runID,
		Timestamp:    observedAt,
		Category:     CategoryMergeObservationRecorded,
		ActorKind:    &actorKind,
		ActorSubject: &subject,
		Payload:      payload,
	}); aerr != nil {
		s.writeError(w, r, http.StatusInternalServerError, "internal_error",
			"append merge observation failed", map[string]any{"error": aerr.Error()})
		return
	}

	s.writeJSON(w, r, http.StatusOK, recordMergeObservationResponse{
		RunID: runID.String(),
		Observation: mergeObservation{
			PullRequestURL:    prURL,
			PullRequestNumber: prNumber,
			MergeCommitSHA:    pr.MergeCommitSHA,
			MergedAt:          mergedAt.Format(time.RFC3339Nano),
			ObservedAt:        observedAt.Format(time.RFC3339Nano),
		},
	})
}

// prStateReaderFor resolves the forge PR reader for a run's forge FAMILY
// (E64.40 / #3151). The fallback ladder is unambiguous (binding approval
// condition 2):
//   - cfg.PRStateReader, when set, OVERRIDES everything for every family — the
//     test seam wins.
//   - a github-family run resolves ONLY through cfg.GitHub. It NEVER falls
//     through to ForgeResolver or the process registry, so registry
//     availability can never change a GitHub outcome. The explicit nil check is
//     load-bearing: a nil *githubclient.Client assigned into an interface is a
//     NON-nil interface holding a nil pointer.
//   - any other forge family resolves through cfg.ForgeResolver (defaulting to
//     forge.Get). A resolver error or a nil forge yields (nil, …), which the
//     caller turns into the 503 rung. "nil forge" here means BOTH a nil
//     interface AND a typed-nil pointer wrapped in a non-nil interface (a
//     resolver returning e.g. (*someForge)(nil) inside a non-nil forge.Forge):
//     a bare `f == nil` catches only the former and would then panic on the
//     first GetPullRequest dispatch, so isNilForge guards both — the analogue
//     of the load-bearing explicit nil check on the concrete cfg.GitHub above.
//
// It returns (nil, nil) rather than panicking when no reader is wired, so the
// caller's 503 fires instead of a nil-interface dispatch.
func (s *Server) prStateReaderFor(forgeID string) (PullRequestStateReader, error) {
	if s.cfg.PRStateReader != nil {
		return s.cfg.PRStateReader, nil
	}
	if forgeID == observationForgeGitHub {
		if s.cfg.GitHub != nil {
			return s.cfg.GitHub, nil
		}
		return nil, nil
	}
	resolver := s.cfg.ForgeResolver
	if resolver == nil {
		resolver = forge.Get
	}
	f, err := resolver(forgeID)
	if err != nil {
		return nil, err
	}
	if isNilForge(f) {
		return nil, nil
	}
	return f, nil
}

// isNilForge reports whether f is effectively nil — a nil interface OR a
// non-nil interface wrapping a typed-nil pointer. A ForgeResolver that
// returns a typed nil (e.g. (*someForge)(nil)) passes a bare `f == nil`
// check yet panics on the first method dispatch, so prStateReaderFor guards
// both, mirroring the concrete nil check on cfg.GitHub.
func isNilForge(f forge.Forge) bool {
	if f == nil {
		return true
	}
	v := reflect.ValueOf(f)
	switch v.Kind() {
	case reflect.Pointer, reflect.Interface, reflect.Map, reflect.Slice, reflect.Func, reflect.Chan:
		return v.IsNil()
	default:
		return false
	}
}

// obsTargetReason discriminates how resolveObservationTarget classified a run's
// recorded pull request URL, so the handler picks rung 5's 400 (malformed) from
// rung 5b's 409 (mismatch) rather than collapsing the two.
type obsTargetReason int

const (
	obsTargetOK obsTargetReason = iota
	obsTargetMalformed
	obsTargetMismatch
)

const (
	observationForgeGitHub = "github"
	observationForgeGitLab = "gitlab"
)

// resolveObservationTarget derives the forge family, repository, and pull
// request number the merge-observation verb should read, plus a discriminator
// for the handler's rung selection (E64.40 / #3151). It replaces the earlier
// GitHub-only rung-5/5b resolution with a forge-FAMILY-aware one so a GitLab
// run reaches the forge read the earlier code refused before it — WITHOUT
// weakening the guard: the URL shape must match the run's forge family and the
// URL must name the run's own repository, so a github-ref run can never present
// a GitLab URL (or vice versa) and every unimplemented forge still fails closed.
//
// The resolution, all fail-closed:
//   - forgeID comes from runRow.InstallationRef: nil / empty / a bare decimal
//     (no ':') is the pre-0076 GitHub shape; "<forge>:<id>" names that forge.
//   - the run's own Repo must be owner/name (malformed otherwise).
//   - the recorded URL is classified family-AGNOSTICALLY: a /pull/<n> segment
//     is github, a /-/merge_requests/<n> (canonical) or /merge_requests/<n>
//     (legacy, binding approval condition 1) segment is gitlab. A URL matching
//     no shape is malformed.
//   - the URL's OWN family must equal the run's forge family, and the URL must
//     name the run's repository (case-insensitively — owner/project names are
//     case-preserving but case-insensitive on both forges) — otherwise it is a
//     mismatch. ANY forge id other than github/gitlab has no recognised URL
//     shape, so its runs land in mismatch: an unimplemented forge must not
//     reach the forge read.
//
// A GitLab subgroup path (group/sub/project) is a known residual, not a
// regression: run.Run.Repo is owner/name everywhere in the backend, so such a
// project cannot be represented on a run row and is unreachable by this verb
// regardless of what the URL parser tolerates.
func resolveObservationTarget(runRow *run.Run) (forgeID string, repo forge.RepoRef, number int, reason obsTargetReason) {
	forgeID = observationForgeID(runRow.InstallationRef)

	repoRef, rerr := parseRepoOwnerName(runRow.Repo)
	if rerr != nil {
		return forgeID, forge.RepoRef{}, 0, obsTargetMalformed
	}
	repo = repoRef

	prURL := ""
	if runRow.PullRequestURL != nil {
		prURL = *runRow.PullRequestURL
	}
	urlFamily, host, projectPath, num, resolved := classifyPRURL(prURL)
	if !resolved {
		return forgeID, repo, 0, obsTargetMalformed
	}
	// The URL's OWN family must equal the run's forge family. This is the sole
	// unknown-forge guard too: classifyPRURL only ever yields github/gitlab, so
	// ANY other forgeID (an unimplemented forge) has urlFamily != forgeID and
	// fails closed here — an unimplemented forge never reaches the switch below,
	// which is why the switch needs no default arm.
	if urlFamily != forgeID {
		return forgeID, repo, num, obsTargetMismatch
	}
	switch forgeID {
	case observationForgeGitHub:
		// Reuse the strict GitHub repo parser: it requires a host and exactly
		// two non-empty segments ahead of /pull/, so a nested path or a
		// host-less URL is a mismatch rather than a false match.
		urlRepo, ok := parsePRURLRepo(prURL)
		if !ok || !strings.EqualFold(urlRepo.Owner, repo.Owner) ||
			!strings.EqualFold(urlRepo.Name, repo.Name) {
			return forgeID, repo, num, obsTargetMismatch
		}
	case observationForgeGitLab:
		if host == "" || !strings.EqualFold(projectPath, runRow.Repo) {
			return forgeID, repo, num, obsTargetMismatch
		}
	}
	return forgeID, repo, num, obsTargetOK
}

// observationForgeID derives a run's forge family id from its persisted
// credential ref. nil / empty / a bare decimal (no ':') is the pre-0076 GitHub
// shape (forge.FromGitHubInstallationID); "<forge>:<id>" names that forge
// (webhook/gitlab.go's "gitlab:<project_id>").
func observationForgeID(ref *string) string {
	if ref == nil || *ref == "" {
		return observationForgeGitHub
	}
	if i := strings.Index(*ref, ":"); i >= 0 {
		return (*ref)[:i]
	}
	return observationForgeGitHub
}

// classifyPRURL classifies a recorded pull request URL family-AGNOSTICALLY,
// returning the forge family its SHAPE implies, its host, the project path
// ahead of the identifying segment, and the pull/merge request number. resolved
// is false only when the URL parses to no recognised (project, number) shape at
// all — the caller reads that as MALFORMED, while a shape that resolves under a
// DIFFERENT family than the run's is a MISMATCH.
//
// The gitlab CANONICAL /-/merge_requests/ shape is checked BEFORE the legacy
// /merge_requests/ shape, so a canonical URL is never mis-split on the
// /merge_requests/ substring its own /-/ separator contains.
func classifyPRURL(prURL string) (family, host, projectPath string, number int, resolved bool) {
	u, err := url.Parse(prURL)
	if err != nil {
		return "", "", "", 0, false
	}
	host = u.Host
	path := u.Path
	if idx := strings.LastIndex(path, "/-/merge_requests/"); idx >= 0 {
		if n := trailingPRNumber(path[idx+len("/-/merge_requests/"):]); n > 0 {
			return observationForgeGitLab, host, strings.Trim(path[:idx], "/"), n, true
		}
	}
	if idx := strings.LastIndex(path, "/pull/"); idx >= 0 {
		if n := trailingPRNumber(path[idx+len("/pull/"):]); n > 0 {
			return observationForgeGitHub, host, strings.Trim(path[:idx], "/"), n, true
		}
	}
	if idx := strings.LastIndex(path, "/merge_requests/"); idx >= 0 {
		if n := trailingPRNumber(path[idx+len("/merge_requests/"):]); n > 0 {
			return observationForgeGitLab, host, strings.Trim(path[:idx], "/"), n, true
		}
	}
	return "", host, "", 0, false
}

// trailingPRNumber parses the positive integer at the head of tail, trimming
// any trailing path/query/fragment (e.g. "/files", "#note"). Returns 0 when
// tail carries no positive integer, mirroring parsePRNumberFromURL's contract.
func trailingPRNumber(tail string) int {
	if cut := strings.IndexAny(tail, "/?#"); cut >= 0 {
		tail = tail[:cut]
	}
	n, err := strconv.Atoi(tail)
	if err != nil || n <= 0 {
		return 0
	}
	return n
}

// parsePRURLRepo extracts the {owner}/{name} pair a GitHub-style pull request
// URL names — the repository the URL CLAIMS, as opposed to runRow.Repo, which
// is the repository the forge read is actually scoped to. resolveObservationTarget's
// github branch compares the two.
//
// It deliberately does NOT reuse parsePRNumberFromURL's substring scan: that
// one answers "is there a trailing number" and is free to fail open, while this
// one feeds a refusal and must be strict about SHAPE. So the URL is parsed as a
// URL (a host is required) and the path ahead of /pull/ must be exactly two
// non-empty segments. Returns ok=false on anything else; the caller treats that
// as a mismatch rather than trying to interpret it.
func parsePRURLRepo(prURL string) (forge.RepoRef, bool) {
	u, err := url.Parse(prURL)
	if err != nil || u.Host == "" {
		return forge.RepoRef{}, false
	}
	idx := strings.LastIndex(u.Path, "/pull/")
	if idx < 0 {
		return forge.RepoRef{}, false
	}
	segs := strings.Split(strings.Trim(u.Path[:idx], "/"), "/")
	if len(segs) != 2 || segs[0] == "" || segs[1] == "" {
		return forge.RepoRef{}, false
	}
	return forge.RepoRef{Owner: segs[0], Name: segs[1]}, true
}

// mergeObservationScope resolves the forge credential scope for the run's PR
// read, preferring the ADR-057 / ADR-058 forge-neutral InstallationRef and
// falling back to the GitHub installation id — the same ladder the sibling
// forge reads in this package use. A run carrying neither yields the zero
// scope, which the reader rejects; that surfaces as the forge-unavailable rung
// rather than a silent unauthenticated read.
func mergeObservationScope(runRow *run.Run) forge.CredentialScope {
	if runRow.InstallationRef != nil && *runRow.InstallationRef != "" {
		return forge.FromRef(*runRow.InstallationRef)
	}
	if runRow.InstallationID != nil && *runRow.InstallationID != 0 {
		return forge.FromGitHubInstallationID(*runRow.InstallationID)
	}
	return forge.CredentialScope{}
}
