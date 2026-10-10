package mcpserver

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/google/uuid"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// EpicDraft is the tool-visible mirror of the canonical refinement draft
// schema (docs/api/v0.openapi.yaml #/components/schemas/EpicDraft): the epic
// plus its children. It is modeled as a typed struct rather than opaque
// json.RawMessage on purpose — the direct-edit arm's MCP input schema must be
// expressive enough for the driving agent, and the server strict-decodes the
// PATCH `draft` body with additionalProperties:false, so any drift between
// this shape and the canonical schema surfaces as a 422 the round-trip wire
// test catches. It is BOTH the direct-edit arm's input and the `latest_draft`
// field of every RefinementSession the tool returns.
type EpicDraft struct {
	Epic     EpicDraftEpic    `json:"epic" jsonschema:"the epic half of the draft: its one-line summary, in-scope prose, and out-of-scope prose"`
	Children []EpicDraftChild `json:"children" jsonschema:"the epic's children, at least one; depends_on entries are 1-based sibling ordinals into THIS list, not issue numbers"`
}

// EpicDraftEpic mirrors the epic sub-object (summary, scope, out_of_scope).
type EpicDraftEpic struct {
	Summary    string `json:"summary" jsonschema:"the epic's one-line summary"`
	Scope      string `json:"scope" jsonschema:"prose describing what the epic covers"`
	OutOfScope string `json:"out_of_scope" jsonschema:"prose describing what the epic deliberately excludes"`
}

// EpicDraftChild mirrors one child of the draft. DependsOn entries are 1-based
// sibling ordinals into EpicDraft.Children (ordinal 1 is the first child), NOT
// issue numbers — children have no issue numbers until the file arm files them.
type EpicDraftChild struct {
	Summary            string   `json:"summary" jsonschema:"the child's one-line summary"`
	Proposal           string   `json:"proposal" jsonschema:"the child's proposal prose"`
	DoneMeans          string   `json:"done_means" jsonschema:"the child's definition of done"`
	AcceptanceCriteria []string `json:"acceptance_criteria" jsonschema:"the child's acceptance criteria"`
	Labels             []string `json:"labels" jsonschema:"the child's labels"`
	DependsOn          []int    `json:"depends_on" jsonschema:"1-based sibling ordinals into this children list this child depends on (not issue numbers); a dangling or cyclic edge fails the edit 422"`
}

// DraftEpicInput carries the five MUTUALLY-EXCLUSIVE arms of
// fishhawk_draft_epic, each mapping 1:1 onto one E34.2/E34.3 refinement
// endpoint. Exactly one arm must be populated; arm dispatch fails closed with
// zero HTTP calls otherwise (see draftEpic). The arms:
//
//   - open:    brief                          -> POST   /v0/refinement/sessions
//   - preview: session_id (alone)             -> GET    /v0/refinement/sessions/{id}
//   - edit:    session_id + (brief_amendment  -> PATCH  /v0/refinement/sessions/{id}/draft
//     XOR draft)
//   - decide:  session_id + decision + reason -> POST   .../decision
//   - file:    session_id + repo              -> POST   .../file
type DraftEpicInput struct {
	Brief          string     `json:"brief,omitempty" jsonschema:"OPEN arm: a natural-language brief to decompose into an epic + children. Opens a new session and returns the initial draft. Mutually exclusive with session_id and every other arm"`
	SessionID      string     `json:"session_id,omitempty" jsonschema:"the refinement session UUID returned by the open arm. Required by every arm except open. Supplied ALONE it is the preview arm (read the current draft + derived state)"`
	BriefAmendment string     `json:"brief_amendment,omitempty" jsonschema:"EDIT arm (agent re-draft): an amendment composed onto the stored brief and re-drafted by the agent. Requires session_id; mutually exclusive with draft. Bounded by a per-session budget of 3"`
	Draft          *EpicDraft `json:"draft,omitempty" jsonschema:"EDIT arm (direct field edit): a full replacement EpicDraft, validated with no agent call. Requires session_id; mutually exclusive with brief_amendment"`
	Decision       string     `json:"decision,omitempty" jsonschema:"DECIDE arm: approved or rejected, pinning the latest revision. Requires session_id and reason"`
	Reason         string     `json:"reason,omitempty" jsonschema:"DECIDE arm: the required decision rationale (recorded and audited). Only valid alongside decision"`
	Repo           string     `json:"repo,omitempty" jsonschema:"FILE arm: the target repository as owner/name to file the approved draft into. Requires session_id; the repo is pinned at first invoke and a re-invoke naming a different repo is rejected"`
}

