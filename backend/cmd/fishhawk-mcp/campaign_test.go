package main

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"
)

// --- fishhawk_start_campaign (E25.8 / #1447) ---

// TestStartCampaign_HappyPath_PostsBodyReturnsCampaign drives the whole
// tool→client→wire→decode chain in one test: the input struct's
// repo/epic_ref/pause_policy reach the backend request body, and the created
// Campaign decodes back out.
func TestStartCampaign_HappyPath_PostsBodyReturnsCampaign(t *testing.T) {
	fb, srv := newFakeBackend(t)
	r := newResolver(srv, nil)

	_, out, err := r.startCampaign(context.Background(), nil, StartCampaignInput{
		Repo:        "kuhlman-labs/fishhawk",
		EpicRef:     "#25",
		PausePolicy: "pause_item",
	})
	if err != nil {
		t.Fatalf("startCampaign: %v", err)
	}
	if fb.createCampaignBody.Repo != "kuhlman-labs/fishhawk" ||
		fb.createCampaignBody.EpicRef != "#25" ||
		fb.createCampaignBody.PausePolicy != "pause_item" {
		t.Errorf("backend got body = %+v", fb.createCampaignBody)
	}
	if out.Campaign.ID == "" {
		t.Errorf("Campaign.ID empty; expected the fake to allocate one")
	}
	if out.Campaign.Repo != "kuhlman-labs/fishhawk" || out.Campaign.EpicRef != "#25" {
		t.Errorf("decoded Campaign = %+v", out.Campaign)
	}
	if out.Campaign.PausePolicy != "pause_item" {
		t.Errorf("Campaign.PausePolicy = %q, want pause_item", out.Campaign.PausePolicy)
	}
}

// TestStartCampaign_OmittedPausePolicy_LeavesBodyEmpty pins the optional
// pause_policy: omitting it sends an empty value (the backend normalizes it).
func TestStartCampaign_OmittedPausePolicy_LeavesBodyEmpty(t *testing.T) {
	fb, srv := newFakeBackend(t)
	r := newResolver(srv, nil)

	_, _, err := r.startCampaign(context.Background(), nil, StartCampaignInput{
		Repo:    "x/y",
		EpicRef: "#1",
	})
	if err != nil {
		t.Fatalf("startCampaign: %v", err)
	}
	if fb.createCampaignBody.PausePolicy != "" {
		t.Errorf("pause_policy = %q, want empty (omit takes the server default)", fb.createCampaignBody.PausePolicy)
	}
}

// TestStartCampaign_MissingRepo_FailsLocally proves the empty-repo guard
// rejects before any HTTP call.
func TestStartCampaign_MissingRepo_FailsLocally(t *testing.T) {
	fb, srv := newFakeBackend(t)
	r := newResolver(srv, nil)

	_, _, err := r.startCampaign(context.Background(), nil, StartCampaignInput{EpicRef: "#1"})
	if err == nil || !strings.Contains(err.Error(), "repo is required") {
		t.Fatalf("err = %v, want local repo-required validation", err)
	}
	if fb.createCampaignBody.EpicRef != "" {
		t.Errorf("backend was called despite missing repo: %+v", fb.createCampaignBody)
	}
}

// TestStartCampaign_MissingEpicRef_FailsLocally proves the empty-epic_ref guard
// rejects before any HTTP call.
func TestStartCampaign_MissingEpicRef_FailsLocally(t *testing.T) {
	fb, srv := newFakeBackend(t)
	r := newResolver(srv, nil)

	_, _, err := r.startCampaign(context.Background(), nil, StartCampaignInput{Repo: "x/y"})
	if err == nil || !strings.Contains(err.Error(), "epic_ref is required") {
		t.Fatalf("err = %v, want local epic_ref-required validation", err)
	}
	if fb.createCampaignBody.Repo != "" {
		t.Errorf("backend was called despite missing epic_ref: %+v", fb.createCampaignBody)
	}
}

func TestStartCampaign_RepoNotInstalled_MapsActionableError(t *testing.T) {
	fb, srv := newFakeBackend(t)
	fb.createCampaignStatus = http.StatusUnprocessableEntity
	fb.createCampaignErr = `{"error":{"code":"repo_not_installed","message":"GitHub App is not installed on the target repository"}}`
	r := newResolver(srv, nil)

	_, _, err := r.startCampaign(context.Background(), nil, StartCampaignInput{Repo: "x/y", EpicRef: "#1"})
	if err == nil {
		t.Fatal("err = nil, want repo_not_installed mapping")
	}
	for _, want := range []string{"repo_not_installed", "install the Fishhawk GitHub App", "x/y"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("err %q missing %q", err.Error(), want)
		}
	}
}

