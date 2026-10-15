package activitypub

import (
	"context"
	"encoding/json"
	"slices"
	"testing"
	"time"

	vocab "github.com/go-ap/activitypub"

	"github.com/cgs-earth/sal-pub/internal/store"
)

func TestSanitizeUsernameEmail(t *testing.T) {
	if got := sanitizeUsername("Jane.Doe@example.org"); got != "jane_doe" {
		t.Fatalf("got %q, want jane_doe", got)
	}
}

func TestSanitizeUsernameTruncates(t *testing.T) {
	if got := sanitizeUsername("a-very-long-github-login-name-indeed"); len(got) != 26 {
		t.Fatalf("got %q (%d chars), want 26 chars", got, len(got))
	}
}

func TestSanitizeUsernameEmpty(t *testing.T) {
	if got := sanitizeUsername("@@@"); got != "user" {
		t.Fatalf("got %q, want user", got)
	}
}

func TestTextToHTMLEscapes(t *testing.T) {
	got := textToHTML("a <b> & c\nnext\n\nsecond")
	want := "<p>a &lt;b&gt; &amp; c<br>next</p><p>second</p>"
	if got != want {
		t.Fatalf("got %q, want %q", got, want)
	}
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
	if err != nil {
		t.Fatal(err)
	}
	var data []byte
	switch v := v.(type) {
	case *store.Actor:
		data, err = marshal(s.actorDocument(v))
	case *vocab.Note:
		data, err = marshal(s.createActivity(testActor(), v))
	}
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatal(err)
	}
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
		if doc[field] != want {
			t.Errorf("%s = %v, want %q", field, doc[field], want)
		}
	}
	key, _ := doc["publicKey"].(map[string]any)
	if key["id"] != "https://sal.example/users/alice#main-key" || key["owner"] != doc["id"] {
		t.Errorf("publicKey = %v", key)
	}
	if _, ok := doc["@context"]; !ok {
		t.Error("missing @context")
	}
}

func TestCreateActivityWrapsPublicNote(t *testing.T) {
	note := vocab.ObjectNew(vocab.NoteType)
	note.ID = "https://sal.example/notes/N1"
	note.Content = vocab.DefaultNaturalLanguage("<p>hi</p>")
	note.Published = time.Now()
	note.To = vocab.ItemCollection{vocab.PublicNS}
	note.CC = vocab.ItemCollection{vocab.IRI("https://sal.example/users/alice/followers")}
	doc := marshalToMap(t, note)

	if doc["type"] != "Create" || doc["actor"] != "https://sal.example/users/alice" {
		t.Fatalf("activity = %v", doc)
	}
	obj, _ := doc["object"].(map[string]any)
	if obj["type"] != "Note" || obj["id"] != "https://sal.example/notes/N1" || obj["content"] != "<p>hi</p>" {
		t.Errorf("object = %v", obj)
	}
	to, _ := obj["to"].([]any)
	if len(to) != 1 || to[0] != "https://www.w3.org/ns/activitystreams#Public" {
		t.Errorf("to = %v", obj["to"])
	}
	cc, _ := doc["cc"].([]any)
	if len(cc) != 1 || cc[0] != "https://sal.example/users/alice/followers" {
		t.Errorf("activity cc = %v", doc["cc"])
	}
}

func TestParseAddressesPublicShorthandAndDedupe(t *testing.T) {
	got, err := ParseAddresses("public, https://remote.example/users/bob\nas:Public https://remote.example/users/bob")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{store.PublicAddress, "https://remote.example/users/bob"}
	if !slices.Equal(got, want) {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestParseAddressesEmpty(t *testing.T) {
	got, err := ParseAddresses("  ,  ")
	if err != nil || len(got) != 0 {
		t.Errorf("got %q, %v; want no addresses", got, err)
	}
}

func TestParseAddressesRejectsHandle(t *testing.T) {
	if _, err := ParseAddresses("@bob@remote.example"); err == nil {
		t.Error("expected an error for a bare handle")
	}
}

func TestResolveActorURILocalHandle(t *testing.T) {
	s, _ := New(nil, "https://sal.example")
	got, err := s.resolveActorURI(context.Background(), "@Bob@sal.example")
	if err != nil || got != "https://sal.example/users/bob" {
		t.Errorf("got %q, %v", got, err)
	}
}

func TestResolveActorURIPassesURLThrough(t *testing.T) {
	s, _ := New(nil, "https://sal.example")
	got, err := s.resolveActorURI(context.Background(), " https://remote.example/users/bob ")
	if err != nil || got != "https://remote.example/users/bob" {
		t.Errorf("got %q, %v", got, err)
	}
}

func TestResolveActorURIRejectsBareName(t *testing.T) {
	s, _ := New(nil, "https://sal.example")
	if _, err := s.resolveActorURI(context.Background(), "bob"); err == nil {
		t.Error("expected an error for a name without a host")
	}
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
	if err := s.receiveNote(context.Background(), testActor(), "https://remote.example/users/bob", create); err == nil {
		t.Error("expected an error for a non-embedded object")
	}
}

func TestReceiveNoteRejectsNoteAttributedToSomeoneElse(t *testing.T) {
	s, _ := New(nil, "https://sal.example")
	create := remoteCreate("https://remote.example/notes/1", "https://remote.example/users/carol")
	if err := s.receiveNote(context.Background(), testActor(), "https://remote.example/users/bob", create); err == nil {
		t.Error("expected an error for a note not attributed to the signer")
	}
}

func TestReceiveNoteRejectsNoteOnAnotherHost(t *testing.T) {
	s, _ := New(nil, "https://sal.example")
	create := remoteCreate("https://elsewhere.example/notes/1", "https://remote.example/users/bob")
	if err := s.receiveNote(context.Background(), testActor(), "https://remote.example/users/bob", create); err == nil {
		t.Error("expected an error for a note hosted on another server")
	}
}
