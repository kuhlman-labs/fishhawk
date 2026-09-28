package pushnotify

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"mime"
	"mime/quotedprintable"
	"net"
	"net/mail"
	"net/smtp"
	"net/textproto"
	"strconv"
	"strings"
	"time"
)

// Email sink environment variables.
const (
	EnvEmailSMTPAddr = "FISHHAWKD_NOTIFY_EMAIL_SMTP_ADDR"
	EnvEmailFrom     = "FISHHAWKD_NOTIFY_EMAIL_FROM"
	EnvEmailTo       = "FISHHAWKD_NOTIFY_EMAIL_TO"
	EnvEmailUsername = "FISHHAWKD_NOTIFY_EMAIL_USERNAME"
	EnvEmailPassword = "FISHHAWKD_NOTIFY_EMAIL_PASSWORD"
	EnvEmailSTARTTLS = "FISHHAWKD_NOTIFY_EMAIL_STARTTLS"
)

// EmailKind is the registered sink kind.
const EmailKind = "email"

// DefaultSMTPTimeout bounds one whole SMTP exchange (dial through QUIT)
// independently of the dispatcher's per-sink context timeout.
const DefaultSMTPTimeout = 10 * time.Second

// ReasonAuthRequiresTLS is the classified reason when credentials are
// configured but the connection was not upgraded to TLS: AUTH is never sent.
const ReasonAuthRequiresTLS = "auth refused: connection not encrypted"

// EmailSubjectPrefix prefixes every subject so a mail filter can route it.
const EmailSubjectPrefix = "[Fishhawk] "

func init() {
	Register(EmailKind, emailFromEnv)
}

var emailEnvVars = []string{
	EnvEmailSMTPAddr, EnvEmailFrom, EnvEmailTo, EnvEmailUsername, EnvEmailPassword, EnvEmailSTARTTLS,
}

func emailFromEnv(getenv func(string) string) (Sink, error) {
	vals := make(map[string]string, len(emailEnvVars))
	anySet := false
	for _, k := range emailEnvVars {
		// The password is taken verbatim: surrounding whitespace may be part
		// of it. Every other value is trimmed.
		v := getenv(k)
		if k != EnvEmailPassword {
			v = strings.TrimSpace(v)
		}
		vals[k] = v
		anySet = anySet || v != ""
	}
	if !anySet {
		return nil, nil
	}
	var errs []error
	for _, k := range []string{EnvEmailSMTPAddr, EnvEmailFrom, EnvEmailTo} {
		if vals[k] == "" {
			errs = append(errs, fmt.Errorf("%s: required when any FISHHAWKD_NOTIFY_EMAIL_* variable is set", k))
		}
	}
	if len(errs) > 0 {
		return nil, errors.Join(errs...)
	}
	startTLS := true
	if raw := vals[EnvEmailSTARTTLS]; raw != "" {
		b, err := strconv.ParseBool(raw)
		if err != nil {
			return nil, fmt.Errorf("%s: must be true or false", EnvEmailSTARTTLS)
		}
		startTLS = b
	}
	return NewEmailSink(EmailConfig{
		Addr:     vals[EnvEmailSMTPAddr],
		From:     vals[EnvEmailFrom],
		To:       strings.Split(vals[EnvEmailTo], ","),
		Username: vals[EnvEmailUsername],
		Password: vals[EnvEmailPassword],
		STARTTLS: startTLS,
	})
}

// EmailConfig configures the SMTP sink.
type EmailConfig struct {
	// Addr is the SMTP server as host:port.
	Addr string
	From string
	// To lists the recipients; empty entries are ignored.
	To []string
	// Username and Password enable SMTP AUTH PLAIN. Both or neither.
	Username string
	Password string
	// STARTTLS upgrades the connection when the server advertises it.
	// Credentials are never sent over a connection that was not upgraded.
	STARTTLS bool
	// Timeout bounds the whole exchange (default DefaultSMTPTimeout).
	Timeout time.Duration
	// TLSConfig overrides the STARTTLS client config (tests pin RootCAs).
	// ServerName is always forced to the configured host.
	TLSConfig *tls.Config
}

// EmailSink sends a plain-text message over SMTP.
type EmailSink struct {
	addr     string
	host     string
	from     *mail.Address
	to       []*mail.Address
	username string
	password string
	startTLS bool
	timeout  time.Duration
	tlsConf  *tls.Config
}

