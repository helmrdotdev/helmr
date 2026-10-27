package secretproxy

import (
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"net/netip"
	"sync"
	"time"

	"github.com/helmrdotdev/helmr/internal/networkpolicy"
	"golang.org/x/net/dns/dnsmessage"
)

// DNS serves a single allocation. Its sockets are created inside the allocation
// namespace; trusted resolution runs on host threads, outside guest routing.
type DNS struct {
	decisions    *DecisionLog
	policy       networkpolicy.Policy
	installation networkpolicy.Installation
	lookup       func(context.Context, string, string) ([]netip.Addr, error)
	udp          net.PacketConn
	tcp          net.Listener
	ctx          context.Context
	cancel       context.CancelFunc
	workers      sync.WaitGroup
	limit        chan struct{}
	mu           sync.Mutex
	connections  map[net.Conn]bool
}

func NewDNS(policy networkpolicy.Policy, in networkpolicy.Installation, lookup func(context.Context, string, string) ([]netip.Addr, error), udp net.PacketConn, tcp net.Listener, decisions *DecisionLog) (*DNS, error) {
	if policy.Kind != networkpolicy.Allowlist || lookup == nil || udp == nil || tcp == nil {
		return nil, errors.New("selective DNS requires allow policy and bound sockets")
	}
	if err := networkpolicy.ValidateInstallation(in); err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	return &DNS{decisions: decisions, policy: policy, installation: in, lookup: lookup, udp: udp, tcp: tcp, ctx: ctx, cancel: cancel, limit: make(chan struct{}, 64), connections: map[net.Conn]bool{}}, nil
}
func (d *DNS) Reply(ctx context.Context, packet []byte) []byte {
	var parser dnsmessage.Parser
	header, err := parser.Start(packet)
	if err != nil || header.Response || header.OpCode != 0 || len(packet) > 4096 {
		d.decisions.record(ctx, "dns", "", netip.Addr{}, 53, false, "malformed_query")
		return nil
	}
	question, err := parser.Question()
	if err != nil {
		d.decisions.record(ctx, "dns", "", netip.Addr{}, 53, false, "malformed_query")
		return nil
	}
	if _, err := parser.Question(); err != dnsmessage.ErrSectionDone {
		d.decisions.record(ctx, "dns", "", netip.Addr{}, 53, false, "malformed_query")
		return nil
	}
	response := dnsmessage.Header{ID: header.ID, Response: true, RecursionDesired: header.RecursionDesired, RecursionAvailable: true}
	hostname := question.Name.String()
	hostname = hostname[:len(hostname)-1]
	allowed := question.Class == dnsmessage.ClassINET && d.policy.AllowsHostname(hostname)
	if !allowed || question.Type != dnsmessage.TypeA && question.Type != dnsmessage.TypeAAAA {
		response.RCode = dnsmessage.RCodeRefused
		d.decisions.record(ctx, "dns", hostname, netip.Addr{}, 53, false, "query_denied")
	}
	if response.RCode == 0 && question.Type == dnsmessage.TypeAAAA {
		d.decisions.record(ctx, "dns", hostname, netip.Addr{}, 53, true, "aaaa_nodata")
	}
	builder := dnsmessage.NewBuilder(nil, response)
	builder.EnableCompression()
	_ = builder.StartQuestions()
	_ = builder.Question(question)
	_ = builder.StartAnswers()
	if response.RCode == 0 && question.Type == dnsmessage.TypeA {
		lookupCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		addresses, e := d.lookup(lookupCtx, "ip4", hostname+".")
		cancel()
		if e != nil {
			d.decisions.record(ctx, "dns", hostname, netip.Addr{}, 53, false, "resolution_failed")
			return dnsError(header, question, dnsmessage.RCodeServerFailure)
		}
		answerCount := 0
		for _, ip := range addresses {
			if answerCount == 12 {
				break
			}
			if !d.policy.AllowsAddress(hostname, ip, d.installation) {
				d.decisions.record(ctx, "dns", hostname, ip, 53, false, "address_denied")
				continue
			}
			d.decisions.record(ctx, "dns", hostname, ip, 53, true, "address_allowed")
			answerCount++
			_ = builder.AResource(dnsmessage.ResourceHeader{Name: question.Name, Type: dnsmessage.TypeA, Class: dnsmessage.ClassINET, TTL: 30}, dnsmessage.AResource{A: ip.As4()})
		}
		if answerCount == 0 {
			d.decisions.record(ctx, "dns", hostname, netip.Addr{}, 53, false, "no_allowed_address")
		}
	}
	raw, err := builder.Finish()
	if err != nil || len(raw) > 4096 {
		return nil
	}
	return raw
}
func dnsError(header dnsmessage.Header, q dnsmessage.Question, code dnsmessage.RCode) []byte {
	b := dnsmessage.NewBuilder(nil, dnsmessage.Header{ID: header.ID, Response: true, RecursionAvailable: true, RecursionDesired: header.RecursionDesired, RCode: code})
	_ = b.StartQuestions()
	_ = b.Question(q)
	raw, _ := b.Finish()
	return raw
}
func (d *DNS) Serve() {
	d.workers.Add(2)
	go func() {
		defer d.workers.Done()
		buffer := make([]byte, 4097)
		for {
			n, peer, err := d.udp.ReadFrom(buffer)
			if err != nil {
				return
			}
			select {
			case d.limit <- struct{}{}:
			default:
				continue
			}
			packet := append([]byte{}, buffer[:n]...)
			d.workers.Add(1)
			go func() {
				defer d.workers.Done()
				defer func() { <-d.limit }()
				if reply := d.Reply(d.ctx, packet); reply != nil {
					_, _ = d.udp.WriteTo(reply, peer)
				}
			}()
		}
	}()
	go func() {
		defer d.workers.Done()
		for {
			conn, err := d.tcp.Accept()
			if err != nil {
				return
			}
			select {
			case d.limit <- struct{}{}:
			default:
				conn.Close()
				continue
			}
			d.mu.Lock()
			if d.ctx.Err() != nil {
				d.mu.Unlock()
				conn.Close()
				<-d.limit
				return
			}
			d.connections[conn] = true
			d.workers.Add(1)
			d.mu.Unlock()
			go func() {
				defer d.workers.Done()
				defer func() { conn.Close(); d.mu.Lock(); delete(d.connections, conn); d.mu.Unlock(); <-d.limit }()
				_ = conn.SetDeadline(time.Now().Add(5 * time.Second))
				var size [2]byte
				if _, err := io.ReadFull(conn, size[:]); err != nil {
					return
				}
				n := int(binary.BigEndian.Uint16(size[:]))
				if n > 4096 {
					return
				}
				packet := make([]byte, n)
				if _, err := io.ReadFull(conn, packet); err != nil {
					return
				}
				reply := d.Reply(d.ctx, packet)
				if reply == nil {
					return
				}
				binary.BigEndian.PutUint16(size[:], uint16(len(reply)))
				_, _ = conn.Write(append(size[:], reply...))
			}()
		}
	}()
}
func (d *DNS) Close() error {
	d.cancel()
	_ = d.udp.Close()
	_ = d.tcp.Close()
	d.mu.Lock()
	for conn := range d.connections {
		_ = conn.Close()
	}
	d.mu.Unlock()
	d.workers.Wait()
	return nil
}
