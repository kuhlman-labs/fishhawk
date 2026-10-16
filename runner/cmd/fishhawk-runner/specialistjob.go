package main

// `fishhawk-runner specialist-job` (ADR-097 D2/D6, E85.9 / #4300).
//
// A specialist job is NOT a run stage: it is evidence work an operator seat
// requests about a run, campaign or head SHA. fishhawkd records the request
// (and renders `draft` in-process); this subcommand executes every other kind
// against a FRESH scratch clone of the subject's head SHA (except the
// argument-free host.* inventory ids, which read no tree and never clone,
// #4330) and ships one typed completion. Four controls bound what a job can
// do, each with a named error:
//
//   - the job HTTP client's specialistJobTransport admits ONLY this job's
//     GET /v0/specialist-jobs/{id} and POST .../{admission,result}; every other
//     method or path (every /v0/runs/... and /v0/stages/... write included)
//     fails errSpecialistJobRouteRefused before any network I/O;
//   - every job git call goes through jobGit, which refuses push / send-pack /
//     a remote add or set-url (errSpecialistJobWriteRefused), a dir outside the
//     job's own scratch parent (errSpecialistJobNotScratch), and a clone that
//     has already executed repository code (errSpecialistJobCloneTainted, the
//     #4143 H1 lesson: no host git after untrusted execution);
//   - every materialized clone passes requireScratchDir, so a job never
//     executes in the lineage worktree or the source checkout;
//   - a draft job is refused errSpecialistJobNotRunnable and nothing ships.
//
// Every go test / go list runs through the counterfactual executor's gate exec
// seam (runBoundedGateArgvDisposed) under the verify lock. A job is admitted
// into the host's local-verify:<host> concurrency group by POST .../admission
// (re-POSTed with the same nonce as the heartbeat).
//
// Result attestation (E85.15 / #4363): with FISHHAWK_RUNNER_ATTEST_TOKEN set
// (a token holding attest:specialist-jobs) the runner authenticates on it and
// the admission answer carries a job-scoped Ed25519 key, held in memory only
// and never logged; the result is signed with it and records provenance
// runner_attested. Without the token, or with no or an undecodable key, the
// runner logs specialist_job_attestation_degraded and ships unsigned (bearer).
// Long-form contract: runner/README.md § "Specialist jobs".

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/kuhlman-labs/fishhawk/runner/internal/gateiso"
	"github.com/kuhlman-labs/fishhawk/runner/internal/gitops"
	"github.com/kuhlman-labs/fishhawk/runner/internal/upload"
)

const specialistJobSchemaVersion = "specialist-job-v1"

// Admission and heartbeat bounds. The poll mirrors the concurrency package's
// 5s ±20% host-dispatch cadence; the heartbeat keeps last_seen_at well inside
// the backend's JobHeartbeatTTL (2m).
const (
	specialistJobAdmitPoll      = 5 * time.Second
	specialistJobAdmitWait      = 30 * time.Minute
	specialistJobHeartbeat      = 15 * time.Second
	specialistJobGitOutputLimit = 4 << 20
)

// Verdicts and the closed completion reason vocabulary
// (docs/spec/specialist-job-v1.schema.json $defs.completion.reason).
const (
	sjVerdictPass         = "pass"
	sjVerdictFail         = "fail"
	sjVerdictInconclusive = "inconclusive"

	sjReasonKindNotExecutable = "kind_not_executable"
	sjReasonMaterializeFailed = "materialize_failed"
	sjReasonUnresolvableSHA   = "unresolvable_sha"
	sjReasonExecutionError    = "execution_error"
	sjReasonTimeout           = "timeout"
	sjReasonProbeError        = "probe_error"
	sjReasonInputsInvalid     = "inputs_invalid"
	// sjReasonNotApplicable: the job cannot apply on this host (a host.*
	// inventory id on an OS outside the per-OS table, #4330).
	sjReasonNotApplicable = "not_applicable"
)

