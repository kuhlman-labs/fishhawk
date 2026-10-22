package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/identity"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

// faSpec renders a workflow-v2 document whose review stage declares an
// approval gate with the given approvals block (indented under `approvals:`),
// plus an optional workflow-level escalations block.
func faSpec(approvals, escalations string) []byte {
	return []byte(`version: "2"
workflows:
  feature_change:
    autonomy: high
` + escalations + `    stages:
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
` + approvals)
}

const (
	faCount1          = "              count: 1\n"
	faHead            = "h2"
	faEscAddGroup     = "    escalations:\n      - match:\n          trigger: [diff]\n        require:\n          approvals:\n            member_of: acme/security\n"
	faEscRaiseToAdmin = "    escalations:\n      - match:\n          trigger: [diff]\n        require:\n          approvals:\n            min_permission: admin\n"
)

var faAt = time.Date(2026, 10, 22, 9, 0, 0, 0, time.UTC)

// faUser is an identity-valid User reviewer node.
func faUser(id int64, state, commit, login string, uid int64) mcqReviewNode {
	return mcqReviewNode{ID: id, State: state, SubmittedAt: faAt.Add(time.Duration(id) * time.Second), Commit: commit,
		Author: mcqActor{Typename: "User", Login: login, ID: uid}}
}

// faSeed seeds a run on the ADR-110 non-Fishhawk fixture repository x/y at
// faHead with spec, and installs the PR's review snapshot.
func faSeed(t *testing.T, f *mcqFixture, spec []byte, fx mcqReviewSnapshot) *mcqRun {
	t.Helper()
	r := f.seed("fa", faHead, spec)
	if fx.Author.Typename == "" {
		fx.Author = mcqActor{Typename: "User", Login: "pr-opener", ID: 1}
	}
	f.gh.setReviewSnapshot(r.pr, fx)
	return r
}

func faFold(t *testing.T, f *mcqFixture, r *mcqRun, head string) forgeApprovalDecision {
	t.Helper()
	d, err := f.srv.forgeApprovalFold(context.Background(), f.runRow(r), head)
	if err != nil {
		t.Fatalf("fold: %v", err)
	}
	return d
}

func faAppend(t *testing.T, f *mcqFixture, r *mcqRun, category string, kind audit.ActorKind, subject string, payload map[string]any) {
	t.Helper()
	raw, _ := json.Marshal(payload)
	if _, err := f.audit.AppendChained(context.Background(), audit.ChainAppendParams{RunID: r.id, Timestamp: time.Now().UTC(),
		Category: category, ActorKind: &kind, ActorSubject: &subject, Payload: raw}); err != nil {
		t.Fatalf("append %s: %v", category, err)
	}
}

func faCleanAdvance(t *testing.T, f *mcqFixture, r *mcqRun, prior, next string, clean bool) {
	t.Helper()
	merge := next
	if !clean {
		merge = "m-" + next // fix-up shaped: new head is not the merge commit
	}
	faAppend(t, f, r, CategoryBranchRebased, audit.ActorSystem, "fishhawk", map[string]any{
		"prior_head_sha": prior, "new_head_sha": next, "merge_commit_sha": merge, "clean_verified": true})
}

