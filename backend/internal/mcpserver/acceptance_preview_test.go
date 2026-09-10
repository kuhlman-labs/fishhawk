package mcpserver

import (
	"reflect"
	"strings"
	"testing"
)

// TestResolveAcceptancePreviewCmd_EnvWinsOverAutoPreview is the operator-env
// precedence pin (#3321 failure mode (b)): an operator-set
// FISHHAWK_ACCEPTANCE_PREVIEW_CMD is returned VERBATIM with source "env" even
// when autoPreview is true. Deleting the env-first branch reddens it.
func TestResolveAcceptancePreviewCmd_EnvWinsOverAutoPreview(t *testing.T) {
	const operatorCmd = "make preview-target"
	env := map[string]string{acceptancePreviewCmdEnv: operatorCmd}
	cmd, src := resolveAcceptancePreviewCmd(envFuncFromMap(env), true)
	if cmd != operatorCmd {
		t.Errorf("cmd = %q, want the operator value %q verbatim (auto_preview must not overwrite it)", cmd, operatorCmd)
	}
	if src != "env" {
		t.Errorf("source = %q, want %q", src, "env")
	}
	// The default must NOT have leaked in.
	if cmd == acceptancePreviewDefaultCmd {
		t.Errorf("cmd = the built-in default; the operator's value was overwritten")
	}
}

// TestResolveAcceptancePreviewCmd_Arms table-drives the GATE decision's three
// arms. The EMPTY third arm is the load-bearing one: it means "do not take the
// proceed branch and inject nothing", and it must NOT default (that is the
// DISPLAY renderer's job — #3321 constraint item 1's decoupling).
func TestResolveAcceptancePreviewCmd_Arms(t *testing.T) {
	tests := []struct {
		name        string
		env         map[string]string
		autoPreview bool
		wantCmd     string
		wantSource  string
	}{
		{"env set, auto off -> env", map[string]string{acceptancePreviewCmdEnv: "x preview"}, false, "x preview", "env"},
		{"env set, auto on -> env wins", map[string]string{acceptancePreviewCmdEnv: "x preview"}, true, "x preview", "env"},
		{"no env, auto on -> default", nil, true, acceptancePreviewDefaultCmd, "auto_preview"},
		{"no env, auto off -> EMPTY (gate decision, not display)", nil, false, "", ""},
		{"empty env value, auto off -> EMPTY", map[string]string{acceptancePreviewCmdEnv: ""}, false, "", ""},
		{"empty env value, auto on -> default", map[string]string{acceptancePreviewCmdEnv: ""}, true, acceptancePreviewDefaultCmd, "auto_preview"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cmd, src := resolveAcceptancePreviewCmd(envFuncFromMap(tc.env), tc.autoPreview)
			if cmd != tc.wantCmd || src != tc.wantSource {
				t.Errorf("got (%q, %q), want (%q, %q)", cmd, src, tc.wantCmd, tc.wantSource)
			}
		})
	}
	t.Run("nil getenv degrades to the auto_preview arm", func(t *testing.T) {
		cmd, src := resolveAcceptancePreviewCmd(nil, true)
		if cmd != acceptancePreviewDefaultCmd || src != "auto_preview" {
			t.Errorf("got (%q, %q), want the default under auto_preview", cmd, src)
		}
	})
}

// TestAcceptancePreviewDisplayCommand_DefaultsAndSHA is the REVISION-1 renderer
// table: the DEFAULT lives in the renderer, so a zero-valued cmd still produces
// a concrete command. Removing the default (returning cmd unchanged) reddens
// every empty-cmd row on the exact-string comparison.
func TestAcceptancePreviewDisplayCommand_DefaultsAndSHA(t *testing.T) {
	const sha40 = "abc1234def5678901234567890123456789abcde"
	tests := []struct {
		name string
		cmd  string
		sha  string
		want string
	}{
		{"empty cmd + valid sha -> defaulted command WITH sha", "", "abc1234", "scripts/dev preview abc1234"},
		{"empty cmd + empty sha -> defaulted command alone", "", "", "scripts/dev preview"},
		{"configured cmd overrides the default", "make preview", "abc1234", "make preview abc1234"},
		{"7-char sha accepted", "", "abc1234", "scripts/dev preview abc1234"},
		{"40-char sha accepted", "", sha40, "scripts/dev preview " + sha40},
		{"mixed-case sha accepted", "", "AbC1234dEf", "scripts/dev preview AbC1234dEf"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := acceptancePreviewDisplayCommand(tc.cmd, tc.sha)
			if got != tc.want {
				t.Errorf("acceptancePreviewDisplayCommand(%q, %q) = %q, want %q", tc.cmd, tc.sha, got, tc.want)
			}
			if got == "" {
				t.Error("rendered command is EMPTY; the renderer must always produce a concrete command")
			}
		})
	}
}

