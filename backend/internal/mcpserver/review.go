package mcpserver

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// ReviewStatus is the lifecycle summary the MCP surface derives from the
// review audit trail for one stage (#600, #664). Status is one of:
//
//   - "none"     — no review was configured (no *_review_started entry).
//   - "pending"  — a review was dispatched (a *_review_started entry exists)
//     but fewer than the configured agent count of terminal entries have
//     landed yet. The round is still running. A reviewer that errors or
//     times out now writes a terminal *_review_failed entry (#664), so
//     "pending" no longer subsumes a silent failure — it means genuinely
//     still-in-flight. Since #1127 "pending" also covers the PARTIAL-LANDING
//     window in the heterogeneous topology: when the first of N configured
//     reviewers has landed but the others have not, the status stays
//     "pending" rather than reporting a half result as "complete".
//   - "complete" — ALL configured agent reviewers have landed a terminal
//     verdict (landed_terminal >= configured_agents, the same completeness
//     rule checkPlanReviewSettled / checkImplementReviewSettled use for the
//     approval/merge gates) AND at least one is a real *_reviewed verdict;
//     Reviews carries one row per configured reviewer — the decoded verdicts
//     (verbatim, incl. approve_with_concerns) plus any synthesized
//     failed/skipped rows.
//   - "skipped"  — a *_review_skipped entry exists (configured agent layer
//     not wired); Reviews carries the synthesized skipped
//     verdict(s).
//   - "failed"   — a terminal *_review_failed entry exists (#664): the
//     reviewer errored or hit FISHHAWKD_PLAN_REVIEW_TIMEOUT; Reviews
//     carries the synthesized failure reason. A definite terminal state,
//     not a bare 'pending'.
//
// Reviews is populated for the complete + skipped + failed states and empty
// for none + pending. It has ONE shape on every status (#2494): the field is
// ALWAYS present and always a non-nil slice, rendering as `[]` on none and
// pending rather than being absent. The prior `omitempty` made a consumer
// handle two shapes — an absent key and an array — for the same field, so a
// naive `len(reviews)` read against a none/pending status hit a missing key
// instead of an empty list.
//
// PollIntervalSeconds is a server-suggested poll cadence (#879): it is
// populated ONLY on the 'pending' status — the one state where a polling
// agent should keep calling fishhawk_get_run_status until a terminal status
// lands — and omitted (zero) on every terminal/none status. Polling
// get_run_status on this cadence is the authoritative way to reach a
// terminal review status; fishhawk_await_review is an optional convenience
// block over the same poll.
type ReviewStatus struct {
	Stage               string       `json:"stage" jsonschema:"the reviewed stage type: 'plan' or 'implement'"`
	Status              string       `json:"status" jsonschema:"one of none, pending, complete, skipped, failed"`
	Reviews             []PlanReview `json:"reviews" jsonschema:"ALWAYS present, one shape on every status: decoded verdicts when status=complete; synthesized skipped verdict(s) when status=skipped; synthesized failure reason when status=failed; an EMPTY array (never absent, never null) for none/pending"`
	PollIntervalSeconds int          `json:"poll_interval_seconds,omitempty" jsonschema:"server-suggested cadence (seconds) for re-polling fishhawk_get_run_status while status=pending; present only on pending, omitted on terminal/none. Poll get_run_status on this cadence as the authoritative path to a terminal status"`
}

// reviewCategories names the three audit categories that describe a stage's
// review lifecycle. The MCP review_status + await semantics derive entirely
// from these — no workflow-spec read is needed because the started entry is
// the backend-emitted proxy for "agent>0 was configured" (#600).
type reviewCategories struct {
	reviewed string
	skipped  string
	started  string
	failed   string
}

// categoriesForStage maps a stage label to its review audit categories.
// Returns an error for any value other than "plan" / "implement" so a bad
// tool input surfaces a clean error before any backend round-trip.
func categoriesForStage(stage string) (reviewCategories, error) {
	switch stage {
	case "plan":
		return reviewCategories{
			reviewed: "plan_reviewed",
			skipped:  "plan_review_skipped",
			started:  "plan_review_started",
			failed:   "plan_review_failed",
		}, nil
	case "implement":
		return reviewCategories{
			reviewed: "implement_reviewed",
			skipped:  "implement_review_skipped",
			started:  "implement_review_started",
			failed:   "implement_review_failed",
		}, nil
	default:
		return reviewCategories{}, fmt.Errorf("stage %q is not one of plan, implement", stage)
	}
}

// reviewAuditQueryLimit caps how many audit entries the review queries
// pull per category. A handful of agents per stage is the realistic
// ceiling; 50 leaves an order-of-magnitude headroom.
const reviewAuditQueryLimit = 50

// decodeReviewVerdicts queries the given *_reviewed category for the run
// and decodes each payload into a PlanReview (the verdict shape is
// identical across plan and implement review, ADR-027). Entries whose
// payload is absent or malformed are silently skipped — a corrupt payload
// is not a reason to fail the whole fetch. Returns nil when no entries
// exist. Shared by loadPlanReviews, loadImplementReviews, and
// reviewStatusFor so the decode lives in one place.
//
// sinceSeq is a fix-up-boundary floor (#894): entries whose audit
// Sequence is <= sinceSeq are dropped before decoding, so a stale
// pre-fix-up verdict is not counted after a fix-up re-opens the stage.
// Callers that want every entry (the plan-reviews / implement_reviews
// listing surfaces) pass sinceSeq == 0; since real and fake audit
// sequences are >= 1, a 0 floor is a no-op and the listing semantics are
// unchanged. Only reviewStatusFor passes a non-zero floor.
func (r *runResolver) decodeReviewVerdicts(ctx context.Context, runID uuid.UUID, category string, sinceSeq int64) ([]PlanReview, error) {
	entries, _, err := r.api.ListRunAudit(ctx, runID, ListRunAuditFilter{
		Category: category,
		Limit:    reviewAuditQueryLimit,
	})
	if err != nil {
		return nil, err
	}
	var reviews []PlanReview
	for _, e := range entries {
		if e.Sequence <= sinceSeq {
			continue
		}
		if e.Payload == nil {
			continue
		}
		raw, merr := json.Marshal(e.Payload)
		if merr != nil {
			continue
		}
		var review PlanReview
		if uerr := json.Unmarshal(raw, &review); uerr != nil {
			continue
		}
		reviews = append(reviews, review)
	}
	return reviews, nil
}

// decodeSkippedReviews queries the given *_review_skipped category and
// synthesizes a PlanReview with verdict "skipped" for each entry (#574).
// Each surfaces the recorded reason/authority so an agent can tell a
// degraded gate from a real verdict without a separate audit query.
//
// sinceSeq is the same fix-up-boundary floor as decodeReviewVerdicts
// (#894): entries with Sequence <= sinceSeq are dropped. The listing
// surfaces pass 0 (no-op); only reviewStatusFor floors to the latest
// fix-up.
func (r *runResolver) decodeSkippedReviews(ctx context.Context, runID uuid.UUID, category string, sinceSeq int64) ([]PlanReview, error) {
	entries, _, err := r.api.ListRunAudit(ctx, runID, ListRunAuditFilter{
		Category: category,
		Limit:    reviewAuditQueryLimit,
	})
	if err != nil {
		return nil, err
	}
	var reviews []PlanReview
	for _, e := range entries {
		if e.Sequence <= sinceSeq {
			continue
		}
		if e.Payload == nil {
			continue
		}
		raw, merr := json.Marshal(e.Payload)
		if merr != nil {
			continue
		}
		var p struct {
			Reason    string `json:"reason"`
			Authority string `json:"authority"`
		}
		if uerr := json.Unmarshal(raw, &p); uerr != nil {
			continue
		}
		reviews = append(reviews, PlanReview{
			ReviewerKind: "agent",
			Authority:    p.Authority,
			Verdict:      "skipped",
			Reason:       p.Reason,
		})
	}
	return reviews, nil
}

