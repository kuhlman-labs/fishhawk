package main

import (
	"flag"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/kuhlman-labs/fishhawk/backend/internal/server"
)

// resolveMaxParallelChildren mirrors runServe's --max-parallel-children
// flag wiring (E24.6 / #1146) so the resolution precedence — explicit flag
// arg > FISHHAWKD_MAX_PARALLEL_CHILDREN env > the built-in 0 default — is
// unit-testable without booting the whole server. It is the same shape as
// the live `fs.Int("max-parallel-children", envOrInt(...), ...)` call.
func resolveMaxParallelChildren(t *testing.T, args []string) int {
	t.Helper()
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	v := fs.Int("max-parallel-children",
		envOrInt("FISHHAWKD_MAX_PARALLEL_CHILDREN", 0),
		"test")
	if err := fs.Parse(args); err != nil {
		t.Fatalf("parse: %v", err)
	}
	return *v
}

// resolveImplementModelConfig mirrors runServe's --implement-model-default and
// --implement-allowed-models flag wiring (#1013) so the env > flag resolution
// and the ParseAllowedModels handoff are unit-testable without booting the
// server. Same shape as the live fs.String(... envOr(...) ...) calls.
func resolveImplementModelConfig(t *testing.T, args []string) (string, server.AllowedModels) {
	t.Helper()
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	deflt := fs.String("implement-model-default",
		envOr("FISHHAWKD_IMPLEMENT_MODEL_DEFAULT", ""), "test")
	allowed := fs.String("implement-allowed-models",
		envOr("FISHHAWKD_IMPLEMENT_ALLOWED_MODELS", ""), "test")
	if err := fs.Parse(args); err != nil {
		t.Fatalf("parse: %v", err)
	}
	return *deflt, server.ParseAllowedModels(*allowed)
}

// TestResolveImplementModelConfig covers the implement-model deployment config
// resolution (#1013): the default model env/flag and the per-adapter
// allowed-model policy parse, plus the empty/fail-open default.
func TestResolveImplementModelConfig(t *testing.T) {
	t.Run("unset yields empty default and fail-open policy", func(t *testing.T) {
		t.Setenv("FISHHAWKD_IMPLEMENT_MODEL_DEFAULT", "")
		t.Setenv("FISHHAWKD_IMPLEMENT_ALLOWED_MODELS", "")
		deflt, policy := resolveImplementModelConfig(t, nil)
		if deflt != "" {
			t.Errorf("default = %q, want empty", deflt)
		}
		if !policy.IsAllowed("claudecode", "anything") {
			t.Error("empty policy must fail open")
		}
	})
	t.Run("env values parse into default and policy", func(t *testing.T) {
		t.Setenv("FISHHAWKD_IMPLEMENT_MODEL_DEFAULT", "claude-sonnet-4-6")
		t.Setenv("FISHHAWKD_IMPLEMENT_ALLOWED_MODELS", "claudecode=claude-opus-4-8;codex=gpt-5.5")
		deflt, policy := resolveImplementModelConfig(t, nil)
		if deflt != "claude-sonnet-4-6" {
			t.Errorf("default = %q, want claude-sonnet-4-6", deflt)
		}
		if !policy.IsAllowed("claudecode", "claude-opus-4-8") {
			t.Error("claudecode opus should be allowed")
		}
		if policy.IsAllowed("claudecode", "gpt-5.5") {
			t.Error("claudecode should reject a codex-only model")
		}
		if !policy.IsAllowed("codex", "gpt-5.5") {
			t.Error("codex gpt-5.5 should be allowed")
		}
	})
	t.Run("flag arg wins over env for the default", func(t *testing.T) {
		t.Setenv("FISHHAWKD_IMPLEMENT_MODEL_DEFAULT", "claude-sonnet-4-6")
		deflt, _ := resolveImplementModelConfig(t, []string{"--implement-model-default", "claude-opus-4-8"})
		if deflt != "claude-opus-4-8" {
			t.Errorf("default = %q, want claude-opus-4-8 (flag wins)", deflt)
		}
	})
}

