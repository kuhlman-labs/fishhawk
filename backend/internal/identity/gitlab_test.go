package identity

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeGitLab stands in for a GitLab instance: the OAuth device endpoints
// (/oauth/authorize_device, /oauth/token) and the REST endpoints
// (/api/v4/...) share one host, exactly as a real GitLab deployment does.
//
// Routing is a plain switch on EscapedPath rather than an http.ServeMux
// because the members endpoints carry a URL-escaped project/group id
// ("owner%2Frepo"), which a ServeMux would silently decode into extra path
// segments and mis-route.
type fakeGitLab struct {
	*httptest.Server

	mu       sync.Mutex
	requests int
	routes   map[string]http.HandlerFunc
}

func newFakeGitLab(t *testing.T) *fakeGitLab {
	t.Helper()
	f := &fakeGitLab{routes: map[string]http.HandlerFunc{}}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		f.requests++
		h := f.routes[r.URL.EscapedPath()]
		f.mu.Unlock()
		if h == nil {
			t.Errorf("fakeGitLab: unrouted request %s %s", r.Method, r.URL.EscapedPath())
			w.WriteHeader(http.StatusNotImplemented)
			return
		}
		h(w, r)
	}))
	t.Cleanup(f.Close)
	return f
}

func (f *fakeGitLab) route(path string, h http.HandlerFunc) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.routes[path] = h
}

func (f *fakeGitLab) requestCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.requests
}

// newTestGitLabProvider returns a GitLabIdentityProvider pointed at the
// fake, with instant sleeps so polling tests never wait on the wall clock.
func newTestGitLabProvider(f *fakeGitLab) *GitLabIdentityProvider {
	return &GitLabIdentityProvider{
		baseURL:        f.URL,
		deviceClientID: "device-client",
		httpClient:     f.Client(),
		pollInterval:   time.Millisecond,
		sleep:          func(context.Context, time.Duration) error { return nil },
		now:            time.Now,
	}
}

// membersPage renders a members-endpoint page body.
func membersPage(members ...gitLabMember) string {
	b, err := json.Marshal(members)
	if err != nil {
		panic(err)
	}
	return string(b)
}

// prefixCollisionPage builds a FULL page of usernames for which login is a
// common prefix ("alice-1".."alice-100" for "alice"), so a walk that stops
// at page one, or one that trusts GitLab's partial `query=` filter and
// takes its first row, answers wrongly.
func prefixCollisionPage(login string) string {
	members := make([]gitLabMember, 0, gitLabMembersPerPage)
	for i := 1; i <= gitLabMembersPerPage; i++ {
		members = append(members, gitLabMember{
			Username:    login + "-" + strconv.Itoa(i),
			AccessLevel: gitLabAccessOwner,
		})
	}
	return membersPage(members...)
}

// ---------------------------------------------------------------------
// Construction
// ---------------------------------------------------------------------

func TestNewGitLabIdentityProvider_NormalizesBaseURL(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"empty falls back to SaaS", "", DefaultGitLabBaseURL},
		{"whitespace-only falls back to SaaS", "   ", DefaultGitLabBaseURL},
		{"trailing slashes trimmed", "https://gitlab.example.com//", "https://gitlab.example.com"},
		{"already clean", "https://gitlab.example.com", "https://gitlab.example.com"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p, ok := NewGitLabIdentityProvider(tc.in, "cid", nil).(*GitLabIdentityProvider)
			if !ok {
				t.Fatal("NewGitLabIdentityProvider did not return *GitLabIdentityProvider")
			}
			if p.baseURL != tc.want {
				t.Errorf("baseURL = %q, want %q", p.baseURL, tc.want)
			}
		})
	}
}

// ---------------------------------------------------------------------
// Device flow (RFC 8628)
// ---------------------------------------------------------------------

func TestGitLabVerifyUser_Success(t *testing.T) {
	f := newFakeGitLab(t)
	var deviceBody, tokenBody string
	f.route("/oauth/authorize_device", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		deviceBody = r.Form.Encode()
		_, _ = w.Write([]byte(`{"device_code":"DC","user_code":"WXYZ-1234","verification_uri":"https://gitlab.example.com/oauth/device","expires_in":900,"interval":5}`))
	})
	f.route("/oauth/token", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		tokenBody = r.Form.Encode()
		_, _ = w.Write([]byte(`{"access_token":"glpat-user"}`))
	})
	f.route("/api/v4/user", func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer glpat-user" {
			t.Errorf("GET /api/v4/user Authorization = %q, want Bearer glpat-user", got)
		}
		_, _ = w.Write([]byte(`{"id":42,"username":"alice"}`))
	})

	p := newTestGitLabProvider(f)
	prompts := 0
	var gotCode, gotURI string
	subject, err := p.VerifyUser(context.Background(), func(userCode, verificationURI string) {
		prompts++
		gotCode, gotURI = userCode, verificationURI
	})
	if err != nil {
		t.Fatalf("VerifyUser: %v", err)
	}
	if subject != "gitlab:alice" {
		t.Errorf("subject = %q, want gitlab:alice", subject)
	}
	if prompts != 1 {
		t.Errorf("prompt invoked %d times, want exactly 1", prompts)
	}
	if gotCode != "WXYZ-1234" || gotURI != "https://gitlab.example.com/oauth/device" {
		t.Errorf("prompt got (%q, %q)", gotCode, gotURI)
	}
	if !strings.Contains(deviceBody, "client_id=device-client") {
		t.Errorf("device-code body = %q, want client_id=device-client", deviceBody)
	}
	if !strings.Contains(deviceBody, "scope="+gitLabDeviceFlowScope) {
		t.Errorf("device-code body = %q, want scope=%s", deviceBody, gitLabDeviceFlowScope)
	}
	if !strings.Contains(tokenBody, "grant_type=urn") {
		t.Errorf("token body = %q, want the RFC 8628 device grant_type", tokenBody)
	}
}

