package activitypub

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/cgs-earth/sal-pub/internal/store"
)

func TestDecodeObjectAcceptsNote(t *testing.T) {
	r := httptest.NewRequest(http.MethodPost, "/objects", strings.NewReader(`{"type":"Note","content":"hi"}`))
	obj, err := decodeObject(httptest.NewRecorder(), r)
	if err != nil || obj.Content.First().String() != "hi" {
		t.Errorf("got %+v, %v", obj, err)
	}
}

func TestDecodeObjectRejectsActivity(t *testing.T) {
	r := httptest.NewRequest(http.MethodPost, "/objects", strings.NewReader(`{"type":"Follow","object":"https://x.example/u"}`))
	if _, err := decodeObject(httptest.NewRecorder(), r); err == nil {
		t.Error("expected an error for an activity")
	}
}

func TestDecodeObjectRejectsInvalidJSON(t *testing.T) {
	r := httptest.NewRequest(http.MethodPost, "/objects", strings.NewReader(`not json`))
	if _, err := decodeObject(httptest.NewRecorder(), r); err == nil {
		t.Error("expected an error for invalid JSON")
	}
}

// crudServer returns a Server backed by the database in
// SALPUB_TEST_DATABASE_URL, or skips the test when it isn't set.
func crudServer(t *testing.T) *Server {
	t.Helper()
	dsn := os.Getenv("SALPUB_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("SALPUB_TEST_DATABASE_URL not set")
	}
	st, err := store.Open(context.Background(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	if err := st.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
	s, err := New(st, "https://sal.example")
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func actorNamed(name string) *store.Actor {
	return &store.Actor{Username: name, ActorURI: "https://sal.example/users/" + name + rand.Text()}
}

// do runs one of the object handlers against /objects/{id}.
func do(h func(http.ResponseWriter, *http.Request, *store.Actor), method, id, body string, a *store.Actor) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, "/objects/"+id, strings.NewReader(body))
	r.SetPathValue("id", id)
	w := httptest.NewRecorder()
	h(w, r, a)
	return w
}

// createNote creates a note as a and returns its id (the last path segment).
func createNote(t *testing.T, s *Server, a *store.Actor, body string) string {
	t.Helper()
	w := do(s.ServeCreateObject, http.MethodPost, "", body, a)
	if w.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", w.Code, w.Body)
	}
	loc := w.Header().Get("Location")
	return loc[strings.LastIndex(loc, "/")+1:]
}

func TestObjectCreateGetUpdateDelete(t *testing.T) {
	s, alice := crudServer(t), actorNamed("alice")
	id := createNote(t, s, alice, `{"type":"Note","content":"v1","to":["https://www.w3.org/ns/activitystreams#Public"]}`)

	w := do(s.ServeObject, http.MethodGet, id, "", nil)
	var got map[string]any
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &got) != nil ||
		got["content"] != "v1" || got["attributedTo"] != alice.ActorURI {
		t.Fatalf("get: %d %s", w.Code, w.Body)
	}

	if w := do(s.ServeUpdateObject, http.MethodPut, id, `{"type":"Note","content":"v2"}`, alice); w.Code != http.StatusOK {
		t.Fatalf("update: %d %s", w.Code, w.Body)
	}
	if w := do(s.ServeObject, http.MethodGet, id, "", alice); !strings.Contains(w.Body.String(), `"v2"`) {
		t.Errorf("after update: %s", w.Body)
	}

	if w := do(s.ServeDeleteObject, http.MethodDelete, id, "", alice); w.Code != http.StatusNoContent {
		t.Fatalf("delete: %d %s", w.Code, w.Body)
	}
	if w := do(s.ServeObject, http.MethodGet, id, "", alice); w.Code != http.StatusNotFound {
		t.Errorf("get after delete: %d", w.Code)
	}
}

func TestObjectUpdateByOtherActorIsForbidden(t *testing.T) {
	s := crudServer(t)
	id := createNote(t, s, actorNamed("alice"), `{"type":"Note","content":"mine"}`)
	if w := do(s.ServeUpdateObject, http.MethodPut, id, `{"type":"Note","content":"x"}`, actorNamed("mallory")); w.Code != http.StatusForbidden {
		t.Errorf("update: %d, want 403", w.Code)
	}
	if w := do(s.ServeDeleteObject, http.MethodDelete, id, "", actorNamed("mallory")); w.Code != http.StatusForbidden {
		t.Errorf("delete: %d, want 403", w.Code)
	}
}

func TestNonPublicObjectHiddenFromOthers(t *testing.T) {
	s, alice := crudServer(t), actorNamed("alice")
	id := createNote(t, s, alice, `{"type":"Note","content":"private"}`)
	if w := do(s.ServeObject, http.MethodGet, id, "", nil); w.Code != http.StatusNotFound {
		t.Errorf("anonymous get: %d, want 404", w.Code)
	}
	if w := do(s.ServeObject, http.MethodGet, id, "", alice); w.Code != http.StatusOK {
		t.Errorf("author get: %d, want 200", w.Code)
	}
}
