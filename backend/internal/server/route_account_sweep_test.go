package server

// route_account_sweep_test.go — the account-isolation ROUTE SWEEP (#4211).
//
// authz_account_test.go pins enforceAccount's per-mode logic by calling each
// require{Run,Stage,Concern}Account wrapper directly; handler unit tests call
// handlers directly and bypass the wrapper. Neither notices a route that is
// registered WITHOUT its wrapper, with the wrong wrapper, or at the wrong
// tier. This file pins that WIRING for every run-scoped route, present and
// future, by enumerating registerRoutes through the routeRegistrar seam
// (handlers.go) rather than a hand-listed route table:
//
//   - TestRunScopedRoutes_CrossAccountBearer_Forbidden drives every
//     {run_id}/{stage_id}/{concern_id} pattern through the full s.Handler()
//     chain with a bearer bound to account B against a run owned by account A,
//     and requires 403 account_forbidden with no handler side effect.
//   - TestRunScopedRoutes_AccessTierPinned infers each route's accountTier by
//     probing its captured wrapped handler and compares it with the pinned
//     expectation (GET → readAccess, any other method → memberWrite, except
//     the adminWriteRoutes set).
//
// No 404 is admitted. enforceAccount answers a RESOLVED foreign run with 403
// account_forbidden on every tier, and the wrappers fall through to the
// handler only when the run cannot be resolved, which never happens for the
// seeded run. No run-scoped route hides existence at the wrapper; a design
// that wants existence-hiding must change enforceAccount and this test
// together.
//
// Residual: the recorder sees only routes registered inside registerRoutes.
// A route mounted on the mux anywhere else escapes the sweep (the
// routeRegistrar doc comment states the rule).

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/account"
	"github.com/kuhlman-labs/fishhawk/backend/internal/apitoken"
	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/concern"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

// Route families, keyed by the first path segment after /v0/. Each family
// maps to one wrapper: runs → requireRunAccount, stages → requireStageAccount,
// concerns → requireConcernAccount.
const (
	sweepFamilyRuns     = "runs"
	sweepFamilyStages   = "stages"
	sweepFamilyConcerns = "concerns"
)

// sweepFamilyFloors is the minimum number of routes each family must
// enumerate: the counts registered at #4211. A floor is raised by hand when
// routes land and never lowered silently — a drop means a route vanished or
// the enumeration broke, and the per-route loop would otherwise pass
// vacuously.
var sweepFamilyFloors = map[string]int{
	sweepFamilyRuns:     56,
	sweepFamilyStages:   12,
	sweepFamilyConcerns: 2,
}

// accountIsolationOptOuts lists run-scoped patterns deliberately exempt from
// the sweep, each with a REVIEWED reason. Empty today: every run-scoped route
// is account-wrapped. Adding an entry is the reviewed act that exempts a
// route; an entry naming a pattern that is no longer registered fails the
// sweep, so the list cannot go stale.
var accountIsolationOptOuts = map[string]string{}

// adminWriteRoutes is every run-scoped route registered at the adminWrite
// tier (the destructive/admin surface). Every other route's expected tier
// follows from its method: GET → readAccess, anything else → memberWrite.
// Moving a route between tiers must update this set — the reviewed act the
// operator's admin-vs-member founder decision requires. An entry naming a
// pattern that is no longer registered fails the sweep.
var adminWriteRoutes = map[string]bool{
	"POST /v0/runs/{run_id}/cancel":                         true,
	"POST /v0/runs/{run_id}/recover":                        true,
	"POST /v0/runs/{run_id}/redrive":                        true,
	"POST /v0/runs/{run_id}/revive":                         true,
	"POST /v0/runs/{run_id}/reset-branch":                   true,
	"POST /v0/runs/{run_id}/rebase-branch":                  true,
	"POST /v0/runs/{run_id}/reviews/reconcile":              true,
	"POST /v0/runs/{run_id}/stages/{stage_id}/reap-failure": true,
	"POST /v0/runs/{run_id}/signing-key":                    true,
	"POST /v0/runs/{run_id}/deployment/rollback":            true,
	"POST /v0/runs/{run_id}/installation-token":             true,
	"POST /v0/runs/{run_id}/mcp-token":                      true,
}

// sweepRoute is one registered pattern with the exact handler registerRoutes
// handed the mux for it.
type sweepRoute struct {
	pattern string
	method  string
	path    string
	family  string
	handler http.HandlerFunc
}

