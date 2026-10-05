package server

// Unit tests for the in-flight advisory pass (E80.6 / #3763) over fakes: every
// degrade reason, every per-run skip reason, the slot, the dedupe and the
// apply trigger. NOT parallel: they swap the pass's package-level seams.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/approval"
	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/crewmessage"
	"github.com/kuhlman-labs/fishhawk/backend/internal/forge"
	"github.com/kuhlman-labs/fishhawk/backend/internal/plan"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
	"github.com/kuhlman-labs/fishhawk/backend/internal/timescale"
	"github.com/kuhlman-labs/fishhawk/backend/internal/upkeep"
)

const (
	ifRepo      = "kuhlman-labs/fishhawk"
	ifFindingID = "advisory:GO-2024-2687:golang.org/x/net"
	ifHeadSHA   = "1111111111111111111111111111111111111111"
	ifGoModPath = "backend/go.mod"
	// ifCallerSentinel is a caller-frame function name that must never reach
	// a sent message.
	ifCallerSentinel = "serveH2SentinelCaller"
)

// ifGoModHead is backend/go.mod at the target run's head: x/net v0.22.0
// (affected, below the v0.23.0 fix) inside the require block.
var ifGoModHead = strings.Join([]string{
	"module github.com/kuhlman-labs/fishhawk/backend",
	"",
	"go 1.25.0",
	"",
	"require (",
	"\tgolang.org/x/net v0.22.0",
	")",
	"",
}, "\n")

// ifGoModPatch bumps x/net v0.21.0 -> v0.22.0 in backend/go.mod.
func ifGoModPatch(path, from, to string) string {
	return strings.Join([]string{
		"diff --git a/" + path + " b/" + path,
		"--- a/" + path,
		"+++ b/" + path,
		"@@ -5,3 +5,3 @@",
		" require (",
		"-\tgolang.org/x/net " + from,
		"+\tgolang.org/x/net " + to,
		" )",
		"",
	}, "\n")
}

// ifRunRepo serves a fixed ListRuns listing on top of approvalRunRepo.
type ifRunRepo struct {
	*approvalRunRepo
	mu      sync.Mutex
	list    []*run.Run
	listErr error
	filters []run.ListRunsFilter
}

func (r *ifRunRepo) ListRuns(_ context.Context, f run.ListRunsFilter) ([]*run.Run, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.filters = append(r.filters, f)
	if r.listErr != nil {
		return nil, r.listErr
	}
	if f.Offset >= len(r.list) {
		return nil, nil
	}
	end := f.Offset + f.Limit
	if end > len(r.list) {
		end = len(r.list)
	}
	return r.list[f.Offset:end], nil
}

// ifSender is the fake mailbox: it records every send AND appends the
// crew_message_sent chain row the real Mailbox would, so the chain dedupe
// reads what was sent.
type ifSender struct {
	mu    sync.Mutex
	au    audit.Repository
	sends []crewmessage.SendParams
	err   error
	delay time.Duration
}

func (f *ifSender) Send(ctx context.Context, p crewmessage.SendParams) (*crewmessage.Row, error) {
	if f.delay > 0 {
		time.Sleep(f.delay)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.err != nil {
		return nil, f.err
	}
	var m crewmessage.Message
	if err := json.Unmarshal(p.RawMessage, &m); err != nil {
		return nil, err
	}
	target := uuid.MustParse(m.Anchor.RunID)
	payload, _ := json.Marshal(map[string]json.RawMessage{"message": p.RawMessage})
	e, err := f.au.AppendChained(context.WithoutCancel(ctx), audit.ChainAppendParams{
		RunID: target, Timestamp: time.Now().UTC(), Category: crewmessage.CategorySent, Payload: payload,
	})
	if err != nil {
		return nil, err
	}
	f.sends = append(f.sends, p)
	return &crewmessage.Row{SentSequence: e.Sequence}, nil
}

func (f *ifSender) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.sends)
}

func (f *ifSender) messages(t *testing.T) []crewmessage.Message {
	t.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []crewmessage.Message
	for _, p := range f.sends {
		var m crewmessage.Message
		if err := json.Unmarshal(p.RawMessage, &m); err != nil {
			t.Fatal(err)
		}
		out = append(out, m)
	}
	return out
}

// ifForge serves one compare + one manifest per head SHA.
type ifForge struct {
	mu         sync.Mutex
	patch      string
	files      []forge.ComparePatchFile
	content    map[string]string // path -> head content
	compareErr error
	fetchErr   error
	block      chan struct{} // non-nil: ComparePatch waits for close or ctx
	entered    chan struct{} // closed on the first ComparePatch
	panicMsg   string
	compares   int
	fetchRefs  []string
}