// decodeFailedReviews queries the given *_review_failed category and
// synthesizes a PlanReview with verdict "failed" for each entry (#664). A
// failed entry is the terminal record of a reviewer that errored or timed
// out; surfacing it as a definite verdict lets an agent distinguish a real
// failure from a still-running review (which stays 'pending'). Mirrors
// decodeSkippedReviews — same reason/authority projection.
//
// sinceSeq is the same fix-up-boundary floor as decodeReviewVerdicts
// (#894): entries with Sequence <= sinceSeq are dropped, so a pre-fix-up
// failure is not treated as the current round's terminal state.
func (r *runResolver) decodeFailedReviews(ctx context.Context, runID uuid.UUID, category string, sinceSeq int64) ([]PlanReview, error) {
	entries, _, err := r.api.ListRunAudit(ctx, runID, ListRunAuditFilter{
		Category: category,
		Limit:    reviewAuditQueryLimit,
	})
	if err != nil {
		return nil, err
	}
	var reviews []PlanReview
	for _, e := range entries {
		if e.Sequence <= sinceSeq {
			continue
		}
		if e.Payload == nil {
			continue
		}
		raw, merr := json.Marshal(e.Payload)
		if merr != nil {
			continue
		}
		var p struct {
			Reason        string `json:"reason"`
			ReviewerModel string `json:"reviewer_model"`
			Authority     string `json:"authority"`
		}
		if uerr := json.Unmarshal(raw, &p); uerr != nil {
			continue
		}
		reviews = append(reviews, PlanReview{
			ReviewerKind:  "agent",
			ReviewerModel: p.ReviewerModel,
			Authority:     p.Authority,
			Verdict:       "failed",
			Reason:        p.Reason,
		})
	}
	return reviews, nil
}

// decodeLatestStartedConfiguredAgents reads the run's *_review_started entries
// for the given category and returns the ConfiguredAgents count from the entry
// with the HIGHEST audit sequence, plus a bool reporting whether any started
// entry exists (the *_review_started proxy: 'started exists => not none').
//
// Reading the highest-sequence entry is load-bearing for the implement stage:
// a fix-up re-review emits a FRESH implement_review_started, so the latest
// started entry carries the CURRENT round's ConfiguredAgents and pairs with
// the sinceSeq-floored terminal count (#1127). The configured count is the
// completeness threshold reviewStatusFor gates 'complete' on — landed_terminal
// >= configured_agents — mirroring the checkPlanReviewSettled /
// checkImplementReviewSettled approval/merge gates (ADR-036) that already wait
// for the full configured count before resolving.
//
// This supersedes the old hasAuditCategory(started) existence check: the bool
// preserves the same 'started exists' meaning, and a started entry whose
// payload is absent or fails to decode reports configured == 0 so the caller
// degrades to the pre-#1127 complete-on-first-verdict predicate rather than
// stranding on 'pending'.
func (r *runResolver) decodeLatestStartedConfiguredAgents(ctx context.Context, runID uuid.UUID, category string) (int, bool, error) {
	entries, _, err := r.api.ListRunAudit(ctx, runID, ListRunAuditFilter{
		Category: category,
		Limit:    reviewAuditQueryLimit,
	})
	if err != nil {
		return 0, false, err
	}
	var latest *AuditEntry
	for i := range entries {
		if latest == nil || entries[i].Sequence > latest.Sequence {
			latest = &entries[i]
		}
	}
	if latest == nil {
		return 0, false, nil
	}
	if latest.Payload == nil {
		return 0, true, nil
	}
	raw, merr := json.Marshal(latest.Payload)
	if merr != nil {
		return 0, true, nil
	}
	var p struct {
		ConfiguredAgents int `json:"configured_agents"`
	}
	if uerr := json.Unmarshal(raw, &p); uerr != nil {
		return 0, true, nil
	}
	return p.ConfiguredAgents, true, nil
}

// reviewStatusFor derives the ReviewStatus for one stage from the audit
// trail (#600, #664, #894, #1127). Completeness is COUNT-GATED: the round is
// only terminal once landed_terminal >= configured_agents (read from the
// latest *_review_started entry's ConfiguredAgents), mirroring the
// checkPlanReviewSettled / checkImplementReviewSettled approval/merge gates
// (ADR-036). While fewer than the configured count of terminal entries have
// landed the status is 'pending' — even when some terminal entries are
// already present — so a poll catching the partial-landing window in the
// heterogeneous topology (reviewers run sequentially; each takes minutes) no
// longer reports 'complete' with only the first reviewer's verdict (#1127).
// Once the round completes, precedence resolves the status (a real *_reviewed
// verdict => complete; else *_review_skipped => skipped; else *_review_failed
// => failed) and Reviews is the UNION of every decoded terminal row, one per
// configured reviewer. The *_review_failed branch (#664) resolves what used
// to fall through to an ambiguous 'pending' — a reviewer that errored or
// timed out writes a terminal entry, so the await/status surface reports a
// definite 'failed' instead of a still-waiting 'pending'.
//
// Fallback (#1127): an absent or non-positive ConfiguredAgents (a run
// predating the field, or a malformed started payload) degrades to the prior
// complete-on-first-verdict predicate via reviewStatusFallback, never
// stranding on 'pending'.
//
// Re-open boundary (#894, #1201): the three TERMINAL-verdict reads (reviewed /
// skipped / failed) are floored to entries that landed AFTER the latest stage
// re-open audit sequence — stage_fixup_triggered for the implement stage
// (latestImplementFixupSeq, #894), plan_revised for the plan stage
// (latestPlanRevisedSeq, #1201, the plan-stage analog: a fishhawk_revise_plan
// re-opens the plan gate) — so once a stage is re-opened the stale pre-re-open
// verdict no longer reads as terminal. The *_review_started proxy check stays
// UNFLOORED on purpose: the round-1 started entry (at a sequence below the
// boundary) is still present, so 'started exists' remains true and the
// precedence falls through to 'pending' in the window between the re-open and
// the re-review's terminal entry — which is exactly what fishhawk_await_review
// must report while the re-review is in flight, the analogous sibling to the
// #870 stale-input fix. sinceSeq is 0 for an implement stage with no prior
// fix-up and for a plan stage with no prior revise; a 0 floor is a no-op
// (sequences are >= 1), so both the no-fix-up implement path and the no-revise
// plan path are byte-for-byte unchanged.
func (r *runResolver) reviewStatusFor(ctx context.Context, runID uuid.UUID, stage string) (*ReviewStatus, error) {
	round, err := r.loadReviewRound(ctx, runID, stage)
	if err != nil {
		return nil, err
	}
	return r.reviewStatusFromRound(stage, round), nil
}

// reviewRound is ONE read of a stage's current review round: the latest
// started anchor, the re-open floor, and the three floored terminal decodes.
//
// It exists so the status derivation and the #2712 restart-strand probe share
// a SINGLE set of audit reads. Two independent read paths would both cost the
// backend twice per poll tick AND could disagree about which entries belong to
// the round — the exact class of bug the attempt-correlation work closed.
type reviewRound struct {
	Started  startedRound
	SinceSeq int64
	Reviewed []PlanReview
	Skipped  []PlanReview
	Failed   []PlanReview
}

// Landed is the round's terminal count: ANY terminal kind counts, matching
// checkPlanReviewSettled's landed_terminal semantics.
func (rd reviewRound) Landed() int {
	return len(rd.Reviewed) + len(rd.Skipped) + len(rd.Failed)
}

// LandedRows is the union of the decoded terminal rows in the surface's
// precedence order: real verdicts first, then synthesized failed, then skipped.
func (rd reviewRound) LandedRows() []PlanReview {
	union := make([]PlanReview, 0, rd.Landed())
	union = append(union, rd.Reviewed...)
	union = append(union, rd.Failed...)
	union = append(union, rd.Skipped...)
	return union
}

