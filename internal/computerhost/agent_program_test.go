package computerhost

import (
	"context"
	"errors"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/helmrdotdev/helmr/internal/frameio"
	agentv1 "github.com/helmrdotdev/helmr/internal/proto/agent/v1"
	"github.com/helmrdotdev/helmr/internal/wire"
	"google.golang.org/protobuf/proto"
)

type sessionProgramStarter struct {
	bytes    map[string]string
	grant    *agentv1.SessionGrant
	writes   int
	releases int
}

func (s *sessionProgramStarter) WriteArtifact(_ context.Context, d *agentv1.SessionProgramArtifact, w io.Writer) error {
	s.writes++
	_, err := io.WriteString(w, s.bytes[d.Digest])
	return err
}
func (s *sessionProgramStarter) ReleaseStart(context.Context) (*agentv1.SessionGrant, error) {
	s.releases++
	return s.grant, nil
}
func programSessionRequest() *agentv1.SessionAttach {
	return &agentv1.SessionAttach{Grant: sessionTransportGrant(), AttachmentSequence: 4, Start: &agentv1.SessionStart{Program: &agentv1.SessionProgram{
		Runtime: &agentv1.SessionProgramArtifact{Digest: "runtime", SizeBytes: 7}, Artifact: &agentv1.SessionProgramArtifact{Digest: "program", SizeBytes: 7},
	}}}
}
func programTransferMachine(t *testing.T, handle func(net.Conn, *agentv1.SessionAttach) error) (*agentControlMachine, <-chan error) {
	t.Helper()
	done := make(chan error, 1)
	machine := &agentControlMachine{handle: func(stream net.Conn) {
		header, _, err := wire.ReadStreamFrameHeader(stream)
		if err != nil {
			done <- err
			return
		}
		if header.Type != wire.StreamTypeAgentSession {
			done <- errors.New("wrong stream")
			return
		}
		request := new(agentv1.SessionAttach)
		if err = frameio.ReadProtoFrameBounded(stream, agentTransportFrameLimit, request); err == nil {
			err = handle(stream, request)
		}
		done <- err
	}}
	return machine, done
}
func requestProgramPart(stream net.Conn, request *agentv1.SessionAttach, kind agentv1.SessionProgramRequest_Kind) error {
	return frameio.WriteProtoFrame(stream, &agentv1.GuestSessionMessage{Identity: request.Grant.Identity, AttachmentSequence: request.AttachmentSequence, Message: &agentv1.GuestSessionMessage_ProgramRequest{ProgramRequest: &agentv1.SessionProgramRequest{Kind: kind}}})
}
func attachProgramReceipt(stream net.Conn, request *agentv1.SessionAttach) error {
	return frameio.WriteProtoFrame(stream, &agentv1.GuestSessionMessage{Identity: request.Grant.Identity, AttachmentSequence: request.AttachmentSequence, Message: &agentv1.GuestSessionMessage_Attached{Attached: &agentv1.SessionAttached{Ready: true}}})
}
func TestAgentSessionProgramTransferAndRetainedRetry(t *testing.T) {
	for _, retained := range []bool{false, true} {
		t.Run(map[bool]string{false: "transfer", true: "retained"}[retained], func(t *testing.T) {
			request := programSessionRequest()
			grant := proto.Clone(request.Grant).(*agentv1.SessionGrant)
			grant.AuthorityGeneration++
			starter := &sessionProgramStarter{bytes: map[string]string{"runtime": "runtime", "program": "program"}, grant: grant}
			machine, done := programTransferMachine(t, func(stream net.Conn, r *agentv1.SessionAttach) error {
				if !retained {
					for _, part := range []struct {
						kind agentv1.SessionProgramRequest_Kind
						body string
					}{{agentv1.SessionProgramRequest_KIND_RUNTIME, "runtime"}, {agentv1.SessionProgramRequest_KIND_ARTIFACT, "program"}} {
						if err := requestProgramPart(stream, r, part.kind); err != nil {
							return err
						}
						data := make([]byte, len(part.body))
						if _, err := io.ReadFull(stream, data); err != nil {
							return err
						}
						if string(data) != part.body {
							return errors.New("wrong Program bytes")
						}
					}
					if err := requestProgramPart(stream, r, agentv1.SessionProgramRequest_KIND_RELEASE); err != nil {
						return err
					}
					released := new(agentv1.SessionGrant)
					if err := frameio.ReadProtoFrameBounded(stream, 64<<10, released); err != nil {
						return err
					}
					if !proto.Equal(released, grant) {
						return errors.New("wrong release")
					}
				}
				return attachProgramReceipt(stream, r)
			})
			connection, receipt, err := StartAgentSession(t.Context(), machine, request, starter)
			if err != nil {
				t.Fatal(err)
			}
			defer connection.Close()
			if !receipt.Ready {
				t.Fatal("not attached")
			}
			if retained {
				if starter.writes != 0 || starter.releases != 0 {
					t.Fatal("retained retry reran transfer")
				}
			} else {
				if starter.writes != 2 || starter.releases != 1 || !proto.Equal(connection.CurrentGrant(), grant) {
					t.Fatal("transfer did not bind released grant")
				}
			}
			if err := <-done; err != nil {
				t.Fatal(err)
			}
		})
	}
}
func TestAgentSessionProgramRejectsInvalidTransfer(t *testing.T) {
	for _, mode := range []string{"short", "long", "repeat", "wrong attachment", "wrong owner", "expired release", "regressed release", "after release"} {
		t.Run(mode, func(t *testing.T) {
			request := programSessionRequest()
			grant := proto.Clone(request.Grant).(*agentv1.SessionGrant)
			starter := &sessionProgramStarter{bytes: map[string]string{"runtime": "runtime"}, grant: grant}
			if mode == "short" {
				starter.bytes["runtime"] = "short"
			}
			if mode == "long" {
				starter.bytes["runtime"] = "too long"
			}
			if mode == "wrong owner" {
				grant.ComputerId = "other"
			}
			if mode == "expired release" {
				grant.ExpiresAtUnixNano = time.Now().Add(-time.Second).UnixNano()
			}
			if mode == "regressed release" {
				grant.AuthorityGeneration--
			}
			machine, done := programTransferMachine(t, func(stream net.Conn, r *agentv1.SessionAttach) error {
				switch mode {
				case "short", "long", "repeat", "wrong attachment":
					if mode == "wrong attachment" {
						r.AttachmentSequence++
					}
					if err := requestProgramPart(stream, r, agentv1.SessionProgramRequest_KIND_RUNTIME); err != nil {
						return err
					}
					bytes := make([]byte, 7)
					if _, err := io.ReadFull(stream, bytes); err != nil {
						return err
					}
					return requestProgramPart(stream, r, agentv1.SessionProgramRequest_KIND_RUNTIME)
				default:
					if err := requestProgramPart(stream, r, agentv1.SessionProgramRequest_KIND_RELEASE); err != nil {
						return err
					}
					released := new(agentv1.SessionGrant)
					if err := frameio.ReadProtoFrameBounded(stream, 64<<10, released); err != nil {
						return err
					}
					return requestProgramPart(stream, r, agentv1.SessionProgramRequest_KIND_ARTIFACT)
				}
			})
			connection, _, err := StartAgentSession(t.Context(), machine, request, starter)
			if connection != nil {
				connection.Close()
			}
			if err == nil {
				t.Fatal("accepted invalid transfer")
			}
			<-done
		})
	}
}
func TestAgentSessionProgramCancellationClosesBlockedTransfer(t *testing.T) {
	request := programSessionRequest()
	requested := make(chan struct{})
	machine, done := programTransferMachine(t, func(stream net.Conn, r *agentv1.SessionAttach) error {
		if err := requestProgramPart(stream, r, agentv1.SessionProgramRequest_KIND_RUNTIME); err != nil {
			return err
		}
		close(requested)
		_, err := io.Copy(io.Discard, stream)
		return err
	})
	ctx, cancel := context.WithCancel(t.Context())
	starter := &sessionProgramStarter{bytes: map[string]string{"runtime": "runtime"}, grant: request.Grant}
	result := make(chan error, 1)
	go func() {
		connection, _, err := StartAgentSession(ctx, machine, request, starter)
		if connection != nil {
			connection.Close()
		}
		result <- err
	}()
	<-requested
	cancel()
	if err := <-result; !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation: %v", err)
	}
	<-done
	if starter.releases != 0 {
		t.Fatal("cancelled transfer released startup")
	}
}
func TestAgentSessionProgramMissingStarter(t *testing.T) {
	_, _, err := StartAgentSession(t.Context(), nil, programSessionRequest(), nil)
	if err == nil || !strings.Contains(err.Error(), "start authority") {
		t.Fatalf("missing startup dependency: %v", err)
	}
}
