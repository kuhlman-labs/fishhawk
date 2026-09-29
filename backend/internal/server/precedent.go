package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/decisionindex"
	"github.com/kuhlman-labs/fishhawk/backend/internal/precedent"
)

// GET /v0/precedent (E75.3 / #3731, ADR-082 #3728 decision (b)) answers "how
// have we decided this kind of thing before, in THIS repository?" from the
// merged decision index (E75.2 / #3730).
//
// It is READ-ONLY in the strongest sense: it writes nothing, mints NO audit
// entry, and grants no authority. A precedent item is evidence a human or a
// delegation rule already weighed something similar — not a licence to decide
// the same way. Every item explains WHICH keys matched and what each score
// component contributed, and cites its source chain entry by sequence and
// entry_hash so the reasoning can be read at the source.
//
// Long-form contract: backend/internal/server/README.md § "Precedent query".

// PrecedentIndex is the read surface GET /v0/precedent needs from the decision
// index. *decisionindex.Store satisfies it. Declared as an interface so the
// handler is testable without Postgres and so a nil field is a named 503 rather
// than a panic.
type PrecedentIndex interface {
	List(ctx context.Context, f decisionindex.ListFilter) ([]decisionindex.Row, error)
	GateContext(ctx context.Context, ref decisionindex.GateRef) (decisionindex.GateContext, error)
}

const (
	// precedentCandidateWindow bounds the rows scored per request. The window is
	// the NEWEST N rows matching the hard filter; a FULL window is reported
	// (truncated + a named degradation), never silently accepted, because a
	// genuinely relevant older decision can fall outside it.
	precedentCandidateWindow = 500
	// precedentDefaultLimit / precedentMaxLimit bound the returned items. The
	// max is what bounds the per-request chain reads: each DISTINCT run in the
	// ranked set is read at most once, so the N+1 is capped at
	// precedentMaxLimit by construction.
	precedentDefaultLimit = 20
	precedentMaxLimit     = 50
	// precedentReasonExcerptCap bounds ONE reason excerpt, in encoded bytes.
	precedentReasonExcerptCap = 280
	// precedentResolvedContextListCap bounds each echoed resolved-context list
	// (touched paths, escalation keys). A caller may supply thousands of paths;
	// echoing them all would make the response size a function of the REQUEST
	// rather than of the answer, and would put the MCP ladder's summary-only
	// floor above the byte budget (#3731 binding condition 2). The untruncated
	// total is reported alongside.
	precedentResolvedContextListCap = 20
)

// Degradation reasons — machine-readable names for something the response could
// not include. Mirrored by the OpenAPI PrecedentDegraded.reason enum.
const (
	precedentDegradedWindowTruncated      = "window_truncated"
	precedentDegradedNoIndexedDecisions   = "no_indexed_decisions"
	precedentDegradedAuditRepoUnavailable = "audit_repo_unconfigured"
	precedentDegradedReasonUnreadable     = "reason_entry_unreadable"
	precedentDegradedReasonMissing        = "reason_entry_missing"
)

// precedentResponse is the GET /v0/precedent body.
type precedentResponse struct {
	ResolvedContext precedentResolvedContext `json:"resolved_context"`
	Summary         precedent.Summary        `json:"summary"`
	Results         []precedent.Item         `json:"results"`
	// Truncated is true when the candidate window was full — there may be
	// relevant rows older than the newest precedentCandidateWindow.
	Truncated bool                `json:"truncated"`
	Degraded  []precedentDegraded `json:"degraded"`
}

