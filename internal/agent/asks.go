package agent

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/conversation"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/jsoncanon"
	"github.com/helmrdotdev/helmr/internal/workergroup"
	"github.com/jackc/pgx/v5"
)

var ErrAskAlreadyResponded = errors.New("ask already responded")
var ErrAskCancelled = errors.New("ask cancelled")

type AskView struct {
	ID, SessionID, TurnID                      uuid.UUID
	Status                                     string
	Prompt, AnswerControl, Question, Answer    json.RawMessage
	CreatedAt                                  time.Time
	RespondedAt, CancelledAt, PayloadExpiredAt *time.Time
	RespondedByUserID, RespondedByAPIKeyID     *uuid.UUID
	Sequence                                   int64
}

const askColumns = `id,session_id,turn_id,status,question,answer,created_at,responded_at,cancelled_at,payload_expired_at,responded_by_user_id,responded_by_api_key_id,created_event_seq`

func scanAsk(row pgx.Row) (AskView, error) {
	var v AskView
	err := row.Scan(&v.ID, &v.SessionID, &v.TurnID, &v.Status, &v.Question, &v.Answer, &v.CreatedAt, &v.RespondedAt, &v.CancelledAt, &v.PayloadExpiredAt, &v.RespondedByUserID, &v.RespondedByAPIKeyID, &v.Sequence)
	if err == nil && v.Question != nil {
		var question struct {
			Prompt json.RawMessage `json:"prompt"`
			Answer json.RawMessage `json:"answer"`
		}
		if err = json.Unmarshal(v.Question, &question); err == nil {
			v.Prompt, v.AnswerControl = question.Prompt, question.Answer
		}
	}
	return v, err
}

// RuntimeAsk admits one frozen question and its event together. Creation identity
// is the ask identity; retries retain that identity after cancellation or expiry.
func RuntimeAsk(ctx context.Context, pool db.TxBeginner, host workergroup.HostPrincipal, e Execution, turn, id uuid.UUID, raw json.RawMessage) error {
	question, err := conversation.Question(raw)
	if err != nil {
		return err
	}
	if id == uuid.Nil() {
		return ErrInvalidInput
	}
	digest := sha256.Sum256(question)
	return hideMissing(db.RunTx(ctx, pool, func(tx pgx.Tx) error {
		if err := lockRuntimeHost(ctx, tx, Caller{Kind: "session", ID: e.SessionID, Execution: e, Host: &host}); err != nil {
			return err
		}
		if err := requireRootCaller(ctx, tx, Caller{Kind: "session", ID: e.SessionID, Execution: e}); err != nil {
			return err
		}
		publication, err := lockSlackPublication(ctx, tx, e.EnvironmentID, e.SessionID)
		if err != nil {
			return err
		}
		if err := lockRuntimeOperationOwner(ctx, tx, host, e); err != nil {
			return err
		}
		if err := lockRuntimeContentTurn(ctx, tx, e, turn); err != nil {
			return err
		}
		var prior []byte
		err = tx.QueryRow(ctx, `SELECT question_digest FROM turn_asks WHERE environment_id=$1 AND session_id=$2 AND turn_id=$3 AND id=$4`, e.EnvironmentID, e.SessionID, turn, id).Scan(&prior)
		if err == nil {
			if !bytes.Equal(prior, digest[:]) {
				return ErrConflict
			}
			return nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		if err := runtimeContentReady(ctx, tx, e, turn, true); err != nil {
			return err
		}
		tag, err := tx.Exec(ctx, `UPDATE turns SET progress_bytes=progress_bytes+$3 WHERE environment_id=$1 AND id=$2 AND progress_bytes+$3<=8388608`, e.EnvironmentID, turn, len(question))
		if err != nil {
			return err
		}
		if tag.RowsAffected() != 1 {
			return fmt.Errorf("%w: Turn progress and questions exceed 8 MiB", conversation.ErrLimit)
		}
		var sequence int64
		data, _ := json.Marshal(map[string]any{"ask_id": id, "question": question})
		if err := tx.QueryRow(ctx, `WITH allocated AS (UPDATE sessions SET next_event_seq=next_event_seq+1 WHERE environment_id=$1 AND id=$2 RETURNING next_event_seq-1 AS seq)
   INSERT INTO session_events(environment_id,session_id,seq,turn_id,kind,data) SELECT $1,$2,seq,$3,'ask.created',$4 FROM allocated RETURNING seq`, e.EnvironmentID, e.SessionID, turn, data).Scan(&sequence); err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `INSERT INTO turn_asks(environment_id,session_id,turn_id,id,process_epoch,created_event_seq,question_digest,question) VALUES($1,$2,$3,$4,$5,$6,$7,$8)`, e.EnvironmentID, e.SessionID, turn, id, e.ProcessEpoch, sequence, digest[:], []byte(question))
		if err != nil {
			return err
		}
		return publication.accept(ctx, tx, e.EnvironmentID, e.SessionID, turn, sequence)
	}))
}

