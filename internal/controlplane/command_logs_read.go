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
	"github.com/helmrdotdev/helmr/internal/command"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/ids"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/telemetry"
	"github.com/helmrdotdev/helmr/internal/telemetry/diagnostic"
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
	// The scope read is neutral to result retention; log history keeps its
	// own boundary.
	scoped, err := command.Get(r.Context(), s.db, command.Ref{
		OrgID: principal.OrgID, ProjectID: pgvalue.MustUUIDValue(projectID),
		EnvironmentID: pgvalue.MustUUIDValue(environmentID), CommandID: id,
	})
	if errors.Is(err, command.ErrNotFound) {
		writeError(w, notFound(codedError{code: "command_not_found", message: "command was not found"}))
		return
	}
	if err != nil {
		writeTelemetryError(w, err)
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
	page, err := s.readCommandLogs(r.Context(), pgvalue.UUID(principal.OrgID), scoped, query.Get("cursor"), limit)
	if err != nil {
		if errors.Is(err, errTelemetryInvalidCursor) {
			writeError(w, badRequest(err))
		} else {
			writeTelemetryError(w, err)
		}
		return
	}
	writeJSON(w, http.StatusOK, page)
}

func (s *Server) readCommandLogs(ctx context.Context, orgID pgtype.UUID, command db.ComputerCommand, raw string, limit int32) (api.CommandLogPage, error) {
	position := commandLogCursor{EnvironmentID: pgvalue.UUIDString(command.EnvironmentID), CommandID: pgvalue.UUIDString(command.ID)}
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
	for i, stream := range streams {
		after := uint64(positions[i])
		page, err := s.telemetryReader.ListCommandLogChunks(ctx, telemetry.CommandLogChunkQuery{OrgID: pgvalue.MustUUIDValue(orgID), EnvironmentID: pgvalue.MustUUIDValue(command.EnvironmentID), CommandID: pgvalue.MustUUIDValue(command.ID), Stream: stream, AfterObservedSeq: &after, Limit: min(limit, 64)})
		if err != nil {
			return api.CommandLogPage{}, err
		}
		pages[i] = page.Chunks
	}
	// Read owner progress after the sink: publication may have advanced during either read.
	current, err := s.db.GetCommandLogState(ctx, db.GetCommandLogStateParams{OrgID: orgID, EnvironmentID: command.EnvironmentID, CommandID: command.ID})
	if err != nil {
		return api.CommandLogPage{}, err
	}
	accepted := []int64{current.StdoutAcceptedThrough, current.StderrAcceptedThrough}
	expired := []int64{current.StdoutExpiredThrough, current.StderrExpiredThrough}
	expires := []pgtype.Timestamptz{current.StdoutLastExpiresAt, current.StderrLastExpiresAt}
	for i := range streams {
		if (expired[i] > positions[i]) || (accepted[i] > positions[i] && expires[i].Valid && !expires[i].Time.After(time.Now())) {
			return api.CommandLogPage{}, gone(codedError{code: "command_log_history_unavailable", message: "command log history is outside retention"})
		}
	}
	result := api.CommandLogPage{OutputState: "open", Logs: make([]api.CommandLogRecord, 0, limit)}
	var lagging error
	ended := [2]bool{}
	for len(result.Logs) < int(limit) && (len(pages[0]) > 0 || len(pages[1]) > 0) {
		i := 0
		if len(pages[0]) == 0 || (len(pages[1]) > 0 && pages[1][0].ObservedAt.Before(pages[0][0].ObservedAt)) {
			i = 1
		}
		chunk := pages[i][0]
		if chunk.ObservedSeq > math.MaxInt64 || chunk.ThroughSequence > math.MaxInt64 {
			return api.CommandLogPage{}, telemetry.ErrHistoricalUnavailable
		}
		record := diagnostic.Record{Stream: streams[i], Kind: chunk.Kind, Sequence: int64(chunk.ObservedSeq), ThroughSequence: int64(chunk.ThroughSequence), ObservedAtUnixNano: chunk.ObservedAt.UnixNano(), Data: chunk.Content, DroppedBytes: chunk.DroppedBytes, Complete: chunk.Complete}
		if record.Validate(s.diagnosticBounds.ChunkBytes) != nil || record.Sequence <= positions[i] || record.ThroughSequence > accepted[i] || ended[i] {
			return api.CommandLogPage{}, telemetry.ErrHistoricalUnavailable
		}
		if record.Sequence != positions[i]+1 {
			// Absence in an eventually available sink never proves a producer gap.
			lagging = telemetry.LaggingError{WatermarkSeq: positions[i], WantSeq: record.Sequence}
			pages[i] = nil
			continue
		}
		pages[i] = pages[i][1:]
		if record.Kind == "end" {
			ended[i] = true
			if record.ThroughSequence != accepted[i] {
				return api.CommandLogPage{}, telemetry.ErrHistoricalUnavailable
			}
			// End is protocol evidence, not a public output record. Re-reading it is harmless.
			continue
		}
		public := api.CommandLogRecord{Stream: streams[i]}
		if record.Kind == "gap" {
			public.Kind = "gap"
			public.FromSequence = strconv.FormatInt(record.Sequence, 10)
			public.ThroughSequence = strconv.FormatInt(record.ThroughSequence, 10)
		} else {
			public.Kind = "output"
			public.ContentBase64 = base64.StdEncoding.EncodeToString(record.Data)
			at := chunk.ObservedAt
			public.ObservedAt = &at
		}
		positions[i] = record.ThroughSequence
		position.Stdout, position.Stderr = positions[0], positions[1]
		cursor, err := s.signCommandLogCursor(position)
		if err != nil {
			return api.CommandLogPage{}, err
		}
		public.Cursor = cursor
		result.Logs = append(result.Logs, public)
		result.NextCursor = cursor
	}
	covered := true
	for i := range streams {
		if positions[i] < accepted[i] && !ended[i] {
			covered = false
			if len(pages[i]) == 0 {
				lagging = telemetry.LaggingError{WatermarkSeq: positions[i], WantSeq: accepted[i]}
			}
		}
	}
	if covered {
		result.OutputState = commandOutputState(current)
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
	if decoder.Decode(&position) != nil || position.Stdout < 0 || position.Stderr < 0 || position.CommandID == "" || position.EnvironmentID == "" {
		return position, errTelemetryInvalidCursor
	}
	return position, nil
}

// Output closure follows explicit pipe frontiers, independently of process exit.
func commandOutputState(command db.ComputerCommand) string {
	if !command.TerminalAt.Valid {
		return "open"
	}
	if !command.ComputerLeaseEpoch.Valid {
		return "closed"
	}
	if command.StdoutEnded && command.StderrEnded && command.StdoutEndComplete && command.StderrEndComplete && command.StdoutFinalThrough.Valid && command.StderrFinalThrough.Valid && command.StdoutAcceptedThrough == command.StdoutFinalThrough.Int64 && command.StderrAcceptedThrough == command.StderrFinalThrough.Int64 {
		return "closed"
	}
	if command.OutputFenced || command.Status == "lost" || command.ProcessReconciledAt.Valid || (command.StdoutEnded && !command.StdoutEndComplete) || (command.StderrEnded && !command.StderrEndComplete) {
		return "unavailable"
	}
	return "open"
}
