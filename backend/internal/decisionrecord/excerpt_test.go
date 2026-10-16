package decisionrecord

import (
	"strings"
	"testing"
)

// TestDecisionExcerpt pins one heading or fence rule per row, on exact output
// bytes (#4489).
func TestDecisionExcerpt(t *testing.T) {
	cases := []struct {
		name   string
		in     string
		want   string
		wantOK bool
	}{
		{
			name: "no Decision heading",
			in:   "# ADR-001\n\n## Context\n\nctx\n\n## Consequences\n\nc\n",
		},
		{
			name:   "a Decision section runs to the next level-2 heading",
			in:     "# ADR-001\n\n## Context\n\nctx\n\n## Decision\n\nDECIDED\n\n## Consequences\n\nCONSEQ\n",
			want:   "## Decision\n\nDECIDED\n\n",
			wantOK: true,
		},
		{
			name:   "a Decision section at EOF without a trailing newline",
			in:     "## Context\n\nctx\n\n## Decision\n\nDECIDED",
			want:   "## Decision\n\nDECIDED",
			wantOK: true,
		},
		{
			name:   "a level-1 heading ends a Decision section",
			in:     "## Decision\n\nDECIDED\n# Appendix\n\nAPPENDIX\n",
			want:   "## Decision\n\nDECIDED\n",
			wantOK: true,
		},
		{
			name:   "an empty level-2 heading ends a Decision section",
			in:     "## Decision\nDECIDED\n##\nAFTER\n",
			want:   "## Decision\nDECIDED\n",
			wantOK: true,
		},
		{
			name:   "a level-3 heading inside a Decision section is kept",
			in:     "## Decision\n\n### Sub\n\nSUB-TEXT\n\n## Consequences\n\nCONSEQ\n",
			want:   "## Decision\n\n### Sub\n\nSUB-TEXT\n\n",
			wantOK: true,
		},
		{
			name: "a level-3 Decision heading alone starts nothing",
			in:   "## Context\n\n### Decision\n\nNOT-A-SECTION\n",
		},
		{
			name: "an indented Decision heading is not recognized",
			in:   "## Context\n\n ## Decision\n\nINDENTED\n",
		},
		{
			name:   "Decisions matches",
			in:     "## Decisions\n\nPLURAL\n## Consequences\n",
			want:   "## Decisions\n\nPLURAL\n",
			wantOK: true,
		},
		{
			name:   "lower-case decision matches",
			in:     "## decision\n\nLOWER\n## Consequences\n",
			want:   "## decision\n\nLOWER\n",
			wantOK: true,
		},
		{
			name:   "a dated Decision heading matches",
			in:     "## Decision (2026-05-21)\n\nDATED\n## Consequences\n",
			want:   "## Decision (2026-05-21)\n\nDATED\n",
			wantOK: true,
		},
		{
			name: "Decisionmaking does not match",
			in:   "## Decisionmaking\n\nPROCESS\n",
		},
		{
			name: "a heading that only ends in Decision does not match",
			in:   "## Recommendation / Decision\n\nRECO\n",
		},
		{
			name:   "two Decision sections concatenate in file order, the section between excluded",
			in:     "## Decision\n\nFIRST\n\n## Consequences\n\nBETWEEN\n\n## Decision addendum (x)\n\nSECOND\n\n## Relations\n\nREL\n",
			want:   "## Decision\n\nFIRST\n\n## Decision addendum (x)\n\nSECOND\n\n",
			wantOK: true,
		},
		{
			name:   "a heading inside a backtick fence does not end the section",
			in:     "## Decision\n\n```yaml\n## Not a heading\n```\n\nAFTER-FENCE\n\n## Consequences\n\nCONSEQ\n",
			want:   "## Decision\n\n```yaml\n## Not a heading\n```\n\nAFTER-FENCE\n\n",
			wantOK: true,
		},
		{
			name:   "a shorter tilde run does not close a longer tilde fence",
			in:     "## Decision\n\n~~~~\n~~~\n## Not a heading\n~~~~\n\nAFTER-FENCE\n## Consequences\n",
			want:   "## Decision\n\n~~~~\n~~~\n## Not a heading\n~~~~\n\nAFTER-FENCE\n",
			wantOK: true,
		},
		{
			name:   "a backtick run does not close a tilde fence",
			in:     "## Decision\n~~~\n```\n## Not a heading\n~~~\nAFTER-FENCE\n## Consequences\n",
			want:   "## Decision\n~~~\n```\n## Not a heading\n~~~\nAFTER-FENCE\n",
			wantOK: true,
		},
		{
			name:   "a closing fence with trailing text does not close it",
			in:     "## Decision\n```\n``` not-a-close\n## Not a heading\n```  \nAFTER-FENCE\n## Consequences\n",
			want:   "## Decision\n```\n``` not-a-close\n## Not a heading\n```  \nAFTER-FENCE\n",
			wantOK: true,
		},
		{
			name:   "an inline backtick span at line start opens no fence",
			in:     "## Decision\n```inline``` span\n## Consequences\nCONSEQ\n",
			want:   "## Decision\n```inline``` span\n",
			wantOK: true,
		},
		{
			name:   "a two-character run opens no fence",
			in:     "## Decision\n``\n~~\n## Consequences\nCONSEQ\n",
			want:   "## Decision\n``\n~~\n",
			wantOK: true,
		},
		{
			name:   "a fence indented four spaces opens nothing",
			in:     "## Decision\n    ```\n## Consequences\nCONSEQ\n",
			want:   "## Decision\n    ```\n",
			wantOK: true,
		},
		{
			name:   "a fence indented three spaces opens one",
			in:     "## Decision\n   ```\n## Not a heading\n   ```\nAFTER-FENCE\n## Consequences\n",
			want:   "## Decision\n   ```\n## Not a heading\n   ```\nAFTER-FENCE\n",
			wantOK: true,
		},
		{
			name:   "a Decision heading inside a fence before the real section starts nothing",
			in:     "## Context\n\n```\n## Decision\nFENCED\n```\n\n## Decision\n\nREAL\n## Consequences\n",
			want:   "## Decision\n\nREAL\n",
			wantOK: true,
		},
		{
			name:   "an unclosed fence runs to EOF",
			in:     "## Decision\n```\n## Not a heading\nTAIL",
			want:   "## Decision\n```\n## Not a heading\nTAIL",
			wantOK: true,
		},
		{
			name:   "CRLF line endings",
			in:     "## Context\r\nctx\r\n## Decision\r\n\r\nDECIDED\r\n## Consequences\r\nCONSEQ\r\n",
			want:   "## Decision\r\n\r\nDECIDED\r\n",
			wantOK: true,
		},
		{
			name: "empty input",
			in:   "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := DecisionExcerpt([]byte(tc.in))
			if ok != tc.wantOK || string(got) != tc.want {
				t.Fatalf("DecisionExcerpt(%q) = %q, %v; want %q, %v", tc.in, got, ok, tc.want, tc.wantOK)
			}
			if !tc.wantOK && got != nil {
				t.Errorf("no excerpt, but returned %q (want nil)", got)
			}
		})
	}
}

