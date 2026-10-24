package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"
)

// demoHistory builds settled Session history the way the control plane would
// have recorded it, then inserts it in dependency order. Every row is terminal
// or fenced: no Turn is queued or running, no Question is pending, and every
// Computer lease and Session process is fenced.
type demoHistory struct {
	sessions []*demoSessionRow
	turns    []*demoTurn
	events   []*demoEvent
	procs    []*demoProcess
	holds    []*demoHold
	controls []*demoControl
	asks     []*demoAsk
	messages []*demoMessage
}

type demoSessionRow struct {
	id, agent, computer, root, parent, origin string
	depth                                     int
	status                                    string
	generation                                int64
	createdAt                                 time.Time
	eligibleAt, expiredAt                     time.Time
	closeAfterIdleMS                          int64
	nextTurn, nextEvent                       int64
}

type demoTurn struct {
	id, session, computer        string
	seq                          int64
	status                       string
	callerKind, callerID, origin string
	method, target, key          string
	input, result, response      []byte
	responseID                   string
	errorCode, errorMessage      string
	epoch, leaseEpoch            int64
	nextMessage                  int64
	createdAt, startedAt         time.Time
	closedAt, recordedAt         time.Time
	terminalAt, expiredAt        time.Time
	saveID                       string
	saveSeq                      int64
	saveRequestedAt, savedAt     time.Time
}

type demoEvent struct {
	session, turn string
	seq           int64
	kind          string
	data          []byte
	at, expiredAt time.Time
}

type demoProcess struct {
	session, computer  string
	epoch, leaseEpoch  int64
	status             string
	fencedAt, failedAt time.Time
}

type demoHold struct {
	id, session, scope, reason string
	issuerKind, issuerID       string
	createdAt, releasedAt      time.Time
}

type demoControl struct {
	id, session, kind, callerKind, callerID string
	key, hold, turn                         string
	at                                      time.Time
}

type demoAsk struct {
	id, session, turn      string
	epoch, createdSeq      int64
	question               []byte
	status                 string
	createdAt, answeredAt  time.Time
	cancelledAt, expiredAt time.Time
	answerID               string
	answer                 []byte
	answeredBy             string
}

type demoMessage struct {
	id, session, turn    string
	epoch, seq           int64
	message              []byte
	status, reason       string
	createdAt, startedAt time.Time
	terminalAt           time.Time
}

// demoLog appends one Session's records with the Session's own counters.
type demoLog struct {
	h     *demoHistory
	row   *demoSessionRow
	at    time.Time
	epoch int64
	lease int64
}

func (h *demoHistory) session(row *demoSessionRow, lease int64) *demoLog {
	row.nextTurn, row.nextEvent = 1, 1
	h.sessions = append(h.sessions, row)
	return &demoLog{h: h, row: row, at: row.createdAt, lease: lease}
}

func (s *demoLog) wait(d time.Duration) time.Time {
	s.at = s.at.Add(d)
	return s.at
}

func (s *demoLog) event(turn *demoTurn, kind string, data any) int64 {
	raw := []byte("{}")
	if data != nil {
		raw = demoJSON(data)
	}
	id := ""
	if turn != nil {
		id = turn.id
	}
	seq := s.row.nextEvent
	s.row.nextEvent++
	s.h.events = append(s.h.events, &demoEvent{session: s.row.id, turn: id, seq: seq, kind: kind, data: raw, at: s.at})
	return seq
}

func (s *demoLog) startProcess() {
	s.epoch++
	s.h.procs = append(s.h.procs, &demoProcess{session: s.row.id, computer: s.row.computer, epoch: s.epoch, leaseEpoch: s.lease, status: "ready"})
}

// stopProcess fences the current process. A lost process records the failure.
func (s *demoLog) stopProcess(status string) {
	for _, p := range s.h.procs {
		if p.session == s.row.id && p.epoch == s.epoch {
			p.status, p.fencedAt = status, s.at
			if status == "lost" {
				p.failedAt = s.at
			}
		}
	}
}

type demoCaller struct{ kind, id, origin, method, target string }

