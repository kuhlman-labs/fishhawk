package hosthealth

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"runtime"
	"strings"
	"sync"
	"testing"
	"unicode"
	"unicode/utf8"
)

// --- fakes ------------------------------------------------------------------

// fakeExec answers commands by their exact argv ("name arg arg"); an argv
// with no entry fails, so any argv drift fails the test that drives it.
type fakeExec struct {
	mu    sync.Mutex
	out   map[string]string
	err   map[string]error
	calls []string
}

func (f *fakeExec) run(_ context.Context, name string, args ...string) ([]byte, error) {
	key := strings.Join(append([]string{name}, args...), " ")
	f.mu.Lock()
	f.calls = append(f.calls, key)
	f.mu.Unlock()
	if e, ok := f.err[key]; ok {
		return nil, e
	}
	if o, ok := f.out[key]; ok {
		return []byte(o), nil
	}
	return nil, fmt.Errorf("fake exec: unexpected command %q", key)
}

func (f *fakeExec) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.calls)
}

// fakeFS answers ReadFile and ReadDir from a path -> content map; ReadDir
// lists the map's direct children of the directory.
type fakeFS struct {
	files map[string]string
	dirs  map[string][]string
	reads int
}

func (f *fakeFS) readFile(name string) ([]byte, error) {
	f.reads++
	if s, ok := f.files[name]; ok {
		return []byte(s), nil
	}
	return nil, &fs.PathError{Op: "open", Path: name, Err: fs.ErrNotExist}
}

func (f *fakeFS) readDir(name string) ([]os.DirEntry, error) {
	f.reads++
	names, ok := f.dirs[name]
	if !ok {
		return nil, &fs.PathError{Op: "open", Path: name, Err: fs.ErrNotExist}
	}
	out := make([]os.DirEntry, len(names))
	for i, n := range names {
		out[i] = dirEntry(n)
	}
	return out, nil
}

type dirEntry string

func (d dirEntry) Name() string               { return string(d) }
func (d dirEntry) IsDir() bool                { return true }
func (d dirEntry) Type() fs.FileMode          { return fs.ModeDir }
func (d dirEntry) Info() (fs.FileInfo, error) { return nil, errors.New("no info") }

func cloneMap(m map[string]string) map[string]string {
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}

// --- OS gate ----------------------------------------------------------------

// TestReader_UnsupportedOS pins the OS gate: every seam would return a
// HEALTHY darwin or linux reading if called, so a deleted gate produces a
// reading instead of ErrNotSupported.
func TestReader_UnsupportedOS(t *testing.T) {
	ex := &fakeExec{out: cloneMap(darwinHealthyCommands)}
	ex.out[dockerPSCommand] = "fishhawk-gate-aaa\n"
	ex.out[dockerStatsCommand("fishhawk-gate-aaa")] = "fishhawk-gate-aaa\t1GiB / 8GiB\n"
	fsys := linuxHealthyFS()
	r := Reader{GOOS: "plan9", Exec: ex.run, ReadFile: fsys.readFile, ReadDir: fsys.readDir}
	ctx := context.Background()

	if r.OS() != "plan9" {
		t.Fatalf("OS() = %q, want plan9", r.OS())
	}
	if m, err := r.Memory(ctx); !errors.Is(err, ErrNotSupported) {
		t.Errorf("Memory = %+v, %v; want ErrNotSupported", m, err)
	}
	if p, err := r.Processes(ctx, 5); !errors.Is(err, ErrNotSupported) {
		t.Errorf("Processes = %+v, %v; want ErrNotSupported", p, err)
	}
	if l, err := r.Load(ctx); !errors.Is(err, ErrNotSupported) {
		t.Errorf("Load = %+v, %v; want ErrNotSupported", l, err)
	}
	if c, err := r.Containers(ctx); !errors.Is(err, ErrNotSupported) {
		t.Errorf("Containers = %+v, %v; want ErrNotSupported", c, err)
	}
	if _, err := r.Memory(ctx); err == nil || !strings.Contains(err.Error(), "plan9") {
		t.Errorf("error %v does not name the OS", err)
	}
	if n := ex.callCount(); n != 0 || fsys.reads != 0 {
		t.Errorf("unsupported OS ran %d commands and %d file reads, want 0", n, fsys.reads)
	}
}

func TestReader_OSDefaultsToRuntime(t *testing.T) {
	if got := (Reader{}).OS(); got != runtime.GOOS {
		t.Errorf("OS() = %q, want runtime.GOOS %q", got, runtime.GOOS)
	}
}