// NewEmailSink validates cfg. Every error names the ENV VAR and the problem,
// never the value.
func NewEmailSink(cfg EmailConfig) (*EmailSink, error) {
	host, port, err := net.SplitHostPort(strings.TrimSpace(cfg.Addr))
	if err != nil || host == "" {
		return nil, fmt.Errorf("%s: must be host:port", EnvEmailSMTPAddr)
	}
	if p, perr := strconv.Atoi(port); perr != nil || p < 1 || p > 65535 {
		return nil, fmt.Errorf("%s: port must be 1-65535", EnvEmailSMTPAddr)
	}
	from, err := parseAddress(cfg.From)
	if err != nil {
		return nil, fmt.Errorf("%s: not a valid email address", EnvEmailFrom)
	}
	var to []*mail.Address
	for i, raw := range cfg.To {
		if strings.TrimSpace(raw) == "" {
			continue
		}
		a, err := parseAddress(raw)
		if err != nil {
			return nil, fmt.Errorf("%s: entry %d is not a valid email address", EnvEmailTo, i+1)
		}
		to = append(to, a)
	}
	if len(to) == 0 {
		return nil, fmt.Errorf("%s: must name at least one recipient", EnvEmailTo)
	}
	switch {
	case cfg.Username != "" && cfg.Password == "":
		return nil, fmt.Errorf("%s: required when %s is set", EnvEmailPassword, EnvEmailUsername)
	case cfg.Username == "" && cfg.Password != "":
		return nil, fmt.Errorf("%s: required when %s is set", EnvEmailUsername, EnvEmailPassword)
	case cfg.Username != "" && !cfg.STARTTLS:
		return nil, fmt.Errorf("%s: must be true when %s is set (credentials are never sent over an unencrypted connection)",
			EnvEmailSTARTTLS, EnvEmailPassword)
	}
	timeout := cfg.Timeout
	if timeout <= 0 {
		timeout = DefaultSMTPTimeout
	}
	var tlsConf *tls.Config
	if cfg.TLSConfig != nil {
		tlsConf = cfg.TLSConfig.Clone()
	} else {
		tlsConf = &tls.Config{}
	}
	tlsConf.ServerName = host
	if tlsConf.MinVersion < tls.VersionTLS12 {
		tlsConf.MinVersion = tls.VersionTLS12
	}
	return &EmailSink{
		addr:     net.JoinHostPort(host, port),
		host:     host,
		from:     from,
		to:       to,
		username: cfg.Username,
		password: cfg.Password,
		startTLS: cfg.STARTTLS,
		timeout:  timeout,
		tlsConf:  tlsConf,
	}, nil
}

// parseAddress parses one RFC 5322 address. net/mail rejects a CR/LF, so a
// configured value can never inject a header (pinned in email_test.go).
func parseAddress(raw string) (*mail.Address, error) {
	return mail.ParseAddress(strings.TrimSpace(raw))
}

// Name implements Sink.
func (*EmailSink) Name() string { return EmailKind }

// DestinationHost implements Destination.
func (s *EmailSink) DestinationHost() string { return s.host }

// Deliver implements Sink. The dial is bounded by net.Dialer{Timeout} and the
// whole exchange by a connection deadline set before any net/smtp call, so a
// wedged server cannot outlive the sink's own timeout even if ctx is never
// cancelled. Every error is returned via SanitizeTransportError.
func (s *EmailSink) Deliver(ctx context.Context, e Event) error {
	msg, err := s.message(e)
	if err != nil {
		return s.fail(ctx, err)
	}
	dialer := net.Dialer{Timeout: s.timeout}
	conn, err := dialer.DialContext(ctx, "tcp", s.addr)
	if err != nil {
		return s.fail(ctx, err)
	}
	deadline := time.Now().Add(s.timeout)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	if err := conn.SetDeadline(deadline); err != nil {
		_ = conn.Close()
		return s.fail(ctx, err)
	}
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()

	c, err := smtp.NewClient(conn, s.host)
	if err != nil {
		_ = conn.Close()
		return s.fail(ctx, err)
	}
	defer func() { _ = c.Close() }()
	if err := s.exchange(c, msg); err != nil {
		return s.fail(ctx, err)
	}
	return nil
}

func (s *EmailSink) exchange(c *smtp.Client, msg []byte) error {
	if err := c.Hello("localhost"); err != nil {
		return err
	}
	if s.startTLS {
		if ok, _ := c.Extension("STARTTLS"); ok {
			if err := c.StartTLS(s.tlsConf); err != nil {
				return err
			}
		}
	}
	if s.username != "" {
		// Explicit guard: net/smtp's PlainAuth would itself send credentials
		// in the clear to a localhost relay, the case an operator is least
		// likely to notice.
		if _, encrypted := c.TLSConnectionState(); !encrypted {
			return &DeliveryError{Sink: EmailKind, Host: s.host, Reason: ReasonAuthRequiresTLS}
		}
		if err := c.Auth(smtp.PlainAuth("", s.username, s.password, s.host)); err != nil {
			return err
		}
	}
	if err := c.Mail(s.from.Address); err != nil {
		return err
	}
	for _, r := range s.to {
		if err := c.Rcpt(r.Address); err != nil {
			return err
		}
	}
	w, err := c.Data()
	if err != nil {
		return err
	}
	if _, err := w.Write(msg); err != nil {
		_ = w.Close()
		return err
	}
	if err := w.Close(); err != nil {
		return err
	}
	return c.Quit()
}

