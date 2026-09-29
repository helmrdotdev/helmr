package controlplane

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/computerkey"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/disk"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/workerapi"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
)

func TestComputerDiskVersionRootRuntimeRetention(t *testing.T) {
	f, b, fence := initialKeyFixture(t)
	key, err := b.initial(t.Context(), fence)
	if err != nil {
		t.Fatal(err)
	}
	defer clear(key.Key)
	var computerID, versionID pgtype.UUID
	if err = f.Pool.QueryRow(t.Context(), `SELECT i.computer_id,c.head_disk_version_id FROM computer_instances i JOIN computers c ON c.id=i.computer_id WHERE i.id=$1`, f.runtime).Scan(&computerID, &versionID); err != nil {
		t.Fatal(err)
	}
	env := pgvalue.UUID(f.EnvironmentID)
	q := db.New(f.Pool)
	digest := dbtest.Digest("generation-root-pack")
	root := disk.GenerationRoot{FormatVersion: 1, LogicalBytes: f.logicalBytes, Offset: 128,
		Pack: disk.GenerationPack{Digest: digest, SizeBytes: 512, Rank: 2},
		Page: disk.GenerationPage{Digest: dbtest.Digest("generation-root-page"), Salt: strings.Repeat("aa", 32), KeyID: key.ID, Kind: 3, Count: 1, SizeBytes: 64}}
	if err = root.Validate(f.logicalBytes); err != nil {
		t.Fatal(err)
	}
	encode := func(r disk.GenerationRoot) []byte {
		t.Helper()
		v, e := json.Marshal(r)
		if e != nil {
			t.Fatal(e)
		}
		return v
	}
	params := db.CreateComputerDiskVersionRootParams{EnvironmentID: env, ComputerID: computerID, VersionID: versionID, Locator: encode(root)}
	integrity := func(err error) {
		t.Helper()
		var pg *pgconn.PgError
		if !errors.As(err, &pg) || (pg.Code != "23503" && pg.Code != "23001") {
			t.Fatalf("expected scoped retention rejection: %v", err)
		}
	}
	dbtest.MustExec(t, t.Context(), f.Pool, `INSERT INTO cas_blobs(digest,size_bytes) VALUES($1,512)`, digest)
	dbtest.MustExec(t, t.Context(), f.Pool, `INSERT INTO cas_objects(org_id,digest,size_bytes,media_type) VALUES($1,$2,512,'application/octet-stream')`, f.OrgID, digest)
	dbtest.MustExec(t, t.Context(), f.Pool, `INSERT INTO computer_objects(environment_id,computer_id,digest,org_id,project_id,size_bytes,media_type,kind,rank,inspection) VALUES($1,$2,$3,$4,$5,512,'application/octet-stream','root',2,'{}')`, env, computerID, digest, f.OrgID, f.ProjectID)
	dbtest.MustExec(t, t.Context(), f.Pool, `INSERT INTO computer_object_keys(environment_id,computer_id,digest,key_id,is_direct) VALUES($1,$2,$3,$4,true)`, env, computerID, digest, key.ID)
	secondID := uuid.NewV7().String()
	secondEnvelope, err := b.wrapper.Wrap(t.Context(), key.Scope, secondID, bytes.Repeat([]byte{8}, 32))
	if err != nil {
		t.Fatal(err)
	}
	dbtest.MustExec(t, t.Context(), f.Pool, `INSERT INTO computer_data_keys(id,environment_id,computer_id,wrapping_key_id,wrapped_key) VALUES($1,$2,$3,$4,$5)`, secondID, env, computerID, secondEnvelope.WrappingKeyID, secondEnvelope.Ciphertext)
	dbtest.MustExec(t, t.Context(), f.Pool, `INSERT INTO computer_object_keys(environment_id,computer_id,digest,key_id,is_direct) VALUES($1,$2,$3,$4,false)`, env, computerID, digest, secondID)
	integrity(q.CreateComputerDiskVersionRoot(t.Context(), params))
	if n, err := q.CertifyComputerObject(t.Context(), db.CertifyComputerObjectParams{EnvironmentID: env, ComputerID: computerID, Digest: digest}); err != nil || n != 1 {
		t.Fatalf("certify root: %d %v", n, err)
	}
	for _, mutate := range []func(*disk.GenerationRoot){func(r *disk.GenerationRoot) { r.Pack.SizeBytes++ }, func(r *disk.GenerationRoot) { r.Pack.Rank++ }, func(r *disk.GenerationRoot) { r.Page.KeyID = uuid.NewV7().String() }} {
		bad := root
		mutate(&bad)
		p := params
		p.Locator = encode(bad)
		integrity(q.CreateComputerDiskVersionRoot(t.Context(), p))
	}
	// Exact size/rank/certification are insufficient: an index pack is not a root.
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE computer_objects SET kind='index' WHERE digest=$1`, digest)
	integrity(q.CreateComputerDiskVersionRoot(t.Context(), params))
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE computer_objects SET kind='root' WHERE digest=$1`, digest)
	for _, capacity := range []int64{0, -4096, 4097} {
		bad := root
		bad.LogicalBytes = capacity
		p := params
		p.Locator = encode(bad)
		var pg *pgconn.PgError
		if e := q.CreateComputerDiskVersionRoot(t.Context(), p); !errors.As(e, &pg) || pg.Code != "23514" {
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
	integrity(db.New(inheritedTx).CreateComputerDiskVersionRoot(t.Context(), inheritedParams))
	if err = inheritedTx.Rollback(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err = q.CreateComputerDiskVersionRoot(t.Context(), params); err != nil {
		t.Fatal(err)
	}
	raw, err := q.GetComputerDiskVersionRoot(t.Context(), db.GetComputerDiskVersionRootParams{EnvironmentID: env, ComputerID: computerID, VersionID: versionID})
	if err != nil {
		t.Fatal(err)
	}
	if got, err := disk.ParseGenerationRoot(raw, f.logicalBytes); err != nil || got != root {
		t.Fatalf("stored full locator changed: %v", err)
	}
	if _, err := q.GetInstanceComputerSourceRoot(t.Context(), f.runtime); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatal("unretained version exposed", err)
	}
	pin := db.PinInstanceComputerSourceParams{ComputerInstanceID: f.runtime, EnvironmentID: env, ComputerID: computerID, VersionID: versionID}
	// Physical deletion racing first admission must either lose to the FK pin
	// or cause admission to fail. Force deletion to hold the root row first.
	locker, err := f.Pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer locker.Rollback(context.Background())
	dbtest.MustExec(t, t.Context(), locker, `DELETE FROM computer_disk_version_roots WHERE environment_id=$1 AND computer_id=$2 AND version_id=$3`, env, computerID, versionID)
	var pid int
	if err = locker.QueryRow(t.Context(), `SELECT pg_backend_pid()`).Scan(&pid); err != nil {
		t.Fatal(err)
	}
	result := make(chan error, 1)
	go func() { _, e := q.PinInstanceComputerSource(t.Context(), pin); result <- e }()
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
	if err = f.Pool.QueryRow(t.Context(), `SELECT source_disk_version_id FROM computer_instances WHERE id=$1`, f.runtime).Scan(&pinned); err != nil || pinned.Valid {
		t.Fatal("failed admission retained source")
	}
	// Recreate only the fixture root, with wrong capacity, before any owner pins it.
	badCapacity := root
	badCapacity.LogicalBytes = 4096
	badParams := params
	badParams.Locator = encode(badCapacity)
	if err = q.CreateComputerDiskVersionRoot(t.Context(), badParams); err != nil {
		t.Fatal(err)
	}
	if n, err := q.PinInstanceComputerSource(t.Context(), pin); err != nil || n != 0 {
		t.Fatalf("wrong capacity admitted: %d %v", n, err)
	}
	dbtest.MustExec(t, t.Context(), f.Pool, `DELETE FROM computer_disk_version_roots WHERE environment_id=$1 AND computer_id=$2 AND version_id=$3`, env, computerID, versionID)
	if err = q.CreateComputerDiskVersionRoot(t.Context(), params); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if n, err := q.PinInstanceComputerSource(t.Context(), pin); err != nil || n != 1 {
			t.Fatalf("pin replay: %d %v", n, err)
		}
	}
	wrong := pin
	wrong.VersionID = pgvalue.NewUUIDv7()
	if n, err := q.PinInstanceComputerSource(t.Context(), wrong); err != nil || n != 0 {
		t.Fatalf("unreserved source accepted: %d %v", n, err)
	}
	assertRetained := func() {
		t.Helper()
		tx, err := f.Pool.Begin(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		defer tx.Rollback(context.Background())
		source, parsed, sourceKeys, err := loadRuntimeComputerGeneration(t.Context(), db.New(tx), f.runtime)
		if err != nil || source.VersionID != versionID || parsed != root || len(sourceKeys) != 2 || pgvalue.UUIDString(sourceKeys[0].ID) != key.ID {
			t.Fatalf("retained generation mismatch: %v", err)
		}
	}
	assertRetained()

	// Read-key delivery is unavailable while this version is still initializing.
	if _, err := b.source(t.Context(), fence); !errors.Is(err, errComputerKeyUnavailable) {
		t.Fatal("initial source key grant", err)
	}
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE computer_disk_versions SET status='committed',published_at=clock_timestamp(),root_pack_digest=$2,logical_bytes=$3,publisher_computer_instance_id=$4,publisher_desired_version=1,publication_request_fingerprint=decode(repeat('ab',32),'hex') WHERE id=$1`, versionID, digest, root.LogicalBytes, f.runtime)
	delivered, err := b.source(t.Context(), fence)
	if err != nil || delivered.Root != root || len(delivered.Keys) != 2 || !bytes.Equal(delivered.Keys[0].Key, key.Key) {
		t.Fatalf("source delivery: %v", err)
	}
	delivered.clear()
	client := sourceKeyHTTPClient(t, f, b, fence)
	wire, err := client.ComputerSource(t.Context(), workerapi.ComputerSourceRequest{ComputerInstanceID: pgvalue.UUIDString(f.runtime), DesiredVersion: 1})
	if err != nil || wire.Root != root || wire.VersionID != pgvalue.UUIDString(versionID) || wire.WriteKeyID != key.ID || len(wire.Keys) != 2 {
		t.Fatalf("authenticated source transport: %v", err)
	}
	wire.Clear()
	if _, err := client.ComputerSource(t.Context(), workerapi.ComputerSourceRequest{ComputerInstanceID: pgvalue.UUIDString(f.runtime), DesiredVersion: 2}); err == nil {
		t.Fatal("stale source fence accepted")
	}

	// Construct a continuation preparation with no write-key pin. Its retained
	// root still needs both old read keys, while new writes use the Computer key.
	writeID := uuid.NewV7().String()
	writePlain := bytes.Repeat([]byte{9}, 32)
	defer clear(writePlain)
	writeEnvelope, err := b.wrapper.Wrap(t.Context(), key.Scope, writeID, writePlain)
	if err != nil {
		t.Fatal(err)
	}
	dbtest.MustExec(t, t.Context(), f.Pool, `INSERT INTO computer_data_keys(id,environment_id,computer_id,wrapping_key_id,wrapped_key) VALUES($1,$2,$3,$4,$5)`, writeID, env, computerID, writeEnvelope.WrappingKeyID, writeEnvelope.Ciphertext)
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE computer_instances SET write_key_id=NULL WHERE id=$1`, f.runtime)
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE computers SET write_key_id=$2 WHERE id=$1`, computerID, writeID)
	for i := range 2 {
		wire, err := client.ComputerSource(t.Context(), workerapi.ComputerSourceRequest{ComputerInstanceID: pgvalue.UUIDString(f.runtime), DesiredVersion: 1})
		if err != nil {
			t.Fatal("distinct write key delivery", err)
		}
		if wire.Root != root || wire.WriteKeyID != writeID || len(wire.Keys) != 3 || wire.Keys[2].ID != writeID || !bytes.Equal(wire.Keys[2].Key, writePlain) {
			wire.Clear()
			t.Fatal("distinct write key not appended to retained closure")
		}
		wire.Clear()
		var pinned pgtype.UUID
		if err := f.Pool.QueryRow(t.Context(), `SELECT write_key_id FROM computer_instances WHERE id=$1`, f.runtime).Scan(&pinned); err != nil || pgvalue.UUIDString(pinned) != writeID {
			t.Fatal("runtime write key not pinned", err)
		}
		if i == 0 {
			// Changing the Computer's key cannot change an existing Runtime pin.
			dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE computers SET write_key_id=$2 WHERE id=$1`, computerID, key.ID)
		}
	}
	assertRetained()

	failing := &partialSourceWrapper{ComputerKeyWrapper: b.wrapper}
	b.wrapper = failing
	if _, err = b.source(t.Context(), fence); !errors.Is(err, errComputerKeyUnavailable) {
		t.Fatal("partial unwrap accepted", err)
	}
	if len(failing.returned) != 2 {
		t.Fatal("partial unwrap not exercised")
	}
	for _, plain := range failing.returned {
		if !bytes.Equal(plain, make([]byte, len(plain))) {
			t.Fatal("partial plaintext retained")
		}
	}
	b.wrapper = failing.ComputerKeyWrapper

	observer := &observingKeyWrapper{ComputerKeyWrapper: b.wrapper}
	bumped := false
	observer.unwrap = func() {
		if bumped {
			return
		}
		bumped = true
		dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE worker_hosts SET claim_version=claim_version+1 WHERE id=$1`, f.runtimeWorker())
	}
	b.wrapper = observer
	if _, err = b.source(t.Context(), fence); !errors.Is(err, errStaleWorkerClaims) {
		t.Fatal("worker with stale claims received source keys", err)
	}
	if !bytes.Equal(observer.returned, make([]byte, len(observer.returned))) {
		t.Fatal("stale-claims plaintext not cleared")
	}
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE worker_hosts SET claim_version=claim_version-1 WHERE id=$1`, f.runtimeWorker())
	b.wrapper = observer.ComputerKeyWrapper

	// A database failure during final revalidation stays retryable unavailability.
	f.server.computerKeys = b
	faulted, restore := finalClaimReadFailure(b)
	response := invokeComputerKeyHandler(t, f.server.workerComputerSource, fence, workerapi.ComputerSourceRequest{ComputerInstanceID: pgvalue.UUIDString(fence.RuntimeID), DesiredVersion: fence.DesiredVersion})
	restore()
	if response.Code != http.StatusServiceUnavailable {
		t.Fatalf("final source claim read failure status=%d body=%s", response.Code, response.Body.String())
	}
	if len(faulted.returned) == 0 || !bytes.Equal(faulted.returned, make([]byte, len(faulted.returned))) {
		t.Fatal("source plaintext not cleared after final claim read failure")
	}

	// A Group claim change between authentication and the source locks asks the
	// Worker to re-authenticate; the replay receives the same source material.
	expected, err := b.source(t.Context(), fence)
	if err != nil {
		t.Fatal(err)
	}
	defer expected.clear()
	const sourcePath = "/worker/v1/run/computer-instances/computer-source"
	race := newWorkerClaimsRace(t, f.Fixture, f.server, map[string]http.HandlerFunc{sourcePath: f.server.workerComputerSource}, map[string]func(context.Context) error{sourcePath: switchPrimaryPool(t, f.Fixture)})
	replayed, err := race.client.ComputerSource(t.Context(), workerapi.ComputerSourceRequest{ComputerInstanceID: pgvalue.UUIDString(fence.RuntimeID), DesiredVersion: fence.DesiredVersion})
	if err != nil {
		t.Fatalf("source across primary pool switch: %v", err)
	}
	if replayed.VersionID != expected.VersionID || replayed.WriteKeyID != expected.WriteKeyID || len(replayed.Keys) != len(expected.Keys) {
		t.Fatalf("replayed source differs: version=%s keys=%d", replayed.VersionID, len(replayed.Keys))
	}
	for i, k := range expected.Keys {
		if replayed.Keys[i].ID != k.ID || !bytes.Equal(replayed.Keys[i].Key, k.Key) {
			t.Fatalf("replayed source key %d differs", i)
		}
	}
	replayed.Clear()
	race.requireReplayed(t, sourcePath)
	keys, err := q.ListInstanceComputerSourceKeys(t.Context(), f.runtime)
	if err != nil || len(keys) != 2 || pgvalue.UUIDString(keys[0].ID) != key.ID {
		t.Fatalf("runtime source keys: count=%d err=%v", len(keys), err)
	}
	deleteRoot := func() error {
		_, e := f.Pool.Exec(t.Context(), `DELETE FROM computer_disk_version_roots WHERE environment_id=$1 AND computer_id=$2 AND version_id=$3`, env, computerID, versionID)
		return e
	}
	integrity(deleteRoot())
	// Reservation loss is not physical exclusion. The immutable source remains pinned.
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE computer_instances SET observed_state='failed',terminal_at=clock_timestamp(),terminal_reason_code='fixture' WHERE id=$1`, f.runtime)
	integrity(deleteRoot())
	assertRetained()
	keys, err = q.ListInstanceComputerSourceKeys(t.Context(), f.runtime)
	if err != nil || len(keys) != 2 {
		t.Fatalf("source lost at terminal observation: %v", err)
	}
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE computer_instances SET admission_state='closed',mount_state='lost',reclaimed_at=clock_timestamp(),reclaim_evidence='{"proof":"fixture"}' WHERE id=$1`, f.runtime)
	if _, err := q.GetInstanceComputerSourceRoot(t.Context(), f.runtime); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatal("reclaimed source exposed", err)
	}
	keys, err = q.ListInstanceComputerSourceKeys(t.Context(), f.runtime)
	if err != nil || len(keys) != 0 {
		t.Fatalf("released runtime retained key delivery: %v", err)
	}
	if err = deleteRoot(); err != nil {
		t.Fatal(err)
	}
	var audit pgtype.UUID
	if err = f.Pool.QueryRow(t.Context(), `SELECT source_disk_version_id FROM computer_instances WHERE id=$1`, f.runtime).Scan(&audit); err != nil || audit != versionID {
		t.Fatal("payload release lost audit identity")
	}
}

type partialSourceWrapper struct {
	ComputerKeyWrapper
	returned [][]byte
}

func (w *partialSourceWrapper) Unwrap(ctx context.Context, scope, id string, e computerkey.Envelope) ([]byte, error) {
	key, err := w.ComputerKeyWrapper.Unwrap(ctx, scope, id, e)
	w.returned = append(w.returned, key)
	if len(w.returned) == 2 {
		return key, errors.New("injected second unwrap failure")
	}
	return key, err
}
