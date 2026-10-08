package server

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/kuhlman-labs/fishhawk/backend/internal/alerttrigger"
	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/forge"
	"github.com/kuhlman-labs/fishhawk/backend/internal/webhook"
	"github.com/kuhlman-labs/fishhawk/backend/internal/workmgmt"
	workmgmtgithub "github.com/kuhlman-labs/fishhawk/backend/internal/workmgmt/github"
)

// maxAlertTriggerBodyBytes caps the POST /v0/triggers/alert body. The alert
// payload's largest field (description) is capped at 16 KiB, so 64 KiB is
// generous but bounded — the same cap POST /v0/work-items applies.
const maxAlertTriggerBodyBytes = 64 * 1024

// alertClaimStaleAfter is how long an unfiled dedup claim is honoured before
// another alert for the same fingerprint may reclaim it (a crashed filer).
// Measured on the database clock by alerttrigger.Store.Claim.
const alertClaimStaleAfter = 10 * time.Minute

// alertIncidentKeyNamespace is the workmgmt idempotency-key namespace stamped
// on every incident issue the ingress files, so a duplicate filed under a lost
// claim is named by its hidden marker.
const alertIncidentKeyNamespace = "alert-incident"

// Global-chain audit categories for ACCEPTED alerts (E35.4 / #1601). A
// rejection is never audited: an unauthenticated caller must not be able to
// append to the audit chain. Registered in backend/internal/audit/categories.go.
const (
	categoryAlertIncidentFiled      = "alert_incident_filed"
	categoryAlertIncidentOccurrence = "alert_incident_occurrence"
)

// Alert-trigger response actions (AlertTriggerResult.action).
const (
	alertActionFiled      = "filed"
	alertActionOccurrence = "occurrence"
	alertActionInFlight   = "in_flight"
)

// Auto-start outcomes (AlertAutoStart.outcome).
const (
	alertAutoStartDisabled       = "disabled"
	alertAutoStartStarted        = "started"
	alertAutoStartAlreadyStarted = "already_started"
	alertAutoStartRefused        = "refused"
	alertAutoStartError          = "error"
)

// alertTriggerResponse is the AlertTriggerResult body for an accepted alert.
type alertTriggerResponse struct {
	Action         string                  `json:"action"`
	IssueNumber    int                     `json:"issue_number,omitempty"`
	IssueURL       string                  `json:"issue_url,omitempty"`
	Occurrences    int                     `json:"occurrences"`
	DedupClaimLost bool                    `json:"dedup_claim_lost,omitempty"`
	AutoStart      *alertAutoStartResponse `json:"auto_start,omitempty"`
}

// alertAutoStartResponse is the AlertAutoStart body (filed only).
type alertAutoStartResponse struct {
	Enabled bool   `json:"enabled"`
	Outcome string `json:"outcome"`
	RunID   string `json:"run_id,omitempty"`
	Code    string `json:"code,omitempty"`
}

