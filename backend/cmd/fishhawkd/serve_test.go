package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"flag"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kuhlman-labs/fishhawk/backend/internal/account"
	accountdb "github.com/kuhlman-labs/fishhawk/backend/internal/account/db"
	"github.com/kuhlman-labs/fishhawk/backend/internal/anthropic"
	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	authpkg "github.com/kuhlman-labs/fishhawk/backend/internal/auth"
	"github.com/kuhlman-labs/fishhawk/backend/internal/campaign"
	"github.com/kuhlman-labs/fishhawk/backend/internal/claudecode"
	"github.com/kuhlman-labs/fishhawk/backend/internal/forge"
	forgegitlab "github.com/kuhlman-labs/fishhawk/backend/internal/forge/gitlab"
	"github.com/kuhlman-labs/fishhawk/backend/internal/githubapp"
	"github.com/kuhlman-labs/fishhawk/backend/internal/githubclient"
	"github.com/kuhlman-labs/fishhawk/backend/internal/identity"
	"github.com/kuhlman-labs/fishhawk/backend/internal/issuecomment"
	"github.com/kuhlman-labs/fishhawk/backend/internal/modeloracle"
	"github.com/kuhlman-labs/fishhawk/backend/internal/operatorrole"
	"github.com/kuhlman-labs/fishhawk/backend/internal/pgtest"
	"github.com/kuhlman-labs/fishhawk/backend/internal/repoacl"
	"github.com/kuhlman-labs/fishhawk/backend/internal/reviewresolver"
	runpkg "github.com/kuhlman-labs/fishhawk/backend/internal/run"
	"github.com/kuhlman-labs/fishhawk/backend/internal/server"
	"github.com/kuhlman-labs/fishhawk/backend/internal/webhook"
	"github.com/kuhlman-labs/fishhawk/backend/internal/workmgmt"
	workmgmtgitlab "github.com/kuhlman-labs/fishhawk/backend/internal/workmgmt/gitlab"
	"github.com/kuhlman-labs/fishhawk/directory/pkg/handoff"
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

// resolveBudgetTierConfig mirrors runServe's --budget-limit-override-usd /
// --budget-ack-multiple / --budget-page-multiple flag wiring (#1371) and
// the handoff into server.Config (serve.go line ~553), so the env > flag
// resolution AND the Config wiring are unit-testable without booting the
// server. It returns a server.Config carrying only the three #1371 fields,
// built exactly as runServe builds them.
func resolveBudgetTierConfig(t *testing.T, args []string) server.Config {
	t.Helper()
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	limitOverride := fs.Float64("budget-limit-override-usd",
		envOrFloat("FISHHAWKD_BUDGET_LIMIT_OVERRIDE_USD", 0), "test")
	ackMultiple := fs.Float64("budget-ack-multiple",
		envOrFloat("FISHHAWKD_BUDGET_ACK_MULTIPLE", 2.0), "test")
	pageMultiple := fs.Float64("budget-page-multiple",
		envOrFloat("FISHHAWKD_BUDGET_PAGE_MULTIPLE", 3.0), "test")
	if err := fs.Parse(args); err != nil {
		t.Fatalf("parse: %v", err)
	}
	return server.Config{
		BudgetLimitOverrideUSD: *limitOverride,
		BudgetAckMultiple:      *ackMultiple,
		BudgetPageMultiple:     *pageMultiple,
	}
}

// TestResolveBudgetTierConfig is binding condition (1): the three #1371
// budget env vars parse and wire into server.Config — both the defaults
// (0 / 2.0 / 3.0) and an explicit override.
func TestResolveBudgetTierConfig(t *testing.T) {
	t.Run("defaults when unset", func(t *testing.T) {
		t.Setenv("FISHHAWKD_BUDGET_LIMIT_OVERRIDE_USD", "")
		t.Setenv("FISHHAWKD_BUDGET_ACK_MULTIPLE", "")
		t.Setenv("FISHHAWKD_BUDGET_PAGE_MULTIPLE", "")
		cfg := resolveBudgetTierConfig(t, nil)
		if cfg.BudgetLimitOverrideUSD != 0 {
			t.Errorf("BudgetLimitOverrideUSD = %g, want 0 (spec limit)", cfg.BudgetLimitOverrideUSD)
		}
		if cfg.BudgetAckMultiple != 2.0 {
			t.Errorf("BudgetAckMultiple = %g, want 2.0", cfg.BudgetAckMultiple)
		}
		if cfg.BudgetPageMultiple != 3.0 {
			t.Errorf("BudgetPageMultiple = %g, want 3.0", cfg.BudgetPageMultiple)
		}
	})
	t.Run("explicit overrides via flags wire into Config", func(t *testing.T) {
		cfg := resolveBudgetTierConfig(t, []string{
			"--budget-limit-override-usd", "250",
			"--budget-ack-multiple", "1.5",
			"--budget-page-multiple", "2.5",
		})
		if cfg.BudgetLimitOverrideUSD != 250 {
			t.Errorf("BudgetLimitOverrideUSD = %g, want 250", cfg.BudgetLimitOverrideUSD)
		}
		if cfg.BudgetAckMultiple != 1.5 {
			t.Errorf("BudgetAckMultiple = %g, want 1.5", cfg.BudgetAckMultiple)
		}
		if cfg.BudgetPageMultiple != 2.5 {
			t.Errorf("BudgetPageMultiple = %g, want 2.5", cfg.BudgetPageMultiple)
		}
	})
	t.Run("env override wins over default", func(t *testing.T) {
		t.Setenv("FISHHAWKD_BUDGET_LIMIT_OVERRIDE_USD", "500")
		t.Setenv("FISHHAWKD_BUDGET_ACK_MULTIPLE", "2.25")
		t.Setenv("FISHHAWKD_BUDGET_PAGE_MULTIPLE", "4")
		cfg := resolveBudgetTierConfig(t, nil)
		if cfg.BudgetLimitOverrideUSD != 500 || cfg.BudgetAckMultiple != 2.25 || cfg.BudgetPageMultiple != 4 {
			t.Errorf("env values not wired: %+v", cfg)
		}
	})
}

// resolveReviewResolution mirrors runServe's --review-resolution flag wiring
// (ADR-031 Phase 2) so the env > flag resolution and the reviewresolver.Select
// handoff are unit-testable without booting the server. Same shape as the live
// fs.String("review-resolution", envOr("FISHHAWKD_REVIEW_RESOLUTION",
// reviewresolver.DefaultResolution), ...) call followed by reviewresolver.Select.
func resolveReviewResolution(t *testing.T, args []string) (reviewresolver.Resolver, error) {
	t.Helper()
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	v := fs.String("review-resolution",
		envOr("FISHHAWKD_REVIEW_RESOLUTION", reviewresolver.DefaultResolution), "test")
	if err := fs.Parse(args); err != nil {
		t.Fatalf("parse: %v", err)
	}
	return reviewresolver.Select(*v)
}

// noopReviewResolver is a named reviewresolver.Resolver for the serve-wiring
// test: it records nothing and resolves to nil, standing in for a registered
// provider so Select can return it.
type noopReviewResolver struct{ name string }

func (n noopReviewResolver) Name() string { return n.name }

func (n noopReviewResolver) ResolveReviewFromPollState(context.Context, uuid.UUID, bool, string) error {
	return nil
}

