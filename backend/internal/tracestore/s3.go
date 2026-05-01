package tracestore

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"
	"github.com/google/uuid"
)

// S3API is the subset of *s3.Client we use. Defining it as an
// interface lets tests substitute fakes if they ever need to
// without spinning a MinIO container — though the actual tests in
// this package use MinIO so behavior is exercised against real S3
// semantics, not a mock.
type S3API interface {
	PutObject(ctx context.Context, in *s3.PutObjectInput, opts ...func(*s3.Options)) (*s3.PutObjectOutput, error)
	GetObject(ctx context.Context, in *s3.GetObjectInput, opts ...func(*s3.Options)) (*s3.GetObjectOutput, error)
	HeadObject(ctx context.Context, in *s3.HeadObjectInput, opts ...func(*s3.Options)) (*s3.HeadObjectOutput, error)
	ListObjectsV2(ctx context.Context, in *s3.ListObjectsV2Input, opts ...func(*s3.Options)) (*s3.ListObjectsV2Output, error)
}

// S3Storage is the S3-backed Storage implementation. Construct
// directly with the bucket name and an *s3.Client; the package does
// not own credential / endpoint configuration so callers can wire
// up AWS production, MinIO local dev, or whatever else they like
// (e.g. a moto-server in CI) the same way.
type S3Storage struct {
	client S3API
	bucket string
}

// NewS3Storage wraps an S3 client + bucket as a Storage.
func NewS3Storage(client S3API, bucket string) *S3Storage {
	return &S3Storage{client: client, bucket: bucket}
}

// Put writes the bundle bytes. The content type is set to
// application/gzip so consumers (including the AWS console) treat
// it correctly.
func (s *S3Storage) Put(ctx context.Context, ref BundleRef, body io.Reader) error {
	if err := ref.Validate(); err != nil {
		return err
	}
	_, err := s.client.PutObject(ctx, &s3.PutObjectInput{
		Bucket:      aws.String(s.bucket),
		Key:         aws.String(ref.Key()),
		Body:        body,
		ContentType: aws.String("application/gzip"),
	})
	if err != nil {
		return fmt.Errorf("tracestore: put %s: %w", ref.Key(), err)
	}
	return nil
}

// Get streams the bundle bytes. Caller closes the returned reader.
func (s *S3Storage) Get(ctx context.Context, ref BundleRef) (io.ReadCloser, error) {
	if err := ref.Validate(); err != nil {
		return nil, err
	}
	out, err := s.client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(ref.Key()),
	})
	if err != nil {
		if isNoSuchKey(err) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("tracestore: get %s: %w", ref.Key(), err)
	}
	return out.Body, nil
}

// Stat reads the head metadata only.
func (s *S3Storage) Stat(ctx context.Context, ref BundleRef) (Stat, error) {
	if err := ref.Validate(); err != nil {
		return Stat{}, err
	}
	out, err := s.client.HeadObject(ctx, &s3.HeadObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(ref.Key()),
	})
	if err != nil {
		if isNoSuchKey(err) {
			return Stat{}, ErrNotFound
		}
		return Stat{}, fmt.Errorf("tracestore: stat %s: %w", ref.Key(), err)
	}
	stat := Stat{}
	if out.ContentLength != nil {
		stat.Size = *out.ContentLength
	}
	if out.ETag != nil {
		stat.ETag = strings.Trim(*out.ETag, `"`)
	}
	if out.LastModified != nil {
		stat.LastModified = *out.LastModified
	}
	return stat, nil
}

// List enumerates every bundle for a run. S3 ListObjectsV2 is
// paginated; we walk all pages so the caller sees the full set.
func (s *S3Storage) List(ctx context.Context, runID uuid.UUID) ([]BundleRef, error) {
	if runID == uuid.Nil {
		return nil, fmt.Errorf("tracestore: List requires a non-zero RunID")
	}
	prefix := runID.String() + "/"
	var (
		out               []BundleRef
		continuationToken *string
	)
	for {
		page, err := s.client.ListObjectsV2(ctx, &s3.ListObjectsV2Input{
			Bucket:            aws.String(s.bucket),
			Prefix:            aws.String(prefix),
			ContinuationToken: continuationToken,
		})
		if err != nil {
			return nil, fmt.Errorf("tracestore: list %s: %w", prefix, err)
		}
		for _, obj := range page.Contents {
			if obj.Key == nil {
				continue
			}
			ref, ok := parseKey(*obj.Key)
			if !ok {
				// Unrelated object under the run prefix; skip
				// silently. Logging would be noisy and not
				// actionable for the caller.
				continue
			}
			out = append(out, ref)
		}
		if page.IsTruncated == nil || !*page.IsTruncated {
			break
		}
		continuationToken = page.NextContinuationToken
	}
	return out, nil
}

// parseKey is the inverse of BundleRef.Key. Lenient: returns false
// for keys that don't match the canonical layout so List can skip
// foreign objects without erroring.
func parseKey(key string) (BundleRef, bool) {
	parts := strings.Split(key, "/")
	if len(parts) != 3 {
		return BundleRef{}, false
	}
	runID, err := uuid.Parse(parts[0])
	if err != nil {
		return BundleRef{}, false
	}
	variant := Variant(parts[1])
	if !variant.Valid() {
		return BundleRef{}, false
	}
	last := parts[2]
	const suffix = ".jsonl.gz"
	if !strings.HasSuffix(last, suffix) {
		return BundleRef{}, false
	}
	hash := strings.TrimSuffix(last, suffix)
	if len(hash) != 64 {
		return BundleRef{}, false
	}
	return BundleRef{RunID: runID, Variant: variant, ContentHash: hash}, true
}

// isNoSuchKey checks whether err corresponds to a missing-object
// response. The aws-sdk-go-v2 reports both NoSuchKey (GET) and
// NotFound (HEAD); both surface as smithy API errors with those
// codes.
func isNoSuchKey(err error) bool {
	if err == nil {
		return false
	}
	var nsk *types.NoSuchKey
	if errors.As(err, &nsk) {
		return true
	}
	var apiErr smithy.APIError
	if errors.As(err, &apiErr) {
		switch apiErr.ErrorCode() {
		case "NoSuchKey", "NotFound", "404":
			return true
		}
	}
	return false
}

// Compile-time check that S3Storage implements Storage.
var _ Storage = (*S3Storage)(nil)
