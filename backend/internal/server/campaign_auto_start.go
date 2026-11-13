package server

import (
	"path/filepath"
	"strings"

	"github.com/kuhlman-labs/fishhawk/backend/internal/campaign"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

// The runner_kind_source values ResolveCampaignAutoStart reports (#4525): the
// ladder rung that chose the auto-started run's runner kind.
const (
	// CampaignAutoStartSourceCampaign: the campaign's recorded runner_kind
	// (#4485) won.
	CampaignAutoStartSourceCampaign = "campaign"
	// CampaignAutoStartSourceFlag: the campaign recorded no runner_kind, so
	// the deployment's --auto-start-runner-kind applied.
	CampaignAutoStartSourceFlag = "auto_start_flag"
)

// The campaign-level auto-start refusal reasons (#4525). Each is a stable
// machine-readable token on the campaign_auto_start_refused audit row and the
// GET /v0/campaigns/{id}/status auto_start_refusal block.
const (
	// CampaignAutoStartRefusalWorkingDirUnbound: the resolved kind is local
	// and the campaign binds no working_dir, so a local run would have no
	// checkout.
	CampaignAutoStartRefusalWorkingDirUnbound = "working_dir_unbound"
	// CampaignAutoStartRefusalWorkingDirNotAbsolute: the resolved kind is
	// local and the campaign's bound working_dir is not absolute (a smuggled
	// row), so it cannot be inherited.
	CampaignAutoStartRefusalWorkingDirNotAbsolute = "working_dir_not_absolute"
	// CampaignAutoStartRefusalRunnerKindInvalid: the campaign's recorded
	// runner_kind is not in run.ValidRunnerKinds (a smuggled row).
	CampaignAutoStartRefusalRunnerKindInvalid = "campaign_runner_kind_invalid"
)

// CampaignAutoStart is ResolveCampaignAutoStart's result: the runner kind and
// checkout the campaign driver auto-starts an item run with, or a refusal.
//
// Detail and Remedy are server-authored constants. They never echo the
// campaign's working_dir, its recorded runner_kind when that is invalid, or any
// issue or campaign text; RunnerKind is empty on a campaign_runner_kind_invalid
// refusal for the same reason.
type CampaignAutoStart struct {
	// RunnerKind is the resolved kind (a run.ValidRunnerKinds member), or ""
	// when the recorded kind is invalid.
	RunnerKind string
	// Source is CampaignAutoStartSourceCampaign or CampaignAutoStartSourceFlag.
	Source string
	// WorkingDir is the campaign's trimmed binding for a local start; empty
	// for any other kind and on a refusal.
	WorkingDir string
	// RefusalReason is one of the CampaignAutoStartRefusal* tokens, or ""
	// when the start may proceed.
	RefusalReason string
	// Detail and Remedy explain a refusal; both are empty otherwise.
	Detail string
	Remedy string
}

// Refused reports whether the resolution refuses the auto-start.
func (a CampaignAutoStart) Refused() bool { return a.RefusalReason != "" }

// ResolveCampaignAutoStart resolves how the campaign driver auto-starts an item
// run for c (#4525). It is pure: the driver's run starter and the campaign
// status read both call it, so the auto_start_refusal block cannot drift from
// what the driver does.
//
// The ladder: the campaign's recorded runner_kind (#4485) when non-empty,
// re-validated against run.ValidRunnerKinds as handleStartCampaignItemRun does
// (a recorded gitlab_ci is passed through, as that path accepts it); else
// deploymentKind, the --auto-start-runner-kind value (empty means
// github_actions). The working_dir -> local inference rung of
// resolveCampaignItemRunnerKind is deliberately NOT applied, so a deployment
// on the default keeps auto-starting github_actions runs for a bound campaign
// that recorded no runner_kind.
//
// A local resolution inherits the campaign's working_dir, re-validated with
// filepath.IsAbs exactly as handleStartCampaignItemRun's inherit rung does:
// an empty binding refuses working_dir_unbound and a relative one refuses
// working_dir_not_absolute. Any other kind leaves WorkingDir empty.
func ResolveCampaignAutoStart(c *campaign.Campaign, deploymentKind string) CampaignAutoStart {
	var recordedKind, boundDir string
	if c != nil {
		recordedKind = c.RunnerKind
		boundDir = strings.TrimSpace(c.WorkingDir)
	}

	res := CampaignAutoStart{RunnerKind: deploymentKind, Source: CampaignAutoStartSourceFlag}
	if res.RunnerKind == "" {
		res.RunnerKind = run.RunnerKindGitHubActions
	}
	if recordedKind != "" {
		res = CampaignAutoStart{RunnerKind: recordedKind, Source: CampaignAutoStartSourceCampaign}
		if _, ok := run.ValidRunnerKinds[recordedKind]; !ok {
			res.RunnerKind = ""
			return res.refuse(CampaignAutoStartRefusalRunnerKindInvalid)
		}
	}

	if res.RunnerKind != run.RunnerKindLocal {
		return res
	}
	if boundDir == "" {
		return res.refuse(CampaignAutoStartRefusalWorkingDirUnbound)
	}
	if !filepath.IsAbs(boundDir) {
		return res.refuse(CampaignAutoStartRefusalWorkingDirNotAbsolute)
	}
	res.WorkingDir = boundDir
	return res
}

// refuse stamps a refusal's reason, detail and remedy onto a resolution.
func (a CampaignAutoStart) refuse(reason string) CampaignAutoStart {
	a.RefusalReason = reason
	a.WorkingDir = ""
	a.Detail, a.Remedy = campaignAutoStartRefusalText(reason, a.Source)
	return a
}

// campaignAutoStartRefusalText returns a refusal's server-constant detail and
// remedy. The remedy's flag clause depends on the source, because
// --auto-start-runner-kind does not override a recorded runner_kind.
func campaignAutoStartRefusalText(reason, source string) (detail, remedy string) {
	const startItem = "start each item with fishhawk_start_campaign_item_run"
	flagClause := "; or set --auto-start-runner-kind github_actions so the driver auto-starts github_actions runs for campaigns that record no runner_kind"
	if source == CampaignAutoStartSourceCampaign {
		flagClause = "; --auto-start-runner-kind does not override the campaign's recorded runner_kind"
	}
	switch reason {
	case CampaignAutoStartRefusalWorkingDirUnbound:
		return "the campaign driver resolved runner_kind local for this campaign, but the campaign binds no working_dir, so a local item run would have no checkout; the driver starts no item of this campaign",
			startItem + " passing an absolute working_dir" + flagClause
	case CampaignAutoStartRefusalWorkingDirNotAbsolute:
		return "the campaign driver resolved runner_kind local for this campaign, but the campaign's bound working_dir is not an absolute path, so it cannot be inherited; the driver starts no item of this campaign",
			startItem + " passing an absolute working_dir" + flagClause
	default: // CampaignAutoStartRefusalRunnerKindInvalid
		return "the campaign's recorded runner_kind is not a valid runner kind (" + validRunnerKindsMessage + "); the driver starts no item of this campaign",
			startItem + " passing an explicit runner_kind (and an absolute working_dir for local)" + flagClause
	}
}

// campaignAutoStartRefusalPayload is the GET /v0/campaigns/{id}/status
// auto_start_refusal block (#4525).
type campaignAutoStartRefusalPayload struct {
	Reason           string `json:"reason"`
	RunnerKind       string `json:"runner_kind,omitempty"`
	RunnerKindSource string `json:"runner_kind_source"`
	Detail           string `json:"detail"`
	Remedy           string `json:"remedy"`
}

// campaignAutoStartRefusalForStatus returns the status read's
// auto_start_refusal block, or nil. It is present only when the campaign
// driver runs (Config.CampaignAutoStartRunnerKind is set), the campaign is
// running (the only state the driver's START pass evaluates) and
// ResolveCampaignAutoStart refuses it. Derived from config plus the campaign
// row, never persisted.
func (s *Server) campaignAutoStartRefusalForStatus(c *campaign.Campaign) *campaignAutoStartRefusalPayload {
	if s.cfg.CampaignAutoStartRunnerKind == "" || c == nil || c.State != campaign.StateRunning {
		return nil
	}
	res := ResolveCampaignAutoStart(c, s.cfg.CampaignAutoStartRunnerKind)
	if !res.Refused() {
		return nil
	}
	return &campaignAutoStartRefusalPayload{
		Reason:           res.RefusalReason,
		RunnerKind:       res.RunnerKind,
		RunnerKindSource: res.Source,
		Detail:           res.Detail,
		Remedy:           res.Remedy,
	}
}
