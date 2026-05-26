package tracestore

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
)

// MemStorage is an in-memory trace bundle store. All operations are
// mutex-guarded; safe for concurrent use from multiple goroutines
// (e.g. an httptest.Server handling parallel raw + redacted uploads).
// Intended for tests that cannot spin a MinIO container.
type MemStorage struct {
	mu   sync.Mutex
	data map[string][]byte
}

// NewMem returns a fresh empty MemStorage.
func NewMem() *MemStorage {
	return &MemStorage{data: make(map[string][]byte)}
}

// Put stores the bundle bytes at ref's canonical key. Idempotent:
// re-uploading identical bytes is a no-op at the storage layer.
func (m *MemStorage) Put(_ context.Context, ref BundleRef, body io.Reader) error {
	if err := ref.Validate(); err != nil {
		return err
	}
	b, err := io.ReadAll(body)
	if err != nil {
		return fmt.Errorf("tracestore/mem: read body: %w", err)
	}
	m.mu.Lock()
	m.data[ref.Key()] = b
	m.mu.Unlock()
	return nil
}

// Get returns a reader for the bundle at ref's canonical key.
// Returns ErrNotFound when the key is absent.
func (m *MemStorage) Get(_ context.Context, ref BundleRef) (io.ReadCloser, error) {
	if err := ref.Validate(); err != nil {
		return nil, err
	}
	m.mu.Lock()
	b, ok := m.data[ref.Key()]
	m.mu.Unlock()
	if !ok {
		return nil, ErrNotFound
	}
	return io.NopCloser(bytes.NewReader(b)), nil
}

// Stat returns metadata for the bundle at ref's canonical key.
// Returns ErrNotFound when the key is absent.
func (m *MemStorage) Stat(_ context.Context, ref BundleRef) (Stat, error) {
	if err := ref.Validate(); err != nil {
		return Stat{}, err
	}
	m.mu.Lock()
	b, ok := m.data[ref.Key()]
	m.mu.Unlock()
	if !ok {
		return Stat{}, ErrNotFound
	}
	return Stat{
		Size:         int64(len(b)),
		ETag:         ref.ContentHash,
		LastModified: time.Time{},
	}, nil
}

// List returns all bundle refs under runID's key prefix, sorted by
// key for stable ordering across calls.
func (m *MemStorage) List(_ context.Context, runID uuid.UUID) ([]BundleRef, error) {
	prefix := runID.String() + "/"
	m.mu.Lock()
	var refs []BundleRef
	for k := range m.data {
		if !strings.HasPrefix(k, prefix) {
			continue
		}
		// Key layout: {run_id}/{variant}/{sha256}.jsonl.gz
		rest := k[len(prefix):]
		slash := strings.IndexByte(rest, '/')
		if slash < 0 {
			continue
		}
		variant := Variant(rest[:slash])
		hashAndSuffix := rest[slash+1:]
		hash := strings.TrimSuffix(hashAndSuffix, ".jsonl.gz")
		refs = append(refs, BundleRef{RunID: runID, Variant: variant, ContentHash: hash})
	}
	m.mu.Unlock()
	sort.Slice(refs, func(i, j int) bool {
		return refs[i].Key() < refs[j].Key()
	})
	return refs, nil
}
