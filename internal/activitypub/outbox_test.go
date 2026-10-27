package activitypub

import (
	"context"
	"crypto/rand"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	vocab "github.com/go-ap/activitypub"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/cgs-earth/sal-pub/internal/store"
	"github.com/cgs-earth/sal-pub/pkg/util"
)

// noStoreServer returns a Server without a database, for checks that fail
// before the store is used.
func noStoreServer(t *testing.T) *Server {
	t.Helper()
	s, err := New(nil, "https://sal.example")
	require.NoError(t, err)
	return s
}

func publicNote(content string) *vocab.Note {
	note := vocab.ObjectNew(vocab.NoteType)
	note.Content = vocab.DefaultNaturalLanguage(content)
	note.To = vocab.ItemCollection{vocab.PublicNS}
	return note
}

func TestNoteTextPrefersPlainTextSource(t *testing.T) {
	note := publicNote("<p>html</p>")
	note.Source = vocab.Source{Content: vocab.DefaultNaturalLanguage(" plain "), MediaType: "text/plain"}
	text, err := noteText(note)
	require.NoError(t, err)
	assert.Equal(t, "plain", text)
}

func TestNoteTextStripsHTMLFromContent(t *testing.T) {
	text, err := noteText(publicNote("<p>a &amp; b</p><p><script>x</script>c</p>"))
	require.NoError(t, err)
	assert.Equal(t, "a & b\n\nxc", text)
}

func TestNoteTextRejectsEmpty(t *testing.T) {
	_, err := noteText(publicNote("<p> </p>"))
	assert.ErrorIs(t, err, util.ErrBadRequest)
}

func TestNoteTextRejectsTooLong(t *testing.T) {
	_, err := noteText(publicNote(strings.Repeat("x", MaxNoteChars+1)))
	assert.ErrorIs(t, err, util.ErrBadRequest)
}

func TestPostOutboxRejectsOtherActor(t *testing.T) {
	create := vocab.CreateNew("", publicNote("hi"))
	create.Actor = vocab.IRI("https://sal.example/users/mallory")
	_, err := noStoreServer(t).PostOutbox(context.Background(), testActor(), create)
	assert.ErrorIs(t, err, util.ErrForbidden)
}

func TestPostOutboxRejectsUnsupportedActivity(t *testing.T) {
	block := vocab.ActivityNew("", vocab.BlockType, vocab.IRI("https://remote.example/users/bob"))
	_, err := noStoreServer(t).PostOutbox(context.Background(), testActor(), block)
	assert.ErrorIs(t, err, util.ErrBadRequest)
}

func TestPostOutboxRejectsNoteWithoutRecipients(t *testing.T) {
	note := publicNote("hi")
	note.To = nil
	_, err := noStoreServer(t).PostOutbox(context.Background(), testActor(), note)
	assert.ErrorIs(t, err, util.ErrBadRequest)
}

func TestPostOutboxRejectsInvalidRecipient(t *testing.T) {
	note := publicNote("hi")
	note.CC = vocab.ItemCollection{vocab.IRI("ftp://x.example")}
	_, err := noStoreServer(t).PostOutbox(context.Background(), testActor(), note)
	assert.ErrorIs(t, err, util.ErrBadRequest)
}

