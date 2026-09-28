package pushnotify

import (
	"bufio"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"io"
	"mime/quotedprintable"
	"net"
	"net/http/httptest"
	"net/mail"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kuhlman-labs/fishhawk/backend/internal/timescale"
)

// emailGoldenSubject / emailGoldenBody are the SHIPPED rendering of
// fixtureEvent: the AC-1 decision context (decision, repo#issue, wait, link)
// plus verdicts. A no-op touch of the renderer cannot satisfy them.
const emailGoldenSubject = "[Fishhawk] Plan ready for approval on kuhlman-labs/fishhawk#2292"

const emailGoldenBody = `Plan ready for approval on kuhlman-labs/fishhawk#2292

Repository:   kuhlman-labs/fishhawk
Issue:        #2292 https://github.com/kuhlman-labs/fishhawk/issues/2292
Run:          4d50291a (4d50291a-8e55-48a7-a5c6-c39a060f7dac)
Workflow:     feature_change
Stage:        plan (awaiting_approval)
Event:        plan_awaiting_approval (source sequence 90417)
Occurred:     2026-09-28T12:00:00Z

Reviews:
  - claude-opus-5-5: approve
  - gpt-6: reject

Waiting on a human for 8m
  - plan_approval: 8m

Open run:     https://fishhawk.example.com/runs/4d50291a
Issue link:   https://github.com/kuhlman-labs/fishhawk/issues/2292
`

// emailGoldenBodyNoIssue is the rendering for a CLI-triggered run: no issue,
// no verdicts, no wait, no links.
const emailGoldenBodyNoIssue = `Scope amendment requested on kuhlman-labs/fishhawk run 0badcafe

Repository:   kuhlman-labs/fishhawk
Run:          0badcafe (0badcafe-0000)
Event:        scope_amendment (source sequence 12)
`

func TestEmailRenderGolden(t *testing.T) {
	subject, body := RenderEmail(fixtureEvent())
	if subject != emailGoldenSubject {
		t.Fatalf("subject drifted.\n got: %q\nwant: %q", subject, emailGoldenSubject)
	}
	if body != emailGoldenBody {
		t.Fatalf("body drifted.\n got:\n%s\nwant:\n%s", body, emailGoldenBody)
	}
	cli := Event{Event: "scope_amendment", SourceSequence: 12, RunID: "0badcafe-0000", RunShortID: "0badcafe", Repo: "kuhlman-labs/fishhawk", Decision: "Scope amendment requested"}
	subject, body = RenderEmail(cli)
	if subject != "[Fishhawk] Scope amendment requested on kuhlman-labs/fishhawk run 0badcafe" {
		t.Fatalf("CLI subject drifted: %q", subject)
	}
	if body != emailGoldenBodyNoIssue {
		t.Fatalf("CLI body drifted.\n got:\n%s\nwant:\n%s", body, emailGoldenBodyNoIssue)
	}
}

// Payload text cannot forge a header (subject) or an extra labelled line.
func TestRenderEmail_CollapsesLineBreaks(t *testing.T) {
	e := fixtureEvent()
	e.Decision = "Approve\r\nBcc: attacker@example.com"
	e.Repo = "a/b\nRun: forged"
	subject, body := RenderEmail(e)
	if strings.ContainsAny(subject, "\r\n") {
		t.Fatalf("subject carries a line break: %q", subject)
	}
	for _, l := range strings.Split(body, "\n") {
		if strings.HasPrefix(l, "Bcc:") || strings.HasPrefix(l, "Run: forged") {
			t.Fatalf("payload forged a body line %q:\n%s", l, body)
		}
	}
}

// ---- fake SMTP server ----

type smtpServer struct {
	ln       net.Listener
	addr     string
	greeting string // default "220 fake ESMTP"
	startTLS *tls.Config
	authFail string            // non-empty: reply to AUTH with this line
	reject   map[string]string // verb -> failure reply line

	mu       sync.Mutex
	commands []string
	data     []string
	authArgs []string
	tlsAuth  []bool // per AUTH: was the connection TLS
}