func (s *demoLog) enqueue(id string, caller demoCaller, key string, input []byte) *demoTurn {
	t := &demoTurn{
		id: id, session: s.row.id, computer: s.row.computer, seq: s.row.nextTurn, status: "queued",
		callerKind: caller.kind, callerID: caller.id, origin: caller.origin, method: caller.method, target: caller.target,
		key: key, input: input, createdAt: s.at, nextMessage: 1,
	}
	s.row.nextTurn++
	s.h.turns = append(s.h.turns, t)
	s.event(t, "turn.queued", nil)
	return t
}

func (s *demoLog) run(t *demoTurn) {
	t.status, t.startedAt, t.epoch, t.leaseEpoch = "running", s.at, s.epoch, s.lease
	s.event(t, "turn.running", nil)
}

func (s *demoLog) output(t *demoTurn, parts ...any) {
	s.event(t, "turn.output", parts)
}

// complete records the outcome before the asynchronous filesystem save.
func (s *demoLog) complete(t *demoTurn, response []byte, result any) int64 {
	t.closedAt = s.at
	t.recordedAt = s.at
	t.result = demoJSON(result)
	if response != nil {
		t.response, t.responseID = response, demoUUID(6000+demoTurnNumber(t.id))
	}
	t.status, t.terminalAt = "completed", s.at
	completed := s.event(t, "turn.completed", nil)
	t.saveRequestedAt = s.at
	t.saveID = demoUUID(2000 + demoTurnNumber(t.id))
	t.savedAt = s.wait(3 * time.Second)
	return completed
}

func (s *demoLog) fail(t *demoTurn, code, message string) {
	t.status, t.errorCode, t.errorMessage, t.closedAt, t.terminalAt = "failed", code, message, s.at, s.at
	s.event(t, "turn.failed", map[string]any{"code": code})
}

// settle ends a Turn as a control or process loss does: pending Questions are
// cancelled and undelivered messages rejected before the Turn's own event.
func (s *demoLog) settle(t *demoTurn, status string) {
	for _, a := range s.h.asks {
		if a.turn == t.id && a.status == "pending" {
			a.status, a.cancelledAt = "cancelled", s.at
			s.event(t, "ask.cancelled", map[string]any{"ask_id": a.id})
		}
	}
	for _, m := range s.h.messages {
		if m.turn == t.id && (m.status == "admitted" || m.status == "started") {
			delivery := "not_delivered"
			if m.status == "started" {
				delivery = "uncertain"
			}
			m.status, m.reason, m.terminalAt = "rejected", "turn_terminated", s.at
			s.event(t, "message.rejected", map[string]any{"message_id": m.id, "status": "rejected", "delivery": delivery, "reason": "turn_terminated"})
		}
	}
	t.status, t.closedAt, t.terminalAt = status, s.at, s.at
	s.event(t, "turn."+status, nil)
}

func (s *demoLog) ask(t *demoTurn, id string, question map[string]any) *demoAsk {
	raw := demoJSON(question)
	seq := s.event(t, "ask.created", map[string]any{"ask_id": id, "question": json.RawMessage(raw)})
	a := &demoAsk{id: id, session: s.row.id, turn: t.id, epoch: t.epoch, createdSeq: seq, question: raw, status: "pending", createdAt: s.at}
	s.h.asks = append(s.h.asks, a)
	return a
}

func (s *demoLog) answer(a *demoAsk, answer any) {
	raw := demoJSON(answer)
	a.status, a.answeredAt, a.answer, a.answerID, a.answeredBy = "answered", s.at, raw, "answer-"+a.id, defaultUserID
	turn := &demoTurn{id: a.turn}
	s.event(turn, "ask.answered", map[string]any{"ask_id": a.id, "answer": json.RawMessage(raw), "answered_by_user_id": defaultUserID})
}

