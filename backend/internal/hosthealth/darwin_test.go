package hosthealth

import (
	"context"
	"errors"
	"strings"
	"testing"
)

const darwinTopCommand = "top -l 1 -o mem -n 20 -stats pid,command,mem,cmprs"

// Captured macOS output shapes (macOS 15/26, Apple silicon, 16 KiB pages).
const darwinVMStatHealthy = `Mach Virtual Memory Statistics: (page size of 16384 bytes)
Pages free:                                    13437.
Pages active:                                 718995.
Pages inactive:                               717458.
Pages speculative:                              2054.
Pages throttled:                                   0.
Pages wired down:                             178726.
Pages purgeable:                               10373.
"Translation faults":                     5638856116.
Pages copy-on-write:                       635012759.
File-backed pages:                            380017.
Anonymous pages:                             1058490.
Pages stored in compressor:                  1248039.
Pages occupied by compressor:                 672062.
Decompressions:                            641878781.
`

const darwinTopHealthy = `Processes: 812 total, 6 running, 806 sleeping, 4980 threads
2026/10/16 09:12:44
Load Avg: 3.86, 5.59, 6.40
CPU usage: 9.52% user, 12.50% sys, 77.97% idle
PhysMem: 35G used (2793M wired, 10G compressor), 238M unused.
VM: 385T vsize, 57G framework vsize, 64821(0) swapins, 176468(0) swapouts.

PID    COMMAND          MEM   CMPRS
9817   com.apple.Virtua 17G   21G
20863  plugin-container 1097M+ 826M-
613    WindowServer     859M  441M
16349  Firefox GPU Help 685M  319M
`

// darwinDoneMeansTop is the #4330 done-means host: one process at 41G.
const darwinDoneMeansTop = `Processes: 640 total, 3 running, 637 sleeping, 3810 threads
Load Avg: 41.20, 38.02, 30.11
PhysMem: 47G used (3100M wired, 22G compressor), 120M unused.

PID    COMMAND          MEM   CMPRS
377    fseventsd        41G   30G
613    WindowServer     812M  402M
16345  firefox          715M  434M
`

var darwinHealthyCommands = map[string]string{
	"sysctl -n hw.memsize":   "38654705664\n",
	"vm_stat":                darwinVMStatHealthy,
	"sysctl -n vm.swapusage": "total = 3072.00M  used = 1482.00M  free = 1590.00M  (encrypted)\n",
	"sysctl -n kern.memorystatus_vm_pressure_level": "1\n",
	darwinTopCommand:       darwinTopHealthy,
	"sysctl -n vm.loadavg": "{ 3.86 5.59 6.40 }\n",
	"ps -axo stat=":        "Ss\nS\nR+\nS\nUs\nS\n",
}

// darwinDoneMeansCommands: 48 GiB host, swap 98%, critical pressure.
func darwinDoneMeansCommands() map[string]string {
	m := cloneMap(darwinHealthyCommands)
	m["sysctl -n hw.memsize"] = "51539607552\n"
	m["sysctl -n vm.swapusage"] = "total = 10240.00M  used = 10035.20M  free = 204.80M  (encrypted)\n"
	m["sysctl -n kern.memorystatus_vm_pressure_level"] = "4\n"
	m[darwinTopCommand] = darwinDoneMeansTop
	return m
}

func darwinReaderFor(cmds map[string]string) (Reader, *fakeExec) {
	ex := &fakeExec{out: cmds}
	return Reader{GOOS: "darwin", Exec: ex.run}, ex
}

