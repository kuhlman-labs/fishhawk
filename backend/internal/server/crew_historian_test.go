package server

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/crewmessage"
	"github.com/kuhlman-labs/fishhawk/backend/internal/decisionindex"
	"github.com/kuhlman-labs/fishhawk/backend/internal/precedent"
	"github.com/kuhlman-labs/fishhawk/backend/internal/prompt"
)

// The historian responder's hermetic suite (E77.8 / #3742). Every arm drives
// the REAL responder over a recording stub index, so the account scope, the
// hard filter, the closed projection and the byte bound are observed on the
// rendered answer rather than reasoned about.

// historianSentinel is planted in every field a reason excerpt could travel
// through. It must never reach a rendered answer.
const historianSentinel = "SENTINEL-HISTORIAN-REASON-DO-NOT-LEAK"

// historianCtxKey marks a context so the stub can prove propagation (C7).
type historianCtxKey struct{}

// stubIndex is a recording PrecedentIndex. Its bookkeeping is mutex-guarded:
// the responder is reachable from the detached consult goroutine, and a racy
// fake would make a -race RED attributable to the FAKE rather than to the
// control under test (#3226).
type stubIndex struct {
	mu sync.Mutex

	gate    decisionindex.GateContext
	gateErr error
	// rows is consulted by decision class; listErr fails every List.
	rows    map[decisionindex.DecisionClass][]decisionindex.Row
	listErr error

	gateRefs    []decisionindex.GateRef
	filters     []decisionindex.ListFilter
	gateCtxMark any
	listCtxMark []any
}

func (s *stubIndex) GateContext(ctx context.Context, ref decisionindex.GateRef) (decisionindex.GateContext, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.gateRefs = append(s.gateRefs, ref)
	s.gateCtxMark = ctx.Value(historianCtxKey{})
	if s.gateErr != nil {
		return decisionindex.GateContext{}, s.gateErr
	}
	return s.gate, nil
}

func (s *stubIndex) List(ctx context.Context, f decisionindex.ListFilter) ([]decisionindex.Row, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.filters = append(s.filters, f)
	s.listCtxMark = append(s.listCtxMark, ctx.Value(historianCtxKey{}))
	if s.listErr != nil {
		return nil, s.listErr
	}
	return s.rows[f.DecisionClass], nil
}

func (s *stubIndex) seenFilters() []decisionindex.ListFilter {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]decisionindex.ListFilter(nil), s.filters...)
}

func (s *stubIndex) seenGateRefs() []decisionindex.GateRef {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]decisionindex.GateRef(nil), s.gateRefs...)
}

// historianRow builds one index row on repo with the given class and paths,
// planting the sentinel in the reason KEY (the only prose-adjacent column the
// index carries — the prose itself lives on the chain, which the responder
// cannot reach).
func historianRow(seq int64, repo string, class decisionindex.DecisionClass, outcome string, paths []string) decisionindex.Row {
	return decisionindex.Row{
		SourceSequence:  seq,
		SourceEntryHash: fmt.Sprintf("%064x", seq),
		RunID:           uuid.New(),
		Repo:            repo,
		DecisionClass:   class,
		StageKind:       "plan",
		Outcome:         outcome,
		DoctrineVersion: "sha1234",
		ActorKind:       "user",
		TouchedPaths:    paths,
		EscalationKeys:  []string{},
		DecidedAt:       time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC),
		ReasonSequence:  seq - 1,
		ReasonKey:       historianSentinel,
	}
}

func newHistorian(t *testing.T, idx PrecedentIndex) CrewResponder {
	t.Helper()
	h, err := NewHistorianResponder(idx)
	if err != nil {
		t.Fatalf("NewHistorianResponder: %v", err)
	}
	return h
}

func historianReq() CrewConsultRequest {
	acct := uuid.MustParse("11111111-2222-3333-4444-555555555555")
	r := consultReq()
	r.AccountID = &acct
	return r
}

// rendered is what writeUntrustedCrewMessages would carry: the two payload
// fields crewMessageForPrompt flattens.
func rendered(a CrewConsultAnswer) string { return "summary: " + a.Summary + "\ndetail: " + a.Detail }

// --- C1: the nil-index refusal -------------------------------------------

