package main

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/kuhlman-labs/fishhawk/runner/internal/agent"
	"github.com/kuhlman-labs/fishhawk/runner/internal/gitops"
	"github.com/kuhlman-labs/fishhawk/runner/internal/hostload"
	"github.com/kuhlman-labs/fishhawk/runner/internal/upload"
)

// withHostLoad overrides the package-wide runTestMain stub for one test,
// restoring it on cleanup.
func withHostLoad(t *testing.T, s hostload.Sample, err error) {
	t.Helper()
	restore := readHostLoad
	t.Cleanup(func() { readHostLoad = restore })
	readHostLoad = func(context.Context) (hostload.Sample, error) { return s, err }
}

// overloadedSample is the #3663 incident's shape: ~140 on a 10-core host (14x).
func overloadedSample() hostload.Sample {
	return hostload.Sample{Load1: 140.2, Cores: 10, Top: []hostload.Consumer{
		{PID: 4242, PCPU: 99.1, Command: "sh"},
		{PID: 4243, PCPU: 98.4, Command: "busyloop"},
	}}
}

// DONE-MEANS #2: a FAILING committed-tree verify on a starved host is
// classified category C with a host_overloaded: lead naming the load, the core
// count and at least one top consumer — and the real verify output is preserved
// verbatim after it.
func TestVerifyFixLoop_HostOverloadedClassifiesCategoryC(t *testing.T) {
	withHostLoad(t, overloadedSample(), nil)
	repo, _, _ := verifiedTreeRepo(t)
	cfg := verifiedTreeCfg(repo, "echo 'FAIL github.com/x/y 0.1s' >&2; false")
	cfg.verifyMaxIterations = 0
	res := agent.Result{OK: true}
	var logSink strings.Builder

	if _, _, err := runVerifyFixLoop(context.Background(), &cfg, nil, "", &fakeInvoker{canned: agent.Result{OK: true}}, agent.Invocation{}, &res, &logSink); err != nil {
		t.Fatalf("runVerifyFixLoop: %v\n%s", err, logSink.String())
	}
	if res.OK {
		t.Fatal("a failing verify must not leave res.OK true")
	}
	if res.FailureCategory != "C" {
		t.Fatalf("FailureCategory = %q, want C — an overloaded host is infrastructure, not an artifact defect\nreason: %s\nlog: %s",
			res.FailureCategory, res.FailureReason, logSink.String())
	}
	if !strings.HasPrefix(res.FailureReason, "host_overloaded:") {
		t.Fatalf("FailureReason must BEGIN with the host_overloaded: lead, got %q", res.FailureReason)
	}
	for _, want := range []string{"140.2", "10 cores", "14.0x", "pid 4242 sh 99.1%"} {
		if !strings.Contains(res.FailureReason, want) {
			t.Errorf("FailureReason missing %q:\n%s", want, res.FailureReason)
		}
	}
	// The verify's own output survives verbatim after the lead, so the reviewer
	// still sees the real failure.
	if !strings.Contains(res.FailureReason, "FAIL github.com/x/y") {
		t.Errorf("the verify output was not preserved after the lead:\n%s", res.FailureReason)
	}
	if n := countEvents(res.Events, "verify_host_overloaded"); n == 0 {
		t.Error("no verify_host_overloaded trace event was emitted")
	}
	if !strings.Contains(logSink.String(), `"event":"verify_host_overloaded"`) {
		t.Errorf("no verify_host_overloaded log line:\n%s", logSink.String())
	}
}

// m9 / c4's vehicle: an ordinary busy host (2.0 on 10 cores) with a FAILING
// verify stays category A with no host_overloaded lead — the threshold is what
// separates "starved" from "busy build".
func TestVerifyFixLoop_NormalLoadFailureStaysCategoryA(t *testing.T) {
	withHostLoad(t, hostload.Sample{Load1: 2.0, Cores: 10}, nil)
	repo, _, _ := verifiedTreeRepo(t)
	cfg := verifiedTreeCfg(repo, "false")
	cfg.verifyMaxIterations = 0
	res := agent.Result{OK: true}
	var logSink strings.Builder

	if _, _, err := runVerifyFixLoop(context.Background(), &cfg, nil, "", &fakeInvoker{canned: agent.Result{OK: true}}, agent.Invocation{}, &res, &logSink); err != nil {
		t.Fatalf("runVerifyFixLoop: %v", err)
	}
	if res.FailureCategory != "A" {
		t.Fatalf("FailureCategory = %q, want A at 2.0 load on 10 cores\nreason: %s", res.FailureCategory, res.FailureReason)
	}
	if strings.Contains(res.FailureReason, "host_overloaded") {
		t.Errorf("FailureReason carries a host_overloaded lead at normal load:\n%s", res.FailureReason)
	}
	if n := countEvents(res.Events, "verify_host_overloaded"); n != 0 {
		t.Errorf("verify_host_overloaded events = %d at normal load, want 0", n)
	}
}

