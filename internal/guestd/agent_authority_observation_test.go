package guestd

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"net"
	"strings"
	"testing"
	"time"

	agentv1 "github.com/helmrdotdev/helmr/internal/proto/agent/v1"
	"google.golang.org/protobuf/proto"
)

func TestAgentRenewalAndExpiryFailureWithoutSecrets(t *testing.T) {
	var output bytes.Buffer
	prior := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&output, nil)))
	defer slog.SetDefault(prior)
	session, process := agentSessionFixture(t)
	session.grant.ChannelCredential = "private-channel"
	session.entry.channelCredential = "private-channel"
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	closed := make(chan error, 1)
	relay := newAgentRelay(ctx, session, func(err error) { closed <- err })
	guest, host := net.Pipe()
	defer guest.Close()
	defer host.Close()
	relay.connection = guest
	relay.attachment = 3
	renewed := proto.Clone(session.grant).(*agentv1.SessionGrant)
	renewed.AuthorityGeneration++
	renewed.ExpiresAtUnixNano += int64(time.Second)
	if err := relay.handleHostMessage(guest, 3, &agentv1.HostSessionMessage{AttachmentSequence: 3, Message: &agentv1.HostSessionMessage_Renew{Renew: renewed}}); err != nil {
		t.Fatal(err)
	}
	wrong := proto.Clone(renewed).(*agentv1.SessionGrant)
	wrong.WorkerHostId = "wrong-owner"
	if err := relay.handleHostMessage(guest, 3, &agentv1.HostSessionMessage{AttachmentSequence: 3, Message: &agentv1.HostSessionMessage_Renew{Renew: wrong}}); err == nil {
		t.Fatal("wrong-owner renewal accepted")
	}
	session.clock = func() time.Time { return time.Unix(0, renewed.ExpiresAtUnixNano) }
	process.freezeError = fmt.Errorf("private-freeze-error: %w", context.DeadlineExceeded)
	done := make(chan struct{})
	go func() { defer close(done); relay.expiryLoop() }()
	<-done
	<-closed
	if !session.terminal || !session.physicalClosed {
		t.Fatal("expiry freeze failure did not close process")
	}
	var failed bool
	for _, event := range relay.outbox {
		failed = failed || event.GetEvent().GetFailed() != nil
	}
	if !failed {
		t.Fatal("expiry freeze failure was not retained")
	}
	if strings.Contains(output.String(), "private-channel") || strings.Contains(output.String(), "private-freeze-error") {
		t.Fatal("authority evidence leaked private details")
	}
}

func TestAgentAuthorityRejectsSupersededRenewalWithoutMutation(t *testing.T) {
	session, _ := agentSessionFixture(t)
	relay := newAgentRelay(t.Context(), session, func(error) {})
	guest, host := net.Pipe()
	defer guest.Close()
	defer host.Close()
	relay.connection, relay.attachment = guest, 3
	original := proto.Clone(session.grant).(*agentv1.SessionGrant)
	renewed := proto.Clone(original).(*agentv1.SessionGrant)
	renewed.AuthorityGeneration++
	renewed.ExpiresAtUnixNano += int64(time.Minute)
	message := &agentv1.HostSessionMessage{AttachmentSequence: 2, Message: &agentv1.HostSessionMessage_Renew{Renew: renewed}}
	if err := relay.handleHostMessage(guest, 3, message); err == nil || err.Error() != "superseded Session connection" {
		t.Fatalf("stale attachment rejection changed: %v", err)
	}
	if !proto.Equal(session.grant, original) {
		t.Fatal("stale attachment changed current authority")
	}
}
