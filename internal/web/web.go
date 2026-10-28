// Package web is the browser-facing side of sal-pub: Keycloak login, a
// session cookie, and a small page for posting notes.
package web

import (
	"context"
	"crypto/rand"
	"embed"
	"errors"
	"fmt"
	"html/template"
	"log"
	"net/http"
	"net/url"
	"strings"

	"github.com/coreos/go-oidc/v3/oidc"
	vocab "github.com/go-ap/activitypub"
	"github.com/gorilla/sessions"
	"golang.org/x/oauth2"

	"github.com/cgs-earth/sal-pub/internal/activitypub"
	"github.com/cgs-earth/sal-pub/internal/store"
	"github.com/cgs-earth/sal-pub/pkg/util"
)

const sessionName = "salpub"

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
	mux.Handle("GET /static/", http.FileServerFS(staticFS))
	mux.HandleFunc("GET /login", a.login)
	mux.HandleFunc("GET "+a.callback, a.oauthCallback)
	mux.Handle("POST /logout", csrf.Handler(http.HandlerFunc(a.logout)))
	mux.Handle("POST /notes", csrf.Handler(a.requireActor(a.postNote)))
	mux.HandleFunc("GET /users/{username}/inbox", a.readInbox)
	mux.Handle("POST /users/{username}/outbox", csrf.Handler(a.requireActor(a.postOutbox)))
	mux.HandleFunc("GET /objects/{id}", a.withViewer(a.ap.ServeObject))
	mux.HandleFunc("GET /objects/{id}/{collection}", a.withViewer(a.ap.ServeInteractions))
	mux.Handle("POST /objects", csrf.Handler(a.requireActor(a.ap.ServeCreateObject)))
	mux.Handle("PUT /objects/{id}", csrf.Handler(a.requireActor(a.ap.ServeUpdateObject)))
	mux.Handle("DELETE /objects/{id}", csrf.Handler(a.requireActor(a.ap.ServeDeleteObject)))
	mux.Handle("POST /follow", csrf.Handler(a.requireActor(a.follow)))
	mux.Handle("POST /unfollow", csrf.Handler(a.requireActor(a.unfollow)))
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

	publicNotes, err := a.ap.PublicNotes(r.Context(), 20, actor)
	if err != nil {
		util.ServerError(w, err)
		return
	}

	data := map[string]any{"Actor": actor, "PublicNotes": publicNotes}
	if actor != nil {
		notes, total, err := a.ap.Notes(r.Context(), actor, 20)
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
		inboxIDs := make([]string, len(inbox))
		for i, n := range inbox {
			inboxIDs[i] = n.ObjectID
		}
		if data["InboxLiked"], err = a.ap.Liked(r.Context(), actor, inboxIDs); err != nil {
			util.ServerError(w, err)
			return
		}
		data["Handle"] = a.ap.Handle(actor.Username)
		data["Notes"] = notes
		data["NoteCount"] = total
		data["Followers"] = followers
		data["MaxNoteChars"] = activitypub.MaxNoteChars
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

// The form routes below are thin wrappers: each builds an activity and posts
// it to the user's outbox through activitypub.PostOutbox.

// post performs act as actor and redirects to back, or replies with the error.
func (a *App) post(w http.ResponseWriter, r *http.Request, actor *store.Actor, act vocab.Item, back string) {
	if _, err := a.ap.PostOutbox(r.Context(), actor, act); err != nil {
		util.ServerError(w, err)
		return
	}
	http.Redirect(w, r, back, http.StatusSeeOther)
}

// postNote publishes a note from form fields "content", "to" and "cc".
func (a *App) postNote(w http.ResponseWriter, r *http.Request, actor *store.Actor) {
	// A reply's addressing is set by the server; the to and cc fields are ignored.
	var to, cc []string
	var err error
	parent := strings.TrimSpace(r.PostFormValue("in_reply_to"))
	if parent != "" {
		if to, cc, err = a.ap.ReplyAddresses(r.Context(), actor, parent); err != nil {
			util.ServerError(w, err)
			return
		}
	} else {
		if to, err = activitypub.ParseAddresses(r.PostFormValue("to")); err != nil {
			http.Error(w, "To: "+err.Error(), http.StatusBadRequest)
			return
		}
		if cc, err = activitypub.ParseAddresses(r.PostFormValue("cc")); err != nil {
			http.Error(w, "CC: "+err.Error(), http.StatusBadRequest)
			return
		}
	}
	note := vocab.ObjectNew(vocab.NoteType)
	note.Source = vocab.Source{
		Content:   vocab.DefaultNaturalLanguage(r.PostFormValue("content")),
		MediaType: "text/plain",
	}
	for _, iri := range to {
		note.To = append(note.To, vocab.IRI(iri))
	}
	for _, iri := range cc {
		note.CC = append(note.CC, vocab.IRI(iri))
	}
	if parent != "" {
		note.InReplyTo = vocab.IRI(parent)
	}
	a.post(w, r, actor, vocab.CreateNew("", note), "/#notes")
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

// withViewer runs h with the logged-in actor, or nil when not logged in. It is
// for reads that show non-public objects only to their author.
func (a *App) withViewer(h func(http.ResponseWriter, *http.Request, *store.Actor)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		actor, err := a.currentActor(r)
		if err != nil {
			util.ServerError(w, err)
			return
		}
		h(w, r, actor)
	}
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
func (a *App) follow(w http.ResponseWriter, r *http.Request, actor *store.Actor) {
	target := vocab.IRI(strings.TrimSpace(r.PostFormValue("target")))
	a.post(w, r, actor, vocab.FollowNew("", target), "/#following")
}

// unfollow undoes the user's Follow of the actor URI in "target". Not
// following it isn't an error.
func (a *App) unfollow(w http.ResponseWriter, r *http.Request, actor *store.Actor) {
	undo := vocab.UndoNew("", vocab.FollowNew("", vocab.IRI(r.PostFormValue("target"))))
	if _, err := a.ap.PostOutbox(r.Context(), actor, undo); err != nil && !errors.Is(err, store.ErrNotFound) {
		util.ServerError(w, err)
		return
	}
	http.Redirect(w, r, "/#following", http.StatusSeeOther)
}

// postOutbox lets a logged-in user post an activity to their own outbox
// (ActivityPub client-to-server). It uses the browser session, so it is
// protected against cross-origin requests like the forms.
func (a *App) postOutbox(w http.ResponseWriter, r *http.Request, actor *store.Actor) {
	if actor.Username != r.PathValue("username") {
		http.Error(w, "this is not your outbox", http.StatusForbidden)
		return
	}
	a.ap.ServePostOutbox(w, r, actor)
}

// address is one entry in a note's to/cc list as shown on the page.
type address struct {
	Label string
	Pill  bool // shown as a pill: the public collection or the user's followers
}

var (
	//go:embed templates
	templateFS embed.FS
	//go:embed static
	staticFS embed.FS

	// homeTemplate renders templates/home.html, with the partials it uses.
	//   - noteHTML: note content that activitypub.PublishNote built from
	//     escaped text, so it is safe to render as HTML.
	//   - plainText: note HTML as plain text (activitypub.HTMLToText), escaped
	//     by html/template as usual. Inbox notes come from other servers, so
	//     they are only shown this way. For a local note it gives the text it
	//     was written from, for the edit form.
	//   - addresses: labels a to/cc list, naming the public collection and the
	//     user's followers collection.
	homeTemplate = template.Must(template.New("home.html").Funcs(template.FuncMap{
		"noteHTML":  func(s string) template.HTML { return template.HTML(s) }, //nolint:gosec // escaped at write time
		"plainText": activitypub.HTMLToText,
		"addresses": func(addrs []string, followersURI string) []address {
			out := make([]address, 0, len(addrs))
			for _, a := range addrs {
				switch a {
				case store.PublicAddress:
					out = append(out, address{"public", true})
				case followersURI:
					out = append(out, address{"followers", true})
				default:
					out = append(out, address{a, false})
				}
			}
			return out
		},
	}).ParseFS(templateFS, "templates/*.html", "templates/partials/*.html"))
)
