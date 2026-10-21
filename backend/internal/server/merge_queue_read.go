package server

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"time"

	"github.com/kuhlman-labs/fishhawk/backend/internal/forge"
	"github.com/kuhlman-labs/fishhawk/backend/internal/mergequeue"
)

// The per-base merge-queue read (ADR-093 slice 1 D7, E87.4 / #4295):
// GET /v0/repos/{owner}/{name}/merge-queue[?base_ref=]. README.md § "Merge
// queue read" is the contract.

// mergeQueueETASamples bounds the line-turn history the ETA median reads: the
// base's newest N merged entries (mergequeue.Store.RecentLineTurns).
const mergeQueueETASamples = 10

// scopeMergeQueueRead is the read's scope tier: the gate view's read:audit,
// the same tier as the cross-run scope-overlap read, because the payload names
// other runs' ids and heads.
const scopeMergeQueueRead = scopeGateViewRead

// mergeQueueETABasisNote is the constant statement of what eta_seconds
// measures (approval condition C9).
const mergeQueueETABasisNote = "eta_seconds = position x the median line-turn time (admission to merge) of the base's newest merged entries; until ADR-093 slice 2's merge-on-pass lands a line turn includes the operator's merge step, so the estimate is as fast as the operator merges"

// Head-of-line current-head degrade reasons: the forge read is best-effort,
// so each names why current_head_sha is absent and the read still answers 200.
const (
	mergeQueueHeadRunReadFailed      = "run_read_failed"
	mergeQueueHeadForgeUnwired       = "forge_unwired"
	mergeQueueHeadPullRequestUnknown = "pull_request_unresolvable"
	mergeQueueHeadForgeReadFailed    = "forge_read_failed"
)

// mergeQueueReadResponse is the read's body. The MCP mirror
// (backend/internal/mcpserver/client.go) decodes it; the json tags MUST
// byte-match or a field silently decodes to its zero value.
type mergeQueueReadResponse struct {
	Repo string `json:"repo"`
	// Bases is one block per base ref: the requested one, or every base of
	// the repository with an active entry in the caller's account. Never nil.
	Bases []mergeQueueBaseView `json:"bases"`
}

// mergeQueueBaseView is one (account, repository, base ref) queue.
type mergeQueueBaseView struct {
	BaseRef string `json:"base_ref"`
	// Entries are the base's active entries in queue order: the live entry
	// first, then held entries FIFO by (enqueued_at, id). Never nil.
	Entries []mergeQueueEntryView `json:"entries"`
	// HeadOfLine is the first entry (the live one, or the next held entry to
	// admit when none is live) with its PR's current head; omitted when the
	// base has no active entry.
	HeadOfLine *mergeQueueHeadOfLine `json:"head_of_line,omitempty"`
	ETABasis   mergeQueueETABasis    `json:"eta_basis"`
}

// mergeQueueEntryView is one active entry.
type mergeQueueEntryView struct {
	RunID   string `json:"run_id"`
	EntryID string `json:"entry_id"`
	State   string `json:"state"`
	// Phase is a live entry's progress; omitted while held (the store
	// clears it on every held row).
	Phase string `json:"phase,omitempty"`
	// Position is the 1-based place among the base's held entries; omitted
	// (0) for the live entry.
	Position int `json:"position,omitempty"`
	// MergeIntent is whether the operator's merge_run recorded merge intent
	// on the entry (ADR-093 D2); false for a rebase-path entry. Always on
	// the wire.
	MergeIntent     bool       `json:"merge_intent"`
	MergeIntentAt   *time.Time `json:"merge_intent_at,omitempty"`
	EnqueuedAt      time.Time  `json:"enqueued_at"`
	AdmittedAt      *time.Time `json:"admitted_at,omitempty"`
	AnchoredHeadSHA string     `json:"anchored_head_sha"`
	AnchoredBaseSHA string     `json:"anchored_base_sha,omitempty"`
	// ETASeconds is a held entry's estimated wait: Position x the median
	// line turn. Omitted for the live entry and when the base has no merged
	// history.
	ETASeconds *int64 `json:"eta_seconds,omitempty"`
}