func TestNewHistorianResponder_RefusesNilIndex(t *testing.T) {
	got, err := NewHistorianResponder(nil)
	if !errors.Is(err, ErrHistorianIndexRequired) {
		t.Fatalf("err = %v, want ErrHistorianIndexRequired", err)
	}
	if got != nil {
		t.Fatalf("responder = %#v, want nil alongside the refusal", got)
	}
}

// --- C2: the row read is account-scoped ----------------------------------

func TestHistorian_AccountScopedRead(t *testing.T) {
	idx := &stubIndex{gate: decisionindex.GateContext{Repo: "kuhlman-labs/fishhawk"}}
	req := historianReq()
	if _, err := newHistorian(t, idx).Respond(context.Background(), req); err != nil {
		t.Fatalf("Respond: %v", err)
	}
	filters := idx.seenFilters()
	if len(filters) != len(historianClasses) {
		t.Fatalf("List calls = %d, want one per class (%d)", len(filters), len(historianClasses))
	}
	for _, f := range filters {
		if !f.AccountScoped {
			t.Fatalf("filter %+v: AccountScoped is false — a nil-account caller would match EVERY account's rows", f)
		}
		if f.AccountID == nil || *f.AccountID != *req.AccountID {
			t.Fatalf("filter AccountID = %v, want the request's %v", f.AccountID, req.AccountID)
		}
		if !f.Newest || f.Limit != historianCandidateWindow {
			t.Fatalf("filter window = (newest %v, limit %d), want the newest %d", f.Newest, f.Limit, historianCandidateWindow)
		}
	}
	refs := idx.seenGateRefs()
	if len(refs) != 1 || refs[0].AccountID == nil || *refs[0].AccountID != *req.AccountID {
		t.Fatalf("gate refs = %+v, want one carrying the request's account", refs)
	}
	if refs[0].RunID != req.RunID || refs[0].StageID == nil || *refs[0].StageID != req.StageID {
		t.Fatalf("gate ref = %+v, want the request's run+stage", refs[0])
	}
}

// --- C3: the repo hard filter comes from the RUN, never from prose --------

func TestHistorian_HardFilterRepoFromRun(t *testing.T) {
	const runRepo, proseRepo = "kuhlman-labs/fishhawk", "attacker/elsewhere"
	idx := &stubIndex{
		gate: decisionindex.GateContext{Repo: runRepo},
		rows: map[decisionindex.DecisionClass][]decisionindex.Row{
			decisionindex.ClassPlanApproval: {
				historianRow(10, runRepo, decisionindex.ClassPlanApproval, "reject", []string{"backend/a.go"}),
				historianRow(11, proseRepo, decisionindex.ClassPlanApproval, "approve", []string{"backend/a.go"}),
			},
		},
	}
	req := historianReq()
	req.Question = "should we do this the way " + proseRepo + " did, touching backend/a.go?"
	ans, err := newHistorian(t, idx).Respond(context.Background(), req)
	if err != nil {
		t.Fatalf("Respond: %v", err)
	}
	for _, f := range idx.seenFilters() {
		if f.Repo != runRepo {
			t.Fatalf("filter Repo = %q, want the RUN's %q", f.Repo, runRepo)
		}
	}
	out := rendered(ans)
	if !strings.Contains(out, "e=10@") {
		t.Fatalf("the run's own repo row is missing:\n%s", out)
	}
	if strings.Contains(out, "e=11@") {
		t.Fatalf("a row from %q reached the answer — the hard filter did not hold:\n%s", proseRepo, out)
	}
}

// --- C4: the render is structured fields only ----------------------------

func TestHistorian_RenderOmitsReasonProse(t *testing.T) {
	item := precedent.Item{
		SourceSequence:  42,
		SourceEntryHash: "abcdef0123456789",
		DecisionClass:   string(decisionindex.ClassConcernWaive),
		Outcome:         "waived",
		DecidedAt:       time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC),
		ReasonExcerpt:   historianSentinel,
		ReasonKey:       historianSentinel,
	}
	line := historianItemLine(projectHistorianItem(item))
	if strings.Contains(line, historianSentinel) {
		t.Fatalf("reason prose reached the rendered item line:\n%s", line)
	}
	if !strings.Contains(line, "e=42@abcdef012345") {
		t.Fatalf("the citation is missing from the item line:\n%s", line)
	}

	// And end to end: a full answer over sentinel-bearing rows.
	idx := &stubIndex{
		gate: decisionindex.GateContext{Repo: "r/p"},
		rows: map[decisionindex.DecisionClass][]decisionindex.Row{
			decisionindex.ClassConcernWaive: {historianRow(9, "r/p", decisionindex.ClassConcernWaive, "waived", []string{"a/b.go"})},
		},
	}
	ans, err := newHistorian(t, idx).Respond(context.Background(), historianReq())
	if err != nil {
		t.Fatalf("Respond: %v", err)
	}
	if strings.Contains(rendered(ans), historianSentinel) {
		t.Fatalf("the sentinel reached the answer:\n%s", rendered(ans))
	}
}

