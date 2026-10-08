package server

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/intakegroom"
	"github.com/kuhlman-labs/fishhawk/backend/internal/prompt"
	"github.com/kuhlman-labs/fishhawk/backend/internal/userreport"
	"github.com/kuhlman-labs/fishhawk/backend/internal/workmgmt"
)

const commsTestRepo = "acme/widgets"

func newCommsRecordServer(au *auditFake) *Server {
	return New(Config{Addr: "127.0.0.1:0", AuditRepo: au})
}

// commsSeed builds a pre-existing audit row for auditFake.seeded.
func commsSeed(t *testing.T, category string, runID uuid.UUID, account *uuid.UUID, payload any) *audit.Entry {
	t.Helper()
	b, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal seed: %v", err)
	}
	rid := runID
	return &audit.Entry{ID: uuid.New(), RunID: &rid, Category: category, Payload: b, AccountID: account}
}

func commsTestPayload(attempt string, ids ...string) commsScanGatheredPayload {
	base := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	p := commsScanGatheredPayload{StageAttempt: attempt, Repo: commsTestRepo}
	for i, id := range ids {
		p.Shown = append(p.Shown, commsShownReport{
			ID: id, Kind: "issue", IssueNumber: i + 1,
			ContentHash: strings.Repeat("a", 64), UpdatedAt: base.Add(time.Duration(i) * time.Minute),
		})
	}
	p.PendingCursor = &commsPendingCursor{Since: base.Add(-time.Hour), NoteSince: base.Add(-time.Hour), Cursor: base.Add(time.Hour), NoteCursor: base.Add(time.Hour)}
	return p
}

func TestCommsGatherDigest_DeterministicAndCoversStageAttempt(t *testing.T) {
	p := commsTestPayload("2026-10-01T12:00:00Z", "UR-issue-1")
	d1 := commsGatherDigest(p)
	if len(d1) != 64 || strings.ToLower(d1) != d1 {
		t.Fatalf("digest %q is not lowercase 64-hex", d1)
	}
	again := commsGatherDigest(p)
	if again != d1 {
		t.Fatalf("digest not deterministic: %s vs %s", again, d1)
	}
	stamped := p
	stamped.GatherDigest = "ignored"
	if d := commsGatherDigest(stamped); d != d1 {
		t.Fatalf("digest depends on gather_digest itself")
	}
	nilLists := commsScanGatheredPayload{Repo: commsTestRepo}
	emptyLists := commsScanGatheredPayload{Repo: commsTestRepo, Shown: []commsShownReport{}, Omitted: []string{},
		Suppressed: []commsSuppression{}, Degradations: []commsGatherDegradation{},
		Charter: commsCharterRecord{RubricIDs: []string{}, NonGoalIDs: []string{}}}
	dn := commsGatherDigest(nilLists)
	de := commsGatherDigest(emptyLists)
	if dn != de {
		t.Fatalf("nil and empty lists digest differently")
	}
	next := p
	next.StageAttempt = "2026-10-01T13:00:00Z"
	if d := commsGatherDigest(next); d == d1 {
		t.Fatalf("a new stage attempt must yield a new digest")
	}
	otherStage := p
	otherStage.StageID = uuid.New()
	if d := commsGatherDigest(otherStage); d == d1 {
		t.Fatalf("a different stage must yield a new digest")
	}
}

// TestRecordCommsScanGathered_OneRowPerDigest pins the dedupe-append: the
// same gather recorded twice appends ONE row; a different gather appends a
// second.
func TestRecordCommsScanGathered_OneRowPerDigest(t *testing.T) {
	au := newAuditFake()
	s := newCommsRecordServer(au)
	ctx := context.Background()
	runID, stageID := uuid.New(), uuid.New()

	p := commsTestPayload("a1", "UR-issue-1")
	p.StageID = uuid.New() // overwritten by the stage argument
	e1, appended, err := s.recordCommsScanGathered(ctx, runID, stageID, p)
	if err != nil || !appended || e1 == nil {
		t.Fatalf("first record: entry=%v appended=%v err=%v", e1, appended, err)
	}
	e2, appended, err := s.recordCommsScanGathered(ctx, runID, stageID, p)
	if err != nil || appended || e2 == nil {
		t.Fatalf("second record: entry=%v appended=%v err=%v", e2, appended, err)
	}
	if len(au.appended) != 1 {
		t.Fatalf("rows after two identical records = %d, want 1", len(au.appended))
	}
	row := au.appended[0]
	if row.Category != CategoryCommsScanGathered || row.StageID == nil || *row.StageID != stageID || row.RunID != runID {
		t.Fatalf("row = %+v", row)
	}
	got, err := decodeCommsScanGathered(row.Payload)
	if err != nil {
		t.Fatal(err)
	}
	if got.StageID != stageID {
		t.Fatalf("payload stage = %s, want the argument %s", got.StageID, stageID)
	}
	want := commsGatherDigest(*got)
	if got.GatherDigest != want || commsEntryDigest(e2) != want {
		t.Fatalf("recorded digest %q, existing-row digest %q, recomputed %q", got.GatherDigest, commsEntryDigest(e2), want)
	}
	if got.Omitted == nil || got.Suppressed == nil || got.Degradations == nil {
		t.Fatalf("lists recorded as null, want []: %s", row.Payload)
	}

	p2 := commsTestPayload("a1", "UR-issue-1", "UR-issue-2")
	if _, appended, err := s.recordCommsScanGathered(ctx, runID, stageID, p2); err != nil || !appended {
		t.Fatalf("new gather: appended=%v err=%v", appended, err)
	}
	if len(au.appended) != 2 {
		t.Fatalf("rows after a new gather = %d, want 2", len(au.appended))
	}
}

func TestRecordCommsScanGathered_Errors(t *testing.T) {
	ctx := context.Background()
	p := commsTestPayload("a1", "UR-issue-1")

	s := New(Config{Addr: "127.0.0.1:0"})
	if _, _, err := s.recordCommsScanGathered(ctx, uuid.New(), uuid.New(), p); err == nil {
		t.Fatal("nil AuditRepo: want error")
	}

	au := newAuditFake()
	au.listByCategoryErr = errors.New("boom")
	if _, _, err := newCommsRecordServer(au).recordCommsScanGathered(ctx, uuid.New(), uuid.New(), p); err == nil || !strings.Contains(err.Error(), "list prior rows") {
		t.Fatalf("list error: err=%v", err)
	}
	if len(au.appended) != 0 {
		t.Fatalf("a list failure appended %d rows", len(au.appended))
	}

	au = newAuditFake()
	au.appendErrCategory = CategoryCommsScanGathered
	if _, appended, err := newCommsRecordServer(au).recordCommsScanGathered(ctx, uuid.New(), uuid.New(), p); err == nil || appended {
		t.Fatalf("append error: appended=%v err=%v", appended, err)
	}
}

