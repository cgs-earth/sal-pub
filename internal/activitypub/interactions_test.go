package activitypub

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	pathpkg "path"
	"testing"

	vocab "github.com/go-ap/activitypub"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/cgs-earth/sal-pub/internal/store"
	"github.com/cgs-earth/sal-pub/pkg/util"
)

func TestReactionIDIsPerActorTypeAndObject(t *testing.T) {
	a := testActor()
	like := reactionID(a, vocab.LikeType, "https://remote.example/notes/1")
	assert.Equal(t, like, reactionID(a, vocab.LikeType, "https://remote.example/notes/1"))
	assert.NotEqual(t, like, reactionID(a, vocab.AnnounceType, "https://remote.example/notes/1"))
	assert.NotEqual(t, like, reactionID(a, vocab.LikeType, "https://remote.example/notes/2"))
}

func TestCountReactionIgnoresRemoteObject(t *testing.T) {
	like := vocab.LikeNew("", vocab.IRI("https://remote.example/notes/1"))
	assert.NoError(t, noStoreServer(t).countReaction(context.Background(), "https://remote.example/users/bob", like, true))
}

func TestServeInteractionsRejectsUnknownCollection(t *testing.T) {
	w := getInteractions(noStoreServer(t), "/notes/x/bookmarks", nil)
	assert.Equal(t, http.StatusNotFound, w.Code)
}

func TestPostOutboxRejectsReplyToBadURL(t *testing.T) {
	note := publicNote("hi")
	note.InReplyTo = vocab.IRI("ftp://x.example/1")
	_, err := noStoreServer(t).PostOutbox(context.Background(), testActor(), note)
	assert.ErrorIs(t, err, util.ErrBadRequest)
}

// The tests below need Postgres (see crudServer).

// getInteractions requests one of an object's interaction collections at path.
func getInteractions(s *Server, path string, viewer *store.Actor) *httptest.ResponseRecorder {
	r := httptest.NewRequest(http.MethodGet, path, nil)
	r.SetPathValue("collection", pathpkg.Base(path))
	w := httptest.NewRecorder()
	s.ServeInteractions(w, r, viewer)
	return w
}