// TestGitLabDeviceFlow_SendsNoClientSecret is the structural half of
// binding condition 7's device/browser application split: the device
// application is NON-Confidential, so neither device-flow POST may carry a
// client_secret. Asserted on BOTH legs (authorize_device and token)
// because a secret leaking onto either one would require a Confidential
// application and break the flow on the very application the operator
// registered for it.
func TestGitLabDeviceFlow_SendsNoClientSecret(t *testing.T) {
	f := newFakeGitLab(t)
	var deviceForm, tokenForm map[string][]string
	f.route("/oauth/authorize_device", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		deviceForm = r.Form
		_, _ = w.Write([]byte(`{"device_code":"DC","user_code":"UC","verification_uri":"URI","expires_in":900,"interval":5}`))
	})
	f.route("/oauth/token", func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		tokenForm = r.Form
		_, _ = w.Write([]byte(`{"access_token":"glpat-user"}`))
	})
	f.route("/api/v4/user", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"id":1,"username":"alice"}`))
	})

	p := newTestGitLabProvider(f)
	if _, err := p.VerifyUser(context.Background(), nil); err != nil {
		t.Fatalf("VerifyUser: %v", err)
	}
	for _, tc := range []struct {
		leg  string
		form map[string][]string
	}{{"authorize_device", deviceForm}, {"token", tokenForm}} {
		if _, ok := tc.form["client_secret"]; ok {
			t.Errorf("%s body carried client_secret=%q; the device application is NON-Confidential and must send none",
				tc.leg, tc.form["client_secret"])
		}
		if got := tc.form["client_id"]; len(got) != 1 || got[0] != "device-client" {
			t.Errorf("%s body client_id = %v, want [device-client]", tc.leg, got)
		}
	}
}

// TestGitLabVerifyUser_PollStates covers each RFC 8628 poll state. GitLab
// (Doorkeeper) serves the pending/slow_down/expired states as OAuth error
// responses with HTTP 400, so each case is served with a non-2xx status —
// the very shape a status-first reader would mistake for a hard failure.
func TestGitLabVerifyUser_PollStates(t *testing.T) {
	tests := []struct {
		name       string
		pollErrors []string // errors served before the success response
		finalBody  string
		finalCode  int
		wantSubj   string
		wantErrIs  error
		wantErrSub string
	}{
		{
			name:       "authorization_pending loops then succeeds",
			pollErrors: []string{"authorization_pending", "authorization_pending"},
			finalBody:  `{"access_token":"glpat-user"}`,
			finalCode:  http.StatusOK,
			wantSubj:   "gitlab:alice",
		},
		{
			name:      "expired_token is a verification timeout",
			finalBody: `{"error":"expired_token"}`,
			finalCode: http.StatusBadRequest,
			wantErrIs: ErrVerificationTimeout,
		},
		{
			name:       "access_denied is terminal",
			finalBody:  `{"error":"access_denied"}`,
			finalCode:  http.StatusBadRequest,
			wantErrSub: "denied by user",
		},
		{
			name:       "unknown error is terminal",
			finalBody:  `{"error":"invalid_grant"}`,
			finalCode:  http.StatusBadRequest,
			wantErrSub: "invalid_grant",
		},
		{
			name:       "non-2xx with no error field is a hard failure",
			finalBody:  `{}`,
			finalCode:  http.StatusInternalServerError,
			wantErrSub: "500",
		},
		{
			name:       "2xx carrying neither token nor error is a hard failure",
			finalBody:  `{}`,
			finalCode:  http.StatusOK,
			wantErrSub: "neither access_token nor error",
		},
		{
			name:       "undecodable non-2xx body reports the status",
			finalBody:  `not json`,
			finalCode:  http.StatusBadGateway,
			wantErrSub: "502",
		},
		{
			name:       "undecodable 2xx body reports a decode failure",
			finalBody:  `not json`,
			finalCode:  http.StatusOK,
			wantErrSub: "decode gitlab device token",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeGitLab(t)
			f.route("/oauth/authorize_device", func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write([]byte(`{"device_code":"DC","user_code":"UC","verification_uri":"URI","expires_in":900,"interval":5}`))
			})
			polls := 0
			f.route("/oauth/token", func(w http.ResponseWriter, _ *http.Request) {
				if polls < len(tc.pollErrors) {
					state := tc.pollErrors[polls]
					polls++
					w.WriteHeader(http.StatusBadRequest)
					_, _ = fmt.Fprintf(w, `{"error":%q}`, state)
					return
				}
				polls++
				w.WriteHeader(tc.finalCode)
				_, _ = w.Write([]byte(tc.finalBody))
			})
			f.route("/api/v4/user", func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write([]byte(`{"id":1,"username":"alice"}`))
			})

			p := newTestGitLabProvider(f)
			subject, err := p.VerifyUser(context.Background(), nil)

			switch {
			case tc.wantErrIs != nil:
				if !errors.Is(err, tc.wantErrIs) {
					t.Fatalf("err = %v, want %v", err, tc.wantErrIs)
				}
			case tc.wantErrSub != "":
				if err == nil || !strings.Contains(err.Error(), tc.wantErrSub) {
					t.Fatalf("err = %v, want one containing %q", err, tc.wantErrSub)
				}
			default:
				if err != nil {
					t.Fatalf("VerifyUser: %v", err)
				}
				if subject != tc.wantSubj {
					t.Errorf("subject = %q, want %q", subject, tc.wantSubj)
				}
				if polls != len(tc.pollErrors)+1 {
					t.Errorf("polls = %d, want %d (the pending states must be looped, not treated as terminal)",
						polls, len(tc.pollErrors)+1)
				}
			}
		})
	}
}

// TestGitLabVerifyUser_SlowDownBumpsInterval pins the RFC 8628 back-off:
// a slow_down response must LENGTHEN the poll interval, either to the
// forge-supplied value or by the fixed increment.
func TestGitLabVerifyUser_SlowDownBumpsInterval(t *testing.T) {
	tests := []struct {
		name         string
		slowDownBody string
		want         time.Duration
	}{
		{"larger forge-supplied interval wins", `{"error":"slow_down","interval":13}`, 13 * time.Second},
		{"absent interval adds the fixed increment", `{"error":"slow_down"}`, 6*time.Second + slowDownIncrement},
		// The boundary that matters: the token endpoint is untrusted, and
		// a slow_down carrying a SMALLER positive interval must not be
		// able to shrink the delay below the one already in effect (2s <
		// the 6s in force, and below minPollInterval besides). slow_down
		// is monotone, so this falls back to the fixed increment.
		{"smaller forge-supplied interval cannot shrink the delay", `{"error":"slow_down","interval":2}`, 6*time.Second + slowDownIncrement},
		// Equal is not larger: an interval echoing the current one must
		// still cost the caller the increment, else a forge could pin the
		// delay flat while signalling slow_down forever.
		{"equal forge-supplied interval still adds the increment", `{"error":"slow_down","interval":6}`, 6*time.Second + slowDownIncrement},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeGitLab(t)
			f.route("/oauth/authorize_device", func(w http.ResponseWriter, _ *http.Request) {
				// interval 6 is ABOVE minPollInterval, so the back-off
				// arithmetic below is the forge value's, not the floor's.
				_, _ = w.Write([]byte(`{"device_code":"DC","user_code":"UC","verification_uri":"URI","expires_in":900,"interval":6}`))
			})
			polls := 0
			f.route("/oauth/token", func(w http.ResponseWriter, _ *http.Request) {
				polls++
				if polls == 1 {
					w.WriteHeader(http.StatusBadRequest)
					_, _ = w.Write([]byte(tc.slowDownBody))
					return
				}
				_, _ = w.Write([]byte(`{"access_token":"glpat-user"}`))
			})
			f.route("/api/v4/user", func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write([]byte(`{"id":1,"username":"alice"}`))
			})

			p := newTestGitLabProvider(f)
			// Do NOT override the forge interval: the back-off must be
			// computed from the forge value, not from a test seam.
			p.pollInterval = 0
			var slept []time.Duration
			p.sleep = func(_ context.Context, d time.Duration) error {
				slept = append(slept, d)
				return nil
			}

			if _, err := p.VerifyUser(context.Background(), nil); err != nil {
				t.Fatalf("VerifyUser: %v", err)
			}
			if len(slept) < 2 {
				t.Fatalf("slept = %v, want at least two waits (pre-poll and post-slow_down)", slept)
			}
			if slept[1] <= slept[0] {
				t.Errorf("interval did not grow after slow_down: %v then %v", slept[0], slept[1])
			}
			if slept[1] != tc.want {
				t.Errorf("post-slow_down interval = %v, want %v", slept[1], tc.want)
			}
		})
	}
}

// TestGitLabVerifyUser_PollIntervalFloor covers the busy-poll guard: a
// device-code response that omits `interval` yields 0, and a ctx-aware
// sleep of 0 returns immediately, so an authorization_pending loop would
// hammer the token endpoint until expiry. The floor supplies the wait.
//
// Asserted on the RECORDED sleep durations, not on elapsed wall clock, so
// the test carries no timing bound to slip under CI load.
func TestGitLabVerifyUser_PollIntervalFloor(t *testing.T) {
	f := newFakeGitLab(t)
	f.route("/oauth/authorize_device", func(w http.ResponseWriter, _ *http.Request) {
		// `interval` omitted entirely → device.Interval == 0.
		_, _ = w.Write([]byte(`{"device_code":"DC","user_code":"UC","verification_uri":"URI","expires_in":900}`))
	})
	f.route("/oauth/token", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"access_token":"glpat-user"}`))
	})
	f.route("/api/v4/user", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"id":1,"username":"alice"}`))
	})

	p := newTestGitLabProvider(f)
	p.pollInterval = 0 // production leaves this 0; the floor must supply the wait
	var slept []time.Duration
	p.sleep = func(_ context.Context, d time.Duration) error {
		slept = append(slept, d)
		return nil
	}

	if _, err := p.VerifyUser(context.Background(), nil); err != nil {
		t.Fatalf("VerifyUser: %v", err)
	}
	if len(slept) == 0 {
		t.Fatal("expected at least one sleep before polling the token endpoint")
	}
	if slept[0] < minPollInterval {
		t.Errorf("poll interval = %v, want >= %v (floor guards a 0/omitted forge interval)", slept[0], minPollInterval)
	}
}

// TestGitLabVerifyUser_ContextCancelled covers the top-of-loop
// cancellation guard: a human who abandons the browser leg (the CLI's ctrl-C)
// must surface as a verification timeout, not as a transport error, and
// must not poll the token endpoint again. The ctx is cancelled from the
// prompt callback — the moment the flow hands off to the human — so the
// device-code request itself still succeeds and the RESULT under test is
// the loop guard rather than a failed HTTP call.
func TestGitLabVerifyUser_ContextCancelled(t *testing.T) {
	f := newFakeGitLab(t)
	f.route("/oauth/authorize_device", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"device_code":"DC","user_code":"UC","verification_uri":"URI","expires_in":900,"interval":5}`))
	})
	f.route("/oauth/token", func(http.ResponseWriter, *http.Request) {
		t.Error("token endpoint polled despite a cancelled context")
	})

	p := newTestGitLabProvider(f)
	p.sleep = sleepCtx // the real ctx-aware wait, not the instant stub
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	_, err := p.VerifyUser(ctx, func(string, string) { cancel() })
	if !errors.Is(err, ErrVerificationTimeout) {
		t.Fatalf("err = %v, want ErrVerificationTimeout", err)
	}
}