// TestResolveReviewResolution covers the --review-resolution /
// FISHHAWKD_REVIEW_RESOLUTION wiring (ADR-031 Phase 2 binding condition): the
// flag default resolves to github_merge, an explicit value is parsed and
// selected, and an UNKNOWN value fails closed (reviewresolver.Select returns
// UnknownResolverError — runServe fails startup, not silently defaulting).
func TestResolveReviewResolution(t *testing.T) {
	// Register the github_merge provider (as runServe does after srv is built)
	// plus a second named provider so the explicit-value branch resolves to a
	// real registration. The registry is global per-process; registering here
	// mirrors the startup wiring without booting a server.
	reviewresolver.Register(noopReviewResolver{name: reviewresolver.DefaultResolution})
	reviewresolver.Register(noopReviewResolver{name: "alt_merge"})

	t.Run("flag default resolves to github_merge", func(t *testing.T) {
		t.Setenv("FISHHAWKD_REVIEW_RESOLUTION", "")
		got, err := resolveReviewResolution(t, nil)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got.Name() != reviewresolver.DefaultResolution {
			t.Errorf("resolved provider = %q, want github_merge", got.Name())
		}
	})

	t.Run("explicit value is parsed and selected", func(t *testing.T) {
		t.Setenv("FISHHAWKD_REVIEW_RESOLUTION", "")
		got, err := resolveReviewResolution(t, []string{"--review-resolution", "alt_merge"})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got.Name() != "alt_merge" {
			t.Errorf("resolved provider = %q, want alt_merge (explicit flag value selected)", got.Name())
		}
	})

	t.Run("env value is parsed and selected", func(t *testing.T) {
		t.Setenv("FISHHAWKD_REVIEW_RESOLUTION", "alt_merge")
		got, err := resolveReviewResolution(t, nil)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got.Name() != "alt_merge" {
			t.Errorf("resolved provider = %q, want alt_merge (env value selected)", got.Name())
		}
	})

	t.Run("unknown value fails closed", func(t *testing.T) {
		t.Setenv("FISHHAWKD_REVIEW_RESOLUTION", "")
		_, err := resolveReviewResolution(t, []string{"--review-resolution", "nonexistent"})
		var unknown *reviewresolver.UnknownResolverError
		if !errors.As(err, &unknown) {
			t.Fatalf("error = %v, want *UnknownResolverError (runServe would fail startup, not default)", err)
		}
		if unknown.ID != "nonexistent" {
			t.Errorf("UnknownResolverError.ID = %q, want nonexistent", unknown.ID)
		}
	})
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

// TestPlanReviewerSet_CodexReasoningEffort covers the #1493 per-reviewer
// reasoning-effort ladder applied deployment-side: the spec value wins over the
// FISHHAWKD_CODEX_REASONING_EFFORT default; an empty spec value falls back to
// the env default; both empty carries no override; and For("codex", ...) routes
// the resolved value into a constructed codex reviewer.
func TestPlanReviewerSet_CodexReasoningEffort(t *testing.T) {
	t.Run("spec value overrides the deployment default", func(t *testing.T) {
		set := &planReviewerSet{opts: planReviewerOptions{codexEffort: "low"}}
		if got := set.resolveCodexEffort("high"); got != "high" {
			t.Errorf("resolveCodexEffort = %q, want high (spec overrides env default)", got)
		}
	})
	t.Run("empty spec value falls back to the deployment default", func(t *testing.T) {
		set := &planReviewerSet{opts: planReviewerOptions{codexEffort: "medium"}}
		if got := set.resolveCodexEffort(""); got != "medium" {
			t.Errorf("resolveCodexEffort = %q, want medium (env default is the lowest rung)", got)
		}
	})
	t.Run("both empty carries no override", func(t *testing.T) {
		set := &planReviewerSet{opts: planReviewerOptions{codexEffort: ""}}
		if got := set.resolveCodexEffort(""); got != "" {
			t.Errorf("resolveCodexEffort = %q, want empty (host config inherited)", got)
		}
	})
	t.Run("For codex constructs a reviewer with the resolved effort", func(t *testing.T) {
		set := &planReviewerSet{opts: planReviewerOptions{
			enableCodexReviewer: true,
			codexModel:          "gpt-5.5",
			codexEffort:         "low",
		}}
		reviewer, err := set.For("codex", "", "high")
		if err != nil {
			t.Fatalf("For(codex): %v", err)
		}
		if reviewer == nil {
			t.Fatal("For(codex) returned a nil reviewer")
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

// TestNewStageOrchestrator_ThreadsExternalURL pins the cross-layer wiring
// (#1774, binding condition 2): newStageOrchestrator must thread
// cfg.ExternalURL into the Orchestrator's ExternalURL field so the consolidated
// PR body's audit-log footer renders the operator-facing base URL rather than a
// relative path. A regression here silently degrades every decomposed-parent PR
// footer to a relative URL.
func TestNewStageOrchestrator_ThreadsExternalURL(t *testing.T) {
	const want = "https://app.fishhawk.test"
	o := newStageOrchestrator(server.Config{ExternalURL: want}, slog.Default())
	if o == nil {
		t.Fatal("newStageOrchestrator returned nil")
	}
	if o.ExternalURL != want {
		t.Errorf("orchestrator ExternalURL = %q, want %q (cfg.ExternalURL not threaded)", o.ExternalURL, want)
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

// TestCampaignDriverStartDecision is the binding-condition serve-level test
// (E25.5 / #1444): the fail-closed switch does NOT construct/start the ticker
// when the flag is false, and skips with a reason when a required dependency
// is unwired. Each branch of campaignDriverStartDecision is asserted (mode a:
// DISABLED in serve.go).
func TestCampaignDriverStartDecision(t *testing.T) {
	wired := server.Config{
		CampaignRepo: campaign.BaseFake{},
		RunRepo:      runpkg.BaseFake{},
		AuditRepo:    audit.BaseFake{},
		GitHub:       &githubclient.Client{},
	}

	// (a) DISABLED: the flag is false → the ticker is NOT started and there is
	// no warn reason (a flag-off skip is silent, not a misconfiguration).
	if start, reason := campaignDriverStartDecision(false, wired); start || reason != "" {
		t.Fatalf("flag-off: start=%v reason=%q; want false + empty (ticker must NOT be constructed/started)", start, reason)
	}

	// Enabled + fully wired → started, no reason.
	if start, reason := campaignDriverStartDecision(true, wired); !start || reason != "" {
		t.Fatalf("wired: start=%v reason=%q; want true + empty", start, reason)
	}

	// Enabled but a required repo missing → fail-closed skip with a reason.
	missingRepo := wired
	missingRepo.CampaignRepo = nil
	if start, reason := campaignDriverStartDecision(true, missingRepo); start || reason == "" {
		t.Fatalf("missing campaign repo: start=%v reason=%q; want false + a reason", start, reason)
	}

	// Enabled but the GitHub client is missing → fail-closed skip (the
	// run-starter needs it to resolve the workflow spec the campaign lacks).
	missingGitHub := wired
	missingGitHub.GitHub = nil
	if start, reason := campaignDriverStartDecision(true, missingGitHub); start || reason == "" {
		t.Fatalf("missing github: start=%v reason=%q; want false + a reason", start, reason)
	}
}

// TestNewCampaignDriver_WiresDependencies asserts the constructor binds every
// required dependency (a nil Starter/Audit would make Run() refuse to start).
func TestNewCampaignDriver_WiresDependencies(t *testing.T) {
	cfg := server.Config{
		Addr:         "127.0.0.1:0",
		CampaignRepo: campaign.BaseFake{},
		RunRepo:      runpkg.BaseFake{},
		AuditRepo:    audit.BaseFake{},
		GitHub:       &githubclient.Client{},
	}
	srv := server.New(cfg)
	notifier := issuecomment.New(issuecomment.Deps{
		GitHub:      cfg.GitHub,
		Runs:        cfg.RunRepo,
		Audit:       cfg.AuditRepo,
		ExternalURL: "https://app.fishhawk.test",
	})
	tk := newCampaignDriver(cfg, srv, slog.Default(), notifier, time.Minute, "feature_change", "")
	if tk == nil {
		t.Fatal("newCampaignDriver returned nil")
	}
	if tk.Campaigns == nil || tk.Runs == nil || tk.Starter == nil || tk.Audit == nil {
		t.Fatalf("ticker has a nil required dependency: %+v", tk)
	}
	if tk.Interval != time.Minute {
		t.Errorf("interval = %v, want 1m", tk.Interval)
	}
	// E25.6: with the GitHub client wired the constructor binds a live
	// GateActor so the driver auto-acts on each run gate.
	if tk.GateActor == nil {
		t.Error("ticker GateActor is nil despite a configured GitHub client; auto-drive would never run")
	}
	// E25.7: a concrete notifier is bound as the page seam so the Paged branch
	// fires the human page.
	if tk.Notifier == nil {
		t.Error("ticker Notifier is nil despite a configured notifier; the Paged branch would never page")
	}
}

// TestNewCampaignDriver_NilNotifier_ObserveOnly guards the typed-nil trap: a nil
// *issuecomment.Notifier (the unconfigured-deps case) must leave the seam a true
// nil interface so the driver's Paged branch takes the observe-only path rather
// than calling a nil pointer.
func TestNewCampaignDriver_NilNotifier_ObserveOnly(t *testing.T) {
	cfg := server.Config{
		Addr:         "127.0.0.1:0",
		CampaignRepo: campaign.BaseFake{},
		RunRepo:      runpkg.BaseFake{},
		AuditRepo:    audit.BaseFake{},
		GitHub:       &githubclient.Client{},
	}
	srv := server.New(cfg)
	tk := newCampaignDriver(cfg, srv, slog.Default(), nil, time.Minute, "feature_change", "")
	if tk.Notifier != nil {
		t.Errorf("ticker Notifier = %#v, want a true nil interface (observe-only)", tk.Notifier)
	}
}

// TestCampaignOperatorIdentity asserts the in-process actor identity carries
// the operator-agent attribution, the gate-action write scopes, and a non-empty
// TokenID (scope-acceptance parity — the handler scope check must apply rather
// than the cookie-session bypass). E25.6 / ADR-047 slice 3.
func TestCampaignOperatorIdentity(t *testing.T) {
	id := campaignOperatorIdentity()
	if id.Subject != operatorrole.CampaignActorSubject {
		t.Errorf("Subject = %q, want %q", id.Subject, operatorrole.CampaignActorSubject)
	}
	if id.TokenID == "" {
		t.Error("TokenID is empty; the handler scope check would be bypassed (cookie-session path) instead of enforced")
	}
	want := operatorrole.CampaignActorScopes()
	if len(id.Scopes) != len(want) {
		t.Fatalf("Scopes = %v, want %v", id.Scopes, want)
	}
	have := map[string]bool{}
	for _, s := range id.Scopes {
		have[s] = true
	}
	for _, s := range want {
		if !have[s] {
			t.Errorf("Scopes missing %q (have %v)", s, id.Scopes)
		}
	}
}

// TestNewCampaignGateActor covers the serve-wiring construction AND the
// fail-closed observe-only path (E25.6 / ADR-047 slice 3): a configured GitHub
// client yields a live actor binding the campaign identity + GitHub merger; an
// unconfigured client returns nil so the driver runs observe-only.
func TestNewCampaignGateActor(t *testing.T) {
	srv := server.New(server.Config{Addr: "127.0.0.1:0"})

	// Configured: GitHub client present → a non-nil actor binding the campaign
	// identity and a GitHubMerger.
	actor := newCampaignGateActor(server.Config{GitHub: &githubclient.Client{}}, srv, slog.Default())
	if actor == nil {
		t.Fatal("newCampaignGateActor returned nil with a configured GitHub client; auto-drive would never run")
	}
	cga, ok := actor.(campaignGateActor)
	if !ok {
		t.Fatalf("actor concrete = %T, want campaignGateActor", actor)
	}
	if cga.id.Subject != operatorrole.CampaignActorSubject {
		t.Errorf("actor identity subject = %q, want %q", cga.id.Subject, operatorrole.CampaignActorSubject)
	}
	if cga.merger == nil {
		t.Error("actor GitHubMerger is nil; a delegated may_merge could not be honoured")
	}

	// Fail-closed: no GitHub client → nil actor (the driver runs observe-only).
	if got := newCampaignGateActor(server.Config{GitHub: nil}, srv, slog.Default()); got != nil {
		t.Errorf("newCampaignGateActor(nil GitHub) = %T, want nil (observe-only fail-closed)", got)
	}
}

// TestGithubAutoMerger_FailsClosed asserts the merger refuses — before any
// HTTP call — a run that lacks the installation id or PR url the merge needs,
// or whose PR url is unparseable. These are the defensive guards the merge seam
// adds; each returns an error the actor surfaces rather than a silent no-op.
func TestGithubAutoMerger_FailsClosed(t *testing.T) {
	m := githubAutoMerger{gh: &githubclient.Client{}}
	ctx := context.Background()
	inst := int64(42)
	prURL := "https://github.com/x/y/pull/7"
	bad := "not-a-url"

	cases := []struct {
		name string
		run  *runpkg.Run
	}{
		{"no installation id", &runpkg.Run{ID: uuid.New(), PullRequestURL: &prURL}},
		{"no pull request url", &runpkg.Run{ID: uuid.New(), InstallationID: &inst}},
		{"unparseable pull request url", &runpkg.Run{ID: uuid.New(), InstallationID: &inst, PullRequestURL: &bad}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := m.MergePullRequest(ctx, tc.run); err == nil {
				t.Fatalf("MergePullRequest(%s) = nil, want an error (fail-closed before HTTP)", tc.name)
			}
		})
	}
}

// mergeFallbackGitHub is a minimal GitHub stand-in for the githubAutoMerger
// clean-status REST fallback test (E48.7 / #1954). It answers the three calls
// the merge seam makes — GET pull (node id), POST /graphql (auto-merge enable),
// PUT pull merge (REST squash) — and records whether the REST merge fired so a
// test can assert the fallback is taken exactly on the clean-status class.
type mergeFallbackGitHub struct {
	graphqlBody string // the enable-auto-merge graphql response body
	mergeStatus int    // status for the REST merge PUT (0 → 200)
	mergeHits   int    // number of REST merge PUTs served
}

func newMergeFallbackClient(t *testing.T, fg *mergeFallbackGitHub) *githubclient.Client {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /repos/{owner}/{repo}/pulls/{number}",
		func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"number":7,"node_id":"PR_node","state":"open","head":{"sha":"abc"}}`)
		})
	mux.HandleFunc("POST /graphql",
		func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, fg.graphqlBody)
		})
	mux.HandleFunc("PUT /repos/{owner}/{repo}/pulls/{number}/merge",
		func(w http.ResponseWriter, _ *http.Request) {
			fg.mergeHits++
			st := fg.mergeStatus
			if st == 0 {
				st = http.StatusOK
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(st)
			_, _ = io.WriteString(w, `{"merged":true,"sha":"deadbeef"}`)
		})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return &githubclient.Client{
		BaseURL: srv.URL,
		Tokens:  fakeTokenProvider{},
		HTTP:    &http.Client{Timeout: 5 * time.Second},
	}
}

func mergeFallbackRun() *runpkg.Run {
	inst := int64(42)
	pr := "https://github.com/x/y/pull/7"
	return &runpkg.Run{ID: uuid.New(), InstallationID: &inst, PullRequestURL: &pr}
}

// TestGithubAutoMerger_CleanStatus_RESTFallback is the binding-condition-3
// guard: the DELEGATED may_merge seam (githubAutoMerger, the GateMerger the
// auto-driver dispatches through) receives the clean-status REST fallback — an
// already-merge-ready PR whose enablePullRequestAutoMerge is rejected is merged
// synchronously via the REST squash merge instead.
func TestGithubAutoMerger_CleanStatus_RESTFallback(t *testing.T) {
	fg := &mergeFallbackGitHub{
		graphqlBody: `{"errors":[{"message":"Pull request is in clean status","type":"UNPROCESSABLE"}]}`,
	}
	m := githubAutoMerger{gh: newMergeFallbackClient(t, fg)}
	if err := m.MergePullRequest(context.Background(), mergeFallbackRun()); err != nil {
		t.Fatalf("MergePullRequest: %v", err)
	}
	if fg.mergeHits != 1 {
		t.Errorf("REST merge fired %d times, want 1 (clean-status must fall back)", fg.mergeHits)
	}
}

// TestGithubAutoMerger_EnableSuccess_NoFallback: a successful auto-merge enable
// takes no REST fallback (the PR is queued, not merged synchronously).
func TestGithubAutoMerger_EnableSuccess_NoFallback(t *testing.T) {
	fg := &mergeFallbackGitHub{
		graphqlBody: `{"data":{"enablePullRequestAutoMerge":{"pullRequest":{"number":7,"state":"OPEN"}}}}`,
	}
	m := githubAutoMerger{gh: newMergeFallbackClient(t, fg)}
	if err := m.MergePullRequest(context.Background(), mergeFallbackRun()); err != nil {
		t.Fatalf("MergePullRequest: %v", err)
	}
	if fg.mergeHits != 0 {
		t.Errorf("REST merge fired %d times on a successful enable, want 0", fg.mergeHits)
	}
}

// TestGithubAutoMerger_UnrelatedEnableError_NoFallback: an enable error that is
// NOT the clean-status class surfaces unchanged and takes no fallback.
func TestGithubAutoMerger_UnrelatedEnableError_NoFallback(t *testing.T) {
	fg := &mergeFallbackGitHub{
		graphqlBody: `{"errors":[{"message":"Auto-merge is not allowed for this repository","type":"UNPROCESSABLE"}]}`,
	}
	m := githubAutoMerger{gh: newMergeFallbackClient(t, fg)}
	if err := m.MergePullRequest(context.Background(), mergeFallbackRun()); err == nil {
		t.Fatal("MergePullRequest = nil, want the unrelated enable error surfaced")
	}
	if fg.mergeHits != 0 {
		t.Errorf("REST merge fired %d times on an unrelated enable error, want 0", fg.mergeHits)
	}
}

// TestParseCampaignPRURL covers the PR-url parser's accept + reject branches.
func TestParseCampaignPRURL(t *testing.T) {
	repo, n, err := parseCampaignPRURL("https://github.com/owner/name/pull/123")
	if err != nil {
		t.Fatalf("valid url: unexpected error %v", err)
	}
	if repo.Owner != "owner" || repo.Name != "name" || n != 123 {
		t.Errorf("parsed = %+v #%d, want owner/name #123", repo, n)
	}
	for _, bad := range []string{"", "https://github.com/owner/name", "https://github.com/owner/name/issues/1", "https://github.com/owner/name/pull/abc"} {
		if _, _, err := parseCampaignPRURL(bad); err == nil {
			t.Errorf("parseCampaignPRURL(%q) = nil error, want a reject", bad)
		}
	}
}

// TestParseInstallationHostAllowlist pins the Mode-2 allowlist config parser
// (E44.15 / #2093): comma-split, trim, lower-case, drop empties; an empty /
// all-whitespace value yields nil (the default scheme/parse-only posture).
func TestParseInstallationHostAllowlist(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want []string
	}{
		{"empty", "", nil},
		{"whitespace only", "   ", nil},
		{"commas only", " , ,, ", nil},
		{"single entry", "acme.ghe.com", []string{"acme.ghe.com"}},
		{"comma list with whitespace", " acme.ghe.com , .ghe.com ", []string{"acme.ghe.com", ".ghe.com"}},
		{"case normalization", "ACME.GHE.COM", []string{"acme.ghe.com"}},
		{"trailing + duplicate empties dropped", "acme.ghe.com,,", []string{"acme.ghe.com"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := parseInstallationHostAllowlist(tc.raw)
			if len(got) != len(tc.want) {
				t.Fatalf("parseInstallationHostAllowlist(%q) = %v, want %v", tc.raw, got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Errorf("entry %d = %q, want %q", i, got[i], tc.want[i])
				}
			}
		})
	}
}

// writeTempAppKey writes a PKCS#1 RSA private key to a temp PEM file and
// returns the path, for driving runServe's GitHub-App block.
func writeTempAppKey(t *testing.T) string {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	pemBytes := pem.EncodeToMemory(&pem.Block{
		Type:  "RSA PRIVATE KEY",
		Bytes: x509.MarshalPKCS1PrivateKey(key),
	})
	path := filepath.Join(t.TempDir(), "app-key.pem")
	if err := os.WriteFile(path, pemBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestServe_InstallationHostAllowlistWiring pins the E44.15 / #2093 boot wiring
// by driving runServe ITSELF: a configured
// FISHHAWKD_GITHUB_INSTALLATION_HOST_ALLOWLIST is parsed and threaded onto the
// App client (proven by the presence log line), and startup proceeds past the
// GitHub-App block (aborting later at the deliberately-invalid
// --review-resolution). An UNSET allowlist must NOT emit the presence log —
// pinning the default-off posture end to end through runServe.
func TestServe_InstallationHostAllowlistWiring(t *testing.T) {
	keyFile := writeTempAppKey(t)

	t.Run("configured allowlist is threaded and logged", func(t *testing.T) {
		code, log := serveWithProfile(t,
			"-github-app-id", "12345",
			"-github-app-private-key-file", keyFile,
			"-github-installation-host-allowlist", "acme.ghe.com, .ghe.com",
			bootstrapAbortFlag)
		if code != exitFailure {
			t.Fatalf("runServe exit = %d, want %d (startup aborts at the invalid --review-resolution, AFTER the App block); log:\n%s", code, exitFailure, log)
		}
		if !strings.Contains(log, "installation host allowlist configured") {
			t.Errorf("configured allowlist did not emit the presence log line:\n%s", log)
		}
	})

	t.Run("unset allowlist stays silent (default-off)", func(t *testing.T) {
		code, log := serveWithProfile(t,
			"-github-app-id", "12345",
			"-github-app-private-key-file", keyFile,
			bootstrapAbortFlag)
		if code != exitFailure {
			t.Fatalf("runServe exit = %d, want %d; log:\n%s", code, exitFailure, log)
		}
		if strings.Contains(log, "installation host allowlist configured") {
			t.Errorf("an unset allowlist must not emit the presence log line (default-off posture):\n%s", log)
		}
	})
}

// TestServe_GitLabInstallationHostAllowlistWiring pins the per-forge GitLab
// Mode-2 allowlist (E44.16 / #2094): --gitlab-installation-host-allowlist is a
// SEPARATE flag from GitHub's (a workspace's github.com and gitlab.com hosts
// differ) and emits its own fail-closed presence log when set. bootstrapAbortFlag
// aborts startup AFTER the forge-registration block, so the log line is
// observable. The log fires independent of GitHub App / GitLab base+token config
// (it is gated only on the allowlist flag).
func TestServe_GitLabInstallationHostAllowlistWiring(t *testing.T) {
	t.Run("configured gitlab allowlist is threaded and logged", func(t *testing.T) {
		code, log := serveWithProfile(t,
			"-gitlab-installation-host-allowlist", "gitlab.acme.example, .gitlab.example.com",
			bootstrapAbortFlag)
		if code != exitFailure {
			t.Fatalf("runServe exit = %d, want %d (startup aborts at the invalid --review-resolution, AFTER the forge-registration block); log:\n%s", code, exitFailure, log)
		}
		if !strings.Contains(log, "gitlab installation host allowlist configured") {
			t.Errorf("configured gitlab allowlist did not emit the presence log line:\n%s", log)
		}
	})

	t.Run("unset gitlab allowlist stays silent (default-off)", func(t *testing.T) {
		code, log := serveWithProfile(t, bootstrapAbortFlag)
		if code != exitFailure {
			t.Fatalf("runServe exit = %d, want %d; log:\n%s", code, exitFailure, log)
		}
		if strings.Contains(log, "gitlab installation host allowlist configured") {
			t.Errorf("an unset gitlab allowlist must not emit the presence log line (default-off posture):\n%s", log)
		}
	})
}

// TestInstallationBaseURLResolver pins the shared per-installation base-URL
// closure factory (E44.16 / #2094) that the App mint, the githubclient REST
// client, and the gitlab forge all consume. A nil resolver yields a nil hook
// (deployment default); a configured resolver returns the provider's
// forge_base_url and propagates a real DB fault UNCHANGED so the consumer fails
// closed; a NULL column resolves to "" (deployment default). The provider
// discriminator is routed verbatim so github and gitlab installs resolve
// independently.
func TestInstallationBaseURLResolver(t *testing.T) {
	t.Run("nil resolver yields a nil hook (deployment default)", func(t *testing.T) {
		if installationBaseURLResolver(nil, "github") != nil {
			t.Fatal("a nil resolver must yield a nil hook so the consumer's ResolveBaseURL stays nil")
		}
	})

	t.Run("github resolves forge_base_url and routes the provider", func(t *testing.T) {
		base := "https://acme.ghe.com"
		fg := &fakeInstGetter{inst: accountdb.Installation{ForgeBaseUrl: &base}}
		hook := installationBaseURLResolver(account.NewEndpointResolver(fg), "github")
		got, err := hook(context.Background(), "1001")
		if err != nil {
			t.Fatalf("hook returned error: %v", err)
		}
		if got != base {
			t.Errorf("resolved base = %q, want %q", got, base)
		}
		if fg.gotProvider != "github" || fg.gotRef != "1001" {
			t.Errorf("resolver called with (%q, %q), want (github, 1001)", fg.gotProvider, fg.gotRef)
		}
	})

	t.Run("gitlab routes the gitlab provider discriminator", func(t *testing.T) {
		base := "https://gitlab.acme.example"
		fg := &fakeInstGetter{inst: accountdb.Installation{ForgeBaseUrl: &base}}
		hook := installationBaseURLResolver(account.NewEndpointResolver(fg), "gitlab")
		got, err := hook(context.Background(), "gitlab:42")
		if err != nil {
			t.Fatalf("hook returned error: %v", err)
		}
		if got != base {
			t.Errorf("resolved base = %q, want %q", got, base)
		}
		if fg.gotProvider != "gitlab" {
			t.Errorf("resolver called with provider %q, want gitlab", fg.gotProvider)
		}
	})

	t.Run("DB fault propagates (fail-closed)", func(t *testing.T) {
		sentinel := errors.New("connection refused")
		fg := &fakeInstGetter{err: sentinel}
		hook := installationBaseURLResolver(account.NewEndpointResolver(fg), "github")
		if _, err := hook(context.Background(), "1001"); !errors.Is(err, sentinel) {
			t.Fatalf("hook error = %v, want the sentinel propagated so the consumer fails closed", err)
		}
	})

	t.Run("NULL column resolves to empty (deployment default)", func(t *testing.T) {
		fg := &fakeInstGetter{} // ForgeBaseUrl nil, no error
		hook := installationBaseURLResolver(account.NewEndpointResolver(fg), "github")
		got, err := hook(context.Background(), "1001")
		if err != nil {
			t.Fatalf("hook returned error: %v", err)
		}
		if got != "" {
			t.Errorf("resolved base = %q, want empty (deployment default) for a NULL column", got)
		}
	})
}

// fakeInstGetter is an account.InstallationGetter that records the lookup key
// and returns a programmed Installation / error, exercising the resolver closure
// without a live database.
type fakeInstGetter struct {
	inst        accountdb.Installation
	err         error
	gotProvider string
	gotRef      string
}

func (f *fakeInstGetter) GetInstallationByRef(_ context.Context, arg accountdb.GetInstallationByRefParams) (accountdb.Installation, error) {
	f.gotProvider = arg.Provider
	f.gotRef = arg.InstallationRef
	return f.inst, f.err
}

// TestBuildModelProviders covers the live ModelOracle provider-registration
// wiring (#1341, binding condition 1 — the previously-untested serve.go
// activation seam). A provider is registered ONLY when its API key is present;
// an absent key leaves it UNREGISTERED so its Snapshot reports ok=false (the
// fail-open / never-a-boot-blocker invariant). Keyed under the existing
// "claudecode"/"codex" strings the allow-list and #1339 already use.
func TestBuildModelProviders(t *testing.T) {
	t.Run("both keys present registers both providers", func(t *testing.T) {
		p := buildModelProviders("anthropic-key", "openai-key")
		if _, ok := p["claudecode"]; !ok {
			t.Error("claudecode not registered with an anthropic key present")
		}
		if _, ok := p["codex"]; !ok {
			t.Error("codex not registered with an openai key present")
		}
		if len(p) != 2 {
			t.Errorf("len(providers) = %d, want 2", len(p))
		}
	})

	t.Run("anthropic key only registers claudecode and leaves codex unregistered", func(t *testing.T) {
		p := buildModelProviders("anthropic-key", "")
		if _, ok := p["claudecode"]; !ok {
			t.Error("claudecode not registered with an anthropic key present")
		}
		if _, ok := p["codex"]; ok {
			t.Error("codex registered despite an absent openai key")
		}
	})

	t.Run("openai key only registers codex and leaves claudecode unregistered", func(t *testing.T) {
		p := buildModelProviders("", "openai-key")
		if _, ok := p["codex"]; !ok {
			t.Error("codex not registered with an openai key present")
		}
		if _, ok := p["claudecode"]; ok {
			t.Error("claudecode registered despite an absent anthropic key")
		}
	})

	t.Run("no keys registers nothing and Snapshot fails open", func(t *testing.T) {
		p := buildModelProviders("", "")
		if len(p) != 0 {
			t.Fatalf("len(providers) = %d, want 0 with no keys", len(p))
		}
		// An unregistered provider must report ok=false so #1339 fails open
		// (never a boot blocker, never a false rejection).
		o := modeloracle.NewCached(p, 24*time.Hour, slog.Default())
		if _, _, ok := o.Snapshot(context.Background(), "claudecode"); ok {
			t.Error("Snapshot ok=true for an unregistered provider, want false (fail-open)")
		}
	})
}

// resolveModelsFlags mirrors runServe's --models-refresh-interval /
// --models-staleness-threshold flag wiring (#1341) so the duration defaults
// (12h refresh / 24h staleness) are unit-testable without booting the server —
// binding condition 1's "assert the two duration-flag defaults parse" clause.
func resolveModelsFlags(t *testing.T, args []string) (refresh, staleness time.Duration) {
	t.Helper()
	fs := flag.NewFlagSet("test", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	r := fs.Duration("models-refresh-interval",
		envOrDuration("FISHHAWKD_MODELS_REFRESH_INTERVAL", 12*time.Hour), "test")
	s := fs.Duration("models-staleness-threshold",
		envOrDuration("FISHHAWKD_MODELS_STALENESS_THRESHOLD", 24*time.Hour), "test")
	if err := fs.Parse(args); err != nil {
		t.Fatalf("parse: %v", err)
	}
	return *r, *s
}

// TestResolveModelsFlags asserts the model-oracle duration defaults parse to 12h
// / 24h, an env override is honored, and an explicit flag wins — the done-means
// defaults from #1341's plan (binding condition 1).
func TestResolveModelsFlags(t *testing.T) {
	t.Run("defaults parse to 12h / 24h", func(t *testing.T) {
		t.Setenv("FISHHAWKD_MODELS_REFRESH_INTERVAL", "")
		t.Setenv("FISHHAWKD_MODELS_STALENESS_THRESHOLD", "")
		refresh, staleness := resolveModelsFlags(t, nil)
		if refresh != 12*time.Hour {
			t.Errorf("refresh default = %s, want 12h", refresh)
		}
		if staleness != 24*time.Hour {
			t.Errorf("staleness default = %s, want 24h", staleness)
		}
	})

	t.Run("env override honored", func(t *testing.T) {
		t.Setenv("FISHHAWKD_MODELS_REFRESH_INTERVAL", "6h")
		t.Setenv("FISHHAWKD_MODELS_STALENESS_THRESHOLD", "48h")
		refresh, staleness := resolveModelsFlags(t, nil)
		if refresh != 6*time.Hour {
			t.Errorf("refresh = %s, want 6h from env", refresh)
		}
		if staleness != 48*time.Hour {
			t.Errorf("staleness = %s, want 48h from env", staleness)
		}
	})

	t.Run("explicit flag wins over env", func(t *testing.T) {
		t.Setenv("FISHHAWKD_MODELS_REFRESH_INTERVAL", "6h")
		refresh, _ := resolveModelsFlags(t, []string{"--models-refresh-interval", "1h"})
		if refresh != time.Hour {
			t.Errorf("refresh = %s, want 1h from the explicit flag", refresh)
		}
	})
}

// TestResolveRefinementDrafter asserts the agent-backed drafter is wired only
// when the local claude adapter is configured — the seam serve.go reads to
// populate Config.RefinementDrafter.
func TestResolveRefinementDrafter(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	t.Run("wired when local claude enabled", func(t *testing.T) {
		d := resolveRefinementDrafter(planReviewerOptions{
			enableLocalClaudeReviewer: true,
			localClaudeBinary:         "claude",
			localClaudeModel:          "claude-opus-4-8",
		}, logger)
		if d == nil {
			t.Fatal("resolveRefinementDrafter returned nil with the local claude adapter enabled")
		}
	})

	t.Run("nil when unconfigured", func(t *testing.T) {
		if d := resolveRefinementDrafter(planReviewerOptions{}, logger); d != nil {
			t.Fatalf("resolveRefinementDrafter = %v, want nil (literal) when unconfigured", d)
		}
	})
}

// TestServeWiresRefinementConfig locks the operator binding condition: the
// production serve path populates BOTH Config.RefinementRepo (always-on
// Postgres adapter, the DB-block call) and Config.RefinementDrafter (agent-
// backed, when the local claude adapter is configured) non-nil. The route-level
// unconfigured-503 tests in the server package cover the nil branches; this
// asserts the wired branch the serve path takes.
func TestServeWiresRefinementConfig(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	var cfg server.Config
	// Drive the SAME production helpers the serve DB block calls, rather than
	// re-constructing the repo inline — a hand-rolled refinement.NewPostgresRepository
	// here would stay green even if the DB block stopped populating the field. A
	// nil pool is fine: the constructor stores it without dialing, and we only
	// assert the field is populated (non-nil), exactly as production wires.
	cfg.RefinementRepo = resolveRefinementRepo(nil)
	cfg.RefinementDrafter = resolveRefinementDrafter(planReviewerOptions{
		enableLocalClaudeReviewer: true,
		localClaudeBinary:         "claude",
		localClaudeModel:          "claude-opus-4-8",
	}, logger)

	if cfg.RefinementRepo == nil {
		t.Error("serve path left Config.RefinementRepo nil")
	}
	if cfg.RefinementDrafter == nil {
		t.Error("serve path left Config.RefinementDrafter nil")
	}
}

// TestResolveIdentityProvider exercises the OAuth-config-gated wiring
// seam the serve OAuth block calls (E39.1 / #1706, binding condition):
// an OAuth client_id present → the constructed Config carries a GitHub
// identity provider; absent → the field is left nil so server.New falls
// back to its NoOp default. Driving the SAME resolveIdentityProvider
// helper the serve block calls (not a hand-rolled construction) keeps
// the assertion honest if the gate ever changes.
func TestResolveIdentityProvider(t *testing.T) {
	// Present: a GitHub provider is constructed.
	present := resolveIdentityProvider("client-id", nil, "", "")
	if present == nil {
		t.Fatal("resolveIdentityProvider with a client_id returned nil; want a GitHub provider")
	}
	if _, ok := present.(*identity.GitHubIdentityProvider); !ok {
		t.Errorf("provider = %T, want *identity.GitHubIdentityProvider", present)
	}

	// Passthrough: the accessor handed to resolveIdentityProvider must reach
	// the constructed provider's unexported token field (E39.10 / #1753) —
	// otherwise the REST reads stay anonymous even when serve wires an
	// accessor. Reflection reads the field's nil-ness without invoking it.
	accessor := func(context.Context) (string, error) { return "tok", nil }
	withTok := resolveIdentityProvider("client-id", accessor, "", "")
	if got := identityProviderTokenIsNil(t, withTok); got {
		t.Error("resolveIdentityProvider dropped the token accessor; provider.token is nil, want non-nil")
	}
	if got := identityProviderTokenIsNil(t, present); !got {
		t.Error("resolveIdentityProvider(nil) left provider.token non-nil, want nil")
	}

	// Endpoint override (E44.2 / #1826): a configured API/OAuth base threads
	// onto the provider's unexported apiBaseURL/oauthBaseURL fields. Empty
	// leaves the GitHub defaults; a configured value overrides them.
	if got := identityProviderBaseURLs(t, present); got.api != identity.DefaultAPIBaseURL || got.oauth != identity.DefaultOAuthBaseURL {
		t.Errorf("default provider base URLs = %+v, want GitHub defaults", got)
	}
	configured := resolveIdentityProvider("client-id", nil, "https://ghes.example.com/api/v3", "https://ghes.example.com")
	if got := identityProviderBaseURLs(t, configured); got.api != "https://ghes.example.com/api/v3" || got.oauth != "https://ghes.example.com" {
		t.Errorf("configured provider base URLs = %+v, want the GHES hosts", got)
	}

	// Absent: nil so server.New defaults to NoOp. Feeding it through
	// server.New proves the end-to-end fallback the seam relies on.
	absent := resolveIdentityProvider("", nil, "", "")
	if absent != nil {
		t.Fatalf("resolveIdentityProvider with no client_id = %#v, want nil", absent)
	}
	srv := server.New(server.Config{IdentityProvider: absent})
	if srv == nil {
		t.Fatal("server.New returned nil")
	}
}

// TestResolveGitHubEndpoints pins the Mode-1 (per-deployment) endpoint
// resolution helper (E44.2 / #1826): unset env → empty override fields so
// every GitHub client keeps its github.com / api.github.com default; a
// configured deployment → each raw env string lands on the matching per-client
// override surface, and the identity device-flow host is derived from the
// scheme+host of the OAuth authorize URL.
func TestResolveGitHubEndpoints(t *testing.T) {
	// Unset: every override empty → clients keep their defaults.
	unset := resolveGitHubEndpoints("", "", "", "", "", "")
	if unset.APIBaseURL != "" || unset.UploadBaseURL != "" || unset.IdentityAPIURL != "" || unset.IdentityOAuthURL != "" {
		t.Errorf("unset endpoints carried non-empty overrides: %+v", unset)
	}
	if unset.OAuth != (authpkg.OAuthURLs{}) {
		t.Errorf("unset OAuth URLs = %+v, want zero (NewGitHubOAuth fills defaults)", unset.OAuth)
	}

	// Configured GHES/EMU deployment: raw strings map onto per-client fields.
	got := resolveGitHubEndpoints(
		"https://ghes.example.com/api/v3",
		"https://ghes.example.com/api/uploads",
		"https://ghes.example.com/login/oauth/authorize",
		"https://ghes.example.com/login/oauth/access_token",
		"https://ghes.example.com/api/v3/user",
		"https://ghes.example.com/api/v3/user/orgs",
	)
	if got.APIBaseURL != "https://ghes.example.com/api/v3" {
		t.Errorf("APIBaseURL = %q", got.APIBaseURL)
	}
	if got.UploadBaseURL != "https://ghes.example.com/api/uploads" {
		t.Errorf("UploadBaseURL = %q", got.UploadBaseURL)
	}
	if got.IdentityAPIURL != "https://ghes.example.com/api/v3" {
		t.Errorf("IdentityAPIURL = %q", got.IdentityAPIURL)
	}
	wantOAuth := authpkg.OAuthURLs{
		AuthorizeURL: "https://ghes.example.com/login/oauth/authorize",
		TokenURL:     "https://ghes.example.com/login/oauth/access_token",
		UserURL:      "https://ghes.example.com/api/v3/user",
		OrgsURL:      "https://ghes.example.com/api/v3/user/orgs",
	}
	if got.OAuth != wantOAuth {
		t.Errorf("OAuth = %+v, want %+v", got.OAuth, wantOAuth)
	}
	// Identity device-flow host derived from the authorize URL's scheme+host.
	if got.IdentityOAuthURL != "https://ghes.example.com" {
		t.Errorf("IdentityOAuthURL = %q, want https://ghes.example.com", got.IdentityOAuthURL)
	}

	// An unparseable authorize URL leaves IdentityOAuthURL empty (default host)
	// rather than propagating a malformed value.
	bad := resolveGitHubEndpoints("", "", "://no-scheme", "", "", "")
	if bad.IdentityOAuthURL != "" {
		t.Errorf("unparseable authorize URL yielded IdentityOAuthURL = %q, want empty", bad.IdentityOAuthURL)
	}
}

// The E44.8 membership-lister registration matrix: github registers
// with OAuth configured; gitlab registers on the BASE URL ALONE (the
// login gate reads groups with the signing-in user's OAuth token, so it
// needs no FISHHAWKD_GITLAB_TOKEN) and stays unregistered otherwise —
// an unregistered provider denies.
func TestResolveMembershipListers(t *testing.T) {
	gh := authpkg.NewGitHubOAuth("id", "secret", "https://example.com/cb", authpkg.OAuthURLs{})

	t.Run("github only when gitlab is unconfigured", func(t *testing.T) {
		got := resolveMembershipListers(gh, "")
		if _, ok := got["github"]; !ok {
			t.Error("github lister not registered")
		}
		if _, ok := got["gitlab"]; ok {
			t.Error("gitlab lister registered with no base URL; an unconfigured forge must deny")
		}
	})
	t.Run("gitlab registers on the base URL alone", func(t *testing.T) {
		got := resolveMembershipListers(gh, "https://gitlab.example.com")
		gl, ok := got["gitlab"]
		if !ok {
			t.Fatal("gitlab lister not registered with a base URL set")
		}
		if gl == nil {
			t.Error("gitlab lister registered as a nil interface value")
		}
		if want := []string{"github", "gitlab"}; !reflect.DeepEqual(sortedKeys(got), want) {
			t.Errorf("providers = %v, want %v", sortedKeys(got), want)
		}
	})
	t.Run("no oauth leaves github unregistered", func(t *testing.T) {
		got := resolveMembershipListers(nil, "")
		if len(got) != 0 {
			t.Errorf("listers = %v, want empty", sortedKeys(got))
		}
	})
}

// TestResolveGitLabOAuth pins the production-reachability config boundary for
// the GitLab browser sign-in flow (E44.22 / #2109, binding condition 1): the
// all-three-or-error credential validation AND the constructed-client path,
// exercised through the same pure resolver serve() calls — no server boot.
func TestResolveGitLabOAuth(t *testing.T) {
	const base = "https://gitlab.example.com"

	t.Run("none set → nil, no error (feature off)", func(t *testing.T) {
		got, err := resolveGitLabOAuth(base, "", "", "")
		if err != nil {
			t.Fatalf("err = %v, want nil (unconfigured is not an error)", err)
		}
		if got != nil {
			t.Errorf("client = %+v, want nil when no GitLab OAuth creds are set", got)
		}
	})

	// Each partial trio (exactly one or two of the three set) must error,
	// covering the all-three-or-error guard on every missing-field branch.
	for _, tc := range []struct {
		name                             string
		clientID, clientSecret, callback string
	}{
		{"only client_id", "cid", "", ""},
		{"only client_secret", "", "csec", ""},
		{"only callback", "", "", "https://ex/cb"},
		{"missing callback", "cid", "csec", ""},
		{"missing client_secret", "cid", "", "https://ex/cb"},
		{"missing client_id", "", "csec", "https://ex/cb"},
	} {
		t.Run("partial config errors: "+tc.name, func(t *testing.T) {
			got, err := resolveGitLabOAuth(base, tc.clientID, tc.clientSecret, tc.callback)
			if err == nil {
				t.Fatalf("err = nil, want all-three-or-error rejection for %s", tc.name)
			}
			if got != nil {
				t.Errorf("client = %+v, want nil on misconfiguration", got)
			}
		})
	}

	t.Run("all three set but no base URL → error", func(t *testing.T) {
		got, err := resolveGitLabOAuth("", "cid", "csec", "https://ex/cb")
		if err == nil {
			t.Fatal("err = nil, want error when the GitLab OAuth creds are set but FISHHAWKD_GITLAB_BASE_URL is empty (no host to reach)")
		}
		if got != nil {
			t.Errorf("client = %+v, want nil", got)
		}
	})

	t.Run("all three + base URL → constructed client on the base host", func(t *testing.T) {
		got, err := resolveGitLabOAuth(base, "cid", "csec", "https://ex/cb")
		if err != nil {
			t.Fatalf("err = %v, want nil", err)
		}
		if got == nil {
			t.Fatal("client = nil, want a constructed *auth.GitLabOAuth")
		}
		// The authorize URL is hosted on the configured base (the endpoint
		// host is FISHHAWKD_GITLAB_BASE_URL), and requests the read_api scope.
		authURL := got.AuthorizeURL("state-1")
		if !strings.HasPrefix(authURL, base+"/oauth/authorize") {
			t.Errorf("AuthorizeURL = %q, want prefixed by %s/oauth/authorize", authURL, base)
		}
		if !strings.Contains(authURL, "scope=read_api") {
			t.Errorf("AuthorizeURL = %q, want scope=read_api", authURL)
		}
	})
}

// TestServeWiresGitLabOAuthConfig drives runServe ITSELF to pin the
// production-reachability boundary the earlier vacuous form only claimed to
// cover (E44.22 / #2109 fix-up). That form assigned resolveGitLabOAuth's result
// to a LOCAL anonymous struct — never touching runServe, env parsing, or the
// real Config wiring — so it would have passed even if runServe never set
// cfg.GitLabOAuth. Here a full GitLab OAuth trio + base URL flows env/flag →
// validate → construct → cfg.GitLabOAuth through the actual boot path (proven by
// the "gitlab oauth sign-in configured" startup log emitted only from that
// assignment branch in serve.go), and a partial trio aborts startup at the
// misconfigured-refusal branch BEFORE the flow is wired. bootstrapAbortFlag
// halts the boot at the invalid review-resolution AFTER the GitLab OAuth block,
// so the full-trio log line is observable.
func TestServeWiresGitLabOAuthConfig(t *testing.T) {
	t.Run("full trio is validated, constructed, and wired", func(t *testing.T) {
		code, log := serveWithProfile(t,
			"-gitlab-base-url", "https://gitlab.example.com",
			"-gitlab-oauth-client-id", "cid",
			"-gitlab-oauth-client-secret", "csec",
			"-gitlab-oauth-callback-url", "https://ex/cb",
			bootstrapAbortFlag)
		if code != exitFailure {
			t.Fatalf("runServe exit = %d, want %d (startup aborts at the invalid review-resolution AFTER the GitLab OAuth block); log:\n%s", code, exitFailure, log)
		}
		if !strings.Contains(log, "gitlab oauth sign-in configured") {
			t.Errorf("a complete GitLab OAuth trio did not wire cfg.GitLabOAuth through runServe (missing the sign-in-configured log); /v0/auth/gitlab/* would stay 503. log:\n%s", log)
		}
	})

	t.Run("partial trio aborts startup with the misconfigured refusal", func(t *testing.T) {
		code, log := serveWithProfile(t,
			"-gitlab-base-url", "https://gitlab.example.com",
			"-gitlab-oauth-client-id", "cid",
			// client-secret and callback deliberately omitted → all-three-or-error.
			bootstrapAbortFlag)
		if code != exitFailure {
			t.Fatalf("runServe exit = %d, want %d (a partial GitLab OAuth trio must refuse to boot); log:\n%s", code, exitFailure, log)
		}
		if !strings.Contains(log, "gitlab oauth misconfigured") {
			t.Errorf("partial GitLab OAuth trio did not emit the misconfigured refusal from runServe; log:\n%s", log)
		}
		if strings.Contains(log, "gitlab oauth sign-in configured") {
			t.Errorf("a partial trio must NOT wire the sign-in flow; log:\n%s", log)
		}
	})

	// A GitLab-ONLY deployment (a database + GitLab OAuth, NO GitHub OAuth) must
	// build the membership resolver — the #2109 fix-up decoupled it from the
	// GitHub OAuth block. Before the fix cfg.AuthMembership stayed nil here, so
	// /v0/auth/gitlab/callback denied every sign-in with a nil-resolver 503 even
	// though cfg.GitLabOAuth was built. The "membership resolver configured" log
	// fires only from that construction branch, so its presence — with NO
	// "github oauth sign-in configured" line — proves the resolver is wired
	// independent of GitHub OAuth.
	t.Run("gitlab-only deployment with a database wires the membership resolver", func(t *testing.T) {
		url := pgtest.NewURL(t)
		code, log := serveWithProfile(t,
			"-db", url,
			"-gitlab-base-url", "https://gitlab.example.com",
			"-gitlab-oauth-client-id", "cid",
			"-gitlab-oauth-client-secret", "csec",
			"-gitlab-oauth-callback-url", "https://ex/cb",
			bootstrapAbortFlag)
		if code != exitFailure {
			t.Fatalf("runServe exit = %d, want %d; log:\n%s", code, exitFailure, log)
		}
		if strings.Contains(log, "github oauth sign-in configured") {
			t.Fatalf("test drove a GitLab-only deployment but GitHub OAuth was configured; log:\n%s", log)
		}
		if !strings.Contains(log, "membership resolver configured") {
			t.Errorf("a GitLab-only deployment with a database did not build the membership resolver; the gitlab callback would 503 on a nil resolver. log:\n%s", log)
		}
		if !strings.Contains(log, "gitlab oauth sign-in configured") {
			t.Errorf("gitlab sign-in flow not wired in a GitLab-only deployment; log:\n%s", log)
		}
	})
}

// EMU enterprise auto-join is ON exactly for a data-resident GHEC OAuth
// endpoint — the posture the serve wiring derives from the already-parsed
// endpoint config, with no new flag.
func TestEMUPostureFromGitHubEndpoints(t *testing.T) {
	ghec := resolveGitHubEndpoints("", "",
		"https://acme.ghe.com/login/oauth/authorize", "", "", "")
	if !authpkg.IsEMUOAuthHost(ghec.OAuth.AuthorizeURL) {
		t.Error("data-resident GHEC authorize URL did not yield EMU posture")
	}
	for _, authorize := range []string{
		"", // github.com default
		"https://github.com/login/oauth/authorize",
		"https://ghes.example.com/login/oauth/authorize",
	} {
		ep := resolveGitHubEndpoints("", "", authorize, "", "", "")
		if authpkg.IsEMUOAuthHost(ep.OAuth.AuthorizeURL) {
			t.Errorf("authorize URL %q yielded EMU posture, want off", authorize)
		}
	}
}

// identityProviderTokenIsNil reports whether the constructed provider's
// unexported REST-read token accessor is nil. It reads (never invokes) the
// field via reflection so the passthrough assertion needs no exported test
// hook on the identity package.
func identityProviderTokenIsNil(t *testing.T, p identity.IdentityProvider) bool {
	t.Helper()
	gh, ok := p.(*identity.GitHubIdentityProvider)
	if !ok {
		t.Fatalf("provider = %T, want *identity.GitHubIdentityProvider", p)
	}
	f := reflect.ValueOf(gh).Elem().FieldByName("token")
	if !f.IsValid() {
		t.Fatal("GitHubIdentityProvider has no token field; passthrough test is stale")
	}
	return f.IsNil()
}

// identityProviderBaseURLs reads the constructed provider's unexported
// apiBaseURL / oauthBaseURL via reflection (E44.2 / #1826), so the endpoint-
// override wiring assertion needs no exported test hook on identity.
func identityProviderBaseURLs(t *testing.T, p identity.IdentityProvider) struct{ api, oauth string } {
	t.Helper()
	gh, ok := p.(*identity.GitHubIdentityProvider)
	if !ok {
		t.Fatalf("provider = %T, want *identity.GitHubIdentityProvider", p)
	}
	v := reflect.ValueOf(gh).Elem()
	api := v.FieldByName("apiBaseURL")
	oauth := v.FieldByName("oauthBaseURL")
	if !api.IsValid() || !oauth.IsValid() {
		t.Fatal("GitHubIdentityProvider missing apiBaseURL/oauthBaseURL fields; base-URL test is stale")
	}
	return struct{ api, oauth string }{api.String(), oauth.String()}
}

// fakeTokenProvider is a non-nil githubapp.TokenProvider for the wiring seam
// test. TestResolveOperatorRepoToken only asserts accessor nil-ness at
// construction, so Token is never invoked.
type fakeTokenProvider struct{}

func (fakeTokenProvider) Token(context.Context, int64) (string, error) { return "tok", nil }

// TestResolveOperatorRepoToken pins the serve-construction seam the
// fake-driven server tests cannot reach (E39.10 / #1753, same lesson as
// resolveRefinementRepo): the operator-repo REST-read accessor is non-nil
// exactly when a GitHub client + TokenProvider + an "owner/name" operator
// repo are all present, and nil (→ anonymous reads) when any is absent or the
// repo is malformed. The current serve wiring passed nil here, which is the
// defect this asserts against.
func TestResolveOperatorRepoToken(t *testing.T) {
	gh := &githubclient.Client{}
	var tokens githubapp.TokenProvider = fakeTokenProvider{}

	if acc := resolveOperatorRepoToken(gh, tokens, "kuhlman-labs/fishhawk"); acc == nil {
		t.Error("resolveOperatorRepoToken with gh+tokens+repo returned nil accessor, want non-nil")
	}

	nilCases := []struct {
		name   string
		gh     *githubclient.Client
		tokens githubapp.TokenProvider
		repo   string
	}{
		{"gh nil", nil, tokens, "kuhlman-labs/fishhawk"},
		{"tokens nil", gh, nil, "kuhlman-labs/fishhawk"},
		{"repo empty", gh, tokens, ""},
		{"repo no slash", gh, tokens, "fishhawk"},
		{"repo empty owner", gh, tokens, "/fishhawk"},
		{"repo empty name", gh, tokens, "kuhlman-labs/"},
		{"repo extra slash", gh, tokens, "kuhlman-labs/fishhawk/extra"},
	}
	for _, tc := range nilCases {
		t.Run(tc.name, func(t *testing.T) {
			if acc := resolveOperatorRepoToken(tc.gh, tc.tokens, tc.repo); acc != nil {
				t.Errorf("resolveOperatorRepoToken(%s) = non-nil accessor, want nil (fail-closed → anonymous reads)", tc.name)
			}
		})
	}
}

// TestResolveGitLabClient covers the FISHHAWKD_GITLAB_BASE_URL /
// FISHHAWKD_GITLAB_TOKEN all-or-warn gate (ADR-058 #1856), mirroring the jira
// gating: both set constructs the client (no warn); exactly one set leaves the
// provider disabled with partial=true so the caller warns naming both vars;
// both empty is simply-unconfigured (nil client, no warn).
func TestResolveGitLabClient(t *testing.T) {
	t.Run("both set constructs the client", func(t *testing.T) {
		client, partial := resolveGitLabClient("https://gitlab.com", "glpat-tok")
		if client == nil {
			t.Fatal("client = nil with both base URL and token set, want a constructed client")
		}
		if partial {
			t.Error("partial = true with a complete config, want false")
		}
	})
	t.Run("only base URL is a partial config (disabled + warn)", func(t *testing.T) {
		client, partial := resolveGitLabClient("https://gitlab.com", "")
		if client != nil {
			t.Errorf("client = %v with token missing, want nil (disabled)", client)
		}
		if !partial {
			t.Error("partial = false with only the base URL set, want true so the caller warns")
		}
	})
	t.Run("only token is a partial config (disabled + warn)", func(t *testing.T) {
		client, partial := resolveGitLabClient("", "glpat-tok")
		if client != nil {
			t.Errorf("client = %v with base URL missing, want nil (disabled)", client)
		}
		if !partial {
			t.Error("partial = false with only the token set, want true so the caller warns")
		}
	})
	t.Run("both empty is unconfigured (no client, no warn)", func(t *testing.T) {
		client, partial := resolveGitLabClient("", "")
		if client != nil {
			t.Errorf("client = %v with nothing set, want nil", client)
		}
		if partial {
			t.Error("partial = true with nothing set, want false (not a misconfiguration)")
		}
	})
}

// gitlabBranchServer is a TLS httptest server answering the single
// Repository-branch read GetBranchSHA issues, counting requests (atomic) and
// recording the PRIVATE-TOKEN it saw. It mirrors the forge/gitlab package's
// countingBranchServer so the serve-seam test can observe which host
// resolveGitLabForge's constructed adapter actually reaches — proving the
// per-installation resolver + allowlist opts are forwarded, not dropped. TLS so
// a resolved base passes account.ValidateResolvedBaseURL's https requirement.
func gitlabBranchServer(t *testing.T) (*httptest.Server, *int64, *string) {
	t.Helper()
	var count int64
	var gotToken string
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/v4/projects/42/repository/branches/{branch}", func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&count, 1)
		gotToken = r.Header.Get("PRIVATE-TOKEN")
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"name":"main","commit":{"id":"deadbeef"}}`)
	})
	srv := httptest.NewTLSServer(mux)
	t.Cleanup(srv.Close)
	return srv, &count, &gotToken
}

// mustHostname returns rawURL's host with any port stripped — the shape
// account.HostAllowed matches an allowlist entry against.
func mustHostname(t *testing.T, rawURL string) string {
	t.Helper()
	u, err := url.Parse(rawURL)
	if err != nil {
		t.Fatalf("parse %q: %v", rawURL, err)
	}
	return u.Hostname()
}

// TestResolveGitLabForge pins the gated registration of the gitlab
// forge.Forge adapter (ADR-058 / E45.5, #1859): a complete config
// (both base URL and token) yields a registerable "gitlab" adapter, and any
// partial/empty config yields nil so the forge registry is left without a
// gitlab entry. The both-set case also drives the adapter through
// forge.Register / forge.Get to prove it is dispatchable under the id the
// registry keys it on.
func TestResolveGitLabForge(t *testing.T) {
	t.Run("both set constructs a registerable adapter", func(t *testing.T) {
		glForge := resolveGitLabForge("https://gitlab.com", "glpat-tok")
		if glForge == nil {
			t.Fatal("adapter = nil with both base URL and token set, want a constructed adapter")
		}
		if glForge.Name() != "gitlab" {
			t.Errorf("Name() = %q, want gitlab", glForge.Name())
		}
		// Dispatchable through the registry under "gitlab".
		forge.Register(glForge)
		got, err := forge.Get("gitlab")
		if err != nil {
			t.Fatalf("forge.Get(gitlab) after register: %v", err)
		}
		if got.Name() != "gitlab" {
			t.Errorf("resolved forge Name() = %q, want gitlab", got.Name())
		}
	})
	t.Run("only base URL is not registered", func(t *testing.T) {
		if glForge := resolveGitLabForge("https://gitlab.com", ""); glForge != nil {
			t.Errorf("adapter = %v with token missing, want nil (not registered)", glForge)
		}
	})
	t.Run("only token is not registered", func(t *testing.T) {
		if glForge := resolveGitLabForge("", "glpat-tok"); glForge != nil {
			t.Errorf("adapter = %v with base URL missing, want nil (not registered)", glForge)
		}
	})
	t.Run("both empty is not registered", func(t *testing.T) {
		if glForge := resolveGitLabForge("", ""); glForge != nil {
			t.Errorf("adapter = %v with nothing set, want nil (not registered)", glForge)
		}
	})
	t.Run("threads per-installation resolver + allowlist options", func(t *testing.T) {
		// The variadic opts (E44.16 / #2094) plumb the per-installation base-URL
		// resolver + host allowlist onto the underlying factory. A non-nil +
		// Name()=="gitlab" assertion is VACUOUS for this seam — it would pass
		// unchanged if resolveGitLabForge silently DROPPED the opts. So drive a
		// real scope-taking forge op through the constructed adapter and observe
		// the outbound host: if WithResolveBaseURL is dropped the request lands
		// on the deployment default instead of the resolved host, and if
		// WithAllowedInstallationHosts is dropped a reachable-but-disallowed
		// resolved host is admitted instead of failing closed. The forge-package
		// tests pin the resolver→client→outbound-host contract itself; this pins
		// that serve.go's helper actually forwards the opts into it.

		// Forwarded resolver → the outbound request lands on the RESOLVED host.
		resolvedSrv, resolvedCount, _ := gitlabBranchServer(t)
		defaultSrv, defaultCount, _ := gitlabBranchServer(t)
		glForge := resolveGitLabForge(defaultSrv.URL, "glpat-tok",
			forgegitlab.WithHTTPClient(resolvedSrv.Client()),
			forgegitlab.WithResolveBaseURL(func(context.Context, string) (string, error) { return resolvedSrv.URL, nil }),
			forgegitlab.WithAllowedInstallationHosts([]string{mustHostname(t, resolvedSrv.URL)}))
		if glForge == nil || glForge.Name() != "gitlab" {
			t.Fatalf("adapter = %v, want a constructed gitlab adapter", glForge)
		}
		if _, _, err := glForge.GetBranchSHA(context.Background(),
			forge.FromRef("gitlab:42"), forge.RepoRef{}, "main"); err != nil {
			t.Fatalf("GetBranchSHA: %v", err)
		}
		if got := atomic.LoadInt64(resolvedCount); got != 1 {
			t.Errorf("resolved host received %d requests, want 1 (WithResolveBaseURL must reach the factory through resolveGitLabForge)", got)
		}
		if got := atomic.LoadInt64(defaultCount); got != 0 {
			t.Errorf("default host received %d requests, want 0 (a forwarded override wins)", got)
		}

		// Forwarded allowlist → a reachable-but-disallowed resolved host FAILS
		// CLOSED. resolvedSrv is reachable, so WITHOUT the allowlist opt this op
		// would succeed; a non-matching allowlist must make it error and ship
		// no request (no token to the disallowed host).
		disSrv, disCount, disToken := gitlabBranchServer(t)
		glForge = resolveGitLabForge("https://gitlab.com", "glpat-tok",
			forgegitlab.WithHTTPClient(disSrv.Client()),
			forgegitlab.WithResolveBaseURL(func(context.Context, string) (string, error) { return disSrv.URL, nil }),
			forgegitlab.WithAllowedInstallationHosts([]string{"not-the-resolved-host.example"}))
		_, _, err := glForge.GetBranchSHA(context.Background(),
			forge.FromRef("gitlab:42"), forge.RepoRef{}, "main")
		if err == nil {
			t.Fatal("GetBranchSHA succeeded on a disallowed resolved host, want fail-closed (WithAllowedInstallationHosts must reach the factory through resolveGitLabForge)")
		}
		if got := atomic.LoadInt64(disCount); got != 0 {
			t.Errorf("disallowed host received %d requests, want 0 (token must never ship past the allowlist)", got)
		}
		if *disToken != "" {
			t.Errorf("disallowed host saw PRIVATE-TOKEN = %q, want empty (fail-closed before the token ships)", *disToken)
		}
	})
	t.Run("nil resolver option is a backward-compatible no-op", func(t *testing.T) {
		// installationBaseURLResolver(nil, …) returns a nil hook; passing it
		// through WithResolveBaseURL must leave a working deployment-default
		// adapter (not panic, not fail).
		glForge := resolveGitLabForge("https://gitlab.com", "glpat-tok",
			forgegitlab.WithResolveBaseURL(installationBaseURLResolver(nil, "gitlab")))
		if glForge == nil {
			t.Fatal("adapter = nil with a nil resolver hook, want a deployment-default adapter")
		}
	})
}

// TestLoadConventionsOverride covers the FISHHAWKD_WORKMGMT_CONVENTIONS
// startup override (ADR-058 #1856), retained as the per-repo loader's
// break-glass fallback (#2022): an empty path is a no-op (nil func, nil
// error — the loader falls to Default()); an unreadable path fails fast with
// an error naming the path; an invalid document STILL fails startup fast
// with an error naming the path + parse cause; a valid file returns the
// break-glass func serving the parsed conventions.
func TestLoadConventionsOverride(t *testing.T) {
	t.Run("empty path is a no-op", func(t *testing.T) {
		loader, err := loadConventionsOverride("")
		if err != nil {
			t.Fatalf("loadConventionsOverride(\"\") err = %v, want nil", err)
		}
		if loader != nil {
			t.Error("loader != nil for an empty path, want nil (no override)")
		}
	})

	t.Run("unreadable path fails fast naming the path", func(t *testing.T) {
		missing := filepath.Join(t.TempDir(), "does-not-exist.yaml")
		loader, err := loadConventionsOverride(missing)
		if err == nil {
			t.Fatal("err = nil for a missing file, want a read failure")
		}
		if loader != nil {
			t.Error("loader != nil on a read failure, want nil")
		}
		if !strings.Contains(err.Error(), missing) {
			t.Errorf("err = %v, want it to name the path %q", err, missing)
		}
	})

	t.Run("invalid document fails fast naming path and parse cause", func(t *testing.T) {
		bad := filepath.Join(t.TempDir(), "bad.yaml")
		// A structurally-invalid conventions doc: missing the required
		// spec_version/types, so workmgmt.Parse returns a typed error.
		if werr := os.WriteFile(bad, []byte("provider: gitlab\n"), 0o600); werr != nil {
			t.Fatalf("write bad file: %v", werr)
		}
		loader, err := loadConventionsOverride(bad)
		if err == nil {
			t.Fatal("err = nil for an invalid document, want a parse failure")
		}
		if loader != nil {
			t.Error("loader != nil on a parse failure, want nil")
		}
		if !strings.Contains(err.Error(), bad) {
			t.Errorf("err = %v, want it to name the path %q", err, bad)
		}
	})

	t.Run("valid file returns the parsed break-glass conventions", func(t *testing.T) {
		good := filepath.Join(t.TempDir(), "work-management.yaml")
		doc := "spec_version: work-management-v0\n" +
			"provider: gitlab\n" +
			"gitlab:\n" +
			"  project: group/subgroup/app\n" +
			"required_fields: [Summary, Done-means, complexity]\n" +
			"types: {feature: {body_skeleton: [Summary]}}\n"
		if werr := os.WriteFile(good, []byte(doc), 0o600); werr != nil {
			t.Fatalf("write good file: %v", werr)
		}
		loader, err := loadConventionsOverride(good)
		if err != nil {
			t.Fatalf("loadConventionsOverride(valid) err = %v, want nil", err)
		}
		if loader == nil {
			t.Fatal("loader = nil for a valid file, want the break-glass func")
		}
		conv, ok := loader()
		if !ok {
			t.Fatal("loader() ok = false, want true (override present)")
		}
		if conv.Provider != workmgmtgitlab.ProviderName {
			t.Errorf("Provider = %q, want %q (parsed from the override file)", conv.Provider, workmgmtgitlab.ProviderName)
		}
		if conv.GitLab == nil || conv.GitLab.Project != "group/subgroup/app" {
			t.Errorf("GitLab = %+v, want the parsed project override", conv.GitLab)
		}
	})
}

// TestBuildRepoConventionsLoader_OverrideAsFallback pins the #2022 wiring
// contract: buildRepoConventionsLoader assembles the per-repo loader with the
// break-glass override as its fallback, so on a deployment with no accounts
// database (nil resolver) and no registered forges a filing resolves the
// override conventions — and with no override, workmgmt.Default(). Together
// with TestLoadConventionsOverride's invalid-document branch this covers the
// serve.go contract that a broken override still fails startup fail-fast
// while a healthy one only surfaces when per-repo resolution falls through.
func TestBuildRepoConventionsLoader_OverrideAsFallback(t *testing.T) {
	srv := server.New(server.Config{})

	override := workmgmt.Default()
	override.Provider = workmgmtgitlab.ProviderName
	override.GitLab = &workmgmt.GitLabConnection{Project: "group/app"}
	loader := buildRepoConventionsLoader(srv, nil, func() (workmgmt.Conventions, bool) { return override, true }, nil)
	conv, err := loader.Load(context.Background(), "acme/widgets")
	if err != nil {
		t.Fatalf("Load err = %v, want nil", err)
	}
	if conv.Provider != workmgmtgitlab.ProviderName {
		t.Errorf("Provider = %q, want %q (the break-glass override must be the fallback)", conv.Provider, workmgmtgitlab.ProviderName)
	}

	noOverride := buildRepoConventionsLoader(srv, nil, nil, nil)
	conv, err = noOverride.Load(context.Background(), "acme/widgets")
	if err != nil {
		t.Fatalf("Load (no override) err = %v, want nil", err)
	}
	if conv.Provider != workmgmt.Default().Provider {
		t.Errorf("Provider = %q, want the Default() provider %q", conv.Provider, workmgmt.Default().Provider)
	}
}

// TestServe_WorkMgmtAllowedDestinations pins the E44.14 / #2090 boot wiring
// by driving runServe ITSELF: a MALFORMED
// FISHHAWKD_WORKMGMT_ALLOWED_DESTINATIONS FAILS startup with an error naming
// the variable — it must never log-and-continue or degrade to an empty
// (strict) allow-list, because a typo silently reverting to strict would
// masquerade as the security posture working while breaking a legitimate
// cross-namespace deployment. A well-formed value is accepted and startup
// proceeds past the guard (aborting later at the deliberately-invalid
// --review-resolution), proving the value was parsed rather than rejected.
// That the parsed value reaches the loader is compile-enforced by
// buildRepoConventionsLoader's signature and behaviorally covered by
// server.TestConventionsLoader_DestinationAllowListed.
func TestServe_WorkMgmtAllowedDestinations(t *testing.T) {
	t.Run("malformed value fails boot", func(t *testing.T) {
		for _, raw := range []string{"acme:github_projects", "acme:bitbucket:acme", ":github_projects:acme"} {
			code, log := serveWithProfile(t, "-workmgmt-allowed-destinations", raw)
			if code != exitFailure {
				t.Fatalf("runServe(%q) exit = %d, want %d (a malformed allow-list must fail boot); log:\n%s", raw, code, exitFailure, log)
			}
			if !strings.Contains(log, "FISHHAWKD_WORKMGMT_ALLOWED_DESTINATIONS") {
				t.Errorf("runServe(%q) log does not name the offending variable:\n%s", raw, log)
			}
			if !strings.Contains(log, raw) {
				t.Errorf("runServe(%q) log does not name the offending entry:\n%s", raw, log)
			}
		}
	})

	t.Run("well-formed value is accepted at boot", func(t *testing.T) {
		code, log := serveWithProfile(t,
			"-workmgmt-allowed-destinations", "acme:github_projects:enterprise,acme:jira:FISH",
			bootstrapAbortFlag)
		if code != exitFailure {
			t.Fatalf("runServe exit = %d, want %d (startup aborts at the invalid --review-resolution, AFTER the allow-list guard); log:\n%s", code, exitFailure, log)
		}
		if strings.Contains(log, "invalid FISHHAWKD_WORKMGMT_ALLOWED_DESTINATIONS") {
			t.Errorf("a well-formed allow-list was rejected at boot:\n%s", log)
		}
		if !strings.Contains(log, "review-resolution") {
			t.Errorf("startup did not reach the later --review-resolution guard, so the allow-list guard was not passed:\n%s", log)
		}
	})
}

// TestWebhookStoreNeeded pins the delivery-store gating (E45.6 / #1860):
// the shared webhook delivery store is created when EITHER forge's
// webhook secret is set — including the GitLab-only case, which the
// prior GitHub-secret-only gate would have skipped — and NOT created
// when neither is set. The GitHub-only case stays unchanged.
func TestWebhookStoreNeeded(t *testing.T) {
	cases := []struct {
		name         string
		githubSecret string
		gitlabSecret string
		want         bool
	}{
		{"neither", "", "", false},
		{"github-only", "gh", "", true},
		{"gitlab-only", "", "gl", true},
		{"both", "gh", "gl", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := webhookStoreNeeded(tc.githubSecret, tc.gitlabSecret); got != tc.want {
				t.Errorf("webhookStoreNeeded(%q,%q) = %v, want %v",
					tc.githubSecret, tc.gitlabSecret, got, tc.want)
			}
		})
	}
}

// TestNewWebhookDeliveryStore pins that the delivery-store CONSTRUCTION
// (not just the webhookStoreNeeded predicate) consults both forge secrets:
// a GitLab-only-secret deployment gets a store just as a GitHub-only one
// does, neither-secret gets nil, and a pool selects the Postgres store with
// its evictor handle (E45.6 binding condition 2 / concern 4). Testing the
// extracted constructor closes most of the "runServe re-inlines a
// GitHub-only gate" seam the predicate test alone left open.
func TestNewWebhookDeliveryStore(t *testing.T) {
	const retention = time.Hour
	// A zero-value pool is non-nil; NewPostgresStore only stores the
	// pointer, so no DB connection is needed to exercise store selection.
	pool := &pgxpool.Pool{}

	cases := []struct {
		name         string
		pool         *pgxpool.Pool
		githubSecret string
		gitlabSecret string
		wantStore    bool
		wantEvictor  bool
	}{
		{"neither-secret-no-store", nil, "", "", false, false},
		{"github-only-memory", nil, "gh", "", true, false},
		{"gitlab-only-memory", nil, "", "gl", true, false},
		{"gitlab-only-postgres", pool, "", "gl", true, true},
		{"both-postgres", pool, "gh", "gl", true, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store, evictor := newWebhookDeliveryStore(tc.pool, tc.githubSecret, tc.gitlabSecret, retention)
			if (store != nil) != tc.wantStore {
				t.Errorf("store != nil = %v, want %v", store != nil, tc.wantStore)
			}
			if (evictor != nil) != tc.wantEvictor {
				t.Errorf("evictor != nil = %v, want %v", evictor != nil, tc.wantEvictor)
			}
			// The Postgres path must return the SAME object for both
			// returns so the evictor is wired to the live store.
			if tc.wantEvictor {
				var _ webhook.DeliveryStore = evictor
				if store != webhook.DeliveryStore(evictor) {
					t.Errorf("postgres path: store and evictor differ; evictor must be the store")
				}
			}
		})
	}
}

