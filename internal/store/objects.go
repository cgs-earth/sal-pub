package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"iter"

	vocab "github.com/go-ap/activitypub"
	"github.com/lib/pq"
)

// ErrExists is returned when creating an object whose id is already stored.
var ErrExists = errors.New("already exists")

// Objects stores ActivityPub objects by id.
type Objects interface {
	GetObject(ctx context.Context, id string) (vocab.Item, error)
	CreateObject(ctx context.Context, it vocab.Item) error
	UpdateObject(ctx context.Context, it vocab.Item) error
	DeleteObject(ctx context.Context, id string) error
}

// Collections stores which items (by id) belong to which collections.
type Collections interface {
	CollectionContains(ctx context.Context, collectionID, itemID string) (bool, error)
	CollectionItems(ctx context.Context, collectionID string) iter.Seq2[string, error]
	AddToCollection(ctx context.Context, collectionID, itemID string) error
	RemoveFromCollection(ctx context.Context, collectionID, itemID string) error
}

var (
	_ Objects     = (*Store)(nil)
	_ Collections = (*Store)(nil)
)

// GetObject returns the object with the given id, or ErrNotFound.
func (s *Store) GetObject(ctx context.Context, id string) (vocab.Item, error) {
	var data []byte
	err := s.db.QueryRowContext(ctx, `SELECT data FROM objects WHERE id = $1`, id).Scan(&data)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	} else if err != nil {
		return nil, fmt.Errorf("get object: %w", err)
	}
	it, err := vocab.UnmarshalJSON(data)
	if err != nil {
		return nil, fmt.Errorf("decode object %s: %w", id, err)
	}
	return it, nil
}

// CreateObject stores a new object under its id. It returns ErrExists if
// that id is already stored.
func (s *Store) CreateObject(ctx context.Context, it vocab.Item) error {
	id, data, err := encodeObject(it)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `INSERT INTO objects (id, data) VALUES ($1, $2)`, id, data)
	if pqErr := (*pq.Error)(nil); errors.As(err, &pqErr) && pqErr.Code == "23505" {
		return ErrExists
	} else if err != nil {
		return fmt.Errorf("create object: %w", err)
	}
	return nil
}

// UpdateObject replaces a stored object with it, matched by id. It returns
// ErrNotFound if the id isn't stored.
func (s *Store) UpdateObject(ctx context.Context, it vocab.Item) error {
	id, data, err := encodeObject(it)
	if err != nil {
		return err
	}
	res, err := s.db.ExecContext(ctx,
		`UPDATE objects SET data = $2, updated_at = now() WHERE id = $1`, id, data)
	if err != nil {
		return fmt.Errorf("update object: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// DeleteObject removes an object, or returns ErrNotFound. Collection
// memberships of the object are left as they are.
func (s *Store) DeleteObject(ctx context.Context, id string) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM objects WHERE id = $1`, id)
	if err != nil {
		return fmt.Errorf("delete object: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

func encodeObject(it vocab.Item) (string, []byte, error) {
	if vocab.IsNil(it) || it.GetID() == "" {
		return "", nil, errors.New("object has no id")
	}
	data, err := vocab.MarshalJSON(it)
	if err != nil {
		return "", nil, fmt.Errorf("encode object %s: %w", it.GetID(), err)
	}
	return it.GetID().String(), data, nil
}

// CollectionContains reports whether itemID is in collectionID.
func (s *Store) CollectionContains(ctx context.Context, collectionID, itemID string) (bool, error) {
	var ok bool
	if err := s.db.QueryRowContext(ctx, `
		SELECT EXISTS (SELECT 1 FROM collection_items WHERE collection_id = $1 AND item_id = $2)`,
		collectionID, itemID,
	).Scan(&ok); err != nil {
		return false, fmt.Errorf("check collection: %w", err)
	}
	return ok, nil
}

// CollectionItems iterates over the item ids in a collection, most recently
// added first. Iteration stops after the first error, which is yielded.
func (s *Store) CollectionItems(ctx context.Context, collectionID string) iter.Seq2[string, error] {
	return func(yield func(string, error) bool) {
		rows, err := s.db.QueryContext(ctx, `
			SELECT item_id FROM collection_items WHERE collection_id = $1
			ORDER BY added_at DESC, item_id`, collectionID)
		if err != nil {
			yield("", fmt.Errorf("list collection: %w", err))
			return
		}
		defer rows.Close()
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				yield("", fmt.Errorf("scan collection item: %w", err))
				return
			}
			if !yield(id, nil) {
				return
			}
		}
		if err := rows.Err(); err != nil {
			yield("", fmt.Errorf("list collection: %w", err))
		}
	}
}

// CollectionObjects returns up to limit stored objects in a collection, most
// recently added first, and the number of items in the collection. Items
// that aren't stored as objects are skipped.
func (s *Store) CollectionObjects(ctx context.Context, collectionID string, limit int) ([]vocab.Item, int, error) {
	var total int
	if err := s.db.QueryRowContext(ctx,
		`SELECT count(*) FROM collection_items WHERE collection_id = $1`, collectionID,
	).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("count collection: %w", err)
	}

	rows, err := s.db.QueryContext(ctx, `
		SELECT o.id, o.data FROM collection_items c JOIN objects o ON o.id = c.item_id
		WHERE c.collection_id = $1 ORDER BY c.added_at DESC, c.item_id LIMIT $2`, collectionID, limit)
	if err != nil {
		return nil, 0, fmt.Errorf("list collection objects: %w", err)
	}
	defer rows.Close()

	var items []vocab.Item
	for rows.Next() {
		var id string
		var data []byte
		if err := rows.Scan(&id, &data); err != nil {
			return nil, 0, fmt.Errorf("scan collection object: %w", err)
		}
		it, err := vocab.UnmarshalJSON(data)
		if err != nil {
			return nil, 0, fmt.Errorf("decode object %s: %w", id, err)
		}
		items = append(items, it)
	}
	return items, total, rows.Err()
}

// AddToCollection adds itemID to collectionID. Adding an item that is
// already there does nothing.
func (s *Store) AddToCollection(ctx context.Context, collectionID, itemID string) error {
	if _, err := s.db.ExecContext(ctx, `
		INSERT INTO collection_items (collection_id, item_id) VALUES ($1, $2)
		ON CONFLICT DO NOTHING`, collectionID, itemID,
	); err != nil {
		return fmt.Errorf("add to collection: %w", err)
	}
	return nil
}

// RemoveFromCollection removes itemID from collectionID, or returns
// ErrNotFound if it isn't there.
func (s *Store) RemoveFromCollection(ctx context.Context, collectionID, itemID string) error {
	res, err := s.db.ExecContext(ctx,
		`DELETE FROM collection_items WHERE collection_id = $1 AND item_id = $2`, collectionID, itemID)
	if err != nil {
		return fmt.Errorf("remove from collection: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}
