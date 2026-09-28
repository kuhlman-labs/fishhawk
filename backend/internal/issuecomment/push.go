package issuecomment

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"math"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/forge"
	"github.com/kuhlman-labs/fishhawk/backend/internal/plan"
	"github.com/kuhlman-labs/fishhawk/backend/internal/pushnotify"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

// Push notification audit categories (#2292). Both are DELIVERY records,
// not run-state activity, so neither enters activityCategories (the anchor
// timeline already renders the underlying page-class event).
const (
	// CategoryPushNotificationSent is the CLAIM row: appended under the
	// per-run claim lock BEFORE the Event reaches the dispatcher. Payload:
	// source_sequence (the dedup key), event, sinks (target sink kinds) and,
	// for a stale reviewer reject recorded without sending, suppressed.
	CategoryPushNotificationSent = "push_notification_sent"
	// CategoryPushNotificationFailed records a delivery that did not land:
	// appended from a dispatcher WORKER per failed sink, or from the request
	// path with sink "*" when the queue was full. Payload: source_sequence,
	// event, sink, error (sanitized), timed_out.
	CategoryPushNotificationFailed = "push_notification_failed"
)

// pushQueueFullReason is the error recorded for a queue-full drop.
const pushQueueFullReason = "queue_full"

// PushDeps groups PushChannel's dependencies.
type PushDeps struct {
	Runs        run.Repository
	Audit       audit.Repository
	ExternalURL string
	Dispatcher  *pushnotify.Dispatcher
	Now         func() time.Time
	Logger      *slog.Logger
}

// PushChannel is the SECOND issuecomment.Channel (ADR-015 / #79, #2292): it
// pushes each page-class event (the same pageClassEvents projection the
// issue-comment pings use) to the operator's configured push sinks. It is a
// RUN surface, not an issue-comment surface, so it fires for CLI- and
// PR-triggered runs too, and it holds no forge client.
//
// On the request path it only DECIDES and CLAIMS: under a per-run lock it
// reads the chain, drops already-claimed source sequences, appends the
// push_notification_sent claim row and only then hands the Event to the
// dispatcher's non-blocking Enqueue. It does no network I/O and always
// returns nil (advisory, never blocking). A failed claim append sends
// nothing (fail toward silence).
type PushChannel struct {
	runs        run.Repository
	audit       audit.Repository
	externalURL string
	dispatcher  *pushnotify.Dispatcher
	now         func() time.Time
	log         *slog.Logger

	locksMu sync.Mutex
	locks   map[uuid.UUID]*pushRunLock

	// afterDedupRead is a test seam invoked between the dedup read and the
	// claim append (the window the claim lock closes). Nil in production.
	afterDedupRead func(runID uuid.UUID)
}

type pushRunLock struct {
	mu   sync.Mutex
	refs int
}

// NewPushChannel returns a PushChannel, or nil when a dependency is missing
// or the dispatcher has no sinks — a nil *PushChannel is a no-op channel.
func NewPushChannel(d PushDeps) *PushChannel {
	if d.Runs == nil || d.Audit == nil || d.Dispatcher == nil || len(d.Dispatcher.SinkNames()) == 0 {
		return nil
	}
	now := d.Now
	if now == nil {
		now = time.Now
	}
	log := d.Logger
	if log == nil {
		log = slog.Default()
	}
	return &PushChannel{
		runs:        d.Runs,
		audit:       d.Audit,
		externalURL: strings.TrimRight(d.ExternalURL, "/"),
		dispatcher:  d.Dispatcher,
		now:         now,
		log:         log,
		locks:       map[uuid.UUID]*pushRunLock{},
	}
}

var _ Channel = (*PushChannel)(nil)

// NotifyStatusUpdateForRun pushes any page-class events the transition crossed.
func (p *PushChannel) NotifyStatusUpdateForRun(ctx context.Context, runID uuid.UUID) error {
	return p.firePushes(ctx, runID)
}

// NotifyPageClassForRun pushes any page-class events the append crossed.
func (p *PushChannel) NotifyPageClassForRun(ctx context.Context, runID uuid.UUID) error {
	return p.firePushes(ctx, runID)
}

// NotifyPlanReady is a no-op: the plan-gate park is pushed through the
// page-class projection on the status/page-class hooks.
func (*PushChannel) NotifyPlanReady(context.Context, uuid.UUID, *run.Stage, *plan.Plan) error {
	return nil
}