// TestAcceptancePreviewDisplayCommand_ConfiguredCmdOverridesDefault pins the
// non-empty check in acceptancePreviewCommandOrDefault (#3321 failure mode (b),
// second half): deleting that check makes the default win over the configured
// value and reddens this.
func TestAcceptancePreviewDisplayCommand_ConfiguredCmdOverridesDefault(t *testing.T) {
	const configured = "make preview-target"
	got := acceptancePreviewDisplayCommand(configured, "abc1234")
	if !strings.HasPrefix(got, configured) {
		t.Errorf("rendered = %q, want it to start with the CONFIGURED command %q", got, configured)
	}
	if strings.Contains(got, acceptancePreviewDefaultCmd) {
		t.Errorf("rendered = %q, want the built-in default %q NOT to appear", got, acceptancePreviewDefaultCmd)
	}
}

// TestAcceptancePreviewDisplayCommand_RejectsImplausibleSHA is #3321 failure
// mode (g). Each malformed value must render the DEFAULTED-OR-CONFIGURED
// command with NO SHA appended — the value is never interpolated into a
// rendered shell command, while the command itself survives the rejection.
// Deleting the regexp check reddens it.
func TestAcceptancePreviewDisplayCommand_RejectsImplausibleSHA(t *testing.T) {
	malformed := []struct {
		name string
		sha  string
	}{
		{"empty", ""},
		{"6 chars (too short)", "abc123"},
		{"65 chars (too long)", strings.Repeat("a", 65)},
		{"non-hex", "zzzzzzz"},
		{"shell semicolon", "abc1234; rm -rf /"},
		{"shell backtick", "abc1234`id`"},
		{"pipe", "abc1234|nc evil 1"},
		{"whitespace-padded", " abc1234 "},
		{"newline injection", "abc1234\nrm -rf /"},
	}
	for _, tc := range malformed {
		t.Run(tc.name, func(t *testing.T) {
			// Defaulted arm.
			got := acceptancePreviewDisplayCommand("", tc.sha)
			if got != acceptancePreviewDefaultCmd {
				t.Errorf("got %q, want the bare defaulted command %q with NO sha appended", got, acceptancePreviewDefaultCmd)
			}
			// Configured arm: the command still survives the rejection.
			const configured = "make preview"
			gotCfg := acceptancePreviewDisplayCommand(configured, tc.sha)
			if gotCfg != configured {
				t.Errorf("got %q, want the bare configured command %q with NO sha appended", gotCfg, configured)
			}
		})
	}
}

// TestAcceptancePreviewAction_AlwaysCarriesACommand pins that no caller can
// produce a command-less action, INCLUDING from a zero-valued cmd (#3321
// constraint item 1).
func TestAcceptancePreviewAction_AlwaysCarriesACommand(t *testing.T) {
	t.Run("zero-valued cmd still renders a concrete command", func(t *testing.T) {
		a := acceptancePreviewAction("", "", "", "")
		if a.Action != acceptancePreviewActionName {
			t.Errorf("Action = %q, want %q", a.Action, acceptancePreviewActionName)
		}
		if a.Params["command"] != acceptancePreviewDefaultCmd {
			t.Errorf("params.command = %q, want %q", a.Params["command"], acceptancePreviewDefaultCmd)
		}
		if a.Consumes != consumesNone {
			t.Errorf("Consumes = %q, want %q", a.Consumes, consumesNone)
		}
		if a.Precondition == "" || a.Reason == "" {
			t.Error("Precondition/Reason must be populated")
		}
		if !strings.Contains(a.Reason, "auto_preview") {
			t.Errorf("Reason = %q, want it to name the auto_preview alternative", a.Reason)
		}
		// Optional params omitted when empty.
		for _, k := range []string{"expected_head_sha", "working_dir", "target_host"} {
			if _, ok := a.Params[k]; ok {
				t.Errorf("params[%q] present on the empty-input action, want omitted", k)
			}
		}
	})
	t.Run("populated inputs land in params", func(t *testing.T) {
		a := acceptancePreviewAction("make preview", "abc1234", "/repo", "localhost:8090")
		want := map[string]string{
			"command":           "make preview abc1234",
			"expected_head_sha": "abc1234",
			"working_dir":       "/repo",
			"target_host":       "localhost:8090",
		}
		if !reflect.DeepEqual(a.Params, want) {
			t.Errorf("params = %#v, want %#v", a.Params, want)
		}
	})
	t.Run("implausible sha is omitted from params too", func(t *testing.T) {
		a := acceptancePreviewAction("", "not-a-sha", "", "")
		if _, ok := a.Params["expected_head_sha"]; ok {
			t.Errorf("params carry an implausible sha %q, want it dropped", a.Params["expected_head_sha"])
		}
		if a.Params["command"] != acceptancePreviewDefaultCmd {
			t.Errorf("params.command = %q, want the concrete default", a.Params["command"])
		}
	})
}

