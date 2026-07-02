package corpusdistill

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
)

// maxErrBody bounds how much of a non-200 response body we read into the
// error message, so a large/HTML error page can't blow up the message.
const maxErrBody = 4096

// FetchStageTrace GETs the redacted trace bundle for a stage from a running
// backend: {baseURL}/v0/stages/{stageID}/trace. When token is non-empty it
// sets Authorization: Bearer <token> — the trace endpoint authenticates a
// non-browser (CLI) client via the Authorization: Bearer header carrying an
// fhk_ API token (backend/internal/server/middleware.go bearerAuth), not a
// session cookie.
//
// The returned bytes are the body as-is. The endpoint sends
// Content-Encoding: gzip, which Go's transport may transparently
// decompress, so the bytes may be gzipped or plain — Distill's gzip-magic
// auto-detect handles either, so this does no manual gunzip. baseURL is a
// parameter so tests can target an httptest.Server.
func FetchStageTrace(ctx context.Context, baseURL, stageID, token string) ([]byte, error) {
	url := fmt.Sprintf("%s/v0/stages/%s/trace", baseURL, stageID)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("corpusdistill: build trace request: %w", err)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("corpusdistill: GET %s: %w", url, err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrBody))
		return nil, fmt.Errorf("corpusdistill: GET %s: unexpected status %d: %s",
			url, resp.StatusCode, string(snippet))
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("corpusdistill: read trace body from %s: %w", url, err)
	}
	return body, nil
}

// FetchRunTriageAudit GETs a run's acceptance_triage_decided audit entries
// from a running backend: {baseURL}/v0/runs/{runID}/audit?category=
// acceptance_triage_decided&limit=500, following next_cursor pages so a
// long-audit run cannot silently truncate (the audit endpoint caps limit at
// 500 — reads.go auditMaxLimit). Non-200 responses error with the same
// status + body-snippet shape as FetchStageTrace; token semantics are
// identical (Authorization: Bearer when non-empty).
func FetchRunTriageAudit(ctx context.Context, baseURL, runID, token string) ([]AuditItem, error) {
	var items []AuditItem
	cursor := ""
	for {
		u := fmt.Sprintf("%s/v0/runs/%s/audit?category=acceptance_triage_decided&limit=500", baseURL, runID)
		if cursor != "" {
			u += "&cursor=" + url.QueryEscape(cursor)
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
		if err != nil {
			return nil, fmt.Errorf("corpusdistill: build audit request: %w", err)
		}
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}

		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return nil, fmt.Errorf("corpusdistill: GET %s: %w", u, err)
		}
		if resp.StatusCode != http.StatusOK {
			snippet, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrBody))
			_ = resp.Body.Close()
			return nil, fmt.Errorf("corpusdistill: GET %s: unexpected status %d: %s",
				u, resp.StatusCode, string(snippet))
		}
		var page struct {
			Items      []AuditItem `json:"items"`
			NextCursor string      `json:"next_cursor"`
		}
		err = json.NewDecoder(resp.Body).Decode(&page)
		_ = resp.Body.Close()
		if err != nil {
			return nil, fmt.Errorf("corpusdistill: decode audit page from %s: %w", u, err)
		}
		items = append(items, page.Items...)
		if page.NextCursor == "" {
			return items, nil
		}
		cursor = page.NextCursor
	}
}
