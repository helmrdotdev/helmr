// Package origin defines the exact HTTPS origin vocabulary used by Secret bindings.
package origin

import (
	"errors"
	"net"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
)

func Canonical(raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Opaque != "" || u.User != nil ||
		u.Host == "" || (u.Path != "" && u.Path != "/") || u.RawPath != "" ||
		u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || strings.Contains(raw, "#") ||
		strings.TrimSpace(raw) != raw {
		return "", errors.New("secret origin must be an exact HTTPS origin without credentials, path, query or fragment")
	}
	host := strings.ToLower(u.Hostname())
	if !ValidHostname(host) || host == "localhost" || strings.HasSuffix(host, ".localhost") {
		return "", errors.New("secret origin must use a DNS hostname, without wildcards or IP addresses")
	}
	port := u.Port()
	if strings.HasSuffix(u.Host, ":") {
		return "", errors.New("secret origin port is invalid")
	}
	if port != "" {
		n, err := strconv.Atoi(port)
		if err != nil || n < 1 || n > 65535 || strconv.Itoa(n) != port {
			return "", errors.New("secret origin port is invalid")
		}
		if port != "443" {
			host = net.JoinHostPort(host, port)
		}
	}
	return "https://" + host, nil
}

func ValidHostname(host string) bool {
	if len(host) == 0 || len(host) > 253 {
		return false
	}
	if strings.Trim(host, "0123456789.") == "" {
		return false
	}
	if _, err := netip.ParseAddr(host); err == nil {
		return false
	}
	for _, label := range strings.Split(host, ".") {
		if len(label) == 0 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for _, c := range label {
			if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-') {
				return false
			}
		}
	}
	return true
}