// SessionGuidance is one legal next fishhawk_draft_epic move for a session's
// derived state — a next_actions-STYLE block LOCAL to this tool. Refinement
// sessions have no run UUID, so it is deliberately NOT threaded through
// next_actions.go's run-scoped NextActions machinery (that would couple two
// unrelated lifecycles). Every session-view arm returns at least one entry so
// the operator never guesses the next verb.
type SessionGuidance struct {
	State     string            `json:"state" jsonschema:"the derived session state this guidance is for (awaiting_approval, approved, rejected, drifted, filing_in_progress, filing_failed, filing_incomplete, filed)"`
	Arm       string            `json:"arm" jsonschema:"the fishhawk_draft_epic arm to invoke next, or 'terminal' when the session is complete"`
	Arguments map[string]string `json:"arguments,omitempty" jsonschema:"the arguments to pass to that arm; values naming a field describe what to supply"`
	Reason    string            `json:"reason" jsonschema:"one-line why-this-now"`
}

// DraftEpicOutput is the tool result. Session carries the RefinementSession
// mirror for the four session-view arms (open, preview, edit, decide); Filing
// carries the RefinementFilingResult for the file arm; exactly one is set.
// SessionGuidance always names the next invocation for the derived state.
type DraftEpicOutput struct {
	Session         *RefinementSession      `json:"session,omitempty" jsonschema:"the session view (state, drifted, revision_count, latest_origin, latest_draft, preview, waves, criteria_precheck, decisions) for the open/preview/edit/decide arms"`
	Filing          *RefinementFilingResult `json:"filing,omitempty" jsonschema:"the file arm's result: status (filing_in_progress, already_in_progress, or filed), the epic + children filed so far, child_count, budget_seconds, already_in_progress, resumed, already_completed"`
	SessionGuidance []SessionGuidance       `json:"session_guidance" jsonschema:"the legal next fishhawk_draft_epic moves for the current derived state, first is the suggested default"`
}

// legalArmsHelp enumerates the five legal arm combinations. It is appended to
// every fail-closed arm-dispatch error so the message is precise about what
// failed AND how to fix it (BRAND_FOUNDATIONS §5 error style).
const legalArmsHelp = `exactly one arm must be populated: ` +
	`(1) open — brief alone; ` +
	`(2) preview — session_id alone; ` +
	`(3) edit — session_id + exactly one of brief_amendment or draft; ` +
	`(4) decide — session_id + decision (approved|rejected) + reason; ` +
	`(5) file — session_id + repo`

