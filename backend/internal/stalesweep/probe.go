package stalesweep

import (
	"context"
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/google/uuid"
)

// runnerBinaryName and runIDArgFlag are the identity tokens scripts/dev
// _parse_live_runs and runner/cmd/fishhawk-runner/lockholder.go match on.
const (
	runnerBinaryName = "fishhawk-runner"
	runIDArgFlag     = "--run-id"
)

// psArgs is the scripts/dev _scan_live_runs invocation. -ww is load-bearing:
// BSD ps truncates the args column to the terminal width otherwise, which
// would hide the --run-id pair and SILENTLY under-report.
var psArgs = []string{"-axww", "-o", "pid=,args="}

// ProbeReport is one live-runner scan.
type ProbeReport struct {
	// RunIDs is the set of run ids a live fishhawk-runner carries as an
	// adjacent `--run-id <uuid>` pair.
	RunIDs map[uuid.UUID]bool
	// UnattributedPIDs are live fishhawk-runner processes whose run id could
	// not be read: no `--run-id` token, no token after it, a non-UUID value,
	// or the single-token `--run-id=<id>` form (which neither scripts/dev nor
	// the runner's lockholder recognises). Apply must refuse on any.
	UnattributedPIDs []string
	// Err is set by Find when the probe failed (or none was configured); the
	// scan then carries no run ids and Apply must refuse.
	Err error
}

// RunnerProbe reports the live fishhawk-runner processes on this host.
type RunnerProbe interface {
	LiveRunners(ctx context.Context) (ProbeReport, error)
}

// PSProbe is the production RunnerProbe: `ps -axww -o pid=,args=` parsed by
// ParseRunnerProcesses. Exec is a test seam; nil runs the real command.
type PSProbe struct {
	Exec func(ctx context.Context, name string, args ...string) ([]byte, error)
}

// LiveRunners runs ps and parses its output. An exec error — ps absent (as in
// the distroless fishhawkd image) or a non-zero exit — is returned as an
// error, never as a clean empty scan.
func (p PSProbe) LiveRunners(ctx context.Context) (ProbeReport, error) {
	execFn := p.Exec
	if execFn == nil {
		execFn = execOutput
	}
	raw, err := execFn(ctx, "ps", psArgs...)
	if err != nil {
		return ProbeReport{}, fmt.Errorf("ps %s: %w", strings.Join(psArgs, " "), err)
	}
	return ParseRunnerProcesses(string(raw)), nil
}

func execOutput(ctx context.Context, name string, args ...string) ([]byte, error) {
	return exec.CommandContext(ctx, name, args...).Output()
}

// ParseRunnerProcesses parses `<pid> <argv...>` lines exactly as scripts/dev
// _parse_live_runs does: the pid must be numeric; argv[0]'s basename, with
// everything from its first '.' stripped, must EQUAL fishhawk-runner; the run
// id is the token after the FIRST token exactly equal to `--run-id`. A process
// that merely MENTIONS fishhawk-runner in a later argument is ignored. A
// matching process whose run id is missing or not a UUID is unattributed —
// including the single-token `--run-id=<id>` form, which is never parsed.
// Repeated pids are counted once.
func ParseRunnerProcesses(raw string) ProbeReport {
	rep := ProbeReport{RunIDs: map[uuid.UUID]bool{}}
	seen := map[string]bool{}
	for _, line := range strings.Split(raw, "\n") {
		tokens := strings.Fields(line)
		if len(tokens) < 2 || !isDigits(tokens[0]) {
			continue
		}
		pid := tokens[0]
		if !binaryIsRunner(tokens[1]) || seen[pid] {
			continue
		}
		seen[pid] = true
		runID := ""
		for i := 1; i < len(tokens); i++ {
			if tokens[i] != runIDArgFlag {
				continue
			}
			if i+1 < len(tokens) {
				runID = tokens[i+1]
			}
			break
		}
		id, err := uuid.Parse(runID)
		if err != nil {
			rep.UnattributedPIDs = append(rep.UnattributedPIDs, pid)
			continue
		}
		rep.RunIDs[id] = true
	}
	return rep
}

func binaryIsRunner(token string) bool {
	base := filepath.Base(token)
	if i := strings.IndexByte(base, '.'); i >= 0 {
		base = base[:i]
	}
	return base == runnerBinaryName
}

func isDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}
