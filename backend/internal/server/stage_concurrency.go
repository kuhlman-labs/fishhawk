package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/campaign"
	"github.com/kuhlman-labs/fishhawk/backend/internal/concurrency"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
	"github.com/kuhlman-labs/fishhawk/backend/internal/spec"
)

// Local stage concurrency groups (#3964 / ADR-087): the server half. The
// host-dispatch spawn marker (host_dispatch.go) asks Config.Concurrency for a
// slot before it moves a GROUPED host-dispatched stage to dispatched; a stage
// that cannot get one is QUEUED (409 concurrency_slot_queued, stage untouched
// at awaiting_host_dispatch). The stage reads project the slot state as the
// `concurrency` block. An operator can promote a queued waiter to the front
// of its group (stage_concurrency_promote.go, #4191), and the marker
// auto-promotes an expedited campaign hotfix's implement waiter through the
// same core (campaign_dispatch_hold.go, #4234). The slot store's
// contract (atomicity, FIFO except a promoted waiter, derived holders,
// tenancy) is backend/internal/concurrency/README.md.

// Audit categories (registered in audit/categories.go). INTERNAL and
// best-effort; deliberately NOT issue-thread activity categories.
const (
	// CategoryStageConcurrencyQueued is written once per queue episode, when
	// the marker queues a grouped stage (Admission.NewlyQueued).
	CategoryStageConcurrencyQueued = "stage_concurrency_queued"
	// CategoryStageConcurrencyAdmitted is written on every grouped admission.
	CategoryStageConcurrencyAdmitted = "stage_concurrency_admitted"
	// CategoryStageConcurrencyPromoted is written when an operator promotes a
	// queued waiter (#4191, stage_concurrency_promote.go), or when the marker
	// auto-promotes an expedited campaign hotfix's implement waiter under the
	// system actor (#4234, campaign_dispatch_hold.go). Unlike the two
	// above it is NOT best-effort: it is appended inside the promotion
	// transaction, so a failed append rolls the promotion back.
	CategoryStageConcurrencyPromoted = "stage_concurrency_promoted"
)

// Stage-block status values.
const (
	stageConcurrencyStatusQueued  = "awaiting_concurrency_slot"
	stageConcurrencyStatusHolding = "holding"
)

// hostDispatchBodyMax bounds the optional {"host", "admission_nonce"} marker
// body; hostLabelMax is a DNS name's length bound; admissionNonceMax bounds
// the slot waiter's nonce (a UUID is 36).
const (
	hostDispatchBodyMax = 4 << 10
	hostLabelMax        = 253
	admissionNonceMax   = 64
)

// hostDispatchBody is the optional POST .../host-dispatch request body.
type hostDispatchBody struct {
	Host string `json:"host"`
	// AdmissionNonce is the MCP slot waiter's per-waiter nonce. An admission
	// records it on the held slot row and echoes it, so a waiter that lost
	// its admission response can tell its own admission from another
	// session's (concurrency README § "Admission nonce").
	AdmissionNonce string `json:"admission_nonce"`
}

// parseHostDispatchBody reads the optional {"host": string, "admission_nonce":
// string} body. An absent or empty body, or an empty host, yields
// concurrency.UnknownHost (a pre-change client sends no body); an absent
// nonce yields "". A malformed body, an unknown key, a host longer than
// hostLabelMax or one outside [A-Za-z0-9._-], or a nonce longer than
// admissionNonceMax or outside [A-Za-z0-9-] answers 400 validation_failed
// (ok=false) before any stage read, so the stage is untouched.
func (s *Server) parseHostDispatchBody(w http.ResponseWriter, r *http.Request) (host, nonce string, ok bool) {
	if r.Body == nil {
		return concurrency.UnknownHost, "", true
	}
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, hostDispatchBodyMax))
	if err != nil {
		s.writeError(w, r, http.StatusBadRequest, "validation_failed",
			fmt.Sprintf("host-dispatch body must be at most %d bytes", hostDispatchBodyMax),
			map[string]any{"field": "body"})
		return "", "", false
	}
	if strings.TrimSpace(string(raw)) == "" {
		return concurrency.UnknownHost, "", true
	}
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()
	var body hostDispatchBody
	if err := dec.Decode(&body); err != nil {
		s.writeError(w, r, http.StatusBadRequest, "validation_failed",
			`host-dispatch body must be a JSON object {"host": string, "admission_nonce": string}`,
			map[string]any{"field": "body", "error": err.Error()})
		return "", "", false
	}
	if body.AdmissionNonce != "" && !validAdmissionNonce(body.AdmissionNonce) {
		s.writeError(w, r, http.StatusBadRequest, "validation_failed",
			fmt.Sprintf("admission_nonce must be 1..%d characters from [A-Za-z0-9-]", admissionNonceMax),
			map[string]any{"field": "admission_nonce"})
		return "", "", false
	}
	if body.Host == "" {
		return concurrency.UnknownHost, body.AdmissionNonce, true
	}
	if !validHostLabel(body.Host) {
		s.writeError(w, r, http.StatusBadRequest, "validation_failed",
			fmt.Sprintf("host must be 1..%d characters from [A-Za-z0-9._-]", hostLabelMax),
			map[string]any{"field": "host"})
		return "", "", false
	}
	return body.Host, body.AdmissionNonce, true
}

