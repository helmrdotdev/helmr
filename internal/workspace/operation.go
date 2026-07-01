package workspace

import (
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/workspace/protocol"
)

func OperationFingerprint(operationKind db.WorkspaceOperationKind, request []byte) (string, error) {
	guestVerb, err := OperationGuestVerb(operationKind)
	if err != nil {
		return "", err
	}
	return protocol.RequestFingerprint(guestVerb, request)
}

func OperationGuestVerb(operationKind db.WorkspaceOperationKind) (string, error) {
	return protocol.GuestVerb(string(operationKind))
}

func ResourceKindString(kind db.WorkspaceResourceKind) string {
	return string(kind)
}
