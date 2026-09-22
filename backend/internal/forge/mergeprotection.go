package forge

import "context"

// AccessLevel is one role permitted to push to or merge into a protected
// branch: the forge's numeric level plus its human description. On GitLab
// the pair is a protected-branch entry's `access_level` +
// `access_level_description` (0 "No one", 30 "Developers + Maintainers",
// 40 "Maintainers", …; https://docs.gitlab.com/api/protected_branches/).
type AccessLevel struct {
	Level       int
	Description string
}

// MergeProtection is a forge's protection posture for ONE branch, read for
// the onboarding readiness `gitlab_merge_gate` rung (E45.66 / #3580): is the
// branch protected at all, by which rules, who may push/merge to it, and
// does the project refuse to merge until the head pipeline succeeds.
//
// Branch is the RESOLVED branch name — when the reader was asked about the
// project's default branch (an empty `branch` argument) this is the name the
// forge reported, so a consumer can render it without a second read.
//
// Protected is true iff at least one protected-branch rule matches Branch,
// exact or wildcard. MatchedRules names EVERY matching rule (the exact-name
// rule first, then wildcard rules in the forge's list order), because GitLab
// applies the MOST PERMISSIVE of all matching rules — a single selected rule
// is NOT the effective protection. PushAccessLevels / MergeAccessLevels are
// therefore the UNION across every matched rule, deduplicated by level and
// sorted ascending so the lowest (most permissive) level comes first, and
// AllowForcePush is the OR across them. All four are empty/false when
// Protected is false.
//
// PipelineMustSucceed, AllowSkippedPipeline and DiscussionsMustBeResolved
// are PROJECT-level merge settings (GitLab: `only_allow_merge_if_pipeline_
// succeeds`, `allow_merge_on_skipped_pipeline`, `only_allow_merge_if_all_
// discussions_are_resolved`) carried alongside the branch read because the
// rung's verdict — pipeline_gated iff Protected && PipelineMustSucceed —
// needs both halves from one authoritative read.
//
// What this DOES NOT express: GitLab has no per-context required status
// check, so a MergeProtection is NOT a claim that any particular commit
// status (the `fishhawk_audit_complete` status Fishhawk posts) individually
// gates the merge. It answers only "is the branch protected and must the
// head pipeline pass" — the closest question GitLab can answer.
type MergeProtection struct {
	Branch         string
	Protected      bool
	MatchedRules   []string
	AllowForcePush bool

	PushAccessLevels  []AccessLevel
	MergeAccessLevels []AccessLevel

	PipelineMustSucceed       bool
	AllowSkippedPipeline      bool
	DiscussionsMustBeResolved bool
}

// MergeProtectionReader is the forge-neutral branch-protection read
// capability behind the onboarding readiness `gitlab_merge_gate` rung
// (E45.66 / #3580). Its ONE consumer is that rung, which reads the
// project's real default branch on every readiness call.
//
// It is deliberately a STANDALONE capability interface rather than a new
// method on Forge, for the same reason FileFetcher (#2022), IssueOperations
// (#2900) and CIRequirementReader (#3490) are: widening Forge would churn
// every implementation and test fake for a capability one consumer uses,
// while a standalone interface lets an adapter carry it off-interface behind
// a compile-time assertion (`var _ forge.MergeProtectionReader =
// (*Forge)(nil)` in forge/gitlab). A consumer names it directly and resolves
// a registered forge.Forge to it with a type assertion.
//
// A forge with NO protected-branch concept MUST NOT implement it. The
// consumer treats a non-implementing forge as a fail-closed nil reader and
// renders `unknown` ("protection unread"); an implementation returning a
// fabricated {Protected:false} would instead be read as an AUTHORITATIVE
// "unprotected" and mislead the operator in the opposite direction.
//
// Errors: implementations return the sentinels in types.go (ErrNotFound,
// ErrForbidden, …) so callers switch on forge-neutral values via errors.Is.
// The project read must precede the protected-branch read, so a project the
// credential cannot see is a real failure (ErrNotFound / ErrForbidden) and
// only an authoritative rule list with no matching rule means "unprotected".
type MergeProtectionReader interface {
	// ReadMergeProtection reads branch's protection under scope. An EMPTY
	// branch means the project's REAL default branch as the forge reports
	// it; the result's Branch carries the resolved name. A project whose
	// default branch is empty (an empty repository) is ErrNotFound. A nil
	// *MergeProtection is returned only alongside a non-nil error.
	ReadMergeProtection(ctx context.Context, scope CredentialScope, repo RepoRef, branch string) (*MergeProtection, error)
}
