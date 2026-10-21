package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/kuhlman-labs/fishhawk/runner/internal/agent"
)

// Fix-up session resume (E83.56 / #4089). An operator fix-up pass RESUMES the
// implement agent's Claude Code session (agent.Invocation.ResumeSessionID →
// `claude --resume <id>`) instead of re-orienting a fresh agent, so the
// resumed turn is only the backend's fetched fix-up prompt. The runner records
// each successful implement agent's session in a sidecar ledger beside the
// lineage worktree and decides, per fix-up dispatch, whether it can resume.
// Every case that cannot resume falls back to today's full fresh fix-up and
// says why in a fixup_session_resume trace event and runner log line.
// Long-form: README.md § "Fix-up session resume (E83.56 / #4089)".

// fixupResumeEnvVar is the operator kill switch: the exact value "off"
// (surrounding whitespace ignored) disables resume, so every fix-up spawns
// fresh. Any other value, including empty, leaves resume on.
const fixupResumeEnvVar = "FISHHAWK_FIXUP_RESUME"

// fixupResumeGetenv is the environment reader decideFixupResume consults for
// the kill switch. A package-level var in the lineageSeedGetenv shape, so a
// test drives the switch without t.Setenv.
var fixupResumeGetenv = os.Getenv

// agentSessionLedgerSchema versions the ledger file. A file carrying any other
// schema is unreadable (ledger_unreadable), never guessed at.
const agentSessionLedgerSchema = "agent-sessions-v1"

// Trace / log event names.
const (
	fixupSessionResumeEvent         = "fixup_session_resume"
	agentSessionRecordedEvent       = "agent_session_recorded"
	agentSessionLedgerDegradedEvent = "agent_session_ledger_degraded"
)

// fixup_session_resume outcomes and reasons. Every reason but
// fixupResumeReasonResumed rides outcome "fallback".
const (
	fixupResumeOutcomeResumed  = "resumed"
	fixupResumeOutcomeFallback = "fallback"

	fixupResumeReasonResumed            = "resumed"
	fixupResumeReasonDisabled           = "disabled"
	fixupResumeReasonUnsupportedAgent   = "unsupported_agent"
	fixupResumeReasonNoSessionRecorded  = "no_session_recorded"
	fixupResumeReasonLedgerUnreadable   = "ledger_unreadable"
	fixupResumeReasonAgentMismatch      = "agent_mismatch"
	fixupResumeReasonModelMismatch      = "model_mismatch"
	fixupResumeReasonWorkingDirMismatch = "working_dir_mismatch"
	fixupResumeReasonResumeFailed       = "resume_failed"
	fixupResumeReasonSelfRetry          = "self_retry"
)

// errNoSessionLedger marks a fix-up dispatch with no lineage worktree, so no
// ledger path to read. Unreachable on the fetched-prompt path a fix-up always
// takes; mapped to ledger_unreadable rather than assumed empty.
var errNoSessionLedger = errors.New("no lineage worktree, so no agent-session ledger")

// agentSessionRecord is one run's recorded implement session.
type agentSessionRecord struct {
	SessionID      string    `json:"session_id"`
	Agent          string    `json:"agent"`
	AgentBinary    string    `json:"agent_binary"`
	RequestedModel string    `json:"requested_model"`
	ReportedModel  string    `json:"reported_model"`
	WorkingDir     string    `json:"working_dir"`
	StageID        string    `json:"stage_id"`
	RecordedAt     time.Time `json:"recorded_at"`
}

// agentSessionLedger is the on-disk shape: one file per lineage, keyed by the
// FULL run id, because decomposed siblings share one lineage worktree.
type agentSessionLedger struct {
	Schema string                        `json:"schema"`
	Runs   map[string]agentSessionRecord `json:"runs"`
}

// agentSessionLedgerPath returns the lineage's ledger path. Like the .runid
// sidecar and the .lock it sits BESIDE the run-<root> worktree, never inside
// it, so it can never be swept into a run's commit.
func agentSessionLedgerPath(wtDir, root string) string {
	return filepath.Join(wtDir, "run-"+root+".agent-sessions.json")
}

// readAgentSessionLedger reads the ledger at path. An absent file is an empty
// ledger, not an error; unparseable JSON or a foreign schema is an error.
func readAgentSessionLedger(path string) (agentSessionLedger, error) {
	empty := agentSessionLedger{Schema: agentSessionLedgerSchema, Runs: map[string]agentSessionRecord{}}
	data, err := os.ReadFile(path) //nolint:gosec // runner-owned sidecar under the shared git dir
	if errors.Is(err, os.ErrNotExist) {
		return empty, nil
	}
	if err != nil {
		return empty, err
	}
	var l agentSessionLedger
	if err := json.Unmarshal(data, &l); err != nil {
		return empty, fmt.Errorf("parse agent-session ledger %s: %w", path, err)
	}
	if l.Schema != agentSessionLedgerSchema {
		return empty, fmt.Errorf("agent-session ledger %s: schema %q, want %q", path, l.Schema, agentSessionLedgerSchema)
	}
	if l.Runs == nil {
		l.Runs = map[string]agentSessionRecord{}
	}
	return l, nil
}