// TestForgeApprovalFold_Table drives one row per fold result and failure mode
// through the PRODUCTION githubclient against the extended mcqGitHub fake.
func TestForgeApprovalFold_Table(t *testing.T) {
	type row struct {
		name      string
		approvals string
		esc       string
		snap      mcqReviewSnapshot
		perms     map[string][2]string // login -> {role_name, permission}
		permFail  []string
		setup     func(t *testing.T, f *mcqFixture, r *mcqRun)
		head      string
		want      string
		reason    string
		next      string
		basis     string
		chain     []string
		voucher   bool
	}
	members := func(m string) string { return faCount1 + "              members: [\"" + m + "\"]\n" }
	notAuthor := "              not: [author]\n"
	rows := []row{
		{name: "write non-member on members-less gate at head approves", approvals: faCount1,
			snap:  mcqReviewSnapshot{Reviews: []mcqReviewNode{faUser(1, "APPROVED", faHead, "wally", 10)}},
			perms: map[string][2]string{"wally": {"write", "write"}}, want: forgeApprovalApproved, basis: forgeApprovalBasisLive},
		{name: "read member at head is absent (floor)", approvals: members("alice"),
			snap:  mcqReviewSnapshot{Reviews: []mcqReviewNode{faUser(1, "APPROVED", faHead, "alice", 10)}},
			perms: map[string][2]string{"alice": {"read", "read"}}, want: forgeApprovalAbsent, next: forgeApprovalNextApprovePR},
		{name: "triage is absent", approvals: faCount1,
			snap:  mcqReviewSnapshot{Reviews: []mcqReviewNode{faUser(1, "APPROVED", faHead, "tri", 10)}},
			perms: map[string][2]string{"tri": {"triage", "read"}}, want: forgeApprovalAbsent},
		{name: "none on a public repo is absent", approvals: faCount1,
			snap:  mcqReviewSnapshot{Reviews: []mcqReviewNode{faUser(1, "APPROVED", faHead, "nobody", 10)}},
			perms: map[string][2]string{"nobody": {"", "none"}}, want: forgeApprovalAbsent},
		{name: "none on a private repo is undetermined", approvals: faCount1,
			snap:  mcqReviewSnapshot{IsPrivate: true, Reviews: []mcqReviewNode{faUser(1, "APPROVED", faHead, "nobody", 10)}},
			perms: map[string][2]string{"nobody": {"", "none"}}, want: forgeApprovalUndetermined, reason: forgeApprovalReasonApprovalUndetermine},
		{name: "permission 404 is undetermined", approvals: faCount1,
			snap: mcqReviewSnapshot{Reviews: []mcqReviewNode{faUser(1, "APPROVED", faHead, "ghostly", 10)}},
			want: forgeApprovalUndetermined, next: forgeApprovalNextRetryAfter},
		{name: "permission 500 is undetermined", approvals: faCount1,
			snap:  mcqReviewSnapshot{Reviews: []mcqReviewNode{faUser(1, "APPROVED", faHead, "wally", 10)}},
			perms: map[string][2]string{"wally": {"write", "write"}}, permFail: []string{"wally"}, want: forgeApprovalUndetermined},
		{name: "no installation is undetermined", approvals: faCount1,
			snap:  mcqReviewSnapshot{Reviews: []mcqReviewNode{faUser(1, "APPROVED", faHead, "wally", 10)}},
			perms: map[string][2]string{"wally": {"write", "write"}},
			setup: func(_ *testing.T, f *mcqFixture, r *mcqRun) {
				f.exec(`UPDATE runs SET installation_id = NULL WHERE id = $1`, r.id)
			}, want: forgeApprovalUndetermined, reason: forgeApprovalReasonRunUnaddressable},
		{name: "a Bot reviewer is not counted", approvals: faCount1,
			snap: mcqReviewSnapshot{Reviews: []mcqReviewNode{{ID: 1, State: "APPROVED", SubmittedAt: faAt, Commit: faHead,
				Author: mcqActor{Typename: "Bot", Login: "wally"}}}},
			perms: map[string][2]string{"wally": {"write", "write"}}, want: forgeApprovalAbsent},
		{name: "a [bot] login is not counted", approvals: faCount1,
			snap:  mcqReviewSnapshot{Reviews: []mcqReviewNode{faUser(1, "APPROVED", faHead, "ci[bot]", 10)}},
			perms: map[string][2]string{"ci[bot]": {"write", "write"}}, want: forgeApprovalAbsent},
		{name: "ghost is not counted", approvals: faCount1,
			snap:  mcqReviewSnapshot{Reviews: []mcqReviewNode{faUser(1, "APPROVED", faHead, "ghost", 10)}},
			perms: map[string][2]string{"ghost": {"write", "write"}}, want: forgeApprovalAbsent},
		{name: "a null author is not counted", approvals: faCount1,
			snap: mcqReviewSnapshot{Reviews: []mcqReviewNode{{ID: 1, State: "APPROVED", SubmittedAt: faAt, Commit: faHead}}},
			want: forgeApprovalAbsent},
		{name: "databaseId 0 is not counted", approvals: faCount1,
			snap:  mcqReviewSnapshot{Reviews: []mcqReviewNode{faUser(1, "APPROVED", faHead, "wally", 0)}},
			perms: map[string][2]string{"wally": {"write", "write"}}, want: forgeApprovalAbsent},
		{name: "older commit with no clean chain is absent", approvals: faCount1,
			snap:  mcqReviewSnapshot{Reviews: []mcqReviewNode{faUser(1, "APPROVED", "h0", "wally", 10)}},
			perms: map[string][2]string{"wally": {"write", "write"}}, want: forgeApprovalAbsent},
		{name: "older commit over a parent-verified chain is transferred", approvals: faCount1,
			snap:  mcqReviewSnapshot{Reviews: []mcqReviewNode{faUser(1, "APPROVED", "h0", "wally", 10)}},
			perms: map[string][2]string{"wally": {"write", "write"}},
			setup: func(t *testing.T, f *mcqFixture, r *mcqRun) {
				faCleanAdvance(t, f, r, "h0", "h1", true)
				faCleanAdvance(t, f, r, "h1", faHead, true)
			}, want: forgeApprovalApproved, basis: forgeApprovalBasisTransferred, chain: []string{"h0", "h1", faHead}},
		{name: "a fix-up-shaped advance breaks the chain", approvals: faCount1,
			snap:  mcqReviewSnapshot{Reviews: []mcqReviewNode{faUser(1, "APPROVED", "h0", "wally", 10)}},
			perms: map[string][2]string{"wally": {"write", "write"}},
			setup: func(t *testing.T, f *mcqFixture, r *mcqRun) {
				faCleanAdvance(t, f, r, "h0", "h1", true)
				faCleanAdvance(t, f, r, "h1", faHead, false)
			}, want: forgeApprovalAbsent},
		{name: "a write non-member on a members gate is absent", approvals: members("github:alice"),
			snap:  mcqReviewSnapshot{Reviews: []mcqReviewNode{faUser(1, "APPROVED", faHead, "wally", 10)}},
			perms: map[string][2]string{"wally": {"write", "write"}}, want: forgeApprovalAbsent},
		{name: "a listed write member approves", approvals: members("github:alice"),
			snap:  mcqReviewSnapshot{Reviews: []mcqReviewNode{faUser(1, "APPROVED", faHead, "alice", 10)}},
			perms: map[string][2]string{"alice": {"write", "write"}}, want: forgeApprovalApproved},
		{name: "min_permission admin with write is absent",
			approvals: faCount1 + "              min_permission: admin\n",
			snap:      mcqReviewSnapshot{Reviews: []mcqReviewNode{faUser(1, "APPROVED", faHead, "wally", 10)}},
			perms:     map[string][2]string{"wally": {"write", "write"}}, want: forgeApprovalAbsent},
		{name: "member_of with no identity provider is provider_unsupported",
			approvals: faCount1 + "              member_of: acme/team\n",
			snap:      mcqReviewSnapshot{Reviews: []mcqReviewNode{faUser(1, "APPROVED", faHead, "wally", 10)}},
			want:      forgeApprovalProviderUnsupported, next: forgeApprovalNextConfigureIdentityProvider},
		{name: "member_of resolution error is undetermined",
			approvals: faCount1 + "              member_of: acme/team\n",
			snap:      mcqReviewSnapshot{Reviews: []mcqReviewNode{faUser(1, "APPROVED", faHead, "wally", 10)}},
			perms:     map[string][2]string{"wally": {"write", "write"}},
			setup: func(_ *testing.T, f *mcqFixture, _ *mcqRun) {
				f.srv.cfg.IdentityProviders = map[string]identity.IdentityProvider{"github": &fakeIdentityProvider{memberErr: errors.New("org read 502")}}
			}, want: forgeApprovalUndetermined},
		{name: "member_of satisfied approves",
			approvals: faCount1 + "              member_of: acme/team\n",
			snap:      mcqReviewSnapshot{Reviews: []mcqReviewNode{faUser(1, "APPROVED", faHead, "wally", 10)}},
			perms:     map[string][2]string{"wally": {"write", "write"}},
			setup: func(_ *testing.T, f *mcqFixture, _ *mcqRun) {
				f.srv.cfg.IdentityProviders = map[string]identity.IdentityProvider{"github": &fakeIdentityProvider{member: true}}
			}, want: forgeApprovalApproved},
		{name: "a GitLab-ref run is provider_unsupported with no next step", approvals: faCount1,
			snap: mcqReviewSnapshot{Reviews: []mcqReviewNode{faUser(1, "APPROVED", faHead, "wally", 10)}},
			setup: func(_ *testing.T, f *mcqFixture, r *mcqRun) {
				f.exec(`UPDATE runs SET installation_ref = 'gitlab:5' WHERE id = $1`, r.id)
			}, want: forgeApprovalProviderUnsupported, next: ""},
		{name: "a write CHANGES_REQUESTED vetoes", approvals: faCount1,
			snap: mcqReviewSnapshot{Reviews: []mcqReviewNode{faUser(1, "APPROVED", faHead, "wally", 10),
				faUser(2, "CHANGES_REQUESTED", faHead, "vera", 11)}},
			perms: map[string][2]string{"wally": {"write", "write"}, "vera": {"write", "write"}},
			want:  forgeApprovalVetoed, next: forgeApprovalNextDismissReview},
		{name: "a read CHANGES_REQUESTED is ignored", approvals: faCount1,
			snap: mcqReviewSnapshot{Reviews: []mcqReviewNode{faUser(1, "APPROVED", faHead, "wally", 10),
				faUser(2, "CHANGES_REQUESTED", faHead, "rita", 11)}},
			perms: map[string][2]string{"wally": {"write", "write"}, "rita": {"read", "read"}}, want: forgeApprovalApproved},
		{name: "C3: min_permission admin, a write CHANGES_REQUESTED does not veto",
			approvals: faCount1 + "              min_permission: admin\n",
			snap: mcqReviewSnapshot{Reviews: []mcqReviewNode{faUser(1, "APPROVED", faHead, "ada", 10),
				faUser(2, "CHANGES_REQUESTED", faHead, "vera", 11)}},
			perms: map[string][2]string{"ada": {"admin", "admin"}, "vera": {"write", "write"}}, want: forgeApprovalApproved},
		{name: "an unresolvable veto is undetermined", approvals: faCount1,
			snap: mcqReviewSnapshot{Reviews: []mcqReviewNode{faUser(1, "APPROVED", faHead, "wally", 10),
				faUser(2, "CHANGES_REQUESTED", faHead, "vera", 11)}},
			perms:    map[string][2]string{"wally": {"write", "write"}, "vera": {"write", "write"}},
			permFail: []string{"vera"}, want: forgeApprovalUndetermined, reason: forgeApprovalReasonVetoUndetermined},
		{name: "latest DISMISSED and no qualifying approval is dismissed", approvals: faCount1,
			snap: mcqReviewSnapshot{Reviews: []mcqReviewNode{faUser(1, "DISMISSED", "h0", "wally", 10)}},
			want: forgeApprovalDismissed, next: forgeApprovalNextApprovePR},
		{name: "REVIEW_REQUIRED with a qualifying approval is review_required", approvals: faCount1,
			snap: mcqReviewSnapshot{ReviewDecision: "REVIEW_REQUIRED",
				Reviews: []mcqReviewNode{faUser(1, "APPROVED", faHead, "wally", 10)}},
			perms: map[string][2]string{"wally": {"write", "write"}}, want: forgeApprovalReviewRequired, next: forgeApprovalNextApprovePR},
		{name: "a truncated listing is reviews_truncated", approvals: faCount1,
			snap: mcqReviewSnapshot{Truncated: true, Reviews: []mcqReviewNode{faUser(1, "APPROVED", faHead, "wally", 10)}},
			want: forgeApprovalReviewsTruncated, next: forgeApprovalNextRetryAfter},
		{name: "voucher exception applies at members=={V}, count 1", approvals: members("vee") + notAuthor,
			snap:  mcqReviewSnapshot{Reviews: []mcqReviewNode{faUser(1, "APPROVED", faHead, "vee", 10)}},
			perms: map[string][2]string{"vee": {"write", "write"}},
			setup: func(t *testing.T, f *mcqFixture, r *mcqRun) {
				faAppend(t, f, r, CategoryOperatorCommitVouched, audit.ActorUser, "github:vee", map[string]any{"vouched_sha": faHead})
			}, want: forgeApprovalApproved, voucher: true},
		{name: "voucher exception refused at count 2",
			approvals: "              count: 2\n              members: [\"vee\"]\n" + notAuthor,
			snap:      mcqReviewSnapshot{Reviews: []mcqReviewNode{faUser(1, "APPROVED", faHead, "vee", 10)}},
			perms:     map[string][2]string{"vee": {"write", "write"}},
			setup: func(t *testing.T, f *mcqFixture, r *mcqRun) {
				faAppend(t, f, r, CategoryOperatorCommitVouched, audit.ActorUser, "github:vee", map[string]any{"vouched_sha": faHead})
			}, want: forgeApprovalAbsent},
		{name: "voucher exception refused once a group escalation fired", approvals: members("vee") + notAuthor, esc: faEscAddGroup,
			snap:  mcqReviewSnapshot{Reviews: []mcqReviewNode{faUser(1, "APPROVED", faHead, "vee", 10)}},
			perms: map[string][2]string{"vee": {"write", "write"}},
			setup: func(t *testing.T, f *mcqFixture, r *mcqRun) {
				f.srv.cfg.IdentityProviders = map[string]identity.IdentityProvider{"github": &fakeIdentityProvider{member: true}}
				faAppend(t, f, r, CategoryOperatorCommitVouched, audit.ActorUser, "github:vee", map[string]any{"vouched_sha": faHead})
			}, want: forgeApprovalAbsent},
		{name: "voucher exception refused on a members-less gate", approvals: faCount1 + notAuthor,
			snap:  mcqReviewSnapshot{Reviews: []mcqReviewNode{faUser(1, "APPROVED", faHead, "vee", 10)}},
			perms: map[string][2]string{"vee": {"write", "write"}},
			setup: func(t *testing.T, f *mcqFixture, r *mcqRun) {
				faAppend(t, f, r, CategoryOperatorCommitVouched, audit.ActorUser, "github:vee", map[string]any{"vouched_sha": faHead})
			}, want: forgeApprovalAbsent},
		{name: "the PR opener User is excluded under not: author", approvals: faCount1 + notAuthor,
			snap: mcqReviewSnapshot{Author: mcqActor{Typename: "User", Login: "olly", ID: 3},
				Reviews: []mcqReviewNode{faUser(1, "APPROVED", faHead, "olly", 3)}},
			perms: map[string][2]string{"olly": {"write", "write"}}, want: forgeApprovalAbsent},
		{name: "a Bot opener is irrelevant under not: author", approvals: faCount1 + notAuthor,
			snap: mcqReviewSnapshot{Author: mcqActor{Typename: "Bot", Login: "fishhawk-dev[bot]"},
				Reviews: []mcqReviewNode{faUser(1, "APPROVED", faHead, "wally", 10)}},
			perms: map[string][2]string{"wally": {"write", "write"}}, want: forgeApprovalApproved},
		{name: "an escalation raising min_permission to admin is applied", approvals: faCount1, esc: faEscRaiseToAdmin,
			snap:  mcqReviewSnapshot{Reviews: []mcqReviewNode{faUser(1, "APPROVED", faHead, "wally", 10)}},
			perms: map[string][2]string{"wally": {"write", "write"}}, want: forgeApprovalAbsent},
		{name: "a moved head is undetermined head_moved", approvals: faCount1, head: "h1",
			snap:  mcqReviewSnapshot{Reviews: []mcqReviewNode{faUser(1, "APPROVED", faHead, "wally", 10)}},
			perms: map[string][2]string{"wally": {"write", "write"}}, want: forgeApprovalUndetermined, reason: forgeApprovalReasonHeadMoved},
		{name: "an empty verified head is undetermined", approvals: faCount1, head: "-",
			snap: mcqReviewSnapshot{Reviews: []mcqReviewNode{faUser(1, "APPROVED", faHead, "wally", 10)}},
			want: forgeApprovalUndetermined, reason: forgeApprovalReasonHeadUnknown},
		{name: "a merged pull request is undetermined and flagged", approvals: faCount1,
			snap: mcqReviewSnapshot{Reviews: []mcqReviewNode{faUser(1, "APPROVED", faHead, "wally", 10)}},
			setup: func(_ *testing.T, f *mcqFixture, r *mcqRun) {
				f.gh.set(func() { f.gh.prs[r.pr].merged = true })
			}, want: forgeApprovalUndetermined, reason: forgeApprovalReasonMerged},
	}
	for _, tc := range rows {
		t.Run(tc.name, func(t *testing.T) {
			f := newMCQFixture(t)
			r := faSeed(t, f, faSpec(tc.approvals, tc.esc), tc.snap)
			for login, p := range tc.perms {
				f.gh.setPermission(login, p[0], p[1])
			}
			for _, login := range tc.permFail {
				f.gh.set(func() { f.gh.permStatus[login] = http.StatusInternalServerError })
			}
			if tc.setup != nil {
				tc.setup(t, f, r)
			}
			head := faHead
			switch tc.head {
			case "-":
				head = ""
			case "":
			default:
				head = tc.head
			}
			d := faFold(t, f, r, head)
			if !d.Declared || d.Result != tc.want {
				t.Fatalf("fold = %s/%s (%s), declared=%v; want %s", d.Result, d.Reason, d.Detail, d.Declared, tc.want)
			}
			if tc.reason != "" && d.Reason != tc.reason {
				t.Errorf("reason = %q, want %q", d.Reason, tc.reason)
			}
			if (tc.next != "" || tc.want == forgeApprovalProviderUnsupported) && d.NextStep != tc.next {
				t.Errorf("next_step = %q, want %q", d.NextStep, tc.next)
			}
			if tc.basis != "" && d.Basis != tc.basis {
				t.Errorf("basis = %q, want %q", d.Basis, tc.basis)
			}
			if tc.chain != nil && !reflect.DeepEqual(d.ChainHeads, tc.chain) {
				t.Errorf("chain = %v, want %v", d.ChainHeads, tc.chain)
			}
			if d.VoucherException != tc.voucher {
				t.Errorf("voucher_exception = %v, want %v", d.VoucherException, tc.voucher)
			}
			if tc.want == forgeApprovalApproved && len(d.Counted) == 0 {
				t.Error("approved with no counted approval")
			}
			if len(d.Detail) > forgeApprovalDetailMaxBytes {
				t.Errorf("detail %d bytes > cap", len(d.Detail))
			}
			if tc.want == forgeApprovalProviderUnsupported && f.gh.snapshotCalls() != 0 {
				t.Errorf("provider_unsupported made %d snapshot reads, want 0", f.gh.snapshotCalls())
			}
			if tc.reason == forgeApprovalReasonMerged && !d.Merged {
				t.Error("merged snapshot not flagged Merged")
			}
		})
	}
}

