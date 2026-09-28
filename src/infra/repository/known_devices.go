package repository

import (
	"context"
	"database/sql"
	"time"

	"github.com/uptrace/bun"

	"github.com/ogen-app/ogen/src/domain/models"
)

// KnownDeviceRepository persists the browsers each account has signed in from.
// The table is account-level, not tenant-scoped. Methods taking a bun.IDB join
// the caller's transaction; nil uses the repository's DB.
type KnownDeviceRepository interface {
	// Touch refreshes a device the account already knows and reports whether
	// one matched. It is the whole known-device path: no email, no lock.
	Touch(ctx context.Context, accountID, deviceHash, ip, userAgent, label string, now time.Time) (bool, error)
	// LockEnrollment locks the account row and reports whether the account has
	// enrolled a device before. Concurrent logins of one account serialize on
	// it, so they agree on which one is the first device.
	LockEnrollment(ctx context.Context, db bun.IDB, accountID string) (bool, error)
	// MarkEnrolled records that the account has enrolled a device; a later call
	// keeps the first timestamp.
	MarkEnrolled(ctx context.Context, db bun.IDB, accountID string, at time.Time) error
	// Upsert inserts the device, or refreshes it if a concurrent login inserted
	// the same (account, hash) first, and sets d.ID to the stored row's id.
	Upsert(ctx context.Context, db bun.IDB, d *models.KnownDevice) error
	// DeleteForAccount forgets every device of the account and its enrolment,
	// so the next login enrols silently again.
	DeleteForAccount(ctx context.Context, db bun.IDB, accountID string) (int, error)
	// DeleteUnseenSince drops devices last seen before cutoff.
	DeleteUnseenSince(ctx context.Context, cutoff time.Time) (int, error)
}

type knownDeviceRepository struct {
	db *bun.DB
}

// NewKnownDeviceRepository returns a Bun-backed KnownDeviceRepository.
func NewKnownDeviceRepository(db *bun.DB) KnownDeviceRepository {
	return &knownDeviceRepository{db: db}
}

func (r *knownDeviceRepository) idb(db bun.IDB) bun.IDB {
	if db == nil {
		return r.db
	}
	return db
}

func (r *knownDeviceRepository) Touch(ctx context.Context, accountID, deviceHash, ip, userAgent, label string, now time.Time) (bool, error) {
	res, err := r.db.NewUpdate().Model((*models.KnownDevice)(nil)).
		Set("last_seen_at = ?", now).
		Set("last_ip = ?", ip).
		Set("user_agent = ?", userAgent).
		Set("device_label = ?", label).
		Where("account_id = ?", accountID).
		Where("device_hash = ?", deviceHash).
		Exec(ctx)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

func (r *knownDeviceRepository) LockEnrollment(ctx context.Context, db bun.IDB, accountID string) (bool, error) {
	var enrolledAt sql.NullTime
	err := r.idb(db).NewSelect().Table("accounts").Column("devices_enrolled_at").
		Where("id = ?", accountID).For("UPDATE").Scan(ctx, &enrolledAt)
	return enrolledAt.Valid, err
}

func (r *knownDeviceRepository) MarkEnrolled(ctx context.Context, db bun.IDB, accountID string, at time.Time) error {
	_, err := r.idb(db).NewUpdate().Table("accounts").
		Set("devices_enrolled_at = COALESCE(devices_enrolled_at, ?)", at).
		Where("id = ?", accountID).
		Exec(ctx)
	return err
}

func (r *knownDeviceRepository) Upsert(ctx context.Context, db bun.IDB, d *models.KnownDevice) error {
	_, err := r.idb(db).NewInsert().Model(d).
		On("CONFLICT (account_id, device_hash) DO UPDATE").
		Set("last_seen_at = EXCLUDED.last_seen_at").
		Set("last_ip = EXCLUDED.last_ip").
		Set("user_agent = EXCLUDED.user_agent").
		Set("device_label = EXCLUDED.device_label").
		Returning("id").
		Exec(ctx)
	return err
}

func (r *knownDeviceRepository) DeleteForAccount(ctx context.Context, db bun.IDB, accountID string) (int, error) {
	db = r.idb(db)
	res, err := db.NewDelete().Model((*models.KnownDevice)(nil)).
		Where("account_id = ?", accountID).
		Exec(ctx)
	if err != nil {
		return 0, err
	}
	if _, err := db.NewUpdate().Table("accounts").
		Set("devices_enrolled_at = NULL").
		Where("id = ?", accountID).
		Exec(ctx); err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

func (r *knownDeviceRepository) DeleteUnseenSince(ctx context.Context, cutoff time.Time) (int, error) {
	res, err := r.db.NewDelete().Model((*models.KnownDevice)(nil)).
		Where("last_seen_at < ?", cutoff).
		Exec(ctx)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}