// Named refusals. Each is returned wrapped; assert with errors.Is.
var (
	errSpecialistJobRouteRefused     = errors.New("specialist job: route outside the job allow-list refused")
	errSpecialistJobWriteRefused     = errors.New("specialist job: git write or remote mutation refused")
	errSpecialistJobNotScratch       = errors.New("specialist job: directory is not inside the job's own scratch parent")
	errSpecialistJobCloneTainted     = errors.New("specialist job: git refused in a clone that has executed repository code")
	errSpecialistJobNotRunnable      = errors.New("specialist job: kind is not runner-executable (draft renders in fishhawkd)")
	errSpecialistJobAdmissionTimeout = errors.New("specialist job: not admitted within the admission wait")
)

// specialistJobOutcome is what a kind executor returns: the verdict, an
// optional closed reason, and the kind's typed evidence (nil only on an
// inconclusive verdict with a reason).
type specialistJobOutcome struct {
	Verdict  string
	Reason   string
	Evidence any
}

// specialistJobExecutor executes one kind against r.
type specialistJobExecutor func(ctx context.Context, r *specialistJobRun) specialistJobOutcome

// specialistJobExecutors is the kind registry. Kind files register through
// registerSpecialistJobExecutor from init(); an unregistered kind ships
// inconclusive/kind_not_executable.
var specialistJobExecutors = map[string]specialistJobExecutor{}

func registerSpecialistJobExecutor(kind string, fn specialistJobExecutor) {
	if _, dup := specialistJobExecutors[kind]; dup {
		panic("specialist job: duplicate executor for kind " + kind)
	}
	specialistJobExecutors[kind] = fn
}

// specialistJobDeps are the subcommand's seams. Production values come from
// defaultSpecialistJobDeps; tests swap single fields.
type specialistJobDeps struct {
	admitPoll   time.Duration
	admitWait   time.Duration
	heartbeat   time.Duration
	newExecutor func(ctx context.Context, source string) *counterfactualExecutor
	gitRun      func(ctx context.Context, dir string, args []string) (string, int, error)
	baseRT      http.RoundTripper
}

func defaultSpecialistJobDeps() specialistJobDeps {
	return specialistJobDeps{
		admitPoll:   specialistJobAdmitPoll,
		admitWait:   specialistJobAdmitWait,
		heartbeat:   specialistJobHeartbeat,
		newExecutor: newCounterfactualExecutor,
		gitRun:      hostJobGitRun,
		baseRT:      http.DefaultTransport,
	}
}

var sjDeps = defaultSpecialistJobDeps()

// specialistJobRun is what a kind executor receives.
type specialistJobRun struct {
	job     *upload.SpecialistJob
	source  string // the clone SOURCE; never executed in
	parent  string // the job's own os.MkdirTemp scratch parent
	x       *counterfactualExecutor
	git     *jobGit
	logSink io.Writer
	// host overrides the host.* inventory seams (nil = the real host,
	// defaultHostInventorySeams); tests set it per run.
	host *hostInventorySeams
}

// Bearer sources, in preference order: the runner attestation token (a
// credential holding attest:specialist-jobs, so its admission carries an
// attestation key) and the operator bearer fishhawk-mcp forwards.
const (
	envRunnerAttestToken = "FISHHAWK_RUNNER_ATTEST_TOKEN"
	envOperatorToken     = "FISHHAWK_API_TOKEN"
)

// Attestation degrade reasons (specialist_job_attestation_degraded): the
// result ships unsigned and records provenance bearer.
const (
	sjAttestNoKey          = "no_attestation_key"
	sjAttestKeyUndecodable = "attestation_key_undecodable"
)

// specialistJobFlags is the parsed subcommand command line.
type specialistJobFlags struct {
	jobID, backendURL, workingDir, host, token string
	// tokenSource names the env var token came from (never its value).
	tokenSource string
}

