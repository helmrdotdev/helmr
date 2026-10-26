package secretproxy

import (
	"context"
	"log/slog"
	"math"
	"net"
	"net/netip"
	"strconv"
	"strings"
	"sync"

	"github.com/helmrdotdev/helmr/internal/networkpolicy"
	"golang.org/x/time/rate"
)

// DecisionLog binds safe destination diagnostics to one installed allocation.
// Session and Turn are unknown for shared/background traffic and stay null.
// Logging is bounded independently of guest traffic; it never queues records.
type DecisionLog struct {
	logger          *slog.Logger
	allowed         *rate.Limiter
	denied          *rate.Limiter
	mu              sync.Mutex
	suppressedAllow uint64
	suppressedDeny  uint64
}

func NewDecisionLog(logger *slog.Logger, computerID, allocationID, ownerKind, ownerID string, phase networkpolicy.Phase, policy networkpolicy.Policy) *DecisionLog {
	var computer any
	if phase == networkpolicy.PhaseRuntime && computerID != "" {
		computer = computerID
	}
	return &DecisionLog{logger: logger.With("computer_id", computer, "allocation_id", allocationID, "owner_kind", ownerKind, "owner_id", ownerID, "phase", string(phase), "policy_id", policy.Digest(), "session_id", nil, "turn_id", nil), allowed: rate.NewLimiter(10, 20), denied: rate.NewLimiter(10, 20)}
}

func (d *DecisionLog) record(ctx context.Context, protocol, host string, address netip.Addr, port uint16, allowed bool, reason string) {
	if d == nil {
		return
	}
	d.mu.Lock()
	limiter := d.denied
	suppressed := &d.suppressedDeny
	if allowed {
		limiter = d.allowed
		suppressed = &d.suppressedAllow
	}
	if !limiter.Allow() {
		if *suppressed < math.MaxUint64 {
			*suppressed += 1
		}
		d.mu.Unlock()
		return
	}
	suppressedAllow, suppressedDeny := d.suppressedAllow, d.suppressedDeny
	d.suppressedAllow, d.suppressedDeny = 0, 0
	d.mu.Unlock()
	// Never record an unparsed ClientHello, DNS payload, URL, error text or marker.
	safeHost, err := networkpolicy.NormalizePattern(host)
	if err != nil || strings.HasPrefix(safeHost, "*.") {
		safeHost = ""
	}
	destination := safeHost
	if address.IsValid() {
		destination = net.JoinHostPort(address.String(), strconv.Itoa(int(port)))
	} else if safeHost != "" {
		destination = net.JoinHostPort(safeHost, strconv.Itoa(int(port)))
	}
	decision := "deny"
	if allowed {
		decision = "allow"
	}
	d.logger.InfoContext(ctx, "Computer egress decision", "protocol", protocol, "hostname", safeHost, "destination", destination, "decision", decision, "reason", reason, "suppressed_allow", suppressedAllow, "suppressed_deny", suppressedDeny)
}

// BindDecisionLog runs before Serve, while the allocation owner still controls
// the prepared transport. Once serving, attribution cannot change.
func (p *Proxy) BindDecisionLog(decisions *DecisionLog) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.listener != nil || p.closed {
		return net.ErrClosed
	}
	p.config.Decisions = decisions
	return nil
}
