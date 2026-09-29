package controlplane

import (
	"context"
	"crypto/hmac"
	"encoding/base64"
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/ids"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/telemetry"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

// A position advances each stream independently: stdout and stderr have no
// shared producer sequence. Cross-stream timestamp ordering is not causal order.
type commandLogCursor struct {
	EnvironmentID string `json:"environment_id"`
	CommandID     string `json:"command_id"`
	Stdout        int64  `json:"stdout"`
	Stderr        int64  `json:"stderr"`
}

func (s *Server) listCommandLogsHTTP(w http.ResponseWriter, r *http.Request) {
	id, err := ids.Parse(chi.URLParam(r, "commandID"))
	if err != nil {
		writeError(w, badRequest(codedError{code: "invalid_command_reference", message: "command ID is invalid"}))
		return
	}
	principal := principalFromContext(r.Context())
	scope, projectID, environmentID, err := s.requestEnvironmentScopeFromRequest(r, principal)
	if err != nil {
		writeError(w, badRequest(err))
		return
	}
	if !canAccessComputerCommandOutput(principal, scope) {
		writeError(w, forbidden(codedError{code: "permission_required", message: errPermissionRequired.Error()}))
		return
	}
	command, err := s.db.GetCommand(r.Context(), db.GetCommandParams{OrgID: pgvalue.UUID(principal.OrgID), ProjectID: projectID, EnvironmentID: environmentID, CommandID: pgvalue.UUID(id)})
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, notFound(codedError{code: "command_not_found", message: "command was not found"}))
		return
	}
	if err != nil {
		writeRunTelemetryError(w, err)
		return
	}
	query, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil {
		writeError(w, badRequest(errors.New("query string is malformed")))
		return
	}
	for key, values := range query {
		if (key != "cursor" && key != "limit") || len(values) != 1 {
			writeError(w, badRequest(codedError{code: "invalid_command_log_query", message: "invalid command log query"}))
			return
		}
	}
	limit := int32(50)
	if raw, ok := query["limit"]; ok {
		n, e := strconv.Atoi(raw[0])
		if e != nil || n < 1 || n > 100 {
			writeError(w, badRequest(codedError{code: "invalid_command_log_query", message: "limit must be in [1,100]"}))
			return
		}
		limit = int32(n)
	}
	page, err := s.readCommandLogs(r.Context(), pgvalue.UUID(principal.OrgID), command, query.Get("cursor"), limit)
	if err != nil {
		if errors.Is(err, errTelemetryInvalidCursor) {
			writeError(w, badRequest(err))
		} else {
			writeRunTelemetryError(w, err)
		}
		return
	}
	writeJSON(w, http.StatusOK, page)
}

