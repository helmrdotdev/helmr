-- name: RetainComputerDiskRoot :one
INSERT INTO computer_disk_roots(id,environment_id,format_version,logical_bytes,pack_digest,pack_size_bytes,pack_rank,
    page_digest,page_salt,page_key_id,page_kind,page_count,page_size_bytes,page_offset)
VALUES(sqlc.arg(id),sqlc.arg(environment_id),sqlc.arg(format_version),sqlc.arg(logical_bytes),sqlc.arg(pack_digest),
    sqlc.arg(pack_size_bytes),sqlc.arg(pack_rank),sqlc.arg(page_digest),sqlc.arg(page_salt),sqlc.arg(page_key_id),
    sqlc.arg(page_kind),sqlc.arg(page_count),sqlc.arg(page_size_bytes),sqlc.arg(page_offset))
ON CONFLICT (environment_id,pack_digest,page_offset) DO UPDATE
 SET pack_digest=EXCLUDED.pack_digest
 WHERE (computer_disk_roots.format_version,computer_disk_roots.logical_bytes,computer_disk_roots.pack_size_bytes,
        computer_disk_roots.pack_rank,computer_disk_roots.page_digest,computer_disk_roots.page_salt,
        computer_disk_roots.page_key_id,computer_disk_roots.page_kind,computer_disk_roots.page_count,
        computer_disk_roots.page_size_bytes)
     = (EXCLUDED.format_version,EXCLUDED.logical_bytes,EXCLUDED.pack_size_bytes,EXCLUDED.pack_rank,
        EXCLUDED.page_digest,EXCLUDED.page_salt,EXCLUDED.page_key_id,EXCLUDED.page_kind,EXCLUDED.page_count,
        EXCLUDED.page_size_bytes)
RETURNING id;
