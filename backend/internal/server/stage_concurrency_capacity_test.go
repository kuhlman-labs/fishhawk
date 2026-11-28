package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"

	"github.com/kuhlman-labs/fishhawk/backend/internal/concurrency"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
	"github.com/kuhlman-labs/fishhawk/backend/internal/spec"
)

// ADR-094 D4/D5 as amended by A2 (#4241) and A5, server half: every stage
// that reaches the host-dispatch marker's admission arm holds one slot of
// host:<host>:capacity at the host's GOVERNING capacity (a fresh dispatcher
// declaration, else fishhawkd's --default-host-capacity, else 3; the marker
// body's `capacity` is validated but never sizes admission); each stage type
// has a default set that every declaration form adds to; and {capacity,
// host} is reserved. Every verdict drives the REAL handler over the newSCPG
// pgtest harness and reads slot rows back. The heartbeat-declaration rungs
// are host_capacity_declaration_test.go.

// specStage renders one workflow stage of typ (id == type) with an optional
// concurrency block for specDoc.
func specStage(typ run.StageType, block string) string {
	st := fmt.Sprintf("      - id: %s\n        type: %s\n        executor:\n          agent: claude-code\n", typ, typ)
	if block != "" {
		st += "        concurrency:" + block + "\n"
	}
	return st
}

// specDoc is a workflow-v2 document for workflow "wf" holding stages.
func specDoc(stages ...string) string {
	return "version: \"2\"\nworkflows:\n  wf:\n    stages:\n" + strings.Join(stages, "")
}

// typedSpec is a one-stage spec of typ declaring block.
func typedSpec(typ run.StageType, block string) string {
	return specDoc(specStage(typ, block))
}

// mustParseSpec fails the test when doc does not parse, so a fixture never
// silently takes the unparseable-spec fallback (no declaration).
func mustParseSpec(t *testing.T, doc string) string {
	t.Helper()
	if _, err := spec.ParseBytes([]byte(doc)); err != nil {
		t.Fatalf("fixture spec does not parse: %v\n%s", err, doc)
	}
	return doc
}

// capacityFixtureSpec declares implement {limit: 2} and acceptance
// {group: local-acceptance}; plan stages are undeclared.
func capacityFixtureSpec(t *testing.T) string {
	t.Helper()
	return mustParseSpec(t, specDoc(
		specStage(run.StageTypeImplement, "\n          limit: 2"),
		specStage(run.StageTypeAcceptance, "\n          group: local-acceptance"),
	))
}

// specStageOf creates a stage of typ for repo carrying doc.
func (f *scPG) specStageOf(repo string, typ run.StageType, doc string) *run.Stage {
	f.t.Helper()
	st := f.stage(repo, typ)
	f.setSpec(st, doc)
	return st
}

