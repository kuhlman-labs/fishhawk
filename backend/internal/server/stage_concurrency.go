package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"slices"
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

// hostDispatchBodyMax bounds the optional {"host", "admission_nonce",
// "capacity"} marker body; hostLabelMax is a DNS name's length bound;
// admissionNonceMax bounds the slot waiter's nonce (a UUID is 36).
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
	// Capacity is the host capacity the client declares for THIS request
	// (ADR-094 D4 as amended by A2, #4241): the limit of
	// host:<host>:capacity for this one Admit call, 1..concurrency.MaxLimit.
	// Absent or JSON null declares none, and the admission uses
	// concurrency.DefaultHostCapacity. Nothing is stored: the next request
	// sizes its own admission.
	Capacity *int `json:"capacity"`
}

// hostDispatchRequest is a parsed marker body.
type hostDispatchRequest struct {
	host  string
	nonce string
	// capacity is the declared host capacity, nil when the body declared
	// none.
	capacity *int
}

// capacityLimit is the host capacity group's limit for this request: the
// declared capacity, else concurrency.DefaultHostCapacity.
func (q hostDispatchRequest) capacityLimit() int {
	if q.capacity != nil {
		return *q.capacity
	}
	return concurrency.DefaultHostCapacity
}

// hostDispatchBodyShape names the body's keys in the decode-error message.
const hostDispatchBodyShape = `{"host": string, "admission_nonce": string, "capacity": integer}`

// parseHostDispatchBody reads the optional {"host": string, "admission_nonce":
// string, "capacity": integer} body. An absent or empty body, or an empty
// host, yields concurrency.UnknownHost (a pre-change client sends no body); an
// absent nonce yields ""; an absent or null capacity yields none. A malformed
// body, an unknown key, a host longer than hostLabelMax or one outside
// [A-Za-z0-9._-], a nonce longer than admissionNonceMax or outside
// [A-Za-z0-9-], or a capacity that is not an integer in
// 1..concurrency.MaxLimit answers 400 validation_failed (ok=false) before any
// stage read, so the stage is untouched and no slot row is written. A
// non-integer capacity (a string, a fraction, a boolean) is named as
// details.field=capacity, not body.
func (s *Server) parseHostDispatchBody(w http.ResponseWriter, r *http.Request) (req hostDispatchRequest, ok bool) {
	none := hostDispatchRequest{host: concurrency.UnknownHost}
	if r.Body == nil {
		return none, true
	}
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, hostDispatchBodyMax))
	if err != nil {
		s.writeError(w, r, http.StatusBadRequest, "validation_failed",
			fmt.Sprintf("host-dispatch body must be at most %d bytes", hostDispatchBodyMax),
			map[string]any{"field": "body"})
		return hostDispatchRequest{}, false
	}
	if strings.TrimSpace(string(raw)) == "" {
		return none, true
	}
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()
	var body hostDispatchBody
	if err := dec.Decode(&body); err != nil {
		var ute *json.UnmarshalTypeError
		if errors.As(err, &ute) && ute.Field == "capacity" {
			s.writeError(w, r, http.StatusBadRequest, "validation_failed",
				hostCapacityRangeMessage(),
				map[string]any{"field": "capacity", "error": err.Error()})
			return hostDispatchRequest{}, false
		}
		s.writeError(w, r, http.StatusBadRequest, "validation_failed",
			"host-dispatch body must be a JSON object "+hostDispatchBodyShape,
			map[string]any{"field": "body", "error": err.Error()})
		return hostDispatchRequest{}, false
	}
	if body.AdmissionNonce != "" && !validAdmissionNonce(body.AdmissionNonce) {
		s.writeError(w, r, http.StatusBadRequest, "validation_failed",
			fmt.Sprintf("admission_nonce must be 1..%d characters from [A-Za-z0-9-]", admissionNonceMax),
			map[string]any{"field": "admission_nonce"})
		return hostDispatchRequest{}, false
	}
	if body.Capacity != nil && (*body.Capacity < 1 || *body.Capacity > concurrency.MaxLimit) {
		s.writeError(w, r, http.StatusBadRequest, "validation_failed",
			hostCapacityRangeMessage(),
			map[string]any{"field": "capacity"})
		return hostDispatchRequest{}, false
	}
	req = hostDispatchRequest{host: body.Host, nonce: body.AdmissionNonce, capacity: body.Capacity}
	if body.Host == "" {
		req.host = concurrency.UnknownHost
		return req, true
	}
	if !validHostLabel(body.Host) {
		s.writeError(w, r, http.StatusBadRequest, "validation_failed",
			fmt.Sprintf("host must be 1..%d characters from [A-Za-z0-9._-]", hostLabelMax),
			map[string]any{"field": "host"})
		return hostDispatchRequest{}, false
	}
	return req, true
}

