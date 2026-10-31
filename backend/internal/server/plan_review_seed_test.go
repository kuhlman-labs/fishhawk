package server

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	"github.com/kuhlman-labs/fishhawk/backend/internal/planreview"
	"github.com/kuhlman-labs/fishhawk/backend/internal/signing"
)

// Plan-review seed (#4436): the plan review exports its tree at the plan seed
// the runner recorded on plan_generated, never at the shared checkout's HEAD.
//
// Every test here uses a real temp repo whose checked-out HEAD is the OLDER
// commit A while the newer commit B (the seed) sits in the object store — the
// #4436 instance — with grounding enabled and run.WorkingDir pointing at it, so
// a fallback to the checkout HEAD would ALWAYS succeed and be observable.

// gitSeedFixture builds a repo with commit A (a.txt), then commit B adding
// seeded.txt, then detaches HEAD back at A. It returns the repo, A and B.
func gitSeedFixture(t *testing.T) (dir, shaA, shaB string) {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not on PATH")
	}
	dir = t.TempDir()
	env := append(os.Environ(),
		"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null",
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t",
	)
	git := func(args ...string) string {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		cmd.Env = env
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
		}
		return strings.TrimSpace(string(out))
	}
	git("init", "-q")
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("a\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git("add", "a.txt")
	git("commit", "-q", "-m", "A")
	shaA = git("rev-parse", "HEAD")
	if err := os.WriteFile(filepath.Join(dir, "seeded.txt"), []byte("seeded\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git("add", "seeded.txt")
	git("commit", "-q", "-m", "B")
	shaB = git("rev-parse", "HEAD")
	git("checkout", "-q", "--detach", shaA)
	if got := git("rev-parse", "HEAD"); got != shaA {
		t.Fatalf("fixture precondition: checkout HEAD = %s, want A %s", got, shaA)
	}
	return dir, shaA, shaB
}

// seedPlanGeneratedSeed appends a plan_generated row on stageID whose payload
// carries seed_sha (omitted when sha is "").
func seedPlanGeneratedSeed(t *testing.T, au *auditFake, runID, stageID uuid.UUID, sha string) {
	t.Helper()
	p := map[string]any{"run_id": runID.String(), "stage_id": stageID.String()}
	if sha != "" {
		p["seed_sha"] = sha
	}
	b, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	sid := stageID
	if _, err := au.AppendChained(context.Background(), audit.ChainAppendParams{
		RunID: runID, StageID: &sid, Timestamp: time.Now().UTC(), Category: "plan_generated", Payload: b,
	}); err != nil {
		t.Fatal(err)
	}
}

// shipPlanRequestWithSeed is shipPlanRequest with the runner's plan seed
// header attached (omitted when seed is "").
func shipPlanRequestWithSeed(t *testing.T, s *Server, runID, stageID uuid.UUID, priv ed25519.PrivateKey, body []byte, seed string) *httptest.ResponseRecorder {
	t.Helper()
	url := fmt.Sprintf("/v0/runs/%s/plan?stage_id=%s", runID, stageID)
	req := httptest.NewRequest(http.MethodPost, url, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	sig := ed25519.Sign(priv, signing.ComputeMessage(body))
	req.Header.Set("X-Fishhawk-Signature", hex.EncodeToString(sig))
	if seed != "" {
		req.Header.Set(planSeedHeader, seed)
	}
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, req)
	return w
}

// seedRoundServer builds an advisory plan-review server over repo with a
// grounding-capable reviewer and a scratch TMPDIR so exports are observable.
func seedRoundServer(t *testing.T, repo string) (s *Server, sf *signingFake, au *auditFake, reviewer *groundingFakeReviewer, runID, stageID uuid.UUID, scratch string) {
	t.Helper()
	scratch = t.TempDir()
	t.Setenv("TMPDIR", scratch)
	runID, stageID = uuid.New(), uuid.New()
	reviewer = &groundingFakeReviewer{
		verdict: &planreview.ReviewVerdict{Verdict: planreview.VerdictApprove},
		model:   "claude-sonnet-4-6",
	}
	s, sf, _, au, rr := newPlanServerWithReviewer(t, runID, stageID, reviewer, specAdvisoryReviewers)
	rr.getRuns[runID].WorkingDir = repo
	return s, sf, au, reviewer, runID, stageID, scratch
}

// planGeneratedPayloads decodes every committed plan_generated row.
func planGeneratedPayloads(t *testing.T, au *auditFake) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, e := range auditFakeEntries(au, "plan_generated") {
		var m map[string]any
		if err := json.Unmarshal(e.Payload, &m); err != nil {
			t.Fatalf("decode plan_generated: %v", err)
		}
		out = append(out, m)
	}
	return out
}

// planReviewStarted decodes the single committed plan_review_started row.
func planReviewStarted(t *testing.T, au *auditFake) planreview.ReviewStartedPayload {
	t.Helper()
	rows := auditFakeEntries(au, "plan_review_started")
	if len(rows) != 1 {
		t.Fatalf("plan_review_started entries = %d, want 1", len(rows))
	}
	var p planreview.ReviewStartedPayload
	if err := json.Unmarshal(rows[0].Payload, &p); err != nil {
		t.Fatalf("decode plan_review_started: %v", err)
	}
	return p
}

// treeCommitLine is the grounded REPOSITORY ACCESS phrase naming the export.
func treeCommitLine(sha string) string { return "TRACKED files exported at commit " + sha[:12] }

// assertUngroundedNoHead asserts the round ran diff-only with the given
// ungrounded reason, exported no tree, and never named the checkout HEAD A.
func assertUngroundedNoHead(t *testing.T, reviewer *groundingFakeReviewer, scratch, shaA, reasonText string) {
	t.Helper()
	reviewer.mu.Lock()
	grounded, treeDir, got := reviewer.reviewGroundedHit, reviewer.treeDir, reviewer.prompt
	reviewer.mu.Unlock()
	if got == "" {
		t.Fatal("reviewer was never invoked")
	}
	if grounded || treeDir != "" {
		t.Errorf("round must run ungrounded (no seed), got grounded=%v treeDir=%q", grounded, treeDir)
	}
	if !strings.Contains(got, reasonText) {
		t.Errorf("prompt missing the ungrounded reason %q:\n%s", reasonText, got)
	}
	if strings.Contains(got, shaA[:12]) {
		t.Errorf("prompt names the checkout HEAD %s — the round fell back to HEAD:\n%s", shaA[:12], got)
	}
	if left := reviewTreeExports(t, scratch); len(left) != 0 {
		t.Errorf("an unresolved seed must export no tree, found %v", left)
	}
}

// (T1) TestPlanReviewSeed_ExportsSeedNotCheckoutHead is the #4436 done-means
// test, crossing the runner-wire header → handleShipPlan → plan_generated →
// resolvePlanReviewSeed → export → prompt render → plan_review_started layers.
// The checkout HEAD is A; the uploaded seed is B. The round must export B.
//
// Counterfactuals: feeding groundReview "HEAD" exports A (no seeded.txt, A's
// prefix in the prompt) → RED; deleting the seed_sha recording line leaves
// plan_generated without seed_sha, so the round runs ungrounded → RED.
func TestPlanReviewSeed_ExportsSeedNotCheckoutHead(t *testing.T) {
	repo, shaA, shaB := gitSeedFixture(t)
	s, sf, au, reviewer, runID, stageID, _ := seedRoundServer(t, repo)
	priv, _ := sf.issue(t, runID)

	w := shipPlanRequestWithSeed(t, s, runID, stageID, priv, validPlanBytes(t), shaB)
	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201:\n%s", w.Code, w.Body.String())
	}
	s.waitBackgroundReviews()
	reviewer.mu.Lock()
	grounded, treeDir, got, files := reviewer.reviewGroundedHit, reviewer.treeDir, reviewer.prompt, reviewer.treeFiles
	reviewer.mu.Unlock()
	seededPresent := false
	for _, f := range files {
		seededPresent = seededPresent || f == "seeded.txt"
	}

	gen := planGeneratedPayloads(t, au)
	if len(gen) != 1 || gen[0]["seed_sha"] != shaB {
		t.Fatalf("plan_generated = %v, want one row with seed_sha %s", gen, shaB)
	}
	if !grounded || treeDir == "" {
		t.Fatalf("plan review not grounded on the seed: grounded=%v treeDir=%q\n%s", grounded, treeDir, got)
	}
	if !seededPresent {
		t.Errorf("export %v lacks seeded.txt — the round exported the checkout HEAD A, not the seed B", files)
	}
	if !strings.Contains(got, treeCommitLine(shaB)) {
		t.Errorf("REPOSITORY ACCESS must name the seed %s:\n%s", shaB[:12], got)
	}
	if strings.Contains(got, shaA[:12]) {
		t.Errorf("prompt names the checkout HEAD %s:\n%s", shaA[:12], got)
	}
	started := planReviewStarted(t, au)
	if started.SeedSHA != shaB || started.SeedUnresolved != "" {
		t.Errorf("plan_review_started seed = (%q, %q), want (%q, \"\")", started.SeedSHA, started.SeedUnresolved, shaB)
	}
}