// TestLatestCommsScanGathered_StageFilter (C17) pins the stage filter: among
// rows of two stages the latest of EACH stage is returned. Rows are appended
// A, A, B so that without the filter stage A would read stage B's row.
func TestLatestCommsScanGathered_StageFilter(t *testing.T) {
	au := newAuditFake()
	s := newCommsRecordServer(au)
	ctx := context.Background()
	runID, stageA, stageB := uuid.New(), uuid.New(), uuid.New()

	for _, rec := range []struct {
		stage   uuid.UUID
		attempt string
	}{{stageA, "a-first"}, {stageA, "a-second"}, {stageB, "b-only"}} {
		if _, _, err := s.recordCommsScanGathered(ctx, runID, rec.stage, commsTestPayload(rec.attempt, "UR-issue-1")); err != nil {
			t.Fatal(err)
		}
	}
	_, pa, err := s.latestCommsScanGathered(ctx, runID, stageA)
	if err != nil {
		t.Fatalf("latest(A): %v", err)
	}
	if pa.StageAttempt != "a-second" || pa.StageID != stageA {
		t.Fatalf("latest(A) = attempt %q stage %s, want a-second on A", pa.StageAttempt, pa.StageID)
	}
	_, pb, err := s.latestCommsScanGathered(ctx, runID, stageB)
	if err != nil || pb.StageAttempt != "b-only" {
		t.Fatalf("latest(B) = %+v err=%v", pb, err)
	}
}

// TestLatestCommsScanGathered_HighestSequenceWins: a higher-sequence row
// listed before a lower one is still the latest.
func TestLatestCommsScanGathered_HighestSequenceWins(t *testing.T) {
	au := newAuditFake()
	runID, stageID := uuid.New(), uuid.New()
	for _, r := range []struct {
		seq     int64
		attempt string
	}{{7, "newer"}, {3, "older"}} {
		p := commsTestPayload(r.attempt).normalized()
		p.StageID = stageID
		e := commsSeed(t, CategoryCommsScanGathered, runID, nil, p)
		e.Sequence = r.seq
		e.StageID = &stageID
		au.seeded = append(au.seeded, e)
	}
	e, p, err := newCommsRecordServer(au).latestCommsScanGathered(context.Background(), runID, stageID)
	if err != nil || p.StageAttempt != "newer" || e.Sequence != 7 {
		t.Fatalf("latest = seq %v attempt %+v err=%v", e, p, err)
	}
}

func TestLatestCommsScanGathered_Errors(t *testing.T) {
	ctx := context.Background()
	runID, stageID := uuid.New(), uuid.New()

	// A store that did not answer is NOT errCommsScanUnbound: the comms ingest
	// maps it to a 500, never to scan_context_absent (#4015).
	if _, _, err := New(Config{Addr: "127.0.0.1:0"}).latestCommsScanGathered(ctx, runID, stageID); err == nil || errors.Is(err, errCommsScanUnbound) {
		t.Fatalf("nil AuditRepo: err=%v, want a non-unbound error", err)
	}
	au := newAuditFake()
	au.listByCategoryErr = errors.New("boom")
	if _, _, err := newCommsRecordServer(au).latestCommsScanGathered(ctx, runID, stageID); err == nil || errors.Is(err, errCommsScanUnbound) {
		t.Fatalf("list error: err=%v, want a non-unbound error", err)
	} else if !errors.Is(err, au.listByCategoryErr) {
		t.Errorf("list error: err=%v does not wrap the store error", err)
	}
	if _, _, err := newCommsRecordServer(newAuditFake()).latestCommsScanGathered(ctx, runID, stageID); err == nil || !strings.Contains(err.Error(), "no comms_scan_gathered row") || !errors.Is(err, errCommsScanUnbound) {
		t.Fatalf("absent: err=%v, want the unbound sentinel", err)
	}

	// The latest row carries an unknown field: strict decode refuses it rather
	// than falling back to an older row.
	au = newAuditFake()
	good := commsTestPayload("ok").normalized()
	good.StageID = stageID
	older := commsSeed(t, CategoryCommsScanGathered, runID, nil, good)
	older.StageID, older.Sequence = &stageID, 1
	bad := commsSeed(t, CategoryCommsScanGathered, runID, nil, map[string]any{"stage_id": stageID, "surprise": true})
	bad.StageID, bad.Sequence = &stageID, 2
	au.seeded = []*audit.Entry{older, bad}
	if _, _, err := newCommsRecordServer(au).latestCommsScanGathered(ctx, runID, stageID); err == nil || !strings.Contains(err.Error(), "unknown field") || !errors.Is(err, errCommsScanUnbound) {
		t.Fatalf("undecodable latest: err=%v, want the unbound sentinel", err)
	} else if strings.Contains(err.Error(), errCommsScanUnbound.Error()) {
		t.Errorf("undecodable latest: message %q gained the sentinel text; the message must stay unchanged", err)
	}

	// The entry's stage column and the payload's stage_id disagree.
	au = newAuditFake()
	other := commsTestPayload("x").normalized()
	other.StageID = uuid.New()
	mis := commsSeed(t, CategoryCommsScanGathered, runID, nil, other)
	mis.StageID = &stageID
	au.seeded = []*audit.Entry{mis}
	if _, _, err := newCommsRecordServer(au).latestCommsScanGathered(ctx, runID, stageID); err == nil || !strings.Contains(err.Error(), "row names stage") || !errors.Is(err, errCommsScanUnbound) {
		t.Fatalf("stage mismatch: err=%v, want the unbound sentinel", err)
	}
}

// TestCommsScanGatheredByDigest_Exact (C17): each digest returns its own row,
// and a prefix, an upper-cased digest or "" match nothing.
func TestCommsScanGatheredByDigest_Exact(t *testing.T) {
	au := newAuditFake()
	s := newCommsRecordServer(au)
	ctx := context.Background()
	runID, stageID := uuid.New(), uuid.New()
	var digests []string
	for _, attempt := range []string{"one", "two"} {
		if _, _, err := s.recordCommsScanGathered(ctx, runID, stageID, commsTestPayload(attempt, "UR-issue-1")); err != nil {
			t.Fatal(err)
		}
		p, _ := decodeCommsScanGathered(au.appended[len(au.appended)-1].Payload)
		digests = append(digests, p.GatherDigest)
	}
	for i, attempt := range []string{"one", "two"} {
		_, p, err := s.commsScanGatheredByDigest(ctx, runID, digests[i])
		if err != nil || p.StageAttempt != attempt || p.GatherDigest != digests[i] {
			t.Fatalf("byDigest(%d) = %+v err=%v", i, p, err)
		}
	}
	// A strictly decodable row with a BLANK digest isolates the "" guard: only
	// the guard keeps byDigest("") from returning it.
	blank := commsTestPayload("blank").normalized()
	au.seeded = append(au.seeded, commsSeed(t, CategoryCommsScanGathered, runID, nil, blank))
	for _, miss := range []string{digests[0][:63], strings.ToUpper(digests[0]), ""} {
		if _, _, err := s.commsScanGatheredByDigest(ctx, runID, miss); err == nil {
			t.Fatalf("byDigest(%q) matched, want error", miss)
		}
	}
	if _, _, err := s.commsScanGatheredByDigest(ctx, uuid.New(), digests[0]); err == nil {
		t.Fatal("another run's digest matched")
	}
}

func TestCommsScanGatheredByDigest_Errors(t *testing.T) {
	ctx := context.Background()
	if _, _, err := New(Config{Addr: "127.0.0.1:0"}).commsScanGatheredByDigest(ctx, uuid.New(), "d"); err == nil {
		t.Fatal("nil AuditRepo: want error")
	}
	au := newAuditFake()
	au.listByCategoryErr = errors.New("boom")
	if _, _, err := newCommsRecordServer(au).commsScanGatheredByDigest(ctx, uuid.New(), "d"); err == nil {
		t.Fatal("list error: want error")
	}
	// The matching row does not strictly decode.
	au = newAuditFake()
	runID := uuid.New()
	au.seeded = []*audit.Entry{commsSeed(t, CategoryCommsScanGathered, runID, nil, map[string]any{"gather_digest": "dd", "extra": 1})}
	if _, _, err := newCommsRecordServer(au).commsScanGatheredByDigest(ctx, runID, "dd"); err == nil || !strings.Contains(err.Error(), "unknown field") {
		t.Fatalf("undecodable match: err=%v", err)
	}
}

