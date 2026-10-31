package identity

import (
	"context"
	"github.com/helmrdotdev/helmr/internal/db"
	"time"
	"uuid"
)

func enqueueSecurityNotification(ctx context.Context, q db.DBTX, event, recipient string) error {
	if recipient == "" {
		return nil
	}
	_, err := q.Exec(ctx, `INSERT INTO account_security_notifications(id,event,recipient) VALUES($1,$2,$3)`, uuid.NewV7(), event, recipient)
	return err
}

type SecurityNotification struct {
	ID, ClaimID      uuid.UUID
	Event, Recipient string
}

// ClaimSecurityNotification commits a bounded lease before any provider call.
// A process crash leaves the message available after the lease expires.
func ClaimSecurityNotification(ctx context.Context, q db.DBTX) (SecurityNotification, bool, error) {
	n := SecurityNotification{ClaimID: uuid.NewV7()}
	err := q.QueryRow(ctx, `UPDATE account_security_notifications SET claim_id=$1,lease_until=clock_timestamp()+interval '2 minutes',attempts=attempts+1
 WHERE id=(SELECT id FROM account_security_notifications WHERE next_attempt_at<=clock_timestamp() AND (lease_until IS NULL OR lease_until<=clock_timestamp()) ORDER BY next_attempt_at,id FOR UPDATE SKIP LOCKED LIMIT 1)
 RETURNING id,event,recipient`, n.ClaimID).Scan(&n.ID, &n.Event, &n.Recipient)
	if isNoRows(err) {
		return n, false, nil
	}
	return n, err == nil, err
}

func CompleteSecurityNotification(ctx context.Context, q db.DBTX, n SecurityNotification, delivered bool) error {
	if delivered {
		_, err := q.Exec(ctx, `DELETE FROM account_security_notifications WHERE id=$1 AND claim_id=$2`, n.ID, n.ClaimID)
		return err
	}
	_, err := q.Exec(ctx, `UPDATE account_security_notifications SET next_attempt_at=clock_timestamp()+LEAST(attempts,60)*interval '1 minute',claim_id=NULL,lease_until=NULL WHERE id=$1 AND claim_id=$2`, n.ID, n.ClaimID)
	return err
}

func (n SecurityNotification) Text() string {
	description := map[string]string{
		"identity.email_changed":        "Your account email address was changed.",
		"identity.credential_added":     "A sign-in method was added to your account.",
		"identity.credential_removed":   "A sign-in method was removed from your account.",
		"identity.access_revoked":       "All human access to your account was revoked. Sign in again to continue.",
		"identity.other_access_revoked": "Your account was signed out of every other browser, CLI and Slack.",
		"identity.account_repaired":     "A deployment operator completed supervised account recovery and revoked previous access.",
	}[n.Event]
	return description + "\nIf you did not request this change, contact your deployment administrator."
}

const SecurityNotificationDeliveryTimeout = 30 * time.Second
