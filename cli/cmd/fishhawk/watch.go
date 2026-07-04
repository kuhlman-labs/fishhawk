package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/cli/internal/httpclient"
)

// `fishhawk run watch <run-id>` is the operator's BLOCKING wait-for-a-
// stage-to-settle verb (E32.3 / #1550). It polls the durable
// (run_id, stage_id) stage-wait status and the run's pending scope
// amendments over the ALREADY-EXISTING backend long-poll endpoints
// (GET /v0/runs/{run_id}/stages/{stage_id}?wait, #1252; GET
// /v0/runs/{run_id}/scope-amendments?wait, #1035) and exits with a
// distinct code per outcome class so an operator agent can dispatch a
// stage detached (fishhawk_dispatch_stage) and block on process
// completion with ZERO coupling to runner-log event names — replacing
// the fragile grep-the-log-for-a-guessed-event-name contract that
// silently stalled run 4459817d. It changes NO backend/runner/MCP
// surface; it reuses endpoints that already exist. The poll loop mirrors
// autoDecideLoop so both detached operator verbs share one shape.

// Watch outcome exit codes. terminal-ok and failed alias the CLI's
// standard exitOK/exitFailure; amendment-pending and timeout are watch-
// specific so a caller can `$?`-switch on the outcome class. Usage
// errors reuse exitUsage (2).
const (
	exitWatchTerminalOK       = exitOK      // 0
	exitWatchFailed           = exitFailure // 1
	exitWatchAmendmentPending = 3
	exitWatchTimeout          = 4
)

// watchPollDefault is the default per-iteration stage-wait ?wait seconds.
const watchPollDefault = 15

// watchPollCap mirrors backend maxRunStageWaitSeconds: a single ?wait
// holds at most this long server-side. We clamp our own flag to it so
// --poll above the cap still works but never claims a longer hold than
// the server honors.
const watchPollCap = 30

// watchMaxDurationDefault bounds the overall wait. Comfortably over an
// implement-stage budget so the watcher settles on the stage rather than
// timing out first.
const watchMaxDurationDefault = 50 * time.Minute

// watchUntil enumerates the settle condition the watcher blocks for.
const (
	watchUntilTerminal  = "terminal"
	watchUntilAmendment = "amendment"
	watchUntilAny       = "any"
)

// watchSummary is the one-line JSON summary emitted to stdout exactly
// once per invocation, immediately before the command returns, so an
// operator agent can `jq` the last stdout line regardless of exit class.
type watchSummary struct {
	RunID     string `json:"run_id"`
	StageID   string `json:"stage_id"`
	StageType string `json:"stage_type"`
	Until     string `json:"until"`
	Outcome   string `json:"outcome"` // terminal_ok | failed | amendment_pending | timeout | error
	State     string `json:"state"`
	ExitCode  int    `json:"exit_code"`
}

// runWatch implements `fishhawk run watch <run-id>`.
func runWatch(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("fishhawk run watch", flag.ContinueOnError)
	fs.SetOutput(stderr)
	cf := bindCommonFlags(fs)
	stageType := fs.String("stage", "implement",
		"stage TYPE to watch (e.g. implement, review, acceptance)")
	until := fs.String("until", watchUntilAny,
		"settle condition: terminal | amendment | any")
	poll := fs.Int("poll", watchPollDefault,
		fmt.Sprintf("per-iteration stage-wait long-poll seconds (clamped to the backend's %ds cap)", watchPollCap))
	maxDuration := fs.Duration("max-duration", watchMaxDurationDefault,
		"overall stop: emit a timeout summary and exit after this long if the stage never settles")
	positionals, err := parseIntermixed(fs, args)
	if err != nil {
		return exitUsage
	}
	switch *until {
	case watchUntilTerminal, watchUntilAmendment, watchUntilAny:
	default:
		_, _ = fmt.Fprintf(stderr, "fishhawk run watch: invalid --until %q (want terminal|amendment|any)\n", *until)
		return exitUsage
	}
	if len(positionals) != 1 {
		_, _ = fmt.Fprintln(stderr, "fishhawk run watch: <run-id> required")
		return exitUsage
	}
	runID, err := uuid.Parse(positionals[0])
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "fishhawk run watch: %q is not a UUID: %v\n", positionals[0], err)
		return exitUsage
	}
	pollSeconds := *poll
	if pollSeconds > watchPollCap {
		pollSeconds = watchPollCap
	}
	if pollSeconds <= 0 {
		pollSeconds = watchPollDefault
	}

	client := newClient(cf)

	ctx, cancel := context.WithTimeout(context.Background(), *maxDuration)
	defer cancel()

	// Resolve the stage id from the run: the operator passes a stage
	// TYPE, not a raw id (mirroring auto-decide's plan-stage resolution).
	// A resolution failure is a non-terminal failure path that still owes
	// the caller exactly one JSON summary (outcome=error) per the
	// exactly-one-summary-line contract — not a bare stderr error.
	stageID, resolveErr := resolveStageID(ctx, client, runID, *stageType)
	if resolveErr != nil {
		_, _ = fmt.Fprintf(stderr, "fishhawk run watch: %v\n", resolveErr)
		return emitWatchSummary(stdout, watchSummary{
			RunID:     runID.String(),
			StageID:   uuid.Nil.String(),
			StageType: *stageType,
			Until:     *until,
			Outcome:   "error",
			ExitCode:  exitWatchFailed,
		})
	}

	return watchLoop(ctx, client, runID, stageID, *stageType, *until, pollSeconds, stdout, stderr)
}