// heldRows counts the HELD slot rows of one group key.
func (f *scPG) heldRows(key string) int {
	f.t.Helper()
	var n int
	if err := f.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM stage_concurrency_slots WHERE group_key = $1 AND state = 'held'`, key).Scan(&n); err != nil {
		f.t.Fatalf("count held rows of %s: %v", key, err)
	}
	return n
}

// hasGroupRef reports whether refs lists key at limit.
func hasGroupRef(refs []concurrencyGroupRef, key string, limit int) bool {
	return slices.ContainsFunc(refs, func(r concurrencyGroupRef) bool { return r.Group == key && r.Limit == limit })
}

// groupStateOf returns the entry of gs for key.
func groupStateOf(gs []concurrencyGroupState, key string) (concurrencyGroupState, bool) {
	i := slices.IndexFunc(gs, func(g concurrencyGroupState) bool { return g.Group == key })
	if i < 0 {
		return concurrencyGroupState{}, false
	}
	return gs[i], true
}

func capacityBody(host string, capacity int) string {
	return fmt.Sprintf(`{"host":%q,"capacity":%d}`, host, capacity)
}

// TestHostDispatch_HostCapacityQueuesFourthStage (done-means, CF1 vehicle):
// at the default capacity of 3 (no capacity in the body), two implements and
// one acceptance on h1 fill host:h1:capacity, and a fourth stage of EVERY
// type — a plan, an acceptance in ANOTHER repository and a merge-candidate
// verify pass — answers 409 naming host:h1:capacity at limit 3. Each fourth
// stage's other groups are free (the org/two acceptance's
// spec:org/two:local-acceptance, the pass's local-verify:h1), so with the
// capacity group dropped from the set each would be admitted at once: no
// type group masks the deletion. The slot table holds exactly 3 held
// host:h1:capacity rows.
func TestHostDispatch_HostCapacityQueuesFourthStage(t *testing.T) {
	f := newSCPG(t, pgStore)
	doc := capacityFixtureSpec(t)
	holders := []*run.Stage{
		f.specStageOf("org/one", run.StageTypeImplement, doc),
		f.specStageOf("org/one", run.StageTypeImplement, doc),
		f.specStageOf("org/one", run.StageTypeAcceptance, doc),
	}
	for i, st := range holders {
		if resp := f.admitted(st, hostBody("h1")); !hasGroupRef(resp.Concurrency.Groups, "host:h1:capacity", concurrency.DefaultHostCapacity) {
			t.Errorf("holder %d groups = %+v, want host:h1:capacity at %d", i, resp.Concurrency.Groups, concurrency.DefaultHostCapacity)
		}
	}
	pass := f.specStageOf("org/one", run.StageTypeImplement, doc)
	f.triggerPass(pass, "4241aaaa")
	for _, tc := range []struct {
		name string
		st   *run.Stage
	}{
		{"plan", f.specStageOf("org/one", run.StageTypePlan, doc)},
		{"acceptance in org/two", f.specStageOf("org/two", run.StageTypeAcceptance, doc)},
		{"merge-candidate verify pass", pass},
	} {
		_, _, raw := f.queued(tc.st, hostBody("h1"))
		g, ok := groupStateOf(queuedGroups(t, raw), "host:h1:capacity")
		if !ok || g.Limit != concurrency.DefaultHostCapacity || len(g.Holders) != 3 {
			t.Errorf("%s details.groups = %s, want host:h1:capacity at limit %d with 3 holders", tc.name, raw, concurrency.DefaultHostCapacity)
		}
	}
	if n := f.heldRows("host:h1:capacity"); n != 3 {
		t.Errorf("held host:h1:capacity rows = %d, want exactly 3", n)
	}
}

// TestHostDispatch_FishhawkdDefaultTwoHonoured (done-means, CF4 vehicle):
// with no heartbeat declaration, fishhawkd's --default-host-capacity 2
// (ADR-094 A5 rung (b)) sizes the group at 2: two plan stages are admitted
// and the third is queued at limit 2. Ignoring the fishhawkd default (always
// DefaultHostCapacity 3) admits the third.
func TestHostDispatch_FishhawkdDefaultTwoHonoured(t *testing.T) {
	f := newSCPGWith(t, pgStore, withDefaultHostCapacity(2))
	for i := range 2 {
		st := f.stage(fhRepo, run.StageTypePlan)
		resp := f.admitted(st, hostBody("h2"))
		if resp.Concurrency.Group != "host:h2:capacity" || resp.Concurrency.Limit != 2 || resp.CapacityWarning != nil {
			t.Fatalf("plan %d = %+v (warning %+v), want host:h2:capacity at limit 2 and no warning", i, resp.Concurrency, resp.CapacityWarning)
		}
	}
	d, _, _ := f.queued(f.stage(fhRepo, run.StageTypePlan), hostBody("h2"))
	if d.Group != "host:h2:capacity" || d.Limit != 2 || len(d.Holders) != 2 {
		t.Fatalf("third plan details = %+v, want queued in host:h2:capacity at limit 2 behind two holders", d)
	}
	if n := f.heldRows("host:h2:capacity"); n != 2 {
		t.Errorf("held host:h2:capacity rows = %d, want 2", n)
	}
}

// TestHostDispatch_OldClientGetsDefaultCapacity (done-means): a client that
// sends no capacity — a host-only body, or no body at all (host
// local-implement's "unknown") — admits three plan stages at
// DefaultHostCapacity and queues the fourth.
func TestHostDispatch_OldClientGetsDefaultCapacity(t *testing.T) {
	for _, tc := range []struct{ name, body, key string }{
		{"host only", hostBody("h5"), "host:h5:capacity"},
		{"no body", "", "host:unknown:capacity"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newSCPG(t, pgStore)
			for range concurrency.DefaultHostCapacity {
				if resp := f.admitted(f.stage(fhRepo, run.StageTypePlan), tc.body); resp.Concurrency.Group != tc.key || resp.Concurrency.Limit != concurrency.DefaultHostCapacity {
					t.Fatalf("block = %+v, want %s at %d", resp.Concurrency, tc.key, concurrency.DefaultHostCapacity)
				}
			}
			if d, _, _ := f.queued(f.stage(fhRepo, run.StageTypePlan), tc.body); d.Group != tc.key || d.Limit != concurrency.DefaultHostCapacity {
				t.Fatalf("fourth plan details = %+v, want queued in %s at %d", d, tc.key, concurrency.DefaultHostCapacity)
			}
		})
	}
}

// TestHostDispatch_RequestCapacityIgnored (ADR-094 A5, CF1 vehicle; the
// inverted A2 C3 guard): a request's capacity never sizes admission. Stage 1
// sends 64 and is admitted at the governing default 3 with capacity_warning
// {64, 3, default}; stages 2 and 3 send none and are admitted at 3 with no
// warning; stage 4 sends 64 and is QUEUED at 3, its 409 details carrying the
// same warning. Honouring the request value would admit stage 4 at 64.
func TestHostDispatch_RequestCapacityIgnored(t *testing.T) {
	f := newSCPG(t, pgStore)
	want := hostCapacityWarning{RequestCapacity: 64, GoverningCapacity: concurrency.DefaultHostCapacity, Source: hostCapacitySourceDefault}
	resp := f.admitted(f.stage(fhRepo, run.StageTypePlan), capacityBody("h3", 64))
	if resp.Concurrency.Limit != concurrency.DefaultHostCapacity || resp.CapacityWarning == nil || *resp.CapacityWarning != want {
		t.Fatalf("stage 1 = %+v (warning %+v), want limit %d and warning %+v", resp.Concurrency, resp.CapacityWarning, concurrency.DefaultHostCapacity, want)
	}
	for i := 2; i <= 3; i++ {
		if resp := f.admitted(f.stage(fhRepo, run.StageTypePlan), hostBody("h3")); resp.Concurrency.Limit != concurrency.DefaultHostCapacity || resp.CapacityWarning != nil {
			t.Fatalf("stage %d = %+v (warning %+v), want limit %d and no warning", i, resp.Concurrency, resp.CapacityWarning, concurrency.DefaultHostCapacity)
		}
	}
	d, _, raw := f.queued(f.stage(fhRepo, run.StageTypePlan), capacityBody("h3", 64))
	if d.Group != "host:h3:capacity" || d.Limit != concurrency.DefaultHostCapacity || len(d.Holders) != 3 {
		t.Fatalf("stage 4 details = %+v, want queued in host:h3:capacity at limit %d behind 3 holders", d, concurrency.DefaultHostCapacity)
	}
	if got := queuedCapacityWarning(t, raw); got == nil || *got != want {
		t.Fatalf("stage 4 details.capacity_warning = %+v, want %+v:\n%s", got, want, raw)
	}
}

// queuedCapacityWarning decodes details.capacity_warning from a queued 409's
// raw details, nil when absent.
func queuedCapacityWarning(t *testing.T, raw string) *hostCapacityWarning {
	t.Helper()
	var d struct {
		CapacityWarning *hostCapacityWarning `json:"capacity_warning"`
	}
	if err := json.Unmarshal([]byte(raw), &d); err != nil {
		t.Fatalf("decode details: %v", err)
	}
	return d.CapacityWarning
}

// capacityExemptFixture (C1): capacity 6 on h4 (fishhawkd's
// --default-host-capacity, ADR-094 A5) with implement {limit: 2}.
// I1 and I2 are admitted and I3 is QUEUED on local-implement:h4 — an earlier
// fresh multi-group waiter on host:h4:capacity that is not capacity-bound
// (local-implement:h4 is full), so it reserves no capacity slot (ADR-094 A4).
func capacityExemptFixture(t *testing.T) (f *scPG, doc string, i3 *run.Stage) {
	t.Helper()
	f = newSCPGWith(t, pgStore, withDefaultHostCapacity(6))
	doc = capacityFixtureSpec(t)
	body := hostBody("h4")
	f.admitted(f.specStageOf("org/one", run.StageTypeImplement, doc), body)
	f.admitted(f.specStageOf("org/one", run.StageTypeImplement, doc), body)
	i3 = f.specStageOf("org/one", run.StageTypeImplement, doc)
	if d, _, _ := f.queued(i3, body); d.Group != "local-implement:h4" || d.Limit != 2 {
		t.Fatalf("fixture: I3 details = %+v, want queued on local-implement:h4 at limit 2", d)
	}
	return f, doc, i3
}

// admitLaterStages admits a plan, an acceptance and a merge-candidate verify
// pass on h4 at capacity 6: holders 2, 3 and 4, each + 0 reservations (I3 is
// not capacity-bound), stay below 6.
func admitLaterStages(f *scPG, doc string) {
	f.t.Helper()
	body := hostBody("h4")
	pass := f.specStageOf("org/one", run.StageTypeImplement, doc)
	f.triggerPass(pass, "4241bbbb")
	for _, st := range []*run.Stage{
		f.specStageOf("org/one", run.StageTypePlan, doc),
		f.specStageOf("org/one", run.StageTypeAcceptance, doc),
		pass,
	} {
		f.admitted(st, body)
	}
}

// TestHostDispatch_CapacityExemptFromFIFOAcrossTypes (C1, CF7 marker
// vehicle): a queued implement never FIFO-blocks a later stage of another
// type on the capacity group. With rule (b) re-applied to capacity, I3's
// earlier fresh queued row on host:h4:capacity queues the plan; condition (a)
// passes (2 holders + 0 reservations < 6: I3 is not capacity-bound), so rule
// (b) is the only thing in the path. I3 stays queued throughout.
func TestHostDispatch_CapacityExemptFromFIFOAcrossTypes(t *testing.T) {
	f, doc, i3 := capacityExemptFixture(t)
	admitLaterStages(f, doc)
	for _, r := range f.slotRows(i3.ID) {
		if r.state != "queued" {
			t.Fatalf("I3 slot rows = %+v, want every row still queued", f.slotRows(i3.ID))
		}
	}
	if n := f.heldRows("host:h4:capacity"); n != 5 {
		t.Errorf("held host:h4:capacity rows = %d, want 5", n)
	}
}

// TestHostDispatch_CapacityFullQueuesLaterStages (ADR-094 A4 through the
// real marker handler): after the later stages are admitted (5 holders), I3
// is queued on local-implement:h4 at limit 2 and is not capacity-bound, so it
// reserves no capacity slot and a second plan is ADMITTED (5 + 0 < 6). With 6
// host:h4:capacity rows held, a third plan is QUEUED on host:h4:capacity at
// limit 6 behind 6 holders.
func TestHostDispatch_CapacityFullQueuesLaterStages(t *testing.T) {
	f, doc, i3 := capacityExemptFixture(t)
	admitLaterStages(f, doc)
	body := hostBody("h4")
	f.admitted(f.specStageOf("org/one", run.StageTypePlan, doc), body)
	if n := f.heldRows("host:h4:capacity"); n != 6 {
		t.Errorf("held host:h4:capacity rows = %d, want 6", n)
	}
	d, _, _ := f.queued(f.specStageOf("org/one", run.StageTypePlan, doc), body)
	if d.Group != "host:h4:capacity" || d.Limit != 6 || len(d.Holders) != 6 {
		t.Fatalf("third plan details = %+v, want queued in host:h4:capacity at limit 6 behind 6 holders", d)
	}
	for _, r := range f.slotRows(i3.ID) {
		if r.state != "queued" {
			t.Fatalf("I3 slot rows = %+v, want every row still queued", f.slotRows(i3.ID))
		}
	}
}

// TestHostDispatch_CapacityBodyRefusals (CF5 vehicle, one assertion per
// failure mode; ADR-094 A5 keeps the request capacity VALIDATED though it no
// longer sizes admission): a capacity outside 1..64, or of a non-integer JSON
// type, answers 400 validation_failed with details.field=capacity before any
// stage read, so the stage is still awaiting_host_dispatch and no slot row
// exists. Without the range check, 0 / -1 / 65 are admitted at the governing
// capacity (200); without the type mapping, the type errors name field=body.
// An absent capacity and a JSON null are both accepted at the default.
func TestHostDispatch_CapacityBodyRefusals(t *testing.T) {
	f := newSCPG(t, pgStore)
	for _, tc := range []struct{ name, value string }{
		{"zero", "0"},
		{"negative", "-1"},
		{"above max", "65"},
		{"string", `"2"`},
		{"fraction", "2.5"},
		{"boolean", "true"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := f.stage(fhRepo, run.StageTypePlan)
			w := f.mark(st, `{"host":"h1","capacity":`+tc.value+`}`)
			code, details := decodeHostDispatchError(t, w)
			if w.Code != http.StatusBadRequest || code != "validation_failed" || details["field"] != "capacity" {
				t.Fatalf("marker = %d %q details %v, want 400 validation_failed field=capacity:\n%s", w.Code, code, details, w.Body.String())
			}
			if got := f.state(st.ID); got != run.StageStateAwaitingHostDispatch {
				t.Errorf("stage reads %s, want awaiting_host_dispatch", got)
			}
			if rows := f.slotRows(st.ID); len(rows) != 0 {
				t.Errorf("slot rows = %+v, want none", rows)
			}
		})
	}
	for _, tc := range []struct{ name, body string }{
		{"absent", `{"host":"h1"}`},
		{"null", `{"host":"h1","capacity":null}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resp := f.admitted(f.stage(fhRepo, run.StageTypePlan), tc.body)
			if resp.Concurrency.Group != "host:h1:capacity" || resp.Concurrency.Limit != concurrency.DefaultHostCapacity {
				t.Fatalf("block = %+v, want host:h1:capacity at the default %d", resp.Concurrency, concurrency.DefaultHostCapacity)
			}
		})
	}
}