// NotifyCIRetry is a no-op: ci_failure is pushed via the page-class projection.
func (*PushChannel) NotifyCIRetry(context.Context, uuid.UUID, uuid.UUID, string, int, int) error {
	return nil
}

// NotifyBudgetAlert is a no-op returning (false, nil): budget alerts do not
// push in v0, so the Router's posted OR and the #758 marker are unchanged.
func (*PushChannel) NotifyBudgetAlert(context.Context, uuid.UUID, BudgetAlertPayload) (bool, error) {
	return false, nil
}

// NotifySlashApprovalReply is a no-op (an issue-thread reply, not a decision).
func (*PushChannel) NotifySlashApprovalReply(context.Context, SlashApprovalReply) error {
	return nil
}

// NotifyRunRejected is a no-op: it fires before any run row exists, so there
// is no chain to claim against.
func (*PushChannel) NotifyRunRejected(context.Context, string, forge.CredentialScope, int, string, string) error {
	return nil
}

// NotifyRunNotApplicable is a no-op for the same reason as NotifyRunRejected.
func (*PushChannel) NotifyRunNotApplicable(context.Context, string, forge.CredentialScope, int, string, string) error {
	return nil
}

// ArtifactListerWired is false: the push channel renders no anchor.
func (*PushChannel) ArtifactListerWired() bool { return false }

// lockRun takes the per-run claim lock and returns its release. Keyed per
// run so two runs never serialize against each other; the entry is dropped
// when its last holder releases.
func (p *PushChannel) lockRun(runID uuid.UUID) func() {
	p.locksMu.Lock()
	l, ok := p.locks[runID]
	if !ok {
		l = &pushRunLock{}
		p.locks[runID] = l
	}
	l.refs++
	p.locksMu.Unlock()
	l.mu.Lock()
	return func() {
		l.mu.Unlock()
		p.locksMu.Lock()
		l.refs--
		if l.refs == 0 {
			delete(p.locks, runID)
		}
		p.locksMu.Unlock()
	}
}

// firePushes decides, claims and enqueues. Always returns nil after logging.
func (p *PushChannel) firePushes(ctx context.Context, runID uuid.UUID) error {
	if p == nil {
		return nil
	}
	unlock := p.lockRun(runID)
	defer unlock()

	runRow, err := p.runs.GetRun(ctx, runID)
	if err != nil {
		p.warn(ctx, "push: get run failed; skipping", runID, err)
		return nil
	}
	stages, err := p.runs.ListStagesForRun(ctx, runID)
	if err != nil {
		p.warn(ctx, "push: list stages failed; skipping", runID, err)
		return nil
	}
	entries, err := p.audit.ListForRun(ctx, runID)
	if err != nil {
		p.warn(ctx, "push: list audit failed; skipping", runID, err)
		return nil
	}
	events := pageClassEvents(entries, stages)
	if len(events) == 0 {
		return nil
	}
	claimed, err := p.claimedSequences(ctx, runID)
	if err != nil {
		// Without the dedup set a send could repeat: fail toward silence.
		p.warn(ctx, "push: load claims failed; skipping", runID, err)
		return nil
	}
	if p.afterDedupRead != nil {
		p.afterDedupRead(runID)
	}

	var latencyCtx *pushnotify.GateLatency
	sinks := p.dispatcher.SinkNames()
	for _, ev := range events {
		if _, done := claimed[ev.sequence]; done {
			continue
		}
		// A reviewer reject the operator already arbitrated is stale: claim
		// it (so it never fires later) without sending, mirroring firePings.
		if pageEventResolved(ev, entries) {
			if err := p.appendClaim(ctx, runID, ev, nil, "resolved"); err != nil {
				p.warn(ctx, "push: claim append failed; not sending", runID, err,
					slog.Int64("source_sequence", ev.sequence))
			}
			continue
		}
		if latencyCtx == nil {
			gl := pushGateLatency(BuildRunEconomics(runRow, entries, nil))
			latencyCtx = &gl
		}
		pe := p.buildEvent(runRow, stages, entries, ev, *latencyCtx)
		// CLAIM FIRST: a failed claim sends nothing.
		if err := p.appendClaim(ctx, runID, ev, sinks, ""); err != nil {
			p.warn(ctx, "push: claim append failed; not sending", runID, err,
				slog.Int64("source_sequence", ev.sequence))
			continue
		}
		if !p.dispatcher.Enqueue(pe) {
			p.log.WarnContext(ctx, "push: dispatcher queue full; notification dropped",
				slog.String("run_id", runID.String()),
				slog.Int64("source_sequence", ev.sequence),
				slog.String("event", ev.kind))
			appendPushFailure(ctx, p.audit, p.now, p.log, runID, ev.sequence, ev.kind, "*", pushQueueFullReason, false)
		}
	}
	return nil
}

