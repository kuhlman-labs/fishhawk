package mcpserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
)

// host_dispatcher_test.go drives the host-dispatcher core (E88.2 / #4526)
// through the REAL apiClient against an httptest backend serving
// backend-shaped JSON: wire decode -> decision -> spawn seam.

const hdTestBearer = "fhm_test_dispatcher_bearer_value"

type hdRun struct {
	ID             string
	State          string
	RunnerKind     string
	WorkingDir     string
	AutoDispatch   bool
	SliceIndex     *int
	DecomposedFrom string
	Stages         []Stage
	// Link is served under include=campaign_link; LinkCode non-zero answers
	// that HTTP status instead.
	Link     *runCampaignLink
	LinkCode int
}

type hdRequest struct {
	Method string
	Path   string
	Body   string
}

type hdBackend struct {
	t    *testing.T
	mu   sync.Mutex
	runs map[string]*hdRun
	reqs []hdRequest
	acts []RecordAutoDriveAct
	// marker overrides the default CAS marker for a stage; nil uses the CAS
	// ({pending, awaiting_host_dispatch} -> dispatched, else transitioned:false).
	marker func(runID, stageID string) (int, string)
	// recordCode non-zero fails every record-act with that status.
	recordCode int
	// parentCode non-zero answers that status for a run id not in runs.
	parentCode int
	// admission is the acceptance-admission 200 body.
	admission string
	reaps     []string
	srv       *httptest.Server
}

func newHDBackend(t *testing.T) *hdBackend {
	t.Helper()
	b := &hdBackend{t: t, runs: map[string]*hdRun{}, admission: `{"short_circuited":false}`}
	b.srv = httptest.NewServer(http.HandlerFunc(b.serve))
	t.Cleanup(b.srv.Close)
	return b
}

func (b *hdBackend) count(pred func(hdRequest) bool) int {
	b.mu.Lock()
	defer b.mu.Unlock()
	n := 0
	for _, r := range b.reqs {
		if pred(r) {
			n++
		}
	}
	return n
}

func isMarkerReq(r hdRequest) bool {
	return r.Method == http.MethodPost && strings.HasSuffix(r.Path, "/host-dispatch")
}
func isRecordReq(r hdRequest) bool {
	return r.Method == http.MethodPost && strings.HasSuffix(r.Path, "/auto-drive/acts")
}
func isLinkReq(r hdRequest) bool {
	return r.Method == http.MethodGet && strings.Contains(r.Path, "include=campaign_link")
}

func writeJSON(w http.ResponseWriter, code int, body string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_, _ = io.WriteString(w, body)
}

func apiErrBody(code, msg string) string {
	return fmt.Sprintf(`{"error":{"code":%q,"message":%q}}`, code, msg)
}

func (b *hdBackend) runJSON(r *hdRun, withLink bool) string {
	m := map[string]any{
		"id": r.ID, "repo": "acme/widgets", "workflow_id": "feature_change", "workflow_sha": "abc",
		"trigger_source": "api", "state": r.State, "runner_kind": r.RunnerKind,
		"forge": "github", "default_branch": "main", "retry_attempt": 0, "max_retries_snapshot": 0,
	}
	if r.WorkingDir != "" {
		m["working_dir"] = r.WorkingDir
	}
	if r.AutoDispatch {
		m["auto_dispatch"] = true
	}
	if r.SliceIndex != nil {
		m["slice_index"] = *r.SliceIndex
	}
	if r.DecomposedFrom != "" {
		m["decomposed_from"] = r.DecomposedFrom
	}
	if withLink && r.Link != nil {
		m["campaign_link"] = r.Link
	}
	out, _ := json.Marshal(m)
	return string(out)
}