// handleAlertTrigger implements POST /v0/triggers/alert (E35.4 / #1601,
// ADR-053 option A): an HMAC-authenticated alert becomes a conventions-complete
// incident issue, deduplicated by fingerprint, with an optional per-source
// auto-start that ships OFF. The route takes NO bearer token; the per-source
// HMAC is its only authentication. The contract, including the response-code
// table and the Mark/Unmark rule, is backend/internal/alerttrigger/README.md.
//
// Order matters, and each step is a control:
//
//  1. configuration (503) and the body cap (413) — nothing read past the cap;
//  2. alerttrigger.Verify — signature, timestamp, constant-time MAC, then the
//     replay window (401, one code per refusal);
//  3. ONLY after a valid MAC, the nonce Mark keyed on the DECODED MAC, so an
//     unauthenticated caller can never write the delivery store and a
//     hex-case-flipped resend is still a duplicate;
//  4. from the Mark on, every branch that neither files the issue (201) nor
//     posts the occurrence comment (200) UNMARKS (the deferred release
//     below) — the 202 in_flight answer included — so an exact retry of a
//     transient failure is processed instead of refused alert_replayed.
func (s *Server) handleAlertTrigger(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if s.cfg.AlertSources.Len() == 0 {
		s.writeError(w, r, http.StatusServiceUnavailable, "alert_trigger_unconfigured",
			"the alert ingress is off on this instance: no alert sources are configured (set FISHHAWKD_ALERT_SOURCES_FILE)", nil)
		return
	}
	if s.cfg.AlertIncidents == nil || s.cfg.WebhookDeliveries == nil || s.cfg.GitHub == nil {
		s.writeError(w, r, http.StatusServiceUnavailable, "alert_store_unconfigured",
			"alert sources are configured but the ingress cannot serve: it needs the database (dedup ledger), the webhook delivery store (replay nonce) and the GitHub App (filing)",
			map[string]any{
				"incident_store": s.cfg.AlertIncidents != nil,
				"delivery_store": s.cfg.WebhookDeliveries != nil,
				"github":         s.cfg.GitHub != nil,
			})
		return
	}

	raw, err := io.ReadAll(io.LimitReader(r.Body, maxAlertTriggerBodyBytes+1))
	if err != nil {
		s.writeError(w, r, http.StatusBadRequest, "validation_failed",
			"could not read request body", map[string]any{"error": err.Error()})
		return
	}
	if len(raw) > maxAlertTriggerBodyBytes {
		s.writeError(w, r, http.StatusRequestEntityTooLarge, "body_too_large",
			"alert body exceeds size cap", map[string]any{"limit_bytes": maxAlertTriggerBodyBytes})
		return
	}

	sourceHeader := r.Header.Get(alerttrigger.HeaderSource)
	now := s.nowFunc()
	// Verify itself maps a non-positive window to DefaultReplayWindow.
	src, mac, verr := alerttrigger.Verify(s.cfg.AlertSources, sourceHeader,
		r.Header.Get(alerttrigger.HeaderTimestamp), r.Header.Get(alerttrigger.HeaderSignature),
		raw, now, s.cfg.AlertReplayWindow)
	if verr != nil {
		s.refuseAlert(w, r, sourceHeader, verr)
		return
	}

	nonce := "alert:" + src.ID + ":" + hex.EncodeToString(mac)
	if err := s.cfg.WebhookDeliveries.Mark(nonce); err != nil {
		if errors.Is(err, webhook.ErrDeliveryDuplicate) {
			s.logAlertRejection(ctx, src.ID, "alert_replayed")
			s.writeError(w, r, http.StatusUnauthorized, "alert_replayed",
				"this signed alert was already received; send a new alert with a fresh timestamp and signature",
				map[string]any{"reason": "duplicate"})
			return
		}
		s.writeError(w, r, http.StatusInternalServerError, "internal_error",
			"failed to record the alert nonce", map[string]any{"error": err.Error()})
		return
	}
	// keep flips true only once the alert was durably filed (201) or its
	// occurrence comment posted (200). Every other exit — an error branch,
	// the 202 in_flight answer, or a panic
	// unwinding this frame — releases the nonce, so a new post-Mark branch
	// fails toward re-processing a retry rather than silently dropping it.
	keep := false
	defer func() {
		if !keep {
			s.unmarkAlertNonce(ctx, src.ID, nonce)
		}
	}()

	alert, perr := alerttrigger.ParseAlert(raw)
	if perr != nil {
		s.writeError(w, r, http.StatusBadRequest, "validation_failed",
			"alert body is not a valid alert", map[string]any{"error": perr.Error()})
		return
	}

	conv, cerr := conventionsLoader(ctx, src.Repo)
	if cerr != nil {
		s.writeError(w, r, http.StatusInternalServerError, "internal_error",
			"could not load work-management conventions for the alert source's repository",
			map[string]any{"error": cerr.Error(), "repo": src.Repo})
		return
	}
	if conv.Provider != workmgmtgithub.ProviderName {
		s.writeError(w, r, http.StatusNotImplemented, "provider_unimplemented",
			fmt.Sprintf("the alert ingress files incidents only through the %s provider; repository %s resolves provider %q", workmgmtgithub.ProviderName, src.Repo, conv.Provider),
			map[string]any{"provider": conv.Provider, "repo": src.Repo})
		return
	}
	// ParseSources refuses a repo that is not owner/name at startup, and
	// Sources has no other constructor, so the split cannot fail here.
	owner, name, _ := splitRepoFullName(src.Repo)
	scope, rerr := s.resolveRepoScope(ctx, owner, name)
	if rerr != nil {
		s.writeError(w, r, http.StatusBadGateway, "work_item_filing_failed",
			"could not resolve the GitHub App installation for the alert source's repository",
			map[string]any{"error": rerr.Error(), "repo": src.Repo})
		return
	}
	target := workmgmt.Target{
		Repo:    workmgmt.Repo{Owner: owner, Name: name},
		Project: conv.Project,
		Scope:   scope,
	}

	key := alerttrigger.Key{SourceID: src.ID, Repo: src.Repo, Fingerprint: alert.Fingerprint}
	claim, clerr := s.cfg.AlertIncidents.Claim(ctx, key, alertClaimStaleAfter)
	if clerr != nil {
		s.writeError(w, r, http.StatusInternalServerError, "internal_error",
			"could not claim the alert incident in the dedup ledger", map[string]any{"error": clerr.Error()})
		return
	}

	switch claim.Kind {
	case alerttrigger.ClaimExisting:
		comment := alerttrigger.OccurrenceComment(alert, claim.Occurrences, now)
		if _, err := s.cfg.GitHub.CreateIssueComment(ctx, scope, forge.RepoRef{Owner: owner, Name: name}, claim.IssueNumber, comment); err != nil {
			s.writeError(w, r, http.StatusBadGateway, "alert_occurrence_failed",
				"could not post the occurrence comment on the existing incident issue",
				map[string]any{"error": err.Error(), "issue_number": claim.IssueNumber})
			return
		}
		keep = true
		s.auditAlertIncident(ctx, categoryAlertIncidentOccurrence, map[string]any{
			"source":       src.ID,
			"repo":         src.Repo,
			"fingerprint":  alert.Fingerprint,
			"issue_number": claim.IssueNumber,
			"occurrences":  claim.Occurrences,
		})
		s.writeJSON(w, r, http.StatusOK, alertTriggerResponse{
			Action:      alertActionOccurrence,
			IssueNumber: claim.IssueNumber,
			IssueURL:    claim.IssueURL,
			Occurrences: claim.Occurrences,
		})
		return

	case alerttrigger.ClaimInFlight:
		// Claim counted the occurrence, but nothing was filed or commented,
		// so the nonce is released (keep stays false): the sender's exact
		// retry lands once the competing claim resolves, as an occurrence
		// comment or — after a stale reclaim — the filing itself.
		s.writeJSON(w, r, http.StatusAccepted, alertTriggerResponse{
			Action:      alertActionInFlight,
			Occurrences: claim.Occurrences,
		})
		return

	case alerttrigger.ClaimNew:
		// Handled below.

	default:
		s.writeError(w, r, http.StatusInternalServerError, "internal_error",
			"dedup ledger answered an unknown claim kind", map[string]any{"claim_kind": string(claim.Kind)})
		return
	}

	issueKey := workmgmt.MintIdempotencyKey(alertIncidentKeyNamespace, src.ID, src.Repo, alert.Fingerprint)
	filing := workmgmt.FilingRequest{
		Type:           src.WorkItemType,
		Summary:        alert.Summary(),
		Sections:       alert.IncidentSections(src, now),
		Labels:         src.Labels,
		Relations:      workmgmt.Relations{ParentEpic: src.ParentEpic},
		IdempotencyKey: issueKey,
	}
	_, created, werr := s.applyAndFileWorkItem(ctx, filing, conv, target, owner, name)
	if werr != nil {
		if err := s.cfg.AlertIncidents.Release(ctx, key, claim.Token); err != nil {
			s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn, "alert trigger: could not release the dedup claim after a filing failure; the next alert reclaims it once stale",
				slog.String("source", src.ID), slog.String("repo", src.Repo), slog.String("error", err.Error()))
		}
		s.writeError(w, r, werr.status, werr.code, werr.msg, werr.details)
		return
	}
	keep = true

	resp := alertTriggerResponse{
		Action:      alertActionFiled,
		IssueNumber: created.Number,
		IssueURL:    created.URL,
		Occurrences: claim.Occurrences,
	}
	if err := s.cfg.AlertIncidents.Complete(ctx, key, claim.Token, created.Number, created.URL); err != nil {
		if errors.Is(err, alerttrigger.ErrClaimLost) {
			resp.DedupClaimLost = true
			s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn, "alert trigger: incident filed but the dedup claim was reclaimed meanwhile; a second issue for the fingerprint is possible",
				slog.String("source", src.ID), slog.String("repo", src.Repo), slog.Int("issue_number", created.Number))
		} else {
			s.cfg.Logger.LogAttrs(ctx, slog.LevelError, "alert trigger: incident filed but the dedup ledger could not record it; the claim is reclaimable once stale",
				slog.String("source", src.ID), slog.String("repo", src.Repo), slog.Int("issue_number", created.Number), slog.String("error", err.Error()))
		}
	}

	as := s.alertAutoStart(ctx, src, key, created.Number, issueKey)
	resp.AutoStart = &as

	auditPayload := map[string]any{
		"source":       src.ID,
		"repo":         src.Repo,
		"fingerprint":  alert.Fingerprint,
		"severity":     alert.Severity,
		"issue_number": created.Number,
		"issue_url":    created.URL,
		"auto_start":   as,
	}
	if resp.DedupClaimLost {
		auditPayload["dedup_claim_lost"] = true
	}
	s.auditAlertIncident(ctx, categoryAlertIncidentFiled, auditPayload)
	s.writeJSON(w, r, http.StatusCreated, resp)
}