// loadReviewRound performs the round's audit reads. Query shape and count are
// byte-for-byte what reviewStatusFor issued before the #2712 split: one
// re-open-boundary read, three terminal reads, one started read.
func (r *runResolver) loadReviewRound(ctx context.Context, runID uuid.UUID, stage string) (reviewRound, error) {
	var out reviewRound
	cats, err := categoriesForStage(stage)
	if err != nil {
		return out, err
	}

	// Resolve the per-stage round boundary the terminal-verdict reads are
	// floored to: the implement stage is re-opened by a fix-up
	// (stage_fixup_triggered), the plan stage by a revise (plan_revised). When
	// no such entry exists the floor is 0 — a no-op since sequences are >= 1 —
	// so a no-fix-up implement stage and a no-revise plan stage are both
	// byte-for-byte unchanged.
	var sinceSeq int64
	switch stage {
	case "implement":
		sinceSeq, err = r.latestImplementFixupSeq(ctx, runID)
		if err != nil {
			return out, err
		}
	case "plan":
		sinceSeq, err = r.latestPlanRevisedSeq(ctx, runID)
		if err != nil {
			return out, err
		}
	}
	out.SinceSeq = sinceSeq

	out.Reviewed, err = r.decodeReviewVerdicts(ctx, runID, cats.reviewed, sinceSeq)
	if err != nil {
		return out, err
	}
	out.Skipped, err = r.decodeSkippedReviews(ctx, runID, cats.skipped, sinceSeq)
	if err != nil {
		return out, err
	}
	out.Failed, err = r.decodeFailedReviews(ctx, runID, cats.failed, sinceSeq)
	if err != nil {
		return out, err
	}

	// The started anchor carries the round's ConfiguredAgents AND (for the
	// strand probe) its dispatch timestamp; latestStartedRound is
	// decodeLatestStartedConfiguredAgents's superset over the same one query.
	out.Started, err = r.latestStartedRound(ctx, runID, cats.started)
	if err != nil {
		return out, err
	}
	return out, nil
}

// reviewStatusFromRound is the pure status derivation over an already-read
// round. Behavior is unchanged from the pre-#2712 inline form.
func (r *runResolver) reviewStatusFromRound(stage string, rd reviewRound) *ReviewStatus {
	reviewed, skipped, failed := rd.Reviewed, rd.Skipped, rd.Failed
	configured, started := rd.Started.ConfiguredAgents, rd.Started.Exists

	// Fallback (#1127): an absent or non-positive configured count — a run
	// predating the ConfiguredAgents field, or a malformed/undecodable started
	// payload — degrades to the pre-#1127 complete-on-first-verdict predicate
	// so the surface never strands on 'pending'. #664 guarantees a terminal
	// entry per reviewer invocation, so the count-based path below reliably
	// reaches the threshold; this is defense-in-depth, not the normal path.
	if configured <= 0 {
		return r.reviewStatusFallback(stage, reviewed, skipped, failed, started)
	}

	// Count-based completeness (#1127): ANY terminal kind counts toward the
	// round, matching checkPlanReviewSettled's 'landed_terminal' semantics.
	// While fewer than the configured agent count of terminal verdicts have
	// landed the round is still in flight — report 'pending' EVEN when some
	// reviewed/failed/skipped entries are already present, so a poll that
	// catches the partial-landing window in the heterogeneous topology no
	// longer returns 'complete' with only the first reviewer's verdict.
	landed := rd.Landed()
	if landed < configured {
		// Reviews is an explicit empty slice, never nil (#2494): the field has
		// one shape on every status, so a pending result renders "reviews":[].
		return &ReviewStatus{Stage: stage, Status: "pending", Reviews: []PlanReview{},
			PollIntervalSeconds: suggestedReviewPollIntervalSeconds}
	}

	// Round complete: resolve by the existing kind precedence (complete >
	// skipped > failed) but build Reviews as the UNION of all decoded terminal
	// rows — reviewed (verbatim verdicts, incl. approve_with_concerns) then
	// synthesized failed then synthesized skipped — so every configured
	// reviewer is represented by exactly one row at the gate the operator acts
	// on.
	union := rd.LandedRows()
	switch {
	case len(reviewed) > 0:
		return &ReviewStatus{Stage: stage, Status: "complete", Reviews: union}
	case len(skipped) > 0:
		return &ReviewStatus{Stage: stage, Status: "skipped", Reviews: union}
	default:
		return &ReviewStatus{Stage: stage, Status: "failed", Reviews: union}
	}
}

// reviewStatusFallback is the pre-#1127 complete-on-first-verdict predicate,
// reached when the *_review_started entry is absent or carries a non-positive
// ConfiguredAgents (an old/malformed payload). It preserves byte-for-byte the
// behavior for runs predating the count gate: any reviewed => complete, else
// any skipped => skipped, else any failed => failed, else a started entry =>
// pending, else none.
func (*runResolver) reviewStatusFallback(stage string, reviewed, skipped, failed []PlanReview, started bool) *ReviewStatus {
	switch {
	case len(reviewed) > 0:
		return &ReviewStatus{Stage: stage, Status: "complete", Reviews: reviewed}
	case len(skipped) > 0:
		return &ReviewStatus{Stage: stage, Status: "skipped", Reviews: skipped}
	case len(failed) > 0:
		return &ReviewStatus{Stage: stage, Status: "failed", Reviews: failed}
	case started:
		// 'pending' is the one state where a polling agent should keep
		// calling — advertise the server-suggested poll cadence (#879).
		// Reviews is an explicit empty slice, never nil (#2494).
		return &ReviewStatus{Stage: stage, Status: "pending", Reviews: []PlanReview{},
			PollIntervalSeconds: suggestedReviewPollIntervalSeconds}
	default:
		return &ReviewStatus{Stage: stage, Status: "none", Reviews: []PlanReview{}}
	}
}

// latestImplementFixupSeq returns the MAX audit Sequence among the run's
// stage_fixup_triggered entries (0 when none exist), the fix-up boundary
// reviewStatusFor floors the implement stage's terminal-verdict reads to
// (#894). It is RUN-scoped, not stage-scoped, to match reviewStatusFor's
// existing run-scoped audit reads (decodeReviewVerdicts filters by
// runID+category only, with no stage_id); a decomposition run with multiple
// implement stages is out of scope here and unchanged from today's
// run-scoped behavior. Reuses categoryStageFixupTriggered from
// review_action_hint.go.
func (r *runResolver) latestImplementFixupSeq(ctx context.Context, runID uuid.UUID) (int64, error) {
	entries, _, err := r.api.ListRunAudit(ctx, runID, ListRunAuditFilter{
		Category: categoryStageFixupTriggered,
		Limit:    reviewAuditQueryLimit,
	})
	if err != nil {
		return 0, err
	}
	var latestSeq int64
	for _, e := range entries {
		if e.Sequence > latestSeq {
			latestSeq = e.Sequence
		}
	}
	return latestSeq, nil
}

// latestPlanRevisedSeq returns the MAX audit Sequence among the run's
// plan_revised entries (0 when none exist), the plan-revision boundary
// reviewStatusFor floors the plan stage's terminal-verdict reads to (#1201).
// It is the plan-stage analog of latestImplementFixupSeq: a fishhawk_revise_plan
// re-opens the plan gate and writes one plan_revised entry per revise pass, so
// the MAX sequence floors past the LAST revise when multiple have run. RUN-scoped
// to match reviewStatusFor's existing run-scoped audit reads (decodeReviewVerdicts
// filters by runID+category only). Reuses categoryPlanRevised from
// review_action_hint.go.
func (r *runResolver) latestPlanRevisedSeq(ctx context.Context, runID uuid.UUID) (int64, error) {
	entries, _, err := r.api.ListRunAudit(ctx, runID, ListRunAuditFilter{
		Category: categoryPlanRevised,
		Limit:    reviewAuditQueryLimit,
	})
	if err != nil {
		return 0, err
	}
	var latestSeq int64
	for _, e := range entries {
		if e.Sequence > latestSeq {
			latestSeq = e.Sequence
		}
	}
	return latestSeq, nil
}

// startedRound is the CURRENT review round's anchor: the latest
// *_review_started entry's configured-agent count, dispatch timestamp and
// audit sequence. decodeLatestStartedConfiguredAgents reads only the count;
// the strand probe additionally needs the TIMESTAMP (to compare against the
// serving daemon's boot instant) so this reads all three from one query.
type startedRound struct {
	ConfiguredAgents int
	Timestamp        time.Time
	Sequence         int64
	Exists           bool
}