func (b *hdBackend) serve(w http.ResponseWriter, req *http.Request) {
	body, _ := io.ReadAll(req.Body)
	path := req.URL.Path
	full := path
	if req.URL.RawQuery != "" {
		full += "?" + req.URL.RawQuery
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	b.reqs = append(b.reqs, hdRequest{Method: req.Method, Path: full, Body: string(body)})
	parts := strings.Split(strings.Trim(path, "/"), "/")
	// /v0/stages/{sid}/acceptance-admission
	if len(parts) == 4 && parts[1] == "stages" && parts[3] == "acceptance-admission" {
		writeJSON(w, 200, b.admission)
		return
	}
	if len(parts) < 3 || parts[0] != "v0" || parts[1] != "runs" {
		writeJSON(w, 404, apiErrBody("not_found", path))
		return
	}
	run, ok := b.runs[parts[2]]
	if !ok {
		if b.parentCode != 0 {
			writeJSON(w, b.parentCode, apiErrBody("internal", "parent read failed"))
			return
		}
		writeJSON(w, 404, apiErrBody("run_not_found", parts[2]))
		return
	}
	switch {
	case len(parts) == 3 && req.Method == http.MethodGet:
		if req.URL.Query().Get("include") == "campaign_link" && run.LinkCode != 0 {
			writeJSON(w, run.LinkCode, apiErrBody("campaign_link_unavailable", "repo read failed"))
			return
		}
		writeJSON(w, 200, b.runJSON(run, req.URL.Query().Get("include") == "campaign_link"))
	case len(parts) == 4 && parts[3] == "stages":
		out, _ := json.Marshal(map[string]any{"items": run.Stages})
		writeJSON(w, 200, string(out))
	case len(parts) == 5 && parts[3] == "auto-drive" && parts[4] == "acts":
		if b.recordCode != 0 {
			writeJSON(w, b.recordCode, apiErrBody("internal", "record failed"))
			return
		}
		var act RecordAutoDriveAct
		_ = json.Unmarshal(body, &act)
		b.acts = append(b.acts, act)
		writeJSON(w, 200, `{"recorded":true}`)
	case len(parts) == 6 && parts[3] == "stages" && parts[5] == "host-dispatch":
		if b.marker != nil {
			code, resp := b.marker(parts[2], parts[4])
			if code == 200 && strings.Contains(resp, `"transitioned":true`) {
				b.setState(run, parts[4], "dispatched")
			}
			writeJSON(w, code, resp)
			return
		}
		for i := range run.Stages {
			if run.Stages[i].ID == parts[4] {
				switch run.Stages[i].State {
				case "pending", "awaiting_host_dispatch":
					run.Stages[i].State = "dispatched"
					writeJSON(w, 200, `{"transitioned":true,"stage_state":"dispatched"}`)
				default:
					writeJSON(w, 200, fmt.Sprintf(`{"transitioned":false,"stage_state":%q}`, run.Stages[i].State))
				}
				return
			}
		}
		writeJSON(w, 404, apiErrBody("stage_not_found", parts[4]))
	case len(parts) == 6 && parts[3] == "stages" && parts[5] == "reap-failure":
		b.reaps = append(b.reaps, parts[4])
		b.setState(run, parts[4], "failed")
		writeJSON(w, 200, `{"state":"failed"}`)
	default:
		writeJSON(w, 404, apiErrBody("not_found", path))
	}
}

func (b *hdBackend) setState(run *hdRun, stageID, state string) {
	for i := range run.Stages {
		if run.Stages[i].ID == stageID {
			run.Stages[i].State = state
		}
	}
}

func (b *hdBackend) stageState(runID, stageID string) string {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, st := range b.runs[runID].Stages {
		if st.ID == stageID {
			return st.State
		}
	}
	return ""
}

type hdSpawn struct {
	Argv []string
	Env  []string
	Run  string
}

type hdSpawner struct {
	mu     sync.Mutex
	spawns []hdSpawn
	err    error
}

func (s *hdSpawner) spawn(_ string, argv, env []string, runID, _ string, _ detachedFailureReporter, _ detachedStageStateProbe) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil {
		return "", s.err
	}
	s.spawns = append(s.spawns, hdSpawn{Argv: append([]string(nil), argv...), Env: append([]string(nil), env...), Run: runID})
	return "/tmp/hd.log", nil
}

func (s *hdSpawner) n() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.spawns)
}

type hdGate struct {
	refusal  string
	admits   int
	releases int
	// onAdmit runs inside Admit (e.g. to model a pause landing).
	onAdmit func() string
}

func (g *hdGate) Admit(_ context.Context, _ hostDispatchCandidate) (func(), string, error) {
	if g.onAdmit != nil {
		if code := g.onAdmit(); code != "" {
			return nil, code, nil
		}
	}
	if g.refusal != "" {
		return nil, g.refusal, nil
	}
	g.admits++
	return func() { g.releases++ }, "", nil
}

type hdCounter struct{ used map[string]bool }

func newHDCounter() *hdCounter { return &hdCounter{used: map[string]bool{}} }

func (c *hdCounter) key(stageID string, epoch time.Time) string {
	return stageID + "@" + epoch.UTC().Format(time.RFC3339Nano)
}
func (c *hdCounter) Allowed(stageID string, epoch time.Time) bool {
	return !c.used[c.key(stageID, epoch)]
}
func (c *hdCounter) Reserve(stageID string, epoch time.Time) (bool, error) {
	k := c.key(stageID, epoch)
	if c.used[k] {
		return false, nil
	}
	c.used[k] = true
	return true, nil
}

type hdHarness struct {
	b       *hdBackend
	d       *hostDispatcher
	spawner *hdSpawner
	gate    *hdGate
	root    string
}

func newHDHarness(t *testing.T) *hdHarness {
	t.Helper()
	b := newHDBackend(t)
	root := t.TempDir()
	api := &apiClient{baseURL: b.srv.URL, token: hdTestBearer, http: &http.Client{Timeout: 10 * time.Second}}
	r := &runResolver{api: api, getenv: func(string) string { return "" }, allowedRoots: []string{root}}
	g := &hdGate{}
	sp := &hdSpawner{}
	d := newHostDispatcher(r, "/bin/fishhawk-runner", g, newHDCounter())
	d.spawn = sp.spawn
	d.probe = func(context.Context, string) runnerLivenessVerdict { return runnerDead }
	d.environ = func() []string { return []string{"PATH=/usr/bin"} }
	return &hdHarness{b: b, d: d, spawner: sp, gate: g, root: root}
}

var hdUnlinked = &runCampaignLink{Status: "unlinked", Campaigns: []runCampaignLinkEntry{}}

func hdLinked(state string) *runCampaignLink {
	return &runCampaignLink{Status: "linked", Campaigns: []runCampaignLinkEntry{{CampaignID: uuid.NewString(), ItemID: uuid.NewString(), State: state}}}
}

// addRun registers a local marked run whose plan stage is parked.
func (h *hdHarness) addRun(mut func(*hdRun)) *hdRun {
	r := &hdRun{
		ID: uuid.NewString(), State: "running", RunnerKind: "local", WorkingDir: h.root,
		AutoDispatch: true, Link: hdUnlinked,
	}
	r.Stages = []Stage{
		{ID: uuid.NewString(), RunID: r.ID, Sequence: 1, Type: "plan", State: "awaiting_host_dispatch", UpdatedAt: time.Now()},
		{ID: uuid.NewString(), RunID: r.ID, Sequence: 2, Type: "implement", State: "pending", UpdatedAt: time.Now()},
	}
	if mut != nil {
		mut(r)
	}
	h.b.mu.Lock()
	h.b.runs[r.ID] = r
	h.b.mu.Unlock()
	return r
}