// TestGitLabVerifyUser_DeadlinePassed covers the expiry branch: a device
// code whose expires_in has already elapsed times out before any poll.
func TestGitLabVerifyUser_DeadlinePassed(t *testing.T) {
	f := newFakeGitLab(t)
	f.route("/oauth/authorize_device", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"device_code":"DC","user_code":"UC","verification_uri":"URI","expires_in":0,"interval":5}`))
	})
	f.route("/oauth/token", func(http.ResponseWriter, *http.Request) {
		t.Error("token endpoint polled after the device code expired")
	})

	p := newTestGitLabProvider(f)
	if _, err := p.VerifyUser(context.Background(), nil); !errors.Is(err, ErrVerificationTimeout) {
		t.Fatalf("err = %v, want ErrVerificationTimeout", err)
	}
}

// TestGitLabVerifyUser_SleepInterrupted covers the sleep-error branch: a
// ctx-aware wait that returns an error aborts the poll loop as a timeout.
func TestGitLabVerifyUser_SleepInterrupted(t *testing.T) {
	f := newFakeGitLab(t)
	f.route("/oauth/authorize_device", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"device_code":"DC","user_code":"UC","verification_uri":"URI","expires_in":900,"interval":5}`))
	})
	f.route("/oauth/token", func(http.ResponseWriter, *http.Request) {
		t.Error("token endpoint polled after an interrupted wait")
	})

	p := newTestGitLabProvider(f)
	p.sleep = func(context.Context, time.Duration) error { return context.DeadlineExceeded }

	if _, err := p.VerifyUser(context.Background(), nil); !errors.Is(err, ErrVerificationTimeout) {
		t.Fatalf("err = %v, want ErrVerificationTimeout", err)
	}
}

func TestGitLabVerifyUser_DeviceCodeFailures(t *testing.T) {
	tests := []struct {
		name    string
		status  int
		body    string
		wantSub string
	}{
		{"non-2xx", http.StatusUnauthorized, `{"error":"invalid_client"}`, "gitlab device code"},
		{"undecodable body", http.StatusOK, `not json`, "decode gitlab device code"},
		{"no device_code", http.StatusOK, `{"user_code":"UC"}`, "carried no device_code"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeGitLab(t)
			f.route("/oauth/authorize_device", func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			})
			p := newTestGitLabProvider(f)
			_, err := p.VerifyUser(context.Background(), nil)
			if err == nil || !strings.Contains(err.Error(), tc.wantSub) {
				t.Fatalf("err = %v, want one containing %q", err, tc.wantSub)
			}
		})
	}
}

// ---------------------------------------------------------------------
// VerifyAccessToken — server-side re-verification
// ---------------------------------------------------------------------

