package keyedhash

import (
	"context"
	"crypto/hmac"
	"errors"
	"fmt"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/jackc/pgx/v5"
)

type Authority struct {
	keys Keyring
}

func NewAuthority(keys Keyring) Authority {
	return Authority{keys: keys}
}

func (a Authority) Validate(ctx context.Context, store interface {
	ListLookupHMACVersions(context.Context) ([]db.LookupHmacVersion, error)
}) error {
	rows, err := store.ListLookupHMACVersions(ctx)
	if err != nil {
		return fmt.Errorf("read lookup HMAC authority: %w", err)
	}
	_, err = a.selectRows(activeRows(rows))
	return err
}

func (a Authority) Lock(ctx context.Context, store interface {
	LockActiveLookupHMACVersions(context.Context) ([]db.LookupHmacVersion, error)
}) (Selection, error) {
	rows, err := store.LockActiveLookupHMACVersions(ctx)
	if err != nil {
		return Selection{}, fmt.Errorf("lock lookup HMAC authority: %w", err)
	}
	return a.selectRows(rows)
}

func (a Authority) Activate(ctx context.Context, tx pgx.Tx, version int32) (db.LookupHmacVersion, error) {
	if tx == nil {
		return db.LookupHmacVersion{}, errors.New("lookup HMAC maintenance transaction is required")
	}
	fingerprint, err := a.keys.Fingerprint(version)
	if err != nil {
		return db.LookupHmacVersion{}, err
	}
	queries := db.New(tx)
	if err := queries.LockLookupHMACMaintenance(ctx); err != nil {
		return db.LookupHmacVersion{}, fmt.Errorf("lock lookup HMAC maintenance: %w", err)
	}
	rows, err := queries.LockLookupHMACVersionsForMaintenance(ctx)
	if err != nil {
		return db.LookupHmacVersion{}, fmt.Errorf("lock lookup HMAC versions: %w", err)
	}
	for _, row := range rows {
		if row.Version == version {
			return db.LookupHmacVersion{}, fmt.Errorf("lookup HMAC version %d is already registered", version)
		}
	}
	active := activeRows(rows)
	if len(active) > 0 {
		if _, err := a.selectRows(active); err != nil {
			return db.LookupHmacVersion{}, err
		}
		updated, err := queries.ClearCurrentLookupHMACVersion(ctx)
		if err != nil {
			return db.LookupHmacVersion{}, fmt.Errorf("clear current lookup HMAC version: %w", err)
		}
		if updated != 1 {
			return db.LookupHmacVersion{}, fmt.Errorf("cleared %d current lookup HMAC versions, want 1", updated)
		}
	}
	created, err := queries.CreateCurrentLookupHMACVersion(ctx, db.CreateCurrentLookupHMACVersionParams{
		Version:        version,
		KeyFingerprint: fingerprint[:],
	})
	if err != nil {
		return db.LookupHmacVersion{}, fmt.Errorf("activate lookup HMAC version %d: %w", version, err)
	}
	return created, nil
}

func (a Authority) Retire(ctx context.Context, tx pgx.Tx, version int32) (db.LookupHmacVersion, error) {
	if tx == nil {
		return db.LookupHmacVersion{}, errors.New("lookup HMAC maintenance transaction is required")
	}
	queries := db.New(tx)
	if err := queries.LockLookupHMACMaintenance(ctx); err != nil {
		return db.LookupHmacVersion{}, fmt.Errorf("lock lookup HMAC maintenance: %w", err)
	}
	rows, err := queries.LockLookupHMACVersionsForMaintenance(ctx)
	if err != nil {
		return db.LookupHmacVersion{}, fmt.Errorf("lock lookup HMAC versions: %w", err)
	}
	if _, err := a.selectRows(activeRows(rows)); err != nil {
		return db.LookupHmacVersion{}, err
	}
	var target *db.LookupHmacVersion
	for index := range rows {
		if rows[index].Version == version {
			target = &rows[index]
			break
		}
	}
	if target == nil {
		return db.LookupHmacVersion{}, fmt.Errorf("lookup HMAC version %d is not registered", version)
	}
	if target.RetiredAt.Valid {
		return db.LookupHmacVersion{}, fmt.Errorf("lookup HMAC version %d is retired", version)
	}
	if target.IsCurrent {
		return db.LookupHmacVersion{}, fmt.Errorf("lookup HMAC version %d is current", version)
	}
	fingerprint, err := a.keys.Fingerprint(version)
	if err != nil {
		return db.LookupHmacVersion{}, err
	}
	if !equalFingerprint(fingerprint, target.KeyFingerprint) {
		return db.LookupHmacVersion{}, fmt.Errorf("lookup HMAC version %d has different key bytes", version)
	}
	retired, err := queries.RetireLookupHMACVersion(ctx, version)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return db.LookupHmacVersion{}, fmt.Errorf("lookup HMAC version %d is still referenced", version)
		}
		return db.LookupHmacVersion{}, fmt.Errorf("retire lookup HMAC version %d: %w", version, err)
	}
	return retired, nil
}

func (a Authority) selectRows(rows []db.LookupHmacVersion) (Selection, error) {
	versions := make([]Version, 0, len(rows))
	for _, row := range rows {
		versions = append(versions, Version{
			Number:      row.Version,
			Fingerprint: row.KeyFingerprint,
			Current:     row.IsCurrent,
		})
	}
	selection, err := a.keys.Select(versions)
	if err != nil {
		return Selection{}, fmt.Errorf("validate lookup HMAC authority: %w", err)
	}
	return selection, nil
}

func activeRows(rows []db.LookupHmacVersion) []db.LookupHmacVersion {
	active := make([]db.LookupHmacVersion, 0, len(rows))
	for _, row := range rows {
		if !row.RetiredAt.Valid {
			active = append(active, row)
		}
	}
	return active
}

func equalFingerprint(expected [32]byte, actual []byte) bool {
	return hmac.Equal(expected[:], actual)
}
