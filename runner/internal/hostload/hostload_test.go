package hostload

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// stubCommands installs a runCommand stub answering from the given table, keyed
// by the command name. An absent key returns an error, so a test can prove which
// source a reading came from by omitting the others.
func stubCommands(t *testing.T, out map[string]string, fail map[string]bool) *[]string {
	t.Helper()
	restore := runCommand
	t.Cleanup(func() { runCommand = restore })
	var calls []string
	runCommand = func(_ context.Context, name string, _ ...string) ([]byte, error) {
		calls = append(calls, name)
		if fail[name] {
			return nil, errors.New(name + ": stub failure")
		}
		s, ok := out[name]
		if !ok {
			return nil, errors.New(name + ": not stubbed")
		}
		return []byte(s), nil
	}
	return &calls
}

func stubProcLoadavg(t *testing.T, content string, err error) {
	t.Helper()
	restore := readProcLoadavg
	t.Cleanup(func() { readProcLoadavg = restore })
	readProcLoadavg = func() ([]byte, error) {
		if err != nil {
			return nil, err
		}
		return []byte(content), nil
	}
}

func stubCores(t *testing.T, n int) {
	t.Helper()
	restore := numCPU
	t.Cleanup(func() { numCPU = restore })
	numCPU = func() int { return n }
}

// --- the three load sources, in precedence order ------------------------

func TestRead_PrefersProcLoadavg(t *testing.T) {
	stubProcLoadavg(t, "12.34 6.83 5.76 2/1234 5678\n", nil)
	calls := stubCommands(t, map[string]string{"ps": ""}, nil)
	stubCores(t, 8)

	got, err := Read(context.Background())
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if got.Load1 != 12.34 {
		t.Errorf("Load1 = %v, want 12.34", got.Load1)
	}
	if got.Cores != 8 {
		t.Errorf("Cores = %d, want 8", got.Cores)
	}
	for _, c := range *calls {
		if c == "sysctl" || c == "uptime" {
			t.Errorf("consulted %q although /proc/loadavg was readable", c)
		}
	}
}

func TestRead_FallsBackToSysctlVMLoadavg(t *testing.T) {
	stubProcLoadavg(t, "", errors.New("no /proc on darwin"))
	stubCommands(t, map[string]string{"sysctl": "{ 7.19 6.83 5.76 }\n", "ps": ""}, nil)
	stubCores(t, 10)

	got, err := Read(context.Background())
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if got.Load1 != 7.19 {
		t.Errorf("Load1 = %v, want 7.19 (darwin brace-wrapped form)", got.Load1)
	}
}

// Both `uptime` spellings: Linux procps prints the singular "load average:",
// macOS the plural "load averages:".
func TestRead_FallsBackToUptimeBothSpellings(t *testing.T) {
	for _, tc := range []struct {
		name string
		out  string
		want float64
	}{
		{"linux_singular", " 19:13:02 up 47 days,  7:16,  5 users,  load average: 140.25, 98.10, 72.44\n", 140.25},
		{"darwin_plural", "19:13  up 47 days,  7:16, 5 users, load averages: 4.61 4.39 4.87\n", 4.61},
		{"comma_decimal", " 19:13:02 up 1 day,  1 user,  load average: 3,50, 2,10, 1,90\n", 3.50},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stubProcLoadavg(t, "", errors.New("no /proc"))
			stubCommands(t, map[string]string{"uptime": tc.out, "ps": ""}, map[string]bool{"sysctl": true})
			stubCores(t, 10)
			got, err := Read(context.Background())
			if err != nil {
				t.Fatalf("Read: %v", err)
			}
			if got.Load1 != tc.want {
				t.Errorf("Load1 = %v, want %v", got.Load1, tc.want)
			}
		})
	}
}

// Every source unreadable → Read returns an error, which is the caller's
// fail-open degrade (m8).
func TestRead_AllSourcesUnreadableReturnsError(t *testing.T) {
	stubProcLoadavg(t, "", errors.New("no /proc"))
	stubCommands(t, nil, map[string]bool{"sysctl": true, "uptime": true, "ps": true})
	stubCores(t, 10)

	if _, err := Read(context.Background()); err == nil {
		t.Fatal("Read returned nil error with every load source unreadable")
	}
}

// A source that answers with UNPARSEABLE content must fall through to the next
// one rather than yielding a bogus zero reading.
func TestRead_UnparseableSourceFallsThrough(t *testing.T) {
	stubProcLoadavg(t, "not-a-number rest\n", nil)
	stubCommands(t, map[string]string{"sysctl": "{ }\n", "uptime": "up 1 day, load averages: 9.50 1.0 1.0\n", "ps": ""}, nil)
	stubCores(t, 10)

	got, err := Read(context.Background())
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if got.Load1 != 9.50 {
		t.Errorf("Load1 = %v, want 9.50 from uptime", got.Load1)
	}
}