func parseSpecialistJobFlags(args []string, logSink io.Writer) (specialistJobFlags, error) {
	var f specialistJobFlags
	fs := flag.NewFlagSet("specialist-job", flag.ContinueOnError)
	fs.SetOutput(logSink)
	fs.StringVar(&f.jobID, "job-id", "", "specialist job id (required)")
	fs.StringVar(&f.backendURL, "backend-url", "", "fishhawkd base URL (required)")
	fs.StringVar(&f.workingDir, "working-dir", "", "absolute path of the clone SOURCE checkout (required)")
	fs.StringVar(&f.host, "host", "", "host label for the local-verify:<host> admission group (required)")
	if err := fs.Parse(args); err != nil {
		return f, err
	}
	f.token, f.tokenSource = os.Getenv(envRunnerAttestToken), envRunnerAttestToken
	if f.token == "" {
		f.token, f.tokenSource = os.Getenv(envOperatorToken), envOperatorToken
	}
	switch {
	case f.jobID == "" || f.backendURL == "" || f.workingDir == "" || f.host == "":
		return f, errors.New("--job-id, --backend-url, --working-dir and --host are required")
	case !filepath.IsAbs(f.workingDir):
		return f, fmt.Errorf("--working-dir %q must be absolute", f.workingDir)
	case f.token == "":
		return f, errors.New(envRunnerAttestToken + " (runner attestation token) or " + envOperatorToken + " (operator bearer) must carry a bearer")
	}
	f.backendURL = strings.TrimRight(f.backendURL, "/")
	return f, nil
}

// runSpecialistJob is the `specialist-job` subcommand body. Exit codes:
// exitOK once a completion shipped (whatever its verdict), exitUsage on a bad
// command line, exitFailure on every refusal or transport failure.
func runSpecialistJob(args []string, logSink io.Writer) int {
	f, err := parseSpecialistJobFlags(args, logSink)
	if err != nil {
		logEvent(logSink, "specialist_job_usage", map[string]string{"error": err.Error()})
		return exitUsage
	}
	ctx, stop := newRunnerContext()
	defer stop()
	if err := executeSpecialistJob(ctx, f, sjDeps, logSink); err != nil {
		logEvent(logSink, "specialist_job_failed", map[string]string{"job_id": f.jobID, "error": err.Error()})
		return exitFailure
	}
	return exitOK
}

// executeSpecialistJob fetches, admits, executes and ships one job.
func executeSpecialistJob(ctx context.Context, f specialistJobFlags, d specialistJobDeps, logSink io.Writer) error {
	client, err := newSpecialistJobClient(f.backendURL, f.jobID, d.baseRT)
	if err != nil {
		return err
	}
	job, err := client.FetchSpecialistJob(ctx, f.token, f.jobID)
	if err != nil {
		return err
	}
	if job.Kind == "draft" {
		return fmt.Errorf("%w: job %s", errSpecialistJobNotRunnable, f.jobID)
	}
	nonce, err := newSpecialistJobNonce()
	if err != nil {
		return err
	}
	adm, err := admitSpecialistJob(ctx, client, f, nonce, d, logSink)
	if err != nil {
		return err
	}
	key := specialistJobAttestationKey(adm, f, logSink)
	hbCtx, stopHB := context.WithCancel(ctx)
	var hbWG sync.WaitGroup
	hbWG.Add(1)
	go func() {
		defer hbWG.Done()
		heartbeatSpecialistJob(hbCtx, client, f, nonce, d.heartbeat, logSink)
	}()
	start := time.Now()
	outcome := runSpecialistJobKind(ctx, job, f, d, logSink)
	stopHB()
	hbWG.Wait()
	completion, err := specialistJobCompletion(job.Kind, nonce, outcome, time.Since(start))
	if err != nil {
		return err
	}
	if _, err := client.ShipSpecialistJobCompletion(ctx, f.token, f.jobID, completion, key); err != nil {
		if !completionLanded(ctx, client, f, err) {
			return err
		}
		logEvent(logSink, "specialist_job_completion_already_landed", map[string]string{"job_id": f.jobID})
	}
	logEvent(logSink, "specialist_job_completed", map[string]string{
		"job_id": f.jobID, "kind": job.Kind, "verdict": outcome.Verdict, "reason": outcome.Reason,
		"signed": fmt.Sprint(key != nil),
	})
	return nil
}

