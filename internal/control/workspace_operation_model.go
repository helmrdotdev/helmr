package control

import (
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/wire"
)

func OperationFingerprint(operationKind db.WorkspaceOperationKind, request []byte) (string, error) {
	guestVerb, err := OperationGuestVerb(operationKind)
	if err != nil {
		return "", err
	}
	return wire.RequestFingerprint(guestVerb, request)
}

func OperationGuestVerb(operationKind db.WorkspaceOperationKind) (string, error) {
	return wire.GuestVerb(string(operationKind))
}

func ResourceKindString(kind db.WorkspaceResourceKind) string {
	return string(kind)
}
