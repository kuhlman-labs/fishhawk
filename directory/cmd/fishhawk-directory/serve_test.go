package main

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/kuhlman-labs/fishhawk/directory/internal/store"
	"github.com/kuhlman-labs/fishhawk/directory/pkg/routing"
)

// setValidEnv installs a complete, valid directory configuration. Each
// fail-closed case then unsets exactly one thing, so the assertion is
// attributable to that one variable.
func setValidEnv(t *testing.T) {
	t.Helper()
	t.Setenv(envDatabaseURL, "postgres://fishhawk@127.0.0.1:1/directory?sslmode=disable")
	t.Setenv(routing.EnvRegions, "us=https://us.cell.example")
	t.Setenv(routing.EnvHandoffSecret, "s3cret")
	t.Setenv(routing.EnvAdminToken, "operator-token")
	t.Setenv(routing.EnvRoutedPaths, "")
	t.Setenv(routing.EnvHandoffTTL, "")
	t.Setenv(envAddr, "")
}

// Every startup guard fails closed with a non-zero exit and a message that
// names the environment variable at fault. None of these reach the
// database: an unreachable URL would hang or fail differently, so a case
// that got that far would not produce the asserted message.
func TestServeFailsClosedOnMissingConfig(t *testing.T) {
	cases := []struct {
		name    string
		unset   func(t *testing.T)
		wantSub string
	}{
		{
			name:    "missing database URL",
			unset:   func(t *testing.T) { t.Setenv(envDatabaseURL, "") },
			wantSub: envDatabaseURL,
		},
		{
			name:    "missing handoff secret",
			unset:   func(t *testing.T) { t.Setenv(routing.EnvHandoffSecret, "") },
			wantSub: routing.EnvHandoffSecret,
		},
		{
			name:    "empty region map",
			unset:   func(t *testing.T) { t.Setenv(routing.EnvRegions, "") },
			wantSub: routing.EnvRegions,
		},
		{
			name:    "unparsable region map",
			unset:   func(t *testing.T) { t.Setenv(routing.EnvRegions, "us~https://us.cell.example") },
			wantSub: routing.EnvRegions,
		},
		{
			name:    "non-absolute cell URL",
			unset:   func(t *testing.T) { t.Setenv(routing.EnvRegions, "us=/cell") },
			wantSub: "must be absolute",
		},
		{
			name:    "unparsable handoff TTL",
			unset:   func(t *testing.T) { t.Setenv(routing.EnvHandoffTTL, "soon") },
			wantSub: routing.EnvHandoffTTL,
		},
		{
			name:    "relative routed path",
			unset:   func(t *testing.T) { t.Setenv(routing.EnvRoutedPaths, "v0/onboarding/start") },
			wantSub: "must be absolute",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			setValidEnv(t)
			tc.unset(t)

			var out bytes.Buffer
			if code := run([]string{"serve"}, &out); code == exitOK {
				t.Fatal("serve exited 0 with an invalid configuration")
			}
			if !strings.Contains(out.String(), tc.wantSub) {
				t.Fatalf("output %q does not name %q", out.String(), tc.wantSub)
			}
		})
	}
}

// A fully-valid configuration whose database is unreachable must fail at
// startup rather than listen without a store: the migration step is the
// first thing that touches Postgres, and its failure is terminal.
func TestServeFailsWhenDatabaseIsUnreachable(t *testing.T) {
	setValidEnv(t)

	var out bytes.Buffer
	if code := run([]string{"serve"}, &out); code != exitFailure {
		t.Fatalf("exit = %d, want %d (output %q)", code, exitFailure, out.String())
	}
	if !strings.Contains(out.String(), "directory migrations failed") {
		t.Fatalf("output %q does not report the migration failure", out.String())
	}
}

// An unset operator credential does not abort startup — it is a runtime
// 503 on both surfaces — but it must be announced, so it is never mistaken
// for a working deployment.
func TestServeWarnsWhenAdminTokenUnset(t *testing.T) {
	setValidEnv(t)
	t.Setenv(routing.EnvAdminToken, "")

	var out bytes.Buffer
	run([]string{"serve"}, &out) // fails later at the unreachable database
	if !strings.Contains(out.String(), "refuse every request") {
		t.Fatalf("output %q carries no unset-credential warning", out.String())
	}
	if !strings.Contains(out.String(), routing.EnvAdminToken) {
		t.Fatalf("warning %q does not name the env var", out.String())
	}
}

func TestServeRejectsUnknownFlag(t *testing.T) {
	setValidEnv(t)
	var out bytes.Buffer
	if code := run([]string{"serve", "--nope"}, &out); code != exitUsage {
		t.Fatalf("exit = %d, want %d", code, exitUsage)
	}
}

func TestMigrateUsage(t *testing.T) {
	cases := []struct {
		name string
		args []string
		env  func(t *testing.T)
		want int
		sub  string
	}{
		{
			name: "no direction",
			args: []string{"migrate"},
			want: exitUsage,
			sub:  "direction (up|down) required",
		},
		{
			name: "unknown direction",
			args: []string{"migrate", "sideways"},
			want: exitUsage,
			sub:  "unknown direction",
		},
		{
			name: "missing database URL",
			args: []string{"migrate", "up"},
			env:  func(t *testing.T) { t.Setenv(envDatabaseURL, "") },
			want: exitUsage,
			sub:  envDatabaseURL,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			setValidEnv(t)
			if tc.env != nil {
				tc.env(t)
			}
			var out bytes.Buffer
			if code := run(tc.args, &out); code != tc.want {
				t.Fatalf("exit = %d, want %d (output %q)", code, tc.want, out.String())
			}
			if !strings.Contains(out.String(), tc.sub) {
				t.Fatalf("output %q does not mention %q", out.String(), tc.sub)
			}
		})
	}
}

