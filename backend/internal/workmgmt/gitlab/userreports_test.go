package gitlab

// Tests for the GitLab user-report reader (#3771). Most rows drive a fake API
// implementing the optional userReportLister extension; the keyset row drives
// a REAL gitlabclient.Client against an httptest GitLab.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/kuhlman-labs/fishhawk/backend/internal/gitlabclient"
	"github.com/kuhlman-labs/fishhawk/backend/internal/workmgmt"
)

type memberAnswer struct {
	level  int
	member bool
	err    error
}

// userReportAPI is the base fakeAPI plus the optional activity reads.
type userReportAPI struct {
	*fakeAPI
	urIssues    []gitlabclient.UpdatedIssue
	urMeta      gitlabclient.ListingMeta
	urIssuesErr error
	notes       map[int][]gitlabclient.Note
	notesErr    map[int]error
	members     map[int64]memberAnswer
	memberCalls map[int64]int
	gotAfter    time.Time
}

func (a *userReportAPI) ListIssuesUpdatedAfter(_ context.Context, _ int, after time.Time) ([]gitlabclient.UpdatedIssue, gitlabclient.ListingMeta, error) {
	a.gotAfter = after
	if a.urIssuesErr != nil {
		return nil, gitlabclient.ListingMeta{}, a.urIssuesErr
	}
	return a.urIssues, a.urMeta, nil
}

func (a *userReportAPI) ListIssueNotes(_ context.Context, _ int, iid int) ([]gitlabclient.Note, error) {
	if err := a.notesErr[iid]; err != nil {
		return nil, err
	}
	return a.notes[iid], nil
}

func (a *userReportAPI) GetProjectMemberAccessLevel(_ context.Context, _ int, userID int64) (int, bool, error) {
	a.memberCalls[userID]++
	m, ok := a.members[userID]
	if !ok {
		return 0, false, nil
	}
	return m.level, m.member, m.err
}

var (
	glSince = time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	glDate  = time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)
)

func newGLUserReportAPI() *userReportAPI {
	return &userReportAPI{
		fakeAPI:     &fakeAPI{},
		urMeta:      gitlabclient.ListingMeta{Date: glDate},
		notes:       map[int][]gitlabclient.Note{},
		notesErr:    map[int]error{},
		members:     map[int64]memberAnswer{},
		memberCalls: map[int64]int{},
	}
}

func glIssue(iid int, authorID int64, username string, updated time.Time) gitlabclient.UpdatedIssue {
	return gitlabclient.UpdatedIssue{
		IID: iid, Title: fmt.Sprintf("issue %d", iid), Description: "desc", State: "opened",
		WebURL: fmt.Sprintf("https://gitlab.com/acme/widgets/-/issues/%d", iid), AuthorID: authorID, AuthorUsername: username,
		Upvotes: 3, Downvotes: 1, CreatedAt: updated, UpdatedAt: updated,
	}
}

func glNote(id int64, authorID int64, username string, updated time.Time) gitlabclient.Note {
	return gitlabclient.Note{
		ID: id, Body: "note", CreatedAt: updated.Format(time.RFC3339), UpdatedAt: updated.Format(time.RFC3339),
		Author: username, AuthorID: authorID,
	}
}

func urTarget() workmgmt.Target {
	return workmgmt.Target{Repo: workmgmt.Repo{Owner: "acme", Name: "widgets"}, GitLab: &workmgmt.GitLabConnection{}}
}

func glList(t *testing.T, api API) (*workmgmt.UserReportPage, error) {
	t.Helper()
	return New(api).ListUserReports(context.Background(), workmgmt.ListUserReportsRequest{Target: urTarget(), Since: glSince})
}

func mustGLList(t *testing.T, api API) *workmgmt.UserReportPage {
	t.Helper()
	page, err := glList(t, api)
	if err != nil {
		t.Fatalf("ListUserReports: %v", err)
	}
	return page
}