// TestLatestReportedHeadSHA covers all three ledger categories, newest-wins
// ordering, and each degrade (empty window, malformed payload, non-ledger
// category). The slice is TIME-DESCENDING, item 0 newest.
func TestLatestReportedHeadSHA(t *testing.T) {
	entry := func(cat string, payload any) AuditEntry {
		return AuditEntry{Category: cat, Payload: payload}
	}
	head := func(sha string) map[string]any { return map[string]any{"head_sha": sha} }

	tests := []struct {
		name   string
		recent []AuditEntry
		want   string
	}{
		{"pull_request_opened", []AuditEntry{entry("pull_request_opened", head("abc1234"))}, "abc1234"},
		{"child_pushed", []AuditEntry{entry("child_pushed", head("bbb2222"))}, "bbb2222"},
		{"fixup_pushed", []AuditEntry{entry("fixup_pushed", head("ccc3333"))}, "ccc3333"},
		{
			"newest wins (fix-up above the PR open)",
			[]AuditEntry{
				entry("fixup_pushed", head("ccc3333")),
				entry("pull_request_opened", head("abc1234")),
			},
			"ccc3333",
		},
		{
			"non-ledger entries are skipped",
			[]AuditEntry{
				entry("acceptance_outcome_recorded", map[string]any{"verdict": "failed"}),
				entry("stage_started", head("nope")),
				entry("pull_request_opened", head("abc1234")),
			},
			"abc1234",
		},
		{"empty window -> empty", nil, ""},
		{"no matching entry -> empty", []AuditEntry{entry("stage_started", head("abc1234"))}, ""},
		{"malformed payload (not an object) -> empty", []AuditEntry{entry("pull_request_opened", "oops")}, ""},
		{"malformed payload (head_sha not a string) -> empty", []AuditEntry{entry("pull_request_opened", map[string]any{"head_sha": 7})}, ""},
		{"nil payload -> empty", []AuditEntry{entry("pull_request_opened", nil)}, ""},
		{
			"empty head_sha falls through to the next ledger entry",
			[]AuditEntry{
				entry("fixup_pushed", head("")),
				entry("pull_request_opened", head("abc1234")),
			},
			"abc1234",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := latestReportedHeadSHA(tc.recent); got != tc.want {
				t.Errorf("latestReportedHeadSHA = %q, want %q", got, tc.want)
			}
		})
	}
}

// acceptanceDispatchAction is the shape the fold keys on.
func acceptanceDispatchAction() SuggestedAction {
	return SuggestedAction{
		Action: "fishhawk_dispatch_stage",
		Params: map[string]string{"run_id": "r", "stage_id": "s", "stage": "acceptance"},
	}
}

// TestFoldAcceptancePreviewAdvisory_InsertsBeforeAcceptanceDispatch pins the
// insert POSITION: the preview is the dispatch's precondition, so it lands
// immediately BEFORE it, and the rest of the list is preserved in order.
func TestFoldAcceptancePreviewAdvisory_InsertsBeforeAcceptanceDispatch(t *testing.T) {
	run := &Run{ID: "r", WorkingDir: "/repo"}
	na := &NextActions{State: "x", Actions: []SuggestedAction{
		{Action: "fishhawk_get_run_status"},
		acceptanceDispatchAction(),
		{Action: "fishhawk_merge_run"},
	}}
	foldAcceptancePreviewAdvisory(run, "", "abc1234", na)

	if len(na.Actions) != 4 {
		t.Fatalf("len(actions) = %d, want 4", len(na.Actions))
	}
	if na.Actions[0].Action != "fishhawk_get_run_status" {
		t.Errorf("actions[0] = %q, want the preceding action preserved", na.Actions[0].Action)
	}
	if na.Actions[1].Action != acceptancePreviewActionName {
		t.Errorf("actions[1] = %q, want %q inserted immediately BEFORE the acceptance dispatch",
			na.Actions[1].Action, acceptancePreviewActionName)
	}
	if na.Actions[2].Action != "fishhawk_dispatch_stage" || na.Actions[2].Params["stage"] != "acceptance" {
		t.Errorf("actions[2] = %+v, want the acceptance dispatch", na.Actions[2])
	}
	if na.Actions[3].Action != "fishhawk_merge_run" {
		t.Errorf("actions[3] = %q, want the trailing action preserved", na.Actions[3].Action)
	}
	if got := na.Actions[1].Params["command"]; got != "scripts/dev preview abc1234" {
		t.Errorf("params.command = %q, want the defaulted command with the head sha", got)
	}
	if got := na.Actions[1].Params["working_dir"]; got != "/repo" {
		t.Errorf("params.working_dir = %q, want the run's bound checkout", got)
	}
}