// TestForgeApprovalFold_PermissionCap: 11 candidates, the first 10 read and
// the 11th write, is undetermined with ZERO permission reads.
func TestForgeApprovalFold_PermissionCap(t *testing.T) {
	f := newMCQFixture(t)
	var nodes []mcqReviewNode
	for i := int64(101); i <= 111; i++ {
		login := "rev" + strconv.FormatInt(i, 10)
		nodes = append(nodes, faUser(i, "APPROVED", faHead, login, i))
		if i == 111 {
			f.gh.setPermission(login, "write", "write")
		} else {
			f.gh.setPermission(login, "read", "read")
		}
	}
	r := faSeed(t, f, faSpec(faCount1, ""), mcqReviewSnapshot{Reviews: nodes})
	d := faFold(t, f, r, faHead)
	if d.Result != forgeApprovalUndetermined || d.Reason != forgeApprovalReasonPermissionCap {
		t.Fatalf("fold = %s/%s, want undetermined/%s", d.Result, d.Reason, forgeApprovalReasonPermissionCap)
	}
}

// TestForgeApprovalFold_SpecPostures pins C4: a run that never recorded a spec
// declares no gate and makes no forge read; a recorded spec that reads back
// empty or does not parse is undetermined.
func TestForgeApprovalFold_SpecPostures(t *testing.T) {
	f := newMCQFixture(t)
	none := faSeed(t, f, nil, mcqReviewSnapshot{})
	d := faFold(t, f, none, faHead)
	if d.Declared || !d.approved() || d.Basis != forgeApprovalBasisNotRequired {
		t.Fatalf("spec-less fold = %+v, want not declared / not_required", d)
	}
	if n := f.gh.snapshotCalls(); n != 0 {
		t.Errorf("spec-less fold made %d snapshot reads, want 0", n)
	}
	for name, spec := range map[string][]byte{"unparseable": []byte("version: [\"2\"\nworkflows: {"), "empty": []byte("  \n")} {
		r := faSeed(t, f, spec, mcqReviewSnapshot{})
		d := faFold(t, f, r, faHead)
		if !d.Declared || d.Result != forgeApprovalUndetermined || d.Reason != forgeApprovalReasonSpecUndeterminable || d.approved() {
			t.Errorf("%s spec fold = %s/%s declared=%v, want undetermined/%s", name, d.Result, d.Reason, d.Declared, forgeApprovalReasonSpecUndeterminable)
		}
	}
	// A workflow with no review approval gate declares none.
	gateless := faSeed(t, f, []byte(strings.Replace(string(faSpec(faCount1, "")), "        gates:\n          - type: approval\n            approvals:\n              count: 1\n", "", 1)), mcqReviewSnapshot{})
	if d := faFold(t, f, gateless, faHead); d.Declared {
		t.Errorf("gate-less review fold declared=%v, want false", d.Declared)
	}
}

