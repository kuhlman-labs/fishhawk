// Package opsmonitor is fishhawkd's opt-in host and process health monitor
// (E85.1 / #4142). A Ticker sweeps a fixed set of probes — data-volume free
// space, sampled GOCACHE size, 1-minute load vs core count, Docker
// reachability with an EIO indicator, Postgres reachability,
// fishhawk-runner processes whose stage or run is terminal, and (E85.2 /
// #4146) pending scope amendments whose decision window is closing or whose
// stage is already terminal, and (E85.11 / #4330) host memory pressure and
// Fishhawk gate-container load — and turns each FAILING (condition, subject)
// into ONE run-less ops_probe_failed audit row plus ONE operator issue
// comment per quiet window. A host-health hysteresis CROSSING additionally
// writes ONE ops_monitor_alert trigger row per tick, and an optional
// AdmissionHold tracks critical memory pressure (hosthealth.go).
//
// Fail-safety is enforced centrally by Sweep: a probe that panics, times out,
// returns no result or an unrecognized status reads unknown, never ok. Every
// host-local probe reports not_applicable WITHOUT running when the resolved
// deployment is not local. Nothing in the package logs at ERROR: the monitor
// is advisory and can never stop fishhawkd. Contract: README.md.
package opsmonitor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/hosthealth"
)

// CategoryOpsProbeFailed is the global-chain audit category written once per
// failing (condition, subject) per quiet window. Registered in
// audit.KnownCategories; INTERNAL (not an issuecomment activity category).
const CategoryOpsProbeFailed = "ops_probe_failed"

// actorSubject is the system actor subject stamped on every row.
const actorSubject = "ops-monitor"

// Status is one probe result's verdict.
type Status string

// Probe result statuses.
const (
	StatusOK            Status = "ok"
	StatusFailing       Status = "failing"
	StatusUnknown       Status = "unknown"
	StatusNotApplicable Status = "not_applicable"
)

func (s Status) valid() bool {
	switch s {
	case StatusOK, StatusFailing, StatusUnknown, StatusNotApplicable:
		return true
	}
	return false
}

// Deployment is where fishhawkd runs relative to its runners.
type Deployment string

// Deployments.
const (
	DeploymentLocal      Deployment = "local"
	DeploymentKubernetes Deployment = "kubernetes"
	DeploymentRemote     Deployment = "remote"
)

// kubernetesHostEnv is injected by the kubelet into every pod.
const kubernetesHostEnv = "KUBERNETES_SERVICE_HOST"

// ResolveDeployment resolves raw ("" or "auto" auto-detects: kubernetes when
// KUBERNETES_SERVICE_HOST is non-empty, else local; local|kubernetes|remote
// are honored verbatim). Anything else is an error naming the accepted set.
func ResolveDeployment(raw string, getenv func(string) string) (Deployment, error) {
	switch v := strings.TrimSpace(raw); v {
	case "", "auto":
		if getenv != nil && getenv(kubernetesHostEnv) != "" {
			return DeploymentKubernetes, nil
		}
		return DeploymentLocal, nil
	case string(DeploymentLocal), string(DeploymentKubernetes), string(DeploymentRemote):
		return Deployment(v), nil
	default:
		return "", fmt.Errorf("opsmonitor: unknown deployment %q (want auto, local, kubernetes or remote)", raw)
	}
}

// Conditions a failing result names.
const (
	ConditionDiskFreeLow          = "disk_free_low"
	ConditionGoCacheOverThreshold = "gocache_over_threshold"
	ConditionHostLoadHigh         = "host_load_high"
	ConditionDockerUnreachable    = "docker_unreachable"
	ConditionStorageEIO           = "storage_eio"
	ConditionPostgresUnreachable  = "postgres_unreachable"
	ConditionOrphanedRunner       = "orphaned_runner"
	// ConditionAmendmentWindowClosing: a pending scope amendment on a live
	// stage is near (or past) the implement agent's decision wait.
	ConditionAmendmentWindowClosing = "amendment_window_closing"
	// ConditionAmendmentPendingOnTerminalStage: a scope amendment is still
	// pending although its stage is terminal.
	ConditionAmendmentPendingOnTerminalStage = "amendment_pending_on_terminal_stage"
	// ConditionHostMemoryPressure: a host memory signal (available memory,
	// swap, the OS pressure signal) is latched past its band (E85.11).
	ConditionHostMemoryPressure = "host_memory_pressure"
	// ConditionWorkloadContainersHigh: running Fishhawk gate containers are
	// too many or hold too much of host RAM (E85.11).
	ConditionWorkloadContainersHigh = "workload_containers_high"
	// ConditionHostLoadIOWait: the load is over the limit and driven by tasks
	// waiting (I/O, swap-in), not by CPU demand (E85.11).
	ConditionHostLoadIOWait = "host_load_io_wait"
)

