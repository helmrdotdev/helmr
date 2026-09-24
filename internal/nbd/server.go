package nbd

import (
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net"
)

const optionMagic uint64 = 0x49484156454f5054
const replyMagic uint64 = 0x3e889045565a9

// Serve owns conn through one fixed-newstyle negotiation and transmission.
// Cancellation closes the stream and interrupts blocked reads/writes; storage
// operations receive ctx. The caller joins this call before releasing the disk.
// One unnamed export supports FLUSH/TRIM, but no FUA or multi-connection claims.
func Serve(ctx context.Context, conn net.Conn, d BlockDevice, size int64) error {
	defer conn.Close()
	closed := make(chan struct{})
	stop := context.AfterFunc(ctx, func() { conn.Close(); close(closed) })
	defer func() {
		if !stop() {
			<-closed
		}
	}()
	if err := ctx.Err(); err != nil {
		return err
	}
	if size <= 0 {
		return errors.New("invalid export size")
	}
	return negotiate(ctx, conn, d, size)
}

// ServeOnce owns listener and accepts exactly one connection. It closes the
// listener on acceptance or cancellation and joins Serve before returning.
// Caller must create it in a private host directory before handing it to Claim.
func ServeOnce(ctx context.Context, listener net.Listener, d BlockDevice, size int64) error {
	defer listener.Close()
	closed := make(chan struct{})
	stop := context.AfterFunc(ctx, func() { listener.Close(); close(closed) })
	defer func() {
		if !stop() {
			<-closed
		}
	}()
	if err := ctx.Err(); err != nil {
		return err
	}
	conn, err := listener.Accept()
	if err != nil {
		return err
	}
	listener.Close()
	return Serve(ctx, conn, d, size)
}

func negotiate(ctx context.Context, rw io.ReadWriter, d BlockDevice, size int64) error {
	var greeting [18]byte
	binary.BigEndian.PutUint64(greeting[:8], 0x4e42444d41474943)
	binary.BigEndian.PutUint64(greeting[8:16], optionMagic)
	binary.BigEndian.PutUint16(greeting[16:], 3) // FIXED_NEWSTYLE | NO_ZEROES
	if err := writeAll(rw, greeting[:]); err != nil {
		return err
	}
	var cf [4]byte
	if _, err := io.ReadFull(rw, cf[:]); err != nil {
		return err
	}
	flags := binary.BigEndian.Uint32(cf[:])
	if flags&1 == 0 || flags & ^uint32(3) != 0 {
		return errors.New("unsupported client flags")
	}
	// Bound both individual payloads and total negotiation work.
	for attempt := 0; attempt < 16; attempt++ {
		var hdr [16]byte
		if _, err := io.ReadFull(rw, hdr[:]); err != nil {
			return err
		}
		if binary.BigEndian.Uint64(hdr[:8]) != optionMagic {
			return errors.New("invalid option magic")
		}
		option := binary.BigEndian.Uint32(hdr[8:12])
		n := binary.BigEndian.Uint32(hdr[12:])
		if n > 4096 {
			return errors.New("option too large")
		}
		payload := make([]byte, int(n))
		if _, err := io.ReadFull(rw, payload); err != nil {
			return err
		}
		switch option {
		case 1: // EXPORT_NAME has no option error reply; invalid names terminate.
			if len(payload) != 0 {
				return errors.New("unknown export")
			}
			var export [10]byte
			binary.BigEndian.PutUint64(export[:8], uint64(size))
			features := uint16(1 | 4 | 32) // HAS_FLAGS | SEND_FLUSH | SEND_TRIM
			binary.BigEndian.PutUint16(export[8:], features)
			if err := writeAll(rw, export[:]); err != nil {
				return err
			}
			if flags&2 == 0 {
				if err := writeAll(rw, make([]byte, 124)); err != nil {
					return err
				}
			}
			return ServeTransmission(ctx, rw, d, size)
		case 2:
			if n != 0 {
				return errors.New("invalid abort")
			}
			return optionReply(rw, option, 1)
		default:
			if err := optionReply(rw, option, 0x80000001); err != nil {
				return err
			}
		}
	}
	return errors.New("too many negotiation options")
}
func optionReply(w io.Writer, option, reply uint32) error {
	var b [20]byte
	binary.BigEndian.PutUint64(b[:8], replyMagic)
	binary.BigEndian.PutUint32(b[8:12], option)
	binary.BigEndian.PutUint32(b[12:16], reply)
	return writeAll(w, b[:])
}
