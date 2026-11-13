package main

import (
	"context"
	"fmt"
	"io"

	"github.com/kuhlman-labs/fishhawk/runner/internal/agent"
	"github.com/kuhlman-labs/fishhawk/runner/internal/netsandbox"
)

// implementSignalSandboxEnvVar is the operator policy for the Seatbelt signal
// confinement of implement-stage agent spawns (#4858, extending ADR-050 /
// #4855): auto (default — wrap when the host enforces the profile, otherwise
// spawn unwrapped LOUDLY), require (fail the stage category-C pre-spawn when
// unavailable), off (never probe, never wrap — the kill switch). It is
// runner-process config: agentenv's FISHHAWK_ deny prefix drops it from the
// agent env.
const implementSignalSandboxEnvVar = "FISHHAWK_IMPLEMENT_SIGNAL_SANDBOX"

// probeImplementSignalSandbox is the host probe configureImplementSignalSandbox
// consults; a package-level var so tests can swap it (runTestMain stubs it
// package-wide so no test execs sandbox-exec by accident).
var probeImplementSignalSandbox = netsandbox.ProbeSignalConfinement

// Trace outcomes of the implement_signal_sandbox event.
const (
	implementSignalApplied     = "applied"
	implementSignalUnavailable = "unavailable"
	implementSignalDisabled    = "disabled"
	// implementSignalNotSpawned is the applied deterministic fix-up (#1165):
	// no agent spawns, so the policy is never evaluated and nothing is
	// claimed about confinement (signals_confined is omitted).
	implementSignalNotSpawned = "not_spawned"
)

// implementSignalSandboxOutcome is one stage's resolved signal-confinement
// configuration: the ExecWrapper for its agent spawns (nil = unwrapped) and
// the fields of its trace event.
type implementSignalSandboxOutcome struct {
	wrapper  []string
	outcome  string
	mode     netsandbox.Mode
	reason   string
	confined bool
}

// implementSignalNotSpawnedOutcome is the outcome of a stage that spawns no
// agent (the applied deterministic fix-up): no probe ran, no wrapper.
func implementSignalNotSpawnedOutcome() implementSignalSandboxOutcome {
	return implementSignalSandboxOutcome{outcome: implementSignalNotSpawned}
}

// traceEvent is the stage's ONE implement_signal_sandbox trace event (R8): it
// rides the uploaded raw trace bundle, so an operator can see a stage that ran
// UNCONFINED under auto — the stderr events reach only the local runner log.
func (o implementSignalSandboxOutcome) traceEvent() agent.Event {
	p := map[string]any{"outcome": o.outcome}
	if o.mode != "" {
		p["mode"] = string(o.mode)
	}
	if o.reason != "" {
		p["reason"] = o.reason
	}
	if o.outcome != implementSignalNotSpawned {
		p["signals_confined"] = o.confined
	}
	return agent.Event{Kind: "implement_signal_sandbox", Payload: agent.MakePayload(p)}
}

// implementSignalSandboxRefusal is the conflict-resolution invoker's typed
// pre-spawn refusal (R1): runConflictResolutionPass maps it to a category-C
// refusal (environment, not judgement) instead of a generic agent failure.
type implementSignalSandboxRefusal struct {
	Reason string
	Detail string
}

func (r *implementSignalSandboxRefusal) Error() string {
	if r.Detail == "" {
		return r.Reason
	}
	return r.Reason + ": " + r.Detail
}

// configureImplementSignalSandbox decides whether this stage's implement
// agent spawns are wrapped in netsandbox.SignalProfile() and logs EXACTLY ONE
// event per call (one call per stage configuration). A non-empty failReason
// is a category-C stage failure the caller reports BEFORE any spawn.
//
// Branches (each asserted by implementsandbox_test.go):
//   - invalid mode          -> implement_signal_sandbox_config {var, detail} (fail)
//   - off                   -> implement_signal_sandbox_disabled, probe never called, no wrapper
//   - auto + unavailable    -> implement_signal_sandbox_unavailable {reason}, no wrapper — loud
//   - require + unavailable -> implement_signal_sandbox_required {reason} (fail)
//   - available             -> implement_signal_sandbox_applied {mechanism:"seatbelt"},
//     wrapper = sandbox-exec -p <SignalProfile>
func configureImplementSignalSandbox(ctx context.Context, getenv func(string) string, probe func(context.Context) (bool, string), logSink io.Writer) (cfg implementSignalSandboxOutcome, failReason, failDetail string) {
	mode, err := netsandbox.ParseMode(getenv(implementSignalSandboxEnvVar))
	if err != nil {
		_, _ = fmt.Fprintf(logSink,
			`{"event":"implement_signal_sandbox_config","var":%q,"detail":%q}`+"\n",
			implementSignalSandboxEnvVar, err.Error())
		return implementSignalSandboxOutcome{}, "implement_signal_sandbox_config", err.Error()
	}
	if mode == netsandbox.ModeOff {
		_, _ = fmt.Fprintf(logSink,
			`{"event":"implement_signal_sandbox_disabled","mode":%q,"signals_confined":false}`+"\n", mode)
		return implementSignalSandboxOutcome{outcome: implementSignalDisabled, mode: mode}, "", ""
	}
	available, reason := probe(ctx)
	if !available {
		if mode == netsandbox.ModeRequire {
			_, _ = fmt.Fprintf(logSink,
				`{"event":"implement_signal_sandbox_required","mode":%q,"reason":%q}`+"\n", mode, reason)
			return implementSignalSandboxOutcome{}, "implement_signal_sandbox_required", reason
		}
		_, _ = fmt.Fprintf(logSink,
			`{"event":"implement_signal_sandbox_unavailable","mode":%q,"reason":%q,"signals_confined":false}`+"\n",
			mode, reason)
		return implementSignalSandboxOutcome{outcome: implementSignalUnavailable, mode: mode, reason: reason}, "", ""
	}
	_, _ = fmt.Fprintf(logSink,
		`{"event":"implement_signal_sandbox_applied","mode":%q,"mechanism":"seatbelt","signals_confined":true}`+"\n", mode)
	// The adapter appends the binary + args (agent.WrapArgv), so the wrapper
	// is the sandbox-exec prefix with NO argv.
	return implementSignalSandboxOutcome{
		wrapper:  netsandbox.Wrap(nil, netsandbox.SignalProfile()),
		outcome:  implementSignalApplied,
		mode:     mode,
		confined: true,
	}, "", ""
}
