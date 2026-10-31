package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"reflect"
	"testing"

	"github.com/kuhlman-labs/fishhawk/backend/internal/basedelta"
)

// E93.1 / #4657 CROSS-BOUNDARY END-TO-END for the DECLARED verdict-transfer
// surface (ADR-124): admission spec bytes → server resolver → real git inputs
// → the REAL basedelta.Classifier with GoListLoader → audit chain → rebase
// handler → merge gate, over pgtest and real repositories built by
// newMCTEWorldWith. Every admission spec is validated with spec.ParseBytes
// before use, so a fixture typo cannot pass as an undeclared surface.

// mctsPythonDeclaredSpecYAML declares a surface for a NON-Go repository:
// requirements.txt is verdict-wide, everything else inert, and a
// completeness check is named so enforce acts. The declaration is complete;
// only the workspace adapter can refuse it.
const mctsPythonDeclaredSpecYAML = mctV2SpecBase + `verdict_transfer:
  global_surface: ["requirements.txt"]
  inert_paths: ["**"]
  completeness_check: "python fixture check"
`

// mctsGoModOnlySpecYAML declares G = go.mod and nothing inert.
const mctsGoModOnlySpecYAML = mctV2SpecBase + `verdict_transfer:
  global_surface: ["go.mod"]
  completeness_check: "e2e fixture check"
`

// mctsToolsInertSpecYAML declares tools/** inert beside a G whose proto/**
// pattern matches no file (prospective coverage).
const mctsToolsInertSpecYAML = mctV2SpecBase + `verdict_transfer:
  global_surface: ["go.mod", "proto/**"]
  inert_paths: ["tools/**"]
  completeness_check: "e2e fixture check"
`

// mctsEverythingInertSpecYAML lists neither the workflow spec nor
// .fishhawk/** and declares every path inert.
const mctsEverythingInertSpecYAML = mctV2SpecBase + `verdict_transfer:
  global_surface: ["go.mod"]
  inert_paths: ["**"]
  completeness_check: "e2e fixture check"
`

// mctsCheckoutSpecYAML is the spec the CHECKOUT tracks: it would make b/**
// verdict-wide, so a b/b.go advance that transfers proves it was never read.
const mctsCheckoutSpecYAML = mctV2SpecBase + `verdict_transfer:
  global_surface: ["go.mod", "b/**"]
  completeness_check: "checkout fixture check"
`

// mctsGoBase is the Go a/b base tree with a package-less tools/gen.py.
func mctsGoBase() map[string]string {
	return map[string]string{
		"go.mod":       "module example.com/m\n\ngo 1.21\n",
		"a/a.go":       "package a\n\nconst A = 1\n",
		"b/b.go":       "package b\n\nconst B = 1\n",
		"tools/gen.py": "print('gen')\n",
	}
}

// mctsPythonBase is a non-Fishhawk Python repository: no go.mod, no go.work.
func mctsPythonBase() map[string]string {
	return map[string]string{
		"app/main.py":      "import lib.util\n",
		"lib/util.py":      "X = 1\n",
		"requirements.txt": "requests==2.0\n",
	}
}

// newMCTSPythonWorld is a Python repository whose PR edits app/main.py and
// whose advance edits lib/util.py, plus extra base files.
func newMCTSPythonWorld(t *testing.T, mode MergeCandidateTransferMode, extra, advance map[string]string) *mcteWorld {
	t.Helper()
	base := mctsPythonBase()
	for p, c := range extra {
		base[p] = c
	}
	return newMCTEWorldWith(t, mcteOpts{
		Mode: mode, Spec: mctsPythonDeclaredSpecYAML, Base: base,
		PR:      map[string]string{"app/main.py": "import lib.util\nprint(lib.util.X)\n"},
		Advance: advance,
	})
}