// lookupAgentSession returns runID's recorded session, nil when none is
// recorded. An empty path is errNoSessionLedger.
func lookupAgentSession(path, runID string) (*agentSessionRecord, error) {
	if path == "" {
		return nil, errNoSessionLedger
	}
	l, err := readAgentSessionLedger(path)
	if err != nil {
		return nil, err
	}
	rec, ok := l.Runs[runID]
	if !ok {
		return nil, nil
	}
	return &rec, nil
}

// writeAgentSessionRecord upserts runID's record. The caller holds the
// lineage lock (same-lineage stages are serialized), so read-modify-write is
// race-free; the write itself is atomic (temp file + rename) so a crash never
// leaves a torn ledger. An unreadable existing ledger is replaced rather than
// failing the record: it is already useless to every reader.
func writeAgentSessionRecord(path, runID string, rec agentSessionRecord) error {
	l, err := readAgentSessionLedger(path)
	if err != nil {
		l = agentSessionLedger{Schema: agentSessionLedgerSchema, Runs: map[string]agentSessionRecord{}}
	}
	l.Runs[runID] = rec
	data, err := json.MarshalIndent(l, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(append(data, '\n')); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		_ = os.Remove(tmpName)
		return err
	}
	return nil
}

// fixupResumeInputs is what the current fix-up dispatch would spawn with.
type fixupResumeInputs struct {
	agent          string
	agentBinary    string
	requestedModel string
	workingDir     string
}

// fixupResumeDecision is decideFixupResume's verdict. resume is true only for
// reason "resumed"; sessionID is then the id to resume.
type fixupResumeDecision struct {
	resume    bool
	sessionID string
	reason    string
}

// decideFixupResume decides whether a fix-up can resume the recorded implement
// session. Pure: the ledger was read by the caller (rec, readErr) and the kill
// switch comes through getenv. The guards run in order, the first that applies
// naming the fallback reason; a session is resumed only when the recorded
// spawn matches the current one on agent id, agent binary, requested model and
// working directory (claude files sessions per cwd, and a different binary or
// model would continue a conversation the current spawn did not hold).
func decideFixupResume(in fixupResumeInputs, rec *agentSessionRecord, readErr error, getenv func(string) string) fixupResumeDecision {
	fallback := func(reason string) fixupResumeDecision { return fixupResumeDecision{reason: reason} }
	switch {
	case strings.TrimSpace(getenv(fixupResumeEnvVar)) == "off":
		return fallback(fixupResumeReasonDisabled)
	case in.agent != "claude-code":
		return fallback(fixupResumeReasonUnsupportedAgent)
	case readErr != nil:
		return fallback(fixupResumeReasonLedgerUnreadable)
	case rec == nil || rec.SessionID == "":
		return fallback(fixupResumeReasonNoSessionRecorded)
	case rec.Agent != in.agent || rec.AgentBinary != in.agentBinary:
		return fallback(fixupResumeReasonAgentMismatch)
	case rec.RequestedModel != in.requestedModel:
		return fallback(fixupResumeReasonModelMismatch)
	case rec.WorkingDir != in.workingDir:
		return fallback(fixupResumeReasonWorkingDirMismatch)
	}
	return fixupResumeDecision{resume: true, sessionID: rec.SessionID, reason: fixupResumeReasonResumed}
}

// resolveFixupResume reads runID's ledger entry and decides against the
// spawn inv describes.
func resolveFixupResume(ledgerPath string, cfg *config, inv agent.Invocation, getenv func(string) string) fixupResumeDecision {
	rec, readErr := lookupAgentSession(ledgerPath, cfg.runID)
	return decideFixupResume(fixupResumeInputs{
		agent:          cfg.agent,
		agentBinary:    cfg.agentBinary,
		requestedModel: inv.Model,
		workingDir:     inv.WorkingDir,
	}, rec, readErr, getenv)
}

// primaryResumeDecision returns the decision invokePrimaryAgent applies on the
// agent loop's attempt number selfRetryCount. Only the FIRST attempt may
// resume: an ADR-023 self-retry follows a failed attempt, so it spawns fresh
// exactly as before #4089 and is traced as fallback self_retry. nil (not a
// fix-up) stays nil.
func primaryResumeDecision(d *fixupResumeDecision, selfRetryCount int) *fixupResumeDecision {
	if d == nil || selfRetryCount == 0 {
		return d
	}
	return &fixupResumeDecision{reason: fixupResumeReasonSelfRetry}
}