func TestStartCampaign_DanglingDependency_MapsActionableError(t *testing.T) {
	fb, srv := newFakeBackend(t)
	fb.createCampaignStatus = http.StatusUnprocessableEntity
	fb.createCampaignErr = `{"error":{"code":"campaign_dangling_dependency","message":"child #27 depends on #99 which is not a fellow child"}}`
	r := newResolver(srv, nil)

	_, _, err := r.startCampaign(context.Background(), nil, StartCampaignInput{Repo: "x/y", EpicRef: "#25"})
	if err == nil {
		t.Fatal("err = nil, want campaign_dangling_dependency mapping")
	}
	for _, want := range []string{"campaign_dangling_dependency", "depends_on", "#25"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("err %q missing %q", err.Error(), want)
		}
	}
}

func TestStartCampaign_RepoUnconfigured_MapsActionableError(t *testing.T) {
	fb, srv := newFakeBackend(t)
	fb.createCampaignStatus = http.StatusServiceUnavailable
	fb.createCampaignErr = `{"error":{"code":"campaign_repo_unconfigured","message":"campaigns endpoint requires a configured campaign repository"}}`
	r := newResolver(srv, nil)

	_, _, err := r.startCampaign(context.Background(), nil, StartCampaignInput{Repo: "x/y", EpicRef: "#1"})
	if err == nil || !strings.Contains(err.Error(), "campaign_repo_unconfigured") {
		t.Fatalf("err = %v, want campaign_repo_unconfigured mapping", err)
	}
}

// TestStartCampaign_ForbiddenScope_SurfacesError proves a runner-bound token's
// 403 (no write:campaigns) surfaces as a tool error rather than a silent
// success — the auth path the plan notes is covered by an error-mapping test.
func TestStartCampaign_ForbiddenScope_SurfacesError(t *testing.T) {
	fb, srv := newFakeBackend(t)
	fb.createCampaignStatus = http.StatusForbidden
	fb.createCampaignErr = `{"error":{"code":"insufficient_scope","message":"token lacks write:campaigns"}}`
	r := newResolver(srv, nil)

	_, _, err := r.startCampaign(context.Background(), nil, StartCampaignInput{Repo: "x/y", EpicRef: "#1"})
	if err == nil || !strings.Contains(err.Error(), "insufficient_scope") {
		t.Fatalf("err = %v, want the 403 insufficient_scope surfaced", err)
	}
}

// --- fishhawk_get_campaign_status (E25.8 / #1447) ---

// TestGetCampaignStatus_HappyPath_ReturnsRollupAndNextActions drives the chain
// end-to-end: the path id round-trips, and the rollup + next_action + the
// embedded next_actions classification all decode back.
func TestGetCampaignStatus_HappyPath_ReturnsRollupAndNextActions(t *testing.T) {
	fb, srv := newFakeBackend(t)
	id := uuid.New()
	fb.campaignStatusByID[id] = CampaignStatus{
		Campaign: Campaign{ID: id.String(), Repo: "x/y", EpicRef: "#25", State: "running", PausePolicy: "pause_campaign"},
		Items: []CampaignItem{
			{ID: uuid.NewString(), IssueRef: "#26", DependsOn: []string{}, State: "eligible"},
		},
		Rollup:     CampaignRollup{Eligible: []string{"#26"}, Blocked: []string{}, Running: []string{}, Done: []string{}, Failed: []string{}, Cancelled: []string{}, Paused: []string{}},
		NextAction: CampaignNextAction{Action: "start_run", IssueRef: "#26", Detail: "this item's dependencies are satisfied"},
	}
	r := newResolver(srv, nil)

	_, out, err := r.getCampaignStatus(context.Background(), nil, GetCampaignStatusInput{CampaignID: id.String()})
	if err != nil {
		t.Fatalf("getCampaignStatus: %v", err)
	}
	if fb.getCampaignStatusID != id {
		t.Errorf("backend got campaign id %s, want %s", fb.getCampaignStatusID, id)
	}
	if len(out.Rollup.Eligible) != 1 || out.Rollup.Eligible[0] != "#26" {
		t.Errorf("Rollup.Eligible = %+v", out.Rollup.Eligible)
	}
	if out.NextAction.Action != "start_run" || out.NextAction.IssueRef != "#26" {
		t.Errorf("NextAction = %+v", out.NextAction)
	}
	if out.NextActions == nil || len(out.NextActions.Actions) == 0 {
		t.Fatalf("NextActions should be a non-empty classification, got %+v", out.NextActions)
	}
	if out.NextActions.Actions[0].Action != "fishhawk_start_run" {
		t.Errorf("classified action = %q, want fishhawk_start_run", out.NextActions.Actions[0].Action)
	}
	if got := out.NextActions.Actions[0].Params["trigger_ref"]; got != "#26" {
		t.Errorf("classified start_run trigger_ref = %q, want #26", got)
	}
}

