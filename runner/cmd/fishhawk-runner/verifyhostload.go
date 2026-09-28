package main

import (
	"context"
	"fmt"
	"io"

	"github.com/kuhlman-labs/fishhawk/runner/internal/agent"
	"github.com/kuhlman-labs/fishhawk/runner/internal/hostload"
)

// readHostLoad is the host-load sampling seam. runTestMain replaces it with a
// fixed NON-overloaded stub for the whole package test binary, so none of the
// existing runVerifyFixLoop call sites probes the real host or changes its
// classification when the developer's machine is loaded — which is exactly the
// #3663 condition. The dedicated preflight tests override it themselves with a
// t.Cleanup-restored assignment.
var readHostLoad = hostload.Read

// hostLoadFactor is the load-average-to-core-count ratio above which a FAILING
// committed-tree verify is reclassified from category A (artifact defect) to
// category C (infrastructure).
const hostLoadFactor = hostload.DefaultFactor

// hostLoadProbe samples the host load immediately before each committed-tree
// verify and retains the LAST sample, so the classification at the ladder tail
// judges the deciding verify's host conditions rather than the stage's average.
//
// Fail-open by construction: a load-read error leaves the probe holding no
// sample, overloaded() reports false, and the classification is exactly what it
// was before this change (category A) with one printed reason.
type hostLoadProbe struct {
	cfg  config
	sink io.Writer
	last hostload.Sample
	ok   bool
}

func newHostLoadProbe(cfg config, sink io.Writer) *hostLoadProbe {
	return &hostLoadProbe{cfg: cfg, sink: sink}
}

// sample takes one reading and returns the trace events it produced. A
// verify_host_overloaded event is emitted whenever the host IS overloaded — even
// when the verify then PASSES — so an overloaded host is visible in the trace
// regardless of the verdict.
func (p *hostLoadProbe) sample(ctx context.Context) []agent.Event {
	if p == nil {
		return nil
	}
	s, err := readHostLoad(ctx)
	if err != nil {
		p.ok = false
		_, _ = fmt.Fprintf(p.sink,
			`{"event":"verify_host_load_unavailable","run_id":%q,"stage_id":%q,"detail":%q}`+"\n",
			p.cfg.runID, p.cfg.stageID, err.Error())
		return nil
	}
	p.last, p.ok = s, true
	if !hostload.Overloaded(s, hostLoadFactor) {
		return nil
	}
	_, _ = fmt.Fprintf(p.sink,
		`{"event":"verify_host_overloaded","run_id":%q,"stage_id":%q,"load1":%.2f,"cores":%d,"factor":%.1f}`+"\n",
		p.cfg.runID, p.cfg.stageID, s.Load1, s.Cores, hostLoadFactor)
	return []agent.Event{{
		Kind: "verify_host_overloaded",
		Payload: agent.MakePayload(map[string]any{
			"load1":  s.Load1,
			"cores":  s.Cores,
			"factor": hostLoadFactor,
			"reason": hostload.Reason(s),
		}),
	}}
}

// overloaded reports whether the LAST successful sample was above the threshold.
// No sample (a read error, or a probe that never sampled) decides nothing.
func (p *hostLoadProbe) overloaded() bool {
	return p != nil && p.ok && hostload.Overloaded(p.last, hostLoadFactor)
}

// reason is the single-line lead PREPENDED to the failure evidence. The verify's
// own output follows it verbatim, so the reviewer still reads the real test
// output rather than a reason that replaced it.
func (p *hostLoadProbe) reason() string {
	if p == nil {
		return ""
	}
	return hostload.Reason(p.last)
}
