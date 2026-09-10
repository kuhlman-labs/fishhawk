package workmgmt

import (
	"fmt"
	"strconv"
	"strings"
)

// ParseIssueRef is the SINGLE shared parser for the three issue-ref forms
// every operator-facing surface advertises: `N`, `#N`, `issue:N` (#3314).
// Before this it was duplicated as two DIVERGENT parsers —
// workmgmt/github.parseIssueRef accepted `N`/`#N` but not `issue:N`, while
// campaign.parseItemRef accepted `N`/`issue:N` but not `#N` — so the MCP
// remedy string, the ErrInvalidItemRef doc, and the docs/api refusal text
// each named a form that failed to parse on one of the two paths. An
// operator following the remedy could retry with the OTHER named form and
// draw the SAME refusal.
//
// Behavior, in this exact order: TrimSpace; strip AT MOST ONE leading
// "issue:" prefix; TrimSpace; strip AT MOST ONE leading "#"; TrimSpace;
// strconv.Atoi; reject n <= 0. Because each prefix is stripped at most once,
// "issue:issue:101" and "##101" are REJECTED rather than silently resolving
// to 101 — "issue:#101" parses as a harmless consequence of the ordering
// (issue: stripped first, then #), not a deliberate fourth form.
//
// ParseIssueRef is the ONLY normalization any caller may apply to a raw ref.
// A caller that pre-strips "issue:" before calling this function would
// double-strip "issue:issue:101" down to "101" on ITS path while a caller
// that delegates directly rejects it — recreating, on the caller's own path,
// the exact cross-path divergence this function exists to eliminate. Pass
// the operator's raw ref straight through.
//
// The guarantee this establishes is ONE normalization per parse WITHIN THE
// SERVER — every backend caller (workmgmt/github, the campaign package)
// reaches this exact function and none of them re-strips. It does NOT extend
// across the wire: a CLIENT that pre-strips "issue:" before the ref ever
// reaches this function (the fishhawk CLI's campaign command does, ahead of
// the HTTP call) is a distinct hop this guarantee cannot see, because
// ParseIssueRef only ever receives whatever the client already sent. So a
// doubled-prefix ref can succeed via that ONE client — "issue:issue:99"
// arrives here as "issue:99" and resolves — while the identical raw value
// posted directly to the API is rejected. That is a known, narrow exception
// on the CLI's own pre-send strip, not a defect in this function's
// single-normalization property.
func ParseIssueRef(ref string) (int, error) {
	s := strings.TrimSpace(ref)
	s = strings.TrimPrefix(s, "issue:")
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(s, "#")
	s = strings.TrimSpace(s)
	n, err := strconv.Atoi(s)
	if err != nil {
		return 0, fmt.Errorf("not a numeric issue reference")
	}
	if n <= 0 {
		return 0, fmt.Errorf("issue number must be > 0")
	}
	return n, nil
}