func glDegradation(page *workmgmt.UserReportPage, code workmgmt.UserReportDegradationCode) (workmgmt.UserReportDegradation, bool) {
	for _, d := range page.Degradations {
		if d.Code == code {
			return d, true
		}
	}
	return workmgmt.UserReportDegradation{}, false
}

func glCodes(page *workmgmt.UserReportPage) []string {
	var out []string
	for _, d := range page.Degradations {
		out = append(out, string(d.Code))
	}
	sort.Strings(out)
	return out
}

func glAssertUnavailable(t *testing.T, err error, want workmgmt.UnavailableReason) {
	t.Helper()
	var ue *workmgmt.UnavailableError
	if !errors.As(err, &ue) {
		t.Fatalf("err = %v (%T), want *workmgmt.UnavailableError", err, err)
	}
	if ue.Reason != want || ue.Provider != ProviderName || ue.Capability != workmgmt.UserReportCapability {
		t.Fatalf("unavailable = %+v, want reason %q for %q/%q", ue, want, ProviderName, workmgmt.UserReportCapability)
	}
}

func itemByComment(page *workmgmt.UserReportPage, id int64) (workmgmt.UserReportItem, bool) {
	for _, it := range page.Items {
		if it.Kind == workmgmt.UserReportKindComment && it.CommentID == id {
			return it, true
		}
	}
	return workmgmt.UserReportItem{}, false
}

func itemByIssue(page *workmgmt.UserReportPage, iid int) (workmgmt.UserReportItem, bool) {
	for _, it := range page.Items {
		if it.Kind == workmgmt.UserReportKindIssue && it.IssueNumber == iid {
			return it, true
		}
	}
	return workmgmt.UserReportItem{}, false
}

// TestListUserReports_GitLabNamesEveryGapDegradation: a clean page (every
// lookup resolved, nothing confidential, anchored, untruncated) names EXACTLY
// the four always-on gap codes, with item counts on the two reaction codes.
// Counterfactual: deleting any one append drops its code from the set.
func TestListUserReports_GitLabNamesEveryGapDegradation(t *testing.T) {
	api := newGLUserReportAPI()
	api.urIssues = []gitlabclient.UpdatedIssue{glIssue(1, 10, "alice", glSince.Add(time.Second))}
	api.notes[1] = []gitlabclient.Note{glNote(100, 10, "alice", glSince.Add(2*time.Second))}
	api.members[10] = memberAnswer{level: 30, member: true}
	page := mustGLList(t, api)
	want := []string{"bot_detection_heuristic", "comment_reactions_unavailable", "comments_via_issue_activity", "reactions_partial"}
	if got := glCodes(page); !reflect.DeepEqual(got, want) {
		t.Errorf("codes = %v, want exactly %v", got, want)
	}
	for _, d := range page.Degradations {
		if d.Detail == "" {
			t.Errorf("degradation %q carries no operator-facing Detail", d.Code)
		}
	}
	if d, _ := glDegradation(page, workmgmt.UserReportReactionsPartial); d.Count != 1 {
		t.Errorf("reactions_partial Count = %d, want 1 issue", d.Count)
	}
	if d, _ := glDegradation(page, workmgmt.UserReportCommentReactionsUnavailable); d.Count != 1 {
		t.Errorf("comment_reactions_unavailable Count = %d, want 1 note", d.Count)
	}
	is, _ := itemByIssue(page, 1)
	if is.Reactions != (workmgmt.ReactionCounts{Total: 4, PlusOne: 3, MinusOne: 1}) {
		t.Errorf("issue reactions = %+v, want upvotes/downvotes mapped, unresolved", is.Reactions)
	}
	if page.Forge != workmgmt.UserReportForgeGitLab || !page.NextCursor.Equal(glDate.Add(-workmgmt.UserReportCursorOverlap)) || !api.gotAfter.Equal(glSince) {
		t.Errorf("page = %+v (after %v), want gitlab forge, Date-anchored cursor, since forwarded", page, api.gotAfter)
	}
	note, _ := itemByComment(page, 100)
	if note.URL != "https://gitlab.com/acme/widgets/-/issues/1#note_100" || note.IssueNumber != 1 {
		t.Errorf("note item = %+v", note)
	}
}