// TestProjectHistorianItem_FieldSetIsClosed enumerates the EXACT projected
// field set. A new precedent.Item field (including a prose one) cannot be
// silently exposed: adding it to historianItem fails this list.
func TestProjectHistorianItem_FieldSetIsClosed(t *testing.T) {
	want := []string{
		"DecisionClass", "Outcome", "RejectClass", "DecidedDate", "Delegated",
		"ActorKind", "DoctrineVersion", "ConcernCategory", "Severity",
		"MatchedPaths", "MatchedKeys", "ScoreTotal", "SourceSequence", "SourceEntryHash",
	}
	typ := reflect.TypeOf(historianItem{})
	got := make([]string, 0, typ.NumField())
	for i := 0; i < typ.NumField(); i++ {
		got = append(got, typ.Field(i).Name)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("historianItem fields = %v, want exactly %v\n"+
			"A field added here crosses the ADR-082 decision (e) boundary into a planner's prompt. "+
			"If the addition is deliberate, state why it is not free-text prose from another run.", got, want)
	}
	// No field may be typed to carry an excerpt by another name.
	for _, banned := range []string{"Reason", "Excerpt", "Note", "Prose", "Detail"} {
		for _, f := range got {
			if strings.Contains(f, banned) {
				t.Fatalf("historianItem.%s looks like prose (%q)", f, banned)
			}
		}
	}
}

// --- C5: the answer fits the crew-message cap BY CONSTRUCTION -------------

// historianPathologicalIndex is the worst case the plan names: 500 candidate
// rows per class, each carrying 200 long touched paths, all of which MATCH the
// ranking context, so every item's matched-key list is maximal.
func historianPathologicalIndex() *stubIndex {
	paths := make([]string, 200)
	for i := range paths {
		paths[i] = fmt.Sprintf("backend/internal/averylongpackagenamehere%03d/deeply/nested/file%03d.go", i, i)
	}
	rows := make([]decisionindex.Row, 0, historianCandidateWindow)
	for i := 0; i < historianCandidateWindow; i++ {
		rows = append(rows, historianRow(int64(1000+i), "kuhlman-labs/fishhawk",
			decisionindex.ClassPlanApproval, "reject", paths))
	}
	byClass := map[decisionindex.DecisionClass][]decisionindex.Row{}
	for _, c := range historianClasses {
		cls := make([]decisionindex.Row, len(rows))
		for i, r := range rows {
			r.DecisionClass = c
			cls[i] = r
		}
		byClass[c] = cls
	}
	return &stubIndex{
		gate: decisionindex.GateContext{Repo: "kuhlman-labs/fishhawk", TouchedPaths: paths},
		rows: byClass,
	}
}

func TestHistorian_AnswerUnderCrewMessageCap(t *testing.T) {
	idx := historianPathologicalIndex()
	ans, err := newHistorian(t, idx).Respond(context.Background(), historianReq())
	if err != nil {
		t.Fatalf("Respond: %v", err)
	}
	out := rendered(ans)
	if n := len(out); n > prompt.MaxCrewMessageBytes {
		t.Fatalf("rendered answer = %d bytes, over prompt.MaxCrewMessageBytes (%d):\n%s",
			n, prompt.MaxCrewMessageBytes, out)
	}
	// Binding approval condition 1: the answer fits BY CONSTRUCTION, not by
	// clamping. The belt-and-braces marker must be ABSENT for this worst case
	// — deleting the per-class item cap or the per-item matched-key cap makes
	// it appear (or blows the byte assertion above).
	if strings.Contains(out, historianTruncatedMarker) {
		t.Fatalf("the belt-and-braces truncation marker fired: the per-class/per-item caps no longer "+
			"make the worst case fit by construction:\n%s", out)
	}
	if len(ans.Evidence) > prompt.MaxCrewMessageEvidenceRefs {
		t.Fatalf("evidence refs = %d, over prompt.MaxCrewMessageEvidenceRefs (%d)",
			len(ans.Evidence), prompt.MaxCrewMessageEvidenceRefs)
	}
	for _, res := range historianClasses {
		if !strings.Contains(out, "~ "+string(res)+":") {
			t.Fatalf("class %s has no agreement line:\n%s", res, out)
		}
	}
	if !strings.Contains(out, "window_truncated=") {
		t.Fatalf("a FULL candidate window was not reported:\n%s", out)
	}
}