// requireRefusedWithPass asserts the enforce-mode refusal shape: one refused
// row for H' with code and detail, no transferred row, ONE pass trigger for
// H', and a merge gate holding on the unverified candidate.
func (w *mcteWorld) requireRefusedWithPass(t *testing.T, code, detail string) mergeCandidateTransferPayload {
	t.Helper()
	f := w.f
	rows := f.transferPayloads(w.r, CategoryMergeCandidateVerdictTransferRefused)
	if len(rows) != 1 {
		t.Fatalf("refused rows = %+v (transferred %+v), want one %s",
			rows, f.transferPayloads(w.r, CategoryMergeCandidateVerdictTransferred), code)
	}
	got := rows[0]
	if got.ReasonCode != code || got.ReasonDetail != detail || !got.ReasonUntrusted || got.ToHead != w.merged {
		t.Fatalf("refused = %+v, want reason_code %q detail %q untrusted for H' %s", got, code, detail, w.merged)
	}
	if got.ClassifierVersion != basedelta.ClassifierVersion {
		t.Errorf("classifier_version = %d, want %d", got.ClassifierVersion, basedelta.ClassifierVersion)
	}
	if n := len(f.rows(w.r, CategoryMergeCandidateVerdictTransferred)); n != 0 {
		t.Fatalf("transferred rows = %d, want 0", n)
	}
	if tr := f.triggers(w.r); len(tr) != 1 || tr[0].ExpectedHeadSHA != w.merged {
		t.Fatalf("triggers = %+v, want one pass for H' %s", tr, w.merged)
	}
	if g := f.srv.mergeCandidateGate(context.Background(), w.runRow(t)); g.Code != mergeCodeCandidateUnverified {
		t.Errorf("merge gate code = %q, want merge_candidate_unverified", g.Code)
	}
	return got
}

func TestMergeCandidateTransfer_E2E_NonGoRepositoryRefusedUnsupportedLanguage(t *testing.T) {
	w := newMCTSPythonWorld(t, MergeCandidateTransferEnforce, nil, map[string]string{"lib/util.py": "X = 2\n"})

	w.f.rebase(w.r)
	got := w.requireRefusedWithPass(t, "undecidable:unsupported_language", "")
	if got.SurfaceProvenance.State != transferSurfaceStateDeclared {
		t.Errorf("provenance = %+v, want declared (the refusal is the workspace adapter's)", got.SurfaceProvenance)
	}
}

func TestMergeCandidateTransfer_E2E_NonGoRepositoryShadowNeverWouldTransfer(t *testing.T) {
	w := newMCTSPythonWorld(t, MergeCandidateTransferShadow, nil, map[string]string{"lib/util.py": "X = 2\n"})
	f := w.f

	f.rebase(w.r)
	if tr := f.triggers(w.r); len(tr) != 1 || tr[0].ExpectedHeadSHA != w.merged {
		t.Fatalf("triggers = %+v, want one pass for H' %s", tr, w.merged)
	}
	f.srv.waitMergeCandidateTransferShadows()
	sh := f.transferPayloads(w.r, CategoryMergeCandidateVerdictTransferShadow)
	if len(sh) != 1 || sh[0].WouldTransfer == nil || *sh[0].WouldTransfer ||
		sh[0].ReasonCode != "undecidable:unsupported_language" || sh[0].ToHead != w.merged ||
		sh[0].Mode != string(MergeCandidateTransferShadow) {
		t.Fatalf("shadow rows = %+v, want one would_transfer=false unsupported_language for %s", sh, w.merged)
	}
	if n := len(f.rows(w.r, CategoryMergeCandidateVerdictTransferred)); n != 0 {
		t.Fatalf("transferred rows = %d in shadow, want 0", n)
	}
}

