package httpapi

import (
	"encoding/json"
	"log/slog"
	"net"
	"net/http"
	"runtime/debug"
	"strings"
	"sync"
	"time"
)

type apiErrorBody struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

type apiErrorEnvelope struct {
	Error apiErrorBody `json:"error"`
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}

func writeError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, apiErrorEnvelope{
		Error: apiErrorBody{
			Code:    code,
			Message: message,
		},
	})
}

// SecurityHeadersMiddleware adds baseline hardening headers.
func SecurityHeadersMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		next.ServeHTTP(w, r)
	})
}

// RecoverMiddleware catches handler panics, logs stack traces, and returns 500 JSON.
func RecoverMiddleware(logger *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer func() {
				if rec := recover(); rec != nil {
					logger.Error("http_panic",
						"panic", rec,
						"path", r.URL.Path,
						"stack", string(debug.Stack()),
					)
					writeError(w, http.StatusInternalServerError, "internal_error", "An internal server error occurred.")
				}
			}()
			next.ServeHTTP(w, r)
		})
	}
}

// CORSMiddleware enforces strict origin allowlisting for Steam, Firefox extensions,
// and explicitly configured VAPOR_ALLOWED_ORIGINS.
func CORSMiddleware(extraOrigins []string) func(http.Handler) http.Handler {
	allowed := make(map[string]struct{}, len(extraOrigins)+1)
	allowed["https://store.steampowered.com"] = struct{}{}
	for _, o := range extraOrigins {
		o = strings.TrimSpace(strings.TrimRight(o, "/"))
		if o != "" {
			allowed[o] = struct{}{}
		}
	}

	isAllowed := func(origin string) bool {
		if origin == "" {
			return false
		}
		if strings.HasPrefix(origin, "moz-extension://") {
			return true
		}
		_, ok := allowed[origin]
		return ok
	}

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			origin := strings.TrimSpace(r.Header.Get("Origin"))
			w.Header().Add("Vary", "Origin")

			if origin != "" && isAllowed(origin) {
				w.Header().Set("Access-Control-Allow-Origin", origin)
				w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
				w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
			}

			if r.Method == http.MethodOptions {
				if origin == "" || !isAllowed(origin) {
					w.WriteHeader(http.StatusForbidden)
					return
				}
				w.WriteHeader(http.StatusNoContent)
				return
			}

			next.ServeHTTP(w, r)
		})
	}
}

type tokenBucket struct {
	tokens   float64
	lastSeen time.Time
}

// RateLimiter implements a lock-safe per-IP token bucket limiter with background
// stale-bucket eviction.
type RateLimiter struct {
	mu         sync.Mutex
	buckets    map[string]*tokenBucket
	ratePerSec float64
	burst      float64
	trustProxy bool
	stopCh     chan struct{}
	stopOnce   sync.Once
}

// NewRateLimiter creates a per-IP token bucket limiter.
func NewRateLimiter(rpm, burst int, trustProxy bool) *RateLimiter {
	if rpm <= 0 {
		rpm = 60
	}
	if burst <= 0 {
		burst = 20
	}
	rl := &RateLimiter{
		buckets:    make(map[string]*tokenBucket),
		ratePerSec: float64(rpm) / 60.0,
		burst:      float64(burst),
		trustProxy: trustProxy,
		stopCh:     make(chan struct{}),
	}
	go rl.cleanupLoop()
	return rl
}

func (rl *RateLimiter) cleanupLoop() {
	ticker := time.NewTicker(2 * time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			cutoff := time.Now().Add(-10 * time.Minute)
			rl.mu.Lock()
			for ip, b := range rl.buckets {
				if b.lastSeen.Before(cutoff) {
					delete(rl.buckets, ip)
				}
			}
			rl.mu.Unlock()
		case <-rl.stopCh:
			return
		}
	}
}

// Close stops the background eviction goroutine.
func (rl *RateLimiter) Close() {
	rl.stopOnce.Do(func() {
		close(rl.stopCh)
	})
}

// Allow checks whether a request from ip has a token available.
func (rl *RateLimiter) Allow(ip string) bool {
	now := time.Now()
	rl.mu.Lock()
	defer rl.mu.Unlock()

	b, ok := rl.buckets[ip]
	if !ok {
		rl.buckets[ip] = &tokenBucket{
			tokens:   rl.burst - 1.0,
			lastSeen: now,
		}
		return true
	}

	elapsed := now.Sub(b.lastSeen).Seconds()
	if elapsed > 0 {
		b.tokens += elapsed * rl.ratePerSec
		if b.tokens > rl.burst {
			b.tokens = rl.burst
		}
	}
	b.lastSeen = now

	if b.tokens < 1.0 {
		return false
	}
	b.tokens -= 1.0
	return true
}

// Middleware wraps an http.Handler with per-IP rate limiting.
func (rl *RateLimiter) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ip := rl.clientIP(r)
		if !rl.Allow(ip) {
			writeError(w, http.StatusTooManyRequests, "rate_limited", "Too many requests; please slow down.")
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (rl *RateLimiter) clientIP(r *http.Request) string {
	if rl.trustProxy {
		if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
			parts := strings.Split(xff, ",")
			if len(parts) > 0 {
				candidate := strings.TrimSpace(parts[0])
				if candidate != "" {
					return candidate
				}
			}
		}
		if xrip := strings.TrimSpace(r.Header.Get("X-Real-IP")); xrip != "" {
			return xrip
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err == nil && host != "" {
		return host
	}
	return r.RemoteAddr
}
