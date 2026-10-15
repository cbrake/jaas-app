package main

import (
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"
)

var slugPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{2,79}$`)

func validSlug(s string) bool { return slugPattern.MatchString(s) }

const (
	formBodyLimit    = 64 << 10
	webhookBodyLimit = 1 << 20
)

// Content-Security-Policy for pages that embed the JaaS iframe. Scripts are
// served from /static (no inline script), the only third-party script and
// frame origin is 8x8.vc.
const (
	cspMeeting = "default-src 'self'; script-src 'self' https://8x8.vc; frame-src https://8x8.vc; " +
		"connect-src 'self' https://8x8.vc; img-src 'self' data:; style-src 'self' 'unsafe-inline'; " +
		"base-uri 'none'; form-action 'self'; frame-ancestors 'none'; object-src 'none'"
	cspDefault = "default-src 'self'; script-src 'self'; img-src 'self' data:; style-src 'self' 'unsafe-inline'; " +
		"base-uri 'none'; form-action 'self'; frame-ancestors 'none'; object-src 'none'"
	permissionsMeeting = `camera=(self "https://8x8.vc"), microphone=(self "https://8x8.vc"), ` +
		`display-capture=(self "https://8x8.vc"), fullscreen=(self "https://8x8.vc"), ` +
		`autoplay=(self "https://8x8.vc"), clipboard-write=(self "https://8x8.vc")`
	permissionsDefault = "camera=(), microphone=(), geolocation=(), payment=(), usb=()"
)

func setSecurityHeaders(w http.ResponseWriter, meeting bool) {
	h := w.Header()
	if meeting {
		h.Set("Content-Security-Policy", cspMeeting)
		h.Set("Permissions-Policy", permissionsMeeting)
	} else {
		h.Set("Content-Security-Policy", cspDefault)
		h.Set("Permissions-Policy", permissionsDefault)
	}
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("X-Frame-Options", "DENY")
	// same-origin, not no-referrer: with no-referrer browsers send "Origin: null"
	// on same-site POSTs, which would defeat the Origin check.
	h.Set("Referrer-Policy", "same-origin")
	h.Set("Cross-Origin-Resource-Policy", "same-origin")
	// Pages carry CSRF tokens, JWTs or meeting data: never cache them.
	h.Set("Cache-Control", "no-store")
}

// clientIP trusts X-Real-IP only when the TCP peer is a configured proxy
// (a reverse proxy that sets it from the real client address).
func (app *App) clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	peer := net.ParseIP(host)
	if peer != nil {
		for _, n := range app.Config.TrustedProxies {
			if n.Contains(peer) {
				if xr := net.ParseIP(strings.TrimSpace(r.Header.Get("X-Real-IP"))); xr != nil {
					return xr.String()
				}
				break
			}
		}
	}
	return host
}

// sameOrigin enforces that a state-changing request was issued by a page of
// this site. Browsers always send Origin on POST; Referer is the fallback.
func (app *App) sameOrigin(r *http.Request) bool {
	if o := r.Header.Get("Origin"); o != "" {
		return o == app.Config.PublicOrigin
	}
	if ref := r.Header.Get("Referer"); ref != "" {
		u, err := url.Parse(ref)
		return err == nil && u.Scheme+"://"+u.Host == app.Config.PublicOrigin
	}
	return false
}

// statusRecorder captures the status code for the access log.
type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (s *statusRecorder) WriteHeader(code int) {
	s.status = code
	s.ResponseWriter.WriteHeader(code)
}

// withMiddleware wraps every route: panic recovery, body size limits, origin
// checks on POST and an access log that records only method, path, status and
// timing (never query strings, bodies or headers).
func (app *App) withMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		defer func() {
			if v := recover(); v != nil {
				slog.Error("panic", "path", r.URL.Path)
				http.Error(rec, "Internal Server Error", http.StatusInternalServerError)
			}
			slog.Info("http", "method", r.Method, "path", r.URL.Path, "status", rec.status,
				"ms", time.Since(start).Milliseconds(), "ip", app.clientIP(r))
		}()

		isWebhook := r.URL.Path == "/webhook/recording"
		if r.Body != nil {
			limit := int64(formBodyLimit)
			if isWebhook {
				limit = webhookBodyLimit
			} else if strings.HasPrefix(r.URL.Path, "/m/") && strings.HasSuffix(r.URL.Path, "/audio") {
				limit = chunkBodyLimit
			}
			r.Body = http.MaxBytesReader(rec, r.Body, limit)
		}
		if r.Method == http.MethodPost && !isWebhook && !app.sameOrigin(r) {
			http.Error(rec, "Forbidden", http.StatusForbidden)
			return
		}
		next.ServeHTTP(rec, r)
	})
}

// --- rate limiting ---

// failureLimiter blocks a key after max failures inside window. It is kept
// in memory: a restart clears it, which is acceptable for a single instance
// sitting behind Cloudflare and nginx rate limits.
type failureLimiter struct {
	mu     sync.Mutex
	max    int
	window time.Duration
	now    func() time.Time
	hits   map[string][]time.Time
}

func newFailureLimiter(max int, window time.Duration) *failureLimiter {
	return &failureLimiter{max: max, window: window, now: time.Now, hits: map[string][]time.Time{}}
}

func (l *failureLimiter) prune(key string, now time.Time) []time.Time {
	kept := l.hits[key][:0]
	for _, t := range l.hits[key] {
		if now.Sub(t) < l.window {
			kept = append(kept, t)
		}
	}
	if len(kept) == 0 {
		delete(l.hits, key)
		return nil
	}
	l.hits[key] = kept
	return kept
}

func (l *failureLimiter) Blocked(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.prune(key, l.now())) >= l.max
}

func (l *failureLimiter) Fail(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	l.prune(key, now)
	l.hits[key] = append(l.hits[key], now)
	if len(l.hits) > 10000 {
		for k := range l.hits {
			l.prune(k, now)
		}
	}
}

func (l *failureLimiter) Reset(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.hits, key)
}
