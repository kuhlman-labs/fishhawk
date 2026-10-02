package mcpserver

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// fishhawk_list_schedules (E79.1 / #3725) is the read-only MCP half of the
// scheduler's visibility surface: a thin wrapper over GET /v0/schedules
// (backend/internal/server/schedules.go). It re-derives nothing — the cron
// math, the window and the last outcome are the scheduler's, and every refusal
// (401, 403 repo_forbidden, 400) is the backend's. It never writes and mints no
// audit entry.

// ListSchedulesInput is the fishhawk_list_schedules tool's input schema.
type ListSchedulesInput struct {
	Repo string `json:"repo,omitempty" jsonschema:"target repo as owner/name; falls back to GITHUB_REPOSITORY env when omitted"`
}

// ScheduleOutcome mirrors one window's recorded attempt on GET /v0/schedules.
type ScheduleOutcome struct {
	Kind        string    `json:"kind" jsonschema:"started, already_started (an Idempotency-Key replay of a run already started for this window), refused (an admission control refused it — code and message say which), or transient_error (log-only, retried next tick)"`
	WindowStart time.Time `json:"window_start"`
	RunID       string    `json:"run_id,omitempty" jsonschema:"the started or replayed run; absent for a refusal"`
	Code        string    `json:"code,omitempty" jsonschema:"the refusal's error code, e.g. budget_exhausted"`
	Message     string    `json:"message,omitempty"`
	At          time.Time `json:"at"`
}

// ScheduleEntry mirrors one scheduled workflow on GET /v0/schedules.
type ScheduleEntry struct {
	WorkflowID         string           `json:"workflow_id"`
	Cron               string           `json:"cron" jsonschema:"the declared 5-field cron expression"`
	Timezone           string           `json:"timezone" jsonschema:"IANA timezone the cron is evaluated in; UTC when the schedule names none"`
	Issue              int              `json:"issue,omitempty" jsonschema:"the schedule's optional anchor issue number"`
	CurrentWindowStart *time.Time       `json:"current_window_start" jsonschema:"the latest cron fire at or before the last tick (UTC); a window yields at most one run. Null before the first fire"`
	NextDueAt          *time.Time       `json:"next_due_at" jsonschema:"the first cron fire strictly after the last tick (UTC); null when none is within the horizon"`
	LastOutcome        *ScheduleOutcome `json:"last_outcome" jsonschema:"the last attempt recorded for this workflow; null when none was made by the running scheduler"`
}

// ListSchedulesOutput mirrors the GET /v0/schedules body and is the tool's
// result. Elisions is present ONLY when the response was reduced to fit the
// tool-result byte budget.
type ListSchedulesOutput struct {
	Repo         string          `json:"repo"`
	Enabled      bool            `json:"enabled" jsonschema:"false when the deployment did not pass --enable-scheduler; reason then names the switch"`
	Reason       string          `json:"reason,omitempty" jsonschema:"why nothing is evaluated: the scheduler is off, or this repo is not in --scheduler-repos"`
	RepoScanned  bool            `json:"repo_scanned" jsonschema:"whether the scheduler scans this repository"`
	LastTickAt   *time.Time      `json:"last_tick_at" jsonschema:"when the scheduler last scanned this repository; null before its first tick"`
	SpecError    string          `json:"spec_error,omitempty" jsonschema:"the most recent spec fetch/parse failure; while set no schedule in the repo is evaluated"`
	RunnerKind   string          `json:"runner_kind,omitempty" jsonschema:"the runner kind every scheduled run starts with"`
	DispatchNote string          `json:"dispatch_note,omitempty" jsonschema:"present for runner_kind local: a started run parks at awaiting_host_dispatch until a host dispatches it; the scheduler does not auto-dispatch"`
	Schedules    []ScheduleEntry `json:"schedules" jsonschema:"one entry per scheduled workflow, sorted by workflow_id"`
	Elisions     *Elisions       `json:"elisions,omitempty"`
}

// registerListSchedules wires the fishhawk_list_schedules tool.
func registerListSchedules(srv *mcp.Server, resolver *runResolver) {
	mcp.AddTool(srv, &mcp.Tool{
		Name: "fishhawk_list_schedules",
		Description: strings.TrimSpace(`
Use this when you need to know whether, when and how a workflow runs on a
schedule — e.g. whether the groomer's weekly run started, why a scheduled run
did not appear, or when the next one is due (E79.1).

ELIGIBILITY: an authenticated read with no scope; the backend refuses a repo
you cannot read (403 repo_forbidden). It never writes and mints no audit entry.

For each workflow declaring a workflow-level schedule it reports the cron,
timezone, optional anchor issue, the current due window (the latest fire at or
before the scheduler's last tick), the next due time, and the last outcome:
started, already_started (an Idempotency-Key replay — the window already has
its run), refused (an admission control's code and message, e.g.
budget_exhausted), or transient_error (retried next tick). Only the LATEST
window is ever started, exactly once per (repo, workflow, window); a started
run still stops at every gate.

enabled:false (with a reason naming --enable-scheduler) means nothing is
scheduled on this deployment — it is not an error. With runner_kind local,
dispatch_note says a started run parks at awaiting_host_dispatch until you
dispatch it (fishhawk_dispatch_stage / fishhawk_run_stage); the scheduler does
not auto-dispatch.

repo falls back to GITHUB_REPOSITORY. Tool errors: repo missing;
validation_failed (400); authentication_required (401); repo_forbidden (403);
service_unavailable (503).
`),
	}, resolver.listSchedules)
}

