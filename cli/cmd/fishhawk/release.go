package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"strings"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/cli/internal/httpclient"
)

// runRelease dispatches `fishhawk release <subcommand>`. It mirrors runDeploy's
// shape and drives the E33.5 operator release surface (ADR-051) from the
// terminal: `preview` renders the release notes for a ref range WITHOUT
// persisting (reading the backend's text/markdown preview body), `prepare`
// persists them as a release_notes artifact, `cut` records the operator's
// ratified version decision as a release_cut audit entry (the decision only —
// no git tag push), and `publish` writes the notes to the GitHub Release body +
// asset. The four verbs are the release-loop steps a runbook stitches together.
func runRelease(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		_, _ = fmt.Fprintln(stderr, `fishhawk release: subcommand required (prepare|preview|cut|publish)`)
		return exitUsage
	}
	sub, rest := args[0], args[1:]
	switch sub {
	case "preview":
		return releasePreview(rest, stdout, stderr)
	case "prepare":
		return releasePrepare(rest, stdout, stderr)
	case "cut":
		return releaseCut(rest, stdout, stderr)
	case "publish":
		return releasePublish(rest, stdout, stderr)
	default:
		_, _ = fmt.Fprintf(stderr, "fishhawk release: unknown subcommand %q\n", sub)
		return exitUsage
	}
}

// releasePreviewOutput is the `--output json` shape for `release preview`: the
// rendered markdown wrapped in a single field so the JSON output is a stable
// object rather than a bare string.
type releasePreviewOutput struct {
	Markdown string `json:"markdown"`
}

// firstEmptyFlag returns the name of the first flag whose value is empty after
// trimming, or "" when all are present. Args alternate name, value:
// firstEmptyFlag("--repo", *repo, "--from", *from). Centralized so the release
// verbs' required-flag checks stay uniform.
func firstEmptyFlag(nameVals ...string) string {
	for i := 0; i+1 < len(nameVals); i += 2 {
		if strings.TrimSpace(nameVals[i+1]) == "" {
			return nameVals[i]
		}
	}
	return ""
}

// releasePreview implements
// `fishhawk release preview --repo R --from REF --to REF [--output text|json]`.
// It renders the release notes for the ref range without persisting anything,
// reading the backend's text/markdown preview body. text output writes the
// markdown verbatim; json wraps it as {"markdown": "..."}.
func releasePreview(args []string, stdout, stderr io.Writer) int {
	const name = "fishhawk release preview"
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(stderr)
	cf := bindCommonFlags(fs)
	repo := fs.String("repo", "", "REQUIRED target repository (owner/name)")
	from := fs.String("from", "", "REQUIRED start ref of the release range")
	to := fs.String("to", "", "REQUIRED end ref of the release range")
	outputFmt := fs.String("output", "text", "output format: text | json")
	fs.StringVar(outputFmt, "o", "text", "output format: text | json (shorthand)")
	if _, err := parseIntermixed(fs, args); err != nil {
		return exitUsage
	}
	if err := validateOutputFormat(*outputFmt); err != nil {
		_, _ = fmt.Fprintf(stderr, "%s: %v\n", name, err)
		return exitUsage
	}
	if missing := firstEmptyFlag("--repo", *repo, "--from", *from, "--to", *to); missing != "" {
		_, _ = fmt.Fprintf(stderr, "%s: %s is required\n", name, missing)
		return exitUsage
	}

	ctx, cancel := context.WithTimeout(context.Background(), *cf.timeout)
	defer cancel()

	md, err := newClient(cf).PreviewReleaseNotes(ctx,
		strings.TrimSpace(*repo), strings.TrimSpace(*from), strings.TrimSpace(*to))
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "%s: %v\n", name, err)
		return exitOnAPIError(err)
	}

	switch *outputFmt {
	case "json":
		if err := json.NewEncoder(stdout).Encode(releasePreviewOutput{Markdown: md}); err != nil {
			_, _ = fmt.Fprintf(stderr, "%s: encode: %v\n", name, err)
			return exitFailure
		}
	default:
		_, _ = io.WriteString(stdout, md)
		if !strings.HasSuffix(md, "\n") {
			_, _ = io.WriteString(stdout, "\n")
		}
	}
	return exitOK
}

