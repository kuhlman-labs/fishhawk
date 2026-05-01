package tracestore_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/google/uuid"
	"github.com/testcontainers/testcontainers-go"
	tcminio "github.com/testcontainers/testcontainers-go/modules/minio"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/kuhlman-labs/fishhawk/backend/internal/tracestore"
)

// startMinIO spins up a MinIO container, configures an S3 client
// against it, and creates a fresh bucket. Skips the test if Docker
// isn't reachable.
func startMinIO(t *testing.T) (*s3.Client, string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	const (
		user = "fishhawk-test"
		pass = "fishhawk-test-secret-key"
	)
	c, err := tcminio.Run(ctx,
		"minio/minio:RELEASE.2025-01-20T14-49-07Z",
		tcminio.WithUsername(user),
		tcminio.WithPassword(pass),
		testcontainers.WithWaitStrategy(
			wait.ForLog("API: ").WithStartupTimeout(60*time.Second),
		),
	)
	if err != nil {
		if isDockerUnavailable(err) {
			t.Skipf("Docker not available; skipping integration test: %v", err)
		}
		t.Fatalf("start minio: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_ = c.Terminate(ctx)
	})

	endpoint, err := c.ConnectionString(ctx)
	if err != nil {
		t.Fatalf("connection string: %v", err)
	}

	cfg, err := config.LoadDefaultConfig(ctx,
		config.WithRegion("us-east-1"),
		config.WithCredentialsProvider(
			credentials.NewStaticCredentialsProvider(user, pass, ""),
		),
	)
	if err != nil {
		t.Fatalf("aws config: %v", err)
	}
	client := s3.NewFromConfig(cfg, func(o *s3.Options) {
		o.BaseEndpoint = aws.String("http://" + endpoint)
		// MinIO requires path-style requests; AWS S3 also accepts
		// them, so this is the safe choice in either environment.
		o.UsePathStyle = true
	})

	// Per-test bucket name keeps parallel tests isolated, though
	// the bucket lifetime ends with the container.
	bucket := "fishhawk-test-" + strings.ReplaceAll(uuid.NewString(), "-", "")[:24]
	if _, err := client.CreateBucket(ctx, &s3.CreateBucketInput{
		Bucket: aws.String(bucket),
	}); err != nil {
		t.Fatalf("create bucket: %v", err)
	}

	return client, bucket
}

func isDockerUnavailable(err error) bool {
	if err == nil {
		return false
	}
	if os.Getenv("FISHHAWK_SKIP_INTEGRATION") != "" {
		return true
	}
	msg := strings.ToLower(err.Error())
	for _, marker := range []string{
		"cannot connect to the docker daemon",
		"docker: not found",
		"executable file not found",
		"dial unix /var/run/docker.sock",
	} {
		if strings.Contains(msg, marker) {
			return true
		}
	}
	return false
}

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// --- Pure unit tests of types ---

func TestVariant_Valid(t *testing.T) {
	cases := map[tracestore.Variant]bool{
		tracestore.VariantRaw:      true,
		tracestore.VariantRedacted: true,
		"unknown":                  false,
		"":                         false,
	}
	for v, want := range cases {
		if got := v.Valid(); got != want {
			t.Errorf("Variant(%q).Valid() = %v, want %v", v, got, want)
		}
	}
}

func TestBundleRef_Key(t *testing.T) {
	runID := uuid.MustParse("11111111-2222-3333-4444-555555555555")
	hash := strings.Repeat("a", 64)
	ref := tracestore.BundleRef{
		RunID:       runID,
		Variant:     tracestore.VariantRedacted,
		ContentHash: hash,
	}
	want := fmt.Sprintf("%s/redacted/%s.jsonl.gz", runID, hash)
	if got := ref.Key(); got != want {
		t.Errorf("Key() = %q, want %q", got, want)
	}
}