func (h *hdHarness) eval(t *testing.T, r *hdRun) hostDispatchOutcome {
	t.Helper()
	return h.d.evaluate(context.Background(), uuid.MustParse(r.ID))
}

func TestHostDispatcherWire(t *testing.T) {
	h := newHDHarness(t)
	marked := h.addRun(func(r *hdRun) { r.Link = hdLinked("paused") })
	unmarked := h.addRun(func(r *hdRun) { r.AutoDispatch = false; r.Link = nil })
	ctx := context.Background()

	got, err := h.d.r.api.GetRun(ctx, uuid.MustParse(marked.ID))
	if err != nil || !got.AutoDispatch || got.CampaignLink != nil {
		t.Fatalf("marked run: auto_dispatch=%v campaign_link=%v err=%v; want true, nil (no include), nil", got.AutoDispatch, got.CampaignLink, err)
	}
	got, err = h.d.r.api.GetRun(ctx, uuid.MustParse(unmarked.ID))
	if err != nil || got.AutoDispatch {
		t.Fatalf("unmarked run decoded auto_dispatch=%v err=%v; want false", got.AutoDispatch, err)
	}
	link, err := h.d.r.api.GetRunCampaignLink(ctx, uuid.MustParse(marked.ID))
	if err != nil || link.Status != "linked" || len(link.Campaigns) != 1 || link.Campaigns[0].State != "paused" {
		t.Fatalf("campaign link = %+v, %v; want linked/paused", link, err)
	}
	if _, err := h.d.r.api.GetRunCampaignLink(ctx, uuid.MustParse(unmarked.ID)); !errors.Is(err, errCampaignLinkAbsent) {
		t.Fatalf("absent block err = %v; want errCampaignLinkAbsent", err)
	}
	if _, err := h.d.r.api.ListRuns(ctx, listRunsFilter{RunnerKind: "local", State: "running"}); err == nil {
		t.Fatal("fake serves no list route; expected an error")
	}
	if n := h.b.count(func(r hdRequest) bool { return strings.Contains(r.Path, "runner_kind=local") }); n != 1 {
		t.Fatalf("ListRuns runner_kind filter not sent: %d requests carried it", n)
	}
	if !asHostMemoryPressure(fmt.Errorf("wrap: %w", &apiError{Code: "host_memory_pressure"})) {
		t.Fatal("asHostMemoryPressure missed a wrapped 409 host_memory_pressure")
	}
	if asHostMemoryPressure(&apiError{Code: "concurrency_slot_queued"}) {
		t.Fatal("asHostMemoryPressure matched a different code")
	}
}

func TestHostDispatcher_SpawnsParkedPlanOnce(t *testing.T) {
	h := newHDHarness(t)
	r := h.addRun(nil)
	o1 := h.eval(t, r)
	o2 := h.eval(t, r)
	if !o1.Spawned || o1.StageType != "plan" {
		t.Fatalf("first evaluation = %+v; want plan spawned", o1)
	}
	if o2.Spawned || o2.Refusal != hdRefuseInFlight {
		t.Fatalf("second evaluation = %+v; want in_flight, no spawn", o2)
	}
	if h.spawner.n() != 1 {
		t.Fatalf("spawns = %d; want exactly 1", h.spawner.n())
	}
	if len(h.b.acts) != 1 || h.b.acts[0].Source != "host_dispatcher" || h.b.acts[0].Action != "dispatch_stage" || h.b.acts[0].Stage != "plan" {
		t.Fatalf("acts = %+v; want one dispatch_stage/plan act tagged host_dispatcher", h.b.acts)
	}
}

func TestHostDispatcher_NeverActsAtGates(t *testing.T) {
	h := newHDHarness(t)
	r := h.addRun(func(r *hdRun) {
		r.Stages = []Stage{
			{ID: uuid.NewString(), RunID: r.ID, Sequence: 1, Type: "plan", State: "awaiting_approval"},
			{ID: uuid.NewString(), RunID: r.ID, Sequence: 2, Type: "review", State: "awaiting_approval"},
		}
	})
	for i := 0; i < 10; i++ {
		if o := h.eval(t, r); o.Refusal != hdRefuseNothingDispatchable {
			t.Fatalf("eval %d = %+v; want nothing_dispatchable", i, o)
		}
	}
	gateish := h.b.count(func(q hdRequest) bool {
		return q.Method != http.MethodGet && (strings.Contains(q.Path, "/auto-drive") || strings.Contains(q.Path, "approv") ||
			strings.Contains(q.Path, "merge") || strings.Contains(q.Path, "scope-amendment") || strings.HasSuffix(q.Path, "/host-dispatch"))
	})
	if gateish != 0 || h.spawner.n() != 0 {
		t.Fatalf("gate/marker requests = %d, spawns = %d; want 0, 0", gateish, h.spawner.n())
	}
}

func TestHostDispatcher_MemoryPressureBacksOff(t *testing.T) {
	h := newHDHarness(t)
	r := h.addRun(nil)
	held := true
	h.b.marker = func(_, _ string) (int, string) {
		if held {
			return 409, apiErrBody("host_memory_pressure", "host memory critical")
		}
		return 200, `{"transitioned":true,"stage_state":"dispatched"}`
	}
	o := h.eval(t, r)
	if o.Spawned || o.Refusal != hdRefuseMemoryPressure || !o.StopMarkers {
		t.Fatalf("held round = %+v; want host_memory_pressure refusal with StopMarkers", o)
	}
	if h.spawner.n() != 0 || h.b.stageState(r.ID, r.Stages[0].ID) != "awaiting_host_dispatch" {
		t.Fatalf("held round spawned %d / stage %s; want 0 / awaiting_host_dispatch", h.spawner.n(), h.b.stageState(r.ID, r.Stages[0].ID))
	}
	if h.gate.releases != 1 {
		t.Fatalf("gate releases = %d; want the reservation released", h.gate.releases)
	}
	held = false
	if o := h.eval(t, r); !o.Spawned {
		t.Fatalf("admitted round = %+v; want a spawn", o)
	}
}