func (f *ifForge) ComparePatch(ctx context.Context, _ forge.CredentialScope, _ forge.RepoRef, _, _ string) (*forge.ComparePatchResult, error) {
	f.mu.Lock()
	f.compares++
	if f.entered != nil && f.compares == 1 {
		close(f.entered)
	}
	f.mu.Unlock()
	if f.panicMsg != "" {
		panic(f.panicMsg)
	}
	if f.block != nil {
		select {
		case <-f.block:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if f.compareErr != nil {
		return nil, f.compareErr
	}
	return &forge.ComparePatchResult{Patch: f.patch, Files: f.files}, nil
}

func (f *ifForge) FetchFile(_ context.Context, _ forge.CredentialScope, _ forge.RepoRef, p, ref string) (*forge.FileContent, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.fetchRefs = append(f.fetchRefs, ref)
	if f.fetchErr != nil {
		return nil, f.fetchErr
	}
	c, ok := f.content[p]
	if !ok {
		return nil, forge.ErrNotFound
	}
	return &forge.FileContent{Path: p, Content: []byte(c)}, nil
}

type ifFixture struct {
	s         *Server
	au        *ukApplyAudit
	runs      *ifRunRepo
	sender    *ifSender
	forge     *ifForge
	scan      *run.Run
	stageID   uuid.UUID
	artID     string
	report    *plan.UpkeepReport
	consumed  map[string]upkeepConsumedDisposition
	forgeNote string // non-empty: the forge seam reports unavailable
}

// ifAdvisoryReport is one approved go advisory citing backend/go.mod.
func ifAdvisoryReport(inUse, fixed string) *plan.UpkeepReport {
	var fx *string
	if fixed != "" {
		fx = &fixed
	}
	return &plan.UpkeepReport{Findings: []plan.UpkeepFinding{{
		ID: ifFindingID, Source: plan.UpkeepSourceAdvisory, Subject: "golang.org/x/net",
		Evidence: []plan.UpkeepEvidenceRef{{Kind: plan.UpkeepEvidenceKindFile, Path: ifGoModPath}},
		Advisory: &plan.UpkeepAdvisory{
			Ecosystem: "go", Package: "golang.org/x/net", Version: inUse,
			AdvisoryIDs: []string{"GO-2024-2687", "CVE-2023-45288"}, FixedVersion: fx,
			Scanner: "govulncheck", Reachability: "called", Severity: "high",
			CallPath: []plan.UpkeepAdvisoryFrame{
				{Module: "golang.org/x/net", Package: "golang.org/x/net/http2", Function: "ReadFrame"},
				{Module: "github.com/kuhlman-labs/fishhawk/backend", Package: "github.com/kuhlman-labs/fishhawk/backend/internal/server",
					Function: ifCallerSentinel, Position: &plan.UpkeepAdvisoryPosition{Filename: "internal/server/sentinel_caller.go"}},
			},
		},
	}}}
}

func newIfFixture(t *testing.T) *ifFixture {
	t.Helper()
	au := &ukApplyAudit{groomingApplyAuditFake: &groomingApplyAuditFake{approvalAuditFake: newApprovalAuditFake()}, listErrCategories: map[string]error{}}
	rr := &ifRunRepo{approvalRunRepo: newApprovalRunRepo()}
	scan := &run.Run{ID: uuid.New(), Repo: ifRepo, State: run.StateRunning}
	rr.seedRun(scan)
	f := &ifFixture{
		au: au, runs: rr, scan: scan, stageID: uuid.New(), artID: uuid.NewString(),
		sender:   &ifSender{au: au},
		forge:    &ifForge{content: map[string]string{ifGoModPath: ifGoModHead}},
		report:   ifAdvisoryReport("v0.22.0", "v0.23.0"),
		consumed: map[string]upkeepConsumedDisposition{ifFindingID: {Verdict: upkeepVerdictApproved}},
	}
	f.forge.patch = ifGoModPatch(ifGoModPath, "v0.21.0", "v0.22.0")
	f.forge.files = []forge.ComparePatchFile{{Path: ifGoModPath, Status: "modified"}}
	f.s = New(Config{
		Addr: "127.0.0.1:0", AuditRepo: au, RunRepo: rr,
		DocumentBaseRef: func(context.Context, forge.RepoRef) (string, error) { return "main", nil },
		Logger:          slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil)),
	})
	f.appendOn(t, scan.ID, CategoryUpkeepReportRecorded, map[string]any{"run_id": scan.ID.String(), "artifact_id": f.artID})

	prevMailbox, prevForge, prevSlot := upkeepInflightMailbox, upkeepInflightForge, upkeepInflightSlot
	upkeepInflightMailbox = func(*Server) upkeepInflightSender { return f.sender }
	upkeepInflightForge = func(_ *Server, _ *run.Run) (patchComparer, forge.FileFetcher, forge.CredentialScope, forge.RepoRef, string) {
		if f.forgeNote != "" {
			return nil, nil, forge.CredentialScope{}, forge.RepoRef{}, f.forgeNote
		}
		return f.forge, f.forge, forge.CredentialScope{}, forge.RepoRef{Owner: "kuhlman-labs", Name: "fishhawk"}, ""
	}
	upkeepInflightSlot = make(chan struct{}, 1)
	t.Cleanup(func() {
		upkeepInflightMailbox, upkeepInflightForge, upkeepInflightSlot = prevMailbox, prevForge, prevSlot
	})
	return f
}