func (s *demoLog) message(t *demoTurn, id, text string) *demoMessage {
	m := &demoMessage{id: id, session: s.row.id, turn: t.id, epoch: t.epoch, seq: t.nextMessage, message: demoJSON([]any{demoText(text)}), status: "admitted", createdAt: s.at}
	t.nextMessage++
	s.h.messages = append(s.h.messages, m)
	s.event(t, "message.admitted", map[string]any{"message_id": id, "status": "admitted", "delivery": "admitted"})
	return m
}

func (s *demoLog) advanceMessage(t *demoTurn, m *demoMessage, status string) {
	m.status = status
	if status == "started" {
		m.startedAt = s.at
	} else {
		m.terminalAt = s.at
	}
	s.event(t, "message."+status, map[string]any{"message_id": m.id, "status": status, "delivery": status})
}

func (s *demoLog) hold(id, scope, reason, issuerKind, issuerID string) *demoHold {
	h := &demoHold{id: id, session: s.row.id, scope: scope, reason: reason, issuerKind: issuerKind, issuerID: issuerID, createdAt: s.at}
	s.h.holds = append(s.h.holds, h)
	s.row.generation++
	return h
}

// control records an accepted Session control and its event, which the
// control plane appends after the control's own effects.
func (s *demoLog) control(id, kind, callerKind, callerID, hold string, turn *demoTurn) {
	c := &demoControl{id: id, session: s.row.id, kind: kind, callerKind: callerKind, callerID: callerID, key: "demo-" + kind + "-" + id[len(id)-4:], hold: hold, at: s.at}
	data := map[string]any{"controlId": id}
	if hold != "" {
		data["holdId"] = hold
	}
	if kind == "turn_interrupt" {
		c.turn = turn.id
		s.h.controls = append(s.h.controls, c)
		s.event(turn, "turn.interrupt", data)
		return
	}
	s.h.controls = append(s.h.controls, c)
	s.event(nil, "session."+kind, data)
}

func (s *demoLog) release(h *demoHold) {
	h.releasedAt = s.at
	s.row.generation++
}

// close ends an idle Session and records its process's physical stop.
func (s *demoLog) close(closeEvent map[string]any, controlID string) {
	if controlID != "" {
		s.control(controlID, "close", "user", defaultUserID, "", nil)
	} else {
		s.event(nil, "session.close", closeEvent)
	}
	s.row.status = "closed"
	s.row.generation++
	s.event(nil, "session.closed", nil)
	s.wait(4 * time.Second)
	s.stopProcess("stopped")
	s.event(nil, "session.process_stopped", nil)
	s.row.eligibleAt = s.at
}

// expire applies the Session history retention policy's content expiry.
func (h *demoHistory) expire(session string, at time.Time) {
	for _, row := range h.sessions {
		if row.id == session {
			row.expiredAt = at
		}
	}
	for _, e := range h.events {
		if e.session == session {
			e.data, e.expiredAt = nil, at
		}
	}
	for _, t := range h.turns {
		if t.session == session {
			t.input, t.result, t.response, t.errorMessage, t.expiredAt = nil, nil, nil, "", at
		}
	}
	for _, a := range h.asks {
		if a.session == session {
			a.question, a.answer, a.expiredAt = nil, nil, at
		}
	}
}