// --- Regional cells: serve wiring (ADR-062, E44.7 / #1831) ------------------

// stubAccountQueries is a minimal account.RegionPinnerQueries so
// resolveRegionPin's "a database is configured" input can be supplied without
// a pool. It records the pin it was asked for.
type stubAccountQueries struct {
	calls []accountdb.PinAccountHomeRegionParams
}

func (s *stubAccountQueries) PinAccountHomeRegion(_ context.Context, arg accountdb.PinAccountHomeRegionParams) (accountdb.Account, error) {
	s.calls = append(s.calls, arg)
	return accountdb.Account{Provider: arg.Provider, AccountKey: arg.AccountKey, HomeRegion: arg.HomeRegion}, nil
}

func (s *stubAccountQueries) GetAccountByKey(_ context.Context, arg accountdb.GetAccountByKeyParams) (accountdb.Account, error) {
	return accountdb.Account{Provider: arg.Provider, AccountKey: arg.AccountKey}, nil
}

// TestResolveRegionPin_Postures covers every branch of the construction gate:
// the surface is built only when the region, the secret AND a query surface are
// all present, and each individual absence disables it.
func TestResolveRegionPin_Postures(t *testing.T) {
	q := &stubAccountQueries{}
	for _, tc := range []struct {
		name        string
		region      string
		secret      string
		queries     account.RegionPinnerQueries
		wantSecret  string
		wantEnabled bool
		wantLog     string
	}{
		{"all three set", "eu", "s3cret", q, "s3cret", true, "region-pin surface enabled"},
		{"region unset", "", "s3cret", q, "", false, "FISHHAWKD_HOME_REGION"},
		{"secret unset", "eu", "", q, "", false, "FISHHAWKD_HANDOFF_SECRET"},
		{"no database", "eu", "s3cret", nil, "", false, "FISHHAWKD_DATABASE_URL"},
		{"nothing set", "", "", nil, "", false, "FISHHAWKD_HOME_REGION"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var buf strings.Builder
			logger := slog.New(slog.NewTextHandler(&buf, nil))
			secret, pinner := resolveRegionPin(regionPinOptions{
				homeRegion:    tc.region,
				handoffSecret: tc.secret,
			}, tc.queries, logger)

			if secret != tc.wantSecret {
				t.Errorf("secret = %q, want %q", secret, tc.wantSecret)
			}
			if pinner.Enabled() != tc.wantEnabled {
				t.Errorf("pinner.Enabled() = %v, want %v", pinner.Enabled(), tc.wantEnabled)
			}
			if !tc.wantEnabled && pinner != nil {
				t.Errorf("pinner = %v, want nil when the surface is disabled", pinner)
			}
			if !strings.Contains(buf.String(), tc.wantLog) {
				t.Errorf("startup log = %q, want it to mention %q", buf.String(), tc.wantLog)
			}
		})
	}
}