// m10 / c5's vehicle: an overloaded host whose verify PASSES stays passed. The
// classification is gated on the deciding verify having FAILED; the overload is
// still recorded as a trace event so it is visible.
func TestVerifyFixLoop_OverloadedButPassingStaysPassed(t *testing.T) {
	withHostLoad(t, overloadedSample(), nil)
	repo, _, _ := verifiedTreeRepo(t)
	cfg := verifiedTreeCfg(repo, "true")
	cfg.verifyMaxIterations = 0
	res := agent.Result{OK: true}
	var logSink strings.Builder

	_, tree, err := runVerifyFixLoop(context.Background(), &cfg, nil, "", &fakeInvoker{canned: agent.Result{OK: true}}, agent.Invocation{}, &res, &logSink)
	if err != nil {
		t.Fatalf("runVerifyFixLoop: %v\n%s", err, logSink.String())
	}
	if !res.OK {
		t.Fatalf("a PASSING verify on an overloaded host must stay passed; category=%q reason=%q", res.FailureCategory, res.FailureReason)
	}
	if res.FailureCategory != "" {
		t.Fatalf("FailureCategory = %q, want empty on a pass", res.FailureCategory)
	}
	if tree == "" {
		t.Error("a passing loop must still return a verified tree")
	}
	if n := countEvents(res.Events, "verify_host_overloaded"); n == 0 {
		t.Error("an overloaded host that PASSES must still emit verify_host_overloaded")
	}
}

// m8: a load-read error fails OPEN — one printed reason, no classification
// change, category A exactly as before this change.
func TestVerifyFixLoop_HostLoadReadErrorFailsOpen(t *testing.T) {
	withHostLoad(t, hostload.Sample{}, errors.New("no readable load average"))
	repo, _, _ := verifiedTreeRepo(t)
	cfg := verifiedTreeCfg(repo, "false")
	cfg.verifyMaxIterations = 0
	res := agent.Result{OK: true}
	var logSink strings.Builder

	if _, _, err := runVerifyFixLoop(context.Background(), &cfg, nil, "", &fakeInvoker{canned: agent.Result{OK: true}}, agent.Invocation{}, &res, &logSink); err != nil {
		t.Fatalf("runVerifyFixLoop: %v", err)
	}
	if res.FailureCategory != "A" {
		t.Fatalf("FailureCategory = %q, want A on a load-read error (fail open)\nreason: %s", res.FailureCategory, res.FailureReason)
	}
	if !strings.Contains(logSink.String(), `"event":"verify_host_load_unavailable"`) {
		t.Errorf("a load-read error must print one named reason:\n%s", logSink.String())
	}
	if strings.Contains(res.FailureReason, "host_overloaded") {
		t.Errorf("a load-read error must not classify:\n%s", res.FailureReason)
	}
}

// The single-shot gate's twin arm: an overloaded host's failing gate wraps
// ErrVerifyInfraFailure, which committedGateFailureCategory resolves to C.
func TestVerifyGateCommitted_HostOverloadedWrapsInfraSentinel(t *testing.T) {
	withHostLoad(t, overloadedSample(), nil)
	repo, _, _ := verifiedTreeRepo(t)
	cfg := verifiedTreeCfg(repo, "echo 'FAIL github.com/x/y 0.1s' >&2; false")
	var logSink strings.Builder

	events, tree, err := runVerifyGateCommitted(context.Background(), cfg, &logSink)
	if err == nil {
		t.Fatalf("a failing gate must return an error; tree=%q", tree)
	}
	if !errors.Is(err, gitops.ErrVerifyInfraFailure) {
		t.Fatalf("error = %v, want it to wrap ErrVerifyInfraFailure so it classifies category C", err)
	}
	if got := committedGateFailureCategory(err); got != "C" {
		t.Fatalf("committedGateFailureCategory = %q, want C", got)
	}
	if !strings.Contains(err.Error(), "host_overloaded:") {
		t.Errorf("error must carry the host_overloaded lead: %v", err)
	}
	if !strings.Contains(err.Error(), "FAIL github.com/x/y") {
		t.Errorf("the gate output must be preserved verbatim after the lead: %v", err)
	}
	if n := countEvents(events, "verify_host_overloaded"); n == 0 {
		t.Error("no verify_host_overloaded event on the single-shot gate path")
	}
}

