package tracestore

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/blob"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/bloberror"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/blockblob"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/container"
	"github.com/google/uuid"
)

// AzureStorage stores bundles in a private Azure Blob container. Credentials
// belong to the caller; the hosted pilot supplies a container-scoped SAS without
// delete permission through Key Vault, never a storage-account key.
type AzureStorage struct{ client *container.Client }

var _ Storage = (*AzureStorage)(nil)

// NewAzureStorage accepts an Azure connection string (including SAS connection
// strings). It never returns credential-bearing SDK constructor diagnostics.
func NewAzureStorage(connectionString, containerName string) (*AzureStorage, error) {
	if connectionString == "" || containerName == "" {
		return nil, errors.New("tracestore: Azure connection string and container are both required")
	}
	client, err := container.NewClientFromConnectionString(connectionString, containerName, nil)
	if err != nil {
		return nil, errors.New("tracestore: invalid Azure storage connection string")
	}
	return &AzureStorage{client: client}, nil
}

// Put streams a bundle with bounded buffering, preserving the canonical key.
func (s *AzureStorage) Put(ctx context.Context, ref BundleRef, body io.Reader) error {
	if err := ref.Validate(); err != nil {
		return err
	}
	contentType := "application/gzip"
	_, err := s.client.NewBlockBlobClient(ref.Key()).UploadStream(ctx, body, &blockblob.UploadStreamOptions{
		BlockSize: 4 * 1024 * 1024, Concurrency: 1,
		HTTPHeaders: &blob.HTTPHeaders{BlobContentType: &contentType},
	})
	return azureStorageError("put", err)
}

// Get streams a bundle; the caller closes the returned body.
func (s *AzureStorage) Get(ctx context.Context, ref BundleRef) (io.ReadCloser, error) {
	if err := ref.Validate(); err != nil {
		return nil, err
	}
	out, err := s.client.NewBlobClient(ref.Key()).DownloadStream(ctx, nil)
	if err != nil {
		return nil, azureStorageError("get", err)
	}
	return out.Body, nil
}

// Stat reads metadata without downloading the bundle.
func (s *AzureStorage) Stat(ctx context.Context, ref BundleRef) (Stat, error) {
	if err := ref.Validate(); err != nil {
		return Stat{}, err
	}
	out, err := s.client.NewBlobClient(ref.Key()).GetProperties(ctx, nil)
	if err != nil {
		return Stat{}, azureStorageError("stat", err)
	}
	var result Stat
	if out.ContentLength != nil {
		result.Size = *out.ContentLength
	}
	if out.ETag != nil {
		result.ETag = strings.Trim(string(*out.ETag), `"`)
	}
	if out.LastModified != nil {
		result.LastModified = *out.LastModified
	}
	return result, nil
}

// List follows every continuation page and ignores objects outside the bundle
// contract. Sorting makes the Storage ordering guarantee provider-independent.
func (s *AzureStorage) List(ctx context.Context, runID uuid.UUID) ([]BundleRef, error) {
	if runID == uuid.Nil {
		return nil, errors.New("tracestore: List requires a non-zero RunID")
	}
	prefix := runID.String() + "/"
	pager := s.client.NewListBlobsFlatPager(&container.ListBlobsFlatOptions{Prefix: &prefix})
	var result []BundleRef
	for pager.More() {
		page, err := pager.NextPage(ctx)
		if err != nil {
			return nil, azureStorageError("list", err)
		}
		if page.Segment == nil {
			continue
		}
		for _, item := range page.Segment.BlobItems {
			if item == nil || item.Name == nil {
				continue
			}
			if ref, ok := parseKey(*item.Name); ok && ref.RunID == runID {
				result = append(result, ref)
			}
		}
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Key() < result[j].Key() })
	return result, nil
}

// SDK errors may include the request URL and its SAS. Keep only typed status
// diagnostics; never put the SDK error/body/URL into an operator-visible error.
func azureStorageError(operation string, err error) error {
	if err == nil {
		return nil
	}
	if bloberror.HasCode(err, bloberror.BlobNotFound) {
		return ErrNotFound
	}
	if errors.Is(err, context.Canceled) {
		return context.Canceled
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return context.DeadlineExceeded
	}
	var response *azcore.ResponseError
	if errors.As(err, &response) {
		return fmt.Errorf("tracestore: Azure %s failed (HTTP %d)", operation, response.StatusCode)
	}
	return fmt.Errorf("tracestore: Azure %s failed", operation)
}
