package retirement

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
)

func TestWorkerMarksOnlyProvenAbsence(t *testing.T) {
	first := uuid.MustParse("019c10d5-a6f7-7af1-8f5f-bb97bcc0dc32")
	second := uuid.MustParse("019c10d5-a6f7-7af1-8f5f-bb97bcc0dc33")
	store := &storeFixture{rows: []db.ListDueEnvironmentImageCacheRetirementsRow{
		{ID: pgvalue.NewUUIDv7(), TargetID: pgvalue.UUID(first)},
		{ID: pgvalue.NewUUIDv7(), TargetID: pgvalue.UUID(second)},
	}}
	retirer := &retirerFixture{fail: map[uuid.UUID]error{first: errors.New("tag mismatch")}}
	worker, err := NewWorker(nil, store, retirer)
	if err != nil {
		t.Fatal(err)
	}
	if err := worker.reconcile(t.Context(), batchLimit); err == nil {
		t.Fatal("retirement mismatch was hidden")
	}
	if len(retirer.ids) != 2 || len(store.marked) != 1 || store.marked[0].EnvironmentID != pgvalue.UUID(second) {
		t.Fatalf("retired = %v, marked = %+v", retirer.ids, store.marked)
	}
}

func TestWorkerRequiresBothAuthorities(t *testing.T) {
	if _, err := NewWorker(nil, nil, nil); err == nil {
		t.Fatal("missing retirement authorities accepted")
	}
	if _, err := NewWorker(nil, &storeFixture{}, nil); err == nil {
		t.Fatal("missing repository retirer accepted")
	}
}

type storeFixture struct {
	rows   []db.ListDueEnvironmentImageCacheRetirementsRow
	marked []db.MarkEnvironmentImageCacheRetiredParams
}

func (fixture *storeFixture) ListDueEnvironmentImageCacheRetirements(context.Context, int32) ([]db.ListDueEnvironmentImageCacheRetirementsRow, error) {
	return fixture.rows, nil
}

func (fixture *storeFixture) MarkEnvironmentImageCacheRetired(_ context.Context, params db.MarkEnvironmentImageCacheRetiredParams) (int64, error) {
	fixture.marked = append(fixture.marked, params)
	return 1, nil
}

type retirerFixture struct {
	ids  []uuid.UUID
	fail map[uuid.UUID]error
}

func (fixture *retirerFixture) Retire(_ context.Context, environmentID uuid.UUID) error {
	fixture.ids = append(fixture.ids, environmentID)
	return fixture.fail[environmentID]
}