func newSMTPServer(t *testing.T, configure func(*smtpServer)) *smtpServer {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s := &smtpServer{ln: ln, addr: ln.Addr().String(), greeting: "220 fake ESMTP"}
	if configure != nil {
		configure(s)
	}
	var wg sync.WaitGroup
	t.Cleanup(func() { _ = ln.Close(); wg.Wait() })
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			wg.Add(1)
			go func() { defer wg.Done(); s.serve(c) }()
		}
	}()
	return s
}

func (s *smtpServer) record(verb string) {
	s.mu.Lock()
	s.commands = append(s.commands, verb)
	s.mu.Unlock()
}

func (s *smtpServer) verbs() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.commands...)
}

func (s *smtpServer) serve(c net.Conn) {
	defer func() { _ = c.Close() }()
	_ = c.SetDeadline(time.Now().Add(timescale.D(5 * time.Second)))
	r, w := bufio.NewReader(c), bufio.NewWriter(c)
	reply := func(l string) { _, _ = w.WriteString(l + "\r\n"); _ = w.Flush() }
	reply(s.greeting)
	if !strings.HasPrefix(s.greeting, "220") {
		return
	}
	isTLS := false
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return
		}
		line = strings.TrimRight(line, "\r\n")
		verb := strings.ToUpper(strings.SplitN(line, " ", 2)[0])
		s.record(verb)
		if r, ok := s.reject[verb]; ok {
			reply(r)
			continue
		}
		switch verb {
		case "EHLO":
			ext := []string{"250-fake", "250-AUTH PLAIN"}
			if s.startTLS != nil && !isTLS {
				ext = append(ext, "250-STARTTLS")
			}
			ext = append(ext, "250 8BITMIME")
			for _, l := range ext {
				_, _ = w.WriteString(l + "\r\n")
			}
			_ = w.Flush()
		case "STARTTLS":
			reply("220 go ahead")
			tc := tls.Server(c, s.startTLS)
			if err := tc.Handshake(); err != nil {
				return
			}
			c, isTLS = tc, true
			r, w = bufio.NewReader(tc), bufio.NewWriter(tc)
		case "AUTH":
			s.mu.Lock()
			s.authArgs = append(s.authArgs, line)
			s.tlsAuth = append(s.tlsAuth, isTLS)
			s.mu.Unlock()
			if s.authFail != "" {
				reply(s.authFail)
			} else {
				reply("235 ok")
			}
		case "DATA":
			reply("354 go")
			var b strings.Builder
			for {
				l, err := r.ReadString('\n')
				if err != nil {
					return
				}
				if l == ".\r\n" {
					break
				}
				b.WriteString(l)
			}
			s.mu.Lock()
			s.data = append(s.data, b.String())
			s.mu.Unlock()
			reply("250 queued")
		case "QUIT":
			reply("221 bye")
			return
		default:
			reply("250 ok")
		}
	}
}

// tlsFixture returns a server TLS config and a client config trusting it
// (httptest's certificate covers 127.0.0.1).
func tlsFixture(t *testing.T) (server, client *tls.Config) {
	t.Helper()
	srv := httptest.NewUnstartedServer(nil)
	srv.StartTLS()
	cert := srv.TLS.Certificates[0]
	pool := x509.NewCertPool()
	pool.AddCert(srv.Certificate())
	srv.Close()
	return &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12}, &tls.Config{RootCAs: pool}
}

func newEmail(t *testing.T, cfg EmailConfig) *EmailSink {
	t.Helper()
	if cfg.From == "" {
		cfg.From = "fishhawk@example.com"
	}
	if cfg.To == nil {
		cfg.To = []string{"ops@example.com", " oncall@example.com "}
	}
	if cfg.Timeout == 0 {
		cfg.Timeout = timescale.D(3 * time.Second)
	}
	s, err := NewEmailSink(cfg)
	if err != nil {
		t.Fatalf("NewEmailSink: %v", err)
	}
	return s
}

