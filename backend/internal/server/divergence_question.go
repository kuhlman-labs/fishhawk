package server

import (
	"context"
	"encoding/json"
	"log/slog"
	"strconv"
	"strings"

	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

// The divergence question (E75.5 / #3733, ADR-082 #3728 decision (d) and rule
// 5). gate_divergence.go records a precedent_divergence entry when a captain's
// decision at an allow-listed gate went against clear precedent; this file
// turns the newest UNANSWERED such entry into one optional question on the
// captain-facing reads — GET /v0/runs/{id} and the gate view — "a one-off, or
// a change of doctrine?". divergence_answer.go records the answer.
//
// It is DISPLAY-ONLY and OPTIONAL: no gate state, allowed action or transition
// reads it, and leaving it unanswered blocks nothing. Long-form contract:
// backend/internal/server/README.md § "Divergence at the gate".

// CategoryPrecedentDivergenceAnswered records the captain's answer to one
// divergence question. INTERNAL: chain-only, never its own issue-thread
// activity line.
const CategoryPrecedentDivergenceAnswered = "precedent_divergence_answered"

// The two answers a divergence question accepts — a CLOSED set.
const (
	divergenceAnswerOneOff         = "one_off"
	divergenceAnswerDoctrineChange = "doctrine_change"
)

// divergenceAnswerTool is the MCP tool the question's answer pointer names.
const divergenceAnswerTool = "fishhawk_answer_divergence"

// divergenceQuestionText is the fixed question prose.
const divergenceQuestionText = "This decision went against clear precedent. Was it a one-off, or a change of doctrine?"

// divergenceQuestion is the bounded question block on the single-run read and
// the gate view. It carries the divergence BY CITATION ONLY (source sequences,
// never reason prose — ADR-082 rule 1), the two answer options and a
// ready-to-issue answer pointer.
type divergenceQuestion struct {
	// Sequence is the chain sequence of the precedent_divergence entry — the
	// key the answer endpoint takes.
	Sequence       int64   `json:"sequence"`
	DecisionClass  string  `json:"decision_class"`
	StageID        string  `json:"stage_id"`
	StageKind      string  `json:"stage_kind,omitempty"`
	Outcome        string  `json:"outcome"`
	RejectClass    string  `json:"reject_class,omitempty"`
	ModalOutcome   string  `json:"modal_outcome"`
	AgreementRatio float64 `json:"agreement_ratio"`
	HumanCount     int     `json:"human_count"`
	// CitedSequences are the chain sequences of the prior decisions the
	// divergence cited, best first; CitedTotal is the untruncated count.
	CitedSequences []int64 `json:"cited_sequences"`
	CitedTotal     int     `json:"cited_total"`
	Question       string  `json:"question"`
	// Options is the closed answer set: one_off, doctrine_change.
	Options []string                `json:"options"`
	Answer  divergenceAnswerPointer `json:"answer"`
	// OpenTotal is how many divergence questions on this run are unanswered;
	// only the newest is rendered.
	OpenTotal int `json:"open_total"`
}

// divergenceAnswerPointer is where the question is answered.
type divergenceAnswerPointer struct {
	Endpoint string `json:"endpoint"`
	Tool     string `json:"tool"`
}

// precedentDivergenceAnsweredPayload is the precedent_divergence_answered
// audit payload. DivergenceSequence is what marks a question answered.
type precedentDivergenceAnsweredPayload struct {
	DivergenceSequence int64  `json:"divergence_sequence"`
	Answer             string `json:"answer"`
	Note               string `json:"note,omitempty"`
	DecisionClass      string `json:"decision_class"`
	StageID            string `json:"stage_id"`
	IssueNumber        int    `json:"issue_number,omitempty"`
	IssueURL           string `json:"issue_url,omitempty"`
	IssueProvider      string `json:"issue_provider,omitempty"`
}

// divergenceEndpoint is the ready-to-issue answer route for one question.
func divergenceEndpoint(runID string, sequence int64) string {
	return "POST /v0/runs/" + runID + "/divergence/" + strconv.FormatInt(sequence, 10) + "/answer"
}

// openDivergenceFor returns the newest unanswered divergence question on ru,
// or nil.
//
// BEST-EFFORT AND TOTAL. Each of these returns nil and makes NO read beyond
// the one that failed, leaving the response byte-identical to a pre-E75.5 one:
// a nil run, a nil or disabled DivergenceConfig (the shipped default — no read
// at all), a run-bound mcp:run: caller, a nil AuditRepo, a failed read of
// either category (warn-logged: a question is never built from a partial read,
// so an answered one can never reappear as open), and no unanswered entry. An
// entry whose payload does not decode is skipped.
func (s *Server) openDivergenceFor(ctx context.Context, ru *run.Run) *divergenceQuestion {
	if ru == nil {
		return nil
	}
	if cfg := s.cfg.DivergenceConfig; cfg == nil || !cfg.Enabled {
		return nil
	}
	// ADR-082 rule 6 at the API surface — the gatePrecedentFor guard. The
	// question cites OTHER runs' human decisions, and both carrying surfaces
	// authorize a run-bound token by the cross-run subject guard alone, so an
	// agent holding its run token gets no block.
	if strings.HasPrefix(IdentityFrom(ctx).Subject, "mcp:run:") {
		return nil
	}
	if s.cfg.AuditRepo == nil {
		return nil
	}
	warn := func(msg string, err error) {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn, "divergence question: "+msg+"; omitting the question",
			slog.String("run_id", ru.ID.String()), slog.String("error", err.Error()))
	}
	entries, err := s.cfg.AuditRepo.ListForRunByCategory(ctx, ru.ID, CategoryPrecedentDivergence)
	if err != nil {
		warn("list precedent_divergence failed", err)
		return nil
	}
	if len(entries) == 0 {
		return nil
	}
	answered, err := s.answeredDivergenceSequences(ctx, ru)
	if err != nil {
		warn("list precedent_divergence_answered failed", err)
		return nil
	}

	var (
		newest    *precedentDivergencePayload
		newestSeq int64
		open      int
	)
	for _, e := range entries {
		if e == nil || answered[e.Sequence] {
			continue
		}
		var p precedentDivergencePayload
		if json.Unmarshal(e.Payload, &p) != nil {
			continue
		}
		open++
		if newest == nil || e.Sequence > newestSeq {
			pc := p
			newest, newestSeq = &pc, e.Sequence
		}
	}
	if newest == nil {
		return nil
	}
	runID := ru.ID.String()
	q := &divergenceQuestion{
		Sequence:       newestSeq,
		DecisionClass:  newest.DecisionClass,
		StageID:        newest.StageID,
		StageKind:      newest.StageKind,
		Outcome:        newest.Outcome,
		RejectClass:    newest.RejectClass,
		ModalOutcome:   newest.ModalOutcome,
		AgreementRatio: newest.AgreementRatio,
		HumanCount:     newest.HumanCount,
		CitedSequences: make([]int64, 0, len(newest.Cited)),
		CitedTotal:     newest.CitedTotal,
		Question:       divergenceQuestionText,
		Options:        []string{divergenceAnswerOneOff, divergenceAnswerDoctrineChange},
		Answer: divergenceAnswerPointer{
			Endpoint: divergenceEndpoint(runID, newestSeq),
			Tool:     divergenceAnswerTool,
		},
		OpenTotal: open,
	}
	for _, c := range newest.Cited {
		q.CitedSequences = append(q.CitedSequences, c.SourceSequence)
	}
	return q
}

// answeredDivergenceSequences is the set of precedent_divergence sequences on
// ru that already carry an answer.
func (s *Server) answeredDivergenceSequences(ctx context.Context, ru *run.Run) (map[int64]bool, error) {
	entries, err := s.cfg.AuditRepo.ListForRunByCategory(ctx, ru.ID, CategoryPrecedentDivergenceAnswered)
	if err != nil {
		return nil, err
	}
	out := make(map[int64]bool, len(entries))
	for _, e := range entries {
		if e == nil {
			continue
		}
		var p precedentDivergenceAnsweredPayload
		if json.Unmarshal(e.Payload, &p) != nil || p.DivergenceSequence <= 0 {
			continue
		}
		out[p.DivergenceSequence] = true
	}
	return out, nil
}
