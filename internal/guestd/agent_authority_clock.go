package guestd

import (
	"bytes"
	"crypto/rand"
	"errors"
	"math"
	"time"

	"github.com/helmrdotdev/helmr/internal/frameio"
	agentv1 "github.com/helmrdotdev/helmr/internal/proto/agent/v1"
)

// The host samples authority time after receiving a connection-local challenge.
// Anchoring that sample before sending the challenge counts transport delay
// against the lease rather than extending authority after a paused VM resumes.
type computerAuthorityClock struct {
	anchor    time.Time
	authority int64
}

func (clock *computerAuthorityClock) now() time.Time {
	elapsed := time.Since(clock.anchor).Nanoseconds()
	if elapsed > math.MaxInt64-clock.authority {
		return time.Unix(0, math.MaxInt64)
	}
	return time.Unix(0, clock.authority+elapsed)
}

// Recalibration must not make a previously expired deadline live again.
func (clock *computerAuthorityClock) notBefore(previous *computerAuthorityClock) *computerAuthorityClock {
	if previous == nil {
		return clock
	}
	anchor := time.Now()
	current := clock.now().UnixNano()
	if old := previous.now().UnixNano(); old > current {
		current = old
	}
	return &computerAuthorityClock{anchor: anchor, authority: current}
}

func (entry *computerMountEntry) authorityNow() time.Time {
	if clock := entry.authorityClock.Load(); clock != nil {
		return clock.now()
	}
	return time.Now()
}

func observeComputerAuthority(connection programConnection) (*computerAuthorityClock, error) {
	nonce := make([]byte, 32)
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	anchor := time.Now()
	if err := connection.SetWriteDeadline(anchor.Add(10 * time.Second)); err != nil {
		return nil, err
	}
	if err := writeAgentTransportFrame(connection, &agentv1.ComputerAuthorityChallenge{Nonce: nonce}); err != nil {
		return nil, err
	}
	var observation agentv1.ComputerAuthorityObservation
	if err := frameio.ReadProtoFrameBounded(connection, 256, &observation); err != nil {
		return nil, err
	}
	return computerAuthorityObservation(nonce, &observation, anchor)
}

func computerAuthorityObservation(nonce []byte, observation *agentv1.ComputerAuthorityObservation, anchor time.Time) (*computerAuthorityClock, error) {
	if time.Since(anchor) > 2*time.Second {
		return nil, errors.New("computer authority challenge round trip exceeded its bound")
	}
	if !bytes.Equal(observation.GetNonce(), nonce) || observation.GetAuthorityTimeUnixNano() <= 0 {
		return nil, errors.New("computer authority observation does not match this connection's challenge")
	}
	return &computerAuthorityClock{anchor: anchor, authority: observation.GetAuthorityTimeUnixNano()}, nil
}