// decisionOnlyAnchor and recommendationOnlyProbe are ADR-101 Q4's answer and
// question. Each occurs exactly once in the file: the answer under Decision,
// the question under Recommendation (approval condition 2: the shared phrase
// "holding only the lease credential" occurs in both, so it cannot prove the
// cut).
const (
	adr101Path              = "docs/adr/101-charter-authorization-mode.md"
	decisionOnlyAnchor      = "**Q4: yes.** Agent classification before issuance"
	recommendationOnlyProbe = "Should the charter subject be classified"
)

// TestDecisionExcerpt_ShippedRecords runs the parser over the real ADR-040,
// ADR-097 and ADR-101: each yields an excerpt that starts with its Decision
// heading and carries no Context or Consequences section, and ADR-101's
// carries its Decision-only Q4 answer and not its Recommendation-only Q4
// question.
func TestDecisionExcerpt_ShippedRecords(t *testing.T) {
	for _, path := range []string{
		"docs/adr/040-operator-agent-role-contract.md",
		"docs/adr/097-operator-as-orchestrator.md",
		adr101Path,
	} {
		t.Run(path, func(t *testing.T) {
			raw := readShipped(t, path)
			got, ok := DecisionExcerpt(raw)
			if !ok {
				t.Fatalf("%s has no Decision excerpt: its Decision heading form is not recognized", path)
			}
			ex := string(got)
			if !strings.HasPrefix(ex, "## Decision") {
				t.Errorf("%s excerpt does not start with its Decision heading:\n%s", path, ex)
			}
			for _, line := range strings.Split(ex, "\n") {
				if strings.HasPrefix(line, "## Consequences") || strings.HasPrefix(line, "## Context") || strings.HasPrefix(line, "## Recommendation") {
					t.Errorf("%s excerpt carries the non-Decision heading %q", path, line)
				}
			}
			if len(got) >= len(raw) {
				t.Errorf("%s excerpt is %d bytes of a %d-byte file: nothing was cut", path, len(got), len(raw))
			}
			t.Logf("%s: %d-byte excerpt of a %d-byte file", path, len(got), len(raw))
			if path != adr101Path {
				return
			}
			file := string(raw)
			for _, anchor := range []string{decisionOnlyAnchor, recommendationOnlyProbe} {
				if n := strings.Count(file, anchor); n != 1 {
					t.Fatalf("premise: %q occurs %d times in %s, want exactly once", anchor, n, path)
				}
			}
			if !strings.Contains(ex, decisionOnlyAnchor) {
				t.Errorf("ADR-101 excerpt lacks its Decision-only Q4 answer %q", decisionOnlyAnchor)
			}
			if strings.Contains(ex, recommendationOnlyProbe) {
				t.Errorf("ADR-101 excerpt carries the Recommendation-only Q4 question %q: the Decision cut leaked Recommendation text", recommendationOnlyProbe)
			}
			if !strings.Contains(ex, "holding only the lease credential") {
				t.Errorf("ADR-101 excerpt lacks %q", "holding only the lease credential")
			}
		})
	}
}
