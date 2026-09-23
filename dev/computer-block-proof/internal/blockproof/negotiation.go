package blockproof

import (
	"encoding/binary"
	"errors"
	"io"
)

const optionMagic uint64 = 0x49484156454f5054
const replyMagic uint64 = 0x3e889045565a9

// Serve negotiates a single unnamed export using fixed-newstyle EXPORT_NAME.
// GO and other options are rejected with ERR_UNSUP; a client may fall back to
// EXPORT_NAME. TLS, structured replies and multiple connections are not supported.
func Serve(rw io.ReadWriter, d device) error {
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
			binary.BigEndian.PutUint64(export[:8], uint64(d.Size()))
			features := uint16(1 | 32) // HAS_FLAGS | SEND_TRIM
			if _, ok := d.(flusher); ok {
				features |= 4
			} // SEND_FLUSH, never SEND_FUA
			binary.BigEndian.PutUint16(export[8:], features)
			if err := writeAll(rw, export[:]); err != nil {
				return err
			}
			if flags&2 == 0 {
				if err := writeAll(rw, make([]byte, 124)); err != nil {
					return err
				}
			}
			return ServeTransmission(rw, d)
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
