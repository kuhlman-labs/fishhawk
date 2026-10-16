// Package hosthealth reads host memory, swap, the OS memory-pressure signal,
// per-process memory footprint, load attribution and Fishhawk gate-container
// memory on macOS and Linux, and holds the thresholds, the hysteresis latch
// and the generic per-OS remedy text built on those readings (E85.11 /
// #4330). It is the portable probe source ADR-100 D3 says #4330 and the
// concurrency controller share. Stdlib only, so any package can import it.
//
// The OS is selected at RUNTIME from Reader.GOOS (no build tags), so both
// OS parsers compile and their fixtures run on every host. Contract:
// README.md.
package hosthealth

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"
)

// ErrNotSupported is returned by every Reader method on a GOOS other than
// darwin or linux. Callers map it to not_applicable, never to ok.
var ErrNotSupported = errors.New("hosthealth: operating system not supported")

// ExecFunc runs one command and returns its stdout. It has the same shape as
// opsmonitor.ExecFunc, so a caller can convert one into the other.
type ExecFunc func(ctx context.Context, name string, args ...string) ([]byte, error)

// execWaitDelay bounds Wait after the child exits or ctx is done, so a
// grandchild that inherited stdout cannot keep the read blocked.
const execWaitDelay = 2 * time.Second

func defaultExec(ctx context.Context, name string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.WaitDelay = execWaitDelay
	return cmd.Output()
}

// Reader reads the host. Every field is optional: the zero Reader reads the
// running host.
type Reader struct {
	// GOOS selects the OS readers; empty means runtime.GOOS.
	GOOS string
	// Exec runs commands (darwin readers, docker); nil runs
	// exec.CommandContext with a 2s WaitDelay.
	Exec ExecFunc
	// ReadFile reads a file (linux readers); nil is os.ReadFile.
	ReadFile func(name string) ([]byte, error)
	// ReadDir lists a directory (linux process scan); nil is os.ReadDir.
	ReadDir func(name string) ([]os.DirEntry, error)
}

// OS is the effective GOOS the Reader dispatches on.
func (r Reader) OS() string {
	if r.GOOS != "" {
		return r.GOOS
	}
	return runtime.GOOS
}

func (r Reader) exec() ExecFunc {
	if r.Exec != nil {
		return r.Exec
	}
	return defaultExec
}

func (r Reader) readFile() func(string) ([]byte, error) {
	if r.ReadFile != nil {
		return r.ReadFile
	}
	return os.ReadFile
}

func (r Reader) readDir() func(string) ([]os.DirEntry, error) {
	if r.ReadDir != nil {
		return r.ReadDir
	}
	return os.ReadDir
}

// osReader is one OS's set of readers.
type osReader interface {
	memory(ctx context.Context) (Memory, error)
	processes(ctx context.Context, n int) ([]Process, error)
	load(ctx context.Context) (LoadAttribution, error)
}

// platform is the single OS gate: darwin and linux get their readers, every
// other GOOS gets ErrNotSupported and no reader runs.
func (r Reader) platform() (osReader, error) {
	switch goos := r.OS(); goos {
	case "darwin":
		return darwinReader{exec: r.exec()}, nil
	case "linux":
		return linuxReader{readFile: r.readFile(), readDir: r.readDir()}, nil
	default:
		return nil, fmt.Errorf("%w: %s", ErrNotSupported, goos)
	}
}

// DefaultTopProcesses is how many processes Processes returns when n <= 0.
const DefaultTopProcesses = 20

// Memory reads host memory, swap and the OS pressure signal.
func (r Reader) Memory(ctx context.Context) (Memory, error) {
	p, err := r.platform()
	if err != nil {
		return Memory{}, err
	}
	return p.memory(ctx)
}

// Processes returns at most n processes (DefaultTopProcesses when n <= 0),
// largest footprint first. Names are passed through SanitizeName.
func (r Reader) Processes(ctx context.Context, n int) ([]Process, error) {
	p, err := r.platform()
	if err != nil {
		return nil, err
	}
	if n <= 0 {
		n = DefaultTopProcesses
	}
	procs, err := p.processes(ctx, n)
	if err != nil {
		return nil, err
	}
	sortByFootprint(procs)
	if len(procs) > n {
		procs = procs[:n]
	}
	return procs, nil
}

// Load reads the 1-minute load average and what it is made of.
func (r Reader) Load(ctx context.Context) (LoadAttribution, error) {
	p, err := r.platform()
	if err != nil {
		return LoadAttribution{}, err
	}
	return p.load(ctx)
}