func (f *ifFixture) appendOn(t *testing.T, runID uuid.UUID, category string, payload any) {
	t.Helper()
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.au.AppendChained(context.Background(), audit.ChainAppendParams{
		RunID: runID, Timestamp: time.Now().UTC(), Category: category, Payload: raw,
	}); err != nil {
		t.Fatalf("seed %s: %v", category, err)
	}
}

// target adds a RUNNING target run with a recorded head to the listing.
func (f *ifFixture) target(t *testing.T, mutate func(*run.Run)) *run.Run {
	t.Helper()
	rn := &run.Run{ID: uuid.New(), Repo: ifRepo, State: run.StateRunning}
	if mutate != nil {
		mutate(rn)
	}
	f.runs.seedRun(rn)
	f.runs.list = append(f.runs.list, rn)
	f.appendOn(t, rn.ID, "pull_request_opened", map[string]any{"head_sha": ifHeadSHA})
	return rn
}

// pass starts the pass through the production entry point and waits.
func (f *ifFixture) pass(t *testing.T) {
	t.Helper()
	f.s.startUpkeepInflightPass(context.Background(), f.scan.ID, f.stageID, f.artID, f.report, f.consumed)
	f.s.waitUpkeepInflight()
}

func (f *ifFixture) summaries(t *testing.T) []upkeepInflightPassPayload {
	t.Helper()
	rows, err := f.au.groomingApplyAuditFake.ListForRunByCategory(context.Background(), f.scan.ID, CategoryUpkeepInflightPassCompleted)
	if err != nil {
		t.Fatal(err)
	}
	var out []upkeepInflightPassPayload
	for _, e := range rows {
		var p upkeepInflightPassPayload
		if err := json.Unmarshal(e.Payload, &p); err != nil {
			t.Fatal(err)
		}
		out = append(out, p)
	}
	return out
}

func (f *ifFixture) summary(t *testing.T) upkeepInflightPassPayload {
	t.Helper()
	s := f.summaries(t)
	if len(s) != 1 {
		t.Fatalf("upkeep_inflight_pass_completed rows = %d, want 1", len(s))
	}
	return s[0]
}

func requireIfDegraded(t *testing.T, p upkeepInflightPassPayload, reason string) {
	t.Helper()
	if !p.Degraded || p.DegradeReason != reason {
		t.Fatalf("summary degraded=%v reason=%q, want degraded %q (%+v)", p.Degraded, p.DegradeReason, reason, p)
	}
}

func requireIfSkip(t *testing.T, f *ifFixture, reason string) {
	t.Helper()
	p := f.summary(t)
	if p.SkippedRuns[reason] != 1 {
		t.Fatalf("skipped_runs = %v, want %s: 1", p.SkippedRuns, reason)
	}
	if n := f.sender.count(); n != 0 {
		t.Fatalf("sends = %d, want 0 on a %s skip", n, reason)
	}
	if p.Degraded {
		t.Fatalf("a per-run skip must not degrade the pass: %+v", p)
	}
}

