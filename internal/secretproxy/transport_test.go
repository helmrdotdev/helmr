package secretproxy

import (
	"context"
	"errors"
	"golang.org/x/net/dns/dnsmessage"
	"net"
	"net/netip"
	"sync/atomic"
	"syscall"
	"testing"
)

func TestDNSChecksAllAAndPinsNumericIPv4(t *testing.T) {
	for _, tc := range []struct {
		name    string
		answers [][4]byte
		blocked bool
	}{
		{"public", [][4]byte{{93, 184, 216, 34}}, false},
		{"mixed private", [][4]byte{{93, 184, 216, 34}, {127, 0, 0, 1}}, true},
		{"worker configured", [][4]byte{{93, 184, 216, 35}}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			socket, err := net.ListenPacket("udp4", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			defer socket.Close()
			var queries atomic.Int32
			go func() {
				buf := make([]byte, 4096)
				for {
					n, peer, e := socket.ReadFrom(buf)
					if e != nil {
						return
					}
					var message dnsmessage.Message
					if e := message.Unpack(buf[:n]); e != nil {
						return
					}
					queries.Add(1)
					message.Header.Response = true
					message.Header.RecursionAvailable = true
					for _, question := range message.Questions {
						if question.Type != dnsmessage.TypeA {
							t.Error("upstream attempted non-IPv4 DNS")
							continue
						}
						for _, address := range tc.answers {
							message.Answers = append(message.Answers, dnsmessage.Resource{Header: dnsmessage.ResourceHeader{Name: question.Name, Type: dnsmessage.TypeA, Class: dnsmessage.ClassINET, TTL: 1}, Body: &dnsmessage.AResource{A: address}})
						}
					}
					packed, e := message.Pack()
					if e == nil {
						socket.WriteTo(packed, peer)
					}
				}
			}()
			resolver := &net.Resolver{PreferGo: true, Dial: func(ctx context.Context, network, address string) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, "udp4", socket.LocalAddr().String())
			}}
			var dials atomic.Int32
			dialer := &Dialer{Blocked: []netip.Prefix{netip.MustParsePrefix("93.184.216.35/32")}, resolver: resolver, dialer: net.Dialer{Control: func(network, address string, _ syscall.RawConn) error {
				dials.Add(1)
				if network != "tcp4" || address != "93.184.216.34:443" {
					t.Errorf("unpinned dial %s %s", network, address)
				}
				return errors.New("synthetic stop before network")
			}}}
			if _, err := dialer.DialContext(t.Context(), "tcp", "synthetic.example:443"); err == nil {
				t.Fatal("fixture unexpectedly connected")
			}
			if queries.Load() != 1 {
				t.Fatalf("DNS requests=%d", queries.Load())
			}
			if tc.blocked && dials.Load() != 0 || !tc.blocked && dials.Load() != 1 {
				t.Fatalf("dial count=%d", dials.Load())
			}
		})
	}
}
