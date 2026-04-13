package backend

import (
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const (
	allowMethods = "GET, POST, PUT, DELETE, OPTIONS"
	allowHeaders = "Content-Type, Authorization, X-Emby-Token, X-Emby-Authorization, X-Emby-Client, X-Emby-Client-Version, X-Emby-Device-Name, X-Emby-Device-Id"
)

type statusCapture struct {
	http.ResponseWriter
	status int
	wrote  bool
}

func (sc *statusCapture) WriteHeader(code int) {
	if !sc.wrote {
		sc.status = code
		sc.wrote = true
	}
	sc.ResponseWriter.WriteHeader(code)
}

func (sc *statusCapture) Write(b []byte) (int, error) {
	if !sc.wrote {
		sc.status = http.StatusOK
		sc.wrote = true
	}
	return sc.ResponseWriter.Write(b)
}

func (sc *statusCapture) Flush() {
	if f, ok := sc.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

const maxRequestBodySize = 2 << 20 // 2 MB

func (a *App) bodyLimitMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Body != nil && r.ContentLength != 0 {
			r.Body = http.MaxBytesReader(w, r.Body, maxRequestBodySize)
		}
		next.ServeHTTP(w, r)
	})
}

// clientIP extracts the real client IP. Proxy headers are only trusted when trustProxy is true.
func clientIP(r *http.Request, trustProxy bool) string {
	if trustProxy {
		if ip := strings.TrimSpace(r.Header.Get("X-Real-IP")); ip != "" {
			return ip
		}
		if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
			if ip, _, _ := strings.Cut(xff, ","); strings.TrimSpace(ip) != "" {
				return strings.TrimSpace(ip)
			}
		}
	}
	if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		return host
	}
	return r.RemoteAddr
}

func (a *App) loggingMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if a.Logger == nil || isAdminAPIPath(r.URL.Path) || isAdminPath(r.URL.Path) || r.URL.Path == "/favicon.ico" {
			next.ServeHTTP(w, r)
			return
		}
		start := time.Now()
		tokenSource := identifyTokenSource(r)
		sc := &statusCapture{ResponseWriter: w, status: http.StatusOK}
		a.Logger.Debugf("→ %s %s [auth:%s]", r.Method, r.URL.Path, tokenSource)
		next.ServeHTTP(sc, r)
		ms := time.Since(start).Milliseconds()
		msg := fmt.Sprintf("%s %s → %d (%dms) [auth:%s]", r.Method, r.URL.Path, sc.status, ms, tokenSource)
		switch {
		case sc.status >= 500:
			a.Logger.Errorf("%s", msg)
		case sc.status >= 400:
			a.Logger.Warnf("%s", msg)
		default:
			a.Logger.Debugf("%s", msg)
		}
	})
}

func identifyTokenSource(r *http.Request) string {
	if r.Header.Get("X-Emby-Token") != "" {
		return "X-Emby-Token"
	}
	if r.URL.Query().Get("api_key") != "" {
		return "api_key"
	}
	if r.URL.Query().Get("ApiKey") != "" {
		return "ApiKey"
	}
	if r.Header.Get("X-Emby-Authorization") != "" {
		return "X-Emby-Authorization"
	}
	if r.Header.Get("Authorization") != "" {
		return "Authorization"
	}
	return "none"
}

func (a *App) prefixCompatMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/emby" || r.URL.Path == "/emby/" {
			clone := r.Clone(r.Context())
			copiedURL := *clone.URL
			clone.URL = &copiedURL
			clone.URL.Path = "/"
			next.ServeHTTP(w, clone)
			return
		}
		if strings.HasPrefix(r.URL.Path, "/emby/") {
			clone := r.Clone(r.Context())
			copiedURL := *clone.URL
			clone.URL = &copiedURL
			clone.URL.Path = strings.TrimPrefix(r.URL.Path, "/emby")
			if clone.URL.Path == "" {
				clone.URL.Path = "/"
			}
			next.ServeHTTP(w, clone)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (a *App) corsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if isAdminPath(r.URL.Path) {
			applyAdminSecurityHeaders(w)
		}
		if isAdminAPIPath(r.URL.Path) {
			if origin := r.Header.Get("Origin"); origin != "" && sameOrigin(origin, adminRequestOrigin(r)) {
				w.Header().Set("Access-Control-Allow-Origin", origin)
				w.Header().Set("Vary", "Origin")
			}
		} else {
			w.Header().Set("Access-Control-Allow-Origin", "*")
		}
		w.Header().Set("Access-Control-Allow-Methods", allowMethods)
		w.Header().Set("Access-Control-Allow-Headers", allowHeaders)
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusOK)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func isAdminPath(path string) bool {
	return path == "/admin" || strings.HasPrefix(path, "/admin/")
}

func isAdminAPIPath(path string) bool {
	return strings.HasPrefix(path, "/admin/api/")
}

func applyAdminSecurityHeaders(w http.ResponseWriter) {
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("X-Frame-Options", "SAMEORIGIN")
	w.Header().Set("X-XSS-Protection", "1; mode=block")
	w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self' 'unsafe-inline' 'unsafe-eval' https://cdn.tailwindcss.com https://unpkg.com; style-src 'self' 'unsafe-inline' https://fonts.googleapis.com; font-src https://fonts.gstatic.com; img-src 'self' data: https:; connect-src 'self'")
}

func requestOrigin(r *http.Request) string {
	scheme := "http"
	if forwarded := strings.TrimSpace(strings.Split(r.Header.Get("X-Forwarded-Proto"), ",")[0]); forwarded != "" {
		scheme = forwarded
	} else if r.TLS != nil {
		scheme = "https"
	}
	host := r.Host
	if forwardedHost := strings.TrimSpace(strings.Split(r.Header.Get("X-Forwarded-Host"), ",")[0]); forwardedHost != "" {
		host = forwardedHost
	}
	return scheme + "://" + host
}

func sameOrigin(aOrigin, bOrigin string) bool {
	parsedA, errA := url.Parse(aOrigin)
	parsedB, errB := url.Parse(bOrigin)
	if errA != nil || errB != nil {
		return false
	}
	return strings.EqualFold(parsedA.Scheme, parsedB.Scheme) && strings.EqualFold(parsedA.Host, parsedB.Host)
}

func adminRequestOrigin(r *http.Request) string {
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	if proto := strings.TrimSpace(strings.Split(r.Header.Get("X-Forwarded-Proto"), ",")[0]); proto != "" {
		scheme = proto
	}
	return scheme + "://" + r.Host
}

// isPrivateOrReservedIP returns true if the host resolves to a private, loopback,
// link-local, or other reserved IP address that should not be reachable via SSRF.
func isPrivateOrReservedIP(host string) bool {
	h := host
	if hp, _, err := net.SplitHostPort(host); err == nil {
		h = hp
	}
	ips, err := net.LookupIP(h)
	if err != nil {
		ip := net.ParseIP(h)
		if ip == nil {
			return false
		}
		ips = []net.IP{ip}
	}
	for _, ip := range ips {
		if ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() ||
			ip.IsLinkLocalMulticast() || ip.IsUnspecified() {
			return true
		}
	}
	return false
}
