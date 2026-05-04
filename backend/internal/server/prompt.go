package server

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/githubclient"
	"github.com/kuhlman-labs/fishhawk/backend/internal/prompt"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
	"github.com/kuhlman-labs/fishhawk/backend/internal/signing"
)

// promptResponse is the 200 body for GET /v0/stages/{stage_id}/prompt.
// Wrapped in a JSON object so future fields (template version,
// hash, redaction notes) can be added without breaking the runner.
type promptResponse struct {
	StageID    string `json:"stage_id"`
	StageType  string `json:"stage_type"`
	Prompt     string `json:"prompt"`
	PromptHash string `json:"prompt_hash"`
}

// issueGetter is the slice of githubclient.Client the prompt
// handler consumes. Defining the interface in the server package
// lets tests substitute a stub without spinning up an httptest
// fake of api.github.com — *githubclient.Client satisfies it
// in production.
type issueGetter interface {
	GetIssue(ctx context.Context, installationID int64, repo githubclient.RepoRef, number int) (*githubclient.Issue, error)
}

// handleGetStagePrompt implements GET /v0/stages/{stage_id}/prompt.
//
// Auth is the same per-run signing-key signature used by the trace
// upload endpoint: the canonical message is sha256("prompt:" +
// stage_id), signed by the runner with the private half issued at
// signing-key time. Bound-to-stage scope keeps a leaked signature
// from being replayed against a different stage's prompt.
//
// Construction is server-side and pull-style (E3.12 design): the
// runner sees the constructed prompt rather than building it
// itself, so two replays of the same stage produce byte-identical
// prompts. Auditability of "what the agent was asked to do" is
// the load-bearing reason for that choice.
func (s *Server) handleGetStagePrompt(w http.ResponseWriter, r *http.Request) {
	github := s.issueGetter()
	if s.cfg.SigningRepo == nil || s.cfg.RunRepo == nil || github == nil {
		s.writeError(w, r, http.StatusServiceUnavailable, "prompt_unconfigured",
			"prompt construction requires signing, run, and GitHub repos to be configured", nil)
		return
	}

	stageID, err := uuid.Parse(r.PathValue("stage_id"))
	if err != nil {
		s.writeError(w, r, http.StatusBadRequest, "validation_failed",
			"stage_id must be a valid UUID",
			map[string]any{"field": "stage_id", "got": r.PathValue("stage_id")})
		return
	}

	stage, err := s.cfg.RunRepo.GetStage(r.Context(), stageID)
	if err != nil {
		if errors.Is(err, run.ErrNotFound) {
			s.writeError(w, r, http.StatusNotFound, "stage_not_found",
				"no stage with that id", map[string]any{"stage_id": stageID.String()})
			return
		}
		s.writeError(w, r, http.StatusInternalServerError, "internal_error",
			"get stage failed", map[string]any{"error": err.Error()})
		return
	}

	runRow, err := s.cfg.RunRepo.GetRun(r.Context(), stage.RunID)
	if err != nil {
		s.writeError(w, r, http.StatusInternalServerError, "internal_error",
			"get run for stage failed", map[string]any{"error": err.Error()})
		return
	}

	if !s.verifyPromptSignature(w, r, runRow.ID, stageID) {
		return
	}

	// Build the trigger context. For issue-style triggers we fetch
	// the issue from GitHub at request time so the prompt reflects
	// the latest title/body — the cost is one API call per stage
	// dispatch, which is acceptable. If the issue can't be fetched
	// (e.g., deleted, App lacks access) we still return a prompt
	// rather than failing — the agent will work without it, just
	// with less context.
	trigger := prompt.Trigger{
		Source: string(runRow.TriggerSource),
		Repo:   runRow.Repo,
	}
	if runRow.TriggerRef != nil {
		if number, ok := parseIssueRef(*runRow.TriggerRef); ok {
			trigger.IssueNumber = number
			s.fillIssueContext(r.Context(), github, runRow, number, &trigger)
		}
	}

	text, err := prompt.Build(string(stage.Type), trigger)
	if err != nil {
		if errors.Is(err, prompt.ErrUnsupportedStage) {
			s.writeError(w, r, http.StatusNotImplemented, "unsupported_stage_type",
				"prompt construction not yet implemented for this stage type",
				map[string]any{"stage_type": string(stage.Type)})
			return
		}
		s.writeError(w, r, http.StatusInternalServerError, "internal_error",
			"build prompt failed", map[string]any{"error": err.Error()})
		return
	}

	hash := signing.ComputeMessage([]byte(text))
	s.writeJSON(w, r, http.StatusOK, promptResponse{
		StageID:    stageID.String(),
		StageType:  string(stage.Type),
		Prompt:     text,
		PromptHash: hex.EncodeToString(hash),
	})
}