// A Go module NESTED below the root is not a Go workspace (ADR-124 D3): even
// an advance confined to that module's own .go file refuses.
func TestMergeCandidateTransfer_E2E_NestedGoModuleIsNotAWorkspace(t *testing.T) {
	w := newMCTSPythonWorld(t, MergeCandidateTransferEnforce,
		map[string]string{
			"tools/go.mod": "module example.com/tools\n\ngo 1.21\n",
			"tools/x.go":   "package tools\n\nconst X = 1\n",
		},
		map[string]string{"tools/x.go": "package tools\n\nconst X = 2\n"})

	w.f.rebase(w.r)
	w.requireRefusedWithPass(t, "undecidable:unsupported_language", "")
}

func TestMergeCandidateTransfer_E2E_UndeclaredSurfaceRefused(t *testing.T) {
	w := newMCTEWorldWith(t, mcteOpts{
		Mode: MergeCandidateTransferEnforce, Spec: mcvUndelegatedSpecYAML,
		Base: map[string]string{
			"go.mod": "module example.com/m\n\ngo 1.21\n",
			"a/a.go": "package a\n\nconst A = 1\n",
			"b/b.go": "package b\n\nconst B = 1\n",
		},
		PR:      map[string]string{"a/a.go": "package a\n\nconst A = 2\n"},
		Advance: map[string]string{"b/b.go": "package b\n\nconst B = 2\n"},
	})

	w.f.rebase(w.r)
	got := w.requireRefusedWithPass(t, "undecidable:surface_undeclared", "")
	if got.SurfaceProvenance.State != transferSurfaceStateUndeclared || got.SurfaceDigest != "" {
		t.Errorf("provenance = %+v digest %q, want undeclared with no digest", got.SurfaceProvenance, got.SurfaceDigest)
	}
}

func TestMergeCandidateTransfer_E2E_UnclassifiedPathRefused(t *testing.T) {
	w := newMCTEWorldWith(t, mcteOpts{
		Mode: MergeCandidateTransferEnforce, Spec: mctsGoModOnlySpecYAML, Base: mctsGoBase(),
		PR:      map[string]string{"a/a.go": "package a\n\nconst A = 2\n"},
		Advance: map[string]string{"tools/gen.py": "print('gen2')\n"},
	})

	w.f.rebase(w.r)
	w.requireRefusedWithPass(t, "undecidable:unclassified_path", "tools/gen.py")
}

// The same advance with tools/** declared inert transfers, and the declared
// G pattern that matches no file is recorded as a diagnostic, never refused.
func TestMergeCandidateTransfer_E2E_DeclaredInertTransfers(t *testing.T) {
	w := newMCTEWorldWith(t, mcteOpts{
		Mode: MergeCandidateTransferEnforce, Spec: mctsToolsInertSpecYAML, Base: mctsGoBase(),
		PR:      map[string]string{"a/a.go": "package a\n\nconst A = 2\n"},
		Advance: map[string]string{"tools/gen.py": "print('gen2')\n"},
	})
	f := w.f

	f.rebase(w.r)
	rows := f.transferPayloads(w.r, CategoryMergeCandidateVerdictTransferred)
	if len(rows) != 1 {
		t.Fatalf("transferred rows = %+v (refused %+v), want one",
			rows, f.transferPayloads(w.r, CategoryMergeCandidateVerdictTransferRefused))
	}
	got := rows[0]
	if got.ReasonCode != transferReasonIndependent || got.ToHead != w.merged {
		t.Fatalf("transferred = %+v, want independent for H' %s", got, w.merged)
	}
	if !reflect.DeepEqual(got.SurfaceUnmatched, []string{"proto/**"}) || got.SurfaceUnmatchedCount != 1 {
		t.Errorf("surface_unmatched = %q (count %d), want [proto/**] (1)", got.SurfaceUnmatched, got.SurfaceUnmatchedCount)
	}
	want, err := basedelta.ResolveSurface([]byte(mctsToolsInertSpecYAML))
	if err != nil || want == nil {
		t.Fatalf("resolve fixture surface: %v (%v)", want, err)
	}
	if got.SurfaceDigest == "" || got.SurfaceDigest != want.Digest() {
		t.Errorf("surface_digest = %q, want %q", got.SurfaceDigest, want.Digest())
	}
	sum := sha256.Sum256([]byte(mctsToolsInertSpecYAML))
	if p := got.SurfaceProvenance; p.Source != transferSurfaceSourceAdmission || p.State != transferSurfaceStateDeclared ||
		p.SpecSHA256 != hex.EncodeToString(sum[:]) {
		t.Errorf("provenance = %+v, want admission_spec / declared / spec_sha256 of the admission bytes", p)
	}
	if tr := f.triggers(w.r); len(tr) != 0 {
		t.Fatalf("triggers = %+v, want none for H'", tr)
	}
	if g := f.srv.mergeCandidateGate(context.Background(), w.runRow(t)); !g.admits() {
		t.Errorf("merge gate = %+v, want admit on the transferred verdict", g)
	}
}

