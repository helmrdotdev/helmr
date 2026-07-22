package runadmission

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
	"github.com/jackc/pgx/v5/pgtype"
)

func TestDeliveryWorkerEnqueuesAndDeliversRunAdmission(t *testing.T) {
	environmentID := uuid.Must(uuid.NewV7())
	runID := uuid.Must(uuid.NewV7())
	orgID := uuid.Must(uuid.NewV7())
	message := deliveryMessage(environmentID, runID, []byte(`{"environmentId":"`+environmentID.String()+`","runId":"`+runID.String()+`"}`))
	store := &deliveryStore{
		messages: []db.OutboxMessage{message},
		run: db.Run{
			ID:    pgvalue.UUID(runID),
			OrgID: pgvalue.UUID(orgID),
		},
	}
	var enqueuedOrg, enqueuedRun pgtype.UUID
	worker, err := NewDeliveryWorker(nil, store, func(_ context.Context, orgID, runID pgtype.UUID) error {
		enqueuedOrg, enqueuedRun = orgID, runID
		return nil
	}, unusedResumeEnqueue(t))
	if err != nil {
		t.Fatal(err)
	}
	worker.now = func() time.Time { return time.Unix(100, 0).UTC() }

	if err := worker.tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if store.claim.Lane != "control" ||
		len(store.claim.Topics) != 2 ||
		store.claim.Topics[0] != "run.admit" ||
		store.claim.Topics[1] != "run.resume" {
		t.Fatalf("claim = %+v", store.claim)
	}
	if pgvalue.UUIDString(enqueuedOrg) != orgID.String() ||
		pgvalue.UUIDString(enqueuedRun) != runID.String() {
		t.Fatalf("enqueued org=%s run=%s", pgvalue.UUIDString(enqueuedOrg), pgvalue.UUIDString(enqueuedRun))
	}
	if store.delivered != message.Attempts {
		t.Fatalf("delivered claim attempt = %d", store.delivered)
	}
}

func TestDeliveryWorkerRetriesEnqueueFailure(t *testing.T) {
	environmentID := uuid.Must(uuid.NewV7())
	runID := uuid.Must(uuid.NewV7())
	message := deliveryMessage(environmentID, runID, []byte(`{"environmentId":"`+environmentID.String()+`","runId":"`+runID.String()+`"}`))
	message.Attempts = 3
	store := &deliveryStore{
		messages: []db.OutboxMessage{message},
		run:      db.Run{ID: pgvalue.UUID(runID), OrgID: pgvalue.UUID(uuid.Must(uuid.NewV7()))},
	}
	worker, err := NewDeliveryWorker(nil, store, func(context.Context, pgtype.UUID, pgtype.UUID) error {
		return errors.New("queue unavailable")
	}, unusedResumeEnqueue(t))
	if err != nil {
		t.Fatal(err)
	}
	now := time.Unix(100, 0).UTC()
	worker.now = func() time.Time { return now }

	if err := worker.tick(context.Background()); err == nil {
		t.Fatal("expected delivery failure")
	}
	if got, want := store.retryAt, now.Add(4*time.Second); !got.Equal(want) {
		t.Fatalf("retry at = %s, want %s", got, want)
	}
	if store.deadLettered {
		t.Fatal("transient enqueue failure was dead-lettered")
	}
}

func TestDeliveryWorkerDeadLettersInvalidPayload(t *testing.T) {
	message := deliveryMessage(uuid.Must(uuid.NewV7()), uuid.Must(uuid.NewV7()), []byte(`{"runId":"invalid"}`))
	store := &deliveryStore{messages: []db.OutboxMessage{message}}
	worker, err := NewDeliveryWorker(nil, store, func(context.Context, pgtype.UUID, pgtype.UUID) error {
		t.Fatal("invalid payload reached enqueuer")
		return nil
	}, unusedResumeEnqueue(t))
	if err != nil {
		t.Fatal(err)
	}

	if err := worker.tick(context.Background()); err == nil {
		t.Fatal("expected invalid payload failure")
	}
	if !store.deadLettered {
		t.Fatal("invalid payload was not dead-lettered")
	}
}