// TestListUserReports_GitLabNotesFilteredBySince: an updated issue's OLD note
// is excluded; a note with no updated_at falls back to created_at.
// Counterfactual: deleting the since filter returns the old note.
func TestListUserReports_GitLabNotesFilteredBySince(t *testing.T) {
	api := newGLUserReportAPI()
	api.urIssues = []gitlabclient.UpdatedIssue{glIssue(1, 10, "alice", glSince.Add(time.Hour))}
	fresh := glNote(102, 10, "alice", glSince.Add(time.Minute))
	fresh.UpdatedAt = ""
	api.notes[1] = []gitlabclient.Note{glNote(101, 10, "alice", glSince.Add(-time.Hour)), fresh}
	page := mustGLList(t, api)
	if _, ok := itemByComment(page, 101); ok {
		t.Errorf("note 101 (updated before since) returned")
	}
	if it, ok := itemByComment(page, 102); !ok || !it.UpdatedAt.Equal(glSince.Add(time.Minute)) {
		t.Errorf("note 102 = %+v (present %v), want kept with UpdatedAt from created_at", it, ok)
	}
}

func TestListUserReports_GitLabMalformedNoteTimestampErrors(t *testing.T) {
	for name, mutate := range map[string]func(*gitlabclient.Note){
		"created_at": func(n *gitlabclient.Note) { n.CreatedAt = "yesterday" },
		"updated_at": func(n *gitlabclient.Note) { n.UpdatedAt = "yesterday" },
	} {
		t.Run(name, func(t *testing.T) {
			api := newGLUserReportAPI()
			api.urIssues = []gitlabclient.UpdatedIssue{glIssue(1, 10, "alice", glSince.Add(time.Hour))}
			n := glNote(101, 10, "alice", glSince.Add(time.Minute))
			mutate(&n)
			api.notes[1] = []gitlabclient.Note{n}
			if page, err := glList(t, api); page != nil || err == nil {
				t.Errorf("= (%+v, %v), want nil page and an error", page, err)
			}
		})
	}
}

// TestListUserReports_GitLabAccessLevelMapsInternal: Developer (30) and above
// are internal, Reporter (20) is not, a 404 non-member is not and is RESOLVED
// (no association_unresolved).
func TestListUserReports_GitLabAccessLevelMapsInternal(t *testing.T) {
	api := newGLUserReportAPI()
	api.urIssues = []gitlabclient.UpdatedIssue{
		glIssue(1, 30, "dev", glSince.Add(time.Second)),
		glIssue(2, 40, "maintainer", glSince.Add(2*time.Second)),
		glIssue(3, 20, "reporter", glSince.Add(3*time.Second)),
		glIssue(4, 99, "outsider", glSince.Add(4*time.Second)),
	}
	api.members[30] = memberAnswer{level: 30, member: true}
	api.members[40] = memberAnswer{level: 40, member: true}
	api.members[20] = memberAnswer{level: 20, member: true}
	page := mustGLList(t, api)
	want := map[int]workmgmt.ReportAuthor{
		1: {Login: "dev", Association: "access_level:30", AssociationResolved: true, Internal: true},
		2: {Login: "maintainer", Association: "access_level:40", AssociationResolved: true, Internal: true},
		3: {Login: "reporter", Association: "access_level:20", AssociationResolved: true},
		4: {Login: "outsider", Association: "non_member", AssociationResolved: true},
	}
	for iid, w := range want {
		it, _ := itemByIssue(page, iid)
		if it.Author != w {
			t.Errorf("issue #%d author = %+v, want %+v", iid, it.Author, w)
		}
	}
	if _, ok := glDegradation(page, workmgmt.UserReportAssociationUnresolved); ok {
		t.Errorf("association_unresolved named, want none (a 404 is a resolved non-member)")
	}
}

