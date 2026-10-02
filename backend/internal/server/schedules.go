package server

import (
	"net/http"
	"sort"
	"strings"
	"time"
)

// GET /v0/schedules?repo=owner/name (E79.1 / #3725) is the read-only
// visibility surface over the in-process scheduler (backend/internal/scheduler):
// for one repository it reports each workflow declaring a `schedule` — its
// cron, timezone, current due window, next due time and the last outcome the
// scheduler recorded for it. It never writes and mints no audit entry; the
// scheduler's own outcome entries (scheduled_run_started / _skipped /
// _refused) ride the global audit chain.
//
// The scheduler lives in its own package and must not be imported here (it is
// wired by fishhawkd, which adapts Ticker.Snapshot onto ScheduleSource), so the
// snapshot shape is declared on THIS side of the seam and the adapter maps
// onto it — the same direction as every other optional Config source.

// ScheduleSource is the scheduler's read seam: Config.Schedules. Nil — the
// default, and the state of every deployment that did not pass
// --enable-scheduler — makes GET /v0/schedules answer 200 enabled:false,
// because "nothing is scheduled here" is the answer, not an outage.
type ScheduleSource interface {
	// ScheduleSnapshot returns the scheduler's current state for repo. ok is
	// false when repo is not in the scheduler's configured repository set;
	// the snapshot's deployment-level RunnerKind is still honored then, and
	// every per-repo field is ignored.
	ScheduleSnapshot(repo string) (snap ScheduleSnapshot, ok bool)
}

// ScheduleSnapshot is one repository's scheduler state.
type ScheduleSnapshot struct {
	// LastTickAt is when the scheduler last scanned this repository; zero
	// when it has not ticked yet.
	LastTickAt time.Time
	// SpecError is the most recent spec fetch/parse failure for the repo,
	// empty when the last scan read a valid spec. A spec error stops every
	// schedule in the repo from being evaluated, so it is reported beside
	// (not instead of) the last-known schedules.
	SpecError string
	// RunnerKind is the runner kind every scheduled run is started with
	// (--scheduler-runner-kind).
	RunnerKind string
	// Schedules is one entry per workflow that declares a schedule.
	Schedules []ScheduleEntry
}

// ScheduleEntry is one scheduled workflow's state.
type ScheduleEntry struct {
	WorkflowID string
	Cron       string
	// Timezone is the IANA name the cron is evaluated in; empty renders as
	// "UTC", the grammar's default.
	Timezone string
	// Issue is the optional anchor issue number; 0 when the schedule names
	// none.
	Issue int
	// CurrentWindowStart is the latest cron fire at or before the last tick;
	// zero when the cron has not fired yet.
	CurrentWindowStart time.Time
	// NextDueAt is the first fire strictly after the last tick; zero when
	// none falls within the parser's horizon.
	NextDueAt time.Time
	// LastOutcome is the last recorded attempt; nil when the scheduler has
	// not attempted any window for this workflow in this process.
	LastOutcome *ScheduleOutcome
}

// The ScheduleOutcome.Kind vocabulary. The first three mirror the audit
// categories the scheduler appends (scheduled_run_started / _skipped /
// _refused); transient_error is log-only on the chain (the window is retried
// next tick) and surfaces here so a stuck schedule is visible.
const (
	ScheduleOutcomeKindStarted        = "started"
	ScheduleOutcomeKindAlreadyStarted = "already_started"
	ScheduleOutcomeKindRefused        = "refused"
	ScheduleOutcomeKindTransientError = "transient_error"
)

// ScheduleOutcome is one window's recorded attempt.
type ScheduleOutcome struct {
	Kind        string
	WindowStart time.Time
	// RunID is the started (or replayed) run; empty for a refusal.
	RunID string
	// Code / Message are the admission refusal's error code and message
	// (e.g. budget_exhausted), or the transient error's text.
	Code    string
	Message string
	At      time.Time
}

// schedulerDisabledReason is the enabled:false reason. It names the switch so
// the reader learns how to turn it on rather than only that it is off.
const schedulerDisabledReason = "the scheduler is not enabled on this deployment (fishhawkd --enable-scheduler / FISHHAWKD_ENABLE_SCHEDULER); no workflow is started on a schedule"

// schedulerRepoNotScannedReason is the repo_scanned:false reason.
const schedulerRepoNotScannedReason = "the scheduler is enabled but does not scan this repository (fishhawkd --scheduler-repos / FISHHAWKD_SCHEDULER_REPOS); its schedules are not evaluated"

// schedulerLocalDispatchNote is rendered whenever scheduled runs start on the
// local runner: the scheduler mints the run, but a local stage parks until a
// host dispatches it, and auto-dispatch is out of scope.
const schedulerLocalDispatchNote = "runner_kind is local: a scheduled run parks at awaiting_host_dispatch until a host dispatches it (fishhawk_dispatch_stage / fishhawk_run_stage); the scheduler does not auto-dispatch"

