package precedent

import (
	"math/rand"
	"reflect"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/decisionindex"
)

// tuning_test.go pins the E75.5 / #3733 replay. Fire counts are asserted to
// MOVE in the expected direction across a candidate axis (not a single value,
// which a stub returning a constant would satisfy), and each replay control —
// strictly-earlier, same-repo, the allow-list, delegated exclusion, the
// candidate window — has a fixture on which it alone decides the count.

var tnBase = time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)

const (
	tnRepo     = "o/r"
	tnDoctrine = "sha-1"
)

var tnRun = uuid.MustParse("00000000-0000-0000-0000-0000000000aa")

// tnRow builds one implement-stage decision decided on day (1-based) of the
// synthetic history.
func tnRow(seq int64, day int, class decisionindex.DecisionClass, outcome string) decisionindex.Row {
	return decisionindex.Row{
		SourceSequence:  seq,
		SourceEntryHash: "h",
		RunID:           tnRun,
		Repo:            tnRepo,
		DoctrineVersion: tnDoctrine,
		DecisionClass:   class,
		StageKind:       "implement",
		Outcome:         outcome,
		ConcernCategory: "correctness",
		Severity:        "major",
		TouchedPaths:    []string{},
		EscalationKeys:  []string{},
		DecidedAt:       tnBase.Add(time.Duration(day) * 24 * time.Hour),
	}
}

// tnHistory lays out decisions one day apart: "W" a concern waive, "D" a
// concern defer, "A" a plan approve, "R" a plan reject.
func tnHistory(pattern string) []decisionindex.Row {
	out := make([]decisionindex.Row, 0, len(pattern))
	for i, ch := range pattern {
		var class decisionindex.DecisionClass
		var outcome string
		switch ch {
		case 'W':
			class, outcome = decisionindex.ClassConcernWaive, "waived"
		case 'D':
			class, outcome = decisionindex.ClassConcernDefer, "deferred"
		case 'A':
			class, outcome = decisionindex.ClassPlanApproval, "approve"
		case 'R':
			class, outcome = decisionindex.ClassPlanApproval, OutcomeReject
		default:
			panic("tnHistory: unknown decision " + string(ch))
		}
		out = append(out, tnRow(int64(i+1), i+1, class, outcome))
	}
	return out
}

func tnCfg(n int, x float64, window time.Duration) DivergenceConfig {
	return DivergenceConfig{MinDecisions: n, MinAgreement: x, Window: window}
}

const tnYear = 365 * 24 * time.Hour

func TestReplay_CountsAndBreakdown(t *testing.T) {
	// Six waives then one contrary defer: the first five waives have too few
	// priors, the sixth agrees, the defer diverges.
	got := Replay(tnHistory("WWWWWWD"), []DivergenceConfig{tnCfg(5, 0.8, tnYear)})
	if len(got) != 1 {
		t.Fatalf("results = %d, want 1", len(got))
	}
	r := got[0]
	want := TuningBreakdown{BelowMinDecisions: 5, AgreedWithPrecedent: 1}
	if r.DecisionsExamined != 7 || r.WouldHaveFired != 1 || r.NotFired != want {
		t.Fatalf("result = %+v, want examined 7, fired 1, not_fired %+v", r, want)
	}
	if r.FireRate != 1.0/7.0 {
		t.Fatalf("fire rate = %v, want 1/7", r.FireRate)
	}
	if !r.Config.Enabled {
		t.Fatal("a candidate must be evaluated as enabled")
	}
}