func contains(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}

// ---- registry / configuration ----

func TestEmailSink_SelfRegisters(t *testing.T) {
	if !contains(Registered(), EmailKind) {
		t.Fatalf("Registered() = %v, missing %q", Registered(), EmailKind)
	}
	env := map[string]string{EnvEmailSMTPAddr: "smtp.example.com:587", EnvEmailFrom: "f@example.com", EnvEmailTo: "a@example.com,b@example.com"}
	sinks, err := SinksFromEnv(func(k string) string { return env[k] })
	if err != nil || len(sinks) != 1 || sinks[0].Name() != EmailKind {
		t.Fatalf("SinksFromEnv = %v, %v; want one email sink", sinks, err)
	}
	es := sinks[0].(*EmailSink)
	if !es.startTLS || es.DestinationHost() != "smtp.example.com" || len(es.to) != 2 {
		t.Fatalf("sink = %+v; want STARTTLS default-on, host smtp.example.com, 2 recipients", es)
	}
}

// W5: an SMTP addr with no _FROM is half-configured: a NAMED error and NO sink.
// The arms differ only in whether FROM is set.
func TestSinksFromEnv_HalfConfiguredEmailFailsClosed(t *testing.T) {
	env := map[string]string{EnvEmailSMTPAddr: "smtp.example.com:587", EnvEmailTo: "a@example.com"}
	sinks, err := SinksFromEnv(func(k string) string { return env[k] })
	if err == nil || !strings.Contains(err.Error(), EnvEmailFrom+": required") {
		t.Fatalf("err = %v; want a named %s error", err, EnvEmailFrom)
	}
	if sinks != nil {
		t.Fatalf("sinks = %v; want none", sinks)
	}
	env[EnvEmailFrom] = "f@example.com"
	if sinks, err = SinksFromEnv(func(k string) string { return env[k] }); err != nil || len(sinks) != 1 {
		t.Fatalf("fully configured: %v, %v", sinks, err)
	}
}

// Every configuration failure names its env var and the problem.
func TestEmailConfig_NamedErrors(t *testing.T) {
	base := func() map[string]string {
		return map[string]string{EnvEmailSMTPAddr: "smtp.example.com:587", EnvEmailFrom: "f@example.com", EnvEmailTo: "a@example.com"}
	}
	cases := []struct {
		name string
		mut  func(map[string]string)
		want string
	}{
		{"only password set", func(m map[string]string) { clear(m); m[EnvEmailPassword] = "x" }, EnvEmailSMTPAddr + ": required"},
		{"missing to", func(m map[string]string) { delete(m, EnvEmailTo) }, EnvEmailTo + ": required"},
		{"empty recipient list", func(m map[string]string) { m[EnvEmailTo] = " , ," }, EnvEmailTo + ": must name at least one recipient"},
		{"bad recipient", func(m map[string]string) { m[EnvEmailTo] = "a@example.com,not an address" }, EnvEmailTo + ": entry 2 is not a valid email address"},
		{"bad from", func(m map[string]string) { m[EnvEmailFrom] = "nope" }, EnvEmailFrom + ": not a valid email address"},
		{"no port", func(m map[string]string) { m[EnvEmailSMTPAddr] = "smtp.example.com" }, EnvEmailSMTPAddr + ": must be host:port"},
		{"bad port", func(m map[string]string) { m[EnvEmailSMTPAddr] = "smtp.example.com:0" }, EnvEmailSMTPAddr + ": port must be 1-65535"},
		{"bad starttls", func(m map[string]string) { m[EnvEmailSTARTTLS] = "maybe" }, EnvEmailSTARTTLS + ": must be true or false"},
		{"username without password", func(m map[string]string) { m[EnvEmailUsername] = "u" }, EnvEmailPassword + ": required when " + EnvEmailUsername},
		{"password without username", func(m map[string]string) { m[EnvEmailPassword] = "p" }, EnvEmailUsername + ": required when " + EnvEmailPassword},
		{"auth with starttls off", func(m map[string]string) {
			m[EnvEmailUsername], m[EnvEmailPassword], m[EnvEmailSTARTTLS] = "u", "p", "false"
		}, EnvEmailSTARTTLS + ": must be true when " + EnvEmailPassword},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := base()
			tc.mut(env)
			sinks, err := SinksFromEnv(func(k string) string { return env[k] })
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v; want %q", err, tc.want)
			}
			if sinks != nil {
				t.Fatalf("sinks = %v; want none", sinks)
			}
		})
	}
	// A CR/LF in a configured address (bare or inside a quoted display name)
	// is refused, never folded into a header.
	for _, from := range []string{"f@example.com\r\nBcc: x@example.com", "\"a\r\nBcc: x@example.com\" <f@example.com>"} {
		if _, err := NewEmailSink(EmailConfig{Addr: "h:25", From: from, To: []string{"a@example.com"}}); err == nil {
			t.Fatalf("a From carrying CRLF was accepted: %q", from)
		}
	}
}

