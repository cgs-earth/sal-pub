package activitypub

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	vocab "github.com/go-ap/activitypub"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/cgs-earth/sal-pub/internal/store"
)

func TestSanitizeUsernameEmail(t *testing.T) {
	assert.Equal(t, "jane_doe", sanitizeUsername("Jane.Doe@example.org"))
}

func TestSanitizeUsernameTruncates(t *testing.T) {
	assert.Len(t, sanitizeUsername("a-very-long-github-login-name-indeed"), 26)
}

func TestSanitizeUsernameEmpty(t *testing.T) {
	assert.Equal(t, "user", sanitizeUsername("@@@"))
}

func TestTextToHTMLEscapes(t *testing.T) {
	assert.Equal(t, "<p>a &lt;b&gt; &amp; c<br>next</p><p>second</p>", textToHTML("a <b> & c\nnext\n\nsecond"))
}

func TestHTMLToTextReversesTextToHTML(t *testing.T) {
	text := "a <b> & c\nnext\n\nsecond"
	assert.Equal(t, text, HTMLToText(textToHTML(text)))
}

func TestHTMLToTextHandlesRemoteHTML(t *testing.T) {
	in := `<p class="x">hi <span class="h-card"><a href="https://r.example/@bob">@<span>bob</span></a></span></p>
<P>line<br />two</P><script>x</script>`
	assert.Equal(t, "hi @bob\n\nline\ntwox", HTMLToText(in))
}

func testActor() *store.Actor {
	return &store.Actor{
		Username:     "alice",
		DisplayName:  "Alice",
		ActorURI:     "https://sal.example/users/alice",
		PublicKeyID:  "https://sal.example/users/alice#main-key",
		PublicKeyPEM: "PEM",
	}
}

func marshalToMap(t *testing.T, v any) map[string]any {
	t.Helper()
	s, err := New(nil, "https://sal.example")
	require.NoError(t, err)
	var data []byte
	switch v := v.(type) {
	case *store.Actor:
		data, err = marshal(s.actorDocument(v))
	case *vocab.Note:
		data, err = marshal(s.createActivity(testActor(), v))
	}
	require.NoError(t, err)
	var out map[string]any
	require.NoError(t, json.Unmarshal(data, &out))
	return out
}

func TestActorDocumentHasMastodonFields(t *testing.T) {
	doc := marshalToMap(t, testActor())

	for field, want := range map[string]string{
		"id":                "https://sal.example/users/alice",
		"type":              "Person",
		"preferredUsername": "alice",
		"inbox":             "https://sal.example/users/alice/inbox",
		"followers":         "https://sal.example/users/alice/followers",
	} {
		assert.Equal(t, want, doc[field], field)
	}
	key, _ := doc["publicKey"].(map[string]any)
	assert.Equal(t, "https://sal.example/users/alice#main-key", key["id"])
	assert.Equal(t, doc["id"], key["owner"])
	assert.Contains(t, doc, "@context")
}

func TestCreateActivityWrapsPublicNote(t *testing.T) {
	note := vocab.ObjectNew(vocab.NoteType)
	note.ID = "https://sal.example/notes/N1"
	note.Content = vocab.DefaultNaturalLanguage("<p>hi</p>")
	note.Published = time.Now()
	note.To = vocab.ItemCollection{vocab.PublicNS}
	note.CC = vocab.ItemCollection{vocab.IRI("https://sal.example/users/alice/followers")}
	doc := marshalToMap(t, note)

	require.Equal(t, "Create", doc["type"])
	require.Equal(t, "https://sal.example/users/alice", doc["actor"])
	obj, _ := doc["object"].(map[string]any)
	assert.Equal(t, "Note", obj["type"])
	assert.Equal(t, "https://sal.example/notes/N1", obj["id"])
	assert.Equal(t, "<p>hi</p>", obj["content"])
	assert.Equal(t, []any{"https://www.w3.org/ns/activitystreams#Public"}, obj["to"])
	assert.Equal(t, []any{"https://sal.example/users/alice/followers"}, doc["cc"])
}

func TestParseAddressesPublicShorthandAndDedupe(t *testing.T) {
	got, err := ParseAddresses("public, https://remote.example/users/bob\nas:Public https://remote.example/users/bob")
	require.NoError(t, err)
	assert.Equal(t, []string{store.PublicAddress, "https://remote.example/users/bob"}, got)
}

func TestParseAddressesEmpty(t *testing.T) {
	got, err := ParseAddresses("  ,  ")
	require.NoError(t, err)
	assert.Empty(t, got)
}

func TestParseAddressesRejectsHandle(t *testing.T) {
	_, err := ParseAddresses("@bob@remote.example")
	assert.Error(t, err, "expected an error for a bare handle")
}

func TestResolveActorURILocalHandle(t *testing.T) {
	s, _ := New(nil, "https://sal.example")
	got, err := s.resolveActorURI(context.Background(), "@Bob@sal.example")
	require.NoError(t, err)
	assert.Equal(t, "https://sal.example/users/bob", got)
}

func TestResolveActorURIPassesURLThrough(t *testing.T) {
	s, _ := New(nil, "https://sal.example")
	got, err := s.resolveActorURI(context.Background(), " https://remote.example/users/bob ")
	require.NoError(t, err)
	assert.Equal(t, "https://remote.example/users/bob", got)
}

func TestResolveActorURIRejectsBareName(t *testing.T) {
	s, _ := New(nil, "https://sal.example")
	_, err := s.resolveActorURI(context.Background(), "bob")
	assert.Error(t, err, "expected an error for a name without a host")
}

func remoteCreate(noteID, attributedTo string) *vocab.Create {
	note := vocab.ObjectNew(vocab.NoteType)
	note.ID = vocab.IRI(noteID)
	note.AttributedTo = vocab.IRI(attributedTo)
	note.Content = vocab.DefaultNaturalLanguage("<p>hi</p>")
	create := vocab.CreateNew(vocab.IRI(noteID+"#create"), note)
	create.Actor = vocab.IRI(attributedTo)
	return create
}

func TestReceiveNoteRejectsBareIRI(t *testing.T) {
	s, _ := New(nil, "https://sal.example")
	create := vocab.CreateNew("https://remote.example/notes/1#create", vocab.IRI("https://remote.example/notes/1"))
	err := s.receiveNote(context.Background(), testActor(), "https://remote.example/users/bob", create)
	assert.Error(t, err, "expected an error for a non-embedded object")
}

func TestReceiveNoteRejectsNoteAttributedToSomeoneElse(t *testing.T) {
	s, _ := New(nil, "https://sal.example")
	create := remoteCreate("https://remote.example/notes/1", "https://remote.example/users/carol")
	err := s.receiveNote(context.Background(), testActor(), "https://remote.example/users/bob", create)
	assert.Error(t, err, "expected an error for a note not attributed to the signer")
}

func TestReceiveNoteRejectsNoteOnAnotherHost(t *testing.T) {
	s, _ := New(nil, "https://sal.example")
	create := remoteCreate("https://elsewhere.example/notes/1", "https://remote.example/users/bob")
	err := s.receiveNote(context.Background(), testActor(), "https://remote.example/users/bob", create)
	assert.Error(t, err, "expected an error for a note hosted on another server")
}