// TestReplay_FireCountFallsAsMinDecisionsRises: the three defers arrive after
// 3, 9 and 19 waives respectively, so raising N past each of those silences
// one more of them.
func TestReplay_FireCountFallsAsMinDecisionsRises(t *testing.T) {
	rows := tnHistory("WWWD" + "WWWWWWD" + "WWWWWWWWWWD")
	grid := []DivergenceConfig{
		tnCfg(3, 0.6, tnYear), tnCfg(8, 0.6, tnYear), tnCfg(15, 0.6, tnYear), tnCfg(30, 0.6, tnYear),
	}
	res := Replay(rows, grid)
	fires := make([]int, len(res))
	for i, r := range res {
		fires[i] = r.WouldHaveFired
	}
	if want := []int{3, 2, 1, 0}; !reflect.DeepEqual(fires, want) {
		t.Fatalf("fires by N = %v, want %v", fires, want)
	}
	for i := 1; i < len(res); i++ {
		if res[i].NotFired.BelowMinDecisions < res[i-1].NotFired.BelowMinDecisions {
			t.Fatalf("below_min_decisions fell as N rose: %+v", res)
		}
	}
}

// TestReplay_FireCountFallsAsWindowShrinks: the same history under shrinking
// windows. The first defer (day 11) follows ten daily waives; the second is
// pushed to day 40, three weeks after the last waive. A 7-day window keeps the
// first and loses the second; a 3-day window loses both and says why.
func TestReplay_FireCountFallsAsWindowShrinks(t *testing.T) {
	rows := tnHistory("WWWWWWWWWWD" + "WWWWWWD")
	day := 24 * time.Hour
	rows[len(rows)-1].DecidedAt = tnBase.Add(40 * day)
	grid := []DivergenceConfig{tnCfg(6, 0.6, tnYear), tnCfg(6, 0.6, 7*day), tnCfg(6, 0.6, 3*day)}
	res := Replay(rows, grid)
	fires := []int{res[0].WouldHaveFired, res[1].WouldHaveFired, res[2].WouldHaveFired}
	if want := []int{2, 1, 0}; !reflect.DeepEqual(fires, want) {
		t.Fatalf("fires by shrinking window = %v, want %v (results %+v)", fires, want, res)
	}
	if res[2].NotFired.OutsideWindow == 0 {
		t.Fatalf("narrowest window reports no outside_window reason: %+v", res[2])
	}
}

// TestReplay_PrecedentIsStrictlyEarlier: every decision shares ONE DecidedAt,
// so only the source-sequence tie-break orders them. The defer is sequence 6;
// exactly five waives precede it. A replay counting the decision itself, or a
// LATER row, would see a different set.
func TestReplay_PrecedentIsStrictlyEarlier(t *testing.T) {
	rows := tnHistory("WWWWWDWWWWWWW")
	for i := range rows {
		rows[i].DecidedAt = tnBase
	}
	res := Replay(rows, []DivergenceConfig{tnCfg(6, 0.8, tnYear)})[0]
	// N=6: the defer has five priors (below), so nothing fires even though
	// seven more waives follow it.
	if res.WouldHaveFired != 0 {
		t.Fatalf("fired %d times with only five earlier priors at N=6: %+v", res.WouldHaveFired, res)
	}
	res = Replay(rows, []DivergenceConfig{tnCfg(5, 0.8, tnYear)})[0]
	if res.WouldHaveFired != 1 {
		t.Fatalf("fired %d times, want 1 (five earlier priors at N=5): %+v", res.WouldHaveFired, res)
	}
}

// TestReplay_OtherRepositoryIsNotPrecedent: ten unanimous waives in ANOTHER
// repository precede the defer; they are not its precedent.
func TestReplay_OtherRepositoryIsNotPrecedent(t *testing.T) {
	rows := tnHistory("WWWWWWWWWWD")
	for i := 0; i < 10; i++ {
		rows[i].Repo = "other/repo"
	}
	res := Replay(rows, []DivergenceConfig{tnCfg(5, 0.8, tnYear)})[0]
	if res.WouldHaveFired != 0 {
		t.Fatalf("cross-repository rows counted as precedent: %+v", res)
	}
}

// TestReplay_DelegatedPriorsDoNotCount: the priors are delegated, so the
// defer has no human precedent at all.
func TestReplay_DelegatedPriorsDoNotCount(t *testing.T) {
	rows := tnHistory("WWWWWWWWD")
	for i := 0; i < 8; i++ {
		rows[i].Delegated = true
	}
	res := Replay(rows, []DivergenceConfig{tnCfg(5, 0.8, tnYear)})[0]
	if res.WouldHaveFired != 0 || res.NotFired.BelowMinDecisions != 9 {
		t.Fatalf("result = %+v, want 0 fired and all 9 below_min_decisions", res)
	}
}