// Conditions lists every condition constant (the remedies-table coverage
// test iterates it).
var Conditions = []string{
	ConditionDiskFreeLow,
	ConditionGoCacheOverThreshold,
	ConditionHostLoadHigh,
	ConditionDockerUnreachable,
	ConditionStorageEIO,
	ConditionPostgresUnreachable,
	ConditionOrphanedRunner,
	ConditionAmendmentWindowClosing,
	ConditionAmendmentPendingOnTerminalStage,
	ConditionHostMemoryPressure,
	ConditionWorkloadContainersHigh,
	ConditionHostLoadIOWait,
}

// remedies is the one human remedy per condition.
var remedies = map[string]string{
	ConditionDiskFreeLow:                     "Free space on the data volume: run `scripts/dev sweep` and `scripts/dev gocache --trim`, prune unused Docker images and volumes, and remove stale run worktrees.",
	ConditionGoCacheOverThreshold:            "Run `scripts/dev gocache --trim` (it refuses while a run is live). Never run `go clean -cache` under a live run: it deletes entries a running build is reading.",
	ConditionHostLoadHigh:                    "Look for orphaned sh/zsh children of pid 1 that no run owns (`ps -axo pid=,ppid=,pcpu=,comm=`) and kill them; verifies started now may fail on host load, not on the change.",
	ConditionDockerUnreachable:               "Start or restart Docker Desktop (`open -a Docker`), confirm `docker info` answers, then reap any stage stranded by the outage (fishhawk_reap_stage).",
	ConditionStorageEIO:                      "Storage is returning I/O errors: free host disk, restart Docker Desktop, confirm Postgres is healthy, then reap stranded stages (fishhawk_reap_stage).",
	ConditionPostgresUnreachable:             "Check the Postgres container (`docker ps`, `docker logs`), free host disk if it crashed on EIO, restart it, then reap stages stranded by the outage.",
	ConditionOrphanedRunner:                  "Confirm the pid is a fishhawk-runner whose stage is terminal (`ps -o pid,args -p <pid>`) and kill it; it holds host resources and can block reload/post-merge.",
	ConditionAmendmentWindowClosing:          "Decide the amendment now with fishhawk_decide_scope_amendment (fishhawk_list_scope_amendments names it). Once the window closes the implement agent stops waiting, and a later approval lands only through fishhawk_retry_stage.",
	ConditionAmendmentPendingOnTerminalStage: "The stage is terminal, so no agent will read a decision: deny the amendment with fishhawk_decide_scope_amendment to close it, or approve it and run fishhawk_retry_stage to land the paths.",
	// The three host-health remedies are host- and codebase-agnostic. The
	// memory and container probes compose their own remedy naming the
	// contributors, which wins over these table entries.
	ConditionHostMemoryPressure:     hosthealth.GenericAction + " " + hosthealth.HoldGuidance,
	ConditionWorkloadContainersHigh: "Too many workload containers are running, or they hold too much host memory: let in-flight stages finish, stop idle containers, and run fewer stages concurrently until the count and memory recover.",
	ConditionHostLoadIOWait:         "The load is driven by tasks waiting on I/O or swap-in, not by CPU demand: free memory (the memory probe names the largest holders) and run fewer I/O-heavy workloads at once; stopping CPU-bound processes will not help.",
}

// genericRemedy fills a failing result whose condition has no table entry.
const genericRemedy = "Inspect the named probe's observed value on the fishhawkd host and correct the underlying condition; see backend/internal/opsmonitor/README.md."

