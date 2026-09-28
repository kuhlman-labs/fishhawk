// Package hostload samples the host's 1-minute load average and top CPU
// consumers so the runner can tell a genuinely starved host from a red tree
// (#3663).
//
// The problem it solves: when the host is CPU-starved — in the #3663 incident by
// orphaned busy-loops a previous stage leaked — a committed-tree verify fails on
// timeouts and container-start deadlines in packages the diff never touched, and
// the runner classifies that as category A (an artifact defect the agent must
// fix). It is not; it is infrastructure. Sampling immediately before each verify
// lets the runner attach a `host_overloaded:` lead to the failure and classify
// it category C instead, so the stage is retryable in place.
//
// The 1-minute average is deliberate: it is the most responsive of the three,
// and the sample is taken immediately before the verify, where a 15-minute
// average would misclassify a host that has just recovered.
package hostload

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"
)

// DefaultFactor is the load-average-to-core-count ratio above which the host is
// treated as starved. A 10-core host must exceed 40 — far above the busy-build
// band, so an ordinary `go test -race ./...` loop never trips it, while the
// #3663 incident (~140 on a 10-core host, 14x) is unambiguous.
const DefaultFactor = 4.0

// topConsumers bounds how many CPU consumers a Sample carries.
const topConsumers = 5

// Consumer is one process's CPU share, as ps reports it.
type Consumer struct {
	PID     int
	PCPU    float64
	Command string
}

// Sample is one reading: the 1-minute load average, the core count it must be
// judged against, and the top CPU consumers (best-effort evidence — a Sample
// with an empty Top is still a valid reading).
type Sample struct {
	Load1 float64
	Cores int
	Top   []Consumer
}

// Injectable seams. The load sources are tried in order: /proc/loadavg (Linux),
// `sysctl -n vm.loadavg` (Darwin), then `uptime` (portable last resort).
var (
	readProcLoadavg = func() ([]byte, error) { return os.ReadFile("/proc/loadavg") }
	runCommand      = defaultRunCommand
	numCPU          = runtime.NumCPU
)

// readTimeout bounds each helper exec so a wedged `ps` or `sysctl` cannot
// stall the verify preflight.
const readTimeout = 5 * time.Second

func defaultRunCommand(ctx context.Context, name string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, readTimeout)
	defer cancel()
	return exec.CommandContext(ctx, name, args...).Output()
}

// uptimeLoadRe tolerates BOTH spellings: Linux procps prints the singular
// "load average:", macOS the plural "load averages:".
var uptimeLoadRe = regexp.MustCompile(`load averages?:\s*([0-9]+(?:[.,][0-9]+)?)`)

// Read takes one reading. It returns an error only when NO load source could be
// read — the caller treats that as a fail-open degrade (one printed reason, no
// classification change). A failure to read the top consumers is NOT an error:
// they are evidence for the failure message, not the decision input.
func Read(ctx context.Context) (Sample, error) {
	load, err := readLoad1(ctx)
	if err != nil {
		return Sample{}, err
	}
	return Sample{Load1: load, Cores: numCPU(), Top: readTop(ctx)}, nil
}

// readLoad1 walks the three load sources in order, returning the first that
// yields a parseable 1-minute average.
func readLoad1(ctx context.Context) (float64, error) {
	if b, err := readProcLoadavg(); err == nil {
		if fields := strings.Fields(string(b)); len(fields) > 0 {
			if v, perr := strconv.ParseFloat(fields[0], 64); perr == nil {
				return v, nil
			}
		}
	}
	if out, err := runCommand(ctx, "sysctl", "-n", "vm.loadavg"); err == nil {
		// Darwin prints `{ 7.19 6.83 5.76 }`.
		trimmed := strings.Trim(strings.TrimSpace(string(out)), "{}")
		if fields := strings.Fields(trimmed); len(fields) > 0 {
			if v, perr := strconv.ParseFloat(fields[0], 64); perr == nil {
				return v, nil
			}
		}
	}
	if out, err := runCommand(ctx, "uptime"); err == nil {
		if m := uptimeLoadRe.FindStringSubmatch(string(out)); len(m) == 2 {
			if v, perr := strconv.ParseFloat(strings.Replace(m[1], ",", ".", 1), 64); perr == nil {
				return v, nil
			}
		}
	}
	return 0, fmt.Errorf("hostload: no readable 1-minute load average (tried /proc/loadavg, sysctl vm.loadavg, uptime)")
}

// readTop returns the top CPU consumers, sorted DESCENDING by %CPU IN GO.
// Never `ps -r` or `--sort=-pcpu`: those flags diverge between BSD and procps,
// so the ordering is done here where it is the same on every host.
func readTop(ctx context.Context) []Consumer {
	out, err := runCommand(ctx, "ps", "-axo", "pid=,pcpu=,comm=")
	if err != nil {
		return nil
	}
	var all []Consumer
	for _, line := range strings.Split(string(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 3 {
			continue
		}
		pid, err1 := strconv.Atoi(fields[0])
		pcpu, err2 := strconv.ParseFloat(fields[1], 64)
		if err1 != nil || err2 != nil {
			continue
		}
		all = append(all, Consumer{PID: pid, PCPU: pcpu, Command: strings.Join(fields[2:], " ")})
	}
	sort.SliceStable(all, func(i, j int) bool { return all[i].PCPU > all[j].PCPU })
	if len(all) > topConsumers {
		all = all[:topConsumers]
	}
	return all
}

// Overloaded reports whether the sample's 1-minute load average exceeds
// factor * cores. A sample with no core count decides nothing (false): a
// zero-core reading is not evidence of starvation.
func Overloaded(s Sample, factor float64) bool {
	if s.Cores <= 0 {
		return false
	}
	return s.Load1 > factor*float64(s.Cores)
}

// Reason renders the single-line lead prepended to an overloaded verify's
// failure evidence. The verify's real output follows it verbatim, so the
// reviewer still sees the test failures.
func Reason(s Sample) string {
	var b strings.Builder
	fmt.Fprintf(&b, "host_overloaded: 1-min load average %.1f on %d cores", s.Load1, s.Cores)
	if s.Cores > 0 {
		fmt.Fprintf(&b, " (%.1fx)", s.Load1/float64(s.Cores))
	}
	if len(s.Top) > 0 {
		parts := make([]string, 0, len(s.Top))
		for _, c := range s.Top {
			parts = append(parts, fmt.Sprintf("pid %d %s %.1f%%", c.PID, c.Command, c.PCPU))
		}
		fmt.Fprintf(&b, "; top CPU: %s", strings.Join(parts, ", "))
	}
	return b.String()
}