// The final truncation is KEPT and pinned separately (binding approval
// condition 1, second half): force it and assert the marker is PRESENT.
func TestHistorian_FinalTruncationMarks(t *testing.T) {
	long := strings.Repeat("x", prompt.MaxCrewMessageBytes*2)
	got := boundHistorianDetail("s", long)
	if !strings.HasSuffix(got, historianTruncatedMarker) {
		t.Fatalf("over-budget detail was not marked; got %d bytes ending %q", len(got), historianTail(got))
	}
	if n := len("summary: s\ndetail: ") + len(got); n > prompt.MaxCrewMessageBytes {
		t.Fatalf("truncated render = %d bytes, still over the cap", n)
	}
	// At or under budget it is returned VERBATIM and unmarked.
	if got := boundHistorianDetail("s", "short detail"); got != "short detail" {
		t.Fatalf("under-budget detail = %q, want it verbatim", got)
	}
}

func historianTail(s string) string {
	if len(s) <= 48 {
		return s
	}
	return s[len(s)-48:]
}

// --- C6: expire rather than fabricate ------------------------------------

func TestHistorian_RunMissingReturnsError(t *testing.T) {
	for name, injected := range map[string]error{
		"run_missing": fmt.Errorf("%w: run x", decisionindex.ErrRunMissing),
		"other":       errors.New("connection refused"),
	} {
		t.Run(name, func(t *testing.T) {
			idx := &stubIndex{gateErr: injected}
			ans, err := newHistorian(t, idx).Respond(context.Background(), historianReq())
			if err == nil {
				t.Fatalf("Respond returned no error; answer = %+v — a fabricated answer against an "+
					"unresolvable run is exactly what the expire-rather-than-fabricate posture forbids", ans)
			}
			if ans.Summary != "" {
				t.Fatalf("Summary = %q alongside the error, want blank", ans.Summary)
			}
			if len(idx.seenFilters()) != 0 {
				t.Fatal("rows were read after the run failed to resolve")
			}
		})
	}
}

// A row-read failure is the same posture: error, never a partial answer.
func TestHistorian_ListErrorReturnsError(t *testing.T) {
	idx := &stubIndex{gate: decisionindex.GateContext{Repo: "r/p"}, listErr: errors.New("index read failed")}
	if _, err := newHistorian(t, idx).Respond(context.Background(), historianReq()); err == nil {
		t.Fatal("Respond returned no error on a failed row read")
	}
}

// --- C7: the responder honours the passed context ------------------------

func TestHistorian_PropagatesContext(t *testing.T) {
	const marker = "ctx-marker-3742"
	idx := &stubIndex{gate: decisionindex.GateContext{Repo: "r/p"}}
	ctx := context.WithValue(context.Background(), historianCtxKey{}, marker)
	if _, err := newHistorian(t, idx).Respond(ctx, historianReq()); err != nil {
		t.Fatalf("Respond: %v", err)
	}
	idx.mu.Lock()
	gateMark, listMarks := idx.gateCtxMark, append([]any(nil), idx.listCtxMark...)
	idx.mu.Unlock()
	if gateMark != marker {
		t.Fatalf("GateContext saw ctx marker %v, want %q — the responder substituted its own context", gateMark, marker)
	}
	if len(listMarks) != len(historianClasses) {
		t.Fatalf("List saw %d contexts, want %d", len(listMarks), len(historianClasses))
	}
	for i, m := range listMarks {
		if m != marker {
			t.Fatalf("List call %d saw ctx marker %v, want %q", i, m, marker)
		}
	}
	// A CANCELLED context must reach the index, not be swallowed.
	cctx, cancel := context.WithCancel(context.Background())
	cancel()
	cancelIdx := &stubIndex{gateErr: cctx.Err()}
	if _, err := newHistorian(t, cancelIdx).Respond(cctx, historianReq()); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want the cancellation to surface", err)
	}
}

