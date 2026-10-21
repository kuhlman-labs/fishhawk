package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/kuhlman-labs/fishhawk/runner/internal/agent"
	"github.com/kuhlman-labs/fishhawk/runner/internal/bundle"
	"github.com/kuhlman-labs/fishhawk/runner/internal/upload"
)

// Fix-up session resume tests (E83.56 / #4089). The run()-level tests share
// the package's throwaway CWD repo (runTestMain), so each uses its OWN
// UUID-shaped run id (a distinct lineage root, so a distinct ledger file) and
// removes that ledger in t.Cleanup: a session recorded by one test can never
// leak into another test's fix-up.

const fixupResumeStageID = "22222222-3333-4444-5555-666666666666"

// fixupResumeArgs is the run() argv for one stage of runID.
func fixupResumeArgs(runID string) []string {
	return []string{
		"--run-id", runID,
		"--backend-url", "https://api.fishhawk.test",
		"--workflow", "feature_change", "--stage", "implement",
		"--stage-id", fixupResumeStageID,
		"--fetch-prompt", "--upload-trace",
	}
}

// cwdLineage returns the lineage worktree path and ledger path run() uses for
// runID in the CWD repo, and removes the ledger before and after the test.
func cwdLineage(t *testing.T, runID string) (worktree, ledger string) {
	t.Helper()
	wtDir, err := worktreesDir(context.Background(), ".")
	if err != nil {
		t.Fatalf("worktreesDir: %v", err)
	}
	root := shortID(runID)
	ledger = agentSessionLedgerPath(wtDir, root)
	_ = os.Remove(ledger)
	t.Cleanup(func() { _ = os.Remove(ledger) })
	return filepath.Join(wtDir, "run-"+root), ledger
}

// seedLedger writes a record for runID that matches what a claude-code
// fix-up of that run will spawn with (default binary resolution, no model).
func seedLedger(t *testing.T, ledger, runID, worktree, sessionID string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(ledger), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := writeAgentSessionRecord(ledger, runID, agentSessionRecord{
		SessionID:   sessionID,
		Agent:       "claude-code",
		AgentBinary: effectiveAgentBinary("claude-code", os.Getenv),
		WorkingDir:  worktree,
		StageID:     fixupResumeStageID,
		RecordedAt:  time.Now().UTC(),
	}); err != nil {
		t.Fatalf("seed ledger: %v", err)
	}
}

// fixupRespFor is fixupPromptResp with a caller-chosen prompt body.
func fixupRespFor(prompt string) *upload.FetchedPrompt {
	r := fixupPromptResp("fixup-branch-tip-sha")
	r.Prompt = prompt
	return r
}

// shippedRaw opens the raw ShipTrace bundle the fake uploader received.
func shippedRaw(t *testing.T, fu *fakeUploader) (bundle.ManifestData, []bundle.Line) {
	t.Helper()
	for _, c := range fu.gotShipCalls {
		if c.Variant != "raw" {
			continue
		}
		m, lines, _, err := openBundleForTest(c.Bundle)
		if err != nil {
			t.Fatalf("open raw bundle: %v", err)
		}
		return m, lines
	}
	t.Fatalf("no raw bundle shipped (%d ShipTrace calls)", len(fu.gotShipCalls))
	return bundle.ManifestData{}, nil
}

// resumeEvents returns every fixup_session_resume payload in lines, in order.
func resumeEvents(t *testing.T, lines []bundle.Line) []map[string]string {
	t.Helper()
	var out []map[string]string
	for _, l := range lines {
		if l.Kind != fixupSessionResumeEvent {
			continue
		}
		var p map[string]string
		if err := json.Unmarshal(l.Data, &p); err != nil {
			t.Fatalf("decode %s payload: %v", fixupSessionResumeEvent, err)
		}
		out = append(out, p)
	}
	return out
}

// B1. One row per reason; every fallback row differs from the resumed
// baseline in EXACTLY ONE input, so deleting that row's guard flips the row
// to "resumed".
func TestDecideFixupResume(t *testing.T) {
	base := fixupResumeInputs{agent: "claude-code", agentBinary: "/opt/claude", requestedModel: "m1", workingDir: "/wt/run-1"}
	rec := agentSessionRecord{SessionID: "S1", Agent: "claude-code", AgentBinary: "/opt/claude", RequestedModel: "m1", WorkingDir: "/wt/run-1"}
	on := func(string) string { return "" }

	cases := []struct {
		name    string
		in      fixupResumeInputs
		rec     *agentSessionRecord
		readErr error
		getenv  func(string) string
		want    string
	}{
		{name: "resumed baseline", in: base, rec: &rec, getenv: on, want: fixupResumeReasonResumed},
		{name: "kill switch off", in: base, rec: &rec, getenv: func(k string) string {
			if k == fixupResumeEnvVar {
				return " off "
			}
			return ""
		}, want: fixupResumeReasonDisabled},
		{name: "kill switch other value ignored", in: base, rec: &rec, getenv: func(string) string { return "0" }, want: fixupResumeReasonResumed},
		{name: "unsupported agent", in: withAgent(base, "codex"), rec: recWithAgent(rec, "codex"), getenv: on, want: fixupResumeReasonUnsupportedAgent},
		{name: "ledger unreadable", in: base, rec: &rec, readErr: errors.New("corrupt"), getenv: on, want: fixupResumeReasonLedgerUnreadable},
		{name: "no session recorded", in: base, rec: nil, getenv: on, want: fixupResumeReasonNoSessionRecorded},
		{name: "empty session id", in: base, rec: recWithSession(rec, ""), getenv: on, want: fixupResumeReasonNoSessionRecorded},
		{name: "agent id mismatch", in: base, rec: recWithAgent(rec, "claude-code-2"), getenv: on, want: fixupResumeReasonAgentMismatch},
		{name: "agent binary mismatch", in: base, rec: recWithBinary(rec, "/usr/local/bin/claude"), getenv: on, want: fixupResumeReasonAgentMismatch},
		{name: "model mismatch", in: base, rec: recWithModel(rec, "m2"), getenv: on, want: fixupResumeReasonModelMismatch},
		{name: "working dir mismatch", in: base, rec: recWithDir(rec, "/old/wt/run-1"), getenv: on, want: fixupResumeReasonWorkingDirMismatch},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := decideFixupResume(tc.in, tc.rec, tc.readErr, tc.getenv)
			if got.reason != tc.want {
				t.Fatalf("reason = %q, want %q", got.reason, tc.want)
			}
			wantResume := tc.want == fixupResumeReasonResumed
			if got.resume != wantResume {
				t.Errorf("resume = %v, want %v", got.resume, wantResume)
			}
			if wantResume && got.sessionID != "S1" {
				t.Errorf("sessionID = %q, want S1", got.sessionID)
			}
			if !wantResume && got.sessionID != "" {
				t.Errorf("a fallback carried sessionID %q", got.sessionID)
			}
		})
	}
}

