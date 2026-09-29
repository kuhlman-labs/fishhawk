package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kuhlman-labs/fishhawk/backend/internal/account"
	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/campaign"
	"github.com/kuhlman-labs/fishhawk/backend/internal/captain"
	"github.com/kuhlman-labs/fishhawk/backend/internal/decisionindex"
	"github.com/kuhlman-labs/fishhawk/backend/internal/digest"
	"github.com/kuhlman-labs/fishhawk/backend/internal/handoverbrief"
	"github.com/kuhlman-labs/fishhawk/backend/internal/pgtest"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

const briefPGRepo = "kuhlman-labs/brief-e2e"

// briefPG is a real-schema fixture wired exactly as serve.go wires the
// collaborators the handover brief composes over: the production audit
// decorator (so merges are indexed), the digest stores, the captain store and
// the Postgres run + campaign repositories.
type briefPG struct {
	pool  *pgxpool.Pool
	audit audit.Repository
	srv   *Server
}

func newBriefPG(t *testing.T) *briefPG {
	t.Helper()
	pool := pgtest.NewPool(t)
	repo, err := decisionindex.NewIndexingRepository(audit.NewPostgresRepository(pool), decisionindex.NewStore(pool),
		decisionindex.NewPoolResolver(pool), slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("indexing repository: %v", err)
	}
	return &briefPG{pool: pool, audit: repo, srv: New(Config{
		Addr: "127.0.0.1:0", AuditRepo: repo,
		DigestStore: digest.NewStore(pool), DigestIndex: decisionindex.NewStore(pool),
		CaptainStore: captain.NewStore(pool),
		RunRepo:      run.NewPostgresRepository(pool),
		CampaignRepo: campaign.NewPostgresRepository(pool),
	})}
}

// seedCaptain appends captain_assigned BY CONSTRUCTION on the global chain.
func (f *briefPG) seedCaptain(t *testing.T, subject string) *audit.Entry {
	t.Helper()
	raw, _ := json.Marshal(map[string]any{"repo": briefPGRepo, "subject": subject, "identity_verified": true})
	kind := audit.ActorUser
	e, err := f.audit.AppendGlobalChained(context.Background(), audit.GlobalChainAppendParams{
		Timestamp: time.Now().UTC(), Category: captain.CategoryAssigned, ActorKind: &kind, ActorSubject: &subject, Payload: raw,
	})
	if err != nil {
		t.Fatalf("seed captain: %v", err)
	}
	return e
}

// seedMerge seeds a succeeded run carrying a cached spec and one
// merge_verdict_recorded entry on it.
func (f *briefPG) seedMerge(t *testing.T) *audit.Entry {
	t.Helper()
	id := uuid.New()
	if _, err := f.pool.Exec(context.Background(), `INSERT INTO runs (id, repo, workflow_id, workflow_sha, trigger_source, state, runner_kind, workflow_spec)
		VALUES ($1, $2, 'feature_change', 'sha-brief', 'cli', 'succeeded', 'local', $3)`, id, briefPGRepo, captainApprovalSpec([2]string{"", ""})); err != nil {
		t.Fatalf("seed run: %v", err)
	}
	kind := audit.ActorUser
	subject := "operator"
	raw, _ := json.Marshal(map[string]any{"verdict": "merged"})
	e, err := f.audit.AppendChained(context.Background(), audit.ChainAppendParams{
		RunID: id, Timestamp: time.Now().UTC(), Category: "merge_verdict_recorded", ActorKind: &kind, ActorSubject: &subject, Payload: raw,
	})
	if err != nil {
		t.Fatalf("append merge: %v", err)
	}
	return e
}

// offerPayload reads the newest captain_handover_offered payload straight
// from audit_entries (not through the code under test).
func (f *briefPG) offerPayload(t *testing.T) map[string]any {
	t.Helper()
	var raw []byte
	if err := f.pool.QueryRow(context.Background(), `SELECT payload FROM audit_entries
		WHERE run_id IS NULL AND category = 'captain_handover_offered' AND payload->>'repo' = $1
		ORDER BY sequence DESC LIMIT 1`, briefPGRepo).Scan(&raw); err != nil {
		t.Fatalf("read committed offer: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func (f *briefPG) offer(t *testing.T, by, successor string) {
	t.Helper()
	w := serveCaptain(t, f.srv, http.MethodPost, "/v0/captain/offer",
		`{"repo":"`+briefPGRepo+`","successor":"`+successor+`"}`, &Identity{Subject: by})
	if w.Code != http.StatusOK {
		t.Fatalf("POST /v0/captain/offer = %d:\n%s", w.Code, w.Body.String())
	}
}

func serveBrief(t *testing.T, s *Server, target string, id *Identity) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, target, nil)
	if id != nil {
		req = req.WithContext(context.WithValue(req.Context(), ctxKeyIdentity, *id))
	}
	w := httptest.NewRecorder()
	s.handleGetHandoverBrief(w, req)
	return w
}