// recordingRegistrar is the routeRegistrar the sweep hands registerRoutes: it
// records every pattern and its handler instead of mounting it.
type recordingRegistrar struct {
	routes []sweepRoute
}

func (r *recordingRegistrar) HandleFunc(pattern string, handler func(http.ResponseWriter, *http.Request)) {
	method, path, ok := strings.Cut(pattern, " ")
	if !ok {
		method, path = "", pattern
	}
	r.routes = append(r.routes, sweepRoute{pattern: pattern, method: method, path: path, handler: handler})
}

// sweepScopedPlaceholders are the path wildcards that make a route
// run-scoped, and therefore swept.
var sweepScopedPlaceholders = map[string]bool{"run_id": true, "stage_id": true, "concern_id": true}

// pathPlaceholders returns the wildcard names in a pattern path, in order
// ("{name}" and "{name...}" both yield name).
func pathPlaceholders(path string) []string {
	var out []string
	for _, seg := range strings.Split(path, "/") {
		if strings.HasPrefix(seg, "{") && strings.HasSuffix(seg, "}") {
			out = append(out, strings.TrimSuffix(strings.TrimSuffix(strings.TrimPrefix(seg, "{"), "}"), "..."))
		}
	}
	return out
}

// sweptRoutes enumerates every run-scoped route s registers. A scoped
// pattern outside the three known families, or without a method, fails
// closed: a new family or shape forces an explicit decision here.
func sweptRoutes(t *testing.T, s *Server) []sweepRoute {
	t.Helper()
	rec := &recordingRegistrar{}
	s.registerRoutes(rec)
	var out []sweepRoute
	for _, rt := range rec.routes {
		scoped := false
		for _, name := range pathPlaceholders(rt.path) {
			if sweepScopedPlaceholders[name] {
				scoped = true
			}
		}
		if !scoped {
			continue
		}
		if rt.method == "" {
			t.Fatalf("run-scoped pattern %q registers no method; the sweep cannot pick a request method or an expected tier for it", rt.pattern)
		}
		rest, ok := strings.CutPrefix(rt.path, "/v0/")
		family, _, _ := strings.Cut(rest, "/")
		switch {
		case ok && (family == sweepFamilyRuns || family == sweepFamilyStages || family == sweepFamilyConcerns):
			rt.family = family
		default:
			t.Fatalf("run-scoped pattern %q is outside the swept families (/v0/runs, /v0/stages, /v0/concerns); extend the sweep with its wrapper's resolution sequence", rt.pattern)
		}
		out = append(out, rt)
	}
	return out
}

// assertSweepGuards fails closed when the enumeration is too small to be
// meaningful or a reviewed list has gone stale.
func assertSweepGuards(t *testing.T, routes []sweepRoute) {
	t.Helper()
	counts := map[string]int{}
	registered := map[string]bool{}
	for _, rt := range routes {
		counts[rt.family]++
		registered[rt.pattern] = true
	}
	for _, family := range []string{sweepFamilyRuns, sweepFamilyStages, sweepFamilyConcerns} {
		if counts[family] < sweepFamilyFloors[family] {
			t.Fatalf("route family %q enumerated %d run-scoped routes, below its floor %d: a route vanished or the registerRoutes enumeration broke",
				family, counts[family], sweepFamilyFloors[family])
		}
	}
	for pattern, reason := range accountIsolationOptOuts {
		if !registered[pattern] {
			t.Errorf("accountIsolationOptOuts names %q, which is not a registered run-scoped route; remove the stale entry", pattern)
		}
		if strings.TrimSpace(reason) == "" {
			t.Errorf("accountIsolationOptOuts entry %q carries no reason; every opt-out needs a reviewed reason", pattern)
		}
	}
	for pattern := range adminWriteRoutes {
		if !registered[pattern] {
			t.Errorf("adminWriteRoutes names %q, which is not a registered run-scoped route; remove the stale entry", pattern)
		}
	}
}

// sweepIDs are the seeded identifiers a concrete request path carries.
type sweepIDs struct {
	run, stage, concern uuid.UUID
}