// TestListUserReports_GitLabMemberLookupErrorDegradesToExternal: a failed
// lookup leaves the author NOT internal, unresolved, and named. An absent
// author id is unresolved without a lookup. Counterfactual: setting Internal
// on the error branch reddens the Internal assertion.
func TestListUserReports_GitLabMemberLookupErrorDegradesToExternal(t *testing.T) {
	api := newGLUserReportAPI()
	api.urIssues = []gitlabclient.UpdatedIssue{
		glIssue(1, 10, "maintainer", glSince.Add(time.Second)),
		glIssue(2, 0, "ghost", glSince.Add(2*time.Second)),
	}
	api.members[10] = memberAnswer{level: 40, member: true, err: &gitlabclient.APIError{Op: "get project member", StatusCode: 403}}
	page := mustGLList(t, api)
	for _, iid := range []int{1, 2} {
		it, _ := itemByIssue(page, iid)
		if it.Author.Internal || it.Author.AssociationResolved || it.Author.Association != "" || it.Author.Bot {
			t.Errorf("issue #%d author = %+v, want unresolved, not internal, not bot", iid, it.Author)
		}
	}
	if d, ok := glDegradation(page, workmgmt.UserReportAssociationUnresolved); !ok || d.Count != 2 {
		t.Errorf("degradations = %+v, want association_unresolved Count 2", page.Degradations)
	}
	if api.memberCalls[0] != 0 {
		t.Errorf("member lookup made for an absent author id")
	}
}

// TestListUserReports_GitLabSystemNoteAndAccessTokenBotAreBots: a system note
// is a bot via its system FLAG (an ordinary username); an access-token-bot
// username is a bot when the member lookup corroborates it.
func TestListUserReports_GitLabSystemNoteAndAccessTokenBotAreBots(t *testing.T) {
	api := newGLUserReportAPI()
	api.urIssues = []gitlabclient.UpdatedIssue{glIssue(1, 50, "project_42_bot_abc", glSince.Add(time.Second))}
	system := glNote(201, 10, "alice", glSince.Add(2*time.Second))
	system.System = true
	api.notes[1] = []gitlabclient.Note{system, glNote(202, 10, "alice", glSince.Add(3*time.Second))}
	api.members[50] = memberAnswer{level: 40, member: true}
	api.members[10] = memberAnswer{level: 30, member: true}
	page := mustGLList(t, api)
	if is, _ := itemByIssue(page, 1); !is.Author.Bot {
		t.Errorf("member access-token bot author = %+v, want Bot", is.Author)
	}
	if n, _ := itemByComment(page, 201); !n.Author.Bot {
		t.Errorf("system note author = %+v, want Bot via the system flag", n.Author)
	}
	if n, _ := itemByComment(page, 202); n.Author.Bot {
		t.Errorf("ordinary note by the same user = %+v, want not Bot", n.Author)
	}
}