// signedOnboardingRequest returns the routed onboarding GET carrying a handoff
// actually signed with secret — not a hand-built parameter set.
func signedOnboardingRequest(t *testing.T, secret, region string) *http.Request {
	t.Helper()
	signed, err := handoff.Sign(secret, handoff.Params{
		Provider:   "github",
		AccountKey: "acme",
		HomeRegion: region,
		ExpiresAt:  time.Now().Add(5 * time.Minute),
		Nonce:      "nonce-abcdef0123456789",
	})
	if err != nil {
		t.Fatalf("sign handoff: %v", err)
	}
	return httptest.NewRequest(http.MethodGet,
		server.RoutedOnboardingPath+"?"+signed.Encode(), nil)
}

// serveRegionPinResponse runs a signed routed request through a server whose
// region-pin config came from resolveRegionPin, i.e. exactly the wiring
// runServe performs.
func serveRegionPinResponse(t *testing.T, opts regionPinOptions, q account.RegionPinnerQueries, signSecret, signRegion string) *httptest.ResponseRecorder {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	cfg := server.Config{Logger: logger}
	cfg.HandoffSecret, cfg.RegionPinner = resolveRegionPin(opts, q, logger)

	rec := httptest.NewRecorder()
	server.New(cfg).Handler().ServeHTTP(rec, signedOnboardingRequest(t, signSecret, signRegion))
	return rec
}

