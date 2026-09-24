package workmgmt

import "testing"

// TestClassifyEpicRef pins the pure classifier (#3648). The load-bearing
// property is (a): every ref ParseIssueRef accepts today classifies as
// EpicRefFormIssue and flows through byte-identically. The group-epic and
// cross-repo arms are recognition heuristics whose only consequence is which
// 422 message an operator reads.
func TestClassifyEpicRef(t *testing.T) {
	cases := []struct {
		name      string
		ref       string
		wantForm  EpicRefForm
		wantNum   int
		wantGroup string
	}{
		// (a) issue forms — the backward-compatibility guarantee.
		{"bare number", "25", EpicRefFormIssue, 25, ""},
		{"hash number", "#25", EpicRefFormIssue, 25, ""},
		{"issue prefix", "issue:25", EpicRefFormIssue, 25, ""},
		{"issue prefix spaced", " issue: 25 ", EpicRefFormIssue, 25, ""},

		// (b) GitLab group-epic forms.
		{"ampersand with group", "mygroup&5", EpicRefFormGroupEpic, 0, "mygroup"},
		{"ampersand nested group", "my/sub/group&12", EpicRefFormGroupEpic, 0, "my/sub/group"},
		{"ampersand bare", "&7", EpicRefFormGroupEpic, 0, ""},
		{"group epic url", "https://gitlab.com/groups/acme/team/-/epics/9", EpicRefFormGroupEpic, 0, "acme/team"},
		{"group epic url no group capture", "/-/epics/3", EpicRefFormGroupEpic, 0, ""},

		// (c) cross-repo issue reference.
		{"cross repo", "owner/name#25", EpicRefFormCrossRepo, 0, ""},

		// (d) unrecognized.
		{"garbage", "abc", EpicRefFormUnrecognized, 0, ""},
		{"empty", "", EpicRefFormUnrecognized, 0, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ClassifyEpicRef(tc.ref)
			if got.Form != tc.wantForm {
				t.Errorf("Form = %q, want %q", got.Form, tc.wantForm)
			}
			if got.Number != tc.wantNum {
				t.Errorf("Number = %d, want %d", got.Number, tc.wantNum)
			}
			if got.Group != tc.wantGroup {
				t.Errorf("Group = %q, want %q", got.Group, tc.wantGroup)
			}
		})
	}
}