// TestDeclaredGroupRequests_HostCapacityReserved (C8, CF6 vehicle): a
// declaration built in Go — so the schema pattern never runs — naming the
// host capacity group answers an error and no set, for the exact spelling
// and every case or whitespace variant of the name or the scope, wherever the
// entry sits. Dropping only the trim/lower fold leaves the exact row refused
// and turns every variant row into a set.
func TestDeclaredGroupRequests_HostCapacityReserved(t *testing.T) {
	first := spec.StageConcurrencyGroup{Group: "docs", Scope: spec.ConcurrencyScopeRepository}
	for _, tc := range []struct{ name, group, scope string }{
		{"exact", "capacity", "host"},
		{"title case", "Capacity", "host"},
		{"upper case", "CAPACITY", "host"},
		{"padded", " capacity ", "host"},
		{"leading space", " capacity", "host"},
		{"padded scope", "capacity", " Host "},
		{"upper scope", "capacity", "HOST"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			entry := spec.StageConcurrencyGroup{Group: tc.group, Scope: tc.scope}
			for i, entries := range [][]spec.StageConcurrencyGroup{{entry}, {first, entry}} {
				got, err := declaredGroupRequests(&spec.StageConcurrency{Groups: entries}, "o/r", "h1")
				want := fmt.Sprintf("concurrency.groups[%d]: host:h1:capacity is reserved", i)
				if err == nil || got != nil || !strings.Contains(err.Error(), want) {
					t.Fatalf("declaredGroupRequests(%+v) = %+v, %v; want no set and an error containing %q", entries, got, err, want)
				}
			}
		})
	}
}