// TestReplay_DoctrineChangeResetsPrecedent: the priors were decided under an
// older doctrine version than the defer.
func TestReplay_DoctrineChangeResetsPrecedent(t *testing.T) {
	rows := tnHistory("WWWWWWWWD")
	rows[8].DoctrineVersion = "sha-2"
	res := Replay(rows, []DivergenceConfig{tnCfg(5, 0.8, tnYear)})[0]
	if res.WouldHaveFired != 0 || res.NotFired.DoctrineVersionMismatch != 1 {
		t.Fatalf("result = %+v, want 0 fired and one doctrine_version_mismatch", res)
	}
}

// TestReplay_PlanApprovalExaminesRejectsOnly: approves are precedent for a
// reject but are never themselves examined.
func TestReplay_PlanApprovalExaminesRejectsOnly(t *testing.T) {
	res := Replay(tnHistory("AAAAAAR"), []DivergenceConfig{tnCfg(5, 0.8, tnYear)})[0]
	if res.DecisionsExamined != 1 || res.WouldHaveFired != 1 || res.FireRate != 1 {
		t.Fatalf("result = %+v, want exactly the reject examined and fired", res)
	}
}

// TestReplay_NarrowedClassesExamineOnlyThoseClasses: a candidate narrowed to
// plan_approval examines no concern decision.
func TestReplay_NarrowedClassesExamineOnlyThoseClasses(t *testing.T) {
	cfg := tnCfg(5, 0.8, tnYear)
	cfg.AllowedClasses = []string{string(decisionindex.ClassPlanApproval)}
	res := Replay(tnHistory("WWWWWWD"), []DivergenceConfig{cfg})[0]
	if res.DecisionsExamined != 0 || res.WouldHaveFired != 0 || res.FireRate != 0 {
		t.Fatalf("result = %+v, want nothing examined", res)
	}
}

// TestReplay_EachCandidateAppliesItsOwnNarrowing: a FULL candidate and one
// narrowed to plan_approval share a grid, so every concern decision is examined
// by the first; the narrowed one must still count none of them.
func TestReplay_EachCandidateAppliesItsOwnNarrowing(t *testing.T) {
	narrowed := tnCfg(5, 0.8, tnYear)
	narrowed.AllowedClasses = []string{string(decisionindex.ClassPlanApproval)}
	res := Replay(tnHistory("WWWWWWD"), []DivergenceConfig{tnCfg(5, 0.8, tnYear), narrowed})
	if res[0].DecisionsExamined != 7 || res[0].WouldHaveFired != 1 {
		t.Fatalf("full candidate = %+v, want 7 examined, 1 fired", res[0])
	}
	if res[1].DecisionsExamined != 0 || res[1].WouldHaveFired != 0 {
		t.Fatalf("narrowed candidate = %+v, want nothing examined", res[1])
	}
}

// TestReplay_OrdersByDecidedAtBeforeSequence: the defer carries the LOWEST
// source sequence but was decided last (sequence order is not decision order
// across a backfill). Ordered by DecidedAt it follows six waives and fires;
// ordered by sequence alone it would have no precedent.
func TestReplay_OrdersByDecidedAtBeforeSequence(t *testing.T) {
	rows := tnHistory("WWWWWWD")
	for i := range rows {
		rows[i].SourceSequence = int64(10 + i)
	}
	rows[6].SourceSequence = 1
	res := Replay(rows, []DivergenceConfig{tnCfg(5, 0.8, tnYear)})[0]
	if res.WouldHaveFired != 1 {
		t.Fatalf("fired %d, want 1 (precedent is ordered by decided_at first): %+v", res.WouldHaveFired, res)
	}
}

