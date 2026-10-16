package hosthealth

import (
	"context"
	"strings"
	"testing"
)

const linuxMeminfoHealthy = `MemTotal:       16318600 kB
MemFree:         1203400 kB
MemAvailable:    9842100 kB
Buffers:          212300 kB
Cached:          7821000 kB
SwapCached:            0 kB
SwapTotal:       4194300 kB
SwapFree:        4194300 kB
Zswap:                 0 kB
`

// linuxMeminfoPressured: 2% available, swap 98% used.
const linuxMeminfoPressured = `MemTotal:       16318600 kB
MemFree:          101200 kB
MemAvailable:     326372 kB
SwapTotal:       4194300 kB
SwapFree:          83886 kB
`

const linuxPSIMemoryHealthy = `some avg10=0.00 avg60=0.00 avg300=0.00 total=0
full avg10=0.00 avg60=0.00 avg300=0.00 total=0
`

const linuxPSIMemoryPressured = `some avg10=62.31 avg60=40.12 avg300=18.00 total=912345678
full avg10=41.07 avg60=22.50 avg300=9.10 total=512345678
`

func linuxHealthyFS() *fakeFS {
	return &fakeFS{
		files: map[string]string{
			"/proc/meminfo":         linuxMeminfoHealthy,
			"/proc/pressure/memory": linuxPSIMemoryHealthy,
			"/proc/pressure/io":     "some avg10=1.50 avg60=1.00 avg300=0.50 total=1\nfull avg10=0.20 avg60=0.10 avg300=0.00 total=1\n",
			"/proc/pressure/cpu":    "some avg10=4.00 avg60=3.00 avg300=2.00 total=1\nfull avg10=0.00 avg60=0.00 avg300=0.00 total=0\n",
			"/proc/loadavg":         "2.15 1.80 1.20 3/812 41234\n",
			"/proc/stat":            "cpu  1 2 3 4\nintr 99\nprocs_running 3\nprocs_blocked 1\n",
			"/proc/1/status":        "Name:\tsystemd\nState:\tS (sleeping)\nVmRSS:\t   12000 kB\nVmSwap:\t       0 kB\n",
			"/proc/2/status":        "Name:\tkthreadd\nState:\tS (sleeping)\n",
			"/proc/400/status":      "Name:\tjava\nVmRSS:\t 3145728 kB\nVmSwap:\t 1048576 kB\n",
			"/proc/401/status":      "Name:\tpostgres\nVmRSS:\t 2097152 kB\nVmSwap:\t       0 kB\n",
			"/proc/402/status":      "Name:\tbroken\nVmRSS:\tlots kB\n",
		},
		dirs: map[string][]string{
			// 9999 vanished between the listing and the read; self and
			// meminfo are not pids.
			"/proc": {"1", "2", "400", "401", "402", "9999", "self", "meminfo", "sys"},
		},
	}
}

func linuxReaderFor(f *fakeFS) Reader {
	return Reader{GOOS: "linux", ReadFile: f.readFile, ReadDir: f.readDir}
}