// The single-shot gate at normal load keeps ErrCommittedTestsFailed → category B.
func TestVerifyGateCommitted_NormalLoadKeepsTestsFailedSentinel(t *testing.T) {
	withHostLoad(t, hostload.Sample{Load1: 2.0, Cores: 10}, nil)
	repo, _, _ := verifiedTreeRepo(t)
	cfg := verifiedTreeCfg(repo, "false")
	var logSink strings.Builder

	_, _, err := runVerifyGateCommitted(context.Background(), cfg, &logSink)
	if err == nil {
		t.Fatal("a failing gate must return an error")
	}
	if !errors.Is(err, gitops.ErrCommittedTestsFailed) {
		t.Fatalf("error = %v, want ErrCommittedTestsFailed at normal load", err)
	}
	if strings.Contains(err.Error(), "host_overloaded") {
		t.Errorf("no host_overloaded lead at normal load: %v", err)
	}
}

// i1 (approval condition 4): with NO per-test override, readHostLoad is
// runTestMain's fixed non-overloaded stub — so none of this package's many
// runVerifyFixLoop callers probes the real host or changes its classification
// when the developer's machine is loaded.
func TestMainStubsHostLoadForPackage(t *testing.T) {
	got, err := readHostLoad(context.Background())
	if err != nil {
		t.Fatalf("the package stub must not error: %v", err)
	}
	if hostload.Overloaded(got, hostLoadFactor) {
		t.Fatalf("the package stub reports an OVERLOADED sample (%+v) — every runVerifyFixLoop caller in this package would reclassify under host load", got)
	}
	if got.Cores <= 0 {
		t.Errorf("the package stub must report a usable core count, got %d", got.Cores)
	}
}

// The probe retains the LAST sample, so the classification judges the deciding
// verify's host conditions rather than the stage's first reading.
func TestHostLoadProbe_RetainsTheLastSample(t *testing.T) {
	restore := readHostLoad
	t.Cleanup(func() { readHostLoad = restore })
	samples := []hostload.Sample{overloadedSample(), {Load1: 1.0, Cores: 10}}
	i := 0
	readHostLoad = func(context.Context) (hostload.Sample, error) {
		s := samples[i]
		if i < len(samples)-1 {
			i++
		}
		return s, nil
	}
	p := newHostLoadProbe(config{runID: "r", stageID: "s"}, &strings.Builder{})
	p.sample(context.Background())
	if !p.overloaded() {
		t.Fatal("after the first (overloaded) sample the probe must report overloaded")
	}
	p.sample(context.Background())
	if p.overloaded() {
		t.Fatal("after the second (calm) sample the probe must report NOT overloaded — the last sample governs")
	}
}

// A read ERROR after an overloaded sample must CLEAR the retained sample, so a
// verify whose actual host conditions were unreadable is classified by nothing
// (category A, fail open) rather than by a stale overloaded reading from an
// earlier verify in the same ladder.
func TestHostLoadProbe_ReadErrorClearsAnOverloadedSample(t *testing.T) {
	restore := readHostLoad
	t.Cleanup(func() { readHostLoad = restore })
	calls := 0
	readHostLoad = func(context.Context) (hostload.Sample, error) {
		calls++
		if calls == 1 {
			return overloadedSample(), nil
		}
		return hostload.Sample{}, errors.New("sysctl vm.loadavg: no such file")
	}
	p := newHostLoadProbe(config{runID: "r", stageID: "s"}, &strings.Builder{})
	p.sample(context.Background())
	if !p.overloaded() {
		t.Fatal("after the first (overloaded) sample the probe must report overloaded")
	}
	var log strings.Builder
	p.sink = &log
	if evs := p.sample(context.Background()); evs != nil {
		t.Fatalf("an erroring sample must emit no trace event, got %+v", evs)
	}
	if p.overloaded() {
		t.Fatal("an erroring sample must CLEAR the retained overloaded sample — the deciding verify's host conditions were unreadable, so the probe must decide nothing")
	}
	if !strings.Contains(log.String(), `"event":"verify_host_load_unavailable"`) {
		t.Fatalf("the read error must print one named reason, got:\n%s", log.String())
	}
}

// A probe that never sampled decides nothing.
func TestHostLoadProbe_UnsampledDecidesNothing(t *testing.T) {
	p := newHostLoadProbe(config{}, &strings.Builder{})
	if p.overloaded() {
		t.Fatal("an unsampled probe must not report overloaded")
	}
	var nilProbe *hostLoadProbe
	if nilProbe.overloaded() || nilProbe.reason() != "" || nilProbe.sample(context.Background()) != nil {
		t.Fatal("a nil probe must be inert")
	}
}

// Guard against the scope fixture drifting: these tests rely on a.txt being the
// single declared scope file, which is what makes the scoped/full verify forms
// behave as the fixture assumes.
func TestVerifyHostLoadFixtureScopeIsStable(t *testing.T) {
	cfg := verifiedTreeCfg("/tmp/x", "true")
	want := []upload.ScopeFile{{Path: "a.txt", Operation: "modify"}}
	if len(cfg.scopeFiles) != len(want) || cfg.scopeFiles[0] != want[0] {
		t.Fatalf("verifiedTreeCfg scope = %+v, want %+v", cfg.scopeFiles, want)
	}
}
