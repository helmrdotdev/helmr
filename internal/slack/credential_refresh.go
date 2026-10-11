package slack

import (
	"context"
	"errors"
	"net/url"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/jackc/pgx/v5"
)

type refreshClaim struct {
	installation, org, attempt uuid.UUID
	revision                   int64
	authorized, deadline       time.Time
	app, team, bot             string
	bundle                     credentialBundle
}

// claimRefresh records possible consumption before HTTP. Another process never
// repeats this attempt: after its deadline, only explicit reauthorization repairs
// an unrecoverable replacement. No database transaction spans the exchange.
func (s *CredentialStore) claimRefresh(ctx context.Context, installation uuid.UUID) (*refreshClaim, error) {
	var claim *refreshClaim
	err := db.RunTx(ctx, s.pool, func(tx pgx.Tx) error {
		c := refreshClaim{installation: installation}
		var ciphertext, nonce []byte
		var attempt *uuid.UUID
		var expired, ready bool
		err := tx.QueryRow(ctx, `SELECT organization_id,credential_revision,authorized_at,app_id,team_id,bot_user_id,credential_ciphertext,credential_nonce,
 refresh_attempt_id,COALESCE(refresh_deadline<=clock_timestamp(),false),
 disconnected_at IS NULL AND authorization_lost_at IS NULL AND COALESCE(credential_expires_at<=clock_timestamp()+interval '5 minutes',false) AND refresh_next_at<=clock_timestamp()
 FROM slack_installations WHERE id=$1 FOR NO KEY UPDATE`, installation).Scan(&c.org, &c.revision, &c.authorized, &c.app, &c.team, &c.bot, &ciphertext, &nonce, &attempt, &expired, &ready)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		if attempt != nil {
			if expired {
				return loseRefresh(ctx, tx, installation, *attempt, c.revision, c.authorized, "refresh_replacement_unrecoverable")
			}
			return nil
		}
		if !ready {
			return nil
		}
		c.bundle, err = s.open(c.org, installation, c.revision, ciphertext, nonce)
		if err != nil || c.bundle.RefreshToken == "" {
			if _, err = tx.Exec(ctx, `UPDATE slack_installations SET refresh_error='credential_unavailable' WHERE id=$1`, installation); err != nil {
				return err
			}
			return recordAuthorizationLoss(ctx, tx, installation, c.revision, c.authorized)
		}
		c.attempt = uuid.NewV7()
		if err = tx.QueryRow(ctx, `UPDATE slack_installations SET refresh_attempt_id=$2,refresh_deadline=clock_timestamp()+interval '60 seconds',refresh_error=NULL WHERE id=$1 RETURNING refresh_deadline`, installation, c.attempt).Scan(&c.deadline); err != nil {
			return err
		}
		claim = &c
		return nil
	})
	if err != nil && claim != nil {
		// A lost claim-commit acknowledgment must be resolved before issuing HTTP.
		// Only this exact durable attempt can be recovered by its original owner.
		var confirmed bool
		recoveryErr := db.RunTx(ctx, s.pool, func(tx pgx.Tx) error {
			return tx.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM slack_installations WHERE id=$1 AND refresh_attempt_id=$2 AND credential_revision=$3 AND authorized_at=$4 AND refresh_deadline>clock_timestamp() AND disconnected_at IS NULL AND authorization_lost_at IS NULL)`, claim.installation, claim.attempt, claim.revision, claim.authorized).Scan(&confirmed)
		})
		if recoveryErr == nil && confirmed {
			return claim, nil
		}
	}
	if err != nil {
		return nil, err
	}
	return claim, nil
}

// Refresh resolves one due installation. Retrying persistence keeps the same
// replacement and attempt; it never issues a second OAuth exchange.
func (s *CredentialStore) Refresh(ctx context.Context, installation uuid.UUID, oauth *OAuthClient) (bool, error) {
	if oauth == nil {
		return false, errors.New("the Slack OAuth client is required")
	}
	claim, err := s.claimRefresh(ctx, installation)
	if err != nil || claim == nil {
		return false, err
	}
	// Once claimed, exchange and persistence share the durable deadline even if
	// the caller leaves. Graceful shutdown joins the owner rather than cancelling
	// a request that may already have consumed the one-use refresh token.
	attemptCtx, cancel := context.WithDeadline(context.WithoutCancel(ctx), claim.deadline)
	defer cancel()
	result := oauth.exchange(attemptCtx, url.Values{"grant_type": {"refresh_token"}, "refresh_token": {claim.bundle.RefreshToken}})
	if result.code == "" && ((result.grant.app != "" && result.grant.app != claim.app) || (result.grant.team != "" && result.grant.team != claim.team) || (result.grant.bot != "" && result.grant.bot != claim.bot)) {
		result = oauthResult{code: "oauth_identity_mismatch"}
	}
	// The owner keeps the replacement in memory through transient database errors.
	// A subsequent process can recognize an already committed replacement by attempt.
	for {
		err = s.finishRefresh(attemptCtx, *claim, result)
		if err == nil {
			return true, nil
		}
		timer := time.NewTimer(100 * time.Millisecond)
		select {
		case <-attemptCtx.Done():
			timer.Stop()
			return true, err
		case <-timer.C:
		}
	}
}

func (s *CredentialStore) finishRefresh(ctx context.Context, c refreshClaim, result oauthResult) error {
	return db.RunTx(ctx, s.pool, func(tx pgx.Tx) error {
		var revision int64
		var attempt, last *uuid.UUID
		var eligible bool
		err := tx.QueryRow(ctx, `SELECT credential_revision,refresh_attempt_id,last_refresh_attempt_id,
 disconnected_at IS NULL AND authorization_lost_at IS NULL AND authorized_at=$2 AND COALESCE(refresh_deadline>clock_timestamp(),false)
 FROM slack_installations WHERE id=$1 FOR NO KEY UPDATE`, c.installation, c.authorized).Scan(&revision, &attempt, &last, &eligible)
		if err != nil {
			return err
		}
		if last != nil && *last == c.attempt && revision == c.revision+1 {
			return nil
		}
		if revision != c.revision || attempt == nil || *attempt != c.attempt {
			return nil
		}
		if !eligible {
			return loseRefresh(ctx, tx, c.installation, c.attempt, c.revision, c.authorized, "refresh_replacement_unrecoverable")
		}
		if result.retryAfter > 0 {
			_, err = tx.Exec(ctx, `UPDATE slack_installations SET refresh_attempt_id=NULL,refresh_deadline=NULL,refresh_next_at=clock_timestamp()+$3*interval '1 second',refresh_error='oauth_rate_limited' WHERE id=$1 AND refresh_attempt_id=$2`, c.installation, c.attempt, result.retryAfter.Seconds())
			return err
		}
		if result.code != "" {
			return loseRefresh(ctx, tx, c.installation, c.attempt, c.revision, c.authorized, result.code)
		}
		ciphertext, nonce, err := s.seal(c.org, c.installation, c.revision+1, result.grant.bundle)
		if err != nil {
			return loseRefresh(ctx, tx, c.installation, c.attempt, c.revision, c.authorized, "credential_replacement_invalid")
		}
		tag, err := tx.Exec(ctx, `UPDATE slack_installations SET credential_revision=credential_revision+1,credential_ciphertext=$3,credential_nonce=$4,credential_expires_at=$5,
 refresh_attempt_id=NULL,refresh_deadline=NULL,last_refresh_attempt_id=$2,refresh_error=NULL,refresh_next_at=clock_timestamp()
 WHERE id=$1 AND refresh_attempt_id=$2 AND refresh_deadline>clock_timestamp()`, c.installation, c.attempt, ciphertext, nonce, result.grant.bundle.ExpiresAt)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return loseRefresh(ctx, tx, c.installation, c.attempt, c.revision, c.authorized, "refresh_replacement_unrecoverable")
		}
		return nil
	})
}

func loseRefresh(ctx context.Context, tx pgx.Tx, installation, attempt uuid.UUID, revision int64, authorized time.Time, code string) error {
	tag, err := tx.Exec(ctx, `UPDATE slack_installations SET refresh_attempt_id=NULL,refresh_deadline=NULL,refresh_error=$3
 WHERE id=$1 AND refresh_attempt_id=$2 AND credential_revision=$4 AND authorized_at=$5 AND disconnected_at IS NULL`, installation, attempt, code, revision, authorized)
	if err != nil || tag.RowsAffected() == 0 {
		return err
	}
	return recordAuthorizationLoss(ctx, tx, installation, revision, authorized)
}
