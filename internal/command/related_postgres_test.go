package command

import (
	"errors"
	"github.com/helmrdotdev/helmr/internal/access"
	"github.com/helmrdotdev/helmr/internal/auth"
	"github.com/helmrdotdev/helmr/internal/db/dbtest"
	"testing"
	"uuid"
)

func TestRelatedCommandsRequireCurrentReadAndSeparateStopPermission(t *testing.T) {
	f := commandFixture(t)
	c := f.create(t, "related")
	scope := auth.Scope{OrgID: f.OrgID, ProjectID: f.ProjectID.String(), EnvironmentID: f.Environment.String()}
	list, err := ListRelated(t.Context(), f.Pool, f.human, scope, f.User, uuid.Nil())
	if err != nil || len(list.Commands) != 1 || list.Commands[0].ID != uuid.UUID(c.ID.Bytes) || !list.CanCancel {
		t.Fatalf("related command: %+v %v", list, err)
	}
	list, err = ListRelated(t.Context(), f.Pool, f.human, scope, uuid.NewV7(), uuid.Nil())
	if err != nil || len(list.Commands) != 0 || list.Commands == nil {
		t.Fatalf("unrelated user: %+v %v", list, err)
	}
	f.SetPermissions(t, "computers.read")
	list, err = ListRelated(t.Context(), f.Pool, f.human, scope, f.User, uuid.Nil())
	if err != nil || len(list.Commands) != 1 || list.CanCancel {
		t.Fatalf("read borrowed stop authority: %+v %v", list, err)
	}
	dbtest.MustExec(t, t.Context(), f.Pool, `UPDATE org_members SET suspended_at=clock_timestamp() WHERE org_id=$1 AND user_id=$2`, f.OrgID, f.User)
	if _, err = ListRelated(t.Context(), f.Pool, f.human, scope, f.User, uuid.Nil()); !errors.Is(err, access.ErrDenied) {
		t.Fatal("suspended reader", err)
	}
}
