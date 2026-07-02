package control

import (
	"context"

	"github.com/google/uuid"
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/pgvalue"
)

func (s *Server) createCapacityPressureLiveRuntimeCheckpointWaitCommands(ctx context.Context, workerInstanceID uuid.UUID, trigger string) {
	if s.db == nil {
		return
	}
	commands, err := s.db.CreateCapacityPressureLiveRuntimeCheckpointWaitCommandsForWorker(ctx, db.CreateCapacityPressureLiveRuntimeCheckpointWaitCommandsForWorkerParams{
		WorkerInstanceID: pgvalue.UUID(workerInstanceID),
		GuestdAbi:        currentGuestdABI,
		AdapterAbi:       currentAdapterABI,
		LimitCount:       capacityPressureCheckpointCommandPreemptLimit,
	})
	if err != nil {
		if s.log != nil {
			s.log.Warn("capacity pressure live wait checkpoint preemption failed",
				"worker_instance_id", workerInstanceID.String(),
				"trigger", trigger,
				"error", err,
			)
		}
		return
	}
	if s.log != nil && len(commands) > 0 {
		s.log.Info("capacity pressure live wait checkpoint commands created",
			"worker_instance_id", workerInstanceID.String(),
			"trigger", trigger,
			"command_count", len(commands),
		)
	}
}

func (s *Server) requestCapacityPressureIdleWorkspaceStops(ctx context.Context, workerInstanceID uuid.UUID, trigger string) {
	if s.db == nil {
		return
	}
	stops, err := s.db.RequestCapacityPressureIdleWorkspaceMountStopsForWorker(ctx, db.RequestCapacityPressureIdleWorkspaceMountStopsForWorkerParams{
		WorkerInstanceID: pgvalue.UUID(workerInstanceID),
		LimitCount:       capacityPressureIdleStopPreemptLimit,
	})
	if err != nil {
		if s.log != nil {
			s.log.Warn("capacity pressure idle workspace stop preemption failed",
				"worker_instance_id", workerInstanceID.String(),
				"trigger", trigger,
				"error", err,
			)
		}
		return
	}
	if s.log != nil && len(stops) > 0 {
		s.log.Info("capacity pressure idle workspace stops requested",
			"worker_instance_id", workerInstanceID.String(),
			"trigger", trigger,
			"mount_count", len(stops),
		)
	}
}
