package activitypub

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
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

func (s *Server) noteID(id string) string {
	return s.baseURL + "/notes/" + id
}

// noteCollections returns the collections one of a's notes is listed in.
func (s *Server) noteCollections(a *store.Actor, note *vocab.Note) []string {
	if isPublic(note) {
		return []string{notesID(a), OutboxURI(a), s.publicNotesID()}
	}
	return []string{notesID(a)}
}

func isPublic(note *vocab.Note) bool {
	return note.To.Contains(vocab.PublicNS) || note.CC.Contains(vocab.PublicNS)
}

// Note is a local note as shown on the home page.
type Note struct {
	ID        string // the {id} in /notes/{id}
	Content   string // HTML built by textToHTML
	Published time.Time
	Updated   time.Time // zero if the note was never edited
	To, CC    []string
	Author    *store.Actor // set by PublicNotes only
}

// noteView converts a stored Note object for the home page.
func (s *Server) noteView(note *vocab.Note) Note {
	n := Note{
		ID:        note.ID.String()[len(s.noteID("")):],
		Published: note.Published,
		Updated:   note.Updated,
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
func (s *Server) Notes(ctx context.Context, a *store.Actor, limit int) ([]Note, int, error) {
	items, total, err := s.store.CollectionObjects(ctx, notesID(a), limit)
	if err != nil {
		return nil, 0, err
	}
	notes := make([]Note, 0, len(items))
	for _, it := range items {
		if note, err := vocab.ToObject(it); err == nil {
			notes = append(notes, s.noteView(note))
		}
	}
	return notes, total, nil
}

// PublicNotes returns up to limit public notes from all local actors, newest
// first, with their authors.
func (s *Server) PublicNotes(ctx context.Context, limit int) ([]Note, error) {
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
		uri := note.AttributedTo.GetLink().String()
		if authors[uri] == nil {
			author, err := s.store.GetByActorURI(ctx, uri)
			if errors.Is(err, store.ErrNotFound) {
				author = &store.Actor{ActorURI: uri}
			} else if err != nil {
				return nil, err
			}
			authors[uri] = author
		}
		n := s.noteView(note)
		n.Author = authors[uri]
		notes = append(notes, n)
	}
	return notes, nil
}

// PublishNote stores a note addressed to the given to/cc IRIs and delivers it
// to their inboxes.
func (s *Server) PublishNote(ctx context.Context, a *store.Actor, text string, to, cc []string) (*vocab.Note, error) {
	note := vocab.ObjectNew(vocab.NoteType)
	note.ID = vocab.IRI(s.noteID(rand.Text()))
	note.AttributedTo = vocab.IRI(a.ActorURI)
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
	go s.deliverNote(a, note, payload)
	return note, nil
}
