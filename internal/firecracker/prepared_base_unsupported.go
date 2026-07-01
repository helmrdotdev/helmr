//go:build !linux

package firecracker

import (
	"context"
	"log/slog"

	"github.com/helmrdotdev/helmr/internal/compute"
	"github.com/helmrdotdev/helmr/internal/vm"
)

type PreparedBaseConnector struct {
	base *Connector

	BackgroundGate interface {
		BeginBackground(context.Context) (context.Context, func(), bool)
	}
}

func NewPreparedBaseConnector(base *Connector, _ int, _ *slog.Logger) *PreparedBaseConnector {
	return &PreparedBaseConnector{base: base}
}

func (c *PreparedBaseConnector) Start(context.Context, compute.NetworkPolicy) {}

func (c *PreparedBaseConnector) Close(context.Context) error { return nil }

func (c *PreparedBaseConnector) Connect(ctx context.Context, request vm.ConnectRequest) (vm.Session, error) {
	return c.base.Connect(ctx, request)
}

func (c *PreparedBaseConnector) Materialize(ctx context.Context, request vm.MaterializeRequest) (vm.Session, error) {
	return c.base.Materialize(ctx, request)
}
