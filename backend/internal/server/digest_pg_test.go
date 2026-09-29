package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kuhlman-labs/fishhawk/backend/internal/account"
	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/decisionindex"
	"github.com/kuhlman-labs/fishhawk/backend/internal/digest"
	"github.com/kuhlman-labs/fishhawk/backend/internal/pgtest"
)

const digestPGRepo = "kuhlman-labs/digest-e2e"

// digestPG is a real-schema fixture: a pgtest database, the PRODUCTION audit
// wiring (the Postgres repository wrapped by the decision-index decorator, as
// serve.go's newAuditRepository builds it) and a Server wired exactly as
// serve.go wires the digest stores.
type digestPG struct {
	pool  *pgxpool.Pool
	audit audit.Repository
	srv   *Server
}

func newDigestPG(t *testing.T) *digestPG {
	t.Helper()
	pool := pgtest.NewPool(t)
	repo, err := decisionindex.NewIndexingRepository(audit.NewPostgresRepository(pool), decisionindex.NewStore(pool),
		decisionindex.NewPoolResolver(pool), slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("indexing repository: %v", err)
	}
	return &digestPG{pool: pool, audit: repo, srv: New(Config{
		Addr: "127.0.0.1:0", AuditRepo: repo,
		DigestStore: digest.NewStore(pool), DigestIndex: decisionindex.NewStore(pool),
	})}
}

func (f *digestPG) seedRun(t *testing.T, state string) uuid.UUID {
	t.Helper()
	id := uuid.New()
	if _, err := f.pool.Exec(context.Background(), `INSERT INTO runs (id, repo, workflow_id, workflow_sha, trigger_source, state, runner_kind)
		VALUES ($1, $2, 'feature_change', 'sha', 'cli', $3, 'local')`, id, digestPGRepo, state); err != nil {
		t.Fatalf("seed run: %v", err)
	}
	return id
}

func (f *digestPG) seedStage(t *testing.T, runID uuid.UUID, kind, state string) uuid.UUID {
	t.Helper()
	id := uuid.New()
	if _, err := f.pool.Exec(context.Background(), `INSERT INTO stages (id, run_id, sequence, stage_type, executor_kind, executor_ref, state)
		VALUES ($1, $2, 1, $3, 'agent', 'claude-code', $4)`, id, runID, kind, state); err != nil {
		t.Fatalf("seed stage: %v", err)
	}
	return id
}

func (f *digestPG) append(t *testing.T, runID uuid.UUID, stageID *uuid.UUID, category string, payload map[string]any) *audit.Entry {
	t.Helper()
	raw, _ := json.Marshal(payload)
	kind := audit.ActorUser
	subject := "operator"
	e, err := f.audit.AppendChained(context.Background(), audit.ChainAppendParams{
		RunID: runID, StageID: stageID, Timestamp: time.Now().UTC(), Category: category,
		ActorKind: &kind, ActorSubject: &subject, Payload: raw,
	})
	if err != nil {
		t.Fatalf("append %s: %v", category, err)
	}
	return e
}

// watermark reads the stored watermark row directly (not through the code
// under test): -1 when absent.
func (f *digestPG) watermark(t *testing.T, subject string) int64 {
	t.Helper()
	var seq int64
	err := f.pool.QueryRow(context.Background(),
		`SELECT sequence FROM captain_read_watermarks WHERE captain_subject = $1 AND repo = $2`, subject, digestPGRepo).Scan(&seq)
	if err != nil {
		if strings.Contains(err.Error(), "no rows") {
			return -1
		}
		t.Fatalf("read watermark: %v", err)
	}
	return seq
}

// markedReadCount counts digest_marked_read entries on the GLOBAL chain.
func (f *digestPG) markedReadCount(t *testing.T) int {
	t.Helper()
	var n int
	if err := f.pool.QueryRow(context.Background(),
		`SELECT count(*) FROM audit_entries WHERE category = 'digest_marked_read' AND run_id IS NULL`).Scan(&n); err != nil {
		t.Fatalf("count digest_marked_read: %v", err)
	}
	return n
}

func (f *digestPG) get(t *testing.T, id *Identity, query string) (digest.Digest, string) {
	t.Helper()
	w := serveDigest(t, f.srv, http.MethodGet, "/v0/digest?repo="+digestPGRepo+query, "", id)
	if w.Code != http.StatusOK {
		t.Fatalf("GET /v0/digest = %d, want 200:\n%s", w.Code, w.Body.String())
	}
	var d digest.Digest
	if err := json.Unmarshal(w.Body.Bytes(), &d); err != nil {
		t.Fatalf("decode digest: %v", err)
	}
	return d, w.Body.String()
}