// TestForgeApprovalFold_SnapshotErrorAndReadOnce: a snapshot transport
// failure is the fold's ONLY error; each reviewer is read at most once.
func TestForgeApprovalFold_SnapshotErrorAndReadOnce(t *testing.T) {
	f := newMCQFixture(t)
	r := faSeed(t, f, faSpec(faCount1, ""), mcqReviewSnapshot{Reviews: []mcqReviewNode{
		faUser(1, "CHANGES_REQUESTED", "h0", "wally", 10), faUser(2, "APPROVED", faHead, "wally", 10)}})
	f.gh.setPermission("wally", "write", "write")
	f.gh.set(func() { f.gh.graphQLFail[r.pr] = 1 })
	if _, err := f.srv.forgeApprovalFold(context.Background(), f.runRow(r), faHead); err == nil {
		t.Fatal("snapshot 500 returned no error")
	}
	d := faFold(t, f, r, faHead)
	if d.Result != forgeApprovalApproved {
		t.Fatalf("latest opinion (APPROVED after CHANGES_REQUESTED) fold = %s/%s", d.Result, d.Reason)
	}
	if n := f.gh.permissionCalls("wally"); n != 1 {
		t.Errorf("wally permission reads = %d, want 1", n)
	}
}

func TestSanitizeForgeReadDetail(t *testing.T) {
	in := "Authorization: Bearer ghs_abc token github_pat_x leaked " + strings.Repeat("x", 2048) + "\nsecond line ghp_zzz"
	got := sanitizeForgeReadDetail("read_failed", in)
	if len(got) > forgeApprovalDetailMaxBytes {
		t.Errorf("len = %d, want <= %d", len(got), forgeApprovalDetailMaxBytes)
	}
	for _, bad := range []string{"ghs_abc", "github_pat_x", "\n", "second line", "ghp_zzz"} {
		if strings.Contains(got, bad) {
			t.Errorf("sanitised detail %q still carries %q", got, bad)
		}
	}
	if !strings.HasPrefix(got, "read_failed: ") || !strings.Contains(got, "[redacted]") {
		t.Errorf("sanitised detail = %q", got)
	}
	if s := sanitizeForgeReadDetail("", "bell\x07ok"); s != "bellok" {
		t.Errorf("control chars not stripped: %q", s)
	}
}