// concretePath substitutes every placeholder in rt's path and returns the
// concrete path plus the path values the mux would set. An unrecognised
// placeholder fails closed, naming the pattern and the placeholder.
func concretePath(t *testing.T, rt sweepRoute, ids sweepIDs) (string, map[string]string) {
	t.Helper()
	vals := map[string]string{}
	segs := strings.Split(rt.path, "/")
	for i, seg := range segs {
		if !strings.HasPrefix(seg, "{") || !strings.HasSuffix(seg, "}") {
			continue
		}
		name := strings.TrimSuffix(strings.TrimPrefix(seg, "{"), "}")
		var v string
		switch name {
		case "run_id":
			v = ids.run.String()
		case "stage_id":
			v = ids.stage.String()
		case "concern_id":
			v = ids.concern.String()
		case "amendment_id":
			v = uuid.NewString()
		case "sequence":
			v = "1"
		default:
			t.Fatalf("pattern %q carries placeholder {%s}, which the sweep cannot fill; add a value for it to concretePath", rt.pattern, name)
		}
		segs[i] = v
		vals[name] = v
	}
	return strings.Join(segs, "/"), vals
}

func setPathValues(req *http.Request, vals map[string]string) {
	for k, v := range vals {
		req.SetPathValue(k, v)
	}
}

// --- Cross-account sweep fixtures ---

// acctSweepRunRepo logs every GetRun / GetStage so the sweep can require the log
// to equal the wrapper's own resolution sequence exactly: any further read
// means the handler body ran. Every other method delegates to fakeRepo.
type acctSweepRunRepo struct {
	*fakeRepo
	logMu sync.Mutex
	calls []string
}

func (r *acctSweepRunRepo) GetRun(ctx context.Context, id uuid.UUID) (*run.Run, error) {
	r.record("GetRun(" + id.String() + ")")
	return r.fakeRepo.GetRun(ctx, id)
}

func (r *acctSweepRunRepo) GetStage(ctx context.Context, id uuid.UUID) (*run.Stage, error) {
	r.record("GetStage(" + id.String() + ")")
	return r.fakeRepo.GetStage(ctx, id)
}

func (r *acctSweepRunRepo) record(call string) {
	r.logMu.Lock()
	defer r.logMu.Unlock()
	r.calls = append(r.calls, call)
}

func (r *acctSweepRunRepo) takeLog() []string {
	r.logMu.Lock()
	defer r.logMu.Unlock()
	out := r.calls
	r.calls = nil
	return out
}

// sweepAuditRepo counts appends. Any other audit method hits the nil embedded
// interface and panics; the recovery middleware turns that into a logged 500,
// which the sweep's ERROR-log assertion catches.
type sweepAuditRepo struct {
	audit.Repository
	mu      sync.Mutex
	appends int
}

func (a *sweepAuditRepo) bump() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.appends++
}

func (a *sweepAuditRepo) Append(context.Context, audit.AppendParams) (*audit.Entry, error) {
	a.bump()
	return &audit.Entry{ID: uuid.New()}, nil
}

func (a *sweepAuditRepo) AppendChained(context.Context, audit.ChainAppendParams) (*audit.Entry, error) {
	a.bump()
	return &audit.Entry{ID: uuid.New()}, nil
}

func (a *sweepAuditRepo) AppendGlobalChained(context.Context, audit.GlobalChainAppendParams) (*audit.Entry, error) {
	a.bump()
	return &audit.Entry{ID: uuid.New()}, nil
}

func (a *sweepAuditRepo) take() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	n := a.appends
	a.appends = 0
	return n
}

// sweepLogCapture is a slog.Handler that keeps ERROR-level records.
type sweepLogCapture struct {
	mu     sync.Mutex
	errors []string
}

func (h *sweepLogCapture) Enabled(context.Context, slog.Level) bool { return true }

func (h *sweepLogCapture) Handle(_ context.Context, rec slog.Record) error {
	if rec.Level < slog.LevelError {
		return nil
	}
	var b strings.Builder
	b.WriteString(rec.Message)
	rec.Attrs(func(a slog.Attr) bool {
		fmt.Fprintf(&b, " %s=%v", a.Key, a.Value)
		return true
	})
	h.mu.Lock()
	defer h.mu.Unlock()
	h.errors = append(h.errors, b.String())
	return nil
}

func (h *sweepLogCapture) WithAttrs([]slog.Attr) slog.Handler { return h }
func (h *sweepLogCapture) WithGroup(string) slog.Handler      { return h }

func (h *sweepLogCapture) take() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := h.errors
	h.errors = nil
	return out
}

