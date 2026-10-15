package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kuhlman-labs/fishhawk/runner/internal/gateiso"
	"github.com/kuhlman-labs/fishhawk/runner/internal/upload"
)

// Tests for the `fishhawk-runner specialist-job` subcommand (E85.9 / #4300).
// Tests that swap the package-level sjDeps or call t.Setenv are NOT parallel;
// every parallel test builds its specialistJobRun directly.

const sjTestJobID = "7d1f4c2a-1b2c-4d3e-8f90-0a1b2c3d4e5f"

// newTestSJRun builds a run over source with the REAL materialize and gate
// exec seams (guarded exactly as runSpecialistJobKind guards them).
func newTestSJRun(t *testing.T, source, kind, head string, inputs any) *specialistJobRun {
	t.Helper()
	raw, err := json.Marshal(inputs)
	if err != nil {
		t.Fatal(err)
	}
	x := newTestCFExecutor(t, source)
	x.materialize = guardScratchMaterialize(x.materialize)
	parent := t.TempDir()
	return &specialistJobRun{
		job:    &upload.SpecialistJob{JobID: sjTestJobID, Kind: kind, Subject: upload.SpecialistJobSubject{HeadSHA: head}, Inputs: raw},
		source: source, parent: parent, x: x,
		git:     &jobGit{parent: parent, run: hostJobGitRun, tainted: map[string]bool{}},
		logSink: io.Discard,
	}
}

// recordingExec is a gate exec seam that records every invocation and never
// runs anything.
type recordingExec struct {
	mu    sync.Mutex
	calls []recordedExec
	out   string
	code  int
}

type recordedExec struct {
	argv []string
	dir  string
}

func (r *recordingExec) exec(_ context.Context, argv []string, dir, _ string, _ time.Duration) (string, int, gateDisposition) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, recordedExec{argv: append([]string(nil), argv...), dir: dir})
	return r.out, r.code, gateExecuted
}

func (r *recordingExec) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.calls)
}

// TestSpecialistJob_RefusesNonScratchExecutionDir: a materialize seam that
// hands back the SOURCE worktree is refused errSpecialistJobNotScratch, and
// neither the inventory nor the battery executor runs anything there.
func TestSpecialistJob_RefusesNonScratchExecutionDir(t *testing.T) {
	t.Parallel()
	repo, head, _ := newCFFixture(t)
	rec := &recordingExec{}
	r := newTestSJRun(t, repo, "inventory", head, map[string]any{"command": "go_list", "args": map[string]any{"packages": []string{"."}}})
	r.x.exec = rec.exec
	r.x.materialize = guardScratchMaterialize(func(context.Context, string, string, string) (string, error) { return repo, nil })

	if _, err := r.freshClone(context.Background(), head); !errors.Is(err, errSpecialistJobNotScratch) {
		t.Fatalf("freshClone err = %v, want errSpecialistJobNotScratch", err)
	}
	if o := executeInventoryJob(context.Background(), r); o.Verdict != sjVerdictInconclusive || o.Reason != sjReasonMaterializeFailed {
		t.Fatalf("inventory outcome = %+v, want inconclusive/materialize_failed", o)
	}
	r.job.Kind = "counterfactual_battery"
	r.job.Inputs, _ = json.Marshal(map[string]any{"mutations": []any{batteryMutation("ctl.go", pinnedFind, "return n >= 0", "^TestAllowed$")}})
	o := executeBatteryJob(context.Background(), r)
	ev := o.Evidence.(sjBatteryEvidence)
	if ev.Mutations[0].Observed != cfOutcomeError || ev.Mutations[0].Reason != cfReasonCloneFailed {
		t.Fatalf("battery mutation = %+v, want error/clone_failed", ev.Mutations[0])
	}
	if n := rec.count(); n != 0 {
		t.Fatalf("exec seam ran %d times (first in %q), want 0 — a job executed outside its scratch clone", n, rec.calls[0].dir)
	}
	if err := requireScratchDir(r.parent, r.parent); !errors.Is(err, errSpecialistJobNotScratch) {
		t.Fatalf("parent itself err = %v, want errSpecialistJobNotScratch", err)
	}
	if err := requireScratchDir(filepath.Join(r.parent, "missing"), r.parent); !errors.Is(err, errSpecialistJobNotScratch) {
		t.Fatalf("missing dir err = %v, want errSpecialistJobNotScratch", err)
	}
}

// TestSpecialistJobGit_RefusesPush: a scratch clone WITH an origin cannot
// push to it, cannot rewrite its remotes, and git refuses a dir outside the
// scratch parent; the bare repo's refs are unchanged.
func TestSpecialistJobGit_RefusesPush(t *testing.T) {
	t.Parallel()
	repo, _, _ := newCFFixture(t)
	bare := t.TempDir()
	cfGit(t, bare, "init", "-q", "--bare")
	cfGit(t, repo, "push", "-q", bare, "HEAD:refs/heads/main")
	before := cfGit(t, bare, "for-each-ref")
	parent := t.TempDir()
	clone := filepath.Join(parent, "tree")
	cfGit(t, parent, "clone", "-q", bare, clone)
	cfGit(t, clone, "commit", "-q", "--allow-empty", "-m", "scratch")
	g := &jobGit{parent: parent, run: hostJobGitRun, tainted: map[string]bool{}}
	ctx := context.Background()
	for _, args := range [][]string{
		{"push", "origin", "HEAD:refs/heads/evil"},
		{"send-pack", bare, "HEAD:refs/heads/evil"},
		{"remote", "add", "evil", bare},
		{"remote", "set-url", "origin", bare},
		{"-C", repo, "status"},
	} {
		if _, _, err := g.Run(ctx, clone, args...); !errors.Is(err, errSpecialistJobWriteRefused) {
			t.Fatalf("git %v err = %v, want errSpecialistJobWriteRefused", args, err)
		}
	}
	if after := cfGit(t, bare, "for-each-ref"); after != before {
		t.Fatalf("bare refs changed:\nbefore %s\nafter  %s", before, after)
	}
	if _, _, err := g.Run(ctx, repo, "status"); !errors.Is(err, errSpecialistJobNotScratch) {
		t.Fatalf("git in source err = %v, want errSpecialistJobNotScratch", err)
	}
	if out, code, err := g.Run(ctx, clone, "rev-parse", "HEAD"); err != nil || code != 0 || len(strings.TrimSpace(out)) != 40 {
		t.Fatalf("read-only git refused: out=%q code=%d err=%v", out, code, err)
	}
}