// registerDraftEpic wires the fishhawk_draft_epic tool (E34.4 / #1595): the
// single operator MCP surface over the E34 refinement loop (ADR-052), exposing
// the five backend operations that already shipped (#1629/#1631/#1632) as five
// mutually-exclusive arms of ONE tool. Reuse-first per the E31.9 precedent —
// every existing decision verb (fishhawk_approve_plan / fishhawk_reject_plan /
// fishhawk_approve_deploy) is stage-gated (it resolves a run/stage UUID and
// posts to the stage-approval endpoint), while refinement sessions are not
// runs and have no stages, so no existing verb's shape fits; one tool with
// arms keeps the registry at +1.
//
// Auth: a write tool requiring write:approvals — no new scope (the E34.2
// precedent), so the operator token already driving fishhawk_approve_plan
// works unchanged.
func registerDraftEpic(srv *mcp.Server, resolver *runResolver) {
	mcp.AddTool(srv, &mcp.Tool{
		Name: "fishhawk_draft_epic",
		Description: strings.TrimSpace(`
Use this when you have a natural-language brief to decompose into an epic plus
its children, and for every subsequent step of that refinement session —
preview, edit, approve/reject, and file. It is the single operator surface over
the E34 refinement loop (ADR-052): draft -> preview -> edit -> approve -> file.
approve and file are ARMS on this tool, not separate verbs — do not reach for
fishhawk_approve_plan (that is stage-gated and resolves a run/stage; a
refinement session is neither).

It wraps five mutually-exclusive arms, exactly one populated per call:
- open:    brief                                  -> drafts an epic + children from the brief, opens a session
- preview: session_id (alone)                      -> reads the current draft + derived approval state
- edit:    session_id + (brief_amendment | draft)  -> appends a new revision (agent re-draft, or a direct EpicDraft edit); either re-gates the session to awaiting_approval
- decide:  session_id + decision + reason          -> approves or rejects the latest revision (reason required)
- file:    session_id + repo                        -> launches filing of the approved, un-drifted draft into the tracker (detached and idempotent; the repo is pinned at first invoke)

Arm dispatch fails closed with NO HTTP call when zero arms or an illegal
combination is populated, and the error enumerates the legal combinations.

Every arm returns the session view (state, drifted, revision_count,
latest_origin, latest_draft, preview, waves, criteria_precheck, decisions,
filing), or — for the file arm — the filing result (status, the epic/children
filed so far, child_count, budget_seconds, already_in_progress, resumed,
already_completed). criteria_precheck is the E34.5 advisory acceptance-criteria
screen over the draft's children: a child flagged no_blocking_criterion sets
needs_attention (approval remains legal).

The file arm is DETACHED: the backend checks the gate synchronously, then files
in the background and returns at once with status filing_in_progress (or
already_in_progress when a filing for the draft is already running — nothing
new is launched), so a long filing can no longer time out mid-sequence. Watch it
with the preview arm: the session's filing block reports in_progress, failed
(last_error, failed_ordinal, step), incomplete (a filing session is open but
nothing is running — a backend restart), or filed, with the items filed so far;
the session state becomes filed once it completes. After failed or incomplete,
re-invoke the file arm with the SAME repo: it resumes without re-filing a
recorded item. A completed session's file arm replays status filed with
already_completed (no writes).

Each result also carries session_guidance: the exact next fishhawk_draft_epic
arm + arguments for the derived state (awaiting_approval -> decide, naming any
criteria-flagged child ordinals; rejected -> re-draft via brief_amendment or a
direct draft edit; approved -> file; drifted -> re-decide the latest revision;
filing in progress -> preview; filing failed or incomplete -> file with the
pinned repo; filed -> terminal), so you never guess the next verb.

Tool errors surface the backend code verbatim: amendment_budget_exhausted (the
per-session brief-amendment budget of 3 is spent — switch to a direct draft
edit), decision_already_recorded (re-gate by EDITING, never decide twice),
refinement_not_approved (approve before filing), refinement_draft_drifted
(re-decide the latest revision), refinement_filing_repo_mismatch (re-invoke
with the pinned repo), refinement_filing_failed (the filing could not be
started — nothing was launched; fix the cause and re-invoke with the SAME
repo), refinement_session_not_found, refinement_repo_unconfigured,
refinement_drafting_unavailable / refinement_drafting_failed.
`),
	}, resolver.draftEpic)
}

