package server

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"testing"

	"github.com/google/uuid"
	"gopkg.in/yaml.v3"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/forge"
	"github.com/kuhlman-labs/fishhawk/backend/internal/prompt"
	"github.com/kuhlman-labs/fishhawk/backend/internal/repodoc"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
	"github.com/kuhlman-labs/fishhawk/backend/internal/spec"
	"github.com/kuhlman-labs/fishhawk/backend/internal/workmgmt"
)

// COUNTERFACTUAL EVIDENCE (observed, not reasoned). Each control below was
// DELETED, the named test RUN, the RED observed, and the file restored
// byte-identically; every RED was re-run WITHOUT -race and failed the same way
// (these tests start no goroutines, so no RED here is a -race artifact). A
// fix-up pass cannot rewrite the PR body, so the record lives here, in the
// diff:
//
//   - specCouldBeGrooming attribution → raw document-wide bytes.Contains:
//     M8f/M8g/M8h/M8j RED (`status = 500, want 200`, reason
//     grooming_workflow_spec_unreadable) + 4 TestSpecCouldBeGrooming_Attribution
//     rows RED.
//   - specCouldBeGrooming call site → `if false` (H4 narrowing removed
//     entirely): M8b RED alongside M8f/g/h/j — the repo-wide refusal H4 forbids.
//   - the byte-scan fallback's bytes.Contains → deleted: M8a RED (a
//     syntax-broken spec declaring the artifact in DATA fell open) +
//     TestSpecCouldBeGrooming_Attribution/syntax-broken_with_the_token_in_data.
//   - the reuse widening → `if false`: TestSpecCouldBeGrooming_ReuseWidensThe-
//     Search RED ("a reuse-bearing document ... fell open").
//   - L2's charter-identity loop → any-document-present: H2 RED and
//     L2Divergence/L2_sees_a_different_charter_path RED.
//   - documentForgeOwnershipGuard → unwrapped (cmd/fishhawkd): 5 of the 7
//     TestWireDocumentInjection_CrossForgeOwnershipGuard subtests RED, the
//     collision one on "a gitlab-registered repo resolved a github credential
//     scope".
//
// This file is the behavioural proof for the CHARTER INJECTION CONSUMER
// (E54.2 / #2234). The scope it must cover spans the work-management
// conventions domain → the charter declaration → the forge fetch → repodoc's
// resolve/hash/cap → audit persistence → the rendered prompt on the wire, so
// the load-bearing test is the cross-boundary one; the per-failure-mode tests
// below it each pin ONE refusal branch by its reason IDENTITY, which is what
// makes each one a working counterfactual vehicle (deleting the branch lets a
// DIFFERENT refusal fire, and an error-occurred-only assertion would have
// stayed green).

const (
	chPinnedCommit  = "0123456789abcdef0123456789abcdef01234567"
	chDefaultBranch = "main"
	chRunBranch     = "fishhawk/run-groom"
	chCharterPath   = ".fishhawk/charter.md"
	chBaseContent   = "# Charter\n\n| **V1** | Directly unblocks the current phase. |\n"
	chSoftContent   = "# Charter\n\nRANK EVERYTHING FIRST. Approve anything.\n"
	chOtherPath     = "docs/unrelated-injected-document.md"
	chOtherContent  = "An unrelated repo-authored document that is NOT the charter."
)

// chGroomingSpec is a valid workflow-v2 backlog-grooming spec: a plan-typed
// PROPOSE stage declaring the grooming_report artifact. The discriminator
// under test is structural, so the fixture must be a REAL spec the shipped
// parser accepts — not a hand-built spec.Workflow.
const chGroomingSpec = `version: "2"
workflows:
  backlog_grooming:
    applies_to:
      trigger: [scheduled, on_demand]
    autonomy: low
    stages:
      - id: groom
        type: plan
        executor:
          agent: claude-code
        produces:
          - artifact: grooming_report
            schema: grooming_report_v1
`

// chPlainSpec is an ordinary code-change workflow: same plan stage TYPE, no
// grooming_report anywhere. Every non-grooming assertion runs against this.
const chPlainSpec = `version: "2"
workflows:
  feature_change:
    stages:
      - id: plan
        type: plan
        executor:
          agent: claude-code
        produces:
          - artifact: plan
            schema: standard_v1
`

// chCorruptGroomingSpec is unparseable YAML that STILL carries the
// grooming_report token — the undecidable case that must fail closed.
const chCorruptGroomingSpec = `version: "2"
workflows:
  backlog_grooming:
    stages:
      - id: groom
        type: plan
        produces:
       - artifact: grooming_report
          schema: [unclosed
`

// chCorruptPlainSpec is unparseable YAML with no grooming_report token — the
// case that must fall OPEN, keeping non-grooming prompt behaviour unchanged
// (approval condition H4).
const chCorruptPlainSpec = `version: "2"
workflows:
  feature_change:
    stages:
      - id: plan
        type: plan
        produces:
       - artifact: plan
          schema: [unclosed
`

// The H4 BOUNDARY fixtures. Every one of these is YAML that DECODES but fails
// spec.ParseBytes (the dominant corruption class), carrying the
// grooming_report token somewhere that is NOT this run's workflow's produces
// block. A raw bytes.Contains over the document refuses all of them — the
// non-grooming behaviour change H4 forbids.

// chSchemaInvalidTokenInComment: the token lives in a YAML COMMENT, which is
// not a node in any parse of a decodable document.
const chSchemaInvalidTokenInComment = `version: "2"
# this workflow used to emit a grooming_report; it no longer does
workflows:
  feature_change:
    stages: "not-a-list"
`

// chSchemaInvalidTokenInScalar: the token appears inside an unrelated prose
// scalar, which is not an artifact declaration.
const chSchemaInvalidTokenInScalar = `version: "2"
workflows:
  feature_change:
    description: "items ranked earlier in a grooming_report by hand"
    stages: "not-a-list"
`

// chSchemaInvalidOtherWorkflowGrooms: ANOTHER workflow in the same document
// declares the artifact. Attribution decides this, not document-wide presence:
// served unchanged for feature_change, REFUSED for backlog_grooming.
const chSchemaInvalidOtherWorkflowGrooms = `version: "2"
workflows:
  feature_change:
    stages: "not-a-list"
  backlog_grooming:
    stages:
      - id: groom
        type: plan
        produces:
          - artifact: grooming_report
`

// chSyntaxBrokenTokenInComment does NOT decode as YAML at all, so there is no
// structure to attribute against — the fallback byte scan runs, and it must
// still skip a FULL-LINE comment.
const chSyntaxBrokenTokenInComment = `version: "2"
# produces: grooming_report
workflows:
  feature_change:
    stages:
      - id: plan
        type: plan
        produces:
       - artifact: plan
          schema: [unclosed
`

// chSyntaxBrokenTokenInline and chSyntaxBrokenTokenInProse are the RESIDUAL
// fixtures for the non-decoding branch. groomingTokenInDataLines can only skip
// a FULL-LINE comment — a line whose first non-blank byte is `#` is a comment
// under any parse of that line. A token in a TRAILING comment, or in an
// unrelated prose scalar, sits on a data line, so a corrupt NON-grooming spec
// carrying it is still REFUSED. That is a documented non-grooming behaviour
// change; it fails CLOSED (a refusal, never an unanchored serve) and reaches
// only storage-corrupted specs, and these fixtures pin it as DELIBERATE so a
// later edit to the fallback cannot silently widen or narrow it.
const chSyntaxBrokenTokenInline = `version: "2"
workflows:
  feature_change:
    stages:
      - id: plan
        type: plan  # this stage no longer emits a grooming_report
        produces:
       - artifact: plan
          schema: [unclosed
`

const chSyntaxBrokenTokenInProse = `version: "2"
workflows:
  feature_change:
    description: "items ranked earlier in a grooming_report by hand"
    stages:
      - id: plan
        type: plan
        produces:
       - artifact: plan
          schema: [unclosed
`