// TestExecInClone_TaintsCloneForGit (C3): once a clone executed repository
// code, jobGit refuses it.
func TestExecInClone_TaintsCloneForGit(t *testing.T) {
	t.Parallel()
	repo, head, _ := newCFFixture(t)
	r := newTestSJRun(t, repo, "inventory", head, map[string]any{})
	rec := &recordingExec{}
	r.x.exec = rec.exec
	clone, err := r.freshClone(context.Background(), head)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := r.git.Run(context.Background(), clone, "status"); err != nil {
		t.Fatalf("git before execution refused: %v", err)
	}
	if _, _, _, err := r.execInClone(context.Background(), clone, clone, []string{"go", "list", "."}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := r.git.Run(context.Background(), clone, "status"); !errors.Is(err, errSpecialistJobCloneTainted) {
		t.Fatalf("git after execution err = %v, want errSpecialistJobCloneTainted", err)
	}
	if _, _, _, err := r.execInClone(context.Background(), repo, repo, []string{"go", "list", "."}); !errors.Is(err, errSpecialistJobNotScratch) {
		t.Fatalf("execInClone in source err = %v, want errSpecialistJobNotScratch", err)
	}
	if rec.count() != 1 {
		t.Fatalf("exec calls = %d, want 1", rec.count())
	}
}

// TestSpecialistJobTransport_RefusesRunStateRoutes: the job client reaches
// only this job's three routes; everything else is refused before I/O.
func TestSpecialistJobTransport_RefusesRunStateRoutes(t *testing.T) {
	t.Parallel()
	var mu sync.Mutex
	var seen []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = append(seen, r.Method+" "+r.URL.Path)
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)
	c, err := newSpecialistJobClient(srv.URL, sjTestJobID, http.DefaultTransport)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ method, path string }{
		{http.MethodPost, "/v0/runs/" + sjTestJobID + "/cancel"},
		{http.MethodPost, "/v0/stages/abc/reap-failure"},
		{http.MethodGet, "/v0/specialist-jobs/other-job"},
		{http.MethodPost, "/v0/specialist-jobs/" + sjTestJobID},
		{http.MethodDelete, "/v0/specialist-jobs/" + sjTestJobID},
		{http.MethodGet, "/v0/specialist-jobs/" + sjTestJobID + "?x=1"},
	} {
		req, _ := http.NewRequest(tc.method, srv.URL+tc.path, strings.NewReader("{}"))
		if _, err := c.HTTP.Do(req); !errors.Is(err, errSpecialistJobRouteRefused) {
			t.Fatalf("%s %s err = %v, want errSpecialistJobRouteRefused", tc.method, tc.path, err)
		}
	}
	mu.Lock()
	n := len(seen)
	mu.Unlock()
	if n != 0 {
		t.Fatalf("backend saw %d refused requests: %v", n, seen)
	}
	for _, tc := range []struct{ method, path string }{
		{http.MethodGet, ""}, {http.MethodPost, "/admission"}, {http.MethodPost, "/result"},
	} {
		req, _ := http.NewRequest(tc.method, srv.URL+"/v0/specialist-jobs/"+sjTestJobID+tc.path, nil)
		resp, err := c.HTTP.Do(req)
		if err != nil {
			t.Fatalf("allowed %s %s refused: %v", tc.method, tc.path, err)
		}
		_ = resp.Body.Close()
	}
	if _, err := newSpecialistJobClient("not a url", sjTestJobID, http.DefaultTransport); err == nil {
		t.Fatal("bad backend url accepted")
	}
}

// fakeSJBackend is an httptest specialist-job backend.
type fakeSJBackend struct {
	mu          sync.Mutex
	job         upload.SpecialistJob
	queuedFor   int
	nonce       string
	admissions  int
	heartbeats  int
	heartbeatCh chan struct{}
	results     [][]byte
	other       []string
	// resultConflict answers the result POST 409 specialist_job_not_running
	// and moves the job to stateAfterConflict (an ambiguous retry).
	resultConflict     bool
	stateAfterConflict string
	// attestKey is delivered as attestation_key on the admitting answer only
	// (never on a heartbeat), as the server does for an attester.
	attestKey string
	// resultRefusal, when set, answers every result POST 401 with this body.
	resultRefusal string
	auths         []string
	resultSigs    []string
}

func newFakeSJBackend(t *testing.T, job upload.SpecialistJob) (*fakeSJBackend, *httptest.Server) {
	t.Helper()
	b := &fakeSJBackend{job: job, heartbeatCh: make(chan struct{})}
	srv := httptest.NewServer(http.HandlerFunc(b.serve))
	t.Cleanup(srv.Close)
	return b, srv
}

func (b *fakeSJBackend) serve(w http.ResponseWriter, r *http.Request) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.auths = append(b.auths, r.Header.Get("Authorization"))
	base := "/v0/specialist-jobs/" + b.job.JobID
	writeJSON := func(v any) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(v)
	}
	switch {
	case r.Method == http.MethodGet && r.URL.Path == base:
		writeJSON(b.job)
	case r.Method == http.MethodPost && r.URL.Path == base+"/admission":
		var in upload.SpecialistJobAdmissionRequest
		_ = json.NewDecoder(r.Body).Decode(&in)
		b.admissions++
		switch {
		case b.nonce == "" && b.queuedFor > 0:
			b.queuedFor--
			writeJSON(upload.SpecialistJobAdmission{Position: 1, Contended: true, Job: b.job})
			return
		case b.nonce == "":
			b.nonce = in.Nonce
			writeJSON(upload.SpecialistJobAdmission{Admitted: true, ExecutionNonce: in.Nonce, Job: b.job, AttestationKey: b.attestKey})
			return
		case in.Nonce == b.nonce:
			b.heartbeats++
			if b.heartbeats == 1 {
				close(b.heartbeatCh)
			}
		default:
			w.WriteHeader(http.StatusConflict)
			_, _ = io.WriteString(w, `{"error":{"code":"specialist_job_already_running"}}`)
			return
		}
		writeJSON(upload.SpecialistJobAdmission{Admitted: true, ExecutionNonce: in.Nonce, Job: b.job})
	case r.Method == http.MethodPost && r.URL.Path == base+"/result":
		raw, _ := io.ReadAll(r.Body)
		b.results = append(b.results, raw)
		b.resultSigs = append(b.resultSigs, r.Header.Get("X-Fishhawk-Signature"))
		if b.resultRefusal != "" {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = io.WriteString(w, b.resultRefusal)
			return
		}
		if b.resultConflict {
			b.job.State = b.stateAfterConflict
			w.WriteHeader(http.StatusConflict)
			_, _ = io.WriteString(w, `{"error":{"code":"specialist_job_not_running"}}`)
			return
		}
		writeJSON(b.job)
	default:
		b.other = append(b.other, r.Method+" "+r.URL.Path)
		w.WriteHeader(http.StatusNotFound)
	}
}