func TestGitLabVerifyAccessToken(t *testing.T) {
	tests := []struct {
		name     string
		token    string
		status   int
		body     string
		want     string
		wantErr  string
		noServer bool
	}{
		{name: "happy path", token: "glpat-x", status: http.StatusOK, body: `{"id":7,"username":"alice"}`, want: "gitlab:alice"},
		{name: "username is trimmed", token: "glpat-x", status: http.StatusOK, body: `{"id":7,"username":"  alice  "}`, want: "gitlab:alice"},
		{name: "non-200 is an error", token: "glpat-x", status: http.StatusUnauthorized, body: `{"message":"401 Unauthorized"}`, wantErr: "gitlab get user: 401"},
		{name: "missing username is an error", token: "glpat-x", status: http.StatusOK, body: `{"id":7}`, wantErr: "carried no id/username"},
		{name: "missing id is an error", token: "glpat-x", status: http.StatusOK, body: `{"username":"alice"}`, wantErr: "carried no id/username"},
		{name: "undecodable body is an error", token: "glpat-x", status: http.StatusOK, body: `not json`, wantErr: "decode gitlab user"},
		// The empty-token case is served by a HAPPY handler on purpose:
		// if the pre-flight refusal were removed, the call would SUCCEED
		// and resolve a subject, so the counterfactual RED lands on the
		// behavioral assertion below rather than on a fixture artifact.
		{name: "empty token is refused before any call", token: "", status: http.StatusOK,
			body: `{"id":7,"username":"alice"}`, wantErr: "access token is empty", noServer: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeGitLab(t)
			f.route("/api/v4/user", func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			})
			p := newTestGitLabProvider(f)

			got, err := p.VerifyAccessToken(context.Background(), tc.token)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err = %v, want one containing %q", err, tc.wantErr)
				}
				if tc.noServer && f.requestCount() != 0 {
					t.Errorf("made %d request(s); an empty token must be refused before any HTTP call", f.requestCount())
				}
				return
			}
			if err != nil {
				t.Fatalf("VerifyAccessToken: %v", err)
			}
			if got != tc.want {
				t.Errorf("subject = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestGitLabVerifyAccessToken_DeploymentCredentialNotSent is BINDING
// CONDITION 1 (E66.4, concern e807cc3b).
//
// VerifyAccessToken's ONLY job is to prove the caller holds a valid GitLab
// token. Attaching the deployment credential (FISHHAWKD_GITLAB_TOKEN, as
// PRIVATE-TOKEN) to that same request is an authentication-bypass shape:
// if GitLab honours the deployment credential, an invalid or revoked
// submitted token resolves as the deployment-token user and the mint
// issues a fishhawkd token for an identity the caller never proved.
//
// BOTH sub-cases matter, and the second is the load-bearing one. The
// header-absence assertion can pass for the wrong reason (a provider whose
// accessor simply happened not to run). The 401/200 split cannot: the fake
// answers 401 to the submitted bearer and 200 to the deployment
// credential, so a provider that sent both — and a GitLab that honoured
// either — would resolve a subject. The call must FAIL.
func TestGitLabVerifyAccessToken_DeploymentCredentialNotSent(t *testing.T) {
	t.Run("no PRIVATE-TOKEN header on the verification request", func(t *testing.T) {
		f := newFakeGitLab(t)
		var gotAuth, gotPrivate string
		f.route("/api/v4/user", func(w http.ResponseWriter, r *http.Request) {
			gotAuth = r.Header.Get("Authorization")
			gotPrivate = r.Header.Get("PRIVATE-TOKEN")
			_, _ = w.Write([]byte(`{"id":7,"username":"alice"}`))
		})

		p := newTestGitLabProvider(f)
		// Constructed WITH a non-empty deployment credential: the point is
		// that a configured credential is still not attached here.
		p.token = func(context.Context) (string, error) { return "deployment-token", nil }

		subject, err := p.VerifyAccessToken(context.Background(), "submitted-token")
		if err != nil {
			t.Fatalf("VerifyAccessToken: %v", err)
		}
		if subject != "gitlab:alice" {
			t.Errorf("subject = %q, want gitlab:alice", subject)
		}
		if gotAuth != "Bearer submitted-token" {
			t.Errorf("Authorization = %q, want Bearer submitted-token", gotAuth)
		}
		if gotPrivate != "" {
			t.Errorf("PRIVATE-TOKEN = %q, want it ABSENT: the deployment credential must never ride on the "+
				"request that verifies the caller's own token", gotPrivate)
		}
	})

	t.Run("discriminating: forge accepts the deployment credential, call must fail", func(t *testing.T) {
		f := newFakeGitLab(t)
		f.route("/api/v4/user", func(w http.ResponseWriter, r *http.Request) {
			// A GitLab that honours the deployment credential: the
			// PRIVATE-TOKEN header alone is enough to resolve a user,
			// while the submitted bearer is invalid.
			if r.Header.Get("PRIVATE-TOKEN") == "deployment-token" {
				_, _ = w.Write([]byte(`{"id":1,"username":"deployment-bot"}`))
				return
			}
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"message":"401 Unauthorized"}`))
		})

		p := newTestGitLabProvider(f)
		p.token = func(context.Context) (string, error) { return "deployment-token", nil }

		subject, err := p.VerifyAccessToken(context.Background(), "revoked-token")
		if err == nil {
			t.Fatalf("VerifyAccessToken resolved %q for a REVOKED submitted token: the deployment credential "+
				"authenticated the request, which is the authentication bypass binding condition 1 forbids", subject)
		}
		if subject != "" {
			t.Errorf("subject = %q on failure, want empty", subject)
		}
		if !strings.Contains(err.Error(), "401") {
			t.Errorf("err = %v, want the forge's 401 surfaced", err)
		}
	})
}

// TestGitLabVerifyAccessToken_RateLimited pins that a rate-limit signal on
// the verification read is reported as ErrRateLimited (back off) rather
// than as a hard authentication failure.
func TestGitLabVerifyAccessToken_RateLimited(t *testing.T) {
	f := newFakeGitLab(t)
	f.route("/api/v4/user", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Retry-After", "60")
		w.WriteHeader(http.StatusTooManyRequests)
	})
	p := newTestGitLabProvider(f)

	if _, err := p.VerifyAccessToken(context.Background(), "glpat-x"); !errors.Is(err, ErrRateLimited) {
		t.Fatalf("err = %v, want ErrRateLimited", err)
	}
}

// ---------------------------------------------------------------------
// PermissionLevel / ResolveMembership — the bounded paginated walk
// ---------------------------------------------------------------------

func TestGitLabPermissionLevel_AccessLevelMapping(t *testing.T) {
	tests := []struct {
		name  string
		level int
		want  Permission
	}{
		{"guest", gitLabAccessGuest, PermissionRead},
		{"reporter", gitLabAccessReporter, PermissionTriage},
		{"developer", gitLabAccessDeveloper, PermissionWrite},
		{"maintainer", gitLabAccessMaintainer, PermissionMaintain},
		{"owner", gitLabAccessOwner, PermissionAdmin},
		{"minimal access denies", 5, PermissionNone},
		{"unknown future level denies", 35, PermissionNone},
		{"zero denies", 0, PermissionNone},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeGitLab(t)
			f.route("/api/v4/projects/owner%2Frepo/members/all", func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write([]byte(membersPage(gitLabMember{Username: "alice", AccessLevel: tc.level})))
			})
			p := newTestGitLabProvider(f)

			got, err := p.PermissionLevel(context.Background(), "owner/repo", "gitlab:alice")
			if err != nil {
				t.Fatalf("PermissionLevel: %v", err)
			}
			if got != tc.want {
				t.Errorf("PermissionLevel = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestGitLabPermissionLevel_GuestDoesNotSatisfyWrite pins the direction
// the /members/all choice fails in. /members/all includes members
// INHERITED from an ancestor group, which is over-permissive relative to
// /members; the mint's OperatorMinPermission gate (default write) is the
// backstop, so a Guest-level member must NOT satisfy it.
func TestGitLabPermissionLevel_GuestDoesNotSatisfyWrite(t *testing.T) {
	if mapGitLabAccessLevel(gitLabAccessGuest).AtLeast(PermissionWrite) {
		t.Error("guest satisfied the default write minimum; the mint gate would admit a read-only member")
	}
}

func TestGitLabPermissionLevel_NotAMember(t *testing.T) {
	f := newFakeGitLab(t)
	f.route("/api/v4/projects/owner%2Frepo/members/all", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(membersPage(gitLabMember{Username: "bob", AccessLevel: gitLabAccessOwner})))
	})
	p := newTestGitLabProvider(f)

	got, err := p.PermissionLevel(context.Background(), "owner/repo", "gitlab:alice")
	if err != nil {
		t.Fatalf("PermissionLevel: %v", err)
	}
	if got != PermissionNone {
		t.Errorf("PermissionLevel = %q, want none", got)
	}
}

// TestGitLabMembers_NotVisible covers the 404 branch on BOTH readers: a
// project or group the caller cannot see (GitLab answers 404, not 403,
// for an invisible resource — including the anonymous-read degrade when no
// deployment credential is configured) is a clean deny, not an error.
func TestGitLabMembers_NotVisible(t *testing.T) {
	f := newFakeGitLab(t)
	notFound := func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"message":"404 Project Not Found"}`))
	}
	f.route("/api/v4/projects/owner%2Frepo/members/all", notFound)
	f.route("/api/v4/groups/acme/members/all", notFound)
	p := newTestGitLabProvider(f)

	perm, err := p.PermissionLevel(context.Background(), "owner/repo", "gitlab:alice")
	if err != nil {
		t.Errorf("PermissionLevel err = %v, want nil (404 is a clean deny)", err)
	}
	if perm != PermissionNone {
		t.Errorf("PermissionLevel = %q, want none", perm)
	}

	member, err := p.ResolveMembership(context.Background(), "acme", "gitlab:alice")
	if err != nil {
		t.Errorf("ResolveMembership err = %v, want nil (404 is a clean deny)", err)
	}
	if member {
		t.Error("ResolveMembership = true, want false")
	}
}

func TestGitLabMembers_RateLimited(t *testing.T) {
	tests := []struct {
		name    string
		status  int
		headers map[string]string
		wantRL  bool
	}{
		{"429 is unambiguously the rate limiter", http.StatusTooManyRequests, nil, true},
		{"429 with headers", http.StatusTooManyRequests, map[string]string{"Retry-After": "30"}, true},
		{"403 with Retry-After", http.StatusForbidden, map[string]string{"Retry-After": "30"}, true},
		{"403 with RateLimit-Remaining 0", http.StatusForbidden, map[string]string{"RateLimit-Remaining": "0"}, true},
		{"bare 403 is a plain forbidden, not a rate limit", http.StatusForbidden, nil, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeGitLab(t)
			f.route("/api/v4/projects/owner%2Frepo/members/all", func(w http.ResponseWriter, _ *http.Request) {
				for k, v := range tc.headers {
					w.Header().Set(k, v)
				}
				w.WriteHeader(tc.status)
			})
			p := newTestGitLabProvider(f)

			_, err := p.PermissionLevel(context.Background(), "owner/repo", "gitlab:alice")
			if err == nil {
				t.Fatal("PermissionLevel err = nil, want an error")
			}
			if got := errors.Is(err, ErrRateLimited); got != tc.wantRL {
				t.Errorf("errors.Is(err, ErrRateLimited) = %v, want %v (err = %v)", got, tc.wantRL, err)
			}
		})
	}
}

func TestGitLabMembers_ServerError(t *testing.T) {
	f := newFakeGitLab(t)
	f.route("/api/v4/projects/owner%2Frepo/members/all", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"message":"boom"}`))
	})
	p := newTestGitLabProvider(f)

	if _, err := p.PermissionLevel(context.Background(), "owner/repo", "gitlab:alice"); err == nil ||
		!strings.Contains(err.Error(), "gitlab members: 500") {
		t.Fatalf("err = %v, want a 'gitlab members: 500' error", err)
	}
}

func TestGitLabMembers_UndecodableBody(t *testing.T) {
	f := newFakeGitLab(t)
	f.route("/api/v4/projects/owner%2Frepo/members/all", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`not json`))
	})
	p := newTestGitLabProvider(f)

	if _, err := p.PermissionLevel(context.Background(), "owner/repo", "gitlab:alice"); err == nil ||
		!strings.Contains(err.Error(), "decode gitlab members") {
		t.Fatalf("err = %v, want a decode error", err)
	}
}

// TestGitLabMembers_OversizedPage_Errors pins the byte cap: an oversized
// page body is REJECTED, never truncated-and-parsed. A truncated page is a
// partial member set, and answering an authorization question from one is
// the same silent wrong answer the walk exists to prevent.
func TestGitLabMembers_OversizedPage_Errors(t *testing.T) {
	f := newFakeGitLab(t)
	f.route("/api/v4/projects/owner%2Frepo/members/all", func(w http.ResponseWriter, _ *http.Request) {
		// A valid JSON array that is comfortably over the cap. Its first
		// row is an exact match, so a truncate-and-parse implementation
		// could plausibly still answer — the assertion is that it errors.
		_, _ = w.Write([]byte(`[{"username":"alice","access_level":50,"note":"`))
		filler := strings.Repeat("x", 64<<10)
		for written := 0; written <= gitLabMaxMemberPageBytes; written += len(filler) {
			_, _ = w.Write([]byte(filler))
		}
		_, _ = w.Write([]byte(`"}]`))
	})
	p := newTestGitLabProvider(f)

	got, err := p.PermissionLevel(context.Background(), "owner/repo", "gitlab:alice")
	if err == nil || !strings.Contains(err.Error(), "exceeded") {
		t.Fatalf("PermissionLevel = (%q, %v), want an 'exceeded … bytes' error", got, err)
	}
	if got != PermissionNone {
		t.Errorf("PermissionLevel = %q on error, want none", got)
	}
}

// TestGitLabPermissionLevel_PageCapExceeded_Errors pins that the page
// bound is LOUD. A server that never stops advertising a next page must
// produce an error, not a silent PermissionNone: a truncated walk that
// denies is an invisible wrong-authorization answer, which is exactly what
// the bounded walk exists to prevent.
func TestGitLabPermissionLevel_PageCapExceeded_Errors(t *testing.T) {
	f := newFakeGitLab(t)
	pages := 0
	f.route("/api/v4/projects/owner%2Frepo/members/all", func(w http.ResponseWriter, _ *http.Request) {
		pages++
		// Always a FULL page that never contains the exact match, and
		// always advertising another — the pathological server.
		w.Header().Set("Link", `<https://gitlab.example.com/next>; rel="next"`)
		_, _ = w.Write([]byte(prefixCollisionPage("alice")))
	})
	p := newTestGitLabProvider(f)

	got, err := p.PermissionLevel(context.Background(), "owner/repo", "gitlab:alice")
	if err == nil {
		t.Fatalf("PermissionLevel = %q with nil error; an exhausted page bound must ERROR, never look "+
			"like a clean 'not a member'", got)
	}
	if !strings.Contains(err.Error(), strconv.Itoa(gitLabMaxMemberPages)+" pages") {
		t.Errorf("err = %v, want one naming the %d-page bound", err, gitLabMaxMemberPages)
	}
	if pages != gitLabMaxMemberPages {
		t.Errorf("walked %d pages, want exactly %d before erroring", pages, gitLabMaxMemberPages)
	}
}

// TestGitLabPermissionLevel_ExactMatchOnSecondPage is the CONSTRAINT-9
// core assertion for the project reader: the exact member sits on page 2,
// behind a full page of prefix collisions. A first-page-only read denies a
// real member — the invisible wrong-authorization answer.
//
// Both next-page detections are covered: the RFC 5988 Link header when the
// deployment sends one, and the full-page fallback when it (or a proxy)
// strips Link headers.
func TestGitLabPermissionLevel_ExactMatchOnSecondPage(t *testing.T) {
	for _, tc := range []struct {
		name     string
		sendLink bool
	}{
		{"Link rel=next present", true},
		{"Link absent — full-page fallback decides", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeGitLab(t)
			var pagesServed []string
			f.route("/api/v4/projects/owner%2Frepo/members/all", func(w http.ResponseWriter, r *http.Request) {
				page := r.URL.Query().Get("page")
				pagesServed = append(pagesServed, page)
				if page == "1" {
					if tc.sendLink {
						w.Header().Set("Link", `<https://gitlab.example.com/x?page=2>; rel="next"`)
					}
					// A FULL page of prefix collisions, so the fallback
					// ("the page was full") also says another follows.
					_, _ = w.Write([]byte(prefixCollisionPage("alice")))
					return
				}
				_, _ = w.Write([]byte(membersPage(gitLabMember{Username: "alice", AccessLevel: gitLabAccessDeveloper})))
			})
			p := newTestGitLabProvider(f)

			got, err := p.PermissionLevel(context.Background(), "owner/repo", "gitlab:alice")
			if err != nil {
				t.Fatalf("PermissionLevel: %v", err)
			}
			if got != PermissionWrite {
				t.Errorf("PermissionLevel = %q, want write — the exact member on page 2 was not reached", got)
			}
			if len(pagesServed) < 2 {
				t.Errorf("pages served = %v, want the walk to reach page 2", pagesServed)
			}
		})
	}
}

