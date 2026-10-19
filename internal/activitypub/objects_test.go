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

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/cgs-earth/sal-pub/internal/store"
)

func TestDecodeObjectAcceptsNote(t *testing.T) {
	r := httptest.NewRequest(http.MethodPost, "/objects", strings.NewReader(`{"type":"Note","content":"hi"}`))
	obj, err := decodeObject(httptest.NewRecorder(), r)
	require.NoError(t, err)
	assert.Equal(t, "hi", obj.Content.First().String())
}

func TestDecodeObjectRejectsActivity(t *testing.T) {
	r := httptest.NewRequest(http.MethodPost, "/objects", strings.NewReader(`{"type":"Follow","object":"https://x.example/u"}`))
	_, err := decodeObject(httptest.NewRecorder(), r)
	assert.Error(t, err, "expected an error for an activity")
}

func TestDecodeObjectRejectsInvalidJSON(t *testing.T) {
	r := httptest.NewRequest(http.MethodPost, "/objects", strings.NewReader(`not json`))
	_, err := decodeObject(httptest.NewRecorder(), r)
	assert.Error(t, err, "expected an error for invalid JSON")
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
	require.NoError(t, err)
	t.Cleanup(func() { st.Close() })
	require.NoError(t, st.Migrate(context.Background()))
	s, err := New(st, "https://sal.example")
	require.NoError(t, err)
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
	require.Equal(t, http.StatusCreated, w.Code, "create: %s", w.Body)
	loc := w.Header().Get("Location")
	return loc[strings.LastIndex(loc, "/")+1:]
}

func TestObjectCreateGetUpdateDelete(t *testing.T) {
	s, alice := crudServer(t), actorNamed("alice")
	id := createNote(t, s, alice, `{"type":"Note","content":"v1","to":["https://www.w3.org/ns/activitystreams#Public"]}`)

	w := do(s.ServeObject, http.MethodGet, id, "", nil)
	require.Equal(t, http.StatusOK, w.Code, "get: %s", w.Body)
	var got map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &got))
	require.Equal(t, "v1", got["content"])
	require.Equal(t, alice.ActorURI, got["attributedTo"])

	w = do(s.ServeUpdateObject, http.MethodPut, id, `{"type":"Note","content":"v2"}`, alice)
	require.Equal(t, http.StatusOK, w.Code, "update: %s", w.Body)
	assert.Contains(t, do(s.ServeObject, http.MethodGet, id, "", alice).Body.String(), `"v2"`, "after update")

	w = do(s.ServeDeleteObject, http.MethodDelete, id, "", alice)
	require.Equal(t, http.StatusNoContent, w.Code, "delete: %s", w.Body)
	assert.Equal(t, http.StatusNotFound, do(s.ServeObject, http.MethodGet, id, "", alice).Code, "get after delete")
}

func TestObjectUpdateByOtherActorIsForbidden(t *testing.T) {
	s := crudServer(t)
	id := createNote(t, s, actorNamed("alice"), `{"type":"Note","content":"mine"}`)
	w := do(s.ServeUpdateObject, http.MethodPut, id, `{"type":"Note","content":"x"}`, actorNamed("mallory"))
	assert.Equal(t, http.StatusForbidden, w.Code, "update")
	w = do(s.ServeDeleteObject, http.MethodDelete, id, "", actorNamed("mallory"))
	assert.Equal(t, http.StatusForbidden, w.Code, "delete")
}

func TestNonPublicObjectHiddenFromOthers(t *testing.T) {
	s, alice := crudServer(t), actorNamed("alice")
	id := createNote(t, s, alice, `{"type":"Note","content":"private"}`)
	assert.Equal(t, http.StatusNotFound, do(s.ServeObject, http.MethodGet, id, "", nil).Code, "anonymous get")
	assert.Equal(t, http.StatusOK, do(s.ServeObject, http.MethodGet, id, "", alice).Code, "author get")
}
