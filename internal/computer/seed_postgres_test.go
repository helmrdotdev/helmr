package computer

import (
	"bytes"
	"context"
	"encoding/json"
	"sync"
	"testing"

	"github.com/helmrdotdev/helmr/internal/cas"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/disk/blockformat"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/jackc/pgx/v5/pgtype"
)

func TestSharedSeedKeyDoesNotAuthorizeUnrelatedObjects(t *testing.T) {
	first, input := newVersionFixture(t)
	second := first.sibling(t)
	key := first.seedKey(t)
	defer clear(key.Key)
	local, err := cas.NewFile(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	writer := blockformat.Writer{Source: local, Sink: local, Scope: key.Scope, ActiveKey: key.ID, Keys: map[string][]byte{key.ID: key.Key}, PackLimit: blockformat.MinPackLimit}
	other, err := writer.Empty(t.Context(), first.logicalBytes, 64)
	if err != nil {
		t.Fatal(err)
	}
	pack, err := blockformat.InspectPack(t.Context(), local, key.Scope, writer.Keys, other.Pack)
	if err != nil {
		t.Fatal(err)
	}
	unrelated := blockformat.ObjectInspection{Pack: &pack}
	first.certifyInitialObject(t, local, unrelated)
	if _, err = first.publisher.PublishInitialVersion(t.Context(), first.principal, first.ref, input); err != nil {
		t.Fatal(err)
	}
	if result, err := second.broker.PrepareSeed(t.Context(), second.principal, second.ref); err != nil || result.Status != "ready" {
		t.Fatalf("adoption: %s %v", result.Status, err)
	}
	source, err := second.broker.SourceKeys(t.Context(), second.principal, second.ref)
	if err != nil {
		t.Fatal(err)
	}
	defer source.Clear()
	var computerID pgtype.UUID
	if err = first.Pool.QueryRow(t.Context(), `SELECT computer_id FROM computer_instances WHERE id=$1`, second.instance).Scan(&computerID); err != nil {
		t.Fatal(err)
	}
	scope := objectScope{objectRetention: objectRetention{environmentID: pgvalue.UUID(first.EnvironmentID), computerID: computerID, instanceID: second.instance, desiredVersion: 1, key: initialPublicationKey(second.ref.InstanceID)}, logicalBytes: first.logicalBytes, orgID: pgvalue.UUID(first.OrgID), projectID: pgvalue.UUID(first.ProjectID), allowedKeys: map[string]bool{}, writeKey: source.WriteKeyID}
	for _, material := range source.Keys {
		scope.allowedKeys[material.ID] = true
	}
	var raw []byte
	if err = first.Pool.QueryRow(t.Context(), `SELECT inspection FROM computer_objects WHERE digest=$1`, input.Root.Pack.Digest).Scan(&raw); err != nil {
		t.Fatal(err)
	}
	var admitted blockformat.ObjectInspection
	if err = json.Unmarshal(raw, &admitted); err != nil {
		t.Fatal(err)
	}
	for name, operation := range map[string]func(blockformat.ObjectInspection) error{
		"reuse": func(e blockformat.ObjectInspection) error {
			tx, err := first.Pool.Begin(t.Context())
			if err != nil {
				return err
			}
			defer tx.Rollback(t.Context())
			return scope.reuseObject(t.Context(), tx, e)
		},
		"register": func(e blockformat.ObjectInspection) error {
			tx, err := first.Pool.Begin(t.Context())
			if err != nil {
				return err
			}
			defer tx.Rollback(t.Context())
			return scope.registerObject(t.Context(), tx, e)
		},
	} {
		t.Run(name, func(t *testing.T) {
			if err := operation(admitted); err != nil {
				t.Fatalf("retained source rejected: %v", err)
			}
			if err := operation(unrelated); err == nil {
				t.Fatal("shared read key admitted an unrelated certified object")
			}
		})
	}
	// Read access permits reuse, but cannot mint new ciphertext with a shared key.
	for _, active := range []string{key.ID, source.WriteKeyID} {
		materials := map[string][]byte{}
		for _, material := range source.Keys {
			materials[material.ID] = material.Key
		}
		candidateWriter := blockformat.Writer{Source: local, Sink: local, Scope: source.Scope, ActiveKey: active, Keys: materials, PackLimit: blockformat.MinPackLimit}
		root, err := candidateWriter.Empty(t.Context(), first.logicalBytes, 256)
		if err != nil {
			t.Fatal(err)
		}
		pack, err := blockformat.InspectPack(t.Context(), local, source.Scope, materials, root.Pack)
		if err != nil {
			t.Fatal(err)
		}
		tx, err := first.Pool.Begin(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		err = scope.registerObject(t.Context(), tx, blockformat.ObjectInspection{Pack: &pack})
		tx.Rollback(t.Context())
		if active == source.WriteKeyID && err != nil {
			t.Fatalf("private write key rejected: %v", err)
		}
		if active == key.ID && err == nil {
			t.Fatal("shared read key admitted new ciphertext")
		}
	}

}

func (f preparationFixture) sibling(t *testing.T) preparationFixture {
	t.Helper()
	q := db.New(f.Pool)
	c, err := q.CreateComputerFromCurrentDeployment(t.Context(), db.CreateComputerFromCurrentDeploymentParams{ID: pgvalue.NewUUIDv7(), InitialVersionID: pgvalue.NewUUIDv7(), OrgID: pgvalue.UUID(f.OrgID), ProjectID: pgvalue.UUID(f.ProjectID), EnvironmentID: pgvalue.UUID(f.EnvironmentID), DeploymentDefinitionID: pgvalue.UUID(f.ComputerDefinitionID), SandboxDeclaredID: declaredID})
	if err != nil {
		t.Fatal(err)
	}
	original, err := q.GetComputerInstance(t.Context(), db.GetComputerInstanceParams{EnvironmentID: c.EnvironmentID, ID: f.instance})
	if err != nil {
		t.Fatal(err)
	}
	i, err := q.AllocateComputerInstance(t.Context(), db.AllocateComputerInstanceParams{ID: pgvalue.NewUUIDv7(), EnvironmentID: c.EnvironmentID, ComputerID: c.ID, ComputerSpecID: c.ComputerSpecID, WorkerGroupID: original.WorkerGroupID, WorkerHostID: original.WorkerHostID, WorkerEpoch: original.WorkerEpoch, VMPlatformID: original.VMPlatformID, VMVCPUCount: original.VMVCPUCount, CPUConfigDigest: original.CPUConfigDigest, ReservedCPUMillis: original.ReservedCPUMillis, ReservedMemoryBytes: original.ReservedMemoryBytes, ReservedGuestEphemeralDiskBytes: f.logicalBytes, ReservedExecutionSlots: 1, PreparationSeconds: 300, WriterTtlSeconds: 600, WriterGeneration: 1, WriterTokenHash: make([]byte, 32), Reason: "computer_preparation"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = q.ChargeComputerPreparation(t.Context(), db.ChargeComputerPreparationParams{ComputerID: c.ID, InstanceID: i.ID}); err != nil {
		t.Fatal(err)
	}
	f.instance = i.ID
	f.ref = PreparationRef{InstanceID: pgvalue.MustUUIDValue(i.ID), DesiredVersion: 1}
	return f
}

func TestSharedSeedConcurrentClaimAndTakeover(t *testing.T) {
	first := newPreparationFixture(t)
	second := first.sibling(t)
	type result struct {
		owner preparationFixture
		value SeedPreparation
		err   error
	}
	start := make(chan struct{})
	results := make(chan result, 2)
	for _, f := range []preparationFixture{first, second} {
		go func() {
			<-start
			value, err := f.broker.PrepareSeed(t.Context(), f.principal, f.ref)
			results <- result{f, value, err}
		}()
	}
	close(start)
	a, b := <-results, <-results
	if a.err != nil || b.err != nil {
		t.Fatalf("claims: %v %v", a.err, b.err)
	}
	if a.value.Status == "waiting" {
		a, b = b, a
	}
	defer clear(a.value.Key.Key)
	if a.value.Status != "convert" || b.value.Status != "waiting" || len(b.value.Key.Key) != 0 {
		t.Fatalf("claim outcomes: %s %s", a.value.Status, b.value.Status)
	}
	var keys int
	if err := first.Pool.QueryRow(t.Context(), `SELECT count(*) FROM computer_data_keys WHERE writer_computer_id IS NULL`).Scan(&keys); err != nil || keys != 1 {
		t.Fatalf("conversion keys=%d: %v", keys, err)
	}
	replay, err := a.owner.broker.PrepareSeed(t.Context(), a.owner.principal, a.owner.ref)
	if err != nil || replay.Status != "convert" || replay.Key.ID != a.value.Key.ID || !bytes.Equal(replay.Key.Key, a.value.Key.Key) {
		t.Fatalf("claim replay: %s %v", replay.Status, err)
	}
	clear(replay.Key.Key)
	// Expired authority permits a new converter but does not prove old physical
	// cleanup. Its key must remain until that exact Instance is reclaimed.
	dbtest.MustExec(t, t.Context(), first.Pool, `UPDATE computer_seeds SET lease_expires_at=clock_timestamp()-interval '1 second'`)
	next, err := b.owner.broker.PrepareSeed(t.Context(), b.owner.principal, b.owner.ref)
	if err != nil || next.Status != "convert" || next.Key.ID == a.value.Key.ID {
		t.Fatalf("takeover: %s %v", next.Status, err)
	}
	defer clear(next.Key.Key)
	if stale, err := a.owner.broker.PrepareSeed(t.Context(), a.owner.principal, a.owner.ref); err == nil || len(stale.Key.Key) != 0 {
		t.Fatal("superseded converter received key", err)
	}
	var keyID pgtype.UUID
	if err = keyID.Scan(a.value.Key.ID); err != nil {
		t.Fatal(err)
	}
	n, err := db.New(first.Pool).RetireUnreferencedComputerKey(t.Context(), keyID)
	if err != nil || n != 0 {
		t.Fatalf("unreconciled seed key retired: %d %v", n, err)
	}
}

func TestSharedSeedProviderRechecksAuthority(t *testing.T) {
	for _, stage := range []string{"wrap", "unwrap", "cancel"} {
		t.Run(stage, func(t *testing.T) {
			f := newPreparationFixture(t)
			private := f.initialKey(t)
			clear(private.Key)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			observer := &observingKeyWrapper{KeyWrapper: f.broker.wrapper}
			expire := func() {
				dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE computer_instances SET preparation_expires_at=clock_timestamp()-interval '1 second' WHERE id=$1`, f.instance)
			}
			switch stage {
			case "wrap":
				observer.wrap = expire
			case "unwrap":
				observer.unwrap = expire
			case "cancel":
				observer.unwrap = cancel
			}
			f.broker.wrapper = observer
			result, err := f.broker.PrepareSeed(ctx, f.principal, f.ref)
			if err == nil || len(result.Key.Key) != 0 {
				clear(result.Key.Key)
				t.Fatal("late seed key escaped its authority")
			}
			if stage == "wrap" {
				var count int
				if err := f.Pool.QueryRow(t.Context(), `SELECT count(*) FROM computer_data_keys WHERE is_seed_key`).Scan(&count); err != nil || count != 0 {
					t.Fatalf("expired wrap retained key: %d %v", count, err)
				}
			} else if len(observer.returned) == 0 || !bytes.Equal(observer.returned, make([]byte, len(observer.returned))) {
				t.Fatal("unreleased plaintext was not erased")
			}
		})
	}
}

func TestSharedSeedAdoptionRetainsIndependentRootsAndWriteKeys(t *testing.T) {
	first, input := newVersionFixture(t)
	second := first.sibling(t)
	third := first.sibling(t)
	original, err := first.publisher.PublishInitialVersion(t.Context(), first.principal, first.ref, input)
	if err != nil {
		t.Fatal(err)
	}
	// Independent consumers adopt the same retained descriptor without another
	// conversion, with independent initial history and private write authority.
	var wg sync.WaitGroup
	for _, f := range []preparationFixture{second, third} {
		wg.Go(func() {
			prepared, err := f.broker.PrepareSeed(t.Context(), f.principal, f.ref)
			if err != nil || prepared.Status != "ready" {
				t.Errorf("adoption: %s %v", prepared.Status, err)
			}
		})
	}
	wg.Wait()
	var versions, roots, writers, objects int
	if err = first.Pool.QueryRow(t.Context(), `SELECT count(DISTINCT v.version_id),count(DISTINCT v.root_id),count(DISTINCT i.write_key_id),(SELECT count(*) FROM computer_objects)
 FROM computer_instances i JOIN computer_disk_version_roots v ON v.version_id=i.source_disk_version_id
 WHERE i.id=ANY($1::uuid[])`, []string{first.ref.InstanceID.String(), second.ref.InstanceID.String(), third.ref.InstanceID.String()}).Scan(&versions, &roots, &writers, &objects); err != nil || versions != 3 || roots != 1 || writers != 3 || objects != 1 {
		t.Fatalf("versions=%d roots=%d writers=%d objects=%d: %v", versions, roots, writers, objects, err)
	}
	firstSource, err := first.broker.SourceKeys(t.Context(), first.principal, first.ref)
	if err != nil {
		t.Fatal(err)
	}
	defer firstSource.Clear()
	secondSource, err := second.broker.SourceKeys(t.Context(), second.principal, second.ref)
	if err != nil {
		t.Fatal(err)
	}
	defer secondSource.Clear()
	if firstSource.Root != secondSource.Root || firstSource.Scope != secondSource.Scope || firstSource.WriteKeyID == secondSource.WriteKeyID || firstSource.WriteKeyID == input.Root.Page.KeyID || secondSource.WriteKeyID == input.Root.Page.KeyID {
		t.Fatal("shared root acquired shared write authority")
	}
	for _, key := range secondSource.Keys {
		if key.ID == firstSource.WriteKeyID {
			t.Fatal("sibling active write key was released")
		}
	}
	// Delete original logical ownership after proven physical exclusion. The seed
	// and sibling's versions each retain the root independently.
	dbtest.MustExec(t, t.Context(), first.Pool, `UPDATE computer_instances SET desired_state='closed',desired_version=desired_version+1,observed_state='closed',terminal_at=clock_timestamp(),terminal_reason_code='fixture',admission_state='closed',mount_state='lost',reclaimed_at=clock_timestamp(),reclaim_evidence='{}' WHERE id=$1`, first.instance)
	dbtest.MustExec(t, t.Context(), first.Pool, `UPDATE computers SET status='deleted',desired_state='deleted',deleted_at=clock_timestamp(),head_disk_version_id=NULL,write_key_id=NULL,sandbox_declared_id=NULL WHERE id=$1`, original.ComputerID)
	retention := &Retention{pool: first.Pool, queries: db.New(first.Pool)}
	if err = retention.collectComputerDiskVersions(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, err = retention.queries.DeleteUnreferencedComputerDiskRoots(t.Context(), 100); err != nil {
		t.Fatal(err)
	}
	got, err := second.broker.SourceKeys(t.Context(), second.principal, second.ref)
	if err != nil || got.Root != input.Root {
		t.Fatalf("source deletion lost sibling root: %v", err)
	}
	got.Clear()
	ready, err := second.broker.PrepareSeed(context.Background(), second.principal, second.ref)
	if err != nil || ready.Status != "ready" {
		t.Fatalf("ready reply replay: %s %v", ready.Status, err)
	}
}
