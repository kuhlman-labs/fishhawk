package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/concurrency"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
	"github.com/kuhlman-labs/fishhawk/backend/internal/spec"
)

// ADR-094 D2/D3 (#4240) server half: a declared `groups` set crosses spec
// decode -> resolveStageConcurrency -> the store's set-wide Admit -> the
// 200/409 wire -> the slot audit rows -> the stage read block.

const fhRepo = "kuhlman-labs/fishhawk"

// groupsSpec is a `concurrency.groups` declaration for concurrencySpec.
func groupsSpec(entries ...string) string {
	return "\n          groups:" + strings.Join(entries, "")
}

func groupEntry(group, scope string, limit int) string {
	e := "\n            - group: " + group + "\n              scope: " + scope
	if limit > 0 {
		e += fmt.Sprintf("\n              limit: %d", limit)
	}
	return e
}

type scGroupRow struct {
	key        string
	state      string
	acquiredAt *time.Time
}

// slotRows reads every slot row of a stage in key order.
func (f *scPG) slotRows(id uuid.UUID) []scGroupRow {
	f.t.Helper()
	rows, err := f.pool.Query(context.Background(),
		`SELECT group_key, state, acquired_at FROM stage_concurrency_slots WHERE stage_id = $1 ORDER BY group_key`, id)
	if err != nil {
		f.t.Fatalf("read slot rows: %v", err)
	}
	defer rows.Close()
	var out []scGroupRow
	for rows.Next() {
		var r scGroupRow
		if err := rows.Scan(&r.key, &r.state, &r.acquiredAt); err != nil {
			f.t.Fatalf("scan slot row: %v", err)
		}
		out = append(out, r)
	}
	return out
}

// auditPayloads returns the payloads of category's rows for one stage.
func (f *scPG) auditPayloads(category string, stageID uuid.UUID) []map[string]any {
	f.t.Helper()
	rows, err := f.pool.Query(context.Background(),
		`SELECT payload FROM audit_entries WHERE category = $1 AND payload->>'stage_id' = $2 ORDER BY id`, category, stageID.String())
	if err != nil {
		f.t.Fatalf("read audit rows: %v", err)
	}
	defer rows.Close()
	var out []map[string]any
	for rows.Next() {
		var raw []byte
		if err := rows.Scan(&raw); err != nil {
			f.t.Fatalf("scan audit row: %v", err)
		}
		var p map[string]any
		if err := json.Unmarshal(raw, &p); err != nil {
			f.t.Fatalf("decode audit payload: %v", err)
		}
		out = append(out, p)
	}
	return out
}

// queuedGroups decodes details.groups from a raw 409 details object.
func queuedGroups(t *testing.T, rawDetails string) []concurrencyGroupState {
	t.Helper()
	var d struct {
		Groups []concurrencyGroupState `json:"groups"`
	}
	if err := json.Unmarshal([]byte(rawDetails), &d); err != nil {
		t.Fatalf("decode details.groups: %v\n%s", err, rawDetails)
	}
	return d.Groups
}

