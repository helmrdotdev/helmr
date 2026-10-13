package computer

import (
	"context"
	"errors"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/disk"
	"github.com/jackc/pgx/v5"
)

// Image is the immutable initial image selected for one Computer.
// PreparationID is the producing attempt, which can differ from the Computer's
// original demand receipt after the permitted sequential replacement.
type Image struct {
	ID           uuid.UUID
	ImageBuildID uuid.UUID
	Sequence     int64
	Root         disk.VersionRoot
}

// Aliases p and spec identify a published build and its immutable specification.
// Pinning and payload retention use the same current-Secret eligibility rule.
const usableImageSecretsSQL = `NOT EXISTS (SELECT 1 FROM secret_bindings binding WHERE binding.environment_id=spec.environment_id AND binding.image_spec_id=spec.id AND NOT EXISTS (
   SELECT 1 FROM secret_exposures x JOIN secrets s ON (s.environment_id,s.id)=(x.environment_id,x.secret_id)
   WHERE x.environment_id=p.environment_id AND x.image_build_id=p.id AND x.secret_id=binding.secret_id
     AND s.status='active' AND s.current_version_id=x.version_id))
 AND NOT EXISTS (SELECT 1 FROM secret_exposures x JOIN secrets s ON (s.environment_id,s.id)=(x.environment_id,x.secret_id)
   WHERE x.environment_id=p.environment_id AND x.image_build_id=p.id AND (s.status<>'active' OR s.current_version_id<>x.version_id))`

// PinImage resolves an admitted Computer's preparation without creating
// another Computer or changing its reservation. Selection serializes with Secret
// rotation/revocation. An existing pin is returned unchanged after rotation;
// execution authorization separately enforces revocation of its retained lineage.
func PinImage(ctx context.Context, pool db.TxBeginner, env, computer uuid.UUID) (Image, error) {
	var result Image
	if env == uuid.Nil() || computer == uuid.Nil() {
		return result, ErrInvalidInput
	}
	err := db.RunTx(ctx, pool, func(tx pgx.Tx) error {
		var err error
		result, err = PinAdmissionImage(ctx, tx, env, computer)
		return err
	})
	if err != nil {
		return Image{}, hideMissing(err)
	}
	return result, nil
}

// PinAdmissionImage participates in its caller's admission transaction. It acquires
// Environment and preparation owners before the Computer, so callers must invoke
// it before acquiring existing Session-root or Computer locks. An error leaves
// rollback responsibility with the caller.
func PinAdmissionImage(ctx context.Context, tx pgx.Tx, env, computer uuid.UUID) (Image, error) {
	var result Image
	var locked uuid.UUID
	if err := tx.QueryRow(ctx, `SELECT id FROM environments WHERE id=$1 FOR NO KEY UPDATE`, env).Scan(&locked); err != nil {
		return result, err
	}
	var spec *uuid.UUID
	if err := tx.QueryRow(ctx, `SELECT image_spec_id FROM computers WHERE environment_id=$1 AND id=$2`, env, computer).Scan(&spec); err != nil {
		return result, err
	}
	if spec == nil {
		return result, ErrNotReady
	}
	if err := LockImageSpecSecrets(ctx, tx, env, *spec); err != nil {
		return result, err
	}
	if err := LockImageSpec(ctx, tx, env, *spec); err != nil {
		return result, err
	}
	var pinned, attached *uuid.UUID
	var live bool
	var maxAge *int64
	if err := tx.QueryRow(ctx, `SELECT image_id,image_build_id,initial_root_digest IS NULL AND preparation_failed_at IS NULL AND deleted_at IS NULL AND preparation_deadline_at>clock_timestamp(),preparation_max_age_ms
 FROM computers WHERE environment_id=$1 AND id=$2 AND image_spec_id=$3 FOR NO KEY UPDATE`, env, computer, *spec).Scan(&pinned, &attached, &live, &maxAge); err != nil {
		return result, err
	}
	if pinned != nil {
		var err error
		result, _, err = readComputerImage(ctx, tx, env, *spec, *pinned)
		return result, err
	}
	if !live {
		return result, ErrNotReady
	}
	var image uuid.UUID
	err := tx.QueryRow(ctx, `SELECT i.id FROM images i
 JOIN image_builds p ON (p.environment_id,p.id)=(i.environment_id,i.image_build_id)
 JOIN image_specs spec ON (spec.environment_id,spec.id)=(i.environment_id,i.image_spec_id)
 WHERE i.environment_id=$1 AND i.image_spec_id=$2 AND i.root_id IS NOT NULL AND p.status='succeeded'
 AND ($4::uuid IS NULL OR p.id=$4 OR p.successor_of=$4)
 AND ($3::bigint IS NULL OR extract(epoch FROM clock_timestamp()-i.published_at)*1000<=$3)
 AND `+usableImageSecretsSQL+`
 ORDER BY i.seq DESC LIMIT 1`, env, *spec, maxAge, attached).Scan(&image)
	if errors.Is(err, pgx.ErrNoRows) {
		return result, ErrNotReady
	}
	if err != nil {
		return result, err
	}
	var rootID uuid.UUID
	result, rootID, err = readComputerImage(ctx, tx, env, *spec, image)
	if err != nil {
		return result, err
	}
	identity, err := result.Root.Digest()
	if err != nil {
		return result, ErrConflict
	}
	// Check the deadline again after selection and storage validation. No SQL
	// lock wait may turn an expired waiter into an initialized Computer.
	changed, err := tx.Exec(ctx, `UPDATE computers SET image_id=$3,initial_root_id=$4,initial_root_digest=decode(substring($5::text from 8),'hex') WHERE environment_id=$1 AND id=$2 AND preparation_deadline_at>clock_timestamp()`, env, computer, image, rootID, identity)
	if err != nil {
		return result, err
	}
	if changed.RowsAffected() != 1 {
		return result, ErrNotReady
	}
	return result, nil
}

