package secretproxy

import (
	"errors"
	"net"
	"net/netip"
	"syscall"
	"testing"
)

func TestDestinationIsNumericPinnedAndPolicyChecked(t *testing.T) {
	// DNS answers are guest input, never authority. The dial cannot resolve a
	// hostname or move from this checked address, including on a later redirect.
	for _, tc := range []struct {
		address string
		dial    bool
	}{
		{"93.184.216.34:8443", true}, {"synthetic.example:8443", false},
		{"127.0.0.1:443", false}, {"169.254.169.254:80", false}, {"10.0.0.1:443", false},
		{"93.184.216.35:443", false}, {"[::ffff:93.184.216.34]:443", false}, {"[::1]:443", false},
	} {
		t.Run(tc.address, func(t *testing.T) {
			called := false
			d := &Dialer{Blocked: []netip.Prefix{netip.MustParsePrefix("93.184.216.35/32")}, dialer: net.Dialer{Control: func(network, address string, _ syscall.RawConn) error {
				called = true
				if network != "tcp4" || address != tc.address {
					t.Errorf("destination changed %s %s", network, address)
				}
				return errors.New("synthetic stop before network")
			}}}
			if _, err := d.DialContext(t.Context(), "tcp4", tc.address); err == nil {
				t.Fatal("unexpected network dial")
			}
			if called != tc.dial {
				t.Fatalf("dial=%v want=%v", called, tc.dial)
			}
		})
	}
}
