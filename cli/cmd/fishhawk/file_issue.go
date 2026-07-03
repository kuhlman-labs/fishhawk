package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/kuhlman-labs/fishhawk/cli/internal/httpclient"
)

// fileIssueRelations / fileIssueRequest / filedWorkItem mirror the
// POST /v0/work-items wire shapes (docs/api/v0.openapi.yaml,
// backend/internal/server/workitems.go). Repeated locally rather than added
// to cli/internal/httpclient because this verb is the only consumer and the
// conventions layer — not the CLI — owns the call shape.
type fileIssueRelations struct {
	ParentEpic   string   `json:"parent_epic,omitempty"`
	Supersedes   []string `json:"supersedes,omitempty"`
	CompanionTo  []string `json:"companion_to,omitempty"`
	EvidenceRuns []string `json:"evidence_runs,omitempty"`
}

type fileIssueRequest struct {
	Repo       string              `json:"repo"`
	Type       string              `json:"type"`
	Summary    string              `json:"summary"`
	Body       string              `json:"body,omitempty"`
	Labels     []string            `json:"labels,omitempty"`
	Complexity string              `json:"complexity,omitempty"`
	Status     string              `json:"status,omitempty"`
	Relations  *fileIssueRelations `json:"relations,omitempty"`
	RunID      string              `json:"run_id,omitempty"`
}

type filedWorkItem struct {
	Type          string   `json:"type"`
	Title         string   `json:"title"`
	Number        int      `json:"number"`
	URL           string   `json:"url"`
	Provider      string   `json:"provider"`
	AppliedLabels []string `json:"applied_labels,omitempty"`
	Complexity    string   `json:"complexity,omitempty"`
	Status        string   `json:"status,omitempty"`
	BoardColumn   string   `json:"board_column,omitempty"`
	Audited       bool     `json:"audited"`
	// DefaultedLabels / MissingLabelNamespaces surface the backend's LOUD
	// label-completeness report (#1616): system-added labels the caller did
	// not supply, and any required namespace still absent (reported, never a
	// rejection).
	DefaultedLabels        []string `json:"defaulted_labels,omitempty"`
	MissingLabelNamespaces []string `json:"missing_label_namespaces,omitempty"`
}

// stringSliceFlag accumulates a repeatable string flag (e.g. --label a
// --label b) into a slice.
type stringSliceFlag []string

func (s *stringSliceFlag) String() string { return strings.Join(*s, ",") }

func (s *stringSliceFlag) Set(v string) error {
	*s = append(*s, v)
	return nil
}

// fileIssueHTTPDo is the HTTP seam for the file-issue verb. Tests swap it
// for a stub; production uses a 60s-timeout client (matching httpclient).
var fileIssueHTTPDo = func(req *http.Request) (*http.Response, error) {
	return (&http.Client{Timeout: 60 * time.Second}).Do(req)
}

// runFileIssue implements `fishhawk file-issue`: file a work item through
// the repo's work-management conventions (#1005). The same call shape works
// against a GitHub-Projects- or Jira-configured repo — only the per-repo
// conventions differ.
func runFileIssue(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("fishhawk file-issue", flag.ContinueOnError)
	fs.SetOutput(stderr)
	cf := bindCommonFlags(fs)

	repo := fs.String("repo", envOr("GITHUB_REPOSITORY", ""), "target repo as owner/name (default $GITHUB_REPOSITORY)")
	itemType := fs.String("type", "", "work-item type: a key in the repo's conventions (feature, bug, chore, adr, ...)")
	summary := fs.String("summary", "", "one-line summary: fills the title and the required Summary field")
	body := fs.String("body", "", "verbatim body; when omitted the body is assembled from the type's skeleton")
	complexity := fs.String("complexity", "", "complexity prior, overriding the type default (e.g. low|medium|high)")
	status := fs.String("status", "", "board status/column, overriding the type default")
	parentEpic := fs.String("parent-epic", "", "epic this item rolls up to (e.g. #1005)")
	runID := fs.String("run-id", "", "in-flight run UUID; when set and non-terminal a work_item_filed audit entry is appended")
	var labels, supersedes, companionTo, evidenceRuns stringSliceFlag
	fs.Var(&labels, "label", "label to add (repeatable); merged on top of the type's default labels")
	fs.Var(&supersedes, "supersedes", "item this supersedes (repeatable)")
	fs.Var(&companionTo, "companion-to", "companion item reference (repeatable)")
	fs.Var(&evidenceRuns, "evidence-run", "evidence run reference (repeatable)")
	outputFmt := fs.String("output", "text", "output format: text | json")
	fs.StringVar(outputFmt, "o", "text", "output format: text | json (shorthand)")

	if err := fs.Parse(args); err != nil {
		return exitUsage
	}
	if *outputFmt != "text" && *outputFmt != "json" {
		_, _ = fmt.Fprintf(stderr, "fishhawk file-issue: invalid --output %q (want text|json)\n", *outputFmt)
		return exitUsage
	}
	if strings.TrimSpace(*repo) == "" {
		_, _ = fmt.Fprintln(stderr, "fishhawk file-issue: --repo is required (owner/name), or set GITHUB_REPOSITORY")
		return exitUsage
	}
	if strings.TrimSpace(*itemType) == "" {
		_, _ = fmt.Fprintln(stderr, "fishhawk file-issue: --type is required")
		return exitUsage
	}
	if strings.TrimSpace(*summary) == "" {
		_, _ = fmt.Fprintln(stderr, "fishhawk file-issue: --summary is required")
		return exitUsage
	}

	req := fileIssueRequest{
		Repo:       strings.TrimSpace(*repo),
		Type:       strings.TrimSpace(*itemType),
		Summary:    *summary,
		Body:       *body,
		Labels:     labels,
		Complexity: *complexity,
		Status:     *status,
		RunID:      strings.TrimSpace(*runID),
	}
	if *parentEpic != "" || len(supersedes) > 0 || len(companionTo) > 0 || len(evidenceRuns) > 0 {
		req.Relations = &fileIssueRelations{
			ParentEpic:   *parentEpic,
			Supersedes:   supersedes,
			CompanionTo:  companionTo,
			EvidenceRuns: evidenceRuns,
		}
	}

	item, err := postWorkItem(context.Background(), *cf.backendURL, *cf.token, req)
	if err != nil {
		_, _ = fmt.Fprintf(stderr, "fishhawk file-issue: %v\n", err)
		return exitFailure
	}

	if *outputFmt == "json" {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(item); err != nil {
			_, _ = fmt.Fprintf(stderr, "fishhawk file-issue: encode: %v\n", err)
			return exitFailure
		}
		return exitOK
	}
	printFiledWorkItem(stdout, item)
	return exitOK
}