// TestUpkeepInflight_SendsOneFindingToMatchingRun is the positive control:
// one finding, security -> reviewer, anchored on the target, system actor,
// fetched at the head COMMIT, and the caller frame never rendered.
func TestUpkeepInflight_SendsOneFindingToMatchingRun(t *testing.T) {
	f := newIfFixture(t)
	tgt := f.target(t, nil)
	f.pass(t)

	msgs := f.sender.messages(t)
	if len(msgs) != 1 {
		t.Fatalf("sends = %d, want 1", len(msgs))
	}
	m := msgs[0]
	if m.Type != crewmessage.TypeFinding || m.SenderRole != crewmessage.RoleSecurity || m.RecipientRole != crewmessage.RoleReviewer {
		t.Fatalf("message = %s %s->%s, want finding security->reviewer", m.Type, m.SenderRole, m.RecipientRole)
	}
	if m.Anchor.RunID != tgt.ID.String() {
		t.Fatalf("anchor = %q, want the target run %s", m.Anchor.RunID, tgt.ID)
	}
	if m.ResponseRequired {
		t.Fatal("an in-flight finding must not require a response")
	}
	if p := f.sender.sends[0]; p.Actor.Kind != audit.ActorSystem || p.Actor.Subject != upkeepInflightActorSubject || p.StageID != nil {
		t.Fatalf("actor = %+v stage = %v, want system %s and no stage", p.Actor, p.StageID, upkeepInflightActorSubject)
	}
	for _, want := range []string{"GO-2024-2687", "CVE-2023-45288", "v0.23.0", "v0.22.0", "golang.org/x/net/http2.ReadFrame"} {
		if !strings.Contains(m.Payload.Summary+m.Payload.Detail, want) {
			t.Errorf("message does not name %q:\n%s\n%s", want, m.Payload.Summary, m.Payload.Detail)
		}
	}
	for _, leak := range []string{ifCallerSentinel, "sentinel_caller.go"} {
		if strings.Contains(m.Payload.Summary+m.Payload.Detail, leak) {
			t.Errorf("caller frame %q reached the message", leak)
		}
	}
	if m.Payload.Severity != "high" {
		t.Errorf("severity = %q, want high", m.Payload.Severity)
	}
	if len(f.forge.fetchRefs) != 1 || f.forge.fetchRefs[0] != ifHeadSHA {
		t.Errorf("manifest fetched at %v, want the head commit %s", f.forge.fetchRefs, ifHeadSHA)
	}
	p := f.summary(t)
	if p.Degraded || len(p.Sent) != 1 || p.Sent[0].RunID != tgt.ID.String() || p.Sent[0].AdvisoryID != "GO-2024-2687" ||
		p.Sent[0].FindingID != ifFindingID || p.RunsExamined != 1 || p.RunsListed != 1 || p.BaseRef != "main" {
		t.Fatalf("summary = %+v", p)
	}
	if fl := f.runs.filters; len(fl) == 0 || fl[0].State != string(run.StateRunning) || fl[0].Repo != ifRepo {
		t.Fatalf("listing filter = %+v, want running runs of %s", fl, ifRepo)
	}
}

// TestUpkeepInflight_RerunSendsNothingNew pins the chain dedupe (C11's unit
// twin): a second pass over the same diff records already_sent.
func TestUpkeepInflight_RerunSendsNothingNew(t *testing.T) {
	f := newIfFixture(t)
	tgt := f.target(t, nil)
	f.pass(t)
	f.pass(t)
	if n := f.sender.count(); n != 1 {
		t.Fatalf("sends after two passes = %d, want 1", n)
	}
	s := f.summaries(t)
	if len(s) != 2 || len(s[1].Sent) != 0 || len(s[1].AlreadySent) != 1 || s[1].AlreadySent[0].RunID != tgt.ID.String() {
		t.Fatalf("second summary = %+v, want one already_sent for %s", s, tgt.ID)
	}
}

// TestUpkeepInflight_DedupeIgnoresNonSecuritySender (C10): a PLANNER-sent
// finding with the identical summary does not suppress the security finding.
func TestUpkeepInflight_DedupeIgnoresNonSecuritySender(t *testing.T) {
	f := newIfFixture(t)
	tgt := f.target(t, nil)
	advs := upkeepInflightAdvisories(f.report, f.consumed)
	planted := crewmessage.Message{
		SchemaVersion: crewmessage.SchemaVersion, Type: crewmessage.TypeFinding,
		SenderRole: crewmessage.RolePlanner, RecipientRole: crewmessage.RoleReviewer,
		Anchor:  crewmessage.Anchor{RunID: tgt.ID.String()},
		Payload: crewmessage.Payload{Summary: upkeep.InFlightFindingSummary(advs[0])},
	}
	raw, _ := json.Marshal(planted)
	f.appendOn(t, tgt.ID, crewmessage.CategorySent, map[string]json.RawMessage{"message": raw})
	f.pass(t)
	if n := f.sender.count(); n != 1 {
		t.Fatalf("sends = %d, want 1: a non-security finding must not suppress the security one", n)
	}
}

// TestUpkeepInflight_ScanRunNeverTargeted (C7): the scan run is in the
// listing with a matching diff and receives nothing, and is not counted.
func TestUpkeepInflight_ScanRunNeverTargeted(t *testing.T) {
	f := newIfFixture(t)
	f.runs.list = append(f.runs.list, f.scan)
	f.appendOn(t, f.scan.ID, "pull_request_opened", map[string]any{"head_sha": ifHeadSHA})
	f.pass(t)
	if n := f.sender.count(); n != 0 {
		t.Fatalf("sends = %d, want 0: the scan run must never be targeted", n)
	}
	if p := f.summary(t); p.RunsListed != 0 || len(p.SkippedRuns) != 0 {
		t.Fatalf("summary = %+v, want the scan run silently excluded", p)
	}
}

