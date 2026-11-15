package prompt

import (
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// omittedMarkerRe parses the N writeIssueComments prints when the total budget
// drops the oldest comments.
var omittedMarkerRe = regexp.MustCompile(`\[(\d+) older comment\(s\) omitted to fit budget\]`)

// renderedOmittedCount renders comments through the REAL writeIssueComments and
// returns the N in its omission marker, or 0 when no marker is printed.
func renderedOmittedCount(t *testing.T, comments []IssueComment, issueURL string) int {
	t.Helper()
	var b strings.Builder
	writeIssueComments(&b, comments, issueURL)
	m := omittedMarkerRe.FindStringSubmatch(b.String())
	if m == nil {
		return 0
	}
	n, err := strconv.Atoi(m[1])
	if err != nil {
		t.Fatalf("parse omitted count %q: %v", m[1], err)
	}
	return n
}

func budgetComment(author string, size int, i int) IssueComment {
	return IssueComment{
		Author:    author,
		Body:      strings.Repeat("a", size) + "_c" + strconv.Itoa(i),
		CreatedAt: "2026-05-01T00:00:0" + strconv.Itoa(i%10) + "Z",
	}
}

// TestOmittedIssueCommentCount_MatchesRenderer is the #5214 seam guard between
// the plan-prompt renderer and the count fishhawk_start_run warns with: for
// every fixture the exported count must equal the N the renderer itself prints,
// including the bot filter and the per-comment cap. A drift between the two —
// a count computed on raw bodies, or counting bot comments — fails here.
func TestOmittedIssueCommentCount_MatchesRenderer(t *testing.T) {
	const url = "https://github.com/x/y/issues/7"
	many := func(n, size int) []IssueComment {
		var out []IssueComment
		for i := 0; i < n; i++ {
			out = append(out, budgetComment("u", size, i))
		}
		return out
	}

	cases := []struct {
		name string
		in   []IssueComment
		// wantPositive: the fixture is over the total budget, so the count
		// must be > 0 (guards against a helper that returns 0 everywhere).
		wantPositive bool
	}{
		{name: "no_comments", in: nil},
		{name: "under_budget_three_small", in: many(3, 1900)},
		{name: "over_budget_ten_under_cap", in: many(10, 1900), wantPositive: true},
		{name: "over_budget_eight_under_cap", in: many(8, 1900), wantPositive: true},
		{
			// Bot comments are excluded from BOTH: 10 large bot comments plus 3
			// human ones is under budget. Counting the bots would report > 0.
			name: "bot_comments_excluded",
			in: append(func() []IssueComment {
				var bots []IssueComment
				for i := 0; i < 10; i++ {
					bots = append(bots, budgetComment("ci[bot]", 1900, i))
				}
				return bots
			}(), many(3, 1900)...),
		},
		{
			// A 50000-byte comment is counted at its CAPPED, marker-bearing size.
			// Counted raw it alone would blow the budget and drop the two
			// comments before it; capped, all three fit.
			name: "over_per_comment_cap_counted_capped",
			in: []IssueComment{
				budgetComment("u", 1900, 0),
				budgetComment("u", 1900, 1),
				budgetComment("big", 50000, 2),
			},
		},
		{
			// Several over-cap comments whose capped + marker size pushes the
			// total over budget.
			name:         "many_over_cap_over_budget",
			in:           many(8, MaxIssueCommentBytes+10),
			wantPositive: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			for _, u := range []string{url, ""} {
				want := renderedOmittedCount(t, tc.in, u)
				got := OmittedIssueCommentCount(tc.in, u)
				if got != want {
					t.Errorf("issueURL=%q: OmittedIssueCommentCount = %d, renderer printed %d", u, got, want)
				}
				if tc.wantPositive && got == 0 {
					t.Errorf("issueURL=%q: fixture is over the %d-byte total budget but the count is 0", u, MaxTotalIssueCommentBytes)
				}
				if !tc.wantPositive && got != 0 {
					t.Errorf("issueURL=%q: fixture is under budget but the count is %d", u, got)
				}
			}
		})
	}
}

// TestOmittedIssueCommentCount_NoMarkerUnderBudget pins the zero arm against the
// render: under budget the renderer prints no omission marker at all.
func TestOmittedIssueCommentCount_NoMarkerUnderBudget(t *testing.T) {
	in := []IssueComment{budgetComment("u", 100, 0), budgetComment("u", 100, 1)}
	var b strings.Builder
	writeIssueComments(&b, in, "")
	if strings.Contains(b.String(), "older comment(s) omitted") {
		t.Fatalf("under-budget render printed an omission marker:\n%s", b.String())
	}
	if got := OmittedIssueCommentCount(in, ""); got != 0 {
		t.Fatalf("OmittedIssueCommentCount = %d, want 0", got)
	}
}