// mergeQueueHeadOfLine is the base's first entry plus its PR's live head.
type mergeQueueHeadOfLine struct {
	RunID           string `json:"run_id"`
	EntryID         string `json:"entry_id"`
	State           string `json:"state"`
	Phase           string `json:"phase,omitempty"`
	AnchoredHeadSHA string `json:"anchored_head_sha"`
	// CurrentHeadSHA is the PR head read from the forge now; when it
	// differs from AnchoredHeadSHA the admission re-anchors before the pass.
	CurrentHeadSHA string `json:"current_head_sha,omitempty"`
	// CurrentHeadUnavailable names why CurrentHeadSHA is absent:
	// run_read_failed, forge_unwired, pull_request_unresolvable or
	// forge_read_failed.
	CurrentHeadUnavailable string `json:"current_head_unavailable,omitempty"`
}

// mergeQueueETABasis is what every eta_seconds on the base is derived from.
type mergeQueueETABasis struct {
	// Samples is how many merged line turns the median read (at most
	// mergeQueueETASamples).
	Samples int `json:"samples"`
	// MedianSeconds is their median, rounded to the second; omitted with no
	// samples.
	MedianSeconds *int64 `json:"median_seconds,omitempty"`
	Note          string `json:"note"`
}

// handleGetMergeQueue implements GET /v0/repos/{owner}/{name}/merge-queue.
//
// Read authorization: the payload names other runs' ids and heads, so a
// run-bound mcp:run:<uuid> token is refused 403 run_token_forbidden
// (isRunBoundSubject). Every other caller must clear scopeMergeQueueRead
// (read:audit) via requireWriteScope: 401 anonymous, 403 insufficient_scope
// for a token missing it; a cookie-session operator passes. The repository is
// a point read (403 repo_forbidden when not visible).
//
// Account scope: the caller identity's AccountID is the Base account, so
// another account's queue on the same repository and base ref is structurally
// unreadable; an untenanted caller reads the untenanted queue. A non-UUID
// account fails closed 500 (callerAccountUUID).
//
// Store reads fail closed (500); the head-of-line forge read is best-effort
// (current_head_unavailable names the degrade, the read answers 200).
func (s *Server) handleGetMergeQueue(w http.ResponseWriter, r *http.Request) {
	if s.cfg.MergeQueue == nil {
		s.writeError(w, r, http.StatusServiceUnavailable, "merge_queue_unconfigured",
			"the merge-queue read requires a configured merge queue", nil)
		return
	}
	if isRunBoundSubject(IdentityFrom(r.Context()).Subject) {
		s.writeError(w, r, http.StatusForbidden, "run_token_forbidden",
			"a run-bound agent token may not read the merge queue; it names other runs and their heads", nil)
		return
	}
	if !s.requireWriteScope(w, r, scopeMergeQueueRead) {
		return
	}
	acct, ok := s.callerAccountUUID(w, r)
	if !ok {
		return
	}
	account := ""
	if acct != nil {
		account = acct.String()
	}
	repo := r.PathValue("owner") + "/" + r.PathValue("name")
	if !s.enforceRepoVisibility(w, r, repo) {
		return
	}
	ctx := r.Context()

	var bases []mergequeue.Base
	if ref := r.URL.Query().Get("base_ref"); ref != "" {
		bases = []mergequeue.Base{{AccountID: account, Repo: repo, BaseRef: ref}}
	} else {
		all, err := s.cfg.MergeQueue.ActiveBases(ctx)
		if err != nil {
			s.writeError(w, r, http.StatusInternalServerError, "internal_error",
				"list merge-queue bases failed", nil)
			return
		}
		for _, b := range all {
			if b.AccountID == account && b.Repo == repo {
				bases = append(bases, b)
			}
		}
	}

	resp := mergeQueueReadResponse{Repo: repo, Bases: make([]mergeQueueBaseView, 0, len(bases))}
	for _, b := range bases {
		view, err := s.mergeQueueBaseViewFor(ctx, b)
		if err != nil {
			s.cfg.Logger.Warn("merge-queue read failed", "repo", repo, "base_ref", b.BaseRef, "error", err.Error())
			s.writeError(w, r, http.StatusInternalServerError, "internal_error",
				"read the merge queue failed", map[string]any{"base_ref": b.BaseRef})
			return
		}
		resp.Bases = append(resp.Bases, view)
	}
	s.writeJSON(w, r, http.StatusOK, resp)
}

