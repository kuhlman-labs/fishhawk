package agenteval

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// ---------------------------------------------------------------------------
// The arm selector (#5566): FISHHAWK_AGENTEVAL_PLANREVIEW_ARMS re-records part
// of the arm table and carries every other arm forward verbatim. Every
// recorder test drives the real RecordCatchRateEvidence and decides with the
// real CheckCatchRateEvidence.
// ---------------------------------------------------------------------------

// armSender is the all-caught scripted sender whose architect answers follow
// the rendered prompts of the checkout in.RepoRoot.
func armSender(t *testing.T, in CatchRateInputs) *scriptedCatchSender {
	t.Helper()
	all := committedCatchTrials(t)
	s := newScriptedCatchSender(t, all, all)
	s.persona = personaScriptFor(t, in.RepoRoot)
	return s
}

func recordIn(t *testing.T, sender MessageSender, in CatchRateInputs, path string, now time.Time, opts RecordCatchRateOptions) (string, error) {
	t.Helper()
	return RecordCatchRateEvidence(context.Background(), sender, testCatchModel, in, path, now, opts)
}

// armsPinnedPair records a full, all-caught pin at catchT0 over a copied
// checkout and returns its inputs and evidence path.
func armsPinnedPair(t *testing.T) (CatchRateInputs, string) {
	t.Helper()
	in := copyPersonaRepo(t)
	path := filepath.Join(t.TempDir(), "evidence.json")
	if _, err := recordIn(t, armSender(t, in), in, path, catchT0, pin("initial baseline")); err != nil {
		t.Fatalf("record the pinned pair: %v", err)
	}
	return in, path
}

// staleArchitect flips one byte of the copied architect remit: treatment, not
// instrument, so only the architect arm's fingerprint moves.
func staleArchitect(t *testing.T, in CatchRateInputs) {
	t.Helper()
	remit := filepath.Join(in.RepoRoot, filepath.FromSlash(personaArchitectRemitPath))
	b := mustRead(t, remit)
	b[len(b)-2] ^= 0x01
	if err := os.WriteFile(remit, b, 0o644); err != nil {
		t.Fatal(err)
	}
}

// rawObject decodes the JSON object at the key path of raw into raw members.
func rawObject(t *testing.T, raw []byte, keys ...string) map[string]json.RawMessage {
	t.Helper()
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	for _, k := range keys {
		inner, ok := m[k]
		if !ok {
			t.Fatalf("no %q member", k)
		}
		m = nil
		if err := json.Unmarshal(inner, &m); err != nil {
			t.Fatal(err)
		}
	}
	return m
}