// specialistJobAttestationKey decodes the admission's job-scoped attestation
// key: base64 (std) of a 64-byte Ed25519 private key whose public half matches
// its seed. The key lives only in the returned value — never logged, never
// written. No key, or one that does not decode, logs
// specialist_job_attestation_degraded (reason and token source only, no key
// material) and returns nil, so the result ships unsigned and records
// provenance bearer.
func specialistJobAttestationKey(adm *upload.SpecialistJobAdmission, f specialistJobFlags, logSink io.Writer) ed25519.PrivateKey {
	reason := sjAttestNoKey
	if adm.AttestationKey != "" {
		key, err := decodeSpecialistJobAttestationKey(adm.AttestationKey)
		if err == nil {
			return key
		}
		reason = sjAttestKeyUndecodable
	}
	logEvent(logSink, "specialist_job_attestation_degraded", map[string]string{
		"job_id": f.jobID, "reason": reason, "token_source": f.tokenSource, "provenance": "bearer",
	})
	return nil
}

// decodeSpecialistJobAttestationKey refuses anything but base64 (std) of a
// 64-byte Ed25519 private key whose stored public half is the one its seed
// derives (a corrupted key would sign a result the server then refuses).
// The error never carries key material.
func decodeSpecialistJobAttestationKey(s string) (ed25519.PrivateKey, error) {
	raw, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		return nil, errors.New("attestation key is not base64")
	}
	if len(raw) != ed25519.PrivateKeySize {
		return nil, fmt.Errorf("attestation key is %d bytes, want %d", len(raw), ed25519.PrivateKeySize)
	}
	key := ed25519.PrivateKey(raw)
	if !bytes.Equal(ed25519.NewKeyFromSeed(key.Seed()), key) {
		return nil, errors.New("attestation key public half does not match its seed")
	}
	return key, nil
}

// runSpecialistJobKind sets up the job's scratch parent and runs the
// registered executor; an unregistered kind is inconclusive.
func runSpecialistJobKind(ctx context.Context, job *upload.SpecialistJob, f specialistJobFlags, d specialistJobDeps, logSink io.Writer) specialistJobOutcome {
	fn, ok := specialistJobExecutors[job.Kind]
	if !ok {
		return specialistJobOutcome{Verdict: sjVerdictInconclusive, Reason: sjReasonKindNotExecutable}
	}
	parent, err := os.MkdirTemp("", "fishhawk-specialist-job-")
	if err != nil {
		return specialistJobOutcome{Verdict: sjVerdictInconclusive, Reason: sjReasonMaterializeFailed}
	}
	defer func() { _ = os.RemoveAll(parent) }()
	x := d.newExecutor(ctx, f.workingDir)
	x.materialize = guardScratchMaterialize(x.materialize)
	r := &specialistJobRun{
		job: job, source: f.workingDir, parent: parent, x: x,
		git:     &jobGit{parent: parent, run: d.gitRun, tainted: map[string]bool{}},
		logSink: logSink,
	}
	return fn(ctx, r)
}

// specialistJobCompletion builds the POST .../result body.
func specialistJobCompletion(kind, nonce string, o specialistJobOutcome, elapsed time.Duration) (upload.SpecialistJobCompletion, error) {
	c := upload.SpecialistJobCompletion{
		SchemaVersion:  specialistJobSchemaVersion,
		ExecutionNonce: nonce,
		Kind:           kind,
		Verdict:        o.Verdict,
		Reason:         o.Reason,
		DurationMS:     elapsed.Milliseconds(),
	}
	if o.Evidence != nil {
		raw, err := json.Marshal(o.Evidence)
		if err != nil {
			return c, fmt.Errorf("specialist job: marshal evidence: %w", err)
		}
		c.Evidence = raw
	}
	return c, nil
}