func (b *fakeSJBackend) snapshot() (admissions, heartbeats int, results [][]byte, nonce string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.admissions, b.heartbeats, append([][]byte(nil), b.results...), b.nonce
}

// useTestSJDeps swaps sjDeps for fast test bounds (NOT parallel-safe).
func useTestSJDeps(t *testing.T, mut func(*specialistJobDeps)) {
	t.Helper()
	prev := sjDeps
	d := defaultSpecialistJobDeps()
	d.admitPoll = time.Millisecond
	d.heartbeat = 5 * time.Millisecond
	d.newExecutor = func(ctx context.Context, source string) *counterfactualExecutor {
		x := newCounterfactualExecutor(ctx, source)
		x.reacquireDelay = 10 * time.Millisecond
		x.runTimeout = 2 * time.Minute
		return x
	}
	if mut != nil {
		mut(&d)
	}
	sjDeps = d
	t.Cleanup(func() { sjDeps = prev })
	t.Setenv("FISHHAWK_API_TOKEN", "test-operator-token")
	t.Setenv("FISHHAWK_RUNNER_ATTEST_TOKEN", "")
}

func runSJSubcommand(srvURL, workingDir string) (int, string) {
	var log bytes.Buffer
	code := run([]string{"specialist-job", "--job-id", sjTestJobID, "--backend-url", srvURL, "--working-dir", workingDir, "--host", "test-host"}, &log)
	return code, log.String()
}

// decodeShippedCompletion strictly decodes one shipped result body.
func decodeShippedCompletion(t *testing.T, raw []byte) upload.SpecialistJobCompletion {
	t.Helper()
	var c upload.SpecialistJobCompletion
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&c); err != nil {
		t.Fatalf("shipped completion does not decode strictly: %v\n%s", err, raw)
	}
	return c
}

func TestSpecialistJob_DraftNotRunnable(t *testing.T) {
	useTestSJDeps(t, nil)
	b, srv := newFakeSJBackend(t, upload.SpecialistJob{JobID: sjTestJobID, Kind: "draft", State: "completed"})
	code, log := runSJSubcommand(srv.URL, t.TempDir())
	if code != exitFailure || !strings.Contains(log, errSpecialistJobNotRunnable.Error()) {
		t.Fatalf("exit %d, log %s; want exitFailure naming errSpecialistJobNotRunnable", code, log)
	}
	if adm, _, results, _ := b.snapshot(); adm != 0 || len(results) != 0 {
		t.Fatalf("draft job reached admission (%d) or shipped (%d)", adm, len(results))
	}
}

// TestSpecialistJob_AdmitWaitThenExecute: queued twice, then admitted; a
// same-nonce heartbeat lands while the executor runs; one completion ships
// under the admitted nonce.
func TestSpecialistJob_AdmitWaitThenExecute(t *testing.T) {
	useTestSJDeps(t, nil)
	b, srv := newFakeSJBackend(t, upload.SpecialistJob{JobID: sjTestJobID, Kind: "sj_test_kind", State: "queued", Inputs: json.RawMessage(`{}`)})
	b.queuedFor = 2
	specialistJobExecutors["sj_test_kind"] = func(ctx context.Context, r *specialistJobRun) specialistJobOutcome {
		select {
		case <-b.heartbeatCh:
		case <-time.After(30 * time.Second):
			return inconclusive(sjReasonTimeout)
		}
		return specialistJobOutcome{Verdict: sjVerdictPass, Evidence: map[string]any{"ok": true}}
	}
	t.Cleanup(func() { delete(specialistJobExecutors, "sj_test_kind") })
	code, log := runSJSubcommand(srv.URL, t.TempDir())
	if code != exitOK {
		t.Fatalf("exit %d, log %s", code, log)
	}
	adm, hb, results, nonce := b.snapshot()
	if adm < 4 || hb < 1 {
		t.Fatalf("admissions=%d heartbeats=%d, want 2 queued + 1 admitted + >=1 heartbeat", adm, hb)
	}
	if len(results) != 1 {
		t.Fatalf("results shipped = %d, want 1", len(results))
	}
	c := decodeShippedCompletion(t, results[0])
	if c.ExecutionNonce != nonce || c.Verdict != sjVerdictPass || c.SchemaVersion != specialistJobSchemaVersion || c.Kind != "sj_test_kind" {
		t.Fatalf("completion = %+v, want pass under nonce %q", c, nonce)
	}
}

// TestSpecialistJob_AdmissionTimeoutLeavesQueued: never admitted within the
// wait → exit non-zero, nothing shipped.
func TestSpecialistJob_AdmissionTimeoutLeavesQueued(t *testing.T) {
	useTestSJDeps(t, func(d *specialistJobDeps) { d.admitWait = 20 * time.Millisecond })
	b, srv := newFakeSJBackend(t, upload.SpecialistJob{JobID: sjTestJobID, Kind: "inventory", State: "queued"})
	b.queuedFor = 1 << 30
	code, log := runSJSubcommand(srv.URL, t.TempDir())
	if code != exitFailure || !strings.Contains(log, errSpecialistJobAdmissionTimeout.Error()) {
		t.Fatalf("exit %d, log %s; want exitFailure naming the admission timeout", code, log)
	}
	if _, _, results, _ := b.snapshot(); len(results) != 0 {
		t.Fatalf("shipped %d results without admission", len(results))
	}
}

