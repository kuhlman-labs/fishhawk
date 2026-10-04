package workmgmt

// Tests for the user-report read capability vocabulary (#3771): the
// UserReportReaderFor chokepoint, the NextUserReportCursor rule and the shared
// DedupeUserReportItems helper. Self-contained doubles, like reader_test.go.

import (
	"context"
	"errors"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"
)

// userReportCapableProvider implements the base Provider PLUS
// UserReportReader.
type userReportCapableProvider struct{ name string }

func (f *userReportCapableProvider) Name() string { return f.name }

func (f *userReportCapableProvider) File(_ context.Context, _ ProviderRequest) (*CreatedItem, error) {
	return &CreatedItem{Provider: f.name}, nil
}

func (f *userReportCapableProvider) ListUserReports(_ context.Context, req ListUserReportsRequest) (*UserReportPage, error) {
	return &UserReportPage{Forge: UserReportForgeGitHub, Since: req.Since}, nil
}

func TestUserReportReaderFor_ResolvesImplementingProvider(t *testing.T) {
	Register(&userReportCapableProvider{name: "user-report-capable"})
	r, err := UserReportReaderFor("user-report-capable")
	if err != nil || r == nil {
		t.Fatalf("UserReportReaderFor = (%v, %v), want a reader", r, err)
	}
	since := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	page, err := r.ListUserReports(context.Background(), ListUserReportsRequest{Since: since})
	if err != nil || !page.Since.Equal(since) {
		t.Errorf("dispatch through the resolved reader = (%+v, %v)", page, err)
	}
}

// TestUserReportReaderFor_ProviderWithoutCapabilityIsTypedUnavailable: a
// provider lacking the capability (jira) yields a typed ReasonNotImplemented
// and a NIL reader. Counterfactual: returning the unasserted nil reader with a
// nil error fails the errors.As assertion.
func TestUserReportReaderFor_ProviderWithoutCapabilityIsTypedUnavailable(t *testing.T) {
	Register(&readerFileOnlyProvider{name: "user-report-file-only"})
	r, err := UserReportReaderFor("user-report-file-only")
	if r != nil {
		t.Errorf("reader = %v, want nil alongside the unavailable error", r)
	}
	var ue *UnavailableError
	if !errors.As(err, &ue) {
		t.Fatalf("err = %v (%T), want *UnavailableError", err, err)
	}
	if ue.Reason != ReasonNotImplemented || ue.Capability != UserReportCapability || ue.Provider != "user-report-file-only" {
		t.Errorf("unavailable = %+v, want not_implemented for the user-report capability of the resolved provider", ue)
	}
}

func TestUserReportReaderFor_UnknownProviderKeepsRegistryError(t *testing.T) {
	_, err := UserReportReaderFor("user-report-never-registered")
	var upe *UnknownProviderError
	if !errors.As(err, &upe) {
		t.Fatalf("err = %v (%T), want *UnknownProviderError", err, err)
	}
}

var cursorSince = time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)

func codesOf(degs []UserReportDegradation) []UserReportDegradationCode {
	out := []UserReportDegradationCode{}
	for _, d := range degs {
		out = append(out, d.Code)
	}
	return out
}

func TestNextUserReportCursor_AnchorsToForgeDateMinusOverlap(t *testing.T) {
	forgeDate := cursorSince.Add(time.Hour)
	next, degs := NextUserReportCursor(cursorSince, forgeDate)
	if want := forgeDate.Add(-2 * time.Minute); !next.Equal(want) {
		t.Errorf("next = %v, want forge Date minus the 2-minute overlap %v", next, want)
	}
	if len(degs) != 0 {
		t.Errorf("degradations = %+v, want none for an anchored, untruncated scan", degs)
	}
}

// TestNextUserReportCursor_ZeroForgeDateHoldsSinceAndNamesDegradation: an
// unanchorable scan never advances. A fallback to time.Now (or to the zero
// anchor minus the overlap, which the clamp would raise to since but without
// naming the gap) is caught by the exact value and the named code.
func TestNextUserReportCursor_ZeroForgeDateHoldsSinceAndNamesDegradation(t *testing.T) {
	next, degs := NextUserReportCursor(cursorSince, time.Time{})
	if !next.Equal(cursorSince) {
		t.Errorf("next = %v, want since %v held exactly", next, cursorSince)
	}
	if got := codesOf(degs); !reflect.DeepEqual(got, []UserReportDegradationCode{UserReportCursorAnchorUnavailable}) {
		t.Errorf("codes = %v, want [cursor_anchor_unavailable]", got)
	}
}

