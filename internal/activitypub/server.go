// Package activitypub serves the federation side of sal-pub: WebFinger,
// actor documents, outboxes, collections, notes and the signed inbox.
package activitypub

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"log"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	vocab "github.com/go-ap/activitypub"
	"github.com/go-ap/jsonld"

	"github.com/cgs-earth/sal-pub/internal/httpsig"
	"github.com/cgs-earth/sal-pub/internal/store"
	"github.com/cgs-earth/sal-pub/pkg/util"
)

const (
	contentType = "application/activity+json"
	outboxLimit = 50
)

type Server struct {
	store   *store.Store
	baseURL string // e.g. https://sal.example.org, no trailing slash
	domain  string // host[:port] used in acct: handles
	client  *http.Client
}

// New returns a Server whose actor and object URIs live under baseURL.
func New(s *store.Store, baseURL string) (*Server, error) {
	u, err := url.Parse(baseURL)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
		return nil, fmt.Errorf("invalid base URL %q", baseURL)
	}
	return &Server{
		store:   s,
		baseURL: strings.TrimRight(baseURL, "/"),
		domain:  u.Host,
		client:  &http.Client{Timeout: 10 * time.Second},
	}, nil
}

// Register adds the ActivityPub routes to mux.
func (s *Server) Register(mux *http.ServeMux) {
	mux.HandleFunc("GET /.well-known/webfinger", s.webfinger)
	mux.HandleFunc("GET /users/{username}", s.actor)
	mux.HandleFunc("POST /users/{username}/inbox", s.inbox)
	mux.HandleFunc("GET /users/{username}/outbox", s.outbox)
	mux.HandleFunc("GET /users/{username}/followers", s.followers)
	mux.HandleFunc("GET /users/{username}/following", s.following)
	mux.HandleFunc("GET /notes/{id}", s.note)
}

// Handle returns the fediverse handle for a username, e.g. @alice@example.org.
func (s *Server) Handle(username string) string {
	return "@" + username + "@" + s.domain
}

// EnsureActor returns the actor for a Keycloak subject, creating it (with a
// fresh signing key) on first login. The username is derived once from the
// preferred username and never changes, because it is part of the actor URI.
func (s *Server) EnsureActor(ctx context.Context, subject, preferredUsername, displayName string) (*store.Actor, error) {
	a, err := s.store.GetBySubject(ctx, subject)
	if err == nil {
		if displayName != "" && displayName != a.DisplayName {
			if err := s.store.UpdateDisplayName(ctx, a.ID, displayName); err != nil {
				return nil, err
			}
			a.DisplayName = displayName
		}
		return a, nil
	}
	if !errors.Is(err, store.ErrNotFound) {
		return nil, err
	}

	privatePEM, publicPEM, err := httpsig.GenerateKey()
	if err != nil {
		return nil, err
	}

	base := sanitizeUsername(preferredUsername)
	for i := 1; i <= 50; i++ {
		username := base
		if i > 1 {
			username = fmt.Sprintf("%s_%d", base, i)
		}
		actorURI := s.baseURL + "/users/" + username
		a, err := s.store.CreateActor(ctx, &store.Actor{
			KeycloakSubject: subject,
			Username:        username,
			DisplayName:     displayName,
			ActorURI:        actorURI,
			PublicKeyID:     actorURI + "#main-key",
			PublicKeyPEM:    publicPEM,
			PrivateKeyPEM:   privatePEM,
		})
		if errors.Is(err, store.ErrUsernameTaken) {
			continue
		}
		if err != nil {
			return nil, err
		}
		log.Printf("created actor %s for subject %s", a.ActorURI, subject)
		return a, nil
	}
	return nil, fmt.Errorf("no free username for %q", base)
}

var invalidUsernameChars = regexp.MustCompile(`[^a-z0-9_]+`)