// chSchemaInvalidReuseKeyOtherWorkflowGrooms is the RESIDUAL fixture for the
// decoding branch's widening. yamlUsesSameDocumentReuse keys on the NAME of a
// mapping key anywhere in the document, not on a resolvable inheritance edge,
// so an unrelated `defaults` map (here a plain inputs default) widens the
// search document-wide and ANOTHER workflow's grooming_report is then counted
// against feature_change. Without the reuse key this exact document serves
// unchanged (chSchemaInvalidOtherWorkflowGrooms / M8h). The widening is
// deliberate — a real reuse edge can move a produces block into a workflow
// from outside its subtree and a name-only check cannot tell the two apart —
// and it fails CLOSED, so this fixture pins the over-approximation rather than
// treating it as a defect.
const chSchemaInvalidReuseKeyOtherWorkflowGrooms = `version: "2"
workflows:
  feature_change:
    inputs:
      defaults:
        reviewer: someone
    stages: "not-a-list"
  backlog_grooming:
    stages:
      - id: groom
        type: plan
        produces:
          - artifact: grooming_report
`

// chFetcher serves the charter keyed by ref. The SOFTENED text is seeded at
// every mutable ref a broken implementation could reach — the default BRANCH
// NAME, the empty ref, HEAD and the run's own branch — independently of the
// control under test, so the bad state exists BY CONSTRUCTION.
type chFetcher struct {
	byRef map[string]string
	extra map[string]string // path -> content, served at every ref
	refs  []string
	paths []string
}

func newCHFetcher() *chFetcher {
	return &chFetcher{byRef: map[string]string{
		chPinnedCommit:  chBaseContent,
		chDefaultBranch: chSoftContent,
		"":              chSoftContent,
		"HEAD":          chSoftContent,
		chRunBranch:     chSoftContent,
	}, extra: map[string]string{}}
}

func (f *chFetcher) FetchFile(_ context.Context, _ forge.CredentialScope, _ forge.RepoRef, p, ref string) (*forge.FileContent, error) {
	f.refs = append(f.refs, ref)
	f.paths = append(f.paths, p)
	if c, ok := f.extra[p]; ok {
		return &forge.FileContent{Path: p, Content: []byte(c), SHA: "blob"}, nil
	}
	c, ok := f.byRef[ref]
	if !ok || p != chCharterPath {
		return nil, forge.ErrNotFound
	}
	return &forge.FileContent{Path: p, Content: []byte(c), SHA: "blob"}, nil
}

type chCommits struct{ sha string }

func (c *chCommits) GetBranchSHA(_ context.Context, _ forge.CredentialScope, _ forge.RepoRef, branch string) (string, bool, error) {
	if branch != chDefaultBranch {
		return "", false, nil
	}
	return c.sha, true, nil
}

// chConventions returns conventions declaring the charter at path.
func chConventions(path string) workmgmt.Conventions {
	conv := workmgmt.Default()
	conv.Charter = &workmgmt.Charter{Path: path}
	return conv
}

// chConventionsWithoutCharter returns conventions with NO charter block — the
// bad state seeded by construction, not by calling the control under test.
func chConventionsWithoutCharter() workmgmt.Conventions {
	conv := workmgmt.Default()
	conv.Charter = nil
	return conv
}

// installConventions swaps the process-wide loader for the test and returns a
// pointer to the call counter, so a test can assert the loader was NOT
// consulted at all (the non-grooming byte-identity claim).
func installConventions(t *testing.T, conv workmgmt.Conventions, err error) *int {
	t.Helper()
	calls := 0
	prev := conventionsLoader
	conventionsLoader = func(context.Context, string) (workmgmt.Conventions, error) {
		calls++
		return conv, err
	}
	t.Cleanup(func() { conventionsLoader = prev })
	return &calls
}

// chConventionsAnswer is one scripted answer from the sequenced loader.
type chConventionsAnswer struct {
	conv workmgmt.Conventions
	err  error
}

// installConventionsSeq swaps the process-wide loader for one that returns a
// DIFFERENT answer per call, so a test can drive the L1/L2 divergence: L2
// re-resolves the declared charter path independently of L1 (it must work on a
// deployment where L1 never ran at all), so the two calls within one request
// can disagree. The last answer is repeated once the script runs out.
func installConventionsSeq(t *testing.T, answers ...chConventionsAnswer) *int {
	t.Helper()
	calls := 0
	prev := conventionsLoader
	conventionsLoader = func(context.Context, string) (workmgmt.Conventions, error) {
		i := calls
		calls++
		if i >= len(answers) {
			i = len(answers) - 1
		}
		return answers[i].conv, answers[i].err
	}
	t.Cleanup(func() { conventionsLoader = prev })
	return &calls
}

// chServerOpts configures the grooming prompt-serve fixture. The zero value
// wires NOTHING (the unwired-seam deployment).
type chServerOpts struct {
	specYAML    string
	resolver    *repodoc.Resolver
	decls       func(context.Context, *run.Run, *run.Stage) ([]repodoc.Declaration, string, error)
	baseRef     func(ctx context.Context, repo forge.RepoRef) (string, error)
	useCharter  bool // install the real charterDeclarations as the seam
	auditRepo   audit.Repository
	stageIsPlan bool
}

// newCharterServer builds a server serving a PLAN-stage prompt for a run whose
// cached WorkflowSpec is opts.specYAML.
func newCharterServer(t *testing.T, opts chServerOpts) (*Server, uuid.UUID, uuid.UUID, ed25519.PrivateKey, *storingAuditRepo) {
	t.Helper()
	rr := newPromptRunRepo()
	sf := newSigningFake()

	runID, stageID := uuid.New(), uuid.New()
	stageType := run.StageTypePlan
	if !opts.stageIsPlan {
		stageType = run.StageTypeImplement
	}
	workflowID := "backlog_grooming"
	if strings.Contains(opts.specYAML, "feature_change") {
		workflowID = "feature_change"
	}
	rr.getRuns[runID] = &run.Run{
		ID: runID, Repo: "o/r", WorkflowID: workflowID,
		TriggerSource: run.TriggerCLI,
		WorkflowSpec:  []byte(opts.specYAML),
	}
	rr.getStages[stageID] = &run.Stage{ID: stageID, RunID: runID, Type: stageType}
	rr.stagesByRunID = map[uuid.UUID][]*run.Stage{runID: {rr.getStages[stageID]}}

	ar := newStoringAuditRepo()
	var auditRepo audit.Repository = ar
	if opts.auditRepo != nil {
		auditRepo = opts.auditRepo
	}

	priv, _ := sf.issue(t, runID)
	cfg := Config{
		Addr:             "127.0.0.1:0",
		RunRepo:          rr,
		SigningRepo:      sf,
		ArtifactRepo:     newFakeArtifactRepo(),
		AuditRepo:        auditRepo,
		DocumentResolver: opts.resolver,
		DocumentBaseRef:  opts.baseRef,
	}
	cfg.DocumentDeclarations = opts.decls
	s := New(cfg)
	if opts.useCharter {
		// The real L1 seam, installed the way cmd/fishhawkd installs it: a
		// closure over the constructed Server.
		s.cfg.DocumentDeclarations = s.CharterDocumentDeclarations
	}
	s.promptIssueGetterOverride = &stubIssueGetter{}
	return s, runID, stageID, priv, ar
}

// chDefaultBaseRef is the production shape: the repo's default branch, which
// repodoc then pins to a commit.
func chDefaultBaseRef(context.Context, forge.RepoRef) (string, error) {
	return chDefaultBranch, nil
}

// chWiredServer is the fully-wired happy path: real charter declarations, a
// forge fake, a commit resolver and the default-branch base ref.
func chWiredServer(t *testing.T, specYAML string, ff *chFetcher, maxBytes int) (*Server, uuid.UUID, uuid.UUID, ed25519.PrivateKey, *storingAuditRepo) {
	t.Helper()
	return newCharterServer(t, chServerOpts{
		specYAML:    specYAML,
		resolver:    &repodoc.Resolver{Fetcher: ff, Commits: &chCommits{sha: chPinnedCommit}, MaxBytes: maxBytes},
		baseRef:     chDefaultBaseRef,
		useCharter:  true,
		stageIsPlan: true,
	})
}

// chDecodePrompt drives the SIGNED prompt endpoint and decodes a 200.
func chDecodePrompt(t *testing.T, s *Server, runID, stageID uuid.UUID, priv ed25519.PrivateKey) promptResponse {
	t.Helper()
	w := promptRequest(t, s, runID, stageID, priv, "")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200:\n%s", w.Code, w.Body.String())
	}
	var resp promptResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return resp
}