func RuntimeObserveAsk(ctx context.Context, pool db.TxBeginner, host workergroup.HostPrincipal, e Execution, turn, id uuid.UUID) (AskView, error) {
	var view AskView
	err := db.RunTx(ctx, pool, func(tx pgx.Tx) error {
		if err := lockRuntimeOperationOwner(ctx, tx, host, e); err != nil {
			return err
		}
		if err := lockRuntimeContentTurn(ctx, tx, e, turn); err != nil {
			return err
		}
		var err error
		view, err = scanAsk(tx.QueryRow(ctx, `SELECT `+askColumns+` FROM turn_asks WHERE environment_id=$1 AND session_id=$2 AND turn_id=$3 AND id=$4 AND process_epoch=$5`, e.EnvironmentID, e.SessionID, turn, id, e.ProcessEpoch))
		return err
	})
	return view, hideMissing(err)
}

func RuntimeWithdrawAsk(ctx context.Context, pool db.TxBeginner, host workergroup.HostPrincipal, e Execution, turn, id uuid.UUID) error {
	return hideMissing(db.RunTx(ctx, pool, func(tx pgx.Tx) error {
		if err := lockRuntimeOperationOwner(ctx, tx, host, e); err != nil {
			return err
		}
		if err := lockRuntimeContentTurn(ctx, tx, e, turn); err != nil {
			return err
		}
		var status string
		if err := tx.QueryRow(ctx, `SELECT status FROM turn_asks WHERE environment_id=$1 AND session_id=$2 AND turn_id=$3 AND id=$4 AND process_epoch=$5 FOR UPDATE`, e.EnvironmentID, e.SessionID, turn, id, e.ProcessEpoch).Scan(&status); err != nil {
			return err
		}
		if status != "pending" {
			return nil
		}
		return cancelAsk(ctx, tx, e.EnvironmentID, e.SessionID, turn, id)
	}))
}
func cancelAsk(ctx context.Context, tx pgx.Tx, env, session, turn, id uuid.UUID) error {
	if _, err := tx.Exec(ctx, `UPDATE turn_asks SET status='cancelled',cancelled_at=clock_timestamp() WHERE environment_id=$1 AND session_id=$2 AND turn_id=$3 AND id=$4 AND status='pending'`, env, session, turn, id); err != nil {
		return err
	}
	data, _ := json.Marshal(map[string]any{"ask_id": id})
	return eventData(ctx, tx, env, session, turn, "ask.cancelled", data)
}

// Call under the owning Session/Turn locks when a Turn becomes terminal.
func cancelTurnAsks(ctx context.Context, tx pgx.Tx, env, session, turn uuid.UUID) error {
	rows, err := tx.Query(ctx, `SELECT id FROM turn_asks WHERE environment_id=$1 AND session_id=$2 AND turn_id=$3 AND status='pending' ORDER BY created_event_seq FOR UPDATE`, env, session, turn)
	if err != nil {
		return err
	}
	var ids []uuid.UUID
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		ids = append(ids, id)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, id := range ids {
		if err := cancelAsk(ctx, tx, env, session, turn, id); err != nil {
			return err
		}
	}
	return nil
}

