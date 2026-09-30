package computer

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/oci"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/helmrdotdev/helmr/internal/workergroup"
	"github.com/jackc/pgx/v5/pgconn"
)

func TestInitialGenerationPublicationReplay(t *testing.T) {
	f, input := newGenerationFixture(t)
	type outcome struct {
		result Publication
		err    error
	}
	results := make(chan outcome, 2)
	for range 2 {
		go func() {
			v, err := f.publisher.PublishInitialVersion(t.Context(), f.principal, f.ref, input)
			results <- outcome{v, err}
		}()
	}
	first, second := <-results, <-results
	if first.err != nil || second.err != nil || first.result != second.result {
		t.Fatalf("concurrent publication: %+v %+v", first, second)
	}
	versionID := pgvalue.UUID(first.result.VersionID)
	var retained bool
	if err := f.Pool.QueryRow(t.Context(), `SELECT source_disk_version_id=$2 AND retained_source_disk_version_id=$2 FROM computer_instances WHERE id=$1`, f.runtime, versionID).Scan(&retained); err != nil || !retained {
		t.Fatalf("published generation is not retained by runtime: %v %v", retained, err)
	}
	var config []byte
	var roots, audits int
	if err := f.Pool.QueryRow(t.Context(), `SELECT initial_config,(SELECT count(*) FROM computer_disk_version_roots WHERE version_id=$1),(SELECT count(*) FROM computer_disk_versions WHERE id=$1 AND publication_request_fingerprint IS NOT NULL) FROM computers WHERE id=$2`, versionID, pgvalue.UUID(first.result.ComputerID)).Scan(&config, &roots, &audits); err != nil {
		t.Fatal(err)
	}
	var got oci.RuntimeConfig
	if err := json.Unmarshal(config, &got); err != nil || got.User != "root" || roots != 1 || audits != 1 {
		t.Fatalf("incomplete publication: %s %d %d %v", config, roots, audits, err)
	}
	// The fixture supplies physical exclusion evidence; this test exercises
	// retention/replay after that boundary, not the Worker's cleanup mechanism.
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE computer_instances SET desired_state='closed',desired_version=desired_version+1,observed_state='failed',terminal_at=clock_timestamp(),terminal_reason_code='fixture',admission_state='closed',mount_state='lost',reclaimed_at=clock_timestamp(),reclaim_evidence='{"proof":"fixture"}' WHERE id=$1`, f.runtime)
	dbtest.MustExec(t, t.Context(), f.Pool, `DELETE FROM computer_disk_version_roots WHERE version_id=$1`, versionID)
	q := db.New(f.Pool)
	if n, err := q.ReleaseReclaimedComputerObjects(t.Context(), 10); err != nil || n != 1 {
		t.Fatalf("release reclaimed pins: %d %v", n, err)
	}
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE computers SET initial_config='{}',head_disk_version_id=NULL,status='deleted',desired_state='deleted',deleted_at=clock_timestamp(),sandbox_declared_id=NULL WHERE id=$1`, pgvalue.UUID(first.result.ComputerID))
	replay, err := f.publisher.PublishInitialVersion(t.Context(), f.principal, f.ref, input)
	if err != nil || replay != first.result {
		t.Fatalf("historical replay: %+v %v", replay, err)
	}
	changed := input
	changed.Config.User = "1000"
	if _, err = f.publisher.PublishInitialVersion(t.Context(), f.principal, f.ref, changed); err == nil {
		t.Fatal("changed config accepted")
	}
	wrong := f.principal
	wrong.HostID = uuid.NewV7()
	if _, err = f.publisher.PublishInitialVersion(t.Context(), wrong, f.ref, input); err == nil {
		t.Fatal("other Worker read publication")
	}
	stale := f.ref
	stale.DesiredVersion++
	if _, err = f.publisher.PublishInitialVersion(t.Context(), f.principal, stale, input); err == nil {
		t.Fatal("other fence read publication")
	}
	if err = f.Pool.QueryRow(t.Context(), `SELECT count(*) FROM computer_disk_version_roots WHERE version_id=$1`, versionID).Scan(&roots); err != nil || roots != 0 {
		t.Fatal("historical replay restored payload", err)
	}
}

