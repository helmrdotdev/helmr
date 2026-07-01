package executor

import (
	"context"
	"sync"
)

type BackgroundWorkGate struct {
	mu         sync.Mutex
	foreground int
	nextID     uint64
	cancels    map[uint64]context.CancelFunc
}

func NewBackgroundWorkGate() *BackgroundWorkGate {
	return &BackgroundWorkGate{cancels: map[uint64]context.CancelFunc{}}
}

func (g *BackgroundWorkGate) BeginForeground() func() {
	if g == nil {
		return func() {}
	}
	g.mu.Lock()
	g.foreground++
	for _, cancel := range g.cancels {
		cancel()
	}
	g.mu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			g.mu.Lock()
			if g.foreground > 0 {
				g.foreground--
			}
			g.mu.Unlock()
		})
	}
}

func (g *BackgroundWorkGate) BeginBackground(parent context.Context) (context.Context, func(), bool) {
	if g == nil {
		return parent, func() {}, true
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.foreground > 0 {
		return nil, func() {}, false
	}
	ctx, cancel := context.WithCancel(parent)
	id := g.nextID
	g.nextID++
	g.cancels[id] = cancel
	return ctx, func() {
		g.mu.Lock()
		delete(g.cancels, id)
		g.mu.Unlock()
		cancel()
	}, true
}
