package storage

import (
	"context"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"

	"github.com/ogen-app/ogen/src/kernel/config"
	"github.com/ogen-app/ogen/src/kernel/tenantctx"
)

// TenantKey namespaces an object key by the tenant in ctx:
// "t/<tenant_id>/<key>". A key built without a tenant in context (system work)
// is returned unprefixed, so pre-existing and global objects round-trip
// unchanged. The prefixed key is what callers must both Upload and persist, so
// every later Delete/Copy/PresignedGetURL operates on the same key.
func TenantKey(ctx context.Context, key string) string {
	if tid, ok := tenantctx.From(ctx); ok && tid != "" {
		return "t/" + tid + "/" + key
	}
	return key
}

// ObjectInfo is the subset of stored-object metadata callers need without
// downloading the object (CON-148 finalize). Size is the authoritative byte
// count; ContentType is whatever was set on PUT.
type ObjectInfo struct {
	Size        int64
	ContentType string
}

// Storage is the interface for object storage backends.
type Storage interface {
	// Upload writes r to the bucket under key and returns the public URL.
	Upload(ctx context.Context, key string, r io.Reader, size int64, contentType string) (string, error)
	// Copy duplicates the object at srcKey to dstKey within the same
	// bucket. Used by post cloning to give a clone its own
	// independent copies of its source's attachments, so deleting
	// either post never removes the other's blobs.
	Copy(ctx context.Context, srcKey, dstKey string) error
	// Delete removes the object at key. Returns nil when the object does not exist.
	Delete(ctx context.Context, key string) error
	// DeletePrefix removes every object whose key starts with prefix, which
	// must be a non-empty folder ending in "/". Used to drop everything an
	// asset or post owns, including objects no row records (an original whose
	// ingestion never finished, a presigned upload never finalized).
	DeletePrefix(ctx context.Context, prefix string) error
	// PublicURL returns the public URL for an object at key.
	PublicURL(key string) string
	// PresignedGetURL returns a short-lived signed GET URL for the object
	// at key. Used for serving private images to the browser and for
	// handing image bytes to Zernio at publish time.
	PresignedGetURL(ctx context.Context, key string, ttl time.Duration) (string, error)
	// PresignedPutURL returns a short-lived signed PUT URL for uploading an
	// object directly to storage under key, bypassing the API process — the
	// large-file video ingest path. contentType binds the
	// Content-Type header the client must send on the PUT.
	PresignedPutURL(ctx context.Context, key, contentType string, ttl time.Duration) (string, error)
	// Head returns metadata for the object at key without downloading it.
	// Used by video finalize to read the authoritative uploaded size before
	// probing.
	Head(ctx context.Context, key string) (*ObjectInfo, error)
	// Download returns a reader over the object at key; the caller must close
	// it. Used by the PDF ingestion job to re-read original.pdf from
	// the bucket on each attempt.
	Download(ctx context.Context, key string) (io.ReadCloser, error)
}

type s3Storage struct {
	client    *s3.Client
	presign   *s3.PresignClient
	bucket    string
	publicURL string // base URL, no trailing slash
}

// New returns an S3-compatible Storage client configured from cfg.
// Returns nil, nil when StorageEndpoint is empty (uploads disabled).
func New(cfg *config.Config) (Storage, error) {
	if cfg.StorageEndpoint == "" {
		return nil, nil
	}

	creds := credentials.NewStaticCredentialsProvider(cfg.StorageAccessKey, cfg.StorageSecretKey, "")

	client := s3.New(s3.Options{
		Region:       cfg.StorageRegion,
		Credentials:  creds,
		BaseEndpoint: aws.String(cfg.StorageEndpoint),
		// Required for path-style access used by R2 and DO Spaces.
		UsePathStyle: true,
	})

	publicURL := strings.TrimRight(cfg.StoragePublicURL, "/")

	return &s3Storage{
		client:    client,
		presign:   s3.NewPresignClient(client),
		bucket:    cfg.StorageBucket,
		publicURL: publicURL,
	}, nil
}

