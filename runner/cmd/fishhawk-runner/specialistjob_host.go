package main

// Host inventory commands (#4330): the four read-only, argument-free host.*
// inventory ids the first officer's host-health duty requests. Each maps,
// per OS, onto a fixed set of reads (a fixed argv or a fixed /proc path), so
// no repository- or host-specific value ever reaches a command line. They
// read no repository tree, so executeInventoryJob routes them here BEFORE it
// clones. They observe only, never remediate (ADR-089 D9). An OS outside the
// table answers inconclusive/not_applicable without running anything.
// Host strings (process and container names) are untrusted: control and
// format characters are stripped, a parsed name is cut to hostNameMaxBytes,
// and every line then goes through boundInventoryLines (redacted, bounded).
// Contract: runner/README.md § "Specialist jobs".

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/kuhlman-labs/fishhawk/runner/internal/gateiso"
)

// The host.* inventory command ids (docs/spec/specialist-job-v1.schema.json
// $defs.inputs-inventory.command).
const (
	sjHostMemory     = "host.memory"
	sjHostTopMemory  = "host.top_memory"
	sjHostLoad       = "host.load"
	sjHostContainers = "host.containers"
)

const (
	// hostInventoryTimeout bounds EACH command or read of a host.* job.
	hostInventoryTimeout = 30 * time.Second
	// hostInventoryOutputLimit caps one command's captured stdout.
	hostInventoryOutputLimit = 4 << 20
	// hostTopProcesses is how many processes host.top_memory lists.
	hostTopProcesses = 50
	// hostGatePrefix is the runner's gate and service container name prefix
	// (gateiso: fishhawk-gate-<hex>, fishhawk-gate-svc-<hex>). docker's name
	// filter is a SUBSTRING match, so every name is re-filtered by it here.
	hostGatePrefix = "fishhawk-gate-"
	// hostContainerMax caps the containers host.containers passes to stats.
	hostContainerMax = 64
	// hostNameMaxBytes caps a parsed process or container name.
	hostNameMaxBytes = 64
)

// errHostDockerUnavailable: no safe, local docker runtime on this host (none
// on PATH, podman only, or a remote endpoint).
var errHostDockerUnavailable = errors.New("host inventory: no safe local docker runtime")

// isHostInventoryCommand reports one of the four host.* ids, exactly.
func isHostInventoryCommand(command string) bool {
	switch command {
	case sjHostMemory, sjHostTopMemory, sjHostLoad, sjHostContainers:
		return true
	}
	return false
}

// hostArgsEmpty validates a host.* id's args: exactly the empty JSON object
// (null, an array, an absent member or any member are refused).
func hostArgsEmpty(raw json.RawMessage) error {
	var a map[string]json.RawMessage
	if err := json.Unmarshal(raw, &a); err != nil {
		return err
	}
	if a == nil || len(a) != 0 {
		return errors.New("host inventory commands take no args")
	}
	return nil
}

// hostInventorySeams is the host surface the host.* ids read through.
type hostInventorySeams struct {
	// goos selects the per-OS table (runtime.GOOS in production).
	goos string
	// run executes argv under env and returns stdout and the exit code; a
	// non-nil error means the command did not run to an exit status.
	run func(ctx context.Context, env, argv []string) (string, int, error)
	// readFile / readDir read /proc (linux).
	readFile func(path string) ([]byte, error)
	readDir  func(path string) ([]os.DirEntry, error)
	// dockerEnv returns the env pinning a safe local docker endpoint and a
	// runner-minted, credential-free DOCKER_CONFIG, plus its cleanup.
	dockerEnv func(ctx context.Context) ([]string, func(), error)
	// timeout bounds each command or read.
	timeout time.Duration
}

func defaultHostInventorySeams() hostInventorySeams {
	return hostInventorySeams{
		goos:      runtime.GOOS,
		run:       hostExec,
		readFile:  os.ReadFile,
		readDir:   os.ReadDir,
		dockerEnv: hostDockerEnv,
		timeout:   hostInventoryTimeout,
	}
}

