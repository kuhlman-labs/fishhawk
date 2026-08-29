package spec

import (
	"reflect"
	"strings"
	"testing"

	"github.com/bmatcuk/doublestar/v4"
)

// allPresets is the closed set the library ships.
var allPresets = []Preset{PresetLow, PresetMedium, PresetHigh}

// parsePreset loads and fully validates a preset (ParseBytes runs schema
// + semantic validation), returning the typed *Spec.
func parsePreset(t *testing.T, p Preset) *Spec {
	t.Helper()
	data, err := PresetBytes(p)
	if err != nil {
		t.Fatalf("PresetBytes(%q): %v", p, err)
	}
	s, err := ParseBytes(data)
	if err != nil {
		t.Fatalf("preset %q failed ParseBytes (schema + semantic): %v", p, err)
	}
	return s
}

// TestPresetsParseAndValidate is the drift-proof gate mirroring the CLI
// one: every mirrored preset must pass ParseBytes (schema + semantic)
// through the backend's embedded workflow-v2 schema. The same bytes
// validating through both the CLI and backend embed copies is the
// cross-boundary check that the docs/spec canonical and both mirrors
// stay in lockstep.
func TestPresetsParseAndValidate(t *testing.T) {
	for _, p := range allPresets {
		p := p
		t.Run(string(p), func(t *testing.T) {
			s := parsePreset(t, p)
			// Re-run the semantic pass explicitly for good measure.
			if err := Validate(s); err != nil {
				t.Fatalf("preset %q failed semantic Validate: %v", p, err)
			}
			if _, ok := s.Workflows["feature_change"]; !ok {
				t.Fatalf("preset %q has no feature_change workflow", p)
			}
		})
	}
}

// operatorAgentOf returns the feature_change workflow's operator_agent
// block from a parsed preset (nil if absent).
func operatorAgentOf(t *testing.T, s *Spec) *OperatorAgent {
	t.Helper()
	wf, ok := s.Workflows["feature_change"]
	if !ok {
		t.Fatal("no feature_change workflow")
	}
	return wf.OperatorAgent
}

// TestPresetOperatorAgentPerTier is the done-means assertion (per
// #1169): it pins the delegation block each parsed preset resolves to,
// so a no-op / comment-only preset edit fails even though the scope path
// was touched.
//
// Since E52.8 the presets declare `autonomy: <tier>` and the parser
// DERIVES the block via expandTier, so the expectation is the FROZEN
// literal table (frozenTierBlocks, written out longhand in
// v2autonomy_test.go) rather than anything the parser computed. Its
// byte-level companion is TestShippedPresetsDeclareTheirTier, which pins
// WHICH tier each preset chose; neither side supplies the other's answer.
func TestPresetOperatorAgentPerTier(t *testing.T) {
	t.Run("low delegates nothing", func(t *testing.T) {
		oa := operatorAgentOf(t, parsePreset(t, PresetLow))
		assertFrozenTierBlock(t, TierLow, oa)
		if len(oa.MustPageHuman) != 0 {
			t.Errorf("low must page on nothing, got %v — it absorbs nothing to except", oa.MustPageHuman)
		}
	})

	t.Run("medium delegates approve/fixup/retry, not waive/merge", func(t *testing.T) {
		oa := operatorAgentOf(t, parsePreset(t, PresetMedium))
		assertFrozenTierBlock(t, TierMedium, oa)
		if oa.MayWaive != "" || oa.MayMerge != "" {
			t.Errorf("medium must NOT delegate waive/merge, got %q/%q", oa.MayWaive, oa.MayMerge)
		}
		assertPageEvents(t, oa)
	})

	t.Run("high adds waive and merge", func(t *testing.T) {
		oa := operatorAgentOf(t, parsePreset(t, PresetHigh))
		assertFrozenTierBlock(t, TierHigh, oa)
		if oa.MayWaive != ConditionSoloLow || oa.MayMerge != ConditionGatesResolvedCIGreen {
			t.Errorf("high must delegate waive/merge, got %q/%q", oa.MayWaive, oa.MayMerge)
		}
		assertPageEvents(t, oa)
	})
}