// --- C8: an empty result still ANSWERS -----------------------------------

func TestHistorian_NoPrecedentStillAnswers(t *testing.T) {
	idx := &stubIndex{gate: decisionindex.GateContext{Repo: "kuhlman-labs/fishhawk"}}
	ans, err := newHistorian(t, idx).Respond(context.Background(), historianReq())
	if err != nil {
		t.Fatalf("Respond: %v", err)
	}
	if strings.TrimSpace(ans.Summary) == "" {
		t.Fatal("blank Summary on an empty result — runCrewConsult would expire every consult on a fresh repository")
	}
	if !strings.Contains(ans.Summary, "No prior") || !strings.Contains(ans.Summary, "kuhlman-labs/fishhawk") {
		t.Fatalf("Summary = %q, want an honest no-precedent statement naming the repository", ans.Summary)
	}
	if len(ans.Evidence) != 0 {
		t.Fatalf("evidence = %v, want none cited", ans.Evidence)
	}
}

// --- C11 + the ranking-signal-only assertion ------------------------------

func TestHistorian_ProsePathsBounded(t *testing.T) {
	var prose strings.Builder
	for i := 0; i < 200; i++ {
		fmt.Fprintf(&prose, "backend/internal/pkg%03d/file%03d.go ", i, i)
	}
	// A token longer than the per-token cap, and a URL, are both dropped.
	prose.WriteString(strings.Repeat("a/", historianMaxProsePathBytes) + " https://example.test/a/b ")
	got := historianProsePaths(prose.String())
	if len(got) > historianMaxProsePaths {
		t.Fatalf("extracted %d prose paths, over historianMaxProsePaths (%d)", len(got), historianMaxProsePaths)
	}
	for _, p := range got {
		if len(p) > historianMaxProsePathBytes {
			t.Fatalf("token %q is %d bytes, over the cap", p, len(p))
		}
		if strings.Contains(p, "://") {
			t.Fatalf("a URL survived extraction: %q", p)
		}
	}
	// Deterministic and sorted, so two identical consults rank identically.
	again := historianProsePaths(prose.String())
	if !reflect.DeepEqual(got, again) {
		t.Fatalf("extraction is not deterministic: %v vs %v", got, again)
	}
	for i := 1; i < len(got); i++ {
		if got[i-1] >= got[i] {
			t.Fatalf("extraction is not sorted/de-duplicated at %d: %v", i, got)
		}
	}

	// And the whole answer stays inside the cap under that prose.
	idx := historianPathologicalIndex()
	req := historianReq()
	req.Context = prose.String()
	ans, err := newHistorian(t, idx).Respond(context.Background(), req)
	if err != nil {
		t.Fatalf("Respond: %v", err)
	}
	if n := len(rendered(ans)); n > prompt.MaxCrewMessageBytes {
		t.Fatalf("rendered answer = %d bytes, over the cap", n)
	}
}

