package server

import (
	"context"
	"encoding/json"
	"log/slog"
	"sort"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/crewmessage"
	"github.com/kuhlman-labs/fishhawk/backend/internal/prompt"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

// Deferred crew delivery (E77.7 / #3741, ADR-081 #3727 D1 option 3): an OPEN
// `finding` or `notice` addressed to a crew role reaches the NEXT plan or
// review render that role reads, inside writeUntrustedCrewMessages' existing
// quarantine envelope — and never an implement render. Two layers keep it out
// of implement: crewRecipientRoleForStage derives no role for any stage type
// but plan and review, and buildImplement/buildImplementFixup never call
// writeUntrustedCrewMessages at all.

// CategoryCrewMessageDelivered records which crew messages a render handed to
// the reading stage's role. It is appended ONLY by the signed prompt handler
// and the in-process review paths — never by the prompt-render preview — and
// read back by resolveDeliverableCrewMessages' once-per-other-stage filter.
const CategoryCrewMessageDelivered = "crew_message_delivered"

// maxCrewDeliveriesPerPrompt bounds how many deferred findings/notices one
// render carries. With at most four consult messages folded in behind them,
// the combined block stays close to prompt.MaxTotalCrewMessageBytes, so the
// renderer's front-dropping cap (which would elide a RECORDED delivery without
// re-offering it) is reachable only with several near-limit messages. Raising
// it means revisiting that residual; TestCrewDelivery_CapSelectsNewest pins it.
const maxCrewDeliveriesPerPrompt = 3

// crewRecipientRoleForStage maps the READING stage's type onto the crew role
// whose deferred mail it receives. A POSITIVE allow-list, declared separately
// from crewSenderRoleForStage so each direction is independently auditable:
// implement, deploy, acceptance and any future stage type derive NO role and
// so receive nothing.
func crewRecipientRoleForStage(st run.StageType) (crewmessage.Role, bool) {
	switch st {
	case run.StageTypePlan:
		return crewmessage.RolePlanner, true
	case run.StageTypeReview:
		return crewmessage.RoleReviewer, true
	}
	return "", false
}

// crewDeliveryTypes is the closed set of message types deferred delivery
// carries: a consult is answered synchronously (E77.5) and an escalation goes
// to the captain (E77.6), so neither is ever delivered here.
var crewDeliveryTypes = map[crewmessage.MessageType]bool{
	crewmessage.TypeFinding: true,
	crewmessage.TypeNotice:  true,
}

// crewDelivery is one resolved delivery: the prompt-shaped messages, in
// ascending sent-sequence order, and the sent sequences they came from (the
// record recordCrewMessagesDelivered appends).
type crewDelivery struct {
	Messages  []prompt.CrewMessage
	Sequences []int64
}

// resolveDeliverableCrewMessages returns the OPEN `finding`/`notice` thread
// roots anchored on runID and addressed to the role readingStage executes,
// excluding any already recorded delivered to a stage OTHER than stageID (a
// retry or a fix-up re-review round of the SAME stage re-renders it). At most
// maxCrewDeliveriesPerPrompt are kept, by NEWEST sent sequence, returned
// ascending.
//
// Best-effort, the posture of resolveAnsweredCrewConsults: an unconfigured
// mailbox/audit repository or a list error logs WARN and returns an empty
// delivery, and one message whose document cannot be resolved is skipped,
// rather than failing the prompt build.
func (s *Server) resolveDeliverableCrewMessages(ctx context.Context, runID, stageID uuid.UUID, readingStage run.StageType) crewDelivery {
	role, ok := crewRecipientRoleForStage(readingStage)
	if !ok || s.cfg.CrewMailbox == nil || s.cfg.AuditRepo == nil {
		return crewDelivery{}
	}
	warn := func(msg string, err error, attrs ...slog.Attr) {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn, msg, append([]slog.Attr{
			slog.String("run_id", runID.String()), slog.String("stage_id", stageID.String()),
			slog.String("error", err.Error()),
		}, attrs...)...)
	}
	rows, err := s.cfg.CrewMailbox.Store().ListByAnchor(ctx, crewmessage.AnchorFilter{RunID: &runID})
	if err != nil {
		warn("prompt: list crew messages for delivery failed", err)
		return crewDelivery{}
	}
	delivered, err := s.cfg.AuditRepo.ListForRunByCategory(ctx, runID, CategoryCrewMessageDelivered)
	if err != nil {
		warn("prompt: list crew deliveries failed", err)
		return crewDelivery{}
	}
	elsewhere := make(map[int64]bool)
	for _, e := range delivered {
		if e.StageID != nil && *e.StageID == stageID {
			continue
		}
		var p crewMessageDeliveredPayload
		if json.Unmarshal(e.Payload, &p) != nil {
			continue
		}
		for _, seq := range p.SentSequences {
			elsewhere[seq] = true
		}
	}
	var candidates []crewmessage.Row
	for _, r := range rows {
		if !crewDeliveryTypes[r.MessageType] || r.RecipientRole != role || r.State != crewmessage.StateOpen ||
			r.ThreadRootSequence != r.SentSequence || elsewhere[r.SentSequence] {
			continue
		}
		candidates = append(candidates, r)
	}
	sort.Slice(candidates, func(i, j int) bool { return candidates[i].SentSequence > candidates[j].SentSequence })
	if len(candidates) > maxCrewDeliveriesPerPrompt {
		candidates = candidates[:maxCrewDeliveriesPerPrompt]
	}
	sort.Slice(candidates, func(i, j int) bool { return candidates[i].SentSequence < candidates[j].SentSequence })
	var out crewDelivery
	for _, r := range candidates {
		msg, err := s.crewSentDocument(ctx, r)
		if err != nil {
			warn("prompt: resolve crew delivery failed", err, slog.Int64("sent_sequence", r.SentSequence))
			continue
		}
		out.Messages = append(out.Messages, crewMessageForPrompt(msg))
		out.Sequences = append(out.Sequences, r.SentSequence)
	}
	return out
}

