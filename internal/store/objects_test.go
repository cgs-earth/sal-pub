package store

import (
	"context"
	"crypto/rand"
	"os"
	"testing"

	vocab "github.com/go-ap/activitypub"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// testStore opens and migrates the database in SALPUB_TEST_DATABASE_URL, or
// skips the test when it isn't set. Use a throwaway database, not salpub.
func testStore(t *testing.T) *Store {
	t.Helper()
	dsn := os.Getenv("SALPUB_TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("SALPUB_TEST_DATABASE_URL not set")
	}
	s, err := Open(context.Background(), dsn)
	require.NoError(t, err)
	t.Cleanup(func() { s.Close() })
	require.NoError(t, s.Migrate(context.Background()))
	return s
}

// uniqueID returns a fresh IRI so tests don't collide across runs.
func uniqueID() string {
	return "https://test.example/" + rand.Text()
}

func testNote(id, content string) *vocab.Object {
	note := vocab.ObjectNew(vocab.NoteType)
	note.ID = vocab.IRI(id)
	note.Content = vocab.DefaultNaturalLanguage(content)
	return note
}

func TestCreateThenGetObject(t *testing.T) {
	s, ctx, id := testStore(t), context.Background(), uniqueID()
	require.NoError(t, s.CreateObject(ctx, testNote(id, "hello")))
	it, err := s.GetObject(ctx, id)
	require.NoError(t, err)
	obj, err := vocab.ToObject(it)
	require.NoError(t, err)
	assert.True(t, obj.Match(vocab.NoteType))
	assert.Equal(t, "hello", obj.Content.First().String())
}

func TestCreateObjectTwiceReturnsErrExists(t *testing.T) {
	s, ctx, id := testStore(t), context.Background(), uniqueID()
	require.NoError(t, s.CreateObject(ctx, testNote(id, "a")))
	assert.ErrorIs(t, s.CreateObject(ctx, testNote(id, "b")), ErrExists)
}

func TestCreateObjectWithoutIDFails(t *testing.T) {
	s := testStore(t)
	assert.Error(t, s.CreateObject(context.Background(), vocab.ObjectNew(vocab.NoteType)))
}

func TestUpdateObjectReplacesData(t *testing.T) {
	s, ctx, id := testStore(t), context.Background(), uniqueID()
	require.NoError(t, s.CreateObject(ctx, testNote(id, "before")))
	require.NoError(t, s.UpdateObject(ctx, testNote(id, "after")))
	it, err := s.GetObject(ctx, id)
	require.NoError(t, err)
	obj, err := vocab.ToObject(it)
	require.NoError(t, err)
	assert.Equal(t, "after", obj.Content.First().String())
}

func TestUpdateMissingObjectReturnsErrNotFound(t *testing.T) {
	s := testStore(t)
	assert.ErrorIs(t, s.UpdateObject(context.Background(), testNote(uniqueID(), "x")), ErrNotFound)
}

func TestDeleteObject(t *testing.T) {
	s, ctx, id := testStore(t), context.Background(), uniqueID()
	require.NoError(t, s.CreateObject(ctx, testNote(id, "x")))
	require.NoError(t, s.DeleteObject(ctx, id))
	_, err := s.GetObject(ctx, id)
	assert.ErrorIs(t, err, ErrNotFound, "get after delete")
	assert.ErrorIs(t, s.DeleteObject(ctx, id), ErrNotFound, "second delete")
}

func TestAddToCollectionAndContains(t *testing.T) {
	s, ctx, coll, item := testStore(t), context.Background(), uniqueID(), uniqueID()
	ok, err := s.CollectionContains(ctx, coll, item)
	require.NoError(t, err)
	require.False(t, ok, "before add")
	require.NoError(t, s.AddToCollection(ctx, coll, item))
	require.NoError(t, s.AddToCollection(ctx, coll, item), "adding twice")
	ok, err = s.CollectionContains(ctx, coll, item)
	require.NoError(t, err)
	assert.True(t, ok, "after add")
}

func TestCollectionItemsNewestFirst(t *testing.T) {
	s, ctx, coll := testStore(t), context.Background(), uniqueID()
	first, second := uniqueID(), uniqueID()
	for _, item := range []string{first, second} {
		require.NoError(t, s.AddToCollection(ctx, coll, item))
	}
	var got []string
	for id, err := range s.CollectionItems(ctx, coll) {
		require.NoError(t, err)
		got = append(got, id)
	}
	assert.Equal(t, []string{second, first}, got)
}

func TestRemoveFromCollection(t *testing.T) {
	s, ctx, coll, item := testStore(t), context.Background(), uniqueID(), uniqueID()
	require.NoError(t, s.AddToCollection(ctx, coll, item))
	require.NoError(t, s.RemoveFromCollection(ctx, coll, item))
	ok, err := s.CollectionContains(ctx, coll, item)
	require.NoError(t, err)
	assert.False(t, ok, "item still in collection after remove")
	assert.ErrorIs(t, s.RemoveFromCollection(ctx, coll, item), ErrNotFound, "second remove")
}