// auditGroupKeys lists a payload's groups[].group values (nil when absent).
func auditGroupKeys(p map[string]any) []string {
	gs, ok := p["groups"].([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(gs))
	for _, g := range gs {
		m, _ := g.(map[string]any)
		k, _ := m["group"].(string)
		out = append(out, k)
	}
	return out
}

// TestHostDispatch_DeclaredGroupsAdmitAtomically (CF14 vehicle): A and B are
// on different hosts and share only the repository group. A is admitted into
// every group of its set — the declared two plus the host capacity group
// (ADR-094 D4) — and B is queued with EVERY row queued (no partial hold),
// and every wire surface lists the set.
func TestHostDispatch_DeclaredGroupsAdmitAtomically(t *testing.T) {
	f := newSCPG(t, pgStore)
	a, b := f.impl(), f.impl()
	doc := concurrencySpec(groupsSpec(groupEntry("implement", "host", 0), groupEntry("deploy-target", "repository", 1)))
	f.setSpec(a, doc)
	f.setSpec(b, doc)
	repoKey := "spec:" + fhRepo + ":deploy-target"

	resp := f.admitted(a, hostBody("h1"))
	wantA := []concurrencyGroupRef{
		{Group: "host:h1:capacity", Scope: "host", Limit: concurrency.DefaultHostCapacity},
		{Group: "local-implement:h1", Scope: "host", Limit: concurrency.DefaultLimit},
		{Group: repoKey, Scope: "repository", Limit: 1},
	}
	if c := resp.Concurrency; c == nil || !slices.Equal(c.Groups, wantA) || c.Group != "local-implement:h1" || c.Limit != concurrency.DefaultLimit {
		t.Fatalf("A 200 concurrency = %+v, want binding local-implement:h1 and groups %+v", resp.Concurrency, wantA)
	}
	if rows := f.slotRows(a.ID); len(rows) != 3 || rows[0].state != "held" || rows[1].state != "held" || rows[2].state != "held" {
		t.Fatalf("A slot rows = %+v, want three held", rows)
	}
	if ps := f.auditPayloads(CategoryStageConcurrencyAdmitted, a.ID); len(ps) != 1 || !slices.Equal(auditGroupKeys(ps[0]), []string{"host:h1:capacity", "local-implement:h1", repoKey}) {
		t.Errorf("A admitted audit = %+v, want one row listing every group", ps)
	}

	d, msg, raw := f.queued(b, hostBody("h2"))
	gs := queuedGroups(t, raw)
	if len(gs) != 3 || gs[0].Group != "host:h2:capacity" || gs[0].Scope != "host" || gs[1].Group != "local-implement:h2" || gs[1].Scope != "host" ||
		gs[2].Group != repoKey || gs[2].Scope != "repository" {
		t.Fatalf("B details.groups = %+v, want host:h2:capacity and local-implement:h2 (host) and %s (repository)", gs, repoKey)
	}
	if !sameStageIDs(holderIDs(gs[2].Holders), a.ID) || len(gs[0].Holders) != 0 || len(gs[1].Holders) != 0 || gs[0].Position != 1 || gs[1].Position != 1 || gs[2].Position != 1 {
		t.Errorf("B details.groups = %+v, want A holding only the repository group, all at position 1", gs)
	}
	// Binding group: equal positions, so the canonical-first non-capacity key.
	if d.Group != "local-implement:h2" || d.Position != 1 {
		t.Errorf("B top-level = %+v, want the binding group local-implement:h2 at 1", d)
	}
	if !strings.HasSuffix(msg, `; group set: ["host:h2:capacity" (limit 3) at position 1, "local-implement:h2" (limit 1) at position 1, "`+repoKey+`" (limit 1) at position 1]`) {
		t.Errorf("B message = %q, want the group-set suffix", msg)
	}
	rows := f.slotRows(b.ID)
	if len(rows) != 3 {
		t.Fatalf("B slot rows = %+v, want three", rows)
	}
	for _, r := range rows {
		if r.state != "queued" || r.acquiredAt != nil {
			t.Fatalf("B slot rows = %+v, want every row queued, none acquired", rows)
		}
	}
	if ps := f.auditPayloads(CategoryStageConcurrencyQueued, b.ID); len(ps) != 1 || !slices.Equal(auditGroupKeys(ps[0]), []string{"host:h2:capacity", "local-implement:h2", repoKey}) || ps[0]["group"] != "local-implement:h2" {
		t.Errorf("B queued audit = %+v, want one row listing every group", ps)
	}
	for name, blk := range f.blocks(b) {
		if blk == nil || blk.Status != stageConcurrencyStatusQueued || blk.Group != "local-implement:h2" || blk.Position != 1 || len(blk.Groups) != 3 ||
			blk.Groups[2].Group != repoKey || blk.Groups[2].Position != 1 || !sameStageIDs(holderIDs(blk.Groups[2].Holders), a.ID) {
			t.Errorf("B %s block = %+v, want queued with every group", name, blk)
		}
	}
	for name, blk := range f.blocks(a) {
		if blk == nil || blk.Status != stageConcurrencyStatusHolding || blk.Group != "local-implement:h1" || len(blk.Groups) != 3 || blk.Groups[0].Position != 0 {
			t.Errorf("A %s block = %+v, want holding with every group and no positions", name, blk)
		}
	}

	f.to(a.ID, run.StageStateRunning, run.StageStateSucceeded)
	if resp := f.admitted(b, hostBody("h2")); len(resp.Concurrency.Groups) != 3 {
		t.Errorf("B 200 groups = %+v, want all three", resp.Concurrency.Groups)
	}
}

// TestHostDispatch_DeclaredHostImplementGroupIsTheDefaultGroup (CF13 server
// half): {implement, host} canonicalises to local-implement:<host>, the
// group an undeclared implement stage holds (ADR-094 A1).
func TestHostDispatch_DeclaredHostImplementGroupIsTheDefaultGroup(t *testing.T) {
	f := newSCPG(t, pgStore)
	x, y := f.impl(), f.impl()
	f.setSpec(y, concurrencySpec(groupsSpec(groupEntry("implement", "host", 0))))
	f.admitted(x, hostBody("h1"))
	d, _, raw := f.queued(y, hostBody("h1"))
	if d.Group != "local-implement:h1" || d.Limit != concurrency.DefaultLimit || !sameStageIDs(holderIDs(d.Holders), x.ID) {
		t.Fatalf("Y details = %+v, want queued behind X in local-implement:h1 at the default limit", d)
	}
	if gs := queuedGroups(t, raw); len(gs) != 2 || gs[0].Group != "host:h1:capacity" || gs[1].Group != "local-implement:h1" || gs[1].Scope != "host" {
		t.Errorf("Y details.groups = %+v, want the capacity group and the one default group", gs)
	}
}

// preChangeQueuedMessage is the pre-#4240 409 message, byte for byte.
func preChangeQueuedMessage(group string, limit, position int, why string) string {
	return fmt.Sprintf("stage queued for a concurrency slot in group %q (limit %d) at position %d: %s; re-POST the marker to keep the queue row live and to be admitted",
		group, limit, position, why)
}

// TestHostDispatch_BindingFieldsKeepPreChangeValues (CF-R7-suffix, ADR-094
// D4): every set now carries the host capacity group, but the top-level
// fields keep every pre-change value because the binding group prefers a
// non-capacity group, and the 409 message keeps the pre-change text byte for
// byte as its prefix, followed by the group-set suffix. The shorthand case
// runs on a PLAN stage, whose default set is the capacity group alone, so
// the named group is its stage-type group.
func TestHostDispatch_BindingFieldsKeepPreChangeValues(t *testing.T) {
	for _, tc := range []struct {
		name, group, scope string
		typ                run.StageType
		doc                string
	}{
		{"default implement", "local-implement:h1", "host", run.StageTypeImplement, concurrencySpec("")},
		{"shorthand plan", "spec:" + fhRepo + ":deploy-target", "repository", run.StageTypePlan, typedSpec(run.StageTypePlan, "\n          group: deploy-target")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newSCPG(t, pgStore)
			a, b := f.stage(fhRepo, tc.typ), f.stage(fhRepo, tc.typ)
			f.setSpec(a, tc.doc)
			f.setSpec(b, tc.doc)
			resp := f.admitted(a, hostBody("h1"))
			wantRefs := []concurrencyGroupRef{{Group: "host:h1:capacity", Scope: "host", Limit: concurrency.DefaultHostCapacity}, {Group: tc.group, Scope: tc.scope, Limit: 1}}
			if c := resp.Concurrency; c.Group != tc.group || c.Limit != 1 || c.QueuedBefore || !slices.Equal(c.Groups, wantRefs) {
				t.Fatalf("A 200 concurrency = %+v, want binding %s and groups %+v", c, tc.group, wantRefs)
			}
			d, msg, raw := f.queued(b, hostBody("h1"))
			if d.Group != tc.group || d.Limit != 1 || d.Position != 1 || !sameStageIDs(holderIDs(d.Holders), a.ID) {
				t.Fatalf("B details = %+v", d)
			}
			want := preChangeQueuedMessage(tc.group, 1, 1, "held by run "+a.RunID.String()+" stage "+a.ID.String()) +
				`; group set: ["host:h1:capacity" (limit 3) at position 1, "` + tc.group + `" (limit 1) at position 1]`
			if msg != want {
				t.Errorf("B message =\n%q\nwant the pre-change text plus the group-set suffix\n%q", msg, want)
			}
			gs := queuedGroups(t, raw)
			if len(gs) != 2 || gs[1].Group != tc.group || gs[1].Scope != tc.scope || gs[1].Limit != 1 || gs[1].Position != 1 || !sameStageIDs(holderIDs(gs[1].Holders), a.ID) {
				t.Errorf("B details.groups = %+v, want the stage-type entry equal to the top-level fields", gs)
			}
			for name, blk := range f.blocks(b) {
				if blk == nil || blk.Group != tc.group || blk.Position != 1 || len(blk.Groups) != 2 || blk.Groups[1].Group != tc.group || blk.Groups[1].Scope != tc.scope || blk.Groups[1].Position != 1 {
					t.Errorf("B %s block = %+v, want the binding group and two groups entries", name, blk)
				}
			}
		})
	}
}