const sweepBearerB = "fhk_sweep_account_b_token"

type sweepFixture struct {
	s     *Server
	runs  *acctSweepRunRepo
	audit *sweepAuditRepo
	logs  *sweepLogCapture
	ids   sweepIDs
}

// newSweepFixture seeds run A (account A) with one stage S and one concern C,
// and wires a bearer token bound to account B with NO scopes: a handler
// reached by mistake refuses on scope (or answers) with a code that is never
// account_forbidden.
func newSweepFixture(t *testing.T) *sweepFixture {
	t.Helper()
	fr := newFakeRepo()
	runA := &run.Run{ID: uuid.New(), Repo: "acme/app", WorkflowID: "feature_change", AccountID: authzAcctA, State: run.StatePending, CreatedAt: time.Now().UTC()}
	fr.runs[runA.ID] = runA
	stage := &run.Stage{ID: uuid.New(), RunID: runA.ID}
	fr.stagesByRun[runA.ID] = []*run.Stage{stage}
	cr := newFakeConcernRepo()
	cr.rows = []*concern.Concern{{ID: uuid.New(), RunID: runA.ID}}
	tokens := &stubAPITokenRepo{tok: &apitoken.Token{
		ID:        uuid.New(),
		Subject:   "github:op",
		AccountID: authzAcctB,
		PlainText: sweepBearerB,
	}}
	fx := &sweepFixture{
		runs:  &acctSweepRunRepo{fakeRepo: fr},
		audit: &sweepAuditRepo{},
		logs:  &sweepLogCapture{},
		ids:   sweepIDs{run: runA.ID, stage: stage.ID, concern: cr.rows[0].ID},
	}
	fx.s = New(Config{
		Addr:         "127.0.0.1:0",
		RunRepo:      fx.runs,
		APITokenRepo: tokens,
		ConcernRepo:  cr,
		AuditRepo:    fx.audit,
		Logger:       slog.New(fx.logs),
	})
	return fx
}

// wantResolution is the exact run-repository read sequence the family's
// wrapper performs to resolve the run, and nothing more.
func (fx *sweepFixture) wantResolution(family string) []string {
	getRun := "GetRun(" + fx.ids.run.String() + ")"
	if family == sweepFamilyStages {
		return []string{"GetStage(" + fx.ids.stage.String() + ")", getRun}
	}
	// runs: GetRun directly; concerns: GetRun after the concern lookup.
	return []string{getRun}
}

// TestRunScopedRoutes_CrossAccountBearer_Forbidden sends a bearer bound to
// account B to every enumerated run-scoped route of a run owned by account A,
// through the full s.Handler() chain, and requires: 403 with exactly one
// error envelope whose code is account_forbidden (no trailing bytes), a
// run-repository read log equal to the wrapper's own resolution sequence (a
// further read means the handler body ran), zero audit appends and zero
// ERROR-level log records. Each concrete request is first checked to route to
// the intended pattern on a real ServeMux, so a shadowing route cannot stand
// in for it.
func TestRunScopedRoutes_CrossAccountBearer_Forbidden(t *testing.T) {
	fx := newSweepFixture(t)
	routes := sweptRoutes(t, fx.s)
	assertSweepGuards(t, routes)

	mux := http.NewServeMux()
	fx.s.registerRoutes(mux)
	h := fx.s.Handler()

	for _, rt := range routes {
		if _, opted := accountIsolationOptOuts[rt.pattern]; opted {
			continue
		}
		t.Run(rt.pattern, func(t *testing.T) {
			path, _ := concretePath(t, rt, fx.ids)
			req := httptest.NewRequest(rt.method, path, nil)
			if _, got := mux.Handler(req); got != rt.pattern {
				t.Fatalf("%s %s routes to pattern %q, want %q", rt.method, path, got, rt.pattern)
			}
			req.Header.Set("Authorization", "Bearer "+sweepBearerB)

			fx.runs.takeLog()
			fx.audit.take()
			fx.logs.take()

			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)

			if rec.Code != http.StatusForbidden {
				t.Fatalf("%s: status = %d, want 403 account_forbidden; body %s", rt.pattern, rec.Code, rec.Body.String())
			}
			dec := json.NewDecoder(bytes.NewReader(rec.Body.Bytes()))
			var env errorEnvelope
			if err := dec.Decode(&env); err != nil {
				t.Fatalf("%s: decode error envelope: %v; body %s", rt.pattern, err, rec.Body.String())
			}
			if env.Error.Code != "account_forbidden" {
				t.Fatalf("%s: error.code = %q, want account_forbidden; body %s", rt.pattern, env.Error.Code, rec.Body.String())
			}
			var extra json.RawMessage
			if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
				t.Errorf("%s: bytes after the denial envelope (err %v): the handler wrote past the wrapper; body %s", rt.pattern, err, rec.Body.String())
			}
			if got, want := fx.runs.takeLog(), fx.wantResolution(rt.family); !slices.Equal(got, want) {
				t.Errorf("%s: run-repository reads = %v, want exactly the wrapper's resolution %v (a further read means the handler ran)", rt.pattern, got, want)
			}
			if n := fx.audit.take(); n != 0 {
				t.Errorf("%s: %d audit appends on a refused cross-account request, want 0", rt.pattern, n)
			}
			if errs := fx.logs.take(); len(errs) != 0 {
				t.Errorf("%s: ERROR-level log records on a refused cross-account request: %q", rt.pattern, errs)
			}
		})
	}
}