// TestNextUserReportCursor_ClampsToSince: a forge Date within the overlap of
// since (or a forge clock behind the stored cursor) never moves the cursor
// backwards. Counterfactual: deleting the clamp returns since - 1m.
func TestNextUserReportCursor_ClampsToSince(t *testing.T) {
	next, _ := NextUserReportCursor(cursorSince, cursorSince.Add(time.Minute))
	if !next.Equal(cursorSince) {
		t.Errorf("next = %v, want clamped to since %v", next, cursorSince)
	}
}

// TestNextUserReportCursor_TruncationTakesEarliestResumeAndNamesScanTruncated:
// NextCursor = min(anchor, every truncated listing's ResumeAt); an exhausted
// listing's ResumeAt is ignored. Counterfactual: ignoring ResumeAt advances to
// the anchor and skips the unread tail.
func TestNextUserReportCursor_TruncationTakesEarliestResumeAndNamesScanTruncated(t *testing.T) {
	forgeDate := cursorSince.Add(time.Hour)
	issuesResume := cursorSince.Add(20 * time.Minute)
	commentsResume := cursorSince.Add(10 * time.Minute)
	next, degs := NextUserReportCursor(cursorSince, forgeDate,
		UserReportListingEnd{Truncated: true, ResumeAt: issuesResume},
		UserReportListingEnd{Truncated: true, ResumeAt: commentsResume},
		UserReportListingEnd{ResumeAt: cursorSince.Add(time.Minute)}, // exhausted: ignored
	)
	if !next.Equal(commentsResume) {
		t.Errorf("next = %v, want the earliest truncated ResumeAt %v", next, commentsResume)
	}
	if len(degs) != 1 || degs[0].Code != UserReportScanTruncated || degs[0].Count != 2 {
		t.Errorf("degradations = %+v, want one scan_truncated with Count 2", degs)
	}

	// A ResumeAt AFTER the anchor leaves the anchor in charge.
	next, _ = NextUserReportCursor(cursorSince, forgeDate, UserReportListingEnd{Truncated: true, ResumeAt: forgeDate})
	if want := forgeDate.Add(-UserReportCursorOverlap); !next.Equal(want) {
		t.Errorf("next = %v, want the anchor %v when it is earlier than ResumeAt", next, want)
	}
}

// TestNextUserReportCursor_TruncatedWithoutResumeHolds: a truncated listing
// that cannot say where it stopped holds the cursor rather than advancing past
// unread items.
func TestNextUserReportCursor_TruncatedWithoutResumeHolds(t *testing.T) {
	next, degs := NextUserReportCursor(cursorSince, cursorSince.Add(time.Hour), UserReportListingEnd{Truncated: true})
	if !next.Equal(cursorSince) {
		t.Errorf("next = %v, want since held", next)
	}
	if got := codesOf(degs); !reflect.DeepEqual(got, []UserReportDegradationCode{UserReportScanTruncated}) {
		t.Errorf("codes = %v, want [scan_truncated]", got)
	}
}

// TestNextUserReportCursor_TruncatedAndUnanchoredNamesBoth: both gaps are
// named and the cursor holds.
func TestNextUserReportCursor_TruncatedAndUnanchoredNamesBoth(t *testing.T) {
	next, degs := NextUserReportCursor(cursorSince, time.Time{}, UserReportListingEnd{Truncated: true, ResumeAt: cursorSince.Add(time.Minute)})
	if !next.Equal(cursorSince) {
		t.Errorf("next = %v, want since held", next)
	}
	want := []UserReportDegradationCode{UserReportScanTruncated, UserReportCursorAnchorUnavailable}
	if got := codesOf(degs); !reflect.DeepEqual(got, want) {
		t.Errorf("codes = %v, want %v", got, want)
	}
}

