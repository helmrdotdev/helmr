package control

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/publicid"
	"github.com/helmrdotdev/helmr/internal/telemetry"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

const (
	runTelemetryDefaultLimit = int32(100)
	runTelemetryMaxLimit     = int32(200)
	runTelemetryCursorPrefix = "rt1."
	runTelemetryCursorMax    = 4096
)

type runTelemetryCursor struct {
	EnvironmentID string   `json:"environment_id"`
	RunID         string   `json:"run_id"`
	RecordKind    string   `json:"record_kind"`
	Filters       []string `json:"filters"`
	Sequence      int64    `json:"sequence"`
}

type runTelemetryTarget struct {
	orgID         pgtype.UUID
	environmentID pgtype.UUID
	runID         pgtype.UUID
	runPublicID   string
}

func (s *Server) listRunLogsHTTP(w http.ResponseWriter, r *http.Request) {
	target, ok := s.resolveRunTelemetryTarget(w, r)
	if !ok {
		return
	}
	levels, err := parseRunTelemetryFilter(r, "level")
	if err != nil {
		writeError(w, badRequest(codedError{code: "invalid_run_log_query", message: err.Error()}))
		return
	}
	after, limit, err := s.parseRunTelemetryPage(
		r,
		target,
		"logs",
		levels,
	)
	if err != nil {
		writeError(w, badRequest(errTelemetryInvalidCursor))
		return
	}
	page, err := s.telemetryReader.ListRunLogChunks(
		r.Context(),
		telemetry.RunLogChunkQuery{
			OrgID:    pgvalue.MustUUIDValue(target.orgID),
			RunID:    pgvalue.MustUUIDValue(target.runID),
			AfterSeq: after, Limit: limit + 1, Levels: levels,
		},
	)
	if err != nil {
		writeRunTelemetryError(w, err)
		return
	}
	hasNext := len(page.Chunks) > int(limit)
	if hasNext {
		page.Chunks = page.Chunks[:limit]
	}
	last := after
	records := make([]api.RunLogRecord, 0, len(page.Chunks))
	for _, chunk := range page.Chunks {
		seq, err := telemetry.ParseCursor(chunk.ID)
		if err != nil {
			writeRunTelemetryError(w, telemetry.ErrHistoricalUnavailable)
			return
		}
		record, err := projectRunLogRecord(chunk)
		if err != nil {
			writeRunTelemetryError(w, telemetry.ErrHistoricalUnavailable)
			return
		}
		last = seq
		records = append(records, record)
	}
	if !hasNext {
		if err := s.requireProjectedRunTelemetry(
			r,
			target,
			db.TelemetryStreamKindRunLog,
			levels,
			last,
		); err != nil {
			writeRunTelemetryError(w, err)
			return
		}
	}
	nextCursor := ""
	if last > after {
		nextCursor, err = s.signRunTelemetryCursor(runTelemetryCursor{
			EnvironmentID: pgvalue.MustUUIDValue(target.environmentID).String(),
			RunID:         target.runPublicID, RecordKind: "logs",
			Filters: levels, Sequence: last,
		})
		if err != nil {
			writeRunTelemetryError(w, err)
			return
		}
	}
	writeJSON(w, http.StatusOK, api.RunLogPage{
		Logs: records, NextCursor: nextCursor,
	})
}

func (s *Server) listRunEventsHTTP(w http.ResponseWriter, r *http.Request) {
	target, ok := s.resolveRunTelemetryTarget(w, r)
	if !ok {
		return
	}
	severities, err := parseRunTelemetryFilter(r, "severity")
	if err != nil {
		writeError(w, badRequest(codedError{code: "invalid_run_event_query", message: err.Error()}))
		return
	}
	after, limit, err := s.parseRunTelemetryPage(
		r,
		target,
		"events",
		severities,
	)
	if err != nil {
		writeError(w, badRequest(errTelemetryInvalidCursor))
		return
	}
	page, err := s.telemetryReader.ListEvents(r.Context(), telemetry.EventQuery{
		OrgID:       pgvalue.MustUUIDValue(target.orgID),
		SubjectType: eventSubjectTypeRun,
		SubjectID:   pgvalue.MustUUIDValue(target.runID),
		AfterSeq:    after, Limit: limit + 1, Severities: severities,
	})
	if err != nil {
		writeRunTelemetryError(w, err)
		return
	}
	hasNext := len(page.Events) > int(limit)
	if hasNext {
		page.Events = page.Events[:limit]
	}
	last := after
	for _, event := range page.Events {
		seq, err := telemetry.ParseCursor(event.ID)
		if err != nil {
			writeRunTelemetryError(w, telemetry.ErrHistoricalUnavailable)
			return
		}
		last = seq
	}
	if !hasNext {
		if err := s.requireProjectedRunTelemetry(
			r,
			target,
			db.TelemetryStreamKindEvent,
			severities,
			last,
		); err != nil {
			writeRunTelemetryError(w, err)
			return
		}
	}
	nextCursor := ""
	if last > after {
		nextCursor, err = s.signRunTelemetryCursor(runTelemetryCursor{
			EnvironmentID: pgvalue.MustUUIDValue(target.environmentID).String(),
			RunID:         target.runPublicID, RecordKind: "events",
			Filters: severities, Sequence: last,
		})
		if err != nil {
			writeRunTelemetryError(w, err)
			return
		}
	}
	writeJSON(w, http.StatusOK, api.RunEventPage{
		Events: page.Events, NextCursor: optionalString(nextCursor),
	})
}

