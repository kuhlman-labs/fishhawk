package hosthealth

import (
	"context"
	"errors"
	"fmt"
	"os"
	"runtime"
	"strconv"
	"strings"
)

// linuxReader reads Linux through /proc only (proc(5),
// https://docs.kernel.org/filesystems/proc.html, and PSI,
// https://docs.kernel.org/accounting/psi.html).
type linuxReader struct {
	readFile func(string) ([]byte, error)
	readDir  func(string) ([]os.DirEntry, error)
}

const procPressureMemory = "/proc/pressure/memory"

func (l linuxReader) memory(context.Context) (Memory, error) {
	b, err := l.readFile("/proc/meminfo")
	if err != nil {
		return Memory{}, fmt.Errorf("/proc/meminfo unreadable: %w", err)
	}
	m, err := parseMeminfo(string(b))
	if err != nil {
		return Memory{}, err
	}
	some, full, ok, err := l.psi(procPressureMemory)
	if err != nil {
		return Memory{}, err
	}
	m.Pressure = Pressure{Source: procPressureMemory, Level: LevelUnknown, SomeAvg10: some, FullAvg10: full, Available: ok}
	return m, nil
}

// psi reads one /proc/pressure file. Any read error (absent with CONFIG_PSI
// off, EOPNOTSUPP under psi=0) means the signal is unavailable, not an
// error; a file that reads but does not parse is an error.
func (l linuxReader) psi(path string) (some, full float64, ok bool, err error) {
	b, rerr := l.readFile(path)
	if rerr != nil {
		return 0, 0, false, nil
	}
	some, full, err = parsePSI(string(b))
	if err != nil {
		return 0, 0, false, fmt.Errorf("%s: %w", path, err)
	}
	return some, full, true, nil
}

// parseMeminfo reads MemTotal, MemAvailable, SwapTotal and SwapFree (kB).
// All four are required: MemAvailable is absent before kernel 3.14, and
// guessing it would risk reading a pressured host as healthy.
func parseMeminfo(s string) (Memory, error) {
	kb := map[string]uint64{}
	for _, line := range strings.Split(s, "\n") {
		key, rest, found := strings.Cut(line, ":")
		if !found {
			continue
		}
		f := strings.Fields(rest)
		if len(f) == 0 {
			continue
		}
		v, err := strconv.ParseUint(f[0], 10, 64)
		if err != nil {
			continue
		}
		kb[strings.TrimSpace(key)] = v
	}
	for _, k := range []string{"MemTotal", "MemAvailable", "SwapTotal", "SwapFree"} {
		if _, ok := kb[k]; !ok {
			return Memory{}, fmt.Errorf("/proc/meminfo: missing %s", k)
		}
	}
	if kb["MemTotal"] == 0 {
		return Memory{}, errors.New("/proc/meminfo: MemTotal is 0")
	}
	if kb["SwapFree"] > kb["SwapTotal"] {
		return Memory{}, errors.New("/proc/meminfo: SwapFree exceeds SwapTotal")
	}
	return Memory{
		TotalBytes:     kb["MemTotal"] << 10,
		AvailableBytes: kb["MemAvailable"] << 10,
		SwapTotalBytes: kb["SwapTotal"] << 10,
		SwapUsedBytes:  (kb["SwapTotal"] - kb["SwapFree"]) << 10,
	}, nil
}

// parsePSI reads the avg10 of the `some` line (required) and the `full`
// line (optional: /proc/pressure/cpu has no full line before 5.13).
func parsePSI(s string) (some, full float64, err error) {
	haveSome := false
	for _, line := range strings.Split(s, "\n") {
		f := strings.Fields(line)
		if len(f) < 2 || (f[0] != "some" && f[0] != "full") {
			continue
		}
		var avg10 float64
		found := false
		for _, kv := range f[1:] {
			k, v, ok := strings.Cut(kv, "=")
			if !ok || k != "avg10" {
				continue
			}
			avg10, err = strconv.ParseFloat(v, 64)
			if err != nil || avg10 < 0 || avg10 > 100 {
				return 0, 0, fmt.Errorf("malformed avg10 %q", v)
			}
			found = true
		}
		if !found {
			return 0, 0, fmt.Errorf("%s line has no avg10", f[0])
		}
		if f[0] == "some" {
			some, haveSome = avg10, true
		} else {
			full = avg10
		}
	}
	if !haveSome {
		return 0, 0, errors.New("no some line")
	}
	return some, full, nil
}