// assertFrozenTierBlock compares a parsed preset's derived block against
// the frozen literal expectation for its tier.
func assertFrozenTierBlock(t *testing.T, tier AutonomyTier, got *OperatorAgent) {
	t.Helper()
	want, ok := frozenTierBlocks[tier]
	if !ok {
		t.Fatalf("no frozen block for tier %s", tier)
	}
	if got == nil {
		t.Fatalf("preset derived a nil operator_agent block, want %+v", want)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("tier %s derived %+v, want the frozen block %+v", tier, got, want)
	}
}

// assertPageEvents pins the shared 7-event page list, in the v2 spelling
// (`gating_reviewer_reject`; the bare `reviewer_reject` token v0/v1 used
// is not declarable at major 2).
func assertPageEvents(t *testing.T, oa *OperatorAgent) {
	t.Helper()
	want := []string{
		"gating_reviewer_reject", "plan_rejection", "scope_amendment",
		"budget_override", "policy_override", "exception_request",
		"requirement_arbitration",
	}
	if len(oa.MustPageHuman) != len(want) {
		t.Fatalf("must_page_human has %d events, want %d: %v", len(oa.MustPageHuman), len(want), oa.MustPageHuman)
	}
	for i, w := range want {
		if oa.MustPageHuman[i] != w {
			t.Errorf("must_page_human[%d] = %q, want %q", i, oa.MustPageHuman[i], w)
		}
	}
}

// --- the shipped workflow-v2 preset bytes (E52.8 / #2220) --------------------

// TestShippedPresetsDeclareTheirTier is the byte-level companion to the
// frozen tier table: each shipped preset declares `version: "2"`, carries
// the one-word tier shorthand `autonomy: <its own tier>`, and declares NO
// `operator_agent` key — the v0/v1 block workflow-v2 removed. It also
// pins that the v0/v1 spellings v2 renamed are gone.
func TestShippedPresetsDeclareTheirTier(t *testing.T) {
	for _, p := range allPresets {
		p := p
		t.Run(string(p), func(t *testing.T) {
			text := string(presetText(t, p))
			if !strings.Contains(text, "version: \"2\"\n") {
				t.Errorf("preset %q does not declare version: \"2\":\n%s", p, text)
			}
			if strings.Contains(text, "operator_agent:") {
				t.Errorf("preset %q still declares an operator_agent block (removed at workflow-v2):\n%s", p, text)
			}
			if want := "\n    autonomy: " + string(p) + "\n"; !strings.Contains(text, want) {
				t.Errorf("preset %q does not declare %q:\n%s", p, strings.TrimSpace(want), text)
			}
			for _, legacy := range []string{"drive:", "max_runtime_minutes:", "must_page_human:"} {
				if strings.Contains(text, legacy) {
					t.Errorf("preset %q still uses the v0/v1 spelling %q:\n%s", p, legacy, text)
				}
			}
		})
	}
}

// TestShippedPresetsDeclareReviewAuthority is criterion 6's text half
// (E53.2 / #2225): every preset spells `authority: advisory` explicitly on
// its plan and implement reviewers blocks (two occurrences), making visible
// what the counts already resolve to. The parsed-value-vs-ResolveAuthority
// half lives in the external spec_test package (planreview imports spec).
func TestShippedPresetsDeclareReviewAuthority(t *testing.T) {
	for _, p := range allPresets {
		p := p
		t.Run(string(p), func(t *testing.T) {
			text := string(presetText(t, p))
			if got := strings.Count(text, "authority: advisory"); got != 2 {
				t.Errorf("preset %q declares `authority: advisory` %d times, want 2 (plan + implement):\n%s", p, got, text)
			}
		})
	}
}