func validAdmissionNonce(n string) bool {
	if len(n) == 0 || len(n) > admissionNonceMax {
		return false
	}
	for _, c := range n {
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '-':
		default:
			return false
		}
	}
	return true
}

func validHostLabel(h string) bool {
	if len(h) == 0 || len(h) > hostLabelMax {
		return false
	}
	for _, c := range h {
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '.', c == '_', c == '-':
		default:
			return false
		}
	}
	return true
}

// resolveStageConcurrency returns the stage's concurrency group SET (ADR-094
// D2), or grouped=false for an ungrouped stage. The spec stage is matched the
// same way resolveAgentTimeout matches it (id == stage type, else type). A
// declared `groups` list resolves through declaredGroupRequests. A declared
// `concurrency` without a group joins the host default group with its
// EffectiveLimit; a named group is repository-scoped. No declaration groups
// implement stages in the host default group at limit 1 and leaves every
// other stage type ungrouped (today's behaviour). An absent or unparseable
// spec falls back to that same default — the restrictive direction — and a
// parse failure logs a WARN. Every arm but the declared list yields exactly
// one group, with the key and limit it resolved to before #4240.
//
// ADR-092 D3 (#4200): an implement stage carrying a LIVE merge-candidate
// verify trigger is a verify-only pass (no agent), so it resolves to the host
// verify group local-verify:<host> at DefaultVerifyLimit BEFORE the declared
// lookup — a spec-declared implement group or limit never captures a pass,
// and a pass never queues behind full implements. See
// isMergeCandidateVerifyPass for the fallback on an unreadable trigger.
//
// A non-nil err (an entry with no canonical key, unreachable for a validated
// spec) fails closed: the marker answers 500 and admits nothing.
func (s *Server) resolveStageConcurrency(ctx context.Context, runRow *run.Run, stage *run.Stage, host string) (groups []concurrency.GroupRequest, grouped bool, err error) {
	one := func(key string, limit int) ([]concurrency.GroupRequest, bool, error) {
		return []concurrency.GroupRequest{{Key: key, Limit: limit}}, true, nil
	}
	if s.isMergeCandidateVerifyPass(ctx, stage) {
		return one(concurrency.VerifyGroupKey(host), concurrency.DefaultVerifyLimit)
	}
	if decl, ok := s.declaredStageConcurrency(ctx, runRow, stage.Type); ok {
		if len(decl.Groups) > 0 {
			groups, err := declaredGroupRequests(decl, runRow.Repo, host)
			if err != nil {
				return nil, false, err
			}
			return groups, true, nil
		}
		if decl.Group == "" {
			return one(concurrency.DefaultGroupKey(host), decl.EffectiveLimit())
		}
		return one(concurrency.NamedGroupKey(runRow.Repo, decl.Group), decl.EffectiveLimit())
	}
	if stage.Type == run.StageTypeImplement {
		return one(concurrency.DefaultGroupKey(host), concurrency.DefaultLimit)
	}
	return nil, false, nil
}

// hostScopedGroupLimit is the slot count of a NON-legacy host-scoped group
// (e.g. host:<h>:preview-target). A host-scoped group is shared by every
// repository on the host, so no repository's spec may size it (approval
// condition H1); host-declared capacity is ADR-094 slice 3's (#4241).
const hostScopedGroupLimit = 1