// latestStartedRound reads the run's *_review_started entries for the category
// and returns the highest-sequence one. Mirrors
// decodeLatestStartedConfiguredAgents's latest-wins selection (a fix-up or
// revise appends a fresh started entry, so the latest anchors the CURRENT
// round) and its permissive payload handling: an absent/undecodable payload
// yields ConfiguredAgents 0 with Exists true, which the caller treats as
// not-stranded via the existing #1127 fallback.
func (r *runResolver) latestStartedRound(ctx context.Context, runID uuid.UUID, category string) (startedRound, error) {
	entries, _, err := r.api.ListRunAudit(ctx, runID, ListRunAuditFilter{
		Category: category,
		Limit:    reviewAuditQueryLimit,
	})
	if err != nil {
		return startedRound{}, err
	}
	var latest *AuditEntry
	for i := range entries {
		if latest == nil || entries[i].Sequence > latest.Sequence {
			latest = &entries[i]
		}
	}
	if latest == nil {
		return startedRound{}, nil
	}
	out := startedRound{Exists: true, Timestamp: latest.Timestamp, Sequence: latest.Sequence}
	if latest.Payload == nil {
		return out, nil
	}
	raw, merr := json.Marshal(latest.Payload)
	if merr != nil {
		return out, nil
	}
	var p struct {
		ConfiguredAgents int `json:"configured_agents"`
	}
	if uerr := json.Unmarshal(raw, &p); uerr != nil {
		return out, nil
	}
	out.ConfiguredAgents = p.ConfiguredAgents
	return out, nil
}

// strandProbeCacheTTL bounds how long one resolved /healthz process_start is
// reused WITHIN a single await call.
//
// It is deliberately a short TTL and NOT a per-call cache (#2712 binding
// condition 1). The restart this probe exists to detect is an event that
// happens DURING the wait — an operator lands one campaign item with
// `scripts/dev post-merge` while another item's review is in flight — so a
// boundary sampled once at call start and pinned for the full (up to 7200s)
// window could never see it, and the wait would still burn its window against
// a pre-restart boundary. Tens of seconds keeps the per-tick /healthz cost
// bounded (at the 3s default poll cadence that is one probe per ~10 ticks)
// while making the boundary re-observable mid-wait.
const strandProbeCacheTTL = 30 * time.Second

// healthBoundary is the resolved restart boundary plus WHEN it was resolved
// (for the TTL) and, when it could not be resolved, why.
type healthBoundary struct {
	ProcessStart time.Time
	// OK is true only when /healthz answered AND carried a parseable
	// process_start. False means UNDECIDABLE — never a boundary.
	OK        bool
	Reason    string
	FetchedAt time.Time
}

// resolveHealthBoundary returns the daemon's restart boundary, re-probing
// /healthz when the cached sample is older than the TTL. cache is per-await-call
// state owned by the caller; a nil cache probes every time.
func (r *runResolver) resolveHealthBoundary(ctx context.Context, cache *healthBoundary, now time.Time) healthBoundary {
	ttl := r.strandProbeTTL
	if ttl <= 0 {
		ttl = strandProbeCacheTTL
	}
	if cache != nil && !cache.FetchedAt.IsZero() && now.Sub(cache.FetchedAt) < ttl {
		return *cache
	}
	hb := r.probeHealthBoundary(ctx, now)
	if cache != nil {
		*cache = hb
	}
	return hb
}

// probeHealthBoundary performs ONE /healthz read and classifies it. Every
// failure mode is UNDECIDABLE with a reason — never a zero time.Time presented
// as a boundary, which would compare as before every audit entry and convert
// every pending review into a false strand.
func (r *runResolver) probeHealthBoundary(ctx context.Context, now time.Time) healthBoundary {
	info, err := r.api.Healthz(ctx)
	switch {
	case err != nil:
		return healthBoundary{Reason: fmt.Sprintf("/healthz was unreachable (%v)", err), FetchedAt: now}
	case info == nil:
		return healthBoundary{Reason: "/healthz returned no body", FetchedAt: now}
	case !info.ProcessStartOK:
		return healthBoundary{
			Reason:    "/healthz carries no parseable process_start (a fishhawkd predating #2712)",
			FetchedAt: now,
		}
	default:
		return healthBoundary{ProcessStart: info.ProcessStart, OK: true, FetchedAt: now}
	}
}

// reviewStrand is the verdict of the restart-strand probe for one review round.
//
// Stranded means: the round's reviewers were dispatched by a fishhawkd process
// that is GONE (the started entry predates the serving daemon's boot instant)
// and fewer terminal verdicts landed than were configured — so no further
// verdict can ever land and waiting is futile.
//
// Undecidable means the boundary could not be read at all; the caller must keep
// waiting exactly as it would today. Both flags false means positively NOT
// stranded — the reviewers belong to the serving process.
type reviewStrand struct {
	Stranded         bool
	Undecidable      bool
	Reason           string
	LandedTerminal   int
	ConfiguredAgents int
	// LandedReviewers names the reviewers that DID report, so the surface can
	// state the shortfall concretely ("1 of 2 configured reviewers landed;
	// claude-fable-5 reported, 1 never did") rather than only that the round
	// is stuck — the silent two-reviewers-to-one degradation is half of what
	// makes this failure dangerous (#2712).
	LandedReviewers    []string
	StartedAt          time.Time
	DaemonProcessStart time.Time
}

// MissingReviewers is the shortfall: configured minus landed, never negative.
func (s reviewStrand) MissingReviewers() int {
	if s.ConfiguredAgents <= s.LandedTerminal {
		return 0
	}
	return s.ConfiguredAgents - s.LandedTerminal
}

// reviewerLabel renders one landed terminal row for the shortfall message.
func reviewerLabel(p PlanReview) string {
	switch {
	case p.ReviewerModel != "" && p.Verdict != "":
		return p.ReviewerModel + " (" + p.Verdict + ")"
	case p.ReviewerModel != "":
		return p.ReviewerModel
	case p.Verdict != "":
		return "unnamed reviewer (" + p.Verdict + ")"
	default:
		return "unnamed reviewer"
	}
}

// reviewRoundStrand decides whether the CURRENT review round for the stage was
// orphaned by a fishhawkd restart. It reuses the same per-round primitives
// reviewStatusFor uses — the re-open floor (fix-up / revise), the three floored
// terminal decoders, and the latest-started anchor — so the two can never
// disagree about which entries belong to the round.
//
// Stranded requires ALL of:
//
//	(1) a *_review_started entry exists for the stage;
//	(2) its ConfiguredAgents > 0 (else the #1127 fallback governs, not this);
//	(3) landed terminals for THIS round < ConfiguredAgents;
//	(4) /healthz resolved a parseable process_start;
//	(5) the started entry's timestamp is BEFORE that boot instant.
//
// (4) failing is UNDECIDABLE — the wait continues unchanged. (5) failing is a
// positive NOT-stranded: the reviewers were dispatched by the daemon serving
// this request, so they are genuinely still running.
func (r *runResolver) reviewRoundStrand(ctx context.Context, runID uuid.UUID, stage string, boundary healthBoundary) (*reviewStrand, error) {
	round, err := r.loadReviewRound(ctx, runID, stage)
	if err != nil {
		return nil, err
	}
	return reviewRoundStrandFrom(round, boundary), nil
}

// reviewRoundStrandFrom is the pure strand derivation over an already-read
// round — the form the await loop uses so one set of audit reads feeds both
// the status and the diagnostic.
func reviewRoundStrandFrom(round reviewRound, boundary healthBoundary) *reviewStrand {
	started := round.Started
	out := &reviewStrand{StartedAt: started.Timestamp, ConfiguredAgents: started.ConfiguredAgents}
	if !started.Exists {
		out.Reason = "no review has been dispatched for this stage"
		return out
	}
	if started.ConfiguredAgents <= 0 {
		out.Reason = "the review round records no configured agent count"
		return out
	}

	landed := round.LandedRows()
	out.LandedTerminal = len(landed)
	for _, p := range landed {
		out.LandedReviewers = append(out.LandedReviewers, reviewerLabel(p))
	}
	if out.LandedTerminal >= started.ConfiguredAgents {
		out.Reason = "every configured reviewer has landed a terminal verdict"
		return out
	}

	if !boundary.OK {
		out.Undecidable = true
		out.Reason = boundary.Reason
		return out
	}
	out.DaemonProcessStart = boundary.ProcessStart
	if !started.Timestamp.Before(boundary.ProcessStart) {
		out.Reason = "the review was dispatched by the fishhawkd process now serving this request, so its reviewers are still running"
		return out
	}
	out.Stranded = true
	return out
}