func TestUpkeepInflight_SkipReasons(t *testing.T) {
	parent := uuid.New()
	for _, tc := range []struct {
		name   string
		reason string
		setup  func(t *testing.T, f *ifFixture)
	}{
		{"decomposition child (C8)", upkeepInflightSkipDecompositionChild, func(t *testing.T, f *ifFixture) {
			f.target(t, func(r *run.Run) { r.DecomposedFrom = &parent })
		}},
		{"foreign account (C6)", upkeepInflightSkipOwnershipRefused, func(t *testing.T, f *ifFixture) {
			f.scan.AccountID = uuid.NewString()
			f.target(t, func(r *run.Run) { r.AccountID = uuid.NewString() })
		}},
		{"foreign repo", upkeepInflightSkipOwnershipRefused, func(t *testing.T, f *ifFixture) {
			f.target(t, func(r *run.Run) { r.Repo = "someone/else" })
		}},
		{"head read failed", upkeepInflightSkipHeadReadFailed, func(t *testing.T, f *ifFixture) {
			f.target(t, nil)
			f.au.listErrCategories["pull_request_opened"] = errors.New("injected head read failure")
		}},
		{"no head", upkeepInflightSkipNoHead, func(t *testing.T, f *ifFixture) {
			rn := &run.Run{ID: uuid.New(), Repo: ifRepo, State: run.StateRunning}
			f.runs.seedRun(rn)
			f.runs.list = append(f.runs.list, rn)
		}},
		{"forge unavailable", upkeepInflightSkipForgeUnavailable, func(t *testing.T, f *ifFixture) {
			f.target(t, nil)
			f.forgeNote = "github client not wired"
		}},
		{"compare failed", upkeepInflightSkipCompareFailed, func(t *testing.T, f *ifFixture) {
			f.target(t, nil)
			f.forge.compareErr = errors.New("injected compare failure")
		}},
		{"patch unavailable", upkeepInflightSkipPatchUnavailable, func(t *testing.T, f *ifFixture) {
			f.target(t, nil)
			f.forge.patch = ""
		}},
		{"manifest fetch failed", upkeepInflightSkipFetchFailed, func(t *testing.T, f *ifFixture) {
			f.target(t, nil)
			f.forge.fetchErr = errors.New("injected fetch failure")
		}},
		{"manifest too large", upkeepInflightSkipTooLarge, func(t *testing.T, f *ifFixture) {
			f.target(t, nil)
			prev := upkeepInflightMaxManifestBytes
			upkeepInflightMaxManifestBytes = 16
			t.Cleanup(func() { upkeepInflightMaxManifestBytes = prev })
		}},
		{"manifest unparseable", upkeepInflightSkipUnparseable, func(t *testing.T, f *ifFixture) {
			f.target(t, nil)
			f.forge.content[ifGoModPath] = "module x\n"
		}},
		{"dedupe read failed", upkeepInflightSkipDedupeReadFailed, func(t *testing.T, f *ifFixture) {
			f.target(t, nil)
			f.au.listErrCategories[crewmessage.CategorySent] = errors.New("injected chain read failure")
		}},
		{"send failed", upkeepInflightSkipSendFailed, func(t *testing.T, f *ifFixture) {
			f.target(t, nil)
			f.sender.err = errors.New("injected send failure")
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newIfFixture(t)
			tc.setup(t, f)
			f.pass(t)
			requireIfSkip(t, f, tc.reason)
		})
	}
}

// TestUpkeepInflight_UncitedOrRemovedManifestIgnored: a manifest no approved
// advisory cites, and a removed one, are never fetched and never match.
func TestUpkeepInflight_UncitedOrRemovedManifestIgnored(t *testing.T) {
	for _, files := range [][]forge.ComparePatchFile{
		{{Path: "runner/go.mod", Status: "modified"}},
		{{Path: ifGoModPath, Status: "removed"}},
		{{Path: "backend/main.go", Status: "modified"}},
	} {
		f := newIfFixture(t)
		f.target(t, nil)
		f.forge.files = files
		f.pass(t)
		if n := f.sender.count(); n != 0 || len(f.forge.fetchRefs) != 0 {
			t.Fatalf("files %+v: sends = %d fetches = %v, want none", files, n, f.forge.fetchRefs)
		}
		if p := f.summary(t); p.RunsExamined != 1 || len(p.SkippedRuns) != 0 {
			t.Fatalf("files %+v: summary = %+v", files, p)
		}
	}
}