// declaredGroupRequests maps a declared `groups` list onto the store's group
// set. Each entry's key is concurrency.ScopedGroupKey with the host label as
// a host scope's id and repo as a repository scope's id, so {implement, host}
// IS the default group local-implement:<host> (ADR-094 Amendment A1).
//
// Limits: a repository entry takes its EffectiveLimit. A host entry NEVER
// reads its own Limit, unconditionally, so a stored spec (one captured under
// an older validator, or decoded around validation) can never size a group
// shared with other repositories: the entry canonicalising to
// local-implement:<host> takes DefaultLimit, local-verify:<host> takes
// DefaultVerifyLimit, and every other host entry takes hostScopedGroupLimit.
// A scope other than host or repository, or an identity ScopedGroupKey
// refuses, answers an error and no set.
func declaredGroupRequests(decl *spec.StageConcurrency, repo, host string) ([]concurrency.GroupRequest, error) {
	out := make([]concurrency.GroupRequest, 0, len(decl.Groups))
	for i, e := range decl.Groups {
		var scopeID string
		switch e.Scope {
		case spec.ConcurrencyScopeHost:
			scopeID = host
		case spec.ConcurrencyScopeRepository:
			scopeID = repo
		default:
			return nil, fmt.Errorf("concurrency.groups[%d]: scope %q is not declarable", i, e.Scope)
		}
		key, err := concurrency.ScopedGroupKey(concurrency.Scope(e.Scope), scopeID, e.Group)
		if err != nil {
			return nil, fmt.Errorf("concurrency.groups[%d]: %w", i, err)
		}
		limit := e.EffectiveLimit()
		if e.Scope == spec.ConcurrencyScopeHost {
			switch key {
			case concurrency.DefaultGroupKey(host):
				limit = concurrency.DefaultLimit
			case concurrency.VerifyGroupKey(host):
				limit = concurrency.DefaultVerifyLimit
			default:
				limit = hostScopedGroupLimit
			}
		}
		out = append(out, concurrency.GroupRequest{Key: key, Limit: limit})
	}
	return out, nil
}

// isMergeCandidateVerifyPass reports whether the runner fetching this stage's
// prompt will be served a merge-candidate verify-only pass: the prompt
// endpoint serves one exactly when an IMPLEMENT stage has a live (unconsumed)
// stage_merge_candidate_verify_triggered row. A fix-up or conflict-resolution
// re-open, and a trigger consumed by a later merge_candidate_verified row,
// carry no live trigger and stay in local-implement. With no audit repository,
// or when the trigger cannot be read (a read error, a malformed payload), the
// prompt endpoint serves NO pass and the runner runs an ordinary implement, so
// this answers false (WARN-logged) and the stage keeps an implement slot.
func (s *Server) isMergeCandidateVerifyPass(ctx context.Context, stage *run.Stage) bool {
	if stage.Type != run.StageTypeImplement || s.cfg.AuditRepo == nil {
		return false
	}
	live, err := s.liveMergeCandidateTrigger(ctx, stage.RunID, stage.ID)
	if err != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn,
			"host-dispatch: merge-candidate verify trigger unreadable; routing the stage as an ordinary implement",
			slog.String("run_id", stage.RunID.String()),
			slog.String("stage_id", stage.ID.String()),
			slog.String("error", err.Error()))
		return false
	}
	return live != nil
}

// declaredStageConcurrency returns the spec stage's declared concurrency
// block, ok=false when there is none (or the spec is absent/unparseable).
func (s *Server) declaredStageConcurrency(ctx context.Context, runRow *run.Run, stageType run.StageType) (*spec.StageConcurrency, bool) {
	if runRow.WorkflowSpec == nil {
		return nil, false
	}
	parsed, err := spec.ParseBytes(runRow.WorkflowSpec)
	if err != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn,
			"host-dispatch: parse workflow spec for concurrency failed; using the default group",
			slog.String("run_id", runRow.ID.String()),
			slog.String("error", err.Error()))
		return nil, false
	}
	wf, ok := parsed.Workflows[runRow.WorkflowID]
	if !ok {
		return nil, false
	}
	for _, match := range []func(spec.Stage) bool{
		func(st spec.Stage) bool { return st.ID == string(stageType) },
		func(st spec.Stage) bool { return string(st.Type) == string(stageType) },
	} {
		for _, st := range wf.Stages {
			if match(st) {
				return st.Concurrency, st.Concurrency != nil
			}
		}
	}
	return nil, false
}