// resolveStageID lists the run's stages and returns the id of the stage
// whose Type == stageType. On a transport failure it returns the error;
// on no match it returns an actionable error naming the available stage
// types. Reuses the same list endpoint auto-decide's plan-stage
// resolution uses.
func resolveStageID(ctx context.Context, client *httpclient.Client, runID uuid.UUID, stageType string) (uuid.UUID, error) {
	stages, err := client.ListRunStages(ctx, runID)
	if err != nil {
		return uuid.Nil, fmt.Errorf("list stages: %w", err)
	}
	available := make([]string, 0, len(stages.Items))
	for _, st := range stages.Items {
		if st.Type == stageType {
			return st.ID, nil
		}
		available = append(available, st.Type)
	}
	return uuid.Nil, fmt.Errorf("no stage of type %q on run %s (available: %s)",
		stageType, runID, strings.Join(available, ", "))
}

// watchLoop is the poll→classify→emit loop, split out of runWatch so
// tests can drive it against an httptest.Server (mirroring
// autoDecideLoop). It emits EXACTLY ONE watchSummary line before
// returning, whichever exit class it lands in.
func watchLoop(ctx context.Context, client *httpclient.Client, runID, stageID uuid.UUID, stageType, until string, pollSeconds int, stdout, stderr io.Writer) int {
	// finish centralizes the exactly-one-summary contract: every return
	// path routes through it so stdout carries one JSON line per run.
	finish := func(outcome, state string, code int) int {
		return emitWatchSummary(stdout, watchSummary{
			RunID:     runID.String(),
			StageID:   stageID.String(),
			StageType: stageType,
			Until:     until,
			Outcome:   outcome,
			State:     state,
			ExitCode:  code,
		})
	}

	for {
		if ctx.Err() != nil {
			return finish("timeout", "", exitWatchTimeout)
		}

		// (b) Cheap non-blocking amendment check at the TOP of the loop
		// when the caller cares about amendments. A freshly-filed
		// amendment is thus observed within one poll iteration, before the
		// blocking stage-wait. Under --until terminal this is skipped
		// entirely — an amendment never ends the wait.
		if until == watchUntilAmendment || until == watchUntilAny {
			amendments, err := client.ListScopeAmendments(ctx, runID, 0)
			if err != nil {
				if ctx.Err() != nil {
					return finish("timeout", "", exitWatchTimeout)
				}
				_, _ = fmt.Fprintf(stderr, "fishhawk run watch: list amendments failed: %v\n", err)
				return finish("error", "", exitWatchFailed)
			}
			for _, am := range amendments {
				if am.Status == "pending" {
					return finish("amendment_pending", "", exitWatchAmendmentPending)
				}
			}
		}

		// (c) Blocking stage-wait: returns the moment the stage settles
		// (terminal OR parked) or after the server-side ?wait cap.
		st, err := client.GetRunStageWait(ctx, runID, stageID, pollSeconds)
		if err != nil {
			if ctx.Err() != nil {
				return finish("timeout", "", exitWatchTimeout)
			}
			_, _ = fmt.Fprintf(stderr, "fishhawk run watch: stage wait failed: %v\n", err)
			return finish("error", "", exitWatchFailed)
		}

		// (d) A settled stage ends the wait for EVERY --until mode,
		// including amendment: once the stage is terminal no amendment can
		// ever arrive, so we return the terminal outcome rather than hang
		// waiting for one (E32.3 binding condition 1).
		if st.Terminal {
			if st.State == "failed" || st.FailureCategory != nil {
				return finish("failed", st.State, exitWatchFailed)
			}
			return finish("terminal_ok", st.State, exitWatchTerminalOK)
		}
		// Not settled: loop. The top-of-loop ctx check and the amendment
		// check drive the timeout / amendment_pending exits.
	}
}

// emitWatchSummary writes the single JSON summary line to stdout and
// returns the summary's exit code, so callers `return emitWatchSummary(...)`.
func emitWatchSummary(stdout io.Writer, s watchSummary) int {
	_ = json.NewEncoder(stdout).Encode(s)
	return s.ExitCode
}