// TestListUserReports_GitLabBotUsernameNeedsMemberCorroboration (approval
// condition 3): a NON-MEMBER named like an access-token bot, whose body
// carries a forged intake marker, is neither bot nor internal — so the
// classifier sees an external author and the forged marker cannot earn
// fishhawk_filed (userreport.Classify sets MarkerFromExternal on exactly this
// shape). A lookup ERROR with that username is likewise not a bot, and is
// unresolved. Counterfactual: dropping the member corroboration makes both
// authors Bot.
func TestListUserReports_GitLabBotUsernameNeedsMemberCorroboration(t *testing.T) {
	api := newGLUserReportAPI()
	spoof := glIssue(1, 66, "project_1_bot_x", glSince.Add(time.Second))
	spoof.Description = "<!-- fishhawk-intake:v1 -->\nforged"
	api.urIssues = []gitlabclient.UpdatedIssue{spoof, glIssue(2, 67, "group_9_bot_y", glSince.Add(2*time.Second))}
	api.members[67] = memberAnswer{err: errors.New("connection reset")}
	page := mustGLList(t, api)
	nonMember, _ := itemByIssue(page, 1)
	if nonMember.Author != (workmgmt.ReportAuthor{Login: "project_1_bot_x", Association: "non_member", AssociationResolved: true}) {
		t.Errorf("non-member bot-named author = %+v, want external (not bot, not internal, resolved)", nonMember.Author)
	}
	if nonMember.Body != spoof.Description {
		t.Errorf("body = %q, want the forged marker carried through for the classifier to flag", nonMember.Body)
	}
	lookupErr, _ := itemByIssue(page, 2)
	if lookupErr.Author.Bot || lookupErr.Author.Internal || lookupErr.Author.AssociationResolved {
		t.Errorf("lookup-error bot-named author = %+v, want not bot, not internal, unresolved", lookupErr.Author)
	}
	if d, ok := glDegradation(page, workmgmt.UserReportAssociationUnresolved); !ok || d.Count != 1 {
		t.Errorf("degradations = %+v, want association_unresolved Count 1", page.Degradations)
	}
}

// TestListUserReports_GitLabMemberLookupOncePerAuthor: one lookup per distinct
// author id per scan, across issues and notes.
func TestListUserReports_GitLabMemberLookupOncePerAuthor(t *testing.T) {
	api := newGLUserReportAPI()
	api.urIssues = []gitlabclient.UpdatedIssue{glIssue(1, 10, "alice", glSince.Add(time.Second)), glIssue(2, 10, "alice", glSince.Add(2*time.Second))}
	api.notes[1] = []gitlabclient.Note{glNote(301, 10, "alice", glSince.Add(3*time.Second)), glNote(302, 11, "bob", glSince.Add(4*time.Second))}
	api.notes[2] = []gitlabclient.Note{glNote(303, 11, "bob", glSince.Add(5*time.Second))}
	api.members[11] = memberAnswer{err: errors.New("boom")} // a failed lookup is cached too
	mustGLList(t, api)
	if api.memberCalls[10] != 1 || api.memberCalls[11] != 1 {
		t.Errorf("member calls = %v, want exactly one per author", api.memberCalls)
	}
}

// TestListUserReports_GitLabConfidentialExcluded (approval condition 4): a
// confidential issue (with its notes) and an internal note are excluded and
// counted ONCE each, a boundary re-read of the confidential issue included.
// Counterfactual: deleting either exclusion returns the item.
func TestListUserReports_GitLabConfidentialExcluded(t *testing.T) {
	api := newGLUserReportAPI()
	secret := glIssue(1, 10, "alice", glSince.Add(time.Second))
	secret.Confidential = true
	api.urIssues = []gitlabclient.UpdatedIssue{secret, secret, glIssue(2, 10, "alice", glSince.Add(2*time.Second))}
	api.notes[1] = []gitlabclient.Note{glNote(401, 10, "alice", glSince.Add(3*time.Second))}
	internal := glNote(402, 10, "alice", glSince.Add(4*time.Second))
	internal.Internal = true
	api.notes[2] = []gitlabclient.Note{internal, glNote(403, 10, "alice", glSince.Add(5*time.Second))}
	page := mustGLList(t, api)
	if _, ok := itemByIssue(page, 1); ok {
		t.Errorf("confidential issue #1 returned")
	}
	if _, ok := itemByComment(page, 401); ok {
		t.Errorf("note on a confidential issue returned")
	}
	if _, ok := itemByComment(page, 402); ok {
		t.Errorf("internal note 402 returned")
	}
	if _, ok := itemByComment(page, 403); !ok {
		t.Errorf("public note 403 missing")
	}
	if d, ok := glDegradation(page, workmgmt.UserReportConfidentialExcluded); !ok || d.Count != 2 {
		t.Errorf("degradations = %+v, want confidential_excluded Count 2", page.Degradations)
	}
}