func TestSpecialistJob_UsageErrors(t *testing.T) {
	t.Setenv("FISHHAWK_API_TOKEN", "")
	t.Setenv("FISHHAWK_RUNNER_ATTEST_TOKEN", "")
	for _, args := range [][]string{
		{"specialist-job"},
		{"specialist-job", "--job-id", "j", "--backend-url", "http://x", "--working-dir", "rel", "--host", "h"},
		{"specialist-job", "--job-id", "j", "--backend-url", "http://x", "--working-dir", "/abs", "--host", "h"},
		{"specialist-job", "--bogus"},
	} {
		if code := run(args, io.Discard); code != exitUsage {
			t.Fatalf("%v exit %d, want exitUsage", args, code)
		}
	}
}

func TestSpecialistJob_UnregisteredKindInconclusive(t *testing.T) {
	t.Parallel()
	o := runSpecialistJobKind(context.Background(), &upload.SpecialistJob{Kind: "sj_never_registered"},
		specialistJobFlags{workingDir: t.TempDir()}, defaultSpecialistJobDeps(), io.Discard)
	if o.Verdict != sjVerdictInconclusive || o.Reason != sjReasonKindNotExecutable || o.Evidence != nil {
		t.Fatalf("outcome = %+v, want inconclusive/kind_not_executable with no evidence", o)
	}
	c, err := specialistJobCompletion("sj_never_registered", "n", o, time.Second)
	if err != nil || c.Evidence != nil || c.Reason != sjReasonKindNotExecutable {
		t.Fatalf("completion = %+v err %v", c, err)
	}
}

func batteryMutation(control, find, replace, run string) map[string]any {
	return map[string]any{
		"control_path": control,
		"mutation":     map[string]any{"find": find, "replace": replace},
		"tests":        map[string]any{"run": run, "package": "."},
		"expect":       "red",
	}
}

// TestBattery_RedGreenErrorVerdicts drives REAL go test through the E85.3
// executor: a pinned control observes red, an unpinned one green, a broken
// baseline error; each entry carries counterfactualMutationSHA256.
func TestBattery_RedGreenErrorVerdicts(t *testing.T) {
	t.Parallel()
	repo, head, _ := newCFFixture(t)
	red := batteryMutation("ctl.go", pinnedFind, "return n >= 0", "^TestAllowed$")
	green := batteryMutation("unpinned.go", "return n > 1", "return n > 2", "^TestAllowed$")
	broken := batteryMutation("ctl.go", pinnedFind, "return n >= 0", "^TestBroken$")
	for _, tc := range []struct {
		name         string
		muts         []any
		observed     []string
		verdict, why string
	}{
		{"red_only_passes", []any{red}, []string{cfOutcomeRed}, sjVerdictPass, ""},
		{"green_fails", []any{red, green}, []string{cfOutcomeRed, cfOutcomeGreen}, sjVerdictFail, ""},
		{"baseline_error_inconclusive", []any{red, broken}, []string{cfOutcomeRed, cfOutcomeError}, sjVerdictInconclusive, sjReasonExecutionError},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := newTestSJRun(t, repo, "counterfactual_battery", head, map[string]any{"mutations": tc.muts})
			o := executeBatteryJob(context.Background(), r)
			if o.Verdict != tc.verdict || o.Reason != tc.why {
				t.Fatalf("verdict = %s/%s, want %s/%s (%+v)", o.Verdict, o.Reason, tc.verdict, tc.why, o.Evidence)
			}
			ev := o.Evidence.(sjBatteryEvidence)
			for i, want := range tc.observed {
				m := ev.Mutations[i]
				if m.Observed != want {
					t.Fatalf("mutation %d observed %q (baseline %q reason %q), want %q", i, m.Observed, m.Baseline, m.Reason, want)
				}
				var in sjBatteryInputs
				_ = decodeJobInputs(r.job.Inputs, &in)
				if wantSum := counterfactualMutationSHA256(batterySpec(i, in.Mutations[i])); m.MutationSHA256 != wantSum || len(wantSum) != 64 {
					t.Fatalf("mutation %d sha %q, want %q", i, m.MutationSHA256, wantSum)
				}
			}
		})
	}
}

func TestBatteryVerdict_Table(t *testing.T) {
	t.Parallel()
	mut := func(obs string) sjBatteryMutationEvidence { return sjBatteryMutationEvidence{Observed: obs} }
	probe := func(obs, exp string) sjProbeEvidence {
		return sjProbeEvidence{Observed: obs, Expected: exp, Matched: obs != probeObservedError && obs == exp}
	}
	for _, tc := range []struct {
		ev              sjBatteryEvidence
		verdict, reason string
	}{
		{sjBatteryEvidence{Mutations: []sjBatteryMutationEvidence{mut("red")}}, sjVerdictPass, ""},
		{sjBatteryEvidence{Mutations: []sjBatteryMutationEvidence{mut("red"), mut("green")}}, sjVerdictFail, ""},
		{sjBatteryEvidence{Mutations: []sjBatteryMutationEvidence{mut("timeout")}}, sjVerdictInconclusive, sjReasonTimeout},
		{sjBatteryEvidence{Mutations: []sjBatteryMutationEvidence{mut("error")}}, sjVerdictInconclusive, sjReasonExecutionError},
		{sjBatteryEvidence{Probes: []sjProbeEvidence{probe("fail", "pass")}}, sjVerdictFail, ""},
		{sjBatteryEvidence{Probes: []sjProbeEvidence{probe("error", "pass")}}, sjVerdictInconclusive, sjReasonProbeError},
		{sjBatteryEvidence{Mutations: []sjBatteryMutationEvidence{mut("error")}, Probes: []sjProbeEvidence{probe("pass", "fail")}}, sjVerdictFail, ""},
		{sjBatteryEvidence{Probes: []sjProbeEvidence{probe("pass", "pass"), probe("fail", "fail")}}, sjVerdictPass, ""},
	} {
		if v, r := batteryVerdict(tc.ev); v != tc.verdict || r != tc.reason {
			t.Fatalf("%+v → %s/%s, want %s/%s", tc.ev, v, r, tc.verdict, tc.reason)
		}
	}
}