func TestHostDispatcher_RacingDispatchersSpawnOnce(t *testing.T) {
	h := newHDHarness(t)
	r := h.addRun(nil)
	won := false
	h.b.marker = func(_, _ string) (int, string) {
		if !won {
			won = true
			return 200, `{"transitioned":true,"stage_state":"dispatched"}`
		}
		return 200, `{"transitioned":false,"stage_state":"dispatched"}`
	}
	// A second dispatcher that read the stage parked before the first won.
	other := newHostDispatcher(h.d.r, h.d.binary, &hdGate{}, newHDCounter())
	other.spawn, other.environ = h.spawner.spawn, h.d.environ
	o1 := h.eval(t, r)
	h.b.setState(r, r.Stages[0].ID, "awaiting_host_dispatch") // the race: both read it parked
	o2 := other.evaluate(context.Background(), uuid.MustParse(r.ID))
	if !o1.Spawned || o2.Spawned || o2.Refusal != hdRefuseMarkerNotTransitioned {
		t.Fatalf("outcomes = %+v / %+v; want one spawn, then marker_not_transitioned", o1, o2)
	}
	if h.spawner.n() != 1 {
		t.Fatalf("spawns = %d; want exactly 1", h.spawner.n())
	}
}

func TestHostDispatcher_EligibilityIsTheMark(t *testing.T) {
	h := newHDHarness(t)
	webhook := h.addRun(func(r *hdRun) { r.AutoDispatch = false })
	upkeep := h.addRun(nil)
	if o := h.eval(t, webhook); o.Spawned || o.Refusal != hdRefuseNotEligible {
		t.Fatalf("unmarked run = %+v; want not_eligible", o)
	}
	if n := h.b.count(isMarkerReq); n != 0 {
		t.Fatalf("unmarked run drew %d marker POSTs; want 0", n)
	}
	if o := h.eval(t, upkeep); !o.Spawned {
		t.Fatalf("marked run = %+v; want spawned", o)
	}
	if h.spawner.n() != 1 || h.spawner.spawns[0].Run != upkeep.ID {
		t.Fatalf("spawns = %+v; want only the marked run", h.spawner.spawns)
	}
}

// F3: a verify-only re-open is eligible only through the mark. An UNMARKED
// run whose implement stage the merge-candidate verifier re-opened is never
// spawned; the same shape on a marked run is.
func TestHostDispatcher_UnmarkedVerifyReopenNotSpawned(t *testing.T) {
	h := newHDHarness(t)
	reopened := func(marked bool) func(*hdRun) {
		return func(r *hdRun) {
			r.AutoDispatch = marked
			r.Stages = []Stage{
				{ID: uuid.NewString(), RunID: r.ID, Sequence: 1, Type: "plan", State: "succeeded"},
				{ID: uuid.NewString(), RunID: r.ID, Sequence: 2, Type: "implement", State: "awaiting_host_dispatch", UpdatedAt: time.Now()},
			}
		}
	}
	unmarked := h.addRun(reopened(false))
	marked := h.addRun(reopened(true))
	if o := h.eval(t, unmarked); o.Spawned || o.Refusal != hdRefuseNotEligible {
		t.Fatalf("unmarked verify re-open = %+v; want not_eligible", o)
	}
	if n := h.b.count(func(q hdRequest) bool { return strings.Contains(q.Path, unmarked.ID) && q.Method == http.MethodPost }); n != 0 {
		t.Fatalf("unmarked verify re-open drew %d POSTs; want 0", n)
	}
	if o := h.eval(t, marked); !o.Spawned || o.StageType != "implement" {
		t.Fatalf("marked verify re-open = %+v; want implement spawned", o)
	}
}

func TestHostDispatcher_ChildEligibility(t *testing.T) {
	h := newHDHarness(t)
	zero := 0
	markedParent := h.addRun(func(r *hdRun) { r.Stages = nil })
	unmarkedParent := h.addRun(func(r *hdRun) { r.AutoDispatch = false; r.Stages = nil })
	child := func(parent string) func(*hdRun) {
		return func(r *hdRun) {
			r.AutoDispatch = false
			r.SliceIndex = &zero
			r.DecomposedFrom = parent
			r.Stages = []Stage{{ID: uuid.NewString(), RunID: r.ID, Sequence: 1, Type: "implement", State: "awaiting_host_dispatch"}}
		}
	}
	ok := h.addRun(child(markedParent.ID))
	no := h.addRun(child(unmarkedParent.ID))
	missing := h.addRun(child(uuid.NewString()))
	if o := h.eval(t, ok); !o.Spawned {
		t.Fatalf("child of marked parent = %+v; want spawned", o)
	}
	if o := h.eval(t, no); o.Spawned || o.Refusal != hdRefuseNotEligible {
		t.Fatalf("child of unmarked parent = %+v; want not_eligible", o)
	}
	h.b.parentCode = 500
	if o := h.eval(t, missing); o.Spawned || o.Refusal != hdRefuseParentUnreadable {
		t.Fatalf("child with unreadable parent = %+v; want parent_unreadable", o)
	}
	if h.spawner.n() != 1 {
		t.Fatalf("spawns = %d; want 1", h.spawner.n())
	}
}