// TestHostDispatch_UnresolvableGroupFailsClosed: a declared entry with no
// canonical key (here a repository name carrying ':', which validation of
// the spec cannot see) answers 500 and admits nothing: no CAS, no slot row.
func TestHostDispatch_UnresolvableGroupFailsClosed(t *testing.T) {
	f := newSCPG(t, pgStore)
	a := f.stage("org/one:bad", run.StageTypeImplement)
	f.setSpec(a, concurrencySpec(groupsSpec(groupEntry("deploy-target", "repository", 1))))
	w := f.mark(a, hostBody("h1"))
	if w.Code != http.StatusInternalServerError || !strings.Contains(w.Body.String(), "could not resolve the stage's concurrency groups") {
		t.Fatalf("marker = %d %s, want 500 naming the unresolvable group set", w.Code, w.Body.String())
	}
	if got := f.state(a.ID); got != run.StageStateAwaitingHostDispatch {
		t.Errorf("stage reads %s, want awaiting_host_dispatch (not admitted)", got)
	}
	if rows := f.slotRows(a.ID); len(rows) != 0 {
		t.Errorf("slot rows = %+v, want none", rows)
	}
}

// TestStageConcurrencyBuilders_EmptyGroupsSynthesiseOneEntry (R7,
// CF-R7-empty): every builder fed Groups nil returns ONE entry equal to the
// top-level fields and never indexes an empty slice.
func TestStageConcurrencyBuilders_EmptyGroupsSynthesiseOneEntry(t *testing.T) {
	h := concurrency.Holder{RunID: uuid.New(), StageID: uuid.New(), Since: time.Now().UTC()}
	req := []concurrency.GroupRequest{{Key: "local-implement:h1", Limit: 2}}

	queued := toStageConcurrency(concurrency.Status{State: concurrency.SlotQueued, GroupKey: "spec:o/r:x", Limit: 3, Position: 2, Holders: []concurrency.Holder{h}})
	if len(queued.Groups) != 1 || queued.Groups[0].Group != "spec:o/r:x" || queued.Groups[0].Scope != "repository" || queued.Groups[0].Limit != 3 ||
		queued.Groups[0].Position != 2 || !sameStageIDs(holderIDs(queued.Groups[0].Holders), h.StageID) {
		t.Errorf("queued read block groups = %+v", queued.Groups)
	}
	held := toStageConcurrency(concurrency.Status{State: concurrency.SlotHeld, GroupKey: "not-a-key", Limit: 1})
	if len(held.Groups) != 1 || held.Groups[0].Group != "not-a-key" || held.Groups[0].Scope != "" || held.Groups[0].Position != 0 || held.Groups[0].Holders == nil {
		t.Errorf("held read block groups = %+v, want one entry, scope omitted, holders []", held.Groups)
	}

	blk := toHostDispatchConcurrency(concurrency.Admission{Admitted: true, WaitedSeconds: 4}, req, "n1")
	if blk.Group != "local-implement:h1" || blk.Limit != 2 || blk.WaitedSeconds != 4 ||
		!slices.Equal(blk.Groups, []concurrencyGroupRef{{Group: "local-implement:h1", Scope: "host", Limit: 2}}) {
		t.Errorf("200 block = %+v", blk)
	}

	b, details := concurrencyQueuedDetails(uuid.New(), req, concurrency.Admission{Position: 3, Holders: []concurrency.Holder{h}})
	gs, _ := details["groups"].([]concurrencyGroupState)
	if b.Key != "local-implement:h1" || details["group"] != "local-implement:h1" || details["limit"] != 2 || details["position"] != 3 ||
		len(gs) != 1 || gs[0].Group != "local-implement:h1" || gs[0].Scope != "host" || gs[0].Limit != 2 || gs[0].Position != 3 || !sameStageIDs(holderIDs(gs[0].Holders), h.StageID) {
		t.Errorf("409 details = %+v, groups %+v", details, gs)
	}
}