// alertAutoStart runs the per-source auto-start for a newly filed incident
// and classifies it for the response and the audit row. It never fails the
// filing: every failure is an outcome. A source whose auto_start is false (the
// default) starts nothing.
func (s *Server) alertAutoStart(ctx context.Context, src alerttrigger.Source, key alerttrigger.Key, issueNumber int, issueKey string) alertAutoStartResponse {
	if !src.AutoStart {
		return alertAutoStartResponse{Enabled: false, Outcome: alertAutoStartDisabled}
	}
	res := alertAutoStartResponse{Enabled: true}
	if s.cfg.AlertSpecSource == nil {
		res.Outcome, res.Code = alertAutoStartError, "alert_spec_source_unconfigured"
		return res
	}
	spec, sha, err := s.cfg.AlertSpecSource.FetchSpec(ctx, src.Repo)
	if err != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn, "alert trigger: auto-start spec fetch failed",
			slog.String("source", src.ID), slog.String("repo", src.Repo), slog.String("error", err.Error()))
		res.Outcome, res.Code = alertAutoStartError, "spec_fetch_failed"
		return res
	}
	// The run's Idempotency-Key is derived from the issue and the incident's
	// idempotency digest, so a retried auto-start for the SAME filed issue
	// replays rather than minting a second run.
	digest := issueKey[strings.LastIndexByte(issueKey, ':')+1:]
	out, err := s.StartAlertRun(ctx, AlertRunParams{
		Repo:           src.Repo,
		WorkflowID:     src.WorkflowID,
		WorkflowSHA:    sha,
		WorkflowSpec:   spec,
		IssueNumber:    issueNumber,
		RunnerKind:     src.RunnerKind,
		IdempotencyKey: fmt.Sprintf("alert:%d:%s", issueNumber, digest),
	})
	if err != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn, "alert trigger: auto-start failed",
			slog.String("source", src.ID), slog.String("repo", src.Repo), slog.String("error", err.Error()))
		res.Outcome, res.Code = alertAutoStartError, "start_failed"
		return res
	}
	switch out.Kind {
	case AlertStartStarted, AlertStartAlreadyStarted:
		res.Outcome = alertAutoStartStarted
		if out.Kind == AlertStartAlreadyStarted {
			res.Outcome = alertAutoStartAlreadyStarted
		}
		res.RunID = out.RunID.String()
		if err := s.cfg.AlertIncidents.RecordRun(ctx, key, out.RunID); err != nil {
			s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn, "alert trigger: auto-started run not recorded on the incident ledger",
				slog.String("source", src.ID), slog.String("run_id", res.RunID), slog.String("error", err.Error()))
		}
	default:
		res.Outcome, res.Code = alertAutoStartRefused, out.Code
	}
	return res
}

