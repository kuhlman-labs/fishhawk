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
	"sync"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/containerd/errdefs"
	"github.com/google/uuid"
	"github.com/testcontainers/testcontainers-go"
	tcminio "github.com/testcontainers/testcontainers-go/modules/minio"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/kuhlman-labs/fishhawk/backend/internal/tracestore"
)

const (
	minioUser  = "fishhawk-test"
	minioPass  = "fishhawk-test-secret-key"
	minioImage = "minio/minio:RELEASE.2025-01-20T14-49-07Z"
	minioPort  = "9000/tcp"
)

// minioRunOptions returns the container customizers used to start the shared
// MinIO container. It is pure and needs no Docker, so the shipped wait-strategy
// configuration is assertable in TestMinioRunOptions_IncludeAPortResolvingWait.
//
// The load-bearing choice is WithAdditionalWaitStrategy over WithWaitStrategy:
// the minio module's Run sets a default wait.ForHTTP("/minio/health/live")
// .WithPort("9000") and then applies these opts. WithWaitStrategy REPLACES that
// default (options.go WithWaitStrategyAndDeadline assigns req.WaitingFor
// outright); WithAdditionalWaitStrategy APPENDS (it prepends the existing
// req.WaitingFor). A log-only wait (wait.ForLog) never resolves the published
// port mapping, so ConnectionString = Host()+MappedPort() can fail immediately
// on Docker Desktop, where the published port is served by an asynchronously
// started proxy. Both the retained ForHTTP default and the added
// ForListeningPort retry MappedPort until the mapping resolves; ForListeningPort
// additionally dials host:port, proving the proxy is serving. SkipInternalCheck
// drops the in-container /bin/sh probe so the strategy depends only on the
// host-side dial. See #2948.
func minioRunOptions() []testcontainers.ContainerCustomizer {
	return []testcontainers.ContainerCustomizer{
		tcminio.WithUsername(minioUser),
		tcminio.WithPassword(minioPass),
		testcontainers.WithAdditionalWaitStrategy(
			wait.ForListeningPort(minioPort).SkipInternalCheck(),
		),
	}
}

// sharedMinIO is process-scoped MinIO state started at most once per package
// process (not the cross-process pgtest reuse — this needs no lease bookkeeping
// and is unaffected by TESTCONTAINERS_RYUK_DISABLED because TestMain terminates
// it deterministically). Collapsing the suite's per-test containers to one
// removes the daemon pressure that opens the port-mapping race.
type sharedMinIOState struct {
	container *tcminio.MinioContainer
	endpoint  string
	err       error
}

var (
	sharedMinIOOnce sync.Once
	sharedMinIO     sharedMinIOState
)

func resolveSharedMinIO() sharedMinIOState {
	sharedMinIOOnce.Do(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
		defer cancel()

		c, err := tcminio.Run(ctx, minioImage, minioRunOptions()...)
		if err != nil {
			// Keep any partially-started container handle so TestMain can
			// terminate it.
			sharedMinIO = sharedMinIOState{container: c, err: err}
			return
		}
		endpoint, err := c.ConnectionString(ctx)
		if err != nil {
			sharedMinIO = sharedMinIOState{container: c, err: err}
			return
		}
		sharedMinIO = sharedMinIOState{container: c, endpoint: endpoint}
	})
	return sharedMinIO
}

func TestMain(m *testing.M) {
	code := m.Run()
	// Terminate the shared container using a FRESH context — the 90s start
	// context is cancelled by resolveSharedMinIO's defer. Tolerate a nil handle
	// (every test skipped, or the Once stored a start error before a container
	// existed), so a skip host cannot turn a clean skip into a package failure.
	if sharedMinIO.container != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		_ = sharedMinIO.container.Terminate(ctx)
		cancel()
	}
	os.Exit(code)
}