// Lock even revoked owners: receipt reads remain valid, while eligibility denies
// new pins. Missing declared owners are rejected by the complete exposure query.
func LockImageSpecSecrets(ctx context.Context, tx pgx.Tx, env, spec uuid.UUID) error {
	rows, err := tx.Query(ctx, `SELECT s.id FROM secrets s WHERE s.environment_id=$1 AND s.id IN (
 SELECT secret_id FROM secret_bindings WHERE environment_id=$1 AND image_spec_id=$2) ORDER BY s.id FOR SHARE`, env, spec)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var id uuid.UUID
		if err = rows.Scan(&id); err != nil {
			return err
		}
	}
	return rows.Err()
}

func readComputerImage(ctx context.Context, tx pgx.Tx, env, spec, image uuid.UUID) (Image, uuid.UUID, error) {
	result := Image{ID: image}
	var rootID uuid.UUID
	var captured string
	err := tx.QueryRow(ctx, `SELECT i.image_build_id,i.seq,i.root_id,p.capture_root,`+DiskRootColumns+` FROM images i
 JOIN disk_roots r ON (r.environment_id,r.id)=(i.environment_id,i.root_id)
 JOIN image_builds p ON (p.environment_id,p.id)=(i.environment_id,i.image_build_id)
 WHERE i.environment_id=$1 AND i.image_spec_id=$2 AND i.id=$3 AND p.status='succeeded'`, env, spec, image).Scan(append([]any{&result.ImageBuildID, &result.Sequence, &rootID, &captured}, DiskRootFields(&result.Root)...)...)
	if err != nil {
		return Image{}, uuid.Nil(), err
	}
	identity, err := result.Root.Digest()
	if err != nil || identity != captured {
		return Image{}, uuid.Nil(), ErrConflict
	}
	return result, rootID, nil
}

// Execution checks may overlap a revoke and linearize before it. A check after
// the revoke commits always denies, without waiting for lifecycle reconciliation.
// This is not physical-stop evidence and is not used to block cleanup controls.
func RequireSecretsNotRevoked(ctx context.Context, tx pgx.Tx, env, computer uuid.UUID) error {
	var revoked bool
	if err := tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM (`+db.RevokedComputerSecretsSQL+`) revoked_secrets WHERE environment_id=$1 AND computer_id=$2)`, env, computer).Scan(&revoked); err != nil {
		return err
	}
	if revoked {
		return ErrDenied
	}
	return nil
}
