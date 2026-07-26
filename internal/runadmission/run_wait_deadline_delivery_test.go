package runadmission

import (
	"context"
	"errors"
	"testing"
)

func TestRunWaitDeadlineDeliveryReconcilesEveryDeadlineKind(t *testing.T) {
	var order []string
	reconcile := func(kind string) RunWaitDeadlineReconcile {
		return func(_ context.Context, limit int32) (int, error) {
			if limit != tokenReconcileBatchLimit {
				t.Fatalf("%s limit = %d", kind, limit)
			}
			order = append(order, kind)
			return 1, nil
		}
	}
	worker, err := NewRunWaitDeadlineDeliveryWorker(
		nil,
		reconcile("timer"),
		reconcile("token"),
		reconcile("actor_input"),
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := worker.tick(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(order) != 3 || order[0] != "timer" ||
		order[1] != "token" || order[2] != "actor_input" {
		t.Fatalf("deadline reconciliation order = %v", order)
	}
}

func TestRunWaitDeadlineDeliveryStopsAfterFailure(t *testing.T) {
	calledAfterFailure := false
	worker, err := NewRunWaitDeadlineDeliveryWorker(
		nil,
		func(context.Context, int32) (int, error) {
			return 0, errors.New("database unavailable")
		},
		func(context.Context, int32) (int, error) {
			calledAfterFailure = true
			return 0, nil
		},
		func(context.Context, int32) (int, error) {
			calledAfterFailure = true
			return 0, nil
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := worker.tick(context.Background()); err == nil {
		t.Fatal("deadline reconciliation failure was ignored")
	}
	if calledAfterFailure {
		t.Fatal("later deadline reconcilers ran after a failure")
	}
}
