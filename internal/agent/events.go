package agent

import (
	"context"
	"encoding/json"
	"errors"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/jackc/pgx/v5"
)

type EventView struct {
	SessionID uuid.UUID
	TurnID    *uuid.UUID
	Sequence  int64
	Kind      string
	Data      json.RawMessage
	CreatedAt time.Time
}

type EventPage struct {
	Records       []EventView
	NextAfter     int64
	HasMore       bool
	RetainedAfter int64
}

var ErrInvalidCursor = errors.New("invalid Session event cursor")

type CursorExpired struct{ RetainedAfter int64 }

func (e *CursorExpired) Error() string { return "Session event cursor has expired" }

// ListEvents reads both the retention boundary and its page from one snapshot.
// Inspection depends on current reader authority, not execution eligibility.
func ListEvents(ctx context.Context, pool db.TxBeginner, caller Caller, req TurnListRequest) (EventPage, error) {
	result := EventPage{Records: []EventView{}, NextAfter: req.After}
	if req.After < 0 || req.After > 9007199254740991 || req.Limit < 1 || req.Limit > 1000 {
		return result, ErrInvalidInput
	}
	err := readTransaction(ctx, pool, caller, req.EnvironmentID, func(tx pgx.Tx) error {
		var end int64
		if err := tx.QueryRow(ctx, `SELECT next_event_seq-1 FROM sessions WHERE environment_id=$1 AND id=$2`, req.EnvironmentID, req.SessionID).Scan(&end); err != nil {
			return err
		}
		if req.After > end {
			return ErrInvalidCursor
		}
		if err := tx.QueryRow(ctx, `SELECT COALESCE(max(seq),0) FROM session_events WHERE environment_id=$1 AND session_id=$2 AND payload_expired_at IS NOT NULL`, req.EnvironmentID, req.SessionID).Scan(&result.RetainedAfter); err != nil {
			return err
		}
		if req.After < result.RetainedAfter {
			return &CursorExpired{RetainedAfter: result.RetainedAfter}
		}
		rows, err := tx.Query(ctx, `SELECT session_id,turn_id,seq,kind,data,created_at FROM session_events WHERE environment_id=$1 AND session_id=$2 AND seq>$3 ORDER BY seq LIMIT $4`, req.EnvironmentID, req.SessionID, req.After, req.Limit+1)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			var e EventView
			if err := rows.Scan(&e.SessionID, &e.TurnID, &e.Sequence, &e.Kind, &e.Data, &e.CreatedAt); err != nil {
				return err
			}
			result.Records = append(result.Records, e)
		}
		if err := rows.Err(); err != nil {
			return err
		}
		if len(result.Records) > req.Limit {
			result.Records = result.Records[:req.Limit]
			result.HasMore = true
		}
		if len(result.Records) > 0 {
			result.NextAfter = result.Records[len(result.Records)-1].Sequence
		}
		return nil
	})
	return result, err
}
