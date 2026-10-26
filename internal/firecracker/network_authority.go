//go:build linux

package firecracker

import (
	"errors"
	"github.com/helmrdotdev/helmr/internal/networkpolicy"
)

func (c *Connector) configForNetwork(policy networkpolicy.Policy, control networkpolicy.Installation) (Config, error) {
	if err := policy.Validate(); err != nil {
		return Config{}, err
	}
	if control.PrivateBusinessCIDRs == nil && control.BlockedIPv4CIDRs == nil {
		return Config{}, errors.New("network policy unsupported: allocation installation authority is missing")
	}
	cfg := c.cfg
	host := networkpolicy.Installation{PrivateBusinessCIDRs: cfg.NetworkPrivateBusinessCIDRs, BlockedIPv4CIDRs: cfg.NetworkBlockedIPv4CIDRs}
	if !networkpolicy.Compatible(host, control) {
		return Config{}, errors.New("network policy unsupported: worker installation is more permissive than allocation")
	}
	if err := networkpolicy.ValidateInstallation(control); err != nil {
		return Config{}, err
	}
	cfg.NetworkPolicy = policy
	// The authenticated control-plane snapshot is the immutable execution identity;
	// stricter host and physical interface denies are added at installation time.
	cfg.NetworkInstallation = control
	return cfg, nil
}