// concurrencyHolder is one live holder on the wire.
type concurrencyHolder struct {
	RunID   uuid.UUID `json:"run_id"`
	StageID uuid.UUID `json:"stage_id"`
	Since   time.Time `json:"since"`
}

// toConcurrencyHolders projects holders, never nil: an empty list renders [].
func toConcurrencyHolders(hs []concurrency.Holder) []concurrencyHolder {
	out := make([]concurrencyHolder, 0, len(hs))
	for _, h := range hs {
		out = append(out, concurrencyHolder{RunID: h.RunID, StageID: h.StageID, Since: h.Since})
	}
	return out
}

// concurrencyGroupRef names one group of a set: the 200 marker block's and
// both slot audit rows' `groups` entries. Scope is omitted when the key does
// not parse (concurrency.ParseGroupKey).
type concurrencyGroupRef struct {
	Group string `json:"group"`
	Scope string `json:"scope,omitempty"`
	Limit int    `json:"limit"`
}

// concurrencyGroupState is one group of a set with its live state: the 409
// details' and the Stage block's `groups` entries (docs/api/v0.openapi.yaml
// ConcurrencyGroupState). Position is the 1-based queue position in this
// group while queued (omitted holding).
type concurrencyGroupState struct {
	Group    string              `json:"group"`
	Scope    string              `json:"scope,omitempty"`
	Limit    int                 `json:"limit"`
	Position int                 `json:"position,omitempty"`
	Holders  []concurrencyHolder `json:"holders"`
}

// groupScopeOf is the wire scope of key: "" when it does not parse.
func groupScopeOf(key string) string {
	g, ok := concurrency.ParseGroupKey(key)
	if !ok {
		return ""
	}
	return string(g.Scope)
}

// wireScope prefers the store's scope and falls back to parsing the key.
func wireScope(gs concurrency.GroupState) string {
	if gs.Scope != "" {
		return string(gs.Scope)
	}
	return groupScopeOf(gs.Key)
}

// admissionGroups is adm's per-group view, never empty: a store that left
// Groups empty (a fake, or a pre-#4240 store) is read as ONE group, the
// canonical-first requested group carrying the top-level Position and
// Holders (R7).
func admissionGroups(adm concurrency.Admission, req []concurrency.GroupRequest) []concurrency.GroupState {
	if len(adm.Groups) > 0 {
		return adm.Groups
	}
	g := concurrency.GroupState{Position: adm.Position, Holders: adm.Holders}
	if len(req) > 0 {
		first := req[0]
		for _, r := range req[1:] {
			if r.Key < first.Key {
				first = r
			}
		}
		g.Key, g.Limit = first.Key, first.Limit
	}
	return []concurrency.GroupState{g}
}

// bindingGroup picks the group the top-level wire fields describe (the
// concurrency.Admission rule): once admitted, the first group in canonical
// order; while queued, the highest position, ties broken in canonical order.
// groups is non-empty (admissionGroups).
func bindingGroup(groups []concurrency.GroupState, admitted bool) concurrency.GroupState {
	sorted := append([]concurrency.GroupState(nil), groups...)
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].Key < sorted[j].Key })
	b := sorted[0]
	if !admitted {
		for _, g := range sorted[1:] {
			if g.Position > b.Position {
				b = g
			}
		}
	}
	return b
}

func toConcurrencyGroupRefs(groups []concurrency.GroupState) []concurrencyGroupRef {
	out := make([]concurrencyGroupRef, 0, len(groups))
	for _, g := range groups {
		out = append(out, concurrencyGroupRef{Group: g.Key, Scope: wireScope(g), Limit: g.Limit})
	}
	return out
}

func toConcurrencyGroupStates(groups []concurrency.GroupState, queued bool) []concurrencyGroupState {
	out := make([]concurrencyGroupState, 0, len(groups))
	for _, g := range groups {
		e := concurrencyGroupState{Group: g.Key, Scope: wireScope(g), Limit: g.Limit, Holders: toConcurrencyHolders(g.Holders)}
		if queued {
			e.Position = g.Position
		}
		out = append(out, e)
	}
	return out
}

