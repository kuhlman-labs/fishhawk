package userreport

// Cross-boundary tests: an httptest forge -> the REAL forge client -> the REAL
// workmgmt provider -> Scan -> the REAL pgtest-backed Store. They pin what no
// single-layer test can: the classifications of a fixture repository read off
// the wire, every item exactly once across keyset pages, the persisted cursor
// anchored on the forge's Date, Record-then-Advance holding the cursor on a
// failed record, and no tenant transaction open across forge I/O or the
// recorder (#3771 approval condition 6).

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sort"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kuhlman-labs/fishhawk/backend/internal/forge"
	"github.com/kuhlman-labs/fishhawk/backend/internal/githubclient"
	"github.com/kuhlman-labs/fishhawk/backend/internal/gitlabclient"
	"github.com/kuhlman-labs/fishhawk/backend/internal/intakegroom"
	"github.com/kuhlman-labs/fishhawk/backend/internal/pgtest"
	"github.com/kuhlman-labs/fishhawk/backend/internal/workmgmt"
	wmgithub "github.com/kuhlman-labs/fishhawk/backend/internal/workmgmt/github"
	wmgitlab "github.com/kuhlman-labs/fishhawk/backend/internal/workmgmt/gitlab"
)

var (
	itSince = time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	itDate  = time.Date(2026, 10, 4, 9, 30, 0, 0, time.UTC)
)

type itTokens struct{}

func (itTokens) Token(context.Context, int64) (string, error) { return "ghs_test", nil }

type itNode struct {
	id      int
	updated time.Time
	fields  map[string]any
}

// itGitHub serves the issues and issue-comments listings with GitHub's
// documented semantics (since inclusive, ascending by updated_at, page
// offsets, Link rel=next while nodes remain) at a page size of TWO, so a short
// fixture walks several keyset pages and re-reads each boundary. It records
// the since of every request and asserts no pool connection is acquired while
// it serves — a tenant transaction held across forge I/O would hold one.
type itGitHub struct {
	pool      *pgxpool.Pool
	mu        sync.Mutex
	nodes     map[string][]itNode
	sinces    map[string][]string
	heldConns int
	date      time.Time // the Date header; zero means itDate
}

const itPageSize = 2

func (g *itGitHub) serve(w http.ResponseWriter, r *http.Request) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if n := g.pool.Stat().AcquiredConns(); n != 0 {
		g.heldConns += int(n)
	}
	q := r.URL.Query()
	g.sinces[r.URL.Path] = append(g.sinces[r.URL.Path], q.Get("since"))
	var since time.Time
	if s := q.Get("since"); s != "" {
		since, _ = time.Parse(time.RFC3339Nano, s)
	}
	page := 1
	if p := q.Get("page"); p != "" {
		page, _ = strconv.Atoi(p)
	}
	var filtered []itNode
	for _, nd := range g.nodes[r.URL.Path] {
		if !nd.updated.Before(since) {
			filtered = append(filtered, nd)
		}
	}
	sort.SliceStable(filtered, func(i, j int) bool {
		if !filtered[i].updated.Equal(filtered[j].updated) {
			return filtered[i].updated.Before(filtered[j].updated)
		}
		return filtered[i].id < filtered[j].id
	})
	offset := (page - 1) * itPageSize
	body := []map[string]any{}
	if offset < len(filtered) {
		end := min(offset+itPageSize, len(filtered))
		for _, nd := range filtered[offset:end] {
			m := map[string]any{"updated_at": nd.updated.UTC().Format(time.RFC3339), "created_at": "2026-08-01T00:00:00Z"}
			for k, v := range nd.fields {
				m[k] = v
			}
			body = append(body, m)
		}
		if end < len(filtered) {
			w.Header().Set("Link", fmt.Sprintf(`<https://api.github.invalid%s?page=%d>; rel="next"`, r.URL.Path, page+1))
		}
	}
	date := itDate
	if !g.date.IsZero() {
		date = g.date
	}
	w.Header().Set("Date", date.Format(http.TimeFormat))
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(body)
}