// refuseAlert maps an alerttrigger.Verify error onto its 401 code. The source
// header is logged (capped) with the code; the body never is.
func (s *Server) refuseAlert(w http.ResponseWriter, r *http.Request, sourceHeader string, err error) {
	var (
		code    string
		msg     string
		details map[string]any
	)
	switch {
	case errors.Is(err, alerttrigger.ErrSignatureMissing):
		code, msg = "alert_signature_missing", err.Error()
	case errors.Is(err, alerttrigger.ErrTimestampInvalid):
		code, msg = "alert_timestamp_invalid", err.Error()
	case errors.Is(err, alerttrigger.ErrStale):
		code, msg = "alert_replayed", err.Error()
		details = map[string]any{"reason": "stale"}
	default:
		// ErrSignatureInvalid and anything unrecognised: the same answer as a
		// mis-signed request, so an unknown source is indistinguishable.
		code, msg = "alert_signature_invalid", alerttrigger.ErrSignatureInvalid.Error()
	}
	s.logAlertRejection(r.Context(), sourceHeader, code)
	s.writeError(w, r, http.StatusUnauthorized, code, msg, details)
}

// maxLoggedAlertSource caps the sender-supplied source header in a log line.
const maxLoggedAlertSource = 64

func (s *Server) logAlertRejection(ctx context.Context, sourceHeader, code string) {
	if len(sourceHeader) > maxLoggedAlertSource {
		sourceHeader = sourceHeader[:maxLoggedAlertSource]
	}
	s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn, "alert trigger rejected",
		slog.String("source", sourceHeader), slog.String("code", code))
}