func TestHostDispatcher_RefusesOutsideAllowedRoots(t *testing.T) {
	h := newHDHarness(t)
	outside := h.addRun(func(r *hdRun) { r.WorkingDir = t.TempDir() })
	empty := h.addRun(func(r *hdRun) { r.WorkingDir = "" })
	relative := h.addRun(func(r *hdRun) { r.WorkingDir = "relative/dir" })
	for _, r := range []*hdRun{outside, empty, relative} {
		if o := h.eval(t, r); o.Spawned || o.Refusal != hdRefusePathOutside {
			t.Fatalf("working_dir %q = %+v; want path_outside_allowed_roots", r.WorkingDir, o)
		}
	}
	if n := h.b.count(isMarkerReq); n != 0 || h.spawner.n() != 0 {
		t.Fatalf("marker POSTs = %d, spawns = %d; want 0, 0", n, h.spawner.n())
	}
	inside := h.addRun(func(r *hdRun) {
		r.WorkingDir = filepath.Join(h.root, "sub")
	})
	if err := os.MkdirAll(inside.WorkingDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if o := h.eval(t, inside); !o.Spawned {
		t.Fatalf("working_dir inside the root = %+v; want spawned", o)
	}
}

func TestHostDispatcher_QueuedStageStaysParked(t *testing.T) {
	h := newHDHarness(t)
	r := h.addRun(nil)
	queued := true
	h.b.marker = func(_, _ string) (int, string) {
		if queued {
			return 409, `{"error":{"code":"concurrency_slot_queued","message":"queued","details":{"group":"local-implement:devbox","limit":1,"position":1}}}`
		}
		return 200, `{"transitioned":true,"stage_state":"dispatched"}`
	}
	o := h.eval(t, r)
	if o.Spawned || o.Refusal != hdRefuseSlotQueued || !o.QueueEpisode {
		t.Fatalf("queued attempt = %+v; want concurrency_slot_queued with a live queue episode", o)
	}
	if h.spawner.n() != 0 || h.b.stageState(r.ID, r.Stages[0].ID) != "awaiting_host_dispatch" || len(h.b.acts) != 1 {
		t.Fatalf("spawns=%d stage=%s acts=%d; want 0, awaiting_host_dispatch, 1", h.spawner.n(), h.b.stageState(r.ID, r.Stages[0].ID), len(h.b.acts))
	}
	queued = false
	if o := h.eval(t, r); !o.Spawned {
		t.Fatalf("admitted re-attempt = %+v; want spawned", o)
	}
	if len(h.b.acts) != 1 || h.spawner.n() != 1 {
		t.Fatalf("acts=%d spawns=%d; want ONE row for the episode and one spawn", len(h.b.acts), h.spawner.n())
	}
}

func TestHostDispatchSpawnable_DispatchedVsFresh(t *testing.T) {
	for state, want := range map[string]bool{"dispatched": false, "pending": true, "awaiting_host_dispatch": true} {
		if got := hostDispatchSpawnable(Stage{State: state}); got != want {
			t.Errorf("hostDispatchSpawnable(%s) = %v; want %v", state, got, want)
		}
	}
}

func staleRun(h *hdHarness, age time.Duration) *hdRun {
	return h.addRun(func(r *hdRun) {
		r.Stages = []Stage{
			{ID: uuid.NewString(), RunID: r.ID, Sequence: 1, Type: "plan", State: "dispatched", UpdatedAt: time.Now().Add(-age)},
		}
	})
}

func TestHostDispatcher_StaleRespawnCappedAtOne(t *testing.T) {
	h := newHDHarness(t)
	r := staleRun(h, time.Hour)
	o := h.eval(t, r)
	if !o.Spawned || !o.Respawn {
		t.Fatalf("first dead observation = %+v; want one re-spawn", o)
	}
	if len(h.b.acts) != 1 || h.b.acts[0].Note != staleRedispatchNote {
		t.Fatalf("acts = %+v; want one act carrying the stale re-dispatch note", h.b.acts)
	}
	// A restarted dispatcher (empty running set) over the same counter and the
	// same marker epoch.
	restarted := newHostDispatcher(h.d.r, h.d.binary, h.gate, h.d.respawns)
	restarted.spawn, restarted.probe, restarted.environ = h.spawner.spawn, h.d.probe, h.d.environ
	for i := 0; i < 5; i++ {
		if o := restarted.evaluate(context.Background(), uuid.MustParse(r.ID)); o.Spawned || o.Refusal != hdRefuseStaleRespawnCap {
			t.Fatalf("eval %d after the re-spawn = %+v; want stale_respawn_cap_reached", i, o)
		}
	}
	if h.spawner.n() != 1 {
		t.Fatalf("spawns = %d; want exactly 1 re-spawn", h.spawner.n())
	}
}

func TestHostDispatcher_StaleProbeLiveOrUnknownNeverSpawns(t *testing.T) {
	for verdict, code := range map[runnerLivenessVerdict]string{runnerLive: hdRefuseStaleRunnerLive, runnerUnknown: hdRefuseStaleRunnerUnknown} {
		h := newHDHarness(t)
		h.d.probe = func(context.Context, string) runnerLivenessVerdict { return verdict }
		r := staleRun(h, time.Hour)
		if o := h.eval(t, r); o.Spawned || o.Refusal != code {
			t.Fatalf("probe %v = %+v; want %s", verdict, o, code)
		}
		if h.b.count(isMarkerReq) != 0 {
			t.Fatalf("probe %v drew a marker POST", verdict)
		}
	}
	h := newHDHarness(t)
	fresh := staleRun(h, time.Second)
	if o := h.eval(t, fresh); o.Spawned || o.Refusal != hdRefuseInFlight {
		t.Fatalf("dispatched within the threshold = %+v; want in_flight", o)
	}
}

// F7(a): an in-process early exit yields no re-spawn. While this process
// lives, a stage it spawned is never re-spawned, even past the threshold with
// a dead probe: its own detached reaper reports the exit (a failed stage).
func TestHostDispatcher_InProcessEarlyExitNoRespawn(t *testing.T) {
	h := newHDHarness(t)
	r := h.addRun(nil)
	if o := h.eval(t, r); !o.Spawned {
		t.Fatalf("first evaluation = %+v; want a spawn", o)
	}
	h.b.mu.Lock()
	r.Stages[0].UpdatedAt = time.Now().Add(-time.Hour) // still 'dispatched', past the threshold
	h.b.mu.Unlock()
	if o := h.eval(t, r); o.Spawned || o.Refusal != hdRefuseInFlight {
		t.Fatalf("own runner past the threshold = %+v; want in_flight", o)
	}
	h.b.setState(r, r.Stages[0].ID, "failed") // the reaper's report
	if o := h.eval(t, r); o.Spawned || o.Refusal != hdRefuseNothingDispatchable {
		t.Fatalf("failed stage = %+v; want nothing_dispatchable", o)
	}
	if h.spawner.n() != 1 {
		t.Fatalf("spawns = %d; want 1", h.spawner.n())
	}
}

func TestHostDispatcher_NoOriginatedRetries(t *testing.T) {
	h := newHDHarness(t)
	r := h.addRun(func(r *hdRun) {
		r.Stages = []Stage{
			{ID: uuid.NewString(), RunID: r.ID, Sequence: 1, Type: "plan", State: "succeeded"},
			{ID: uuid.NewString(), RunID: r.ID, Sequence: 2, Type: "implement", State: "failed"},
			{ID: uuid.NewString(), RunID: r.ID, Sequence: 3, Type: "review", State: "pending"},
			{ID: uuid.NewString(), RunID: r.ID, Sequence: 4, Type: "acceptance", State: "pending"},
		}
	})
	// The marker admits ANY stage, so only the selection can prevent a spawn.
	h.b.marker = func(_, _ string) (int, string) { return 200, `{"transitioned":true,"stage_state":"dispatched"}` }
	for i := 0; i < 5; i++ {
		if o := h.eval(t, r); o.Spawned {
			t.Fatalf("eval %d spawned a failed stage: %+v", i, o)
		}
	}
	if n := h.b.count(isMarkerReq); n != 0 || h.spawner.n() != 0 {
		t.Fatalf("marker POSTs = %d, spawns = %d; want 0, 0", n, h.spawner.n())
	}
	h.b.setState(r, r.Stages[1].ID, "pending") // a fix-up re-opened it
	if o := h.eval(t, r); !o.Spawned || o.StageType != "implement" {
		t.Fatalf("after the fix-up = %+v; want implement spawned", o)
	}
}

func TestHostDispatcher_PauseBeforeMarker(t *testing.T) {
	h := newHDHarness(t)
	r := h.addRun(nil)
	h.gate.onAdmit = func() string {
		// The campaign read already ran; the pause lands now.
		if h.b.count(isLinkReq) == 0 {
			t.Error("gate consulted before the campaign-link read")
		}
		return "paused"
	}
	if o := h.eval(t, r); o.Spawned || o.Refusal != "paused" {
		t.Fatalf("paused = %+v; want the gate's refusal", o)
	}
	if h.b.count(isRecordReq) != 0 || h.b.count(isMarkerReq) != 0 || h.b.stageState(r.ID, r.Stages[0].ID) != "awaiting_host_dispatch" {
		t.Fatalf("paused tick committed: records=%d markers=%d stage=%s", h.b.count(isRecordReq), h.b.count(isMarkerReq), h.b.stageState(r.ID, r.Stages[0].ID))
	}
}

func TestHostDispatcher_NilGateFailsClosed(t *testing.T) {
	h := newHDHarness(t)
	h.d.gate = nil
	r := h.addRun(nil)
	if o := h.eval(t, r); o.Spawned || o.Refusal != hdRefuseGateUnconfigured {
		t.Fatalf("nil gate = %+v; want gate_unconfigured", o)
	}
	if h.b.count(isMarkerReq) != 0 {
		t.Fatal("nil gate drew a marker POST")
	}
}

func childHarness(t *testing.T, base string) (*hdHarness, *hdRun, []*hdRun) {
	h := newHDHarness(t)
	parent := h.addRun(func(r *hdRun) { r.Stages = nil })
	var kids []*hdRun
	for i := 0; i < 2; i++ {
		idx := i
		kids = append(kids, h.addRun(func(r *hdRun) {
			r.AutoDispatch = false
			r.SliceIndex = &idx
			r.DecomposedFrom = parent.ID
			r.Stages = []Stage{{ID: uuid.NewString(), RunID: r.ID, Sequence: 1, Type: "implement", State: "awaiting_host_dispatch"}}
		}))
	}
	h.b.marker = func(_, _ string) (int, string) {
		return 200, fmt.Sprintf(`{"transitioned":true,"stage_state":"dispatched","base_branch":%q}`, base)
	}
	return h, parent, kids
}

func TestHostDispatcher_ChildArgvMatchesRunChildren(t *testing.T) {
	h, parent, kids := childHarness(t, "")
	_ = h
	base := "fishhawk/run-" + parent.ID
	h.b.marker = func(_, _ string) (int, string) {
		return 200, fmt.Sprintf(`{"transitioned":true,"stage_state":"dispatched","base_branch":%q}`, base)
	}
	kid := kids[0]
	if o := h.eval(t, kid); !o.Spawned {
		t.Fatalf("child = %+v; want spawned", o)
	}
	want := childRunnerArgv(kid.ID, kid.Stages[0].ID, h.b.srv.URL, "feature_change", kid.WorkingDir, "acme/widgets",
		runForgeTarget{Forge: startRunForgeGitHub, Repo: "acme/widgets", DefaultBranch: "main"}, "", base)
	got := h.spawner.spawns[0].Argv
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("child argv\n got %v\nwant %v", got, want)
	}
	for _, flag := range []string{"--base-branch", "--check-base-ref"} {
		if argvValue(got, flag) != base {
			t.Fatalf("%s = %q; want %q", flag, argvValue(got, flag), base)
		}
	}
	if !argvHas(got, "--parallel-isolate") {
		t.Fatal("child argv lacks --parallel-isolate")
	}
}

