package agent

import (
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/disk"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/jackc/pgx/v5/pgtype"
)

// diskRootColumns selects one disk_roots row, aliased r, in the field
// order of diskRootFields. Its digest stays the digest of the rebuilt
// disk.VersionRoot, so storage form and identity are independent.
const diskRootColumns = "r.format_version,r.logical_bytes,r.pack_digest,r.pack_size_bytes,r.pack_rank,r.page_digest,r.page_salt,r.page_key_id::text,r.page_kind,r.page_count,r.page_size_bytes,r.page_offset"

func diskRootFields(root *disk.VersionRoot) []any {
	return []any{&root.FormatVersion, &root.LogicalBytes, &root.Pack.Digest, &root.Pack.SizeBytes, &root.Pack.Rank,
		&root.Page.Digest, &root.Page.Salt, &root.Page.KeyID, &root.Page.Kind, &root.Page.Count, &root.Page.SizeBytes, &root.Offset}
}

func retainDiskRootParams(environment pgtype.UUID, root disk.VersionRoot) (db.RetainDiskRootParams, error) {
	if err := root.Validate(root.LogicalBytes); err != nil {
		return db.RetainDiskRootParams{}, err
	}
	key, err := uuid.Parse(root.Page.KeyID)
	if err != nil {
		return db.RetainDiskRootParams{}, err
	}
	return db.RetainDiskRootParams{
		ID: pgvalue.NewUUIDv7(), EnvironmentID: environment,
		FormatVersion: int32(root.FormatVersion), LogicalBytes: root.LogicalBytes,
		PackDigest: root.Pack.Digest, PackSizeBytes: root.Pack.SizeBytes, PackRank: int32(root.Pack.Rank),
		PageDigest: root.Page.Digest, PageSalt: root.Page.Salt, PageKeyID: pgvalue.UUID(key),
		PageKind: int32(root.Page.Kind), PageCount: int32(root.Page.Count), PageSizeBytes: root.Page.SizeBytes,
		PageOffset: root.Offset,
	}, nil
}
