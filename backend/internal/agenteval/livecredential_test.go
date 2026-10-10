package agenteval

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/anthropics/anthropic-sdk-go/option"

	"github.com/kuhlman-labs/fishhawk/backend/internal/anthropic"
)

// The shared credential gate for every operator-run live arm (E83.96 / #4223).
//
// A live arm authenticates with EXACTLY ONE of two operator-shell variables:
// FISHHAWKD_ANTHROPIC_API_KEY (sent as X-Api-Key) or
// FISHHAWKD_ANTHROPIC_AUTH_TOKEN (an OAuth bearer, sent as
// `Authorization: Bearer`). Neither set skips the arm as UNMEASURED; both set is
// a refusal, not a skip, because the operator opted in with a contradictory
// configuration and a silent skip would read as "unmeasured" rather than
// "misconfigured". Which bearer tokens Anthropic's terms permit for direct API
// use is the operator's responsibility.
//
// This file is test-only on purpose: it is not a scripts/check-review-prompt-eval
// trigger path, and the runner's default-deny gate env drops every FISHHAWKD_*
// variable, so no in-loop run resolves a credential here.

const (
	liveAPIKeyEnv    = "FISHHAWKD_ANTHROPIC_API_KEY"
	liveAuthTokenEnv = "FISHHAWKD_ANTHROPIC_AUTH_TOKEN"
)

// liveCredential is the one credential a live arm resolved. Exactly one field is
// non-empty for a resolved credential; the zero value means none resolved.
type liveCredential struct {
	apiKey    string
	authToken string
}

// resolveLiveCredential reads the two credential variables through getenv.
// Neither set returns the zero credential and a skipReason naming BOTH
// variables; both set returns an error naming both and saying exactly one is
// accepted; exactly one set returns that credential.
func resolveLiveCredential(getenv func(string) string) (cred liveCredential, skipReason string, err error) {
	key, tok := getenv(liveAPIKeyEnv), getenv(liveAuthTokenEnv)
	switch {
	case key != "" && tok != "":
		return liveCredential{}, "", &liveCredentialConflictError{}
	case key != "":
		return liveCredential{apiKey: key}, "", nil
	case tok != "":
		return liveCredential{authToken: tok}, "", nil
	}
	return liveCredential{}, liveAPIKeyEnv + " and " + liveAuthTokenEnv + " are both unset (set exactly one);", nil
}

// liveCredentialConflictError is the both-set refusal. It names both variables
// and the exactly-one rule.
type liveCredentialConflictError struct{}

func (*liveCredentialConflictError) Error() string {
	return liveAPIKeyEnv + " and " + liveAuthTokenEnv + " are both set; exactly one is accepted"
}

// config stamps exactly the resolved credential onto cfg, overwriting both
// credential fields so a caller's literal can never carry the other one.
func (c liveCredential) config(cfg anthropic.Config) anthropic.Config {
	cfg.APIKey = c.apiKey
	cfg.AuthToken = c.authToken
	return cfg
}

// requireLiveCredential resolves the credential from the process environment.
// Both set fails the test (a refusal); neither set skips with the shared
// skipReason followed by the arm-specific detail (what the skip leaves
// UNMEASURED).
func requireLiveCredential(t *testing.T, detail string) liveCredential {
	t.Helper()
	cred, skipReason, err := resolveLiveCredential(os.Getenv)
	if err != nil {
		t.Fatalf("refusing to run the live arm: %v", err)
	}
	if skipReason != "" {
		t.Skip(skipReason + " " + detail)
	}
	return cred
}

func TestResolveLiveCredential(t *testing.T) {
	const key, tok = "key-sentinel", "tok-sentinel"
	for _, tc := range []struct {
		name      string
		env       map[string]string
		want      liveCredential
		wantSkip  bool
		wantErr   bool
		skipNames []string
		errNames  []string
	}{
		{name: "neither", env: nil, wantSkip: true, skipNames: []string{liveAPIKeyEnv, liveAuthTokenEnv}},
		{name: "key only", env: map[string]string{liveAPIKeyEnv: key}, want: liveCredential{apiKey: key}},
		{name: "token only", env: map[string]string{liveAuthTokenEnv: tok}, want: liveCredential{authToken: tok}},
		{name: "both", env: map[string]string{liveAPIKeyEnv: key, liveAuthTokenEnv: tok}, wantErr: true,
			errNames: []string{liveAPIKeyEnv, liveAuthTokenEnv, "exactly one"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cred, skip, err := resolveLiveCredential(func(k string) string { return tc.env[k] })
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tc.wantErr)
			}
			if (skip != "") != tc.wantSkip {
				t.Fatalf("skipReason = %q, wantSkip %v", skip, tc.wantSkip)
			}
			if cred != tc.want {
				t.Errorf("cred = %+v, want %+v", cred, tc.want)
			}
			for _, n := range tc.skipNames {
				if !strings.Contains(skip, n) {
					t.Errorf("skipReason %q does not name %s", skip, n)
				}
			}
			for _, n := range tc.errNames {
				if err == nil || !strings.Contains(err.Error(), n) {
					t.Errorf("err %v does not name %q", err, n)
				}
			}
			if tc.wantErr && (cred != liveCredential{}) {
				t.Errorf("a refused configuration resolved a credential: %+v", cred)
			}
		})
	}
}