// TestServeWiring_SecretSetRegionUnsetRefusesSignedRequest is the fail-closed
// behavioral assertion (binding condition 8). Asserting only that the pinner
// was not constructed would not prove the surface REFUSES rather than bypasses,
// so this sends a genuinely-signed handoff — one the configured secret would
// verify if the region were set — and asserts it is turned away.
func TestServeWiring_SecretSetRegionUnsetRefusesSignedRequest(t *testing.T) {
	const secret = "shared-cell-directory-secret"
	q := &stubAccountQueries{}

	rec := serveRegionPinResponse(t, regionPinOptions{
		homeRegion:    "", // the fail-closed input under test
		handoffSecret: secret,
	}, q, secret, "eu")

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want %d (a signed handoff must be REFUSED, not bypassed, when the cell region is unset); body: %s",
			rec.Code, http.StatusServiceUnavailable, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "region_pin_disabled") {
		t.Errorf("body = %s, want the region_pin_disabled error code", rec.Body.String())
	}
	if len(q.calls) != 0 {
		t.Errorf("PinAccountHomeRegion called %d times, want 0 — a disabled cell must not write", len(q.calls))
	}
}

// TestServeWiring_RegionSetSecretUnsetRefusesSignedRequest is the mirror
// fail-closed case: the cell knows its region but shares no secret, so it
// cannot verify a residency claim and must refuse rather than serve.
func TestServeWiring_RegionSetSecretUnsetRefusesSignedRequest(t *testing.T) {
	q := &stubAccountQueries{}

	rec := serveRegionPinResponse(t, regionPinOptions{
		homeRegion:    "eu",
		handoffSecret: "", // the fail-closed input under test
	}, q, "some-other-secret", "eu")

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want %d; body: %s", rec.Code, http.StatusServiceUnavailable, rec.Body.String())
	}
	if len(q.calls) != 0 {
		t.Errorf("PinAccountHomeRegion called %d times, want 0", len(q.calls))
	}
}