// TestPresetsAreLockstepBaseAndTierDelta is the machine-enforced
// statement of the base-plus-deltas invariant, mirrored from
// cli/internal/spec so BOTH embedded mirror sets are held to it.
//
// Cross-FILE inclusion is deliberately out of ADR-067 scope, so each
// preset must remain a standalone document `fishhawk init` writes whole;
// this test — not a generator — is what keeps the three in lockstep.
// It strips ONLY the leading header comment block (the one deliberately
// tier-specific prose block) and the single `autonomy:` line, then
// compares the remainders BYTE FOR BYTE. Every other comment
// participates: stripping all comments would leave the test unable to
// detect exactly the tier-specific drift it exists to prevent.
//
// The vacuity guard is that the line it stripped from each preset must
// have been that preset's OWN autonomy line, so the test cannot pass on
// three files that share no autonomy key at all.
func TestPresetsAreLockstepBaseAndTierDelta(t *testing.T) {
	normalized := make(map[Preset]string, len(allPresets))
	for _, p := range allPresets {
		body, headerLines := stripLeadingCommentBlock(string(presetText(t, p)))
		if headerLines == 0 {
			t.Fatalf("preset %q has no leading header comment block to strip", p)
		}
		rest, tier, n := stripAutonomyLine(body)
		if n != 1 {
			t.Fatalf("preset %q declares %d autonomy lines, want exactly 1", p, n)
		}
		if tier != string(p) {
			t.Fatalf("preset %q stripped `autonomy: %s`, want its own tier — the comparison below would be vacuous", p, tier)
		}
		normalized[p] = rest
	}
	base := normalized[PresetMedium]
	for _, p := range []Preset{PresetLow, PresetHigh} {
		if normalized[p] != base {
			t.Errorf("preset %q is not the medium base plus its one autonomy line.\n--- %s ---\n%s\n--- medium ---\n%s", p, p, normalized[p], base)
		}
	}
	// Load-bearing guard: the shared base must be substantial, so an
	// accidental over-strip cannot make three empty strings compare equal.
	if len(base) < 1000 {
		t.Fatalf("normalized base is only %d bytes — the lockstep comparison is not load-bearing", len(base))
	}
}

// stripLeadingCommentBlock removes the contiguous run of comment and
// blank lines at the top of a document, returning the remainder and how
// many lines it dropped.
func stripLeadingCommentBlock(text string) (string, int) {
	lines := strings.Split(text, "\n")
	i := 0
	for i < len(lines) {
		trimmed := strings.TrimSpace(lines[i])
		if trimmed != "" && !strings.HasPrefix(trimmed, "#") {
			break
		}
		i++
	}
	return strings.Join(lines[i:], "\n"), i
}

// stripAutonomyLine removes every `autonomy: <tier>` line, returning the
// remainder, the last tier value seen, and how many lines matched.
func stripAutonomyLine(text string) (string, string, int) {
	lines := strings.Split(text, "\n")
	kept := make([]string, 0, len(lines))
	tier := ""
	n := 0
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "autonomy:") {
			tier = strings.TrimSpace(strings.TrimPrefix(trimmed, "autonomy:"))
			n++
			continue
		}
		kept = append(kept, line)
	}
	return strings.Join(kept, "\n"), tier, n
}

// presetText returns a preset's shipped bytes, failing the test on a read
// error.
func presetText(t *testing.T, p Preset) []byte {
	t.Helper()
	data, err := PresetBytes(p)
	if err != nil {
		t.Fatalf("PresetBytes(%q): %v", p, err)
	}
	return data
}

