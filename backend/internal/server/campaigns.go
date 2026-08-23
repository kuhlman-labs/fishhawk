package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/campaign"
	"github.com/kuhlman-labs/fishhawk/backend/internal/forge"
	"github.com/kuhlman-labs/fishhawk/backend/internal/githubclient"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
	"github.com/kuhlman-labs/fishhawk/backend/internal/spec"
	"github.com/kuhlman-labs/fishhawk/backend/internal/workmgmt"
)

// Campaign list pagination bounds, mirroring runsDefaultLimit / runsMaxLimit.
const (
	campaignsDefaultLimit = 50
	campaignsMaxLimit     = 200
)

// campaignResponse is the JSON shape POST /v0/campaigns and
// GET /v0/campaigns/{id} return. Field names + types match
// docs/api/v0.openapi.yaml's `Campaign` schema exactly, mirroring
// runResponse (runs.go) so there's never a translation step between the
// OpenAPI doc and the wire format.
type campaignResponse struct {
	ID      uuid.UUID `json:"id"`
	Repo    string    `json:"repo"`
	EpicRef string    `json:"epic_ref"`
	State   string    `json:"state"`
	// PausePolicy is the operator-chosen pause behavior on a gate hand-off
	// (E25.7): pause_campaign (block the whole campaign, the default) or
	// pause_item (continue-others). Always normalized (never empty) on a
	// persisted campaign.
	PausePolicy string `json:"pause_policy"`
	// OperatorAgent is the OPTIONAL campaign-level operator_agent delegation
	// override (E25.12 / #1451): when present it wins WHOLESALE as the
	// outermost rung of the resolution ladder (campaign > gate > workflow) for
	// every issue-run of the campaign. Surfaced as the raw JSON block the
	// campaign was created with (omitted when the campaign carries no override,
	// the unchanged-behavior default). Because it wins wholesale for every
	// issue-run, the campaign block IS each issue-run's effective contract when
	// present.
	OperatorAgent json.RawMessage `json:"operator_agent,omitempty"`
	// WorkingDir is the OPTIONAL campaign-level checkout binding (E48.87 /
	// #2527): the absolute path every item run minted from this campaign
	// inherits when the per-item call passes none. omitempty so an unbound
	// campaign carries no working_dir key — the same convention runResponse
	// uses for the run-level binding.
	WorkingDir string    `json:"working_dir,omitempty"`
	CreatedAt  time.Time `json:"created_at"`
	UpdatedAt  time.Time `json:"updated_at"`
	// Idempotent is set true ONLY on an idempotent-replay response: a POST
	// /v0/campaigns whose Idempotency-Key resolved to an existing campaign,
	// returned 200 instead of minting a duplicate at 201 (E25.13 / #1455).
	// omitempty so a fresh create (201), GET, list, and status responses carry
	// no idempotent key — same convention as the ship-artifact endpoints
	// (plan/pull-request/deployment).
	Idempotent bool `json:"idempotent,omitempty"`
	// GroomingSource is the DURABLE provenance of a campaign created from an
	// approved grooming run's ratified order (E54.6 / #2238): the source
	// run/stage/artifact, the report's content hash, the ordered refs, the named
	// exclusions, the applied limit and any acknowledged supersession. Read back
	// from the campaigns.grooming_source column, so it is carried by GET and
	// list responses too — not just the create response. Omitted (omitempty) for
	// every epic_ref / explicit-items campaign.
	GroomingSource json.RawMessage `json:"grooming_source,omitempty"`
}