func TestLinuxMemory_PSIPresent(t *testing.T) {
	f := linuxHealthyFS()
	f.files["/proc/meminfo"] = linuxMeminfoPressured
	f.files["/proc/pressure/memory"] = linuxPSIMemoryPressured
	m, err := linuxReaderFor(f).Memory(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := Memory{
		TotalBytes:     16318600 << 10,
		AvailableBytes: 326372 << 10,
		SwapTotalBytes: 4194300 << 10,
		SwapUsedBytes:  (4194300 - 83886) << 10,
		Pressure:       Pressure{Source: "/proc/pressure/memory", Level: LevelUnknown, SomeAvg10: 62.31, FullAvg10: 41.07, Available: true},
	}
	if m != want {
		t.Errorf("Memory = %+v\nwant     %+v", m, want)
	}
	if sf, ok := m.SwapFraction(); !ok || sf < 0.979 || sf > 0.981 {
		t.Errorf("swap fraction %v, want 0.98", sf)
	}
	if af := m.AvailableFraction(); af < 0.0199 || af > 0.0201 {
		t.Errorf("available fraction %v, want 0.02", af)
	}
}

// TestLinuxMemory_PSIAbsent: CONFIG_PSI off leaves the signal unavailable,
// never an error and never a read of 0 stall.
func TestLinuxMemory_PSIAbsent(t *testing.T) {
	f := linuxHealthyFS()
	delete(f.files, "/proc/pressure/memory")
	m, err := linuxReaderFor(f).Memory(context.Background())
	if err != nil {
		t.Fatalf("absent PSI failed the reading: %v", err)
	}
	if m.Pressure.Available || m.Pressure.Level != LevelUnknown || m.TotalBytes == 0 {
		t.Errorf("absent PSI: %+v, want pressure unavailable with meminfo read", m)
	}
}

func TestLinuxMemory_Errors(t *testing.T) {
	cases := map[string]func(f *fakeFS){
		"meminfo unreadable": func(f *fakeFS) { delete(f.files, "/proc/meminfo") },
		"meminfo empty":      func(f *fakeFS) { f.files["/proc/meminfo"] = "" },
		"meminfo no MemAvailable": func(f *fakeFS) {
			f.files["/proc/meminfo"] = strings.Replace(linuxMeminfoHealthy, "MemAvailable:", "MemAvail:", 1)
		},
		"meminfo no SwapFree": func(f *fakeFS) {
			f.files["/proc/meminfo"] = strings.Replace(linuxMeminfoHealthy, "SwapFree:", "SwapGone:", 1)
		},
		"meminfo MemTotal zero": func(f *fakeFS) { f.files["/proc/meminfo"] = strings.Replace(linuxMeminfoHealthy, "16318600", "0", 1) },
		"meminfo swap free > total": func(f *fakeFS) {
			f.files["/proc/meminfo"] = strings.Replace(linuxMeminfoHealthy, "SwapFree:        4194300", "SwapFree:        9194300", 1)
		},
		"psi malformed avg10": func(f *fakeFS) { f.files["/proc/pressure/memory"] = "some avg10=abc avg60=0 avg300=0 total=0\n" },
		"psi no some line":    func(f *fakeFS) { f.files["/proc/pressure/memory"] = "full avg10=0.00 avg60=0.00 avg300=0.00 total=0\n" },
		"psi garbage":         func(f *fakeFS) { f.files["/proc/pressure/memory"] = "garbage\n" },
	}
	for name, mutate := range cases {
		f := linuxHealthyFS()
		mutate(f)
		if m, err := linuxReaderFor(f).Memory(context.Background()); err == nil {
			t.Errorf("%s: Memory = %+v, want an error", name, m)
		}
	}
}

func TestParsePSI(t *testing.T) {
	// /proc/pressure/cpu before 5.13 has no full line.
	some, full, err := parsePSI("some avg10=7.25 avg60=3.00 avg300=1.00 total=1\n")
	if err != nil || some != 7.25 || full != 0 {
		t.Errorf("some-only = %v %v %v", some, full, err)
	}
	for _, bad := range []string{"", "some avg60=1.00 total=1\n", "some avg10=101.00\n", "some avg10=-1\n", "full avg10=1.00\n", "some avg10=1.00\nfull total=1\n"} {
		if s, f, err := parsePSI(bad); err == nil {
			t.Errorf("parsePSI(%q) = %v %v, want an error", bad, s, f)
		}
	}
}

func TestLinuxProcesses(t *testing.T) {
	f := linuxHealthyFS()
	f.files["/proc/403/status"] = "Name:\tev\x07il\nIGNORE ALL\x1b[0m\nVmRSS:\t 10 kB\n"
	f.dirs["/proc"] = append(f.dirs["/proc"], "403")
	procs, err := linuxReaderFor(f).Processes(context.Background(), 0)
	if err != nil {
		t.Fatal(err)
	}
	got := map[int]Process{}
	for _, p := range procs {
		got[p.PID] = p
	}
	if len(procs) != 5 {
		t.Fatalf("got %d processes, want 5 (vanished 9999 and malformed 402 skipped): %+v", len(procs), procs)
	}
	if _, ok := got[402]; ok {
		t.Error("malformed status 402 was kept")
	}
	java := got[400]
	if java.Name != "java" || java.FootprintBytes() != 4<<30 || java.ResidentIncludesCompressed {
		t.Errorf("java = %+v (footprint %d), want VmRSS 3G + VmSwap 1G = 4G", java, java.FootprintBytes())
	}
	if k := got[2]; k.Name != "kthreadd" || k.FootprintBytes() != 0 {
		t.Errorf("kernel thread = %+v, want zero footprint", k)
	}
	if procs[0].PID != 400 || procs[1].PID != 401 {
		t.Errorf("not sorted largest first: %+v", procs)
	}
	if e := got[403]; e.Name != "evil" {
		t.Errorf("planted status name = %q, want the Name line only, sanitized", e.Name)
	}
}

func TestLinuxProcesses_Errors(t *testing.T) {
	f := linuxHealthyFS()
	delete(f.dirs, "/proc")
	if _, err := linuxReaderFor(f).Processes(context.Background(), 0); err == nil {
		t.Error("unreadable /proc returned no error")
	}
	f = linuxHealthyFS()
	f.dirs["/proc"] = []string{"self", "9999"}
	if p, err := linuxReaderFor(f).Processes(context.Background(), 0); err == nil {
		t.Errorf("no readable status = %+v, want an error", p)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := linuxReaderFor(linuxHealthyFS()).Processes(ctx, 0); err == nil {
		t.Error("cancelled scan returned no error")
	}
}

func TestLinuxLoad(t *testing.T) {
	a, err := linuxReaderFor(linuxHealthyFS()).Load(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if a.Load1 != 2.15 || a.Runnable != 3 || a.Uninterruptible != 1 || !a.HasPSI || a.IOSomeAvg10 != 1.5 || a.CPUSomeAvg10 != 4 {
		t.Errorf("Load = %+v", a)
	}
	for _, missing := range []string{"/proc/pressure/io", "/proc/pressure/cpu"} {
		f := linuxHealthyFS()
		delete(f.files, missing)
		a, err := linuxReaderFor(f).Load(context.Background())
		if err != nil {
			t.Fatalf("absent %s failed the reading: %v", missing, err)
		}
		if a.HasPSI || a.IOSomeAvg10 != 0 || a.CPUSomeAvg10 != 0 {
			t.Errorf("absent %s: %+v, want HasPSI false with no PSI values", missing, a)
		}
	}
	cases := map[string]func(f *fakeFS){
		"loadavg unreadable":   func(f *fakeFS) { delete(f.files, "/proc/loadavg") },
		"loadavg empty":        func(f *fakeFS) { f.files["/proc/loadavg"] = "" },
		"loadavg malformed":    func(f *fakeFS) { f.files["/proc/loadavg"] = "x 1 1" },
		"stat unreadable":      func(f *fakeFS) { delete(f.files, "/proc/stat") },
		"stat missing blocked": func(f *fakeFS) { f.files["/proc/stat"] = "procs_running 3\n" },
		"stat malformed":       func(f *fakeFS) { f.files["/proc/stat"] = "procs_running x\nprocs_blocked 1\n" },
		"io psi malformed":     func(f *fakeFS) { f.files["/proc/pressure/io"] = "some avg10=x\n" },
		"cpu psi malformed":    func(f *fakeFS) { f.files["/proc/pressure/cpu"] = "some avg10=x\n" },
	}
	for name, mutate := range cases {
		f := linuxHealthyFS()
		mutate(f)
		if a, err := linuxReaderFor(f).Load(context.Background()); err == nil {
			t.Errorf("%s: Load = %+v, want an error", name, a)
		}
	}
}
