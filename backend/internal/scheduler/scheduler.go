// Package scheduler is the in-process producer of the `scheduled` trigger
// form (E79.1 / #3725). A workflow declaring a workflow-level `schedule`
// (spec.Schedule: a five-field cron, an IANA timezone and an optional anchor
// issue) gets exactly one run per due window.
//
// It reuses the established background-worker shape —
// backend/internal/campaigndriver.Ticker: an exported Tick(ctx) one-pass
// method driven by a Run(ctx) interval loop, an off-by-default
// --enable-scheduler flag, and a fail-closed start decision in serve.go that
// refuses to start when a required dependency is unwired.
//
// Per tick, for each configured repository the ticker fetches the spec
// (SpecSource), parses it, and for each workflow WITH a schedule computes the
// due window — the latest cron fire at or before now (CronSchedule.Prev) —
// and, when this process has not yet attempted that window, starts it through
// the RunStarter seam with the Idempotency-Key IdempotencyKey(workflow,
// window). The outcome is appended to the GLOBAL audit chain as one of
// CategoryScheduledRunStarted / CategoryScheduledRunSkipped /
// CategoryScheduledRunRefused. A transient starter error is log-only and is
// retried on the next tick.
//
// Exactly-once per (repo, workflow, window) does NOT rest on this package's
// in-memory attempted mark: that mark only suppresses a per-tick audit flood.
// The run row's exactly-once guarantee is the (idempotency_key, repo) unique
// index behind POST /v0/runs' Idempotency-Key replay, which a restarted
// process (with an empty mark) reaches and which answers already_started.
//
// The package never imports backend/internal/server: the RunStarter,
// SpecSource and AuditAppender seams are defined HERE and bound by serve.go
// adapters, so the ticker is unit-testable with recording fakes. See
// README.md for the full contract.
package scheduler

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"sort"
	"sync"
	"time"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
	"github.com/kuhlman-labs/fishhawk/backend/internal/spec"
)

// DefaultInterval is the tick period applied when Interval is zero. Matches
// the other background workers' 60s cadence; a cron's finest grain is one
// minute, so a shorter interval buys nothing.
const DefaultInterval = 60 * time.Second

// Audit categories the scheduler emits. They ride the GLOBAL chain
// (AppendGlobalChained) because a refusal has no run to chain on; the run
// linkage of a started/skipped window travels in the payload's run_id field.
// Registered in backend/internal/audit/categories.go.
const (
	// CategoryScheduledRunStarted records a due window whose start created a
	// new run.
	CategoryScheduledRunStarted = "scheduled_run_started"
	// CategoryScheduledRunSkipped records a due window whose start replayed
	// an existing run (the Idempotency-Key matched: a restart, or a second
	// instance, already started this window).
	CategoryScheduledRunSkipped = "scheduled_run_skipped"
	// CategoryScheduledRunRefused records a due window whose start was
	// refused by an admission control (e.g. budget_exhausted, the applies_to
	// or charter gates), carrying the refusal's code, message and status.
	CategoryScheduledRunRefused = "scheduled_run_refused"
)

// ActorSubject stamps every scheduler audit entry (ActorKind system).
const ActorSubject = "fishhawkd/scheduler"

// SkipReasonAlreadyStarted is the scheduled_run_skipped payload's reason.
const SkipReasonAlreadyStarted = "already_started"

// DispatchNoteLocal is carried on audit entries and snapshots when the
// scheduler starts runs with runner_kind local: nothing auto-dispatches a
// local run, so it parks until a host picks it up.
const DispatchNoteLocal = "runner_kind local: a started run parks at awaiting_host_dispatch until a host dispatches it (fishhawk_dispatch_stage / fishhawk_run_stage); the scheduler does not auto-dispatch"

// OutcomeKind classifies a RunStarter result.
type OutcomeKind string

const (
	// OutcomeStarted means a new run was created (POST /v0/runs 201).
	OutcomeStarted OutcomeKind = "started"
	// OutcomeAlreadyStarted means the Idempotency-Key replayed an existing
	// run (POST /v0/runs 200).
	OutcomeAlreadyStarted OutcomeKind = "already_started"
	// OutcomeRefused means an admission control refused the run (a 4xx):
	// a definitive answer for that window.
	OutcomeRefused OutcomeKind = "refused"
	// OutcomeTransientError is snapshot-only: the starter returned an error
	// (a 5xx or a transport failure). It is never audited and the window is
	// retried on the next tick.
	OutcomeTransientError OutcomeKind = "transient_error"
)

