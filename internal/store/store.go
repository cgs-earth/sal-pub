// Package store is the Postgres persistence layer for actors, notes and followers.
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"sort"
	"time"

	"github.com/lib/pq"

	"github.com/cgs-earth/sal-pub/migrations"
)

var (
	ErrNotFound      = errors.New("not found")
	ErrUsernameTaken = errors.New("username taken")
)

type Actor struct {
	ID              string
	KeycloakSubject string
	Username        string
	DisplayName     string
	ActorURI        string
	PublicKeyID     string
	PublicKeyPEM    string
	PrivateKeyPEM   string
}

type Note struct {
	ID        string
	ActorID   string
	Content   string
	Published time.Time
}

type Store struct {
	db *sql.DB
}

// Open connects to Postgres and checks the connection.
func Open(ctx context.Context, dsn string) (*Store, error) {
	db, err := sql.Open("postgres", dsn)
	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}
	if err := db.PingContext(ctx); err != nil {
		db.Close()
		return nil, fmt.Errorf("ping database: %w", err)
	}
	return &Store{db: db}, nil
}

func (s *Store) Close() error {
	return s.db.Close()
}

// Migrate applies any migrations in migrations/ that haven't run yet, in
// filename order, each in its own transaction.
func (s *Store) Migrate(ctx context.Context) error {
	if _, err := s.db.ExecContext(ctx, `
		CREATE TABLE IF NOT EXISTS schema_migrations (
			version TEXT PRIMARY KEY,
			applied_at TIMESTAMPTZ NOT NULL DEFAULT now()
		)`); err != nil {
		return fmt.Errorf("create schema_migrations: %w", err)
	}

	names, err := fs.Glob(migrations.FS, "*.sql")
	if err != nil {
		return fmt.Errorf("list migrations: %w", err)
	}
	sort.Strings(names)

	for _, name := range names {
		var applied bool
		if err := s.db.QueryRowContext(ctx,
			`SELECT EXISTS (SELECT 1 FROM schema_migrations WHERE version = $1)`, name,
		).Scan(&applied); err != nil {
			return fmt.Errorf("check migration %s: %w", name, err)
		}
		if applied {
			continue
		}

		script, err := fs.ReadFile(migrations.FS, name)
		if err != nil {
			return fmt.Errorf("read migration %s: %w", name, err)
		}

		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return fmt.Errorf("begin migration %s: %w", name, err)
		}
		if _, err := tx.ExecContext(ctx, string(script)); err != nil {
			tx.Rollback()
			return fmt.Errorf("apply migration %s: %w", name, err)
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO schema_migrations (version) VALUES ($1)`, name,
		); err != nil {
			tx.Rollback()
			return fmt.Errorf("record migration %s: %w", name, err)
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("commit migration %s: %w", name, err)
		}
	}
	return nil
}

const actorColumns = `id, keycloak_subject, username, COALESCE(display_name, ''),
	actor_uri, public_key_id, public_key_pem, private_key_pem`

func scanActor(row *sql.Row) (*Actor, error) {
	a := new(Actor)
	err := row.Scan(
		&a.ID, &a.KeycloakSubject, &a.Username, &a.DisplayName,
		&a.ActorURI, &a.PublicKeyID, &a.PublicKeyPEM, &a.PrivateKeyPEM,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("scan actor: %w", err)
	}
	return a, nil
}

func (s *Store) GetByUsername(ctx context.Context, username string) (*Actor, error) {
	return scanActor(s.db.QueryRowContext(ctx,
		`SELECT `+actorColumns+` FROM actors WHERE username = $1`, username))
}

func (s *Store) GetBySubject(ctx context.Context, subject string) (*Actor, error) {
	return scanActor(s.db.QueryRowContext(ctx,
		`SELECT `+actorColumns+` FROM actors WHERE keycloak_subject = $1`, subject))
}

// CreateActor inserts a and sets a.ID. If another actor already has the
// username it returns ErrUsernameTaken. If an actor already exists for the
// Keycloak subject (a concurrent first login), it returns that actor instead.
func (s *Store) CreateActor(ctx context.Context, a *Actor) (*Actor, error) {
	err := s.db.QueryRowContext(ctx, `
		INSERT INTO actors (id, keycloak_subject, username, display_name,
		                    actor_uri, public_key_id, public_key_pem, private_key_pem)
		VALUES (gen_random_uuid(), $1, $2, NULLIF($3, ''), $4, $5, $6, $7)
		ON CONFLICT (keycloak_subject) DO NOTHING
		RETURNING id`,
		a.KeycloakSubject, a.Username, a.DisplayName,
		a.ActorURI, a.PublicKeyID, a.PublicKeyPEM, a.PrivateKeyPEM,
	).Scan(&a.ID)

	var pqErr *pq.Error
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return s.GetBySubject(ctx, a.KeycloakSubject)
	case errors.As(err, &pqErr) && pqErr.Code == "23505": // unique_violation
		return nil, ErrUsernameTaken
	case err != nil:
		return nil, fmt.Errorf("insert actor: %w", err)
	}
	return a, nil
}

func (s *Store) UpdateDisplayName(ctx context.Context, actorID, displayName string) error {
	if _, err := s.db.ExecContext(ctx,
		`UPDATE actors SET display_name = NULLIF($2, '') WHERE id = $1`, actorID, displayName,
	); err != nil {
		return fmt.Errorf("update display name: %w", err)
	}
	return nil
}

func (s *Store) CreateNote(ctx context.Context, actorID, content string) (*Note, error) {
	n := &Note{ActorID: actorID, Content: content}
	if err := s.db.QueryRowContext(ctx, `
		INSERT INTO notes (actor_id, content) VALUES ($1, $2)
		RETURNING id, published`, actorID, content,
	).Scan(&n.ID, &n.Published); err != nil {
		return nil, fmt.Errorf("insert note: %w", err)
	}
	return n, nil
}

// GetNote returns a note and its author.
func (s *Store) GetNote(ctx context.Context, id string) (*Note, *Actor, error) {
	n := new(Note)
	if err := s.db.QueryRowContext(ctx,
		`SELECT id, actor_id, content, published FROM notes WHERE id = $1`, id,
	).Scan(&n.ID, &n.ActorID, &n.Content, &n.Published); errors.Is(err, sql.ErrNoRows) {
		return nil, nil, ErrNotFound
	} else if err != nil {
		return nil, nil, fmt.Errorf("get note: %w", err)
	}

	a, err := scanActor(s.db.QueryRowContext(ctx,
		`SELECT `+actorColumns+` FROM actors WHERE id = $1`, n.ActorID))
	if err != nil {
		return nil, nil, err
	}
	return n, a, nil
}

// ListNotes returns an actor's most recent notes, newest first, and the total count.
func (s *Store) ListNotes(ctx context.Context, actorID string, limit int) ([]Note, int, error) {
	var total int
	if err := s.db.QueryRowContext(ctx,
		`SELECT count(*) FROM notes WHERE actor_id = $1`, actorID,
	).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("count notes: %w", err)
	}

	rows, err := s.db.QueryContext(ctx, `
		SELECT id, actor_id, content, published FROM notes
		WHERE actor_id = $1 ORDER BY published DESC LIMIT $2`, actorID, limit)
	if err != nil {
		return nil, 0, fmt.Errorf("list notes: %w", err)
	}
	defer rows.Close()

	var notes []Note
	for rows.Next() {
		var n Note
		if err := rows.Scan(&n.ID, &n.ActorID, &n.Content, &n.Published); err != nil {
			return nil, 0, fmt.Errorf("scan note: %w", err)
		}
		notes = append(notes, n)
	}
	return notes, total, rows.Err()
}

func (s *Store) AddFollower(ctx context.Context, actorID, followerURI, inbox string) error {
	if _, err := s.db.ExecContext(ctx, `
		INSERT INTO followers (actor_id, follower_uri, inbox) VALUES ($1, $2, $3)
		ON CONFLICT (actor_id, follower_uri) DO UPDATE SET inbox = EXCLUDED.inbox`,
		actorID, followerURI, inbox,
	); err != nil {
		return fmt.Errorf("add follower: %w", err)
	}
	return nil
}

func (s *Store) RemoveFollower(ctx context.Context, actorID, followerURI string) error {
	if _, err := s.db.ExecContext(ctx,
		`DELETE FROM followers WHERE actor_id = $1 AND follower_uri = $2`, actorID, followerURI,
	); err != nil {
		return fmt.Errorf("remove follower: %w", err)
	}
	return nil
}

// Followers returns the URIs of an actor's followers, oldest first.
func (s *Store) Followers(ctx context.Context, actorID string) ([]string, error) {
	return s.strings(ctx,
		`SELECT follower_uri FROM followers WHERE actor_id = $1 ORDER BY created_at`, actorID)
}

// FollowerInboxes returns the distinct inboxes to deliver an actor's activities to.
func (s *Store) FollowerInboxes(ctx context.Context, actorID string) ([]string, error) {
	return s.strings(ctx,
		`SELECT DISTINCT inbox FROM followers WHERE actor_id = $1`, actorID)
}

func (s *Store) strings(ctx context.Context, query string, args ...any) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("query: %w", err)
	}
	defer rows.Close()

	var out []string
	for rows.Next() {
		var v string
		if err := rows.Scan(&v); err != nil {
			return nil, fmt.Errorf("scan: %w", err)
		}
		out = append(out, v)
	}
	return out, rows.Err()
}