// precedentResolvedContext echoes what the ranking was actually performed
// against, so a caller can see how a gate reference resolved and which of its
// own inputs survived. Each list is capped — see
// precedentResolvedContextListCap.
type precedentResolvedContext struct {
	Repo            string `json:"repo"`
	DecisionClass   string `json:"decision_class"`
	StageKind       string `json:"stage_kind,omitempty"`
	ConcernCategory string `json:"concern_category,omitempty"`
	Severity        string `json:"severity,omitempty"`
	RunID           string `json:"run_id,omitempty"`
	StageID         string `json:"stage_id,omitempty"`

	TouchedPaths          []string `json:"touched_paths"`
	TouchedPathsTotal     int      `json:"touched_paths_total"`
	TouchedPathsTruncated bool     `json:"touched_paths_truncated,omitempty"`

	EscalationKeys          []string `json:"escalation_keys"`
	EscalationKeysTotal     int      `json:"escalation_keys_total"`
	EscalationKeysTruncated bool     `json:"escalation_keys_truncated,omitempty"`
}

// precedentDegraded names one thing the response could not include.
type precedentDegraded struct {
	Reason         string `json:"reason"`
	SourceSequence int64  `json:"source_sequence,omitempty"`
	RunID          string `json:"run_id,omitempty"`
	Detail         string `json:"detail,omitempty"`
}