func item(kind UserReportKind, issue int, comment int64, updated time.Time, body string) UserReportItem {
	return UserReportItem{Kind: kind, IssueNumber: issue, CommentID: comment, UpdatedAt: updated, Body: body}
}

// TestDedupeUserReportItems_KeepsNewestOccurrenceAndSortsDeterministically:
// a boundary re-read and a mid-scan update both collapse to ONE item carrying
// the newest state; an issue and a comment sharing a numeric id stay distinct;
// the order is (UpdatedAt, issue-before-comment, number, comment id).
// Counterfactual: deleting the seen-check returns the duplicates.
func TestDedupeUserReportItems_KeepsNewestOccurrenceAndSortsDeterministically(t *testing.T) {
	t0 := cursorSince
	in := []UserReportItem{
		item(UserReportKindIssue, 7, 0, t0.Add(2*time.Second), "old"),
		item(UserReportKindComment, 3, 7, t0.Add(2*time.Second), "comment 7"), // same numeric id as issue 7: distinct
		item(UserReportKindIssue, 5, 0, t0.Add(time.Second), "five"),
		item(UserReportKindIssue, 7, 0, t0.Add(2*time.Second), "boundary re-read"), // tie: first kept
		item(UserReportKindIssue, 7, 0, t0.Add(9*time.Second), "updated mid-scan"), // newer: replaces
		item(UserReportKindComment, 3, 6, t0.Add(2*time.Second), "comment 6"),
		item(UserReportKindIssue, 5, 0, t0, "older shifted copy"), // older: dropped
	}
	inCopy := append([]UserReportItem(nil), in...)
	got := DedupeUserReportItems(in)
	want := []UserReportItem{
		item(UserReportKindIssue, 5, 0, t0.Add(time.Second), "five"),
		item(UserReportKindComment, 3, 6, t0.Add(2*time.Second), "comment 6"),
		item(UserReportKindComment, 3, 7, t0.Add(2*time.Second), "comment 7"),
		item(UserReportKindIssue, 7, 0, t0.Add(9*time.Second), "updated mid-scan"),
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("dedupe =\n %+v\nwant\n %+v", got, want)
	}
	if !reflect.DeepEqual(in, inCopy) {
		t.Errorf("input mutated")
	}

	// Equal updated_at: issue before comment, then number.
	tie := DedupeUserReportItems([]UserReportItem{
		item(UserReportKindComment, 1, 1, t0, ""),
		item(UserReportKindIssue, 9, 0, t0, ""),
		item(UserReportKindIssue, 2, 0, t0, ""),
	})
	if tie[0].IssueNumber != 2 || tie[1].IssueNumber != 9 || tie[2].Kind != UserReportKindComment {
		t.Errorf("tie order = %+v, want issue #2, issue #9, comment", tie)
	}
}

func TestUserReportDegradationCodes_ClosedSortedSet(t *testing.T) {
	codes := UserReportDegradationCodes()
	if len(codes) != 8 {
		t.Fatalf("codes = %v, want the 8-code closed set", codes)
	}
	seen := map[UserReportDegradationCode]bool{}
	for i, c := range codes {
		if seen[c] || (i > 0 && codes[i-1] >= c) {
			t.Errorf("codes = %v, want sorted and unique", codes)
		}
		seen[c] = true
	}
	for _, c := range []UserReportDegradationCode{UserReportScanTruncated, UserReportConfidentialExcluded} {
		if !seen[c] {
			t.Errorf("code %q missing from the page set", c)
		}
	}
}

// TestBoardCapabilityMatrixDocumentsEveryUserReportDegradationCode: every
// page-level code appears, backticked, in docs/board-capability-matrix.md's
// user-report vocabulary, so a new code cannot ship undocumented.
func TestBoardCapabilityMatrixDocumentsEveryUserReportDegradationCode(t *testing.T) {
	raw, err := os.ReadFile(boardCapabilityMatrixPath)
	if err != nil {
		t.Fatalf("read %s: %v", boardCapabilityMatrixPath, err)
	}
	doc := string(raw)
	for _, c := range UserReportDegradationCodes() {
		if !strings.Contains(doc, "`"+string(c)+"`") {
			t.Errorf("%s does not document user-report degradation code %q; add it to § User-report degradation vocabulary", boardCapabilityMatrixPath, c)
		}
	}
}
