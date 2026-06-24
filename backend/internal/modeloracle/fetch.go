package modeloracle

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	anthropicsdk "github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"
)

// Fetcher fetches the set of model ids a single provider currently serves.
// Implementations hit the vendor's GET /v1/models endpoint and return the id
// set; an error keeps the prior cached snapshot (the caller fails open).
type Fetcher interface {
	Fetch(ctx context.Context) ([]string, error)
}

// defaultFetchTimeout bounds a single provider fetch when the caller supplies no
// http.Client. It is generous (the refresh runs on a 12h cadence) but finite so
// a hung vendor endpoint cannot wedge the refresh goroutine.
const defaultFetchTimeout = 30 * time.Second

// defaultOpenAIBaseURL is the OpenAI API host used when no override is supplied.
// The fetcher appends /v1/models to it. Anthropic needs no analogue: the SDK
// carries its own production base URL.
const defaultOpenAIBaseURL = "https://api.openai.com"

// AnthropicFetcher fetches Anthropic's served model ids via the already-imported
// anthropic-sdk-go (Models.ListAutoPaging → ModelInfo.ID, draining every page).
// It is registered under the "claudecode" provider key (the executor/adapter
// vocabulary the allow-list also keys on), fetching its vendor — Anthropic —
// internally.
type AnthropicFetcher struct {
	apiKey  string
	baseURL string // optional; empty uses the SDK's production default
	client  *http.Client
}

// NewAnthropicFetcher builds an AnthropicFetcher. baseURL is optional (tests
// point it at an httptest server via the SDK's option.WithBaseURL); a nil client
// gets a default bounded-timeout client.
func NewAnthropicFetcher(apiKey, baseURL string, client *http.Client) *AnthropicFetcher {
	if client == nil {
		client = &http.Client{Timeout: defaultFetchTimeout}
	}
	return &AnthropicFetcher{apiKey: apiKey, baseURL: baseURL, client: client}
}

// Fetch lists every Anthropic model id, transparently draining pagination via
// ListAutoPaging. A page-fetch error aborts and surfaces (the caller keeps the
// prior snapshot).
func (f *AnthropicFetcher) Fetch(ctx context.Context) ([]string, error) {
	opts := []option.RequestOption{
		option.WithAPIKey(f.apiKey),
		option.WithHTTPClient(f.client),
	}
	if f.baseURL != "" {
		opts = append(opts, option.WithBaseURL(f.baseURL))
	}
	client := anthropicsdk.NewClient(opts...)

	var ids []string
	pager := client.Models.ListAutoPaging(ctx, anthropicsdk.ModelListParams{})
	for pager.Next() {
		ids = append(ids, pager.Current().ID)
	}
	if err := pager.Err(); err != nil {
		return nil, fmt.Errorf("anthropic models list: %w", err)
	}
	return ids, nil
}

// OpenAIFetcher fetches OpenAI's served model ids with a raw net/http GET to
// {baseURL}/v1/models (Authorization: Bearer <key>) — there is no OpenAI Go SDK.
// It is registered under the "codex" provider key, fetching its vendor — OpenAI
// — internally.
type OpenAIFetcher struct {
	apiKey  string
	baseURL string
	client  *http.Client
}

// NewOpenAIFetcher builds an OpenAIFetcher. An empty baseURL defaults to the
// OpenAI production host; a nil client gets a default bounded-timeout client.
func NewOpenAIFetcher(apiKey, baseURL string, client *http.Client) *OpenAIFetcher {
	if baseURL == "" {
		baseURL = defaultOpenAIBaseURL
	}
	if client == nil {
		client = &http.Client{Timeout: defaultFetchTimeout}
	}
	return &OpenAIFetcher{apiKey: apiKey, baseURL: baseURL, client: client}
}

// Fetch GETs /v1/models and returns the id set. A non-200 status or a decode
// failure is an error (the caller keeps the prior snapshot).
func (f *OpenAIFetcher) Fetch(ctx context.Context) ([]string, error) {
	url := strings.TrimRight(f.baseURL, "/") + "/v1/models"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("openai models request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+f.apiKey)

	resp, err := f.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("openai models list: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("openai models list: status %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	var parsed struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		return nil, fmt.Errorf("openai models decode: %w", err)
	}
	ids := make([]string, 0, len(parsed.Data))
	for _, m := range parsed.Data {
		ids = append(ids, m.ID)
	}
	return ids, nil
}