func TestInitialGenerationPublicationRejectsInvalidCandidate(t *testing.T) {
	for _, kind := range []string{"malformed", "page", "capacity", "claim", "pin", "closed", "atomic failure", "source pin failure"} {
		t.Run(kind, func(t *testing.T) {
			f, input := newGenerationFixture(t)
			principal := f.principal
			switch kind {
			case "malformed":
				input.Root.FormatVersion = 2
			case "page":
				input.Root.Page.Digest = dbtest.Digest("wrong page")
			case "capacity":
				input.Root.LogicalBytes += 4096
			case "claim":
				principal.HostClaimVersion++
			case "pin":
				dbtest.MustExec(t, t.Context(), f.Pool, `DELETE FROM computer_object_pins WHERE computer_instance_id=$1`, f.runtime)
			case "closed":
				dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE computer_instances SET desired_state='closed',desired_version=desired_version+1 WHERE id=$1`, f.runtime)
			case "source pin failure":
				dbtest.MustExec(t, t.Context(), f.Pool, `CREATE FUNCTION reject_source_pin() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.source_disk_version_id IS NOT NULL THEN RAISE EXCEPTION 'injected source pin failure'; END IF; RETURN NEW; END $$; CREATE TRIGGER reject_source_pin BEFORE UPDATE ON computer_instances FOR EACH ROW EXECUTE FUNCTION reject_source_pin()`)
			case "atomic failure":
				dbtest.MustExec(t, t.Context(), f.Pool, `CREATE FUNCTION reject_publication() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'injected publication failure'; END $$; CREATE TRIGGER reject_publication BEFORE INSERT ON computer_disk_version_roots FOR EACH ROW EXECUTE FUNCTION reject_publication()`)
			}
			_, err := f.publisher.PublishInitialVersion(t.Context(), principal, f.ref, input)
			if err == nil {
				t.Fatal("invalid publication accepted")
			}
			// Deterministic rejections are object conflicts or changed
			// authority; an unexpected database failure keeps its cause.
			var objectConflict ObjectConflictError
			var inputErr InputError
			switch kind {
			case "malformed":
				if !errors.As(err, &inputErr) {
					t.Fatalf("malformed root classified as %v", err)
				}
			case "claim":
				if !errors.Is(err, workergroup.ErrStaleClaims) {
					t.Fatalf("stale claims classified as %v", err)
				}
			case "page", "capacity", "pin":
				if !errors.As(err, &objectConflict) {
					t.Fatalf("%s classified as %v", kind, err)
				}
			case "closed":
				if !errors.Is(err, ErrAuthorityChanged) {
					t.Fatalf("closed Instance classified as %v", err)
				}
			default:
				var pgErr *pgconn.PgError
				if !errors.As(err, &pgErr) || errors.As(err, &objectConflict) || errors.Is(err, ErrAuthorityChanged) {
					t.Fatalf("%s classified as %v", kind, err)
				}
			}
			var unchanged bool
			if err := f.Pool.QueryRow(t.Context(), `SELECT runtime.source_disk_version_id IS NULL AND v.status='initializing' AND v.publication_request_fingerprint IS NULL AND c.initial_config IS NULL AND NOT EXISTS(SELECT 1 FROM computer_disk_version_roots r WHERE r.version_id=v.id) FROM computer_instances runtime JOIN computer_disk_versions v ON v.computer_id=runtime.computer_id AND v.status='initializing' JOIN computers c ON c.id=v.computer_id WHERE runtime.id=$1`, f.runtime).Scan(&unchanged); err != nil || !unchanged {
				t.Fatalf("partial publication survived: %v %v", unchanged, err)
			}
		})
	}
}

func TestInitialGenerationPublicationDeadlineAfterLockWait(t *testing.T) {
	f, input := newGenerationFixture(t)
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE computer_instances SET preparation_expires_at=clock_timestamp()+interval '1 second' WHERE id=$1`, f.runtime)
	tx, err := f.Pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.Background())
	var pid int
	if err = tx.QueryRow(t.Context(), `SELECT pg_backend_pid() FROM computer_objects WHERE digest=$1 FOR UPDATE`, input.Root.Pack.Digest).Scan(&pid); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := f.publisher.PublishInitialVersion(t.Context(), f.principal, f.ref, input)
		done <- err
	}()
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	tick := time.NewTicker(5 * time.Millisecond)
	defer tick.Stop()
	for {
		var blocked, expired bool
		if err = f.Pool.QueryRow(t.Context(), `SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE datname=current_database() AND $1=ANY(pg_blocking_pids(pid))),preparation_expires_at<clock_timestamp() FROM computer_instances WHERE id=$2`, pid, f.runtime).Scan(&blocked, &expired); err != nil {
			t.Fatal(err)
		}
		if blocked && expired {
			break
		}
		select {
		case <-deadline.C:
			t.Fatal("publication did not block before expiration")
		case <-tick.C:
		}
	}
	if err = tx.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err = <-done; err == nil {
		t.Fatal("expired publisher committed")
	}
	var count int
	if err = f.Pool.QueryRow(t.Context(), `SELECT count(*) FROM computer_disk_versions WHERE publisher_computer_instance_id=$1`, f.runtime).Scan(&count); err != nil || count != 0 {
		t.Fatal("expired publication retained audit", err)
	}
}

func TestInitialGenerationPublicationRevocationWinsLockWait(t *testing.T) {
	f, input := newGenerationFixture(t)
	tx, err := f.Pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.Background())
	var pid int
	if err = tx.QueryRow(t.Context(), `SELECT pg_backend_pid() FROM computer_instances WHERE id=$1 FOR UPDATE`, f.runtime).Scan(&pid); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := f.publisher.PublishInitialVersion(t.Context(), f.principal, f.ref, input)
		done <- err
	}()
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
			t.Fatal("publication did not wait on Instance authority")
		case <-tick.C:
		}
	}
	dbtest.MustExec(t, t.Context(), tx, `UPDATE computer_instances SET desired_state='closed',desired_version=desired_version+1 WHERE id=$1`, f.runtime)
	if err = tx.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err = <-done; err == nil {
		t.Fatal("revoked publisher committed after lock wait")
	}
	var count int
	if err = f.Pool.QueryRow(t.Context(), `SELECT count(*) FROM computer_disk_versions WHERE publisher_computer_instance_id=$1`, f.runtime).Scan(&count); err != nil || count != 0 {
		t.Fatal("revoked publication retained audit", err)
	}
}

