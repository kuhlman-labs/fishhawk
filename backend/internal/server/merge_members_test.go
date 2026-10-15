package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/forge"
	forgegitlab "github.com/kuhlman-labs/fishhawk/backend/internal/forge/gitlab"
	"github.com/kuhlman-labs/fishhawk/backend/internal/githubclient"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
	"github.com/kuhlman-labs/fishhawk/backend/internal/spec"
)

// membersSpecYAML renders a feature_change spec whose review stage (a
// pull_request input, so it settles on merge) carries one approval gate.
// membersLine is the YAML after `members:` ("" omits the key).
func membersSpecYAML(t *testing.T, membersLine string) []byte {
	t.Helper()
	body := `version: "1.0"
workflows:
  feature_change:
    stages:
      - id: plan
        type: plan
        executor:
          agent: claude-code
        produces:
          - artifact: plan
            schema: standard_v1
      - id: implement
        type: implement
        executor:
          agent: claude-code
        produces:
          - artifact: pull_request
      - id: review
        type: review
        executor:
          human: true
        inputs:
          - artifact: pull_request
            from_stage: implement
        gates:
          - type: approval
            approvals:
              count: 1
`
	if membersLine != "" {
		body += "              members: " + membersLine + "\n"
	}
	if _, err := spec.ParseBytes([]byte(body)); err != nil {
		t.Fatalf("membersSpecYAML does not parse: %v", err)
	}
	return []byte(body)
}

// mmReview is one review the GitHub stub serves.
type mmReview struct {
	login string
	state string
}

// mmGitHub is what the GitHub stub serves.
type mmGitHub struct {
	merger           string // "" serves merged_by: null
	prStatus         int
	reviews          []mmReview
	reviewsStatus    int
	reviewsTruncated bool
}

// membersGitHubStub is an httptest GitHub serving GET /pulls/{n} (with
// merged_by) and GET /pulls/{n}/reviews, call-counted, mutable mid-test.
type membersGitHubStub struct {
	mu sync.Mutex
	mmGitHub
	prCalls     int
	reviewCalls int
}

func (st *membersGitHubStub) calls() int {
	st.mu.Lock()
	defer st.mu.Unlock()
	return st.prCalls + st.reviewCalls
}

func (st *membersGitHubStub) setReviews(rs []mmReview) {
	st.mu.Lock()
	defer st.mu.Unlock()
	st.reviews = rs
}

