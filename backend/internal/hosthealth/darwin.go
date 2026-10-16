package hosthealth

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"runtime"
	"strconv"
	"strings"
)

// darwinReader reads macOS through commands only (sysctl, vm_stat, top, ps).
type darwinReader struct {
	exec ExecFunc
}

// darwinPressureSysctl is the macOS memory-pressure level sysctl.
const darwinPressureSysctl = "kern.memorystatus_vm_pressure_level"

func (d darwinReader) run(ctx context.Context, name string, args ...string) (string, error) {
	out, err := d.exec(ctx, name, args...)
	if err != nil {
		return "", fmt.Errorf("%s failed: %w", name, err)
	}
	return string(out), nil
}

func (d darwinReader) memory(ctx context.Context) (Memory, error) {
	out, err := d.run(ctx, "sysctl", "-n", "hw.memsize")
	if err != nil {
		return Memory{}, err
	}
	total, err := strconv.ParseUint(strings.TrimSpace(out), 10, 64)
	if err != nil || total == 0 {
		return Memory{}, fmt.Errorf("malformed hw.memsize %q", strings.TrimSpace(out))
	}
	out, err = d.run(ctx, "vm_stat")
	if err != nil {
		return Memory{}, err
	}
	avail, compressed, err := parseVMStat(out)
	if err != nil {
		return Memory{}, err
	}
	out, err = d.run(ctx, "sysctl", "-n", "vm.swapusage")
	if err != nil {
		return Memory{}, err
	}
	swapTotal, swapUsed, err := parseSwapUsage(out)
	if err != nil {
		return Memory{}, err
	}
	m := Memory{
		TotalBytes:      total,
		AvailableBytes:  avail,
		CompressedBytes: compressed,
		SwapTotalBytes:  swapTotal,
		SwapUsedBytes:   swapUsed,
		Pressure:        Pressure{Source: darwinPressureSysctl, Level: LevelUnknown},
	}
	// The pressure level is an extra signal: an unreadable sysctl leaves it
	// unavailable rather than failing the swap and available readings.
	if out, err := d.run(ctx, "sysctl", "-n", darwinPressureSysctl); err == nil {
		m.Pressure.Available = true
		m.Pressure.Level = parseDarwinPressureLevel(out)
	}
	return m, nil
}

// parseDarwinPressureLevel maps NOTE_MEMORYSTATUS_PRESSURE_* (xnu
// bsd/sys/event.h): 1 normal, 2 warn, 4 critical. Anything else is unknown,
// never normal.
func parseDarwinPressureLevel(s string) Level {
	switch strings.TrimSpace(s) {
	case "1":
		return LevelNormal
	case "2":
		return LevelWarn
	case "4":
		return LevelCritical
	default:
		return LevelUnknown
	}
}

var (
	vmStatPageSizeRe = regexp.MustCompile(`page size of (\d+) bytes`)
	vmStatLineRe     = regexp.MustCompile(`^"?([A-Za-z][A-Za-z -]*?)"?:\s+(\d+)\.?\s*$`)
)

// parseVMStat returns available bytes (free + inactive + speculative pages)
// and compressor-occupied bytes. Every one of the four counters and the page
// size must be present.
func parseVMStat(s string) (available, compressed uint64, err error) {
	m := vmStatPageSizeRe.FindStringSubmatch(s)
	if m == nil {
		return 0, 0, errors.New("vm_stat: no page size header")
	}
	pageSize, err := strconv.ParseUint(m[1], 10, 64)
	if err != nil || pageSize == 0 {
		return 0, 0, fmt.Errorf("vm_stat: malformed page size %q", m[1])
	}
	pages := map[string]uint64{}
	for _, line := range strings.Split(s, "\n") {
		lm := vmStatLineRe.FindStringSubmatch(strings.TrimSpace(line))
		if lm == nil {
			continue
		}
		v, perr := strconv.ParseUint(lm[2], 10, 64)
		if perr != nil {
			return 0, 0, fmt.Errorf("vm_stat: malformed %q", lm[1])
		}
		pages[lm[1]] = v
	}
	need := []string{"Pages free", "Pages inactive", "Pages speculative", "Pages occupied by compressor"}
	for _, k := range need {
		if _, ok := pages[k]; !ok {
			return 0, 0, fmt.Errorf("vm_stat: missing %q", k)
		}
	}
	available = (pages["Pages free"] + pages["Pages inactive"] + pages["Pages speculative"]) * pageSize
	compressed = pages["Pages occupied by compressor"] * pageSize
	return available, compressed, nil
}

var swapUsageRe = regexp.MustCompile(`total\s*=\s*([0-9.]+[KMGT]?)\s+used\s*=\s*([0-9.]+[KMGT]?)`)

// parseSwapUsage parses `total = 3072.00M  used = 1482.00M  free = ...`.
func parseSwapUsage(s string) (total, used uint64, err error) {
	m := swapUsageRe.FindStringSubmatch(s)
	if m == nil {
		return 0, 0, fmt.Errorf("vm.swapusage: unrecognized %q", strings.TrimSpace(s))
	}
	if total, err = parseBinarySize(m[1]); err != nil {
		return 0, 0, fmt.Errorf("vm.swapusage total: %w", err)
	}
	if used, err = parseBinarySize(m[2]); err != nil {
		return 0, 0, fmt.Errorf("vm.swapusage used: %w", err)
	}
	if used > total {
		return 0, 0, fmt.Errorf("vm.swapusage: used %d exceeds total %d", used, total)
	}
	return total, used, nil
}