func TestDarwinMemory_Healthy(t *testing.T) {
	r, _ := darwinReaderFor(cloneMap(darwinHealthyCommands))
	m, err := r.Memory(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	const page = 16384
	want := Memory{
		TotalBytes:      38654705664,
		AvailableBytes:  (13437 + 717458 + 2054) * page,
		CompressedBytes: 672062 * page,
		SwapTotalBytes:  3072 << 20,
		SwapUsedBytes:   1482 << 20,
		Pressure:        Pressure{Source: "kern.memorystatus_vm_pressure_level", Level: LevelNormal, Available: true},
	}
	if m != want {
		t.Errorf("Memory = %+v\nwant     %+v", m, want)
	}
}

func TestDarwinMemory_DoneMeansSwap98(t *testing.T) {
	r, _ := darwinReaderFor(darwinDoneMeansCommands())
	m, err := r.Memory(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	f, ok := m.SwapFraction()
	if !ok || f < 0.979 || f > 0.981 {
		t.Errorf("swap fraction %v, %v; want 0.98", f, ok)
	}
	if m.Pressure.Level != LevelCritical {
		t.Errorf("pressure level %q, want critical", m.Pressure.Level)
	}
}

func TestDarwinMemory_PressureLevels(t *testing.T) {
	for out, want := range map[string]Level{"1\n": LevelNormal, "2": LevelWarn, "4\n": LevelCritical, "0": LevelUnknown, "3": LevelUnknown, "garbage": LevelUnknown, "": LevelUnknown} {
		cmds := cloneMap(darwinHealthyCommands)
		cmds["sysctl -n kern.memorystatus_vm_pressure_level"] = out
		r, _ := darwinReaderFor(cmds)
		m, err := r.Memory(context.Background())
		if err != nil {
			t.Fatalf("level %q: %v", out, err)
		}
		if m.Pressure.Level != want || !m.Pressure.Available {
			t.Errorf("level %q: got %q available %v, want %q available", out, m.Pressure.Level, m.Pressure.Available, want)
		}
	}
	// An unreadable pressure sysctl leaves the signal unavailable and the
	// rest of the reading intact.
	ex := &fakeExec{out: cloneMap(darwinHealthyCommands), err: map[string]error{"sysctl -n kern.memorystatus_vm_pressure_level": errors.New("unknown oid")}}
	m, err := Reader{GOOS: "darwin", Exec: ex.run}.Memory(context.Background())
	if err != nil {
		t.Fatalf("pressure sysctl failure failed the reading: %v", err)
	}
	if m.Pressure.Available || m.Pressure.Level != LevelUnknown || m.SwapTotalBytes == 0 {
		t.Errorf("pressure sysctl failure: %+v, want unavailable/unknown with swap still read", m)
	}
}

// TestDarwinMemory_Errors: every required source failing, empty or
// malformed is an error, never a zero reading that would read healthy.
func TestDarwinMemory_Errors(t *testing.T) {
	cases := map[string]func(m map[string]string, errs map[string]error){
		"hw.memsize fails":     func(_ map[string]string, e map[string]error) { e["sysctl -n hw.memsize"] = errors.New("boom") },
		"hw.memsize malformed": func(m map[string]string, _ map[string]error) { m["sysctl -n hw.memsize"] = "lots" },
		"hw.memsize zero":      func(m map[string]string, _ map[string]error) { m["sysctl -n hw.memsize"] = "0" },
		"vm_stat fails":        func(_ map[string]string, e map[string]error) { e["vm_stat"] = errors.New("boom") },
		"vm_stat empty":        func(m map[string]string, _ map[string]error) { m["vm_stat"] = "" },
		"vm_stat no page size": func(m map[string]string, _ map[string]error) {
			m["vm_stat"] = strings.SplitN(darwinVMStatHealthy, "\n", 2)[1]
		},
		"vm_stat missing compressor": func(m map[string]string, _ map[string]error) {
			m["vm_stat"] = strings.Replace(darwinVMStatHealthy, "Pages occupied by compressor:", "Pages gone:", 1)
		},
		"vm_stat missing free": func(m map[string]string, _ map[string]error) {
			m["vm_stat"] = strings.Replace(darwinVMStatHealthy, "Pages free:", "Pages lost:", 1)
		},
		"vm_stat zero page size": func(m map[string]string, _ map[string]error) {
			m["vm_stat"] = strings.Replace(darwinVMStatHealthy, "16384", "0", 1)
		},
		"vm_stat counter overflows": func(m map[string]string, _ map[string]error) {
			m["vm_stat"] = strings.Replace(darwinVMStatHealthy, "13437.", "99999999999999999999999.", 1)
		},
		"swapusage fails":     func(_ map[string]string, e map[string]error) { e["sysctl -n vm.swapusage"] = errors.New("boom") },
		"swapusage malformed": func(m map[string]string, _ map[string]error) { m["sysctl -n vm.swapusage"] = "no swap here" },
		"swapusage bad total unit": func(m map[string]string, _ map[string]error) {
			m["sysctl -n vm.swapusage"] = "total = 1.2.3M  used = 1.00M"
		},
		"swapusage bad used": func(m map[string]string, _ map[string]error) {
			m["sysctl -n vm.swapusage"] = "total = 10.00M  used = 1.2.3M"
		},
		"swapusage used over total": func(m map[string]string, _ map[string]error) {
			m["sysctl -n vm.swapusage"] = "total = 10.00M  used = 20.00M"
		},
	}
	for name, mutate := range cases {
		cmds, errs := cloneMap(darwinHealthyCommands), map[string]error{}
		mutate(cmds, errs)
		ex := &fakeExec{out: cmds, err: errs}
		if m, err := (Reader{GOOS: "darwin", Exec: ex.run}).Memory(context.Background()); err == nil {
			t.Errorf("%s: Memory = %+v, want an error", name, m)
		}
	}
}

func TestDarwinProcesses_ParsesTop(t *testing.T) {
	r, _ := darwinReaderFor(cloneMap(darwinHealthyCommands))
	procs, err := r.Processes(context.Background(), 0)
	if err != nil {
		t.Fatal(err)
	}
	want := []Process{
		{PID: 9817, Name: "com.apple.Virtua", ResidentBytes: 17 << 30, CompressedBytes: 21 << 30, ResidentIncludesCompressed: true},
		{PID: 20863, Name: "plugin-container", ResidentBytes: 1097 << 20, CompressedBytes: 826 << 20, ResidentIncludesCompressed: true},
		{PID: 613, Name: "WindowServer", ResidentBytes: 859 << 20, CompressedBytes: 441 << 20, ResidentIncludesCompressed: true},
		{PID: 16349, Name: "Firefox GPU Help", ResidentBytes: 685 << 20, CompressedBytes: 319 << 20, ResidentIncludesCompressed: true},
	}
	if len(procs) != len(want) {
		t.Fatalf("got %d processes, want %d: %+v", len(procs), len(want), procs)
	}
	for i := range want {
		if procs[i] != want[i] {
			t.Errorf("row %d = %+v, want %+v", i, procs[i], want[i])
		}
	}
	// The VM row's compressed pages exceed its MEM: footprint is CMPRS.
	if got := procs[0].FootprintBytes(); got != 21<<30 {
		t.Errorf("VM footprint %d, want 21G", got)
	}
}

func TestDarwinProcesses_DoneMeans41G(t *testing.T) {
	r, _ := darwinReaderFor(darwinDoneMeansCommands())
	procs, err := r.Processes(context.Background(), 0)
	if err != nil {
		t.Fatal(err)
	}
	top := procs[0]
	// mem=41G cmprs=30G -> footprint 41G (MEM already includes compressed).
	if top.Name != "fseventsd" || top.PID != 377 || top.FootprintBytes() != 41<<30 {
		t.Errorf("top holder %+v (footprint %d), want fseventsd pid 377 at 41G", top, top.FootprintBytes())
	}
}

func TestDarwinProcesses_Errors(t *testing.T) {
	header := "PID    COMMAND          MEM   CMPRS\n"
	cases := map[string]string{
		"empty":               "",
		"no header":           "613 WindowServer 859M 441M\n",
		"wrong header":        "PID COMMAND MEM\n613 WindowServer 859M\n",
		"header only":         header,
		"non-numeric pid":     header + "abc   WindowServer     859M  441M\n",
		"negative pid":        header + "-3    kernel_task      859M  441M\n",
		"missing command":     header + "613   859M  441M\n",
		"bad mem":             header + "613   WindowServer     lots  441M\n",
		"bad cmprs":           header + "613   WindowServer     859M  12Q\n",
		"too few fields":      header + "613 441M\n",
		"malformed later row": header + "613   WindowServer     859M  441M\n614   x\n",
	}
	for name, out := range cases {
		cmds := cloneMap(darwinHealthyCommands)
		cmds[darwinTopCommand] = out
		r, _ := darwinReaderFor(cmds)
		if p, err := r.Processes(context.Background(), 0); err == nil {
			t.Errorf("%s: Processes = %+v, want an error", name, p)
		}
	}
	ex := &fakeExec{out: cloneMap(darwinHealthyCommands), err: map[string]error{darwinTopCommand: errors.New("boom")}}
	if _, err := (Reader{GOOS: "darwin", Exec: ex.run}).Processes(context.Background(), 0); err == nil {
		t.Error("top failure returned no error")
	}
}

func TestDarwinLoad(t *testing.T) {
	r, _ := darwinReaderFor(cloneMap(darwinHealthyCommands))
	a, err := r.Load(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if a.Load1 != 3.86 || a.Runnable != 1 || a.Uninterruptible != 1 || a.HasPSI || a.Cores <= 0 {
		t.Errorf("Load = %+v, want load1 3.86, 1 runnable, 1 uninterruptible, no PSI", a)
	}
	thrash := cloneMap(darwinHealthyCommands)
	thrash["sysctl -n vm.loadavg"] = "{ 160.02 120.00 90.00 }"
	thrash["ps -axo stat="] = strings.Repeat("U\n", 150) + strings.Repeat("R\n", 3) + "D+\n"
	r, _ = darwinReaderFor(thrash)
	a, err = r.Load(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if a.Uninterruptible != 151 || a.Runnable != 3 || !a.WaitDominated() {
		t.Errorf("thrash Load = %+v, want 151 uninterruptible / 3 runnable, wait-dominated", a)
	}

	errCases := map[string]func(m map[string]string, e map[string]error){
		"loadavg fails":     func(_ map[string]string, e map[string]error) { e["sysctl -n vm.loadavg"] = errors.New("boom") },
		"loadavg empty":     func(m map[string]string, _ map[string]error) { m["sysctl -n vm.loadavg"] = "{ }" },
		"loadavg malformed": func(m map[string]string, _ map[string]error) { m["sysctl -n vm.loadavg"] = "{ x y z }" },
		"ps fails":          func(_ map[string]string, e map[string]error) { e["ps -axo stat="] = errors.New("boom") },
		"ps empty":          func(m map[string]string, _ map[string]error) { m["ps -axo stat="] = "\n \n" },
	}
	for name, mutate := range errCases {
		cmds, errs := cloneMap(darwinHealthyCommands), map[string]error{}
		mutate(cmds, errs)
		ex := &fakeExec{out: cmds, err: errs}
		if a, err := (Reader{GOOS: "darwin", Exec: ex.run}).Load(context.Background()); err == nil {
			t.Errorf("%s: Load = %+v, want an error", name, a)
		}
	}
}

// TestDarwinReader_Argv pins the exact command set: the fake fails any argv
// it has no entry for, and the calls are compared in order.
func TestDarwinReader_Argv(t *testing.T) {
	r, ex := darwinReaderFor(cloneMap(darwinHealthyCommands))
	ctx := context.Background()
	if _, err := r.Memory(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Processes(ctx, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Load(ctx); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"sysctl -n hw.memsize", "vm_stat", "sysctl -n vm.swapusage", "sysctl -n kern.memorystatus_vm_pressure_level",
		darwinTopCommand,
		"sysctl -n vm.loadavg", "ps -axo stat=",
	}
	if strings.Join(ex.calls, "|") != strings.Join(want, "|") {
		t.Errorf("argv sequence\n got %q\nwant %q", ex.calls, want)
	}
}