func probeQ(id, expect string, probe map[string]any) map[string]any {
	return map[string]any{"id": id, "question": "q " + id, "probe": probe, "expect": expect}
}

// TestProbes_MatchMismatch: each structured probe form against the fixture,
// matched and mismatched, plus the refused forms.
func TestProbes_MatchMismatch(t *testing.T) {
	t.Parallel()
	repo, head, _ := newCFFixture(t)
	qs := []any{
		probeQ("exists", "pass", map[string]any{"path_exists": map[string]any{"path": "ctl.go"}}),
		probeQ("missing", "pass", map[string]any{"path_exists": map[string]any{"path": "nope.go"}}),
		probeQ("contains", "pass", map[string]any{"file_contains": map[string]any{"path": "ctl.go", "literal": pinnedFind}}),
		probeQ("lacks", "fail", map[string]any{"file_contains": map[string]any{"path": "ctl.go", "literal": "not in there"}}),
		probeQ("gotest-pass", "pass", map[string]any{"go_test": map[string]any{"tests": map[string]any{"run": "^TestAllowed$", "package": "."}}}),
		probeQ("gotest-fail", "pass", map[string]any{"go_test": map[string]any{"tests": map[string]any{"run": "^TestBroken$", "package": "."}}}),
		probeQ("escape", "pass", map[string]any{"path_exists": map[string]any{"path": "../x"}}),
		probeQ("gitdir", "pass", map[string]any{"path_exists": map[string]any{"path": ".git/config"}}),
		probeQ("two-forms", "pass", map[string]any{"path_exists": map[string]any{"path": "ctl.go"}, "file_contains": map[string]any{"path": "ctl.go", "literal": "x"}}),
	}
	r := newTestSJRun(t, repo, "counterfactual_battery", head, map[string]any{"probes": qs})
	o := executeBatteryJob(context.Background(), r)
	ev := o.Evidence.(sjBatteryEvidence)
	want := []struct {
		observed string
		matched  bool
	}{
		{"pass", true}, {"fail", false}, {"pass", true}, {"fail", true}, {"pass", true}, {"fail", false},
		{"error", false}, {"error", false}, {"error", false},
	}
	if len(ev.Probes) != len(want) {
		t.Fatalf("probes = %d, want %d", len(ev.Probes), len(want))
	}
	for i, w := range want {
		if p := ev.Probes[i]; p.Observed != w.observed || p.Matched != w.matched {
			t.Fatalf("probe %s = %+v, want observed=%s matched=%v", p.ID, p, w.observed, w.matched)
		}
	}
	if o.Verdict != sjVerdictFail {
		t.Fatalf("verdict %s, want fail on a mismatch", o.Verdict)
	}
	if _, err := os.Stat(filepath.Join(repo, ".git")); err != nil {
		t.Fatal(err)
	}
	if status := cfGit(t, repo, "status", "--porcelain"); status != "" {
		t.Fatalf("source checkout modified: %s", status)
	}
}

// TestInventory_ArgvBoundsAndRedaction: git_log is bounded and redacted,
// ls_files truncates at max_entries, go_list runs the fixed argv through the
// gate exec seam under the verify lock, an unknown command is refused.
func TestInventory_ArgvBoundsAndRedaction(t *testing.T) {
	t.Parallel()
	repo, _, _ := newCFFixture(t)
	secret := "ghp_" + strings.Repeat("A1", 18)
	for i := 0; i < 3; i++ {
		cfGit(t, repo, "commit", "-q", "--allow-empty", "-m", "leak "+secret)
	}
	head := cfGit(t, repo, "rev-parse", "HEAD")
	ctx := context.Background()

	r := newTestSJRun(t, repo, "inventory", head, map[string]any{"command": "git_log", "args": map[string]any{"max_count": 2}})
	o := executeInventoryJob(ctx, r)
	ev := o.Evidence.(sjInventoryEvidence)
	if o.Verdict != sjVerdictPass || len(ev.Lines) != 2 || ev.ExitCode != 0 || ev.Command != "git_log" {
		t.Fatalf("git_log = %+v %+v", o, ev)
	}
	for _, l := range ev.Lines {
		if strings.Contains(l, secret) || !strings.HasPrefix(l, head[:7]) && !strings.Contains(l, "\t") {
			t.Fatalf("git_log line not redacted or malformed: %q", l)
		}
	}

	r = newTestSJRun(t, repo, "inventory", head, map[string]any{"command": "ls_files", "args": map[string]any{"max_entries": 2}})
	o = executeInventoryJob(ctx, r)
	ev = o.Evidence.(sjInventoryEvidence)
	if o.Verdict != sjVerdictPass || len(ev.Lines) != 2 || !ev.Truncated {
		t.Fatalf("ls_files = %+v %+v, want 2 lines truncated", o, ev)
	}

	r = newTestSJRun(t, repo, "inventory", head, map[string]any{"command": "go_list", "args": map[string]any{"packages": []string{"."}}})
	probe := &lockProbe{lockPath: r.x.lockPath}
	rec := &recordingExec{out: "example.com/cf\n"}
	r.x.exec = func(ctx context.Context, argv []string, dir, lc string, to time.Duration) (string, int, gateDisposition) {
		if got, err := os.Readlink(probe.lockPath); err != nil || !strings.HasSuffix(got, ":runner") {
			t.Errorf("go list ran without the verify lock (link %q, %v)", got, err)
		}
		return rec.exec(ctx, argv, dir, lc, to)
	}
	o = executeInventoryJob(ctx, r)
	if o.Verdict != sjVerdictPass || rec.count() != 1 || strings.Join(rec.calls[0].argv, " ") != "go list -f {{.ImportPath}} ." {
		t.Fatalf("go_list = %+v calls %+v", o, rec.calls)
	}
	if err := requireScratchDir(rec.calls[0].dir, r.parent); err != nil {
		t.Fatalf("go list ran outside scratch: %v", err)
	}
	rec.code = 1
	if o = executeInventoryJob(ctx, r); o.Verdict != sjVerdictInconclusive || o.Reason != sjReasonExecutionError {
		t.Fatalf("non-zero go list = %+v, want inconclusive/execution_error", o)
	}

	for _, bad := range []map[string]any{
		{"command": "rm_rf", "args": map[string]any{}},
		{"command": "git_log", "args": map[string]any{"max_count": 201}},
		{"command": "git_log", "args": map[string]any{"max_count": 1, "paths": []string{"../etc"}}},
		{"command": "ls_files", "args": map[string]any{"max_entries": 1, "extra": true}},
		{"command": "go_list", "args": map[string]any{"packages": []string{"-toolexec=x"}}},
	} {
		r = newTestSJRun(t, repo, "inventory", head, bad)
		if o := executeInventoryJob(ctx, r); o.Verdict != sjVerdictInconclusive || o.Reason != sjReasonInputsInvalid {
			t.Fatalf("%v = %+v, want inconclusive/inputs_invalid", bad, o)
		}
	}
	// A fake materialize: a real clone whose rev-parse fails returns before
	// its detached fetch maintenance settles (AGENTS.md #3503 trap).
	r = newTestSJRun(t, repo, "inventory", head, map[string]any{"command": "ls_files", "args": map[string]any{"max_entries": 1}})
	r.x.materialize = func(context.Context, string, string, string) (string, error) {
		return "", fmt.Errorf("wrapped: %w", gateiso.ErrCommitNotFound)
	}
	if o := executeInventoryJob(ctx, r); o.Reason != sjReasonUnresolvableSHA {
		t.Fatalf("unknown head = %+v, want unresolvable_sha", o)
	}
}