func TestForgeApprovalRefusal_WireContract(t *testing.T) {
	url := "https://github.com/x/y/pull/1"
	rr := &run.Run{PullRequestURL: &url}
	ref := forgeApprovalRefusal(forgeApprovalDecision{Declared: true, VerifiedHead: faHead, Result: forgeApprovalVetoed,
		Reason: forgeApprovalReasonVeto, Detail: "vetoed: x", NextStep: forgeApprovalNextDismissReview, VetoReviewer: "vera"}, rr)
	if ref.Code != mergeCodeForgeApprovalRequired || !strings.Contains(ref.Message, "vera") {
		t.Fatalf("refusal = %+v", ref)
	}
	for k, want := range map[string]string{"pr_url": url, "head_sha": faHead, "fold": forgeApprovalVetoed,
		"reason": forgeApprovalReasonVeto, "next_step": forgeApprovalNextDismissReview, "detail": "vetoed: x", "veto_reviewer": "vera"} {
		if ref.Details[k] != want {
			t.Errorf("details[%s] = %v, want %s", k, ref.Details[k], want)
		}
	}
	for _, next := range []string{forgeApprovalNextApprovePR, forgeApprovalNextRetryAfter, forgeApprovalNextConfigureIdentityProvider, ""} {
		if forgeApprovalNextStepProse(forgeApprovalDecision{NextStep: next}) == "" {
			t.Errorf("no prose for next step %q", next)
		}
	}
	if mergeCodeAuthorizationUnrecorded != "merge_authorization_unrecorded" {
		t.Error("503 code drifted")
	}
}