// (T2) TestPlanReviewSeed_UnresolvedNotRecorded: no seed recorded — (a) an
// upload with no header (a pre-#4436 runner), (b) no plan_generated row at all.
// The round runs diff-only (no_ref) and records seed_not_recorded; it never
// grounds on the checkout HEAD, which is valid here.
//
// Counterfactuals: a HEAD fallback grounds the round on A → RED; blanking the
// seed_unresolved stamp leaves the committed row without the reason → RED.
func TestPlanReviewSeed_UnresolvedNotRecorded(t *testing.T) {
	noRef := "no resolved commit was available to export"
	t.Run("upload without header", func(t *testing.T) {
		repo, shaA, _ := gitSeedFixture(t)
		s, sf, au, reviewer, runID, stageID, scratch := seedRoundServer(t, repo)
		priv, _ := sf.issue(t, runID)
		if w := shipPlanRequestWithSeed(t, s, runID, stageID, priv, validPlanBytes(t), ""); w.Code != http.StatusCreated {
			t.Fatalf("status = %d, want 201:\n%s", w.Code, w.Body.String())
		}
		s.waitBackgroundReviews()
		gen := planGeneratedPayloads(t, au)
		if len(gen) != 1 {
			t.Fatalf("plan_generated rows = %d, want 1", len(gen))
		}
		if _, ok := gen[0]["seed_sha"]; ok {
			t.Errorf("no header must record no seed_sha: %v", gen[0])
		}
		if _, ok := gen[0]["seed_sha_malformed"]; ok {
			t.Errorf("no header must record no seed_sha_malformed: %v", gen[0])
		}
		assertUngroundedNoHead(t, reviewer, scratch, shaA, noRef)
		if got := planReviewStarted(t, au); got.SeedUnresolved != planSeedUnresolvedNotRecorded || got.SeedSHA != "" {
			t.Errorf("plan_review_started seed = (%q, %q), want (\"\", %q)", got.SeedSHA, got.SeedUnresolved, planSeedUnresolvedNotRecorded)
		}
	})
	t.Run("no plan_generated row", func(t *testing.T) {
		repo, shaA, _ := gitSeedFixture(t)
		s, _, au, reviewer, runID, stageID, scratch := seedRoundServer(t, repo)
		if s.runPlanReviews(t.Context(), runID, stageID, validPlanBytes(t), nil, nil, nil, nil, nil) {
			t.Fatal("advisory runPlanReviews returned true")
		}
		s.waitBackgroundReviews()
		assertUngroundedNoHead(t, reviewer, scratch, shaA, noRef)
		if got := planReviewStarted(t, au); got.SeedUnresolved != planSeedUnresolvedNotRecorded || got.SeedSHA != "" {
			t.Errorf("plan_review_started seed = (%q, %q), want (\"\", %q)", got.SeedSHA, got.SeedUnresolved, planSeedUnresolvedNotRecorded)
		}
	})
}