// TestConcurrencyQueuedMessage_KeepsSubstrings (R7, CF-R7-suffix): each arm
// keeps its substring with one group and with two; only a set of more than
// one group carries the suffix.
func TestConcurrencyQueuedMessage_KeepsSubstrings(t *testing.T) {
	h := concurrency.Holder{RunID: uuid.New(), StageID: uuid.New()}
	two := []concurrency.GroupState{{Key: "local-implement:h1", Limit: 1, Position: 1}, {Key: "spec:o/r:x", Limit: 2, Position: 2}}
	for _, tc := range []struct {
		name string
		adm  concurrency.Admission
		want string
	}{
		{"held", concurrency.Admission{Position: 1, Holders: []concurrency.Holder{h}}, "held by "},
		{"contended", concurrency.Admission{Position: 1, Contended: true}, "no live holders; another admission held the group lock"},
		{"waiters", concurrency.Admission{Position: 2}, "no live holders; queued behind earlier waiters"},
	} {
		one := tc.adm
		one.Groups = two[:1]
		got := concurrencyQueuedMessage("g", 1, one)
		if !strings.Contains(got, tc.want) || strings.Contains(got, "group set") {
			t.Errorf("%s one group: %q, want %q and no group-set suffix", tc.name, got, tc.want)
		}
		set := tc.adm
		set.Groups = two
		got = concurrencyQueuedMessage("g", 1, set)
		if !strings.Contains(got, tc.want) || !strings.HasSuffix(got, `; group set: ["local-implement:h1" (limit 1) at position 1, "spec:o/r:x" (limit 2) at position 2]`) {
			t.Errorf("%s two groups: %q, want %q and the group-set suffix", tc.name, got, tc.want)
		}
	}
}

