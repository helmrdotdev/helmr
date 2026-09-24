package controlplane

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/helmrdotdev/helmr/internal/cas"
	"github.com/helmrdotdev/helmr/internal/computer"
	"github.com/helmrdotdev/helmr/internal/computer/blockformat"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/oci"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
)

func generationPublicationFixture(t *testing.T) (initialPublicationFixture, computerKeyFence, initialComputerPublication) {
	t.Helper()
	f, b, fence := initialKeyFixture(t)
	key, err := b.initial(t.Context(), fence)
	if err != nil {
		t.Fatal(err)
	}
	defer clear(key.Key)
	store, err := cas.NewFile(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	writer := blockformat.Writer{Source: store, Sink: store, Scope: key.Scope, ActiveKey: key.ID, Keys: map[string][]byte{key.ID: key.Key}, PackLimit: blockformat.MinPackLimit}
	locator, err := writer.Empty(t.Context(), f.request.LogicalBytes, 64)
	if err != nil {
		t.Fatal(err)
	}
	inspected, err := blockformat.InspectPack(t.Context(), store, key.Scope, writer.Keys, locator.Pack)
	if err != nil {
		t.Fatal(err)
	}
	evidence := blockformat.ObjectInspection{Pack: &inspected}
	if err = recordInitialComputerObject(t.Context(), f.Pool, fence, evidence, nil); err != nil {
		t.Fatal(err)
	}
	root, err := computer.NewGenerationRoot(locator, f.request.LogicalBytes)
	if err != nil {
		t.Fatal(err)
	}
	body, err := store.Get(t.Context(), root.Pack.Digest)
	if err != nil {
		t.Fatal(err)
	}
	defer body.Close()
	uploaded, err := f.server.cas.Put(t.Context(), "application/octet-stream", body)
	if err != nil {
		t.Fatal(err)
	}
	if err = recordInitialComputerObject(t.Context(), f.Pool, fence, evidence, &uploaded); err != nil {
		t.Fatal(err)
	}
	return f, fence, initialComputerPublication{Root: root, Config: oci.RuntimeConfig{User: "root", WorkingDir: "/workspace"}}
}

func TestInitialGenerationPublicationReplay(t *testing.T) {
	f, fence, input := generationPublicationFixture(t)
	type outcome struct {
		result computerPublicationResult
		err    error
	}
	results := make(chan outcome, 2)
	for range 2 {
		go func() {
			v, err := f.server.publishInitialComputerGeneration(t.Context(), fence, input)
			results <- outcome{v, err}
		}()
	}
	first, second := <-results, <-results
	if first.err != nil || second.err != nil || first.result != second.result {
		t.Fatalf("concurrent publication: %+v %+v", first, second)
	}
	var retained bool
	if err := f.Pool.QueryRow(t.Context(), `SELECT computer_source_version_id=$2 AND retained_computer_source_version_id=$2 FROM runtime_instances WHERE id=$1`, f.runtime, first.result.VersionID).Scan(&retained); err != nil || !retained {
		t.Fatalf("published generation is not retained by runtime: %v %v", retained, err)
	}
	var config []byte
	var roots, audits int
	if err := f.Pool.QueryRow(t.Context(), `SELECT initial_config,(SELECT count(*) FROM computer_version_roots WHERE version_id=$1),(SELECT count(*) FROM computer_versions WHERE id=$1 AND publication_request_fingerprint IS NOT NULL) FROM computers WHERE id=$2`, first.result.VersionID, first.result.ComputerID).Scan(&config, &roots, &audits); err != nil {
		t.Fatal(err)
	}
	var got oci.RuntimeConfig
	if err := json.Unmarshal(config, &got); err != nil || got.User != "root" || roots != 1 || audits != 1 {
		t.Fatalf("incomplete publication: %s %d %d %v", config, roots, audits, err)
	}
	// The fixture supplies physical exclusion evidence; this test exercises
	// retention/replay after that boundary, not the Worker's cleanup mechanism.
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE runtime_instances SET desired_state='closed',desired_version=desired_version+1,observed_state='failed',reserved_run_id=NULL,reserved_attempt_number=NULL,reserved_workspace_version_id=NULL,terminal_at=clock_timestamp(),terminal_reason_code='fixture',reclaimed_at=clock_timestamp(),reclaim_evidence='{"proof":"fixture"}' WHERE id=$1`, f.runtime)
	dbtest.MustExec(t, t.Context(), f.Pool, `DELETE FROM computer_version_roots WHERE version_id=$1`, first.result.VersionID)
	if n, err := f.server.db.ReleaseReclaimedComputerObjects(t.Context(), 10); err != nil || n != 1 {
		t.Fatalf("release reclaimed pins: %d %v", n, err)
	}
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE computers SET initial_config='{}',head_version_id=NULL,status='deleted',desired_state='deleted',deleted_at=clock_timestamp(),owner_run_id=NULL,owner_session_id=NULL,sandbox_declared_id=NULL WHERE id=$1`, first.result.ComputerID)
	replay, err := f.server.publishInitialComputerGeneration(t.Context(), fence, input)
	if err != nil || replay != first.result {
		t.Fatalf("historical replay: %+v %v", replay, err)
	}
	changed := input
	changed.Config.User = "1000"
	if _, err = f.server.publishInitialComputerGeneration(t.Context(), fence, changed); err == nil {
		t.Fatal("changed config accepted")
	}
	wrong := fence
	wrong.WorkerID = pgvalue.NewUUIDv7()
	if _, err = f.server.publishInitialComputerGeneration(t.Context(), wrong, input); err == nil {
		t.Fatal("other Worker read publication")
	}
	wrong = fence
	wrong.DesiredVersion++
	if _, err = f.server.publishInitialComputerGeneration(t.Context(), wrong, input); err == nil {
		t.Fatal("other fence read publication")
	}
	if err = f.Pool.QueryRow(t.Context(), `SELECT count(*) FROM computer_version_roots WHERE version_id=$1`, first.result.VersionID).Scan(&roots); err != nil || roots != 0 {
		t.Fatal("historical replay restored payload", err)
	}
}

