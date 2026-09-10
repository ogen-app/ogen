//go:build integration

package integration_test

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	s3types "github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"

	"github.com/ogen-app/ogen/src/infra/storage"
	"github.com/ogen-app/ogen/src/kernel/config"
)

// minioEndpoint is the path-style S3 endpoint exposed by
// docker-compose.integration.yml. The Makefile waits on
// /minio/health/live before kicking the test binary off.
const (
	minioEndpoint  = "http://localhost:9100"
	minioRegion    = "us-east-1"
	minioAccessKey = "integration"
	minioSecretKey = "integration-secret"
)

// integrationBucketSeq guarantees every call to mustOpenIntegrationStorage
// gets a unique bucket so concurrent ginkgo Describes never clobber each
// other's objects.
var integrationBucketSeq atomic.Uint64

// mustOpenIntegrationStorage returns a real storage.Storage backed by
// MinIO plus a raw *s3.Client for direct assertions (HeadObject,
// ListObjects), and the bucket name. Bucket is created on first use;
// callers don't need to clean it up — they can DeleteObject keys
// individually when they care, or just leak the bucket (tests are
// throwaway).
func mustOpenIntegrationStorage() (storage.Storage, *s3.Client, string) {
	bucket := fmt.Sprintf("ogen-it-%d", integrationBucketSeq.Add(1))

	cfg := &config.Config{
		StorageEndpoint:  minioEndpoint,
		StorageRegion:    minioRegion,
		StorageAccessKey: minioAccessKey,
		StorageSecretKey: minioSecretKey,
		StorageBucket:    bucket,
		StoragePublicURL: minioEndpoint + "/" + bucket,
	}
	store, err := storage.New(cfg)
	if err != nil {
		panic(err)
	}
	if store == nil {
		panic("storage.New returned nil — check StorageEndpoint")
	}

	raw := newRawS3()
	if _, err := raw.CreateBucket(tenantCtx(), &s3.CreateBucketInput{
		Bucket: aws.String(bucket),
	}); err != nil {
		apiErr, ok := errors.AsType[smithy.APIError](err)
		if !ok || (apiErr.ErrorCode() != "BucketAlreadyOwnedByYou" && apiErr.ErrorCode() != "BucketAlreadyExists") {
			panic(fmt.Sprintf("create bucket %s: %v", bucket, err))
		}
	}

	return store, raw, bucket
}

// newRawS3 builds an s3.Client pointed at the MinIO endpoint with the
// same path-style + static credentials used by storage.New. Used by
// integration tests for assertions that the production interface
// doesn't (and shouldn't) expose — HeadObject, ListObjectsV2.
func newRawS3() *s3.Client {
	creds := credentials.NewStaticCredentialsProvider(minioAccessKey, minioSecretKey, "")
	return s3.New(s3.Options{
		Region:       minioRegion,
		Credentials:  creds,
		BaseEndpoint: aws.String(minioEndpoint),
		UsePathStyle: true,
	})
}

// objectExists reports whether key is present in bucket. False when
// HeadObject returns a 404-class NotFound error.
func objectExists(ctx context.Context, raw *s3.Client, bucket, key string) bool {
	_, err := raw.HeadObject(ctx, &s3.HeadObjectInput{
		Bucket: aws.String(bucket),
		Key:    aws.String(key),
	})
	if err == nil {
		return true
	}
	if _, ok := errors.AsType[*s3types.NotFound](err); ok {
		return false
	}
	// Any other error is propagated as a panic so the test fails loudly
	// rather than silently treating a misconfigured client as "object
	// missing".
	panic(fmt.Sprintf("HeadObject %s/%s: %v", bucket, key, err))
}

// countObjects returns how many keys exist in bucket. Used to assert
// "no PutObject happened" after a 415.
func countObjects(ctx context.Context, raw *s3.Client, bucket string) int {
	out, err := raw.ListObjectsV2(ctx, &s3.ListObjectsV2Input{
		Bucket: aws.String(bucket),
	})
	if err != nil {
		panic(fmt.Sprintf("ListObjectsV2 %s: %v", bucket, err))
	}
	return len(out.Contents)
}