func TestGetCampaignStatus_InvalidUUID_FailsLocally(t *testing.T) {
	_, srv := newFakeBackend(t)
	r := newResolver(srv, nil)

	_, _, err := r.getCampaignStatus(context.Background(), nil, GetCampaignStatusInput{CampaignID: "not-a-uuid"})
	if err == nil || !strings.Contains(err.Error(), "not a valid UUID") {
		t.Fatalf("err = %v, want local UUID validation error", err)
	}
}

func TestGetCampaignStatus_NotFound_MapsActionableError(t *testing.T) {
	fb, srv := newFakeBackend(t)
	fb.campaignStatusStatus = http.StatusNotFound
	fb.campaignStatusErr = `{"error":{"code":"campaign_not_found","message":"no campaign with that id"}}`
	r := newResolver(srv, nil)

	_, _, err := r.getCampaignStatus(context.Background(), nil, GetCampaignStatusInput{CampaignID: uuid.NewString()})
	if err == nil || !strings.Contains(err.Error(), "campaign_not_found") {
		t.Fatalf("err = %v, want campaign_not_found mapping", err)
	}
}

// --- fishhawk_resume_campaign (E25.8 / #1447) ---

// TestResumeCampaign_HappyPath_PostsToResumePath drives the chain: the path id
// round-trips and the updated (resumed) campaign decodes back.
func TestResumeCampaign_HappyPath_PostsToResumePath(t *testing.T) {
	fb, srv := newFakeBackend(t)
	id := uuid.New()
	fb.resumeCampaignResp = Campaign{ID: id.String(), Repo: "x/y", EpicRef: "#25", State: "running", PausePolicy: "pause_campaign"}
	r := newResolver(srv, nil)

	_, out, err := r.resumeCampaign(context.Background(), nil, ResumeCampaignInput{CampaignID: id.String()})
	if err != nil {
		t.Fatalf("resumeCampaign: %v", err)
	}
	if fb.resumeCampaignID != id {
		t.Errorf("backend got resume id %s, want %s", fb.resumeCampaignID, id)
	}
	if out.Campaign.State != "running" {
		t.Errorf("Campaign.State = %q, want running", out.Campaign.State)
	}
}

func TestResumeCampaign_InvalidUUID_FailsLocally(t *testing.T) {
	_, srv := newFakeBackend(t)
	r := newResolver(srv, nil)

	_, _, err := r.resumeCampaign(context.Background(), nil, ResumeCampaignInput{CampaignID: "nope"})
	if err == nil || !strings.Contains(err.Error(), "not a valid UUID") {
		t.Fatalf("err = %v, want local UUID validation error", err)
	}
}

func TestResumeCampaign_NotPaused_MapsActionableError(t *testing.T) {
	fb, srv := newFakeBackend(t)
	fb.resumeCampaignStatus = http.StatusConflict
	fb.resumeCampaignErr = `{"error":{"code":"campaign_not_paused","message":"campaign has no paused state to resume"}}`
	r := newResolver(srv, nil)

	_, _, err := r.resumeCampaign(context.Background(), nil, ResumeCampaignInput{CampaignID: uuid.NewString()})
	if err == nil {
		t.Fatal("err = nil, want campaign_not_paused mapping")
	}
	for _, want := range []string{"campaign_not_paused", "nothing to resume"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("err %q missing %q", err.Error(), want)
		}
	}
}

func TestResumeCampaign_NotFound_MapsActionableError(t *testing.T) {
	fb, srv := newFakeBackend(t)
	fb.resumeCampaignStatus = http.StatusNotFound
	fb.resumeCampaignErr = `{"error":{"code":"campaign_not_found","message":"no campaign with that id"}}`
	r := newResolver(srv, nil)

	_, _, err := r.resumeCampaign(context.Background(), nil, ResumeCampaignInput{CampaignID: uuid.NewString()})
	if err == nil || !strings.Contains(err.Error(), "campaign_not_found") {
		t.Fatalf("err = %v, want campaign_not_found mapping", err)
	}
}