// write inserts the history in foreign-key order: the completed result exists
// before its filesystem save is published.
func (h *demoHistory) write(ctx context.Context, tx pgx.Tx, environment string, saveRetiredAt time.Time) error {
	var saves []*demoTurn
	for _, t := range h.turns {
		if t.saveID != "" {
			saves = append(saves, t)
		}
	}
	slices.SortFunc(saves, func(a, b *demoTurn) int { return a.saveRequestedAt.Compare(b.saveRequestedAt) })
	for i, t := range saves {
		t.saveSeq = int64(i + 1)
	}
	b := &pgx.Batch{}
	for _, s := range h.sessions {
		root := s.root
		if root == "" {
			root = s.id
		}
		b.Queue(`INSERT INTO sessions(environment_id,id,agent_id,deployment_id,computer_id,root_session_id,parent_session_id,requester_session_id,causal_depth,status,authority_generation,next_turn_seq,next_event_seq,created_at,close_after_idle_ms,history_eligible_at,history_expired_at,admission_mode,origin_kind,origin_user_id)
VALUES($1,$2,$3,$4,$5,$6,$7,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,'scoped','user','00000000-0000-7000-8000-000000000101')`,
			environment, s.id, s.agent, demoSeedDeploymentID, s.computer, root, demoNullable(s.parent), s.depth, s.status, s.generation,
			s.nextTurn, s.nextEvent, s.createdAt, demoNullableInt(s.closeAfterIdleMS), demoTime(s.eligibleAt), demoTime(s.expiredAt))
	}
	for _, p := range h.procs {
		b.Queue(`INSERT INTO session_processes(environment_id,session_id,epoch,computer_id,computer_lease_epoch,status,fenced_at,failure_recorded_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8)`,
			environment, p.session, p.epoch, p.computer, p.leaseEpoch, p.status, demoTime(p.fencedAt), demoTime(p.failedAt))
	}
	for _, t := range h.turns {
		status := t.status
		var responseDigest []byte
		if t.responseID != "" {
			responseDigest = demoSHA(t.responseID)
		}
		var responseStaged, responseExpired any
		if t.responseID != "" {
			responseStaged = t.recordedAt
			if t.response == nil {
				responseExpired = t.expiredAt
			}
		}
		var resultDigest []byte
		if !t.recordedAt.IsZero() {
			resultDigest = demoSHA("result:" + t.id)
		}
		var drain any
		if !t.recordedAt.IsZero() || t.status == "failed" {
			drain = "Turn drained before its result was recorded"
		}
		var errorMessage []byte
		if t.errorMessage != "" {
			errorMessage = []byte(t.errorMessage)
		}
		var result any
		if t.result != nil {
			result = string(t.result)
		}
		var epoch any
		if t.epoch != 0 {
			epoch = t.epoch
		}
		b.Queue(`INSERT INTO turns(environment_id,id,session_id,computer_id,seq,status,caller_kind,caller_id,origin_turn_id,created_at,admission_method,target_id,idempotency_key,request_digest,
 input,result,error_code,error_message,payload_expired_at,response_id,response_digest,response,response_staged_at,response_expired_at,process_epoch,next_message_seq,processing_closed_at,result_recorded_at,result_digest,drain_evidence,started_at,terminal_at)
VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16::jsonb,$17,$18,$19,$20,$21,$22,$23,$24,$25,$26,$27,$28,$29,$30,$31,$32)`,
			environment, t.id, t.session, t.computer, t.seq, status, t.callerKind, t.callerID, demoNullable(t.origin), t.createdAt, t.method, t.target, demoNullable(t.key), demoSHA("request:"+t.id),
			t.input, result, demoNullable(t.errorCode), errorMessage, demoTime(t.expiredAt), demoNullable(t.responseID), responseDigest, t.response, responseStaged, responseExpired,
			epoch, t.nextMessage, demoTime(t.closedAt), demoTime(t.recordedAt), resultDigest, drain, demoTime(t.startedAt), demoTime(t.terminalAt))
	}
	for _, s := range h.sessions {
		if s.origin != "" {
			b.Queue(`UPDATE sessions SET origin_turn_id=$3 WHERE environment_id=$1 AND id=$2`, environment, s.id, s.origin)
		}
	}
	for _, t := range saves {
		b.Queue(`INSERT INTO computer_saves(environment_id,id,computer_id,computer_lease_epoch,seq,turn_id,status,requested_at,flush_acknowledged_at,captured_at,captured_root_digest,capture_evidence,publication_evidence,payload_retired_at)
VALUES($1,$2,$3,$4,$5,$6,'published',$7,$8,$8,$9,'Turn completion captured','Turn completion published',$10)`,
			environment, t.saveID, t.computer, t.leaseEpoch, t.saveSeq, t.id, t.saveRequestedAt, t.savedAt, demoSHA("root:"+t.id), saveRetiredAt)
	}
	b.Queue(`UPDATE computers SET next_save_seq=$3 WHERE environment_id=$1 AND id=$2`, environment, demoSeedHistoryComputerID, len(saves)+1)
	for _, e := range h.events {
		b.Queue(`INSERT INTO session_events(environment_id,session_id,seq,turn_id,kind,data,payload_expired_at,created_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8)`,
			environment, e.session, e.seq, demoNullable(e.turn), e.kind, e.data, demoTime(e.expiredAt), e.at)
	}
	for _, hold := range h.holds {
		b.Queue(`INSERT INTO session_holds(environment_id,id,session_id,scope,reason,issuer_kind,issuer_id,created_at,released_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9)`,
			environment, hold.id, hold.session, hold.scope, hold.reason, hold.issuerKind, demoNullable(hold.issuerID), hold.createdAt, demoTime(hold.releasedAt))
	}
	for _, c := range h.controls {
		b.Queue(`INSERT INTO session_controls(environment_id,id,session_id,kind,caller_kind,caller_id,idempotency_key,request_digest,hold_id,turn_id,accepted_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)`,
			environment, c.id, c.session, c.kind, c.callerKind, c.callerID, c.key, demoSHA("control:"+c.id), demoNullable(c.hold), demoNullable(c.turn), c.at)
	}
	for _, a := range h.asks {
		var answerDigest []byte
		if a.answerID != "" {
			answerDigest = demoSHA("answer:" + a.id)
		}
		b.Queue(`INSERT INTO turn_asks(environment_id,session_id,turn_id,id,process_epoch,created_event_seq,question_digest,question,status,created_at,answered_at,cancelled_at,answer_id,answer_digest,answer,answered_by_user_id,payload_expired_at)
VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14,$15,$16,$17)`,
			environment, a.session, a.turn, a.id, a.epoch, a.createdSeq, demoSHA("question:"+a.id), a.question, a.status, a.createdAt, demoTime(a.answeredAt), demoTime(a.cancelledAt),
			demoNullable(a.answerID), answerDigest, a.answer, demoNullable(a.answeredBy), demoTime(a.expiredAt))
	}
	for _, m := range h.messages {
		b.Queue(`INSERT INTO turn_messages(environment_id,id,session_id,turn_id,process_epoch,seq,message,caller_kind,caller_id,admission_method,target_id,request_digest,status,rejection_reason,created_at,started_at,terminal_at)
VALUES($1,$2,$3,$4,$5,$6,$7,'user',$8,'turn_send',$4,$9,$10,$11,$12,$13,$14)`,
			environment, m.id, m.session, m.turn, m.epoch, m.seq, m.message, defaultUserID, demoSHA("message:"+m.id), m.status, demoNullable(m.reason), m.createdAt, demoTime(m.startedAt), demoTime(m.terminalAt))
	}
	return tx.SendBatch(ctx, b).Close()
}

