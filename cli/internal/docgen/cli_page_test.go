package docgen

import (
	"regexp"
	"strings"
	"testing"

	"github.com/kuhlman-labs/fishhawk/cli/internal/cmdinfo"
)

// bannedPrefixes mirrors scripts/check-site-voice's BANNED_TERMS (minus
// "trust", which is a human-review item there and here).
var bannedPrefixes = []string{
	"revolutionary", "game-changing", "next-generation", "industry-leading",
	"world-class", "ai-powered", "frictionless", "seamless", "effortless", "empower",
}

// TestCLIPageRendersEveryCommand asserts the CLI reference renders every
// cmdinfo command and every flag it declares — the render side of the
// cmdinfo binding.
func TestCLIPageRendersEveryCommand(t *testing.T) {
	md := RenderCLI()
	for _, c := range cmdinfo.Commands() {
		if !strings.Contains(md, "fishhawk "+c.Key) {
			t.Errorf("CLI page missing command %q", c.Key)
		}
		for _, f := range c.Flags {
			if !strings.Contains(md, "--"+f) {
				t.Errorf("CLI page missing flag --%s of %q", f, c.Key)
			}
		}
	}
}

// TestVocabularySubstitutionsAreTabled asserts the substitution table is
// well-formed (every key is a §5 banned prefix), that applyVocabulary
// actually substitutes and reports what it fired, and that no banned
// prefix survives in any generated reference page.
func TestVocabularySubstitutionsAreTabled(t *testing.T) {
	// Table keys are all banned prefixes.
	bannedSet := map[string]bool{}
	for _, b := range bannedPrefixes {
		bannedSet[b] = true
	}
	for k := range VocabularySubstitutions {
		if !bannedSet[k] {
			t.Errorf("VocabularySubstitutions key %q is not a BRAND_FOUNDATIONS §5 banned prefix", k)
		}
	}

	// applyVocabulary substitutes and reports.
	out, fired := applyVocabulary("a seamless and empowering flow")
	if strings.Contains(strings.ToLower(out), "seamless") || strings.Contains(strings.ToLower(out), "empower") {
		t.Errorf("applyVocabulary left a banned term in %q", out)
	}
	if len(fired) == 0 {
		t.Errorf("applyVocabulary reported no substitutions on banned input")
	}

	// No banned prefix survives in any generated page.
	root := testRepoRoot(t)
	re := regexp.MustCompile(`(?i)\b(` + strings.Join(bannedPrefixes, "|") + `)`)
	for _, id := range PageIDs {
		gen, err := GeneratePageContent(root, id)
		if err != nil {
			t.Fatalf("%s: generate: %v", id, err)
		}
		if m := re.FindString(gen); m != "" {
			t.Errorf("%s: banned vocabulary %q survives in the generated region", id, m)
		}
	}
}
