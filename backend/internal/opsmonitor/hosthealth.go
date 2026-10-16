package opsmonitor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/hosthealth"
)

// CategoryOpsMonitorAlert is the global-chain audit category written once
// per tick that has at least one undeduplicated host-health hysteresis
// crossing (E85.11 / #4330): the ADR-097 D3 trigger record the first
// officer's standing watch consumes. Registered in audit.KnownCategories;
// INTERNAL (not an issue-comment surface). Also its own restart-surviving
// dedup record.
const CategoryOpsMonitorAlert = "ops_monitor_alert"

// Host-health result subjects.
const (
	subjectHost           = "host"
	subjectGateContainers = "gate_containers"
)

// fallbackHolders is how many of the largest processes are named when none
// reaches the holder floor.
const fallbackHolders = 3

// holdStaleFactor: an AdmissionHold whose last update is older than this
// many effective intervals reads not held.
const holdStaleFactor = 3

// ThresholdsFunc resolves the host-health bands from the host's total RAM
// (read at Check time, so nothing runs at flag registration) and its core
// count. nil selects hosthealth.DefaultThresholds.
type ThresholdsFunc func(totalRAM uint64, cores int) hosthealth.Thresholds

func resolveThresholds(fn ThresholdsFunc, cores func() int, totalRAM uint64) (hosthealth.Thresholds, error) {
	if fn == nil {
		fn = hosthealth.DefaultThresholds
	}
	if cores == nil {
		cores = runtime.NumCPU
	}
	th := fn(totalRAM, cores())
	if err := th.Validate(); err != nil {
		return hosthealth.Thresholds{}, err
	}
	return th, nil
}

// hostReadFailure maps a hosthealth read error: an unsupported OS is
// not_applicable (the reason names the OS), anything else is unknown with a
// fixed-template reason. Raw command or file error text never reaches a
// result.
func hostReadFailure(probe, goos, what string, err error) Result {
	switch {
	case errors.Is(err, hosthealth.ErrNotSupported):
		return Result{Probe: probe, Status: StatusNotApplicable, OS: goos,
			Reason: fmt.Sprintf("host %s probe not supported on %s", what, goos)}
	case errors.Is(err, hosthealth.ErrRuntimeAbsent):
		return Result{Probe: probe, Status: StatusUnknown, OS: goos,
			Reason: "docker CLI not on PATH: gate containers are unobservable"}
	default:
		return Result{Probe: probe, Status: StatusUnknown, OS: goos,
			Reason: fmt.Sprintf("host %s reading unavailable on %s", what, goos)}
	}
}

// --- contributors -----------------------------------------------------------

// Contributor is one process or container a host-health failing result
// names. Kind is "process" or "container"; a container has no PID.
type Contributor struct {
	Kind            string `json:"kind"`
	PID             int    `json:"pid,omitempty"`
	Name            string `json:"name"`
	ResidentBytes   uint64 `json:"resident_bytes"`
	CompressedBytes uint64 `json:"compressed_bytes,omitempty"`
}

func contributorsFromProcesses(procs []hosthealth.Process) []Contributor {
	out := make([]Contributor, 0, len(procs))
	for _, p := range procs {
		out = append(out, Contributor{Kind: "process", PID: p.PID, Name: p.Name, ResidentBytes: p.ResidentBytes, CompressedBytes: p.CompressedBytes})
	}
	return boundContributors(out)
}

func contributorsFromContainers(cs []hosthealth.Container) []Contributor {
	out := make([]Contributor, 0, len(cs))
	for _, c := range cs {
		out = append(out, Contributor{Kind: "container", Name: c.Name, ResidentBytes: c.MemoryBytes})
	}
	return boundContributors(out)
}

// boundContributors is the boundary every contributor crosses before it
// reaches a result, a remedy or the untenanted ops_monitor_alert row: at
// most hosthealth.MaxContributors entries, each name sanitized (control
// characters and newlines stripped, at most hosthealth.MaxNameBytes).
// Names are host data, never instructions.
func boundContributors(cs []Contributor) []Contributor {
	n := min(len(cs), hosthealth.MaxContributors)
	out := make([]Contributor, n)
	for i := range n {
		out[i] = cs[i]
		out[i].Name = hosthealth.SanitizeName(cs[i].Name)
	}
	return out
}

// --- memory -----------------------------------------------------------------