func TestDispatch(t *testing.T) {
	var out bytes.Buffer
	if code := run([]string{"help"}, &out); code != exitOK {
		t.Fatalf("help exit = %d, want %d", code, exitOK)
	}
	if !strings.Contains(out.String(), "Usage: fishhawk-directory") {
		t.Fatalf("help printed no usage: %q", out.String())
	}

	out.Reset()
	if code := run([]string{"teleport"}, &out); code != exitUsage {
		t.Fatalf("unknown subcommand exit = %d, want %d", code, exitUsage)
	}
	if !strings.Contains(out.String(), `unknown subcommand "teleport"`) {
		t.Fatalf("output %q does not name the bad subcommand", out.String())
	}
}

func TestSplitCommand(t *testing.T) {
	cases := []struct {
		args     []string
		wantCmd  string
		wantRest int
	}{
		{nil, "", 0},
		{[]string{"--addr=:9000"}, "", 1},
		{[]string{"serve", "--addr=:9000"}, "serve", 1},
		{[]string{"migrate", "up"}, "migrate", 1},
	}
	for _, tc := range cases {
		cmd, rest := splitCommand(tc.args)
		if cmd != tc.wantCmd || len(rest) != tc.wantRest {
			t.Fatalf("splitCommand(%v) = %q, %v", tc.args, cmd, rest)
		}
	}
}

// --- the mounted handler -----------------------------------------------

type fakeStore struct{ regions map[string]string }

func (f fakeStore) AssignRegion(_ context.Context, provider, accountKey, region string) (string, error) {
	k := provider + "/" + accountKey
	if existing, ok := f.regions[k]; ok {
		return existing, nil
	}
	f.regions[k] = region
	return region, nil
}

func (f fakeStore) Lookup(_ context.Context, provider, accountKey string) (string, error) {
	region, ok := f.regions[provider+"/"+accountKey]
	if !ok {
		return "", fmt.Errorf("%w: %s/%s", store.ErrNotFound, provider, accountKey)
	}
	return region, nil
}

func newTestHandler(t *testing.T, adminToken string) http.Handler {
	t.Helper()
	cfg, err := routing.LoadConfig(func(k string) string {
		switch k {
		case routing.EnvRegions:
			return "us=https://us.cell.example"
		case routing.EnvHandoffSecret:
			return "s3cret"
		case routing.EnvAdminToken:
			return adminToken
		default:
			return ""
		}
	})
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	router, err := routing.New(cfg, fakeStore{regions: map[string]string{"github/acme": "us"}})
	if err != nil {
		t.Fatalf("routing.New: %v", err)
	}
	return newHandler(router)
}

// /healthz answers without the operator credential: a liveness probe has
// none, and gating it would make an unconfigured directory look dead
// rather than closed.
func TestHealthzIsUnauthenticated(t *testing.T) {
	for _, adminToken := range []string{"operator-token", ""} {
		w := httptest.NewRecorder()
		newTestHandler(t, adminToken).ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/healthz", nil))
		if w.Code != http.StatusOK {
			t.Fatalf("admin token %q: /healthz = %d, want 200", adminToken, w.Code)
		}
		if !strings.Contains(w.Body.String(), `"ok"`) {
			t.Fatalf("/healthz body = %q", w.Body.String())
		}
	}
}

// Mounting under /healthz must not shadow the router: the routed surface
// still redirects, and the assign surface is still reachable.
func TestHandlerDelegatesToRouter(t *testing.T) {
	h := newTestHandler(t, "operator-token")

	r := httptest.NewRequest(http.MethodGet, routing.DefaultRoutedPath+"?provider=github&account_key=acme", nil)
	r.Header.Set("Authorization", "Bearer operator-token")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusFound {
		t.Fatalf("routed GET = %d, want 302 (body %s)", w.Code, w.Body)
	}
	if loc := w.Header().Get("Location"); !strings.HasPrefix(loc, "https://us.cell.example"+routing.DefaultRoutedPath+"?") {
		t.Fatalf("Location = %q", loc)
	}
}

// An unset operator credential leaves the mounted surfaces refusing, not
// open — the serve path only warns, so this is what proves the posture.
func TestHandlerRefusesWhenAdminTokenUnset(t *testing.T) {
	h := newTestHandler(t, "")

	for _, tc := range []struct{ method, target string }{
		{http.MethodGet, routing.DefaultRoutedPath + "?provider=github&account_key=acme"},
		{http.MethodPost, routing.AssignPath},
	} {
		r := httptest.NewRequest(tc.method, tc.target, strings.NewReader(`{"provider":"github","account_key":"acme","region":"us"}`))
		r.Header.Set("Authorization", "Bearer anything")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != http.StatusServiceUnavailable {
			t.Fatalf("%s %s = %d, want 503", tc.method, tc.target, w.Code)
		}
	}
}
