package slack

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"strings"
	"time"
	"unicode/utf8"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/jsoncanon"
	"github.com/jackc/pgx/v5"
)

var errGestureConflict = errors.New("slack gesture content conflicts with its receipt")
var errGestureInvalid = errors.New("invalid Slack gesture")

type inboundGesture struct {
	Installation          uuid.UUID
	Key, Actor            string
	OccurredAt, ExpiresAt time.Time
	Payload               json.RawMessage
}

type requestReceipt struct {
	ID     uuid.UUID
	Status string
}

// receiveGesture is called only after transport verification, explicit-gesture
// classification and installation-generation resolution. It persists identity
// before acknowledgment and never admits core work. Showing a historical result
// still requires the viewer's current identity and authorization.
func receiveGesture(ctx context.Context, pool db.TxBeginner, gesture inboundGesture) (requestReceipt, error) {
	var result requestReceipt
	if gesture.Installation == uuid.Nil() || len(gesture.Key) == 0 || len(gesture.Key) > 1024 || !utf8.ValidString(gesture.Key) || strings.ContainsRune(gesture.Key, 0) ||
		gesture.Actor == "" || len(gesture.Actor) > 128 || !utf8.ValidString(gesture.Actor) || strings.ContainsRune(gesture.Actor, 0) || gesture.OccurredAt.IsZero() || gesture.ExpiresAt.IsZero() {
		return result, errGestureInvalid
	}
	payload, err := jsoncanon.Transform(gesture.Payload)
	if err != nil || len(payload) > 1024*1024 {
		return result, errGestureInvalid
	}
	digest := gestureDigest(gesture.Actor, gesture.OccurredAt, payload)

	err = db.RunTx(ctx, pool, func(tx pgx.Tx) error {
		var active, sourceFresh, unexpired bool
		err := tx.QueryRow(ctx, `SELECT disconnected_at IS NULL AND authorization_lost_at IS NULL,
 $2>=authorized_at AND $2>=connected_at,$3>clock_timestamp() FROM slack_installations WHERE id=$1 FOR KEY SHARE`, gesture.Installation, gesture.OccurredAt, gesture.ExpiresAt).Scan(&active, &sourceFresh, &unexpired)
		if errors.Is(err, pgx.ErrNoRows) {
			return errGestureInvalid
		}
		if err != nil {
			return err
		}
		code := ""
		if !active {
			code = "installation_unavailable"
		} else if !sourceFresh {
			code = "stale_gesture"
		} else if !unexpired {
			code = "gesture_expired"
		}
		candidate := uuid.NewV7()
		_, err = tx.Exec(ctx, `INSERT INTO slack_requests(id,installation_id,request_key,request_digest,source_occurred_at,slack_user_id,payload,expires_at,status,finished_at,error)
 VALUES($1,$2,$3,$4,$5,$6,$7,$8,CASE WHEN $9='' THEN 'received' ELSE 'rejected' END,CASE WHEN $9='' THEN NULL ELSE clock_timestamp() END,NULLIF($9,''))
 ON CONFLICT(installation_id,request_key) DO NOTHING`, candidate, gesture.Installation, gesture.Key, digest[:], gesture.OccurredAt, gesture.Actor, payload, gesture.ExpiresAt, code)
		if err != nil {
			return err
		}
		var retained []byte
		if err = tx.QueryRow(ctx, `SELECT id,status,request_digest FROM slack_requests WHERE installation_id=$1 AND request_key=$2 FOR UPDATE`, gesture.Installation, gesture.Key).Scan(&result.ID, &result.Status, &retained); err != nil {
			return err
		}
		if !bytes.Equal(retained, digest[:]) {
			return errGestureConflict
		}
		return nil
	})
	return result, err
}

// gestureDigest covers normalized source content, independently of the delivery
// event ID. The caller supplies canonical JSON so retries preserve this identity.
func gestureDigest(actor string, occurred time.Time, payload json.RawMessage) [32]byte {
	submitted, _ := json.Marshal(struct {
		Actor      string
		OccurredAt time.Time
		Payload    json.RawMessage
	}{actor, occurred.UTC(), payload})
	return sha256.Sum256(submitted)
}