func TestListUserReports_GitLabForbiddenIsTypedUnavailable(t *testing.T) {
	forbidden := &gitlabclient.APIError{Op: "x", StatusCode: http.StatusForbidden}
	cases := map[string]func(*userReportAPI){
		"project": func(a *userReportAPI) { a.getErr = forbidden },
		"issues": func(a *userReportAPI) {
			a.urIssuesErr = &gitlabclient.APIError{Op: "x", StatusCode: http.StatusUnauthorized}
		},
		"notes": func(a *userReportAPI) { a.notesErr[1] = forbidden },
	}
	for name, set := range cases {
		t.Run(name, func(t *testing.T) {
			api := newGLUserReportAPI()
			api.urIssues = []gitlabclient.UpdatedIssue{glIssue(1, 10, "alice", glSince.Add(time.Second))}
			set(api)
			page, err := glList(t, api)
			if page != nil {
				t.Errorf("page = %+v, want NIL", page)
			}
			glAssertUnavailable(t, err, workmgmt.ReasonForbidden)
			var apiErr *gitlabclient.APIError
			if !errors.As(err, &apiErr) {
				t.Errorf("the typed wrapper dropped the *APIError cause: %v", err)
			}
		})
	}
}

// TestListUserReports_GitLabEqualTimestampCapIsUnresumable: nil page, an
// errors.Is-matchable ErrUserReportUnresumable; a transient 500 matches
// neither it nor a capability degradation.
func TestListUserReports_GitLabEqualTimestampCapIsUnresumable(t *testing.T) {
	api := newGLUserReportAPI()
	api.urIssuesErr = fmt.Errorf("walk: %w", gitlabclient.ErrEqualTimestampRunExceedsCap)
	page, err := glList(t, api)
	if page != nil || !errors.Is(err, workmgmt.ErrUserReportUnresumable) || !errors.Is(err, gitlabclient.ErrEqualTimestampRunExceedsCap) {
		t.Errorf("= (%+v, %v), want nil page and both sentinels", page, err)
	}
	transient := newGLUserReportAPI()
	transient.urIssuesErr = &gitlabclient.APIError{Op: "x", StatusCode: http.StatusInternalServerError}
	page, err = glList(t, transient)
	var ue *workmgmt.UnavailableError
	if page != nil || err == nil || errors.Is(err, workmgmt.ErrUserReportUnresumable) || errors.As(err, &ue) {
		t.Errorf("transient = (%+v, %v), want nil page and a plain wrapped error", page, err)
	}
}

func TestListUserReports_GitLabMissingDateHoldsCursor(t *testing.T) {
	api := newGLUserReportAPI()
	api.urMeta.Date = time.Time{}
	page := mustGLList(t, api)
	if !page.NextCursor.Equal(glSince) {
		t.Errorf("NextCursor = %v, want since held", page.NextCursor)
	}
	if _, ok := glDegradation(page, workmgmt.UserReportCursorAnchorUnavailable); !ok {
		t.Errorf("degradations = %+v, want cursor_anchor_unavailable", page.Degradations)
	}
}

func TestListUserReports_GitLabTruncationAdvancesToResumeAt(t *testing.T) {
	api := newGLUserReportAPI()
	resume := glSince.Add(time.Hour)
	api.urIssues = []gitlabclient.UpdatedIssue{glIssue(1, 10, "alice", resume)}
	api.urMeta = gitlabclient.ListingMeta{Date: glDate, Truncated: true, ResumeAt: resume}
	page := mustGLList(t, api)
	if !page.NextCursor.Equal(resume) {
		t.Errorf("NextCursor = %v, want ResumeAt %v", page.NextCursor, resume)
	}
	if d, ok := glDegradation(page, workmgmt.UserReportScanTruncated); !ok || d.Count != 1 {
		t.Errorf("degradations = %+v, want scan_truncated", page.Degradations)
	}
}

