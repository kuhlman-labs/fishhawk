package agent

import (
	"encoding/json"
	"errors"
	"testing"
)

func TestMakePayload_RoundTrips(t *testing.T) {
	p := MakePayload(map[string]any{"k": "v", "n": 7})
	var got map[string]any
	if err := json.Unmarshal(p, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got["k"] != "v" {
		t.Errorf(`got["k"] = %v, want "v"`, got["k"])
	}
	// JSON numbers decode to float64; check via formatted equal.
	if n, ok := got["n"].(float64); !ok || n != 7 {
		t.Errorf(`got["n"] = %v, want 7`, got["n"])
	}
}

func TestMakePayload_PanicsOnUnmarshalable(t *testing.T) {
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("expected panic on unmarshalable input")
		}
	}()
	// Channels can't be marshaled — any panic is fine.
	MakePayload(make(chan int))
}

func TestErrors_AreDistinct(t *testing.T) {
	pairs := []struct {
		a, b error
	}{
		{ErrAgentFailed, ErrBudgetExceeded},
		{ErrAgentFailed, ErrTimeout},
		{ErrAgentFailed, ErrBinaryNotFound},
		{ErrBudgetExceeded, ErrTimeout},
		{ErrBudgetExceeded, ErrBinaryNotFound},
		{ErrTimeout, ErrBinaryNotFound},
		{ErrAgentThinkingBlock, ErrAgentFailed},
		{ErrAgentThinkingBlock, ErrBudgetExceeded},
		{ErrAgentThinkingBlock, ErrTimeout},
		{ErrAgentThinkingBlock, ErrBinaryNotFound},
	}
	for _, p := range pairs {
		if errors.Is(p.a, p.b) {
			t.Errorf("errors.Is(%v, %v) = true, want false", p.a, p.b)
		}
	}
}
