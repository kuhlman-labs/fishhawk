package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/kuhlman-labs/fishhawk/backend/internal/campaign"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

// TestResolveCampaignAutoStart pins the campaign driver's auto-start ladder
// (#4525): recorded campaign runner_kind > deployment flag, with no
// working_dir -> local inference rung, and the local checkout re-validated.
func TestResolveCampaignAutoStart(t *testing.T) {
	const bound = "/abs/checkout"
	cases := []struct {
		name           string
		recordedKind   string
		workingDir     string
		deploymentKind string
		wantKind       string
		wantSource     string
		wantDir        string
		wantReason     string
	}{
		{name: "recorded local wins over a github_actions flag", recordedKind: run.RunnerKindLocal, workingDir: bound,
			deploymentKind: run.RunnerKindGitHubActions, wantKind: run.RunnerKindLocal, wantSource: CampaignAutoStartSourceCampaign, wantDir: bound},
		{name: "recorded github_actions wins over a local flag", recordedKind: run.RunnerKindGitHubActions, workingDir: bound,
			deploymentKind: run.RunnerKindLocal, wantKind: run.RunnerKindGitHubActions, wantSource: CampaignAutoStartSourceCampaign},
		{name: "recorded gitlab_ci passes through", recordedKind: run.RunnerKindGitLabCI, workingDir: bound,
			deploymentKind: run.RunnerKindLocal, wantKind: run.RunnerKindGitLabCI, wantSource: CampaignAutoStartSourceCampaign},
		{name: "no recorded kind, local flag, bound", workingDir: bound,
			deploymentKind: run.RunnerKindLocal, wantKind: run.RunnerKindLocal, wantSource: CampaignAutoStartSourceFlag, wantDir: bound},
		{name: "no recorded kind, local flag, binding trimmed", workingDir: "  " + bound + "\n",
			deploymentKind: run.RunnerKindLocal, wantKind: run.RunnerKindLocal, wantSource: CampaignAutoStartSourceFlag, wantDir: bound},
		{name: "no recorded kind, local flag, empty binding", deploymentKind: run.RunnerKindLocal,
			wantKind: run.RunnerKindLocal, wantSource: CampaignAutoStartSourceFlag, wantReason: CampaignAutoStartRefusalWorkingDirUnbound},
		{name: "no recorded kind, local flag, whitespace binding", workingDir: " \t ", deploymentKind: run.RunnerKindLocal,
			wantKind: run.RunnerKindLocal, wantSource: CampaignAutoStartSourceFlag, wantReason: CampaignAutoStartRefusalWorkingDirUnbound},
		{name: "no recorded kind, local flag, relative binding", workingDir: "relative/checkout", deploymentKind: run.RunnerKindLocal,
			wantKind: run.RunnerKindLocal, wantSource: CampaignAutoStartSourceFlag, wantReason: CampaignAutoStartRefusalWorkingDirNotAbsolute},
		{name: "recorded local, relative binding", recordedKind: run.RunnerKindLocal, workingDir: "relative/checkout",
			wantKind: run.RunnerKindLocal, wantSource: CampaignAutoStartSourceCampaign, wantReason: CampaignAutoStartRefusalWorkingDirNotAbsolute},
		{name: "invalid recorded kind", recordedKind: "bogus", workingDir: bound, deploymentKind: run.RunnerKindLocal,
			wantKind: "", wantSource: CampaignAutoStartSourceCampaign, wantReason: CampaignAutoStartRefusalRunnerKindInvalid},
		{name: "empty deployment kind means github_actions", wantKind: run.RunnerKindGitHubActions, wantSource: CampaignAutoStartSourceFlag},
		// No working_dir -> local inference rung: a bound campaign that
		// recorded no runner_kind keeps the github_actions default.
		{name: "bound pre-#4485 campaign under the default", workingDir: bound,
			wantKind: run.RunnerKindGitHubActions, wantSource: CampaignAutoStartSourceFlag},
		// C2: under the default (empty) flag a recorded runner_kind still wins.
		{name: "default flag, recorded local, bound", recordedKind: run.RunnerKindLocal, workingDir: bound,
			wantKind: run.RunnerKindLocal, wantSource: CampaignAutoStartSourceCampaign, wantDir: bound},
		{name: "default flag, recorded local, unbound", recordedKind: run.RunnerKindLocal,
			wantKind: run.RunnerKindLocal, wantSource: CampaignAutoStartSourceCampaign, wantReason: CampaignAutoStartRefusalWorkingDirUnbound},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := &campaign.Campaign{RunnerKind: tc.recordedKind, WorkingDir: tc.workingDir}
			got := ResolveCampaignAutoStart(c, tc.deploymentKind)
			if got.RunnerKind != tc.wantKind || got.Source != tc.wantSource || got.WorkingDir != tc.wantDir || got.RefusalReason != tc.wantReason {
				t.Fatalf("ResolveCampaignAutoStart = {kind %q source %q dir %q reason %q}, want {kind %q source %q dir %q reason %q}",
					got.RunnerKind, got.Source, got.WorkingDir, got.RefusalReason,
					tc.wantKind, tc.wantSource, tc.wantDir, tc.wantReason)
			}
			if got.Refused() != (tc.wantReason != "") {
				t.Fatalf("Refused() = %v, want %v", got.Refused(), tc.wantReason != "")
			}
			if !got.Refused() {
				if got.Detail != "" || got.Remedy != "" {
					t.Fatalf("a non-refusal carries detail %q / remedy %q, want both empty", got.Detail, got.Remedy)
				}
				return
			}
			if got.Detail == "" || got.Remedy == "" {
				t.Fatalf("a refusal must carry detail and remedy, got %q / %q", got.Detail, got.Remedy)
			}
			if !strings.Contains(got.Remedy, "fishhawk_start_campaign_item_run") || !strings.Contains(got.Remedy, "--auto-start-runner-kind") {
				t.Errorf("remedy %q must name fishhawk_start_campaign_item_run and --auto-start-runner-kind", got.Remedy)
			}
			// C8: Detail and Remedy are server constants; neither echoes the
			// campaign's working_dir nor its recorded (invalid) runner_kind.
			for _, field := range []string{got.Detail, got.Remedy} {
				if dir := strings.TrimSpace(tc.workingDir); dir != "" && strings.Contains(field, dir) {
					t.Errorf("refusal text %q echoes the campaign working_dir %q", field, dir)
				}
				if tc.recordedKind == "bogus" && strings.Contains(field, "bogus") {
					t.Errorf("refusal text %q echoes the invalid recorded runner_kind", field)
				}
			}
		})
	}

	t.Run("nil campaign resolves the deployment kind", func(t *testing.T) {
		got := ResolveCampaignAutoStart(nil, run.RunnerKindLocal)
		if got.RefusalReason != CampaignAutoStartRefusalWorkingDirUnbound {
			t.Fatalf("reason = %q, want %q", got.RefusalReason, CampaignAutoStartRefusalWorkingDirUnbound)
		}
	})
}