// TestReplay_CandidateWindowMirrorsLiveQuery: the 20 OLDEST waives match the
// defer's touched paths (so they outrank everything) but were decided under an
// old doctrine; the 500 newer ones match nothing but share the defer's
// doctrine. The live store reads only the newest TuningCandidateWindow rows,
// so the old ones are never scored and the defer fires; scoring the whole
// history would cite the old-doctrine rows and stay quiet.
func TestReplay_CandidateWindowMirrorsLiveQuery(t *testing.T) {
	var rows []decisionindex.Row
	seq := int64(0)
	for i := 0; i < 20; i++ {
		seq++
		r := tnRow(seq, 1, decisionindex.ClassConcernWaive, "waived")
		r.TouchedPaths = []string{"backend/internal/x.go"}
		r.DoctrineVersion = "sha-old"
		rows = append(rows, r)
	}
	for i := 0; i < TuningCandidateWindow; i++ {
		seq++
		rows = append(rows, tnRow(seq, 2, decisionindex.ClassConcernWaive, "waived"))
	}
	seq++
	d := tnRow(seq, 3, decisionindex.ClassConcernDefer, "deferred")
	d.TouchedPaths = []string{"backend/internal/x.go"}
	rows = append(rows, d)

	res := Replay(rows, []DivergenceConfig{tnCfg(5, 0.8, tnYear)})[0]
	if res.WouldHaveFired != 1 {
		t.Fatalf("fired %d, want 1 (only the newest %d candidates are scored): %+v",
			res.WouldHaveFired, TuningCandidateWindow, res)
	}
}

// TestReplay_CandidateWindowCountsOnlyAdmittedRows: 500 rows from ANOTHER
// repository are newer than the six same-repository waives. The store applies
// the hard filter before its LIMIT, so the window holds only the admitted six
// and the defer fires; capping before filtering would fill the window with
// foreign rows that Rank then drops, leaving no precedent at all.
func TestReplay_CandidateWindowCountsOnlyAdmittedRows(t *testing.T) {
	rows := tnHistory("WWWWWW")
	seq := int64(len(rows))
	for i := 0; i < TuningCandidateWindow; i++ {
		seq++
		r := tnRow(seq, 7, decisionindex.ClassConcernWaive, "waived")
		r.Repo = "other/repo"
		rows = append(rows, r)
	}
	seq++
	rows = append(rows, tnRow(seq, 8, decisionindex.ClassConcernDefer, "deferred"))
	res := Replay(rows, []DivergenceConfig{tnCfg(5, 0.8, tnYear)})[0]
	if res.WouldHaveFired != 1 {
		t.Fatalf("fired %d, want 1 (the window must count only same-repository rows): %+v", res.WouldHaveFired, res)
	}
}

func TestReplay_DeterministicUnderInputReordering(t *testing.T) {
	rows := tnHistory("WWWD" + "WWWWWWD" + "AAAAR" + "WWWWWWWWWWD")
	grid := []DivergenceConfig{tnCfg(3, 0.6, tnYear), tnCfg(8, 0.9, 10*24*time.Hour)}
	want := Replay(rows, grid)
	rng := rand.New(rand.NewSource(7))
	for i := 0; i < 5; i++ {
		shuffled := append([]decisionindex.Row(nil), rows...)
		rng.Shuffle(len(shuffled), func(a, b int) { shuffled[a], shuffled[b] = shuffled[b], shuffled[a] })
		if got := Replay(shuffled, grid); !reflect.DeepEqual(got, want) {
			t.Fatalf("replay depends on input order:\n got %+v\nwant %+v", got, want)
		}
	}
}

func TestReplay_EmptyInputs(t *testing.T) {
	if got := Replay(tnHistory("WWD"), nil); len(got) != 0 {
		t.Fatalf("empty grid produced %d results", len(got))
	}
	got := Replay(nil, []DivergenceConfig{tnCfg(5, 0.8, tnYear)})
	if len(got) != 1 || got[0].DecisionsExamined != 0 || got[0].FireRate != 0 {
		t.Fatalf("no history: %+v, want one zero result", got)
	}
}