// handleGetPrecedent implements GET /v0/precedent.
//
// Two input modes:
//
//	(a) EXPLICIT CONTEXT — repo (required), decision_class (required),
//	    stage_kind, paths (repeatable), concern_category, severity,
//	    escalation_keys (repeatable).
//	(b) GATE REFERENCE — run_id (+ optional stage_id), from which the index
//	    derives repo, stage kind, touched paths and escalation keys through the
//	    same joins the indexer used. decision_class stays REQUIRED: a gate does
//	    not determine which CLASS of decision the caller wants precedent about,
//	    and inferring one from the stage kind would be a guess the response
//	    could not explain.
//
// An explicit field supplied ALONGSIDE a gate reference OVERRIDES the derived
// value, and resolved_context echoes the result either way.
//
// Auth: the gate-reference resolve is ACCOUNT-SCOPED (binding condition 1) — the
// run is loaded under the caller's account exactly as requireRunAccount's
// ownership rule narrows a run read, so a run in another account answers the
// same 404 as a nonexistent one and NO resolved context is echoed. The candidate
// row window is narrowed by the same account. The repository is additionally
// subject to the point-read repo-visibility DENY, because a precedent item
// carries a decision's reason prose.
func (s *Server) handleGetPrecedent(w http.ResponseWriter, r *http.Request) {
	if s.cfg.PrecedentIndex == nil {
		s.writeError(w, r, http.StatusServiceUnavailable, "precedent_unconfigured",
			"precedent endpoint requires the decision index to be configured", nil)
		return
	}
	q := r.URL.Query()
	ctx := r.Context()

	class := strings.TrimSpace(q.Get("decision_class"))
	accepted := precedentDecisionClasses()
	if class == "" || !precedentClassAccepted(accepted, class) {
		s.writeError(w, r, http.StatusBadRequest, "validation_failed",
			"decision_class is required and must be one of the recognized decision classes",
			map[string]any{"field": "decision_class", "got": class, "accepted": accepted})
		return
	}
	limit, err := parseLimit(q.Get("limit"), precedentDefaultLimit, precedentMaxLimit)
	if err != nil {
		s.writeError(w, r, http.StatusBadRequest, "validation_failed",
			err.Error(), map[string]any{"field": "limit"})
		return
	}

	acct, ok := s.callerAccountUUID(w, r)
	if !ok {
		return
	}

	pctx := precedent.Context{DecisionClass: decisionindex.DecisionClass(class)}
	resolved := precedentResolvedContext{DecisionClass: class}

	// (b) Gate reference first, so an explicit field can override a derived one.
	if raw := strings.TrimSpace(q.Get("run_id")); raw != "" {
		ref, bad := parseGateRef(raw, strings.TrimSpace(q.Get("stage_id")), acct)
		if bad != "" {
			s.writeError(w, r, http.StatusBadRequest, "validation_failed", bad,
				map[string]any{"field": "run_id"})
			return
		}
		gc, gerr := s.cfg.PrecedentIndex.GateContext(ctx, ref)
		if gerr != nil {
			if errors.Is(gerr, decisionindex.ErrRunMissing) {
				// The SAME 404 for a nonexistent run, a run in another account,
				// and a stage that is not on the run — and nothing derived is
				// echoed on any of those paths.
				s.writeError(w, r, http.StatusNotFound, "not_found",
					"no such run (or the stage does not belong to it)", nil)
				return
			}
			s.writeError(w, r, http.StatusInternalServerError, "internal_error",
				"resolve gate context failed", map[string]any{"error": gerr.Error()})
			return
		}
		pctx.Repo = gc.Repo
		pctx.StageKind = gc.StageKind
		pctx.TouchedPaths = gc.TouchedPaths
		pctx.EscalationKeys = gc.EscalationKeys
		resolved.RunID = ref.RunID.String()
		if ref.StageID != nil {
			resolved.StageID = ref.StageID.String()
		}
	}

	// (a) Explicit context, overriding whatever the gate reference derived.
	if v := strings.TrimSpace(q.Get("repo")); v != "" {
		pctx.Repo = v
	}
	if v := strings.TrimSpace(q.Get("stage_kind")); v != "" {
		pctx.StageKind = v
	}
	if vs := nonEmptyValues(q["paths"]); len(vs) > 0 {
		pctx.TouchedPaths = vs
	}
	if vs := nonEmptyValues(q["escalation_keys"]); len(vs) > 0 {
		pctx.EscalationKeys = vs
	}
	if v := strings.TrimSpace(q.Get("severity")); v != "" {
		pctx.Severity = v
	}
	if v := strings.TrimSpace(q.Get("concern_category")); v != "" {
		// Normalized so the caller's raw spelling matches the canonical value
		// the index stored. An unmapped value normalizes to itself, so it still
		// matches rows indexed with the same unmapped spelling.
		pctx.ConcernCategory, _ = decisionindex.NormalizeConcernCategory(v)
	}

	if pctx.Repo == "" {
		s.writeError(w, r, http.StatusBadRequest, "validation_failed",
			"repo is required (supply it directly, or a run_id whose run resolves one)",
			map[string]any{"field": "repo"})
		return
	}
	// A precedent item carries decision reason prose, so the repository is
	// subject to the same point-read visibility DENY as the repo dashboards.
	if !s.enforceRepoVisibility(w, r, pctx.Repo) {
		return
	}

	rows, err := s.cfg.PrecedentIndex.List(ctx, decisionindex.ListFilter{
		Repo:          pctx.Repo,
		DecisionClass: pctx.DecisionClass,
		StageKind:     pctx.StageKind,
		AccountID:     acct,
		Newest:        true,
		Limit:         precedentCandidateWindow,
	})
	if err != nil {
		s.writeError(w, r, http.StatusInternalServerError, "internal_error",
			"list decision index failed", map[string]any{"error": err.Error()})
		return
	}

	items, summary := precedent.Rank(pctx, rows, limit)
	resp := precedentResponse{
		Summary:  summary,
		Results:  items,
		Degraded: []precedentDegraded{},
	}
	if len(rows) >= precedentCandidateWindow {
		resp.Truncated = true
		resp.Degraded = append(resp.Degraded, precedentDegraded{
			Reason: precedentDegradedWindowTruncated,
			Detail: fmt.Sprintf("the newest %d matching rows were scored; an older relevant decision may exist outside the window",
				precedentCandidateWindow),
		})
	}
	if len(rows) == 0 {
		resp.Degraded = append(resp.Degraded, precedentDegraded{
			Reason: precedentDegradedNoIndexedDecisions,
			Detail: "no indexed decision of this class exists for this repository yet",
		})
	}

	resp.Results, resp.Degraded = s.attachReasonExcerpts(ctx, resp.Results, resp.Degraded)

	resolved.Repo = pctx.Repo
	resolved.StageKind = pctx.StageKind
	resolved.ConcernCategory = pctx.ConcernCategory
	resolved.Severity = pctx.Severity
	resolved.TouchedPaths, resolved.TouchedPathsTotal, resolved.TouchedPathsTruncated =
		capResolvedList(pctx.TouchedPaths)
	resolved.EscalationKeys, resolved.EscalationKeysTotal, resolved.EscalationKeysTruncated =
		capResolvedList(pctx.EscalationKeys)
	resp.ResolvedContext = resolved

	s.writeJSON(w, r, http.StatusOK, resp)
}

