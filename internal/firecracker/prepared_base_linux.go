//go:build linux

package firecracker

import (
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"sync"

	"github.com/helmrdotdev/helmr/internal/compute"
	"github.com/helmrdotdev/helmr/internal/vm"
)

type PreparedBaseConnector struct {
	base *Connector
	size int
	log  *slog.Logger

	mu      sync.Mutex
	ctx     context.Context
	cancel  context.CancelFunc
	entries map[string][]vm.Session
	filling map[string]int

	BackgroundGate interface {
		BeginBackground(context.Context) (context.Context, func(), bool)
	}
}

func NewPreparedBaseConnector(base *Connector, size int, log *slog.Logger) *PreparedBaseConnector {
	return &PreparedBaseConnector{
		base:    base,
		size:    size,
		log:     log,
		entries: map[string][]vm.Session{},
		filling: map[string]int{},
	}
}

func (c *PreparedBaseConnector) Start(ctx context.Context, network compute.NetworkPolicy) {
	if c == nil || c.size <= 0 || c.base == nil {
		return
	}
	c.mu.Lock()
	if c.ctx == nil {
		c.ctx, c.cancel = context.WithCancel(ctx)
		go c.closeOnDone(c.ctx)
	}
	c.mu.Unlock()
	c.refill(network)
}

func (c *PreparedBaseConnector) Close(ctx context.Context) error {
	if c == nil {
		return nil
	}
	c.mu.Lock()
	if c.cancel != nil {
		c.cancel()
	}
	var sessions []vm.Session
	for key, entries := range c.entries {
		sessions = append(sessions, entries...)
		delete(c.entries, key)
	}
	c.mu.Unlock()
	var err error
	for _, session := range sessions {
		if closeErr := session.Close(ctx); closeErr != nil && err == nil {
			err = closeErr
		}
	}
	return err
}

func (c *PreparedBaseConnector) Connect(ctx context.Context, request vm.ConnectRequest) (vm.Session, error) {
	return c.base.Connect(ctx, request)
}

func (c *PreparedBaseConnector) Materialize(ctx context.Context, request vm.MaterializeRequest) (vm.Session, error) {
	if err := c.base.validateMaterializeRequest(request); err != nil {
		return nil, err
	}
	if request.Topology.Substrate != nil {
		c.logInfo("prepared base connector bypassed", "reason", "runtime_substrate_topology")
		return c.base.Connect(ctx, vm.ConnectRequest{Network: request.Network, Topology: request.Topology})
	}
	if session, ok := c.checkout(request.Network); ok {
		c.logInfo("prepared base connector hit")
		c.refill(request.Network)
		return session, nil
	}
	c.logInfo("prepared base connector miss")
	session, err := c.base.Connect(ctx, vm.ConnectRequest{Network: request.Network, Topology: request.Topology})
	c.refill(request.Network)
	return session, err
}

func (c *PreparedBaseConnector) checkout(network compute.NetworkPolicy) (vm.Session, bool) {
	key := networkKey(network)
	c.mu.Lock()
	defer c.mu.Unlock()
	entries := c.entries[key]
	if len(entries) == 0 {
		return nil, false
	}
	session := entries[len(entries)-1]
	c.entries[key] = entries[:len(entries)-1]
	return session, true
}

func (c *PreparedBaseConnector) refill(network compute.NetworkPolicy) {
	if c == nil || c.size <= 0 || c.base == nil {
		return
	}
	key := networkKey(network)
	c.mu.Lock()
	ctx := c.ctx
	if ctx == nil {
		c.mu.Unlock()
		return
	}
	available := len(c.entries[key]) + c.filling[key]
	if available >= c.size {
		c.mu.Unlock()
		return
	}
	c.filling[key]++
	c.mu.Unlock()
	backgroundCtx, finish, ok := c.beginBackground(ctx)
	if !ok {
		c.mu.Lock()
		c.filling[key]--
		c.mu.Unlock()
		c.logInfo("prepared base connector refill skipped", "reason", "foreground_active")
		return
	}
	go func() {
		defer finish()
		c.refillOne(backgroundCtx, key, network)
	}()
}

func (c *PreparedBaseConnector) refillOne(ctx context.Context, key string, network compute.NetworkPolicy) {
	session, err := c.base.Connect(ctx, vm.ConnectRequest{Network: network})
	if err != nil {
		c.logInfo("prepared base connector refill failed", "error", err.Error())
		c.mu.Lock()
		c.filling[key]--
		c.mu.Unlock()
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.filling[key]--
	if ctx.Err() != nil || len(c.entries[key]) >= c.size {
		go func() { _ = session.Close(context.Background()) }()
		return
	}
	c.entries[key] = append(c.entries[key], session)
	c.logInfo("prepared base connector refilled")
}

func (c *PreparedBaseConnector) closeOnDone(ctx context.Context) {
	<-ctx.Done()
	_ = c.Close(context.Background())
}

func (c *PreparedBaseConnector) logInfo(message string, attrs ...any) {
	if c == nil || c.log == nil {
		return
	}
	c.log.Info(message, attrs...)
}

func (c *PreparedBaseConnector) beginBackground(ctx context.Context) (context.Context, func(), bool) {
	if c == nil || c.BackgroundGate == nil {
		return ctx, func() {}, true
	}
	return c.BackgroundGate.BeginBackground(ctx)
}

func networkKey(network compute.NetworkPolicy) string {
	body, err := json.Marshal(network)
	if err != nil {
		return strings.TrimSpace(err.Error())
	}
	return string(body)
}