// defaultReviewPollInterval is the fallback poll cadence for
// fishhawk_await_review when the resolver's reviewPollInterval is unset.
// Tests inject a sub-millisecond interval so the poll loop runs without
// wall-clock sleeps.
const defaultReviewPollInterval = 3 * time.Second

// suggestedReviewPollIntervalSeconds is the server-suggested cadence a
// polling agent should use to re-poll fishhawk_get_run_status while a
// review is 'pending' (#879). Advertised on ReviewStatus.PollIntervalSeconds
// (pending only) and on the await tool's pending-after-timeout output so a
// resuming caller stops guessing sleep durations.
const suggestedReviewPollIntervalSeconds = 15

// awaitReviewTimeout bounds. The default is sized to the measured review
// latency (#878): real reviews complete in 3.5–4.5min (4m33s=273s worst
// case across the four cited runs) and the reviewer's own budget
// (FISHHAWKD_PLAN_REVIEW_TIMEOUT) is 300s, so a 360s default exceeds both —
// leaving ~60s headroom for a terminal *_review_failed entry to land within
// the await window. The 600s cap keeps a runaway input from holding the MCP
// session open indefinitely. poll-the-handle (fishhawk_get_run_status) is
// the blessed authoritative path; await is a best-effort, idempotent,
// resumable convenience over it (#879).
const (
	awaitReviewTimeoutDefault = 360
	awaitReviewTimeoutMax     = 600
)

// awaitHeartbeatTimeoutMax is the RAISED timeout cap (7200s / 2h), reachable
// two ways (#1963, #2490): a client-supplied progressToken (whose heartbeat
// resets the client's idle clock), OR the caller-set long_wait input. With a
// full 2h window one call can wait out a full implement pass — the 50-minute
// implement budget plus review rounds and CI-gated merge latency. The
// progressToken path is client metadata a tool-calling agent cannot set (#2490),
// so long_wait exists to make the raised cap REACHABLE from a tool call. The
// tradeoff on the long_wait path is explicit: with no keep-alive the client's
// own idle timeout may still cut the call short — a safe no-op because the wait
// holds no state and is resumable. A call opting into neither keeps the
// unchanged 600s cap via clampAwaitTimeout.
const awaitHeartbeatTimeoutMax = 7200

// AwaitReviewInput is the fishhawk_await_review tool's input schema (#600).
type AwaitReviewInput struct {
	RunID          string `json:"run_id" jsonschema:"the Fishhawk run UUID"`
	Stage          string `json:"stage" jsonschema:"which review to wait on: 'plan' or 'implement'"`
	TimeoutSeconds int    `json:"timeout_seconds,omitempty" jsonschema:"how long to wait before returning 'pending' (default 360). Cap is 600 by default, raised to 7200 when long_wait=true OR your MCP client supplied a progressToken. On timeout the call returns pending + poll_interval_seconds; re-call to resume the wait"`
	// LongWait makes the raised 7200s timeout cap REACHABLE from a tool call
	// (#2490): the progressToken path is client-supplied MCP request metadata an
	// agent cannot set, so this boolean is the caller-settable knob that unlocks
	// the same cap.
	LongWait bool `json:"long_wait,omitempty" jsonschema:"unlock the 7200s timeout cap WITHOUT a progressToken (default false = 600s cap). There is no keep-alive on this path, so your MCP client's own idle timeout may still cut the call short — but the wait holds no state and is resumable, so a cut-short call is a safe no-op to re-issue"`
}

// AwaitReviewOutput is the fishhawk_await_review response. Status mirrors
// ReviewStatus.Status. WaitedSeconds reports the elapsed wall time so the
// caller can see whether it returned immediately or polled. Message is
// populated only on a pending-after-timeout result and names the
// actionable next step.
type AwaitReviewOutput struct {
	Stage         string       `json:"stage"`
	Status        string       `json:"status" jsonschema:"one of none, pending, complete, skipped, failed, stranded"`
	Reviews       []PlanReview `json:"reviews,omitempty" jsonschema:"decoded verdicts when status=complete; synthesized skipped verdict(s) when status=skipped; synthesized failure reason when status=failed"`
	WaitedSeconds float64      `json:"waited_seconds" jsonschema:"elapsed wall time spent waiting"`
	Message       string       `json:"message,omitempty" jsonschema:"actionable explanation when status=pending after the timeout"`
	// PollIntervalSeconds carries the server-suggested poll cadence (#879)
	// on a pending-after-timeout result so a resuming/idempotent re-caller
	// (or an agent switching to fishhawk_get_run_status polling) uses the
	// server cadence rather than guessing. Omitted on a terminal result.
	PollIntervalSeconds int `json:"poll_interval_seconds,omitempty" jsonschema:"server-suggested cadence (seconds) for the resumable re-call or for switching to fishhawk_get_run_status polling; present only on a pending-after-timeout result"`
	// Heartbeat reports whether the CLIENT supplied a progressToken and a
	// per-tick keep-alive was therefore emitted (#2490). It is the operator's
	// evidence of which regime they got: a false value means their client does
	// not supply a progressToken, so the raised cap is reachable only via
	// long_wait. Present on every return path.
	Heartbeat bool `json:"heartbeat" jsonschema:"true when your MCP client supplied a progressToken and a per-tick keep-alive was emitted; false means it did not (use long_wait to reach the raised timeout cap)"`
	// TimeoutCapSeconds is the timeout cap actually applied to this call: 7200
	// when long_wait or a progressToken was in effect, else 600 (#2490).
	// Present on every return path.
	TimeoutCapSeconds int `json:"timeout_cap_seconds" jsonschema:"the timeout cap actually applied to this call (600 by default, 7200 when long_wait or a progressToken was in effect)"`
	// Stranded and the four fields below are the #2712 restart diagnostic. A
	// stranded round can never land another verdict: its reviewers were
	// dispatched by a fishhawkd process that has since restarted, so the wait
	// resolves IMMEDIATELY instead of burning its full window and then
	// claiming the dead reviewer is still running.
	Stranded bool `json:"stranded,omitempty" jsonschema:"true when the round's reviewers were dispatched by a fishhawkd process that has since restarted and fewer than configured_agents verdicts landed — no further verdict can ever land; recover with fishhawk_reconcile_reviews"`
	// Undecidable reports that the restart boundary could not be read, so the
	// wait behaved exactly as it did before this diagnostic existed.
	Undecidable       bool   `json:"undecidable,omitempty" jsonschema:"true when the dispatching daemon could not be verified (/healthz unreachable, or a fishhawkd predating process_start); the wait continued unchanged and a restart cannot be ruled out"`
	LandedTerminal    int    `json:"landed_terminal,omitempty" jsonschema:"how many of the configured reviewers have landed a terminal verdict for the current round"`
	ConfiguredAgents  int    `json:"configured_agents,omitempty" jsonschema:"how many agent reviewers the round was dispatched with; landed_terminal < configured_agents is the partial-landing shortfall"`
	DaemonRestartedAt string `json:"daemon_restarted_at,omitempty" jsonschema:"RFC3339 boot instant of the fishhawkd now serving, when a restart was positively identified as the cause of the strand"`
}

// clampAwaitTimeout applies the default + cap. Non-positive falls back to
// the default (360s — sized to the measured 3.5–4.5min review latency and
// the 300s reviewer budget, #878); values over the cap (600s) clamp down.
func clampAwaitTimeout(n int) int {
	if n <= 0 {
		return awaitReviewTimeoutDefault
	}
	if n > awaitReviewTimeoutMax {
		return awaitReviewTimeoutMax
	}
	return n
}

