package controlplane

import (
	"errors"
	"net/http"

	"github.com/helmrdotdev/helmr/internal/agent"
	"github.com/helmrdotdev/helmr/internal/workerapi"
)

func allocationIdentity(in workerapi.AllocationIdentity) (agent.HostAllocation, error) {
	env, e1 := agentComputerID(in.EnvironmentID)
	owner, e2 := agentComputerID(in.OwnerID)
	instance, e3 := agentComputerID(in.InstanceID)
	if e1 != nil || e2 != nil || e3 != nil || in.Epoch <= 0 || (in.Kind != "computer" && in.Kind != "preparation") {
		return agent.HostAllocation{}, agent.ErrInvalidInput
	}
	return agent.HostAllocation{Kind: in.Kind, EnvironmentID: env, OwnerID: owner, InstanceID: instance, Epoch: in.Epoch}, nil
}
func allocationWireIdentity(in agent.HostAllocation) workerapi.AllocationIdentity {
	return workerapi.AllocationIdentity{Kind: in.Kind, EnvironmentID: in.EnvironmentID.String(), OwnerID: in.OwnerID.String(), InstanceID: in.InstanceID.String(), Epoch: in.Epoch}
}
func allocationWireReceipt(kind string, in agent.Allocation) workerapi.AllocationIdentity {
	return allocationWireIdentity(agent.HostAllocation{Kind: kind, EnvironmentID: in.EnvironmentID, OwnerID: in.OwnerID, InstanceID: in.InstanceID, Epoch: in.Epoch})
}
func allocationWireShape(in agent.AllocationShape) workerapi.AllocationShape {
	return workerapi.AllocationShape{CPUMillis: in.CPUMillis, MemoryBytes: in.MemoryBytes, ScratchBytes: in.ScratchBytes, VMPlatformID: in.VMPlatformID, VCPUCount: in.VCPUCount, CPUConfigDigest: in.CPUConfigDigest}
}
func (server *Server) writeAllocationError(w http.ResponseWriter, err error) {
	var transport apiError
	if errors.As(err, &transport) {
		writeError(w, err)
		return
	}
	if errors.Is(err, agent.ErrAllocationClosed) {
		writeError(w, conflict(codedError{code: workerapi.AllocationClosed, message: "allocation requires physical stop confirmation"}))
		return
	}
	server.writeAgentComputerError(w, err)
}
func (server *Server) workerListAllocations(w http.ResponseWriter, r *http.Request) {
	var request workerapi.AllocationListRequest
	if err := decodeRequestJSON(r, &request); err != nil {
		server.writeAllocationError(w, err)
		return
	}
	var after *agent.HostAllocation
	if request.After != nil {
		parsed, err := allocationIdentity(*request.After)
		if err != nil {
			server.writeAllocationError(w, err)
			return
		}
		after = &parsed
	}
	records, err := agent.ListHostAllocations(r.Context(), server.tx, workerFromContext(r.Context()), after)
	if err != nil {
		server.writeAllocationError(w, err)
		return
	}
	response := workerapi.AllocationListResponse{Allocations: make([]workerapi.AllocationIdentity, 0, len(records))}
	for _, record := range records {
		response.Allocations = append(response.Allocations, allocationWireIdentity(record))
	}
	writeJSON(w, http.StatusOK, response)
}
func decodeAllocationIdentity(r *http.Request, kind string) (agent.HostAllocation, error) {
	var request workerapi.AllocationIdentity
	if err := decodeRequestJSON(r, &request); err != nil {
		return agent.HostAllocation{}, err
	}
	if request.Kind != kind {
		return agent.HostAllocation{}, agent.ErrInvalidInput
	}
	return allocationIdentity(request)
}
func (server *Server) workerDeliverPreparationAllocation(w http.ResponseWriter, r *http.Request) {
	identity, err := decodeAllocationIdentity(r, "preparation")
	if err != nil {
		server.writeAllocationError(w, err)
		return
	}
	result, err := server.allocator.DeliverPreparation(r.Context(), workerFromContext(r.Context()), agent.PreparationIdentity{EnvironmentID: identity.EnvironmentID, PreparationID: identity.OwnerID, InstanceID: identity.InstanceID, Epoch: identity.Epoch})
	if err != nil {
		server.writeAllocationError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, workerapi.PreparationAllocationDelivery{Identity: allocationWireReceipt("preparation", result.Allocation), Shape: allocationWireShape(result.Shape), ExpiresAt: *result.ExpiresAt, ChannelCredential: result.Executor.ChannelCredential})
}
func computerAllocationIdentity(in agent.HostAllocation) agent.ComputerLeaseIdentity {
	return agent.ComputerLeaseIdentity{EnvironmentID: in.EnvironmentID, ComputerID: in.OwnerID, InstanceID: in.InstanceID, Epoch: in.Epoch}
}
func (server *Server) workerDeliverComputerAllocation(w http.ResponseWriter, r *http.Request) {
	identity, err := decodeAllocationIdentity(r, "computer")
	if err != nil {
		server.writeAllocationError(w, err)
		return
	}
	result, err := server.allocator.DeliverComputer(r.Context(), workerFromContext(r.Context()), computerAllocationIdentity(identity))
	if err != nil {
		server.writeAllocationError(w, err)
		return
	}
	response := workerapi.ComputerAllocationDelivery{Identity: allocationWireReceipt("computer", result.Allocation), Shape: allocationWireShape(result.Shape), ExpiresAt: *result.ExpiresAt, ChannelCredential: result.ChannelCredential, BaseVersion: result.BaseVersion}
	if result.RestoredFrom != nil {
		response.RestoredFrom = result.RestoredFrom.String()
	}
	writeJSON(w, http.StatusOK, response)
}
func (server *Server) workerFreshComputerReady(w http.ResponseWriter, r *http.Request) {
	var request workerapi.ComputerAllocationReady
	if err := decodeRequestJSON(r, &request); err != nil {
		server.writeAllocationError(w, err)
		return
	}
	if request.Identity.Kind != "computer" {
		server.writeAllocationError(w, agent.ErrInvalidInput)
		return
	}
	identity, err := allocationIdentity(request.Identity)
	if err != nil {
		server.writeAllocationError(w, err)
		return
	}
	shape := agent.AllocationShape{CPUMillis: request.Shape.CPUMillis, MemoryBytes: request.Shape.MemoryBytes, ScratchBytes: request.Shape.ScratchBytes, VMPlatformID: request.Shape.VMPlatformID, VCPUCount: request.Shape.VCPUCount, CPUConfigDigest: request.Shape.CPUConfigDigest}
	if err := agent.ObserveFreshComputerReady(r.Context(), server.tx, workerFromContext(r.Context()), computerAllocationIdentity(identity), shape, request.BaseVersion); err != nil {
		server.writeAllocationError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
func (server *Server) workerPreparationStopped(w http.ResponseWriter, r *http.Request) {
	identity, err := decodeAllocationIdentity(r, "preparation")
	if err != nil {
		server.writeAllocationError(w, err)
		return
	}
	if err := agent.ObservePreparationStopped(r.Context(), server.tx, workerFromContext(r.Context()), agent.PreparationIdentity{EnvironmentID: identity.EnvironmentID, PreparationID: identity.OwnerID, InstanceID: identity.InstanceID, Epoch: identity.Epoch}); err != nil {
		server.writeAllocationError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (server *Server) workerListComputerProcesses(w http.ResponseWriter, r *http.Request) {
	var request workerapi.ComputerProcessesRequest
	if err := decodeRequestJSON(r, &request); err != nil {
		server.writeAllocationError(w, err)
		return
	}
	identity, err := allocationIdentity(request.Identity)
	if err != nil || identity.Kind != "computer" {
		server.writeAllocationError(w, agent.ErrInvalidInput)
		return
	}
	var after *agent.ProcessIdentity
	if request.After != nil {
		id, err := agentComputerID(request.After.SessionID)
		if err != nil || request.After.Epoch <= 0 {
			server.writeAllocationError(w, agent.ErrInvalidInput)
			return
		}
		after = &agent.ProcessIdentity{SessionID: id, Epoch: request.After.Epoch}
	}
	processes, err := agent.ListComputerProcesses(r.Context(), server.tx, workerFromContext(r.Context()), computerAllocationIdentity(identity), after)
	if err != nil {
		server.writeAllocationError(w, err)
		return
	}
	response := workerapi.ComputerProcessesResponse{Processes: make([]workerapi.ProcessIdentity, 0, len(processes))}
	for _, p := range processes {
		response.Processes = append(response.Processes, workerapi.ProcessIdentity{SessionID: p.SessionID.String(), Epoch: p.Epoch})
	}
	writeJSON(w, http.StatusOK, response)
}

func (server *Server) workerComputerAllocationSource(w http.ResponseWriter, r *http.Request) {
	identity, err := decodeAllocationIdentity(r, "computer")
	if err != nil {
		server.writeAllocationError(w, err)
		return
	}
	source, err := server.allocationKeys.Source(r.Context(), workerFromContext(r.Context()), computerAllocationIdentity(identity))
	if err != nil {
		server.writeAllocationError(w, err)
		return
	}
	defer source.Clear()
	boot, err := agent.ReadComputerBoot(r.Context(), server.tx, workerFromContext(r.Context()), computerAllocationIdentity(identity))
	if err != nil {
		server.writeAllocationError(w, err)
		return
	}
	response := workerapi.ComputerAllocationSource{Disk: workerapi.ComputerSourceMaterial{VersionID: source.BaseVersion, Root: source.Root, WriteKeyID: source.WriteKeyID.String(), Keys: make([]workerapi.ComputerKeyMaterial, 0, len(source.Keys))}, ImageConfig: boot.ImageConfig, RootfsDigest: boot.RootfsDigest}
	for _, key := range source.Keys {
		response.Disk.Keys = append(response.Disk.Keys, workerapi.ComputerKeyMaterial{Scope: source.Scope, ID: key.ID.String(), Key: key.Key})
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, response)
}
