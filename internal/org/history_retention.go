package org

import (
	"context"
	"fmt"
	"github.com/helmrdotdev/helmr/internal/audit"
	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"uuid"
)

// UpdateProjectHistoryRetention changes the policy for all remaining history.
// Cleanup takes a shared lock on the same Project until its deletion commits.
func UpdateProjectHistoryRetention(ctx context.Context, database db.TxBeginner, m ManagingMember, projectID uuid.UUID, seconds *int64) error {
	orgID := m.OrgID
	if seconds != nil && *seconds <= 0 {
		return invalidInput("history retention must be positive")
	}
	return db.RunTx(ctx, database, func(tx pgx.Tx) error {
		if err := lockAccessMembers(ctx, tx, &m, auth.PermissionProjectsUpdate); err != nil {
			return err
		}
		var old pgtype.Int8
		if err := tx.QueryRow(ctx, `SELECT history_retention_seconds FROM projects WHERE org_id=$1 AND id=$2 FOR NO KEY UPDATE`, orgID, projectID).Scan(&old); isNoRows(err) {
			return ErrProjectNotFound
		} else if err != nil {
			return fmt.Errorf("lock project retention: %w", err)
		}
		if (seconds == nil && !old.Valid) || (seconds != nil && old.Valid && *seconds == old.Int64) {
			return nil
		}
		// Shortening can delete remaining history on the next cleanup pass, so
		// it needs recent proof; lengthening or disabling expiry deletes nothing.
		if seconds != nil && (!old.Valid || *seconds < old.Int64) {
			if err := auth.RequireRecentProof(m.Human); err != nil {
				return err
			}
		}
		if _, err := tx.Exec(ctx, `UPDATE projects SET history_retention_seconds=$3,updated_at=now() WHERE org_id=$1 AND id=$2`, orgID, projectID, seconds); err != nil {
			return fmt.Errorf("update project retention: %w", err)
		}
		var before *int64
		if old.Valid {
			before = &old.Int64
		}
		return audit.Append(ctx, tx, m.AuditPolicy, audit.Event{Evidence: m.Actor, OrgID: orgID, ProjectID: projectID, Action: "project.retention_changed", TargetKind: "project", TargetID: projectID, Details: audit.RetentionChangedDetails{OldRetentionSeconds: before, NewRetentionSeconds: seconds}})
	})
}
