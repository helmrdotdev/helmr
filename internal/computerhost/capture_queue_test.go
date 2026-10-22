package computerhost

import (
	"sync"
	"sync/atomic"
	"testing"
)

func TestCaptureQueueFairnessAndCancellation(t *testing.T) {
	var queue captureQueue
	first, canceled, second, third := queue.request(), queue.request(), queue.request(), queue.request()
	if !first.take() {
		t.Fatal("first source not admitted")
	}
	canceled.release()
	if second.take() || third.take() {
		t.Fatal("concurrent capture admitted")
	}
	first.release()
	first.release()
	if third.take() || !second.take() {
		t.Fatal("FIFO position was lost")
	}
	second.release()
	if !third.take() {
		t.Fatal("canceled waiter blocked next source")
	}
	third.release()
	if queue.active != nil || len(queue.waiting) != 0 {
		t.Fatal("capture permit leaked")
	}
}

func TestCaptureQueueConcurrentClaimsCannotOverlap(t *testing.T) {
	var queue captureQueue
	var ready []*capturePermit
	for range 100 {
		ready = append(ready, queue.request())
	}
	var jobs sync.WaitGroup
	var admitted atomic.Int64
	for _, permit := range ready {
		jobs.Go(func() {
			if permit.take() {
				admitted.Add(1)
			}
		})
	}
	jobs.Wait()
	if admitted.Load() != 1 {
		t.Fatalf("admitted %d simultaneous captures", admitted.Load())
	}
	for _, permit := range ready {
		permit.release()
	}
	if queue.active != nil || len(queue.waiting) != 0 {
		t.Fatal("capture permit leaked")
	}
}