// effectiveAwaitCap returns the timeout cap that applies to an await call
// (#1963, #2490): the raised awaitHeartbeatTimeoutMax (7200s) when EITHER a
// progressToken keep-alive is present (heartbeat) OR the caller opted in via
// long_wait, and the unchanged awaitReviewTimeoutMax (600s) otherwise. The
// disjunction is the whole point of #2490: the progressToken path is client
// metadata a tool-calling agent cannot set, so long_wait must independently
// unlock the same cap. A call opting into neither keeps today's 600s cap.
func effectiveAwaitCap(heartbeat, longWait bool) int {
	if heartbeat || longWait {
		return awaitHeartbeatTimeoutMax
	}
	return awaitReviewTimeoutMax
}

// clampAwaitTimeoutHeartbeat is the heartbeat/long_wait-aware timeout clamp
// (#1963, #2490). The non-positive -> default (360s) branch is unchanged in
// EVERY regime. A positive value is clamped against effectiveAwaitCap, so it is
// byte-identical to clampAwaitTimeout when neither heartbeat nor long_wait is
// set — keeping token-less await callers and fishhawk_merge_run's two
// clampAwaitTimeout call sites on today's 360 default / 600 cap — and clamps to
// 7200s when either is set, letting one call wait out a full implement pass or
// review round.
func clampAwaitTimeoutHeartbeat(n int, heartbeat, longWait bool) int {
	if n <= 0 {
		return awaitReviewTimeoutDefault
	}
	capSeconds := effectiveAwaitCap(heartbeat, longWait)
	if n > capSeconds {
		return capSeconds
	}
	return n
}

// registerAwaitReview wires the fishhawk_await_review tool (#600). It is
// the ergonomic replacement for curl-polling GET /v0/runs/{id}/audit for a
// plan_reviewed / implement_reviewed entry: the tool blocks until the
// verdict lands (or the review is skipped / was never configured) or the
// timeout elapses. Read-only per ADR-021 — it only polls the audit
// endpoint, server-side, on an injectable interval.
func registerAwaitReview(srv *mcp.Server, resolver *runResolver) {
	mcp.AddTool(srv, &mcp.Tool{
		Name: "fishhawk_await_review",
		Description: strings.TrimSpace(`
OPTIONAL convenience block over polling. fishhawk_get_run_status is the
AUTHORITATIVE source of truth for a review's terminal status — reach for it
FIRST and re-poll on the poll_interval_seconds it advertises while a review
is "pending". This tool just blocks that poll for you when you would rather
wait synchronously than loop yourself.

Resolves the review_status from the audit trail and:

  - Returns immediately when the review is already "complete" (ALL configured
    agent reviewers have landed a terminal verdict), "skipped", "failed", or
    "none" (no review configured).
  - On "pending" (a review was dispatched but the configured reviewers have
    not all landed yet — including the heterogeneous partial-landing window
    where some but not all reviewers have returned) polls the audit endpoint
    until every configured reviewer lands a terminal entry, the run reaches a
    terminal state with NO review in flight (the review can no longer progress —
    it never strands, ADR-036 #874), or the timeout elapses. When the run goes
    terminal WHILE the review is still in flight, the wait keeps polling (#1915):
    a dispatched review's verdict is recorded with no run-state guard, so it WILL
    land — the run flip is derived bookkeeping, not a review gate.

Idempotent / resumable: a timeout returns status "pending" plus
poll_interval_seconds; the wait holds nothing — re-call to resume it, or
switch to fishhawk_get_run_status polling.

Raising the timeout cap (#1963, #2490): the default cap is 600s; it rises to
7200s (2h) so one call can wait out a full implement pass or review round.
There are two ways to reach the raised cap, and only one is settable from a
tool call:

  - long_wait:true — the REACHABLE knob. Set it plus timeout_seconds up to 7200.
    There is no keep-alive on this path, so your MCP client's own idle timeout
    may still cut the call short — that is fine because poll-the-handle is the
    blessed primary path and a cut-short await is a no-op you can re-issue (the
    wait holds no state).
  - a progressToken — this is MCP request metadata
    supplied by your MCP client, not a tool input; you cannot set it from a tool
    call, and whether your client sends one depends on the client
    implementation. When present it also drives an MCP notifications/progress
    keep-alive once per poll tick (stage + elapsed) that resets the client's idle
    clock. The response's heartbeat field reports whether yours did.

WITHOUT either the 600s cap and the resumable-timeout re-arm contract are
unchanged: the default is a long (360s) synchronous call with no keep-alive.

Inputs:
  - run_id          (required) — Fishhawk run UUID.
  - stage           (required) — "plan" or "implement".
  - long_wait       — unlock the 7200s cap from a tool call (default false).
  - timeout_seconds — default 360; cap 600, or 7200 when long_wait=true OR a
                      progressToken is present.

Response: {stage, status, reviews[], waited_seconds, message,
poll_interval_seconds, heartbeat, timeout_cap_seconds}. heartbeat reports
whether your client supplied a progressToken (a per-tick keep-alive was
emitted); timeout_cap_seconds is the cap actually applied. A "failed" status is a definite terminal state: the
reviewer errored or timed out (e.g. it hit FISHHAWKD_PLAN_REVIEW_TIMEOUT)
and a terminal *_review_failed audit entry was written — reviews[] carries
the failure reason. A "pending" status after the timeout means the review is
genuinely STILL RUNNING (no terminal entry yet); re-call to resume, switch
to fishhawk_get_run_status polling on poll_interval_seconds, or check the
fishhawkd logs. If the message reports the run has gone terminal while the
review is in flight, the verdict will still land but the run must be
re-admitted with fishhawk_revive_run to progress past the gate.
`),
	}, resolver.awaitReview)
}

// runStateIsTerminal reports whether a run's state is one past which a
// review can no longer make progress (ADR-036 #874). The terminal set —
// succeeded / failed / cancelled — is compared INLINE here against the
// fishhawk-mcp-local Run.State string (client.go); the backend's run.State
// type and its IsTerminal() method are deliberately NOT imported, as they
// are not available in this package.
func runStateIsTerminal(state string) bool {
	switch state {
	case "succeeded", "failed", "cancelled":
		return true
	default:
		return false
	}
}

// awaitRunTerminalBackstop decides how a pending review's wait resolves when
// the run itself has gone terminal (ADR-036 #874, refined for in-flight reviews
// by #1915). Three outcomes, via the (output, resolved, terminalInFlight)
// return:
//
//   - (output, true, _): resolve the wait NOW — the run is terminal and NO
//     dispatched review is in flight, so no verdict can ever land (the #874
//     non-stranding backstop). The message explains the review can no longer
//     progress.
//   - (zero, false, false): keep polling — the run is not terminal (a GetRun
//     error or a non-terminal run leaves the normal poll/timeout path in
//     charge, byte-identical to before).
//   - (zero, false, true): keep polling, but the run is terminal WITH a review
//     still in flight. A dispatched review's verdict is RECORDED server-side
//     with no run-state guard (runPlanReviews / runImplementReviews and their
//     append loops never gate on IsTerminal, pinned by the server-side plan/
//     trace tests), so a 'pending' review on a terminal run WILL still land its
//     verdict. Resolving early here would abandon a review that is genuinely
//     about to answer. The caller records terminalInFlight so a subsequent
//     timeout names fishhawk_revive_run for re-admitting the run.
//
// review_status 'pending' is the in-flight signal: it implies a *_review_started
// marker exists (#600) with fewer than the configured verdicts landed. The fast-
// path statuses (none/skipped/failed/complete) never reach this backstop — the
// caller only invokes it on 'pending'.
func (r *runResolver) awaitRunTerminalBackstop(ctx context.Context, runID uuid.UUID, stage string, st *ReviewStatus, start time.Time, heartbeat bool, timeoutCap int) (AwaitReviewOutput, bool, bool) {
	runRow, err := r.api.GetRun(ctx, runID)
	if err != nil || runRow == nil {
		return AwaitReviewOutput{}, false, false
	}
	if !runStateIsTerminal(runRow.State) {
		return AwaitReviewOutput{}, false, false
	}
	// Terminal run with a dispatched review still in flight: keep polling and
	// signal terminalInFlight so a timeout names fishhawk_revive_run (#1915).
	if st.Status == "pending" {
		return AwaitReviewOutput{}, false, true
	}
	// Terminal run, no review in flight — resolve early (#874).
	return AwaitReviewOutput{
		Stage:             stage,
		Status:            st.Status,
		Reviews:           st.Reviews,
		WaitedSeconds:     time.Since(start).Seconds(),
		Heartbeat:         heartbeat,
		TimeoutCapSeconds: timeoutCap,
		Message: fmt.Sprintf("%s review is %q and run %s has reached terminal state %q with no review in flight — "+
			"the review can no longer progress, so the wait resolved instead of holding the "+
			"session open. Poll fishhawk_get_run_status for the final run state.",
			stage, st.Status, runID, runRow.State),
	}, true, false
}

