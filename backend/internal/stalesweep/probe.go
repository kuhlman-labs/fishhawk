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
// stageIDArgFlag is read with the same exact-token rule for the ops monitor's
// orphaned-runner probe (E85.1 / #4142).
const (
	runnerBinaryName = "fishhawk-runner"
	runIDArgFlag     = "--run-id"
	stageIDArgFlag   = "--stage-id"
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
	raw, err := p.ps(ctx)
	if err != nil {
		return ProbeReport{}, err
	}
	return ParseRunnerProcesses(raw), nil
}

// Processes runs the same ps invocation as LiveRunners and returns one
// RunnerProcess per live fishhawk-runner (ParseRunnerProcessList). An exec
// error is returned as an error, never as a clean empty list.
func (p PSProbe) Processes(ctx context.Context) ([]RunnerProcess, error) {
	raw, err := p.ps(ctx)
	if err != nil {
		return nil, err
	}
	return ParseRunnerProcessList(raw), nil
}

func (p PSProbe) ps(ctx context.Context) (string, error) {
	execFn := p.Exec
	if execFn == nil {
		execFn = execOutput
	}
	raw, err := execFn(ctx, "ps", psArgs...)
	if err != nil {
		return "", fmt.Errorf("ps %s: %w", strings.Join(psArgs, " "), err)
	}
	return string(raw), nil
}

func execOutput(ctx context.Context, name string, args ...string) ([]byte, error) {
	return exec.CommandContext(ctx, name, args...).Output()
}

// RunnerProcess is one live fishhawk-runner process.
type RunnerProcess struct {
	PID string
	// RunID is the UUID after the FIRST token exactly equal to `--run-id`;
	// nil when that token is missing, has no value, is not a UUID, or only
	// the single-token `--run-id=<id>` form is present.
	RunID *uuid.UUID
	// StageID is the UUID after the FIRST token exactly equal to
	// `--stage-id`; nil when no such token exists or its value is missing or
	// not a UUID.
	StageID *uuid.UUID
	// StageIDMalformed reports a stage claim that could not be read: a
	// `--stage-id` token with a missing or non-UUID value, or the
	// single-token `--stage-id=<id>` form. A process with NO stage claim at
	// all leaves it false (StageID nil).
	StageIDMalformed bool
}

// ParseRunnerProcessList parses `<pid> <argv...>` lines exactly as scripts/dev
// _parse_live_runs does: the pid must be numeric; argv[0]'s basename, with
// everything from its first '.' stripped, must EQUAL fishhawk-runner; the run
// id is the token after the FIRST token exactly equal to `--run-id`, and the
// stage id the token after the FIRST token exactly equal to `--stage-id`. A
// process that merely MENTIONS fishhawk-runner in a later argument is
// ignored. Repeated pids are listed once (first occurrence wins).
func ParseRunnerProcessList(raw string) []RunnerProcess {
	var out []RunnerProcess
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
		proc := RunnerProcess{PID: pid}
		if v, ok := flagValue(tokens, runIDArgFlag); ok {
			if id, err := uuid.Parse(v); err == nil {
				proc.RunID = &id
			}
		}
		if v, ok := flagValue(tokens, stageIDArgFlag); ok {
			if id, err := uuid.Parse(v); err == nil {
				proc.StageID = &id
			} else {
				proc.StageIDMalformed = true
			}
		} else if hasPrefixToken(tokens, stageIDArgFlag+"=") {
			proc.StageIDMalformed = true
		}
		out = append(out, proc)
	}
	return out
}

// flagValue returns the token after the FIRST token (from argv[0] on) exactly
// equal to flag. ok reports that the flag token exists; the value is "" when
// it is the last token.
func flagValue(tokens []string, flag string) (string, bool) {
	for i := 1; i < len(tokens); i++ {
		if tokens[i] != flag {
			continue
		}
		if i+1 < len(tokens) {
			return tokens[i+1], true
		}
		return "", true
	}
	return "", false
}

func hasPrefixToken(tokens []string, prefix string) bool {
	for _, tok := range tokens[1:] {
		if strings.HasPrefix(tok, prefix) {
			return true
		}
	}
	return false
}

// ParseRunnerProcesses projects ParseRunnerProcessList onto the sweep's
// report, so ONE identity rule exists: a process with a run id is attributed,
// one without (missing, valueless, non-UUID, or the single-token
// `--run-id=<id>` form, which is never parsed) is unattributed.
func ParseRunnerProcesses(raw string) ProbeReport {
	rep := ProbeReport{RunIDs: map[uuid.UUID]bool{}}
	for _, proc := range ParseRunnerProcessList(raw) {
		if proc.RunID == nil {
			rep.UnattributedPIDs = append(rep.UnattributedPIDs, proc.PID)
			continue
		}
		rep.RunIDs[*proc.RunID] = true
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