// unmarkAlertNonce releases a recorded alert nonce so an exact retry is
// processed again. Best-effort: a failure is logged (the retry may then be
// refused alert_replayed) and never masks the original answer.
func (s *Server) unmarkAlertNonce(ctx context.Context, sourceID, nonce string) {
	if err := s.cfg.WebhookDeliveries.Unmark(nonce); err != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelError, "alert trigger: failed to unmark the alert nonce after a failure; an exact retry may be refused alert_replayed",
			slog.String("source", sourceID), slog.String("error", err.Error()))
	}
}

// auditAlertIncident appends a best-effort global-chain entry for an ACCEPTED
// alert under the attribution-only AlertRunSubject. A nil AuditRepo or an
// append failure is warn-logged: the incident is already filed or commented
// and the audit must not undo it.
func (s *Server) auditAlertIncident(ctx context.Context, category string, fields map[string]any) {
	if s.cfg.AuditRepo == nil {
		return
	}
	payload, err := json.Marshal(fields)
	if err != nil {
		s.cfg.Logger.Warn("encode alert incident audit payload failed", "category", category, "error", err.Error())
		return
	}
	kind := actorKindForSubject(AlertRunSubject)
	subject := AlertRunSubject
	if _, err := s.cfg.AuditRepo.AppendGlobalChained(ctx, audit.GlobalChainAppendParams{
		Timestamp:    time.Now().UTC(),
		Category:     category,
		ActorKind:    &kind,
		ActorSubject: &subject,
		Payload:      payload,
	}); err != nil {
		s.cfg.Logger.Warn("append alert incident audit entry failed", "category", category, "error", err.Error())
	}
}