// mergeQueueBaseViewFor projects one base: its entries in queue order, the
// ETA basis and the head of line. A store read error is returned.
func (s *Server) mergeQueueBaseViewFor(ctx context.Context, b mergequeue.Base) (mergeQueueBaseView, error) {
	entries, err := s.cfg.MergeQueue.ListForBase(ctx, b)
	if err != nil {
		return mergeQueueBaseView{}, err
	}
	turns, err := s.cfg.MergeQueue.RecentLineTurns(ctx, b, mergeQueueETASamples)
	if err != nil {
		return mergeQueueBaseView{}, err
	}
	view := mergeQueueBaseView{
		BaseRef:  b.BaseRef,
		Entries:  make([]mergeQueueEntryView, 0, len(entries)),
		ETABasis: mergeQueueETABasis{Samples: len(turns), Note: mergeQueueETABasisNote},
	}
	median, haveMedian := medianLineTurnSeconds(turns)
	if haveMedian {
		view.ETABasis.MedianSeconds = &median
	}
	held := 0
	for _, e := range entries {
		ev := mergeQueueEntryView{
			RunID:           e.RunID.String(),
			EntryID:         e.ID.String(),
			State:           string(e.State),
			Phase:           string(e.Phase),
			MergeIntent:     e.MergeIntentAt != nil,
			MergeIntentAt:   e.MergeIntentAt,
			EnqueuedAt:      e.EnqueuedAt,
			AdmittedAt:      e.AdmittedAt,
			AnchoredHeadSHA: e.AnchoredHeadSHA,
			AnchoredBaseSHA: e.AnchoredBaseSHA,
		}
		if e.State == mergequeue.StateHeld {
			held++
			ev.Position = held
			if haveMedian {
				eta := int64(held) * median
				ev.ETASeconds = &eta
			}
		}
		view.Entries = append(view.Entries, ev)
	}
	if len(entries) > 0 {
		view.HeadOfLine = s.mergeQueueHeadOfLineFor(ctx, entries[0])
	}
	return view, nil
}

// medianLineTurnSeconds is the median of turns rounded to the second (the
// mean of the middle two for an even count); false with no samples.
func medianLineTurnSeconds(turns []time.Duration) (int64, bool) {
	if len(turns) == 0 {
		return 0, false
	}
	sorted := slices.Clone(turns)
	slices.Sort(sorted)
	mid := len(sorted) / 2
	m := sorted[mid]
	if len(sorted)%2 == 0 {
		m = (sorted[mid-1] + sorted[mid]) / 2
	}
	return int64(m.Round(time.Second) / time.Second), true
}

// mergeQueueHeadOfLineFor projects the base's first entry and reads its PR's
// live head, best-effort: every uncertainty leaves CurrentHeadSHA empty and
// names the reason in CurrentHeadUnavailable.
func (s *Server) mergeQueueHeadOfLineFor(ctx context.Context, e mergequeue.Entry) *mergeQueueHeadOfLine {
	out := &mergeQueueHeadOfLine{
		RunID:           e.RunID.String(),
		EntryID:         e.ID.String(),
		State:           string(e.State),
		Phase:           string(e.Phase),
		AnchoredHeadSHA: e.AnchoredHeadSHA,
	}
	head, reason := s.mergeQueueCurrentHead(ctx, e)
	out.CurrentHeadSHA, out.CurrentHeadUnavailable = head, reason
	return out
}

// mergeQueueCurrentHead reads the entry's PR head from the forge, or returns
// the degrade reason.
func (s *Server) mergeQueueCurrentHead(ctx context.Context, e mergequeue.Entry) (string, string) {
	if s.cfg.RunRepo == nil {
		return "", mergeQueueHeadRunReadFailed
	}
	runRow, err := s.cfg.RunRepo.GetRun(ctx, e.RunID)
	if err != nil {
		return "", mergeQueueHeadRunReadFailed
	}
	if s.cfg.GitHub == nil || runRow.InstallationID == nil || *runRow.InstallationID == 0 {
		return "", mergeQueueHeadForgeUnwired
	}
	repo, err := parseRepoOwnerName(runRow.Repo)
	prNumber := parsePRNumberFromURL(runRow.PullRequestURL)
	if err != nil || prNumber <= 0 {
		return "", mergeQueueHeadPullRequestUnknown
	}
	pr, err := s.cfg.GitHub.GetPullRequest(ctx, forge.FromGitHubInstallationID(*runRow.InstallationID), repo, prNumber)
	if err == nil && pr.HeadSHA == "" {
		err = errors.New("the forge returned an empty head sha")
	}
	if err != nil {
		s.cfg.Logger.Warn("merge-queue read: head-of-line PR read failed; omitting current_head_sha",
			"run_id", e.RunID.String(), "error", err.Error())
		return "", mergeQueueHeadForgeReadFailed
	}
	return pr.HeadSHA, ""
}