// hostBaseEnv is the whole environment of a host.* command: PATH and a fixed
// locale. Nothing else of the runner's environment (no token) is passed on.
func hostBaseEnv() []string {
	return []string{"PATH=" + os.Getenv("PATH"), "LC_ALL=C"}
}

// errHostProgramNotAllowed: a host.* argv names a program outside the closed
// set hostCommand builds.
var errHostProgramNotAllowed = errors.New("host inventory: program not in the host command set")

// hostExec is the production run seam: the program must be one hostCommand
// builds, then runHostCmd runs it.
func hostExec(ctx context.Context, env, argv []string) (string, int, error) {
	cmd, err := hostCommand(ctx, argv)
	if err != nil {
		return "", -1, err
	}
	return runHostCmd(cmd, env)
}

// hostCommand builds the command for a host.* argv. Every program is a string
// literal from the closed set the per-OS tables run, so this seam cannot exec
// any other binary (the coding agent included) and needs no entry in the
// #3945 spawn-site allowlist; any other argv[0] is refused.
func hostCommand(ctx context.Context, argv []string) (*exec.Cmd, error) {
	if len(argv) == 0 {
		return nil, fmt.Errorf("%w: empty argv", errHostProgramNotAllowed)
	}
	args := argv[1:]
	switch argv[0] {
	case "vm_stat":
		return exec.CommandContext(ctx, "vm_stat", args...), nil
	case "sysctl":
		return exec.CommandContext(ctx, "sysctl", args...), nil
	case "top":
		return exec.CommandContext(ctx, "top", args...), nil
	case "uptime":
		return exec.CommandContext(ctx, "uptime", args...), nil
	case "ps":
		return exec.CommandContext(ctx, "ps", args...), nil
	case "docker":
		return exec.CommandContext(ctx, "docker", args...), nil
	}
	return nil, fmt.Errorf("%w: %q", errHostProgramNotAllowed, argv[0])
}

// runHostCmd runs cmd under env: stdout capped, stderr discarded, a 2s
// WaitDelay so an inherited pipe cannot outlive the deadline.
func runHostCmd(cmd *exec.Cmd, env []string) (string, int, error) {
	cmd.Env = env
	cmd.WaitDelay = 2 * time.Second
	var stdout cappedBuffer
	stdout.limit = hostInventoryOutputLimit
	cmd.Stdout = &stdout
	cmd.Stderr = io.Discard
	err := cmd.Run()
	var exitErr *exec.ExitError
	switch {
	case err == nil:
		return stdout.String(), 0, nil
	case errors.As(err, &exitErr):
		return stdout.String(), exitErr.ExitCode(), nil
	}
	return stdout.String(), -1, err
}

// hostDockerEnv is the production dockerEnv seam: the docker CLI's endpoint
// must classify as a safe local socket (gateiso.DetectRuntime), and every
// call runs under a runner-minted, credential-free DOCKER_CONFIG
// (gateiso.NewAnonymousDockerConfig), so no credential helper is consulted
// (the #4046 locked-screen hang).
func hostDockerEnv(ctx context.Context) ([]string, func(), error) {
	rt := gateiso.DetectRuntime(ctx, gateiso.DefaultProbes())
	if rt.Kind != gateiso.KindDocker || !rt.Safe {
		return nil, nil, fmt.Errorf("%w (%s: %s)", errHostDockerUnavailable, rt.Kind, rt.Reason)
	}
	cfg, err := gateiso.NewAnonymousDockerConfig(nil)
	if err != nil {
		return nil, nil, err
	}
	env, err := rt.BindEndpointEnv(hostBaseEnv(), cfg.Dir)
	if err != nil {
		_ = cfg.Remove()
		return nil, nil, err
	}
	return env, func() { _ = cfg.Remove() }, nil
}

