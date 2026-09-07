package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/forge"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

// CategoryOperatorCommitVouched is the audit-log category for the entry
// the vouch handler writes when an operator declares a foreign commit on
// a run branch to be run-authored lineage (ADR-035 remediation, #1044).
// It is a durable, operator-authored declaration: the payload names the
// run, the vouched SHA, and the operator's reason. The reported-head
// ledger (lineage.go) unions vouched SHAs alongside the run's own
// pull_request_opened / child_pushed / fixup_pushed provenance, so an
// operator's mechanical remediation commit no longer wedges the run it
// fixed. Internal audit kind — NOT an issue-comment surface (the living
// anchor comment #1067 projects it via the audit chain).
const CategoryOperatorCommitVouched = "operator_commit_vouched"

// lineageVouchedSHAField is the payload field carrying the vouched commit
// SHA on an operator_commit_vouched entry. It is the write→read seam: the
// vouch handler writes it (handleVouchCommit) and the reported-head ledger
// reads it (addVouchedSHAs). Both sides reference this single constant so
// the seam cannot drift on a literal typo.
const lineageVouchedSHAField = "vouched_sha"

// vouchCommitRequest is the JSON body of POST
// /v0/runs/{run_id}/vouch-commit. Both fields are required: the vouch is
// an audited operator declaration, so it must name the commit and carry a
// rationale.
type vouchCommitRequest struct {
	SHA    string `json:"sha"`
	Reason string `json:"reason"`
}

// vouchCommitResponse echoes the recorded declaration and reports whether the
// vouch's audit-complete Check Run re-post landed (E64.14 / #3109).
type vouchCommitResponse struct {
	RunID      string `json:"run_id"`
	VouchedSHA string `json:"vouched_sha"`
	Reason     string `json:"reason"`
	// AuditCheckRepublished reports whether the fishhawk_audit_complete Check
	// Run was successfully re-posted at the vouched head (E64.14 / #3109).
	// FALSE when the re-post did not land — it errored (see
	// AuditCheckRepublishWarning), no publisher is wired (dev/CLI posture, no
	// check to post), or the PUBLISH BOUND declined to stamp the check (E64.26
	// / #3129: the vouched sha is not the run's own live pull-request head, or
	// that head could not be resolved at all). The vouch itself always succeeds
	// regardless — the declaration is recorded verbatim for ANY sha; this field
	// makes a swallowed re-post failure VISIBLE (binding condition 1b) so the
	// operator learns the required check is missing HERE rather than at a later
	// blocked merge. The reconciler heal does NOT retry it (normal head
	// resolution excludes operator-vouched commits), so re-invoking
	// fishhawk_vouch_commit is the first-class idempotent retry (condition 1c):
	// the publisher's dedup cache records only successes, so a re-vouch re-posts
	// exactly the dropped check and no-ops once it is live.
	AuditCheckRepublished bool `json:"audit_check_republished"`
	// AuditCheckRepublishWarning, when non-empty, names why the re-post did not
	// land, so the failure is legible in the response body rather than only in
	// the daemon log (E64.14 / #3109, binding condition 1b). It carries THREE
	// kinds of non-landing: a publish that errored, a vouched sha that is not
	// the run's own live PR head (naming BOTH shas), and a live head that could
	// not be resolved (naming the resolution failure) — the latter two are the
	// E64.26 / #3129 publish bound. Empty when the re-post succeeded or when no
	// publisher is wired.
	AuditCheckRepublishWarning string `json:"audit_check_republish_warning,omitempty"`
}

