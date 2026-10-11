package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/concurrency"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
	"github.com/kuhlman-labs/fishhawk/backend/internal/spec"
)

// Local stage concurrency groups (#3964 / ADR-087): the server half. The
// host-dispatch spawn marker (host_dispatch.go) asks Config.Concurrency for a
// slot before it moves a GROUPED host-dispatched stage to dispatched; a stage
// that cannot get one is QUEUED (409 concurrency_slot_queued, stage untouched
// at awaiting_host_dispatch). The stage reads project the slot state as the
// `concurrency` block. The slot store's contract (atomicity, FIFO, derived
// holders, tenancy) is backend/internal/concurrency/README.md.

// Audit categories (registered in audit/categories.go). INTERNAL and
// best-effort; deliberately NOT issue-thread activity categories.
const (
	// CategoryStageConcurrencyQueued is written once per queue episode, when
	// the marker queues a grouped stage (Admission.NewlyQueued).
	CategoryStageConcurrencyQueued = "stage_concurrency_queued"
	// CategoryStageConcurrencyAdmitted is written on every grouped admission.
	CategoryStageConcurrencyAdmitted = "stage_concurrency_admitted"
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

// resolveStageConcurrency returns the stage's concurrency group and limit, or
// grouped=false for an ungrouped stage. The spec stage is matched the same way
// resolveAgentTimeout matches it (id == stage type, else type). A declared
// `concurrency` without a group joins the host default group with its
// EffectiveLimit; a named group is repository-scoped. No declaration groups
// implement stages in the host default group at limit 1 and leaves every other
// stage type ungrouped (today's behaviour). An absent or unparseable spec
// falls back to that same default — the restrictive direction — and a parse
// failure logs a WARN.
//
// ADR-092 D3 (#4200): an implement stage carrying a LIVE merge-candidate
// verify trigger is a verify-only pass (no agent), so it resolves to the host
// verify group local-verify:<host> at DefaultVerifyLimit BEFORE the declared
// lookup — a spec-declared implement group or limit never captures a pass,
// and a pass never queues behind full implements. See
// isMergeCandidateVerifyPass for the fallback on an unreadable trigger.
func (s *Server) resolveStageConcurrency(ctx context.Context, runRow *run.Run, stage *run.Stage, host string) (group string, limit int, grouped bool) {
	if s.isMergeCandidateVerifyPass(ctx, stage) {
		return concurrency.VerifyGroupKey(host), concurrency.DefaultVerifyLimit, true
	}
	if decl, ok := s.declaredStageConcurrency(ctx, runRow, stage.Type); ok {
		if decl.Group == "" {
			return concurrency.DefaultGroupKey(host), decl.EffectiveLimit(), true
		}
		return concurrency.NamedGroupKey(runRow.Repo, decl.Group), decl.EffectiveLimit(), true
	}
	if stage.Type == run.StageTypeImplement {
		return concurrency.DefaultGroupKey(host), concurrency.DefaultLimit, true
	}
	return "", 0, false
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

// hostDispatchConcurrency is the 200 marker body's `concurrency` block for a
// grouped admission. AdmissionNonce echoes the request's nonce (omitted when
// it carried none).
type hostDispatchConcurrency struct {
	Group          string `json:"group"`
	Limit          int    `json:"limit"`
	QueuedBefore   bool   `json:"queued_before"`
	WaitedSeconds  int    `json:"waited_seconds"`
	AdmissionNonce string `json:"admission_nonce,omitempty"`
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
}

func toStageConcurrency(st concurrency.Status) *stageConcurrency {
	out := &stageConcurrency{
		Group:      st.GroupKey,
		Limit:      st.Limit,
		Holders:    toConcurrencyHolders(st.Holders),
		EnqueuedAt: st.EnqueuedAt,
	}
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
func (s *Server) writeConcurrencySlotQueued(w http.ResponseWriter, r *http.Request, stageID uuid.UUID, group string, limit int, adm concurrency.Admission) {
	w.Header().Set("Retry-After", strconv.Itoa(int(concurrency.WaiterPollInterval/time.Second)))
	s.writeError(w, r, http.StatusConflict, "concurrency_slot_queued",
		concurrencyQueuedMessage(group, limit, adm),
		map[string]any{
			"stage_id":              stageID.String(),
			"group":                 group,
			"limit":                 limit,
			"position":              adm.Position,
			"holders":               toConcurrencyHolders(adm.Holders),
			"enqueued_at":           adm.EnqueuedAt,
			"contended":             adm.Contended,
			"queue_ttl_seconds":     int(concurrency.QueueTTL / time.Second),
			"poll_interval_seconds": int(concurrency.WaiterPollInterval / time.Second),
		})
}

// concurrencyQueuedMessage renders the 409 message. An empty holders list
// (queued behind earlier waiters, or a contended lock) is named as such
// rather than rendered as an empty "held by".
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
	return fmt.Sprintf("stage queued for a concurrency slot in group %q (limit %d) at position %d: %s; re-POST the marker to keep the queue row live and to be admitted",
		group, limit, adm.Position, why)
}

// emitStageConcurrencyQueued / emitStageConcurrencyAdmitted append the two
// slot audit rows AFTER the admission transaction committed. Best-effort, the
// emitHostDispatchAcceptanceAnchor posture: a nil AuditRepo or a failed append
// logs a WARN and never changes the response.
func (s *Server) emitStageConcurrencyQueued(ctx context.Context, stage *run.Stage, group string, limit int, host string, adm concurrency.Admission) {
	s.emitStageConcurrency(ctx, stage, CategoryStageConcurrencyQueued, map[string]any{
		"stage_id":  stage.ID.String(),
		"group":     group,
		"limit":     limit,
		"position":  adm.Position,
		"holders":   toConcurrencyHolders(adm.Holders),
		"host":      host,
		"contended": adm.Contended,
	})
}

func (s *Server) emitStageConcurrencyAdmitted(ctx context.Context, stage *run.Stage, group string, limit int, adm concurrency.Admission) {
	s.emitStageConcurrency(ctx, stage, CategoryStageConcurrencyAdmitted, map[string]any{
		"stage_id":       stage.ID.String(),
		"group":          group,
		"limit":          limit,
		"queued_before":  adm.QueuedBefore,
		"waited_seconds": adm.WaitedSeconds,
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
func (s *Server) admitGroupedStage(w http.ResponseWriter, r *http.Request, stage *run.Stage, group string, limit int, host, nonce string) (updated *run.Stage, block *hostDispatchConcurrency, done bool, err error) {
	adm, err := s.cfg.Concurrency.Admit(r.Context(), concurrency.Request{
		StageID:        stage.ID,
		RunID:          stage.RunID,
		From:           stage.State,
		GroupKey:       group,
		Limit:          limit,
		Host:           host,
		AdmissionNonce: nonce,
	})
	if err != nil {
		return nil, nil, false, err
	}
	if !adm.Admitted {
		if adm.NewlyQueued {
			s.emitStageConcurrencyQueued(r.Context(), stage, group, limit, host, adm)
		}
		s.writeConcurrencySlotQueued(w, r, stage.ID, group, limit, adm)
		return nil, nil, true, nil
	}
	s.emitStageConcurrencyAdmitted(r.Context(), stage, group, limit, adm)
	return adm.Stage, &hostDispatchConcurrency{
		Group:          group,
		Limit:          limit,
		QueuedBefore:   adm.QueuedBefore,
		WaitedSeconds:  adm.WaitedSeconds,
		AdmissionNonce: nonce,
	}, false, nil
}
