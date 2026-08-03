package controlplane

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
)

func TestImageCacheRetirementMarksOnlyProvenAbsence(t *testing.T) {
	first := uuid.MustParse("019c10d5-a6f7-7af1-8f5f-bb97bcc0dc32")
	second := uuid.MustParse("019c10d5-a6f7-7af1-8f5f-bb97bcc0dc33")
	store := &imageCacheRetirementStoreFixture{rows: []db.ListDueEnvironmentImageCacheRetirementsRow{
		{ID: pgvalue.NewUUIDv7(), TargetID: pgvalue.UUID(first)},
		{ID: pgvalue.NewUUIDv7(), TargetID: pgvalue.UUID(second)},
	}}
	retirer := &imageCacheRetirerFixture{fail: map[uuid.UUID]error{first: errors.New("tag mismatch")}}
	workflow := imageCacheRetirementWorkflow{store: store, retirer: retirer}
	if err := workflow.reconcile(t.Context(), imageCacheRetirementLimit); err == nil {
		t.Fatal("retirement mismatch was hidden")
	}
	if len(retirer.ids) != 2 || len(store.marked) != 1 || store.marked[0].EnvironmentID != pgvalue.UUID(second) {
		t.Fatalf("retired = %v, marked = %+v", retirer.ids, store.marked)
	}
}

func TestImageCacheRetirementRequiresBothAuthorities(t *testing.T) {
	if err := (imageCacheRetirementWorkflow{}).reconcile(t.Context(), 1); err == nil {
		t.Fatal("missing retirement authorities accepted")
	}
	if err := (imageCacheRetirementWorkflow{store: &imageCacheRetirementStoreFixture{}}).reconcile(t.Context(), 1); err == nil {
		t.Fatal("missing repository retirer accepted")
	}
}

type imageCacheRetirementStoreFixture struct {
	rows   []db.ListDueEnvironmentImageCacheRetirementsRow
	marked []db.MarkEnvironmentImageCacheRetiredParams
}

func (fixture *imageCacheRetirementStoreFixture) ListDueEnvironmentImageCacheRetirements(context.Context, int32) ([]db.ListDueEnvironmentImageCacheRetirementsRow, error) {
	return fixture.rows, nil
}

func (fixture *imageCacheRetirementStoreFixture) MarkEnvironmentImageCacheRetired(_ context.Context, params db.MarkEnvironmentImageCacheRetiredParams) (int64, error) {
	fixture.marked = append(fixture.marked, params)
	return 1, nil
}

type imageCacheRetirerFixture struct {
	ids  []uuid.UUID
	fail map[uuid.UUID]error
}

func (fixture *imageCacheRetirerFixture) Retire(_ context.Context, environmentID uuid.UUID) error {
	fixture.ids = append(fixture.ids, environmentID)
	return fixture.fail[environmentID]
}