// StartRequest is what the scheduler asks the RunStarter to start. The
// serve.go adapter translates it to server.ScheduledRunParams.
type StartRequest struct {
	Repo        string
	WorkflowID  string
	WorkflowSHA string
	// WorkflowSpec is the raw spec document the window was computed from,
	// handed to the starter so the run is admitted against exactly the
	// bytes the scheduler read.
	WorkflowSpec   []byte
	IdempotencyKey string
	// IssueNumber is the schedule's optional anchor issue (0 = none).
	IssueNumber int
	RunnerKind  string
}

// StartOutcome is a definitive RunStarter result. A transient failure is NOT
// an outcome: it is the starter's error return.
type StartOutcome struct {
	Kind OutcomeKind
	// RunID is set for OutcomeStarted and OutcomeAlreadyStarted.
	RunID string
	// Code, Message and Status are set for OutcomeRefused: the admission
	// error code (e.g. budget_exhausted), its message and the HTTP status.
	Code    string
	Message string
	Status  int
}

// RunStarter starts one scheduled run. Satisfied in serve.go by an adapter
// over server.Server.StartScheduledRun (which drives handleCreateRun
// in-process, so every admission control applies); unit tests substitute a
// recording fake. Defined HERE so the scheduler never imports server.
//
// Contract: a nil error means the outcome is definitive for the window and
// the scheduler records it and marks the window attempted; a non-nil error
// is TRANSIENT and the window is retried on the next tick.
type RunStarter interface {
	StartScheduledRun(ctx context.Context, req StartRequest) (StartOutcome, error)
}

// SpecSource fetches a repository's workflow spec. Satisfied in serve.go by
// an adapter over the GitHub App (GetRepoInstallation + GetWorkflowSpec at
// the default branch). sha is the fetched blob SHA, used as the run's
// workflow_sha.
type SpecSource interface {
	FetchSpec(ctx context.Context, repo string) (content []byte, sha string, err error)
}

// AuditAppender records the scheduler's outcome entries on the global chain.
// Satisfied by audit.Repository.
type AuditAppender interface {
	AppendGlobalChained(ctx context.Context, p audit.GlobalChainAppendParams) (*audit.Entry, error)
}

// Ticker starts each scheduled workflow once per due window. Run() blocks
// until ctx is cancelled. Specs, Starter and Audit are required and Repos
// must be non-empty; a missing one is a configuration error caught by Run()
// and makes Tick() a logged no-op (fail-closed).
type Ticker struct {
	// Repos are the owner/name repositories whose specs are scanned.
	Repos   []string
	Specs   SpecSource
	Starter RunStarter
	Audit   AuditAppender

	// RunnerKind is the runner_kind every scheduled run is created with
	// (run.RunnerKindGitHubActions or run.RunnerKindLocal). serve.go
	// validates it at startup.
	RunnerKind string

	// Interval is the tick period. Defaults to DefaultInterval when zero.
	Interval time.Duration

	// Now sources the current time. nil → time.Now.
	Now func() time.Time

	// Logger receives structured warnings. nil → slog.Default().
	Logger *slog.Logger

	mu sync.Mutex
	// attempted holds, per (repo, workflow), the latest window this process
	// recorded a definitive outcome for. Only the latest window is ever
	// attempted, so one entry per workflow bounds the memory.
	attempted map[workflowKey]time.Time
	// repos is the per-repository observation state Snapshot reads.
	repos map[string]*repoState
}

type workflowKey struct{ repo, workflow string }

type repoState struct {
	scanned    bool
	lastTickAt time.Time
	specError  string
	workflows  map[string]*workflowState
}

type workflowState struct {
	cron, timezone     string
	issue              int
	currentWindowStart time.Time
	nextDueAt          time.Time
	scheduleError      string
	lastOutcome        *LastOutcome
}

// LastOutcome is the most recent result the scheduler observed for one
// workflow.
type LastOutcome struct {
	Kind        OutcomeKind
	WindowStart time.Time
	RunID       string
	Code        string
	Message     string
	At          time.Time
}

// WorkflowSnapshot is one scheduled workflow's state.
type WorkflowSnapshot struct {
	WorkflowID string
	Cron       string
	Timezone   string
	Issue      int
	// CurrentWindowStart is the latest fire at or before the last tick
	// (zero when the schedule has not fired yet).
	CurrentWindowStart time.Time
	// NextDueAt is the first fire strictly after the last tick.
	NextDueAt time.Time
	// ScheduleError is set when the declared schedule failed to parse at
	// tick time (validation normally refuses such a spec first).
	ScheduleError string
	// LastOutcome is nil until the scheduler attempted a window.
	LastOutcome *LastOutcome
}

