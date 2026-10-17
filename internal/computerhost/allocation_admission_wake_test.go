package computerhost

import (
	"context"
	"testing"
	"testing/synctest"
	"time"

	"github.com/helmrdotdev/helmr/internal/reservation"
)

func TestAllocationAdmissionWakesOnLocalCapacityRelease(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		capacity := reservation.Vector{CPUMillis: 1000, MemoryBytes: 4096, VMSlots: 1}
		ledger, err := reservation.New(capacity)
		if err != nil {
			t.Fatal(err)
		}
		old := reservation.Key{Kind: "instance", Epoch: 1, ID: "old"}
		next := reservation.Key{Kind: "instance", Epoch: 1, ID: "next"}
		if _, err = ledger.Reserve(old, capacity); err != nil {
			t.Fatal(err)
		}
		started := time.Now()
		done := make(chan error, 1)
		go func() {
			done <- waitAllocationCapacity(t.Context(), func(context.Context) error { return nil }, ledger, next, capacity, &allocationWake{})
		}()
		synctest.Wait()
		if err = ledger.Release(old); err != nil {
			t.Fatal(err)
		}
		synctest.Wait()
		select {
		case err = <-done:
			if err != nil {
				t.Fatal(err)
			}
		default:
			t.Fatal("capacity release waited for catch-up timer")
		}
		if !time.Now().Equal(started) {
			t.Fatal("admission used a timer instead of capacity notification")
		}
	})
}