// TestGitLabResolveMembership_ExactMatchOnSecondPage is the CONSTRAINT-9
// core assertion for the group reader — the same shape as the project one,
// proving the shared findMemberExact helper serves both.
func TestGitLabResolveMembership_ExactMatchOnSecondPage(t *testing.T) {
	for _, tc := range []struct {
		name     string
		sendLink bool
	}{
		{"Link rel=next present", true},
		{"Link absent — full-page fallback decides", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeGitLab(t)
			f.route("/api/v4/groups/acme%2Fplatform/members/all", func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Query().Get("page") == "1" {
					if tc.sendLink {
						w.Header().Set("Link", `<https://gitlab.example.com/x?page=2>; rel="next"`)
					}
					_, _ = w.Write([]byte(prefixCollisionPage("alice")))
					return
				}
				_, _ = w.Write([]byte(membersPage(gitLabMember{Username: "alice", AccessLevel: gitLabAccessReporter})))
			})
			p := newTestGitLabProvider(f)

			member, err := p.ResolveMembership(context.Background(), "acme/platform", "gitlab:alice")
			if err != nil {
				t.Fatalf("ResolveMembership: %v", err)
			}
			if !member {
				t.Error("ResolveMembership = false, want true — the exact member on page 2 was not reached")
			}
		})
	}
}