// --- readings ---------------------------------------------------------------

// Level is a discrete OS memory-pressure level.
type Level string

// Pressure levels. LevelUnknown is never read as normal.
const (
	LevelNormal   Level = "normal"
	LevelWarn     Level = "warn"
	LevelCritical Level = "critical"
	LevelUnknown  Level = "unknown"
)

// Pressure is the OS memory-pressure signal. macOS reports a discrete Level
// (kern.memorystatus_vm_pressure_level); Linux reports PSI stall percentages
// (SomeAvg10, FullAvg10) and leaves Level unknown, because PSI has no
// discrete level: consumers threshold SomeAvg10 against Thresholds.
type Pressure struct {
	// Source names the OS signal read (a sysctl name or a /proc file).
	Source string
	Level  Level
	// SomeAvg10 / FullAvg10 are PSI's 10-second "some" and "full" stall
	// percentages (Linux only; 0 elsewhere).
	SomeAvg10 float64
	FullAvg10 float64
	// Available is false when the signal could not be read (the sysctl
	// failed, or CONFIG_PSI is off); the rest of the reading still holds.
	Available bool
}

// Memory is one host memory reading.
type Memory struct {
	TotalBytes     uint64
	AvailableBytes uint64
	// CompressedBytes is the memory held by the macOS compressor (0 on
	// Linux).
	CompressedBytes uint64
	SwapTotalBytes  uint64
	SwapUsedBytes   uint64
	Pressure        Pressure
}

// AvailableFraction is AvailableBytes / TotalBytes (0 when TotalBytes is 0).
func (m Memory) AvailableFraction() float64 {
	if m.TotalBytes == 0 {
		return 0
	}
	return float64(m.AvailableBytes) / float64(m.TotalBytes)
}

// SwapFraction is SwapUsedBytes / SwapTotalBytes; ok is false when the host
// has no swap configured, so a swapless host never reads a 0% swap signal.
func (m Memory) SwapFraction() (fraction float64, ok bool) {
	if m.SwapTotalBytes == 0 {
		return 0, false
	}
	return float64(m.SwapUsedBytes) / float64(m.SwapTotalBytes), true
}

// Process is one process's memory use.
type Process struct {
	PID  int
	Name string
	// ResidentBytes is top's MEM (the physical footprint) on macOS and
	// VmRSS on Linux.
	ResidentBytes uint64
	// CompressedBytes is top's CMPRS on macOS and VmSwap on Linux.
	CompressedBytes uint64
	// ResidentIncludesCompressed is set by the macOS reader: MEM is the
	// physical footprint, which already counts compressed pages, so the two
	// are not added.
	ResidentIncludesCompressed bool
}

// FootprintBytes is the process's memory including compressed or swapped
// pages. On Linux it is VmRSS + VmSwap. On macOS it is max(MEM, CMPRS): MEM
// already includes compressed memory, but a process whose compressed pages
// are accounted outside its physical footprint (observed for the
// virtualization VM process, CMPRS 21G over MEM 17G) is never under-reported
// below its compressed size.
func (p Process) FootprintBytes() uint64 {
	if p.ResidentIncludesCompressed {
		return max(p.ResidentBytes, p.CompressedBytes)
	}
	sum := p.ResidentBytes + p.CompressedBytes
	if sum < p.ResidentBytes { // overflow: saturate
		return ^uint64(0)
	}
	return sum
}

// LoadAttribution is the 1-minute load average and what it is made of:
// runnable tasks (CPU demand) versus uninterruptible tasks (waiting on I/O,
// which includes swap-in under memory thrash).
type LoadAttribution struct {
	Load1           float64
	Runnable        int
	Uninterruptible int
	Cores           int
	// IOSomeAvg10 / CPUSomeAvg10 are PSI stall percentages, set only when
	// HasPSI (Linux with CONFIG_PSI).
	IOSomeAvg10  float64
	CPUSomeAvg10 float64
	HasPSI       bool
}

// WaitDominated reports whether the load is driven by waiting rather than by
// CPU demand. With PSI the kernel's own stall accounting decides (I/O some >
// CPU some); without it, uninterruptible tasks outnumber runnable ones.
func (a LoadAttribution) WaitDominated() bool {
	if a.HasPSI {
		return a.IOSomeAvg10 > a.CPUSomeAvg10
	}
	return a.Uninterruptible > a.Runnable
}

// --- thresholds -------------------------------------------------------------

const gib = 1 << 30

