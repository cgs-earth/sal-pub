package activitypub

import (
	"encoding/json"
	"testing"
	"time"

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
	case *store.Note:
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
	note := &store.Note{ID: "5bd2beed-0580-4b44-8189-cf80b71d7746", Content: "<p>hi</p>", Published: time.Now()}
	doc := marshalToMap(t, note)

	if doc["type"] != "Create" || doc["actor"] != "https://sal.example/users/alice" {
		t.Fatalf("activity = %v", doc)
	}
	obj, _ := doc["object"].(map[string]any)
	if obj["type"] != "Note" || obj["id"] != "https://sal.example/notes/"+note.ID || obj["content"] != "<p>hi</p>" {
		t.Errorf("object = %v", obj)
	}
	to, _ := obj["to"].([]any)
	if len(to) != 1 || to[0] != "https://www.w3.org/ns/activitystreams#Public" {
		t.Errorf("to = %v", obj["to"])
	}
}