func (f *briefPG) get(t *testing.T, query string) handoverbrief.Brief {
	t.Helper()
	w := serveBrief(t, f.srv, "/v0/handover-brief?repo="+briefPGRepo+query, &Identity{Subject: "github:reader"})
	if w.Code != http.StatusOK {
		t.Fatalf("GET /v0/handover-brief = %d:\n%s", w.Code, w.Body.String())
	}
	var b handoverbrief.Brief
	if err := json.Unmarshal(w.Body.Bytes(), &b); err != nil {
		t.Fatalf("decode brief: %v", err)
	}
	return b
}

func briefPart(t *testing.T, b handoverbrief.Brief, sec handoverbrief.SectionKind, part handoverbrief.PartKind) handoverbrief.Part {
	t.Helper()
	for _, s := range b.Sections {
		if s.Kind != sec {
			continue
		}
		for _, p := range s.Parts {
			if p.Kind == part {
				return p
			}
		}
	}
	t.Fatalf("brief has no %s/%s part", sec, part)
	return handoverbrief.Part{}
}

func briefHasDegradation(b handoverbrief.Brief, kind string) bool {
	for _, d := range b.Degradations {
		if d.Kind == kind {
			return true
		}
	}
	return false
}

// failingDB is a digest.DBTX whose every read fails — the reachable way to
// make the brief's WINDOW (chain-head) read fail while the captain store,
// on its own pool, keeps working.
type failingDB struct{}

var errBriefDBDown = errors.New("digest db unavailable")

func (failingDB) Exec(context.Context, string, ...any) (pgconn.CommandTag, error) {
	return pgconn.CommandTag{}, errBriefDBDown
}
func (failingDB) Query(context.Context, string, ...any) (pgx.Rows, error) { return nil, errBriefDBDown }
func (failingDB) QueryRow(context.Context, string, ...any) pgx.Row        { return failingRow{} }

type failingRow struct{}

func (failingRow) Scan(...any) error { return errBriefDBDown }

// failingIndex is a digest.IndexReader whose reads fail: a SECTION read error
// (merges / waivers come from the decision index), not a window failure.
type failingIndex struct{}

func (failingIndex) List(context.Context, decisionindex.ListFilter) ([]decisionindex.Row, error) {
	return nil, errors.New("decision index unavailable")
}
func (failingIndex) GapsInWindow(context.Context, decisionindex.GapFilter) (*decisionindex.GapReport, error) {
	return nil, errors.New("decision index unavailable")
}

// TestHandoverBriefRoute_Registered: the route is on the mux (an anonymous
// call reaches the handler's auth gate — 401 — rather than 404/405).
func TestHandoverBriefRoute_Registered(t *testing.T) {
	h := New(Config{Addr: "127.0.0.1:0"}).Handler()
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/v0/handover-brief?repo=o/r", nil))
	if w.Code != http.StatusUnauthorized {
		t.Errorf("GET /v0/handover-brief = %d, want 401 (route registered, auth gate reached)", w.Code)
	}
}

// TestGetHandoverBrief_UnconfiguredIs501: each missing REQUIRED collaborator is
// named in the 501 handover_brief_unconfigured envelope. With the guard's body
// returning true the request would reach Compose, which answers its own
// dependency_unconfigured as a 503 — so the 501 code is this guard's.
func TestGetHandoverBrief_UnconfiguredIs501(t *testing.T) {
	id := &Identity{Subject: "github:alice"}
	cases := map[string]struct {
		cfg  Config
		want []string
	}{
		"nothing":   {Config{}, []string{"digest_store", "decision_index", "captain_store"}},
		"no digest": {Config{DigestIndex: digestNoIndex{t}, CaptainStore: captain.NewStore(nil)}, []string{"digest_store"}},
		"no index":  {Config{DigestStore: digest.NewStore(nil), CaptainStore: captain.NewStore(nil)}, []string{"decision_index"}},
		"no captain": {Config{DigestStore: digest.NewStore(nil), DigestIndex: digestNoIndex{t}},
			[]string{"captain_store"}},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			tc.cfg.Addr = "127.0.0.1:0"
			w := serveBrief(t, New(tc.cfg), "/v0/handover-brief?repo=o/r", id)
			if w.Code != http.StatusNotImplemented {
				t.Fatalf("status = %d, want 501:\n%s", w.Code, w.Body.String())
			}
			e := digestErrorBody(t, w)
			if e.Code != "handover_brief_unconfigured" {
				t.Errorf("code = %q, want handover_brief_unconfigured", e.Code)
			}
			for _, m := range tc.want {
				if !strings.Contains(e.Message, m) {
					t.Errorf("message %q does not name %s", e.Message, m)
				}
			}
		})
	}
}

