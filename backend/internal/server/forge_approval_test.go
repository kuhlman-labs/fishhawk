package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/githubclient"
	"github.com/kuhlman-labs/fishhawk/backend/internal/identity"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
	"github.com/kuhlman-labs/fishhawk/backend/internal/spec"
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
		vetoer    string
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
		// R1(d): a GraphQL Bot login carries NO "[bot]" suffix, so only the
		// __typename == "User" check excludes a Bot or Mannequin that has a
		// non-zero databaseId and write permission.
		{name: "a Bot with a databaseId and no [bot] suffix is not counted", approvals: faCount1,
			snap: mcqReviewSnapshot{Reviews: []mcqReviewNode{{ID: 1, State: "APPROVED", SubmittedAt: faAt, Commit: faHead,
				Author: mcqActor{Typename: "Bot", Login: "renovate", ID: 29139614}}}},
			perms: map[string][2]string{"renovate": {"write", "write"}}, want: forgeApprovalAbsent, next: forgeApprovalNextApprovePR},
		{name: "a Mannequin with a databaseId is not counted", approvals: faCount1,
			snap: mcqReviewSnapshot{Reviews: []mcqReviewNode{{ID: 1, State: "APPROVED", SubmittedAt: faAt, Commit: faHead,
				Author: mcqActor{Typename: "Mannequin", Login: "imported-alice", ID: 31}}}},
			perms: map[string][2]string{"imported-alice": {"write", "write"}}, want: forgeApprovalAbsent, next: forgeApprovalNextApprovePR},
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
		// #4749: the negative member_of control. Each reviewer below has write
		// permission and passes every other predicate; only the provider's
		// answer differs from the approving positive controls.
		{name: "member_of false membership is absent",
			approvals: faCount1 + "              member_of: acme/team\n",
			snap:      mcqReviewSnapshot{Reviews: []mcqReviewNode{faUser(1, "APPROVED", faHead, "wally", 10)}},
			perms:     map[string][2]string{"wally": {"write", "write"}},
			setup: func(_ *testing.T, f *mcqFixture, _ *mcqRun) {
				faInstallGroups(f, map[string]bool{"acme/team": false})
			}, want: forgeApprovalAbsent, reason: forgeApprovalReasonNoQualifying, next: forgeApprovalNextApprovePR},
		{name: "member_of multi-group partially satisfied (team false) is absent",
			approvals: faCount1 + "              member_of: acme/team\n", esc: faEscAddGroup,
			snap:  mcqReviewSnapshot{Reviews: []mcqReviewNode{faUser(1, "APPROVED", faHead, "wally", 10)}},
			perms: map[string][2]string{"wally": {"write", "write"}},
			setup: func(_ *testing.T, f *mcqFixture, _ *mcqRun) {
				faInstallGroups(f, map[string]bool{"acme/security": true, "acme/team": false})
			}, want: forgeApprovalAbsent, reason: forgeApprovalReasonNoQualifying, next: forgeApprovalNextApprovePR},
		{name: "member_of multi-group partially satisfied (security false) is absent",
			approvals: faCount1 + "              member_of: acme/team\n", esc: faEscAddGroup,
			snap:  mcqReviewSnapshot{Reviews: []mcqReviewNode{faUser(1, "APPROVED", faHead, "wally", 10)}},
			perms: map[string][2]string{"wally": {"write", "write"}},
			setup: func(_ *testing.T, f *mcqFixture, _ *mcqRun) {
				faInstallGroups(f, map[string]bool{"acme/security": false, "acme/team": true})
			}, want: forgeApprovalAbsent, reason: forgeApprovalReasonNoQualifying, next: forgeApprovalNextApprovePR},
		{name: "member_of multi-group fully satisfied approves",
			approvals: faCount1 + "              member_of: acme/team\n", esc: faEscAddGroup,
			snap:  mcqReviewSnapshot{Reviews: []mcqReviewNode{faUser(1, "APPROVED", faHead, "wally", 10)}},
			perms: map[string][2]string{"wally": {"write", "write"}},
			setup: func(_ *testing.T, f *mcqFixture, _ *mcqRun) {
				faInstallGroups(f, map[string]bool{"acme/security": true, "acme/team": true})
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
			want:  forgeApprovalVetoed, next: forgeApprovalNextDismissReview, vetoer: "vera"},
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
			permFail: []string{"vera"}, want: forgeApprovalUndetermined, reason: forgeApprovalReasonVetoUndetermined,
			next: forgeApprovalNextDismissReview, vetoer: "vera"},
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
			if d.VetoReviewer != tc.vetoer {
				t.Errorf("veto_reviewer = %q, want %q", d.VetoReviewer, tc.vetoer)
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
// the 11th write, is undetermined with ZERO permission reads: the cap is
// judged on the candidate count BEFORE any permission read, so no reviewer's
// permission (rev101..rev111) is read at all.
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
	for i := int64(101); i <= 111; i++ {
		login := "rev" + strconv.FormatInt(i, 10)
		if n := f.gh.permissionCalls(login); n != 0 {
			t.Errorf("permission reads for %s = %d, want 0 (the cap is judged before any read)", login, n)
		}
	}
}