// (T3) TestPlanReviewSeed_UnresolvedMalformed: a header that is not a hex
// object name is NOT stored (seed_sha_malformed instead), never fails the
// upload, and the round runs diff-only recording seed_malformed.
//
// Counterfactual: weakening validPlanSeedSHA to `s != ""` stores seed_sha
// "HEAD", which the resolver exports — the round grounds on A → RED.
func TestPlanReviewSeed_UnresolvedMalformed(t *testing.T) {
	for _, tc := range []struct{ name, header string }{
		{"symbolic ref", "HEAD"},
		{"39 hex chars", strings.Repeat("a", 39)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo, shaA, _ := gitSeedFixture(t)
			s, sf, au, reviewer, runID, stageID, scratch := seedRoundServer(t, repo)
			priv, _ := sf.issue(t, runID)
			if w := shipPlanRequestWithSeed(t, s, runID, stageID, priv, validPlanBytes(t), tc.header); w.Code != http.StatusCreated {
				t.Fatalf("a malformed seed must never fail the upload: status = %d:\n%s", w.Code, w.Body.String())
			}
			s.waitBackgroundReviews()
			gen := planGeneratedPayloads(t, au)
			if len(gen) != 1 || gen[0]["seed_sha_malformed"] != true {
				t.Fatalf("plan_generated = %v, want seed_sha_malformed true", gen)
			}
			if v, ok := gen[0]["seed_sha"]; ok {
				t.Errorf("a malformed seed must not be stored, got seed_sha %v", v)
			}
			assertUngroundedNoHead(t, reviewer, scratch, shaA, "no resolved commit was available to export")
			if got := planReviewStarted(t, au); got.SeedUnresolved != planSeedUnresolvedMalformed || got.SeedSHA != "" {
				t.Errorf("plan_review_started seed = (%q, %q), want (\"\", %q)", got.SeedSHA, got.SeedUnresolved, planSeedUnresolvedMalformed)
			}
		})
	}
}