// RemedyFor returns condition's remedy, the generic one when unlisted.
func RemedyFor(condition string) string {
	if r := remedies[condition]; r != "" {
		return r
	}
	return genericRemedy
}

// Result is one probe outcome.
type Result struct {
	Probe     string         `json:"probe"`
	Condition string         `json:"condition,omitempty"`
	Subject   string         `json:"subject,omitempty"`
	Status    Status         `json:"status"`
	Observed  string         `json:"observed,omitempty"`
	Threshold string         `json:"threshold,omitempty"`
	Remedy    string         `json:"remedy,omitempty"`
	Reason    string         `json:"reason,omitempty"`
	Detail    map[string]any `json:"detail,omitempty"`
	// Fields are structured values emit copies VERBATIM onto the untenanted
	// ops_probe_failed row (payload key `fields`), which every tenant member
	// can read. Unlike Detail they must carry only ids, counts and
	// durations: never host paths, argv, amendment paths or agent-written
	// reason text.
	Fields map[string]any `json:"fields,omitempty"`
	// Contributors names the processes or containers a host-health failing
	// result attributes the condition to (E85.11). The alert row carries
	// them bounded and sanitized (boundContributors).
	Contributors []Contributor `json:"contributors,omitempty"`
	// OS is the operating system a host-health probe read (E85.11).
	OS string `json:"os,omitempty"`
	// Crossing is set on the tick a host-health latch crosses from clear to
	// latched; only a crossing writes an ops_monitor_alert row. Critical is
	// set while the memory probe's critical latch holds; it drives the
	// AdmissionHold. Neither is serialized.
	Crossing bool `json:"-"`
	Critical bool `json:"-"`
}

// Probe is one health check. HostLocal probes observe the fishhawkd host and
// are skipped (not_applicable) under a non-local deployment.
type Probe interface {
	Name() string
	HostLocal() bool
	Check(ctx context.Context) []Result
}

// Defaults.
const (
	DefaultInterval      = 5 * time.Minute
	DefaultProbeTimeout  = 30 * time.Second
	DefaultQuietWindow   = time.Hour
	DefaultNotifyTimeout = 15 * time.Second
	// auditLookupLimit bounds the restart-surviving dedup lookup.
	auditLookupLimit = 200
)

// Sweep runs every probe once and is the single fail-safe chokepoint: a
// host-local probe under a non-local deployment yields one not_applicable
// result without its Check being called; every other probe runs on its own
// goroutine under probeTimeout, and a deadline, a panic, zero results or an
// empty/unrecognized status reads unknown. A failing result with no remedy
// gets the table (or generic) remedy.
func Sweep(ctx context.Context, probes []Probe, deployment Deployment, probeTimeout time.Duration) []Result {
	if probeTimeout <= 0 {
		probeTimeout = DefaultProbeTimeout
	}
	var out []Result
	for _, p := range probes {
		out = append(out, runProbe(ctx, p, deployment, probeTimeout)...)
	}
	return out
}

func unknown(probe, reason string) Result {
	return Result{Probe: probe, Status: StatusUnknown, Reason: reason}
}

// probeMeta reads Name and HostLocal under a recover, so a probe whose
// metadata panics still reads unknown.
func probeMeta(p Probe) (name string, hostLocal bool, ok bool) {
	defer func() {
		if recover() != nil {
			ok = false
		}
	}()
	return p.Name(), p.HostLocal(), true
}

type checkOutcome struct {
	results  []Result
	panicked bool
}