func TestHostDispatcher_SameWaveSiblingsIsolated(t *testing.T) {
	h, _, kids := childHarness(t, "")
	for _, k := range kids {
		if o := h.eval(t, k); !o.Spawned {
			t.Fatalf("sibling %s = %+v; want spawned", k.ID, o)
		}
	}
	if h.spawner.n() != 2 {
		t.Fatalf("spawns = %d; want 2", h.spawner.n())
	}
	a, b := h.spawner.spawns[0].Argv, h.spawner.spawns[1].Argv
	if argvValue(a, "--run-id") == argvValue(b, "--run-id") {
		t.Fatalf("siblings share --run-id %q", argvValue(a, "--run-id"))
	}
	if argvValue(a, "--run-id") != kids[0].ID || argvValue(b, "--run-id") != kids[1].ID {
		t.Fatalf("--run-id values %q/%q; want each child's own id", argvValue(a, "--run-id"), argvValue(b, "--run-id"))
	}
	if argvValue(a, "--working-dir") != argvValue(b, "--working-dir") || !argvHas(a, "--parallel-isolate") || !argvHas(b, "--parallel-isolate") {
		t.Fatal("siblings must share the root and both carry --parallel-isolate")
	}
	if argvValue(a, "--base-branch") != "main" {
		t.Fatalf("an empty marker base_branch must keep the resolved base; got %q", argvValue(a, "--base-branch"))
	}
}