func TestHistorian_ProsePathsExtraction(t *testing.T) {
	for name, tc := range map[string]struct {
		in   string
		want []string
	}{
		"backticked":   {"touching `backend/internal/server/crew_consult.go` today", []string{"backend/internal/server/crew_consult.go"}},
		"parenthesed":  {"(cli/internal/spec/spec.go)", []string{"cli/internal/spec/spec.go"}},
		"comma_listed": {"a/b.go,c/d.go", []string{"a/b.go", "c/d.go"}},
		"trailing_dot": {"see backend/x.go.", []string{"backend/x.go"}},
		"no_slash":     {"crew_consult.go", nil},
		"url_dropped":  {"https://example.test/a/b", nil},
	} {
		t.Run(name, func(t *testing.T) {
			got := historianProsePaths(tc.in)
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("historianProsePaths(%q) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}

// TestHistorian_ProsePathsRankOnly asserts the extracted tokens reach ONLY the
// ranking context and the echoed matched keys. CrewConsultAnswer has no scope,
// constraint or authority field for them to reach — pinned structurally — and
// the answer says so.
func TestHistorian_ProsePathsRankOnly(t *testing.T) {
	const prosePath = "backend/internal/server/crew_historian.go"
	idx := &stubIndex{
		gate: decisionindex.GateContext{Repo: "r/p"},
		rows: map[decisionindex.DecisionClass][]decisionindex.Row{
			decisionindex.ClassPlanApproval: {
				historianRow(5, "r/p", decisionindex.ClassPlanApproval, "reject", []string{prosePath}),
			},
		},
	}
	req := historianReq()
	req.Context = "paths: " + prosePath
	ans, err := newHistorian(t, idx).Respond(context.Background(), req)
	if err != nil {
		t.Fatalf("Respond: %v", err)
	}
	// It RANKED: the row matched on the prose-supplied path.
	if !strings.Contains(ans.Detail, "p=") {
		t.Fatalf("the prose path produced no matched-key echo:\n%s", ans.Detail)
	}
	if !strings.Contains(ans.Detail, "ranked_paths=1") {
		t.Fatalf("the resolved-context echo does not report the ranked path:\n%s", ans.Detail)
	}
	if !strings.Contains(ans.Detail, "ADVICE, not an order") {
		t.Fatalf("the answer does not state it is advice (ADR-081 rule 2):\n%s", ans.Detail)
	}
	// STRUCTURALLY: the answer type carries no field that could express scope.
	typ := reflect.TypeOf(CrewConsultAnswer{})
	for i := 0; i < typ.NumField(); i++ {
		switch typ.Field(i).Name {
		case "Summary", "Detail", "Evidence":
		default:
			t.Fatalf("CrewConsultAnswer gained field %q — a responder must have no channel that can express "+
				"scope, constraint or authority (ADR-081 rule 2)", typ.Field(i).Name)
		}
	}
}

// --- the closed-vocabulary category resolve, one case per branch ----------

func TestHistorian_ConcernCategoryResolve(t *testing.T) {
	for name, tc := range map[string]struct {
		prose string
		want  string
	}{
		"exactly_one":       {"is this a testing concern?", "testing"},
		"exactly_one_case":  {"a PERFORMANCE question", "performance"},
		"zero":              {"has this been decided before?", ""},
		"two_is_ambiguous":  {"a testing and performance question", ""},
		"substring_is_not":  {"scopecreep and documentation", "documentation"},
		"word_boundary_off": {"scopecreep only", ""},
	} {
		t.Run(name, func(t *testing.T) {
			if got := historianConcernCategory(tc.prose); got != tc.want {
				t.Fatalf("historianConcernCategory(%q) = %q, want %q", tc.prose, got, tc.want)
			}
		})
	}
	// Only the CLOSED canonical vocabulary is reachable — never a free-text
	// value the agent invented.
	canonical := map[string]bool{}
	for _, c := range decisionindex.CanonicalConcernCategories() {
		canonical[c] = true
	}
	if got := historianConcernCategory("this is a flakiness concern"); got != "" && !canonical[got] {
		t.Fatalf("resolved %q, which is not in the canonical vocabulary", got)
	}
}

// --- determinism / no model call (issue AC 3) -----------------------------

func TestHistorian_AnswerByteIdenticalAcrossCalls(t *testing.T) {
	build := func() CrewConsultAnswer {
		idx := &stubIndex{
			gate: decisionindex.GateContext{Repo: "r/p", TouchedPaths: []string{"backend/a.go"}},
			rows: map[decisionindex.DecisionClass][]decisionindex.Row{
				decisionindex.ClassPlanApproval: {
					historianRow(3, "r/p", decisionindex.ClassPlanApproval, "reject", []string{"backend/a.go"}),
					historianRow(4, "r/p", decisionindex.ClassPlanApproval, "approve", []string{"backend/b.go"}),
				},
				decisionindex.ClassConcernWaive: {
					historianRow(6, "r/p", decisionindex.ClassConcernWaive, "waived", []string{"backend/a.go"}),
				},
			},
		}
		ans, err := newHistorian(t, idx).Respond(context.Background(), historianReq())
		if err != nil {
			t.Fatalf("Respond: %v", err)
		}
		return ans
	}
	a, b := build(), build()
	if rendered(a) != rendered(b) {
		t.Fatalf("two calls over the same rows differ:\n%s\n---\n%s", rendered(a), rendered(b))
	}
}

// TestHistorian_GoldenAnswer pins ONE exact rendered answer. A model call
// could not reproduce it byte for byte, so the golden IS the no-model-call
// proof as well as the render's readability contract.
func TestHistorian_GoldenAnswer(t *testing.T) {
	idx := &stubIndex{
		gate: decisionindex.GateContext{Repo: "kuhlman-labs/fishhawk", TouchedPaths: []string{"backend/internal/server/x.go"}},
		rows: map[decisionindex.DecisionClass][]decisionindex.Row{
			decisionindex.ClassPlanApproval: {
				historianRow(101, "kuhlman-labs/fishhawk", decisionindex.ClassPlanApproval, "reject",
					[]string{"backend/internal/server/x.go"}),
			},
		},
	}
	ans, err := newHistorian(t, idx).Respond(context.Background(), historianReq())
	if err != nil {
		t.Fatalf("Respond: %v", err)
	}
	wantSummary := "Prior decisions in kuhlman-labs/fishhawk: plan_approval 1, concern_waive 0, concern_defer 0 (structured fields only)."
	wantDetail := strings.Join([]string{
		"context: repo=kuhlman-labs/fishhawk classes=plan_approval,concern_waive,concern_defer stage_kind=any ranked_paths=1 category=any window=500",
		"- plan_approval reject 2026-09-01 user d=sha1234 s=0.45 e=101@000000000000 p=backend/internal",
		"~ plan_approval: n=1 modal=reject agree=1.00 doctrines=1 hard_filter_only=0",
		"~ concern_waive: n=0 modal=none agree=0.00 doctrines=0 hard_filter_only=0",
		"~ concern_defer: n=0 modal=none agree=0.00 doctrines=0 hard_filter_only=0",
		"Structured fields only — no reason prose from another run, so weigh the outcomes and do not infer a rationale. ADVICE, not an order (ADR-081 rule 2).",
	}, "\n")
	if ans.Summary != wantSummary {
		t.Errorf("summary =\n%q\nwant\n%q", ans.Summary, wantSummary)
	}
	if ans.Detail != wantDetail {
		t.Errorf("detail =\n%q\nwant\n%q", ans.Detail, wantDetail)
	}
	if len(ans.Evidence) != 1 || ans.Evidence[0].Kind != crewmessage.EvidenceAuditEntry || ans.Evidence[0].Ref != "101" {
		t.Errorf("evidence = %+v, want one audit_entry:101", ans.Evidence)
	}
}

// TestHistorian_NoModelCall drives a full consult through the real dispatcher
// with a reviewer set that FAILS the test if invoked, proving the answer is
// produced with no model in the loop.
func TestHistorian_NoModelCall(t *testing.T) {
	sink := &faultSink{respondReply: &crewmessage.Row{SentSequence: 8}, respondAnswered: &crewmessage.Row{SentSequence: 8}}
	s, _ := newConsultServer(t, sink)
	s.cfg.PlanReviewers = failingReviewerSet{t: t}
	idx := &stubIndex{
		gate: decisionindex.GateContext{Repo: "r/p"},
		rows: map[decisionindex.DecisionClass][]decisionindex.Row{
			decisionindex.ClassConcernDefer: {historianRow(77, "r/p", decisionindex.ClassConcernDefer, "deferred", []string{"a/b.go"})},
		},
	}
	s.runCrewConsult(context.Background(), newHistorian(t, idx), historianReq())
	respond, dispose := sink.counts()
	if respond != 1 || dispose != 0 {
		t.Fatalf("sink calls = (respond %d, dispose %d), want the answer recorded and no expiry", respond, dispose)
	}
}

// failingReviewerSet fails the test if any review adapter is resolved: the
// historian must reach no model.
type failingReviewerSet struct{ t *testing.T }

func (f failingReviewerSet) Default() PlanReviewer {
	f.t.Error("the historian resolved a review adapter — it must make NO model call")
	return nil
}

func (f failingReviewerSet) For(provider, model string, _ ...string) (PlanReviewer, error) {
	f.t.Errorf("the historian resolved a review adapter (%s/%s) — it must make NO model call", provider, model)
	return nil, errors.New("no reviewer")
}
