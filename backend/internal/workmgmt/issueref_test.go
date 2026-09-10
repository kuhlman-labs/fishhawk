package workmgmt

import "testing"

// TestParseIssueRef tables every accepted form and every rejection class,
// asserting both the returned number and, for the two rejection modes, the
// EXACT error text downstream callers depend on: campaigns_test.go
// constructs "not a numeric issue reference" verbatim as the shape the real
// resolver returns, and the github provider_test counterfactual comments
// quote both messages.
//
// COUNTERFACTUAL (plan step 12-i) — OBSERVED, not reasoned: deleting the
// `if n <= 0 { ... }` guard below and running `go test -run
// 'TestParseIssueRef|TestFilterToSubset_NonPositiveItemRef'
// ./internal/workmgmt/... ./internal/campaign/...` produced:
//
//	--- FAIL: TestParseIssueRef/hash-zero
//	    issueref_test.go:46: ParseIssueRef("#0") = 0, nil; want error "issue number must be > 0"
//	--- FAIL: TestParseIssueRef/zero
//	    issueref_test.go:46: ParseIssueRef("0") = 0, nil; want error "issue number must be > 0"
//	--- FAIL: TestParseIssueRef/issue-zero
//	    issueref_test.go:46: ParseIssueRef("issue:0") = 0, nil; want error "issue number must be > 0"
//	--- FAIL: TestParseIssueRef/negative
//	    issueref_test.go:46: ParseIssueRef("-5") = -5, nil; want error "issue number must be > 0"
//	--- FAIL: TestParseIssueRef (workmgmt/github)
//	    provider_test.go:1771: parseIssueRef("#0") want error
//	    provider_test.go:1771: parseIssueRef("0") want error
//	    provider_test.go:1771: parseIssueRef("issue:0") want error
//	--- FAIL: TestFilterToSubset_NonPositiveItemRef_ReturnsErrInvalidItemRef (campaign)
//	    subset_test.go:369: FilterToSubset("0") err = campaign: subset item is not a child of the epic: 0, want ErrInvalidItemRef
//	    subset_test.go:372: FilterToSubset("0") err = campaign: subset item is not a child of the epic: 0, want NOT ErrItemNotChild
//	    subset_test.go:369: FilterToSubset("-5") err = campaign: subset item is not a child of the epic: -5, want ErrInvalidItemRef
//	    subset_test.go:372: FilterToSubset("-5") err = campaign: subset item is not a child of the epic: -5, want NOT ErrItemNotChild
//
// Restored byte-identically; all three green again.
func TestParseIssueRef(t *testing.T) {
	for _, tc := range []struct {
		name    string
		in      string
		want    int
		wantErr string
	}{
		{name: "bare", in: "101", want: 101},
		{name: "hash", in: "#101", want: 101},
		{name: "issue-prefix", in: "issue:101", want: 101},
		{name: "hash-padded", in: " #42 ", want: 42},
		{name: "issue-prefix-padded-and-spaced", in: " issue: 42 ", want: 42},
		{name: "issue-then-hash", in: "issue:#101", want: 101},
		{name: "empty", in: "", wantErr: "not a numeric issue reference"},
		{name: "non-numeric", in: "abc", wantErr: "not a numeric issue reference"},
		{name: "non-numeric-word", in: "not-a-ref", wantErr: "not a numeric issue reference"},
		{name: "hash-only", in: "#", wantErr: "not a numeric issue reference"},
		{name: "issue-prefix-only", in: "issue:", wantErr: "not a numeric issue reference"},
		{name: "hash-zero", in: "#0", wantErr: "issue number must be > 0"},
		{name: "zero", in: "0", wantErr: "issue number must be > 0"},
		{name: "issue-zero", in: "issue:0", wantErr: "issue number must be > 0"},
		{name: "negative", in: "-5", wantErr: "issue number must be > 0"},
		{name: "trailing-garbage", in: "10x", wantErr: "not a numeric issue reference"},
		{name: "two-numbers", in: "1 2", wantErr: "not a numeric issue reference"},
		// Single-strip pins (#3314 operator constraint 1): each prefix strips
		// AT MOST ONCE, so a doubled prefix is REJECTED rather than silently
		// resolving — the property the pre-strip removal at the three github
		// call sites depends on.
		{name: "double-issue-prefix", in: "issue:issue:101", wantErr: "not a numeric issue reference"},
		{name: "double-hash", in: "##101", wantErr: "not a numeric issue reference"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ParseIssueRef(tc.in)
			if tc.wantErr != "" {
				if err == nil {
					t.Fatalf("ParseIssueRef(%q) = %d, nil; want error %q", tc.in, got, tc.wantErr)
				}
				if err.Error() != tc.wantErr {
					t.Fatalf("ParseIssueRef(%q) err = %q, want %q", tc.in, err.Error(), tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("ParseIssueRef(%q) = %v, want %d", tc.in, err, tc.want)
			}
			if got != tc.want {
				t.Fatalf("ParseIssueRef(%q) = %d, want %d", tc.in, got, tc.want)
			}
		})
	}
}