// sanitizeUsername turns a Keycloak username (often an email address for
// brokered logins) into something Mastodon accepts: [a-z0-9_], max 30 chars.
func sanitizeUsername(name string) string {
	name = strings.ToLower(name)
	name, _, _ = strings.Cut(name, "@")
	name = strings.Trim(invalidUsernameChars.ReplaceAllString(name, "_"), "_")
	if len(name) > 26 { // leave room for a _NN suffix
		name = name[:26]
	}
	if name == "" {
		name = "user"
	}
	return name
}

// PublishNote stores a public note and delivers it to the actor's followers.
func (s *Server) PublishNote(ctx context.Context, a *store.Actor, text string) (*store.Note, error) {
	n, err := s.store.CreateNote(ctx, a.ID, textToHTML(text))
	if err != nil {
		return nil, err
	}

	payload, err := marshal(s.createActivity(a, n))
	if err != nil {
		return nil, fmt.Errorf("encode Create: %w", err)
	}
	go s.deliverToFollowers(a, payload)
	return n, nil
}

func textToHTML(text string) string {
	var paragraphs []string
	for _, p := range strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n\n") {
		if p = strings.TrimSpace(p); p != "" {
			paragraphs = append(paragraphs, "<p>"+strings.ReplaceAll(html.EscapeString(p), "\n", "<br>")+"</p>")
		}
	}
	return strings.Join(paragraphs, "")
}

// ---------------------------------------------------------------------------
// Documents
// ---------------------------------------------------------------------------

func (s *Server) noteURI(n *store.Note) vocab.IRI {
	return vocab.IRI(s.baseURL + "/notes/" + n.ID)
}

func (s *Server) actorDocument(a *store.Actor) *vocab.Person {
	id := vocab.IRI(a.ActorURI)
	p := vocab.PersonNew(id)
	p.PreferredUsername = vocab.DefaultNaturalLanguage(a.Username)
	if a.DisplayName != "" {
		p.Name = vocab.DefaultNaturalLanguage(a.DisplayName)
	}
	p.Inbox = id.AddPath("inbox")
	p.Outbox = id.AddPath("outbox")
	p.Followers = id.AddPath("followers")
	p.Following = id.AddPath("following")
	p.PublicKey = vocab.PublicKey{
		ID:           vocab.IRI(a.PublicKeyID),
		Owner:        id,
		PublicKeyPem: a.PublicKeyPEM,
	}
	return p
}

func (s *Server) noteObject(a *store.Actor, n *store.Note) *vocab.Note {
	note := vocab.ObjectNew(vocab.NoteType)
	note.ID = s.noteURI(n)
	note.AttributedTo = vocab.IRI(a.ActorURI)
	note.Content = vocab.DefaultNaturalLanguage(n.Content)
	note.Published = n.Published.UTC().Truncate(time.Second)
	note.To = vocab.ItemCollection{vocab.PublicNS}
	note.CC = vocab.ItemCollection{vocab.IRI(a.ActorURI).AddPath("followers")}
	return note
}

func (s *Server) createActivity(a *store.Actor, n *store.Note) *vocab.Create {
	note := s.noteObject(a, n)
	create := vocab.CreateNew(note.ID+"#create", note)
	create.Actor = vocab.IRI(a.ActorURI)
	create.Published = note.Published
	create.To = note.To
	create.CC = note.CC
	return create
}

func orderedCollection(id vocab.IRI, total int, items vocab.ItemCollection) *vocab.OrderedCollection {
	col := vocab.OrderedCollectionNew(id)
	col.TotalItems = uint(total)
	col.OrderedItems = items
	return col
}

// marshal encodes an item with the ActivityStreams and security contexts.
func marshal(it vocab.Item) ([]byte, error) {
	return jsonld.WithContext(
		jsonld.IRI(vocab.ActivityBaseURI),
		jsonld.IRI(vocab.SecurityContextURI),
	).Marshal(it)
}

// ---------------------------------------------------------------------------
// Handlers
// ---------------------------------------------------------------------------