// fail renders err through SanitizeTransportError. A cancelled or expired ctx
// wins over the closed-connection error it caused; an SMTP reply is reduced
// to its status code (the reply text is server-controlled and never kept).
func (s *EmailSink) fail(ctx context.Context, err error) error {
	if cerr := ctx.Err(); cerr != nil {
		err = cerr
	}
	var tp *textproto.Error
	if errors.As(err, &tp) {
		err = &DeliveryError{Sink: EmailKind, Host: s.host, Reason: fmt.Sprintf("smtp status %d", tp.Code)}
	}
	return SanitizeTransportError(EmailKind, AtHost(s.host, err))
}

// message builds the RFC 5322 message: headers plus a quoted-printable
// text/plain body.
func (s *EmailSink) message(e Event) ([]byte, error) {
	subject, body := RenderEmail(e)
	to := make([]string, 0, len(s.to))
	for _, a := range s.to {
		to = append(to, a.String())
	}
	var buf bytes.Buffer
	hdr := func(k, v string) { buf.WriteString(k + ": " + v + "\r\n") }
	hdr("From", s.from.String())
	hdr("To", strings.Join(to, ", "))
	hdr("Subject", mime.QEncoding.Encode("utf-8", subject))
	date := e.OccurredAt
	if date.IsZero() {
		date = time.Now()
	}
	hdr("Date", date.UTC().Format(time.RFC1123Z))
	hdr("Message-ID", "<"+headerToken(DeliveryID(e))+"@fishhawk>")
	hdr("X-Fishhawk-Event", headerToken(e.Event))
	hdr(HeaderDelivery, headerToken(DeliveryID(e)))
	hdr("MIME-Version", "1.0")
	hdr("Content-Type", "text/plain; charset=utf-8")
	hdr("Content-Transfer-Encoding", "quoted-printable")
	buf.WriteString("\r\n")
	qp := quotedprintable.NewWriter(&buf)
	if _, err := qp.Write([]byte(strings.ReplaceAll(body, "\n", "\r\n"))); err != nil {
		return nil, err
	}
	if err := qp.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// headerToken keeps only characters safe in a structured header value.
func headerToken(s string) string {
	return strings.Map(func(r rune) rune {
		if r > ' ' && r < 0x7f && r != '<' && r != '>' {
			return r
		}
		return -1
	}, s)
}

// oneLine collapses line breaks so payload text cannot forge extra lines in
// a subject or a labelled body field.
func oneLine(s string) string {
	return strings.Join(strings.Fields(strings.NewReplacer("\r", " ", "\n", " ").Replace(s)), " ")
}

// RenderEmail renders e as a plain-text email: the subject is the decision
// line (`[Fishhawk] <decision> on <repo>#<issue>`) and the body carries the
// same one-screen context as the webhook payload. It is pure and
// deterministic; lines with no value are omitted.
func RenderEmail(e Event) (subject, body string) {
	line := oneLine(decisionLine(e))
	subject = EmailSubjectPrefix + line

	var b strings.Builder
	b.WriteString(line + "\n")
	field := func(label, value string) {
		if value = oneLine(value); value != "" {
			fmt.Fprintf(&b, "%-13s %s\n", label+":", value)
		}
	}
	b.WriteString("\n")
	field("Repository", e.Repo)
	if e.Issue != nil && e.Issue.Number > 0 {
		field("Issue", joinNonEmpty(" ", "#"+strconv.Itoa(e.Issue.Number), e.Issue.URL))
	}
	run := e.RunShortID
	if e.RunID != "" && e.RunID != e.RunShortID {
		run = joinNonEmpty(" ", run, "("+e.RunID+")")
	}
	field("Run", run)
	field("Workflow", e.WorkflowID)
	if e.Stage != nil {
		field("Stage", joinNonEmpty(" ", e.Stage.Type, parens(e.Stage.State)))
	}
	field("Event", joinNonEmpty(" ", e.Event, "(source sequence "+strconv.FormatInt(e.SourceSequence, 10)+")"))
	if !e.OccurredAt.IsZero() {
		field("Occurred", e.OccurredAt.UTC().Format(time.RFC3339))
	}

	if len(e.Verdicts) > 0 {
		b.WriteString("\nReviews:\n")
		for _, v := range e.Verdicts {
			b.WriteString("  - " + oneLine(v.ReviewerModel) + ": " + oneLine(v.Verdict) + "\n")
		}
	}

	if e.GateLatency.TotalWaitOnHumanSeconds > 0 {
		b.WriteString("\nWaiting on a human for " + FormatWait(e.GateLatency.TotalWaitOnHumanSeconds) + "\n")
		for _, g := range e.GateLatency.Gates {
			b.WriteString("  - " + oneLine(g.Gate) + ": " + FormatWait(g.WaitSeconds) + "\n")
		}
	}

	if e.Links.Run != "" || e.Links.Issue != "" || e.Links.PullRequest != "" {
		b.WriteString("\n")
		field("Open run", e.Links.Run)
		field("Issue link", e.Links.Issue)
		field("Pull request", e.Links.PullRequest)
	}
	return subject, b.String()
}