func TestDecodeCommsScanGathered_RefusesTrailingData(t *testing.T) {
	if _, err := decodeCommsScanGathered(json.RawMessage(`{"repo":"a/b"} {"repo":"c/d"}`)); err == nil {
		t.Fatal("trailing value: want error")
	}
	if _, err := decodeCommsScanGathered(json.RawMessage(`[`)); err == nil {
		t.Fatal("malformed: want error")
	}
	if commsEntryDigest(&audit.Entry{Payload: json.RawMessage(`nope`)}) != "" {
		t.Fatal("an undecodable row must read as no digest")
	}
}

// TestCommsGatherDigest_NilAndEmptyClustersDigestIdentically: no clusters
// records as [] (never null), so a nil and an empty list digest and record
// identically.
func TestCommsGatherDigest_NilAndEmptyClustersDigestIdentically(t *testing.T) {
	nilClusters := commsTestPayload("a1", "UR-issue-1")
	emptyClusters := nilClusters
	emptyClusters.SuggestedClusters = []commsRecordedCluster{}
	if dn, de := commsGatherDigest(nilClusters), commsGatherDigest(emptyClusters); dn != de {
		t.Fatalf("nil clusters digest %s != empty clusters digest %s", dn, de)
	}
	withCluster := nilClusters
	withCluster.SuggestedClusters = []commsRecordedCluster{{ReportIDs: []string{"UR-issue-1", "UR-issue-2"}, Score: 0.8}}
	if commsGatherDigest(withCluster) == commsGatherDigest(nilClusters) {
		t.Fatal("a recorded cluster must change the digest")
	}

	au := newAuditFake()
	if _, _, err := newCommsRecordServer(au).recordCommsScanGathered(context.Background(), uuid.New(), uuid.New(), nilClusters); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(au.appended[0].Payload), `"suggested_clusters":[]`) {
		t.Fatalf("nil clusters recorded as %s, want suggested_clusters []", au.appended[0].Payload)
	}
	if !commsGatheredClustersRecorded(au.appended[0].Payload) {
		t.Fatal("a row recorded with no clusters must still read as clusters-recorded")
	}
}

// TestRecordCommsScanGathered_RefusesUnencodablePayload: a payload Marshal
// rejects (a NaN score) is refused, not digested as empty bytes and appended
// with a null payload.
func TestRecordCommsScanGathered_RefusesUnencodablePayload(t *testing.T) {
	au := newAuditFake()
	p := commsTestPayload("a1", "UR-issue-1", "UR-issue-2")
	p.SuggestedClusters = []commsRecordedCluster{{ReportIDs: []string{"UR-issue-1", "UR-issue-2"}, Score: math.NaN()}}
	_, appended, err := newCommsRecordServer(au).recordCommsScanGathered(context.Background(), uuid.New(), uuid.New(), p)
	if err == nil || appended || !strings.Contains(err.Error(), "encode payload") {
		t.Fatalf("record = appended %v, err %v; want an encode refusal", appended, err)
	}
	if n := len(au.appended); n != 0 {
		t.Fatalf("an unencodable gather appended %d row(s), want 0", n)
	}
}

// TestCommsGatheredClustersRecorded_LegacyRow: a row recorded before #4016
// (no suggested_clusters key) still strictly decodes with nil clusters and
// probes not-recorded; a null value and an undecodable row probe
// not-recorded; a present key probes recorded. The stored gather_digest is
// authoritative: the legacy row re-digested no longer equals its stored
// digest, and the exact-digest lookup still finds it by the stored value.
func TestCommsGatheredClustersRecorded_LegacyRow(t *testing.T) {
	p := commsTestPayload("a1", "UR-issue-1").normalized()
	p.StageID = uuid.New()
	current, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	// The pre-#4016 struct is this one minus the field, so stripping its
	// encoding reproduces the old writer's bytes exactly.
	legacy := bytes.Replace(current, []byte(`"suggested_clusters":[],`), nil, 1)
	if bytes.Equal(legacy, current) {
		t.Fatalf("fixture: no suggested_clusters key to strip in %s", current)
	}
	sum := sha256.Sum256(legacy)
	legacyDigest := hex.EncodeToString(sum[:])
	p.GatherDigest = legacyDigest
	legacyRow, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	legacyRow = bytes.Replace(legacyRow, []byte(`"suggested_clusters":[],`), nil, 1)

	got, err := decodeCommsScanGathered(legacyRow)
	if err != nil {
		t.Fatalf("a legacy row must still strictly decode: %v", err)
	}
	if got.SuggestedClusters != nil {
		t.Fatalf("legacy row decoded clusters %v, want nil", got.SuggestedClusters)
	}
	if commsGatheredClustersRecorded(legacyRow) {
		t.Fatal("a legacy row must probe clusters-NOT-recorded")
	}
	if d := commsGatherDigest(*got); d == legacyDigest {
		t.Fatal("fixture: a re-digested legacy row should NOT equal its stored digest")
	}
	au := newAuditFake()
	runID := uuid.New()
	au.seeded = []*audit.Entry{{ID: uuid.New(), RunID: &runID, Category: CategoryCommsScanGathered, Payload: legacyRow}}
	if _, byDigest, err := newCommsRecordServer(au).commsScanGatheredByDigest(context.Background(), runID, legacyDigest); err != nil || byDigest.GatherDigest != legacyDigest {
		t.Fatalf("lookup by the STORED legacy digest = %v, %v", byDigest, err)
	}

	for name, raw := range map[string]string{
		"null":        `{"repo":"a/b","suggested_clusters":null}`,
		"undecodable": `nope`,
		"absent":      `{"repo":"a/b"}`,
	} {
		if commsGatheredClustersRecorded(json.RawMessage(raw)) {
			t.Errorf("%s: probe = true, want false", name)
		}
	}
	for name, raw := range map[string]string{
		"empty":   `{"repo":"a/b","suggested_clusters":[]}`,
		"present": `{"repo":"a/b","suggested_clusters":[{"report_ids":["UR-issue-1","UR-issue-2"],"score":1}]}`,
	} {
		if !commsGatheredClustersRecorded(json.RawMessage(raw)) {
			t.Errorf("%s: probe = false, want true", name)
		}
	}
	withKey, err := decodeCommsScanGathered(json.RawMessage(`{"repo":"a/b","suggested_clusters":[{"report_ids":["UR-issue-1","UR-issue-2"],"score":0.5}]}`))
	if err != nil || len(withKey.SuggestedClusters) != 1 || withKey.SuggestedClusters[0].Score != 0.5 {
		t.Fatalf("row with clusters decoded %+v, %v", withKey, err)
	}
}

func applyRow(repo string, sups ...commsSuppression) commsApplyCompletedRecord {
	return commsApplyCompletedRecord{Repo: repo, Suppressions: sups}
}