// compactJSON is raw without insignificant whitespace: a record arm and a
// baseline arm sit at different depths, so their raw bytes differ in
// indentation alone.
func compactJSON(t *testing.T, raw json.RawMessage) []byte {
	t.Helper()
	var b bytes.Buffer
	if err := json.Compact(&b, raw); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

// assertArmRefusal is every per-control refusal's assertion set: the
// sentinel, the named arm(s), ZERO model calls and a byte-identical pair.
func assertArmRefusal(t *testing.T, err error, sender *scriptedCatchSender, path string, before, beforeTrials []byte, names ...string) {
	t.Helper()
	if err == nil || !errors.Is(err, ErrCatchRateArmSelection) {
		t.Fatalf("err = %v, want an ErrCatchRateArmSelection refusal", err)
	}
	for _, n := range names {
		if !strings.Contains(err.Error(), n) {
			t.Errorf("the refusal does not name %q: %v", n, err)
		}
	}
	if sender.total() != 0 {
		t.Errorf("%d model calls were made before the refusal (the preflight runs before any model call)", sender.total())
	}
	assertPairBytes(t, path, before, beforeTrials)
}

// assertPairBytes reads the pair back AFTER the call: nil before means the
// file must not exist.
func assertPairBytes(t *testing.T, path string, before, beforeTrials []byte) {
	t.Helper()
	for _, f := range []struct {
		path string
		want []byte
	}{{path, before}, {CatchRateTrialsPath(path), beforeTrials}} {
		got, err := os.ReadFile(f.path)
		if f.want == nil {
			if !errors.Is(err, os.ErrNotExist) {
				t.Errorf("%s was written (err %v)", f.path, err)
			}
			continue
		}
		if err != nil || !bytes.Equal(got, f.want) {
			t.Errorf("%s changed (err %v)", f.path, err)
		}
	}
}

func TestParseCatchRateArms(t *testing.T) {
	env := func(v string) func(string) string {
		return func(k string) string {
			if k == CatchRateArmsEnv {
				return v
			}
			return ""
		}
	}
	if got, err := ParseCatchRateArms(env("")); err != nil || got != nil {
		t.Fatalf("unset = %v, %v; want nil, nil", got, err)
	}
	got, err := ParseCatchRateArms(env(" architect , architect,security"))
	if err != nil || !reflect.DeepEqual(got, []string{ArmArchitect, ArmSecurity}) {
		t.Fatalf("got %v, %v; want [architect security]", got, err)
	}
	if _, err := ParseCatchRateArms(env(",, ")); err == nil || !strings.Contains(err.Error(), CatchRateArmsEnv) || !errors.Is(err, ErrCatchRateArmSelection) {
		t.Fatalf("a blank list: err = %v, want a refusal naming %s", err, CatchRateArmsEnv)
	}
}

func TestCatchRateArmSelector(t *testing.T) {
	for _, tc := range []struct {
		arms []string
		want string
	}{
		{[]string{ArmArchitect}, "FISHHAWK_AGENTEVAL_PLANREVIEW_ARMS=architect"},
		{[]string{ArmWithConventions}, "FISHHAWK_AGENTEVAL_PLANREVIEW_ARMS=with_conventions,without_conventions"},
		{[]string{ArmSecurity, ArmWithoutConventions, ArmArchitect}, "FISHHAWK_AGENTEVAL_PLANREVIEW_ARMS=architect,security,with_conventions,without_conventions"},
	} {
		if got := catchRateArmSelector(tc.arms); got != tc.want {
			t.Errorf("catchRateArmSelector(%v) = %q, want %q", tc.arms, got, tc.want)
		}
	}
}

func TestRecordCatchRateEvidence_FullRecordingStampsEveryArmRecordedAt(t *testing.T) {
	path := freshCatchEvidence(t)
	rec := readCatchEvidence(t, path)
	if len(rec.Arms) != 5 || len(rec.Baseline.Arms) != 5 {
		t.Fatalf("premise: five arms, got %d record / %d baseline", len(rec.Arms), len(rec.Baseline.Arms))
	}
	for name, a := range rec.Arms {
		if a.RecordedAt != rec.RecordedAt {
			t.Errorf("record arm %q recorded_at %q, want the record's %q", name, a.RecordedAt, rec.RecordedAt)
		}
	}
	for name, a := range rec.Baseline.Arms {
		if a.RecordedAt != rec.RecordedAt {
			t.Errorf("baseline arm %q recorded_at %q, want %q", name, a.RecordedAt, rec.RecordedAt)
		}
	}
}

// TestCatchRateCommittedPairRoundTripsByteIdentical: decoding the COMMITTED
// pair strictly and re-encoding it reproduces both files byte for byte, which
// is what makes a carry-forward of the committed pair verbatim.
func TestCatchRateCommittedPairRoundTripsByteIdentical(t *testing.T) {
	raw := mustRead(t, committedCatchEvidence)
	rec, err := decodeCatchRateEvidence(raw)
	if err != nil {
		t.Fatal(err)
	}
	b, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(append(b, '\n'), raw) {
		t.Errorf("the committed evidence.json does not round-trip byte for byte")
	}
	rawTrials := mustRead(t, CatchRateTrialsPath(committedCatchEvidence))
	trials, err := readCatchRateTrialsStrict(CatchRateTrialsPath(committedCatchEvidence))
	if err != nil {
		t.Fatal(err)
	}
	b, err = json.MarshalIndent(trials, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(append(b, '\n'), rawTrials) {
		t.Errorf("the committed evidence.trials.json does not round-trip byte for byte")
	}
}

// TestRecordCatchRateEvidence_ArmSubsetCarriesUnselectedArmsVerbatim is
// done-means 1: re-recording the stale architect arm alone carries every
// other arm, its sidecar trials and the baseline forward byte for byte.
func TestRecordCatchRateEvidence_ArmSubsetCarriesUnselectedArmsVerbatim(t *testing.T) {
	in, path := armsPinnedPair(t)
	raw0, rawTrials0 := mustRead(t, path), mustRead(t, CatchRateTrialsPath(path))
	staleArchitect(t, in)
	if _, err := CheckCatchRateEvidence(path, in, testCatchModel); err == nil || !strings.Contains(err.Error(), "STALE arm(s) architect:") {
		t.Fatalf("premise: the gate names ONLY architect STALE; err = %v", err)
	}
	t1 := catchT0.Add(time.Hour)
	sender := armSender(t, in)
	report, err := recordIn(t, sender, in, path, t1, RecordCatchRateOptions{Arms: []string{ArmArchitect}})
	if err != nil {
		t.Fatalf("subset recording: %v\n%s", err, report)
	}
	raw1, rawTrials1 := mustRead(t, path), mustRead(t, CatchRateTrialsPath(path))
	arms0, arms1 := rawObject(t, raw0, "arms"), rawObject(t, raw1, "arms")
	tArms0, tArms1 := rawObject(t, rawTrials0, "arms"), rawObject(t, rawTrials1, "arms")
	for _, name := range []string{ArmWithoutConventions, ArmWithConventions, ArmSecurity, ArmSecurityPortability} {
		if !bytes.Equal(arms0[name], arms1[name]) {
			t.Errorf("evidence arm %q is not carried verbatim", name)
		}
		if !bytes.Equal(tArms0[name], tArms1[name]) {
			t.Errorf("sidecar arm %q is not carried verbatim", name)
		}
	}
	if !bytes.Equal(rawObject(t, raw0)["baseline"], rawObject(t, raw1)["baseline"]) {
		t.Errorf("the baseline is not carried verbatim")
	}
	rec := readCatchEvidence(t, path)
	rows, err := catchRateArmTable(in)
	if err != nil {
		t.Fatal(err)
	}
	arch := armRow(t, rows, ArmArchitect)
	if rec.Arms[ArmArchitect].PromptFingerprint != arch.fingerprint || rec.Arms[ArmArchitect].RecordedAt != t1.Format(time.RFC3339) {
		t.Errorf("architect = fingerprint %s recorded_at %q, want the current fingerprint and %s", rec.Arms[ArmArchitect].PromptFingerprint, rec.Arms[ArmArchitect].RecordedAt, t1.Format(time.RFC3339))
	}
	if rec.RecordedAt != t1.Format(time.RFC3339) {
		t.Errorf("record recorded_at %q, want %s", rec.RecordedAt, t1.Format(time.RFC3339))
	}
	tr := readCatchTrials(t, path)
	if tr.RecordedAt != rec.RecordedAt || tr.PromptFingerprint != rec.PromptFingerprint {
		t.Errorf("sidecar binds (%s, %s), the record is (%s, %s)", tr.RecordedAt, tr.PromptFingerprint, rec.RecordedAt, rec.PromptFingerprint)
	}
	if want := arch.samples * len(arch.cases); sender.calls[ArmArchitect]+sender.cleanCalls != want || sender.total() != want {
		t.Errorf("model calls: architect %d, total %d; want exactly %d architect calls", sender.calls[ArmArchitect]+sender.cleanCalls, sender.total(), want)
	}
	// R2: a carried arm is labelled, never shown as measured.
	t0 := catchT0.Format(time.RFC3339)
	for _, want := range []string{
		"Arm selection: measured architect; carried forward security (recorded_at " + t0 + "), security_portability (recorded_at " + t0 + "), with_conventions (recorded_at " + t0 + "), without_conventions (recorded_at " + t0 + ").",
		"  security: carried forward (recorded_at " + t0 + "), not measured by this recording",
		"  security_portability: carried forward (recorded_at " + t0 + "), not measured by this recording",
	} {
		if !strings.Contains(report, want) {
			t.Errorf("report lacks %q:\n%s", want, report)
		}
	}
	for _, bad := range []string{"security_portability repository-specific findings", "security false alarms by category", "security_portability false alarms by category", "security absent-control concerns"} {
		if strings.Contains(report, bad) {
			t.Errorf("report presents a carried arm as measured (%q):\n%s", bad, report)
		}
	}
	if _, err := CheckCatchRateEvidence(path, in, testCatchModel); err != nil {
		t.Fatalf("the gate refuses the subset recording: %v", err)
	}
}

// TestRecordCatchRateEvidence_ArmSubsetRePinKeepsOtherBaselineEntries is the
// done-means re-pin: a subset pin moves only the selected arm's baseline
// entry. The T1 recording catches at 0.95 on the conventions arms, so the
// record's conventions entries DIFFER from the pinned ones and a baseline
// rebuilt from the record (not merged) is detectable.
func TestRecordCatchRateEvidence_ArmSubsetRePinKeepsOtherBaselineEntries(t *testing.T) {
	in, path := armsPinnedPair(t)
	k := catchesAtRate(t, 0.95)
	s1 := newScriptedCatchSender(t, k, k)
	s1.persona = personaScriptFor(t, in.RepoRoot)
	if _, err := recordIn(t, s1, in, path, catchT0.Add(time.Hour), RecordCatchRateOptions{}); err != nil {
		t.Fatalf("T1 recording: %v", err)
	}
	raw1 := mustRead(t, path)
	base1 := rawObject(t, raw1, "baseline", "arms")
	recArms1 := rawObject(t, raw1, "arms")
	if bytes.Equal(compactJSON(t, base1[ArmWithConventions]), compactJSON(t, recArms1[ArmWithConventions])) {
		t.Fatal("premise: the record's conventions entry differs from the pinned one")
	}
	staleArchitect(t, in)
	t2 := catchT0.Add(2 * time.Hour)
	const reason = "re-pin the architect arm after the remit edit"
	if report, err := recordIn(t, armSender(t, in), in, path, t2, RecordCatchRateOptions{Arms: []string{ArmArchitect}, PinBaseline: true, Reason: reason}); err != nil {
		t.Fatalf("subset pin: %v\n%s", err, report)
	}
	raw2 := mustRead(t, path)
	base2 := rawObject(t, raw2, "baseline", "arms")
	recArms2 := rawObject(t, raw2, "arms")
	for _, name := range []string{ArmWithoutConventions, ArmWithConventions, ArmSecurity, ArmSecurityPortability} {
		if !bytes.Equal(base2[name], base1[name]) {
			t.Errorf("baseline entry %q moved: a subset pin re-pins only the selected arms", name)
		}
	}
	for _, name := range []string{ArmWithoutConventions, ArmWithConventions} {
		if bytes.Equal(compactJSON(t, base2[name]), compactJSON(t, recArms2[name])) {
			t.Errorf("baseline entry %q equals the record's: it was rebuilt from the record, not kept", name)
		}
	}
	if !bytes.Equal(compactJSON(t, base2[ArmArchitect]), compactJSON(t, recArms2[ArmArchitect])) {
		t.Errorf("the baseline architect entry is not the record's measurement")
	}
	rec := readCatchEvidence(t, path)
	if rec.Baseline.PinnedAt != t2.Format(time.RFC3339) || rec.Baseline.Reason != reason {
		t.Errorf("baseline pinned_at %q reason %q, want %s / %q", rec.Baseline.PinnedAt, rec.Baseline.Reason, t2.Format(time.RFC3339), reason)
	}
	if _, err := CheckCatchRateEvidence(path, in, testCatchModel); err != nil {
		t.Fatalf("the gate refuses the subset pin: %v", err)
	}
}

// TestRecordCatchRateEvidence_ArmSubsetNoPinJudgedAgainstExistingBaseline: a
// selected arm that regresses against its baseline entry is refused.
func TestRecordCatchRateEvidence_ArmSubsetNoPinJudgedAgainstExistingBaseline(t *testing.T) {
	path := freshCatchEvidence(t)
	before, beforeTrials := mustRead(t, path), mustRead(t, CatchRateTrialsPath(path))
	sender := newScriptedCatchSender(t, 0, 0)
	sender.caught[ArmArchitect] = 0
	_, err := record(t, sender, path, catchT0.Add(time.Hour), RecordCatchRateOptions{Arms: []string{ArmArchitect}})
	if err == nil || !errors.Is(err, ErrCatchRateRegressed) || !strings.Contains(err.Error(), "architect catch rate") {
		t.Fatalf("err = %v, want an architect regression against its baseline entry", err)
	}
	if sender.calls[ArmWithoutConventions]+sender.calls[ArmWithConventions] != 0 {
		t.Errorf("unselected arms were measured")
	}
	assertPairBytes(t, path, before, beforeTrials)
}

func TestRecordCatchRateEvidence_ArmSelectionOfEveryArmIsFull(t *testing.T) {
	path := filepath.Join(t.TempDir(), "evidence.json")
	all := committedCatchTrials(t)
	opts := pin("full by name")
	opts.Arms = []string{ArmWithoutConventions, ArmWithConventions, ArmArchitect, ArmSecurity, ArmSecurityPortability}
	report, err := record(t, newScriptedCatchSender(t, all, all), path, catchT0, opts)
	if err != nil {
		t.Fatalf("a selection of every arm over an absent record: %v", err)
	}
	if strings.Contains(report, "Arm selection:") || strings.Contains(report, "carried forward") {
		t.Errorf("a full selection reported a carry-forward:\n%s", report)
	}
	if _, err := CheckCatchRateEvidence(path, committedCatchInputs(), testCatchModel); err != nil {
		t.Fatal(err)
	}
}

func TestRecordCatchRateEvidence_ArmSelectionNarrowsSubsetDryRun(t *testing.T) {
	path := filepath.Join(t.TempDir(), "evidence.json")
	sender := newScriptedCatchSender(t, 0, 0)
	report, err := record(t, sender, path, catchT0, RecordCatchRateOptions{DryRun: true, Subset: CatchRateSubset{TrialsPerCase: 1}, Arms: []string{ArmArchitect}})
	if err != nil {
		t.Fatalf("subset dry run: %v", err)
	}
	if !strings.Contains(report, "  architect:") || strings.Contains(report, "  with_conventions:") || strings.Contains(report, "  security:") {
		t.Errorf("the report is not architect-only:\n%s", report)
	}
	rows, err := catchRateArmTable(committedCatchInputs())
	if err != nil {
		t.Fatal(err)
	}
	if want := len(armRow(t, rows, ArmArchitect).cases); sender.total() != want || sender.calls[ArmArchitect]+sender.cleanCalls != want {
		t.Errorf("model calls %d, want %d architect-case calls only", sender.total(), want)
	}
	assertPairBytes(t, path, nil, nil)
}

func TestCheckCatchRateEvidence_StaleArmsNameTheSelector(t *testing.T) {
	in, path := armsPinnedPair(t)
	staleArchitect(t, in)
	_, err := CheckCatchRateEvidence(path, in, testCatchModel)
	if err == nil || !strings.Contains(err.Error(), "STALE arm(s) architect:") || !strings.Contains(err.Error(), "re-record only those arms: FISHHAWK_AGENTEVAL_PLANREVIEW_ARMS=architect (") {
		t.Fatalf("err = %v, want the STALE prefix and the architect selector", err)
	}

	conv := filepath.Join(t.TempDir(), "conventions.md")
	if err := os.WriteFile(conv, mustRead(t, committedCatchConventions), 0o644); err != nil {
		t.Fatal(err)
	}
	cin := catchInputs(committedCatchCorpus, conv)
	cpath := filepath.Join(t.TempDir(), "evidence.json")
	all := committedCatchTrials(t)
	if _, err := recordIn(t, newScriptedCatchSender(t, all, all), cin, cpath, catchT0, pin("baseline")); err != nil {
		t.Fatal(err)
	}
	b := mustRead(t, conv)
	b[len(b)-2] ^= 0x01
	if err := os.WriteFile(conv, b, 0o644); err != nil {
		t.Fatal(err)
	}
	_, err = CheckCatchRateEvidence(cpath, cin, testCatchModel)
	if err == nil || !strings.Contains(err.Error(), "STALE arm(s) with_conventions:") || !strings.Contains(err.Error(), "FISHHAWK_AGENTEVAL_PLANREVIEW_ARMS=with_conventions,without_conventions") {
		t.Fatalf("err = %v, want with_conventions ONLY stale and the pair-closed selector", err)
	}
}

// ---------------------------------------------------------------------------
// Per-control refusals. Each asserts the sentinel, the named arm(s), ZERO
// model calls and a byte-identical pair, and is a hand-run counterfactual.
// ---------------------------------------------------------------------------

// (C1) Without the guard no known arm is selected, every arm is carried and a
// no-op pair is WRITTEN with a new recorded_at.
func TestRecordCatchRateEvidence_ArmSelectionRefusesUnknownArm(t *testing.T) {
	path := freshCatchEvidence(t)
	before, beforeTrials := mustRead(t, path), mustRead(t, CatchRateTrialsPath(path))
	sender := newScriptedCatchSender(t, 0, 0)
	_, err := record(t, sender, path, catchT0.Add(time.Hour), RecordCatchRateOptions{Arms: []string{"architekt"}})
	assertArmRefusal(t, err, sender, path, before, beforeTrials, "architekt", CatchRateArmsEnv)
}

// (C2) done-means 3. Every arm is fresh and the sender catches at the pinned
// rate, so without the guard with_conventions is measured, passes the
// within-run rule against the carried without_conventions, and is written.
func TestRecordCatchRateEvidence_ArmSelectionRefusesSplitPair(t *testing.T) {
	path := freshCatchEvidence(t)
	before, beforeTrials := mustRead(t, path), mustRead(t, CatchRateTrialsPath(path))
	all := committedCatchTrials(t)
	sender := newScriptedCatchSender(t, all, all)
	_, err := record(t, sender, path, catchT0.Add(time.Hour), RecordCatchRateOptions{Arms: []string{ArmWithConventions}})
	assertArmRefusal(t, err, sender, path, before, beforeTrials, ArmWithoutConventions, ArmWithConventions, "splits the comparison pair")
}

// (C3) done-means 2. The remit is treatment, not instrument, so input_digest
// and comparability still hold: without the guard architect's stale entry is
// carried and the pair is written.
func TestRecordCatchRateEvidence_ArmSelectionRefusesStaleUnselectedArm(t *testing.T) {
	in, path := armsPinnedPair(t)
	staleArchitect(t, in)
	before, beforeTrials := mustRead(t, path), mustRead(t, CatchRateTrialsPath(path))
	sender := armSender(t, in)
	_, err := recordIn(t, sender, in, path, catchT0.Add(time.Hour), RecordCatchRateOptions{Arms: []string{ArmSecurity}})
	assertArmRefusal(t, err, sender, path, before, beforeTrials, "unselected arm(s) architect are STALE", "FISHHAWK_AGENTEVAL_PLANREVIEW_ARMS=architect,security")
}

// (C4) The security input_digest is the SAME bogus value in the record and
// the baseline (comparability holds; the sidecar binds at record level), so
// only the drift guard sees it.
func TestRecordCatchRateEvidence_ArmSelectionRefusesDriftedUnselectedArm(t *testing.T) {
	path := freshCatchEvidence(t)
	rec := readCatchEvidence(t, path)
	setArmInputDigest(rec.Arms, ArmSecurity, "sha256:bogus")
	setArmInputDigest(rec.Baseline.Arms, ArmSecurity, "sha256:bogus")
	writeCatchEvidence(t, path, rec)
	before, beforeTrials := mustRead(t, path), mustRead(t, CatchRateTrialsPath(path))
	sender := newScriptedCatchSender(t, 0, 0)
	_, err := record(t, sender, path, catchT0.Add(time.Hour), RecordCatchRateOptions{Arms: []string{ArmArchitect}})
	assertArmRefusal(t, err, sender, path, before, beforeTrials, "unselected arm(s) security were measured on another instrument")
}

// (C5) The record names old-model while the baseline keeps the current
// model, so C8 and judgeCatchRate both pass: without the guard arms measured
// on old-model are written under a record naming the current model.
func TestRecordCatchRateEvidence_ArmSelectionRefusesMixedInstrument(t *testing.T) {
	path := freshCatchEvidence(t)
	rec := readCatchEvidence(t, path)
	rec.GeneratorModel = "old-model"
	writeCatchEvidence(t, path, rec)
	before, beforeTrials := mustRead(t, path), mustRead(t, CatchRateTrialsPath(path))
	sender := newScriptedCatchSender(t, 0, 0)
	_, err := record(t, sender, path, catchT0.Add(time.Hour), RecordCatchRateOptions{Arms: []string{ArmArchitect}})
	assertArmRefusal(t, err, sender, path, before, beforeTrials, "old-model", "instrument")
}

// (C6) No record to carry from: absent, legacy v1, or baseline-less.
func TestRecordCatchRateEvidence_ArmSelectionRefusesWithoutExistingRecord(t *testing.T) {
	opts := pin("subset over nothing")
	opts.Arms = []string{ArmArchitect}

	absent := filepath.Join(t.TempDir(), "evidence.json")
	sender := newScriptedCatchSender(t, 0, 0)
	_, err := record(t, sender, absent, catchT0, opts)
	assertArmRefusal(t, err, sender, absent, nil, nil, "is absent")

	legacy := filepath.Join(t.TempDir(), "evidence.json")
	v1 := []byte(`{"schema":"planreview-catchrate-evidence-v1","samples_per_case":12,"arms":{"without_conventions":{"per_case":{}},"with_conventions":{"per_case":{}}}}` + "\n")
	v1Trials := []byte(`{"schema":"planreview-catchrate-trials-v1"}` + "\n")
	if err := os.WriteFile(legacy, v1, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(CatchRateTrialsPath(legacy), v1Trials, 0o644); err != nil {
		t.Fatal(err)
	}
	sender = newScriptedCatchSender(t, 0, 0)
	_, err = record(t, sender, legacy, catchT0, opts)
	assertArmRefusal(t, err, sender, legacy, v1, v1Trials, "legacy "+catchRateEvidenceSchemaV1)

	noBase := freshCatchEvidence(t)
	rec := readCatchEvidence(t, noBase)
	rec.Baseline = nil
	writeCatchEvidence(t, noBase, rec)
	before, beforeTrials := mustRead(t, noBase), mustRead(t, CatchRateTrialsPath(noBase))
	sender = newScriptedCatchSender(t, 0, 0)
	_, err = record(t, sender, noBase, catchT0.Add(time.Hour), opts)
	assertArmRefusal(t, err, sender, noBase, before, beforeTrials, "no pinned baseline")
}

// (C7) The sidecar still decodes, so without the guard its rows are re-bound
// to the new record and written.
func TestRecordCatchRateEvidence_ArmSelectionRefusesUnboundSidecar(t *testing.T) {
	path := freshCatchEvidence(t)
	tr := readCatchTrials(t, path)
	tr.RecordedAt = "2026-01-01T00:00:00Z"
	writeCatchTrials(t, path, tr)
	before, beforeTrials := mustRead(t, path), mustRead(t, CatchRateTrialsPath(path))
	sender := newScriptedCatchSender(t, 0, 0)
	_, err := record(t, sender, path, catchT0.Add(time.Hour), RecordCatchRateOptions{Arms: []string{ArmArchitect}})
	assertArmRefusal(t, err, sender, path, before, beforeTrials, "sidecar does not bind", "recorded_at")
}

// (C8) A pin makes judgeCatchRate tolerate a non-comparable baseline, so
// without the guard a baseline keeping a non-comparable security entry is
// written.
func TestRecordCatchRateEvidence_ArmSelectionRefusesNonComparableCarriedBaseline(t *testing.T) {
	path := freshCatchEvidence(t)
	rec := readCatchEvidence(t, path)
	setArmInputDigest(rec.Baseline.Arms, ArmSecurity, "sha256:bogus")
	writeCatchEvidence(t, path, rec)
	before, beforeTrials := mustRead(t, path), mustRead(t, CatchRateTrialsPath(path))
	sender := newScriptedCatchSender(t, 0, 0)
	opts := pin("subset pin")
	opts.Arms = []string{ArmArchitect}
	_, err := record(t, sender, path, catchT0.Add(time.Hour), opts)
	assertArmRefusal(t, err, sender, path, before, beforeTrials, "cannot keep judging the carried arm(s)", `arm "security" case inputs changed`)
}

// (C-i) retired_arm is in the record, the baseline AND the sidecar (so C7
// passes); the per-row loops iterate table rows only and a pin tolerates
// non-comparability, so without the guard a record dropping retired_arm is
// written beside a baseline keeping it.
func TestRecordCatchRateEvidence_ArmSelectionRefusesRetiredArm(t *testing.T) {
	path := freshCatchEvidence(t)
	rec := readCatchEvidence(t, path)
	rec.Arms["retired_arm"] = rec.Arms[ArmSecurity]
	rec.Baseline.Arms["retired_arm"] = rec.Baseline.Arms[ArmSecurity]
	writeCatchEvidence(t, path, rec)
	tr := readCatchTrials(t, path)
	tr.Arms["retired_arm"] = tr.Arms[ArmSecurity]
	writeCatchTrials(t, path, tr)
	before, beforeTrials := mustRead(t, path), mustRead(t, CatchRateTrialsPath(path))
	sender := newScriptedCatchSender(t, 0, 0)
	opts := pin("subset pin")
	opts.Arms = []string{ArmArchitect}
	_, err := record(t, sender, path, catchT0.Add(time.Hour), opts)
	assertArmRefusal(t, err, sender, path, before, beforeTrials, "retired_arm", "arm-table change needs a full recording")
}

// (R3) Without a pin, a selected arm whose baseline entry is not comparable
// is refused BEFORE its model calls (it would otherwise be measured and then
// refused by judgeCatchRate).
func TestRecordCatchRateEvidence_ArmSelectionRefusesNonComparableSelectedArm(t *testing.T) {
	path := freshCatchEvidence(t)
	rec := readCatchEvidence(t, path)
	setArmInputDigest(rec.Baseline.Arms, ArmArchitect, "sha256:bogus")
	writeCatchEvidence(t, path, rec)
	before, beforeTrials := mustRead(t, path), mustRead(t, CatchRateTrialsPath(path))
	sender := newScriptedCatchSender(t, 0, 0)
	_, err := record(t, sender, path, catchT0.Add(time.Hour), RecordCatchRateOptions{Arms: []string{ArmArchitect}})
	assertArmRefusal(t, err, sender, path, before, beforeTrials, `selected arm "architect"`, "FISHHAWK_AGENTEVAL_PLANREVIEW_PIN_REASON")
}

// The subset-knob refusal names the recordable arm selector.
func TestRecordCatchRateEvidence_SubsetRefusalNamesArmSelector(t *testing.T) {
	_, err := record(t, newScriptedCatchSender(t, 0, 0), filepath.Join(t.TempDir(), "evidence.json"), catchT0, RecordCatchRateOptions{Subset: CatchRateSubset{TrialsPerCase: 1}})
	if err == nil || !errors.Is(err, ErrCatchRateSubsetNotRecordable) || !strings.Contains(err.Error(), CatchRateArmsEnv) {
		t.Fatalf("err = %v, want the subset refusal naming %s", err, CatchRateArmsEnv)
	}
}

// (C8, instrument half) A baseline pinned on another generator model or
// catch rule cannot keep judging the carried arms. The record keeps the
// current model and rule (C5 passes) and a pin tolerates a non-comparable
// baseline, so only carriedBaselineComparable sees it.
func TestRecordCatchRateEvidence_ArmSelectionRefusesBaselineOnOtherInstrument(t *testing.T) {
	for _, tc := range []struct {
		name string
		edit func(*CatchRateBaseline)
		want string
	}{
		{"generator model", func(b *CatchRateBaseline) { b.GeneratorModel = "old-model" }, `the baseline's generator model "old-model"`},
		{"catch rule", func(b *CatchRateBaseline) { b.CatchRuleVersion = "planreview-catch-v0: old" }, "the baseline's catch rule planreview-catch-v0"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := freshCatchEvidence(t)
			rec := readCatchEvidence(t, path)
			tc.edit(rec.Baseline)
			writeCatchEvidence(t, path, rec)
			before, beforeTrials := mustRead(t, path), mustRead(t, CatchRateTrialsPath(path))
			sender := newScriptedCatchSender(t, 0, 0)
			opts := pin("subset pin")
			opts.Arms = []string{ArmArchitect}
			_, err := record(t, sender, path, catchT0.Add(time.Hour), opts)
			assertArmRefusal(t, err, sender, path, before, beforeTrials, "cannot keep judging the carried arm(s)", tc.want)
		})
	}
}
