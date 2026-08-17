//go:build !linux

package firecracker

import "context"

func (*Connector) HostRuntimeEvidence(context.Context) (HostRuntimeEvidence, error) {
	return HostRuntimeEvidence{}, ErrUnsupported
}
