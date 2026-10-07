// Package web is the browser-facing side of sal-pub: Keycloak login, a
// session cookie, and a small page for posting notes.
package web

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"errors"
	"fmt"
	"html/template"
	"log"
	"net/http"
	"net/url"
	"strings"

	"github.com/coreos/go-oidc/v3/oidc"
	"github.com/gorilla/sessions"
	"golang.org/x/oauth2"

	"github.com/cgs-earth/sal-pub/internal/activitypub"
	"github.com/cgs-earth/sal-pub/internal/store"
	"github.com/cgs-earth/sal-pub/pkg/util"
)

const (
	sessionName  = "salpub"
	maxNoteChars = 5000
)

type Config struct {
	Issuer string // e.g. http://localhost/auth/realms/sal

	// Optional internal URL to fetch the discovery document from, for when the
	// public issuer URL isn't reachable from here (e.g. inside Docker). Tokens
	// are still checked against Issuer. Pair it with Keycloak's
	// KC_HOSTNAME_BACKCHANNEL_DYNAMIC=true so token/JWKS URLs are internal too.
	DiscoveryURL  string
	ClientID      string
	ClientSecret  string
	RedirectURL   string
	SessionSecret []byte
	SecureCookies bool // set when served over HTTPS

	// Where Keycloak sends the browser after logout. Must be listed in the
	// client's "Valid post logout redirect URIs".
	PostLogoutRedirectURL string

	// DEVELOPMENT ONLY: skip TLS verification when talking to Keycloak, for
	// a Caddy local CA. Applies to OIDC calls only, never to federation.
	InsecureSkipVerify bool
}

type App struct {
	store    *store.Store
	ap       *activitypub.Server
	oauth    *oauth2.Config
	verifier *oidc.IDTokenVerifier
	sessions *sessions.CookieStore
	client   *http.Client // for Keycloak
	callback string       // path component of RedirectURL

	endSessionURL         string // Keycloak's end_session_endpoint, if advertised
	postLogoutRedirectURL string
}

func New(ctx context.Context, cfg Config, st *store.Store, ap *activitypub.Server) (*App, error) {
	client := http.DefaultClient
	if cfg.InsecureSkipVerify {
		log.Print("WARNING: TLS verification disabled for Keycloak (OIDC_INSECURE_SKIP_VERIFY)")
		client = &http.Client{Transport: &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, //nolint:gosec // development only
		}}
	}

	discoveryCtx, discoveryURL := oidc.ClientContext(ctx, client), cfg.Issuer
	if cfg.DiscoveryURL != "" {
		discoveryCtx, discoveryURL = oidc.InsecureIssuerURLContext(discoveryCtx, cfg.Issuer), cfg.DiscoveryURL
	}
	provider, err := oidc.NewProvider(discoveryCtx, discoveryURL)
	if err != nil {
		return nil, fmt.Errorf("discover Keycloak OIDC provider %s: %w", discoveryURL, err)
	}

	var discovery struct {
		EndSessionEndpoint string `json:"end_session_endpoint"`
	}
	if err := provider.Claims(&discovery); err != nil {
		return nil, fmt.Errorf("read OIDC discovery document: %w", err)
	}

	redirect, err := url.Parse(cfg.RedirectURL)
	if err != nil || redirect.Path == "" {
		return nil, fmt.Errorf("invalid redirect URL %q", cfg.RedirectURL)
	}

	cookies := sessions.NewCookieStore(cfg.SessionSecret)
	cookies.Options = &sessions.Options{
		Path:     "/",
		MaxAge:   7 * 24 * 60 * 60,
		HttpOnly: true,
		Secure:   cfg.SecureCookies,
		SameSite: http.SameSiteLaxMode,
	}

	return &App{
		store:    st,
		ap:       ap,
		verifier: provider.Verifier(&oidc.Config{ClientID: cfg.ClientID}),
		oauth: &oauth2.Config{
			ClientID:     cfg.ClientID,
			ClientSecret: cfg.ClientSecret,
			Endpoint:     provider.Endpoint(),
			RedirectURL:  cfg.RedirectURL,
			Scopes:       []string{oidc.ScopeOpenID, "profile", "email"},
		},
		sessions: cookies,
		client:   client,
		callback: redirect.Path,

		endSessionURL:         discovery.EndSessionEndpoint,
		postLogoutRedirectURL: cfg.PostLogoutRedirectURL,
	}, nil
}

