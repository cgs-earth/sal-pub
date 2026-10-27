package activitypub

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	vocab "github.com/go-ap/activitypub"

	"github.com/cgs-earth/sal-pub/internal/store"
)

// Notes published by local actors live in the generic object store, as Note
// objects with ids <base>/notes/{id}, and are listed in collections:
//
//   - <actor>#notes: private collection of all of the actor's notes.
//   - <actor>/outbox: the actor's public notes (served as Create activities).
//   - <base>/notes: public notes from all local actors.
//   - <parent>/replies: for a public reply to a local note or object.

// OutboxURI returns the IRI of an actor's outbox.
func OutboxURI(a *store.Actor) string {
	return a.ActorURI + "/outbox"
}

func notesID(a *store.Actor) string {
	return a.ActorURI + "#notes"
}

func (s *Server) publicNotesID() string {
	return s.baseURL + "/notes"
}

// NoteURI returns the IRI of the local note /notes/{id}.
func (s *Server) NoteURI(id string) string {
	return s.baseURL + "/notes/" + id
}

// noteCollections returns the collections one of a's notes is listed in.
func (s *Server) noteCollections(a *store.Actor, note *vocab.Note) []string {
	if !isPublic(note) {
		return []string{notesID(a)}
	}
	collections := []string{notesID(a), OutboxURI(a), s.publicNotesID()}
	if !vocab.IsNil(note.InReplyTo) && s.isLocalObject(note.InReplyTo.GetLink().String()) {
		collections = append(collections, note.InReplyTo.GetLink().String()+"/replies")
	}
	return collections
}

func isPublic(note *vocab.Note) bool {
	return note.To.Contains(vocab.PublicNS) || note.CC.Contains(vocab.PublicNS)
}

// Note is a local note as shown on the home page.
type Note struct {
	ID           string // the {id} in /notes/{id}
	URI          string // the note's full id
	AttributedTo string // the author's actor URI
	Content      string // HTML built by textToHTML
	Published    time.Time
	Updated      time.Time // zero if the note was never edited
	To, CC       []string
	InReplyTo    string       // the id of the object it replies to, if any
	Likes        string       // /notes/{id}/likes
	Shares       string       // /notes/{id}/shares
	Replies      string       // /notes/{id}/replies
	Author       *store.Actor // set by PublicNotes and for local replies

	// Set by Notes and PublicNotes (see addInteractions).
	LikeCount  int
	Liked      bool   // the viewer likes it
	CanReact   bool   // there is a viewer, who can like and reply
	Mine       bool   // the viewer wrote it, and can edit and delete it
	ReplyNotes []Note // public replies, oldest first, with their own replies; remote ones only have URI
}

// noteView converts a stored Note object for the home page.
func (s *Server) noteView(note *vocab.Note) Note {
	n := Note{
		ID:        note.ID.String()[len(s.NoteURI("")):],
		URI:       note.ID.String(),
		Published: note.Published,
		Updated:   note.Updated,
		Likes:     note.ID.String() + "/likes",
		Shares:    note.ID.String() + "/shares",
		Replies:   note.ID.String() + "/replies",
	}
	if !vocab.IsNil(note.AttributedTo) {
		n.AttributedTo = note.AttributedTo.GetLink().String()
	}
	if !vocab.IsNil(note.InReplyTo) {
		n.InReplyTo = note.InReplyTo.GetLink().String()
	}
	if c := note.Content.First(); c != nil {
		n.Content = c.String()
	}
	for _, it := range note.To {
		n.To = append(n.To, it.GetLink().String())
	}
	for _, it := range note.CC {
		n.CC = append(n.CC, it.GetLink().String())
	}
	return n
}

// Notes returns up to limit of a's notes, newest first, and how many a has.
// Public replies to a's own notes are only in those notes' replies (see
// isNested), so fewer than limit may be returned.
func (s *Server) Notes(ctx context.Context, a *store.Actor, limit int) ([]Note, int, error) {
	items, total, err := s.store.CollectionObjects(ctx, notesID(a), limit)
	if err != nil {
		return nil, 0, err
	}
	notes := make([]Note, 0, len(items))
	authors := map[string]*store.Actor{a.ActorURI: a}
	for _, it := range items {
		note, err := vocab.ToObject(it)
		if err != nil {
			continue
		}
		if nested, err := s.isNested(ctx, note, notesID(a)); err != nil {
			return nil, 0, err
		} else if nested {
			continue
		}
		n := s.noteView(note)
		if err := s.addInteractions(ctx, &n, a, authors); err != nil {
			return nil, 0, err
		}
		notes = append(notes, n)
	}
	return notes, total, nil
}

// isNested reports whether note is left out of the list of collectionID and
// shown in its parent's replies instead: it is a public reply to a note in
// that collection.
func (s *Server) isNested(ctx context.Context, note *vocab.Note, collectionID string) (bool, error) {
	if vocab.IsNil(note.InReplyTo) || !isPublic(note) {
		return false, nil
	}
	return s.store.CollectionContains(ctx, collectionID, note.InReplyTo.GetLink().String())
}