// awaitReview is the tool handler.
func (r *runResolver) awaitReview(ctx context.Context, req *mcp.CallToolRequest, in AwaitReviewInput) (*mcp.CallToolResult, AwaitReviewOutput, error) {
	runID, err := uuid.Parse(in.RunID)
	if err != nil {
		return nil, AwaitReviewOutput{}, fmt.Errorf("run_id %q is not a valid UUID: %w", in.RunID, err)
	}
	if _, err := categoriesForStage(in.Stage); err != nil {
		return nil, AwaitReviewOutput{}, err
	}

	// Progress heartbeat (#1963): capture the client-supplied progressToken
	// exactly as drive_run.go does (nil-guarding req and req.Params). A token
	// unlocks the 7200s cap AND a per-tick keep-alive; long_wait unlocks the same
	// cap from a tool call WITHOUT emitting (#2490); neither keeps the
	// byte-identical 360/600 contract. MCP progress is opt-in per spec: no token
	// (or no session) => no emission — long_wait must NOT cause an emission.
	var progToken any
	if req != nil && req.Params != nil {
		progToken = req.Params.GetProgressToken()
	}
	heartbeat := progToken != nil
	capSeconds := effectiveAwaitCap(heartbeat, in.LongWait)
	timeout := clampAwaitTimeoutHeartbeat(in.TimeoutSeconds, heartbeat, in.LongWait)
	start := time.Now()

	// Fast path: terminal / none returns immediately without polling. The
	// round is read ONCE and both the status and the #2712 strand diagnostic
	// are derived from it, so the diagnostic costs no extra audit round-trip.
	round, err := r.loadReviewRound(ctx, runID, in.Stage)
	if err != nil {
		return nil, AwaitReviewOutput{}, fmt.Errorf("review status: %w", err)
	}
	st := r.reviewStatusFromRound(in.Stage, round)
	if st.Status != "pending" {
		return nil, r.awaitTerminalOutput(in.Stage, st, start, heartbeat, capSeconds), nil
	}

	// Pending: poll until a terminal entry lands, the run itself goes
	// terminal WITHOUT a review in flight (the ADR-036 #874 non-stranding
	// backstop), or the deadline fires. Check the run-terminal backstop once
	// before the loop so a run that is already terminal at call time resolves
	// (or, with an in-flight review, is flagged) without a poll tick.
	//
	// terminalInFlight tracks the #1915 case: the run went terminal while the
	// review is still in flight. The verdict is recorded with no run-state
	// guard so it WILL land, so we keep polling — but a subsequent timeout must
	// name fishhawk_revive_run rather than the ordinary still-running message.
	// Terminality is captured HERE (with a live context) rather than re-queried
	// at timeout, where the poll context is already cancelled.
	terminalInFlight := false
	if out, done, tif := r.awaitRunTerminalBackstop(ctx, runID, in.Stage, st, start, heartbeat, capSeconds); done {
		return nil, out, nil
	} else if tif {
		terminalInFlight = true
	}

	// Restart-strand probe (#2712). boundaryCache is PER-CALL state with a
	// bounded TTL, re-probed across ticks: the restart that strands a review
	// typically happens DURING this wait (an operator lands a sibling item
	// with `scripts/dev post-merge` while this review is in flight), so a
	// boundary pinned at call start could never see it. lastStrand carries the
	// most recent verdict into the timeout message.
	var boundaryCache healthBoundary
	lastStrand := reviewRoundStrandFrom(round, r.resolveHealthBoundary(ctx, &boundaryCache, time.Now()))
	if lastStrand.Stranded {
		return nil, r.awaitStrandedOutput(in.Stage, lastStrand, start, heartbeat, capSeconds), nil
	}

	interval := r.reviewPollInterval
	if interval <= 0 {
		interval = defaultReviewPollInterval
	}
	pollCtx, cancel := context.WithTimeout(ctx, time.Duration(timeout)*time.Second)
	defer cancel()
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	var progress float64
	for {
		select {
		case <-pollCtx.Done():
			return nil, r.awaitPendingTimeoutOutput(in.Stage, timeout, start, terminalInFlight, heartbeat, capSeconds, lastStrand), nil
		case <-ticker.C:
			// Best-effort progress heartbeat once per tick (opt-in): keeps a
			// long wait from being aborted by the client's idle timeout. Emitted
			// only when the caller supplied a progressToken AND the request
			// carries a live session. A failed notify is SWALLOWED — the await is
			// authoritative, the heartbeat advisory; a notify error must never
			// terminate or fail the wait.
			if progToken != nil && req != nil && req.Session != nil {
				progress++
				_ = req.Session.NotifyProgress(ctx, &mcp.ProgressNotificationParams{
					ProgressToken: progToken,
					Progress:      progress,
					Message:       awaitReviewProgressMessage(in.Stage, time.Since(start), terminalInFlight),
				})
			}
			round, err := r.loadReviewRound(pollCtx, runID, in.Stage)
			if err != nil {
				// A deadline hit mid-poll cancels the in-flight request;
				// that is a timeout, not a transport failure — return
				// pending rather than surfacing the cancellation as an error.
				if pollCtx.Err() != nil {
					return nil, r.awaitPendingTimeoutOutput(in.Stage, timeout, start, terminalInFlight, heartbeat, capSeconds, lastStrand), nil
				}
				return nil, AwaitReviewOutput{}, fmt.Errorf("poll review status: %w", err)
			}
			st := r.reviewStatusFromRound(in.Stage, round)
			if st.Status != "pending" {
				return nil, r.awaitTerminalOutput(in.Stage, st, start, heartbeat, capSeconds), nil
			}
			// Still pending: re-derive the strand verdict, re-probing the
			// restart boundary on the TTL so a restart landing MID-WAIT is seen
			// by THIS call rather than only by a subsequent one (#2712).
			lastStrand = reviewRoundStrandFrom(round, r.resolveHealthBoundary(pollCtx, &boundaryCache, time.Now()))
			if lastStrand.Stranded {
				return nil, r.awaitStrandedOutput(in.Stage, lastStrand, start, heartbeat, capSeconds), nil
			}
			// Still pending: the review hasn't landed a verdict. If the run
			// itself has gone terminal with NO review in flight the review
			// never will — resolve now (#874). With a review still in flight
			// keep polling (its verdict is recorded unguarded and WILL land)
			// but flag terminalInFlight so a timeout names fishhawk_revive_run
			// (#1915).
			if out, done, tif := r.awaitRunTerminalBackstop(pollCtx, runID, in.Stage, st, start, heartbeat, capSeconds); done {
				return nil, out, nil
			} else if tif {
				terminalInFlight = true
			}
		}
	}
}

// awaitTerminalOutput builds the response for a resolved (non-pending)
// review status.
func (*runResolver) awaitTerminalOutput(stage string, st *ReviewStatus, start time.Time, heartbeat bool, timeoutCap int) AwaitReviewOutput {
	return AwaitReviewOutput{
		Stage:             stage,
		Status:            st.Status,
		Reviews:           st.Reviews,
		WaitedSeconds:     time.Since(start).Seconds(),
		Heartbeat:         heartbeat,
		TimeoutCapSeconds: timeoutCap,
	}
}