// TestDeclaredGroupRequests_HostKeyLimitOverrideUnconditional (S4 + H1,
// CF-S4-override): a hand-built declaration, never validated, carries limits
// on host entries. The host implement group takes the host default, every
// other host group takes hostScopedGroupLimit, and only a repository entry
// keeps its own limit. (Host verify is refused outright:
// TestDeclaredGroupRequests_HostVerifyReserved.)
func TestDeclaredGroupRequests_HostKeyLimitOverrideUnconditional(t *testing.T) {
	decl := &spec.StageConcurrency{Groups: []spec.StageConcurrencyGroup{
		{Group: "implement", Scope: spec.ConcurrencyScopeHost, Limit: 2},
		{Group: "preview-target", Scope: spec.ConcurrencyScopeHost, Limit: 2},
		{Group: "deploy-target", Scope: spec.ConcurrencyScopeRepository, Limit: 2},
		{Group: "docs", Scope: spec.ConcurrencyScopeRepository},
	}}
	got, err := declaredGroupRequests(decl, "o/r", "h1")
	if err != nil {
		t.Fatalf("declaredGroupRequests: %v", err)
	}
	want := []concurrency.GroupRequest{
		{Key: "local-implement:h1", Limit: concurrency.DefaultLimit},
		{Key: "host:h1:preview-target", Limit: 1},
		{Key: "spec:o/r:deploy-target", Limit: 2},
		{Key: "spec:o/r:docs", Limit: 1},
	}
	if !slices.Equal(got, want) {
		t.Errorf("declaredGroupRequests =\n%+v\nwant\n%+v", got, want)
	}
}