// TestResolveStageConcurrency_DefaultSets: each stage type's default set and
// every declaration form, ADDED to it (ADR-094 A2). The capacity group is
// appended last at the request's capacity. A merge-candidate verify pass
// resolves to {local-verify:<host>, capacity} whatever is declared (C5: the
// host verify group ADR-092 D3 keeps, not D5's per-base group).
func TestResolveStageConcurrency_DefaultSets(t *testing.T) {
	f := newSCPG(t, pgStore)
	ctx := context.Background()
	const capLimit = 5
	capG := concurrency.GroupRequest{Key: "host:h1:capacity", Limit: capLimit}
	impl := func(n int) concurrency.GroupRequest {
		return concurrency.GroupRequest{Key: "local-implement:h1", Limit: n}
	}
	named := concurrency.GroupRequest{Key: "spec:" + fhRepo + ":x", Limit: 2}
	verify := concurrency.GroupRequest{Key: "local-verify:h1", Limit: concurrency.DefaultVerifyLimit}
	forms := []struct{ name, block string }{
		{"none", ""},
		{"limit shorthand", "\n          limit: 2"},
		{"named shorthand", "\n          group: x\n          limit: 2"},
		{"groups list", groupsSpec(groupEntry("x", "repository", 2))},
		{"groups host implement", groupsSpec(groupEntry("implement", "host", 0))},
	}
	nonImplement := map[string][]concurrency.GroupRequest{
		"none":                  {capG},
		"limit shorthand":       {impl(2), capG},
		"named shorthand":       {named, capG},
		"groups list":           {named, capG},
		"groups host implement": {impl(1), capG},
	}
	want := map[run.StageType]map[string][]concurrency.GroupRequest{
		run.StageTypePlan:       nonImplement,
		run.StageTypeAcceptance: nonImplement,
		run.StageTypeReview:     nonImplement,
		run.StageTypeImplement: {
			"none":                  {impl(1), capG},
			"limit shorthand":       {impl(2), capG},
			"named shorthand":       {impl(1), named, capG},
			"groups list":           {impl(1), named, capG},
			"groups host implement": {impl(1), capG},
		},
	}
	check := func(name string, st *run.Stage, wantSet []concurrency.GroupRequest) {
		t.Helper()
		runRow, err := f.repo.GetRun(ctx, st.RunID)
		if err != nil {
			t.Fatalf("get run: %v", err)
		}
		got, err := f.srv.resolveStageConcurrency(ctx, runRow, st, "h1", capLimit)
		if err != nil || !slices.Equal(got, wantSet) {
			t.Errorf("%s: resolveStageConcurrency = %+v, %v; want %+v", name, got, err, wantSet)
		}
	}
	for typ, byForm := range want {
		for _, form := range forms {
			st := f.specStageOf(fhRepo, typ, mustParseSpec(t, typedSpec(typ, form.block)))
			check(string(typ)+"/"+form.name, st, byForm[form.name])
		}
	}
	for _, form := range forms {
		st := f.specStageOf(fhRepo, run.StageTypeImplement, mustParseSpec(t, typedSpec(run.StageTypeImplement, form.block)))
		f.triggerPass(st, "4241cccc")
		check("verify pass/"+form.name, st, []concurrency.GroupRequest{verify, capG})
	}
}