func (l linuxReader) processes(ctx context.Context, _ int) ([]Process, error) {
	entries, err := l.readDir("/proc")
	if err != nil {
		return nil, fmt.Errorf("/proc unreadable: %w", err)
	}
	var procs []Process
	for _, e := range entries {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		pid, err := strconv.Atoi(e.Name())
		if err != nil || pid <= 0 {
			continue
		}
		b, err := l.readFile("/proc/" + e.Name() + "/status")
		if err != nil {
			continue // the process exited between the listing and the read
		}
		p, ok := parseProcStatus(pid, string(b))
		if !ok {
			continue
		}
		procs = append(procs, p)
	}
	if len(procs) == 0 {
		return nil, errors.New("/proc: no readable process status")
	}
	return procs, nil
}

// parseProcStatus reads Name, VmRSS and VmSwap (kB). ok is false for a
// status without a Name or with a malformed VmRSS/VmSwap. A kernel thread
// has no Vm* lines and reads as a zero footprint.
func parseProcStatus(pid int, s string) (Process, bool) {
	p := Process{PID: pid}
	haveName := false
	for _, line := range strings.Split(s, "\n") {
		key, rest, found := strings.Cut(line, ":")
		if !found {
			continue
		}
		switch key {
		case "Name":
			p.Name, haveName = SanitizeName(rest), true
		case "VmRSS", "VmSwap":
			f := strings.Fields(rest)
			if len(f) == 0 {
				return Process{}, false
			}
			v, err := strconv.ParseUint(f[0], 10, 64)
			if err != nil {
				return Process{}, false
			}
			if key == "VmRSS" {
				p.ResidentBytes = v << 10
			} else {
				p.CompressedBytes = v << 10
			}
		}
	}
	return p, haveName
}

func (l linuxReader) load(context.Context) (LoadAttribution, error) {
	b, err := l.readFile("/proc/loadavg")
	if err != nil {
		return LoadAttribution{}, fmt.Errorf("/proc/loadavg unreadable: %w", err)
	}
	f := strings.Fields(string(b))
	if len(f) == 0 {
		return LoadAttribution{}, errors.New("/proc/loadavg: empty")
	}
	load1, err := strconv.ParseFloat(f[0], 64)
	if err != nil || load1 < 0 {
		return LoadAttribution{}, fmt.Errorf("/proc/loadavg: malformed %q", f[0])
	}
	b, err = l.readFile("/proc/stat")
	if err != nil {
		return LoadAttribution{}, fmt.Errorf("/proc/stat unreadable: %w", err)
	}
	running, blocked, err := parseProcStat(string(b))
	if err != nil {
		return LoadAttribution{}, err
	}
	a := LoadAttribution{Load1: load1, Runnable: running, Uninterruptible: blocked, Cores: runtime.NumCPU()}
	ioSome, _, ioOK, err := l.psi("/proc/pressure/io")
	if err != nil {
		return LoadAttribution{}, err
	}
	cpuSome, _, cpuOK, err := l.psi("/proc/pressure/cpu")
	if err != nil {
		return LoadAttribution{}, err
	}
	if ioOK && cpuOK {
		a.IOSomeAvg10, a.CPUSomeAvg10, a.HasPSI = ioSome, cpuSome, true
	}
	return a, nil
}

// parseProcStat reads procs_running and procs_blocked; both are required.
func parseProcStat(s string) (running, blocked int, err error) {
	have := map[string]int{}
	for _, line := range strings.Split(s, "\n") {
		f := strings.Fields(line)
		if len(f) != 2 || (f[0] != "procs_running" && f[0] != "procs_blocked") {
			continue
		}
		v, perr := strconv.Atoi(f[1])
		if perr != nil || v < 0 {
			return 0, 0, fmt.Errorf("/proc/stat: malformed %s %q", f[0], f[1])
		}
		have[f[0]] = v
	}
	r, okR := have["procs_running"]
	bl, okB := have["procs_blocked"]
	if !okR || !okB {
		return 0, 0, errors.New("/proc/stat: missing procs_running or procs_blocked")
	}
	return r, bl, nil
}