// TestPresetApprovalsGateHandleFree is the E39.2 / #1707 done-means: every
// preset's feature_change approval gates ship the forge-neutral approvals
// predicate (Count==1, Not==[author,agent]) and NONE carries the legacy
// approvers form, the @your-github-handle placeholder, or a top-level
// roles map. A comment-only / no-op preset touch that left the handle or
// the approvers gate in place fails here even though the scope path was
// touched.
func TestPresetApprovalsGateHandleFree(t *testing.T) {
	for _, p := range allPresets {
		p := p
		t.Run(string(p), func(t *testing.T) {
			// Byte-level: the placeholder handle and the top-level roles
			// key must be gone from the shipped document.
			data, err := PresetBytes(p)
			if err != nil {
				t.Fatalf("PresetBytes(%q): %v", p, err)
			}
			text := string(data)
			if strings.Contains(text, "@your-github-handle") {
				t.Errorf("preset %q still carries the @your-github-handle placeholder:\n%s", p, text)
			}
			for _, line := range strings.Split(text, "\n") {
				if strings.HasPrefix(line, "roles:") {
					t.Errorf("preset %q still declares a top-level roles map:\n%s", p, text)
				}
			}

			// Struct-level: each approval gate carries the forge-neutral
			// approvals predicate and no legacy approvers form.
			wf, ok := parsePreset(t, p).Workflows["feature_change"]
			if !ok {
				t.Fatalf("preset %q has no feature_change workflow", p)
			}
			gates := 0
			for _, st := range wf.Stages {
				for _, g := range st.Gates {
					if g.Type != GateTypeApproval {
						continue
					}
					gates++
					if g.Approvers != nil {
						t.Errorf("preset %q stage %q: approval gate still uses legacy approvers %+v", p, st.ID, g.Approvers)
					}
					a := g.Approvals
					if a == nil {
						t.Fatalf("preset %q stage %q: approval gate missing approvals block", p, st.ID)
					}
					if a.Count == nil || *a.Count != 1 {
						t.Errorf("preset %q stage %q: approvals.count = %v, want 1", p, st.ID, a.Count)
					}
					if len(a.Not) != 2 || a.Not[0] != "author" || a.Not[1] != "agent" {
						t.Errorf("preset %q stage %q: approvals.not = %v, want [author agent]", p, st.ID, a.Not)
					}
				}
			}
			if gates == 0 {
				t.Errorf("preset %q has no approval gates to assert on", p)
			}
		})
	}
}

// --- control-surface starters (E53.6 capstone) -------------------------------

// uncommentStarters returns the preset text with the two COMMENTED
// control-surface starter blocks (the lines strictly between each
// `fishhawk:starter-begin`/`-end` sentinel) uncommented in place, so the
// shipped starter SHAPES can be run through the real validator. The
// sentinel lines themselves stay comments. This is what proves a shipped
// starter is not merely present but still parses and validates: comments
// are discarded at parse, so nothing else would catch a starter that
// drifted out of schema.
func uncommentStarters(text string) string {
	lines := strings.Split(text, "\n")
	out := make([]string, 0, len(lines))
	in := false
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		switch {
		case strings.HasPrefix(trimmed, "# fishhawk:starter-begin"):
			in = true
			out = append(out, line)
		case strings.HasPrefix(trimmed, "# fishhawk:starter-end"):
			in = false
			out = append(out, line)
		case in:
			out = append(out, strings.Replace(line, "# ", "", 1))
		default:
			out = append(out, line)
		}
	}
	return strings.Join(out, "\n")
}

// TestShippedPresetsCarryControlSurfaceStarters is the done-means for the
// E53.6 capstone's preset half, mirrored on the backend embed so BOTH
// mirror sets are held to it. One assertion per named failure mode: every
// shipped preset carries the two COMMENTED control-surface starter blocks
// (applies_to and escalations); the starters stay INERT (never a live
// key); and — per the operator's binding condition that a commented
// starter nothing validates is unacceptable — the starter shapes still
// parse and validate (ParseBytes: schema + semantic, incl. the escalation
// only-raise check) when uncommented, so a starter drifted out of schema
// fails the build here rather than the first operator who uncomments it.
// A comment-only / no-op touch of a preset path fails (a) or (b).
func TestShippedPresetsCarryControlSurfaceStarters(t *testing.T) {
	for _, p := range allPresets {
		p := p
		t.Run(string(p), func(t *testing.T) {
			text := string(presetText(t, p))

			// (a) the commented applies_to starter is present.
			if !strings.Contains(text, "# fishhawk:starter-begin applies_to") {
				t.Errorf("preset %q is missing the commented applies_to starter block:\n%s", p, text)
			}
			// (b) the commented escalations starter is present.
			if !strings.Contains(text, "# fishhawk:starter-begin escalations") {
				t.Errorf("preset %q is missing the commented escalations starter block:\n%s", p, text)
			}
			// (c) the starters stay INERT: no line, trimmed of leading
			// whitespace, begins with a LIVE applies_to:/escalations: key.
			// A future edit that uncomments one fails here rather than
			// silently shipping a routing/escalation policy into every
			// scaffolded repo.
			for i, line := range strings.Split(text, "\n") {
				tl := strings.TrimSpace(line)
				if strings.HasPrefix(tl, "applies_to:") || strings.HasPrefix(tl, "escalations:") {
					t.Errorf("preset %q line %d ships a LIVE control-surface key, want it commented: %s", p, i+1, line)
				}
			}
			// (d) the shipped preset validates, AND the starter shapes
			// validate once uncommented (CONDITION 2(a)): comments are
			// discarded at parse, so this is the only machine check of the
			// starter shape.
			if _, err := ParseBytes([]byte(text)); err != nil {
				t.Fatalf("shipped preset %q does not validate: %v", p, err)
			}
			uc := uncommentStarters(text)
			if !strings.Contains(uc, "\n    applies_to:\n") || !strings.Contains(uc, "\n    escalations:\n") {
				t.Fatalf("preset %q uncomment did not surface both starter keys — the sentinels did not match:\n%s", p, uc)
			}
			if _, err := ParseBytes([]byte(uc)); err != nil {
				t.Fatalf("preset %q starter shapes do not validate once uncommented: %v\n%s", p, err, uc)
			}
		})
	}
}