// campaignItemResponse mirrors docs/api/v0.openapi.yaml's `CampaignItem`
// schema. run_id is omitempty (a *uuid.UUID) so an unlinked item — the
// pre-dispatch default — carries no run_id key rather than a null.
type campaignItemResponse struct {
	ID        uuid.UUID  `json:"id"`
	IssueRef  string     `json:"issue_ref"`
	DependsOn []string   `json:"depends_on"`
	RunID     *uuid.UUID `json:"run_id,omitempty"`
	State     string     `json:"state"`
	// PauseReason records why a paused item was handed off to a human (the
	// page event + run/stage/gate). Omitted (omitempty) unless the item is —
	// or was — paused; mirrors the domain *PauseReason.
	PauseReason *campaign.PauseReason `json:"pause_reason,omitempty"`
	// Position is the item's 0-based place in the campaign queue
	// (campaign_items.queue_position, migration 0074) — for a grooming-sourced
	// campaign, its ratified rank. Always present: it is the field that makes
	// the queue order legible without inferring it from the array index.
	Position  int       `json:"position"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// campaignRollupPayload is the engine's readiness partition over a
// campaign's items (campaign.Eligibility, engine.go). Every slice holds
// issue refs; an item appears in exactly one slice. Mirrors
// docs/api/v0.openapi.yaml's `CampaignRollup` schema. Slices are
// normalized to non-nil so the wire shape is always a JSON array.
type campaignRollupPayload struct {
	Eligible []string `json:"eligible"`
	// HumanLed holds deps-satisfied items carrying autonomy:low — human-led work
	// diverted out of Eligible so the auto-driver never dispatches it. Like the
	// other slices it is always an array (never null).
	HumanLed  []string `json:"human_led"`
	Blocked   []string `json:"blocked"`
	Running   []string `json:"running"`
	Done      []string `json:"done"`
	Failed    []string `json:"failed"`
	Cancelled []string `json:"cancelled"`
	// Paused holds items the auto-driver handed off to a human (E25.7). Like
	// the other slices it is always an array (never null).
	Paused []string `json:"paused"`
}

// campaignNextActionPayload tells the operator-agent what to do next with
// a campaign, distilled from the rollup partition by
// computeCampaignNextAction. Mirrors runNextActionPayload (runs.go): kept
// distinct from any domain type so the API surface is explicit.
type campaignNextActionPayload struct {
	Action   string `json:"action"`
	IssueRef string `json:"issue_ref,omitempty"`
	Detail   string `json:"detail,omitempty"`
}

// createCampaignRequest is the POST /v0/campaigns body: the repo and EITHER an
// epic ref to decompose into the campaign DAG OR an explicit items issue list
// (the no-epic variant, E48.36 / #2051). repo is always required; at least one
// of epic_ref / items must be present.
type createCampaignRequest struct {
	Repo string `json:"repo"`
	// EpicRef is the epic to decompose into the campaign DAG. OPTIONAL as of
	// #2051: when it is absent AND items is present, the campaign is assembled
	// over exactly the named items (the no-epic variant) by resolving each
	// item's depends_on directly. An empty request (neither epic_ref nor items)
	// still fails closed with 400 validation_failed.
	EpicRef string `json:"epic_ref,omitempty"`
	// PausePolicy is the OPTIONAL pause behavior for a gate hand-off (E25.7):
	// "pause_campaign" (block the whole campaign, the default) or "pause_item"
	// (continue-others). Empty normalizes to pause_campaign inside
	// campaign.Persist, so omitting it yields the conservative default.
	PausePolicy string `json:"pause_policy,omitempty"`
	// OperatorAgent is the OPTIONAL campaign-level operator_agent override
	// (E25.12 / #1451): a delegation block that tightens or relaxes the
	// per-workflow contract for ALL the campaign's issue-runs, winning
	// wholesale over the gate/workflow blocks. Carried as raw JSON and
	// validated against spec.OperatorAgent (unknown fields rejected) in the
	// handler before it is stored opaquely. Omit it for no override (each
	// issue-run inherits its workflow's contract — the unchanged default).
	OperatorAgent json.RawMessage `json:"operator_agent,omitempty"`
	// Items is the OPTIONAL subset filter (#2003) / no-epic item list (#2051):
	// issue refs (bare number or issue:N). WITH epic_ref it is a subset filter —
	// every ref must be a child of epic_ref (a non-child fails
	// campaign_item_not_child, 422) and the DAG is built over just these
	// children. WITHOUT epic_ref it is the AUTHORITATIVE set — the campaign
	// assembles over exactly these issues, resolving each one's depends_on
	// directly (the no-epic variant). In BOTH modes an included item whose
	// depends_on targets an issue OUTSIDE the set fails
	// campaign_dangling_dependency, exactly as a cross-epic dangling edge does.
	// Empty/omitted WITH epic_ref sweeps every child — the backward-compatible
	// default; empty/omitted WITHOUT epic_ref fails 400 validation_failed.
	Items []string `json:"items,omitempty"`
	// WorkingDir is the OPTIONAL campaign-level checkout binding (E48.87 /
	// #2527): bound ONCE here, every item run minted from this campaign
	// inherits it when POST /v0/campaigns/{id}/runs passes no per-item
	// working_dir. When non-empty it must be ABSOLUTE (400 validation_failed
	// otherwise) — a relative binding would resolve against the daemon host's
	// cwd and poison every run that inherited it. Omit for no binding: each
	// item run then needs #2498's explicit per-item value (a local one is
	// refused working_dir_required without either).
	WorkingDir string `json:"working_dir,omitempty"`
	// GroomingSource is the OPTIONAL THIRD campaign source (E54.6 / #2238): an
	// approved grooming run whose ratified priority order becomes this
	// campaign's queue. Its PRESENCE selects the source and it is MUTUALLY
	// EXCLUSIVE with both epic_ref and items — silent precedence among three
	// sources is how an operator gets a batch they did not ask for, so the
	// combination is a 400 rather than a precedence rule.
	GroomingSource *groomingSourceRequest `json:"grooming_source,omitempty"`
}

func toCampaignResponse(c *campaign.Campaign) campaignResponse {
	return campaignResponse{
		ID:          c.ID,
		Repo:        c.Repo,
		EpicRef:     c.EpicRef,
		State:       string(c.State),
		PausePolicy: string(c.PausePolicy),
		// Raw JSON passthrough: nil bytes → omitted (omitempty), so a campaign
		// with no override carries no operator_agent key.
		OperatorAgent: json.RawMessage(c.OperatorAgent),
		// The campaign-level checkout binding (E48.87 / #2527). Empty → omitted,
		// so an unbound campaign carries no working_dir key.
		WorkingDir: c.WorkingDir,
		// Durable grooming provenance, raw JSON passthrough (E54.6 / #2238):
		// nil bytes → omitted. Read from the COLUMN, so a campaign names its
		// source order for as long as the row exists — the create response is a
		// convenience, not the system of record.
		GroomingSource: json.RawMessage(c.GroomingSource),
		CreatedAt:      c.CreatedAt,
		UpdatedAt:      c.UpdatedAt,
	}
}

func toCampaignItemResponse(it *campaign.Item) campaignItemResponse {
	deps := it.DependsOn
	if deps == nil {
		deps = []string{}
	}
	return campaignItemResponse{
		ID:          it.ID,
		IssueRef:    it.IssueRef,
		DependsOn:   deps,
		RunID:       it.RunID,
		State:       string(it.State),
		PauseReason: it.PauseReason,
		Position:    it.Position,
		CreatedAt:   it.CreatedAt,
		UpdatedAt:   it.UpdatedAt,
	}
}

// toCampaignRollupPayload maps a campaign.Eligibility partition onto the
// wire payload, normalizing every nil slice to an empty slice so the JSON
// shape is a stable array (never null) for each partition.
//
// The engine's Restartable partition (deps-satisfied, non-human-led cancelled
// items restartable via the operator verb, #1729) has NO dedicated wire slice:
// it is FOLDED BACK into the `cancelled` array so the CampaignRollup wire
// contract (OpenAPI + the fishhawk-mcp client struct) is unchanged. A
// restartable item is still a cancelled item to a rollup reader; the restart
// signal reaches the operator through next_action (start_run), not a new slice.
func toCampaignRollupPayload(e campaign.Eligibility) campaignRollupPayload {
	nz := func(s []string) []string {
		if s == nil {
			return []string{}
		}
		return s
	}
	// Restartable items are cancelled items on the wire — fold them back into
	// the cancelled slice so the rollup shape is unchanged (#1729).
	cancelled := make([]string, 0, len(e.Cancelled)+len(e.Restartable))
	cancelled = append(cancelled, e.Cancelled...)
	cancelled = append(cancelled, e.Restartable...)
	return campaignRollupPayload{
		Eligible:  nz(e.Eligible),
		HumanLed:  nz(e.HumanLed),
		Blocked:   nz(e.Blocked),
		Running:   nz(e.Running),
		Done:      nz(e.Done),
		Failed:    nz(e.Failed),
		Cancelled: cancelled,
		Paused:    nz(e.Paused),
	}
}

// computeCampaignNextAction distills the rollup partition into the single
// next step for the operator-agent. PRECEDENCE is FORWARD-PROGRESS-first and
// STRICT, in this order (#1838 reordered this so a failed item no longer
// suppresses sibling dispatch):
//
//  1. any paused item -> "resume" on the first paused ref. The auto-driver
//     handed a gate off to a human (E25.7); the campaign is stalled until the
//     gate is handled and the operator resumes it. Ranked first so a paused
//     hand-off is surfaced before new dispatch.
//  2. else any eligible (autonomous) item -> "start_run" on the first eligible
//     ref. start_run WINS over attend_human_led AND over a stuck failed item:
//     whenever ANY autonomous item is dispatchable it is surfaced first, so a
//     failed or human-led item never stalls DAG-independent autonomous work
//     (#1838: a quarantined failed item leaves its eligible siblings dispatchable).
//  3. else any restartable (cancelled OR failed, deps-satisfied, non-human-led)
//     item -> "start_run" on the first restartable ref. A cancelled (#1729) or
//     failed (#1838) item with a satisfied DAG position has a forward path via
//     the operator restart verb: surfaced as start_run so dependents no longer
//     stay blocked forever. Ranked below Eligible (a fresh unstarted item is
//     preferred over a restart) and above HumanLed.
//  4. else any human-led item -> "attend_human_led" on the first human-led ref.
//     Fires ONLY when len(Eligible)==0 && len(Restartable)==0 && len(HumanLed)>0
//     — every deps-satisfied item that remains is autonomy:low, reserved for
//     human leadership, so the operator (not an auto-driver) must pick it up.
//  5. else any failed item -> "attention" on the first failed ref. Only a
//     GENUINELY-STUCK failed item reaches here — deps-unsatisfied or human-led,
//     so NextEligible left it in Failed rather than diverting it to Restartable
//     (#1838). It cannot be auto-restarted (no satisfied forward path), so the
//     operator must resolve its run manually. Ranked below the dispatch arms so a
//     stuck failure never suppresses still-actionable sibling work.
//  6. else any running or blocked item -> "wait".
//  7. else (every item terminal done/cancelled) -> "complete".
//
// TERMINAL POST-FILTER (#2681). The seven arms above read ONLY the item
// partition, so a campaign that has itself gone TERMINAL could still advertise
// a verb the campaign-state gate refuses — the wedge this filter closes. When
// state.IsTerminal() the base action is rewritten:
//
//   - start_run on a Restartable item under a FAILED campaign is KEPT (with a
//     reopen-flavoured detail): handleStartCampaignItemRun now admits a
//     terminal-failed campaign and reopens it atomically, so this verb is
//     genuinely legal;
//   - complete is KEPT: a terminal campaign whose every item is done advertises
//     no verb at all, so it is already truthful;
//   - everything else becomes the `closed` action, which advertises no campaign
//     verb and points the operator at driving the leftover issue standalone.
//
// A NON-terminal campaign is byte-identical to the pre-#2681 behavior. In
// particular a PAUSED campaign is not terminal, so it still advertises `resume`
// — and that is correct, not an omission: resume is legal on a paused campaign
// (POST /resume accepts it), so the "never advertise a refused verb" invariant
// already holds there. Reporting `closed` for a paused campaign would be the
// actual defect: it would tell the operator to abandon a campaign that is one
// verb away from continuing.
func computeCampaignNextAction(state campaign.State, e campaign.Eligibility) campaignNextActionPayload {
	base := baseCampaignNextAction(e)
	if !state.IsTerminal() {
		return base
	}
	switch {
	case base.Action == "start_run" && state == campaign.StateFailed && containsRef(e.Restartable, base.IssueRef):
		// The one verb that IS legal against a terminal campaign: the operator
		// restart, which reopens the failed campaign and resets the item in one
		// atomic repository call (campaign.Repository.ReopenCampaignForItemRestart).
		base.Detail = "this item failed but its dependencies are satisfied; fishhawk_start_campaign_item_run REOPENS the terminal-failed campaign (flipping it back to running) and resets the item in one atomic step, then mints a fresh run"
		return base
	case base.Action == "complete":
		// Every item terminal-and-done: no verb is advertised, so nothing to fix.
		return base
	default:
		// Counts mirror the WIRE rollup the operator reads: toCampaignRollupPayload
		// folds Restartable back into the cancelled slice, so this fold matches.
		cancelled := len(e.Cancelled) + len(e.Restartable)
		return campaignNextActionPayload{
			Action: "closed",
			// Carry the ref the base arm named (when it named one) so the operator
			// still has the stranded issue to drive standalone.
			IssueRef: base.IssueRef,
			Detail: fmt.Sprintf(
				"the campaign is %s and can start no further item runs (%d succeeded, %d failed, %d cancelled); drive any remaining issue standalone with fishhawk_start_run — the campaign will not track its outcome",
				state, len(e.Done), len(e.Failed), cancelled),
		}
	}
}

// baseCampaignNextAction is the item-partition-only computation: the seven-arm
// precedence documented on computeCampaignNextAction, unchanged since #1838. It
// is split out so the terminal post-filter reads as a filter over a stable base
// rather than as seven more interleaved conditions.
func baseCampaignNextAction(e campaign.Eligibility) campaignNextActionPayload {
	switch {
	case len(e.Paused) > 0:
		return campaignNextActionPayload{
			Action:   "resume",
			IssueRef: e.Paused[0],
			Detail:   "the auto-driver paged a human at a run gate; handle the gate then POST /resume to continue the campaign",
		}
	case len(e.Eligible) > 0:
		return campaignNextActionPayload{
			Action:   "start_run",
			IssueRef: e.Eligible[0],
			Detail:   "this item's dependencies are satisfied and it has no run yet",
		}
	case len(e.Restartable) > 0:
		return campaignNextActionPayload{
			Action:   "start_run",
			IssueRef: e.Restartable[0],
			Detail:   "this item was cancelled or failed but its dependencies are satisfied; restart it via fishhawk_start_campaign_item_run to unblock its dependents",
		}
	case len(e.HumanLed) > 0:
		return campaignNextActionPayload{
			Action:   "attend_human_led",
			IssueRef: e.HumanLed[0],
			Detail:   "this item's dependencies are satisfied but it is autonomy:low (human-led); a human must lead it — do not dispatch an agent run",
		}
	case len(e.Failed) > 0:
		return campaignNextActionPayload{
			Action:   "attention",
			IssueRef: e.Failed[0],
			Detail:   "a campaign item failed and cannot be auto-restarted (its dependencies are unsatisfied or it is human-led); resolve its run manually before the campaign can proceed",
		}
	case len(e.Running) > 0 || len(e.Blocked) > 0:
		return campaignNextActionPayload{
			Action: "wait",
			Detail: "items are running or blocked on a dependency; nothing to dispatch yet",
		}
	default:
		return campaignNextActionPayload{
			Action: "complete",
			Detail: "every campaign item reached a terminal state",
		}
	}
}

// handleCreateCampaign implements POST /v0/campaigns. It assembles a
// campaign from an epic ref — querying the epic's children + depends_on
// edges via the workmgmt provider, wave-ordering them with
// campaign.Assemble, and persisting the result with campaign.Persist — and
// returns the created campaign.
//
// This is a RUNLESS create: there is no run row, so it resolves the repo's
// GitHub App installation directly via s.cfg.GitHub.GetRepoInstallation
// (the same path POST /v0/runs uses at runs.go:498), which the real GitHub
// provider needs to mint an installation token for the children query.
//
// Idempotency-Key (E25.13 / #1455) is honoured, mirroring POST /v0/runs
// (runs.go): when the header is set, a previously-created campaign with the
// same (repo, key) is returned 200 with idempotent:true instead of minting a
// duplicate at 201. An empty header is equivalent to "not idempotent" — every
// call mints a new campaign.
func (s *Server) handleCreateCampaign(w http.ResponseWriter, r *http.Request) {
	if !s.requireWriteScope(w, r, "write:campaigns") {
		return
	}
	if s.cfg.CampaignRepo == nil {
		s.writeError(w, r, http.StatusServiceUnavailable, "campaign_repo_unconfigured",
			"campaigns endpoint requires a configured campaign repository", nil)
		return
	}

	var req createCampaignRequest
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		s.writeError(w, r, http.StatusBadRequest, "validation_failed",
			"request body is not valid JSON or contains unknown fields",
			map[string]any{"error": err.Error()})
		return
	}

	owner, name, ok := splitRepoFullName(req.Repo)
	if !ok {
		s.writeError(w, r, http.StatusBadRequest, "validation_failed",
			"repo must be in owner/name form",
			map[string]any{"field": "repo", "got": req.Repo})
		return
	}
	// epic_ref is OPTIONAL as of #2051, but an empty request still fails closed:
	// at least one of epic_ref / items must be present. epic_ref PRESENT keeps
	// the epic-sweep + optional items-subset path byte-identical; epic_ref ABSENT
	// with items present routes to the no-epic branch below.
	epicRef := strings.TrimSpace(req.EpicRef)
	if epicRef == "" && len(req.Items) == 0 && req.GroomingSource == nil {
		s.writeError(w, r, http.StatusBadRequest, "validation_failed",
			"one of epic_ref, items or grooming_source is required", map[string]any{"field": "epic_ref"})
		return
	}
	// grooming_source is the THIRD source and is MUTUALLY EXCLUSIVE with the
	// other two. Refusal, not precedence: three sources with a silent winner is
	// how an operator gets a campaign they did not ask for. Validated HERE, with
	// the other body checks and BEFORE the Idempotency-Key lookup and the
	// installation resolution, so the conflict costs no forge round-trip.
	if req.GroomingSource != nil {
		if epicRef != "" {
			s.writeError(w, r, http.StatusBadRequest, "validation_failed",
				"grooming_source cannot be combined with epic_ref: an approved grooming order IS the item set",
				map[string]any{"field": "grooming_source", "conflicts_with": "epic_ref"})
			return
		}
		if len(req.Items) > 0 {
			s.writeError(w, r, http.StatusBadRequest, "validation_failed",
				"grooming_source cannot be combined with items: an approved grooming order IS the item set",
				map[string]any{"field": "grooming_source", "conflicts_with": "items"})
			return
		}
		if strings.TrimSpace(req.GroomingSource.RunID) == "" {
			s.writeError(w, r, http.StatusBadRequest, "validation_failed",
				"grooming_source.run_id is required", map[string]any{"field": "grooming_source.run_id"})
			return
		}
		if req.GroomingSource.Limit < 0 {
			s.writeError(w, r, http.StatusBadRequest, "validation_failed",
				"grooming_source.limit must be >= 0 (0 means no cap)",
				map[string]any{"field": "grooming_source.limit", "got": req.GroomingSource.Limit})
			return
		}
	}
	// pause_policy is optional; an empty value normalizes to pause_campaign in
	// campaign.Persist. A non-empty value must be a recognized policy, caught
	// here so a typo surfaces a 400 rather than a DB CHECK violation at insert.
	if req.PausePolicy != "" && !validPausePolicies[req.PausePolicy] {
		s.writeError(w, r, http.StatusBadRequest, "validation_failed",
			"pause_policy must be one of pause_campaign, pause_item",
			map[string]any{"field": "pause_policy", "got": req.PausePolicy})
		return
	}
	// operator_agent is OPTIONAL. When present, it must be a well-formed
	// spec.OperatorAgent block (unknown fields rejected) — validated HERE so a
	// malformed override surfaces a 400 at create rather than being stored
	// opaquely and failing later at the auto-driver consumer (slice B). The
	// validated raw bytes are stored verbatim; the campaign package stays
	// spec-free. A JSON `null` (or omission) is treated as "no override".
	operatorAgentBytes, ok := validateOperatorAgent(req.OperatorAgent)
	if !ok {
		s.writeError(w, r, http.StatusBadRequest, "validation_failed",
			"operator_agent must be a valid operator-agent block",
			map[string]any{"field": "operator_agent"})
		return
	}
	// working_dir is the OPTIONAL campaign-level checkout binding (E48.87 /
	// #2527). When non-empty it must be ABSOLUTE — the same guard
	// handleStartCampaignItemRun and handleCreateRun apply to their per-run
	// bindings, applied here because a relative binding stored on the campaign
	// would be inherited by every item run and resolve against the daemon host's
	// cwd rather than the operator's project (the #1866 contamination shape,
	// multiplied across the batch). Validated HERE — with the other body checks,
	// BEFORE the Idempotency-Key lookup and the installation resolution — so a
	// bad value costs no forge round-trip and the refusal is reachable without a
	// live forge.
	if req.WorkingDir != "" && !filepath.IsAbs(req.WorkingDir) {
		s.writeError(w, r, http.StatusBadRequest, "validation_failed",
			"working_dir must be an absolute path",
			map[string]any{"field": "working_dir", "got": req.WorkingDir})
		return
	}

	// Idempotency-Key (E25.13 / #1455). When set, a previously-created campaign
	// with the same (repo, key) is returned 200 + idempotent:true instead of
	// minting + dispatching a duplicate. Resolved BEFORE the installation +
	// epic-children query so a replay does no GitHub work. Empty header is
	// equivalent to "not idempotent" — every call mints a new campaign. The
	// three-branch shape (hit / ErrNotFound / other error) mirrors runs.go.
	idempKey := strings.TrimSpace(r.Header.Get("Idempotency-Key"))
	if idempKey != "" {
		existing, err := s.cfg.CampaignRepo.GetCampaignByIdempotencyKey(r.Context(), req.Repo, idempKey)
		switch {
		case err == nil:
			// Replay: return the prior campaign with 200 + idempotent:true.
			resp := toCampaignResponse(existing)
			resp.Idempotent = true
			s.writeJSON(w, r, http.StatusOK, resp)
			return
		case errors.Is(err, campaign.ErrNotFound):
			// First call with this key — fall through to create.
		default:
			s.writeError(w, r, http.StatusInternalServerError, "internal_error",
				"idempotency lookup failed", map[string]any{"error": err.Error()})
			return
		}
	}

	// Resolve the App installation for the target repo (#713 / runs.go:498).
	// A runless create has no run row to carry the id, so resolve it directly:
	// the real GitHub provider needs it to query the epic's children.
	var scope forge.CredentialScope
	if s.cfg.GitHub != nil {
		id, err := s.cfg.GitHub.GetRepoInstallation(r.Context(), forge.RepoRef{Owner: owner, Name: name})
		switch {
		case err == nil:
			scope = forge.FromGitHubInstallationID(id)
		case errors.Is(err, forge.ErrNotInstalled):
			s.writeError(w, r, http.StatusUnprocessableEntity, "repo_not_installed",
				"GitHub App is not installed on the target repository",
				map[string]any{"repo": req.Repo})
			return
		default:
			s.writeError(w, r, http.StatusBadGateway, "installation_resolution_failed",
				"could not resolve the GitHub App installation for the target repo",
				map[string]any{"error": err.Error()})
			return
		}
	}

	// Resolve the work-management provider and its optional epic-children
	// query capability, exactly as POST /v0/work-items does (workitems.go).
	conv, err := conventionsLoader(r.Context(), req.Repo)
	if err != nil {
		s.writeError(w, r, http.StatusInternalServerError, "internal_error",
			"could not load work-management conventions", map[string]any{"error": err.Error()})
		return
	}
	provider, err := workmgmt.Get(conv.Provider)
	if err != nil {
		var unk *workmgmt.UnknownProviderError
		if errors.As(err, &unk) {
			// Static literal message (E67.15 / #2587): the same product-owned
			// facts ride the allow-listed provider/registered detail keys, so
			// nothing is lost from the CALLER's view, and unk.Error() as the
			// message is a raw-cause syntax the AST guard flags. The cause
			// itself rides internalCauseKey so writeError still joins it to
			// error_ref in the operator log rather than dropping it (E67.29 /
			// #2631, operator condition 3) — the AST guard can prove the
			// message is static but not that the cause is retained, which is
			// what TestCreateCampaign_UnknownProvider_RedactsCauseJoinsInLog
			// asserts behaviourally.
			s.writeError(w, r, http.StatusNotImplemented, "provider_unimplemented",
				"the resolved work-item provider is not implemented",
				map[string]any{
					"provider":       unk.ID,
					"registered":     unk.Known,
					internalCauseKey: unk.Error(),
				})
			return
		}
		s.writeError(w, r, http.StatusInternalServerError, "internal_error",
			"could not resolve work-item provider", map[string]any{"error": err.Error()})
		return
	}
	// The filing Target is shared by both branches (epic sweep + no-epic
	// item-set resolution).
	target := workmgmt.Target{
		Repo:    workmgmt.Repo{Owner: owner, Name: name},
		Scope:   scope,
		Project: conv.Project,
		Jira:    conv.Jira,
	}

	// THE THIRD SOURCE (E54.6 / #2238). Resolve the approved grooming order
	// BEFORE the branch below, so its rank-ordered issue refs become the input
	// to the EXISTING #2051 no-epic item path — no bespoke assembly, board write
	// or dispatch path is added. The whole ladder runs against local
	// run/artifact/approval state, so every refusal is reachable without a live
	// forge. The order is read EXACTLY ONCE, here; nothing downstream re-reads a
	// report or a board.
	var groomingOrder *campaign.GroomingOrder
	var groomingProvenance *campaignGroomingSourcePayload
	itemRefs := req.Items
	if req.GroomingSource != nil {
		var gerr error
		groomingOrder, groomingProvenance, gerr = s.resolveGroomingOrder(r.Context(), owner, name, req.Repo, req.GroomingSource)
		if gerr != nil {
			var gse *groomingSourceError
			if errors.As(gerr, &gse) {
				s.writeError(w, r, gse.Status, gse.Code, gse.Message, gse.Details)
				return
			}
			s.writeError(w, r, http.StatusInternalServerError, "internal_error",
				"could not resolve the grooming order", map[string]any{"error": gerr.Error()})
			return
		}
		itemRefs = groomingOrder.Refs
	}

	// Resolve the epic-children (or no-epic item-set) result to assemble from.
	// epic_ref PRESENT keeps the EpicChildrenQuerier sweep + optional
	// FilterToSubset path byte-identical; epic_ref ABSENT + items present routes
	// to the IssueSetDependencyResolver, which resolves each named issue's
	// depends_on directly (there is no epic sweep to derive the edge set from,
	// #2051). Both branches feed the same *EpicChildrenResult into
	// campaign.Assemble below.
	var result *workmgmt.EpicChildrenResult
	if epicRef != "" {
		querier, ok := provider.(workmgmt.EpicChildrenQuerier)
		if !ok {
			s.writeError(w, r, http.StatusNotImplemented, "epic_children_unsupported",
				"the configured work-item provider cannot query epic children",
				map[string]any{"provider": conv.Provider})
			return
		}

		result, err = querier.EpicChildren(r.Context(), workmgmt.EpicChildrenRequest{
			Target: target,
			Epic:   req.EpicRef,
		})
		if err != nil {
			s.writeError(w, r, http.StatusBadGateway, "epic_children_query_failed",
				"could not query the epic's children",
				map[string]any{"error": err.Error()})
			return
		}

		// Narrow the children to the OPTIONAL requested subset (#2003) before
		// assembly. FilterToSubset fails closed on a ref that is not a child of the
		// epic (campaign_item_not_child) and re-classifies an included->excluded
		// depends_on into a dropped edge, which Assemble then surfaces as a dangling
		// dependency. Empty/omitted items is a no-op that sweeps every child.
		result, err = campaign.FilterToSubset(result, req.Items)
		if err != nil {
			if errors.Is(err, campaign.ErrItemNotChild) {
				s.writeError(w, r, http.StatusUnprocessableEntity, "campaign_item_not_child",
					err.Error(), map[string]any{"epic_ref": req.EpicRef, "items": req.Items})
				return
			}
			s.writeError(w, r, http.StatusInternalServerError, "internal_error",
				"filter campaign items to subset failed", map[string]any{"error": err.Error()})
			return
		}
	} else {
		// No-epic variant (#2051): items is the authoritative set. Resolve each
		// named issue's depends_on directly. A provider that cannot resolve an
		// arbitrary issue set (jira is interface-only in v0) fails 501, mirroring
		// the epic_children_unsupported shape.
		resolver, ok := provider.(workmgmt.IssueSetDependencyResolver)
		if !ok {
			s.writeError(w, r, http.StatusNotImplemented, "issue_set_resolution_unsupported",
				"the configured work-item provider cannot resolve an explicit issue set without an epic",
				map[string]any{"provider": conv.Provider})
			return
		}
		result, err = resolver.ResolveDependencies(r.Context(), workmgmt.IssueSetRequest{
			Target: target,
			// itemRefs is req.Items for the #2051 no-epic variant and the
			// grooming order's rank-ordered refs for the #2238 variant — one
			// resolution path, two ways of naming the set.
			Items: itemRefs,
		})
		if err != nil {
			s.writeError(w, r, http.StatusBadGateway, "issue_set_resolution_failed",
				"could not resolve the campaign's issue set",
				map[string]any{"error": err.Error()})
			return
		}
	}

	// PERMUTE THE RESOLVED CHILDREN INTO RATIFIED RANK ORDER. Assemble stamps
	// each item's queue position from its index in res.Children, so this is what
	// makes the approved order the order items are CREATED in and therefore the
	// order ListCampaignItemsForCampaign (queue_position ASC) returns them and
	// the engine's Eligible slice works. Edges/DroppedEdges are untouched:
	// Assemble builds its index map from the actual Children order, so the DAG
	// is invariant under the permutation. A set mismatch is a 500, not a 4xx —
	// it means the provider returned a set the order did not name, which is a
	// bug rather than operator input.
	if groomingOrder != nil {
		result, err = campaign.ReorderByPriority(result, groomingOrder.Numbers)
		if err != nil {
			s.writeError(w, r, http.StatusInternalServerError, "internal_error",
				"the resolved issue set does not match the grooming order",
				map[string]any{"error": err.Error()})
			return
		}
	}

	// Assemble the wave-ordered DAG, failing closed on a dangling dependency
	// (a depends_on target outside the assembled set) or a dependency cycle. The
	// no-epic branch assembles with an empty EpicRef, persisting a no-epic
	// campaign (campaigns.epic_ref is TEXT NOT NULL with no CHECK, so "" is valid
	// and denotes the items-only variant). Assemble on the TRIMMED epicRef, not
	// raw req.EpicRef: a whitespace-only epic_ref routes to the no-epic branch
	// above (epicRef == "") but must PERSIST the empty-string no-epic sentinel,
	// not "   " — a future surface parsing Campaign.EpicRef expects "" for the
	// items-only variant (README: empty-string EpicRef). For a well-formed epic
	// ref TrimSpace is a no-op, so the epic path stays byte-identical.
	assembly, err := campaign.Assemble(epicRef, result)
	if err != nil {
		switch {
		case errors.Is(err, campaign.ErrDanglingDependency):
			// Enrich the details with the categorized edge lists so the MCP tool
			// can name the real cause+remedy per cause without string-parsing the
			// message (#2120). A defensively-wrapped dangling error (no typed
			// form) still maps to the 422 with just epic_ref.
			s.writeError(w, r, http.StatusUnprocessableEntity, "campaign_dangling_dependency",
				err.Error(), groomingDanglingDetails(DanglingDependencyDetails(err, req.EpicRef), groomingOrder))
			return
		case errors.Is(err, campaign.ErrCycle):
			s.writeError(w, r, http.StatusBadRequest, "validation_failed",
				err.Error(), map[string]any{"epic_ref": req.EpicRef})
			return
		default:
			s.writeError(w, r, http.StatusBadRequest, "validation_failed",
				err.Error(), map[string]any{"epic_ref": req.EpicRef})
			return
		}
	}

	// Thread the create request's pause_policy onto the assembly so it reaches
	// the persisted campaign (the call-site update deferred from slice 1; a
	// zero value is normalized to pause_campaign inside campaign.Persist).
	assembly.PausePolicy = campaign.PausePolicy(req.PausePolicy)
	// Thread the validated campaign-level operator_agent override onto the
	// assembly (E25.12). Nil = no override; the campaign inherits each
	// issue-run's workflow contract unchanged.
	assembly.OperatorAgent = operatorAgentBytes
	// Thread the validated campaign-level checkout binding onto the assembly so
	// it reaches the persisted campaign (E48.87 / #2527). Empty = no binding;
	// every item run then falls back to #2498's explicit per-item working_dir.
	assembly.WorkingDir = req.WorkingDir
	// Thread the non-empty Idempotency-Key onto the assembly so it is stored on
	// the campaign (E25.13), making a later replay resolvable. Empty = nil = no
	// key (the unchanged default).
	if idempKey != "" {
		assembly.IdempotencyKey = &idempKey
	}

	// Thread the DURABLE grooming provenance onto the assembly so it rides the
	// campaign's OWN single-row INSERT (E54.6 / #2238). This is why a
	// grooming-sourced campaign can never exist unprovenanced: campaign.Persist
	// is not transactional and the audit emit below is best-effort, so a
	// provenance record written after the fact could be lost while the campaign
	// survived.
	if groomingProvenance != nil {
		raw, merr := json.Marshal(groomingProvenance)
		if merr != nil {
			s.writeError(w, r, http.StatusInternalServerError, "internal_error",
				"could not encode the grooming provenance", map[string]any{"error": merr.Error()})
			return
		}
		assembly.GroomingSource = raw
	}

	created, err := campaign.Persist(r.Context(), s.cfg.CampaignRepo, req.Repo, assembly)
	if err != nil {
		s.writeError(w, r, http.StatusInternalServerError, "internal_error",
			"persist campaign failed", map[string]any{"error": err.Error()})
		return
	}

	// The audit entry is a SECOND copy of the provenance, for the chain. It is
	// best-effort, exactly like every other campaign audit emit — which is
	// precisely why it is not the system of record: the campaigns.grooming_source
	// column above is.
	if groomingProvenance != nil {
		s.emitCampaignAudit(r.Context(), auditCampaignGroomingSourceResolved, map[string]any{
			"campaign_id":     created.ID.String(),
			"repo":            created.Repo,
			"grooming_source": groomingProvenance,
		})
	}

	s.writeJSON(w, r, http.StatusCreated, toCampaignResponse(created))
}

// Detail keys the campaign_dangling_dependency error carries, one per cause
// (#2120). They are the CONTRACT between this producer and the fishhawk-mcp
// consumer (campaign.go's startCampaign), which branches its operator remedy on
// their presence — so the source-to-consumer test drives DanglingDependencyDetails
// (below) and asserts the operator message rather than pinning the seam only with
// matching string literals in two files.
const (
	danglingNotChildKey           = "dangling_not_child"
	danglingExcludedIncompleteKey = "dangling_excluded_incomplete"
)

// DanglingDependencyDetails builds the campaign_dangling_dependency error's
// details map from a (possibly-wrapped) assembly error, categorizing the
// blocking edges so the MCP tool can name the real cause+remedy per cause
// without string-parsing the message (#2120). It always carries epic_ref; a
// typed *campaign.DanglingDependencyError additionally contributes the
// dangling_not_child / dangling_excluded_incomplete edge lists. A defensively-
// wrapped dangling error (no typed form) maps to just epic_ref. Exported so the
// fishhawk-mcp source-to-consumer test can thread the REAL details map into the
// operator-message render (binding condition 1(b) / #2120), rather than feeding
// the consumer hand-written details JSON.
func DanglingDependencyDetails(err error, epicRef string) map[string]any {
	details := map[string]any{"epic_ref": epicRef}
	var de *campaign.DanglingDependencyError
	if errors.As(err, &de) {
		if refs := edgeRefs(de.NotChild); len(refs) > 0 {
			details[danglingNotChildKey] = refs
		}
		if refs := edgeRefs(de.ExcludedIncomplete); len(refs) > 0 {
			details[danglingExcludedIncompleteKey] = refs
		}
	}
	return details
}

// edgeRefs formats dropped depends_on edges as "issue:From->issue:To" strings
// for the campaign_dangling_dependency details map, so the MCP tool can branch
// its operator message on which categories are present (#2120). Returns nil for
// an empty slice so the caller omits the details key.
func edgeRefs(edges []workmgmt.DependsEdge) []string {
	if len(edges) == 0 {
		return nil
	}
	refs := make([]string, 0, len(edges))
	for _, e := range edges {
		refs = append(refs, "issue:"+strconv.Itoa(e.From)+"->issue:"+strconv.Itoa(e.To))
	}
	return refs
}

// handleListCampaigns implements GET /v0/campaigns. Offset-cursor
// paginated, optional repo + state filters, mirroring handleListRuns.
func (s *Server) handleListCampaigns(w http.ResponseWriter, r *http.Request) {
	if s.cfg.CampaignRepo == nil {
		s.writeError(w, r, http.StatusServiceUnavailable, "campaign_repo_unconfigured",
			"campaigns endpoint requires a configured campaign repository", nil)
		return
	}
	q := r.URL.Query()
	limit, err := parseLimit(q.Get("limit"), campaignsDefaultLimit, campaignsMaxLimit)
	if err != nil {
		s.writeError(w, r, http.StatusBadRequest, "validation_failed",
			err.Error(), map[string]any{"field": "limit"})
		return
	}
	offset, err := decodeOffsetCursor(q.Get("cursor"))
	if err != nil {
		s.writeError(w, r, http.StatusBadRequest, "cursor_invalid", err.Error(), nil)
		return
	}
	stateFilter := q.Get("state")
	if stateFilter != "" {
		if _, ok := validCampaignStates[stateFilter]; !ok {
			s.writeError(w, r, http.StatusBadRequest, "validation_failed",
				"state must be one of pending, running, paused, succeeded, failed, cancelled",
				map[string]any{"field": "state", "got": stateFilter})
			return
		}
	}

	// Scope the listing to the caller's workspace account (ADR-057 /
	// #1830), mirroring handleListRuns: a tenanted caller sees their
	// account's campaigns plus untenanted (NULL account_id) ones; an
	// untenanted caller (empty AccountID) is unconstrained, per
	// ListCampaignsFilter.AccountID's contract.
	accountFilter := IdentityFrom(r.Context()).AccountID

	// Fetch one extra row to compute the next cursor without a COUNT, the
	// same limit+1 trick handleListRuns uses.
	rows, err := s.cfg.CampaignRepo.ListCampaigns(r.Context(), campaign.ListCampaignsFilter{
		Repo:      q.Get("repo"),
		State:     stateFilter,
		AccountID: accountFilter,
		Limit:     limit + 1,
		Offset:    offset,
	})
	if err != nil {
		s.writeError(w, r, http.StatusInternalServerError, "internal_error",
			"list campaigns failed", map[string]any{"error": err.Error()})
		return
	}

	var nextCursor string
	if len(rows) > limit {
		nextCursor = encodeOffsetCursor(offset + limit)
		rows = rows[:limit]
	}
	// Repo-scoped narrowing (ADR-057 Amendment A2 / #2071), mirroring
	// handleListRuns: drop page rows whose repo the caller holds no forge
	// `read` on, strictly on top of the account filter. The cursor is computed
	// from the PRE-filter count, so a page can come back short while
	// next_cursor stays non-empty — following it to exhaustion still yields
	// every visible campaign exactly once.
	filter, ok := s.requestRepoFilter(w, r)
	if !ok {
		return
	}
	items := make([]campaignResponse, 0, len(rows))
	for _, c := range rows {
		allowed, ferr := filter.allows(r.Context(), c.Repo)
		if ferr != nil {
			s.writeRepoFilterUnavailable(w, r)
			return
		}
		if !allowed {
			continue
		}
		items = append(items, toCampaignResponse(c))
	}
	s.writeJSON(w, r, http.StatusOK, map[string]any{
		"items":       items,
		"next_cursor": nextCursor,
	})
}

// validCampaignStates pins the closed set per docs/api/v0.openapi.yaml,
// defense-in-depth at the handler so a typo'd state filter surfaces a 400
// rather than reaching the DB layer.
var validCampaignStates = map[string]struct{}{
	string(campaign.StatePending):   {},
	string(campaign.StateRunning):   {},
	string(campaign.StatePaused):    {},
	string(campaign.StateSucceeded): {},
	string(campaign.StateFailed):    {},
	string(campaign.StateCancelled): {},
}

// validPausePolicies pins the closed set of create-request pause_policy values
// per docs/api/v0.openapi.yaml, defense-in-depth at the handler so a typo
// surfaces a 400 rather than reaching the column CHECK at insert. The empty
// value is intentionally NOT here — it is the "omit, take the default" signal
// normalized to pause_campaign inside campaign.Persist.
var validPausePolicies = map[string]bool{
	string(campaign.PausePolicyPauseCampaign): true,
	string(campaign.PausePolicyPauseItem):     true,
}

// validateOperatorAgent validates the OPTIONAL campaign-level operator_agent
// override carried as raw JSON. It returns the bytes to store plus ok:
//   - empty/absent (len 0) or a JSON `null` literal -> (nil, true): no override
//     (each issue-run inherits its workflow contract — the unchanged default).
//   - a well-formed spec.OperatorAgent block (unknown fields rejected) ->
//     (raw, true): stored verbatim, opaque to the campaign package.
//   - anything else — malformed JSON, an unknown field, or a non-object —
//     -> (nil, false): the handler answers 400 validation_failed.
//
// Validating against the spec.OperatorAgent Go type with DisallowUnknownFields
// IS the v0 validation (no separate JSON schema), consistent with how
// pause_policy is validated in Go. The reused spec type is why no
// docs/spec/*.schema.json change — and so no schema-sync gate — is triggered.
func validateOperatorAgent(raw json.RawMessage) ([]byte, bool) {
	if len(raw) == 0 || string(bytes.TrimSpace(raw)) == "null" {
		return nil, true
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var oa spec.OperatorAgent
	if err := dec.Decode(&oa); err != nil {
		return nil, false
	}
	return raw, true
}

// handleGetCampaign implements GET /v0/campaigns/{campaign_id}.
func (s *Server) handleGetCampaign(w http.ResponseWriter, r *http.Request) {
	if s.cfg.CampaignRepo == nil {
		s.writeError(w, r, http.StatusServiceUnavailable, "campaign_repo_unconfigured",
			"campaigns endpoint requires a configured campaign repository", nil)
		return
	}
	id, err := uuid.Parse(r.PathValue("campaign_id"))
	if err != nil {
		s.writeError(w, r, http.StatusBadRequest, "validation_failed",
			"campaign_id must be a valid UUID",
			map[string]any{"field": "campaign_id", "got": r.PathValue("campaign_id")})
		return
	}
	c, err := s.cfg.CampaignRepo.GetCampaign(r.Context(), id)
	if err != nil {
		if errors.Is(err, campaign.ErrNotFound) {
			s.writeError(w, r, http.StatusNotFound, "campaign_not_found",
				"no campaign with that id", map[string]any{"campaign_id": id.String()})
			return
		}
		s.writeError(w, r, http.StatusInternalServerError, "internal_error",
			"get campaign failed", map[string]any{"error": err.Error()})
		return
	}
	if !s.enforceCampaignAccount(w, r, id) {
		return
	}
	// Repo visibility (#2071): campaigns are NOT covered by the run-scoped
	// account middleware wrappers, so each campaign point read applies the
	// check itself. Point reads DENY (403 repo_forbidden) where lists filter.
	if !s.enforceRepoVisibility(w, r, c.Repo) {
		return
	}
	s.writeJSON(w, r, http.StatusOK, toCampaignResponse(c))
}

// enforceCampaignAccount applies the ownership check for an
// already-resolved campaign (ADR-057 / #1830), mirroring enforceAccount's
// posture for runs: a tenanted campaign whose account disagrees with the
// caller's Identity.AccountID → 403 account_forbidden; an untenanted
// campaign (NULL account_id → "") is allowed — the NULL-allow window a
// later E44 child closes. The account is read via the
// campaign.AccountGetter method (the domain Campaign type doesn't carry the
// column). The lookup is UNCONDITIONAL: AccountGetter is a REQUIRED part of
// campaign.Repository (E44.11 / #2074), so there is no longer a
// capability-absent branch that could degrade the gate to untenanted-allow —
// a repo cannot escape this check by not carrying the method.
//
// A lookup ERROR fails CLOSED (503), never open: a transient DB failure on
// the account probe must NOT fall through to untenanted-allow and disclose
// the already-resolved tenanted campaign to a caller from another account.
// This mirrors the mcp:run identity's account resolution in bearerAuth,
// whose written contract is "any lookup ERROR fails CLOSED with 503".
// Returns true when the request may proceed; on denial it writes the error
// envelope and returns false.
func (s *Server) enforceCampaignAccount(w http.ResponseWriter, r *http.Request, id uuid.UUID) bool {
	if s.cfg.CampaignRepo == nil {
		// Unconfigured repo: fail CLOSED like any other unresolvable
		// account. The sole caller (handleGetCampaign) rejects a nil repo
		// with campaign_repo_unconfigured before reaching here, so this is
		// belt-and-braces for the helper called directly — but it must NOT
		// be an allow, which is the shape of the bypass #2074/#2082 closed.
		s.writeDBUnavailable(w, r)
		return false
	}
	acct, err := s.cfg.CampaignRepo.GetCampaignAccountID(r.Context(), id)
	if err != nil {
		// Fail CLOSED: a probe error must never fall through to allow.
		s.writeDBUnavailable(w, r)
		return false
	}
	if acct == "" {
		// Untenanted (NULL account_id): allowed under any caller.
		return true
	}
	if IdentityFrom(r.Context()).AccountID != acct {
		s.writeError(w, r, http.StatusForbidden, "account_forbidden",
			"this campaign belongs to a different workspace account", nil)
		return false
	}
	return true
}

// handleListCampaignItems implements GET /v0/campaigns/{campaign_id}/items.
func (s *Server) handleListCampaignItems(w http.ResponseWriter, r *http.Request) {
	if s.cfg.CampaignRepo == nil {
		s.writeError(w, r, http.StatusServiceUnavailable, "campaign_repo_unconfigured",
			"campaigns endpoint requires a configured campaign repository", nil)
		return
	}
	id, err := uuid.Parse(r.PathValue("campaign_id"))
	if err != nil {
		s.writeError(w, r, http.StatusBadRequest, "validation_failed",
			"campaign_id must be a valid UUID",
			map[string]any{"field": "campaign_id", "got": r.PathValue("campaign_id")})
		return
	}
	// ListCampaignItemsForCampaign returns an empty slice (not ErrNotFound)
	// for an unknown campaign id, so existence is checked explicitly via
	// GetCampaign first — a 404 for a missing campaign is more useful than an
	// ambiguous empty list.
	c, err := s.cfg.CampaignRepo.GetCampaign(r.Context(), id)
	if err != nil {
		if errors.Is(err, campaign.ErrNotFound) {
			s.writeError(w, r, http.StatusNotFound, "campaign_not_found",
				"no campaign with that id", map[string]any{"campaign_id": id.String()})
			return
		}
		s.writeError(w, r, http.StatusInternalServerError, "internal_error",
			"get campaign failed", map[string]any{"error": err.Error()})
		return
	}
	// Repo visibility (#2071): the items of a campaign on a repo the caller
	// cannot read are as sensitive as the campaign itself.
	if !s.enforceRepoVisibility(w, r, c.Repo) {
		return
	}
	items, err := s.cfg.CampaignRepo.ListCampaignItemsForCampaign(r.Context(), id)
	if err != nil {
		s.writeError(w, r, http.StatusInternalServerError, "internal_error",
			"list campaign items failed", map[string]any{"error": err.Error()})
		return
	}
	out := make([]campaignItemResponse, 0, len(items))
	for _, it := range items {
		out = append(out, toCampaignItemResponse(it))
	}
	s.writeJSON(w, r, http.StatusOK, map[string]any{"items": out})
}

// handleGetCampaignStatus implements GET /v0/campaigns/{campaign_id}/status:
// the campaign + its items + the engine's readiness rollup + the distilled
// next_action. This is the surface the operator-agent polls to drive a
// campaign.
func (s *Server) handleGetCampaignStatus(w http.ResponseWriter, r *http.Request) {
	if s.cfg.CampaignRepo == nil {
		s.writeError(w, r, http.StatusServiceUnavailable, "campaign_repo_unconfigured",
			"campaigns endpoint requires a configured campaign repository", nil)
		return
	}
	id, err := uuid.Parse(r.PathValue("campaign_id"))
	if err != nil {
		s.writeError(w, r, http.StatusBadRequest, "validation_failed",
			"campaign_id must be a valid UUID",
			map[string]any{"field": "campaign_id", "got": r.PathValue("campaign_id")})
		return
	}
	c, err := s.cfg.CampaignRepo.GetCampaign(r.Context(), id)
	if err != nil {
		if errors.Is(err, campaign.ErrNotFound) {
			s.writeError(w, r, http.StatusNotFound, "campaign_not_found",
				"no campaign with that id", map[string]any{"campaign_id": id.String()})
			return
		}
		s.writeError(w, r, http.StatusInternalServerError, "internal_error",
			"get campaign failed", map[string]any{"error": err.Error()})
		return
	}
	// Repo visibility (#2071): deny the status rollup for a campaign on a repo
	// the caller cannot read, BEFORE the reconcile-on-read pass — a denied
	// caller must not drive campaign state as a side effect of a read.
	if !s.enforceRepoVisibility(w, r, c.Repo) {
		return
	}
	items, err := s.cfg.CampaignRepo.ListCampaignItemsForCampaign(r.Context(), id)
	if err != nil {
		s.writeError(w, r, http.StatusInternalServerError, "internal_error",
			"list campaign items failed", map[string]any{"error": err.Error()})
		return
	}

	// Reconcile-on-read (E26.2 / #1481): settle any running item whose linked
	// run reached terminal and re-derive the campaign, mirroring the
	// campaigndriver ADVANCE pass. This is the local-native, driver-independent
	// advance trigger — the operator-agent already polls this surface to drive
	// the loop, so the rollup + next_action move in DAG order as each run is
	// driven to merge, with no auto-driver and no GHA. Best-effort and
	// idempotent: it NEVER fails the read (errors are logged and swallowed) and
	// a re-poll over an already-settled item performs no further transition
	// (the item/campaign transitions are state-guarded under the repo's SELECT
	// FOR UPDATE). When it settled anything, re-read so the rollup reflects the
	// fresh item + campaign state.
	if s.reconcileCampaignItemsOnRead(r.Context(), c, items) {
		if fresh, ferr := s.cfg.CampaignRepo.ListCampaignItemsForCampaign(r.Context(), id); ferr == nil {
			items = fresh
		}
		if fc, ferr := s.cfg.CampaignRepo.GetCampaign(r.Context(), id); ferr == nil {
			c = fc
		}
	}

	elig := campaign.NextEligible(items)
	out := make([]campaignItemResponse, 0, len(items))
	for _, it := range items {
		out = append(out, toCampaignItemResponse(it))
	}
	s.writeJSON(w, r, http.StatusOK, map[string]any{
		"campaign": toCampaignResponse(c),
		"items":    out,
		"rollup":   toCampaignRollupPayload(elig),
		// The POST-reconcile campaign state feeds the terminal post-filter (#2681)
		// so a campaign that just derived terminal never advertises a refused verb.
		"next_action": computeCampaignNextAction(c.State, elig),
	})
}

// handleResumeCampaign implements POST /v0/campaigns/{campaign_id}/resume: the
// operator's hand-back after the auto-driver paged a human at a run gate
// (E25.7 / ADR-047 Track C). It flips the campaign paused → running (when the
// campaign itself was paused — the pause_campaign policy) AND every paused item
// paused → running, so the next driver tick re-engages the campaign and the
// continuation proceeds.
//
// It serves BOTH pause policies: under pause_campaign the campaign and the
// affected item are paused together; under pause_item only the item is paused
// while the campaign stays running. So "nothing to resume" is the joint
// condition — the campaign is not paused AND no item is paused — which yields
// 409 campaign_not_paused; a resume with any paused item proceeds even when the
// campaign is already running.
//
// The nil-CampaignRepo guard is checked BEFORE the write-scope check so an
// unconfigured deployment answers 503 (not 401), matching the read handlers and
// the 503-vs-404 route-registration idiom.
func (s *Server) handleResumeCampaign(w http.ResponseWriter, r *http.Request) {
	if s.cfg.CampaignRepo == nil {
		s.writeError(w, r, http.StatusServiceUnavailable, "campaign_repo_unconfigured",
			"campaigns endpoint requires a configured campaign repository", nil)
		return
	}
	if !s.requireWriteScope(w, r, "write:campaigns") {
		return
	}
	id, err := uuid.Parse(r.PathValue("campaign_id"))
	if err != nil {
		s.writeError(w, r, http.StatusBadRequest, "validation_failed",
			"campaign_id must be a valid UUID",
			map[string]any{"field": "campaign_id", "got": r.PathValue("campaign_id")})
		return
	}

	c, err := s.cfg.CampaignRepo.GetCampaign(r.Context(), id)
	if err != nil {
		if errors.Is(err, campaign.ErrNotFound) {
			s.writeError(w, r, http.StatusNotFound, "campaign_not_found",
				"no campaign with that id", map[string]any{"campaign_id": id.String()})
			return
		}
		s.writeError(w, r, http.StatusInternalServerError, "internal_error",
			"get campaign failed", map[string]any{"error": err.Error()})
		return
	}

	items, err := s.cfg.CampaignRepo.ListCampaignItemsForCampaign(r.Context(), id)
	if err != nil {
		s.writeError(w, r, http.StatusInternalServerError, "internal_error",
			"list campaign items failed", map[string]any{"error": err.Error()})
		return
	}
	var pausedItems []*campaign.Item
	for _, it := range items {
		if it.State == campaign.ItemStatePaused {
			pausedItems = append(pausedItems, it)
		}
	}

	// Nothing is paused on either axis — there is nothing to resume.
	if c.State != campaign.StatePaused && len(pausedItems) == 0 {
		s.writeError(w, r, http.StatusConflict, "campaign_not_paused",
			"campaign has no paused state to resume",
			map[string]any{"campaign_id": id.String(), "state": string(c.State)})
		return
	}

	// Resume the campaign first (pause_campaign policy). A same-state running
	// campaign (pause_item policy) is left untouched — only paused items move.
	if c.State == campaign.StatePaused {
		if _, err := s.cfg.CampaignRepo.TransitionCampaign(r.Context(), id, campaign.StateRunning); err != nil {
			var inv campaign.InvalidTransitionError
			if errors.As(err, &inv) {
				s.writeError(w, r, http.StatusConflict, "invalid_transition",
					err.Error(), map[string]any{"campaign_id": id.String()})
				return
			}
			s.writeError(w, r, http.StatusInternalServerError, "internal_error",
				"resume campaign failed", map[string]any{"error": err.Error()})
			return
		}
	}

	// Resume each paused item. Best-effort retryability: a transition error
	// surfaces 500, and because a retry re-enters with the still-paused items
	// (the joint condition above), it is not stranded by the campaign already
	// being running.
	for _, it := range pausedItems {
		if _, err := s.cfg.CampaignRepo.TransitionCampaignItem(r.Context(), it.ID, campaign.ItemStateRunning); err != nil {
			var inv campaign.InvalidTransitionError
			if errors.As(err, &inv) {
				s.writeError(w, r, http.StatusConflict, "invalid_transition",
					err.Error(), map[string]any{"item_id": it.ID.String()})
				return
			}
			s.writeError(w, r, http.StatusInternalServerError, "internal_error",
				"resume campaign item failed",
				map[string]any{"error": err.Error(), "item_id": it.ID.String()})
			return
		}
	}

	// Return the freshest campaign state.
	updated, err := s.cfg.CampaignRepo.GetCampaign(r.Context(), id)
	if err != nil {
		s.writeError(w, r, http.StatusInternalServerError, "internal_error",
			"get campaign after resume failed", map[string]any{"error": err.Error()})
		return
	}
	s.writeJSON(w, r, http.StatusOK, toCampaignResponse(updated))
}

// handleCancelCampaign implements POST /v0/campaigns/{campaign_id}/cancel: the
// operator's clean shutdown of an abandoned or rebuilt campaign (#2355). It marks
// every NON-terminal item cancelled and then the campaign itself cancelled, so an
// orphaned campaign stops showing as live `running` work in GET /v0/campaigns.
//
// RECOVERY CONTRACT — IDEMPOTENT AND CONVERGENT (operator condition 2). The
// cancellation is N independent item transitions followed by the campaign
// transition, WITHOUT a single wrapping transaction. It is safe because it is
// convergent: the campaign transition runs LAST, so any mid-loop failure leaves
// the campaign in a still-cancellable (non-terminal) state, and a re-invoke
// re-lists the items, skips the ones already cancelled (now terminal, excluded
// from the non-terminal filter — and TransitionCampaignItem is idempotent on a
// same-state item anyway), cancels the remainder, then transitions the campaign.
// So a partial failure never strands the campaign half-cancelled: re-invoking the
// verb completes it. Each item transition is individually state-guarded under the
// repo's SELECT FOR UPDATE, so a concurrent driver cannot race it into an
// inconsistent state.
//
// Cancelling a campaign deliberately does NOT cancel the linked RUNS — a run in
// flight keeps running; fishhawk_cancel_run owns run cancellation. The refusal,
// the tool description, and the API docs all say so.
//
// The nil-CampaignRepo guard is checked BEFORE the write-scope check so an
// unconfigured deployment answers 503 (not 401), matching the other campaign
// write handlers and the 503-vs-404 route-registration idiom.
func (s *Server) handleCancelCampaign(w http.ResponseWriter, r *http.Request) {
	if s.cfg.CampaignRepo == nil {
		s.writeError(w, r, http.StatusServiceUnavailable, "campaign_repo_unconfigured",
			"campaigns endpoint requires a configured campaign repository", nil)
		return
	}
	if !s.requireWriteScope(w, r, "write:campaigns") {
		return
	}
	id, err := uuid.Parse(r.PathValue("campaign_id"))
	if err != nil {
		s.writeError(w, r, http.StatusBadRequest, "validation_failed",
			"campaign_id must be a valid UUID",
			map[string]any{"field": "campaign_id", "got": r.PathValue("campaign_id")})
		return
	}

	c, err := s.cfg.CampaignRepo.GetCampaign(r.Context(), id)
	if err != nil {
		if errors.Is(err, campaign.ErrNotFound) {
			s.writeError(w, r, http.StatusNotFound, "campaign_not_found",
				"no campaign with that id", map[string]any{"campaign_id": id.String()})
			return
		}
		s.writeError(w, r, http.StatusInternalServerError, "internal_error",
			"get campaign failed", map[string]any{"error": err.Error()})
		return
	}

	// Terminal-state guard: a succeeded/failed/cancelled campaign admits no
	// further transition (campaign.State.IsTerminal), so cancel is refused 409
	// campaign_not_cancellable and the campaign row is left untouched.
	if c.State.IsTerminal() {
		s.writeError(w, r, http.StatusConflict, "campaign_not_cancellable",
			"campaign is already in a terminal state and cannot be cancelled",
			map[string]any{"campaign_id": id.String(), "state": string(c.State)})
		return
	}

	items, err := s.cfg.CampaignRepo.ListCampaignItemsForCampaign(r.Context(), id)
	if err != nil {
		s.writeError(w, r, http.StatusInternalServerError, "internal_error",
			"list campaign items failed", map[string]any{"error": err.Error()})
		return
	}

	// Capture the pre-transition campaign state for the audit BEFORE any write:
	// TransitionCampaign may mutate the shared campaign pointer (the in-memory fake
	// aliases it), so reading c.State after the write would report "cancelled" for
	// the audit "from" — same aliasing guard deriveCampaignAfterChange applies.
	fromState := c.State

	// Cancel every NON-terminal item (pending/blocked/running/paused -> cancelled
	// are all valid edges in campaignItemTransitions; a terminal item is left as
	// it stands). A transition failure surfaces an error and leaves the campaign
	// non-terminal, so a re-invoke re-lists and completes the cancellation.
	itemsCancelled := 0
	for _, it := range items {
		if it.State.IsTerminal() {
			continue
		}
		if _, err := s.cfg.CampaignRepo.TransitionCampaignItem(r.Context(), it.ID, campaign.ItemStateCancelled); err != nil {
			var inv campaign.InvalidTransitionError
			if errors.As(err, &inv) {
				s.writeError(w, r, http.StatusConflict, "invalid_transition",
					err.Error(), map[string]any{"item_id": it.ID.String()})
				return
			}
			s.writeError(w, r, http.StatusInternalServerError, "internal_error",
				"cancel campaign item failed",
				map[string]any{"error": err.Error(), "item_id": it.ID.String()})
			return
		}
		itemsCancelled++
	}

	// Cancel the campaign itself (pending/running/paused -> cancelled are all
	// valid). Runs last so the whole verb is convergent under a partial failure.
	updated, err := s.cfg.CampaignRepo.TransitionCampaign(r.Context(), id, campaign.StateCancelled)
	if err != nil {
		var inv campaign.InvalidTransitionError
		if errors.As(err, &inv) {
			s.writeError(w, r, http.StatusConflict, "invalid_transition",
				err.Error(), map[string]any{"campaign_id": id.String()})
			return
		}
		s.writeError(w, r, http.StatusInternalServerError, "internal_error",
			"cancel campaign failed", map[string]any{"error": err.Error()})
		return
	}

	s.emitCampaignAudit(r.Context(), categoryCampaignCancelled, map[string]any{
		"campaign_id":     id.String(),
		"from":            string(fromState),
		"items_cancelled": itemsCancelled,
	})

	s.writeJSON(w, r, http.StatusOK, toCampaignResponse(updated))
}

// Campaign audit categories the operator-driven start + reconcile-on-read path
// emits on the GLOBAL audit chain (E26.2 / #1481). They are the SAME free-form
// category strings the campaigndriver emits (campaigndriver/driver.go) — a
// campaign-linked run started/settled/advanced is the same surface whether the
// auto-driver or the operator-agent drove it — documented in
// docs/issue-comment-surfaces.md. They ride the global chain
// (AppendGlobalChained) because a campaign is not a run; the run linkage travels
// in the payload's run_id field.
const (
	categoryCampaignIssueStarted   = "campaign_issue_started"
	categoryCampaignIssueSettled   = "campaign_issue_settled"
	categoryCampaignAdvanced       = "campaign_advanced"
	categoryCampaignIssueRestarted = "campaign_issue_restarted"
	// categoryCampaignItemAutonomyRefreshed marks a reconcile-on-read autonomy
	// refresh: an item's stored tier was overwritten from the issue's live
	// autonomy:* label so a relabel unblocks a human-led item (#2355).
	categoryCampaignItemAutonomyRefreshed = "campaign_item_autonomy_refreshed"
	// categoryCampaignCancelled marks the operator cancel verb marking a
	// campaign and its unfinished items cancelled (#2355). It does NOT cancel
	// the linked runs (fishhawk_cancel_run owns that).
	categoryCampaignCancelled = "campaign_cancelled"
)

// startCampaignItemRunRequest is the POST /v0/campaigns/{campaign_id}/runs body.
// issue_ref + workflow_id are required; workflow_ref (empty = the repo's default
// branch) and runner_kind (empty = github_actions; pass "local" for the local
// dogfood loop) are optional. There is deliberately NO idempotency_key field:
// the server does not dedup this create-link-transition sequence, so advertising
// one would be a field the server ignores (#1443 honesty). A caller that needs
// idempotency gates on the DAG eligibility instead — a re-POST against an
// already-running item is refused item_not_eligible.
type startCampaignItemRunRequest struct {
	IssueRef    string `json:"issue_ref"`
	WorkflowID  string `json:"workflow_id"`
	WorkflowRef string `json:"workflow_ref,omitempty"`
	RunnerKind  string `json:"runner_kind,omitempty"`
	// WorkingDir binds the minted run's local checkout (E48.69 / #2498) so the
	// later runner-spawning verbs inherit it. As of E48.87 / #2527 it is an
	// OVERRIDE of the campaign's own working_dir binding and is needed only when
	// the campaign carries none: empty falls through to the campaign binding,
	// and a local item with neither is refused working_dir_required. A non-empty
	// value that DIFFERS from the campaign binding is accepted as a deliberate
	// override (a campaign is a batch; an item in another checkout is
	// conceivable) and logged. When non-empty it must be absolute — the handler
	// 400s a relative value the same way POST /v0/runs does. The
	// DisallowUnknownFields decoder makes declaring the field mandatory for it
	// to be accepted at all.
	WorkingDir string `json:"working_dir,omitempty"`
}

// handleStartCampaignItemRun implements POST /v0/campaigns/{campaign_id}/runs:
// the operator-driven, campaign-aware run start (E26.2 / #1481). For an
// issue_ref in the campaign it refuses unless the item is eligible per
// campaign.NextEligible (naming the blocking dependency on refusal). A
// deps-satisfied autonomy:low (human-led) item is refused at the primary DAG
// gate with a DISTINCT item_human_led code whose detail says a human must lead
// it — never "start the ref" (#1697); every other refusal keeps the generic
// item_not_eligible detail. On an eligible/restartable item it mints the
// run via Server.StartRunForCampaignIssue (carrying runner_kind so the local
// loop gets runner_kind:local, and working_dir so the minted run binds the
// operator's checkout that every later runner-spawning verb inherits — E48.69 /
// #2498), links it with SetCampaignItemRun, transitions the item pending →
// running, and derives/advances the campaign so a pending campaign moves to
// running on its first dispatch. A non-empty non-absolute working_dir 400s
// validation_failed, mirroring POST /v0/runs.
//
// The minted run's checkout is resolved through the E48.87 / #2527 ladder —
// explicit per-item value (accepted even when it conflicts, as a deliberate
// override) > the campaign's own working_dir binding (re-validated absolute) >
// a working_dir_required 400 for a local item with neither. The ladder runs
// BEFORE any mutation (item restart, run mint, link) so a refusal leaves no
// committed state.
//
// The nil-CampaignRepo guard is checked BEFORE the write-scope check so an
// unconfigured deployment answers 503 (not 401), matching the other campaign
// write handlers and the 503-vs-404 route-registration idiom.
func (s *Server) handleStartCampaignItemRun(w http.ResponseWriter, r *http.Request) {
	if s.cfg.CampaignRepo == nil {
		s.writeError(w, r, http.StatusServiceUnavailable, "campaign_repo_unconfigured",
			"campaigns endpoint requires a configured campaign repository", nil)
		return
	}
	if !s.requireWriteScope(w, r, "write:campaigns") {
		return
	}
	id, err := uuid.Parse(r.PathValue("campaign_id"))
	if err != nil {
		s.writeError(w, r, http.StatusBadRequest, "validation_failed",
			"campaign_id must be a valid UUID",
			map[string]any{"field": "campaign_id", "got": r.PathValue("campaign_id")})
		return
	}

	var req startCampaignItemRunRequest
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		s.writeError(w, r, http.StatusBadRequest, "validation_failed",
			"request body is not valid JSON or contains unknown fields",
			map[string]any{"error": err.Error()})
		return
	}
	if strings.TrimSpace(req.IssueRef) == "" {
		s.writeError(w, r, http.StatusBadRequest, "validation_failed",
			"issue_ref is required", map[string]any{"field": "issue_ref"})
		return
	}
	if strings.TrimSpace(req.WorkflowID) == "" {
		s.writeError(w, r, http.StatusBadRequest, "validation_failed",
			"workflow_id is required", map[string]any{"field": "workflow_id"})
		return
	}
	if req.RunnerKind != "" {
		if _, ok := run.ValidRunnerKinds[req.RunnerKind]; !ok {
			s.writeError(w, r, http.StatusBadRequest, "validation_failed",
				"runner_kind must be one of github_actions, local",
				map[string]any{"field": "runner_kind", "got": req.RunnerKind})
			return
		}
	}
	// working_dir, when supplied, must be absolute (E48.69 / #2498) — the same
	// guard handleCreateRun applies at POST /v0/runs, so both run-minting
	// endpoints refuse a relative binding the same way. A relative value would
	// resolve against the daemon host's cwd and poison every later inheriting
	// verb. This is the transport-agnostic REST guard; the actionable
	// agent-facing 'local requires working_dir' refusal lives in the MCP layer.
	if req.WorkingDir != "" && !filepath.IsAbs(req.WorkingDir) {
		s.writeError(w, r, http.StatusBadRequest, "validation_failed",
			"working_dir must be an absolute path",
			map[string]any{"field": "working_dir", "got": req.WorkingDir})
		return
	}

	c, err := s.cfg.CampaignRepo.GetCampaign(r.Context(), id)
	if err != nil {
		if errors.Is(err, campaign.ErrNotFound) {
			s.writeError(w, r, http.StatusNotFound, "campaign_not_found",
				"no campaign with that id", map[string]any{"campaign_id": id.String()})
			return
		}
		s.writeError(w, r, http.StatusInternalServerError, "internal_error",
			"get campaign failed", map[string]any{"error": err.Error()})
		return
	}
	// A pending or running campaign can dispatch a new item run — and so, as of
	// #2681, can a terminal-FAILED one: a campaign whose LAST unsettled item
	// failed derives terminal (DeriveState: anyFailed && allTerminal) while that
	// item is still deps-satisfied and restartable, and refusing here made it
	// unrecoverable inside the campaign. The reopen happens strictly AFTER the
	// DAG gate below, so a failed campaign whose named item is NOT restartable is
	// still refused with the campaign left failed and nothing written.
	//
	// The remaining refusals are paused and cancelled/succeeded, and the detail
	// distinguishes them: a paused campaign has a forward verb (resume); a
	// cancelled or succeeded one is genuinely closed.
	if c.State != campaign.StatePending && c.State != campaign.StateRunning && c.State != campaign.StateFailed {
		detail := "campaign is cancelled or succeeded — it is closed and can start no further item runs; drive the issue standalone with fishhawk_start_run (the campaign will not track it)"
		if c.State == campaign.StatePaused {
			detail = "campaign is paused — resume it (fishhawk_resume_campaign) before starting an item run"
		}
		s.writeError(w, r, http.StatusConflict, "campaign_not_startable",
			detail,
			map[string]any{"campaign_id": id.String(), "state": string(c.State)})
		return
	}

	// Resolve the minted run's checkout through the campaign ladder (E48.87 /
	// #2527). The campaign binds the checkout ONCE at create; this is where an
	// item run inherits it, so the operator stops re-typing an identical
	// absolute path per item. Three rungs, in order:
	//
	//  1. An explicit per-item working_dir WINS — including when it DIFFERS from
	//     the campaign binding, which is ACCEPTED as a deliberate override
	//     rather than refused. Unlike resolveWorkingDirForRun's run binding
	//     (which anchors an in-flight run whose branch lineage already lives in
	//     one tree, so a conflict there is incoherent), a campaign is a BATCH:
	//     an item legitimately executing in a different checkout is conceivable,
	//     and refusing would make the parameter #2498 shipped useless as the
	//     override the issue calls it. The divergence is logged so the applied
	//     resolution is observable, and the minted run row carries the value
	//     that actually applied.
	//  2. Otherwise INHERIT the campaign's binding — re-validated through the
	//     same absolute-path gate an explicit value passes, so a relative value
	//     smuggled onto a campaign row (a direct DB write, or a row predating
	//     the create-time guard) cannot bypass validation by arriving through a
	//     different door.
	//  3. Otherwise, a LOCAL item with no resolvable checkout is refused
	//     working_dir_required. This is #2498's rung-3 refusal relocated from
	//     the MCP tool to here, because the MCP layer cannot see the campaign's
	//     binding without a read; it stays transport-independent (it fires for
	//     stdio and HTTP MCP clients alike) and now also covers direct REST
	//     callers, which the MCP-only guard never did.
	workingDir := req.WorkingDir
	switch {
	case workingDir != "":
		if c.WorkingDir != "" && filepath.Clean(workingDir) != filepath.Clean(c.WorkingDir) {
			s.cfg.Logger.Info("campaign item run overrides the campaign working_dir binding",
				"campaign_id", id.String(),
				"issue_ref", req.IssueRef,
				"campaign_working_dir", c.WorkingDir,
				"override_working_dir", workingDir)
		}
	case c.WorkingDir != "":
		if !filepath.IsAbs(c.WorkingDir) {
			s.writeError(w, r, http.StatusBadRequest, "validation_failed",
				"the campaign's bound working_dir is not an absolute path, so it cannot be inherited; pass an absolute working_dir for this item",
				map[string]any{"field": "working_dir", "got": c.WorkingDir, "campaign_id": id.String()})
			return
		}
		workingDir = c.WorkingDir
	case req.RunnerKind == string(run.RunnerKindLocal):
		s.writeError(w, r, http.StatusBadRequest, "working_dir_required",
			"a local item run needs a resolvable checkout: bind working_dir once at fishhawk_start_campaign so every item run inherits it, or pass an absolute working_dir for this item",
			map[string]any{"field": "working_dir", "campaign_id": id.String(), "issue_ref": req.IssueRef})
		return
	}

	items, err := s.cfg.CampaignRepo.ListCampaignItemsForCampaign(r.Context(), id)
	if err != nil {
		s.writeError(w, r, http.StatusInternalServerError, "internal_error",
			"list campaign items failed", map[string]any{"error": err.Error()})
		return
	}
	var item *campaign.Item
	for _, it := range items {
		if it.IssueRef == req.IssueRef {
			item = it
			break
		}
	}
	if item == nil {
		s.writeError(w, r, http.StatusNotFound, "campaign_item_not_found",
			"no campaign item with that issue_ref",
			map[string]any{"campaign_id": id.String(), "issue_ref": req.IssueRef})
		return
	}

	// Targeted autonomy refresh (#2355): re-read the ONE named item's autonomy:*
	// label BEFORE the DAG gate so a relabel-then-start succeeds without an
	// intervening status poll — an operator who relabels autonomy:low ->
	// autonomy:medium and immediately starts the item is not refused item_human_led
	// on a stale tier. Best-effort and fail-open on every guard; when it DID change
	// the tier, re-list so NextEligible below partitions on the fresh value. It runs
	// AFTER the campaign-state gate above, so it never widens the campaign-state
	// refusal surface (a paused/terminal campaign still refuses with no forge call).
	if refreshed, changed := s.refreshOneItemAutonomy(r.Context(), c, item); changed {
		item = refreshed
		if relisted, lerr := s.cfg.CampaignRepo.ListCampaignItemsForCampaign(r.Context(), id); lerr == nil {
			items = relisted
			for _, it := range items {
				if it.IssueRef == req.IssueRef {
					item = it
					break
				}
			}
		} else {
			// Relist FAILED after the tier changed (#2355 fixup): the initial `items`
			// slice still carries the PRE-refresh tier for this item (against the real
			// repo its rows are value snapshots, so the SetCampaignItemAutonomy write
			// does not reach them). Patch the refreshed item into the stale slice IN
			// PLACE so the NextEligible gate below partitions on the FRESH tier. Without
			// this the gate reads the pre-refresh autonomy:low and refuses a
			// just-promoted item item_human_led even though its persisted tier is now
			// medium — the one branch where the best-effort refresh's fail-open works
			// against the operator.
			for i, it := range items {
				if it.IssueRef == req.IssueRef {
					items[i] = refreshed
					break
				}
			}
		}
	}

	// DAG gate: refuse unless the item is eligible OR restartable per the pure
	// engine. On refusal, name the precise blocker — the first unmet dependency
	// for a blocked item, otherwise the item's current state — so the
	// operator-agent knows what to wait on. Restartable admits a deps-satisfied,
	// non-human-led CANCELLED (#1729) or FAILED (#1838) item: it has no eligible
	// forward path (cancelled/failed is terminal for auto-dispatch) but the
	// operator verb resets it. RestartCampaignItem accepts both from-states
	// (postgres.go).
	elig := campaign.NextEligible(items)
	isRestartable := containsRef(elig.Restartable, req.IssueRef)
	if !containsRef(elig.Eligible, req.IssueRef) && !isRestartable {
		// A deps-satisfied autonomy:low item is human-led (#1551 / E32.4): it is
		// refused with a DISTINCT item_human_led code whose detail names the
		// human-led reason and does NOT tell the caller to start a ref (its
		// next_action is attend_human_led, which names no startable ref — #1697).
		// Every other refusal keeps the generic item_not_eligible detail.
		if containsRef(elig.HumanLed, req.IssueRef) {
			s.writeError(w, r, http.StatusConflict, "item_human_led",
				humanLedDetail(), map[string]any{
					"campaign_id": id.String(),
					"issue_ref":   req.IssueRef,
					"item_state":  string(item.State),
				})
			return
		}
		s.writeError(w, r, http.StatusConflict, "item_not_eligible",
			ineligibilityDetail(item, items), map[string]any{
				"campaign_id": id.String(),
				"issue_ref":   req.IssueRef,
				"item_state":  string(item.State),
			})
		return
	}

	// Defensive: a terminal-FAILED campaign is admitted ONLY to reopen a
	// restartable item. Under such a campaign every item is terminal by
	// construction (DeriveState's allTerminal conjunct), so Eligible is empty and
	// reaching here with !isRestartable is unreachable — but if it ever were
	// reachable, minting a run inside a still-failed campaign is the wrong
	// outcome, so refuse rather than fall through.
	if c.State == campaign.StateFailed && !isRestartable {
		s.writeError(w, r, http.StatusConflict, "campaign_not_startable",
			"campaign is failed and this item is not restartable, so there is nothing to reopen; drive the issue standalone with fishhawk_start_run (the campaign will not track it)",
			map[string]any{"campaign_id": id.String(), "state": string(c.State), "issue_ref": req.IssueRef})
		return
	}

	// Restart path (#1729 cancelled / #1838 failed): reset the restartable item
	// back to pending, clearing its stale run link, BEFORE the mint/link/transition flow — which
	// then runs unchanged over the now-pending item. The reset is atomic under
	// the repo's FOR UPDATE lock; capture the prior run/state for the restart
	// audit before RestartCampaignItem clears them.
	//
	// Under a TERMINAL-FAILED campaign (#2681) the item reset alone is not
	// enough — the campaign must be reopened too, or the very next status read
	// re-derives it terminal. Both writes go through
	// ReopenCampaignForItemRestart, ONE transaction holding row locks on the
	// campaign and the item, so no concurrent reconcile-on-read can observe a
	// running campaign whose every item is still terminal.
	if isRestartable {
		priorState := string(item.State)
		priorRunID := ""
		if item.RunID != nil {
			priorRunID = item.RunID.String()
		}
		reopened := c.State == campaign.StateFailed
		var (
			resetItem     *campaign.Item
			reopenedCmp   *campaign.Campaign
			rerr          error
			restartFailed = "restart campaign item failed"
		)
		if reopened {
			restartFailed = "reopen campaign for item restart failed"
			reopenedCmp, resetItem, rerr = s.cfg.CampaignRepo.ReopenCampaignForItemRestart(r.Context(), id, item.ID)
		} else {
			resetItem, rerr = s.cfg.CampaignRepo.RestartCampaignItem(r.Context(), item.ID)
		}
		if rerr != nil {
			var inv campaign.InvalidTransitionError
			switch {
			case errors.As(rerr, &inv) && inv.Kind == "campaign":
				// Reopen only: the campaign left `failed` underneath us (a concurrent
				// reconcile re-derived it, or an operator cancelled it), so the
				// reopen no longer applies. NOTHING was written — the primitive's
				// single transaction rolled back.
				s.writeError(w, r, http.StatusConflict, "campaign_not_startable",
					"campaign is no longer in the terminal-failed state this restart would reopen; re-poll fishhawk_get_campaign_status and follow its next_action",
					map[string]any{"campaign_id": id.String(), "state": string(c.State), "issue_ref": req.IssueRef})
			case errors.As(rerr, &inv):
				// A concurrent restart/dispatch already moved the item off its
				// terminal state — it is no longer restartable.
				s.writeError(w, r, http.StatusConflict, "item_not_eligible",
					ineligibilityDetail(item, items), map[string]any{
						"campaign_id": id.String(),
						"issue_ref":   req.IssueRef,
						"item_state":  string(item.State),
					})
			case errors.Is(rerr, campaign.ErrCampaignNotFound):
				// The CAMPAIGN row vanished (concurrently deleted) — reported under
				// the campaign-shaped code, never the item-shaped one, so the
				// operator is not sent hunting for a missing item (#2681 condition 3).
				s.writeError(w, r, http.StatusNotFound, "campaign_not_found",
					"no campaign with that id", map[string]any{"campaign_id": id.String()})
			case errors.Is(rerr, campaign.ErrNotFound):
				// The ITEM is missing — either genuinely gone or (from the reopen
				// primitive) owned by another campaign, which from this campaign's
				// perspective is the same thing. RestartCampaignItem's plain
				// ErrNotFound lands here unchanged.
				s.writeError(w, r, http.StatusNotFound, "campaign_item_not_found",
					"no campaign item with that issue_ref",
					map[string]any{"campaign_id": id.String(), "issue_ref": req.IssueRef})
			default:
				s.writeError(w, r, http.StatusInternalServerError, "internal_error",
					restartFailed,
					map[string]any{"error": rerr.Error(), "item_id": item.ID.String()})
			}
			return
		}
		item = resetItem
		if reopened {
			// Adopt the reopened campaign so the pending -> running derivation below
			// sees the fresh `running` state and does not re-derive it.
			c = reopenedCmp
			s.emitCampaignAudit(r.Context(), categoryCampaignAdvanced, map[string]any{
				"campaign_id": id.String(),
				"from":        string(campaign.StateFailed),
				"to":          string(campaign.StateRunning),
			})
		}
		s.emitCampaignAudit(r.Context(), categoryCampaignIssueRestarted, map[string]any{
			"campaign_id":  id.String(),
			"issue_ref":    item.IssueRef,
			"prior_run_id": priorRunID,
			"prior_state":  priorState,
		})
		// Re-list so the flow below sees the item as pending (run link cleared).
		refreshed, lerr := s.cfg.CampaignRepo.ListCampaignItemsForCampaign(r.Context(), id)
		if lerr != nil {
			s.writeError(w, r, http.StatusInternalServerError, "internal_error",
				"list campaign items failed", map[string]any{"error": lerr.Error()})
			return
		}
		items = refreshed
	}

	// Defensive re-check: NextEligible already excludes linked/terminal/running
	// items, but confirm the running transition is reachable before minting a
	// run we could not link.
	if !campaign.ValidCampaignItemTransition(item.State, campaign.ItemStateRunning) {
		s.writeError(w, r, http.StatusConflict, "item_not_eligible",
			ineligibilityDetail(item, items), map[string]any{
				"campaign_id": id.String(),
				"issue_ref":   req.IssueRef,
				"item_state":  string(item.State),
			})
		return
	}

	// Mint the run via the reused seam, carrying runner_kind so the local loop
	// gets runner_kind:local. A resolution failure (installation/spec) surfaces
	// 502 — the same upstream-dependency class as the create handler's
	// epic_children_query_failed — leaving the item un-started for a retry.
	runRow, err := s.StartRunForCampaignIssue(r.Context(), StartRunForCampaignIssueParams{
		Repo:        c.Repo,
		IssueRef:    req.IssueRef,
		WorkflowID:  req.WorkflowID,
		WorkflowRef: req.WorkflowRef,
		RunnerKind:  req.RunnerKind,
		// The LADDER-RESOLVED checkout (E48.87 / #2527), not req.WorkingDir: an
		// item run that passed none inherits the campaign's binding here, and the
		// minted run row therefore records the resolution that actually applied.
		WorkingDir: workingDir,
	})
	if err != nil {
		s.writeError(w, r, http.StatusBadGateway, "campaign_run_start_failed",
			"could not start a run for the campaign item",
			map[string]any{"error": err.Error(), "issue_ref": req.IssueRef})
		return
	}

	// Link the item to its run, then transition it to running. On a transition
	// failure after the link committed, roll the link back so the item
	// re-partitions as Eligible and the operator can retry — mirroring the
	// driver's start() rollback (a linked-but-not-running item is classified
	// Running by NextEligible and would otherwise be stranded).
	if _, err := s.cfg.CampaignRepo.SetCampaignItemRun(r.Context(), item.ID, &runRow.ID); err != nil {
		s.writeError(w, r, http.StatusInternalServerError, "internal_error",
			"link campaign item to run failed",
			map[string]any{"error": err.Error(), "item_id": item.ID.String(), "run_id": runRow.ID.String()})
		return
	}
	updatedItem, err := s.cfg.CampaignRepo.TransitionCampaignItem(r.Context(), item.ID, campaign.ItemStateRunning)
	if err != nil {
		if _, uerr := s.cfg.CampaignRepo.SetCampaignItemRun(r.Context(), item.ID, nil); uerr != nil {
			s.cfg.Logger.Warn("campaign item-run unlink after failed running transition also failed; item left linked-but-not-running",
				"campaign_id", id.String(), "item_id", item.ID.String(), "run_id", runRow.ID.String(), "error", uerr.Error())
		}
		var inv campaign.InvalidTransitionError
		if errors.As(err, &inv) {
			s.writeError(w, r, http.StatusConflict, "invalid_transition",
				err.Error(), map[string]any{"item_id": item.ID.String()})
			return
		}
		s.writeError(w, r, http.StatusInternalServerError, "internal_error",
			"transition campaign item to running failed",
			map[string]any{"error": err.Error(), "item_id": item.ID.String()})
		return
	}

	// Emit the campaign_issue_started marker (best-effort, global chain).
	s.emitCampaignAudit(r.Context(), categoryCampaignIssueStarted, map[string]any{
		"campaign_id": id.String(),
		"issue_ref":   item.IssueRef,
		"run_id":      runRow.ID.String(),
	})

	// Derive the campaign forward: a pending campaign becomes running on its
	// first dispatch. Best-effort — a derivation/transition failure does not
	// unwind the started run (the run + link + item transition already
	// committed); the next status-read reconcile re-derives.
	if c.State == campaign.StatePending {
		refreshed, lerr := s.cfg.CampaignRepo.ListCampaignItemsForCampaign(r.Context(), id)
		if lerr != nil {
			s.cfg.Logger.Warn("re-list items for campaign derivation failed; campaign left pending (status-read reconcile re-derives)",
				"campaign_id", id.String(), "error", lerr.Error())
		} else {
			s.deriveCampaignAfterChange(r.Context(), c, refreshed)
		}
	}

	s.writeJSON(w, r, http.StatusCreated, map[string]any{
		"run":  toRunResponse(runRow),
		"item": toCampaignItemResponse(updatedItem),
	})
}

// containsRef reports whether ref is in refs.
func containsRef(refs []string, ref string) bool {
	for _, r := range refs {
		if r == ref {
			return true
		}
	}
	return false
}

// ineligibilityDetail returns a precise, operator-actionable reason an item is
// not eligible to start. A blocked item (pending/blocked with an unmet
// dependency) names the first depends_on ref not yet succeeded; any other state
// reports the item's current state.
func ineligibilityDetail(item *campaign.Item, items []*campaign.Item) string {
	switch item.State {
	case campaign.ItemStatePending, campaign.ItemStateBlocked:
		if dep, ok := firstUnmetDependency(item, items); ok {
			return "blocked on dependency " + dep + "; it must succeed before this item can start"
		}
		// No unmet dependency but still not eligible — almost always an
		// already-linked item (RunID set). Report the state.
		return "item is not eligible to start in state " + string(item.State)
	default:
		return "item is already in state " + string(item.State) + "; only an eligible (unstarted, dependencies-satisfied) item can be started"
	}
}

// humanLedDetail returns the refusal detail for a deps-satisfied autonomy:low
// (human-led) item. It single-sources the attend_human_led wording the campaign
// status surface already uses (computeCampaignNextAction, #1681) and, unlike
// ineligibilityDetail, does NOT tell the caller to start a ref — a human-led
// item's next_action names no startable ref (#1697).
func humanLedDetail() string {
	return "item is deps-satisfied but autonomy:low (human-led); a human must lead it — do not start an agent run (next_action: attend_human_led). The tier shown is re-read from the issue's autonomy:* label on each status poll and on this start verb (#2355); to drive it agent-led, relabel the issue (e.g. autonomy:low -> autonomy:medium) and re-poll fishhawk_get_campaign_status — if it still refuses, the label did not land"
}

// firstUnmetDependency returns the item's first depends_on ref whose target
// item has not succeeded (or is absent from the campaign), and ok=false when
// every dependency is satisfied.
func firstUnmetDependency(item *campaign.Item, items []*campaign.Item) (string, bool) {
	done := make(map[string]bool, len(items))
	for _, it := range items {
		if it.State == campaign.ItemStateSucceeded {
			done[it.IssueRef] = true
		}
	}
	for _, dep := range item.DependsOn {
		if !done[dep] {
			return dep, true
		}
	}
	return "", false
}

// reconcileCampaignItemsOnRead settles campaign items out of band and re-derives
// the campaign (E26.2 / #1481), mirroring the campaigndriver ADVANCE pass. It
// returns whether it settled anything so the caller can re-read the fresh item +
// campaign state. Two passes run:
//
//  1. RUN-LINKED terminal settle: a running item whose linked run reached a
//     terminal run state settles to the mapped item-terminal state. No-ops when
//     no run repository is wired (the local-native trigger needs run state).
//  2. ISSUE-CLOSED settle (#1558, extended #2029): a deps-satisfied item whose
//     GitHub issue is closed-as-completed settles succeeded, in two classes — a
//     RUN-LESS pending/blocked item (a human-led item that merged and closed
//     OUTSIDE the run lifecycle, which pass 1 can never see) AND a run-LINKED
//     item whose linked run went terminal-non-succeeded (cancelled/failed) but
//     was delivered out-of-band (the re-shaped-then-delivered case). Only a
//     closed-as-completed issue settles either class; an open or not_planned
//     closure never does. See settleIssueClosedItems.
//
// It is BEST-EFFORT and NEVER fails the read: every error is logged and
// swallowed. It is IDEMPOTENT: the item/campaign transitions go through the
// campaign repo's state-guarded SELECT FOR UPDATE transitions, so a concurrent
// re-poll cannot double-transition — a second pass over an already-settled item
// is a no-op (its state is no longer running/pending) and emits nothing.
func (s *Server) reconcileCampaignItemsOnRead(ctx context.Context, c *campaign.Campaign, items []*campaign.Item) bool {
	settledAny := false
	// Pass 1: run-linked terminal settle. Needs a run repository to read run
	// state; skipped when unwired (the run-less pass 2 below is independent).
	if s.cfg.RunRepo != nil {
		for _, it := range items {
			if it.State != campaign.ItemStateRunning || it.RunID == nil {
				continue
			}
			runRow, err := s.cfg.RunRepo.GetRun(ctx, *it.RunID)
			if err != nil {
				s.cfg.Logger.Warn("reconcile-on-read: get linked run failed; item left running",
					"campaign_id", c.ID.String(), "item_id", it.ID.String(), "run_id", it.RunID.String(), "error", err.Error())
				continue
			}
			if !runRow.State.IsTerminal() {
				continue
			}
			target, ok := mapRunTerminalToItemState(runRow.State)
			if !ok {
				s.cfg.Logger.Warn("reconcile-on-read: unmapped terminal run state; item left running",
					"campaign_id", c.ID.String(), "item_id", it.ID.String(), "run_state", string(runRow.State))
				continue
			}
			// Recovery-lineage walk (#1751): a linked run that failed category-B
			// may have been recovered via fishhawk_resume_run, which mints a
			// recovery child carrying parent_run_id = the failed run. Settling
			// straight off the failed run ignores that recovery and flips the
			// campaign failed after the recovered work merged. So when the linked
			// run is terminal-failed, walk its recovery descendants and settle off
			// the newest terminal one instead. A still-in-flight recovery
			// descendant leaves the item running (re-settled on a later read); no
			// descendants preserves today's failed settle.
			relinkID := it.RunID
			if runRow.State == run.StateFailed {
				desc, inFlight := s.newestTerminalRecoveryDescendant(ctx, *it.RunID)
				if inFlight {
					continue
				}
				if desc != nil {
					dtarget, dok := mapRunTerminalToItemState(desc.State)
					if dok {
						target = dtarget
						id := desc.ID
						relinkID = &id
					}
				}
			}
			if !campaign.ValidCampaignItemTransition(it.State, target) {
				continue
			}
			if _, err := s.cfg.CampaignRepo.TransitionCampaignItem(ctx, it.ID, target); err != nil {
				s.cfg.Logger.Warn("reconcile-on-read: settle item transition failed; left for next read",
					"campaign_id", c.ID.String(), "item_id", it.ID.String(), "target", string(target), "error", err.Error())
				continue
			}
			// Re-link the item to the recovery descendant it settled off, so the
			// item's run_id reflects the run that actually produced the outcome
			// (provenance). Best-effort: a link failure logs and does not unwind
			// the settled transition.
			if relinkID != it.RunID {
				if _, err := s.cfg.CampaignRepo.SetCampaignItemRun(ctx, it.ID, relinkID); err != nil {
					s.cfg.Logger.Warn("reconcile-on-read: relink item to recovery descendant failed; item settled but link stale",
						"campaign_id", c.ID.String(), "item_id", it.ID.String(), "run_id", relinkID.String(), "error", err.Error())
				}
			}
			settledAny = true
			s.emitCampaignAudit(ctx, categoryCampaignIssueSettled, map[string]any{
				"campaign_id": c.ID.String(),
				"issue_ref":   it.IssueRef,
				"run_id":      relinkID.String(),
				"outcome":     string(target),
			})
		}
	}
	// Pass 2: run-less issue-closed settle (#1558). Independent of the run
	// repository — it reads GitHub issue state — so it runs even when RunRepo is
	// unwired. Uses the item states as they stand after pass 1.
	if s.settleIssueClosedItems(ctx, c, items) {
		settledAny = true
	}
	if !settledAny {
		return false
	}
	// Re-derive the campaign over the freshly-settled items, unless it is
	// paused (sticky — a paused campaign re-engages only on resume, never on a
	// derivation; mirrors the driver's deriveAndTransition guard).
	if c.State != campaign.StatePaused {
		refreshed, err := s.cfg.CampaignRepo.ListCampaignItemsForCampaign(ctx, c.ID)
		if err != nil {
			s.cfg.Logger.Warn("reconcile-on-read: re-list items after settle failed; campaign not re-derived",
				"campaign_id", c.ID.String(), "error", err.Error())
		} else {
			s.deriveCampaignAfterChange(ctx, c, refreshed)
		}
	}
	return true
}

// settleIssueClosedItems is reconcile-on-read pass 2 (#1558, extended #2029):
// the issue-closed settle for a campaign item delivered OUTSIDE the run
// lifecycle. It reads the item's GitHub issue and settles the item succeeded
// ONLY when the issue is CLOSED as completed (state_reason=completed), for two
// deps-satisfied candidate classes:
//
//	A (run-less): a run-less pending/blocked item — a human-led (autonomy:low)
//	  issue merged and closed by a maintainer PR OUTSIDE any run. Settled via the
//	  guarded TransitionCampaignItem(->succeeded); its campaign_issue_settled
//	  marker carries settled_via=issue_closed and NO run_id.
//	B (out-of-band terminal, #2029): a run-LINKED item whose linked run went
//	  terminal-non-succeeded (cancelled/failed) — a re-shaped-then-delivered item
//	  whose dead run left it terminal but whose issue is now closed-as-completed.
//	  A terminal item cannot go succeeded through the transition table (it refuses
//	  every terminal from), so it is settled via the guard-bypassing
//	  SettleCampaignItemOutOfBand, which RETAINS the run link; its marker carries
//	  settled_via=issue_closed AND the retained run_id (the distinguishing field).
//
// Returns whether it settled anything so the caller re-derives and re-reads. An
// open issue or a not_planned closure settles NEITHER class.
//
// SOLE SETTLE SITE: reconcileCampaignItemsOnRead (this pass's only caller) is
// the ONLY place a run-less issue-closed item is settled. The create path
// (handleCreateCampaign) performs NO settle, so the FIRST status read of a
// campaign is the effective assembly-time settle — the MCP start_campaign flow
// reads status immediately after assembly, so a chain of already-closed
// children converges on that first read (#1758).
//
// SINGLE-READ FIXPOINT: closed-completed status is resolved via GitHub at most
// ONCE per item in Phase 1, then Phase 2 settles to a fixpoint purely in-memory
// — repeatedly settling any closed-completed candidate whose deps are now in
// the done-set and adding its ref, until a full pass settles nothing new. So a
// closed child C that depends_on a closed child D converges in the SAME read
// regardless of iteration order, instead of one dependency-hop per read.
//
// BEST-EFFORT and FAIL-CLOSED: every guard leaves the item unsettled and NEVER
// fails the read — a nil GitHub client, a repo not in owner/name form, an
// installation-resolution error, a GetIssue error, an unparseable issue_ref, an
// open issue, or a not_planned closure each logs (where useful) and skips. The
// repo installation is resolved at most ONCE per reconcile (lazily, only when a
// candidate item exists) and cached across items; the pass short-circuits
// entirely when GitHub is unwired. The depends_on DAG-order guard is preserved:
// a CLOSED item whose dependency is still OPEN never enters the done-set and so
// stays Blocked, never Eligible.
func (s *Server) settleIssueClosedItems(ctx context.Context, c *campaign.Campaign, items []*campaign.Item) bool {
	if s.cfg.GitHub == nil {
		return false
	}
	owner, name, ok := splitRepoFullName(c.Repo)
	if !ok {
		s.cfg.Logger.Warn("reconcile-on-read: campaign repo not in owner/name form; run-less settle pass skipped",
			"campaign_id", c.ID.String(), "repo", c.Repo)
		return false
	}

	// Done-set: refs of items that have already succeeded (the
	// firstUnmetDependency / NextEligible idiom). A dependency is satisfied iff
	// its ref is in this set; an absent ref is therefore not-satisfied for free.
	// Seeded from the already-succeeded items, then GROWN in Phase 2 as each
	// candidate settles — the growth is what converges a run-less closed-closed
	// chain within this single read.
	done := make(map[string]bool, len(items))
	for _, it := range items {
		if it.State == campaign.ItemStateSucceeded {
			done[it.IssueRef] = true
		}
	}

	// Phase 1 (GitHub reads, at most ONCE per item): the ONE GetIssue per item
	// serves BOTH the settle candidacy check AND the autonomy refresh (#2355), so
	// no new GitHub call CLASS is introduced. The fetch set is the AUTONOMY-RELEVANT
	// set — any item in pending/blocked/failed/cancelled, i.e. every state where
	// campaign.NextEligible still consults Autonomy to partition (running/succeeded/
	// paused route nothing on autonomy: running/succeeded don't consult it, and a
	// PAUSED item is bucketed by NextEligible BEFORE any autonomy check, so its tier
	// is never read — leaving it unfetched is correct). The settle candidate classes
	// A and B are a SUBSET of that set, so the union equals the autonomy-relevant
	// set; the widening does add reads on some polls — for run-less terminal
	// (cancelled/failed with no run link) and run-linked non-running (pending/blocked
	// carrying a run link) items — over the settle-only fetch, honestly NOT parity.
	// Each fetched issue is kept in `fetched` keyed by item id for the refresh pass.
	// Every fail-closed guard applies here EXCEPT the depends_on gate — Phase 2
	// enforces that in-memory so the closed-status read happens exactly once per item
	// and the fixpoint never re-reads GitHub.
	var candidates []*campaign.Item
	fetched := make(map[uuid.UUID]*githubclient.Issue, len(items))
	var scope forge.CredentialScope
	instResolved := false
	for _, it := range items {
		// Two candidate classes settle here (both gated on closed-as-completed):
		//   A (run-less delivery): a run-less item in a settleable pending/blocked
		//     state — a human-led issue merged and closed OUTSIDE any run.
		//   B (out-of-band delivery, #2029): a run-LINKED item whose linked run
		//     went terminal-non-succeeded (cancelled/failed) — a re-shaped-then-
		//     delivered item whose dead run left it terminal but whose issue is now
		//     closed-as-completed. Pass 1 already mapped the terminal run onto the
		//     item state, so no RunRepo read is needed here.
		// A running item is pass 1's; a succeeded item is already settled.
		isClassA := it.RunID == nil &&
			(it.State == campaign.ItemStatePending || it.State == campaign.ItemStateBlocked)
		isClassB := it.RunID != nil &&
			(it.State == campaign.ItemStateCancelled || it.State == campaign.ItemStateFailed)
		// Autonomy-relevant: every state where NextEligible partitions on the tier.
		// Class A/B are a subset, so this predicate governs the whole fetch set.
		autonomyRelevant := it.State == campaign.ItemStatePending ||
			it.State == campaign.ItemStateBlocked ||
			it.State == campaign.ItemStateFailed ||
			it.State == campaign.ItemStateCancelled
		if !autonomyRelevant {
			continue
		}
		number, ok := parseIssueTriggerRef(it.IssueRef)
		if !ok {
			// A non issue:N ref (e.g. a Jira key) has no GitHub issue to read.
			continue
		}
		// Resolve the installation lazily on the first candidate, then reuse it.
		if !instResolved {
			id, err := s.cfg.GitHub.GetRepoInstallation(ctx, forge.RepoRef{Owner: owner, Name: name})
			if err != nil {
				s.cfg.Logger.Warn("reconcile-on-read: resolve installation failed; run-less settle + autonomy refresh pass skipped",
					"campaign_id", c.ID.String(), "repo", c.Repo, "error", err.Error())
				return false
			}
			scope = forge.FromGitHubInstallationID(id)
			instResolved = true
		}
		issue, err := s.cfg.GitHub.GetIssue(ctx, scope, forge.RepoRef{Owner: owner, Name: name}, number)
		if err != nil {
			s.cfg.Logger.Warn("reconcile-on-read: get issue failed; item left unsettled and tier unrefreshed",
				"campaign_id", c.ID.String(), "item_id", it.ID.String(), "issue_ref", it.IssueRef, "error", err.Error())
			continue
		}
		// Keep the issue for the autonomy refresh (its Labels carry the live tier).
		fetched[it.ID] = issue
		// Settle candidacy is class A/B AND a genuine completion: closed AND
		// state_reason=completed. An open issue or a not_planned closure is left
		// unsettled (but its tier is still refreshed below from the same fetch).
		if !isClassA && !isClassB {
			continue
		}
		if issue.State != "closed" || issue.StateReason != "completed" {
			continue
		}
		candidates = append(candidates, it)
	}

	// Autonomy refresh (#2355): fold the fresh tier of every fetched item through
	// SetCampaignItemAutonomy when it DIFFERS from the stored tier, so a relabelled
	// child (autonomy:low -> autonomy:medium) unblocks a campaign parked on
	// attend_human_led on the SAME poll. This runs off the labels the settle fetch
	// already read — no extra GitHub call. It is reported as `refreshedAny` so the
	// caller re-reads items and the rollup reflects the fresh tier.
	refreshedAny := s.refreshItemAutonomyFromIssues(ctx, c, items, fetched)

	// Phase 2 (in-memory fixpoint, no further GitHub calls): settle any candidate
	// whose depends_on refs are all in the done-set, add its ref to the done-set,
	// and repeat until a full pass settles nothing new. This converges a closed
	// C-depends_on-closed-D chain in a SINGLE read regardless of iteration order,
	// while a closed candidate whose dependency is still OPEN is never reached
	// (its ref never enters the done-set) and stays Blocked.
	settledAny := false
	settled := make(map[uuid.UUID]bool, len(candidates))
	for {
		progressed := false
		for _, it := range candidates {
			if settled[it.ID] {
				continue
			}
			// DAG ordering: an out-of-dependency-order human-merge (a dependency
			// not yet in the done-set) is deliberately NOT settled, preserving
			// wave order — a closed item whose dep is still OPEN stays Blocked.
			if !depsSatisfiedRefs(it.DependsOn, done) {
				continue
			}
			// Mark before transitioning so an untransitionable or failing
			// candidate is never revisited by the fixpoint (bounds iteration and
			// avoids a duplicate transition attempt this read).
			settled[it.ID] = true
			// The audit payload for either class; class B additionally carries the
			// retained run_id (the distinguishing marker of the out-of-band-terminal
			// arm — a run-less class-A settle has none).
			payload := map[string]any{
				"campaign_id":  c.ID.String(),
				"issue_ref":    it.IssueRef,
				"outcome":      string(campaign.ItemStateSucceeded),
				"settled_via":  "issue_closed",
				"state_reason": "completed",
			}
			if it.RunID != nil {
				// Class B (out-of-band terminal delivery, #2029): a cancelled/failed
				// item cannot go succeeded through the transition table (it refuses
				// every terminal from), so bypass the gate via
				// SettleCampaignItemOutOfBand, which retains the run link for
				// provenance. run_id present is what distinguishes this arm's audit.
				if _, err := s.cfg.CampaignRepo.SettleCampaignItemOutOfBand(ctx, it.ID); err != nil {
					s.cfg.Logger.Warn("reconcile-on-read: out-of-band terminal settle failed; left for next read",
						"campaign_id", c.ID.String(), "item_id", it.ID.String(), "error", err.Error())
					continue
				}
				payload["run_id"] = it.RunID.String()
			} else {
				// Class A (run-less delivery): the item is pending/blocked, so the
				// guarded transition applies. A defensive re-check keeps the run-less
				// arm untouched.
				if !campaign.ValidCampaignItemTransition(it.State, campaign.ItemStateSucceeded) {
					continue
				}
				if _, err := s.cfg.CampaignRepo.TransitionCampaignItem(ctx, it.ID, campaign.ItemStateSucceeded); err != nil {
					s.cfg.Logger.Warn("reconcile-on-read: run-less settle transition failed; left for next read",
						"campaign_id", c.ID.String(), "item_id", it.ID.String(), "error", err.Error())
					continue
				}
			}
			done[it.IssueRef] = true
			progressed = true
			settledAny = true
			s.emitCampaignAudit(ctx, categoryCampaignIssueSettled, payload)
		}
		if !progressed {
			break
		}
	}
	// The pass CHANGED state (so the caller re-reads) when it settled an item OR
	// refreshed a tier (#2355).
	return settledAny || refreshedAny
}

// refreshItemAutonomyFromIssues folds the live autonomy:* tier of each fetched
// issue back onto its campaign item (#2355), so relabelling a child unblocks a
// campaign parked on attend_human_led without a rebuild. For every item present
// in `fetched` (the autonomy-relevant items settleIssueClosedItems' Phase 1 read
// exactly once), it parses workmgmt.ParseAutonomyLabel(issue.Labels) and — ONLY
// when the parsed tier DIFFERS from the stored tier — calls
// SetCampaignItemAutonomy, emits a campaign_item_autonomy_refreshed audit, and
// reports changed. An unchanged tier performs NO write (the no-op-write
// guarantee). It is BEST-EFFORT and NEVER fails the read: a SetCampaignItemAutonomy
// error logs and leaves the tier stale. Returns whether it refreshed anything.
func (s *Server) refreshItemAutonomyFromIssues(ctx context.Context, c *campaign.Campaign, items []*campaign.Item, fetched map[uuid.UUID]*githubclient.Issue) bool {
	refreshedAny := false
	for _, it := range items {
		issue, ok := fetched[it.ID]
		if !ok {
			continue // not fetched (non-autonomy-relevant state, non issue:N ref, or a GetIssue error).
		}
		want := workmgmt.ParseAutonomyLabel(issue.Labels)
		// No-op-write guarantee: only a genuine tier change is persisted. Both
		// sides are already in the normalized set {"", low, medium, high}, so the
		// comparison is exact.
		if want == it.Autonomy {
			continue
		}
		if _, err := s.cfg.CampaignRepo.SetCampaignItemAutonomy(ctx, it.ID, want); err != nil {
			s.cfg.Logger.Warn("reconcile-on-read: refresh item autonomy failed; tier left stale (read still succeeds)",
				"campaign_id", c.ID.String(), "item_id", it.ID.String(), "issue_ref", it.IssueRef,
				"from", it.Autonomy, "to", want, "error", err.Error())
			continue
		}
		s.emitCampaignAudit(ctx, categoryCampaignItemAutonomyRefreshed, map[string]any{
			"campaign_id": c.ID.String(),
			"issue_ref":   it.IssueRef,
			"from":        it.Autonomy,
			"to":          want,
		})
		refreshedAny = true
	}
	return refreshedAny
}

// refreshOneItemAutonomy re-reads the single named item's autonomy:* label from
// its GitHub issue and persists the fresh tier when it differs (#2355) — the
// targeted refresh the operator start verb runs so a relabel-then-start works
// without an intervening status poll. It returns the (possibly-updated) item and
// whether it changed the stored tier. BEST-EFFORT and FAIL-OPEN on every guard —
// GitHub unwired, a repo not in owner/name form, an installation-resolve error, a
// non issue:N ref, a GetIssue error, or a SetCampaignItemAutonomy error each
// leaves the item on its stored tier and reports changed=false, so the DAG gate
// runs exactly as it would today.
func (s *Server) refreshOneItemAutonomy(ctx context.Context, c *campaign.Campaign, item *campaign.Item) (*campaign.Item, bool) {
	if s.cfg.GitHub == nil {
		return item, false
	}
	owner, name, ok := splitRepoFullName(c.Repo)
	if !ok {
		return item, false
	}
	number, ok := parseIssueTriggerRef(item.IssueRef)
	if !ok {
		return item, false // a non issue:N ref (e.g. a Jira key) has no GitHub issue to read.
	}
	instID, err := s.cfg.GitHub.GetRepoInstallation(ctx, forge.RepoRef{Owner: owner, Name: name})
	if err != nil {
		s.cfg.Logger.Warn("start-verb autonomy refresh: resolve installation failed; item keeps stored tier",
			"campaign_id", c.ID.String(), "item_id", item.ID.String(), "error", err.Error())
		return item, false
	}
	scope := forge.FromGitHubInstallationID(instID)
	issue, err := s.cfg.GitHub.GetIssue(ctx, scope, forge.RepoRef{Owner: owner, Name: name}, number)
	if err != nil {
		s.cfg.Logger.Warn("start-verb autonomy refresh: get issue failed; item keeps stored tier",
			"campaign_id", c.ID.String(), "item_id", item.ID.String(), "issue_ref", item.IssueRef, "error", err.Error())
		return item, false
	}
	want := workmgmt.ParseAutonomyLabel(issue.Labels)
	if want == item.Autonomy {
		return item, false
	}
	updated, err := s.cfg.CampaignRepo.SetCampaignItemAutonomy(ctx, item.ID, want)
	if err != nil {
		s.cfg.Logger.Warn("start-verb autonomy refresh: persist tier failed; item keeps stored tier",
			"campaign_id", c.ID.String(), "item_id", item.ID.String(), "from", item.Autonomy, "to", want, "error", err.Error())
		return item, false
	}
	s.emitCampaignAudit(ctx, categoryCampaignItemAutonomyRefreshed, map[string]any{
		"campaign_id": c.ID.String(),
		"issue_ref":   item.IssueRef,
		"from":        item.Autonomy,
		"to":          want,
	})
	return updated, true
}

// depsSatisfiedRefs reports whether every dep ref is in the done set. An empty
// dep list is trivially satisfied; an absent ref is not-satisfied. Mirrors the
// campaign engine's depsSatisfied (unexported there) for the run-less settle
// pass's DAG-order gate.
func depsSatisfiedRefs(deps []string, done map[string]bool) bool {
	for _, d := range deps {
		if !done[d] {
			return false
		}
	}
	return true
}

// deriveCampaignAfterChange re-derives the campaign state from its items and,
// when it differs and the transition is valid, transitions the campaign and
// emits a campaign_advanced marker. Best-effort and idempotent: a no-change
// derivation, an invalid transition, or a transition error emits nothing and
// never unwinds the caller. Shared by the operator-driven start (pending →
// running on first dispatch) and reconcile-on-read (running → succeeded/failed
// as items settle).
func (s *Server) deriveCampaignAfterChange(ctx context.Context, c *campaign.Campaign, items []*campaign.Item) {
	// Capture the pre-transition state: TransitionCampaign may mutate the shared
	// campaign pointer (the in-memory fake aliases it), so read prevState BEFORE
	// the write to keep the pending->running board sweep and the audit "from"
	// correct regardless of aliasing.
	prevState := c.State
	newState := campaign.DeriveState(items)
	if newState == prevState || !campaign.ValidCampaignTransition(prevState, newState) {
		return
	}
	if _, err := s.cfg.CampaignRepo.TransitionCampaign(ctx, c.ID, newState); err != nil {
		s.cfg.Logger.Warn("campaign derivation transition failed; left for next read",
			"campaign_id", c.ID.String(), "from", string(prevState), "to", string(newState), "error", err.Error())
		return
	}
	s.emitCampaignAudit(ctx, categoryCampaignAdvanced, map[string]any{
		"campaign_id": c.ID.String(),
		"from":        string(prevState),
		"to":          string(newState),
	})

	// On the pending -> running edge (#1816), sweep the campaign's still-queued
	// items onto the board's Up Next column via the campaign_started board hook.
	// The just-dispatched running item is naturally excluded (it is no longer
	// pending). Best-effort: each move is a no-op-or-log and never unwinds the
	// derivation the campaign already recorded.
	if prevState == campaign.StatePending && newState == campaign.StateRunning {
		for _, it := range items {
			if it.State != campaign.ItemStatePending {
				continue
			}
			number, ok := parseIssueTriggerRef(it.IssueRef)
			if !ok {
				continue // a non issue:N ref (e.g. a Jira key) has no GitHub issue to board.
			}
			s.boardTransitionForCampaignItem(ctx, c, number, lifecycleCampaignStarted)
		}
	}
}

// recoveryDescendantListLimit bounds each ListRuns page fetching a run's
// recovery children. A resume/recovery fan-out at any single level is a
// handful of runs (a run is resumed a few times at most), so 100 is generous
// and never truncates a real lineage.
const recoveryDescendantListLimit = 100

// newestTerminalRecoveryDescendant transitively walks the recovery-child
// lineage rooted at failedRunID — runs carrying parent_run_id = an ancestor,
// the same lineage loadApprovedPlanForRun walks upward (#216) — and returns
// the newest terminal descendant by CreatedAt, or inFlight=true when ANY
// descendant is still non-terminal (a recovery in progress). Returns
// (nil, false) when the failed run has no recovery children at all — the
// caller then preserves today's direct failed settle.
//
// It is a BFS over the parent_run_id tree, cycle-guarded with a visited set so
// a corrupt parent_run_id cycle can't loop forever.
//
// The ParentRunID filter alone is NOT a discriminator (#2549):
// run.ChildParamsFrom sets ParentRunID for EVERY child kind, so a decomposition
// slice carries BOTH parent_run_id AND decomposed_from and is indistinguishable
// from a recovery under that filter. What makes this lineage recovery-only is
// the explicit DecomposedFrom == nil skip below — handleRecoverRun
// (server/recover.go, operator resume) and the CI-failure retry mint
// (webhook/dispatcher.go) both leave DecomposedFrom zero, while orchestrator
// fan-out sets it (#455). The skip buys two outcomes: a failed decomposition
// parent settles failed off ITSELF regardless of whichever slice sorts newest
// by CreatedAt, and a still-running slice is not read as an in-flight recovery
// so it cannot hang the campaign item running. The #1751 settle-off-the-
// recovery behavior is unchanged for the lineage that owns it.
//
// BEST-EFFORT: a ListRuns error logs and returns (nil, false) so reconcile
// never fails the read — the item then keeps today's failed settle.
func (s *Server) newestTerminalRecoveryDescendant(ctx context.Context, failedRunID uuid.UUID) (*run.Run, bool) {
	if s.cfg.RunRepo == nil {
		return nil, false
	}
	visited := map[uuid.UUID]bool{failedRunID: true}
	queue := []uuid.UUID{failedRunID}
	var newest *run.Run
	for len(queue) > 0 {
		parent := queue[0]
		queue = queue[1:]
		children, err := s.cfg.RunRepo.ListRuns(ctx, run.ListRunsFilter{
			ParentRunID: &parent,
			Limit:       recoveryDescendantListLimit,
		})
		if err != nil {
			s.cfg.Logger.Warn("reconcile-on-read: list recovery children failed; treating failed run as having no recovery lineage",
				"parent_run_id", parent.String(), "error", err.Error())
			return nil, false
		}
		for _, child := range children {
			if visited[child.ID] {
				continue
			}
			visited[child.ID] = true
			// The discriminator (#2549): a decomposition slice carries
			// parent_run_id like every other child, so the filter above
			// returns it — but it is neither a settle candidate nor an
			// in-flight signal, and its own subtree (e.g. a resume OF a
			// slice) is not this run's recovery lineage either, so it is
			// not enqueued. Marking it visited first keeps the cycle
			// guard total over the parent_run_id graph.
			if child.DecomposedFrom != nil {
				continue
			}
			// Any non-terminal descendant means a recovery is still in flight;
			// leave the item running for a later read rather than settling it.
			if !child.State.IsTerminal() {
				return nil, true
			}
			if newest == nil || child.CreatedAt.After(newest.CreatedAt) {
				newest = child
			}
			queue = append(queue, child.ID)
		}
	}
	return newest, false
}

// mapRunTerminalToItemState maps a terminal run state to the campaign item's
// terminal state (ok=false for a non-terminal or unmapped state). It replicates
// campaigndriver.mapRunTerminalToItem rather than importing the driver package
// (an unusual server → driver import edge); the run enum's terminal set is
// exactly {succeeded, failed, cancelled}.
func mapRunTerminalToItemState(st run.State) (campaign.ItemState, bool) {
	switch st {
	case run.StateSucceeded:
		return campaign.ItemStateSucceeded, true
	case run.StateFailed:
		return campaign.ItemStateFailed, true
	case run.StateCancelled:
		return campaign.ItemStateCancelled, true
	default:
		return "", false
	}
}

// emitCampaignAudit appends one campaign-level audit entry on the global chain,
// mirroring the campaigndriver emit posture. Best-effort: a nil AuditRepo, a
// marshal error, or an append error logs and never unwinds the transition it
// records.
func (s *Server) emitCampaignAudit(ctx context.Context, category string, payload map[string]any) {
	if s.cfg.AuditRepo == nil {
		return
	}
	body, err := json.Marshal(payload)
	if err != nil {
		s.cfg.Logger.Warn("marshal campaign audit payload failed",
			"category", category, "error", err.Error())
		return
	}
	systemKind := audit.ActorSystem
	if _, err := s.cfg.AuditRepo.AppendGlobalChained(ctx, audit.GlobalChainAppendParams{
		Timestamp: time.Now().UTC(),
		Category:  category,
		ActorKind: &systemKind,
		Payload:   body,
		// Every emit site is request-triggered (create/restart handlers and
		// the reconcile-on-read settle paths), so the caller's Identity is the
		// account source (ADR-057 / #1828); for a tenanted campaign
		// enforceCampaignAccount has already pinned it to the campaign's own
		// account before any of these paths run.
		AccountID: identityAccountID(ctx),
	}); err != nil {
		s.cfg.Logger.Warn("append campaign audit entry failed",
			"category", category, "error", err.Error())
	}
}