// Thresholds are the enter/exit bands the latches use. Fractions are in
// (0,1); PSI values are percentages in [0,100].
//
// Direction matters. HIGH-water signals (swap, PSI, container memory) trip
// when the reading rises to Enter and clear below Exit, so Exit < Enter. The
// LOW-water signal (available memory) trips when the reading falls to Enter
// and clears above Exit, so Exit > Enter.
type Thresholds struct {
	// AvailableEnter / AvailableExit: available memory as a fraction of
	// total (low-water). Default 5% / 10%.
	AvailableEnter float64
	AvailableExit  float64
	// SwapEnter / SwapExit: used swap as a fraction of swap total
	// (high-water). Default 90% / 70% (ADR-100 D4 trip / recovery).
	SwapEnter float64
	SwapExit  float64
	// CriticalSwap / CriticalAvailable are the critical-pressure entries
	// (default swap 97%, ADR-100 D4, or available 2%). A critical latch
	// enters there and exits at SwapExit / AvailableExit.
	CriticalSwap      float64
	CriticalAvailable float64
	// PSIEnter / PSIExit: PSI memory some-avg10 percent (high-water).
	// Default 25 / 10.
	PSIEnter float64
	PSIExit  float64
	// HolderMinBytes is the per-process footprint at or over which a
	// process is named as a contributor. It is attribution, not a trigger,
	// so it has no exit. Default max(1 GiB, 25% of RAM).
	HolderMinBytes uint64
	// ContainerMemEnter / ContainerMemExit: total gate-container memory as
	// a fraction of host RAM (high-water). Default 50% / 40%.
	ContainerMemEnter float64
	ContainerMemExit  float64
	// ContainerCountMax: more running gate containers than this trips.
	// Default max(4, cores/2).
	ContainerCountMax int
}

// DefaultThresholds scales the defaults to a host with totalRAM bytes and
// cores CPUs. A zero totalRAM or cores yields the floors (1 GiB, 4).
func DefaultThresholds(totalRAM uint64, cores int) Thresholds {
	return Thresholds{
		AvailableEnter:    0.05,
		AvailableExit:     0.10,
		SwapEnter:         0.90,
		SwapExit:          0.70,
		CriticalSwap:      0.97,
		CriticalAvailable: 0.02,
		PSIEnter:          25,
		PSIExit:           10,
		HolderMinBytes:    max(uint64(gib), totalRAM/4),
		ContainerMemEnter: 0.50,
		ContainerMemExit:  0.40,
		ContainerCountMax: max(4, cores/2),
	}
}

// Validate refuses a band whose exit sits on the wrong side of its enter
// for the signal's direction, a fraction outside (0,1), a critical entry
// no more severe than the failing entry, and a non-positive floor or count.
// Every violation is reported.
func (t Thresholds) Validate() error {
	var errs []error
	fraction := func(name string, v float64) {
		if v <= 0 || v >= 1 {
			errs = append(errs, fmt.Errorf("%s %v must be a fraction in (0,1)", name, v))
		}
	}
	fraction("available enter", t.AvailableEnter)
	fraction("available exit", t.AvailableExit)
	fraction("swap enter", t.SwapEnter)
	fraction("swap exit", t.SwapExit)
	fraction("critical swap", t.CriticalSwap)
	fraction("critical available", t.CriticalAvailable)
	fraction("container memory enter", t.ContainerMemEnter)
	fraction("container memory exit", t.ContainerMemExit)

	lowWater := func(name string, enter, exit float64) {
		if exit <= enter {
			errs = append(errs, fmt.Errorf("%s is low-water: exit %v must be above enter %v", name, exit, enter))
		}
	}
	highWater := func(name string, enter, exit float64) {
		if exit >= enter {
			errs = append(errs, fmt.Errorf("%s is high-water: exit %v must be below enter %v", name, exit, enter))
		}
	}
	lowWater("available memory", t.AvailableEnter, t.AvailableExit)
	highWater("swap", t.SwapEnter, t.SwapExit)
	highWater("psi memory", t.PSIEnter, t.PSIExit)
	highWater("container memory", t.ContainerMemEnter, t.ContainerMemExit)

	if t.CriticalSwap <= t.SwapEnter {
		errs = append(errs, fmt.Errorf("critical swap %v must be above swap enter %v", t.CriticalSwap, t.SwapEnter))
	}
	if t.CriticalAvailable >= t.AvailableEnter {
		errs = append(errs, fmt.Errorf("critical available %v must be below available enter %v", t.CriticalAvailable, t.AvailableEnter))
	}
	if t.PSIEnter <= 0 || t.PSIEnter > 100 || t.PSIExit < 0 {
		errs = append(errs, fmt.Errorf("psi memory enter %v / exit %v must lie in [0,100] with enter > 0", t.PSIEnter, t.PSIExit))
	}
	if t.HolderMinBytes == 0 {
		errs = append(errs, errors.New("holder floor must be positive"))
	}
	if t.ContainerCountMax < 1 {
		errs = append(errs, fmt.Errorf("container count max %d must be >= 1", t.ContainerCountMax))
	}
	return errors.Join(errs...)
}

