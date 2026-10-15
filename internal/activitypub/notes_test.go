package activitypub

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/cgs-earth/sal-pub/internal/store"
)

// These tests need Postgres (see crudServer). Notes are addressed only to the
// public collection, so delivery has nothing to send.

// publish publishes a note as a and returns its {id}.
func publish(t *testing.T, s *Server, a *store.Actor, text string, to []string) string {
	t.Helper()
	note, err := s.PublishNote(context.Background(), a, text, to, nil)
	if err != nil {
		t.Fatal(err)
	}
	return s.noteView(note).ID
}

func TestPublishNoteListsPublicNoteEverywhere(t *testing.T) {
	s, a := crudServer(t), actorNamed("alice")
	id := publish(t, s, a, "hello", []string{store.PublicAddress})

	notes, total, err := s.Notes(context.Background(), a, 20)
	if err != nil || total != 1 || len(notes) != 1 || notes[0].ID != id || notes[0].Content != "<p>hello</p>" {
		t.Fatalf("Notes = %+v, %d, %v", notes, total, err)
	}
	if in, _ := s.store.CollectionContains(context.Background(), OutboxURI(a), s.noteID(id)); !in {
		t.Error("a public note should be in the outbox")
	}
	public, err := s.PublicNotes(context.Background(), 50)
	if err != nil || len(public) == 0 || public[0].ID != id || public[0].Author.ActorURI != a.ActorURI {
		t.Errorf("PublicNotes = %+v, %v", public, err)
	}
}

func TestPublishNoteKeepsNonPublicNoteOutOfOutbox(t *testing.T) {
	s, a := crudServer(t), actorNamed("alice")
	id := publish(t, s, a, "secret", []string{a.ActorURI})

	if in, _ := s.store.CollectionContains(context.Background(), OutboxURI(a), s.noteID(id)); in {
		t.Error("a non-public note must not be in the outbox")
	}
	r := httptest.NewRequest(http.MethodGet, "/notes/"+id, nil)
	r.SetPathValue("id", id)
	w := httptest.NewRecorder()
	s.note(w, r)
	if w.Code != http.StatusNotFound {
		t.Errorf("GET /notes/{id} = %d, want 404", w.Code)
	}
}

func TestEditNoteReplacesContent(t *testing.T) {
	s, a := crudServer(t), actorNamed("alice")
	id := publish(t, s, a, "old", []string{store.PublicAddress})

	if _, err := s.EditNote(context.Background(), a, id, "new"); err != nil {
		t.Fatal(err)
	}
	notes, _, err := s.Notes(context.Background(), a, 20)
	if err != nil || len(notes) != 1 || notes[0].Content != "<p>new</p>" || notes[0].Updated.IsZero() {
		t.Errorf("Notes = %+v, %v", notes, err)
	}
}

func TestEditNoteByOtherActorReturnsErrNotFound(t *testing.T) {
	s := crudServer(t)
	id := publish(t, s, actorNamed("alice"), "mine", []string{store.PublicAddress})
	if _, err := s.EditNote(context.Background(), actorNamed("bob"), id, "theirs"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("err = %v, want store.ErrNotFound", err)
	}
}

func TestDeleteNoteRemovesItEverywhere(t *testing.T) {
	s, a := crudServer(t), actorNamed("alice")
	id := publish(t, s, a, "bye", []string{store.PublicAddress})

	if err := s.DeleteNote(context.Background(), actorNamed("bob"), id); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("delete by other actor: err = %v, want store.ErrNotFound", err)
	}
	if err := s.DeleteNote(context.Background(), a, id); err != nil {
		t.Fatal(err)
	}
	if _, err := s.store.GetObject(context.Background(), s.noteID(id)); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("GetObject after delete: err = %v, want store.ErrNotFound", err)
	}
	for _, c := range []string{notesID(a), OutboxURI(a), s.publicNotesID()} {
		if in, _ := s.store.CollectionContains(context.Background(), c, s.noteID(id)); in {
			t.Errorf("deleted note still in %s", c)
		}
	}
}