func TestBoundInventoryLines(t *testing.T) {
	t.Parallel()
	long := strings.Repeat("é", inventoryMaxLineChars+5)
	lines, truncated := boundInventoryLines("a\n\n"+long+"\nb\n", 0)
	if len(lines) != 3 || !truncated || len([]rune(lines[1])) != inventoryMaxLineChars {
		t.Fatalf("lines %d truncated %v len %d", len(lines), truncated, len([]rune(lines[1])))
	}
	if lines, truncated = boundInventoryLines("a\nb\nc\n", 2); len(lines) != 2 || !truncated {
		t.Fatalf("limit not applied: %v %v", lines, truncated)
	}
}

// TestSpecialistJob_EndToEndBatteryAndInventory drives the subcommand
// against a fixture repo and a fake backend; each shipped completion decodes
// strictly into the golden-pinned envelope and the kind's typed evidence.
func TestSpecialistJob_EndToEndBatteryAndInventory(t *testing.T) {
	useTestSJDeps(t, nil)
	repo, head, _ := newCFFixture(t)
	for _, tc := range []struct {
		kind    string
		inputs  any
		verdict string
		decode  func([]byte) error
	}{
		{"counterfactual_battery", map[string]any{
			"mutations": []any{batteryMutation("ctl.go", pinnedFind, "return n >= 0", "^TestAllowed$")},
			"probes":    []any{probeQ("p1", "pass", map[string]any{"path_exists": map[string]any{"path": "ctl.go"}})},
		}, sjVerdictPass, func(b []byte) error { var e sjBatteryEvidence; return decodeJobInputs(b, &e) }},
		{"inventory", map[string]any{"command": "ls_files", "args": map[string]any{"max_entries": 50}},
			sjVerdictPass, func(b []byte) error { var e sjInventoryEvidence; return decodeJobInputs(b, &e) }},
	} {
		raw, _ := json.Marshal(tc.inputs)
		b, srv := newFakeSJBackend(t, upload.SpecialistJob{JobID: sjTestJobID, Kind: tc.kind, State: "queued",
			Subject: upload.SpecialistJobSubject{HeadSHA: head}, Inputs: raw})
		code, log := runSJSubcommand(srv.URL, repo)
		if code != exitOK {
			t.Fatalf("%s exit %d log %s", tc.kind, code, log)
		}
		_, _, results, nonce := b.snapshot()
		if len(results) != 1 || len(b.other) != 0 {
			t.Fatalf("%s results %d other %v", tc.kind, len(results), b.other)
		}
		c := decodeShippedCompletion(t, results[0])
		if c.Kind != tc.kind || c.Verdict != tc.verdict || c.ExecutionNonce != nonce {
			t.Fatalf("%s completion %+v", tc.kind, c)
		}
		if err := tc.decode(c.Evidence); err != nil {
			t.Fatalf("%s evidence does not decode strictly: %v\n%s", tc.kind, err, c.Evidence)
		}
	}
	if status := cfGit(t, repo, "status", "--porcelain"); status != "" {
		t.Fatalf("source checkout modified: %s", status)
	}
}

// TestSpecialistJobEvidence_MatchesSharedGolden pins this file's typed
// evidence against testdata/wire/specialist_job.json in both directions.
func TestSpecialistJobEvidence_MatchesSharedGolden(t *testing.T) {
	t.Parallel()
	raw, err := os.ReadFile(filepath.Join(pkgSrcDir, "..", "..", "..", "testdata", "wire", "specialist_job.json"))
	if err != nil {
		t.Fatal(err)
	}
	var golden struct {
		Completions map[string]json.RawMessage `json:"completions"`
	}
	if err := json.Unmarshal(raw, &golden); err != nil {
		t.Fatal(err)
	}
	for kind, target := range map[string]func() any{
		"counterfactual_battery": func() any { return &sjBatteryEvidence{} },
		"inventory":              func() any { return &sjInventoryEvidence{} },
	} {
		c := decodeShippedCompletion(t, golden.Completions[kind])
		v := target()
		if err := decodeJobInputs(c.Evidence, v); err != nil {
			t.Fatalf("%s golden evidence: %v", kind, err)
		}
		back, _ := json.Marshal(v)
		var a, b any
		_ = json.Unmarshal(back, &a)
		_ = json.Unmarshal(c.Evidence, &b)
		if ja, _ := json.Marshal(a); string(ja) != func() string { jb, _ := json.Marshal(b); return string(jb) }() {
			t.Fatalf("%s evidence does not round-trip:\n got %s\nwant %s", kind, back, c.Evidence)
		}
	}
}