// completionLanded resolves the one ambiguous completion refusal: a
// `409 specialist_job_not_running` can mean a retried POST whose first attempt
// already landed, so the job is re-read and the shipment counts as landed only
// when the job now reads completed. Every other error is a real failure.
func completionLanded(ctx context.Context, client *upload.Client, f specialistJobFlags, err error) bool {
	if !errors.Is(err, upload.ErrSpecialistJobConflict) || !strings.Contains(err.Error(), "specialist_job_not_running") {
		return false
	}
	job, ferr := client.FetchSpecialistJob(ctx, f.token, f.jobID)
	return ferr == nil && job.State == "completed"
}

// admitSpecialistJob POSTs .../admission until admitted, sleeping the poll
// ±20% between attempts, and gives up after the admission wait leaving the job
// queued (the read surface marks it stale). It returns the admitting answer,
// which alone carries the attestation key.
func admitSpecialistJob(ctx context.Context, client *upload.Client, f specialistJobFlags, nonce string, d specialistJobDeps, logSink io.Writer) (*upload.SpecialistJobAdmission, error) {
	deadline := time.Now().Add(d.admitWait)
	for {
		adm, err := client.AdmitSpecialistJob(ctx, f.token, f.jobID, upload.SpecialistJobAdmissionRequest{Host: f.host, Nonce: nonce})
		if err != nil {
			return nil, err
		}
		if adm.Admitted {
			logEvent(logSink, "specialist_job_admitted", map[string]string{
				"job_id": f.jobID, "host": f.host, "taken_over": fmt.Sprint(adm.TakenOver),
				"token_source": f.tokenSource, "attestation_key": fmt.Sprint(adm.AttestationKey != ""),
			})
			return adm, nil
		}
		logEvent(logSink, "specialist_job_queued", map[string]string{
			"job_id": f.jobID, "position": fmt.Sprint(adm.Position), "contended": fmt.Sprint(adm.Contended),
		})
		if !time.Now().Before(deadline) {
			return nil, fmt.Errorf("%w: %s after %s", errSpecialistJobAdmissionTimeout, f.jobID, d.admitWait)
		}
		if !sleepCtx(ctx, jitter(d.admitPoll)) {
			return nil, ctx.Err()
		}
	}
}

// heartbeatSpecialistJob re-POSTs .../admission with the admitted nonce every
// interval until ctx ends. A failed heartbeat is logged, never fatal: a job
// whose heartbeat lapses is taken over under a new nonce and this runner's
// completion then fails nonce_mismatch.
func heartbeatSpecialistJob(ctx context.Context, client *upload.Client, f specialistJobFlags, nonce string, interval time.Duration, logSink io.Writer) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		if _, err := client.AdmitSpecialistJob(ctx, f.token, f.jobID, upload.SpecialistJobAdmissionRequest{Host: f.host, Nonce: nonce}); err != nil && ctx.Err() == nil {
			logEvent(logSink, "specialist_job_heartbeat_failed", map[string]string{"job_id": f.jobID, "error": err.Error()})
		}
	}
}

// jitter returns d ±20%.
func jitter(d time.Duration) time.Duration {
	span := int64(d) / 5
	if span <= 0 {
		return d
	}
	n, err := rand.Int(rand.Reader, big.NewInt(2*span+1))
	if err != nil {
		return d
	}
	return d - time.Duration(span) + time.Duration(n.Int64())
}

func newSpecialistJobNonce() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("specialist job: nonce: %w", err)
	}
	return hex.EncodeToString(b), nil
}

// newSpecialistJobClient is the upload client every job call goes through,
// its transport confined by specialistJobTransport.
func newSpecialistJobClient(backendURL, jobID string, base http.RoundTripper) (*upload.Client, error) {
	u, err := url.Parse(backendURL)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return nil, fmt.Errorf("specialist job: bad --backend-url %q", backendURL)
	}
	c := upload.New(backendURL)
	c.HTTP.Transport = &specialistJobTransport{base: base, scheme: u.Scheme, host: u.Host, basePath: strings.TrimRight(u.Path, "/"), jobID: jobID}
	return c, nil
}