// TestPresetBytesUnknown covers the unknown-preset error branch.
func TestPresetBytesUnknown(t *testing.T) {
	if _, err := PresetBytes(Preset("bogus")); err == nil {
		t.Fatal("PresetBytes with unknown preset must error")
	}
}

// --- the forge-neutral self-modification guard --------------------------------

// forbiddenPathsOfImplement returns the implement stage's resolved
// forbidden_paths list from a parsed preset. It fails LOUDLY — not
// vacuously — when the implement stage, its constraints, or the
// forbidden_paths kind is absent, so a preset that dropped the guard
// entirely cannot silently satisfy an emptiness-tolerant assertion.
//
// workflow-v2 spells constraints as one OBJECT which the parser
// normalizes into a one-element []Constraint (see the Constraint doc in
// spec.go), so this walks the list rather than assuming index 0.
func forbiddenPathsOfImplement(t *testing.T, p Preset) []string {
	t.Helper()
	s := parsePreset(t, p)
	wf, ok := s.Workflows["feature_change"]
	if !ok {
		t.Fatalf("preset %q has no feature_change workflow", p)
	}
	for _, st := range wf.Stages {
		if st.ID != "implement" {
			continue
		}
		if len(st.Constraints) == 0 {
			t.Fatalf("preset %q implement stage declares no constraints", p)
		}
		for _, c := range st.Constraints {
			if len(c.ForbiddenPaths) > 0 {
				return c.ForbiddenPaths
			}
		}
		t.Fatalf("preset %q implement stage declares no forbidden_paths constraint: %+v", p, st.Constraints)
	}
	t.Fatalf("preset %q has no implement stage", p)
	return nil
}

// TestPresetsForbidBothForgeCIEntryPoints is the done-means assertion on
// the PARSED constraint value (never a text substring, which would pass
// on a commented-out entry): the shipped implement stage may not rewrite
// its own executor configuration on EITHER forge. One assertion per
// named failure mode, including the additive mode — the pre-existing
// governance and legal entries must survive, so the change cannot have
// rewritten the list instead of extending it.
func TestPresetsForbidBothForgeCIEntryPoints(t *testing.T) {
	for _, tc := range []struct {
		mode string
		want string
	}{
		{"github entry point", ".github/workflows/**"},
		{"gitlab entry point", ".gitlab-ci.yml"},
		{"gitlab include fragments", ".gitlab/ci/**"},
		{"gitlab agent ci_access config", ".gitlab/agents/**"},
		{"additive: governance survived", ".fishhawk/**"},
		{"additive: LICENSE survived", "LICENSE"},
		{"additive: NOTICE survived", "NOTICE"},
	} {
		tc := tc
		for _, p := range allPresets {
			p := p
			t.Run(string(p)+"/"+tc.mode, func(t *testing.T) {
				got := forbiddenPathsOfImplement(t, p)
				for _, entry := range got {
					if entry == tc.want {
						return
					}
				}
				t.Errorf("preset %q implement forbidden_paths is missing %q (%s): %v", p, tc.want, tc.mode, got)
			})
		}
	}
}