// hostOutput accumulates one host.* job's sections and its first failure.
type hostOutput struct {
	b        strings.Builder
	code     int
	timedOut bool
	err      error
}

func (h *hostOutput) section(title string) { h.b.WriteString("# " + title + "\n") }

func (h *hostOutput) write(s string) {
	h.b.WriteString(s)
	if s != "" && !strings.HasSuffix(s, "\n") {
		h.b.WriteString("\n")
	}
}

func (h *hostOutput) failed() bool { return h.timedOut || h.err != nil || h.code != 0 }

// run executes argv under its own deadline after a section header naming
// it (title, or the argv when title is empty, so a header never echoes a
// host-supplied argument). It returns the stdout and whether the command
// exited 0.
func (h *hostOutput) run(ctx context.Context, s hostInventorySeams, env []string, title string, argv []string) (string, bool) {
	cctx, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()
	if title == "" {
		title = strings.Join(argv, " ")
	}
	h.section(title)
	out, code, err := s.run(cctx, env, argv)
	switch {
	case errors.Is(cctx.Err(), context.DeadlineExceeded):
		h.timedOut, h.code = true, -1
	case err != nil:
		h.err, h.code = err, -1
	case code != 0:
		h.code = code
	}
	return out, !h.failed()
}

// readFile reads one fixed path. An optional file that does not exist is
// recorded as unavailable (a signal the kernel does not expose), not a
// failure.
func (h *hostOutput) readFile(s hostInventorySeams, path string, optional bool) (string, bool) {
	h.section(path)
	b, err := s.readFile(path)
	switch {
	case err == nil:
		return string(b), true
	case optional && errors.Is(err, fs.ErrNotExist):
		h.write("unavailable")
		return "", false
	}
	h.err, h.code = err, -1
	return "", false
}

// hostCollector gathers one host.* command's output into h.
type hostCollector func(ctx context.Context, s hostInventorySeams, h *hostOutput)

// hostInventoryTable is the closed per-OS table. A GOOS absent here answers
// not_applicable; every supported OS carries all four ids.
var hostInventoryTable = map[string]map[string]hostCollector{
	"darwin": {
		sjHostMemory:     darwinHostMemory,
		sjHostTopMemory:  darwinHostTopMemory,
		sjHostLoad:       darwinHostLoad,
		sjHostContainers: hostContainers,
	},
	"linux": {
		sjHostMemory:     linuxHostMemory,
		sjHostTopMemory:  linuxHostTopMemory,
		sjHostLoad:       linuxHostLoad,
		sjHostContainers: hostContainers,
	},
}

// executeHostInventory runs one host.* id from the per-OS table and returns
// the bounded, redacted lines as typed inventory evidence: pass when every
// read succeeded, inconclusive timeout / execution_error otherwise, and
// inconclusive not_applicable (nothing run) on an OS outside the table.
func executeHostInventory(ctx context.Context, s hostInventorySeams, command string) specialistJobOutcome {
	collect, ok := hostInventoryTable[s.goos][command]
	if !ok {
		if _, supported := hostInventoryTable[s.goos]; !supported {
			return inconclusive(sjReasonNotApplicable)
		}
		return inconclusive(sjReasonInputsInvalid)
	}
	var h hostOutput
	collect(ctx, s, &h)
	lines, truncated := boundInventoryLines(stripHostControl(h.b.String()), inventoryMaxLines)
	ev := sjInventoryEvidence{Command: command, Lines: lines, Truncated: truncated, ExitCode: h.code}
	switch {
	case h.timedOut:
		return specialistJobOutcome{Verdict: sjVerdictInconclusive, Reason: sjReasonTimeout, Evidence: ev}
	case h.failed():
		return specialistJobOutcome{Verdict: sjVerdictInconclusive, Reason: sjReasonExecutionError, Evidence: ev}
	}
	return specialistJobOutcome{Verdict: sjVerdictPass, Evidence: ev}
}