// TestForgeApprovalFold_SpecPostures pins C4: a run that never recorded a spec
// (empty WorkflowSHA, no cached bytes) declares no gate and makes no forge
// read; a recorded spec that is absent (the legacy row shape), reads back
// empty or does not parse is undetermined.
func TestForgeApprovalFold_SpecPostures(t *testing.T) {
	f := newMCQFixture(t)
	none := faSeed(t, f, nil, mcqReviewSnapshot{})
	f.exec(`UPDATE runs SET workflow_sha = '' WHERE id = $1`, none.id)
	d := faFold(t, f, none, faHead)
	if d.Declared || !d.approved() || d.Basis != forgeApprovalBasisNotRequired {
		t.Fatalf("spec-less fold = %+v, want not declared / not_required", d)
	}
	if n := f.gh.snapshotCalls(); n != 0 {
		t.Errorf("spec-less fold made %d snapshot reads, want 0", n)
	}
	// The legacy row shape: WorkflowSHA set (a spec WAS recorded) but no cached
	// bytes (the row predates the workflow_spec column) is undetermined, with
	// no forge read.
	legacy := faSeed(t, f, nil, mcqReviewSnapshot{})
	if rr := f.runRow(legacy); rr.WorkflowSHA == "" || len(rr.WorkflowSpec) != 0 {
		t.Fatalf("legacy fixture sha=%q spec=%d bytes, want a recorded sha and no cached spec", rr.WorkflowSHA, len(rr.WorkflowSpec))
	}
	d = faFold(t, f, legacy, faHead)
	if !d.Declared || d.Result != forgeApprovalUndetermined || d.Reason != forgeApprovalReasonSpecUndeterminable || d.approved() {
		t.Errorf("legacy spec fold = %s/%s declared=%v, want undetermined/%s", d.Result, d.Reason, d.Declared, forgeApprovalReasonSpecUndeterminable)
	}
	if n := f.gh.snapshotCalls(); n != 0 {
		t.Errorf("legacy spec fold made %d snapshot reads, want 0", n)
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
	// Credential-shaped fixtures are built at runtime so no source line
	// matches a secrets-scanner pattern.
	hdr := "Author" + "ization: " + "Bea" + "rer "
	ghs := "gh" + "s_" + strings.Repeat("a", 36)
	pat := "github" + "_pat_" + strings.Repeat("b", 22)
	ghp := "gh" + "p_" + strings.Repeat("c", 36)
	opaque := strings.Repeat("Zq9", 12) // matches no token pattern: only the header/bearer rules redact it

	in := hdr + ghs + " token " + pat + " leaked " + strings.Repeat("x", 2048) + "\nsecond line " + ghp
	got := sanitizeForgeReadDetail("read_failed", in)
	if len(got) > forgeApprovalDetailMaxBytes {
		t.Errorf("len = %d, want <= %d", len(got), forgeApprovalDetailMaxBytes)
	}
	for _, bad := range []string{ghs, "gh" + "s_", pat, "\n", "second line", ghp} {
		if strings.Contains(got, bad) {
			t.Errorf("sanitised detail %q still carries %q", got, bad)
		}
	}
	if !strings.HasPrefix(got, "read_failed: ") || !strings.Contains(got, "[redacted]") {
		t.Errorf("sanitised detail = %q", got)
	}

	// Opaque tokens match no gh*_ pattern, so these pin the header and bearer
	// rules: a real bearer authorization header, a token-scheme header (the
	// header rule alone) and a bare bearer credential (the bearer rule alone).
	for name, msg := range map[string]string{
		"bearer header": "upstream said " + hdr + opaque + " rejected",
		"token header":  "upstream said " + "Author" + "ization: token " + opaque + " rejected",
		"bare bearer":   "retry with " + "Bea" + "rer " + opaque + " later",
	} {
		s := sanitizeForgeReadDetail("read_failed", msg)
		if strings.Contains(s, opaque) || !strings.Contains(s, "[redacted]") {
			t.Errorf("%s: sanitised detail %q still carries the opaque token", name, s)
		}
	}
	// A bare ghs_ installation token outside any header.
	if s := sanitizeForgeReadDetail("read_failed", "token "+ghs+" expired"); strings.Contains(s, ghs) || !strings.Contains(s, "[redacted]") {
		t.Errorf("ghs token not redacted: %q", s)
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
	// #4764: a veto_undetermined decision names dismiss_review with its own
	// prose: the review could not be JUDGED, not that changes were requested.
	vu := forgeApprovalRefusal(forgeApprovalDecision{Declared: true, VerifiedHead: faHead, Result: forgeApprovalUndetermined,
		Reason: forgeApprovalReasonVetoUndetermined, NextStep: forgeApprovalNextDismissReview, VetoReviewer: "vera"}, rr)
	if !strings.Contains(vu.Message, "Reviewer vera's CHANGES_REQUESTED review could not be judged because their repository permission could not be read") ||
		strings.Contains(vu.Message, "requested changes: resolve them") || vu.Details["veto_reviewer"] != "vera" || vu.Details["next_step"] != forgeApprovalNextDismissReview {
		t.Errorf("veto_undetermined refusal = %+v", vu)
	}
	if mergeCodeAuthorizationUnrecorded != "merge_authorization_unrecorded" {
		t.Error("503 code drifted")
	}
}

// faStageReadRepo is a run.Repository whose ListStagesForRun answers stages
// (when non-nil) instead of the embedded repository, fails with fail (when
// set and failWhen is nil or reports true), or hides every review stage row.
type faStageReadRepo struct {
	run.Repository
	stages     []*run.Stage
	fail       error
	failWhen   func() bool
	hideReview bool
}

func (r faStageReadRepo) ListStagesForRun(ctx context.Context, runID uuid.UUID) ([]*run.Stage, error) {
	if r.fail != nil && (r.failWhen == nil || r.failWhen()) {
		return nil, r.fail
	}
	if r.stages != nil {
		return r.stages, nil
	}
	stages, err := r.Repository.ListStagesForRun(ctx, runID)
	if err != nil || !r.hideReview {
		return stages, err
	}
	var out []*run.Stage
	for _, st := range stages {
		if st.Type != run.StageTypeReview {
			out = append(out, st)
		}
	}
	return out, nil
}

// faCategoryAudit answers ListForRunByCategory from rows, keyed by category.
type faCategoryAudit struct {
	audit.BaseFake
	rows map[string][]*audit.Entry
}

func (a faCategoryAudit) ListForRunByCategory(_ context.Context, _ uuid.UUID, category string) ([]*audit.Entry, error) {
	return a.rows[category], nil
}

// TestReviewGateApprovalSequence_ReadErrorRefuses (#4764) pins the
// distinction between legitimately absent evidence and an evidence-read
// error: a ListStagesForRun failure (or no run repository) is RETURNED, so
// writeMergeQueueAuthorized refuses before any append; a run with no review
// stage row answers 0 with no error; otherwise the newest approval_submitted
// sequence on the review stage. Counterfactual: swallowing the stage-read
// error (the pre-#4764 `if err != nil { return 0, nil }`) answers (0, nil)
// on the read failure, RED.
func TestReviewGateApprovalSequence_ReadErrorRefuses(t *testing.T) {
	ctx := context.Background()
	runID := uuid.New()
	review := &run.Stage{ID: uuid.New(), RunID: runID, Sequence: 3, Type: run.StageTypeReview}
	implement := &run.Stage{ID: uuid.New(), RunID: runID, Sequence: 2, Type: run.StageTypeImplement}
	approvals := faCategoryAudit{rows: map[string][]*audit.Entry{"approval_submitted": {
		{Sequence: 4, StageID: &review.ID},
		{Sequence: 9, StageID: &review.ID},
		{Sequence: 12, StageID: &implement.ID},
		{Sequence: 15},
	}}}
	injected := errors.New("stages read failed")

	t.Run("a stage read failure is an error", func(t *testing.T) {
		s := &Server{cfg: Config{AuditRepo: approvals, RunRepo: faStageReadRepo{Repository: run.BaseFake{}, fail: injected}}}
		seq, err := s.reviewGateApprovalSequence(ctx, runID)
		if !errors.Is(err, injected) || seq != 0 {
			t.Fatalf("reviewGateApprovalSequence = %d, %v; want 0 and an error wrapping %v", seq, err, injected)
		}
	})
	t.Run("no run repository is an error", func(t *testing.T) {
		s := &Server{cfg: Config{AuditRepo: approvals}}
		if seq, err := s.reviewGateApprovalSequence(ctx, runID); err == nil || seq != 0 {
			t.Fatalf("reviewGateApprovalSequence = %d, %v; want 0 and an error", seq, err)
		}
	})
	t.Run("no review stage row is 0 with no error", func(t *testing.T) {
		s := &Server{cfg: Config{AuditRepo: approvals, RunRepo: faStageReadRepo{Repository: run.BaseFake{}, stages: []*run.Stage{implement}}}}
		if seq, err := s.reviewGateApprovalSequence(ctx, runID); err != nil || seq != 0 {
			t.Fatalf("reviewGateApprovalSequence = %d, %v; want 0, nil", seq, err)
		}
	})
	t.Run("the newest review-stage approval", func(t *testing.T) {
		s := &Server{cfg: Config{AuditRepo: approvals, RunRepo: faStageReadRepo{Repository: run.BaseFake{}, stages: []*run.Stage{implement, review}}}}
		if seq, err := s.reviewGateApprovalSequence(ctx, runID); err != nil || seq != 9 {
			t.Fatalf("reviewGateApprovalSequence = %d, %v; want 9, nil", seq, err)
		}
	})
}

// TestWriteMergeQueueAuthorized_StageReadErrorWritesNothing (#4764) reads
// COMMITTED STATE: an approved fold whose review-stage read fails writes no
// merge_queue_authorized row (never review_gate_approval_seq 0) and returns
// the error; a run whose stage rows carry no review stage still writes it,
// with review_gate_approval_seq 0. Counterfactual: swallowing the stage-read
// error appends a row on the read failure, RED on the zero-rows assertion.
func TestWriteMergeQueueAuthorized_StageReadErrorWritesNothing(t *testing.T) {
	f := newMCQFixture(t)
	r := faSeed(t, f, faSpec(faCount1, ""), mcqReviewSnapshot{Reviews: []mcqReviewNode{faUser(1, "APPROVED", faHead, "wally", 10)}})
	f.gh.setPermission("wally", "write", "write")
	d := faFold(t, f, r, faHead)
	if d.Result != forgeApprovalApproved {
		t.Fatalf("fold = %s/%s", d.Result, d.Reason)
	}
	ctx := context.Background()
	realRuns := f.srv.cfg.RunRepo
	injected := errors.New("stages read failed")
	f.srv.cfg.RunRepo = faStageReadRepo{Repository: realRuns, fail: injected}
	if seq, err := f.srv.writeMergeQueueAuthorized(ctx, f.runRow(r), d, forgeApprovalAuthorization{Path: "queue", IntentSequence: 7}); !errors.Is(err, injected) || seq != 0 {
		t.Fatalf("write on a stage read failure = %d, %v; want 0 and an error wrapping %v", seq, err, injected)
	}
	if rows, err := f.audit.ListForRunByCategory(ctx, r.id, CategoryMergeQueueAuthorized); err != nil || len(rows) != 0 {
		t.Fatalf("merge_queue_authorized rows after a stage read failure = %d (%v), want 0", len(rows), err)
	}

	f.srv.cfg.RunRepo = faStageReadRepo{Repository: realRuns, hideReview: true}
	seq, err := f.srv.writeMergeQueueAuthorized(ctx, f.runRow(r), d, forgeApprovalAuthorization{Path: "queue", IntentSequence: 7})
	if err != nil || seq == 0 {
		t.Fatalf("write with no review stage row = %d, %v; want a row", seq, err)
	}
	rows, err := f.audit.ListForRunByCategory(ctx, r.id, CategoryMergeQueueAuthorized)
	if err != nil || len(rows) != 1 {
		t.Fatalf("merge_queue_authorized rows = %d (%v), want 1", len(rows), err)
	}
	var p map[string]any
	if err := json.Unmarshal(rows[0].Payload, &p); err != nil {
		t.Fatal(err)
	}
	if p["review_gate_approval_seq"] != float64(0) {
		t.Errorf("review_gate_approval_seq = %v, want 0 for a run with no review stage row", p["review_gate_approval_seq"])
	}
}

// faPermReader is a forgeApprovalReader whose Permission answers perms, or
// errs for a login listed there.
type faPermReader struct {
	perms map[string]githubclient.RepoPermission
	errs  map[string]error
}

func (faPermReader) ReviewSnapshot(context.Context) (*githubclient.ReviewSnapshot, error) {
	return nil, errors.New("faPermReader: no snapshot")
}

func (r faPermReader) Permission(_ context.Context, login string) (githubclient.RepoPermission, error) {
	if err := r.errs[login]; err != nil {
		return githubclient.RepoPermission{}, err
	}
	return r.perms[login], nil
}

// TestFoldSnapshot_VetoUndeterminedNamesDismissReview (#4764): a
// CHANGES_REQUESTED author whose permission read fails, under ANY error class
// (a 404, a 500, a timeout: classifyForgePermission does not tell them
// apart), makes the fold veto_undetermined naming dismiss_review and that
// reviewer, and the inline 409 carries both. Counterfactuals: next_step back
// to retry_after, or VetoReviewer emptied, RED.
func TestFoldSnapshot_VetoUndeterminedNamesDismissReview(t *testing.T) {
	head := faHead
	actor := func(login string, id int64) githubclient.ReviewActor {
		return githubclient.ReviewActor{Present: true, Typename: "User", Login: login, DatabaseID: id}
	}
	snap := &githubclient.ReviewSnapshot{HeadOID: head, Author: actor("pr-opener", 1), Reviews: []githubclient.SnapshotReview{
		{DatabaseID: 1, State: "APPROVED", SubmittedAt: faAt, CommitOID: head, Author: actor("wally", 10)},
		{DatabaseID: 2, State: "CHANGES_REQUESTED", SubmittedAt: faAt.Add(time.Second), CommitOID: head, Author: actor("vera", 11)},
	}}
	url := "https://github.com/x/y/pull/1"
	runRow := &run.Run{ID: uuid.New(), Repo: "x/y", PullRequestURL: &url}
	s := &Server{cfg: Config{AuditRepo: audit.BaseFake{}}}
	for name, readErr := range map[string]error{
		"404":     fmt.Errorf("githubclient: get collaborator permission: %w", githubclient.ErrNotFound),
		"500":     errors.New("githubclient: get collaborator permission: status 500"),
		"timeout": context.DeadlineExceeded,
	} {
		t.Run(name, func(t *testing.T) {
			reader := faPermReader{perms: map[string]githubclient.RepoPermission{"wally": {RoleName: "write", Permission: "write"}},
				errs: map[string]error{"vera": readErr}}
			d := forgeApprovalDecision{Declared: true, VerifiedHead: head, approvals: effectiveApprovals(nil, spec.ComposedRequirements{})}
			d, err := s.foldSnapshot(context.Background(), runRow, d, snap, reader, nil, nil)
			if err != nil {
				t.Fatalf("foldSnapshot: %v", err)
			}
			if d.Result != forgeApprovalUndetermined || d.Reason != forgeApprovalReasonVetoUndetermined {
				t.Fatalf("fold = %s/%s (%s), want undetermined/%s", d.Result, d.Reason, d.Detail, forgeApprovalReasonVetoUndetermined)
			}
			if d.NextStep != forgeApprovalNextDismissReview {
				t.Errorf("next_step = %q, want %q", d.NextStep, forgeApprovalNextDismissReview)
			}
			if d.VetoReviewer != "vera" {
				t.Errorf("veto_reviewer = %q, want vera", d.VetoReviewer)
			}
			ref := forgeApprovalRefusal(d, runRow)
			if ref.Details["next_step"] != forgeApprovalNextDismissReview || ref.Details["veto_reviewer"] != "vera" || !strings.Contains(ref.Message, "Reviewer vera") {
				t.Errorf("refusal = %+v, want next_step %s, veto_reviewer vera and a message naming vera", ref, forgeApprovalNextDismissReview)
			}
		})
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

// TestVerifyPassReference pins the merge_queue_authorized verify_pass
// reference (#4749): it resolves the SAME predicate that admitted the head
// (mergeCandidateVerifyState), so a transferred verdict names its transfer
// row, not_required appears only on a head the predicate determined needs no
// verify, and every other state fails closed — with writeMergeQueueAuthorized
// then appending NOTHING. Counterfactuals: the transferred branch made
// never-true records the recorded shape; the fail-closed return replaced by
// not_required records a row for an unverified head, RED.
func TestVerifyPassReference(t *testing.T) {
	const root = "9999999999999999999999999999999999999999"
	ctx := context.Background()
	seqOf := func(t *testing.T, f *mcvFixture, category string) int64 {
		t.Helper()
		rows, err := f.au.ListForRunByCategory(ctx, f.runID, category)
		if err != nil || len(rows) != 1 || rows[0].Sequence == 0 {
			t.Fatalf("%s rows = %d (%v), want one sequenced row", category, len(rows), err)
		}
		return rows[0].Sequence
	}
	transferFixture := func(t *testing.T, mode MergeCandidateTransferMode) *mcvFixture {
		f := newMCVFixture(t, mcvUndelegatedSpecYAML)
		f.s.cfg.MergeCandidateTransfer.Mode = mode
		f.seedRebased(mcvHead, mcvHead, false)
		f.seedRebased(mcvOtherHead, mcvOtherHead, false)
		f.seedVerdict(mcvOtherHead, mergeCandidateResultPassed)
		f.appendRow(nil, CategoryMergeCandidateVerdictTransferred, mergeCandidateTransferPayload{
			FromHead: mcvOtherHead, ToHead: mcvHead, RootHead: root, ReasonCode: transferReasonIndependent})
		return f
	}
	// requireRefusedWritesNothing drives writeMergeQueueAuthorized with an
	// APPROVED decision at the head: it must error with zero rows appended.
	requireRefusedWritesNothing := func(t *testing.T, f *mcvFixture) {
		t.Helper()
		d := forgeApprovalDecision{Declared: true, Result: forgeApprovalApproved, VerifiedHead: mcvHead, Basis: forgeApprovalBasisLive}
		if seq, err := f.s.writeMergeQueueAuthorized(ctx, f.runRow(t), d, forgeApprovalAuthorization{Path: "inline"}); err == nil || seq != 0 {
			t.Fatalf("writeMergeQueueAuthorized = %d, %v; want a refusal", seq, err)
		}
		if rows, err := f.au.ListForRunByCategory(ctx, f.runID, CategoryMergeQueueAuthorized); err != nil || len(rows) != 0 {
			t.Fatalf("merge_queue_authorized rows = %d (%v), want 0", len(rows), err)
		}
	}

	t.Run("no verify command is not_required", func(t *testing.T) {
		f := newMCVFixture(t, mcvNoVerifySpecYAML)
		f.seedRebased(mcvHead, mcvHead, false)
		ref, err := f.s.verifyPassReference(ctx, f.runRow(t), mcvHead)
		if err != nil || ref != forgeApprovalBasisNotRequired {
			t.Fatalf("ref = %v, %v; want not_required", ref, err)
		}
	})
	t.Run("recorded pass names its merge_candidate_verified row", func(t *testing.T) {
		f := newMCVFixture(t, mcvUndelegatedSpecYAML)
		f.seedRebased(mcvHead, mcvHead, false)
		f.seedVerdict(mcvHead, mergeCandidateResultPassed)
		ref, err := f.s.verifyPassReference(ctx, f.runRow(t), mcvHead)
		want := map[string]any{"source": mergeCandidateSourceRecorded,
			"merge_candidate_verified_sequence": seqOf(t, f, CategoryMergeCandidateVerified)}
		if err != nil || !reflect.DeepEqual(ref, want) {
			t.Fatalf("ref = %v, %v; want %v", ref, err, want)
		}
	})
	t.Run("enforce-mode transfer names its transfer row", func(t *testing.T) {
		f := transferFixture(t, MergeCandidateTransferEnforce)
		ref, err := f.s.verifyPassReference(ctx, f.runRow(t), mcvHead)
		want := map[string]any{"source": mergeCandidateSourceTransferred,
			"merge_candidate_verdict_transferred_sequence": seqOf(t, f, CategoryMergeCandidateVerdictTransferred),
			"transferred_from": mcvOtherHead, "root_head": root}
		if err != nil || !reflect.DeepEqual(ref, want) {
			t.Fatalf("ref = %v, %v; want %v", ref, err, want)
		}
	})
	t.Run("a transfer with transfer mode off fails closed", func(t *testing.T) {
		f := transferFixture(t, MergeCandidateTransferOff)
		if ref, err := f.s.verifyPassReference(ctx, f.runRow(t), mcvHead); err == nil || ref != nil {
			t.Fatalf("ref = %v, %v; want an error (a revoked transfer is never not_required)", ref, err)
		}
		requireRefusedWritesNothing(t, f)
	})
	t.Run("a classified head with no verdict fails closed", func(t *testing.T) {
		f := newMCVFixture(t, mcvUndelegatedSpecYAML)
		f.seedRebased(mcvHead, mcvHead, false)
		ref, err := f.s.verifyPassReference(ctx, f.runRow(t), mcvHead)
		if err == nil || ref != nil || !strings.Contains(err.Error(), "not an admitted verdict") {
			t.Fatalf("ref = %v, %v; want the not-an-admitted-verdict error", ref, err)
		}
		requireRefusedWritesNothing(t, f)
	})
	t.Run("an audit read error fails closed", func(t *testing.T) {
		f := newMCVFixture(t, mcvUndelegatedSpecYAML)
		f.seedRebased(mcvHead, mcvHead, false)
		f.seedVerdict(mcvHead, mergeCandidateResultPassed)
		injected := errors.New("injected audit read failure")
		f.au.listByCategoryErr = injected
		if ref, err := f.s.verifyPassReference(ctx, f.runRow(t), mcvHead); !errors.Is(err, injected) || ref != nil {
			t.Fatalf("ref = %v, %v; want an error wrapping %v", ref, err, injected)
		}
	})
}

// faGroupIDP is a per-group identity provider (#4749): ResolveMembership
// answers from groups and records every group asked, under a mutex (#3226).
type faGroupIDP struct {
	fakeIdentityProvider
	groups map[string]bool
	mu     sync.Mutex
	asked  []string
}

func (p *faGroupIDP) ResolveMembership(_ context.Context, group, _ string) (bool, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.asked = append(p.asked, group)
	return p.groups[group], nil
}

func (p *faGroupIDP) askedGroups() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.asked...)
}

// faInstallGroups wires a faGroupIDP answering groups as f's github provider.
func faInstallGroups(f *mcqFixture, groups map[string]bool) *faGroupIDP {
	idp := &faGroupIDP{groups: groups}
	f.srv.cfg.IdentityProviders = map[string]identity.IdentityProvider{"github": idp}
	return idp
}

// TestForgeApproval_MemberOfNegativeRefusesWithoutAuthorization pins the
// ADR-112 fold's negative member_of control end to end (#4749): for a false
// single-group membership and both partially-satisfied multi-group shapes the
// fold is absent (no_qualifying_approval) with the provider CONSULTED,
// writeMergeQueueAuthorized refuses, the delegated arm's fold step does not
// proceed, and no merge_queue_authorized row and no merge-dispatch claim
// exist. Counterfactuals: the fold's `if !member { continue }` and
// forgeApprovalMemberOf's `if !ok { return false, nil }` each made a no-op
// count the reviewer, so the fold approves and an authorization row lands,
// RED.
func TestForgeApproval_MemberOfNegativeRefusesWithoutAuthorization(t *testing.T) {
	for _, tc := range []struct {
		name   string
		esc    string
		groups map[string]bool
	}{
		{"single group false", "", map[string]bool{"acme/team": false}},
		{"multi-group team false", faEscAddGroup, map[string]bool{"acme/security": true, "acme/team": false}},
		{"multi-group security false", faEscAddGroup, map[string]bool{"acme/security": false, "acme/team": true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newMCQFixture(t)
			r := faSeed(t, f, faSpec(faCount1+"              member_of: acme/team\n", tc.esc),
				mcqReviewSnapshot{Reviews: []mcqReviewNode{faUser(1, "APPROVED", faHead, "wally", 10)}})
			f.gh.setPermission("wally", "write", "write")
			idp := faInstallGroups(f, tc.groups)
			ctx := context.Background()
			d := faFold(t, f, r, faHead)
			if d.Result != forgeApprovalAbsent || d.Reason != forgeApprovalReasonNoQualifying {
				t.Fatalf("fold = %s/%s (%s), want absent/%s", d.Result, d.Reason, d.Detail, forgeApprovalReasonNoQualifying)
			}
			if len(idp.askedGroups()) == 0 {
				t.Fatal("ResolveMembership was never consulted; the member_of predicate did not run")
			}
			if seq, err := f.srv.writeMergeQueueAuthorized(ctx, f.runRow(r), d, forgeApprovalAuthorization{Path: "inline"}); err == nil || seq != 0 {
				t.Fatalf("writeMergeQueueAuthorized = %d, %v; want a refusal", seq, err)
			}
			if outcome, _ := f.srv.delegatedForgeApproval(ctx, f.runRow(r), faHead); outcome == mergeDispatchMerged {
				t.Fatalf("delegated fold outcome = %v, want a non-proceeding outcome", outcome)
			}
			if rows, err := f.audit.ListForRunByCategory(ctx, r.id, CategoryMergeQueueAuthorized); err != nil || len(rows) != 0 {
				t.Fatalf("merge_queue_authorized rows = %d (%v), want 0", len(rows), err)
			}
			var claims int
			if err := f.pool.QueryRow(ctx, `SELECT count(*) FROM merge_dispatch_claims WHERE run_id = $1`, r.id).Scan(&claims); err != nil || claims != 0 {
				t.Fatalf("merge-dispatch claims = %d (%v), want 0", claims, err)
			}
		})
	}
}
