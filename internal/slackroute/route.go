// Package slackroute owns the Slack route authority: connections, channel
// bindings and thread membership, and the locks that gate them. Lock order:
// registration, installation, connection, channel binding (step 3 of the
// global order in package pglock), before Environment or Session owners.
package slackroute

import (
	"context"
	"errors"
	"uuid"

	"github.com/jackc/pgx/v5"
)

var (
	ErrTargetNotPublished  = errors.New("target Agent has no connected Slack app")
	ErrConversationChanged = errors.New("conversation configuration changed during validation; retry the same request")
)

// RequiredScopes returns the bot scopes needed by connected Slack connections.
// OAuth, app manifests and transactional admission use the same scope set.
func RequiredScopes() []string {
	return []string{"app_mentions:read", "channels:read", "channels:history", "groups:read", "groups:history", "chat:write", "assistant:write"}
}

// LockConnections takes registration, installation, then connection gates
// before Environment or Session owners. Immutable routes never confer access.
func LockConnections(ctx context.Context, tx pgx.Tx, env uuid.UUID, ids []uuid.UUID) (bool, error) {
	return lockConnections(ctx, tx, env, ids, false)
}

func lockConnections(ctx context.Context, tx pgx.Tx, env uuid.UUID, ids []uuid.UUID, reserve bool) (bool, error) {
	if len(ids) == 0 {
		return true, nil
	}
	type connection struct {
		registration uuid.UUID
		installation *uuid.UUID
	}
	observed := map[uuid.UUID]connection{}
	rows, err := tx.Query(ctx, `SELECT id,slack_app_registration_id,slack_installation_id FROM slack_connections WHERE environment_id=$1 AND id=ANY($2::uuid[])`, env, ids)
	if err != nil {
		return false, err
	}
	for rows.Next() {
		var id uuid.UUID
		var c connection
		if err = rows.Scan(&id, &c.registration, &c.installation); err != nil {
			rows.Close()
			return false, err
		}
		observed[id] = c
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		return false, err
	}
	registrations, installations := []uuid.UUID{}, []uuid.UUID{}
	for _, c := range observed {
		registrations = append(registrations, c.registration)
		if c.installation != nil {
			installations = append(installations, *c.installation)
		}
	}
	installationLock := "FOR SHARE"
	// Delivery owners also reserve installation-wide pacing. Take the write gate
	// here, before connection/Environment/Session locks, to avoid lock upgrades.
	if reserve {
		installationLock = "FOR NO KEY UPDATE"
	}
	for _, gate := range []struct {
		query string
		ids   []uuid.UUID
	}{
		{`SELECT id FROM slack_app_registrations WHERE id=ANY($1::uuid[]) ORDER BY id FOR SHARE`, registrations},
		{`SELECT id FROM slack_installations WHERE id=ANY($1::uuid[]) ORDER BY id ` + installationLock, installations},
	} {
		rows, err = tx.Query(ctx, gate.query, gate.ids)
		if err != nil {
			return false, err
		}
		for rows.Next() {
			var id uuid.UUID
			if err = rows.Scan(&id); err != nil {
				rows.Close()
				return false, err
			}
		}
		rows.Close()
		if err = rows.Err(); err != nil {
			return false, err
		}
	}
	rows, err = tx.Query(ctx, `SELECT p.id,p.slack_app_registration_id,p.slack_installation_id,
 p.revoked_at IS NULL AND r.retired_at IS NULL AND r.credential_ciphertext IS NOT NULL AND i.id IS NOT NULL
 AND i.disconnected_at IS NULL AND i.authorization_lost_at IS NULL AND i.credential_ciphertext IS NOT NULL
 AND r.org_id=e.org_id AND i.org_id=e.org_id AND r.app_id=i.app_id AND i.granted_scopes @> $3::text[]
 FROM slack_connections p JOIN slack_app_registrations r ON r.id=p.slack_app_registration_id JOIN environments e ON e.id=p.environment_id
 LEFT JOIN slack_installations i ON i.id=p.slack_installation_id
 WHERE p.environment_id=$1 AND p.id=ANY($2::uuid[]) ORDER BY p.id FOR SHARE OF p`, env, ids, RequiredScopes())
	if err != nil {
		return false, err
	}
	defer rows.Close()
	seen := map[uuid.UUID]bool{}
	active := true
	for rows.Next() {
		var id, registration uuid.UUID
		var installation *uuid.UUID
		var eligible bool
		if err = rows.Scan(&id, &registration, &installation, &eligible); err != nil {
			return false, err
		}
		old, ok := observed[id]
		if !ok || old.registration != registration || (old.installation == nil) != (installation == nil) || installation != nil && *old.installation != *installation {
			return false, ErrConversationChanged
		}
		seen[id] = true
		active = active && eligible
	}
	if err = rows.Err(); err != nil {
		return false, err
	}
	for _, id := range ids {
		active = active && seen[id]
	}
	return active, nil
}

func LockChannels(ctx context.Context, tx pgx.Tx, env uuid.UUID, ids []uuid.UUID) (bool, error) {
	return lockChannels(ctx, tx, env, ids, false)
}

// LockChannelsForDelivery uses the same authority and lock order, with a
// write gate for callers that reserve installation-wide delivery/read pacing.
func LockChannelsForDelivery(ctx context.Context, tx pgx.Tx, env uuid.UUID, ids []uuid.UUID) (bool, error) {
	return lockChannels(ctx, tx, env, ids, true)
}

func lockChannels(ctx context.Context, tx pgx.Tx, env uuid.UUID, ids []uuid.UUID, reserve bool) (bool, error) {
	if len(ids) == 0 {
		return true, nil
	}
	rows, err := tx.Query(ctx, `SELECT id,connection_id FROM slack_channel_bindings WHERE environment_id=$1 AND id=ANY($2::uuid[])`, env, ids)
	if err != nil {
		return false, err
	}
	pubs := []uuid.UUID{}
	seen := map[uuid.UUID]bool{}
	for rows.Next() {
		var id, pub uuid.UUID
		if err = rows.Scan(&id, &pub); err != nil {
			rows.Close()
			return false, err
		}
		seen[id] = true
		pubs = append(pubs, pub)
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		return false, err
	}
	active, err := lockConnections(ctx, tx, env, pubs, reserve)
	if err != nil {
		return false, err
	}
	for _, id := range ids {
		active = active && seen[id]
	}
	return active, nil
}
