package main

import (
	"context"
	"crypto/tls"
	"encoding/gob"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"

	"github.com/coreos/go-oidc/v3/oidc"
	"github.com/gorilla/sessions"
	"golang.org/x/oauth2"

	"github.com/cgs-earth/sal-pub/pkg/util"
)

var (
	oauth2Config *oauth2.Config
	verifier     *oidc.IDTokenVerifier

	store = sessions.NewCookieStore(
		[]byte(util.GetEnv("SESSION_SECRET", "change-this-secret")),
	)
)

type User struct {
	Subject    string `json:"sub"`
	Username   string `json:"preferred_username"`
	Email      string `json:"email"`
	Name       string `json:"name"`
	GivenName  string `json:"given_name"`
	FamilyName string `json:"family_name"`
}

func main() {
	gob.Register(User{})

	ctx := context.Background()

	// -------------------------------------------------------------------------
	// Configuration
	// -------------------------------------------------------------------------

	keycloakHost := util.GetEnv(
		"KC_HOSTNAME",
		"https://localhost",
	)

	// KC_HOSTNAME may be a bare host (e.g. "localhost"), which Keycloak and
	// Caddy accept. Caddy serves bare hosts over HTTPS, so default to that.
	if !strings.Contains(keycloakHost, "://") {
		keycloakHost = "https://" + keycloakHost
	}

	keycloakPath := util.GetEnv(
		"KC_HTTP_RELATIVE_PATH",
		"/auth",
	)

	keycloakURL := fmt.Sprintf(
		"%s%s",
		keycloakHost,
		keycloakPath,
	)

	realm := util.GetEnv(
		"KEYCLOAK_REALM",
		"sal",
	)

	clientID := util.GetEnv(
		"KEYCLOAK_CLIENT_ID",
		"sal-pub",
	)

	clientSecret := os.Getenv("KEYCLOAK_CLIENT_SECRET")

	redirectURL := util.GetEnv(
		"REDIRECT_URL",
		"http://localhost:8000/auth/callback",
	)

	// -------------------------------------------------------------------------
	// DEVELOPMENT ONLY:
	// Disable TLS certificate verification.
	//
	// This is useful for your local Caddy/Keycloak setup where you're using
	// a local CA certificate.
	// -------------------------------------------------------------------------

	http.DefaultTransport.(*http.Transport).TLSClientConfig = &tls.Config{
		InsecureSkipVerify: true, //nolint:gosec
	}

	// -------------------------------------------------------------------------
	// OIDC discovery
	// -------------------------------------------------------------------------

	issuer := fmt.Sprintf(
		"%s/realms/%s",
		keycloakURL,
		realm,
	)

	log.Printf("Using Keycloak issuer: %s", issuer)

	provider, err := oidc.NewProvider(ctx, issuer)
	if err != nil {
		log.Fatalf(
			"initialize Keycloak OIDC provider: %v",
			err,
		)
	}

	// -------------------------------------------------------------------------
	// OIDC token verifier
	// -------------------------------------------------------------------------

	verifier = provider.Verifier(
		&oidc.Config{
			ClientID: clientID,
		},
	)

	// -------------------------------------------------------------------------
	// OAuth2 configuration
	// -------------------------------------------------------------------------

	oauth2Config = &oauth2.Config{
		ClientID:     clientID,
		ClientSecret: clientSecret,
		Endpoint:     provider.Endpoint(),
		RedirectURL:  redirectURL,
		Scopes: []string{
			oidc.ScopeOpenID,
			"profile",
			"email",
		},
	}

	// -------------------------------------------------------------------------
	// Routes
	// -------------------------------------------------------------------------

	http.HandleFunc("/", index)
	http.HandleFunc("/login", login)
	http.HandleFunc("/auth/callback", callback)
	http.HandleFunc("/logout", logout)

	log.Println("Server listening on http://localhost:8000")

	log.Fatal(
		http.ListenAndServe(
			":8000",
			nil,
		),
	)
}

// -----------------------------------------------------------------------------
// Home
// -----------------------------------------------------------------------------

func index(w http.ResponseWriter, r *http.Request) {
	session, err := store.Get(r, "session")
	if err != nil {
		http.Error(
			w,
			err.Error(),
			http.StatusInternalServerError,
		)
		return
	}

	username, _ := session.Values["username"].(string)
	email, _ := session.Values["email"].(string)
	subject, _ := session.Values["subject"].(string)

	if username == "" && email == "" && subject == "" {
		fmt.Fprint(w, `
<!doctype html>
<html>
<head>
	<title>Go + Keycloak</title>
</head>
<body>
	<h1>Go + Keycloak</h1>

	<p>You are not logged in.</p>

	<a href="/login">Log in with Keycloak</a>
</body>
</html>
`)

		return
	}

	fmt.Fprintf(w, `
<!doctype html>
<html>
<head>
	<title>Logged In</title>
</head>
<body>
	<h1>Welcome!</h1>

	<p>
		Logged in as:
		<strong>%s</strong>
	</p>

	<p>
		Email:
		<strong>%s</strong>
	</p>

	<p>
		Subject:
		<code>%s</code>
	</p>

	<p>
		<a href="/logout">Log out</a>
	</p>
</body>
</html>
`,
		username,
		email,
		subject,
	)
}