// releasePrepare implements
// `fishhawk release prepare --repo R --from REF --to REF --stage-id UUID [--output text|json]`.
// It persists the rendered notes as a release_notes artifact keyed to the
// caller-supplied stage id and echoes the artifact id + content hash + markdown.
func releasePrepare(args []string, stdout, stderr io.Writer) int {
	const name = "fishhawk release prepare"
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(stderr)
	cf := bindCommonFlags(fs)
	repo := fs.String("repo", "", "REQUIRED target repository (owner/name)")
	from := fs.String("from", "", "REQUIRED start ref of the release range")
	to := fs.String("to", "", "REQUIRED end ref of the release range")
	stageID := fs.String("stage-id", "", "REQUIRED stage id to key the persisted release_notes artifact")
	outputFmt := fs.String("output", "text", "output format: text | json")
	fs.StringVar(outputFmt, "o", "text", "output format: text | json (shorthand)")
	if _, err := parseIntermixed(fs, args); err != nil {
		return exitUsage
	}
	if err := validateOutputFormat(*outputFmt); err != nil {
		_, _ = fmt.Fprintf(stderr, "%s: %v\n", name, err)
		return exitUsage
	}
	if missing := firstEmptyFlag("--repo", *repo, "--from", *from, "--to", *to, "--stage-id", *stageID); missing != "" {
		_, _ = fmt.Fprintf(stderr, "%s: %s is required\n", name, missing)
		return exitUsage
	}
	sid, err := uuid.Parse(strings.TrimSpace(*stageID))
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "%s: --stage-id %q is not a UUID: %v\n", name, *stageID, err)
		return exitUsage
	}

	ctx, cancel := context.WithTimeout(context.Background(), *cf.timeout)
	defer cancel()

	res, err := newClient(cf).PrepareReleaseNotes(ctx, httpclient.PrepareReleaseNotesInput{
		Repo:    strings.TrimSpace(*repo),
		From:    strings.TrimSpace(*from),
		To:      strings.TrimSpace(*to),
		StageID: sid.String(),
	})
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "%s: %v\n", name, err)
		return exitOnAPIError(err)
	}

	switch *outputFmt {
	case "json":
		if err := json.NewEncoder(stdout).Encode(res); err != nil {
			_, _ = fmt.Fprintf(stderr, "%s: encode: %v\n", name, err)
			return exitFailure
		}
	default:
		_, _ = fmt.Fprintf(stdout, "artifact_id:    %s\n", res.ArtifactID)
		_, _ = fmt.Fprintf(stdout, "stage_id:       %s\n", res.StageID)
		_, _ = fmt.Fprintf(stdout, "content_hash:   %s\n", res.ContentHash)
		_, _ = fmt.Fprintln(stdout, "--- release notes ---")
		_, _ = io.WriteString(stdout, res.Markdown)
		if !strings.HasSuffix(res.Markdown, "\n") {
			_, _ = io.WriteString(stdout, "\n")
		}
	}
	return exitOK
}

// releaseCut implements
// `fishhawk release cut --repo R --run-id UUID --artifact-id UUID --version V [--stage-id UUID] [--bump-level L] [--output text|json]`.
// It records the operator's ratified release-version decision as a release_cut
// audit entry on the run's chain. It records the DECISION only: Fishhawk pushes
// NO git tag — tagging the release stays a human git action per the delegating
// posture, and the text output says so.
func releaseCut(args []string, stdout, stderr io.Writer) int {
	const name = "fishhawk release cut"
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(stderr)
	cf := bindCommonFlags(fs)
	repo := fs.String("repo", "", "REQUIRED target repository (owner/name)")
	runID := fs.String("run-id", "", "REQUIRED release run id the cut decision is recorded on")
	artifactID := fs.String("artifact-id", "", "REQUIRED release_notes artifact id the version is cut against")
	version := fs.String("version", "", "REQUIRED ratified release version (e.g. v1.4.0)")
	stageID := fs.String("stage-id", "", "optional stage id to key the release_cut audit entry")
	bumpLevel := fs.String("bump-level", "", "optional advisory semver level recorded verbatim (patch|minor|major)")
	outputFmt := fs.String("output", "text", "output format: text | json")
	fs.StringVar(outputFmt, "o", "text", "output format: text | json (shorthand)")
	if _, err := parseIntermixed(fs, args); err != nil {
		return exitUsage
	}
	if err := validateOutputFormat(*outputFmt); err != nil {
		_, _ = fmt.Fprintf(stderr, "%s: %v\n", name, err)
		return exitUsage
	}
	if missing := firstEmptyFlag("--repo", *repo, "--run-id", *runID, "--artifact-id", *artifactID, "--version", *version); missing != "" {
		_, _ = fmt.Fprintf(stderr, "%s: %s is required\n", name, missing)
		return exitUsage
	}
	rid, err := uuid.Parse(strings.TrimSpace(*runID))
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "%s: --run-id %q is not a UUID: %v\n", name, *runID, err)
		return exitUsage
	}
	aid, err := uuid.Parse(strings.TrimSpace(*artifactID))
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "%s: --artifact-id %q is not a UUID: %v\n", name, *artifactID, err)
		return exitUsage
	}
	sidStr := ""
	if s := strings.TrimSpace(*stageID); s != "" {
		sid, err := uuid.Parse(s)
		if err != nil {
			_, _ = fmt.Fprintf(stderr, "%s: --stage-id %q is not a UUID: %v\n", name, *stageID, err)
			return exitUsage
		}
		sidStr = sid.String()
	}

	ctx, cancel := context.WithTimeout(context.Background(), *cf.timeout)
	defer cancel()

	res, err := newClient(cf).CutRelease(ctx, httpclient.CutReleaseInput{
		Repo:       strings.TrimSpace(*repo),
		RunID:      rid.String(),
		StageID:    sidStr,
		ArtifactID: aid.String(),
		Version:    strings.TrimSpace(*version),
		BumpLevel:  strings.TrimSpace(*bumpLevel),
	})
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "%s: %v\n", name, err)
		return exitOnAPIError(err)
	}

	switch *outputFmt {
	case "json":
		if err := json.NewEncoder(stdout).Encode(res); err != nil {
			_, _ = fmt.Fprintf(stderr, "%s: encode: %v\n", name, err)
			return exitFailure
		}
	default:
		_, _ = fmt.Fprintf(stdout, "version:        %s\n", res.Version)
		_, _ = fmt.Fprintf(stdout, "artifact_id:    %s\n", res.ArtifactID)
		_, _ = fmt.Fprintf(stdout, "content_hash:   %s\n", res.ContentHash)
		if res.BumpLevel != "" {
			_, _ = fmt.Fprintf(stdout, "bump_level:     %s\n", res.BumpLevel)
		}
		_, _ = fmt.Fprintf(stdout, "recorded:       %t\n", res.Recorded)
		// The delegating posture (binding approval condition): cut records the
		// decision only. Push the git tag yourself — Fishhawk performs no tag push.
		_, _ = fmt.Fprintf(stdout,
			"note:           decision recorded only — push the git tag %s yourself (fishhawk performs no tag push)\n",
			res.Version)
	}
	return exitOK
}