// callerAccountUUID resolves the caller's workspace account to a uuid. An
// identity carrying NO account yields nil — the untenanted posture. A non-empty
// value that is not a uuid FAILS CLOSED with a 500 rather than silently
// widening the read to every account: the field is written from a sessions row,
// so an unparseable value is a corrupted invariant, not a caller mistake.
func (s *Server) callerAccountUUID(w http.ResponseWriter, r *http.Request) (*uuid.UUID, bool) {
	raw := IdentityFrom(r.Context()).AccountID
	if raw == "" {
		return nil, true
	}
	id, err := uuid.Parse(raw)
	if err != nil {
		s.writeError(w, r, http.StatusInternalServerError, "internal_error",
			"the session's workspace account id is not a UUID", nil)
		return nil, false
	}
	return &id, true
}

// parseGateRef parses the run/stage ids of a gate reference, carrying the
// caller's account into the reference so the resolve itself is narrowed.
func parseGateRef(rawRun, rawStage string, acct *uuid.UUID) (decisionindex.GateRef, string) {
	runID, err := uuid.Parse(rawRun)
	if err != nil {
		return decisionindex.GateRef{}, "run_id must be a UUID"
	}
	ref := decisionindex.GateRef{RunID: runID, AccountID: acct}
	if rawStage != "" {
		stageID, serr := uuid.Parse(rawStage)
		if serr != nil {
			return decisionindex.GateRef{}, "stage_id must be a UUID"
		}
		ref.StageID = &stageID
	}
	return ref, ""
}

// attachReasonExcerpts reads each item's cited reason AT QUERY TIME from the
// chain (ADR-082 rule 1 — the prose is never copied into the index) and caps it.
//
// The chain is read ONCE PER DISTINCT RUN, so the read count is bounded by the
// number of distinct runs in the ranked set (itself bounded by
// precedentMaxLimit). Every failure DEGRADES with a named reason and no excerpt
// — a nil audit repository, a failed read, a missing entry, a missing payload
// key, or a non-string value. It never fabricates an excerpt and never fails the
// request.
func (s *Server) attachReasonExcerpts(ctx context.Context, items []precedent.Item, degraded []precedentDegraded) ([]precedent.Item, []precedentDegraded) {
	if len(items) == 0 {
		return items, degraded
	}
	if s.cfg.AuditRepo == nil {
		return items, append(degraded, precedentDegraded{
			Reason: precedentDegradedAuditRepoUnavailable,
			Detail: "reason excerpts are read from the audit chain at query time; no audit repository is configured",
		})
	}
	type runChain struct {
		bySeq map[int64]json.RawMessage
		err   error
	}
	chains := map[string]*runChain{}
	for i := range items {
		it := &items[i]
		if it.ReasonKey == "" {
			continue
		}
		chain, loaded := chains[it.RunID]
		if !loaded {
			chain = &runChain{}
			chains[it.RunID] = chain
			runID, perr := uuid.Parse(it.RunID)
			if perr != nil {
				chain.err = perr
			} else {
				entries, lerr := s.cfg.AuditRepo.ListForRun(ctx, runID)
				if lerr != nil {
					chain.err = lerr
				} else {
					chain.bySeq = make(map[int64]json.RawMessage, len(entries))
					for _, e := range entries {
						if e != nil {
							chain.bySeq[e.Sequence] = e.Payload
						}
					}
				}
			}
		}
		if chain.err != nil {
			degraded = append(degraded, precedentDegraded{
				Reason: precedentDegradedReasonUnreadable, RunID: it.RunID,
				SourceSequence: it.SourceSequence, Detail: chain.err.Error(),
			})
			continue
		}
		payload, found := chain.bySeq[it.ReasonSequence]
		if !found {
			degraded = append(degraded, precedentDegraded{
				Reason: precedentDegradedReasonMissing, RunID: it.RunID,
				SourceSequence: it.SourceSequence,
				Detail:         fmt.Sprintf("no chain entry at sequence %d", it.ReasonSequence),
			})
			continue
		}
		excerpt, okVal := payloadString(payload, it.ReasonKey)
		if !okVal {
			degraded = append(degraded, precedentDegraded{
				Reason: precedentDegradedReasonMissing, RunID: it.RunID,
				SourceSequence: it.SourceSequence,
				Detail:         fmt.Sprintf("entry %d carries no string %q key", it.ReasonSequence, it.ReasonKey),
			})
			continue
		}
		it.ReasonExcerpt = capExcerpt(excerpt, precedentReasonExcerptCap)
	}
	// No sort here, deliberately. The per-item degradations are appended while
	// walking the RANKED items, whose order is already total (precedent.Rank),
	// and the request-level ones are appended at fixed points — so the list is
	// deterministic by construction. The `chains` map is a per-run read cache
	// that is never ITERATED, so map ordering cannot reach the response. Adding a
	// sort would be a control with nothing to control; the byte-identity claim is
	// pinned by TestPrecedent_RepeatedCallsByteIdentical.
	return items, degraded
}

