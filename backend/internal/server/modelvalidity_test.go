package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/kuhlman-labs/fishhawk/backend/internal/modeloracle"
	"github.com/kuhlman-labs/fishhawk/backend/internal/run"
)

// gateProbe drives checkModelValidityGate directly with the given oracle,
// resolved model, and adapter, returning whether it passed and the recorder.
func gateProbe(oracle modeloracle.ModelOracle, resolved, adapter string) (bool, *httptest.ResponseRecorder) {
	s := New(Config{Addr: "127.0.0.1:0", ModelOracle: oracle})
	stage := &run.Stage{ID: uuid.New()}
	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/v0/stages/x/approvals", nil)
	ok := s.checkModelValidityGate(w, r, stage, resolved, adapter)
	return ok, w
}

func freshGateOracle() modeloracle.Static {
	return modeloracle.Static{
		Models: map[string][]string{"claudecode": {"claude-opus-4-8", "claude-sonnet-4-6"}},
		Fresh:  true,
	}
}

// reject: a fresh+ok snapshot that omits the resolved model → 422 model_invalid,
// gate returns false.
func TestCheckModelValidityGate_RejectOnFreshAbsence(t *testing.T) {
	ok, w := gateProbe(freshGateOracle(), "claude-typo-9", "claudecode")
	if ok {
		t.Fatal("gate returned true, want false on a fresh-absence reject")
	}
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422:\n%s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), `"model_invalid"`) {
		t.Errorf("body missing model_invalid: %s", w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "claude-opus-4-8") {
		t.Errorf("body does not name the available set: %s", w.Body.String())
	}
}

// accept: a model present in a fresh+ok snapshot passes with no response write.
func TestCheckModelValidityGate_AcceptOnFreshPresent(t *testing.T) {
	ok, w := gateProbe(freshGateOracle(), "claude-opus-4-8", "claudecode")
	if !ok {
		t.Fatalf("gate returned false, want true:\n%s", w.Body.String())
	}
	if w.Code != http.StatusOK { // recorder default; nothing written
		t.Errorf("status = %d, want untouched (200 default)", w.Code)
	}
}

// fail-open-stale: a stale snapshot (Fresh=false) cannot reject — gate passes.
func TestCheckModelValidityGate_FailOpenStale(t *testing.T) {
	o := freshGateOracle()
	o.Fresh = false
	ok, _ := gateProbe(o, "claude-typo-9", "claudecode")
	if !ok {
		t.Fatal("gate returned false, want true on a stale snapshot (fail open)")
	}
}

// fail-open-no-snapshot: nil oracle and the wired NoData oracle both pass.
func TestCheckModelValidityGate_FailOpenNoSnapshot(t *testing.T) {
	if ok, _ := gateProbe(nil, "claude-anything", "claudecode"); !ok {
		t.Error("nil oracle: gate returned false, want true (fail open)")
	}
	if ok, _ := gateProbe(modeloracle.NewNoData(), "claude-anything", "claudecode"); !ok {
		t.Error("NoData oracle: gate returned false, want true (fail open)")
	}
	// An unconfigured adapter on an otherwise-fresh oracle is ok=false → pass.
	if ok, _ := gateProbe(freshGateOracle(), "gpt-5.5", "codex"); !ok {
		t.Error("unconfigured adapter: gate returned false, want true (fail open)")
	}
}

// empty resolved model (today's default spawn) is never validated → pass.
func TestCheckModelValidityGate_EmptyModelPasses(t *testing.T) {
	if ok, _ := gateProbe(freshGateOracle(), "", "claudecode"); !ok {
		t.Error("empty model: gate returned false, want true")
	}
}