// fixupResumeTrace appends the fixup_session_resume event to events and logs
// the matching runner line.
func fixupResumeTrace(events []agent.Event, cfg *config, outcome, reason, sessionID, detail string, logSink io.Writer) []agent.Event {
	payload := map[string]string{"outcome": outcome, "reason": reason}
	if sessionID != "" {
		payload["session_id"] = sessionID
	}
	if detail != "" {
		payload["detail"] = detail
	}
	_, _ = fmt.Fprintf(logSink,
		`{"event":%q,"run_id":%q,"stage_id":%q,"outcome":%q,"reason":%q,"session_id":%q,"detail":%q}`+"\n",
		fixupSessionResumeEvent, cfg.runID, cfg.stageID, outcome, reason, sessionID, detail)
	return append(events, agent.Event{
		Kind:      fixupSessionResumeEvent,
		Timestamp: time.Now(),
		Payload:   agent.MakePayload(payload),
	})
}

// invokePrimaryAgent runs the agent loop's primary invocation. d is nil on a
// non-fix-up stage: a plain Invoke with no event. On a fix-up that cannot
// resume it is a plain Invoke traced as fallback with d.reason. On a resume it
// invokes a BY-VALUE copy of inv carrying ResumeSessionID (the caller's inv,
// which the verify-fix and base-rebase re-invokes copy, is never mutated); when
// the CLI refuses the resume (agent.ErrSessionResumeFailed) it traces fallback
// resume_failed and re-invokes the ORIGINAL inv fresh. The two attempts'
// events and token buckets are summed, and only the fresh attempt's
// SessionID / Model / StructuredOutput survive, so the dead id the CLI echoes
// on a failed resume is never recorded.
func invokePrimaryAgent(ctx context.Context, invoker agent.Invoker, inv agent.Invocation, d *fixupResumeDecision, cfg *config, logSink io.Writer) (agent.Result, error) {
	if d == nil {
		return invoker.Invoke(ctx, inv)
	}
	if !d.resume {
		res, err := invoker.Invoke(ctx, inv)
		res.Events = fixupResumeTrace(res.Events, cfg, fixupResumeOutcomeFallback, d.reason, "", "", logSink)
		return res, err
	}
	resumeInv := inv
	resumeInv.ResumeSessionID = d.sessionID
	res, err := invoker.Invoke(ctx, resumeInv)
	if !errors.Is(err, agent.ErrSessionResumeFailed) {
		res.Events = fixupResumeTrace(res.Events, cfg, fixupResumeOutcomeResumed, fixupResumeReasonResumed, d.sessionID, "", logSink)
		return res, err
	}
	events := fixupResumeTrace(res.Events, cfg, fixupResumeOutcomeFallback, fixupResumeReasonResumeFailed, d.sessionID, err.Error(), logSink)
	fresh, freshErr := invoker.Invoke(ctx, inv)
	fresh.Events = append(events, fresh.Events...)
	fresh.TokensUsed += res.TokensUsed
	fresh.InputTokens += res.InputTokens
	fresh.OutputTokens += res.OutputTokens
	fresh.CacheReadInputTokens += res.CacheReadInputTokens
	fresh.CacheWriteInputTokens += res.CacheWriteInputTokens
	return fresh, freshErr
}

// recordAgentSession writes an implement stage's primary session into the
// ledger so a later fix-up of the same run can resume it. It records only an
// OK primary carrying a session id (a failed resume echoes the dead requested
// id, so a failed Result is never trusted), appends one agent_session_recorded
// event, and degrades to an agent_session_ledger_degraded log line on any IO
// error: the ledger is an optimization and never fails the stage. An empty
// ledgerPath (no lineage worktree) records nothing.
func recordAgentSession(ledgerPath, stageType string, cfg *config, inv agent.Invocation, res *agent.Result, logSink io.Writer) {
	if ledgerPath == "" || stageType != "implement" || !res.OK || res.SessionID == "" {
		return
	}
	rec := agentSessionRecord{
		SessionID:      res.SessionID,
		Agent:          cfg.agent,
		AgentBinary:    cfg.agentBinary,
		RequestedModel: inv.Model,
		ReportedModel:  res.Model,
		WorkingDir:     inv.WorkingDir,
		StageID:        cfg.stageID,
		RecordedAt:     time.Now().UTC(),
	}
	if err := writeAgentSessionRecord(ledgerPath, cfg.runID, rec); err != nil {
		_, _ = fmt.Fprintf(logSink,
			`{"event":%q,"run_id":%q,"stage_id":%q,"path":%q,"detail":%q}`+"\n",
			agentSessionLedgerDegradedEvent, cfg.runID, cfg.stageID, ledgerPath, err.Error())
		return
	}
	_, _ = fmt.Fprintf(logSink,
		`{"event":%q,"run_id":%q,"stage_id":%q,"session_id":%q,"path":%q}`+"\n",
		agentSessionRecordedEvent, cfg.runID, cfg.stageID, rec.SessionID, ledgerPath)
	res.Events = append(res.Events, agent.Event{
		Kind:      agentSessionRecordedEvent,
		Timestamp: time.Now(),
		Payload:   agent.MakePayload(map[string]string{"session_id": rec.SessionID}),
	})
}
