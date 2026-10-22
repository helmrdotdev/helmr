package slack

import (
	"context"
	"errors"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/jackc/pgx/v5"
)

const socketLeaseDuration = 30 * time.Second

var errSocketClaimLost = errors.New("socket ownership changed")

type socketClaim struct {
	Registration uuid.UUID
	Installation uuid.UUID
	ID           uuid.UUID
	Revision     int64
	AppID        string
	TeamID       string
	Token        string
}

// acquireSocket locks registration before installation/connection, matching
// credential replacement and disconnection. Another replica skips a busy app.
func (s *CredentialStore) acquireSocket(ctx context.Context, replica uuid.UUID) (*socketClaim, error) {
	var claim *socketClaim
	err := db.RunTx(ctx, s.pool, func(tx pgx.Tx) error {
		c := socketClaim{ID: uuid.NewV7()}
		var org uuid.UUID
		var ciphertext, nonce []byte
		err := tx.QueryRow(ctx, `SELECT r.id,r.org_id,r.app_id,r.credential_revision,r.credential_ciphertext,r.credential_nonce,i.id,i.team_id
 FROM slack_app_registrations r
 JOIN slack_installations i ON i.app_registration_id=r.id
 JOIN slack_connections p ON p.slack_app_registration_id=r.id AND p.slack_installation_id=i.id AND p.revoked_at IS NULL
 LEFT JOIN slack_socket_connections sc ON sc.app_registration_id=r.id
 WHERE r.transport='socket' AND r.retired_at IS NULL AND r.app_id IS NOT NULL AND r.credential_revision>0
 AND i.disconnected_at IS NULL AND i.authorization_lost_at IS NULL
 AND (sc.claim_id IS NULL OR sc.expires_at<=clock_timestamp())
 AND (sc.retry_at IS NULL OR sc.retry_at<=clock_timestamp() OR sc.credential_revision<>r.credential_revision)
 ORDER BY sc.retry_at NULLS FIRST,r.id LIMIT 1 FOR NO KEY UPDATE OF r SKIP LOCKED`).Scan(&c.Registration, &org, &c.AppID, &c.Revision, &ciphertext, &nonce, &c.Installation, &c.TeamID)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		var active bool
		if err = tx.QueryRow(ctx, `SELECT disconnected_at IS NULL AND authorization_lost_at IS NULL FROM slack_installations WHERE id=$1 FOR SHARE`, c.Installation).Scan(&active); err != nil {
			return err
		}
		if !active {
			return nil
		}
		tag, err := tx.Exec(ctx, `INSERT INTO slack_socket_connections(app_registration_id,installation_id,claim_id,replica_id,expires_at,credential_revision,state)
 VALUES($1,$2,$3,$4,clock_timestamp()+interval '30 seconds',$5,'connecting')
 ON CONFLICT(app_registration_id) DO UPDATE SET installation_id=EXCLUDED.installation_id,claim_id=EXCLUDED.claim_id,replica_id=EXCLUDED.replica_id,expires_at=EXCLUDED.expires_at,credential_revision=EXCLUDED.credential_revision,state='connecting',reason=NULL
 WHERE slack_socket_connections.claim_id IS NULL OR slack_socket_connections.expires_at<=clock_timestamp()`, c.Registration, c.Installation, c.ID, replica, c.Revision)
		if err != nil {
			return err
		}
		if tag.RowsAffected() != 1 {
			return nil
		}
		credentials, err := s.openApp(org, c.Registration, c.Revision, ciphertext, nonce)
		if err != nil {
			_, err = tx.Exec(ctx, `UPDATE slack_socket_connections SET state='action_required',reason='credential_unavailable',claim_id=NULL,replica_id=NULL,expires_at=NULL,retry_at=clock_timestamp()+interval '5 minutes' WHERE app_registration_id=$1 AND claim_id=$2`, c.Registration, c.ID)
			return err
		}
		c.Token = credentials.AppToken
		claim = &c
		return nil
	})
	return claim, err
}