func (s *Server) webfinger(w http.ResponseWriter, r *http.Request) {
	resource := r.URL.Query().Get("resource")

	var username string
	if acct, ok := strings.CutPrefix(resource, "acct:"); ok {
		user, domain, ok := strings.Cut(strings.TrimPrefix(acct, "@"), "@")
		if !ok || !strings.EqualFold(domain, s.domain) {
			http.NotFound(w, r)
			return
		}
		username = user
	} else if user, ok := strings.CutPrefix(resource, s.baseURL+"/users/"); ok {
		username = user
	} else {
		http.Error(w, "invalid resource", http.StatusBadRequest)
		return
	}

	a, ok := s.lookupActor(w, r, strings.ToLower(username))
	if !ok {
		return
	}
	data, err := json.Marshal(map[string]any{
		"subject": "acct:" + a.Username + "@" + s.domain,
		"aliases": []string{a.ActorURI},
		"links": []map[string]string{
			{"rel": "self", "type": contentType, "href": a.ActorURI},
		},
	})
	if err != nil {
		util.ServerError(w, fmt.Errorf("encode response: %w", err))
		return
	}
	w.Header().Set("Content-Type", "application/jrd+json")
	w.Write(data)
}

func (s *Server) actor(w http.ResponseWriter, r *http.Request) {
	a, ok := s.lookupActor(w, r, r.PathValue("username"))
	if !ok {
		return
	}
	writeAP(w, s.actorDocument(a))
}

func (s *Server) outbox(w http.ResponseWriter, r *http.Request) {
	a, ok := s.lookupActor(w, r, r.PathValue("username"))
	if !ok {
		return
	}
	notes, total, err := s.store.ListNotes(r.Context(), a.ID, outboxLimit)
	if err != nil {
		util.ServerError(w, err)
		return
	}
	items := make(vocab.ItemCollection, 0, len(notes))
	for i := range notes {
		items = append(items, s.createActivity(a, &notes[i]))
	}
	writeAP(w, orderedCollection(vocab.IRI(a.ActorURI).AddPath("outbox"), total, items))
}

func (s *Server) followers(w http.ResponseWriter, r *http.Request) {
	a, ok := s.lookupActor(w, r, r.PathValue("username"))
	if !ok {
		return
	}
	followers, err := s.store.Followers(r.Context(), a.ID)
	if err != nil {
		util.ServerError(w, err)
		return
	}
	items := make(vocab.ItemCollection, 0, len(followers))
	for _, f := range followers {
		items = append(items, vocab.IRI(f))
	}
	writeAP(w, orderedCollection(vocab.IRI(a.ActorURI).AddPath("followers"), len(items), items))
}

// following is always empty: local actors can't follow anyone yet.
func (s *Server) following(w http.ResponseWriter, r *http.Request) {
	a, ok := s.lookupActor(w, r, r.PathValue("username"))
	if !ok {
		return
	}
	writeAP(w, orderedCollection(vocab.IRI(a.ActorURI).AddPath("following"), 0, nil))
}

var uuidPattern = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

func (s *Server) note(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if !uuidPattern.MatchString(id) {
		http.NotFound(w, r)
		return
	}
	n, a, err := s.store.GetNote(r.Context(), id)
	if errors.Is(err, store.ErrNotFound) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		util.ServerError(w, err)
		return
	}
	writeAP(w, s.noteObject(a, n))
}

func (s *Server) lookupActor(w http.ResponseWriter, r *http.Request, username string) (*store.Actor, bool) {
	a, err := s.store.GetByUsername(r.Context(), username)
	if errors.Is(err, store.ErrNotFound) {
		http.NotFound(w, r)
		return nil, false
	}
	if err != nil {
		util.ServerError(w, err)
		return nil, false
	}
	return a, true
}

func writeAP(w http.ResponseWriter, it vocab.Item) {
	data, err := marshal(it)
	if err != nil {
		util.ServerError(w, fmt.Errorf("encode response: %w", err))
		return
	}
	w.Header().Set("Content-Type", contentType)
	w.Write(data)
}