func (f *digestPG) markRead(t *testing.T, id *Identity, to int64) (int, string) {
	t.Helper()
	w := serveDigest(t, f.srv, http.MethodPost, "/v0/digest/mark-read",
		fmt.Sprintf(`{"repo":%q,"to_sequence":%d}`, digestPGRepo, to), id)
	return w.Code, w.Body.String()
}

func digestSection(t *testing.T, d digest.Digest, k digest.SectionKind) digest.Section {
	t.Helper()
	for _, s := range d.Sections {
		if s.Kind == k {
			return s
		}
	}
	t.Fatalf("digest has no %s section: %+v", k, d.Sections)
	return digest.Section{}
}

var (
	digestCaptainA = digestTokenIdentity("captain:a", scopeDigestRead, scopeDigestMarkRead)
	digestCaptainB = digestTokenIdentity("captain:b", scopeDigestRead, scopeDigestMarkRead)
)

// TestDigestEndToEnd_WatermarkRoundTrip: two runs in one repo (one merged,
// one with a waived concern) written through the production audit wiring
// surface in GET /v0/digest citing real chain entries; POST mark-read appends
// digest_marked_read on the GLOBAL chain and advances the watermark; the
// re-read no longer reports the already-read items.
func TestDigestEndToEnd_WatermarkRoundTrip(t *testing.T) {
	f := newDigestPG(t)
	merged := f.seedRun(t, "succeeded")
	merge := f.append(t, merged, nil, "merge_verdict_recorded", map[string]any{"verdict": "merged"})
	waiving := f.seedRun(t, "running")
	stage := f.seedStage(t, waiving, "implement", "succeeded")
	waive := f.append(t, waiving, &stage, "concern_waived", map[string]any{"reason": "accepted risk", "category": "correctness"})

	d, _ := f.get(t, digestCaptainA, "")
	if d.HasWatermark || d.ChainHead != waive.Sequence || d.ToSequence != waive.Sequence {
		t.Fatalf("digest window = has_watermark %v head %d to %d, want no watermark, head=to=%d", d.HasWatermark, d.ChainHead, d.ToSequence, waive.Sequence)
	}
	m := digestSection(t, d, digest.SectionMerges)
	if len(m.Items) != 1 || m.Items[0].SourceSequence != merge.Sequence || m.Items[0].SourceEntryHash != merge.EntryHash {
		t.Errorf("merges = %+v, want one item citing %d/%s", m.Items, merge.Sequence, merge.EntryHash)
	}
	wv := digestSection(t, d, digest.SectionWaiversAndDeferrals)
	if len(wv.Items) != 1 || wv.Items[0].SourceSequence != waive.Sequence || wv.Items[0].SourceEntryHash != waive.EntryHash {
		t.Errorf("waivers = %+v, want one item citing %d/%s", wv.Items, waive.Sequence, waive.EntryHash)
	}
	// Every cited sequence resolves to a real audit_entries row with that hash.
	for _, s := range d.Sections {
		for _, it := range s.Items {
			var hash string
			if err := f.pool.QueryRow(context.Background(), `SELECT entry_hash FROM audit_entries WHERE sequence = $1`,
				it.SourceSequence).Scan(&hash); err != nil || hash != it.SourceEntryHash {
				t.Errorf("%s item cites %d/%s; chain has %q (%v)", s.Kind, it.SourceSequence, it.SourceEntryHash, hash, err)
			}
		}
	}
	if len(d.Gaps) != 0 {
		t.Errorf("gaps = %+v, want none (both decisions were indexed by the production decorator)", d.Gaps)
	}

	code, body := f.markRead(t, digestCaptainA, d.ToSequence)
	if code != http.StatusOK {
		t.Fatalf("mark-read = %d, want 200:\n%s", code, body)
	}
	var res digestMarkReadResponse
	if err := json.Unmarshal([]byte(body), &res); err != nil || !res.Advanced || res.Sequence != d.ToSequence || res.CaptainSubject != "captain:a" {
		t.Errorf("mark-read response = %+v (%v), want advanced to %d for captain:a", res, err, d.ToSequence)
	}
	if got := f.watermark(t, "captain:a"); got != d.ToSequence {
		t.Errorf("stored watermark = %d, want %d", got, d.ToSequence)
	}
	var (
		cat, actor string
		payload    []byte
	)
	if err := f.pool.QueryRow(context.Background(), `SELECT category, actor_subject, payload FROM audit_entries
		WHERE category = 'digest_marked_read' AND run_id IS NULL`).Scan(&cat, &actor, &payload); err != nil {
		t.Fatalf("digest_marked_read not on the global chain: %v", err)
	}
	if actor != "captain:a" || !strings.Contains(string(payload), fmt.Sprintf(`"to_sequence": %d`, d.ToSequence)) {
		t.Errorf("digest_marked_read actor=%q payload=%s, want captain:a with to_sequence %d", actor, payload, d.ToSequence)
	}

	after, _ := f.get(t, digestCaptainA, "")
	if !after.HasWatermark || after.Watermark != d.ToSequence {
		t.Errorf("re-read watermark = %d (has %v), want %d", after.Watermark, after.HasWatermark, d.ToSequence)
	}
	for _, k := range []digest.SectionKind{digest.SectionMerges, digest.SectionWaiversAndDeferrals} {
		if s := digestSection(t, after, k); len(s.Items) != 0 {
			t.Errorf("after mark-read %s = %+v, want the already-read items gone", k, s.Items)
		}
	}
}