// precedentDecisionClasses is the accepted decision_class set, DERIVED from
// decisionindex's own decision-bearing table rather than transcribed, so a new
// class is accepted here the moment it is registered there.
func precedentDecisionClasses() []string {
	seen := map[string]struct{}{}
	for _, cat := range decisionindex.DecisionBearingCategories() {
		if cl, ok := decisionindex.ClassFor(cat); ok {
			seen[string(cl)] = struct{}{}
		}
	}
	out := make([]string, 0, len(seen))
	for c := range seen {
		out = append(out, c)
	}
	sort.Strings(out)
	return out
}

func precedentClassAccepted(hay []string, needle string) bool {
	for _, h := range hay {
		if h == needle {
			return true
		}
	}
	return false
}

// payloadString returns key's value when the payload carries it as a JSON
// string. Anything else — absent, wrong type, undecodable payload — is (.., false).
func payloadString(raw json.RawMessage, key string) (string, bool) {
	if len(raw) == 0 {
		return "", false
	}
	var m map[string]json.RawMessage
	if json.Unmarshal(raw, &m) != nil {
		return "", false
	}
	v, present := m[key]
	if !present {
		return "", false
	}
	var s string
	if json.Unmarshal(v, &s) != nil {
		return "", false
	}
	return s, true
}

// capExcerpt truncates to at most limit BYTES on a UTF-8 rune boundary, appending
// a marker when it bites so a truncation is never silent.
func capExcerpt(s string, limit int) string {
	if len(s) <= limit {
		return s
	}
	const marker = "…[truncated]"
	keep := limit - len(marker)
	if keep < 0 {
		keep = 0
	}
	for keep > 0 && !utf8RuneStart(s[keep]) {
		keep--
	}
	return s[:keep] + marker
}

// utf8RuneStart reports whether b begins a UTF-8 rune (i.e. is not a
// continuation byte).
func utf8RuneStart(b byte) bool { return b&0xC0 != 0x80 }

// capResolvedList bounds one echoed resolved-context list, reporting the
// untruncated total. It sorts first so the echo is deterministic and the kept
// prefix is stable for the same input set in any order.
func capResolvedList(in []string) (kept []string, total int, truncated bool) {
	out := append([]string(nil), in...)
	sort.Strings(out)
	total = len(out)
	if total > precedentResolvedContextListCap {
		return out[:precedentResolvedContextListCap], total, true
	}
	if out == nil {
		return []string{}, 0, false
	}
	return out, total, false
}

// nonEmptyValues trims a repeatable query parameter's values and drops the
// blanks, so `?paths=&paths=a.go` is one path rather than two.
func nonEmptyValues(in []string) []string {
	out := make([]string, 0, len(in))
	for _, v := range in {
		if t := strings.TrimSpace(v); t != "" {
			out = append(out, t)
		}
	}
	return out
}
