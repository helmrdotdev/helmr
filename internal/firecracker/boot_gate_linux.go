//go:build linux

package firecracker

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"io"
	"time"
	"uuid"

	"github.com/helmrdotdev/helmr/internal/bootgate"
)

type bootstrapReceipt struct {
	BootID string
	Random [32]byte
}

func (c *Connector) bootstrapGuest(ctx context.Context, path string, exited *machineExit, probe bool) (bootstrapReceipt, error) {
	var receipt bootstrapReceipt
	ctx, cancel := context.WithTimeout(ctx, c.cfg.HealthTimeout)
	defer cancel()
	conn, err := c.connectGuestPortAt(ctx, path, bootgate.Port, exited)
	if err != nil {
		return receipt, err
	}
	defer conn.Close()
	closed := make(chan struct{})
	stop := context.AfterFunc(ctx, func() { defer close(closed); _ = conn.Close() })
	defer func() {
		if !stop() {
			<-closed
		}
	}()
	if probe {
		if _, err := conn.Write([]byte{bootgate.Probe}); err != nil {
			return receipt, err
		}
		raw, err := io.ReadAll(io.LimitReader(conn, int64(len(bootgate.Protocol)+1)))
		if err != nil {
			return receipt, err
		}
		if string(raw) != bootgate.Protocol {
			return receipt, errors.New("guest bootstrap protocol differs")
		}
		return receipt, nil
	}
	request := bootgate.Request{Time: time.Now()}
	if _, err := rand.Read(request.Entropy[:]); err != nil {
		return receipt, err
	}
	defer clear(request.Entropy[:])
	if _, err := rand.Read(request.Challenge[:]); err != nil {
		return receipt, err
	}
	raw := append([]byte{bootgate.Resume}, request.Encode()...)
	defer clear(raw)
	if _, err := conn.Write(raw); err != nil {
		return receipt, err
	}
	var response [bootgate.ReceiptBytes]byte
	if _, err := io.ReadFull(conn, response[:]); err != nil {
		return receipt, err
	}
	if !bytes.Equal(response[:bootgate.ChallengeBytes], request.Challenge[:]) {
		return receipt, errors.New("guest bootstrap challenge differs")
	}
	receipt.BootID = string(response[bootgate.ChallengeBytes : bootgate.ChallengeBytes+36])
	if _, err := uuid.Parse(receipt.BootID); err != nil {
		return receipt, errors.New("guest bootstrap identity is invalid")
	}
	copy(receipt.Random[:], response[bootgate.ChallengeBytes+36:])
	if receipt.Random == [32]byte{} {
		return receipt, errors.New("guest bootstrap randomness is empty")
	}
	return receipt, nil
}
