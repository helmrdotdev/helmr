package controlplane

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/computer"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
)

func TestComputerVersionRootRuntimeRetention(t *testing.T) {
	f, b, fence := initialKeyFixture(t)
	key, err := b.initial(t.Context(), fence)
	if err != nil {
		t.Fatal(err)
	}
	defer clear(key.Key)
	var computerID, versionID pgtype.UUID
	if err = f.Pool.QueryRow(t.Context(), `SELECT workspace_id,reserved_workspace_version_id FROM runtime_instances WHERE id=$1`, f.runtime).Scan(&computerID, &versionID); err != nil {
		t.Fatal(err)
	}
	env := pgvalue.UUID(f.EnvironmentID)
	q := db.New(f.Pool)
	digest := dbtest.Digest("generation-root-pack")
	root := computer.GenerationRoot{FormatVersion: 1, LogicalBytes: f.request.LogicalBytes, Offset: 128,
		Pack: computer.GenerationPack{Digest: digest, SizeBytes: 512, Rank: 2},
		Page: computer.GenerationPage{Digest: dbtest.Digest("generation-root-page"), Salt: strings.Repeat("aa", 32), KeyID: key.ID, Kind: 3, Count: 1, SizeBytes: 64}}
	if err = root.Validate(f.request.LogicalBytes); err != nil {
		t.Fatal(err)
	}
	encode := func(r computer.GenerationRoot) []byte {
		t.Helper()
		v, e := json.Marshal(r)
		if e != nil {
			t.Fatal(e)
		}
		return v
	}
	params := db.CreateComputerVersionRootParams{EnvironmentID: env, ComputerID: computerID, VersionID: versionID, Locator: encode(root)}
	integrity := func(err error) {
		t.Helper()
		var pg *pgconn.PgError
		if !errors.As(err, &pg) || (pg.Code != "23503" && pg.Code != "23001") {
			t.Fatalf("expected scoped retention rejection: %v", err)
		}
	}
	dbtest.MustExec(t, t.Context(), f.Pool, `INSERT INTO cas_object_lifetimes(digest) VALUES($1)`, digest)
	dbtest.MustExec(t, t.Context(), f.Pool, `INSERT INTO cas_objects(org_id,digest,size_bytes,media_type) VALUES($1,$2,512,'application/octet-stream')`, f.OrgID, digest)
	dbtest.MustExec(t, t.Context(), f.Pool, `INSERT INTO computer_objects(environment_id,computer_id,digest,org_id,project_id,size_bytes,media_type,kind,rank,inspection) VALUES($1,$2,$3,$4,$5,512,'application/octet-stream','root',2,'{}')`, env, computerID, digest, f.OrgID, f.ProjectID)
	dbtest.MustExec(t, t.Context(), f.Pool, `INSERT INTO computer_object_keys(environment_id,computer_id,digest,key_id,is_direct) VALUES($1,$2,$3,$4,true)`, env, computerID, digest, key.ID)
	integrity(q.CreateComputerVersionRoot(t.Context(), params))
	if n, err := q.CertifyComputerObject(t.Context(), db.CertifyComputerObjectParams{EnvironmentID: env, ComputerID: computerID, Digest: digest}); err != nil || n != 1 {
		t.Fatalf("certify root: %d %v", n, err)
	}
	for _, mutate := range []func(*computer.GenerationRoot){func(r *computer.GenerationRoot) { r.Pack.SizeBytes++ }, func(r *computer.GenerationRoot) { r.Pack.Rank++ }, func(r *computer.GenerationRoot) { r.Page.KeyID = uuid.NewV7().String() }} {
		bad := root
		mutate(&bad)
		p := params
		p.Locator = encode(bad)
		integrity(q.CreateComputerVersionRoot(t.Context(), p))
	}
	// Exact size/rank/certification are insufficient: an index pack is not a root.
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE computer_objects SET kind='index' WHERE digest=$1`, digest)
	integrity(q.CreateComputerVersionRoot(t.Context(), params))
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE computer_objects SET kind='root' WHERE digest=$1`, digest)
	for _, capacity := range []int64{0, -4096, 4097} {
		bad := root
		bad.LogicalBytes = capacity
		p := params
		p.Locator = encode(bad)
		var pg *pgconn.PgError
		if e := q.CreateComputerVersionRoot(t.Context(), p); !errors.As(e, &pg) || pg.Code != "23514" {
			t.Fatalf("invalid persisted capacity %d accepted: %v", capacity, e)
		}
	}
	// Merely belonging to the transitive read-key set does not authorize a key
	// as the root page's own encryption key. Roll back this isolated fixture.
	inheritedTx, err := f.Pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer inheritedTx.Rollback(context.Background())
	inheritedKey := pgvalue.NewUUIDv7()
	dbtest.MustExec(t, t.Context(), inheritedTx, `INSERT INTO computer_data_keys(id,environment_id,computer_id,wrapping_key_id,wrapped_key) VALUES($1,$2,$3,'fixture',decode('01','hex'))`, inheritedKey, env, computerID)
	dbtest.MustExec(t, t.Context(), inheritedTx, `INSERT INTO computer_object_keys(environment_id,computer_id,digest,key_id,is_direct) VALUES($1,$2,$3,$4,false)`, env, computerID, digest, inheritedKey)
	inheritedRoot := root
	inheritedRoot.Page.KeyID = pgvalue.UUIDString(inheritedKey)
	inheritedParams := params
	inheritedParams.Locator = encode(inheritedRoot)
	integrity(db.New(inheritedTx).CreateComputerVersionRoot(t.Context(), inheritedParams))
	if err = inheritedTx.Rollback(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err = q.CreateComputerVersionRoot(t.Context(), params); err != nil {
		t.Fatal(err)
	}
	raw, err := q.GetComputerVersionRoot(t.Context(), db.GetComputerVersionRootParams{EnvironmentID: env, ComputerID: computerID, VersionID: versionID})
	if err != nil {
		t.Fatal(err)
	}
	if got, err := computer.ParseGenerationRoot(raw, f.request.LogicalBytes); err != nil || got != root {
		t.Fatalf("stored full locator changed: %v", err)
	}
	pin := db.PinRuntimeComputerSourceParams{RuntimeInstanceID: f.runtime, EnvironmentID: env, ComputerID: computerID, VersionID: versionID}
	// Physical deletion racing first admission must either lose to the FK pin
	// or cause admission to fail. Force deletion to hold the root row first.
	locker, err := f.Pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer locker.Rollback(context.Background())
	dbtest.MustExec(t, t.Context(), locker, `DELETE FROM computer_version_roots WHERE environment_id=$1 AND computer_id=$2 AND version_id=$3`, env, computerID, versionID)
	var pid int
	if err = locker.QueryRow(t.Context(), `SELECT pg_backend_pid()`).Scan(&pid); err != nil {
		t.Fatal(err)
	}
	result := make(chan error, 1)
	go func() { _, e := q.PinRuntimeComputerSource(t.Context(), pin); result <- e }()
	timeout := time.NewTimer(5 * time.Second)
	defer timeout.Stop()
	tick := time.NewTicker(5 * time.Millisecond)
	defer tick.Stop()
	for {
		var blocked bool
		if err = f.Pool.QueryRow(t.Context(), `SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE datname=current_database() AND $1=ANY(pg_blocking_pids(pid)))`, pid).Scan(&blocked); err != nil {
			t.Fatal(err)
		}
		if blocked {
			break
		}
		select {
		case <-timeout.C:
			t.Fatal("source pin did not wait on retiring root")
		case <-tick.C:
		}
	}
	if err = locker.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
	integrity(<-result)
	var pinned pgtype.UUID
	if err = f.Pool.QueryRow(t.Context(), `SELECT computer_source_version_id FROM runtime_instances WHERE id=$1`, f.runtime).Scan(&pinned); err != nil || pinned.Valid {
		t.Fatal("failed admission retained source")
	}
	// Recreate only the fixture root, with wrong capacity, before any owner pins it.
	badCapacity := root
	badCapacity.LogicalBytes = 4096
	badParams := params
	badParams.Locator = encode(badCapacity)
	if err = q.CreateComputerVersionRoot(t.Context(), badParams); err != nil {
		t.Fatal(err)
	}
	if n, err := q.PinRuntimeComputerSource(t.Context(), pin); err != nil || n != 0 {
		t.Fatalf("wrong capacity admitted: %d %v", n, err)
	}
	dbtest.MustExec(t, t.Context(), f.Pool, `DELETE FROM computer_version_roots WHERE environment_id=$1 AND computer_id=$2 AND version_id=$3`, env, computerID, versionID)
	if err = q.CreateComputerVersionRoot(t.Context(), params); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if n, err := q.PinRuntimeComputerSource(t.Context(), pin); err != nil || n != 1 {
			t.Fatalf("pin replay: %d %v", n, err)
		}
	}
	wrong := pin
	wrong.VersionID = pgvalue.NewUUIDv7()
	if n, err := q.PinRuntimeComputerSource(t.Context(), wrong); err != nil || n != 0 {
		t.Fatalf("unreserved source accepted: %d %v", n, err)
	}
	keys, err := q.ListRuntimeComputerSourceKeys(t.Context(), f.runtime)
	if err != nil || len(keys) != 1 || pgvalue.UUIDString(keys[0].ID) != key.ID {
		t.Fatalf("runtime source keys: count=%d err=%v", len(keys), err)
	}
	deleteRoot := func() error {
		_, e := f.Pool.Exec(t.Context(), `DELETE FROM computer_version_roots WHERE environment_id=$1 AND computer_id=$2 AND version_id=$3`, env, computerID, versionID)
		return e
	}
	integrity(deleteRoot())
	// Reservation loss is not physical exclusion. The immutable source remains pinned.
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE runtime_instances SET observed_state='failed',terminal_at=clock_timestamp(),terminal_reason_code='fixture',reserved_run_id=NULL,reserved_attempt_number=NULL,reserved_workspace_version_id=NULL WHERE id=$1`, f.runtime)
	integrity(deleteRoot())
	keys, err = q.ListRuntimeComputerSourceKeys(t.Context(), f.runtime)
	if err != nil || len(keys) != 1 {
		t.Fatalf("source lost at terminal observation: %v", err)
	}
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE runtime_instances SET reclaimed_at=clock_timestamp(),reclaim_evidence='{"proof":"fixture"}' WHERE id=$1`, f.runtime)
	keys, err = q.ListRuntimeComputerSourceKeys(t.Context(), f.runtime)
	if err != nil || len(keys) != 0 {
		t.Fatalf("released runtime retained key delivery: %v", err)
	}
	if err = deleteRoot(); err != nil {
		t.Fatal(err)
	}
	var audit pgtype.UUID
	if err = f.Pool.QueryRow(t.Context(), `SELECT computer_source_version_id FROM runtime_instances WHERE id=$1`, f.runtime).Scan(&audit); err != nil || audit != versionID {
		t.Fatal("payload release lost audit identity")
	}
}