// hostCapacityRangeMessage is the 400 message for a capacity outside the
// accepted range or of the wrong JSON type.
func hostCapacityRangeMessage() string {
	return fmt.Sprintf("capacity must be an integer in 1..%d (the host capacity for this request; omit it for the default %d)",
		concurrency.MaxLimit, concurrency.DefaultHostCapacity)
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
// D2, D4, D5 as amended by A2, #4241). Every stage that reaches the marker's
// admission arm is grouped: the set is the stage type's DEFAULT set, plus
// every declared group (add-only, deduplicated by key; a declared entry's
// limit wins on a shared key), plus one slot of the host capacity group
// host:<host>:capacity at capacityLimit (the request's declared capacity,
// else concurrency.DefaultHostCapacity; nothing is stored, so the limit sizes
// THIS admission only). Default sets, before the capacity group:
//
//   - implement: local-implement:<host> at DefaultLimit.
//   - a merge-candidate verify pass: local-verify:<host> at
//     DefaultVerifyLimit, and nothing declared (below).
//   - plan, acceptance, an agent review and every other agent stage type:
//     nothing, so the set is the capacity group alone.
//
// Declaration forms, each ADDED to the default set (ADR-094 Q5 supersedes
// ADR-087's replace rule, and A2 supersedes A1 decision 5):
//
//   - {limit: N} declares local-implement:<host> at N: it resizes the
//     implement default on an implement stage, and adds that group elsewhere.
//   - {group: X, limit: N} adds the repository group spec:<repo>:X at N.
//   - a `groups` list adds each entry through declaredGroupRequests.
//
// The spec stage is matched the same way resolveAgentTimeout matches it
// (id == stage type, else type). An absent or unparseable spec declares
// nothing, so the stage keeps its default set (a parse failure logs a WARN).
//
// ADR-092 D3 (#4200): an implement stage carrying a LIVE merge-candidate
// verify trigger is a verify-only pass (no agent), so it resolves to
// {capacity, local-verify:<host>} BEFORE the declared lookup — a
// spec-declared implement group or limit never captures a pass, and a pass
// never queues behind full implements in local-implement:<host>. This
// deliberately keeps ADR-092 D3's host verify group instead of D5's per-base
// verify group (ADR-094 A2). No declared stage shares the pass's group:
// declaredGroupRequests refuses an entry for local-verify:<host>. See
// isMergeCandidateVerifyPass for the fallback on an unreadable trigger.
//
// A non-nil err (an entry with no canonical key, or one for a reserved host
// group; both unreachable for a validated spec) fails closed: the marker
// answers 500 and admits nothing.
func (s *Server) resolveStageConcurrency(ctx context.Context, runRow *run.Run, stage *run.Stage, host string, capacityLimit int) ([]concurrency.GroupRequest, error) {
	if s.isMergeCandidateVerifyPass(ctx, stage) {
		return withHostCapacity([]concurrency.GroupRequest{{Key: concurrency.VerifyGroupKey(host), Limit: concurrency.DefaultVerifyLimit}}, host, capacityLimit), nil
	}
	var set []concurrency.GroupRequest
	if stage.Type == run.StageTypeImplement {
		set = []concurrency.GroupRequest{{Key: concurrency.DefaultGroupKey(host), Limit: concurrency.DefaultLimit}}
	}
	if decl, ok := s.declaredStageConcurrency(ctx, runRow, stage.Type); ok {
		declared, err := declaredStageGroups(decl, runRow.Repo, host)
		if err != nil {
			return nil, err
		}
		set = unionGroupRequests(set, declared)
	}
	return withHostCapacity(set, host, capacityLimit), nil
}

// declaredStageGroups maps one declaration form onto its group requests: the
// `groups` list through declaredGroupRequests, the named shorthand onto its
// repository group, and the bare {limit: N} onto the host implement group.
func declaredStageGroups(decl *spec.StageConcurrency, repo, host string) ([]concurrency.GroupRequest, error) {
	switch {
	case len(decl.Groups) > 0:
		return declaredGroupRequests(decl, repo, host)
	case decl.Group == "":
		return []concurrency.GroupRequest{{Key: concurrency.DefaultGroupKey(host), Limit: decl.EffectiveLimit()}}, nil
	default:
		return []concurrency.GroupRequest{{Key: concurrency.NamedGroupKey(repo, decl.Group), Limit: decl.EffectiveLimit()}}, nil
	}
}

// unionGroupRequests returns base plus every add entry, deduplicated by key:
// an add entry whose key base already holds replaces that entry's limit in
// place, so {limit: N} resizes an implement stage's default group.
func unionGroupRequests(base, add []concurrency.GroupRequest) []concurrency.GroupRequest {
	out := append([]concurrency.GroupRequest(nil), base...)
	for _, g := range add {
		i := slices.IndexFunc(out, func(o concurrency.GroupRequest) bool { return o.Key == g.Key })
		if i >= 0 {
			out[i].Limit = g.Limit
			continue
		}
		out = append(out, g)
	}
	return out
}

// withHostCapacity adds the host capacity group host:<host>:capacity at limit
// to set (ADR-094 D4): every stage that reaches the marker's admission arm
// holds one capacity slot. The capacity group is exempt from the store's FIFO
// rule (b) and admits on ADR-087's reservation formula alone: holders plus
// earlier live waiters below the limit, so each earlier waiter reserves one
// slot even while it is blocked only on another group. At capacity 3, two
// holding implements and one implement queued on local-implement:<host> fill
// capacity, and a later plan, acceptance or verify pass queues; at capacity 4
// one slot stays free (concurrency README § "Host capacity group").
func withHostCapacity(set []concurrency.GroupRequest, host string, limit int) []concurrency.GroupRequest {
	return unionGroupRequests(set, []concurrency.GroupRequest{{Key: concurrency.CapacityGroupKey(host), Limit: limit}})
}

// hostScopedGroupLimit is the slot count of a NON-legacy, non-capacity
// host-scoped group (e.g. the built-in host:<h>:preview-target). A
// host-scoped group is shared by every repository on the host, so no
// repository's spec may size it (approval condition H1); the host operator
// sizes only the capacity group, through FISHHAWK_HOST_CAPACITY.
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
// local-implement:<host> takes DefaultLimit, and every other host entry
// (preview-target included) takes hostScopedGroupLimit. A scope other than
// host or repository, or an identity ScopedGroupKey refuses, answers an error
// and no set.
//
// Two host groups are reserved, and an entry for either answers an error and
// no set. An entry whose CANONICAL identity, its scope and name trimmed and
// lowercased, is the host capacity group concurrency.CapacityGroupKey(host):
// the server adds that group to every set at the request's capacity, so no
// spec may declare or size it (ADR-094 A2). The fold is defence in depth for
// a declaration built in Go: the schema pattern already refuses a case or
// whitespace variant at parse time. And an entry canonicalising to
// concurrency.VerifyGroupKey(host): local-verify:<host> is reserved for
// server-decided merge-candidate verify passes and specialist jobs (ADR-094
// A1 decision 6). Both validators refuse both entries, and spec.ParseBytes
// runs the backend's, so a captured spec carrying one takes the
// unparseable-spec fallback; these refusals keep a declaration that skipped
// validation out of those groups too.
func declaredGroupRequests(decl *spec.StageConcurrency, repo, host string) ([]concurrency.GroupRequest, error) {
	out := make([]concurrency.GroupRequest, 0, len(decl.Groups))
	for i, e := range decl.Groups {
		if isHostCapacityEntry(e, host) {
			return nil, fmt.Errorf("concurrency.groups[%d]: %s is reserved: the host capacity group is added to every host-dispatched stage's set and sized by the host operator", i, concurrency.CapacityGroupKey(host))
		}
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
		if key == concurrency.VerifyGroupKey(host) {
			return nil, fmt.Errorf("concurrency.groups[%d]: %s is reserved for merge-candidate verify passes and specialist jobs", i, key)
		}
		limit := e.EffectiveLimit()
		if e.Scope == spec.ConcurrencyScopeHost {
			switch key {
			case concurrency.DefaultGroupKey(host):
				limit = concurrency.DefaultLimit
			default:
				limit = hostScopedGroupLimit
			}
		}
		out = append(out, concurrency.GroupRequest{Key: key, Limit: limit})
	}
	return out, nil
}

// isHostCapacityEntry reports whether e's canonical identity — its scope and
// group name trimmed and lowercased — is the host capacity group of host.
func isHostCapacityEntry(e spec.StageConcurrencyGroup, host string) bool {
	scope := strings.ToLower(strings.TrimSpace(e.Scope))
	name := strings.ToLower(strings.TrimSpace(e.Group))
	key, err := concurrency.ScopedGroupKey(concurrency.Scope(scope), host, name)
	return err == nil && key == concurrency.CapacityGroupKey(host)
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
// requested set's binding group (the first non-capacity group in canonical
// order, concurrency.BindingIndex) carrying the top-level Position and
// Holders (R7).
func admissionGroups(adm concurrency.Admission, req []concurrency.GroupRequest) []concurrency.GroupState {
	if len(adm.Groups) > 0 {
		return adm.Groups
	}
	g := concurrency.GroupState{Position: adm.Position, Holders: adm.Holders}
	if len(req) > 0 {
		states := make([]concurrency.GroupState, len(req))
		for i, r := range req {
			states[i] = concurrency.GroupState{Key: r.Key, Limit: r.Limit}
		}
		first := states[concurrency.BindingIndex(states, false)]
		g.Key, g.Limit = first.Key, first.Limit
	}
	return []concurrency.GroupState{g}
}

// bindingGroup picks the group the top-level wire fields describe, by the
// store's own rule (concurrency.BindingIndex): once admitted, the first
// non-capacity group in canonical order; while queued, the highest position,
// ties preferring a non-capacity group, then canonical order. So the host
// capacity group binds only when it alone is strictly highest while queued,
// or is the set's only group, and a pre-#4240 client keeps reading the
// stage-type group. groups is non-empty (admissionGroups).
func bindingGroup(groups []concurrency.GroupState, admitted bool) concurrency.GroupState {
	return groups[concurrency.BindingIndex(groups, !admitted)]
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
// grouped admission. Group and Limit describe the BINDING group (bindingGroup:
// the first non-capacity group in canonical order), retained for pre-#4240
// clients; Groups lists the whole
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
// active slot row (never marked, settled, parked, bypassed).
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
// describing the binding group (bindingGroup: highest position, ties
// preferring a non-capacity group, then canonical order),
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
