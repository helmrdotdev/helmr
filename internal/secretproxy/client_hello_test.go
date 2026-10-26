package secretproxy

import (
	"bytes"
	"encoding/binary"
	"testing"
)

func testHello(host string, ech bool) []byte {
	body := make([]byte, 34)
	body = append(body, 0, 0, 2, 0x13, 1, 1, 0)
	name := []byte(host)
	sni := []byte{0, byte(len(name) + 3), 0, 0, byte(len(name))}
	sni = append(sni, name...)
	extensions := []byte{0, 0, 0, byte(len(sni))}
	extensions = append(extensions, sni...)
	if ech {
		extensions = append(extensions, 0xfe, 0x0d, 0, 0)
	}
	body = append(body, byte(len(extensions)>>8), byte(len(extensions)))
	body = append(body, extensions...)
	return append([]byte{1, byte(len(body) >> 16), byte(len(body) >> 8), byte(len(body))}, body...)
}
func record(body []byte) []byte {
	v := []byte{22, 3, 3, 0, 0}
	binary.BigEndian.PutUint16(v[3:], uint16(len(body)))
	return append(v, body...)
}
func TestClientHelloFragmentationAndRejection(t *testing.T) {
	hello := testHello("api.example.com", false)
	for split := 1; split < len(hello); split++ {
		raw := append(record(hello[:split]), record(hello[split:])...)
		host, replay, err := inspectClientHello(bytes.NewReader(raw))
		if err != nil || host != "api.example.com" || !bytes.Equal(replay, raw) {
			t.Fatalf("split %d: %q %v", split, host, err)
		}
	}
	for _, raw := range [][]byte{record(testHello("api.example.com", true)), record(testHello("", false)), record(testHello("127.0.0.1", false)), []byte("GET / HTTP/1.1\r\n"), record([]byte{1, 255, 255, 255})} {
		if _, _, err := inspectClientHello(bytes.NewReader(raw)); err == nil {
			t.Fatal("accepted invalid/ECH/absent SNI")
		}
	}
}