// TestFoldAcceptancePreviewAdvisory_KeysOnRunStageToo pins that a suggested
// acceptance fishhawk_run_stage is keyed on as well, not only dispatch.
func TestFoldAcceptancePreviewAdvisory_KeysOnRunStageToo(t *testing.T) {
	run := &Run{ID: "r"}
	na := &NextActions{Actions: []SuggestedAction{
		{Action: "fishhawk_run_stage", Params: map[string]string{"stage": "acceptance"}},
	}}
	foldAcceptancePreviewAdvisory(run, "", "", na)
	if len(na.Actions) != 2 || na.Actions[0].Action != acceptancePreviewActionName {
		t.Fatalf("actions = %+v, want the preview folded ahead of the acceptance run_stage", na.Actions)
	}
}

// TestFoldAcceptancePreviewAdvisory_NoOpWithoutAcceptanceDispatch is #3321
// failure mode (h): a plan-gate / implement-gate / terminal actions list comes
// back BYTE-IDENTICAL (reflect.DeepEqual against a pre-call copy). Deleting the
// has-acceptance-dispatch predicate reddens it.
func TestFoldAcceptancePreviewAdvisory_NoOpWithoutAcceptanceDispatch(t *testing.T) {
	cases := []struct {
		name    string
		actions []SuggestedAction
	}{
		{"plan gate", []SuggestedAction{{Action: "fishhawk_approve_plan", Params: map[string]string{"run_id": "r"}}}},
		{
			"implement gate (a dispatch for a DIFFERENT stage)",
			[]SuggestedAction{{Action: "fishhawk_dispatch_stage", Params: map[string]string{"stage": "implement"}}},
		},
		{
			"dispatch with NO stage param",
			[]SuggestedAction{{Action: "fishhawk_dispatch_stage", Params: map[string]string{"run_id": "r"}}},
		},
		{"terminal (nil actions)", nil},
		{"merge ritual", []SuggestedAction{{Action: "fishhawk_merge_run", Params: map[string]string{"run_id": "r"}}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			na := &NextActions{State: "s", Actions: tc.actions}
			before := append([]SuggestedAction(nil), tc.actions...)
			foldAcceptancePreviewAdvisory(&Run{ID: "r"}, "", "abc1234", na)
			if !reflect.DeepEqual(na.Actions, tc.actions) {
				t.Errorf("actions mutated:\n got %#v\nwant %#v", na.Actions, before)
			}
		})
	}
}

// TestFoldAcceptancePreviewAdvisory_NilSafe pins the nil arms.
func TestFoldAcceptancePreviewAdvisory_NilSafe(t *testing.T) {
	t.Run("nil na", func(t *testing.T) {
		foldAcceptancePreviewAdvisory(&Run{ID: "r"}, "", "abc1234", nil) // must not panic
	})
	t.Run("nil run leaves actions untouched", func(t *testing.T) {
		actions := []SuggestedAction{acceptanceDispatchAction()}
		na := &NextActions{Actions: actions}
		foldAcceptancePreviewAdvisory(nil, "", "abc1234", na)
		if !reflect.DeepEqual(na.Actions, actions) {
			t.Errorf("actions = %#v, want byte-identical under a nil run", na.Actions)
		}
	})
}

// TestFoldAcceptancePreviewAdvisory_EmptyCmdIsNotANoOpArm is the direct
// REVISION-1 pin at the fold layer: an empty cmd must still fold in a CONCRETE
// command, because the renderer defaults.
func TestFoldAcceptancePreviewAdvisory_EmptyCmdIsNotANoOpArm(t *testing.T) {
	na := &NextActions{Actions: []SuggestedAction{acceptanceDispatchAction()}}
	foldAcceptancePreviewAdvisory(&Run{ID: "r"}, "", "", na)
	if len(na.Actions) != 2 {
		t.Fatalf("len(actions) = %d, want the preview folded in even with an empty cmd", len(na.Actions))
	}
	if got := na.Actions[0].Params["command"]; got != acceptancePreviewDefaultCmd {
		t.Errorf("params.command = %q, want the concrete default %q — an empty cmd is NOT a no-op arm",
			got, acceptancePreviewDefaultCmd)
	}
}

// TestFoldAcceptancePreviewAdvisory_ConfiguredCmdIsRendered pins that an
// operator-set command reaches the folded action instead of the default.
func TestFoldAcceptancePreviewAdvisory_ConfiguredCmdIsRendered(t *testing.T) {
	na := &NextActions{Actions: []SuggestedAction{acceptanceDispatchAction()}}
	foldAcceptancePreviewAdvisory(&Run{ID: "r"}, "make preview", "abc1234", na)
	if got := na.Actions[0].Params["command"]; got != "make preview abc1234" {
		t.Errorf("params.command = %q, want the configured command", got)
	}
}
