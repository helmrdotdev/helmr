-- name: RetainComputerDiskRoot :one
INSERT INTO computer_disk_roots(id,environment_id,locator)
VALUES(sqlc.arg(id),sqlc.arg(environment_id),sqlc.arg(locator))
ON CONFLICT (environment_id,root_pack_digest,root_page_offset) DO UPDATE
 SET locator=EXCLUDED.locator
 WHERE computer_disk_roots.locator=EXCLUDED.locator
RETURNING id;
