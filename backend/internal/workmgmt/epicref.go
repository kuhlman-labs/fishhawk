package workmgmt

import (
	"regexp"
	"strings"
)

// EpicRefForm classifies the SHAPE of a campaign epic_ref for refusal
// enrichment (#3648). It is NOT a normalization: the only consequence of a
// form is which refusal message an operator reads, so a corner-case
// misclassification degrades to the generic unrecognized refusal rather than
// to a wrong outcome.
type EpicRefForm string

const (
	// EpicRefFormIssue is the single form the campaign epic path can resolve:
	// a same-repo issue reference (`N`, `#N`, `issue:N`) ParseIssueRef accepts.
	// EVERY ref accepted today classifies here and flows through byte-identically
	// — the backward-compatibility guarantee.
	EpicRefFormIssue EpicRefForm = "issue"
	// EpicRefFormGroupEpic is a GitLab group-epic reference — `<group>&<iid>`
	// (leading group path optional) or a URL path containing `/-/epics/<iid>`.
	// GitLab group epics are a Premium group-level object v0 does not model.
	EpicRefFormGroupEpic EpicRefForm = "gitlab_group_epic"
	// EpicRefFormCrossRepo is a cross-repo issue reference `<owner>/<name>#<iid>`
	// — a well-formed issue ref that names an issue in a DIFFERENT repo than the
	// campaign's own, so it names no epic in this repo.
	EpicRefFormCrossRepo EpicRefForm = "cross_repo_issue"
	// EpicRefFormUnrecognized is anything else — the generic refusal.
	EpicRefFormUnrecognized EpicRefForm = "unrecognized"
)

// EpicRefClass is the result of ClassifyEpicRef: the recognized form, plus the
// issue Number for the issue form and the Group path for a group-epic form that
// names one. Number/Group are zero-valued for forms that do not carry them.
type EpicRefClass struct {
	Form   EpicRefForm
	Number int
	Group  string
}

// The group-epic and cross-repo shapes are RECOGNITION HEURISTICS (#3648): a
// miss degrades to EpicRefFormUnrecognized, never a wrong outcome.
var (
	// groupEpicAmpersandRe matches `<group-path>&<digits>` with the group path
	// OPTIONAL, so a bare `&5` also matches. The group path, when present, may
	// carry `/` (a nested GitLab group path like `my/sub/group`).
	groupEpicAmpersandRe = regexp.MustCompile(`^([A-Za-z0-9][A-Za-z0-9._/-]*)?&(\d+)$`)
	// groupEpicURLRe recognizes a group-epic canonical URL path segment.
	groupEpicURLRe = regexp.MustCompile(`/-/epics/\d+`)
	// groupEpicURLGroupRe extracts the group path from a `/groups/<path>/-/epics/<iid>`
	// URL when it names one.
	groupEpicURLGroupRe = regexp.MustCompile(`/groups/(.+?)/-/epics/\d+`)
	// crossRepoRe matches `<owner>/<name>#<digits>`.
	crossRepoRe = regexp.MustCompile(`^([A-Za-z0-9][A-Za-z0-9._-]*)/([A-Za-z0-9][A-Za-z0-9._-]*)#(\d+)$`)
)

// ClassifyEpicRef is a PURE, fail-soft classifier for the campaign epic_ref
// surface (#3648). It is used ONLY to enrich a refusal — it never normalizes,
// and no provider calls it, so the single-normalization property ParseIssueRef
// establishes (issueref.go) is untouched.
//
// Classification order is LOAD-BEARING:
//
//	(a) if ParseIssueRef(ref) succeeds -> EpicRefFormIssue with Number set, so
//	    EVERY ref accepted today is classified issue and flows through
//	    byte-identically (the backward-compatibility guarantee);
//	(b) else the GitLab group-epic shape -> EpicRefFormGroupEpic with Group set
//	    when the form names one;
//	(c) else the cross-repo `<owner>/<name>#<digits>` shape -> EpicRefFormCrossRepo;
//	(d) else EpicRefFormUnrecognized.
//
// (b) and (c) are RECOGNITION HEURISTICS whose only consequence is which of two
// 422 messages the operator reads, so a corner-case miss degrades to the
// generic unrecognized refusal.
func ClassifyEpicRef(ref string) EpicRefClass {
	if n, err := ParseIssueRef(ref); err == nil {
		return EpicRefClass{Form: EpicRefFormIssue, Number: n}
	}
	trimmed := strings.TrimSpace(ref)
	// (b) GitLab group-epic ampersand form (`<group>&<iid>`, group optional).
	if m := groupEpicAmpersandRe.FindStringSubmatch(trimmed); m != nil {
		return EpicRefClass{Form: EpicRefFormGroupEpic, Group: m[1]}
	}
	// GitLab group-epic URL form (`.../-/epics/<iid>`, group path extracted when
	// the `/groups/<path>/-/epics/<iid>` shape names one).
	if groupEpicURLRe.MatchString(trimmed) {
		group := ""
		if gm := groupEpicURLGroupRe.FindStringSubmatch(trimmed); gm != nil {
			group = gm[1]
		}
		return EpicRefClass{Form: EpicRefFormGroupEpic, Group: group}
	}
	// (c) cross-repo issue reference.
	if crossRepoRe.MatchString(trimmed) {
		return EpicRefClass{Form: EpicRefFormCrossRepo}
	}
	// (d) anything else.
	return EpicRefClass{Form: EpicRefFormUnrecognized}
}