// hostDispatchConcurrency is the 200 marker body's `concurrency` block for a
// grouped admission. Group and Limit describe the BINDING group (the first in
// canonical order), retained for pre-#4240 clients; Groups lists the whole
// set. AdmissionNonce echoes the request's nonce (omitted when it carried
// none).
type hostDispatchConcurrency struct {
	Group          string                `json:"group"`
	Limit          int                   `json:"limit"`
	QueuedBefore   bool                  `json:"queued_before"`
	WaitedSeconds  int                   `json:"waited_seconds"`
	AdmissionNonce string                `json:"admission_nonce,omitempty"`
	Groups         []concurrencyGroupRef `json:"groups"`
}

// toHostDispatchConcurrency builds the 200 block for an admission of req.
func toHostDispatchConcurrency(adm concurrency.Admission, req []concurrency.GroupRequest, nonce string) *hostDispatchConcurrency {
	groups := admissionGroups(adm, req)
	b := bindingGroup(groups, true)
	return &hostDispatchConcurrency{
		Group:          b.Key,
		Limit:          b.Limit,
		QueuedBefore:   adm.QueuedBefore,
		WaitedSeconds:  adm.WaitedSeconds,
		AdmissionNonce: nonce,
		Groups:         toConcurrencyGroupRefs(groups),
	}
}

// stageConcurrency is the Stage `concurrency` block (docs/api/v0.openapi.yaml
// StageConcurrency): the stage's ACTIVE slot state. Absent for a stage with no
// active slot row (ungrouped, settled, parked, bypassed).
type stageConcurrency struct {
	// Status is awaiting_concurrency_slot (queued) or holding.
	Status string `json:"status"`
	Group  string `json:"group"`
	Limit  int    `json:"limit"`
	// Position is the 1-based queue position while queued (omitted holding).
	Position   int                 `json:"position,omitempty"`
	Holders    []concurrencyHolder `json:"holders"`
	EnqueuedAt time.Time           `json:"enqueued_at"`
	AcquiredAt *time.Time          `json:"acquired_at,omitempty"`
	// WaiterLive reports a queued row was refreshed within the queue TTL
	// (false while holding).
	WaiterLive bool `json:"waiter_live"`
	// Promoted reports an operator promoted this queued waiter in its current
	// queue episode (#4191); omitted when false and always while holding.
	Promoted bool `json:"promoted,omitempty"`
	// Host and HeldDispatchedAt describe the admission a holding row records
	// (the host label it was admitted for, and the dispatched_at of the
	// attempt it admitted). Informational: two sessions on one host share a
	// label. AdmissionNonce is the slot-waiter nonce the admitting request
	// carried (omitted when none), which is how a waiter that lost its
	// admission response tells its own admission from another session's
	// (approval condition 3).
	Host             string     `json:"host,omitempty"`
	HeldDispatchedAt *time.Time `json:"held_dispatched_at,omitempty"`
	AdmissionNonce   string     `json:"admission_nonce,omitempty"`
	// Groups lists every group of the stage's set (ADR-094 D2); the
	// top-level Group, Limit, Position and Holders describe the binding
	// group, retained for pre-#4240 clients.
	Groups []concurrencyGroupState `json:"groups"`
}

func toStageConcurrency(st concurrency.Status) *stageConcurrency {
	out := &stageConcurrency{
		Group:      st.GroupKey,
		Limit:      st.Limit,
		Holders:    toConcurrencyHolders(st.Holders),
		EnqueuedAt: st.EnqueuedAt,
	}
	groups := st.Groups
	if len(groups) == 0 {
		// R7: a store that left Groups empty reads as one group, the
		// top-level fields.
		groups = []concurrency.GroupState{{Key: st.GroupKey, Limit: st.Limit, Position: st.Position, Holders: st.Holders}}
	}
	out.Groups = toConcurrencyGroupStates(groups, st.State != concurrency.SlotHeld)
	if st.State == concurrency.SlotHeld {
		out.Status = stageConcurrencyStatusHolding
		out.AcquiredAt = st.AcquiredAt
		out.Host = st.Host
		out.HeldDispatchedAt = st.HeldDispatchedAt
		out.AdmissionNonce = st.AdmissionNonce
		return out
	}
	out.Status = stageConcurrencyStatusQueued
	out.Position = st.Position
	out.WaiterLive = st.WaiterLive
	out.Promoted = st.Promoted
	return out
}