// postWorkItem POSTs the filing request to /v0/work-items and decodes the
// created item. On a non-2xx it decodes the OpenAPI error envelope into an
// *httpclient.APIError so callers see the typed code (mirrors
// httpclient.do, which is unexported). The /v0/work-items endpoint is not on
// the shared httpclient.Client surface — this verb is its only consumer.
func postWorkItem(ctx context.Context, backendURL, token string, req fileIssueRequest) (*filedWorkItem, error) {
	body, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}
	url := strings.TrimRight(backendURL, "/") + "/v0/work-items"
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set("Accept", "application/json")
	if token != "" {
		httpReq.Header.Set("Authorization", "Bearer "+token)
	}

	resp, err := fileIssueHTTPDo(httpReq)
	if err != nil {
		return nil, fmt.Errorf("http: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read body: %w", err)
	}
	if resp.StatusCode >= 400 {
		apiErr := &httpclient.APIError{StatusCode: resp.StatusCode}
		var env struct {
			Error struct {
				Code    string         `json:"code"`
				Message string         `json:"message"`
				Details map[string]any `json:"details"`
			} `json:"error"`
		}
		if json.Unmarshal(raw, &env) == nil {
			apiErr.Code = env.Error.Code
			apiErr.Message = env.Error.Message
			apiErr.Details = env.Error.Details
		}
		return nil, apiErr
	}

	var out filedWorkItem
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("decode response: %w", err)
	}
	return &out, nil
}

// printFiledWorkItem renders the created item for the text output format.
func printFiledWorkItem(w io.Writer, item *filedWorkItem) {
	_, _ = fmt.Fprintf(w, "filed %s #%d\n", item.Type, item.Number)
	_, _ = fmt.Fprintf(w, "  title:      %s\n", item.Title)
	_, _ = fmt.Fprintf(w, "  url:        %s\n", item.URL)
	_, _ = fmt.Fprintf(w, "  provider:   %s\n", item.Provider)
	if len(item.AppliedLabels) > 0 {
		_, _ = fmt.Fprintf(w, "  labels:     %s\n", strings.Join(item.AppliedLabels, ", "))
	}
	if len(item.DefaultedLabels) > 0 {
		_, _ = fmt.Fprintf(w, "  defaulted:  %s\n", strings.Join(item.DefaultedLabels, ", "))
	}
	if len(item.MissingLabelNamespaces) > 0 {
		_, _ = fmt.Fprintf(w, "  missing ns: %s\n", strings.Join(item.MissingLabelNamespaces, ", "))
	}
	if item.Complexity != "" {
		_, _ = fmt.Fprintf(w, "  complexity: %s\n", item.Complexity)
	}
	if item.Status != "" {
		_, _ = fmt.Fprintf(w, "  status:     %s\n", item.Status)
	}
	if item.BoardColumn != "" {
		_, _ = fmt.Fprintf(w, "  board:      %s\n", item.BoardColumn)
	}
	_, _ = fmt.Fprintf(w, "  audited:    %t\n", item.Audited)
}