// Answer authority is independent of Session browsing. Human membership and
// service-key grants are rechecked and locked in the answer transaction.
func authorizeAskAnswer(ctx context.Context, tx pgx.Tx, caller Caller, env uuid.UUID) error {
	var id uuid.UUID
	var err error
	switch caller.Kind {
	case "user":
		err = tx.QueryRow(ctx, `SELECT m.user_id FROM environments e JOIN org_members m ON m.org_id=e.org_id JOIN users u ON u.id=m.user_id WHERE e.id=$1 AND m.user_id=$2 AND m.disabled_at IS NULL AND u.disabled_at IS NULL AND m.role IN ('owner','admin','developer','viewer') FOR SHARE OF m,u`, env, caller.ID).Scan(&id)
	case "api_key":
		err = tx.QueryRow(ctx, `SELECT k.id FROM api_keys k JOIN environments e ON (e.id,e.project_id,e.org_id)=(k.environment_id,k.project_id,k.org_id) WHERE e.id=$1 AND k.id=$2 AND k.revoked_at IS NULL AND (k.expires_at IS NULL OR k.expires_at>clock_timestamp()) AND k.role IN ('owner','admin') AND 'asks.respond'=ANY(k.permissions) FOR SHARE OF k`, env, caller.ID).Scan(&id)
	default:
		return ErrDenied
	}
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrDenied
	}
	return err
}

type AskAnswerRequest struct {
	EnvironmentID, SessionID, TurnID, AskID uuid.UUID
	ResponseID                              string
	Answer                                  json.RawMessage
}

func RespondAsk(ctx context.Context, pool db.TxBeginner, caller Caller, req AskAnswerRequest) (AskView, error) {
	var view AskView
	if strings.TrimSpace(req.ResponseID) == "" || len(req.ResponseID) > 512 || !utf8.ValidString(req.ResponseID) || strings.ContainsRune(req.ResponseID, 0) {
		return view, ErrInvalidInput
	}
	value, err := jsoncanon.Transform(req.Answer)
	if err != nil {
		return view, conversation.ErrAnswerInvalid
	}
	if len(value) > conversation.AnswerBytes {
		return view, fmt.Errorf("%w: Answer exceeds 64 KiB", conversation.ErrLimit)
	}
	digest := sha256.Sum256(value)
	err = db.RunTx(ctx, pool, func(tx pgx.Tx) error {
		if err := authorizeAskAnswer(ctx, tx, caller, req.EnvironmentID); err != nil {
			return err
		}
		if _, err := lockSession(ctx, tx, req.EnvironmentID, req.SessionID); err != nil {
			return err
		}
		var running bool
		if err := tx.QueryRow(ctx, `SELECT status='running' AND (deadline_at IS NULL OR deadline_at>clock_timestamp()) FROM turns WHERE environment_id=$1 AND session_id=$2 AND id=$3 FOR NO KEY UPDATE`, req.EnvironmentID, req.SessionID, req.TurnID).Scan(&running); err != nil {
			return err
		}
		var priorID *string
		var priorDigest []byte
		if err := tx.QueryRow(ctx, `SELECT response_id,answer_digest FROM turn_asks WHERE environment_id=$1 AND session_id=$2 AND turn_id=$3 AND id=$4 FOR UPDATE`, req.EnvironmentID, req.SessionID, req.TurnID, req.AskID).Scan(&priorID, &priorDigest); err != nil {
			return err
		}
		var err error
		view, err = scanAsk(tx.QueryRow(ctx, `SELECT `+askColumns+` FROM turn_asks WHERE environment_id=$1 AND session_id=$2 AND turn_id=$3 AND id=$4`, req.EnvironmentID, req.SessionID, req.TurnID, req.AskID))
		if err != nil {
			return err
		}
		if err := authorizeAskAnswer(ctx, tx, caller, req.EnvironmentID); err != nil {
			return err
		}
		if priorID != nil {
			if *priorID != req.ResponseID {
				return ErrAskAlreadyResponded
			}
			sameResponder := caller.Kind == "user" && view.RespondedByUserID != nil && *view.RespondedByUserID == caller.ID || caller.Kind == "api_key" && view.RespondedByAPIKeyID != nil && *view.RespondedByAPIKeyID == caller.ID
			if !sameResponder || !bytes.Equal(priorDigest, digest[:]) {
				return ErrConflict
			}
			return nil
		}
		if view.Status == "cancelled" {
			return ErrAskCancelled
		}
		if !running {
			return ErrNotReady
		}
		if _, err := conversation.Answer(view.Question, value); err != nil {
			return err
		}
		var user, key *uuid.UUID
		if caller.Kind == "user" {
			user = &caller.ID
		} else {
			key = &caller.ID
		}
		view, err = scanAsk(tx.QueryRow(ctx, `UPDATE turn_asks SET status='responded',response_id=$5,answer_digest=$6,answer=$7,responded_at=clock_timestamp(),responded_by_user_id=$8,responded_by_api_key_id=$9 WHERE environment_id=$1 AND session_id=$2 AND turn_id=$3 AND id=$4 RETURNING `+askColumns, req.EnvironmentID, req.SessionID, req.TurnID, req.AskID, req.ResponseID, digest[:], []byte(value), user, key))
		if err != nil {
			return err
		}
		data := map[string]any{"ask_id": req.AskID, "answer": value}
		if user != nil {
			data["responded_by_user_id"] = user
		} else {
			data["responded_by_api_key_id"] = key
		}
		raw, _ := json.Marshal(data)
		return eventData(ctx, tx, req.EnvironmentID, req.SessionID, req.TurnID, "ask.responded", raw)
	})
	return view, hideMissing(err)
}