// ---- delivery ----

// Local relay: no STARTTLS advertised, no credentials — the message is sent,
// with the rendered subject/body and the delivery headers, to every recipient.
func TestEmailSink_DeliversRenderedMessage(t *testing.T) {
	srv := newSMTPServer(t, nil)
	s := newEmail(t, EmailConfig{Addr: srv.addr, STARTTLS: true})
	if err := s.Deliver(context.Background(), fixtureEvent()); err != nil {
		t.Fatalf("Deliver: %v", err)
	}
	verbs := srv.verbs()
	for _, want := range []string{"EHLO", "MAIL", "RCPT", "DATA", "QUIT"} {
		if !contains(verbs, want) {
			t.Fatalf("commands = %v; missing %s", verbs, want)
		}
	}
	rcpts := 0
	for _, v := range verbs {
		if v == "RCPT" {
			rcpts++
		}
	}
	if rcpts != 2 || contains(verbs, "AUTH") || contains(verbs, "STARTTLS") {
		t.Fatalf("commands = %v; want 2 RCPT, no AUTH, no STARTTLS", verbs)
	}
	srv.mu.Lock()
	raw := srv.data[0]
	srv.mu.Unlock()
	msg, err := mail.ReadMessage(strings.NewReader(raw))
	if err != nil {
		t.Fatalf("message does not parse: %v\n%s", err, raw)
	}
	if got := msg.Header.Get("Subject"); got != emailGoldenSubject {
		t.Fatalf("Subject = %q", got)
	}
	if got := msg.Header.Get(HeaderDelivery); got != DeliveryID(fixtureEvent()) {
		t.Fatalf("%s = %q, want %q", HeaderDelivery, got, DeliveryID(fixtureEvent()))
	}
	if got := msg.Header.Get("Content-Type"); got != "text/plain; charset=utf-8" {
		t.Fatalf("Content-Type = %q", got)
	}
	body, err := io.ReadAll(quotedprintable.NewReader(msg.Body))
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.ReplaceAll(string(body), "\r\n", "\n"); got != emailGoldenBody {
		t.Fatalf("wire body != rendered golden.\n got:\n%s", got)
	}
}

// With STARTTLS advertised and credentials configured the sink upgrades
// first and authenticates only on the encrypted connection.
func TestEmailSink_STARTTLSThenAuth(t *testing.T) {
	serverTLS, clientTLS := tlsFixture(t)
	srv := newSMTPServer(t, func(s *smtpServer) { s.startTLS = serverTLS })
	s := newEmail(t, EmailConfig{Addr: srv.addr, Username: "u", Password: "p", STARTTLS: true, TLSConfig: clientTLS})
	if err := s.Deliver(context.Background(), fixtureEvent()); err != nil {
		t.Fatalf("Deliver: %v", err)
	}
	verbs := srv.verbs()
	iTLS, iAuth := -1, -1
	for i, v := range verbs {
		if v == "STARTTLS" && iTLS < 0 {
			iTLS = i
		}
		if v == "AUTH" && iAuth < 0 {
			iAuth = i
		}
	}
	if iTLS < 0 || iAuth < 0 || iAuth < iTLS {
		t.Fatalf("commands = %v; want STARTTLS before AUTH", verbs)
	}
	srv.mu.Lock()
	defer srv.mu.Unlock()
	if len(srv.tlsAuth) != 1 || !srv.tlsAuth[0] || len(srv.data) != 1 {
		t.Fatalf("AUTH over TLS = %v, messages = %d; want one AUTH on TLS and one message", srv.tlsAuth, len(srv.data))
	}
}