// TestCommsSuppressionMemory_HashMatch (C4): a prior suppression at the OLD
// hash does not suppress the report once its content changed.
func TestCommsSuppressionMemory_HashMatch(t *testing.T) {
	oldHash := userreport.ContentHash(workmgmt.UserReportKindIssue, "Crash", "on save")
	newHash := userreport.ContentHash(workmgmt.UserReportKindIssue, "Crash", "on save, with steps to reproduce")
	au := newAuditFake()
	au.seeded = []*audit.Entry{commsSeed(t, CategoryCommsApplyCompleted, uuid.New(), nil,
		applyRow(commsTestRepo, commsSuppression{ID: "UR-issue-4", Hash: oldHash, Basis: commsSuppressionBasisFiled}))}
	mem, err := newCommsRecordServer(au).loadCommsSuppressionMemory(context.Background(), nil, commsTestRepo)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := mem.lookup("UR-issue-4", oldHash); !ok {
		t.Fatal("unchanged report not suppressed (fixture precondition)")
	}
	if sup, ok := mem.lookup("UR-issue-4", newHash); ok {
		t.Fatalf("materially edited report suppressed by %+v; want it to re-enter", sup)
	}
}

// TestCommsSuppressionMemory_RepoFilter (C5): report ids collide across
// repositories, so another repo's suppression with the SAME id and hash must
// not suppress.
func TestCommsSuppressionMemory_RepoFilter(t *testing.T) {
	h := strings.Repeat("b", 64)
	au := newAuditFake()
	au.seeded = []*audit.Entry{
		commsSeed(t, CategoryCommsApplyCompleted, uuid.New(), nil,
			applyRow("acme/other", commsSuppression{ID: "UR-issue-1", Hash: h, Basis: commsSuppressionBasisRejected})),
		commsSeed(t, CategoryCommsApplyCompleted, uuid.New(), nil,
			applyRow(commsTestRepo, commsSuppression{ID: "UR-issue-2", Hash: h, Basis: commsSuppressionBasisNDrift})),
	}
	mem, err := newCommsRecordServer(au).loadCommsSuppressionMemory(context.Background(), nil, commsTestRepo)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := mem.lookup("UR-issue-1", h); ok {
		t.Fatal("another repository's suppression applied")
	}
	if sup, ok := mem.lookup("UR-issue-2", h); !ok || sup.Basis != commsSuppressionBasisNDrift {
		t.Fatalf("same-repo suppression missing: %+v %v", sup, ok)
	}
}

// TestCommsSuppressionMemory_AccountFilter (C5, approval condition 1).
//
// FIXTURE REQUIREMENT: the shared auditFake.ListAll ignores
// ListAllParams.AccountID, and rows appended through AppendChained carry a
// nil account. So the other-account row MUST be seeded through
// auditFake.seeded with an explicit NON-NIL AccountID, and the read must run
// under a different (or nil) account. Otherwise the fake hands back nothing
// to filter, and deleting the account filter would stay green.
func TestCommsSuppressionMemory_AccountFilter(t *testing.T) {
	h := strings.Repeat("c", 64)
	acctOther, acctRun := uuid.New(), uuid.New()
	au := newAuditFake()
	au.seeded = []*audit.Entry{
		commsSeed(t, CategoryCommsApplyCompleted, uuid.New(), &acctOther,
			applyRow(commsTestRepo, commsSuppression{ID: "UR-issue-1", Hash: h, Basis: commsSuppressionBasisFiled})),
		commsSeed(t, CategoryCommsApplyCompleted, uuid.New(), &acctRun,
			applyRow(commsTestRepo, commsSuppression{ID: "UR-issue-2", Hash: h, Basis: commsSuppressionBasisFiled})),
		commsSeed(t, CategoryCommsApplyCompleted, uuid.New(), nil,
			applyRow(commsTestRepo, commsSuppression{ID: "UR-issue-3", Hash: h, Basis: commsSuppressionBasisFiled})),
	}
	s := newCommsRecordServer(au)
	ctx := context.Background()

	// Run under a DIFFERENT non-nil account.
	mem, err := s.loadCommsSuppressionMemory(ctx, &acctRun, commsTestRepo)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := mem.lookup("UR-issue-1", h); ok {
		t.Fatal("another account's suppression applied to the run's account")
	}
	if _, ok := mem.lookup("UR-issue-3", h); ok {
		t.Fatal("a NULL-account row applied to a tenanted run")
	}
	if _, ok := mem.lookup("UR-issue-2", h); !ok {
		t.Fatal("the run's own account's suppression missing")
	}

	// Run under a NIL account: only nil-account rows apply.
	mem, err = s.loadCommsSuppressionMemory(ctx, nil, commsTestRepo)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := mem.lookup("UR-issue-1", h); ok {
		t.Fatal("a tenanted row applied to an untenanted run")
	}
	if _, ok := mem.lookup("UR-issue-3", h); !ok {
		t.Fatal("nil==nil: the untenanted suppression missing")
	}
}

// TestCommsSuppressionMemory_NotBoundedByRunCount (C5b): the walk reads the
// suppression category's rows, never "the last N runs", so one old
// suppression survives 150 newer runs that recorded no apply row.
func TestCommsSuppressionMemory_NotBoundedByRunCount(t *testing.T) {
	h := strings.Repeat("d", 64)
	au := newAuditFake()
	old := commsSeed(t, CategoryCommsApplyCompleted, uuid.New(), nil,
		applyRow(commsTestRepo, commsSuppression{ID: "UR-issue-1", Hash: h, Basis: commsSuppressionBasisFiled}))
	old.Timestamp = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	au.seeded = append(au.seeded, old)
	for i := 0; i < 150; i++ {
		e := commsSeed(t, CategoryCommsScanGathered, uuid.New(), nil, commsTestPayload("x").normalized())
		e.Timestamp = old.Timestamp.Add(time.Duration(i+1) * time.Hour)
		au.seeded = append(au.seeded, e)
	}
	mem, err := newCommsRecordServer(au).loadCommsSuppressionMemory(context.Background(), nil, commsTestRepo)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := mem.lookup("UR-issue-1", h); !ok {
		t.Fatal("an old suppression was lost behind 150 newer runs")
	}
}

func TestCommsSuppressionMemory_SkipsUndecodableAndEmptyEntries(t *testing.T) {
	h := strings.Repeat("e", 64)
	au := newAuditFake()
	broken := &audit.Entry{ID: uuid.New(), Category: CategoryCommsApplyCompleted, Payload: json.RawMessage(`{"repo":`)}
	au.seeded = []*audit.Entry{
		broken,
		commsSeed(t, CategoryCommsApplyCompleted, uuid.New(), nil, applyRow(commsTestRepo,
			commsSuppression{ID: "", Hash: h, Basis: commsSuppressionBasisFiled},
			commsSuppression{ID: "UR-issue-5", Hash: "", Basis: commsSuppressionBasisFiled},
			commsSuppression{ID: "UR-issue-6", Hash: h, Basis: commsSuppressionBasisRejected},
			commsSuppression{ID: "UR-issue-6", Hash: h, Basis: commsSuppressionBasisFiled},
		)),
	}
	mem, err := newCommsRecordServer(au).loadCommsSuppressionMemory(context.Background(), nil, commsTestRepo)
	if err != nil {
		t.Fatal(err)
	}
	if len(mem) != 1 {
		t.Fatalf("memory = %+v, want only UR-issue-6", mem)
	}
	if sup, _ := mem.lookup("UR-issue-6", h); sup.Basis != commsSuppressionBasisRejected {
		t.Fatalf("first basis must win, got %+v", sup)
	}
	if _, ok := mem.lookup("", h); ok {
		t.Fatal("an id-less suppression was kept")
	}
}