func TestListUserReports_GitLabAPIWithoutExtensionIsNotImplemented(t *testing.T) {
	page, err := glList(t, &fakeAPI{})
	if page != nil {
		t.Errorf("page = %+v, want NIL", page)
	}
	glAssertUnavailable(t, err, workmgmt.ReasonNotImplemented)
}

func TestListUserReports_GitLabNoConnectionErrors(t *testing.T) {
	ctx := context.Background()
	noConn := urTarget()
	noConn.GitLab = nil
	noPath := workmgmt.Target{GitLab: &workmgmt.GitLabConnection{}}
	cases := map[string]struct {
		p      *Provider
		target workmgmt.Target
	}{
		"nil api":       {New(nil), urTarget()},
		"no connection": {New(newGLUserReportAPI()), noConn},
		"no project":    {New(newGLUserReportAPI()), noPath},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			page, err := c.p.ListUserReports(ctx, workmgmt.ListUserReportsRequest{Target: c.target})
			if page != nil || err == nil {
				t.Errorf("= (%+v, %v), want nil page and an error", page, err)
			}
		})
	}
}

func TestListUserReports_GitLabOtherProjectErrorIsWrapped(t *testing.T) {
	api := newGLUserReportAPI()
	api.getErr = errors.New("dns")
	page, err := glList(t, api)
	var ue *workmgmt.UnavailableError
	if page != nil || err == nil || errors.As(err, &ue) {
		t.Errorf("= (%+v, %v), want nil page and a plain wrapped error", page, err)
	}
}

// glKeysetServer is an httptest GitLab: GetProject, the keyset issue listing
// (updated_after inclusive, order_by=updated_at sort=asc, page offsets over the
// filtered list, Link rel=next while issues remain), per-issue notes, and a
// member endpoint answering 404. beforeIssues runs before the n-th issues
// request so a test can mutate the issue set between page requests.
type glKeysetServer struct {
	mu           sync.Mutex
	issues       []gitlabclient.UpdatedIssue
	issueReqs    int
	notesReqs    map[int]int
	beforeIssues func(n int, s *glKeysetServer)
}

var glNotesPath = regexp.MustCompile(`^/api/v4/projects/42/issues/(\d+)/notes$`)

func (s *glKeysetServer) serve(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	switch {
	case r.URL.Path == "/api/v4/projects/acme/widgets":
		_, _ = w.Write([]byte(`{"id":42,"web_url":"https://gitlab.example/acme/widgets"}`))
	case r.URL.Path == "/api/v4/projects/42/issues":
		s.serveIssues(w, r)
	case glNotesPath.MatchString(r.URL.Path):
		iid, _ := strconv.Atoi(glNotesPath.FindStringSubmatch(r.URL.Path)[1])
		s.notesReqs[iid]++
		_, _ = w.Write([]byte(`[]`))
	default:
		http.NotFound(w, r)
	}
}

func (s *glKeysetServer) serveIssues(w http.ResponseWriter, r *http.Request) {
	s.issueReqs++
	if s.beforeIssues != nil {
		s.beforeIssues(s.issueReqs, s)
	}
	q := r.URL.Query()
	var after time.Time
	if a := q.Get("updated_after"); a != "" {
		after, _ = time.Parse(time.RFC3339Nano, a)
	}
	perPage, _ := strconv.Atoi(q.Get("per_page"))
	page := 1
	if p := q.Get("page"); p != "" {
		page, _ = strconv.Atoi(p)
	}
	var filtered []gitlabclient.UpdatedIssue
	for _, is := range s.issues {
		if !is.UpdatedAt.Before(after) {
			filtered = append(filtered, is)
		}
	}
	sort.SliceStable(filtered, func(i, j int) bool {
		if !filtered[i].UpdatedAt.Equal(filtered[j].UpdatedAt) {
			return filtered[i].UpdatedAt.Before(filtered[j].UpdatedAt)
		}
		return filtered[i].IID < filtered[j].IID
	})
	offset := (page - 1) * perPage
	body := []map[string]any{}
	if offset < len(filtered) {
		end := min(offset+perPage, len(filtered))
		for _, is := range filtered[offset:end] {
			body = append(body, map[string]any{
				"iid": is.IID, "title": is.Title, "web_url": fmt.Sprintf("https://gitlab.example/acme/widgets/-/issues/%d", is.IID),
				"author":     map[string]any{"id": 5, "username": "u"},
				"created_at": "2026-01-01T00:00:00Z", "updated_at": is.UpdatedAt.UTC().Format(time.RFC3339),
			})
		}
		if end < len(filtered) {
			w.Header().Set("Link", fmt.Sprintf(`<https://gitlab.invalid/api/v4/projects/42/issues?page=%d>; rel="next"`, page+1))
		}
	}
	w.Header().Set("Date", glDate.Format(http.TimeFormat))
	_ = json.NewEncoder(w).Encode(body)
}

