// Package store is the Postgres persistence layer for actors, inbox notes and
// generic ActivityPub objects and collections.
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
	"github.com/cgs-earth/sal-pub/pkg/util"
)

var (
	ErrNotFound      = util.ErrNotFound // so util.ServerError replies 404
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

// PublicAddress is the ActivityStreams IRI that makes an object public.
const PublicAddress = "https://www.w3.org/ns/activitystreams#Public"

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

func (s *Store) GetByActorURI(ctx context.Context, uri string) (*Actor, error) {
	return scanActor(s.db.QueryRowContext(ctx,
		`SELECT `+actorColumns+` FROM actors WHERE actor_uri = $1`, uri))
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

// InboxNote is a note delivered to a local actor. Content is the sender's
// unsanitized HTML.
type InboxNote struct {
	ObjectID     string
	AttributedTo string
	Content      string
	Published    time.Time
}

// AddInboxNote stores a note delivered to actorID. A note already received
// is ignored.
func (s *Store) AddInboxNote(ctx context.Context, actorID string, n InboxNote) error {
	if _, err := s.db.ExecContext(ctx, `
		INSERT INTO inbox_notes (actor_id, object_id, attributed_to, content, published)
		VALUES ($1, $2, $3, $4, $5) ON CONFLICT DO NOTHING`,
		actorID, n.ObjectID, n.AttributedTo, n.Content, n.Published,
	); err != nil {
		return fmt.Errorf("add inbox note: %w", err)
	}
	return nil
}

// UpdateInboxNotes replaces the content of the note objectID in every inbox
// it was delivered to.
func (s *Store) UpdateInboxNotes(ctx context.Context, objectID, content string) error {
	if _, err := s.db.ExecContext(ctx,
		`UPDATE inbox_notes SET content = $2 WHERE object_id = $1`, objectID, content,
	); err != nil {
		return fmt.Errorf("update inbox notes: %w", err)
	}
	return nil
}

// DeleteInboxNotes removes the note objectID from every inbox it was
// delivered to.
func (s *Store) DeleteInboxNotes(ctx context.Context, objectID string) error {
	if _, err := s.db.ExecContext(ctx, `DELETE FROM inbox_notes WHERE object_id = $1`, objectID); err != nil {
		return fmt.Errorf("delete inbox notes: %w", err)
	}
	return nil
}

// ListInboxNotes returns the notes delivered to actorID, newest first.
func (s *Store) ListInboxNotes(ctx context.Context, actorID string, limit int) ([]InboxNote, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT object_id, attributed_to, content, published FROM inbox_notes
		WHERE actor_id = $1 ORDER BY published DESC LIMIT $2`, actorID, limit)
	if err != nil {
		return nil, fmt.Errorf("list inbox notes: %w", err)
	}
	defer rows.Close()

	var out []InboxNote
	for rows.Next() {
		var n InboxNote
		if err := rows.Scan(&n.ObjectID, &n.AttributedTo, &n.Content, &n.Published); err != nil {
			return nil, fmt.Errorf("scan inbox note: %w", err)
		}
		out = append(out, n)
	}
	return out, rows.Err()
}