func TestCommsMemoryReads_DegradeNamed(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		name   string
		s      *Server
		reason string
		load   func(*Server) error
	}{
		{"suppression nil repo", New(Config{Addr: "127.0.0.1:0"}), commsDegradeSuppressionMemoryUnavailable,
			func(s *Server) error { _, err := s.loadCommsSuppressionMemory(ctx, nil, commsTestRepo); return err }},
		{"draft-filed nil repo", New(Config{Addr: "127.0.0.1:0"}), commsDegradeDraftFiledUnavailable,
			func(s *Server) error { _, err := s.loadCommsDraftFiled(ctx, nil, commsTestRepo); return err }},
		{"suppression list error", func() *Server {
			au := newAuditFake()
			au.listAllErrCategory = CategoryCommsApplyCompleted
			return newCommsRecordServer(au)
		}(), commsDegradeSuppressionMemoryUnavailable,
			func(s *Server) error { _, err := s.loadCommsSuppressionMemory(ctx, nil, commsTestRepo); return err }},
		{"draft-filed list error", func() *Server {
			au := newAuditFake()
			au.listAllErrCategory = CategoryCommsDraftFiled
			return newCommsRecordServer(au)
		}(), commsDegradeDraftFiledUnavailable,
			func(s *Server) error { _, err := s.loadCommsDraftFiled(ctx, nil, commsTestRepo); return err }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.load(tc.s)
			var d *commsMemoryUnavailableError
			if !errors.As(err, &d) {
				t.Fatalf("err = %v, want *commsMemoryUnavailableError", err)
			}
			if d.Reason != tc.reason || errors.Unwrap(d) == nil || !strings.Contains(d.Error(), tc.reason) {
				t.Fatalf("degrade = %+v (%v)", d, d)
			}
			want := commsGatherDegradation{Source: commsDegradeSourceAudit, Reason: tc.reason, Count: 1}
			if d.Degradation() != want {
				t.Fatalf("Degradation() = %+v, want %+v", d.Degradation(), want)
			}
		})
	}
	// A degraded suppression read still returns usable (empty) memory.
	mem, _ := New(Config{Addr: "127.0.0.1:0"}).loadCommsSuppressionMemory(ctx, nil, commsTestRepo)
	if mem == nil || len(mem) != 0 {
		t.Fatalf("degraded memory = %#v, want empty non-nil", mem)
	}
}

func TestLoadCommsDraftFiled_FiltersRepoAccountAndUndecodable(t *testing.T) {
	acct := uuid.New()
	rep := userreport.MarkedReport{ID: "UR-issue-1", ContentHash: strings.Repeat("f", 64)}
	au := newAuditFake()
	au.seeded = []*audit.Entry{
		commsSeed(t, CategoryCommsDraftFiled, uuid.New(), &acct, commsDraftFiledRecord{Repo: commsTestRepo, IssueNumber: 10, Reports: []userreport.MarkedReport{rep}}),
		commsSeed(t, CategoryCommsDraftFiled, uuid.New(), &acct, commsDraftFiledRecord{Repo: "acme/other", IssueNumber: 11, Reports: []userreport.MarkedReport{rep}}),
		commsSeed(t, CategoryCommsDraftFiled, uuid.New(), nil, commsDraftFiledRecord{Repo: commsTestRepo, IssueNumber: 12, Reports: []userreport.MarkedReport{rep}}),
		{ID: uuid.New(), Category: CategoryCommsDraftFiled, AccountID: &acct, Payload: json.RawMessage(`"not an object"`)},
	}
	got, err := newCommsRecordServer(au).loadCommsDraftFiled(context.Background(), &acct, commsTestRepo)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].IssueNumber != 10 || len(got[0].Reports) != 1 || got[0].Reports[0] != rep {
		t.Fatalf("draft-filed = %+v, want only issue 10", got)
	}
}

// --- the #4017 row cap ------------------------------------------------------

// commsCappedAudit wraps the shared auditFake: it records every
// ListAllParams and, unlike the shared fake, HONOURS Limit by keeping the
// NEWEST rows (ts descending), mirroring the Postgres repository's
// pushed-down LIMIT (pinned there by TestPostgres_ListAll_Limit).
type commsCappedAudit struct {
	*auditFake
	pmu    sync.Mutex
	params []audit.ListAllParams
}

func (c *commsCappedAudit) ListAll(ctx context.Context, p audit.ListAllParams) ([]*audit.Entry, error) {
	c.pmu.Lock()
	c.params = append(c.params, p)
	c.pmu.Unlock()
	out, err := c.auditFake.ListAll(ctx, p)
	if err != nil || p.Limit <= 0 {
		return out, err
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Timestamp.After(out[j].Timestamp) })
	if len(out) > p.Limit {
		out = out[:p.Limit]
	}
	return out, nil
}

func (c *commsCappedAudit) captured() []audit.ListAllParams {
	c.pmu.Lock()
	defer c.pmu.Unlock()
	return append([]audit.ListAllParams(nil), c.params...)
}

// withCommsMemoryMaxRows shrinks the cap for one NON-parallel test.
func withCommsMemoryMaxRows(t *testing.T, n int) {
	t.Helper()
	prev := commsMemoryMaxRows
	commsMemoryMaxRows = n
	t.Cleanup(func() { commsMemoryMaxRows = prev })
}

// commsCapSeed seeds one row of category per payload, the i-th at minute i,
// so a later index is a NEWER row.
func commsCapSeed(t *testing.T, au *auditFake, category string, account *uuid.UUID, payloads ...any) {
	t.Helper()
	base := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	for i, p := range payloads {
		e := commsSeed(t, category, uuid.New(), account, p)
		e.Timestamp = base.Add(time.Duration(i) * time.Minute)
		au.seeded = append(au.seeded, e)
	}
}

// TestListCommsCategory_PassesRowCap: both memory reads push
// commsMemoryMaxRows into ListAllParams.Limit (with the category and the
// run's account), so the bound is applied in the SQL rather than after a
// full read.
func TestListCommsCategory_PassesRowCap(t *testing.T) {
	acct := uuid.New()
	capped := &commsCappedAudit{auditFake: newAuditFake()}
	s := New(Config{Addr: "127.0.0.1:0", AuditRepo: capped})
	ctx := context.Background()
	if _, err := s.loadCommsSuppressionMemory(ctx, &acct, commsTestRepo); err != nil {
		t.Fatal(err)
	}
	if _, err := s.loadCommsDraftFiled(ctx, &acct, commsTestRepo); err != nil {
		t.Fatal(err)
	}
	got := capped.captured()
	if len(got) != 2 {
		t.Fatalf("ListAll calls = %d, want 2", len(got))
	}
	for i, wantCat := range []string{CategoryCommsApplyCompleted, CategoryCommsDraftFiled} {
		p := got[i]
		if p.Category == nil || *p.Category != wantCat || p.AccountID != acct.String() {
			t.Fatalf("call %d params = %+v, want category %s account %s", i, p, wantCat, acct)
		}
		if p.Limit != commsMemoryMaxRows || p.Limit <= 0 {
			t.Fatalf("call %d Limit = %d, want the row cap %d", i, p.Limit, commsMemoryMaxRows)
		}
	}
}

// commsAssertTruncated asserts err is the named truncation degrade.
func commsAssertTruncated(t *testing.T, err error, reason string, capRows int) {
	t.Helper()
	var tr *commsMemoryTruncatedError
	if !errors.As(err, &tr) {
		t.Fatalf("err = %v, want *commsMemoryTruncatedError", err)
	}
	var d commsMemoryDegrader
	if !errors.As(err, &d) {
		t.Fatal("a truncation error must satisfy commsMemoryDegrader")
	}
	want := commsGatherDegradation{Source: commsDegradeSourceAudit, Reason: reason, Count: capRows}
	if d.Degradation() != want {
		t.Fatalf("Degradation() = %+v, want %+v", d.Degradation(), want)
	}
	if !strings.Contains(err.Error(), reason) || !strings.Contains(err.Error(), fmt.Sprint(capRows)) {
		t.Fatalf("Error() = %q, want the reason and the cap named", err.Error())
	}
}