// MemoryProbe reports host memory pressure: available memory (low-water),
// swap (high-water, only when swap is configured) and the OS pressure
// signal (the macOS level, or Linux PSI memory some-avg10), each behind its
// own hysteresis latch, plus a critical latch (available <= CriticalAvailable
// or swap >= CriticalSwap, clearing at the ordinary exits). It emits ONE
// result: failing host_memory_pressure while any signal latch is latched,
// naming the largest holders as contributors and a composed remedy.
// Crossing is set only on the clear -> failing edge. Stateful: use it by
// pointer and share one instance across sweeps.
type MemoryProbe struct {
	// Reader reads the host; the zero Reader reads the running host.
	Reader hosthealth.Reader
	// Thresholds resolves the bands; nil is hosthealth.DefaultThresholds.
	Thresholds ThresholdsFunc
	// Cores is the core count; nil is runtime.NumCPU.
	Cores func() int

	mu                                  sync.Mutex
	available, swap, pressure, critical hosthealth.Latch
	failing                             bool
}

// Name implements Probe.
func (*MemoryProbe) Name() string { return "memory" }

// HostLocal implements Probe.
func (*MemoryProbe) HostLocal() bool { return true }

// Check implements Probe. The process list is read only while failing, so a
// healthy host runs no per-process scan.
func (p *MemoryProbe) Check(ctx context.Context) []Result {
	goos := p.Reader.OS()
	m, err := p.Reader.Memory(ctx)
	if err != nil {
		return []Result{hostReadFailure(p.Name(), goos, "memory", err)}
	}
	th, err := resolveThresholds(p.Thresholds, p.Cores, m.TotalBytes)
	if err != nil {
		return []Result{unknown(p.Name(), "memory thresholds invalid: "+err.Error())}
	}
	failing, crossed, critical := p.advance(goos, m, th)
	r := Result{
		Probe:     p.Name(),
		Status:    StatusOK,
		Observed:  memoryObserved(goos, m),
		Threshold: memoryThreshold(goos, m, th),
		OS:        goos,
	}
	if !failing {
		return []Result{r}
	}
	r.Status = StatusFailing
	r.Condition = ConditionHostMemoryPressure
	r.Subject = subjectHost
	r.Crossing = crossed
	r.Critical = critical
	var holders []hosthealth.Process
	if procs, perr := p.Reader.Processes(ctx, hosthealth.DefaultTopProcesses); perr == nil {
		holders = hosthealth.TopHolders(procs, th.HolderMinBytes, fallbackHolders)
	} else {
		r.Observed += "; largest holders unreadable"
	}
	r.Contributors = contributorsFromProcesses(holders)
	r.Remedy = hosthealth.ComposeRemedy(goos, holders)
	return []Result{r}
}

// advance feeds one reading to every latch. failing is any signal latch
// latched; crossed is the clear -> failing edge; critical is the critical
// latch while failing.
func (p *MemoryProbe) advance(goos string, m hosthealth.Memory, th hosthealth.Thresholds) (failing, crossed, critical bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	avail := m.AvailableFraction()
	availLatched, _ := p.available.UpdateLowWater(avail, th.AvailableEnter, th.AvailableExit)
	swapFrac, hasSwap := m.SwapFraction()
	var swapLatched bool
	if hasSwap {
		swapLatched, _ = p.swap.UpdateHighWater(swapFrac, th.SwapEnter, th.SwapExit)
	} else {
		swapLatched, _ = p.swap.Update(false, true)
	}
	pressureLatched, _ := p.pressure.Update(pressureBand(goos, m.Pressure, th))
	criticalEnter := avail <= th.CriticalAvailable || (hasSwap && swapFrac >= th.CriticalSwap)
	criticalExit := avail > th.AvailableExit && (!hasSwap || swapFrac < th.SwapExit)
	criticalLatched, _ := p.critical.Update(criticalEnter, criticalExit)

	failing = availLatched || swapLatched || pressureLatched
	crossed = failing && !p.failing
	p.failing = failing
	return failing, crossed, failing && criticalLatched
}

// pressureBand maps the OS pressure signal onto the latch's enter/exit. An
// unavailable signal clears (it cannot hold a latch it no longer observes).
// macOS: critical enters, normal clears, warn and unknown hold. Linux: PSI
// memory some-avg10 against PSIEnter / PSIExit.
func pressureBand(goos string, pr hosthealth.Pressure, th hosthealth.Thresholds) (enter, exit bool) {
	if !pr.Available {
		return false, true
	}
	if goos == "linux" {
		return pr.SomeAvg10 >= th.PSIEnter, pr.SomeAvg10 < th.PSIExit
	}
	switch pr.Level {
	case hosthealth.LevelCritical:
		return true, false
	case hosthealth.LevelNormal:
		return false, true
	default:
		return false, false
	}
}

func pct(f float64) string { return fmt.Sprintf("%.1f%%", 100*f) }