func TestServePostOutboxRejectsInvalidJSON(t *testing.T) {
	w := httptest.NewRecorder()
	noStoreServer(t).ServePostOutbox(w, httptest.NewRequest(http.MethodPost, "/users/alice/outbox", strings.NewReader("nope")), testActor())
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

// The tests below need Postgres (see crudServer).

// postOutbox posts body to a's outbox and returns the response.
func postOutbox(s *Server, a *store.Actor, body string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	s.ServePostOutbox(w, httptest.NewRequest(http.MethodPost, "/users/"+a.Username+"/outbox", strings.NewReader(body)), a)
	return w
}

func TestOutboxCreateNotePublishesEscapedText(t *testing.T) {
	s, a := crudServer(t), actorNamed("alice")
	w := postOutbox(s, a, `{"type":"Create","object":{"type":"Note","content":"<script>alert(1)</script>hi",
		"to":["https://www.w3.org/ns/activitystreams#Public"]}}`)
	require.Equal(t, http.StatusCreated, w.Code, "%s", w.Body)
	assert.True(t, strings.HasSuffix(w.Header().Get("Location"), "#create"), "Location %q", w.Header().Get("Location"))

	notes, _, err := s.Notes(context.Background(), a, 1)
	require.NoError(t, err)
	require.Len(t, notes, 1)
	assert.Equal(t, "<p>alert(1)hi</p>", notes[0].Content)
}

func TestOutboxBareObjectIsCreated(t *testing.T) {
	s, a := crudServer(t), actorNamed("alice")
	w := postOutbox(s, a, `{"type":"Article","name":"draft"}`)
	require.Equal(t, http.StatusCreated, w.Code, "%s", w.Body)

	act, err := vocab.UnmarshalJSON(w.Body.Bytes())
	require.NoError(t, err)
	create, err := vocab.ToActivity(act)
	require.NoError(t, err)
	id := create.Object.GetLink().String()
	assert.True(t, strings.HasPrefix(id, s.objectURI("")), "object id %q", id)
	_, err = s.ownObject(context.Background(), a, id)
	assert.NoError(t, err)
}

func TestOutboxUpdateAndDeleteNote(t *testing.T) {
	s, a := crudServer(t), actorNamed("alice")
	uri := s.NoteURI(publish(t, s, a, "old", []string{store.PublicAddress}))

	update := vocab.UpdateNew("", &vocab.Note{ID: vocab.IRI(uri), Type: vocab.NoteType, Content: vocab.DefaultNaturalLanguage("new")})
	_, err := s.PostOutbox(context.Background(), a, update)
	require.NoError(t, err)
	notes, _, err := s.Notes(context.Background(), a, 1)
	require.NoError(t, err)
	require.Len(t, notes, 1)
	assert.Equal(t, "<p>new</p>", notes[0].Content)

	_, err = s.PostOutbox(context.Background(), a, vocab.DeleteNew("", vocab.IRI(uri)))
	require.NoError(t, err)
	_, err = s.store.GetObject(context.Background(), uri)
	assert.ErrorIs(t, err, store.ErrNotFound)
}

func TestOutboxDeleteOtherActorsNoteIsForbidden(t *testing.T) {
	s := crudServer(t)
	uri := s.NoteURI(publish(t, s, actorNamed("alice"), "mine", []string{store.PublicAddress}))
	_, err := s.PostOutbox(context.Background(), actorNamed("mallory"), vocab.DeleteNew("", vocab.IRI(uri)))
	assert.ErrorIs(t, err, util.ErrForbidden)
}

func TestOutboxUpdateOtherActorsObjectIsForbidden(t *testing.T) {
	s := crudServer(t)
	id := createNote(t, s, actorNamed("alice"), `{"type":"Note","content":"mine"}`)
	update := vocab.UpdateNew("", &vocab.Object{ID: vocab.IRI(s.objectURI(id)), Type: vocab.NoteType})
	_, err := s.PostOutbox(context.Background(), actorNamed("mallory"), update)
	assert.ErrorIs(t, err, util.ErrForbidden)
}

func TestOutboxFollowAndUndoLocalActor(t *testing.T) {
	s := crudServer(t)
	ctx := context.Background()
	alice, err := s.EnsureActor(ctx, rand.Text(), "alice_"+rand.Text()[:8], "")
	require.NoError(t, err)
	bob, err := s.EnsureActor(ctx, rand.Text(), "bob_"+rand.Text()[:8], "")
	require.NoError(t, err)

	_, err = s.PostOutbox(ctx, alice, vocab.FollowNew("", vocab.IRI(bob.ActorURI)))
	require.NoError(t, err)
	following, err := s.store.CollectionContains(ctx, FollowingURI(alice), bob.ActorURI)
	require.NoError(t, err)
	assert.True(t, following, "alice should follow bob")

	undo := vocab.UndoNew("", vocab.FollowNew("", vocab.IRI(bob.ActorURI)))
	_, err = s.PostOutbox(ctx, alice, undo)
	require.NoError(t, err)
	following, err = s.store.CollectionContains(ctx, FollowingURI(alice), bob.ActorURI)
	require.NoError(t, err)
	assert.False(t, following, "alice should no longer follow bob")
}