// TestUpkeepInflight_OnlyApprovedAdvisories: an advisory the captain did not
// approve (or rejected) starts NO pass and writes no row.
func TestUpkeepInflight_OnlyApprovedAdvisories(t *testing.T) {
	for _, consumed := range []map[string]upkeepConsumedDisposition{
		{},
		{ifFindingID: {Verdict: "rejected"}},
	} {
		f := newIfFixture(t)
		f.target(t, nil)
		f.consumed = consumed
		f.pass(t)
		if n := f.sender.count(); n != 0 || len(f.summaries(t)) != 0 {
			t.Fatalf("consumed %v: sends = %d rows = %d, want none", consumed, n, len(f.summaries(t)))
		}
	}
}

// TestUpkeepInflight_NoMailboxStartsNothing: no mailbox -> no goroutine, no row.
func TestUpkeepInflight_NoMailboxStartsNothing(t *testing.T) {
	f := newIfFixture(t)
	f.target(t, nil)
	upkeepInflightMailbox = func(*Server) upkeepInflightSender { return nil }
	f.pass(t)
	if len(f.summaries(t)) != 0 || f.forge.compares != 0 {
		t.Fatal("a pass ran without a mailbox")
	}
	// The production resolver maps an unwired mailbox to nil (never a
	// non-nil interface holding a nil *Mailbox).
	if got := upkeepInflightMailboxFromConfig(New(Config{Addr: "127.0.0.1:0"})); got != nil {
		t.Fatalf("production resolver returned %v for an unwired mailbox, want nil", got)
	}
	if got := upkeepInflightMailboxFromConfig(New(Config{Addr: "127.0.0.1:0", CrewMailbox: crewmessage.NewMailbox(nil, 0)})); got == nil {
		t.Fatal("production resolver returned nil for a wired mailbox")
	}
}

func TestUpkeepInflight_Degrades(t *testing.T) {
	for _, tc := range []struct {
		name, reason string
		setup        func(t *testing.T, f *ifFixture)
	}{
		{"report unreadable", upkeepInflightReportUnreadable, func(t *testing.T, f *ifFixture) {
			f.artID = uuid.NewString() // no recorded row names it
		}},
		{"scan run unreadable", upkeepInflightScanRunUnreadable, func(t *testing.T, f *ifFixture) {
			delete(f.runs.runs, f.scan.ID)
		}},
		{"base ref resolver unwired", upkeepInflightBaseRefUnavailable, func(t *testing.T, f *ifFixture) {
			f.s.cfg.DocumentBaseRef = nil
		}},
		{"base ref resolver errors", upkeepInflightBaseRefUnavailable, func(t *testing.T, f *ifFixture) {
			f.s.cfg.DocumentBaseRef = func(context.Context, forge.RepoRef) (string, error) { return "", errors.New("boom") }
		}},
		{"base ref empty", upkeepInflightBaseRefUnavailable, func(t *testing.T, f *ifFixture) {
			f.s.cfg.DocumentBaseRef = func(context.Context, forge.RepoRef) (string, error) { return "", nil }
		}},
		{"base ref unparseable repo", upkeepInflightBaseRefUnavailable, func(t *testing.T, f *ifFixture) {
			f.scan.Repo = "no-slash"
		}},
		{"run list failed", upkeepInflightRunListFailed, func(t *testing.T, f *ifFixture) {
			f.runs.listErr = errors.New("injected list failure")
		}},
		{"pass panic", upkeepInflightPassPanic, func(t *testing.T, f *ifFixture) {
			f.target(t, nil)
			f.forge.panicMsg = "injected forge panic"
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newIfFixture(t)
			tc.setup(t, f)
			f.pass(t)
			requireIfDegraded(t, f.summary(t), tc.reason)
			if n := f.sender.count(); n != 0 {
				t.Fatalf("sends = %d on a degraded pass", n)
			}
		})
	}
}

// TestUpkeepInflight_BudgetExceededRowSurvivesExpiredContext: the forge seam
// blocks past the budget; the summary row still lands because it is written
// on a FRESH context — the fake audit repo rejects an expired one, so a
// regression to the pass context loses the row and reddens this test.
func TestUpkeepInflight_BudgetExceededRowSurvivesExpiredContext(t *testing.T) {
	f := newIfFixture(t)
	f.target(t, nil)
	f.forge.block = make(chan struct{})
	defer close(f.forge.block)
	prev := upkeepInflightBudget
	upkeepInflightBudget = timescale.D(100 * time.Millisecond)
	t.Cleanup(func() { upkeepInflightBudget = prev })
	f.pass(t)
	p := f.summary(t)
	requireIfDegraded(t, p, upkeepInflightBudgetExceeded)
	if p.SkippedRuns[upkeepInflightSkipCompareFailed] != 1 {
		t.Fatalf("skipped_runs = %v, want the in-flight compare counted", p.SkippedRuns)
	}
}