// stripHostControl drops every control character except newline and tab, and
// every format character (bidi overrides, zero-width joiners), so a planted
// process or container name cannot carry terminal escapes or hidden text.
func stripHostControl(s string) string {
	return strings.Map(func(r rune) rune {
		if r == '\n' || r == '\t' {
			return r
		}
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
			return -1
		}
		return r
	}, strings.ToValidUTF8(s, "�"))
}

// hostField cleans one parsed host name: control and format characters
// stripped, whitespace collapsed to `_` so it stays one field, and cut to
// hostNameMaxBytes on a rune boundary.
func hostField(s string) string {
	s = strings.Map(func(r rune) rune {
		if unicode.IsSpace(r) {
			return '_'
		}
		return r
	}, stripHostControl(s))
	if len(s) <= hostNameMaxBytes {
		return s
	}
	cut := hostNameMaxBytes
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut]
}

// ---- darwin ----

func darwinHostMemory(ctx context.Context, s hostInventorySeams, h *hostOutput) {
	for _, argv := range [][]string{{"vm_stat"}, {"sysctl", "-n", "vm.swapusage"}} {
		out, ok := h.run(ctx, s, hostBaseEnv(), "", argv)
		h.write(out)
		if !ok {
			return
		}
	}
}

func darwinHostTopMemory(ctx context.Context, s hostInventorySeams, h *hostOutput) {
	out, _ := h.run(ctx, s, hostBaseEnv(), "", []string{"top", "-l", "1", "-o", "mem", "-n", strconv.Itoa(hostTopProcesses), "-stats", "pid,command,mem,cmprs"})
	h.write(out)
}

// darwinHostLoad: the load averages, then the runnable (R) and
// uninterruptible (U) process counts from ps — a load driven by
// uninterruptible waits is I/O or swap, not CPU.
func darwinHostLoad(ctx context.Context, s hostInventorySeams, h *hostOutput) {
	out, ok := h.run(ctx, s, hostBaseEnv(), "", []string{"uptime"})
	h.write(out)
	if !ok {
		return
	}
	out, ok = h.run(ctx, s, hostBaseEnv(), "", []string{"ps", "-axo", "stat="})
	if !ok {
		return
	}
	var runnable, uninterruptible, total int
	sc := bufio.NewScanner(strings.NewReader(out))
	for sc.Scan() {
		st := strings.TrimSpace(sc.Text())
		if st == "" {
			continue
		}
		total++
		switch st[0] {
		case 'R':
			runnable++
		case 'U':
			uninterruptible++
		}
	}
	h.write(fmt.Sprintf("processes %d\nrunnable %d\nuninterruptible %d", total, runnable, uninterruptible))
}

// ---- linux ----

func linuxHostMemory(_ context.Context, s hostInventorySeams, h *hostOutput) {
	out, ok := h.readFile(s, "/proc/meminfo", false)
	h.write(out)
	if !ok {
		return
	}
	out, _ = h.readFile(s, "/proc/pressure/memory", true)
	h.write(out)
}

// linuxHostLoad: /proc/loadavg, then procs_running / procs_blocked from
// /proc/stat (blocked = uninterruptible, the I/O / swap-wait signal).
func linuxHostLoad(_ context.Context, s hostInventorySeams, h *hostOutput) {
	out, ok := h.readFile(s, "/proc/loadavg", false)
	h.write(out)
	if !ok {
		return
	}
	out, ok = h.readFile(s, "/proc/stat", false)
	if !ok {
		return
	}
	for _, l := range strings.Split(out, "\n") {
		if strings.HasPrefix(l, "procs_running ") || strings.HasPrefix(l, "procs_blocked ") {
			h.write(l)
		}
	}
}

type linuxProc struct {
	pid             int
	name            string
	rssKB, swapKB   int64
	hasResidentSize bool
}

