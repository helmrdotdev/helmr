package control

import (
	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/wire"
)

func operationFingerprint(operationKind db.WorkspaceOperationKind, request []byte) (string, error) {
	guestVerb, err := operationGuestVerb(operationKind)
	if err != nil {
		return "", err
	}
	return wire.RequestFingerprint(guestVerb, request)
}

func operationGuestVerb(operationKind db.WorkspaceOperationKind) (string, error) {
	return wire.GuestVerb(string(operationKind))
}

func resourceKindString(kind db.WorkspaceResourceKind) string {
	return string(kind)
}
