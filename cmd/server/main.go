// Command server runs sal-pub: Keycloak login plus an ActivityPub server
// that gives every Keycloak user a followable actor.
package main

import (
	"context"
	"crypto/rand"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/cgs-earth/sal-pub/internal/activitypub"
	"github.com/cgs-earth/sal-pub/internal/store"
	"github.com/cgs-earth/sal-pub/internal/web"
	"github.com/cgs-earth/sal-pub/pkg/util"
)

func main() {
	ctx := context.Background()

	// Public URL of this server. Actor IDs are built from it, so it must be
	// what remote servers use to reach us, and it shouldn't change later.
	baseURL := util.WithScheme(util.GetEnv("SALPUB_HOSTNAME", "http://localhost:8000"))
	listenAddr := util.GetEnv("LISTEN_ADDR", ":8000")
	databaseURL := util.GetEnv("DATABASE_URL",
		"postgres://keycloak:keycloak@localhost:5432/salpub?sslmode=disable")

	// KC_HOSTNAME is either a bare host ("localhost") or a full URL, which
	// Keycloak requires to include the relative path ("https://localhost/auth").
	keycloakURL := util.WithScheme(util.GetEnv("KC_HOSTNAME", "http://localhost"))
	if relativePath := util.GetEnv("KC_HTTP_RELATIVE_PATH", "/auth"); !strings.HasSuffix(keycloakURL, relativePath) {
		keycloakURL += relativePath
	}
	issuer := keycloakURL + "/realms/" + util.GetEnv("KEYCLOAK_REALM", "sal")

	sessionSecret := []byte(util.GetEnv("SESSION_SECRET", ""))
	if len(sessionSecret) == 0 {
		log.Print("WARNING: SESSION_SECRET is not set; using a random one, so sessions end on restart")
		sessionSecret = make([]byte, 32)
		rand.Read(sessionSecret)
	}

	st, err := store.Open(ctx, databaseURL)
	if err != nil {
		log.Fatalf("connect to Postgres (does the salpub database exist? see AGENTS.md): %v", err)
	}
	defer st.Close()
	if err := st.Migrate(ctx); err != nil {
		log.Fatalf("migrate: %v", err)
	}

	ap, err := activitypub.New(st, baseURL)
	if err != nil {
		log.Fatal(err)
	}

	log.Printf("Using Keycloak issuer: %s", issuer)
	app, err := web.New(ctx, web.Config{
		Issuer:                issuer,
		DiscoveryURL:          util.GetEnv("OIDC_DISCOVERY_URL", ""),
		ClientID:              util.GetEnv("KEYCLOAK_CLIENT_ID", "sal-pub"),
		ClientSecret:          util.GetEnv("KEYCLOAK_CLIENT_SECRET", ""),
		RedirectURL:           util.WithScheme(util.GetEnv("REDIRECT_URL", baseURL+"/oauth/callback")),
		SessionSecret:         sessionSecret,
		SecureCookies:         strings.HasPrefix(baseURL, "https://"),
		PostLogoutRedirectURL: strings.TrimRight(baseURL, "/") + "/",
		InsecureSkipVerify:    util.GetEnv("OIDC_INSECURE_SKIP_VERIFY", "") == "true",
	}, st, ap)
	if err != nil {
		log.Fatal(err)
	}

	mux := http.NewServeMux()
	ap.Register(mux)
	app.Register(mux)

	srv := &http.Server{
		Addr:              listenAddr,
		Handler:           util.LogRequests(mux),
		ReadHeaderTimeout: 10 * time.Second,
	}
	log.Printf("sal-pub listening on %s (public URL %s)", listenAddr, baseURL)
	log.Fatal(srv.ListenAndServe())
}