func TestBundleRef_Validate(t *testing.T) {
	good := tracestore.BundleRef{
		RunID:       uuid.New(),
		Variant:     tracestore.VariantRaw,
		ContentHash: strings.Repeat("a", 64),
	}
	if err := good.Validate(); err != nil {
		t.Errorf("good ref validates with err = %v", err)
	}

	cases := []struct {
		name string
		ref  tracestore.BundleRef
	}{
		{"zero RunID", tracestore.BundleRef{Variant: tracestore.VariantRaw, ContentHash: strings.Repeat("a", 64)}},
		{"bad Variant", tracestore.BundleRef{RunID: uuid.New(), Variant: "weird", ContentHash: strings.Repeat("a", 64)}},
		{"short ContentHash", tracestore.BundleRef{RunID: uuid.New(), Variant: tracestore.VariantRaw, ContentHash: "short"}},
		{"empty ContentHash", tracestore.BundleRef{RunID: uuid.New(), Variant: tracestore.VariantRaw, ContentHash: ""}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.ref.Validate(); err == nil {
				t.Errorf("expected error for %s; got none", tc.name)
			}
		})
	}
}

// --- MinIO-backed integration tests ---

func TestS3_PutAndGetRoundTrip(t *testing.T) {
	client, bucket := startMinIO(t)
	store := tracestore.NewS3Storage(client, bucket)

	body := []byte("dummy gzipped trace payload")
	ref := tracestore.BundleRef{
		RunID:       uuid.New(),
		Variant:     tracestore.VariantRaw,
		ContentHash: sha256Hex(body),
	}
	if err := store.Put(context.Background(), ref, bytes.NewReader(body)); err != nil {
		t.Fatalf("Put: %v", err)
	}

	rc, err := store.Get(context.Background(), ref)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	defer rc.Close()
	got, err := io.ReadAll(rc)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !bytes.Equal(got, body) {
		t.Errorf("round-trip mismatch")
	}
}