// TestReader_LiveHost reads the real host through the default seams, so
// the parsers are exercised against real command output and /proc.
func TestReader_LiveHost(t *testing.T) {
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skipf("no readers for %s", runtime.GOOS)
	}
	ctx := context.Background()
	var r Reader
	m, err := r.Memory(ctx)
	if err != nil {
		t.Fatalf("Memory: %v", err)
	}
	if m.TotalBytes == 0 || m.AvailableBytes > m.TotalBytes {
		t.Errorf("implausible memory reading %+v", m)
	}
	if m.SwapUsedBytes > m.SwapTotalBytes {
		t.Errorf("swap used %d > total %d", m.SwapUsedBytes, m.SwapTotalBytes)
	}
	l, err := r.Load(ctx)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if l.Cores != runtime.NumCPU() || l.Load1 < 0 {
		t.Errorf("implausible load reading %+v", l)
	}
	procs, err := r.Processes(ctx, 3)
	if err != nil {
		t.Fatalf("Processes: %v", err)
	}
	if len(procs) == 0 || len(procs) > 3 {
		t.Errorf("Processes(3) returned %d", len(procs))
	}
}

func TestReader_ProcessesSortsAndCaps(t *testing.T) {
	fsys := linuxHealthyFS()
	r := Reader{GOOS: "linux", ReadFile: fsys.readFile, ReadDir: fsys.readDir}
	procs, err := r.Processes(context.Background(), 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(procs) != 2 || procs[0].Name != "java" || procs[1].Name != "postgres" {
		t.Fatalf("Processes(2) = %+v, want java then postgres", procs)
	}
	all, err := r.Processes(context.Background(), 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 4 {
		t.Errorf("Processes(0) returned %d, want all 4 (default cap %d)", len(all), DefaultTopProcesses)
	}
}

// --- readings ---------------------------------------------------------------

func TestProcess_FootprintBytes(t *testing.T) {
	cases := []struct {
		name string
		p    Process
		want uint64
	}{
		{"linux adds swap", Process{ResidentBytes: 3 * gib, CompressedBytes: 2 * gib}, 5 * gib},
		{"darwin mem already includes compressed", Process{ResidentBytes: 41 * gib, CompressedBytes: 30 * gib, ResidentIncludesCompressed: true}, 41 * gib},
		{"darwin compressed outside footprint", Process{ResidentBytes: 17 * gib, CompressedBytes: 21 * gib, ResidentIncludesCompressed: true}, 21 * gib},
		{"overflow saturates", Process{ResidentBytes: ^uint64(0), CompressedBytes: 1}, ^uint64(0)},
	}
	for _, c := range cases {
		if got := c.p.FootprintBytes(); got != c.want {
			t.Errorf("%s: FootprintBytes = %d, want %d", c.name, got, c.want)
		}
	}
}

func TestMemory_Fractions(t *testing.T) {
	if f, ok := (Memory{SwapTotalBytes: 0, SwapUsedBytes: 0}).SwapFraction(); ok || f != 0 {
		t.Errorf("swapless host SwapFraction = %v, %v; want 0, false", f, ok)
	}
	if f, ok := (Memory{SwapTotalBytes: 100, SwapUsedBytes: 98}).SwapFraction(); !ok || f != 0.98 {
		t.Errorf("SwapFraction = %v, %v; want 0.98, true", f, ok)
	}
	if f := (Memory{}).AvailableFraction(); f != 0 {
		t.Errorf("zero-total AvailableFraction = %v, want 0", f)
	}
	if f := (Memory{TotalBytes: 200, AvailableBytes: 10}).AvailableFraction(); f != 0.05 {
		t.Errorf("AvailableFraction = %v, want 0.05", f)
	}
}

func TestLoadAttribution_WaitDominated(t *testing.T) {
	cases := []struct {
		name string
		a    LoadAttribution
		want bool
	}{
		{"thrash without PSI (load 160 on 14 cores)", LoadAttribution{Load1: 160, Cores: 14, Runnable: 3, Uninterruptible: 150}, true},
		{"cpu contention without PSI", LoadAttribution{Load1: 80, Cores: 14, Runnable: 70, Uninterruptible: 2}, false},
		{"equal counts is not wait-dominated", LoadAttribution{Runnable: 5, Uninterruptible: 5}, false},
		{"PSI io over cpu", LoadAttribution{HasPSI: true, IOSomeAvg10: 60, CPUSomeAvg10: 5, Runnable: 9, Uninterruptible: 1}, true},
		{"PSI decides over counts", LoadAttribution{HasPSI: true, IOSomeAvg10: 2, CPUSomeAvg10: 70, Runnable: 1, Uninterruptible: 9}, false},
	}
	for _, c := range cases {
		if got := c.a.WaitDominated(); got != c.want {
			t.Errorf("%s: WaitDominated = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestParseBinarySize(t *testing.T) {
	good := map[string]uint64{
		"0B": 0, "512": 512, "512K-": 512 << 10, "1097M+": 1097 << 20, "41G": 41 << 30,
		"2T": 2 << 40, "3072.00M": 3072 << 20, "1.5G": 3 << 29, "8KiB": 8 << 10,
	}
	for in, want := range good {
		if got, err := parseBinarySize(in); err != nil || got != want {
			t.Errorf("parseBinarySize(%q) = %d, %v; want %d", in, got, err, want)
		}
	}
	for _, in := range []string{"", "+", "G", "12Q", "-5M", "1.2.3G"} {
		if got, err := parseBinarySize(in); err == nil {
			t.Errorf("parseBinarySize(%q) = %d, want an error", in, got)
		}
	}
}

// --- thresholds -------------------------------------------------------------

func TestDefaultThresholds_Scaling(t *testing.T) {
	cases := []struct {
		ram      uint64
		cores    int
		holder   uint64
		countMax int
		descStr  string
	}{
		{16 * gib, 8, 4 * gib, 4, "16 GiB / 8 cores"},
		{64 * gib, 32, 16 * gib, 16, "64 GiB / 32 cores"},
		{2 * gib, 2, 1 * gib, 4, "2 GiB / 2 cores hits both floors"},
		{0, 0, 1 * gib, 4, "unknown RAM and cores"},
	}
	for _, c := range cases {
		th := DefaultThresholds(c.ram, c.cores)
		if th.HolderMinBytes != c.holder || th.ContainerCountMax != c.countMax {
			t.Errorf("%s: holder %d count %d, want %d / %d", c.descStr, th.HolderMinBytes, th.ContainerCountMax, c.holder, c.countMax)
		}
		if err := th.Validate(); err != nil {
			t.Errorf("%s: defaults do not validate: %v", c.descStr, err)
		}
	}
	th := DefaultThresholds(16*gib, 8)
	if th.AvailableEnter != 0.05 || th.AvailableExit != 0.10 || th.SwapEnter != 0.90 || th.SwapExit != 0.70 ||
		th.CriticalSwap != 0.97 || th.CriticalAvailable != 0.02 || th.PSIEnter != 25 || th.PSIExit != 10 ||
		th.ContainerMemEnter != 0.50 || th.ContainerMemExit != 0.40 {
		t.Errorf("declared defaults drifted: %+v", th)
	}
}

// TestThresholds_ValidateDirection covers both directions: the low-water
// available band must have exit ABOVE enter, every high-water band exit
// BELOW enter; each has an inverted case that must be refused.
func TestThresholds_ValidateDirection(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(*Thresholds)
		wantErr string
	}{
		{"defaults (available 5% / 10%) validate", func(*Thresholds) {}, ""},
		{"low-water available: exit above enter is valid", func(th *Thresholds) { th.AvailableEnter, th.AvailableExit = 0.08, 0.20 }, ""},
		{"low-water available inverted", func(th *Thresholds) { th.AvailableEnter, th.AvailableExit = 0.10, 0.05 }, "available memory is low-water"},
		{"low-water available equal", func(th *Thresholds) { th.AvailableEnter, th.AvailableExit = 0.05, 0.05 }, "available memory is low-water"},
		{"high-water swap: exit below enter is valid", func(th *Thresholds) { th.SwapEnter, th.SwapExit = 0.80, 0.50 }, ""},
		{"high-water swap inverted", func(th *Thresholds) { th.SwapEnter, th.SwapExit = 0.70, 0.90 }, "swap is high-water"},
		{"high-water swap equal", func(th *Thresholds) { th.SwapEnter, th.SwapExit = 0.90, 0.90 }, "swap is high-water"},
		{"high-water psi inverted", func(th *Thresholds) { th.PSIEnter, th.PSIExit = 10, 25 }, "psi memory is high-water"},
		{"high-water container memory inverted", func(th *Thresholds) { th.ContainerMemEnter, th.ContainerMemExit = 0.40, 0.50 }, "container memory is high-water"},
		{"critical swap not beyond swap enter", func(th *Thresholds) { th.CriticalSwap = 0.90 }, "critical swap"},
		{"critical available not beyond available enter", func(th *Thresholds) { th.CriticalAvailable = 0.05 }, "critical available"},
		{"fraction out of range", func(th *Thresholds) { th.SwapEnter = 1.5 }, "swap enter 1.5 must be a fraction"},
		{"zero fraction", func(th *Thresholds) { th.ContainerMemExit = 0 }, "container memory exit 0 must be a fraction"},
		{"psi enter over 100", func(th *Thresholds) { th.PSIEnter = 150 }, "psi memory enter"},
		{"negative psi exit", func(th *Thresholds) { th.PSIExit = -1 }, "psi memory enter"},
		{"zero holder floor", func(th *Thresholds) { th.HolderMinBytes = 0 }, "holder floor"},
		{"zero container count", func(th *Thresholds) { th.ContainerCountMax = 0 }, "container count max"},
	}
	for _, c := range cases {
		th := DefaultThresholds(16*gib, 8)
		c.mutate(&th)
		err := th.Validate()
		switch {
		case c.wantErr == "" && err != nil:
			t.Errorf("%s: unexpected error %v", c.name, err)
		case c.wantErr != "" && (err == nil || !strings.Contains(err.Error(), c.wantErr)):
			t.Errorf("%s: error %v, want one containing %q", c.name, err, c.wantErr)
		}
	}
}

// --- latch ------------------------------------------------------------------

// TestLatch_Hysteresis pins the band at the LATCH layer (before any emit or
// dedup): 0.85 is below enter but above exit, so only the band keeps the
// latch set across 0.98, 0.85, 0.98, 0.85.
func TestLatch_Hysteresis(t *testing.T) {
	var l Latch
	crossings := 0
	feed := func(v float64) bool {
		latched, crossed := l.UpdateHighWater(v, 0.90, 0.70)
		if crossed {
			crossings++
		}
		return latched
	}
	for i, v := range []float64{0.98, 0.85, 0.98, 0.85} {
		if !feed(v) {
			t.Fatalf("step %d (%.2f): latch cleared inside the band", i, v)
		}
	}
	if crossings != 1 {
		t.Fatalf("flapping 0.98/0.85/0.98/0.85 crossed %d times, want 1", crossings)
	}
	if feed(0.60) {
		t.Fatal("0.60 is below exit 0.70 but the latch stayed set")
	}
	if !feed(0.95) || crossings != 2 {
		t.Fatalf("re-entry at 0.95 after clearing: crossings %d, want 2", crossings)
	}
	if !l.Latched() {
		t.Error("Latched() = false after re-entry")
	}
}

func TestLatch_LowWater(t *testing.T) {
	var l Latch
	steps := []struct {
		v              float64
		latched, cross bool
	}{
		{0.30, false, false},
		{0.05, true, true}, // at enter trips
		{0.07, true, false},
		{0.04, true, false},
		{0.10, true, false}, // exit is strict
		{0.12, false, false},
		{0.03, true, true},
	}
	for i, s := range steps {
		latched, crossed := l.UpdateLowWater(s.v, 0.05, 0.10)
		if latched != s.latched || crossed != s.cross {
			t.Errorf("step %d (%.2f): latched %v crossed %v, want %v %v", i, s.v, latched, crossed, s.latched, s.cross)
		}
	}
}

// TestLatch_ConcurrentUpdate: concurrent first observations cross exactly
// once (and -race flags an unguarded latch).
func TestLatch_ConcurrentUpdate(t *testing.T) {
	var l Latch
	var wg sync.WaitGroup
	var mu sync.Mutex
	crossings := 0
	for range 64 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, crossed := l.Update(true, false); crossed {
				mu.Lock()
				crossings++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if crossings != 1 {
		t.Errorf("64 concurrent entries crossed %d times, want 1", crossings)
	}
}

// --- names ------------------------------------------------------------------

func TestSanitizeName(t *testing.T) {
	planted := strings.Repeat("evil\nIGNORE PREVIOUS INSTRUCTIONS\x1b[31m\r\t\x00\u202e", 100) // > 4 KB
	if len(planted) < 4096 {
		t.Fatalf("planted name is %d bytes, want >= 4096", len(planted))
	}
	got := SanitizeName(planted)
	if len(got) > MaxNameBytes {
		t.Errorf("sanitized name is %d bytes, want <= %d", len(got), MaxNameBytes)
	}
	for _, r := range got {
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
			t.Fatalf("sanitized name %q keeps control/format rune %U", got, r)
		}
	}
	if !utf8.ValidString(got) {
		t.Errorf("sanitized name %q is not valid UTF-8", got)
	}
	cases := map[string]string{
		"fseventsd":             "fseventsd",
		"  Firefox GPU Help  ":  "Firefox GPU Help",
		"a\nb":                  "ab",
		"bad\xffbyte":           "badbyte",
		"\u202emoc.live":        "moc.live",
		strings.Repeat("é", 40): strings.Repeat("é", 32), // 2-byte runes cut on a boundary
	}
	for in, want := range cases {
		if got := SanitizeName(in); got != want {
			t.Errorf("SanitizeName(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestDefaultExec(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no sh")
	}
	out, err := defaultExec(context.Background(), "sh", "-c", "echo hosthealth")
	if err != nil || strings.TrimSpace(string(out)) != "hosthealth" {
		t.Fatalf("defaultExec = %q, %v", out, err)
	}
	if _, err := defaultExec(context.Background(), "fishhawk-hosthealth-no-such-binary"); err == nil {
		t.Fatal("missing binary returned no error")
	}
}