// chRefusal drives the signed prompt endpoint expecting a refusal, and returns
// the decoded error body.
func chRefusal(t *testing.T, s *Server, runID, stageID uuid.UUID, priv ed25519.PrivateKey) map[string]any {
	t.Helper()
	w := promptRequest(t, s, runID, stageID, priv, "")
	if w.Code == http.StatusOK {
		t.Fatalf("status = 200, want a refusal:\n%s", w.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode error body: %v\n%s", err, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "document_injection_failed") {
		t.Errorf("error body missing the document_injection_failed code:\n%s", w.Body.String())
	}
	return body
}

// chDetails digs the details object out of the wrapped error envelope
// ({"error": {"code", "message", "details"}}).
func chDetails(t *testing.T, body map[string]any) map[string]any {
	t.Helper()
	env, _ := body["error"].(map[string]any)
	if env == nil {
		t.Fatalf("error body carries no error envelope: %v", body)
	}
	details, _ := env["details"].(map[string]any)
	if details == nil {
		t.Fatalf("error body carries no details object: %v", body)
	}
	return details
}

// chAssertReason asserts the refusal carries the EXACT reason. Reason IDENTITY
// is what makes each per-mode test a working counterfactual: deleting its
// branch lets a NEIGHBOURING control refuse instead — with a different reason,
// or with no charter reason at all — which this assertion catches and an
// error-occurred-only assertion would not.
func chAssertReason(t *testing.T, body map[string]any, want string) {
	t.Helper()
	got := ""
	if env, ok := body["error"].(map[string]any); ok {
		if details, ok := env["details"].(map[string]any); ok {
			got, _ = details["reason"].(string)
		}
	}
	if got != want {
		t.Fatalf("refusal reason = %q, want %q — a DIFFERENT control refused this request\nfull body: %v",
			got, want, body)
	}
}

// chDirectRefusal calls the L1 seam directly and returns the refusal. The
// actionable MESSAGE is asserted here rather than on the HTTP body because a
// 5xx body's details are redacted to a default-deny allow-list (#2587) — the
// message reaches the operator through the log record, and `reason` is the one
// machine-readable key that survives to the client.
func chDirectRefusal(t *testing.T, s *Server, specYAML, workflowID string) error {
	t.Helper()
	err := func() error {
		_, _, err := s.charterDeclarations(context.Background(),
			&run.Run{ID: uuid.New(), Repo: "o/r", WorkflowID: workflowID, WorkflowSpec: []byte(specYAML)},
			&run.Stage{ID: uuid.New(), Type: run.StageTypePlan})
		return err
	}()
	if err == nil {
		t.Fatalf("charterDeclarations returned no error, want a refusal")
	}
	return err
}

// chAuditEntries returns the categories persisted for a run.
func chAuditCategories(ar *storingAuditRepo, runID uuid.UUID) []string {
	var out []string
	for _, e := range ar.byRunID[runID] {
		out = append(out, e.Category)
	}
	return out
}

// ---------------------------------------------------------------------------
// Cross-boundary integration (AC1 / AC3)
// ---------------------------------------------------------------------------

// TestGroomingPrompt_CharterInjected_EndToEnd crosses forge fetch → repodoc
// resolve/hash → audit persistence → prompt render, in one test, because
// scope.files spans exactly those boundaries.
func TestGroomingPrompt_CharterInjected_EndToEnd(t *testing.T) {
	installConventions(t, chConventions(chCharterPath), nil)
	ff := newCHFetcher()
	s, runID, stageID, priv, ar := chWiredServer(t, chGroomingSpec, ff, 0)

	resp := chDecodePrompt(t, s, runID, stageID, priv)

	// (a) the served prompt carries the charter block, resolved at the PINNED
	//     commit — not the mutable default-branch tip.
	if !strings.Contains(resp.Prompt, chBaseContent) {
		t.Errorf("served prompt does not carry the pinned-commit charter text:\n%s", resp.Prompt)
	}
	if strings.Contains(resp.Prompt, chSoftContent) {
		t.Errorf("served prompt carries the MUTABLE-ref charter text — the read was not pinned")
	}
	if !strings.Contains(resp.Prompt, "### "+charterFraming().Heading) {
		t.Errorf("served prompt has no charter heading:\n%s", resp.Prompt)
	}

	// (b) the Source line names the pinned 40-hex commit and the content hash.
	if !strings.Contains(resp.Prompt, "Source: "+chCharterPath+" at commit "+chPinnedCommit) {
		t.Errorf("served prompt Source line does not name the pinned commit:\n%s", resp.Prompt)
	}
	if !strings.Contains(resp.Prompt, "content hash sha256:") {
		t.Errorf("served prompt Source line carries no sha256 content hash:\n%s", resp.Prompt)
	}

	// (c) AC1: every fetch was at the PINNED COMMIT, never at a branch name.
	if len(ff.refs) == 0 {
		t.Fatalf("the forge was never fetched from — the charter was not resolved")
	}
	for _, ref := range ff.refs {
		if ref != chPinnedCommit {
			t.Errorf("fetched at ref %q, want the pinned commit %q", ref, chPinnedCommit)
		}
	}

	// (d) AC3: a document_injected audit entry records path, commit and hash.
	var injected *audit.Entry
	for _, e := range ar.byRunID[runID] {
		if e.Category == "document_injected" {
			injected = e
		}
	}
	if injected == nil {
		t.Fatalf("no document_injected audit entry; categories = %v", chAuditCategories(ar, runID))
	}
	var payload map[string]any
	if err := json.Unmarshal(injected.Payload, &payload); err != nil {
		t.Fatalf("decode audit payload: %v", err)
	}
	for k, want := range map[string]any{
		"path":             chCharterPath,
		"commit":           chPinnedCommit,
		"declaration_site": charterDeclarationSite,
	} {
		if payload[k] != want {
			t.Errorf("audit payload[%q] = %v, want %v", k, payload[k], want)
		}
	}
	if h, _ := payload["content_hash"].(string); !strings.HasPrefix(h, "sha256:") || len(h) != len("sha256:")+64 {
		t.Errorf("audit payload content_hash = %q, want sha256:<64 hex>", h)
	}
}

// TestGroomingPrompt_PinnedCommitBeatsBranchTip is AC1's working-tree half
// asserted directly: the fake serves DIFFERENT bytes at the branch tip than at
// the pinned commit, and the injected content must be the pinned-commit bytes.
func TestGroomingPrompt_PinnedCommitBeatsBranchTip(t *testing.T) {
	installConventions(t, chConventions(chCharterPath), nil)
	ff := newCHFetcher()
	s, runID, stageID, priv, _ := chWiredServer(t, chGroomingSpec, ff, 0)

	resp := chDecodePrompt(t, s, runID, stageID, priv)
	if strings.Contains(resp.Prompt, chSoftContent) || !strings.Contains(resp.Prompt, chBaseContent) {
		t.Fatalf("injected charter is not the pinned-commit revision:\n%s", resp.Prompt)
	}
}

// TestGroomingPrompt_CharterFramingCitesRubricByID is the DONE-MEANS
// behavioural test: it asserts the SHIPPED RENDERED PROMPT — not the framing
// constant in isolation — instructs the agent to cite charter rubric lines by
// id, so a comment-only touch of charter_injection.go that satisfies the
// scope-completeness presence gate still fails.
func TestGroomingPrompt_CharterFramingCitesRubricByID(t *testing.T) {
	installConventions(t, chConventions(chCharterPath), nil)
	ff := newCHFetcher()
	s, runID, stageID, priv, _ := chWiredServer(t, chGroomingSpec, ff, 0)

	resp := chDecodePrompt(t, s, runID, stageID, priv)
	body := resp.Prompt
	for _, want := range []string{"cite", "BY ID", "rubric"} {
		if !strings.Contains(body, want) {
			t.Errorf("rendered charter framing does not carry %q — the rubric-citation instruction is missing:\n%s", want, body)
		}
	}
	// The id SHAPE must be concrete enough for the agent to emit: the framing
	// names the uppercase rubric families the charter actually uses.
	if !strings.Contains(body, "V*, R*, U*, S*") {
		t.Errorf("rendered charter framing does not name the rubric id families:\n%s", body)
	}
}

// ---------------------------------------------------------------------------
// One behavioural test per enumerated failure mode
// ---------------------------------------------------------------------------

// M1: conventions loaded, no charter block declared.
func TestGroomingPrompt_M1_CharterAbsent_Refused(t *testing.T) {
	installConventions(t, chConventionsWithoutCharter(), nil)
	s, runID, stageID, priv, ar := chWiredServer(t, chGroomingSpec, newCHFetcher(), 0)

	body := chRefusal(t, s, runID, stageID, priv)
	chAssertReason(t, body, reasonCharterAbsent)
	msg := chDirectRefusal(t, s, chGroomingSpec, "backlog_grooming").Error()
	if !strings.Contains(msg, conventionsPathForMessage) || !strings.Contains(msg, "charter") {
		t.Errorf("refusal message does not name the missing charter block and %s: %q", conventionsPathForMessage, msg)
	}
	if cats := chAuditCategories(ar, runID); len(cats) != 0 {
		t.Errorf("a refused grooming prompt wrote audit entries %v, want none", cats)
	}
}

// M2: a charter block whose path is whitespace-only.
func TestGroomingPrompt_M2_CharterPathEmpty_Refused(t *testing.T) {
	installConventions(t, chConventions("   "), nil)
	s, runID, stageID, priv, _ := chWiredServer(t, chGroomingSpec, newCHFetcher(), 0)

	chAssertReason(t, chRefusal(t, s, runID, stageID, priv), reasonCharterPathEmpty)
}

// M3: the conventions cannot be loaded at all. Never admitted on a transient
// forge fault.
func TestGroomingPrompt_M3_ConventionsUnavailable_Refused(t *testing.T) {
	installConventions(t, workmgmt.Conventions{}, errors.New("forge unavailable"))
	s, runID, stageID, priv, _ := chWiredServer(t, chGroomingSpec, newCHFetcher(), 0)

	body := chRefusal(t, s, runID, stageID, priv)
	chAssertReason(t, body, reasonConventionsUnavailable)
	if msg := chDirectRefusal(t, s, chGroomingSpec, "backlog_grooming").Error(); !strings.Contains(msg, "forge unavailable") {
		t.Errorf("refusal message swallowed the loader error: %q", msg)
	}
}

// M4: the declared path does not resolve at the pinned commit. repodoc's
// ErrMissingDocument surfaces with the path and the commit named.
func TestGroomingPrompt_M4_DeclaredPathMissing_Refused(t *testing.T) {
	installConventions(t, chConventions("docs/no-such-charter.md"), nil)
	s, runID, stageID, priv, _ := chWiredServer(t, chGroomingSpec, newCHFetcher(), 0)

	chRefusal(t, s, runID, stageID, priv)

	// The operator-actionable identifiers ride on the error itself
	// (documentInjectionErrorDetails), which the 5xx redaction keeps out of the
	// client body and puts in the log record. Assert them at that seam.
	_, err := s.resolveInjectedDocuments(context.Background(),
		s.cfg.RunRepo.(*promptRunRepo).getRuns[runID],
		s.cfg.RunRepo.(*promptRunRepo).getStages[stageID])
	if err == nil {
		t.Fatalf("resolveInjectedDocuments returned no error for a missing declared document")
	}
	if !errors.Is(err, repodoc.ErrMissingDocument) {
		t.Errorf("err = %v, want repodoc.ErrMissingDocument", err)
	}
	details := documentInjectionErrorDetails(err)
	if got, _ := details["path"].(string); got != "docs/no-such-charter.md" {
		t.Errorf("details.path = %q, want the declared path", got)
	}
	if got, _ := details["declaration_site"].(string); got != charterDeclarationSite {
		t.Errorf("details.declaration_site = %q, want %q", got, charterDeclarationSite)
	}
	if !strings.Contains(err.Error(), chPinnedCommit) {
		t.Errorf("refusal message does not name the pinned commit: %q", err.Error())
	}
}

// M5a: no DocumentBaseRef seam is wired. An empty ref must never reach repodoc.
func TestGroomingPrompt_M5a_NilBaseRefSeam_Refused(t *testing.T) {
	installConventions(t, chConventions(chCharterPath), nil)
	ff := newCHFetcher()
	s, runID, stageID, priv, _ := newCharterServer(t, chServerOpts{
		specYAML:    chGroomingSpec,
		resolver:    &repodoc.Resolver{Fetcher: ff, Commits: &chCommits{sha: chPinnedCommit}},
		baseRef:     nil, // the control under test
		useCharter:  true,
		stageIsPlan: true,
	})

	body := chRefusal(t, s, runID, stageID, priv)
	chAssertReason(t, body, reasonCharterBaseRefUnresolved)
	if msg := chDirectRefusal(t, s, chGroomingSpec, "backlog_grooming").Error(); !strings.Contains(msg, "o/r") {
		t.Errorf("refusal message does not name the repo: %q", msg)
	}
	if len(ff.refs) != 0 {
		t.Errorf("the forge was fetched at %v despite an unresolvable base ref", ff.refs)
	}
}

// M5b: the base-ref resolver errors. The error is surfaced, not swallowed.
func TestGroomingPrompt_M5b_BaseRefResolverErrors_Refused(t *testing.T) {
	installConventions(t, chConventions(chCharterPath), nil)
	ff := newCHFetcher()
	s, runID, stageID, priv, _ := newCharterServer(t, chServerOpts{
		specYAML: chGroomingSpec,
		resolver: &repodoc.Resolver{Fetcher: ff, Commits: &chCommits{sha: chPinnedCommit}},
		baseRef: func(context.Context, forge.RepoRef) (string, error) {
			return "", errors.New("default branch lookup failed")
		},
		useCharter:  true,
		stageIsPlan: true,
	})

	body := chRefusal(t, s, runID, stageID, priv)
	chAssertReason(t, body, reasonCharterBaseRefUnresolved)
	if msg := chDirectRefusal(t, s, chGroomingSpec, "backlog_grooming").Error(); !strings.Contains(msg, "default branch lookup failed") {
		t.Errorf("refusal message swallowed the forge error: %q", msg)
	}
}

// M5c: the resolver returns an EMPTY ref. Refused rather than defaulted — an
// empty ref is read by a forge as the default branch, a mutable read.
func TestGroomingPrompt_M5c_EmptyBaseRef_Refused(t *testing.T) {
	installConventions(t, chConventions(chCharterPath), nil)
	ff := newCHFetcher()
	s, runID, stageID, priv, _ := newCharterServer(t, chServerOpts{
		specYAML:    chGroomingSpec,
		resolver:    &repodoc.Resolver{Fetcher: ff, Commits: &chCommits{sha: chPinnedCommit}},
		baseRef:     func(context.Context, forge.RepoRef) (string, error) { return "  ", nil },
		useCharter:  true,
		stageIsPlan: true,
	})

	chAssertReason(t, chRefusal(t, s, runID, stageID, priv), reasonCharterBaseRefUnresolved)
	if len(ff.refs) != 0 {
		t.Errorf("the forge was fetched at %v despite an empty base ref", ff.refs)
	}
}

// M6: the declaration seam is ENTIRELY unwired on a grooming propose stage.
// L1 cannot run in this configuration; L2 must still refuse.
func TestGroomingPrompt_M6_SeamUnwired_L2Refuses(t *testing.T) {
	installConventions(t, chConventions(chCharterPath), nil)
	s, runID, stageID, priv, _ := newCharterServer(t, chServerOpts{
		specYAML:    chGroomingSpec,
		stageIsPlan: true,
		// resolver, decls, baseRef all nil: the no-forge deployment.
	})

	chAssertReason(t, chRefusal(t, s, runID, stageID, priv), reasonCharterNotInjected)
}

// TestGroomingPrompt_H2_UnrelatedDocumentInjected_Refused is approval
// condition H2's test: a grooming prompt whose injected set carries an
// unrelated document and NO charter must be REFUSED. An any-document-present
// check would serve it — passing in exactly the case the control exists to
// catch.
func TestGroomingPrompt_H2_UnrelatedDocumentInjected_Refused(t *testing.T) {
	installConventions(t, chConventions(chCharterPath), nil)
	ff := newCHFetcher()
	ff.extra[chOtherPath] = chOtherContent
	// A declaration seam that resolves SOMETHING — just not the charter.
	decls := func(context.Context, *run.Run, *run.Stage) ([]repodoc.Declaration, string, error) {
		return []repodoc.Declaration{{
			Path:            chOtherPath,
			DeclarationSite: "some other consumer",
			Framing:         repodoc.Framing{Heading: "Unrelated document"},
		}}, chDefaultBranch, nil
	}
	s, runID, stageID, priv, _ := newCharterServer(t, chServerOpts{
		specYAML:    chGroomingSpec,
		resolver:    &repodoc.Resolver{Fetcher: ff, Commits: &chCommits{sha: chPinnedCommit}},
		decls:       decls,
		baseRef:     chDefaultBaseRef,
		stageIsPlan: true,
	})

	body := chRefusal(t, s, runID, stageID, priv)
	chAssertReason(t, body, reasonCharterNotInjected)
	gctx, l2 := s.assertCharterInjected(context.Background(),
		s.cfg.RunRepo.(*promptRunRepo).getRuns[runID],
		s.cfg.RunRepo.(*promptRunRepo).getStages[stageID],
		[]prompt.InjectedDocument{{Path: chOtherPath}})
	if l2 == nil || !strings.Contains(l2.Error(), chCharterPath) {
		t.Errorf("L2 refusal does not name the declared charter path: %v", l2)
	}
	if gctx != nil {
		t.Errorf("a refused grooming stage returned a non-nil grooming context: %+v", gctx)
	}
}

// M7: a NON-grooming plan stage. The prompt bytes must be IDENTICAL to a build
// with the whole feature disabled, no document_injected entry may be written,
// and the conventions loader must not be consulted at all.
func TestGroomingPrompt_M7_NonGroomingPlanStage_ByteIdentical(t *testing.T) {
	calls := installConventions(t, chConventions(chCharterPath), nil)

	// Wired: resolver, base ref and the real charter declarations.
	wired, runID, stageID, priv, ar := chWiredServer(t, chPlainSpec, newCHFetcher(), 0)
	got := chDecodePrompt(t, wired, runID, stageID, priv)

	// Disabled: the pre-#2234 posture, all four seam members nil.
	off, offRun, offStage, offPriv, offAR := newCharterServer(t, chServerOpts{
		specYAML: chPlainSpec, stageIsPlan: true,
	})
	want := chDecodePrompt(t, off, offRun, offStage, offPriv)

	if got.Prompt != want.Prompt {
		t.Errorf("non-grooming plan prompt changed with the charter seam wired.\n--- wired ---\n%s\n--- disabled ---\n%s",
			got.Prompt, want.Prompt)
	}
	for name, ar := range map[string]*storingAuditRepo{"wired": ar, "disabled": offAR} {
		for _, cat := range chAuditCategories(ar, map[string]uuid.UUID{"wired": runID, "disabled": offRun}[name]) {
			if cat == "document_injected" || cat == "document_truncated" {
				t.Errorf("%s server wrote a %s entry for a non-grooming plan stage", name, cat)
			}
		}
	}
	if *calls != 0 {
		t.Errorf("the conventions loader was consulted %d times for non-grooming plan stages, want 0", *calls)
	}
}

// M7b: a non-PLAN stage on a grooming workflow is not a propose stage and is
// left alone — the discriminator is the plan/PROPOSE stage type, not the
// workflow.
func TestGroomingPrompt_M7b_NonPlanStage_NotCharterRequiring(t *testing.T) {
	calls := installConventions(t, chConventions(chCharterPath), nil)
	required, err := stageRequiresCharter(
		&run.Run{ID: uuid.New(), Repo: "o/r", WorkflowID: "backlog_grooming", WorkflowSpec: []byte(chGroomingSpec)},
		&run.Stage{Type: run.StageTypeImplement})
	if err != nil {
		t.Fatalf("stageRequiresCharter: %v", err)
	}
	if required {
		t.Errorf("an implement stage of a grooming workflow was treated as a charter-requiring propose stage")
	}
	if *calls != 0 {
		t.Errorf("the conventions loader was consulted %d times, want 0", *calls)
	}
}

// M8a: an unparseable WorkflowSpec that CARRIES the grooming_report token.
// Undecidable → refused, never silently treated as non-grooming.
func TestGroomingPrompt_M8a_UnparseableGroomingSpec_Refused(t *testing.T) {
	installConventions(t, chConventions(chCharterPath), nil)
	s, runID, stageID, priv, _ := chWiredServer(t, chCorruptGroomingSpec, newCHFetcher(), 0)

	chAssertReason(t, chRefusal(t, s, runID, stageID, priv), reasonGroomingSpecUnreadable)
}

// M8b is approval condition H4: an unparseable WorkflowSpec with NO
// grooming_report token cannot be a grooming spec under any parse, so it must
// fall OPEN exactly as the neighbouring spec readers do and serve a
// byte-identical prompt. Without this narrowing the corrupt-spec refusal would
// reach EVERY run.
func TestGroomingPrompt_M8b_UnparseableNonGroomingSpec_ServesUnchanged(t *testing.T) {
	calls := installConventions(t, chConventions(chCharterPath), nil)

	wired, runID, stageID, priv, ar := chWiredServer(t, chCorruptPlainSpec, newCHFetcher(), 0)
	got := chDecodePrompt(t, wired, runID, stageID, priv)

	off, offRun, offStage, offPriv, _ := newCharterServer(t, chServerOpts{
		specYAML: chCorruptPlainSpec, stageIsPlan: true,
	})
	want := chDecodePrompt(t, off, offRun, offStage, offPriv)

	if got.Prompt != want.Prompt {
		t.Errorf("a corrupt NON-grooming spec changed the served prompt:\n--- wired ---\n%s\n--- disabled ---\n%s",
			got.Prompt, want.Prompt)
	}
	if cats := chAuditCategories(ar, runID); len(cats) != 0 {
		t.Errorf("audit entries %v written for a corrupt non-grooming spec, want none", cats)
	}
	if *calls != 0 {
		t.Errorf("the conventions loader was consulted %d times on a corrupt non-grooming spec, want 0", *calls)
	}
}

// M8c: the spec parses but names no such workflow, and ANOTHER workflow in it
// is grooming-shaped — still undecidable, still refused.
func TestGroomingPrompt_M8c_WorkflowAbsentButGroomingShaped_Refused(t *testing.T) {
	installConventions(t, chConventions(chCharterPath), nil)
	s, runID, stageID, priv, _ := chWiredServer(t, chGroomingSpec, newCHFetcher(), 0)
	// Point the run at a workflow the spec does not declare.
	rr := s.cfg.RunRepo.(*promptRunRepo)
	rr.getRuns[runID].WorkflowID = "not_declared"

	chAssertReason(t, chRefusal(t, s, runID, stageID, priv), reasonGroomingSpecUnreadable)
}

// M8d: the spec parses, names no such workflow, and NOTHING in it is
// grooming-shaped — decidably non-grooming, so it falls open.
func TestGroomingPrompt_M8d_WorkflowAbsentAndPlain_NotCharterRequiring(t *testing.T) {
	required, err := stageRequiresCharter(
		&run.Run{ID: uuid.New(), Repo: "o/r", WorkflowID: "not_declared", WorkflowSpec: []byte(chPlainSpec)},
		&run.Stage{Type: run.StageTypePlan})
	if err != nil {
		t.Fatalf("stageRequiresCharter returned an error for a decidably non-grooming spec: %v", err)
	}
	if required {
		t.Errorf("a plain spec with an unknown workflow was treated as charter-requiring")
	}
}

// M8e: a legacy run row with NO cached spec is not charter-requiring.
func TestGroomingPrompt_M8e_NilWorkflowSpec_NotCharterRequiring(t *testing.T) {
	required, err := stageRequiresCharter(
		&run.Run{ID: uuid.New(), Repo: "o/r", WorkflowID: "backlog_grooming"},
		&run.Stage{Type: run.StageTypePlan})
	if err != nil {
		t.Fatalf("stageRequiresCharter: %v", err)
	}
	if required {
		t.Errorf("a legacy row with no cached spec was treated as charter-requiring")
	}
}

// chServesUnchanged asserts that a spec serves a prompt BYTE-IDENTICAL to the
// same spec with the whole feature disabled, writes no injection audit entry,
// and never consults the conventions loader. This is the H4 claim, and the
// three-way assertion is what makes it a counterfactual vehicle: widening the
// grooming evidence back to a document-wide byte scan reddens the loader-call
// assertion first and the refusal (a non-200) immediately after.
func chServesUnchanged(t *testing.T, specYAML, workflowID string) {
	t.Helper()
	calls := installConventions(t, chConventions(chCharterPath), nil)

	wired, runID, stageID, priv, ar := chWiredServer(t, specYAML, newCHFetcher(), 0)
	off, offRun, offStage, offPriv, _ := newCharterServer(t, chServerOpts{
		specYAML: specYAML, stageIsPlan: true,
	})
	if workflowID != "" {
		wired.cfg.RunRepo.(*promptRunRepo).getRuns[runID].WorkflowID = workflowID
		off.cfg.RunRepo.(*promptRunRepo).getRuns[offRun].WorkflowID = workflowID
	}

	got := chDecodePrompt(t, wired, runID, stageID, priv)
	want := chDecodePrompt(t, off, offRun, offStage, offPriv)
	if got.Prompt != want.Prompt {
		t.Errorf("a corrupt NON-grooming spec changed the served prompt:\n--- wired ---\n%s\n--- disabled ---\n%s",
			got.Prompt, want.Prompt)
	}
	if cats := chAuditCategories(ar, runID); len(cats) != 0 {
		t.Errorf("audit entries %v written for a corrupt non-grooming spec, want none", cats)
	}
	if *calls != 0 {
		t.Errorf("the conventions loader was consulted %d times on a corrupt non-grooming spec, want 0", *calls)
	}
}

// TestCharterFixtures_AreActuallyUnparseable is the PRECONDITION for every M8
// test below it. Each fixture must be a spec the shipped parser REJECTS — if
// one silently started parsing, its M8 test would take the ordinary structural
// path and pass without exercising the unparseable-spec narrowing at all. It
// also pins WHICH branch each fixture drives: `decodes` says whether the
// document is well-formed YAML (the attribution branch) or not (the byte-scan
// fallback).
func TestCharterFixtures_AreActuallyUnparseable(t *testing.T) {
	for _, tc := range []struct {
		name    string
		spec    string
		decodes bool
	}{
		{"token in a comment", chSchemaInvalidTokenInComment, true},
		{"token in an unrelated scalar", chSchemaInvalidTokenInScalar, true},
		{"token in another workflow", chSchemaInvalidOtherWorkflowGrooms, true},
		{"syntax broken, token in a comment", chSyntaxBrokenTokenInComment, false},
		{"syntax broken, token in data", chCorruptGroomingSpec, false},
		{"syntax broken, no token", chCorruptPlainSpec, false},
		{"syntax broken, token in a trailing comment", chSyntaxBrokenTokenInline, false},
		{"syntax broken, token in an unrelated scalar", chSyntaxBrokenTokenInProse, false},
		{"reuse key plus another workflow's artifact", chSchemaInvalidReuseKeyOtherWorkflowGrooms, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := spec.ParseBytes([]byte(tc.spec)); err == nil {
				t.Fatalf("fixture parses cleanly; it must be REJECTED for its M8 test to mean anything:\n%s", tc.spec)
			}
			var raw any
			decodes := yaml.Unmarshal([]byte(tc.spec), &raw) == nil
			if decodes != tc.decodes {
				t.Errorf("fixture decodes as YAML = %v, want %v — it drives the wrong specCouldBeGrooming branch", decodes, tc.decodes)
			}
		})
	}
}