// -----------------------------------------------------------------------------
// Login
// -----------------------------------------------------------------------------

func login(w http.ResponseWriter, r *http.Request) {
	// NOTE:
	// For production, generate a cryptographically random state value.
	state := "some-random-state"

	session, err := store.Get(r, "session")
	if err != nil {
		http.Error(
			w,
			err.Error(),
			http.StatusInternalServerError,
		)
		return
	}

	session.Values["state"] = state

	if err := session.Save(r, w); err != nil {
		http.Error(
			w,
			err.Error(),
			http.StatusInternalServerError,
		)
		return
	}

	url := oauth2Config.AuthCodeURL(
		state,
		oauth2.AccessTypeOffline,
	)

	http.Redirect(
		w,
		r,
		url,
		http.StatusFound,
	)
}

// -----------------------------------------------------------------------------
// OAuth callback
// -----------------------------------------------------------------------------

func callback(w http.ResponseWriter, r *http.Request) {
	session, err := store.Get(r, "session")
	if err != nil {
		http.Error(
			w,
			err.Error(),
			http.StatusInternalServerError,
		)
		return
	}

	// -------------------------------------------------------------------------
	// Verify OAuth state
	// -------------------------------------------------------------------------

	expectedState, _ := session.Values["state"].(string)
	actualState := r.URL.Query().Get("state")

	if expectedState == "" || actualState != expectedState {
		http.Error(
			w,
			"invalid OAuth state",
			http.StatusBadRequest,
		)
		return
	}

	// -------------------------------------------------------------------------
	// Authorization code
	// -------------------------------------------------------------------------

	code := r.URL.Query().Get("code")

	if code == "" {
		http.Error(
			w,
			"missing authorization code",
			http.StatusBadRequest,
		)
		return
	}

	// -------------------------------------------------------------------------
	// Exchange code for tokens
	// -------------------------------------------------------------------------

	token, err := oauth2Config.Exchange(
		r.Context(),
		code,
	)

	if err != nil {
		http.Error(
			w,
			fmt.Sprintf(
				"token exchange failed: %v",
				err,
			),
			http.StatusInternalServerError,
		)
		return
	}

	// -------------------------------------------------------------------------
	// Get ID token
	// -------------------------------------------------------------------------

	rawIDToken, ok := token.Extra("id_token").(string)
	if !ok {
		http.Error(
			w,
			"missing id_token",
			http.StatusInternalServerError,
		)
		return
	}

	// -------------------------------------------------------------------------
	// Verify ID token
	// -------------------------------------------------------------------------

	idToken, err := verifier.Verify(
		r.Context(),
		rawIDToken,
	)

	if err != nil {
		http.Error(
			w,
			fmt.Sprintf(
				"ID token verification failed: %v",
				err,
			),
			http.StatusInternalServerError,
		)
		return
	}

	// -------------------------------------------------------------------------
	// Parse our User struct
	// -------------------------------------------------------------------------

	var user User

	if err := idToken.Claims(&user); err != nil {
		http.Error(
			w,
			fmt.Sprintf(
				"failed to parse user claims: %v",
				err,
			),
			http.StatusInternalServerError,
		)
		return
	}

	log.Printf(
		"USER STRUCT: %+v",
		user,
	)

	// -------------------------------------------------------------------------
	// Store only small values in the browser cookie.
	//
	// DO NOT store access_token or id_token here.
	// -------------------------------------------------------------------------

	session.Values["username"] = user.Username
	session.Values["email"] = user.Email
	session.Values["subject"] = user.Subject

	delete(session.Values, "state")

	if err := session.Save(r, w); err != nil {
		http.Error(
			w,
			fmt.Sprintf(
				"failed to save session: %v",
				err,
			),
			http.StatusInternalServerError,
		)
		return
	}

	// -------------------------------------------------------------------------
	// Redirect to application
	// -------------------------------------------------------------------------

	http.Redirect(
		w,
		r,
		"/",
		http.StatusFound,
	)
}

// -----------------------------------------------------------------------------
// Logout
// -----------------------------------------------------------------------------

func logout(w http.ResponseWriter, r *http.Request) {
	session, err := store.Get(r, "session")
	if err != nil {
		http.Error(
			w,
			err.Error(),
			http.StatusInternalServerError,
		)
		return
	}

	// Clear all application session values.
	session.Values = make(map[interface{}]interface{})

	// Expire the cookie.
	session.Options.MaxAge = -1

	if err := session.Save(r, w); err != nil {
		http.Error(
			w,
			err.Error(),
			http.StatusInternalServerError,
		)
		return
	}

	http.Redirect(
		w,
		r,
		"/",
		http.StatusFound,
	)
}