func GetAsk(ctx context.Context, pool db.TxBeginner, caller Caller, env, session, turn, id uuid.UUID) (AskView, error) {
	var view AskView
	err := readTransaction(ctx, pool, caller, env, func(tx pgx.Tx) error {
		var err error
		view, err = scanAsk(tx.QueryRow(ctx, `SELECT `+askColumns+` FROM turn_asks WHERE environment_id=$1 AND session_id=$2 AND turn_id=$3 AND id=$4`, env, session, turn, id))
		return err
	})
	return view, err
}

type AskPage struct {
	Asks         []AskView
	NextSequence int64
}

func ListAsks(ctx context.Context, pool db.TxBeginner, caller Caller, req TurnListRequest, turn uuid.UUID) (AskPage, error) {
	page := AskPage{Asks: []AskView{}}
	if req.After < 0 || req.Limit < 1 || req.Limit > 100 {
		return page, ErrInvalidInput
	}
	err := readTransaction(ctx, pool, caller, req.EnvironmentID, func(tx pgx.Tx) error {
		var id uuid.UUID
		if err := tx.QueryRow(ctx, `SELECT id FROM turns WHERE environment_id=$1 AND session_id=$2 AND id=$3`, req.EnvironmentID, req.SessionID, turn).Scan(&id); err != nil {
			return err
		}
		rows, err := tx.Query(ctx, `SELECT `+askColumns+` FROM turn_asks WHERE environment_id=$1 AND session_id=$2 AND turn_id=$3 AND created_event_seq>$4 ORDER BY created_event_seq LIMIT $5`, req.EnvironmentID, req.SessionID, turn, req.After, req.Limit+1)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			v, err := scanAsk(rows)
			if err != nil {
				return err
			}
			page.Asks = append(page.Asks, v)
		}
		if err := rows.Err(); err != nil {
			return err
		}
		if len(page.Asks) > req.Limit {
			page.Asks = page.Asks[:req.Limit]
			page.NextSequence = page.Asks[len(page.Asks)-1].Sequence
		}
		return nil
	})
	return page, err
}