// STARTTLS=false never upgrades, even when the server advertises it.
func TestEmailSink_STARTTLSDisabledDoesNotUpgrade(t *testing.T) {
	serverTLS, _ := tlsFixture(t)
	srv := newSMTPServer(t, func(s *smtpServer) { s.startTLS = serverTLS })
	s := newEmail(t, EmailConfig{Addr: srv.addr, STARTTLS: false})
	if err := s.Deliver(context.Background(), fixtureEvent()); err != nil {
		t.Fatalf("Deliver: %v", err)
	}
	if verbs := srv.verbs(); contains(verbs, "STARTTLS") || !contains(verbs, "DATA") {
		t.Fatalf("commands = %v; want DATA and no STARTTLS", verbs)
	}
}

// S1: a server advertising NO STARTTLS with credentials configured — the sink
// refuses before AUTH. Asserted on the server's RECORDED command list (no
// AUTH, no MAIL) as well as the named error. The fixture is on 127.0.0.1,
// where net/smtp's own PlainAuth WOULD send credentials in the clear, so this
// guard is the only control in the path.
func TestEmailSink_RefusesPlaintextAuthWithoutTLS(t *testing.T) {
	srv := newSMTPServer(t, nil)
	s := newEmail(t, EmailConfig{Addr: srv.addr, Username: "u", Password: "p", STARTTLS: true})
	err := s.Deliver(context.Background(), fixtureEvent())
	var de *DeliveryError
	if !errors.As(err, &de) || de.Reason != ReasonAuthRequiresTLS || de.Sink != EmailKind || de.Host != "127.0.0.1" {
		t.Fatalf("err = %#v; want %q from the email sink at 127.0.0.1", err, ReasonAuthRequiresTLS)
	}
	verbs := srv.verbs()
	if !contains(verbs, "EHLO") {
		t.Fatalf("commands = %v; the exchange never reached EHLO (vacuous)", verbs)
	}
	if contains(verbs, "AUTH") || contains(verbs, "MAIL") || contains(verbs, "DATA") {
		t.Fatalf("commands = %v; credentials or mail were sent over plaintext", verbs)
	}
}

// A server that accepts the connection but never speaks is bounded by the
// sink's own I/O deadline even under a context that is never cancelled.
func TestEmailSink_OwnDeadlineBoundsWedgedServer(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	held := make(chan net.Conn, 1)
	go func() {
		c, err := ln.Accept()
		if err == nil {
			held <- c
		}
	}()
	t.Cleanup(func() {
		_ = ln.Close()
		select {
		case c := <-held:
			_ = c.Close()
		default:
		}
	})
	s := newEmail(t, EmailConfig{Addr: ln.Addr().String(), Timeout: timescale.D(100 * time.Millisecond)})
	done := make(chan error, 1)
	go func() { done <- s.Deliver(context.Background(), fixtureEvent()) }()
	select {
	case err := <-done:
		var de *DeliveryError
		if !errors.As(err, &de) || de.Reason != ReasonTimeout {
			t.Fatalf("err = %#v; want a timeout DeliveryError", err)
		}
	case <-time.After(timescale.D(3 * time.Second)):
		t.Fatal("Deliver against a silent server did not return: no connection deadline")
	}
}