func (s *Server) readCommandLogs(ctx context.Context, orgID pgtype.UUID, command db.ComputerCommand, raw string, limit int32) (api.CommandLogPage, error) {
	// Once the command spans the retention boundary, missing history cannot be
	// distinguished from empty output after outbox collection. Do not invent EOF.
	if !command.CreatedAt.Valid || !time.Now().Before(command.CreatedAt.Time.Add(90*24*time.Hour)) {
		return api.CommandLogPage{}, gone(codedError{code: "command_log_history_unavailable", message: "complete command log history is outside retention"})
	}
	position := commandLogCursor{EnvironmentID: pgvalue.UUIDString(command.EnvironmentID), CommandID: pgvalue.UUIDString(command.ID), Stdout: -1, Stderr: -1}
	if raw != "" {
		parsed, err := s.parseCommandLogCursor(raw)
		if err != nil || parsed.EnvironmentID != position.EnvironmentID || parsed.CommandID != position.CommandID {
			return api.CommandLogPage{}, errTelemetryInvalidCursor
		}
		position = parsed
	}
	if limit < 1 || limit > 100 {
		return api.CommandLogPage{}, errors.New("invalid command log limit")
	}
	if s.telemetryReader == nil {
		return api.CommandLogPage{}, telemetry.ErrHistoricalUnavailable
	}
	streams := []string{"stdout", "stderr"}
	positions := []int64{position.Stdout, position.Stderr}
	pages := make([][]telemetry.CommandLogChunk, 2)
	var lagging error
	for i, stream := range streams {
		var after *uint64
		if positions[i] >= 0 {
			v := uint64(positions[i])
			after = &v
		}
		page, err := s.telemetryReader.ListCommandLogChunks(ctx, telemetry.CommandLogChunkQuery{OrgID: pgvalue.MustUUIDValue(orgID), EnvironmentID: pgvalue.MustUUIDValue(command.EnvironmentID), CommandID: pgvalue.MustUUIDValue(command.ID), Stream: stream, AfterObservedSeq: after, Limit: min(limit, 64)})
		if err != nil {
			return api.CommandLogPage{}, err
		}
		last := positions[i]
		for _, chunk := range page.Chunks {
			if chunk.ObservedSeq > math.MaxInt64 || int64(chunk.ObservedSeq) <= last {
				return api.CommandLogPage{}, telemetry.ErrHistoricalUnavailable
			}
			last = int64(chunk.ObservedSeq)
		}
		through := pgtype.Int8{}
		if len(page.Chunks) == int(min(limit, 64)) {
			through = pgtype.Int8{Int64: last, Valid: true}
		}
		// Check after reading the sink so an in-flight delivery is never skipped.
		frontier, err := s.db.GetCommandLogFrontier(ctx, db.GetCommandLogFrontierParams{OrgID: orgID, EnvironmentID: command.EnvironmentID, CommandID: command.ID, StreamName: stream, AfterObservedSeq: positions[i], ThroughObservedSeq: through})
		if err != nil {
			return api.CommandLogPage{}, err
		}
		if frontier.PendingSeq >= 0 {
			for len(page.Chunks) > 0 && int64(page.Chunks[len(page.Chunks)-1].ObservedSeq) >= frontier.PendingSeq {
				page.Chunks = page.Chunks[:len(page.Chunks)-1]
			}
		}
		if len(page.Chunks) == 0 && (frontier.PendingSeq >= 0 || frontier.ObservedSeq > positions[i]) {
			lagging = telemetry.LaggingError{WatermarkSeq: positions[i], WantSeq: frontier.ObservedSeq}
		}
		pages[i] = page.Chunks
	}
	result := api.CommandLogPage{OutputState: commandOutputState(command), Logs: make([]api.CommandLogRecord, 0, limit)}
	for len(result.Logs) < int(limit) && (len(pages[0]) > 0 || len(pages[1]) > 0) {
		i := 0
		if len(pages[0]) == 0 || (len(pages[1]) > 0 && pages[1][0].ObservedAt.Before(pages[0][0].ObservedAt)) {
			i = 1
		}
		chunk := pages[i][0]
		record := api.CommandLogRecord{Stream: streams[i]}
		if int64(chunk.ObservedSeq) > positions[i]+1 {
			if !command.TerminalAt.Valid {
				lagging = telemetry.LaggingError{WatermarkSeq: positions[i], WantSeq: int64(chunk.ObservedSeq)}
				pages[i] = nil
				continue
			}
			// Only collected/expired history may become a gap. If the outbox
			// still knows these chunks, an incomplete sink read is retryable.
			missing, err := s.db.GetCommandLogFrontier(ctx, db.GetCommandLogFrontierParams{OrgID: orgID, EnvironmentID: command.EnvironmentID, CommandID: command.ID, StreamName: streams[i], AfterObservedSeq: positions[i], ThroughObservedSeq: pgtype.Int8{Int64: int64(chunk.ObservedSeq) - 1, Valid: true}})
			if err != nil {
				return api.CommandLogPage{}, err
			}
			if missing.ObservedSeq >= 0 {
				lagging = telemetry.LaggingError{WatermarkSeq: positions[i], WantSeq: missing.ObservedSeq}
				pages[i] = nil
				continue
			}
			record.Kind = "gap"
			record.FromSequence = strconv.FormatInt(positions[i]+1, 10)
			record.ThroughSequence = strconv.FormatInt(int64(chunk.ObservedSeq)-1, 10)
			positions[i] = int64(chunk.ObservedSeq) - 1
		} else {
			record.Kind = "output"
			record.ContentBase64 = base64.StdEncoding.EncodeToString(chunk.Content)
			at := chunk.ObservedAt
			record.ObservedAt = &at
			positions[i] = int64(chunk.ObservedSeq)
			pages[i] = pages[i][1:]
		}
		position.Stdout, position.Stderr = positions[0], positions[1]
		cursor, err := s.signCommandLogCursor(position)
		if err != nil {
			return api.CommandLogPage{}, err
		}
		record.Cursor = cursor
		result.Logs = append(result.Logs, record)
		result.NextCursor = cursor
	}
	if len(result.Logs) == 0 && lagging != nil {
		return api.CommandLogPage{}, lagging
	}
	return result, nil
}

func (s *Server) signCommandLogCursor(position commandLogCursor) (string, error) {
	if len(s.authKeys.TelemetryCursor) == 0 {
		return "", errors.New("command log cursor signer unavailable")
	}
	payload, err := json.Marshal(position)
	if err != nil {
		return "", err
	}
	signature, err := auth.MAC(s.authKeys.TelemetryCursor, payload)
	if err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(payload) + "." + base64.RawURLEncoding.EncodeToString(signature), nil
}
func (s *Server) parseCommandLogCursor(raw string) (commandLogCursor, error) {
	var position commandLogCursor
	if len(raw) > 4096 || len(s.authKeys.TelemetryCursor) == 0 {
		return position, errTelemetryInvalidCursor
	}
	parts := strings.Split(raw, ".")
	if len(parts) != 2 {
		return position, errTelemetryInvalidCursor
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return position, errTelemetryInvalidCursor
	}
	signature, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return position, errTelemetryInvalidCursor
	}
	expected, err := auth.MAC(s.authKeys.TelemetryCursor, payload)
	if err != nil || !hmac.Equal(signature, expected) {
		return position, errTelemetryInvalidCursor
	}
	decoder := json.NewDecoder(strings.NewReader(string(payload)))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&position) != nil || position.Stdout < -1 || position.Stderr < -1 || position.CommandID == "" || position.EnvironmentID == "" {
		return position, errTelemetryInvalidCursor
	}
	return position, nil
}

// Output closure is not the command's result-retention lifecycle. The producer
// must acknowledge every chunk before these completion reasons can be recorded.
func commandOutputState(command db.ComputerCommand) string {
	if !command.TerminalAt.Valid {
		return "open"
	}
	if !command.ComputerInstanceID.Valid {
		return "closed"
	}
	switch command.TerminalReasonCode.String {
	case "computer_command_completed", "computer_command_timed_out", "computer_command_cancelled",
		"computer_command_signaled", "computer_command_launch_failed", "computer_command_secret_delivery_failed":
		return "closed"
	default:
		return "unavailable"
	}
}