// schedulesResponse is the GET /v0/schedules body.
type schedulesResponse struct {
	Repo         string                  `json:"repo"`
	Enabled      bool                    `json:"enabled"`
	Reason       string                  `json:"reason,omitempty"`
	RepoScanned  bool                    `json:"repo_scanned"`
	LastTickAt   *time.Time              `json:"last_tick_at"`
	SpecError    string                  `json:"spec_error,omitempty"`
	RunnerKind   string                  `json:"runner_kind,omitempty"`
	DispatchNote string                  `json:"dispatch_note,omitempty"`
	Schedules    []scheduleEntryResponse `json:"schedules"`
}

type scheduleEntryResponse struct {
	WorkflowID         string                   `json:"workflow_id"`
	Cron               string                   `json:"cron"`
	Timezone           string                   `json:"timezone"`
	Issue              int                      `json:"issue,omitempty"`
	CurrentWindowStart *time.Time               `json:"current_window_start"`
	NextDueAt          *time.Time               `json:"next_due_at"`
	LastOutcome        *scheduleOutcomeResponse `json:"last_outcome"`
}

type scheduleOutcomeResponse struct {
	Kind        string    `json:"kind"`
	WindowStart time.Time `json:"window_start"`
	RunID       string    `json:"run_id,omitempty"`
	Code        string    `json:"code,omitempty"`
	Message     string    `json:"message,omitempty"`
	At          time.Time `json:"at"`
}

// handleGetSchedules serves GET /v0/schedules?repo=owner/name.
//
// The read is authenticated and checks no scope (the fishhawk_doctor /
// onboarding-readiness posture): an anonymous caller is refused 401 before
// anything else. After that, ORDER IS LOAD-BEARING (operator condition 4 on
// #3725): the required repo param is validated first, then the point-read
// repo-visibility DENY, and only then the nil-source short-circuit. So an
// unconfigured deployment cannot be used to probe a repository the caller
// cannot read — the 403 lands before the enabled:false 200 does.
func (s *Server) handleGetSchedules(w http.ResponseWriter, r *http.Request) {
	if IdentityFrom(r.Context()).IsAnonymous() {
		s.writeError(w, r, http.StatusUnauthorized, "authentication_required",
			"an authenticated token or session is required", nil)
		return
	}
	repo := strings.TrimSpace(r.URL.Query().Get("repo"))
	if !validScheduleRepo(repo) {
		s.writeError(w, r, http.StatusBadRequest, "validation_failed",
			"repo is required (owner/name)", map[string]any{"field": "repo", "got": repo})
		return
	}
	if !s.enforceRepoVisibility(w, r, repo) {
		return
	}
	if s.cfg.Schedules == nil {
		s.writeJSON(w, r, http.StatusOK, schedulesResponse{
			Repo: repo, Enabled: false, Reason: schedulerDisabledReason,
			Schedules: []scheduleEntryResponse{},
		})
		return
	}
	snap, scanned := s.cfg.Schedules.ScheduleSnapshot(repo)
	s.writeJSON(w, r, http.StatusOK, renderSchedules(repo, snap, scanned))
}

// validScheduleRepo reports whether repo is a non-empty owner/name pair.
func validScheduleRepo(repo string) bool {
	owner, name, ok := strings.Cut(repo, "/")
	return ok && owner != "" && name != "" && !strings.Contains(name, "/")
}

// renderSchedules projects a snapshot onto the wire. Schedules are sorted by
// workflow_id so the response is stable across ticks whatever order the
// source keeps them in; zero times render as null rather than year 1.
func renderSchedules(repo string, snap ScheduleSnapshot, scanned bool) schedulesResponse {
	out := schedulesResponse{
		Repo:        repo,
		Enabled:     true,
		RepoScanned: scanned,
		RunnerKind:  snap.RunnerKind,
		Schedules:   []scheduleEntryResponse{},
	}
	if snap.RunnerKind == "local" {
		out.DispatchNote = schedulerLocalDispatchNote
	}
	if !scanned {
		out.Reason = schedulerRepoNotScannedReason
		return out
	}
	out.LastTickAt = utcTimePtr(snap.LastTickAt)
	out.SpecError = snap.SpecError
	for _, e := range snap.Schedules {
		tz := e.Timezone
		if tz == "" {
			tz = "UTC"
		}
		entry := scheduleEntryResponse{
			WorkflowID:         e.WorkflowID,
			Cron:               e.Cron,
			Timezone:           tz,
			Issue:              e.Issue,
			CurrentWindowStart: utcTimePtr(e.CurrentWindowStart),
			NextDueAt:          utcTimePtr(e.NextDueAt),
		}
		if o := e.LastOutcome; o != nil {
			entry.LastOutcome = &scheduleOutcomeResponse{
				Kind:        o.Kind,
				WindowStart: o.WindowStart.UTC(),
				RunID:       o.RunID,
				Code:        o.Code,
				Message:     o.Message,
				At:          o.At.UTC(),
			}
		}
		out.Schedules = append(out.Schedules, entry)
	}
	sort.SliceStable(out.Schedules, func(i, j int) bool {
		return out.Schedules[i].WorkflowID < out.Schedules[j].WorkflowID
	})
	return out
}

// utcTimePtr returns nil for the zero time and a UTC copy otherwise.
func utcTimePtr(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	u := t.UTC()
	return &u
}
