package fixupattempt

import (
	"reflect"
	"testing"
)

// Real fixtures from the incident that motivated this package (#2896): run
// 925addab-92f4-43c7-abbf-b58325e29fd8, stage 908f1b25, the two concerns routed
// to fix-up pass 1 (concern f5c464c6, the medium; concern 9955251a, the low) and
// the operator's routed reason text for passes 1 and 2. Copied VERBATIM from
// that run's stage_fixup_triggered audit payloads so the coverage claim is
// proved against reality rather than against synthetic strings (binding
// condition 2). Do not "clean up" this text — its exact shape is the fixture.

// realNoteMedium is concern f5c464c6's note: the MEDIUM that fix-up pass 1
// silently dropped. It names no repository path at all.
const realNoteMedium = "The repository\u2019s long-form security documentation initially overstates the authorization gate as applying to every caller without forge read. This contradicts the documented exceptions and the binding requirement that every characterization of the 403 use the non-admin cookie-session qualification. The inconsistency remains until that initial characterization accurately reflects the qualified caller class."

// realNoteLow is concern 9955251a's note: the LOW that pass 1 did land. It names
// test and function identifiers but no repository path.
const realNoteLow = "TestOnboardingReadiness_AnonymousBeforeVisibility is a weak ordering pin by the test's own admission (CONDITION 2 disclosure): repoFilterFor also short-circuits anonymous callers with a nil filter, so if the guard were hoisted above the anonymous check the test would still see 401 (or at worst 403/200 rather than 503), and the 503-vs-401 discrimination the plan described does not actually exist. The test still asserts something load-bearing (401 authentication_required + zero mirror calls for anonymous), and the disclosure is honest, but it should not be read as a hoist counterfactual. Resolution: none required beyond keeping the comment accurate; the README's claim that _AnonymousBeforeVisibility 'pins' ordering could be softened to match. Diff-only review \u2014 I could not read repoFilterFor to confirm the anonymous short-circuit the comment describes."

// realReasonPass1 is the operator's routed reason for pass 1 — the two-concern
// pass. It names both files explicitly.
const realReasonPass1 = "Two documentation-accuracy corrections. Both are small and precise. The implementation and tests are correct and need NO change \u2014 I re-ran your counterfactual independently against the PR head and it reproduces exactly (RED on `status = 200, want 403` at onboarding_test.go:564 with the leaked installation+spec body, GREEN after restore, and identical without -race). Do not touch onboarding.go or either test file's logic.\n\nCONCERN 1 (sol, medium) \u2014 docs/onboarding.md, the opening sentence of the \"Repo read-visibility gate (#1512)\" section.\n\nLines 234-235 currently read, unqualified:\n\n    a caller who holds no forge `read` on the queried repo is denied `403 repo_forbidden`\n\nThat is overbroad in exactly the way the approval made binding. Three identity classes hold no forge `read` and are NOT denied \u2014 bearer/MCP identities, workspace admins, and no-mirror deployments \u2014 and your own section says so three paragraphs later.\n\nI want to be fair about what you did here, because it affects how you should fix it: the section as a whole is genuinely good. It enumerates all three unfiltered classes with rationale, and the \"Who CAN see the `403`\" block even pre-empts this exact error with an explicit parenthetical about why \"a browser cookie session on a mirror-wired deployment\" would be wrong. The defect is ONLY that the lead sentence states the unqualified claim before the qualifications arrive.\n\nThat still matters, because a lead sentence is what gets skimmed, quoted, and copied into other docs. Fix the sentence so it is accurate standing alone \u2014 carry the non-admin cookie-session qualification into it, or explicitly forward-reference the exceptions (\"subject to the three unfiltered classes below\"). Either shape is fine; a reader must not be able to take that one sentence away and be wrong.\n\nThen sweep the same section for any other unqualified characterization of who gets the 403 and fix those the same way. Do not weaken or delete the \"Who CAN see the `403`\" block \u2014 it is the best part of the section.\n\nCONCERN 2 (fable, low) \u2014 backend/internal/server/README.md, the claim that `_AnonymousBeforeVisibility` \"pins\" ordering.\n\nYour CONDITION 2 disclosure was the right call and I want to reinforce it rather than have you walk it back: you found that an anonymous caller is blocked by TWO independent mechanisms (the handler's 401 precedes the guard, AND repoFilterFor returns nil for anonymous before the mirror is consulted), so that test cannot discriminate a hoisted guard the way the plan predicted. You said so in the test's own doc comment. That is exactly right.\n\nThe README did not get the same treatment \u2014 it still claims the test \"pins\" ordering. Soften it to match the test comment: the test asserts something load-bearing (anonymous \u2192 401 authentication_required with a mirror wired and the mirror never consulted) but is not a hoist counterfactual. Keep `_MalformedRepoBeforeVisibility` described as a genuine discriminator, because it is one \u2014 400 before 403 with the mirror never handed the malformed key.\n\nThe general principle, worth applying anywhere else in the docs you touched: a test that cannot fail when the control is removed must not be described as pinning that control. Describing it accurately is not a weakness in the change; it is what lets the next person trust the rest of the claims.\n\nScope stays the 8 files already declared. Create no files. Change no test logic and no production code."