// forgeGuardMustBlock / forgeGuardMustNotBlock are the shipped-behavior
// table: which repository paths the implement stage may not write, and
// — the over-broad-pattern failure mode — which it must stay free to.
// The MUST-BLOCK rows now cover THREE of the four rationale families
// (docs/spec/workflow-preset.md): self-execution (the .github / .gitlab-ci
// / .gitlab/ci / .gitlab/agents rows), self-governance (.fishhawk) and
// legal (LICENSE / NOTICE).
//
// The two .gitlab/agents rows are the E45.34 (#2962) addition: GitLab reads
// the agent-for-Kubernetes config from .gitlab/agents/<name>/config.yaml,
// whose `ci_access` block grants the stage's own pipeline jobs cluster
// access, so it is a capability-granting file on the agent's execution
// surface. .gitlab/agents/deploy/config.yaml pins the exact documented path
// shape; .gitlab/agents/nested/deep/config.yaml pins that the `**` suffix
// reaches arbitrary depth (mirroring the .gitlab/ci/nested row).
//
// The two .gitlab/ template rows and the nested sub/project row are
// load-bearing, not decorative, and stay load-bearing AFTER the .gitlab/agents
// addition. The templates pin the ratified `.gitlab/ci/**`-not-`.gitlab/**`
// breadth decision — the agent rows added a NARROW second .gitlab/ prefix,
// not a widening to .gitlab/**, so the template rows must still stay
// UNBLOCKED or a future widening to .gitlab/** would slip in silently. The
// nested row pins the root-anchoring claim the guard's residual analysis
// rests on: a bare-filename pattern must NOT match a nested path of the same
// name, because GitLab reads the pipeline definition from the repository root
// and a nested file of that name executes nothing.
var (
	forgeGuardMustBlock = []string{
		".github/workflows/fishhawk.yml",
		".gitlab-ci.yml",
		".gitlab/ci/build.yml",
		".gitlab/ci/nested/fragment.yml",
		".gitlab/agents/deploy/config.yaml",
		".gitlab/agents/nested/deep/config.yaml",
		".fishhawk/workflows.yaml",
		"LICENSE",
		"NOTICE",
	}
	forgeGuardMustNotBlock = []string{
		"backend/internal/spec/preset.go",
		"docs/deploy/gitlab.md",
		"site/src/content/docs/concepts/constraint.md",
		".gitlab/issue_templates/bug.md",
		".gitlab/merge_request_templates/default.md",
		"sub/project/.gitlab-ci.yml",
	}
)

// matchesAnyForbidden evaluates a path against a forbidden_paths list
// with doublestar.PathMatch — the IDENTICAL call the runner's
// enforcement makes in runner/internal/constraint/constraint.go
// (checkForbidden), so the constraint VALUE declared in the spec
// document is checked through the SAME matcher production enforces
// with, rather than each side being assumed correct alone.
func matchesAnyForbidden(t *testing.T, pats []string, path string) (bool, string) {
	t.Helper()
	for _, pat := range pats {
		ok, err := doublestar.PathMatch(pat, path)
		if err != nil {
			t.Fatalf("doublestar.PathMatch(%q, %q): %v", pat, path, err)
		}
		if ok {
			return true, pat
		}
	}
	return false, ""
}

// TestPresetForbiddenPathsBlockBothForgeEntryPoints asserts the SHIPPED
// OBSERVABLE BEHAVIOR of the guard — which files the implement stage is
// actually refused — rather than the presence of an edited line. A
// comment-only or no-op preset touch that satisfied the scope
// completeness gate fails here.
func TestPresetForbiddenPathsBlockBothForgeEntryPoints(t *testing.T) {
	for _, p := range allPresets {
		p := p
		t.Run(string(p), func(t *testing.T) {
			pats := forbiddenPathsOfImplement(t, p)
			for _, path := range forgeGuardMustBlock {
				if ok, _ := matchesAnyForbidden(t, pats, path); !ok {
					t.Errorf("MUST-BLOCK %q is not matched by any forbidden_paths pattern of preset %q: %v", path, p, pats)
				}
			}
			for _, path := range forgeGuardMustNotBlock {
				if ok, pat := matchesAnyForbidden(t, pats, path); ok {
					t.Errorf("MUST-NOT-BLOCK %q is blocked by pattern %q of preset %q — the guard is over-broad", path, pat, p)
				}
			}
		})
	}
}
