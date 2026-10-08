package store

import (
	"context"
	"crypto/rand"
	"errors"
	"os"
	"testing"

	vocab "github.com/go-ap/activitypub"
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
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	if err := s.Migrate(context.Background()); err != nil {
		t.Fatal(err)
	}
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
	if err := s.CreateObject(ctx, testNote(id, "hello")); err != nil {
		t.Fatal(err)
	}
	it, err := s.GetObject(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	obj, err := vocab.ToObject(it)
	if err != nil || !obj.Match(vocab.NoteType) || obj.Content.First().String() != "hello" {
		t.Errorf("got %+v, %v", it, err)
	}
}

func TestCreateObjectTwiceReturnsErrExists(t *testing.T) {
	s, ctx, id := testStore(t), context.Background(), uniqueID()
	if err := s.CreateObject(ctx, testNote(id, "a")); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateObject(ctx, testNote(id, "b")); !errors.Is(err, ErrExists) {
		t.Errorf("got %v, want ErrExists", err)
	}
}

func TestCreateObjectWithoutIDFails(t *testing.T) {
	s := testStore(t)
	if err := s.CreateObject(context.Background(), vocab.ObjectNew(vocab.NoteType)); err == nil {
		t.Error("expected an error for an object without an id")
	}
}

func TestUpdateObjectReplacesData(t *testing.T) {
	s, ctx, id := testStore(t), context.Background(), uniqueID()
	if err := s.CreateObject(ctx, testNote(id, "before")); err != nil {
		t.Fatal(err)
	}
	if err := s.UpdateObject(ctx, testNote(id, "after")); err != nil {
		t.Fatal(err)
	}
	it, err := s.GetObject(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if obj, _ := vocab.ToObject(it); obj.Content.First().String() != "after" {
		t.Errorf("content = %q, want after", obj.Content.First())
	}
}

func TestUpdateMissingObjectReturnsErrNotFound(t *testing.T) {
	s := testStore(t)
	if err := s.UpdateObject(context.Background(), testNote(uniqueID(), "x")); !errors.Is(err, ErrNotFound) {
		t.Errorf("got %v, want ErrNotFound", err)
	}
}

func TestDeleteObject(t *testing.T) {
	s, ctx, id := testStore(t), context.Background(), uniqueID()
	if err := s.CreateObject(ctx, testNote(id, "x")); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteObject(ctx, id); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetObject(ctx, id); !errors.Is(err, ErrNotFound) {
		t.Errorf("get after delete: %v, want ErrNotFound", err)
	}
	if err := s.DeleteObject(ctx, id); !errors.Is(err, ErrNotFound) {
		t.Errorf("second delete: %v, want ErrNotFound", err)
	}
}

func TestAddToCollectionAndContains(t *testing.T) {
	s, ctx, coll, item := testStore(t), context.Background(), uniqueID(), uniqueID()
	if ok, err := s.CollectionContains(ctx, coll, item); err != nil || ok {
		t.Fatalf("before add: %v, %v", ok, err)
	}
	if err := s.AddToCollection(ctx, coll, item); err != nil {
		t.Fatal(err)
	}
	if err := s.AddToCollection(ctx, coll, item); err != nil {
		t.Fatalf("adding twice: %v", err)
	}
	if ok, err := s.CollectionContains(ctx, coll, item); err != nil || !ok {
		t.Errorf("after add: %v, %v", ok, err)
	}
}

func TestCollectionItemsNewestFirst(t *testing.T) {
	s, ctx, coll := testStore(t), context.Background(), uniqueID()
	first, second := uniqueID(), uniqueID()
	for _, item := range []string{first, second} {
		if err := s.AddToCollection(ctx, coll, item); err != nil {
			t.Fatal(err)
		}
	}
	var got []string
	for id, err := range s.CollectionItems(ctx, coll) {
		if err != nil {
			t.Fatal(err)
		}
		got = append(got, id)
	}
	if len(got) != 2 || got[0] != second || got[1] != first {
		t.Errorf("got %q, want [%s %s]", got, second, first)
	}
}

func TestRemoveFromCollection(t *testing.T) {
	s, ctx, coll, item := testStore(t), context.Background(), uniqueID(), uniqueID()
	if err := s.AddToCollection(ctx, coll, item); err != nil {
		t.Fatal(err)
	}
	if err := s.RemoveFromCollection(ctx, coll, item); err != nil {
		t.Fatal(err)
	}
	if ok, _ := s.CollectionContains(ctx, coll, item); ok {
		t.Error("item still in collection after remove")
	}
	if err := s.RemoveFromCollection(ctx, coll, item); !errors.Is(err, ErrNotFound) {
		t.Errorf("second remove: %v, want ErrNotFound", err)
	}
}