// crewMessageDeliveredPayload is the crew_message_delivered chain payload.
type crewMessageDeliveredPayload struct {
	SentSequences []int64 `json:"sent_sequences"`
	StageType     string  `json:"stage_type"`
	RecipientRole string  `json:"recipient_role"`
	Render        string  `json:"render"`
}

// recordCrewMessagesDelivered appends one crew_message_delivered entry naming
// the sent sequences render handed to the reading stage's role. Called ONLY
// after the render built successfully, from the signed prompt handler and the
// review paths — never from the prompt-render preview, which must stay a read.
//
// A no-op on an unconfigured audit repository or an empty delivery. An append
// failure logs WARN: the message is then offered again to the next stage,
// which is the safe direction for advisory text.
func (s *Server) recordCrewMessagesDelivered(ctx context.Context, runID, stageID uuid.UUID, readingStage run.StageType, render string, seqs []int64) {
	if s.cfg.AuditRepo == nil || len(seqs) == 0 {
		return
	}
	role, _ := crewRecipientRoleForStage(readingStage)
	payload, err := json.Marshal(crewMessageDeliveredPayload{
		SentSequences: seqs,
		StageType:     string(readingStage),
		RecipientRole: string(role),
		Render:        render,
	})
	if err != nil {
		return
	}
	systemKind := audit.ActorSystem
	if _, aerr := s.cfg.AuditRepo.AppendChained(ctx, audit.ChainAppendParams{
		RunID:     runID,
		StageID:   &stageID,
		Timestamp: time.Now().UTC(),
		Category:  CategoryCrewMessageDelivered,
		ActorKind: &systemKind,
		Payload:   payload,
	}); aerr != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn, "prompt: append crew_message_delivered failed",
			slog.String("run_id", runID.String()),
			slog.String("stage_id", stageID.String()),
			slog.String("error", aerr.Error()),
		)
	}
}