// TestGetCampaignStatus_AutoStartRefusal pins the status read's
// auto_start_refusal block (#4525): present only when the driver runs
// (Config.CampaignAutoStartRunnerKind set), the campaign is running and the
// resolver refuses it; absent otherwise, with every other key unchanged.
func TestGetCampaignStatus_AutoStartRefusal(t *testing.T) {
	cases := []struct {
		name         string
		configKind   string
		recordedKind string
		workingDir   string
		state        campaign.State
		wantReason   string
		wantKind     string
		wantSource   string
	}{
		{name: "local flag, running unbound campaign", configKind: run.RunnerKindLocal, state: campaign.StateRunning,
			wantReason: CampaignAutoStartRefusalWorkingDirUnbound, wantKind: run.RunnerKindLocal, wantSource: CampaignAutoStartSourceFlag},
		{name: "github_actions flag, running recorded-local unbound campaign", configKind: run.RunnerKindGitHubActions,
			recordedKind: run.RunnerKindLocal, state: campaign.StateRunning,
			wantReason: CampaignAutoStartRefusalWorkingDirUnbound, wantKind: run.RunnerKindLocal, wantSource: CampaignAutoStartSourceCampaign},
		{name: "local flag, running relative binding", configKind: run.RunnerKindLocal, workingDir: "relative/dir", state: campaign.StateRunning,
			wantReason: CampaignAutoStartRefusalWorkingDirNotAbsolute, wantKind: run.RunnerKindLocal, wantSource: CampaignAutoStartSourceFlag},
		{name: "local flag, running invalid recorded kind", configKind: run.RunnerKindLocal, recordedKind: "bogus", workingDir: seededCampaignWorkingDir,
			state: campaign.StateRunning, wantReason: CampaignAutoStartRefusalRunnerKindInvalid, wantSource: CampaignAutoStartSourceCampaign},
		{name: "local flag, bound campaign: absent", configKind: run.RunnerKindLocal, workingDir: seededCampaignWorkingDir, state: campaign.StateRunning},
		// C7: the driver-off fixture independently resolves to a refusal
		// (recorded local, unbound), so only the Config gate keeps it absent.
		{name: "driver off: absent", recordedKind: run.RunnerKindLocal, state: campaign.StateRunning},
		// C3: only a running campaign is evaluated by the driver.
		{name: "paused campaign: absent", configKind: run.RunnerKindLocal, recordedKind: run.RunnerKindLocal, state: campaign.StatePaused},
		{name: "terminal campaign: absent", configKind: run.RunnerKindLocal, recordedKind: run.RunnerKindLocal, state: campaign.StateCancelled},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			repo := newFakeCampaignRepo()
			c := repo.seedCampaignWithItems("kuhlman-labs/fishhawk", "issue:99", []*campaign.Item{
				{IssueRef: "issue:100", State: campaign.ItemStatePending},
			})
			c.RunnerKind = tc.recordedKind
			c.WorkingDir = tc.workingDir
			c.State = tc.state
			s := New(Config{CampaignRepo: repo, CampaignAutoStartRunnerKind: tc.configKind})

			req := httptest.NewRequest(http.MethodGet, "/v0/campaigns/"+c.ID.String()+"/status", nil)
			req.SetPathValue("campaign_id", c.ID.String())
			w := httptest.NewRecorder()
			s.handleGetCampaignStatus(w, withAuth(req))
			if w.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200 (body=%s)", w.Code, w.Body.String())
			}
			var body map[string]json.RawMessage
			if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
				t.Fatalf("decode: %v", err)
			}
			for _, key := range []string{"campaign", "items", "rollup", "next_action"} {
				if _, ok := body[key]; !ok {
					t.Errorf("status body lost key %q", key)
				}
			}
			raw, present := body["auto_start_refusal"]
			if tc.wantReason == "" {
				if present {
					t.Fatalf("auto_start_refusal present (%s), want absent", raw)
				}
				return
			}
			if !present {
				t.Fatalf("auto_start_refusal absent, want reason %q (body=%s)", tc.wantReason, w.Body.String())
			}
			var block map[string]string
			if err := json.Unmarshal(raw, &block); err != nil {
				t.Fatalf("decode auto_start_refusal: %v", err)
			}
			if block["reason"] != tc.wantReason || block["runner_kind"] != tc.wantKind || block["runner_kind_source"] != tc.wantSource {
				t.Fatalf("auto_start_refusal = %v, want reason %q runner_kind %q runner_kind_source %q",
					block, tc.wantReason, tc.wantKind, tc.wantSource)
			}
			if block["detail"] == "" || !strings.Contains(block["remedy"], "fishhawk_start_campaign_item_run") {
				t.Fatalf("auto_start_refusal detail/remedy = %q / %q", block["detail"], block["remedy"])
			}
			if tc.workingDir != "" && strings.Contains(string(raw), tc.workingDir) {
				t.Fatalf("auto_start_refusal %s echoes the campaign working_dir", raw)
			}
			if strings.Contains(string(raw), "bogus") {
				t.Fatalf("auto_start_refusal %s echoes the invalid recorded runner_kind", raw)
			}
		})
	}
}