// TestUpkeepInflight_QueuedPassDegradesBusyWithinItsBudget: a pass queued
// behind a slow pass gives up inside its OWN budget (clocked from the call
// site) with pass_busy, instead of blocking behind the slow one.
func TestUpkeepInflight_QueuedPassDegradesBusyWithinItsBudget(t *testing.T) {
	f := newIfFixture(t)
	f.target(t, nil)
	f.forge.block = make(chan struct{})
	f.forge.entered = make(chan struct{})

	f.s.startUpkeepInflightPass(context.Background(), f.scan.ID, f.stageID, f.artID, f.report, f.consumed)
	<-f.forge.entered // the slow pass holds the slot

	prev := upkeepInflightBudget
	upkeepInflightBudget = timescale.D(100 * time.Millisecond)
	t.Cleanup(func() { upkeepInflightBudget = prev })
	start := time.Now()
	f.s.startUpkeepInflightPass(context.Background(), f.scan.ID, f.stageID, f.artID, f.report, f.consumed)

	deadline := time.Now().Add(timescale.D(5 * time.Second))
	var busy *upkeepInflightPassPayload
	for busy == nil && time.Now().Before(deadline) {
		for _, p := range f.summaries(t) {
			if p.DegradeReason == upkeepInflightBusy {
				p := p
				busy = &p
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	elapsed := time.Since(start)
	close(f.forge.block)
	f.s.waitUpkeepInflight()
	if busy == nil {
		t.Fatalf("the queued pass recorded no %s row while the slow pass held the slot", upkeepInflightBusy)
	}
	if elapsed > timescale.D(3*time.Second) {
		t.Fatalf("queued pass took %v, want it to end inside its own budget", elapsed)
	}
	if n := f.sender.count(); n != 1 {
		t.Fatalf("sends = %d, want 1 (the slow pass only)", n)
	}
}

// TestUpkeepInflight_ConcurrentPassesSendOnce (C9): two passes started
// together send exactly once; the slot serializes them.
func TestUpkeepInflight_ConcurrentPassesSendOnce(t *testing.T) {
	f := newIfFixture(t)
	f.target(t, nil)
	f.sender.delay = 50 * time.Millisecond
	f.s.startUpkeepInflightPass(context.Background(), f.scan.ID, f.stageID, f.artID, f.report, f.consumed)
	f.s.startUpkeepInflightPass(context.Background(), f.scan.ID, f.stageID, f.artID, f.report, f.consumed)
	f.s.waitUpkeepInflight()
	if n := f.sender.count(); n != 1 {
		t.Fatalf("sends = %d, want exactly 1 across two concurrent passes", n)
	}
}

// TestUpkeepInflight_ProjectionErrorCountsAsSent: a committed-but-unprojected
// send is SENT, never retried.
func TestUpkeepInflight_ProjectionErrorCountsAsSent(t *testing.T) {
	f := newIfFixture(t)
	f.target(t, nil)
	f.sender.err = &crewmessage.ProjectionError{Sequence: 77, Err: errors.New("projection down")}
	f.pass(t)
	p := f.summary(t)
	if len(p.Sent) != 1 || p.Sent[0].SentSequence != 77 || len(p.SkippedRuns) != 0 {
		t.Fatalf("summary = %+v, want one sent at sequence 77", p)
	}
}

// TestUpkeepInflight_RunWindowTruncated: the examined-run cap is recorded.
func TestUpkeepInflight_RunWindowTruncated(t *testing.T) {
	f := newIfFixture(t)
	f.target(t, nil)
	f.target(t, nil)
	prev := upkeepInflightMaxRuns
	upkeepInflightMaxRuns = 1
	t.Cleanup(func() { upkeepInflightMaxRuns = prev })
	f.pass(t)
	if p := f.summary(t); !p.RunsWindowTruncated || p.RunsListed != 1 || len(p.Sent) != 1 {
		t.Fatalf("summary = %+v, want truncated after one run", p)
	}
}

// TestUpkeepInflight_C2_OtherVersionLineNotSent (approval condition 5): an
// advisory in use at v2.1.0 fixed at v2.3.0, a run change to v1.9.0 — below
// the fix but on another major line — is NOT sent.
func TestUpkeepInflight_C2_OtherVersionLineNotSent(t *testing.T) {
	f := newIfFixture(t)
	f.target(t, nil)
	f.report = ifAdvisoryReport("v2.1.0", "v2.3.0")
	f.forge.patch = ifGoModPatch(ifGoModPath, "v1.8.0", "v1.9.0")
	f.forge.content[ifGoModPath] = strings.Replace(ifGoModHead, "v0.22.0", "v1.9.0", 1)
	f.pass(t)
	if n := f.sender.count(); n != 0 {
		t.Fatalf("sends = %d, want 0 for a change on another version line", n)
	}
	if p := f.summary(t); p.RunsExamined != 1 {
		t.Fatalf("summary = %+v, want the run examined", p)
	}
}

// TestApplyApprovedUpkeep_StartsInflightPassForApprovedAdvisories (C12's unit
// twin): the trigger is the captain-approved apply, and a second apply of the
// same settled window sends nothing new (approval condition 1).
func TestApplyApprovedUpkeep_StartsInflightPassForApprovedAdvisories(t *testing.T) {
	af := newUkApplyFixture(t, ukApplyOpts{reportBody: ukAdvisoryBody(t, nil)})
	f := newIfFixture(t)
	// Point the inflight fakes at the apply fixture's server and stores.
	f.au, f.scan = af.au, af.run
	f.sender.au = af.au
	rr := &ifRunRepo{approvalRunRepo: af.runs.approvalRunRepo}
	af.s.cfg.RunRepo = rr
	f.runs = rr
	f.s = af.s
	af.s.cfg.DocumentBaseRef = func(context.Context, forge.RepoRef) (string, error) { return "main", nil }
	af.s.cfg.Logger = slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil))
	f.scan.Repo = ukApplyRepo
	tgt := f.target(t, func(r *run.Run) { r.Repo = ukApplyRepo })

	af.grant(t)
	af.dispose(t, ifFindingID, upkeepVerdictApproved, false, "")
	af.apply(t, approval.DecisionApprove)
	af.s.waitUpkeepInflight()
	if n := f.sender.count(); n != 1 {
		t.Fatalf("sends after the approved apply = %d, want 1", n)
	}
	if got := f.sender.messages(t)[0].Anchor.RunID; got != tgt.ID.String() {
		t.Fatalf("anchor = %s, want %s", got, tgt.ID)
	}

	af.apply(t, approval.DecisionApprove)
	af.s.waitUpkeepInflight()
	if n := f.sender.count(); n != 1 {
		t.Fatalf("sends after a second apply of the same window = %d, want still 1", n)
	}
	if s := f.summaries(t); len(s) != 2 || len(s[1].AlreadySent) != 1 {
		t.Fatalf("summaries = %+v, want the second pass recording already_sent", s)
	}
}

// TestApplyApprovedUpkeep_RejectStartsNoInflightPass: a rejected gate, and an
// approved gate whose advisory finding the captain rejected, start nothing.
func TestApplyApprovedUpkeep_RejectStartsNoInflightPass(t *testing.T) {
	for _, tc := range []struct {
		name     string
		decision approval.Decision
		verdict  string
	}{
		{"gate rejected", approval.DecisionReject, upkeepVerdictApproved},
		{"finding rejected", approval.DecisionApprove, "rejected"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			af := newUkApplyFixture(t, ukApplyOpts{reportBody: ukAdvisoryBody(t, nil)})
			f := newIfFixture(t)
			f.sender.au = af.au
			rr := &ifRunRepo{approvalRunRepo: af.runs.approvalRunRepo}
			af.s.cfg.RunRepo = rr
			af.s.cfg.DocumentBaseRef = func(context.Context, forge.RepoRef) (string, error) { return "main", nil }
			f.runs, f.au, f.s = rr, af.au, af.s
			f.target(t, func(r *run.Run) { r.Repo = ukApplyRepo })
			af.grant(t)
			af.dispose(t, ifFindingID, tc.verdict, false, "")
			af.apply(t, tc.decision)
			af.s.waitUpkeepInflight()
			if n := f.sender.count(); n != 0 || f.forge.compares != 0 {
				t.Fatalf("sends = %d compares = %d, want no pass", n, f.forge.compares)
			}
		})
	}
}

// TestUpkeepInflight_ListFailureAfterBudgetIsBudgetExceeded: a listing error
// once the budget has expired is the budget's degrade, not run_list_failed.
func TestUpkeepInflight_ListFailureAfterBudgetIsBudgetExceeded(t *testing.T) {
	f := newIfFixture(t)
	f.runs.listErr = errors.New("injected list failure")
	f.s.cfg.DocumentBaseRef = func(ctx context.Context, _ forge.RepoRef) (string, error) {
		<-ctx.Done()
		return "main", nil
	}
	prev := upkeepInflightBudget
	upkeepInflightBudget = timescale.D(50 * time.Millisecond)
	t.Cleanup(func() { upkeepInflightBudget = prev })
	f.pass(t)
	requireIfDegraded(t, f.summary(t), upkeepInflightBudgetExceeded)
}