// TestGitLabPermissionLevel_ExactMatchBeforePrefixCollision pins the
// client-side exact comparison. GitLab's `query=` is a PARTIAL filter, so
// an implementation that trusted it — taking the filtered page's first row
// — would answer with "alice-1"'s Owner rather than "alice"'s Guest. The
// collision is placed BEFORE the exact match on the same page so the
// short-circuit is the failure, not the pagination.
func TestGitLabPermissionLevel_ExactMatchBeforePrefixCollision(t *testing.T) {
	f := newFakeGitLab(t)
	f.route("/api/v4/projects/owner%2Frepo/members/all", func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Query().Get("query"); got != "alice" {
			t.Errorf("query = %q, want alice (the narrowing filter is still sent)", got)
		}
		_, _ = w.Write([]byte(membersPage(
			gitLabMember{Username: "alice-1", AccessLevel: gitLabAccessOwner},
			gitLabMember{Username: "alicia", AccessLevel: gitLabAccessMaintainer},
			gitLabMember{Username: "alice", AccessLevel: gitLabAccessGuest},
			gitLabMember{Username: "alice-2", AccessLevel: gitLabAccessOwner},
		)))
	})
	p := newTestGitLabProvider(f)

	got, err := p.PermissionLevel(context.Background(), "owner/repo", "gitlab:alice")
	if err != nil {
		t.Fatalf("PermissionLevel: %v", err)
	}
	if got != PermissionRead {
		t.Errorf("PermissionLevel = %q, want read (alice is Guest); a prefix collision was matched instead", got)
	}
}

// TestGitLabPermissionLevel_ExactMatchIsCaseSensitive pins that the
// comparison does not fold case: GitLab usernames are distinct accounts.
func TestGitLabPermissionLevel_ExactMatchIsCaseSensitive(t *testing.T) {
	f := newFakeGitLab(t)
	f.route("/api/v4/projects/owner%2Frepo/members/all", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(membersPage(gitLabMember{Username: "Alice", AccessLevel: gitLabAccessOwner})))
	})
	p := newTestGitLabProvider(f)

	got, err := p.PermissionLevel(context.Background(), "owner/repo", "gitlab:alice")
	if err != nil {
		t.Fatalf("PermissionLevel: %v", err)
	}
	if got != PermissionNone {
		t.Errorf("PermissionLevel = %q, want none — %q is a different account from %q", got, "Alice", "alice")
	}
}

// TestGitLabMembers_ForeignSubject_NoNetworkCall pins the foreign-provider
// guard on BOTH readers. A "github:alice" and a "gitlab:alice" are
// different humans, so resolving one against GitLab would answer the wrong
// authorization question. The assertion is on the REQUEST COUNT of a
// reachable in-test server: with the guard removed the fake is hit, so the
// counterfactual lands on this assertion and not on a connection error.
func TestGitLabMembers_ForeignSubject_NoNetworkCall(t *testing.T) {
	subjects := []struct {
		name    string
		subject string
	}{
		{"github-qualified", "github:alice"},
		{"unqualified bare login", "alice"},
		{"empty", ""},
		{"prefix with an empty login", "gitlab:"},
		{"prefix with a whitespace-only login", "gitlab:   "},
		{"mcp run subject", "mcp:run:abc123"},
	}
	for _, tc := range subjects {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeGitLab(t)
			f.route("/api/v4/projects/owner%2Frepo/members/all", func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write([]byte(membersPage(gitLabMember{Username: "alice", AccessLevel: gitLabAccessOwner})))
			})
			f.route("/api/v4/groups/acme/members/all", func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write([]byte(membersPage(gitLabMember{Username: "alice", AccessLevel: gitLabAccessOwner})))
			})
			p := newTestGitLabProvider(f)

			perm, err := p.PermissionLevel(context.Background(), "owner/repo", tc.subject)
			if err != nil || perm != PermissionNone {
				t.Errorf("PermissionLevel(%q) = (%q, %v), want (none, nil)", tc.subject, perm, err)
			}
			member, err := p.ResolveMembership(context.Background(), "acme", tc.subject)
			if err != nil || member {
				t.Errorf("ResolveMembership(%q) = (%v, %v), want (false, nil)", tc.subject, member, err)
			}
			if n := f.requestCount(); n != 0 {
				t.Errorf("made %d request(s) for subject %q; a subject this provider must not resolve "+
					"has to deny with ZERO network calls", n, tc.subject)
			}
		})
	}
}