// TestGetDigest_DoesNotAdvanceWatermark: retrieval never writes — three GETs
// leave no watermark row and no chain entry for a first-time captain, and
// leave an existing watermark unchanged.
func TestGetDigest_DoesNotAdvanceWatermark(t *testing.T) {
	f := newDigestPG(t)
	rn := f.seedRun(t, "succeeded")
	first := f.append(t, rn, nil, "merge_verdict_recorded", map[string]any{"verdict": "merged"})
	for i := 0; i < 3; i++ {
		f.get(t, digestCaptainA, "")
	}
	if got := f.watermark(t, "captain:a"); got != -1 {
		t.Fatalf("watermark after GETs = %d, want no row", got)
	}
	if code, body := f.markRead(t, digestCaptainA, first.Sequence); code != http.StatusOK {
		t.Fatalf("mark-read = %d:\n%s", code, body)
	}
	f.append(t, f.seedRun(t, "succeeded"), nil, "merge_verdict_recorded", map[string]any{"verdict": "merged"})
	before := f.markedReadCount(t)
	for i := 0; i < 3; i++ {
		f.get(t, digestCaptainA, "")
	}
	if got := f.watermark(t, "captain:a"); got != first.Sequence {
		t.Errorf("watermark after GETs = %d, want unchanged %d", got, first.Sequence)
	}
	if n := f.markedReadCount(t); n != before {
		t.Errorf("digest_marked_read entries = %d after GETs, want %d (retrieval appended)", n, before)
	}
}

// TestMarkRead_AdvancesWatermark: the stored row equals to_sequence.
func TestMarkRead_AdvancesWatermark(t *testing.T) {
	f := newDigestPG(t)
	rn := f.seedRun(t, "succeeded")
	e := f.append(t, rn, nil, "merge_verdict_recorded", map[string]any{"verdict": "merged"})
	if code, body := f.markRead(t, digestCaptainA, e.Sequence); code != http.StatusOK {
		t.Fatalf("mark-read = %d:\n%s", code, body)
	}
	if got := f.watermark(t, "captain:a"); got != e.Sequence {
		t.Errorf("stored watermark = %d, want %d", got, e.Sequence)
	}
}

// TestMarkRead_BeyondChainHeadRefused (failure mode 3): the repo's head is the
// seeded entry; to_sequence far above it is a 400 naming the head, with no
// entry appended and no row written.
func TestMarkRead_BeyondChainHeadRefused(t *testing.T) {
	f := newDigestPG(t)
	e := f.append(t, f.seedRun(t, "succeeded"), nil, "merge_verdict_recorded", map[string]any{"verdict": "merged"})
	code, body := f.markRead(t, digestCaptainA, e.Sequence+999)
	if code != http.StatusBadRequest {
		t.Fatalf("mark-read = %d, want 400:\n%s", code, body)
	}
	var env errorEnvelope
	_ = json.Unmarshal([]byte(body), &env)
	if env.Error.Code != "to_sequence_beyond_chain_head" || env.Error.Details["chain_head"] != float64(e.Sequence) ||
		!strings.Contains(env.Error.Message, fmt.Sprint(e.Sequence)) {
		t.Errorf("error = %+v, want to_sequence_beyond_chain_head naming head %d", env.Error, e.Sequence)
	}
	if f.watermark(t, "captain:a") != -1 || f.markedReadCount(t) != 0 {
		t.Error("a refused mark-read wrote a watermark or a chain entry")
	}
}