func demoUUID(n int) string { return fmt.Sprintf("00000000-0000-7000-8000-%012d", n) }

func demoTurnNumber(id string) int {
	var n int
	_, _ = fmt.Sscanf(id[len(id)-12:], "%d", &n)
	return n
}

func demoSHA(label string) []byte {
	sum := sha256.Sum256([]byte("helmr.dev.console-demo-seed\x00" + label))
	return sum[:]
}

// demoJSON encodes canonical-looking JSON: sorted map keys and no HTML escaping.
func demoJSON(v any) []byte {
	var buf bytes.Buffer
	encoder := json.NewEncoder(&buf)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(v); err != nil {
		panic(err)
	}
	return bytes.TrimSuffix(buf.Bytes(), []byte("\n"))
}

func demoText(text string) map[string]any { return map[string]any{"type": "text", "text": text} }

func demoValue(value any) map[string]any { return map[string]any{"type": "json", "value": value} }

func demoContent(text string) []byte { return demoJSON([]any{demoText(text)}) }

func demoNullable(value string) any {
	if value == "" {
		return nil
	}
	return value
}

func demoNullableInt(value int64) any {
	if value == 0 {
		return nil
	}
	return value
}

func demoTime(value time.Time) any {
	if value.IsZero() {
		return nil
	}
	return value
}