// startMinIO returns an S3 client bound to the shared MinIO container plus a
// fresh per-test bucket (the uuid-suffixed name keeps tests isolated even
// though they share the container). It skips only for the environmental
// preconditions minioSkipReason recognises, and fatals on anything else.
func startMinIO(t *testing.T) (*s3.Client, string) {
	t.Helper()

	// FISHHAWK_SKIP_INTEGRATION is an unconditional pre-startup gate: it must be
	// honoured on a healthy Docker host too, so it cannot live only inside the
	// error classifier, which runs only on a startup/endpoint error.
	if os.Getenv("FISHHAWK_SKIP_INTEGRATION") != "" {
		t.Skip(skipMsg("FISHHAWK_SKIP_INTEGRATION is set"))
	}

	shared := resolveSharedMinIO()
	if shared.err != nil {
		if reason, ok := minioSkipReason(shared.err); ok {
			t.Skip(reason)
		}
		t.Fatalf("start minio: %v", shared.err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	cfg, err := config.LoadDefaultConfig(ctx,
		config.WithRegion("us-east-1"),
		config.WithCredentialsProvider(
			credentials.NewStaticCredentialsProvider(minioUser, minioPass, ""),
		),
	)
	if err != nil {
		t.Fatalf("aws config: %v", err)
	}
	client := s3.NewFromConfig(cfg, func(o *s3.Options) {
		o.BaseEndpoint = aws.String("http://" + shared.endpoint)
		// MinIO requires path-style requests; AWS S3 also accepts
		// them, so this is the safe choice in either environment.
		o.UsePathStyle = true
	})

	// Per-test bucket name keeps tests isolated on the shared container.
	bucket := "fishhawk-test-" + strings.ReplaceAll(uuid.NewString(), "-", "")[:24]
	if _, err := client.CreateBucket(ctx, &s3.CreateBucketInput{
		Bucket: aws.String(bucket),
	}); err != nil {
		t.Fatalf("create bucket: %v", err)
	}

	return client, bucket
}

// skipMsg builds an actionable skip line naming the package, the reason, and
// the operator remedy.
func skipMsg(reason string) string {
	return fmt.Sprintf("tracestore S3 integration suite skipped: %s. "+
		"Check `docker ps` and restart Docker Desktop if the daemon is loaded. See #2948.", reason)
}

// minioSkipReason classifies a MinIO container-start / endpoint-resolution error
// into a fail-soft skip. It returns an actionable reason and true for exactly
// the environmental preconditions this suite cannot control, and false for
// everything else so a genuine tracestore regression still fails. It is reached
// ONLY from the container-start / endpoint path in startMinIO, never from an S3
// operation.
func minioSkipReason(err error) (string, bool) {
	if err == nil {
		return "", false
	}
	// (a) explicit opt-out. Also honoured as a pre-startup gate in startMinIO.
	if os.Getenv("FISHHAWK_SKIP_INTEGRATION") != "" {
		return skipMsg("FISHHAWK_SKIP_INTEGRATION is set"), true
	}
	msg := strings.ToLower(err.Error())
	// (b) Docker daemon unreachable.
	for _, marker := range []string{
		"cannot connect to the docker daemon",
		"docker: not found",
		"executable file not found",
		"dial unix /var/run/docker.sock",
	} {
		if strings.Contains(msg, marker) {
			return skipMsg("Docker daemon unreachable: " + marker), true
		}
	}
	// (c) the published-port mapping never appeared — the #2948 shape. The real
	// error is errdefs.ErrNotFound.WithMessage(`port "9000/tcp" not found`)
	// (docker.go PortEndpoint). Require the CONJUNCTION of the sentinel AND the
	// port-naming message, NOT errors.Is alone: ErrNotFound is a GENERIC
	// not-found sentinel (also raised for a missing image, container, or
	// network), so matching it alone would silently skip the whole suite on an
	// unrelated Docker not-found.
	portNotFound := strings.Contains(msg, `port "9000/tcp" not found`)
	if errors.Is(err, errdefs.ErrNotFound) && portNotFound {
		return skipMsg(`MinIO published port "9000/tcp" not found (errdefs.ErrNotFound); ` +
			`the Docker Desktop port proxy never resolved the mapping`), true
	}
	// A bare port-not-found string carrying no sentinel is strictly narrower and
	// still unambiguously ours.
	if portNotFound {
		return skipMsg(`MinIO published port "9000/tcp" not found; ` +
			`the Docker Desktop port proxy never resolved the mapping`), true
	}
	return "", false
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

// --- Done-means and fail-soft classifier tests (no Docker required) ---

// flattenStrategies walks a wait strategy tree, recursing into the exported
// *wait.MultiStrategy.Strategies, returning every leaf strategy.
func flattenStrategies(s wait.Strategy) []wait.Strategy {
	if s == nil {
		return nil
	}
	if ms, ok := s.(*wait.MultiStrategy); ok {
		var out []wait.Strategy
		for _, inner := range ms.Strategies {
			out = append(out, flattenStrategies(inner)...)
		}
		return out
	}
	return []wait.Strategy{s}
}

// TestMinioRunOptions_IncludeAPortResolvingWait is the done-means test: the
// change's correctness is configuration, not compilation, so presence-of-touch
// proves nothing. It pre-seeds req.WaitingFor with a log-only stand-in, applies
// the shipped minioRunOptions() customizers, and asserts the result APPENDS —
// retaining the seeded log strategy AND adding a port-resolving one. A revert to
// the replacing WithWaitStrategy(wait.ForLog(...)) drops the seeded strategy and
// fails here, which is what makes counterfactual (i) meaningful. It needs no
// Docker, so it runs everywhere including CI.
func TestMinioRunOptions_IncludeAPortResolvingWait(t *testing.T) {
	const seededLog = "seeded-log-sentinel"
	req := testcontainers.GenericContainerRequest{
		ContainerRequest: testcontainers.ContainerRequest{
			Env:        map[string]string{},
			WaitingFor: wait.ForLog(seededLog),
		},
	}
	for _, opt := range minioRunOptions() {
		if err := opt.Customize(&req); err != nil {
			t.Fatalf("customize: %v", err)
		}
	}

	strategies := flattenStrategies(req.WaitingFor)
	if len(strategies) == 0 {
		t.Fatal("no wait strategy present after applying options")
	}

	var haveSeededLog, havePort, haveNonSeededLog bool
	for _, s := range strategies {
		switch v := s.(type) {
		case *wait.LogStrategy:
			if v.Log == seededLog {
				haveSeededLog = true
			} else {
				haveNonSeededLog = true
			}
		case *wait.HostPortStrategy:
			if v.Port != "" {
				havePort = true
			}
			haveNonSeededLog = true
		case *wait.HTTPStrategy:
			// An HTTP wait always resolves a mapped port before probing, so its
			// presence is a port-resolving strategy. (network.Port has no string
			// comparison; the HostPortStrategy above is the primary proof here.)
			_ = v
			havePort = true
			haveNonSeededLog = true
		default:
			haveNonSeededLog = true
		}
	}

	if !haveSeededLog {
		t.Error("append lost the pre-seeded log strategy: WithWaitStrategy replaced instead of appending")
	}
	if !havePort {
		t.Error("no port-resolving wait strategy present (*wait.HostPortStrategy or *wait.HTTPStrategy with a port)")
	}
	if !haveNonSeededLog {
		t.Error("wait strategy set is log-only; the port-mapping wait was dropped")
	}
}

// TestMinioSkipReason asserts one case per named branch of the fail-soft
// classifier, plus two negative controls, so the skip cannot mask a genuine
// tracestore regression. It asserts on the returned reason text, not only the
// bool.
func TestMinioSkipReason(t *testing.T) {
	t.Run("FISHHAWK_SKIP_INTEGRATION set", func(t *testing.T) {
		t.Setenv("FISHHAWK_SKIP_INTEGRATION", "1")
		reason, ok := minioSkipReason(errors.New("anything at all"))
		if !ok {
			t.Fatal("expected skip when FISHHAWK_SKIP_INTEGRATION is set")
		}
		if !strings.Contains(reason, "FISHHAWK_SKIP_INTEGRATION") {
			t.Errorf("reason %q should name the flag", reason)
		}
	})

	// Ensure the flag is unset for the error-based cases (the test binary may be
	// invoked with it set).
	t.Setenv("FISHHAWK_SKIP_INTEGRATION", "")

	dockerMarkers := []string{
		"cannot connect to the docker daemon",
		"docker: not found",
		"executable file not found",
		"dial unix /var/run/docker.sock",
	}
	for _, marker := range dockerMarkers {
		t.Run("docker unavailable: "+marker, func(t *testing.T) {
			reason, ok := minioSkipReason(fmt.Errorf("run minio: %s", marker))
			if !ok {
				t.Fatalf("expected skip for docker-unavailable marker %q", marker)
			}
			if !strings.Contains(strings.ToLower(reason), marker) {
				t.Errorf("reason %q should name the marker %q", reason, marker)
			}
		})
	}

	t.Run("errdefs.ErrNotFound port-not-found", func(t *testing.T) {
		err := fmt.Errorf("connection string: %w",
			errdefs.ErrNotFound.WithMessage(`port "9000/tcp" not found`))
		reason, ok := minioSkipReason(err)
		if !ok {
			t.Fatal("expected skip for errdefs.ErrNotFound-wrapped port-not-found")
		}
		// The errdefs branch must be the one that fired (distinct reason text),
		// so deleting it and falling through to the bare-string branch is
		// observable.
		if !strings.Contains(reason, `port "9000/tcp" not found`) {
			t.Errorf("reason %q should name the port", reason)
		}
		if !strings.Contains(reason, "errdefs.ErrNotFound") {
			t.Errorf("reason %q should name the errdefs sentinel branch", reason)
		}
	})

	t.Run("bare port-not-found string", func(t *testing.T) {
		reason, ok := minioSkipReason(errors.New(`port "9000/tcp" not found`))
		if !ok {
			t.Fatal("expected skip for bare port-not-found string")
		}
		if !strings.Contains(reason, `port "9000/tcp" not found`) {
			t.Errorf("reason %q should name the port", reason)
		}
	})

	// Negative controls: the fail-soft must NOT swallow these.
	t.Run("errdefs.ErrNotFound unrelated message does not skip", func(t *testing.T) {
		// The exact hole C1 closes: ErrNotFound is a generic sentinel raised for
		// a missing image/container/network too.
		err := errdefs.ErrNotFound.WithMessage("No such image: minio/minio:latest")
		if reason, ok := minioSkipReason(err); ok {
			t.Errorf("ErrNotFound with an unrelated message must not skip; got reason %q", reason)
		}
	})

	t.Run("unrelated S3 error does not skip", func(t *testing.T) {
		if reason, ok := minioSkipReason(errors.New("NoSuchBucket: the specified bucket does not exist")); ok {
			t.Errorf("unrelated S3 error must not skip; got reason %q", reason)
		}
	})

	t.Run("plain error does not skip", func(t *testing.T) {
		if reason, ok := minioSkipReason(errors.New("boom")); ok {
			t.Errorf("plain error must not skip; got reason %q", reason)
		}
	})
}