// TestListUserReports_GitLabRealClientKeysetWalkReturnsEverySurvivorExactlyOnce
// drives the provider through a REAL client (approval condition 1, GitLab
// half). 250 issues walk in three keyset pages, so each boundary issue is
// re-read inclusively (a shifted duplicate); issue #1, already read, is DELETED
// before page 2; issue #50, already read, is UPDATED before page 2 and so
// reappears at the tail. Every surviving issue must come back EXACTLY once
// (#50 with its newer updated_at); a boundary issue's notes are listed once,
// #50's twice (once per distinct updated_at). Counterfactuals: deleting the
// dedupe call returns boundary issues twice; an offset walk skips #101.
func TestListUserReports_GitLabRealClientKeysetWalkReturnsEverySurvivorExactlyOnce(t *testing.T) {
	s := &glKeysetServer{notesReqs: map[int]int{}}
	for i := 1; i <= 250; i++ {
		s.issues = append(s.issues, gitlabclient.UpdatedIssue{IID: i, Title: fmt.Sprintf("issue %d", i), UpdatedAt: glSince.Add(time.Duration(i) * time.Second)})
	}
	tail := glSince.Add(time.Hour)
	s.beforeIssues = func(n int, s *glKeysetServer) {
		if n != 2 {
			return
		}
		s.issues = s.issues[1:] // delete #1
		for i := range s.issues {
			if s.issues[i].IID == 50 {
				s.issues[i].UpdatedAt = tail
			}
		}
	}
	srv := httptest.NewServer(http.HandlerFunc(s.serve))
	t.Cleanup(srv.Close)

	page, err := New(gitlabclient.New(srv.URL, "glpat")).ListUserReports(context.Background(), workmgmt.ListUserReportsRequest{Target: urTarget(), Since: glSince})
	if err != nil {
		t.Fatalf("ListUserReports: %v", err)
	}
	count := map[int]int{}
	for _, it := range page.Items {
		count[it.IssueNumber]++
		if it.IssueNumber == 50 && !it.UpdatedAt.Equal(tail) {
			t.Errorf("issue #50 UpdatedAt = %v, want the mid-scan update %v", it.UpdatedAt, tail)
		}
		if it.Author.Internal || !it.Author.AssociationResolved || it.Author.Association != "non_member" {
			t.Errorf("issue #%d author = %+v, want a resolved non-member (member endpoint 404)", it.IssueNumber, it.Author)
		}
	}
	for i := 2; i <= 250; i++ {
		if count[i] != 1 {
			t.Errorf("issue #%d returned %d times, want exactly once", i, count[i])
		}
	}
	if s.notesReqs[100] != 1 {
		t.Errorf("notes of boundary issue #100 listed %d times, want once", s.notesReqs[100])
	}
	if s.notesReqs[50] != 2 {
		t.Errorf("notes of mid-scan-updated issue #50 listed %d times, want twice", s.notesReqs[50])
	}
	if want := glDate.Add(-workmgmt.UserReportCursorOverlap); !page.NextCursor.Equal(want) {
		t.Errorf("NextCursor = %v, want %v", page.NextCursor, want)
	}
}