func TestInitialGenerationPublicationRejectsInvalidCandidate(t *testing.T) {
	for _, kind := range []string{"page", "capacity", "claim", "pin", "closed", "atomic failure", "source pin failure"} {
		t.Run(kind, func(t *testing.T) {
			f, fence, input := generationPublicationFixture(t)
			switch kind {
			case "page":
				input.Root.Page.Digest = dbtest.Digest("wrong page")
			case "capacity":
				input.Root.LogicalBytes += 4096
			case "claim":
				fence.ClaimVersion++
			case "pin":
				dbtest.MustExec(t, t.Context(), f.Pool, `DELETE FROM runtime_computer_object_pins WHERE runtime_instance_id=$1`, f.runtime)
			case "closed":
				dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE runtime_instances SET desired_state='closed',desired_version=desired_version+1 WHERE id=$1`, f.runtime)
			case "source pin failure":
				dbtest.MustExec(t, t.Context(), f.Pool, `CREATE FUNCTION reject_source_pin() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN IF NEW.computer_source_version_id IS NOT NULL THEN RAISE EXCEPTION 'injected source pin failure'; END IF; RETURN NEW; END $$; CREATE TRIGGER reject_source_pin BEFORE UPDATE ON runtime_instances FOR EACH ROW EXECUTE FUNCTION reject_source_pin()`)
			case "atomic failure":
				dbtest.MustExec(t, t.Context(), f.Pool, `CREATE FUNCTION reject_publication() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'injected publication failure'; END $$; CREATE TRIGGER reject_publication BEFORE INSERT ON computer_version_roots FOR EACH ROW EXECUTE FUNCTION reject_publication()`)
			}
			if _, err := f.server.publishInitialComputerGeneration(t.Context(), fence, input); err == nil {
				t.Fatal("invalid publication accepted")
			}
			var unchanged bool
			if err := f.Pool.QueryRow(t.Context(), `SELECT runtime.computer_source_version_id IS NULL AND v.status='initializing' AND v.publication_request_fingerprint IS NULL AND c.initial_config IS NULL AND NOT EXISTS(SELECT 1 FROM computer_version_roots r WHERE r.version_id=v.id) FROM runtime_instances runtime JOIN computer_versions v ON v.id=runtime.reserved_workspace_version_id JOIN computers c ON c.id=v.workspace_id WHERE runtime.id=$1`, f.runtime).Scan(&unchanged); err != nil || !unchanged {
				t.Fatalf("partial publication survived: %v %v", unchanged, err)
			}
		})
	}
}

func TestInitialGenerationPublicationDeadlineAfterLockWait(t *testing.T) {
	f, fence, input := generationPublicationFixture(t)
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE runtime_instances SET preparation_expires_at=clock_timestamp()+interval '1 second' WHERE id=$1`, f.runtime)
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
	go func() { _, err := f.server.publishInitialComputerGeneration(t.Context(), fence, input); done <- err }()
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	tick := time.NewTicker(5 * time.Millisecond)
	defer tick.Stop()
	for {
		var blocked, expired bool
		if err = f.Pool.QueryRow(t.Context(), `SELECT EXISTS(SELECT 1 FROM pg_stat_activity WHERE datname=current_database() AND $1=ANY(pg_blocking_pids(pid))),preparation_expires_at<clock_timestamp() FROM runtime_instances WHERE id=$2`, pid, f.runtime).Scan(&blocked, &expired); err != nil {
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
	if err = f.Pool.QueryRow(t.Context(), `SELECT count(*) FROM computer_versions WHERE publisher_runtime_instance_id=$1`, f.runtime).Scan(&count); err != nil || count != 0 {
		t.Fatal("expired publication retained audit", err)
	}
}

func TestInitialGenerationPublicationRevocationWinsLockWait(t *testing.T) {
	f, fence, input := generationPublicationFixture(t)
	tx, err := f.Pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(context.Background())
	var pid int
	if err = tx.QueryRow(t.Context(), `SELECT pg_backend_pid() FROM runs WHERE id=$1 FOR UPDATE`, f.run.RunID).Scan(&pid); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { _, err := f.server.publishInitialComputerGeneration(t.Context(), fence, input); done <- err }()
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
			t.Fatal("publication did not wait on Run authority")
		case <-tick.C:
		}
	}
	dbtest.MustExec(t, t.Context(), tx, `UPDATE runtime_instances SET desired_state='closed',desired_version=desired_version+1 WHERE id=$1`, f.runtime)
	if err = tx.Commit(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err = <-done; err == nil {
		t.Fatal("revoked publisher committed after lock wait")
	}
	var count int
	if err = f.Pool.QueryRow(t.Context(), `SELECT count(*) FROM computer_versions WHERE publisher_runtime_instance_id=$1`, f.runtime).Scan(&count); err != nil || count != 0 {
		t.Fatal("revoked publication retained audit", err)
	}
}
