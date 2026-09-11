package stub

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/kuhlman-labs/fishhawk/backend/internal/githubapp"
)

// Transport returns an in-process http.RoundTripper that serves every
// request through h and returns the captured *http.Response. No socket
// is opened and no httptest server is started: the production clients
// (githubclient over cfg.GitHub, gitlabclient behind the gitlab forge) are
// pointed at the `.invalid` base URLs and reach the stub through this
// round-tripper alone, so the preview needs no second port and a request
// that somehow escaped it could not resolve on the network.
func Transport(h http.Handler) http.RoundTripper {
	return &transport{h: h}
}

type transport struct {
	h http.Handler
}

// RoundTrip serves req through the handler. The request context is
// honoured before dispatch (a cancelled caller never reaches the stub);
// the handler itself runs synchronously on the caller's goroutine.
func (t *transport) RoundTrip(req *http.Request) (*http.Response, error) {
	if err := req.Context().Err(); err != nil {
		return nil, err
	}
	// Server-side handlers read r.Host and expect r.RequestURI-style
	// paths; clone so the caller's request is never mutated.
	r := req.Clone(req.Context())
	if r.Host == "" {
		r.Host = r.URL.Host
	}
	if r.Body == nil {
		r.Body = http.NoBody
	}
	rec := &recorder{header: http.Header{}}
	t.h.ServeHTTP(rec, r)
	if req.Body != nil {
		_ = req.Body.Close()
	}
	code := rec.code
	if code == 0 {
		code = http.StatusOK
	}
	return &http.Response{
		Status:        http.StatusText(code),
		StatusCode:    code,
		Proto:         "HTTP/1.1",
		ProtoMajor:    1,
		ProtoMinor:    1,
		Header:        rec.header,
		Body:          io.NopCloser(bytes.NewReader(rec.body.Bytes())),
		ContentLength: int64(rec.body.Len()),
		Request:       req,
	}, nil
}

// recorder is the minimal http.ResponseWriter the transport captures into.
// It is deliberately not httptest.ResponseRecorder so production code
// carries no httptest import.
type recorder struct {
	header http.Header
	code   int
	body   bytes.Buffer
}

func (r *recorder) Header() http.Header { return r.header }

func (r *recorder) WriteHeader(code int) {
	if r.code == 0 {
		r.code = code
	}
}

func (r *recorder) Write(p []byte) (int, error) {
	if r.code == 0 {
		r.code = http.StatusOK
	}
	return r.body.Write(p)
}

// Handler routes a request to the family handler by host: GitHubBaseURL's
// host reaches GitHubHandler, GitLabBaseURL's host reaches GitLabHandler,
// and any other host answers a JSON 404.
func (f *Forge) Handler() http.Handler {
	gh := f.GitHubHandler()
	gl := f.GitLabHandler()
	ghHost := mustHost(GitHubBaseURL)
	glHost := mustHost(GitLabBaseURL)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host := r.Host
		if host == "" {
			host = r.URL.Host
		}
		switch strings.ToLower(host) {
		case ghHost:
			gh.ServeHTTP(w, r)
		case glHost:
			gl.ServeHTTP(w, r)
		default:
			writeJSON(w, http.StatusNotFound, map[string]any{"message": "stub: unknown forge host " + host})
		}
	})
}

// HTTPClient returns an *http.Client whose only transport is the
// in-process stub. Hand it to githubclient.Client.HTTP and
// forgegitlab.WithHTTPClient.
func (f *Forge) HTTPClient() *http.Client {
	return &http.Client{Transport: Transport(f.Handler())}
}

func mustHost(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		panic("stub: bad base url " + raw + ": " + err.Error())
	}
	return strings.ToLower(u.Host)
}

// StaticTokens is a githubapp.TokenProvider returning one fixed token for
// every installation. The METHOD name is fixed by the interface —
// githubapp.TokenProvider requires
// `Token(ctx context.Context, installationID int64) (string, error)` — so
// the FIELD is named Value (Go rejects a field and a method of the same
// name on one type). The second parameter is blank so this package never
// declares an `installationID int64` identifier, which the forge
// credential-scope gate (credential_scope_gate_test.go) forbids outside
// its allowlist.
type StaticTokens struct {
	Value string
}

// Token returns the fixed Value for any installation.
func (s StaticTokens) Token(_ context.Context, _ int64) (string, error) {
	return s.Value, nil
}

// Compile-time pin: a regression that renames Token or changes its
// signature fails `go build`, not a test.
var _ githubapp.TokenProvider = StaticTokens{}

// SignGitHubDelivery returns the X-Hub-Signature-256 header value for body
// under secret: "sha256=" + hex(HMAC-SHA256(secret, body)), exactly the
// form webhook.VerifySignature accepts.
func SignGitHubDelivery(secret, body []byte) string {
	mac := hmac.New(sha256.New, secret)
	mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}