// TestGitLabResolveMembership_NonPositiveAccessLevel pins that a member
// row carrying a non-positive access level is not a membership.
func TestGitLabResolveMembership_NonPositiveAccessLevel(t *testing.T) {
	f := newFakeGitLab(t)
	f.route("/api/v4/groups/acme/members/all", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(membersPage(gitLabMember{Username: "alice", AccessLevel: 0})))
	})
	p := newTestGitLabProvider(f)

	member, err := p.ResolveMembership(context.Background(), "acme", "gitlab:alice")
	if err != nil {
		t.Fatalf("ResolveMembership: %v", err)
	}
	if member {
		t.Error("ResolveMembership = true for a zero access level, want false")
	}
}

// ---------------------------------------------------------------------
// Deployment credential on the members reads
// ---------------------------------------------------------------------

// TestGitLabMembers_DeploymentCredentialAttached is the positive half of
// binding condition 1's confinement: the credential the verification read
// must NOT carry is exactly the one the members reads MUST carry.
func TestGitLabMembers_DeploymentCredentialAttached(t *testing.T) {
	f := newFakeGitLab(t)
	var gotPrivate, gotAuth string
	f.route("/api/v4/projects/owner%2Frepo/members/all", func(w http.ResponseWriter, r *http.Request) {
		gotPrivate = r.Header.Get("PRIVATE-TOKEN")
		gotAuth = r.Header.Get("Authorization")
		_, _ = w.Write([]byte(membersPage(gitLabMember{Username: "alice", AccessLevel: gitLabAccessDeveloper})))
	})
	p := newTestGitLabProvider(f)
	p.token = func(context.Context) (string, error) { return "deployment-token", nil }

	got, err := p.PermissionLevel(context.Background(), "owner/repo", "gitlab:alice")
	if err != nil {
		t.Fatalf("PermissionLevel: %v", err)
	}
	if got != PermissionWrite {
		t.Errorf("PermissionLevel = %q, want write", got)
	}
	if gotPrivate != "deployment-token" {
		t.Errorf("PRIVATE-TOKEN = %q, want deployment-token", gotPrivate)
	}
	if gotAuth != "" {
		t.Errorf("Authorization = %q, want it absent on a members read", gotAuth)
	}
}

// TestGitLabMembers_CrossOriginRedirect_RefusedAndCredentialNotLeaked pins
// the redirect guard on the credential-bearing members read.
//
// Go's redirect handling strips Authorization/Cookie on a cross-host hop
// but copies a NONSTANDARD header verbatim — and the deployment credential
// rides in exactly such a header, PRIVATE-TOKEN. Without the guard, a
// GitLab (or a proxy in front of it) answering the members read with a 302
// to a host it controls receives FISHHAWKD_GITLAB_TOKEN in full, and its
// reply — here an Owner grant — is accepted as the authorization answer.
//
// The redirect target is a REAL in-test server, not an unreachable
// address: an unreachable target would fail the call through a connection
// error whether or not the guard exists, which would make the test pass
// for the wrong reason. This one is reachable and eager to answer.
func TestGitLabMembers_CrossOriginRedirect_RefusedAndCredentialNotLeaked(t *testing.T) {
	var attackerHits int
	var attackerSawToken string
	attacker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attackerHits++
		attackerSawToken = r.Header.Get("PRIVATE-TOKEN")
		// Answer the authorization question wrongly-in-our-favour, so a
		// followed redirect is visible as a granted permission and not
		// merely as a transport error.
		_, _ = w.Write([]byte(membersPage(gitLabMember{Username: "alice", AccessLevel: gitLabAccessOwner})))
	}))
	t.Cleanup(attacker.Close)

	f := newFakeGitLab(t)
	f.route("/api/v4/projects/owner%2Frepo/members/all", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, attacker.URL+"/api/v4/projects/owner%2Frepo/members/all", http.StatusFound)
	})
	p := newTestGitLabProvider(f)
	p.token = func(context.Context) (string, error) { return "deployment-token", nil }

	got, err := p.PermissionLevel(context.Background(), "owner/repo", "gitlab:alice")
	if err == nil {
		t.Fatalf("PermissionLevel = %q, want an error refusing the cross-origin redirect", got)
	}
	if got != PermissionNone {
		t.Errorf("PermissionLevel = %q, want none on a refused redirect", got)
	}
	if attackerHits != 0 {
		t.Errorf("redirect target received %d requests, want 0", attackerHits)
	}
	if attackerSawToken != "" {
		t.Errorf("redirect target saw PRIVATE-TOKEN = %q, want the credential never to leave the origin", attackerSawToken)
	}
}