// listSchedules is the tool handler.
func (r *runResolver) listSchedules(ctx context.Context, req *mcp.CallToolRequest, in ListSchedulesInput) (*mcp.CallToolResult, ListSchedulesOutput, error) {
	repo := strings.TrimSpace(in.Repo)
	if repo == "" {
		repo = strings.TrimSpace(r.getenv("GITHUB_REPOSITORY"))
	}
	if repo == "" {
		return nil, ListSchedulesOutput{}, fmt.Errorf("repo is required: pass repo or set GITHUB_REPOSITORY")
	}
	res, err := r.api.ListSchedules(ctx, repo)
	if err != nil {
		// A refusal is a TOOL ERROR, never an empty list: an empty schedules
		// array reads as "nothing is scheduled", the opposite of "we could
		// not tell you".
		return nil, ListSchedulesOutput{}, fmt.Errorf("list schedules: %w", err)
	}
	out, err := boundListSchedulesOutput(*res, repo, r.responseBudget(req))
	if err != nil {
		return nil, ListSchedulesOutput{}, err
	}
	return nil, out, nil
}

// ListSchedules reads GET /v0/schedules?repo= (E79.1 / #3725). It never
// writes. 4xx/5xx surfaces as *apiError:
//   - 400 validation_failed (repo missing or not owner/name)
//   - 401 authentication_required
//   - 403 repo_forbidden
//   - 503 service_unavailable (repo visibility unresolvable)
func (c *apiClient) ListSchedules(ctx context.Context, repo string) (*ListSchedulesOutput, error) {
	q := url.Values{}
	q.Set("repo", repo)
	var out ListSchedulesOutput
	if err := c.do(ctx, http.MethodGet, "/v0/schedules?"+q.Encode(), nil, &out); err != nil {
		return nil, err
	}
	if out.Schedules == nil {
		out.Schedules = []ScheduleEntry{}
	}
	return &out, nil
}

// schedulesStringCap bounds spec_error and each last_outcome.message at the B1
// tier, in ENCODED bytes (capJSONString).
const schedulesStringCap = 256

// boundListSchedulesOutput is the ADR-077 ladder for fishhawk_list_schedules.
// The body is one entry per scheduled workflow, so it is small in practice;
// the ladder exists so an outsized spec error or refusal message, or a spec
// declaring very many schedules, can never fail the tool result. Every tier
// MARKS its truncation, and both point at the unbounded REST read.
//
//	B1  cap spec_error and every last_outcome.message.
//	B2  drop schedules from the TAIL of the workflow_id-sorted list, halving,
//	    down to zero entries. At zero the body is the constant-size scalars
//	    plus the capped spec_error and the elision block, which is what makes
//	    the ladder converge.
func boundListSchedulesOutput(out ListSchedulesOutput, repo string, budget responseBudget) (ListSchedulesOutput, error) {
	set := func(o *ListSchedulesOutput, e *Elisions) { o.Elisions = e }
	n, err := marshalledLen(out)
	if err != nil {
		return out, err
	}
	if n <= budget.bytes {
		return out, nil
	}
	surface := pointerREST("/v0/schedules?repo=" + url.QueryEscape(repo))
	led := &elisionLedger{budget: budget.bytes, source: budget.source, tier: "B1",
		note: "response reduced to fit the tool-result byte budget"}

	capped := 0
	if c := capJSONString(out.SpecError, schedulesStringCap); c != out.SpecError {
		out.SpecError = c
		capped++
	}
	for i := range out.Schedules {
		if o := out.Schedules[i].LastOutcome; o != nil {
			if c := capJSONString(o.Message, schedulesStringCap); c != o.Message {
				cp := *o
				cp.Message = c
				out.Schedules[i].LastOutcome = &cp
				capped++
			}
		}
	}
	if capped > 0 {
		led.add(newOversizedCapableElision("spec_error, schedules[].last_outcome.message", fmt.Sprintf(
			"%d strings were truncated to fit the byte budget; the surface below returns them in full", capped),
			surface, capped))
	}
	fits, err := attachAndMeasureOut(&out, led, budget.bytes, set)
	if err != nil || fits {
		return out, err
	}

	base := append([]elidedField(nil), led.entries...)
	total := len(out.Schedules)
	for len(out.Schedules) > 0 {
		keep := len(out.Schedules) / 2
		out.Schedules = out.Schedules[:keep]
		led.tier = "B2"
		led.entries = append(append([]elidedField(nil), base...),
			newStoredElision("schedules", fmt.Sprintf(
				"%d trailing schedules were dropped to fit the byte budget. The list is workflow_id-ASCENDING, so the drop is positional; the surface below returns every schedule",
				total-keep), surface.retrievalPointer, total-keep))
		fits, err := attachAndMeasureOut(&out, led, budget.bytes, set)
		if err != nil || fits {
			return out, err
		}
	}
	return out, nil
}