// TestGetDigest_BeyondChainHeadRefused: GET refuses a to_sequence above the
// head the same way, naming the head.
func TestGetDigest_BeyondChainHeadRefused(t *testing.T) {
	f := newDigestPG(t)
	e := f.append(t, f.seedRun(t, "succeeded"), nil, "merge_verdict_recorded", map[string]any{"verdict": "merged"})
	w := serveDigest(t, f.srv, http.MethodGet, fmt.Sprintf("/v0/digest?repo=%s&to_sequence=%d", digestPGRepo, e.Sequence+50), "", digestCaptainA)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("GET = %d, want 400:\n%s", w.Code, w.Body.String())
	}
	if env := digestErrorBody(t, w); env.Code != "to_sequence_beyond_chain_head" || env.Details["chain_head"] != float64(e.Sequence) {
		t.Errorf("error = %+v, want to_sequence_beyond_chain_head naming head %d", env, e.Sequence)
	}
}

// TestMarkRead_AtOrBelowWatermarkIsNoOp (failure mode 4): a mark at or below
// the watermark answers advanced=false, leaves the row unchanged AND appends
// nothing (the count is the assertion the monotonic upsert cannot mask).
func TestMarkRead_AtOrBelowWatermarkIsNoOp(t *testing.T) {
	f := newDigestPG(t)
	low := f.append(t, f.seedRun(t, "succeeded"), nil, "merge_verdict_recorded", map[string]any{"verdict": "merged"})
	high := f.append(t, f.seedRun(t, "succeeded"), nil, "merge_verdict_recorded", map[string]any{"verdict": "merged"})
	if code, body := f.markRead(t, digestCaptainA, high.Sequence); code != http.StatusOK {
		t.Fatalf("mark-read = %d:\n%s", code, body)
	}
	if n := f.markedReadCount(t); n != 1 {
		t.Fatalf("digest_marked_read entries = %d, want 1", n)
	}
	for _, to := range []int64{high.Sequence, low.Sequence} {
		code, body := f.markRead(t, digestCaptainA, to)
		if code != http.StatusOK {
			t.Fatalf("mark-read(%d) = %d:\n%s", to, code, body)
		}
		var res digestMarkReadResponse
		if err := json.Unmarshal([]byte(body), &res); err != nil || res.Advanced || res.Sequence != high.Sequence {
			t.Errorf("mark-read(%d) = %+v (%v), want advanced=false at %d", to, res, err, high.Sequence)
		}
	}
	if n := f.markedReadCount(t); n != 1 {
		t.Errorf("digest_marked_read entries = %d after no-op marks, want 1", n)
	}
	if got := f.watermark(t, "captain:a"); got != high.Sequence {
		t.Errorf("stored watermark = %d, want %d", got, high.Sequence)
	}
}

// digestFailingAppend wraps the real audit repository and fails every
// global-chain append — the digest_marked_read write path.
type digestFailingAppend struct {
	audit.Repository
}

func (digestFailingAppend) AppendGlobalChained(context.Context, audit.GlobalChainAppendParams) (*audit.Entry, error) {
	return nil, errors.New("injected global-chain append failure")
}

// TestMarkRead_AppendFailureLeavesWatermarkUnmoved (failure mode 1): the
// append fails, so the call errors AND the stored watermark is still the prior
// one — a state read after the call, not only the error identity.
func TestMarkRead_AppendFailureLeavesWatermarkUnmoved(t *testing.T) {
	f := newDigestPG(t)
	prior := f.append(t, f.seedRun(t, "succeeded"), nil, "merge_verdict_recorded", map[string]any{"verdict": "merged"})
	if code, body := f.markRead(t, digestCaptainA, prior.Sequence); code != http.StatusOK {
		t.Fatalf("seed mark-read = %d:\n%s", code, body)
	}
	next := f.append(t, f.seedRun(t, "succeeded"), nil, "merge_verdict_recorded", map[string]any{"verdict": "merged"})
	f.srv.cfg.AuditRepo = digestFailingAppend{f.audit}
	code, body := f.markRead(t, digestCaptainA, next.Sequence)
	if code != http.StatusInternalServerError {
		t.Fatalf("mark-read with a failing append = %d, want 500:\n%s", code, body)
	}
	if got := f.watermark(t, "captain:a"); got != prior.Sequence {
		t.Errorf("stored watermark = %d after a failed append, want the prior %d", got, prior.Sequence)
	}
}