func memoryObserved(goos string, m hosthealth.Memory) string {
	parts := []string{fmt.Sprintf("available %s of %s", pct(m.AvailableFraction()), gibString(m.TotalBytes))}
	if f, ok := m.SwapFraction(); ok {
		parts = append(parts, fmt.Sprintf("swap %s of %s", pct(f), gibString(m.SwapTotalBytes)))
	} else {
		parts = append(parts, "no swap configured")
	}
	switch {
	case !m.Pressure.Available:
		parts = append(parts, "pressure signal unavailable")
	case goos == "linux":
		parts = append(parts, fmt.Sprintf("PSI memory some avg10 %.2f", m.Pressure.SomeAvg10))
	default:
		parts = append(parts, fmt.Sprintf("pressure level %s", m.Pressure.Level))
	}
	return strings.Join(parts, "; ")
}

func memoryThreshold(goos string, m hosthealth.Memory, th hosthealth.Thresholds) string {
	parts := []string{fmt.Sprintf("available > %s (clears above %s)", pct(th.AvailableEnter), pct(th.AvailableExit))}
	if _, ok := m.SwapFraction(); ok {
		parts = append(parts, fmt.Sprintf("swap < %s (clears below %s)", pct(th.SwapEnter), pct(th.SwapExit)))
	}
	if goos == "linux" {
		parts = append(parts, fmt.Sprintf("PSI memory some avg10 < %.1f (clears below %.1f)", th.PSIEnter, th.PSIExit))
	} else {
		parts = append(parts, "pressure level below critical (clears at normal)")
	}
	parts = append(parts, fmt.Sprintf("critical at available <= %s or swap >= %s", pct(th.CriticalAvailable), pct(th.CriticalSwap)))
	return strings.Join(parts, "; ")
}

// --- workload containers ----------------------------------------------------

// WorkloadContainersProbe reports the running Fishhawk gate and service
// containers (hosthealth.GateContainerPrefix): failing
// workload_containers_high while their total memory (high-water, a fraction
// of host RAM) or their count is latched. The count trips above
// ContainerCountMax and clears below it. Contributors are the containers,
// largest first. Stateful: use it by pointer.
type WorkloadContainersProbe struct {
	// Reader reads the host; the zero Reader reads the running host.
	Reader hosthealth.Reader
	// Thresholds resolves the bands; nil is hosthealth.DefaultThresholds.
	Thresholds ThresholdsFunc
	// Cores is the core count; nil is runtime.NumCPU.
	Cores func() int

	mu            sync.Mutex
	memory, count hosthealth.Latch
	failing       bool
}

// Name implements Probe.
func (*WorkloadContainersProbe) Name() string { return "workload_containers" }

// HostLocal implements Probe.
func (*WorkloadContainersProbe) HostLocal() bool { return true }

// Check implements Probe.
func (p *WorkloadContainersProbe) Check(ctx context.Context) []Result {
	goos := p.Reader.OS()
	m, err := p.Reader.Memory(ctx)
	if err != nil {
		return []Result{hostReadFailure(p.Name(), goos, "memory", err)}
	}
	cs, err := p.Reader.Containers(ctx)
	if err != nil {
		return []Result{hostReadFailure(p.Name(), goos, "container", err)}
	}
	th, err := resolveThresholds(p.Thresholds, p.Cores, m.TotalBytes)
	if err != nil {
		return []Result{unknown(p.Name(), "container thresholds invalid: "+err.Error())}
	}
	var used uint64
	for _, c := range cs {
		used += c.MemoryBytes
	}
	frac := float64(used) / float64(m.TotalBytes)
	n := len(cs)

	p.mu.Lock()
	memLatched, _ := p.memory.UpdateHighWater(frac, th.ContainerMemEnter, th.ContainerMemExit)
	countLatched, _ := p.count.Update(n > th.ContainerCountMax, n < th.ContainerCountMax)
	failing := memLatched || countLatched
	crossed := failing && !p.failing
	p.failing = failing
	p.mu.Unlock()

	r := Result{
		Probe:    p.Name(),
		Status:   StatusOK,
		Observed: fmt.Sprintf("%d gate container(s) using %s (%s of %s)", n, gibString(used), pct(frac), gibString(m.TotalBytes)),
		Threshold: fmt.Sprintf("<= %d containers (clears below %d); memory < %s of RAM (clears below %s)",
			th.ContainerCountMax, th.ContainerCountMax, pct(th.ContainerMemEnter), pct(th.ContainerMemExit)),
		OS: goos,
	}
	if !failing {
		return []Result{r}
	}
	r.Status = StatusFailing
	r.Condition = ConditionWorkloadContainersHigh
	r.Subject = subjectGateContainers
	r.Crossing = crossed
	r.Contributors = contributorsFromContainers(cs)
	r.Remedy = containersRemedy(r.Contributors)
	return []Result{r}
}

