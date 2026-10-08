// Package web is the browser-facing side of sal-pub: Keycloak login, a
// session cookie, and a small page for posting notes.
package web

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"html"
	"html/template"
	"log"
	"net/http"
	"net/url"
	"regexp"
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
}

type App struct {
	store    *store.Store
	ap       *activitypub.Server
	oauth    *oauth2.Config
	verifier *oidc.IDTokenVerifier
	sessions *sessions.CookieStore
	callback string // path component of RedirectURL

	endSessionURL         string // Keycloak's end_session_endpoint, if advertised
	postLogoutRedirectURL string
}

func New(ctx context.Context, cfg Config, st *store.Store, ap *activitypub.Server) (*App, error) {
	discoveryCtx, discoveryURL := ctx, cfg.Issuer
	if cfg.DiscoveryURL != "" {
		discoveryCtx, discoveryURL = oidc.InsecureIssuerURLContext(ctx, cfg.Issuer), cfg.DiscoveryURL
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
	mux.HandleFunc("GET /users/{username}/inbox", a.readInbox)
	mux.HandleFunc("GET /objects/{id}", a.getObject)
	mux.Handle("POST /objects", csrf.Handler(a.requireActor(a.ap.ServeCreateObject)))
	mux.Handle("PUT /objects/{id}", csrf.Handler(a.requireActor(a.ap.ServeUpdateObject)))
	mux.Handle("DELETE /objects/{id}", csrf.Handler(a.requireActor(a.ap.ServeDeleteObject)))
	mux.Handle("POST /follow", csrf.Handler(http.HandlerFunc(a.follow)))
	mux.Handle("POST /unfollow", csrf.Handler(http.HandlerFunc(a.unfollow)))
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

	publicNotes, err := a.store.ListPublicNotes(r.Context(), 20)
	if err != nil {
		util.ServerError(w, err)
		return
	}

	data := map[string]any{"Actor": actor, "PublicNotes": publicNotes}
	if actor != nil {
		notes, total, err := a.store.ListNotes(r.Context(), actor.ID, 20, false)
		if err != nil {
			util.ServerError(w, err)
			return
		}
		followers, err := a.ap.Followers(r.Context(), actor)
		if err != nil {
			util.ServerError(w, err)
			return
		}
		following, err := a.ap.Following(r.Context(), actor)
		if err != nil {
			util.ServerError(w, err)
			return
		}
		data["Following"] = following
		inbox, err := a.store.ListInboxNotes(r.Context(), actor.ID, 20)
		if err != nil {
			util.ServerError(w, err)
			return
		}
		data["Inbox"] = inbox
		data["Handle"] = a.ap.Handle(actor.Username)
		data["Notes"] = notes
		data["NoteCount"] = total
		data["Followers"] = followers
		data["MaxNoteChars"] = maxNoteChars
		data["DefaultTo"] = "public"
		data["DefaultCC"] = activitypub.FollowersURI(actor)
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

	token, err := a.oauth.Exchange(r.Context(), r.URL.Query().Get("code"), oauth2.VerifierOption(verifier))
	if err != nil {
		log.Printf("token exchange: %v", err)
		http.Error(w, "login failed", http.StatusUnauthorized)
		return
	}
	rawIDToken, _ := token.Extra("id_token").(string)
	idToken, err := a.verifier.Verify(r.Context(), rawIDToken)
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
	to, err := activitypub.ParseAddresses(r.PostFormValue("to"))
	if err != nil {
		http.Error(w, "To: "+err.Error(), http.StatusBadRequest)
		return
	}
	cc, err := activitypub.ParseAddresses(r.PostFormValue("cc"))
	if err != nil {
		http.Error(w, "CC: "+err.Error(), http.StatusBadRequest)
		return
	}
	if len(to) == 0 && len(cc) == 0 {
		http.Error(w, "a note needs at least one recipient in To or CC", http.StatusBadRequest)
		return
	}
	if _, err := a.ap.PublishNote(r.Context(), actor, text, to, cc); err != nil {
		util.ServerError(w, err)
		return
	}
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// requireActor wraps a handler that needs the logged-in user's actor,
// replying 401 when nobody is logged in.
func (a *App) requireActor(h func(http.ResponseWriter, *http.Request, *store.Actor)) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		actor, err := a.currentActor(r)
		if err != nil {
			util.ServerError(w, err)
			return
		}
		if actor == nil {
			http.Error(w, "log in first", http.StatusUnauthorized)
			return
		}
		h(w, r, actor)
	})
}

// getObject serves /objects/{id}; non-public objects only to their author.
func (a *App) getObject(w http.ResponseWriter, r *http.Request) {
	actor, err := a.currentActor(r)
	if err != nil {
		util.ServerError(w, err)
		return
	}
	a.ap.ServeObject(w, r, actor)
}

// readInbox lets a logged-in user read their own inbox. (POST to the same
// path is federation delivery, handled by the activitypub package.)
func (a *App) readInbox(w http.ResponseWriter, r *http.Request) {
	actor, err := a.currentActor(r)
	if err != nil {
		util.ServerError(w, err)
		return
	}
	if actor == nil {
		http.Error(w, "log in first", http.StatusUnauthorized)
		return
	}
	if actor.Username != r.PathValue("username") {
		http.Error(w, "this is not your inbox", http.StatusForbidden)
		return
	}
	a.ap.ServeInbox(w, r, actor)
}