func (s *s3Storage) Upload(ctx context.Context, key string, r io.Reader, size int64, contentType string) (string, error) {
	_, err := s.client.PutObject(ctx, &s3.PutObjectInput{
		Bucket:        aws.String(s.bucket),
		Key:           aws.String(key),
		Body:          r,
		ContentLength: aws.Int64(size),
		ContentType:   aws.String(contentType),
	})
	if err != nil {
		return "", fmt.Errorf("storage: upload %s: %w", key, err)
	}
	return s.PublicURL(key), nil
}

func (s *s3Storage) Copy(ctx context.Context, srcKey, dstKey string) error {
	_, err := s.client.CopyObject(ctx, &s3.CopyObjectInput{
		Bucket:     aws.String(s.bucket),
		CopySource: aws.String(s.bucket + "/" + srcKey),
		Key:        aws.String(dstKey),
	})
	if err != nil {
		return fmt.Errorf("storage: copy %s -> %s: %w", srcKey, dstKey, err)
	}
	return nil
}

func (s *s3Storage) Delete(ctx context.Context, key string) error {
	_, err := s.client.DeleteObject(ctx, &s3.DeleteObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		return fmt.Errorf("storage: delete %s: %w", key, err)
	}
	return nil
}

func (s *s3Storage) DeletePrefix(ctx context.Context, prefix string) error {
	if !strings.HasSuffix(prefix, "/") || strings.Trim(prefix, "/") == "" {
		return fmt.Errorf("storage: delete prefix %q: must be a non-empty folder ending in /", prefix)
	}
	pages := s3.NewListObjectsV2Paginator(s.client, &s3.ListObjectsV2Input{
		Bucket: aws.String(s.bucket),
		Prefix: aws.String(prefix),
	})
	for pages.HasMorePages() {
		page, err := pages.NextPage(ctx)
		if err != nil {
			return fmt.Errorf("storage: list %s: %w", prefix, err)
		}
		// One DeleteObject per key: R2 and other S3-compatibles disagree on
		// the checksum headers the batch DeleteObjects call requires.
		for _, obj := range page.Contents {
			if err := s.Delete(ctx, aws.ToString(obj.Key)); err != nil {
				return err
			}
		}
	}
	return nil
}

func (s *s3Storage) PresignedPutURL(ctx context.Context, key, contentType string, ttl time.Duration) (string, error) {
	in := &s3.PutObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
	}
	if contentType != "" {
		in.ContentType = aws.String(contentType)
	}
	req, err := s.presign.PresignPutObject(ctx, in, s3.WithPresignExpires(ttl))
	if err != nil {
		return "", fmt.Errorf("storage: presign put %s: %w", key, err)
	}
	return req.URL, nil
}

func (s *s3Storage) Head(ctx context.Context, key string) (*ObjectInfo, error) {
	out, err := s.client.HeadObject(ctx, &s3.HeadObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		return nil, fmt.Errorf("storage: head %s: %w", key, err)
	}
	info := &ObjectInfo{}
	if out.ContentLength != nil {
		info.Size = *out.ContentLength
	}
	if out.ContentType != nil {
		info.ContentType = *out.ContentType
	}
	return info, nil
}

func (s *s3Storage) Download(ctx context.Context, key string) (io.ReadCloser, error) {
	out, err := s.client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		return nil, fmt.Errorf("storage: download %s: %w", key, err)
	}
	return out.Body, nil
}

func (s *s3Storage) PublicURL(key string) string {
	return s.publicURL + "/" + key
}

func (s *s3Storage) PresignedGetURL(ctx context.Context, key string, ttl time.Duration) (string, error) {
	req, err := s.presign.PresignGetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
	}, s3.WithPresignExpires(ttl))
	if err != nil {
		return "", fmt.Errorf("storage: presign get %s: %w", key, err)
	}
	return req.URL, nil
}