func argvValue(argv []string, flag string) string {
	for i := 0; i+1 < len(argv); i++ {
		if argv[i] == flag {
			return argv[i+1]
		}
	}
	return ""
}

func argvHas(argv []string, flag string) bool {
	for _, a := range argv {
		if a == flag {
			return true
		}
	}
	return false
}

func TestHostDispatcher_RecordFailsClosed(t *testing.T) {
	h := newHDHarness(t)
	h.b.recordCode = 500
	r := h.addRun(nil)
	if o := h.eval(t, r); o.Spawned || o.Refusal != hdRefuseRecordFailed {
		t.Fatalf("record 500 = %+v; want record_act_failed", o)
	}
	if h.b.count(isMarkerReq) != 0 || h.spawner.n() != 0 || h.gate.releases != 1 {
		t.Fatalf("markers=%d spawns=%d releases=%d; want 0, 0, 1", h.b.count(isMarkerReq), h.spawner.n(), h.gate.releases)
	}
}

func acceptanceRun(h *hdHarness) *hdRun {
	return h.addRun(func(r *hdRun) {
		r.Stages = []Stage{
			{ID: uuid.NewString(), RunID: r.ID, Sequence: 1, Type: "plan", State: "succeeded"},
			{ID: uuid.NewString(), RunID: r.ID, Sequence: 2, Type: "implement", State: "succeeded"},
			{ID: uuid.NewString(), RunID: r.ID, Sequence: 3, Type: "review", State: "succeeded"},
			{ID: uuid.NewString(), RunID: r.ID, Sequence: 4, Type: "acceptance", State: "awaiting_host_dispatch"},
		}
	})
}

func TestHostDispatcher_AcceptanceAutoPreview(t *testing.T) {
	h := newHDHarness(t)
	r := acceptanceRun(h)
	if o := h.eval(t, r); !o.Spawned || o.StageType != "acceptance" {
		t.Fatalf("acceptance = %+v; want spawned", o)
	}
	env := strings.Join(h.spawner.spawns[0].Env, "\n")
	if !strings.Contains(env, acceptancePreviewCmdEnv+"=") || !strings.Contains(env, acceptancePreviewTeardownCmdEnv+"=") {
		t.Fatalf("acceptance env lacks the auto-preview pair:\n%s", env)
	}
	if h.b.count(func(q hdRequest) bool { return strings.HasSuffix(q.Path, "/acceptance-admission") }) != 1 {
		t.Fatal("acceptance admission was not consulted")
	}
}

func TestHostDispatcher_AcceptanceShortCircuitNoSpawn(t *testing.T) {
	h := newHDHarness(t)
	h.b.admission = `{"short_circuited":true,"kind":"not_validated"}`
	r := acceptanceRun(h)
	if o := h.eval(t, r); o.Spawned || o.Refusal != hdRefuseAcceptanceShortCircut {
		t.Fatalf("short-circuited acceptance = %+v; want acceptance_short_circuited", o)
	}
	if h.b.count(isMarkerReq) != 0 || h.b.count(isRecordReq) != 0 || h.gate.releases != 1 {
		t.Fatal("a short-circuited acceptance must record nothing, POST no marker and release the reservation")
	}
}

func TestHostDispatcher_RunnerEnvExcludesDispatcherToken(t *testing.T) {
	h := newHDHarness(t)
	t.Setenv("FISHHAWK_API_TOKEN", hdTestBearer)
	t.Setenv("FISHHAWK_HD_TEST_ALIAS", hdTestBearer)
	h.d.environ = os.Environ
	r := h.addRun(nil)
	if o := h.eval(t, r); !o.Spawned {
		t.Fatalf("eval = %+v; want spawned", o)
	}
	for _, kv := range h.spawner.spawns[0].Env {
		if strings.HasPrefix(kv, "FISHHAWK_API_TOKEN=") || strings.Contains(kv, hdTestBearer) {
			t.Fatalf("runner env carries the dispatcher bearer: %q", kv)
		}
	}
}