func TestLiveCredentialConfig_StampsExactlyOne(t *testing.T) {
	base := anthropic.Config{Model: "m", MaxTokens: 7, Timeout: time.Second}

	got := liveCredential{apiKey: "k"}.config(base)
	if got.APIKey != "k" || got.AuthToken != "" {
		t.Errorf("key credential stamped APIKey=%q AuthToken=%q, want only the key", got.APIKey, got.AuthToken)
	}
	got = liveCredential{authToken: "t"}.config(base)
	if got.AuthToken != "t" || got.APIKey != "" {
		t.Errorf("token credential stamped APIKey=%q AuthToken=%q, want only the token", got.APIKey, got.AuthToken)
	}
	if got.Model != "m" || got.MaxTokens != 7 || got.Timeout != time.Second {
		t.Errorf("config() dropped the arm's own settings: %+v", got)
	}
	// A literal that already carries a credential cannot smuggle it past the
	// resolved one.
	got = liveCredential{authToken: "t"}.config(anthropic.Config{APIKey: "stale"})
	if got.APIKey != "" || got.AuthToken != "t" {
		t.Errorf("stale APIKey survived: APIKey=%q AuthToken=%q", got.APIKey, got.AuthToken)
	}
}

// TestLiveCredential_TokenOnlyReachesWireAsBearer is the cross-boundary test:
// env predicate -> liveCredential.config -> anthropic.Config -> NewClient ->
// SDK -> wire. It is the offline proxy for "a token-only operator shell
// authenticates a live arm": the request carries `Authorization: Bearer <tok>`
// and NO X-Api-Key header, even with an ambient ANTHROPIC_API_KEY set.
func TestLiveCredential_TokenOnlyReachesWireAsBearer(t *testing.T) {
	const tok = "tok-sentinel"
	cred, skipReason, err := resolveLiveCredential(func(k string) string {
		if k == liveAuthTokenEnv {
			return tok
		}
		return ""
	})
	// Preconditions asserted BEFORE the client is built, so a gate that stops
	// accepting the token fails here for the stated reason rather than as a
	// confusing wire mismatch.
	if err != nil {
		t.Fatalf("precondition: resolveLiveCredential err = %v, want nil", err)
	}
	if skipReason != "" {
		t.Fatalf("precondition: resolveLiveCredential skipReason = %q, want empty (a token-only shell must resolve a credential)", skipReason)
	}
	if cred.authToken != tok {
		t.Fatalf("precondition: cred.authToken = %q, want %q", cred.authToken, tok)
	}

	t.Setenv("ANTHROPIC_API_KEY", "ambient-api-key-sentinel")

	var (
		requests      int
		auth          string
		apiKeyPresent bool
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		auth = r.Header.Get("Authorization")
		apiKeyPresent = len(r.Header.Values("X-Api-Key")) > 0
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": "msg_test", "type": "message", "role": "assistant",
			"content":     []map[string]string{{"type": "text", "text": "ok"}},
			"model":       "claude-sonnet-4-6",
			"stop_reason": "end_turn",
			"usage":       map[string]int{"input_tokens": 1, "output_tokens": 1},
		})
	}))
	t.Cleanup(srv.Close)

	client := anthropic.NewClient(cred.config(anthropic.Config{
		Model:     "claude-sonnet-4-6",
		MaxTokens: 16,
		Timeout:   5 * time.Second,
	}), option.WithBaseURL(srv.URL))
	if _, _, _, _, _, _, err := client.Messages(context.Background(), "sys", "user"); err != nil {
		t.Fatalf("Messages: %v", err)
	}
	if requests != 1 {
		t.Fatalf("server saw %d requests, want 1", requests)
	}
	if auth != "Bearer "+tok {
		t.Errorf("Authorization = %q, want %q", auth, "Bearer "+tok)
	}
	if apiKeyPresent {
		t.Error("X-Api-Key header present; a token-only live arm must authenticate by bearer alone")
	}
}

// TestLiveArmsResolveCredentialThroughSharedGate is a source scan over this
// package's *_test.go files. A live arm that read the API-key variable directly
// would 401 on a bearer token and diverge from the exactly-one rule, so:
//   - no file other than this one may os.Getenv the API-key variable; and
//   - every file that reads a FISHHAWK_AGENTEVAL_*_LIVE opt-in flag must call
//     requireLiveCredential.
//
// The searched literals are built by concatenation so this file does not match
// its own scan.
func TestLiveArmsResolveCredentialThroughSharedGate(t *testing.T) {
	directKey := "os.Getenv(\"" + "FISHHAWKD_ANTHROPIC" + "_API_KEY\")"
	directKeyConst := "os.Getenv(" + "liveAPI" + "KeyEnv)"
	liveFlag := regexp.MustCompile(`os\.Getenv\("FISHHAWK_AGENTEVAL_` + `[A-Z_]+_LIVE"\)`)

	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read package dir: %v", err)
	}
	scanned, liveFiles := 0, 0
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, "_test.go") || name == "livecredential_test.go" {
			continue
		}
		b, err := os.ReadFile(name)
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		src := string(b)
		scanned++
		if strings.Contains(src, directKey) || strings.Contains(src, directKeyConst) {
			t.Errorf("%s reads the API-key variable directly; resolve the credential through requireLiveCredential so the bearer-token variable and the exactly-one rule apply", name)
		}
		if liveFlag.MatchString(src) {
			liveFiles++
			if !strings.Contains(src, "requireLiveCredential(") {
				t.Errorf("%s reads a FISHHAWK_AGENTEVAL_*_LIVE opt-in flag but never calls requireLiveCredential", name)
			}
		}
	}
	if scanned == 0 {
		t.Fatal("scanned no test files; the source scan is vacuous")
	}
	if liveFiles == 0 {
		t.Fatal("found no file reading a live opt-in flag; the scan's flag pattern is stale")
	}
}