// TestGetHandoverBrief_RequestShapeRefused: anonymous 401, wrong scope 403,
// and each 400 — all before any I/O (the stores have nil pools).
func TestGetHandoverBrief_RequestShapeRefused(t *testing.T) {
	s := New(Config{Addr: "127.0.0.1:0", DigestStore: digest.NewStore(nil), DigestIndex: digestNoIndex{t}, CaptainStore: captain.NewStore(nil)})
	full := &Identity{Subject: "github:alice"}
	cases := []struct {
		name, query string
		id          *Identity
		status      int
		field       string
	}{
		{"anonymous", "?repo=o/r", nil, http.StatusUnauthorized, ""},
		{"wrong scope", "?repo=o/r", digestTokenIdentity("tok", "write:approvals"), http.StatusForbidden, ""},
		{"no repo", "", full, http.StatusBadRequest, "repo"},
		{"unknown section", "?repo=o/r&section=bogus", full, http.StatusBadRequest, "section"},
		{"bad from", "?repo=o/r&from_sequence=-1", full, http.StatusBadRequest, "from_sequence"},
		{"bad to", "?repo=o/r&to_sequence=x", full, http.StatusBadRequest, "to_sequence"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := serveBrief(t, s, "/v0/handover-brief"+tc.query, tc.id)
			if w.Code != tc.status {
				t.Fatalf("status = %d, want %d:\n%s", w.Code, tc.status, w.Body.String())
			}
			if tc.field != "" {
				if e := digestErrorBody(t, w); e.Details["field"] != tc.field {
					t.Errorf("details.field = %v, want %s", e.Details["field"], tc.field)
				}
			}
		})
	}
}

// TestWriteHandoverBriefError_MapsEachClass: invalid -> 400, unestablishable ->
// 503 naming the reason, anything else -> 500.
func TestWriteHandoverBriefError_MapsEachClass(t *testing.T) {
	s := New(Config{Addr: "127.0.0.1:0"})
	cases := []struct {
		err    error
		status int
		code   string
	}{
		{handoverbrief.ErrInvalidRequest, http.StatusBadRequest, "validation_failed"},
		{&handoverbrief.UnavailableError{Reason: handoverbrief.ReasonWindowReadFailed, Err: errBriefDBDown}, http.StatusServiceUnavailable, "handover_brief_unavailable"},
		{errors.New("boom"), http.StatusInternalServerError, "internal_error"},
	}
	for _, tc := range cases {
		w := httptest.NewRecorder()
		s.writeHandoverBriefError(w, httptest.NewRequest(http.MethodGet, "/", nil), tc.err)
		if w.Code != tc.status {
			t.Errorf("%v -> %d, want %d", tc.err, w.Code, tc.status)
		}
		e := digestErrorBody(t, w)
		if e.Code != tc.code {
			t.Errorf("%v -> code %q, want %q", tc.err, e.Code, tc.code)
		}
		if tc.status == http.StatusServiceUnavailable && !strings.Contains(e.Message, handoverbrief.ReasonWindowReadFailed) {
			t.Errorf("503 message %q does not name the reason", e.Message)
		}
	}
}