// --- Tier probe ---

// tierProbeSentinel is the panic value tierProbeRunRepo raises on the first
// repository read past the wrapper's own resolution.
type tierProbeSentinel struct{}

// tierProbeRunRepo is ONE resettable fake shared by every probe of the probe
// server. reset sets the run's account and the number of reads the wrapper's
// resolution needs (runs/concerns: GetRun; stages: GetStage then GetRun).
// The first read past that budget panics with tierProbeSentinel, so a reached
// handler stops at its first repository touch; every other run.Repository
// method hits the nil embedded interface and panics too. The probe recovers
// both and classifies them as "reached".
type tierProbeRunRepo struct {
	run.Repository
	mu     sync.Mutex
	runRow run.Run
	stage  run.Stage
	budget int
}

func (p *tierProbeRunRepo) reset(accountID string, budget int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.runRow.AccountID = accountID
	p.budget = budget
}

func (p *tierProbeRunRepo) spend() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.budget <= 0 {
		panic(tierProbeSentinel{})
	}
	p.budget--
}

func (p *tierProbeRunRepo) GetRun(_ context.Context, id uuid.UUID) (*run.Run, error) {
	p.spend()
	p.mu.Lock()
	defer p.mu.Unlock()
	if id != p.runRow.ID {
		return nil, run.ErrNotFound
	}
	rn := p.runRow
	return &rn, nil
}

func (p *tierProbeRunRepo) GetStage(_ context.Context, id uuid.UUID) (*run.Stage, error) {
	p.spend()
	p.mu.Lock()
	defer p.mu.Unlock()
	if id != p.stage.ID {
		return nil, run.ErrNotFound
	}
	st := p.stage
	return &st, nil
}

// tierProbeOutcome is what one probe observed: either the handler was reached
// (a recovered panic, or any response other than a wrapper denial), or the
// wrapper denied with code.
type tierProbeOutcome struct {
	reached bool
	code    string
	detail  string
}

// wrapperDenialCodes are the 403 codes enforceAccount writes.
var wrapperDenialCodes = map[string]bool{
	"account_forbidden":  true,
	"account_unresolved": true,
	"insufficient_role":  true,
	"repo_forbidden":     true,
}

func probeTier(t *testing.T, repo *tierProbeRunRepo, rt sweepRoute, ids sweepIDs, id Identity, runAccount string) tierProbeOutcome {
	t.Helper()
	budget := 1
	if rt.family == sweepFamilyStages {
		budget = 2
	}
	repo.reset(runAccount, budget)
	path, vals := concretePath(t, rt, ids)
	req := httptest.NewRequest(rt.method, path, nil)
	req = req.WithContext(context.WithValue(req.Context(), ctxKeyIdentity, id))
	setPathValues(req, vals)
	rec := httptest.NewRecorder()

	var recovered any
	func() {
		defer func() { recovered = recover() }()
		rt.handler(rec, req)
	}()
	if recovered != nil {
		return tierProbeOutcome{reached: true, detail: fmt.Sprintf("handler reached (panic %v)", recovered)}
	}
	var env errorEnvelope
	_ = json.Unmarshal(rec.Body.Bytes(), &env)
	detail := fmt.Sprintf("status %d code %q", rec.Code, env.Error.Code)
	if rec.Code == http.StatusForbidden && wrapperDenialCodes[env.Error.Code] {
		return tierProbeOutcome{code: env.Error.Code, detail: detail}
	}
	return tierProbeOutcome{reached: true, detail: "handler reached (" + detail + ")"}
}

