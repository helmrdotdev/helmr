package session

import (
	"context"
	"errors"
	"testing"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/computer"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"github.com/helmrdotdev/helmr/internal/run/runtest"
	"github.com/jackc/pgx/v5"
)

func TestSessionOperationsReportLostComputerAuthority(t *testing.T) {
	f := runtest.New(t)
	work := f.AddRunLease(t, "assigned", time.Now())
	sessionID := f.ConvertToActor(t, t.Context(), work, `{"enabled":false}`)
	// A private head disk version withdraws the Computer's admission
	// authority while the Session remains.
	head := uuid.NewV7()
	dbtest.MustExec(t, t.Context(), f.Pool, `WITH head AS (
 INSERT INTO computer_disk_versions(id,environment_id,computer_id,parent_version_id,root_pack_digest,logical_bytes,status,writer_generation,source_computer_instance_id)
 SELECT $2,v.environment_id,v.computer_id,v.id,v.root_pack_digest,v.logical_bytes,'private',i.writer_generation,i.id FROM sessions s JOIN computers c ON c.id=s.computer_id JOIN computer_disk_versions v ON v.id=c.head_disk_version_id JOIN computer_instances i ON i.computer_id=c.id AND i.reclaimed_at IS NULL WHERE s.id=$1
 RETURNING id,environment_id,computer_id,parent_version_id
), root AS (
 INSERT INTO computer_disk_version_roots(environment_id,computer_id,version_id,locator) SELECT head.environment_id,head.computer_id,head.id,r.locator FROM head JOIN computer_disk_version_roots r ON r.version_id=head.parent_version_id
)
UPDATE computers SET head_disk_version_id=(SELECT id FROM head) WHERE id=(SELECT computer_id FROM head)`, sessionID, head)

	tx, err := f.Pool.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback(context.Background()) }()
	_, err = Close(t.Context(), tx, ControlRequest{Target: Target{EnvironmentID: f.EnvironmentID, SessionID: sessionID}})
	if !errors.Is(err, ErrComputerAuthority) || !errors.Is(err, computer.ErrNotFound) || errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("close without Computer authority = %v, want ErrComputerAuthority wrapping computer.ErrNotFound", err)
	}
	var operation *OperationError
	if errors.As(err, &operation) {
		t.Fatalf("close without Computer authority returned operation error %q", operation.Code)
	}
}
