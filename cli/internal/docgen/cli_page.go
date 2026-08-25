package docgen

import (
	"regexp"
	"strings"

	"github.com/kuhlman-labs/fishhawk/cli/internal/cmdinfo"
)

// VocabularySubstitutions maps a BRAND_FOUNDATIONS §5 banned prefix to a
// site-safe replacement, applied at render time to any canonical
// description so the generated pages pass scripts/check-site-voice. It
// lives in production code — reviewable and tested, never an ad-hoc
// rewrite — exactly like DeliberateExclusions. The canonical sources are
// currently voice-clean, so this fires on nothing today; the mechanism
// exists so a future banned term in a schema description cannot red-line
// the site-voice gate through the generated region.
var VocabularySubstitutions = map[string]string{
	"revolutionary":    "new",
	"game-changing":    "significant",
	"next-generation":  "new",
	"industry-leading": "strong",
	"world-class":      "strong",
	"ai-powered":       "agent-driven",
	"frictionless":     "low-friction",
	"seamless":         "smooth",
	"effortless":       "straightforward",
	"empower":          "let",
}

// vocabRE matches a word beginning with any banned prefix (leading
// word-boundary anchored, case-insensitive), mirroring check-site-voice.
var vocabRE = func() *regexp.Regexp {
	keys := make([]string, 0, len(VocabularySubstitutions))
	for k := range VocabularySubstitutions {
		keys = append(keys, regexp.QuoteMeta(k))
	}
	return regexp.MustCompile(`(?i)\b(` + strings.Join(keys, "|") + `)[a-z-]*`)
}()

// applyVocabulary substitutes any banned prefix in s and returns the
// substituted text plus the list of banned prefixes it replaced.
func applyVocabulary(s string) (string, []string) {
	var fired []string
	out := vocabRE.ReplaceAllStringFunc(s, func(m string) string {
		lower := strings.ToLower(m)
		for prefix, repl := range VocabularySubstitutions {
			if strings.HasPrefix(lower, prefix) {
				fired = append(fired, prefix)
				return repl
			}
		}
		return m
	})
	return out, fired
}

// RenderCLI renders the CLI reference from the cmdinfo inventory.
func RenderCLI() string {
	var b strings.Builder
	b.WriteString("| Command | Arguments | Synopsis |\n")
	b.WriteString("|---|---|---|\n")
	for _, c := range cmdinfo.Commands() {
		syn, _ := applyVocabulary(c.Synopsis)
		args := c.Args
		if args == "" {
			args = "—"
		}
		b.WriteString("| " + codeSpan("fishhawk "+c.Key) + " | " + proseCell(args) + " | " +
			proseCell(syn) + " |\n")
	}
	b.WriteString("\n### Flags per command\n\n")
	for _, c := range cmdinfo.Commands() {
		b.WriteString("#### `fishhawk " + c.Key + "`\n\n")
		if len(c.Flags) == 0 {
			b.WriteString("Flags: none.\n\n")
			continue
		}
		coded := make([]string, 0, len(c.Flags))
		for _, f := range c.Flags {
			coded = append(coded, codeSpan("--"+f))
		}
		b.WriteString("Flags: " + strings.Join(coded, ", ") + "\n\n")
	}
	return b.String()
}

func generateCLIPage() (string, error) {
	var b strings.Builder
	b.WriteString(genPreamble())
	b.WriteString("\n")
	b.WriteString(crossLinkBlock())
	b.WriteString("\n")
	b.WriteString("## Commands\n\n")
	b.WriteString("Every command below is rendered from the `cli/internal/cmdinfo` inventory, " +
		"which `TestCLIFlagsMatchExecutableSurface` binds to the live `flag.FlagSet` of each " +
		"command in both directions — so a flag cannot appear here that the binary does not " +
		"register, nor a registered flag be omitted. `fishhawk <command> --help` prints the " +
		"same flags with their defaults.\n\n")
	b.WriteString(RenderCLI())
	return b.String(), nil
}
