package server

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
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
//     unwired, or NEITHER cfg.PRStateReader nor cfg.GitHub is wired. A verb that
//     records forge evidence must never record evidence it could not read;
//  3. 404 run_not_found;
//  4. 409 record_merge_observation_no_pull_request — the run carries no
//     PullRequestURL, so it never reached a PR and there is nothing to observe;
//  5. 400 record_merge_observation_malformed_pr_url — the URL does not resolve
//     to a (repo, number) pair;
//     Rung 5b, 409 record_merge_observation_pr_url_repo_mismatch: the URL
//     resolves, but it does not name THIS run's repository on THIS run's
//     forge. See the rung comment below — without it the verb can confirm a
//     different pull request and then record a row asserting the run's own URL
//     was observed merged;
//  6. IDEMPOTENT 200 with already_recorded:true, appending NOTHING, when the
//     chain ALREADY carries pr_merged / post_merge_observed /
//     merge_observation_recorded. A chain-read failure here is a 500 and never a
//     write — fail closed on unknown evidence;
//  7. 502 record_merge_observation_forge_unavailable — GetPullRequest errored;
//  8. 409 record_merge_observation_pr_not_merged — the forge answers NOT merged.
//     This is the guard that stops the verb manufacturing evidence for a change
//     that never shipped: without it an operator could mint the row that lets
//     reconcile-merge stamp `succeeded` on an unmerged run;
//  9. 409 record_merge_observation_no_merge_commit — the forge answers merged but
//     reports an EMPTY merge_commit_sha. Refuse rather than record an
//     observation carrying no commit. This is also what gives a GitLab run a
//     truthful named refusal: that adapter leaves both new forge fields zero
//     until its deferred half lands;
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
	// Rung 2. The reader seam falls back to the production GitHub client; when
	// BOTH are nil there is no forge to read, so refuse rather than degrade.
	reader := s.prStateReader()
	if s.cfg.RunRepo == nil || s.cfg.AuditRepo == nil || reader == nil {
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
	// Rung 5. Both halves of the resolution must succeed: an unparseable repo
	// and an unparseable PR number are the same defect from the caller's side.
	repo, rerr := parseRepoOwnerName(runRow.Repo)
	prNumber := parsePRNumberFromURL(runRow.PullRequestURL)
	if rerr != nil || prNumber <= 0 {
		s.writeError(w, r, http.StatusBadRequest, "record_merge_observation_malformed_pr_url",
			"could not resolve the run's repository and pull request number from its recorded pull request URL",
			map[string]any{
				"run_id":           runID.String(),
				"repo":             runRow.Repo,
				"pull_request_url": prURL,
			})
		return
	}
	// Rung 5b. The forge read is scoped by runRow.Repo, but the pull request
	// NUMBER is scraped from runRow.PullRequestURL — and that URL is what the
	// appended row records as the pull request observed merged. Nothing so far
	// has checked that the two agree. If the URL names a DIFFERENT repository
	// (or a different forge), the handler confirms somebody else's pull request
	// and then writes a trusted row asserting that THIS url was observed
	// merged, which reconcile-merge reads to settle the run. That is precisely
	// the evidence-that-proves-something-else failure this verb exists to
	// prevent, sitting inside the verb.
	//
	// Refuse BEFORE the forge read, so a mismatch also costs no forge request
	// and — like every other rung — leaves ZERO rows.
	//
	// Three trigger conditions, one rung, because they are one defect from the
	// caller's side (the run's recorded URL is not this run's pull request):
	//
	//	a. the URL carries no resolvable {owner}/{name} ahead of its /pull/
	//	   segment (no host, or the wrong path shape);
	//	b. that pair does not equal runRow.Repo. Compared case-INSENSITIVELY:
	//	   GitHub owner/repo names are case-preserving but case-insensitive, so
	//	   a case-differing spelling is the SAME repository and normalising is
	//	   the right move rather than loosening the check;
	//	c. the run's persisted credential ref names a NON-GitHub forge. Getting
	//	   this far means the URL is a GitHub-style /pull/ URL (rung 5 resolves
	//	   the number from that segment alone), and a run authenticating against
	//	   another forge cannot own a pull request there.
	//
	// Status is 409 rather than rung 5's 400: rung 5 refuses a MALFORMED value,
	// while this refuses two well-formed recorded facts that CONFLICT.
	urlRepo, urlOK := parsePRURLRepo(prURL)
	if !urlOK || !strings.EqualFold(urlRepo.Owner, repo.Owner) ||
		!strings.EqualFold(urlRepo.Name, repo.Name) ||
		refNamesNonGitHubForge(runRow.InstallationRef) {
		s.writeError(w, r, http.StatusConflict, "record_merge_observation_pr_url_repo_mismatch",
			"the run's recorded pull request URL does not name this run's repository on this run's forge; refusing to confirm a pull request that is not this run's",
			map[string]any{
				"run_id":           runID.String(),
				"repo":             runRow.Repo,
				"pull_request_url": prURL,
				"url_repo":         urlRepo.String(),
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
	// Rung 7.
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
			"the forge reports this pull request merged but carries no merge commit SHA; refusing to record an observation with no commit (the GitLab adapter does not populate it yet)",
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

// prStateReader resolves the forge PR reader: the injected test seam when set,
// otherwise the production GitHub client. Returns nil when NEITHER is wired, so
// the caller's 503 rung fires instead of a nil-interface panic.
//
// The explicit nil check on cfg.GitHub is load-bearing: a nil *githubclient.Client
// assigned into an interface variable is a NON-nil interface holding a nil
// pointer, so returning it unguarded would defeat the caller's `reader == nil`
// test and panic on the first method call.
func (s *Server) prStateReader() PullRequestStateReader {
	if s.cfg.PRStateReader != nil {
		return s.cfg.PRStateReader
	}
	if s.cfg.GitHub != nil {
		return s.cfg.GitHub
	}
	return nil
}

// parsePRURLRepo extracts the {owner}/{name} pair a GitHub-style pull request
// URL names — the repository the URL CLAIMS, as opposed to runRow.Repo, which
// is the repository the forge read is actually scoped to. Rung 5b compares the
// two.
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

// refNamesNonGitHubForge reports whether a run's persisted credential ref names
// a forge OTHER than GitHub. The canonical GitHub ref is the installation id's
// bare base-10 decimal (forge.FromGitHubInstallationID); a non-GitHub ref is
// "<forge>:<id>" (webhook/gitlab.go's "gitlab:<project_id>"). nil and the
// recorded-as-empty pointer are BOTH "no ref recorded" — the pre-0076 GitHub
// shape, per Run.InstallationRef's three-state contract — and neither is a
// non-GitHub forge, so neither refuses here.
func refNamesNonGitHubForge(ref *string) bool {
	return ref != nil && strings.Contains(*ref, ":")
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