// TestServeWiring_BothSetHonoursSignedRequest is the positive control: with
// both env values set the wiring really does construct a working surface, so
// the two refusals above are fail-closed behavior and not a dead route.
func TestServeWiring_BothSetHonoursSignedRequest(t *testing.T) {
	const secret = "shared-cell-directory-secret"
	q := &stubAccountQueries{}

	rec := serveRegionPinResponse(t, regionPinOptions{
		homeRegion:    "eu",
		handoffSecret: secret,
	}, q, secret, "eu")

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}
	if len(q.calls) != 1 {
		t.Fatalf("PinAccountHomeRegion called %d times, want exactly 1", len(q.calls))
	}
	got := q.calls[0]
	if got.Provider != "github" || got.AccountKey != "acme" || got.HomeRegion == nil || *got.HomeRegion != "eu" {
		t.Errorf("pin = %+v, want the identity carried by the signed handoff (github/acme/eu)", got)
	}
}

// TestInferenceAPIKey_RegionKeyWinsElseAnthropicKey pins the credential
// precedence for the region-scoped inference endpoint.
func TestInferenceAPIKey_RegionKeyWinsElseAnthropicKey(t *testing.T) {
	for _, tc := range []struct {
		name      string
		anthropic string
		region    string
		baseURL   string
		want      string
	}{
		// A region-scoped key with NO region endpoint is half-configured in the
		// direction that EGRESSES: the SDK would present it — and the review
		// text — to its global default endpoint. Fail closed.
		{"region key without a region endpoint", "deployment-key", "region-key", "", ""},
		{"falls back to the anthropic key on the default endpoint", "deployment-key", "", "", "deployment-key"},
		{"both empty", "", "", "", ""},
		// A custom endpoint with no region key must NOT receive the
		// deployment credential — that would ship a production secret (and
		// the review text it authenticates) to an operator-supplied host.
		{"no fallback to a custom endpoint", "deployment-key", "", "https://eu.inference.example.com", ""},
		{"region key still wins on a custom endpoint", "deployment-key", "region-key", "https://eu.inference.example.com", "region-key"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			opts := planReviewerOptions{anthropicAPIKey: tc.anthropic, modelAPIKey: tc.region, modelBaseURL: tc.baseURL}
			if got := opts.inferenceAPIKey(); got != tc.want {
				t.Errorf("inferenceAPIKey() = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestResolvePlanReviewers_DeploymentKeyNeverReachesCustomEndpoint is the
// behavioral half of the no-fallback rule: with a custom endpoint configured
// and NO region-scoped key, the reviewer must not present
// FISHHAWKD_ANTHROPIC_API_KEY — nor any ambient Anthropic credential — to that
// endpoint. Asserting the resolved string alone would not prove what the adapter
// puts on the wire, so this drives the real resolvePlanReviewers -> Default() ->
// Review path through an httptest endpoint and inspects the observed headers.
//
// It proves PRODUCTION behavior, not an environment-cleared artifact: an
// operator shell commonly exports ambient Anthropic credentials, so this SETS
// ANTHROPIC_API_KEY and ANTHROPIC_AUTH_TOKEN to distinct sentinels (never clears
// them). With the fix (anthropic.NewClient appending
// option.WithoutEnvironmentDefaults() on an empty key), the SDK autoloader is
// suppressed, so a request may legitimately reach the mock carrying NO
// credential — the security invariant is header-ABSENCE on the wire, not a
// request count or a NoCredentialsError. The assertion is therefore that neither
// the deployment key nor either ambient sentinel appears in any observed
// x-api-key / Authorization header.
func TestResolvePlanReviewers_DeploymentKeyNeverReachesCustomEndpoint(t *testing.T) {
	const (
		deploymentKey    = "deployment-anthropic-key"
		ambientAPIKey    = "ambient-api-key-sentinel"
		ambientAuthToken = "ambient-auth-token-sentinel"
	)
	// SET the ambient sources to distinct sentinels — the production posture —
	// rather than blanking them, so the test proves the autoloader is neutralized
	// and not merely that the host environment happened to be empty.
	t.Setenv("ANTHROPIC_API_KEY", ambientAPIKey)
	t.Setenv("ANTHROPIC_AUTH_TOKEN", ambientAuthToken)

	var mu sync.Mutex
	var seenAPIKey, seenAuth []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seenAPIKey = append(seenAPIKey, r.Header.Get("x-api-key"))
		seenAuth = append(seenAuth, r.Header.Get("Authorization"))
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"msg_test","type":"message","role":"assistant",` +
			`"content":[{"type":"text","text":"{\"verdict\":\"approve\"}"}],` +
			`"model":"claude-sonnet-4-6","stop_reason":"end_turn",` +
			`"usage":{"input_tokens":1,"output_tokens":1}}`))
	}))
	defer srv.Close()

	set, err := resolvePlanReviewers(planReviewerOptions{
		anthropicAPIKey:     deploymentKey,
		planReviewModel:     "claude-sonnet-4-6",
		planReviewMaxTokens: 1024,
		planReviewTimeout:   10 * time.Second,
		modelBaseURL:        srv.URL,
		modelAPIKey:         "", // the half-configured input under test
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("resolvePlanReviewers error = %v, want nil (no FISHHAWKD_HOME_REGION, so no fail-closed refusal)", err)
	}

	reviewer := set.Default()
	if reviewer == nil {
		t.Fatal("Default() = nil, want the anthropic adapter (the deployment key still selects it)")
	}
	// Whether this reaches the 200 stub or errors before opening a connection is
	// irrelevant post-fix: with the autoloader suppressed and an empty explicit
	// key the request carries no credential either way. The return is ignored;
	// the observed wire headers are the invariant.
	_, _, _ = reviewer.Review(context.Background(), "criteria\n### Plan artifact\n\nthe plan")

	mu.Lock()
	defer mu.Unlock()
	// No operator or ambient credential may travel to FISHHAWKD_MODEL_BASE_URL —
	// one assertion per named credential source, across every observed request.
	for i := range seenAPIKey {
		for _, secret := range []struct{ name, val string }{
			{"deployment FISHHAWKD_ANTHROPIC_API_KEY", deploymentKey},
			{"ambient ANTHROPIC_API_KEY", ambientAPIKey},
			{"ambient ANTHROPIC_AUTH_TOKEN", ambientAuthToken},
		} {
			if strings.Contains(seenAPIKey[i], secret.val) || strings.Contains(seenAuth[i], secret.val) {
				t.Errorf("request %d carried the %s to the custom endpoint (x-api-key=%q Authorization=%q); "+
					"no operator or ambient credential may reach FISHHAWKD_MODEL_BASE_URL",
					i, secret.name, seenAPIKey[i], seenAuth[i])
			}
		}
	}
}

// TestResolvePlanReviewers_WarnsOnBaseURLWithoutRegionKey pins the operator
// signal for that half-configured posture: it fails closed, and says so.
func TestResolvePlanReviewers_WarnsOnBaseURLWithoutRegionKey(t *testing.T) {
	var buf strings.Builder
	resolvePlanReviewers(planReviewerOptions{
		anthropicAPIKey: "deployment-key",
		modelBaseURL:    "https://eu.inference.example.com",
	}, slog.New(slog.NewTextHandler(&buf, nil)))

	if !strings.Contains(buf.String(), "FISHHAWKD_MODEL_API_KEY") {
		t.Errorf("startup log = %q, want a warning naming FISHHAWKD_MODEL_API_KEY", buf.String())
	}
}

// TestResolvePlanReviewers_RegionKeyWithoutEndpointWithholdsAnthropic is the
// mirror fail-closed posture: FISHHAWKD_MODEL_API_KEY set with no
// FISHHAWKD_MODEL_BASE_URL would point the SDK at its GLOBAL default endpoint,
// sending both the region-scoped credential and the review text out of region.
// Withholding the credential alone would not help — the request body still
// travels — so the adapter itself must be withheld, on both resolution paths.
func TestResolvePlanReviewers_RegionKeyWithoutEndpointWithholdsAnthropic(t *testing.T) {
	var buf strings.Builder
	set, err := resolvePlanReviewers(planReviewerOptions{
		anthropicAPIKey:     "deployment-key",
		planReviewModel:     "claude-sonnet-4-6",
		planReviewMaxTokens: 1024,
		planReviewTimeout:   10 * time.Second,
		modelAPIKey:         "eu-region-scoped-key",
		modelBaseURL:        "", // the half-configured input under test
	}, slog.New(slog.NewTextHandler(&buf, nil)))
	if err != nil {
		t.Fatalf("resolvePlanReviewers error = %v, want nil (no FISHHAWKD_HOME_REGION, so the withhold-and-warn path stays, not a refusal)", err)
	}

	if got := set.Default(); got != nil {
		t.Errorf("Default() = %T, want nil — the anthropic adapter must not be constructed against the global default endpoint with a region key", got)
	}
	if _, err := set.For("anthropic", ""); err == nil {
		t.Error("For(\"anthropic\") = nil error, want a refusal naming FISHHAWKD_MODEL_BASE_URL")
	} else if !strings.Contains(err.Error(), "FISHHAWKD_MODEL_BASE_URL") {
		t.Errorf("For(\"anthropic\") error = %v, want it to name FISHHAWKD_MODEL_BASE_URL", err)
	}
	if !strings.Contains(buf.String(), "FISHHAWKD_MODEL_BASE_URL") {
		t.Errorf("startup log = %q, want a warning naming FISHHAWKD_MODEL_BASE_URL", buf.String())
	}
}

// TestResolvePlanReviewers_RegionScopedHalfConfiguredRefusesAllAdapters is the
// #2107 fix and inverts the former
// ...RegionKeyWithoutEndpointFallsThroughToOtherAdapters test: when the cell is
// REGION-SCOPED (FISHHAWKD_HOME_REGION set), a subprocess adapter is no longer a
// safe fall-through — it would egress residency-sensitive review text via its
// own unverified global endpoint. So in the region-key-without-endpoint posture
// with claudecode enabled, resolvePlanReviewers refuses: a nil set and a
// non-nil error naming the missing FISHHAWKD_MODEL_BASE_URL. Mode (1) of the
// per-mode matrix.
func TestResolvePlanReviewers_RegionScopedHalfConfiguredRefusesAllAdapters(t *testing.T) {
	set, err := resolvePlanReviewers(planReviewerOptions{
		homeRegion:                "eu",
		anthropicAPIKey:           "deployment-key",
		enableLocalClaudeReviewer: true,
		localClaudeBinary:         "claude",
		localClaudeModel:          "claude-sonnet-4-6",
		modelAPIKey:               "eu-region-scoped-key",
		modelBaseURL:              "", // region-scoped, in-region endpoint missing
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))

	if err == nil {
		t.Fatal("resolvePlanReviewers error = nil, want a fail-closed refusal (a region-scoped cell must not fall through to the claudecode subprocess, which egresses out of region)")
	}
	if !strings.Contains(err.Error(), "FISHHAWKD_MODEL_BASE_URL") {
		t.Errorf("refusal error = %v, want it to name the missing FISHHAWKD_MODEL_BASE_URL", err)
	}
	if set != nil {
		t.Errorf("resolvePlanReviewers set = %T, want nil on refusal (no reviewer may run)", set)
	}
}

// TestServe_RegionScopedHalfConfiguredRefusesToBoot pins the binding-condition
// call-site wiring (#2107): the serve() production path MUST consume the
// resolvePlanReviewers error and return exitFailure — an implementer who writes
// `set, _ := resolvePlanReviewers(...)` and drops the error must be caught here,
// not silently boot. It drives runServe ITSELF in a region-scoped half-configured
// posture (FISHHAWKD_HOME_REGION set, region endpoint missing, claudecode
// enabled). bootstrapAbortFlag is included so that IF the error were discarded,
// startup would continue past the reviewer-resolution point and abort later at
// the review-resolution guard (still exitFailure) WITHOUT the reviewer-refusal
// log line — so the log assertion, not the exit code alone, is what fails when
// the call site ignores the error.
func TestServe_RegionScopedHalfConfiguredRefusesToBoot(t *testing.T) {
	code, log := serveWithProfile(t,
		"-home-region=eu",
		"-model-api-key=eu-region-scoped-key",
		"-enable-local-claude-reviewer",
		bootstrapAbortFlag)

	if code != exitFailure {
		t.Fatalf("runServe exit = %d, want %d — a region-scoped cell with half-configured in-region inference must refuse to boot; log:\n%s", code, exitFailure, log)
	}
	// "plan-review configuration refused startup" is emitted ONLY by the serve()
	// call site's error branch (the resolver returns the error, it does not log
	// it). So this phrase's presence proves the boot aborted AT the reviewer
	// resolution because the call site consumed the error — not later at the
	// bootstrapAbortFlag guard because the call site discarded it.
	if !strings.Contains(log, "plan-review configuration refused startup") {
		t.Errorf("startup log did not carry the call-site refusal; the call site likely discarded the resolvePlanReviewers error and aborted elsewhere. log:\n%s", log)
	}
	if !strings.Contains(log, "FISHHAWKD_MODEL_BASE_URL") {
		t.Errorf("refusal log = %q, want it to name the missing FISHHAWKD_MODEL_BASE_URL", log)
	}
}

// TestServe_RegionScopedBaseURLWithoutKeyRefusesToBoot is the mirror call-site
// wiring assertion for the other half-configured posture (endpoint set, key
// missing): runServe returns exitFailure and the refusal names
// FISHHAWKD_MODEL_API_KEY.
func TestServe_RegionScopedBaseURLWithoutKeyRefusesToBoot(t *testing.T) {
	code, log := serveWithProfile(t,
		"-home-region=eu",
		"-model-base-url=https://eu.inference.example.com",
		"-enable-local-claude-reviewer",
		bootstrapAbortFlag)

	if code != exitFailure {
		t.Fatalf("runServe exit = %d, want %d; log:\n%s", code, exitFailure, log)
	}
	if !strings.Contains(log, "plan-review configuration refused startup") || !strings.Contains(log, "FISHHAWKD_MODEL_API_KEY") {
		t.Errorf("startup log did not carry the call-site reviewer refusal naming FISHHAWKD_MODEL_API_KEY; log:\n%s", log)
	}
}

// TestResolvePlanReviewers_RegionScopedBaseURLWithoutKeyRefuses is mode (2): the
// other half-configured posture (FISHHAWKD_MODEL_BASE_URL set, API key unset) on
// a region-scoped cell refuses too, and the error names the missing
// FISHHAWKD_MODEL_API_KEY.
func TestResolvePlanReviewers_RegionScopedBaseURLWithoutKeyRefuses(t *testing.T) {
	set, err := resolvePlanReviewers(planReviewerOptions{
		homeRegion:      "eu",
		anthropicAPIKey: "deployment-key",
		modelBaseURL:    "https://eu.inference.example.com",
		modelAPIKey:     "", // region-scoped, credential missing
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))

	if err == nil {
		t.Fatal("resolvePlanReviewers error = nil, want a fail-closed refusal for a region-scoped cell with FISHHAWKD_MODEL_API_KEY unset")
	}
	if !strings.Contains(err.Error(), "FISHHAWKD_MODEL_API_KEY") {
		t.Errorf("refusal error = %v, want it to name the missing FISHHAWKD_MODEL_API_KEY", err)
	}
	if set != nil {
		t.Errorf("resolvePlanReviewers set = %T, want nil on refusal", set)
	}
}

// TestResolvePlanReviewers_RegionScopedNoInferenceNamesBothVars is mode (3): a
// region-scoped cell with BOTH region knobs unset and only FISHHAWKD_ANTHROPIC_API_KEY
// set — the posture that would otherwise run the anthropic adapter on the SDK's
// global default endpoint — refuses, and the error names BOTH missing variables.
func TestResolvePlanReviewers_RegionScopedNoInferenceNamesBothVars(t *testing.T) {
	set, err := resolvePlanReviewers(planReviewerOptions{
		homeRegion:      "eu",
		anthropicAPIKey: "deployment-key",
		// both region knobs unset
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))

	if err == nil {
		t.Fatal("resolvePlanReviewers error = nil, want a refusal — the anthropic adapter would otherwise run on the SDK global default endpoint out of region")
	}
	if !strings.Contains(err.Error(), "FISHHAWKD_MODEL_BASE_URL") || !strings.Contains(err.Error(), "FISHHAWKD_MODEL_API_KEY") {
		t.Errorf("refusal error = %v, want it to name BOTH FISHHAWKD_MODEL_BASE_URL and FISHHAWKD_MODEL_API_KEY", err)
	}
	if set != nil {
		t.Errorf("resolvePlanReviewers set = %T, want nil on refusal", set)
	}
}

// TestResolvePlanReviewers_RegionScopedFullyConfiguredBoots is mode (4): the
// happy regional path — a region-scoped cell with both region knobs set and an
// anthropic key boots with no error, and Default() returns the region-pinned
// anthropic adapter.
func TestResolvePlanReviewers_RegionScopedFullyConfiguredBoots(t *testing.T) {
	set, err := resolvePlanReviewers(planReviewerOptions{
		homeRegion:      "eu",
		anthropicAPIKey: "deployment-key",
		planReviewModel: "claude-sonnet-4-6",
		modelBaseURL:    "https://eu.inference.example.com",
		modelAPIKey:     "eu-region-scoped-key",
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("resolvePlanReviewers error = %v, want nil on the fully-configured regional path", err)
	}
	if _, ok := set.Default().(*anthropic.Reviewer); !ok {
		t.Errorf("Default() = %T, want the region-pinned *anthropic.Reviewer", set.Default())
	}
}

// TestResolvePlanReviewers_HomeRegionUnsetPreservesFallThrough is mode (5) and
// guards against over-reach: with FISHHAWKD_HOME_REGION UNSET (every deployment
// today) the region-key-without-endpoint posture still falls through to the
// claudecode subprocess adapter, byte-for-byte as before the #2107 fix — the
// refusal keys on the cell being region-scoped, not on the inference config
// alone.
func TestResolvePlanReviewers_HomeRegionUnsetPreservesFallThrough(t *testing.T) {
	set, err := resolvePlanReviewers(planReviewerOptions{
		// homeRegion deliberately unset
		anthropicAPIKey:           "deployment-key",
		enableLocalClaudeReviewer: true,
		localClaudeBinary:         "claude",
		localClaudeModel:          "claude-sonnet-4-6",
		modelAPIKey:               "eu-region-scoped-key",
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("resolvePlanReviewers error = %v, want nil when FISHHAWKD_HOME_REGION is unset (fall-through preserved)", err)
	}
	if _, ok := set.Default().(*claudecode.Reviewer); !ok {
		t.Errorf("Default() = %T, want the claudecode adapter (the preserved fall-through)", set.Default())
	}
}

// TestResolvePlanReviewers_RegionScopedNoReviewerBoots is mode (6): a
// region-scoped cell with in-region inference NOT fully configured but NO
// reviewer adapter configured has no egress risk, so it still boots (nil error)
// and keeps the existing configured==0 warning. The refusal fires only when a
// reviewer would actually run.
func TestResolvePlanReviewers_RegionScopedNoReviewerBoots(t *testing.T) {
	var buf strings.Builder
	set, err := resolvePlanReviewers(planReviewerOptions{
		homeRegion: "eu",
		// no reviewer adapter, region inference not fully configured
	}, slog.New(slog.NewTextHandler(&buf, nil)))
	if err != nil {
		t.Fatalf("resolvePlanReviewers error = %v, want nil — a reviewer-less region-scoped cell has no egress risk and must still boot", err)
	}
	if set == nil {
		t.Fatal("resolvePlanReviewers set = nil, want a usable (empty) set")
	}
	if got := set.Default(); got != nil {
		t.Errorf("Default() = %T, want nil when no adapter is configured", got)
	}
	if !strings.Contains(buf.String(), "plan-review agent not configured") {
		t.Errorf("startup log = %q, want the existing configured==0 warning preserved", buf.String())
	}
}

// TestResolvePlanReviewers_RegionKeyAloneDoesNotSelectAnthropic guards the
// documented non-effect: FISHHAWKD_MODEL_API_KEY redirects a credential, it
// does not enable an adapter FISHHAWKD_ANTHROPIC_API_KEY did not select.
func TestResolvePlanReviewers_RegionKeyAloneDoesNotSelectAnthropic(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	set, err := resolvePlanReviewers(planReviewerOptions{
		modelAPIKey:  "region-key",
		modelBaseURL: "https://eu.inference.example.com",
	}, logger)
	if err != nil {
		t.Fatalf("resolvePlanReviewers error = %v, want nil (no FISHHAWKD_HOME_REGION, no reviewer adapter)", err)
	}
	if set.Default() != nil {
		t.Errorf("Default() = %T, want nil — a region key alone must not select the anthropic adapter", set.Default())
	}
}

// TestResolvePlanReviewers_AnthropicReviewerUsesRegionEndpoint is the
// serve-side half of binding condition 7: the reviewer this wiring actually
// hands the server calls the region endpoint with the region key. The
// per-review-path coverage lives in
// backend/internal/anthropic/client_test.go's
// TestRegionScopedInference_BothReviewPaths, which drives both the plan and the
// implement-review prompt shape through the same adapter.
func TestResolvePlanReviewers_AnthropicReviewerUsesRegionEndpoint(t *testing.T) {
	const regionKey = "eu-region-scoped-key"
	var gotHost, gotAPIKey, gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotHost, gotAPIKey, gotAuth = r.Host, r.Header.Get("x-api-key"), r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"msg_test","type":"message","role":"assistant",` +
			`"content":[{"type":"text","text":"{\"verdict\":\"approve\"}"}],` +
			`"model":"claude-sonnet-4-6","stop_reason":"end_turn",` +
			`"usage":{"input_tokens":1,"output_tokens":1}}`))
	}))
	defer srv.Close()

	set, err := resolvePlanReviewers(planReviewerOptions{
		anthropicAPIKey:     "deployment-key",
		planReviewModel:     "claude-sonnet-4-6",
		planReviewMaxTokens: 1024,
		planReviewTimeout:   10 * time.Second,
		modelBaseURL:        srv.URL,
		modelAPIKey:         regionKey,
	}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatalf("resolvePlanReviewers error = %v, want nil (fully configured, no FISHHAWKD_HOME_REGION)", err)
	}

	reviewer := set.Default()
	if reviewer == nil {
		t.Fatal("Default() = nil, want the anthropic adapter")
	}
	if _, _, err := reviewer.Review(context.Background(), "criteria\n### Plan artifact\n\nthe plan"); err != nil {
		t.Fatalf("Review: %v", err)
	}

	if want := strings.TrimPrefix(srv.URL, "http://"); gotHost != want {
		t.Errorf("request reached host %q, want the region endpoint %q", gotHost, want)
	}
	if gotAPIKey != regionKey && gotAuth != "Bearer "+regionKey {
		t.Errorf("credential = x-api-key:%q Authorization:%q, want the region-scoped key %q",
			gotAPIKey, gotAuth, regionKey)
	}
}