// linuxHostTopMemory: every numeric /proc entry's status (Name, VmRSS,
// VmSwap), sorted by resident + swapped descending, top hostTopProcesses. A
// pid that vanished between the listing and the read is skipped, as is a
// kernel thread (no VmRSS).
func linuxHostTopMemory(_ context.Context, s hostInventorySeams, h *hostOutput) {
	h.section(fmt.Sprintf("/proc/<pid>/status VmRSS+VmSwap, top %d", hostTopProcesses))
	entries, err := s.readDir("/proc")
	if err != nil {
		h.err, h.code = err, -1
		return
	}
	var procs []linuxProc
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil || pid <= 0 {
			continue
		}
		b, err := s.readFile(filepath.Join("/proc", e.Name(), "status"))
		if err != nil {
			continue
		}
		if p, ok := parseLinuxStatus(pid, string(b)); ok {
			procs = append(procs, p)
		}
	}
	sort.SliceStable(procs, func(i, j int) bool {
		a, b := procs[i].rssKB+procs[i].swapKB, procs[j].rssKB+procs[j].swapKB
		if a != b {
			return a > b
		}
		return procs[i].pid < procs[j].pid
	})
	if len(procs) > hostTopProcesses {
		procs = procs[:hostTopProcesses]
	}
	for _, p := range procs {
		h.write(fmt.Sprintf("pid=%d name=%s rss_kb=%d swap_kb=%d", p.pid, hostField(p.name), p.rssKB, p.swapKB))
	}
}

func parseLinuxStatus(pid int, status string) (linuxProc, bool) {
	p := linuxProc{pid: pid}
	for _, l := range strings.Split(status, "\n") {
		k, v, ok := strings.Cut(l, ":")
		if !ok {
			continue
		}
		v = strings.TrimSpace(v)
		switch k {
		case "Name":
			p.name = v
		case "VmRSS":
			p.rssKB, p.hasResidentSize = statusKB(v), true
		case "VmSwap":
			p.swapKB = statusKB(v)
		}
	}
	return p, p.hasResidentSize
}

// statusKB parses a "<n> kB" status value (0 on anything else).
func statusKB(v string) int64 {
	n, err := strconv.ParseInt(strings.TrimSpace(strings.TrimSuffix(v, "kB")), 10, 64)
	if err != nil || n < 0 {
		return 0
	}
	return n
}

// ---- both ----

// hostContainers: the runner's fishhawk-gate- containers and their memory,
// under a credential-free docker config. Only names carrying the exact
// prefix are counted or passed on, whatever docker's substring filter
// returned.
func hostContainers(ctx context.Context, s hostInventorySeams, h *hostOutput) {
	h.section("docker (credential-free config)")
	env, cleanup, err := s.dockerEnv(ctx)
	if err != nil {
		h.err, h.code = err, -1
		return
	}
	defer cleanup()
	out, ok := h.run(ctx, s, env, "", []string{"docker", "ps", "--filter", "name=" + hostGatePrefix, "--format", "{{.Names}}"})
	if !ok {
		return
	}
	var names []string
	for _, n := range strings.Split(out, "\n") {
		n = strings.TrimSpace(n)
		if strings.HasPrefix(n, hostGatePrefix) && len(names) < hostContainerMax {
			names = append(names, n)
		}
	}
	h.write(fmt.Sprintf("%s containers %d", hostGatePrefix, len(names)))
	if len(names) == 0 {
		return
	}
	argv := append([]string{"docker", "stats", "--no-stream", "--format", "{{.Name}}\t{{.MemUsage}}"}, names...)
	out, ok = h.run(ctx, s, env, "docker stats --no-stream <fishhawk-gate- containers>", argv)
	if !ok {
		return
	}
	for _, l := range strings.Split(out, "\n") {
		name, usage, _ := strings.Cut(strings.TrimSpace(l), "\t")
		if strings.HasPrefix(name, hostGatePrefix) {
			h.write(hostField(name) + "\t" + stripHostControl(usage))
		}
	}
}
