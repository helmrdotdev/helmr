package controlplane

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"testing"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/api"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/telemetry"
	"github.com/jackc/pgx/v5/pgtype"
)

func TestCommandCancelHTTPPendingAndTerminalReplay(t *testing.T) {
	c := newCommandHTTP(t)
	command := c.exec(t, `{"command":["sleep","10"],"idempotency_key":"cancel-pending"}`)
	cancel := func() api.CommandCancelReceipt {
		t.Helper()
		var receipt api.CommandCancelReceipt
		if err := json.Unmarshal(c.expect(t, c.key, http.MethodPost, "/v1/commands/"+command.CommandID+"/cancel", "", http.StatusAccepted, ""), &receipt); err != nil {
			t.Fatal(err)
		}
		return receipt
	}
	first := cancel()
	if first.ID == "" || first.TargetID != command.CommandID || first.Status != "accepted" {
		t.Fatalf("receipt=%+v", first)
	}
	var status string
	var revision int64
	var terminal, requested bool
	if err := c.pool.QueryRow(t.Context(), `SELECT status,revision,terminal_at IS NOT NULL,cancel_requested_at IS NOT NULL FROM computer_commands WHERE id=$1`, command.CommandID).Scan(&status, &revision, &terminal, &requested); err != nil {
		t.Fatal(err)
	}
	if status != "cancelled" || !terminal || !requested {
		t.Fatalf("pending cancel status=%s terminal=%v requested=%v", status, terminal, requested)
	}
	if replay := cancel(); replay != first {
		t.Fatalf("replay=%+v first=%+v", replay, first)
	}
	var after int64
	if err := c.pool.QueryRow(t.Context(), `SELECT revision FROM computer_commands WHERE id=$1`, command.CommandID).Scan(&after); err != nil {
		t.Fatal(err)
	}
	if after != revision {
		t.Fatalf("replay mutated revision %d -> %d", revision, after)
	}
	var info api.CommandInfo
	if err := json.Unmarshal(c.expect(t, c.key, http.MethodGet, "/v1/commands/"+command.CommandID, "", http.StatusOK, ""), &info); err != nil {
		t.Fatal(err)
	}
	if info.Status != "cancelled" || info.Outcome == nil || info.Outcome.Kind != "cancelled" {
		t.Fatalf("retrieve=%+v", info)
	}
	c.expect(t, c.key, http.MethodPost, "/v1/commands/"+uuid.NewV7().String()+"/cancel", "", http.StatusNotFound, "computer_command_not_found")
}

// Pruning a Command's result expires its public result only: cancellation
// still replays its receipt and log history stays readable within its own
// retention.
func TestCommandCancelAndLogsSurviveResultPruning(t *testing.T) {
	at := time.Now().UTC().Truncate(time.Microsecond)
	sink := &commandLogTestReader{chunks: map[string][]telemetry.CommandLogChunk{
		"stdout": {{ObservedSeq: 0, Content: []byte("out"), ObservedAt: at}},
	}}
	c := newCommandHTTP(t, func(cfg *ServerConfig) { cfg.TelemetryReader = sink })
	command := c.exec(t, `{"command":["true"],"idempotency_key":"pruned"}`)
	commandID := pgvalue.UUID(uuid.MustParse(command.CommandID))
	if _, err := c.server.db.InsertCommandLogChunk(t.Context(), db.InsertCommandLogChunkParams{
		OrgID: pgvalue.UUID(c.orgID), ProjectID: pgvalue.UUID(c.projectID), EnvironmentID: pgvalue.UUID(c.environmentID), CommandID: commandID,
		StreamName: "stdout", Content: []byte("out"), ObservedSeq: 0, ObservedAt: pgtype.Timestamptz{Time: at, Valid: true},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.pool.Exec(t.Context(), `UPDATE telemetry_outbox SET status='written',written_at=now() WHERE command_id=$1`, commandID); err != nil {
		t.Fatal(err)
	}
	cancelPath := "/v1/commands/" + command.CommandID + "/cancel"
	var first api.CommandCancelReceipt
	if err := json.Unmarshal(c.expect(t, c.key, http.MethodPost, cancelPath, "", http.StatusAccepted, ""), &first); err != nil {
		t.Fatal(err)
	}
	if _, err := c.pool.Exec(t.Context(), `UPDATE computer_commands SET result_expires_at=now()-interval '1 day' WHERE id=$1`, commandID); err != nil {
		t.Fatal(err)
	}
	if pruned, err := c.server.db.PruneExpiredComputerCommandResults(t.Context(), 100); err != nil || pruned != 1 {
		t.Fatalf("prune result=%d, %v", pruned, err)
	}
	c.expect(t, c.key, http.MethodGet, "/v1/commands/"+command.CommandID, "", http.StatusGone, "command_result_expired")
	var replayed api.CommandCancelReceipt
	if err := json.Unmarshal(c.expect(t, c.key, http.MethodPost, cancelPath, "", http.StatusAccepted, ""), &replayed); err != nil || replayed != first {
		t.Fatalf("cancel replay=%+v, %v; want %+v", replayed, err, first)
	}
	var page api.CommandLogPage
	if err := json.Unmarshal(c.expect(t, c.key, http.MethodGet, "/v1/commands/"+command.CommandID+"/logs", "", http.StatusOK, ""), &page); err != nil {
		t.Fatal(err)
	}
	if len(page.Logs) != 1 || page.Logs[0].Kind != "output" || page.Logs[0].ContentBase64 != base64.StdEncoding.EncodeToString([]byte("out")) {
		t.Fatalf("logs after pruning=%+v", page)
	}
	c.expect(t, c.key, http.MethodGet, "/v1/commands/"+uuid.NewV7().String()+"/logs", "", http.StatusNotFound, "command_not_found")
}
