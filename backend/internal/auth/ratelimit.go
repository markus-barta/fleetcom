package auth

import (
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

const (
	rateLimitWindow      = 10 * time.Minute
	rateLimitMaxAttempts = 5
)

type rateLimitEntry struct {
	Count       int
	WindowStart time.Time
	LastSeen    time.Time
}

type rateLimiter struct {
	mu      sync.Mutex
	entries map[string]rateLimitEntry
}

var limiter = &rateLimiter{entries: make(map[string]rateLimitEntry)}

// AllowAttempt returns true if the attempt is allowed, along with the retry-after duration.
func AllowAttempt(scope string, r *http.Request, identity string) (bool, time.Duration) {
	limiter.mu.Lock()
	defer limiter.mu.Unlock()

	limiter.prune()

	now := time.Now()
	keys := buildKeys(scope, r, identity)

	for _, key := range keys {
		e, ok := limiter.entries[key]
		if !ok {
			continue
		}
		if now.Sub(e.WindowStart) > rateLimitWindow {
			delete(limiter.entries, key)
			continue
		}
		if e.Count >= rateLimitMaxAttempts {
			retryAfter := rateLimitWindow - now.Sub(e.WindowStart)
			return false, retryAfter
		}
	}
	return true, 0
}

// RecordFailure increments the failure counter for the given scope.
func RecordFailure(scope string, r *http.Request, identity string) {
	limiter.mu.Lock()
	defer limiter.mu.Unlock()

	now := time.Now()
	keys := buildKeys(scope, r, identity)

	for _, key := range keys {
		e, ok := limiter.entries[key]
		if !ok || now.Sub(e.WindowStart) > rateLimitWindow {
			limiter.entries[key] = rateLimitEntry{Count: 1, WindowStart: now, LastSeen: now}
		} else {
			e.Count++
			e.LastSeen = now
			limiter.entries[key] = e
		}
	}
}

// ResetFailures clears failure counters after a successful attempt.
func ResetFailures(scope string, r *http.Request, identity string) {
	limiter.mu.Lock()
	defer limiter.mu.Unlock()

	for _, key := range buildKeys(scope, r, identity) {
		delete(limiter.entries, key)
	}
}

func buildKeys(scope string, r *http.Request, identity string) []string {
	keys := []string{scope + ":ip:" + ClientIP(r)}
	if identity != "" {
		keys = append(keys, scope+":id:"+strings.ToLower(identity))
	}
	return keys
}

func (rl *rateLimiter) prune() {
	cutoff := time.Now().Add(-2 * rateLimitWindow)
	for key, e := range rl.entries {
		if e.LastSeen.Before(cutoff) {
			delete(rl.entries, key)
		}
	}
}

// ClientIP extracts the client IP, preferring X-Real-IP (set by chi middleware.RealIP).
func ClientIP(r *http.Request) string {
	if ip := r.Header.Get("X-Real-IP"); ip != "" {
		return ip
	}
	if ip := r.Header.Get("X-Forwarded-For"); ip != "" {
		return strings.Split(ip, ",")[0]
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// SetRetryAfter sets the Retry-After header on the response.
func SetRetryAfter(w http.ResponseWriter, d time.Duration) {
	secs := int(d.Seconds()) + 1
	if secs < 1 {
		secs = 1
	}
	w.Header().Set("Retry-After", http.TimeFormat)
	http.Error(w, "too many attempts, try again later", http.StatusTooManyRequests)
}