// RepoSnapshot is one configured repository's scheduler state.
type RepoSnapshot struct {
	Repo string
	// Scanned is false until the first tick reached this repository.
	Scanned    bool
	LastTickAt time.Time
	// SpecError is set when the last tick could not fetch or parse the
	// spec; Schedules then holds the last successfully observed state.
	SpecError    string
	RunnerKind   string
	DispatchNote string
	Schedules    []WorkflowSnapshot
}

// IdempotencyKey derives the Idempotency-Key for one (workflow, window):
// `scheduled:<workflow_id>:<window start UTC RFC3339>`. Combined with the
// repository by the (idempotency_key, repo) unique index, it makes the run
// row exactly-once per (repo, workflow, window) across restarts and racing
// ticks. The UTC form keeps the two fall-back occurrences of a repeated local
// hour distinct (two instants, two windows).
func IdempotencyKey(workflowID string, window time.Time) string {
	return "scheduled:" + workflowID + ":" + window.UTC().Format(time.RFC3339)
}

// Run drives the ticker until ctx is cancelled. Per-repo and per-workflow
// errors log but never abort the loop.
func (t *Ticker) Run(ctx context.Context) error {
	if err := t.checkDeps(); err != nil {
		return err
	}
	interval := t.Interval
	if interval <= 0 {
		interval = DefaultInterval
	}

	// Fire once at startup so a window that fell due while fishhawkd was
	// down is started without waiting a full interval.
	t.Tick(ctx)

	tick := time.NewTicker(interval)
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

var errMissingDeps = errors.New("scheduler: Specs, Starter and Audit must all be set and Repos must be non-empty")

func (t *Ticker) checkDeps() error {
	if t.Specs == nil || t.Starter == nil || t.Audit == nil || len(t.Repos) == 0 {
		return errMissingDeps
	}
	return nil
}

// Tick performs one pass over the configured repositories. Exported so tests
// drive a single deterministic pass. A missing dependency makes Tick a logged
// no-op that starts no run.
func (t *Ticker) Tick(ctx context.Context) {
	logger := t.logger()
	if err := t.checkDeps(); err != nil {
		logger.LogAttrs(ctx, slog.LevelWarn, "scheduler: tick skipped; "+err.Error())
		return
	}
	now := t.now()
	for _, repo := range t.Repos {
		t.tickRepo(ctx, logger, repo, now)
	}
}

func (t *Ticker) tickRepo(ctx context.Context, logger *slog.Logger, repo string, now time.Time) {
	content, sha, err := t.Specs.FetchSpec(ctx, repo)
	var parsed *spec.Spec
	if err == nil {
		parsed, err = spec.ParseBytes(content)
	}
	if err != nil {
		// A fetch or parse failure is recorded for visibility and audits
		// nothing: there is no window decision to record.
		logger.LogAttrs(ctx, slog.LevelWarn, "scheduler: spec unavailable; repository skipped this tick",
			slog.String("repo", repo),
			slog.String("error", err.Error()))
		t.withRepo(repo, func(rs *repoState) {
			rs.scanned = true
			rs.lastTickAt = now
			rs.specError = err.Error()
		})
		return
	}

	ids := make([]string, 0, len(parsed.Workflows))
	for id, wf := range parsed.Workflows {
		if wf.Schedule != nil {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)

	t.withRepo(repo, func(rs *repoState) {
		rs.scanned = true
		rs.lastTickAt = now
		rs.specError = ""
		// Drop workflows whose schedule was removed from the spec.
		keep := make(map[string]*workflowState, len(ids))
		for _, id := range ids {
			if ws, ok := rs.workflows[id]; ok {
				keep[id] = ws
			}
		}
		rs.workflows = keep
	})

	for _, id := range ids {
		t.tickWorkflow(ctx, logger, repo, id, *parsed.Workflows[id].Schedule, content, sha, now)
	}
}

func (t *Ticker) tickWorkflow(ctx context.Context, logger *slog.Logger, repo, workflowID string, sched spec.Schedule, content []byte, sha string, now time.Time) {
	cron, err := spec.ParseSchedule(sched)
	if err != nil {
		// Validation refuses such a spec before it reaches here; this is
		// defence in depth, recorded for visibility and never audited.
		logger.LogAttrs(ctx, slog.LevelWarn, "scheduler: schedule does not parse; workflow skipped",
			slog.String("repo", repo),
			slog.String("workflow_id", workflowID),
			slog.String("error", err.Error()))
		t.withWorkflow(repo, workflowID, func(ws *workflowState) {
			ws.cron, ws.timezone, ws.issue = sched.Cron, sched.EffectiveTimezone(), sched.Issue
			ws.currentWindowStart, ws.nextDueAt = time.Time{}, time.Time{}
			ws.scheduleError = err.Error()
		})
		return
	}

	window := cron.Prev(now)
	next := cron.Next(now)
	t.withWorkflow(repo, workflowID, func(ws *workflowState) {
		ws.cron, ws.timezone, ws.issue = sched.Cron, sched.EffectiveTimezone(), sched.Issue
		ws.currentWindowStart, ws.nextDueAt = window, next
		ws.scheduleError = ""
	})
	if window.IsZero() {
		// The schedule has not fired yet within the search horizon.
		return
	}
	if t.alreadyAttempted(repo, workflowID, window) {
		return
	}

	key := IdempotencyKey(workflowID, window)
	out, err := t.Starter.StartScheduledRun(ctx, StartRequest{
		Repo:           repo,
		WorkflowID:     workflowID,
		WorkflowSHA:    sha,
		WorkflowSpec:   content,
		IdempotencyKey: key,
		IssueNumber:    sched.Issue,
		RunnerKind:     t.RunnerKind,
	})
	if err != nil {
		// TRANSIENT: log-only, and the window is deliberately NOT marked
		// attempted, so the next tick retries it.
		logger.LogAttrs(ctx, slog.LevelWarn, "scheduler: start failed transiently; retrying next tick",
			slog.String("repo", repo),
			slog.String("workflow_id", workflowID),
			slog.String("window_start", window.UTC().Format(time.RFC3339)),
			slog.String("error", err.Error()))
		t.withWorkflow(repo, workflowID, func(ws *workflowState) {
			ws.lastOutcome = &LastOutcome{Kind: OutcomeTransientError, WindowStart: window, Message: err.Error(), At: now}
		})
		return
	}

	category, ok := categoryFor(out.Kind)
	if !ok {
		// An unknown outcome kind is a starter-contract violation; treat it
		// like a transient failure rather than guess what happened.
		logger.LogAttrs(ctx, slog.LevelWarn, "scheduler: starter returned an unknown outcome kind; retrying next tick",
			slog.String("repo", repo),
			slog.String("workflow_id", workflowID),
			slog.String("kind", string(out.Kind)))
		return
	}

	payload := map[string]any{
		"repo":            repo,
		"workflow_id":     workflowID,
		"window_start":    window.UTC().Format(time.RFC3339),
		"idempotency_key": key,
		"runner_kind":     t.RunnerKind,
	}
	if t.RunnerKind == run.RunnerKindLocal {
		payload["dispatch_note"] = DispatchNoteLocal
	}
	switch out.Kind {
	case OutcomeStarted:
		payload["run_id"] = out.RunID
	case OutcomeAlreadyStarted:
		payload["run_id"] = out.RunID
		payload["reason"] = SkipReasonAlreadyStarted
	case OutcomeRefused:
		payload["code"] = out.Code
		payload["message"] = out.Message
		payload["status"] = out.Status
	}
	t.emit(ctx, logger, category, payload, now)
	t.markAttempted(repo, workflowID, window)
	t.withWorkflow(repo, workflowID, func(ws *workflowState) {
		ws.lastOutcome = &LastOutcome{
			Kind:        out.Kind,
			WindowStart: window,
			RunID:       out.RunID,
			Code:        out.Code,
			Message:     out.Message,
			At:          now,
		}
	})
}

func categoryFor(k OutcomeKind) (string, bool) {
	switch k {
	case OutcomeStarted:
		return CategoryScheduledRunStarted, true
	case OutcomeAlreadyStarted:
		return CategoryScheduledRunSkipped, true
	case OutcomeRefused:
		return CategoryScheduledRunRefused, true
	}
	return "", false
}

// emit appends one outcome entry on the global chain. Best-effort, like the
// campaign driver's emit: a marshal or append failure WARN-logs and does not
// un-decide the window (re-attempting would only replay the same run).
func (t *Ticker) emit(ctx context.Context, logger *slog.Logger, category string, payload map[string]any, now time.Time) {
	body, err := json.Marshal(payload)
	if err != nil {
		logger.LogAttrs(ctx, slog.LevelWarn, "scheduler: marshal audit payload failed",
			slog.String("category", category),
			slog.String("error", err.Error()))
		return
	}
	systemKind := audit.ActorSystem
	subject := ActorSubject
	if _, err := t.Audit.AppendGlobalChained(ctx, audit.GlobalChainAppendParams{
		Timestamp:    now,
		Category:     category,
		ActorKind:    &systemKind,
		ActorSubject: &subject,
		Payload:      body,
	}); err != nil {
		logger.LogAttrs(ctx, slog.LevelWarn, "scheduler: append audit entry failed",
			slog.String("category", category),
			slog.String("error", err.Error()))
	}
}

// alreadyAttempted reports whether this process already recorded a
// definitive outcome for window (or a later one) of (repo, workflow).
func (t *Ticker) alreadyAttempted(repo, workflowID string, window time.Time) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	last, ok := t.attempted[workflowKey{repo, workflowID}]
	return ok && !window.After(last)
}

func (t *Ticker) markAttempted(repo, workflowID string, window time.Time) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.attempted == nil {
		t.attempted = map[workflowKey]time.Time{}
	}
	t.attempted[workflowKey{repo, workflowID}] = window
}