// TestSpecialistJob_AmbiguousNotRunningReReadsJob: a 409
// specialist_job_not_running on the result counts as landed only when the
// re-read job is completed; a job still queued is a failure.
func TestSpecialistJob_AmbiguousNotRunningReReadsJob(t *testing.T) {
	useTestSJDeps(t, nil)
	for _, tc := range []struct {
		state string
		want  int
	}{{"completed", exitOK}, {"queued", exitFailure}} {
		b, srv := newFakeSJBackend(t, upload.SpecialistJob{JobID: sjTestJobID, Kind: "sj_never_registered", State: "queued"})
		b.resultConflict, b.stateAfterConflict = true, tc.state
		if code, log := runSJSubcommand(srv.URL, t.TempDir()); code != tc.want {
			t.Fatalf("job re-read as %s: exit %d, want %d; log %s", tc.state, code, tc.want, log)
		}
	}
}

// ---- Result attestation (E85.15 / #4363) ----

// sjTestAttestKey is a fixed job-scoped key as an attester's admission
// delivers it (base64 std of the 64-byte private key).
func sjTestAttestKey(t *testing.T) (ed25519.PrivateKey, string) {
	t.Helper()
	seed := bytes.Repeat([]byte{0x5a}, ed25519.SeedSize)
	priv := ed25519.NewKeyFromSeed(seed)
	return priv, base64.StdEncoding.EncodeToString(priv)
}

// runSJAttestJob registers a pass executor and drives the subcommand against
// b; the caller sets the env first.
func runSJAttestJob(t *testing.T, b *fakeSJBackend, srvURL string) (int, string) {
	t.Helper()
	specialistJobExecutors["sj_attest_kind"] = func(ctx context.Context, r *specialistJobRun) specialistJobOutcome {
		return specialistJobOutcome{Verdict: sjVerdictPass, Evidence: map[string]any{"ok": true}}
	}
	t.Cleanup(func() { delete(specialistJobExecutors, "sj_attest_kind") })
	return runSJSubcommand(srvURL, t.TempDir())
}

func newAttestSJBackend(t *testing.T) (*fakeSJBackend, string) {
	t.Helper()
	b, srv := newFakeSJBackend(t, upload.SpecialistJob{JobID: sjTestJobID, Kind: "sj_attest_kind", State: "queued", Inputs: json.RawMessage(`{}`)})
	return b, srv.URL
}

// TestParseSpecialistJobFlags_PrefersAttestToken: the runner attestation
// token wins when set; either variable alone suffices; neither is a usage
// error naming both.
func TestParseSpecialistJobFlags_PrefersAttestToken(t *testing.T) {
	args := []string{"--job-id", "j", "--backend-url", "http://x/", "--working-dir", "/abs", "--host", "h"}
	for _, tc := range []struct {
		attest, operator, wantToken, wantSource string
	}{
		{"attest-tok", "op-tok", "attest-tok", "FISHHAWK_RUNNER_ATTEST_TOKEN"},
		{"attest-tok", "", "attest-tok", "FISHHAWK_RUNNER_ATTEST_TOKEN"},
		{"", "op-tok", "op-tok", "FISHHAWK_API_TOKEN"},
	} {
		t.Setenv("FISHHAWK_RUNNER_ATTEST_TOKEN", tc.attest)
		t.Setenv("FISHHAWK_API_TOKEN", tc.operator)
		f, err := parseSpecialistJobFlags(args, io.Discard)
		if err != nil || f.token != tc.wantToken || f.tokenSource != tc.wantSource || f.backendURL != "http://x" {
			t.Fatalf("attest=%q operator=%q: token=%q source=%q err=%v; want %q from %s", tc.attest, tc.operator, f.token, f.tokenSource, err, tc.wantToken, tc.wantSource)
		}
	}
	t.Setenv("FISHHAWK_RUNNER_ATTEST_TOKEN", "")
	t.Setenv("FISHHAWK_API_TOKEN", "")
	if _, err := parseSpecialistJobFlags(args, io.Discard); err == nil ||
		!strings.Contains(err.Error(), "FISHHAWK_RUNNER_ATTEST_TOKEN") || !strings.Contains(err.Error(), "FISHHAWK_API_TOKEN") {
		t.Fatalf("no token: err = %v, want a usage error naming both variables", err)
	}
}

// TestExecuteSpecialistJob_SignsWithAdmissionKey: the admission delivers a
// key; every call rides the attestation token; the one shipped result carries
// a signature that verifies over its exact bytes; the log never names the key.
func TestExecuteSpecialistJob_SignsWithAdmissionKey(t *testing.T) {
	useTestSJDeps(t, nil)
	t.Setenv("FISHHAWK_RUNNER_ATTEST_TOKEN", "runner-attest-token")
	priv, keyB64 := sjTestAttestKey(t)
	b, url := newAttestSJBackend(t)
	b.attestKey = keyB64
	code, log := runSJAttestJob(t, b, url)
	if code != exitOK {
		t.Fatalf("exit %d, log %s", code, log)
	}
	b.mu.Lock()
	results, sigs, auths := append([][]byte(nil), b.results...), append([]string(nil), b.resultSigs...), append([]string(nil), b.auths...)
	b.mu.Unlock()
	if len(results) != 1 {
		t.Fatalf("results = %d, want 1", len(results))
	}
	sig, err := hex.DecodeString(sigs[0])
	if err != nil || !ed25519.Verify(priv.Public().(ed25519.PublicKey), upload.SpecialistJobResultMessage(sjTestJobID, results[0]), sig) {
		t.Fatalf("result signature %q does not verify over the shipped body (%v)", sigs[0], err)
	}
	for i, a := range auths {
		if a != "Bearer runner-attest-token" {
			t.Fatalf("request %d authenticated %q, want the runner attestation token", i, a)
		}
	}
	if strings.Contains(log, "specialist_job_attestation_degraded") {
		t.Fatalf("a delivered key logged a degrade: %s", log)
	}
}