// strandShortfall renders the partial-landing shortfall in concrete terms:
// how many of how many configured reviewers reported, WHICH ones did, and how
// many never will. A message that says only "stranded" leaves the more
// dangerous half of #2712 open — an operator who silently went from two
// reviewers to one has a degraded gate, not merely a stuck one.
func strandShortfall(s *reviewStrand) string {
	base := fmt.Sprintf("you heard from %d of %d configured reviewers", s.LandedTerminal, s.ConfiguredAgents)
	if len(s.LandedReviewers) > 0 {
		base += fmt.Sprintf(" (landed: %s)", strings.Join(s.LandedReviewers, ", "))
	}
	return fmt.Sprintf("%s; %d never reported and never will", base, s.MissingReviewers())
}

// awaitStrandedOutput builds the immediate 'stranded' resolution: the round's
// reviewers died with a prior fishhawkd process, so no further verdict can
// land. It names the shortfall concretely, states that the landed verdicts are
// durable, and points at the recovery verb.
func (*runResolver) awaitStrandedOutput(stage string, s *reviewStrand, start time.Time, heartbeat bool, timeoutCap int) AwaitReviewOutput {
	out := AwaitReviewOutput{
		Stage:               stage,
		Status:              "stranded",
		WaitedSeconds:       time.Since(start).Seconds(),
		Heartbeat:           heartbeat,
		TimeoutCapSeconds:   timeoutCap,
		Stranded:            true,
		LandedTerminal:      s.LandedTerminal,
		ConfiguredAgents:    s.ConfiguredAgents,
		PollIntervalSeconds: suggestedReviewPollIntervalSeconds,
	}
	if !s.DaemonProcessStart.IsZero() {
		out.DaemonRestartedAt = s.DaemonProcessStart.UTC().Format(time.RFC3339)
	}
	out.Message = fmt.Sprintf(
		"%s review CANNOT progress: it was dispatched at %s by a fishhawkd process that has since restarted (the daemon now serving booted at %s), so the reviewing goroutine died with that process and no further verdict will ever land. The gate DEGRADED rather than merely stalling — %s. The landed verdict(s) are durable audit entries and are preserved. Recover with fishhawk_reconcile_reviews (run_id=this run): it synthesizes ONLY the missing terminal entries, so nothing already landed is re-paid for, and the review then resolves to a terminal status you can act on.",
		stage,
		s.StartedAt.UTC().Format(time.RFC3339),
		out.DaemonRestartedAt,
		strandShortfall(s),
	)
	return out
}

// awaitPendingTimeoutOutput builds the resumable pending-after-timeout
// response (#879). The wait holds no state, so a timeout is a documented,
// idempotent checkpoint — not an error: the message frames the re-call (or a
// switch to fishhawk_get_run_status polling) as the next step and carries
// the server-suggested poll cadence. Since #664 a reviewer that errors or
// times out writes a terminal *_review_failed entry that resolves to a
// definite 'failed' status, so a lingering 'pending' still means the review
// is genuinely in flight.
//
// terminalInFlight (#1915): when the run went terminal while the review was
// still in flight, the verdict IS recorded server-side (unguarded) and will
// land, but the run must be re-admitted to progress past the gate. That case
// names fishhawk_revive_run instead of the ordinary still-running message. The
// caller captures terminalInFlight during polling (with a live context) rather
// than re-querying here, where the poll context is already cancelled.
// strand (#2712) carries the last restart-boundary verdict the poll loop
// resolved (nil when the probe never produced one). It replaces the old
// UNCONDITIONAL "the review is genuinely still running" assertion — which was
// simply false for an orphaned round — with a claim bounded by what was
// actually verified: a not-stranded verdict THAT ACTUALLY COMPARED THE BOUNDARY
// (DaemonProcessStart non-zero) says the dispatching daemon is the one serving;
// an undecidable verdict says so and names fishhawk_reconcile_reviews as the
// recovery if a restart did happen; and a verdict from one of the early returns
// that never reached the boundary check falls back to the neutral pre-#2712
// wording rather than claiming a verification that never ran.
func (*runResolver) awaitPendingTimeoutOutput(stage string, timeout int, start time.Time, terminalInFlight bool, heartbeat bool, timeoutCap int, strand *reviewStrand) AwaitReviewOutput {
	out := AwaitReviewOutput{
		Stage:               stage,
		Status:              "pending",
		WaitedSeconds:       time.Since(start).Seconds(),
		PollIntervalSeconds: suggestedReviewPollIntervalSeconds,
		Heartbeat:           heartbeat,
		TimeoutCapSeconds:   timeoutCap,
	}
	if strand != nil {
		out.Undecidable = strand.Undecidable
		out.LandedTerminal = strand.LandedTerminal
		out.ConfiguredAgents = strand.ConfiguredAgents
	}
	if terminalInFlight {
		out.Message = fmt.Sprintf("%s review still pending after %ds and the run has reached a terminal state while the "+
			"review is still in flight. The review's verdict is recorded with no run-state guard, so it WILL land — but "+
			"the run must be re-admitted to progress past the gate. Call fishhawk_revive_run to re-park the failed "+
			"stage(s) and flip the run back to running, then re-call fishhawk_await_review or poll fishhawk_get_run_status "+
			"every %ds (the authoritative path).",
			stage, timeout, suggestedReviewPollIntervalSeconds)
		return out
	}
	if strand != nil && strand.Undecidable {
		out.Message = fmt.Sprintf("%s review still pending after %ds (no terminal audit entry yet; a reviewer that errored "+
			"or hit FISHHAWKD_PLAN_REVIEW_TIMEOUT would have resolved to a definite 'failed' status). The dispatching "+
			"daemon could NOT be verified (%s): if fishhawkd restarted since this round was dispatched, its reviewer(s) "+
			"died with that process and no verdict will ever land — %d of %d configured reviewers have reported so far. "+
			"Try fishhawk_reconcile_reviews to synthesize only the missing terminal entries (already-landed verdicts are "+
			"preserved), or re-call fishhawk_await_review / poll fishhawk_get_run_status every %ds (the authoritative path).",
			stage, timeout, strand.Reason, strand.LandedTerminal, strand.ConfiguredAgents, suggestedReviewPollIntervalSeconds)
		return out
	}
	verified := "no terminal audit entry yet; a reviewer that errored or hit FISHHAWKD_PLAN_REVIEW_TIMEOUT would have " +
		"resolved to a definite 'failed' status"
	// The positive liveness claim is gated on EVIDENCE THE BOUNDARY COMPARISON
	// ACTUALLY RAN, not merely on the absence of a stranded/undecidable verdict.
	// reviewRoundStrandFrom returns that same !Stranded && !Undecidable shape on
	// three early returns that never consult /healthz at all — no started entry,
	// ConfiguredAgents <= 0 (a pre-#1127 round, still reachable at the timeout
	// path via reviewStatusFallback), and every-reviewer-landed. Claiming
	// "verified" there would assert a check that was never performed — the same
	// confident-wrong-signal class #2712 exists to eliminate, merely moved to
	// the legacy-round edge. DaemonProcessStart is set ONLY after boundary.OK,
	// so a non-zero value IS the proof the comparison happened.
	if strand != nil && !strand.Stranded && !strand.Undecidable && !strand.DaemonProcessStart.IsZero() {
		verified = "verified: the round was dispatched by the fishhawkd process currently serving, so its reviewer(s) are " +
			"still running rather than orphaned by a restart"
	}
	out.Message = fmt.Sprintf("%s review still pending after %ds — the review is still running (%s). The wait holds "+
		"nothing: re-call fishhawk_await_review to resume it, or poll fishhawk_get_run_status every %ds (the "+
		"authoritative path). Check the fishhawkd logs if this persists.",
		stage, timeout, verified, suggestedReviewPollIntervalSeconds)
	return out
}

// awaitReviewProgressMessage builds the per-tick heartbeat message for a
// pending review wait (#1963): the stage, the pending status, the elapsed
// wall-clock seconds, and — when the run has gone terminal while the review is
// still in flight (#1915) — a short note. Pure (no I/O) so a table test pins
// it.
func awaitReviewProgressMessage(stage string, elapsed time.Duration, terminalInFlight bool) string {
	msg := fmt.Sprintf("await_review: %s review pending; elapsed %ds", stage, int(elapsed.Seconds()))
	if terminalInFlight {
		msg += "; run terminal with review still in flight"
	}
	return msg
}
