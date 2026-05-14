package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/cli/internal/httpclient"
)

// runAudit dispatches `fishhawk audit <subcommand>`. The audit log
// has been SPA-only until now (per ADR-019 / #320, every dashboard
// surface needs a terminal equivalent so operators don't have to
// alt-tab to inspect a run).
func runAudit(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		_, _ = fmt.Fprintln(stderr, `fishhawk audit: subcommand required (list)`)
		return exitUsage
	}
	sub, rest := args[0], args[1:]
	switch sub {
	case "list":
		return auditList(rest, stdout, stderr)
	default:
		_, _ = fmt.Fprintf(stderr, "fishhawk audit: unknown subcommand %q\n", sub)
		return exitUsage
	}
}

// auditList implements `fishhawk audit list <run-id> [--category C] [--stage UUID] [--limit N] [--cursor X] [--output text|json]`.
//
// Text output is a four-column table: `seq | category | actor | when
// | summary`. The summary column is best-effort: payloads vary by
// category, so the renderer picks a few well-known fields when they
// exist and falls back to a compact JSON one-liner otherwise.
//
// JSON output is NDJSON (one entry per line) rather than a single
// JSON array — friendlier for jq pipelines and lets the user pipe
// large pages through `head` without breaking the parser.
func auditList(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("fishhawk audit list", flag.ContinueOnError)
	fs.SetOutput(stderr)
	cf := bindCommonFlags(fs)
	category := fs.String("category", "", "filter by audit category (e.g. plan_generated)")
	stage := fs.String("stage", "", "filter by stage id (UUID)")
	limit := fs.Int("limit", 0, "max items per page (server default 50, max 500)")
	cursor := fs.String("cursor", "", "pagination cursor from a prior list response")
	outputFmt := fs.String("output", "text", "output format: text | json (ndjson)")
	fs.StringVar(outputFmt, "o", "text", "output format: text | json (shorthand)")
	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	if err := validateOutputFormat(*outputFmt); err != nil {
		_, _ = fmt.Fprintf(stderr, "fishhawk audit list: %v\n", err)
		return exitUsage
	}
	if fs.NArg() != 1 {
		_, _ = fmt.Fprintln(stderr, "fishhawk audit list: <run-id> required")
		return exitUsage
	}
	runID, err := uuid.Parse(fs.Arg(0))
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "fishhawk audit list: %q is not a UUID: %v\n", fs.Arg(0), err)
		return exitUsage
	}
	// --stage is a UUID too; surface a clear local error before the
	// network round-trip so the operator doesn't get a generic
	// "validation_failed" from the backend.
	if *stage != "" {
		if _, err := uuid.Parse(*stage); err != nil {
			_, _ = fmt.Fprintf(stderr, "fishhawk audit list: --stage %q is not a UUID: %v\n", *stage, err)
			return exitUsage
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), *cf.timeout)
	defer cancel()
	res, err := newClient(cf).ListRunAudit(ctx, runID, httpclient.ListRunAuditFilter{
		Category: *category,
		StageID:  *stage,
		Limit:    *limit,
		Cursor:   *cursor,
	})
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "fishhawk audit list: %v\n", err)
		return exitOnAPIError(err)
	}

	switch *outputFmt {
	case "json":
		enc := json.NewEncoder(stdout)
		for i := range res.Items {
			if err := enc.Encode(res.Items[i]); err != nil {
				_, _ = fmt.Fprintf(stderr, "fishhawk audit list: encode: %v\n", err)
				return exitFailure
			}
		}
	default:
		printAuditTable(stdout, res.Items)
	}
	if res.NextCursor != "" {
		_, _ = fmt.Fprintf(stdout, "\nMore: --cursor %s\n", res.NextCursor)
	}
	return exitOK
}

// printAuditTable renders an audit page as a four-column table. The
// "summary" column is a best-effort one-liner pulled from the
// payload; columns are padded to fixed widths so a small page is
// readable without piping through column(1).
func printAuditTable(w io.Writer, entries []httpclient.AuditEntry) {
	if len(entries) == 0 {
		_, _ = fmt.Fprintln(w, "(no audit entries)")
		return
	}
	_, _ = fmt.Fprintf(w, "%-6s  %-30s  %-15s  %-20s  %s\n", "SEQ", "CATEGORY", "ACTOR", "WHEN", "SUMMARY")
	for i := range entries {
		e := &entries[i]
		actor := "system"
		if e.ActorSubject != nil && *e.ActorSubject != "" {
			actor = *e.ActorSubject
		}
		when := e.Timestamp.UTC().Format(time.RFC3339)
		_, _ = fmt.Fprintf(w, "%-6d  %-30s  %-15s  %-20s  %s\n",
			e.Sequence, truncateColumn(e.Category, 30), truncateColumn(actor, 15),
			when, summarizePayload(e.Payload))
	}
}

// truncateColumn caps s at max with a trailing ellipsis when over
// budget. Keeps the table aligned on long category / actor strings.
func truncateColumn(s string, max int) string {
	if len(s) <= max {
		return s
	}
	if max <= 1 {
		return s[:max]
	}
	return s[:max-1] + "…"
}

// summarizePayload picks one operator-relevant field out of an
// audit payload when present, falling back to a compact JSON
// one-liner. Payloads vary by category — this is best-effort; the
// JSON / `--output json` path always carries the full body.
func summarizePayload(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var p map[string]any
	if err := json.Unmarshal(raw, &p); err != nil {
		return strings.TrimSpace(string(raw))
	}
	// The closed set below tracks the audit categories the SPA
	// surfaces today. Order matters — earlier keys win.
	for _, k := range []string{
		"summary", "message", "reason", "decision",
		"check_name", "retry_attempt", "kind",
		"pr_url", "issue_number", "head_sha",
	} {
		if v, ok := p[k]; ok && v != nil {
			s := fmt.Sprint(v)
			if s != "" {
				return truncateColumn(s, 64)
			}
		}
	}
	// Fall back to the compact JSON if no recognized field exists.
	compact, _ := json.Marshal(p)
	return truncateColumn(string(compact), 64)
}