// TestLoadCommsSuppressionMemory_TruncatedDegradesNamed: five apply rows
// under a cap of three — the memory carries the three NEWEST rows'
// suppressions (the partial memory is kept, not discarded) AND the read
// names suppression_memory_truncated with count = cap.
func TestLoadCommsSuppressionMemory_TruncatedDegradesNamed(t *testing.T) {
	withCommsMemoryMaxRows(t, 3)
	h := strings.Repeat("1", 64)
	capped := &commsCappedAudit{auditFake: newAuditFake()}
	var rows []any
	for i := 1; i <= 5; i++ {
		rows = append(rows, applyRow(commsTestRepo, commsSuppression{ID: fmt.Sprintf("UR-issue-%d", i), Hash: h, Basis: commsSuppressionBasisFiled}))
	}
	commsCapSeed(t, capped.auditFake, CategoryCommsApplyCompleted, nil, rows...)
	mem, err := New(Config{Addr: "127.0.0.1:0", AuditRepo: capped}).loadCommsSuppressionMemory(context.Background(), nil, commsTestRepo)
	commsAssertTruncated(t, err, commsDegradeSuppressionMemoryTruncated, 3)
	for i := 1; i <= 5; i++ {
		_, ok := mem.lookup(fmt.Sprintf("UR-issue-%d", i), h)
		if want := i >= 3; ok != want {
			t.Fatalf("UR-issue-%d suppressed = %v, want %v (only the 3 newest rows are read)", i, ok, want)
		}
	}
}

// TestLoadCommsDraftFiled_TruncatedDegradesNamed: the draft-filed read keeps
// the three NEWEST records and names draft_filed_truncated.
func TestLoadCommsDraftFiled_TruncatedDegradesNamed(t *testing.T) {
	withCommsMemoryMaxRows(t, 3)
	capped := &commsCappedAudit{auditFake: newAuditFake()}
	var rows []any
	for i := 1; i <= 5; i++ {
		rows = append(rows, commsDraftFiledRecord{Repo: commsTestRepo, IssueNumber: 100 + i})
	}
	commsCapSeed(t, capped.auditFake, CategoryCommsDraftFiled, nil, rows...)
	got, err := New(Config{Addr: "127.0.0.1:0", AuditRepo: capped}).loadCommsDraftFiled(context.Background(), nil, commsTestRepo)
	commsAssertTruncated(t, err, commsDegradeDraftFiledTruncated, 3)
	var nums []int
	for _, r := range got {
		nums = append(nums, r.IssueNumber)
	}
	if fmt.Sprint(nums) != fmt.Sprint([]int{105, 104, 103}) {
		t.Fatalf("draft-filed issue numbers = %v, want the 3 newest [105 104 103]", nums)
	}
}

// TestLoadCommsSuppressionMemory_BelowCapNoDegrade: a read under the cap
// names nothing; a read returning EXACTLY the cap records the truncation
// (the reader cannot tell it from a cut read — the honest direction).
func TestLoadCommsSuppressionMemory_BelowCapNoDegrade(t *testing.T) {
	withCommsMemoryMaxRows(t, 3)
	h := strings.Repeat("2", 64)
	row := func(id string) any {
		return applyRow(commsTestRepo, commsSuppression{ID: id, Hash: h, Basis: commsSuppressionBasisRejected})
	}
	below := &commsCappedAudit{auditFake: newAuditFake()}
	commsCapSeed(t, below.auditFake, CategoryCommsApplyCompleted, nil, row("UR-issue-1"), row("UR-issue-2"))
	mem, err := New(Config{Addr: "127.0.0.1:0", AuditRepo: below}).loadCommsSuppressionMemory(context.Background(), nil, commsTestRepo)
	if err != nil {
		t.Fatalf("below-cap read err = %v, want nil", err)
	}
	if len(mem) != 2 {
		t.Fatalf("below-cap memory = %+v, want both rows", mem)
	}
	at := &commsCappedAudit{auditFake: newAuditFake()}
	commsCapSeed(t, at.auditFake, CategoryCommsApplyCompleted, nil, row("UR-issue-1"), row("UR-issue-2"), row("UR-issue-3"))
	mem, err = New(Config{Addr: "127.0.0.1:0", AuditRepo: at}).loadCommsSuppressionMemory(context.Background(), nil, commsTestRepo)
	commsAssertTruncated(t, err, commsDegradeSuppressionMemoryTruncated, 3)
	if len(mem) != 3 {
		t.Fatalf("at-cap memory = %+v, want all three rows", mem)
	}
}

// TestLoadCommsSuppressionMemory_CapCountsSiblingRepoRows (approval
// condition 7): the cap counts ACCOUNT-category rows before the in-app repo
// filter, so newer rows of a SIBLING repository consume it and the run's own
// older suppression is not read — with the truncation named.
func TestLoadCommsSuppressionMemory_CapCountsSiblingRepoRows(t *testing.T) {
	withCommsMemoryMaxRows(t, 3)
	h := strings.Repeat("3", 64)
	capped := &commsCappedAudit{auditFake: newAuditFake()}
	own := applyRow(commsTestRepo, commsSuppression{ID: "UR-issue-1", Hash: h, Basis: commsSuppressionBasisFiled})
	sib := applyRow("acme/sibling", commsSuppression{ID: "UR-issue-9", Hash: h, Basis: commsSuppressionBasisFiled})
	commsCapSeed(t, capped.auditFake, CategoryCommsApplyCompleted, nil, own, sib, sib, sib)
	mem, err := New(Config{Addr: "127.0.0.1:0", AuditRepo: capped}).loadCommsSuppressionMemory(context.Background(), nil, commsTestRepo)
	commsAssertTruncated(t, err, commsDegradeSuppressionMemoryTruncated, 3)
	if len(mem) != 0 {
		t.Fatalf("memory = %+v, want empty: three newer sibling-repo rows consumed the cap", mem)
	}
}