// follow sends a Follow to the actor URL or @user@host handle in "target".
func (a *App) follow(w http.ResponseWriter, r *http.Request) {
	actor, err := a.currentActor(r)
	if err != nil {
		util.ServerError(w, err)
		return
	}
	if actor == nil {
		http.Error(w, "log in first", http.StatusUnauthorized)
		return
	}
	target := strings.TrimSpace(r.PostFormValue("target"))
	if _, err := a.ap.Follow(r.Context(), actor, target); err != nil {
		log.Printf("follow %q: %v", target, err)
		http.Error(w, fmt.Sprintf("could not follow %s: %v", target, err), http.StatusBadGateway)
		return
	}
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

func (a *App) unfollow(w http.ResponseWriter, r *http.Request) {
	actor, err := a.currentActor(r)
	if err != nil {
		util.ServerError(w, err)
		return
	}
	if actor == nil {
		http.Error(w, "log in first", http.StatusUnauthorized)
		return
	}
	err = a.ap.Unfollow(r.Context(), actor, r.PostFormValue("target"))
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		util.ServerError(w, err)
		return
	}
	http.Redirect(w, r, "/", http.StatusSeeOther)
}

var htmlTag = regexp.MustCompile(`<[^>]*>`)

// Note content is HTML that activitypub.PublishNote built from escaped text.
// Inbox notes come from other servers, so they are only shown as plain text:
// tags are stripped and the result is escaped by html/template as usual.
var homeTemplate = template.Must(template.New("home").Funcs(template.FuncMap{
	"noteHTML":  func(s string) template.HTML { return template.HTML(s) }, //nolint:gosec // escaped at write time
	"plainText": func(s string) string { return html.UnescapeString(htmlTag.ReplaceAllString(s, " ")) },
}).Parse(`<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>sal-pub</title>
<style>
  body { font-family: system-ui, sans-serif; max-width: 40rem; margin: 2rem auto; padding: 0 1rem; line-height: 1.5; }
  textarea, input[type=text] { width: 100%; box-sizing: border-box; font: inherit; }
  label { display: block; margin-top: .5rem; }
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
  <h2>Public posts</h2>
  {{range .PublicNotes}}
    <div class="note">
      <div class="meta"><a href="{{.ActorURI}}"><strong>{{or .DisplayName .Username}}</strong></a>
        · <a href="/notes/{{.ID}}">{{.Published.Format "2006-01-02 15:04 MST"}}</a></div>
      <div>{{noteHTML .Content}}</div>
    </div>
  {{else}}
    <p class="meta">No public posts yet.</p>
  {{end}}
{{else}}
  <p>Signed in as <strong>{{or .Actor.DisplayName .Actor.Username}}</strong>.
     Your fediverse handle is <strong>{{.Handle}}</strong>.</p>
  <p class="meta">Actor: <a href="{{.Actor.ActorURI}}"><code>{{.Actor.ActorURI}}</code></a><br>
     {{.NoteCount}} notes · {{len .Followers}} followers</p>
  <form method="post" action="/logout"><button>Log out</button></form>

  <h2>New note</h2>
  <form method="post" action="/notes">
    <label>To <input type="text" name="to" value="{{.DefaultTo}}"></label>
    <label>CC <input type="text" name="cc" value="{{.DefaultCC}}"></label>
    <p class="meta">Actor or collection URLs, separated by spaces or commas. Use <code>public</code> for
       everyone and <code>{{.DefaultCC}}</code> for your followers. Notes not addressed to
       <code>public</code> are delivered only to their recipients and hidden from your outbox.</p>
    <textarea name="content" rows="4" maxlength="{{.MaxNoteChars}}" required></textarea>
    <p><button>Publish</button></p>
  </form>

  <h2>Following</h2>
  <form method="post" action="/follow" class="field">
    <input type="text" name="target" placeholder="@user@example.org or actor URL" required>
    <button>Follow</button>
  </form>
  {{if .Following}}
  <ul>{{range .Following}}
    <li><a href="{{.TargetURI}}">{{.TargetURI}}</a>
      {{if not .Accepted}}<span class="pill">pending</span>{{end}}
      <form method="post" action="/unfollow" style="display:inline">
        <input type="hidden" name="target" value="{{.TargetURI}}"><button>Unfollow</button>
      </form></li>
  {{end}}</ul>
  {{end}}

  {{if .Followers}}
  <h2>Followers</h2>
  <ul>{{range .Followers}}<li><a href="{{.}}">{{.}}</a></li>{{end}}</ul>
  {{end}}

  <h2>Inbox</h2>
  {{range .Inbox}}
    <div class="note">
      <div class="meta"><a href="{{.AttributedTo}}">{{.AttributedTo}}</a>
        · <a href="{{.ObjectID}}">{{.Published.Format "2006-01-02 15:04 MST"}}</a></div>
      <div>{{plainText .Content}}</div>
    </div>
  {{else}}
    <p class="meta">Nothing yet. Posts from accounts you follow, and posts addressed to you, show up here.</p>
  {{end}}

  <h2>Your notes</h2>
  {{range .Notes}}
    <div class="note">
      <div class="meta"><a href="/notes/{{.ID}}">{{.Published.Format "2006-01-02 15:04 MST"}}</a>
        · to: {{range $i, $a := .To}}{{if $i}}, {{end}}{{$a}}{{else}}—{{end}}
        {{with .CC}}· cc: {{range $i, $a := .}}{{if $i}}, {{end}}{{$a}}{{end}}{{end}}</div>
      <div>{{noteHTML .Content}}</div>
    </div>
  {{else}}
    <p class="meta">Nothing yet.</p>
  {{end}}
{{end}}

</body>
</html>
`))
