package computerhost

import (
	"context"
	"errors"
	"net"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/helmrdotdev/helmr/internal/frameio"
	agentv1 "github.com/helmrdotdev/helmr/internal/proto/agent/v1"
	"github.com/helmrdotdev/helmr/internal/workerapi"
)

type pressureRenewalClient struct {
	terminalControlClient
	generation atomic.Int64
	renewals   atomic.Int64
}

func (c *pressureRenewalClient) RenewAgentAuthority(context.Context, workerapi.RuntimeSession) (workerapi.AgentAuthorityResponse, error) {
	c.renewals.Add(1)
	return workerapi.AgentAuthorityResponse{AuthorityGeneration: c.generation.Load(), ExpiresAt: time.Now().Add(30 * time.Second)}, nil
}

func TestPressureStopPendingACKKeepsCurrentAuthorityAcrossRecovery(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		grant := sessionTransportGrant()
		grant.ExpiresAtUnixNano = time.Now().Add(30 * time.Second).UnixNano()
		grant.ChannelCredential = "channel-secret"
		var renewFrames, controlFrames atomic.Int64
		machine := sessionTransportMachine(t, func(stream net.Conn, _ *agentv1.SessionAttach) {
			for {
				var frame agentv1.HostSessionMessage
				if err := frameio.ReadProtoFrameBounded(stream, agentTransportFrameLimit, &frame); err != nil {
					return
				}
				if renewed := frame.GetRenew(); renewed != nil {
					if renewed.ExpiresAtUnixNano <= time.Now().UnixNano() {
						t.Error("guest received expired renewal")
					}
					renewFrames.Add(1)
				}
				if frame.GetCommand().GetControl() != nil {
					controlFrames.Add(1)
				}
			}
		})
		connection, attached, err := OpenAgentSession(t.Context(), machine, &agentv1.SessionAttach{Grant: grant, AttachmentSequence: 1})
		if err != nil {
			t.Fatal(err)
		}
		client := &pressureRenewalClient{}
		client.generation.Store(3)
		turn := "blocked-turn"
		deadline := time.Now().Add(40 * time.Second)
		client.prepare = func(context.Context, workerapi.AgentControlRequest) (workerapi.AgentControlResponse, error) {
			value := workerapi.AgentControlResponse{Sequence: 3, AuthorityGeneration: 3, Kind: "suspend", StopSequence: 1, StopTurnID: &turn, StopDeadline: &deadline, ExpiresAt: time.Now().Add(30 * time.Second)}
			if client.generation.Load() == 4 {
				value.Sequence = 4
				value.AuthorityGeneration = 4
				value.Kind = "resume"
				value.StopDeadline = &deadline
			}
			return value, nil
		}
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		done := make(chan error, 1)
		go func() {
			done <- ServeControlledAgentSession(ctx, connection, attached, "environment", client, func(context.Context, *agentv1.GuestSessionMessage) error { return errors.New("unexpected event") }, nil)
		}()
		synctest.Wait()
		time.Sleep(20 * time.Second)
		synctest.Wait()
		if client.renewals.Load() < 1 || renewFrames.Load() < 2 || controlFrames.Load() != 1 || connection.CurrentGrant().ExpiresAtUnixNano <= time.Now().UnixNano() {
			t.Fatalf("pending stop starved authority calls=%d frames=%d controls=%d", client.renewals.Load(), renewFrames.Load(), controlFrames.Load())
		}
		client.generation.Store(4)
		connection.localWork.notify()
		time.Sleep(10 * time.Second)
		synctest.Wait()
		if controlFrames.Load() != 2 || connection.CurrentGrant().AuthorityGeneration != 4 || connection.CurrentGrant().ExpiresAtUnixNano <= time.Now().UnixNano() {
			t.Fatal("pressure recovery starved authority")
		}
		cancel()
		_ = connection.Close()
		<-done
	})
}