// TestCommsScan_TruncatedMemoryRecordedOnGather: a gather whose suppression
// read hits the cap records suppression_memory_truncated (count = cap) on the
// comms_scan_gathered payload and in the prompt's coverage block, AND still
// applies the partial memory it read — a report matched by a returned (newest)
// suppression stays suppressed while one whose suppression lay past the cap
// is shown.
func TestCommsScan_TruncatedMemoryRecordedOnGather(t *testing.T) {
	withCommsMemoryMaxRows(t, 3)
	var capped *commsCappedAudit
	f := newCSFixture(t, func(c *Config) {
		capped = &commsCappedAudit{auditFake: c.AuditRepo.(*auditFake)}
		c.AuditRepo = capped
	})
	a := csIssue(1, "Crash on save", "a", csBase.Add(1*time.Minute), commsExternalAuthor)
	old := csIssue(2, "Sync is slow", "b", csBase.Add(2*time.Minute), commsExternalAuthor)
	f.reader.items = []workmgmt.UserReportItem{a, old}
	sup := func(it workmgmt.UserReportItem) commsSuppression {
		return commsSuppression{ID: prompt.UserReportID(string(it.Kind), it.IssueNumber, 0),
			Hash: userreport.ContentHash(it.Kind, it.Title, it.Body), Basis: commsSuppressionBasisRejected}
	}
	pad := applyRow(commsTestRepo, commsSuppression{ID: "UR-issue-77", Hash: strings.Repeat("4", 64), Basis: commsSuppressionBasisFiled})
	// Oldest row suppresses `old` (past the cap); the newest suppresses `a`.
	commsCapSeed(t, f.au, CategoryCommsApplyCompleted, nil,
		applyRow(commsTestRepo, sup(old)), pad, pad, pad, applyRow(commsTestRepo, sup(a)))

	cc, g := f.gather(t)
	want := commsGatherDegradation{Source: commsDegradeSourceAudit, Reason: commsDegradeSuppressionMemoryTruncated, Count: 3}
	found := false
	for _, d := range g.Payload.Degradations {
		if d == want {
			found = true
		}
	}
	if !found {
		t.Fatalf("payload degradations %+v lack %+v", g.Payload.Degradations, want)
	}
	if line := "- degraded source audit: reason suppression_memory_truncated, count 3"; !strings.Contains(csBuild(t, cc), line) {
		t.Fatalf("prompt coverage block lacks %q", line)
	}
	if got := csShownIDs(g); !csEqual(got, []string{"UR-issue-2"}) {
		t.Fatalf("shown = %v, want only UR-issue-2 (UR-issue-1 suppressed by the partial memory; UR-issue-2's suppression lay past the cap)", got)
	}
	if len(capped.captured()) == 0 {
		t.Fatal("the gather never read through the capped repository (fixture precondition)")
	}
}

// --- marker trust ---------------------------------------------------------

var (
	commsInternalAuthor = workmgmt.ReportAuthor{Login: "maintainer", Association: "MEMBER", AssociationResolved: true, Internal: true}
	commsExternalAuthor = workmgmt.ReportAuthor{Login: "mallory", Association: "NONE", AssociationResolved: true}
	commsBotAuthor      = workmgmt.ReportAuthor{Login: "ci[bot]", Bot: true}
)

// commsClassified runs the REAL userreport.Classify over it, so the
// classification a test exercises is the one production assigns.
func commsClassified(it workmgmt.UserReportItem) userreport.ReportItem {
	res := userreport.Classify(it, userreport.ClassifyContext{})
	return userreport.ReportItem{UserReportItem: it, Classification: res.Class, Basis: res.Basis, MarkerFromExternal: res.MarkerFromExternal}
}

// commsVictim is the report an attacker wants suppressed, at its CURRENT
// hash.
func commsVictim() userreport.MarkedReport {
	return userreport.MarkedReport{
		ID:          prompt.UserReportID(string(workmgmt.UserReportKindIssue), 77, 0),
		ContentHash: userreport.ContentHash(workmgmt.UserReportKindIssue, "Data loss on sync", "My files vanished."),
	}
}

// derivesFromForgedBody renders a filed draft's body through the REAL
// intakegroom.RenderBody, whose Derives-from block quotes an attacker-chosen
// source title holding a well-formed comms marker for victim.
func derivesFromForgedBody(t *testing.T, draftBody string, victim userreport.MarkedReport) string {
	t.Helper()
	forged := userreport.DraftMarker([]userreport.MarkedReport{victim})
	if forged == "" {
		t.Fatal("fixture: forged marker did not render")
	}
	body := intakegroom.RenderBody(draftBody, intakegroom.Signals{
		DerivesFrom: []intakegroom.SourceItem{{Number: 9, Title: "Crash on save " + forged, InWindow: true}},
		Score:       intakegroom.Score{Unscored: true, CharterGap: "no rubric line fired"},
	})
	parsed, _ := userreport.ParseDraftMarkers(body)
	if !containsMarked(parsed, victim) {
		t.Fatalf("fixture: the Derives-from block does not carry a parseable marker for the victim:\n%s", body)
	}
	return body
}

func containsMarked(rs []userreport.MarkedReport, want userreport.MarkedReport) bool {
	for _, r := range rs {
		if r == want {
			return true
		}
	}
	return false
}

// TestTrustedCommsMarkers_NoDraftFiledRow (C2): a legitimately filed draft
// (fishhawk_filed) whose Derives-from block quotes a forged marker suppresses
// nothing when no comms_draft_filed row backs it.
func TestTrustedCommsMarkers_NoDraftFiledRow(t *testing.T) {
	v := commsVictim()
	item := commsClassified(workmgmt.UserReportItem{Kind: workmgmt.UserReportKindIssue, IssueNumber: 500,
		Title: "Draft", Body: derivesFromForgedBody(t, "Filed draft.", v), Author: commsInternalAuthor})
	if item.Classification != userreport.ClassFishhawkFiled {
		t.Fatalf("fixture: classification = %s, want fishhawk_filed", item.Classification)
	}
	trusted, _ := trustedCommsMarkers([]userreport.ReportItem{item}, nil)
	if _, ok := trusted.lookup(v.ID, v.ContentHash); ok {
		t.Fatal("a marker with no comms_draft_filed row suppressed the victim")
	}
}

// TestTrustedCommsMarkers_RowNamesOnlyLegitEntries (C3): the draft-filed row
// names only A; the body carries A's legit marker AND the Derives-from forged
// marker for V. Only A is trusted.
func TestTrustedCommsMarkers_RowNamesOnlyLegitEntries(t *testing.T) {
	v := commsVictim()
	a := userreport.MarkedReport{ID: "UR-issue-3", ContentHash: userreport.ContentHash(workmgmt.UserReportKindIssue, "Slow start", "takes 30s")}
	body := userreport.DraftMarker([]userreport.MarkedReport{a}) + "\n\n" + derivesFromForgedBody(t, "Filed draft.", v)
	item := commsClassified(workmgmt.UserReportItem{Kind: workmgmt.UserReportKindIssue, IssueNumber: 501, Body: body, Author: commsInternalAuthor})
	if item.Classification != userreport.ClassFishhawkFiled {
		t.Fatalf("fixture: classification = %s", item.Classification)
	}
	rows := []commsDraftFiledRecord{{Repo: commsTestRepo, IssueNumber: 501, Reports: []userreport.MarkedReport{a}}}
	trusted, malformed := trustedCommsMarkers([]userreport.ReportItem{item}, rows)
	if sup, ok := trusted.lookup(a.ID, a.ContentHash); !ok || sup.Basis != commsSuppressionBasisMarker {
		t.Fatalf("legit marker entry not trusted: %+v %v", sup, ok)
	}
	if _, ok := trusted.lookup(v.ID, v.ContentHash); ok {
		t.Fatal("the forged Derives-from entry was trusted because a row exists for the item")
	}
	if len(trusted) != 1 || malformed != 0 {
		t.Fatalf("trusted = %+v malformed = %d", trusted, malformed)
	}
}