// draftEpic is the tool handler. It classifies which of the five arms the
// input populated, fails closed (no HTTP) on zero arms or an illegal
// combination, dispatches to the matching apiClient method, and attaches the
// derived-state session_guidance so the operator's next arm is named.
func (r *runResolver) draftEpic(ctx context.Context, _ *mcp.CallToolRequest, in DraftEpicInput) (*mcp.CallToolResult, DraftEpicOutput, error) {
	hasBrief := strings.TrimSpace(in.Brief) != ""
	hasSession := strings.TrimSpace(in.SessionID) != ""
	hasAmendment := strings.TrimSpace(in.BriefAmendment) != ""
	hasDraft := in.Draft != nil
	hasDecision := strings.TrimSpace(in.Decision) != ""
	hasReason := strings.TrimSpace(in.Reason) != ""
	hasRepo := strings.TrimSpace(in.Repo) != ""

	// OPEN arm: brief alone. Any other populated field is an illegal combination.
	if hasBrief {
		if hasSession || hasAmendment || hasDraft || hasDecision || hasReason || hasRepo {
			return armError("the open arm (brief) cannot be combined with any other field")
		}
		return r.draftEpicOpen(ctx, in.Brief)
	}

	// Every remaining arm requires session_id.
	if !hasSession {
		return armError("no arm populated: pass brief to open a session, or session_id to act on one")
	}

	sessionID, err := uuid.Parse(strings.TrimSpace(in.SessionID))
	if err != nil {
		return nil, DraftEpicOutput{}, fmt.Errorf("session_id is not a valid UUID: %q", in.SessionID)
	}

	// Classify the session-scoped sub-arm. Exactly one of edit / decide / file
	// may be populated alongside session_id; more than one is illegal.
	editArm := hasAmendment || hasDraft
	decideArm := hasDecision || hasReason
	fileArm := hasRepo
	subArms := 0
	if editArm {
		subArms++
	}
	if decideArm {
		subArms++
	}
	if fileArm {
		subArms++
	}
	if subArms > 1 {
		return armError("session_id may carry only ONE of the edit (brief_amendment|draft), decide (decision+reason), or file (repo) arms")
	}

	switch {
	case subArms == 0:
		// PREVIEW arm: session_id alone.
		return r.draftEpicPreview(ctx, sessionID)
	case editArm:
		if hasAmendment && hasDraft {
			return armError("the edit arm takes exactly one of brief_amendment or draft, not both")
		}
		return r.draftEpicEdit(ctx, sessionID, in.BriefAmendment, in.Draft)
	case decideArm:
		if !hasDecision {
			return armError("the decide arm requires decision (approved or rejected); reason alone is not a valid arm")
		}
		if in.Decision != "approved" && in.Decision != "rejected" {
			return nil, DraftEpicOutput{}, fmt.Errorf("decision must be approved or rejected, got %q", in.Decision)
		}
		if !hasReason {
			return nil, DraftEpicOutput{}, fmt.Errorf("the decide arm requires a non-empty reason (the decision rationale is recorded and audited)")
		}
		return r.draftEpicDecide(ctx, sessionID, in.Decision, in.Reason)
	default: // fileArm
		return r.draftEpicFile(ctx, sessionID, in.Repo)
	}
}

// armError returns the fail-closed dispatch error: the specific problem plus
// the enumeration of the legal arms, and an empty output (no HTTP was made).
func armError(problem string) (*mcp.CallToolResult, DraftEpicOutput, error) {
	return nil, DraftEpicOutput{}, fmt.Errorf("fishhawk_draft_epic: %s. %s", problem, legalArmsHelp)
}

func (r *runResolver) draftEpicOpen(ctx context.Context, brief string) (*mcp.CallToolResult, DraftEpicOutput, error) {
	sess, err := r.api.CreateRefinementSession(ctx, brief)
	if err != nil {
		return nil, DraftEpicOutput{}, fmt.Errorf("draft epic: %w", err)
	}
	return nil, DraftEpicOutput{Session: sess, SessionGuidance: guidanceForSession(sess)}, nil
}

func (r *runResolver) draftEpicPreview(ctx context.Context, sessionID uuid.UUID) (*mcp.CallToolResult, DraftEpicOutput, error) {
	sess, err := r.api.GetRefinementSession(ctx, sessionID)
	if err != nil {
		return nil, DraftEpicOutput{}, fmt.Errorf("preview refinement session: %w", err)
	}
	return nil, DraftEpicOutput{Session: sess, SessionGuidance: guidanceForSession(sess)}, nil
}

func (r *runResolver) draftEpicEdit(ctx context.Context, sessionID uuid.UUID, briefAmendment string, draft *EpicDraft) (*mcp.CallToolResult, DraftEpicOutput, error) {
	sess, err := r.api.EditRefinementDraft(ctx, sessionID, briefAmendment, draft)
	if err != nil {
		var ae *apiError
		if errors.As(err, &ae) && ae.Code == "amendment_budget_exhausted" {
			// The brief-amendment budget (3) is spent; the direct draft edit
			// remains open and re-gates the session the same way.
			return nil, DraftEpicOutput{}, fmt.Errorf("edit refinement draft: %w: the per-session brief-amendment budget (3) is spent — switch to a direct draft edit (session_id + draft), which re-gates the session with no agent call", err)
		}
		return nil, DraftEpicOutput{}, fmt.Errorf("edit refinement draft: %w", err)
	}
	return nil, DraftEpicOutput{Session: sess, SessionGuidance: guidanceForSession(sess)}, nil
}

