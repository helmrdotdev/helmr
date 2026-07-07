package control

import (
	"bytes"
	"fmt"

	"github.com/helmrdotdev/helmr/internal/db"
	"github.com/helmrdotdev/helmr/internal/wire"
)

func operationFingerprint(operationKind db.WorkspaceOperationKind, request []byte) (string, error) {
	guestVerb, err := operationGuestVerbForRequest(operationKind, request)
	if err != nil {
		return "", err
	}
	return wire.RequestFingerprint(guestVerb, request)
}

func operationGuestVerbForRequest(operationKind db.WorkspaceOperationKind, request []byte) (string, error) {
	switch operationKind {
	case db.WorkspaceOperationKindStartProcess:
		switch {
		case bytes.Contains(request, []byte(`"exec_id"`)):
			return wire.GuestVerb("start_exec")
		case bytes.Contains(request, []byte(`"pty_id"`)):
			return wire.GuestVerb("create_pty")
		default:
			return "", fmt.Errorf("start_process request must include exec_id or pty_id")
		}
	case db.WorkspaceOperationKindResizeProcess:
		return wire.GuestVerb("resize_pty")
	case db.WorkspaceOperationKindCloseProcess:
		return wire.GuestVerb("close_pty")
	default:
		return "", fmt.Errorf("unsupported workspace operation kind %q", operationKind)
	}
}