func runProbe(ctx context.Context, p Probe, deployment Deployment, timeout time.Duration) []Result {
	if p == nil {
		return []Result{unknown("unnamed", "probe is nil")}
	}
	name, hostLocal, ok := probeMeta(p)
	if !ok {
		return []Result{unknown("unnamed", "probe panicked")}
	}
	if name == "" {
		name = "unnamed"
	}
	if hostLocal && deployment != DeploymentLocal {
		return []Result{{
			Probe:  name,
			Status: StatusNotApplicable,
			Reason: fmt.Sprintf("host-local probe skipped: deployment is %s", deployment),
		}}
	}

	pctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	// Buffered so a late result never blocks the sender. A Check that ignores
	// its context and never returns is abandoned here: one leaked goroutine
	// per tick for as long as it stays wedged (README "Fail-safety").
	ch := make(chan checkOutcome, 1)
	go func() {
		defer func() {
			if recover() != nil {
				ch <- checkOutcome{panicked: true}
			}
		}()
		ch <- checkOutcome{results: p.Check(pctx)}
	}()

	var o checkOutcome
	select {
	case o = <-ch:
	case <-pctx.Done():
		return []Result{unknown(name, deadlineReason(ctx, timeout))}
	}
	// A result that arrived after the deadline is not trusted: a probe that
	// returns ok only once its context is cancelled observed nothing.
	if pctx.Err() != nil {
		return []Result{unknown(name, deadlineReason(ctx, timeout))}
	}
	if o.panicked {
		return []Result{unknown(name, "probe panicked")}
	}
	if len(o.results) == 0 {
		return []Result{unknown(name, "probe returned no result")}
	}
	out := make([]Result, 0, len(o.results))
	for _, r := range o.results {
		out = append(out, normalize(name, r))
	}
	return out
}

func deadlineReason(parent context.Context, timeout time.Duration) string {
	if parent.Err() != nil {
		return "sweep cancelled before the probe finished"
	}
	return fmt.Sprintf("probe timed out after %s", timeout)
}

func normalize(name string, r Result) Result {
	if r.Probe == "" {
		r.Probe = name
	}
	if !r.Status.valid() {
		return Result{
			Probe:     r.Probe,
			Condition: r.Condition,
			Subject:   r.Subject,
			Status:    StatusUnknown,
			Reason:    fmt.Sprintf("probe returned an unrecognized status %q", string(r.Status)),
		}
	}
	if r.Status == StatusFailing && r.Remedy == "" {
		r.Remedy = RemedyFor(r.Condition)
	}
	if r.Status == StatusUnknown && r.Reason == "" {
		r.Reason = "probe reported unknown without a reason"
	}
	return r
}

// Ticker sweeps Probes on Interval and emits each failing condition once per
// QuietWindow. Run blocks until ctx is done; start it on its own goroutine.
type Ticker struct {
	// Probes is the sweep set. Required (Run refuses an empty set).
	Probes []Probe
	// Deployment gates the host-local probes.
	Deployment Deployment
	// Audit receives the ops_probe_failed rows and backs the
	// restart-surviving dedup lookup. Optional: nil is in-memory dedup only
	// and no row.
	Audit audit.Repository
	// Notifier posts the operator comment. Optional: nil records
	// notification=disabled.
	Notifier Notifier
	// Logger receives INFO/WARN lines. nil → slog.Default().
	Logger *slog.Logger

	Interval      time.Duration // default DefaultInterval
	ProbeTimeout  time.Duration // default DefaultProbeTimeout
	QuietWindow   time.Duration // default DefaultQuietWindow
	NotifyTimeout time.Duration // default DefaultNotifyTimeout
	// Now is the clock for the dedup window and the row timestamp.
	Now func() time.Time
	// Hold, when set, is updated on every tick: held while a memory result
	// is critical, cleared otherwise. INTERIM (see AdmissionHold). Its
	// update never depends on an audit write.
	Hold *AdmissionHold

	mu          sync.Mutex
	lastEmitted map[string]time.Time
	lastWarned  map[string]time.Time
	lastAlerted map[string]time.Time
}