func (r *runResolver) draftEpicDecide(ctx context.Context, sessionID uuid.UUID, decision, reason string) (*mcp.CallToolResult, DraftEpicOutput, error) {
	sess, err := r.api.DecideRefinementSession(ctx, sessionID, decision, reason)
	if err != nil {
		var ae *apiError
		if errors.As(err, &ae) && ae.Code == "decision_already_recorded" {
			// A revision carries at most one decision. Re-gate by editing (a new
			// revision resets the gate), never by deciding the same revision twice.
			return nil, DraftEpicOutput{}, fmt.Errorf("decide refinement session: %w: the latest revision already carries a decision — re-gate by EDITING (session_id + brief_amendment, or session_id + draft), which appends a new revision; do not decide the same revision twice", err)
		}
		return nil, DraftEpicOutput{}, fmt.Errorf("decide refinement session: %w", err)
	}
	return nil, DraftEpicOutput{Session: sess, SessionGuidance: guidanceForSession(sess)}, nil
}

func (r *runResolver) draftEpicFile(ctx context.Context, sessionID uuid.UUID, repo string) (*mcp.CallToolResult, DraftEpicOutput, error) {
	fr, err := r.api.FileRefinementSession(ctx, sessionID, repo)
	if err != nil {
		var ae *apiError
		if errors.As(err, &ae) {
			switch ae.Code {
			case "refinement_not_approved":
				return nil, DraftEpicOutput{}, fmt.Errorf("file refinement draft: %w: the draft is not approved — approve the latest revision first (session_id + decision=approved + reason), then re-invoke the file arm", err)
			case "refinement_draft_drifted":
				return nil, DraftEpicOutput{}, fmt.Errorf("file refinement draft: %w: the approved content drifted (an edit landed after approval) — re-decide the latest revision (session_id + decision + reason), then re-invoke the file arm", err)
			case "refinement_filing_failed":
				// Since the file arm detached (#4153) this is a SYNCHRONOUS refusal
				// to start (e.g. the target repo's GitHub App installation could
				// not be resolved): nothing was launched. A mid-sequence failure
				// is no longer an error here — it surfaces on the preview arm's
				// filing block. Filing is idempotent, so a re-invoke with the
				// pinned repo never re-files a recorded item. The backend's
				// details (the underlying error) ride the wrapped apiError.
				return nil, DraftEpicOutput{}, fmt.Errorf("file refinement draft: %w: the backend could not start the filing, so nothing was launched — fix the cause, then re-invoke the file arm with the SAME repo %q; filing is idempotent and never re-files a recorded item", err, repo)
			}
		}
		return nil, DraftEpicOutput{}, fmt.Errorf("file refinement draft: %w", err)
	}
	return nil, DraftEpicOutput{Filing: fr, SessionGuidance: guidanceForFiling(fr)}, nil
}

// The file response statuses and the session view's filing.state values
// (#4153), mirrored from backend/internal/server/refinement_file.go.
const (
	refinementFilingStatusInProgress        = "filing_in_progress"
	refinementFilingStatusAlreadyInProgress = "already_in_progress"

	refinementFilingStateInProgress = "in_progress"
	refinementFilingStateFailed     = "failed"
	refinementFilingStateIncomplete = "incomplete"
	refinementFilingStateFiled      = "filed"
)