// --- Single-tenant deployment profile (ADR-057 Mode 1, E44.9 / #1833) -------
//
// These drive runServe ITSELF, not account.EnsureSingleTenantAccount, so the
// startup-bootstrap criterion cannot pass while serve.go fails to invoke it
// (binding condition 2). Each configured-profile case aborts startup at the
// deliberately-invalid --review-resolution guard, which runs AFTER the
// bootstrap and BEFORE the listener binds — so runServe returns rather than
// serving, and the assertion is on what it wrote (or did not write) to the
// database on the way there.
const bootstrapAbortFlag = "-review-resolution=not-a-real-resolver"

func serveWithProfile(t *testing.T, args ...string) (int, string) {
	t.Helper()
	var logSink bytes.Buffer
	code := runServe(args, &logSink)
	return code, logSink.String()
}

func countAccounts(t *testing.T, url, accountKey string) int {
	t.Helper()
	pool, err := pgxpool.New(context.Background(), url)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	defer pool.Close()
	var n int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM accounts WHERE $1 = '' OR account_key = $1`, accountKey).Scan(&n); err != nil {
		t.Fatalf("count accounts: %v", err)
	}
	return n
}

// (b) A configured profile: runServe REALLY performs the bootstrap — the
// accounts row exists afterwards, carrying the internally-defaulted
// granularity and a non-NULL auto-join role.
func TestServe_SingleTenantProfileBootstrapsAccountRow(t *testing.T) {
	url := pgtest.NewURL(t)

	code, log := serveWithProfile(t, "-db", url,
		"-single-tenant-account-key", "acme-corp", bootstrapAbortFlag)
	if code != exitFailure {
		t.Fatalf("runServe exit = %d, want %d (startup aborts at the invalid review-resolution AFTER the bootstrap); log:\n%s", code, exitFailure, log)
	}

	pool, err := pgxpool.New(context.Background(), url)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	defer pool.Close()
	var granularity, provider string
	var role *string
	if err := pool.QueryRow(context.Background(),
		`SELECT provider, granularity, auto_join_role FROM accounts WHERE account_key = $1`, "acme-corp",
	).Scan(&provider, &granularity, &role); err != nil {
		t.Fatalf("runServe did not bootstrap the account row: %v; log:\n%s", err, log)
	}
	if provider != account.DefaultSingleTenantProvider || granularity != account.DefaultSingleTenantGranularity {
		t.Errorf("(provider, granularity) = (%q, %q), want the internal defaults (%q, %q)",
			provider, granularity, account.DefaultSingleTenantProvider, account.DefaultSingleTenantGranularity)
	}
	if role == nil || *role != account.DefaultSingleTenantAutoJoinRole {
		t.Errorf("auto_join_role = %v, want the internal default %q (a NULL role admits nobody)",
			role, account.DefaultSingleTenantAutoJoinRole)
	}
}

// (a) Nothing set: runServe writes NO accounts row — the hosted multi-tenant
// path is unchanged.
func TestServe_NoSingleTenantProfile_WritesNoAccount(t *testing.T) {
	url := pgtest.NewURL(t)

	code, log := serveWithProfile(t, "-db", url, bootstrapAbortFlag)
	if code != exitFailure {
		t.Fatalf("runServe exit = %d, want %d; log:\n%s", code, exitFailure, log)
	}

	pool, err := pgxpool.New(context.Background(), url)
	if err != nil {
		t.Fatalf("pool: %v", err)
	}
	defer pool.Close()
	var n int
	if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM accounts`).Scan(&n); err != nil {
		t.Fatalf("count accounts: %v", err)
	}
	if n != 0 {
		t.Errorf("accounts rows = %d, want 0 — an unconfigured deployment must not bootstrap anything", n)
	}
}