func (p *PushChannel) warn(ctx context.Context, msg string, runID uuid.UUID, err error, attrs ...slog.Attr) {
	a := append([]slog.Attr{slog.String("run_id", runID.String()), slog.String("error", err.Error())}, attrs...)
	p.log.LogAttrs(ctx, slog.LevelWarn, msg, a...)
}

// claimedSequences returns the source sequences already claimed.
func (p *PushChannel) claimedSequences(ctx context.Context, runID uuid.UUID) (map[int64]struct{}, error) {
	rows, err := p.audit.ListForRunByCategory(ctx, runID, CategoryPushNotificationSent)
	if err != nil {
		return nil, err
	}
	seen := make(map[int64]struct{}, len(rows))
	for _, e := range rows {
		if seq := pingSourceSequence(e.Payload); seq > 0 {
			seen[seq] = struct{}{}
		}
	}
	return seen, nil
}

func (p *PushChannel) appendClaim(ctx context.Context, runID uuid.UUID, ev pageEvent, sinks []string, suppressed string) error {
	if sinks == nil {
		sinks = []string{}
	}
	body := map[string]any{
		"source_sequence": ev.sequence,
		"event":           ev.kind,
		"sinks":           sinks,
	}
	if suppressed != "" {
		body["suppressed"] = suppressed
	}
	payload, _ := json.Marshal(body)
	systemKind := audit.ActorSystem
	_, err := p.audit.AppendChained(ctx, audit.ChainAppendParams{
		RunID:     runID,
		Timestamp: p.now().UTC(),
		Category:  CategoryPushNotificationSent,
		ActorKind: &systemKind,
		Payload:   payload,
	})
	return err
}

// PushOutcomeRecorder returns the dispatcher OutcomeFunc that appends one
// push_notification_failed row per FAILED sink. It runs on a dispatcher
// worker under the worker's detached context. o.Err is always a sanitized
// *pushnotify.DeliveryError, so its text carries sink, host and a classified
// reason only — never a URL, path, query or credential.
func PushOutcomeRecorder(auditRepo audit.Repository, now func() time.Time, log *slog.Logger) pushnotify.OutcomeFunc {
	if now == nil {
		now = time.Now
	}
	if log == nil {
		log = slog.Default()
	}
	return func(ctx context.Context, o pushnotify.Outcome) {
		if o.Err == nil || auditRepo == nil {
			return
		}
		runID, err := uuid.Parse(o.Event.RunID)
		if err != nil {
			return
		}
		appendPushFailure(ctx, auditRepo, now, log, runID, o.Event.SourceSequence, o.Event.Event, o.Sink, o.Err.Error(), o.TimedOut)
	}
}

func appendPushFailure(ctx context.Context, auditRepo audit.Repository, now func() time.Time, log *slog.Logger,
	runID uuid.UUID, seq int64, kind, sink, reason string, timedOut bool) {
	payload, _ := json.Marshal(map[string]any{
		"source_sequence": seq,
		"event":           kind,
		"sink":            sink,
		"error":           reason,
		"timed_out":       timedOut,
	})
	systemKind := audit.ActorSystem
	if _, err := auditRepo.AppendChained(ctx, audit.ChainAppendParams{
		RunID:     runID,
		Timestamp: now().UTC(),
		Category:  CategoryPushNotificationFailed,
		ActorKind: &systemKind,
		Payload:   payload,
	}); err != nil {
		log.WarnContext(ctx, "push: failure-row append failed",
			slog.String("run_id", runID.String()),
			slog.Int64("source_sequence", seq),
			slog.String("sink", sink),
			slog.String("error", err.Error()))
	}
}