// realReasonPass2 is the operator's routed reason for pass 2, the forced
// single-concern re-route after the drop was caught by hand.
const realReasonPass2 = "ONE change, in ONE file. The previous pass fixed the README item and silently skipped this one \u2014 docs/onboarding.md was not touched at all (verified: the fix-up commit changed only backend/internal/server/README.md, 2 insertions / 2 deletions). That is why this is routed alone.\n\nTHE ONLY THING TO CHANGE: the lead sentence of the \"Repo read-visibility gate (#1512)\" section in docs/onboarding.md. It currently reads, unqualified:\n\n    Since #1512 (ADR-057 Amendment A2 / #2071), the readiness endpoint applies the\n    repo-scoped read-visibility gate the product already owns: a caller who holds\n    no forge `read` on the queried repo is denied `403 repo_forbidden` **before**\n    any installation resolve or spec fetch, so verbatim `spec.Error` text and\n    installation state never reach a caller with no read on the repo.\n\n\"a caller who holds no forge `read` ... is denied\" is false as stated. Three identity classes hold no forge read and are NOT denied \u2014 bearer/MCP identities, workspace admins, and no-mirror deployments \u2014 and your own section documents all three a few paragraphs below. The sentence also repeats the unqualified claim at its end (\"never reach a caller with no read on the repo\").\n\nMake that sentence true standing alone. Concretely, qualify BOTH halves \u2014 for example:\n\n    ... a NON-ADMIN cookie-session caller who holds no forge `read` on the queried\n    repo is denied `403 repo_forbidden` **before** any installation resolve or spec\n    fetch, so verbatim `spec.Error` text and installation state never reach such a\n    caller. (Three identity classes stay unfiltered \u2014 see below.)\n\nThe exact wording is yours. The test is: a reader who takes ONLY that sentence away must not end up with a false belief about who is denied.\n\nThen re-read the rest of that section and fix any other sentence that characterizes who receives the 403 without the qualification. Do not weaken or remove the \"Three identity classes are UNFILTERED\" list or the \"Who CAN see the `403`\" block \u2014 those are correct and are the best part of the section.\n\nNOTHING ELSE. Do not touch onboarding.go, either test file, or any other doc. Do not change test logic. The implementation is verified correct \u2014 I re-ran the counterfactual myself against this PR head and it reproduces exactly (RED on `status = 200, want 403` at onboarding_test.go:564, GREEN after restore, identical without -race). The README fix from your previous pass is good and should stay as written.\n\nIf for any reason you conclude this sentence should not change, do NOT silently leave it \u2014 say so explicitly in your self-report with your reasoning. A skipped item reported as done is worse than a disagreement stated plainly."

// realCandidates is a representative candidate set for that run: the declared
// scope files the two concerns and the reason could name.
var realCandidates = []string{
	"backend/internal/server/README.md",
	"backend/internal/server/onboarding.go",
	"backend/internal/server/onboarding_test.go",
	"docs/onboarding.md",
}

func touchedSet(paths ...string) map[string]struct{} {
	m := make(map[string]struct{}, len(paths))
	for _, p := range paths {
		m[p] = struct{}{}
	}
	return m
}

