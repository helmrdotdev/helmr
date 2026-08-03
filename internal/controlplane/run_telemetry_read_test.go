package controlplane

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/telemetry"
	"github.com/helmrdotdev/helmr/internal/workerapi"
)

type runTelemetryFrontierStore struct {
	db.Querier
	frontier db.GetRunTelemetryFrontierRow
	params   db.GetRunTelemetryFrontierParams
}

func (store *runTelemetryFrontierStore) GetRunTelemetryFrontier(
	_ context.Context,
	params db.GetRunTelemetryFrontierParams,
) (db.GetRunTelemetryFrontierRow, error) {
	store.params = params
	return store.frontier, nil
}

func TestRunTelemetryCursorIsIntegrityAndScopeBound(t *testing.T) {
	server := &Server{authKeys: auth.Keys{TelemetryCursor: make([]byte, auth.RootKeySize)}}
	want := runTelemetryCursor{
		EnvironmentID: "00000000-0000-0000-0000-000000000001",
		RunID:         "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc31",
		RecordKind:    "logs",
		Filters:       []string{"error", "warn"},
		Sequence:      42,
	}
	raw, err := server.signRunTelemetryCursor(want)
	if err != nil {
		t.Fatal(err)
	}
	got, err := server.parseRunTelemetryCursor(raw)
	if err != nil {
		t.Fatal(err)
	}
	if got.EnvironmentID != want.EnvironmentID ||
		got.RunID != want.RunID ||
		got.RecordKind != want.RecordKind ||
		got.Sequence != want.Sequence {
		t.Fatalf("cursor = %+v, want %+v", got, want)
	}
	if _, err := server.parseRunTelemetryCursor(raw + "x"); err == nil {
		t.Fatal("tampered cursor was accepted")
	}
}

func TestRunTelemetryFilterIsNormalized(t *testing.T) {
	request := httptest.NewRequest(
		"GET",
		"/?level=warn,error&level=warn",
		nil,
	)
	got, err := parseRunTelemetryFilter(request, "level")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0] != "error" || got[1] != "warn" {
		t.Fatalf("levels = %+v", got)
	}
}

func TestProjectRunLogRecordDistinguishesStructuredAndStream(t *testing.T) {
	at := time.Date(2026, 7, 25, 1, 2, 3, 0, time.UTC)
	structuredBody, err := json.Marshal(map[string]any{
		"level": "error", "message": "failed",
		"attributes": map[string]any{"step": 2},
	})
	if err != nil {
		t.Fatal(err)
	}
	structured, err := projectRunLogRecord(api.RunLogChunk{
		ID: "tc1.structured", RunID: "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc31",
		AttemptNumber: 2, Stream: string(workerapi.LogStreamStructured),
		ContentBase64: base64.StdEncoding.EncodeToString(structuredBody), At: at,
	}, "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc38")
	if err != nil {
		t.Fatal(err)
	}
	if structured.Kind != "structured" ||
		structured.RunID != "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc38" ||
		structured.Level != "error" ||
		structured.Message != "failed" ||
		string(structured.Attributes) != `{"step":2}` {
		t.Fatalf("structured = %+v", structured)
	}
	stream, err := projectRunLogRecord(api.RunLogChunk{
		ID: "tc1.stdout", RunID: "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc31",
		AttemptNumber: 2, Stream: string(workerapi.LogStreamStdout),
		ObservedSeq: 3, ContentBase64: "b2sK", Bytes: 3, At: at,
	}, "019c10d5-a6f7-7af1-8f5f-bb97bcc0dc38")
	if err != nil {
		t.Fatal(err)
	}
	if stream.Kind != "stdout" ||
		stream.ObservedSequence == nil ||
		*stream.ObservedSequence != 3 ||
		stream.Bytes == nil ||
		*stream.Bytes != 3 {
		t.Fatalf("stream = %+v", stream)
	}
}

func TestRunTelemetryFrontierFailsClosedWhileProjectionLags(t *testing.T) {
	orgID := uuid.MustParse("00000000-0000-0000-0000-000000000001")
	runID := uuid.MustParse("00000000-0000-0000-0000-000000000002")
	store := &runTelemetryFrontierStore{
		frontier: db.GetRunTelemetryFrontierRow{
			ObservedSeq: 12,
			PendingSeq:  11,
		},
	}
	server := &Server{db: store}
	request := httptest.NewRequest("GET", "/", nil)
	err := server.requireProjectedRunTelemetry(
		request,
		runTelemetryTarget{
			orgID: pgvalue.UUID(orgID),
			runID: pgvalue.UUID(runID),
		},
		db.TelemetryStreamKindRunLog,
		[]string{"error"},
		10,
		0,
		10,
		true,
	)
	var lagging telemetry.LaggingError
	if !errors.As(err, &lagging) ||
		lagging.WatermarkSeq != 10 ||
		lagging.WantSeq != 12 {
		t.Fatalf("error = %v", err)
	}
	if store.params.StreamKind != db.TelemetryStreamKindRunLog ||
		len(store.params.FilterValues) != 1 ||
		store.params.FilterValues[0] != "error" {
		t.Fatalf("params = %+v", store.params)
	}
}

func TestRunTelemetryFrontierChecksOnlyCandidatePageBoundary(t *testing.T) {
	orgID := uuid.MustParse("00000000-0000-0000-0000-000000000001")
	runID := uuid.MustParse("00000000-0000-0000-0000-000000000002")
	store := &runTelemetryFrontierStore{
		frontier: db.GetRunTelemetryFrontierRow{
			ObservedSeq: 100,
		},
	}
	server := &Server{db: store}
	err := server.requireProjectedRunTelemetry(
		httptest.NewRequest("GET", "/", nil),
		runTelemetryTarget{
			orgID: pgvalue.UUID(orgID),
			runID: pgvalue.UUID(runID),
		},
		db.TelemetryStreamKindRunLog,
		nil,
		10,
		100,
		100,
		false,
	)
	if err != nil {
		t.Fatalf("error = %v", err)
	}
	if store.params.ThroughSeq != 100 {
		t.Fatalf("frontier through seq = %d, want 100", store.params.ThroughSeq)
	}
}

func TestRunTelemetryExactLimitRetainsCandidatePageBoundary(t *testing.T) {
	if !hasRunTelemetryPageBoundary(100, 100) {
		t.Fatal("an exact-limit page must remain bounded")
	}
	if hasRunTelemetryPageBoundary(99, 100) {
		t.Fatal("a short page must run the unbounded completeness check")
	}
}