// TestTrustedCommsMarkers_ExternalForgedMarkerNotHonored (C1, the gate's
// home): an external author's item carries a well-formed marker for V, and a
// comms_draft_filed row naming THAT item and V exists — so the audit
// cross-check is satisfied and only the classification gate stands between
// the marker and suppression.
func TestTrustedCommsMarkers_ExternalForgedMarkerNotHonored(t *testing.T) {
	v := commsVictim()
	marker := userreport.DraftMarker([]userreport.MarkedReport{v})
	items := []userreport.ReportItem{
		commsClassified(workmgmt.UserReportItem{Kind: workmgmt.UserReportKindIssue, IssueNumber: 600, Body: marker, Author: commsExternalAuthor}),
		commsClassified(workmgmt.UserReportItem{Kind: workmgmt.UserReportKindComment, IssueNumber: 601, CommentID: 9001, Body: marker, Author: commsExternalAuthor, System: true}),
		{UserReportItem: workmgmt.UserReportItem{Kind: workmgmt.UserReportKindIssue, IssueNumber: 602, Body: marker, Author: commsBotAuthor}, Classification: userreport.ClassBot},
		{UserReportItem: workmgmt.UserReportItem{Kind: workmgmt.UserReportKindIssue, IssueNumber: 603, Body: marker, Author: commsInternalAuthor}, Classification: userreport.ClassInternal},
	}
	if items[0].Classification != userreport.ClassExternal || !items[0].MarkerFromExternal {
		t.Fatalf("fixture: external item classified %+v", items[0])
	}
	var rows []commsDraftFiledRecord
	for _, it := range items {
		rows = append(rows, commsDraftFiledRecord{Repo: commsTestRepo, IssueNumber: it.IssueNumber, CommentID: it.CommentID, Reports: []userreport.MarkedReport{v}})
	}
	trusted, _ := trustedCommsMarkers(items, rows)
	if _, ok := trusted.lookup(v.ID, v.ContentHash); ok {
		t.Fatal("a marker on a non-fishhawk_filed item suppressed the victim")
	}
}

// TestTrustedCommsMarkers_RowMustMatchItemAndHash: a row for ANOTHER item, or
// naming the report at a different hash, does not trust the entry; a
// comment-kind filed item matches on its comment id.
func TestTrustedCommsMarkers_RowMustMatchItemAndHash(t *testing.T) {
	a := userreport.MarkedReport{ID: "UR-comment-4-44", ContentHash: strings.Repeat("1", 64)}
	body := userreport.DraftMarker([]userreport.MarkedReport{a})
	issueItem := commsClassified(workmgmt.UserReportItem{Kind: workmgmt.UserReportKindIssue, IssueNumber: 700, Body: body, Author: commsInternalAuthor})
	commentItem := commsClassified(workmgmt.UserReportItem{Kind: workmgmt.UserReportKindComment, IssueNumber: 701, CommentID: 55, Body: body, Author: commsInternalAuthor})

	otherItem := []commsDraftFiledRecord{{IssueNumber: 999, Reports: []userreport.MarkedReport{a}}}
	if trusted, _ := trustedCommsMarkers([]userreport.ReportItem{issueItem}, otherItem); len(trusted) != 0 {
		t.Fatalf("a row for another item trusted %+v", trusted)
	}
	otherHash := []commsDraftFiledRecord{{IssueNumber: 700, Reports: []userreport.MarkedReport{{ID: a.ID, ContentHash: strings.Repeat("2", 64)}}}}
	if trusted, _ := trustedCommsMarkers([]userreport.ReportItem{issueItem}, otherHash); len(trusted) != 0 {
		t.Fatalf("a row naming another hash trusted %+v", trusted)
	}
	issueRowForComment := []commsDraftFiledRecord{{IssueNumber: 701, Reports: []userreport.MarkedReport{a}}}
	if trusted, _ := trustedCommsMarkers([]userreport.ReportItem{commentItem}, issueRowForComment); len(trusted) != 0 {
		t.Fatalf("an issue-level row trusted a comment item's marker: %+v", trusted)
	}
	commentRow := []commsDraftFiledRecord{
		{IssueNumber: 701, CommentID: 55, Reports: nil},
		{IssueNumber: 701, CommentID: 55, Reports: []userreport.MarkedReport{a}},
	}
	if trusted, _ := trustedCommsMarkers([]userreport.ReportItem{commentItem}, commentRow); len(trusted) != 1 {
		t.Fatalf("matching comment row did not trust the entry: %+v", trusted)
	}
}

// TestTrustedCommsMarkers_MalformedCountedOnlyOnParsedItems: malformed
// markers are counted for observability, only on items that are parsed.
func TestTrustedCommsMarkers_MalformedCountedOnlyOnParsedItems(t *testing.T) {
	broken := userreport.CommsMarkerPrefix + `{"reports":[` // unterminated
	filed := commsClassified(workmgmt.UserReportItem{Kind: workmgmt.UserReportKindIssue, IssueNumber: 800, Body: broken, Author: commsInternalAuthor})
	ext := commsClassified(workmgmt.UserReportItem{Kind: workmgmt.UserReportKindIssue, IssueNumber: 801, Body: broken, Author: commsExternalAuthor})
	_, malformed := trustedCommsMarkers([]userreport.ReportItem{filed, ext}, nil)
	if malformed != 1 {
		t.Fatalf("malformed = %d, want 1 (the external item is never parsed)", malformed)
	}
}

// --- cursor hold-back (C11) -----------------------------------------------

func TestCommsCursorHoldBack(t *testing.T) {
	t0 := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	at := func(h int) time.Time { return t0.Add(time.Duration(h) * time.Hour) }
	row := commsScanGatheredPayload{
		Shown: []commsShownReport{
			{ID: "UR-issue-1", UpdatedAt: at(3)},
			{ID: "UR-issue-2", UpdatedAt: at(5)},
			{ID: "UR-comment-2-9", UpdatedAt: at(1)}, // listed through the note floor, before Since
			{ID: "UR-comment-2-8", UpdatedAt: at(-1)},
		},
		PendingCursor: &commsPendingCursor{Since: at(2), NoteSince: at(0), Cursor: at(8), NoteCursor: at(7)},
	}
	cases := []struct {
		name             string
		ids              []string
		cursor, noteCurs time.Time
	}{
		{"nothing held", nil, at(8), at(7)},
		{"one id holds both bounds", []string{"UR-issue-2"}, at(5), at(5)},
		{"held cursor EQUALS the report's updated_at (inclusive re-read)", []string{"UR-issue-1"}, at(3), at(3)},
		{"min over several ids", []string{"UR-issue-2", "UR-issue-1"}, at(3), at(3)},
		{"unknown id: read bounds, no movement", []string{"UR-issue-2", "UR-issue-404"}, at(2), at(0)},
		{"cursor never retreats behind since; note held to the report", []string{"UR-comment-2-9"}, at(2), at(1)},
		{"note never retreats behind note_since", []string{"UR-comment-2-8"}, at(2), at(0)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c, n, ok := commsCursorHoldBack(row, tc.ids)
			if !ok || !c.Equal(tc.cursor) || !n.Equal(tc.noteCurs) {
				t.Fatalf("holdBack(%v) = (%s, %s, %v), want (%s, %s, true)", tc.ids, c, n, ok, tc.cursor, tc.noteCurs)
			}
			if n.After(c) {
				t.Fatalf("note cursor %s after cursor %s", n, c)
			}
		})
	}

	if _, _, ok := commsCursorHoldBack(commsScanGatheredPayload{Shown: row.Shown}, []string{"UR-issue-1"}); ok {
		t.Fatal("absent pending cursor: want ok=false (no advance)")
	}

	// A row whose note_since exceeds since still yields note <= cursor.
	inverted := commsScanGatheredPayload{PendingCursor: &commsPendingCursor{Since: at(2), NoteSince: at(4), Cursor: at(3), NoteCursor: at(3)}}
	c, n, ok := commsCursorHoldBack(inverted, nil)
	if !ok || n.After(c) {
		t.Fatalf("inverted row: (%s, %s, %v), want note <= cursor", c, n, ok)
	}
}