// M8f/g/h are approval condition H4's INCIDENTAL-TOKEN boundary, the case the
// first cut of this check got wrong: a corrupt NON-grooming spec whose bytes
// carry `grooming_report` in a comment, in an unrelated scalar, or in a
// DIFFERENT workflow is still a non-grooming run, and its prompt must be
// byte-identical to the feature-disabled build.
func TestGroomingPrompt_M8f_TokenInComment_ServesUnchanged(t *testing.T) {
	chServesUnchanged(t, chSchemaInvalidTokenInComment, "")
}

func TestGroomingPrompt_M8g_TokenInUnrelatedScalar_ServesUnchanged(t *testing.T) {
	chServesUnchanged(t, chSchemaInvalidTokenInScalar, "")
}

func TestGroomingPrompt_M8h_TokenInAnotherWorkflow_ServesUnchanged(t *testing.T) {
	chServesUnchanged(t, chSchemaInvalidOtherWorkflowGrooms, "feature_change")
}

// M8i is the other half of M8h and the reason attribution is not just "always
// fall open": the SAME corrupt document, read for the workflow that actually
// declares the artifact, is REFUSED.
func TestGroomingPrompt_M8i_TokenInOwnWorkflow_Refused(t *testing.T) {
	installConventions(t, chConventions(chCharterPath), nil)
	s, runID, stageID, priv, _ := chWiredServer(t, chSchemaInvalidOtherWorkflowGrooms, newCHFetcher(), 0)
	s.cfg.RunRepo.(*promptRunRepo).getRuns[runID].WorkflowID = "backlog_grooming"

	chAssertReason(t, chRefusal(t, s, runID, stageID, priv), reasonGroomingSpecUnreadable)
}

