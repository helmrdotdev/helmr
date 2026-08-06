package controlplane

import (
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

const (
	workerEnrollmentRateWindow     = time.Minute
	workerEnrollmentPerSourceLimit = 512
	workerEnrollmentGlobalLimit    = 1024
)

type workerEnrollmentRate struct {
	windowStart time.Time
	enrollments int
}

type workerEnrollmentGuard struct {
	mu      sync.Mutex
	sources map[string]workerEnrollmentRate
	global  workerEnrollmentRate
}

func newWorkerEnrollmentGuard() *workerEnrollmentGuard {
	return &workerEnrollmentGuard{
		sources: make(map[string]workerEnrollmentRate),
	}
}

func (g *workerEnrollmentGuard) allowEnrollment(source string, now time.Time) bool {
	g.mu.Lock()
	defer g.mu.Unlock()

	if g.global.windowStart.IsZero() || now.Sub(g.global.windowStart) >= workerEnrollmentRateWindow {
		g.global = workerEnrollmentRate{windowStart: now}
		for key, rate := range g.sources {
			if now.Sub(rate.windowStart) >= workerEnrollmentRateWindow {
				delete(g.sources, key)
			}
		}
	}
	rate := g.sources[source]
	if rate.windowStart.IsZero() || now.Sub(rate.windowStart) >= workerEnrollmentRateWindow {
		rate = workerEnrollmentRate{windowStart: now}
	}
	if rate.enrollments >= workerEnrollmentPerSourceLimit || g.global.enrollments >= workerEnrollmentGlobalLimit {
		return false
	}
	rate.enrollments++
	g.global.enrollments++
	g.sources[source] = rate
	return true
}

func workerEnrollmentSource(request *http.Request) string {
	if forwarded := request.Header.Values("X-Forwarded-For"); len(forwarded) > 0 {
		parts := strings.Split(forwarded[len(forwarded)-1], ",")
		if candidate := strings.TrimSpace(parts[len(parts)-1]); net.ParseIP(candidate) != nil {
			return candidate
		}
	}
	host, _, err := net.SplitHostPort(request.RemoteAddr)
	if err == nil && net.ParseIP(host) != nil {
		return host
	}
	return "unknown"
}