// author returns the local actor with actor URI uri, or an Actor with only
// that URI if it isn't local. Lookups are cached in authors.
func (s *Server) author(ctx context.Context, uri string, authors map[string]*store.Actor) (*store.Actor, error) {
	if authors[uri] == nil {
		author, err := s.store.GetByActorURI(ctx, uri)
		if errors.Is(err, store.ErrNotFound) {
			author = &store.Actor{ActorURI: uri}
		} else if err != nil {
			return nil, err
		}
		authors[uri] = author
	}
	return authors[uri], nil
}

// addInteractions fills in n's like count, its public replies and, when
// viewer isn't nil, whether viewer likes or wrote it. Local replies are
// loaded with their authors (cached in authors) and their own interactions,
// so the whole thread below n is filled in; remote ones only get their URI.
func (s *Server) addInteractions(ctx context.Context, n *Note, viewer *store.Actor, authors map[string]*store.Actor) error {
	var err error
	if n.LikeCount, err = s.store.CollectionCount(ctx, n.Likes); err != nil {
		return err
	}
	if viewer != nil {
		n.CanReact = true
		n.Mine = n.AttributedTo == viewer.ActorURI
		if n.Liked, err = s.store.CollectionContains(ctx, n.Likes, viewer.ActorURI); err != nil {
			return err
		}
	}
	for id, err := range s.store.CollectionItems(ctx, n.Replies) {
		if err != nil {
			return err
		}
		reply := Note{URI: id}
		if strings.HasPrefix(id, s.NoteURI("")) {
			it, err := s.store.GetObject(ctx, id)
			if err != nil && !errors.Is(err, store.ErrNotFound) {
				return err
			}
			if note, err := vocab.ToObject(it); err == nil {
				reply = s.noteView(note)
				if reply.Author, err = s.author(ctx, reply.AttributedTo, authors); err != nil {
					return err
				}
				if err := s.addInteractions(ctx, &reply, viewer, authors); err != nil {
					return err
				}
			}
		}
		n.ReplyNotes = append(n.ReplyNotes, reply)
	}
	slices.Reverse(n.ReplyNotes)
	return nil
}

// Liked returns which of the objects ids viewer has liked.
func (s *Server) Liked(ctx context.Context, viewer *store.Actor, ids []string) (map[string]bool, error) {
	liked := map[string]bool{}
	for _, id := range ids {
		_, err := s.store.GetObject(ctx, reactionID(viewer, vocab.LikeType, id).String())
		if err == nil {
			liked[id] = true
		} else if !errors.Is(err, store.ErrNotFound) {
			return nil, err
		}
	}
	return liked, nil
}

// PublicNotes returns up to limit public notes from all local actors, newest
// first, with their authors and interactions. viewer is the logged-in actor,
// or nil. Replies to public local notes are only in those notes' replies (see
// isNested), so fewer than limit may be returned.
func (s *Server) PublicNotes(ctx context.Context, limit int, viewer *store.Actor) ([]Note, error) {
	items, _, err := s.store.CollectionObjects(ctx, s.publicNotesID(), limit)
	if err != nil {
		return nil, err
	}
	authors := map[string]*store.Actor{}
	notes := make([]Note, 0, len(items))
	for _, it := range items {
		note, err := vocab.ToObject(it)
		if err != nil || vocab.IsNil(note.AttributedTo) {
			continue
		}
		if nested, err := s.isNested(ctx, note, s.publicNotesID()); err != nil {
			return nil, err
		} else if nested {
			continue
		}
		n := s.noteView(note)
		if n.Author, err = s.author(ctx, n.AttributedTo, authors); err != nil {
			return nil, err
		}
		if err := s.addInteractions(ctx, &n, viewer, authors); err != nil {
			return nil, err
		}
		notes = append(notes, n)
	}
	return notes, nil
}

// PublishNote stores a note addressed to the given to/cc IRIs, in reply to
// inReplyTo unless that is empty, and delivers it to their inboxes.
func (s *Server) PublishNote(ctx context.Context, a *store.Actor, text string, to, cc []string, inReplyTo string) (*vocab.Note, error) {
	note := vocab.ObjectNew(vocab.NoteType)
	note.ID = vocab.IRI(s.NoteURI(rand.Text()))
	setInteractionCollections(note)
	note.AttributedTo = vocab.IRI(a.ActorURI)
	if inReplyTo != "" {
		note.InReplyTo = vocab.IRI(inReplyTo)
	}
	note.Content = vocab.DefaultNaturalLanguage(textToHTML(text))
	note.Published = time.Now().UTC().Truncate(time.Second)
	for _, iri := range to {
		note.To = append(note.To, vocab.IRI(iri))
	}
	for _, iri := range cc {
		note.CC = append(note.CC, vocab.IRI(iri))
	}
	if err := s.store.CreateObject(ctx, note); err != nil {
		return nil, err
	}
	for _, c := range s.noteCollections(a, note) {
		if err := s.store.AddToCollection(ctx, c, note.ID.String()); err != nil {
			return nil, err
		}
	}

	payload, err := marshal(s.createActivity(a, note))
	if err != nil {
		return nil, fmt.Errorf("encode Create: %w", err)
	}
	go s.deliverNote(a, note, payload, true)
	return note, nil
}