// stageConcurrencyBlocks reads the active slot state of the listed stages in
// ONE batch. Best-effort: a nil store or a read error yields nil (every block
// absent), matching the progress / resolved_model read posture.
func (s *Server) stageConcurrencyBlocks(ctx context.Context, stageIDs []uuid.UUID) map[uuid.UUID]*stageConcurrency {
	if s.cfg.Concurrency == nil || len(stageIDs) == 0 {
		return nil
	}
	statuses, err := s.cfg.Concurrency.StatusForStages(ctx, stageIDs)
	if err != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn, "stage concurrency status read failed; block omitted",
			slog.String("error", err.Error()))
		return nil
	}
	out := make(map[uuid.UUID]*stageConcurrency, len(statuses))
	for id, st := range statuses {
		out[id] = toStageConcurrency(st)
	}
	return out
}

// writeConcurrencySlotQueued answers 409 concurrency_slot_queued with a
// Retry-After of the waiter poll interval. Queued is a 409, not a 2xx, so a
// pre-change client (which treats any 2xx as "proceed and spawn") fails
// closed.
func (s *Server) writeConcurrencySlotQueued(w http.ResponseWriter, r *http.Request, stageID uuid.UUID, req []concurrency.GroupRequest, adm concurrency.Admission) {
	w.Header().Set("Retry-After", strconv.Itoa(int(concurrency.WaiterPollInterval/time.Second)))
	b, details := concurrencyQueuedDetails(stageID, req, adm)
	adm.Position, adm.Holders = b.Position, b.Holders
	s.writeError(w, r, http.StatusConflict, "concurrency_slot_queued",
		concurrencyQueuedMessage(b.Key, b.Limit, adm), details)
}

// concurrencyQueuedDetails builds the 409 details: every pre-#4240 field,
// describing the binding group (highest position, ties in canonical order),
// plus `groups` listing the whole set. It returns the binding group too; the
// top-level position and holders are the binding group's, so they stay
// consistent with `group` after an auto-promote rewrote the set's positions.
func concurrencyQueuedDetails(stageID uuid.UUID, req []concurrency.GroupRequest, adm concurrency.Admission) (concurrency.GroupState, map[string]any) {
	groups := admissionGroups(adm, req)
	b := bindingGroup(groups, false)
	return b, map[string]any{
		"stage_id":              stageID.String(),
		"group":                 b.Key,
		"limit":                 b.Limit,
		"position":              b.Position,
		"holders":               toConcurrencyHolders(b.Holders),
		"enqueued_at":           adm.EnqueuedAt,
		"contended":             adm.Contended,
		"queue_ttl_seconds":     int(concurrency.QueueTTL / time.Second),
		"poll_interval_seconds": int(concurrency.WaiterPollInterval / time.Second),
		"groups":                toConcurrencyGroupStates(groups, true),
	}
}

// concurrencyQueuedMessage renders the 409 message. An empty holders list
// (queued behind earlier waiters, or a contended lock) is named as such
// rather than rendered as an empty "held by". A set of more than one group
// appends a "; group set: [...]" suffix naming each group's limit and
// position, so a single-group message is unchanged byte for byte.
func concurrencyQueuedMessage(group string, limit int, adm concurrency.Admission) string {
	var why string
	switch {
	case len(adm.Holders) > 0:
		parts := make([]string, 0, len(adm.Holders))
		for _, h := range adm.Holders {
			parts = append(parts, fmt.Sprintf("run %s stage %s", h.RunID, h.StageID))
		}
		why = "held by " + strings.Join(parts, ", ")
	case adm.Contended:
		why = "no live holders; another admission held the group lock"
	default:
		why = "no live holders; queued behind earlier waiters"
	}
	msg := fmt.Sprintf("stage queued for a concurrency slot in group %q (limit %d) at position %d: %s; re-POST the marker to keep the queue row live and to be admitted",
		group, limit, adm.Position, why)
	if len(adm.Groups) > 1 {
		parts := make([]string, 0, len(adm.Groups))
		for _, g := range adm.Groups {
			parts = append(parts, fmt.Sprintf("%q (limit %d) at position %d", g.Key, g.Limit, g.Position))
		}
		msg += "; group set: [" + strings.Join(parts, ", ") + "]"
	}
	return msg
}