// TestResolveMaxParallelChildren covers the FISHHAWKD_MAX_PARALLEL_CHILDREN
// resolution branches: the default applies when unset, the env value wins
// over the default, an explicit env 0 is honored as the unlimited semantic
// (not coerced), and a flag arg wins over the env.
func TestResolveMaxParallelChildren(t *testing.T) {
	const key = "FISHHAWKD_MAX_PARALLEL_CHILDREN"

	t.Run("unset resolves to default 0 (unlimited)", func(t *testing.T) {
		t.Setenv(key, "")
		if got := resolveMaxParallelChildren(t, nil); got != 0 {
			t.Errorf("got %d, want 0 (default unlimited)", got)
		}
	})

	t.Run("env value wins over default", func(t *testing.T) {
		t.Setenv(key, "4")
		if got := resolveMaxParallelChildren(t, nil); got != 4 {
			t.Errorf("got %d, want 4 (env over default)", got)
		}
	})

	t.Run("explicit env 0 is honored as unlimited", func(t *testing.T) {
		t.Setenv(key, "0")
		if got := resolveMaxParallelChildren(t, nil); got != 0 {
			t.Errorf("got %d, want 0 (explicit 0 must reach the cap as unlimited, not be coerced)", got)
		}
	})

	t.Run("flag arg wins over env", func(t *testing.T) {
		t.Setenv(key, "4")
		if got := resolveMaxParallelChildren(t, []string{"--max-parallel-children", "7"}); got != 7 {
			t.Errorf("got %d, want 7 (explicit flag over env)", got)
		}
	})
}

// TestEnvOrInt_MaxParallelChildren pins the FISHHAWKD_MAX_PARALLEL_CHILDREN
// env name the flag default resolves through envOrInt, so the env name can't
// silently drift from the flag wiring in runServe. Mirrors the explicit-0
// discipline of the plan-review-max-retries test: an env "0" must reach the
// setter as 0 (unlimited), not be treated as empty.
func TestEnvOrInt_MaxParallelChildren(t *testing.T) {
	const key = "FISHHAWKD_MAX_PARALLEL_CHILDREN"
	t.Run("unset returns default 0", func(t *testing.T) {
		t.Setenv(key, "")
		if got := envOrInt(key, 0); got != 0 {
			t.Errorf("got %d, want 0", got)
		}
	})
	t.Run("explicit 0 resolves to 0", func(t *testing.T) {
		t.Setenv(key, "0")
		if got := envOrInt(key, 0); got != 0 {
			t.Errorf("got %d, want 0 (explicit 0 is the unlimited sentinel, not empty)", got)
		}
	})
	t.Run("positive value resolves verbatim", func(t *testing.T) {
		t.Setenv(key, "5")
		if got := envOrInt(key, 0); got != 5 {
			t.Errorf("got %d, want 5", got)
		}
	})
}

// TestNewStageOrchestrator_WiresDriveEngine pins the construction-site
// wiring (E24.3 / #1143): the orchestrator runServe builds must carry a
// non-nil Drive engine, so the RuleChildrenDispatch run_auto_advanced
// trail for concurrent decomposed-child dispatch can't be silently
// dropped behind the orchestrator-fake behavioral tests.
func TestNewStageOrchestrator_WiresDriveEngine(t *testing.T) {
	o := newStageOrchestrator(server.Config{}, slog.Default())
	if o == nil {
		t.Fatal("newStageOrchestrator returned nil")
	}
	if o.Drive == nil {
		t.Error("orchestrator Drive engine is nil; the RuleChildrenDispatch trail would be dropped")
	}
}

// TestNewChildCompletionSweeper_WiresDispatchBackstop pins the
// construction-site wiring (E24.3 / #1143): the sweeper runServe builds
// must carry a non-nil Dispatch backstop (the childCompletionAdvancer
// adapter), so the fail-closed concurrent-dispatch top-up can't be
// silently omitted. Advance + Integrate are asserted alongside so the
// extraction can't regress the pre-existing wiring either.
func TestNewChildCompletionSweeper_WiresDispatchBackstop(t *testing.T) {
	sw := newChildCompletionSweeper(server.Config{}, slog.Default(), time.Minute)
	if sw == nil {
		t.Fatal("newChildCompletionSweeper returned nil")
	}
	if sw.Dispatch == nil {
		t.Error("sweeper Dispatch backstop is nil; the fail-closed dispatch top-up would be omitted")
	}
	if sw.Advance == nil {
		t.Error("sweeper Advance adapter is nil")
	}
	if sw.Integrate == nil {
		t.Error("sweeper Integrate adapter is nil")
	}
}
