package server

import (
	"log"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// limiter is a token bucket per client address. It slows down guessing and
// floods on the endpoints anyone on the internet can call (sign-in, invite
// links, the setup token).
type limiter struct {
	mu    sync.Mutex
	rate  float64 // tokens per second
	burst float64
	max   int // addresses tracked at once
	m     map[string]*bucket
	now   func() time.Time
}

type bucket struct {
	tokens float64
	last   time.Time
}

func newLimiter(perMinute, burst float64, maxKeys int) *limiter {
	return &limiter{rate: perMinute / 60, burst: burst, max: maxKeys, m: map[string]*bucket{}, now: time.Now}
}

// overflowKey shares one bucket among addresses that arrive while the table
// is full, so spraying addresses can't grow memory or reset limits.
const overflowKey = "\x00overflow"

// take spends cost tokens for key. It reports whether the request may go
// ahead, and if not, how long until it may.
func (l *limiter) take(key string, cost float64) (bool, time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	t := l.now()
	b, ok := l.m[key]
	if !ok {
		if len(l.m) >= l.max {
			l.prune(t)
		}
		if len(l.m) >= l.max {
			key = overflowKey
			b = l.m[key]
		}
		if b == nil {
			b = &bucket{tokens: l.burst, last: t}
			l.m[key] = b
		}
	}
	b.tokens = min(l.burst, b.tokens+t.Sub(b.last).Seconds()*l.rate)
	b.last = t
	if b.tokens < cost {
		// Failures keep draining, so hammering only makes the wait longer.
		b.tokens = max(b.tokens-cost, -l.burst)
		wait := time.Duration((cost - b.tokens) / l.rate * float64(time.Second))
		return false, wait
	}
	b.tokens -= cost
	return true, 0
}

// prune drops addresses whose bucket has refilled (they are idle).
func (l *limiter) prune(t time.Time) {
	for k, b := range l.m {
		if b.tokens+t.Sub(b.last).Seconds()*l.rate >= l.burst {
			delete(l.m, k)
		}
	}
}

// limitKey groups IPv6 clients by /64: one host usually owns a whole /64.
func limitKey(ip string) string {
	a := net.ParseIP(ip)
	if a == nil {
		return ip
	}
	if a.To4() == nil {
		return a.Mask(net.CIDRMask(64, 128)).String() + "/64"
	}
	return a.String()
}

func tooMany(w http.ResponseWriter, wait time.Duration) {
	secs := int(wait.Seconds()) + 1
	w.Header().Set("Retry-After", strconv.Itoa(secs))
	httpError(w, http.StatusTooManyRequests, "too many attempts; try again in "+strconv.Itoa(secs)+" seconds")
}

// clientIP is the address the request came from. Behind a reverse proxy on
// this machine (tailscale serve, Caddy, nginx, cloudflared …) that is the
// last X-Forwarded-For entry, the one the proxy itself added.
func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	if isLoopback(r.RemoteAddr) {
		if xff := r.Header.Values("X-Forwarded-For"); len(xff) > 0 {
			parts := strings.Split(xff[len(xff)-1], ",")
			if ip := net.ParseIP(strings.TrimSpace(parts[len(parts)-1])); ip != nil {
				return ip.String()
			}
		}
	}
	return host
}

// authLimited lists the public sign-in endpoints that are rate limited.
// (/api/auth/status is fetched on every page load and reveals nothing.)
var authLimited = map[string]bool{
	"/api/auth/login/begin":   true,
	"/api/auth/login/finish":  true,
	"/api/auth/invite":        true,
	"/api/auth/invite/begin":  true,
	"/api/auth/invite/finish": true,
	"/api/login":              true,
	"/api/auth/verify/begin":  true,
	"/api/auth/verify/finish": true,
}

// failCost is what a failed attempt costs on top of the request itself.
const failCost = 4

// authFailed makes a failed sign-in, token or link attempt count extra
// against the client's budget, and logs it (for fail2ban and the like).
func (s *Server) authFailed(r *http.Request, what string, err error) {
	ip := clientIP(r)
	s.limit.take(limitKey(ip), failCost)
	// The reason may echo what the client sent: quoted, it can't forge
	// log lines (and get someone else's address banned).
	why := ""
	if err != nil {
		why = " " + strconv.Quote(err.Error())
	}
	log.Printf("auth: failed %s from %s%s", what, ip, why)
}
