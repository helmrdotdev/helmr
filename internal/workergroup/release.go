package workergroup

import (
	"context"
	"errors"
	"fmt"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/release"
	"github.com/jackc/pgx/v5"
)

// ErrReleaseNotPublished means the deployment has not published its release
// for worker hosts.
var ErrReleaseNotPublished = errors.New("the Control Plane's release is not published for worker hosts")

// ReleaseObject is one object of a published release, by its digest and size.
type ReleaseObject struct {
	Digest    string `json:"digest"`
	SizeBytes int64  `json:"size_bytes"`
}

// PublishedRelease is a release worker hosts install from: its signed index,
// the index's signature and the host assets the index lists.
type PublishedRelease struct {
	Version   string        `json:"version"`
	Index     ReleaseObject `json:"index"`
	Signature ReleaseObject `json:"signature"`
}

// RecordRelease records a release whose verified index, signature and host
// assets the deployment published to the platform store. Recording the same
// release again is a no-op; recording other bytes for a version is refused.
func RecordRelease(ctx context.Context, txb db.TxBeginner, verified release.Index, index, signature ReleaseObject) error {
	return db.RunTx(ctx, txb, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `INSERT INTO platform_releases(version,index_digest,index_size_bytes,signature_digest,signature_size_bytes)
 VALUES($1,$2,$3,$4,$5) ON CONFLICT (version) DO NOTHING`, verified.Version, index.Digest, index.SizeBytes, signature.Digest, signature.SizeBytes)
		if err != nil {
			return fmt.Errorf("record release %s: %w", verified.Version, err)
		}
		if tag.RowsAffected() == 0 {
			var same bool
			if err := tx.QueryRow(ctx, `SELECT index_digest=$2 FROM platform_releases WHERE version=$1`, verified.Version, index.Digest).Scan(&same); err != nil {
				return fmt.Errorf("read recorded release %s: %w", verified.Version, err)
			}
			if !same {
				return conflict("release " + verified.Version + " is already recorded with another index")
			}
			return nil
		}
		for _, name := range release.HostAssets {
			asset := verified.Assets[name]
			if _, err := tx.Exec(ctx, `INSERT INTO platform_release_objects(version,name,digest,size_bytes) VALUES($1,$2,$3,$4)`, verified.Version, name, asset.Digest, asset.SizeBytes); err != nil {
				return fmt.Errorf("record release %s object %s: %w", verified.Version, name, err)
			}
		}
		return nil
	})
}

// GetPublishedRelease returns the recorded release of version.
func GetPublishedRelease(ctx context.Context, q db.DBTX, version string) (PublishedRelease, error) {
	published := PublishedRelease{Version: version}
	err := q.QueryRow(ctx, `SELECT index_digest,index_size_bytes,signature_digest,signature_size_bytes FROM platform_releases WHERE version=$1`, version).
		Scan(&published.Index.Digest, &published.Index.SizeBytes, &published.Signature.Digest, &published.Signature.SizeBytes)
	if errors.Is(err, pgx.ErrNoRows) {
		return PublishedRelease{}, ErrReleaseNotPublished
	}
	if err != nil {
		return PublishedRelease{}, fmt.Errorf("read release %s: %w", version, err)
	}
	return published, nil
}

// IsReleaseObject reports whether digest is the index, signature or a host
// asset of the recorded release version.
func IsReleaseObject(ctx context.Context, q db.DBTX, version, digest string) (bool, error) {
	var found bool
	err := q.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM platform_release_objects WHERE version=$1 AND digest=$2)
 OR EXISTS(SELECT 1 FROM platform_releases WHERE version=$1 AND (index_digest=$2 OR signature_digest=$2))`, version, digest).Scan(&found)
	return found, err
}
