package db_test

import (
	"testing"

	"github.com/helmrdotdev/helmr/internal/agent/agenttest"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
)

func TestStoredDiagnosticIngestSizeTracksData(t *testing.T) {
	f := agenttest.New(t)
	ctx := t.Context()
	dbtest.MustExec(t, ctx, f.Pool, `INSERT INTO diagnostic_chunks(environment_id,session_id,process_epoch,stream,sequence,through_sequence,byte_offset,through_byte_offset,kind,observed_at_unix_nano,data,dropped_bytes,complete,accepted_at) VALUES($1,$2,1,'stdout',1,1,0,5,'data',1,convert_to('hello','UTF8'),0,false,now())`, f.Environment, f.Session)
	var size int64
	if err := f.Pool.QueryRow(ctx, `SELECT ingest_size_bytes FROM diagnostic_chunks WHERE session_id=$1`, f.Session).Scan(&size); err != nil || size != 5 {
		t.Fatalf("diagnostic size=%d %v", size, err)
	}
	if err := f.Pool.QueryRow(ctx, `UPDATE diagnostic_chunks SET data=convert_to('world!','UTF8'),through_byte_offset=6 WHERE session_id=$1 RETURNING ingest_size_bytes`, f.Session).Scan(&size); err != nil || size != 6 {
		t.Fatalf("updated diagnostic size=%d %v", size, err)
	}
}