// TestDeclaredGroupRequests_HostVerifyReserved (ADR-094 A1 decision 6): a
// hand-built declaration, never validated, names {verify, host}, which
// canonicalises to local-verify:<host>, the group reserved for server-decided
// merge-candidate verify passes and specialist jobs. declaredGroupRequests
// answers an error and no set wherever the entry sits, with or without a
// limit, so the marker fails closed (500, nothing admitted) instead of
// admitting a declared stage into that group. Host implement in the same set
// is declarable and resolves on its own.
func TestDeclaredGroupRequests_HostVerifyReserved(t *testing.T) {
	implement := spec.StageConcurrencyGroup{Group: "implement", Scope: spec.ConcurrencyScopeHost}
	verify := spec.StageConcurrencyGroup{Group: "verify", Scope: spec.ConcurrencyScopeHost}
	limited := verify
	limited.Limit = 3
	for _, tc := range []struct {
		name    string
		entries []spec.StageConcurrencyGroup
		index   int
	}{
		{"alone", []spec.StageConcurrencyGroup{verify}, 0},
		{"after host implement", []spec.StageConcurrencyGroup{implement, verify}, 1},
		{"with a limit", []spec.StageConcurrencyGroup{implement, limited}, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := declaredGroupRequests(&spec.StageConcurrency{Groups: tc.entries}, "o/r", "h1")
			want := fmt.Sprintf("concurrency.groups[%d]: local-verify:h1 is reserved", tc.index)
			if err == nil || got != nil || !strings.Contains(err.Error(), want) {
				t.Fatalf("declaredGroupRequests = %+v, %v; want no set and an error containing %q", got, err, want)
			}
		})
	}
	got, err := declaredGroupRequests(&spec.StageConcurrency{Groups: []spec.StageConcurrencyGroup{implement}}, "o/r", "h1")
	if err != nil || !slices.Equal(got, []concurrency.GroupRequest{{Key: "local-implement:h1", Limit: concurrency.DefaultLimit}}) {
		t.Errorf("declaredGroupRequests({implement, host}) = %+v, %v; want local-implement:h1 at DefaultLimit", got, err)
	}
}

// TestDeclaredGroupRequests_Refusals: one subtest per refusal, each an
// identity that has no canonical key, answers an error and no set.
func TestDeclaredGroupRequests_Refusals(t *testing.T) {
	for _, tc := range []struct {
		name       string
		entry      spec.StageConcurrencyGroup
		repo, host string
		wantKeyErr bool
	}{
		{"account scope", spec.StageConcurrencyGroup{Group: "review", Scope: "account"}, "o/r", "h1", false},
		{"empty scope", spec.StageConcurrencyGroup{Group: "x"}, "o/r", "h1", false},
		{"repository with colon", spec.StageConcurrencyGroup{Group: "x", Scope: spec.ConcurrencyScopeRepository}, "o/r:bad", "h1", true},
		{"host with colon", spec.StageConcurrencyGroup{Group: "x", Scope: spec.ConcurrencyScopeHost}, "o/r", "h:1", true},
		{"empty name", spec.StageConcurrencyGroup{Scope: spec.ConcurrencyScopeRepository}, "o/r", "h1", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// A valid first entry in the OTHER scope, so the refusal is the
			// second entry's and the error must name index 1.
			first := spec.StageConcurrencyGroup{Group: "implement", Scope: spec.ConcurrencyScopeHost}
			if strings.Contains(tc.host, ":") {
				first = spec.StageConcurrencyGroup{Group: "docs", Scope: spec.ConcurrencyScopeRepository}
			}
			decl := &spec.StageConcurrency{Groups: []spec.StageConcurrencyGroup{first, tc.entry}}
			got, err := declaredGroupRequests(decl, tc.repo, tc.host)
			if err == nil || got != nil {
				t.Fatalf("declaredGroupRequests = %+v, %v; want an error and no set", got, err)
			}
			if tc.wantKeyErr != errors.Is(err, concurrency.ErrInvalidGroupKey) || !strings.Contains(err.Error(), "concurrency.groups[1]") {
				t.Errorf("err = %v, want ErrInvalidGroupKey=%v naming concurrency.groups[1]", err, tc.wantKeyErr)
			}
		})
	}
}

