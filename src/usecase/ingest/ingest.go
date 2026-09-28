// Package ingest creates content-bank assets: it stores an asset's original
// bytes ahead of its rows, inserts the asset (and file row) together with its
// ingestion job in one transaction (transactional outbox: a committed asset
// always has its job, a rolled-back one leaves neither rows nor bytes), and
// dedupes image uploads by the SHA-256 of their original bytes.
package ingest

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/uptrace/bun"

	"github.com/ogen-app/ogen/src/domain/models"
	"github.com/ogen-app/ogen/src/infra/repository"
	"github.com/ogen-app/ogen/src/infra/storage"
)

// checksumIndex is the per-tenant unique index on asset_files.checksum_sha256.
const checksumIndex = "idx_asset_files_tenant_checksum"

// Enqueue schedules an asset's ingestion job inside the creating transaction.
type Enqueue func(ctx context.Context, tx *sql.Tx) error

// Blob is an object stored ahead of the rows that reference it, so a failed
// upload never leaves a row pointing at missing bytes.
type Blob struct {
	store storage.Storage
	key   string
}

// PutBlob uploads data to key. The error is the storage error, unwrapped.
func PutBlob(ctx context.Context, store storage.Storage, key string, data []byte, contentType string) (*Blob, error) {
	if _, err := store.Upload(ctx, key, bytes.NewReader(data), int64(len(data)), contentType); err != nil {
		return nil, err
	}
	return &Blob{store: store, key: key}, nil
}

// Key is the blob's full storage key.
func (b *Blob) Key() string { return b.key }

// Discard deletes the blob, best-effort. Nil-safe.
func (b *Blob) Discard(ctx context.Context) {
	if b == nil {
		return
	}
	_ = b.store.Delete(ctx, b.key)
}

// Checksum is the hex SHA-256 of an upload's original bytes — the dedupe key,
// stable regardless of later normalization.
func Checksum(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// Service creates assets. Files and Images are optional: nil Files disables
// checksum dedupe, nil Images leaves mirrored image blobs out of ObjectKeys.
type Service struct {
	DB     *bun.DB
	Assets repository.AssetRepository
	Files  repository.AssetFileRepository
	Images repository.AssetImageRepository
}

// NewAsset is one asset to create. File, Blob and Enqueue are optional.
type NewAsset struct {
	Asset *models.Asset
	File  *models.AssetFile
	// Blob is the already-stored original; it is discarded when the create
	// rolls back.
	Blob    *Blob
	Enqueue Enqueue
}

// Create inserts the asset, its file row and its ingestion job atomically. It
// returns the asset that holds the content: in.Asset, or — when a concurrent
// upload of the same bytes won the checksum unique index — that earlier
// asset, so identical uploads stay idempotent.
func (s *Service) Create(ctx context.Context, in NewAsset) (*models.Asset, error) {
	err := s.DB.RunInTx(ctx, nil, func(ctx context.Context, tx bun.Tx) error {
		if _, err := tx.NewInsert().Model(in.Asset).Exec(ctx); err != nil {
			return err
		}
		if in.File != nil {
			if _, err := tx.NewInsert().Model(in.File).Exec(ctx); err != nil {
				return err
			}
		}
		if in.Enqueue == nil {
			return nil
		}
		return in.Enqueue(ctx, tx.Tx)
	})
	if err == nil {
		return in.Asset, nil
	}
	in.Blob.Discard(ctx)
	if in.File != nil && in.File.ChecksumSHA256 != "" && isUniqueViolationOn(err, checksumIndex) {
		if existing := s.FindByChecksum(ctx, in.File.ChecksumSHA256); existing != nil {
			return existing, nil
		}
	}
	return nil, err
}

// FindByChecksum returns the caller tenant's asset whose original has this
// checksum, or nil when there is none (or dedupe is unavailable). Lookup
// errors are treated as "no duplicate".
func (s *Service) FindByChecksum(ctx context.Context, checksum string) *models.Asset {
	if s.Files == nil {
		return nil
	}
	f, err := s.Files.GetByChecksum(ctx, checksum)
	if err != nil || f == nil {
		return nil
	}
	a, err := s.Assets.GetByID(ctx, f.AssetID)
	if err != nil {
		return nil
	}
	return a
}

// ObjectKeys lists every stored object of an asset, captured before the row
// is deleted (the cascade drops the file and image rows): the original and
// thumbnail, mirrored images, and the deterministic normalized derivatives
// (audio .opus, image .png) that have no row of their own — deleting a
// missing one is a no-op for other asset types.
func (s *Service) ObjectKeys(ctx context.Context, assetID string) ([]string, error) {
	var keys []string
	if s.Files != nil {
		f, err := s.Files.GetByAssetID(ctx, assetID)
		switch {
		case err == nil && f != nil:
			keys = appendNonEmpty(keys, f.S3Key)
			if f.ThumbnailS3Key != nil {
				keys = appendNonEmpty(keys, *f.ThumbnailS3Key)
			}
		case err != nil && !errors.Is(err, sql.ErrNoRows):
			return nil, err
		}
	}
	if s.Images != nil {
		if imgs, err := s.Images.GetByAssetID(ctx, assetID); err == nil {
			for i := range imgs {
				keys = appendNonEmpty(keys, imgs[i].S3Key)
			}
		}
	}
	return append(keys,
		storage.TenantKey(ctx, fmt.Sprintf("assets/%s/normalized.opus", assetID)),
		storage.TenantKey(ctx, fmt.Sprintf("assets/%s/normalized.png", assetID)),
	), nil
}

func appendNonEmpty(keys []string, k string) []string {
	if k == "" {
		return keys
	}
	return append(keys, k)
}

func isUniqueViolationOn(err error, constraint string) bool {
	pgErr, ok := errors.AsType[*pgconn.PgError](err)
	return ok && pgErr.Code == "23505" && pgErr.ConstraintName == constraint
}
