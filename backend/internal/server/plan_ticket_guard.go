package server

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/plan"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

// planTicketMismatchCode is the error code POST /v0/runs/{run_id}/plan
// returns when an uploaded plan-stage artifact's ticket_reference names a
// different issue than the run's issue:N trigger (#4067). The runner maps
// it to category B (runner/internal/upload agentOutputInvalidCodes), which
// agrees with the fail-B transition the guard performs.
const planTicketMismatchCode = "plan_ticket_mismatch"

// ticketClaim is one issue a ticket_reference confidently names: the
// repository path (empty when the form names only a number, e.g. "#N")
// and the issue number.
type ticketClaim struct {
	repo   string
	number int
}

// parseTicketClaims extracts every confident issue claim from a plan-stage
// artifact's top-level ticket_reference. It accepts the object form
// {id, url} and a bare-string ticket_reference (the pre-coercion shape,
// treated as a url). Recognized forms:
//
//   - id "owner/name#N", "group/sub/project#N" → (that repo, N)
//   - id "#N"                                  → (no repo, N)
//   - url path ending /issues/N or /-/issues/N → (path before the suffix, N)
//
// An API-host or /api/ url names the issue number only (its path prefix is
// not a repository). Anything else — id "unknown", the comms unanchored
// "<owner>/<repo>" id, an issues-index url, an unparseable body — yields no
// claim, so the guard fails OPEN on it. The parse is pure and never errors.
func parseTicketClaims(raw json.RawMessage) []ticketClaim {
	var doc struct {
		TicketReference json.RawMessage `json:"ticket_reference"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil || len(doc.TicketReference) == 0 {
		return nil
	}
	var idStr, urlStr string
	var asString string
	if err := json.Unmarshal(doc.TicketReference, &asString); err == nil {
		urlStr = asString
	} else {
		var obj map[string]json.RawMessage
		if err := json.Unmarshal(doc.TicketReference, &obj); err != nil {
			return nil
		}
		_ = json.Unmarshal(obj["id"], &idStr)
		_ = json.Unmarshal(obj["url"], &urlStr)
	}
	var claims []ticketClaim
	if c, ok := claimFromTicketID(idStr); ok {
		claims = append(claims, c)
	}
	if c, ok := claimFromTicketURL(urlStr); ok {
		claims = append(claims, c)
	}
	return claims
}

// claimFromTicketID parses "owner/name#N", "group/sub/project#N" or "#N".
func claimFromTicketID(id string) (ticketClaim, bool) {
	id = strings.TrimSpace(id)
	i := strings.LastIndex(id, "#")
	if i < 0 {
		return ticketClaim{}, false
	}
	n, err := strconv.Atoi(id[i+1:])
	if err != nil || n <= 0 {
		return ticketClaim{}, false
	}
	repo := id[:i]
	if repo != "" && !validRepoPath(repo) {
		return ticketClaim{}, false
	}
	return ticketClaim{repo: repo, number: n}, true
}

// claimFromTicketURL parses a url whose path ends /issues/N or /-/issues/N.
func claimFromTicketURL(raw string) (ticketClaim, bool) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Host == "" {
		return ticketClaim{}, false
	}
	segs := strings.Split(strings.Trim(u.Path, "/"), "/")
	if len(segs) < 3 || segs[len(segs)-2] != "issues" {
		return ticketClaim{}, false
	}
	n, err := strconv.Atoi(segs[len(segs)-1])
	if err != nil || n <= 0 {
		return ticketClaim{}, false
	}
	repoSegs := segs[:len(segs)-2]
	if len(repoSegs) > 0 && repoSegs[len(repoSegs)-1] == "-" {
		repoSegs = repoSegs[:len(repoSegs)-1]
	}
	repo := strings.Join(repoSegs, "/")
	if strings.HasPrefix(strings.ToLower(u.Host), "api.") || strings.EqualFold(segs[0], "api") {
		// An API url's path prefix (repos/…, api/v4/projects/…) is not a
		// repository path: claim the number only.
		repo = ""
	} else if !validRepoPath(repo) {
		return ticketClaim{}, false
	}
	return ticketClaim{repo: repo, number: n}, true
}

// validRepoPath reports whether s is a slash-separated path of at least two
// non-empty segments ("owner/name", "group/sub/project").
func validRepoPath(s string) bool {
	segs := strings.Split(s, "/")
	if len(segs) < 2 {
		return false
	}
	for _, seg := range segs {
		if seg == "" || strings.ContainsAny(seg, " \t#") {
			return false
		}
	}
	return true
}

// guardPlanTicketReference refuses a plan-stage upload whose
// ticket_reference confidently names a different issue than the run's
// issue:N trigger (#4067 defense in depth: a contaminated /tmp handoff must
// never let a run ship another run's plan or sibling artifact). It runs in
// handleShipPlan before kind routing, so it covers plan,
// clarification_request, grooming_report, upkeep_report and comms_report
// alike, and before any artifact or audit row is written.
//
// Fails OPEN (returns false, upload proceeds) when the run cannot be
// loaded, has no TriggerRef, its TriggerRef is not issue:N, or the
// ticket_reference yields no parseable claim. Refuses when ANY claim's
// number differs from the trigger number or its repo (when the claim names
// one) differs case-insensitively from run.Repo: the stage is failed
// category-B with a plan_ticket_mismatch failure_reason, the run advanced,
// and 400 plan_ticket_mismatch written. Not retried in-run — with keyed
// handoff paths a mismatch is a terminal, operator-visible fault.
// Returns true when it wrote the refusal.
func (s *Server) guardPlanTicketReference(w http.ResponseWriter, r *http.Request, runID, stageID uuid.UUID, kind plan.ArtifactKind, body []byte) bool {
	runRow, err := s.cfg.RunRepo.GetRun(r.Context(), runID)
	if err != nil || runRow == nil {
		return false // fail open: a later layer owns a run load error
	}
	if runRow.TriggerRef == nil {
		return false // fail open: an ad-hoc run names no ticket
	}
	triggerN, ok := parseIssueRef(*runRow.TriggerRef)
	if !ok {
		return false // fail open: a non-issue trigger (schedule:…) names no issue
	}
	var bad *ticketClaim
	for _, c := range parseTicketClaims(body) {
		if c.number != triggerN || (c.repo != "" && !strings.EqualFold(c.repo, runRow.Repo)) {
			c := c
			bad = &c
			break
		}
	}
	if bad == nil {
		return false
	}

	claimedID, claimedURL := ticketReferenceStrings(body)
	claimed := fmt.Sprintf("#%d", bad.number)
	if bad.repo != "" {
		claimed = bad.repo + claimed
	}
	reason := fmt.Sprintf("%s: %s ticket_reference names %s but the run's trigger is %s on %s",
		planTicketMismatchCode, kind, claimed, *runRow.TriggerRef, runRow.Repo)
	if _, ferr := run.FailStage(r.Context(), s.cfg.RunRepo, stageID, run.FailureB, reason); ferr != nil {
		s.cfg.Logger.LogAttrs(r.Context(), slog.LevelWarn,
			"plan upload: transition to failed-B after ticket mismatch failed",
			slog.String("run_id", runID.String()),
			slog.String("stage_id", stageID.String()),
			slog.String("error", ferr.Error()))
	}
	s.advanceAfterFailure(r, runID, stageID)
	s.writeError(w, r, http.StatusBadRequest, planTicketMismatchCode,
		"uploaded "+string(kind)+" names a different ticket than the run's issue trigger",
		map[string]any{
			"artifact_kind":   string(kind),
			"claimed_id":      claimedID,
			"claimed_url":     claimedURL,
			"run_trigger_ref": *runRow.TriggerRef,
			"run_repo":        runRow.Repo,
		})
	return true
}

// ticketReferenceStrings returns the raw id and url of the body's
// ticket_reference for the refusal details (a bare string is the url).
func ticketReferenceStrings(body []byte) (id, u string) {
	var doc struct {
		TicketReference json.RawMessage `json:"ticket_reference"`
	}
	if json.Unmarshal(body, &doc) != nil {
		return "", ""
	}
	if json.Unmarshal(doc.TicketReference, &u) == nil {
		return "", u
	}
	var obj struct {
		ID  string `json:"id"`
		URL string `json:"url"`
	}
	_ = json.Unmarshal(doc.TicketReference, &obj)
	return obj.ID, obj.URL
}