// TestBindingGroup_PicksTheDocumentedGroup: while queued the top-level
// fields describe the highest position (here the canonical-second group);
// once admitted, the canonical-first group, whatever the input order.
func TestBindingGroup_PicksTheDocumentedGroup(t *testing.T) {
	h := concurrency.Holder{RunID: uuid.New(), StageID: uuid.New()}
	set := []concurrency.GroupState{
		{Key: "spec:o/r:x", Limit: 2, Position: 3, Holders: []concurrency.Holder{h}},
		{Key: "local-implement:h1", Limit: 1, Position: 1},
	}
	b, details := concurrencyQueuedDetails(uuid.New(), nil, concurrency.Admission{Position: 1, Groups: set})
	if b.Key != "spec:o/r:x" || details["group"] != "spec:o/r:x" || details["limit"] != 2 || details["position"] != 3 {
		t.Errorf("queued binding = %+v, details %+v; want spec:o/r:x at position 3", b, details)
	}
	if hs, _ := details["holders"].([]concurrencyHolder); !sameStageIDs(holderIDs(hs), h.StageID) {
		t.Errorf("queued holders = %+v, want the binding group's", details["holders"])
	}
	if blk := toHostDispatchConcurrency(concurrency.Admission{Admitted: true, Groups: set}, nil, ""); blk.Group != "local-implement:h1" || blk.Limit != 1 || len(blk.Groups) != 2 {
		t.Errorf("admitted block = %+v, want binding local-implement:h1 and both groups", blk)
	}
}

// TestBindingGroup_SkipsCapacity (ADR-094 D4): the host capacity group sorts
// canonical-first (host:... < local-...) but never binds while another group
// ties or outranks it: admitted, the stage-type group binds; queued at equal
// positions, the stage-type group binds; only a capacity group alone at the
// strictly highest position binds. The R7 fallback (a store that left Groups
// empty) synthesises the requested set's non-capacity group too.
func TestBindingGroup_SkipsCapacity(t *testing.T) {
	cap3 := concurrency.GroupState{Key: "host:h1:capacity", Limit: 3, Position: 1}
	impl := concurrency.GroupState{Key: "local-implement:h1", Limit: 1, Position: 1}
	if b := bindingGroup([]concurrency.GroupState{cap3, impl}, true); b.Key != impl.Key {
		t.Errorf("admitted binding = %s, want %s", b.Key, impl.Key)
	}
	if b := bindingGroup([]concurrency.GroupState{cap3, impl}, false); b.Key != impl.Key {
		t.Errorf("queued tie binding = %s, want %s", b.Key, impl.Key)
	}
	high := cap3
	high.Position = 2
	if b := bindingGroup([]concurrency.GroupState{high, impl}, false); b.Key != high.Key {
		t.Errorf("queued capacity-highest binding = %s, want %s", b.Key, high.Key)
	}
	if b := bindingGroup([]concurrency.GroupState{cap3}, true); b.Key != cap3.Key {
		t.Errorf("capacity-only binding = %s, want %s", b.Key, cap3.Key)
	}
	req := []concurrency.GroupRequest{{Key: "host:h1:capacity", Limit: 3}, {Key: "local-implement:h1", Limit: 1}}
	if gs := admissionGroups(concurrency.Admission{Position: 2}, req); len(gs) != 1 || gs[0].Key != "local-implement:h1" || gs[0].Limit != 1 || gs[0].Position != 2 {
		t.Errorf("R7 fallback = %+v, want local-implement:h1 at limit 1, position 2", gs)
	}
}

// TestWithPromotedGroups: a promoted group's position and holders replace
// the admission's by key, a group the admission did not list is appended,
// and the input slice is not mutated.
func TestWithPromotedGroups(t *testing.T) {
	h := concurrency.Holder{RunID: uuid.New(), StageID: uuid.New()}
	in := []concurrency.GroupState{{Key: "local-implement:h1", Limit: 1, Position: 2}}
	got := withPromotedGroups(in, []concurrency.PromotionGroup{
		{Key: "local-implement:h1", Limit: 1, OldPosition: 2, NewPosition: 1, Holders: []concurrency.Holder{h}},
		{Key: "spec:o/r:x", Limit: 2, OldPosition: 3, NewPosition: 1},
	})
	if len(got) != 2 || got[0].Position != 1 || !slices.Equal(got[0].Holders, []concurrency.Holder{h}) ||
		got[1].Key != "spec:o/r:x" || got[1].Limit != 2 || got[1].Position != 1 {
		t.Errorf("withPromotedGroups = %+v", got)
	}
	if in[0].Position != 2 {
		t.Errorf("input mutated: %+v", in)
	}
}