func (s *Server) resolveRunTelemetryTarget(
	w http.ResponseWriter,
	r *http.Request,
) (runTelemetryTarget, bool) {
	principal := actorFromContext(r.Context())
	scope, projectID, environmentID, ok := s.authorizeRunRequest(
		w,
		r,
		principal,
		auth.PermissionRunsRead,
	)
	if !ok {
		return runTelemetryTarget{}, false
	}
	runPublicID := strings.TrimSpace(chi.URLParam(r, "runID"))
	if publicid.ValidateFor(publicid.Run, runPublicID) != nil {
		writeError(w, notFound(codedError{code: "run_not_found", message: "Run not found"}))
		return runTelemetryTarget{}, false
	}
	row, err := s.db.GetRunSnapshot(r.Context(), db.GetRunSnapshotParams{
		OrgID: pgvalue.UUID(scope.OrgID), ProjectID: projectID,
		EnvironmentID: environmentID, PublicID: runPublicID,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, notFound(codedError{code: "run_not_found", message: "Run not found"}))
		return runTelemetryTarget{}, false
	}
	if err != nil {
		s.writeRunReadAuthorityError(w)
		return runTelemetryTarget{}, false
	}
	return runTelemetryTarget{
		orgID: row.OrgID, environmentID: row.EnvironmentID,
		runID: row.ID, runPublicID: row.PublicID,
	}, true
}

func (s *Server) parseRunTelemetryPage(
	r *http.Request,
	target runTelemetryTarget,
	recordKind string,
	filters []string,
) (int64, int32, error) {
	limit, err := parseRunTelemetryLimit(r)
	if err != nil {
		return 0, 0, err
	}
	raw := strings.TrimSpace(r.URL.Query().Get("cursor"))
	if raw == "" {
		return 0, limit, nil
	}
	cursor, err := s.parseRunTelemetryCursor(raw)
	if err != nil ||
		cursor.EnvironmentID != pgvalue.MustUUIDValue(target.environmentID).String() ||
		cursor.RunID != target.runPublicID ||
		cursor.RecordKind != recordKind ||
		!slices.Equal(cursor.Filters, filters) {
		return 0, 0, errTelemetryInvalidCursor
	}
	return cursor.Sequence, limit, nil
}

func (s *Server) requireProjectedRunTelemetry(
	r *http.Request,
	target runTelemetryTarget,
	streamKind db.TelemetryStreamKind,
	filters []string,
	after int64,
) error {
	frontier, err := s.db.GetRunTelemetryFrontier(
		r.Context(),
		db.GetRunTelemetryFrontierParams{
			AfterSeq: after, OrgID: target.orgID, RunID: target.runID,
			StreamKind: streamKind, FilterValues: filters,
		},
	)
	if err != nil {
		return fmt.Errorf("read Run telemetry frontier: %w", err)
	}
	if frontier.ObservedSeq <= after {
		return nil
	}
	if frontier.DeadLetteredAfter {
		return telemetry.ErrHistoricalUnavailable
	}
	return telemetry.LaggingError{
		WatermarkSeq: after,
		WantSeq:      frontier.ObservedSeq,
	}
}