// (T4) TestPlanReviewSeed_UnresolvedLookupFailed: the plan_generated read fails
// — never read as "no seed, use HEAD". The round runs diff-only and records
// seed_lookup_failed.
func TestPlanReviewSeed_UnresolvedLookupFailed(t *testing.T) {
	repo, shaA, shaB := gitSeedFixture(t)
	s, _, au, reviewer, runID, stageID, scratch := seedRoundServer(t, repo)
	seedPlanGeneratedSeed(t, au, runID, stageID, shaB)
	au.listByCategoryErrCategory = "plan_generated"
	if s.runPlanReviews(t.Context(), runID, stageID, validPlanBytes(t), nil, nil, nil, nil, nil) {
		t.Fatal("advisory runPlanReviews returned true")
	}
	s.waitBackgroundReviews()
	assertUngroundedNoHead(t, reviewer, scratch, shaA, "no resolved commit was available to export")
	if got := planReviewStarted(t, au); got.SeedUnresolved != planSeedUnresolvedLookupFailed || got.SeedSHA != "" {
		t.Errorf("plan_review_started seed = (%q, %q), want (\"\", %q)", got.SeedSHA, got.SeedUnresolved, planSeedUnresolvedLookupFailed)
	}
}

// (T5) TestPlanReviewSeed_SeedObjectUnavailable: a well-formed seed absent from
// the repo (and no origin to fetch it from) degrades to ref_unavailable — the
// export never substitutes the checkout HEAD — and the row still records the
// seed the round targeted.
func TestPlanReviewSeed_SeedObjectUnavailable(t *testing.T) {
	repo, shaA, _ := gitSeedFixture(t)
	s, _, au, reviewer, runID, stageID, scratch := seedRoundServer(t, repo)
	missing := strings.Repeat("d", 40)
	seedPlanGeneratedSeed(t, au, runID, stageID, missing)
	if s.runPlanReviews(t.Context(), runID, stageID, validPlanBytes(t), nil, nil, nil, nil, nil) {
		t.Fatal("advisory runPlanReviews returned true")
	}
	s.waitBackgroundReviews()
	assertUngroundedNoHead(t, reviewer, scratch, shaA, "the reviewed commit is not present locally and could not be fetched from origin")
	if got := planReviewStarted(t, au); got.SeedSHA != missing || got.SeedUnresolved != "" {
		t.Errorf("plan_review_started seed = (%q, %q), want (%q, \"\")", got.SeedSHA, got.SeedUnresolved, missing)
	}
}

