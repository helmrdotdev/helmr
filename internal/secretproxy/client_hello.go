package secretproxy

import (
	"encoding/binary"
	"errors"
	"io"

	"github.com/helmrdotdev/helmr/internal/networkpolicy"
)

// inspectClientHello retains the initial records verbatim for end-to-end TLS.
// It refuses ECH before resolving or opening any upstream socket.
func inspectClientHello(reader io.Reader) (host string, records []byte, err error) {
	var handshake []byte
	for len(records) < maxClientHelloBytes {
		header := make([]byte, 5)
		if _, err = io.ReadFull(reader, header); err != nil {
			return
		}
		if header[0] != 22 || header[1] != 3 || header[2] > 3 {
			err = errors.New("TLS ClientHello required")
			return
		}
		size := int(binary.BigEndian.Uint16(header[3:]))
		if size == 0 || size > 16384 || len(records)+5+size > maxClientHelloBytes {
			err = errors.New("TLS record exceeds inspection bound")
			return
		}
		body := make([]byte, size)
		if _, err = io.ReadFull(reader, body); err != nil {
			return
		}
		records = append(records, header...)
		records = append(records, body...)
		handshake = append(handshake, body...)
		if len(handshake) < 4 {
			continue
		}
		length := int(handshake[1])<<16 | int(handshake[2])<<8 | int(handshake[3])
		if handshake[0] != 1 || length > maxClientHelloBytes-4 {
			err = errors.New("invalid TLS ClientHello")
			return
		}
		if len(handshake) < length+4 {
			continue
		}
		host, err = parseClientHello(handshake[4 : 4+length])
		return
	}
	return "", records, errors.New("TLS ClientHello exceeds inspection bound")
}
func parseClientHello(body []byte) (string, error) {
	bad := errors.New("invalid TLS ClientHello")
	take := func(n int) []byte {
		if n < 0 || len(body) < n {
			body = nil
			return nil
		}
		v := body[:n]
		body = body[n:]
		return v
	}
	read8 := func() int {
		v := take(1)
		if v == nil {
			return -1
		}
		return int(v[0])
	}
	read16 := func() int {
		v := take(2)
		if v == nil {
			return -1
		}
		return int(binary.BigEndian.Uint16(v))
	}
	if take(34) == nil {
		return "", bad
	}
	session := read8()
	if session < 0 || session > 32 || take(session) == nil {
		return "", bad
	}
	ciphers := read16()
	if ciphers < 2 || ciphers%2 != 0 || take(ciphers) == nil {
		return "", bad
	}
	compression := read8()
	if compression < 1 || take(compression) == nil {
		return "", bad
	}
	extensions := read16()
	if extensions < 0 || extensions != len(body) {
		return "", bad
	}
	seen := map[int]bool{}
	host := ""
	for len(body) > 0 {
		kind := read16()
		length := read16()
		value := take(length)
		if kind < 0 || length < 0 || value == nil || seen[kind] {
			return "", bad
		}
		seen[kind] = true
		if kind == 0xfe0d {
			return "", errors.New("encrypted ClientHello is unsupported")
		}
		if kind != 0 {
			continue
		}
		if len(value) < 5 || int(binary.BigEndian.Uint16(value)) != len(value)-2 || value[2] != 0 || int(binary.BigEndian.Uint16(value[3:])) != len(value)-5 {
			return "", bad
		}
		raw := string(value[5:])
		normalized, err := networkpolicy.NormalizePattern(raw)
		if err != nil || normalized != raw || len(raw) > 0 && raw[0] == '*' {
			return "", bad
		}
		host = normalized
	}
	if host == "" {
		return "", errors.New("TLS SNI required")
	}
	return host, nil
}