func projectRunLogRecord(chunk api.RunLogChunk) (api.RunLogRecord, error) {
	if chunk.Stream != string(api.WorkerLogStreamStructured) {
		if chunk.Stream != string(api.WorkerLogStreamStdout) &&
			chunk.Stream != string(api.WorkerLogStreamStderr) {
			return api.RunLogRecord{}, errors.New("Run log stream is invalid")
		}
		observed := chunk.ObservedSeq
		size := chunk.Bytes
		return api.RunLogRecord{
			ID: chunk.ID, Kind: chunk.Stream, RunID: chunk.RunID,
			AttemptNumber:    chunk.AttemptNumber,
			ObservedSequence: &observed, ContentBase64: chunk.ContentBase64,
			Bytes: &size, At: chunk.At,
		}, nil
	}
	content, err := base64.StdEncoding.DecodeString(chunk.ContentBase64)
	if err != nil {
		return api.RunLogRecord{}, err
	}
	var value struct {
		Level      string          `json:"level"`
		Message    string          `json:"message"`
		Attributes json.RawMessage `json:"attributes"`
	}
	decoder := json.NewDecoder(bytes.NewReader(content))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&value); err != nil ||
		!validRunTelemetryLevel(value.Level) ||
		len(value.Attributes) == 0 {
		return api.RunLogRecord{}, errors.New("structured Run log is invalid")
	}
	return api.RunLogRecord{
		ID: chunk.ID, Kind: "structured", RunID: chunk.RunID,
		AttemptNumber: chunk.AttemptNumber, Level: value.Level,
		Message: value.Message, Attributes: value.Attributes, At: chunk.At,
	}, nil
}

func parseRunTelemetryFilter(r *http.Request, name string) ([]string, error) {
	var rawValues []string
	for _, value := range r.URL.Query()[name] {
		rawValues = append(rawValues, strings.Split(value, ",")...)
	}
	seen := make(map[string]struct{}, len(rawValues))
	values := make([]string, 0, len(rawValues))
	for _, raw := range rawValues {
		value := strings.TrimSpace(raw)
		if !validRunTelemetryLevel(value) {
			return nil, fmt.Errorf("%s %q is invalid", name, raw)
		}
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		values = append(values, value)
	}
	slices.Sort(values)
	return values, nil
}

func validRunTelemetryLevel(value string) bool {
	switch value {
	case "debug", "info", "warn", "error":
		return true
	default:
		return false
	}
}

func parseRunTelemetryLimit(r *http.Request) (int32, error) {
	raw := strings.TrimSpace(r.URL.Query().Get("limit"))
	if raw == "" {
		return runTelemetryDefaultLimit, nil
	}
	value, err := strconv.ParseInt(raw, 10, 32)
	if err != nil || value < 1 || value > int64(runTelemetryMaxLimit) {
		return 0, fmt.Errorf("limit must be between 1 and %d", runTelemetryMaxLimit)
	}
	return int32(value), nil
}

func (s *Server) signRunTelemetryCursor(cursor runTelemetryCursor) (string, error) {
	if len(s.authSecret) == 0 || cursor.Sequence < 0 {
		return "", errors.New("Run telemetry cursor signer is unavailable")
	}
	payload, err := json.Marshal(cursor)
	if err != nil {
		return "", err
	}
	mac := hmac.New(sha256.New, s.authSecret)
	_, _ = mac.Write(payload)
	return runTelemetryCursorPrefix +
		base64.RawURLEncoding.EncodeToString(payload) + "." +
		base64.RawURLEncoding.EncodeToString(mac.Sum(nil)), nil
}

func (s *Server) parseRunTelemetryCursor(raw string) (runTelemetryCursor, error) {
	if len(raw) > runTelemetryCursorMax ||
		!strings.HasPrefix(raw, runTelemetryCursorPrefix) ||
		len(s.authSecret) == 0 {
		return runTelemetryCursor{}, errTelemetryInvalidCursor
	}
	parts := strings.Split(strings.TrimPrefix(raw, runTelemetryCursorPrefix), ".")
	if len(parts) != 2 {
		return runTelemetryCursor{}, errTelemetryInvalidCursor
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return runTelemetryCursor{}, errTelemetryInvalidCursor
	}
	signature, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return runTelemetryCursor{}, errTelemetryInvalidCursor
	}
	mac := hmac.New(sha256.New, s.authSecret)
	_, _ = mac.Write(payload)
	if !hmac.Equal(signature, mac.Sum(nil)) {
		return runTelemetryCursor{}, errTelemetryInvalidCursor
	}
	var cursor runTelemetryCursor
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&cursor); err != nil ||
		cursor.Sequence < 0 ||
		cursor.EnvironmentID == "" ||
		cursor.RunID == "" ||
		(cursor.RecordKind != "logs" && cursor.RecordKind != "events") ||
		!slices.IsSorted(cursor.Filters) {
		return runTelemetryCursor{}, errTelemetryInvalidCursor
	}
	for index, value := range cursor.Filters {
		if !validRunTelemetryLevel(value) ||
			(index > 0 && cursor.Filters[index-1] == value) {
			return runTelemetryCursor{}, errTelemetryInvalidCursor
		}
	}
	return cursor, nil
}

func optionalString(value string) *string {
	if value == "" {
		return nil
	}
	return &value
}
