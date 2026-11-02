package tracestore

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/container"
	"github.com/google/uuid"
)

func azureFixture(t *testing.T, handler http.HandlerFunc) *AzureStorage {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	client, err := container.NewClientWithNoCredential(server.URL+"/traces?sig=never-log-this", &container.ClientOptions{ClientOptions: policy.ClientOptions{Retry: policy.RetryOptions{MaxRetries: -1}}})
	if err != nil {
		t.Fatal(err)
	}
	return &AzureStorage{client: client}
}

func TestAzureStorageRoundTrip(t *testing.T) {
	ref := BundleRef{RunID: uuid.New(), Variant: VariantRaw, ContentHash: strings.Repeat("a", 64)}
	payload := "compressed trace bytes"
	var uploaded string
	stamp := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	store := azureFixture(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/traces/"+ref.Key() {
			t.Errorf("unexpected key %s", r.URL.Path)
		}
		w.Header().Set("ETag", `"abc"`)
		w.Header().Set("Last-Modified", stamp.Format(http.TimeFormat))
		switch r.Method {
		case http.MethodPut:
			if r.URL.Query().Get("comp") != "blocklist" {
				data, err := io.ReadAll(r.Body)
				if err != nil {
					t.Error(err)
				}
				uploaded = string(data)
			}
			w.WriteHeader(http.StatusCreated)
		case http.MethodGet:
			_, _ = io.WriteString(w, uploaded)
		case http.MethodHead:
			w.Header().Set("Content-Length", fmt.Sprint(len(uploaded)))
		default:
			t.Errorf("unexpected method %s", r.Method)
		}
	})
	ctx := context.Background()
	if err := store.Put(ctx, ref, strings.NewReader(payload)); err != nil {
		t.Fatal(err)
	}
	if uploaded != payload {
		t.Fatalf("upload %q", uploaded)
	}
	body, err := store.Get(ctx, ref)
	if err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(body)
	_ = body.Close()
	if err != nil || string(got) != payload {
		t.Fatalf("get %q %v", got, err)
	}
	stat, err := store.Stat(ctx, ref)
	if err != nil {
		t.Fatal(err)
	}
	if stat.Size != int64(len(payload)) || stat.ETag != "abc" || !stat.LastModified.Equal(stamp) {
		t.Fatalf("stat %+v", stat)
	}
}

func TestAzureStorageListPages(t *testing.T) {
	run := uuid.New()
	raw := BundleRef{RunID: run, Variant: VariantRaw, ContentHash: strings.Repeat("b", 64)}
	redacted := BundleRef{RunID: run, Variant: VariantRedacted, ContentHash: strings.Repeat("a", 64)}
	calls := 0
	store := azureFixture(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		if r.URL.Query().Get("prefix") != run.String()+"/" {
			t.Error("missing run scope")
		}
		w.Header().Set("Content-Type", "application/xml")
		if calls == 1 {
			_, _ = fmt.Fprintf(w, `<EnumerationResults><Blobs><Blob><Name>%s</Name></Blob><Blob><Name>unrelated</Name></Blob><Blob><Name>%s/raw/%s.jsonl.gz</Name></Blob></Blobs><NextMarker>next</NextMarker></EnumerationResults>`, redacted.Key(), uuid.New(), strings.Repeat("c", 64))
		} else {
			if r.URL.Query().Get("marker") != "next" {
				t.Error("missing continuation")
			}
			_, _ = fmt.Fprintf(w, `<EnumerationResults><Blobs><Blob><Name>%s</Name></Blob></Blobs><NextMarker/></EnumerationResults>`, raw.Key())
		}
	})
	refs, err := store.List(context.Background(), run)
	if err != nil || calls != 2 || len(refs) != 2 {
		t.Fatalf("list %+v calls=%d err=%v", refs, calls, err)
	}
	if refs[0] != raw || refs[1] != redacted {
		t.Fatalf("ordering %+v", refs)
	}
}

func TestAzureStorageFailures(t *testing.T) {
	ref := BundleRef{RunID: uuid.New(), Variant: VariantRaw, ContentHash: strings.Repeat("a", 64)}
	for _, code := range []string{"BlobNotFound", "ContainerNotFound", "AuthorizationFailure"} {
		t.Run(code, func(t *testing.T) {
			store := azureFixture(t, func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("x-ms-error-code", code)
				w.WriteHeader(http.StatusNotFound)
				_, _ = fmt.Fprintf(w, `<Error><Code>%s</Code><Message>never-log-this</Message></Error>`, code)
			})
			ctx := context.Background()
			_, getErr := store.Get(ctx, ref)
			_, statErr := store.Stat(ctx, ref)
			_, listErr := store.List(ctx, ref.RunID)
			putErr := store.Put(ctx, ref, strings.NewReader("trace"))
			for _, err := range []error{getErr, statErr, listErr, putErr} {
				if err == nil || strings.Contains(err.Error(), "never-log-this") {
					t.Fatalf("unsafe error %v", err)
				}
				if errors.Is(err, ErrNotFound) != (code == "BlobNotFound") {
					t.Fatalf("wrong missing-object classification %v", err)
				}
			}
		})
	}
	store := azureFixture(t, func(w http.ResponseWriter, r *http.Request) { t.Error("invalid ref reached network") })
	for _, bad := range []BundleRef{{}, {RunID: ref.RunID, Variant: "bad", ContentHash: ref.ContentHash}, {RunID: ref.RunID, Variant: VariantRaw, ContentHash: "short"}} {
		if err := store.Put(context.Background(), bad, strings.NewReader("")); err == nil {
			t.Error("put accepted bad ref")
		}
		if _, err := store.Get(context.Background(), bad); err == nil {
			t.Error("get accepted bad ref")
		}
		if _, err := store.Stat(context.Background(), bad); err == nil {
			t.Error("stat accepted bad ref")
		}
	}
	if _, err := store.List(context.Background(), uuid.Nil); err == nil {
		t.Error("list accepted zero run")
	}
}

func TestAzureConstructorAndErrorRedaction(t *testing.T) {
	for _, args := range [][2]string{{"", "traces"}, {"invalid never-log-this", "traces"}, {"anything", ""}} {
		_, err := NewAzureStorage(args[0], args[1])
		if err == nil || strings.Contains(err.Error(), "never-log-this") {
			t.Fatalf("constructor %v", err)
		}
	}
	if _, err := NewAzureStorage("BlobEndpoint=https://example.blob.core.windows.net/;SharedAccessSignature=sv=2023-11-03&sig=secret", "traces"); err != nil {
		t.Fatal(err)
	}
	for _, err := range []error{context.Canceled, context.DeadlineExceeded} {
		if !errors.Is(azureStorageError("get", err), err) {
			t.Error("lost context error")
		}
	}
	if got := azureStorageError("get", errors.New("https://example/?sig=never-log-this")); strings.Contains(got.Error(), "never-log-this") {
		t.Fatal(got)
	}
	if got := azureStorageError("get", &azcore.ResponseError{StatusCode: 403, ErrorCode: "never-log-this"}); got.Error() != "tracestore: Azure get failed (HTTP 403)" {
		t.Fatal(got)
	}
}