// TestHostDispatch_DeclaredGroupsAreAdditive (done-means): an implement
// declaring groups [{preview-target, host}] holds the capacity group, its
// default local-implement:h1 AND the built-in preview-target, the last at
// limit 1. Under A1's replace rule it held preview-target alone.
func TestHostDispatch_DeclaredGroupsAreAdditive(t *testing.T) {
	f := newSCPG(t, pgStore)
	st := f.specStageOf(fhRepo, run.StageTypeImplement, mustParseSpec(t, concurrencySpec(groupsSpec(groupEntry("preview-target", "host", 0)))))
	resp := f.admitted(st, hostBody("h1"))
	want := []concurrencyGroupRef{
		{Group: "host:h1:capacity", Scope: "host", Limit: concurrency.DefaultHostCapacity},
		{Group: "host:h1:preview-target", Scope: "host", Limit: 1},
		{Group: "local-implement:h1", Scope: "host", Limit: concurrency.DefaultLimit},
	}
	if !slices.Equal(resp.Concurrency.Groups, want) || resp.Concurrency.Group != "host:h1:preview-target" {
		t.Fatalf("block = %+v, want binding host:h1:preview-target and groups %+v", resp.Concurrency, want)
	}
	rows := f.slotRows(st.ID)
	keys := make([]string, 0, len(rows))
	for _, r := range rows {
		if r.state != "held" {
			t.Fatalf("slot rows = %+v, want every row held", rows)
		}
		keys = append(keys, r.key)
	}
	if !slices.Equal(keys, []string{"host:h1:capacity", "host:h1:preview-target", "local-implement:h1"}) {
		t.Errorf("slot row keys = %v, want capacity, preview-target and local-implement", keys)
	}
}

