package controlplane

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/session/sessiontest"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Model command observations are separate from native agent settlement timings.
func retainNativeModelDiagnostics(ctx context.Context, pool *pgxpool.Pool, env uuid.UUID, computer, evidence string) error {
	var raw []byte
	err := pool.QueryRow(ctx, `SELECT COALESCE(jsonb_agg(jsonb_build_object(
 'session_id',d.session_id,'process_epoch',d.process_epoch,'stream',d.stream,'kind',d.kind,
 'sequence',d.sequence,'through_sequence',d.through_sequence,'byte_offset',d.byte_offset,
 'through_byte_offset',d.through_byte_offset,'data_base64',encode(d.data,'base64'))
 ORDER BY d.session_id,d.process_epoch,d.sequence),'[]'::jsonb)
 FROM diagnostic_chunks d JOIN sessions s ON(s.environment_id,s.id)=(d.environment_id,d.session_id)
 JOIN agents a ON(a.environment_id,a.id)=(s.environment_id,s.agent_id)
 WHERE d.environment_id=$1 AND s.computer_id=$2 AND a.key='models' AND d.stream='stderr'`, env, computer).Scan(&raw)
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(evidence, "performance-model-diagnostics.json"), raw, 0600)
}

func TestNativePerformanceRetainsModelDiagnosticsSeparately(t *testing.T) {
	f := sessiontest.New(t)
	model, agent := uuid.NewV7(), uuid.NewV7()
	dbtest.MustExec(t, t.Context(), f.Pool, `INSERT INTO agents(environment_id,id,key) VALUES($1,$2,'models');
 INSERT INTO agent_definitions(environment_id,agent_id,deployment_id,template_id,has_init) VALUES($1,$2,$3,$3,false);
 INSERT INTO sessions(environment_id,id,agent_id,deployment_id,computer_id,root_session_id,causal_depth,origin_user_id) VALUES($1,$4,$2,$3,$5,$4,0,(SELECT origin_user_id FROM sessions WHERE environment_id=$1 ORDER BY created_at,id LIMIT 1));
 INSERT INTO session_processes(environment_id,session_id,epoch,computer_id,computer_lease_epoch,status) VALUES($1,$4,1,$5,1,'ready');`, pgx.QueryExecModeSimpleProtocol, f.Environment, agent, f.Deployment, model, f.Computer)
	observation := "{\"event\":\"native.performance.command_observation\",\"targetMiss\":true}\n"
	for _, seed := range []struct {
		session      uuid.UUID
		stream, data string
	}{
		{model, "stderr", observation}, {model, "stdout", "excluded model stdout"}, {f.Session, "stderr", "excluded native timing"},
	} {
		dbtest.MustExec(t, t.Context(), f.Pool, `INSERT INTO diagnostic_chunks(environment_id,session_id,process_epoch,stream,sequence,through_sequence,byte_offset,through_byte_offset,kind,observed_at_unix_nano,data,dropped_bytes,complete)
 VALUES($1,$2,1,$3,1,1,0,octet_length($4::bytea),'data',1,$4,0,false)`, f.Environment, seed.session, seed.stream, []byte(seed.data))
	}
	directory := t.TempDir()
	for _, computer := range []string{f.Computer.String(), uuid.NewV7().String()} {
		if err := retainNativeModelDiagnostics(t.Context(), f.Pool, f.Environment, computer, directory); err != nil {
			t.Fatal(err)
		}
		raw, err := os.ReadFile(filepath.Join(directory, "performance-model-diagnostics.json"))
		if err != nil {
			t.Fatal(err)
		}
		var records []struct {
			Session  string `json:"session_id"`
			Data     []byte `json:"data_base64"`
			Kind     string `json:"kind"`
			Sequence int64  `json:"sequence"`
		}
		if err := json.Unmarshal(raw, &records); err != nil {
			t.Fatal(err)
		}
		if computer != f.Computer.String() {
			if len(records) != 0 {
				t.Fatalf("another Computer selected models: %s", raw)
			}
			continue
		}
		if len(records) != 1 || records[0].Session != model.String() || records[0].Kind != "data" || records[0].Sequence != 1 || string(records[0].Data) != observation {
			t.Fatalf("model evidence missing or mixed with native diagnostics: %s", raw)
		}
	}
}
