package util

import (
	"log"
	"net/http"
	"strings"
	"time"
)

// ServerError logs err and replies with a generic 500, so internal details
// never reach the client.
func ServerError(w http.ResponseWriter, err error) {
	log.Printf("error: %v", err)
	http.Error(w, "internal server error", http.StatusInternalServerError)
}

// LogRequests is middleware that logs each request's method, URI and duration.
func LogRequests(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		next.ServeHTTP(w, r)
		log.Printf("%s %s (%s)", r.Method, r.URL.RequestURI(), time.Since(start).Round(time.Millisecond))
	})
}

// WithScheme adds https:// to a bare host such as "localhost". Keycloak and
// Caddy accept bare hosts in SALPUB_HOSTNAME/KC_HOSTNAME, and Caddy serves
// them over HTTPS.
func WithScheme(u string) string {
	if !strings.Contains(u, "://") {
		return "https://" + u
	}
	return u
}
