-- One ActivityPub actor per Keycloak subject.
CREATE TABLE IF NOT EXISTS actors (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    keycloak_subject TEXT NOT NULL UNIQUE,
    username TEXT NOT NULL UNIQUE,
    display_name TEXT,
    actor_uri TEXT NOT NULL UNIQUE,
    public_key_id TEXT NOT NULL UNIQUE,
    public_key_pem TEXT NOT NULL,
    private_key_pem TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS notes (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    actor_id UUID NOT NULL REFERENCES actors (id) ON DELETE CASCADE,
    content TEXT NOT NULL,
    published TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS notes_actor_published_idx ON notes (actor_id, published DESC);

-- Remote actors following a local actor. inbox is where we deliver to:
-- the follower's sharedInbox when it advertises one, otherwise its inbox.
CREATE TABLE IF NOT EXISTS followers (
    actor_id UUID NOT NULL REFERENCES actors (id) ON DELETE CASCADE,
    follower_uri TEXT NOT NULL,
    inbox TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (actor_id, follower_uri)
);