func TestS3_Get_NotFound(t *testing.T) {
	client, bucket := startMinIO(t)
	store := tracestore.NewS3Storage(client, bucket)

	ref := tracestore.BundleRef{
		RunID:       uuid.New(),
		Variant:     tracestore.VariantRedacted,
		ContentHash: sha256Hex([]byte("not stored")),
	}
	_, err := store.Get(context.Background(), ref)
	if !errors.Is(err, tracestore.ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

func TestS3_Stat_NotFound(t *testing.T) {
	client, bucket := startMinIO(t)
	store := tracestore.NewS3Storage(client, bucket)

	ref := tracestore.BundleRef{
		RunID:       uuid.New(),
		Variant:     tracestore.VariantRaw,
		ContentHash: sha256Hex([]byte("not stored")),
	}
	_, err := store.Stat(context.Background(), ref)
	if !errors.Is(err, tracestore.ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

func TestS3_StatReturnsSizeAndETag(t *testing.T) {
	client, bucket := startMinIO(t)
	store := tracestore.NewS3Storage(client, bucket)

	body := []byte("0123456789abcdef")
	ref := tracestore.BundleRef{
		RunID:       uuid.New(),
		Variant:     tracestore.VariantRaw,
		ContentHash: sha256Hex(body),
	}
	if err := store.Put(context.Background(), ref, bytes.NewReader(body)); err != nil {
		t.Fatalf("Put: %v", err)
	}
	stat, err := store.Stat(context.Background(), ref)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if stat.Size != int64(len(body)) {
		t.Errorf("Size = %d, want %d", stat.Size, len(body))
	}
	if stat.ETag == "" {
		t.Error("ETag should be set")
	}
	if stat.LastModified.IsZero() {
		t.Error("LastModified should be set")
	}
}

func TestS3_Put_DedupsIdenticalContent(t *testing.T) {
	// Putting byte-identical bundles a second time should be a no-op
	// at the API level: same key (content-addressed), same bytes,
	// Get returns the same content.
	client, bucket := startMinIO(t)
	store := tracestore.NewS3Storage(client, bucket)

	body := []byte("identical bytes")
	ref := tracestore.BundleRef{
		RunID:       uuid.New(),
		Variant:     tracestore.VariantRaw,
		ContentHash: sha256Hex(body),
	}
	for i := 0; i < 3; i++ {
		if err := store.Put(context.Background(), ref, bytes.NewReader(body)); err != nil {
			t.Fatalf("Put #%d: %v", i, err)
		}
	}
	rc, err := store.Get(context.Background(), ref)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	defer rc.Close()
	got, _ := io.ReadAll(rc)
	if !bytes.Equal(got, body) {
		t.Errorf("after dedup: content mismatch")
	}
}

func TestS3_ListReturnsBundlesUnderRun(t *testing.T) {
	client, bucket := startMinIO(t)
	store := tracestore.NewS3Storage(client, bucket)

	runID := uuid.New()
	otherRun := uuid.New()
	bundles := []struct {
		runID   uuid.UUID
		variant tracestore.Variant
		body    []byte
	}{
		{runID, tracestore.VariantRaw, []byte("run1 raw")},
		{runID, tracestore.VariantRedacted, []byte("run1 redacted")},
		{runID, tracestore.VariantRaw, []byte("run1 raw v2")},
		{otherRun, tracestore.VariantRaw, []byte("run2 raw")},
	}
	for _, b := range bundles {
		ref := tracestore.BundleRef{
			RunID:       b.runID,
			Variant:     b.variant,
			ContentHash: sha256Hex(b.body),
		}
		if err := store.Put(context.Background(), ref, bytes.NewReader(b.body)); err != nil {
			t.Fatalf("Put: %v", err)
		}
	}

	got, err := store.List(context.Background(), runID)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 3 {
		t.Errorf("got %d bundles for runID, want 3 (others should be filtered): %v", len(got), got)
	}
	for _, ref := range got {
		if ref.RunID != runID {
			t.Errorf("ref %v leaked from a different run", ref)
		}
	}
}

func TestS3_List_IgnoresForeignObjects(t *testing.T) {
	// An object whose key doesn't fit the canonical layout (e.g.
	// dropped there by an unrelated process) should be ignored by
	// List rather than returned as a half-broken BundleRef.
	client, bucket := startMinIO(t)
	store := tracestore.NewS3Storage(client, bucket)
	runID := uuid.New()

	body := []byte("legit bundle")
	canonical := tracestore.BundleRef{
		RunID:       runID,
		Variant:     tracestore.VariantRaw,
		ContentHash: sha256Hex(body),
	}
	if err := store.Put(context.Background(), canonical, bytes.NewReader(body)); err != nil {
		t.Fatalf("Put canonical: %v", err)
	}

	// Direct PUT under the same run prefix with a non-canonical key.
	foreignKey := runID.String() + "/something/else.txt"
	_, err := client.PutObject(context.Background(), &s3.PutObjectInput{
		Bucket: aws.String(bucket),
		Key:    aws.String(foreignKey),
		Body:   bytes.NewReader([]byte("foreign")),
	})
	if err != nil {
		t.Fatalf("put foreign: %v", err)
	}

	got, err := store.List(context.Background(), runID)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(got) != 1 {
		t.Errorf("List returned %d, want 1 (foreign should be filtered): %v", len(got), got)
	}
}

func TestS3_List_ZeroRunIDRejected(t *testing.T) {
	client, bucket := startMinIO(t)
	store := tracestore.NewS3Storage(client, bucket)

	if _, err := store.List(context.Background(), uuid.Nil); err == nil {
		t.Fatal("List with zero RunID should error")
	}
}

func TestS3_Operations_RejectInvalidRef(t *testing.T) {
	client, bucket := startMinIO(t)
	store := tracestore.NewS3Storage(client, bucket)

	bad := tracestore.BundleRef{} // all-zero, fails Validate

	if err := store.Put(context.Background(), bad, bytes.NewReader([]byte{})); err == nil {
		t.Error("Put should reject bad ref")
	}
	if _, err := store.Get(context.Background(), bad); err == nil {
		t.Error("Get should reject bad ref")
	}
	if _, err := store.Stat(context.Background(), bad); err == nil {
		t.Error("Stat should reject bad ref")
	}
}
