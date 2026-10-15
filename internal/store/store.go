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
	To        []string // absolute IRIs, e.g. PublicAddress or a followers collection
	CC        []string
}

// PublicAddress is the ActivityStreams IRI that makes an object public.
const PublicAddress = "https://www.w3.org/ns/activitystreams#Public"

const noteColumns = `id, actor_id, content, published, to_addresses, cc_addresses`

func scanNote(row interface{ Scan(...any) error }, n *Note) error {
	return row.Scan(&n.ID, &n.ActorID, &n.Content, &n.Published, pq.Array(&n.To), pq.Array(&n.CC))
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

func (s *Store) CreateNote(ctx context.Context, actorID, content string, to, cc []string) (*Note, error) {
	// pq.Array sends a nil slice as NULL, but the columns are NOT NULL.
	to, cc = append([]string{}, to...), append([]string{}, cc...)
	n := &Note{ActorID: actorID, Content: content, To: to, CC: cc}
	if err := s.db.QueryRowContext(ctx, `
		INSERT INTO notes (actor_id, content, to_addresses, cc_addresses) VALUES ($1, $2, $3, $4)
		RETURNING id, published`, actorID, content, pq.Array(to), pq.Array(cc),
	).Scan(&n.ID, &n.Published); err != nil {
		return nil, fmt.Errorf("insert note: %w", err)
	}
	return n, nil
}

// GetNote returns a note and its author.
func (s *Store) GetNote(ctx context.Context, id string) (*Note, *Actor, error) {
	n := new(Note)
	if err := scanNote(s.db.QueryRowContext(ctx,
		`SELECT `+noteColumns+` FROM notes WHERE id = $1`, id,
	), n); errors.Is(err, sql.ErrNoRows) {
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

// ListNotes returns an actor's most recent notes, newest first, and the total
// count. With publicOnly, only notes addressed to PublicAddress are included.
func (s *Store) ListNotes(ctx context.Context, actorID string, limit int, publicOnly bool) ([]Note, int, error) {
	const where = `WHERE actor_id = $1 AND (NOT $2 OR $3 = ANY(to_addresses || cc_addresses))`
	var total int
	if err := s.db.QueryRowContext(ctx,
		`SELECT count(*) FROM notes `+where, actorID, publicOnly, PublicAddress,
	).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("count notes: %w", err)
	}

	rows, err := s.db.QueryContext(ctx, `
		SELECT `+noteColumns+` FROM notes `+where+`
		ORDER BY published DESC LIMIT $4`, actorID, publicOnly, PublicAddress, limit)
	if err != nil {
		return nil, 0, fmt.Errorf("list notes: %w", err)
	}
	defer rows.Close()

	var notes []Note
	for rows.Next() {
		var n Note
		if err := scanNote(rows, &n); err != nil {
			return nil, 0, fmt.Errorf("scan note: %w", err)
		}
		notes = append(notes, n)
	}
	return notes, total, rows.Err()
}

// PublicNote is a public note with the author details needed to show it.
type PublicNote struct {
	Note
	Username    string
	DisplayName string
	ActorURI    string
}

// ListPublicNotes returns the most recent notes addressed to PublicAddress
// from all actors, newest first.
func (s *Store) ListPublicNotes(ctx context.Context, limit int) ([]PublicNote, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT n.id, n.actor_id, n.content, n.published, n.to_addresses, n.cc_addresses,
			a.username, COALESCE(a.display_name, ''), a.actor_uri
		FROM notes n JOIN actors a ON a.id = n.actor_id
		WHERE $1 = ANY(n.to_addresses || n.cc_addresses)
		ORDER BY n.published DESC LIMIT $2`, PublicAddress, limit)
	if err != nil {
		return nil, fmt.Errorf("list public notes: %w", err)
	}
	defer rows.Close()

	var notes []PublicNote
	for rows.Next() {
		var n PublicNote
		if err := rows.Scan(&n.ID, &n.ActorID, &n.Content, &n.Published, pq.Array(&n.To), pq.Array(&n.CC),
			&n.Username, &n.DisplayName, &n.ActorURI); err != nil {
			return nil, fmt.Errorf("scan public note: %w", err)
		}
		notes = append(notes, n)
	}
	return notes, rows.Err()
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
