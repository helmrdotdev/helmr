package guestd

import (
	"context"
	"errors"
	"io"
	"time"

	"github.com/helmrdotdev/helmr/internal/frameio"
	agentv1 "github.com/helmrdotdev/helmr/internal/proto/agent/v1"
)

type agentProgramTransfer struct {
	connection programConnection
	request    *agentv1.SessionAttach
}

func (t agentProgramTransfer) requestArtifact(kind agentv1.SessionProgramRequest_Kind) error {
	return frameio.WriteProtoFrame(t.connection, &agentv1.GuestSessionMessage{Identity: t.request.GetGrant().GetIdentity(), AttachmentSequence: t.request.GetAttachmentSequence(), Message: &agentv1.GuestSessionMessage_ProgramRequest{ProgramRequest: &agentv1.SessionProgramRequest{Kind: kind}}})
}
func (t agentProgramTransfer) copyArtifact(ctx context.Context, kind agentv1.SessionProgramRequest_Kind, descriptor *agentv1.SessionProgramArtifact, destination io.Writer) error {
	stop := context.AfterFunc(ctx, func() { _ = t.connection.Close() })
	defer stop()
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := t.requestArtifact(kind); err != nil {
		return err
	}
	_, err := io.CopyN(destination, t.connection, descriptor.SizeBytes)
	return err
}
func (t agentProgramTransfer) releaseStart(ctx context.Context) (*agentv1.SessionGrant, error) {
	stop := context.AfterFunc(ctx, func() { _ = t.connection.Close() })
	defer stop()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := t.requestArtifact(agentv1.SessionProgramRequest_KIND_RELEASE); err != nil {
		return nil, err
	}
	grant := new(agentv1.SessionGrant)
	if err := frameio.ReadProtoFrameBounded(t.connection, 64<<10, grant); err != nil {
		return nil, err
	}
	if !sameSessionGrantOwner(t.request.GetGrant(), grant) || grant.AuthorityGeneration < t.request.GetGrant().AuthorityGeneration {
		return nil, errors.New("session start release changed its owner")
	}
	if err := t.connection.SetReadDeadline(time.Time{}); err != nil {
		return nil, err
	}
	if err := t.connection.SetWriteDeadline(time.Time{}); err != nil {
		return nil, err
	}
	return grant, nil
}
