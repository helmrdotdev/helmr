package control

import (
	"context"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

const (
	workerEnrollmentRateWindow      = time.Minute
	workerChallengePerSourceLimit   = 256
	workerEnrollmentPerSourceLimit  = 512
	workerChallengeGlobalLimit      = 512
	workerEnrollmentGlobalLimit     = 1024
	workerEnrollmentVerificationMax = 16
)

type workerEnrollmentRate struct {
	windowStart time.Time
	challenges  int
	enrollments int
}

type workerEnrollmentGuard struct {
	mu      sync.Mutex
	sources map[string]workerEnrollmentRate
	global  workerEnrollmentRate
	verify  chan struct{}
}

func newWorkerEnrollmentGuard() *workerEnrollmentGuard {
	return &workerEnrollmentGuard{
		sources: make(map[string]workerEnrollmentRate),
		verify:  make(chan struct{}, workerEnrollmentVerificationMax),
	}
}

func (g *workerEnrollmentGuard) allowChallenge(source string, now time.Time) bool {
	return g.allow(source, now, true)
}

func (g *workerEnrollmentGuard) allowEnrollment(source string, now time.Time) bool {
	return g.allow(source, now, false)
}

func (g *workerEnrollmentGuard) allow(source string, now time.Time, challenge bool) bool {
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
	if challenge {
		if rate.challenges >= workerChallengePerSourceLimit || g.global.challenges >= workerChallengeGlobalLimit {
			return false
		}
		rate.challenges++
		g.global.challenges++
	} else {
		if rate.enrollments >= workerEnrollmentPerSourceLimit || g.global.enrollments >= workerEnrollmentGlobalLimit {
			return false
		}
		rate.enrollments++
		g.global.enrollments++
	}
	g.sources[source] = rate
	return true
}

func (g *workerEnrollmentGuard) beginVerification(ctx context.Context) bool {
	select {
	case g.verify <- struct{}{}:
		return true
	case <-ctx.Done():
		return false
	}
}

func (g *workerEnrollmentGuard) endVerification() {
	<-g.verify
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