// failingAuthorizedAudit fails AppendChained for merge_queue_authorized only.
type failingAuthorizedAudit struct{ audit.Repository }

func (a failingAuthorizedAudit) AppendChained(ctx context.Context, p audit.ChainAppendParams) (*audit.Entry, error) {
	if p.Category == CategoryMergeQueueAuthorized {
		return nil, errors.New("audit down")
	}
	return a.Repository.AppendChained(ctx, p)
}

// TestWriteMergeQueueAuthorized reads the R4 payload back from the REAL audit
// repository, and pins the refusals and the append-error return.
func TestWriteMergeQueueAuthorized(t *testing.T) {
	f := newMCQFixture(t)
	r := faSeed(t, f, faSpec(faCount1, ""), mcqReviewSnapshot{Reviews: []mcqReviewNode{faUser(1, "APPROVED", "h0", "wally", 10)}})
	f.gh.setPermission("wally", "write", "write")
	faCleanAdvance(t, f, r, "h0", faHead, true)
	d := faFold(t, f, r, faHead)
	if d.Result != forgeApprovalApproved {
		t.Fatalf("fold = %s/%s", d.Result, d.Reason)
	}
	ctx := context.Background()
	seq, err := f.srv.writeMergeQueueAuthorized(ctx, f.runRow(r), d, forgeApprovalAuthorization{Path: "queue", IntentSequence: 7})
	if err != nil || seq == 0 {
		t.Fatalf("write = %d, %v", seq, err)
	}
	rows, err := f.audit.ListForRunByCategory(ctx, r.id, CategoryMergeQueueAuthorized)
	if err != nil || len(rows) != 1 || rows[0].Sequence != seq {
		t.Fatalf("rows = %d, %v", len(rows), err)
	}
	var p map[string]any
	if err := json.Unmarshal(rows[0].Payload, &p); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"predicate_snapshot", "counted_approvals", "observed_at", "chain_heads", "policy_version", "basis",
		"review_gate_approval_seq", "intent_sequence", "verify_pass", "voucher_exception", "path", "head_sha"} {
		if _, ok := p[k]; !ok {
			t.Errorf("payload lacks %q", k)
		}
	}
	ps := p["predicate_snapshot"].(map[string]any)
	for _, k := range []string{"count_required", "identity", "submitter_class", "auth_method", "channel", "min_permission",
		"member_of", "members", "escalated", "escalated_member_of"} {
		if _, ok := ps[k]; !ok {
			t.Errorf("predicate_snapshot lacks %q", k)
		}
	}
	if ps["channel"] != "forge_review" || ps["auth_method"] != "artifact" || ps["min_permission"] != "write" {
		t.Errorf("predicate_snapshot = %v", ps)
	}
	counted := p["counted_approvals"].([]any)[0].(map[string]any)
	if counted["login"] != "wally" || counted["permission"] != "write" || counted["commit"] != "h0" || counted["actor_id"] != float64(10) {
		t.Errorf("counted = %v", counted)
	}
	if p["basis"] != forgeApprovalBasisTransferred || p["intent_sequence"] != float64(7) || p["verify_pass"] != "not_required" {
		t.Errorf("basis/intent/verify = %v/%v/%v", p["basis"], p["intent_sequence"], p["verify_pass"])
	}
	if pv := p["policy_version"].(map[string]any); pv["workflow_spec_sha256"] == "" {
		t.Errorf("policy_version = %v", pv)
	}

	// A non-approved or undeclared decision is never written.
	if _, err := f.srv.writeMergeQueueAuthorized(ctx, f.runRow(r), forgeApprovalDecision{Declared: true, Result: forgeApprovalAbsent},
		forgeApprovalAuthorization{Path: "inline"}); err == nil {
		t.Error("an absent decision was written")
	}
	if _, err := f.srv.writeMergeQueueAuthorized(ctx, f.runRow(r), forgeApprovalDecision{Result: forgeApprovalApproved},
		forgeApprovalAuthorization{Path: "inline"}); err == nil {
		t.Error("an undeclared decision was written")
	}
	// The append error is RETURNED.
	f.srv.cfg.AuditRepo = failingAuthorizedAudit{f.audit}
	if _, err := f.srv.writeMergeQueueAuthorized(ctx, f.runRow(r), d, forgeApprovalAuthorization{Path: "inline"}); err == nil {
		t.Error("a failed append returned nil")
	}
	if rows, _ := f.audit.ListForRunByCategory(ctx, r.id, CategoryMergeQueueAuthorized); len(rows) != 1 {
		t.Errorf("rows after failed append = %d, want 1", len(rows))
	}
}