// guidanceForSession maps a session's DERIVED state onto the legal next
// fishhawk_draft_epic arm(s). A filed session is terminal. drifted then takes
// precedence (it fail-closes back to awaiting_approval, so a stale approval
// must be re-decided, and the file arm would refuse it). Next the filing
// block (#4153): a filing in progress routes to the preview arm, a failed or
// incomplete one to the file arm with the PINNED repo. Every non-terminal
// state names at least one arm.
func guidanceForSession(sess *RefinementSession) []SessionGuidance {
	if sess == nil {
		return nil
	}
	if sess.State == refinementFilingStateFiled || (sess.Filing != nil && sess.Filing.State == refinementFilingStateFiled) {
		var args map[string]string
		if sess.Filing != nil {
			args = filedCoordinates(sess.Filing.Epic, sess.Filing.Children)
		}
		return []SessionGuidance{{
			State:     "filed",
			Arm:       "terminal",
			Arguments: args,
			Reason:    "the approved draft is filed — the session is complete",
		}}
	}
	if sess.Drifted {
		return []SessionGuidance{{
			State:     "drifted",
			Arm:       "decide",
			Arguments: map[string]string{"session_id": sess.SessionID, "decision": "approved|rejected", "reason": "why"},
			Reason:    "the latest revision's approval pins a content hash that no longer matches (an edit landed after approval); fail-closed to awaiting_approval — re-decide the latest revision",
		}}
	}
	if g := guidanceForFilingProgress(sess.SessionID, sess.Filing); g != nil {
		return g
	}
	switch sess.State {
	case "approved":
		return []SessionGuidance{{
			State:     "approved",
			Arm:       "file",
			Arguments: map[string]string{"session_id": sess.SessionID, "repo": "owner/name"},
			Reason:    "the latest revision is approved — file it into the tracker (the repo is pinned at first invoke)",
		}}
	case "rejected":
		return []SessionGuidance{{
			State:     "rejected",
			Arm:       "edit",
			Arguments: map[string]string{"session_id": sess.SessionID, "brief_amendment": "amendment (bounded to 3), OR draft: a direct EpicDraft edit"},
			Reason:    "the latest revision was rejected — re-draft via a brief_amendment (budget of 3) or a direct draft edit; a new revision re-gates the session to awaiting_approval",
		}}
	default: // awaiting_approval
		decideReason := "the latest revision is awaiting a verdict — approve or reject it"
		if flagged := flaggedCriteriaOrdinals(sess); len(flagged) > 0 {
			// The acceptance-criteria pre-check flagged one or more children with
			// an unjustified missing blocking criterion. The flag is ADVISORY —
			// approval remains legal — but name the offending ordinals so the
			// operator sees the defect before deciding.
			decideReason = fmt.Sprintf(
				"the latest revision is awaiting a verdict — approve or reject it. Advisory: the acceptance-criteria pre-check flagged %s (no_blocking_criterion); approval is still legal, or edit the child(ren) first",
				formatOrdinals(flagged),
			)
		}
		return []SessionGuidance{
			{
				State:     "awaiting_approval",
				Arm:       "decide",
				Arguments: map[string]string{"session_id": sess.SessionID, "decision": "approved|rejected", "reason": "why"},
				Reason:    decideReason,
			},
			{
				State:     "awaiting_approval",
				Arm:       "edit",
				Arguments: map[string]string{"session_id": sess.SessionID, "brief_amendment": "amendment, OR draft: a direct EpicDraft edit"},
				Reason:    "alternatively, edit the draft first (brief_amendment or draft); either appends a new revision and re-gates to awaiting_approval",
			},
		}
	}
}

// flaggedCriteriaOrdinals returns the 1-based ordinals of the children the
// acceptance-criteria pre-check flagged needs_attention (an unjustified
// no_blocking_criterion). Empty when the pre-check is clean or absent.
func flaggedCriteriaOrdinals(sess *RefinementSession) []int {
	var out []int
	for _, c := range sess.CriteriaPrecheck.Children {
		if c.NeedsAttention {
			out = append(out, c.Ordinal)
		}
	}
	return out
}

// formatOrdinals renders flagged child ordinals as human-readable prose, e.g.
// "child 2" or "children 1, 3".
func formatOrdinals(ordinals []int) string {
	if len(ordinals) == 1 {
		return fmt.Sprintf("child %d", ordinals[0])
	}
	parts := make([]string, len(ordinals))
	for i, o := range ordinals {
		parts[i] = strconv.Itoa(o)
	}
	return "children " + strings.Join(parts, ", ")
}

