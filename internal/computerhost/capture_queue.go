package computerhost

import "sync"

// captureQueue serializes physical capture before dispatch is sealed.
// Requests retain FIFO position while their save owner continues servicing work.
// Immutable publication proceeds outside the FIFO; disk remains charged to the VM.
type captureQueue struct {
	mu      sync.Mutex
	active  *capturePermit
	waiting []*capturePermit
}

type capturePermit struct {
	queue    *captureQueue
	released bool
}

func (q *captureQueue) request() *capturePermit {
	q.mu.Lock()
	defer q.mu.Unlock()
	permit := &capturePermit{queue: q}
	q.waiting = append(q.waiting, permit)
	return permit
}

func (p *capturePermit) take() bool {
	q := p.queue
	q.mu.Lock()
	defer q.mu.Unlock()
	if p.released || q.active != nil || len(q.waiting) == 0 || q.waiting[0] != p {
		return false
	}
	q.waiting[0] = nil
	q.waiting = q.waiting[1:]
	q.active = p
	return true
}

func (p *capturePermit) release() {
	if p == nil {
		return
	}
	q := p.queue
	q.mu.Lock()
	defer q.mu.Unlock()
	if p.released {
		return
	}
	p.released = true
	if q.active == p {
		q.active = nil
		return
	}
	for i, waiter := range q.waiting {
		if waiter == p {
			copy(q.waiting[i:], q.waiting[i+1:])
			q.waiting[len(q.waiting)-1] = nil
			q.waiting = q.waiting[:len(q.waiting)-1]
			return
		}
	}
}
