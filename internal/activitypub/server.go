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
	"slices"
	"strings"
	"time"
	"unicode"

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
	mux.HandleFunc("GET /notes", s.publicNotes)
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

// ParseAddresses splits a comma- or space-separated list of recipients into
// absolute http(s) IRIs, dropping duplicates. "public" and "as:Public" are
// accepted as shorthand for the public collection.
func ParseAddresses(field string) ([]string, error) {
	var out []string
	for _, f := range strings.FieldsFunc(field, func(r rune) bool { return r == ',' || unicode.IsSpace(r) }) {
		if strings.EqualFold(f, "public") || f == "as:Public" {
			f = store.PublicAddress
		}
		if u, err := url.Parse(f); err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
			return nil, fmt.Errorf("invalid address %q: must be an http(s) URL or \"public\"", f)
		}
		if !slices.Contains(out, f) {
			out = append(out, f)
		}
	}
	return out, nil
}

// FollowersURI returns the IRI of an actor's followers collection.
func FollowersURI(a *store.Actor) string {
	return a.ActorURI + "/followers"
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

// createActivity wraps one of a's notes in a Create.
func (s *Server) createActivity(a *store.Actor, note *vocab.Note) *vocab.Create {
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
	// Fetches aren't authenticated, so the outbox only holds public notes.
	notes, total, err := s.store.CollectionObjects(r.Context(), OutboxURI(a), outboxLimit)
	if err != nil {
		util.ServerError(w, err)
		return
	}
	items := make(vocab.ItemCollection, 0, len(notes))
	for _, it := range notes {
		if note, err := vocab.ToObject(it); err == nil {
			items = append(items, s.createActivity(a, note))
		}
	}
	writeAP(w, orderedCollection(vocab.IRI(OutboxURI(a)), total, items))
}

func (s *Server) followers(w http.ResponseWriter, r *http.Request) {
	a, ok := s.lookupActor(w, r, r.PathValue("username"))
	if !ok {
		return
	}
	followers, err := s.Followers(r.Context(), a)
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

// following lists the actors a local actor follows (accepted follows only).
func (s *Server) following(w http.ResponseWriter, r *http.Request) {
	a, ok := s.lookupActor(w, r, r.PathValue("username"))
	if !ok {
		return
	}
	following, err := collect(s.store.CollectionItems(r.Context(), FollowingURI(a)))
	if err != nil {
		util.ServerError(w, err)
		return
	}
	items := make(vocab.ItemCollection, 0, len(following))
	for _, f := range following {
		items = append(items, vocab.IRI(f))
	}
	writeAP(w, orderedCollection(vocab.IRI(a.ActorURI).AddPath("following"), len(items), items))
}

// ServeInbox writes the notes delivered to a's inbox, newest first, as an
// OrderedCollection. The caller must have checked that the request is from a.
func (s *Server) ServeInbox(w http.ResponseWriter, r *http.Request, a *store.Actor) {
	notes, err := s.store.ListInboxNotes(r.Context(), a.ID, outboxLimit)
	if err != nil {
		util.ServerError(w, err)
		return
	}
	items := make(vocab.ItemCollection, 0, len(notes))
	for _, n := range notes {
		note := vocab.ObjectNew(vocab.NoteType)
		note.ID = vocab.IRI(n.ObjectID)
		note.AttributedTo = vocab.IRI(n.AttributedTo)
		note.Content = vocab.DefaultNaturalLanguage(n.Content)
		note.Published = n.Published.UTC().Truncate(time.Second)
		items = append(items, note)
	}
	writeAP(w, orderedCollection(vocab.IRI(a.ActorURI).AddPath("inbox"), len(items), items))
}

// publicNotes lists the most recent public notes from all local actors.
func (s *Server) publicNotes(w http.ResponseWriter, r *http.Request) {
	notes, total, err := s.store.CollectionObjects(r.Context(), s.publicNotesID(), outboxLimit)
	if err != nil {
		util.ServerError(w, err)
		return
	}
	writeAP(w, orderedCollection(vocab.IRI(s.publicNotesID()), total, notes))
}

// note serves a single local note.
func (s *Server) note(w http.ResponseWriter, r *http.Request) {
	it, err := s.store.GetObject(r.Context(), s.noteID(r.PathValue("id")))
	if errors.Is(err, store.ErrNotFound) {
		http.NotFound(w, r)
		return
	} else if err != nil {
		util.ServerError(w, err)
		return
	}
	// Fetches aren't authenticated, so non-public notes are hidden.
	note, err := vocab.ToObject(it)
	if err != nil || note.Type != vocab.NoteType || !isPublic(note) {
		http.NotFound(w, r)
		return
	}
	writeAP(w, note)
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