func newMembersGitHubClient(t *testing.T, st *membersGitHubStub) *githubclient.Client {
	t.Helper()
	mux := http.NewServeMux()
	var srv *httptest.Server
	mux.HandleFunc("GET /repos/{owner}/{repo}/pulls/{number}", func(w http.ResponseWriter, _ *http.Request) {
		st.mu.Lock()
		st.prCalls++
		status, merger := st.prStatus, st.merger
		st.mu.Unlock()
		if status != 0 && status != http.StatusOK {
			w.WriteHeader(status)
			return
		}
		var mergedBy any
		if merger != "" {
			mergedBy = map[string]any{"login": merger}
		}
		raw, _ := json.Marshal(map[string]any{
			"node_id": "PR_x", "state": "closed", "merged": true, "merged_by": mergedBy,
			"head": map[string]any{"sha": "h"}, "base": map[string]any{"ref": "main"},
		})
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(raw)
	})
	mux.HandleFunc("GET /repos/{owner}/{repo}/pulls/{number}/reviews", func(w http.ResponseWriter, _ *http.Request) {
		st.mu.Lock()
		st.reviewCalls++
		status, reviews, truncated := st.reviewsStatus, append([]mmReview(nil), st.reviews...), st.reviewsTruncated
		st.mu.Unlock()
		if status != 0 && status != http.StatusOK {
			w.WriteHeader(status)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		if truncated {
			w.Header().Set("Link", fmt.Sprintf(`<%s/next?page=2>; rel="next"`, srv.URL))
			_, _ = w.Write([]byte("[]"))
			return
		}
		out := make([]map[string]any, 0, len(reviews))
		for i, rv := range reviews {
			out = append(out, map[string]any{"id": i + 1, "user": map[string]any{"login": rv.login}, "state": rv.state})
		}
		raw, _ := json.Marshal(out)
		_, _ = w.Write(raw)
	})
	srv = httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return &githubclient.Client{
		BaseURL: srv.URL,
		Tokens:  &fakeTokenProvider{tok: "ghs_t"},
		HTTP:    &http.Client{Timeout: 5 * time.Second},
	}
}

// membersGitLabStub serves the single MR read through a real
// *forgegitlab.Forge (the realGitLabForge pattern), call-counted.
type membersGitLabStub struct {
	mu sync.Mutex
	mmGitLab
	calls int
}

// mmGitLab is what the GitLab stub serves (status 0 is 200).
type mmGitLab struct {
	body   string
	status int
}

func (st *membersGitLabStub) count() int {
	st.mu.Lock()
	defer st.mu.Unlock()
	return st.calls
}

func newMembersGitLabForge(t *testing.T, st *membersGitLabStub) *forgegitlab.Forge {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v4/projects/5/merge_requests/7", func(w http.ResponseWriter, _ *http.Request) {
		st.mu.Lock()
		st.calls++
		body, status := st.body, st.status
		st.mu.Unlock()
		if status == 0 {
			status = http.StatusOK
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return forgegitlab.New(srv.URL, forgegitlab.NewStaticCredentialProvider("glpat-test"),
		forgegitlab.WithHTTPClient(srv.Client()))
}

// gitLabMRReadBody renders a merged MR read naming mergeUser / mergedBy ("" is
// JSON null).
func gitLabMRReadBody(mergeUser, mergedBy string) string {
	user := func(u string) any {
		if u == "" {
			return nil
		}
		return map[string]any{"username": u}
	}
	raw, _ := json.Marshal(map[string]any{
		"iid": 7, "state": "merged", "target_branch": "main",
		"merge_user": user(mergeUser), "merged_by": user(mergedBy),
	})
	return string(raw)
}

// membersAuditRepo records appends and serves them back by category, with an
// injectable list error and an injectable refusal-append error.
type membersAuditRepo struct {
	audit.Repository
	mu       sync.Mutex
	appended []audit.ChainAppendParams
	listErr  error
	// appendErr, when set, fails every merge_settle_members_refused append
	// (counted in refusalAttempts, not recorded); other categories still
	// record, so a regression that settled the merge leaves a visible row.
	appendErr       error
	refusalAttempts int
}

func (r *membersAuditRepo) AppendChained(_ context.Context, p audit.ChainAppendParams) (*audit.Entry, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if p.Category == CategoryMergeSettleMembersRefused {
		r.refusalAttempts++
		if r.appendErr != nil {
			return nil, r.appendErr
		}
	}
	r.appended = append(r.appended, p)
	return &audit.Entry{ID: uuid.New()}, nil
}

func (r *membersAuditRepo) ListForRunByCategory(_ context.Context, runID uuid.UUID, category string) ([]*audit.Entry, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.listErr != nil {
		return nil, r.listErr
	}
	var out []*audit.Entry
	for _, ap := range r.appended {
		if ap.RunID == runID && ap.Category == category {
			rid := ap.RunID
			out = append(out, &audit.Entry{RunID: &rid, StageID: ap.StageID, Category: ap.Category, Payload: ap.Payload})
		}
	}
	return out, nil
}

// refusalRows returns the merge_settle_members_refused payloads in rows.
func refusalRows(t *testing.T, rows []audit.ChainAppendParams) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, r := range rows {
		if r.Category != CategoryMergeSettleMembersRefused {
			continue
		}
		var p map[string]any
		if err := json.Unmarshal(r.Payload, &p); err != nil {
			t.Fatalf("refusal payload: %v", err)
		}
		out = append(out, p)
	}
	return out
}

const (
	mmGitHubPRURL = "https://github.com/o/r/pull/42"
	mmGitLabPRURL = "https://gitlab.example/group/project/-/merge_requests/7"
)

func mmGitHubRun(specBytes []byte) *run.Run {
	inst := int64(7)
	u := mmGitHubPRURL
	return &run.Run{ID: uuid.New(), WorkflowID: "feature_change", Repo: "o/r",
		PullRequestURL: &u, InstallationID: &inst, WorkflowSpec: specBytes}
}

func mmGitLabRun(specBytes []byte) *run.Run {
	ref := "gitlab:5"
	u := mmGitLabPRURL
	return &run.Run{ID: uuid.New(), WorkflowID: "feature_change", Repo: "group/project",
		PullRequestURL: &u, InstallationRef: &ref, WorkflowSpec: specBytes}
}

func mmPollMeta(prURL string) reviewMergeMeta {
	return reviewMergeMeta{prURL: prURL, actorLogin: mergeReconcilerActor, actorKind: audit.ActorSystem}
}

func mmWebhookMeta(prURL, login string) reviewMergeMeta {
	return reviewMergeMeta{prURL: prURL, actorLogin: login, actorKind: audit.ActorKind("user")}
}

// TestMergeSettledMembers_Verdicts drives mergeSettledMembersAdmit directly
// over every rung (#4132). Each HOLD row asserts false AND exactly one
// refusal row naming the reason; each ADMIT row asserts true and zero rows.
// The forge ladder is the production one: cfg.PRStateReader stays nil.
func TestMergeSettledMembers_Verdicts(t *testing.T) {
	const listed = `["github:kuhlman-labs"]`
	const bot = "fishhawk-dev[bot]"
	type row struct {
		name        string
		gitlab      bool
		spec        []byte // nil: membersSpecYAML(membersLine)
		membersLine string
		noSpec      bool
		stageState  run.StageState
		// github
		ghNil     bool
		noInstall bool
		gh        mmGitHub
		// gitlab
		gl          mmGitLab
		resolverErr bool
		meta        *reviewMergeMeta
		wantAdmit   bool
		wantReason  string
		zeroCalls   bool
		check       func(t *testing.T, p map[string]any)
	}
	webhook := func(login string, gitlab bool) *reviewMergeMeta {
		u := mmGitHubPRURL
		if gitlab {
			u = mmGitLabPRURL
		}
		m := mmWebhookMeta(u, login)
		return &m
	}
	unlistedOnly := mmGitHub{merger: bot, reviews: []mmReview{{"alice", "APPROVED"}}}
	rows := []row{
		// No-forge rows.
		{name: "01 gate with no members admits", membersLine: "", gh: unlistedOnly, wantAdmit: true, zeroCalls: true},
		{name: "02 empty members admits", membersLine: "[]", gh: unlistedOnly, wantAdmit: true, zeroCalls: true},
		{name: "03 no cached spec admits", noSpec: true, gh: unlistedOnly, wantAdmit: true, zeroCalls: true},
		{name: "04 spec without a review stage admits", spec: []byte(mcvUndelegatedSpecYAML), gh: unlistedOnly, wantAdmit: true, zeroCalls: true},
		{name: "05 unparseable spec holds members_unevaluable", spec: []byte("version: [\n"), gh: unlistedOnly, wantReason: membersHoldUnevaluable, zeroCalls: true},
		{name: "06 workflow missing from spec holds members_unevaluable", spec: []byte(`version: "0.5"
workflows:
  other_workflow:
    stages:
      - id: plan
        type: plan
        executor:
          agent: claude-code
        produces:
          - artifact: plan
            schema: standard_v1
`), gh: unlistedOnly, wantReason: membersHoldUnevaluable, zeroCalls: true},
		// GitHub rows.
		{name: "07 listed APPROVED reviewer, bot merger admits", membersLine: listed,
			gh: mmGitHub{merger: bot, reviews: []mmReview{{"kuhlman-labs", "APPROVED"}}}, wantAdmit: true},
		{name: "08 listed merger, zero reviews admits", membersLine: listed,
			gh: mmGitHub{merger: "kuhlman-labs"}, wantAdmit: true},
		{name: "09 only unlisted approver holds no_listed_identity", membersLine: listed, gh: unlistedOnly,
			wantReason: membersHoldNoListedIdentity, check: func(t *testing.T, p map[string]any) {
				if fmt.Sprint(p["approvers"]) != "[github:alice]" || p["merger"] != "github:"+bot || p["merger_source"] != mergerSourceForge {
					t.Errorf("payload = %+v, want approvers [github:alice], merger github:%s, merger_source forge", p, bot)
				}
				if fmt.Sprint(p["members"]) != "[github:kuhlman-labs]" || p["forge"] != "github" {
					t.Errorf("payload members/forge = %v/%v", p["members"], p["forge"])
				}
			}},
		{name: "10 listed APPROVED then DISMISSED holds", membersLine: listed,
			gh:         mmGitHub{merger: bot, reviews: []mmReview{{"kuhlman-labs", "APPROVED"}, {"kuhlman-labs", "DISMISSED"}}},
			wantReason: membersHoldNoListedIdentity},
		{name: "11 listed APPROVED then COMMENTED admits", membersLine: listed,
			gh:        mmGitHub{merger: bot, reviews: []mmReview{{"kuhlman-labs", "APPROVED"}, {"kuhlman-labs", "COMMENTED"}}},
			wantAdmit: true},
		{name: "12 listed APPROVED then CHANGES_REQUESTED holds", membersLine: listed,
			gh:         mmGitHub{merger: bot, reviews: []mmReview{{"kuhlman-labs", "APPROVED"}, {"kuhlman-labs", "CHANGES_REQUESTED"}}},
			wantReason: membersHoldNoListedIdentity},
		{name: "13 plain member is qualified and admits", membersLine: `["kuhlman-labs"]`,
			gh: mmGitHub{merger: bot, reviews: []mmReview{{"kuhlman-labs", "APPROVED"}}}, wantAdmit: true},
		{name: "14 byte-exact member comparison holds", membersLine: `["github:Kuhlman-Labs"]`,
			gh:         mmGitHub{merger: bot, reviews: []mmReview{{"kuhlman-labs", "APPROVED"}}},
			wantReason: membersHoldNoListedIdentity},
		{name: "15 no GitHub client holds identity_unavailable", membersLine: listed, ghNil: true,
			wantReason: membersHoldIdentityUnavailable, zeroCalls: true},
		{name: "16 no GitHub client, listed webhook user admits via fallback", membersLine: listed, ghNil: true,
			meta: webhook("kuhlman-labs", false), wantAdmit: true, zeroCalls: true},
		{name: "17 no installation holds identity_unavailable", membersLine: listed, noInstall: true, gh: unlistedOnly,
			wantReason: membersHoldIdentityUnavailable},
		{name: "18 PR read 500, unlisted approver holds identity_unavailable", membersLine: listed,
			gh:         mmGitHub{prStatus: 500, reviews: []mmReview{{"alice", "APPROVED"}}},
			wantReason: membersHoldIdentityUnavailable},
		{name: "19 PR read 500, listed webhook user admits via fallback", membersLine: listed,
			gh: mmGitHub{prStatus: 500}, meta: webhook("kuhlman-labs", false), wantAdmit: true},
		{name: "20 reviews 500, unlisted merger holds identity_unavailable", membersLine: listed,
			gh: mmGitHub{merger: bot, reviewsStatus: 500}, wantReason: membersHoldIdentityUnavailable},
		{name: "21 reviews 500, listed merger admits", membersLine: listed,
			gh: mmGitHub{merger: "kuhlman-labs", reviewsStatus: 500}, wantAdmit: true},
		{name: "22 reviews truncated, unlisted merger holds identity_unavailable", membersLine: listed,
			gh: mmGitHub{merger: bot, reviewsTruncated: true}, wantReason: membersHoldIdentityUnavailable},
		{name: "23 terminal review stage admits without a forge read", membersLine: listed, stageState: run.StageStateSucceeded,
			gh: unlistedOnly, wantAdmit: true, zeroCalls: true},
		// GitLab rows.
		{name: "24 gitlab poll, listed forge merger admits", gitlab: true, membersLine: `["alice"]`,
			gl: mmGitLab{body: gitLabMRReadBody("alice", "")}, wantAdmit: true},
		{name: "25 gitlab poll, unlisted forge merger holds no_listed_identity", gitlab: true, membersLine: `["gitlab:alice"]`,
			gl: mmGitLab{body: gitLabMRReadBody("bob", "")}, wantReason: membersHoldNoListedIdentity,
			check: func(t *testing.T, p map[string]any) {
				if p["merger"] != "gitlab:bob" || p["merger_source"] != mergerSourceForge || fmt.Sprint(p["approvers"]) != "[]" {
					t.Errorf("payload = %+v, want merger gitlab:bob, merger_source forge, approvers []", p)
				}
			}},
		{name: "26 gitlab webhook listed user, MR read 500 admits via fallback", gitlab: true, membersLine: `["gitlab:alice"]`,
			gl: mmGitLab{status: 500, body: `{"message":"boom"}`}, meta: webhook("alice", true), wantAdmit: true},
		{name: "27 gitlab webhook listed user, forge merger unlisted holds (forge first)", gitlab: true, membersLine: `["gitlab:alice"]`,
			gl: mmGitLab{body: gitLabMRReadBody("bob", "")}, meta: webhook("alice", true), wantReason: membersHoldNoListedIdentity},
		{name: "28 gitlab poll, MR read 500 holds identity_unavailable", gitlab: true, membersLine: `["gitlab:alice"]`,
			gl: mmGitLab{status: 500, body: `{"message":"boom"}`}, wantReason: membersHoldIdentityUnavailable,
			check: func(t *testing.T, p map[string]any) {
				if p["merger"] != "" || p["merger_source"] != "" {
					t.Errorf("payload merger = %v/%v, want empty (the system actor is never an identity)", p["merger"], p["merger_source"])
				}
			}},
		{name: "29 gitlab poll, merged MR with no merger holds identity_unavailable", gitlab: true, membersLine: `["gitlab:alice"]`,
			gl: mmGitLab{body: gitLabMRReadBody("", "")}, wantReason: membersHoldIdentityUnavailable},
		{name: "30 gitlab run vs github member holds (cross-forge)", gitlab: true, membersLine: `["github:alice"]`,
			gl: mmGitLab{body: gitLabMRReadBody("alice", "")}, wantReason: membersHoldNoListedIdentity},
		{name: "31 gitlab poll, forge resolver error holds identity_unavailable", gitlab: true, membersLine: `["gitlab:alice"]`,
			resolverErr: true, wantReason: membersHoldIdentityUnavailable, zeroCalls: true},
	}
	for _, tc := range rows {
		t.Run(tc.name, func(t *testing.T) {
			specBytes := tc.spec
			if specBytes == nil && !tc.noSpec {
				specBytes = membersSpecYAML(t, tc.membersLine)
			}
			ar := &membersAuditRepo{}
			cfg := Config{Addr: "127.0.0.1:0", RunRepo: &prEventsRunRepo{}, AuditRepo: ar}
			gh := &membersGitHubStub{mmGitHub: tc.gh}
			gl := &membersGitLabStub{mmGitLab: tc.gl}
			var target *run.Run
			var meta reviewMergeMeta
			if tc.gitlab {
				target = mmGitLabRun(specBytes)
				meta = mmPollMeta(mmGitLabPRURL)
				f := newMembersGitLabForge(t, gl)
				cfg.ForgeResolver = func(id string) (forge.Forge, error) {
					if tc.resolverErr {
						return nil, errors.New("forge registry unavailable")
					}
					if id != "gitlab" {
						return nil, fmt.Errorf("unexpected forge %q", id)
					}
					return f, nil
				}
			} else {
				target = mmGitHubRun(specBytes)
				meta = mmPollMeta(mmGitHubPRURL)
				if tc.noInstall {
					target.InstallationID = nil
				}
				if !tc.ghNil {
					cfg.GitHub = newMembersGitHubClient(t, gh)
				}
			}
			if tc.meta != nil {
				meta = *tc.meta
			}
			state := tc.stageState
			if state == "" {
				state = run.StageStateAwaitingApproval
			}
			reviewStage := &run.Stage{ID: uuid.New(), RunID: target.ID, Type: run.StageTypeReview, State: state}
			s := New(cfg)

			got := s.mergeSettledMembersAdmit(context.Background(), target, reviewStage, meta)

			rows := refusalRows(t, ar.appended)
			if tc.wantAdmit {
				if !got {
					t.Fatalf("admit = false, want true; refusal rows %+v", rows)
				}
				if len(rows) != 0 {
					t.Errorf("refusal rows = %d, want 0 on an admit: %+v", len(rows), rows)
				}
			} else {
				if got {
					t.Fatalf("admit = true, want a %s hold", tc.wantReason)
				}
				if len(rows) != 1 {
					t.Fatalf("refusal rows = %d, want exactly 1", len(rows))
				}
				if rows[0]["reason"] != tc.wantReason {
					t.Errorf("payload.reason = %v, want %s (payload %+v)", rows[0]["reason"], tc.wantReason, rows[0])
				}
				if rows[0]["stage_id"] != reviewStage.ID.String() {
					t.Errorf("payload.stage_id = %v, want %s", rows[0]["stage_id"], reviewStage.ID)
				}
				if tc.check != nil {
					tc.check(t, rows[0])
				}
			}
			if tc.zeroCalls {
				if n := gh.calls() + gl.count(); n != 0 {
					t.Errorf("forge calls = %d, want 0", n)
				}
			}
		})
	}
}

// TestMergeSettledMembers_AuditDedup pins the refusal row's dedup: the
// reconciler re-polls a parked stage every tick, so an identical hold appends
// no second row, a changed identity set appends a new one, and an audit-list
// error appends anyway (a duplicate is preferred to a silent hold).
func TestMergeSettledMembers_AuditDedup(t *testing.T) {
	newFixture := func(t *testing.T, ar *membersAuditRepo) (*Server, *membersGitHubStub, *run.Run, *run.Stage) {
		gh := &membersGitHubStub{mmGitHub: mmGitHub{merger: "fishhawk-dev[bot]", reviews: []mmReview{{"alice", "APPROVED"}}}}
		s := New(Config{Addr: "127.0.0.1:0", RunRepo: &prEventsRunRepo{}, AuditRepo: ar, GitHub: newMembersGitHubClient(t, gh)})
		target := mmGitHubRun(membersSpecYAML(t, `["github:kuhlman-labs"]`))
		st := &run.Stage{ID: uuid.New(), RunID: target.ID, Type: run.StageTypeReview, State: run.StageStateAwaitingApproval}
		return s, gh, target, st
	}
	meta := mmPollMeta(mmGitHubPRURL)

	t.Run("identical holds dedup; a new approver appends", func(t *testing.T) {
		ar := &membersAuditRepo{}
		s, gh, target, st := newFixture(t, ar)
		for i := 0; i < 2; i++ {
			if s.mergeSettledMembersAdmit(context.Background(), target, st, meta) {
				t.Fatal("admit = true, want a hold")
			}
		}
		if n := len(refusalRows(t, ar.appended)); n != 1 {
			t.Fatalf("refusal rows after two identical holds = %d, want 1", n)
		}
		gh.setReviews([]mmReview{{"alice", "APPROVED"}, {"bob", "APPROVED"}})
		if s.mergeSettledMembersAdmit(context.Background(), target, st, meta) {
			t.Fatal("admit = true, want a hold")
		}
		if n := len(refusalRows(t, ar.appended)); n != 2 {
			t.Fatalf("refusal rows after a changed identity set = %d, want 2", n)
		}
	})

	t.Run("audit list error still appends", func(t *testing.T) {
		ar := &membersAuditRepo{listErr: errors.New("audit list boom")}
		s, _, target, st := newFixture(t, ar)
		for i := 0; i < 2; i++ {
			if s.mergeSettledMembersAdmit(context.Background(), target, st, meta) {
				t.Fatal("admit = true, want a hold")
			}
		}
		if n := len(refusalRows(t, ar.appended)); n != 2 {
			t.Fatalf("refusal rows = %d, want 2 (a list error must not suppress the append)", n)
		}
	})

	t.Run("audit append error still holds", func(t *testing.T) {
		ar := &membersAuditRepo{appendErr: errors.New("audit append boom")}
		s, _, target, st := newFixture(t, ar)
		if s.mergeSettledMembersAdmit(context.Background(), target, st, meta) {
			t.Fatal("admit = true after a failed refusal append, want a hold")
		}
		if ar.refusalAttempts != 1 {
			t.Fatalf("refusal append attempts = %d, want 1 (the append-failure branch was not reached)", ar.refusalAttempts)
		}
		if n := len(refusalRows(t, ar.appended)); n != 0 {
			t.Fatalf("refusal rows recorded = %d, want 0 (the append failed)", n)
		}
	})
}

// TestResolveReviewStageOnMerge_MembersAppendFailureStillHolds drives the
// reconciler poll end to end with the refusal append failing: the merge must
// still HOLD — review stage parked at awaiting_approval, no transition, no
// pr_merged row (which the fake WOULD record, since only the refusal category
// fails). An audit outage on the refusal row never flips the gate to admit.
func TestResolveReviewStageOnMerge_MembersAppendFailureStillHolds(t *testing.T) {
	gh := &membersGitHubStub{mmGitHub: mmGitHub{merger: "fishhawk-dev[bot]", reviews: []mmReview{{"alice", "APPROVED"}}}}
	target := mmGitHubRun(membersSpecYAML(t, `["github:kuhlman-labs"]`))
	reviewStageID := uuid.New()
	rr := &prEventsRunRepo{
		listResult: []*run.Run{target},
		stages: map[uuid.UUID][]*run.Stage{
			target.ID: {
				{ID: uuid.New(), RunID: target.ID, Type: run.StageTypeImplement, State: run.StageStateSucceeded},
				{ID: reviewStageID, RunID: target.ID, Type: run.StageTypeReview, State: run.StageStateAwaitingApproval},
			},
		},
	}
	ar := &membersAuditRepo{appendErr: errors.New("audit append boom")}
	s := New(Config{Addr: "127.0.0.1:0", RunRepo: rr, AuditRepo: ar, GitHub: newMembersGitHubClient(t, gh)})

	if err := s.ResolveReviewFromPollState(context.Background(), target.ID, true, mmGitHubPRURL); err != nil {
		t.Fatalf("poll: %v", err)
	}
	if ar.refusalAttempts != 1 {
		t.Fatalf("refusal append attempts = %d, want 1 (the members hold was not reached)", ar.refusalAttempts)
	}
	if got := stageStateByID(t, rr, target.ID, reviewStageID); got != run.StageStateAwaitingApproval {
		t.Errorf("review stage = %s, want awaiting_approval (held despite the failed append)", got)
	}
	if len(rr.transitions) != 0 {
		t.Errorf("transitions = %+v, want none", rr.transitions)
	}
	if findCategory(ar.appended, CategoryPRMerged) != nil {
		t.Errorf("pr_merged written after a failed refusal append; categories %v", auditCategories(ar.appended))
	}
}
