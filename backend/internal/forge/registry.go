package forge

import (
	"fmt"
	"sort"
	"strings"
	"sync"
)

// This file is the forge provider registry, deliberately a copy of the
// proven workmgmt provider registry (workmgmt/provider.go) rather than a
// new design: same global-map-under-RWMutex shape, same fail-closed Get,
// same sorted Registered for startup logging. Two registries with the
// same semantics and the same code shape are cheaper to reason about
// than one clever abstraction over both.

// UnknownForgeError is returned by Get when no forge is registered for
// the requested id. It is the fail-closed path for an unimplemented
// forge (e.g. gitlab, which is interface-only until ADR-058 lands its
// implementation) or a config typo: the error names the missing id and
// the registered set rather than panicking on a nil dispatch.
type UnknownForgeError struct {
	ID    string
	Known []string
}

func (e *UnknownForgeError) Error() string {
	if len(e.Known) == 0 {
		return fmt.Sprintf("forge: no forge registered for %q (no forges registered)", e.ID)
	}
	return fmt.Sprintf("forge: no forge registered for %q; registered forges: %s",
		e.ID, strings.Join(e.Known, ", "))
}

var (
	registryMu sync.RWMutex
	registry   = map[string]Forge{}
)

// Register adds f to the global forge registry under f.Name(),
// replacing any prior registration for that id. The server wires the
// concrete forges (today: forge/github) at startup; tests register
// fakes.
func Register(f Forge) {
	registryMu.Lock()
	defer registryMu.Unlock()
	registry[f.Name()] = f
}

// Get returns the registered forge for id, or an *UnknownForgeError
// naming id and the registered set. Callers MUST surface this error
// rather than dispatching against a nil forge.
func Get(id string) (Forge, error) {
	registryMu.RLock()
	defer registryMu.RUnlock()
	f, ok := registry[id]
	if !ok {
		return nil, &UnknownForgeError{ID: id, Known: knownIDsLocked()}
	}
	return f, nil
}

// Registered returns the sorted set of registered forge ids — used by
// startup logging and the unknown-forge error.
func Registered() []string {
	registryMu.RLock()
	defer registryMu.RUnlock()
	return knownIDsLocked()
}

// knownIDsLocked returns the sorted registry keys. Callers hold
// registryMu (read or write).
func knownIDsLocked() []string {
	ids := make([]string, 0, len(registry))
	for id := range registry {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids
}