// TestMarkRead_AnonymousIs401 / TestMarkRead_AuthenticatedWithoutScopeIs403
// (failure mode 5): an unauthenticated caller is 401; an AUTHENTICATED token
// whose scope list is exactly [read:runs] is 403 insufficient_scope. Both
// against a fully wired server whose mark would otherwise succeed, and neither
// writes a row or a chain entry.
func TestMarkRead_AnonymousIs401(t *testing.T) {
	f := newDigestPG(t)
	e := f.append(t, f.seedRun(t, "succeeded"), nil, "merge_verdict_recorded", map[string]any{"verdict": "merged"})
	if code, body := f.markRead(t, nil, e.Sequence); code != http.StatusUnauthorized {
		t.Fatalf("anonymous mark-read = %d, want 401:\n%s", code, body)
	}
	if f.markedReadCount(t) != 0 {
		t.Error("anonymous mark-read appended a chain entry")
	}
}

func TestMarkRead_AuthenticatedWithoutScopeIs403(t *testing.T) {
	f := newDigestPG(t)
	e := f.append(t, f.seedRun(t, "succeeded"), nil, "merge_verdict_recorded", map[string]any{"verdict": "merged"})
	code, body := f.markRead(t, digestTokenIdentity("captain:a", "read:runs"), e.Sequence)
	if code != http.StatusForbidden || !strings.Contains(body, "insufficient_scope") {
		t.Fatalf("read:runs-only mark-read = %d, want 403 insufficient_scope:\n%s", code, body)
	}
	if f.watermark(t, "captain:a") != -1 || f.markedReadCount(t) != 0 {
		t.Error("an unscoped mark-read wrote a watermark or a chain entry")
	}
}

// TestWatermark_TwoCaptainsAreIndependent: captain A marking read leaves
// captain B's digest byte-identical.
func TestWatermark_TwoCaptainsAreIndependent(t *testing.T) {
	f := newDigestPG(t)
	e := f.append(t, f.seedRun(t, "succeeded"), nil, "merge_verdict_recorded", map[string]any{"verdict": "merged"})
	_, before := f.get(t, digestCaptainB, "")
	if code, body := f.markRead(t, digestCaptainA, e.Sequence); code != http.StatusOK {
		t.Fatalf("mark-read = %d:\n%s", code, body)
	}
	_, after := f.get(t, digestCaptainB, "")
	if before != after {
		t.Errorf("captain B's digest changed after captain A marked read:\nbefore %s\nafter  %s", before, after)
	}
	if f.watermark(t, "captain:b") != -1 {
		t.Error("captain A's mark wrote captain B's watermark")
	}
}

// TestGetDigest_SectionSelectorAndBound: the section selector narrows the
// response to one section and the response honours the ONE bound.
func TestGetDigest_SectionSelectorAndBound(t *testing.T) {
	f := newDigestPG(t)
	f.append(t, f.seedRun(t, "succeeded"), nil, "merge_verdict_recorded", map[string]any{"verdict": "merged"})
	d, body := f.get(t, digestCaptainA, "&section=merges")
	if len(d.Sections) != 1 || d.Sections[0].Kind != digest.SectionMerges || d.Section != digest.SectionMerges {
		t.Errorf("sections = %+v (section %q), want only merges", d.Sections, d.Section)
	}
	if len(body) > digest.DefaultByteBudget {
		t.Errorf("response is %d bytes, over the %d budget", len(body), digest.DefaultByteBudget)
	}
}

// TestGetDigest_RepoForbidden403: a non-admin cookie-session caller who cannot
// read the repository at the forge gets 403 repo_forbidden (the #2071
// point-read deny). The fixture HOLDS a digestable entry, so with the gate's
// call site removed the handler would answer 200 with it instead.
func TestGetDigest_RepoForbidden403(t *testing.T) {
	f := newDigestPG(t)
	f.append(t, f.seedRun(t, "succeeded"), nil, "merge_verdict_recorded", map[string]any{"verdict": "merged"})
	f.srv.cfg.AccountRoles = fakeAccountRoles{role: account.RoleMember}
	f.srv.cfg.RepoVisibility = newFakeRepoVisibility(map[string]bool{"other/repo": true})
	id := memberIdentity()
	w := serveDigest(t, f.srv, http.MethodGet, "/v0/digest?repo="+digestPGRepo, "", &id)
	if w.Code != http.StatusForbidden {
		t.Fatalf("GET = %d, want 403:\n%s", w.Code, w.Body.String())
	}
	if e := digestErrorBody(t, w); e.Code != "repo_forbidden" {
		t.Errorf("code = %q, want repo_forbidden", e.Code)
	}
}
