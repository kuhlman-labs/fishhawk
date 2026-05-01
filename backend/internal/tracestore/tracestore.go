// Package tracestore persists agent trace bundles to an S3-compatible
// object store (E2.2 / #23). Per ADR-003 (#67) production runs against
// AWS S3; local dev and tests run against MinIO with the same code
// path — the SDK is identical, only the endpoint and credentials
// differ.
//
// Bundles are content-addressed: the key embeds the sha256 of the
// (already gzipped) payload bytes, which gives free dedup on
// re-uploads of identical content (e.g. retried trace shipping). The
// raw / redacted variant split mirrors the access-control split from
// MVP_SPEC §4.4 — redacted is broadly readable, raw is gated by a
// stricter policy on the bucket.
//
// Key layout: {run_id}/{variant}/{sha256}.jsonl.gz
package tracestore

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/google/uuid"
)

// Variant is whether the bundle is the raw trace as written or the
// post-redaction view that's safe to expose more broadly.
type Variant string

// Variants per ADR-003 (#67).
const (
	VariantRaw      Variant = "raw"
	VariantRedacted Variant = "redacted"
)

// Valid reports whether v is one of the closed-set values.
func (v Variant) Valid() bool {
	return v == VariantRaw || v == VariantRedacted
}

// BundleRef identifies a trace bundle in the store.
type BundleRef struct {
	// RunID scopes the bundle to a single workflow run.
	RunID uuid.UUID
	// Variant separates raw from redacted within a run.
	Variant Variant
	// ContentHash is hex-encoded sha256 of the bundle bytes (the
	// gzipped JSON Lines payload, ADR-007). Acts as the storage
	// dedup key — two callers with byte-identical bundles produce
	// the same key, so a re-upload is a no-op.
	ContentHash string
}

// Key returns the canonical object key for the bundle.
//
//	{run_id}/{variant}/{sha256}.jsonl.gz
func (b BundleRef) Key() string {
	return fmt.Sprintf("%s/%s/%s.jsonl.gz", b.RunID, b.Variant, b.ContentHash)
}

// Validate performs sanity checks before serializing the ref into
// a key. Returns a wrapped error for bad inputs so callers don't
// trip into surprising S3 PUT errors.
func (b BundleRef) Validate() error {
	if b.RunID == uuid.Nil {
		return fmt.Errorf("tracestore: BundleRef.RunID is the zero UUID")
	}
	if !b.Variant.Valid() {
		return fmt.Errorf("tracestore: BundleRef.Variant=%q is not raw or redacted", b.Variant)
	}
	// sha256 in lowercase hex is exactly 64 chars; the runner is
	// expected to produce that format.
	if len(b.ContentHash) != 64 {
		return fmt.Errorf("tracestore: BundleRef.ContentHash length=%d, want 64 (hex sha256)", len(b.ContentHash))
	}
	return nil
}

// Stat is the metadata returned by Storage.Stat. Size and ETag are
// useful for integrity-check workflows and for the audit log's
// reference back to the bundle.
type Stat struct {
	Size         int64
	ETag         string
	LastModified time.Time
}

// Storage is the abstraction over the trace bundle store. The S3-
// backed implementation works against AWS S3 in production and
// MinIO in local dev / tests — same code, different endpoint.
type Storage interface {
	// Put writes the bundle bytes at the canonical key. Idempotent
	// at the storage layer: writing identical bytes a second time
	// is a no-op and Get returns the same object. Body is consumed
	// fully; callers should not reuse the reader after Put returns.
	Put(ctx context.Context, ref BundleRef, body io.Reader) error

	// Get returns a reader over the stored bundle bytes. Callers
	// must Close the returned reader. Returns ErrNotFound if no
	// object exists at the canonical key.
	Get(ctx context.Context, ref BundleRef) (io.ReadCloser, error)

	// Stat returns metadata for the bundle without downloading
	// bytes. Returns ErrNotFound if no object exists.
	Stat(ctx context.Context, ref BundleRef) (Stat, error)

	// List returns every bundle under a run prefix, ordered by key
	// (which is variant-then-hash and therefore stable). Used by
	// the audit-log export (E9) and the run-detail UI to enumerate
	// the bundles a run produced.
	List(ctx context.Context, runID uuid.UUID) ([]BundleRef, error)
}

// ErrNotFound indicates the requested bundle does not exist.
var ErrNotFound = errors.New("tracestore: bundle not found")