// Cancelling ctx mid-exchange (a server that accepted but never greets, and a
// sink timeout far beyond the test bound) closes the connection promptly and
// is classified canceled, not as the closed-connection error it caused.
func TestEmailSink_CancelledContextMidExchange(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	held := make(chan net.Conn, 1)
	go func() {
		c, err := ln.Accept()
		if err == nil {
			held <- c
		}
	}()
	t.Cleanup(func() {
		_ = ln.Close()
		select {
		case c := <-held:
			_ = c.Close()
		default:
		}
	})
	s := newEmail(t, EmailConfig{Addr: ln.Addr().String(), Timeout: timescale.D(30 * time.Second)})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- s.Deliver(ctx, fixtureEvent()) }()
	conn := <-held // connected; the sink is now blocked reading the greeting
	t.Cleanup(func() { _ = conn.Close() })
	cancel()
	select {
	case err := <-done:
		var de *DeliveryError
		if !errors.As(err, &de) || de.Reason != ReasonCanceled {
			t.Fatalf("err = %#v; want canceled", err)
		}
	case <-time.After(timescale.D(3 * time.Second)):
		t.Fatal("Deliver ignored ctx cancellation")
	}
}

// ---- L3: no credential echo ----

// L3: the SMTP password (and a canary planted in every other configured
// value) reaches no configuration error, no returned delivery error, no
// dispatcher WARN log and no outcome feeding push_notification_failed —
// across a config error, a dial failure, a greeting rejection and an AUTH
// rejection whose server reply text echoes the canary.
func TestEmailSink_ErrorLeaksNoPassword(t *testing.T) {
	password := "pw-" + canary

	// Configuration errors: canary in the addr, the from, a recipient and the
	// password.
	for name, env := range map[string]map[string]string{
		"bad addr": {EnvEmailSMTPAddr: "smtp." + canary + ".example", EnvEmailFrom: "f@example.com", EnvEmailTo: "a@example.com", EnvEmailUsername: "u", EnvEmailPassword: password},
		"bad from": {EnvEmailSMTPAddr: "smtp.example.com:587", EnvEmailFrom: canary, EnvEmailTo: "a@example.com", EnvEmailUsername: "u", EnvEmailPassword: password},
		"bad to":   {EnvEmailSMTPAddr: "smtp.example.com:587", EnvEmailFrom: "f@example.com", EnvEmailTo: canary + " x", EnvEmailUsername: "u", EnvEmailPassword: password},
		"no user":  {EnvEmailSMTPAddr: "smtp.example.com:587", EnvEmailFrom: "f@example.com", EnvEmailTo: "a@example.com", EnvEmailPassword: password},
	} {
		_, err := SinksFromEnv(func(k string) string { return env[k] })
		if err == nil {
			t.Fatalf("%s: config accepted", name)
		}
		if strings.Contains(err.Error(), canary) {
			t.Fatalf("%s: config error echoes a value: %v", name, err)
		}
		if !strings.Contains(err.Error(), "FISHHAWKD_NOTIFY_EMAIL_") {
			t.Fatalf("%s: config error names no env var (vacuous): %v", name, err)
		}
	}

	serverTLS, clientTLS := tlsFixture(t)
	greetReject := newSMTPServer(t, func(s *smtpServer) { s.greeting = "554 no service for " + canary })
	authReject := newSMTPServer(t, func(s *smtpServer) {
		s.startTLS = serverTLS
		s.authFail = "535 bad credentials " + password
	})
	cfg := func(addr string) EmailConfig {
		return EmailConfig{Addr: addr, Username: "u", Password: password, STARTTLS: true, TLSConfig: clientTLS}
	}
	refused := newEmail(t, cfg("127.0.0.1:1"))
	greet := newEmail(t, cfg(greetReject.addr))
	auth := newEmail(t, cfg(authReject.addr))

	// Ordered: the two arms whose server reply echoes a secret run first.
	arms := []struct {
		s      *EmailSink
		reason string
	}{{greet, "smtp status 554"}, {auth, "smtp status 535"}, {refused, ReasonConnectionRefused}}
	texts := map[string]string{}
	for _, arm := range arms {
		s, reason := arm.s, arm.reason
		err := s.Deliver(context.Background(), fixtureEvent())
		if err == nil {
			t.Fatalf("Deliver succeeded; want reason %q", reason)
		}
		if strings.Contains(err.Error(), canary) {
			t.Fatalf("Deliver error leaks a credential: %s", err)
		}
		var de *DeliveryError
		if !errors.As(err, &de) || de.Reason != reason {
			t.Fatalf("Deliver err = %#v; want reason %q", err, reason)
		}
		texts["error "+reason] = err.Error()
	}
	// The AUTH arm really did send the password (over TLS), so the reply
	// echo is a genuine opportunity to leak.
	authReject.mu.Lock()
	sentAuth := len(authReject.authArgs)
	authReject.mu.Unlock()
	if sentAuth != 1 {
		t.Fatalf("AUTH attempts = %d; want 1 (fixture never reached AUTH)", sentAuth)
	}

	rec := &outcomeRecorder{}
	log, logs := captureLogger()
	d := NewDispatcher([]Sink{refused, greet, auth}, Options{OnOutcome: rec.record, Logger: log})
	closeOnCleanup(t, d)
	d.Enqueue(fixtureEvent())
	drain(t, d)
	outs := rec.all()
	if len(outs) != 3 {
		t.Fatalf("outcomes = %+v; want three", outs)
	}
	for i, o := range outs {
		if o.Err == nil {
			t.Fatalf("outcome %d succeeded; want failure", i)
		}
		texts["outcome "+o.Err.Error()] = o.Err.Error()
	}
	texts["log"] = logs.String()
	for where, text := range texts {
		if strings.Contains(text, canary) {
			t.Fatalf("%s leaks a credential: %s", where, text)
		}
		if !strings.Contains(text, EmailKind) {
			t.Fatalf("%s does not name the sink (vacuous assertion): %s", where, text)
		}
	}
}