func TestHostDispatcher_CampaignGateFailsClosed(t *testing.T) {
	h := newHDHarness(t)
	h.b.marker = func(_, _ string) (int, string) { return 200, `{"transitioned":true,"stage_state":"dispatched"}` }
	cases := []struct {
		name string
		mut  func(*hdRun)
		code string
	}{
		{"cancelled", func(r *hdRun) { r.Link = hdLinked("cancelled") }, hdRefuseCampaignNotRunning},
		{"paused", func(r *hdRun) { r.Link = hdLinked("paused") }, hdRefuseCampaignNotRunning},
		{"unresolved", func(r *hdRun) {
			r.Link = &runCampaignLink{Status: "unresolved", Reason: "missing_parent", Campaigns: []runCampaignLinkEntry{}}
		}, hdRefuseCampaignUnresolved},
		{"linked-empty", func(r *hdRun) { r.Link = &runCampaignLink{Status: "linked", Campaigns: []runCampaignLinkEntry{}} }, hdRefuseCampaignUnresolved},
		{"read-503", func(r *hdRun) { r.LinkCode = 503 }, hdRefuseCampaignUnavailable},
		{"absent-block", func(r *hdRun) { r.Link = nil }, hdRefuseCampaignUnavailable},
	}
	for _, c := range cases {
		r := h.addRun(c.mut)
		o := h.eval(t, r)
		if o.Spawned || o.Refusal != c.code || !o.CampaignRefused {
			t.Fatalf("%s = %+v; want %s (campaign refusal)", c.name, o, c.code)
		}
		if n := h.b.count(func(q hdRequest) bool { return q.Method == http.MethodPost && strings.Contains(q.Path, r.ID) }); n != 0 {
			t.Fatalf("%s drew %d POSTs; want 0", c.name, n)
		}
	}
	running := h.addRun(func(r *hdRun) { r.Link = hdLinked("running") })
	unlinked := h.addRun(nil)
	for _, r := range []*hdRun{running, unlinked} {
		if o := h.eval(t, r); !o.Spawned {
			t.Fatalf("permitted run = %+v; want spawned", o)
		}
	}
	if h.spawner.n() != 2 {
		t.Fatalf("spawns = %d; want 2", h.spawner.n())
	}
}

func TestHostDispatcher_TerminalAndNonLocalRunsSkipped(t *testing.T) {
	h := newHDHarness(t)
	cancelled := h.addRun(func(r *hdRun) { r.State = "cancelled" })
	remote := h.addRun(func(r *hdRun) { r.RunnerKind = "github_actions" })
	if o := h.eval(t, cancelled); o.Refusal != hdRefuseRunTerminal {
		t.Fatalf("cancelled = %+v; want run_terminal", o)
	}
	if o := h.eval(t, remote); o.Refusal != hdRefuseNotLocal {
		t.Fatalf("github_actions = %+v; want runner_kind_not_local", o)
	}
	if h.b.count(isMarkerReq) != 0 || h.spawner.n() != 0 {
		t.Fatal("a terminal or non-local run drew a marker or a spawn")
	}
}

func TestHostDispatcher_SpawnFailureReportsCategoryC(t *testing.T) {
	h := newHDHarness(t)
	h.spawner.err = errors.New("exec: no such file")
	r := h.addRun(nil)
	if o := h.eval(t, r); o.Spawned || o.Refusal != hdRefuseSpawnFailed {
		t.Fatalf("spawn error = %+v; want spawn_failed", o)
	}
	if len(h.b.reaps) != 1 || h.gate.releases != 1 {
		t.Fatalf("reaps=%v releases=%d; want one category-C report and the reservation released", h.b.reaps, h.gate.releases)
	}
	h2 := newHDHarness(t)
	h2.spawner.err = fmt.Errorf("wrap: %w", errRunCancelledBeforeSpawn)
	r2 := h2.addRun(nil)
	if o := h2.eval(t, r2); o.Refusal != hdRefuseSpawnCancelled || len(h2.b.reaps) != 0 {
		t.Fatalf("cancel-refused spawn = %+v reaps=%v; want spawn_refused_run_cancelled and no report", o, h2.b.reaps)
	}
}

func TestHostDispatcherRunnerEnv(t *testing.T) {
	got := hostDispatchRunnerEnv([]string{"PATH=/bin", "FISHHAWK_API_TOKEN=x", "ALIAS=secret", "KEEP=1"}, "secret")
	if want := []string{"PATH=/bin", "KEEP=1"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("env = %v; want %v", got, want)
	}
}

// racingCounter models a second dispatcher process consuming the epoch between
// the early Allowed check and the pre-spawn Reserve: Reserve is the binding
// layer, isolated here.
type racingCounter struct{}

func (racingCounter) Allowed(string, time.Time) bool          { return true }
func (racingCounter) Reserve(string, time.Time) (bool, error) { return false, nil }

func TestHostDispatcher_StaleRespawnReserveIsBinding(t *testing.T) {
	h := newHDHarness(t)
	h.d.respawns = racingCounter{}
	r := staleRun(h, time.Hour)
	if o := h.eval(t, r); o.Spawned || o.Refusal != hdRefuseStaleRespawnCap {
		t.Fatalf("lost Reserve = %+v; want stale_respawn_cap_reached", o)
	}
	if h.b.count(isRecordReq) != 0 || h.b.count(isMarkerReq) != 0 || h.gate.releases != 1 {
		t.Fatalf("records=%d markers=%d releases=%d; want 0, 0, 1", h.b.count(isRecordReq), h.b.count(isMarkerReq), h.gate.releases)
	}
}