// --- top consumers -----------------------------------------------------

// The sort is done IN GO, never via `ps -r` / `--sort=-pcpu` (those flags
// diverge between BSD and procps), and the list is truncated to topConsumers.
func TestRead_TopConsumersSortedDescendingInGoAndTruncated(t *testing.T) {
	stubProcLoadavg(t, "1.0 1 1\n", nil)
	psOut := strings.Join([]string{
		"  100  1.5 low",
		"  101 99.1 busyloop-a",
		"  102 12.0 mid",
		"  103 98.0 busyloop-b",
		"  104  0.1 idle",
		"  105 50.0 half",
		"  106  0.0 zero",
		"  bad  x.y malformed",
		"",
	}, "\n")
	stubCommands(t, map[string]string{"ps": psOut}, nil)
	stubCores(t, 10)

	got, err := Read(context.Background())
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(got.Top) != topConsumers {
		t.Fatalf("Top has %d entries, want %d: %+v", len(got.Top), topConsumers, got.Top)
	}
	wantPIDs := []int{101, 103, 105, 102, 100}
	for i, want := range wantPIDs {
		if got.Top[i].PID != want {
			t.Fatalf("Top pids = %+v, want descending by %%CPU: %v", got.Top, wantPIDs)
		}
	}
	if got.Top[0].Command != "busyloop-a" {
		t.Errorf("Top[0].Command = %q, want busyloop-a", got.Top[0].Command)
	}
}

// A failed `ps` is NOT an error: the consumers are evidence for the failure
// message, not the decision input.
func TestRead_TopConsumerFailureIsNotAnError(t *testing.T) {
	stubProcLoadavg(t, "42.0 1 1\n", nil)
	stubCommands(t, nil, map[string]bool{"ps": true})
	stubCores(t, 10)

	got, err := Read(context.Background())
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if got.Load1 != 42.0 {
		t.Errorf("Load1 = %v, want 42.0", got.Load1)
	}
	if len(got.Top) != 0 {
		t.Errorf("Top = %+v, want empty on a ps failure", got.Top)
	}
}

// --- Overloaded --------------------------------------------------------

// The threshold straddle: below, exactly at, and above. Exactly-at is NOT
// overloaded (strict >), so the factor is a genuine ceiling.
func TestOverloaded_ThresholdStraddle(t *testing.T) {
	for _, tc := range []struct {
		name  string
		load  float64
		cores int
		want  bool
	}{
		{"idle", 0.5, 10, false},
		{"busy_build", 12.0, 10, false},
		{"just_below", 39.9, 10, false},
		{"exactly_at", 40.0, 10, false},
		{"just_above", 40.1, 10, true},
		{"incident_3663", 140.2, 10, true},
		{"single_core_above", 4.5, 1, true},
		{"zero_cores_decides_nothing", 999.0, 0, false},
		{"negative_cores_decides_nothing", 999.0, -1, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := Overloaded(Sample{Load1: tc.load, Cores: tc.cores}, DefaultFactor)
			if got != tc.want {
				t.Fatalf("Overloaded(load=%v cores=%d, factor=%v) = %v, want %v",
					tc.load, tc.cores, DefaultFactor, got, tc.want)
			}
		})
	}
}

// --- Reason ------------------------------------------------------------

func TestReason_NamesLoadCoresRatioAndTopConsumer(t *testing.T) {
	s := Sample{Load1: 140.25, Cores: 10, Top: []Consumer{
		{PID: 123, PCPU: 99.1, Command: "sh"},
		{PID: 124, PCPU: 98.0, Command: "busyloop"},
	}}
	got := Reason(s)
	for _, want := range []string{"host_overloaded:", "140.2", "10 cores", "14.0x", "pid 123 sh 99.1%", "pid 124 busyloop 98.0%"} {
		if !strings.Contains(got, want) {
			t.Errorf("Reason() = %q, missing %q", got, want)
		}
	}
	if !strings.HasPrefix(got, "host_overloaded:") {
		t.Errorf("Reason() must begin with the host_overloaded: lead, got %q", got)
	}
	if strings.Contains(got, "\n") {
		t.Errorf("Reason() must be a single line, got %q", got)
	}
}

func TestReason_TolerantOfAnEmptySample(t *testing.T) {
	got := Reason(Sample{})
	if !strings.HasPrefix(got, "host_overloaded:") {
		t.Errorf("Reason(empty) = %q", got)
	}
	if strings.Contains(got, "top CPU") {
		t.Errorf("Reason(empty) should omit the top-CPU clause, got %q", got)
	}
}