// TestHandoverBriefRoute_ComposesAndBounds drives the REAL handler chain
// (scope -> configured -> visibility -> Compose -> Select -> Bound -> JSON) over
// a pgtest fixture: the merge after the captain_assigned entry appears in
// what_changed citing its chain entry, the brief carries a hash, the declared
// absences and the run-cache delegation view; a ?section= read keeps the SAME
// brief_hash (the hash of the whole canonical brief, never re-hashed).
func TestHandoverBriefRoute_ComposesAndBounds(t *testing.T) {
	f := newBriefPG(t)
	assigned := f.seedCaptain(t, "github:alice")
	merge := f.seedMerge(t)

	b := f.get(t, "")
	if b.Repo != briefPGRepo || b.CaptainSubject != "github:alice" {
		t.Errorf("brief repo/captain = %q/%q", b.Repo, b.CaptainSubject)
	}
	if b.Window.FromSequence != assigned.Sequence+1 || b.Window.Basis != handoverbrief.BasisSinceLastHandover {
		t.Errorf("window = %+v, want from %d since_last_handover", b.Window, assigned.Sequence+1)
	}
	m := briefPart(t, b, handoverbrief.SectionWhatChanged, handoverbrief.PartMerges)
	if len(m.Items) != 1 || m.Items[0].SourceSequence != merge.Sequence || m.Items[0].SourceEntryHash != merge.EntryHash {
		t.Errorf("merges = %+v, want one item citing %d/%s", m.Items, merge.Sequence, merge.EntryHash)
	}
	if b.BriefHash == "" || b.BriefHash != handoverbrief.Hash(b) {
		t.Errorf("brief_hash = %q, want the canonical hash %q", b.BriefHash, handoverbrief.Hash(b))
	}
	absent := map[string]bool{}
	for _, a := range b.Absent {
		absent[a.Section] = a.Reason != ""
	}
	for _, want := range []string{"charter_revision", "adr_index", "doctrine_changes"} {
		if !absent[want] {
			t.Errorf("absent = %+v, want %s declared with a reason", b.Absent, want)
		}
	}
	wf := briefPart(t, b, handoverbrief.SectionDelegationInForce, handoverbrief.PartWorkflows)
	if wf.Unavailable || len(wf.Workflows) != 1 || wf.Workflows[0].Confirmation != handoverbrief.ConfirmationUnavailable {
		t.Errorf("delegation part = %+v, want one workflow from the run-cache spec with confirmation unavailable", wf)
	}
	if briefHasDegradation(b, handoverbrief.DegradationDelegationUnavailable) {
		t.Errorf("degradations = %+v, want no delegation_unavailable with a cached spec", b.Degradations)
	}

	sel := f.get(t, "&section=in_flight")
	if sel.Section != handoverbrief.SectionInFlight || len(sel.Sections) != 1 || sel.Sections[0].Kind != handoverbrief.SectionInFlight {
		t.Errorf("section read = %s with %d sections, want in_flight only", sel.Section, len(sel.Sections))
	}
	if sel.BriefHash != b.BriefHash {
		t.Errorf("section read brief_hash = %q, want the whole brief's %q", sel.BriefHash, b.BriefHash)
	}
}

// TestHandoverBriefRoute_ForeignRepoIsForbidden: a member who cannot read the
// repository gets 403 repo_forbidden. The fixture's brief is NON-EMPTY (a
// cited merge), so with the visibility call removed the handler would answer
// 200 with content instead.
func TestHandoverBriefRoute_ForeignRepoIsForbidden(t *testing.T) {
	f := newBriefPG(t)
	f.seedCaptain(t, "github:alice")
	f.seedMerge(t)
	f.srv.cfg.AccountRoles = fakeAccountRoles{role: account.RoleMember}
	f.srv.cfg.RepoVisibility = newFakeRepoVisibility(map[string]bool{"other/repo": true})
	id := memberIdentity()
	w := serveBrief(t, f.srv, "/v0/handover-brief?repo="+briefPGRepo, &id)
	if w.Code != http.StatusForbidden {
		t.Fatalf("GET = %d, want 403:\n%s", w.Code, w.Body.String())
	}
	if e := digestErrorBody(t, w); e.Code != "repo_forbidden" {
		t.Errorf("code = %q, want repo_forbidden", e.Code)
	}
}

// TestHandoverBriefRoute_WindowFailureIs503: an unestablishable brief (the
// chain-head read fails) is 503 handover_brief_unavailable, never a 200 with
// an unhashed brief.
func TestHandoverBriefRoute_WindowFailureIs503(t *testing.T) {
	f := newBriefPG(t)
	f.srv.cfg.DigestStore = digest.NewStore(failingDB{})
	w := serveBrief(t, f.srv, "/v0/handover-brief?repo="+briefPGRepo, &Identity{Subject: "github:reader"})
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("GET = %d, want 503:\n%s", w.Code, w.Body.String())
	}
	if e := digestErrorBody(t, w); e.Code != "handover_brief_unavailable" || !strings.Contains(e.Message, handoverbrief.ReasonWindowReadFailed) {
		t.Errorf("error = %+v, want handover_brief_unavailable naming window_read_failed", e)
	}
}