func containersRemedy(cs []Contributor) string {
	if len(cs) == 0 {
		return remedies[ConditionWorkloadContainersHigh]
	}
	descs := make([]string, len(cs))
	for i, c := range cs {
		descs[i] = fmt.Sprintf("%s (%s)", c.Name, gibString(c.ResidentBytes))
	}
	return "Largest workload containers: " + strings.Join(descs, ", ") + ". " + remedies[ConditionWorkloadContainersHigh]
}

// --- admission hold -----------------------------------------------------------

// AdmissionHold is the optional, default-off dispatch hold (E85.11 / #4330):
// held while the memory probe reads CRITICAL pressure, so the server can
// refuse NEW host-dispatch admissions (409 host_memory_pressure) without
// touching in-flight stages. It is INTERIM: ADR-100's adaptive concurrency
// controller absorbs it. The Ticker updates it every tick (held iff a
// failing host_memory_pressure result is Critical; ok, unknown,
// not_applicable or a missing memory result clear it). Held reads false
// once the last update is older than 3 x the Ticker's EFFECTIVE interval
// (zero resolves to DefaultInterval, as Run does), so a dead ticker can
// never pin it. Safe for concurrent use; the zero value and a nil pointer
// are not held.
type AdmissionHold struct {
	mu         sync.Mutex
	held       bool
	reason     string
	updated    time.Time
	staleAfter time.Duration
}

// Held reports whether new host-dispatch admissions are held at now, and
// the reason (fixed template plus the probe's observed numbers).
func (h *AdmissionHold) Held(now time.Time) (bool, string) {
	if h == nil {
		return false, ""
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if !h.held || !within(now.Sub(h.updated), h.staleAfter) {
		return false, ""
	}
	return true, h.reason
}

func (h *AdmissionHold) set(now time.Time, held bool, reason string, staleAfter time.Duration) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.held, h.reason, h.updated, h.staleAfter = held, reason, now, staleAfter
}

func (t *Ticker) updateHold(results []Result) {
	if t.Hold == nil {
		return
	}
	held, reason := false, ""
	for _, r := range results {
		if r.Status == StatusFailing && r.Condition == ConditionHostMemoryPressure && r.Critical {
			held, reason = true, "host memory pressure is critical: "+r.Observed
			break
		}
	}
	t.Hold.set(t.now(), held, reason, holdStaleFactor*t.interval())
}

// --- ops_monitor_alert --------------------------------------------------------

// alertRecovered writes the tick's alert row under a recover, so a panicking
// store or lookup is a WARN and never stops the ticker.
func (t *Ticker) alertRecovered(ctx context.Context, results []Result) {
	defer func() {
		if rec := recover(); rec != nil {
			t.logger().LogAttrs(ctx, slog.LevelWarn, "opsmonitor: alert panicked")
		}
	}()
	t.alert(ctx, results)
}

// alert writes ONE ops_monitor_alert row for the tick's failing crossings,
// skipping each crossing whose condition|subject was alerted inside the
// quiet window (in memory, then through the audit log, fail-open). Nothing
// is written when no crossing occurred or every one is suppressed.
func (t *Ticker) alert(ctx context.Context, results []Result) {
	var crossings []Result
	for _, r := range results {
		if r.Status == StatusFailing && r.Crossing {
			crossings = append(crossings, r)
		}
	}
	if len(crossings) == 0 {
		return
	}
	now, quiet := t.now(), t.quietWindow()
	var recorded map[string]time.Time
	looked := false
	var written []Result
	for _, r := range crossings {
		key := dedupKey(r)
		t.mu.Lock()
		last, seen := t.lastAlerted[key]
		t.mu.Unlock()
		if seen && within(now.Sub(last), quiet) {
			continue
		}
		if !looked {
			recorded, looked = t.alertRowsInWindow(ctx, now, quiet), true
		}
		if ts, ok := recorded[key]; ok {
			t.markAlerted(key, ts)
			continue
		}
		written = append(written, r)
	}
	if len(written) == 0 {
		return
	}
	// Recorded before the append, so a failing append cannot turn every
	// later re-crossing inside the window into another row attempt.
	for _, r := range written {
		t.markAlerted(dedupKey(r), now)
	}
	payload, critical := alertPayload(written, t.Deployment, quiet)
	conditions := make([]string, len(written))
	for i, r := range written {
		conditions[i] = r.Condition
	}
	t.logger().LogAttrs(ctx, slog.LevelWarn, "opsmonitor: host health crossing",
		slog.String("conditions", strings.Join(conditions, ",")), slog.Bool("critical", critical))
	if t.Audit == nil {
		return
	}
	system := audit.ActorSystem
	subject := actorSubject
	if _, err := t.Audit.AppendGlobalChained(ctx, audit.GlobalChainAppendParams{
		Timestamp:    now,
		Category:     CategoryOpsMonitorAlert,
		ActorKind:    &system,
		ActorSubject: &subject,
		Payload:      payload,
	}); err != nil {
		t.logger().LogAttrs(ctx, slog.LevelWarn, "opsmonitor: alert append failed",
			slog.String("conditions", strings.Join(conditions, ",")), slog.String("error", err.Error()))
	}
}