func (t *Ticker) withRepo(repo string, fn func(*repoState)) {
	t.mu.Lock()
	defer t.mu.Unlock()
	fn(t.repoLocked(repo))
}

func (t *Ticker) withWorkflow(repo, workflowID string, fn func(*workflowState)) {
	t.mu.Lock()
	defer t.mu.Unlock()
	rs := t.repoLocked(repo)
	ws, ok := rs.workflows[workflowID]
	if !ok {
		ws = &workflowState{}
		rs.workflows[workflowID] = ws
	}
	fn(ws)
}

func (t *Ticker) repoLocked(repo string) *repoState {
	if t.repos == nil {
		t.repos = map[string]*repoState{}
	}
	rs, ok := t.repos[repo]
	if !ok {
		rs = &repoState{workflows: map[string]*workflowState{}}
		t.repos[repo] = rs
	}
	return rs
}

// Snapshot returns the state of every configured repository, in Repos order.
func (t *Ticker) Snapshot() []RepoSnapshot {
	out := make([]RepoSnapshot, 0, len(t.Repos))
	for _, repo := range t.Repos {
		snap, _ := t.SnapshotFor(repo)
		out = append(out, snap)
	}
	return out
}

// SnapshotFor returns one repository's state. ok is false when repo is not
// among the configured Repos (the scheduler never scans it).
func (t *Ticker) SnapshotFor(repo string) (RepoSnapshot, bool) {
	configured := false
	for _, r := range t.Repos {
		if r == repo {
			configured = true
			break
		}
	}
	if !configured {
		return RepoSnapshot{}, false
	}
	snap := RepoSnapshot{Repo: repo, RunnerKind: t.RunnerKind, Schedules: []WorkflowSnapshot{}}
	if t.RunnerKind == run.RunnerKindLocal {
		snap.DispatchNote = DispatchNoteLocal
	}

	t.mu.Lock()
	defer t.mu.Unlock()
	rs, ok := t.repos[repo]
	if !ok {
		return snap, true
	}
	snap.Scanned = rs.scanned
	snap.LastTickAt = rs.lastTickAt
	snap.SpecError = rs.specError
	ids := make([]string, 0, len(rs.workflows))
	for id := range rs.workflows {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		ws := rs.workflows[id]
		w := WorkflowSnapshot{
			WorkflowID:         id,
			Cron:               ws.cron,
			Timezone:           ws.timezone,
			Issue:              ws.issue,
			CurrentWindowStart: ws.currentWindowStart,
			NextDueAt:          ws.nextDueAt,
			ScheduleError:      ws.scheduleError,
		}
		if ws.lastOutcome != nil {
			lo := *ws.lastOutcome
			w.LastOutcome = &lo
		}
		snap.Schedules = append(snap.Schedules, w)
	}
	return snap, true
}

func (t *Ticker) now() time.Time {
	if t.Now != nil {
		return t.Now()
	}
	return time.Now()
}

func (t *Ticker) logger() *slog.Logger {
	if t.Logger != nil {
		return t.Logger
	}
	return slog.Default()
}