// specialistJobTransport is control (a): an allow-list RoundTripper. It admits
// exactly GET <base>/v0/specialist-jobs/<job> and POST .../admission and
// .../result on the configured backend host — redirects included, since
// http.Client re-enters the transport — and refuses everything else before
// any network I/O.
type specialistJobTransport struct {
	base                   http.RoundTripper
	scheme, host, basePath string
	jobID                  string
}

func (t *specialistJobTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if !t.allowed(req) {
		if req.Body != nil {
			_ = req.Body.Close()
		}
		return nil, fmt.Errorf("%w: %s %s", errSpecialistJobRouteRefused, req.Method, req.URL.Redacted())
	}
	return t.base.RoundTrip(req)
}

func (t *specialistJobTransport) allowed(req *http.Request) bool {
	u := req.URL
	if u == nil || u.Scheme != t.scheme || u.Host != t.host || u.RawQuery != "" || t.jobID == "" {
		return false
	}
	job := t.basePath + "/v0/specialist-jobs/" + t.jobID
	switch req.Method {
	case http.MethodGet:
		return u.Path == job
	case http.MethodPost:
		return u.Path == job+"/admission" || u.Path == job+"/result"
	}
	return false
}

// requireScratchDir is control (c): dir must resolve STRICTLY inside parent
// (both through EvalSymlinks, so /var -> /private/var still matches). A dir
// equal to parent, outside it, or unresolvable is refused.
func requireScratchDir(dir, parent string) error {
	if dir == "" || parent == "" {
		return fmt.Errorf("%w: empty path", errSpecialistJobNotScratch)
	}
	realParent, err := filepath.EvalSymlinks(parent)
	if err != nil {
		return fmt.Errorf("%w: parent %q: %v", errSpecialistJobNotScratch, parent, err)
	}
	realDir, err := filepath.EvalSymlinks(dir)
	if err != nil {
		return fmt.Errorf("%w: %q: %v", errSpecialistJobNotScratch, dir, err)
	}
	rel, err := filepath.Rel(realParent, realDir)
	if err != nil || rel == "." || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return fmt.Errorf("%w: %q is not inside %q", errSpecialistJobNotScratch, dir, parent)
	}
	return nil
}

// guardScratchMaterialize wraps a materialize seam so every clone it returns
// passes requireScratchDir against the parent it was asked to clone into.
// The counterfactual executor's seam is wrapped with it too.
func guardScratchMaterialize(inner func(ctx context.Context, repoDir, headSHA, parent string) (string, error)) func(ctx context.Context, repoDir, headSHA, parent string) (string, error) {
	return func(ctx context.Context, repoDir, headSHA, parent string) (string, error) {
		dir, err := inner(ctx, repoDir, headSHA, parent)
		if err != nil {
			return "", err
		}
		if err := requireScratchDir(dir, parent); err != nil {
			return "", err
		}
		return dir, nil
	}
}

// freshClone materializes a new clone of sha in its own subdirectory of the
// job's scratch parent.
func (r *specialistJobRun) freshClone(ctx context.Context, sha string) (string, error) {
	sub, err := os.MkdirTemp(r.parent, "clone-")
	if err != nil {
		return "", err
	}
	dir, err := r.x.materialize(ctx, r.source, sha, sub)
	if err != nil {
		return "", err
	}
	if err := requireScratchDir(dir, r.parent); err != nil {
		return "", err
	}
	return dir, nil
}

// materializeReason maps a freshClone error onto the closed reason.
func materializeReason(err error) string {
	if errors.Is(err, gateiso.ErrCommitNotFound) {
		return sjReasonUnresolvableSHA
	}
	return sjReasonMaterializeFailed
}

