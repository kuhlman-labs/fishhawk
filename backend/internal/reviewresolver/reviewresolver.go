// Package reviewresolver is the provider seam around a run's review-gate
// resolution (ADR-031 Phase 2). The merge-status reconciler resolves a
// review-gated run on a VERIFIED PR merge state; today the only resolution
// strategy is github_merge — read the live PR via the GitHub REST API and
// advance the gate through the shared webhook+poll path. This package extracts
// that strategy behind a named-provider registry so a deployment can select
// the resolution provider by config (review.resolution) without the
// reconciler hard-coding *server.Server.
//
// The shape mirrors the workmgmt provider registry (backend/internal/workmgmt):
// a Resolver interface keyed by Name(), a sync.RWMutex-guarded registry
// (Register/Get/Registered), and a Select helper that defaults the empty
// config string to github_merge and fails closed on an unknown name. The
// fail-closed Select is the ADR-031 guarantee: a misconfigured resolver must
// fail startup, never silently default to github_merge and mask a deployment
// error (succeeded must always mean a verified GitHub merge).
//
// The Func adapter wraps an arbitrary resolve-signature func as a named
// provider. This lets the github_merge logic be registered from
// cmd/fishhawkd/serve.go as a thin wrapper over srv.ResolveReviewFromPollState
// with NO import of the server package here — avoiding an import cycle. The
// Resolver method signature is identical to mergereconciler.Resolver, so any
// Resolver structurally satisfies mergereconciler.Ticker.Resolver with no
// change to that package.
package reviewresolver

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/google/uuid"
)

// DefaultResolution is the review-gate resolution provider id selected when
// the deployment leaves review.resolution empty. It is the github_merge
// strategy: resolve the gate only on a verified GitHub merge, exactly as the
// pre-seam reconciler did.
const DefaultResolution = "github_merge"

// Resolver resolves a run's review stage from a poll's terminal PR state,
// routing through the same path the pull_request.closed webhook uses. The
// method signature is identical to mergereconciler.Resolver, so a Resolver
// structurally satisfies mergereconciler.Ticker.Resolver. Concrete providers
// are registered at startup; the deployment selects one by id via Select.
type Resolver interface {
	Name() string
	ResolveReviewFromPollState(ctx context.Context, runID uuid.UUID, merged bool, prURL string) error
}

// UnknownResolverError is returned by Get/Select when no provider is
// registered for the requested id. It is the fail-closed path for a config
// typo or an unimplemented provider: the error names the missing id and the
// registered set rather than silently defaulting (ADR-031 — a misconfigured
// resolver must fail startup, not mask a deployment error).
type UnknownResolverError struct {
	ID    string
	Known []string
}

func (e *UnknownResolverError) Error() string {
	if len(e.Known) == 0 {
		return fmt.Sprintf("reviewresolver: no review resolver registered for %q (no resolvers registered)", e.ID)
	}
	return fmt.Sprintf("reviewresolver: no review resolver registered for %q; registered resolvers: %s",
		e.ID, strings.Join(e.Known, ", "))
}

var (
	registryMu sync.RWMutex
	registry   = map[string]Resolver{}
)

// Register adds r to the global resolver registry under r.Name(), replacing
// any prior registration for that id. The server wires the concrete
// github_merge provider at startup; tests register fakes.
func Register(r Resolver) {
	registryMu.Lock()
	defer registryMu.Unlock()
	registry[r.Name()] = r
}

// Get returns the registered resolver for id, or an *UnknownResolverError
// naming id and the registered set. Callers MUST surface this error rather
// than dispatching against a nil resolver.
func Get(id string) (Resolver, error) {
	registryMu.RLock()
	defer registryMu.RUnlock()
	r, ok := registry[id]
	if !ok {
		return nil, &UnknownResolverError{ID: id, Known: knownIDsLocked()}
	}
	return r, nil
}

// Registered returns the sorted set of registered resolver ids — used by
// startup logging and the unknown-resolver error.
func Registered() []string {
	registryMu.RLock()
	defer registryMu.RUnlock()
	return knownIDsLocked()
}

// Select resolves the deployment-configured review.resolution name to a
// registered provider. An empty name defaults to DefaultResolution
// (github_merge); any name is then looked up via Get, so an unknown value
// returns an *UnknownResolverError (fail closed — no silent fallback).
func Select(name string) (Resolver, error) {
	if name == "" {
		name = DefaultResolution
	}
	return Get(name)
}

// knownIDsLocked returns the sorted registry keys. Callers hold registryMu
// (read or write).
func knownIDsLocked() []string {
	ids := make([]string, 0, len(registry))
	for id := range registry {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}

// funcResolver adapts an arbitrary resolve-signature func into a named
// Resolver. It lets serve.go register the github_merge provider as a thin
// wrapper over srv.ResolveReviewFromPollState without this package importing
// the server package (avoiding an import cycle).
type funcResolver struct {
	name string
	fn   func(ctx context.Context, runID uuid.UUID, merged bool, prURL string) error
}

func (f funcResolver) Name() string { return f.name }

func (f funcResolver) ResolveReviewFromPollState(ctx context.Context, runID uuid.UUID, merged bool, prURL string) error {
	return f.fn(ctx, runID, merged, prURL)
}

// Func wraps fn as a Resolver named name. The returned provider forwards
// every ResolveReviewFromPollState call straight to fn, so registering
// Func(DefaultResolution, srv.ResolveReviewFromPollState) preserves the
// github_merge default path byte-for-byte.
func Func(name string, fn func(ctx context.Context, runID uuid.UUID, merged bool, prURL string) error) Resolver {
	return funcResolver{name: name, fn: fn}
}