// TestExecuteSpecialistJob_UndecodableKeyShipsUnsigned: no key, a non-base64
// key, a wrong-length key and a key whose public half does not match its seed
// each log specialist_job_attestation_degraded with its reason and ship ONE
// unsigned result.
func TestExecuteSpecialistJob_UndecodableKeyShipsUnsigned(t *testing.T) {
	priv, _ := sjTestAttestKey(t)
	mismatched := append(ed25519.PrivateKey(nil), priv...)
	mismatched[63] ^= 0xff
	for _, tc := range []struct {
		name, key, reason string
	}{
		{"no key", "", sjAttestNoKey},
		{"not base64", "!!not-base64!!", sjAttestKeyUndecodable},
		{"wrong length", base64.StdEncoding.EncodeToString(priv[:32]), sjAttestKeyUndecodable},
		{"public half mismatch", base64.StdEncoding.EncodeToString(mismatched), sjAttestKeyUndecodable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			useTestSJDeps(t, nil)
			t.Setenv("FISHHAWK_RUNNER_ATTEST_TOKEN", "runner-attest-token")
			b, url := newAttestSJBackend(t)
			b.attestKey = tc.key
			code, log := runSJAttestJob(t, b, url)
			if code != exitOK {
				t.Fatalf("exit %d, log %s", code, log)
			}
			b.mu.Lock()
			n, sigs := len(b.results), append([]string(nil), b.resultSigs...)
			b.mu.Unlock()
			if n != 1 || sigs[0] != "" {
				t.Fatalf("results %d signatures %q, want one unsigned result", n, sigs)
			}
			if !strings.Contains(log, "specialist_job_attestation_degraded") || !strings.Contains(log, tc.reason) {
				t.Fatalf("log does not name the degrade with reason %s: %s", tc.reason, log)
			}
			if tc.key != "" && strings.Contains(log, tc.key) {
				t.Fatalf("log carries the key material: %s", log)
			}
		})
	}
}

// TestExecuteSpecialistJob_NeverLogsAttestationKey: neither the base64 key
// the admission delivered nor its hex form nor its seed reaches the log sink,
// on the signed path or on a refused shipment.
func TestExecuteSpecialistJob_NeverLogsAttestationKey(t *testing.T) {
	priv, keyB64 := sjTestAttestKey(t)
	for _, refuse := range []bool{false, true} {
		useTestSJDeps(t, nil)
		t.Setenv("FISHHAWK_RUNNER_ATTEST_TOKEN", "runner-attest-token")
		b, url := newAttestSJBackend(t)
		b.attestKey = keyB64
		if refuse {
			b.resultRefusal = `{"error":{"code":"specialist_job_attestation_invalid","details":{"reason":"signature_invalid"}}}`
		}
		_, log := runSJAttestJob(t, b, url)
		for _, secret := range []string{keyB64, hex.EncodeToString(priv), hex.EncodeToString(priv.Seed()), base64.StdEncoding.EncodeToString(priv.Seed())} {
			if strings.Contains(log, secret) {
				t.Fatalf("refuse=%v: log carries attestation key material: %s", refuse, log)
			}
		}
		if !strings.Contains(log, `"attestation_key":"true"`) {
			t.Fatalf("refuse=%v: admission log does not record that a key was delivered: %s", refuse, log)
		}
	}
}

// TestExecuteSpecialistJob_AttestationRefusalIsPermanent: a 401
// specialist_job_attestation_invalid on the signed result exits 1 with the
// reason and does NOT re-ship unsigned — exactly one (signed) result POST.
func TestExecuteSpecialistJob_AttestationRefusalIsPermanent(t *testing.T) {
	useTestSJDeps(t, nil)
	t.Setenv("FISHHAWK_RUNNER_ATTEST_TOKEN", "runner-attest-token")
	_, keyB64 := sjTestAttestKey(t)
	b, url := newAttestSJBackend(t)
	b.attestKey = keyB64
	b.resultRefusal = `{"error":{"code":"specialist_job_attestation_invalid","details":{"reason":"attestation_expired"}}}`
	code, log := runSJAttestJob(t, b, url)
	if code != exitFailure || !strings.Contains(log, "attestation_expired") {
		t.Fatalf("exit %d, log %s; want exitFailure naming the refusal reason", code, log)
	}
	b.mu.Lock()
	n, sigs := len(b.results), append([]string(nil), b.resultSigs...)
	b.mu.Unlock()
	if n != 1 || sigs[0] == "" {
		t.Fatalf("result POSTs %d signatures %q, want exactly one signed POST and no unsigned re-ship", n, sigs)
	}
}

// TestDecodeSpecialistJobAttestationKey: each refusal row isolates ONE guard.
// "valid key + trailing garbage" decodes all 64 bytes before the base64 error,
// so only the error check refuses it; the 16-byte key passes base64, so only
// the length check refuses it (without it key.Seed() panics); the mismatched
// key is base64 and 64 bytes, so only the seed/public-half check refuses it.
func TestDecodeSpecialistJobAttestationKey(t *testing.T) {
	t.Parallel()
	priv, keyB64 := sjTestAttestKey(t)
	mismatched := append(ed25519.PrivateKey(nil), priv...)
	mismatched[40] ^= 0x01
	got, err := decodeSpecialistJobAttestationKey(keyB64)
	if err != nil || !bytes.Equal(got, priv) {
		t.Fatalf("valid key: %v", err)
	}
	for _, tc := range []struct{ name, in, want string }{
		{"valid key + trailing garbage", keyB64 + "!", "not base64"},
		{"short key", base64.StdEncoding.EncodeToString(priv[:16]), "16 bytes"},
		{"public half mismatch", base64.StdEncoding.EncodeToString(mismatched), "does not match its seed"},
	} {
		k, err := decodeSpecialistJobAttestationKey(tc.in)
		if err == nil || k != nil || !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("%s: key=%v err=%v, want refusal naming %q", tc.name, k != nil, err, tc.want)
		}
		if strings.Contains(err.Error(), keyB64) || strings.Contains(err.Error(), hex.EncodeToString(priv)) {
			t.Fatalf("%s: error carries key material: %v", tc.name, err)
		}
	}
}