// emitStageConcurrencyQueued / emitStageConcurrencyAdmitted append the two
// slot audit rows AFTER the admission transaction committed. Best-effort, the
// emitHostDispatchAcceptanceAnchor posture: a nil AuditRepo or a failed append
// logs a WARN and never changes the response.
func (s *Server) emitStageConcurrencyQueued(ctx context.Context, stage *run.Stage, req []concurrency.GroupRequest, host string, adm concurrency.Admission) {
	groups := admissionGroups(adm, req)
	b := bindingGroup(groups, false)
	s.emitStageConcurrency(ctx, stage, CategoryStageConcurrencyQueued, map[string]any{
		"stage_id":  stage.ID.String(),
		"group":     b.Key,
		"limit":     b.Limit,
		"position":  adm.Position,
		"holders":   toConcurrencyHolders(adm.Holders),
		"host":      host,
		"contended": adm.Contended,
		"groups":    toConcurrencyGroupRefs(groups),
	})
}

func (s *Server) emitStageConcurrencyAdmitted(ctx context.Context, stage *run.Stage, req []concurrency.GroupRequest, adm concurrency.Admission) {
	groups := admissionGroups(adm, req)
	b := bindingGroup(groups, true)
	s.emitStageConcurrency(ctx, stage, CategoryStageConcurrencyAdmitted, map[string]any{
		"stage_id":       stage.ID.String(),
		"group":          b.Key,
		"limit":          b.Limit,
		"queued_before":  adm.QueuedBefore,
		"waited_seconds": adm.WaitedSeconds,
		"groups":         toConcurrencyGroupRefs(groups),
	})
}

func (s *Server) emitStageConcurrency(ctx context.Context, stage *run.Stage, category string, payload map[string]any) {
	attrs := []slog.Attr{
		slog.String("run_id", stage.RunID.String()),
		slog.String("stage_id", stage.ID.String()),
		slog.String("category", category),
	}
	if s.cfg.AuditRepo == nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn, "host-dispatch: AuditRepo not configured; concurrency audit row skipped", attrs...)
		return
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn, "host-dispatch: marshal concurrency audit payload failed",
			append(attrs, slog.String("error", err.Error()))...)
		return
	}
	systemKind := audit.ActorSystem
	stageID := stage.ID
	if _, err := s.cfg.AuditRepo.AppendChained(ctx, audit.ChainAppendParams{
		RunID:     stage.RunID,
		StageID:   &stageID,
		Timestamp: time.Now().UTC(),
		Category:  category,
		ActorKind: &systemKind,
		Payload:   raw,
	}); err != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn, "host-dispatch: append concurrency audit row failed",
			append(attrs, slog.String("error", err.Error()))...)
	}
}

// admitGroupedStage runs the grouped admission for the marker's
// pending|awaiting_host_dispatch arm. done=true means the response was
// written (queued 409); otherwise updated/block carry an admission, or err
// carries the store error for the caller's existing drift reclassification.
//
// expedited is the campaign item guardCampaignDispatchHold resolved the stage
// to when that item is expedited (#4234), else nil. A QUEUED stage of an
// expedited item is auto-promoted to the head of its group
// (autoPromoteExpeditedWaiter, fail-open), and the 409 reports the
// post-promote position.
func (s *Server) admitGroupedStage(w http.ResponseWriter, r *http.Request, stage *run.Stage, groups []concurrency.GroupRequest, host, nonce string, expedited *campaign.Item) (updated *run.Stage, block *hostDispatchConcurrency, done bool, err error) {
	adm, err := s.cfg.Concurrency.Admit(r.Context(), concurrency.Request{
		StageID:        stage.ID,
		RunID:          stage.RunID,
		From:           stage.State,
		Groups:         groups,
		Host:           host,
		AdmissionNonce: nonce,
	})
	if err != nil {
		return nil, nil, false, err
	}
	if !adm.Admitted {
		if adm.NewlyQueued {
			s.emitStageConcurrencyQueued(r.Context(), stage, groups, host, adm)
		}
		if expedited != nil {
			adm = s.autoPromoteExpeditedWaiter(r.Context(), stage, expedited, adm)
		}
		s.writeConcurrencySlotQueued(w, r, stage.ID, groups, adm)
		return nil, nil, true, nil
	}
	s.emitStageConcurrencyAdmitted(r.Context(), stage, groups, adm)
	return adm.Stage, toHostDispatchConcurrency(adm, groups, nonce), false, nil
}