// TestHandoverBriefRoute_BeyondChainHeadIs400: a to_sequence above the chain
// head is the package's ErrInvalidRequest, mapped to 400.
func TestHandoverBriefRoute_BeyondChainHeadIs400(t *testing.T) {
	f := newBriefPG(t)
	f.seedCaptain(t, "github:alice")
	merge := f.seedMerge(t)
	w := serveBrief(t, f.srv, "/v0/handover-brief?repo="+briefPGRepo+"&to_sequence=999999", &Identity{Subject: "github:reader"})
	if w.Code != http.StatusBadRequest || digestErrorBody(t, w).Code != "validation_failed" {
		t.Errorf("GET beyond head (%d) = %d:\n%s", merge.Sequence, w.Code, w.Body.String())
	}
}

// TestHandoverBriefDelegation_EachUnavailableBranch: every way the run-cache
// delegation view cannot be projected returns nil with a NAMED reason (which
// the brief records as delegation_unavailable), and a valid cached spec
// projects a view with the run-cache source and the run's workflow_sha.
func TestHandoverBriefDelegation_EachUnavailableBranch(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		name string
		repo run.Repository
		want string
	}{
		{"no run repo", nil, "no run repository"},
		{"list fails", &captainRunRepo{err: errors.New("db down")}, "list runs failed"},
		{"no run", &captainRunRepo{}, "no run of this repository"},
		{"no cached spec", &captainRunRepo{rows: []*run.Run{{}}}, "no run of this repository"},
		{"invalid spec", &captainRunRepo{rows: []*run.Run{{WorkflowSpec: []byte("version: \"1.0\"\nworkflows: 7\n")}}}, "does not validate"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := Config{Addr: "127.0.0.1:0"}
			if tc.repo != nil {
				cfg.RunRepo = tc.repo
			}
			v, reason := New(cfg).handoverBriefDelegation(ctx, "o/r")
			if v != nil || !strings.Contains(reason, tc.want) {
				t.Errorf("view=%v reason=%q, want nil and a reason containing %q", v, reason, tc.want)
			}
		})
	}
	s := New(Config{Addr: "127.0.0.1:0", RunRepo: &captainRunRepo{rows: []*run.Run{{
		WorkflowSpec: captainApprovalSpec([2]string{"", ""}), WorkflowSHA: "abc123",
	}}}})
	v, reason := s.handoverBriefDelegation(ctx, "o/r")
	if v == nil || reason != "" {
		t.Fatalf("valid spec: view=%v reason=%q", v, reason)
	}
	if v.Source != delegationSourceRunCache || v.WorkflowSHA != "abc123" || v.SpecVersion != "1.0" || v.SchemaMajor != 1 ||
		len(v.Workflows) != 1 || v.ContentHash == "" {
		t.Errorf("view = %+v, want run_cache source, sha abc123, version 1.0, one workflow, a content hash", v)
	}
}

// TestHandoverBriefDelegation_UnavailableDegradesTheBrief: with no cached spec
// the brief still composes (and hashes) with delegation_unavailable recorded
// carrying the server's named reason.
func TestHandoverBriefDelegation_UnavailableDegradesTheBrief(t *testing.T) {
	f := newBriefPG(t)
	f.seedCaptain(t, "github:alice")
	b := f.get(t, "")
	if b.BriefHash == "" {
		t.Error("brief unhashed; a degradation must not fail the brief")
	}
	found := false
	for _, d := range b.Degradations {
		if d.Kind == handoverbrief.DegradationDelegationUnavailable && strings.Contains(d.Detail, "no run of this repository") {
			found = true
		}
	}
	if !found {
		t.Errorf("degradations = %+v, want delegation_unavailable naming the missing cached spec", b.Degradations)
	}
}

// TestOpenAPI_HandoverBriefDocumented: the route and the offer's brief fields
// are in the OpenAPI source of truth.
func TestOpenAPI_HandoverBriefDocumented(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "..", "docs", "api", "v0.openapi.yaml"))
	if err != nil {
		t.Fatalf("read openapi: %v", err)
	}
	doc := string(raw)
	for _, want := range []string{"\n  /v0/handover-brief:\n", "operationId: getHandoverBrief", "brief_hash:", "brief_unavailable:", "brief_unavailable_reason:", "handover_brief_unconfigured"} {
		if !strings.Contains(doc, want) {
			t.Errorf("docs/api/v0.openapi.yaml is missing %q", strings.TrimSpace(want))
		}
	}
}