func TestComputerPublicationPinsRejectOtherOwnerAndReleaseOneAtATime(t *testing.T) {
	f, input := newGenerationFixture(t)
	other := bytes.Repeat([]byte{0xab}, 32)
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE computer_object_pins SET publication_key=$2 WHERE computer_instance_id=$1`, f.runtime, other)
	if _, err := f.publisher.PublishInitialVersion(t.Context(), f.principal, f.ref, input); err == nil {
		t.Fatal("other publication pin authorized initial root")
	}
	dbtest.MustExec(t, t.Context(), f.Pool, `INSERT INTO computer_object_pins(computer_instance_id,publication_key,digest,environment_id,computer_id,instance_desired_version)
 SELECT computer_instance_id,$2,digest,environment_id,computer_id,instance_desired_version FROM computer_object_pins WHERE computer_instance_id=$1`, f.runtime, []byte(initialPublicationKey(f.ref.InstanceID)))
	q := db.New(f.Pool)
	if n, err := q.ReleaseReclaimedComputerObjects(t.Context(), 1); err != nil || n != 0 {
		t.Fatalf("live Runtime released pins: %d %v", n, err)
	}
	// The test supplies physical exclusion evidence; it does not prove host cleanup.
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE computer_instances SET desired_state='closed',desired_version=desired_version+1,observed_state='failed',terminal_at=clock_timestamp(),terminal_reason_code='fixture',admission_state='closed',mount_state='lost',reclaimed_at=clock_timestamp(),reclaim_evidence='{"proof":"fixture"}' WHERE id=$1`, f.runtime)
	for want := 1; want >= 0; want-- {
		if n, err := q.ReleaseReclaimedComputerObjects(t.Context(), 1); err != nil || n != 1 {
			t.Fatalf("bounded release: %d %v", n, err)
		}
		var retained int
		if err := f.Pool.QueryRow(t.Context(), `SELECT count(*) FROM computer_object_pins WHERE computer_instance_id=$1`, f.runtime).Scan(&retained); err != nil || retained != want {
			t.Fatalf("released another publication: %d want %d (%v)", retained, want, err)
		}
	}
}