// handleVouchCommit implements POST /v0/runs/{run_id}/vouch-commit.
//
// It is the operator-gated, audited ADR-035 provenance path (#1044) for a
// foreign commit on a run branch that no loop-native remediation can route
// — an operator's mechanical remediation commit (e.g. a sync-schemas
// output pushed onto a fan-out branch whose children are all terminal with
// zero open concerns). The operator declares the commit run-authored
// lineage; the declaration is recorded as an operator_commit_vouched audit
// entry and unioned into the reported-head ledger, un-wedging the merge
// reconciler. It is distinct from reset-branch (which DROPS an on-top
// foreign commit): vouch KEEPS the operator commit and attributes it.
//
// Auth is operator-token-only by design (ADR-035 sole-writer invariant):
//
//   - anonymous → 401 authentication_required;
//   - a run-bound MCP token ("mcp:run:<uuid>" subject) is REJECTED OUTRIGHT
//     (403 run_token_forbidden), even for its own run. A run-bound agent
//     token self-declaring lineage for a foreign commit on its own branch
//     would defeat the sole-writer invariant the vouch must preserve (the
//     #797/#856 cross-write protection). Mirrors the #961
//     decide_scope_amendment run-bound rejection. Only an operator fhk_*
//     token carrying write:stages may vouch;
//   - any identity without write:stages → 403 insufficient_scope. This is
//     enforced UNCONDITIONALLY (no cookie-session bypass): a non-token
//     operator identity must not be able to append vouch lineage either.
//
// The handler records the operator's declaration verbatim — it does NOT
// verify the SHA exists on the branch or in the compare set. This is
// deliberate and safe: vouching a non-existent or wrong SHA simply adds an
// unreachable ledger entry and un-wedges nothing (the real foreign commit
// still flags), so the fail-closed property (an unvouched foreign commit
// still fails category-B) holds.
//
// The vouch is ALSO the re-post trigger for the fishhawk_audit_complete Check
// Run on the operator's head (E64.14 / #3109). After the declaration is
// durable the handler recomputes audit-complete and republishes the Check Run
// via the head override, because an operator-pushed commit is in no
// head-report audit category and the publisher's normal head resolution would
// otherwise re-post against a stale sha — leaving the required check absent
// from the live merge head. The re-post is best-effort for the vouch's success
// but its outcome is REPORTED on the response (audit_check_republished /
// audit_check_republish_warning) rather than swallowed, and re-invoking the
// vouch idempotently retries the re-post.
//
// THE PUBLISH IS BOUND TO THE RUN'S OWN LIVE PR HEAD (E64.26 / #3129). The
// re-post used to be stamped at WHATEVER sha the operator named, so an
// operator carrying write:stages could recompute this run's check state onto
// any commit in the repository — including the head of an unrelated pull
// request. The bound narrows only WHERE the check is stamped: before
// republishing, the handler resolves the run's live pull-request head through
// the same determinability ladder rebase-branch uses
// (resolveVouchPublishHead) and publishes ONLY when the vouched sha equals
// that head. On a mismatch it SKIPS the publish and names BOTH shas on
// audit_check_republish_warning; when the head cannot be resolved at all it
// fails closed the way the rebase verb does — skip the publish, never stamp
// the check at an unverified sha.
//
// What did NOT change: the RECORD stays verbatim for any sha. The
// operator_commit_vouched append and the 200 are byte-identical for a matching
// and a non-matching sha, the ADR-035 escape hatch is untouched, and vouching
// a wrong sha still un-wedges nothing. No new response field, no new error
// code, no status-code change.
func (s *Server) handleVouchCommit(w http.ResponseWriter, r *http.Request) {
	id := IdentityFrom(r.Context())
	if id.IsAnonymous() {
		s.writeError(w, r, http.StatusUnauthorized, "authentication_required",
			"an authenticated token is required", nil)
		return
	}
	// Operator-token-only: a run-bound agent token may NEVER vouch — not
	// even for its own run. Vouch declares git lineage, and an agent
	// self-declaring lineage for a commit on its own branch defeats the
	// ADR-035 sole-writer invariant. Rejected outright, mirroring the #961
	// decide_scope_amendment guard. Defense in depth: implement-stage
	// tokens are never issued write:stages either.
	if _, runBound := runBoundTokenRunID(id); runBound {
		s.writeError(w, r, http.StatusForbidden, "run_token_forbidden",
			"a run-bound agent token may not vouch a commit; vouching git lineage is an operator action (ADR-035 sole-writer invariant)",
			nil)
		return
	}
	// write:stages is enforced UNCONDITIONALLY here — the binding approval
	// condition made vouch operator-fhk-token-only, so this deliberately
	// does NOT mirror the sibling `id.TokenID != ""` guard (reset-branch,
	// waive, retry, …) that waves operator cookie-session identities (empty
	// TokenID, no scopes) past the scope gate. A cookie session, a future
	// non-token credential, or any authenticated-but-unscoped identity must
	// NOT be able to append operator_commit_vouched lineage and unblock the
	// branch-lineage check: vouching git lineage is a scoped operator action
	// (ADR-035 sole-writer invariant).
	if !hasScope(id, "write:stages") {
		s.writeError(w, r, http.StatusForbidden, "insufficient_scope",
			"token is missing required scope: write:stages",
			map[string]any{"required_scope": "write:stages"})
		return
	}

	if s.cfg.RunRepo == nil || s.cfg.AuditRepo == nil {
		s.writeError(w, r, http.StatusServiceUnavailable, "vouch_unconfigured",
			"vouch-commit endpoint requires run + audit repositories", nil)
		return
	}

	runID, err := uuid.Parse(r.PathValue("run_id"))
	if err != nil {
		s.writeError(w, r, http.StatusBadRequest, "validation_failed",
			"run_id must be a valid UUID",
			map[string]any{"field": "run_id", "got": r.PathValue("run_id")})
		return
	}

	var reqBody vouchCommitRequest
	if r.Body != nil {
		if decErr := json.NewDecoder(r.Body).Decode(&reqBody); decErr != nil && !errors.Is(decErr, io.EOF) {
			s.writeError(w, r, http.StatusBadRequest, "validation_failed",
				"request body must be valid JSON {sha, reason}",
				map[string]any{"error": decErr.Error()})
			return
		}
	}
	sha := strings.TrimSpace(reqBody.SHA)
	if sha == "" {
		s.writeError(w, r, http.StatusBadRequest, "validation_failed",
			"sha is required: name the commit to vouch as run-authored lineage",
			map[string]any{"field": "sha"})
		return
	}
	reason := strings.TrimSpace(reqBody.Reason)
	if reason == "" {
		s.writeError(w, r, http.StatusBadRequest, "validation_failed",
			"reason is required: the vouch is an audited operator declaration; state why this commit is run-authored lineage",
			map[string]any{"field": "reason"})
		return
	}

	runRow, err := s.cfg.RunRepo.GetRun(r.Context(), runID)
	if err != nil {
		if errors.Is(err, run.ErrNotFound) {
			s.writeError(w, r, http.StatusNotFound, "run_not_found",
				"no run with that id", map[string]any{"run_id": runID.String()})
			return
		}
		s.writeError(w, r, http.StatusInternalServerError, "internal_error",
			"get run failed", map[string]any{"error": err.Error()})
		return
	}

	subject := id.Subject
	if subject == "" {
		subject = "anonymous"
	}
	actorKind := audit.ActorUser
	payload, _ := json.Marshal(map[string]any{
		"run_id":               runID.String(),
		lineageVouchedSHAField: sha,
		"reason":               reason,
	})
	if _, err := s.cfg.AuditRepo.AppendChained(r.Context(), audit.ChainAppendParams{
		RunID:        runID,
		Timestamp:    time.Now().UTC(),
		Category:     CategoryOperatorCommitVouched,
		ActorKind:    &actorKind,
		ActorSubject: &subject,
		Payload:      payload,
	}); err != nil {
		s.cfg.Logger.LogAttrs(r.Context(), slog.LevelWarn,
			"vouch-commit: append operator_commit_vouched audit entry failed",
			slog.String("run_id", runID.String()),
			slog.String("vouched_sha", sha),
			slog.String("error", err.Error()))
		s.writeError(w, r, http.StatusInternalServerError, "internal_error",
			"record vouch failed", map[string]any{"error": err.Error()})
		return
	}

	// Re-post the fishhawk_audit_complete Check Run AT the vouched sha (E64.14 /
	// #3109). This runs ONLY after the operator_commit_vouched entry is durably
	// appended, so the recompute observes the vouch in the reported-head ledger.
	// The vouched commit is an operator-pushed head that appears in NO
	// head-report audit category, so the publisher's normal head resolution
	// (fixup_pushed > child_pushed > pull_request_opened) would republish
	// against a STALE sha and leave the required check absent from the live
	// merge head — the exact wedge this endpoint closes. The head override
	// targets the vouched sha directly — but ONLY once the publish bound below
	// has confirmed that sha IS the run's own live pull-request head.
	//
	// A re-post failure does NOT fail the vouch — the declaration is already
	// durable. But it is NOT swallowed either (binding condition 1b): the
	// outcome is surfaced on the response so the operator knows the required
	// check is missing rather than discovering it at a blocked merge. The
	// reconciler heal cannot recover it (it uses the same operator-vouched-blind
	// head resolution), so re-invoking fishhawk_vouch_commit is the sanctioned
	// idempotent retry (condition 1c): the publisher dedups on success, so a
	// re-vouch re-posts exactly the dropped check and no-ops once it is live.
	//
	// THE PUBLISH BOUND (E64.26 / #3129). The re-post is stamped ONLY at the
	// run's OWN live pull-request head. Without it an operator carrying
	// write:stages could name any commit in the repository — including the head
	// of an unrelated PR — and have this run's recomputed check state stamped
	// there. The three arms below run AFTER the append, so they can only affect
	// WHERE the check is posted: the record is already durable and the 200 is
	// byte-identical for a matching and a non-matching sha.
	resp := vouchCommitResponse{
		RunID:      runID.String(),
		VouchedSHA: sha,
		Reason:     reason,
	}
	// ARM (a): no publisher wired (dev/CLI posture). Checked FIRST so this
	// posture keeps its documented silent, warning-free skip — resolving a live
	// PR head we would never publish to would newly warn on every dev vouch.
	// publishAuditCheckWithOptions already returns (false, nil) here.
	if s.auditCheckPublisher != nil {
		liveHead, headErr := s.resolveVouchPublishHead(r.Context(), runRow)
		switch {
		case headErr != nil:
			// FAIL CLOSED, mirroring the rebase verb: an unresolvable head is
			// never a licence to stamp the check at an unverified sha.
			resp.AuditCheckRepublishWarning = "the vouch is recorded and durable, but the fishhawk_audit_complete check was NOT re-posted: the run's live pull-request head could not be resolved, and publishing at an unverified commit is refused (" + headErr.Error() + "); re-invoke fishhawk_vouch_commit once the pull request is readable"
		case !shaEqualsHead(sha, liveHead):
			// MISMATCH. Name BOTH shas so the operator can see what was vouched
			// versus what is live — this is also what tells an operator who
			// pasted an ABBREVIATED sha to re-vouch with the full one.
			resp.AuditCheckRepublishWarning = "the vouch IS recorded and durable, but the fishhawk_audit_complete check was NOT re-posted: the vouched commit " + sha + " is not the run's live pull-request head " + liveHead + ", and the check is only ever stamped on the run's own head (#3129); re-invoke fishhawk_vouch_commit naming the live head to re-post the check"
		default:
			republished, pubErr := s.recomputeAndPublishAuditCompleteAtHead(r.Context(), runID, sha)
			resp.AuditCheckRepublished = republished
			if pubErr != nil {
				resp.AuditCheckRepublishWarning = "the vouch is recorded and durable, but re-posting the fishhawk_audit_complete check at the vouched commit failed; the required check may be absent from the merge head — re-invoke fishhawk_vouch_commit to retry the re-post: " + pubErr.Error()
			}
		}
	}
	s.writeJSON(w, r, http.StatusOK, resp)
}