// The checkout TRACKS a workflow spec making b/** verdict-wide; the admission
// spec does not. The b/b.go advance transfers: only the admission spec is
// read (ADR-124 D1).
func TestMergeCandidateTransfer_E2E_CheckoutDeclarationNeverRead(t *testing.T) {
	mctValidSpecs(t, mctsCheckoutSpecYAML)
	w := newMCTEWorldWith(t, mcteOpts{
		Mode: MergeCandidateTransferEnforce, Spec: mcteDeclaredSpecYAML,
		Base: map[string]string{
			"go.mod":                   "module example.com/m\n\ngo 1.21\n",
			"a/a.go":                   "package a\n\nconst A = 1\n",
			"b/b.go":                   "package b\n\nconst B = 1\n",
			basedelta.WorkflowSpecPath: mctsCheckoutSpecYAML,
		},
		PR:      map[string]string{"a/a.go": "package a\n\nconst A = 2\n"},
		Advance: map[string]string{"b/b.go": "package b\n\nconst B = 2\n"},
	})
	f := w.f

	f.rebase(w.r)
	rows := f.transferPayloads(w.r, CategoryMergeCandidateVerdictTransferred)
	if len(rows) != 1 || rows[0].ReasonCode != transferReasonIndependent {
		t.Fatalf("transferred rows = %+v (refused %+v), want one independent: the checkout's b/** declaration must not be read",
			rows, f.transferPayloads(w.r, CategoryMergeCandidateVerdictTransferRefused))
	}
	sum := sha256.Sum256([]byte(mcteDeclaredSpecYAML))
	if rows[0].SurfaceProvenance.SpecSHA256 != hex.EncodeToString(sum[:]) {
		t.Errorf("provenance = %+v, want the admission spec's sha256", rows[0].SurfaceProvenance)
	}
	if tr := f.triggers(w.r); len(tr) != 0 {
		t.Fatalf("triggers = %+v, want none for H'", tr)
	}
}

// A change to the workflow spec itself forces a pass even when the
// declaration lists neither it nor .fishhawk/** and declares everything inert
// (ADR-124 D5 self-coverage).
func TestMergeCandidateTransfer_E2E_WorkflowSpecChangeForcesPass(t *testing.T) {
	base := map[string]string{
		"go.mod":                   "module example.com/m\n\ngo 1.21\n",
		"a/a.go":                   "package a\n\nconst A = 1\n",
		"b/b.go":                   "package b\n\nconst B = 1\n",
		basedelta.WorkflowSpecPath: mcteDeclaredSpecYAML,
	}
	w := newMCTEWorldWith(t, mcteOpts{
		Mode: MergeCandidateTransferEnforce, Spec: mctsEverythingInertSpecYAML, Base: base,
		PR:      map[string]string{"a/a.go": "package a\n\nconst A = 2\n"},
		Advance: map[string]string{basedelta.WorkflowSpecPath: mctsEverythingInertSpecYAML},
	})

	w.f.rebase(w.r)
	w.requireRefusedWithPass(t, transferReasonGlobalSurface, basedelta.WorkflowSpecPath)
}