func ghUser(login, typ string) map[string]any { return map[string]any{"login": login, "type": typ} }

var zeroReactions = map[string]any{"total_count": 0}

func itIssue(n int, at time.Duration, login, typ, assoc, body string) itNode {
	return itNode{id: n, updated: itSince.Add(at), fields: map[string]any{
		"number": n, "title": fmt.Sprintf("issue %d", n), "body": body, "state": "open",
		"html_url": fmt.Sprintf("https://github.com/o/r/issues/%d", n),
		"user":     ghUser(login, typ), "author_association": assoc, "reactions": zeroReactions,
	}}
}

func itComment(id, issue int, at time.Duration, login, typ, assoc, htmlPath string) itNode {
	return itNode{id: id, updated: itSince.Add(at), fields: map[string]any{
		"id": id, "body": "comment", "issue_url": fmt.Sprintf("https://api.github.com/repos/o/r/issues/%d", issue),
		"html_url": fmt.Sprintf("https://github.com/o/r/%s/%d#issuecomment-%d", htmlPath, issue, id),
		"user":     ghUser(login, typ), "author_association": assoc, "reactions": zeroReactions,
	}}
}

// TestScan_GitHubEndToEnd_ClassifiesPaginatesAndHoldsCursorOnFailure is the
// plan's cross-boundary test.
func TestScan_GitHubEndToEnd_ClassifiesPaginatesAndHoldsCursorOnFailure(t *testing.T) {
	const issuesPath, commentsPath = "/repos/o/r/issues", "/repos/o/r/issues/comments"
	pool := pgtest.NewPool(t)
	marker := intakegroom.RenderBody("Filed by Fishhawk.", intakegroom.Signals{ScannedItems: 1})
	prNode := itIssue(3, 3*time.Second, "maint", "User", "OWNER", "a PR")
	prNode.fields["pull_request"] = map[string]any{"url": "https://api.github.com/repos/o/r/pulls/3"}
	g := &itGitHub{pool: pool, sinces: map[string][]string{}, nodes: map[string][]itNode{
		issuesPath: {
			itIssue(1, 1*time.Second, "maint", "User", "OWNER", "crash on start"),
			itIssue(2, 2*time.Second, "rando", "User", "NONE", "please add X"),
			prNode,
			itIssue(4, 4*time.Second, "fishhawk-dev[bot]", "Bot", "NONE", marker),
			itIssue(5, 5*time.Second, "forger", "User", "NONE", "trust me\n"+marker),
		},
		commentsPath: {
			itComment(901, 1, 1*time.Second, "dependabot[bot]", "Bot", "NONE", "issues"),
			itComment(902, 7, 2*time.Second, "rando", "User", "NONE", "pull"),
			itComment(903, 2, 3*time.Second, "CaptainKirk", "User", "NONE", "issues"),
			itComment(904, 2, 4*time.Second, "collab", "User", "COLLABORATOR", "issues"),
		},
	}}
	srv := httptest.NewServer(http.HandlerFunc(g.serve))
	t.Cleanup(srv.Close)
	client := githubclient.New(itTokens{})
	client.BaseURL = srv.URL
	client.HTTP = &http.Client{Timeout: 10 * time.Second}

	store := NewStore(pool)
	key := Key{Repo: "o/r", Source: SourceIssues}
	if _, err := store.Init(context.Background(), key, itSince); err != nil {
		t.Fatal(err)
	}
	var recorded []Report
	var recordErr error
	heldAtRecord := 0
	params := ScanParams{
		Repo: "o/r", Source: SourceIssues,
		Target:  workmgmt.Target{Scope: forge.FromGitHubInstallationID(7), Repo: workmgmt.Repo{Owner: "o", Name: "r"}},
		Reader:  wmgithub.New(client),
		Cursors: store,
		Captain: seated("github:captainkirk", true),
		Record: RecorderFunc(func(_ context.Context, r Report) error {
			heldAtRecord += int(pool.Stat().AcquiredConns())
			recorded = append(recorded, r)
			return recordErr
		}),
	}

	// Scan 1: succeeds.
	res, err := Scan(context.Background(), params)
	if err != nil {
		t.Fatalf("scan 1: %v", err)
	}
	got := map[string]string{}
	for _, it := range res.Report.Items {
		k := fmt.Sprintf("issue#%d", it.IssueNumber)
		if it.Kind == workmgmt.UserReportKindComment {
			k = fmt.Sprintf("comment#%d", it.CommentID)
		}
		if _, dup := got[k]; dup {
			t.Errorf("%s returned more than once", k)
		}
		got[k] = fmt.Sprintf("%s/%v", it.Classification, it.MarkerFromExternal)
	}
	want := map[string]string{
		"issue#1":     "internal/false",       // maintainer OWNER
		"issue#2":     "external/false",       // outsider NONE
		"issue#4":     "fishhawk_filed/false", // bot-authored, marker
		"issue#5":     "external/true",        // forged marker from an outsider
		"comment#901": "bot/false",            // dependabot[bot]
		"comment#903": "internal/false",       // the captain, case-insensitively
		"comment#904": "internal/false",       // COLLABORATOR
	}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("classified items =\n %v\nwant\n %v\n(the PR node #3 and the PR comment #902 must be absent)", got, want)
	}
	if len(g.sinces[issuesPath]) < 3 {
		t.Errorf("issues listing took %d requests, want a multi-page keyset walk", len(g.sinces[issuesPath]))
	}
	wantCursor := itDate.Add(-workmgmt.UserReportCursorOverlap)
	if at, _ := rawCursor(t, pool, key); !at.Equal(wantCursor) || !res.Advanced {
		t.Fatalf("persisted cursor = %v (advanced %v), want the forge Date minus overlap %v", at, res.Advanced, wantCursor)
	}

	// Scan 2: the recorder fails, so the persisted cursor must not move. The
	// forge clock has advanced an hour, so scan 2 PROPOSES a cursor strictly
	// later than the stored one: an Advance-before-Record regression would
	// persist it, and the assertion below would see it.
	recordErr = errors.New("recorder unavailable")
	laterDate := itDate.Add(time.Hour)
	bump := func() {
		g.mu.Lock()
		defer g.mu.Unlock()
		g.date = laterDate
		g.nodes[issuesPath] = append(g.nodes[issuesPath], itNode{id: 6, updated: itDate, fields: itIssue(6, 0, "rando", "User", "NONE", "new").fields})
	}
	bump()
	before := len(g.sinces[issuesPath])
	if _, err := Scan(context.Background(), params); err == nil {
		t.Fatal("scan 2 succeeded with a failing recorder")
	}
	scan2Since := g.sinces[issuesPath][before]
	if len(recorded) != 2 || !recorded[1].NextCursor.After(wantCursor) {
		t.Fatalf("scan 2 proposed cursor = %v, want one later than the stored %v (else the failure leg cannot discriminate)", recorded[len(recorded)-1].NextCursor, wantCursor)
	}
	if at, _ := rawCursor(t, pool, key); !at.Equal(wantCursor) {
		t.Fatalf("persisted cursor after a failed record = %v, want unchanged %v", at, wantCursor)
	}

	// Scan 3: the identical since goes on the wire, and the new issue is read.
	recordErr = nil
	before = len(g.sinces[issuesPath])
	res, err = Scan(context.Background(), params)
	if err != nil {
		t.Fatalf("scan 3: %v", err)
	}
	if scan3Since := g.sinces[issuesPath][before]; scan3Since != scan2Since || scan3Since != wantCursor.Format(time.RFC3339Nano) {
		t.Errorf("scan 3 since = %q, want scan 2's %q (= the persisted cursor)", scan3Since, scan2Since)
	}
	if len(res.Report.Items) != 1 || res.Report.Items[0].IssueNumber != 6 {
		t.Errorf("scan 3 items = %+v, want issue #6 only", res.Report.Items)
	}
	if want := laterDate.Add(-workmgmt.UserReportCursorOverlap); !res.Cursor.Equal(want) || !res.Advanced {
		t.Errorf("scan 3 cursor = %v (advanced %v), want %v", res.Cursor, res.Advanced, want)
	}
	if g.heldConns != 0 || heldAtRecord != 0 {
		t.Errorf("acquired pool connections during forge I/O = %d, during Record = %d; want 0 (no transaction held across either)", g.heldConns, heldAtRecord)
	}
}