func withAgent(in fixupResumeInputs, a string) fixupResumeInputs { in.agent = a; return in }
func recWithAgent(r agentSessionRecord, a string) *agentSessionRecord {
	r.Agent = a
	return &r
}
func recWithSession(r agentSessionRecord, s string) *agentSessionRecord {
	r.SessionID = s
	return &r
}
func recWithBinary(r agentSessionRecord, b string) *agentSessionRecord {
	r.AgentBinary = b
	return &r
}
func recWithModel(r agentSessionRecord, m string) *agentSessionRecord {
	r.RequestedModel = m
	return &r
}
func recWithDir(r agentSessionRecord, d string) *agentSessionRecord {
	r.WorkingDir = d
	return &r
}

// B2. Two runs' records share one lineage ledger, each read back from DISK;
// the file sits beside the worktree under fishhawk-worktrees/, never inside
// it; the write leaves no temp file behind; a corrupt or foreign-schema file
// is a read error, an absent one an empty ledger.
func TestAgentSessionLedger_RoundTripAndAtomicWrite(t *testing.T) {
	repo := initRepo(t)
	ctx := context.Background()
	const root = "ledger01"
	wt, err := provisionLineageWorktree(ctx, repo, root, "main", io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	wtDir, err := worktreesDir(ctx, repo)
	if err != nil {
		t.Fatal(err)
	}
	path := agentSessionLedgerPath(wtDir, root)

	if rec, err := lookupAgentSession(path, "run-a"); err != nil || rec != nil {
		t.Fatalf("absent ledger: rec=%v err=%v, want nil, nil", rec, err)
	}
	a := agentSessionRecord{SessionID: "SA", Agent: "claude-code", WorkingDir: wt, StageID: "st-a"}
	b := agentSessionRecord{SessionID: "SB", Agent: "claude-code", WorkingDir: wt, StageID: "st-b"}
	if err := writeAgentSessionRecord(path, "run-a", a); err != nil {
		t.Fatal(err)
	}
	if err := writeAgentSessionRecord(path, "run-b", b); err != nil {
		t.Fatal(err)
	}
	for id, want := range map[string]string{"run-a": "SA", "run-b": "SB"} {
		rec, err := lookupAgentSession(path, id)
		if err != nil || rec == nil || rec.SessionID != want {
			t.Errorf("lookup %s = %+v, %v; want session %s", id, rec, err, want)
		}
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"schema": "agent-sessions-v1"`) {
		t.Errorf("ledger missing schema tag:\n%s", raw)
	}
	if filepath.Dir(path) != wtDir || filepath.Base(filepath.Dir(path)) != "fishhawk-worktrees" {
		t.Errorf("ledger %s is not directly under fishhawk-worktrees/", path)
	}
	if rel, err := filepath.Rel(wt, path); err == nil && !strings.HasPrefix(rel, "..") {
		t.Errorf("ledger %s lies inside the worktree %s", path, wt)
	}
	if status := gitPorcelain(t, wt); status != "" {
		t.Errorf("ledger write dirtied the worktree:\n%s", status)
	}
	entries, _ := os.ReadDir(wtDir)
	for _, e := range entries {
		if strings.Contains(e.Name(), ".tmp-") {
			t.Errorf("atomic write left a temp file: %s", e.Name())
		}
	}

	for name, body := range map[string]string{
		"corrupt":        "{not json",
		"foreign schema": `{"schema":"agent-sessions-v9","runs":{}}`,
	} {
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := lookupAgentSession(path, "run-a"); err == nil {
			t.Errorf("%s ledger read without error", name)
		}
	}
	// A write over an unreadable ledger replaces it.
	if err := writeAgentSessionRecord(path, "run-c", a); err != nil {
		t.Fatal(err)
	}
	if rec, err := lookupAgentSession(path, "run-c"); err != nil || rec == nil {
		t.Errorf("write over an unreadable ledger: rec=%v err=%v", rec, err)
	}
	if _, err := lookupAgentSession("", "run-a"); !errors.Is(err, errNoSessionLedger) {
		t.Errorf("empty ledger path err = %v, want errNoSessionLedger", err)
	}
}

// B3. Only an OK primary with a session id is recorded, read back from disk.
func TestRecordAgentSession_SkipsFailedOrSessionless(t *testing.T) {
	cfg := &config{runID: "run-rec", stageID: "st-rec", agent: "claude-code", agentBinary: "/opt/claude"}
	inv := agent.Invocation{Model: "m1", WorkingDir: "/wt/run-rec"}

	t.Run("not recorded", func(t *testing.T) {
		for name, res := range map[string]agent.Result{
			"failed primary": {OK: false, SessionID: "S-dead"},
			"no session id":  {OK: true},
		} {
			dir := t.TempDir()
			path := filepath.Join(dir, "run-x.agent-sessions.json")
			prior := []byte(`{"schema":"agent-sessions-v1","runs":{}}`)
			if err := os.WriteFile(path, prior, 0o600); err != nil {
				t.Fatal(err)
			}
			r := res
			var log bytes.Buffer
			recordAgentSession(path, "implement", cfg, inv, &r, &log)
			got, _ := os.ReadFile(path)
			if !bytes.Equal(got, prior) {
				t.Errorf("%s: ledger changed:\n%s", name, got)
			}
			if countResumeKind(r.Events, agentSessionRecordedEvent) != 0 || log.Len() != 0 {
				t.Errorf("%s: recorded event/log emitted: %v %s", name, r.Events, log.String())
			}
		}
		// A non-implement stage never records.
		path := filepath.Join(t.TempDir(), "run-y.agent-sessions.json")
		r := agent.Result{OK: true, SessionID: "S1"}
		recordAgentSession(path, "plan", cfg, inv, &r, io.Discard)
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Errorf("plan stage wrote a ledger: %v", err)
		}
	})

	t.Run("ok primary recorded", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "run-x.agent-sessions.json")
		r := agent.Result{OK: true, SessionID: "S1", Model: "claude-reported"}
		var log bytes.Buffer
		recordAgentSession(path, "implement", cfg, inv, &r, &log)
		rec, err := lookupAgentSession(path, "run-rec")
		if err != nil || rec == nil {
			t.Fatalf("lookup: %v %v", rec, err)
		}
		want := agentSessionRecord{SessionID: "S1", Agent: "claude-code", AgentBinary: "/opt/claude",
			RequestedModel: "m1", ReportedModel: "claude-reported", WorkingDir: "/wt/run-rec", StageID: "st-rec"}
		rec.RecordedAt = time.Time{}
		if *rec != want {
			t.Errorf("record = %+v, want %+v", *rec, want)
		}
		if n := countResumeKind(r.Events, agentSessionRecordedEvent); n != 1 {
			t.Errorf("agent_session_recorded events = %d, want 1", n)
		}
		if !strings.Contains(log.String(), `"event":"agent_session_recorded"`) {
			t.Errorf("missing log line:\n%s", log.String())
		}
	})

	t.Run("unwritable dir degrades", func(t *testing.T) {
		if runtime.GOOS == "windows" || os.Geteuid() == 0 {
			t.Skip("needs POSIX permissions enforced on a non-root user")
		}
		dir := t.TempDir()
		if err := os.Chmod(dir, 0o500); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
		r := agent.Result{OK: true, SessionID: "S1"}
		var log bytes.Buffer
		recordAgentSession(filepath.Join(dir, "run-x.agent-sessions.json"), "implement", cfg, inv, &r, &log)
		if !strings.Contains(log.String(), `"event":"agent_session_ledger_degraded"`) {
			t.Errorf("missing agent_session_ledger_degraded line:\n%s", log.String())
		}
		if !r.OK || countResumeKind(r.Events, agentSessionRecordedEvent) != 0 {
			t.Errorf("degraded write changed the result: OK=%v events=%v", r.OK, r.Events)
		}
	})
}

func countResumeKind(evs []agent.Event, kind string) int {
	n := 0
	for _, e := range evs {
		if e.Kind == kind {
			n++
		}
	}
	return n
}

func resumeKindPayloads(t *testing.T, evs []agent.Event, kind string) []map[string]string {
	t.Helper()
	var out []map[string]string
	for _, e := range evs {
		if e.Kind != kind {
			continue
		}
		var p map[string]string
		if err := json.Unmarshal(e.Payload, &p); err != nil {
			t.Fatalf("decode %s: %v", kind, err)
		}
		out = append(out, p)
	}
	return out
}

// B4. A refused resume falls back to ONE fresh re-invoke of the original
// prompt; tokens are summed and only the fresh session id survives.
func TestInvokePrimaryAgent_ResumeFailedFallsBackFresh(t *testing.T) {
	fake := &fakeInvoker{
		cannedSeq: []agent.Result{
			// The refused attempt carries non-zero buckets (the real adapter
			// reports zero) so a dropped += is observable here.
			{OK: false, FailureCategory: "A", SessionID: "S-dead", TokensUsed: 3, InputTokens: 2, OutputTokens: 1,
				CacheReadInputTokens: 4, CacheWriteInputTokens: 5, Events: []agent.Event{{Kind: "invocation_start"}}},
			{OK: true, SessionID: "S2", Model: "claude-fresh", TokensUsed: 41000, InputTokens: 40000, OutputTokens: 1000,
				CacheReadInputTokens: 7, Events: []agent.Event{{Kind: "invocation_start"}}},
		},
		errSeq: []error{fmt.Errorf("%w: no such session", agent.ErrSessionResumeFailed), nil},
	}
	var calls []agent.Invocation
	fake.onInvoke = func(_ int, inv agent.Invocation) { calls = append(calls, inv) }
	cfg := &config{runID: "r", stageID: "s"}
	inv := agent.Invocation{Prompt: "FIXUP PROMPT"}
	var log bytes.Buffer
	res, err := invokePrimaryAgent(context.Background(), fake, inv,
		&fixupResumeDecision{resume: true, sessionID: "S-dead", reason: fixupResumeReasonResumed}, cfg, &log)
	if err != nil {
		t.Fatalf("err = %v, want nil (the fresh fallback succeeded)", err)
	}
	if len(calls) != 2 {
		t.Fatalf("Invoke calls = %d, want 2 (resume, then fresh)", len(calls))
	}
	if calls[0].ResumeSessionID != "S-dead" || calls[1].ResumeSessionID != "" {
		t.Errorf("resume ids = %q, %q; want S-dead then empty", calls[0].ResumeSessionID, calls[1].ResumeSessionID)
	}
	if calls[1].Prompt != "FIXUP PROMPT" {
		t.Errorf("fresh prompt = %q, want the original fix-up prompt", calls[1].Prompt)
	}
	if inv.ResumeSessionID != "" {
		t.Error("the caller's Invocation was mutated")
	}
	if res.SessionID != "S2" || res.Model != "claude-fresh" || !res.OK {
		t.Errorf("result session=%q model=%q ok=%v, want the fresh attempt's", res.SessionID, res.Model, res.OK)
	}
	if res.TokensUsed != 41003 || res.InputTokens != 40002 || res.OutputTokens != 1001 ||
		res.CacheReadInputTokens != 11 || res.CacheWriteInputTokens != 5 {
		t.Errorf("token buckets not summed across both attempts: used=%d in=%d out=%d cache_read=%d cache_write=%d",
			res.TokensUsed, res.InputTokens, res.OutputTokens, res.CacheReadInputTokens, res.CacheWriteInputTokens)
	}
	evs := resumeKindPayloads(t, res.Events, fixupSessionResumeEvent)
	if len(evs) != 1 || evs[0]["outcome"] != "fallback" || evs[0]["reason"] != "resume_failed" || evs[0]["session_id"] != "S-dead" {
		t.Errorf("fixup_session_resume events = %v, want one fallback/resume_failed", evs)
	}
	if n := countResumeKind(res.Events, "invocation_start"); n != 2 {
		t.Errorf("invocation_start events = %d, want both attempts' (2)", n)
	}
	if !strings.Contains(log.String(), `"reason":"resume_failed"`) {
		t.Errorf("missing resume_failed log line:\n%s", log.String())
	}
}

// The other invokePrimaryAgent branches: no decision (non-fix-up) emits
// nothing; a fallback decision invokes fresh and traces its reason; a resume
// whose spawn fails for any OTHER reason is NOT re-spawned.
func TestInvokePrimaryAgent_Branches(t *testing.T) {
	cfg := &config{runID: "r", stageID: "s"}
	inv := agent.Invocation{Prompt: "P"}

	fake := &fakeInvoker{canned: agent.Result{OK: true}}
	res, _ := invokePrimaryAgent(context.Background(), fake, inv, nil, cfg, io.Discard)
	if fake.callIdx != 1 || countResumeKind(res.Events, fixupSessionResumeEvent) != 0 || fake.gotInv.ResumeSessionID != "" {
		t.Errorf("nil decision: calls=%d events=%v", fake.callIdx, res.Events)
	}

	fake = &fakeInvoker{canned: agent.Result{OK: true}}
	res, _ = invokePrimaryAgent(context.Background(), fake, inv, &fixupResumeDecision{reason: fixupResumeReasonDisabled}, cfg, io.Discard)
	evs := resumeKindPayloads(t, res.Events, fixupSessionResumeEvent)
	if fake.gotInv.ResumeSessionID != "" || len(evs) != 1 || evs[0]["outcome"] != "fallback" || evs[0]["reason"] != "disabled" {
		t.Errorf("fallback decision: resume=%q events=%v", fake.gotInv.ResumeSessionID, evs)
	}

	fake = &fakeInvoker{canned: agent.Result{OK: false, FailureCategory: "A"}, returnErr: agent.ErrAgentQuotaUnavailable}
	res, err := invokePrimaryAgent(context.Background(), fake, inv, &fixupResumeDecision{resume: true, sessionID: "S1", reason: "resumed"}, cfg, io.Discard)
	evs = resumeKindPayloads(t, res.Events, fixupSessionResumeEvent)
	if fake.callIdx != 1 || !errors.Is(err, agent.ErrAgentQuotaUnavailable) {
		t.Errorf("non-resume failure: calls=%d err=%v, want 1 call and the quota error", fake.callIdx, err)
	}
	if len(evs) != 1 || evs[0]["outcome"] != "resumed" || evs[0]["session_id"] != "S1" {
		t.Errorf("resumed events = %v", evs)
	}
}

func TestPrimaryResumeDecision_FirstAttemptOnly(t *testing.T) {
	d := &fixupResumeDecision{resume: true, sessionID: "S1", reason: fixupResumeReasonResumed}
	if got := primaryResumeDecision(d, 0); got != d {
		t.Errorf("attempt 0 = %+v, want the decision", got)
	}
	if got := primaryResumeDecision(d, 1); got == nil || got.resume || got.reason != fixupResumeReasonSelfRetry {
		t.Errorf("attempt 1 = %+v, want fallback self_retry", got)
	}
	if got := primaryResumeDecision(nil, 1); got != nil {
		t.Errorf("nil decision on a retry = %+v, want nil", got)
	}
}

// B5. run() end to end: pass 1 (implement) records S1, pass 2 (fix-up)
// resumes it with the fetched fix-up prompt as the turn.
func TestRun_FixupResume_ResumesRecordedSession(t *testing.T) {
	implementEnv(t, "kuhlman-labs/fishhawk", "main")
	const runID = "40890b05-0000-4000-8000-000000000001"
	wt, ledger := cwdLineage(t, runID)

	pass1 := &fakeInvoker{canned: agent.Result{OK: true, SessionID: "S1"}}
	withFakeInvoker(t, pass1)
	fu := newFakeUploader(t)
	fu.promptResp = &upload.FetchedPrompt{StageID: fixupResumeStageID, StageType: "implement", Prompt: "IMPLEMENT", PromptHash: "h"}
	withFakeUploader(t, fu)
	withFakeGitOps(t, &fakePusher{}, &fakePROpener{})
	var stderr strings.Builder
	if got := run(fixupResumeArgs(runID), &stderr); got != exitOK {
		t.Fatalf("pass 1 run = %d:\n%s", got, stderr.String())
	}
	if pass1.gotInv.ResumeSessionID != "" {
		t.Errorf("a non-fix-up implement spawn carried --resume %q", pass1.gotInv.ResumeSessionID)
	}
	if strings.Contains(stderr.String(), `"event":"fixup_session_resume"`) {
		t.Errorf("a non-fix-up stage emitted fixup_session_resume:\n%s", stderr.String())
	}
	rec, err := lookupAgentSession(ledger, runID)
	if err != nil || rec == nil || rec.SessionID != "S1" || rec.WorkingDir != wt {
		t.Fatalf("pass 1 ledger record = %+v, %v; want S1 in %s", rec, err, wt)
	}

	pass2 := &fakeInvoker{canned: agent.Result{OK: true, SessionID: "S1"}}
	withFakeInvoker(t, pass2)
	fu2 := newFakeUploader(t)
	fu2.promptResp = fixupRespFor("FIXUP: address concern c1")
	withFakeUploader(t, fu2)
	stderr.Reset()
	if got := run(fixupResumeArgs(runID), &stderr); got != exitOK {
		t.Fatalf("pass 2 run = %d:\n%s", got, stderr.String())
	}
	if pass2.gotInv.ResumeSessionID != "S1" {
		t.Errorf("fix-up ResumeSessionID = %q, want S1 from the ledger", pass2.gotInv.ResumeSessionID)
	}
	if pass2.gotInv.Prompt != "FIXUP: address concern c1" {
		t.Errorf("fix-up turn = %q, want the fetched fix-up prompt", pass2.gotInv.Prompt)
	}
	_, lines := shippedRaw(t, fu2)
	evs := resumeEvents(t, lines)
	if len(evs) != 1 || evs[0]["outcome"] != "resumed" || evs[0]["session_id"] != "S1" {
		t.Errorf("bundle fixup_session_resume = %v, want one resumed S1", evs)
	}
}

// B6. A fix-up with no recorded session spawns fresh, traced in bundle + log.
func TestRun_FixupResume_NoLedger_FallsBackTraced(t *testing.T) {
	implementEnv(t, "kuhlman-labs/fishhawk", "main")
	const runID = "40890b06-0000-4000-8000-000000000001"
	cwdLineage(t, runID)
	fake := &fakeInvoker{canned: agent.Result{OK: true}}
	withFakeInvoker(t, fake)
	fu := newFakeUploader(t)
	fu.promptResp = fixupRespFor("FIXUP")
	withFakeUploader(t, fu)
	withFakeGitOps(t, &fakePusher{}, &fakePROpener{})
	var stderr strings.Builder
	if got := run(fixupResumeArgs(runID), &stderr); got != exitOK {
		t.Fatalf("run = %d:\n%s", got, stderr.String())
	}
	if fake.gotInv.ResumeSessionID != "" {
		t.Errorf("ResumeSessionID = %q, want empty", fake.gotInv.ResumeSessionID)
	}
	_, lines := shippedRaw(t, fu)
	evs := resumeEvents(t, lines)
	if len(evs) != 1 || evs[0]["outcome"] != "fallback" || evs[0]["reason"] != "no_session_recorded" {
		t.Errorf("bundle fixup_session_resume = %v, want one fallback/no_session_recorded", evs)
	}
	if !strings.Contains(stderr.String(), `"event":"fixup_session_resume"`) ||
		!strings.Contains(stderr.String(), `"reason":"no_session_recorded"`) {
		t.Errorf("missing fallback log line:\n%s", stderr.String())
	}
}

// B7. An ADR-023 self-retry after a failed resumed attempt spawns fresh.
func TestRun_FixupResume_SelfRetrySpawnsFresh(t *testing.T) {
	implementEnv(t, "kuhlman-labs/fishhawk", "main")
	const runID = "40890b07-0000-4000-8000-000000000001"
	wt, ledger := cwdLineage(t, runID)
	seedLedger(t, ledger, runID, wt, "S1")

	var resumeIDs []string
	fake := &fakeInvoker{
		cannedSeq: []agent.Result{
			{OK: false, FailureCategory: "A", FailureReason: "agent crash"},
			{OK: true},
		},
		errSeq:   []error{agent.ErrAgentFailed, nil},
		onInvoke: func(_ int, inv agent.Invocation) { resumeIDs = append(resumeIDs, inv.ResumeSessionID) },
	}
	withFakeInvoker(t, fake)
	fu := newFakeUploader(t)
	resp := fixupRespFor("FIXUP")
	resp.AgentSelfRetry = true
	resp.MaxRetriesSnapshot = 1
	fu.promptResp = resp
	withFakeUploader(t, fu)
	withFakeGitOps(t, &fakePusher{}, &fakePROpener{})
	var stderr strings.Builder
	if got := run(fixupResumeArgs(runID), &stderr); got != exitOK {
		t.Fatalf("run = %d:\n%s", got, stderr.String())
	}
	if len(resumeIDs) != 2 || resumeIDs[0] != "S1" || resumeIDs[1] != "" {
		t.Fatalf("resume ids per attempt = %q, want [S1, \"\"] (the self-retry spawns fresh)", resumeIDs)
	}
	_, lines := shippedRaw(t, fu)
	evs := resumeEvents(t, lines)
	if len(evs) != 1 || evs[0]["reason"] != fixupResumeReasonSelfRetry {
		t.Errorf("shipped (retry attempt) fixup_session_resume = %v, want fallback self_retry", evs)
	}
}

// B8. The verify-fix re-invoke copies the shared inv, so it stays fresh while
// the primary resumed.
func TestRun_FixupResume_VerifyFixReinvokeStaysFresh(t *testing.T) {
	repo := verifyFixBaseRepo(t)
	implementEnv(t, "kuhlman-labs/fishhawk", "main")
	ctx := context.Background()
	wtDir, err := worktreesDir(ctx, repo)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(wtDir, 0o755); err != nil {
		t.Fatal(err)
	}
	wt := filepath.Join(wtDir, "run-"+shortID(verifyFixRunID))
	seedLedger(t, agentSessionLedgerPath(wtDir, shortID(verifyFixRunID)), verifyFixRunID, wt, "S1")

	regPath := filepath.Join(repo, "mod", "reg.go")
	var resumeIDs []string
	invoker := &fakeInvoker{
		mirrorWorkingTreeFrom: repo,
		canned:                agent.Result{OK: true, Events: []agent.Event{{Kind: "invocation_start"}}},
		onInvoke: func(idx int, inv agent.Invocation) {
			resumeIDs = append(resumeIDs, inv.ResumeSessionID)
			if idx == 0 {
				mustWrite(t, regPath, regGetBuggy)
				mustWrite(t, filepath.Join(repo, "mod", "reg_test.go"), regGetTest)
			} else {
				mustWrite(t, regPath, regGetFixed)
			}
		},
	}
	withFakeInvoker(t, invoker)
	fu := newFakeUploader(t)
	resp := fixupVerifyLandingPrompt()
	resp.VerifyCommand = "cd mod && go test ./..."
	resp.VerifyMaxIterations = 2
	resp.ScopeFiles = []upload.ScopeFile{
		{Path: "mod/reg.go", Operation: "create"},
		{Path: "mod/reg_test.go", Operation: "create"},
	}
	fu.promptResp = resp
	withFakeUploader(t, fu)
	withFakeGitOps(t, &fakePusher{}, &fakePROpener{})

	bundlePath := filepath.Join(t.TempDir(), "trace.jsonl.gz")
	var stderr strings.Builder
	if got := run(verifyFixRunArgs(repo, bundlePath), &stderr); got != exitOK {
		t.Fatalf("run = %d:\n%s", got, stderr.String())
	}
	if len(resumeIDs) != 2 {
		t.Fatalf("Invoke calls = %d (%q), want 2 (primary + one verify-fix re-invoke)", len(resumeIDs), resumeIDs)
	}
	if resumeIDs[0] != "S1" {
		t.Errorf("primary ResumeSessionID = %q, want S1", resumeIDs[0])
	}
	if resumeIDs[1] != "" {
		t.Errorf("verify-fix re-invoke ResumeSessionID = %q, want empty (the shared inv must not be mutated)", resumeIDs[1])
	}
}

// B9. A swept or pruned lineage leaves no agent-session ledger behind.
func TestSweepTerminalWorktrees_RemovesAgentSessionLedger(t *testing.T) {
	cases := []struct {
		name, root, sidecar string
		client              *fakeLineageClient
	}{
		{name: "swept complete lineage", root: "led00001", sidecar: "1ed00000-0000-0000-0000-000000000001",
			client: &fakeLineageClient{complete: map[string]bool{"1ed00000-0000-0000-0000-000000000001": true}}},
		{name: "pruned non-run root", root: "led00002", sidecar: "rid", client: &fakeLineageClient{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			repo := initRepo(t)
			ctx := context.Background()
			if _, err := provisionLineageWorktree(ctx, repo, tc.root, "main", io.Discard); err != nil {
				t.Fatal(err)
			}
			writeLineageRunID(ctx, repo, tc.root, tc.sidecar, io.Discard)
			wtDir, _ := worktreesDir(ctx, repo)
			ledger := agentSessionLedgerPath(wtDir, tc.root)
			if err := writeAgentSessionRecord(ledger, tc.sidecar, agentSessionRecord{SessionID: "S1"}); err != nil {
				t.Fatal(err)
			}
			sweepTerminalWorktrees(ctx, repo, tc.client, io.Discard)
			if _, err := os.Stat(ledger); !os.IsNotExist(err) {
				t.Errorf("agent-session ledger survived the sweep: stat err = %v", err)
			}
		})
	}
}

// fakeClaudeReorientTokens models the ~50k-token re-orientation a fresh
// fix-up agent pays (#4089's observed cost) in the fake CLI's token model.
const fakeClaudeReorientTokens = 40000

// writeFakeClaude writes a fake `claude` POSIX sh script whose session store
// and call log are baked in (the writeEnvDumpAgent pattern) and uses only sh
// builtins, so it runs under the agent's default-deny env. Token model, fixed
// and named here: a FRESH spawn reports input = ceil(len(turn)/4) +
// fakeClaudeReorientTokens; a RESUMED spawn reports input = ceil(len(turn)/4)
// with cache_read_input_tokens = ceil(prior transcript bytes/4); output is
// 1000 either way. --resume of an unknown session prints the probed claude
// 2.1.305 not-found result line and stderr marker and exits 1.
func writeFakeClaude(t *testing.T) (bin, store, calls string) {
	t.Helper()
	dir := t.TempDir()
	store = filepath.Join(dir, "store")
	calls = filepath.Join(dir, "calls")
	for _, d := range []string{store, calls} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	notFound := `{"type":"result","subtype":"error_during_execution","duration_ms":0,"duration_api_ms":0,"is_error":true,"num_turns":0,"stop_reason":null,"session_id":"'"$resume"'","total_cost_usd":0,"usage":{"input_tokens":0,"cache_creation_input_tokens":0,"cache_read_input_tokens":0,"output_tokens":0},"modelUsage":{},"permission_denials":[],"errors":["No conversation found with session ID: '"$resume"'"]}`
	script := `#!/bin/sh
STORE='` + store + `'
CALLS='` + calls + `'
if [ "$1" = "--version" ]; then echo "2.1.305 (fishhawk fake)"; exit 0; fi
resume=""
prompt=""
while [ $# -gt 0 ]; do
  case "$1" in
    --resume) resume="$2"; shift 2 ;;
    -p) prompt="$2"; shift 2 ;;
    *) shift ;;
  esac
done
n=0
if [ -f "$CALLS/count" ]; then read n < "$CALLS/count"; fi
n=$((n + 1))
echo "$n" > "$CALLS/count"
printf '%s' "$resume" > "$CALLS/$n.resume"
printf '%s' "$prompt" > "$CALLS/$n.prompt"
plen=${#prompt}
turn=$(( (plen + 3) / 4 ))
if [ -n "$resume" ]; then
  if [ ! -f "$STORE/$resume.len" ]; then
    printf '%s\n' '` + notFound + `'
    printf '%s\n' "No conversation found with session ID: $resume" >&2
    exit 1
  fi
  sid="$resume"
  read prior < "$STORE/$sid.len"
  input=$turn
  cache=$(( (prior + 3) / 4 ))
else
  sid="fake-session-$n"
  prior=0
  input=$(( turn + ` + strconv.Itoa(fakeClaudeReorientTokens) + ` ))
  cache=0
fi
printf '%s' "$prompt" >> "$STORE/$sid.jsonl"
echo $(( prior + plen )) > "$STORE/$sid.len"
printf '%s\n' '{"type":"system","subtype":"init","session_id":"'"$sid"'"}'
printf '%s\n' '{"type":"result","subtype":"success","is_error":false,"num_turns":1,"session_id":"'"$sid"'","model":"claude-fake-1","result":"done","usage":{"input_tokens":'"$input"',"output_tokens":1000,"cache_read_input_tokens":'"$cache"',"cache_creation_input_tokens":0}}'
exit 0
`
	bin = filepath.Join(dir, "claude")
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil { //nolint:gosec // test fixture must be executable
		t.Fatalf("write fake claude: %v", err)
	}
	return bin, store, calls
}

// fakeClaudeCall is one recorded spawn of the fake CLI.
type fakeClaudeCall struct{ resume, prompt string }

func fakeClaudeCalls(t *testing.T, calls string) []fakeClaudeCall {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(calls, "count"))
	if err != nil {
		t.Fatalf("the fake claude never ran: %v", err)
	}
	n, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		t.Fatal(err)
	}
	out := make([]fakeClaudeCall, 0, n)
	for i := 1; i <= n; i++ {
		r, _ := os.ReadFile(filepath.Join(calls, strconv.Itoa(i)+".resume"))
		p, _ := os.ReadFile(filepath.Join(calls, strconv.Itoa(i)+".prompt"))
		out = append(out, fakeClaudeCall{resume: string(r), prompt: string(p)})
	}
	return out
}

// runnerCompletedTokens reads tokens_used off the runner_completed log line.
func runnerCompletedTokens(t *testing.T, log string) int {
	t.Helper()
	for _, line := range strings.Split(log, "\n") {
		if !strings.Contains(line, `"event":"runner_completed"`) {
			continue
		}
		var ev struct {
			Outcome    string `json:"outcome"`
			TokensUsed int    `json:"tokens_used"`
		}
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			t.Fatalf("parse runner_completed: %v\n%s", err, line)
		}
		if ev.Outcome != "ok" {
			t.Fatalf("runner_completed outcome = %q:\n%s", ev.Outcome, log)
		}
		return ev.TokensUsed
	}
	t.Fatalf("no runner_completed line:\n%s", log)
	return 0
}

// fixupHarnessArm is one arm's measured fix-up.
type fixupHarnessArm struct {
	calls          []fakeClaudeCall
	pass1Session   string
	completed      int
	manifest       bundle.ManifestData
	resumeEvents   []map[string]string
	fixupCallCount int
}

const (
	harnessImplementPrompt = "IMPLEMENT-PROMPT-MARKER: build the whole feature described in the issue, with tests and docs."
	harnessFixupPrompt     = "FIXUP: address review concern c1 (one line in pkg/a.go); prior diff attached; branch at tip."
)

// runFixupHarnessArm drives run() twice against the REAL claudecode adapter
// spawning the fake `claude`: pass 1 an implement stage, pass 2 a one-concern
// fix-up. betweenPasses runs after pass 1 (Arm X deletes the stored session).
func runFixupHarnessArm(t *testing.T, runID string, betweenPasses func(store, sessionID string)) fixupHarnessArm {
	t.Helper()
	bin, store, calls := writeFakeClaude(t)
	t.Setenv("FISHHAWK_AGENT_BIN", bin)
	_, ledger := cwdLineage(t, runID)
	withFakeGitOps(t, &fakePusher{}, &fakePROpener{})

	fu := newFakeUploader(t)
	fu.promptResp = &upload.FetchedPrompt{StageID: fixupResumeStageID, StageType: "implement", Prompt: harnessImplementPrompt, PromptHash: "h"}
	withFakeUploader(t, fu)
	var stderr strings.Builder
	if got := run(fixupResumeArgs(runID), &stderr); got != exitOK {
		t.Fatalf("pass 1 run = %d:\n%s", got, stderr.String())
	}
	rec, err := lookupAgentSession(ledger, runID)
	if err != nil || rec == nil || rec.SessionID == "" {
		t.Fatalf("pass 1 recorded no session: %+v %v\n%s", rec, err, stderr.String())
	}
	pass1Calls := len(fakeClaudeCalls(t, calls))
	if betweenPasses != nil {
		betweenPasses(store, rec.SessionID)
	}

	fu2 := newFakeUploader(t)
	fu2.promptResp = fixupRespFor(harnessFixupPrompt)
	withFakeUploader(t, fu2)
	stderr.Reset()
	if got := run(fixupResumeArgs(runID), &stderr); got != exitOK {
		t.Fatalf("pass 2 (fix-up) run = %d:\n%s", got, stderr.String())
	}
	m, lines := shippedRaw(t, fu2)
	all := fakeClaudeCalls(t, calls)
	return fixupHarnessArm{
		calls:          all,
		pass1Session:   rec.SessionID,
		completed:      runnerCompletedTokens(t, stderr.String()),
		manifest:       m,
		resumeEvents:   resumeEvents(t, lines),
		fixupCallCount: len(all) - pass1Calls,
	}
}

// B10. The cross-boundary measurement harness: real claudecode adapter, a
// real spawned fake `claude`, the on-disk ledger, run() end to end and the
// shipped bundle manifest. The 3x is a property of the fake's NAMED token
// model (writeFakeClaude): it proves the mechanism and the runner's
// bookkeeping, not real-world dollars, which the live operator walk measures.
func TestFixupResume_TokenCostHarness_ThreeXCheaper(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the fake claude is a /bin/sh script")
	}
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	implementEnv(t, "kuhlman-labs/fishhawk", "main")

	// Arm R: resume.
	armR := runFixupHarnessArm(t, "4089b10a-0000-4000-8000-000000000001", nil)
	if armR.fixupCallCount != 1 {
		t.Fatalf("arm R fix-up spawns = %d, want 1", armR.fixupCallCount)
	}
	fix := armR.calls[len(armR.calls)-1]
	if fix.resume != armR.pass1Session || fix.resume == "" {
		t.Errorf("arm R fix-up --resume %q, want the pass-1 session %q the fake minted", fix.resume, armR.pass1Session)
	}
	if fix.prompt != harnessFixupPrompt {
		t.Errorf("arm R fix-up turn = %q, want the fetched fix-up prompt byte for byte", fix.prompt)
	}
	if strings.Contains(fix.prompt, "IMPLEMENT-PROMPT-MARKER") {
		t.Error("arm R fix-up re-sent the implement prompt")
	}
	if armR.manifest.CacheReadInputTokens <= 0 {
		t.Errorf("arm R manifest cache_read_input_tokens = %d, want > 0 (the carried context must stay visible)", armR.manifest.CacheReadInputTokens)
	}
	if len(armR.resumeEvents) != 1 || armR.resumeEvents[0]["outcome"] != "resumed" {
		t.Errorf("arm R fixup_session_resume = %v, want one resumed", armR.resumeEvents)
	}

	// Arm F: the identical scenario with the kill switch off.
	orig := fixupResumeGetenv
	t.Cleanup(func() { fixupResumeGetenv = orig })
	fixupResumeGetenv = func(k string) string {
		if k == fixupResumeEnvVar {
			return "off"
		}
		return os.Getenv(k)
	}
	armF := runFixupHarnessArm(t, "4089b10b-0000-4000-8000-000000000001", nil)
	fixupResumeGetenv = orig
	if fixF := armF.calls[len(armF.calls)-1]; fixF.resume != "" || fixF.prompt != harnessFixupPrompt {
		t.Errorf("arm F fix-up spawn = %+v, want fresh with the fix-up prompt", fixF)
	}
	if len(armF.resumeEvents) != 1 || armF.resumeEvents[0]["reason"] != "disabled" {
		t.Errorf("arm F fixup_session_resume = %v, want fallback disabled", armF.resumeEvents)
	}

	resumedUsage := armR.manifest.InputTokens + armR.manifest.OutputTokens
	freshUsage := armF.manifest.InputTokens + armF.manifest.OutputTokens
	if armR.completed != resumedUsage || armF.completed != freshUsage {
		t.Errorf("runner_completed tokens (R=%d F=%d) disagree with the manifests (R=%d F=%d)",
			armR.completed, armF.completed, resumedUsage, freshUsage)
	}
	if freshUsage < 3*resumedUsage {
		t.Errorf("fresh fix-up usage %d < 3 x resumed %d", freshUsage, resumedUsage)
	}
	t.Logf("harness token model: resumed fix-up = %d (cache_read %d), fresh fix-up = %d (%.1fx)",
		resumedUsage, armR.manifest.CacheReadInputTokens, freshUsage, float64(freshUsage)/float64(resumedUsage))

	// Arm X: the stored session expired between passes.
	armX := runFixupHarnessArm(t, "4089b10c-0000-4000-8000-000000000001", func(store, sid string) {
		_ = os.Remove(filepath.Join(store, sid+".len"))
		_ = os.Remove(filepath.Join(store, sid+".jsonl"))
	})
	if armX.fixupCallCount != 2 {
		t.Fatalf("arm X fix-up spawns = %d, want 2 (refused resume, then fresh)", armX.fixupCallCount)
	}
	if x := armX.calls[len(armX.calls)-2]; x.resume != armX.pass1Session {
		t.Errorf("arm X first fix-up spawn --resume %q, want %q", x.resume, armX.pass1Session)
	}
	if x := armX.calls[len(armX.calls)-1]; x.resume != "" || x.prompt != harnessFixupPrompt {
		t.Errorf("arm X fallback spawn = %+v, want fresh with the fix-up prompt", x)
	}
	if len(armX.resumeEvents) != 1 || armX.resumeEvents[0]["outcome"] != "fallback" || armX.resumeEvents[0]["reason"] != "resume_failed" {
		t.Errorf("arm X fixup_session_resume = %v, want fallback resume_failed", armX.resumeEvents)
	}
	if xUsage := armX.manifest.InputTokens + armX.manifest.OutputTokens; xUsage != freshUsage || armX.completed != freshUsage {
		t.Errorf("arm X usage manifest=%d completed=%d, want arm F's %d plus the zero-token refused attempt", xUsage, armX.completed, freshUsage)
	}
}