// TestGitLabVerifyAccessToken_CrossOriginRedirect_Refused covers the same
// guard on the verification read. The stdlib would drop the bearer on the
// hop, so the leak here is not the credential but the ANSWER: an
// attacker-controlled target that replies with a valid-looking user object
// would dictate the subject the mint issues a fishhawkd token for.
func TestGitLabVerifyAccessToken_CrossOriginRedirect_Refused(t *testing.T) {
	attacker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"id":99,"username":"attacker"}`))
	}))
	t.Cleanup(attacker.Close)

	f := newFakeGitLab(t)
	f.route("/api/v4/user", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, attacker.URL+"/api/v4/user", http.StatusFound)
	})
	p := newTestGitLabProvider(f)

	subject, err := p.VerifyAccessToken(context.Background(), "glpat-user")
	if err == nil {
		t.Fatalf("VerifyAccessToken = %q, want an error refusing the cross-origin redirect", subject)
	}
	if subject != "" {
		t.Errorf("subject = %q, want empty — a redirected identity read must not resolve a subject", subject)
	}
}

// TestGitLabMembers_SameOriginRedirect_Followed is the other half of the
// guard: it refuses a change of ORIGIN, not redirects as such, so a
// same-origin hop (the shape a GitLab deployment actually serves for a
// canonicalised path) still completes and still carries the credential.
func TestGitLabMembers_SameOriginRedirect_Followed(t *testing.T) {
	f := newFakeGitLab(t)
	var gotPrivate string
	f.route("/api/v4/projects/owner%2Frepo/members/all", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/api/v4/projects/owner%2Frepo/members/all/", http.StatusFound)
	})
	f.route("/api/v4/projects/owner%2Frepo/members/all/", func(w http.ResponseWriter, r *http.Request) {
		gotPrivate = r.Header.Get("PRIVATE-TOKEN")
		_, _ = w.Write([]byte(membersPage(gitLabMember{Username: "alice", AccessLevel: gitLabAccessDeveloper})))
	})
	p := newTestGitLabProvider(f)
	p.token = func(context.Context) (string, error) { return "deployment-token", nil }

	got, err := p.PermissionLevel(context.Background(), "owner/repo", "gitlab:alice")
	if err != nil {
		t.Fatalf("PermissionLevel: %v", err)
	}
	if got != PermissionWrite {
		t.Errorf("PermissionLevel = %q, want write across a same-origin redirect", got)
	}
	if gotPrivate != "deployment-token" {
		t.Errorf("PRIVATE-TOKEN after same-origin redirect = %q, want deployment-token", gotPrivate)
	}
}

// TestGitLabOrigin covers the origin comparison the redirect guard rests
// on: the default port is not a different origin, case is not a different
// origin, and a scheme downgrade IS.
func TestGitLabOrigin(t *testing.T) {
	tests := []struct {
		raw  string
		want string
	}{
		{"https://gitlab.example.com/api/v4/user", "https://gitlab.example.com"},
		{"https://gitlab.example.com:443/api/v4/user", "https://gitlab.example.com"},
		{"https://GitLab.Example.COM/api/v4/user", "https://gitlab.example.com"},
		{"http://gitlab.example.com:80/api/v4/user", "http://gitlab.example.com"},
		{"http://gitlab.example.com/api/v4/user", "http://gitlab.example.com"},
		{"https://gitlab.example.com:8443/api/v4/user", "https://gitlab.example.com:8443"},
	}
	for _, tc := range tests {
		u, err := url.Parse(tc.raw)
		if err != nil {
			t.Fatalf("parse %q: %v", tc.raw, err)
		}
		if got := gitLabOrigin(u); got != tc.want {
			t.Errorf("gitLabOrigin(%q) = %q, want %q", tc.raw, got, tc.want)
		}
	}
	// A scheme downgrade on the same host is a DIFFERENT origin, so the
	// credential is not sent in clear.
	secure, _ := url.Parse("https://gitlab.example.com/x")
	plain, _ := url.Parse("http://gitlab.example.com/x")
	if gitLabOrigin(secure) == gitLabOrigin(plain) {
		t.Errorf("https and http on one host compared equal; a scheme downgrade must be a different origin")
	}
}

// TestGitLabMembers_AnonymousWhenUnconfigured covers the nil-accessor
// degrade: no auth header at all, which on a private project draws the
// 404 that maps to PermissionNone.
func TestGitLabMembers_AnonymousWhenUnconfigured(t *testing.T) {
	f := newFakeGitLab(t)
	var sawAuth bool
	f.route("/api/v4/projects/owner%2Frepo/members/all", func(w http.ResponseWriter, r *http.Request) {
		sawAuth = r.Header.Get("PRIVATE-TOKEN") != "" || r.Header.Get("Authorization") != ""
		_, _ = w.Write([]byte(membersPage()))
	})
	p := newTestGitLabProvider(f) // token accessor left nil

	if _, err := p.PermissionLevel(context.Background(), "owner/repo", "gitlab:alice"); err != nil {
		t.Fatalf("PermissionLevel: %v", err)
	}
	if sawAuth {
		t.Error("an unconfigured deployment credential still sent an auth header")
	}
}

// TestGitLabMembers_EmptyTokenSendsNoHeader covers the empty-string branch
// of the accessor: a configured-but-empty credential must not produce an
// empty PRIVATE-TOKEN header.
func TestGitLabMembers_EmptyTokenSendsNoHeader(t *testing.T) {
	f := newFakeGitLab(t)
	var hadHeader bool
	f.route("/api/v4/projects/owner%2Frepo/members/all", func(w http.ResponseWriter, r *http.Request) {
		_, hadHeader = r.Header["Private-Token"]
		_, _ = w.Write([]byte(membersPage()))
	})
	p := newTestGitLabProvider(f)
	p.token = func(context.Context) (string, error) { return "", nil }

	if _, err := p.PermissionLevel(context.Background(), "owner/repo", "gitlab:alice"); err != nil {
		t.Fatalf("PermissionLevel: %v", err)
	}
	if hadHeader {
		t.Error("an empty credential still set a PRIVATE-TOKEN header")
	}
}

// TestGitLabMembers_TokenAccessorError pins that a broken deployment
// credential is LOUD: it aborts before any HTTP call rather than falling
// back to an anonymous read, which would be a silent permission downgrade
// to whatever the anonymous view happens to show.
func TestGitLabMembers_TokenAccessorError(t *testing.T) {
	f := newFakeGitLab(t)
	f.route("/api/v4/projects/owner%2Frepo/members/all", func(http.ResponseWriter, *http.Request) {
		t.Error("members endpoint hit despite a failing credential accessor")
	})
	p := newTestGitLabProvider(f)
	p.token = func(context.Context) (string, error) { return "", errors.New("vault down") }

	_, err := p.PermissionLevel(context.Background(), "owner/repo", "gitlab:alice")
	if err == nil || !strings.Contains(err.Error(), "resolve gitlab token") {
		t.Fatalf("err = %v, want an 'identity: resolve gitlab token' wrap", err)
	}
	if n := f.requestCount(); n != 0 {
		t.Errorf("made %d request(s); a failing accessor must abort before the HTTP call", n)
	}
}

// ---------------------------------------------------------------------
// Concurrency + small pure helpers
// ---------------------------------------------------------------------

// TestGitLabProvider_ConcurrentReads runs the members readers concurrently
// against one provider under -race. The struct holds only immutable
// config, so a mutable-state regression trips the detector here.
func TestGitLabProvider_ConcurrentReads(t *testing.T) {
	f := newFakeGitLab(t)
	f.route("/api/v4/projects/owner%2Frepo/members/all", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(membersPage(gitLabMember{Username: "alice", AccessLevel: gitLabAccessDeveloper})))
	})
	f.route("/api/v4/groups/acme/members/all", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(membersPage(gitLabMember{Username: "alice", AccessLevel: gitLabAccessDeveloper})))
	})
	p := newTestGitLabProvider(f)
	p.token = func(context.Context) (string, error) { return "deployment-token", nil }

	var wg sync.WaitGroup
	for range 8 {
		wg.Add(2)
		go func() {
			defer wg.Done()
			if _, err := p.PermissionLevel(context.Background(), "owner/repo", "gitlab:alice"); err != nil {
				t.Errorf("PermissionLevel: %v", err)
			}
		}()
		go func() {
			defer wg.Done()
			if _, err := p.ResolveMembership(context.Background(), "acme", "gitlab:alice"); err != nil {
				t.Errorf("ResolveMembership: %v", err)
			}
		}()
	}
	wg.Wait()
}

func TestGitLabHasNextLink(t *testing.T) {
	tests := []struct {
		name   string
		header string
		want   bool
	}{
		{"quoted next", `<https://x/?page=2>; rel="next"`, true},
		{"unquoted next", `<https://x/?page=2>; rel=next`, true},
		{"next among several", `<https://x/?page=1>; rel="prev", <https://x/?page=3>; rel="next"`, true},
		{"only prev/last", `<https://x/?page=1>; rel="first", <https://x/?page=9>; rel="last"`, false},
		{"empty", "", false},
		{"url containing rel=next but no param", `<https://x/?rel=next>; rel="last"`, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := gitLabHasNextLink(tc.header); got != tc.want {
				t.Errorf("gitLabHasNextLink(%q) = %v, want %v", tc.header, got, tc.want)
			}
		})
	}
}

func TestGitLabLoginFromSubject(t *testing.T) {
	tests := []struct {
		subject string
		want    string
		wantOK  bool
	}{
		{"gitlab:alice", "alice", true},
		{"gitlab: alice ", "alice", true},
		{"gitlab:", "", false},
		{"gitlab:  ", "", false},
		{"github:alice", "", false},
		{"alice", "", false},
		{"", "", false},
		{"GITLAB:alice", "", false},
	}
	for _, tc := range tests {
		t.Run(tc.subject, func(t *testing.T) {
			got, ok := gitLabLoginFromSubject(tc.subject)
			if got != tc.want || ok != tc.wantOK {
				t.Errorf("gitLabLoginFromSubject(%q) = (%q, %v), want (%q, %v)", tc.subject, got, ok, tc.want, tc.wantOK)
			}
		})
	}
}
