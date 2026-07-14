package control

import (
	"crypto/sha256"

	"github.com/helmrdotdev/helmr/internal/db"
)

func streamDataSHA256(data []byte) []byte {
	sum := sha256.Sum256(data)
	return sum[:]
}

func execChunkFromReceipt(receipt db.WorkspaceProcessStreamReceipt, data []byte) db.WorkspaceProcessStreamChunk {
	return db.WorkspaceProcessStreamChunk{
		ID:            receipt.ID,
		OrgID:         receipt.OrgID,
		ProjectID:     receipt.ProjectID,
		EnvironmentID: receipt.EnvironmentID,
		WorkspaceID:   receipt.WorkspaceID,
		ProcessID:     receipt.ProcessID,
		StreamName:    receipt.StreamName,
		Direction:     receipt.Direction,
		OffsetStart:   receipt.OffsetStart,
		OffsetEnd:     receipt.OffsetEnd,
		Data:          data,
		ObservedAt:    receipt.ObservedAt,
		CreatedAt:     receipt.CreatedAt,
	}
}

func ptyChunkFromReceipt(receipt db.WorkspaceProcessStreamReceipt, data []byte) db.WorkspaceProcessStreamChunk {
	return db.WorkspaceProcessStreamChunk{
		ID:            receipt.ID,
		OrgID:         receipt.OrgID,
		ProjectID:     receipt.ProjectID,
		EnvironmentID: receipt.EnvironmentID,
		WorkspaceID:   receipt.WorkspaceID,
		ProcessID:     receipt.ProcessID,
		StreamName:    receipt.StreamName,
		Direction:     receipt.Direction,
		OffsetStart:   receipt.OffsetStart,
		OffsetEnd:     receipt.OffsetEnd,
		Data:          data,
		ObservedAt:    receipt.ObservedAt,
		CreatedAt:     receipt.CreatedAt,
	}
}