// Run drives the ticker until ctx is cancelled. It errors only on an empty
// probe list.
func (t *Ticker) Run(ctx context.Context) error {
	if len(t.Probes) == 0 {
		return fmt.Errorf("opsmonitor: ticker requires at least one probe")
	}
	t.logNotApplicable(ctx)

	t.Tick(ctx)
	tick := time.NewTicker(t.interval())
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

func (t *Ticker) logNotApplicable(ctx context.Context) {
	if t.Deployment == DeploymentLocal {
		return
	}
	var skipped []string
	for _, p := range t.Probes {
		if p == nil {
			continue
		}
		if name, hostLocal, ok := probeMeta(p); ok && hostLocal {
			skipped = append(skipped, name)
		}
	}
	if len(skipped) == 0 {
		return
	}
	t.logger().LogAttrs(ctx, slog.LevelInfo, "opsmonitor: host-local probes not applicable",
		slog.String("deployment", string(t.Deployment)),
		slog.String("probes", strings.Join(skipped, ",")))
}

// Tick runs one sweep, updates the Hold, emits its failing results, then
// writes at most ONE ops_monitor_alert row for the tick's host-health
// crossings. A panicking audit store or notifier is recovered per result
// (and once for the alert) into a WARN, so one emit can never stop another
// or crash the process. Exposed for deterministic tests.
func (t *Ticker) Tick(ctx context.Context) []Result {
	results := Sweep(ctx, t.Probes, t.Deployment, t.ProbeTimeout)
	// Before any audit write, so the hold never depends on one succeeding.
	t.updateHold(results)
	for _, r := range results {
		switch r.Status {
		case StatusFailing:
			t.emitRecovered(ctx, r)
		case StatusUnknown:
			t.warnUnknown(ctx, r)
		}
	}
	t.alertRecovered(ctx, results)
	return results
}

func (t *Ticker) emitRecovered(ctx context.Context, r Result) {
	defer func() {
		if rec := recover(); rec != nil {
			t.logger().LogAttrs(ctx, slog.LevelWarn, "opsmonitor: emit panicked",
				slog.String("probe", r.Probe), slog.String("condition", r.Condition))
		}
	}()
	t.emit(ctx, r)
}

func dedupKey(r Result) string { return r.Condition + "|" + r.Subject }

func (t *Ticker) emit(ctx context.Context, r Result) {
	now := t.now()
	quiet := t.quietWindow()
	key := dedupKey(r)

	t.mu.Lock()
	last, seen := t.lastEmitted[key]
	t.mu.Unlock()
	if seen && within(now.Sub(last), quiet) {
		return
	}
	if ts, found := t.auditSuppressed(ctx, r, now, quiet); found {
		t.markEmitted(key, ts)
		return
	}
	// Recorded before the notify and the append, so neither failing can turn
	// every later tick into another notification.
	t.markEmitted(key, now)

	notification := "disabled"
	if t.Notifier != nil {
		if err := t.notify(ctx, r); err != nil {
			notification = "failed"
			t.logger().LogAttrs(ctx, slog.LevelWarn, "opsmonitor: notification failed",
				slog.String("condition", r.Condition), slog.String("error", err.Error()))
		} else {
			notification = "posted"
		}
	}
	t.logger().LogAttrs(ctx, slog.LevelWarn, "opsmonitor: condition failing",
		slog.String("condition", r.Condition), slog.String("probe", r.Probe),
		slog.String("subject", r.Subject), slog.String("observed", r.Observed),
		slog.String("threshold", r.Threshold), slog.String("notification", notification))

	if t.Audit == nil {
		return
	}
	// No Detail: the row is untenanted (nil account), so GET /v0/audit shows
	// it to every tenant member, and Detail carries host paths (the disk
	// path, the GOCACHE dir). The row carries the comment's field set, plus
	// the result's Fields (ids/counts/durations only, see Result.Fields).
	body := map[string]any{
		"condition":            r.Condition,
		"probe":                r.Probe,
		"subject":              r.Subject,
		"status":               r.Status,
		"observed":             r.Observed,
		"threshold":            r.Threshold,
		"remedy":               r.Remedy,
		"reason":               r.Reason,
		"deployment":           t.Deployment,
		"quiet_window_seconds": int64(quiet / time.Second),
		"notification":         notification,
	}
	if len(r.Fields) > 0 {
		body["fields"] = r.Fields
	}
	payload, err := json.Marshal(body)
	if err != nil {
		t.logger().LogAttrs(ctx, slog.LevelWarn, "opsmonitor: marshal audit payload failed",
			slog.String("condition", r.Condition), slog.String("error", err.Error()))
		return
	}
	system := audit.ActorSystem
	subject := actorSubject
	if _, err := t.Audit.AppendGlobalChained(ctx, audit.GlobalChainAppendParams{
		Timestamp:    now,
		Category:     CategoryOpsProbeFailed,
		ActorKind:    &system,
		ActorSubject: &subject,
		Payload:      payload,
	}); err != nil {
		t.logger().LogAttrs(ctx, slog.LevelWarn, "opsmonitor: audit append failed",
			slog.String("condition", r.Condition), slog.String("error", err.Error()))
	}
}

// notify calls the Notifier under NotifyTimeout, converting a panic into an
// error so the audit row still records notification=failed.
func (t *Ticker) notify(ctx context.Context, r Result) (err error) {
	defer func() {
		if rec := recover(); rec != nil {
			err = fmt.Errorf("notifier panicked")
		}
	}()
	timeout := t.NotifyTimeout
	if timeout <= 0 {
		timeout = DefaultNotifyTimeout
	}
	nctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	return t.Notifier.Notify(nctx, r, t.Deployment)
}

// auditSuppressed reports whether the audit log already holds an
// ops_probe_failed row for r's (condition, subject) inside the quiet window,
// returning its timestamp. A read error or a panicking lookup WARN-logs and
// returns false: fail open toward one notification, never toward silence.
func (t *Ticker) auditSuppressed(ctx context.Context, r Result, now time.Time, quiet time.Duration) (time.Time, bool) {
	if t.Audit == nil {
		return time.Time{}, false
	}
	entries, err := t.listOpsRows(ctx, CategoryOpsProbeFailed)
	if err != nil {
		t.logger().LogAttrs(ctx, slog.LevelWarn, "opsmonitor: audit dedup lookup failed; using in-memory dedup only",
			slog.String("error", err.Error()))
		return time.Time{}, false
	}
	for _, e := range entries {
		if e == nil || e.Category != CategoryOpsProbeFailed {
			continue
		}
		var p struct {
			Condition string `json:"condition"`
			Subject   string `json:"subject"`
		}
		if json.Unmarshal(e.Payload, &p) != nil {
			continue
		}
		if p.Condition == r.Condition && p.Subject == r.Subject && within(now.Sub(e.Timestamp), quiet) {
			return e.Timestamp, true
		}
	}
	return time.Time{}, false
}

// listOpsRows runs a dedup lookup (the newest auditLookupLimit rows of one
// category) under its own recover, so a panicking store degrades to the
// in-memory path like a read error instead of unwinding emit before the
// notifier is reached.
func (t *Ticker) listOpsRows(ctx context.Context, category string) (entries []*audit.Entry, err error) {
	defer func() {
		if recover() != nil {
			entries, err = nil, errors.New("audit lookup panicked")
		}
	}()
	return t.Audit.ListAll(ctx, audit.ListAllParams{Category: &category, Limit: auditLookupLimit})
}

// warnUnknown logs one WARN per probe per quiet window for unknown results.
func (t *Ticker) warnUnknown(ctx context.Context, r Result) {
	now := t.now()
	t.mu.Lock()
	if t.lastWarned == nil {
		t.lastWarned = map[string]time.Time{}
	}
	last, seen := t.lastWarned[r.Probe]
	if seen && within(now.Sub(last), t.quietWindow()) {
		t.mu.Unlock()
		return
	}
	t.lastWarned[r.Probe] = now
	t.mu.Unlock()
	t.logger().LogAttrs(ctx, slog.LevelWarn, "opsmonitor: probe unknown",
		slog.String("probe", r.Probe), slog.String("reason", r.Reason))
}

func (t *Ticker) markEmitted(key string, ts time.Time) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.lastEmitted == nil {
		t.lastEmitted = map[string]time.Time{}
	}
	t.lastEmitted[key] = ts
}

// within reports whether an elapsed duration falls inside the quiet window
// in either direction, so a clock step backwards cannot suppress forever.
func within(d, quiet time.Duration) bool {
	return d < quiet && d > -quiet
}

// interval is the effective sweep interval: Run and the AdmissionHold
// staleness bound share this one zero -> DefaultInterval resolution.
func (t *Ticker) interval() time.Duration {
	if t.Interval <= 0 {
		return DefaultInterval
	}
	return t.Interval
}

func (t *Ticker) quietWindow() time.Duration {
	if t.QuietWindow <= 0 {
		return DefaultQuietWindow
	}
	return t.QuietWindow
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