// (c) A single-tenant field set with the account key EMPTY fails startup with
// a message naming the missing flag — the partial-configuration path, which
// must never degrade to hosted mode.
func TestServe_SingleTenantPartialConfig_FailsStartup(t *testing.T) {
	url := pgtest.NewURL(t)

	for _, tc := range []struct {
		name string
		flag []string
	}{
		{"granularity without a key", []string{"-single-tenant-granularity", "organization"}},
		{"auto-join role without a key", []string{"-single-tenant-auto-join-role", "admin"}},
		{"provider without a key", []string{"-single-tenant-provider", "gitlab"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			code, log := serveWithProfile(t, append([]string{"-db", url}, tc.flag...)...)
			if code != exitFailure {
				t.Fatalf("runServe exit = %d, want %d; log:\n%s", code, exitFailure, log)
			}
			if !strings.Contains(log, "--single-tenant-account-key") {
				t.Errorf("log does not name the missing --single-tenant-account-key:\n%s", log)
			}
			if n := countAccounts(t, url, ""); n != 0 {
				t.Errorf("accounts rows = %d, want 0", n)
			}
		})
	}
}

// A configured profile with NO database fails startup rather than booting a
// deployment whose bootstrap silently did not happen.
func TestServe_SingleTenantConfiguredWithoutPool(t *testing.T) {
	code, log := serveWithProfile(t, "-single-tenant-account-key", "acme-corp")
	if code != exitFailure {
		t.Fatalf("runServe exit = %d, want %d; log:\n%s", code, exitFailure, log)
	}
	if !strings.Contains(log, "FISHHAWKD_DATABASE_URL") {
		t.Errorf("log does not name the missing database:\n%s", log)
	}
}

// An invalid granularity fails startup with an actionable message rather than
// a raw SQLSTATE 23514 from the accounts_granularity_check constraint.
func TestServe_SingleTenantInvalidGranularity_FailsStartup(t *testing.T) {
	url := pgtest.NewURL(t)

	code, log := serveWithProfile(t, "-db", url,
		"-single-tenant-account-key", "acme-corp",
		"-single-tenant-granularity", "team")
	if code != exitFailure {
		t.Fatalf("runServe exit = %d, want %d; log:\n%s", code, exitFailure, log)
	}
	if !strings.Contains(log, "--single-tenant-granularity") {
		t.Errorf("log does not name the offending flag:\n%s", log)
	}
	if n := countAccounts(t, url, "acme-corp"); n != 0 {
		t.Errorf("accounts rows = %d, want 0 — validation must precede the write", n)
	}
}

// ---- repo-ACL mirror wiring (ADR-057 Amendment A2, E44.10 / #2071) --------

// stubRepoACLStore is a repoacl.Store that is never actually queried by these
// wiring tests — they assert construction and gating, not mirror behavior.
type stubRepoACLStore struct{}

func (stubRepoACLStore) Get(context.Context, string, string, string) (repoacl.Entry, bool, error) {
	return repoacl.Entry{}, false, nil
}

func (stubRepoACLStore) Upsert(context.Context, string, string, string, identity.Permission, int64) error {
	return nil
}

func (stubRepoACLStore) EnsurePurgeGeneration(context.Context, string, string) (int64, error) {
	return 0, nil
}

func (stubRepoACLStore) BumpPurgeWatermark(context.Context, string, string) error { return nil }

func (stubRepoACLStore) DeleteForSubject(context.Context, string, string) error { return nil }

// stubPermissionResolver stands in for a configured IdentityProvider.
type stubPermissionResolver struct{}

func (stubPermissionResolver) PermissionLevel(context.Context, string, string) (identity.Permission, error) {
	return identity.PermissionRead, nil
}

// TestResolveRepoVisibility_BothRequired pins the gating contract: the mirror
// is constructed only when a store AND a permission resolver are both present.
// Anything less leaves the seam nil, which is server.Config's untenanted-allow
// posture — the pre-#2071 read surface, not a deny-all.
func TestResolveRepoVisibility_BothRequired(t *testing.T) {
	cases := []struct {
		name     string
		store    repoacl.Store
		resolver repoacl.PermissionResolver
		wantNil  bool
	}{
		{name: "both present", store: stubRepoACLStore{}, resolver: stubPermissionResolver{}},
		{name: "no store (no database)", resolver: stubPermissionResolver{}, wantNil: true},
		{name: "no resolver (no identity provider)", store: stubRepoACLStore{}, wantNil: true},
		{name: "neither", wantNil: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := resolveRepoVisibility(tc.store, tc.resolver, 0, slog.New(slog.NewTextHandler(io.Discard, nil)))
			if tc.wantNil {
				// Must be an UNTYPED nil interface. A typed (*Mirror)(nil)
				// would pass a `!= nil` check in the server and then report
				// ErrNotConfigured on every call, turning the unwired posture
				// into a 503 on every read.
				if got != nil {
					t.Fatalf("resolveRepoVisibility = %#v, want a nil interface", got)
				}
				return
			}
			if got == nil {
				t.Fatal("resolveRepoVisibility = nil, want a constructed mirror")
			}
			if _, ok := got.(*repoacl.Mirror); !ok {
				t.Errorf("mirror = %T, want *repoacl.Mirror", got)
			}
		})
	}
}

// TestResolveRepoVisibility_ShipsDefaultTTL is the DONE-MEANS behavioral test.
// The shipped TTL is a config value the compiler cannot enforce, so this
// asserts the value the constructed mirror actually carries: unset →
// repoacl.DefaultTTL, set → the operator's value. A comment-only touch of the
// wiring fails here where a scope-presence check would pass.
func TestResolveRepoVisibility_ShipsDefaultTTL(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	// The flag's default is what an unset FISHHAWKD_REPO_ACL_TTL resolves to.
	t.Setenv("FISHHAWKD_REPO_ACL_TTL", "")
	if got := envOrDuration("FISHHAWKD_REPO_ACL_TTL", repoacl.DefaultTTL); got != repoacl.DefaultTTL {
		t.Fatalf("unset env TTL = %s, want the shipped default %s", got, repoacl.DefaultTTL)
	}
	shipped := resolveRepoVisibility(stubRepoACLStore{}, stubPermissionResolver{},
		envOrDuration("FISHHAWKD_REPO_ACL_TTL", repoacl.DefaultTTL), logger)
	if got := shipped.(*repoacl.Mirror).TTL(); got != repoacl.DefaultTTL {
		t.Errorf("shipped mirror TTL = %s, want repoacl.DefaultTTL (%s)", got, repoacl.DefaultTTL)
	}

	// A configured value overrides it end to end.
	t.Setenv("FISHHAWKD_REPO_ACL_TTL", "90s")
	overridden := resolveRepoVisibility(stubRepoACLStore{}, stubPermissionResolver{},
		envOrDuration("FISHHAWKD_REPO_ACL_TTL", repoacl.DefaultTTL), logger)
	if got := overridden.(*repoacl.Mirror).TTL(); got != 90*time.Second {
		t.Errorf("overridden mirror TTL = %s, want 90s", got)
	}
}

// TestServeRegistersRepoACLTTLFlag pins the operator-facing surface: the flag
// exists, is documented, and defaults to the shipped TTL.
//
// It inspects the flag set runServe ITSELF builds, by driving runServe with
// -h: flag.ContinueOnError makes Parse print the usage of the real flag set to
// the log sink and return ErrHelp, before any listener is bound. So renaming,
// removing, or re-defaulting --repo-acl-ttl fails this test — which the
// earlier hand-rolled FlagSet mirror of the registration could not do.
func TestServeRegistersRepoACLTTLFlag(t *testing.T) {
	t.Setenv("FISHHAWKD_REPO_ACL_TTL", "")
	var logSink bytes.Buffer
	if code := runServe([]string{"-h"}, &logSink); code != exitFailure {
		t.Fatalf("runServe(-h) = %d, want %d (parse aborts before serving)", code, exitFailure)
	}
	usage := logSink.String()
	// PrintDefaults emits "  -<name> <type>\n    \t<usage> (default <v>)\n",
	// so slice from this flag's header to the next flag's to assert against
	// THIS flag's entry rather than anywhere in the whole usage block.
	const header = "  -repo-acl-ttl duration\n"
	i := strings.Index(usage, header)
	if i < 0 {
		t.Fatalf("runServe usage does not register --repo-acl-ttl as a duration flag; got:\n%s", usage)
	}
	entry := usage[i+len(header):]
	if j := strings.Index(entry, "\n  -"); j >= 0 {
		entry = entry[:j]
	}
	if want := "(default " + repoacl.DefaultTTL.String() + ")"; !strings.Contains(entry, want) {
		t.Errorf("--repo-acl-ttl entry = %q, want it to carry %s", entry, want)
	}
	if !strings.Contains(entry, "ADR-057 Amendment A2") {
		t.Errorf("--repo-acl-ttl usage text lost its operator-facing documentation; got %q", entry)
	}
}
