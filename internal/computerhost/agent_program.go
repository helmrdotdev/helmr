package computerhost

import (
	"context"
	"errors"
	"io"
	"time"

	"github.com/helmrdotdev/helmr/internal/frameio"
	agentv1 "github.com/helmrdotdev/helmr/internal/proto/agent/v1"
	"github.com/helmrdotdev/helmr/internal/vm"
	"google.golang.org/protobuf/proto"
)

// AgentSessionStarter supplies verified immutable Program bytes and obtains a
// fresh execution grant after transfer. ReleaseStart must authorize first setup,
// not merely renew a retained process's control authority.
type AgentSessionStarter interface {
	WriteArtifact(context.Context, *agentv1.SessionProgramArtifact, io.Writer) error
	ReleaseStart(context.Context) (*agentv1.SessionGrant, error)
}

// StartAgentSession is the first-start path. An uncertain retry still attaches
// to the existing guest process; only the guest decides whether bytes are needed.
func StartAgentSession(ctx context.Context, machine vm.GuestControlMachine, request *agentv1.SessionAttach, starter AgentSessionStarter) (*AgentSessionConnection, *agentv1.SessionAttached, error) {
	if request.GetStart().GetProgram() == nil || starter == nil {
		return nil, nil, errors.New("session startup requires its pinned Program and start authority")
	}
	return openAgentSession(ctx, machine, request, starter)
}

func (connection *AgentSessionConnection) exchangeProgram(ctx context.Context, request *agentv1.SessionAttach, starter AgentSessionStarter, receipt *agentv1.GuestSessionMessage) error {
	seen := make(map[agentv1.SessionProgramRequest_Kind]bool)
	for receipt.GetProgramRequest() != nil {
		if starter == nil || !proto.Equal(receipt.GetIdentity(), connection.identity) || receipt.AttachmentSequence != connection.attachment {
			return errors.New("unexpected Session Program transfer")
		}
		kind := receipt.GetProgramRequest().Kind
		if seen[kind] || seen[agentv1.SessionProgramRequest_KIND_RELEASE] {
			return errors.New("repeated or out-of-order Session Program transfer")
		}
		seen[kind] = true
		var descriptor *agentv1.SessionProgramArtifact
		switch kind {
		case agentv1.SessionProgramRequest_KIND_RUNTIME:
			descriptor = request.GetStart().GetProgram().GetRuntime()
		case agentv1.SessionProgramRequest_KIND_ARTIFACT:
			descriptor = request.GetStart().GetProgram().GetArtifact()
		case agentv1.SessionProgramRequest_KIND_RELEASE:
			grant, err := starter.ReleaseStart(ctx)
			if err != nil {
				return err
			}
			if grant == nil {
				return errors.New("missing Session start release")
			}
			owner := proto.Clone(grant).(*agentv1.SessionGrant)
			owner.AuthorityGeneration = connection.grant.AuthorityGeneration
			owner.ExpiresAtUnixNano = connection.grant.ExpiresAtUnixNano
			if !proto.Equal(owner, connection.grant) || grant.AuthorityGeneration < connection.grant.AuthorityGeneration || grant.ExpiresAtUnixNano <= time.Now().UnixNano() {
				return errors.New("session start release changed or expired its owner")
			}
			if err = frameio.WriteProtoFrame(connection.stream, grant); err != nil {
				return err
			}
			connection.grant = proto.Clone(grant).(*agentv1.SessionGrant)
		default:
			return errors.New("unknown Session Program transfer")
		}
		if descriptor != nil {
			if descriptor.SizeBytes <= 0 {
				return errors.New("invalid Session Program size")
			}
			writer := &programArtifactWriter{writer: connection.stream, remaining: descriptor.SizeBytes}
			if err := starter.WriteArtifact(ctx, descriptor, writer); err != nil {
				return err
			}
			if writer.remaining != 0 {
				return errors.New("session Program source ended early")
			}
		} else if kind != agentv1.SessionProgramRequest_KIND_RELEASE {
			return errors.New("missing Session Program descriptor")
		}
		receipt.Reset()
		if err := frameio.ReadProtoFrameBounded(connection.stream, agentTransportFrameLimit, receipt); err != nil {
			return err
		}
	}
	return nil
}

type programArtifactWriter struct {
	writer    io.Writer
	remaining int64
}

func (w *programArtifactWriter) Write(p []byte) (int, error) {
	if int64(len(p)) > w.remaining {
		return 0, errors.New("session Program source exceeds its descriptor")
	}
	n, err := w.writer.Write(p)
	w.remaining -= int64(n)
	return n, err
}
