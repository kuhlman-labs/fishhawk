package main

import (
	"context"
	"errors"
	"flag"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/kuhlman-labs/fishhawk/backend/internal/audit"
	authpkg "github.com/kuhlman-labs/fishhawk/backend/internal/auth"
	"github.com/kuhlman-labs/fishhawk/backend/internal/campaign"
	"github.com/kuhlman-labs/fishhawk/backend/internal/forge"
	"github.com/kuhlman-labs/fishhawk/backend/internal/githubapp"
	"github.com/kuhlman-labs/fishhawk/backend/internal/githubclient"
	"github.com/kuhlman-labs/fishhawk/backend/internal/identity"
	"github.com/kuhlman-labs/fishhawk/backend/internal/issuecomment"
	"github.com/kuhlman-labs/fishhawk/backend/internal/modeloracle"
	"github.com/kuhlman-labs/fishhawk/backend/internal/operatorrole"
	"github.com/kuhlman-labs/fishhawk/backend/internal/reviewresolver"
	runpkg "github.com/kuhlman-labs/fishhawk/backend/internal/run"
	"github.com/kuhlman-labs/fishhawk/backend/internal/server"
	"github.com/kuhlman-labs/fishhawk/backend/internal/webhook"
	"github.com/kuhlman-labs/fishhawk/backend/internal/workmgmt"
	workmgmtgitlab "github.com/kuhlman-labs/fishhawk/backend/internal/workmgmt/gitlab"
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
	loader := buildRepoConventionsLoader(srv, nil, func() (workmgmt.Conventions, bool) { return override, true })
	conv, err := loader.Load(context.Background(), "acme/widgets")
	if err != nil {
		t.Fatalf("Load err = %v, want nil", err)
	}
	if conv.Provider != workmgmtgitlab.ProviderName {
		t.Errorf("Provider = %q, want %q (the break-glass override must be the fallback)", conv.Provider, workmgmtgitlab.ProviderName)
	}

	noOverride := buildRepoConventionsLoader(srv, nil, nil)
	conv, err = noOverride.Load(context.Background(), "acme/widgets")
	if err != nil {
		t.Fatalf("Load (no override) err = %v, want nil", err)
	}
	if conv.Provider != workmgmt.Default().Provider {
		t.Errorf("Provider = %q, want the Default() provider %q", conv.Provider, workmgmt.Default().Provider)
	}
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

// TestResolveRegionInference covers the region-scoped inference resolver
// (ADR-062 / E44.7): the unregionalized default, the regionalized happy path,
// and each fail-closed branch. A regional cell must never boot without its
// in-region model endpoint and reviewer key.
func TestResolveRegionInference(t *testing.T) {
	tests := []struct {
		name        string
		homeRegion  string
		baseURL     string
		apiKey      string
		wantErr     string
		wantRegion  string
		wantBaseURL string
	}{
		{
			name: "unregionalized deployment: nothing required",
		},
		{
			name:        "unregionalized deployment may still override the endpoint",
			baseURL:     "https://api.anthropic.com",
			wantBaseURL: "https://api.anthropic.com",
		},
		{
			name:        "regional cell with endpoint and key",
			homeRegion:  " EU ",
			baseURL:     "https://eu.api.example.com",
			apiKey:      "sk-test",
			wantRegion:  "eu",
			wantBaseURL: "https://eu.api.example.com",
		},
		{
			name:       "fail closed: home region without a model endpoint",
			homeRegion: "eu",
			apiKey:     "sk-test",
			wantErr:    "FISHHAWKD_ANTHROPIC_BASE_URL",
		},
		{
			name:       "fail closed: home region without a reviewer key",
			homeRegion: "eu",
			baseURL:    "https://eu.api.example.com",
			wantErr:    "FISHHAWKD_ANTHROPIC_API_KEY",
		},
		{
			name:    "fail closed: endpoint is not an absolute http(s) URL",
			baseURL: "eu.api.example.com",
			wantErr: "not an absolute http(s) URL",
		},
		{
			name:       "fail closed: endpoint scheme is not http(s)",
			homeRegion: "eu",
			baseURL:    "ftp://eu.api.example.com",
			apiKey:     "sk-test",
			wantErr:    "not an absolute http(s) URL",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := resolveRegionInference(tc.homeRegion, tc.baseURL, tc.apiKey)
			if tc.wantErr != "" {
				if err == nil {
					t.Fatalf("resolveRegionInference = %+v, want error containing %q", got, tc.wantErr)
				}
				if !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("error = %q, want it to contain %q", err, tc.wantErr)
				}
				if got != (regionInference{}) {
					t.Errorf("on error got %+v, want the zero regionInference", got)
				}
				return
			}
			if err != nil {
				t.Fatalf("resolveRegionInference: %v", err)
			}
			if got.homeRegion != tc.wantRegion {
				t.Errorf("homeRegion = %q, want %q", got.homeRegion, tc.wantRegion)
			}
			if got.anthropicBaseURL != tc.wantBaseURL {
				t.Errorf("anthropicBaseURL = %q, want %q", got.anthropicBaseURL, tc.wantBaseURL)
			}
		})
	}
}

// TestRunServe_RegionalCellWithoutEndpointFailsStartup pins the fail-closed
// branch at the STARTUP boundary, not just in the pure resolver: runServe
// returns a failure exit code (never binding a listener) when FISHHAWKD_HOME_REGION
// is set without its in-region model endpoint.
func TestRunServe_RegionalCellWithoutEndpointFailsStartup(t *testing.T) {
	var logs strings.Builder
	code := runServe([]string{
		"--home-region", "eu",
		"--anthropic-base-url", "",
		"--anthropic-api-key", "sk-test",
		"--operator-min-permission", "write",
	}, &logs)
	if code == 0 {
		t.Fatalf("runServe exit code = 0, want non-zero (regional cell without a model endpoint must fail closed)")
	}
	if !strings.Contains(logs.String(), "region-scoped inference config invalid") {
		t.Errorf("logs = %q, want them to name the region-scoped inference failure", logs.String())
	}
}

// TestPlanReviewerSet_AnthropicTargetsRegionalEndpoint is the cross-boundary
// wiring assertion: the base URL resolved in serve.go reaches the reviewer's
// live Messages client, so a review request actually leaves for the in-region
// endpoint. Asserted by driving a real Review against an httptest server that
// stands in for the regional endpoint.
func TestPlanReviewerSet_AnthropicTargetsRegionalEndpoint(t *testing.T) {
	hits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits++
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"msg_test","type":"message","role":"assistant","model":"claude-sonnet-4-6","stop_reason":"end_turn","content":[{"type":"text","text":"{\"verdict\":\"approve\"}"}],"usage":{"input_tokens":1,"output_tokens":1}}`))
	}))
	defer srv.Close()

	set := &planReviewerSet{opts: planReviewerOptions{
		anthropicAPIKey:     "sk-test",
		anthropicBaseURL:    srv.URL,
		planReviewModel:     "claude-sonnet-4-6",
		planReviewMaxTokens: 1024,
		planReviewTimeout:   10 * time.Second,
	}}
	reviewer := set.Default()
	if reviewer == nil {
		t.Fatal("Default() = nil, want the anthropic adapter")
	}
	if _, _, err := reviewer.Review(context.Background(), "preamble"); err != nil {
		t.Fatalf("Review: %v", err)
	}
	if hits != 1 {
		t.Fatalf("regional endpoint hits = %d, want 1 (anthropicBaseURL was not threaded to the Messages client)", hits)
	}
}