// execInClone is the ONE door job code takes to run a command that executes
// repository code (go test, go list): it refuses a non-scratch clone, marks
// the clone tainted so jobGit refuses it from then on, and runs argv through
// the gate exec seam under the verify lock. A lock that cannot be taken runs
// nothing and returns the lock error.
func (r *specialistJobRun) execInClone(ctx context.Context, clone, runDir string, argv []string) (string, int, gateDisposition, error) {
	if err := requireScratchDir(clone, r.parent); err != nil {
		return "", -1, gateExecuted, err
	}
	r.git.taint(clone)
	release, err := r.x.acquireLock(ctx)
	if err != nil {
		return "", -1, gateExecuted, err
	}
	defer release()
	out, code, disp := r.x.exec(ctx, argv, runDir, filepath.Join(r.parent, "golangci-lint-cache"), r.x.runTimeout)
	return out, code, disp, nil
}

// jobGit is control (b): the ONLY way job code runs git.
type jobGit struct {
	parent  string
	run     func(ctx context.Context, dir string, args []string) (string, int, error)
	mu      sync.Mutex
	tainted map[string]bool
}

// taint records that clone executed repository code.
func (g *jobGit) taint(clone string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.tainted[filepath.Clean(clone)] = true
}

func (g *jobGit) isTainted(dir string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	d := filepath.Clean(dir)
	for c := range g.tainted {
		if d == c || strings.HasPrefix(d, c+string(filepath.Separator)) {
			return true
		}
	}
	return false
}

// Run runs `git <args>` in dir and returns stdout and the exit code. It
// refuses a write subcommand, a dir outside the scratch parent, and a
// tainted clone, before any process starts.
func (g *jobGit) Run(ctx context.Context, dir string, args ...string) (string, int, error) {
	if err := refuseJobGitWrite(args); err != nil {
		return "", -1, err
	}
	if err := requireScratchDir(dir, g.parent); err != nil {
		return "", -1, err
	}
	if g.isTainted(dir) {
		return "", -1, fmt.Errorf("%w: %s", errSpecialistJobCloneTainted, dir)
	}
	return g.run(ctx, dir, args)
}

// refuseJobGitWrite refuses a leading global option (which could redirect
// the dir or the config), push / send-pack, and a remote mutation.
func refuseJobGitWrite(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("%w: empty git command", errSpecialistJobWriteRefused)
	}
	sub := args[0]
	switch {
	case strings.HasPrefix(sub, "-"):
		return fmt.Errorf("%w: global option %q", errSpecialistJobWriteRefused, sub)
	case sub == "push" || sub == "send-pack":
		return fmt.Errorf("%w: git %s", errSpecialistJobWriteRefused, sub)
	case sub == "remote":
		for _, a := range args[1:] {
			switch a {
			case "add", "set-url", "rename", "remove", "rm", "set-head", "set-branches":
				return fmt.Errorf("%w: git remote %s", errSpecialistJobWriteRefused, a)
			}
		}
	}
	return nil
}

// hostJobGitRun is the production jobGit runner: hardened host git with the
// operator's global and system config pinned off and stdout capped.
func hostJobGitRun(ctx context.Context, dir string, args []string) (string, int, error) {
	cmd := exec.CommandContext(ctx, "git", append(gitops.HardeningArgs(), args...)...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_TERMINAL_PROMPT=0",
	)
	var stdout cappedBuffer
	stdout.limit = specialistJobGitOutputLimit
	cmd.Stdout = &stdout
	err := cmd.Run()
	var exitErr *exec.ExitError
	switch {
	case err == nil:
		return stdout.String(), 0, nil
	case errors.As(err, &exitErr):
		return stdout.String(), exitErr.ExitCode(), nil
	}
	return stdout.String(), -1, err
}

// cappedBuffer keeps the first limit bytes and discards the rest, so a
// runaway command cannot exhaust memory.
type cappedBuffer struct {
	strings.Builder
	limit     int
	truncated bool
}

func (b *cappedBuffer) Write(p []byte) (int, error) {
	if room := b.limit - b.Len(); room < len(p) {
		b.truncated = true
		if room > 0 {
			b.Builder.Write(p[:room])
		}
		return len(p), nil
	}
	b.Builder.Write(p)
	return len(p), nil
}
