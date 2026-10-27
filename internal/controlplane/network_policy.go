package controlplane

import (
	"errors"
	"net/http"

	"github.com/helmrdotdev/helmr/internal/computer"
	"github.com/helmrdotdev/helmr/internal/dispatch"
	"github.com/helmrdotdev/helmr/internal/networkpolicy"
	"github.com/helmrdotdev/helmr/internal/workerapi"
)

var errNetworkPolicyUnsupported = errors.New("network policy unsupported by this worker")

func (server *Server) checkWorkerNetwork(report *workerapi.NetworkCapabilities) error {
	if report == nil || report.Version != workerapi.NetworkPolicyVersion || report.Installation.PrivateBusinessCIDRs == nil || report.Installation.BlockedIPv4CIDRs == nil || !networkpolicy.Compatible(report.Installation, server.networkInstallation) {
		return errNetworkPolicyUnsupported
	}
	return nil
}

func (server *Server) decodeNetworkAllocation(r *http.Request, kind string) (dispatch.HostAllocation, error) {
	var request workerapi.AllocationRequest
	if err := decodeRequestJSON(r, &request); err != nil {
		return dispatch.HostAllocation{}, err
	}
	if request.Kind != kind {
		return dispatch.HostAllocation{}, computer.ErrInvalidInput
	}
	if err := server.checkWorkerNetwork(request.Network); err != nil {
		return dispatch.HostAllocation{}, err
	}
	return allocationIdentity(request.AllocationIdentity)
}
