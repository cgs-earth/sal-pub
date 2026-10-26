package util

import (
	"errors"
	"log"
	"net/http"
	"strings"
	"time"
)

// Errors that ServerError reports with their own status. Wrap them to add a
// message for the client: fmt.Errorf("%w: note is empty", util.ErrBadRequest).
var (
	ErrBadRequest = errors.New("bad request")
	ErrForbidden  = errors.New("forbidden")
	ErrNotFound   = errors.New("not found")
	ErrBadGateway = errors.New("bad gateway")
)

// ServerError replies to a failed request. Errors wrapping ErrBadRequest,
// ErrForbidden or ErrBadGateway are sent with their message and status, and
// ErrNotFound as a plain 404. Anything else is logged and gets a generic 500,
// so internal details never reach the client.
func ServerError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, ErrBadRequest):
		http.Error(w, err.Error(), http.StatusBadRequest)
	case errors.Is(err, ErrForbidden):
		http.Error(w, err.Error(), http.StatusForbidden)
	case errors.Is(err, ErrNotFound):
		http.Error(w, "not found", http.StatusNotFound)
	case errors.Is(err, ErrBadGateway):
		log.Printf("error: %v", err)
		http.Error(w, err.Error(), http.StatusBadGateway)
	default:
		log.Printf("error: %v", err)
		http.Error(w, "internal server error", http.StatusInternalServerError)
	}
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