// releasePublish implements
// `fishhawk release publish --repo R --tag T --run-id UUID --artifact-id UUID [--stage-id UUID] [--output text|json]`.
// It writes the persisted release_notes markdown to the GitHub Release body +
// fixed-name asset and records a release_published audit entry. Idempotent on
// content hash server-side; the response's published/idempotent flags
// distinguish a real publish from a no-op re-invoke.
func releasePublish(args []string, stdout, stderr io.Writer) int {
	const name = "fishhawk release publish"
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(stderr)
	cf := bindCommonFlags(fs)
	repo := fs.String("repo", "", "REQUIRED target repository (owner/name)")
	tag := fs.String("tag", "", "REQUIRED git tag of the published GitHub Release to update")
	runID := fs.String("run-id", "", "REQUIRED release run id the publish is recorded on")
	artifactID := fs.String("artifact-id", "", "REQUIRED release_notes artifact id whose markdown becomes the Release body")
	stageID := fs.String("stage-id", "", "optional stage id to key the release_published audit entry")
	outputFmt := fs.String("output", "text", "output format: text | json")
	fs.StringVar(outputFmt, "o", "text", "output format: text | json (shorthand)")
	if _, err := parseIntermixed(fs, args); err != nil {
		return exitUsage
	}
	if err := validateOutputFormat(*outputFmt); err != nil {
		_, _ = fmt.Fprintf(stderr, "%s: %v\n", name, err)
		return exitUsage
	}
	if missing := firstEmptyFlag("--repo", *repo, "--tag", *tag, "--run-id", *runID, "--artifact-id", *artifactID); missing != "" {
		_, _ = fmt.Fprintf(stderr, "%s: %s is required\n", name, missing)
		return exitUsage
	}
	rid, err := uuid.Parse(strings.TrimSpace(*runID))
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "%s: --run-id %q is not a UUID: %v\n", name, *runID, err)
		return exitUsage
	}
	aid, err := uuid.Parse(strings.TrimSpace(*artifactID))
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "%s: --artifact-id %q is not a UUID: %v\n", name, *artifactID, err)
		return exitUsage
	}
	sidStr := ""
	if s := strings.TrimSpace(*stageID); s != "" {
		sid, err := uuid.Parse(s)
		if err != nil {
			_, _ = fmt.Fprintf(stderr, "%s: --stage-id %q is not a UUID: %v\n", name, *stageID, err)
			return exitUsage
		}
		sidStr = sid.String()
	}

	ctx, cancel := context.WithTimeout(context.Background(), *cf.timeout)
	defer cancel()

	res, err := newClient(cf).PublishRelease(ctx, httpclient.PublishReleaseInput{
		Repo:       strings.TrimSpace(*repo),
		Tag:        strings.TrimSpace(*tag),
		RunID:      rid.String(),
		StageID:    sidStr,
		ArtifactID: aid.String(),
	})
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "%s: %v\n", name, err)
		return exitOnAPIError(err)
	}

	switch *outputFmt {
	case "json":
		if err := json.NewEncoder(stdout).Encode(res); err != nil {
			_, _ = fmt.Fprintf(stderr, "%s: encode: %v\n", name, err)
			return exitFailure
		}
	default:
		_, _ = fmt.Fprintf(stdout, "release_url:    %s\n", res.ReleaseURL)
		_, _ = fmt.Fprintf(stdout, "tag:            %s\n", res.Tag)
		_, _ = fmt.Fprintf(stdout, "artifact_id:    %s\n", res.ArtifactID)
		_, _ = fmt.Fprintf(stdout, "content_hash:   %s\n", res.ContentHash)
		_, _ = fmt.Fprintf(stdout, "published:      %t\n", res.Published)
		_, _ = fmt.Fprintf(stdout, "idempotent:     %t\n", res.Idempotent)
	}
	return exitOK
}