func TestDeliveryWorkerEnqueuesResumeWithExactGeneration(t *testing.T) {
	environmentID := uuid.Must(uuid.NewV7())
	runID := uuid.Must(uuid.NewV7())
	runWaitID := uuid.Must(uuid.NewV7())
	message := deliveryMessage(environmentID, runID, []byte(`{
		"environmentId":"`+environmentID.String()+`",
		"runId":"`+runID.String()+`",
		"runWaitId":"`+runWaitID.String()+`",
		"resumeRequestVersion":7
	}`))
	message.Topic = "run.resume"
	store := &deliveryStore{messages: []db.OutboxMessage{message}}
	var gotEnvironment, gotRun, gotWait pgtype.UUID
	var gotVersion int64
	worker, err := NewDeliveryWorker(
		nil,
		store,
		func(context.Context, pgtype.UUID, pgtype.UUID) error {
			t.Fatal("resume reached admission enqueuer")
			return nil
		},
		func(_ context.Context, environmentID, runID, runWaitID pgtype.UUID, version int64) error {
			gotEnvironment, gotRun, gotWait, gotVersion = environmentID, runID, runWaitID, version
			return nil
		},
	)
	if err != nil {
		t.Fatal(err)
	}

	if err := worker.tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if gotEnvironment != pgvalue.UUID(environmentID) || gotRun != pgvalue.UUID(runID) ||
		gotWait != pgvalue.UUID(runWaitID) || gotVersion != 7 {
		t.Fatalf("resume fence = environment %v run %v wait %v version %d", gotEnvironment, gotRun, gotWait, gotVersion)
	}
	if store.delivered != message.Attempts {
		t.Fatalf("delivered claim attempt = %d", store.delivered)
	}
}

func TestDeliveryWorkerRetriesStaleResumeHint(t *testing.T) {
	environmentID := uuid.Must(uuid.NewV7())
	runID := uuid.Must(uuid.NewV7())
	runWaitID := uuid.Must(uuid.NewV7())
	message := deliveryMessage(environmentID, runID, []byte(`{
		"environmentId":"`+environmentID.String()+`",
		"runId":"`+runID.String()+`",
		"runWaitId":"`+runWaitID.String()+`",
		"resumeRequestVersion":1
	}`))
	message.Topic = "run.resume"
	store := &deliveryStore{messages: []db.OutboxMessage{message}}
	worker, err := NewDeliveryWorker(
		nil,
		store,
		func(context.Context, pgtype.UUID, pgtype.UUID) error { return nil },
		func(context.Context, pgtype.UUID, pgtype.UUID, pgtype.UUID, int64) error {
			return errors.New("resume authority is not ready")
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := worker.tick(context.Background()); err == nil {
		t.Fatal("expected resume retry")
	}
	if store.retryAt.IsZero() || store.delivered != 0 || store.deadLettered {
		t.Fatalf("retry=%s delivered=%d dead=%t", store.retryAt, store.delivered, store.deadLettered)
	}
}

func unusedResumeEnqueue(t *testing.T) RunResumeEnqueue {
	t.Helper()
	return func(context.Context, pgtype.UUID, pgtype.UUID, pgtype.UUID, int64) error {
		t.Fatal("unexpected resume enqueue")
		return nil
	}
}

type deliveryStore struct {
	messages     []db.OutboxMessage
	claim        db.ClaimOutboxMessagesParams
	run          db.Run
	getRunErr    error
	delivered    int32
	retryAt      time.Time
	deadLettered bool
}

func (s *deliveryStore) ClaimOutboxMessages(_ context.Context, claim db.ClaimOutboxMessagesParams) ([]db.OutboxMessage, error) {
	s.claim = claim
	return s.messages, nil
}

func (s *deliveryStore) GetRun(context.Context, db.GetRunParams) (db.Run, error) {
	return s.run, s.getRunErr
}

func (s *deliveryStore) DeliverOutboxMessage(_ context.Context, value db.DeliverOutboxMessageParams) (db.OutboxMessage, error) {
	s.delivered = value.ClaimAttempt
	return db.OutboxMessage{}, nil
}

func (s *deliveryStore) RetryOutboxMessage(_ context.Context, value db.RetryOutboxMessageParams) (db.OutboxMessage, error) {
	s.retryAt = value.AvailableAt.Time
	return db.OutboxMessage{}, nil
}

func (s *deliveryStore) DeadLetterOutboxMessage(context.Context, db.DeadLetterOutboxMessageParams) (db.OutboxMessage, error) {
	s.deadLettered = true
	return db.OutboxMessage{}, nil
}

func deliveryMessage(environmentID, runID uuid.UUID, payload []byte) db.OutboxMessage {
	return db.OutboxMessage{
		ID:        pgvalue.UUID(uuid.Must(uuid.NewV7())),
		Lane:      "control",
		Topic:     "run.admit",
		Payload:   payload,
		State:     "claimed",
		Attempts:  1,
		ClaimedBy: pgtype.Text{String: "worker", Valid: true},
	}
}
