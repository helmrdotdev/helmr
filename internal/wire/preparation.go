package wire

import (
	"errors"
	computerv0 "github.com/helmrdotdev/helmr/internal/proto/computer/v0"
	"google.golang.org/protobuf/proto"
	"math"
)

// Leave environment launch headroom for the
// image environment, runtime arguments and pointer array under Linux ARG_MAX.
const PreparationSecretEnvironmentBytes = 1 << 20
const PreparationControlFrameBytes = PreparationSecretEnvironmentBytes + (64 << 10)

func ValidatePreparationControl(request *computerv0.PreparationControlRequest) error {
	if request.GetStart() != nil {
		if err := ValidatePreparationLogLimits(request.GetStart().GetLogLimits()); err != nil {
			return err
		}
	}
	if len(request.GetStart().GetSecrets())+len(request.GetStart().GetProtectedEnv()) > 64 || proto.Size(request) > PreparationControlFrameBytes {
		return errors.New("preparation startup exceeds its transport limit")
	}
	environmentBytes := 0
	for name, placeholder := range request.GetStart().GetProtectedEnv() {
		entryBytes := len(name) + len(placeholder) + 2
		if entryBytes > 128<<10 {
			return errors.New("preparation Secret exceeds the environment entry limit")
		}
		environmentBytes += entryBytes
	}
	for _, secret := range request.GetStart().GetSecrets() {
		switch secret.GetPlacementKind() {
		case "env":
			entryBytes := len(secret.GetPlacementTarget()) + len(secret.GetValue()) + 2 // '=' and NUL
			if entryBytes > 128<<10 {
				return errors.New("preparation Secret exceeds the environment entry limit")
			}
			environmentBytes += entryBytes
		case "file":
			// File contents share the bounded startup frame but do not consume ARG_MAX.
		default:
			return errors.New("invalid preparation Secret placement")
		}
	}
	if environmentBytes > PreparationSecretEnvironmentBytes {
		return errors.New("preparation Secrets exceed their environment limit")
	}
	return nil
}

func ValidatePreparationLogLimits(limits *computerv0.PreparationLogLimits) error {
	if limits.GetChunkBytes() <= 0 || limits.GetChunkBytes() > PreparationControlFrameBytes-4096 || limits.GetBufferBytes() < int64(limits.GetChunkBytes()) || limits.GetBufferRecords() <= 0 {
		return errors.New("preparation logs require positive, consistent transport bounds")
	}
	return nil
}

func ValidatePreparationLog(log *computerv0.PreparationLog, chunkBytes int32) error {
	if log == nil || log.Sequence <= 0 || log.ThroughSequence < log.Sequence || log.ObservedAtUnixNano <= 0 || len(log.Data) > int(chunkBytes) {
		return errors.New("invalid preparation log envelope")
	}
	switch log.Kind {
	case "data":
		if log.Sequence != log.ThroughSequence || log.ThroughSequence == math.MaxInt64 || len(log.Data) == 0 || log.DroppedBytes != 0 || log.Complete {
			return errors.New("invalid preparation data")
		}
	case "gap":
		if log.ThroughSequence == math.MaxInt64 || len(log.Data) != 0 || log.DroppedBytes < log.ThroughSequence-log.Sequence+1 || log.Complete {
			return errors.New("invalid preparation gap")
		}
	case "end":
		if log.Sequence != log.ThroughSequence || len(log.Data) != 0 || log.DroppedBytes != 0 {
			return errors.New("invalid preparation boundary")
		}
	default:
		return errors.New("invalid preparation log kind")
	}
	return nil
}