// shaEqualsHead reports whether a vouched sha names the same commit as the
// run's live pull-request head. The compare is a case-insensitive FULL-string
// compare on the trimmed spellings: forge shas are lowercase hex, so folding
// case only tolerates an operator pasting mixed case and can never widen the
// bound past the run's own head. An ABBREVIATED sha deliberately does NOT
// match — the mismatch warning names both shas, which is what tells the
// operator to re-vouch with the full sha.
func shaEqualsHead(vouched, liveHead string) bool {
	return strings.EqualFold(strings.TrimSpace(vouched), strings.TrimSpace(liveHead))
}

// resolveVouchPublishHead resolves the run's own live pull-request head sha,
// which is the ONLY commit the vouch's audit-complete re-post may be stamped
// at (E64.26 / #3129). It mirrors the determinability ladder the rebase verb
// already uses (rebase_branch.go) rather than inventing a second one, and
// every rung returns an ERROR — there is no empty-string-means-ok result, so a
// caller cannot fall through to publishing on an uncertain read. It performs
// NO writes.
func (s *Server) resolveVouchPublishHead(ctx context.Context, runRow *run.Run) (string, error) {
	if runRow == nil || runRow.InstallationID == nil || *runRow.InstallationID == 0 {
		return "", errors.New("run has no installation to read its pull request")
	}
	scope := forge.FromGitHubInstallationID(*runRow.InstallationID)
	repo, err := parseRepoOwnerName(runRow.Repo)
	if err != nil {
		return "", errors.New("run repo is unparseable: " + err.Error())
	}
	prNumber := parsePRNumberFromURL(runRow.PullRequestURL)
	if prNumber <= 0 {
		return "", errors.New("run has no tracked pull request")
	}
	if s.cfg.GitHub == nil {
		return "", errors.New("no GitHub client is wired to read the pull request")
	}
	pr, err := s.cfg.GitHub.GetPullRequest(ctx, scope, repo, prNumber)
	if err != nil {
		return "", errors.New("read live pull request head failed: " + err.Error())
	}
	if strings.TrimSpace(pr.HeadSHA) == "" {
		return "", errors.New("the pull request returned an empty head sha")
	}
	return pr.HeadSHA, nil
}
