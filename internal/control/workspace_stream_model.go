package control

import (
	"crypto/sha256"

	"github.com/helmrdotdev/helmr/internal/db"
)

func streamDataSHA256(data []byte) []byte {
	sum := sha256.Sum256(data)
	return sum[:]
}

func execChunkFromReceipt(receipt db.WorkspaceExecStreamChunkReceipt, data []byte) db.WorkspaceExecStreamChunk {
	return db.WorkspaceExecStreamChunk{
		ID:            receipt.ID,
		OrgID:         receipt.OrgID,
		ProjectID:     receipt.ProjectID,
		EnvironmentID: receipt.EnvironmentID,
		WorkspaceID:   receipt.WorkspaceID,
		ExecID:        receipt.ExecID,
		Stream:        receipt.Stream,
		OffsetStart:   receipt.OffsetStart,
		OffsetEnd:     receipt.OffsetEnd,
		Data:          data,
		ObservedAt:    receipt.ObservedAt,
		CreatedAt:     receipt.CreatedAt,
	}
}

func ptyChunkFromReceipt(receipt db.WorkspacePtyStreamChunkReceipt, data []byte) db.WorkspacePtyStreamChunk {
	return db.WorkspacePtyStreamChunk{
		ID:            receipt.ID,
		OrgID:         receipt.OrgID,
		ProjectID:     receipt.ProjectID,
		EnvironmentID: receipt.EnvironmentID,
		WorkspaceID:   receipt.WorkspaceID,
		PtySessionID:  receipt.PtySessionID,
		Stream:        receipt.Stream,
		OffsetStart:   receipt.OffsetStart,
		OffsetEnd:     receipt.OffsetEnd,
		Data:          data,
		ObservedAt:    receipt.ObservedAt,
		CreatedAt:     receipt.CreatedAt,
	}
}