// guidanceForFilingProgress maps the session view's filing block onto the
// next arm (#4153): in_progress -> preview (the detached filing is running;
// re-invoking would only return already_in_progress), failed / incomplete ->
// file with the PINNED repo (a re-invoke launches a resume that never re-files
// a recorded item). nil when there is no filing block or its state needs no
// filing-specific move (filed is handled by the caller; an unknown state falls
// through to the approval-state guidance).
func guidanceForFilingProgress(sessionID string, f *RefinementFilingProgress) []SessionGuidance {
	if f == nil {
		return nil
	}
	repo := f.Repo
	if repo == "" {
		repo = "owner/name"
	}
	switch f.State {
	case refinementFilingStateInProgress:
		return []SessionGuidance{{
			State:     "filing_in_progress",
			Arm:       "preview",
			Arguments: map[string]string{"session_id": sessionID},
			Reason: fmt.Sprintf("the filing into %s is running in the background (%d of %d items recorded) — re-read the session with the preview arm until filing.state leaves in_progress; do not re-invoke the file arm while it runs",
				repo, f.FiledCount, f.ChildCount+1),
		}}
	case refinementFilingStateFailed:
		return []SessionGuidance{{
			State:     "filing_failed",
			Arm:       "file",
			Arguments: map[string]string{"session_id": sessionID, "repo": repo},
			Reason: fmt.Sprintf("the last filing stopped%s: %s — the %d recorded item(s) are durable; re-invoke the file arm with the SAME repo %s to resume (it never re-files a recorded item)",
				failurePoint(f), f.LastError, f.FiledCount, repo),
		}}
	case refinementFilingStateIncomplete:
		return []SessionGuidance{{
			State:     "filing_incomplete",
			Arm:       "file",
			Arguments: map[string]string{"session_id": sessionID, "repo": repo},
			Reason: fmt.Sprintf("a filing into %s is open with %d of %d items recorded but nothing is running (e.g. the backend restarted) — re-invoke the file arm with the SAME repo to resume (it never re-files a recorded item)",
				repo, f.FiledCount, f.ChildCount+1),
		}}
	}
	return nil
}

// failurePoint renders where a failed filing stopped (" at step link,
// ordinal 3"), or "" when the failure names neither.
func failurePoint(f *RefinementFilingProgress) string {
	var parts []string
	if f.Step != "" {
		parts = append(parts, "step "+f.Step)
	}
	if f.FailedOrdinal != nil {
		parts = append(parts, fmt.Sprintf("ordinal %d", *f.FailedOrdinal))
	}
	if len(parts) == 0 {
		return ""
	}
	return " at " + strings.Join(parts, ", ")
}

// filedCoordinates renders the filed epic + children as guidance arguments
// (epic, child_<ordinal>). nil when nothing is recorded.
func filedCoordinates(epic *RefinementFilingEpic, children []RefinementFilingChild) map[string]string {
	if epic == nil && len(children) == 0 {
		return nil
	}
	args := make(map[string]string, len(children)+1)
	if epic != nil {
		args["epic"] = fmt.Sprintf("#%d %s", epic.Number, epic.URL)
	}
	for _, c := range children {
		args[fmt.Sprintf("child_%d", c.Ordinal)] = fmt.Sprintf("#%d %s", c.Number, c.URL)
	}
	return args
}

// guidanceForFiling maps the file arm's result onto the next arm. A launch
// (filing_in_progress) or a concurrent call (already_in_progress) routes to
// the preview arm, where the detached filing's progress is observed (#4153).
// Anything else — a completed session's replay (status filed,
// already_completed), or a pre-#4153 backend's synchronous 200 that carries
// no status — is terminal: the session is complete.
func guidanceForFiling(fr *RefinementFilingResult) []SessionGuidance {
	if fr == nil {
		return nil
	}
	if fr.AlreadyInProgress || fr.Status == refinementFilingStatusInProgress || fr.Status == refinementFilingStatusAlreadyInProgress {
		reason := fmt.Sprintf("the filing into %s was launched in the background (budget %ds) — re-read the session with the preview arm and follow its filing block; do not re-invoke the file arm while it runs",
			fr.Repo, fr.BudgetSeconds)
		if fr.AlreadyInProgress || fr.Status == refinementFilingStatusAlreadyInProgress {
			reason = fmt.Sprintf("a filing into %s is already running; nothing new was launched — re-read the session with the preview arm and follow its filing block", fr.Repo)
		}
		return []SessionGuidance{{
			State:     "filing_in_progress",
			Arm:       "preview",
			Arguments: map[string]string{"session_id": fr.SessionID},
			Reason:    reason,
		}}
	}
	reason := "the approved draft was filed — the session is complete"
	if fr.AlreadyCompleted {
		reason = "the session was already filed; this replayed the recorded result (no writes) — the session is complete"
	} else if fr.Resumed {
		reason = "the approved draft finished filing (resumed from a partial invoke) — the session is complete"
	}
	return []SessionGuidance{{
		State:     "filed",
		Arm:       "terminal",
		Arguments: filedCoordinates(fr.Epic, fr.Children),
		Reason:    reason,
	}}
}