// Register adds the browser routes to mux. State-changing routes are
// protected against cross-origin form posts.
func (a *App) Register(mux *http.ServeMux) {
	csrf := http.NewCrossOriginProtection()

	mux.HandleFunc("GET /{$}", a.home)
	mux.HandleFunc("GET /login", a.login)
	mux.HandleFunc("GET "+a.callback, a.oauthCallback)
	mux.Handle("POST /logout", csrf.Handler(http.HandlerFunc(a.logout)))
	mux.Handle("POST /notes", csrf.Handler(http.HandlerFunc(a.postNote)))
}

// currentActor returns the logged-in user's actor, or nil if not logged in.
func (a *App) currentActor(r *http.Request) (*store.Actor, error) {
	session, _ := a.sessions.Get(r, sessionName) // a bad cookie just means a new session
	subject, _ := session.Values["subject"].(string)
	if subject == "" {
		return nil, nil
	}
	actor, err := a.store.GetBySubject(r.Context(), subject)
	if errors.Is(err, store.ErrNotFound) {
		return nil, nil
	}
	return actor, err
}

func (a *App) home(w http.ResponseWriter, r *http.Request) {
	actor, err := a.currentActor(r)
	if err != nil {
		util.ServerError(w, err)
		return
	}

	data := map[string]any{"Actor": actor}
	if actor != nil {
		notes, total, err := a.store.ListNotes(r.Context(), actor.ID, 20)
		if err != nil {
			util.ServerError(w, err)
			return
		}
		followers, err := a.store.Followers(r.Context(), actor.ID)
		if err != nil {
			util.ServerError(w, err)
			return
		}
		data["Handle"] = a.ap.Handle(actor.Username)
		data["Notes"] = notes
		data["NoteCount"] = total
		data["Followers"] = followers
		data["MaxNoteChars"] = maxNoteChars
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := homeTemplate.Execute(w, data); err != nil {
		log.Printf("render home: %v", err)
	}
}

func (a *App) login(w http.ResponseWriter, r *http.Request) {
	session, _ := a.sessions.Get(r, sessionName)
	state := rand.Text()
	verifier := oauth2.GenerateVerifier()
	session.Values["state"] = state
	session.Values["pkce"] = verifier
	if err := session.Save(r, w); err != nil {
		util.ServerError(w, err)
		return
	}
	http.Redirect(w, r, a.oauth.AuthCodeURL(state, oauth2.S256ChallengeOption(verifier)), http.StatusFound)
}

// claims are the ID token claims we use.
type claims struct {
	Subject           string `json:"sub"`
	PreferredUsername string `json:"preferred_username"`
	Name              string `json:"name"`
	Email             string `json:"email"`
}

func (a *App) oauthCallback(w http.ResponseWriter, r *http.Request) {
	session, _ := a.sessions.Get(r, sessionName)
	state, _ := session.Values["state"].(string)
	verifier, _ := session.Values["pkce"].(string)
	delete(session.Values, "state")
	delete(session.Values, "pkce")

	if state == "" || r.URL.Query().Get("state") != state {
		http.Error(w, "invalid OAuth state; try logging in again", http.StatusBadRequest)
		return
	}
	if e := r.URL.Query().Get("error"); e != "" {
		http.Error(w, "login failed: "+e, http.StatusUnauthorized)
		return
	}

	ctx := context.WithValue(r.Context(), oauth2.HTTPClient, a.client)
	token, err := a.oauth.Exchange(ctx, r.URL.Query().Get("code"), oauth2.VerifierOption(verifier))
	if err != nil {
		log.Printf("token exchange: %v", err)
		http.Error(w, "login failed", http.StatusUnauthorized)
		return
	}
	rawIDToken, _ := token.Extra("id_token").(string)
	idToken, err := a.verifier.Verify(oidc.ClientContext(r.Context(), a.client), rawIDToken)
	if err != nil {
		log.Printf("verify ID token: %v", err)
		http.Error(w, "login failed", http.StatusUnauthorized)
		return
	}

	var c claims
	if err := idToken.Claims(&c); err != nil {
		util.ServerError(w, fmt.Errorf("parse ID token claims: %w", err))
		return
	}
	username := c.PreferredUsername
	if username == "" {
		username = c.Email
	}

	actor, err := a.ap.EnsureActor(r.Context(), c.Subject, username, c.Name)
	if err != nil {
		util.ServerError(w, err)
		return
	}

	session.Values["subject"] = actor.KeycloakSubject
	session.Values["id_token"] = rawIDToken // needed as id_token_hint on logout
	if err := session.Save(r, w); err != nil {
		util.ServerError(w, err)
		return
	}
	http.Redirect(w, r, "/", http.StatusFound)
}

// logout clears the local session and then sends the browser to Keycloak's
// end-session endpoint (RP-initiated logout) so the SSO session ends too.
func (a *App) logout(w http.ResponseWriter, r *http.Request) {
	session, _ := a.sessions.Get(r, sessionName)
	idToken, _ := session.Values["id_token"].(string)
	session.Values = map[any]any{}
	session.Options.MaxAge = -1
	if err := session.Save(r, w); err != nil {
		util.ServerError(w, err)
		return
	}

	if a.endSessionURL == "" {
		http.Redirect(w, r, "/", http.StatusSeeOther)
		return
	}
	params := url.Values{"client_id": {a.oauth.ClientID}}
	if idToken != "" {
		params.Set("id_token_hint", idToken)
	}
	if a.postLogoutRedirectURL != "" {
		params.Set("post_logout_redirect_uri", a.postLogoutRedirectURL)
	}
	http.Redirect(w, r, a.endSessionURL+"?"+params.Encode(), http.StatusSeeOther)
}

func (a *App) postNote(w http.ResponseWriter, r *http.Request) {
	actor, err := a.currentActor(r)
	if err != nil {
		util.ServerError(w, err)
		return
	}
	if actor == nil {
		http.Error(w, "log in first", http.StatusUnauthorized)
		return
	}

	text := strings.TrimSpace(r.PostFormValue("content"))
	if text == "" || len([]rune(text)) > maxNoteChars {
		http.Error(w, fmt.Sprintf("a note must be 1-%d characters", maxNoteChars), http.StatusBadRequest)
		return
	}
	if _, err := a.ap.PublishNote(r.Context(), actor, text); err != nil {
		util.ServerError(w, err)
		return
	}
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// Note content is HTML that activitypub.PublishNote built from escaped text.
var homeTemplate = template.Must(template.New("home").Funcs(template.FuncMap{
	"noteHTML": func(s string) template.HTML { return template.HTML(s) }, //nolint:gosec // escaped at write time
}).Parse(`<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>sal-pub</title>
<style>
  body { font-family: system-ui, sans-serif; max-width: 40rem; margin: 2rem auto; padding: 0 1rem; line-height: 1.5; }
  textarea { width: 100%; box-sizing: border-box; font: inherit; }
  .note { border-top: 1px solid #ddd; padding: .5rem 0; }
  .meta { color: #666; font-size: .875rem; }
  code { word-break: break-all; }
</style>
</head>
<body>
<h1>sal-pub</h1>
{{if not .Actor}}
  <p>Sign in to get an ActivityPub identity that other fediverse servers can follow.</p>
  <p><a href="/login">Log in with Keycloak</a></p>
{{else}}
  <p>Signed in as <strong>{{or .Actor.DisplayName .Actor.Username}}</strong>.
     Your fediverse handle is <strong>{{.Handle}}</strong>.</p>
  <p class="meta">Actor: <a href="{{.Actor.ActorURI}}"><code>{{.Actor.ActorURI}}</code></a><br>
     {{.NoteCount}} notes · {{len .Followers}} followers</p>
  <form method="post" action="/logout"><button>Log out</button></form>

  <h2>New note</h2>
  <form method="post" action="/notes">
    <textarea name="content" rows="4" maxlength="{{.MaxNoteChars}}" required></textarea>
    <p><button>Publish</button> <span class="meta">Public, and delivered to your followers.</span></p>
  </form>

  {{if .Followers}}
  <h2>Followers</h2>
  <ul>{{range .Followers}}<li><a href="{{.}}">{{.}}</a></li>{{end}}</ul>
  {{end}}

  <h2>Your notes</h2>
  {{range .Notes}}
    <div class="note">
      <div class="meta"><a href="/notes/{{.ID}}">{{.Published.Format "2006-01-02 15:04 MST"}}</a></div>
      <div>{{noteHTML .Content}}</div>
    </div>
  {{else}}
    <p class="meta">Nothing yet.</p>
  {{end}}
{{end}}
</body>
</html>
`))