func (d darwinReader) processes(ctx context.Context, n int) ([]Process, error) {
	out, err := d.run(ctx, "top", "-l", "1", "-o", "mem", "-n", strconv.Itoa(n), "-stats", "pid,command,mem,cmprs")
	if err != nil {
		return nil, err
	}
	return parseDarwinTop(out)
}

// parseDarwinTop parses `top -l 1 -stats pid,command,mem,cmprs`: the summary
// block, then the `PID COMMAND MEM CMPRS` header, then one row per process.
// PID is the first field, CMPRS the last, MEM the second-last, and COMMAND
// is everything between (so a name with spaces survives). Any malformed row
// fails the whole read: rows are sorted by memory, so skipping one could
// hide the largest holder.
func parseDarwinTop(s string) ([]Process, error) {
	lines := strings.Split(s, "\n")
	start := -1
	for i, line := range lines {
		if f := strings.Fields(line); len(f) == 4 && f[0] == "PID" && f[1] == "COMMAND" && f[2] == "MEM" && f[3] == "CMPRS" {
			start = i + 1
			break
		}
	}
	if start < 0 {
		return nil, errors.New("top: no PID COMMAND MEM CMPRS header")
	}
	var procs []Process
	for _, line := range lines[start:] {
		line = strings.TrimRight(line, " \t\r")
		if strings.TrimSpace(line) == "" {
			continue
		}
		p, err := parseDarwinTopRow(line)
		if err != nil {
			return nil, err
		}
		procs = append(procs, p)
	}
	if len(procs) == 0 {
		return nil, errors.New("top: no process rows")
	}
	return procs, nil
}

func parseDarwinTopRow(line string) (Process, error) {
	rest, cmprsField := cutLastField(line)
	rest, memField := cutLastField(rest)
	rest = strings.TrimLeft(rest, " \t")
	pidEnd := strings.IndexAny(rest, " \t")
	if pidEnd <= 0 || cmprsField == "" || memField == "" {
		return Process{}, fmt.Errorf("top: malformed row %q", SanitizeName(line))
	}
	// PID 0 is kernel_task, a legitimate (often large) row.
	pid, err := strconv.Atoi(rest[:pidEnd])
	if err != nil || pid < 0 {
		return Process{}, fmt.Errorf("top: malformed pid in row %q", SanitizeName(line))
	}
	name := strings.TrimSpace(rest[pidEnd:])
	if name == "" {
		return Process{}, fmt.Errorf("top: empty command in row for pid %d", pid)
	}
	mem, err := parseBinarySize(memField)
	if err != nil {
		return Process{}, fmt.Errorf("top: pid %d MEM: %w", pid, err)
	}
	cmprs, err := parseBinarySize(cmprsField)
	if err != nil {
		return Process{}, fmt.Errorf("top: pid %d CMPRS: %w", pid, err)
	}
	return Process{
		PID:                        pid,
		Name:                       SanitizeName(name),
		ResidentBytes:              mem,
		CompressedBytes:            cmprs,
		ResidentIncludesCompressed: true,
	}, nil
}

// cutLastField splits off the last whitespace-separated field.
func cutLastField(s string) (rest, field string) {
	s = strings.TrimRight(s, " \t")
	i := strings.LastIndexAny(s, " \t")
	if i < 0 {
		return "", s
	}
	return s[:i], s[i+1:]
}

func (d darwinReader) load(ctx context.Context) (LoadAttribution, error) {
	out, err := d.run(ctx, "sysctl", "-n", "vm.loadavg")
	if err != nil {
		return LoadAttribution{}, err
	}
	load1, err := parseSysctlLoadavg(out)
	if err != nil {
		return LoadAttribution{}, err
	}
	out, err = d.run(ctx, "ps", "-axo", "stat=")
	if err != nil {
		return LoadAttribution{}, err
	}
	runnable, uninterruptible, err := countProcessStates(out)
	if err != nil {
		return LoadAttribution{}, err
	}
	return LoadAttribution{
		Load1:           load1,
		Runnable:        runnable,
		Uninterruptible: uninterruptible,
		Cores:           runtime.NumCPU(),
	}, nil
}

// parseSysctlLoadavg parses macOS's `{ 7.19 6.83 5.76 }`.
func parseSysctlLoadavg(s string) (float64, error) {
	fields := strings.Fields(strings.Trim(strings.TrimSpace(s), "{}"))
	if len(fields) == 0 {
		return 0, errors.New("vm.loadavg: empty")
	}
	v, err := strconv.ParseFloat(fields[0], 64)
	if err != nil || v < 0 {
		return 0, fmt.Errorf("vm.loadavg: malformed %q", fields[0])
	}
	return v, nil
}

// countProcessStates counts `ps -axo stat=` lines by their first state
// letter: R is runnable; U (macOS) and D (procps) are uninterruptible wait.
func countProcessStates(s string) (runnable, uninterruptible int, err error) {
	lines := 0
	for _, line := range strings.Split(s, "\n") {
		st := strings.TrimSpace(line)
		if st == "" {
			continue
		}
		lines++
		switch st[0] {
		case 'R':
			runnable++
		case 'U', 'D':
			uninterruptible++
		}
	}
	if lines == 0 {
		return 0, 0, errors.New("ps: no process states")
	}
	return runnable, uninterruptible, nil
}
