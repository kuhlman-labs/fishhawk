package main

import (
	"context"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// ReleaseNotesInput is the fishhawk_release_notes tool's input schema (E33.5 /
// #1590, ADR-051). One tool covers the prepare/preview pair over the E33.2
// endpoints: mode selects between a read-only render (preview) and a render +
// persist (prepare). repo falls back to GITHUB_REPOSITORY; from/to are the ref
// range; stage_id is required only for mode=prepare (it keys the persisted
// release_notes artifact).
type ReleaseNotesInput struct {
	Mode    string `json:"mode,omitempty" jsonschema:"'preview' (default) renders the notes markdown WITHOUT persisting — read-only; 'prepare' renders AND persists a release_notes artifact keyed to stage_id"`
	Repo    string `json:"repo,omitempty" jsonschema:"target repo as owner/name; falls back to GITHUB_REPOSITORY env when omitted"`
	From    string `json:"from" jsonschema:"the start ref of the release range (e.g. the previous release tag)"`
	To      string `json:"to" jsonschema:"the end ref of the release range (e.g. HEAD or the release commit)"`
	StageID string `json:"stage_id,omitempty" jsonschema:"REQUIRED for mode=prepare: the stage UUID the persisted release_notes artifact is keyed to; ignored for mode=preview"`
}

// ReleaseNotesOutput surfaces the rendered notes and, for mode=prepare, the
// persisted artifact's id + content hash so the cut/publish verbs can name it.
type ReleaseNotesOutput struct {
	Mode        string `json:"mode" jsonschema:"the mode that ran: 'preview' or 'prepare'"`
	Markdown    string `json:"markdown" jsonschema:"the rendered release-notes markdown; carries the advisory semver bump hint (E33.4)"`
	ArtifactID  string `json:"artifact_id,omitempty" jsonschema:"the persisted release_notes artifact id; present only for mode=prepare"`
	StageID     string `json:"stage_id,omitempty" jsonschema:"the stage the artifact was keyed to; present only for mode=prepare"`
	Repo        string `json:"repo,omitempty"`
	From        string `json:"from,omitempty"`
	To          string `json:"to,omitempty"`
	ContentHash string `json:"content_hash,omitempty" jsonschema:"the persisted artifact content hash; present only for mode=prepare"`
}

// registerReleaseNotes wires the fishhawk_release_notes tool (E33.5 / #1590):
// the single operator MCP surface over the E33.2 release-notes endpoints. The
// cut and publish verbs live on the CLI (over /v0/releases/cut and
// /v0/releases/publish); next_actions names them at the release-loop states.
//
// Auth: preview is an authenticated read (401 anonymous); prepare is a
// persisting write and additionally needs write:runs (403 without it) — the
// backend enforces both.
func registerReleaseNotes(srv *mcp.Server, resolver *runResolver) {
	mcp.AddTool(srv, &mcp.Tool{
		Name: "fishhawk_release_notes",
		Description: strings.TrimSpace(`
Prepare or preview a release's notes for the delegating "release" workflow
(E33.5 / ADR-051). Use this when driving the release loop after
fishhawk_get_run_status reports a notes_ready or awaiting_cut next-action — it
is the operator surface over the E33.2 release-notes endpoints, as distinct
from the CLI cut/publish verbs (fishhawk release cut | publish) that record the
version decision and publish to the GitHub Release.

Modes:
  - "preview" (default) : render the notes markdown for the ref range WITHOUT
    persisting anything — read-only. Reach for this to review the changelog and
    the advisory semver bump hint before cutting.
  - "prepare"           : render AND persist a release_notes artifact keyed to
    stage_id. The persisted artifact is what the cut and publish verbs consume.

Inputs:
  - mode     : "preview" (default) or "prepare".
  - repo     : owner/name; falls back to GITHUB_REPOSITORY.
  - from, to : the release ref range (previous tag .. release ref).
  - stage_id : REQUIRED for mode=prepare — keys the persisted artifact.

Returns the rendered markdown (both modes) plus, for mode=prepare, the
persisted artifact_id + content_hash. The rendered notes carry the advisory
semver bump hint. Tool errors:
  - repo/from/to missing (caught before the HTTP hop)
  - stage_id missing or non-UUID for mode=prepare
  - mode not one of preview/prepare
  - validation_failed (400) / authentication_required (401) /
    insufficient_scope (403, prepare needs write:runs) / stage_not_found (404) /
    release_notes_unconfigured (503)
`),
	}, resolver.releaseNotes)
}

// releaseNotes is the tool handler. It resolves repo (falling back to
// GITHUB_REPOSITORY), validates the ref range, and dispatches on mode: preview
// reads the text/markdown render; prepare validates stage_id and persists.
func (r *runResolver) releaseNotes(ctx context.Context, _ *mcp.CallToolRequest, in ReleaseNotesInput) (*mcp.CallToolResult, ReleaseNotesOutput, error) {
	repo := strings.TrimSpace(in.Repo)
	if repo == "" {
		repo = strings.TrimSpace(r.getenv("GITHUB_REPOSITORY"))
	}
	if repo == "" {
		return nil, ReleaseNotesOutput{}, fmt.Errorf("repo is required: pass repo or set GITHUB_REPOSITORY")
	}
	from := strings.TrimSpace(in.From)
	to := strings.TrimSpace(in.To)
	if from == "" || to == "" {
		return nil, ReleaseNotesOutput{}, fmt.Errorf("from and to release-range refs are required")
	}

	mode := strings.TrimSpace(in.Mode)
	if mode == "" {
		mode = "preview"
	}
	switch mode {
	case "preview":
		md, err := r.api.PreviewReleaseNotes(ctx, repo, from, to)
		if err != nil {
			return nil, ReleaseNotesOutput{}, fmt.Errorf("preview release notes: %w", err)
		}
		return nil, ReleaseNotesOutput{Mode: "preview", Markdown: md, Repo: repo, From: from, To: to}, nil
	case "prepare":
		stageID := strings.TrimSpace(in.StageID)
		if stageID == "" {
			return nil, ReleaseNotesOutput{}, fmt.Errorf("stage_id is required for mode=prepare: it keys the persisted release_notes artifact")
		}
		if _, err := uuid.Parse(stageID); err != nil {
			return nil, ReleaseNotesOutput{}, fmt.Errorf("stage_id %q is not a valid UUID: %w", stageID, err)
		}
		res, err := r.api.PersistReleaseNotes(ctx, repo, from, to, stageID)
		if err != nil {
			return nil, ReleaseNotesOutput{}, fmt.Errorf("prepare release notes: %w", err)
		}
		return nil, ReleaseNotesOutput{
			Mode:        "prepare",
			Markdown:    res.Markdown,
			ArtifactID:  res.ArtifactID,
			StageID:     res.StageID,
			Repo:        res.Repo,
			From:        res.From,
			To:          res.To,
			ContentHash: res.ContentHash,
		}, nil
	default:
		return nil, ReleaseNotesOutput{}, fmt.Errorf("mode %q is not recognized: use 'preview' or 'prepare'", in.Mode)
	}
}

// releaseSignalsFor derives the release-loop signals (E33.5 / #1590) for a
// WorkflowID == "release" run. The cut/published signals are read off the
// recent-audit slice getRunStatus already fetched (the mergeObservedIn idiom);
// the deploy state comes from the stage rows already fetched. NotesPrepared
// needs a stage-artifact probe because the persist endpoint creates a
// release_notes ARTIFACT and emits no audit entry. Best-effort throughout: an
// artifact-read error leaves NotesPrepared false (the notes_ready arm, itself a
// legal prepare/poll move), so the release surface never fails the status
// snapshot. Display-only.
func (r *runResolver) releaseSignalsFor(ctx context.Context, stages []Stage, recent []AuditEntry) releaseSignals {
	sig := releaseSignals{IsRelease: true}
	if deploy := stageByType(stages, "deploy"); deploy != nil {
		sig.DeployState = deploy.State
	}
	for _, e := range recent {
		switch e.Category {
		case "release_cut":
			sig.Cut = true
		case "release_published":
			sig.Published = true
		}
	}
	sig.NotesPrepared = r.releaseNotesArtifactPresent(ctx, stages)
	return sig
}

// releaseNotesArtifactPresent reports whether any of the run's stages carries a
// persisted release_notes artifact (the prepare-verb signal). It scans the
// run's stages — a release run has only a deploy stage or two, so the per-stage
// artifact read is bounded, and the whole probe is cost-gated to release runs
// by the caller. An unparseable stage id or a per-stage read error is skipped
// (fail-open toward not-present, i.e. notes_ready).
func (r *runResolver) releaseNotesArtifactPresent(ctx context.Context, stages []Stage) bool {
	for i := range stages {
		stageID, err := uuid.Parse(stages[i].ID)
		if err != nil {
			continue
		}
		arts, err := r.api.ListStageArtifacts(ctx, stageID)
		if err != nil {
			continue
		}
		for j := range arts {
			if arts[j].Kind == "release_notes" {
				return true
			}
		}
	}
	return false
}
