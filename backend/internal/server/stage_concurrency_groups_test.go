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
// both of its groups; B is queued with BOTH rows queued (no partial hold),
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
		{Group: "local-implement:h1", Scope: "host", Limit: concurrency.DefaultLimit},
		{Group: repoKey, Scope: "repository", Limit: 1},
	}
	if c := resp.Concurrency; c == nil || !slices.Equal(c.Groups, wantA) || c.Group != "local-implement:h1" || c.Limit != concurrency.DefaultLimit {
		t.Fatalf("A 200 concurrency = %+v, want binding local-implement:h1 and groups %+v", resp.Concurrency, wantA)
	}
	if rows := f.slotRows(a.ID); len(rows) != 2 || rows[0].state != "held" || rows[1].state != "held" {
		t.Fatalf("A slot rows = %+v, want two held", rows)
	}
	if ps := f.auditPayloads(CategoryStageConcurrencyAdmitted, a.ID); len(ps) != 1 || !slices.Equal(auditGroupKeys(ps[0]), []string{"local-implement:h1", repoKey}) {
		t.Errorf("A admitted audit = %+v, want one row listing both groups", ps)
	}

	d, msg, raw := f.queued(b, hostBody("h2"))
	gs := queuedGroups(t, raw)
	if len(gs) != 2 || gs[0].Group != "local-implement:h2" || gs[0].Scope != "host" || gs[1].Group != repoKey || gs[1].Scope != "repository" {
		t.Fatalf("B details.groups = %+v, want local-implement:h2 (host) and %s (repository)", gs, repoKey)
	}
	if !sameStageIDs(holderIDs(gs[1].Holders), a.ID) || len(gs[0].Holders) != 0 || gs[0].Position != 1 || gs[1].Position != 1 {
		t.Errorf("B details.groups = %+v, want A holding only the repository group, both at position 1", gs)
	}
	// Binding group: equal positions, so the canonical-first key.
	if d.Group != "local-implement:h2" || d.Position != 1 {
		t.Errorf("B top-level = %+v, want the binding group local-implement:h2 at 1", d)
	}
	if !strings.HasSuffix(msg, `; group set: ["local-implement:h2" (limit 1) at position 1, "`+repoKey+`" (limit 1) at position 1]`) {
		t.Errorf("B message = %q, want the group-set suffix", msg)
	}
	if rows := f.slotRows(b.ID); len(rows) != 2 || rows[0].state != "queued" || rows[1].state != "queued" || rows[0].acquiredAt != nil || rows[1].acquiredAt != nil {
		t.Fatalf("B slot rows = %+v, want both queued, none acquired", rows)
	}
	if ps := f.auditPayloads(CategoryStageConcurrencyQueued, b.ID); len(ps) != 1 || !slices.Equal(auditGroupKeys(ps[0]), []string{"local-implement:h2", repoKey}) || ps[0]["group"] != "local-implement:h2" {
		t.Errorf("B queued audit = %+v, want one row listing both groups", ps)
	}
	for name, blk := range f.blocks(b) {
		if blk == nil || blk.Status != stageConcurrencyStatusQueued || blk.Group != "local-implement:h2" || blk.Position != 1 || len(blk.Groups) != 2 ||
			blk.Groups[1].Group != repoKey || blk.Groups[1].Position != 1 || !sameStageIDs(holderIDs(blk.Groups[1].Holders), a.ID) {
			t.Errorf("B %s block = %+v, want queued with both groups", name, blk)
		}
	}
	for name, blk := range f.blocks(a) {
		if blk == nil || blk.Status != stageConcurrencyStatusHolding || len(blk.Groups) != 2 || blk.Groups[0].Position != 0 {
			t.Errorf("A %s block = %+v, want holding with both groups and no positions", name, blk)
		}
	}

	f.to(a.ID, run.StageStateRunning, run.StageStateSucceeded)
	if resp := f.admitted(b, hostBody("h2")); len(resp.Concurrency.Groups) != 2 {
		t.Errorf("B 200 groups = %+v, want both", resp.Concurrency.Groups)
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
	if gs := queuedGroups(t, raw); len(gs) != 1 || gs[0].Group != "local-implement:h1" || gs[0].Scope != "host" {
		t.Errorf("Y details.groups = %+v, want the one default group", gs)
	}
}