// TestHostDispatch_PreviewTargetIsHostScoped: preview-target is one group per
// HOST, shared across repositories and held at limit 1. Two acceptance stages
// in different repositories on h1 contend for it (the second queues behind the
// first), and a third on h2 is admitted.
func TestHostDispatch_PreviewTargetIsHostScoped(t *testing.T) {
	f := newSCPG(t, pgStore)
	doc := mustParseSpec(t, typedSpec(run.StageTypeAcceptance, groupsSpec(groupEntry("preview-target", "host", 0))))
	a := f.specStageOf("org/one", run.StageTypeAcceptance, doc)
	b := f.specStageOf("org/two", run.StageTypeAcceptance, doc)
	c := f.specStageOf("org/two", run.StageTypeAcceptance, doc)
	f.admitted(a, hostBody("h1"))
	d, _, raw := f.queued(b, hostBody("h1"))
	g, ok := groupStateOf(queuedGroups(t, raw), "host:h1:preview-target")
	if d.Group != "host:h1:preview-target" || !ok || g.Limit != 1 || !sameStageIDs(holderIDs(g.Holders), a.ID) {
		t.Fatalf("B details = %s, want queued in host:h1:preview-target (limit 1) behind A", raw)
	}
	if resp := f.admitted(c, hostBody("h2")); !hasGroupRef(resp.Concurrency.Groups, "host:h2:preview-target", 1) {
		t.Fatalf("C groups = %+v, want host:h2:preview-target", resp.Concurrency.Groups)
	}
}