type alertCrossing struct {
	Probe     string `json:"probe"`
	Condition string `json:"condition"`
	Subject   string `json:"subject"`
	Observed  string `json:"observed"`
	Threshold string `json:"threshold"`
}

// alertPayload builds the row body. Contributors are the union of the
// crossings' contributors, deduplicated and re-bounded (at most
// hosthealth.MaxContributors in total, names sanitized), because the row is
// untenanted. No Detail, argv or paths.
func alertPayload(written []Result, deployment Deployment, quiet time.Duration) ([]byte, bool) {
	crossings := make([]alertCrossing, 0, len(written))
	var contributors []Contributor
	seenContributor := map[string]bool{}
	var remedyParts []string
	seenRemedy := map[string]bool{}
	critical := false
	goos := ""
	for _, r := range written {
		crossings = append(crossings, alertCrossing{Probe: r.Probe, Condition: r.Condition, Subject: r.Subject, Observed: r.Observed, Threshold: r.Threshold})
		for _, c := range r.Contributors {
			k := fmt.Sprintf("%s|%d|%s", c.Kind, c.PID, c.Name)
			if !seenContributor[k] {
				seenContributor[k] = true
				contributors = append(contributors, c)
			}
		}
		if r.Remedy != "" && !seenRemedy[r.Remedy] {
			seenRemedy[r.Remedy] = true
			remedyParts = append(remedyParts, r.Remedy)
		}
		critical = critical || r.Critical
		if goos == "" {
			goos = r.OS
		}
	}
	if goos == "" {
		goos = runtime.GOOS
	}
	body := map[string]any{
		"crossings":            crossings,
		"contributors":         boundContributors(contributors),
		"remedy":               strings.Join(remedyParts, " "),
		"hold_guidance":        hosthealth.HoldGuidance,
		"os":                   goos,
		"critical":             critical,
		"deployment":           deployment,
		"quiet_window_seconds": int64(quiet / time.Second),
	}
	// Every value is a string, bool, number or a slice of plain structs, so
	// Marshal cannot fail.
	payload, _ := json.Marshal(body)
	return payload, critical
}

// alertRowsInWindow reads the newest ops_monitor_alert rows and returns,
// per crossing condition|subject, the newest timestamp inside the quiet
// window. A read error or a panicking lookup WARN-logs and returns nothing:
// fail open toward one alert, never toward silence.
func (t *Ticker) alertRowsInWindow(ctx context.Context, now time.Time, quiet time.Duration) map[string]time.Time {
	if t.Audit == nil {
		return nil
	}
	entries, err := t.listOpsRows(ctx, CategoryOpsMonitorAlert)
	if err != nil {
		t.logger().LogAttrs(ctx, slog.LevelWarn, "opsmonitor: alert dedup lookup failed; using in-memory dedup only",
			slog.String("error", err.Error()))
		return nil
	}
	out := map[string]time.Time{}
	for _, e := range entries {
		if e == nil || e.Category != CategoryOpsMonitorAlert || !within(now.Sub(e.Timestamp), quiet) {
			continue
		}
		var p struct {
			Crossings []struct {
				Condition string `json:"condition"`
				Subject   string `json:"subject"`
			} `json:"crossings"`
		}
		if json.Unmarshal(e.Payload, &p) != nil {
			continue
		}
		for _, c := range p.Crossings {
			key := c.Condition + "|" + c.Subject
			if prev, ok := out[key]; !ok || e.Timestamp.After(prev) {
				out[key] = e.Timestamp
			}
		}
	}
	return out
}

func (t *Ticker) markAlerted(key string, ts time.Time) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.lastAlerted == nil {
		t.lastAlerted = map[string]time.Time{}
	}
	t.lastAlerted[key] = ts
}