// preChangeQueuedMessage is the pre-#4240 409 message, byte for byte.
func preChangeQueuedMessage(group string, limit, position int, why string) string {
	return fmt.Sprintf("stage queued for a concurrency slot in group %q (limit %d) at position %d: %s; re-POST the marker to keep the queue row live and to be admitted",
		group, limit, position, why)
}

// TestHostDispatch_SingleGroupBlockGainsOneGroupEntry (CF-R7-suffix): a
// shorthand stage and a default stage keep every pre-change field value, the
// 409 message is byte-identical, and groups carries exactly one entry.
func TestHostDispatch_SingleGroupBlockGainsOneGroupEntry(t *testing.T) {
	for _, tc := range []struct {
		name, block, group, scope string
	}{
		{"default", "", "local-implement:h1", "host"},
		{"shorthand", "\n          group: deploy-target", "spec:" + fhRepo + ":deploy-target", "repository"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newSCPG(t, pgStore)
			a, b := f.impl(), f.impl()
			f.setSpec(a, concurrencySpec(tc.block))
			f.setSpec(b, concurrencySpec(tc.block))
			resp := f.admitted(a, hostBody("h1"))
			if c := resp.Concurrency; c.Group != tc.group || c.Limit != 1 || c.QueuedBefore ||
				!slices.Equal(c.Groups, []concurrencyGroupRef{{Group: tc.group, Scope: tc.scope, Limit: 1}}) {
				t.Fatalf("A 200 concurrency = %+v", c)
			}
			d, msg, raw := f.queued(b, hostBody("h1"))
			if d.Group != tc.group || d.Limit != 1 || d.Position != 1 || !sameStageIDs(holderIDs(d.Holders), a.ID) {
				t.Fatalf("B details = %+v", d)
			}
			want := preChangeQueuedMessage(tc.group, 1, 1, "held by run "+a.RunID.String()+" stage "+a.ID.String())
			if msg != want {
				t.Errorf("B message =\n%q\nwant byte-identical\n%q", msg, want)
			}
			gs := queuedGroups(t, raw)
			if len(gs) != 1 || gs[0].Group != tc.group || gs[0].Scope != tc.scope || gs[0].Limit != 1 || gs[0].Position != 1 || !sameStageIDs(holderIDs(gs[0].Holders), a.ID) {
				t.Errorf("B details.groups = %+v, want one entry equal to the top-level fields", gs)
			}
			for name, blk := range f.blocks(b) {
				if blk == nil || blk.Group != tc.group || blk.Position != 1 || len(blk.Groups) != 1 || blk.Groups[0].Group != tc.group || blk.Groups[0].Scope != tc.scope || blk.Groups[0].Position != 1 {
					t.Errorf("B %s block = %+v, want one groups entry", name, blk)
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
// on host entries. The host's legacy groups take the host defaults, every
// other host group takes hostScopedGroupLimit, and only a repository entry
// keeps its own limit.
func TestDeclaredGroupRequests_HostKeyLimitOverrideUnconditional(t *testing.T) {
	decl := &spec.StageConcurrency{Groups: []spec.StageConcurrencyGroup{
		{Group: "implement", Scope: spec.ConcurrencyScopeHost, Limit: 2},
		{Group: "verify", Scope: spec.ConcurrencyScopeHost, Limit: 3},
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
		{Key: "local-verify:h1", Limit: concurrency.DefaultVerifyLimit},
		{Key: "host:h1:preview-target", Limit: 1},
		{Key: "spec:o/r:deploy-target", Limit: 2},
		{Key: "spec:o/r:docs", Limit: 1},
	}
	if !slices.Equal(got, want) {
		t.Errorf("declaredGroupRequests =\n%+v\nwant\n%+v", got, want)
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