// collectionOf decodes an interaction collection response.
func collectionOf(t *testing.T, w *httptest.ResponseRecorder) (total int, items []string) {
	t.Helper()
	require.Equal(t, http.StatusOK, w.Code, "%s", w.Body)
	var col struct {
		TotalItems int      `json:"totalItems"`
		Items      []string `json:"items"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &col))
	return col.TotalItems, col.Items
}

func TestLikeLocalNoteIsCountedOnceAndUndone(t *testing.T) {
	s := crudServer(t)
	ctx := context.Background()
	alice, bob := actorNamed("alice"), actorNamed("bob")
	id := publish(t, s, alice, "hello", []string{store.PublicAddress})
	likes := "/notes/" + id + "/likes"

	_, err := s.PostOutbox(ctx, bob, vocab.LikeNew("", vocab.IRI(s.NoteURI(id))))
	require.NoError(t, err)
	_, err = s.PostOutbox(ctx, bob, vocab.LikeNew("", vocab.IRI(s.NoteURI(id))))
	require.NoError(t, err)
	total, items := collectionOf(t, getInteractions(s, likes, nil))
	assert.Equal(t, 1, total)
	assert.Empty(t, items, "likers are not published")

	_, err = s.PostOutbox(ctx, bob, vocab.UndoNew("", vocab.LikeNew("", vocab.IRI(s.NoteURI(id)))))
	require.NoError(t, err)
	total, _ = collectionOf(t, getInteractions(s, likes, nil))
	assert.Equal(t, 0, total)
}

func TestUndoLikeByID(t *testing.T) {
	s := crudServer(t)
	ctx := context.Background()
	bob := actorNamed("bob")
	id := publish(t, s, actorNamed("alice"), "hello", []string{store.PublicAddress})
	like, err := s.PostOutbox(ctx, bob, vocab.LikeNew("", vocab.IRI(s.NoteURI(id))))
	require.NoError(t, err)

	_, err = s.PostOutbox(ctx, actorNamed("mallory"), vocab.UndoNew("", like.ID))
	assert.ErrorIs(t, err, util.ErrForbidden, "someone else's Like")
	_, err = s.PostOutbox(ctx, bob, vocab.UndoNew("", like.ID))
	require.NoError(t, err)
	total, _ := collectionOf(t, getInteractions(s, "/notes/"+id+"/likes", nil))
	assert.Equal(t, 0, total)
}

func TestShareLocalNoteIsCounted(t *testing.T) {
	s := crudServer(t)
	id := publish(t, s, actorNamed("alice"), "hello", []string{store.PublicAddress})
	share, err := s.PostOutbox(context.Background(), actorNamed("bob"), vocab.AnnounceNew("", vocab.IRI(s.NoteURI(id))))
	require.NoError(t, err)
	assert.True(t, share.To.Contains(vocab.PublicNS), "an Announce is public")
	total, _ := collectionOf(t, getInteractions(s, "/notes/"+id+"/shares", nil))
	assert.Equal(t, 1, total)
}

func TestShareNonPublicObjectIsRejected(t *testing.T) {
	s := crudServer(t)
	alice := actorNamed("alice")
	id := createNote(t, s, alice, `{"type":"Note","content":"private"}`)
	_, err := s.PostOutbox(context.Background(), alice, vocab.AnnounceNew("", vocab.IRI(s.objectURI(id))))
	assert.ErrorIs(t, err, util.ErrBadRequest)
}

func TestLikeInvisibleObjectIsNotFound(t *testing.T) {
	s := crudServer(t)
	id := createNote(t, s, actorNamed("alice"), `{"type":"Note","content":"private"}`)
	_, err := s.PostOutbox(context.Background(), actorNamed("bob"), vocab.LikeNew("", vocab.IRI(s.objectURI(id))))
	assert.ErrorIs(t, err, store.ErrNotFound)
}

func TestReplyToLocalNoteIsListedUntilDeleted(t *testing.T) {
	s := crudServer(t)
	ctx := context.Background()
	alice, bob := actorNamed("alice"), actorNamed("bob")
	parent := publish(t, s, alice, "question", []string{store.PublicAddress})
	reply, err := s.PublishNote(ctx, bob, "answer", []string{store.PublicAddress}, nil, s.NoteURI(parent))
	require.NoError(t, err)
	assert.Equal(t, s.NoteURI(parent), reply.InReplyTo.GetLink().String())

	total, items := collectionOf(t, getInteractions(s, "/notes/"+parent+"/replies", nil))
	assert.Equal(t, 1, total)
	assert.Equal(t, []string{reply.ID.String()}, items)

	_, err = s.PostOutbox(ctx, bob, vocab.DeleteNew("", reply.ID))
	require.NoError(t, err)
	total, _ = collectionOf(t, getInteractions(s, "/notes/"+parent+"/replies", nil))
	assert.Equal(t, 0, total)
}

func TestReplyToMissingLocalNoteIsRejected(t *testing.T) {
	s := crudServer(t)
	note := publicNote("answer")
	note.InReplyTo = vocab.IRI(s.NoteURI("missing"))
	_, err := s.PostOutbox(context.Background(), actorNamed("bob"), note)
	assert.ErrorIs(t, err, util.ErrBadRequest)
}

func TestRemoteReplyAndLikeAreCounted(t *testing.T) {
	s := crudServer(t)
	ctx := context.Background()
	id := publish(t, s, actorNamed("alice"), "hello", []string{store.PublicAddress})
	signer := "https://remote.example/users/bob"

	reply := publicNote("hi")
	reply.ID = "https://remote.example/notes/1"
	reply.InReplyTo = vocab.IRI(s.NoteURI(id))
	require.NoError(t, s.addReply(ctx, reply))
	_, items := collectionOf(t, getInteractions(s, "/notes/"+id+"/replies", nil))
	assert.Equal(t, []string{reply.ID.String()}, items)

	like := vocab.LikeNew("", vocab.IRI(s.NoteURI(id)))
	require.NoError(t, s.countReaction(ctx, signer, like, true))
	total, _ := collectionOf(t, getInteractions(s, "/notes/"+id+"/likes", nil))
	assert.Equal(t, 1, total)
	require.NoError(t, s.countReaction(ctx, signer, like, false))
	total, _ = collectionOf(t, getInteractions(s, "/notes/"+id+"/likes", nil))
	assert.Equal(t, 0, total)
}

func TestServeInteractionsHidesNonPublicObject(t *testing.T) {
	s := crudServer(t)
	alice := actorNamed("alice")
	id := createNote(t, s, alice, `{"type":"Note","content":"private"}`)
	assert.Equal(t, http.StatusNotFound, getInteractions(s, "/objects/"+id+"/likes", nil).Code, "anonymous")
	assert.Equal(t, http.StatusOK, getInteractions(s, "/objects/"+id+"/likes", alice).Code, "author")
}

func TestPublishedNotePointsAtInteractionCollections(t *testing.T) {
	s := crudServer(t)
	id := publish(t, s, actorNamed("alice"), "hello", []string{store.PublicAddress})
	it, err := s.store.GetObject(context.Background(), s.NoteURI(id))
	require.NoError(t, err)
	note, err := vocab.ToObject(it)
	require.NoError(t, err)
	assert.Equal(t, s.NoteURI(id)+"/likes", note.Likes.GetLink().String())
	assert.Equal(t, s.NoteURI(id)+"/shares", note.Shares.GetLink().String())
	assert.Equal(t, s.NoteURI(id)+"/replies", note.Replies.GetLink().String())
}

func TestNotesShowLikesAndReplies(t *testing.T) {
	s := crudServer(t)
	ctx := context.Background()
	alice, bob := actorNamed("alice"), actorNamed("bob")
	id := publish(t, s, alice, "question", []string{store.PublicAddress})
	_, err := s.PostOutbox(ctx, bob, vocab.LikeNew("", vocab.IRI(s.NoteURI(id))))
	require.NoError(t, err)
	reply, err := s.PublishNote(ctx, bob, "answer", []string{store.PublicAddress}, nil, s.NoteURI(id))
	require.NoError(t, err)

	notes, _, err := s.Notes(ctx, alice, 5)
	require.NoError(t, err)
	require.Len(t, notes, 1)
	assert.Equal(t, 1, notes[0].LikeCount)
	assert.False(t, notes[0].Liked, "alice hasn't liked her own note")
	require.Len(t, notes[0].ReplyNotes, 1)
	assert.Equal(t, reply.ID.String(), notes[0].ReplyNotes[0].URI)
	assert.Equal(t, "<p>answer</p>", notes[0].ReplyNotes[0].Content)
	assert.Equal(t, bob.ActorURI, notes[0].ReplyNotes[0].Author.ActorURI)
}

func TestLikedReportsRemoteLikes(t *testing.T) {
	s := crudServer(t)
	bob := actorNamed("bob")
	like := vocab.LikeNew(reactionID(bob, vocab.LikeType, "https://r.example/n/1"), vocab.IRI("https://r.example/n/1"))
	require.NoError(t, s.store.CreateObject(context.Background(), like))
	liked, err := s.Liked(context.Background(), bob, []string{"https://r.example/n/1", "https://r.example/n/2"})
	require.NoError(t, err)
	assert.Equal(t, map[string]bool{"https://r.example/n/1": true}, liked)
}

func TestRepliesAreOnlyShownNested(t *testing.T) {
	s := crudServer(t)
	ctx := context.Background()
	alice, bob := actorNamed("alice"), actorNamed("bob")
	id := publish(t, s, alice, "question", []string{store.PublicAddress})
	reply, err := s.PublishNote(ctx, alice, "answer", []string{store.PublicAddress}, nil, s.NoteURI(id))
	require.NoError(t, err)
	nested, err := s.PublishNote(ctx, bob, "follow-up", []string{store.PublicAddress}, nil, reply.ID.String())
	require.NoError(t, err)

	notes, _, err := s.Notes(ctx, alice, 5)
	require.NoError(t, err)
	require.Len(t, notes, 1, "alice's reply to her own note is only nested")
	require.Len(t, notes[0].ReplyNotes, 1)
	assert.True(t, notes[0].ReplyNotes[0].Mine)
	require.Len(t, notes[0].ReplyNotes[0].ReplyNotes, 1)
	assert.Equal(t, nested.ID.String(), notes[0].ReplyNotes[0].ReplyNotes[0].URI)

	public, err := s.PublicNotes(ctx, 50, nil)
	require.NoError(t, err)
	for _, n := range public {
		assert.NotEqual(t, reply.ID.String(), n.URI, "a reply isn't a top-level public note")
		assert.NotEqual(t, nested.ID.String(), n.URI, "a reply isn't a top-level public note")
	}
}

func TestReplyToOthersNoteStaysInOwnOutbox(t *testing.T) {
	s := crudServer(t)
	ctx := context.Background()
	alice, bob := actorNamed("alice"), actorNamed("bob")
	id := publish(t, s, alice, "question", []string{store.PublicAddress})
	_, err := s.PublishNote(ctx, bob, "answer", []string{store.PublicAddress}, nil, s.NoteURI(id))
	require.NoError(t, err)

	notes, _, err := s.Notes(ctx, bob, 5)
	require.NoError(t, err)
	require.Len(t, notes, 1, "bob's reply isn't under one of his notes, so it is listed")
	assert.Equal(t, s.NoteURI(id), notes[0].InReplyTo)
}