// buildEvent assembles the one-screen decision payload for ev.
func (p *PushChannel) buildEvent(runRow *run.Run, stages []*run.Stage, entries []*audit.Entry, ev pageEvent, gl pushnotify.GateLatency) pushnotify.Event {
	out := pushnotify.Event{
		SchemaVersion:  pushnotify.SchemaVersion,
		Event:          ev.kind,
		SourceSequence: ev.sequence,
		RunID:          runRow.ID.String(),
		RunShortID:     shortID(runRow.ID),
		Repo:           runRow.Repo,
		WorkflowID:     runRow.WorkflowID,
		Decision:       pushDecision(ev),
		Verdicts:       pushVerdicts(ev, entries),
		GateLatency:    gl,
		Links:          pushnotify.Links{Run: runURLFor(p.externalURL, runRow.ID)},
	}
	src := entryBySequence(entries, ev.sequence)
	if src != nil {
		out.OccurredAt = src.Timestamp.UTC()
		if src.StageID != nil {
			for _, s := range stages {
				if s.ID == *src.StageID {
					out.Stage = &pushnotify.Stage{Type: string(s.Type), State: string(s.State)}
					break
				}
			}
		}
	}
	if out.OccurredAt.IsZero() {
		out.OccurredAt = p.now().UTC()
	}
	if runRow.IsIssueAnchored() && runRow.TriggerRef != nil {
		if n, ok := parseIssueRef(*runRow.TriggerRef); ok {
			out.Issue = &pushnotify.Issue{Number: n}
			if commentForgeFamily(runRow) == commentFamilyGitHub && runRow.Repo != "" {
				out.Issue.URL = fmt.Sprintf("https://github.com/%s/issues/%d", runRow.Repo, n)
				out.Links.Issue = out.Issue.URL
			}
		}
	}
	if runRow.PullRequestURL != nil {
		out.Links.PullRequest = *runRow.PullRequestURL
	}
	return out
}

func entryBySequence(entries []*audit.Entry, seq int64) *audit.Entry {
	for _, e := range entries {
		if e.Sequence == seq {
			return e
		}
	}
	return nil
}

// pushDecision is the one-line human-readable ask: the ping message with its
// leading glyph and trailing period trimmed (the reject pings already name
// the flagging reviewer model).
func pushDecision(ev pageEvent) string {
	m := strings.TrimLeftFunc(ev.message, func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	})
	return strings.TrimSuffix(strings.TrimSpace(m), ".")
}

// pushVerdicts returns the reviewer verdicts for a review-derived event: the
// <stage>_reviewed entries of the round the event belongs to. Empty for any
// other event.
func pushVerdicts(ev pageEvent, entries []*audit.Entry) []pushnotify.Verdict {
	var category string
	var from, to int64
	switch ev.kind {
	case "plan_awaiting_approval":
		category, from, to = "plan_reviewed", ev.sequence, math.MaxInt64
	case "plan_review_rejected":
		category, to = "plan_reviewed", ev.sequence
		from = latestSequenceBefore(entries, ev.sequence, "plan_generated")
	case "implement_review_rejected":
		category, to = "implement_reviewed", ev.sequence
		from = latestSequenceBefore(entries, ev.sequence, "stage_fixup_triggered")
	default:
		return []pushnotify.Verdict{}
	}
	out := []pushnotify.Verdict{}
	for _, e := range entries {
		if e.Category != category || e.Sequence <= from || e.Sequence > to {
			continue
		}
		verdict, model := decodeReviewerVerdict(e.Payload)
		if verdict == "" {
			continue
		}
		out = append(out, pushnotify.Verdict{ReviewerModel: model, Verdict: verdict})
	}
	return out
}

func latestSequenceBefore(entries []*audit.Entry, before int64, category string) int64 {
	var seq int64
	for _, e := range entries {
		if e.Category == category && e.Sequence < before && e.Sequence > seq {
			seq = e.Sequence
		}
	}
	return seq
}

// pushGateLatency projects BuildRunEconomics' gate-latency rollup (the
// existing latency.AggregateGateLatency fold — no parallel timing path) onto
// the payload's whole-second shape.
func pushGateLatency(in *EconomicsInput) pushnotify.GateLatency {
	out := pushnotify.GateLatency{Gates: []pushnotify.GateWait{}}
	if in == nil {
		return out
	}
	out.TotalWaitOnHumanSeconds = int64(math.Round(in.Latency.TotalWaitOnHumanSeconds))
	for _, g := range in.Latency.Gates {
		out.Gates = append(out.Gates, pushnotify.GateWait{Gate: g.Gate, WaitSeconds: int64(math.Round(g.WaitSeconds))})
	}
	return out
}