// (T6) TestPlanReviewSeed_NewestStageRowWins pins resolvePlanReviewSeed's
// scoping: the NEWEST plan_generated row on THIS stage wins over an older one
// on the stage and a newer one on another stage of the same run; a row that
// cannot be decoded, or carries an invalid seed_sha on read, is malformed.
func TestPlanReviewSeed_NewestStageRowWins(t *testing.T) {
	runID, stageID, otherStage := uuid.New(), uuid.New(), uuid.New()
	shaX, shaB, shaY := strings.Repeat("1", 40), strings.Repeat("b", 64), strings.Repeat("9", 40)
	s, _, _, au, _ := newPlanServer(t, runID, stageID)
	seedPlanGeneratedSeed(t, au, runID, stageID, shaX)
	seedPlanGeneratedSeed(t, au, runID, stageID, shaB)
	seedPlanGeneratedSeed(t, au, runID, otherStage, shaY)
	if got := s.resolvePlanReviewSeed(t.Context(), runID, stageID); got.SHA != shaB || got.Unresolved != "" {
		t.Errorf("resolvePlanReviewSeed = %+v, want SHA %s (newest row on this stage)", got, shaB)
	}

	t.Run("invalid seed_sha on read", func(t *testing.T) {
		runID, stageID := uuid.New(), uuid.New()
		s, _, _, au, _ := newPlanServer(t, runID, stageID)
		seedPlanGeneratedSeed(t, au, runID, stageID, "--upload-pack=x")
		if got := s.resolvePlanReviewSeed(t.Context(), runID, stageID); got.SHA != "" || got.Unresolved != planSeedUnresolvedMalformed {
			t.Errorf("resolvePlanReviewSeed = %+v, want unresolved %s", got, planSeedUnresolvedMalformed)
		}
	})
	t.Run("undecodable payload", func(t *testing.T) {
		runID, stageID := uuid.New(), uuid.New()
		s, _, _, au, _ := newPlanServer(t, runID, stageID)
		sid := stageID
		if _, err := au.AppendChained(t.Context(), audit.ChainAppendParams{
			RunID: runID, StageID: &sid, Category: "plan_generated", Payload: json.RawMessage(`["not","an","object"]`),
		}); err != nil {
			t.Fatal(err)
		}
		if got := s.resolvePlanReviewSeed(t.Context(), runID, stageID); got.SHA != "" || got.Unresolved != planSeedUnresolvedMalformed {
			t.Errorf("resolvePlanReviewSeed = %+v, want unresolved %s", got, planSeedUnresolvedMalformed)
		}
	})
}

// (T7) TestPlanSeedHeader_MatchesSharedWireFixture pins the header name to the
// fixture runner/internal/upload's PlanSeedHeader is pinned to, so the name the
// runner sends and the name the backend reads cannot drift silently.
func TestPlanSeedHeader_MatchesSharedWireFixture(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join(pkgSrcDir, "..", "..", "..", "testdata", "wire", "plan_seed_header.json"))
	if err != nil {
		t.Fatalf("read shared fixture: %v", err)
	}
	var fx struct {
		Header  string `json:"header"`
		SeedSHA string `json:"seed_sha"`
	}
	if err := json.Unmarshal(raw, &fx); err != nil {
		t.Fatalf("decode shared fixture: %v", err)
	}
	if planSeedHeader != fx.Header {
		t.Errorf("planSeedHeader = %q, fixture header = %q", planSeedHeader, fx.Header)
	}
	if !validPlanSeedSHA(fx.SeedSHA) {
		t.Errorf("fixture seed_sha %q must validate", fx.SeedSHA)
	}
}

// TestValidPlanSeedSHA pins the accepted object-name shapes: full lowercase
// SHA-1 or SHA-256 hex only.
func TestValidPlanSeedSHA(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want bool
	}{
		{strings.Repeat("a", 40), true},
		{strings.Repeat("0", 64), true},
		{"", false},
		{"HEAD", false},
		{strings.Repeat("a", 39), false},
		{strings.Repeat("a", 41), false},
		{strings.Repeat("a", 63), false},
		{strings.Repeat("A", 40), false},
		{"-" + strings.Repeat("a", 39), false},
		{strings.Repeat("a", 40) + "\n", false},
	} {
		if got := validPlanSeedSHA(tc.in); got != tc.want {
			t.Errorf("validPlanSeedSHA(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

// TestEmitReviewStarted_SeedIsPlanOnly pins that the plan seed marker reaches
// plan_review_started only: implement_review_started stays byte-identical even
// when the context carries a seed.
func TestEmitReviewStarted_SeedIsPlanOnly(t *testing.T) {
	runID, stageID := uuid.New(), uuid.New()
	s, _, _, au, _ := newPlanServer(t, runID, stageID)
	ctx := withPlanReviewSeed(t.Context(), planReviewSeed{SHA: strings.Repeat("c", 40)})
	s.emitReviewStarted(ctx, runID, stageID, "implement_review_started", planreview.AuthorityMode("advisory"), 1, nil, "", "", "", nil)
	rows := auditFakeEntries(au, "implement_review_started")
	if len(rows) != 1 {
		t.Fatalf("implement_review_started rows = %d, want 1", len(rows))
	}
	if want := `{"configured_agents":1,"authority":"advisory"}`; string(rows[0].Payload) != want {
		t.Errorf("implement_review_started = %s, want %s", rows[0].Payload, want)
	}
}