// TestImplicated covers the resolver's real risk surface: the SHAPES a reviewer
// actually writes a path in (plain, `./`-prefixed, backticked, parenthesised,
// `path:LINE`, repo-prefix omitted) must resolve, and the near-miss shapes must
// NOT — a spurious match marks an untouched file as touched and MASKS a genuine
// drop, which is the exact failure this package exists to detect.
func TestImplicated(t *testing.T) {
	candidates := []string{
		"backend/internal/server/README.md",
		"backend/internal/server/onboarding.go",
		"docs/onboarding.md",
		"site/README.md",
		"site/dashboard.go",
	}
	tests := []struct {
		name string
		note string
		want []string
	}{
		{"plain repo-relative path", "Fix docs/onboarding.md please", []string{"docs/onboarding.md"}},
		{"dot-slash prefixed", "see ./docs/onboarding.md", []string{"docs/onboarding.md"}},
		{"backticked", "the `docs/onboarding.md` lead sentence", []string{"docs/onboarding.md"}},
		{"parenthesised", "(docs/onboarding.md) is wrong", []string{"docs/onboarding.md"}},
		{"path:LINE suffix", "docs/onboarding.md:234 is unqualified", []string{"docs/onboarding.md"}},
		{"path:LINE-RANGE suffix", "docs/onboarding.md:234-235 reads", []string{"docs/onboarding.md"}},
		{"trailing sentence period", "The offender is docs/onboarding.md.", []string{"docs/onboarding.md"}},
		{"trailing comma", "docs/onboarding.md, and nothing else", []string{"docs/onboarding.md"}},
		{"repo prefix omitted, unique suffix", "internal/server/onboarding.go hoists the guard",
			[]string{"backend/internal/server/onboarding.go"}},
		{"several paths, candidate order", "both backend/internal/server/README.md and docs/onboarding.md",
			[]string{"backend/internal/server/README.md", "docs/onboarding.md"}},
		{"deduped", "docs/onboarding.md twice: docs/onboarding.md", []string{"docs/onboarding.md"}},

		// The negative boundary cases. Each of these WOULD match under a naive
		// strings.Contains and would then mask a genuine drop.
		{"longer path is not the candidate", "the stale docs/onboarding.md.bak copy", nil},
		{"prefixed path is not the candidate", "see xdocs/onboarding.md instead", nil},
		{"ambiguous suffix is undeterminable, never a guess", "the README.md needs a row", nil},
		// The suffix rule must match on a '/' BOUNDARY. Without it a note naming
		// board.go (a file that is not a candidate at all) would implicate
		// site/dashboard.go, marking an unrelated file as this concern's — and a
		// spurious match masks a genuine drop.
		{"suffix must fall on a path boundary", "board.go needs the same treatment", nil},
		{"non-candidate path yields nothing (anti-phantom)", "edit backend/internal/server/trace.go", nil},
		{"bare prose word", "The README did not get the same treatment", nil},
		{"empty note", "", nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Implicated(tt.note, candidates)
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("Implicated(%q) = %v, want %v", tt.note, got, tt.want)
			}
		})
	}
}

// TestImplicated_NoCandidates pins the empty-candidate-set guard: with nothing
// to anchor on the resolver can only mint phantoms, so it resolves nothing.
func TestImplicated_NoCandidates(t *testing.T) {
	if got := Implicated("docs/onboarding.md is wrong", nil); got != nil {
		t.Fatalf("Implicated with no candidates = %v, want nil", got)
	}
}

// TestImplicated_LongestCandidateNotShadowed pins that a nested candidate does
// not shadow a longer one: a note naming the deeper path must resolve to the
// deeper path, not to its prefix.
func TestImplicated_LongestCandidateNotShadowed(t *testing.T) {
	candidates := []string{"docs/onboarding.md", "docs/onboarding.md.tmpl"}
	got := Implicated("regenerate docs/onboarding.md.tmpl", candidates)
	want := []string{"docs/onboarding.md.tmpl"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Implicated = %v, want %v", got, want)
	}
}

// TestImplicated_RealIncidentNotes is the reality check binding condition 2
// demands: run the REAL routed text from #2896's own reproduction through the
// resolver and report what actually resolves.
//
// The honest result, and the reason this package does not classify from concern
// NOTES alone: NEITHER real note resolves a path. The medium (f5c464c6) — the
// concern the pass silently dropped — describes its target as "the repository's
// long-form security documentation" and names no file; the low (9955251a) names
// a test function and a Go identifier but no path. A note-only derivation would
// therefore have bucketed BOTH concerns as undeterminable and detected nothing,
// shipping as an inert control that reads as coverage.
//
// The operator's routed REASON, part of the same routed instruction text the
// agent read, names both files explicitly — which is why the caller also
// resolves that shared text (reported unattributed when several concerns are
// routed). Against the reason, docs/onboarding.md resolves, so the incident IS
// detected: the pass touched only backend/internal/server/README.md.
func TestImplicated_RealIncidentNotes(t *testing.T) {
	if got := Implicated(realNoteMedium, realCandidates); got != nil {
		t.Fatalf("real medium note resolved %v; the fixture is expected to name no path — "+
			"if this now resolves, re-read the note-only coverage claim in the README", got)
	}
	if got := Implicated(realNoteLow, realCandidates); got != nil {
		t.Fatalf("real low note resolved %v; want nil", got)
	}

	// The routed reason for the two-concern pass names both files.
	gotPass1 := Implicated(realReasonPass1, realCandidates)
	wantPass1 := []string{
		"backend/internal/server/README.md",
		"backend/internal/server/onboarding.go",
		"backend/internal/server/onboarding_test.go",
		"docs/onboarding.md",
	}
	if !reflect.DeepEqual(gotPass1, wantPass1) {
		t.Fatalf("real pass-1 reason resolved %v, want %v", gotPass1, wantPass1)
	}

	// THE INCIDENT: pass 1 committed only backend/internal/server/README.md.
	// docs/onboarding.md — the medium's file — must surface as untouched.
	untouched := Untouched(gotPass1, touchedSet("backend/internal/server/README.md"))
	found := false
	for _, p := range untouched {
		if p == "docs/onboarding.md" {
			found = true
		}
	}
	if !found {
		t.Fatalf("the #2896 incident would NOT have been detected: untouched = %v, "+
			"want it to include docs/onboarding.md", untouched)
	}

	// Pass 2's single-concern reason names the same file, so a single-concern
	// pass attributes it directly to the routed concern.
	gotPass2 := Implicated(realReasonPass2, realCandidates)
	hasDoc := false
	for _, p := range gotPass2 {
		if p == "docs/onboarding.md" {
			hasDoc = true
		}
	}
	if !hasDoc {
		t.Fatalf("real pass-2 reason resolved %v, want it to include docs/onboarding.md", gotPass2)
	}
}