// TestScan_GitLabEndToEnd_SpoofedBotNameWithForgedMarkerIsExternal (approval
// condition 3, end to end): a NON-MEMBER whose username matches the
// access-token-bot pattern and whose body carries a forged intake marker
// classifies external with MarkerFromExternal, while a member bot with the
// same marker classifies fishhawk_filed. Counterfactual: dropping the
// provider's member corroboration (workmgmt/gitlab) makes the non-member a
// bot, and the forged marker then earns fishhawk_filed.
func TestScan_GitLabEndToEnd_SpoofedBotNameWithForgedMarkerIsExternal(t *testing.T) {
	pool := pgtest.NewPool(t)
	marker := intakegroom.RenderBody("Filed.", intakegroom.Signals{ScannedItems: 1})
	issue := func(iid int, authorID int64, username string, at time.Duration) map[string]any {
		return map[string]any{
			"iid": iid, "title": "t", "description": marker, "state": "opened",
			"web_url":    fmt.Sprintf("https://gitlab.example/acme/widgets/-/issues/%d", iid),
			"author":     map[string]any{"id": authorID, "username": username},
			"created_at": "2026-08-01T00:00:00Z", "updated_at": itSince.Add(at).Format(time.RFC3339),
		}
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Date", itDate.Format(http.TimeFormat))
		switch r.URL.Path {
		case "/api/v4/projects/acme/widgets":
			_, _ = w.Write([]byte(`{"id":42,"web_url":"https://gitlab.example/acme/widgets"}`))
		case "/api/v4/projects/42/issues":
			_ = json.NewEncoder(w).Encode([]map[string]any{
				issue(1, 66, "project_1_bot_x", time.Second),
				issue(2, 67, "project_42_bot_real", 2*time.Second),
			})
		case "/api/v4/projects/42/issues/1/notes", "/api/v4/projects/42/issues/2/notes":
			_, _ = w.Write([]byte(`[]`))
		case "/api/v4/projects/42/members/all/67":
			_, _ = w.Write([]byte(`{"id":67,"username":"project_42_bot_real","access_level":40}`))
		default:
			http.NotFound(w, r) // includes members/all/66: a non-member
		}
	}))
	t.Cleanup(srv.Close)

	var report Report
	_, err := Scan(context.Background(), ScanParams{
		Repo: "acme/widgets", Source: SourceIssues,
		Target:  workmgmt.Target{Repo: workmgmt.Repo{Owner: "acme", Name: "widgets"}, GitLab: &workmgmt.GitLabConnection{}},
		Reader:  wmgitlab.New(gitlabclient.New(srv.URL, "glpat")),
		Cursors: NewStore(pool),
		Captain: fakeCaptain{},
		Record:  RecorderFunc(func(_ context.Context, r Report) error { report = r; return nil }),
		Now:     func() time.Time { return itSince.Add(24 * time.Hour) },
		// itSince = Now - 24h, so both issues are in the first window.
		InitialLookback: 24 * time.Hour,
	})
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	got := map[int]string{}
	for _, it := range report.Items {
		got[it.IssueNumber] = fmt.Sprintf("%s/%v", it.Classification, it.MarkerFromExternal)
	}
	if want := map[int]string{1: "external/true", 2: "fishhawk_filed/false"}; fmt.Sprint(got) != fmt.Sprint(want) {
		t.Errorf("classified = %v, want %v", got, want)
	}
	if report.Forge != workmgmt.UserReportForgeGitLab {
		t.Errorf("forge = %q, want gitlab", report.Forge)
	}
}