// TestOpenAPI_CampaignAutoStartRefusalDocumented pins the documented contract
// (#4525): CampaignStatus.auto_start_refusal references the
// CampaignAutoStartRefusal schema, whose properties are exactly the payload's
// JSON keys and whose reason enum is exactly the resolver's refusal tokens.
func TestOpenAPI_CampaignAutoStartRefusalDocumented(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join(pkgSrcDir, "..", "..", "..", "docs", "api", "v0.openapi.yaml"))
	if err != nil {
		t.Fatalf("read openapi: %v", err)
	}
	var doc struct {
		Components struct {
			Schemas map[string]struct {
				Properties map[string]struct {
					Ref  string   `yaml:"$ref"`
					Enum []string `yaml:"enum"`
				} `yaml:"properties"`
			} `yaml:"schemas"`
		} `yaml:"components"`
	}
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("parse openapi: %v", err)
	}
	if got := doc.Components.Schemas["CampaignStatus"].Properties["auto_start_refusal"].Ref; got != "#/components/schemas/CampaignAutoStartRefusal" {
		t.Fatalf("CampaignStatus.auto_start_refusal $ref = %q", got)
	}
	props := doc.Components.Schemas["CampaignAutoStartRefusal"].Properties
	payload, _ := json.Marshal(campaignAutoStartRefusalPayload{Reason: "r", RunnerKind: "k", RunnerKindSource: "s", Detail: "d", Remedy: "m"})
	var keys map[string]any
	_ = json.Unmarshal(payload, &keys)
	if len(props) != len(keys) {
		t.Errorf("CampaignAutoStartRefusal has %d properties, payload has %d keys", len(props), len(keys))
	}
	for k := range keys {
		if _, ok := props[k]; !ok {
			t.Errorf("CampaignAutoStartRefusal is missing property %q", k)
		}
	}
	want := []string{CampaignAutoStartRefusalWorkingDirUnbound, CampaignAutoStartRefusalWorkingDirNotAbsolute, CampaignAutoStartRefusalRunnerKindInvalid}
	if got := props["reason"].Enum; strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("reason enum = %v, want %v", got, want)
	}
	if got := props["runner_kind_source"].Enum; strings.Join(got, ",") != CampaignAutoStartSourceCampaign+","+CampaignAutoStartSourceFlag {
		t.Errorf("runner_kind_source enum = %v", got)
	}
}
