package activitypub

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/cgs-earth/sal-pub/internal/store"
)

// These tests need Postgres (see crudServer). Notes are addressed only to the
// public collection, so delivery has nothing to send.

// publish publishes a note as a and returns its {id}.
func publish(t *testing.T, s *Server, a *store.Actor, text string, to []string) string {
	t.Helper()
	note, err := s.PublishNote(context.Background(), a, text, to, nil)
	require.NoError(t, err)
	return s.noteView(note).ID
}

func TestPublishNoteListsPublicNoteEverywhere(t *testing.T) {
	s, a := crudServer(t), actorNamed("alice")
	id := publish(t, s, a, "hello", []string{store.PublicAddress})

	notes, total, err := s.Notes(context.Background(), a, 20)
	require.NoError(t, err)
	assert.Equal(t, 1, total)
	require.Len(t, notes, 1)
	assert.Equal(t, id, notes[0].ID)
	assert.Equal(t, "<p>hello</p>", notes[0].Content)

	in, err := s.store.CollectionContains(context.Background(), OutboxURI(a), s.noteID(id))
	require.NoError(t, err)
	assert.True(t, in, "a public note should be in the outbox")

	public, err := s.PublicNotes(context.Background(), 50)
	require.NoError(t, err)
	require.NotEmpty(t, public)
	assert.Equal(t, id, public[0].ID)
	assert.Equal(t, a.ActorURI, public[0].Author.ActorURI)
}

func TestPublishNoteKeepsNonPublicNoteOutOfOutbox(t *testing.T) {
	s, a := crudServer(t), actorNamed("alice")
	id := publish(t, s, a, "secret", []string{a.ActorURI})

	in, err := s.store.CollectionContains(context.Background(), OutboxURI(a), s.noteID(id))
	require.NoError(t, err)
	assert.False(t, in, "a non-public note must not be in the outbox")

	r := httptest.NewRequest(http.MethodGet, "/notes/"+id, nil)
	r.SetPathValue("id", id)
	w := httptest.NewRecorder()
	s.note(w, r)
	assert.Equal(t, http.StatusNotFound, w.Code, "GET /notes/{id}")
}

func TestEditNoteReplacesContent(t *testing.T) {
	s, a := crudServer(t), actorNamed("alice")
	id := publish(t, s, a, "old", []string{store.PublicAddress})

	_, err := s.EditNote(context.Background(), a, id, "new")
	require.NoError(t, err)
	notes, _, err := s.Notes(context.Background(), a, 20)
	require.NoError(t, err)
	require.Len(t, notes, 1)
	assert.Equal(t, "<p>new</p>", notes[0].Content)
	assert.False(t, notes[0].Updated.IsZero(), "an edited note should have an updated time")
}

func TestEditNoteByOtherActorReturnsErrNotFound(t *testing.T) {
	s := crudServer(t)
	id := publish(t, s, actorNamed("alice"), "mine", []string{store.PublicAddress})
	_, err := s.EditNote(context.Background(), actorNamed("bob"), id, "theirs")
	assert.ErrorIs(t, err, store.ErrNotFound)
}

func TestDeleteNoteRemovesItEverywhere(t *testing.T) {
	s, a := crudServer(t), actorNamed("alice")
	id := publish(t, s, a, "bye", []string{store.PublicAddress})

	assert.ErrorIs(t, s.DeleteNote(context.Background(), actorNamed("bob"), id), store.ErrNotFound, "delete by other actor")
	require.NoError(t, s.DeleteNote(context.Background(), a, id))
	_, err := s.store.GetObject(context.Background(), s.noteID(id))
	assert.ErrorIs(t, err, store.ErrNotFound, "GetObject after delete")
	for _, c := range []string{notesID(a), OutboxURI(a), s.publicNotesID()} {
		in, err := s.store.CollectionContains(context.Background(), c, s.noteID(id))
		require.NoError(t, err)
		assert.False(t, in, "deleted note still in %s", c)
	}
}