// TestUnattempted is the classifier's table, including the MANDATED two-concern
// negative in its pure form and the condition-1 position case.
func TestUnattempted(t *testing.T) {
	t.Run("second of two id-less concerns is reported at position 2", func(t *testing.T) {
		// Binding condition 1: only the SECOND routed concern is unattempted, and
		// neither carries an id. A Position re-derived from the FILTERED finding
		// set would label this finding 1 and send an operator to inspect the
		// concern that WAS attempted. Dropping the FIRST instead would pass
		// either way and prove nothing.
		routed := []Concern{
			{Position: 1, Severity: "low", Category: "verification", Implicated: []string{"a.go"}},
			{Position: 2, Severity: "medium", Category: "security", Implicated: []string{"b.go"}},
		}
		findings, undet := Unattempted(routed, touchedSet("a.go"))
		if len(findings) != 1 {
			t.Fatalf("findings = %d, want 1: %+v", len(findings), findings)
		}
		if findings[0].Position != 2 {
			t.Fatalf("Position = %d, want 2 — the finding mislabels WHICH concern was dropped", findings[0].Position)
		}
		if findings[0].Severity != "medium" {
			t.Fatalf("Severity = %q, want medium", findings[0].Severity)
		}
		if undet != 0 {
			t.Fatalf("undeterminable = %d, want 0", undet)
		}
	})

	t.Run("all attempted yields nothing", func(t *testing.T) {
		routed := []Concern{
			{ID: "c1", Position: 1, Implicated: []string{"a.go"}},
			{ID: "c2", Position: 2, Implicated: []string{"b.go"}},
		}
		findings, undet := Unattempted(routed, touchedSet("a.go", "b.go"))
		if len(findings) != 0 || undet != 0 {
			t.Fatalf("findings=%v undeterminable=%d, want none", findings, undet)
		}
	})

	t.Run("partially touched implicated set counts as attempted", func(t *testing.T) {
		routed := []Concern{{ID: "c1", Position: 1, Implicated: []string{"a.go", "b.go"}}}
		findings, _ := Unattempted(routed, touchedSet("b.go"))
		if len(findings) != 0 {
			t.Fatalf("findings = %+v, want none: one named file was touched", findings)
		}
	})

	t.Run("empty implicated set is counted undeterminable, never a finding", func(t *testing.T) {
		routed := []Concern{
			{ID: "c1", Position: 1},
			{ID: "c2", Position: 2, Implicated: []string{"b.go"}},
		}
		findings, undet := Unattempted(routed, touchedSet("b.go"))
		if len(findings) != 0 {
			t.Fatalf("findings = %+v, want none — promoting an undeterminable concern would fire on every pass", findings)
		}
		if undet != 1 {
			t.Fatalf("undeterminable = %d, want 1", undet)
		}
	})

	t.Run("finding carries its own implicated file list", func(t *testing.T) {
		routed := []Concern{{ID: "c1", Position: 1, Implicated: []string{"a.go", "b.go"}}}
		findings, _ := Unattempted(routed, touchedSet("z.go"))
		if len(findings) != 1 || !reflect.DeepEqual(findings[0].ImplicatedFiles, []string{"a.go", "b.go"}) {
			t.Fatalf("findings = %+v, want the two named files", findings)
		}
		// Mutating the source must not reach the finding.
		routed[0].Implicated[0] = "mutated"
		if findings[0].ImplicatedFiles[0] != "a.go" {
			t.Fatal("Finding.ImplicatedFiles aliases the caller's slice")
		}
	})
}

func TestUntouched(t *testing.T) {
	got := Untouched([]string{"a.go", "b.go", "a.go", "c.go"}, touchedSet("b.go"))
	want := []string{"a.go", "c.go"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Untouched = %v, want %v", got, want)
	}
	if got := Untouched(nil, touchedSet("b.go")); got != nil {
		t.Fatalf("Untouched(nil) = %v, want nil", got)
	}
}