func tierName(tier accountTier) string {
	switch tier {
	case readAccess:
		return "readAccess"
	case memberWrite:
		return "memberWrite"
	case adminWrite:
		return "adminWrite"
	}
	return fmt.Sprintf("accountTier(%d)", int(tier))
}

func expectedTier(rt sweepRoute) accountTier {
	if adminWriteRoutes[rt.pattern] {
		return adminWrite
	}
	if rt.method == http.MethodGet {
		return readAccess
	}
	return memberWrite
}

// TestRunScopedRoutes_AccessTierPinned infers every run-scoped route's
// accountTier and requires it to equal the pinned expectation.
//
// Construction (binding approval condition): the probe uses its OWN Server,
// built with RunRepo set to ONE resettable tierProbeRunRepo shared across
// every probe, ConcernRepo set to a fakeConcernRepo that resolves concern C to
// the probe run, and AccountRoles set to fakeAccountRoles{role:
// account.RoleMember}. Without AccountRoles enforceAccount skips
// role-bounding, so every adminWrite route would be inferred as memberWrite.
// The routes are enumerated against THAT server (s.registerRoutes on a
// recorder), so each captured wrapped handler closes over the probe repos;
// the test never swaps a repository after New().
//
// Each captured handler is invoked directly with an injected cookie identity:
//
//	P1: an accountless cookie against an UNTENANTED run. readAccess passes
//	    through to the handler; both write tiers answer 403 account_unresolved.
//	P2 (write tiers only): an account-A member cookie against run A.
//	    adminWrite answers 403 insufficient_role; memberWrite passes through.
//
// P1 reached → readAccess; P1 account_unresolved + P2 insufficient_role →
// adminWrite; P1 account_unresolved + P2 reached → memberWrite. Any other
// outcome fails with the observed responses.
func TestRunScopedRoutes_AccessTierPinned(t *testing.T) {
	runID := uuid.New()
	ids := sweepIDs{run: runID, stage: uuid.New(), concern: uuid.New()}
	repo := &tierProbeRunRepo{
		runRow: run.Run{ID: runID, Repo: "acme/app", WorkflowID: "feature_change", State: run.StatePending, CreatedAt: time.Now().UTC()},
		stage:  run.Stage{ID: ids.stage, RunID: runID},
	}
	concerns := newFakeConcernRepo()
	concerns.rows = []*concern.Concern{{ID: ids.concern, RunID: runID}}
	s := New(Config{
		Addr:         "127.0.0.1:0",
		RunRepo:      repo,
		ConcernRepo:  concerns,
		AccountRoles: fakeAccountRoles{role: account.RoleMember},
		Logger:       slog.New(slog.NewTextHandler(io.Discard, nil)),
	})

	routes := sweptRoutes(t, s)
	assertSweepGuards(t, routes)
	sort.Slice(routes, func(i, j int) bool { return routes[i].pattern < routes[j].pattern })

	for _, rt := range routes {
		if _, opted := accountIsolationOptOuts[rt.pattern]; opted {
			continue
		}
		t.Run(rt.pattern, func(t *testing.T) {
			p1 := probeTier(t, repo, rt, ids, cookieID(""), "")
			var observed accountTier
			switch {
			case p1.reached:
				observed = readAccess
			case p1.code == "account_unresolved":
				p2 := probeTier(t, repo, rt, ids, cookieID(authzAcctA), authzAcctA)
				switch {
				case p2.reached:
					observed = memberWrite
				case p2.code == "insufficient_role":
					observed = adminWrite
				default:
					t.Fatalf("%s: cannot infer tier: P1 %s, P2 %s", rt.pattern, p1.detail, p2.detail)
				}
			default:
				t.Fatalf("%s: cannot infer tier: P1 %s", rt.pattern, p1.detail)
			}
			if want := expectedTier(rt); observed != want {
				t.Errorf("%s: registered at tier %s, want %s (GET → readAccess, other methods → memberWrite, adminWriteRoutes → adminWrite); P1 %s",
					rt.pattern, tierName(observed), tierName(want), p1.detail)
			}
		})
	}
}