// verifyPromptSignature reads the X-Fishhawk-Signature header and
// validates it against sha256("prompt:" + stage_id) using the
// run's stored public key. Returns true on success; on failure
// writes the response and returns false so the caller short-circuits.
func (s *Server) verifyPromptSignature(w http.ResponseWriter, r *http.Request, runID, stageID uuid.UUID) bool {
	sigHeader := r.Header.Get("X-Fishhawk-Signature")
	if sigHeader == "" {
		s.writeError(w, r, http.StatusUnauthorized, "signature_missing",
			"X-Fishhawk-Signature header is required", nil)
		return false
	}
	signature, err := hex.DecodeString(sigHeader)
	if err != nil {
		s.writeError(w, r, http.StatusUnauthorized, "signature_invalid",
			"X-Fishhawk-Signature is not valid hex",
			map[string]any{"error": err.Error()})
		return false
	}

	message := promptCanonicalMessage(stageID)
	if err := s.cfg.SigningRepo.Verify(r.Context(), runID, message, signature); err != nil {
		switch {
		case errors.Is(err, signing.ErrNotFound):
			s.writeError(w, r, http.StatusNotFound, "signing_key_not_found",
				"no signing key issued for this run", map[string]any{"run_id": runID.String()})
		case errors.Is(err, signing.ErrExpired):
			s.writeError(w, r, http.StatusUnauthorized, "signing_key_expired",
				"signing key TTL has passed", map[string]any{"run_id": runID.String()})
		case errors.Is(err, signing.ErrSignatureInvalid):
			s.writeError(w, r, http.StatusUnauthorized, "signature_invalid",
				"signature does not match the run's stored public key", nil)
		default:
			s.writeError(w, r, http.StatusInternalServerError, "internal_error",
				"signature verification failed", map[string]any{"error": err.Error()})
		}
		return false
	}
	return true
}

// PromptCanonicalMessage exposes the canonical message the prompt
// endpoint signs over so the runner can derive the same bytes
// without re-implementing the format.
func PromptCanonicalMessage(stageID uuid.UUID) []byte {
	return promptCanonicalMessage(stageID)
}

func promptCanonicalMessage(stageID uuid.UUID) []byte {
	return signing.ComputeMessage([]byte("prompt:" + stageID.String()))
}

// fillIssueContext populates the trigger's IssueTitle and IssueBody
// by fetching from GitHub. Best-effort: failure to fetch logs and
// returns silently — the prompt will fall back to "no issue
// context provided" which the agent can handle.
func (s *Server) fillIssueContext(ctx context.Context, github issueGetter, runRow *run.Run, issueNumber int, trigger *prompt.Trigger) {
	if runRow.InstallationID == nil {
		return
	}
	repo, err := parseRepoOwnerName(runRow.Repo)
	if err != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn, "prompt: parse repo failed",
			slog.String("run_id", runRow.ID.String()),
			slog.String("repo", runRow.Repo),
			slog.String("error", err.Error()),
		)
		return
	}
	issue, err := github.GetIssue(ctx, *runRow.InstallationID, repo, issueNumber)
	if err != nil {
		s.cfg.Logger.LogAttrs(ctx, slog.LevelWarn, "prompt: get issue failed",
			slog.String("run_id", runRow.ID.String()),
			slog.Int("issue", issueNumber),
			slog.String("error", err.Error()),
		)
		return
	}
	trigger.IssueTitle = issue.Title
	trigger.IssueBody = issue.Body
}

// issueGetter returns the configured client cast to the small
// interface the handler needs. Returns nil when GitHub is unset.
// The promptIssueGetterOverride test seam takes precedence so
// handler tests don't need a real *githubclient.Client.
func (s *Server) issueGetter() issueGetter {
	if s.promptIssueGetterOverride != nil {
		return s.promptIssueGetterOverride
	}
	if s.cfg.GitHub == nil {
		return nil
	}
	return s.cfg.GitHub
}

// parseIssueRef extracts the issue number from a TriggerRef of the
// form "issue:<n>". Returns (n, true) on match; (0, false) otherwise.
func parseIssueRef(ref string) (int, bool) {
	const prefix = "issue:"
	if !strings.HasPrefix(ref, prefix) {
		return 0, false
	}
	n, err := strconv.Atoi(ref[len(prefix):])
	if err != nil || n <= 0 {
		return 0, false
	}
	return n, true
}

// parseRepoOwnerName splits "owner/name" into a RepoRef. Returns
// an error if the input doesn't contain exactly one slash with
// non-empty segments.
func parseRepoOwnerName(s string) (githubclient.RepoRef, error) {
	parts := strings.Split(s, "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return githubclient.RepoRef{}, fmt.Errorf("repo %q is not owner/name", s)
	}
	return githubclient.RepoRef{Owner: parts[0], Name: parts[1]}, nil
}