// --- latch ------------------------------------------------------------------

// Latch is a hysteresis latch: it latches when a reading passes its enter
// threshold and unlatches only once the reading has recovered past its exit
// threshold, so a reading oscillating inside the band crosses once. Safe for
// concurrent use; the zero Latch is unlatched.
type Latch struct {
	mu      sync.Mutex
	latched bool
}

// Update advances the latch by one observation. crossed is true only on the
// unlatched -> latched edge.
func (l *Latch) Update(aboveEnter, belowExit bool) (latched, crossed bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	switch {
	case !l.latched && aboveEnter:
		l.latched = true
		return true, true
	case l.latched && belowExit:
		l.latched = false
	}
	return l.latched, false
}

// UpdateHighWater feeds a high-water reading: enter at v >= enter, clear at
// v < exit.
func (l *Latch) UpdateHighWater(v, enter, exit float64) (latched, crossed bool) {
	return l.Update(v >= enter, v < exit)
}

// UpdateLowWater feeds a low-water reading: enter at v <= enter, clear at
// v > exit.
func (l *Latch) UpdateLowWater(v, enter, exit float64) (latched, crossed bool) {
	return l.Update(v <= enter, v > exit)
}

// Latched reports the current state without advancing it.
func (l *Latch) Latched() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.latched
}

// --- names ------------------------------------------------------------------

// MaxNameBytes bounds a process or container name once sanitized.
const MaxNameBytes = 64

// SanitizeName makes a host-supplied name safe to put in an audit payload or
// remedy text: invalid UTF-8, control characters (newlines included) and
// Unicode format characters (bidi overrides) are dropped, surrounding space
// is trimmed, and the result is cut to MaxNameBytes on a rune boundary.
// Process names are host data, never instructions.
func SanitizeName(s string) string {
	var b strings.Builder
	for _, r := range strings.ToValidUTF8(s, "") {
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
			continue
		}
		b.WriteRune(r)
	}
	out := strings.TrimSpace(b.String())
	if len(out) <= MaxNameBytes {
		return out
	}
	cut := MaxNameBytes
	for cut > 0 && !utf8.RuneStart(out[cut]) {
		cut--
	}
	return strings.TrimSpace(out[:cut])
}

// --- shared parsing ---------------------------------------------------------

// parseBinarySize parses a size with an optional binary-unit suffix
// (B, K, M, G, T, case-insensitive, an optional trailing "iB"/"B") and
// top's trailing +/- delta markers, e.g. "41G", "1097M+", "512K-", "0B".
func parseBinarySize(s string) (uint64, error) {
	t := strings.TrimRight(strings.TrimSpace(s), "+-")
	if t == "" {
		return 0, errors.New("empty size")
	}
	i := 0
	for i < len(t) && (t[i] == '.' || (t[i] >= '0' && t[i] <= '9')) {
		i++
	}
	num, unit := t[:i], strings.ToUpper(t[i:])
	v, err := strconv.ParseFloat(num, 64)
	if err != nil || v < 0 {
		return 0, fmt.Errorf("malformed size %q", s)
	}
	var mult float64
	switch strings.TrimSuffix(strings.TrimSuffix(unit, "B"), "I") {
	case "":
		mult = 1
	case "K":
		mult = 1 << 10
	case "M":
		mult = 1 << 20
	case "G":
		mult = 1 << 30
	case "T":
		mult = 1 << 40
	default:
		return 0, fmt.Errorf("unknown size unit in %q", s)
	}
	return uint64(v * mult), nil
}

// sortByFootprint orders largest footprint first, ties by ascending PID.
func sortByFootprint(procs []Process) {
	slices.SortStableFunc(procs, func(a, b Process) int {
		if c := cmp.Compare(b.FootprintBytes(), a.FootprintBytes()); c != 0 {
			return c
		}
		return cmp.Compare(a.PID, b.PID)
	})
}