// lockSocketClaim is used in receipt transactions and before renewal. Holding
// these shared locks prevents an authority or owner change during a receipt.
func lockSocketClaim(ctx context.Context, tx pgx.Tx, c socketClaim) error {
	var id uuid.UUID
	if err := tx.QueryRow(ctx, `SELECT id FROM slack_app_registrations WHERE id=$1 AND transport='socket' AND retired_at IS NULL AND credential_revision=$2 FOR SHARE`, c.Registration, c.Revision).Scan(&id); err != nil {
		return socketClaimError(err)
	}
	if err := tx.QueryRow(ctx, `SELECT id FROM slack_installations WHERE id=$1 AND app_registration_id=$2 AND disconnected_at IS NULL AND authorization_lost_at IS NULL FOR SHARE`, c.Installation, c.Registration).Scan(&id); err != nil {
		return socketClaimError(err)
	}
	if err := tx.QueryRow(ctx, `SELECT app_registration_id FROM slack_socket_connections WHERE app_registration_id=$1 AND installation_id=$2 AND claim_id=$3 AND credential_revision=$4 AND expires_at>clock_timestamp() FOR UPDATE`, c.Registration, c.Installation, c.ID, c.Revision).Scan(&id); err != nil {
		return socketClaimError(err)
	}
	return nil
}
func socketClaimError(err error) error {
	if errors.Is(err, pgx.ErrNoRows) {
		return errSocketClaimLost
	}
	return err
}

func (s *CredentialStore) renewSocket(ctx context.Context, c socketClaim) error {
	return db.RunTx(ctx, s.pool, func(tx pgx.Tx) error {
		if err := lockSocketClaim(ctx, tx, c); err != nil {
			return err
		}
		tag, err := tx.Exec(ctx, `UPDATE slack_socket_connections SET expires_at=clock_timestamp()+interval '30 seconds' WHERE app_registration_id=$1 AND claim_id=$2 AND credential_revision=$3 AND expires_at>clock_timestamp()`, c.Registration, c.ID, c.Revision)
		if err == nil && tag.RowsAffected() != 1 {
			return errSocketClaimLost
		}
		return err
	})
}

func (s *CredentialStore) observeSocket(ctx context.Context, c socketClaim, received bool) error {
	return db.RunTx(ctx, s.pool, func(tx pgx.Tx) error {
		if err := lockSocketClaim(ctx, tx, c); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `UPDATE slack_socket_connections SET state='connected',reason=NULL,last_connected_at=CASE WHEN $3 THEN last_connected_at ELSE clock_timestamp() END,last_received_at=CASE WHEN $3 THEN clock_timestamp() ELSE last_received_at END WHERE app_registration_id=$1 AND claim_id=$2`, c.Registration, c.ID, received)
		return err
	})
}

func (s *CredentialStore) releaseSocket(ctx context.Context, c socketClaim, state, reason string, retry time.Duration) error {
	return db.RunTx(ctx, s.pool, func(tx pgx.Tx) error {
		_, err := tx.Exec(ctx, `UPDATE slack_socket_connections SET claim_id=NULL,replica_id=NULL,expires_at=NULL,state=$4,reason=NULLIF($5,''),retry_at=clock_timestamp()+$6::bigint*interval '1 millisecond' WHERE app_registration_id=$1 AND claim_id=$2 AND credential_revision=$3`, c.Registration, c.ID, c.Revision, state, reason, retry.Milliseconds())
		return err
	})
}

// socketReceiptDB applies connection fencing to every existing receipt/observation
// transaction without moving application authorization into the socket supervisor.
type socketReceiptDB struct {
	pool  db.TxBeginner
	claim socketClaim
}

func (b socketReceiptDB) Begin(ctx context.Context) (pgx.Tx, error) {
	tx, err := b.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	if err = lockSocketClaim(ctx, tx, b.claim); err != nil {
		_ = tx.Rollback(context.WithoutCancel(ctx))
		return nil, err
	}
	return &socketReceiptTx{Tx: tx, claim: b.claim}, nil
}

type socketReceiptTx struct {
	pgx.Tx
	claim socketClaim
}

func (t *socketReceiptTx) Commit(ctx context.Context) error {
	// Recheck expiry immediately before commit; another owner cannot interleave
	// because the socket row is still locked from Begin.
	if err := lockSocketClaim(ctx, t.Tx, t.claim); err != nil {
		return err
	}
	return t.Tx.Commit(ctx)
}