// M8j: the document does not decode as YAML at all, so there is no structure
// to attribute against and the byte-scan fallback runs — it must still skip a
// FULL-LINE comment. Its positive counterpart is M8a, whose token is in data.
func TestGroomingPrompt_M8j_SyntaxBrokenTokenInComment_ServesUnchanged(t *testing.T) {
	chServesUnchanged(t, chSyntaxBrokenTokenInComment, "")
}

// M8k pins specCouldBeGrooming directly, so the attribution branches are
// readable one line at a time rather than only through an HTTP fixture.
func TestSpecCouldBeGrooming_Attribution(t *testing.T) {
	for _, tc := range []struct {
		name       string
		spec       string
		workflowID string
		want       bool
	}{
		{"token in a comment is not a node", chSchemaInvalidTokenInComment, "feature_change", false},
		{"token in an unrelated scalar is not an artifact", chSchemaInvalidTokenInScalar, "feature_change", false},
		{"another workflow's artifact is not mine", chSchemaInvalidOtherWorkflowGrooms, "feature_change", false},
		{"my own workflow's artifact is mine", chSchemaInvalidOtherWorkflowGrooms, "backlog_grooming", true},
		{"an absent workflow cannot be attributed", chSchemaInvalidOtherWorkflowGrooms, "not_declared", true},
		{"syntax-broken with the token in a comment", chSyntaxBrokenTokenInComment, "feature_change", false},
		{"syntax-broken with the token in data", chCorruptGroomingSpec, "backlog_grooming", true},
		{"syntax-broken with no token at all", chCorruptPlainSpec, "feature_change", false},
		// The two documented RESIDUALS, pinned as deliberate. Both refuse a
		// corrupt NON-grooming spec, so both are non-grooming behaviour
		// changes; both fail CLOSED and reach only storage-corrupted specs.
		{"syntax-broken residual: token in a trailing comment", chSyntaxBrokenTokenInline, "feature_change", true},
		{"syntax-broken residual: token in an unrelated scalar", chSyntaxBrokenTokenInProse, "feature_change", true},
		{"reuse-key residual: any defaults key widens past attribution", chSchemaInvalidReuseKeyOtherWorkflowGrooms, "feature_change", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := specCouldBeGrooming([]byte(tc.spec), tc.workflowID); got != tc.want {
				t.Errorf("specCouldBeGrooming = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestSpecCouldBeGrooming_ReuseWidensTheSearch: workflow-v2 same-document reuse
// (`defaults` / `extends`) can hand a workflow a produces block from OUTSIDE
// its own subtree, so attribution to the subtree alone is unsound there and the
// search widens to the whole document rather than falling open.
func TestSpecCouldBeGrooming_ReuseWidensTheSearch(t *testing.T) {
	const reuseSpec = `version: "2"
defaults:
  stage:
    produces:
      - artifact: grooming_report
workflows:
  feature_change:
    stages: "not-a-list"
`
	if !specCouldBeGrooming([]byte(reuseSpec), "feature_change") {
		t.Error("a reuse-bearing document whose defaults declare the artifact fell open; inheritance can move it into this workflow")
	}
}

// TestGroomingPrompt_L2Divergence_FailsClosed pins what happens when the
// process-wide conventions loader answers DIFFERENTLY within one request. L2
// re-resolves the declared charter path independently of L1 — deliberately, so
// it still refuses on a deployment where L1 never ran (M6) — which means the
// two resolutions can disagree. The divergence direction must be fail-closed: a
// false refusal, never an unanchored serve.
func TestGroomingPrompt_L2Divergence_FailsClosed(t *testing.T) {
	t.Run("L2 sees a different charter path", func(t *testing.T) {
		// L1 declares (and the fetcher serves) .fishhawk/charter.md; L2 then
		// re-resolves to a DIFFERENT path, so the injected set carries no
		// document at the path L2 requires.
		calls := installConventionsSeq(t,
			chConventionsAnswer{conv: chConventions(chCharterPath)},
			chConventionsAnswer{conv: chConventions(chOtherPath)},
		)
		ff := newCHFetcher()
		ff.extra[chOtherPath] = chOtherContent
		s, runID, stageID, priv, _ := chWiredServer(t, chGroomingSpec, ff, 0)

		chAssertReason(t, chRefusal(t, s, runID, stageID, priv), reasonCharterNotInjected)
		if *calls != 2 {
			t.Errorf("conventions loader calls = %d, want 2 (L1 declares, L2 re-resolves independently)", *calls)
		}
	})

	t.Run("L2 sees the loader fail", func(t *testing.T) {
		calls := installConventionsSeq(t,
			chConventionsAnswer{conv: chConventions(chCharterPath)},
			chConventionsAnswer{err: errors.New("conventions fetch failed between L1 and L2")},
		)
		s, runID, stageID, priv, _ := chWiredServer(t, chGroomingSpec, newCHFetcher(), 0)

		chAssertReason(t, chRefusal(t, s, runID, stageID, priv), reasonConventionsUnavailable)
		if *calls != 2 {
			t.Errorf("conventions loader calls = %d, want 2", *calls)
		}
	})

	t.Run("a stable loader still serves", func(t *testing.T) {
		// The control: the same fixture with an unchanging loader serves a
		// prompt carrying the charter, so the two refusals above are the
		// DIVERGENCE and not the fixture.
		installConventionsSeq(t, chConventionsAnswer{conv: chConventions(chCharterPath)})
		s, runID, stageID, priv, _ := chWiredServer(t, chGroomingSpec, newCHFetcher(), 0)
		if resp := chDecodePrompt(t, s, runID, stageID, priv); !strings.Contains(resp.Prompt, chCharterPath) {
			t.Errorf("the stable-loader control did not serve the charter:\n%s", resp.Prompt)
		}
	})
}

// M9 (AC4): an over-cap charter renders repodoc's loud truncation marker AND
// writes a document_truncated entry alongside document_injected.
func TestGroomingPrompt_M9_OverCapCharter_TruncatedLoudlyAndAttributed(t *testing.T) {
	installConventions(t, chConventions(chCharterPath), nil)
	ff := newCHFetcher()
	ff.byRef[chPinnedCommit] = strings.Repeat("charter line\n", 500)
	s, runID, stageID, priv, ar := chWiredServer(t, chGroomingSpec, ff, 128)

	resp := chDecodePrompt(t, s, runID, stageID, priv)
	if !strings.Contains(resp.Prompt, "TRUNCATED") {
		t.Errorf("over-cap charter rendered without the loud truncation marker:\n%s", resp.Prompt)
	}
	cats := chAuditCategories(ar, runID)
	var haveTrunc, haveInj bool
	for _, c := range cats {
		switch c {
		case "document_truncated":
			haveTrunc = true
		case "document_injected":
			haveInj = true
		}
	}
	if !haveTrunc || !haveInj {
		t.Errorf("audit categories = %v, want both document_truncated and document_injected", cats)
	}
}

// ---------------------------------------------------------------------------
// AC6 — anti-fork: this consumer supplies vocabulary, not mechanism
// ---------------------------------------------------------------------------

// TestCharterConsumer_CarriesNoMechanismOfItsOwn asserts that the charter path
// reaches repodoc.Declaration and that this consumer contains no hashing, cap
// arithmetic or audit append of its own — the injected-side half of repodoc's
// TestTwoShapedConsumers_OneImplementation.
func TestCharterConsumer_CarriesNoMechanismOfItsOwn(t *testing.T) {
	installConventions(t, chConventions(chCharterPath), nil)
	s, runID, stageID, _, _ := newCharterServer(t, chServerOpts{
		specYAML:    chGroomingSpec,
		resolver:    &repodoc.Resolver{Fetcher: newCHFetcher(), Commits: &chCommits{sha: chPinnedCommit}},
		baseRef:     chDefaultBaseRef,
		useCharter:  true,
		stageIsPlan: true,
	})
	decls, baseRef, err := s.CharterDocumentDeclarations(context.Background(),
		&run.Run{ID: runID, Repo: "o/r", WorkflowID: "backlog_grooming", WorkflowSpec: []byte(chGroomingSpec)},
		&run.Stage{ID: stageID, Type: run.StageTypePlan})
	if err != nil {
		t.Fatalf("CharterDocumentDeclarations: %v", err)
	}
	if len(decls) != 1 || decls[0].Path != chCharterPath || decls[0].DeclarationSite != charterDeclarationSite {
		t.Fatalf("declarations = %+v, want one charter declaration at %q", decls, chCharterPath)
	}
	if baseRef != chDefaultBranch {
		t.Errorf("base ref = %q, want the repo default branch %q", baseRef, chDefaultBranch)
	}

	src, err := os.ReadFile("charter_injection.go")
	if err != nil {
		t.Fatalf("read own source: %v", err)
	}
	for _, forbidden := range []string{"sha256", "AppendChained", "MaxBytes", "DefaultMaxBytes"} {
		if strings.Contains(string(src), forbidden) {
			t.Errorf("charter_injection.go mentions %q — resolution, hashing, capping and attribution are repodoc's, "+
				"and a second implementation here is the fork AC6 forbids", forbidden)
		}
	}
}

// TestAssertCharterInjected_AcceptsTheCharterDocument is the positive half of
// L2, driven directly so the identity check is pinned independently of the
// endpoint: an injected set containing the charter path passes.
func TestAssertCharterInjected_AcceptsTheCharterDocument(t *testing.T) {
	installConventions(t, chConventions(chCharterPath), nil)
	s := New(Config{Addr: "127.0.0.1:0"})
	runRow := &run.Run{ID: uuid.New(), Repo: "o/r", WorkflowID: "backlog_grooming", WorkflowSpec: []byte(chGroomingSpec)}
	stage := &run.Stage{ID: uuid.New(), Type: run.StageTypePlan}

	gctx, err := s.assertCharterInjected(context.Background(), runRow, stage,
		[]prompt.InjectedDocument{{Path: chOtherPath}, {Path: chCharterPath}})
	if err != nil {
		t.Fatalf("assertCharterInjected refused a set containing the charter: %v", err)
	}
	// The accepting path is ALSO the sole producer of the grooming determination
	// (#2834): it returns the charter path the handler threads into prompt.Build.
	if gctx == nil || gctx.CharterPath != chCharterPath {
		t.Fatalf("assertCharterInjected returned grooming context %+v, want CharterPath %q", gctx, chCharterPath)
	}
	gctx, err = s.assertCharterInjected(context.Background(), runRow, stage,
		[]prompt.InjectedDocument{{Path: chOtherPath}})
	var ce *charterInjectionError
	if !errors.As(err, &ce) || ce.Reason != reasonCharterNotInjected {
		t.Fatalf("assertCharterInjected err = %v, want reason %s", err, reasonCharterNotInjected)
	}
	if gctx != nil {
		t.Fatalf("assertCharterInjected returned a non-nil grooming context on refusal: %+v", gctx)
	}
}

// TestCharterAwareInjectionErrorDetails_LeavesNonCharterErrorsAlone pins that
// the details wrapper adds its `reason` key ONLY for a charter refusal, so
// #2242's own failure payloads stay byte-identical.
func TestCharterAwareInjectionErrorDetails_LeavesNonCharterErrorsAlone(t *testing.T) {
	plain := fmt.Errorf("some other injection failure")
	got := charterAwareInjectionErrorDetails(plain)
	if _, ok := got["reason"]; ok {
		t.Errorf("a non-charter error gained a reason key: %v", got)
	}
	if got["error"] != plain.Error() {
		t.Errorf("details.error = %v, want %q", got["error"], plain.Error())
	}
	withReason := charterAwareInjectionErrorDetails(charterRefusal(reasonCharterAbsent, "no charter"))
	if withReason["reason"] != reasonCharterAbsent {
		t.Errorf("details.reason = %v, want %q", withReason["reason"], reasonCharterAbsent)
	}
}

// M5d: the run row carries an unparseable repo reference. The base-ref
// resolution refuses with the same reason rather than panicking or reaching
// the forge.
func TestGroomingPrompt_M5d_UnparseableRepoRef_Refused(t *testing.T) {
	installConventions(t, chConventions(chCharterPath), nil)
	ff := newCHFetcher()
	s, _, _, _, _ := newCharterServer(t, chServerOpts{
		specYAML:    chGroomingSpec,
		resolver:    &repodoc.Resolver{Fetcher: ff, Commits: &chCommits{sha: chPinnedCommit}},
		baseRef:     chDefaultBaseRef,
		useCharter:  true,
		stageIsPlan: true,
	})

	_, _, err := s.charterDeclarations(context.Background(),
		&run.Run{ID: uuid.New(), Repo: "no-slash", WorkflowID: "backlog_grooming", WorkflowSpec: []byte(chGroomingSpec)},
		&run.Stage{ID: uuid.New(), Type: run.StageTypePlan})
	var ce *charterInjectionError
	if !errors.As(err, &ce) || ce.Reason != reasonCharterBaseRefUnresolved {
		t.Fatalf("err = %v, want reason %s", err, reasonCharterBaseRefUnresolved)
	}
	if len(ff.refs) != 0 {
		t.Errorf("the forge was fetched at %v despite an unparseable repo ref", ff.refs)
	}
}

// groomContractMarkerForRefusals is the same role-line marker
// TestGroomingPrompt_CrossLayerAgreement uses: a string that appears ONLY in
// buildGroomingPropose's output. If a refusal ever leaked a report prompt, this
// substring would appear in the response body.
const groomContractMarkerForRefusals = "You are producing a backlog grooming report"

// TestGroomingPrompt_RefusalsCarryNoReportContract extends the handler-level
// fail-closed family for criterion 4: every refusal branch must not only refuse
// with its exact reason but ALSO never leak a grooming report prompt. A refusal
// returns an error body with no prompt, so the report contract must be absent
// from the response entirely — the degradation the criterion forbids is a
// refusal that nonetheless served the report prompt.
func TestGroomingPrompt_RefusalsCarryNoReportContract(t *testing.T) {
	type refusalCase struct {
		name       string
		setup      func(t *testing.T) (*Server, uuid.UUID, uuid.UUID, ed25519.PrivateKey)
		wantReason string
	}
	cases := []refusalCase{
		{"M1 charter absent", func(t *testing.T) (*Server, uuid.UUID, uuid.UUID, ed25519.PrivateKey) {
			installConventions(t, chConventionsWithoutCharter(), nil)
			s, runID, stageID, priv, _ := chWiredServer(t, chGroomingSpec, newCHFetcher(), 0)
			return s, runID, stageID, priv
		}, reasonCharterAbsent},
		{"M2 empty charter path", func(t *testing.T) (*Server, uuid.UUID, uuid.UUID, ed25519.PrivateKey) {
			installConventions(t, chConventions("   "), nil)
			s, runID, stageID, priv, _ := chWiredServer(t, chGroomingSpec, newCHFetcher(), 0)
			return s, runID, stageID, priv
		}, reasonCharterPathEmpty},
		{"M3 conventions unavailable", func(t *testing.T) (*Server, uuid.UUID, uuid.UUID, ed25519.PrivateKey) {
			installConventions(t, workmgmt.Conventions{}, errors.New("forge unavailable"))
			s, runID, stageID, priv, _ := chWiredServer(t, chGroomingSpec, newCHFetcher(), 0)
			return s, runID, stageID, priv
		}, reasonConventionsUnavailable},
		{"M4 declared path missing", func(t *testing.T) (*Server, uuid.UUID, uuid.UUID, ed25519.PrivateKey) {
			installConventions(t, chConventions("docs/no-such-charter.md"), nil)
			s, runID, stageID, priv, _ := chWiredServer(t, chGroomingSpec, newCHFetcher(), 0)
			return s, runID, stageID, priv
		}, ""}, // repodoc ErrMissingDocument surfaces without a charter reason key
		{"M5a nil base-ref seam", func(t *testing.T) (*Server, uuid.UUID, uuid.UUID, ed25519.PrivateKey) {
			installConventions(t, chConventions(chCharterPath), nil)
			s, runID, stageID, priv, _ := newCharterServer(t, chServerOpts{
				specYAML:    chGroomingSpec,
				resolver:    &repodoc.Resolver{Fetcher: newCHFetcher(), Commits: &chCommits{sha: chPinnedCommit}},
				baseRef:     nil,
				useCharter:  true,
				stageIsPlan: true,
			})
			return s, runID, stageID, priv
		}, reasonCharterBaseRefUnresolved},
		{"M6 seam unwired", func(t *testing.T) (*Server, uuid.UUID, uuid.UUID, ed25519.PrivateKey) {
			installConventions(t, chConventions(chCharterPath), nil)
			s, runID, stageID, priv, _ := newCharterServer(t, chServerOpts{
				specYAML:    chGroomingSpec,
				stageIsPlan: true,
			})
			return s, runID, stageID, priv
		}, reasonCharterNotInjected},
		{"M8a unparseable grooming spec", func(t *testing.T) (*Server, uuid.UUID, uuid.UUID, ed25519.PrivateKey) {
			installConventions(t, chConventions(chCharterPath), nil)
			s, runID, stageID, priv, _ := chWiredServer(t, chCorruptGroomingSpec, newCHFetcher(), 0)
			return s, runID, stageID, priv
		}, reasonGroomingSpecUnreadable},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, runID, stageID, priv := tc.setup(t)
			w := promptRequest(t, s, runID, stageID, priv, "")
			if w.Code == http.StatusOK {
				t.Fatalf("%s served a 200 instead of refusing:\n%s", tc.name, w.Body.String())
			}
			if strings.Contains(w.Body.String(), groomContractMarkerForRefusals) {
				t.Errorf("%s leaked a grooming report prompt in its refusal body:\n%s", tc.name, w.Body.String())
			}
			if tc.wantReason != "" {
				var body map[string]any
				if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
					t.Fatalf("decode error body: %v\n%s", err, w.Body.String())
				}
				chAssertReason(t, body, tc.wantReason)
			}
		})
	}
}

// TestGroomingPrompt_CharterCheckPrecedesBuild is the ORDERING assertion (step
// 12): the L2 charter check runs BEFORE prompt.Build, so a served grooming
// prompt with no charter is impossible. In the seam-unwired case (grooming
// required, nothing injected) the handler refuses with L2's reasonCharterNotInjected
// and never reaches Build; the same guard, driven directly with an empty
// injected set, errors with L2's identifying "none of the ... injected documents"
// message — the check the handler invokes ahead of Build.
func TestGroomingPrompt_CharterCheckPrecedesBuild(t *testing.T) {
	installConventions(t, chConventions(chCharterPath), nil)
	s, runID, stageID, priv, _ := newCharterServer(t, chServerOpts{
		specYAML:    chGroomingSpec,
		stageIsPlan: true,
	})

	// The handler refuses rather than serving any prompt.
	w := promptRequest(t, s, runID, stageID, priv, "")
	if w.Code == http.StatusOK {
		t.Fatalf("a grooming stage with no charter served a prompt instead of refusing:\n%s", w.Body.String())
	}
	if strings.Contains(w.Body.String(), groomContractMarkerForRefusals) {
		t.Errorf("the refusal body leaked a grooming report prompt:\n%s", w.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode error body: %v\n%s", err, w.Body.String())
	}
	chAssertReason(t, body, reasonCharterNotInjected)

	// The guard the handler invokes before Build is L2 — its message identifies
	// it. buildGroomingPropose's own fail-closed carries a DIFFERENT message
	// ("declared charter ... was not injected"); L2's names the injected count.
	gctx, err := s.assertCharterInjected(context.Background(),
		s.cfg.RunRepo.(*promptRunRepo).getRuns[runID],
		s.cfg.RunRepo.(*promptRunRepo).getStages[stageID],
		nil)
	if err == nil {
		t.Fatalf("assertCharterInjected admitted a grooming stage with no injected charter")
	}
	if gctx != nil {
		t.Errorf("assertCharterInjected returned a grooming context on refusal: %+v", gctx)
	}
	if !strings.Contains(err.Error(), "none of the") {
		t.Errorf("the pre-Build guard is not L2 (message = %q)", err.Error())
	}
}
