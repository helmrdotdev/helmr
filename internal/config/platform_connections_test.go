package config

import "testing"

func TestPlatformConnections(t *testing.T) {
	for _, mode := range []string{"disabled", "http", "socket"} {
		if err := validatePlatformConnections(mode, "http://proxy.internal:3128"); err != nil {
			t.Fatal(err)
		}
	}
	for _, mode := range []string{"", "private", "auto", "HTTP"} {
		if err := validatePlatformConnections(mode, ""); err == nil {
			t.Fatalf("accepted mode %q", mode)
		}
	}
	if err := validatePlatformConnections("socket", "socks5://proxy"); err == nil {
		t.Fatal("accepted unsupported proxy")
	}
}