// A protocol rejection at any later step is reported by SMTP status code, and
// a STARTTLS handshake against an untrusted certificate is classified tls
// (and never falls back to plaintext AUTH).
func TestEmailSink_ProtocolFailuresClassified(t *testing.T) {
	serverTLS, _ := tlsFixture(t)
	for _, tc := range []struct {
		name   string
		cfg    func(*smtpServer)
		creds  bool
		reason string
	}{
		{"rcpt rejected", func(s *smtpServer) { s.reject = map[string]string{"RCPT": "550 no such user"} }, false, "smtp status 550"},
		{"mail rejected", func(s *smtpServer) { s.reject = map[string]string{"MAIL": "451 try later"} }, false, "smtp status 451"},
		{"data rejected", func(s *smtpServer) { s.reject = map[string]string{"DATA": "554 refused"} }, false, "smtp status 554"},
		{"ehlo rejected", func(s *smtpServer) { s.reject = map[string]string{"EHLO": "502 no", "HELO": "502 no"} }, false, "smtp status 502"},
		{"untrusted starttls cert", func(s *smtpServer) { s.startTLS = serverTLS }, true, ReasonTLS},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := newSMTPServer(t, tc.cfg)
			cfg := EmailConfig{Addr: srv.addr, STARTTLS: true} // no RootCAs: system pool
			if tc.creds {
				cfg.Username, cfg.Password = "u", "p"
			}
			err := newEmail(t, cfg).Deliver(context.Background(), fixtureEvent())
			var de *DeliveryError
			if !errors.As(err, &de) || de.Reason != tc.reason || de.Host != "127.0.0.1" {
				t.Fatalf("err = %#v; want reason %q at 127.0.0.1", err, tc.reason)
			}
			if contains(srv.verbs(), "AUTH") {
				t.Fatalf("commands = %v; AUTH sent after a failed upgrade", srv.verbs())
			}
		})
	}
}
