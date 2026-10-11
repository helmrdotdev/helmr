package guestd

import (
	"crypto/subtle"
	"errors"
	agentv1 "github.com/helmrdotdev/helmr/internal/proto/agent/v1"
	"google.golang.org/protobuf/proto"
	"strings"
	"time"
)

var errSessionGrantExpired = errors.New("session grant expired")

func validateSessionGrant(entry *computerMountEntry, grant *agentv1.SessionGrant, now time.Time) error {
	if entry == nil || grant == nil || grant.GetIdentity() == nil ||
		strings.TrimSpace(grant.GetIdentity().GetSessionId()) == "" || grant.GetIdentity().GetProcessEpoch() <= 0 ||
		strings.TrimSpace(grant.GetWorkerHostId()) == "" || grant.GetComputerLeaseEpoch() <= 0 || grant.GetAuthorityGeneration() <= 0 ||
		grant.GetWriterGeneration() <= 0 || grant.GetChannelCredential() == "" {
		return errors.New("session grant is incomplete or expired")
	}
	if grant.GetExpiresAtUnixNano() <= now.UnixNano() {
		return errSessionGrantExpired
	}
	if grant.GetComputerId() != entry.computerID || grant.GetComputerInstanceId() != entry.computerInstanceID ||
		grant.GetWriterGeneration() != int64(entry.currentWriterGeneration()) ||
		subtle.ConstantTimeCompare([]byte(grant.GetChannelCredential()), []byte(entry.channelCredential)) != 1 {
		return errors.New("session grant does not own the mounted Computer")
	}
	return nil
}

// Ordinary renewal cannot move physical ownership or replace a process epoch.
// Coherent restoration has a separate capture-bound installation boundary.
func sameSessionGrantOwner(left, right *agentv1.SessionGrant) bool {
	if left == nil || right == nil {
		return false
	}
	a := proto.Clone(left).(*agentv1.SessionGrant)
	b := proto.Clone(right).(*agentv1.SessionGrant)
	a.ExpiresAtUnixNano, b.ExpiresAtUnixNano = 0, 0
	a.AuthorityGeneration, b.AuthorityGeneration = 0, 0
	return proto.Equal(a, b)
}
