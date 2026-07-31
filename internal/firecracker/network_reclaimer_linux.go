//go:build linux

package firecracker

import (
	"context"
	"errors"
	"strings"

	"github.com/helmrdotdev/helmr/internal/vm"
)

// NetworkReclaimer owns startup and drain recovery of routed attachments. It
// deliberately needs no boot artifacts or running Connector: the persisted
// owner manifest is the sole physical cleanup authority.
type NetworkReclaimer struct {
	connector *Connector
}

func NewNetworkReclaimer(cfg Config) (*NetworkReclaimer, error) {
	if strings.TrimSpace(cfg.StateDir) == "" || strings.TrimSpace(cfg.IPPath) == "" || strings.TrimSpace(cfg.NFTPath) == "" {
		return nil, errors.New("network reclaimer paths are incomplete")
	}
	if cfg.JailerUID <= 0 || cfg.JailerGID <= 0 {
		return nil, errors.New("network reclaimer TAP ownership is incomplete")
	}
	if _, _, err := configuredNetworkPools(cfg); err != nil {
		return nil, err
	}
	return &NetworkReclaimer{connector: &Connector{cfg: cfg}}, nil
}

func (reclaimer *NetworkReclaimer) Reclaim(ctx context.Context, owner vm.Owner) error {
	if reclaimer == nil || reclaimer.connector == nil {
		return errors.New("network reclaimer is nil")
	}
	if err := owner.Validate(); err != nil {
		return err
	}
	return reclaimer.connector.cleanupNetworkAttachment(ctx, owner)
}
